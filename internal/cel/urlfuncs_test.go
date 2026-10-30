// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"strings"
	"testing"

	"cel.dev/cel-go/checker"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestURLEscapes checks url.pathEscape and url.queryEscape
// (docs/architecture/02-configuration-model.md "Limits"): upper-case
// hexadecimal digits; pathEscape keeps the unreserved characters, the
// sub-delimiters, ":" and "@", so its result is one path segment with
// "/", "?", "#" and "%" escaped; queryEscape keeps only the unreserved
// characters, so "&", "=", "+" and the space are escaped too; every byte
// of a multi-byte character is escaped.
func TestURLEscapes(t *testing.T) {
	c := newCompiler(t)
	tests := []struct{ src, want string }{
		{`url.pathEscape("AZaz09-._~")`, "AZaz09-._~"},
		{`url.pathEscape("!$&'()*+,;=:@")`, "!$&'()*+,;=:@"},
		{`url.pathEscape("a/b?c#d%e")`, "a%2Fb%3Fc%23d%25e"},
		{`url.pathEscape("a b\"<>[]{}|\\^` + "`" + `")`, "a%20b%22%3C%3E%5B%5D%7B%7D%7C%5C%5E%60"},
		{`url.pathEscape("é")`, "%C3%A9"},
		{`url.pathEscape("\u007f\u0000")`, "%7F%00"},
		{`url.pathEscape("")`, ""},
		{`url.queryEscape("AZaz09-._~")`, "AZaz09-._~"},
		{`url.queryEscape("a&b=c+d e")`, "a%26b%3Dc%2Bd%20e"},
		{`url.queryEscape("!$'()*,;:@/?#%")`, "%21%24%27%28%29%2A%2C%3B%3A%40%2F%3F%23%25"},
		{`url.queryEscape("é")`, "%C3%A9"},
		{`"/orders/" + url.pathEscape(request.pathParams.orderId)`, "/orders/42"},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			prg, iss := c.NewBuilder(nil, expr.InputCaps{}).Compile(sequential(expr.PlaceStepPathExpression), tt.src)
			if prg == nil {
				t.Fatalf("Compile: %+v", iss)
			}
			got, err := prg.EvalString(t.Context(), testVars())
			if err != nil || got != tt.want {
				t.Errorf("EvalString = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// TestURLFunctionsInEveryPlace checks that the url functions compile in
// every CEL place, each with its own result type.
func TestURLFunctionsInEveryPlace(t *testing.T) {
	c := newCompiler(t)
	for _, p := range expr.Places() {
		site := expr.Site{Place: p.ID}
		if p.ID == expr.PlaceStepPathExpression || p.ID == expr.PlaceStepWhen {
			site.Mode = v1alpha1.CompositionModeSequential
		}
		src := `url.pathEscape("a/b") + url.queryEscape("c d")`
		switch p.Result {
		case expr.ResultBool:
			src = `url.pathEscape("a/b") == url.queryEscape("a/b")`
		case expr.ResultInt:
			src = `size(url.pathEscape("a/b") + url.queryEscape("c d"))`
		case expr.ResultString, expr.ResultDyn:
		}
		if iss := c.Check(site, src); len(iss) > 0 {
			t.Errorf("%s: Check(%q) = %+v", p.ID, src, iss)
		}
	}
}

// sizedNode is a checker.AstNode with a computed size.
type sizedNode struct{ size *checker.SizeEstimate }

func (n sizedNode) Path() []string                      { return nil }
func (sizedNode) Type() *types.Type                     { return types.StringType }
func (sizedNode) Expr() ast.Expr                        { return nil }
func (n sizedNode) ComputedSize() *checker.SizeEstimate { return n.size }

// TestEscapeEstimate checks the static cost model of a url function: 1
// plus 0.1 unit per input byte, rounded up, with the result sized at three
// times the input.
func TestEscapeEstimate(t *testing.T) {
	est := estimateEscape(nominalEstimator{}, nil, []checker.AstNode{sizedNode{&checker.SizeEstimate{Min: 2, Max: 256}}})
	if est == nil {
		t.Fatal("no estimate")
	}
	if est.CostEstimate.Min != 1+1 || est.CostEstimate.Max != 1+26 {
		t.Errorf("cost = %+v, want {2 27}", est.CostEstimate)
	}
	if est.ResultSize == nil || est.ResultSize.Min != 2 || est.ResultSize.Max != 768 {
		t.Errorf("result size = %+v, want {2 768}", est.ResultSize)
	}
	if got := estimateEscape(nominalEstimator{}, nil, nil); got != nil {
		t.Errorf("estimate without an argument = %+v, want nil", got)
	}
}

// TestEscapeRuntimeCost checks the runtime cost of a url function call, 1
// plus 0.1 unit per input byte rounded up, charged by tracked programs and
// within the static estimate.
func TestEscapeRuntimeCost(t *testing.T) {
	c := newCompiler(t)
	site := sequential(expr.PlaceStepPathExpression)
	v := testVars()
	v.Request.Path = "/" + strings.Repeat("x", 99)
	base, iss := c.compile(site, `request.path`)
	if base == nil {
		t.Fatal(iss)
	}
	for _, src := range []string{`url.pathEscape(request.path)`, `url.queryEscape(request.path)`} {
		cc, iss := c.compile(site, src)
		if cc == nil {
			t.Fatal(iss)
		}
		got := actualCost(t, cc, v) - actualCost(t, base, v)
		if got != 1+10 {
			t.Errorf("%s: call cost = %d, want 11 for a 100-byte input", src, got)
		}
		if actual := actualCost(t, cc, v); actual > cc.cost.Max {
			t.Errorf("%s: actual cost %d above the static estimate %d", src, actual, cc.cost.Max)
		}
	}
}

// TestEscapeSizesSplit checks that the comprehension sizer bounds a url
// function's result at three times its input, so a split of it counts at
// most one entry more.
func TestEscapeSizesSplit(t *testing.T) {
	c := newCompiler(t)
	cc, iss := c.compile(at(expr.PlaceAuthzCELRule), `url.pathEscape("ab").split("%").exists(s, s == "x")`)
	if cc == nil {
		t.Fatal(iss)
	}
	a := cc.ast.NativeRep()
	s := &sizer{a: a, facts: cc.facts, est: nominalEstimator{}, iters: map[int64]sized{}}
	comp := a.Expr()
	if comp.Kind() != ast.ComprehensionKind {
		t.Fatalf("root is %v, want the exists comprehension", comp.Kind())
	}
	if n, ok := s.iterations(comp.ID()); !ok || n != 3*2+1 {
		t.Errorf("iterations = %d, %v; want 7", n, ok)
	}
}
