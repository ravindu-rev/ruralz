// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestLegPhasesUnderAttemptSpan checks that every leg Phase runs under its
// attempt's ruralz.upstream span, so upstream-scoped Policies are children
// of their leg's span (docs/architecture/10-observability.md "Span
// model"): onUpstreamResponseHeaders of an attempt that failed without a
// response, after the attempt's context ended, and onUpstreamResponseBody,
// which runs on the leg context, as much as onUpstreamRequest. The span
// stays open until the Phase ends, even when the body Phase reads the body
// to its end and so ends the rest of the attempt, and ends once.
func TestLegPhasesUnderAttemptSpan(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, "ok"))
	const refused = "127.0.0.1:1"
	f := newFixture(t, refused)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", refused, srv.addr)})
	h := &hooks{onBody: func(leg *filter.Leg) *filter.Response {
		if _, err := io.ReadAll(leg.Response.Body); err != nil {
			t.Errorf("read the body in onUpstreamResponseBody: %v", err)
		}
		return nil
	}}
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	want := []string{"request 1 under 1", "headers 1 under 1", "request 2 under 2", "headers 2 under 2", "body 2 under 2"}
	if got := h.listUnder(); !slices.Equal(got, want) {
		t.Errorf("leg Phases under spans = %q, want %q", got, want)
	}
	// The body Phase read the body to its end, so the second attempt's
	// span ended with the Phase.
	spans := f.tracer.list()
	if len(spans) != 2 || spans[0].attrs.Attempt != 1 || spans[0].errType != "connect" || spans[1].attrs.Attempt != 2 || spans[1].status != http.StatusOK {
		t.Errorf("ended spans = %+v, want attempt 1 with error connect, then attempt 2 with 200", spans)
	}
	readAll(t, r)
	if n := len(f.tracer.list()); n != 2 {
		t.Errorf("%d spans ended once the body closed, want 2", n)
	}
}

// TestLegBodyPhaseKeepsOpenAttemptSpan checks that a body Phase that does
// not read the body leaves the attempt's span open until the body ends.
func TestLegBodyPhaseKeepsOpenAttemptSpan(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, "ok"))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	h := &hooks{}
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if got, want := h.listUnder(), []string{"request 1 under 1", "headers 1 under 1", "body 1 under 1"}; !slices.Equal(got, want) {
		t.Errorf("leg Phases under spans = %q, want %q", got, want)
	}
	if n := len(f.tracer.list()); n != 0 {
		t.Errorf("%d spans ended while the body is open, want 0", n)
	}
	if body := readAll(t, r); body != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
	if spans := f.tracer.list(); len(spans) != 1 || spans[0].status != http.StatusOK {
		t.Errorf("ended spans = %+v, want the attempt's with 200", spans)
	}
}

// TestLegParkedResponseBodyPhaseUnderItsSpan checks that the parked
// response a retry could not replace (here the bulkhead fills during the
// Retry-After delay) runs onUpstreamResponseBody under its own attempt's
// span (docs/architecture/10-observability.md "Span model").
func TestLegParkedResponseBodyPhaseUnderItsSpan(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var n atomic.Int32
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
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
	h := &hooks{}
	ch := goLeg(t.Context(), up, outbound(exchange(http.MethodGet, "/"), h))
	sleeping(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	other := goLeg(ctx, up, outbound(exchange(http.MethodGet, "/"), nil))
	waitFor(t, "the other leg", func() bool { return srv.hits.Load() == 2 })
	f.clk.Advance(time.Second)
	res := await(t, ch)
	if res.err != nil {
		t.Fatalf("Leg: %v", res.err)
	}
	if body := readAll(t, res.r); res.r.Status != http.StatusServiceUnavailable || body != "first" {
		t.Errorf("Leg = %d %q, want the parked 503 first", res.r.Status, body)
	}
	if got, want := h.listUnder(), []string{"request 1 under 1", "headers 1 under 1", "body 1 under 1"}; !slices.Equal(got, want) {
		t.Errorf("leg Phases under spans = %q, want %q", got, want)
	}
	cancel()
	await(t, other)
}
