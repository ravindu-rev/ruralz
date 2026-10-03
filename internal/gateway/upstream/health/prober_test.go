// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// probeCall is one recorded ProbeFunc call.
type probeCall struct {
	at    time.Duration // since epoch
	probe Probe
}

// probeScript answers probes from a per-Endpoint status table; a missing
// entry answers 200. block makes probes wait for ctx.
type probeScript struct {
	clk    *clocktest.Fake
	mu     sync.Mutex
	status map[string]int
	errs   map[string]error
	block  map[string]bool
	calls  []probeCall
	causes []error
}

func newScript(clk *clocktest.Fake) *probeScript {
	return &probeScript{clk: clk, status: map[string]int{}, errs: map[string]error{}, block: map[string]bool{}}
}

func (s *probeScript) probe(ctx context.Context, p Probe) (int, error) {
	s.mu.Lock()
	s.calls = append(s.calls, probeCall{s.clk.Now().Sub(epoch()), p})
	st, ok := s.status[p.Endpoint]
	err, block := s.errs[p.Endpoint], s.block[p.Endpoint]
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		s.mu.Lock()
		s.causes = append(s.causes, context.Cause(ctx))
		s.mu.Unlock()
		return 0, ctx.Err()
	}
	if !ok {
		st = 200
	}
	return st, err
}

func (s *probeScript) set(ep string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[ep] = status
}

func (s *probeScript) callsFor(ep string) []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []time.Duration
	for _, c := range s.calls {
		if c.probe.Endpoint == ep {
			out = append(out, c.at)
		}
	}
	return out
}

func (s *probeScript) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// proberHarness runs a Prober on a fake clock.
type proberHarness struct {
	p      *Prober
	clk    *clocktest.Fake
	status *fakeStatus
	cancel context.CancelFunc
	done   chan struct{}
}

func startProber(t *testing.T, clk *clocktest.Fake, cfg ProberConfig) *proberHarness {
	t.Helper()
	h := &proberHarness{clk: clk, status: &fakeStatus{}, done: make(chan struct{})}
	if cfg.Clock == nil {
		cfg.Clock = clk
	}
	cfg.Status = h.status
	h.p = NewProber(cfg)
	ctx, cancel := context.WithCancel(t.Context())
	h.cancel = cancel
	go func() {
		defer close(h.done)
		h.p.Run(ctx)
	}()
	t.Cleanup(h.stop)
	return h
}

func (h *proberHarness) stop() {
	h.cancel()
	<-h.done
}

// settle waits until Run blocks again after the last wake and no probe is
// in flight.
func (h *proberHarness) settle(t *testing.T) {
	t.Helper()
	h.wake(t, true)
}

// wake waits until Run blocks again after a poke, and for no probe in
// flight when quiet is set.
func (h *proberHarness) wake(t *testing.T, quiet bool) {
	t.Helper()
	w := h.p.waits.Load()
	h.p.poke()
	waitFor(t, "prober idle", func() bool { return h.p.waits.Load() > w && (!quiet || h.p.inUse.Load() == 0) })
}

// advance moves the fake clock in steps, letting Run catch up after each
// (and the probes it started finish, when quiet is set).
func (h *proberHarness) advance(t *testing.T, d, step time.Duration, quiet bool) {
	t.Helper()
	for d > 0 {
		s := min(d, step)
		h.clk.Advance(s)
		h.wake(t, quiet)
		d -= s
	}
}

