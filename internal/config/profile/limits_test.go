// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import "testing"

// TestGoccyLimitations pins valid YAML 1.2 that goccy v1.19.2 refuses and
// the profile keeps refused (doc.go, goccy v1.19.2 limitations; 01 req 8,
// 10, 12, 15; minor findings of the eighth WP-33 review), with the finding
// production gives, so a goccy upgrade that reads one of them shows here.
// None is accepted with another tree: goccy's own parse of the whole
// document refuses each too, or the token pass refuses it before goccy's
// misreading.
func TestGoccyLimitations(t *testing.T) {
	const (
		notAllowed = "syntax error: value is not allowed in this context"
		noBracket  = "syntax error: could not find '[' character corresponding to ']'"
		tabStart   = "syntax error: found character '\t' that cannot start any token"
	)
	tests := []struct {
		src, want, msg string
	}{
		// A tag that ends its line before a block scalar header: {k: "x\n"}.
		{"k: !!str\n  |\n   x\n", "RZ-CFG-001@2:3", notAllowed},
		// An explicit key or an empty value in a flow sequence:
		// [{a: null}].
		{"k: [? a]\n", "RZ-CFG-001@1:8", noBracket},
		{"k: [a: ]\n", "RZ-CFG-001@1:8", noBracket},
		{"k: {x: [a:]}\n", "RZ-CFG-001@1:11", noBracket},
		{"k: [k: ,]\n", "RZ-CFG-001@1:8", "syntax error: unexpected scalar value type"},
		// A plain scalar in a flow collection continued by a line starting
		// with '-': ["a -b"] and {a: "b -c"}.
		{"k: [a\n  -b]\n", "RZ-CFG-001@1:5", msgPlainFlowIndicator},
		{"k: {a: b\n  -c}\n", "RZ-CFG-001@2:3", msgFlowMapEntry},
		// An explicit key whose tag ends the '?' line, which goccy pairs
		// with the '?' alone: {a: b}.
		{"? !!str\n  a\n: b\n", "RZ-CFG-001@3:1", msgExplicitKeyTag},
		// A value over several lines after a tagged implicit key or a
		// quoted explicit key, which goccy's scanner splits into one token
		// per line: {k: "x y"}, {k: "x\n"}, {a: "x -y"}.
		{"!!str k: x\n  y\n", "RZ-CFG-001@2:3", notAllowed},
		{"!!str c: run the\n  job\n", "RZ-CFG-001@2:3", notAllowed},
		{"k:\n  !!str c: a\n    b\n", "RZ-CFG-001@3:5", notAllowed},
		{"- !!str c: a\n    b\n", "RZ-CFG-001@2:5", notAllowed},
		{"a:\n  !!str k: x\n    y\n", "RZ-CFG-001@3:5", notAllowed},
		{"!!str k: |\n  x\n", "RZ-CFG-001@1:10", msgBlockScalar},
		{"- !!str a: |\n      y\n", "RZ-CFG-001@1:12", msgBlockScalar},
		{"? 'a'\n: |\n  x\n", "RZ-CFG-001@2:3", msgBlockScalar},
		{"? \"a\"\n: x\n  -y\n", "RZ-CFG-001@3:3", notAllowed},
		{"? 'a'\n:\n  x\n  y\n", "RZ-CFG-001@4:3", notAllowed},
		// A line of white space holding a tab between two lines of
		// content, or after the last when it is no blank line: {a: 1},
		// {a: {b: 1}}, {a: 1, b: 2} (YAML Test Suite DK95/04). The JSON front
		// end accepts the first.
		{"{\"a\": 1}\n\t\n", "RZ-CFG-001@2:1", tabStart},
		{"a:\n\t# c\n  b: 1\n", "RZ-CFG-001@2:1", tabStart},
		{"a: 1\n\t\nb: 2\n", "RZ-CFG-001@2:1", tabStart},
		// A tagged empty node before a sibling, which goccy nests under the
		// tag: {a: "", b: 1}.
		{"a: !!str\nb: 1\n", "RZ-CFG-001@1:4", "a tagged node has no value"},
	}
	for _, tt := range tests {
		docs, diags := parseYAML(t, tt.src)
		if got := codes(diags); got != tt.want || len(docs) != 0 || diags[0].Message != tt.msg {
			t.Errorf("%q: %q %+v, want %s %q", tt.src, got, diags, tt.want, tt.msg)
		}
		if _, whole := parseWith(t, tt.src, -1); whole == "" {
			t.Errorf("%q: goccy's whole parse accepts it", tt.src)
		}
	}
}
