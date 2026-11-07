// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// decodeFixture decodes a recorded cargo metadata fixture.
func decodeFixture(t *testing.T, name string) cargoMetadata {
	t.Helper()
	md, err := decodeCargo(strings.NewReader(fixture(t, name)))
	if err != nil {
		t.Fatal(err)
	}
	return md
}

// names lists packages as "<name> <version>".
func names(ps []cargoPackage) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name + " " + p.Version
	}
	return out
}

// TestDecodeCargo refuses output depgate cannot read: another format
// version, no resolve graph (--no-deps), no workspace member, invalid or
// several JSON values, and pnpm output given to -cargo.
func TestDecodeCargo(t *testing.T) {
	if md := decodeFixture(t, "cargo-pdk.json"); len(md.Packages) != 17 || len(md.Resolve.Nodes) != 17 || md.Resolve.Root == nil {
		t.Errorf("cargo-pdk.json: %d packages, %d nodes, root %v", len(md.Packages), len(md.Resolve.Nodes), md.Resolve.Root)
	}
	tests := map[string]string{
		`{"version":2,"packages":[],"workspace_members":["a"],"resolve":{"nodes":[]}}`: "format version 2; depgate reads --format-version 1",
		`{"version":1,"packages":[],"workspace_members":["a"],"resolve":null}`:         "no resolve graph: run cargo metadata without --no-deps",
		`{"version":1,"packages":[],"workspace_members":[],"resolve":{"nodes":[]}}`:    "names no workspace member",
		`{"version":1`:                  "reading the cargo metadata output: unexpected EOF",
		`{"version":1} {"version":1}`:   "more than one JSON value",
		fixture(t, "pnpm-console.json"): "format version 0",
	}
	for in, want := range tests {
		if _, err := decodeCargo(strings.NewReader(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("decodeCargo(%.60q) = %v, want an error containing %q", in, err, want)
		}
	}
}

// TestCargoShipped resolves what a PDK ships from recorded output: the
// normal dependencies of the package cargo metadata ran for, or of every
// member of a virtual workspace, through normal dependencies; never a
// proc-macro crate or what only proc-macro crates reach (serde_derive,
// syn, quote, proc-macro2), a build-dependency (version_check, autocfg)
// or a dev-dependency (static_assertions), nor a workspace member. A crate
// that is both a build-dependency and a normal one ships, as do both
// versions of a crate. A platform's dependencies are those cargo resolved:
// --filter-platform drops cfg(windows) ones, without it they ship.
func TestCargoShipped(t *testing.T) {
	tests := []struct {
		fixture        string
		roots, shipped []string
	}{
		{"cargo-pdk.json", []string{"example-pdk 0.1.0"}, []string{
			"cfg-if 1.0.0", "memchr 2.8.3", "num-traits 0.2.19", "serde 1.0.229", "serde_core 1.0.229", "unicode-ident 1.0.27", "vendored-codec 0.3.1",
		}},
		{"cargo-edge.json", []string{"edge-plugin-api 0.1.0", "edge-plugin-util 0.1.0"}, []string{
			"and-crate 3.2.0", "badexpr-crate 1.1.0", "elect-crate 0.8.0", "git-crate 0.7.0", "gpl-crate 1.0.0", "licensefile-crate 0.4.2",
			"lower-crate 0.5.0", "lowerop-crate 0.6.0", "mpl-crate 2.0.1", "nolicense-crate 0.2.0", "or-crate 1.4.0", "plus-crate 1.0.3",
			"ref-crate 0.1.0", "shared-crate 1.2.0", "twin-crate 1.0.0", "twin-crate 2.0.0", "unicode-crate 15.1.0", "with-crate 0.9.0",
		}},
		{"cargo-platform-filtered.json", []string{"platform-pdk 0.2.0"}, []string{"shared-crate 1.2.0"}},
		{"cargo-platform-unfiltered.json", []string{"platform-pdk 0.2.0"}, []string{"shared-crate 1.2.0", "win-only-crate 1.0.0"}},
	}
	for _, tt := range tests {
		a, err := cargoShipped(decodeFixture(t, tt.fixture))
		if err != nil {
			t.Fatalf("%s: %v", tt.fixture, err)
		}
		if got := names(a.roots); !slices.Equal(got, tt.roots) {
			t.Errorf("%s: roots = %q, want %q", tt.fixture, got, tt.roots)
		}
		if got := names(a.shipped); !slices.Equal(got, tt.shipped) {
			t.Errorf("%s: shipped = %q\nwant %q", tt.fixture, got, tt.shipped)
		}
	}
}

// TestCargoShippedProcMacroMember: in a virtual workspace every member is
// a root, and a proc-macro member ships nothing, so what only it reaches
// stays out while the other member's dependencies ship.
func TestCargoShippedProcMacroMember(t *testing.T) {
	lib, derive, dep, gpl := "path+file:///w/lib#0.1.0", "path+file:///w/derive#0.1.0", "registry+x#dep@1.0.0", "registry+x#gpl-support@1.0.0"
	normal := []cargoDepKind{{Kind: nil}}
	md := cargoMetadata{
		Version: 1, WorkspaceMembers: []string{lib, derive},
		Packages: []cargoPackage{
			{Name: "lib", Version: "0.1.0", ID: lib, Targets: []cargoTarget{{Kind: []string{"lib"}}}},
			{Name: "derive", Version: "0.1.0", ID: derive, Targets: []cargoTarget{{Kind: []string{"proc-macro"}, CrateTypes: []string{"proc-macro"}}}},
			{Name: "dep", Version: "1.0.0", ID: dep},
			{Name: "gpl-support", Version: "1.0.0", ID: gpl},
		},
		Resolve: &cargoResolve{Nodes: []cargoNode{
			{ID: lib, Deps: []cargoDep{{Pkg: dep, DepKinds: normal}}},
			{ID: derive, Deps: []cargoDep{{Pkg: gpl, DepKinds: normal}}},
			{ID: dep},
			{ID: gpl},
		}},
	}
	a, err := cargoShipped(md)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(a.roots); !slices.Equal(got, []string{"derive 0.1.0", "lib 0.1.0"}) {
		t.Errorf("roots = %q", got)
	}
	if got := names(a.shipped); !slices.Equal(got, []string{"dep 1.0.0"}) {
		t.Errorf("shipped = %q, want only the library member's dependency", got)
	}
}

// TestCargoShippedGraphErrors: a dependency or root the output does not
// describe is an error, never a silent gap in the gate.
func TestCargoShippedGraphErrors(t *testing.T) {
	root := "path+file:///w#a@0.1.0"
	tests := map[string]cargoMetadata{
		"lists no package path+file:///w#b@1.0.0, a dependency of": {
			Version: 1, WorkspaceMembers: []string{root},
			Packages: []cargoPackage{{Name: "a", Version: "0.1.0", ID: root}},
			Resolve:  &cargoResolve{Nodes: []cargoNode{{ID: root, Deps: []cargoDep{{Pkg: "path+file:///w#b@1.0.0"}}}}, Root: &root},
		},
		"has no node for": {
			Version: 1, WorkspaceMembers: []string{root},
			Packages: []cargoPackage{{Name: "a", Version: "0.1.0", ID: root}},
			Resolve:  &cargoResolve{Root: &root},
		},
		"lists no package path+file:///w#a@0.1.0": {
			Version: 1, WorkspaceMembers: []string{root},
			Resolve: &cargoResolve{Nodes: []cargoNode{{ID: root}}, Root: &root},
		},
	}
	for want, md := range tests {
		if _, err := cargoShipped(md); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want one containing %q", err, want)
		}
	}
}

