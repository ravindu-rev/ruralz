// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// fixture returns a recorded `cargo metadata` or `pnpm licenses` output
// from testdata. The outputs were recorded under /ci, which testdata/ci
// mirrors with each shipped package's manifest and legal texts, so the
// paths are rewritten to that mirror.
func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // A fixture of this package.
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join("testdata", "ci"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(data), "/ci/", filepath.ToSlash(root)+"/")
}

// golden compares got with a golden file of testdata, rewriting it with
// -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(file, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file) //nolint:gosec // A golden file of this package.
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the output; rerun with -update after checking it:\n%s", name, got)
	}
}

// testPolicy builds a policy from test tables.
func testPolicy(t *testing.T, ecosystem string, addendum, mpl []string, elections, overrides map[string]string) spdxPolicy {
	t.Helper()
	p, err := newSPDXPolicy(ecosystem, allowedLicenses(), addendum, mpl, elections, overrides)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// realPolicy builds an ecosystem's policy from policy.go.
func realPolicy(t *testing.T, ecosystem string) spdxPolicy {
	t.Helper()
	p, err := ecosystemPolicy(ecosystem)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEcosystemPolicy: the G2 list admits the same licenses in every
// ecosystem; Unicode-3.0 is the Cargo addendum's (ADR-0002) and admitted
// for no npm package; MPL-2.0 needs a named exception in both.
func TestEcosystemPolicy(t *testing.T) {
	cargo, npm := realPolicy(t, ecosystemCargo), realPolicy(t, ecosystemNPM)
	for _, id := range []string{"apache-2.0", "mit", "bsd-2-clause", "bsd-3-clause", "isc"} {
		if !cargo.admitted[id] || !npm.admitted[id] {
			t.Errorf("%s is not admitted in both ecosystems", id)
		}
	}
	if !cargo.admitted["unicode-3.0"] || npm.admitted["unicode-3.0"] {
		t.Errorf("Unicode-3.0 admitted: cargo %v, npm %v; want the Cargo addendum only", cargo.admitted["unicode-3.0"], npm.admitted["unicode-3.0"])
	}
	for _, p := range []spdxPolicy{cargo, npm} {
		if p.admitted["mpl-2.0"] || p.admitted["edl-1.0"] {
			t.Errorf("%s admits MPL-2.0 or EDL-1.0 without an exception or election", p.ecosystem)
		}
	}
}

// TestNewSPDXPolicyKeys: a policy takes only the exception, election and
// override entries keyed "<ecosystem>:<name>"; Go module paths and the
// other ecosystem's entries stay out.
func TestNewSPDXPolicyKeys(t *testing.T) {
	mpl := []string{"cargo:mpl-crate", "npm:mpl-pkg", "github.com/hashicorp/raft"}
	elections := map[string]string{"cargo:elect-crate": "Zlib", "npm:elect-pkg": "Zlib", "github.com/eclipse/paho.golang": "EDL-1.0"}
	overrides := map[string]string{"cargo:x": "MIT", "npm:y": "ISC", "example.com/m": "MIT"}
	p := testPolicy(t, ecosystemCargo, nil, mpl, elections, overrides)
	if !p.mpl["mpl-crate"] || len(p.mpl) != 1 {
		t.Errorf("mpl = %v, want mpl-crate only", p.mpl)
	}
	if p.elections["elect-crate"] != "Zlib" || p.electionKeys["elect-crate"] != "zlib" || len(p.elections) != 1 {
		t.Errorf("elections = %v (%v), want elect-crate only", p.elections, p.electionKeys)
	}
	if p.overrides["x"] != "MIT" || len(p.overrides) != 1 {
		t.Errorf("overrides = %v, want x only", p.overrides)
	}
	if _, err := newSPDXPolicy(ecosystemNPM, allowedLicenses(), []string{"MIT OR ISC"}, nil, nil, nil); err == nil {
		t.Error("an addendum entry that is an expression was accepted")
	}
	if _, err := newSPDXPolicy(ecosystemNPM, allowedLicenses(), nil, nil, map[string]string{"npm:x": "MIT/ISC"}, nil); err == nil {
		t.Error("an election that is no license term was accepted")
	}
}

// TestPolicyTablesHoldNoCopyleft guards the tables of policy.go: every
// addendum entry is one license term, and none is a copyleft or
// source-available license, nor MPL-2.0, which only named exceptions admit
// (ADR-0002); every crate and npm package election is one term and every
// override a valid SPDX expression.
func TestPolicyTablesHoldNoCopyleft(t *testing.T) {
	denied := []string{"gpl", "agpl", "lgpl", "sspl", "busl", "bsl-1.1", "elastic", "rsal", "epl", "eupl", "cddl", "osl", "mpl", "cc-by-sa", "cc-by-nc", "commons-clause", "polyform", "unlicensed"}
	for eco, terms := range licenseAddenda() {
		if eco != ecosystemCargo && eco != ecosystemNPM {
			t.Errorf("addendum of unknown ecosystem %q", eco)
		}
		for _, term := range terms {
			key, err := spdxTermKey(term)
			if err != nil {
				t.Errorf("%s addendum entry %q: %v", eco, term, err)
				continue
			}
			for _, d := range denied {
				if strings.Contains(key, d) {
					t.Errorf("%s addendum entry %q is a copyleft, source-available or MPL-2.0 license", eco, term)
				}
			}
		}
	}
	for k, term := range licenseElections() {
		if strings.HasPrefix(k, ecosystemCargo+":") || strings.HasPrefix(k, ecosystemNPM+":") {
			if _, err := spdxTermKey(term); err != nil {
				t.Errorf("election %s = %q: %v", k, term, err)
			}
		}
	}
	for k, expr := range licenseOverrides() {
		if strings.HasPrefix(k, ecosystemCargo+":") || strings.HasPrefix(k, ecosystemNPM+":") {
			if _, err := parseSPDX(expr, false); err != nil {
				t.Errorf("override %s = %q: %v", k, expr, err)
			}
		}
	}
	for _, eco := range []string{ecosystemCargo, ecosystemNPM} {
		if _, err := ecosystemPolicy(eco); err != nil {
			t.Errorf("ecosystemPolicy(%s): %v", eco, err)
		}
	}
}

// TestCheckPackage applies G2 to single packages: overrides replace the
// declared expression, a package without one or with an invalid one is a
// finding, an election admits the term it names only when the expression
// offers it, MPL-2.0 passes only through a named exception and marks the
// package as used under MPL-2.0, and the Cargo "/" reads as OR for crates
// only.
func TestCheckPackage(t *testing.T) {
	cargo := testPolicy(t, ecosystemCargo, []string{"Unicode-3.0"},
		[]string{"cargo:mpl-crate", "cargo:dual-mpl"},
		map[string]string{"cargo:elect-crate": "Zlib", "cargo:odd-crate": "Zlib", "cargo:elect-mpl": "MPL-2.0"},
		map[string]string{"cargo:reviewed": "MIT OR Apache-2.0", "cargo:broken": "MIT/ISC", "cargo:blank": "BSD-3-Clause"})
	npm := testPolicy(t, ecosystemNPM, nil, nil, nil, nil)
	tests := []struct {
		name     string
		pol      spdxPolicy
		pkg      spdxPackage
		want     string // a substring of the finding; "" passes
		expr     string // the rendered expression, when the package passes
		election string
		override string
		mpl      bool
	}{
		{name: "allowed license", pol: cargo, pkg: spdxPackage{Name: "a", Declared: "MIT"}, expr: "MIT"},
		{name: "OR with one allowed operand", pol: cargo, pkg: spdxPackage{Name: "a", Declared: "Unlicense OR MIT"}, expr: "Unlicense OR MIT"},
		{name: "AND with an addendum operand", pol: cargo, pkg: spdxPackage{Name: "a", Declared: "(MIT OR Apache-2.0) AND Unicode-3.0"}, expr: "(MIT OR Apache-2.0) AND Unicode-3.0"},
		{
			name: "the addendum belongs to its ecosystem", pol: npm, pkg: spdxPackage{Name: "a", Declared: "(MIT OR Apache-2.0) AND Unicode-3.0"},
			want: "license expression (MIT OR Apache-2.0) AND Unicode-3.0 does not resolve to the G2 list or the npm addendum; not admitted: Unicode-3.0 (G2)",
		},
		{name: "copyleft", pol: cargo, pkg: spdxPackage{Name: "a", Declared: "GPL-3.0-only"}, want: "not admitted: GPL-3.0-only (G2)"},
		{name: "AND with a copyleft operand", pol: cargo, pkg: spdxPackage{Name: "a", Declared: "MIT AND GPL-2.0-only"}, want: "not admitted: GPL-2.0-only (G2)"},
		{name: "legacy slash for a crate", pol: cargo, pkg: spdxPackage{Name: "a", Declared: "MIT/Apache-2.0"}, expr: "MIT OR Apache-2.0"},
		{
			name: "no legacy slash for an npm package", pol: npm, pkg: spdxPackage{Name: "a", Declared: "MIT/Apache-2.0"},
			want: `license expression "MIT/Apache-2.0" is not a valid SPDX expression: character '/' is not part of an SPDX expression; add a reviewed override (G2)`,
		},
		{name: "no expression", pol: cargo, pkg: spdxPackage{Name: "a"}, want: "declares no license expression; add a reviewed override (G2)"},
		{name: "blank expression", pol: cargo, pkg: spdxPackage{Name: "a", Declared: "  "}, want: "declares no license expression; add a reviewed override (G2)"},
		{
			name: "what the manifest holds instead", pol: cargo, pkg: spdxPackage{Name: "a", Missing: "declares only the license file L.txt and no license expression"},
			want: "declares only the license file L.txt and no license expression; add a reviewed override (G2)",
		},
		{name: "override replaces a missing expression", pol: cargo, pkg: spdxPackage{Name: "reviewed"}, expr: "MIT OR Apache-2.0", override: "MIT OR Apache-2.0"},
		{name: "override replaces a declared expression", pol: cargo, pkg: spdxPackage{Name: "blank", Declared: "GPL-3.0-only"}, expr: "BSD-3-Clause", override: "BSD-3-Clause"},
		{
			name: "override is strict SPDX", pol: cargo, pkg: spdxPackage{Name: "broken", Declared: "MIT"},
			want: `reviewed override "MIT/ISC" is not a valid SPDX expression: character '/' is not part of an SPDX expression (G2)`,
		},
		{name: "election admits an offered license", pol: cargo, pkg: spdxPackage{Name: "elect-crate", Declared: "Zlib OR GPL-3.0-only"}, expr: "Zlib OR GPL-3.0-only", election: "Zlib"},
		{
			name: "election of a license not offered", pol: cargo, pkg: spdxPackage{Name: "odd-crate", Declared: "MIT OR GPL-3.0-only"},
			want: "elected license Zlib is not among the package's licenses MIT OR GPL-3.0-only (G2)",
		},
		{name: "election does not lift a required license", pol: cargo, pkg: spdxPackage{Name: "elect-crate", Declared: "Zlib AND GPL-3.0-only"}, want: "not admitted: GPL-3.0-only (G2)"},
		{name: "MPL-2.0 without an exception", pol: cargo, pkg: spdxPackage{Name: "a", Declared: "MPL-2.0"}, want: "not admitted: MPL-2.0 (G2)"},
		{name: "MPL-2.0 with an exception", pol: cargo, pkg: spdxPackage{Name: "mpl-crate", Declared: "MPL-2.0"}, expr: "MPL-2.0", mpl: true},
		{name: "MPL-2.0 exception unused when another choice resolves", pol: cargo, pkg: spdxPackage{Name: "dual-mpl", Declared: "MPL-2.0 OR MIT"}, expr: "MPL-2.0 OR MIT"},
		{name: "MPL-2.0 exception needed for an AND", pol: cargo, pkg: spdxPackage{Name: "dual-mpl", Declared: "MPL-2.0 AND MIT"}, expr: "MPL-2.0 AND MIT", mpl: true},
		{name: "MPL-2.0 through an election", pol: cargo, pkg: spdxPackage{Name: "elect-mpl", Declared: "MPL-2.0 OR GPL-3.0-only"}, expr: "MPL-2.0 OR GPL-3.0-only", election: "MPL-2.0", mpl: true},
		{name: "WITH pair not admitted", pol: cargo, pkg: spdxPackage{Name: "a", Declared: "Apache-2.0 WITH LLVM-exception"}, want: "not admitted: Apache-2.0 WITH LLVM-exception (G2)"},
		{name: "source-available WITH", pol: npm, pkg: spdxPackage{Name: "a", Declared: "Apache-2.0 WITH Commons-Clause"}, want: "not admitted: Apache-2.0 WITH Commons-Clause (G2)"},
		{name: "later version", pol: cargo, pkg: spdxPackage{Name: "a", Declared: "Apache-2.0+"}, expr: "Apache-2.0+"},
		{name: "case-insensitive identifiers", pol: npm, pkg: spdxPackage{Name: "a", Declared: "mit OR apache-2.0"}, expr: "mit OR apache-2.0"},
		{
			name: "lower-case operator", pol: npm, pkg: spdxPackage{Name: "a", Declared: "MIT or Apache-2.0"},
			want: `"or" is not an operator: SPDX operators are AND, OR and WITH in upper case; add a reviewed override (G2)`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, finding := checkPackage(tt.pol, tt.pkg)
			if tt.want != "" {
				if !strings.Contains(finding, tt.want) {
					t.Errorf("finding = %q, want one containing %q", finding, tt.want)
				}
				return
			}
			if finding != "" {
				t.Fatalf("finding = %q, want a pass", finding)
			}
			if m.Expression != tt.expr || m.Election != tt.election || m.Override != tt.override || m.MPL != tt.mpl {
				t.Errorf("notice = expression %q, election %q, override %q, MPL %v; want %q, %q, %q, %v",
					m.Expression, m.Election, m.Override, m.MPL, tt.expr, tt.election, tt.override, tt.mpl)
			}
		})
	}
}

// TestPackageTexts reads a package root's legal texts and the license
// files its manifest names besides them, once each and sorted.
func TestPackageTexts(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{"LICENSE-MIT": "Permission is hereby granted, free of charge\r\n", "NOTICE": "Example\n", "COPYRIGHT.txt": "\ufeffCopyright 2026 Example\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "legal"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legal", "TERMS"), []byte("Terms\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := spdxPackage{Name: "x", Version: "1.0.0", Dir: dir, LicenseFiles: []string{"COPYRIGHT.txt", "./LICENSE-MIT", "legal/TERMS"}}
	got, err := packageTexts(readLegal, os.ReadFile, p)
	if err != nil {
		t.Fatal(err)
	}
	want := []legalText{
		{Rel: "COPYRIGHT.txt", Kind: kindLicense, Text: "Copyright 2026 Example\n"},
		{Rel: "LICENSE-MIT", Kind: kindLicense, Text: "Permission is hereby granted, free of charge\n"},
		{Rel: "NOTICE", Kind: kindNotice, Text: "Example\n"},
		{Rel: "legal/TERMS", Kind: kindLicense, Text: "Terms\n"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("packageTexts = %+v\nwant %+v", got, want)
	}
	p.LicenseFiles = []string{"MISSING.txt"}
	if _, err := packageTexts(readLegal, os.ReadFile, p); err == nil || !strings.Contains(err.Error(), "the license file of x 1.0.0") {
		t.Errorf("missing license file: err = %v", err)
	}
	p.Dir = filepath.Join(dir, "absent")
	if _, err := packageTexts(readLegal, os.ReadFile, p); err == nil || !strings.Contains(err.Error(), "the legal texts of x 1.0.0") {
		t.Errorf("missing directory: err = %v", err)
	}
}

// TestOpenInput reads a named file or standard input for "-".
func TestOpenInput(t *testing.T) {
	file := filepath.Join(t.TempDir(), "in.json")
	if err := os.WriteFile(file, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{file: "file", "-": "stdin"} {
		r, err := openInput(name, strings.NewReader("stdin"))
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || string(data) != want {
			t.Errorf("openInput(%q) read %q, %v, want %q", name, data, err, want)
		}
	}
	if _, err := openInput(filepath.Join(t.TempDir(), "absent.json"), nil); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("absent file: err = %v", err)
	}
	if _, err := openInput("-", nil); err == nil {
		t.Error("no standard input accepted")
	}
}

// TestDecodeOne refuses an input holding more than one JSON value.
func TestDecodeOne(t *testing.T) {
	var v map[string]any
	if err := decodeOne(strings.NewReader("{\"a\": 1}\n"), &v); err != nil || v["a"] != float64(1) {
		t.Errorf("one value: %v, %v", v, err)
	}
	for _, in := range []string{`{"a": 1} {"b": 2}`, `{"a": 1} x`, `{`, ``} {
		if err := decodeOne(strings.NewReader(in), &v); err == nil {
			t.Errorf("decodeOne(%q) accepted", in)
		}
	}
}
