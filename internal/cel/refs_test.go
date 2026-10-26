// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"reflect"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestRefs checks the static facts a program reports: the variables it
// reads, the bodies it selects (body gates), and the steps it reads.
func TestRefs(t *testing.T) {
	bodyWhen := expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnRequestBody, GatesBody: true}
	cases := []struct {
		name string
		site expr.Site
		src  string
		want expr.Refs
	}{
		{
			"variables", at(expr.PlaceRateLimitKey), `consumer == null ? source.ip : consumer.name`,
			expr.Refs{Vars: expr.VarSource | expr.VarConsumer},
		},
		{
			"body in a comprehension", at(expr.PlaceAuthzCELRule), `request.body.items.exists(i, i.qty > 1)`,
			expr.Refs{Vars: expr.VarRequest, RequestBody: true},
		},
		{
			"body in a ternary", bodyWhen, `request.method == "POST" ? request.body.kind == "x" : false`,
			expr.Refs{Vars: expr.VarRequest, RequestBody: true},
		},
		{
			"body presence test", bodyWhen, `has(request.body.kind)`,
			expr.Refs{Vars: expr.VarRequest, RequestBody: true},
		},
		{
			"no body", at(expr.PlaceAuthzCELRule), `request.headers["x"] == "y"`,
			expr.Refs{Vars: expr.VarRequest},
		},
		{
			"response body", at(expr.PlaceTransformResponseValue), `string(response.body.total)`,
			expr.Refs{Vars: expr.VarResponse, ResponseBody: true},
		},
		{
			"a shadowing local is not the variable", at(expr.PlaceAccessLogWhen), `[1, 2].exists(response, response == 1)`,
			expr.Refs{},
		},
		{
			"step keys", sequential(expr.PlaceStepWhen, "order", "stock"), `steps.order.body.id == 1 && steps["stock"].status == 200`,
			expr.Refs{Vars: expr.VarSteps, Steps: []string{"order", "stock"}, StepBodies: []string{"order"}},
		},
		{
			"step presence", sequential(expr.PlaceStepWhen, "order"), `"order" in steps && has(steps.order)`,
			expr.Refs{Vars: expr.VarSteps, Steps: []string{"order"}},
		},
		{
			"computed step key", sequential(expr.PlaceStepWhen, "order"), `steps[request.headers["s"]].status == 200`,
			expr.Refs{Vars: expr.VarRequest | expr.VarSteps, AnyStep: true},
		},
		{
			"every step iterated", sequential(expr.PlaceStepWhen, "order"), `steps.all(k, steps[k].status == 200)`,
			expr.Refs{Vars: expr.VarSteps, AnyStep: true},
		},
		{
			"steps sized", sequential(expr.PlaceStepPathExpression, "order"), `"/n/" + string(size(steps))`,
			expr.Refs{Vars: expr.VarSteps, AnyStep: true},
		},
		{
			"computed step body", sequential(expr.PlaceStepWhen, "order"), `steps[request.headers["s"]].body.ok == true`,
			expr.Refs{Vars: expr.VarRequest | expr.VarSteps, AnyStep: true},
		},
	}
	c := newCompiler(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prg, iss := c.NewBuilder(nil).Compile(tc.site, tc.src)
			if prg == nil {
				t.Fatalf("Compile(%q) = %+v", tc.src, iss)
			}
			if got := prg.Refs(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Refs(%q) = %+v, want %+v", tc.src, got, tc.want)
			}
		})
	}
}

// TestReferencesRequestBody checks the authz.cel Phase oracle: a rule that
// selects request.body runs in onRequestBody, and a rule that does not
// compile is an error.
func TestReferencesRequestBody(t *testing.T) {
	c := newCompiler(t)
	cases := []struct {
		src     string
		want    bool
		wantErr bool
	}{
		{`auth.claims.scope.split(" ").exists(s, s == "orders:read")`, false, false},
		{`request.body.amount < 1000`, true, false},
		{`has(request.body.amount)`, true, false},
		{`request.method == `, false, true},
		{`response.status == 200`, false, true},
	}
	for _, tc := range cases {
		got, err := c.ReferencesRequestBody(tc.src)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("ReferencesRequestBody(%q) = %v, %v; want %v, error %v", tc.src, got, err, tc.want, tc.wantErr)
		}
	}
}

// TestStepsModeNeedsSequential checks that the steps rules follow the mode:
// a sequential composition reads earlier steps, an aggregate one none.
func TestStepsModeNeedsSequential(t *testing.T) {
	c := newCompiler(t)
	src := `steps.order.status == 200`
	if iss := c.Check(sequential(expr.PlaceStepWhen, "order"), src); len(iss) != 0 {
		t.Errorf("sequential: %+v", iss)
	}
	for _, mode := range []v1alpha1.CompositionMode{v1alpha1.CompositionModeAggregate, v1alpha1.CompositionModeConditional} {
		if iss := c.Check(expr.Site{Place: expr.PlaceStepWhen, Mode: mode, EarlierSteps: []string{"order"}}, src); len(iss) != 1 {
			t.Errorf("%s: Issues = %+v, want one", mode, iss)
		}
	}
}
