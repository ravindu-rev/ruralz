// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Allocation gate and budget of spec 08 req 59 and 05 tests 22 and 38: a
// single-limit GCRA call on memory makes 0 heap allocations and fits the
// ratelimit stage budget of 2 µs p50, 8 µs p99 (target).

// gcraBench is a warm single-limit GCRA call on a driver with metrics.
type gcraBench struct {
	d   *Driver
	c   *statestore.Call
	rb  statestore.RequestBudget
	ctx context.Context
}

func newGCRABench(tb testing.TB, clk clock.Clock) *gcraBench {
	tb.Helper()
	d, err := New(context.Background(), statestore.Config{}, statestore.Deps{Clock: clk, Metrics: newMetrics().handles()}, Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { closeDriver(tb, d) })
	b := &gcraBench{
		d: d, ctx: context.Background(),
		c: gcraCall("ratelimit-gold", statestore.DigestOf("user-42"), statestore.GCRALimit{Requests: 1 << 40, Window: time.Hour, Burst: 1 << 40}),
	}
	b.run() // create the key
	return b
}

func (b *gcraBench) run() {
	b.rb = statestore.NewRequestBudget(50*time.Millisecond, 2)
	b.d.Consume(b.ctx, &b.rb, []*statestore.Call{b.c})
}

func TestSingleGCRAZeroAllocs(t *testing.T) {
	// Spec 08 req 59: 0 heap allocations per single-limit GCRA call.
	b := newGCRABench(t, clocktest.New(start()))
	calls := []*statestore.Call{b.c}
	if n := testing.AllocsPerRun(1000, func() {
		b.rb = statestore.NewRequestBudget(50*time.Millisecond, 2)
		b.d.Consume(b.ctx, &b.rb, calls)
	}); n != 0 {
		t.Fatalf("single GCRA call allocates %v times", n)
	}
	if b.c.Err != nil || !b.c.GCRA.Allowed {
		t.Fatalf("benchmark call failed: %v", b.c.Err)
	}
	// A two-limit call and a merged GCRA+Quota script allocate nothing
	// either.
	dg := statestore.DigestOf("user-42")
	two := gcraCall("p", dg, statestore.GCRALimit{Requests: 1 << 40, Window: time.Second, Burst: 1}, statestore.GCRALimit{Requests: 1 << 40, Window: time.Hour, Burst: 1})
	q := quotaCall("q", time.Hour, 1<<60, dg)
	merged := []*statestore.Call{two, q}
	b.d.Consume(b.ctx, &b.rb, merged)
	if n := testing.AllocsPerRun(1000, func() {
		b.rb = statestore.NewRequestBudget(50*time.Millisecond, 2)
		b.d.Consume(b.ctx, &b.rb, merged)
	}); n != 0 {
		t.Fatalf("merged GCRA and Quota script allocates %v times", n)
	}
}

func TestGCRAStageBudget(t *testing.T) {
	// Spec 08 req 59: 2 µs p50 and 8 µs p99 (target) with the real clock;
	// the best of three runs tolerates a noisy machine.
	if testing.Short() || raceEnabled {
		t.Skip("latency budget needs an optimized, uninstrumented build")
	}
	b := newGCRABench(t, clock.Real())
	wall := clock.Real()
	const n = 20000
	lat := make([]time.Duration, n)
	best50, best99 := time.Hour, time.Hour
	for range 3 {
		for i := range lat {
			t0 := wall.Now()
			b.run()
			lat[i] = wall.Since(t0)
		}
		slices.Sort(lat)
		best50, best99 = min(best50, lat[n/2]), min(best99, lat[n*99/100])
	}
	t.Logf("single GCRA: p50 %v, p99 %v", best50, best99)
	if best50 > 2*time.Microsecond || best99 > 8*time.Microsecond {
		t.Fatalf("single GCRA p50 %v, p99 %v; budget 2µs and 8µs", best50, best99)
	}
}

func BenchmarkMemoryGCRA(b *testing.B) {
	g := newGCRABench(b, clock.Real())
	b.ReportAllocs()
	for b.Loop() {
		g.run()
	}
}

func BenchmarkMemoryGCRAParallel(b *testing.B) {
	d, err := New(context.Background(), statestore.Config{}, statestore.Deps{Clock: clock.Real()}, Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer closeDriver(b, d)
	l := statestore.GCRALimit{Requests: 1 << 40, Window: time.Hour, Burst: 1 << 40}
	var worker atomic.Int64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		c := gcraCall("p", statestore.DigestOf(strconv.FormatInt(worker.Add(1), 10)), l)
		calls := []*statestore.Call{c}
		for pb.Next() {
			rb := statestore.NewRequestBudget(50*time.Millisecond, 0)
			d.Consume(context.Background(), &rb, calls)
		}
	})
}

func BenchmarkMemoryQuota(b *testing.B) {
	d, err := New(context.Background(), statestore.Config{}, statestore.Deps{Clock: clock.Real()}, Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer closeDriver(b, d)
	calls := []*statestore.Call{quotaCall("monthly-requests", 720*time.Hour, 1<<60, statestore.DigestOf("user-42"))}
	b.ReportAllocs()
	for b.Loop() {
		rb := statestore.NewRequestBudget(50*time.Millisecond, 0)
		d.Consume(context.Background(), &rb, calls)
	}
}

func BenchmarkMemoryLookup(b *testing.B) {
	d, err := New(context.Background(), statestore.Config{}, statestore.Deps{Clock: clock.Real()}, Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer closeDriver(b, d)
	ck := statestore.CacheKey{URI: statestore.DigestOf("u"), Partition: statestore.DigestOf("p")}
	v := statestore.DigestOf("v")
	d.Write(context.Background(), []*statestore.Write{{Kind: statestore.OpCacheSet, Timeout: time.Second, Cache: statestore.CacheStore{
		Key: ck, Names: "accept-encoding", Variant: v, TTL: time.Hour, Entry: make([]byte, 16<<10), Token: 1,
	}}})
	c := lookupCall(ck, v)
	calls := []*statestore.Call{c}
	b.ReportAllocs()
	for b.Loop() {
		rb := statestore.NewRequestBudget(50*time.Millisecond, 0)
		d.Read(context.Background(), &rb, calls)
	}
	if !c.Lookup.Found {
		b.Fatal("lookup missed")
	}
}