// TestProbeSlots covers 05 req 20: slots = min(256, ceil(Σ probes per
// second × timeout)) shared across Upstreams, at least 1 while probing.
func TestProbeSlots(t *testing.T) {
	clk := clocktest.New(epoch())
	mk := func(name string, n int, a *ActivePolicy) *Tracker {
		tr := NewTracker(Config{Upstream: name, Clock: clk, Active: a})
		if err := tr.SetEndpoints(ids(n)); err != nil {
			t.Fatal(err)
		}
		return tr
	}
	tests := []struct {
		name  string
		ups   []*Tracker
		max   int
		slots int
	}{
		{"nothing attached", nil, 0, 0},
		{"no active policy", []*Tracker{mk("a", 10, nil)}, 0, 0},
		{"defaults: 10 × 2 s / 10 s", []*Tracker{mk("a", 10, &ActivePolicy{})}, 0, 2},
		{"at least one", []*Tracker{mk("a", 1, &ActivePolicy{})}, 0, 1},
		{
			"sum over Upstreams rounds up",
			[]*Tracker{
				mk("a", 10, &ActivePolicy{}),
				mk("b", 5, &ActivePolicy{Interval: time.Second, Timeout: 500 * time.Millisecond}),
			},
			0, 5,
		},
		{"capped at 256", []*Tracker{mk("a", 1280, &ActivePolicy{Interval: time.Second, Timeout: time.Second})}, 0, MaxProbeSlots},
		{"configured cap", []*Tracker{mk("a", 100, &ActivePolicy{})}, 7, 7},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProber(ProberConfig{Clock: clk, MaxSlots: tc.max, Rand: fixedSource(0)})
			for _, tr := range tc.ups {
				p.Attach(tr, func(context.Context, Probe) (int, error) { return 200, nil })
			}
			var h entryHeap
			p.sync(p.elapsed(), &h)
			if p.Slots() != tc.slots {
				t.Fatalf("slots %d, want %d", p.Slots(), tc.slots)
			}
			want := 0
			for _, tr := range tc.ups {
				if tr.Active() != nil {
					want += tr.Len()
				}
			}
			if len(h) != want {
				t.Fatalf("%d probes scheduled, want %d", len(h), want)
			}
		})
	}
}

// TestProbeThresholds covers 05 req 20: unhealthyThreshold consecutive
// failures mark unhealthy (counting an active ejection), healthyThreshold
// consecutive successes mark healthy, interleaved results restart the run,
// new Endpoints start healthy; active removals ignore the passive cap.
func TestProbeThresholds(t *testing.T) {
	tr, clk, m, _ := newTracker(t, 2, func(c *Config) { c.Active = &ActivePolicy{HealthyThreshold: 2, UnhealthyThreshold: 3} })
	now := clk.Now()
	down := func() bool { return tr.View(now).Down(0) }
	steps := []struct {
		ok   bool
		down bool
	}{
		{false, false}, {false, false}, {true, false}, // a success restarts the run
		{false, false}, {false, false}, {false, true}, // third consecutive failure
		{false, true}, {true, true}, {false, true}, {true, true}, {true, false},
	}
	for i, s := range steps {
		tr.RecordProbe("ep-000", s.ok, now)
		if down() != s.down {
			t.Fatalf("step %d: down %v, want %v", i, down(), s.down)
		}
	}
	if m.active.n.Load() != 1 {
		t.Fatalf("active ejections %d, want 1", m.active.n.Load())
	}
	// Both Endpoints unhealthy: active removals ignore the 50% cap.
	for range 3 {
		tr.RecordProbe("ep-000", false, now)
		tr.RecordProbe("ep-001", false, now)
	}
	if h, _ := tr.Healthy(now); h != 0 {
		t.Fatalf("healthy %d, want 0", h)
	}
	if tr.RecordProbe("ep-009", false, now) {
		t.Fatal("unknown identity recorded")
	}
	if err := tr.SetEndpoints([]string{"ep-000", "ep-001", "ep-002"}); err != nil {
		t.Fatal(err)
	}
	if tr.View(now).Down(2) {
		t.Fatal("new Endpoint did not start healthy")
	}
	tr.Close()
	if tr.RecordProbe("ep-002", false, now) {
		t.Fatal("closed Tracker recorded")
	}
}

