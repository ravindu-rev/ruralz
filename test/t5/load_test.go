// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build t5 && linux

package t5

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// The Route under test answers with the mock Upstream's body and the
// header its headers Policy sets, so a response that carries both passed
// the Filter Chain from the Upstream.
const (
	upstreamBody = "t5 upstream\n"
	policyHeader = "X-T5"
	policyValue  = "ruralz"
)

// maxResponseBytes bounds the response body a check reads.
const maxResponseBytes = 64 << 10

// startUpstream serves the mock Upstream on a loopback port until t ends.
func startUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(up.Close)
	return up
}

// checkResponse returns why a response to GET / is not the Route's: 200,
// the protocol the client spoke, the Policy's header and the Upstream's
// body; nil when it is.
func checkResponse(status int, proto string, header http.Header, body []byte, want loadgen.Protocol) error {
	wantProto := "HTTP/1.1"
	if want == loadgen.H2C {
		wantProto = "HTTP/2.0"
	}
	switch {
	case status != http.StatusOK:
		return fmt.Errorf("status %d, want 200", status)
	case proto != wantProto:
		return fmt.Errorf("protocol %s, want %s", proto, wantProto)
	case header.Get(policyHeader) != policyValue:
		return fmt.Errorf("header %s %q, want %q", policyHeader, header.Get(policyHeader), policyValue)
	case string(body) != upstreamBody:
		return fmt.Errorf("body %q, want %q", body, upstreamBody)
	}
	return nil
}

// checkedTarget is a loadgen.Target that spreads GET requests over a
// fixed set of connections, request i on connection i mod n, each lane a
// transport of one connection, and checks every response with
// checkResponse. Its clients honor Connection: close and GOAWAY as
// net/http does: a closed keep-alive connection or a GOAWAY moves the
// next request to a new connection.
type checkedTarget struct {
	url   string
	proto loadgen.Protocol
	lanes []*http.Client
}

// newCheckedTarget returns a target of conns connections speaking proto,
// HTTP/1.1 keep-alive or h2c, to url.
func newCheckedTarget(url string, proto loadgen.Protocol, conns int) *checkedTarget {
	c := &checkedTarget{url: url, proto: proto}
	for range conns {
		var protocols http.Protocols
		if proto == loadgen.H2C {
			protocols.SetUnencryptedHTTP2(true)
		} else {
			protocols.SetHTTP1(true)
		}
		c.lanes = append(c.lanes, &http.Client{
			Transport: &http.Transport{
				Proxy: nil, DisableCompression: true, Protocols: &protocols,
				MaxConnsPerHost: 1, MaxIdleConns: 1, MaxIdleConnsPerHost: 1,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		})
	}
	return c
}

// Do sends request r on its lane and checks the response.
func (c *checkedTarget) Do(ctx context.Context, r loadgen.Request) loadgen.Response {
	var (
		mu   sync.Mutex
		info loadgen.ConnInfo
	)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(g httptrace.GotConnInfo) {
		mu.Lock()
		defer mu.Unlock()
		info = loadgen.ConnInfo{Reused: g.Reused}
		if g.Conn != nil {
			info.Local, info.Remote = g.Conn.LocalAddr().String(), g.Conn.RemoteAddr().String()
		}
	}})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return loadgen.Response{Outcome: loadgen.Other, Err: err}
	}
	res, err := c.lanes[r.Seq%uint64(len(c.lanes))].Do(req)
	mu.Lock()
	resp := loadgen.Response{Conn: info}
	mu.Unlock()
	if err != nil {
		resp.Err, resp.Outcome = err, loadgen.Classify(err)
		return resp
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	resp.Status, resp.Proto, resp.Header, resp.Bytes = res.StatusCode, res.Proto, res.Header, int64(len(body))
	resp.Outcome = loadgen.OutcomeForStatus(res.StatusCode)
	switch {
	case err != nil:
		resp.Err = err
		if resp.Outcome = loadgen.Classify(err); resp.Outcome == loadgen.Success {
			resp.Outcome = loadgen.Other
		}
	default:
		if bad := checkResponse(res.StatusCode, res.Proto, res.Header, body, c.proto); bad != nil {
			resp.Err = bad
			if resp.Outcome == loadgen.Success {
				resp.Outcome = loadgen.Other
			}
		}
	}
	return resp
}

// close closes the target's idle connections.
func (c *checkedTarget) close() {
	for _, l := range c.lanes {
		l.CloseIdleConnections()
	}
}

// loadSpec is one client population of a run: its protocol, connections
// and share of the rate.
type loadSpec struct {
	proto loadgen.Protocol
	conns int
	rate  float64
}

