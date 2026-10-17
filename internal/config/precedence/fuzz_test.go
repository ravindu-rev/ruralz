// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// gen draws small numbers from fuzz input; past the end it yields 0.
type gen struct {
	data []byte
	i    int
}

func (g *gen) next(n int) int {
	if n <= 1 || g.i >= len(g.data) {
		g.i++
		return 0
	}
	v := int(g.data[g.i]) % n
	g.i++
	return v
}

func (g *gen) flip() bool { return g.next(2) == 1 }

// fuzzTypes are the attachable types the generator draws from; scopes the
// registry rejects exercise RZ-CFG-020.
func fuzzTypes() []v1alpha1.PolicyType {
	return []v1alpha1.PolicyType{
		v1alpha1.PolicyTypeAuthJWT, v1alpha1.PolicyTypeAuthAPIKey, v1alpha1.PolicyTypeAuthzCEL, v1alpha1.PolicyTypeAuthzIP,
		v1alpha1.PolicyTypeRateLimit, v1alpha1.PolicyTypeQuota, v1alpha1.PolicyTypeValidationJSONSchema, v1alpha1.PolicyTypeCORS,
		v1alpha1.PolicyTypeCache, v1alpha1.PolicyTypeHeaders, v1alpha1.PolicyTypeTransformRequest, v1alpha1.PolicyTypeTransformResponse,
		v1alpha1.PolicyTypeAuthUpstreamOAuth2, v1alpha1.PolicyTypePlugin, v1alpha1.PolicyTypeAITokenBudget, v1alpha1.PolicyTypeAIGuardrail,
	}
}

// genBundle builds a random Bundle of Gateway, Route and Upstream
// attachments of registry types, with slots, overridable: false,
// failureMode open, missing Policies and Plugins, and Route Upstreams and
// composition steps in any order (02 test 30).
func genBundle(data []byte) *hub.Bundle {
	g := &gen{data: data}
	b := newBuilder()
	// Every Phase, so a Plugin can put two or more entries in each Phase
	// of either leg kind (TestFuzzSeedsCoverLaws).
	phases := make([]v1alpha1.Phase, 0, phase.Count)
	for p := range phase.Count {
		phases = append(phases, p.V1alpha1())
	}
	for i := range 3 {
		var ps []v1alpha1.Phase
		for _, p := range phases {
			if g.next(3) == 0 {
				ps = append(ps, p)
			}
		}
		b.plugin(fmt.Sprintf("pl%d", i), ps...)
	}
	types := fuzzTypes()
	slots := []string{"", "s1", "s2", "auth", "cors"}
	n := 4 + g.next(10)
	pool := make([]string, 0, n+1)
	for i := range n {
		name := fmt.Sprintf("p%d", i)
		typ := types[g.next(len(types))]
		var mods []func(*v1alpha1.PolicySpec)
		if s := slots[g.next(len(slots))]; s != "" {
			mods = append(mods, slot(s))
		}
		if g.next(3) == 0 {
			mods = append(mods, locked)
		}
		if g.next(4) == 0 {
			mods = append(mods, failOpen)
		}
		var cfg any
		switch typ {
		case v1alpha1.PolicyTypeHeaders:
			cfg = headersCfg(g.flip(), g.flip())
		case v1alpha1.PolicyTypeTransformRequest:
			cfg = &v1alpha1.TransformRequestConfig{Body: []string{"", "x"}[g.next(2)]}
		case v1alpha1.PolicyTypeTransformResponse:
			cfg = &v1alpha1.TransformResponseConfig{Body: []string{"", "x"}[g.next(2)]}
		case v1alpha1.PolicyTypeAuthzCEL:
			cfg = authzCfg([]string{"", "auth.ok", "request.body.ok"}[g.next(3)])
		case v1alpha1.PolicyTypePlugin:
			classes := []v1alpha1.FilterClass{
				v1alpha1.FilterClassCORS, v1alpha1.FilterClassAuth, v1alpha1.FilterClassAuthz, v1alpha1.FilterClassAdmission,
				v1alpha1.FilterClassValidation, v1alpha1.FilterClassCache, v1alpha1.FilterClassUpstreamAuth,
				v1alpha1.FilterClassTransform, v1alpha1.FilterClassCustom,
			}
			mods = append(mods, pluginOf(fmt.Sprintf("pl%d", g.next(4)), classes[g.next(len(classes))]))
		default:
		}
		b.policy(name, typ, cfg, mods...)
		pool = append(pool, name)
	}
	pool = append(pool, "ghost")
	pick := func(limit int) []string {
		var out []string
		for range g.next(limit + 1) {
			out = append(out, pool[g.next(len(pool))])
		}
		return out
	}
	b.gateway(pick(6)...)
	for i := range 2 {
		b.upstream(fmt.Sprintf("u%d", i), pick(3)...)
	}
	// Upstream names in drawn order (u2 does not exist), and sometimes
	// composition steps, so the leg order rule (02 req 41) sees unsorted,
	// repeated and missing names.
	upstreams := []string{"u0", "u1", "u2"}
	for i := range 1 + g.next(3) {
		var ups []string
		for range g.next(4) {
			ups = append(ups, upstreams[g.next(len(upstreams))])
		}
		rt := b.route(fmt.Sprintf("r%d", i), pick(5), pick(3), ups)
		if g.next(3) == 0 {
			rt.Spec.Composition = &v1alpha1.Composition{Mode: v1alpha1.CompositionModeSequential}
			for j := range 1 + g.next(3) {
				rt.Spec.Composition.Steps = append(rt.Spec.Composition.Steps, v1alpha1.CompositionStep{
					Name: fmt.Sprintf("s%d", j), Upstream: upstreams[g.next(len(upstreams))],
				})
			}
		}
	}
	return b.bundle()
}

