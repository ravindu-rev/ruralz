// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestRowsReasons covers every reason template of 02 req 51 and the row
// order: Phases in order, client leg before the legs, removed rows last
// with replaced before excluded whatever Chain.Removed's order.
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
		e("odd", phase.ScopeNone, "odd", true, ""),
	}
	c.Client[phase.OnLog] = []hub.Entry{e("late", phase.ScopeRoute, "late", true, "")}
	leg := hub.Leg{Upstream: "orders"}
	leg.Phases[phase.OnRequestHeaders] = []hub.Entry{e("u", phase.ScopeUpstream, "u", true, "")}
	leg.Phases[phase.OnUpstreamRequest] = []hub.Entry{e("u2", phase.ScopeUpstream, "u2", true, "")}
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

	var got [][5]string
	for _, r := range Rows(c, b.bundle()) {
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
		{"onRequestHeaders", "client", "odd", "", ""},
		{"onRequestHeaders", "orders", "u", "Upstream orders", "leg orders only"},
		{"onUpstreamRequest", "orders", "u2", "Upstream orders", "leg orders only"},
		{"onLog", "client", "late", "Route", "own slot"},
		{"none", "none", "odd-removal", "Gateway", ""},
		{"none", "none", "old-cors", "Gateway", "replaced in slot cors by r-repl"},
		{"none", "none", "ex", "Gateway", "excluded by Route"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows:\n got %q\nwant %q", got, want)
	}

	rows := Rows(c, b.bundle())
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
	if r := Rows(c, nil)[len(rows)-1]; r.Type != "" || r.FilterClass != "" {
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
