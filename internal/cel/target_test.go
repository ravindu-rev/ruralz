// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the per-target result of the transform set[] valueExpression
// places (docs/architecture/02-configuration-model.md "Allowed places":
// string for targets header, query and path, dyn for target body), the
// program key that keeps those results apart, and References.

// transformValueSite returns the site of a transform.request set[]
// valueExpression at Route scope with result r.
func transformValueSite(r expr.Result) expr.Site {
	return expr.Site{
		Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeRoute,
		PolicyType: v1alpha1.PolicyTypeTransformRequest, Result: r,
	}
}

// TestSiteResultNarrowsTransformValue checks that a transform set[]
// valueExpression takes the result its site names: a string site refuses
// an int with RZ-CFG-014 and takes a string or dyn, a dyn site and the
// place's own dyn take anything, for both transform types.
func TestSiteResultNarrowsTransformValue(t *testing.T) {
	c := newCompiler(t)
	for _, place := range []expr.PlaceID{expr.PlaceTransformRequestValue, expr.PlaceTransformResponseValue} {
		cases := []struct {
			name   string
			result expr.Result
			src    string
			ok     bool
		}{
			{"string site takes a string", expr.ResultString, `"x-" + request.method`, true},
			{"string site takes dyn", expr.ResultString, `dyn(request.headers["x-a"])`, true},
			{"string site refuses an int", expr.ResultString, `1 + 2`, false},
			{"string site refuses a map", expr.ResultString, `{"a": 1}`, false},
			{"dyn site takes an int", expr.ResultDyn, `1 + 2`, true},
			{"dyn site takes a map", expr.ResultDyn, `{"a": [1, "b"]}`, true},
			{"place result takes an int", 0, `1 + 2`, true},
		}
		for _, tc := range cases {
			site := expr.Site{Place: place, Scope: phase.ScopeRoute, Result: tc.result}
			iss := c.Check(site, tc.src)
			if tc.ok && len(iss) > 0 {
				t.Errorf("%s: %s: Check(%q) = %+v, want no issue", place, tc.name, tc.src, iss)
			}
			if !tc.ok && (len(iss) == 0 || iss[0].Code != codeInvalid || !strings.Contains(iss[0].Message, "want string")) {
				t.Errorf("%s: %s: Check(%q) = %+v, want RZ-CFG-014 naming the string result", place, tc.name, tc.src, iss)
			}
		}
	}
}

// TestSiteResultRefusesOtherNarrowing checks that a site naming a result
// its place cannot narrow to is refused with RZ-CFG-014 whatever the
// source: a string result on a bool place, a bool result on a string
// place, an unknown result.
func TestSiteResultRefusesOtherNarrowing(t *testing.T) {
	c := newCompiler(t)
	for _, site := range []expr.Site{
		{Place: expr.PlaceAuthzCELRule, Result: expr.ResultString},
		{Place: expr.PlaceRateLimitKey, Result: expr.ResultBool},
		{Place: expr.PlaceTransformRequestValue, Result: expr.Result(9)},
	} {
		iss := c.Check(site, `true`)
		if len(iss) != 1 || iss[0].Code != codeInvalid || !strings.Contains(iss[0].Message, "does not narrow") {
			t.Errorf("Check at %s with result %d = %+v, want one RZ-CFG-014", site.Place, site.Result, iss)
		}
	}
	// Restating the place's own result is no narrowing.
	if iss := c.Check(expr.Site{Place: expr.PlaceAuthzCELRule, Result: expr.ResultBool}, `true`); len(iss) > 0 {
		t.Errorf("Check restating the place's result = %+v", iss)
	}
}

// TestBuilderKeepsResultsApart checks that one Builder compiles a source
// at a dyn site and at a string site separately: the dyn program is not
// reused where the string check refuses the source.
func TestBuilderKeepsResultsApart(t *testing.T) {
	b := newCompiler(t).NewBuilder(nil, expr.InputCaps{})
	if prg, iss := b.Compile(transformValueSite(expr.ResultDyn), `1`); prg == nil {
		t.Fatalf("Compile at the dyn site = %+v", iss)
	}
	if prg, iss := b.Compile(transformValueSite(expr.ResultString), `1`); prg != nil || len(iss) == 0 {
		t.Errorf("Compile at the string site = %v, %+v; want the string check's issue", prg, iss)
	}
	a, _ := resolve(transformValueSite(expr.ResultDyn))
	s, _ := resolve(transformValueSite(expr.ResultString))
	if a.key() == s.key() {
		t.Error("sites differing only in their result share a key")
	}
}

