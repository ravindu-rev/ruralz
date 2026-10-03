// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// TestPassiveThreshold covers 05 req 18: consecutiveErrors consecutive
// failures eject; a success in between restarts the run; the ejection
// counts ruralz_upstream_ejections_total{reason="passive"}.
func TestPassiveThreshold(t *testing.T) {
	tests := []struct {
		name      string
		policy    PassivePolicy
		threshold int
	}{
		{"default five", PassivePolicy{}, DefaultConsecutiveErrors},
		{"configured three", PassivePolicy{ConsecutiveErrors: 3, EjectionTime: 10 * time.Second}, 3},
		{"configured one", PassivePolicy{ConsecutiveErrors: 1}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr, clk, m, _ := newTracker(t, 4, func(c *Config) { c.Passive = tc.policy })
			for range tc.threshold - 1 {
				if fail(tr, clk, 0, CauseOther) {
					t.Fatal("ejected below the threshold")
				}
			}
			succeed(tr, clk, 0)
			for n := 1; n <= tc.threshold; n++ {
				ejected := fail(tr, clk, 0, CauseOther)
				if ejected != (n == tc.threshold) {
					t.Fatalf("failure %d ejected=%v", n, ejected)
				}
			}
			if !tr.View(clk.Now()).Down(0) || m.passive.n.Load() != 1 {
				t.Fatalf("down %v, ejections %d", tr.View(clk.Now()).Down(0), m.passive.n.Load())
			}
			st := tr.Status(clk.Now())[0]
			want := tc.policy.WithDefaults().EjectionTime
			if !st.Ejected || st.Ejections != 1 || !st.EjectedUntil.Equal(clk.Now().Add(want)) || st.ConsecutiveErrors != 0 {
				t.Fatalf("status %+v", st)
			}
		})
	}
}

// TestEjectionTimeMultiplier covers 05 req 18: the n-th ejection lasts
// ejectionTime × n, n capped at 10, and the count decays by one per
// ejectionTime spent not ejected (05 section 9 item 10).
func TestEjectionTimeMultiplier(t *testing.T) {
	const et = 10 * time.Second
	tr, clk, _, _ := newTracker(t, 4, func(c *Config) { c.Passive = PassivePolicy{ConsecutiveErrors: 1, EjectionTime: et} })
	for n := 1; n <= 12; n++ {
		if !fail(tr, clk, 0, CauseOther) {
			t.Fatalf("ejection %d skipped", n)
		}
		want := min(n, MaxEjectionMultiplier)
		st := tr.Status(clk.Now())[0]
		if st.Ejections != want || st.EjectedUntil.Sub(clk.Now()) != time.Duration(want)*et {
			t.Fatalf("ejection %d: count %d for %v, want %d for %v", n, st.Ejections, st.EjectedUntil.Sub(clk.Now()), want, time.Duration(want)*et)
		}
		// Failures of attempts ending while ejected do not count.
		if fail(tr, clk, 0, CauseOther) {
			t.Fatal("ejected while ejected")
		}
		clk.Advance(time.Duration(want) * et)
		if tr.View(clk.Now()).Down(0) {
			t.Fatalf("still ejected after %v", time.Duration(want)*et)
		}
	}
	// Decay: 10 → 7 after three ejection times outside ejection.
	clk.Advance(3*et + et/2)
	if got := tr.Status(clk.Now())[0].Ejections; got != 7 {
		t.Fatalf("decayed count %d, want 7", got)
	}
	fail(tr, clk, 0, CauseOther)
	if st := tr.Status(clk.Now())[0]; st.Ejections != 8 || st.EjectedUntil.Sub(clk.Now()) != 8*et {
		t.Fatalf("after decay: %+v", st)
	}
	// Full decay back to a first ejection.
	clk.Advance(8*et + 20*et)
	if got := tr.Status(clk.Now())[0].Ejections; got != 0 {
		t.Fatalf("fully decayed count %d", got)
	}
	fail(tr, clk, 0, CauseOther)
	if st := tr.Status(clk.Now())[0]; st.Ejections != 1 || st.EjectedUntil.Sub(clk.Now()) != et {
		t.Fatalf("after full decay: %+v", st)
	}
	if decayed(&endpoint{ejections: 2, decayFrom: 0}, int64(time.Hour), 0) != 0 {
		t.Fatal("decayed with a zero ejection time")
	}
}

