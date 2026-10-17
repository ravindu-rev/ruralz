// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The reference model of stage I for FuzzPrecedenceInvariants. It restates
// the resolution rules of foundation pack section 8.12 (02 req 35 to 41)
// in the most direct form, with linear searches and no shared state, and
// takes only Phase selection, slots and classes from the registry (whose
// own tests cover them). Comparing Resolve with it checks every law for
// completeness, not only soundness: a finding Resolve drops, or a chain
// entry it loses, differs from the model.

// modelItem is one valid attachment of one spec.policies list.
type modelItem struct {
	name, slot string
	typ        v1alpha1.PolicyType
	class      phase.Class
	scope      phase.Scope
	pos        int
	set        phase.Set
	locked     bool
	// replaces is the Gateway Policy a Route item replaces.
	replaces string
}

// key renders the item the way entryKey renders a hub.Entry.
func (it modelItem) key() string {
	return fmt.Sprintf("%s@%s#%d slot=%s replaces=%s", it.name, it.scope, it.pos, it.slot, it.replaces)
}

// entryKey renders the fields of e the model predicts.
func entryKey(e hub.Entry) string {
	return fmt.Sprintf("%s@%s#%d slot=%s replaces=%s", e.Policy, e.Scope, e.Position, e.Slot, e.Replaces)
}

// modelLeg is one expected upstream leg.
type modelLeg struct {
	upstream string
	phases   [phase.Count][]string
}

// modelChain is the expected chain of one Route.
type modelChain struct {
	client  [phase.Count][]string
	legs    []modelLeg
	removed []hub.Removed
}

// precedenceModel is the expected result of Run for one Bundle.
type precedenceModel struct {
	t      *testing.T
	b      *hub.Bundle
	reg    *registry.Registry
	diags  map[string]int
	chains map[string]modelChain
	// related holds the related locations expected on each RZ-CFG-018
	// finding, in text form ("first entry at file:line:column"); none for
	// a list without positions.
	related map[string][]string
}

// findingKey names one expected finding: code, resource and path.
func findingKey(code, kind, name string, path diag.Path) string {
	return code + " " + kind + "/" + name + " " + path.String()
}

// specPath returns spec.<field>, or spec.<field>[name=<name>] when name is
// set.
func specPath(field, name string) diag.Path {
	p := diag.Path{diag.Field("spec"), diag.Field(field)}
	if name != "" {
		p = append(p, diag.Keyed("name", name))
	}
	return p
}

func (m *precedenceModel) add(code string, kind v1alpha1.Kind, name string, path diag.Path) {
	m.diags[findingKey(code, string(kind), name, path)]++
}

// closedOnly is foundation pack section 8.10: auth.* (auth.upstream-*
// included), authz.*, and a plugin Policy of Filter class auth or authz.
func closedOnly(p *v1alpha1.Policy) bool {
	t := string(p.Spec.Type)
	if strings.HasPrefix(t, "auth.") || strings.HasPrefix(t, "authz.") {
		return true
	}
	return p.Spec.Type == v1alpha1.PolicyTypePlugin && p.Spec.FilterClass != nil &&
		(*p.Spec.FilterClass == v1alpha1.FilterClassAuth || *p.Spec.FilterClass == v1alpha1.FilterClassAuthz)
}

