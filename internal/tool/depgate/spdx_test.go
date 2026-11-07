// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"slices"
	"strings"
	"testing"
)

// TestParseSPDX covers the SPDX 2.3 grammar: AND, OR, WITH and
// parentheses with WITH binding tighter than AND and AND tighter than OR,
// "+", LicenseRef and DocumentRef forms, identifiers in any case, and
// Cargo's deprecated "/" only where legacySlash allows it. String renders
// the parse canonically.
func TestParseSPDX(t *testing.T) {
	tests := []struct {
		in          string
		legacySlash bool
		want        string
	}{
		{"MIT", false, "MIT"},
		{"mit", false, "mit"},
		{"MIT OR Apache-2.0", false, "MIT OR Apache-2.0"},
		{"  MIT\tOR\nApache-2.0 ", false, "MIT OR Apache-2.0"},
		{"(MIT OR Apache-2.0) AND Unicode-3.0", false, "(MIT OR Apache-2.0) AND Unicode-3.0"},
		{"MIT AND Apache-2.0 OR BSD-3-Clause", false, "MIT AND Apache-2.0 OR BSD-3-Clause"},
		{"MIT AND (Apache-2.0 OR BSD-3-Clause)", false, "MIT AND (Apache-2.0 OR BSD-3-Clause)"},
		{"(MIT OR GPL-2.0-only)", false, "MIT OR GPL-2.0-only"},
		{"((MIT))", false, "MIT"},
		{"MIT OR (ISC OR Zlib)", false, "MIT OR ISC OR Zlib"},
		{"Apache-2.0 WITH LLVM-exception OR Apache-2.0 OR MIT", false, "Apache-2.0 WITH LLVM-exception OR Apache-2.0 OR MIT"},
		{"GPL-2.0-or-later WITH Bison-exception-2.2", false, "GPL-2.0-or-later WITH Bison-exception-2.2"},
		{"Apache-2.0+", false, "Apache-2.0+"},
		{"GPL-2.0+ WITH Classpath-exception-2.0", false, "GPL-2.0+ WITH Classpath-exception-2.0"},
		{"LicenseRef-Proprietary", false, "LicenseRef-Proprietary"},
		{"DocumentRef-spdx-tool-1.2:LicenseRef-MIT-Style-2", false, "DocumentRef-spdx-tool-1.2:LicenseRef-MIT-Style-2"},
		{"Apache-2.0 WITH AdditionRef-Custom-1", false, "Apache-2.0 WITH AdditionRef-Custom-1"},
		{"MIT/Apache-2.0", true, "MIT OR Apache-2.0"},
		{"Apache-2.0/MIT AND ISC", true, "Apache-2.0 OR MIT AND ISC"},
	}
	for _, tt := range tests {
		n, err := parseSPDX(tt.in, tt.legacySlash)
		if err != nil {
			t.Errorf("parseSPDX(%q, %v) = error %v, want %q", tt.in, tt.legacySlash, err, tt.want)
			continue
		}
		if got := n.String(); got != tt.want {
			t.Errorf("parseSPDX(%q, %v) = %q, want %q", tt.in, tt.legacySlash, got, tt.want)
		}
	}
}

// TestParseSPDXErrors: anything outside the grammar is refused with a
// message that says why, lower-case operators included.
func TestParseSPDXErrors(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", "the expression is empty"},
		{"   ", "the expression is empty"},
		{"MIT OR", "the expression ends where a license is expected"},
		{"AND MIT", `"AND" stands where a license is expected`},
		{"(MIT", "an opening parenthesis is not closed"},
		{"MIT)", "a closing parenthesis has no opening one"},
		{"()", `")" stands where a license is expected`},
		{"MIT or Apache-2.0", `"or" is not an operator: SPDX operators are AND, OR and WITH in upper case`},
		{"MIT and Apache-2.0", `"and" is not an operator`},
		{"Apache 2.0", `"2.0" follows a license without an operator between them`},
		{"MIT (Apache-2.0)", `"(" follows a license without an operator between them`},
		{"MIT/Apache-2.0", `character '/' is not part of an SPDX expression`},
		{"MIT, Apache-2.0", `character ',' is not part of an SPDX expression`},
		{"MIT WITH", "the expression ends after WITH"},
		{"MIT WITH (LLVM-exception)", `WITH is followed by "(", not a license exception`},
		{"MIT WITH LLVM-exception WITH Bison-exception-2.2", "WITH follows a single license, which takes one exception"},
		{"(MIT OR Apache-2.0) WITH LLVM-exception", "WITH follows a single license, not a parenthesized expression"},
		{"MIT++", `"MIT++" is not a license identifier or LicenseRef`},
		{"+", `"+" is not a license identifier or LicenseRef`},
		{"Foo:Bar", `"Foo:Bar" is not a license identifier or LicenseRef`},
		{"DocumentRef-x:Bar", `"DocumentRef-x:Bar" is not a license identifier or LicenseRef`},
		{"MIT WITH DocumentRef-x:Bar", `WITH is followed by "DocumentRef-x:Bar", not a license exception`},
		{"MIT AND AND ISC", `"AND" stands where a license is expected`},
		{"MIT \u00a9", "character '\u00a9' is not part of an SPDX expression"},
	}
	for _, tt := range tests {
		n, err := parseSPDX(tt.in, false)
		if err == nil {
			t.Errorf("parseSPDX(%q) = %q, want an error containing %q", tt.in, n, tt.want)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("parseSPDX(%q) = error %q, want one containing %q", tt.in, err, tt.want)
		}
	}
}

