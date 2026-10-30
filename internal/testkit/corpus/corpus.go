// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package corpus reads the configuration conformance corpus under
// test/conformance/config (docs/engineering/03-testing-and-quality-
// strategy.md "Golden tests"): the golden entries, the JSON subset pairs
// and the negative fixtures. The conformance suite runs every entry from
// it, and every fuzz target that parses configuration seeds itself
// through its helpers (TQ "Fuzzing": "seeded from the golden and negative
// corpora"):
//
//	func FuzzProfileYAML(f *testing.F) {
//		corpus.AddYAML(f)
//		f.Fuzz(func(t *testing.T, data []byte) { ... })
//	}
//
// # Layout
//
// A golden entry, golden/<entry>/, holds its inputs and its outputs per
// schema level:
//
//	bundle/              the input Bundle, a frozen copy
//	bundle-json/         optionally, the JSON twin of bundle/
//	from/, to/           a diff entry's two Bundles, in place of bundle/
//	environments.yaml    the Environments the entry renders, if any
//	level-<n>/           the outputs at schema level n: expected.json
//	                     (the digests), canonical/<render>.json,
//	                     rendered/<render>.yaml and .json,
//	                     diagnostics/<render>.txt and .json,
//	                     effective/<render>/<route>.txt and .json,
//	                     diff/<from>.<to>.txt and .json
//
// A JSON subset pair, json-subset/<case>/, holds the two Bundles yaml/ and
// json/. A negative fixture is any other directory that holds a
// fixture.yaml, by convention negative/<code>-<slug>/: its Bundle in
// bundle/, an optional environments.yaml and its expected diagnostics in
// expected.txt and expected.json. A raw/ directory, wherever it sits, is
// an ordinary directory of the Bundle that holds it; only Git treats it
// apart (.gitattributes stores it byte for byte).
//
// Every part is optional: an absent directory yields no entries and no
// error, so targets run before the corpus exists. Only regular files are
// read (symbolic links, FIFOs and other special files that tests create
// are skipped), dot-directories are skipped, and files above MaxSeedBytes
// are left out.
package corpus

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Module is the module path whose go.mod marks the repository root.
const Module = "github.com/ravindu-rev/ruralz"

// ErrNoRepo reports that no go.mod declaring Module encloses the start
// directory; seeding helpers treat it as an absent corpus.
var ErrNoRepo = errors.New("corpus: not inside the " + Module + " module")

// Dir is the corpus directory relative to the repository root.
const Dir = "test/conformance/config"

// MaxSeedBytes bounds one seed file; larger files are skipped (hostile
// inputs are generated at test time, never committed).
const MaxSeedBytes = 1 << 20

// Names of the directories and files of the layout.
const (
	// GoldenDir holds the golden entries.
	GoldenDir = "golden"
	// JSONSubsetDir holds the JSON subset pairs.
	JSONSubsetDir = "json-subset"
	// NegativeDir holds the negative fixtures.
	NegativeDir = "negative"
	// BundleDir is the input Bundle of a golden entry or a negative
	// fixture.
	BundleDir = "bundle"
	// BundleJSONDir is the JSON twin of a golden entry's Bundle.
	BundleJSONDir = "bundle-json"
	// FromDir and ToDir are the Bundles of a diff entry.
	FromDir = "from"
	ToDir   = "to"
	// YAMLDir and JSONDir are the Bundles of a JSON subset pair.
	YAMLDir = "yaml"
	JSONDir = "json"
	// EnvironmentsFile is the Environments an entry renders.
	EnvironmentsFile = "environments.yaml"
	// FixtureFile describes a negative fixture.
	FixtureFile = "fixture.yaml"
	// ExpectedFile is a level's digests, or a negative fixture's
	// diagnostics in their JSON form.
	ExpectedFile = "expected.json"
	// ExpectedTextFile is a negative fixture's diagnostics in their text
	// form.
	ExpectedTextFile = "expected.txt"
	// CanonicalDir holds a level's canonical/<render>.json files.
	CanonicalDir = "canonical"
	// levelPrefix starts the name of a level directory, level-<n>.
	levelPrefix = "level-"
)

