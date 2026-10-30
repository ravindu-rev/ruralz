// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the Response Cache's hooks in the executor
// (docs/architecture/03-data-plane.md "Short-circuit";
// docs/architecture/09-traffic-management-and-resilience.md "Response
// caching"): a response the cache serves runs the whole client response
// chain, a stored response may stand in for the leg's before onResponse,
// and the invalidation runs whatever spec.when decided.

// cacheFilter is a fakeFilter that also implements filter.Substituter and
// filter.Invalidator, logging "<name>.substitute" and "<name>.invalidate".
type cacheFilter struct {
	fakeFilter
	sub *filter.Response
	// boom panics in Substitute and Invalidate.
	boom bool
}

func (f *cacheFilter) Substitute(context.Context, filter.Exchange) *filter.Response {
	f.ev.add(f.name + ".substitute")
	if f.st != nil && (f.st.cur == nil || f.st.cur.Name != f.name || f.st.ph != phase.OnResponse) {
		f.t.Errorf("%s: Substitute without Enter for onResponse", f.name)
	}
	if f.boom {
		panic("substitute exploded")
	}
	return f.sub
}

func (f *cacheFilter) Invalidate(context.Context, filter.Exchange) {
	f.ev.add(f.name + ".invalidate")
	if f.st != nil && (f.st.cur == nil || f.st.cur.Name != f.name) {
		f.t.Errorf("%s: Invalidate without Enter", f.name)
	}
	if f.boom {
		panic("invalidate exploded")
	}
}

// cachedResponse is a response the Response Cache serves.
func cachedResponse() *filter.Response {
	return &filter.Response{Status: 200, Body: []byte("stored"), Cached: true}
}

// responseChain is cors, cache, sem and tr subscribed to onResponse (and
// cache to onRequestHeaders), cache being c.
func responseChain(h *harness, c *cacheFilter) []*snapshot.Policy {
	on := []phase.Phase{phase.OnResponse}
	return []*snapshot.Policy{
		policy(spec{name: "cors", class: phase.ClassCORS, phases: on, f: h.filter("cors", nil)}),
		policy(spec{name: "cache", class: phase.ClassCache, typ: v1alpha1.PolicyTypeCache, phases: []phase.Phase{phase.OnRequestHeaders, phase.OnResponse}, f: c}),
		policy(spec{name: "sem", class: phase.ClassCache, typ: v1alpha1.PolicyTypeAISemanticCache, phases: on, f: h.filter("sem", nil)}),
		policy(spec{name: "tr", class: phase.ClassTransform, typ: v1alpha1.PolicyTypeTransformResponse, phases: on, f: h.filter("tr", nil)}),
	}
}

func newCacheFilter(h *harness, script map[phase.Phase]result) *cacheFilter {
	return &cacheFilter{fakeFilter: *h.filter("cache", script)}
}

func TestCachedShortCircuitRunsWholeChain(t *testing.T) {
	// A hit the cache serves in onRequestHeaders is no generated response:
	// every onResponse Policy runs on it, transform.response included, as
	// on an Upstream response, while a generated short-circuit skips
	// transform.response, cache and ai.semantic-cache.
	for _, tc := range []struct {
		name   string
		answer result
		want   []string
	}{
		{"cached hit", func(context.Context, filter.Exchange) filter.Result { return filter.Reply(cachedResponse()) },
			[]string{"cache.onRequestHeaders", "tr.onResponse", "sem.onResponse", "cache.onResponse", "cors.onResponse"}},
		{"generated short-circuit", respondWith(504, "RZ-RT-018"),
			[]string{"cache.onRequestHeaders", "cors.onResponse"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			c := newCacheFilter(h, map[phase.Phase]result{phase.OnRequestHeaders: tc.answer})
			r := h.e.Begin(h.st, chain(responseChain(h, c), nil), nil)
			defer r.Release()
			if resp := r.Request(context.Background(), phase.OnRequestHeaders); resp == nil {
				t.Fatal("no short-circuit")
			}
			if repl := r.Response(context.Background(), ResponseGenerated); repl != nil {
				t.Fatalf("replacement %s", got(repl))
			}
			if evs := h.ev.list(); !slices.Equal(evs, tc.want) {
				t.Fatalf("events %v, want %v", evs, tc.want)
			}
		})
	}
}