// TestShipsWith: only normal edges ship; an edge without kinds counts as
// one, so an unexpected output never shrinks the gated set.
func TestShipsWith(t *testing.T) {
	dev, build, other := "dev", "build", "artifact"
	tests := []struct {
		kinds []cargoDepKind
		want  bool
	}{
		{[]cargoDepKind{{Kind: nil}}, true},
		{[]cargoDepKind{{Kind: &dev}}, false},
		{[]cargoDepKind{{Kind: &build}}, false},
		{[]cargoDepKind{{Kind: &dev}, {Kind: &build}}, false},
		{[]cargoDepKind{{Kind: &build}, {Kind: nil}}, true},
		{[]cargoDepKind{{Kind: &other}}, true},
		{nil, true},
	}
	for _, tt := range tests {
		if got := shipsWith(cargoDep{DepKinds: tt.kinds}); got != tt.want {
			t.Errorf("shipsWith(%v) = %v, want %v", tt.kinds, got, tt.want)
		}
	}
}

// TestCrateSource names where each kind of source keeps a crate: the
// crates.io download, the Git commit, another registry, or a path relative
// to the workspace root.
func TestCrateSource(t *testing.T) {
	str := func(s string) *string { return &s }
	root := filepath.Join(t.TempDir(), "ws")
	tests := []struct {
		pkg  cargoPackage
		want string
	}{
		{cargoPackage{Name: "serde", Version: "1.0.229", Source: str("registry+https://github.com/rust-lang/crates.io-index")}, "https://static.crates.io/crates/serde/1.0.229/download"},
		{cargoPackage{Name: "serde", Version: "1.0.229", Source: str("sparse+https://index.crates.io/")}, "https://static.crates.io/crates/serde/1.0.229/download"},
		{cargoPackage{Name: "g", Version: "0.7.0", Source: str("git+https://github.com/example/g?branch=main#00e45b2e4da1b509566f27b78bdda00973c464a1")}, "https://github.com/example/g at commit 00e45b2e4da1b509566f27b78bdda00973c464a1"},
		{cargoPackage{Name: "g", Version: "0.7.0", Source: str("git+https://github.com/example/g")}, "https://github.com/example/g"},
		{cargoPackage{Name: "r", Version: "1.0.0", Source: str("sparse+https://registry.example.com/index/")}, "the registry whose index is https://registry.example.com/index/"},
		{cargoPackage{Name: "p", Version: "1.0.0", ManifestPath: filepath.Join(root, "..", "vendor", "p", "Cargo.toml")}, "a local path (../vendor/p), not published"},
		{cargoPackage{Name: "x", Version: "1.0.0", Source: str("local-registry+file:///r")}, "local-registry+file:///r"},
	}
	for _, tt := range tests {
		if got := crateSource(tt.pkg, root); got != tt.want {
			t.Errorf("crateSource(%s) = %q, want %q", tt.pkg.Name, got, tt.want)
		}
	}
}

