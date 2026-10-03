// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"errors"
	"testing"
	"time"
)

// TestSetEndpointsKeepsState covers 05 req 2: per-Endpoint state is kept
// for identities that remain; new Endpoints start healthy and unejected
// (05 req 20); the list must be sorted and unique.
func TestSetEndpointsKeepsState(t *testing.T) {
	tr, clk, _, log := newTracker(t, 3, func(c *Config) {
		c.Passive = PassivePolicy{ConsecutiveErrors: 1}
		c.Active = &ActivePolicy{UnhealthyThreshold: 1}
	})
	if got := log.last(); got != [2]int{3, 3} {
		t.Fatalf("OnHealthy after SetEndpoints = %v", got)
	}
	fail(tr, clk, 1, CauseConnect)             // ep-001 ejected
	tr.RecordProbe("ep-002", false, clk.Now()) // ep-002 unhealthy
	if err := tr.SetEndpoints([]string{"ep-000", "ep-001", "ep-002", "ep-003"}); err != nil {
		t.Fatal(err)
	}
	v := tr.View(clk.Now())
	if v.Len() != 4 || v.Down(0) || !v.Down(1) || !v.Down(2) || v.Down(3) {
		t.Fatalf("state not carried by identity: %v %v %v %v", v.Down(0), v.Down(1), v.Down(2), v.Down(3))
	}
	if err := tr.SetEndpoints([]string{"ep-002", "ep-004"}); err != nil {
		t.Fatal(err)
	}
	v = tr.View(clk.Now())
	if v.Identity(0) != "ep-002" || !v.Down(0) || v.Down(1) || v.Identity(1) != "ep-004" {
		t.Fatal("state after removal")
	}
	// A re-added identity starts fresh.
	if err := tr.SetEndpoints([]string{"ep-001", "ep-002", "ep-004"}); err != nil {
		t.Fatal(err)
	}
	if tr.View(clk.Now()).Down(0) {
		t.Fatal("re-added Endpoint kept its old ejection")
	}
	for _, bad := range [][]string{{"b", "a"}, {"a", "a"}} {
		if err := tr.SetEndpoints(bad); !errors.Is(err, ErrNotNormalized) {
			t.Fatalf("SetEndpoints(%q) = %v", bad, err)
		}
	}
	if i, ok := tr.Index("ep-002"); !ok || i != 1 {
		t.Fatalf("Index = %d, %v", i, ok)
	}
	if _, ok := tr.Index("ep-003"); ok {
		t.Fatal("Index of a missing identity")
	}
	if tr.Len() != 3 || tr.Upstream() != "orders" {
		t.Fatal("Len or Upstream")
	}
}

// TestHealthyGauge covers 05 req 23: ruralz_upstream_healthy_endpoints is
// the Endpoints eligible after step (b) of 05 req 11, which in panic mode
// is every Endpoint; OnHealthy reports the raw healthy count (0 in panic)
// so the Upstream layer can raise upstream_panic; ejection expiry updates
// both without a request.
func TestHealthyGauge(t *testing.T) {
	tr, clk, m, log := newTracker(t, 2, func(c *Config) { c.Passive = PassivePolicy{ConsecutiveErrors: 1, EjectionTime: 10 * time.Second} })
	if m.healthyG.v.Load() != 2 {
		t.Fatalf("gauge %d", m.healthyG.v.Load())
	}
	fail(tr, clk, 0, CauseOther)
	if m.healthyG.v.Load() != 1 || log.last() != [2]int{1, 2} {
		t.Fatalf("after one ejection: gauge %d, OnHealthy %v", m.healthyG.v.Load(), log.last())
	}
	clk.Advance(time.Second)
	fail(tr, clk, 1, CauseConnect)
	if h, n := tr.Healthy(clk.Now()); h != 0 || n != 2 {
		t.Fatalf("Healthy = %d/%d", h, n)
	}
	if m.healthyG.v.Load() != 2 || log.last() != [2]int{0, 2} {
		t.Fatalf("panic mode: gauge %d, OnHealthy %v", m.healthyG.v.Load(), log.last())
	}
	// The first ejection ends at 10 s, the second at 11 s.
	clk.Advance(9 * time.Second)
	if log.last() != [2]int{1, 2} || m.healthyG.v.Load() != 1 {
		t.Fatalf("after the first expiry: OnHealthy %v gauge %d", log.last(), m.healthyG.v.Load())
	}
	clk.Advance(time.Second)
	if log.last() != [2]int{2, 2} || m.healthyG.v.Load() != 2 {
		t.Fatalf("after the second expiry: OnHealthy %v gauge %d", log.last(), m.healthyG.v.Load())
	}
	if clk.Pending() != 0 {
		t.Fatalf("%d timers left armed", clk.Pending())
	}
	// New handles get the current value.
	m2 := newMetrics()
	tr.SetMetrics(m2.m)
	if m2.healthyG.v.Load() != 2 {
		t.Fatal("SetMetrics did not set the gauge")
	}
	tr.SetMetrics(nil)
	fail(tr, clk, 0, CauseConnect) // no handles: nothing to count, no panic
	tr.Close()
	if clk.Pending() != 0 {
		t.Fatal("Close left the expiry timer armed")
	}
}

