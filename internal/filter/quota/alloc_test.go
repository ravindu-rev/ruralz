// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// recordExchange is a filtertest.Exchange whose charged-counter record is
// one reused Charge, as the data plane's pooled record is once warm.
type recordExchange struct {
	*filtertest.Exchange
	rec filter.Charge
}

// Charge implements filter.Exchange: every counter is new, recorded in
// place.
func (x *recordExchange) Charge(c filter.Counter) (*filter.Charge, bool) {
	x.rec = filter.Charge{Counter: c, Policy: x.PolicyName}
	return &x.rec, true
}

// admitLoop is one admitted request as the executor runs it, over a
// reused exchange: Prepare, one Consume on the memory driver, Complete,
// onLog and Finish.
type admitLoop struct {
	fx    *fixture
	f     *Filter
	x     *recordExchange
	call  statestore.Call
	calls [1]*statestore.Call
	rb    statestore.RequestBudget
}

// newAdmitLoop returns a loop over a quota with the default key and cost
// that never runs out.
func newAdmitLoop(tb testing.TB) *admitLoop {
	tb.Helper()
	fx := newFixture(tb)
	f, _ := fx.build(tb, "quota-bench", config("daily"))
	x := &recordExchange{Exchange: exchange("quota-bench", vars(consumer("acme", windowQuota("daily", 1<<40, time.Hour)), "GET", ""))}
	x.FinalV = served()
	l := &admitLoop{fx: fx, f: f, x: x}
	l.calls[0] = &l.call
	return l
}

// run admits one request, or stops the test.
func (l *admitLoop) run(tb testing.TB, ctx context.Context) {
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
	l.f.Handle(ctx, phase.OnLog, l.x)
	l.f.Finish(ctx, l.x)
}

// TestAdmitAllocatesNothing checks that an admitted request, its
// reservation on the memory driver, its RateLimit field and its onLog
// allocate nothing once warm with the default key and cost.
func TestAdmitAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not compared under -race")
	}
	l := newAdmitLoop(t)
	ctx := t.Context()
	for range 10 {
		l.run(t, ctx)
	}
	if n := testing.AllocsPerRun(500, func() { l.run(t, ctx) }); n != 0 {
		t.Errorf("an admitted request allocates %v per run", n)
	}
}

// BenchmarkAdmit measures one admitted request on the memory driver.
func BenchmarkAdmit(b *testing.B) {
	l := newAdmitLoop(b)
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		l.run(b, ctx)
	}
}
