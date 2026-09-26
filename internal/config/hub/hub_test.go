// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for architecture section 2.9 (WP-01): NewBundle ordering and
// indexes (spec 02 section 2.2 requirements 4 to 8, canonical kind order),
// Chain.Leg, Entry.FirstPhase, the offline Checks table (R-34) and the
// secret-to-destination constant (R-49).

func res(kind v1alpha1.Kind, name string) *Resource {
	r := &Resource{ID: ID{Kind: kind, Name: name}}
	meta := v1alpha1.ObjectMeta{Name: name}
	switch kind {
	case v1alpha1.KindGateway:
		r.Object = &v1alpha1.Gateway{Metadata: meta}
	case v1alpha1.KindRoute:
		r.Object = &v1alpha1.Route{Metadata: meta}
	case v1alpha1.KindUpstream:
		r.Object = &v1alpha1.Upstream{Metadata: meta}
	case v1alpha1.KindPolicy:
		r.Object = &v1alpha1.Policy{Metadata: meta}
	case v1alpha1.KindConsumer:
		r.Object = &v1alpha1.Consumer{Metadata: meta}
	case v1alpha1.KindPlugin:
		r.Object = &v1alpha1.Plugin{Metadata: meta}
	case v1alpha1.KindAIProvider:
		r.Object = &v1alpha1.AIProvider{Metadata: meta}
	case v1alpha1.KindAIModel:
		r.Object = &v1alpha1.AIModel{Metadata: meta}
	default:
		// Environment and Cluster never reach a Bundle (RZ-CFG-017).
	}
	return r
}

func TestKindOrders(t *testing.T) {
	canonical := []v1alpha1.Kind{
		v1alpha1.KindGateway, v1alpha1.KindUpstream, v1alpha1.KindPlugin, v1alpha1.KindPolicy,
		v1alpha1.KindConsumer, v1alpha1.KindAIProvider, v1alpha1.KindAIModel, v1alpha1.KindRoute,
	}
	for i, k := range canonical {
		if KindOrder(k) != i {
			t.Errorf("KindOrder(%s) = %d, want %d", k, KindOrder(k), i)
		}
	}
	catalog := []v1alpha1.Kind{
		v1alpha1.KindGateway, v1alpha1.KindRoute, v1alpha1.KindUpstream, v1alpha1.KindPolicy,
		v1alpha1.KindPlugin, v1alpha1.KindConsumer, v1alpha1.KindAIProvider, v1alpha1.KindAIModel,
	}
	for i, k := range catalog {
		if CatalogOrder(k) != i {
			t.Errorf("CatalogOrder(%s) = %d, want %d", k, CatalogOrder(k), i)
		}
	}
	for _, k := range []v1alpha1.Kind{v1alpha1.KindEnvironment, v1alpha1.KindCluster, "Unknown", ""} {
		if KindOrder(k) != 99 || CatalogOrder(k) != 99 {
			t.Errorf("%q sorts at %d/%d, want last (99)", k, KindOrder(k), CatalogOrder(k))
		}
	}
}

