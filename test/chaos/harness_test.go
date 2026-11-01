// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/l4lb"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

var t0 = time.Unix(1_000_000, 0)

func at(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

// TestParseProfile checks the profile names: "" is reduced, unknown
// names fail.
func TestParseProfile(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		err      bool
	}{{"", Reduced, false}, {Reduced, Reduced, false}, {Nightly, Nightly, false}, {Target, Target, false}, {"huge", "", true}} {
		p, err := ParseProfile(tc.in)
		if (err != nil) != tc.err || p.Name != tc.want {
			t.Errorf("ParseProfile(%q) = %q, %v; want %q, error %v", tc.in, p.Name, err, tc.want, tc.err)
		}
	}
}

// TestProfiles checks each profile's Nodes, gates and runs
// (docs/engineering/03-testing-and-quality-strategy.md "Scale profiles").
func TestProfiles(t *testing.T) {
	r, _ := ParseProfile(Reduced)
	n, _ := ParseProfile(Nightly)
	g, _ := ParseProfile(Target)
	if r.Nodes != 3 || n.Nodes != 10 || g.Nodes != 10 || g.CE5Nodes != 100 || n.CE5Nodes != 10 {
		t.Errorf("Nodes: reduced %d, nightly %d/%d, target %d/%d", r.Nodes, n.Nodes, n.CE5Nodes, g.Nodes, g.CE5Nodes)
	}
	for _, k := range []Kind{Functional, Latency, CPU} {
		if r.Gates(k) {
			t.Errorf("reduced gates %s", k)
		}
		if n.Gates(k) != (k == Functional) {
			t.Errorf("nightly gates %s = %v", k, n.Gates(k))
		}
		if !g.Gates(k) {
			t.Errorf("target does not gate %s", k)
		}
	}
	for _, id := range []string{CE1, CE2, V3, RowUpstream, "CE-5/cluster"} {
		if !r.Runs(id) || !n.Runs(id) {
			t.Errorf("reduced or nightly does not run %s", id)
		}
	}
	for id, want := range map[string]bool{CE3: true, "CE-5/sentinel": true, CE12: true, "CE-12/cpu": true, CE15: true, GD2: true, CE1: false, V6: false, Saturation: true} {
		if g.Runs(id) != want {
			t.Errorf("target runs %s = %v, want %v", id, g.Runs(id), want)
		}
	}
	if n.HotRate(4000) != 4000 || g.HotRate(4000) != 1_000_000 || n.FloodRate(4000) != 4000 || n.FloodRate(2e5) != 1e5 || g.FloodRate(4000) != 1e5 {
		t.Error("hot or flood rates differ from the profile table")
	}
}

// TestZoneSplitAndBase checks GD-2's zone split and run-name bases.
func TestZoneSplitAndBase(t *testing.T) {
	if got := ZoneSplit(10, 3); !slices.Equal(got, []int{4, 3, 3}) {
		t.Errorf("ZoneSplit(10, 3) = %v", got)
	}
	if got := ZoneSplit(3, 0); got != nil {
		t.Errorf("ZoneSplit(3, 0) = %v", got)
	}
	if Base("CE-5/cluster") != CE5 || Base(CE1) != CE1 {
		t.Error("Base")
	}
}

// TestCheck checks every comparison, and that a value that is not a
// finite number fails.
func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		op   Op
		b    float64
		pass bool
	}{
		{1, LE, 1, true},
		{2, LE, 1, false},
		{0, LT, 1, true},
		{1, LT, 1, false},
		{1, GE, 1, true},
		{2, GT, 1, true},
		{1, GT, 1, false},
		{3, EQ, 3, true},
		{math.NaN(), LE, 1, false},
		{math.Inf(1), GE, 0, false},
	} {
		c := Check("c", Functional, tc.v, tc.op, tc.b)
		if (c.Verdict == VerdictPass) != tc.pass {
			t.Errorf("Check(%v %s %v) = %s", tc.v, tc.op, tc.b, c.Verdict)
		}
	}
	rec := Record{Criteria: []Criterion{Check("lat", Latency, 9, LE, 1), Check("fn", Functional, 9, LE, 1)}}
	n, _ := ParseProfile(Nightly)
	if f := rec.Failed(n); len(f) != 1 || f[0].Name != "fn" {
		t.Errorf("nightly Failed = %v, want only the functional criterion", f)
	}
}

