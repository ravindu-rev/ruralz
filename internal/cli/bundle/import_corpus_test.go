// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// corpusRoot is the vendored SM-8 OpenAPI corpus
// (docs/vision/01-vision-and-positioning.md SM-8), read in place.
const corpusRoot = "../../../test/fixtures/openapi-corpus"

// corpusDocument returns the root document of the corpus entry dir: the
// document line of its SOURCE, under data/.
func corpusDocument(t *testing.T, dir string) string {
	t.Helper()
	src := readFile(t, filepath.Join(dir, "SOURCE"))
	for line := range strings.SplitSeq(src, "\n") {
		if doc, ok := strings.CutPrefix(line, "document: "); ok {
			return filepath.Join(dir, "data", filepath.FromSlash(strings.TrimSpace(doc)))
		}
	}
	t.Fatalf("%s/SOURCE names no document", dir)
	return ""
}

// corpusLicenses are the licenses a corpus document may carry, those that
// allow its redistribution in the repository.
func corpusLicenses() []string {
	return []string{"Apache-2.0", "MIT", "BSD-2-Clause", "BSD-3-Clause", "ISC", "CC0-1.0"}
}

// sha256Of returns the hexadecimal SHA-256 of the file at path.
func sha256Of(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(readFile(t, path)))
	return hex.EncodeToString(sum[:])
}

// TestCorpusProvenance: every corpus entry is pinned and holds exactly
// what its SOURCE names: a GitHub repository at a full commit, a root
// document among its files, every file under data/ with its SHA-256 and
// no other, a LICENSE of a license that allows redistribution with its
// SHA-256, and a NOTICE with its SHA-256 exactly when SOURCE names one.
func TestCorpusProvenance(t *testing.T) {
	entries, err := os.ReadDir(corpusRoot)
	if err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	commit := regexp.MustCompile(`^[0-9a-f]{40}$`)
	license := regexp.MustCompile(`^(\S+) \((\S+)\)$`)
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("%s is no corpus entry directory", e.Name())
			continue
		}
		n++
		dir := filepath.Join(corpusRoot, e.Name())
		fields := map[string]string{}
		files := map[string]string{}
		for line := range strings.SplitSeq(strings.TrimSuffix(readFile(t, filepath.Join(dir, "SOURCE")), "\n"), "\n") {
			key, value, ok := strings.Cut(line, ": ")
			if !ok {
				t.Errorf("%s/SOURCE: line %q is no key: value", e.Name(), line)
				continue
			}
			if key == "file" {
				p, sum, ok := strings.Cut(value, " sha256 ")
				if !ok {
					t.Errorf("%s/SOURCE: file line %q has no sha256", e.Name(), value)
				}
				files[p] = sum
				continue
			}
			fields[key] = value
		}
		want := []string{"name", "url", "commit", "base", "document", "license", "license-sha256", "updated-by"}
		for _, k := range want {
			if fields[k] == "" {
				t.Errorf("%s/SOURCE lacks %s", e.Name(), k)
			}
		}
		m := license.FindStringSubmatch(fields["license"])
		switch {
		case !name.MatchString(e.Name()):
			t.Errorf("entry name %q is not lower-case words joined by hyphens", e.Name())
		case !strings.HasPrefix(fields["url"], "https://github.com/"):
			t.Errorf("%s: url %q is no GitHub repository", e.Name(), fields["url"])
		case !commit.MatchString(fields["commit"]):
			t.Errorf("%s: commit %q is no full commit SHA", e.Name(), fields["commit"])
		case m == nil || !slices.Contains(corpusLicenses(), m[1]):
			t.Errorf("%s: license %q is not one of %q", e.Name(), fields["license"], corpusLicenses())
		case files[fields["document"]] == "":
			t.Errorf("%s: document %q is none of its files", e.Name(), fields["document"])
		}
		if got := sha256Of(t, filepath.Join(dir, "LICENSE")); got != fields["license-sha256"] {
			t.Errorf("%s: LICENSE SHA-256 %s, SOURCE pins %s", e.Name(), got, fields["license-sha256"])
		}
		top := []string{"LICENSE", "SOURCE", "data"}
		if fields["notice"] != "" {
			top = append(top, "NOTICE")
			if got := sha256Of(t, filepath.Join(dir, "NOTICE")); got != fields["notice-sha256"] {
				t.Errorf("%s: NOTICE SHA-256 %s, SOURCE pins %s", e.Name(), got, fields["notice-sha256"])
			}
		}
		slices.Sort(top)
		var held []string
		for _, f := range mustReadDir(t, dir) {
			held = append(held, f.Name())
		}
		if !slices.Equal(held, top) {
			t.Errorf("%s holds %q, want %q", e.Name(), held, top)
		}
		found := map[string]bool{}
		err := filepath.WalkDir(filepath.Join(dir, "data"), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(filepath.Join(dir, "data"), p)
			rel = filepath.ToSlash(rel)
			found[rel] = true
			if sum, ok := files[rel]; !ok {
				t.Errorf("%s: data/%s is not in SOURCE", e.Name(), rel)
			} else if got := sha256Of(t, p); got != sum {
				t.Errorf("%s: data/%s SHA-256 %s, SOURCE pins %s", e.Name(), rel, got, sum)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for p := range files {
			if !found[p] {
				t.Errorf("%s: SOURCE names data/%s, which is missing", e.Name(), p)
			}
		}
	}
	if n == 0 {
		t.Fatal("the corpus holds no entry")
	}
}

