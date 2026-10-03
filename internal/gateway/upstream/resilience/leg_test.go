// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptrace"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
)

// legFixture is one Upstream runtime on a fake clock.
type legFixture struct {
	clk *clocktest.Fake
	rt  *Runtime
	cfg Config
}

func newLegFixture(src rand.Source) *legFixture {
	clk := newClock()
	return &legFixture{clk: clk, rt: NewRuntime(clk, src, nil), cfg: DefaultConfig()}
}

// begin starts a leg with the default 15 s Route timeout.
func (f *legFixture) begin(t *testing.T) *Leg {
	t.Helper()
	var l Leg
	if g := l.Begin(f.rt, &f.cfg, RouteDeadline(f.clk.Now(), 0), 1); g != GateNone {
		t.Fatalf("Begin: gate %d", g)
	}
	return &l
}

// wantRetry is a RetryInput whose every condition holds.
func wantRetry() RetryInput { return RetryInput{RetryOn: true, Replayable: true} }

// mustStart starts the leg's next attempt and fails the test when the
// breaker refuses it.
func mustStart(t *testing.T, l *Leg) Attempt {
	t.Helper()
	a, g := l.StartAttempt()
	if g != GateNone {
		t.Fatalf("StartAttempt: gate %d, state %v", g, l.rt.Breaker().State())
	}
	return a
}

// TestLegAttempts_05Req32: attempts counts retries (attempts: 2 allows
// three attempts, numbered from 1). 30 other legs in flight lift the retry
// budget to 6, so only attempts stops these legs.
func TestLegAttempts_05Req32(t *testing.T) {
	for _, attempts := range []int{0, 1, 2, 5} {
		f := newLegFixture(fixedSource{0})
		f.cfg.Retry.Attempts = attempts
		for range 30 {
			f.rt.Budget().BeginLeg()
		}
		l := f.begin(t)
		ran := 0
		for {
			a := mustStart(t, l)
			ran++
			if a.Number != ran {
				t.Fatalf("attempt number %d, want %d", a.Number, ran)
			}
			d := l.Decide(wantRetry())
			if !d.Retry {
				if d.Reason != ReasonAttempts {
					t.Fatalf("stopped for %v", d.Reason)
				}
				break
			}
		}
		if ran != attempts+1 || l.Attempts() != ran {
			t.Fatalf("attempts: %d ran %d attempts", attempts, ran)
		}
		l.End(LegSuccess)
	}
}

// TestLegAloneRetryCap_05Req34: a leg alone in flight makes at most
// max(3, floor(20% × 1)) = 3 retries whatever attempts says, because each
// of its retries stays in flight until the leg ends.
func TestLegAloneRetryCap_05Req34(t *testing.T) {
	f := newLegFixture(fixedSource{0})
	f.cfg.Retry.Attempts = 5
	l := f.begin(t)
	for n := 1; ; n++ {
		l.StartAttempt()
		d := l.Decide(wantRetry())
		if d.Retry {
			continue
		}
		if d.Reason != ReasonBudget || n != RetryBudgetMin+1 || l.Retries() != RetryBudgetMin {
			t.Fatalf("attempt %d stopped for %v holding %d retries", n, d.Reason, l.Retries())
		}
		break
	}
	l.End(LegFailure)
}

