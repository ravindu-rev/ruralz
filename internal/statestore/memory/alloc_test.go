// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// gcraFixture is a warm single-limit GCRA call on a driver with metrics.
type gcraFixture struct {
	d   *Driver
	c   *statestore.Call
	rb  statestore.RequestBudget
	ctx context.Context
}

// newGCRAFixture returns the fixture on a driver with a controlled clock,
// with the call's key already created.
func newGCRAFixture(tb testing.TB) *gcraFixture {
	tb.Helper()
	d, err := New(context.Background(), statestore.Config{}, statestore.Deps{Clock: clocktest.New(start()), Metrics: newMetrics().handles()}, Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { closeDriver(tb, d) })
	f := &gcraFixture{
		d: d, ctx: context.Background(),
		c: gcraCall("ratelimit-gold", statestore.DigestOf("user-42"), statestore.GCRALimit{Requests: 1 << 40, Window: time.Hour, Burst: 1 << 40}),
	}
	f.rb = statestore.NewRequestBudget(50*time.Millisecond, 2)
	f.d.Consume(f.ctx, &f.rb, []*statestore.Call{f.c})
	return f
}

// TestSingleGCRAZeroAllocs checks that a single-limit GCRA call on the
// memory driver, a two-limit call and a merged GCRA and Quota script make
// no heap allocations.
func TestSingleGCRAZeroAllocs(t *testing.T) {
	f := newGCRAFixture(t)
	calls := []*statestore.Call{f.c}
	if n := testing.AllocsPerRun(1000, func() {
		f.rb = statestore.NewRequestBudget(50*time.Millisecond, 2)
		f.d.Consume(f.ctx, &f.rb, calls)
	}); n != 0 {
		t.Fatalf("single GCRA call allocates %v times", n)
	}
	if f.c.Err != nil || !f.c.GCRA.Allowed {
		t.Fatalf("GCRA call failed: %v", f.c.Err)
	}
	dg := statestore.DigestOf("user-42")
	two := gcraCall("p", dg, statestore.GCRALimit{Requests: 1 << 40, Window: time.Second, Burst: 1}, statestore.GCRALimit{Requests: 1 << 40, Window: time.Hour, Burst: 1})
	q := quotaCall(time.Hour, 1<<60, dg)
	merged := []*statestore.Call{two, q}
	f.d.Consume(f.ctx, &f.rb, merged)
	if n := testing.AllocsPerRun(1000, func() {
		f.rb = statestore.NewRequestBudget(50*time.Millisecond, 2)
		f.d.Consume(f.ctx, &f.rb, merged)
	}); n != 0 {
		t.Fatalf("merged GCRA and Quota script allocates %v times", n)
	}
}