// TestEjectionCap covers 05 req 19: at most floor(50% × E) Endpoints are
// passively ejected at once; the skipped ejection keeps the failure run so
// the next failure tries again once room frees; connect errors, attempt
// timeouts on suspect Endpoints and active removals bypass the cap.
func TestEjectionCap(t *testing.T) {
	tests := []struct {
		n, limit int
	}{{1, 0}, {2, 1}, {3, 1}, {4, 2}, {5, 2}, {10, 5}}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("E=%d", tc.n), func(t *testing.T) {
			tr, clk, _, _ := newTracker(t, tc.n, func(c *Config) { c.Passive = PassivePolicy{ConsecutiveErrors: 1} })
			for i := range tc.n {
				ejected := fail(tr, clk, i, CauseOther)
				if ejected != (i < tc.limit) {
					t.Fatalf("Endpoint %d ejected=%v with limit %d", i, ejected, tc.limit)
				}
			}
			if got := ejectedCount(tr, clk.Now()); got != tc.limit {
				t.Fatalf("%d ejected, want %d", got, tc.limit)
			}
			// Connect errors bypass the cap.
			if !fail(tr, clk, tc.n-1, CauseConnect) && tc.n-1 >= tc.limit {
				t.Fatal("connect error did not bypass the cap")
			}
		})
	}

	// A skipped ejection retries on the next failure once room frees.
	tr, clk, _, _ := newTracker(t, 2, func(c *Config) { c.Passive = PassivePolicy{ConsecutiveErrors: 2, EjectionTime: 10 * time.Second} })
	fail(tr, clk, 0, CauseOther)
	if !fail(tr, clk, 0, CauseOther) {
		t.Fatal("first ejection skipped")
	}
	fail(tr, clk, 1, CauseOther)
	if fail(tr, clk, 1, CauseOther) {
		t.Fatal("cap exceeded")
	}
	if st := tr.Status(clk.Now())[1]; st.ConsecutiveErrors != 2 {
		t.Fatalf("failure run lost on a skipped ejection: %+v", st)
	}
	clk.Advance(10 * time.Second)
	if !fail(tr, clk, 1, CauseOther) {
		t.Fatal("retry after room freed was skipped")
	}
}

// TestSuspectTimeoutEjectsAtOnce covers 05 reqs 19 and 21: an attempt
// timeout on a suspect Endpoint ejects it at once, over the cap and below
// consecutiveErrors; a timeout on a healthy-looking Endpoint does not.
func TestSuspectTimeoutEjectsAtOnce(t *testing.T) {
	tr, clk, m, _ := newTracker(t, 2, nil)
	// Fill the cap (floor(50% × 2) = 1) with Endpoint 0.
	for range DefaultConsecutiveErrors {
		fail(tr, clk, 0, CauseConnect)
	}
	var a Attempt
	now := clk.Now()
	tr.View(now).Begin(&a, 1, now)
	clk.Advance(StallAfter)
	if !a.Stalled() || !tr.View(clk.Now()).Suspect(1) {
		t.Fatal("attempt not stalled and suspect after 1 s without headers")
	}
	clk.Advance(2 * time.Second)
	if !a.End(clk.Now(), Outcome{Failed: true, Cause: CauseTimeout}) {
		t.Fatal("timeout on a suspect Endpoint did not eject it")
	}
	if ejectedCount(tr, clk.Now()) != 2 || m.passive.n.Load() != 2 {
		t.Fatalf("ejected %d, counted %d", ejectedCount(tr, clk.Now()), m.passive.n.Load())
	}

	// A timeout without suspicion (headers came recently) counts as an
	// ordinary failure.
	tr2, clk2, _, _ := newTracker(t, 4, nil)
	succeed(tr2, clk2, 0)
	var b Attempt
	now = clk2.Now()
	tr2.View(now).Begin(&b, 0, now)
	clk2.Advance(500 * time.Millisecond)
	if b.End(clk2.Now(), Outcome{Failed: true, Cause: CauseTimeout}) {
		t.Fatal("timeout on a non-suspect Endpoint ejected it")
	}
}

// TestOutcomePolicy covers 05 req 2: an attempt carries its snapshot's
// thresholds; the zero policy means the Tracker's current one.
func TestOutcomePolicy(t *testing.T) {
	tr, clk, _, _ := newTracker(t, 4, func(c *Config) { c.Passive = PassivePolicy{ConsecutiveErrors: 10} })
	old := PassivePolicy{ConsecutiveErrors: 2, EjectionTime: time.Second}
	for n := 1; n <= 2; n++ {
		var a Attempt
		tr.View(clk.Now()).Begin(&a, 0, clk.Now())
		if a.End(clk.Now(), Outcome{Failed: true, Cause: CauseOther, Passive: old}) != (n == 2) {
			t.Fatalf("attempt %d under the old snapshot's thresholds", n)
		}
	}
	if st := tr.Status(clk.Now())[0]; !st.EjectedUntil.Equal(clk.Now().Add(time.Second)) {
		t.Fatalf("ejection time %v", st.EjectedUntil.Sub(clk.Now()))
	}
	tr.SetPolicy(PassivePolicy{ConsecutiveErrors: 1}, nil, 0)
	if !fail(tr, clk, 1, CauseOther) || tr.Passive().ConsecutiveErrors != 1 {
		t.Fatal("new thresholds not applied")
	}
}