// TestSaturationState checks the job's saturation S: unmeasured at
// first, a failed measurement failing every later run at once, and a
// measured or handed-down value winning over an earlier failure.
func TestSaturationState(t *testing.T) {
	h := &Harness{}
	if _, err := h.Saturation(); !errors.Is(err, ErrNoSaturation) {
		t.Fatalf("Saturation before a measurement = %v, want ErrNoSaturation", err)
	}
	failed := errors.New("ladder failed")
	h.FailSaturation(failed)
	if _, err := h.Saturation(); !errors.Is(err, failed) {
		t.Fatalf("Saturation after a failed measurement = %v, want that failure", err)
	}
	h.SetSaturation(1234)
	if s, err := h.Saturation(); err != nil || s != 1234 {
		t.Fatalf("Saturation after SetSaturation = %v, %v; want 1234", s, err)
	}
	h.FailSaturation(failed)
	if s, err := h.Saturation(); err != nil || s != 1234 {
		t.Errorf("a failure after a measurement replaced it: %v, %v", s, err)
	}
}

// TestSaturationSamplesNothing: the saturation measurement's run samples
// no /metrics, whose rounds would show in the steps' latency, and a run
// without a sampler stops its background work and records its
// breakdowns.
func TestSaturationSamplesNothing(t *testing.T) {
	if s := saturationSpec(); s.Name != Saturation || s.SampleEvery >= 0 {
		t.Errorf("saturationSpec() = %+v, want the %q run with a negative SampleEvery", s, Saturation)
	}
	r := &Run{rec: Record{Breakdowns: map[string]float64{}}}
	r.stopBackground()
	r.counterBreakdowns()
	if len(r.rec.Breakdowns) != 0 {
		t.Errorf("breakdowns of a run without a sampler or pump = %v", r.rec.Breakdowns)
	}
}

// fakeAdvancer is a controllable clock whose Advance can also move the
// real clock, as a slow Node's answer does.
type fakeAdvancer struct {
	now  time.Time
	real *clocktest.Fake
	lag  time.Duration
	adv  []time.Duration
}

func (f *fakeAdvancer) Advance(_ context.Context, d time.Duration) error {
	f.adv = append(f.adv, d)
	f.now = f.now.Add(d)
	f.real.Advance(f.lag)
	return nil
}

func (f *fakeAdvancer) Now() time.Time { return f.now }

