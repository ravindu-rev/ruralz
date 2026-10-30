// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// cannedTransport answers every attempt with one reused response and
// allocates nothing, so a leg's own allocations can be counted.
type cannedTransport struct {
	resp http.Response
	body cannedBody
	hdr  http.Header
}

// cannedBody is an empty response body.
type cannedBody struct{}

func (cannedBody) Read([]byte) (int, error) { return 0, io.EOF }
func (cannedBody) Close() error             { return nil }

func (c *cannedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
	c.resp = http.Response{StatusCode: http.StatusOK, Header: c.hdr, Body: c.body, Request: r}
	return &c.resp, nil
}

// legAllocs returns the allocations of one pass-through GET leg on a
// pooled Endpoint, net/http excluded (the canned transport stands in for
// it), on the real clock as in production.
func legAllocs(t *testing.T) float64 {
	t.Helper()
	return testing.AllocsPerRun(200, cannedLeg(t))
}

// cannedLeg returns one pass-through GET leg on a pooled Endpoint whose
// transport is canned.
func cannedLeg(t testing.TB) func() {
	t.Helper()
	srv := newServer(t, status(http.StatusOK, "ok"))
	m := NewManager(Options{Clock: clock.Real(), Rand: lowSource{}, Dial: newDial().dial})
	s, err := m.Compile(t.Context(), Input{Upstreams: []Config{{Upstream: newUpstream("orders", srv.addr)}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.Activate()
	u, _ := s.Upstream("orders")
	o := outbound(exchange(http.MethodGet, "/v1/orders/42"), nil)
	o.Deadline = time.Now().Add(time.Hour)
	r, err := u.Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	st, ok := u.rt.lookup(srv.addr)
	if !ok {
		t.Fatal("no Endpoint state")
	}
	pools := st.pools.Load().list()
	if len(pools) != 1 {
		t.Fatalf("pools = %d, want 1", len(pools))
	}
	pools[0].send = &cannedTransport{hdr: http.Header{"Content-Type": {"text/plain"}}}
	return func() {
		r, err := u.Leg(t.Context(), o, Target{})
		if err != nil {
			t.Fatalf("Leg: %v", err)
		}
		_ = r.Body.Close()
	}
}

// BenchmarkLegCanned measures the attempt loop of a pass-through leg
// without net/http (the canned transport).
func BenchmarkLegCanned(b *testing.B) {
	leg := cannedLeg(b)
	b.ReportAllocs()
	for b.Loop() {
		leg()
	}
}

// TestLegAllocations reports the Ruralz-owned allocations of the Upstream
// layer stage for a pass-through leg (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Per-stage latency budget", Upstream layer: 6 (target)) and fails when
// they grow past the count this implementation reaches (maxLegAllocs).
func TestLegAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are meaningless under -race")
	}
	got := legAllocs(t)
	t.Logf("allocations per leg: %.0f (stage target 6)", got)
	if got > maxLegAllocs {
		t.Errorf("allocations per leg = %.0f, want at most %d", got, maxLegAllocs)
	}
}

// maxLegAllocs is the allocation count of one pass-through leg: the
// attempt record 1, which is the attempt context and holds its Deadline,
// the request and its URL, and the cancellation of
// context.WithCancelCause 2 (its context and its cancel function); the
// deadline timer and the trace hooks are the pooled leg state's, and the
// copy-free forwarding writer builds the header into a pooled one. A
// relayed Early Hints hook adds 1 and the tracer's injection 2 more. See
// TestLegAllocations.
const maxLegAllocs = 3