// positionAll gives every Gateway, Route and Upstream of b its own file
// "<kind>/<name>.yaml" and a positioned tree holding its spec.policies
// (entry i at line 10+i, column 7) and spec.excludePolicies (entry i at
// line 1000+i), as modelEntryAt expects, so every finding stage I raises
// on a list has a location and RZ-CFG-018 a related one; it returns the
// FileTable.
func positionAll(b *hub.Bundle) *tree.FileTable {
	files := &tree.FileTable{}
	for _, r := range b.Resources() {
		var policies, excludes []v1alpha1.PolicyRef
		switch o := r.Object.(type) {
		case *v1alpha1.Gateway:
			policies = o.Spec.Policies
		case *v1alpha1.Upstream:
			policies = o.Spec.Policies
		case *v1alpha1.Route:
			policies, excludes = o.Spec.Policies, o.Spec.ExcludePolicies
		default:
			continue
		}
		path := strings.ToLower(string(r.Kind)) + "/" + r.Name + ".yaml"
		id := files.Add(tree.File{Path: path})
		items := func(refs []v1alpha1.PolicyRef, line int32) *tree.Node {
			out := &tree.Node{Kind: tree.KindList, Pos: tree.Pos{File: id, Line: line - 1, Column: 5}}
			for i, ref := range refs {
				p := tree.Pos{File: id, Line: line + int32(i), Column: 7}
				out.Items = append(out.Items, &tree.Node{Kind: tree.KindMap, Pos: p, Members: []tree.Member{
					{Key: "name", KeyPos: p, Value: &tree.Node{Kind: tree.KindString, Text: ref.Name, Pos: p}},
				}})
			}
			return out
		}
		spec := &tree.Node{Kind: tree.KindMap, Pos: tree.Pos{File: id, Line: 4, Column: 3}, Members: []tree.Member{
			{Key: fieldPolicies, Value: items(policies, 10)},
			{Key: fieldExclude, Value: items(excludes, 1000)},
		}}
		r.Tree = &tree.Node{Kind: tree.KindMap, Pos: tree.Pos{File: id, Line: 1, Column: 1}, Members: []tree.Member{{Key: "spec", Value: spec}}}
		r.Source.File = path
	}
	return files
}

