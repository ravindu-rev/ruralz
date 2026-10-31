// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"encoding/json"
	"testing"
)

func TestEqual(t *testing.T) {
	obj := func(ms ...Member) *Object { return &Object{members: ms} }
	cases := []struct {
		a, b any
		eq   bool
	}{
		{nil, nil, true},
		{nil, (*Object)(nil), true},
		{map[string]any(nil), []any(nil), true},
		{nil, false, false},
		{true, true, true},
		{true, false, false},
		{"a", "a", true},
		{"a", "b", false},
		{"1", json.Number("1"), false},
		{json.Number("1"), json.Number("1"), true},
		{json.Number("1"), json.Number("1.0"), false},
		{[]any{"a", nil}, []any{"a", nil}, true},
		{[]any{"a"}, []any{"a", nil}, false},
		{[]any{"a"}, []any{"b"}, false},
		{[]any{}, obj(), false},
		{obj(Member{"a", "1"}, Member{"b", "2"}), obj(Member{"b", "2"}, Member{"a", "1"}), true},
		{obj(Member{"a", "1"}), map[string]any{"a": "1"}, true},
		{map[string]any{"a": "1"}, obj(Member{"a", "1"}), true},
		{obj(Member{"a", "1"}), obj(Member{"a", "2"}), false},
		{obj(Member{"a", "1"}), obj(Member{"b", "1"}), false},
		{obj(Member{"a", "1"}), obj(Member{"a", "1"}, Member{"b", "1"}), false},
		{obj(), "x", false},
		{map[string]any{"a": "1"}, map[string]any{}, false},
		{map[string]any{"a": "1"}, map[string]any{"a": "1"}, true},
		{obj(Member{"a", "1"}), map[string]any{"a": "1", "b": "2"}, false},
		{3, 3, false},
	}
	for _, c := range cases {
		if Equal(c.a, c.b) != c.eq {
			t.Fatalf("Equal(%#v, %#v) = %v", c.a, c.b, !c.eq)
		}
	}
}

func TestCostOtherTypes(t *testing.T) {
	for _, v := range []any{1, int64(2), uint8(3), 1.5, float32(2)} {
		if Cost(v) != CostValue+8 {
			t.Fatalf("Cost(%T) = %d", v, Cost(v))
		}
	}
	for _, v := range []any{nil, (*Object)(nil), struct{}{}} {
		if Cost(v) != CostValue {
			t.Fatalf("Cost(%#v) = %d", v, Cost(v))
		}
	}
}
