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
	ctx, st := s.start(context.Background(), 50*time.Millisecond)
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
	next, st2 := s.start(context.Background(), 50*time.Millisecond)
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
		ctx, st := s.start(context.Background(), 50*time.Millisecond)
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
// context that has ended, a deadline that has passed and a stop of zero
// give the canceled context at once, and a deadline before the stop arms
// the timer for the deadline.
func TestStopPoolEndedRequest(t *testing.T) {
	now := time.Now()
	clk := clocktest.New(now)
	s := newStopPool(clk)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), now.Add(-time.Second))
	defer cancelExpired()
	for name, tc := range map[string]struct {
		ctx context.Context
		d   time.Duration
	}{
		"canceled request":  {canceled, time.Hour},
		"expired deadline":  {expired, time.Hour},
		"stop of zero":      {context.Background(), 0},
		"negative stop":     {context.Background(), -time.Second},
		"canceled and zero": {canceled, 0},
	} {
		ctx, st := s.start(tc.ctx, tc.d)
		if st != nil || !done(ctx) || clk.Pending() != 0 {
			t.Errorf("%s: stopper %v, canceled %v, %d timers; want the ended context", name, st, done(ctx), clk.Pending())
		}
		s.end(st)
	}
	soon, cancelSoon := context.WithDeadline(context.Background(), now.Add(time.Hour))
	defer cancelSoon()
	ctx, st := s.start(soon, 2*time.Hour)
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

// TestStopPoolAllocatesNothing checks that an evaluation that finishes in
// time builds no context, timer or closure: a stop is taken from the pool
// and returned.
func TestStopPoolAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under -race")
	}
	s := newStopPool(clocktest.New(time.Now()))
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	_, st := s.start(ctx, comprehensionStop)
	s.end(st)
	if n := testing.AllocsPerRun(200, func() {
		_, st := s.start(ctx, comprehensionStop)
		s.end(st)
	}); n != 0 {
		t.Errorf("start and end allocate %v per evaluation, want 0", n)
	}
}

// TestComprehensionStopFires checks the stop end to end on the wall clock:
// a tracked comprehension over 10,000 tokens, which takes far longer than
// the stop, ends with KindDeadline at a 5 ms stop and at a 5 ms request
// deadline, and the next evaluation of the program finishes normally.
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := slow.EvalBool(ctx, longScope(10_000)); !errors.As(err, &ee) || ee.Kind != expr.KindDeadline {
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
