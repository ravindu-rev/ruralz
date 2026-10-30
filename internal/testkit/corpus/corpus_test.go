// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package corpus

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Tests that the corpus reads the layout of docs/engineering/03-testing-
// and-quality-strategy.md "Golden tests": golden entries with their
// levels, diff entries and JSON twins, JSON subset pairs and negative
// fixtures, and that seed corpora come from it through shared helpers.

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
		"golden/minimal/bundle/ruralz.yaml":                    "kind: Gateway\n",
		"golden/minimal/bundle/routes/orders.yaml":             "kind: Route\n",
		"golden/minimal/bundle/nested/deeper/x.yml":            "k: v\n",
		"golden/minimal/bundle/raw/crlf.yaml":                  "kind: Route\r\n",
		"golden/minimal/bundle-json/ruralz.yaml":               `{"kind": "Gateway"}`,
		"golden/minimal/environments.yaml":                     "kind: Environment\n",
		"golden/minimal/level-0/expected.json":                 `{"level":0,"digests":{"-":"sha256:00"}}`,
		"golden/minimal/level-0/canonical/prod.json":           `{"prod":true}`,
		"golden/minimal/level-0/canonical/-.json":              `{"none":true}`,
		"golden/minimal/level-0/effective/prod/orders.txt":     "rows\n",
		"golden/minimal/level-0/diff/-.prod.json":              "{}\n",
		"golden/minimal/level-10/expected.json":                `{"level":10,"digests":{}}`,
		"golden/minimal/level-10/canonical/deep/x.json":        "{}",
		"golden/minimal/level-2/expected.json":                 `{"level":2,"digests":{}}`,
		"golden/minimal/level-01/expected.json":                "{}",
		"golden/minimal/expected.json":                         "{}",
		"golden/minimal/CHANGES":                               "",
		"golden/shop-diff/from/ruralz.yaml":                    "kind: Gateway\n",
		"golden/shop-diff/to/ruralz.yaml":                      "kind: Gateway\n",
		"golden/shop-diff/to/routes/a.yaml":                    "kind: Route\n",
		"golden/shop-diff/level-0/diff/prod.prod.txt":          "header\n",
		"golden/shop-json/bundle/ruralz.yaml":                  `{"kind":"Gateway"}`,
		"golden/shop-json/bundle/routes/a.json":                `{"kind":"Route"}`,
		"golden/shop-json/fixture.yaml":                        "code: RZ-CFG-001\n",
		"json-subset/tabs/yaml/ruralz.yaml":                    "a: 1\n",
		"json-subset/tabs/json/ruralz.yaml":                    "{\t\"a\": 1}",
		"json-subset/tabs/README":                              "ignored member\n",
		"negative/RZ-CFG-005-missing-spec/fixture.yaml":        "code: RZ-CFG-005\nstage: F\n",
		"negative/RZ-CFG-005-missing-spec/bundle/ruralz.yaml":  "kind: Gateway\n",
		"negative/RZ-CFG-005-missing-spec/bundle/sub/b.yaml":   "kind: Route\n",
		"negative/RZ-CFG-005-missing-spec/expected.json":       `[{"code":"RZ-CFG-005"}]`,
		"negative/RZ-CFG-005-missing-spec/expected.txt":        "ruralz.yaml:1:1 error RZ-CFG-005: x\n",
		"negative/RZ-CFG-005-missing-spec/nested/fixture.yaml": "code: RZ-CFG-006\n",
		"negative/RZ-CFG-005-missing-spec/nested/bundle/c.yml": "kind: Route\n",
		"negative/002-duplicate-key/fixture.yaml":              "stage: B\n",
		"negative/002-duplicate-key/bundle/ruralz.yaml":        "a: 1\na: 2\n",
		"negative/002-duplicate-key/notes.md":                  "an unknown file\n",
		"negative/rz-cfg-003-anchor/fixture.yaml":              "code: \"RZ-CFG-003\" # anchors\n",
		"loader/rz-cfg-004-tag/fixture.yaml":                   "binaries: [ruralz]\n",
		"loader/rz-cfg-004-tag/bundle/ruralz.yaml":             "a: !include x\n",
		"loader/rz-cfg-004-tag/environments.yaml":              "kind: Environment\n",
		"notes/README.md":                                      "not a fixture\n",
		".hidden/fixture.yaml":                                 "code: RZ-CFG-001\n",
		"negative/RZ-RT-001-weird/fixture.yaml":                "",
		"negative/readme-fixture/fixture.yaml":                 "",
		"golden/big/bundle/huge.yaml":                          strings.Repeat("x", MaxSeedBytes+1),
	})
	return root
}

