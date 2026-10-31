// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/expr"
)

// done reports whether ctx is canceled.
func done(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// TestStopPoolFiresAtTheStop checks the timer path of a stop: the context
// stays open until the stop passes on the clock, is canceled then, and a
// stopper that fired never comes back from the pool.
func TestStopPoolFiresAtTheStop(t *testing.T) {
	clk := clocktest.New(time.Now())
	s := newStopPool(clk)
	ctx, st := s.start(context.Background(), time.Time{}, 50*time.Millisecond)
	if st == nil || done(ctx) {
		t.Fatal("a fresh stop is already canceled")
	}
	clk.Advance(49 * time.Millisecond)
	if done(ctx) {
		t.Fatal("the stop fired before 50 ms")
	}
	clk.Advance(time.Millisecond)
	if !done(ctx) {
		t.Fatal("the stop did not fire at 50 ms")
	}
	s.end(st)
	next, st2 := s.start(context.Background(), time.Time{}, 50*time.Millisecond)
	if st2 == st || done(next) {
		t.Error("a fired stop came back from the pool")
	}
	s.end(st2)
}

// TestStopPoolReusesStops checks that a stop whose timer did not fire goes
// back to the pool uncanceled, with its timer stopped, so the next
// evaluation starts from an open context.
func TestStopPoolReusesStops(t *testing.T) {
	clk := clocktest.New(time.Now())
	s := newStopPool(clk)
	for range 3 {
		ctx, st := s.start(context.Background(), clk.Now().Add(time.Hour), 50*time.Millisecond)
		if done(ctx) {
			t.Fatal("a reused stop is canceled")
		}
		s.end(st)
		if clk.Pending() != 0 {
			t.Fatalf("%d timers armed after end, want 0", clk.Pending())
		}
		clk.Advance(time.Hour)
		if done(ctx) {
			t.Fatal("a released stop fired")
		}
	}
}

// TestStopPoolEndedRequest checks the stops that need no timer: a request
// context that has ended, a request deadline that has passed or is now,
// and a stop of zero give the canceled context at once, and a request
// deadline before the stop arms the timer for the deadline.
func TestStopPoolEndedRequest(t *testing.T) {
	now := time.Now()
	clk := clocktest.New(now)
	s := newStopPool(clk)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		ctx      context.Context
		deadline time.Time
		d        time.Duration
	}{
		"canceled request":  {canceled, time.Time{}, time.Hour},
		"passed deadline":   {context.Background(), now.Add(-time.Second), time.Hour},
		"deadline now":      {context.Background(), now, time.Hour},
		"stop of zero":      {context.Background(), time.Time{}, 0},
		"negative stop":     {context.Background(), time.Time{}, -time.Second},
		"canceled and zero": {canceled, time.Time{}, 0},
	} {
		ctx, st := s.start(tc.ctx, tc.deadline, tc.d)
		if st != nil || !done(ctx) || clk.Pending() != 0 {
			t.Errorf("%s: stopper %v, canceled %v, %d timers; want the ended context", name, st, done(ctx), clk.Pending())
		}
		s.end(st)
	}
	ctx, st := s.start(context.Background(), now.Add(time.Hour), 2*time.Hour)
	clk.Advance(time.Hour - time.Millisecond)
	if done(ctx) {
		t.Fatal("the stop fired before the request deadline")
	}
	clk.Advance(time.Millisecond)
	if !done(ctx) {
		t.Error("the stop did not fire at the request deadline")
	}
	s.end(st)
}

// TestStopPoolIgnoresContextDeadline checks that the request deadline
// comes from its argument alone: a context's deadline, a wall-clock
// instant, never moves the stop, which runs on the pool's clock, as every
// Node deadline does.
func TestStopPoolIgnoresContextDeadline(t *testing.T) {
	clk := clocktest.New(time.Now())
	s := newStopPool(clk)
	ctx, cancel := context.WithDeadline(context.Background(), clk.Now().Add(time.Hour))
	defer cancel()
	stop, st := s.start(ctx, time.Time{}, 2*time.Hour)
	clk.Advance(time.Hour)
	if done(stop) {
		t.Fatal("the stop fired at the context's deadline")
	}
	clk.Advance(time.Hour)
	if !done(stop) {
		t.Error("the stop did not fire at the stop")
	}
	s.end(st)
}