// TestPumpCatchUp checks the clock pump: it follows real time one step
// at a time, never runs faster than real time when behind, adds jumps,
// and follows another caller's advance.
func TestPumpCatchUp(t *testing.T) {
	realClock := clocktest.New(t0)
	c := &fakeAdvancer{now: t0.Add(time.Hour), real: realClock}
	p := &Pump{c: c, real: realClock, step: 10 * time.Millisecond, offset: time.Hour, marks: []Mark{{Sent: t0, Answered: t0, Node: c.now}}}
	realClock.Advance(10 * time.Millisecond)
	if err := p.catchUp(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	// Behind by 100 ms: one step only, the rest dropped from the track.
	realClock.Advance(100 * time.Millisecond)
	if err := p.catchUp(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.adv, []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}) {
		t.Fatalf("advances %v, want two steps", c.adv)
	}
	if p.MaxBehind() != 100*time.Millisecond {
		t.Errorf("MaxBehind = %v", p.MaxBehind())
	}
	realClock.Advance(10 * time.Millisecond)
	if err := p.catchUp(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if got := c.adv[len(c.adv)-1]; got != 10*time.Millisecond {
		t.Errorf("after the drop the pump advanced %v, want one step", got)
	}
	if err := p.Jump(t.Context(), 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := c.adv[len(c.adv)-1]; got != 5*time.Second {
		t.Errorf("Jump advanced %v", got)
	}
	c.now = c.now.Add(time.Minute) // another caller's advance
	n := len(c.adv)
	if err := p.catchUp(t.Context(), 0); err != nil || len(c.adv) != n {
		t.Errorf("after another caller's advance the pump advanced again (%v)", err)
	}
	if err := p.Jump(t.Context(), -time.Second); err == nil {
		t.Error("a negative jump passed")
	}
}

// TestPumpWalk checks a walk through a timeline: steps of the given size
// until the condition holds, at most the maximum, each a jump the pump's
// track keeps.
func TestPumpWalk(t *testing.T) {
	realClock := clocktest.New(t0)
	c := &fakeAdvancer{now: t0, real: realClock}
	p := &Pump{c: c, real: realClock, step: 10 * time.Millisecond, marks: []Mark{{Sent: t0, Answered: t0, Node: t0}}}
	ok, err := p.Walk(t.Context(), time.Second, 0, 10*time.Second, func() bool { return len(c.adv) == 3 })
	if err != nil || !ok || !slices.Equal(c.adv, []time.Duration{time.Second, time.Second, time.Second}) {
		t.Fatalf("Walk until three steps = %v, %v with advances %v", ok, err, c.adv)
	}
	c.adv = nil
	ok, err = p.Walk(t.Context(), time.Second, 0, 5*time.Second, func() bool { return false })
	if err != nil || ok || len(c.adv) != 5 {
		t.Errorf("Walk to its maximum = %v, %v with %d advances, want false after 5", ok, err, len(c.adv))
	}
	if !c.now.Equal(t0.Add(8 * time.Second)) {
		t.Errorf("the Nodes' clock reads %v, want 8s after the start", c.now.Sub(t0))
	}
	if _, err := p.Walk(t.Context(), 0, 0, time.Second, func() bool { return false }); err == nil {
		t.Error("a walk with a zero step passed")
	}
}

// crossingAdvancer is a fakeAdvancer that keeps the real time at which
// its clock first reached a deadline.
type crossingAdvancer struct {
	fakeAdvancer
	deadline time.Time
	crossed  time.Time
}

func (f *crossingAdvancer) Advance(ctx context.Context, d time.Duration) error {
	if err := f.fakeAdvancer.Advance(ctx, d); err != nil {
		return err
	}
	if f.crossed.IsZero() && !f.now.Before(f.deadline) {
		f.crossed = f.real.Now()
	}
	return nil
}

// TestWalkLeavesConnectTimeoutRealTime checks the walk of a run under
// load: an Upstream dial's 1 s connectTimeout on the Nodes' clock, armed
// just before a walk starts, as a handover successor's first dials are,
// ends only after at least 150 ms of real time, where the harness's 1 s
// Drain steps ended it at the walk's first step and failed the request
// with RZ-UP-001.
func TestWalkLeavesConnectTimeoutRealTime(t *testing.T) {
	realClock := clocktest.New(t0)
	c := &crossingAdvancer{fakeAdvancer: fakeAdvancer{now: t0, real: realClock}, deadline: t0.Add(resilience.DefaultConnectTimeout)}
	p := &Pump{c: c, real: realClock, step: DefaultPumpStep, marks: []Mark{{Sent: t0, Answered: t0, Node: t0}}}
	ctx, stop := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() {
		defer stop()
		_, err := p.Walk(ctx, WalkStep, WalkStepWait, 2*resilience.DefaultConnectTimeout, func() bool { return !c.crossed.IsZero() })
		errc <- err
	}()
	for ctx.Err() == nil {
		if err := realClock.BlockUntil(ctx, 1); err != nil {
			break
		}
		realClock.Advance(WalkStepWait)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if c.crossed.IsZero() {
		t.Fatal("the walk never reached the dial's deadline")
	}
	if got := c.crossed.Sub(t0); got < 150*time.Millisecond {
		t.Errorf("the walk reached a 1 s connectTimeout after %v of real time, want at least 150ms", got)
	}
	// Each step adds a WalkStep to the real time the pump follows.
	if speed := float64(WalkStep+WalkStepWait) / float64(WalkStepWait); speed > 6 {
		t.Errorf("a walk runs Node time %.1f times as fast as real time, want at most 6", speed)
	}
}

// TestNodeSpans checks the mapping of real instants to Node times.
func TestNodeSpans(t *testing.T) {
	marks := []Mark{
		{Sent: at(0), Answered: at(0), Node: at(100)},
		{Sent: at(10), Answered: at(12), Node: at(110)},
		{Sent: at(20), Answered: at(25), Node: at(120)},
	}
	for _, tc := range []struct {
		real      int
		lo, hi    int
		causeLo   int
		ambiguous bool
	}{
		{5, 100, 100, 100, false},
		{11, 100, 110, 100, true},
		{15, 110, 110, 100, false},
		{22, 110, 120, 100, true},
		{30, 120, 120, 110, false},
	} {
		lo, hi, err := nodeSpanAt(marks, at(tc.real))
		if err != nil || !lo.Equal(at(tc.lo)) || !hi.Equal(at(tc.hi)) {
			t.Errorf("nodeSpanAt(%d) = %v, %v, %v; want %d..%d", tc.real, lo.Sub(t0), hi.Sub(t0), err, tc.lo, tc.hi)
		}
		clo, _, _ := causeSpanAt(marks, at(tc.real))
		if !clo.Equal(at(tc.causeLo)) {
			t.Errorf("causeSpanAt(%d) lo = %v, want %d", tc.real, clo.Sub(t0), tc.causeLo)
		}
		if _, amb, _ := nodeTimeAt(marks, at(tc.real)); amb != tc.ambiguous {
			t.Errorf("nodeTimeAt(%d) ambiguous = %v", tc.real, amb)
		}
	}
	if _, _, err := nodeTimeAt(marks, at(-1)); err == nil {
		t.Error("an instant before the pump mapped")
	}
}

// TestIncrease checks counter increases, a reset counting from 0.
func TestIncrease(t *testing.T) {
	tr := []Point{{Real: at(0), Value: 5}, {Real: at(10), Value: 8}, {Real: at(20), Value: 2}, {Real: at(30), Value: 4}, {Real: at(40), Value: 100}}
	if got := Increase(tr, at(0), at(30)); got != 3+2+2 {
		t.Errorf("Increase = %v, want 7", got)
	}
	if got := Increase(tr, at(5), at(10)); got != 3 {
		t.Errorf("Increase from the value before the window = %v, want 3", got)
	}
	if got := Increase(tr, at(-10), at(0)); got != 0 {
		t.Errorf("Increase without a value before the window = %v, want 0", got)
	}
	if r := Rises(tr); len(r) != 3 {
		t.Errorf("Rises = %v", r)
	}
}

// TestAdmissionBound checks the per-key window bound, a request whose
// readings straddle two windows going where it keeps the fuller one
// smallest.
func TestAdmissionBound(t *testing.T) {
	span := func(x time.Time) (time.Time, time.Time) { return x, x }
	var reqs []Request
	add := func(sent, done int) {
		reqs = append(reqs, Request{Key: "k", Intended: at(sent), Sent: at(sent), Done: at(done), Status: 200})
	}
	for range 3 {
		add(100, 200) // window 0
	}
	add(990, 1010) // straddles windows 0 and 1
	add(1100, 1200)
	worst, ok := AdmissionBound(reqs, Admitted, span, time.Second, at(0), at(5000), 3)
	if !ok || worst.Count != 3 {
		t.Errorf("AdmissionBound = %+v, %v; want 3 and holding", worst, ok)
	}
	add(1300, 1400)
	add(1400, 1500)
	if worst, ok = AdmissionBound(reqs, Admitted, span, time.Second, at(0), at(5000), 3); ok || worst.Count != 4 {
		t.Errorf("AdmissionBound = %+v, %v; want 4 and broken", worst, ok)
	}
}

// TestBucketLaw checks the summed local bucket law: full buckets admit
// their capacity at once and then their refill, an admission past both
// is an excess, and the widest decision interval widens the capacity so
// timing uncertainty never fails a run.
func TestBucketLaw(t *testing.T) {
	span := func(x time.Time) (time.Time, time.Time) { return x, x }
	var reqs []Request
	add := func(sent, done int) {
		reqs = append(reqs, Request{Intended: at(sent), Sent: at(sent), Done: at(done), Status: 200})
	}
	for range 3 {
		add(0, 0)
	}
	add(1000, 1000) // each second refills one token
	add(2000, 2000)
	excess, slack := BucketLaw(reqs, Admitted, span, 3, 1, at(0), at(5000))
	if excess != 0 || slack != 0 {
		t.Errorf("BucketLaw of a conforming run = %v, %v; want 0, 0", excess, slack)
	}
	add(2000, 2000)
	if excess, _ = BucketLaw(reqs, Admitted, span, 3, 1, at(0), at(5000)); excess != 1 {
		t.Errorf("BucketLaw with one admission past the law = %v, want 1", excess)
	}
	// The same admissions, one of them known only within 1 s: capacity
	// widens by the refill of that second.
	reqs[0].Done = at(1000)
	if excess, slack = BucketLaw(reqs, Admitted, span, 3, 1, at(0), at(5000)); excess != 0 || slack != time.Second {
		t.Errorf("BucketLaw with a 1 s decision interval = %v, %v; want 0, 1s", excess, slack)
	}
	if excess, _ = BucketLaw(reqs, Admitted, span, 3, 1, at(2000), at(5000)); excess != 0 {
		t.Errorf("BucketLaw of the last two admissions alone = %v, want 0", excess)
	}
	if excess, _ = BucketLaw(nil, Admitted, span, 3, 1, at(0), at(5000)); excess != 0 {
		t.Errorf("BucketLaw of no admission = %v", excess)
	}
}

// TestAnalyzeWaves checks CE-2's wave rules on synthetic closes.
func TestAnalyzeWaves(t *testing.T) {
	var closes []Span
	for i := range WaveCount {
		for range 13 {
			off := time.Duration(i)*JitterWindow/WaveCount + 600*time.Millisecond
			closes = append(closes, Span{Lo: off, Hi: off})
		}
	}
	rep := AnalyzeWaves(closes)
	if rep.Held != 104 || rep.Empty != 0 || rep.MaxSlice != 13 || rep.SliceBound != 26 || rep.LeftAtDeadline != 0 {
		t.Errorf("even waves: %+v", rep)
	}
	closes = append(closes[13:], Span{Lo: NeverClosed, Hi: NeverClosed}, Span{Lo: 21 * time.Second, Hi: 21 * time.Second})
	rep = AnalyzeWaves(closes)
	if rep.Empty != 1 || rep.LeftAtDeadline != 2 {
		t.Errorf("a missing wave and two late closes: %+v", rep)
	}
	if got := maxInWindow([]time.Duration{0, 500 * time.Millisecond, 999 * time.Millisecond, time.Second, 3 * time.Second}, time.Second); got != 3 {
		t.Errorf("maxInWindow = %d, want 3", got)
	}
}

// fakeConns is a ConnSource.
type fakeConns struct {
	open   []l4lb.ConnInfo
	closed map[string]l4lb.ConnInfo
}

func (f *fakeConns) Listener() string       { return "http" }
func (f *fakeConns) Conns() []l4lb.ConnInfo { return f.open }
func (f *fakeConns) ConnOf(c string) (l4lb.ConnInfo, bool) {
	ci, ok := f.closed[c]
	return ci, ok
}

// TestConnWatch checks that a watch keeps closed connections, refreshes
// their close time, and HeldAt selects those open at an instant.
func TestConnWatch(t *testing.T) {
	src := &fakeConns{open: []l4lb.ConnInfo{
		{ID: 1, Client: "a", Backend: "node-1", Opened: at(0)},
		{ID: 2, Client: "b", Backend: "node-2", Opened: at(0)},
	}, closed: map[string]l4lb.ConnInfo{}}
	w := &ConnWatch{srcs: []ConnSource{src}, seen: map[connKey]Conn{}}
	w.Poll()
	src.open = src.open[1:]
	src.closed["a"] = l4lb.ConnInfo{ID: 1, Client: "a", Backend: "node-1", Opened: at(0), Closed: at(500)}
	conns := w.Conns()
	if len(conns) != 2 || !conns[0].Closed.Equal(at(500)) {
		t.Fatalf("Conns = %+v", conns)
	}
	if h := HeldAt(conns, "node-1", at(400)); len(h) != 1 {
		t.Errorf("HeldAt before the close = %v", h)
	}
	if h := HeldAt(conns, "node-1", at(600)); len(h) != 0 {
		t.Errorf("HeldAt after the close = %v", h)
	}
	if open := w.Open(); open["node-2"] != 1 || open["node-1"] != 0 {
		t.Errorf("Open = %v", open)
	}
}

// TestConnWatchGone checks the close of a connection the balancer's
// history no longer gives, its client address reused by a newer one: the
// listing that stopped showing it bounds its close, which CloseSpan and
// HeldAt use, while a connection still listed stays open.
func TestConnWatchGone(t *testing.T) {
	src := &fakeConns{open: []l4lb.ConnInfo{
		{ID: 1, Client: "a", Backend: "node-1", Opened: at(0)},
		{ID: 2, Client: "b", Backend: "node-1", Opened: at(0)},
	}, closed: map[string]l4lb.ConnInfo{}}
	w := &ConnWatch{srcs: []ConnSource{src}, seen: map[connKey]Conn{}}
	w.Poll()
	before := time.Now()
	src.open = src.open[1:]
	src.closed["a"] = l4lb.ConnInfo{ID: 9, Client: "a", Backend: "node-2", Opened: at(100), Closed: at(200)}
	w.Poll()
	conns := w.Conns()
	if len(conns) != 2 {
		t.Fatalf("Conns = %+v", conns)
	}
	lo, hi, closed := conns[0].CloseSpan()
	if !closed || lo.After(before) || hi.Before(before) || !conns[0].Closed.IsZero() {
		t.Errorf("CloseSpan of the reused address = %v, %v, %v; want the listings around %v", lo, hi, closed, before)
	}
	if _, _, closed := conns[1].CloseSpan(); closed {
		t.Error("a listed connection has a close span")
	}
	if h := HeldAt(conns, "node-1", hi.Add(time.Millisecond)); len(h) != 1 || h[0].ID != 2 {
		t.Errorf("HeldAt after the gone listing = %+v, want only connection 2", h)
	}
	if h := HeldAt(conns, "node-1", lo.Add(-time.Millisecond)); len(h) != 2 {
		t.Errorf("HeldAt before the close = %+v, want both", h)
	}
}

// TestReplicaLag checks L from offset samples.
func TestReplicaLag(t *testing.T) {
	s := []OffsetSample{{At: at(0), Master: 100}, {At: at(100), Master: 200}, {At: at(200), Master: 300, Replica: 150}}
	if l, ok := ReplicaLag(s); !ok || l != 100*time.Millisecond {
		t.Errorf("ReplicaLag = %v, %v; want 100ms", l, ok)
	}
	s[2].Replica = 300
	if l, ok := ReplicaLag(s); !ok || l != 0 {
		t.Errorf("ReplicaLag in step = %v, %v", l, ok)
	}
	s[2].Replica = 50
	if _, ok := ReplicaLag(s); ok {
		t.Error("a replica behind every sample gave a lag")
	}
	if _, ok := ReplicaLag(nil); ok {
		t.Error("no samples gave a lag")
	}
}

// TestWithoutFieldAndOverlay checks the N-1 schema: the field is gone
// from its definition, and the overlay source parses as Go.
func TestWithoutFieldAndOverlay(t *testing.T) {
	doc := []byte(`{"$defs":{"RouteSpec":{"properties":{"timeout":{},"match":{}},"required":["match","timeout"]}}}`)
	out, err := WithoutField(doc, NMinusOneDef, NMinusOneField)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Defs map[string]struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	if _, ok := v.Defs["RouteSpec"].Properties["timeout"]; ok || !slices.Equal(v.Defs["RouteSpec"].Required, []string{"match"}) {
		t.Errorf("WithoutField = %s", out)
	}
	if _, err := WithoutField(out, NMinusOneDef, NMinusOneField); err == nil {
		t.Error("removing an absent field passed")
	}
	src := SchemaOverlay(map[string][]byte{"ruralz/v1alpha1/authoring.schema.json": out, "ruralz/v1alpha1/rendered.schema.json": out})
	if _, err := parser.ParseFile(token.NewFileSet(), "schema.go", src, 0); err != nil {
		t.Errorf("SchemaOverlay does not parse: %v", err)
	}
}

// TestRefusedForMemory checks how Fill reads a filler write: redisserver
// returns an error reply as a *ReplyError beside the reply, an OOM one
// meaning a full noeviction deployment refused the write, any other one
// or a transport error failing the fill.
func TestRefusedForMemory(t *testing.T) {
	oom := redisserver.Value{Kind: '-', Str: "OOM command not allowed when used memory > 'maxmemory'."}
	if refused, err := refusedForMemory(oom, oom.Err()); !refused || err != nil {
		t.Errorf("OOM reply: refused %v, %v; want refused", refused, err)
	}
	wrong := redisserver.Value{Kind: '-', Str: "WRONGTYPE Operation against a key holding the wrong kind of value"}
	if refused, err := refusedForMemory(wrong, wrong.Err()); refused || err == nil || !strings.Contains(err.Error(), "WRONGTYPE") {
		t.Errorf("WRONGTYPE reply: refused %v, %v; want an error naming it", refused, err)
	}
	cut := errors.New("connection reset")
	if refused, err := refusedForMemory(redisserver.Value{}, cut); refused || !errors.Is(err, cut) {
		t.Errorf("transport error: refused %v, %v; want that error", refused, err)
	}
	if refused, err := refusedForMemory(redisserver.Value{Kind: '+', Str: "OK"}, nil); refused || err != nil {
		t.Errorf("OK: refused %v, %v", refused, err)
	}
}

// TestCallsPerWindow checks the per-window count of a key's calls.
func TestCallsPerWindow(t *testing.T) {
	span := func(x time.Time) (time.Time, time.Time) { return x, x }
	tag := KeyTag(HotKey)
	key := "rz:rl:" + PolicyHot + ":10/1s:{" + tag + "}"
	calls := []Call{{At: at(100), Key: key}, {At: at(200), Key: key}, {At: at(1100), Key: key}, {At: at(150), Key: "rz:rl:other:10/1s:{00}"}}
	worst, total := CallsPerWindow(calls, tag, span, time.Second, at(0), at(5000))
	if worst != 2 || total != 3 {
		t.Errorf("CallsPerWindow = %d, %d; want 2, 3", worst, total)
	}
}

// TestPlanLimits checks that every key is offered at least twice its
// fail-open bound.
func TestPlanLimits(t *testing.T) {
	p := PlanLimits(1500, 3)
	perKey := p.Limited / float64(len(Consumers()))
	if float64(3*p.Ceiling)*2 > perKey || p.Requests != 3*p.Ceiling {
		t.Errorf("limited: ceiling %d, requests %d for %v per key", p.Ceiling, p.Requests, perKey)
	}
	if float64(3*p.UndeclaredRequests)*2 > p.Undeclared/float64(len(p.UndeclaredKeys)) {
		t.Errorf("undeclared: %d for %v", p.UndeclaredRequests, p.Undeclared)
	}
	if got := len(p.Streams()); got != 4 {
		t.Errorf("%d streams", got)
	}
}

// TestStreamKeepAttributesEveryRequest: a stream attributes every
// finished request to its Node as it ends, an answered one as well as a
// failed one, before a later connection from the same client address, as
// a stream opening a connection per request makes, takes the address
// over.
func TestStreamKeepAttributesEveryRequest(t *testing.T) {
	st := &Stream{Spec: StreamSpec{Name: "closed-fresh", DisableKeepAlives: true}}
	const local = "127.0.0.1:40000"
	carrier := "node-1"
	attr := func(_ bool, client string) string {
		if client != local {
			return ""
		}
		return carrier
	}
	st.keep(Request{Seq: 1, Outcome: loadgen.Success, Status: 200, Local: local}, attr)
	st.keep(Request{Seq: 2, Outcome: loadgen.Reset, Local: local}, attr)
	carrier = "node-2" // a later connection from the same address
	st.keep(Request{Seq: 3, Outcome: loadgen.Success, Status: 200, Local: local}, attr)
	st.keep(Request{Seq: 4, Outcome: loadgen.Success, Status: 200}, attr)
	want := []string{"node-1", "node-1", "node-2", ""}
	for i, q := range st.Requests() {
		if q.Node != want[i] {
			t.Errorf("request %d attributed to %q, want %q", q.Seq, q.Node, want[i])
		}
	}
}

// TestStreamAnalyses checks the code order, admissions after a code and
// phase statistics over a stream's requests.
func TestStreamAnalyses(t *testing.T) {
	st := &Stream{Spec: StreamSpec{Name: "closed", Counted: append(Decisions(), STSCodes()...)}}
	add := func(ms, status int, code string, outcome loadgen.Outcome) {
		st.requests = append(st.requests, Request{Intended: at(ms), Sent: at(ms), Done: at(ms + 1), Status: status, Code: code, Outcome: outcome})
	}
	add(0, 200, "", loadgen.Success)
	add(10, 503, CodeSTSTimeout, loadgen.ServerError)
	add(20, 503, CodeSTSOpen, loadgen.ServerError)
	add(30, 200, "", loadgen.Success)
	add(40, 0, "", loadgen.Reset)
	if got := CodeOrder(st, at(0), at(100), STSCodes()...); !slices.Equal(got, []string{CodeSTSTimeout, CodeSTSOpen}) {
		t.Errorf("CodeOrder = %v", got)
	}
	if got := AdmittedAfter(st, at(0), at(100), STSCodes()...); got != 1 {
		t.Errorf("AdmittedAfter = %d, want 1", got)
	}
	if got := CodesIn(st, at(0), at(100), CodeSTSOpen); got != 1 {
		t.Errorf("CodesIn = %d", got)
	}
	ps := PhaseStats(st, at(0), at(100))
	if ps.Offered != 5 || ps.Achieved != 4 || ps.Failed != 1 {
		t.Errorf("PhaseStats = %+v", ps)
	}
	l := &Load{Streams: []*Stream{st}}
	if got := ErrorsAfter(l, at(0), at(100), STSCodes()...); got != 3 {
		t.Errorf("ErrorsAfter = %d, want 3", got)
	}
	if got := Percentile([]time.Duration{4, 1, 3, 2}, 50); got != 2 {
		t.Errorf("Percentile = %v", got)
	}
}
