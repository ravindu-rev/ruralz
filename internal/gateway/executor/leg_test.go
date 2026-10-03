// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// Tests for LegHooks and LegRun over RequestState.Leg (R-42; spec 04 reqs
// 39, 42; spec 05 req 33; spec 07 reqs 6 and 16): per-leg when decisions,
// PolicyState slots and Finish at End, and concurrent legs sharing nothing
// mutable.

// legFilter stores its leg's identity in PolicyState at onUpstreamRequest
// and checks it in the later Phases and Finish (R-39: per-leg slots).
type legFilter struct {
	name string
	ev   *events
	t    testing.TB
}

func legID(x filter.Exchange) string { return x.Leg().Upstream + "/" + x.Leg().Step }

func (f *legFilter) Handle(_ context.Context, ph phase.Phase, x filter.Exchange) filter.Result {
	f.ev.add(fmt.Sprintf("%s.%s@%s#%d", f.name, ph, legID(x), x.Leg().Attempt))
	slot := x.PolicyState()
	switch {
	case *slot == nil:
		*slot = legID(x)
	case *slot != legID(x):
		f.t.Errorf("%s %s: slot holds %v on leg %s", f.name, ph, *slot, legID(x))
	}
	return filter.Next()
}

func (f *legFilter) Finish(_ context.Context, x filter.Exchange) {
	f.ev.add(f.name + ".finish@" + fmt.Sprint(*x.PolicyState()))
}

func legPolicies(ev *events, t testing.TB) []*snapshot.Policy {
	all := []phase.Phase{phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders, phase.OnUpstreamResponseBody}
	return []*snapshot.Policy{
		policy(spec{name: "oauth", class: phase.ClassUpstreamAuth, scope: phase.ScopeUpstream, phases: []phase.Phase{phase.OnUpstreamRequest}, f: &legFilter{"oauth", ev, t}}),
		policy(spec{name: "hdr", class: phase.ClassTransform, scope: phase.ScopeUpstream, phases: all, f: &legFilter{"hdr", ev, t}}),
		policy(spec{name: "tr", class: phase.ClassTransform, scope: phase.ScopeUpstream, phases: []phase.Phase{phase.OnUpstreamResponseBody}, f: &legFilter{"tr", ev, t}}),
	}
}

func TestLegPhasesAndFinishOrder(t *testing.T) {
	// Spec 04 req 39: onUpstreamRequest and onUpstreamResponseHeaders run per
	// attempt, onUpstreamResponseBody per leg; response Phases reversed;
	// End runs Finish for the leg's Policies in request order and releases
	// the leg state (R-40, R-42).
	h := newHarness(t)
	ch := chain(nil, map[string][]*snapshot.Policy{"orders": legPolicies(h.ev, t)})
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	l := r.BeginLeg(ctx, "orders", "")
	for attempt := 1; attempt <= 2; attempt++ {
		leg := &filter.Leg{Upstream: "orders", Attempt: attempt}
		if resp := l.OnUpstreamRequest(ctx, leg); resp != nil {
			t.Fatal(got(resp))
		}
		if rt, repl := l.OnUpstreamResponseHeaders(ctx, leg); rt || repl != nil {
			t.Fatal("unexpected retry or replacement")
		}
	}
	if repl := l.OnUpstreamResponseBody(ctx, &filter.Leg{Upstream: "orders", Attempt: 2}); repl != nil {
		t.Fatal(got(repl))
	}
	l.End(ctx)
	l.End(ctx) // a second End on the same value is ignored
	want := []string{
		"oauth.onUpstreamRequest@orders/#1", "hdr.onUpstreamRequest@orders/#1", "hdr.onUpstreamResponseHeaders@orders/#1",
		"oauth.onUpstreamRequest@orders/#2", "hdr.onUpstreamRequest@orders/#2", "hdr.onUpstreamResponseHeaders@orders/#2",
		"tr.onUpstreamResponseBody@orders/#2", "hdr.onUpstreamResponseBody@orders/#2",
		"oauth.finish@orders/", "hdr.finish@orders/",
	}
	evs := h.ev.list()
	// tr first runs in onUpstreamResponseBody; its Finish follows hdr's
	// (same class and scope, later position).
	want = append(want, "tr.finish@orders/")
	if !slices.Equal(evs, want) {
		t.Fatalf("events:\n got %v\nwant %v", evs, want)
	}
	if len(h.st.legs) != 1 || !h.st.legs[0].released {
		t.Fatal("the leg state was not released")
	}
	if h.st.enters != 0 {
		t.Fatal("leg calls entered the client state")
	}
}