// modelOf computes the expected chains and findings of b.
func modelOf(t *testing.T, b *hub.Bundle) *precedenceModel {
	t.Helper()
	m := &precedenceModel{t: t, b: b, reg: registry.New(), diags: map[string]int{}, chains: map[string]modelChain{}, related: map[string][]string{}}
	// Rule of 02 req 37: once per Policy, attached or not.
	for _, res := range b.Resources() {
		if p, ok := hub.Object[v1alpha1.Policy](res); ok && res.Kind == v1alpha1.KindPolicy &&
			p.Spec.FailureMode != nil && *p.Spec.FailureMode == v1alpha1.FailureModeOpen && closedOnly(p) {
			m.add(CodeFailureMode, v1alpha1.KindPolicy, res.Name, specPath("failureMode", ""))
		}
	}
	// Every list is checked once, whether or not a Route reaches it.
	var gwName string
	var gw []modelItem
	ups := map[string][]modelItem{}
	for _, res := range b.Resources() {
		switch res.Kind {
		case v1alpha1.KindGateway:
			if g, ok := hub.Object[v1alpha1.Gateway](res); ok && gwName == "" {
				gwName, gw = res.Name, m.list(res, phase.ScopeGateway, g.Spec.Policies)
			}
		case v1alpha1.KindUpstream:
			if u, ok := hub.Object[v1alpha1.Upstream](res); ok {
				ups[res.Name] = m.list(res, phase.ScopeUpstream, u.Spec.Policies)
			}
		default:
		}
	}
	for _, res := range b.Resources() {
		if rt, ok := hub.Object[v1alpha1.Route](res); ok && res.Kind == v1alpha1.KindRoute {
			m.chains[res.Name] = m.route(res, rt, gwName, gw, ups)
		}
	}
	return m
}

// modelEntryAt is the location positionAll gives entry i of the list
// spec.<field> of res, or "" when res has no positions: the text form of
// a related location.
func modelEntryAt(res *hub.Resource, field string, i int) string {
	if res.Tree == nil || res.Source.File == "" {
		return ""
	}
	line := 10 + i
	if field == fieldExclude {
		line = 1000 + i
	}
	return fmt.Sprintf("%s:%d:7", res.Source.File, line)
}

// list checks one spec.policies list (rules 3 and 4): unresolved entries,
// a name longer than any resource name can be included (01 req 39 row
// 009, 02 req 40), and an entry naming the same Policy as an earlier one
// (stage G's RZ-CFG-005, 01 req 34) are skipped; a disallowed scope is
// RZ-CFG-020, a slot an earlier valid entry of another Policy holds is
// RZ-CFG-018 with that entry's location related; both drop the entry.
func (m *precedenceModel) list(owner *hub.Resource, scope phase.Scope, refs []v1alpha1.PolicyRef) []modelItem {
	var out []modelItem
	seen := map[string]bool{}
	for i, ref := range refs {
		if len(ref.Name) > 63 {
			continue
		}
		res, ok := m.b.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: ref.Name})
		if !ok {
			continue
		}
		p, _ := hub.Object[v1alpha1.Policy](res)
		e, ok := m.reg.Lookup(p.Spec.Type)
		if !ok {
			m.t.Fatalf("model: Policy %s of unknown type %s", ref.Name, p.Spec.Type)
		}
		var pluginPhases []v1alpha1.Phase
		if p.Spec.Type == v1alpha1.PolicyTypePlugin {
			pl, ok := m.b.Plugin(p.Spec.Plugin)
			if !ok || len(p.Spec.Plugin) > 63 {
				continue
			}
			pluginPhases = pl.Spec.Phases
		}
		if seen[ref.Name] {
			continue
		}
		seen[ref.Name] = true
		set, err := m.reg.Phases(registry.Attachment{Policy: p, Config: res.Config, Scope: scope, PluginPhases: pluginPhases}, bodyOracle{})
		var se *registry.ScopeError
		if errors.As(err, &se) {
			m.add(CodeScope, owner.Kind, owner.Name, specPath(fieldPolicies, ref.Name))
			continue
		}
		if err != nil {
			m.t.Fatalf("model: Phases of %s: %v", ref.Name, err)
		}
		it := modelItem{
			name: p.Metadata.Name, slot: e.EffectiveSlot(p), typ: p.Spec.Type, class: e.EffectiveClass(p), scope: scope, pos: i,
			set: set, locked: p.Spec.Overridable != nil && !*p.Spec.Overridable,
		}
		if k := slices.IndexFunc(out, func(o modelItem) bool { return o.slot == it.slot }); k >= 0 {
			key := findingKey(CodeSlot, string(owner.Kind), owner.Name, specPath(fieldPolicies, ref.Name))
			m.diags[key]++
			if at := modelEntryAt(owner, fieldPolicies, out[k].pos); at != "" {
				m.related[key] = []string{"first entry at " + at}
			}
			continue
		}
		out = append(out, it)
	}
	return out
}

