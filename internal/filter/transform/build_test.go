// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// codeOf returns the RZ code err carries, "" for none.
func codeOf(err error) string {
	code, _ := errcode.CodeOf(err)
	return code
}

// TestBuildPhase covers the one Phase a Filter acts in, by type and
// scope.
func TestBuildPhase(t *testing.T) {
	req := &v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{reqRemove("header", "x-a")}}
	resp := &v1alpha1.TransformResponseConfig{Remove: []v1alpha1.TransformResponseRemove{respRemove("header", "x-a")}}
	tests := []struct {
		cfg   any
		scope phase.Scope
		want  phase.Phase
	}{
		{req, phase.ScopeGateway, phase.OnRequestBody},
		{req, phase.ScopeRoute, phase.OnRequestBody},
		{req, phase.ScopeUpstream, phase.OnUpstreamRequest},
		{resp, phase.ScopeGateway, phase.OnResponse},
		{resp, phase.ScopeRoute, phase.OnResponse},
		{resp, phase.ScopeUpstream, phase.OnUpstreamResponseBody},
	}
	for _, tc := range tests {
		if got := build(t, tc.cfg, tc.scope).phase; got != tc.want {
			t.Errorf("Build(%T at %v).phase = %v, want %v", tc.cfg, tc.scope, got, tc.want)
		}
	}
}

// TestBuildRefusesFindings covers Build on a config with findings:
// RZ-CFG-005 naming the first one, schema findings included.
func TestBuildRefusesFindings(t *testing.T) {
	tests := []struct {
		cfg  any
		want string
	}{
		{
			&v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("path", "p", `"/a"`), reqSet("header", "host", `"a"`)}},
			"transform.request: config.set[0].name: target path takes no name",
		},
		{
			&v1alpha1.TransformResponseConfig{Set: []v1alpha1.TransformResponseSet{respSet("query", "a", `"b"`)}},
			`transform.response: config.set[0].target: target "query" is not header or body`,
		},
		{
			&v1alpha1.TransformResponseConfig{ContentType: ptr("json")},
			`transform.response: config.contentType: "json" is not an RFC 9110 media type`,
		},
	}
	for _, tc := range tests {
		_, err := Build(context.Background(), buildEnv(t, tc.cfg, phase.ScopeRoute), specOf(t, tc.cfg))
		if codeOf(err) != CodeConfig || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Build = %v (code %s), want %s with %q", err, codeOf(err), CodeConfig, tc.want)
		}
	}
	many := &v1alpha1.TransformResponseConfig{}
	for range MaxEntries + 1 {
		many.Replace = append(many.Replace, literally("", "a", "b"))
	}
	if _, err := Build(context.Background(), buildEnv(t, many, phase.ScopeRoute), specOf(t, many)); codeOf(err) != CodeConfig ||
		!strings.Contains(err.Error(), "transform.response: config: 33 entries") {
		t.Errorf("Build of 33 entries = %v", err)
	}
}

