// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

func reqKey(route, class string) string {
	return fmt.Sprintf(`%s{route=%q,status_class=%q}`, catalog.HTTPRequestsTotal, route, class)
}

// seriesState returns ruralz_telemetry_series{state} from a collection.
func seriesState(t testing.TB, pts map[string]point) (live, retiring float64) {
	t.Helper()
	return mustGet(t, pts, catalog.TelemetrySeries+`{state="live"}`).value,
		mustGet(t, pts, catalog.TelemetrySeries+`{state="retiring"}`).value
}

// Spec 09 req 47: values live in aggregates kept by name across Hot
// Reloads; a label set admitted again keeps counting from its value and
// keeps its StartTime.
func TestKeptByNameAcrossReloads_Req47(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	p1, b1 := admitBind(t, r, emit.Shape{Routes: []string{"a"}})
	p1.Route("a").Requests.Inc(0, 200)
	p1.Route("a").Requests.Inc(1, 200)
	start := mustGet(t, collectNow(t, r, clk), reqKey("a", "2xx")).start

	clk.Advance(time.Minute)
	p2, b2 := admitBind(t, r, emit.Shape{Routes: []string{"a", "b"}})
	b1.Retire()
	p1.Route("a").Requests.Inc(2, 200) // a request pinned to the old snapshot
	b1.Release()
	p2.Route("a").Requests.Inc(3, 200)
	pts := collectNow(t, r, clk)
	if p := mustGet(t, pts, reqKey("a", "2xx")); p.value != 4 || !p.start.Equal(start) {
		t.Errorf("a = %v since %v, want 4 since %v", p.value, p.start, start)
	}
	if p := mustGet(t, pts, reqKey("b", "2xx")); p.value != 0 || !p.start.After(start) {
		t.Errorf("b = %+v", p)
	}
	b2.Release()
}

// Spec 09 req 56: a label set only retired snapshots reference is
// retiring; it is released with its last snapshot, and a returning name
// restarts from 0 with a new StartTime (req 51).
func TestRetiringAndRelease_Req56(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	p1, b1 := admitBind(t, r, emit.Shape{Routes: []string{"old"}})
	p1.Route("old").Requests.Inc(0, 500)
	pts := collectNow(t, r, clk)
	live0, ret0 := seriesState(t, pts)
	if ret0 != 0 {
		t.Fatalf("retiring = %v before any retirement", ret0)
	}
	oldStart := mustGet(t, pts, reqKey("old", "5xx")).start

	_, b2 := admitBind(t, r, emit.Shape{Routes: []string{"new"}})
	b1.Retire()
	b1.Retire() // idempotent
	pts = collectNow(t, r, clk)
	live1, ret1 := seriesState(t, pts)
	// Route families: 5 + 16 + 5 series per Route.
	if ret1 != 26 {
		t.Errorf("retiring = %v, want 26", ret1)
	}
	if live1 != live0 {
		t.Errorf("live = %v, want %v (one Route live)", live1, live0)
	}
	if got := mustGet(t, pts, reqKey("old", "5xx")).value; got != 1 {
		t.Errorf("retiring label set = %v, want 1 (still exported)", got)
	}

	b1.Release()
	b1.Release() // idempotent
	pts = collectNow(t, r, clk)
	if _, ok := pts[reqKey("old", "5xx")]; ok {
		t.Error("released label set still exported")
	}
	if _, ret := seriesState(t, pts); ret != 0 {
		t.Errorf("retiring after release = %v", ret)
	}

	clk.Advance(time.Hour)
	p3, b3 := admitBind(t, r, emit.Shape{Routes: []string{"new", "old"}})
	b2.Retire()
	b2.Release()
	p3.Route("old").Requests.Inc(0, 200)
	pts = collectNow(t, r, clk)
	p := mustGet(t, pts, reqKey("old", "5xx"))
	if p.value != 0 || !p.start.After(oldStart) {
		t.Errorf("returning name = %v since %v, want 0 since after %v", p.value, p.start, oldStart)
	}
	if got := mustGet(t, pts, reqKey("old", "2xx")).value; got != 1 {
		t.Errorf("returning name 2xx = %v", got)
	}
	b3.Release()
}

