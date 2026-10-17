// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestRowsReasons covers every reason template of 02 req 51 and the row
// order: Phases in order, client leg before the legs, removed rows last
// with replaced before excluded whatever Chain.Removed's order. The
// ", not overridable" suffix belongs to the two Gateway templates only
// (WP-38 S2 round-3 minor): a Route entry in its own slot, in another
// slot or replacing a Gateway Policy, and an Upstream entry, each with
// overridable: false (valid input, R-79 golden test 9), print without it.
func TestRowsReasons(t *testing.T) {
	e := func(name string, scope phase.Scope, slot string, overridable bool, replaces string) hub.Entry {
		return hub.Entry{
			Policy: name, Type: v1alpha1.PolicyTypeHeaders, Class: phase.ClassTransform, Slot: slot, Scope: scope,
			Overridable: overridable, Replaces: replaces, Phases: phase.Of(phase.OnRequestHeaders),
		}
	}
	c := &hub.Chain{Route: "r"}
	c.Client[phase.OnRequestHeaders] = []hub.Entry{
		e("g-own", phase.ScopeGateway, "g-own", true, ""),
		e("g-own-locked", phase.ScopeGateway, "g-own-locked", false, ""),
		e("g-slot", phase.ScopeGateway, "shared", true, ""),
		e("g-slot-locked", phase.ScopeGateway, "auth", false, ""),
		e("r-repl", phase.ScopeRoute, "cors", true, "old-cors"),
		e("r-own", phase.ScopeRoute, "r-own", true, ""),
		e("r-slot", phase.ScopeRoute, "custom-slot", true, ""),
		e("r-own-locked", phase.ScopeRoute, "r-own-locked", false, ""),
		e("r-slot-locked", phase.ScopeRoute, "other-slot", false, ""),
		e("r-repl-locked", phase.ScopeRoute, "cors2", false, "old-cors2"),
		e("odd", phase.ScopeNone, "odd", true, ""),
	}
	c.Client[phase.OnLog] = []hub.Entry{e("late", phase.ScopeRoute, "late", true, "")}
	leg := hub.Leg{Upstream: "orders"}
	leg.Phases[phase.OnRequestHeaders] = []hub.Entry{e("u", phase.ScopeUpstream, "u", true, "")}
	leg.Phases[phase.OnUpstreamRequest] = []hub.Entry{e("u2", phase.ScopeUpstream, "u2", true, ""), e("u-locked", phase.ScopeUpstream, "u-locked", false, "")}
	c.Legs = []hub.Leg{leg}
	c.Removed = []hub.Removed{
		{Policy: "ex", Slot: "ex", Reason: hub.Excluded},
		{Policy: "old-cors", Slot: "cors", By: "r-repl", Reason: hub.Replaced},
		{Policy: "odd-removal", Slot: "x", Reason: 0},
	}
	b := newBuilder()
	b.policy("old-cors", v1alpha1.PolicyTypeCORS, nil)
	unmaterialized := b.policy("ex", v1alpha1.PolicyTypeAuthJWT, nil)
	unmaterialized.Spec.FilterClass = nil
	odd := b.policy("odd-removal", v1alpha1.PolicyTypeRateLimit, nil)
	odd.Spec.FilterClass, odd.Spec.Type = nil, "no.such.type"

	r := mustNew(t, nil, Options{})
	var got [][5]string
	for _, r := range r.Rows(c, b.bundle()) {
		got = append(got, [5]string{r.Phase, r.Leg, r.Policy, r.From, r.Reason})
	}
	want := [][5]string{
		{"onRequestHeaders", "client", "g-own", "Gateway", "own slot"},
		{"onRequestHeaders", "client", "g-own-locked", "Gateway", "own slot, not overridable"},
		{"onRequestHeaders", "client", "g-slot", "Gateway", "slot shared, inherited"},
		{"onRequestHeaders", "client", "g-slot-locked", "Gateway", "slot auth, inherited, not overridable"},
		{"onRequestHeaders", "client", "r-repl", "Route", "slot cors, replaces old-cors"},
		{"onRequestHeaders", "client", "r-own", "Route", "own slot"},
		{"onRequestHeaders", "client", "r-slot", "Route", "slot custom-slot"},
		{"onRequestHeaders", "client", "r-own-locked", "Route", "own slot"},
		{"onRequestHeaders", "client", "r-slot-locked", "Route", "slot other-slot"},
		{"onRequestHeaders", "client", "r-repl-locked", "Route", "slot cors2, replaces old-cors2"},
		{"onRequestHeaders", "client", "odd", "", ""},
		{"onRequestHeaders", "orders", "u", "Upstream orders", "leg orders only"},
		{"onUpstreamRequest", "orders", "u2", "Upstream orders", "leg orders only"},
		{"onUpstreamRequest", "orders", "u-locked", "Upstream orders", "leg orders only"},
		{"onLog", "client", "late", "Route", "own slot"},
		{"none", "none", "odd-removal", "Gateway", ""},
		{"none", "none", "old-cors", "Gateway", "replaced in slot cors by r-repl"},
		{"none", "none", "ex", "Gateway", "excluded by Route"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows:\n got %q\nwant %q", got, want)
	}

	rows := r.Rows(c, b.bundle())
	removed := rows[len(rows)-3:]
	if r := removed[1]; r.Type != "cors" || r.FilterClass != "cors" || r.Slot != "cors" {
		t.Errorf("materialized removed row = %+v", r)
	}
	if r := removed[2]; r.Type != "auth.jwt" || r.FilterClass != "auth" {
		t.Errorf("removed row without filterClass = %+v, want the registry class", r)
	}
	if r := removed[0]; r.Type != "no.such.type" || r.FilterClass != "" {
		t.Errorf("removed row of an unknown type = %+v", r)
	}
	if r := r.Rows(c, nil)[len(rows)-1]; r.Type != "" || r.FilterClass != "" {
		t.Errorf("removed row without a Bundle = %+v", r)
	}
	if !slices.Equal(c.Removed, []hub.Removed{
		{Policy: "ex", Slot: "ex", Reason: hub.Excluded},
		{Policy: "old-cors", Slot: "cors", By: "r-repl", Reason: hub.Replaced},
		{Policy: "odd-removal", Slot: "x", Reason: 0},
	}) {
		t.Error("Rows reordered the chain's Removed")
	}
	if r := rows[0]; r.Type != "headers" || r.FilterClass != "transform" || r.Slot != "g-own" {
		t.Errorf("entry row = %+v", r)
	}
}

// policyRow returns the row of Policy name in rows, and whether there is
// one.
func policyRow(rows []Row, name string) (Row, bool) {
	for _, r := range rows {
		if r.Policy == name {
			return r, true
		}
	}
	return Row{}, false
}

// classOf returns a spec modifier that authors spec.filterClass c.
func classOf(c v1alpha1.FilterClass) func(*v1alpha1.PolicySpec) {
	return func(p *v1alpha1.PolicySpec) { p.FilterClass = &c }
}

// liveAndRemoved attaches the Policy x of b at the Gateway, resolves a
// Route that keeps it and one that excludes it, and returns x's live row
// and its removed row.
func liveAndRemoved(t *testing.T, b *builder) (live, removed Row) {
	t.Helper()
	b.gateway("x")
	b.route("keep", nil, nil, nil)
	b.route("drop", nil, []string{"x"}, nil)
	bundle := b.bundle()
	r := mustNew(t, nil, Options{Body: bodyOracle{}})
	chains, ds, err := r.Run(t.Context(), bundle, nil, 1)
	if err != nil || len(ds) != 0 {
		t.Fatalf("Run: %v\n%s", err, diagText(t, ds))
	}
	live, ok := policyRow(r.Rows(chains["keep"], bundle), "x")
	if !ok {
		t.Fatalf("no live row of x: %+v", chains["keep"])
	}
	removed, ok = policyRow(r.Rows(chains["drop"], bundle), "x")
	if !ok || removed.Phase != None || removed.Reason != "excluded by Route" {
		t.Fatalf("removed row of x = %+v, %v", removed, ok)
	}
	return live, removed
}

// TestRowsRemovedClassMatchesLive covers the WP-38 S2 finding on Rows:
// removed rows took their Filter class from a second registry.New() and
// from the raw spec.filterClass, so they could disagree with the class
// Resolve gives the same Policy as a live entry. Each filterClass shape
// gets its own case, and every registered type the Gateway may attach is
// checked, so the removed row always prints the live row's class (02 req
// 51; one registry per Resolver).
func TestRowsRemovedClassMatchesLive(t *testing.T) {
	cases := []struct {
		name string
		typ  v1alpha1.PolicyType
		mods []func(*v1alpha1.PolicySpec)
		post func(*v1alpha1.Policy)
		want string
	}{
		{name: "materialized registry class", typ: v1alpha1.PolicyTypeCORS, want: "cors"},
		{
			name: "non-plugin type with another authored filterClass", typ: v1alpha1.PolicyTypeCORS,
			mods: []func(*v1alpha1.PolicySpec){classOf(v1alpha1.FilterClassCustom)}, want: "cors",
		},
		{
			name: "plugin with a Filter class", typ: v1alpha1.PolicyTypePlugin,
			mods: []func(*v1alpha1.PolicySpec){pluginOf("pl", v1alpha1.FilterClassAuth)}, want: "auth",
		},
		{
			name: "plugin with a filterClass that is no Filter class", typ: v1alpha1.PolicyTypePlugin,
			mods: []func(*v1alpha1.PolicySpec){pluginOf("pl", "bogus")}, want: "custom",
		},
		{
			name: "plugin without filterClass", typ: v1alpha1.PolicyTypePlugin,
			mods: []func(*v1alpha1.PolicySpec){pluginOf("pl", v1alpha1.FilterClassAuthz)},
			post: func(p *v1alpha1.Policy) { p.Spec.FilterClass = nil }, want: "custom",
		},
		{
			name: "unmaterialized filterClass", typ: v1alpha1.PolicyTypeAuthJWT,
			post: func(p *v1alpha1.Policy) { p.Spec.FilterClass = nil }, want: "auth",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuilder()
			b.plugin("pl", v1alpha1.PhaseOnRequestHeaders)
			p := b.policy("x", tc.typ, nil, tc.mods...)
			if tc.post != nil {
				tc.post(p)
			}
			live, removed := liveAndRemoved(t, b)
			if live.FilterClass != tc.want || removed.FilterClass != tc.want {
				t.Errorf("FilterClass live %q, removed %q, want %q", live.FilterClass, removed.FilterClass, tc.want)
			}
			if removed.Type != live.Type || removed.Slot != live.Slot {
				t.Errorf("removed row %+v disagrees with live row %+v", removed, live)
			}
		})
	}

	for _, e := range registry.New().Entries() {
		if !e.Scopes.Has(phase.ScopeGateway) {
			continue
		}
		t.Run("type "+string(e.Type), func(t *testing.T) {
			b := newBuilder()
			b.plugin("pl", v1alpha1.PhaseOnRequestHeaders)
			var cfg any
			var mods []func(*v1alpha1.PolicySpec)
			switch e.Type {
			case v1alpha1.PolicyTypeHeaders:
				cfg = headersCfg(true, true)
			case v1alpha1.PolicyTypeTransformRequest:
				cfg = transformReqCfg("x")
			case v1alpha1.PolicyTypeTransformResponse:
				cfg = &v1alpha1.TransformResponseConfig{Body: "x"}
			case v1alpha1.PolicyTypeAuthzCEL:
				cfg = authzCfg("auth.ok")
			case v1alpha1.PolicyTypePlugin:
				mods = append(mods, pluginOf("pl", v1alpha1.FilterClassValidation))
			default:
			}
			b.policy("x", e.Type, cfg, mods...)
			live, removed := liveAndRemoved(t, b)
			if live.FilterClass == "" || removed.FilterClass != live.FilterClass {
				t.Errorf("FilterClass live %q, removed %q", live.FilterClass, removed.FilterClass)
			}
		})
	}
}

