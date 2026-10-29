// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// legResult is the outcome of a leg run on another goroutine.
type legResult struct {
	r   *snapshot.UpstreamResponse
	err error
}

// goLeg runs a leg on a new goroutine; the test waits for its result.
func goLeg(ctx context.Context, u *Upstream, o *snapshot.Outbound) <-chan legResult {
	ch := make(chan legResult, 1)
	go func() {
		r, err := u.Leg(ctx, o, Target{})
		ch <- legResult{r, err}
	}()
	return ch
}

// await returns a goLeg result, failing the test after a real-time bound.
func await(t *testing.T, ch <-chan legResult) legResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(10 * time.Second):
		t.Fatal("leg did not return")
		return legResult{}
	}
}

// blocking returns a handler that blocks until release is closed, then
// answers 200 with "ok".
func blocking(release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}
}

// TestLegBreakerOpens checks the circuit breaker
// (docs/architecture/09-traffic-management-and-resilience.md "Circuit
// breakers"): a leg whose final result matches failureWhen counts as a
// failure, the breaker opens past its thresholds, the state gauge follows,
// and a leg is then refused at once with 503 RZ-UP-005 without any
// attempt.
func TestLegBreakerOpens(t *testing.T) {
	srv := newServer(t, status(http.StatusServiceUnavailable, ""))
	f := newFixture(t)
	met := newMetrics()
	u := newUpstream("orders", srv.addr)
	u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
	u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{ConsecutiveFailures: ptr[int32](1), MinimumLegs: ptr[int32](1)}
	s := f.compile(nil, Config{Upstream: u, Metrics: met.m})
	up := f.upstream(s, "orders")

	r, err := up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("first Leg: %v", err)
	}
	readAll(t, r)
	if st := up.rt.res.Breaker().State(); st != resilience.StateOpen {
		t.Fatalf("breaker = %v after a failed leg, want open", st)
	}
	if met.breaker[resilience.StateOpen].get() != 1 || met.breaker[resilience.StateClosed].get() != 0 {
		t.Errorf("breaker gauges = closed %d open %d, want 0 and 1", met.breaker[resilience.StateClosed].get(), met.breaker[resilience.StateOpen].get())
	}
	_, err = up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if codeOf(err) != "RZ-UP-005" || srv.hits.Load() != 1 {
		t.Errorf("Leg with the breaker open = %v after %d requests, want RZ-UP-005 after 1", err, srv.hits.Load())
	}
}

// TestLegBulkhead checks the in-flight ceiling: an attempt holds a slot
// until its response body closes, a full pending queue refuses at once
// with 503 RZ-UP-006, and a waiter proceeds when a slot frees.
func TestLegBulkhead(t *testing.T) {
	release := make(chan struct{})
	srv := newServer(t, blocking(release))
	f := newFixture(t)
	u := newUpstream("orders", srv.addr)
	u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
	u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{MaxConnections: ptr[int32](1), MaxPendingRequests: ptr[int32](1)}
	s := f.compile(nil, Config{Upstream: u})
	up := f.upstream(s, "orders")
	get := func() *snapshot.Outbound { return outbound(exchange(http.MethodGet, "/"), nil) }

	first := goLeg(t.Context(), up, get())
	waitFor(t, "the first attempt", func() bool { return srv.hits.Load() == 1 })
	second := goLeg(t.Context(), up, get())
	waitFor(t, "a waiter", func() bool { return up.rt.res.Bulkhead().Pending() == 1 })
	if _, err := up.Leg(t.Context(), get(), Target{}); codeOf(err) != "RZ-UP-006" {
		t.Errorf("Leg with every slot and the queue full = %v, want RZ-UP-006", err)
	}
	close(release)
	res := await(t, first)
	if res.err != nil {
		t.Fatalf("first Leg: %v", res.err)
	}
	// The first response's body is still open: its slot is held, so the
	// waiter has not started.
	if hits := srv.hits.Load(); hits != 1 {
		t.Errorf("requests before the first body closed = %d, want 1", hits)
	}
	readAll(t, res.r)
	res = await(t, second)
	if res.err != nil {
		t.Fatalf("waiting Leg: %v", res.err)
	}
	readAll(t, res.r)
	if n := up.rt.res.Bulkhead().InFlight(); n != 0 {
		t.Errorf("slots held after both bodies closed = %d, want 0", n)
	}
}

