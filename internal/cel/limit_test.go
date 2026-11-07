// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"errors"
	"testing"

	"cel.dev/cel-go/common/cost"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Scope lengths around the runtime cost limit for the orders rule, which
// charges 7.2 units a token: 150,000 tokens charge about 1,080,000 units,
// past the limit, and 100,000 tokens 720,015, within it.
const (
	overLimitTokens  = 150_000
	underLimitTokens = 100_000
)

// TestCostLimitEndsAComprehension checks that the cost limit alone bounds a
// tracked comprehension (docs/architecture/02-configuration-model.md
// "Limits", ADR-0019): the orders rule over overLimitTokens tokens ends
// with KindCostLimit, also where a logical operator would absorb an error
// value, as in `list.exists(...) || b`, since cel-go aborts the evaluation
// at the limit; over underLimitTokens tokens it runs to its end however
// long that takes; and the program evaluates normally after the limit.
func TestCostLimitEndsAComprehension(t *testing.T) {
	c := newCompiler(t)
	for _, tc := range []struct {
		src  string
		long bool
	}{
		{ordersRule, false},
		{ordersRule + ` || request.method == "GET"`, true},
	} {
		prg := mustProgram(t, c, at(expr.PlaceAuthzCELRule), tc.src)
		if !prg.tracked {
			t.Fatalf("%q is untracked with every input unbounded", tc.src)
		}
		var ee *expr.EvalError
		if _, err := prg.EvalBool(longScope(overLimitTokens)); !errors.As(err, &ee) || ee.Kind != expr.KindCostLimit {
			t.Errorf("%q over %d tokens = %v, want KindCostLimit", tc.src, overLimitTokens, err)
		}
		if got, err := prg.EvalBool(longScope(underLimitTokens)); err != nil || got != tc.long {
			t.Errorf("%q over %d tokens = %v, %v; want %v", tc.src, underLimitTokens, got, err, tc.long)
		}
		if got, err := prg.EvalBool(testVars()); err != nil || !got {
			t.Errorf("%q after the cost limit = %v, %v; want true", tc.src, got, err)
		}
	}
}

// TestCostModelIsPinned checks that every environment estimates and tracks
// under revision 1 of cel-go's cost model (costModelRevision): the
// revision cel-go's estimators see, read through an overload estimator, is
// 1, and the runtime charge follows it, which charges a comparison with an
// empty string at least 1 unit where revision 0 charges nothing.
func TestCostModelIsPinned(t *testing.T) {
	c := newCompiler(t)
	cc, iss := c.compile(at(expr.PlaceRouteMatchWhen), `url.pathEscape(request.path) == "x"`)
	if cc == nil {
		t.Fatal(iss)
	}
	var seen []uint32
	probe := cost.OverloadCostEstimate(overloadPathEscape, func(e cost.Estimator, target *cost.AstNode, args []cost.AstNode) *cost.CallEstimate {
		seen = append(seen, cost.ModelVersionOf(e))
		return estimateEscape(e, target, args)
	})
	if _, err := cc.env.EstimateCost(cc.ast, nominalEstimator{}, probe); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != 1 {
		t.Errorf("cost model revisions seen by the estimator = %v, want [1]", seen)
	}
	empty, iss := c.compile(at(expr.PlaceRouteMatchWhen), `request.method == ""`)
	if empty == nil {
		t.Fatal(iss)
	}
	if got := actualCost(t, empty, testVars()); got != 3 {
		t.Errorf(`actual cost of request.method == "" = %d, want 3 under revision 1`, got)
	}
}
