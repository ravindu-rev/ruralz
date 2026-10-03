// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestStateLabels_05Req37 aligns the states with the state label of
// ruralz_upstream_breaker_state_info and the emit indexes.
func TestStateLabels_05Req37(t *testing.T) {
	fam, ok := catalog.Lookup(catalog.UpstreamBreakerStateInfo)
	if !ok {
		t.Fatal("catalog has no ruralz_upstream_breaker_state_info")
	}
	i := slices.IndexFunc(fam.Labels, func(l catalog.Label) bool { return l.Name == "state" })
	var names []string
	for s := range State(NumStates) {
		names = append(names, s.String())
	}
	if i < 0 || !slices.Equal(names, fam.Labels[i].Values) {
		t.Fatalf("states %v, catalog labels %v", names, fam.Labels)
	}
	if NumStates != emit.NumBreakerStates || int(StateClosed) != emit.BreakerClosed || int(StateOpen) != emit.BreakerOpen || int(StateHalfOpen) != emit.BreakerHalfOpen {
		t.Fatal("State values differ from the emit breaker-state indexes")
	}
	if State(9).String() != "closed" {
		t.Error("unknown state label")
	}
}

// TestTransition_05Req37 lists the edges of Figure 2.
func TestTransition_05Req37(t *testing.T) {
	edges := map[edge]bool{
		{StateClosed, StateOpen}:     true,
		{StateOpen, StateHalfOpen}:   true,
		{StateHalfOpen, StateClosed}: true,
		{StateHalfOpen, StateOpen}:   true,
	}
	for from := range State(NumStates + 1) {
		for to := range State(NumStates + 1) {
			if got := Transition(from, to); got != edges[edge{from, to}] {
				t.Errorf("Transition(%v, %v) = %v", from, to, got)
			}
		}
	}
}

// breakerFixture is a breaker on a fake clock with zero-jitter draws, so
// the open time is exactly 80% of openDuration.
type breakerFixture struct {
	clk *clocktest.Fake
	rec *recorder
	b   *Breaker
	cfg BreakerConfig
}

func newBreakerFixture() *breakerFixture {
	f := &breakerFixture{clk: newClock(), rec: &recorder{}, cfg: DefaultBreakerConfig()}
	f.b = NewBreaker(f.clk, fixedSource{0}, f.rec.record)
	return f
}

// leg runs one leg through the breaker and reports whether it was allowed.
func (f *breakerFixture) leg(r LegResult) bool {
	p, ok := f.b.Allow(&f.cfg)
	if ok {
		f.b.Record(&f.cfg, p, r)
	}
	return ok
}

// legs runs n legs with result r.
func (f *breakerFixture) legs(t *testing.T, n int, r LegResult) {
	t.Helper()
	for i := range n {
		if !f.leg(r) {
			t.Fatalf("leg %d of %d refused in state %v", i+1, n, f.b.State())
		}
	}
}