// TestReferences checks References: the bodies and variables a source
// reads at its site, an error for a source that does not check there (an
// undeclared upstream outside Upstream scope) and for an unknown place.
func TestReferences(t *testing.T) {
	c := newCompiler(t)
	cases := []struct {
		name      string
		site      expr.Site
		src       string
		reqBody   bool
		respBody  bool
		vars      expr.Var
		wantError bool
	}{
		{"request body read", transformValueSite(expr.ResultString), `string(request.body.total)`, true, false, expr.VarRequest, false},
		{"no body read", transformValueSite(expr.ResultString), `request.headers["x-a"]`, false, false, expr.VarRequest, false},
		{
			"response body read",
			expr.Site{Place: expr.PlaceTransformResponseBody, Scope: phase.ScopeRoute},
			`{"total": response.body.total, "route": route.name}`, false, true, expr.VarResponse | expr.VarRoute, false,
		},
		{
			"upstream at Upstream scope",
			expr.Site{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeUpstream, Result: expr.ResultString},
			`upstream.name + string(request.body.n)`, true, false, expr.VarUpstream | expr.VarRequest, false,
		},
		{"upstream outside Upstream scope", transformValueSite(expr.ResultString), `upstream.name`, false, false, 0, true},
		{"syntax error", transformValueSite(expr.ResultString), `request.`, false, false, 0, true},
		{"unknown place", expr.Site{Place: "Route.spec.bogus"}, `true`, false, false, 0, true},
	}
	for _, tc := range cases {
		refs, err := c.References(tc.site, tc.src)
		if tc.wantError {
			if err == nil {
				t.Errorf("%s: References(%q) = %+v, want an error", tc.name, tc.src, refs)
			}
			continue
		}
		if err != nil || refs.RequestBody != tc.reqBody || refs.ResponseBody != tc.respBody || refs.Vars != tc.vars {
			t.Errorf("%s: References(%q) = %+v, %v; want request body %v, response body %v, vars %v",
				tc.name, tc.src, refs, err, tc.reqBody, tc.respBody, tc.vars.Names())
		}
	}
	// A result the source cannot take is Check's finding, not an error of
	// References.
	if _, err := c.References(transformValueSite(expr.ResultString), `1`); err != nil {
		t.Errorf("References of a source with the wrong result = %v, want no error", err)
	}
}

// TestEvalValueNestedJSON checks AppendJSON on the Values EvalValue
// returns: a string quoted, bytes as base64, other values as AppendBody
// writes them, and a value without a JSON form refused.
func TestEvalValueNestedJSON(t *testing.T) {
	c := newCompiler(t)
	site := expr.Site{Place: expr.PlaceTransformRequestBody, Scope: phase.ScopeRoute}
	cases := []struct {
		src, nested, body string
	}{
		{`"a\"b"`, `"a\"b"`, `a"b`},
		{`b"\x00\xff"`, `"AP8="`, "\x00\xff"},
		{`{"b": 1, "a": [true, null, 2.5]}`, `{"a":[true,null,2.5],"b":1}`, `{"a":[true,null,2.5],"b":1}`},
		{`7`, `7`, `7`},
		{`null`, `null`, `null`},
		{`{"k": b"\x01"}`, `{"k":"AQ=="}`, `{"k":"AQ=="}`},
		{`timestamp("2026-10-30T01:02:03Z")`, `"2026-10-30T01:02:03Z"`, `"2026-10-30T01:02:03Z"`},
		{`duration("1.5s")`, `"1.5s"`, `"1.5s"`},
	}
	for _, tc := range cases {
		prg, iss := c.NewBuilder(nil, expr.InputCaps{}).Compile(site, tc.src)
		if prg == nil {
			t.Fatalf("Compile(%q) = %+v", tc.src, iss)
		}
		v, err := prg.EvalValue(t.Context(), &expr.Vars{})
		if err != nil {
			t.Fatalf("EvalValue(%q) = %v", tc.src, err)
		}
		nj, ok := v.(expr.NestedJSON)
		if !ok {
			t.Fatalf("EvalValue(%q) returned %T, which is no expr.NestedJSON", tc.src, v)
		}
		got, err := nj.AppendJSON([]byte("p:"))
		if err != nil || string(got) != "p:"+tc.nested {
			t.Errorf("AppendJSON of %s = %q, %v; want %q", tc.src, got, err, tc.nested)
		}
		body, err := v.AppendBody(nil)
		if err != nil || string(body) != tc.body {
			t.Errorf("AppendBody of %s = %q, %v; want %q", tc.src, body, err, tc.body)
		}
	}
	prg, _ := c.NewBuilder(nil, expr.InputCaps{}).Compile(site, `[double("NaN")]`)
	v, err := prg.EvalValue(t.Context(), &expr.Vars{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.(expr.NestedJSON).AppendJSON(nil); err == nil {
		t.Error("AppendJSON of NaN succeeded")
	}
}

// TestDecodedBodyNestedJSON checks AppendJSON on a decoded body view:
// numbers keep their literal text and a top-level string is quoted.
func TestDecodedBodyNestedJSON(t *testing.T) {
	c := newCompiler(t)
	for in, want := range map[string]string{
		`{"total": 1.50E+02, "a": "x"}`: `{"a":"x","total":1.50E+02}`,
		`"plain"`:                       `"plain"`,
	} {
		v, _, err := c.DecodeJSON([]byte(in), 0)
		if err != nil {
			t.Fatal(err)
		}
		got, err := v.(expr.NestedJSON).AppendJSON(nil)
		if err != nil || string(got) != want {
			t.Errorf("AppendJSON of decoded %s = %q, %v; want %q", in, got, err, want)
		}
	}
}
