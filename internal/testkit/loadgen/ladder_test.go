// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// PBB's ladder: 13 open-loop steps of 5% from 50% to 110% of the expected
// saturation, 60 s each after a 60 s warm-up (target).
func TestBenchmarkLadderSteps(t *testing.T) {
	l := BenchmarkLadder(40000)
	rates, err := l.Rates()
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 13 {
		t.Fatalf("%d steps, want 13", len(rates))
	}
	for k, r := range rates {
		if want := 40000 * (0.50 + 0.05*float64(k)); math.Abs(r-want) > 1e-6 {
			t.Errorf("step %d = %v, want %v", k, r, want)
		}
	}
	if l.StepDuration != time.Minute || l.Warmup != time.Minute {
		t.Errorf("step %v warm-up %v, want 1m each", l.StepDuration, l.Warmup)
	}
	for _, bad := range []Ladder{
		{Expected: 0, From: 0.5, To: 1, Step: 0.1},
		{Expected: 1, From: 0, To: 1, Step: 0.1},
		{Expected: 1, From: 0.5, To: 0.4, Step: 0.1},
		{Expected: 1, From: 0.5, To: 1, Step: 0},
		{Expected: math.Inf(1), From: 0.5, To: 1, Step: 0.1},
	} {
		if _, err := bad.Rates(); err == nil {
			t.Errorf("Rates(%+v) succeeded", bad)
		}
	}
}

// Saturation is the highest step within 0.5% of its offered rate, with
// p99 of 10 ms or less and errors at 0.01% or less (target).
func TestSaturationRule(t *testing.T) {
	c := BenchmarkSaturation()
	if c.MaxRateError != 0.005 || c.MaxP99 != 10*time.Millisecond || c.MaxErrorRate != 0.0001 {
		t.Fatalf("BenchmarkSaturation = %+v", c)
	}
	steps := []StepSummary{
		{Offered: 20000, RateError: 0, P99: ms(1)},
		{Offered: 30000, RateError: 0.004, P99: ms(9), ErrorRate: 0.0001},
		{Offered: 35000, RateError: 0.006, P99: ms(5)},              // falls behind
		{Offered: 32000, RateError: 0, P99: ms(11)},                 // too slow
		{Offered: 33000, RateError: 0, P99: ms(2), ErrorRate: 0.01}, // errors
	}
	if rate, ok := Saturation(steps, c); !ok || rate != 30000 {
		t.Errorf("Saturation = %v, %v; want 30000", rate, ok)
	}
	if _, ok := Saturation(steps[2:], c); ok {
		t.Error("Saturation found a step though none qualifies")
	}
}

// RunLadder runs each step in turn, a warm-up before the first only, and
// stops climbing when Stop says so.
func TestRunLadderStopsWhenAsked(t *testing.T) {
	fk := clocktest.New(epoch())
	var stepsSeen atomic.Int32
	l := Ladder{
		Expected: 100, From: 0.5, To: 1.0, Step: 0.25, StepDuration: time.Second, Warmup: time.Second,
		Stop: func(*Result) bool { return stepsSeen.Add(1) == 2 },
	}
	done := make(chan struct{})
	defer close(done)
	go func() { // a clock that keeps moving, so each step's schedule runs out
		for {
			select {
			case <-done:
				return
			default:
				fk.Advance(ms(10))
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()
	steps, err := RunLadder(t.Context(), ok(), Config{Clock: fk, Duration: time.Hour, Settle: &Settle{}}, l)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("%d steps ran, want 2 (Stop after the second)", len(steps))
	}
	if steps[0].Rate != 50 || steps[1].Rate != 75 {
		t.Errorf("step rates %v, %v; want 50, 75", steps[0].Rate, steps[1].Rate)
	}
	if steps[0].Warmup.Offered != 50 || steps[1].Warmup.Offered != 0 {
		t.Errorf("warm-up requests %d, %d; want 50 before the first step only", steps[0].Warmup.Offered, steps[1].Warmup.Offered)
	}
	for i, s := range steps {
		if s.Measured.Offered != int64(s.Rate) || s.MeasuredDuration() != time.Second {
			t.Errorf("step %d offered %d over %v, want %v over 1s", i, s.Measured.Offered, s.MeasuredDuration(), s.Rate)
		}
		if sum := s.Summary(); sum.Offered != s.Rate || sum.RateError != 0 || sum.ErrorRate != 0 {
			t.Errorf("step %d summary %+v", i, sum)
		}
	}
	if _, err := RunLadder(t.Context(), ok(), Config{Clock: fk}, Ladder{Expected: 10, From: 1, To: 1, Step: 1}); err == nil {
		t.Error("RunLadder without a step duration succeeded")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := RunLadder(ctx, ok(), Config{Clock: fk}, Ladder{Expected: 10, From: 1, To: 1, Step: 1, StepDuration: time.Second}); err == nil {
		t.Error("RunLadder on an ended context succeeded")
	}
}
