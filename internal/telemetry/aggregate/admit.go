// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// ErrShape is returned by Admit for a Shape no valid Revision produces.
var ErrShape = errors.New("aggregate: invalid shape")

// unit is one admission decision: the label sets of one family for one
// listener or resource (all its Phases for per-Phase families), admitted
// or folded together (spec 09 req 55).
type unit struct {
	fam      *family
	kind     role
	res      string
	phases   []int8
	codes    []string
	fan      int
	series   int
	admitted bool
	striped  bool
}

// normShape is a Shape with sorted, validated names.
type normShape struct {
	listeners []string
	routes    []string
	upstreams []string
	policies  []emit.PolicyShape
	// cached holds the Routes with a cache Policy (Shape.CachedRoutes).
	cached map[string]bool
}

// normalize sorts every list by name bytes, sorts and deduplicates each
// Policy's codes, and rejects empty, reserved and duplicate names and a
// cached Route that is not a Route, so any permutation of a Shape admits
// identically (spec 09 test 21). A cached Route may repeat (a Route with
// cache Policies at two scopes): it is marked once, as Policy codes are
// deduplicated.
func normalize(s emit.Shape) (normShape, error) {
	var n normShape
	var err error
	if n.listeners, err = sortedNames("listener", s.Listeners); err != nil {
		return n, err
	}
	if n.routes, err = sortedNames("Route", s.Routes); err != nil {
		return n, err
	}
	n.cached = make(map[string]bool, len(s.CachedRoutes))
	for _, name := range s.CachedRoutes {
		if _, ok := slices.BinarySearch(n.routes, name); !ok {
			return n, fmt.Errorf("%w: cached Route %q is not a Route", ErrShape, name)
		}
		n.cached[name] = true
	}
	if n.upstreams, err = sortedNames("Upstream", s.Upstreams); err != nil {
		return n, err
	}
	n.policies = make([]emit.PolicyShape, len(s.Policies))
	names := make([]string, len(s.Policies))
	for i, p := range s.Policies {
		p.Codes = slices.Compact(slices.Sorted(slices.Values(p.Codes)))
		n.policies[i] = p
		names[i] = p.Name
	}
	if _, err := sortedNames("Policy", names); err != nil {
		return n, err
	}
	slices.SortFunc(n.policies, func(a, b emit.PolicyShape) int { return cmp.Compare(a.Name, b.Name) })
	return n, nil
}

func sortedNames(kind string, in []string) ([]string, error) {
	out := slices.Sorted(slices.Values(in))
	for i, name := range out {
		switch {
		case name == "":
			return nil, fmt.Errorf("%w: empty %s name", ErrShape, kind)
		case strings.HasPrefix(name, "_"):
			return nil, fmt.Errorf("%w: %s name %q is reserved", ErrShape, kind, name)
		case i > 0 && out[i-1] == name:
			return nil, fmt.Errorf("%w: duplicate %s %q", ErrShape, kind, name)
		}
	}
	return out, nil
}

// budget tracks admission and striping counts while deciding one Plan.
type budget struct {
	lim            Limits
	famSets        []int
	famResources   []int
	revision       int
	listenerSeries int
	listenerHists  int
	policySeries   int
	policyHists    int
}

// decide computes the admission of a normalized shape: listener families
// first (never folded), then Policies, Routes and Upstreams in (kind
// bytes, name bytes) order, each resource's families in catalog order.
// It depends only on the shape and the limits.
func (r *Registry) decide(n normShape) []unit {
	b := &budget{
		lim:            r.limits,
		famSets:        make([]int, len(r.fams)),
		famResources:   make([]int, len(r.fams)),
		listenerSeries: r.nodeStripedSeries,
		listenerHists:  r.nodeStripedHists,
	}
	var units []unit
	for _, l := range n.listeners {
		for _, f := range r.fams {
			if f.role != roleListener {
				continue
			}
			u := unit{fam: f, kind: roleListener, res: l, phases: []int8{-1}, fan: f.n, admitted: true}
			u.series = u.fan * f.seriesPerSet()
			b.revision += u.series
			b.famSets[f.idx] += u.fan
			u.striped = b.stripeListener(f, u.fan)
			units = append(units, u)
		}
	}
	for _, p := range n.policies {
		for _, f := range r.fams {
			if f.role != rolePolicy || !policyApplies(f, p.Type) {
				continue
			}
			u := unit{fam: f, kind: rolePolicy, res: p.Name, phases: policyPhases(f, p)}
			if len(u.phases) == 0 {
				continue
			}
			if f.layout == layoutResultCode {
				u.codes = p.Codes
				u.fan = 1 + len(p.Codes)
			} else {
				u.fan = f.n * len(u.phases)
			}
			b.admit(&u)
			if u.admitted && p.Gateway {
				u.striped = b.stripePolicy(f, u.fan)
			}
			units = append(units, u)
		}
	}
	units = r.decideResources(b, units, roleRoute, n.routes, n.cached, b.lim.StripedRoutes)
	return r.decideResources(b, units, roleUpstream, n.upstreams, nil, b.lim.StripedUpstreams)
}

