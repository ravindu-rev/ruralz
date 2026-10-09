// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// stepClock models clock.Real across a wall-clock step (an NTP step at
// boot, a VM resume, a manual date change): Now, the wall reading, jumps
// by step, while Since and the timers follow elapsed time, as clock.Real
// derives them from the monotonic reading. The readings the test passes as
// the Upstream layer's now come from the embedded Fake, unstepped: they
// stand for clock.Real readings, whose monotonic part makes differences
// elapsed time. Since is exact for readings taken before any step, which
// is all the code under test passes it (its base).
type stepClock struct {
	*clocktest.Fake
	step atomic.Int64
}

func (c *stepClock) Now() time.Time { return c.Fake.Now().Add(time.Duration(c.step.Load())) }

func (c *stepClock) Since(t time.Time) time.Duration { return c.Fake.Now().Sub(t) }

// wallStep is one wall-clock step case.
type wallStep struct {
	name string
	step time.Duration
}

// wallSteps returns the steps the tests apply: back and forward.
func wallSteps() []wallStep {
	return []wallStep{
		{"wall clock back 1 h", -time.Hour},
		{"wall clock forward 1 h", time.Hour},
	}
}

// TestTrackerWallClockStep covers 05 reqs 18 and 21 across a wall-clock
// step: an ejection ends after ejectionTime × its count of elapsed time,
// neither later (a backward step) nor earlier (a forward one), and an
// attempt without headers stalls 1 s after it started.
func TestTrackerWallClockStep(t *testing.T) {
	const et = 30 * time.Second
	for _, tc := range wallSteps() {
		t.Run(tc.name, func(t *testing.T) {
			clk := &stepClock{Fake: clocktest.New(epoch())}
			m := newMetrics()
			log := &healthyLog{}
			tr := NewTracker(Config{
				Upstream: "orders", Clock: clk, Metrics: m.m, OnHealthy: log.record,
				Passive: PassivePolicy{ConsecutiveErrors: 1, EjectionTime: et},
			})
			if err := tr.SetEndpoints(ids(4)); err != nil {
				t.Fatal(err)
			}
			for n := 1; n <= 2; n++ {
				if !fail(tr, clk.Fake, 0, CauseConnect) {
					t.Fatalf("ejection %d skipped", n)
				}
				clk.step.Add(int64(tc.step))
				// A recount on the Tracker's own reading (an activation)
				// neither ends nor extends the ejection.
				tr.SetMetrics(m.m)
				if got := m.healthyG.v.Load(); got != 3 {
					t.Fatalf("ejection %d right after the step: healthy gauge %d, want 3", n, got)
				}
				clk.Advance(time.Duration(n)*et - time.Millisecond)
				if got := m.healthyG.v.Load(); got != 3 || !tr.View(clk.Fake.Now()).Down(0) {
					t.Fatalf("ejection %d ended early: healthy gauge %d", n, got)
				}
				at := clk.Fake.Now()
				if st := tr.Status(at); !st[0].Ejected || !st[0].EjectedUntil.Equal(at.Add(time.Millisecond)) {
					t.Fatalf("ejection %d: Status ejectedUntil %v at %v, want 1ms later", n, st[0].EjectedUntil, at)
				}
				clk.Advance(time.Millisecond)
				if got := m.healthyG.v.Load(); got != 4 || log.last() != [2]int{4, 4} || clk.Pending() != 0 {
					t.Fatalf("ejection %d did not end after %v: gauge %d, OnHealthy %v, %d timers",
						n, time.Duration(n)*et, got, log.last(), clk.Pending())
				}
				if h, _ := tr.Healthy(clk.Fake.Now()); h != 4 {
					t.Fatalf("Healthy %d after ejection %d ended", h, n)
				}
			}
			var a Attempt
			now := clk.Fake.Now()
			tr.View(now).Begin(&a, 1, now)
			clk.step.Add(int64(tc.step))
			clk.Advance(StallAfter)
			if !a.Stalled() || !tr.View(clk.Fake.Now()).Suspect(1) {
				t.Fatal("attempt without headers did not stall 1 s after it started")
			}
			a.Cancel()
			tr.Close()
		})
	}
}

// TestProberWallClockStep covers 05 req 20 across a wall-clock step:
// probes stay interval ±10% apart, neither halting for the step (a
// backward one) nor bursting (a forward one), and their verdicts land.
func TestProberWallClockStep(t *testing.T) {
	for _, tc := range wallSteps() {
		t.Run(tc.name, func(t *testing.T) {
			fake := clocktest.New(epoch())
			clk := &stepClock{Fake: fake}
			tr := NewTracker(Config{Upstream: "orders", Clock: clk, Active: &ActivePolicy{
				Timeout: 10 * time.Second, HealthyThreshold: 1, UnhealthyThreshold: 1,
			}})
			if err := tr.SetEndpoints([]string{"a"}); err != nil {
				t.Fatal(err)
			}
			h := startProber(t, fake, ProberConfig{Clock: clk, Rand: fixedSource(0)})
			s := newScript(fake)
			h.p.Attach(tr, s.probe)
			h.settle(t)
			h.advance(t, 20*time.Second, 500*time.Millisecond, true)
			clk.step.Store(int64(tc.step))
			s.set("a", 503)
			h.settle(t)
			h.advance(t, 30*time.Second, 500*time.Millisecond, true)
			// fixedSource(0): the first probe at once, then every 9 s.
			want := []time.Duration{0, 9 * time.Second, 18 * time.Second, 27 * time.Second, 36 * time.Second, 45 * time.Second}
			if got := s.callsFor("a"); !slices.Equal(got, want) {
				t.Fatalf("probed at %v, want %v", got, want)
			}
			if !tr.View(fake.Now()).Down(0) {
				t.Fatal("probe failure after the step not recorded")
			}
			if _, skipped := h.p.Stats(); skipped != 0 || h.p.Degraded() {
				t.Fatalf("%d probes skipped", skipped)
			}
		})
	}
}