// mustReadDir returns the entries of dir, sorted by name.
func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	out, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// corpusOutcome is what the SM-8 measurement found for one document.
type corpusOutcome struct {
	// line is the document's line of the golden file.
	line string
	// clean reports a document imported without manual edits.
	clean bool
}

// TestImportCorpus is the SM-8 measurement
// (docs/vision/01-vision-and-positioning.md "Success metrics"): every
// document of the corpus is imported into a new directory, which ruralz
// bundle validate and ruralz bundle render must then accept unchanged. A
// document counts as imported without manual edits when the import exits
// 0 with every operation a Route and both commands exit 0. The golden file
// pins each document's outcome and the share of documents imported
// without manual edits; the documents run in parallel.
func TestImportCorpus(t *testing.T) {
	entries, err := os.ReadDir(corpusRoot)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	outcomes := make([]corpusOutcome, len(names))
	t.Run("documents", func(t *testing.T) {
		for i, name := range names {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				outcomes[i] = importCorpusDocument(t, name)
			})
		}
	})
	var b strings.Builder
	clean := 0
	for _, o := range outcomes {
		b.WriteString(o.line + "\n")
		if o.clean {
			clean++
		}
	}
	fmt.Fprintf(&b, "imported without manual edits: %d of %d documents (%.1f%%)\n", clean, len(names), 100*float64(clean)/float64(len(names)))
	t.Logf("SM-8: %d of %d documents imported without manual edits", clean, len(names))
	golden(t, "import/corpus.txt", b.String())
}

// importCorpusDocument imports the corpus entry name and checks the
// Bundle as TestImportCorpus describes. Paths in its line are relative to
// the entry.
func importCorpusDocument(t *testing.T, name string) corpusOutcome {
	t.Helper()
	dir := filepath.Join(corpusRoot, name)
	doc := corpusDocument(t, dir)
	out := filepath.Join(t.TempDir(), "bundle")
	r := run(t, "bundle", "import", "openapi", doc, "--output-dir", out)
	if r.code != command.ExitOK {
		first := strings.TrimPrefix(lines(r.stderr)[0], filepath.ToSlash(dir)+"/")
		return corpusOutcome{line: fmt.Sprintf("%s: not imported, exit %d: %s", name, r.code, first)}
	}
	routes, _ := os.ReadDir(filepath.Join(out, "routes"))
	skipped := strings.Count(r.stderr, " is not imported: ")
	validate := run(t, "bundle", "validate", out)
	render := run(t, "bundle", "render", out)
	o := corpusOutcome{}
	status := "clean"
	switch {
	case validate.code != command.ExitOK:
		status = fmt.Sprintf("validate exit %d: %s", validate.code, lines(validate.stdout)[0])
	case render.code != command.ExitOK:
		status = fmt.Sprintf("render exit %d: %s", render.code, lines(render.stderr)[0])
	case skipped > 0:
		status = "operations left out"
	default:
		o.clean = true
	}
	o.line = fmt.Sprintf("%s: %d of %d operations imported, %s", name, len(routes), len(routes)+skipped, status)
	return o
}
