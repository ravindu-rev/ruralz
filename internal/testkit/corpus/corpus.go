// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package corpus loads the configuration conformance corpora under
// test/conformance/config as fuzz seeds (11 req 27, TQ "Fuzzing": "seeded
// from the golden and negative corpora"). Every fuzz target that parses
// configuration seeds itself through this one helper:
//
//	func FuzzProfileYAML(f *testing.F) {
//		corpus.AddYAML(f)
//		f.Fuzz(func(t *testing.T, data []byte) { ... })
//	}
//
// The layout follows the configuration specs: golden entries under
// golden/<entry>/ (bundle/, environments.yaml, expected.json,
// canonical/<env>.json, ...), JSON subset pairs under
// json-subset/<case>/{yaml,json}/, and negative fixtures under
// <code>-<slug>/ (fixture.yaml, bundle/, optional environments.yaml).
// Every part is optional: an absent directory yields no entries and no
// error, so targets run before the corpus exists. Only regular files are
// read (symbolic links, FIFOs and other special files that negative
// fixtures create are skipped), and files above MaxSeedBytes are left out.
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
	// Bundle files) or to its entry directory.
	Name string
	Data []byte
}

// Bundle is one Bundle directory of an entry.
type Bundle struct {
	// Name is the directory name: "bundle", or "yaml" and "json" for a
	// JSON subset pair.
	Name  string
	Files []File
}

// Entry is one golden entry, JSON subset pair or negative fixture.
type Entry struct {
	Kind Kind
	// Name is the entry path relative to the corpus directory, for example
	// "golden/minimal" or "RZ-CFG-005-missing-spec".
	Name string
	// Code is the RZ code of a negative fixture: the "code:" line of
	// fixture.yaml when present, else the code the directory name starts
	// with ("RZ-CFG-005-..." or "005-..."); empty otherwise.
	Code    string
	Bundles []Bundle
	// Environments is environments.yaml when present.
	Environments []byte
	// Fixture is fixture.yaml of a negative fixture.
	Fixture []byte
	// Expected is expected.json when present.
	Expected []byte
	// Canonical holds golden canonical/<env>.json by <env>.
	Canonical map[string][]byte
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
		if path.Base(n) == "fixture.yaml" && !underAny(n, "golden/", "json-subset/") {
			negatives = append(negatives, path.Dir(n))
		}
	}
	for _, n := range names {
		parts := strings.Split(n, "/")
		switch {
		case parts[0] == "golden" && len(parts) >= 3:
			e := entryOf(Golden, "golden/"+parts[1])
			addFile(e, strings.Join(parts[2:], "/"), all[n], "bundle")
		case parts[0] == "json-subset" && len(parts) >= 3:
			e := entryOf(JSONSubset, "json-subset/"+parts[1])
			addFile(e, strings.Join(parts[2:], "/"), all[n], "yaml", "json")
		default:
			dir := deepestPrefix(negatives, n)
			if dir == "" {
				continue
			}
			rel := n
			if dir != "." {
				rel = strings.TrimPrefix(n, dir+"/")
			}
			addFile(entryOf(Negative, dir), rel, all[n], "bundle")
		}
	}
	out := make([]Entry, 0, len(byDir))
	for _, e := range byDir {
		if e.Kind == Negative {
			e.Code = negativeCode(path.Base(e.Name), e.Fixture)
		}
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

// addFile files rel (relative to the entry directory) into e.
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
	switch {
	case rel == "environments.yaml":
		e.Environments = data
	case rel == "fixture.yaml":
		e.Fixture = data
	case rel == "expected.json":
		e.Expected = data
	case first == "canonical" && nested && path.Ext(rest) == ".json" && !strings.Contains(rest, "/"):
		if e.Canonical == nil {
			e.Canonical = map[string][]byte{}
		}
		e.Canonical[strings.TrimSuffix(rest, ".json")] = data
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
	upper := strings.ToUpper(dir)
	if rest, ok := strings.CutPrefix(upper, "RZ-"); ok {
		// RZ-<AREA>-<NNN>-...
		area, tail, ok := strings.Cut(rest, "-")
		if ok && len(tail) >= 3 && allDigits(tail[:3]) && area != "" {
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

// Canonical returns every golden canonical form, sorted by entry and
// Environment.
func (c *Corpus) Canonical() [][]byte {
	var out [][]byte
	for _, e := range c.Entries {
		envs := make([]string, 0, len(e.Canonical))
		for env := range e.Canonical {
			envs = append(envs, env)
		}
		slices.Sort(envs)
		for _, env := range envs {
			out = append(out, e.Canonical[env])
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