// checkInvariants asserts the precedence laws of 02 test 30 on one result:
// the chains and findings equal those of the reference model (modelOf),
// which makes every law complete as well as sound, and the laws hold in
// their own terms on the chains.
func checkInvariants(t *testing.T, b *hub.Bundle, chains map[string]*hub.Chain, ds diag.List) {
	t.Helper()
	m := modelOf(t, b)
	m.compare(t, chains, ds)
	for name, c := range chains {
		if c.Route != name {
			t.Errorf("chain %q keyed %q", c.Route, name)
		}
		checkPhases(t, name, LegClient, &c.Client, func(p phase.Phase, e hub.Entry) bool {
			return p.ClientLeg() && (e.Scope == phase.ScopeGateway || e.Scope == phase.ScopeRoute)
		})
		// One Policy per slot: on the client leg Gateway and Route entries
		// never share one, and on each Upstream leg no two entries do.
		checkSlots(t, name, LegClient, &c.Client)
		for i := range c.Legs {
			checkPhases(t, name, c.Legs[i].Upstream, &c.Legs[i].Phases, func(p phase.Phase, e hub.Entry) bool {
				return p.UpstreamLeg() && e.Scope == phase.ScopeUpstream
			})
			checkSlots(t, name, c.Legs[i].Upstream, &c.Legs[i].Phases)
		}
		for p := range phase.Count {
			for _, e := range c.Client[p] {
				if e.Scope == phase.ScopeGateway && slices.ContainsFunc(c.Removed, func(r hub.Removed) bool { return r.Policy == e.Policy }) {
					t.Errorf("%s: removed Gateway Policy %s still runs", name, e.Policy)
				}
			}
		}
		// overridable: false is never removed; replaced before excluded.
		for i, r := range c.Removed {
			if p, ok := b.Policy(r.Policy); !ok || (p.Spec.Overridable != nil && !*p.Spec.Overridable) {
				t.Errorf("%s: removed %s, which is not overridable", name, r.Policy)
			}
			if i > 0 && c.Removed[i-1].Reason > r.Reason {
				t.Errorf("%s: Removed out of order: %+v", name, c.Removed)
			}
			if (r.Reason == hub.Replaced) != (r.By != "") {
				t.Errorf("%s: removal %+v", name, r)
			}
		}
		rows := mustNew(t, nil, Options{}).Rows(c, b)
		want := len(c.Removed)
		for p := range phase.Count {
			want += len(c.Client[p])
			for _, l := range c.Legs {
				want += len(l.Phases[p])
			}
		}
		if len(rows) != want {
			t.Errorf("%s: %d rows, want %d", name, len(rows), want)
		}
	}
}

// checkSlots asserts one Policy per slot on one leg: entries that share a
// slot are one entry (one scope and position) listed in several Phases
// (02 req 35.3).
func checkSlots(t *testing.T, route, leg string, phases *[phase.Count][]hub.Entry) {
	t.Helper()
	slotOf := map[string]hub.Entry{}
	for p := range phase.Count {
		for _, e := range phases[p] {
			if prev, ok := slotOf[e.Slot]; ok && (prev.Scope != e.Scope || prev.Position != e.Position) {
				t.Errorf("%s/%s: %s and %s share slot %q", route, leg, prev.Policy, e.Policy, e.Slot)
			}
			slotOf[e.Slot] = e
		}
	}
}

// compareEntries orders entries by Filter class, scope, then position
// (foundation pack 8.12 rule 5), the request order checkPhases expects.
func compareEntries(a, b hub.Entry) int {
	return cmp.Or(cmp.Compare(a.Class, b.Class), cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.Position, b.Position))
}

// checkPhases asserts per-Phase membership, leg and order rules (02 req
// 35.4 and 35.5, 04 req 40).
func checkPhases(t *testing.T, route, leg string, phases *[phase.Count][]hub.Entry, allowed func(phase.Phase, hub.Entry) bool) {
	t.Helper()
	for p := range phase.Count {
		l := phases[p]
		for i, e := range l {
			if !e.Phases.Has(p) || !allowed(p, e) {
				t.Errorf("%s/%s: %s in %s at scope %s", route, leg, e.Policy, p, e.Scope)
			}
			if i == 0 {
				continue
			}
			c := compareEntries(l[i-1], e)
			if (p.IsResponse() && c <= 0) || (!p.IsResponse() && c >= 0) {
				t.Errorf("%s/%s %s: %s before %s breaks class, scope, position order", route, leg, p, l[i-1].Policy, e.Policy)
			}
		}
	}
}

