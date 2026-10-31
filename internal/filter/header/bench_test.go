// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package header

import (
	"context"
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Allocation gates and benchmarks of headers
// (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Per-stage latency budget": onUpstreamRequest with headers setting two
// values, 2 µs at p50 and 8 µs at p99, 2 allocations, targets).

// twoLiterals sets two literal request values.
func twoLiterals() *v1alpha1.HeadersConfig {
	return &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Set: []v1alpha1.HeaderRequestSet{reqLit("x-shop-tenant", "acme"), reqLit("x-shop-region", "eu")},
	}}
}

// twoComputed sets two computed request values.
func twoComputed() *v1alpha1.HeadersConfig {
	return &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Set: []v1alpha1.HeaderRequestSet{
			reqExpr("x-shop-consumer", `consumer == null ? "anonymous" : consumer.name`),
			reqExpr("x-shop-route", `route.name`),
		},
	}}
}

// outgoing returns a typical outgoing request header map.
func outgoing() http.Header {
	return http.Header{
		"Accept": {"application/json"}, "Authorization": {"Bearer x"}, "User-Agent": {"shop/1.0"},
		"Content-Type": {"application/json"}, "X-Request-Id": {"4bf92f3577b34da6"}, "Traceparent": {"00-4bf9-00f0-01"},
	}
}

// TestLiteralSideAllocatesNothing checks that setting and removing literal
// values allocates nothing when the message's keys are canonical, the
// fields the Policy sets included.
func TestLiteralSideAllocatesNothing(t *testing.T) {
	cfg := twoLiterals()
	cfg.Request.Remove = []string{"x-internal"}
	f := build(t, cfg, phase.ScopeUpstream)
	h := outgoing()
	x := onRequest(t, h)
	ctx := t.Context()
	mustContinue(t, f, phase.OnUpstreamRequest, x)
	if n := testing.AllocsPerRun(200, func() { f.Handle(ctx, phase.OnUpstreamRequest, x) }); n != 0 {
		t.Errorf("Handle allocates %v per run, want 0", n)
	}
	resp := build(t, &v1alpha1.HeadersConfig{Response: &v1alpha1.HeaderResponseOps{
		Set: []v1alpha1.HeaderResponseSet{respLit("strict-transport-security", "max-age=31536000")},
	}}, phase.ScopeGateway)
	rx := onResponse(t, 200, http.Header{"Content-Type": {"application/json"}})
	mustContinue(t, resp, phase.OnResponse, rx)
	if n := testing.AllocsPerRun(200, func() { resp.Handle(ctx, phase.OnResponse, rx) }); n != 0 {
		t.Errorf("response Handle allocates %v per run, want 0", n)
	}
}

// TestComputedSideAllocations checks the budget of two computed values:
// the Filter adds one allocation to the evaluations, the slice the values
// share, within the budget's 2.
func TestComputedSideAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("cel-go's pools drop items at random under -race")
	}
	f := build(t, twoComputed(), phase.ScopeUpstream)
	h := outgoing()
	x := onRequest(t, h)
	ctx := t.Context()
	mustContinue(t, f, phase.OnUpstreamRequest, x)
	handle := testing.AllocsPerRun(200, func() { f.Handle(ctx, phase.OnUpstreamRequest, x) })
	eval := testing.AllocsPerRun(200, func() {
		for _, e := range f.request.set {
			_, _ = e.prog.EvalString(ctx, x.Vars())
		}
	})
	if handle > eval+1 {
		t.Errorf("Handle allocates %.0f per run, its evaluations %.0f: the Filter adds more than one", handle, eval)
	}
	// Over the prepared Consumer and Route the evaluations allocate
	// nothing, so setting the two values costs the shared slice alone.
	if handle != 1 || eval != 0 {
		t.Errorf("Handle allocates %.0f per run and its evaluations %.0f, want 1 and 0", handle, eval)
	}
}

// benchmarkSide measures Handle of cfg at Upstream scope in
// onUpstreamRequest over a typical outgoing header map, edited again on
// every iteration.
func benchmarkSide(b *testing.B, cfg *v1alpha1.HeadersConfig) {
	f := build(b, cfg, phase.ScopeUpstream)
	h := outgoing()
	x := onRequest(b, h)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if r := f.Handle(ctx, phase.OnUpstreamRequest, x); r.Outcome != filter.Continue {
			b.Fatalf("Handle = %+v, want Continue", r)
		}
	}
}

// BenchmarkSetTwoLiterals sets two literal values.
func BenchmarkSetTwoLiterals(b *testing.B) { benchmarkSide(b, twoLiterals()) }

// BenchmarkSetTwoComputed sets two computed values.
func BenchmarkSetTwoComputed(b *testing.B) { benchmarkSide(b, twoComputed()) }