// Spec 09 req 56: beyond the retiring ceiling the oldest retiring sets
// fold; their series end, their handles record into _overflow, and the
// overflow series never rises by a folded set's accumulated value.
func TestRetiringCeilingFoldsOldest_Req56(t *testing.T) {
	// One Route's counter families: 5 + 5 series; histograms 16. Ceiling
	// 30 holds one retiring Route (26 series).
	r, clk := newTestRegistry(t, func(o *Options) { o.Limits = Limits{Retiring: 30} })
	var plans []*Plan
	var binds []emit.Binding
	for i := range 4 {
		p, b := admitBind(t, r, emit.Shape{Routes: []string{fmt.Sprintf("r%d", i)}})
		p.Route(fmt.Sprintf("r%d", i)).Requests.Inc(0, 200)
		p.Route(fmt.Sprintf("r%d", i)).Requests.Inc(0, 200)
		if i > 0 {
			binds[i-1].Retire() // pinned: never released
		}
		plans = append(plans, p)
		binds = append(binds, b)
	}
	pts := collectNow(t, r, clk)
	// r0 and r1 folded (oldest first); r2 retiring; r3 live.
	for _, name := range []string{"r0", "r1"} {
		if _, ok := pts[reqKey(name, "2xx")]; ok {
			t.Errorf("%s still exported after folding", name)
		}
	}
	if got := mustGet(t, pts, reqKey("r2", "2xx")).value; got != 2 {
		t.Errorf("r2 = %v", got)
	}
	if _, ret := seriesState(t, pts); ret > 30 {
		t.Errorf("retiring %v over the ceiling 30", ret)
	}
	if got := mustGet(t, pts, reqKey(Overflow, "2xx")).value; got != 0 {
		t.Errorf("overflow = %v, want 0: folding never carries values", got)
	}
	folded := mustGet(t, pts, fmt.Sprintf(`%s{instrument=%q}`, catalog.TelemetryFoldedLabelSets, catalog.HTTPRequestsTotal)).value
	if folded != 10 {
		t.Errorf("folded label sets = %v, want 10", folded)
	}
	// A request still pinned to r0's snapshot records into _overflow.
	plans[0].Route("r0").Requests.Inc(1, 200)
	plans[0].Route("r0").Duration.Record(1, 5)
	pts = collectNow(t, r, clk)
	if got := mustGet(t, pts, reqKey(Overflow, "2xx")).value; got != 1 {
		t.Errorf("overflow after a late record = %v, want 1", got)
	}
	if got := mustGet(t, pts, catalog.HTTPRequestDurationSeconds+`{route="_overflow"}`).count; got != 1 {
		t.Errorf("overflow duration count = %v", got)
	}
	// Releasing the folded sets' snapshots lowers the folded count.
	binds[0].Release()
	binds[1].Release()
	pts = collectNow(t, r, clk)
	if got := mustGet(t, pts, fmt.Sprintf(`%s{instrument=%q}`, catalog.TelemetryFoldedLabelSets, catalog.HTTPRequestsTotal)).value; got != 0 {
		t.Errorf("folded after release = %v", got)
	}
	// A folded name that returns starts from 0.
	p4, b4 := admitBind(t, r, emit.Shape{Routes: []string{"r0", "r3"}})
	binds[3].Retire()
	binds[3].Release()
	p4.Route("r0").Requests.Inc(0, 200)
	if got := mustGet(t, collectNow(t, r, clk), reqKey("r0", "2xx")).value; got != 1 {
		t.Errorf("returning folded name = %v, want 1", got)
	}
	binds[2].Release()
	b4.Release()
}

