// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// attachKey names one Policy attached at one scope. Phase selection
// depends on the Policy and the scope only, so it runs once per key
// however many Routes attach the Policy.
type attachKey struct {
	policy string
	scope  phase.Scope
}

// selection is the outcome of Phase selection for one attachKey.
type selection struct {
	// missing is set when the Policy, or the Plugin a plugin Policy names,
	// does not resolve: the attachment is skipped silently (RZ-CFG-009 is
	// stage H's, 02 req 40).
	missing bool
	// scopeErr is the registry's RZ-CFG-020 finding.
	scopeErr *registry.ScopeError
	// err is a programming error; Resolve returns the first one.
	err error
	// entry is the per-Policy part of every hub.Entry of this key.
	entry hub.Entry
	// res is the Policy resource, for the "declared in" location.
	res *hub.Resource
}

// attached is one valid entry of one spec.policies list.
type attached struct {
	entry hub.Entry
	res   *hub.Resource
}

// list is one checked spec.policies list: its valid entries in authored
// order, one per slot, with indexes by slot and Policy name.
type list struct {
	items  []attached
	bySlot map[string]int
	byName map[string]int
	diags  diag.List
}

// legTemplate is the upstream-leg chain of one Upstream; Routes share its
// slices.
type legTemplate struct {
	phases [phase.Count][]hub.Entry
	diags  diag.List
}

// routeResult is the outcome of one Route.
type routeResult struct {
	chain *hub.Chain
	diags diag.List
}

// state is the per-call working set of Resolve. Fields are written by the
// sequential steps and the index-owned worker slots, then only read.
type state struct {
	r *Resolver
	b *hub.Bundle

	gatewayRes *hub.Resource
	gatewayObj *v1alpha1.Gateway
	routes     []*hub.Resource

	keys []attachKey
	sels map[attachKey]*selection

	gateway       list
	upstreamOrder []string
	upstreams     map[string]*legTemplate
}

func newState(r *Resolver, b *hub.Bundle) *state {
	s := &state{r: r, b: b, sels: make(map[attachKey]*selection), upstreams: make(map[string]*legTemplate)}
	for _, res := range b.Resources() {
		switch res.Kind {
		case v1alpha1.KindGateway:
			if g, ok := hub.Object[v1alpha1.Gateway](res); ok && s.gatewayRes == nil {
				s.gatewayRes, s.gatewayObj = res, g
				s.addKeys(g.Spec.Policies, phase.ScopeGateway)
			}
		case v1alpha1.KindUpstream:
			if u, ok := hub.Object[v1alpha1.Upstream](res); ok {
				s.upstreamOrder = append(s.upstreamOrder, res.Name)
				s.addKeys(u.Spec.Policies, phase.ScopeUpstream)
			}
		case v1alpha1.KindRoute:
			if rt, ok := hub.Object[v1alpha1.Route](res); ok {
				s.routes = append(s.routes, res)
				s.addKeys(rt.Spec.Policies, phase.ScopeRoute)
			}
		default:
		}
	}
	return s
}

// addKeys records the attachKeys of refs in first-seen order.
func (s *state) addKeys(refs []v1alpha1.PolicyRef, scope phase.Scope) {
	for _, ref := range refs {
		k := attachKey{policy: ref.Name, scope: scope}
		if _, seen := s.sels[k]; !seen {
			s.sels[k] = nil
			s.keys = append(s.keys, k)
		}
	}
}

// selectAll runs Phase selection for every attachKey on the worker pool
// and returns the first programming error in key order.
func (s *state) selectAll(ctx context.Context, workers int) error {
	out := make([]selection, len(s.keys))
	if err := forEach(ctx, workers, len(s.keys), func(i int) { out[i] = s.selectOne(s.keys[i]) }); err != nil {
		return err
	}
	for i, k := range s.keys {
		if out[i].err != nil {
			return out[i].err
		}
		s.sels[k] = &out[i]
	}
	return nil
}