// TestViewBounds: out-of-range indexes are healthy, unsuspected and
// untracked.
func TestViewBounds(t *testing.T) {
	tr, clk, _, _ := newTracker(t, 1, nil)
	v := tr.View(clk.Now())
	var a Attempt
	for _, i := range []int{-1, 1} {
		if v.Down(i) || v.Suspect(i) || v.Avoid(i) || v.Failures(i) != 0 || v.Identity(i) != "" || v.Begin(&a, i, clk.Now()) {
			t.Fatalf("index %d not neutral", i)
		}
	}
	if a.End(clk.Now(), Outcome{Failed: true}) || a.Stalled() {
		t.Fatal("untracked Attempt ended with an effect")
	}
	a.Headers(clk.Now())
	a.Cancel()
}

// TestSetPolicyActiveOff: removing healthCheck.active marks every
// Endpoint healthy again.
func TestSetPolicyActiveOff(t *testing.T) {
	tr, clk, _, log := newTracker(t, 2, func(c *Config) { c.Active = &ActivePolicy{UnhealthyThreshold: 1} })
	if tr.Active() == nil || tr.Active().Path != DefaultProbePath {
		t.Fatalf("active %+v", tr.Active())
	}
	tr.RecordProbe("ep-000", false, clk.Now())
	if !tr.View(clk.Now()).Down(0) || log.last() != [2]int{1, 2} {
		t.Fatal("probe failure did not mark unhealthy")
	}
	tr.SetPolicy(PassivePolicy{}, nil, 0)
	if tr.View(clk.Now()).Down(0) || log.last() != [2]int{2, 2} || tr.Active() != nil {
		t.Fatal("active removal kept the verdict")
	}
	tr.SetPolicy(PassivePolicy{}, nil, 0) // already off: no-op
	if tr.RecordProbe("ep-000", false, clk.Now()) {
		t.Fatal("probe recorded without an active policy")
	}
}

// TestStatus covers the /debug/upstreams Endpoint fields (05 req 96).
func TestStatus(t *testing.T) {
	tr, clk, _, _ := newTracker(t, 2, func(c *Config) {
		c.Passive = PassivePolicy{ConsecutiveErrors: 3, EjectionTime: 5 * time.Second}
		c.Active = &ActivePolicy{UnhealthyThreshold: 1}
	})
	fail(tr, clk, 0, CauseOther)
	fail(tr, clk, 0, CauseOther)
	tr.RecordProbe("ep-001", false, clk.Now())
	st := tr.Status(clk.Now())
	if st[0].Identity != "ep-000" || st[0].ConsecutiveErrors != 2 || st[0].Failures1s != 2 || st[0].Ejected || !st[0].Healthy {
		t.Fatalf("status[0] %+v", st[0])
	}
	if st[1].Healthy || st[1].Ejected || !st[1].EjectedUntil.IsZero() {
		t.Fatalf("status[1] %+v", st[1])
	}
	fail(tr, clk, 0, CauseOther)
	st = tr.Status(clk.Now())
	if !st[0].Ejected || st[0].EjectedUntil.Location() != time.UTC || st[0].Ejections != 1 {
		t.Fatalf("status[0] after ejection %+v", st[0])
	}
}