// TestBreakerBothConditions_05Req37 is part of 05 section 6 test 6: the
// consecutive and the ratio conditions are both required, and below
// minimumLegs the breaker never opens.
func TestBreakerBothConditions_05Req37(t *testing.T) {
	t.Run("consecutive without ratio stays closed", func(t *testing.T) {
		f := newBreakerFixture()
		f.legs(t, 15, LegSuccess)
		f.legs(t, 5, LegFailure) // 5 consecutive, 25% of 20
		if f.b.State() != StateClosed {
			t.Fatal("opened at 25% failures")
		}
		f.legs(t, 9, LegFailure) // 14 of 29: 48%
		if f.b.State() != StateClosed {
			t.Fatal("opened below 50%")
		}
		f.legs(t, 1, LegFailure) // 15 of 30: 50%
		if f.b.State() != StateOpen {
			t.Fatal("did not open at 50% of 30 legs with 15 consecutive failures")
		}
	})
	t.Run("ratio without consecutive stays closed", func(t *testing.T) {
		f := newBreakerFixture()
		for range 50 {
			f.legs(t, 1, LegFailure)
			f.legs(t, 1, LegSuccess)
		}
		f.legs(t, 4, LegFailure) // 54 of 104 failed, only 4 in a row
		if f.b.State() != StateClosed {
			t.Fatal("opened without consecutiveFailures")
		}
		f.legs(t, 1, LegFailure)
		if f.b.State() != StateOpen {
			t.Fatal("did not open at the 5th consecutive failure")
		}
	})
	t.Run("below minimumLegs never opens", func(t *testing.T) {
		f := newBreakerFixture()
		f.legs(t, 19, LegFailure)
		if f.b.State() != StateClosed {
			t.Fatal("opened with 19 legs in the window")
		}
		f.legs(t, 1, LegFailure)
		if f.b.State() != StateOpen {
			t.Fatal("did not open at 20 failed legs")
		}
	})
	t.Run("the window rolls over ten 1 s buckets", func(t *testing.T) {
		f := newBreakerFixture()
		f.legs(t, 15, LegFailure)
		f.clk.Advance(10 * time.Second) // those 15 legs leave the window
		f.legs(t, 19, LegFailure)
		if f.b.State() != StateClosed {
			t.Fatal("counted legs older than 10 s")
		}
		f.clk.Advance(9 * time.Second) // the 19 legs are still inside
		f.legs(t, 1, LegFailure)
		if f.b.State() != StateOpen {
			t.Fatal("dropped legs younger than 10 s")
		}
	})
	t.Run("not-counted legs change nothing", func(t *testing.T) {
		f := newBreakerFixture()
		f.legs(t, 19, LegFailure)
		f.legs(t, 100, LegNotCounted)
		f.legs(t, 1, LegFailure)
		if f.b.State() != StateOpen {
			t.Fatal("not-counted legs reset the run or the window")
		}
	})
}

// TestBreakerOpenAndHalfOpen_05Req37 is part of 05 section 6 test 6: open
// jitter, a single half-open probe, three successes close, one failure
// reopens.
func TestBreakerOpenAndHalfOpen_05Req37(t *testing.T) {
	f := newBreakerFixture()
	f.legs(t, 20, LegFailure)
	opened := f.clk.Now()
	if f.b.State() != StateOpen || !f.b.OpenUntil().Equal(opened.Add(24*time.Second)) {
		t.Fatalf("state %v until %v", f.b.State(), f.b.OpenUntil())
	}
	f.clk.Advance(24*time.Second - time.Nanosecond)
	if _, ok := f.b.Allow(&f.cfg); ok {
		t.Fatal("open breaker admitted a leg before the jittered openDuration")
	}
	f.clk.Advance(time.Nanosecond)
	probe, ok := f.b.Allow(&f.cfg)
	if !ok || !probe.Probe() || f.b.State() != StateHalfOpen {
		t.Fatalf("no probe after openDuration: ok %v, state %v", ok, f.b.State())
	}
	if !f.b.OpenUntil().IsZero() {
		t.Fatal("OpenUntil set while half-open")
	}
	if _, ok := f.b.Allow(&f.cfg); ok {
		t.Fatal("a second leg passed with the probe in flight")
	}
	f.b.Record(&f.cfg, probe, LegNotCounted) // a gated probe frees the slot
	for i := range 3 {
		if f.b.State() != StateHalfOpen {
			t.Fatalf("closed after %d probe successes", i)
		}
		p, ok := f.b.Allow(&f.cfg)
		if !ok || !p.Probe() {
			t.Fatalf("probe %d refused", i+1)
		}
		f.b.Record(&f.cfg, p, LegSuccess)
	}
	if f.b.State() != StateClosed {
		t.Fatal("three probe successes did not close the breaker")
	}
	// A fresh window after closing: 19 failures do not reopen it.
	f.legs(t, 19, LegFailure)
	if f.b.State() != StateClosed {
		t.Fatal("old legs counted after closing")
	}
	f.legs(t, 1, LegFailure)
	f.clk.Advance(24 * time.Second)
	p, _ := f.b.Allow(&f.cfg)
	f.b.Record(&f.cfg, p, LegSuccess)
	p, _ = f.b.Allow(&f.cfg)
	f.b.Record(&f.cfg, p, LegFailure)
	if f.b.State() != StateOpen {
		t.Fatal("a probe failure did not reopen the breaker")
	}
	want := []edge{
		{StateClosed, StateOpen},
		{StateOpen, StateHalfOpen},
		{StateHalfOpen, StateClosed},
		{StateClosed, StateOpen},
		{StateOpen, StateHalfOpen},
		{StateHalfOpen, StateOpen},
	}
	if got := f.rec.all(); !slices.Equal(got, want) {
		t.Fatalf("transitions %v, want %v", got, want)
	}
}