// Spec 09 req 56: when a Hot Reload moves a live label set past a limit,
// records after the fold go to _overflow while the old snapshot's handles
// keep the (now retiring) label set until it is released.
func TestLiveSetMovedPastLimit_Req56(t *testing.T) {
	r, clk := newTestRegistry(t, func(o *Options) { o.Limits = Limits{CounterFamily: 10} })
	p1, b1 := admitBind(t, r, emit.Shape{Routes: []string{"m", "z"}})
	p1.Route("z").Requests.Inc(0, 200)
	p2, b2 := admitBind(t, r, emit.Shape{Routes: []string{"a", "m", "z"}}) // z now folds
	b1.Retire()
	p1.Route("z").Requests.Inc(0, 200) // old snapshot: z's own set
	p2.Route("z").Requests.Inc(0, 200) // new snapshot: _overflow
	pts := collectNow(t, r, clk)
	if got := mustGet(t, pts, reqKey("z", "2xx")).value; got != 2 {
		t.Errorf("z = %v, want 2", got)
	}
	if got := mustGet(t, pts, reqKey(Overflow, "2xx")).value; got != 1 {
		t.Errorf("overflow = %v, want 1", got)
	}
	b1.Release()
	pts = collectNow(t, r, clk)
	if _, ok := pts[reqKey("z", "2xx")]; ok {
		t.Error("z still exported after its last snapshot was freed")
	}
	if got := mustGet(t, pts, reqKey(Overflow, "2xx")).value; got != 1 {
		t.Errorf("overflow = %v, want 1: folding never carries values", got)
	}
	b2.Release()
}

