// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package corpus

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Tests for 11 req 27 (seed corpora come from the golden and negative
// corpora through a shared helper) and the WP-26 scope rule "tolerant of
// absent dirs".

// writeTree writes files (slash paths relative to root).
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func sampleTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "config")
	writeTree(t, root, map[string]string{
		"golden/minimal/bundle/ruralz.yaml":           "kind: Gateway\n",
		"golden/minimal/bundle/routes/orders.yaml":    "kind: Route\n",
		"golden/minimal/environments.yaml":            "kind: Environment\n",
		"golden/minimal/expected.json":                `{"level":0,"digests":{"-":"sha256:00"}}`,
		"golden/minimal/canonical/prod.json":          `{"prod":true}`,
		"golden/minimal/canonical/-.json":             `{"none":true}`,
		"golden/minimal/effective/prod/orders.txt":    "rows\n",
		"golden/minimal/CHANGES":                      "",
		"golden/shop-json/bundle/ruralz.yaml":         `{"kind":"Gateway"}`,
		"golden/shop-json/bundle/routes/a.json":       `{"kind":"Route"}`,
		"json-subset/tabs/yaml/ruralz.yaml":           "a: 1\n",
		"json-subset/tabs/json/ruralz.yaml":           "{\t\"a\": 1}",
		"RZ-CFG-005-missing-spec/fixture.yaml":        "code: RZ-CFG-005\nstage: F\n",
		"RZ-CFG-005-missing-spec/bundle/ruralz.yaml":  "kind: Gateway\n",
		"RZ-CFG-005-missing-spec/expected.json":       `{"code":"RZ-CFG-005"}`,
		"002-duplicate-key/fixture.yaml":              "stage: B\n",
		"002-duplicate-key/bundle/ruralz.yaml":        "a: 1\na: 2\n",
		"rz-cfg-003-anchor/fixture.yaml":              "code: \"RZ-CFG-003\" # anchors\n",
		"loader/rz-cfg-004-tag/fixture.yaml":          "binaries: [ruralz]\n",
		"loader/rz-cfg-004-tag/bundle/ruralz.yaml":    "a: !include x\n",
		"loader/rz-cfg-004-tag/environments.yaml":     "kind: Environment\n",
		"notes/README.md":                             "not a fixture\n",
		".hidden/fixture.yaml":                        "code: RZ-CFG-001\n",
		"RZ-RT-001-weird/fixture.yaml":                "",
		"readme-fixture/fixture.yaml":                 "",
		"golden/big/bundle/huge.yaml":                 strings.Repeat("x", MaxSeedBytes+1),
		"golden/minimal/bundle/nested/deeper/x.yml":   "k: v\n",
		"json-subset/tabs/README":                     "ignored member\n",
		"RZ-CFG-005-missing-spec/bundle/sub/b.yaml":   "kind: Route\n",
		"RZ-CFG-005-missing-spec/nested/fixture.yaml": "code: RZ-CFG-006\n",
		"RZ-CFG-005-missing-spec/nested/bundle/c.yml": "kind: Route\n",
	})
	return root
}