// TestBreakerOpenJitter_05Req37 keeps the open time within ±20%.
func TestBreakerOpenJitter_05Req37(t *testing.T) {
	clk := newClock()
	cfg := DefaultBreakerConfig()
	cfg.MinimumLegs, cfg.ConsecutiveFailures = 1, 1
	for i, src := range []*lockedSource{newLockedSource(1), newLockedSource(2)} {
		b := NewBreaker(clk, src, nil)
		lo, hi := time.Duration(math.MaxInt64), time.Duration(0)
		for range 500 {
			p, _ := b.Allow(&cfg)
			b.Record(&cfg, p, LegFailure) // closed → open, or a failed probe
			d := b.OpenUntil().Sub(clk.Now())
			lo, hi = min(lo, d), max(hi, d)
			clk.Advance(d)
		}
		if lo < 24*time.Second || hi > 36*time.Second || hi-lo < 10*time.Second {
			t.Fatalf("source %d: open times in [%v, %v], want spread within [24s, 36s]", i, lo, hi)
		}
	}
	b := NewBreaker(clk, fixedSource{math.MaxUint64}, nil)
	p, _ := b.Allow(&cfg)
	b.Record(&cfg, p, LegFailure)
	if d := b.OpenUntil().Sub(clk.Now()); d != 36*time.Second {
		t.Fatalf("largest draw: %v", d)
	}
	cfg.OpenDuration = 0
	b = NewBreaker(clk, nil, nil)
	p, _ = b.Allow(&cfg)
	b.Record(&cfg, p, LegFailure)
	if p, ok := b.Allow(&cfg); !ok || !p.Probe() {
		t.Fatal("openDuration 0 did not let the probe through at once")
	}
}

// TestBreakerStalePermits: results of legs admitted in an earlier period
// are ignored.
func TestBreakerStalePermits_05Req37(t *testing.T) {
	f := newBreakerFixture()
	old, _ := f.b.Allow(&f.cfg)
	f.legs(t, 20, LegFailure)
	f.b.Record(&f.cfg, old, LegSuccess) // finishes while open
	f.clk.Advance(24 * time.Second)
	for range 3 {
		p, _ := f.b.Allow(&f.cfg)
		f.b.Record(&f.cfg, p, LegSuccess)
	}
	f.b.Record(&f.cfg, old, LegFailure) // and again after closing
	f.legs(t, 19, LegFailure)
	if f.b.State() != StateClosed {
		t.Fatal("a stale permit counted in the new closed period")
	}
}

// TestBreakerAdmits_05Req11: a leg may start another attempt only while
// the breaker is closed, or half-open and the leg is the current probe
// (05 req 11 step e, req 33 condition 3, req 37).
func TestBreakerAdmits_05Req11(t *testing.T) {
	f := newBreakerFixture()
	f.cfg.OpenDuration = 0
	old, _ := f.b.Allow(&f.cfg)
	if !f.b.Admits(old) {
		t.Fatal("closed breaker refused a retry")
	}
	f.legs(t, 20, LegFailure)
	if f.b.State() != StateOpen || f.b.Admits(old) {
		t.Fatal("open breaker admitted a retry")
	}
	probe, ok := f.b.Allow(&f.cfg) // openDuration 0: half-open at once
	if !ok || !probe.Probe() || f.b.State() != StateHalfOpen {
		t.Fatalf("no probe: %v", f.b.State())
	}
	if f.b.Admits(old) {
		t.Fatal("a leg admitted while closed retried beside the half-open probe")
	}
	if !f.b.Admits(probe) {
		t.Fatal("the probe may not retry")
	}
	if f.b.Admits(Permit{gen: probe.gen}) || f.b.Admits(Permit{gen: probe.gen - 1, probe: true}) {
		t.Fatal("a non-probe or stale probe permit admitted while half-open")
	}
	for range 3 {
		f.b.Record(&f.cfg, probe, LegSuccess)
		probe, _ = f.b.Allow(&f.cfg)
	}
	if f.b.State() != StateClosed || !f.b.Admits(old) {
		t.Fatalf("state %v: a closed breaker admits every leg's retry", f.b.State())
	}
}