// TestLegDeadlines_05Req24 is 05 section 6 test 4: the default perTry is
// the time left divided by the retries left plus one at each attempt, the
// last attempt runs to the leg deadline, and the leg deadline covers the
// backoff.
func TestLegDeadlines_05Req24(t *testing.T) {
	f := newLegFixture(fixedSource{0})
	f.cfg.Retry.Attempts = 2
	var l Leg
	route := f.clk.Now().Add(9 * time.Second)
	if g := l.Begin(f.rt, &f.cfg, route, 1); g != GateNone {
		t.Fatal(g)
	}
	if !l.Deadline().Equal(route) || !l.Start().Equal(epoch) || !l.Active() {
		t.Fatalf("leg deadline %v", l.Deadline())
	}
	want := []struct {
		perTry time.Duration
		cause  error
	}{
		{3 * time.Second, ErrAttemptTimeout},
		{3 * time.Second, ErrAttemptTimeout},
		{3 * time.Second, ErrLegTimeout},
	}
	for i, w := range want {
		a := mustStart(t, &l)
		if a.PerTry != w.perTry || !a.Deadline.Equal(a.Start.Add(w.perTry)) || !errors.Is(a.Cause, w.cause) {
			t.Fatalf("attempt %d: %+v", i+1, a)
		}
		f.clk.Advance(w.perTry) // the attempt times out
		if i < 2 {
			if d := l.Decide(wantRetry()); !d.Retry {
				t.Fatalf("attempt %d: no retry: %v", i+1, d.Reason)
			}
		}
	}
	if !l.Expired() || SelectCode(l.Outcome(false, KindTimeout)) != "RZ-UP-003" {
		t.Fatal("the expired leg does not select RZ-UP-003")
	}
	l.End(LegFailure)

	t.Run("configured perTryTimeout and Upstream timeout", func(t *testing.T) {
		f := newLegFixture(fixedSource{0})
		f.cfg.Timeout = 2 * time.Second
		f.cfg.Retry.PerTryTimeout = 500 * time.Millisecond
		l := f.begin(t)
		if !l.Deadline().Equal(epoch.Add(2 * time.Second)) {
			t.Fatalf("leg deadline %v", l.Deadline())
		}
		a := mustStart(t, l)
		if a.PerTry != 500*time.Millisecond || !a.Deadline.Equal(epoch.Add(500*time.Millisecond)) || !errors.Is(a.Cause, ErrAttemptTimeout) {
			t.Fatalf("attempt %+v", a)
		}
		f.clk.Advance(1800 * time.Millisecond)
		a = mustStart(t, l)
		if !a.Deadline.Equal(l.Deadline()) || !errors.Is(a.Cause, ErrLegTimeout) {
			t.Fatalf("clamped attempt %+v", a)
		}
		l.End(LegSuccess)
	})
	t.Run("no Route bound", func(t *testing.T) {
		f := newLegFixture(fixedSource{0})
		var l Leg
		l.Begin(f.rt, &f.cfg, time.Time{}, 1)
		a := mustStart(t, &l)
		if !a.Deadline.IsZero() || a.PerTry != 0 || !errors.Is(a.Cause, ErrAttemptTimeout) || l.Expired() {
			t.Fatalf("unbounded attempt %+v", a)
		}
		if d := l.Decide(wantRetry()); !d.Retry {
			t.Fatalf("unbounded leg: %v", d.Reason)
		}
		l.End(LegSuccess)
	})
}

// TestLegDecide_05Req33 is the retry-condition part of 05 section 6
// test 3.
func TestLegDecide_05Req33(t *testing.T) {
	later := func(d time.Duration) string { return epoch.Add(d).Format(http.TimeFormat) }
	tests := []struct {
		name      string
		src       rand.Source
		route     time.Duration // Route timeout
		in        RetryInput
		reason    Reason
		wantDelay time.Duration
	}{
		{name: "retryOn false", in: RetryInput{Replayable: true}, reason: ReasonNotWanted},
		{name: "retryOn true", in: wantRetry(), reason: ReasonRetry},
		{name: "Filter-requested retry without retryOn", in: RetryInput{FilterRetry: true, Replayable: true}, reason: ReasonRetry},
		{name: "committed", in: RetryInput{RetryOn: true, Replayable: true, Committed: true}, reason: ReasonCommitted},
		{name: "streamed body after bytes were read", in: RetryInput{RetryOn: true}, reason: ReasonNotReplayable},
		{name: "Filter retry with a non-replayable body", in: RetryInput{FilterRetry: true}, reason: ReasonNotReplayable},
		{name: "Retry-After 11", in: RetryInput{RetryOn: true, Replayable: true, RetryAfter: "11"}, reason: ReasonRetryAfter},
		{name: "Retry-After 10", in: RetryInput{RetryOn: true, Replayable: true, RetryAfter: "10"}, reason: ReasonRetry, wantDelay: 10 * time.Second},
		{name: "Retry-After 2", in: RetryInput{RetryOn: true, Replayable: true, RetryAfter: "2"}, reason: ReasonRetry, wantDelay: 2 * time.Second},
		{name: "Retry-After 2 beyond the leg deadline", route: 1500 * time.Millisecond, in: RetryInput{RetryOn: true, Replayable: true, RetryAfter: "2"}, reason: ReasonDeadline},
		{name: "Retry-After HTTP-date", in: RetryInput{RetryOn: true, Replayable: true, RetryAfter: later(3 * time.Second)}, reason: ReasonRetry, wantDelay: 3 * time.Second},
		{name: "Retry-After HTTP-date past", in: RetryInput{RetryOn: true, Replayable: true, RetryAfter: later(-time.Minute)}, reason: ReasonRetry},
		{name: "Retry-After HTTP-date beyond 10 s", in: RetryInput{RetryOn: true, Replayable: true, RetryAfter: later(time.Minute)}, reason: ReasonRetryAfter},
		{name: "invalid Retry-After ignored", in: RetryInput{RetryOn: true, Replayable: true, RetryAfter: "later"}, reason: ReasonRetry},
		{name: "longer backoff wins over Retry-After 0", src: fixedSource{math.MaxUint64}, in: RetryInput{RetryOn: true, Replayable: true, RetryAfter: "0"}, reason: ReasonRetry, wantDelay: 25 * time.Millisecond},
		{name: "backoff past the leg deadline", src: fixedSource{math.MaxUint64}, route: 20 * time.Millisecond, in: wantRetry(), reason: ReasonDeadline},
		{name: "backoff ending at the leg deadline", src: fixedSource{math.MaxUint64}, route: 25 * time.Millisecond, in: wantRetry(), reason: ReasonDeadline},
		{name: "full-jitter backoff", src: fixedSource{math.MaxUint64}, in: wantRetry(), reason: ReasonRetry, wantDelay: 25 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := tt.src
			if src == nil {
				src = fixedSource{0}
			}
			f := newLegFixture(src)
			var l Leg
			l.Begin(f.rt, &f.cfg, RouteDeadline(f.clk.Now(), tt.route), 1)
			l.StartAttempt()
			d := l.Decide(tt.in)
			if d.Reason != tt.reason || d.Retry != (tt.reason == ReasonRetry) || d.Delay != tt.wantDelay {
				t.Fatalf("Decide = %+v (%v), want %v after %v", d, d.Reason, tt.reason, tt.wantDelay)
			}
			if (l.Retries() == 1) != d.Retry {
				t.Fatalf("Retries() = %d", l.Retries())
			}
			l.End(LegSuccess)
			if o, r := f.rt.Budget().InFlight(); o != 0 || r != 0 {
				t.Fatalf("budget left %d, %d", o, r)
			}
		})
	}
}

