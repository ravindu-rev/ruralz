// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"strings"
	"testing"

	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/ext"

	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
)

// actualCost evaluates cc over v with cost tracking and returns the cost.
func actualCost(t *testing.T, cc *compiled, v *expr.Vars) uint64 {
	t.Helper()
	prg, err := cc.env.Program(cc.ast, celgo.EvalOptions(celgo.OptTrackCost))
	if err != nil {
		t.Fatal(err)
	}
	_, det, _ := prg.Eval(celtypes.NewActivation(v))
	if det == nil || det.ActualCost() == nil {
		t.Fatalf("Eval(%q) tracked no cost", cc.src)
	}
	return *det.ActualCost()
}

// bodyItems returns n copies of item as a body {"items": [...]}.
func bodyItems(n int, item map[string]any) expr.Value {
	items := make([]any, n)
	for i := range items {
		items[i] = item
	}
	return celtypes.FromNative(map[string]any{"items": items})
}

// TestEstimateChargesDynSelections checks the correction of cel-go's
// estimate: selecting a field of a dyn value costs a unit at runtime that
// cel-go's estimator leaves out, so its estimate falls short of the actual
// cost at nominal sizes, and the static estimate, which adds a unit per
// evaluation of each such selection, does not.
func TestEstimateChargesDynSelections(t *testing.T) {
	tests := []struct {
		name, src string
		vars      func() *expr.Vars
	}{
		{"one selection of the claims", `auth.claims.scope == "x"`, testVars},
		{"selections in a loop over a body list", `request.body.items.exists(i, i.a.b == "z")`, func() *expr.Vars {
			v := testVars()
			v.Request.Body = bodyItems(nominalContainerSize, map[string]any{"a": map[string]any{"b": "y"}})
			return v
		}},
		{"selections in a loop over a filter result", `request.body.items.filter(i, i.ok).exists(i, i.a == "z")`, func() *expr.Vars {
			v := testVars()
			v.Request.Body = bodyItems(nominalContainerSize, map[string]any{"ok": true, "a": "y"})
			return v
		}},
	}
	c := newCompiler(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc, iss := c.compile(at(expr.PlaceAuthzCELRule), tt.src)
			if cc == nil {
				t.Fatalf("compile(%q) = %+v", tt.src, iss)
			}
			raw, err := cc.env.EstimateCost(cc.ast, nominalEstimator{containers: cc.an.containers})
			if err != nil {
				t.Fatal(err)
			}
			actual := actualCost(t, cc, tt.vars())
			if actual <= raw.Max {
				t.Fatalf("cel-go's estimate %d already covers the actual cost %d: the case shows nothing", raw.Max, actual)
			}
			if actual > cc.cost.Max {
				t.Errorf("actual cost %d above the static estimate %d", actual, cc.cost.Max)
			}
		})
	}
}

// TestEstimateChargesRelativeAttributes checks the correction of cel-go's
// estimate for a selection, presence test or index whose operand is not
// an attribute, such as a literal, a call result or a comprehension
// result: cel-go evaluates it as a relative attribute over the operand
// and charges that attribute a unit per evaluation its estimator leaves
// out, so its estimate of these constant expressions falls short of the
// actual cost. The static estimate and the capped bound, which add the
// unit for each evaluation of each such attribute, cover it; the last
// case's nominal sizes alone would hide the shortfall the capped bound
// shows.
func TestEstimateChargesRelativeAttributes(t *testing.T) {
	tests := []struct{ name, src string }{
		{"index of an empty list literal", `[][0]`},
		{"index of a map literal", `{"a": 1}["a"]`},
		{"selection of a map literal", `{"a": 1}.a`},
		{"presence test on a map literal", `has({"a": 1}.a)`},
		{"indexes of one list literal", `[[1]][0][0]`},
		{"index of a call result", `([1] + [2])[0]`},
		{"index with a computed key", `[1][size([1]) - 1]`},
		{"selections of a dyn call result", `dyn({"a": {"b": 1}}).a.b`},
		{"index of a comprehension result", `[1, 2].map(x, x)[0]`},
		{"two indexed literals", `[1][0] + [2][0]`},
		{"an index in every iteration", `[1, 2, 3].all(x, [x][0] > 0)`},
		{"a selection in every inner iteration", `[[1, 2, 3]].map(l, l.map(x, {"v": x}.v))`},
	}
	c := newCompiler(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc, iss := c.compile(at(expr.PlaceTransformRequestBody), tt.src)
			if cc == nil {
				t.Fatalf("compile(%q) = %+v", tt.src, iss)
			}
			// With every input unbounded, cel-go sizes only the literals.
			raw, err := cc.env.EstimateCost(cc.ast, cappedEstimator{paths: cc.facts.paths})
			if err != nil {
				t.Fatal(err)
			}
			actual := actualCost(t, cc, testVars())
			if actual <= raw.Max {
				t.Fatalf("cel-go's estimate %d already covers the actual cost %d: the case shows nothing", raw.Max, actual)
			}
			if actual > cc.cost.Max {
				t.Errorf("actual cost %d above the static estimate %d", actual, cc.cost.Max)
			}
			bound, err := cc.cappedBound(expr.InputCaps{})
			if err != nil {
				t.Fatal(err)
			}
			if actual > bound {
				t.Errorf("actual cost %d above the capped bound %d", actual, bound)
			}
		})
	}
}