// FuzzPrecedenceInvariants is the precedence property of TQ "Required
// properties" (02 test 30, 11 req 25): for random attachments, one Policy
// per slot per scope on the client leg and on every Upstream leg,
// overridable: false never removed (RZ-CFG-019 instead), order by class,
// scope and position, response Phases reversed, onLog in request order,
// legs in leg order, a result independent of the worker count and of
// yields, a capped run keeping the first findings in order, and the same
// laws with every slot made long and full of control characters, with
// bounded messages and one table line per row (checkLongSlots). Every
// law is checked for completeness as well as soundness against the
// reference model: each RZ-CFG-018, RZ-CFG-019 (one per excludePolicies
// entry naming an attached overridable: false Gateway Policy and one per
// valid Route entry in its slot), RZ-CFG-020, RZ-CFG-029 and RZ-CFG-038 it
// predicts is raised exactly once and no other finding is, and every
// chain, leg and removal is the one it predicts.
func FuzzPrecedenceInvariants(f *testing.F) {
	for _, seed := range fuzzSeeds(f) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		b := genBundle(data)
		r := mustNew(t, nil, Options{Body: bodyOracle{}})
		chains, ds, err := r.Run(t.Context(), b, nil, 1)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		checkInvariants(t, b, chains, ds)
		checkLongSlots(t, r, b, ds)
		chains4, ds4, err := r.Run(t.Context(), b, nil, 4)
		if err != nil || !reflect.DeepEqual(chains, chains4) || !reflect.DeepEqual(ds, ds4) {
			t.Errorf("workers 4 differ from workers 1 (%v)", err)
		}
		// The same Bundle with every list positioned: the model holds,
		// related locations included, on 1 and 4 workers.
		pb := genBundle(data)
		files := positionAll(pb)
		for _, workers := range []int{1, 4} {
			pc, pds, err := r.Run(t.Context(), pb, files, workers)
			if err != nil {
				t.Fatalf("positioned Run: %v", err)
			}
			checkInvariants(t, pb, pc, pds)
		}
		// The cap keeps the first findings in order (01 req 50), and
		// yields never change the result (01 req 53).
		clock := &stepClock{step: time.Microsecond}
		capped := mustNew(t, nil, Options{Body: bodyOracle{}, MaxDiagnostics: 2, Now: clock.now, YieldAfter: time.Microsecond})
		for _, workers := range []int{1, 4} {
			cc, dc, err := capped.Run(t.Context(), b, nil, workers)
			if err != nil || !reflect.DeepEqual(cc, chains) || !reflect.DeepEqual(dc, ds[:min(len(ds), 3)]) {
				t.Errorf("workers %d with cap 2 and yields: %v\n%s\nwant the first 3 of\n%s", workers, err, diagText(t, dc), diagText(t, ds))
			}
		}
	})
}

// longSlotOf returns s with a tail of control characters and 200 bytes, so
// the slot quoted whole would break a table row and pass every message
// bound; it is injective, so it keeps which slots are equal.
func longSlotOf(s string) string { return s + "\n\t\x1b[2J" + strings.Repeat("y", 200) }

// relabel returns a copy of b whose Policies have the slot longSlotOf
// theirs (registry defaults included: genBundle's Policies are
// materialized).
func relabel(b *hub.Bundle) *hub.Bundle {
	var rs []*hub.Resource
	for _, r := range b.Resources() {
		if p, ok := object[v1alpha1.Policy](r); ok {
			cp, cr := *p, *r
			cp.Spec.Slot = longSlotOf(p.Spec.Slot)
			cr.Object = &cp
			r = &cr
		}
		rs = append(rs, r)
	}
	return hub.NewBundle(rs)
}

