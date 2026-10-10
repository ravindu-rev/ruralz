// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"
	"testing"
)

// TestFlowBounds covers the bounds of goccy's parse of a flow collection
// (01 req 9; 11 req 17, 26; blockers of the sixth WP-33 review). The paths
// goccy builds for nested flow mappings with long keys, and the token
// moves of the nulls it inserts for mapping entries without a value, are
// RZ-CFG-001 past their bounds, at the token that passes the path bound or
// at the collection that holds too many such entries; collections under
// the bounds parse, and each outermost collection has bounds of its own.
// Scalar tags before ',' or ':', whose default value goccy inserts the same
// way, and a collection a document leaves open, which goccy's parse fills
// with nulls before it fails at the end, are bounded too (blocker of the
// seventh WP-33 review).
func TestFlowBounds(t *testing.T) {
	distinct := func(n int) string {
		var b strings.Builder
		b.WriteString("a: {")
		for i := range n {
			b.WriteString("k" + itoa(i) + ",")
		}
		b.WriteString("z}\n")
		return b.String()
	}
	pairs := "a: " + strings.Repeat("["+strings.Repeat("k", 1000)+": ", 40) + "[" + strings.Repeat("1,", 2000) + "1]" + strings.Repeat("]", 40) + "\n"
	var many strings.Builder
	for i := range 40 {
		many.WriteString("k" + itoa(i) + ": " + longKeysInFlow(10, 100, 2000)[3:])
	}
	tests := []struct {
		name, src, want, msg string
	}{
		{"long keys in flow mappings", longKeysInFlow(60, 1000, 16384), "RZ-CFG-001@1:", msgFlowPath},
		{"long keys of single-pair mappings", pairs, "RZ-CFG-001@1:", msgFlowPath},
		{"short keys in flow mappings", longKeysInFlow(10, 100, 2000), "", ""},
		{"many collections under the bound", many.String(), "", ""},
		{"keys without values under the bound", distinct(8000), "", ""},
		{"keys without values past the bound", distinct(12000), "RZ-CFG-001@1:4", msgFlowNulls},
		{"keys without values in a sequence", "a: [" + strings.Repeat("{k},", 12000) + "1]\n", "RZ-CFG-001@1:4", msgFlowNulls},
		{"single-pair mappings without values", "a: [" + strings.Repeat("k:\n,", 12000) + "k:\n]\n", "RZ-CFG-001@1:4", msgFlowNulls},
		{"explicit keys without values in a sequence", "a: [" + strings.Repeat("? k\n,", 12000) + "k]\n", "RZ-CFG-001@1:4", msgFlowNulls},
		{"plain scalars ending in ':'", "a: [" + strings.Repeat("k:,", 12000) + "k:]\n", "RZ-CFG-001@1:6", msgFlowColonEnd},
		{"tagged empty nodes past the bound", taggedEmpties(12000, false), "RZ-CFG-001@1:4", msgFlowNulls},
		{"tagged empty values past the bound", taggedEmpties(8000, true), "RZ-CFG-001@1:4", msgFlowNulls},
		{"tagged empty nodes under the bound", taggedEmpties(4000, false), "", ""},
		{"keys without values left open", "a: {" + strings.Repeat("a,", 12000) + "\n", "RZ-CFG-001@1:4", msgFlowNulls},
		{"keys without values left open before a document", "a: {" + strings.Repeat("a,", 12000) + "\n---\nb: 1\n", "RZ-CFG-001@1:4", msgFlowNulls},
		{"keys without values left open under the bound", "a: {" + strings.Repeat("a,", 4000) + "\n", "RZ-CFG-001@", ""},
	}
	for _, tt := range tests {
		_, diags := parseYAML(t, tt.src)
		got := codes(diags)
		if !strings.HasPrefix(got, tt.want) || (tt.want == "") != (got == "") {
			t.Errorf("%s: %.80s, want %s", tt.name, got, tt.want)
		}
		if tt.msg != "" && (len(diags) != 1 || diags[0].Message != tt.msg) {
			t.Errorf("%s: messages %.200v", tt.name, diags)
		}
	}
}

// TestFlowNullSites covers which flow entries get a token goccy inserts
// (01 test plan FuzzProfileYAML: 2 s per input; 11 req 26): a mapping
// entry with no ':', or with nothing after its ':', and a single pair of a
// sequence with nothing after its ':' or an explicit key with no ':' get
// an implicit null (blocker of the eighth WP-33 review); a scalar tag
// directly before ',' or ':' gets its default value, in sequences and
// mappings alike (blocker of the seventh WP-33 review).
func TestFlowNullSites(t *testing.T) {
	for src, want := range map[string]int{
		"a: {a, b: , c: 1, : , ? d}\n":      4,
		"a: {a: {b}, c: [d]}\n":             1,
		"a: [k:\n, k: v, {k}]\n":            2,
		"a: [? k\n, ? k: v, ? k:\n]\n":      2,
		"a: {a: !!str , b: !!str c}\n":      1,
		"a: {a: 1,}\n":                      0,
		"a: {a: # c\n  , b}\n":              2,
		"a: [!!str , !!int # c\n , x]\n":    2,
		"a: [!!str x, !!null ]\n":           0,
		"a: {!!str : v, !!bool , !!map }\n": 4,
	} {
		p := &fileParser{path: "f.yaml", opts: Options{}.withDefaults()}
		pass := &tokenPass{p: p, maxDepth: 64, maxTokens: 1000, splitAt: 1}
		tks, _ := scanString(src)
		pass.run(tks, 0, columnsOf(tks, src), src)
		if pass.nulls != want || len(p.diags) != 0 {
			t.Errorf("%q: %d nulls, want %d; %q", src, pass.nulls, want, codes(p.diags))
		}
	}
	for n, want := range map[int]int{0: 1, 9: 1, 10: 2, 99: 2, 100: 3, 123456: 6} {
		if got := digitsOf(n); got != want {
			t.Errorf("digitsOf(%d) = %d, want %d", n, got, want)
		}
	}
}