// Kind classifies an entry.
type Kind int

// Entry kinds.
const (
	// Golden is a golden corpus entry (golden/<entry>/).
	Golden Kind = iota + 1
	// JSONSubset is a JSON subset pair (json-subset/<case>/).
	JSONSubset
	// Negative is a negative fixture (a directory holding fixture.yaml).
	Negative
)

// String returns the kind name.
func (k Kind) String() string {
	switch k {
	case Golden:
		return "golden"
	case JSONSubset:
		return "json-subset"
	case Negative:
		return "negative"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// File is one corpus file.
type File struct {
	// Name is the slash-separated path relative to its Bundle root (for
	// Bundle files), to its level directory (for outputs) or to its entry
	// directory.
	Name string
	Data []byte
}

// Bundle is one Bundle directory of an entry.
type Bundle struct {
	// Name is the directory name: BundleDir, BundleJSONDir, FromDir or
	// ToDir, or YAMLDir and JSONDir for a JSON subset pair.
	Name  string
	Files []File
}

// Level is the outputs of a golden entry at one schema level
// (level-<n>/).
type Level struct {
	// N is the schema level.
	N int
	// Expected is expected.json, the digests at the level.
	Expected []byte
	// Canonical holds canonical/<render>.json by render.
	Canonical map[string][]byte
	// Files holds every file of the level, expected.json included, by its
	// path relative to the level directory, sorted.
	Files []File
}

// File returns the level's file name.
func (l *Level) File(name string) ([]byte, bool) {
	i := slices.IndexFunc(l.Files, func(f File) bool { return f.Name == name })
	if i < 0 {
		return nil, false
	}
	return l.Files[i].Data, true
}

// Entry is one golden entry, JSON subset pair or negative fixture.
type Entry struct {
	Kind Kind
	// Name is the entry path relative to the corpus directory, for example
	// "golden/shop-bundle" or "negative/RZ-CFG-005-missing-spec".
	Name string
	// Code is the RZ code of a negative fixture: the "code:" line of
	// fixture.yaml when present, else the code the directory name starts
	// with ("RZ-CFG-005-..." or "005-..."); empty otherwise.
	Code string
	// Bundles are the entry's Bundle directories, sorted by name.
	Bundles []Bundle
	// Environments is environments.yaml when present.
	Environments []byte
	// Fixture is fixture.yaml of a negative fixture.
	Fixture []byte
	// Expected is expected.json of a negative fixture.
	Expected []byte
	// ExpectedText is expected.txt of a negative fixture.
	ExpectedText []byte
	// Levels are the schema levels of a golden entry, sorted by N.
	Levels []Level
	// Unknown lists the entry's files that fit no part of the layout, by
	// their paths relative to the entry directory.
	Unknown []string
}

// Bundle returns the entry's Bundle directory name.
func (e *Entry) Bundle(name string) (Bundle, bool) {
	i := slices.IndexFunc(e.Bundles, func(b Bundle) bool { return b.Name == name })
	if i < 0 {
		return Bundle{}, false
	}
	return e.Bundles[i], true
}

// IsDiff reports a diff entry: a golden entry whose Bundles are from/ and
// to/ in place of bundle/.
func (e *Entry) IsDiff() bool {
	_, from := e.Bundle(FromDir)
	_, to := e.Bundle(ToDir)
	return e.Kind == Golden && from && to
}

// Level returns the entry's level n.
func (e *Entry) Level(n int) (*Level, bool) {
	i := slices.IndexFunc(e.Levels, func(l Level) bool { return l.N == n })
	if i < 0 {
		return nil, false
	}
	return &e.Levels[i], true
}

// LevelDir returns the name of the directory of schema level n, level-<n>.
func LevelDir(n int) string { return levelPrefix + strconv.Itoa(n) }

// ParseLevel returns the schema level a directory name level-<n> names:
// n is a decimal integer without a sign or a leading zero.
func ParseLevel(name string) (int, bool) {
	digits, ok := strings.CutPrefix(name, levelPrefix)
	if !ok || digits == "" || !allDigits(digits) || (len(digits) > 1 && digits[0] == '0') || len(digits) > 9 {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	return n, err == nil
}

// Corpus is a loaded corpus.
type Corpus struct {
	// Root is the absolute corpus directory; it may not exist.
	Root string
	// Entries are sorted by kind and name.
	Entries []Entry
	// files holds every seed-sized regular file by corpus-relative path.
	files []File
}

// Find returns the absolute corpus directory of the repository containing
// the working directory. The directory itself may be absent.
func Find() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("corpus: %w", err)
	}
	root, err := RepoRoot(wd)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(Dir)), nil
}

