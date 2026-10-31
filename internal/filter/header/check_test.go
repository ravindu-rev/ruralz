// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package header

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// schemaDiagnostics validates the JSON Policy document src against the
// embedded rendered schema (stage F) and returns its findings.
func schemaDiagnostics(t *testing.T, src string) diag.List {
	t.Helper()
	idx, err := schemaidx.Embedded()
	if err != nil {
		t.Fatalf("schemaidx.Embedded: %v", err)
	}
	set, err := schemaview.Embedded(idx)
	if err != nil {
		t.Fatalf("schemaview.Embedded: %v", err)
	}
	files := &tree.FileTable{}
	file := files.Add(tree.File{Path: "policy.json"})
	docs, ds, err := profile.Parse(t.Context(), []byte(src), file, "policy.json", profile.FormatJSON, profile.Options{})
	if err != nil || len(ds) != 0 || len(docs) != 1 {
		t.Fatalf("profile.Parse = %d documents, %v, %v", len(docs), ds, err)
	}
	res := &tree.Resource{
		ID:         tree.ID{Kind: v1alpha1.KindPolicy, Name: "headers-test"},
		APIVersion: "ruralz/v1alpha1", Root: docs[0].Root, Start: docs[0].Start,
	}
	out, ok := set.Validate(res, files)
	if !ok {
		t.Fatal("ruralz/v1alpha1 is not served")
	}
	return out
}

