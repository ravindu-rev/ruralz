// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package breaker

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Tests for spec 08 reqs 19 to 22 and the breaker and Pacer rows of the
// 08 section 6.1 unit table, on clocktest.Fake with a fixed random delay.

func start() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// fixed returns a Rand that always answers d (clamped by the breaker).
func fixed(d time.Duration) Rand {
	return func(time.Duration, time.Duration) time.Duration { return d }
}

func newBreaker(t *testing.T, delay time.Duration) (*Breaker, *clocktest.Fake, *[]string) {
	t.Helper()
	clk := clocktest.New(start())
	var log []string
	cfg := DefaultConfig()
	cfg.OnChange = func(from, to State) { log = append(log, from.String()+">"+to.String()) }
	return New(clk, cfg, fixed(delay)), clk, &log
}

// run completes n calls, the first fails of them failing.
func run(t *testing.T, b *Breaker, n, fails int) {
	t.Helper()
	for i := range n {
		var tk Ticket
		if !b.Allow(&tk) {
			t.Fatalf("call %d not admitted in state %v", i, b.State())
		}
		b.Done(&tk, i >= fails)
	}
}

func TestDefaultsMatchLimits(t *testing.T) {
	// The breaker targets equal statestore.DefaultLimits (OQ-scalability-
	// and-distributed-state-4 (a)).
	l := statestore.DefaultLimits()
	c := DefaultConfig()
	if c.Window != l.BreakerWindow || c.MinCalls != l.BreakerMinCalls || c.FailureRatio != l.BreakerFailureRatio ||
		c.ConnectFailures != l.BreakerConnectFailures || c.OpenMin != l.BreakerOpenMin || c.OpenMax != l.BreakerOpenMax ||
		c.CloseAfter != l.BreakerCloseAfter {
		t.Fatalf("DefaultConfig %+v differs from statestore.DefaultLimits %+v", c, l)
	}
	p := DefaultPacerConfig()
	if p.Base != l.ReconnectBase || p.Cap != l.ReconnectCap || p.PerSecond != l.ConnectsPerSecond {
		t.Fatalf("DefaultPacerConfig %+v differs from statestore.DefaultLimits %+v", p, l)
	}
}

func TestOpensOnFailureRatio(t *testing.T) {
	// 08 req 19: opens when at least 50% of at least 20 calls completed in
	// a sliding 5 s window failed.
	cases := []struct {
		name        string
		calls, fail int
		want        State
	}{
		{"19 of 19 fail: under MinCalls", 19, 19, Closed},
		{"10 of 20 fail", 20, 10, Open},
		{"9 of 20 fail", 20, 9, Closed},
		{"20 of 20 fail", 20, 20, Open},
		{"0 of 100 fail", 100, 0, Closed},
		{"49 of 100 fail", 100, 49, Closed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, _, _ := newBreaker(t, 2*time.Second)
			// Successes first, so the verdict comes at the last failure.
			for i := range c.calls {
				var tk Ticket
				if !b.Allow(&tk) {
					break
				}
				b.Done(&tk, i < c.calls-c.fail)
			}
			if got := b.State(); got != c.want {
				t.Fatalf("state %v, want %v", got, c.want)
			}
		})
	}
}

func TestWindowRolls(t *testing.T) {
	// 08 req 19: the window slides; failures older than 5 s stop counting.
	b, clk, _ := newBreaker(t, 2*time.Second)
	run(t, b, 10, 10)
	clk.Advance(5 * time.Second)
	run(t, b, 10, 10)
	if got := b.State(); got != Closed {
		t.Fatalf("state %v after the window rolled; want closed (only 10 calls inside)", got)
	}
	// Inside one window the same calls open it.
	clk.Advance(4 * time.Second)
	run(t, b, 10, 10)
	if got := b.State(); got != Open {
		t.Fatalf("state %v, want open: 20 failures within 5 s", got)
	}
}

func TestConnectFailuresOpen(t *testing.T) {
	// 08 req 19: 5 consecutive connect failures open it with zero calls; a
	// success in between resets the count.
	b, _, log := newBreaker(t, time.Second)
	for range 4 {
		b.ConnectFailed()
	}
	b.ConnectSucceeded()
	for range 4 {
		b.ConnectFailed()
	}
	if b.State() != Closed {
		t.Fatal("opened before 5 consecutive connect failures")
	}
	b.ConnectFailed()
	if b.State() != Open {
		t.Fatalf("state %v after 5 consecutive connect failures, want open", b.State())
	}
	if len(*log) != 1 || (*log)[0] != "closed>open" {
		t.Fatalf("transitions %v", *log)
	}
	// Connect failures while open change nothing.
	for range 10 {
		b.ConnectFailed()
	}
	if len(*log) != 1 {
		t.Fatalf("transitions %v", *log)
	}
}

