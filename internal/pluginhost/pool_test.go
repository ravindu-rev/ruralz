// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"context"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	ht "github.com/ravindu-rev/ruralz/internal/pluginhost/pluginhosttest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestPoolBounds pins the pool sizing of "Default limits": N from the
// share, the compute bound, the compute reserve R and the parked bound.
func TestPoolBounds(t *testing.T) {
	cases := []struct {
		name                        string
		cap, mem                    int64
		state                       bool
		procs, peak                 int
		n, compute, reserve, parked int
	}{
		{name: "a header Plugin at the defaults", cap: DefaultNodeCap, mem: DefaultMemoryBytes, procs: 8, n: 32, compute: 16},
		{name: "a state Plugin at the defaults, idle", cap: DefaultNodeCap, mem: DefaultMemoryBytes, state: true, procs: 8, n: 64, compute: 16, reserve: 2, parked: 62},
		{name: "a state Plugin at a 32-core peak of 1.5", cap: DefaultNodeCap, mem: DefaultMemoryBytes, state: true, procs: 32, peak: 1, n: 64, compute: 64, reserve: 2, parked: 62},
		{name: "a state Plugin at a peak of 12", cap: DefaultNodeCap, mem: DefaultMemoryBytes, state: true, procs: 8, peak: 12, n: 64, compute: 16, reserve: 16, parked: 48},
		{name: "a 64 MiB state Plugin", cap: DefaultNodeCap, mem: 64 << 20, state: true, procs: 8, n: 16, compute: 16, reserve: 2, parked: 14},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := instanceLimit(tc.cap, tc.mem, tc.state)
			b := boundsOf(n, tc.procs, tc.peak, tc.state)
			if b.n != tc.n || b.compute != tc.compute || b.reserve != tc.reserve || b.parked != tc.parked {
				t.Errorf("bounds = %+v, want n %d compute %d reserve %d parked %d", b, tc.n, tc.compute, tc.reserve, tc.parked)
			}
		})
	}
	if g := guaranteedParked(16); g != 12 {
		t.Errorf("guaranteedParked(16) = %d, want 12", g)
	}
	if instanceLimit(DefaultNodeCap, DefaultNodeCap, false) != 0 {
		t.Error("a limits.memoryBytes above the share fits an instance, want none")
	}
}

// TestOversizedMemoryRefused: a limits.memoryBytes above the pool share
// fails the Build with RZ-CFG-052.
func TestOversizedMemoryRefused(t *testing.T) {
	h := newHarness(t)
	p := headersPlugin(continueGuest())
	p.name, p.policy = "p", "pol"
	p.memBytes = 640 << 20
	_, err := h.build(h.bundleOf(p), "pol", buildOpts{})
	wantCode(t, err, CodeReservation)
}

// TestNoIdleInstance: with no idle instance within the 1 ms wait the call
// is RZ-PLG-005; after a fill the Node cap refused it is RZ-PLG-004.
func TestNoIdleInstance(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "no instance", true: "cap refused"}[refused], func(t *testing.T) {
			h := newHarness(t)
			f := h.one(headersPlugin(continueGuest()), buildOpts{})
			p := f.pool
			// Hold every compute slot, so the call waits.
			for range cap(p.compute) {
				<-p.compute
			}
			p.mu.Lock()
			p.capRefused = refused
			p.mu.Unlock()
			done := make(chan filter.Result, 1)
			go func() { done <- handle(f, phase.OnRequestHeaders, exchange(nil)) }()
			if err := h.clk.BlockUntilDue(t.Context(), h.clk.Now().Add(poolWait)); err != nil {
				t.Fatal(err)
			}
			h.clk.Advance(poolWait)
			want := CodeNoInstance
			if refused {
				want = CodeCapRefused
			}
			if r := <-done; codeOf(r) != want {
				t.Fatalf("Handle = %+v, want %s", r, want)
			}
			for range cap(p.compute) {
				p.compute <- struct{}{}
			}
		})
	}
}