// TestStopPoolAllocatesNothing checks that an evaluation that finishes in
// time builds no context, timer or closure: a stop is taken from the pool
// and returned.
func TestStopPoolAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under -race")
	}
	clk := clocktest.New(time.Now())
	s := newStopPool(clk)
	ctx, deadline := context.Background(), clk.Now().Add(time.Hour)
	_, st := s.start(ctx, deadline, comprehensionStop)
	s.end(st)
	if n := testing.AllocsPerRun(200, func() {
		_, st := s.start(ctx, deadline, comprehensionStop)
		s.end(st)
	}); n != 0 {
		t.Errorf("start and end allocate %v per evaluation, want 0", n)
	}
}

// TestComprehensionStopFires checks the stop end to end on the wall clock:
// a tracked comprehension over 10,000 tokens, which takes far longer than
// the stop, ends with KindDeadline at a 5 ms stop and at a request
// deadline 5 ms ahead, and the next evaluation of the program finishes
// normally.
func TestComprehensionStopFires(t *testing.T) {
	c := newCompiler(t)
	c.stop = 5 * time.Millisecond
	prg := mustProgram(t, c, at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read")`)
	var ee *expr.EvalError
	if _, err := prg.EvalBool(context.Background(), longScope(10_000)); !errors.As(err, &ee) || ee.Kind != expr.KindDeadline {
		t.Errorf("EvalBool past a 5 ms stop = %v, want KindDeadline", err)
	}
	c.stop = time.Hour
	slow := mustProgram(t, c, at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s != "x" && s == "orders:read")`)
	v := longScope(10_000)
	v.Deadline = time.Now().Add(5 * time.Millisecond)
	if _, err := slow.EvalBool(context.Background(), v); !errors.As(err, &ee) || ee.Kind != expr.KindDeadline {
		t.Errorf("EvalBool past a 5 ms request deadline = %v, want KindDeadline", err)
	}
	if got, err := prg.EvalBool(context.Background(), testVars()); err != nil || !got {
		t.Errorf("EvalBool after a stop = %v, %v; want true", got, err)
	}
}

// TestNewWithClockTimesTheStop checks that a Compiler built on a clock
// times its comprehension stops there, as ruralzd's does on its process
// clock: a tracked comprehension over 20,000 tokens, which runs far past
// 50 ms of real time, ends normally while that clock stands still, and
// ends with KindDeadline once the clock passes the 50 ms stop.
func TestNewWithClockTimesTheStop(t *testing.T) {
	clk := clocktest.New(time.Now())
	c, err := NewWithClock(clk)
	if err != nil {
		t.Fatal(err)
	}
	prg := mustProgram(t, c, at(expr.PlaceAuthzCELRule), ordersRule)
	if !prg.stoppable {
		t.Fatalf("%q is not stoppable with every input unbounded", ordersRule)
	}
	start := time.Now()
	if got, err := prg.EvalBool(context.Background(), longScope(20_000)); err != nil || got {
		t.Errorf("EvalBool with the clock standing still = %v, %v after %v of real time; want false", got, err, time.Since(start))
	}
	errs := make(chan error, 1)
	go func() {
		_, err := prg.EvalBool(context.Background(), longScope(20_000))
		errs <- err
	}()
	// The evaluation arms its stop when it starts.
	for clk.Pending() == 0 {
		select {
		case err := <-errs:
			t.Fatalf("EvalBool ended before its stop was armed: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	clk.Advance(comprehensionStop)
	var ee *expr.EvalError
	if err := <-errs; !errors.As(err, &ee) || ee.Kind != expr.KindDeadline {
		t.Errorf("EvalBool once the clock passed the stop = %v, want KindDeadline", err)
	}
}

// absorbingRule holds ordersRule under a logical OR whose other operand is
// true for testVars: cel-go ends an interrupted comprehension with an
// error value, which the OR would absorb.
const absorbingRule = ordersRule + ` || request.method == "GET"`

// TestStopIsNotAbsorbed checks that a stop is a runtime error even where a
// logical operator would absorb the interrupted comprehension's error
// value: once the clock passes the stop during the evaluation, and when the
// request had ended before it started, the evaluation ends with
// KindDeadline instead of the OR's true. A comprehension too short to reach
// an interrupt check still finishes under an ended request.
func TestStopIsNotAbsorbed(t *testing.T) {
	clk := clocktest.New(time.Now())
	c, err := NewWithClock(clk)
	if err != nil {
		t.Fatal(err)
	}
	prg := mustProgram(t, c, at(expr.PlaceAuthzCELRule), absorbingRule)
	if !prg.stoppable {
		t.Fatalf("%q is not stoppable with every input unbounded", absorbingRule)
	}
	var ee *expr.EvalError
	errs := make(chan error, 1)
	go func() {
		_, err := prg.EvalBool(context.Background(), longScope(20_000))
		errs <- err
	}()
	for clk.Pending() == 0 {
		select {
		case err := <-errs:
			t.Fatalf("EvalBool ended before its stop was armed: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	clk.Advance(comprehensionStop)
	if err := <-errs; !errors.As(err, &ee) || ee.Kind != expr.KindDeadline {
		t.Errorf("EvalBool once the clock passed the stop = %v, want KindDeadline", err)
	}

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := prg.EvalBool(ended, longScope(1000)); !errors.As(err, &ee) || ee.Kind != expr.KindDeadline {
		t.Errorf("EvalBool under an ended request = %v, %v; want KindDeadline", got, err)
	}
	if got, err := prg.EvalBool(ended, testVars()); err != nil || !got {
		t.Errorf("a short comprehension under an ended request = %v, %v; want true", got, err)
	}
	// Only the stopped evaluations failed: the program evaluates normally
	// afterwards.
	if got, err := prg.EvalBool(context.Background(), longScope(1000)); err != nil || !got {
		t.Errorf("EvalBool after a stop = %v, %v; want true", got, err)
	}
}

// TestComprehensionStopAtTheRequestDeadline checks that the activation's
// request deadline, expr.Vars.Deadline, ends a tracked comprehension
// before the 50 ms stop when it comes first: with the deadline 20 ms ahead
// on the Compiler's clock, an evaluation over 20,000 tokens, which runs
// far past 50 ms of real time, ends with KindDeadline once that clock has
// moved 20 ms.
func TestComprehensionStopAtTheRequestDeadline(t *testing.T) {
	clk := clocktest.New(time.Now())
	c, err := NewWithClock(clk)
	if err != nil {
		t.Fatal(err)
	}
	prg := mustProgram(t, c, at(expr.PlaceAuthzCELRule), ordersRule)
	const routeDeadline = 20 * time.Millisecond
	v := longScope(20_000)
	v.Deadline = clk.Now().Add(routeDeadline)
	errs := make(chan error, 1)
	go func() {
		_, err := prg.EvalBool(context.Background(), v)
		errs <- err
	}()
	for clk.Pending() == 0 {
		select {
		case err := <-errs:
			t.Fatalf("EvalBool ended before its stop was armed: %v", err)
		case <-time.After(time.Millisecond):
		}
	}
	clk.Advance(routeDeadline)
	var ee *expr.EvalError
	if err := <-errs; !errors.As(err, &ee) || ee.Kind != expr.KindDeadline {
		t.Errorf("EvalBool once the clock passed a request deadline 20 ms ahead = %v, want KindDeadline", err)
	}
}

// TestEvaluationPastTheDeadlineStopsAtOnce checks that an evaluation that
// starts at or after its request deadline, as an onLog one does after a
// request that ran to its Route deadline, stops at its first interrupt
// check with KindDeadline while the clock stands still: on the deadline
// and past it, also where a logical operator would absorb the interrupted
// comprehension's error value. A comprehension too short to reach an
// interrupt check still finishes, and a program without a stop ignores
// the deadline.
func TestEvaluationPastTheDeadlineStopsAtOnce(t *testing.T) {
	clk := clocktest.New(time.Now())
	c, err := NewWithClock(clk)
	if err != nil {
		t.Fatal(err)
	}
	orders := mustProgram(t, c, at(expr.PlaceAuthzCELRule), ordersRule)
	absorbing := mustProgram(t, c, at(expr.PlaceAuthzCELRule), absorbingRule)
	var ee *expr.EvalError
	for name, deadline := range map[string]time.Time{
		"at the deadline":   clk.Now(),
		"past the deadline": clk.Now().Add(-time.Second),
	} {
		for _, prg := range []*Program{orders, absorbing} {
			v := longScope(1000)
			v.Deadline = deadline
			if got, err := prg.EvalBool(context.Background(), v); !errors.As(err, &ee) || ee.Kind != expr.KindDeadline {
				t.Errorf("%s: %q = %v, %v; want KindDeadline", name, prg.Source(), got, err)
			}
		}
	}
	short := testVars()
	short.Deadline = clk.Now()
	if got, err := absorbing.EvalBool(context.Background(), short); err != nil || !got {
		t.Errorf("a short comprehension past its deadline = %v, %v; want true", got, err)
	}
	untracked := mustProgram(t, c, at(expr.PlaceRouteMatchWhen), `["GET", "HEAD"].exists(m, m == request.method)`)
	if untracked.stoppable {
		t.Fatalf("%q is stoppable", untracked.Source())
	}
	if got, err := untracked.EvalBool(context.Background(), short); err != nil || !got {
		t.Errorf("a program without a stop past its deadline = %v, %v; want true", got, err)
	}
}