// TestLegBackoffGrows_05Req33: retry n draws from [0, min(250 ms,
// 25 ms × 2^(n−1))].
func TestLegBackoffGrows_05Req33(t *testing.T) {
	f := newLegFixture(fixedSource{math.MaxUint64})
	f.cfg.Retry.Attempts = 7
	f.cfg.Timeout = time.Hour
	for range 40 { // budget room for seven retries
		f.rt.Budget().BeginLeg()
	}
	l := f.begin(t)
	for n := 1; n <= 7; n++ {
		l.StartAttempt()
		d := l.Decide(wantRetry())
		if !d.Retry || d.Delay != BackoffCeiling(n) {
			t.Fatalf("retry %d: %+v", n, d)
		}
		f.clk.Advance(d.Delay)
	}
	l.End(LegSuccess)
}

// TestLegBreakerOpen_05Req33: no retry while the breaker is open.
func TestLegBreakerOpen_05Req33(t *testing.T) {
	f := newLegFixture(fixedSource{0})
	l := f.begin(t)
	l.StartAttempt()
	for range 20 {
		var other Leg
		other.Begin(f.rt, &f.cfg, time.Time{}, 1)
		other.StartAttempt()
		other.End(LegFailure)
	}
	if f.rt.Breaker().State() != StateOpen {
		t.Fatal("the breaker did not open")
	}
	if d := l.Decide(wantRetry()); d.Reason != ReasonBreakerOpen {
		t.Fatalf("Decide = %v", d.Reason)
	}
	l.End(LegFailure) // a stale permit: ignored
	var refused Leg
	if g := refused.Begin(f.rt, &f.cfg, time.Time{}, 1); g != GateBreaker || refused.Active() {
		t.Fatalf("Begin with the breaker open: gate %d", g)
	}
	if d := refused.Decide(wantRetry()); d.Retry {
		t.Fatal("an idle leg retried")
	}
	refused.End(LegFailure) // no-op
	if o, r := f.rt.Budget().InFlight(); o != 0 || r != 0 {
		t.Fatalf("budget %d, %d", o, r)
	}
	if code := SelectCode(Outcome{Gate: GateBreaker}); code != "RZ-UP-005" {
		t.Fatal(code)
	}
}