func TestLegWhenOncePerLeg(t *testing.T) {
	// Spec 07 req 8: when is evaluated once per leg at Upstream scope, and a
	// false when skips the Policy for every attempt of that leg only.
	h := newHarness(t)
	prog := &constProgram{ok: false}
	p := policy(spec{
		name: "p", class: phase.ClassTransform, scope: phase.ScopeUpstream, when: prog,
		phases: []phase.Phase{phase.OnUpstreamRequest}, f: &fakeFilter{name: "p", ev: h.ev},
	})
	ch := chain(nil, map[string][]*snapshot.Policy{"up": {p}})
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	for legN := range 2 {
		l := r.BeginLeg(ctx, "up", fmt.Sprint("step", legN))
		for attempt := 1; attempt <= 3; attempt++ {
			l.OnUpstreamRequest(ctx, &filter.Leg{Upstream: "up", Attempt: attempt})
		}
		l.End(ctx)
	}
	if prog.n.Load() != 2 || len(h.ev.list()) != 0 {
		t.Fatalf("when evaluated %d times (want once per leg), calls %v", prog.n.Load(), h.ev.list())
	}
	if !h.st.legs[0].released || !h.st.legs[1].released || h.st.legs[1].step != "step1" {
		t.Fatal("leg states")
	}
}

func TestConcurrentLegsShareNothingMutable(t *testing.T) {
	// R-42: legs of an aggregate composition run concurrently, each with
	// its own LegRun and leg state (PolicyState slots, when decisions,
	// Leg); run under -race. The fake client state's Leg is not
	// synchronized, so the executor must serialize BeginLeg.
	h := newHarness(t)
	ch := chain(nil, map[string][]*snapshot.Policy{
		"a": legPolicies(h.ev, t),
		"b": legPolicies(h.ev, t),
	})
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	const legs = 16
	var wg sync.WaitGroup
	for i := range legs {
		wg.Go(func() {
			up := []string{"a", "b"}[i%2]
			step := fmt.Sprint("s", i)
			l := r.BeginLeg(ctx, up, step)
			for attempt := 1; attempt <= 2; attempt++ {
				leg := &filter.Leg{Upstream: up, Step: step, Attempt: attempt}
				if resp := l.OnUpstreamRequest(ctx, leg); resp != nil {
					t.Error(got(resp))
				}
				l.OnUpstreamResponseHeaders(ctx, leg)
			}
			l.OnUpstreamResponseBody(ctx, &filter.Leg{Upstream: up, Step: step, Attempt: 2})
			l.End(ctx)
		})
	}
	wg.Wait()
	if h.st.legCalls != legs || len(h.st.legs) != legs {
		t.Fatalf("leg states %d/%d", h.st.legCalls, len(h.st.legs))
	}
	for _, l := range h.st.legs {
		if !l.released {
			t.Fatal("a leg state was not released")
		}
	}
	// Every leg finished its own Policies on its own slots.
	finishes := 0
	for _, e := range h.ev.list() {
		if len(e) > 10 && e[:10] == "hdr.finish" {
			finishes++
		}
	}
	if finishes != legs {
		t.Fatalf("hdr finished %d times, want %d", finishes, legs)
	}
}

func TestBeginLegsForReplay(t *testing.T) {
	// R-43: the replay path runs one leg on a synthetic exchange with the
	// snapshot's LegChains and no client Phases.
	h := newHarness(t)
	legs := chain(nil, map[string][]*snapshot.Policy{"up": legPolicies(h.ev, t)}).Legs
	ctx := context.Background()
	r := h.e.BeginLegs(h.st, legs)
	defer r.Release()
	if r.Request(ctx, phase.OnRequestHeaders) != nil || r.Response(ctx, ResponseUpstream) != nil {
		t.Fatal("client Phases ran on a legs-only Run")
	}
	r.Log(ctx)
	l := r.BeginLeg(ctx, "up", "")
	l.OnUpstreamRequest(ctx, &filter.Leg{Upstream: "up", Attempt: 1})
	l.End(ctx)
	if n := len(h.ev.with("oauth.onUpstreamRequest")); n != 1 {
		t.Fatalf("replay leg calls %v", h.ev.list())
	}
}

func TestRequestOrder(t *testing.T) {
	a := &snapshot.Policy{Class: phase.ClassAuth, Scope: phase.ScopeRoute, Position: 3}
	b := &snapshot.Policy{Class: phase.ClassAuth, Scope: phase.ScopeGateway, Position: 9}
	c := &snapshot.Policy{Class: phase.ClassCORS, Scope: phase.ScopeUpstream, Position: 1}
	d := &snapshot.Policy{Class: phase.ClassAuth, Scope: phase.ScopeRoute, Position: 1}
	ps := []*snapshot.Policy{a, b, c, d}
	slices.SortStableFunc(ps, requestOrder)
	if !slices.Equal(ps, []*snapshot.Policy{c, b, d, a}) {
		t.Fatal("request order is class, scope, position (FP 8.12)")
	}
}