// TestBreakerStats_05Req96 reads the fields of the breaker object of
// /debug/upstreams in every state.
func TestBreakerStats_05Req96(t *testing.T) {
	f := newBreakerFixture()
	type step struct {
		name string
		do   func()
		want BreakerStats
	}
	var opened time.Time
	steps := []step{
		{"new", func() {}, BreakerStats{State: StateClosed}},
		{"closed with legs", func() {
			f.legs(t, 6, LegSuccess)
			f.legs(t, 3, LegFailure)
		}, BreakerStats{State: StateClosed, WindowLegs: 9, WindowFailures: 3, ConsecutiveFailures: 3}},
		{"window spans buckets", func() {
			f.clk.Advance(4 * time.Second)
			f.legs(t, 1, LegFailure)
		}, BreakerStats{State: StateClosed, WindowLegs: 10, WindowFailures: 4, ConsecutiveFailures: 4}},
		{"old bucket leaves the window", func() {
			f.clk.Advance(6 * time.Second)
		}, BreakerStats{State: StateClosed, WindowLegs: 1, WindowFailures: 1, ConsecutiveFailures: 4}},
		{"open", func() {
			f.legs(t, 19, LegFailure)
			opened = f.clk.Now()
		}, BreakerStats{State: StateOpen, WindowLegs: 20, WindowFailures: 20}},
		{"half-open keeps openedAt", func() {
			f.clk.Advance(24 * time.Second)
			p, _ := f.b.Allow(&f.cfg)
			f.b.Record(&f.cfg, p, LegSuccess)
		}, BreakerStats{State: StateHalfOpen}},
		{"closed again", func() {
			f.legs(t, 2, LegSuccess)
		}, BreakerStats{State: StateClosed}},
	}
	for _, st := range steps {
		st.do()
		want := st.want
		if want.State != StateClosed {
			want.OpenedAt = opened
		}
		if want.State == StateOpen {
			want.OpenUntil = opened.Add(24 * time.Second)
		}
		if got := f.b.Stats(); got != want {
			t.Fatalf("%s: Stats() = %+v, want %+v", st.name, got, want)
		}
	}
}

// TestBreakerProbeLease_05Req37: a probe that outlives its lease (its leg
// deadline plus openDuration) is presumed lost; the next leg takes the
// probe slot and the lost probe's permit goes stale. Without a lease the
// slot is held until the probe ends.
func TestBreakerProbeLease_05Req37(t *testing.T) {
	f := newBreakerFixture()
	f.legs(t, 20, LegFailure)
	f.clk.Advance(24 * time.Second)
	legDeadline := f.clk.Now().Add(15 * time.Second)
	lost, ok := f.b.AllowLease(&f.cfg, legDeadline)
	if !ok || !lost.Probe() {
		t.Fatal("no probe")
	}
	f.clk.Advance(15*time.Second + DefaultOpenDuration - time.Nanosecond)
	if _, ok := f.b.AllowLease(&f.cfg, f.clk.Now().Add(time.Second)); ok {
		t.Fatal("a second probe within the lease")
	}
	f.clk.Advance(time.Nanosecond)
	probe, ok := f.b.AllowLease(&f.cfg, time.Time{})
	if !ok || !probe.Probe() || f.b.State() != StateHalfOpen {
		t.Fatal("the lapsed lease was not taken over")
	}
	if f.b.Admits(lost) || !f.b.Admits(probe) {
		t.Fatal("the lost probe may still retry")
	}
	f.b.Record(&f.cfg, lost, LegFailure) // stale: ignored
	if f.b.State() != StateHalfOpen {
		t.Fatal("the lost probe's late result counted")
	}
	f.clk.Advance(time.Hour) // the new probe has no lease
	if _, ok := f.b.Allow(&f.cfg); ok {
		t.Fatal("a probe without a lease was taken over")
	}
	f.b.Record(&f.cfg, probe, LegFailure)
	want := []edge{{StateClosed, StateOpen}, {StateOpen, StateHalfOpen}, {StateHalfOpen, StateOpen}}
	if got := f.rec.all(); !slices.Equal(got, want) {
		t.Fatalf("transitions %v, want %v (a takeover is no transition)", got, want)
	}
}

