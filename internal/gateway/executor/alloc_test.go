// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Allocation gate and benchmark (PB-8: 30 or fewer Ruralz-owned
// allocations per S1 request around the Router and the executor, spec 04
// req 80; the per-stage budget gives Filters of onRequestHeaders, onRoute,
// onUpstreamRequest without attached work and the ratelimit memory path
// 0 allocations). The executor's share is zero: a pooled Run and LegRun,
// no per-call allocation, spans and metrics through preallocated handles.

// nopFilter continues and implements Finisher.
type nopFilter struct{}

func (*nopFilter) Handle(context.Context, phase.Phase, filter.Exchange) filter.Result {
	return filter.Next()
}

func (*nopFilter) Finish(context.Context, filter.Exchange) {}

// nopConsumer prepares one GCRA call and admits.
type nopConsumer struct{ nopFilter }

func (*nopConsumer) Prepare(_ context.Context, _ filter.Exchange, call *statestore.Call) (filter.Result, bool) {
	call.Kind = statestore.OpGCRA
	return filter.Result{}, false
}

func (*nopConsumer) Complete(context.Context, filter.Exchange, *statestore.Call) filter.Result {
	return filter.Next()
}

func (*nopConsumer) Undo(filter.Exchange) {}

// allocFixture is a 3-Policy chain (cors, ratelimit, headers) plus one
// upstream-leg headers Policy, on a fake state with a reusable leg state.
type allocFixture struct {
	e     *Executor
	st    *fakeState
	ch    *snapshot.Chain
	store *filtertest.Store
}

func newAllocFixture(tb testing.TB, tracer bool) *allocFixture {
	tb.Helper()
	d := Deps{}
	if tracer {
		d.Tracer = &noopTracer{}
	}
	cors := policy(spec{
		name: "cors", class: phase.ClassCORS, typ: v1alpha1.PolicyTypeCORS,
		phases: []phase.Phase{phase.OnRequestHeaders, phase.OnResponse}, f: &nopFilter{},
	})
	rl := policy(spec{
		name: "ratelimit", class: phase.ClassAdmission, typ: v1alpha1.PolicyTypeRateLimit,
		mode: v1alpha1.FailureModeOpen, phases: []phase.Phase{phase.OnRequestHeaders}, f: &nopConsumer{},
	})
	hdr := policy(spec{
		name: "headers", class: phase.ClassTransform,
		phases: []phase.Phase{phase.OnRequestHeaders, phase.OnRoute, phase.OnResponse, phase.OnLog}, f: &nopFilter{},
	})
	up := policy(spec{
		name: "up-headers", class: phase.ClassTransform, scope: phase.ScopeUpstream,
		phases: []phase.Phase{phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders}, f: &nopFilter{},
	})
	for _, p := range []*snapshot.Policy{cors, rl, hdr, up} {
		p.Metrics, _ = newProbe()
	}
	st := newState()
	st.spare = newState()
	return &allocFixture{
		e: New(d), st: st, store: &filtertest.Store{},
		ch: chain([]*snapshot.Policy{cors, rl, hdr}, map[string][]*snapshot.Policy{"up": {up}}),
	}
}

// request runs one request through every Phase.
func (f *allocFixture) request(ctx context.Context, leg *filter.Leg) {
	f.st.whens = [maxPolicies]uint8{}
	f.st.cur = nil
	r := f.e.Begin(f.st, f.ch, f.store)
	for _, ph := range []phase.Phase{phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute} {
		if r.Request(ctx, ph) != nil {
			panic("short-circuit")
		}
	}
	l := r.BeginLeg(ctx, "up", "")
	l.OnUpstreamRequest(ctx, leg)
	l.OnUpstreamResponseHeaders(ctx, leg)
	l.OnUpstreamResponseBody(ctx, leg)
	l.End(ctx)
	r.Response(ctx, ResponseUpstream)
	r.Log(ctx)
	r.Release()
}

func TestAllocsPerRunThreePolicyChain(t *testing.T) {
	// Done when (WP-20): AllocsPerRun on a 3-Policy chain within the PB-8
	// share, which is zero for the executor.
	if raceEnabled {
		t.Skip("sync.Pool drops items under -race; the allocation gate runs without it")
	}
	for _, tracer := range []bool{false, true} {
		f := newAllocFixture(t, tracer)
		ctx := context.Background()
		leg := &filter.Leg{Upstream: "up", Attempt: 1}
		f.request(ctx, leg) // warm the pools and scratch slices
		if n := testing.AllocsPerRun(200, func() { f.request(ctx, leg) }); n != 0 {
			t.Fatalf("tracer %v: %v allocations per request, want 0", tracer, n)
		}
	}
	// An empty chain costs nothing either (S1).
	e, st := New(Deps{}), newState()
	empty := chain(nil, nil)
	if n := testing.AllocsPerRun(200, func() {
		r := e.Begin(st, empty, nil)
		r.Request(context.Background(), phase.OnRequestHeaders)
		r.BeginLeg(context.Background(), "up", "").End(context.Background())
		r.Response(context.Background(), ResponseUpstream)
		r.Log(context.Background())
		r.Release()
	}); n != 0 {
		t.Fatalf("empty chain: %v allocations", n)
	}
}

func BenchmarkThreePolicyChain(b *testing.B) {
	f := newAllocFixture(b, true)
	ctx := context.Background()
	leg := &filter.Leg{Upstream: "up", Attempt: 1}
	b.ReportAllocs()
	for b.Loop() {
		f.request(ctx, leg)
	}
}

func BenchmarkEmptyChain(b *testing.B) {
	e, st := New(Deps{}), newState()
	ch := chain(nil, nil)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		r := e.Begin(st, ch, nil)
		for _, ph := range []phase.Phase{phase.OnRequestHeaders, phase.OnRequestBody, phase.OnRoute} {
			r.Request(ctx, ph)
		}
		r.Response(ctx, ResponseUpstream)
		r.Log(ctx)
		r.Release()
	}
}