// checkLongSlots is the metamorphic law of the WP-38 S2 round-4 blocker
// and its Rows minor: giving every Policy a long slot with control
// characters, equality kept, changes no law (the reference model holds
// on the relabeled Bundle and its findings are those of b, by code,
// resource and path), every message stays under 200 bytes, and the
// render --effective table of every chain has one line per row.
func checkLongSlots(t *testing.T, r *Resolver, b *hub.Bundle, ds diag.List) {
	t.Helper()
	lb := relabel(b)
	lc, lds, err := r.Run(t.Context(), lb, nil, 1)
	if err != nil {
		t.Fatalf("Run with long slots: %v", err)
	}
	checkInvariants(t, lb, lc, lds)
	key := func(ds diag.List) []string {
		var out []string
		for _, d := range ds {
			out = append(out, d.Code+" "+d.Resource.Kind+"/"+d.Resource.Name+" "+d.Path.String())
			if len(d.Message) > 200 {
				t.Errorf("%s message of %d bytes", d.Code, len(d.Message))
			}
		}
		slices.Sort(out)
		return out
	}
	if got, want := key(lds), key(ds); !slices.Equal(got, want) {
		t.Errorf("long slots changed the findings:\n got %q\nwant %q", got, want)
	}
	for _, name := range slices.Sorted(maps.Keys(lc)) {
		rows := r.Rows(lc[name], lb)
		if text := table(t, rows); strings.Count(text, "\n") != len(rows)+1 {
			t.Errorf("%s: table of %d rows has %d lines:\n%s", name, len(rows), strings.Count(text, "\n"), text)
		}
	}
}

// fuzzSeeds is the seed corpus of FuzzPrecedenceInvariants, which plain go
// test runs: fixed patterns, 24 spread inputs, and inputs found by search
// for the laws the others miss (TestFuzzSeedsCoverLaws keeps every law
// covered when genBundle changes).
func fuzzSeeds(tb testing.TB) [][]byte {
	tb.Helper()
	seeds := [][]byte{
		{},
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20},
		[]byte("precedence laws: slots, overridable, order"),
		bytes.Repeat([]byte{9}, 59),
	}
	for i := range 24 {
		seed := make([]byte, 160)
		for j := range seed {
			seed[j] = byte((i*131 + j*j*7 + j) % 251)
		}
		seeds = append(seeds, seed)
	}
	for _, h := range []string{
		// Legs with two or more entries in onUpstreamRequest and
		// onUpstreamResponseHeaders. (It also names one Upstream Policy
		// twice in one list, which raised RZ-CFG-018 before stage I left
		// that duplicate to stage G, WP-38 S2 round-2 minor.)
		"13c5f9b7687e6f0b64cd8f0de82f2127e24a94dfc4d4c4038c8a808149fb6a7779056bc6644e1dba781ff8c9f9f37caf3bc176d4ebdd5d7063d1a7be11a8664f74e847546b5364fd9271d2722301915eb445a1e426b5e465ac0b658a78695f65",
		// A cache Policy whose chain runs only a plugin auth Policy in
		// onRequestBody (RZ-CFG-038 from that Policy alone), and a client
		// onChunk with two or more entries.
		"61053b92e5f7b1bc3cdd301a28650dfbcf66d0f6114e4ca81f7e6fe14825ae1d8dc8dc5ef19188453de62b12d52663a520020c3fcf926ee24505749609d5e7dc07430f274086bb356f50c99160567f5535f2edfa7a71acc9b1e733ff703b358b",
		// Legs with two or more entries in onUpstreamResponseBody and in
		// onChunk.
		"0147eee4b0632b05aecf856617d1fb9e389fc22ddaa27a75eebe0e1216e16a57b224dcc715f319182c7d550eecb44fbd471bff78deb430e493f0665275549e0bdb4c130eed40b85654214adfe8495ea5dd8d5c46c94bd65e38af0766b2154cbd",
		// A client onRoute with two or more entries.
		"3014a8e25c27a6e792a434801ba7e639334beeeaa84ca78ff00c8fa8cdd1b7e026fbf0211bb92408a0135d5f20118a9578483defd3c8b220c30630dac8b70d53375eac96a7ce75a578d554d304e66855bad188fac732ae41a4825302f33c7d13",
		// Two Routes removing different Gateway Policies from one client
		// Phase in which they run no Policy of their own (two removal-only
		// slices that must not be shared).
		"60dce830ea19a56be2fc0eeffaea62fab64dfc5b802e6be18baaccb0f6c63400fa93d61a21932391eed980c50f79eb30e4146a7468243296e52f5f4f93edd7c78f84758075165f7a94090e7907291c04c3cc939a7e2977f8bcc558f57ff22ce6",
		// Two distinct Upstream Policies in one slot, both running on a leg
		// a Route reaches (RZ-CFG-018 with the first entry related, and
		// one Policy per slot on that leg).
		"8fba7119f58e990a7e9ec76efa65294b7387707422541aa6dde0fd80192b1543b5724a7e1cec8770803dc3528c89de89a20721b3229ce97a6c4a80437341b3e6f10eecd58c0b03d78aa94446e2d1b62ba774fca1d867ef6ea8ebe4fb66061264d253754321335df1a001ed2c16e6c5ee9b1e8c3bdda66d9b42a4a90644642804f6f386c9e2438fee07a3e431772bd52eb2b5ecf5b239abbc21355faf1078066e",
	} {
		seed, err := hex.DecodeString(h)
		if err != nil {
			tb.Fatal(err)
		}
		seeds = append(seeds, seed)
	}
	return seeds
}

