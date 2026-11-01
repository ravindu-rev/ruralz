// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package statestore

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// inFlightCeiling is the Node's ceiling on State Store calls in flight,
// 8,192 (target), which every driver shares
// (docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes").
const inFlightCeiling = 8192

// The burst's deadlines, above the time a loaded runner takes to answer
// its 8,448 requests once the replies pass, the Upstream's connections
// opened as they go: under the default 15 s Route timeout and the
// harness's 20 s client bound the burst depended on the runner's speed,
// and the Route deadline reset the streams of the slowest requests.
const (
	inFlightRouteTimeout  = "60s"
	inFlightClientTimeout = 90 * time.Second
)

// TestInFlightCeiling checks the 8,192-call ceiling: with the counting
// proxy holding every EVALSHA reply, the calls of a burst of requests take
// the ceiling's slots and wait, and every call past it is skipped without
// touching the network, RZ-STS-004 under failureMode closed, counted as a
// skipped call; once the replies pass, the held calls complete.
func TestInFlightCeiling(t *testing.T) {
	e := newEnv(t, envOptions{})
	// The Upstream's bulkhead takes the whole burst once the replies
	// pass.
	e.start(bundle{
		stateStore:   redisStore("standalone", urlRef),
		upstream:     e.up.Addr(),
		upstreamSpec: []string{"circuitBreaker: {maxConnections: 16384, maxPendingRequests: 16384}"},
		docs:         []string{rateLimit("rl", 1000000, "1s", "", "closed"), route("limited", "/limited", "rl") + "  timeout: " + inFlightRouteTimeout + "\n"},
	}.render(), mainURLs())
	e.warmUp("/limited", "x-client", "hot")
	m := e.meter("main")
	before := e.metrics()
	release := m.hold(queueHold)
	defer release()
	client := h2cClient(e, inFlightClientTimeout)

	const over = 256
	total := inFlightCeiling + over
	var skipped, admitted atomic.Int64
	var failed atomic.Value
	var wg sync.WaitGroup
	start := time.Now()
	for range total {
		wg.Go(func() {
			r, err := e.send(client, http.MethodGet, "/limited", nil, "x-client", "hot")
			switch {
			case err != nil:
				failed.CompareAndSwap(nil, err)
			case r.status == http.StatusServiceUnavailable && r.code == "RZ-STS-004":
				skipped.Add(1)
			case r.status == http.StatusOK:
				admitted.Add(1)
			default:
				failed.CompareAndSwap(nil, statusError{r})
			}
			e.top.Capture("inflight-client", r.body)
		})
	}
	e.waitFor("the calls past the ceiling to be skipped", func() bool { return skipped.Load() >= over })
	if since := time.Since(start); since > queueHold-time.Second {
		t.Fatalf("the burst took %v, too long for the held replies", since)
	}
	if got := admitted.Load(); got != 0 {
		t.Errorf("%d requests completed while their replies were held", got)
	}
	release()
	wg.Wait()
	if err, _ := failed.Load().(error); err != nil {
		t.Fatalf("a request failed: %v", err)
	}
	if skipped.Load() != over || admitted.Load() != inFlightCeiling {
		t.Errorf("%d requests skipped and %d admitted, want %d past the ceiling and %d within it", skipped.Load(), admitted.Load(), over, inFlightCeiling)
	}
	d := promtext.Delta(before, e.metrics(), "ruralz_state_calls_total", promtext.L("op", "gcra", "result", "skipped"))
	if d != over {
		t.Errorf("ruralz_state_calls_total{op=\"gcra\",result=\"skipped\"} rose by %v, want %d", d, over)
	}
}

// h2cClient returns a client speaking HTTP/2 over cleartext with prior
// knowledge to the Node's web listener, many streams per connection, each
// request bounded by timeout.
func h2cClient(e *env, timeout time.Duration) *http.Client {
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: &p}
	e.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: timeout}
}