// edgeTitle prefixes the findings on the virtual workspace of
// cargo-edge.json.
const edgeTitle = "edge-plugin-api 0.1.0, edge-plugin-util 0.1.0: "

// TestAnalyzeCargo applies G2 to recorded PDKs. Under the real Cargo
// policy the example PDK passes, its legacy "MIT/Apache-2.0" crates, its
// "Unlicense OR MIT" crate and unicode-ident's Unicode-3.0 included; the
// edge workspace fails on exactly the crates it ships whose expressions do
// not resolve, while its GPL and AGPL build-dependency, dev-dependency and
// proc-macro crates fail nothing. An election, an MPL-2.0 exception, a
// WITH addendum entry and overrides clear their findings.
func TestAnalyzeCargo(t *testing.T) {
	cargo := realPolicy(t, ecosystemCargo)
	injected := testPolicy(t, ecosystemCargo, []string{"Unicode-3.0", "Apache-2.0 WITH LLVM-exception"},
		[]string{"cargo:mpl-crate"},
		map[string]string{"cargo:elect-crate": "Zlib"},
		map[string]string{"cargo:nolicense-crate": "MIT", "cargo:licensefile-crate": "BSD-3-Clause", "cargo:badexpr-crate": "Apache-2.0", "cargo:lowerop-crate": "MIT OR Apache-2.0"})
	tests := []struct {
		name, fixture string
		pol           spdxPolicy
		want          []string
	}{
		{"example PDK", "cargo-pdk.json", cargo, nil},
		{"example PDK without the Cargo addendum", "cargo-pdk.json", testPolicy(t, ecosystemCargo, nil, nil, nil, nil), []string{
			"example-pdk 0.1.0: unicode-ident 1.0.27: license expression (MIT OR Apache-2.0) AND Unicode-3.0 does not resolve to the G2 list or the cargo addendum; not admitted: Unicode-3.0 (G2)",
		}},
		{"edge workspace", "cargo-edge.json", cargo, []string{
			edgeTitle + "and-crate 3.2.0: license expression MIT AND GPL-2.0-only does not resolve to the G2 list or the cargo addendum; not admitted: GPL-2.0-only (G2)",
			edgeTitle + `badexpr-crate 1.1.0: license expression "Apache 2.0" is not a valid SPDX expression: "2.0" follows a license without an operator between them; add a reviewed override (G2)`,
			edgeTitle + "elect-crate 0.8.0: license expression Zlib OR GPL-3.0-only does not resolve to the G2 list or the cargo addendum; not admitted: Zlib, GPL-3.0-only (G2)",
			edgeTitle + "gpl-crate 1.0.0: license expression GPL-3.0-only does not resolve to the G2 list or the cargo addendum; not admitted: GPL-3.0-only (G2)",
			edgeTitle + "licensefile-crate 0.4.2: declares only the license file COPYRIGHT.txt and no license expression; add a reviewed override (G2)",
			edgeTitle + `lowerop-crate 0.6.0: license expression "MIT or Apache-2.0" is not a valid SPDX expression: "or" is not an operator: SPDX operators are AND, OR and WITH in upper case; add a reviewed override (G2)`,
			edgeTitle + "mpl-crate 2.0.1: license expression MPL-2.0 does not resolve to the G2 list or the cargo addendum; not admitted: MPL-2.0 (G2)",
			edgeTitle + "nolicense-crate 0.2.0: declares no license expression; add a reviewed override (G2)",
			edgeTitle + "ref-crate 0.1.0: license expression LicenseRef-Proprietary does not resolve to the G2 list or the cargo addendum; not admitted: LicenseRef-Proprietary (G2)",
			edgeTitle + "with-crate 0.9.0: license expression Apache-2.0 WITH LLVM-exception does not resolve to the G2 list or the cargo addendum; not admitted: Apache-2.0 WITH LLVM-exception (G2)",
		}},
		{"edge workspace with elections, exceptions and overrides", "cargo-edge.json", injected, []string{
			edgeTitle + "and-crate 3.2.0: license expression MIT AND GPL-2.0-only does not resolve to the G2 list or the cargo addendum; not admitted: GPL-2.0-only (G2)",
			edgeTitle + "gpl-crate 1.0.0: license expression GPL-3.0-only does not resolve to the G2 list or the cargo addendum; not admitted: GPL-3.0-only (G2)",
			edgeTitle + "ref-crate 0.1.0: license expression LicenseRef-Proprietary does not resolve to the G2 list or the cargo addendum; not admitted: LicenseRef-Proprietary (G2)",
		}},
		{"a cfg(windows) dependency without --filter-platform", "cargo-platform-unfiltered.json", cargo, []string{
			"platform-pdk 0.2.0: win-only-crate 1.0.0: license expression SSPL-1.0 does not resolve to the G2 list or the cargo addendum; not admitted: SSPL-1.0 (G2)",
		}},
		{"the same dependency filtered for wasm32", "cargo-platform-filtered.json", cargo, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := analyzeCargo(decodeFixture(t, tt.fixture), tt.pol, readLegal, os.ReadFile)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(res.findings, tt.want) {
				t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(res.findings, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

// TestAnalyzeCargoNotices: the shipped crates carry their legal texts, a
// declared license-file among them, the reviewed and elected licenses, and
// the MPL-2.0 mark of a crate used under its exception.
func TestAnalyzeCargoNotices(t *testing.T) {
	pol := testPolicy(t, ecosystemCargo, []string{"Unicode-3.0", "Apache-2.0 WITH LLVM-exception"},
		[]string{"cargo:mpl-crate"}, map[string]string{"cargo:elect-crate": "Zlib"},
		map[string]string{"cargo:nolicense-crate": "MIT", "cargo:licensefile-crate": "BSD-3-Clause"})
	res, err := analyzeCargo(decodeFixture(t, "cargo-edge.json"), pol, readLegal, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	if res.title != "edge-plugin-api 0.1.0, edge-plugin-util 0.1.0" || !res.plural || len(res.crates) != 18 {
		t.Fatalf("title %q, plural %v, %d crates", res.title, res.plural, len(res.crates))
	}
	byName := map[string]moduleNotice{}
	for _, c := range res.crates {
		byName[c.Path+" "+c.Version] = c
	}
	rels := func(m moduleNotice) []string {
		var out []string
		for _, x := range m.Texts {
			out = append(out, x.Rel)
		}
		return out
	}
	if c := byName["licensefile-crate 0.4.2"]; c.Override != "BSD-3-Clause" || !slices.Equal(rels(c), []string{"COPYRIGHT.txt"}) {
		t.Errorf("licensefile-crate = %+v", c)
	}
	if c := byName["elect-crate 0.8.0"]; c.Election != "Zlib" || c.Expression != "Zlib OR GPL-3.0-only" || !slices.Equal(rels(c), []string{"LICENSE-ZLIB"}) {
		t.Errorf("elect-crate = %+v", c)
	}
	if c := byName["mpl-crate 2.0.1"]; !c.MPL {
		t.Errorf("mpl-crate is not marked MPL-2.0: %+v", c)
	}
	if c := byName["git-crate 0.7.0"]; c.Source != "https://github.com/example/git-crate at commit 00e45b2e4da1b509566f27b78bdda00973c464a1" {
		t.Errorf("git-crate source = %q", c.Source)
	}
	if c := byName["twin-crate 2.0.0"]; c.Source != "a local path (../vendor/twin-2), not published" || c.Expression != "BSD-2-Clause" {
		t.Errorf("twin-crate 2.0.0 = %+v", c)
	}
	tp := renderCrateThirdParty(res)
	for _, want := range []string{
		"edge-plugin-api 0.1.0, edge-plugin-util 0.1.0 are licensed under the Apache License, Version 2.0\n(see LICENSE and NOTICE). A Plugin built with them links",
		"  elect-crate 0.8.0: Zlib OR GPL-3.0-only; Ruralz elects Zlib\n",
		"  nolicense-crate 0.2.0: MIT (reviewed)\n",
		"MPL-2.0 components are used unmodified.",
		sectionRule + "\nmpl-crate 2.0.1\nLicense: MPL-2.0\nSource: a local path (../vendor/mpl-crate), not published\nMPL-2.0 Source Code Form: the Source address above.\n",
		sectionRule + "\nnolicense-crate 0.2.0\nLicense: MIT (reviewed)\nSource: a local path (../vendor/nolicense-crate), not published\n" + textRule + "\nThe package ships no license text; its license was reviewed as stated above.\n",
		"COPYRIGHT.txt:\n\nCopyright 2026 Example Authors.",
	} {
		if !strings.Contains(tp, want) {
			t.Errorf("THIRD_PARTY_LICENSES lacks %q:\n%s", want, tp)
		}
	}
	notice := renderCrateNotice(res, "Ruralz\n")
	if !strings.Contains(notice, "\nelect-crate 0.8.0 is offered under Zlib OR GPL-3.0-only; Ruralz elects Zlib.\n") {
		t.Errorf("NOTICE lacks the election:\n%s", notice)
	}
}

// TestCargoNoticesGolden: the example PDK's THIRD_PARTY_LICENSES and
// NOTICE, the latter with the vendored crate's NOTICE text.
func TestCargoNoticesGolden(t *testing.T) {
	res, err := analyzeCargo(decodeFixture(t, "cargo-pdk.json"), realPolicy(t, ecosystemCargo), readLegal, os.ReadFile)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "cargo-pdk.THIRD_PARTY_LICENSES", renderCrateThirdParty(res))
	golden(t, "cargo-pdk.NOTICE", renderCrateNotice(res, repoNoticeText))
	if plain := renderCrateNotice(cargoResult{title: "x 1.0.0", crates: []moduleNotice{{Path: "a", Version: "1.0.0", Expression: "MIT"}}}, repoNoticeText); plain != repoNoticeText {
		t.Errorf("NOTICE without notices or elections = %q, want the repository NOTICE", plain)
	}
	if empty := renderCrateThirdParty(cargoResult{title: "x 1.0.0"}); !strings.Contains(empty, "Components:\n  None.\n") {
		t.Errorf("no crates:\n%s", empty)
	}
	textless := renderCrateThirdParty(cargoResult{title: "x 1.0.0", crates: []moduleNotice{{Path: "a", Version: "1.0.0", Expression: "MIT", Source: "s"}}})
	if !strings.Contains(textless, textRule+"\nThe package ships no license text; its manifest declares the license above.\n") {
		t.Errorf("a crate without license texts:\n%s", textless)
	}
}

// TestWriteCrateNotices writes a PDK package's two files into an absent
// or empty directory and refuses a directory an earlier run filled.
func TestWriteCrateNotices(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pdk")
	res := cargoResult{title: "x 1.0.0", crates: []moduleNotice{{Path: "a", Version: "1.0.0", Expression: "MIT", Source: "s", Texts: lic(mitText)}}}
	if err := writeCrateNotices(dir, res, repoNoticeText); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{thirdPartyName, noticeName} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || len(data) == 0 { //nolint:gosec // The test reads its own output.
			t.Errorf("%s: %v, %d bytes", name, err, len(data))
		}
	}
	if err := writeCrateNotices(dir, res, repoNoticeText); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("non-empty directory: err = %v", err)
	}
}

// repoNoticeText stands for the repository NOTICE in notice tests.
const repoNoticeText = "Ruralz\nCopyright 2026 Revington\n"

// noticeRoot returns a module root holding only the repository NOTICE.
func noticeRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "NOTICE"), []byte(repoNoticeText), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestRunMainCargo runs depgate -cargo end to end: from standard input or
// a file, a pass printing its line and writing the PDK package's notices,
// findings with exit status 1, and unreadable input with exit status 2.
func TestRunMainCargo(t *testing.T) {
	root := noticeRoot(t)
	dir := filepath.Join(t.TempDir(), "notices")
	var stdout, stderr bytes.Buffer
	code := runMain(t.Context(), []string{"-C", root, "-cargo", "-", "-notices", dir}, strings.NewReader(fixture(t, "cargo-pdk.json")), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr.String())
	}
	if want := "depgate: G2 passes over the 7 crates example-pdk 0.1.0 ships\ndepgate: wrote THIRD_PARTY_LICENSES and NOTICE of example-pdk 0.1.0 under " + dir + "\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	for file, goldenName := range map[string]string{thirdPartyName: "cargo-pdk.THIRD_PARTY_LICENSES", noticeName: "cargo-pdk.NOTICE"} {
		got, err := os.ReadFile(filepath.Join(dir, file)) //nolint:gosec // The test reads its own output.
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("testdata", goldenName)) //nolint:gosec // A golden file of this package.
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s", file, goldenName)
		}
	}

	in := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(in, []byte(fixture(t, "cargo-edge.json")), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runMain(t.Context(), []string{"-C", root, "-cargo", in, "-notices", filepath.Join(t.TempDir(), "n")}, nil, &stdout, &stderr); code != 1 {
		t.Fatalf("edge workspace: exit %d, want 1:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), edgeTitle+"gpl-crate 1.0.0: license expression GPL-3.0-only") || !strings.HasSuffix(stderr.String(), "depgate: 10 finding(s)\n") || stdout.Len() != 0 {
		t.Errorf("edge workspace: stdout %q, stderr:\n%s", stdout.String(), stderr.String())
	}

	for _, tt := range []struct {
		args  []string
		stdin string
		want  string
	}{
		{[]string{"-cargo", "-"}, "{", "depgate: reading the cargo metadata output: unexpected EOF"},
		{[]string{"-cargo", filepath.Join(t.TempDir(), "absent.json")}, "", "depgate: open "},
		{[]string{"-cargo", "-", "-pnpm", "-"}, "", "depgate: -cargo and -pnpm gate different artifacts"},
		{[]string{"-C", t.TempDir(), "-cargo", "-", "-notices", filepath.Join(t.TempDir(), "n")}, fixture(t, "cargo-pdk.json"), "depgate: reading the repository NOTICE"},
	} {
		stderr.Reset()
		if code := runMain(t.Context(), tt.args, strings.NewReader(tt.stdin), &bytes.Buffer{}, &stderr); code != 2 || !strings.HasPrefix(stderr.String(), tt.want) {
			t.Errorf("runMain(%q) = %d, %q; want 2, %q", tt.args, code, stderr.String(), tt.want)
		}
	}
}