// lawsOf names the precedence laws and input shapes one generated Bundle
// exercises.
func lawsOf(t *testing.T, b *hub.Bundle, chains map[string]*hub.Chain, ds diag.List) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	reg := registry.New()
	for _, d := range ds {
		got[d.Code+" at "+d.Resource.Kind+" "+d.Path[1].Name] = true
		if d.Code == CodeSlot && len(d.Related) > 0 {
			got["RZ-CFG-018 with the first entry related at "+d.Resource.Kind] = true
		}
		// RZ-CFG-018 on an Upstream that a Route reaches, for a Policy
		// that would run there: the leg must still hold one per slot.
		if d.Code != CodeSlot || d.Resource.Kind != string(v1alpha1.KindUpstream) {
			continue
		}
		res, _ := b.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: d.Path[2].Key})
		p, _ := hub.Object[v1alpha1.Policy](res)
		var pluginPhases []v1alpha1.Phase
		if pl, ok := b.Plugin(p.Spec.Plugin); ok {
			pluginPhases = pl.Spec.Phases
		}
		set, err := reg.Phases(registry.Attachment{Policy: p, Config: res.Config, Scope: phase.ScopeUpstream, PluginPhases: pluginPhases}, bodyOracle{})
		if err != nil || set == 0 {
			continue
		}
		for _, c := range chains {
			if slices.ContainsFunc(c.Legs, func(l hub.Leg) bool { return l.Upstream == d.Resource.Name }) {
				got["RZ-CFG-018 on a reached Upstream leg"] = true
			}
		}
	}
	for _, res := range b.Resources() {
		var refs, excludes []v1alpha1.PolicyRef
		switch o := res.Object.(type) {
		case *v1alpha1.Gateway:
			refs = o.Spec.Policies
		case *v1alpha1.Upstream:
			refs = o.Spec.Policies
		case *v1alpha1.Route:
			refs, excludes = o.Spec.Policies, o.Spec.ExcludePolicies
		default:
		}
		seen := map[string]bool{}
		for _, ref := range refs {
			p, ok := b.Policy(ref.Name)
			if !ok {
				got["attachment: missing Policy"] = true
			} else if _, ok := b.Plugin(p.Spec.Plugin); p.Spec.Type == v1alpha1.PolicyTypePlugin && !ok {
				got["attachment: missing Plugin"] = true
			} else if seen[ref.Name] {
				// One Policy named twice in one list (stage G's
				// RZ-CFG-005), which raises no stage I finding.
				got["attachment: repeated name in "+string(res.Kind)+" policies"] = true
			}
			seen[ref.Name] = true
		}
		exSeen := map[string]bool{}
		for _, ex := range excludes {
			if p, ok := b.Policy(ex.Name); ok && exSeen[ex.Name] && p.Spec.Overridable != nil && !*p.Spec.Overridable && b.Gateway() != nil &&
				slices.ContainsFunc(b.Gateway().Spec.Policies, func(r v1alpha1.PolicyRef) bool { return r.Name == ex.Name }) {
				got["exclusion: repeated name of a locked Gateway Policy"] = true
			}
			exSeen[ex.Name] = true
		}
	}
	for law := range removalOnlyLaws(t, b, chains) {
		got[law] = true
	}
	for name, c := range chains {
		res, _ := b.Get(hub.ID{Kind: v1alpha1.KindRoute, Name: name})
		rt, _ := hub.Object[v1alpha1.Route](res)
		for _, r := range c.Removed {
			got[map[hub.RemovalReason]string{hub.Replaced: "removed: replaced", hub.Excluded: "removed: excluded"}[r.Reason]] = true
		}
		var names []string
		for _, u := range rt.Spec.Upstreams {
			names = append(names, u.Name)
			if _, ok := b.Get(hub.ID{Kind: v1alpha1.KindUpstream, Name: u.Name}); !ok {
				got["leg: missing Upstream"] = true
			}
		}
		if len(c.Legs) >= 2 && !slices.IsSorted(names) {
			got["leg: unsorted spec.upstreams"] = true
		}
		if rt.Spec.Composition != nil && len(c.Legs) > len(slices.Compact(slices.Sorted(slices.Values(names)))) {
			got["leg: composition step"] = true
		}
		// The cache guardrail with exactly one kind of bypassed Policy, so
		// each kind alone must raise RZ-CFG-038.
		if slices.ContainsFunc(c.Client[phase.OnRequestHeaders], func(e hub.Entry) bool { return e.Type == v1alpha1.PolicyTypeCache }) {
			kinds := map[string]bool{}
			for _, x := range c.Client[phase.OnRequestBody] {
				switch {
				case x.Class == phase.ClassAuthz, x.Class == phase.ClassValidation:
					kinds[x.Class.String()] = true
				case x.Class == phase.ClassAuth && x.Type == v1alpha1.PolicyTypePlugin:
					kinds["auth plugin"] = true
				default:
				}
			}
			if len(kinds) == 1 {
				for k := range kinds {
					got["038 only beside "+k] = true
				}
			}
		}
		for p := range phase.Count {
			for _, e := range c.Client[p] {
				if e.Scope == phase.ScopeGateway && !e.Overridable {
					got["chain: locked Gateway Policy runs"] = true
				}
			}
			if len(c.Client[p]) >= 2 {
				got[orderLaw("client", p)] = true
			}
			for _, l := range c.Legs {
				if len(l.Phases[p]) >= 2 {
					got[orderLaw("leg", p)] = true
				}
			}
		}
	}
	return got
}