// TestLegHalfOpenOldLeg_05Req37 is the review's case: with openDuration 0
// a leg admitted while the breaker was closed is still running when 20
// failing legs open it and a new leg becomes the half-open probe. The old
// leg must not retry beside the probe (05 req 11 step e, reqs 35 and 37);
// the probe may.
func TestLegHalfOpenOldLeg_05Req37(t *testing.T) {
	f := newLegFixture(fixedSource{0})
	f.cfg.Breaker.OpenDuration = 0
	f.cfg.Timeout = time.Hour
	old := f.begin(t)
	old.StartAttempt()
	for range 20 {
		l := f.begin(t)
		l.StartAttempt()
		l.End(LegFailure)
	}
	if d := old.Decide(wantRetry()); d.Reason != ReasonBreakerOpen {
		t.Fatalf("old leg while open: %v", d.Reason)
	}
	probe := f.begin(t)
	if !probe.Probe() || f.rt.Breaker().State() != StateHalfOpen {
		t.Fatalf("no probe: state %v", f.rt.Breaker().State())
	}
	if d := old.Decide(wantRetry()); d.Retry || d.Reason != ReasonBreakerOpen {
		t.Fatalf("old leg retried beside the probe: %+v", d)
	}
	probe.StartAttempt()
	if d := probe.Decide(wantRetry()); !d.Retry {
		t.Fatalf("the probe may not retry: %v", d.Reason)
	}
	o := old.Outcome(false, KindReset)
	if o.Attempts != 1 || SelectCode(o) != CodeReset {
		t.Fatalf("the refused retry did not keep the previous outcome: %+v", o)
	}
	old.End(BreakerResult(true, o, 0, 1)) // stale permit: ignored
	probe.End(LegSuccess)
	if st := f.rt.Breaker().State(); st != StateHalfOpen {
		t.Fatalf("state %v after one probe success", st)
	}
}

// TestLegBeginGateOrder_05Req11: an empty Endpoint set (step a) is checked
// before the breaker (step e): RZ-UP-008 even with the breaker open, and a
// half-open breaker keeps its probe for a leg that can run.
func TestLegBeginGateOrder_05Req11(t *testing.T) {
	f := newLegFixture(fixedSource{0})
	var l Leg
	if g := l.Begin(f.rt, &f.cfg, time.Time{}, 0); g != GateNoEndpoint || l.Active() || g.Code() != CodeNoEndpoint {
		t.Fatalf("closed breaker, no Endpoint: gate %d", g)
	}
	for range 20 {
		l := f.begin(t)
		l.StartAttempt()
		l.End(LegFailure)
	}
	if g := l.Begin(f.rt, &f.cfg, time.Time{}, 0); g != GateNoEndpoint {
		t.Fatalf("open breaker, no Endpoint: gate %d", g)
	}
	f.clk.Advance(24 * time.Second)
	if g := l.Begin(f.rt, &f.cfg, time.Time{}, 0); g != GateNoEndpoint || f.rt.Breaker().State() != StateOpen {
		t.Fatalf("no Endpoint after openDuration: gate %d, state %v", g, f.rt.Breaker().State())
	}
	l.End(LegFailure) // no-op: the leg did not start
	if probe := f.begin(t); !probe.Probe() {
		t.Fatal("an empty-set leg spent the half-open probe")
	}
	if o, r := f.rt.Budget().InFlight(); o != 1 || r != 0 {
		t.Fatalf("budget %d, %d", o, r)
	}
}

// TestLegBeginReuse_05Req37: Begin on a Leg that is still active ends it
// first without counting, so a pooled Leg reused without End frees its
// half-open probe slot and its retry budget units instead of leaking them.
func TestLegBeginReuse_05Req37(t *testing.T) {
	f := newLegFixture(fixedSource{0})
	for range 20 {
		l := f.begin(t)
		l.StartAttempt()
		l.End(LegFailure)
	}
	f.clk.Advance(24 * time.Second)
	var l Leg
	if g := l.Begin(f.rt, &f.cfg, time.Time{}, 1); g != GateNone || !l.Probe() {
		t.Fatalf("no probe: gate %d", g)
	}
	l.StartAttempt()
	if d := l.Decide(wantRetry()); !d.Retry {
		t.Fatal(d.Reason)
	}
	// Reused without End: the old probe ends not counted, the slot is free.
	if g := l.Begin(f.rt, &f.cfg, time.Time{}, 1); g != GateNone || !l.Probe() || l.Attempts() != 0 || l.Retries() != 0 {
		t.Fatalf("reused leg: gate %d, probe %v", g, l.Probe())
	}
	if o, r := f.rt.Budget().InFlight(); o != 1 || r != 0 {
		t.Fatalf("budget %d, %d after reuse", o, r)
	}
	l.End(LegSuccess)
	if st := f.rt.Breaker().Stats(); st.State != StateHalfOpen {
		t.Fatalf("state %v", st.State)
	}
}

