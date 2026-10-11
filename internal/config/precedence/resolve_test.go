// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// resolve runs Run with the test oracle and fails on an error.
func resolve(t *testing.T, b *hub.Bundle) (map[string]*hub.Chain, diag.List) {
	t.Helper()
	chains, ds, err := New(nil, Options{Body: bodyOracle{}}).Run(t.Context(), b, 1)
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

// TestCompilerIsBodyOracle checks that the CEL compiler contract
// satisfies the oracle Options.Body takes (architecture 1.2 injection).
func TestCompilerIsBodyOracle(t *testing.T) {
	var c expr.Compiler
	if o := (Options{Body: registry.BodyOracle(c)}); o.Body != nil {
		t.Fatal("a nil Compiler must be a nil oracle")
	}
}

// TestPhaseSelection covers 02 test 17 (02 req 13 and 13a; 07 req 2).
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
	for _, r := range Rows(c, nil) {
		if r.Policy == "hdr-empty" {
			t.Errorf("an empty headers config got a row: %+v", r)
		}
	}
}

// TestOrdering covers 02 test 18 and 02 req 35.5, 04 req 40: class, then
// scope, then position; response Phases reversed; onLog in request
// order; two Route ratelimit Policies keep list order.
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

// TestUpstreamLegOrder covers 02 req 35.4 and 41: leg Policies only in
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

// TestReplaceAndExclude covers 02 req 35.1, 35.2 and 36 and R-16: the
// Removed list and Replaces, exclusion before replacement, an exclusion
// of a Policy the Gateway does not attach, and a locked Policy that
// stays in the chain after both RZ-CFG-019 findings.
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

// TestSlotsPerScope covers 02 req 35.3, 35.4 and R-15: RZ-CFG-018 within
// one list only (Upstream lists take no part in slot comparison), the
// Gateway and Upstream lists checked once however many Routes use them,
// and RZ-CFG-020 dropping the entry without a cascade.
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

// TestCacheGuardrail covers 02 req 39 beyond the fixtures: a
// Gateway-scoped authz.cel reading the body, an onRequestBody
// transform.request (not covered by the rule) and a plugin auth Policy.
func TestCacheGuardrail(t *testing.T) {
	tests := []struct {
		name    string
		gateway []string
		route   []string
		want    int
	}{
		{"gateway authz reads the body", []string{"authz-body"}, []string{"cache"}, 1},
		{"transform.request is not covered", nil, []string{"cache", "xform"}, 0},
		{"plugin auth in onRequestBody", nil, []string{"cache", "plugin-auth"}, 1},
		{"plugin custom in onRequestBody", nil, []string{"cache", "plugin-custom"}, 0},
		{"no cache", []string{"authz-body"}, []string{"validate"}, 0},
		{"validation without cache, cache elsewhere", nil, []string{"validate"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuilder()
			b.plugin("body", v1alpha1.PhaseOnRequestBody)
			b.policy("cache", v1alpha1.PolicyTypeCache, nil)
			b.policy("authz-body", v1alpha1.PolicyTypeAuthzCEL, authzCfg("request.body.x"))
			b.policy("xform", v1alpha1.PolicyTypeTransformRequest, transformReqCfg("request.body"))
			b.policy("validate", v1alpha1.PolicyTypeValidationJSONSchema, nil)
			b.policy("plugin-auth", v1alpha1.PolicyTypePlugin, nil, pluginOf("body", v1alpha1.FilterClassAuth))
			b.policy("plugin-custom", v1alpha1.PolicyTypePlugin, nil, pluginOf("body", v1alpha1.FilterClassCustom))
			b.gateway(tc.gateway...)
			b.route("r", tc.route, nil, nil)
			_, ds := resolve(t, b.bundle())
			if len(ds) != tc.want {
				t.Fatalf("got %v, want %d RZ-CFG-038", ds, tc.want)
			}
			for _, d := range ds {
				if d.Code != CodeCacheGuardrail || d.Path.String() != "spec.policies[name=cache]" || d.Resource.Name != "r" {
					t.Errorf("diagnostic %+v", d)
				}
			}
		})
	}
}

// TestUnresolvedReferences covers 02 req 2 and 40: missing Policies,
// Plugins and Upstreams are skipped without diagnostics, and every other
// attachment and Route is still checked.
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
// diagnostics: CR 20 (ErrNoBodyOracle, a config of the wrong type), an
// unknown type and a canceled context.
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
			chains, ds, err := New(nil, tc.opts).Run(t.Context(), tc.b, 2)
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
	chains, ds, err := New(nil, Options{Body: failingOracle{}}).Run(t.Context(), newBundle("a", v1alpha1.PolicyTypeAuthzCEL, authzCfg("(")), 1)
	if err != nil || len(ds) > 0 || len(chains["r"].Client[phase.OnRequestHeaders]) != 1 {
		t.Errorf("failing oracle: %v %v %v", chains, ds, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, w := range []int{1, 4} {
		if _, _, err := New(nil, Options{}).Resolve(ctx, newBundle("x", v1alpha1.PolicyTypeCORS, nil), w); !errors.Is(err, context.Canceled) {
			t.Errorf("workers %d: canceled Resolve err = %v", w, err)
		}
	}
}

// TestNilBundle covers the empty inputs.
func TestNilBundle(t *testing.T) {
	r := New(nil, Options{})
	chains, ds, err := r.Run(t.Context(), nil, 4)
	if err != nil || len(chains) != 0 || len(ds) != 0 {
		t.Errorf("Run(nil) = %v, %v, %v", chains, ds, err)
	}
	if ds := r.CheckPolicies(nil); ds != nil {
		t.Errorf("CheckPolicies(nil) = %v", ds)
	}
	if rows := Rows(nil, nil); rows != nil {
		t.Errorf("Rows(nil) = %v", rows)
	}
}

// TestCheckPolicies covers 02 req 37 on typed Policies: once per Policy,
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
	ds := New(nil, Options{}).CheckPolicies(b.bundle())
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
// worker count or scheduling (02 section 3 concurrency rules).
func TestWorkersDeterministic(t *testing.T) {
	b, files := loadBundle(t, shopBundle, "testdata/fixtures/019-exclude")
	r := New(nil, Options{Body: bodyOracle{}, Files: files})
	c1, d1, err1 := r.Run(t.Context(), b, 1)
	for _, w := range []int{2, 3, 16} {
		cw, dw, errw := r.Run(t.Context(), b, w)
		if err1 != nil || errw != nil || !reflect.DeepEqual(c1, cw) || !reflect.DeepEqual(d1, dw) {
			t.Errorf("workers %d differ from workers 1", w)
		}
	}
}

// TestConcurrentResolve shares one Resolver between goroutines (-race).
func TestConcurrentResolve(t *testing.T) {
	b, files := loadBundle(t, shopBundle)
	r := New(nil, Options{Body: bodyOracle{}, Files: files})
	want, _, err := r.Run(t.Context(), b, 1)
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			got, _, err := r.Run(t.Context(), b, 3)
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