// RepoRoot walks up from start to the directory whose go.mod declares
// Module.
func RepoRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("corpus: %w", err)
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // G304: fixed file name under a walked directory
		if err == nil && modulePath(data) == Module {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("%w: %s", ErrNoRepo, start)
		}
		dir = parent
	}
}

// modulePath returns the module directive of a go.mod file.
func modulePath(gomod []byte) string {
	for line := range strings.SplitSeq(string(gomod), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module"); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

// Default loads the corpus of the repository containing the working
// directory.
func Default() (*Corpus, error) {
	root, err := Find()
	if err != nil {
		return nil, err
	}
	return Load(root)
}

// Load reads the corpus at root. An absent root is an empty corpus.
func Load(root string) (*Corpus, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("corpus: %w", err)
	}
	c := &Corpus{Root: abs}
	if _, err := os.Stat(abs); errors.Is(err, fs.ErrNotExist) {
		return c, nil
	} else if err != nil {
		return nil, fmt.Errorf("corpus: %w", err)
	}
	all := map[string][]byte{}
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != abs && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxSeedBytes {
			return nil
		}
		data, err := os.ReadFile(p) //nolint:gosec // G304: walking the corpus directory
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return err
		}
		all[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("corpus: load %s: %w", abs, err)
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		c.files = append(c.files, File{Name: n, Data: all[n]})
	}
	c.Entries = entries(names, all)
	return c, nil
}

