// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// decodePnpmFixture decodes a recorded pnpm licenses fixture.
func decodePnpmFixture(t *testing.T, name string) []pnpmPackage {
	t.Helper()
	pkgs, err := decodePnpm(strings.NewReader(fixture(t, name)))
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

// TestDecodePnpm reads the object of licenses pnpm writes, every package
// of every license, and refuses anything else.
func TestDecodePnpm(t *testing.T) {
	pkgs := decodePnpmFixture(t, "pnpm-console.json")
	var got []string
	for _, p := range pkgs {
		got = append(got, p.Name+" "+strings.Join(p.Versions, ","))
	}
	want := []string{"long 5.3.2", "widget file:../vendor/widget", "source-map-js 1.2.1", "semver 6.3.1,7.7.3", "@emotion/hash 0.9.2", "react 19.2.0", "react-dom 19.2.0", "scheduler 0.27.0"}
	if !slices.Equal(got, want) {
		t.Errorf("packages = %q\nwant %q", got, want)
	}
	if pkgs, err := decodePnpm(strings.NewReader("{}\n")); err != nil || len(pkgs) != 0 {
		t.Errorf("empty output: %v, %v", pkgs, err)
	}
	for in, want := range map[string]string{
		`null`:                               "not a JSON object of licenses",
		`[]`:                                 "reading the pnpm licenses output",
		`{"MIT": [{"versions": ["1.0.0"]}]}`: `lists a package without a name under "MIT"`,
		`{"MIT": []} {}`:                     "more than one JSON value",
		fixture(t, "cargo-pdk.json"):         "reading the pnpm licenses output",
	} {
		if _, err := decodePnpm(strings.NewReader(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("decodePnpm(%.50q) = %v, want an error containing %q", in, err, want)
		}
	}
}

// TestDeclaredLicense reads a package.json as pnpm does: "license", else
// the legacy "licenses"; a string, an object's "type" or "name", or an
// array of them as alternatives; "SEE LICENSE IN" declares no expression.
func TestDeclaredLicense(t *testing.T) {
	tests := []struct {
		manifest      string
		expr, missing string
	}{
		{`{"license": "MIT"}`, "MIT", ""},
		{`{"license": "(MIT OR Apache-2.0)"}`, "(MIT OR Apache-2.0)", ""},
		{`{"license": {"type": "ISC", "url": "https://example.com"}}`, "ISC", ""},
		{`{"license": {"name": "BSD-3-Clause"}}`, "BSD-3-Clause", ""},
		{`{"license": ["MIT", {"type": "Apache-2.0"}]}`, "(MIT OR Apache-2.0)", ""},
		{`{"license": [{"type": "MIT"}]}`, "MIT", ""},
		{`{"licenses": [{"type": "MIT"}, {"type": "Apache-2.0"}]}`, "(MIT OR Apache-2.0)", ""},
		{`{"license": "", "licenses": [{"type": "ISC"}]}`, "ISC", ""},
		{`{"license": "MIT", "licenses": [{"type": "GPL-2.0-only"}]}`, "MIT", ""},
		{`{"license": "UNLICENSED"}`, "UNLICENSED", ""},
		{`{"license": "SEE LICENSE IN LICENSE.md"}`, "", `declares "SEE LICENSE IN LICENSE.md", no license expression`},
		{`{"license": "see license in COPYING"}`, "", `declares "see license in COPYING", no license expression`},
		{`{}`, "", "declares no license expression"},
		{`{"license": null}`, "", "declares no license expression"},
		{`{"license": []}`, "", "declares no license expression"},
		{`{"license": {"url": "https://example.com"}}`, "", "declares no license expression"},
		{`{"license": 7}`, "", "declares no license expression"},
	}
	for _, tt := range tests {
		var m npmManifest
		if err := json.Unmarshal([]byte(tt.manifest), &m); err != nil {
			t.Fatal(err)
		}
		if expr, missing := declaredLicense(m); expr != tt.expr || missing != tt.missing {
			t.Errorf("declaredLicense(%s) = %q, %q; want %q, %q", tt.manifest, expr, missing, tt.expr, tt.missing)
		}
	}
}

// TestNpmSource: the npm registry's tarball, scoped packages included; the
// named registry; and the specifier of a package no registry serves.
func TestNpmSource(t *testing.T) {
	tests := []struct {
		name, version, registry string
		reported, specifiers    []string
		want                    string
	}{
		{"react", "19.2.0", "", []string{"19.2.0"}, nil, "https://registry.npmjs.org/react/-/react-19.2.0.tgz"},
		{"@emotion/hash", "0.9.2", "", []string{"0.9.2"}, nil, "https://registry.npmjs.org/@emotion/hash/-/hash-0.9.2.tgz"},
		{"x", "1.0.0", "corp", []string{"1.0.0"}, nil, "the pnpm named registry corp"},
		{"widget", "1.4.0", "", []string{"file:../vendor/widget"}, []string{"file:../vendor/widget"}, "no registry; pnpm reports it as file:../vendor/widget"},
		{"y", "1.0.0", "", nil, nil, "no registry"},
	}
	for _, tt := range tests {
		if got := npmSource(tt.name, tt.version, tt.registry, tt.reported, tt.specifiers); got != tt.want {
			t.Errorf("npmSource(%s) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// consolePrefix prefixes the findings on Ruralz Console's packages.
const consolePrefix = consoleName + ": "

// TestAnalyzeConsole applies G2 to recorded Console trees under the npm
// policy. The example Console passes, both semver versions and its
// vendored package included. In the edge tree the declared expressions
// decide, never pnpm's report: guess-pkg declares no license though pnpm
// reports MIT from its license file's text, and see-pkg defers to a file;
// legacy license objects and arrays resolve; "MIT/Apache-2.0" is no SPDX
// expression in npm; UNLICENSED, copyleft, MPL-2.0 without an exception,
// a Commons-Clause WITH and 0BSD, which no npm addendum names, fail; the
// GPL devDependency is not reported, so it fails nothing.
func TestAnalyzeConsole(t *testing.T) {
	npm := realPolicy(t, ecosystemNPM)
	tests := []struct {
		name, fixture string
		pol           spdxPolicy
		want          []string
	}{
		{"example Console", "pnpm-console.json", npm, nil},
		{"edge tree", "pnpm-edge.json", npm, []string{
			consolePrefix + "commons-pkg 4.2.0: license expression Apache-2.0 WITH Commons-Clause does not resolve to the G2 list or the npm addendum; not admitted: Apache-2.0 WITH Commons-Clause (G2)",
			consolePrefix + "gpl-pkg 1.0.0: license expression GPL-3.0-only does not resolve to the G2 list or the npm addendum; not admitted: GPL-3.0-only (G2)",
			consolePrefix + "guess-pkg 2.1.0: declares no license expression; add a reviewed override (G2)",
			consolePrefix + "mpl-pkg 2.5.0: license expression MPL-2.0 does not resolve to the G2 list or the npm addendum; not admitted: MPL-2.0 (G2)",
			consolePrefix + "none-pkg 0.0.1: declares no license expression; add a reviewed override (G2)",
			consolePrefix + `see-pkg 1.3.0: declares "SEE LICENSE IN LICENSE.md", no license expression; add a reviewed override (G2)`,
			consolePrefix + `slash-pkg 1.0.0: license expression "MIT/Apache-2.0" is not a valid SPDX expression: character '/' is not part of an SPDX expression; add a reviewed override (G2)`,
			consolePrefix + "tslib 2.8.1: license expression 0BSD does not resolve to the G2 list or the npm addendum; not admitted: 0BSD (G2)",
			consolePrefix + "unl-pkg 1.0.0: license expression UNLICENSED does not resolve to the G2 list or the npm addendum; not admitted: UNLICENSED (G2)",
		}},
		{
			"edge tree with an addendum, an exception and overrides", "pnpm-edge.json",
			testPolicy(t, ecosystemNPM, []string{"0BSD"}, []string{"npm:mpl-pkg"}, nil,
				map[string]string{"npm:guess-pkg": "MIT", "npm:see-pkg": "LicenseRef-See", "npm:slash-pkg": "MIT OR Apache-2.0"}),
			[]string{
				consolePrefix + "commons-pkg 4.2.0: license expression Apache-2.0 WITH Commons-Clause does not resolve to the G2 list or the npm addendum; not admitted: Apache-2.0 WITH Commons-Clause (G2)",
				consolePrefix + "gpl-pkg 1.0.0: license expression GPL-3.0-only does not resolve to the G2 list or the npm addendum; not admitted: GPL-3.0-only (G2)",
				consolePrefix + "none-pkg 0.0.1: declares no license expression; add a reviewed override (G2)",
				consolePrefix + "see-pkg 1.3.0: license expression LicenseRef-See does not resolve to the G2 list or the npm addendum; not admitted: LicenseRef-See (G2)",
				consolePrefix + "unl-pkg 1.0.0: license expression UNLICENSED does not resolve to the G2 list or the npm addendum; not admitted: UNLICENSED (G2)",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := analyzeConsole(decodePnpmFixture(t, tt.fixture), tt.pol, readLegal, os.ReadFile)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.findings, tt.want) {
				t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(res.findings, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

// TestAnalyzeConsolePackages: one notice per package version, from its
// own manifest, with its source and legal texts; the vendored package's
// version is its manifest's, not pnpm's file: specifier.
func TestAnalyzeConsolePackages(t *testing.T) {
	res, err := analyzeConsole(decodePnpmFixture(t, "pnpm-console.json"), realPolicy(t, ecosystemNPM), readLegal, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range res.packages {
		got = append(got, p.Path+" "+p.Version+": "+p.Expression+" from "+p.Source)
	}
	want := []string{
		"@emotion/hash 0.9.2: MIT from https://registry.npmjs.org/@emotion/hash/-/hash-0.9.2.tgz",
		"long 5.3.2: Apache-2.0 from https://registry.npmjs.org/long/-/long-5.3.2.tgz",
		"react 19.2.0: MIT from https://registry.npmjs.org/react/-/react-19.2.0.tgz",
		"react-dom 19.2.0: MIT from https://registry.npmjs.org/react-dom/-/react-dom-19.2.0.tgz",
		"scheduler 0.27.0: MIT from https://registry.npmjs.org/scheduler/-/scheduler-0.27.0.tgz",
		"semver 6.3.1: ISC from https://registry.npmjs.org/semver/-/semver-6.3.1.tgz",
		"semver 7.7.3: ISC from https://registry.npmjs.org/semver/-/semver-7.7.3.tgz",
		"source-map-js 1.2.1: BSD-3-Clause from https://registry.npmjs.org/source-map-js/-/source-map-js-1.2.1.tgz",
		"widget 1.4.0: Apache-2.0 from no registry; pnpm reports it as file:../vendor/widget",
	}
	if !slices.Equal(got, want) {
		t.Errorf("packages:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, p := range res.packages {
		if p.Path == "widget" && (len(p.Texts) != 2 || p.Texts[1].Kind != kindNotice) {
			t.Errorf("widget texts = %+v, want its LICENSE and NOTICE", p.Texts)
		}
	}
}

// fakeTree serves package directories from memory: each directory's
// package.json and legal texts; "broken" fails to read.
type fakeTree map[string]map[string]string

func (f fakeTree) readFile(name string) ([]byte, error) {
	dir, file := filepath.Split(filepath.ToSlash(name))
	if strings.Contains(dir, "broken") {
		return nil, errors.New("read error")
	}
	files, ok := f[strings.TrimSuffix(dir, "/")]
	if !ok {
		return nil, os.ErrNotExist
	}
	text, ok := files[file]
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(text), nil
}

func (f fakeTree) read(dir string) ([]legalText, error) {
	var out []legalText
	for name, text := range f[filepath.ToSlash(dir)] {
		if kind := legalKind(name); kind != "" {
			out = append(out, legalText{Rel: name, Kind: kind, Text: text})
		}
	}
	slices.SortFunc(out, func(a, b legalText) int { return strings.Compare(a.Rel, b.Rel) })
	return out, nil
}

// TestAnalyzeConsoleInstances: peer-dependency variants of one version are
// one package, read from its first directory; a package pnpm found no
// directory or manifest for, or whose manifest has no version, is a
// finding; an unreadable or invalid manifest is an error. Packages of a
// named registry are their own packages.
func TestAnalyzeConsoleInstances(t *testing.T) {
	str := func(s string) *string { return &s }
	tree := fakeTree{
		"m/.pnpm/a@1.0.0_x@1/node_modules/a": {"package.json": `{"name": "a", "version": "1.0.0", "license": "MIT"}`, "LICENSE": "first\n"},
		"m/.pnpm/a@1.0.0_x@2/node_modules/a": {"package.json": `{"name": "a", "version": "1.0.0", "license": "MIT"}`, "LICENSE": "second\n"},
		"m/.pnpm/b@2.0.0/node_modules/b":     {"package.json": `{"name": "b", "license": "MIT"}`},
		"m/.pnpm/c@1.0.0/node_modules/c":     {"package.json": `{"name": "c", "version": "1.0.0", "license": "ISC"}`},
		"m/.pnpm/bad@1.0.0/node_modules/bad": {"package.json": `{`},
	}
	pol := realPolicy(t, ecosystemNPM)
	entries := []pnpmPackage{
		{Name: "a", Versions: []string{"1.0.0"}, Paths: []*string{str("m/.pnpm/a@1.0.0_x@2/node_modules/a"), str("m/.pnpm/a@1.0.0_x@1/node_modules/a")}},
		{Name: "b", Versions: []string{"2.0.0"}, Paths: []*string{str("m/.pnpm/b@2.0.0/node_modules/b")}},
		{Name: "c", Versions: []string{"1.0.0"}, Paths: []*string{str("m/.pnpm/c@1.0.0/node_modules/c")}},
		{Name: "c", Versions: []string{"1.0.0"}, Paths: []*string{str("m/.pnpm/c@1.0.0/node_modules/c")}, RegistryName: "corp"},
		{Name: "d", Versions: []string{"3.0.0"}, Paths: []*string{nil}},
		{Name: "e", Versions: []string{"4.0.0"}, Paths: []*string{str("m/.pnpm/e@4.0.0/node_modules/e")}},
	}
	res, err := analyzeConsole(entries, pol, tree.read, tree.readFile)
	if err != nil {
		t.Fatal(err)
	}
	wantFindings := []string{
		consolePrefix + "b 2.0.0: the package.json at m/.pnpm/b@2.0.0/node_modules/b declares no version",
		consolePrefix + "d 3.0.0: pnpm reports no directory for it; install with pnpm install --frozen-lockfile",
		consolePrefix + "e 4.0.0: no package.json at m/.pnpm/e@4.0.0/node_modules/e; install with pnpm install --frozen-lockfile",
	}
	if !slices.Equal(res.findings, wantFindings) {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(res.findings, "\n"), strings.Join(wantFindings, "\n"))
	}
	var got []string
	for _, p := range res.packages {
		var texts []string
		for _, x := range p.Texts {
			texts = append(texts, strings.TrimSpace(x.Text))
		}
		got = append(got, p.Path+" "+p.Version+" "+p.Source+" "+strings.Join(texts, ","))
	}
	want := []string{
		"a 1.0.0 https://registry.npmjs.org/a/-/a-1.0.0.tgz first",
		"c 1.0.0 https://registry.npmjs.org/c/-/c-1.0.0.tgz ",
		"c 1.0.0 the pnpm named registry corp ",
	}
	if !slices.Equal(got, want) {
		t.Errorf("packages:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, e := range []pnpmPackage{
		{Name: "bad", Versions: []string{"1.0.0"}, Paths: []*string{str("m/.pnpm/bad@1.0.0/node_modules/bad")}},
		{Name: "broken", Versions: []string{"1.0.0"}, Paths: []*string{str("m/broken")}},
	} {
		if _, err := analyzeConsole([]pnpmPackage{e}, pol, tree.read, tree.readFile); err == nil {
			t.Errorf("%s: no error", e.Name)
		}
	}
}

// consoleGoModules are the Go modules of the Console notices golden.
func consoleGoModules() []moduleNotice {
	return []moduleNotice{{Path: "github.com/BurntSushi/toml", Version: "v1.4.0", IDs: []string{"MIT"}, Texts: lic(mitText)}}
}

// TestConsoleNoticesGolden: ruralz-control's THIRD_PARTY_LICENSES lists
// the npm packages of the Ruralz Console it embeds after its Go modules,
// and its NOTICE carries their NOTICE texts.
func TestConsoleNoticesGolden(t *testing.T) {
	res, err := analyzeConsole(decodePnpmFixture(t, "pnpm-console.json"), realPolicy(t, ecosystemNPM), readLegal, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	tg := target{"ruralz-control", "linux", "amd64"}
	golden(t, "console.THIRD_PARTY_LICENSES", renderThirdParty(tg, testStd(), consoleGoModules(), res.packages))
	golden(t, "console.NOTICE", renderNotice(tg, repoNoticeText, consoleGoModules(), res.packages))
	if plain := renderThirdParty(tg, testStd(), consoleGoModules(), nil); strings.Contains(plain, "Ruralz Console") {
		t.Errorf("no Console packages, yet the file mentions Ruralz Console:\n%s", plain)
	}
}

// emptyList is a go list that finds no package for any target.
func emptyList(context.Context, string, target) ([]goPackage, error) { return nil, nil }

// testStdLib returns testStd for run's std hook.
func testStdLib(context.Context, string) (standardLibrary, error) { return testStd(), nil }

// TestRunConsole runs the Go gates with -pnpm: the Console's packages are
// gated, and with -notices only ruralz-control's notices list them, on
// every platform; a Console finding fails the run with the Go gates.
func TestRunConsole(t *testing.T) {
	root := noticeRoot(t)
	dir := filepath.Join(t.TempDir(), "notices")
	in := filepath.Join(t.TempDir(), "licenses.json")
	if err := os.WriteFile(in, []byte(fixture(t, "pnpm-console.json")), 0o600); err != nil {
		t.Fatal(err)
	}
	o := options{dir: root, notices: dir, pnpm: in, list: emptyList, std: testStdLib}
	findings, summary, err := run(t.Context(), o)
	if err != nil || len(findings) > 0 {
		t.Fatalf("run = %q, %v", findings, err)
	}
	wantSummary := []string{goGatesLine, "depgate: G2 passes over the 9 npm packages Ruralz Console ships", "depgate: wrote THIRD_PARTY_LICENSES and NOTICE of 13 artifacts under " + dir}
	if !slices.Equal(summary, wantSummary) {
		t.Errorf("summary = %q, want %q", summary, wantSummary)
	}
	for _, tg := range targets() {
		tp, err := os.ReadFile(filepath.Join(dir, tg.name(), thirdPartyName)) //nolint:gosec // The test reads its own output.
		if err != nil {
			t.Fatal(err)
		}
		n, err := os.ReadFile(filepath.Join(dir, tg.name(), noticeName)) //nolint:gosec // The test reads its own output.
		if err != nil {
			t.Fatal(err)
		}
		lists := bytes.Contains(tp, []byte("\n"+sectionRule+"\nreact 19.2.0\nLicense: MIT\n"))
		notices := bytes.Contains(n, []byte("Widget\nCopyright 2026 Example Widget Authors\n"))
		if lists != embedsConsole(tg) || notices != embedsConsole(tg) {
			t.Errorf("%s: lists the Console packages %v, their notices %v; want %v", tg.name(), lists, notices, embedsConsole(tg))
		}
	}

	edge := filepath.Join(t.TempDir(), "edge.json")
	if err := os.WriteFile(edge, []byte(fixture(t, "pnpm-edge.json")), 0o600); err != nil {
		t.Fatal(err)
	}
	o = options{dir: root, notices: filepath.Join(t.TempDir(), "n"), pnpm: edge, list: emptyList, std: testStdLib}
	findings, summary, err = run(t.Context(), o)
	if err != nil || len(findings) != 9 || summary != nil || !strings.HasPrefix(findings[0], consolePrefix+"commons-pkg 4.2.0:") {
		t.Errorf("edge tree: findings %q, summary %q, err %v", findings, summary, err)
	}
	if _, err := os.Stat(o.notices); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failing run wrote notices: %v", err)
	}
}

// TestRunMainConsole reads -pnpm from standard input and refuses output
// that is not pnpm's, before any go list runs.
func TestRunMainConsole(t *testing.T) {
	var stderr bytes.Buffer
	if code := runMain(t.Context(), []string{"-pnpm", "-"}, strings.NewReader(`["not", "pnpm"]`), &bytes.Buffer{}, &stderr); code != 2 || !strings.HasPrefix(stderr.String(), "depgate: reading the pnpm licenses output") {
		t.Errorf("runMain = %d, %q", code, stderr.String())
	}
	o := options{dir: noticeRoot(t), pnpm: "-", stdin: strings.NewReader(fixture(t, "pnpm-console.json")), list: emptyList, std: testStdLib}
	if findings, summary, err := run(t.Context(), o); err != nil || findings != nil || len(summary) != 2 {
		t.Errorf("run from standard input = %q, %q, %v", findings, summary, err)
	}
}
