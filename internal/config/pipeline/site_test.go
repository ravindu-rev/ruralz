// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// placePath returns the resource kind, Policy type and key-aware path that
// a place's ID names: "Kind.a.b[]..." or "<type> config.a.b[]...", each
// list element written as its first index.
func placePath(id expr.PlaceID) (v1alpha1.Kind, v1alpha1.PolicyType, diag.Path) {
	s := string(id)
	var (
		kind  v1alpha1.Kind
		ptype v1alpha1.PolicyType
		path  diag.Path
		rest  string
	)
	if t, after, ok := strings.Cut(s, " config"); ok {
		kind, ptype, rest = v1alpha1.KindPolicy, v1alpha1.PolicyType(t), after
		path = diag.Path{diag.Field("spec"), diag.Field("config")}
	} else {
		k, after, _ := strings.Cut(s, ".")
		kind, rest = v1alpha1.Kind(k), "."+after
	}
	for _, seg := range strings.Split(strings.TrimPrefix(rest, "."), ".") {
		name, list := strings.CutSuffix(seg, "[]")
		path = append(path, diag.Field(name))
		if list {
			path = append(path, diag.Index(0))
		}
	}
	return kind, ptype, path
}

// TestPlaceOfEveryPlace covers PlaceOf for every place of expr.Places: the
// field it names maps back to it, list entries by key, index or item
// alike.
func TestPlaceOfEveryPlace(t *testing.T) {
	for _, p := range expr.Places() {
		kind, ptype, path := placePath(p.ID)
		if got, ok := PlaceOf(kind, ptype, path); !ok || got != p.ID {
			t.Errorf("PlaceOf(%s, %q, %s) = %q, %v; want %q", kind, ptype, path, got, ok, p.ID)
		}
		keyed := slices.Clone(path)
		for i, e := range keyed {
			if e.Kind == diag.ElemIndex {
				keyed[i] = diag.Keyed("name", "x")
			}
		}
		if got, ok := PlaceOf(kind, ptype, keyed); !ok || got != p.ID {
			t.Errorf("PlaceOf(%s, %q, %s) = %q, %v; want %q", kind, ptype, keyed, got, ok, p.ID)
		}
	}
}

// TestPlaceOfSpecialCases covers the add[] entries of a headers config,
// which share the place of its set[] entries, a Policy's own when beside
// its config, and fields that are no place.
func TestPlaceOfSpecialCases(t *testing.T) {
	path := func(fields ...string) diag.Path {
		var p diag.Path
		for _, f := range fields {
			if f == "[]" {
				p = append(p, diag.Keyed("name", "x"))
				continue
			}
			p = append(p, diag.Field(f))
		}
		return p
	}
	tests := []struct {
		kind  v1alpha1.Kind
		ptype v1alpha1.PolicyType
		path  diag.Path
		want  expr.PlaceID
	}{
		{v1alpha1.KindPolicy, v1alpha1.PolicyTypeHeaders, path("spec", "config", "request", "add", "[]", "valueExpression"), expr.PlaceHeadersRequestValue},
		{v1alpha1.KindPolicy, v1alpha1.PolicyTypeHeaders, path("spec", "config", "response", "add", "[]", "valueExpression"), expr.PlaceHeadersResponseValue},
		{v1alpha1.KindPolicy, v1alpha1.PolicyTypeRateLimit, path("spec", "when"), expr.PlacePolicyWhen},
		{v1alpha1.KindPolicy, v1alpha1.PolicyTypeTransformRequest, path("spec", "config", "add", "[]", "valueExpression"), ""},
		{v1alpha1.KindRoute, "", path("spec", "timeout"), ""},
		{v1alpha1.KindPolicy, v1alpha1.PolicyTypeCORS, path("spec", "config", "key"), ""},
		{v1alpha1.KindPolicy, v1alpha1.PolicyTypeRateLimit, path("spec"), ""},
	}
	for _, tc := range tests {
		got, ok := PlaceOf(tc.kind, tc.ptype, tc.path)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("PlaceOf(%s, %q, %s) = %q, %v; want %q", tc.kind, tc.ptype, tc.path, got, ok, tc.want)
		}
	}
}