func TestPolicyDefaults(t *testing.T) {
	p := PassivePolicy{}.WithDefaults()
	if p.ConsecutiveErrors != 5 || p.EjectionTime != 30*time.Second {
		t.Fatalf("passive defaults %+v", p)
	}
	a := ActivePolicy{}.WithDefaults()
	if a.Path != "/" || a.Interval != 10*time.Second || a.Timeout != 2*time.Second || a.HealthyThreshold != 2 || a.UnhealthyThreshold != 3 {
		t.Fatalf("active defaults %+v", a)
	}
	custom := ActivePolicy{Path: "/healthz", Interval: time.Second, Timeout: time.Millisecond, HealthyThreshold: 7, UnhealthyThreshold: 9}
	if custom.WithDefaults() != custom {
		t.Fatal("WithDefaults changed set fields")
	}
	tr := NewTracker(Config{})
	if tr.clk == nil || tr.maxConns.Load() != DefaultMaxConnections || tr.Len() != 0 {
		t.Fatal("tracker defaults")
	}
	if h, n := tr.Healthy(time.Now()); h != 0 || n != 0 {
		t.Fatal("empty tracker")
	}
}

// TestFailureWindow covers 05 req 13: attempts matching failureWhen in the
// last second, in 100 ms buckets.
func TestFailureWindow(t *testing.T) {
	var w window
	base := epoch().UnixNano()
	for i := range 10 {
		w.add(base + int64(i)*int64(100*time.Millisecond))
	}
	if got := w.sum(base + int64(900*time.Millisecond)); got != 10 {
		t.Fatalf("sum %d, want 10", got)
	}
	if got := w.sum(base + int64(1000*time.Millisecond)); got != 9 {
		t.Fatalf("sum after 1 s %d, want 9", got)
	}
	if got := w.sum(base + int64(5*time.Second)); got != 0 {
		t.Fatalf("sum after 5 s %d", got)
	}
	w.add(base + int64(5*time.Second))
	w.add(base + int64(5*time.Second))
	if got := w.sum(base + int64(5*time.Second)); got != 2 {
		t.Fatalf("reused bucket sum %d", got)
	}
}

// TestOnHealthyLatestWins: when two changes report out of order (a
// goroutine preempted between its change and its report), the last
// OnHealthy delivery still carries the current count, so the Upstream
// layer's upstream_panic reason (05 req 11 step b) never sticks; equal
// values are not delivered twice.
func TestOnHealthyLatestWins(t *testing.T) {
	tr, clk, _, log := newTracker(t, 2, func(c *Config) { c.Passive = PassivePolicy{ConsecutiveErrors: 1, EjectionTime: 10 * time.Second} })
	calls := func() int {
		log.mu.Lock()
		defer log.mu.Unlock()
		return len(log.calls)
	}
	before := calls()
	// Change A ejects ep-000 but its report is held back.
	e := tr.tab.Load().eps[0]
	tr.mu.Lock()
	e.ejectedUntil.Store(tr.at(clk.Now().Add(10 * time.Second)))
	changedA := tr.recountLocked(tr.at(clk.Now()))
	tr.mu.Unlock()
	if !changedA {
		t.Fatal("ejection did not change the count")
	}
	// Change B, the ejection's end, reports first.
	clk.Advance(10 * time.Second)
	// A's late report must not deliver its stale (1, 2).
	tr.report(changedA)
	if got := log.last(); got != [2]int{2, 2} {
		t.Fatalf("last OnHealthy %v, want [2 2]", got)
	}
	if calls() != before {
		t.Fatalf("%d deliveries of an unchanged count", calls()-before)
	}
	// SetMetrics reports a change it finds.
	tr.mu.Lock()
	e.ejectedUntil.Store(tr.at(clk.Now().Add(time.Hour)))
	tr.mu.Unlock()
	tr.SetMetrics(nil)
	if got := log.last(); got != [2]int{1, 2} {
		t.Fatalf("SetMetrics did not report the change: %v", got)
	}
}
