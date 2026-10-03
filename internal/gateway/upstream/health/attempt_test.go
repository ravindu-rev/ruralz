// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// TestStallAndSuspect covers 05 req 21: an attempt without headers 1 s
// after it started is stalled; its Endpoint is suspect while it has
// delivered no headers for 1 s; any attempt getting headers clears the
// suspicion; selection avoids suspect Endpoints (View.Avoid).
func TestStallAndSuspect(t *testing.T) {
	tr, clk, _, _ := newTracker(t, 3, nil)
	var a Attempt
	start := clk.Now()
	if !tr.View(start).Begin(&a, 1, start) {
		t.Fatal("Begin")
	}
	clk.Advance(StallAfter - time.Millisecond)
	if a.Stalled() || tr.View(clk.Now()).Avoid(1) {
		t.Fatal("stalled before 1 s")
	}
	clk.Advance(time.Millisecond)
	v := tr.View(clk.Now())
	if !a.Stalled() || !v.Suspect(1) || !v.Avoid(1) || v.Avoid(0) || v.Down(1) {
		t.Fatalf("at 1 s: stalled %v suspect %v avoid %v", a.Stalled(), v.Suspect(1), v.Avoid(1))
	}
	if st := tr.Status(clk.Now())[1]; !st.Suspect || st.Stalled != 1 {
		t.Fatalf("status %+v", st)
	}

	// Another attempt to the same Endpoint gets headers: not suspect for
	// 1 s, though the first attempt is still stalled.
	var b Attempt
	tr.View(clk.Now()).Begin(&b, 1, clk.Now())
	clk.Advance(100 * time.Millisecond)
	b.Headers(clk.Now())
	b.End(clk.Now(), Outcome{})
	if tr.View(clk.Now()).Suspect(1) {
		t.Fatal("suspect right after headers")
	}
	clk.Advance(SuspectAfter)
	if !tr.View(clk.Now()).Suspect(1) {
		t.Fatal("not suspect again 1 s after the last headers with a stalled attempt")
	}
	// The stalled attempt gets its headers: stall released, not suspect.
	a.Headers(clk.Now())
	if a.Stalled() || tr.View(clk.Now()).Suspect(1) || tr.stalled.Load() != 0 {
		t.Fatal("headers did not release the stall")
	}
	a.End(clk.Now(), Outcome{})
	// Headers are monotonic: an older timestamp does not move them back.
	a2 := Attempt{}
	tr.View(clk.Now()).Begin(&a2, 1, clk.Now())
	a2.Headers(clk.Now().Add(-time.Hour))
	if tr.tab.Load().eps[1].lastHeaders.Load() != tr.at(clk.Now()) {
		t.Fatal("lastHeaders moved backwards")
	}
	a2.End(clk.Now(), Outcome{})
}

// TestStalledShareRule covers 05 req 21: once stalled attempts hold 50% of
// maxConnections, every Endpoint holding one is avoided, suspect or not.
func TestStalledShareRule(t *testing.T) {
	tr, clk, _, _ := newTracker(t, 3, func(c *Config) { c.MaxConnections = 4 })
	attempts := make([]Attempt, 2)
	for i := range attempts {
		tr.View(clk.Now()).Begin(&attempts[i], i, clk.Now())
	}
	// Endpoint 0 and 1 hold stalled attempts; both got headers recently
	// through other attempts, so neither is suspect.
	clk.Advance(StallAfter)
	for i := range 2 {
		var h Attempt
		tr.View(clk.Now()).Begin(&h, i, clk.Now())
		h.Headers(clk.Now())
		h.End(clk.Now(), Outcome{})
	}
	v := tr.View(clk.Now())
	if v.Suspect(0) || v.Suspect(1) {
		t.Fatal("suspect despite recent headers")
	}
	if !v.Avoid(0) || !v.Avoid(1) || v.Avoid(2) {
		t.Fatalf("2 of 4 stalled: avoid %v %v %v", v.Avoid(0), v.Avoid(1), v.Avoid(2))
	}
	attempts[0].Cancel()
	v = tr.View(clk.Now())
	if v.Avoid(1) {
		t.Fatal("1 of 4 stalled still avoided")
	}
	attempts[1].End(clk.Now(), Outcome{})
	if tr.stalled.Load() != 0 {
		t.Fatalf("stalled count %d", tr.stalled.Load())
	}
}