// TestLegWaiterTimesOut checks that a waiter for a bulkhead slot is bounded
// by its attempt context: at the per-try timeout it ends as error kind
// timeout without reaching an Endpoint, 504 RZ-UP-003.
func TestLegWaiterTimesOut(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := newServer(t, blocking(release))
	f := newFixture(t)
	u := newUpstream("orders", srv.addr)
	u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0), PerTryTimeout: dur(time.Second)}
	u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{MaxConnections: ptr[int32](1), MaxPendingRequests: ptr[int32](1)}
	s := f.compile(nil, Config{Upstream: u})
	up := f.upstream(s, "orders")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := goLeg(ctx, up, outbound(exchange(http.MethodGet, "/"), nil))
	waitFor(t, "the first attempt", func() bool { return srv.hits.Load() == 1 })
	second := goLeg(t.Context(), up, outbound(exchange(http.MethodGet, "/"), nil))
	waitFor(t, "a waiter", func() bool { return up.rt.res.Bulkhead().Pending() == 1 })
	f.clk.Advance(time.Second)
	res := await(t, second)
	var le *LegError
	if !errors.As(res.err, &le) || codeOf(res.err) != "RZ-UP-003" || le.Kind != resilience.KindTimeout || le.Endpoint != "" {
		t.Errorf("waiting Leg = %v, want RZ-UP-003 of kind timeout without an Endpoint", res.err)
	}
	cancel()
	await(t, first)
}

// TestLegAttemptTimeout checks the attempt deadline: retries.perTryTimeout
// when set, else the leg time left divided by the retries left plus one,
// ends a silent attempt as error kind timeout; the default retryOn does not
// retry it, so the leg is 504 RZ-UP-003.
func TestLegAttemptTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retries *v1alpha1.Retries
		timeout *v1alpha1.Duration
		wait    time.Duration
	}{
		{"perTryTimeout", &v1alpha1.Retries{Attempts: ptr[int32](0), PerTryTimeout: dur(time.Second)}, nil, time.Second},
		{"derived from the leg time", &v1alpha1.Retries{Attempts: ptr[int32](1)}, dur(2 * time.Second), time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			srv := newServer(t, blocking(release))
			f := newFixture(t)
			met := newMetrics()
			u := newUpstream("orders", srv.addr)
			u.Spec.Retries, u.Spec.Timeout = tc.retries, tc.timeout
			s := f.compile(nil, Config{Upstream: u, Metrics: met.m})
			up := f.upstream(s, "orders")
			ch := goLeg(t.Context(), up, outbound(exchange(http.MethodGet, "/"), nil))
			waitFor(t, "the attempt", func() bool { return srv.hits.Load() == 1 })
			f.clk.Advance(tc.wait - time.Nanosecond)
			select {
			case res := <-ch:
				t.Fatalf("Leg returned before its deadline: %v", res.err)
			case <-time.After(20 * time.Millisecond):
			}
			f.clk.Advance(time.Nanosecond)
			res := await(t, ch)
			var le *LegError
			if !errors.As(res.err, &le) || codeOf(res.err) != "RZ-UP-003" || le.Kind != resilience.KindTimeout || le.Attempts != 1 {
				t.Errorf("Leg = %v, want RZ-UP-003 of kind timeout after 1 attempt", res.err)
			}
			if a := met.attempts.list(); !slices.Equal(a, []string{"timeout"}) {
				t.Errorf("attempts = %v, want [timeout]", a)
			}
		})
	}
}

// TestLegCanceledIsNoUpstreamFailure checks that an attempt ended by the
// caller (the client went away, a sibling step failed) is not an Upstream
// failure: no failureWhen, no passive ejection count, no breaker count, no
// attempt count, no retry.
func TestLegCanceledIsNoUpstreamFailure(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := newServer(t, blocking(release))
	f := newFixture(t)
	met := newMetrics()
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr), Metrics: met.m})
	up := f.upstream(s, "orders")
	ctx, cancel := context.WithCancel(t.Context())
	ch := goLeg(ctx, up, outbound(exchange(http.MethodGet, "/"), nil))
	waitFor(t, "the attempt", func() bool { return srv.hits.Load() == 1 })
	cancel()
	res := await(t, ch)
	if res.err == nil {
		t.Fatal("Leg = nil error after the caller canceled, want an error")
	}
	st := up.rt.tracker.Status(f.clk.Now())
	stats := up.rt.res.Breaker().Stats()
	if len(met.attempts.list()) != 0 || st[0].ConsecutiveErrors != 0 || stats.WindowLegs != 0 || srv.hits.Load() != 1 {
		t.Errorf("after cancel: attempts %v, consecutive errors %d, breaker legs %d, requests %d; want none, 0, 0, 1",
			met.attempts.list(), st[0].ConsecutiveErrors, stats.WindowLegs, srv.hits.Load())
	}
}