// decideResources appends the units of the Routes or Upstreams in names.
// Only the Routes in cached get ruralz_cache_requests_total label sets
// (spec 09 req 45): the others' cache handles stay no-ops and spend none
// of that family's budget.
func (r *Registry) decideResources(b *budget, units []unit, kind role, names []string, cached map[string]bool, stripedCap int) []unit {
	for _, name := range names {
		for _, f := range r.fams {
			if f.role != kind || (f.cat.Name == catalog.CacheRequestsTotal && !cached[name]) {
				continue
			}
			u := unit{fam: f, kind: kind, res: name, phases: []int8{-1}, fan: f.n}
			if f.layout == layoutAttempts {
				u.fan = numClasses + len(f.enums[1].Values) - 1
			}
			b.admit(&u)
			if u.admitted {
				u.striped = f.cat.Striped && b.famResources[f.idx] < stripedCap
				b.famResources[f.idx]++
			}
			units = append(units, u)
		}
	}
	return units
}

// admit decides one resource unit against the family and Revision limits.
func (b *budget) admit(u *unit) {
	f := u.fam
	u.series = u.fan * f.seriesPerSet()
	limit := b.lim.CounterFamily
	if f.hist {
		limit = b.lim.HistogramFamily
	}
	if b.famSets[f.idx]+u.fan <= limit && b.revision+u.series <= b.lim.Revision {
		u.admitted = true
		b.famSets[f.idx] += u.fan
		b.revision += u.series
	}
}

// stripeListener reports whether a listener unit fits the listener and
// enumeration-only striping caps, and counts it.
func (b *budget) stripeListener(f *family, fan int) bool {
	if !f.cat.Striped {
		return false
	}
	if f.hist {
		if b.listenerHists+fan > b.lim.ListenerStripedHistograms {
			return false
		}
		b.listenerHists += fan
		return true
	}
	if b.listenerSeries+fan > b.lim.ListenerStripedSeries {
		return false
	}
	b.listenerSeries += fan
	return true
}

// stripePolicy reports whether a Gateway-scoped Policy unit fits the
// Policy striping caps, and counts it.
func (b *budget) stripePolicy(f *family, fan int) bool {
	if !f.cat.Striped {
		return false
	}
	if f.hist {
		if b.policyHists+fan > b.lim.PolicyStripedHistograms {
			return false
		}
		b.policyHists += fan
		return true
	}
	if b.policySeries+fan > b.lim.PolicyStripedSeries {
		return false
	}
	b.policySeries += fan
	return true
}

// policyPhases returns the Phases of p that family f has label sets for;
// {-1} for families without a phase label.
func policyPhases(f *family, p emit.PolicyShape) []int8 {
	if f.phase == nil {
		return []int8{-1}
	}
	var out []int8
	for i := range f.phase {
		if p.Phases&(1<<uint(i)) != 0 && phaseApplies(f, i) {
			out = append(out, int8(i))
		}
	}
	return out
}

// Plan is the admission result of one Revision (emit.Plan): handles for
// every listener, Route, Upstream and Policy of its Shape, folded label
// sets pointing at _overflow. It is immutable once Admit returns.
type Plan struct {
	r         *Registry
	listeners map[string]*emit.ListenerMetrics
	routes    map[string]*emit.RouteMetrics
	upstreams map[string]*emit.UpstreamMetrics
	policies  map[string]*emit.PolicyMetrics
	entries   []*planEntry
	folded    []int
	binding   *Binding
}

// planEntry is one plan's reference to one group.
type planEntry struct {
	g   *group
	hs  handleSet
	pos int
}