// route applies rules 1, 2 and 5, the cache guardrail and the leg order to
// one Route.
func (m *precedenceModel) route(res *hub.Resource, rt *v1alpha1.Route, gwName string, gw []modelItem, ups map[string][]modelItem) modelChain {
	name := res.Name
	removed := make([]*hub.Removed, len(gw))
	// Rule 1: excluding a locked Gateway Policy is RZ-CFG-019 at the first
	// excludePolicies entry naming it; a repeated name is stage G's
	// RZ-CFG-005 (01 req 34) and changes nothing.
	exSeen := map[string]bool{}
	for _, ex := range rt.Spec.ExcludePolicies {
		gi := slices.IndexFunc(gw, func(g modelItem) bool { return g.name == ex.Name })
		again := exSeen[ex.Name]
		exSeen[ex.Name] = true
		switch {
		case gi < 0, again:
		case gw[gi].locked:
			m.add(CodeOverridable, v1alpha1.KindRoute, name, specPath(fieldExclude, ex.Name))
		case removed[gi] == nil:
			removed[gi] = &hub.Removed{Policy: gw[gi].name, Slot: gw[gi].slot, Reason: hub.Excluded}
		default:
		}
	}
	// Rule 2: a valid Route entry in the slot of a remaining Gateway
	// Policy replaces it, or is RZ-CFG-019 when that Policy is locked.
	var all []modelItem
	var kept []modelItem
	for _, it := range m.list(res, phase.ScopeRoute, rt.Spec.Policies) {
		gi := slices.IndexFunc(gw, func(g modelItem) bool { return g.slot == it.slot })
		if gi >= 0 && removed[gi] == nil {
			if gw[gi].locked {
				m.add(CodeOverridable, v1alpha1.KindRoute, name, specPath(fieldPolicies, it.name))
				continue
			}
			removed[gi] = &hub.Removed{Policy: gw[gi].name, Slot: gw[gi].slot, By: it.name, Reason: hub.Replaced}
			it.replaces = gw[gi].name
		}
		kept = append(kept, it)
	}
	for gi, g := range gw {
		if removed[gi] == nil {
			all = append(all, g)
		}
	}
	all = append(all, kept...)

	var c modelChain
	c.client = modelPhases(all)
	// Cache guardrail (02 req 39): one RZ-CFG-038 per cache attachment of
	// a chain whose onRequestBody runs authz, validation or plugin auth.
	if slices.ContainsFunc(all, func(it modelItem) bool {
		return it.set.Has(phase.OnRequestBody) &&
			(it.class == phase.ClassAuthz || it.class == phase.ClassValidation || (it.class == phase.ClassAuth && it.typ == v1alpha1.PolicyTypePlugin))
	}) {
		for _, it := range all {
			if it.typ != v1alpha1.PolicyTypeCache {
				continue
			}
			if it.scope == phase.ScopeGateway {
				m.add(CodeCacheGuardrail, v1alpha1.KindGateway, gwName, specPath(fieldPolicies, it.name))
			} else {
				m.add(CodeCacheGuardrail, v1alpha1.KindRoute, name, specPath(fieldPolicies, it.name))
			}
		}
	}
	// Leg order (02 req 41): spec.upstreams by name, then composition
	// steps in authored order, each existing Upstream once.
	var names []string
	for _, u := range rt.Spec.Upstreams {
		if len(u.Name) <= 63 {
			names = append(names, u.Name)
		}
	}
	slices.Sort(names)
	if rt.Spec.Composition != nil {
		for _, st := range rt.Spec.Composition.Steps {
			if len(st.Upstream) <= 63 {
				names = append(names, st.Upstream)
			}
		}
	}
	for i, n := range names {
		items, ok := ups[n]
		if ok && !slices.Contains(names[:i], n) {
			c.legs = append(c.legs, modelLeg{upstream: n, phases: modelPhases(items)})
		}
	}
	for _, reason := range []hub.RemovalReason{hub.Replaced, hub.Excluded} {
		for _, r := range removed {
			if r != nil && r.Reason == reason {
				c.removed = append(c.removed, *r)
			}
		}
	}
	return c
}