func TestSubstituteServesInPlaceOfTheLegResponse(t *testing.T) {
	// For an Upstream response or failure the cache may serve a stored
	// response in its place before onResponse (a 304's refreshed entry, a
	// stale-if-error answer): it replaces the response, and every
	// onResponse Policy then runs on it as on an Upstream response.
	for _, kind := range []ResponseKind{ResponseUpstream, ResponseUpstreamError} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			h := newHarness(t)
			c := newCacheFilter(h, nil)
			c.st, c.sub = h.st, cachedResponse()
			r := h.e.Begin(h.st, chain(responseChain(h, c), nil), nil)
			defer r.Release()
			repl := r.Response(context.Background(), kind)
			if repl != c.sub || h.st.x.Replaced != c.sub {
				t.Fatalf("replacement %v (exchange %v), want the substitute", repl, h.st.x.Replaced)
			}
			want := []string{"cache.substitute", "tr.onResponse", "sem.onResponse", "cache.onResponse", "cors.onResponse"}
			if evs := h.ev.list(); !slices.Equal(evs, want) {
				t.Fatalf("events %v, want %v", evs, want)
			}
		})
	}
}

func TestSubstituteNeverOnRequestPhaseResponses(t *testing.T) {
	// A response a request Phase produced, generated or served by the
	// cache itself, is never offered for substitution.
	for _, tc := range []struct {
		name   string
		answer result
	}{
		{"generated", respondWith(401, "RZ-AUTH-001")},
		{"cached", func(context.Context, filter.Exchange) filter.Result { return filter.Reply(cachedResponse()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			c := newCacheFilter(h, map[phase.Phase]result{phase.OnRequestHeaders: tc.answer})
			c.sub = cachedResponse()
			r := h.e.Begin(h.st, chain(responseChain(h, c), nil), nil)
			defer r.Release()
			r.Request(context.Background(), phase.OnRequestHeaders)
			r.Response(context.Background(), ResponseUpstream)
			if slices.Contains(h.ev.list(), "cache.substitute") {
				t.Fatalf("substitute offered a request Phase's response: %v", h.ev.list())
			}
		})
	}
}

func TestSubstituteRefusals(t *testing.T) {
	// No substitute: none offered, one that is not Cached or has no final
	// status, a panic (recovered), or a Policy spec.when skipped; the
	// Upstream response then runs every Policy unchanged.
	for _, tc := range []struct {
		name string
		sub  *filter.Response
		boom bool
		when *constProgram
	}{
		{"none", nil, false, nil},
		{"not cached", &filter.Response{Status: 200}, false, nil},
		{"no final status", &filter.Response{Status: 0, Cached: true}, false, nil},
		{"panic", cachedResponse(), true, nil},
		{"skipped by when", cachedResponse(), false, &constProgram{ok: false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			c := newCacheFilter(h, nil)
			c.sub, c.boom = tc.sub, tc.boom
			ps := responseChain(h, c)
			if tc.when != nil {
				ps[1].When = tc.when
			}
			r := h.e.Begin(h.st, chain(ps, nil), nil)
			defer r.Release()
			if repl := r.Response(context.Background(), ResponseUpstream); repl != nil || h.st.x.Replaced != nil {
				t.Fatalf("replacement %v (exchange %v), want none", repl, h.st.x.Replaced)
			}
			if !slices.Contains(h.ev.list(), "tr.onResponse") {
				t.Fatalf("the Upstream response did not run every Policy: %v", h.ev.list())
			}
		})
	}
}

func TestInvalidateIgnoresWhen(t *testing.T) {
	// The cache's invalidation runs after onLog and before Finish for
	// every client Policy implementing it, whatever spec.when decided and
	// whether or not the request reached it; an Upstream-scope one never
	// runs, and a panic is recovered.
	h := newHarness(t)
	skipped := newCacheFilter(h, nil)
	skipped.name = "skipped"
	unreached := newCacheFilter(h, nil)
	unreached.name, unreached.boom = "unreached", true
	leg := newCacheFilter(h, nil)
	leg.name = "leg"
	fin := &finishFilter{fakeFilter: *h.filter("fin", nil)}
	on := []phase.Phase{phase.OnRequestHeaders, phase.OnResponse}
	ps := []*snapshot.Policy{
		policy(spec{name: "skipped", class: phase.ClassCache, typ: v1alpha1.PolicyTypeCache, phases: on, f: skipped, when: &constProgram{ok: false}}),
		policy(spec{name: "fin", class: phase.ClassTransform, phases: []phase.Phase{phase.OnRequestHeaders}, f: fin}),
		policy(spec{name: "unreached", class: phase.ClassCache, typ: v1alpha1.PolicyTypeCache, phases: []phase.Phase{phase.OnResponse}, f: unreached}),
		policy(spec{name: "leg", class: phase.ClassCache, scope: phase.ScopeUpstream, phases: []phase.Phase{phase.OnUpstreamRequest}, f: leg}),
	}
	r := h.e.Begin(h.st, chain(ps, nil), nil)
	defer r.Release()
	r.Request(context.Background(), phase.OnRequestHeaders)
	r.Log(context.Background())
	want := []string{"fin.onRequestHeaders", "skipped.invalidate", "unreached.invalidate", "fin.finish"}
	if evs := h.ev.list(); !slices.Equal(evs, want) {
		t.Fatalf("events %v, want %v", evs, want)
	}
}