// TestEjectionBoundConcurrent is the "ejection bound never exceeded"
// property (WP-23 done-when): attempts from many goroutines with random
// outcomes, while the clock moves, never leave more than floor(50% × E)
// Endpoints passively ejected when no failure may bypass the cap.
func TestEjectionBoundConcurrent(t *testing.T) {
	const n = 9
	tr, clk, _, _ := newTracker(t, n, func(c *Config) { c.Passive = PassivePolicy{ConsecutiveErrors: 2, EjectionTime: 50 * time.Millisecond} })
	limit := n * MaxEjectionPercent / 100
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var mu sync.Mutex
	violations := 0
	for g := range 8 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(g), 99)) //nolint:gosec // G404: deterministic test outcomes, not secrets.
			var a Attempt
			for {
				select {
				case <-stop:
					return
				default:
				}
				now := clk.Now()
				v := tr.View(now)
				i := r.IntN(n)
				v.Begin(&a, i, now)
				a.End(now, Outcome{Failed: r.IntN(4) != 0, Cause: CauseOther})
				if got := ejectedNow(tr, clk); got > limit {
					mu.Lock()
					violations++
					mu.Unlock()
				}
			}
		})
	}
	for range 400 {
		clk.Advance(5 * time.Millisecond)
		time.Sleep(50 * time.Microsecond)
	}
	close(stop)
	wg.Wait()
	if violations > 0 {
		t.Fatalf("%d observations over the cap of %d", violations, limit)
	}
}

// ejectedNow counts passively ejected Endpoints at a time no earlier than
// any ejection so far: the clock is read under the Tracker's lock, which
// every ejection holds after reading its own time.
func ejectedNow(tr *Tracker, clk *clocktest.Fake) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	now := tr.at(clk.Now())
	n := 0
	for _, e := range tr.tab.Load().eps {
		if e.ejectedUntil.Load() > now {
			n++
		}
	}
	return n
}

// FuzzEjectionCap drives one Tracker with arbitrary attempt sequences and
// clock steps: without bypassing causes the passively ejected count never
// exceeds floor(50% × E) (05 req 19), and the healthy gauge always equals
// the Endpoints eligible after step (b) (05 req 23).
func FuzzEjectionCap(f *testing.F) {
	f.Add(uint8(5), []byte{0, 1, 0, 1, 2, 3, 4, 0xff, 0, 1})
	f.Add(uint8(1), []byte{0, 0, 0, 0, 0, 0})
	f.Add(uint8(12), []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 0xfe, 0xfd})
	f.Fuzz(func(t *testing.T, size uint8, ops []byte) {
		n := int(size%16) + 1
		clk := clocktest.New(epoch())
		m := newMetrics()
		tr := NewTracker(Config{Upstream: "u", Clock: clk, Metrics: m.m, Passive: PassivePolicy{ConsecutiveErrors: 2, EjectionTime: time.Second}})
		if err := tr.SetEndpoints(ids(n)); err != nil {
			t.Fatal(err)
		}
		limit := n * MaxEjectionPercent / 100
		for _, op := range ops {
			switch {
			case op >= 0xf0:
				clk.Advance(time.Duration(op-0xef) * 300 * time.Millisecond)
			case op&0x80 != 0:
				succeed(tr, clk, int(op)%n)
			default:
				fail(tr, clk, int(op)%n, []Cause{CauseOther, CauseNone, CauseTimeout}[int(op>>4)%3])
			}
			now := clk.Now()
			if got := ejectedCount(tr, now); got > limit {
				t.Fatalf("%d ejected over the cap %d", got, limit)
			}
			h, total := tr.Healthy(now)
			want := h
			if want == 0 {
				want = total
			}
			if got := m.healthyG.v.Load(); got != int64(want) {
				t.Fatalf("healthy gauge %d, want %d", got, want)
			}
		}
	})
}

// TestClosedAndRemoved: a closed Tracker and an Endpoint that left the set
// eject nothing.
func TestClosedAndRemoved(t *testing.T) {
	tr, clk, _, _ := newTracker(t, 4, func(c *Config) { c.Passive = PassivePolicy{ConsecutiveErrors: 1} })
	var a Attempt
	tr.View(clk.Now()).Begin(&a, 3, clk.Now())
	if err := tr.SetEndpoints(ids(3)); err != nil {
		t.Fatal(err)
	}
	if a.End(clk.Now(), Outcome{Failed: true}) {
		t.Fatal("ejected an Endpoint that left the set")
	}
	tr.Close()
	if fail(tr, clk, 0, CauseConnect) {
		t.Fatal("closed Tracker ejected")
	}
}