// removalOnlyLaws names the removal-only client Phases of chains that
// two Routes have in common (a Phase where some Gateway Policy was
// removed and no Route Policy runs), which Resolve shares through its
// memo: two Routes removing the same Gateway Policies from one Phase, and
// two removing different ones, so the reference model checks that the
// memo neither splits equal content nor merges different content.
func removalOnlyLaws(t *testing.T, b *hub.Bundle, chains map[string]*hub.Chain) map[string]bool {
	t.Helper()
	reg := registry.New()
	gatewayPhases := func(name string) phase.Set {
		res, ok := b.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: name})
		if !ok {
			return 0
		}
		p, _ := hub.Object[v1alpha1.Policy](res)
		var pluginPhases []v1alpha1.Phase
		if pl, ok := b.Plugin(p.Spec.Plugin); ok {
			pluginPhases = pl.Spec.Phases
		}
		set, err := reg.Phases(registry.Attachment{Policy: p, Config: res.Config, Scope: phase.ScopeGateway, PluginPhases: pluginPhases}, bodyOracle{})
		if err != nil {
			return 0
		}
		return set
	}
	got := map[string]bool{}
	byPhase := map[phase.Phase][]string{}
	for _, name := range slices.Sorted(maps.Keys(chains)) {
		c := chains[name]
		for p := range phase.Count {
			if slices.ContainsFunc(c.Client[p], func(e hub.Entry) bool { return e.Scope == phase.ScopeRoute }) {
				continue
			}
			var key []string
			for _, r := range c.Removed {
				if gatewayPhases(r.Policy).Has(p) {
					key = append(key, r.Policy)
				}
			}
			if len(key) > 0 {
				byPhase[p] = append(byPhase[p], strings.Join(key, ","))
			}
		}
	}
	for _, keys := range byPhase {
		for i := range keys {
			for j := range i {
				if keys[i] == keys[j] {
					got["shared: equal removals in one Phase of two Routes"] = true
				} else {
					got["shared: different removals in one Phase of two Routes"] = true
				}
			}
		}
	}
	return got
}

