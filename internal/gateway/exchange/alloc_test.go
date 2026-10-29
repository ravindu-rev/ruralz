// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/executor"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Allocation gate and benchmark (PB-8: 30 or fewer Ruralz-owned
// allocations per S1 request around the Router and the executor; the
// per-stage budget gives snapshot pin and Route match, onRequestHeaders,
// onRoute and onUpstreamRequest without attached work 0 allocations). The
// exchange's share is zero: pooled Requests and leg states, slots and
// decisions kept across reuse, Filter views and messages as conversions.

// touchFilter reads what a Filter typically reads and keeps a pointer in
// its PolicyState slot.
type touchFilter struct{ mark *int }

func (f *touchFilter) Handle(_ context.Context, _ phase.Phase, x filter.Exchange) filter.Result {
	if s := x.PolicyState(); *s == nil {
		*s = f.mark
	}
	_ = x.Vars().Request.Header
	_ = x.Message().Header()
	_ = x.RouteMetrics()
	return filter.Next()
}

// allocFixture is a Route with a cors-like, a headers-like and an
// upstream-leg Policy, a request, an attempt and an Upstream response
// reused across requests.
type allocFixture struct {
	pool *Pool
	e    *executor.Executor
	rt   *snapshot.Route
	r    *http.Request
	st   *Start
	leg  *filter.Leg
	up   *snapshot.UpstreamResponse
	rec  emit.AccessRecord
}

func newAllocFixture(tb testing.TB) *allocFixture {
	tb.Helper()
	mark := new(int)
	f := &touchFilter{mark: mark}
	rt := testRoute("orders", snapshot.BodyNeeds{})
	rt.Chain = *chainOf([]*snapshot.Policy{
		compiled("cors", phase.ClassCORS, phase.ScopeRoute, f, phase.OnRequestHeaders, phase.OnResponse),
		compiled("headers", phase.ClassTransform, phase.ScopeRoute, f, phase.OnRequestHeaders, phase.OnRoute, phase.OnResponse, phase.OnLog),
	}, "orders", []*snapshot.Policy{
		compiled("headers-up", phase.ClassTransform, phase.ScopeUpstream, f, phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders),
	})
	rt.Metrics = &emit.RouteMetrics{}
	r := serverRequest(http.MethodGet, "/v1/orders?page=2", "", http.Header{"User-Agent": {"bench"}})
	out := &http.Request{Method: http.MethodGet, URL: r.URL, Header: http.Header{"X-Out": {"1"}}, Body: http.NoBody}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody}
	return &allocFixture{
		pool: newFixture(tb).pool, e: executor.New(executor.Deps{}), rt: rt, r: r, st: start(r.URL.Path),
		leg: &filter.Leg{Upstream: "orders", Attempt: 1, Endpoint: "10.0.0.1:8080", Request: out, Response: resp},
		up:  &snapshot.UpstreamResponse{Status: http.StatusOK, Header: resp.Header, Body: http.NoBody, Upstream: "orders", Endpoint: "10.0.0.1:8080"},
	}
}

// request runs one request through every Phase, as the handler will.
func (a *allocFixture) request(ctx context.Context) {
	x := a.pool.Get()
	x.Begin(a.r, a.st)
	_ = x.MatchRequest()
	x.SetRoute(a.rt, x.ParamBuf())
	run := a.e.Begin(x, &a.rt.Chain, nil)
	for _, ph := range []phase.Phase{phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute} {
		if run.Request(ctx, ph) != nil {
			panic("short-circuit")
		}
		if ph == phase.OnRequestHeaders {
			_ = x.PrepareBody(ctx)
		}
	}
	o := x.Outbound()
	o.Hooks = run
	l := o.Hooks.BeginLeg(ctx, "orders", "")
	x.Timer().Enter(a.st.Time)
	l.OnUpstreamRequest(ctx, a.leg)
	l.OnUpstreamResponseHeaders(ctx, a.leg)
	x.Timer().Leave(a.st.Time.Add(time.Millisecond))
	l.End(ctx)
	x.SetResponse(a.up)
	run.Response(ctx, executor.ResponseUpstream)
	x.SetFinal(filter.Final{Status: http.StatusOK, Committed: true}, 2*time.Millisecond)
	run.Log(ctx)
	x.FillAccess(&a.rec)
	run.Release()
	a.pool.Put(x)
}

// TestAllocsPerRunRequest checks the exchange's PB-8 share: a request with
// a three-Policy chain and one upstream leg, driven by the executor
// through every Phase, allocates nothing once the pools are warm.
func TestAllocsPerRunRequest(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items under -race; the allocation gate runs without it")
	}
	a := newAllocFixture(t)
	ctx := context.Background()
	a.request(ctx) // warm the pools, slots and decisions
	if n := testing.AllocsPerRun(200, func() { a.request(ctx) }); n != 0 {
		t.Fatalf("%v allocations per request, want 0", n)
	}
}

func BenchmarkRequest(b *testing.B) {
	a := newAllocFixture(b)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		a.request(ctx)
	}
}
