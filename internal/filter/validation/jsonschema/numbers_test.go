// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNumberInRange covers the proposed number bounds (package
// documentation): every double and long integers pass; long literals and
// large exponents do not.
func TestNumberInRange(t *testing.T) {
	for _, tc := range []struct {
		lit  string
		want bool
	}{
		{"0", true},
		{"-12.5", true},
		{"12345678901234567890", true},
		{"1e400", true},
		{"1.7976931348623157e308", true},
		{"4.9e-324", true},
		{"1e1000", true},
		{"1E+1000", true},
		{"1e-1000", true},
		{"1e0001000", true},
		{"1e1001", false},
		{"1e-1001", false},
		{"1e99999", false},
		{"1e999999999999", false},
		{"0e9999", false},
		{strings.Repeat("9", MaxNumberBytes), true},
		{strings.Repeat("9", MaxNumberBytes+1), false},
		{"1e" + strings.Repeat("0", MaxNumberBytes-3) + "5", true},
	} {
		if got := numberInRange(tc.lit); got != tc.want {
			t.Errorf("numberInRange(%q) = %v, want %v", tc.lit, got, tc.want)
		}
	}
}

func TestNumbersInRange(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		want bool
	}{
		{`null`, true},
		{`{"a":[1,2,{"b":3e5}],"c":"1e9999"}`, true},
		{`[1,[2,[1e1001]]]`, false},
		{`{"a":{"b":1e-5000}}`, false},
	} {
		d := json.NewDecoder(strings.NewReader(tc.doc))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		if got := numbersInRange(v); got != tc.want {
			t.Errorf("numbersInRange(%s) = %v, want %v", tc.doc, got, tc.want)
		}
	}
}