// sleeping waits until the leg sleeps its retry delay on the fake clock:
// the only timer armed.
func sleeping(t *testing.T, f *fixture) {
	t.Helper()
	waitFor(t, "the retry delay", func() bool { return f.clk.Pending() == 1 })
}

// TestLegRetryAfter checks the Upstream's Retry-After: over 10 s the
// retry is refused and the response returns; within it, the retry waits
// for it (longer than the backoff) on the clock.
func TestLegRetryAfter(t *testing.T) {
	var n int
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		n++
		switch n {
		case 1:
			w.Header().Set("Retry-After", "11")
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	up := f.upstream(s, "orders")
	r, err := up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if r.Status != 503 || r.Attempts != 1 {
		t.Errorf("Leg with Retry-After 11 = %d after %d attempts, want 503 after 1", r.Status, r.Attempts)
	}
	ch := goLeg(t.Context(), up, outbound(exchange(http.MethodGet, "/"), nil))
	sleeping(t, f)
	f.clk.Advance(2*time.Second - time.Nanosecond)
	if srv.hits.Load() != 2 {
		t.Errorf("requests before Retry-After elapsed = %d, want 2", srv.hits.Load())
	}
	f.clk.Advance(time.Nanosecond)
	res := await(t, ch)
	if res.err != nil {
		t.Fatalf("Leg: %v", res.err)
	}
	readAll(t, res.r)
	if res.r.Status != 200 || res.r.Attempts != 2 {
		t.Errorf("Leg with Retry-After 2 = %d after %d attempts, want 200 after 2", res.r.Status, res.r.Attempts)
	}
}

// TestLegRetryBudget checks the retry budget: a leg alone in flight makes
// at most 3 retries whatever retries.attempts allows; the refused retry is
// counted in ruralz_upstream_retry_budget_exhausted_total and the leg
// returns its failure.
func TestLegRetryBudget(t *testing.T) {
	const refused = "127.0.0.1:1"
	f := newFixture(t, refused)
	met := newMetrics()
	u := newUpstream("orders", refused)
	u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](5)}
	s := f.compile(nil, Config{Upstream: u, Metrics: met.m})
	_, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	var le *LegError
	if !errors.As(err, &le) || codeOf(err) != "RZ-UP-007" || le.Attempts != 4 {
		t.Errorf("Leg = %v, want RZ-UP-007 after 4 attempts", err)
	}
	if met.budget.get() != 1 || met.retries.get() != 3 {
		t.Errorf("budget exhausted = %d, retries = %d, want 1 and 3", met.budget.get(), met.retries.get())
	}
}

// TestLegReturnsParkedResponse checks that a leg whose retry cannot start
// (here the bulkhead fills during the Retry-After delay) returns the last
// response unchanged, body included.
func TestLegReturnsParkedResponse(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var n int
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "first")
			return
		}
		blocking(release)(w, r)
	})
	f := newFixture(t)
	u := newUpstream("orders", srv.addr)
	u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{MaxConnections: ptr[int32](1), MaxPendingRequests: ptr[int32](0)}
	s := f.compile(nil, Config{Upstream: u})
	up := f.upstream(s, "orders")
	ch := goLeg(t.Context(), up, outbound(exchange(http.MethodGet, "/"), nil))
	sleeping(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	other := goLeg(ctx, up, outbound(exchange(http.MethodGet, "/"), nil))
	waitFor(t, "the other leg", func() bool { return srv.hits.Load() == 2 })
	f.clk.Advance(time.Second)
	res := await(t, ch)
	if res.err != nil {
		t.Fatalf("Leg: %v", res.err)
	}
	if body := readAll(t, res.r); res.r.Status != 503 || body != "first" || res.r.Attempts != 1 {
		t.Errorf("Leg = %d %q after %d attempts, want the parked 503 first after 1", res.r.Status, body, res.r.Attempts)
	}
	cancel()
	await(t, other)
}