// TestLegBudget_05Req34: over budget the leg keeps its original failure
// and the reason names the budget; every retry of a leg holds its own unit
// from its backoff start until the leg ends, so a leg's second retry is a
// second retry in flight.
func TestLegBudget_05Req34(t *testing.T) {
	f := newLegFixture(fixedSource{0})
	f.cfg.Retry.Attempts = 3
	holders := make([]*Leg, 3)
	for i := range holders {
		holders[i] = f.begin(t)
		holders[i].StartAttempt()
		if d := holders[i].Decide(wantRetry()); !d.Retry {
			t.Fatalf("holder %d: %v", i, d.Reason)
		}
	}
	l := f.begin(t)
	l.StartAttempt()
	if d := l.Decide(wantRetry()); d.Reason != ReasonBudget || l.Retries() != 0 {
		t.Fatalf("over budget: %+v", d)
	}
	if o, r := f.rt.Budget().InFlight(); o != 4 || r != 3 {
		t.Fatalf("InFlight %d, %d", o, r)
	}
	// A holder's second retry needs a unit of its own: refused while the
	// budget is full.
	holders[0].StartAttempt()
	if d := holders[0].Decide(wantRetry()); d.Reason != ReasonBudget || holders[0].Retries() != 1 {
		t.Fatalf("second retry within a full budget: %+v", d)
	}
	holders[1].End(LegFailure) // frees one unit
	if d := holders[0].Decide(wantRetry()); !d.Retry || holders[0].Retries() != 2 {
		t.Fatalf("second retry after a unit freed: %v", d.Reason)
	}
	if o, r := f.rt.Budget().InFlight(); o != 3 || r != 3 {
		t.Fatalf("InFlight %d, %d with a leg on its second retry", o, r)
	}
	holders[0].End(LegFailure) // releases both of its units
	if o, r := f.rt.Budget().InFlight(); o != 2 || r != 1 {
		t.Fatalf("InFlight %d, %d after a two-retry leg ended", o, r)
	}
	if d := l.Decide(wantRetry()); !d.Retry {
		t.Fatalf("after units freed: %v", d.Reason)
	}
	holders[2].End(LegSuccess)
	l.End(LegSuccess)
	if o, r := f.rt.Budget().InFlight(); o != 0 || r != 0 {
		t.Fatalf("InFlight %d, %d", o, r)
	}
}

// TestLegBudgetAmplification_05Req34 is the review's example: 15 legs in
// flight, each wanting up to three retries. Counting every retry from its
// backoff start to its leg's end, no more than max(3, floor(20% × 15)) = 3
// retries are ever in flight, so amplification stays at most 1.2×, and
// from 15 to 200 legs the retries in flight stay within the limit.
func TestLegBudgetAmplification_05Req34(t *testing.T) {
	for n := 15; n <= 200; n += 37 {
		f := newLegFixture(fixedSource{0})
		f.cfg.Retry.Attempts = 3
		f.cfg.Timeout = time.Hour
		legs := make([]*Leg, n)
		for i := range legs {
			legs[i] = f.begin(t)
			legs[i].StartAttempt()
		}
		for range 3 { // every leg tries its first, second and third retry
			for _, l := range legs {
				if l.Decide(wantRetry()).Retry {
					l.StartAttempt()
				}
				o, r := f.rt.Budget().InFlight()
				if r > RetryBudgetLimit(o) || 5*(o+r) > 6*o {
					t.Fatalf("%d legs: %d retries in flight, limit %d", n, r, RetryBudgetLimit(o))
				}
			}
		}
		held := 0
		for _, l := range legs {
			held += l.Retries()
		}
		if _, r := f.rt.Budget().InFlight(); r != held || r != RetryBudgetLimit(n) {
			t.Fatalf("%d legs: %d retries in flight, legs hold %d, limit %d", n, r, held, RetryBudgetLimit(n))
		}
		for _, l := range legs {
			l.End(LegFailure)
		}
		if o, r := f.rt.Budget().InFlight(); o != 0 || r != 0 {
			t.Fatalf("%d legs: InFlight %d, %d after every leg ended", n, o, r)
		}
	}
}

// TestLegAbandonAttempt_05Req35: a retry that cannot start ends the leg
// with the previous attempt's outcome; a first attempt that cannot start
// ends it with the gate's code, not counted (05 req 37). AbandonAttempt
// uncounts only the attempt StartAttempt last counted.
func TestLegAbandonAttempt_05Req35(t *testing.T) {
	tests := []struct {
		name string
		gate Gate
		code string
	}{
		{"bulkhead full", GateBulkhead, "RZ-UP-006"},
		{"no Endpoint", GateNoEndpoint, "RZ-UP-008"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLegFixture(fixedSource{0})
			l := f.begin(t)
			mustStart(t, l)
			if d := l.Decide(wantRetry()); !d.Retry {
				t.Fatal(d.Reason)
			}
			if a := mustStart(t, l); a.Number != 2 {
				t.Fatalf("retry attempt %d", a.Number)
			}
			l.AbandonAttempt(tt.gate)
			l.AbandonAttempt(tt.gate) // nothing left to abandon
			o := l.Outcome(false, KindReset)
			if o.Attempts != 1 || o.Gate != tt.gate || SelectCode(o) != CodeReset {
				t.Fatalf("outcome %+v selects %s", o, SelectCode(o))
			}
			l.End(BreakerResult(true, o, 0, 2))

			ran := f.begin(t)
			mustStart(t, ran)
			ran.Decide(RetryInput{})
			ran.AbandonAttempt(tt.gate) // the attempt ran: kept
			if o := ran.Outcome(false, KindReset); o.Attempts != 1 || o.Gate != GateNone {
				t.Fatalf("an attempt that ran was abandoned: %+v", o)
			}
			ran.End(LegFailure)

			var first Leg
			if g := first.Begin(f.rt, &f.cfg, time.Time{}, 1); g != GateNone {
				t.Fatal(g)
			}
			mustStart(t, &first)
			first.AbandonAttempt(tt.gate)
			first.AbandonAttempt(GateNone)
			o = first.Outcome(false, KindNone)
			if o.Attempts != 0 || o.Gate != tt.gate {
				t.Fatalf("first attempt refused: %+v", o)
			}
			if code, _ := errcode.CodeOf(o.Err(nil)); code != tt.code {
				t.Fatalf("first attempt refused: %s, want %s", code, tt.code)
			}
			if BreakerResult(true, o, 0, 2) != LegNotCounted {
				t.Fatal("a gated leg counted")
			}
			first.End(LegNotCounted)
		})
	}
}

