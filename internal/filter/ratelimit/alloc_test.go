// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The ratelimit stage budget of
// docs/architecture/12-performance-budgets-and-benchmarking.md "Per-stage
// latency budget": local token bucket shard, then GCRA on the memory
// driver, 2 µs at p50 and 8 µs at p99 with 0 allocations (targets).

// admitLoop is one admitted request as the executor's batch runs it, over
// a reused exchange: Prepare, one Consume on the memory driver, Complete
// and Finish.
type admitLoop struct {
	fx    *fixture
	f     *Filter
	x     *filtertest.Exchange
	call  statestore.Call
	calls [1]*statestore.Call
	rb    statestore.RequestBudget
}

// newAdmitLoop returns a loop over a 1,000,000 per second limit keyed by
// x-key, the S2 ratelimit-bench shape, which never denies.
func newAdmitLoop(tb testing.TB) *admitLoop {
	tb.Helper()
	fx := newFixture(tb, nil)
	f, _ := fx.build(tb, "ratelimit-bench", keyed(limit(1_000_000, time.Second)))
	l := &admitLoop{fx: fx, f: f, x: &filtertest.Exchange{VarsV: vars("client-1", "")}}
	l.calls[0] = &l.call
	return l
}

// run admits one request, or stops the test.
func (l *admitLoop) run(ctx context.Context, tb testing.TB) {
	l.x.RateLimits = l.x.RateLimits[:0]
	l.rb = statestore.NewRequestBudget(time.Second, 0)
	l.x.StateDeadline = &l.rb
	l.call = statestore.Call{Batch: -1}
	r, done := l.f.Prepare(ctx, l.x, &l.call)
	if !done {
		l.fx.store.Consume(ctx, &l.rb, l.calls[:])
		r = l.f.Complete(ctx, l.x, &l.call)
	}
	if r.Outcome != filter.Continue {
		tb.Fatalf("admitted request = %+v", r)
	}
	l.f.Finish(ctx, l.x)
}

// TestAdmitAddsNoAllocations checks the stage budget: an admitted
// request, its bucket, its GCRA call on the memory driver and its
// RateLimit field allocate nothing beyond the key's own evaluation once
// warm (cel-go boxes the header value it reads).
func TestAdmitAddsNoAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not compared under -race")
	}
	l := newAdmitLoop(t)
	ctx := t.Context()
	for range 10 {
		l.run(ctx, t)
	}
	key := testing.AllocsPerRun(500, func() { _, _ = l.f.key.EvalString(ctx, l.x.Vars()) })
	if n := testing.AllocsPerRun(500, func() { l.run(ctx, t) }); n > key {
		t.Errorf("an admitted request allocates %v per run, its key evaluation %v: the Filter adds allocations", n, key)
	}
}

// TestKeylessAdmitAllocatesNothing checks a service limit, which
// evaluates no expression: an admitted request allocates nothing.
func TestKeylessAdmitAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not compared under -race")
	}
	l := newAdmitLoop(t)
	f, _ := l.fx.build(t, "svc", &v1alpha1.RateLimitConfig{Limits: []v1alpha1.RateLimit{limit(1_000_000, time.Second)}})
	l.f = f
	ctx := t.Context()
	for range 10 {
		l.run(ctx, t)
	}
	if n := testing.AllocsPerRun(500, func() { l.run(ctx, t) }); n != 0 {
		t.Errorf("a keyless admitted request allocates %v per run, want 0", n)
	}
}

// TestLocalDenyAllocatesOnlyTheResponse checks that a local denial
// allocates only its response: the Response, its header map and the
// Retry-After value.
func TestLocalDenyAllocatesOnlyTheResponse(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not compared under -race")
	}
	fx := newFixture(t, nil)
	cfg := keyed(limit(1, time.Hour))
	cfg.ResponseHeaders = ptr(v1alpha1.RateLimitResponseHeadersNone)
	f, _ := fx.build(t, "rl", cfg)
	fx.request(t, f, vars("a", ""))
	x := &filtertest.Exchange{VarsV: vars("a", "")}
	ctx := t.Context()
	deny := func() {
		var call statestore.Call
		if r, done := f.Prepare(ctx, x, &call); !done || r.Outcome != filter.Respond {
			t.Fatalf("Prepare = %+v, %v; want a local denial", r, done)
		}
		f.Finish(ctx, x)
	}
	deny()
	key := testing.AllocsPerRun(200, func() { _, _ = f.key.EvalString(ctx, x.Vars()) })
	if n := testing.AllocsPerRun(200, deny); n-key > 5 {
		t.Errorf("a local denial allocates %v per run beyond its key's %v, want its response only (at most 5)", n-key, key)
	}
}

// BenchmarkAdmit measures an admitted request on the memory driver.
func BenchmarkAdmit(b *testing.B) {
	l := newAdmitLoop(b)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		l.run(ctx, b)
	}
}

// BenchmarkAdmitKeyless measures the stage budget's memory path alone: an
// admitted request on a service limit, whose local bucket and GCRA call on
// the memory driver allocate nothing, without a key expression's cost.
func BenchmarkAdmitKeyless(b *testing.B) {
	l := newAdmitLoop(b)
	f, _ := l.fx.build(b, "svc", &v1alpha1.RateLimitConfig{Limits: []v1alpha1.RateLimit{limit(1_000_000, time.Second)}})
	l.f = f
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		l.run(ctx, b)
	}
}

// BenchmarkLocalOnly measures an admission on local buckets alone, at the
// highest limit the configuration check accepts (an interval of 1µs).
func BenchmarkLocalOnly(b *testing.B) {
	fx := newFixture(b, nil)
	cfg := keyed(limit(1_000_000, time.Second))
	cfg.LocalOnly = ptr(true)
	f, _ := fx.build(b, "rl", cfg)
	x := &filtertest.Exchange{VarsV: vars("client-1", "")}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		x.RateLimits = x.RateLimits[:0]
		var call statestore.Call
		if r, done := f.Prepare(ctx, x, &call); !done || r.Outcome != filter.Continue {
			b.Fatalf("Prepare = %+v, %v", r, done)
		}
		f.Finish(ctx, x)
	}
}