// TestBreakerResult_05Req37: gated legs are not counted; a connect error
// counts only while more than 50% of the Endpoints are ejected.
func TestBreakerResult_05Req37(t *testing.T) {
	tests := []struct {
		name               string
		failed             bool
		o                  Outcome
		ejected, endpoints int
		want               LegResult
	}{
		{"breaker gate", true, Outcome{Gate: GateBreaker}, 0, 4, LegNotCounted},
		{"bulkhead gate", true, Outcome{Gate: GateBulkhead}, 0, 4, LegNotCounted},
		{"empty set", true, Outcome{Gate: GateNoEndpoint}, 0, 0, LegNotCounted},
		{"connect, none ejected", true, Outcome{Kind: KindConnect, Attempts: 2}, 0, 4, LegNotCounted},
		{"connect, half ejected", true, Outcome{Kind: KindConnect, Attempts: 1}, 2, 4, LegNotCounted},
		{"connect, majority ejected", true, Outcome{Kind: KindConnect, Attempts: 1}, 3, 4, LegFailure},
		{"connect, all ejected", true, Outcome{Kind: KindConnect, Attempts: 1}, 1, 1, LegFailure},
		{"reset", true, Outcome{Kind: KindReset, Attempts: 1}, 0, 4, LegFailure},
		{"timeout", true, Outcome{Kind: KindTimeout, Attempts: 1, Expired: true}, 0, 4, LegFailure},
		{"503 response", true, Outcome{Responded: true, Attempts: 1}, 0, 4, LegFailure},
		{"200 response", false, Outcome{Responded: true, Attempts: 1}, 0, 4, LegSuccess},
		{"429 not a failure by default", false, Outcome{Responded: true, Attempts: 3}, 0, 4, LegSuccess},
	}
	for _, tt := range tests {
		if got := BreakerResult(tt.failed, tt.o, tt.ejected, tt.endpoints); got != tt.want {
			t.Errorf("%s: BreakerResult = %d, want %d", tt.name, got, tt.want)
		}
	}
}

// TestBreakerHotReload_05Req2 carries the state across a Hot Reload while
// the new snapshot's thresholds apply to later calls.
func TestBreakerHotReload_05Req2(t *testing.T) {
	f := newBreakerFixture()
	f.legs(t, 10, LegFailure)
	f.cfg = BreakerConfig{ConsecutiveFailures: 3, FailureRatio: 0.5, MinimumLegs: 11, HalfOpenSuccesses: 1, OpenDuration: 10 * time.Second}
	f.legs(t, 1, LegFailure) // 11 legs: opens under the new minimum
	if f.b.State() != StateOpen || !f.b.OpenUntil().Equal(f.clk.Now().Add(8*time.Second)) {
		t.Fatalf("state %v until %v after the reload", f.b.State(), f.b.OpenUntil())
	}
	f.clk.Advance(8 * time.Second)
	f.legs(t, 1, LegSuccess) // halfOpenSuccesses 1
	if f.b.State() != StateClosed {
		t.Fatal("the new halfOpenSuccesses did not apply")
	}
}

// TestBreakerConcurrent runs legs from many goroutines under -race: the
// breaker never admits two probes at once and every transition is an edge
// of Figure 2 (05 req 37).
func TestBreakerConcurrent(t *testing.T) {
	clk := newClock()
	cfg := BreakerConfig{ConsecutiveFailures: 2, FailureRatio: 0.1, MinimumLegs: 4, HalfOpenSuccesses: 2}
	var (
		mu     sync.Mutex
		probes int
		maxP   int
	)
	b := NewBreaker(clk, newLockedSource(9), func(from, to State) {
		if !Transition(from, to) {
			t.Errorf("illegal transition %v → %v", from, to)
		}
	})
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 500 {
				p, ok := b.Allow(&cfg)
				if !ok {
					continue
				}
				if p.Probe() {
					mu.Lock()
					probes++
					maxP = max(maxP, probes)
					mu.Unlock()
				}
				r := LegSuccess
				if (g+i)%3 == 0 {
					r = LegFailure
				}
				if p.Probe() {
					mu.Lock()
					probes--
					mu.Unlock()
				}
				b.Record(&cfg, p, r)
			}
		})
	}
	wg.Wait()
	if maxP > 1 {
		t.Fatalf("%d probes in flight at once", maxP)
	}
}