// TestBuildCompilesCEL covers the CEL fields: each compiles at its site,
// a set valueExpression at its target's result, so an int is refused for
// targets header, query and path and taken for target body, and a field
// that does not compile is RZ-CFG-014 or RZ-CFG-015 naming the field.
func TestBuildCompilesCEL(t *testing.T) {
	ok := []any{
		&v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("body", "n", "1"), reqSet("body", "m", "request.body")}},
		&v1alpha1.TransformRequestConfig{Body: `{"a": [1, 2.5, "x", null, true, b"\x00"]}`},
		&v1alpha1.TransformResponseConfig{Body: "response.body", Set: []v1alpha1.TransformResponseSet{respSet("header", "x-s", "string(response.status)")}},
	}
	for _, cfg := range ok {
		build(t, cfg, phase.ScopeRoute)
	}
	tests := []struct {
		name  string
		cfg   any
		scope phase.Scope
		code  string
		want  string
	}{
		{
			"header int", &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("header", "x-a", "1")}}, phase.ScopeRoute,
			CodeCEL, "transform.request: config.set[0].valueExpression: CEL transform.request config.set[].valueExpression: expression has type int, want string",
		},
		{
			"query int", &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("query", "q", "1")}}, phase.ScopeRoute,
			CodeCEL, "config.set[0].valueExpression",
		},
		{
			"path int", &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("path", "", "1")}}, phase.ScopeRoute,
			CodeCEL, "config.set[0].valueExpression",
		},
		{
			"response header int", &v1alpha1.TransformResponseConfig{Set: []v1alpha1.TransformResponseSet{respSet("header", "x-a", "response.status")}}, phase.ScopeRoute,
			CodeCEL, "config.set[0].valueExpression",
		},
		{"syntax", &v1alpha1.TransformRequestConfig{Body: "request.body +"}, phase.ScopeRoute, CodeCEL, "transform.request: config.body: CEL"},
		{"upstream outside Upstream scope", &v1alpha1.TransformResponseConfig{Body: "upstream.name"}, phase.ScopeRoute, CodeCEL, "undeclared reference to 'upstream'"},
		{"response in a request transform", &v1alpha1.TransformRequestConfig{Body: "response.body"}, phase.ScopeUpstream, CodeCEL, "undeclared reference to 'response'"},
		{"cost", &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("header", "x-a",
			`request.headers.all(a, request.headers.all(b, request.headers.all(c, a + b + c != ""))) ? "y" : "n"`)}}, phase.ScopeRoute, CodeCost, "config.set[0].valueExpression"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build(context.Background(), buildEnv(t, tc.cfg, tc.scope), specOf(t, tc.cfg))
			if codeOf(err) != tc.code || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Build = %v (code %s), want %s with %q", err, codeOf(err), tc.code, tc.want)
			}
		})
	}
	upstream := &v1alpha1.TransformResponseConfig{Body: `{"from": upstream.name}`}
	build(t, upstream, phase.ScopeUpstream)
}

// recordingBuilder records the sites it compiles at.
type recordingBuilder struct {
	expr.Builder
	mu    sync.Mutex
	sites []expr.Site
}

// Compile records site and compiles.
func (b *recordingBuilder) Compile(site expr.Site, src string) (expr.Program, []expr.Issue) {
	b.mu.Lock()
	b.sites = append(b.sites, site)
	b.mu.Unlock()
	return b.Builder.Compile(site, src)
}

// TestBuildSites covers the sites Build compiles at: BuildEnv.Site's site
// of each place, narrowed for a set valueExpression to its target's
// result, config.body keeping the place's dyn.
func TestBuildSites(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{
		Body: "request.body",
		Set: []v1alpha1.TransformRequestSet{
			reqSet("header", "x-a", `"a"`), reqSet("query", "q", `"b"`), reqSet("path", "", `"/c"`), reqSet("body", "d", "1"),
		},
	}
	env := buildEnv(t, cfg, phase.ScopeUpstream)
	rec := &recordingBuilder{Builder: env.CEL}
	env.CEL = rec
	env.Site = func(p expr.PlaceID) expr.Site {
		return expr.Site{Place: p, Scope: phase.ScopeUpstream, PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: expr.ResultBool}
	}
	if _, err := Build(context.Background(), env, RequestSpec(cfg)); err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []expr.Site{
		{Place: expr.PlaceTransformRequestBody, Scope: phase.ScopeUpstream, PolicyType: v1alpha1.PolicyTypeTransformRequest},
		{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeUpstream, PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: expr.ResultString},
		{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeUpstream, PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: expr.ResultString},
		{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeUpstream, PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: expr.ResultString},
		{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeUpstream, PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: expr.ResultDyn},
	}
	if len(rec.sites) != len(want) {
		t.Fatalf("sites %+v, want %+v", rec.sites, want)
	}
	for i := range want {
		g, w := rec.sites[i], want[i]
		if g.Place != w.Place || g.Scope != w.Scope || g.PolicyType != w.PolicyType || g.Result != w.Result {
			t.Errorf("site %d = %+v, want %+v", i, g, w)
		}
	}
	env.Site = nil
	rec.sites = nil
	if _, err := Build(context.Background(), env, RequestSpec(cfg)); err != nil {
		t.Fatalf("Build without Site: %v", err)
	}
	if s := rec.sites[1]; s.Scope != phase.ScopeUpstream || s.PolicyType != v1alpha1.PolicyTypeTransformRequest || s.Result != expr.ResultString {
		t.Errorf("site without BuildEnv.Site = %+v", s)
	}
}