// load is open-loop load from internal/testkit/loadgen, one generator per
// client population, until stop.
type load struct {
	gens    []*loadgen.Generator
	targets []*checkedTarget
}

// startLoad starts the populations against url; t's end stops them.
func startLoad(ctx context.Context, t *testing.T, url string, specs ...loadSpec) *load {
	t.Helper()
	l := &load{}
	for _, s := range specs {
		tg := newCheckedTarget(url, s.proto, s.conns)
		g, err := loadgen.Start(ctx, tg, loadgen.Config{Rate: s.rate, MaxFailures: 100})
		if err != nil {
			t.Fatal(err)
		}
		l.gens, l.targets = append(l.gens, g), append(l.targets, tg)
	}
	t.Cleanup(func() { _, _ = l.stop() })
	return l
}

// done returns the requests that ended so far.
func (l *load) done() int64 {
	var n int64
	for _, g := range l.gens {
		n += g.Progress().Done
	}
	return n
}

// loadWindow is how many requests a check waits to see end on each side
// of an event: a second of the refused handover's load.
const loadWindow = 100

// waitMore waits until loadWindow more requests than now ended, so a check
// covers load on both sides of an event.
func (l *load) waitMore(ctx context.Context) error {
	target := l.done() + loadWindow
	return waitFor(ctx, readyBound, func() (bool, string) {
		d := l.done()
		return d >= target, fmt.Sprintf("%d of %d requests ended", d, target)
	})
}

// loadSummary is a stopped run: requests offered, those that did not end
// in a checked 200, and the first failures.
type loadSummary struct {
	offered, failed int64
	failures        []string
}

// stop ends every population and returns the summary; requests in flight
// finish first. It is idempotent.
func (l *load) stop() (loadSummary, error) {
	var (
		sum  loadSummary
		errs []error
	)
	for i, g := range l.gens {
		g.Stop()
		res, err := g.Wait()
		l.targets[i].close()
		if err != nil {
			errs = append(errs, err)
		}
		if res == nil {
			continue
		}
		sum.offered += res.Measured.Offered
		sum.failed += res.Measured.Errors()
		for _, f := range res.Failures {
			if len(sum.failures) < 20 {
				sum.failures = append(sum.failures, fmt.Sprintf("%s %s on %s: %v", f.Outcome, f.Proto, f.Conn.Local, f.Err))
			}
		}
	}
	return sum, errors.Join(errs...)
}

// readyzProbeInterval spaces the keep-alive checker's probes.
const readyzProbeInterval = 100 * time.Millisecond

// readyzChecker probes /readyz over one keep-alive connection, as a
// balancer's health checker does, until stopped, and counts every probe
// that does not answer 200.
type readyzChecker struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	probes   int
	failures []string
	failed   int
}

// startChecker starts probing url; t's end stops it.
func startChecker(ctx context.Context, t *testing.T, url string) *readyzChecker {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	c := &readyzChecker{cancel: cancel, done: make(chan struct{})}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	client := &http.Client{Transport: &http.Transport{
		Proxy: nil, Protocols: &protocols, MaxConnsPerHost: 1, MaxIdleConns: 1, MaxIdleConnsPerHost: 1,
	}, Timeout: 5 * time.Second}
	go func() {
		defer close(c.done)
		defer client.CloseIdleConnections()
		tick := time.NewTicker(readyzProbeInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			c.probe(ctx, client, url)
		}
	}()
	t.Cleanup(func() { c.stop() })
	return c
}

// probe sends one probe and records its outcome; a probe the checker's
// own stop cancels is not counted.
func (c *readyzChecker) probe(ctx context.Context, client *http.Client, url string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.record(err.Error())
		return
	}
	res, err := client.Do(req)
	if ctx.Err() != nil {
		if err == nil {
			_ = res.Body.Close()
		}
		return
	}
	if err != nil {
		c.record(err.Error())
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxResponseBytes))
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		c.record(fmt.Sprintf("status %d", res.StatusCode))
		return
	}
	c.record("")
}

// record counts a probe; failure is empty for a 200.
func (c *readyzChecker) record(failure string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes++
	if failure == "" {
		return
	}
	c.failed++
	if len(c.failures) < 20 {
		c.failures = append(c.failures, failure)
	}
}

// stop ends the probes and returns how many ran, how many failed and the
// first failures. It is idempotent.
func (c *readyzChecker) stop() (probes, failed int, failures []string) {
	c.cancel()
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.probes, c.failed, append([]string(nil), c.failures...)
}
