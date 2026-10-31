// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Scrape bounds (docs/architecture/10-observability.md "Observability
// principles", targets).
const (
	// scrapeSlots is how many scrapes encode at once.
	scrapeSlots = 4
	// scrapeWait is how long a scrape waits for a slot before 503.
	scrapeWait = 10 * time.Second
)

// manyRoutes returns the documents of count Routes named <prefix>-NNNN to
// the Upstream u, each its own path prefix.
func manyRoutes(prefix string, count int, timeout string) string {
	var b strings.Builder
	for i := range count {
		name := fmt.Sprintf("%s-%04d", prefix, i)
		b.WriteString(routeDoc(name, "/"+name, timeout))
	}
	return b.String()
}

// stuckScrape is a /metrics request whose client never reads the body:
// over HTTP/2 with a stream window of 1 KiB, the Node's write of a large
// exposition blocks once the window is spent, so the scrape keeps its slot
// while its headers have already arrived.
type stuckScrape struct {
	resp *http.Response
	tr   *http.Transport
}

// startStuckScrape sends GET /metrics on its own HTTP/2 connection and
// returns once the response headers arrived: the scrape holds a slot.
func startStuckScrape(t *testing.T, top *topology.Topology, n *topology.Node) *stuckScrape {
	t.Helper()
	tr := &http.Transport{
		TLSClientConfig: top.ClientTLS(), ForceAttemptHTTP2: true,
		HTTP2: &http.HTTP2Config{MaxReceiveBufferPerStream: 1 << 10},
	}
	req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodGet, n.AdminURL()+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.MetricsToken())
	resp, err := (&http.Client{Transport: tr}).Do(req) //nolint:bodyclose // stuckScrape.close closes it at the test's end
	if err != nil {
		t.Fatal(err)
	}
	s := &stuckScrape{resp: resp, tr: tr}
	t.Cleanup(s.close)
	if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
		t.Fatalf("stuck scrape = %d over %s, want 200 over HTTP/2", resp.StatusCode, resp.Proto)
	}
	return s
}

// close resets the scrape's stream, which ends its write and frees its
// slot, and closes its connection.
func (s *stuckScrape) close() {
	_ = s.resp.Body.Close()
	s.tr.CloseIdleConnections()
}

// TestScrapeSlots checks the /metrics bounds: with four scrapes encoding
// at once, a fifth waits 10 s of the clock for a slot and then gets 503,
// and a scrape after the four end gets 200. The wait is a Node timer the
// controllable clock drives, so the fifth answers once the clock passes
// 10 s, well before 10 s of real time.
func TestScrapeSlots(t *testing.T) {
	requireClock(t)
	top := newTopology(t, topology.Config{})
	mock, err := top.StartMockUpstream(mockup.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// 100 Routes make an exposition of hundreds of KiB, far above the
	// stuck scrapes' 1 KiB stream window.
	n := startNode(t, top, topology.NodeOptions{Bundle: []byte(gatewayDoc("") + upstreamDoc(mock.Addr()) + manyRoutes("r", 100, ""))})
	stuck := make([]*stuckScrape, scrapeSlots)
	for i := range stuck {
		stuck[i] = startStuckScrape(t, top, n)
	}
	type answer struct {
		status int
		err    error
	}
	fifth := make(chan answer, 1)
	go func() {
		resp, err := n.AdminAs(context.WithoutCancel(t.Context()), "/metrics", topology.Metrics)
		fifth <- answer{resp.Status, err}
	}()
	const step = 100 * time.Millisecond
	start := time.Now()
	var advanced time.Duration
	var got answer
wait:
	for {
		select {
		case got = <-fifth:
			break wait
		case <-time.After(20 * time.Millisecond):
		}
		if advanced > scrapeWait+2*time.Second {
			t.Fatalf("the fifth scrape had no answer after %v of the clock", advanced)
		}
		if err := top.Advance(t.Context(), step); err != nil {
			t.Fatal(err)
		}
		advanced += step
	}
	if got.err != nil || got.status != http.StatusServiceUnavailable {
		t.Fatalf("the fifth scrape = %d, %v; want 503", got.status, got.err)
	}
	// The clock moved from before the fifth scrape arrived, so by at
	// least its 10 s wait; the slack covers its arrival.
	if advanced < scrapeWait || advanced > scrapeWait+time.Second {
		t.Errorf("the fifth scrape answered after %v of the clock, want its 10 s wait", advanced)
	}
	if elapsed := time.Since(start); elapsed >= scrapeWait {
		t.Errorf("the fifth scrape answered after %v of real time: its wait follows the real clock", elapsed)
	}
	for _, s := range stuck {
		s.close()
	}
	eventually(t, "a scrape once the slots are free", func() bool {
		resp, err := n.AdminAs(t.Context(), "/metrics", topology.Metrics)
		return err == nil && resp.Status == http.StatusOK
	})
}