// openBreaker ends 20 failing legs, which opens a default breaker (05 req
// 37: minimumLegs 20, failureRatio 0.5, consecutiveFailures 5).
func openBreaker(t *testing.T, f *legFixture) {
	t.Helper()
	for range 20 {
		l := f.begin(t)
		mustStart(t, l)
		l.End(LegFailure)
	}
	if st := f.rt.Breaker().State(); st != StateOpen {
		t.Fatalf("the breaker did not open: %v", st)
	}
}

// TestLegRetryDelaySpansBreaker_05Req35 is the residual of review finding
// 1: a leg decides to retry while the breaker is closed and sleeps for a
// Retry-After delay, during which the breaker opens (and, with a short
// openDuration, goes half-open with another leg as its probe). When the
// delay ends the retry attempt must not start: StartAttempt applies the
// breaker gate of 05 req 11 step e again, the leg keeps the previous
// attempt's outcome (05 req 35), no retry runs while the breaker is open
// and none beside the probe (05 req 37), while the probe itself may retry.
func TestLegRetryDelaySpansBreaker_05Req35(t *testing.T) {
	tests := []struct {
		name         string
		openDuration time.Duration // 0: the default 30 s
		retryAfter   string
		probeAfter   time.Duration // a new leg begins this long into the delay; 0: none
		state        State         // the breaker when the delay ends
	}{
		{name: "delay spans the open transition", retryAfter: "2", state: StateOpen},
		{name: "delay spans open to half-open with another probe", openDuration: time.Second, retryAfter: "5", probeAfter: 2 * time.Second, state: StateHalfOpen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLegFixture(fixedSource{0})
			if tt.openDuration > 0 {
				f.cfg.Breaker.OpenDuration = tt.openDuration
			}
			old := f.begin(t)
			mustStart(t, old)
			d := old.Decide(RetryInput{RetryOn: true, Replayable: true, RetryAfter: tt.retryAfter})
			if !d.Retry || d.Delay < 2*time.Second {
				t.Fatalf("Decide = %+v (%v)", d, d.Reason)
			}
			openBreaker(t, f)
			var probe *Leg
			if tt.probeAfter > 0 {
				f.clk.Advance(tt.probeAfter)
				probe = f.begin(t)
				if !probe.Probe() {
					t.Fatal("the new leg is not the half-open probe")
				}
				mustStart(t, probe)
				f.clk.Advance(d.Delay - tt.probeAfter)
			} else {
				f.clk.Advance(d.Delay)
			}
			if st := f.rt.Breaker().State(); st != tt.state {
				t.Fatalf("state %v when the delay ended, want %v", st, tt.state)
			}
			a, g := old.StartAttempt()
			if g != GateBreaker || a != (Attempt{}) || old.Attempts() != 1 {
				t.Fatalf("the retry started: gate %d, attempt %+v, attempts %d", g, a, old.Attempts())
			}
			old.AbandonAttempt(GateBulkhead) // nothing to abandon: the attempt was not counted
			o := old.Outcome(false, KindReset)
			if o.Attempts != 1 || o.Gate != GateBreaker || SelectCode(o) != CodeReset {
				t.Fatalf("the refused retry did not keep the previous outcome: %+v selects %s", o, SelectCode(o))
			}
			old.End(BreakerResult(true, o, 0, 1)) // stale permit: ignored
			if probe != nil {
				if d := probe.Decide(wantRetry()); !d.Retry {
					t.Fatalf("the probe may not retry: %v", d.Reason)
				}
				f.clk.Advance(d.Delay)
				if a := mustStart(t, probe); a.Number != 2 {
					t.Fatalf("probe retry attempt %d", a.Number)
				}
				probe.End(LegSuccess)
			}
			if st := f.rt.Breaker().State(); st != tt.state {
				t.Fatalf("state %v after the legs ended, want %v", st, tt.state)
			}
			if o, r := f.rt.Budget().InFlight(); o != 0 || r != 0 {
				t.Fatalf("budget %d, %d", o, r)
			}
		})
	}
}