// TestLegTLS checks a TLS Upstream: the server is verified against
// tls.caCertificate, HTTP/2 is negotiated by ALPN, the request reaches the
// Endpoint authority, and the HTTP/2 connection counts as active while the
// attempt uses it, then idle.
func TestLegTLS(t *testing.T) {
	srv := newTLSServer(t, status(http.StatusOK, "secure"))
	f := newFixture(t)
	met := newMetrics()
	u := newUpstream("orders", srv.addr)
	u.Spec.TLS = &v1alpha1.UpstreamTLS{CACertificate: &v1alpha1.SecretValue{SecretRef: v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderFile, Name: caRef().Name}}}
	s := f.compile(caStore(srv.Server), Config{Upstream: u, Metrics: met.m})
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/v1"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if met.pool[emit.PoolActive].get() != 1 {
		t.Errorf("active connections while the body is open = %d, want 1", met.pool[emit.PoolActive].get())
	}
	if body := readAll(t, r); body != "secure" {
		t.Errorf("body = %q, want secure", body)
	}
	if met.pool[emit.PoolActive].get() != 0 || met.pool[emit.PoolIdle].get() != 1 {
		t.Errorf("connections after the body = active %d idle %d, want 0 and 1", met.pool[emit.PoolActive].get(), met.pool[emit.PoolIdle].get())
	}
	if got := srv.last(); got.ProtoMajor != 2 || got.Host != srv.addr || got.TLS == nil {
		t.Errorf("request over HTTP/%d to Host %q, TLS %v; want HTTP/2 to %q over TLS", got.ProtoMajor, got.Host, got.TLS != nil, srv.addr)
	}
}

// TestLegTLSVerificationFails checks that an Upstream certificate the
// roots do not trust fails the handshake: error kind tls, not retried by
// the default retryOn, 502 RZ-UP-002.
func TestLegTLSVerificationFails(t *testing.T) {
	srv := newTLSServer(t, status(http.StatusOK, ""))
	f := newFixture(t)
	met := newMetrics()
	u := newUpstream("orders", srv.addr)
	u.Spec.TLS = &v1alpha1.UpstreamTLS{}
	s := f.compile(nil, Config{Upstream: u, Metrics: met.m})
	_, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	var le *LegError
	if !errors.As(err, &le) || codeOf(err) != "RZ-UP-002" || le.Kind != resilience.KindTLS || le.Attempts != 1 {
		t.Errorf("Leg = %v, want RZ-UP-002 of kind tls after 1 attempt", err)
	}
	if srv.hits.Load() != 0 || !slices.Equal(met.attempts.list(), []string{"tls"}) {
		t.Errorf("requests = %d, attempts = %v; want 0 and [tls]", srv.hits.Load(), met.attempts.list())
	}
}

// TestLegConnectErrorsCountPastEjectedMajority checks the breaker's connect
// rule (docs/architecture/09-traffic-management-and-resilience.md "Circuit
// breakers"): a leg ending in a connect error counts only while more than
// 50% of the Endpoints are ejected, so a bad Endpoint alone never opens
// the breaker.
func TestLegConnectErrorsCountPastEjectedMajority(t *testing.T) {
	const refused = "127.0.0.1:1"
	for _, tc := range []struct {
		name    string
		errors  int32
		opened  bool
		ejected bool
	}{
		{"not ejected: not counted", 5, false, false},
		{"ejected: counted", 1, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, refused)
			u := newUpstream("orders", refused)
			u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
			u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{ConsecutiveFailures: ptr[int32](1), MinimumLegs: ptr[int32](1)}
			u.Spec.HealthCheck = &v1alpha1.HealthCheck{Passive: &v1alpha1.PassiveHealthCheck{ConsecutiveErrors: ptr(tc.errors)}}
			s := f.compile(nil, Config{Upstream: u})
			up := f.upstream(s, "orders")
			if _, err := up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{}); codeOf(err) != "RZ-UP-001" {
				t.Fatalf("Leg = %v, want RZ-UP-001", err)
			}
			ejected := up.rt.tracker.Status(f.clk.Now())[0].Ejected
			opened := up.rt.res.Breaker().State() == resilience.StateOpen
			if ejected != tc.ejected || opened != tc.opened {
				t.Errorf("ejected %v, breaker open %v; want %v and %v", ejected, opened, tc.ejected, tc.opened)
			}
		})
	}
}