// orderLaw names the law that a Phase of a leg kind ("client" or "leg")
// holds two or more entries, so its order rule is checked.
func orderLaw(kind string, p phase.Phase) string {
	return "order: two or more in " + kind + " " + p.String()
}

// TestFuzzSeedsCoverLaws checks that the seed corpus alone, which every go
// test run checks, exercises every finding at every scope it applies to,
// both removal reasons, every leg order input, both sharing outcomes of a
// removal-only client Phase, and the order rule of every
// Phase on each leg kind it runs on (two or more entries in it: the
// client Phases onRequestHeaders, onRequestBody, onRoute, onResponse,
// onLog and onChunk, and the leg Phases onUpstreamRequest,
// onUpstreamResponseHeaders, onUpstreamResponseBody and onChunk), so the
// completeness checks of FuzzPrecedenceInvariants run without -fuzz.
func TestFuzzSeedsCoverLaws(t *testing.T) {
	covered := map[string]bool{}
	for _, seed := range fuzzSeeds(t) {
		b := genBundle(seed)
		chains, ds, err := mustNew(t, nil, Options{Body: bodyOracle{}}).Run(t.Context(), b, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		for law := range lawsOf(t, b, chains, ds) {
			covered[law] = true
		}
		pb := genBundle(seed)
		files := positionAll(pb)
		pc, pds, err := mustNew(t, nil, Options{Body: bodyOracle{}}).Run(t.Context(), pb, files, 1)
		if err != nil {
			t.Fatal(err)
		}
		for law := range lawsOf(t, pb, pc, pds) {
			covered[law] = true
		}
	}
	laws := []string{
		"RZ-CFG-018 at Gateway policies", "RZ-CFG-018 at Route policies", "RZ-CFG-018 at Upstream policies",
		"RZ-CFG-018 on a reached Upstream leg",
		"RZ-CFG-019 at Route excludePolicies", "RZ-CFG-019 at Route policies",
		"RZ-CFG-020 at Gateway policies", "RZ-CFG-020 at Route policies", "RZ-CFG-020 at Upstream policies",
		"RZ-CFG-029 at Policy failureMode", "RZ-CFG-038 at Route policies",
		"038 only beside authz", "038 only beside validation", "038 only beside auth plugin",
		"removed: replaced", "removed: excluded",
		"leg: missing Upstream", "leg: unsorted spec.upstreams", "leg: composition step",
		"chain: locked Gateway Policy runs", "attachment: missing Policy", "attachment: missing Plugin",
		"shared: equal removals in one Phase of two Routes", "shared: different removals in one Phase of two Routes",
		"RZ-CFG-018 with the first entry related at Gateway", "RZ-CFG-018 with the first entry related at Route",
		"RZ-CFG-018 with the first entry related at Upstream",
		"attachment: repeated name in Gateway policies", "attachment: repeated name in Route policies",
		"attachment: repeated name in Upstream policies", "exclusion: repeated name of a locked Gateway Policy",
	}
	for p := range phase.Count {
		if p.ClientLeg() {
			laws = append(laws, orderLaw("client", p))
		}
		if p.UpstreamLeg() {
			laws = append(laws, orderLaw("leg", p))
		}
	}
	if len(laws) != 41 {
		t.Errorf("%d laws, want 41 (22 findings and inputs, 2 sharing, 3 related locations, 4 repeated names, 6 client and 4 leg order laws)", len(laws))
	}
	for _, law := range laws {
		if !covered[law] {
			t.Errorf("no seed exercises %q", law)
		}
	}
}