func TestOpenDelayThenHalfOpen(t *testing.T) {
	// 08 req 19: open skips every call without waiting; after the random
	// delay in [1 s, 3 s] it becomes half-open.
	for _, delay := range []time.Duration{time.Second, 1500 * time.Millisecond, 3 * time.Second} {
		b, clk, _ := newBreaker(t, delay)
		run(t, b, 20, 20)
		var tk Ticket
		clk.Advance(delay - time.Millisecond)
		if b.Allow(&tk) || b.State() != Open {
			t.Fatalf("delay %v: admitted or not open 1 ms early", delay)
		}
		clk.Advance(time.Millisecond)
		if b.State() != HalfOpen {
			t.Fatalf("delay %v: state %v at the delay, want half-open", delay, b.State())
		}
		if !b.Allow(&tk) || !tk.Probe() {
			t.Fatalf("delay %v: first call after the delay is not a probe", delay)
		}
	}
	// A Rand outside [OpenMin, OpenMax] is clamped.
	for _, c := range []struct{ rnd, want time.Duration }{{0, time.Second}, {time.Hour, 3 * time.Second}} {
		b, clk, _ := newBreaker(t, c.rnd)
		run(t, b, 20, 20)
		clk.Advance(c.want - time.Millisecond)
		if b.State() != Open {
			t.Fatalf("rand %v: not open before %v", c.rnd, c.want)
		}
		clk.Advance(time.Millisecond)
		if b.State() != HalfOpen {
			t.Fatalf("rand %v: not half-open at %v", c.rnd, c.want)
		}
	}
}