// TestAttemptReuse: a pooled Attempt serves many attempts; each generation
// stalls on its own timer, Begin finishes an unfinished predecessor, and
// End or Cancel twice has no second effect.
func TestAttemptReuse(t *testing.T) {
	tr, clk, _, _ := newTracker(t, 2, func(c *Config) { c.Passive = PassivePolicy{ConsecutiveErrors: 2} })
	var a Attempt
	for range 5 {
		tr.View(clk.Now()).Begin(&a, 0, clk.Now())
		clk.Advance(StallAfter / 2)
		a.Headers(clk.Now())
		clk.Advance(StallAfter)
		if a.Stalled() {
			t.Fatal("attempt with headers stalled")
		}
		a.End(clk.Now(), Outcome{})
	}
	// Begin while the previous attempt is stalled releases it.
	tr.View(clk.Now()).Begin(&a, 0, clk.Now())
	clk.Advance(StallAfter)
	if !a.Stalled() || tr.stalled.Load() != 1 {
		t.Fatal("not stalled")
	}
	tr.View(clk.Now()).Begin(&a, 1, clk.Now())
	if tr.stalled.Load() != 0 || a.Stalled() {
		t.Fatal("previous stalled attempt not released")
	}
	clk.Advance(StallAfter)
	if !a.Stalled() || tr.tab.Load().eps[1].stalled.Load() != 1 {
		t.Fatal("new generation did not stall on Endpoint 1")
	}
	if a.End(clk.Now(), Outcome{Failed: true}) {
		t.Fatal("ejected below the threshold")
	}
	// A second End of the same attempt has no effect.
	if again := a.End(clk.Now(), Outcome{Failed: true}); again {
		t.Fatal("second End ejected")
	}
	if st := tr.Status(clk.Now())[1]; st.ConsecutiveErrors != 1 {
		t.Fatalf("second End counted: %+v", st)
	}
	a.Cancel()
	a.Headers(clk.Now())
	if clk.Pending() != 0 {
		t.Fatalf("%d timers armed after End", clk.Pending())
	}
}

// TestLateStallCallback: a stall firing armed for an earlier generation
// that runs after the Attempt was reused does not stall the new attempt
// early (05 req 21 on the real clock's asynchronous timers).
func TestLateStallCallback(t *testing.T) {
	tr, clk, _, _ := newTracker(t, 1, nil)
	var a Attempt
	tr.View(clk.Now()).Begin(&a, 0, clk.Now())
	a.End(clk.Now(), Outcome{})
	clk.Advance(10 * time.Second)
	tr.View(clk.Now()).Begin(&a, 0, clk.Now())
	clk.Advance(StallAfter / 2)
	a.onStall() // the previous generation's callback, delivered late
	if a.Stalled() {
		t.Fatal("late callback stalled a young attempt")
	}
	a.state.Store(a.state.Load()&^phaseMask | phaseSettled)
	a.onStall() // settled attempts never stall
	if a.Stalled() {
		t.Fatal("settled attempt stalled")
	}
	var fresh Attempt
	fresh.onStall() // never begun
	if fresh.Stalled() {
		t.Fatal("idle attempt stalled")
	}
}

// TestAttemptsRealClock runs attempts concurrently on the real clock under
// the race detector: stall timers fire asynchronously while Attempts are
// reused.
func TestAttemptsRealClock(t *testing.T) {
	tr := NewTracker(Config{Upstream: "u", Clock: clock.Real()})
	if err := tr.SetEndpoints(ids(4)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			var a Attempt
			for i := range 200 {
				now := time.Now()
				tr.View(now).Begin(&a, (g+i)%4, now)
				if i%3 == 0 {
					a.Headers(time.Now())
				}
				a.End(time.Now(), Outcome{Failed: i%5 == 0})
			}
		})
	}
	wg.Wait()
	if tr.stalled.Load() != 0 {
		t.Fatalf("stalled count %d after every attempt ended", tr.stalled.Load())
	}
	tr.Close()
}

// TestAttemptAllocations: tracking a pooled attempt allocates nothing
// (spec 05 test 22: onUpstreamResponseHeaders accounting 0 allocations).
func TestAttemptAllocations(t *testing.T) {
	tr, clk, _, _ := newTracker(t, 4, nil)
	var a Attempt
	now := clk.Now()
	tr.View(now).Begin(&a, 0, now)
	a.End(now, Outcome{})
	allocs := testing.AllocsPerRun(1000, func() {
		v := tr.View(now)
		_ = v.Down(1) || v.Avoid(1)
		_ = v.Failures(1)
		v.Begin(&a, 1, now)
		a.Headers(now)
		a.End(now, Outcome{Failed: true, Cause: CauseOther})
		v.Begin(&a, 1, now)
		a.End(now, Outcome{})
	})
	if allocs != 0 {
		t.Fatalf("%v allocations per attempt", allocs)
	}
}
