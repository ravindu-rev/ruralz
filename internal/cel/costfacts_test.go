// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"strconv"
	"strings"
	"testing"

	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/cost"
	"cel.dev/cel-go/common/types"
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

// TestEstimatesAreCelGos checks that both estimates are cel-go's own, with
// no unit added: cel-go's cost model charges what its runtime charges, a
// unit for each field selection of a dyn value, of the claims or a body
// list's elements, and for each selection, presence test or index of an
// operand cel-go evaluates as a relative attribute, such as a literal, a
// call result or a comprehension result. Each estimate covers the actual
// cost at nominal sizes, a loop over an index into a map result included.
func TestEstimatesAreCelGos(t *testing.T) {
	items := func() *expr.Vars {
		v := testVars()
		v.Request.Body = bodyItems(nominalContainerSize, map[string]any{"ok": true, "a": map[string]any{"b": "y"}})
		return v
	}
	lists := func() *expr.Vars {
		inner := make([]any, nominalContainerSize)
		for i := range inner {
			inner[i] = map[string]any{"a": map[string]any{"b": "y"}}
		}
		v := testVars()
		v.Request.Body = celtypes.FromNative(map[string]any{"lists": []any{inner}})
		return v
	}
	tests := []struct {
		name, src string
		vars      func() *expr.Vars
	}{
		{"one selection of the claims", `auth.claims.scope == "x"`, testVars},
		{"selections in a loop over a body list", `request.body.items.exists(i, i.a.b == "z")`, items},
		{"selections in a loop over a filter result", `request.body.items.filter(i, i.ok).exists(i, i.a == "z")`, items},
		{"a loop over an index into a map result", `request.body.lists.map(l, l)[0].exists(x, x.a.b == 1)`, lists},
		{"index of an empty list literal", `[][0]`, testVars},
		{"index of a map literal", `{"a": 1}["a"]`, testVars},
		{"selection of a map literal", `{"a": 1}.a`, testVars},
		{"presence test on a map literal", `has({"a": 1}.a)`, testVars},
		{"indexes of one list literal", `[[1]][0][0]`, testVars},
		{"index of a call result", `([1] + [2])[0]`, testVars},
		{"index with a computed key", `[1][size([1]) - 1]`, testVars},
		{"selections of a dyn call result", `dyn({"a": {"b": 1}}).a.b`, testVars},
		{"index of a comprehension result", `[1, 2].map(x, x)[0]`, testVars},
		{"two indexed literals", `[1][0] + [2][0]`, testVars},
		{"an index in every iteration", `[1, 2, 3].all(x, [x][0] > 0)`, testVars},
		{"a selection in every inner iteration", `[[1, 2, 3]].map(l, l.map(x, {"v": x}.v))`, testVars},
	}
	c := newCompiler(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc, iss := c.compile(at(expr.PlaceTransformRequestBody), tt.src)
			if cc == nil {
				t.Fatalf("compile(%q) = %+v", tt.src, iss)
			}
			raw, err := cc.env.EstimateCost(cc.ast, nominalEstimator{containers: cc.an.containers, elements: cc.facts.elements})
			if err != nil {
				t.Fatal(err)
			}
			if cc.cost.Min != raw.Min || cc.cost.Max != raw.Max {
				t.Errorf("static estimate %+v, cel-go's %+v", cc.cost, raw)
			}
			capped, err := cc.env.EstimateCost(cc.ast, cappedEstimator{})
			if err != nil {
				t.Fatal(err)
			}
			bound, err := cc.cappedBound(expr.InputCaps{})
			if err != nil {
				t.Fatal(err)
			}
			if bound != capped.Max {
				t.Errorf("capped bound %d, cel-go's %d", bound, capped.Max)
			}
			actual := actualCost(t, cc, tt.vars())
			if actual > cc.cost.Max {
				t.Errorf("actual cost %d above the static estimate %d", actual, cc.cost.Max)
			}
			if actual > bound {
				t.Errorf("actual cost %d above the capped bound %d", actual, bound)
			}
		})
	}
}

// TestSplitEstimateCountsTheLastEntry checks the split estimate of
// ext.Strings, which the environment keeps: "," splits into two entries,
// one more than its string's size, so a comprehension whose every
// iteration scans a 32-entry list costs about twice the estimate of a
// one-entry split, and stays within the estimate, which counts the second
// iteration.
func TestSplitEstimateCountsTheLastEntry(t *testing.T) {
	const src = `",".split(",").exists(s, y.exists(k, k == "b"))`
	env, err := celgo.NewEnv(ext.Strings(ext.StringsVersion(5)), celgo.Variable("y", celgo.ListType(celgo.StringType)))
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
	if actual := *det.ActualCost(); actual > est.Max {
		t.Errorf("actual cost %d above ext.Strings' estimate %d", actual, est.Max)
	}
}