// TestProberSchedule covers 05 req 20: the first probe of each Endpoint
// falls within its first interval, later ones interval ±10% apart; GET
// path with the policy's path; 200-399 succeed, other statuses and errors
// fail.
func TestProberSchedule(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rnd         fixedSource
		first, next time.Duration
	}{
		{"low end", 0, 0, 9 * time.Second},
		{"high end", fixedSource(^uint64(0)), 10 * time.Second, 11 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := clocktest.New(epoch())
			// Timeout = interval: 2 Endpoints × 10 s / 10 s = 2 slots, so the
			// simultaneous probes of this deterministic schedule all run.
			tr := NewTracker(Config{Upstream: "orders", Clock: clk, Active: &ActivePolicy{
				Path: "/healthz", Timeout: 10 * time.Second, UnhealthyThreshold: 1, HealthyThreshold: 1,
			}})
			if err := tr.SetEndpoints(ids(2)); err != nil {
				t.Fatal(err)
			}
			h := startProber(t, clk, ProberConfig{Rand: tc.rnd})
			s := newScript(clk)
			s.set("ep-001", 503)
			h.p.Attach(tr, s.probe)
			h.settle(t)
			h.advance(t, tc.first+3*tc.next, 500*time.Millisecond, true)
			want := []time.Duration{tc.first, tc.first + tc.next, tc.first + 2*tc.next, tc.first + 3*tc.next}
			for _, ep := range []string{"ep-000", "ep-001"} {
				if got := s.callsFor(ep); !slices.Equal(got, want) {
					t.Fatalf("%s probed at %v, want %v", ep, got, want)
				}
			}
			s.mu.Lock()
			call := s.calls[0].probe
			s.mu.Unlock()
			if call.Upstream != "orders" || call.Path != "/healthz" {
				t.Fatalf("probe %+v", call)
			}
			v := tr.View(clk.Now())
			if v.Down(0) || !v.Down(1) {
				t.Fatalf("verdicts: ep-000 down %v, ep-001 down %v", v.Down(0), v.Down(1))
			}
		})
	}
}

// TestProbeVerdicts covers the success rule of 05 req 20: a status from 200
// to 399 within timeout.
func TestProbeVerdicts(t *testing.T) {
	cases := []struct {
		status int
		err    error
		ok     bool
	}{
		{200, nil, true},
		{204, nil, true},
		{301, nil, true},
		{399, nil, true},
		{199, nil, false},
		{400, nil, false},
		{404, nil, false},
		{500, nil, false},
		{503, nil, false},
		{200, errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d/%v", tc.status, tc.err), func(t *testing.T) {
			clk := clocktest.New(epoch())
			tr := NewTracker(Config{Upstream: "u", Clock: clk, Active: &ActivePolicy{UnhealthyThreshold: 1}})
			if err := tr.SetEndpoints(ids(1)); err != nil {
				t.Fatal(err)
			}
			p := NewProber(ProberConfig{Clock: clk, Rand: fixedSource(0)})
			s := newScript(clk)
			s.set("ep-000", tc.status)
			s.errs["ep-000"] = tc.err
			a := &attachment{t: tr, probe: s.probe}
			p.probeOnce(t.Context(), a, tr.tab.Load().eps[0], *tr.Active())
			if down := tr.View(clk.Now()).Down(0); down == tc.ok {
				t.Fatalf("down %v, want ok %v", down, tc.ok)
			}
		})
	}
}

// TestProbeTimeout: a probe still running at the policy timeout is canceled
// with ErrProbeTimeout and fails (05 req 20).
func TestProbeTimeout(t *testing.T) {
	clk := clocktest.New(epoch())
	tr := NewTracker(Config{Upstream: "u", Clock: clk, Active: &ActivePolicy{Timeout: 2 * time.Second, UnhealthyThreshold: 1}})
	if err := tr.SetEndpoints(ids(1)); err != nil {
		t.Fatal(err)
	}
	h := startProber(t, clk, ProberConfig{Rand: fixedSource(0)})
	s := newScript(clk)
	s.block["ep-000"] = true
	h.p.Attach(tr, s.probe)
	waitFor(t, "probe start", func() bool { return s.count() == 1 && clk.Pending() >= 2 })
	clk.Advance(2*time.Second - time.Millisecond)
	if tr.View(clk.Now()).Down(0) {
		t.Fatal("failed before the timeout")
	}
	clk.Advance(time.Millisecond)
	waitFor(t, "timeout verdict", func() bool { return tr.View(clk.Now()).Down(0) })
	s.mu.Lock()
	cause := s.causes[0]
	s.mu.Unlock()
	if !errors.Is(cause, ErrProbeTimeout) {
		t.Fatalf("cause %v", cause)
	}
}