// TestLegStartAttemptBreakerGate_05Req11: the breaker gate of 05 req 11
// step e applies to every attempt. A first attempt the breaker refuses (it
// opened after Begin) ends the leg with RZ-UP-005, not counted (05 req
// 37); a probe whose lease another leg took over can start no attempt; a
// leg admitted before the breaker opened may retry again once a probe
// closed it.
func TestLegStartAttemptBreakerGate_05Req11(t *testing.T) {
	t.Run("first attempt after the breaker opened", func(t *testing.T) {
		f := newLegFixture(fixedSource{0})
		l := f.begin(t)
		openBreaker(t, f)
		if _, g := l.StartAttempt(); g != GateBreaker || l.Attempts() != 0 {
			t.Fatalf("gate %d, attempts %d", g, l.Attempts())
		}
		o := l.Outcome(false, KindNone)
		if code, _ := errcode.CodeOf(o.Err(nil)); code != CodeBreakerOpen || !errors.Is(o.Err(nil), ErrBreakerOpen) {
			t.Fatalf("outcome %+v: code %s", o, code)
		}
		if BreakerResult(true, o, 0, 1) != LegNotCounted {
			t.Fatal("a gated leg counted")
		}
		l.End(LegNotCounted)
	})
	t.Run("probe whose lease another leg took", func(t *testing.T) {
		f := newLegFixture(fixedSource{0})
		openBreaker(t, f)
		f.clk.Advance(DefaultOpenDuration * 6 / 5)
		lost := f.begin(t)
		mustStart(t, lost)
		f.clk.Advance(lost.Deadline().Sub(f.clk.Now()) + DefaultOpenDuration)
		next := f.begin(t)
		if !next.Probe() {
			t.Fatal("the lease did not pass to the next leg")
		}
		if _, g := lost.StartAttempt(); g != GateBreaker || lost.Attempts() != 1 {
			t.Fatalf("the lost probe started an attempt: gate %d", g)
		}
		mustStart(t, next)
		lost.End(LegFailure) // stale permit: ignored
		next.End(LegSuccess)
		if st := f.rt.Breaker().State(); st != StateHalfOpen {
			t.Fatalf("state %v", st)
		}
	})
	t.Run("closed again during the delay", func(t *testing.T) {
		f := newLegFixture(fixedSource{0})
		f.cfg.Breaker.OpenDuration = time.Second
		old := f.begin(t)
		mustStart(t, old)
		d := old.Decide(RetryInput{RetryOn: true, Replayable: true, RetryAfter: "10"})
		if !d.Retry {
			t.Fatal(d.Reason)
		}
		openBreaker(t, f)
		f.clk.Advance(2 * time.Second)
		for range DefaultHalfOpenSuccesses {
			probe := f.begin(t)
			mustStart(t, probe)
			probe.End(LegSuccess)
		}
		if st := f.rt.Breaker().State(); st != StateClosed {
			t.Fatalf("state %v", st)
		}
		f.clk.Advance(d.Delay - 2*time.Second)
		if a := mustStart(t, old); a.Number != 2 {
			t.Fatalf("retry attempt %d", a.Number)
		}
		old.End(LegFailure) // stale permit: ignored
		if st := f.rt.Breaker().Stats(); st.State != StateClosed || st.WindowLegs != 0 {
			t.Fatalf("stats %+v", st)
		}
	})
}

// TestLegProbe_05Req37: a half-open probe leg that ends without counting
// frees the probe slot for the next leg.
func TestLegProbe_05Req37(t *testing.T) {
	f := newLegFixture(fixedSource{0})
	for range 20 {
		l := f.begin(t)
		l.StartAttempt()
		l.End(LegFailure)
	}
	f.clk.Advance(24 * time.Second)
	probe := f.begin(t)
	if !probe.Probe() {
		t.Fatal("the first leg after openDuration is not the probe")
	}
	var other Leg
	if g := other.Begin(f.rt, &f.cfg, time.Time{}, 1); g != GateBreaker {
		t.Fatal("a second leg passed with the probe in flight")
	}
	probe.End(LegNotCounted)
	next := f.begin(t)
	if !next.Probe() {
		t.Fatal("the probe slot was not freed")
	}
	next.End(LegSuccess)
}

