// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// resolve runs Run with the test oracle and fails on an error.
func resolve(t *testing.T, b *hub.Bundle) (map[string]*hub.Chain, diag.List) {
	t.Helper()
	chains, ds, err := mustNew(t, nil, Options{Body: bodyOracle{}}).Run(t.Context(), b, nil, 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return chains, ds
}

func codes(ds diag.List) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Code
	}
	return out
}

// TestCompilerIsBodyOracle checks that the CEL compiler contract satisfies
// the oracle Options.Body takes.
func TestCompilerIsBodyOracle(t *testing.T) {
	var c expr.Compiler
	if o := (Options{Body: registry.BodyOracle(c)}); o.Body != nil {
		t.Fatal("a nil Compiler must be a nil oracle")
	}
}

// TestPhaseSelection checks the Phases each Policy runs in: headers by the
// parts they touch, transform.request on the upstream leg, authz.cel in
// onRequestBody only when its rule reads the body, and no row for an empty
// headers config.
func TestPhaseSelection(t *testing.T) {
	b := newBuilder()
	b.policy("hdr-resp", v1alpha1.PolicyTypeHeaders, headersCfg(false, true))
	b.policy("hdr-req", v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
	b.policy("hdr-both", v1alpha1.PolicyTypeHeaders, headersCfg(true, true))
	b.policy("hdr-empty", v1alpha1.PolicyTypeHeaders, &v1alpha1.HeadersConfig{})
	b.policy("xform", v1alpha1.PolicyTypeTransformRequest, transformReqCfg(`{"a": 1}`))
	b.policy("authz-body", v1alpha1.PolicyTypeAuthzCEL, authzCfg(`request.body.ok`))
	b.policy("authz-head", v1alpha1.PolicyTypeAuthzCEL, authzCfg(`auth.claims.ok`))
	b.gateway("hdr-resp")
	b.upstream("up", "hdr-req", "xform")
	b.route("r", []string{"hdr-both", "hdr-empty", "authz-body", "authz-head"}, nil, []string{"up"})
	chains, ds := resolve(t, b.bundle())
	if len(ds) > 0 {
		t.Fatalf("diagnostics: %v", ds)
	}
	c := chains["r"]
	want := map[phase.Phase][]string{
		phase.OnRequestHeaders: {"authz-head", "hdr-both"},
		phase.OnRequestBody:    {"authz-body"},
		phase.OnResponse:       {"hdr-both", "hdr-resp"},
	}
	for p := range phase.Count {
		if got := names(c.Client[p]); !slices.Equal(got, want[p]) && len(got)+len(want[p]) > 0 {
			t.Errorf("client %s = %q, want %q", p, got, want[p])
		}
	}
	leg := c.Legs[0]
	if got := names(leg.Phases[phase.OnUpstreamRequest]); !slices.Equal(got, []string{"hdr-req", "xform"}) {
		t.Errorf("leg onUpstreamRequest = %q", got)
	}
	for p := range phase.Count {
		if p != phase.OnUpstreamRequest && len(leg.Phases[p]) > 0 {
			t.Errorf("leg %s = %q, want none", p, names(leg.Phases[p]))
		}
	}
	for _, r := range mustNew(t, nil, Options{}).Rows(c, nil) {
		if r.Policy == "hdr-empty" {
			t.Errorf("an empty headers config got a row: %+v", r)
		}
	}
}

// TestOrdering covers the order within a Phase: class, then scope, then
// position; response Phases reversed; onLog in request order; two Route
// ratelimit Policies keep list order.
func TestOrdering(t *testing.T) {
	b := newBuilder()
	b.policy("quota-g", v1alpha1.PolicyTypeQuota, nil)
	b.policy("quota-r", v1alpha1.PolicyTypeQuota, nil)
	b.policy("rl-1", v1alpha1.PolicyTypeRateLimit, nil)
	b.policy("rl-2", v1alpha1.PolicyTypeRateLimit, nil)
	b.policy("cors-g", v1alpha1.PolicyTypeCORS, nil)
	b.policy("hdr-g", v1alpha1.PolicyTypeHeaders, headersCfg(true, true))
	b.policy("hdr-r", v1alpha1.PolicyTypeHeaders, headersCfg(true, true))
	b.policy("jwt", v1alpha1.PolicyTypeAuthJWT, nil)
	b.gateway("hdr-g", "quota-g", "cors-g")
	b.route("r", []string{"rl-1", "hdr-r", "quota-r", "rl-2", "jwt"}, nil, nil)
	chains, ds := resolve(t, b.bundle())
	if len(ds) > 0 {
		t.Fatalf("diagnostics: %v", ds)
	}
	c := chains["r"]
	tests := []struct {
		p    phase.Phase
		want []string
	}{
		{phase.OnRequestHeaders, []string{"cors-g", "jwt", "quota-g", "rl-1", "quota-r", "rl-2", "hdr-g", "hdr-r"}},
		{phase.OnResponse, []string{"hdr-r", "hdr-g", "cors-g"}},
		{phase.OnLog, []string{"quota-g", "quota-r"}},
	}
	for _, tc := range tests {
		if got := names(c.Client[tc.p]); !slices.Equal(got, tc.want) {
			t.Errorf("%s = %q, want %q", tc.p, got, tc.want)
		}
	}
}

// TestUpstreamLegOrder covers upstream legs: leg Policies only in
// upstream-leg Phases, sorted by class then position and reversed in
// response Phases; legs in first-appearance order, each once.
func TestUpstreamLegOrder(t *testing.T) {
	b := newBuilder()
	b.policy("oauth", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
	b.policy("hdr", v1alpha1.PolicyTypeHeaders, headersCfg(true, true))
	b.policy("xresp", v1alpha1.PolicyTypeTransformResponse, &v1alpha1.TransformResponseConfig{Body: "response.body"})
	b.upstream("b-up", "hdr", "oauth", "xresp")
	b.upstream("a-up")
	b.upstream("unused", "oauth")
	b.route("plain", nil, nil, []string{"b-up", "a-up", "b-up", "missing"})
	comp := b.route("composed", nil, nil, nil)
	comp.Spec.Composition = &v1alpha1.Composition{Mode: v1alpha1.CompositionModeSequential, Steps: []v1alpha1.CompositionStep{
		{Name: "one", Upstream: "b-up"}, {Name: "two", Upstream: "a-up"}, {Name: "three", Upstream: "b-up"},
	}}
	chains, ds := resolve(t, b.bundle())
	if len(ds) > 0 {
		t.Fatalf("diagnostics: %v", ds)
	}
	legs := func(c *hub.Chain) []string {
		var out []string
		for _, l := range c.Legs {
			out = append(out, l.Upstream)
		}
		return out
	}
	if got := legs(chains["plain"]); !slices.Equal(got, []string{"a-up", "b-up"}) {
		t.Errorf("plain legs = %q, want [a-up b-up] (by name, once, missing skipped)", got)
	}
	if got := legs(chains["composed"]); !slices.Equal(got, []string{"b-up", "a-up"}) {
		t.Errorf("composed legs = %q, want [b-up a-up] (authored step order)", got)
	}
	leg, ok := chains["composed"].Leg("b-up")
	if !ok {
		t.Fatal("no b-up leg")
	}
	if got := names(leg.Phases[phase.OnUpstreamRequest]); !slices.Equal(got, []string{"oauth", "hdr"}) {
		t.Errorf("onUpstreamRequest = %q, want [oauth hdr]", got)
	}
	if got := names(leg.Phases[phase.OnUpstreamResponseHeaders]); !slices.Equal(got, []string{"hdr"}) {
		t.Errorf("onUpstreamResponseHeaders = %q", got)
	}
	if got := names(leg.Phases[phase.OnUpstreamResponseBody]); !slices.Equal(got, []string{"xresp"}) {
		t.Errorf("onUpstreamResponseBody = %q", got)
	}
	for p := range phase.Count {
		for _, e := range leg.Phases[p] {
			if !p.UpstreamLeg() || e.Scope != phase.ScopeUpstream {
				t.Errorf("leg entry %s in %s at scope %s", e.Policy, p, e.Scope)
			}
		}
		if len(chains["composed"].Client[p]) > 0 {
			t.Errorf("client %s holds upstream Policies", p)
		}
	}
	if _, ok := chains["plain"].Leg("unused"); ok {
		t.Error("an Upstream the Route does not reach became a leg")
	}
}

// TestReplaceAndExclude covers the Removed list and Replaces, exclusion
// before replacement, an exclusion of a Policy the Gateway does not attach,
// and a locked Policy that stays in the chain after both RZ-CFG-019
// findings.
func TestReplaceAndExclude(t *testing.T) {
	b := newBuilder()
	b.policy("cors-g", v1alpha1.PolicyTypeCORS, nil)
	b.policy("jwt-g", v1alpha1.PolicyTypeAuthJWT, nil)
	b.policy("rl-g", v1alpha1.PolicyTypeRateLimit, nil)
	b.policy("hdr-g", v1alpha1.PolicyTypeHeaders, headersCfg(false, true), slot("security"))
	b.policy("locked", v1alpha1.PolicyTypeHeaders, headersCfg(false, true), slot("locked-slot"), locked)
	b.policy("cors-r", v1alpha1.PolicyTypeCORS, nil)
	b.policy("key-r", v1alpha1.PolicyTypeAuthAPIKey, nil)
	b.policy("hdr-r", v1alpha1.PolicyTypeHeaders, headersCfg(false, true), slot("security"))
	b.policy("lock-r", v1alpha1.PolicyTypeHeaders, headersCfg(false, true), slot("locked-slot"))
	b.policy("other", v1alpha1.PolicyTypeRateLimit, nil)
	b.gateway("cors-g", "jwt-g", "rl-g", "hdr-g", "locked")
	// excludes hdr-g, then attaches hdr-r in its slot: stacks, no Replaces.
	b.route("r", []string{"key-r", "cors-r", "hdr-r", "lock-r"}, []string{"rl-g", "hdr-g", "locked", "other"}, nil)
	chains, ds := resolve(t, b.bundle())
	c := chains["r"]
	want := []hub.Removed{
		{Policy: "cors-g", Slot: "cors", By: "cors-r", Reason: hub.Replaced},
		{Policy: "jwt-g", Slot: "auth", By: "key-r", Reason: hub.Replaced},
		{Policy: "rl-g", Slot: "rl-g", Reason: hub.Excluded},
		{Policy: "hdr-g", Slot: "security", Reason: hub.Excluded},
	}
	if !slices.Equal(c.Removed, want) {
		t.Errorf("Removed = %+v\nwant %+v", c.Removed, want)
	}
	var got []string
	for _, d := range ds {
		got = append(got, d.Code+" "+d.Path.String())
	}
	wantDiags := []string{
		"RZ-CFG-019 spec.excludePolicies[name=locked]",
		"RZ-CFG-019 spec.policies[name=lock-r]",
	}
	if !slices.Equal(got, wantDiags) {
		t.Errorf("diagnostics = %q, want %q", got, wantDiags)
	}
	resp := names(c.Client[phase.OnResponse])
	if !slices.Equal(resp, []string{"hdr-r", "locked", "cors-r"}) {
		t.Errorf("onResponse = %q, want [hdr-r locked cors-r]", resp)
	}
	for _, e := range c.Client[phase.OnResponse] {
		if e.Policy == "hdr-r" && e.Replaces != "" {
			t.Errorf("hdr-r replaced %q after its Gateway twin was excluded", e.Replaces)
		}
		if e.Policy == "cors-r" && e.Replaces != "cors-g" {
			t.Errorf("cors-r Replaces = %q", e.Replaces)
		}
	}
	for _, d := range ds {
		if d.Resource == nil || d.Resource.Kind != "Route" || d.Resource.Name != "r" || len(d.Related) != 0 {
			t.Errorf("diagnostic %+v: want Route/r and no related location without positions", d)
		}
	}
}

// TestRepeatedEntries checks that a Policy named twice in one list raises
// no stage I finding on top of stage G's RZ-CFG-005 duplicate
// (spec.policies is an orderedMap and spec.excludePolicies a map, both
// keyed by name), since RZ-CFG-018 is for two distinct Policies: the second
// entry of a name is skipped silently, after the first decided, in every
// list stage I checks. One row per member: a valid entry repeated in the
// Gateway, a Route and an Upstream list (no RZ-CFG-018), an entry rejected
// with RZ-CFG-020 repeated at Gateway and Route scope (one RZ-CFG-020), a
// repeated exclusion of a locked Gateway Policy (one RZ-CFG-019, at the
// first) and of an overridable one (removed once). It also pins the chains
// exactly: the Gateway keeps the first rl, the Route twice runs [gl q] in
// class order, and the Route listed runs its own q once, at position 0,
// replacing the Gateway's.
func TestRepeatedEntries(t *testing.T) {
	b := newBuilder()
	b.policy("rl", v1alpha1.PolicyTypeRateLimit, nil)
	b.policy("q", v1alpha1.PolicyTypeQuota, nil)
	b.policy("gl", v1alpha1.PolicyTypeCORS, nil, locked)
	b.policy("c", v1alpha1.PolicyTypeCache, nil)
	b.policy("oa", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
	b.policy("h", v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
	b.gateway("rl", "q", "rl", "gl", "c", "c")
	b.upstream("up", "h", "h")
	b.route("twice", nil, []string{"rl", "rl"}, []string{"up"})
	b.route("locked", nil, []string{"gl", "gl"}, nil)
	b.route("listed", []string{"q", "q"}, nil, nil)
	b.route("scope", []string{"oa", "oa"}, nil, nil)
	for _, workers := range []int{1, 4} {
		chains, ds, err := mustNew(t, nil, Options{Body: bodyOracle{}}).Run(t.Context(), b.bundle(), nil, workers)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, d := range ds {
			got = append(got, d.Code+" "+d.Resource.Kind+"/"+d.Resource.Name+" "+d.Path.String())
		}
		want := []string{
			"RZ-CFG-019 Route/locked spec.excludePolicies[name=gl]",
			"RZ-CFG-020 Gateway/edge spec.policies[name=c]",
			"RZ-CFG-020 Route/scope spec.policies[name=oa]",
		}
		if !slices.Equal(got, want) {
			t.Errorf("workers %d: diagnostics:\n got %q\nwant %q", workers, got, want)
		}
		if want := []hub.Removed{{Policy: "rl", Slot: "rl", Reason: hub.Excluded}}; !slices.Equal(chains["twice"].Removed, want) {
			t.Errorf("twice: Removed = %+v, want %+v", chains["twice"].Removed, want)
		}
		if got := names(chains["twice"].Client[phase.OnRequestHeaders]); !slices.Equal(got, []string{"gl", "q"}) {
			t.Errorf("twice: onRequestHeaders = %q, want [gl q]", got)
		}
		if l, ok := chains["twice"].Leg("up"); !ok || !slices.Equal(names(l.Phases[phase.OnUpstreamRequest]), []string{"h"}) {
			t.Errorf("twice: leg up = %+v, want h once", l)
		}
		if got := chains["locked"].Removed; len(got) != 0 {
			t.Errorf("locked: Removed = %+v, want none", got)
		}
		if got := names(chains["locked"].Client[phase.OnRequestHeaders]); !slices.Equal(got, []string{"gl", "rl", "q"}) {
			t.Errorf("locked: onRequestHeaders = %q, want [gl rl q]: the Gateway's first rl only", got)
		}
		var own []hub.Entry
		for _, e := range chains["listed"].Client[phase.OnRequestHeaders] {
			if e.Policy == "q" {
				own = append(own, e)
			}
		}
		if len(own) != 1 || own[0].Scope != phase.ScopeRoute || own[0].Position != 0 || own[0].Replaces != "q" {
			t.Errorf("listed: q entries %+v, want the Route's first entry only, replacing the Gateway's q", own)
		}
		if want := []hub.Removed{{Policy: "q", Slot: "q", By: "q", Reason: hub.Replaced}}; !slices.Equal(chains["listed"].Removed, want) {
			t.Errorf("listed: Removed = %+v, want %+v", chains["listed"].Removed, want)
		}
	}
}

// TestRepeatedAfterFinding covers the repeated names TestRepeatedEntries
// leaves out: a name listed again after its first entry raised RZ-CFG-018,
// in the Gateway list, a Route list and an Upstream list, or after its
// first entry's replacement of a locked Gateway Policy was refused with
// RZ-CFG-019, raises nothing more, since a rejected first entry keeps its
// mark. Each list is positioned (entry i at line 10+i), so the one finding
// is shown to be at the first entry of the repeated name, on 1 and 4
// workers.
func TestRepeatedAfterFinding(t *testing.T) {
	for _, tc := range []struct {
		name, dir string
		// build adds the Policies and returns the resource whose list
		// spec.policies holds list.
		build func(b *builder, list []string) *hub.Resource
		list  []string
		want  string
	}{
		{"Gateway list: after RZ-CFG-018", "gateway", func(b *builder, list []string) *hub.Resource {
			b.policy("a", v1alpha1.PolicyTypeRateLimit, nil, slot("s"))
			b.policy("b", v1alpha1.PolicyTypeRateLimit, nil, slot("s"))
			b.gateway(list...)
			return b.rs[len(b.rs)-1]
		}, []string{"a", "b", "b"}, "gateway/edge.yaml:11:7 RZ-CFG-018 Gateway/edge spec.policies[name=b]"},
		{"Route list: after RZ-CFG-018", "routes", func(b *builder, list []string) *hub.Resource {
			b.policy("a", v1alpha1.PolicyTypeAuthJWT, nil)
			b.policy("b", v1alpha1.PolicyTypeAuthAPIKey, nil)
			b.route("r", list, nil, nil)
			return b.rs[len(b.rs)-1]
		}, []string{"a", "b", "b"}, "routes/r.yaml:11:7 RZ-CFG-018 Route/r spec.policies[name=b]"},
		{"Upstream list: after RZ-CFG-018", "upstreams", func(b *builder, list []string) *hub.Resource {
			b.policy("a", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
			b.policy("b", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
			b.upstream("u", list...)
			return b.rs[len(b.rs)-1]
		}, []string{"a", "b", "b"}, "upstreams/u.yaml:11:7 RZ-CFG-018 Upstream/u spec.policies[name=b]"},
		{"Route list: after a refused replacement (RZ-CFG-019)", "routes", func(b *builder, list []string) *hub.Resource {
			b.policy("gl", v1alpha1.PolicyTypeCORS, nil, locked)
			b.policy("rc", v1alpha1.PolicyTypeCORS, nil)
			b.gateway("gl")
			b.route("r", list, nil, nil)
			return b.rs[len(b.rs)-1]
		}, []string{"rc", "rc"}, "routes/r.yaml:10:7 RZ-CFG-019 Route/r spec.policies[name=rc]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuilder()
			owner := tc.build(b, tc.list)
			files := &tree.FileTable{}
			positionList(files, owner, tc.dir, fieldPolicies, tc.list)
			bundle := b.bundle()
			for _, workers := range []int{1, 4} {
				_, ds, err := mustNew(t, nil, Options{Body: bodyOracle{}}).Run(t.Context(), bundle, files, workers)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, d := range ds {
					got = append(got, fmt.Sprintf("%s:%d:%d %s %s/%s %s", d.File, d.Line, d.Column, d.Code, d.Resource.Kind, d.Resource.Name, d.Path))
				}
				if !slices.Equal(got, []string{tc.want}) {
					t.Errorf("workers %d: findings %q, want only %q", workers, got, tc.want)
				}
			}
		})
	}
}

// TestSlotsPerScope covers slots per scope: RZ-CFG-018 within one list only
// (Upstream lists take no part in slot comparison), the Gateway and
// Upstream lists checked once however many Routes use them, and RZ-CFG-020
// dropping the entry without a cascade.
func TestSlotsPerScope(t *testing.T) {
	b := newBuilder()
	b.policy("rl-a", v1alpha1.PolicyTypeRateLimit, nil, slot("limits"))
	b.policy("rl-b", v1alpha1.PolicyTypeRateLimit, nil, slot("limits"))
	b.policy("hdr", v1alpha1.PolicyTypeHeaders, headersCfg(true, false), slot("limits"))
	b.policy("cache", v1alpha1.PolicyTypeCache, nil)
	b.policy("cache2", v1alpha1.PolicyTypeCache, nil, slot("cache"))
	b.gateway("rl-a", "rl-b", "cache", "cache2")
	b.upstream("up", "hdr")
	b.upstream("lonely", "rl-a")
	for _, n := range []string{"r1", "r2", "r3"} {
		b.route(n, []string{"hdr"}, nil, []string{"up"})
	}
	chains, ds := resolve(t, b.bundle())
	var got []string
	for _, d := range ds {
		got = append(got, d.Code+" "+d.Resource.Kind+"/"+d.Resource.Name+" "+d.Path.String())
	}
	want := []string{
		"RZ-CFG-018 Gateway/edge spec.policies[name=rl-b]",
		"RZ-CFG-020 Gateway/edge spec.policies[name=cache]",
		"RZ-CFG-020 Gateway/edge spec.policies[name=cache2]",
		"RZ-CFG-020 Upstream/lonely spec.policies[name=rl-a]",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("diagnostics:\n got %q\nwant %q", got, want)
	}
	// Each Route's hdr replaces the overridable rl-a in slot limits.
	for _, n := range []string{"r1", "r2", "r3"} {
		if want := []hub.Removed{{Policy: "rl-a", Slot: "limits", By: "hdr", Reason: hub.Replaced}}; !slices.Equal(chains[n].Removed, want) {
			t.Errorf("%s Removed = %+v, want %+v", n, chains[n].Removed, want)
		}
	}
}

// TestSlotsPerScopeOverridable is TestSlotsPerScope's chain side: a Route
// Policy in the slot of an overridable Gateway Policy replaces it, and an
// Upstream Policy in the same slot does not.
func TestSlotsPerScopeOverridable(t *testing.T) {
	b := newBuilder()
	b.policy("rl-a", v1alpha1.PolicyTypeRateLimit, nil, slot("limits"))
	b.policy("hdr", v1alpha1.PolicyTypeHeaders, headersCfg(true, false), slot("limits"))
	b.gateway("rl-a")
	b.upstream("up", "hdr")
	b.route("r", nil, nil, []string{"up"})
	chains, ds := resolve(t, b.bundle())
	if len(ds) > 0 {
		t.Fatalf("diagnostics: %v", ds)
	}
	c := chains["r"]
	if got := names(c.Client[phase.OnRequestHeaders]); !slices.Equal(got, []string{"rl-a"}) {
		t.Errorf("onRequestHeaders = %q, want [rl-a]: an Upstream Policy takes no part in slot comparison", got)
	}
	if len(c.Removed) != 0 {
		t.Errorf("Removed = %+v", c.Removed)
	}
}

// TestCacheGuardrail covers RZ-CFG-038 beyond the fixtures: a
// Gateway-scoped authz.cel reading the body, an onRequestBody
// transform.request (not covered by the rule) and a plugin auth Policy.
func TestCacheGuardrail(t *testing.T) {
	tests := []struct {
		name     string
		gateway  []string
		route    []string
		excludes []string
		want     int
		// with is the onRequestBody Policy the finding names, if any.
		with string
	}{
		{"gateway authz reads the body", []string{"authz-body"}, []string{"cache"}, nil, 1, "authz-body"},
		{"transform.request is not covered", nil, []string{"cache", "xform"}, nil, 0, ""},
		{"plugin auth in onRequestBody", nil, []string{"cache", "plugin-auth"}, nil, 1, "plugin-auth"},
		{"plugin custom in onRequestBody", nil, []string{"cache", "plugin-custom"}, nil, 0, ""},
		{"no cache", []string{"authz-body"}, []string{"validate"}, nil, 0, ""},
		{"validation without cache, cache elsewhere", nil, []string{"validate"}, nil, 0, ""},
		// The shared onRequestBody of Routes removing the same Gateway
		// Policies (Resolve's memo) keeps the guardrail.
		{"one gateway authz excluded, another remains", []string{"authz-body", "authz-body2"}, []string{"cache"}, []string{"authz-body"}, 1, "authz-body2"},
		{"every gateway authz excluded", []string{"authz-body", "authz-body2"}, []string{"cache"}, []string{"authz-body", "authz-body2"}, 0, ""},
		{"route authz beside a shared gateway one", []string{"xform-g"}, []string{"cache", "authz-body"}, nil, 1, "authz-body"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuilder()
			b.plugin("body", v1alpha1.PhaseOnRequestBody)
			b.policy("cache", v1alpha1.PolicyTypeCache, nil)
			b.policy("authz-body", v1alpha1.PolicyTypeAuthzCEL, authzCfg("request.body.x"))
			b.policy("authz-body2", v1alpha1.PolicyTypeAuthzCEL, authzCfg("request.body.y"))
			b.policy("xform-g", v1alpha1.PolicyTypeTransformRequest, transformReqCfg("request.body"))
			b.policy("xform", v1alpha1.PolicyTypeTransformRequest, transformReqCfg("request.body"))
			b.policy("validate", v1alpha1.PolicyTypeValidationJSONSchema, nil)
			b.policy("plugin-auth", v1alpha1.PolicyTypePlugin, nil, pluginOf("body", v1alpha1.FilterClassAuth))
			b.policy("plugin-custom", v1alpha1.PolicyTypePlugin, nil, pluginOf("body", v1alpha1.FilterClassCustom))
			b.gateway(tc.gateway...)
			b.route("r", tc.route, tc.excludes, nil)
			b.route("inherit", nil, nil, nil)
			_, ds := resolve(t, b.bundle())
			if len(ds) != tc.want {
				t.Fatalf("got %v, want %d RZ-CFG-038", ds, tc.want)
			}
			for _, d := range ds {
				if d.Code != CodeCacheGuardrail || d.Path.String() != "spec.policies[name=cache]" || d.Resource.Name != "r" ||
					!strings.Contains(d.Message, fmt.Sprintf("with Policy %q", tc.with)) {
					t.Errorf("diagnostic %+v", d)
				}
			}
		})
	}
}

// TestUnresolvedReferences checks that missing Policies, Plugins and
// Upstreams are skipped without diagnostics, and every other attachment and
// Route is still checked.
func TestUnresolvedReferences(t *testing.T) {
	b := newBuilder()
	b.policy("rl", v1alpha1.PolicyTypeRateLimit, nil)
	b.policy("jwt", v1alpha1.PolicyTypeAuthJWT, nil)
	b.policy("key", v1alpha1.PolicyTypeAuthAPIKey, nil)
	b.policy("orphan-plugin", v1alpha1.PolicyTypePlugin, nil, pluginOf("gone", v1alpha1.FilterClassCustom))
	b.gateway("missing-g", "rl")
	b.upstream("up", "missing-u")
	b.route("broken", []string{"missing-r", "orphan-plugin", "jwt"}, []string{"missing-x"}, []string{"up", "missing-up"})
	b.route("bad", []string{"jwt", "key"}, nil, nil)
	b.add(v1alpha1.KindPolicy, "not-a-policy", &v1alpha1.Route{})
	b.route("odd", []string{"not-a-policy"}, nil, nil)
	chains, ds := resolve(t, b.bundle())
	if got := codes(ds); !slices.Equal(got, []string{CodeSlot}) || ds[0].Resource.Name != "bad" {
		t.Errorf("diagnostics = %v, want one RZ-CFG-018 on Route/bad", ds)
	}
	c := chains["broken"]
	if got := names(c.Client[phase.OnRequestHeaders]); !slices.Equal(got, []string{"jwt", "rl"}) {
		t.Errorf("broken onRequestHeaders = %q", got)
	}
	if got := c.Client[phase.OnRequestHeaders][0]; got.Position != 2 {
		t.Errorf("jwt Position = %d, want its authored index 2", got.Position)
	}
	if len(c.Legs) != 1 || c.Legs[0].Upstream != "up" {
		t.Errorf("broken legs = %+v", c.Legs)
	}
	if len(chains) != 3 {
		t.Errorf("chains = %d, want 3", len(chains))
	}
}

// TestNoGateway resolves Routes of a Bundle without a Gateway (RZ-CFG-016
// is stage C's).
func TestNoGateway(t *testing.T) {
	b := newBuilder()
	b.policy("rl", v1alpha1.PolicyTypeRateLimit, nil)
	b.route("r", []string{"rl"}, []string{"rl"}, nil)
	chains, ds := resolve(t, b.bundle())
	if len(ds) > 0 || len(chains["r"].Removed) > 0 {
		t.Fatalf("diagnostics %v, removed %v", ds, chains["r"].Removed)
	}
	if got := names(chains["r"].Client[phase.OnRequestHeaders]); !slices.Equal(got, []string{"rl"}) {
		t.Errorf("onRequestHeaders = %q", got)
	}
}

// TestEntryFields checks the materialized per-entry values (hub.Entry).
func TestEntryFields(t *testing.T) {
	b := newBuilder()
	b.plugin("p", v1alpha1.PhaseOnRequestHeaders, v1alpha1.PhaseOnLog)
	b.policy("plug", v1alpha1.PolicyTypePlugin, nil, pluginOf("p", v1alpha1.FilterClassAdmission), locked)
	b.policy("rl", v1alpha1.PolicyTypeRateLimit, nil, failOpen)
	b.gateway("rl", "plug")
	b.route("r", nil, nil, nil)
	chains, ds := resolve(t, b.bundle())
	if len(ds) > 0 {
		t.Fatalf("diagnostics %v", ds)
	}
	got := chains["r"].Client[phase.OnRequestHeaders]
	want := []hub.Entry{
		{
			Policy: "rl", Type: v1alpha1.PolicyTypeRateLimit, Class: phase.ClassAdmission, Slot: "rl", Scope: phase.ScopeGateway,
			FailureMode: v1alpha1.FailureModeOpen, Overridable: true, Phases: phase.Of(phase.OnRequestHeaders),
		},
		{
			Policy: "plug", Type: v1alpha1.PolicyTypePlugin, Class: phase.ClassAdmission, Slot: "plug", Scope: phase.ScopeGateway, Position: 1,
			FailureMode: v1alpha1.FailureModeClosed, Phases: phase.Of(phase.OnRequestHeaders, phase.OnLog),
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries:\n got %+v\nwant %+v", got, want)
	}
}

type failingOracle struct{}

func (failingOracle) ReferencesRequestBody(string) (bool, error) { return false, errors.New("no") }

// TestProgrammingErrors covers the errors Resolve returns instead of
// diagnostics: ErrNoBodyOracle, a config of the wrong type, an unknown type
// and a canceled context.
func TestProgrammingErrors(t *testing.T) {
	newBundle := func(name string, typ v1alpha1.PolicyType, cfg any) *hub.Bundle {
		b := newBuilder()
		b.policy(name, typ, cfg)
		b.gateway(name)
		b.route("r", nil, nil, nil)
		return b.bundle()
	}
	tests := []struct {
		name string
		b    *hub.Bundle
		opts Options
		want error
	}{
		{"authz.cel without oracle", newBundle("a", v1alpha1.PolicyTypeAuthzCEL, authzCfg("true")), Options{}, registry.ErrNoBodyOracle},
		{"unknown type", newBundle("u", "auth.unknown", &struct{}{}), Options{Body: bodyOracle{}}, registry.ErrUnknownType},
		{"config of another type", newBundle("c", v1alpha1.PolicyTypeCORS, authzCfg("true")), Options{Body: bodyOracle{}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chains, ds, err := mustNew(t, nil, tc.opts).Run(t.Context(), tc.b, nil, 2)
			if err == nil || chains != nil || ds != nil {
				t.Fatalf("Run = %v, %v, %v; want an error and no result", chains, ds, err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "precedence: ") {
				t.Errorf("err = %q, want the package prefix", err)
			}
		})
	}

	// An oracle error selects onRequestHeaders; stage J reports the rule.
	chains, ds, err := mustNew(t, nil, Options{Body: failingOracle{}}).Run(t.Context(), newBundle("a", v1alpha1.PolicyTypeAuthzCEL, authzCfg("(")), nil, 1)
	if err != nil || len(ds) > 0 || len(chains["r"].Client[phase.OnRequestHeaders]) != 1 {
		t.Errorf("failing oracle: %v %v %v", chains, ds, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, w := range []int{1, 4} {
		if _, _, err := mustNew(t, nil, Options{}).Resolve(ctx, newBundle("x", v1alpha1.PolicyTypeCORS, nil), nil, w); !errors.Is(err, context.Canceled) {
			t.Errorf("workers %d: canceled Resolve err = %v", w, err)
		}
	}
}

// TestNilBundle covers the empty inputs.
func TestNilBundle(t *testing.T) {
	r := mustNew(t, nil, Options{})
	chains, ds, err := r.Run(t.Context(), nil, nil, 4)
	if err != nil || len(chains) != 0 || len(ds) != 0 {
		t.Errorf("Run(nil) = %v, %v, %v", chains, ds, err)
	}
	if ds := r.CheckPolicies(nil, nil); ds != nil {
		t.Errorf("CheckPolicies(nil) = %v", ds)
	}
	if rows := r.Rows(nil, nil); rows != nil {
		t.Errorf("Rows(nil) = %v", rows)
	}
}

// TestCheckPolicies covers RZ-CFG-029 on typed Policies: once per Policy,
// attached or not, with the resource named and sorted output.
func TestCheckPolicies(t *testing.T) {
	b := newBuilder()
	b.policy("z-jwt", v1alpha1.PolicyTypeAuthJWT, nil, failOpen)
	b.policy("a-oauth", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil, failOpen)
	b.policy("rl", v1alpha1.PolicyTypeRateLimit, nil, failOpen)
	b.policy("plugin-auth", v1alpha1.PolicyTypePlugin, nil, pluginOf("p", v1alpha1.FilterClassAuth), failOpen)
	b.add(v1alpha1.KindPolicy, "weird", &v1alpha1.Route{})
	b.gateway("z-jwt", "z-jwt")
	b.route("r", []string{"z-jwt"}, nil, nil)
	ds := mustNew(t, nil, Options{}).CheckPolicies(b.bundle(), nil)
	var got []string
	for _, d := range ds {
		got = append(got, d.Code+" "+d.Resource.Name+" "+d.Path.String())
	}
	want := []string{
		"RZ-CFG-029 z-jwt spec.failureMode",
		"RZ-CFG-029 a-oauth spec.failureMode",
		"RZ-CFG-029 plugin-auth spec.failureMode",
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("CheckPolicies = %q, want %q", got, want)
	}
}

// TestWorkersDeterministic checks that the result never depends on the
// worker count or scheduling.
func TestWorkersDeterministic(t *testing.T) {
	b, files := fixture(t, shopEntry, "019-exclude")
	r := mustNew(t, nil, Options{Body: bodyOracle{}})
	c1, d1, err1 := r.Run(t.Context(), b, files, 1)
	for _, w := range []int{2, 3, 16} {
		cw, dw, errw := r.Run(t.Context(), b, files, w)
		if err1 != nil || errw != nil || !reflect.DeepEqual(c1, cw) || !reflect.DeepEqual(d1, dw) {
			t.Errorf("workers %d differ from workers 1", w)
		}
	}
}

// TestConcurrentResolve shares one Resolver between goroutines (-race).
func TestConcurrentResolve(t *testing.T) {
	b, files := loadShop(t)
	r := mustNew(t, nil, Options{Body: bodyOracle{}})
	want, _, err := r.Run(t.Context(), b, files, 1)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			got, _, err := r.Run(t.Context(), b, files, 3)
			if err == nil && !reflect.DeepEqual(got, want) {
				err = errors.New("concurrent result differs")
			}
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

// TestResolverAcrossLoads checks that a Resolver keeps no tree.FileTable of
// the load it was built for, so a Resolver reused for the next load (one
// Resolver per process) names the right files: one Resolver runs three
// loads in turn, each with its own FileTable, and every diagnostic names
// the files of its own load, as the fixtures' golden files (fresh
// Resolvers) do.
func TestResolverAcrossLoads(t *testing.T) {
	r := mustNew(t, nil, Options{Body: bodyOracle{}})
	for i, load := range [][2]string{
		{shopEntry, "019-exclude"},
		{common, "018-route-auth"},
		{common, "020-plugin-route"},
		{shopEntry, "019-exclude"},
	} {
		b, files := fixture(t, load[0], load[1])
		_, ds, err := r.Run(t.Context(), b, files, 2)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("testdata", "fixtures", load[1], "want.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if got := diagText(t, ds); got != string(want) {
			t.Errorf("load %d (%s): got %q, want %q", i, load[1], got, want)
		}
	}
}

// TestCacheGuardrailGatewayCache covers the guardrail's Gateway branch,
// which registry.New cannot reach (cache is a Route-only type, so a
// Gateway cache attachment is RZ-CFG-020 and leaves the chain): with
// selections injected as a registry allowing it would make them, a
// Gateway cache entry beside an onRequestBody authz entry is RZ-CFG-038
// owned by the Gateway, on every Route that keeps it and on none that
// excludes it.
func TestCacheGuardrailGatewayCache(t *testing.T) {
	b := newBuilder()
	b.gateway("c", "z")
	b.route("keep", nil, nil, nil)
	b.route("drop", nil, []string{"c"}, nil)
	bundle := b.bundle()
	s := newState(mustNew(t, nil, Options{}), bundle, nil, 1)
	w0 := s.pool.worker(0)
	s.sels[attachKey{policy: "c", scope: phase.ScopeGateway}] = &selection{slot: s.slotID(w0, "cache"), entry: hub.Entry{
		Policy: "c", Type: v1alpha1.PolicyTypeCache, Class: phase.ClassCache, Slot: "cache", Scope: phase.ScopeGateway,
		Overridable: true, Phases: phase.Of(phase.OnRequestHeaders, phase.OnResponse),
	}}
	s.sels[attachKey{policy: "z", scope: phase.ScopeGateway}] = &selection{slot: s.slotID(w0, "z"), entry: hub.Entry{
		Policy: "z", Type: v1alpha1.PolicyTypeAuthzCEL, Class: phase.ClassAuthz, Slot: "z", Scope: phase.ScopeGateway,
		Overridable: true, Phases: phase.Of(phase.OnRequestBody),
	}}
	s.attachGateway()
	if !slices.Equal(s.gatewayCache, []int{0}) {
		t.Fatalf("gatewayCache = %v, want [0]", s.gatewayCache)
	}
	for _, tc := range []struct {
		route string
		want  []string
	}{
		{"keep", []string{"RZ-CFG-038 Gateway/edge spec.policies[name=c]"}},
		{"drop", nil},
	} {
		res, _ := bundle.Get(hub.ID{Kind: v1alpha1.KindRoute, Name: tc.route})
		rt, _ := hub.Object[v1alpha1.Route](res)
		out := newCollector(keepOf(DefaultMaxDiagnostics), 1)
		w := s.r.newWorker(out)
		s.route(w, res, rt)
		w.flush()
		var got []string
		for _, d := range diagnostics(out.sorted(w)) {
			got = append(got, d.Code+" "+d.Resource.Kind+"/"+d.Resource.Name+" "+d.Path.String())
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.route, got, tc.want)
		}
	}
}

// TestRemovedOrder pins hub.Chain.Removed's order (replaced before
// excluded, each in Gateway list order) where Gateway order alone would
// differ: excluded Policies listed before replaced ones, and several of
// each.
func TestRemovedOrder(t *testing.T) {
	b := newBuilder()
	b.policy("rl-g", v1alpha1.PolicyTypeRateLimit, nil)
	b.policy("cors-g", v1alpha1.PolicyTypeCORS, nil)
	b.policy("quota-g", v1alpha1.PolicyTypeQuota, nil)
	b.policy("jwt-g", v1alpha1.PolicyTypeAuthJWT, nil)
	b.policy("cors-r", v1alpha1.PolicyTypeCORS, nil)
	b.policy("key-r", v1alpha1.PolicyTypeAuthAPIKey, nil)
	b.gateway("rl-g", "cors-g", "quota-g", "jwt-g")
	b.route("r", []string{"key-r", "cors-r"}, []string{"quota-g", "rl-g"}, nil)
	chains, ds := resolve(t, b.bundle())
	if len(ds) != 0 {
		t.Fatalf("diagnostics: %v", ds)
	}
	want := []hub.Removed{
		{Policy: "cors-g", Slot: "cors", By: "cors-r", Reason: hub.Replaced},
		{Policy: "jwt-g", Slot: "auth", By: "key-r", Reason: hub.Replaced},
		{Policy: "rl-g", Slot: "rl-g", Reason: hub.Excluded},
		{Policy: "quota-g", Slot: "quota-g", Reason: hub.Excluded},
	}
	if got := chains["r"].Removed; !slices.Equal(got, want) {
		t.Errorf("Removed = %+v\nwant %+v", got, want)
	}
}

// cancelingOracle cancels its context on its first call.
type cancelingOracle struct{ cancel context.CancelFunc }

func (o cancelingOracle) ReferencesRequestBody(string) (bool, error) {
	o.cancel()
	return false, nil
}

// TestResolveCanceledBetweenStages covers a ctx that ends after Phase
// selection has dispatched every unit (so selection itself completes):
// the next stage that has a unit to start, the Upstream lists or the
// Routes, observes it and Resolve returns ctx.Err() with no result.
func TestResolveCanceledBetweenStages(t *testing.T) {
	for _, tc := range []struct {
		name      string
		upstreams bool
	}{
		{"before the Upstream lists", true},
		{"before the Routes", false},
	} {
		for _, workers := range []int{1, 4} {
			t.Run(fmt.Sprintf("%s/workers=%d", tc.name, workers), func(t *testing.T) {
				b := newBuilder()
				b.policy("a", v1alpha1.PolicyTypeAuthzCEL, authzCfg("x"))
				b.gateway("a")
				if tc.upstreams {
					b.upstream("up")
				}
				b.route("r", nil, nil, nil)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				chains, ds, err := mustNew(t, nil, Options{Body: cancelingOracle{cancel: cancel}}).Run(ctx, b.bundle(), nil, workers)
				if !errors.Is(err, context.Canceled) || chains != nil || ds != nil {
					t.Errorf("Run = %v, %v, %v; want context.Canceled and no result", chains, ds, err)
				}
			})
		}
	}
}

// TestScopeErrorHoldsNoSlot pins the cascade rule of attachList: an entry
// reported RZ-CFG-020 holds no slot, so a later valid entry of the same
// list in its slot is no RZ-CFG-018 and runs, at every scope; and an entry
// after a valid one in its slot is RZ-CFG-020 when its type is not allowed
// there, never RZ-CFG-018 as well.
func TestScopeErrorHoldsNoSlot(t *testing.T) {
	tests := []struct {
		name string
		// rejected is not allowed at the scope; plugin shares its slot.
		rejected v1alpha1.PolicyType
		slot     string
		attach   func(b *builder, list []string)
		kind     string
		// phase is the plugin's Phase, one the scope allows.
		phase v1alpha1.Phase
	}{
		{"Gateway", v1alpha1.PolicyTypeValidationJSONSchema, "validation", func(b *builder, l []string) { b.gateway(l...) }, "Gateway", v1alpha1.PhaseOnRequestHeaders},
		{"Route", v1alpha1.PolicyTypeAuthUpstreamOAuth2, "upstream-auth", func(b *builder, l []string) { b.route("r", l, nil, nil) }, "Route", v1alpha1.PhaseOnRequestHeaders},
		{"Upstream", v1alpha1.PolicyTypeCORS, "cors", func(b *builder, l []string) { b.upstream("up", l...); b.route("r", nil, nil, []string{"up"}) }, "Upstream", v1alpha1.PhaseOnUpstreamRequest},
	}
	for _, tc := range tests {
		for _, order := range [][]string{{"bad", "pv"}, {"pv", "bad"}} {
			t.Run(fmt.Sprintf("%s/%v", tc.name, order), func(t *testing.T) {
				b := newBuilder()
				b.plugin("pl", tc.phase)
				b.policy("bad", tc.rejected, nil)
				b.policy("pv", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassCustom), slot(tc.slot))
				tc.attach(b, order)
				b.route("probe", nil, nil, nil)
				chains, ds := resolve(t, b.bundle())
				if len(ds) != 1 || ds[0].Code != CodeScope || ds[0].Resource.Kind != tc.kind || ds[0].Path.String() != "spec.policies[name=bad]" {
					t.Fatalf("findings %v, want one RZ-CFG-020 at spec.policies[name=bad] of the %s", ds, tc.kind)
				}
				c := chains["r"]
				if tc.name == "Gateway" {
					c = chains["probe"]
				}
				ran := false
				for p := range phase.Count {
					ran = ran || slices.Contains(names(c.Client[p]), "pv")
					for _, l := range c.Legs {
						ran = ran || slices.Contains(names(l.Phases[p]), "pv")
					}
				}
				if !ran {
					t.Errorf("pv, in the slot of the rejected entry, does not run: %+v", c)
				}
			})
		}
	}
}

// objectVariants are the hub.Resource.Object values without a typed view
// of a kind: a typed nil of the kind's type, a nil interface and another
// type, none of which may panic in newState, whatever the kind.
func objectVariants(k v1alpha1.Kind) map[string]any {
	typedNil := map[v1alpha1.Kind]any{
		v1alpha1.KindGateway: (*v1alpha1.Gateway)(nil), v1alpha1.KindRoute: (*v1alpha1.Route)(nil),
		v1alpha1.KindUpstream: (*v1alpha1.Upstream)(nil), v1alpha1.KindPolicy: (*v1alpha1.Policy)(nil),
		v1alpha1.KindPlugin: (*v1alpha1.Plugin)(nil),
	}[k]
	return map[string]any{"typed nil": typedNil, "nil interface": nil, "another type": &v1alpha1.Consumer{}}
}

// TestObjectWithoutTypedView covers that class for every kind stage I
// reads (Gateway, Route, Upstream, Policy, Plugin): such a resource is
// treated as absent, like an unresolved reference, so Run on 1 and 4
// workers returns exactly what it returns for the Bundle without the
// resource, with no panic and no error; CheckPolicies skips such a
// Policy, and Rows leaves its type empty.
func TestObjectWithoutTypedView(t *testing.T) {
	build := func(skip hub.ID, obj any) *hub.Bundle {
		b := newBuilder()
		b.plugin("pl", v1alpha1.PhaseOnRequestHeaders)
		b.policy("rl", v1alpha1.PolicyTypeRateLimit, nil)
		b.policy("jwt", v1alpha1.PolicyTypeAuthJWT, nil, failOpen)
		b.policy("h", v1alpha1.PolicyTypeHeaders, headersCfg(true, true))
		b.policy("q", v1alpha1.PolicyTypeQuota, nil)
		b.policy("pp", v1alpha1.PolicyTypePlugin, nil, pluginOf("pl", v1alpha1.FilterClassCustom))
		b.gateway("rl", "jwt")
		b.upstream("up", "h")
		b.route("r", []string{"q", "pp"}, nil, []string{"up"})
		b.route("r2", nil, []string{"rl"}, nil)
		for _, r := range b.rs {
			if r.ID == skip {
				r.Object = obj
			}
		}
		return b.bundle()
	}
	ids := []hub.ID{
		{Kind: v1alpha1.KindGateway, Name: "edge"},
		{Kind: v1alpha1.KindRoute, Name: "r"},
		{Kind: v1alpha1.KindUpstream, Name: "up"},
		{Kind: v1alpha1.KindPolicy, Name: "rl"},
		{Kind: v1alpha1.KindPolicy, Name: "jwt"},
		{Kind: v1alpha1.KindPlugin, Name: "pl"},
	}
	good := build(hub.ID{}, nil)
	goodChains := stageIChains(t, good)
	for _, id := range ids {
		absent := func() *hub.Bundle {
			var rs []*hub.Resource
			for _, r := range build(hub.ID{}, nil).Resources() {
				if r.ID != id {
					rs = append(rs, r)
				}
			}
			return hub.NewBundle(rs)
		}()
		for variant, obj := range objectVariants(id.Kind) {
			for _, workers := range []int{1, 4} {
				t.Run(fmt.Sprintf("%s %s/%s/workers=%d", id.Kind, id.Name, variant, workers), func(t *testing.T) {
					bad := build(id, obj)
					r := mustNew(t, nil, Options{Body: bodyOracle{}})
					chains, ds, err := r.Run(t.Context(), bad, nil, workers)
					if err != nil {
						t.Fatalf("Run: %v", err)
					}
					wantChains, wantDs, err := r.Run(t.Context(), absent, nil, workers)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(chains, wantChains) || !reflect.DeepEqual(ds, wantDs) {
						t.Errorf("Run differs from the Bundle without %s/%s:\n%v\n%v", id.Kind, id.Name, ds, wantDs)
					}
					if id.Kind == v1alpha1.KindPolicy {
						if got := r.CheckPolicies(bad, nil); slices.ContainsFunc(got, func(d diag.Diagnostic) bool { return d.Resource.Name == id.Name }) {
							t.Errorf("CheckPolicies reported %s: %v", id.Name, got)
						}
						for _, c := range goodChains {
							for _, row := range r.Rows(c, bad) {
								if row.Policy == id.Name && row.Phase == None && row.Type != "" {
									t.Errorf("removed row of %s = %+v, want no type", id.Name, row)
								}
							}
						}
					}
				})
			}
		}
	}
}

// stageIChains returns the chains of b.
func stageIChains(t *testing.T, b *hub.Bundle) map[string]*hub.Chain {
	t.Helper()
	chains, _, err := mustNew(t, nil, Options{Body: bodyOracle{}}).Run(t.Context(), b, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	return chains
}
