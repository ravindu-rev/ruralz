// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"Apache License\nVersion 2.0, January 2004":                                                               "Apache-2.0",
		"Permission is hereby granted, free of charge, to any person":                                             "MIT",
		"Redistribution and use in source and binary forms ... Neither the name of Google":                        "BSD-3-Clause",
		"Redistribution and use in source and binary forms, with or without\nmodification, are permitted":         "BSD-2-Clause",
		"Permission to use, copy, modify, and/or distribute this software for any purpose with or without fee":    "ISC",
		"Mozilla Public License Version 2.0":                                                                      "MPL-2.0",
		"GNU GENERAL PUBLIC LICENSE Version 3":                                                                    "GPL",
		"GNU AFFERO GENERAL PUBLIC LICENSE":                                                                       "AGPL",
		"GNU LESSER GENERAL PUBLIC LICENSE":                                                                       "LGPL",
		"Server Side Public License":                                                                              "SSPL",
		"Business Source License 1.1":                                                                             "BUSL",
		"Redis Source Available License 2.0":                                                                      "RSAL",
		"Eclipse Public License - v 2.0":                                                                          "EPL",
		"Eclipse Distribution License - v 1.0 ... Redistribution and use in source and binary forms":              "EDL-1.0",
		"All rights reserved.":                                                                                    "unknown",
		"This GNU General Public License text also says Permission is hereby granted, free of charge, somewhere.": "GPL",
	}
	for text, want := range cases {
		if got := classify(text); got != want {
			t.Errorf("classify(%q) = %s, want %s", text, got, want)
		}
	}
}