func TestLoadGroupsEntries(t *testing.T) { // 11 req 27
	root := sampleTree(t)
	// Special files that negative fixtures create are skipped, never read.
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "golden/minimal/bundle/escape.yaml")); err != nil {
		t.Fatal(err)
	}
	mkfifo(t, filepath.Join(root, "RZ-CFG-005-missing-spec/bundle/x.yaml"))
	c, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		kind Kind
		name string
		code string
	}
	var got []row
	for _, e := range c.Entries {
		got = append(got, row{e.Kind, e.Name, e.Code})
	}
	want := []row{
		{Golden, "golden/minimal", ""},
		{Golden, "golden/shop-json", ""},
		{JSONSubset, "json-subset/tabs", ""},
		{Negative, "002-duplicate-key", "RZ-CFG-002"},
		{Negative, "RZ-CFG-005-missing-spec", "RZ-CFG-005"},
		{Negative, "RZ-CFG-005-missing-spec/nested", "RZ-CFG-006"},
		{Negative, "RZ-RT-001-weird", "RZ-RT-001"},
		{Negative, "loader/rz-cfg-004-tag", "RZ-CFG-004"},
		{Negative, "readme-fixture", ""},
		{Negative, "rz-cfg-003-anchor", "RZ-CFG-003"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("entries:\n got %v\nwant %v", got, want)
	}
	byName := map[string]Entry{}
	for _, e := range c.Entries {
		byName[e.Name] = e
	}
	m := byName["golden/minimal"]
	if len(m.Bundles) != 1 || m.Bundles[0].Name != "bundle" {
		t.Fatalf("minimal bundles = %+v", m.Bundles)
	}
	var names []string
	for _, f := range m.Bundles[0].Files {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"nested/deeper/x.yml", "routes/orders.yaml", "ruralz.yaml"}) {
		t.Fatalf("minimal bundle files = %v", names)
	}
	if string(m.Environments) != "kind: Environment\n" || !bytes.Contains(m.Expected, []byte("digests")) {
		t.Fatalf("minimal metadata = %q %q", m.Environments, m.Expected)
	}
	if len(m.Canonical) != 2 || string(m.Canonical["prod"]) != `{"prod":true}` || m.Canonical["-"] == nil {
		t.Fatalf("canonical = %v", m.Canonical)
	}
	if _, ok := byName["golden/big"]; ok {
		t.Fatal("an entry holding only an oversized file was loaded")
	}
	subset := byName["json-subset/tabs"]
	if len(subset.Bundles) != 2 || subset.Bundles[0].Name != "json" || subset.Bundles[1].Name != "yaml" {
		t.Fatalf("subset bundles = %+v", subset.Bundles)
	}
	neg := byName["RZ-CFG-005-missing-spec"]
	if len(neg.Bundles) != 1 || len(neg.Bundles[0].Files) != 2 || neg.Fixture == nil || neg.Expected == nil {
		t.Fatalf("negative = %+v", neg)
	}
	tag := byName["loader/rz-cfg-004-tag"]
	if tag.Environments == nil || len(tag.Bundles) != 1 {
		t.Fatalf("nested negative = %+v", tag)
	}
	for _, f := range c.Files() {
		if strings.HasPrefix(f.Name, ".hidden/") || strings.HasSuffix(f.Name, "escape.yaml") || strings.HasSuffix(f.Name, "x.yaml") {
			t.Fatalf("file %s should be skipped", f.Name)
		}
	}
}

func TestSeeds(t *testing.T) { // 11 req 27: YAML, JSON, canonical and Bundle seeds
	c, err := Load(sampleTree(t))
	if err != nil {
		t.Fatal(err)
	}
	yaml := c.YAML()
	if len(yaml) != 20 {
		var names []string
		for _, f := range c.Files(".yaml", ".yml") {
			names = append(names, f.Name)
		}
		t.Fatalf("YAML seeds = %d: %v", len(yaml), names)
	}
	json := c.JSON()
	if len(json) != 5 {
		t.Fatalf("JSON seeds = %d", len(json))
	}
	canon := c.Canonical()
	if len(canon) != 2 || string(canon[0]) != `{"none":true}` {
		t.Fatalf("Canonical = %q", canon)
	}
	arch := c.Archives()
	// golden/minimal, golden/shop-json, json-subset yaml+json, four
	// negative Bundles.
	if len(arch) != 8 {
		t.Fatalf("Archives = %d", len(arch))
	}
	a := ParseArchive(arch[0])
	if string(a.Comment) != "golden/minimal/bundle\n" || len(a.Files) != 3 {
		t.Fatalf("first archive = %q", arch[0])
	}
	if got := a.Map()["routes/orders.yaml"]; string(got) != "kind: Route\n" {
		t.Fatalf("archive file = %q", got)
	}
	if all := c.Files(); len(all) < len(yaml)+len(json) {
		t.Fatalf("Files() = %d", len(all))
	}
}

func TestAbsent(t *testing.T) { // WP-26 scope: tolerant of absent dirs
	c, err := Load(filepath.Join(t.TempDir(), "no", "such"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Entries) != 0 || len(c.YAML()) != 0 || len(c.Archives()) != 0 || len(c.Canonical()) != 0 {
		t.Fatalf("absent corpus not empty: %+v", c)
	}
	// Present root, absent golden/ and json-subset/.
	root := t.TempDir()
	c, err = Load(root)
	if err != nil || len(c.Entries) != 0 {
		t.Fatalf("empty corpus: %+v, %v", c, err)
	}
}

func TestLoadErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission checks do not apply to root")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{"golden/x/bundle/a.yaml": "a: 1\n"})
	p := filepath.Join(root, "golden/x/bundle/a.yaml")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil {
		t.Fatal("unreadable file: want error")
	}
}