func TestLoadGroupsEntries(t *testing.T) {
	root := sampleTree(t)
	// Special files that tests create are skipped, never read.
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "golden/minimal/bundle/escape.yaml")); err != nil {
		t.Fatal(err)
	}
	mkfifo(t, filepath.Join(root, "negative/RZ-CFG-005-missing-spec/bundle/x.yaml"))
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
		{Golden, "golden/shop-diff", ""},
		{Golden, "golden/shop-json", ""},
		{JSONSubset, "json-subset/tabs", ""},
		{Negative, "loader/rz-cfg-004-tag", "RZ-CFG-004"},
		{Negative, "negative/002-duplicate-key", "RZ-CFG-002"},
		{Negative, "negative/RZ-CFG-005-missing-spec", "RZ-CFG-005"},
		{Negative, "negative/RZ-CFG-005-missing-spec/nested", "RZ-CFG-006"},
		{Negative, "negative/RZ-RT-001-weird", "RZ-RT-001"},
		{Negative, "negative/readme-fixture", ""},
		{Negative, "negative/rz-cfg-003-anchor", "RZ-CFG-003"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("entries:\n got %v\nwant %v", got, want)
	}
	byName := map[string]Entry{}
	for _, e := range c.Entries {
		byName[e.Name] = e
	}
	m := byName["golden/minimal"]
	var bundles []string
	for _, b := range m.Bundles {
		bundles = append(bundles, b.Name)
	}
	if !slices.Equal(bundles, []string{BundleDir, BundleJSONDir}) || m.IsDiff() {
		t.Fatalf("minimal bundles = %q, diff %v", bundles, m.IsDiff())
	}
	b, ok := m.Bundle(BundleDir)
	if !ok {
		t.Fatal("minimal has no bundle/")
	}
	var names []string
	for _, f := range b.Files {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"nested/deeper/x.yml", "raw/crlf.yaml", "routes/orders.yaml", "ruralz.yaml"}) {
		t.Fatalf("minimal bundle files = %v", names)
	}
	if string(b.Files[1].Data) != "kind: Route\r\n" {
		t.Errorf("a raw/ file changed: %q", b.Files[1].Data)
	}
	if string(m.Environments) != "kind: Environment\n" {
		t.Fatalf("minimal environments = %q", m.Environments)
	}
	var levels []int
	for _, l := range m.Levels {
		levels = append(levels, l.N)
	}
	if !slices.Equal(levels, []int{0, 2, 10}) {
		t.Fatalf("minimal levels = %v, want [0 2 10] (level-01 is no level)", levels)
	}
	l0, ok := m.Level(0)
	if !ok || !bytes.Contains(l0.Expected, []byte(`"digests"`)) {
		t.Fatalf("level 0 = %+v", l0)
	}
	if len(l0.Canonical) != 2 || string(l0.Canonical["prod"]) != `{"prod":true}` || l0.Canonical["-"] == nil {
		t.Fatalf("level 0 canonical = %v", l0.Canonical)
	}
	if data, ok := l0.File("effective/prod/orders.txt"); !ok || string(data) != "rows\n" {
		t.Errorf("level 0 effective table = %q, %v", data, ok)
	}
	if _, ok := l0.File("nope"); ok {
		t.Error("File(nope) found a file")
	}
	var l0names []string
	for _, f := range l0.Files {
		l0names = append(l0names, f.Name)
	}
	if !slices.Equal(l0names, []string{"canonical/-.json", "canonical/prod.json", "diff/-.prod.json", "effective/prod/orders.txt", "expected.json"}) {
		t.Errorf("level 0 files = %v", l0names)
	}
	if l10, _ := m.Level(10); len(l10.Canonical) != 0 {
		t.Errorf("level 10 canonical = %v; a nested name is no render", l10.Canonical)
	}
	if _, ok := m.Level(1); ok {
		t.Error("Level(1) found a level")
	}
	if want := []string{"CHANGES", "expected.json", "level-01/expected.json"}; !slices.Equal(m.Unknown, want) {
		t.Errorf("minimal unknown = %q, want %q", m.Unknown, want)
	}
	if _, ok := byName["golden/big"]; ok {
		t.Fatal("an entry holding only an oversized file was loaded")
	}
	diff := byName["golden/shop-diff"]
	if !diff.IsDiff() || len(diff.Levels) != 1 || len(diff.Bundles) != 2 {
		t.Fatalf("diff entry = %+v", diff)
	}
	if js := byName["golden/shop-json"]; js.Fixture != nil || !slices.Equal(js.Unknown, []string{"fixture.yaml"}) {
		t.Errorf("a golden entry's fixture.yaml = %q, unknown %q", js.Fixture, js.Unknown)
	}
	subset := byName["json-subset/tabs"]
	if len(subset.Bundles) != 2 || subset.Bundles[0].Name != JSONDir || subset.Bundles[1].Name != YAMLDir {
		t.Fatalf("subset bundles = %+v", subset.Bundles)
	}
	if !slices.Equal(subset.Unknown, []string{"README"}) {
		t.Errorf("subset unknown = %q", subset.Unknown)
	}
	neg := byName["negative/RZ-CFG-005-missing-spec"]
	if len(neg.Bundles) != 1 || len(neg.Bundles[0].Files) != 2 || neg.Fixture == nil || neg.Expected == nil || neg.ExpectedText == nil {
		t.Fatalf("negative = %+v", neg)
	}
	if neg.IsDiff() || len(neg.Levels) != 0 {
		t.Errorf("negative fixture with levels or diff: %+v", neg)
	}
	if dup := byName["negative/002-duplicate-key"]; !slices.Equal(dup.Unknown, []string{"notes.md"}) {
		t.Errorf("negative unknown = %q", dup.Unknown)
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

func TestSeeds(t *testing.T) { // YAML, JSON, canonical and Bundle seeds
	c, err := Load(sampleTree(t))
	if err != nil {
		t.Fatal(err)
	}
	yaml := c.YAML()
	if len(yaml) != 26 {
		var names []string
		for _, f := range c.Files(".yaml", ".yml") {
			names = append(names, f.Name)
		}
		t.Fatalf("YAML seeds = %d: %v", len(yaml), names)
	}
	json := c.JSON()
	if len(json) != 11 {
		t.Fatalf("JSON seeds = %d", len(json))
	}
	canon := c.Canonical()
	if len(canon) != 2 || string(canon[0]) != `{"none":true}` || string(canon[1]) != `{"prod":true}` {
		t.Fatalf("Canonical = %q", canon)
	}
	arch := c.Archives()
	// golden/minimal bundle and bundle-json, golden/shop-diff from and to,
	// golden/shop-json, json-subset yaml and json, four negative Bundles.
	if len(arch) != 11 {
		t.Fatalf("Archives = %d", len(arch))
	}
	a := ParseArchive(arch[0])
	if string(a.Comment) != "golden/minimal/bundle\n" || len(a.Files) != 4 {
		t.Fatalf("first archive = %q", arch[0])
	}
	if got := a.Map()["routes/orders.yaml"]; string(got) != "kind: Route\n" {
		t.Fatalf("archive file = %q", got)
	}
	if all := c.Files(); len(all) < len(yaml)+len(json) {
		t.Fatalf("Files() = %d", len(all))
	}
}

func TestAbsent(t *testing.T) { // tolerant of absent dirs
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
		{"RZ-CFG-021", "", "RZ-CFG-021"},
		{"RZ-CFG-0055-x", "", ""},
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

func TestParseLevel(t *testing.T) {
	for _, tt := range []struct {
		name string
		n    int
		ok   bool
	}{
		{"level-0", 0, true},
		{"level-7", 7, true},
		{"level-12", 12, true},
		{"level-012", 0, false},
		{"level-", 0, false},
		{"level--1", 0, false},
		{"level-+1", 0, false},
		{"level-1a", 0, false},
		{"level-1234567890", 0, false},
		{"levels-1", 0, false},
		{"Level-1", 0, false},
	} {
		n, ok := ParseLevel(tt.name)
		if n != tt.n || ok != tt.ok {
			t.Errorf("ParseLevel(%q) = %d, %v; want %d, %v", tt.name, n, ok, tt.n, tt.ok)
		}
	}
	for _, n := range []int{0, 3, 41} {
		if got, ok := ParseLevel(LevelDir(n)); !ok || got != n {
			t.Errorf("ParseLevel(LevelDir(%d)) = %d, %v", n, got, ok)
		}
	}
}

// TestNewestRelease checks the release tag the corpus files are held by:
// the highest vMAJOR.MINOR.PATCH, compared as numbers, never a release
// candidate's or another tag's.
func TestNewestRelease(t *testing.T) {
	for _, tt := range []struct {
		tags []string
		want string
	}{
		{nil, ""},
		{[]string{"", "latest", "v1", "v1.2", "1.2.3", "v01.2.3", "v1.2.3-rc.1"}, ""},
		{[]string{"v0.1.0"}, "v0.1.0"},
		{[]string{"v0.9.0", "v0.10.0", "v0.2.5"}, "v0.10.0"},
		{[]string{"v1.0.0-rc.1", "v0.3.1", "v0.3.0", " v0.3.2 "}, "v0.3.2"},
		{[]string{"v2.0.0", "v10.0.0", "v9.99.99"}, "v10.0.0"},
	} {
		if got := NewestRelease(tt.tags); got != tt.want {
			t.Errorf("NewestRelease(%q) = %q, want %q", tt.tags, got, tt.want)
		}
	}
}

// TestHeld checks the files a release holds on a Git repository: none
// before the first release, then the corpus files of the newest release
// tag (never a candidate's), wherever the tag sits, and only those under
// the corpus directory.
func TestHeld(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		//nolint:gosec // Test arguments, passed without a shell.
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	writeTree(t, dir, map[string]string{
		Dir + "/golden/a/bundle/ruralz.yaml": "a: 1\n",
		"docs/x.md":                          "x\n",
	})
	run("add", ".")
	run("commit", "-q", "-m", "base")
	tag, held, err := Held(t.Context(), dir)
	if err != nil || tag != "" || len(held) != 0 {
		t.Fatalf("before a release: %q, %v, %v", tag, held, err)
	}
	run("tag", "v0.1.0")
	writeTree(t, dir, map[string]string{Dir + "/golden/b/bundle/ruralz.yaml": "b: 1\n"})
	run("add", ".")
	run("commit", "-q", "-m", "next")
	run("tag", "v0.2.0-rc.1")
	tag, held, err = Held(t.Context(), dir)
	if err != nil || tag != "v0.1.0" || len(held) != 1 || !held[Dir+"/golden/a/bundle/ruralz.yaml"] {
		t.Fatalf("after a candidate: %q, %v, %v", tag, held, err)
	}
	// A patch tagged on a release branch, unreachable from main.
	run("checkout", "-q", "-b", "release-0.1", "v0.1.0")
	writeTree(t, dir, map[string]string{Dir + "/golden/c/bundle/ruralz.yaml": "c: 1\n"})
	run("add", ".")
	run("commit", "-q", "-m", "patch")
	run("tag", "v0.1.1")
	run("checkout", "-q", "main")
	tag, held, err = Held(t.Context(), dir)
	if err != nil || tag != "v0.1.1" || len(held) != 2 || !held[Dir+"/golden/c/bundle/ruralz.yaml"] || held["docs/x.md"] {
		t.Fatalf("after a patch: %q, %v, %v", tag, held, err)
	}
	if _, _, err := Held(t.Context(), t.TempDir()); !errors.Is(err, ErrNoGit) {
		t.Errorf("Held outside a repository = %v, want ErrNoGit", err)
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

func FuzzParseArchive(f *testing.F) { // Seeded through the corpus helpers themselves
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