func TestLegalKind(t *testing.T) {
	cases := map[string]string{
		"LICENSE": kindLicense, "license.txt": kindLicense, "LICENSE-APACHE": kindLicense, "LICENCE": kindLicense, //nolint:misspell // LICENCE is a real file name spelling.
		"COPYING": kindLicense, "UNLICENSE": kindLicense, "PATENTS": kindPatents, "NOTICE": kindNotice,
		"NOTICE.txt": kindNotice, "README.md": "", "go.mod": "", "licenses.go": kindLicense,
	}
	for name, want := range cases {
		if got := legalKind(name); got != want {
			t.Errorf("legalKind(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestReadLegal covers the directory reader: license, patent and notice
// files only, sorted, with CRLF and a byte order mark normalized away.
func TestReadLegal(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"LICENSE-MIT":    "Permission is hereby granted, free of charge\r\n\r\n",
		"LICENSE-APACHE": "\ufeffApache License\nVersion 2.0",
		"NOTICE":         "Copyright Example Authors",
		"PATENTS":        "Additional IP Rights Grant",
		"README.md":      "GNU General Public License",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "LICENSES"), 0o750); err != nil {
		t.Fatal(err)
	}
	got, err := readLegal(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []legalText{
		{Rel: "LICENSE-APACHE", Kind: kindLicense, Text: "Apache License\nVersion 2.0\n"},
		{Rel: "LICENSE-MIT", Kind: kindLicense, Text: "Permission is hereby granted, free of charge\n"},
		{Rel: "NOTICE", Kind: kindNotice, Text: "Copyright Example Authors\n"},
		{Rel: "PATENTS", Kind: kindPatents, Text: "Additional IP Rights Grant\n"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("readLegal = %+v\nwant %+v", got, want)
	}
	if _, err := readLegal(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing directory not reported")
	}
}

// fakeFS maps directories (slash paths) to their legal texts; the
// directory "broken" fails.
func fakeFS(dirs map[string][]legalText) dirReader {
	return func(dir string) ([]legalText, error) {
		dir = filepath.ToSlash(dir)
		if dir == "broken" {
			return nil, errors.New("read error")
		}
		return dirs[dir], nil
	}
}

func lic(text string) []legalText {
	return []legalText{{Rel: "LICENSE", Kind: kindLicense, Text: text + "\n"}}
}

const (
	mitText = "Permission is hereby granted, free of charge"
	gplText = "GNU General Public License"
	mplText = "Mozilla Public License Version 2.0"
	bsdText = "Redistribution and use in source and binary forms. Neither the name of"
)

func mod(path, version string) *goModule {
	return &goModule{Path: path, Version: version, Dir: path}
}

func pkg(importPath string, m *goModule) goPackage {
	return goPackage{ImportPath: importPath, Dir: filepath.FromSlash(importPath), Module: m}
}

func TestAnalyze(t *testing.T) {
	read := fakeFS(map[string][]legalText{
		"example.com/mit":                     lic(mitText),
		"example.com/gpl":                     lic(gplText),
		"example.com/dual":                    append(lic(mitText), legalText{Rel: "LICENSE-GPL", Kind: kindLicense, Text: gplText}),
		"example.com/mpl":                     lic(mplText),
		"github.com/hashicorp/raft":           lic(mplText),
		"github.com/eclipse/paho.golang":      {{Rel: "LICENSE", Kind: kindLicense, Text: "Eclipse Distribution License"}, {Rel: "epl-v20", Kind: kindLicense, Text: "Eclipse Public License"}},
		"github.com/gorilla/websocket":        lic("Redistribution and use in source and binary forms"),
		"example.com/crypto":                  lic(mitText),
		"golang.org/x/crypto":                 lic(bsdText),
		"github.com/quic-go/quic-go":          lic(mitText),
		"example.com/replacement":             lic(mitText),
		"github.com/hashicorp/raft-boltdb/v2": lic(mplText),
	})
	pkgs := []goPackage{
		{ImportPath: "fmt", Standard: true},
		{ImportPath: "github.com/ravindu-rev/ruralz/internal/cli", Module: &goModule{Path: "github.com/ravindu-rev/ruralz", Main: true}},
		pkg("example.com/mit/a", mod("example.com/mit", "v1.0.0")),
		pkg("example.com/mit/b", mod("example.com/mit", "v1.0.0")),
		pkg("example.com/gpl", mod("example.com/gpl", "v1.0.0")),
		pkg("example.com/dual", mod("example.com/dual", "v1.0.0")),
		pkg("example.com/mpl", mod("example.com/mpl", "v1.0.0")),
		pkg("github.com/hashicorp/raft", mod("github.com/hashicorp/raft", "v1.8.0")),
		pkg("github.com/hashicorp/raft-boltdb/v2", mod("github.com/hashicorp/raft-boltdb/v2", "v2.4.1")),
		pkg("github.com/eclipse/paho.golang/paho", mod("github.com/eclipse/paho.golang", "v0.23.0")),
		pkg("github.com/gorilla/websocket", mod("github.com/gorilla/websocket", "v1.5.1")),
		pkg("example.com/crypto/sign", mod("example.com/crypto", "v1.0.0")),
		pkg("golang.org/x/crypto/chacha20", mod("golang.org/x/crypto", "v0.40.0")),
		pkg("github.com/quic-go/quic-go/internal/handshake/crypto", mod("github.com/quic-go/quic-go", "v0.63.0")),
		{ImportPath: "example.com/replaced", Dir: "example.com/replacement", Module: &goModule{Path: "example.com/replaced", Version: "v1.0.0", Replace: mod("example.com/replacement", "")}},
		{ImportPath: "example.com/nodir", Module: &goModule{Path: "example.com/nodir", Version: "v1.0.0"}},
		pkg("example.com/nolicense", mod("example.com/nolicense", "v1.0.0")),
	}
	got, err := analyze(target{"ruralzd", "linux", "amd64"}, pkgs, read)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"example.com/gpl: license GPL is not allowed",
		"example.com/mpl: license MPL-2.0 is not allowed",
		"github.com/hashicorp/raft: ruralzd never links Raft",
		"github.com/hashicorp/raft-boltdb/v2: ruralzd never links Raft",
		"github.com/hashicorp/raft-boltdb/v2 v2.4.1 is below the advisory floor v2.4.2",
		"github.com/gorilla/websocket v1.5.1 is below the advisory floor v1.5.3",
		"example.com/crypto/sign: cryptography outside Go crypto",
		"golang.org/x/crypto/chacha20: golang.org/x/crypto package off the FIPS delegation allowlist",
		"example.com/nodir: module source not downloaded",
		"example.com/nolicense: no license text found",
	}
	joined := strings.Join(got.findings, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("missing finding %q in:\n%s", w, joined)
		}
	}
	if len(got.findings) != len(want) {
		t.Errorf("got %d findings, want %d:\n%s", len(got.findings), len(want), joined)
	}
	for _, g := range got.findings {
		if !strings.HasPrefix(g, "ruralzd linux/amd64: ") {
			t.Errorf("finding lacks its target: %q", g)
		}
	}
	// Every linked third-party module is reported once, sorted, with its
	// classified licenses; the paho election is recorded.
	var paths []string
	for _, m := range got.modules {
		paths = append(paths, m.Path)
		switch m.Path {
		case "example.com/mit":
			if !slices.Equal(m.IDs, []string{"MIT"}) || len(m.Texts) != 1 {
				t.Errorf("example.com/mit notice = %+v", m)
			}
		case "github.com/eclipse/paho.golang":
			if m.Election != "EDL-1.0" || !slices.Equal(m.IDs, []string{"EDL-1.0", "EPL"}) {
				t.Errorf("paho notice = %+v", m)
			}
		case "example.com/replaced":
			if m.ReplacePath != "example.com/replacement" || m.ReplaceVersion != "" {
				t.Errorf("replaced notice = %+v", m)
			}
		}
	}
	if !slices.IsSorted(paths) || len(paths) != 14 {
		t.Errorf("modules = %v, want 14 sorted paths", paths)
	}
	// Raft is allowed outside ruralzd.
	other, err := analyze(target{"ruralz-control", "linux", "amd64"}, pkgs[:8], read)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range other.findings {
		if strings.Contains(g, "never links Raft") || strings.Contains(g, "hashicorp/raft: license") {
			t.Errorf("unexpected finding for ruralz-control: %q", g)
		}
	}
	if _, err := analyze(target{"ruralz", "linux", "amd64"}, []goPackage{{ImportPath: "x", Module: &goModule{Path: "x", Dir: "broken"}}}, fakeFS(nil)); err == nil {
		t.Error("read error not reported")
	}
}

// TestAnalyzeJWXDenylist: ruralz never links jwx, at any major version,
// since the offline Policy checks it links import no module the CLI does
// not link (Dependency graph); ruralzd and ruralz-control link it freely,
// and the modules jwx links are not jwx.
func TestAnalyzeJWXDenylist(t *testing.T) {
	read := fakeFS(map[string][]legalText{
		"github.com/lestrrat-go/jwx/v4": lic(mitText),
		"github.com/lestrrat-go/jwx":    lic(mitText),
		"github.com/lestrrat-go/dsig":   lic(mitText),
	})
	jwx := []goPackage{
		pkg("github.com/lestrrat-go/jwx/v4/jws", mod("github.com/lestrrat-go/jwx/v4", "v4.5.0")),
		pkg("github.com/lestrrat-go/jwx/v4/jwk", mod("github.com/lestrrat-go/jwx/v4", "v4.5.0")),
	}
	old := []goPackage{pkg("github.com/lestrrat-go/jwx/jws", mod("github.com/lestrrat-go/jwx", "v1.2.29"))}
	dsig := []goPackage{pkg("github.com/lestrrat-go/dsig", mod("github.com/lestrrat-go/dsig", "v1.4.0"))}
	for _, tt := range []struct {
		name   string
		target target
		pkgs   []goPackage
		want   string
	}{
		{"ruralz links jwx v4", target{"ruralz", "windows", "amd64"}, jwx, "ruralz windows/amd64: github.com/lestrrat-go/jwx/v4: ruralz never links jwx"},
		{"ruralz links jwx v1", target{"ruralz", "linux", "arm64"}, old, "ruralz linux/arm64: github.com/lestrrat-go/jwx: ruralz never links jwx"},
		{"ruralz links dsig alone", target{"ruralz", "linux", "amd64"}, dsig, ""},
		{"ruralzd", target{"ruralzd", "linux", "amd64"}, jwx, ""},
		{"ruralz-control", target{"ruralz-control", "darwin", "arm64"}, jwx, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := analyze(tt.target, tt.pkgs, read)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if len(got.findings) != 0 {
					t.Errorf("findings = %q, want none", got.findings)
				}
				return
			}
			if len(got.findings) != 1 || !strings.HasPrefix(got.findings[0], tt.want) {
				t.Errorf("findings = %q, want one starting %q", got.findings, tt.want)
			}
		})
	}
}

