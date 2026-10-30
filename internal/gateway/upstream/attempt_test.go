// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestLegResponseHeadersOnFailedAttempt checks that onUpstreamResponseHeaders
// runs for an attempt that got no response too (Provider Fallback decides
// there): the leg has no response and names the attempt's error kind, and
// the retry follows as usual.
func TestLegResponseHeadersOnFailedAttempt(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, "ok"))
	const refused = "127.0.0.1:1"
	f := newFixture(t, refused)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", refused, srv.addr)})
	var seen []filter.Leg
	h := &hooks{onHeaders: func(leg *filter.Leg) (bool, *filter.Response) {
		seen = append(seen, *leg)
		return false, nil
	}}
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	want := []string{`begin orders ""`, "request 1 " + refused, "headers 1 error connect", "request 2 " + srv.addr, "headers 2 200", "body 200", "end"}
	if ev := h.list(); !slices.Equal(ev, want) {
		t.Errorf("leg Phases = %q, want %q", ev, want)
	}
	if len(seen) != 2 || seen[0].Response != nil || seen[0].ErrorKind != "connect" || seen[0].Endpoint != refused || seen[1].ErrorKind != "" {
		t.Errorf("legs seen = %+v, want a connect attempt without response, then a response", seen)
	}
}

// TestLegFilterRetriesFailedAttempt checks that a Filter's retry on an
// attempt without a response replaces retryOn: a reset POST, which the
// default retryOn never retries, is retried, under every other retry
// condition.
func TestLegFilterRetriesFailedAttempt(t *testing.T) {
	var mu sync.Mutex
	n := 0
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		if first {
			resetConn(t, w)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	up := f.upstream(s, "orders")
	h := &hooks{onHeaders: func(leg *filter.Leg) (bool, *filter.Response) { return leg.ErrorKind == "reset", nil }}
	r, err := up.Leg(t.Context(), outbound(exchange(http.MethodPost, "/"), h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if r.Status != http.StatusCreated || r.Attempts != 2 {
		t.Errorf("Leg = %d after %d attempts, want 201 after 2", r.Status, r.Attempts)
	}

	// Without the Filter's retry the default retryOn keeps the reset.
	mu.Lock()
	n = 0
	mu.Unlock()
	_, err = up.Leg(t.Context(), outbound(exchange(http.MethodPost, "/"), &hooks{}), Target{})
	if codeOf(err) != "RZ-UP-004" {
		t.Errorf("Leg without the Filter's retry = %v, want RZ-UP-004", err)
	}
}

// resetConn closes the connection of w without a response.
func resetConn(t *testing.T, w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Error("no Hijacker")
		return
	}
	if c, _, err := hj.Hijack(); err == nil {
		_ = c.Close()
	}
}

// TestLegReplaceOnFailedAttempt checks that a failed response Phase on an
// attempt without a response (502 RZ-RT-012) ends the leg with its
// replacement, with no retry, and the leg counts as the attempt's failure
// for the breaker.
func TestLegReplaceOnFailedAttempt(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, ""))
	const refused = "127.0.0.1:1"
	f := newFixture(t, refused)
	u := newUpstream("orders", refused, srv.addr)
	u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{ConsecutiveFailures: ptr[int32](1)}
	u.Spec.HealthCheck = &v1alpha1.HealthCheck{Passive: &v1alpha1.PassiveHealthCheck{ConsecutiveErrors: ptr[int32](1)}}
	s := f.compile(nil, Config{Upstream: u})
	up := f.upstream(s, "orders")
	rep := &filter.Response{Status: http.StatusBadGateway, Code: "RZ-RT-012"}
	h := &hooks{onHeaders: func(leg *filter.Leg) (bool, *filter.Response) {
		if leg.Response == nil {
			return false, rep
		}
		return false, nil
	}}
	r, err := up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if r.Generated != rep || r.Attempts != 1 || srv.hits.Load() != 0 {
		t.Errorf("Leg = %+v after %d requests, want the replacement after 1 attempt and none served", r, srv.hits.Load())
	}
	// One of two Endpoints ejected is no majority: the connect error does
	// not count, so the breaker window stays empty.
	if st := up.rt.res.Breaker().Stats(); st.State != resilience.StateClosed || st.WindowLegs != 0 {
		t.Errorf("breaker = %+v, want closed with no counted leg", st)
	}
}