// TestFaultBreaker: faults above half of at least 200 calls in 10 s trip
// the breaker for 5 s, every call meanwhile RZ-PLG-009 without reaching
// the guest, and plugin_pool_degraded stays raised; half-open lets every
// twentieth call through and closes after a clean window.
func TestFaultBreaker(t *testing.T) {
	clk := newHarness(t).clk
	var changes []bool
	b := newFaultBreaker(clk, func(open bool) { changes = append(changes, open) })
	for i := range breakerMinCalls {
		ok, _ := b.allow()
		if !ok {
			t.Fatalf("call %d refused before the trip", i)
		}
		b.record(i%2 == 0 || i < 2, false)
	}
	if !b.open() {
		t.Fatal("breaker closed after more than half of 200 calls faulted")
	}
	if ok, _ := b.allow(); ok {
		t.Fatal("a tripped breaker let a call through")
	}
	clk.Advance(breakerOpen)
	through := 0
	for range 100 {
		if ok, probe := b.allow(); ok {
			if !probe {
				t.Fatal("a half-open call was not a probe")
			}
			through++
			b.record(false, true)
		}
	}
	if through != 100*breakerProbePercent/100 {
		t.Errorf("half-open let %d of 100 calls through, want %d", through, 100*breakerProbePercent/100)
	}
	clk.Advance(breakerWindow)
	if b.open() {
		t.Fatal("breaker still open after a clean half-open window")
	}
	if len(changes) != 2 || !changes[0] || changes[1] {
		t.Errorf("changes = %v, want [true false]", changes)
	}
}

// TestBreakerShortCircuits: a pool whose breaker is open answers
// RZ-PLG-009 and raises plugin_pool_degraded.
func TestBreakerShortCircuits(t *testing.T) {
	h := newHarness(t)
	f := h.one(headersPlugin(continueGuest()), buildOpts{})
	f.pool.breaker.mu.Lock()
	f.pool.breaker.trip(h.clk.Now())
	f.pool.breaker.mu.Unlock()
	h.e.poolHealthChanged()
	if r := handle(f, phase.OnRequestHeaders, exchange(nil)); codeOf(r) != CodeBreakerOpen {
		t.Fatalf("Handle = %+v, want %s", r, CodeBreakerOpen)
	}
	if !h.status.raised(catalog.ReasonPluginPoolDegraded) {
		t.Error("plugin_pool_degraded not raised while the breaker is open")
	}
}

// TestRecycling: an instance is replaced after recycleAfter calls.
func TestRecycling(t *testing.T) {
	h := newHarness(t)
	f := h.one(headersPlugin(continueGuest()), buildOpts{})
	waitIdle(t, f.pool)
	inst := <-f.pool.idle
	inst.calls = recycleAfter - 1
	f.pool.idle <- inst
	drainOthers(f.pool, inst)
	if r := handle(f, phase.OnRequestHeaders, exchange(nil)); r.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v, want Continue", r)
	}
	if !inst.mod.IsClosed() {
		t.Error("an instance past recycleAfter calls stayed open")
	}
}

// drainOthers destroys every idle instance but keep.
func drainOthers(p *pool, keep *instance) {
	var held []*instance
	for {
		select {
		case i := <-p.idle:
			if i == keep {
				held = append(held, i)
				continue
			}
			p.destroy(context.Background(), i)
		default:
			for _, i := range held {
				p.idle <- i
			}
			return
		}
	}
}

// TestNodeCap: instances reserve their whole memory limit from the Node
// cap; a fill past it is refused and returns nothing reserved.
func TestNodeCap(t *testing.T) {
	h := newHarness(t)
	p := headersPlugin(continueGuest())
	p.capBytes = 64 << 20
	f := h.one(p, buildOpts{})
	waitIdle(t, f.pool)
	if got := h.e.Reserved(); got%DefaultMemoryBytes != 0 || got > 64<<20 {
		t.Errorf("reserved = %d, want whole 16 MiB instances within the 64 MiB cap", got)
	}
	if h.e.reserve(64<<20, false) {
		t.Error("a reservation past the cap was admitted")
	}
}

// TestSharedPool: Policies of equal digest, config and limits share a
// pool; another config gets its own.
func TestSharedPool(t *testing.T) {
	h := newHarness(t)
	bin := continueGuest()
	a := headersPlugin(bin)
	a.name, a.policy, a.config = "p", "a", map[string]any{"x": "1"}
	b := headersPlugin(bin)
	b.name, b.policy, b.config = "p", "b", map[string]any{"x": "1"}
	c := headersPlugin(bin)
	c.name, c.policy, c.config = "p", "c", map[string]any{"x": "2"}
	bundle := h.bundleOf(a, b, c)
	fa := h.mustBuild(bundle, "a", buildOpts{})
	fb := h.mustBuild(bundle, "b", buildOpts{})
	fc := h.mustBuild(bundle, "c", buildOpts{})
	if fa.pool != fb.pool {
		t.Error("equal configs got two pools")
	}
	if fa.pool == fc.pool {
		t.Error("different configs share a pool")
	}
	if h.src.fetches != 1 {
		t.Errorf("fetches = %d, want 1 per digest", h.src.fetches)
	}
}