// TestSchemaRefusesEmptyValueExpression checks that the schema refuses an
// empty valueExpression (minLength 1) in the set and add entries of both
// sides with RZ-CFG-005 at that member, so stage F reports it before the
// Check backstop, and accepts an expression.
func TestSchemaRefusesEmptyValueExpression(t *testing.T) {
	for _, tc := range []struct {
		side, list, at string
	}{
		{"request", "set", "spec.config.request.set[name=x-a].valueExpression"},
		{"request", "add", "spec.config.request.add[0].valueExpression"},
		{"response", "set", "spec.config.response.set[name=x-a].valueExpression"},
		{"response", "add", "spec.config.response.add[0].valueExpression"},
	} {
		for _, expression := range []string{`""`, `"route.name"`} {
			src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "headers-test"},` +
				`"spec": {"type": "headers", "config": {"` + tc.side + `": {"` + tc.list + `": ` +
				`[{"name": "x-a", "valueExpression": ` + expression + `}]}}}}`
			ds := schemaDiagnostics(t, src)
			refused := false
			for _, d := range ds {
				if d.Code == CodeConfig && d.Path.String() == tc.at {
					refused = true
				}
			}
			if want := expression == `""`; refused != want || (!want && len(ds) != 0) {
				t.Errorf("%s.%s valueExpression %s: schema diagnostics %v, want refused %v at %s", tc.side, tc.list, expression, ds, want, tc.at)
			}
		}
	}
}

// managedNames are the field names the Node manages, in assorted case
// (httpfield.Protect).
func managedNames() []string {
	return []string{
		"host", "Content-Length", "expect", "connection", "Keep-Alive", "proxy-connection", "te", "trailer",
		"Transfer-Encoding", "upgrade", "proxy-authenticate", "Proxy-Authorization", "traceparent", "TraceState",
	}
}

// entries returns n distinct literal request entries.
func entries(n int) []v1alpha1.HeaderRequestSet {
	out := make([]v1alpha1.HeaderRequestSet, n)
	for i := range out {
		out[i] = reqLit("x-entry-"+strings.Repeat("a", i+1), "v")
	}
	return out
}

// respEntries returns n distinct literal response entries.
func respEntries(n int) []v1alpha1.HeaderResponseSet {
	out := make([]v1alpha1.HeaderResponseSet, n)
	for i := range out {
		out[i] = respLit("x-entry-"+strings.Repeat("b", i+1), "v")
	}
	return out
}

// configCase is a named config of a table; schema reports whether the
// JSON Schema reports an invalid config's finding itself, so Check leaves
// it out.
type configCase struct {
	name   string
	cfg    *v1alpha1.HeadersConfig
	schema bool
}

// invalidConfigs are configs Build refuses with RZ-CFG-005.
func invalidConfigs() []configCase {
	req := func(o v1alpha1.HeaderRequestOps) *v1alpha1.HeadersConfig { return &v1alpha1.HeadersConfig{Request: &o} }
	resp := func(o v1alpha1.HeaderResponseOps) *v1alpha1.HeadersConfig {
		return &v1alpha1.HeadersConfig{Response: &o}
	}
	both := "1"
	out := []configCase{
		{"empty name", req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit("", "1")}}), false},
		{"name with a space", req(v1alpha1.HeaderRequestOps{Add: []v1alpha1.HeaderRequestSet{reqLit("x a", "1")}}), false},
		{"pseudo-header name", resp(v1alpha1.HeaderResponseOps{Set: []v1alpha1.HeaderResponseSet{respLit(":status", "200")}}), false},
		{"name over the limit", req(v1alpha1.HeaderRequestOps{Remove: []string{strings.Repeat("x", httpfield.MaxNameBytes+1)}}), false},
		{"non-ASCII name", resp(v1alpha1.HeaderResponseOps{Remove: []string{"x-größe"}}), false},
		{"duplicate set names ignoring case", req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit("x-a", "1"), reqLit("X-A", "2")}}), false},
		{"duplicate add names", resp(v1alpha1.HeaderResponseOps{Add: []v1alpha1.HeaderResponseSet{respLit("x-a", "1"), respLit("x-a", "2")}}), false},
		{"duplicate add names ignoring case", req(v1alpha1.HeaderRequestOps{Add: []v1alpha1.HeaderRequestSet{reqLit("x-a", "1"), reqLit("X-a", "2")}}), false},
		{"duplicate remove names ignoring case", resp(v1alpha1.HeaderResponseOps{Remove: []string{"server", "Server"}}), false},
		{"byte-equal set names", req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit("x-a", "1"), reqLit("x-a", "2")}}), true},
		{"byte-equal remove names", req(v1alpha1.HeaderRequestOps{Remove: []string{"x-a", "x-a"}}), true},
		{"carriage return in a value", req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit("x-a", "a\rb")}}), false},
		{"line feed in a value", resp(v1alpha1.HeaderResponseOps{Add: []v1alpha1.HeaderResponseSet{respLit("x-a", "a\nb")}}), false},
		{"NUL in a value", req(v1alpha1.HeaderRequestOps{Add: []v1alpha1.HeaderRequestSet{reqLit("x-a", "a\x00b")}}), false},
		{"DEL in a value", req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit("x-a", "a\x7fb")}}), false},
		{"leading space", resp(v1alpha1.HeaderResponseOps{Set: []v1alpha1.HeaderResponseSet{respLit("x-a", " a")}}), false},
		{"trailing tab", req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit("x-a", "a\t")}}), false},
		{"value over the limit", req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit("x-a", strings.Repeat("v", httpfield.MaxValueBytes+1))}}), false},
		{"empty valueExpression", req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{{Name: "x-a"}}}), false},
		{"empty valueExpression in add", resp(v1alpha1.HeaderResponseOps{Add: []v1alpha1.HeaderResponseSet{{Name: "x-a"}}}), false},
		{"value and valueExpression", req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{{Name: "x-a", Value: &both, ValueExpression: `"2"`}}}), true},
		{"33 entries across both sides", &v1alpha1.HeadersConfig{
			Request:  &v1alpha1.HeaderRequestOps{Set: entries(20), Remove: []string{"x-r"}},
			Response: &v1alpha1.HeaderResponseOps{Add: respEntries(11), Remove: []string{"x-r"}},
		}, false},
	}
	for _, name := range managedNames() {
		out = append(out,
			configCase{"managed " + name + " in set", req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit(name, "1")}}), false},
			configCase{"managed " + name + " in remove", resp(v1alpha1.HeaderResponseOps{Remove: []string{name}}), false},
		)
	}
	return out
}

// validConfigs are configs Build compiles.
func validConfigs() []configCase {
	empty := ""
	req := func(o v1alpha1.HeaderRequestOps) *v1alpha1.HeadersConfig { return &v1alpha1.HeadersConfig{Request: &o} }
	return []configCase{
		{name: "empty config", cfg: &v1alpha1.HeadersConfig{}},
		{name: "forwarding and cookie fields", cfg: &v1alpha1.HeadersConfig{
			Request: &v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{
				reqLit("x-forwarded-for", "192.0.2.1"), reqLit("forwarded", "for=192.0.2.1"),
				reqLit("x-forwarded-proto", "https"), reqLit("x-forwarded-host", "shop.example"),
			}},
			Response: &v1alpha1.HeaderResponseOps{Add: []v1alpha1.HeaderResponseSet{respLit("set-cookie", "a=1"), respLit("content-type", "text/plain")}},
		}},
		{name: "empty literal value", cfg: req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{{Name: "x-a", Value: &empty}}})},
		{name: "obs-text and inner tab", cfg: req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit("x-a", "caf\xc3\xa9\tb")}})},
		{name: "value at the limit", cfg: req(v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit("x-a", strings.Repeat("v", httpfield.MaxValueBytes))}})},
		{name: "name at the limit", cfg: req(v1alpha1.HeaderRequestOps{Remove: []string{strings.Repeat("x", httpfield.MaxNameBytes)}})},
		{name: "one name in every list of both sides", cfg: &v1alpha1.HeadersConfig{
			Request: &v1alpha1.HeaderRequestOps{
				Remove: []string{"x-a"}, Set: []v1alpha1.HeaderRequestSet{reqLit("X-A", "1")}, Add: []v1alpha1.HeaderRequestSet{reqLit("x-A", "2")},
			},
			Response: &v1alpha1.HeaderResponseOps{
				Remove: []string{"x-a"}, Set: []v1alpha1.HeaderResponseSet{respLit("X-A", "1")}, Add: []v1alpha1.HeaderResponseSet{respLit("x-A", "2")},
			},
		}},
		{name: "32 entries", cfg: &v1alpha1.HeadersConfig{
			Request:  &v1alpha1.HeaderRequestOps{Set: entries(30)},
			Response: &v1alpha1.HeaderResponseOps{Remove: []string{"server", "x-powered-by"}},
		}},
		{name: "documented headers-security", cfg: &v1alpha1.HeadersConfig{Response: &v1alpha1.HeaderResponseOps{
			Set: []v1alpha1.HeaderResponseSet{respLit("strict-transport-security", "max-age=31536000")},
		}}},
		{name: "documented headers-internal", cfg: req(v1alpha1.HeaderRequestOps{
			Set: []v1alpha1.HeaderRequestSet{reqExpr("x-shop-consumer", `consumer == null ? "anonymous" : consumer.name`)},
		})},
	}
}

// TestBuildRejectsFindings checks that Build refuses every finding with
// RZ-CFG-005, those the schema reports itself included, and compiles the
// valid configs, among them the example Bundle's two headers Policies.
func TestBuildRejectsFindings(t *testing.T) {
	for _, tt := range invalidConfigs() {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Factory{}.Build(t.Context(), buildEnv(t, tt.cfg, phase.ScopeRoute))
			if code, ok := errcode.CodeOf(err); f != nil || !ok || code != CodeConfig {
				t.Errorf("Build = %v, %v; want an %s error", f, err, CodeConfig)
			}
		})
	}
	for _, tt := range validConfigs() {
		for _, scope := range []phase.Scope{phase.ScopeGateway, phase.ScopeRoute, phase.ScopeUpstream} {
			if _, err := (Factory{}).Build(t.Context(), buildEnv(t, tt.cfg, scope)); err != nil {
				t.Errorf("Build(%s) at %s scope = %v, want a Filter", tt.name, scope, err)
			}
		}
	}
	for _, cfg := range []any{nil, &v1alpha1.AuthzCELConfig{Rule: "true"}, (*v1alpha1.HeadersConfig)(nil)} {
		env := buildEnv(t, nil, phase.ScopeRoute)
		env.Config = cfg
		f, err := Factory{}.Build(t.Context(), env)
		if code, ok := errcode.CodeOf(err); f != nil || !ok || code != CodeConfig {
			t.Errorf("Build(config %T) = %v, %v; want an %s error", cfg, f, err, CodeConfig)
		}
	}
}

// TestBuildCompilesValueExpressions checks the CEL refusals of Build: a
// syntax, type or unavailable-variable error is RZ-CFG-014 and an
// estimate over the cost ceiling RZ-CFG-015, on either side; response and
// upstream are variables of the response side only.
func TestBuildCompilesValueExpressions(t *testing.T) {
	req := func(src string) *v1alpha1.HeadersConfig {
		return &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqExpr("x-a", src)}}}
	}
	resp := func(src string) *v1alpha1.HeadersConfig {
		return &v1alpha1.HeadersConfig{Response: &v1alpha1.HeaderResponseOps{Add: []v1alpha1.HeaderResponseSet{respExpr("x-a", src)}}}
	}
	tests := []struct {
		name string
		cfg  *v1alpha1.HeadersConfig
		code string
	}{
		{"syntax error", req(`consumer.name +`), CodeCEL},
		{"bool result", req(`consumer != null`), CodeCEL},
		{"int result on the response side", resp(`response.status`), CodeCEL},
		{"response on the request side", req(`string(response.status)`), CodeCEL},
		{"upstream on the request side", req(`upstream.name`), CodeCEL},
		{"request body on the request side", req(`string(request.body.id)`), CodeCEL},
		{"response body on the response side", resp(`string(response.body.total)`), CodeCEL},
		{"cost ceiling", req(`request.headers.exists(k, request.headers.exists(j, k + j == "x")) ? "a" : "b"`), CodeCost},
		{"cost ceiling on the response side", resp(`response.headers.exists(k, response.headers.exists(j, k + j == "x")) ? "a" : "b"`), CodeCost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, scope := range []phase.Scope{phase.ScopeRoute, phase.ScopeUpstream} {
				f, err := Factory{}.Build(t.Context(), buildEnv(t, tt.cfg, scope))
				if code, ok := errcode.CodeOf(err); f != nil || !ok || code != tt.code {
					t.Errorf("Build at %s scope = %v, %v; want an %s error", scope, f, err, tt.code)
				}
			}
		})
	}
	// The response side reads response and upstream.
	if _, err := (Factory{}).Build(t.Context(), buildEnv(t, resp(`string(response.status) + upstream.name`), phase.ScopeRoute)); err != nil {
		t.Errorf("Build of a response expression = %v, want a Filter", err)
	}
}

// TestBuildPreconditions checks the refusals without a code: an ended
// context returns its error, and a valueExpression without a CEL builder
// ErrNoCEL, while literal values need none.
func TestBuildPreconditions(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Factory{}.Build(ctx, buildEnv(t, &v1alpha1.HeadersConfig{}, phase.ScopeRoute))
	if _, coded := errcode.CodeOf(err); !errors.Is(err, context.Canceled) || coded {
		t.Errorf("Build with a canceled context = %v, want context.Canceled without a code", err)
	}
	env := buildEnv(t, &v1alpha1.HeadersConfig{Response: &v1alpha1.HeaderResponseOps{
		Set: []v1alpha1.HeaderResponseSet{respExpr("x-a", `upstream.name`)},
	}}, phase.ScopeRoute)
	env.CEL = nil
	if _, err := (Factory{}).Build(t.Context(), env); !errors.Is(err, ErrNoCEL) {
		t.Errorf("Build of a valueExpression without a CEL builder = %v, want ErrNoCEL", err)
	}
	env = buildEnv(t, &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Set: []v1alpha1.HeaderRequestSet{reqLit("x-a", "1")},
	}}, phase.ScopeRoute)
	env.CEL = nil
	if _, err := (Factory{}).Build(t.Context(), env); err != nil {
		t.Errorf("Build of literal values without a CEL builder = %v, want a Filter", err)
	}
}

// TestBuildCompilesAtTheSites checks that Build compiles request
// valueExpressions at the request place and response ones at the response
// place, through BuildEnv.Site, and at the place refined by scope and type
// when Site is unset.
func TestBuildCompilesAtTheSites(t *testing.T) {
	cfg := &v1alpha1.HeadersConfig{
		Request:  &v1alpha1.HeaderRequestOps{Add: []v1alpha1.HeaderRequestSet{reqExpr("x-a", `"a"`)}},
		Response: &v1alpha1.HeaderResponseOps{Set: []v1alpha1.HeaderResponseSet{respExpr("x-b", `upstream.name`)}},
	}
	env := buildEnv(t, cfg, phase.ScopeUpstream)
	var asked []expr.PlaceID
	env.Site = func(p expr.PlaceID) expr.Site {
		asked = append(asked, p)
		return expr.Site{Place: p, Scope: phase.ScopeUpstream, PolicyType: Type}
	}
	if _, err := (Factory{}).Build(t.Context(), env); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if want := []expr.PlaceID{expr.PlaceHeadersRequestValue, expr.PlaceHeadersResponseValue}; !slices.Equal(asked, want) {
		t.Errorf("Site asked for %v, want %v", asked, want)
	}
	// The request place has no upstream: a site of it refuses the response
	// expression, so Build uses the site Site returns.
	env.Site = func(expr.PlaceID) expr.Site { return expr.Site{Place: expr.PlaceHeadersRequestValue} }
	if _, err := (Factory{}).Build(t.Context(), env); err == nil {
		t.Error("Build ignored the site BuildEnv.Site returned")
	}
	env.Site = nil
	if _, err := (Factory{}).Build(t.Context(), env); err != nil {
		t.Errorf("Build without Site = %v, want a Filter", err)
	}
}

// TestCheckReportsFindings checks the offline check: each finding is
// RZ-CFG-005 at its path under spec.config, located through loc, a
// duplicate with the first entry as its related location; the findings the
// schema reports itself are left out; and a valid config, another config
// type, a nil resource or a nil add yield nothing.
func TestCheckReportsFindings(t *testing.T) {
	both := "1"
	cfg := &v1alpha1.HeadersConfig{
		Request: &v1alpha1.HeaderRequestOps{
			Set: []v1alpha1.HeaderRequestSet{
				reqLit("host", "shop.example"), reqLit("x-a", "a\nb"),
				{Name: "x-b"},
				reqLit("X-A", "1"),
				{Name: "x-c", Value: &both, ValueExpression: `"2"`},
			},
			Add:    []v1alpha1.HeaderRequestSet{reqLit("x a", "1"), reqLit("x-d", "1"), reqLit("x-d", "2")},
			Remove: []string{"x-e", "X-E", "x-e", "traceparent"},
		},
		Response: &v1alpha1.HeaderResponseOps{
			Set: []v1alpha1.HeaderResponseSet{respLit("x-f", " padded")},
			Add: respEntries(20),
		},
	}
	res := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "headers-internal"}, Config: cfg}
	var got []diag.Diagnostic
	loc := func(p diag.Path) diag.Location { return diag.Location{File: "policies.yaml", Line: len(p)} }
	Check(res, loc, func(d diag.Diagnostic) { got = append(got, d) })
	want := []struct{ path, message, related string }{
		{"spec.config", "33 entries across the set, add and remove lists, more than 32", ""},
		{"spec.config.request.set[name=host].name", "header host is managed by the Node (host)", ""},
		{"spec.config.request.set[name=x-a].value", "field value contains a control byte at offset 1", ""},
		{"spec.config.request.set[name=x-b]", "neither value nor a non-empty valueExpression is set", ""},
		{"spec.config.request.set[name=X-A].name", `"X-A" names the same field as "x-a", ignoring case`, "first entry at"},
		{"spec.config.request.add[0].name", "field name is not an RFC 9110 token: byte ' ' at offset 1", ""},
		{"spec.config.request.add[2].name", `"x-d" names the same field as "x-d", ignoring case`, "first entry at"},
		{"spec.config.request.remove[item=X-E]", `"X-E" names the same field as "x-e", ignoring case`, "first element at"},
		{"spec.config.request.remove[item=traceparent]", "header traceparent is managed by the Node (trace context)", ""},
		{"spec.config.response.set[name=x-f].value", "field value has leading or trailing whitespace", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("Check reported %d diagnostics, want %d:\n%v", len(got), len(want), got)
	}
	for i, d := range got {
		if d.Code != CodeConfig || d.Severity != diag.SeverityError || d.Path.String() != want[i].path || d.Message != want[i].message {
			t.Errorf("diagnostic %d = %s %v at %s %q, want %s at %s %q", i, d.Code, d.Severity, d.Path, d.Message, CodeConfig, want[i].path, want[i].message)
		}
		if d.Resource == nil || d.Resource.Name != "headers-internal" || d.File != "policies.yaml" || d.Line != len(d.Path) {
			t.Errorf("diagnostic %d resource %+v, location %+v; want the Policy and loc's location", i, d.Resource, d.Location)
		}
		switch {
		case want[i].related == "" && len(d.Related) != 0:
			t.Errorf("diagnostic %d related %+v, want none", i, d.Related)
		case want[i].related != "" && (len(d.Related) != 1 || d.Related[0].Message != want[i].related || d.Related[0].File != "policies.yaml"):
			t.Errorf("diagnostic %d related %+v, want %q at the first entry", i, d.Related, want[i].related)
		}
	}
	var n int
	count := func(diag.Diagnostic) { n++ }
	for _, tt := range validConfigs() {
		Check(&hub.Resource{Config: tt.cfg}, loc, count)
	}
	Check(&hub.Resource{Config: &v1alpha1.AuthzCELConfig{}}, nil, count)
	Check(&hub.Resource{Config: (*v1alpha1.HeadersConfig)(nil)}, nil, count)
	Check(nil, nil, count)
	Check(res, nil, nil)
	if n != 0 {
		t.Errorf("Check reported %d diagnostics for valid or foreign input, want 0", n)
	}
}

// TestCheckMatchesBuild checks that validation refuses what Build refuses:
// every invalid config yields a Check diagnostic unless the schema reports
// its finding, and Check never reports a config Build compiles.
func TestCheckMatchesBuild(t *testing.T) {
	for _, tt := range invalidConfigs() {
		var n int
		Check(&hub.Resource{Config: tt.cfg}, nil, func(diag.Diagnostic) { n++ })
		if tt.schema && n != 0 {
			t.Errorf("%s: Check reported %d diagnostics for a finding the schema reports", tt.name, n)
		}
		if !tt.schema && n == 0 {
			t.Errorf("%s: Check reported nothing, Build refuses it", tt.name)
		}
	}
	// Without loc, diagnostics carry no location and no related location.
	var got []diag.Diagnostic
	dup := &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit("x-a", "1"), reqLit("X-A", "2")}}}
	Check(&hub.Resource{Config: dup}, nil, func(d diag.Diagnostic) { got = append(got, d) })
	if len(got) != 1 || got[0].Location != (diag.Location{}) || got[0].Related != nil {
		t.Errorf("Check without loc = %+v, want one diagnostic without locations", got)
	}
}

// TestBuildNamesTheFinding checks that Build's error names the first
// finding's path under config and its message.
func TestBuildNamesTheFinding(t *testing.T) {
	_, err := Factory{}.Build(t.Context(), filter.BuildEnv{Config: &v1alpha1.HeadersConfig{
		Response: &v1alpha1.HeaderResponseOps{Remove: []string{"content-length"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "config.response.remove[item=content-length]: header content-length is managed by the Node (framing)") {
		t.Errorf("Build = %v, want the finding's path and message", err)
	}
	_, err = Factory{}.Build(t.Context(), filter.BuildEnv{Config: &v1alpha1.HeadersConfig{
		Request: &v1alpha1.HeaderRequestOps{Set: entries(MaxEntries + 1)},
	}})
	if err == nil || !strings.Contains(err.Error(), "headers: config: 33 entries") {
		t.Errorf("Build = %v, want the count at config", err)
	}
}