// TestSplitEstimateCountsTheLastEntry checks the split estimate: "," splits
// into two entries, one more than ext.Strings' own estimate counts, so a
// comprehension whose every iteration scans a 32-entry list costs about
// twice that estimate, and stays within the replacement, which counts the
// second iteration.
func TestSplitEstimateCountsTheLastEntry(t *testing.T) {
	const src = `",".split(",").exists(s, y.exists(k, k == "b"))`
	estimate := func(t *testing.T, opts ...celgo.EnvOption) (*celgo.Env, *celgo.Ast, uint64) {
		t.Helper()
		base := []celgo.EnvOption{ext.Strings(ext.StringsVersion(5)), celgo.Variable("y", celgo.ListType(celgo.StringType))}
		env, err := celgo.NewEnv(append(base, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		a, iss := env.Compile(src)
		if iss.Err() != nil {
			t.Fatal(iss.Err())
		}
		est, err := env.EstimateCost(a, nominalEstimator{})
		if err != nil {
			t.Fatal(err)
		}
		return env, a, est.Max
	}
	_, _, extOwn := estimate(t)
	env, a, replaced := estimate(t, celgo.CostEstimatorOptions(splitEstimates()...))
	prg, err := env.Program(a, celgo.EvalOptions(celgo.OptTrackCost))
	if err != nil {
		t.Fatal(err)
	}
	y := make([]string, nominalContainerSize)
	for i := range y {
		y[i] = "a"
	}
	out, det, err := prg.Eval(map[string]any{"y": y})
	if err != nil || out.Value() != false {
		t.Fatalf("Eval = %v, %v; want false", out, err)
	}
	actual := *det.ActualCost()
	if actual <= extOwn {
		t.Errorf("ext.Strings' estimate %d covers the actual cost %d", extOwn, actual)
	}
	if actual > replaced {
		t.Errorf("actual cost %d above the replaced estimate %d", actual, replaced)
	}
}

// TestCappedEstimatorSizesOnlyValuesFromVariables checks that a cap bounds
// only a value reached from its variable: a field named like a variable
// on a call result, which cel-go gives a path starting with that name,
// and a comprehension variable named like one stay unbounded, so their
// rules stay tracked under the caps, while the variable itself is capped.
func TestCappedEstimatorSizesOnlyValuesFromVariables(t *testing.T) {
	c := newCompiler(t)
	for _, tt := range []struct {
		src     string
		tracked bool
	}{
		{`request.headers.exists(k, k == "a")`, false},
		{`dyn(request.body).request.headers.exists(k, k == "a")`, true},
		{`[request.body].exists(request, request.headers.exists(k, k == "a"))`, true},
		{`dyn(request.body).consumer.tags.exists(t, t.contains("eu"))`, true},
	} {
		prg := mustCapped(t, c, at(expr.PlaceAuthzCELRule), tt.src, exampleCaps())
		if prg.tracked != tt.tracked {
			t.Errorf("%q tracked %v under the caps, want %v", tt.src, prg.tracked, tt.tracked)
		}
	}
}

// TestBoundFallsBackToTheChainFactor checks the correction where a loop's
// range has no size bound, here an index into a map result: the
// selections in that loop are bounded through their chain's base, so the
// bound is the estimate times one plus the longest such chain, plus one
// unit for each uncounted unit outside a loop. It still covers the actual
// cost.
func TestBoundFallsBackToTheChainFactor(t *testing.T) {
	c := newCompiler(t)
	src := `request.body.lists.map(l, l)[0].exists(x, x.a.b == 1)`
	cc, iss := c.compile(at(expr.PlaceAuthzCELRule), src)
	if cc == nil {
		t.Fatalf("compile(%q) = %+v", src, iss)
	}
	nominal := nominalEstimator{containers: cc.an.containers}
	raw, err := cc.env.EstimateCost(cc.ast, nominal)
	if err != nil {
		t.Fatal(err)
	}
	// x.a.b is a chain of two in the unsized loop; outside any loop are
	// one selection, request.body.lists, and the relative attribute of the
	// index into the map result.
	if want := raw.Max*3 + 2; cc.cost.Max != want {
		t.Errorf("static estimate %d, want %d (raw %d)", cc.cost.Max, want, raw.Max)
	}
	inner := make([]any, nominalContainerSize)
	for i := range inner {
		inner[i] = map[string]any{"a": map[string]any{"b": "y"}}
	}
	v := testVars()
	v.Request.Body = celtypes.FromNative(map[string]any{"lists": []any{inner}})
	if actual := actualCost(t, cc, v); actual > cc.cost.Max {
		t.Errorf("actual cost %d above the static estimate %d", actual, cc.cost.Max)
	}
}

// TestCostFactsPaths checks the paths of the values reached from a Ruralz
// variable: through selections, indexing and comprehension variables, not
// through a call result or a shadowing comprehension variable.
func TestCostFactsPaths(t *testing.T) {
	c := newCompiler(t)
	src := `consumer.tags.exists(t, t == "eu") && request.headers["x"] == "y" && [1].exists(request, request == 1)`
	cc, iss := c.compile(at(expr.PlaceAuthzCELRule), src)
	if cc == nil {
		t.Fatalf("compile(%q) = %+v", src, iss)
	}
	got := map[string]bool{}
	for _, p := range cc.facts.paths {
		got[strings.Join(p, ".")] = true
	}
	for _, want := range []string{"consumer", "consumer.tags", "consumer.tags.@items", "request", "request.headers", "request.headers.@values"} {
		if !got[want] {
			t.Errorf("no path %s; paths %v", want, got)
		}
	}
	for p := range got {
		if strings.HasPrefix(p, "@") {
			t.Errorf("path %s does not start at a variable", p)
		}
	}
	if n := len(cc.facts.uncounted); n != 0 {
		t.Errorf("%d uncounted selections in typed fields, want 0", n)
	}
}