// modelPhases places items in every Phase of their set and orders each
// Phase by class, scope and position, response Phases reversed (rule 5).
func modelPhases(items []modelItem) [phase.Count][]string {
	var out [phase.Count][]string
	for p := range phase.Count {
		var es []modelItem
		for _, it := range items {
			if it.set.Has(p) {
				es = append(es, it)
			}
		}
		slices.SortStableFunc(es, func(a, b modelItem) int {
			if a.class != b.class {
				return cmp.Compare(a.class, b.class)
			}
			if a.scope != b.scope {
				return cmp.Compare(a.scope, b.scope)
			}
			return cmp.Compare(a.pos, b.pos)
		})
		if p.IsResponse() {
			slices.Reverse(es)
		}
		for _, it := range es {
			out[p] = append(out[p], it.key())
		}
	}
	return out
}

// keys renders the entries of one Phase.
func keys(es []hub.Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, entryKey(e))
	}
	return out
}

// compare reports every difference between the model and a Run result.
func (m *precedenceModel) compare(t *testing.T, chains map[string]*hub.Chain, ds diag.List) {
	t.Helper()
	got := map[string]int{}
	for _, d := range ds {
		switch d.Code {
		case CodeSlot, CodeOverridable, CodeScope, CodeFailureMode, CodeCacheGuardrail:
		default:
			t.Errorf("stage I raised %s: %+v", d.Code, d)
			continue
		}
		if d.Resource == nil {
			t.Errorf("finding without a resource: %+v", d)
			continue
		}
		key := findingKey(d.Code, d.Resource.Kind, d.Resource.Name, d.Path)
		got[key]++
		if d.Code == CodeOverridable {
			continue
		}
		var rel []string
		for _, r := range d.Related {
			rel = append(rel, fmt.Sprintf("%s %s:%d:%d", r.Message, r.File, r.Line, r.Column))
		}
		if want := m.related[key]; !slices.Equal(rel, want) {
			t.Errorf("finding %s: related %q, want %q", key, rel, want)
		}
	}
	for k, n := range got {
		if want := m.diags[k]; n != want {
			t.Errorf("finding %s raised %d times, want %d", k, n, want)
		}
	}
	for k, want := range m.diags {
		if _, ok := got[k]; !ok {
			t.Errorf("finding %s raised 0 times, want %d", k, want)
		}
	}
	if len(chains) != len(m.chains) {
		t.Errorf("%d chains, want %d", len(chains), len(m.chains))
	}
	for name, want := range m.chains {
		c, ok := chains[name]
		if !ok {
			t.Errorf("no chain for Route %s", name)
			continue
		}
		for p := range phase.Count {
			if g := keys(c.Client[p]); !slices.Equal(g, want.client[p]) {
				t.Errorf("%s client %s = %q, want %q", name, p, g, want.client[p])
			}
		}
		var legs []string
		for _, l := range c.Legs {
			legs = append(legs, l.Upstream)
		}
		var wantLegs []string
		for _, l := range want.legs {
			wantLegs = append(wantLegs, l.upstream)
		}
		if !slices.Equal(legs, wantLegs) {
			t.Errorf("%s legs = %q, want %q", name, legs, wantLegs)
			continue
		}
		for i, l := range want.legs {
			for p := range phase.Count {
				if g := keys(c.Legs[i].Phases[p]); !slices.Equal(g, l.phases[p]) {
					t.Errorf("%s leg %s %s = %q, want %q", name, l.upstream, p, g, l.phases[p])
				}
			}
		}
		if !slices.Equal(c.Removed, want.removed) {
			t.Errorf("%s Removed = %+v, want %+v", name, c.Removed, want.removed)
		}
	}
}