// admitting returns an admission function for the given terms, compared
// by admission key.
func admitting(t *testing.T, terms ...string) func(string) bool {
	t.Helper()
	keys := map[string]bool{}
	for _, term := range terms {
		k, err := spdxTermKey(term)
		if err != nil {
			t.Fatal(err)
		}
		keys[k] = true
	}
	return func(k string) bool { return keys[k] }
}

// TestSPDXResolves: OR resolves when one operand does, AND only when every
// one does; identifiers match ignoring case; "X+" resolves as X, the
// version it names being one of its choices; "X WITH E" only when that
// pair is admitted, never through X alone, since an exception such as
// Commons-Clause restricts; a LicenseRef only when admitted by name.
func TestSPDXResolves(t *testing.T) {
	admit := admitting(t, "MIT", "Apache-2.0", "ISC", "Unicode-3.0", "Apache-2.0 WITH LLVM-exception")
	tests := []struct {
		expr string
		want bool
	}{
		{"MIT", true},
		{"mit", true},
		{"APACHE-2.0", true},
		{"GPL-3.0-only", false},
		{"MIT OR GPL-3.0-only", true},
		{"GPL-3.0-only OR MIT", true},
		{"GPL-3.0-only OR AGPL-3.0-only", false},
		{"MIT AND ISC", true},
		{"MIT AND GPL-2.0-only", false},
		{"(MIT OR Apache-2.0) AND Unicode-3.0", true},
		{"(MIT OR GPL-3.0-only) AND (ISC OR SSPL-1.0)", true},
		{"(GPL-3.0-only OR SSPL-1.0) AND MIT", false},
		{"MIT AND GPL-3.0-only OR ISC", true},
		{"Apache-2.0+", true},
		{"GPL-2.0+", false},
		{"Apache-2.0 WITH LLVM-exception", true},
		{"apache-2.0 with llvm-exception", false}, // lower-case WITH does not parse
		{"Apache-2.0 WITH Commons-Clause", false},
		{"MIT WITH Commons-Clause", false},
		{"Apache-2.0 WITH Commons-Clause OR MIT", true},
		{"LicenseRef-Proprietary", false},
	}
	for _, tt := range tests {
		n, err := parseSPDX(tt.expr, false)
		if err != nil {
			if tt.want {
				t.Errorf("parseSPDX(%q) = %v", tt.expr, err)
			}
			continue
		}
		if got := n.resolves(admit); got != tt.want {
			t.Errorf("parseSPDX(%q).resolves = %v, want %v", tt.expr, got, tt.want)
		}
	}
}

// TestSPDXBlocking names the terms that keep an expression from
// resolving, as written and once each, never a term of an operand that
// resolves.
func TestSPDXBlocking(t *testing.T) {
	admit := admitting(t, "MIT", "ISC")
	tests := []struct {
		expr string
		want []string
	}{
		{"MIT", nil},
		{"GPL-3.0-only", []string{"GPL-3.0-only"}},
		{"MIT AND GPL-2.0-only", []string{"GPL-2.0-only"}},
		{"Zlib OR GPL-3.0-only", []string{"Zlib", "GPL-3.0-only"}},
		{"(MIT OR GPL-3.0-only) AND LicenseRef-X", []string{"LicenseRef-X"}},
		{"(Zlib OR GPL-3.0-only) AND (GPL-3.0-only OR ISC) AND SSPL-1.0", []string{"Zlib", "GPL-3.0-only", "SSPL-1.0"}},
		{"Apache-2.0+ WITH LLVM-exception", []string{"Apache-2.0+ WITH LLVM-exception"}},
	}
	for _, tt := range tests {
		n, err := parseSPDX(tt.expr, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := n.blocking(admit); !slices.Equal(got, tt.want) {
			t.Errorf("parseSPDX(%q).blocking = %q, want %q", tt.expr, got, tt.want)
		}
	}
}

// TestSPDXKeys lists every term's admission key once: lower case, "+"
// dropped, WITH exceptions kept.
func TestSPDXKeys(t *testing.T) {
	n, err := parseSPDX("(MIT OR Apache-2.0+) AND mit AND Apache-2.0 WITH LLVM-exception", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := n.keys(), []string{"mit", "apache-2.0", "apache-2.0 with llvm-exception"}; !slices.Equal(got, want) {
		t.Errorf("keys = %q, want %q", got, want)
	}
}

// TestSPDXTermKey: a policy entry is one license term; an expression or
// anything unparsable is refused.
func TestSPDXTermKey(t *testing.T) {
	for in, want := range map[string]string{
		"Unicode-3.0":                    "unicode-3.0",
		"Apache-2.0 WITH LLVM-exception": "apache-2.0 with llvm-exception",
		"EDL-1.0":                        "edl-1.0",
	} {
		if got, err := spdxTermKey(in); err != nil || got != want {
			t.Errorf("spdxTermKey(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"MIT OR Apache-2.0", "MIT AND ISC", "MIT/Apache-2.0", ""} {
		if got, err := spdxTermKey(in); err == nil {
			t.Errorf("spdxTermKey(%q) = %q, want an error", in, got)
		}
	}
}