// entries groups the files into golden entries, JSON subset pairs and
// negative fixtures.
func entries(names []string, all map[string][]byte) []Entry {
	byDir := map[string]*Entry{}
	entryOf := func(kind Kind, dir string) *Entry {
		e, ok := byDir[dir]
		if !ok {
			e = &Entry{Kind: kind, Name: dir}
			byDir[dir] = e
		}
		return e
	}
	// Negative fixtures are the directories holding fixture.yaml outside
	// golden/ and json-subset/.
	var negatives []string
	for _, n := range names {
		if path.Base(n) == FixtureFile && !underAny(n, GoldenDir+"/", JSONSubsetDir+"/") {
			negatives = append(negatives, path.Dir(n))
		}
	}
	for _, n := range names {
		parts := strings.Split(n, "/")
		switch {
		case parts[0] == GoldenDir && len(parts) >= 3:
			e := entryOf(Golden, GoldenDir+"/"+parts[1])
			addFile(e, strings.Join(parts[2:], "/"), all[n], BundleDir, BundleJSONDir, FromDir, ToDir)
		case parts[0] == JSONSubsetDir && len(parts) >= 3:
			e := entryOf(JSONSubset, JSONSubsetDir+"/"+parts[1])
			addFile(e, strings.Join(parts[2:], "/"), all[n], YAMLDir, JSONDir)
		default:
			dir := deepestPrefix(negatives, n)
			if dir == "" {
				continue
			}
			rel := n
			if dir != "." {
				rel = strings.TrimPrefix(n, dir+"/")
			}
			addFile(entryOf(Negative, dir), rel, all[n], BundleDir)
		}
	}
	out := make([]Entry, 0, len(byDir))
	for _, e := range byDir {
		if e.Kind == Negative {
			e.Code = negativeCode(path.Base(e.Name), e.Fixture)
		}
		slices.SortFunc(e.Bundles, func(a, b Bundle) int { return strings.Compare(a.Name, b.Name) })
		slices.SortFunc(e.Levels, func(a, b Level) int { return a.N - b.N })
		out = append(out, *e)
	}
	slices.SortFunc(out, func(a, b Entry) int {
		if a.Kind != b.Kind {
			return int(a.Kind) - int(b.Kind)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

func underAny(name string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// deepestPrefix returns the longest directory in dirs that contains name.
func deepestPrefix(dirs []string, name string) string {
	best := ""
	for _, d := range dirs {
		if (d == "." || strings.HasPrefix(name, d+"/")) && len(d) > len(best) {
			best = d
		}
	}
	return best
}

// addFile files rel (relative to the entry directory) into e: a file of
// one of the Bundle directories bundleDirs, of a level directory of a
// golden entry, or one of the entry's own files; anything else is
// recorded as unknown.
func addFile(e *Entry, rel string, data []byte, bundleDirs ...string) {
	first, rest, nested := strings.Cut(rel, "/")
	if nested && slices.Contains(bundleDirs, first) {
		i := slices.IndexFunc(e.Bundles, func(b Bundle) bool { return b.Name == first })
		if i < 0 {
			e.Bundles = append(e.Bundles, Bundle{Name: first})
			i = len(e.Bundles) - 1
		}
		e.Bundles[i].Files = append(e.Bundles[i].Files, File{Name: rest, Data: data})
		return
	}
	if n, ok := ParseLevel(first); ok && nested && e.Kind == Golden {
		addLevelFile(e, n, rest, data)
		return
	}
	switch {
	case rel == EnvironmentsFile:
		e.Environments = data
	case rel == FixtureFile && e.Kind == Negative:
		e.Fixture = data
	case rel == ExpectedFile && e.Kind == Negative:
		e.Expected = data
	case rel == ExpectedTextFile && e.Kind == Negative:
		e.ExpectedText = data
	default:
		e.Unknown = append(e.Unknown, rel)
	}
}

// addLevelFile files rel (relative to level-<n>/) into level n of e.
func addLevelFile(e *Entry, n int, rel string, data []byte) {
	i := slices.IndexFunc(e.Levels, func(l Level) bool { return l.N == n })
	if i < 0 {
		e.Levels = append(e.Levels, Level{N: n})
		i = len(e.Levels) - 1
	}
	l := &e.Levels[i]
	l.Files = append(l.Files, File{Name: rel, Data: data})
	if rel == ExpectedFile {
		l.Expected = data
	}
	if render, ok := strings.CutPrefix(rel, CanonicalDir+"/"); ok && path.Ext(render) == ".json" && !strings.Contains(render, "/") {
		if l.Canonical == nil {
			l.Canonical = map[string][]byte{}
		}
		l.Canonical[strings.TrimSuffix(render, ".json")] = data
	}
}

// negativeCode returns the code of a negative fixture: the top-level
// "code:" line of fixture.yaml, else the code the directory name starts
// with.
func negativeCode(dir string, fixture []byte) string {
	for line := range strings.SplitSeq(string(fixture), "\n") {
		if rest, ok := strings.CutPrefix(line, "code:"); ok {
			v := strings.TrimSpace(rest)
			if i := strings.Index(v, " #"); i >= 0 {
				v = strings.TrimSpace(v[:i])
			}
			if v = strings.Trim(v, `"'`); v != "" {
				return v
			}
		}
	}
	return DirCode(dir)
}

// DirCode returns the RZ code a negative fixture's directory name starts
// with: "RZ-<AREA>-<NNN>-<slug>", in any case, or "<NNN>-<slug>" for an
// RZ-CFG code; "" for any other name.
func DirCode(dir string) string {
	upper := strings.ToUpper(dir)
	if rest, ok := strings.CutPrefix(upper, "RZ-"); ok {
		// RZ-<AREA>-<NNN>-...
		area, tail, ok := strings.Cut(rest, "-")
		if ok && len(tail) >= 3 && allDigits(tail[:3]) && area != "" && (len(tail) == 3 || tail[3] == '-') {
			return "RZ-" + area + "-" + tail[:3]
		}
		return ""
	}
	if len(upper) >= 3 && allDigits(upper[:3]) && (len(upper) == 3 || upper[3] == '-') {
		return "RZ-CFG-" + upper[:3]
	}
	return ""
}

func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// Files returns the corpus files whose names end with one of exts (for
// example ".yaml"), sorted by path; no extension returns every file.
func (c *Corpus) Files(exts ...string) []File {
	var out []File
	for _, f := range c.files {
		if len(exts) == 0 || slices.ContainsFunc(exts, func(e string) bool { return strings.HasSuffix(f.Name, e) }) {
			out = append(out, f)
		}
	}
	return out
}

// YAML returns the contents of every .yaml and .yml file (Bundle files,
// environments files, fixture metadata), sorted by path.
func (c *Corpus) YAML() [][]byte { return contents(c.Files(".yaml", ".yml")) }

// JSON returns the contents of every .json file (JSON twins, canonical
// forms, expected digests and diagnostics), sorted by path.
func (c *Corpus) JSON() [][]byte { return contents(c.Files(".json")) }

// Canonical returns every golden canonical form, sorted by entry, level
// and render.
func (c *Corpus) Canonical() [][]byte {
	var out [][]byte
	for _, e := range c.Entries {
		for _, l := range e.Levels {
			renders := make([]string, 0, len(l.Canonical))
			for r := range l.Canonical {
				renders = append(renders, r)
			}
			slices.Sort(renders)
			for _, r := range renders {
				out = append(out, l.Canonical[r])
			}
		}
	}
	return out
}

// Archives returns every Bundle of every entry as a txtar archive (file
// names relative to the Bundle root), the input format of Bundle-level
// fuzz targets.
func (c *Corpus) Archives() [][]byte {
	var out [][]byte
	for _, e := range c.Entries {
		for _, b := range e.Bundles {
			a := Archive{Comment: []byte(e.Name + "/" + b.Name + "\n"), Files: b.Files}
			out = append(out, a.Format())
		}
	}
	return out
}

func contents(files []File) [][]byte {
	out := make([][]byte, len(files))
	for i, f := range files {
		out[i] = f.Data
	}
	return out
}

// AddYAML adds every YAML corpus file as a seed of a func(*testing.T,
// []byte) target and returns the count. An absent corpus adds nothing; a
// read error fails f.
func AddYAML(f *testing.F) int { return add(f, (*Corpus).YAML) }

// AddJSON adds every JSON corpus file as a seed and returns the count.
func AddJSON(f *testing.F) int { return add(f, (*Corpus).JSON) }

// AddCanonical adds every golden canonical form as a seed and returns the
// count.
func AddCanonical(f *testing.F) int { return add(f, (*Corpus).Canonical) }

// AddArchives adds every Bundle as a txtar seed and returns the count.
func AddArchives(f *testing.F) int { return add(f, (*Corpus).Archives) }

func add(f *testing.F, seeds func(*Corpus) [][]byte) int {
	f.Helper()
	c, err := Default()
	if errors.Is(err, ErrNoRepo) {
		return 0
	}
	if err != nil {
		f.Fatal(err)
	}
	s := seeds(c)
	for _, b := range s {
		f.Add(bytes.Clone(b))
	}
	return len(s)
}