// TestHotSwap: a Filter carried over keeps its pool; a replacement pool
// warms to its predecessor's peak and the predecessor trims; closing the
// last Filter closes the pool and releases the digest's code.
func TestHotSwap(t *testing.T) {
	h := newHarness(t)
	v1 := headersPlugin(continueGuest())
	f1 := h.one(v1, buildOpts{})
	old := f1.pool
	old.mu.Lock()
	old.peaks.note(h.clk.Now(), 0, 3)
	old.mu.Unlock()
	v2 := headersPlugin(simple(nil, nil, func(*ht.Guest) [][]byte { return [][]byte{ht.I32Const(0), ht.Op(ht.OpNop)} }))
	f2 := h.one(v2, buildOpts{previous: f1})
	if f2.pool == old {
		t.Fatal("a new digest reused the old pool")
	}
	if got := len(f2.pool.idle); got < 3+idleTarget(3) {
		t.Errorf("replacement pool warmed %d instances, want the predecessor's peak 3 plus %d", got, idleTarget(3))
	}
	if code := h.e.code; code != f1.pool.d.accounted+f2.pool.d.accounted {
		t.Errorf("accounted code = %d, want both digests'", code)
	}
	_ = f1.Close()
	if !old.closed {
		t.Error("the old pool stayed open after its last Filter closed")
	}
	if code := h.e.code; code != f2.pool.d.accounted {
		t.Errorf("accounted code after retire = %d, want the new digest's alone", code)
	}
}

// TestUnavailableArtifact: a fetch failure fails the Build with
// RZ-CFG-028; without a source it is transient.
func TestUnavailableArtifact(t *testing.T) {
	h := newHarness(t)
	h.e.source = nil
	_, err := h.build(h.bundleOf(pluginOpts{name: "p", policy: "pol", bin: continueGuest(), phases: []v1alpha1.Phase{v1alpha1.PhaseOnRequestHeaders}}), "pol", buildOpts{})
	wantCode(t, err, CodeArtifact)
	if !IsTransient(err) {
		t.Error("an unavailable artifact is deterministic, want transient")
	}
}

// TestConfigRejected: rz_configure refusing the config fails activation
// deterministically with RZ-CFG-051.
func TestConfigRejected(t *testing.T) {
	h := newHarness(t)
	g := ht.NewGuest()
	g.Configure(ht.I32Const(1))
	g.Phase("rz_on_request_headers", nil, ht.I32Const(0))
	_, err := h.build(h.bundleOf(pluginOpts{name: "p", policy: "pol", bin: g.Encode(), phases: []v1alpha1.Phase{v1alpha1.PhaseOnRequestHeaders}}), "pol", buildOpts{})
	wantCode(t, err, CodeConfigRejected)
	if IsTransient(err) {
		t.Error("a config rejection is transient, want deterministic")
	}
}

// TestConfigureMayCallOnlyLogClockRandomCrypto: a Host Function outside
// rz_configure's subset makes the configure fail.
func TestConfigureMayCallOnlyLogClockRandomCrypto(t *testing.T) {
	h := newHarness(t)
	g := ht.NewGuest("request_header_get", "clock_now")
	g.Configure(
		ht.Check(g.Call("clock_now", ht.I32Const(int32(ht.Scratch))), 0),
		g.Call("request_header_get", g.Str("a"), ht.I32Const(int32(ht.Scratch)), ht.I32Const(4)), ht.Op(ht.OpDrop),
		ht.I32Const(0))
	g.Phase("rz_on_request_headers", nil, ht.I32Const(0))
	p := headersPlugin(g.Encode(), v1alpha1.CapabilityRequestHeadersRead, v1alpha1.CapabilityClockRead)
	p.name, p.policy = "p", "pol"
	_, err := h.build(h.bundleOf(p), "pol", buildOpts{})
	wantCode(t, err, CodeConfigRejected)
}

// TestInstantiateDeadline: a start function past the 100 ms instantiate
// limit is a transient configure failure.
func TestInstantiateDeadline(t *testing.T) {
	h := newHarness(t)
	g := ht.NewGuest()
	spin := g.Func(ht.Sig(nil), nil, ht.Loop(ht.BlockEmpty), ht.Br(0), ht.End())
	g.M.Start(spin)
	g.Phase("rz_on_request_headers", nil, ht.I32Const(0))
	p := pluginOpts{name: "p", policy: "pol", bin: g.Encode(), phases: []v1alpha1.Phase{v1alpha1.PhaseOnRequestHeaders}}
	done := make(chan error, 1)
	go func() {
		_, err := h.build(h.bundleOf(p), "pol", buildOpts{})
		done <- err
	}()
	// The instantiate limit's timer, due at the fake time the build began,
	// arms once the compile ends.
	if err := h.clk.BlockUntilDue(t.Context(), h.clk.Now().Add(instantiateTimeout)); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(instantiateTimeout)
	err := <-done
	wantCode(t, err, CodeConfigRejected)
	if !IsTransient(err) {
		t.Error("an instantiate timeout is deterministic, want transient")
	}
}