// TestRowsUpstreamsNamedLikeLabels pins the rows of Upstreams named
// "client" and "none", which the metadata.name pattern allows and which
// equal the Leg labels of client-leg and removed rows (WP-38 S2 round-2
// minor; 02 req 51 and 52 fix Leg as "client" or the Upstream name, so the
// label change is a contract request). Until it lands, (Phase, Leg, From)
// identifies every row: a leg row's From is "Upstream <Leg>", a client
// row's From is Gateway or Route with Leg "client", and a removed row has
// Phase and Leg "none" with From Gateway.
func TestRowsUpstreamsNamedLikeLabels(t *testing.T) {
	b := newBuilder()
	b.policy("h-client", v1alpha1.PolicyTypeHeaders, headersCfg(true, true))
	b.policy("h-gone", v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
	b.policy("h-up", v1alpha1.PolicyTypeHeaders, headersCfg(true, true))
	b.policy("h-none", v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
	b.gateway("h-client", "h-gone")
	b.upstream("client", "h-up")
	b.upstream("none", "h-none")
	b.route("r", nil, []string{"h-gone"}, []string{"none", "client"})
	bundle := b.bundle()
	r := mustNew(t, nil, Options{})
	chains, ds, err := r.Run(t.Context(), bundle, nil, 1)
	if err != nil || len(ds) != 0 {
		t.Fatalf("Run: %v %v", err, ds)
	}
	var got [][4]string
	for _, row := range r.Rows(chains["r"], bundle) {
		got = append(got, [4]string{row.Phase, row.Leg, row.Policy, row.From})
	}
	want := [][4]string{
		{"onRequestHeaders", "client", "h-client", "Gateway"},
		{"onUpstreamRequest", "client", "h-up", "Upstream client"},
		{"onUpstreamRequest", "none", "h-none", "Upstream none"},
		{"onUpstreamResponseHeaders", "client", "h-up", "Upstream client"},
		{"onResponse", "client", "h-client", "Gateway"},
		{"none", "none", "h-gone", "Gateway"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rows:\n got %q\nwant %q", got, want)
	}
	for _, row := range got {
		phaseName, leg, from := row[0], row[1], row[3]
		kinds := 0
		if from == "Upstream "+leg && phaseName != None {
			kinds++ // leg row
		}
		if leg == LegClient && (from == "Gateway" || from == "Route") && phaseName != None {
			kinds++ // client-leg row
		}
		if phaseName == None && leg == None && from == "Gateway" {
			kinds++ // removed row
		}
		if kinds != 1 {
			t.Errorf("row %q matches %d row kinds, want exactly 1", row, kinds)
		}
	}
}
