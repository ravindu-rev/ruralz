// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
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
// attachments of registry types (02 test 30).
func genBundle(data []byte) *hub.Bundle {
	g := &gen{data: data}
	b := newBuilder()
	phases := []v1alpha1.Phase{
		v1alpha1.PhaseOnRequestHeaders, v1alpha1.PhaseOnRequestBody, v1alpha1.PhaseOnResponse, v1alpha1.PhaseOnLog,
		v1alpha1.PhaseOnUpstreamRequest, v1alpha1.PhaseOnUpstreamResponseHeaders, v1alpha1.PhaseOnChunk,
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
	for i := range 1 + g.next(3) {
		var ups []string
		for _, u := range []string{"u0", "u1", "u2"} {
			if g.flip() {
				ups = append(ups, u)
			}
		}
		b.route(fmt.Sprintf("r%d", i), pick(5), pick(3), ups)
	}
	return b.bundle()
}

// checkInvariants asserts the precedence laws of 02 test 30 on one result.
func checkInvariants(t *testing.T, b *hub.Bundle, chains map[string]*hub.Chain, ds diag.List) {
	t.Helper()
	for _, d := range ds {
		if d.Code == CodeOverridable {
			p, ok := b.Policy(d.Path[2].Key)
			if d.Path[1].Name == fieldExclude && (!ok || p.Spec.Overridable == nil || *p.Spec.Overridable) {
				t.Errorf("RZ-CFG-019 for excluding an overridable Policy: %+v", d)
			}
		}
	}
	for name, c := range chains {
		if c.Route != name {
			t.Errorf("chain %q keyed %q", c.Route, name)
		}
		checkPhases(t, name, "client", &c.Client, func(p phase.Phase, e hub.Entry) bool {
			return p.ClientLeg() && (e.Scope == phase.ScopeGateway || e.Scope == phase.ScopeRoute)
		})
		for i := range c.Legs {
			checkPhases(t, name, c.Legs[i].Upstream, &c.Legs[i].Phases, func(p phase.Phase, e hub.Entry) bool {
				return p.UpstreamLeg() && e.Scope == phase.ScopeUpstream
			})
		}
		// One Policy per slot: Gateway and Route entries never share one.
		slotOf := map[string]hub.Entry{}
		for p := range phase.Count {
			for _, e := range c.Client[p] {
				if prev, ok := slotOf[e.Slot]; ok && (prev.Scope != e.Scope || prev.Position != e.Position) {
					t.Errorf("%s: %s and %s share slot %q", name, prev.Policy, e.Policy, e.Slot)
				}
				slotOf[e.Slot] = e
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
		rows := Rows(c, b)
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
// per slot per scope, overridable: false never removed (RZ-CFG-019
// instead), order by class, scope and position, response Phases
// reversed, onLog in request order, and a result independent of the
// worker count.
func FuzzPrecedenceInvariants(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20})
	f.Add([]byte("precedence laws: slots, overridable, order"))
	f.Add([]byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9})
	for i := range 24 {
		seed := make([]byte, 160)
		for j := range seed {
			seed[j] = byte((i*131 + j*j*7 + j) % 251)
		}
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		b := genBundle(data)
		r := New(nil, Options{Body: bodyOracle{}})
		chains, ds, err := r.Run(t.Context(), b, 1)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		checkInvariants(t, b, chains, ds)
		chains4, ds4, err := r.Run(t.Context(), b, 4)
		if err != nil || !reflect.DeepEqual(chains, chains4) || !reflect.DeepEqual(ds, ds4) {
			t.Errorf("workers 4 differ from workers 1 (%v)", err)
		}
	})
}