// TestProbesSkipped covers 05 req 20: a due probe that finds no slot is
// skipped and counted per Upstream; more than 10% skipped over the last
// minute raises probes_skipped, which clears as the window slides.
func TestProbesSkipped(t *testing.T) {
	clk := clocktest.New(epoch())
	m := newMetrics()
	// 2 Endpoints × 2 s / 10 s: one slot.
	tr := NewTracker(Config{Upstream: "orders", Clock: clk, Metrics: m.m, Active: &ActivePolicy{}})
	if err := tr.SetEndpoints(ids(2)); err != nil {
		t.Fatal(err)
	}
	h := startProber(t, clk, ProberConfig{Rand: fixedSource(0)})
	s := newScript(clk)
	s.block["ep-000"] = true
	s.block["ep-001"] = true
	h.p.Attach(tr, s.probe)
	// Both are due at once: one takes the slot, the other is skipped.
	waitFor(t, "first probes", func() bool { return h.p.probes.Load() == 1 && h.p.skips.Load() == 1 })
	if h.p.Slots() != 1 || m.skipped.n.Load() != 1 {
		t.Fatalf("slots %d, skipped counter %d", h.p.Slots(), m.skipped.n.Load())
	}
	waitFor(t, "degraded", h.p.Degraded)
	if !h.status.held(catalog.ReasonProbesSkipped, ProbesSkippedSource) {
		t.Fatal("probes_skipped not raised on the Node")
	}
	started, skipped := h.p.Stats()
	if started != 1 || skipped != 1 {
		t.Fatalf("stats %d/%d", started, skipped)
	}
	// Stop probing: the window drains and the reason clears within a
	// minute plus a second.
	h.p.Detach(tr)
	h.advance(t, 2*time.Second, time.Second, false) // the blocked probe times out
	h.advance(t, ProbesSkippedWindow, time.Second, true)
	if h.p.Degraded() || h.status.held(catalog.ReasonProbesSkipped, ProbesSkippedSource) {
		t.Fatal("probes_skipped not cleared after the window")
	}
	// Raised once and cleared once, under the Prober's own source.
	want := []string{
		fmt.Sprintf("%s/%s=true", catalog.ReasonProbesSkipped, ProbesSkippedSource),
		fmt.Sprintf("%s/%s=false", catalog.ReasonProbesSkipped, ProbesSkippedSource),
	}
	if got := h.status.list(); !slices.Equal(got, want) {
		t.Fatalf("degraded events %q, want %q", got, want)
	}
}

// TestSkipWindowThreshold: exactly 10% skipped is not degraded; more is.
func TestSkipWindowThreshold(t *testing.T) {
	p := NewProber(ProberConfig{Clock: clocktest.New(epoch())})
	now := epoch().UnixNano()
	for i := range 10 {
		p.win.add(now+int64(i)*int64(time.Second), i == 0)
	}
	last := now + int64(9*time.Second)
	p.evaluate(last)
	if p.Degraded() {
		t.Fatal("10% skipped raised the reason")
	}
	p.win.add(last, true)
	p.evaluate(last)
	if !p.Degraded() {
		t.Fatal("2 of 11 skipped did not raise the reason")
	}
	// Entries older than a minute leave the window.
	p.evaluate(last + int64(ProbesSkippedWindow))
	if p.Degraded() {
		t.Fatal("stale skips kept the reason")
	}
	if sk, tot := p.win.sums(last); tot != 11 || sk != 2 {
		t.Fatalf("window sums %d/%d, want 2/11", sk, tot)
	}
	// 59.999 s later only the newest second's bucket remains.
	if sk, tot := p.win.sums(last + int64(ProbesSkippedWindow) - 1); tot != 2 || sk != 1 {
		t.Fatalf("window sums %d/%d, want 1/2", sk, tot)
	}
}