// TestLegCanceledAttemptRunsNoResponsePhase checks that an attempt the
// caller canceled runs no onUpstreamResponseHeaders: it has no Upstream
// outcome.
func TestLegCanceledAttemptRunsNoResponsePhase(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := newServer(t, blocking(release))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	h := &hooks{}
	ctx, cancel := context.WithCancel(t.Context())
	ch := goLeg(ctx, f.upstream(s, "orders"), outbound(exchange(http.MethodGet, "/"), h))
	waitFor(t, "the attempt", func() bool { return srv.hits.Load() == 1 })
	cancel()
	if res := await(t, ch); res.err == nil {
		t.Fatal("Leg = nil error after the caller canceled")
	}
	want := []string{`begin orders ""`, "request 1 " + srv.addr, "end"}
	if ev := h.list(); !slices.Equal(ev, want) {
		t.Errorf("leg Phases = %q, want %q", ev, want)
	}
}

// TestLegStepCap checks that a step's maxBodyBytes reaches the leg's
// Phases (filter.Leg.MaxBodyBytes), which cap its response body.
func TestLegStepCap(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, ""))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	h := &hooks{}
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), h), Target{Step: "order", MaxBodyBytes: 4096})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if len(h.legs) != 1 || h.legs[0].MaxBodyBytes != 4096 || h.legs[0].Step != "order" {
		t.Errorf("legs = %+v, want one of step order capped at 4096", h.legs)
	}
}

// excluder records gateway-added time exclusions.
type excluder struct {
	mu                    sync.Mutex
	depth, enters, leaves int
	unbalanced            bool
}

func (e *excluder) Enter(time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.depth++
	e.enters++
}

func (e *excluder) Leave(time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.depth == 0 {
		e.unbalanced = true
	}
	e.depth--
	e.leaves++
}

func (e *excluder) state() (depth, enters, leaves int, unbalanced bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.depth, e.enters, e.leaves, e.unbalanced
}

// TestLegExcludesUpstreamIO checks Outbound.Excluder: the round trip is
// excluded from gateway-added time while it runs, and so is every read of
// the leg's response body, balanced.
func TestLegExcludesUpstreamIO(t *testing.T) {
	ex := &excluder{}
	var during atomic.Int64
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		d, _, _, _ := ex.state()
		during.Store(int64(d))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	})
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	o := outbound(exchange(http.MethodGet, "/"), nil)
	o.Excluder = ex
	r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if d := during.Load(); d != 1 {
		t.Errorf("exclusion depth while the Upstream answered = %d, want 1", d)
	}
	_, before, _, _ := ex.state()
	readAll(t, r)
	depth, enters, leaves, unbalanced := ex.state()
	if depth != 0 || unbalanced || enters != leaves || enters <= before {
		t.Errorf("exclusions: depth %d, %d enters, %d leaves (%d before the body), unbalanced %v; want the body reads excluded too",
			depth, enters, leaves, before, unbalanced)
	}
}

// TestPoolIdleConnTimeout checks that a pooled Upstream connection closes
// after 90 s idle (docs/architecture/03-data-plane.md "Upstream layer"):
// the Endpoint's pool transport carries the timeout.
func TestPoolIdleConnTimeout(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, ""))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	up := f.upstream(s, "orders")
	r, err := up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	st, ok := up.rt.lookup(srv.addr)
	if !ok {
		t.Fatal("no Endpoint state")
	}
	pools := st.pools.Load().list()
	if len(pools) != 1 {
		t.Fatalf("pools = %d, want 1", len(pools))
	}
	if got := pools[0].tr.IdleConnTimeout; got != 90*time.Second {
		t.Errorf("pool IdleConnTimeout = %v, want 90s", got)
	}
}

// TestLegResponseHeaderCap checks that an Upstream response header block is
// capped at the client header ceiling, 256 KiB: a larger block fails the
// attempt as a protocol error (502 RZ-UP-004), a smaller one passes.
func TestLegResponseHeaderCap(t *testing.T) {
	if MaxResponseHeaderBytes != 256<<10 {
		t.Fatalf("MaxResponseHeaderBytes = %d, want 256 KiB", MaxResponseHeaderBytes)
	}
	for _, tc := range []struct {
		size int
		code string
	}{
		{200 << 10, ""},
		{300 << 10, "RZ-UP-004"},
	} {
		srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Big", strings.Repeat("v", tc.size))
			w.WriteHeader(http.StatusOK)
		})
		f := newFixture(t)
		u := newUpstream("orders", srv.addr)
		u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
		s := f.compile(nil, Config{Upstream: u})
		r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
		if err == nil {
			readAll(t, r)
		}
		var le *LegError
		if codeOf(err) != tc.code || (tc.code != "" && (!errors.As(err, &le) || le.Kind != resilience.KindReset)) {
			t.Errorf("Leg with a %d-byte header = %v, want code %q", tc.size, err, tc.code)
		}
	}
}
