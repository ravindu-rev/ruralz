// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Codes of the precedence rules.
const (
	codeSlot   = "RZ-CFG-018"
	codePinned = "RZ-CFG-019"
	codeScope  = "RZ-CFG-020"
)

// FuzzPrecedence is the Precedence row of the Required properties: one
// Policy per slot per scope; overridable: false Policies are never
// removed; order follows Filter class, scope and list position; response
// Phases reverse and onLog keeps request order (foundation pack section
// 8.12). A random Bundle, written as source files and run through the
// pipeline, either holds no violation, and then every Route's effective
// chain equals the one a reference model of the rules computes (each
// Phase's Policies with their scope, position, class, slot and the
// Gateway Policy each replaces; every leg; every removed Gateway Policy),
// or violates exactly one rule, which the pipeline reports with its code
// alone: two Policies in one slot of one list (RZ-CFG-018), an
// overridable: false Gateway Policy excluded or replaced (RZ-CFG-019), a
// type at a scope it does not allow (RZ-CFG-020). The model reads only the
// Phases of each attachment from the Policy type registry, which narrows
// them by config.
func FuzzPrecedence(f *testing.F) {
	for i := range uint8(32) {
		f.Add(uint64(i)*2654435761+1, i%numGenModes)
	}
	f.Fuzz(func(t *testing.T, seed uint64, mode uint8) {
		tc := tools(t)
		md := int(mode % numGenModes)
		g := generate(newRand(seed), genOptions{mode: md})
		files := g.files("/etc/ruralz/oauth/client-secret")
		res := tc.run(t, memBundle{}.withFiles(files), noEnv, nil, false)
		want := map[int][]string{genSlot: {codeSlot}, genPinned: {codePinned}, genScope: {codeScope}}[md]
		got := slices.Compact(slices.Sorted(slices.Values(codesOf(res.Diagnostics))))
		if !slices.Equal(got, want) {
			t.Fatalf("mode %d: the Bundle reports %q, want %q:\n%s\n%s", md, got, want, texts(res.Diagnostics), dumpFiles(files))
		}
		if md != genValid {
			if res.Validated != nil {
				t.Fatalf("mode %d: an invalid Bundle builds a Revision", md)
			}
			return
		}
		checkChains(t, tc, g, res.Validated, files)
	})
}

// modelEntry is one Policy of a modeled chain.
type modelEntry struct {
	policy, slot, replaces string
	class                  phase.Class
	scope                  phase.Scope
	pos                    int
}

// String is the entry as failure messages and comparisons show it.
func (e modelEntry) String() string {
	return fmt.Sprintf("%s(%s#%d %s slot=%s replaces=%s)", e.policy, e.scope, e.pos, e.class, e.slot, e.replaces)
}

// modelChain is the reference model's effective chain of one Route.
type modelChain struct {
	client  [phase.Count][]modelEntry
	legs    []modelLeg
	removed []hub.Removed
}

// modelLeg is the modeled chain of one upstream leg.
type modelLeg struct {
	upstream string
	phases   [phase.Count][]modelEntry
}

// attachment is one modeled attachment with the Phases it runs in.
type attachment struct {
	modelEntry
	phases phase.Set
}

// model returns the effective chain of rt by the rules of foundation pack
// section 8.12: the Gateway's Policies minus rt's exclusions, then rt's,
// each replacing the remaining Gateway Policy in its slot; per Phase in
// class, scope and position order, reversed for response Phases; one leg
// per Upstream rt names, in name order (map list order), with the
// Upstream's Policies; and the removed Gateway Policies, replaced before
// excluded, each in Gateway list order. phasesOf gives the Phases of an
// attachment.
func (g *genBundle) model(rt *genRoute, phasesOf func(name string, scope phase.Scope) phase.Set) modelChain {
	var mc modelChain
	entry := func(name string, scope phase.Scope, pos int) attachment {
		p := g.policy(name)
		return attachment{modelEntry{policy: name, slot: p.effectiveSlot(), class: p.kind.class, scope: scope, pos: pos}, phasesOf(name, scope)}
	}
	var routeAtts []attachment
	for j, name := range rt.policies {
		routeAtts = append(routeAtts, entry(name, phase.ScopeRoute, j))
	}
	replacedBy := map[string]string{}
	var client []attachment
	for i, name := range g.gateway {
		if slices.Contains(rt.exclude, name) {
			continue
		}
		a := entry(name, phase.ScopeGateway, i)
		if k := slices.IndexFunc(routeAtts, func(r attachment) bool { return r.slot == a.slot }); k >= 0 {
			replacedBy[name] = routeAtts[k].policy
			routeAtts[k].replaces = name
			continue
		}
		client = append(client, a)
	}
	client = append(client, routeAtts...)
	mc.client = byPhase(client)
	for _, name := range g.gateway {
		if by, ok := replacedBy[name]; ok {
			mc.removed = append(mc.removed, hub.Removed{Policy: name, Slot: g.policy(name).effectiveSlot(), By: by, Reason: hub.Replaced})
		}
	}
	for _, name := range g.gateway {
		if slices.Contains(rt.exclude, name) {
			mc.removed = append(mc.removed, hub.Removed{Policy: name, Slot: g.policy(name).effectiveSlot(), Reason: hub.Excluded})
		}
	}
	for _, un := range slices.Sorted(slices.Values(rt.upstreams)) {
		var atts []attachment
		for _, u := range g.upstreams {
			if u.name != un {
				continue
			}
			for i, name := range u.policies {
				atts = append(atts, entry(name, phase.ScopeUpstream, i))
			}
		}
		mc.legs = append(mc.legs, modelLeg{upstream: un, phases: byPhase(atts)})
	}
	return mc
}