// TestDenied covers the module match of the denylists: a root, a later
// major version or a path below it, and a sibling module named after it,
// never a module that only shares a prefix.
func TestDenied(t *testing.T) {
	roots := []string{"github.com/hashicorp/raft", "github.com/lestrrat-go/jwx"}
	cases := map[string]bool{
		"github.com/hashicorp/raft":           true,
		"github.com/hashicorp/raft-boltdb/v2": true,
		"github.com/lestrrat-go/jwx/v4":       true,
		"github.com/lestrrat-go/jwx":          true,
		"github.com/lestrrat-go/jwxtra":       false,
		"github.com/lestrrat-go/dsig":         false,
		"github.com/hashicorp/rafty":          false,
		"github.com/hashicorp/go-hclog":       false,
	}
	for path, want := range cases {
		if got := denied(path, roots); got != want {
			t.Errorf("denied(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestAnalyzeNestedLicense: a linked package inside a directory with its
// own license text is governed by that text, which G2 checks and the
// notices include; packages without one fall back to the module root.
func TestAnalyzeNestedLicense(t *testing.T) {
	read := fakeFS(map[string][]legalText{
		"example.com/m":                    append(lic(mitText), legalText{Rel: "NOTICE", Kind: kindNotice, Text: "Example notice\n"}),
		"example.com/m/third_party/gplish": lic(gplText),
		"example.com/m/third_party/bsdish": lic(bsdText),
	})
	m := mod("example.com/m", "v1.2.0")
	pkgs := []goPackage{
		pkg("example.com/m/a/b", m),
		pkg("example.com/m/third_party/gplish/x", m),
		pkg("example.com/m/third_party/bsdish", m),
	}
	got, err := analyze(target{"ruralz", "linux", "amd64"}, pkgs, read)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.findings) != 1 || !strings.Contains(got.findings[0], "example.com/m (third_party/gplish): license GPL is not allowed (G2)") {
		t.Errorf("findings = %q, want one for the nested GPL text", got.findings)
	}
	if len(got.modules) != 1 {
		t.Fatalf("modules = %+v", got.modules)
	}
	var rels []string
	for _, x := range got.modules[0].Texts {
		rels = append(rels, x.Rel)
	}
	if want := []string{"LICENSE", "NOTICE", "third_party/bsdish/LICENSE", "third_party/gplish/LICENSE"}; !slices.Equal(rels, want) {
		t.Errorf("texts = %v, want %v", rels, want)
	}
	if want := []string{"BSD-3-Clause", "GPL", "MIT"}; !slices.Equal(got.modules[0].IDs, want) {
		t.Errorf("IDs = %v, want %v", got.modules[0].IDs, want)
	}
}

// TestCheckLicenseOverride: a reviewed override replaces the classified
// texts of its key, a module root or a nested directory.
func TestCheckLicenseOverride(t *testing.T) {
	ids, override, finding := checkLicense("example.com/m", "example.com/m", lic("All rights reserved."))
	if finding == "" || override != "" || !slices.Equal(ids, []string{"unknown"}) {
		t.Errorf("unknown text: ids %v, override %q, finding %q", ids, override, finding)
	}
	if _, _, finding := checkLicense("example.com/m", "example.com/m", nil); !strings.Contains(finding, "no license text found") {
		t.Errorf("no text: finding %q", finding)
	}
	if _, _, finding := checkLicense("github.com/hashicorp/raft", "github.com/hashicorp/raft", lic(mplText)); finding != "" {
		t.Errorf("MPL-2.0 exception: finding %q", finding)
	}
}

func TestAnalyzeTestSupport(t *testing.T) {
	main := &goModule{Path: "github.com/ravindu-rev/ruralz", Main: true}
	inMain := func(rel string) goPackage {
		return goPackage{ImportPath: main.Path + "/" + rel, Module: main}
	}
	read := fakeFS(map[string][]legalText{
		"github.com/testcontainers/testcontainers-go":               lic(mitText),
		"github.com/testcontainers/testcontainers-go/modules/redis": lic(mitText),
		"go.yaml.in/yaml/v3": lic(mitText),
	})
	// go.yaml.in/yaml/v3 ships: cel-go's common/env links it into both
	// binaries (docs/engineering/01-tech-stack-and-libraries.md).
	denied := []goPackage{
		inMain("internal/testkit/proc"),
		inMain("internal/tool/depgate"),
		inMain("test/e2e"),
		inMain("internal/clock/clocktest"),
		inMain("internal/statestore/statestoretest/redisserver"),
		{ImportPath: "testing", Standard: true},
		{ImportPath: "testing/fstest", Standard: true},
		{ImportPath: "net/http/httptest", Standard: true},
		pkg("github.com/testcontainers/testcontainers-go", mod("github.com/testcontainers/testcontainers-go", "v0.44.0")),
		pkg("github.com/testcontainers/testcontainers-go/modules/redis", mod("github.com/testcontainers/testcontainers-go/modules/redis", "v0.44.0")),
	}
	allowed := []goPackage{
		pkg("go.yaml.in/yaml/v3", mod("go.yaml.in/yaml/v3", "v3.0.5")),
		inMain("internal/cli/test"),
		inMain("internal/signing/attest"),
		inMain("internal/testdata"),
		inMain("cmd/ruralzd"),
		{ImportPath: "internal/synctest", Standard: true},
		{ImportPath: "net/http", Standard: true},
	}
	for _, p := range denied {
		got, err := analyze(target{"ruralzd", "linux", "amd64"}, []goPackage{p}, read)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.findings) != 1 || !strings.Contains(got.findings[0], "never linked into a binary") {
			t.Errorf("%s: findings %q, want the test support denial", p.ImportPath, got.findings)
		}
	}
	for _, p := range allowed {
		got, err := analyze(target{"ruralzd", "linux", "amd64"}, []goPackage{p}, read)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.findings) != 0 {
			t.Errorf("%s: unexpected findings %q", p.ImportPath, got.findings)
		}
	}
}

func TestIsTestSupport(t *testing.T) {
	cases := map[string]bool{
		"internal/testkit":                     true,
		"internal/testkit/loadgen":             true,
		"internal/tool/releasekit":             true,
		"test":                                 true,
		"test/conformance/config":              true,
		"internal/clock/clocktest":             true,
		"internal/cel/celtest":                 true,
		"internal/gateway/handler/handlertest": true,
		"internal/statestore/statestoretest/x": true,
		"internal/testkitx":                    false,
		"internal/cli/test":                    false,
		"internal/signing/attest":              false,
		"testdata":                             false,
		"internal/gateway/handler":             false,
	}
	for rel, want := range cases {
		if got := isTestSupport(rel); got != want {
			t.Errorf("isTestSupport(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestDecodePackages(t *testing.T) {
	stream := `{"ImportPath":"fmt","Standard":true}
{"ImportPath":"example.com/a","Dir":"/m/a","Module":{"Path":"example.com/a","Version":"v1.2.3","Dir":"/m/a"}}`
	pkgs, err := decodePackages(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 || !pkgs[0].Standard || pkgs[1].Module.Version != "v1.2.3" || pkgs[1].Dir != "/m/a" {
		t.Fatalf("pkgs = %+v", pkgs)
	}
	if _, err := decodePackages(strings.NewReader("{")); err == nil {
		t.Error("bad JSON accepted")
	}
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.5.3", "v1.5.3", 0},
		{"v1.5.2", "v1.5.3", -1},
		{"v1.10.0", "v1.9.9", 1},
		{"v2.0.0-rc.1", "v2.0.0", -1},
		{"v2.0.0", "v2.0.0-rc.1", 1},
		{"v0.0.0-20260101000000-abcdef123456", "v0.0.1", -1},
		{"v1.2.3+incompatible", "v1.2.3", 0},
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestTargets: every binary on every platform it ships for, the darwin
// server archives (development only) included.
func TestTargets(t *testing.T) {
	n := map[string]int{}
	var names []string
	for _, tg := range targets() {
		n[tg.binary]++
		names = append(names, tg.name())
	}
	if n["ruralzd"] != 4 || n["ruralz-control"] != 4 || n["ruralz"] != 5 {
		t.Errorf("targets per binary = %v", n)
	}
	for _, want := range []string{"ruralzd_linux_amd64", "ruralzd_linux_arm64", "ruralzd_darwin_arm64", "ruralzd_darwin_amd64", "ruralz_windows_amd64"} {
		if !slices.Contains(names, want) {
			t.Errorf("targets lack %s: %v", want, names)
		}
	}
}

func TestEscapeModule(t *testing.T) {
	cases := map[string]string{
		"github.com/BurntSushi/toml": "github.com/!burnt!sushi/toml",
		"v1.2.3":                     "v1.2.3",
		"v0.0.0-2026-ABC":            "v0.0.0-2026-!a!b!c",
	}
	for in, want := range cases {
		if got := escapeModule(in); got != want {
			t.Errorf("escapeModule(%q) = %q, want %q", in, got, want)
		}
	}
}

func testStd() standardLibrary {
	return standardLibrary{Version: "go1.27.3", Texts: []legalText{
		{Rel: "LICENSE", Kind: kindLicense, Text: "Copyright The Go Authors.\n\n" + bsdText + "\n"},
		{Rel: "PATENTS", Kind: kindPatents, Text: "Additional IP Rights Grant (Patents)\n"},
	}}
}

// TestRenderThirdParty covers THIRD_PARTY_LICENSES: the Go standard
// library section, a section per module with its source address, the
// MPL-2.0 source statement only when an MPL-2.0 module is linked, and
// byte-identical output for identical input.
func TestRenderThirdParty(t *testing.T) {
	tg := target{"ruralzd", "linux", "amd64"}
	modules := []moduleNotice{
		{Path: "github.com/BurntSushi/toml", Version: "v1.4.0", IDs: []string{"MIT"}, Texts: lic(mitText)},
		{Path: "github.com/hashicorp/raft", Version: "v1.8.0", IDs: []string{"MPL-2.0"}, Texts: lic(mplText)},
		{Path: "example.com/old", Version: "v1.0.0", ReplacePath: "example.com/new", ReplaceVersion: "v1.0.1", IDs: []string{"Apache-2.0"}, Override: "Apache-2.0"},
	}
	got := renderThirdParty(tg, testStd(), modules)
	for _, want := range []string{
		"THIRD_PARTY_LICENSES for ruralzd on linux/amd64\n",
		"  Go standard library and runtime go1.27.3: BSD-3-Clause\n",
		"  github.com/BurntSushi/toml v1.4.0: MIT\n",
		sectionRule + "\nGo standard library and runtime go1.27.3\nLicense: BSD-3-Clause\nSource: https://go.dev/dl/go1.27.3.src.tar.gz\n",
		"PATENTS:\n\nAdditional IP Rights Grant (Patents)\n",
		sectionRule + "\ngithub.com/BurntSushi/toml v1.4.0\nLicense: MIT\nSource: https://proxy.golang.org/github.com/!burnt!sushi/toml/@v/v1.4.0.zip\n",
		"MPL-2.0 components are used unmodified.",
		"MPL-2.0 Source Code Form: the Source address above.\n",
		"Replaced by: example.com/new v1.0.1\nLicense: Apache-2.0 (reviewed)\nSource: https://proxy.golang.org/example.com/new/@v/v1.0.1.zip\n",
		"The module ships no license text; its license was reviewed as stated above.\n",
		"LICENSE:\n\n" + mitText + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("THIRD_PARTY_LICENSES lacks %q:\n%s", want, got)
		}
	}
	if again := renderThirdParty(tg, testStd(), modules); again != got {
		t.Error("rendering is not deterministic")
	}
	plain := renderThirdParty(tg, testStd(), modules[:1])
	if strings.Contains(plain, "MPL-2.0") {
		t.Errorf("no MPL-2.0 module, yet the file mentions MPL-2.0:\n%s", plain)
	}
	none := renderThirdParty(tg, testStd(), nil)
	if !strings.HasSuffix(none, "PATENTS:\n\nAdditional IP Rights Grant (Patents)\n\n") {
		t.Errorf("standard library only: unexpected end:\n%s", none)
	}
}

// TestRenderNotice: the repository NOTICE unchanged without upstream
// notices or elections; otherwise followed by the modules' NOTICE texts
// and the elections (ADR-0002).
func TestRenderNotice(t *testing.T) {
	tg := target{"ruralz", "darwin", "arm64"}
	repo := "Ruralz\nCopyright 2026 Revington\r\n"
	if got := renderNotice(tg, repo, []moduleNotice{{Path: "example.com/m", Version: "v1.0.0", Texts: lic(mitText)}}); got != "Ruralz\nCopyright 2026 Revington\n" {
		t.Errorf("plain NOTICE = %q", got)
	}
	modules := []moduleNotice{
		{Path: "example.com/m", Version: "v1.0.0", Texts: append(lic(mitText), legalText{Rel: "NOTICE", Kind: kindNotice, Text: "Example Corp product\n"})},
		{Path: "github.com/eclipse/paho.golang", Version: "v0.23.0", IDs: []string{"EDL-1.0", "EPL"}, Election: "EDL-1.0"},
	}
	got := renderNotice(tg, repo, modules)
	for _, want := range []string{
		"Ruralz\nCopyright 2026 Revington\n\n" + sectionRule + "\nThird-party notices\n",
		"ruralz on darwin/arm64 includes third-party software",
		textRule + "\nexample.com/m v1.0.0\n" + textRule + "\nNOTICE:\n\nExample Corp product\n",
		"github.com/eclipse/paho.golang v0.23.0 is offered under EDL-1.0 or EPL; Ruralz elects EDL-1.0.\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("NOTICE lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, mitText) {
		t.Error("NOTICE reproduces a license text; those belong in THIRD_PARTY_LICENSES")
	}
}

func TestWriteNotices(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notices")
	tg := target{"ruralz", "windows", "amd64"}
	results := map[target]analysis{tg: {modules: []moduleNotice{{Path: "example.com/m", Version: "v1.0.0", IDs: []string{"MIT"}, Texts: lic(mitText)}}}}
	if err := writeNotices(dir, []target{tg}, results, testStd(), "Ruralz\n"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{thirdPartyName, noticeName} {
		data, err := os.ReadFile(filepath.Join(dir, "ruralz_windows_amd64", name)) //nolint:gosec // The test reads its own output.
		if err != nil || len(data) == 0 {
			t.Errorf("%s: %v, %d bytes", name, err, len(data))
		}
	}
	// A second run into the same directory is refused: no stale notice
	// survives.
	if err := writeNotices(dir, []target{tg}, results, testStd(), "Ruralz\n"); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("non-empty directory: err = %v", err)
	}
}

func TestRunMainUsage(t *testing.T) {
	for _, args := range [][]string{{"extra"}, {"-unknown"}} {
		var stderr bytes.Buffer
		if code := runMain(t.Context(), args, &bytes.Buffer{}, &stderr); code != 2 {
			t.Errorf("runMain(%q) = %d, want 2 (%s)", args, code, stderr.String())
		}
	}
}

// TestRepositoryPasses runs every gate on the real tree, then writes the
// notices of every target and checks their shape.
func TestRepositoryPasses(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	dir := filepath.Join(t.TempDir(), "notices")
	var stdout, stderr bytes.Buffer
	if code := runMain(t.Context(), []string{"-C", root, "-notices", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("depgate exit %d:\n%s", code, stderr.String())
	}
	notice, err := os.ReadFile(filepath.Join(root, "NOTICE")) //nolint:gosec // The repository NOTICE under test.
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range targets() {
		tp, err := os.ReadFile(filepath.Join(dir, tg.name(), thirdPartyName)) //nolint:gosec // The test reads its own output.
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(tp, []byte("\n"+sectionRule+"\n"+stdHeading+" go1.")) {
			t.Errorf("%s: THIRD_PARTY_LICENSES has no Go standard library section", tg.name())
		}
		n, err := os.ReadFile(filepath.Join(dir, tg.name(), noticeName)) //nolint:gosec // The test reads its own output.
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(n, bytes.TrimRight(notice, "\n")) {
			t.Errorf("%s: NOTICE does not start with the repository NOTICE", tg.name())
		}
	}
}