func TestNewBundleOrderAndIndex(t *testing.T) {
	in := []*Resource{
		res(v1alpha1.KindRoute, "orders"),
		res(v1alpha1.KindPolicy, "rl"),
		res(v1alpha1.KindRoute, "cart"),
		res(v1alpha1.KindUpstream, "inventory"),
		res(v1alpha1.KindConsumer, "acme"),
		res(v1alpha1.KindPlugin, "geo"),
		res(v1alpha1.KindGateway, "main"),
		res(v1alpha1.KindAIModel, "chat"),
		res(v1alpha1.KindAIProvider, "openai"),
		res(v1alpha1.KindPolicy, "auth"),
		res(v1alpha1.KindUpstream, "billing"),
		{ID: ID{Kind: "Future", Name: "x"}},
	}
	inCopy := slices.Clone(in)
	b := NewBundle(in)
	if !slices.Equal(in, inCopy) {
		t.Fatal("NewBundle reordered its argument")
	}
	var got []string
	for _, r := range b.Resources() {
		got = append(got, r.String())
	}
	want := []string{
		"Gateway/main", "Upstream/billing", "Upstream/inventory", "Plugin/geo", "Policy/auth", "Policy/rl",
		"Consumer/acme", "AIProvider/openai", "AIModel/chat", "Route/cart", "Route/orders", "Future/x",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	for _, r := range in {
		if g, ok := b.Get(r.ID); !ok || g != r {
			t.Errorf("Get(%s) = %v, %v", r.ID, g, ok)
		}
	}
	if _, ok := b.Get(ID{Kind: v1alpha1.KindRoute, Name: "missing"}); ok {
		t.Error("Get of a missing resource succeeded")
	}
	// Names sort bytewise.
	b2 := NewBundle([]*Resource{res(v1alpha1.KindRoute, "b"), res(v1alpha1.KindRoute, "B"), res(v1alpha1.KindRoute, "a-1"), res(v1alpha1.KindRoute, "a")})
	var names []string
	for _, r := range b2.Routes() {
		names = append(names, r.Metadata.Name)
	}
	if !slices.Equal(names, []string{"B", "a", "a-1", "b"}) {
		t.Fatalf("Routes %v, want bytewise order", names)
	}
}

func TestNewBundleDuplicateKeepsFirst(t *testing.T) {
	first := res(v1alpha1.KindRoute, "orders")
	second := res(v1alpha1.KindRoute, "orders")
	b := NewBundle([]*Resource{first, res(v1alpha1.KindRoute, "cart"), second})
	if len(b.Resources()) != 2 {
		t.Fatalf("%d resources, want the duplicate dropped", len(b.Resources()))
	}
	if r, _ := b.Get(first.ID); r != first {
		t.Fatal("duplicate identity did not keep the first resource")
	}
	if b.Resources()[1] != first {
		t.Fatal("the kept duplicate is not the first one")
	}
}

func TestTypedAccessors(t *testing.T) {
	b := NewBundle([]*Resource{
		res(v1alpha1.KindGateway, "main"),
		res(v1alpha1.KindRoute, "r2"), res(v1alpha1.KindRoute, "r1"),
		res(v1alpha1.KindUpstream, "u1"),
		res(v1alpha1.KindPolicy, "p1"), res(v1alpha1.KindPolicy, "p0"),
		res(v1alpha1.KindConsumer, "c1"),
		res(v1alpha1.KindPlugin, "g1"),
		// A resource whose Object is not the kind's type is skipped.
		{ID: ID{Kind: v1alpha1.KindRoute, Name: "broken"}, Object: &v1alpha1.Upstream{}},
	})
	if g := b.Gateway(); g == nil || g.Metadata.Name != "main" {
		t.Fatalf("Gateway = %v", g)
	}
	if r, ok := b.Route("r1"); !ok || r.Metadata.Name != "r1" {
		t.Fatal("Route(r1) failed")
	}
	if _, ok := b.Route("broken"); ok {
		t.Fatal("Route(broken) returned a mistyped object")
	}
	if _, ok := b.Route("nope"); ok {
		t.Fatal("Route(nope) succeeded")
	}
	if u, ok := b.Upstream("u1"); !ok || u.Metadata.Name != "u1" {
		t.Fatal("Upstream(u1) failed")
	}
	if p, ok := b.Policy("p0"); !ok || p.Metadata.Name != "p0" {
		t.Fatal("Policy(p0) failed")
	}
	if c, ok := b.Consumer("c1"); !ok || c.Metadata.Name != "c1" {
		t.Fatal("Consumer(c1) failed")
	}
	if g, ok := b.Plugin("g1"); !ok || g.Metadata.Name != "g1" {
		t.Fatal("Plugin(g1) failed")
	}
	names := func(n int, name func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = name(i)
		}
		return out
	}
	routes, ups, pols, cons := b.Routes(), b.Upstreams(), b.Policies(), b.Consumers()
	if got := names(len(routes), func(i int) string { return routes[i].Metadata.Name }); !slices.Equal(got, []string{"r1", "r2"}) {
		t.Fatalf("Routes = %v", got)
	}
	if len(ups) != 1 || len(cons) != 1 {
		t.Fatalf("Upstreams %d, Consumers %d", len(ups), len(cons))
	}
	if got := names(len(pols), func(i int) string { return pols[i].Metadata.Name }); !slices.Equal(got, []string{"p0", "p1"}) {
		t.Fatalf("Policies = %v", got)
	}

	empty := NewBundle(nil)
	if empty.Gateway() != nil || len(empty.Resources()) != 0 || empty.Routes() != nil {
		t.Fatal("an empty Bundle is not empty")
	}
}