// byPhase sorts attachments into Phases: class, then scope, then position,
// reversed for the response Phases.
func byPhase(atts []attachment) [phase.Count][]modelEntry {
	var out [phase.Count][]modelEntry
	for ph := phase.OnRequestHeaders; ph < phase.Count; ph++ {
		var es []modelEntry
		for _, a := range atts {
			if a.phases.Has(ph) {
				es = append(es, a.modelEntry)
			}
		}
		slices.SortStableFunc(es, func(a, b modelEntry) int {
			return cmp.Or(cmp.Compare(a.class, b.class), cmp.Compare(a.scope, b.scope), cmp.Compare(a.pos, b.pos))
		})
		if ph.IsResponse() {
			slices.Reverse(es)
		}
		out[ph] = es
	}
	return out
}

// checkChains holds every Route's chain in v equal to the model's.
func checkChains(t *testing.T, tc *toolchain, g *genBundle, v *hub.Validated, files map[string][]byte) {
	t.Helper()
	reg := tc.pipe.Registry()
	phasesOf := func(name string, scope phase.Scope) phase.Set {
		t.Helper()
		res, ok := v.Bundle.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: name})
		pol, isPolicy := hub.Object[v1alpha1.Policy](res)
		if !ok || !isPolicy {
			t.Fatalf("the Revision holds no Policy %s", name)
		}
		set, err := reg.Phases(registry.Attachment{Policy: pol, Config: res.Config, Scope: scope}, tc.cel)
		if err != nil {
			t.Fatalf("Phases(%s at %s): %v", name, scope, err)
		}
		return set
	}
	if got, want := slices.Sorted(maps.Keys(v.Chains)), routeNames(g); !slices.Equal(got, want) {
		t.Fatalf("chains of Routes %q, want %q", got, want)
	}
	for _, rt := range g.routes {
		want := g.model(rt, phasesOf)
		got := v.Chains[rt.name]
		for ph := phase.OnRequestHeaders; ph < phase.Count; ph++ {
			if g, w := entriesOf(got.Client[ph]), strings.Join(stringsOf(want.client[ph]), " "); g != w {
				t.Fatalf("Route %s %s client: got %s\nwant %s\n%s", rt.name, ph, g, w, dumpFiles(files))
			}
		}
		var gotLegs, wantLegs []string
		for _, l := range got.Legs {
			gotLegs = append(gotLegs, l.Upstream)
		}
		for _, l := range want.legs {
			wantLegs = append(wantLegs, l.upstream)
		}
		if !slices.Equal(gotLegs, wantLegs) {
			t.Fatalf("Route %s legs %q, want %q", rt.name, gotLegs, wantLegs)
		}
		for i, l := range want.legs {
			for ph := phase.OnRequestHeaders; ph < phase.Count; ph++ {
				if g, w := entriesOf(got.Legs[i].Phases[ph]), strings.Join(stringsOf(l.phases[ph]), " "); g != w {
					t.Fatalf("Route %s %s leg %s: got %s\nwant %s\n%s", rt.name, ph, l.upstream, g, w, dumpFiles(files))
				}
			}
		}
		if !slices.Equal(got.Removed, want.removed) {
			t.Fatalf("Route %s removed %+v, want %+v\n%s", rt.name, got.Removed, want.removed, dumpFiles(files))
		}
		checkLaws(t, rt.name, got, g)
	}
}

// checkLaws restates the laws of the row on one chain: within each scope
// of a Phase the slots are distinct, and no overridable: false Gateway
// Policy is removed.
func checkLaws(t *testing.T, route string, c *hub.Chain, g *genBundle) {
	t.Helper()
	for ph := phase.OnRequestHeaders; ph < phase.Count; ph++ {
		seen := map[string]bool{}
		for _, e := range c.Client[ph] {
			key := e.Scope.String() + "/" + e.Slot
			if seen[key] {
				t.Fatalf("Route %s %s: two Policies in slot %s at %s scope", route, ph, e.Slot, e.Scope)
			}
			seen[key] = true
		}
	}
	for _, r := range c.Removed {
		if p := g.policy(r.Policy); p != nil && p.pinned {
			t.Fatalf("Route %s removed the overridable: false Policy %s", route, r.Policy)
		}
	}
}

// entriesOf returns a chain's entries as the model's strings.
func entriesOf(es []hub.Entry) string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = modelEntry{policy: e.Policy, slot: e.Slot, replaces: e.Replaces, class: e.Class, scope: e.Scope, pos: e.Position}.String()
	}
	return strings.Join(out, " ")
}

// stringsOf returns model entries as strings.
func stringsOf(es []modelEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.String()
	}
	return out
}

// routeNames returns the generated Route names, sorted.
func routeNames(g *genBundle) []string {
	var out []string
	for _, rt := range g.routes {
		out = append(out, rt.name)
	}
	slices.Sort(out)
	return out
}
