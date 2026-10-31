// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"debug/buildinfo"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func linkedInfo() *buildinfo.BuildInfo {
	return &buildinfo.BuildInfo{
		GoVersion: "go1.27.3",
		Deps: []*debug.Module{
			{Path: "github.com/BurntSushi/toml", Version: "v1.4.0"},
			{Path: "example.com/old", Version: "v1.0.0", Replace: &debug.Module{Path: "example.com/new", Version: "v1.0.1"}},
		},
	}
}

// TestLegalProblems: NOTICE starts with the repository NOTICE, and
// THIRD_PARTY_LICENSES has a section for the Go standard library and every
// linked module.
func TestLegalProblems(t *testing.T) {
	bi := linkedInfo()
	full := []byte(thirdPartyText("go1.27.3", bi.Deps))
	notice := []byte(fixtureNotice + "\nThird-party notices\n")
	if got := legalProblems(notice, full, []byte(fixtureNotice), bi); len(got) != 0 {
		t.Errorf("complete files: %q", got)
	}
	cases := []struct {
		name               string
		notice, thirdParty string
		want               string
	}{
		{"foreign NOTICE", "Other\n", string(full), "NOTICE does not start with the repository NOTICE"},
		{"other Go", string(notice), thirdPartyText("go1.26.8", bi.Deps), "no section for the Go standard library go1.27.3"},
		{"missing module", string(notice), thirdPartyText("go1.27.3", bi.Deps[:1]), "no section for example.com/old v1.0.0"},
		{"heading not after a rule", string(notice), "Go standard library and runtime go1.27.3\n", "no section for the Go standard library"},
	}
	for _, c := range cases {
		if got := legalProblems([]byte(c.notice), []byte(c.thirdParty), []byte(fixtureNotice), bi); !hasFinding(got, c.want) {
			t.Errorf("%s: %q lack %q", c.name, got, c.want)
		}
	}
}

// TestSbomProblems: the SBOM is CycloneDX 1.7 JSON naming every linked
// module, whatever case, escaping or qualifiers its package URLs use.
func TestSbomProblems(t *testing.T) {
	dir := t.TempDir()
	bi := linkedInfo()
	cases := []struct {
		name, content, want string
	}{
		{"complete", `{"bomFormat":"CycloneDX","specVersion":"1.7","components":[` +
			`{"purl":"pkg:golang/github.com/burntsushi/toml@v1.4.0?type=module"},` +
			`{"components":[{"purl":"pkg:golang/example.com/new@v1.0.1#sub"}]}]}`, ""},
		{"spec version", `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[{"purl":"pkg:golang/github.com/BurntSushi/toml@v1.4.0"},{"purl":"pkg:golang/example.com/old@v1.0.0"}]}`, "want CycloneDX 1.7"},
		{"missing module", `{"bomFormat":"CycloneDX","specVersion":"1.7","components":[{"purl":"pkg:golang/example.com/old@v1.0.0"}]}`, "lacks github.com/BurntSushi/toml v1.4.0"},
		{"not JSON", `<bom/>`, "is not JSON"},
	}
	for _, c := range cases {
		file := filepath.Join(dir, c.name+".cdx.json")
		writeFile(t, file, c.content)
		got, err := sbomProblems(file, bi)
		if err != nil {
			t.Fatal(err)
		}
		if c.want == "" && len(got) > 0 || c.want != "" && !hasFinding(got, c.want) {
			t.Errorf("%s: problems %q, want %q", c.name, got, c.want)
		}
	}
	got, err := sbomProblems(filepath.Join(dir, "absent.cdx.json"), bi)
	if err != nil || !hasFinding(got, "absent.cdx.json is missing") {
		t.Errorf("missing SBOM: %q %v", got, err)
	}
}

func TestNormalizePurl(t *testing.T) {
	cases := map[string]string{
		"pkg:golang/github.com/A/B@v1.2.3%2Bincompatible?type=module#x": "pkg:golang/github.com/a/b@v1.2.3+incompatible",
		"pkg:golang/example.com/m@v1.0.0":                               "pkg:golang/example.com/m@v1.0.0",
	}
	for in, want := range cases {
		if got := normalizePurl(in); got != want {
			t.Errorf("normalizePurl(%q) = %q, want %q", in, got, want)
		}
	}
}