// TestBuildErrors covers the errors without a config finding: an ended
// context, a CEL field without a builder.
func TestBuildErrors(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{Body: "request.body"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, buildEnv(t, cfg, phase.ScopeRoute), RequestSpec(cfg)); !errors.Is(err, context.Canceled) {
		t.Errorf("Build with an ended context = %v", err)
	}
	env := buildEnv(t, cfg, phase.ScopeRoute)
	env.CEL = nil
	if _, err := Build(context.Background(), env, RequestSpec(cfg)); !errors.Is(err, ErrNoCEL) {
		t.Errorf("Build without CEL = %v, want ErrNoCEL", err)
	}
	literal := &v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{reqRemove("body", "a")}}
	env = buildEnv(t, literal, phase.ScopeRoute)
	env.CEL = nil
	if _, err := Build(context.Background(), env, RequestSpec(literal)); err != nil {
		t.Errorf("Build of literal entries without CEL = %v", err)
	}
}

// TestBuildBodyNeedMatchesRegistry covers the body need: a Filter reads
// the body exactly when the registry tells the snapshot compiler to gate
// it (registry.Registry.TransformBody), at every scope.
func TestBuildBodyNeedMatchesRegistry(t *testing.T) {
	reg := registry.New()
	refs := celtest.Compiler(t)
	configs := []any{
		&v1alpha1.TransformRequestConfig{},
		&v1alpha1.TransformRequestConfig{Body: `"x"`},
		&v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("header", "x-a", `"a"`), reqSet("query", "q", "request.path"), reqSet("path", "", `"/a"`)}},
		&v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("header", "x-a", "string(request.body.id)")}},
		&v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("body", "a", `"b"`)}},
		&v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{reqRemove("body", "a")}},
		&v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{reqRemove("header", "x-a"), reqRemove("query", "q")}},
		&v1alpha1.TransformRequestConfig{ArrayOps: []v1alpha1.ArrayOp{arrayOp("delete", "a.0", "")}},
		&v1alpha1.TransformRequestConfig{Replace: []v1alpha1.Replace{literally("", "a", "b")}},
		&v1alpha1.TransformResponseConfig{Set: []v1alpha1.TransformResponseSet{respSet("header", "x-a", "string(response.status)")}},
		&v1alpha1.TransformResponseConfig{Set: []v1alpha1.TransformResponseSet{respSet("header", "x-a", "string(response.body.total)")}},
		&v1alpha1.TransformResponseConfig{Body: "response.body"},
		&v1alpha1.TransformResponseConfig{Remove: []v1alpha1.TransformResponseRemove{respRemove("body", "a")}},
	}
	for i, cfg := range configs {
		s := specOf(t, cfg)
		for _, scope := range []phase.Scope{phase.ScopeGateway, phase.ScopeRoute, phase.ScopeUpstream} {
			f := build(t, cfg, scope)
			pol := &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "p"}, Spec: v1alpha1.PolicySpec{Type: s.Side.PolicyType()}}
			want, err := reg.TransformBody(registry.Attachment{Policy: pol, Config: cfg, Scope: scope}, refs)
			if err != nil {
				t.Fatalf("config %d: TransformBody: %v", i, err)
			}
			if f.needsBody != want {
				t.Errorf("config %d at %v: needsBody = %v, registry says %v", i, scope, f.needsBody, want)
			}
		}
	}
}