// selectOne resolves the Policy of k and asks the registry for its Phases
// at k's scope.
func (s *state) selectOne(k attachKey) selection {
	res, ok := s.b.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: k.policy})
	if !ok {
		return selection{missing: true}
	}
	p, ok := hub.Object[v1alpha1.Policy](res)
	if !ok {
		return selection{missing: true}
	}
	e, ok := s.r.reg.Lookup(p.Spec.Type)
	if !ok {
		return selection{err: fmt.Errorf("precedence: Policy %q: %w %q", k.policy, registry.ErrUnknownType, p.Spec.Type)}
	}
	var pluginPhases []v1alpha1.Phase
	if e.PhasesFromPlugin {
		pl, ok := s.b.Plugin(p.Spec.Plugin)
		if !ok {
			return selection{missing: true}
		}
		pluginPhases = pl.Spec.Phases
	}
	set, err := s.r.reg.Phases(registry.Attachment{
		Policy: p, Config: res.Config, Scope: k.scope, PluginPhases: pluginPhases,
	}, s.r.body)
	if err != nil {
		var se *registry.ScopeError
		if errors.As(err, &se) {
			return selection{scopeErr: se, res: res}
		}
		return selection{err: fmt.Errorf("precedence: Policy %q at %s scope: %w", k.policy, k.scope, err)}
	}
	return selection{res: res, entry: hub.Entry{
		Policy:      p.Metadata.Name,
		Type:        p.Spec.Type,
		Class:       e.EffectiveClass(p),
		Slot:        e.EffectiveSlot(p),
		Scope:       k.scope,
		FailureMode: e.EffectiveFailureMode(p),
		Overridable: p.Spec.Overridable == nil || *p.Spec.Overridable,
		Phases:      set,
	}}
}

// attachList checks one spec.policies list of owner at scope: RZ-CFG-020
// for a disallowed scope and RZ-CFG-018 for a slot already taken in the
// list (02 req 35.3 and 35.4). Both drop the entry from the chain, so one
// mistake yields one finding.
func (s *state) attachList(owner *hub.Resource, scope phase.Scope, refs []v1alpha1.PolicyRef) list {
	l := list{bySlot: make(map[string]int, len(refs)), byName: make(map[string]int, len(refs))}
	for i, ref := range refs {
		sel := s.sels[attachKey{policy: ref.Name, scope: scope}]
		if sel == nil || sel.missing {
			continue
		}
		loc := s.r.loc.entry(owner, fieldPolicies, i, ref.Name)
		path := entryPath(fieldPolicies, ref.Name)
		if sel.scopeErr != nil {
			d := sel.scopeErr.Diagnostic(path)
			d.Resource, d.Location = owner.ResourceID(), loc
			l.diags = append(l.diags, d)
			continue
		}
		e := sel.entry
		e.Position, e.Ref = i, loc
		if j, dup := l.bySlot[e.Slot]; dup {
			l.diags = append(l.diags, diag.Diagnostic{
				Code: CodeSlot, Severity: diag.SeverityError, Location: loc, Resource: owner.ResourceID(), Path: path,
				Message: fmt.Sprintf("Policies %q and %q share slot %q at %s scope", l.items[j].entry.Policy, e.Policy, e.Slot, scope),
			})
			continue
		}
		l.bySlot[e.Slot] = len(l.items)
		if _, seen := l.byName[e.Policy]; !seen {
			l.byName[e.Policy] = len(l.items)
		}
		l.items = append(l.items, attached{entry: e, res: sel.res})
	}
	return l
}

// attachGateway checks the Gateway list once.
func (s *state) attachGateway() list {
	if s.gatewayObj == nil {
		return list{}
	}
	return s.attachList(s.gatewayRes, phase.ScopeGateway, s.gatewayObj.Spec.Policies)
}

// attachUpstreams checks every Upstream list once and builds its leg.
func (s *state) attachUpstreams(ctx context.Context, workers int) error {
	ress := make([]*hub.Resource, len(s.upstreamOrder))
	for i, name := range s.upstreamOrder {
		ress[i], _ = s.b.Get(hub.ID{Kind: v1alpha1.KindUpstream, Name: name})
	}
	legs := make([]legTemplate, len(ress))
	err := forEach(ctx, workers, len(ress), func(i int) {
		u, _ := hub.Object[v1alpha1.Upstream](ress[i])
		l := s.attachList(ress[i], phase.ScopeUpstream, u.Spec.Policies)
		legs[i].diags = l.diags
		entries := make([]hub.Entry, len(l.items))
		for j, it := range l.items {
			entries[j] = it.entry
		}
		fill(&legs[i].phases, entries)
	})
	if err != nil {
		return err
	}
	for i, name := range s.upstreamOrder {
		s.upstreams[name] = &legs[i]
	}
	return nil
}