// TestCappedEstimatorSizesOnlyValuesFromVariables checks that a cap bounds
// only a value reached from its variable: a field named like a variable
// on a call result and a comprehension variable named like one stay
// unbounded, so their rules stay tracked under the caps, while the
// variable itself is capped.
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

// TestEstimatorsAnswerElementNodes checks both estimators on the nodes
// cel-go asks about the elements of a container, which carry a path and
// a type but no expression: the nominal estimator sizes typed elements by
// their type and dyn ones by the expression's use of the values at that
// path, and the capped estimator sizes the elements of a capped value at
// its cap and leaves any other one, a path-less one included, unbounded.
func TestEstimatorsAnswerElementNodes(t *testing.T) {
	node := func(path string, t *types.Type) cost.AstNode {
		var p []string
		if path != "" {
			p = strings.Split(path, ".")
		}
		return cost.NewAstNode(nil, p, t, nil)
	}
	size := func(sz *cost.SizeEstimate) string {
		if sz == nil {
			return "none"
		}
		return strconv.FormatUint(sz.Min, 10) + ".." + strconv.FormatUint(sz.Max, 10)
	}
	nominal := nominalEstimator{elements: map[string]bool{
		elementKey([]string{"request", "body", "items", "@items"}): true,
	}}
	capped := cappedEstimator{caps: exampleCaps(), clientRequest: true}
	tests := []struct {
		path         string
		typ          *types.Type
		nominal, cap string
	}{
		{"request.headers.@keys", types.StringType, "0..256", "0..65536"},
		{"request.headers.@values", types.StringType, "0..256", "0..65536"},
		{"consumer.tags.@items", types.StringType, "0..256", "0..63"},
		{"consumer.labels.@values", types.StringType, "0..256", "0..63"},
		{"auth.claims.roles.@items", types.DynType, "0..256", "0..65536"},
		{"auth.claims.@items.@items", types.DynType, "0..256", "0..65536"},
		{"request.body.items.@items", types.DynType, "0..32", "none"},
		{"request.body.items.@values", types.DynType, "0..32", "none"},
		{"request.body.items.@items.@items", types.DynType, "0..256", "none"},
		{"steps.@values", types.NewObjectType(celtypes.TypeStep), "1..1", "1..1"},
		{"", types.StringType, "0..256", "none"},
		{"", types.IntType, "none", "none"},
	}
	for _, tt := range tests {
		n := node(tt.path, tt.typ)
		if got := size(nominal.EstimateSize(n)); got != tt.nominal {
			t.Errorf("nominal EstimateSize(%q, %s) = %s, want %s", tt.path, tt.typ, got, tt.nominal)
		}
		if got := size(capped.EstimateSize(n)); got != tt.cap {
			t.Errorf("capped EstimateSize(%q, %s) = %s, want %s", tt.path, tt.typ, got, tt.cap)
		}
	}
	if got := nominal.EstimateSize(cost.NewAstNode(nil, nil, nil, nil)); got != nil {
		t.Errorf("nominal EstimateSize of an untyped node = %+v, want none", got)
	}
}

// TestCostFactsElements checks the element uses the nominal estimator
// reads: a dyn value read by index or as a comprehension variable below a
// Ruralz variable, keyed by its container's path whatever element step
// names it, true when every value read there is used as a container. A
// typed element, a value of a call result and a shadowing comprehension
// variable record nothing.
func TestCostFactsElements(t *testing.T) {
	c := newCompiler(t)
	src := `request.body.items.exists(i, i.exists(j, j == "z")) &&
		request.body["a"].exists(x, x == 1) && request.body["b"] == "c" &&
		auth.claims.roles[0] == "admin" &&
		consumer.tags.exists(t, t == "eu") && request.headers["x"] == "y" &&
		dyn(request.body).items.exists(d, d == 1) &&
		[request.body].exists(request, request.items.exists(r, r == 1))`
	cc, iss := c.compile(at(expr.PlaceAuthzCELRule), src)
	if cc == nil {
		t.Fatalf("compile(%q) = %+v", src, iss)
	}
	want := map[string]bool{
		elementKey([]string{"request", "body", "items", "@items"}):           true,
		elementKey([]string{"request", "body", "items", "@items", "@items"}): false,
		elementKey([]string{"request", "body", "@values"}):                   false,
		elementKey([]string{"request", "body", "@values", "@items"}):         false,
		elementKey([]string{"auth", "claims", "roles", "@items"}):            false,
	}
	if len(cc.facts.elements) != len(want) {
		t.Errorf("elements %q, want %q", keys(cc.facts.elements), keys(want))
	}
	for k, v := range want {
		if got, ok := cc.facts.elements[k]; !ok || got != v {
			t.Errorf("elements[%q] = %v, %v; want %v", strings.ReplaceAll(k, pathSeparator, "."), got, ok, v)
		}
	}
}

// keys returns the element keys of m with their steps joined by dots.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, strings.ReplaceAll(k, pathSeparator, "."))
	}
	return out
}