func TestObject(t *testing.T) {
	r := res(v1alpha1.KindRoute, "r")
	if got, ok := Object[v1alpha1.Route](r); !ok || got.Metadata.Name != "r" {
		t.Fatal("Object[Route] failed")
	}
	if _, ok := Object[v1alpha1.Gateway](r); ok {
		t.Fatal("Object[Gateway] of a Route succeeded")
	}
	if _, ok := Object[v1alpha1.Route](nil); ok {
		t.Fatal("Object of nil succeeded")
	}
}

func TestChainLeg(t *testing.T) {
	c := &Chain{Route: "orders", Legs: []Leg{{Upstream: "inventory"}, {Upstream: "pricing"}}}
	c.Legs[1].Phases[phase.OnUpstreamRequest] = []Entry{{Policy: "sign"}}
	leg, ok := c.Leg("pricing")
	if !ok || leg.Upstream != "pricing" || leg.Phases[phase.OnUpstreamRequest][0].Policy != "sign" {
		t.Fatalf("Leg(pricing) = %+v, %v", leg, ok)
	}
	// Leg returns a pointer into the chain, not a copy.
	if leg != &c.Legs[1] {
		t.Fatal("Leg returned a copy")
	}
	if _, ok := c.Leg("billing"); ok {
		t.Fatal("Leg(billing) succeeded")
	}
	if _, ok := (&Chain{}).Leg("x"); ok {
		t.Fatal("Leg on an empty chain succeeded")
	}
}

func TestEntryFirstPhase(t *testing.T) {
	tests := []struct {
		phases phase.Set
		want   phase.Phase
	}{
		{phase.Of(phase.OnRequestHeaders, phase.OnResponse), phase.OnRequestHeaders},
		{phase.Of(phase.OnResponse, phase.OnRequestBody), phase.OnRequestBody},
		{phase.Of(phase.OnUpstreamResponseBody), phase.OnUpstreamResponseBody},
		{phase.Of(phase.OnLog), phase.OnLog},
		{0, phase.OnRequestHeaders}, // no Phase: the zero Phase
	}
	for _, tt := range tests {
		if got := (Entry{Phases: tt.phases}).FirstPhase(); got != tt.want {
			t.Errorf("FirstPhase(%v) = %v, want %v", tt.phases.Phases(), got, tt.want)
		}
	}
}

func TestChecksTable(t *testing.T) {
	// R-34: a type package exports a hub.PolicyCheck; the pipeline calls it
	// with a location mapper and a sink, never importing internal/filter.
	check := func(r *Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
		p := diag.Path{diag.Field("spec"), diag.Field("config"), diag.Field("request"), diag.Field("set"), diag.Keyed("name", "host")}
		add(diag.Diagnostic{
			Code: "RZ-CFG-005", Severity: diag.SeverityError, Location: loc(p),
			Resource: r.ResourceID(), Path: p, Message: "protected header",
		})
	}
	checks := Checks{v1alpha1.PolicyTypeHeaders: check}

	pol := res(v1alpha1.KindPolicy, "hdr")
	var got diag.List
	var asked []string
	loc := func(p diag.Path) diag.Location {
		asked = append(asked, p.String())
		return diag.Location{File: "policies/hdr.yaml", Line: 7, Column: 9}
	}
	fn, ok := checks[v1alpha1.PolicyTypeHeaders]
	if !ok {
		t.Fatal("Checks lookup failed")
	}
	fn(pol, loc, func(d diag.Diagnostic) { got = append(got, d) })
	if len(got) != 1 || len(asked) != 1 || asked[0] != "spec.config.request.set[name=host]" {
		t.Fatalf("check produced %v, asked %v", got, asked)
	}
	want := "policies/hdr.yaml:7:9 error RZ-CFG-005 Policy/hdr spec.config.request.set[name=host]: protected header"
	if s := string(got[0].AppendText(nil)); s != want {
		t.Fatalf("diagnostic %s, want %s", s, want)
	}
	if _, ok := checks[v1alpha1.PolicyTypeRateLimit]; ok {
		t.Fatal("a type without a check is present")
	}
}

func TestDestinationLocalValue(t *testing.T) {
	// R-49: "every other secret field is `local`"; the value is printed by
	// RZ-CFG-041 messages and diff output, so it is pinned to the spelling
	// of the resolution.
	if DestinationLocal != "local" {
		t.Fatalf("DestinationLocal = %q, want %q", DestinationLocal, "local")
	}
}