// FuzzBreakerTransitions is 05 section 6 test 27: under any sequence of
// legs, results, clock steps and reloads every transition is an edge of
// Figure 2, at most one probe is in flight, and the state the callback
// reports is the state the breaker reads.
func FuzzBreakerTransitions(f *testing.F) {
	f.Add([]byte{0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 5, 0, 2})
	f.Add([]byte{0, 0, 0, 0, 1, 1, 1, 1, 1, 1, 6, 0, 0, 2, 2, 2, 7, 0, 3})
	f.Add([]byte{0, 1, 5, 0, 3, 0, 4, 0, 2, 8, 0, 1})
	f.Fuzz(func(t *testing.T, ops []byte) {
		clk := newClock()
		cfgs := []BreakerConfig{
			{ConsecutiveFailures: 1, FailureRatio: 0.5, MinimumLegs: 2, HalfOpenSuccesses: 1, OpenDuration: time.Second},
			{ConsecutiveFailures: 2, FailureRatio: 0, MinimumLegs: 1, HalfOpenSuccesses: 2, OpenDuration: 0},
			DefaultBreakerConfig(),
		}
		cfg := &cfgs[0]
		var (
			last    = StateClosed
			permits []Permit
			probes  int
		)
		b := NewBreaker(clk, fixedSource{0x5555_5555_5555_5555}, func(from, to State) {
			if !Transition(from, to) {
				t.Fatalf("illegal transition %v → %v", from, to)
			}
			if from != last {
				t.Fatalf("transition from %v while the last state was %v", from, last)
			}
			last = to
		})
		end := func(i int, r LegResult) {
			p := permits[i]
			permits = append(permits[:i], permits[i+1:]...)
			if p.Probe() {
				probes--
			}
			b.Record(cfg, p, r)
		}
		for _, op := range ops {
			switch op % 9 {
			case 0: // a leg arrives
				if p, ok := b.Allow(cfg); ok {
					permits = append(permits, p)
					if p.Probe() {
						probes++
					}
				}
			case 1, 2, 3: // the oldest leg ends: failure, success, not counted
				if len(permits) > 0 {
					end(0, [...]LegResult{LegFailure, LegSuccess, LegNotCounted}[op%9-1])
				}
			case 4: // the newest leg fails
				if len(permits) > 0 {
					end(len(permits)-1, LegFailure)
				}
			case 5:
				clk.Advance(300 * time.Millisecond)
			case 6:
				clk.Advance(30 * time.Second)
			case 7, 8: // Hot Reload
				cfg = &cfgs[int(op)%len(cfgs)]
			}
			if probes > 1 {
				t.Fatalf("%d probes in flight", probes)
			}
			admitted := 0
			for _, p := range permits {
				if b.Admits(p) {
					admitted++
				}
			}
			if st := b.State(); (st == StateOpen && admitted > 0) || (st == StateHalfOpen && admitted > 1) {
				t.Fatalf("%d legs may retry in state %v", admitted, st)
			}
			if b.State() != last {
				t.Fatalf("State() = %v, callback reported %v", b.State(), last)
			}
		}
	})
}

// BenchmarkBreakerAccounting is the breaker budget of 05 section 6 test 38
// (2 µs p50, 8 µs p99 target): one closed-state Allow and Record per leg,
// from every core.
func BenchmarkBreakerAccounting(b *testing.B) {
	br := NewBreaker(clock.Real(), nil, nil)
	cfg := DefaultBreakerConfig()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p, _ := br.Allow(&cfg)
			br.Record(&cfg, p, LegSuccess)
		}
	})
}

// TestBreakerAccountingAllocs keeps the closed-state accounting free of
// allocations (05 section 6 test 22).
func TestBreakerAccountingAllocs(t *testing.T) {
	br := NewBreaker(clock.Real(), nil, nil)
	cfg := DefaultBreakerConfig()
	allocs := testing.AllocsPerRun(1000, func() {
		p, _ := br.Allow(&cfg)
		br.Record(&cfg, p, LegFailure)
		p, _ = br.Allow(&cfg)
		br.Record(&cfg, p, LegSuccess)
	})
	if allocs != 0 {
		t.Fatalf("%v allocations per leg", allocs)
	}
}