// TestLegReuse: End returns the Leg to idle for pooled reuse, releasing
// the retry budget (05 req 34).
func TestLegReuse(t *testing.T) {
	f := newLegFixture(fixedSource{0})
	var l Leg
	for range 3 {
		l.Begin(f.rt, &f.cfg, time.Time{}, 1)
		l.StartAttempt()
		l.Decide(wantRetry())
		l.End(LegSuccess)
		if l.Active() || l.Attempts() != 0 || l.Retries() != 0 || !l.Deadline().IsZero() || l.Expired() {
			t.Fatalf("leg not idle after End: %+v", l)
		}
	}
	if f.rt.Clock() != f.clk || f.rt.Bulkhead() == nil {
		t.Fatal("runtime accessors")
	}
}

// TestLegLoopRealSockets drives whole legs over real sockets: classification,
// the default retryOn and failureWhen, the retry rule and code selection
// together (05 reqs 32, 33, 39, 40).
func TestLegLoopRealSockets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		method   string
		attempts int
		want     string
		wantRan  int
	}{
		{"GET connect errors retried then RZ-UP-007", http.MethodGet, 2, "RZ-UP-007", 3},
		{"POST connect error retried", http.MethodPost, 1, "RZ-UP-007", 2},
		{"no retries: RZ-UP-001", http.MethodGet, 0, "RZ-UP-001", 1},
	}
	addr := closedAddr(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLegFixture(fixedSource{0})
			f.cfg.Retry.Attempts = tt.attempts
			tr := transport(t, nil, nil)
			var l Leg
			if g := l.Begin(f.rt, &f.cfg, RouteDeadline(f.clk.Now(), 0), 1); g != GateNone {
				t.Fatal(g)
			}
			var (
				kind   Kind
				failed bool
			)
			for {
				a := mustStart(t, &l)
				ctx, d := WithDeadline(context.Background(), f.rt.Clock(), a.Deadline, a.Cause)
				if err := f.rt.Bulkhead().Acquire(ctx, &f.cfg.Bulkhead); err != nil {
					t.Fatal(err)
				}
				var st StageTrace
				req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, st.ClientTrace()), tt.method, "http://"+addr+"/", http.NoBody)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := tr.RoundTrip(req)
				if resp != nil {
					_ = resp.Body.Close()
					t.Fatal("got a response")
				}
				kind = ClassifyStage(ctx, err, st.Stage()) // before Cancel
				f.rt.Bulkhead().Release()
				d.Cancel()
				v := legVars(tt.method, kind, 0)
				v.Attempt = a.Number
				failed, _ = EvalFailureWhen(ctx, nil, v)
				retry, _ := EvalRetryOn(ctx, nil, v)
				dec := l.Decide(RetryInput{RetryOn: retry, Replayable: true})
				if !dec.Retry {
					break
				}
			}
			o := l.Outcome(false, kind)
			if got := SelectCode(o); got != tt.want || o.Attempts != tt.wantRan {
				t.Fatalf("code %s after %d attempts, want %s after %d", got, o.Attempts, tt.want, tt.wantRan)
			}
			if code, _ := errcode.CodeOf(o.Err(nil)); code != tt.want {
				t.Fatalf("Err code %s", code)
			}
			l.End(BreakerResult(failed, o, 0, 1))
		})
	}
}

// BenchmarkLegDecide measures one leg's bookkeeping: begin, an attempt, a
// retry decision and the end.
func BenchmarkLegDecide(b *testing.B) {
	clk := newClock()
	rt := NewRuntime(clk, fixedSource{1 << 60}, nil)
	cfg := DefaultConfig()
	route := RouteDeadline(clk.Now(), 0)
	v := &expr.Vars{Request: &expr.Request{Method: http.MethodGet}, Error: &expr.AttemptError{Kind: "reset"}}
	b.ReportAllocs()
	for b.Loop() {
		var l Leg
		l.Begin(rt, &cfg, route, 3)
		l.StartAttempt()
		retry, _ := EvalRetryOn(context.Background(), nil, v)
		l.Decide(RetryInput{RetryOn: retry, Replayable: true})
		l.End(LegSuccess)
	}
}

// TestReasonString names every retry decision reason (05 req 33).
func TestReasonString(t *testing.T) {
	want := []string{"retry", "not_wanted", "committed", "not_replayable", "attempts", "retry_after", "deadline", "breaker_open", "budget"}
	for i, w := range want {
		if got := Reason(i).String(); got != w {
			t.Errorf("Reason(%d) = %q, want %q", i, got, w)
		}
	}
	if got := (ReasonBudget + 1).String(); got != "unknown" {
		t.Errorf("unknown reason: %q", got)
	}
}
