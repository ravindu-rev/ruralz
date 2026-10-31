// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtest

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// TestProgramEvaluatesViews compiles a program and evaluates it over the
// prepared views and a decoded body.
func TestProgramEvaluatesViews(t *testing.T) {
	prg := Program(t, At(expr.PlaceAuthzCELRule),
		`route.labels.team == "core" && consumer.tier == "gold" && consumer.labels.org == "acme" && request.body.ok`)
	v := &expr.Vars{
		Request:  &expr.Request{Body: JSON(t, `{"ok": true}`)},
		Route:    Route("orders", map[string]string{"team": "core"}),
		Consumer: Consumer(expr.Consumer{Name: "acme", Tier: "gold", Labels: map[string]string{"org": "acme"}}),
	}
	got, err := prg.EvalBool(t.Context(), v)
	if err != nil || !got {
		t.Errorf("EvalBool = %v, %v; want true", got, err)
	}
}

// TestPreparedViews checks that Route and Consumer set Prepared, and that
// Consumer leaves its argument alone.
func TestPreparedViews(t *testing.T) {
	if r := Route("r", nil); r.Prepared == nil || r.Name != "r" {
		t.Errorf("Route = %+v", r)
	}
	in := expr.Consumer{Name: "c"}
	if c := Consumer(in); c.Prepared == nil || c.Name != "c" || in.Prepared != nil {
		t.Errorf("Consumer = %+v, argument %+v", c, in)
	}
}

// TestCompilerChecks checks that the Compiler reports Issues.
func TestCompilerChecks(t *testing.T) {
	if iss := Compiler(t).Check(At(expr.PlaceRouteMatchWhen), `response.status == 200`); len(iss) == 0 {
		t.Error("Check accepted response in Route.spec.match.when")
	}
}