// TestRouteSite covers the site of a Route field: the composition places
// carry the mode and the names of the steps before the step, in list
// order; any other place, a Route without a composition, and a step the
// composition does not hold have no earlier steps.
func TestRouteSite(t *testing.T) {
	route := &v1alpha1.Route{Spec: v1alpha1.RouteSpec{Composition: &v1alpha1.Composition{
		Mode:  v1alpha1.CompositionModeSequential,
		Steps: []v1alpha1.CompositionStep{{Name: "one"}, {Name: "two"}, {Name: "three"}},
	}}}
	tests := []struct {
		name  string
		route *v1alpha1.Route
		place expr.PlaceID
		step  string
		want  expr.Site
	}{
		{"first step", route, expr.PlaceStepWhen, "one", expr.Site{Place: expr.PlaceStepWhen, Mode: v1alpha1.CompositionModeSequential}},
		{"third step", route, expr.PlaceStepPathExpression, "three", expr.Site{Place: expr.PlaceStepPathExpression, Mode: v1alpha1.CompositionModeSequential, EarlierSteps: []string{"one", "two"}}},
		{"unknown step", route, expr.PlaceStepWhen, "four", expr.Site{Place: expr.PlaceStepWhen, Mode: v1alpha1.CompositionModeSequential}},
		{"match", route, expr.PlaceRouteMatchWhen, "", expr.Site{Place: expr.PlaceRouteMatchWhen}},
		{"no composition", &v1alpha1.Route{}, expr.PlaceStepWhen, "one", expr.Site{Place: expr.PlaceStepWhen}},
		{"no Route", nil, expr.PlaceStepWhen, "one", expr.Site{Place: expr.PlaceStepWhen}},
	}
	for _, tc := range tests {
		if got := RouteSite(tc.route, tc.place, tc.step); siteKey(got) != siteKey(tc.want) {
			t.Errorf("%s: RouteSite = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if got := stepOf(diag.Path{diag.Field("spec"), diag.Field("composition"), diag.Field("steps"), diag.Keyed("name", "two"), diag.Field("when")}); got != "two" {
		t.Errorf("stepOf = %q, want two", got)
	}
	if got := stepOf(diag.Path{diag.Field("spec"), diag.Field("match"), diag.Field("when")}); got != "" {
		t.Errorf("stepOf(match) = %q, want none", got)
	}
}

// TestPolicySite covers the site the snapshot compiler compiles a Policy
// field at: the scope and type; for Policy.spec.when the first Phase there
// and whether the Policy gates the request body there, which for
// transform.request is the registry's body rule; for a transform set[]
// valueExpression the result its entry's target takes; and a resource
// that is not a Policy gets the bare place.
func TestPolicySite(t *testing.T) {
	p := newPipeline(t, Options{})
	policy := func(typ v1alpha1.PolicyType, cfg any) *hub.Resource {
		return &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "p"}, Object: &v1alpha1.Policy{Spec: v1alpha1.PolicySpec{Type: typ}}, Config: cfg}
	}
	body := &v1alpha1.TransformRequestConfig{Body: "request.body"}
	headerOnly := &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{{Target: v1alpha1.TransformRequestTargetHeader, Name: "x", ValueExpression: `"y"`}}}
	readsBody := &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{{Target: v1alpha1.TransformRequestTargetHeader, Name: "x", ValueExpression: "string(request.body.id)"}}}
	removesBody := &v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{{Target: v1alpha1.TransformRequestTargetBody, Name: "secret"}}}
	badValue := &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{{Target: v1alpha1.TransformRequestTargetHeader, Name: "x", ValueExpression: "request.body +"}}}
	targets := &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{
		{Target: v1alpha1.TransformRequestTargetHeader, Name: "x", ValueExpression: `"h"`},
		{Target: v1alpha1.TransformRequestTargetQuery, Name: "q", ValueExpression: `"q"`},
		{Target: v1alpha1.TransformRequestTargetPath, ValueExpression: `"/p"`},
		{Target: v1alpha1.TransformRequestTargetBody, Name: "b", ValueExpression: "1"},
	}}
	response := &v1alpha1.TransformResponseConfig{Set: []v1alpha1.TransformResponseSet{
		{Target: v1alpha1.TransformResponseTargetHeader, Name: "x", ValueExpression: `"h"`},
		{Target: v1alpha1.TransformResponseTargetBody, Name: "b", ValueExpression: "response.body"},
	}}
	value := func(i int) diag.Path {
		return diag.Path{diag.Field("spec"), diag.Field("config"), diag.Field("set"), diag.Index(i), diag.Field("valueExpression")}
	}
	tests := []struct {
		name   string
		r      *hub.Resource
		place  expr.PlaceID
		path   diag.Path
		scope  phase.Scope
		phases phase.Set
		want   expr.Site
	}{
		{"when in a body Phase", policy(v1alpha1.PolicyTypeValidationJSONSchema, nil), expr.PlacePolicyWhen, nil, phase.ScopeRoute, phase.Of(phase.OnRequestBody),
			expr.Site{Place: expr.PlacePolicyWhen, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeValidationJSONSchema, FirstPhase: phase.OnRequestBody, GatesBody: true}},
		{"when in a response Phase", policy(v1alpha1.PolicyTypeHeaders, nil), expr.PlacePolicyWhen, nil, phase.ScopeGateway, phase.Of(phase.OnResponse),
			expr.Site{Place: expr.PlacePolicyWhen, Scope: phase.ScopeGateway, PolicyType: v1alpha1.PolicyTypeHeaders, FirstPhase: phase.OnResponse}},
		{"transform.request with a body", policy(v1alpha1.PolicyTypeTransformRequest, body), expr.PlacePolicyWhen, nil, phase.ScopeUpstream, phase.Of(phase.OnUpstreamRequest),
			expr.Site{Place: expr.PlacePolicyWhen, Scope: phase.ScopeUpstream, PolicyType: v1alpha1.PolicyTypeTransformRequest, FirstPhase: phase.OnUpstreamRequest, GatesBody: true}},
		{"transform.request removing a body member", policy(v1alpha1.PolicyTypeTransformRequest, removesBody), expr.PlacePolicyWhen, nil, phase.ScopeRoute, phase.Of(phase.OnRequestBody),
			expr.Site{Place: expr.PlacePolicyWhen, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeTransformRequest, FirstPhase: phase.OnRequestBody, GatesBody: true}},
		{"transform.request without a body need", policy(v1alpha1.PolicyTypeTransformRequest, headerOnly), expr.PlacePolicyWhen, nil, phase.ScopeRoute, phase.Of(phase.OnRequestBody),
			expr.Site{Place: expr.PlacePolicyWhen, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeTransformRequest, FirstPhase: phase.OnRequestBody}},
		{"transform.request value reading the body", policy(v1alpha1.PolicyTypeTransformRequest, readsBody), expr.PlacePolicyWhen, nil, phase.ScopeRoute, phase.Of(phase.OnRequestBody),
			expr.Site{Place: expr.PlacePolicyWhen, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeTransformRequest, FirstPhase: phase.OnRequestBody, GatesBody: true}},
		{"transform.request value that does not compile", policy(v1alpha1.PolicyTypeTransformRequest, badValue), expr.PlacePolicyWhen, nil, phase.ScopeRoute, phase.Of(phase.OnRequestBody),
			expr.Site{Place: expr.PlacePolicyWhen, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeTransformRequest, FirstPhase: phase.OnRequestBody}},
		{"transform.response when", policy(v1alpha1.PolicyTypeTransformResponse, response), expr.PlacePolicyWhen, nil, phase.ScopeRoute, phase.Of(phase.OnResponse),
			expr.Site{Place: expr.PlacePolicyWhen, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeTransformResponse, FirstPhase: phase.OnResponse}},
		{"header value", policy(v1alpha1.PolicyTypeTransformRequest, targets), expr.PlaceTransformRequestValue, value(0), phase.ScopeRoute, phase.Of(phase.OnRequestHeaders),
			expr.Site{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: expr.ResultString}},
		{"query value", policy(v1alpha1.PolicyTypeTransformRequest, targets), expr.PlaceTransformRequestValue, value(1), phase.ScopeRoute, phase.Of(phase.OnRequestHeaders),
			expr.Site{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: expr.ResultString}},
		{"path value", policy(v1alpha1.PolicyTypeTransformRequest, targets), expr.PlaceTransformRequestValue, value(2), phase.ScopeUpstream, phase.Of(phase.OnUpstreamRequest),
			expr.Site{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeUpstream, PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: expr.ResultString}},
		{"body value", policy(v1alpha1.PolicyTypeTransformRequest, targets), expr.PlaceTransformRequestValue, value(3), phase.ScopeRoute, phase.Of(phase.OnRequestBody),
			expr.Site{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: expr.ResultDyn}},
		{"value past the entries", policy(v1alpha1.PolicyTypeTransformRequest, targets), expr.PlaceTransformRequestValue, value(4), phase.ScopeRoute, phase.Of(phase.OnRequestBody),
			expr.Site{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeTransformRequest}},
		{"value without a path", policy(v1alpha1.PolicyTypeTransformRequest, targets), expr.PlaceTransformRequestValue, nil, phase.ScopeRoute, phase.Of(phase.OnRequestBody),
			expr.Site{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeTransformRequest}},
		{"response header value", policy(v1alpha1.PolicyTypeTransformResponse, response), expr.PlaceTransformResponseValue, value(0), phase.ScopeGateway, phase.Of(phase.OnResponse),
			expr.Site{Place: expr.PlaceTransformResponseValue, Scope: phase.ScopeGateway, PolicyType: v1alpha1.PolicyTypeTransformResponse, Result: expr.ResultString}},
		{"response body value", policy(v1alpha1.PolicyTypeTransformResponse, response), expr.PlaceTransformResponseValue, value(1), phase.ScopeUpstream, phase.Of(phase.OnUpstreamResponseBody),
			expr.Site{Place: expr.PlaceTransformResponseValue, Scope: phase.ScopeUpstream, PolicyType: v1alpha1.PolicyTypeTransformResponse, Result: expr.ResultDyn}},
		{"config field", policy(v1alpha1.PolicyTypeRateLimit, nil), expr.PlaceRateLimitKey, nil, phase.ScopeRoute, phase.Of(phase.OnRequestHeaders),
			expr.Site{Place: expr.PlaceRateLimitKey, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeRateLimit}},
		{"not a Policy", &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Object: &v1alpha1.Route{}}, expr.PlaceRouteMatchWhen, nil, phase.ScopeRoute, 0,
			expr.Site{Place: expr.PlaceRouteMatchWhen}},
	}
	for _, tc := range tests {
		if got := p.PolicySite(tc.r, tc.place, tc.path, tc.scope, tc.phases); siteKey(got) != siteKey(tc.want) {
			t.Errorf("%s: PolicySite = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// countingCEL counts the References calls that reach the compiler.
type countingCEL struct {
	expr.Compiler
	n atomic.Int64
}

// References counts the call and asks the compiler.
func (c *countingCEL) References(site expr.Site, src string) (expr.Refs, error) {
	c.n.Add(1)
	return c.Compiler.References(site, src)
}

// TestSitesReferencesCache covers the References answers one run keeps:
// a site and source reaches the compiler once, an error included, and a
// sites without a cache asks every time.
func TestSitesReferencesCache(t *testing.T) {
	p := newPipeline(t, Options{})
	c := &countingCEL{Compiler: p.cel}
	site := expr.Site{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: expr.ResultString}
	s := sites{cel: c, reg: p.reg, cache: &sync.Map{}}
	for range 3 {
		refs, err := s.References(site, "string(request.body.id)")
		if err != nil || !refs.RequestBody {
			t.Fatalf("References = %+v, %v; want a request body read", refs, err)
		}
		if _, err := s.References(site, "request.body +"); err == nil {
			t.Fatal("References of a broken source: no error")
		}
	}
	if got := c.n.Load(); got != 2 {
		t.Errorf("compiler References calls = %d, want 2", got)
	}
	bare := sites{cel: c, reg: p.reg}
	for range 2 {
		if _, err := bare.References(site, `"x"`); err != nil {
			t.Fatalf("References: %v", err)
		}
	}
	if got := c.n.Load(); got != 4 {
		t.Errorf("compiler References calls without a cache = %d, want 4", got)
	}
}

// TestSiteKeyDistinguishesSites covers the key of stage J's compile
// cache: sites that differ in any field, step names included however they
// split, get different keys.
func TestSiteKeyDistinguishesSites(t *testing.T) {
	sites := []expr.Site{
		{Place: expr.PlacePolicyWhen},
		{Place: expr.PlacePolicyWhen, Scope: phase.ScopeRoute},
		{Place: expr.PlacePolicyWhen, PolicyType: v1alpha1.PolicyTypeHeaders},
		{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnResponse},
		{Place: expr.PlacePolicyWhen, GatesBody: true},
		{Place: expr.PlaceTransformRequestValue},
		{Place: expr.PlaceTransformRequestValue, Result: expr.ResultString},
		{Place: expr.PlaceTransformRequestValue, Result: expr.ResultDyn},
		{Place: expr.PlaceStepWhen, Mode: v1alpha1.CompositionModeSequential},
		{Place: expr.PlaceStepWhen, Mode: v1alpha1.CompositionModeSequential, EarlierSteps: []string{"a", "b"}},
		{Place: expr.PlaceStepWhen, Mode: v1alpha1.CompositionModeSequential, EarlierSteps: []string{"ab"}},
		{Place: expr.PlaceStepWhen, Mode: v1alpha1.CompositionModeSequential, EarlierSteps: []string{"a1:b"}},
	}
	seen := map[string]int{}
	for i, s := range sites {
		k := siteKey(s)
		if j, dup := seen[k]; dup {
			t.Errorf("sites %d and %d share key %q", j, i, k)
		}
		seen[k] = i
	}
}
