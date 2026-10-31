// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"errors"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// bodyVars returns an activation whose request body is v.
func bodyVars(v expr.Value) *expr.Vars {
	vars := testVars()
	vars.Request.Body = v
	return vars
}

// TestDecodeJSONReadsInCEL checks the decoded values CEL sees: integers
// within int64 as int, other numbers as double, keys in ascending order.
func TestDecodeJSONReadsInCEL(t *testing.T) {
	c := newCompiler(t)
	v, built, err := c.DecodeJSON([]byte(`{"b": 1, "a": 9007199254740993, "f": 1.5, "e": 1e3, "s": "x", "n": null, "l": [true]}`), 0)
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if built <= 0 {
		t.Errorf("built = %d, want the decoded size", built)
	}
	cases := []string{
		`request.body.map(k, k) == ["a", "b", "e", "f", "l", "n", "s"]`,
		`request.body.a == 9007199254740993 && type(request.body.a) == int`,
		`type(request.body.f) == double && type(request.body.e) == double && request.body.e == 1000.0`,
		`request.body.s == "x" && request.body.n == null && request.body.l[0]`,
	}
	for _, src := range cases {
		got, err := mustProgram(t, c, at(expr.PlaceAuthzCELRule), src).EvalBool(t.Context(), bodyVars(v))
		if err != nil || !got {
			t.Errorf("%q = %v, %v; want true", src, got, err)
		}
	}
}

// TestDecodeJSONIsStrict checks that invalid UTF-8 and a repeated member
// name at any depth are errors, and that the returned value then fails an
// expression that reads it with KindBody.
func TestDecodeJSONIsStrict(t *testing.T) {
	c := newCompiler(t)
	cases := map[string]struct {
		data []byte
		kind error
	}{
		"duplicate name":        {[]byte(`{"a": 1, "a": 2}`), jsonval.ErrDuplicateName},
		"nested duplicate name": {[]byte(`{"x": {"model": "a", "model": "b"}}`), jsonval.ErrDuplicateName},
		"invalid UTF-8":         {[]byte("{\"a\": \"\xff\"}"), jsonval.ErrInvalidUTF8},
		"not JSON":              {[]byte(`{"a": }`), jsonval.ErrSyntax},
	}
	prg := mustProgram(t, c, at(expr.PlaceAuthzCELRule), `request.body.a == 1`)
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			v, _, err := c.DecodeJSON(tc.data, 0)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("DecodeJSON error = %v, want %v", err, tc.kind)
			}
			if errors.Is(err, expr.ErrTooLarge) {
				t.Errorf("a malformed body reads as too large: %v", err)
			}
			_, evalErr := prg.EvalBool(t.Context(), bodyVars(v))
			var ee *expr.EvalError
			if !errors.As(evalErr, &ee) || ee.Kind != expr.KindBody {
				t.Errorf("reading the failed body = %v, want KindBody", evalErr)
			}
		})
	}
}

// TestDecodeJSONBudget checks that a document past its budget fails with
// expr.ErrTooLarge, charging what it built until then, and that the same
// document fits a budget equal to its cost.
func TestDecodeJSONBudget(t *testing.T) {
	c := newCompiler(t)
	data := []byte(`{"items": ["a", "b", "c", "d"], "total": 12}`)
	_, cost, err := c.DecodeJSON(data, 0)
	if err != nil {
		t.Fatalf("DecodeJSON without budget: %v", err)
	}
	if _, built, err := c.DecodeJSON(data, cost); err != nil || built != cost {
		t.Errorf("DecodeJSON at its cost = %d, %v; want %d, nil", built, err, cost)
	}
	_, built, err := c.DecodeJSON(data, cost-1)
	if !errors.Is(err, expr.ErrTooLarge) || !errors.Is(err, jsonval.ErrTooLarge) {
		t.Fatalf("DecodeJSON past budget error = %v, want expr.ErrTooLarge", err)
	}
	if built <= cost-1 || built > cost {
		t.Errorf("built = %d, want above the budget %d and at most the full cost %d", built, cost-1, cost)
	}
}