// TestProberFollowsChanges: new Endpoints get probed, removed ones stop,
// a removed active policy stops probing, a closed Tracker is detached, and
// Run returns after waiting for probes in flight.
func TestProberFollowsChanges(t *testing.T) {
	clk := clocktest.New(epoch())
	tr := NewTracker(Config{Upstream: "u", Clock: clk, Active: &ActivePolicy{}})
	if err := tr.SetEndpoints([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	h := startProber(t, clk, ProberConfig{Rand: fixedSource(0)})
	s := newScript(clk)
	h.p.Attach(tr, s.probe)
	h.settle(t)
	if err := tr.SetEndpoints([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	h.settle(t)
	if len(s.callsFor("b")) != 1 {
		t.Fatal("new Endpoint not probed")
	}
	if err := tr.SetEndpoints([]string{"b"}); err != nil {
		t.Fatal(err)
	}
	h.advance(t, 20*time.Second, time.Second, true)
	if n := len(s.callsFor("a")); n != 1 {
		t.Fatalf("removed Endpoint probed %d times", n)
	}
	if n := len(s.callsFor("b")); n != 3 {
		t.Fatalf("Endpoint probed %d times in 20 s, want 3", n)
	}
	tr.SetPolicy(PassivePolicy{}, nil, 0)
	before := s.count()
	h.advance(t, 20*time.Second, time.Second, true)
	if s.count() != before || h.p.Slots() != 0 {
		t.Fatalf("probing without an active policy: %d calls, %d slots", s.count()-before, h.p.Slots())
	}
	tr.SetPolicy(PassivePolicy{}, &ActivePolicy{}, 0)
	h.settle(t)
	if s.count() != before+1 {
		t.Fatal("probing did not resume")
	}
	// Reattaching replaces the probe function.
	s2 := newScript(clk)
	h.p.Attach(tr, s2.probe)
	h.advance(t, 10*time.Second, time.Second, true)
	if s2.count() == 0 {
		t.Fatal("replacement probe function unused")
	}
	tr.Close()
	h.settle(t)
	h.p.mu.Lock()
	attached := len(h.p.atts)
	h.p.mu.Unlock()
	if attached != 0 {
		t.Fatal("closed Tracker still attached")
	}

	// Run waits for probes in flight, which see the cancellation.
	tr2 := NewTracker(Config{Upstream: "v", Clock: clk, Active: &ActivePolicy{}})
	if err := tr2.SetEndpoints([]string{"x"}); err != nil {
		t.Fatal(err)
	}
	s3 := newScript(clk)
	s3.block["x"] = true
	h.p.Attach(tr2, s3.probe)
	waitFor(t, "blocked probe", func() bool { return s3.count() == 1 })
	// A second Run returns at once.
	h.p.Run(t.Context())
	h.stop()
	s3.mu.Lock()
	causes := len(s3.causes)
	s3.mu.Unlock()
	if causes != 1 || h.p.inUse.Load() != 0 {
		t.Fatal("Run returned before its probes")
	}
	if tr2.View(clk.Now()).Down(0) {
		t.Fatal("a probe cut short by shutdown recorded a verdict")
	}
}

func TestJitterWindow(t *testing.T) {
	src := newRand(11)
	lo, hi := time.Hour, time.Duration(0)
	for range 5000 {
		d := jitter(src, 10*time.Second)
		if d < 9*time.Second || d > 11*time.Second {
			t.Fatalf("jitter %v outside 9-11 s", d)
		}
		lo, hi = min(lo, d), max(hi, d)
	}
	if lo > 9100*time.Millisecond || hi < 10900*time.Millisecond {
		t.Fatalf("jitter spread %v-%v", lo, hi)
	}
	if uniform(src, 0) != 0 {
		t.Fatal("uniform(0)")
	}
	p := NewProber(ProberConfig{})
	if p.clk == nil || p.rnd == nil || p.maxSlots != MaxProbeSlots {
		t.Fatal("prober defaults")
	}
	if a, b := p.rnd.Uint64(), p.rnd.Uint64(); a == b {
		t.Fatal("global source repeats")
	}
	var h entryHeap
	h.Push(entry{due: 1})
	if h.Len() != 1 || h.Pop().(entry).due != 1 { //nolint:forcetypeassert // test of the heap's own values.
		t.Fatal("heap")
	}
}

// TestProberRestart: a Prober whose Run stopped schedules every attached
// Endpoint again when Run starts once more (05 req 20 keeps probing
// across a restart of the scheduler).
func TestProberRestart(t *testing.T) {
	clk := clocktest.New(epoch())
	// Timeout = interval: two slots, so both first probes run at once.
	tr := NewTracker(Config{Upstream: "u", Clock: clk, Active: &ActivePolicy{Timeout: 10 * time.Second}})
	if err := tr.SetEndpoints([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	h := startProber(t, clk, ProberConfig{Rand: fixedSource(0)})
	s := newScript(clk)
	h.p.Attach(tr, s.probe)
	h.settle(t)
	if s.count() != 2 {
		t.Fatalf("%d probes before the restart, want 2", s.count())
	}
	h.stop()
	h2 := &proberHarness{p: h.p, clk: clk, status: h.status, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	h2.cancel = cancel
	go func() {
		defer close(h2.done)
		h2.p.Run(ctx)
	}()
	t.Cleanup(h2.stop)
	h2.settle(t)
	if s.count() != 4 {
		t.Fatalf("%d probes after the restart, want 4 (both Endpoints again)", s.count())
	}
}