func TestHalfOpenSingleProbe(t *testing.T) {
	// 08 req 19: half-open admits one probe at a time (64 concurrent
	// Allow calls, exactly one admitted).
	b, clk, _ := newBreaker(t, time.Second)
	run(t, b, 20, 20)
	clk.Advance(time.Second)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	tickets := make([]Ticket, 64)
	for i := range tickets {
		wg.Go(func() {
			if b.Allow(&tickets[i]) {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("%d probes admitted, want exactly 1", admitted.Load())
	}
}

func TestProbesCloseAndReopen(t *testing.T) {
	// 08 req 19: 3 consecutive successful probes close it; a failing probe
	// reopens it with a new random delay.
	b, clk, log := newBreaker(t, time.Second)
	run(t, b, 20, 20)
	clk.Advance(time.Second)
	for i := range 2 {
		var tk Ticket
		if !b.Allow(&tk) || !tk.Probe() {
			t.Fatalf("probe %d not admitted", i)
		}
		var other Ticket
		if b.Allow(&other) {
			t.Fatalf("second call admitted while probe %d in flight", i)
		}
		b.Done(&tk, true)
		if b.State() != HalfOpen {
			t.Fatalf("closed after %d probes", i+1)
		}
	}
	// A failing third probe reopens with a new delay.
	var tk Ticket
	b.Allow(&tk)
	b.Done(&tk, false)
	if b.State() != Open {
		t.Fatalf("state %v after a failed probe, want open", b.State())
	}
	clk.Advance(999 * time.Millisecond)
	if b.State() != Open {
		t.Fatal("reopened breaker ignored its new delay")
	}
	clk.Advance(time.Millisecond)
	for i := range 3 {
		var p Ticket
		if !b.Allow(&p) {
			t.Fatalf("probe %d not admitted", i)
		}
		b.Done(&p, true)
	}
	if b.State() != Closed {
		t.Fatalf("state %v after 3 successful probes, want closed", b.State())
	}
	want := []string{"closed>open", "open>half_open", "half_open>open", "open>half_open", "half_open>closed"}
	if len(*log) != len(want) {
		t.Fatalf("transitions %v, want %v", *log, want)
	}
	for i := range want {
		if (*log)[i] != want[i] {
			t.Fatalf("transitions %v, want %v", *log, want)
		}
	}
	// The closed breaker starts a fresh window: old failures do not count.
	run(t, b, 19, 19)
	if b.State() != Closed {
		t.Fatal("failures from before the close still counted")
	}
}

// recRand is a Rand that records its calls and answers delays in turn,
// so a test sees whether a transition drew a fresh open delay.
type recRand struct {
	delays []time.Duration
	calls  int
	bounds [][2]time.Duration
}

func (r *recRand) rand(lo, hi time.Duration) time.Duration {
	d := r.delays[r.calls%len(r.delays)]
	r.calls++
	r.bounds = append(r.bounds, [2]time.Duration{lo, hi})
	return d
}

// rig is one breaker under a transition test: its clock, change log,
// recording Rand and the probe ticket a half-open setup holds.
type rig struct {
	b     *Breaker
	clk   *clocktest.Fake
	log   []string
	rnd   *recRand
	probe Ticket
}

func TestTransitions(t *testing.T) {
	// Spec 08 req 19, every transition as (from state, event) -> (state,
	// OnChange calls, open delays drawn). The Rand answers 1 s, then 2 s,
	// then 3 s, so a reopen shows it drew a fresh delay: the second one.
	toOpen := func(t *testing.T, r *rig) {
		t.Helper()
		run(t, r.b, 20, 20)
	}
	toProbing := func(t *testing.T, r *rig) {
		t.Helper()
		toOpen(t, r)
		r.clk.Advance(time.Second)
		if !r.b.Allow(&r.probe) || !r.probe.Probe() {
			t.Fatal("setup: no probe admitted after the open delay")
		}
	}
	toIdleHalfOpen := func(t *testing.T, r *rig) {
		t.Helper()
		toProbing(t, r)
		r.b.Done(&r.probe, true)
	}
	// calls completes n calls, failures last, so the verdict comes at the
	// last failure.
	calls := func(n, fails int) func(*testing.T, *rig) {
		return func(t *testing.T, r *rig) {
			t.Helper()
			for i := range n {
				var tk Ticket
				if !r.b.Allow(&tk) {
					t.Fatalf("call %d not admitted", i)
				}
				r.b.Done(&tk, i < n-fails)
			}
		}
	}
	probes := func(n int, ok bool) func(*testing.T, *rig) {
		return func(t *testing.T, r *rig) {
			t.Helper()
			for i := range n {
				var p Ticket
				if !r.b.Allow(&p) || !p.Probe() {
					t.Fatalf("probe %d not admitted", i)
				}
				r.b.Done(&p, ok)
			}
		}
	}
	cases := []struct {
		name    string
		setup   func(*testing.T, *rig)
		from    State
		event   func(*testing.T, *rig)
		want    State
		changes []string
		draws   int
	}{
		{"closed, failure ratio reached", nil, Closed, calls(20, 10), Open, []string{"closed>open"}, 1},
		{"closed, failure ratio not reached", nil, Closed, calls(20, 9), Closed, nil, 0},
		{"closed, too few calls", nil, Closed, calls(19, 19), Closed, nil, 0},
		{
			"closed, 5 connect failures", nil, Closed,
			func(_ *testing.T, r *rig) {
				for range 5 {
					r.b.ConnectFailed()
				}
			}, Open,
			[]string{"closed>open"},
			1,
		},
		{
			"closed, 4 connect failures", nil, Closed,
			func(_ *testing.T, r *rig) {
				for range 4 {
					r.b.ConnectFailed()
				}
			}, Closed, nil, 0,
		},
		{
			"open, call before the delay", toOpen, Open,
			func(t *testing.T, r *rig) {
				r.clk.Advance(time.Second - time.Millisecond)
				var tk Ticket
				if r.b.Allow(&tk) {
					t.Fatal("open breaker admitted a call")
				}
			}, Open, nil, 0,
		},
		{
			"open, call after the delay", toOpen, Open,
			func(t *testing.T, r *rig) {
				r.clk.Advance(time.Second)
				var tk Ticket
				if !r.b.Allow(&tk) || !tk.Probe() {
					t.Fatal("first call after the delay is not a probe")
				}
			}, HalfOpen,
			[]string{"open>half_open"},
			0,
		},
		{
			"open, connect failures", toOpen, Open,
			func(_ *testing.T, r *rig) {
				for range 10 {
					r.b.ConnectFailed()
				}
			}, Open, nil, 0,
		},
		{
			"half-open, second call while probing", toProbing, HalfOpen,
			func(t *testing.T, r *rig) {
				var tk Ticket
				if r.b.Allow(&tk) {
					t.Fatal("second call admitted while a probe is in flight")
				}
			}, HalfOpen, nil, 0,
		},
		{
			"half-open, first probe succeeds", toProbing, HalfOpen,
			func(_ *testing.T, r *rig) { r.b.Done(&r.probe, true) }, HalfOpen, nil, 0,
		},
		{
			"half-open, probe fails", toProbing, HalfOpen,
			func(_ *testing.T, r *rig) { r.b.Done(&r.probe, false) }, Open,
			[]string{"half_open>open"},
			1,
		},
		{
			"half-open, later probe fails", toIdleHalfOpen, HalfOpen,
			probes(1, false), Open,
			[]string{"half_open>open"},
			1,
		},
		{
			"half-open, third consecutive success", toIdleHalfOpen, HalfOpen,
			probes(2, true), Closed,
			[]string{"half_open>closed"},
			0,
		},
		{
			"half-open, connect failures", toIdleHalfOpen, HalfOpen,
			func(_ *testing.T, r *rig) {
				for range 10 {
					r.b.ConnectFailed()
				}
			}, HalfOpen, nil, 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &rig{clk: clocktest.New(start()), rnd: &recRand{delays: []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}}}
			cfg := DefaultConfig()
			cfg.OnChange = func(from, to State) { r.log = append(r.log, from.String()+">"+to.String()) }
			r.b = New(r.clk, cfg, r.rnd.rand)
			if c.setup != nil {
				c.setup(t, r)
			}
			if got := r.b.State(); got != c.from {
				t.Fatalf("setup reached %v, want %v", got, c.from)
			}
			logged, drawn := len(r.log), r.rnd.calls
			c.event(t, r)
			if got := r.b.State(); got != c.want {
				t.Fatalf("state %v, want %v", got, c.want)
			}
			if got := r.log[logged:]; !slices.Equal(got, c.changes) {
				t.Fatalf("OnChange %v, want %v", got, c.changes)
			}
			if got := r.rnd.calls - drawn; got != c.draws {
				t.Fatalf("%d open delays drawn, want %d", got, c.draws)
			}
			for _, b := range r.rnd.bounds {
				if b != [2]time.Duration{time.Second, 3 * time.Second} {
					t.Fatalf("Rand called with [%v, %v], want [1s, 3s]", b[0], b[1])
				}
			}
			if c.want != Open || c.draws == 0 {
				return
			}
			// The breaker stays open exactly for the delay just drawn.
			delay := r.rnd.delays[drawn]
			r.clk.Advance(delay - time.Millisecond)
			if r.b.State() != Open {
				t.Fatalf("not open 1 ms before the drawn delay %v", delay)
			}
			r.clk.Advance(time.Millisecond)
			if r.b.State() != HalfOpen {
				t.Fatalf("not half-open at the drawn delay %v", delay)
			}
		})
	}
}

func TestDoneOnce(t *testing.T) {
	// 08 req 20: a timeout is recorded once even when the reply also
	// arrives; tickets never admitted record nothing.
	b, _, _ := newBreaker(t, time.Second)
	tickets := make([]Ticket, 20)
	for i := range tickets {
		if !b.Allow(&tickets[i]) {
			t.Fatal("not admitted")
		}
	}
	var wg sync.WaitGroup
	var firsts atomic.Int32
	for i := range tickets {
		for _, ok := range []bool{false, true} { // timeout, then late reply
			wg.Go(func() {
				if b.Done(&tickets[i], ok) {
					firsts.Add(1)
				}
			})
		}
	}
	wg.Wait()
	if firsts.Load() != 20 {
		t.Fatalf("%d first Done calls, want 20", firsts.Load())
	}
	if calls, _ := b.counts(b.period(b.clk.Now()), 0); calls != 20 {
		t.Fatalf("%d outcomes counted, want 20", calls)
	}
	var never Ticket
	if b.Done(&never, false) {
		t.Fatal("Done on an unadmitted ticket counted")
	}
	// A skipped call's ticket is not admitted either.
	b2, _, _ := newBreaker(t, time.Second)
	run(t, b2, 20, 20)
	var skipped Ticket
	if b2.Allow(&skipped) || b2.Done(&skipped, false) {
		t.Fatal("skipped call admitted or counted")
	}
}

func TestStaleOutcomesIgnored(t *testing.T) {
	// Calls admitted before a transition never count after it.
	b, clk, _ := newBreaker(t, time.Second)
	late := make([]Ticket, 30)
	for i := range late {
		b.Allow(&late[i])
	}
	run(t, b, 20, 20)
	clk.Advance(time.Second)
	for range 3 {
		var p Ticket
		b.Allow(&p)
		b.Done(&p, true)
	}
	if b.State() != Closed {
		t.Fatal("not closed")
	}
	for i := range late {
		b.Done(&late[i], false)
	}
	if b.State() != Closed {
		t.Fatal("stale failures reopened the breaker")
	}
	// A late probe outcome from an earlier half-open epoch is ignored too.
	run(t, b, 20, 20)
	clk.Advance(time.Second)
	var p Ticket
	b.Allow(&p)
	b.Done(&p, false) // reopen
	clk.Advance(time.Second)
	var p2 Ticket
	b.Allow(&p2)
	if b.Done(&p, true) {
		t.Fatal("second Done on a ticket counted")
	}
	if b.State() != HalfOpen {
		t.Fatalf("state %v, want half-open", b.State())
	}
}

func TestProbeDue(t *testing.T) {
	// 08 req 19: with no request probe for 1 s in half-open the driver
	// sends PING as the probe.
	b, clk, _ := newBreaker(t, time.Second)
	if b.ProbeDue() {
		t.Fatal("closed breaker wants a probe")
	}
	run(t, b, 20, 20)
	clk.Advance(time.Second)
	if b.ProbeDue() {
		t.Fatal("probe due as soon as half-open")
	}
	clk.Advance(time.Second)
	if !b.ProbeDue() {
		t.Fatal("no probe due after 1 s idle in half-open")
	}
	var p Ticket
	if !b.Allow(&p) {
		t.Fatal("PING probe not admitted")
	}
	if b.ProbeDue() {
		t.Fatal("probe due while one is in flight")
	}
	b.Done(&p, true)
	clk.Advance(999 * time.Millisecond)
	if b.ProbeDue() {
		t.Fatal("probe due before 1 s after the last probe")
	}
	clk.Advance(time.Millisecond)
	if !b.ProbeDue() {
		t.Fatal("no probe due 1 s after the last probe")
	}
}

func TestSaturatedBucketsKeepRatio(t *testing.T) {
	// Counters halve at saturation, so the failure share survives any call
	// rate: successes alone never open it, and failures open it once they
	// reach half of the counted calls.
	b, _, _ := newBreaker(t, time.Second)
	run(t, b, 3*countMax, 0)
	calls, fails := b.counts(b.period(b.clk.Now()), 0)
	if fails != 0 || calls < countMax/2 || calls > countMax {
		t.Fatalf("saturated counts %d failures of %d", fails, calls)
	}
	if b.State() != Closed {
		t.Fatal("successes opened the breaker")
	}
	for i := range countMax + 2 {
		var tk Ticket
		if !b.Allow(&tk) {
			t.Fatalf("not admitted after %d failures", i)
		}
		b.Done(&tk, false)
		if b.State() == Open {
			if i+1 < calls/2 {
				t.Fatalf("opened after %d failures against %d successes", i+1, calls)
			}
			return
		}
	}
	t.Fatal("failures never opened the saturated breaker")
}

func TestNewDefaults(t *testing.T) {
	clk := clocktest.New(start())
	b := New(clk, Config{OpenMax: time.Millisecond, CloseAfter: 50}, nil)
	def := DefaultConfig()
	if b.cfg.Window != def.Window || b.cfg.MinCalls != def.MinCalls || b.cfg.OpenMin != def.OpenMin ||
		b.cfg.OpenMax != def.OpenMax || b.cfg.CloseAfter != maxClose || b.cfg.ProbeIdle != def.ProbeIdle {
		t.Fatalf("config %+v", b.cfg)
	}
	for range 1000 {
		d := Uniform(time.Second, 3*time.Second)
		if d < time.Second || d > 3*time.Second {
			t.Fatalf("Uniform out of range: %v", d)
		}
	}
	if Uniform(time.Second, time.Second) != time.Second || Uniform(2, 1) != 2 {
		t.Fatal("Uniform with an empty range")
	}
	for _, s := range []State{Closed, Open, HalfOpen, 9} {
		if s.String() == "" {
			t.Fatalf("state %d has no name", s)
		}
	}
}

func BenchmarkBreakerClosed(b *testing.B) {
	br := New(clocktest.New(start()), DefaultConfig(), nil)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var tk Ticket
		for pb.Next() {
			if br.Allow(&tk) {
				br.Done(&tk, true)
			}
		}
	})
}