func TestRepoRootAndFind(t *testing.T) {
	root, err := RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "internal", "testkit", "corpus")); err != nil {
		t.Fatalf("RepoRoot = %s: %v", root, err)
	}
	dir, err := Find()
	if err != nil || dir != filepath.Join(root, "test", "conformance", "config") {
		t.Fatalf("Find = %s, %v", dir, err)
	}
	// The repository corpus may not exist yet; Default tolerates that.
	if _, err := Default(); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	writeTree(t, other, map[string]string{"go.mod": "module example.com/other\n"})
	if _, err := RepoRoot(other); !errors.Is(err, ErrNoRepo) {
		t.Fatalf("RepoRoot outside the module = %v, want ErrNoRepo", err)
	}
	for in, want := range map[string]string{
		"module github.com/ravindu-rev/ruralz\n":         Module,
		"// c\nmodule \"github.com/ravindu-rev/ruralz\"": Module,
		"go 1.26\n": "",
	} {
		if got := modulePath([]byte(in)); got != want {
			t.Errorf("modulePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNegativeCode(t *testing.T) {
	tests := []struct {
		dir, fixture, want string
	}{
		{"RZ-CFG-005-missing-spec", "", "RZ-CFG-005"},
		{"rz-cfg-013-literal", "", "RZ-CFG-013"},
		{"021-image", "", "RZ-CFG-021"},
		{"021", "", "RZ-CFG-021"},
		{"0210-x", "", ""},
		{"RZ-CFG-x", "", ""},
		{"RZ--001", "", ""},
		{"misc", "", ""},
		{"misc", "code: 'RZ-CFG-010'\n", "RZ-CFG-010"},
		{"RZ-CFG-001-x", "stage: B\n  code: nested\ncode:\n", "RZ-CFG-001"},
	}
	for _, tt := range tests {
		if got := negativeCode(tt.dir, []byte(tt.fixture)); got != tt.want {
			t.Errorf("negativeCode(%q, %q) = %q, want %q", tt.dir, tt.fixture, got, tt.want)
		}
	}
	if Golden.String() != "golden" || JSONSubset.String() != "json-subset" || Negative.String() != "negative" || Kind(9).String() != "Kind(9)" {
		t.Fatal("Kind.String")
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Archive
	}{
		{"empty", "", Archive{}},
		{"comment only", "hello", Archive{Comment: []byte("hello\n")}},
		{"files", "c\n-- a.yaml --\nx: 1\n-- b/c.json --\n{}", Archive{
			Comment: []byte("c\n"),
			Files:   []File{{Name: "a.yaml", Data: []byte("x: 1\n")}, {Name: "b/c.json", Data: []byte("{}\n")}},
		}},
		{"not markers", "--a--\n-- --\n--  --\n-- x --y\n", Archive{Comment: []byte("--a--\n-- --\n--  --\n-- x --y\n")}},
		{"crlf marker", "-- a --\r\nx\r\n", Archive{Files: []File{{Name: "a", Data: []byte("x\r\n")}}}},
		{"empty file", "-- a --\n-- b --\nq\n", Archive{Files: []File{{Name: "a"}, {Name: "b", Data: []byte("q\n")}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseArchive([]byte(tt.in))
			if !archivesEqual(got, &tt.want) {
				t.Fatalf("ParseArchive = %+v, want %+v", got, tt.want)
			}
			if again := ParseArchive(got.Format()); !archivesEqual(again, got) {
				t.Fatalf("round trip = %+v, want %+v", again, got)
			}
		})
	}
}

func archivesEqual(a, b *Archive) bool {
	if !bytes.Equal(a.Comment, b.Comment) || len(a.Files) != len(b.Files) {
		return false
	}
	for i := range a.Files {
		if a.Files[i].Name != b.Files[i].Name || !bytes.Equal(a.Files[i].Data, b.Files[i].Data) {
			return false
		}
	}
	return true
}

func FuzzParseArchive(f *testing.F) { // 11 req 27: seeded through the corpus helpers themselves
	AddArchives(f)
	AddYAML(f)
	AddJSON(f)
	AddCanonical(f)
	f.Add([]byte("comment\n-- a --\nx\n-- b --\n"))
	f.Add([]byte("-- a --"))
	f.Fuzz(func(t *testing.T, data []byte) {
		a := ParseArchive(data)
		// Formatting then parsing again is the identity on parsed archives.
		if again := ParseArchive(a.Format()); !archivesEqual(again, a) {
			t.Fatalf("round trip changed %q", data)
		}
	})
}