// removal is the per-Route fate of one Gateway list entry.
type removal struct {
	reason hub.RemovalReason
	by     string
}

// route computes the chain of one Route (02 req 35, 39, 41).
func (s *state) route(res *hub.Resource) routeResult {
	rt, _ := hub.Object[v1alpha1.Route](res)
	c := &hub.Chain{Route: res.Name}
	var ds diag.List
	gw := s.gateway.items
	fate := make([]removal, len(gw))

	// Rule 1: Gateway policies minus excludePolicies.
	for i, ex := range rt.Spec.ExcludePolicies {
		j, ok := s.gateway.byName[ex.Name]
		if !ok || fate[j].reason != 0 {
			continue
		}
		if g := gw[j]; !g.entry.Overridable {
			ds = append(ds, s.notOverridable(res, s.r.loc.entry(res, fieldExclude, i, ex.Name), entryPath(fieldExclude, ex.Name), g,
				"cannot exclude a Policy with overridable: false"))
			continue
		}
		fate[j] = removal{reason: hub.Excluded}
	}

	// Rule 2: Route policies replace same-slot Gateway policies.
	own := s.attachList(res, phase.ScopeRoute, rt.Spec.Policies)
	ds = append(ds, own.diags...)
	kept := own.items[:0:0]
	for _, it := range own.items {
		if j, ok := s.gateway.bySlot[it.entry.Slot]; ok && fate[j].reason == 0 {
			g := gw[j]
			if !g.entry.Overridable {
				ds = append(ds, s.notOverridable(res, it.entry.Ref, entryPath(fieldPolicies, it.entry.Policy), g,
					fmt.Sprintf("cannot replace Gateway Policy %q in slot %q: it has overridable: false", g.entry.Policy, g.entry.Slot)))
				continue
			}
			fate[j] = removal{reason: hub.Replaced, by: it.entry.Policy}
			it.entry.Replaces = g.entry.Policy
		}
		kept = append(kept, it)
	}

	// Client leg: class, scope, position; response Phases reversed.
	entries := make([]hub.Entry, 0, len(gw)+len(kept))
	for j, g := range gw {
		if fate[j].reason == 0 {
			entries = append(entries, g.entry)
		}
	}
	for _, it := range kept {
		entries = append(entries, it.entry)
	}
	fill(&c.Client, entries)
	ds = append(ds, s.cacheGuardrail(res, c, entries)...)

	for _, name := range legOrder(rt) {
		if t, ok := s.upstreams[name]; ok {
			c.Legs = append(c.Legs, hub.Leg{Upstream: name, Phases: t.phases})
		}
	}
	for _, reason := range []hub.RemovalReason{hub.Replaced, hub.Excluded} {
		for j, g := range gw {
			if fate[j].reason == reason {
				c.Removed = append(c.Removed, hub.Removed{Policy: g.entry.Policy, Slot: g.entry.Slot, By: fate[j].by, Reason: reason})
			}
		}
	}
	return routeResult{chain: c, diags: ds}
}

// notOverridable returns the RZ-CFG-019 finding at path of the Route res,
// with the position where the Gateway Policy g declares overridable: false
// as a related "declared in" location when it is known.
func (s *state) notOverridable(res *hub.Resource, at diag.Location, path diag.Path, g attached, msg string) diag.Diagnostic {
	d := diag.Diagnostic{
		Code: CodeOverridable, Severity: diag.SeverityError, Location: at, Resource: res.ResourceID(), Path: path, Message: msg,
	}
	if decl := s.r.loc.path(g.res, diag.Path{diag.Field("spec"), diag.Field("overridable")}, true); decl.File != "" {
		d.Related = []diag.Related{{Location: decl, Message: "declared in"}}
	}
	return d
}