// Two plans admitted before either binds create one label set per name;
// a plan whose label set was released between Admit and Bind gets a
// fresh one.
func TestBindResolvesConcurrentPlans(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	pa, err := r.Admit(emit.Shape{Routes: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	pb, err := r.Admit(emit.Shape{Routes: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	ba := r.Bind(pa)
	bb := r.Bind(pb)
	if again := r.Bind(pa); again != ba {
		t.Error("second Bind returned a new Binding")
	}
	pa.Route("x").Requests.Inc(0, 200)
	pb.Route("x").Requests.Inc(1, 200)
	if got := mustGet(t, collectNow(t, r, clk), reqKey("x", "2xx")).value; got != 2 {
		t.Errorf("x = %v, want 2 (one label set)", got)
	}

	// pc references x while it is live; x is released before pc binds.
	pc, err := r.Admit(emit.Shape{Routes: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	ba.Release()
	bb.Release()
	if _, ok := collectNow(t, r, clk)[reqKey("x", "2xx")]; ok {
		t.Fatal("x exported after release")
	}
	bc := r.Bind(pc)
	pc.Route("x").Requests.Inc(0, 200)
	pts := collectNow(t, r, clk)
	if got := mustGet(t, pts, reqKey("x", "2xx")).value; got != 1 {
		t.Errorf("x after rebinding = %v, want 1 (restarted)", got)
	}
	bc.Release()

	// A plan never bound leaves nothing behind.
	if _, err := r.Admit(emit.Shape{Routes: []string{"ghost"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := collectNow(t, r, clk)[reqKey("ghost", "2xx")]; ok {
		t.Error("an unbound plan's label set is exported")
	}
}

// A Plan from another registry binds to a no-op Binding.
func TestBindForeignPlan(t *testing.T) {
	r1, _ := newTestRegistry(t, nil)
	r2, _ := newTestRegistry(t, nil)
	p, err := r1.Admit(emit.Shape{Routes: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	b := r2.Bind(p)
	if _, ok := b.(noopBinding); !ok {
		t.Fatalf("Bind of a foreign plan = %T", b)
	}
	b.Retire()
	b.Release()
}

// Listener label sets retire and release like resources but never fold.
func TestListenerRetireNeverFolds(t *testing.T) {
	r, clk := newTestRegistry(t, func(o *Options) { o.Limits = Limits{Retiring: 1} })
	_, b1 := admitBind(t, r, emit.Shape{Listeners: []string{"l1"}})
	_, b2 := admitBind(t, r, emit.Shape{Listeners: []string{"l2"}})
	b1.Retire()
	pts := collectNow(t, r, clk)
	if len(familySeries(pts, catalog.HTTPListenerRequestsTotal)) != 60 {
		t.Error("retiring listener label sets not exported")
	}
	if _, ret := seriesState(t, pts); ret == 0 {
		t.Error("retiring listener series not counted")
	}
	b1.Release()
	if n := len(familySeries(collectNow(t, r, clk), catalog.HTTPListenerRequestsTotal)); n != 30 {
		t.Errorf("listener series after release = %d, want 30", n)
	}
	b2.Release()
}

// Spec 09 test 22 (property): for random record streams interleaved with
// Hot Reloads that move label sets past the limits and the retiring
// ceiling, the family sum over exported series never increases by more
// than the records recorded since the previous collection.
func TestFoldingNeverCarriesValues_Test22(t *testing.T) {
	rng := testRand(22, 56)
	for trial := range 20 {
		r, clk := newTestRegistry(t, func(o *Options) {
			o.Limits = Limits{CounterFamily: 5 * (2 + rng.IntN(4)), HistogramFamily: 1 + rng.IntN(4), Retiring: 26 * (1 + rng.IntN(3))}
		})
		type snap struct {
			p     *Plan
			b     emit.Binding
			names []string
		}
		var live []snap
		recorded := 0
		prev := 0.0
		check := func(step string) {
			pts := collectNow(t, r, clk)
			sum := 0.0
			for _, p := range familySeries(pts, catalog.HTTPRequestsTotal) {
				sum += p.value
			}
			if sum-prev > float64(recorded)+1e-9 {
				t.Fatalf("trial %d %s: family sum rose by %v with %d records", trial, step, sum-prev, recorded)
			}
			if _, ret := seriesState(t, pts); ret > float64(r.limits.Retiring) {
				t.Fatalf("trial %d %s: retiring %v over ceiling %d", trial, step, ret, r.limits.Retiring)
			}
			prev, recorded = sum, 0
		}
		for step := range 60 {
			switch op := rng.IntN(5); {
			case op == 0 || len(live) == 0:
				var names []string
				for range 1 + rng.IntN(6) {
					names = append(names, fmt.Sprintf("r%d", rng.IntN(12)))
				}
				names = dedupe(names)
				pl, err := r.Admit(emit.Shape{Routes: names})
				if err != nil {
					t.Fatal(err)
				}
				b := r.Bind(pl)
				if n := len(live); n > 0 {
					live[n-1].b.Retire()
				}
				live = append(live, snap{p: pl.(*Plan), b: b, names: names})
			case op == 1 && len(live) > 1:
				i := rng.IntN(len(live) - 1)
				live[i].b.Release()
				live = append(live[:i], live[i+1:]...)
			default:
				s := live[rng.IntN(len(live))]
				for range 1 + rng.IntN(20) {
					s.p.Route(s.names[rng.IntN(len(s.names))]).Requests.Inc(stripe(rng.IntN(8)), 100+rng.IntN(500))
					recorded++
				}
			}
			if rng.IntN(3) == 0 {
				check(fmt.Sprintf("step %d", step))
			}
		}
		check("end")
		for _, s := range live {
			s.b.Release()
		}
		pts := collectNow(t, r, clk)
		for k := range familySeries(pts, catalog.HTTPRequestsTotal) {
			if !strings.Contains(k, Overflow) && !strings.Contains(k, Unmatched) {
				t.Fatalf("trial %d: %s exported after every snapshot was released", trial, k)
			}
		}
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Per-Phase Policy groups and auth code tables survive re-admission with
// different Phases or codes.
func TestPolicyReadmissionChangesShape(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	p1, b1 := admitBind(t, r, emit.Shape{Policies: []emit.PolicyShape{
		{Name: "jwt", Type: "auth.jwt", Phases: phases(phase.OnRequestHeaders), Codes: []string{"RZ-AUTH-001"}},
	}})
	p1.Policy("jwt").Auth.Deny(0, "RZ-AUTH-001")
	p2, b2 := admitBind(t, r, emit.Shape{Policies: []emit.PolicyShape{
		{Name: "jwt", Type: "auth.jwt", Phases: phases(phase.OnRequestHeaders, phase.OnResponse), Codes: []string{"RZ-AUTH-001", "RZ-AUTH-004"}},
	}})
	b1.Retire()
	b1.Release()
	p2.Policy("jwt").Auth.Deny(0, "RZ-AUTH-001")
	p2.Policy("jwt").Auth.Deny(0, "RZ-AUTH-004")
	p2.Policy("jwt").Duration[phase.OnResponse].Record(0, 10)
	pts := collectNow(t, r, clk)
	for k, v := range map[string]float64{
		catalog.AuthDecisionsTotal + `{code="RZ-AUTH-001",policy="jwt",result="deny"}`: 2,
		catalog.AuthDecisionsTotal + `{code="RZ-AUTH-004",policy="jwt",result="deny"}`: 1,
	} {
		if got := mustGet(t, pts, k).value; got != v {
			t.Errorf("%s = %v, want %v", k, got, v)
		}
	}
	if got := mustGet(t, pts, catalog.FilterDurationSeconds+`{phase="onResponse",policy="jwt"}`).count; got != 1 {
		t.Errorf("new Phase duration count = %d", got)
	}
	b2.Release()
}

// Spec 09 req 56 (unit scale of spec 09 test 41): Hot Reloads that rename
// every Route while K = 2 retired snapshots and one closing snapshot stay
// pinned keep the retiring series within the ceiling, fold only retiring
// sets, never raise _overflow by a folded set's value, and return to the
// single-Revision series count once the old snapshots are freed.
func TestReloadSequenceReturnsToSingleRevision_Req56(t *testing.T) {
	const routes, reloads, ceiling = 20, 60, 26 * 30
	r, clk := newTestRegistry(t, func(o *Options) { o.Limits = Limits{Retiring: ceiling} })
	shape := func(gen int) emit.Shape {
		s := emit.Shape{Listeners: []string{"public"}}
		for i := range routes {
			s.Routes = append(s.Routes, fmt.Sprintf("g%d-r%d", gen, i))
		}
		return s
	}
	var pinned []emit.Binding
	var plans []*Plan
	for gen := range reloads {
		p, b := admitBind(t, r, shape(gen))
		for _, name := range shape(gen).Routes {
			p.Route(name).Requests.Inc(0, 200)
		}
		if n := len(pinned); n > 0 {
			pinned[n-1].Retire()
		}
		pinned = append(pinned, b)
		plans = append(plans, p)
		// K = 2 retired plus one closing: the fourth-newest is freed.
		if len(pinned) > 4 {
			pinned[0].Release()
			pinned, plans = pinned[1:], plans[1:]
		}
		// A request pinned to the oldest snapshot records late.
		plans[0].Route(fmt.Sprintf("g%d-r0", gen-len(plans)+1)).Requests.Inc(1, 200)
		pts := collectNow(t, r, clk)
		if _, ret := seriesState(t, pts); ret > ceiling {
			t.Fatalf("reload %d: retiring %v over %d", gen, ret, ceiling)
		}
		if got := mustGet(t, pts, reqKey(Overflow, "2xx")).value; got > float64(gen+1) {
			t.Fatalf("reload %d: overflow %v rose by more than the late records", gen, got)
		}
		if n := len(familySeries(pts, catalog.ConfigRevisionInfo)); n > 2 {
			t.Fatalf("revision info series = %d", n)
		}
	}
	for _, b := range pinned[:len(pinned)-1] {
		b.Retire()
		b.Release()
	}
	live, ret := seriesState(t, collectNow(t, r, clk))
	fresh, fclk := newTestRegistry(t, nil)
	_, fb := admitBind(t, fresh, shape(reloads-1))
	defer fb.Release()
	want, _ := seriesState(t, collectNow(t, fresh, fclk))
	if ret != 0 || live != want {
		t.Errorf("after freeing old snapshots: live %v retiring %v, want live %v retiring 0", live, ret, want)
	}
}

// checkRetiringQueue verifies the retiring accounting against the groups
// and returns the number of retiring foldable groups: items before
// queueHead are cleared; every other item is cleared (counted in stale)
// or holds a retiring foldable group at its qpos; every retiring foldable
// group is queued once; retiringSeries is the sum of ceil over every
// retiring group; and the queue keeps at most queueSlack items besides
// those of the retiring foldable groups.
func checkRetiringQueue(t testing.TB, r *Registry) int {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	stale := 0
	for i, it := range r.queue {
		switch {
		case i < r.queueHead:
			if it.g != nil {
				t.Fatalf("queue[%d] before head %d holds a group", i, r.queueHead)
			}
		case it.g == nil:
			stale++
		case it.g.state != stRetiring || it.g.qpos != i || !it.g.foldable():
			t.Fatalf("queue[%d] holds %s/%s in state %d at qpos %d", i, it.g.fam.cat.Name, it.g.key.res, it.g.state, it.g.qpos)
		}
	}
	if stale != r.stale {
		t.Fatalf("stale = %d, counted %d", r.stale, stale)
	}
	series, foldable := 0, 0
	for _, f := range r.fams {
		for _, g := range f.groups {
			if g.state != stRetiring {
				if g.qpos != -1 {
					t.Fatalf("%s/%s in state %d has qpos %d", f.cat.Name, g.key.res, g.state, g.qpos)
				}
				continue
			}
			series += g.ceil
			if !g.foldable() {
				continue
			}
			foldable++
			if g.qpos < r.queueHead || g.qpos >= len(r.queue) || r.queue[g.qpos].g != g {
				t.Fatalf("retiring %s/%s not queued (qpos %d)", f.cat.Name, g.key.res, g.qpos)
			}
		}
	}
	if series != r.retiringSeries {
		t.Fatalf("retiringSeries = %d, retiring groups hold %d", r.retiringSeries, series)
	}
	if n := len(r.queue); n-r.queueHead > foldable+queueSlack || r.queueHead > queueSlack {
		t.Fatalf("queue holds %d items from head %d for %d retiring groups", n, r.queueHead, foldable)
	}
	return foldable
}

// Regression (WP-09 review) for spec 09 req 56 and the bounded-queue
// convention: Hot Reloads that rename some Routes while the retiring
// series stay far below the ceiling keep the retiring queue within the
// retiring groups plus queueSlack, and the groups released meanwhile are
// collectable (the queue keeps none reachable). Names come back while
// still retiring, so Bind also takes groups off the queue.
func TestRetiringQueueBounded_Req56(t *testing.T) {
	const routes, renamed, reloads = 40, 5, 400
	r, clk := newTestRegistry(t, nil)
	gen := make([]int, routes)
	shape := func() emit.Shape {
		var s emit.Shape
		for i, g := range gen {
			s.Routes = append(s.Routes, fmt.Sprintf("r%d-g%d", i, g))
		}
		return s
	}
	var released []weak.Pointer[group]
	pinned := []emit.Binding{}
	maxRetiring := 0
	for reload := range reloads {
		for k := range renamed {
			i := (reload*renamed + k) % routes
			gen[i] = (gen[i] + 1) % 3
		}
		pl, err := r.Admit(shape())
		if err != nil {
			t.Fatal(err)
		}
		b := r.Bind(pl)
		if n := len(pinned); n > 0 {
			pinned[n-1].Retire()
		}
		pinned = append(pinned, b)
		// K = 2 retired plus one closing: the fourth-newest is freed.
		if len(pinned) > 4 {
			old := pinned[0].(*Binding)
			if len(released) < 50 {
				for _, e := range old.p.entries {
					if e.g.refs == 1 && e.g.state == stRetiring {
						released = append(released, weak.Make(e.g))
					}
				}
			}
			old.Release()
			pinned[0] = nil
			pinned = pinned[1:]
		}
		maxRetiring = max(maxRetiring, checkRetiringQueue(t, r))
	}
	if maxRetiring == 0 || len(released) == 0 {
		t.Fatalf("no retiring groups (%d) or released groups (%d): the test exercises nothing", maxRetiring, len(released))
	}
	for _, b := range pinned[:len(pinned)-1] {
		b.Release()
	}
	pinned = pinned[len(pinned)-1:]
	if n := checkRetiringQueue(t, r); n != 0 {
		t.Errorf("retiring groups after freeing old snapshots = %d", n)
	}
	collectNow(t, r, clk) // rebuilds the exported group lists
	runtime.GC()
	runtime.GC()
	for i, w := range released {
		if w.Value() != nil {
			t.Fatalf("released group %d of %d still reachable", i, len(released))
		}
	}
	runtime.KeepAlive(pinned)
}

// listenerSeries returns the series one admitted listener exports.
func listenerSeries(r *Registry) int {
	n := 0
	for _, f := range r.fams {
		if f.role == roleListener {
			n += f.n * f.seriesPerSet()
		}
	}
	return n
}

// Spec 09 req 56 and test 41 (unit scale): the exported
// ruralz_telemetry_series{state="retiring"} stays within the ceiling
// while retiring listener label sets (which never fold) and auth decision
// code label sets beyond the declared codes count against it; resource
// label sets fold instead.
func TestExportedRetiringWithinCeiling_Req56(t *testing.T) {
	base, _ := newTestRegistry(t, nil)
	ceiling := 3*listenerSeries(base) + 200
	r, clk := newTestRegistry(t, func(o *Options) { o.Limits = Limits{Retiring: ceiling} })
	undeclared := []string{"RZ-AUTH-002", "RZ-AUTH-003", "RZ-AUTH-004", "RZ-AUTH-005", "RZ-AUTH-006"}
	var pinned []*Plan
	var binds []emit.Binding
	sawRetiring, sawFolded := false, false
	for gen := range 30 {
		shape := emit.Shape{
			Listeners: []string{fmt.Sprintf("l%d", gen)},
			Routes:    []string{fmt.Sprintf("a%d", gen), fmt.Sprintf("b%d", gen)},
			Policies: []emit.PolicyShape{{
				Name: fmt.Sprintf("jwt%d", gen), Type: "auth.jwt", Phases: phases(phase.OnRequestHeaders), Codes: []string{"RZ-AUTH-001"},
			}},
		}
		pl, b := admitBind(t, r, shape)
		if n := len(binds); n > 0 {
			binds[n-1].Retire()
		}
		pinned, binds = append(pinned, pl), append(binds, b)
		if len(binds) > 4 {
			binds[0].Release()
			pinned, binds = pinned[1:], binds[1:]
		}
		// Requests pinned to every snapshot record declared and
		// undeclared codes and listener requests.
		for k, p := range pinned {
			name := fmt.Sprintf("jwt%d", gen-len(pinned)+1+k)
			for _, c := range append([]string{"RZ-AUTH-001"}, undeclared...) {
				p.Policy(name).Auth.Deny(0, c)
			}
			p.Listener(fmt.Sprintf("l%d", gen-len(pinned)+1+k)).Requests.Inc(0, emit.ProtoHTTP1, 200, emit.OriginUpstream)
		}
		pts := collectNow(t, r, clk)
		_, ret := seriesState(t, pts)
		if ret > float64(ceiling) {
			t.Fatalf("reload %d: exported retiring %v over the ceiling %d", gen, ret, ceiling)
		}
		sawRetiring = sawRetiring || ret > 0
		folded := mustGet(t, pts, fmt.Sprintf(`%s{instrument=%q}`, catalog.TelemetryFoldedLabelSets, catalog.HTTPRequestsTotal)).value
		sawFolded = sawFolded || folded > 0
		checkRetiringQueue(t, r)
	}
	if !sawRetiring || !sawFolded {
		t.Errorf("retiring seen %v, folds seen %v: the ceiling was not exercised", sawRetiring, sawFolded)
	}
	for _, b := range binds {
		b.Retire()
		b.Release()
	}
}