// Admit implements emit.Meter: it plans handles for Shape s off the
// request path. Admission depends only on s (spec 09 req 55): existing
// label sets of the same names are reused so values survive Hot Reloads
// (req 47), new ones stay pending until Bind.
func (r *Registry) Admit(s emit.Shape) (emit.Plan, error) {
	n, err := normalize(s)
	if err != nil {
		return nil, err
	}
	units := r.decide(n)
	now := r.clk.Now()
	p := &Plan{
		r:         r,
		listeners: make(map[string]*emit.ListenerMetrics, len(n.listeners)),
		routes:    make(map[string]*emit.RouteMetrics, len(n.routes)+1),
		upstreams: make(map[string]*emit.UpstreamMetrics, len(n.upstreams)),
		policies:  make(map[string]*emit.PolicyMetrics, len(n.policies)),
		folded:    make([]int, len(r.fams)),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range n.listeners {
		p.listeners[l] = noopListenerMetrics()
	}
	for _, name := range n.routes {
		p.routes[name] = noopRouteMetrics()
	}
	for _, name := range n.upstreams {
		p.upstreams[name] = noopUpstreamMetrics()
	}
	for _, pol := range n.policies {
		p.policies[pol.Name] = noopPolicyMetrics()
	}
	p.routes[Unmatched] = r.unmatchedRoute(now)
	for i := range units {
		if f := units[i].fam; f.foldable() {
			r.overflowGroups(f, now)
		}
	}
	for i := range units {
		u := &units[i]
		for _, ph := range u.phases {
			var hs handleSet
			if u.admitted {
				hs = r.planGroup(p, u, ph, now)
			} else {
				hs = r.overflowGroup(u.fam, ph, now).shared
			}
			p.wire(u, ph, hs)
		}
		if !u.admitted {
			p.folded[u.fam.idx] += u.fan
		}
	}
	return p, nil
}

// planGroup returns handles for an admitted unit's group, reusing a live
// or retiring group of the same key (kept by name) or creating a pending
// one.
func (r *Registry) planGroup(p *Plan, u *unit, ph int8, now time.Time) handleSet {
	key := groupKey{res: u.res, phase: ph}
	g := u.fam.groups[key]
	if g == nil {
		g = r.newGroup(u.fam, groupSpec{key: key, striped: u.striped, codes: u.codes, start: now})
	}
	hs := newHandleSet(g)
	p.entries = append(p.entries, &planEntry{g: g, hs: hs})
	return hs
}

// overflowGroup returns the _overflow group of family f (and Phase ph),
// exported at 0 from the family's first admission (spec 09 req 45). It is
// never folded or released.
func (r *Registry) overflowGroup(f *family, ph int8, now time.Time) *group {
	if g := f.overflow[ph]; g != nil {
		return g
	}
	g := r.fixedGroup(f, groupKey{res: Overflow, phase: ph}, f.overflowStriped, now)
	f.overflow[ph] = g
	return g
}

// overflowGroups creates the _overflow label sets of family f at its first
// admission: every non-code enumeration, for each Phase a Policy of a
// per-Phase family can record (the label sets a folded Policy can reach).
func (r *Registry) overflowGroups(f *family, now time.Time) {
	for _, ph := range f.overflowPhases() {
		r.overflowGroup(f, ph, now)
	}
}

// unmatchedRoute returns the Route handles of requests no Route matched
// (route="_unmatched", spec 09 req 41); cache families never record them.
func (r *Registry) unmatchedRoute(now time.Time) *emit.RouteMetrics {
	m := noopRouteMetrics()
	for _, f := range r.fams {
		if !f.hasUnmatched() {
			continue
		}
		if f.unmatched == nil {
			f.unmatched = r.fixedGroup(f, groupKey{res: Unmatched, phase: -1}, f.unmatchedStriped, now)
		}
		if hs := f.unmatched.shared; f.hist {
			m.Duration = hs.hist(0)
		} else {
			m.Requests = hs.status()
		}
	}
	return m
}

// wire stores the handles of one group into the plan's metric structs.
func (p *Plan) wire(u *unit, ph int8, hs handleSet) {
	f := u.fam
	switch u.kind {
	case roleListener:
		wireListener(p.listeners[u.res], f.cat.Name, hs)
	case roleRoute:
		wireRoute(p.routes[u.res], f.cat.Name, hs)
	case roleUpstream:
		wireUpstream(p.upstreams[u.res], f.cat.Name, hs)
	case rolePolicy:
		wirePolicy(p.policies[u.res], f.cat.Name, ph, hs)
	case roleNode:
	}
}

// Listener implements emit.Plan; an unknown name records nothing.
func (p *Plan) Listener(name string) *emit.ListenerMetrics {
	if m := p.listeners[name]; m != nil {
		return m
	}
	return noopListenerMetrics()
}

// Route implements emit.Plan; Route(Unmatched) records route="_unmatched",
// an unknown name records nothing.
func (p *Plan) Route(name string) *emit.RouteMetrics {
	if m := p.routes[name]; m != nil {
		return m
	}
	return noopRouteMetrics()
}

// Upstream implements emit.Plan; an unknown name records nothing.
func (p *Plan) Upstream(name string) *emit.UpstreamMetrics {
	if m := p.upstreams[name]; m != nil {
		return m
	}
	return noopUpstreamMetrics()
}

// Policy implements emit.Plan; an unknown name records nothing.
func (p *Plan) Policy(name string) *emit.PolicyMetrics {
	if m := p.policies[name]; m != nil {
		return m
	}
	return noopPolicyMetrics()
}

// Folded returns the number of label sets of family name this Plan folded
// into _overflow at admission.
func (p *Plan) Folded(name string) int {
	if f := p.r.byName[name]; f != nil {
		return p.folded[f.idx]
	}
	return 0
}