// cacheGuardrail returns RZ-CFG-038 at each cache attachment of a chain
// that also runs an authz or validation Policy, or a plugin auth or authz
// Policy, in onRequestBody: a cache hit skips onRequestBody, so it would
// bypass them (02 req 39, OQ-traffic-management-and-resilience-21 (a)).
func (s *state) cacheGuardrail(res *hub.Resource, c *hub.Chain, entries []hub.Entry) diag.List {
	i := slices.IndexFunc(c.Client[phase.OnRequestBody], bypassedByCache)
	if i < 0 {
		return nil
	}
	x := c.Client[phase.OnRequestBody][i]
	var out diag.List
	for _, e := range entries {
		if e.Type != v1alpha1.PolicyTypeCache {
			continue
		}
		owner := res
		if e.Scope == phase.ScopeGateway {
			owner = s.gatewayRes
		}
		out = append(out, diag.Diagnostic{
			Code: CodeCacheGuardrail, Severity: diag.SeverityError, Location: e.Ref, Resource: owner.ResourceID(),
			Path: entryPath(fieldPolicies, e.Policy),
			Message: fmt.Sprintf("Response Cache Policy %q is combined with Policy %q (%s, Filter class %s), which runs in onRequestBody: a cache hit would skip it",
				e.Policy, x.Policy, x.Type, x.Class),
		})
	}
	return out
}

// bypassedByCache reports whether an onRequestBody entry may not share a
// chain with a cache Policy.
func bypassedByCache(e hub.Entry) bool {
	switch e.Class {
	case phase.ClassAuthz, phase.ClassValidation:
		return true
	case phase.ClassAuth:
		return e.Type == v1alpha1.PolicyTypePlugin
	default:
		return false
	}
}

// legOrder returns the Upstream names of rt in leg order (02 req 41):
// spec.upstreams by name, as stage G's map sorting leaves them, then
// spec.composition.steps in authored order; each name once.
func legOrder(rt *v1alpha1.Route) []string {
	names := make([]string, 0, len(rt.Spec.Upstreams))
	for _, u := range rt.Spec.Upstreams {
		names = append(names, u.Name)
	}
	slices.Sort(names)
	if rt.Spec.Composition != nil {
		for _, st := range rt.Spec.Composition.Steps {
			names = append(names, st.Upstream)
		}
	}
	seen := make(map[string]struct{}, len(names))
	out := names[:0]
	for _, n := range names {
		if _, dup := seen[n]; !dup {
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}

// fill puts every entry into each Phase it runs in, all Phases sharing one
// exactly sized backing array (a Phase without entries stays nil), then
// orders them.
func fill(phases *[phase.Count][]hub.Entry, entries []hub.Entry) {
	var counts [phase.Count]int
	total := 0
	for _, e := range entries {
		for p := range phase.Count {
			if e.Phases.Has(p) {
				counts[p]++
				total++
			}
		}
	}
	if total == 0 {
		return
	}
	buf := make([]hub.Entry, total)
	off := 0
	for p := range phase.Count {
		if n := counts[p]; n > 0 {
			phases[p] = buf[off : off : off+n]
			off += n
		}
	}
	for _, e := range entries {
		for p := range phase.Count {
			if e.Phases.Has(p) {
				phases[p] = append(phases[p], e)
			}
		}
	}
	order(phases)
}

// order sorts every Phase by class, scope and position (foundation pack
// 8.12 rule 5) and reverses the response Phases; onLog keeps request
// order.
func order(phases *[phase.Count][]hub.Entry) {
	for p := range phase.Count {
		l := phases[p]
		if len(l) < 2 {
			continue
		}
		slices.SortStableFunc(l, compareEntries)
		if p.IsResponse() {
			slices.Reverse(l)
		}
	}
}

// compareEntries orders entries by Filter class, scope, then position.
func compareEntries(a, b hub.Entry) int {
	return cmp.Or(cmp.Compare(a.Class, b.Class), cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.Position, b.Position))
}

// List fields that attach or exclude Policies.
const (
	fieldPolicies = "policies"
	fieldExclude  = "excludePolicies"
)

// entryPath returns spec.<field>[name=<name>].
func entryPath(field, name string) diag.Path {
	return diag.Path{diag.Field("spec"), diag.Field(field), diag.Keyed("name", name)}
}
