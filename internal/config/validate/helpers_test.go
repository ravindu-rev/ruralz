// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"bytes"
	"flag"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/config/defaults"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the want files under testdata")

// stages holds the schema indexes, the compiled schema views, the loader
// over them and stage G, built once for the package's tests.
type stages struct {
	idx    *schemaidx.Set
	views  *schemaview.Set
	g      *defaults.Stage
	reg    *registry.Registry
	loader *loader.Loader
}

var (
	stagesOnce sync.Once
	stagesVal  stages
	stagesErr  error
)

// pipeline returns the shared stages.
func pipeline(t testing.TB) stages {
	t.Helper()
	stagesOnce.Do(func() {
		idx, err := schemaidx.Embedded()
		if err != nil {
			stagesErr = err
			return
		}
		views, err := schemaview.Embedded(idx)
		if err != nil {
			stagesErr = err
			return
		}
		reg := registry.New()
		g, err := defaults.New(defaults.Options{Schemas: idx, Registry: reg})
		if err != nil {
			stagesErr = err
			return
		}
		ld, err := loader.New(loader.Options{Schemas: idx, Views: views})
		if err != nil {
			stagesErr = err
			return
		}
		stagesVal = stages{idx: idx, views: views, g: g, reg: reg, loader: ld}
	})
	if stagesErr != nil {
		t.Fatal(stagesErr)
	}
	return stagesVal
}

// newValidator returns a Validator over the shared stages; edit adjusts
// the options first.
func newValidator(t testing.TB, edit func(*Options)) *Validator {
	t.Helper()
	s := pipeline(t)
	o := Options{Schemas: s.idx, Registry: s.reg}
	if edit != nil {
		edit(&o)
	}
	v, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v
}

// readDirs reads every .yaml file under dirs into a map from slash path
// to content; a file of a later dir replaces the same path of an earlier
// one, and an empty file deletes it.
func readDirs(t testing.TB, dirs ...string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".yaml") {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			src, err := os.ReadFile(p) //nolint:gosec // G304: a file under testdata.
			if err != nil {
				return err
			}
			if len(bytes.TrimSpace(src)) == 0 {
				delete(files, filepath.ToSlash(rel))
				return nil
			}
			files[filepath.ToSlash(rel)] = string(src)
			return nil
		})
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
	}
	return files
}

// loadFiles runs the profile parser (stage B), the rendered schema (stage
// F) and stage G on files, a map from slash path to YAML, in path order,
// and returns the hub.Bundle and its file table. A fixture must pass
// stages B to G: their findings fail the test.
func loadFiles(t testing.TB, files map[string]string) (*hub.Bundle, *tree.FileTable) {
	t.Helper()
	paths := slices.Sorted(maps.Keys(files))
	table := &tree.FileTable{}
	var rs []*tree.Resource
	for _, rel := range paths {
		id := table.Add(tree.File{Path: rel, Role: tree.RoleBase})
		docs, ds, err := profile.Parse(t.Context(), []byte(files[rel]), id, rel, profile.FormatYAML, profile.Options{})
		if err != nil || len(ds) > 0 {
			t.Fatalf("parse %s: %v\n%s", rel, err, diagText(t, ds))
		}
		for _, d := range docs {
			k, _ := d.Root.Get("kind")
			meta, _ := d.Root.Get("metadata")
			n, _ := meta.Get("name")
			av, _ := d.Root.Get("apiVersion")
			rs = append(rs, &tree.Resource{ID: tree.ID{Kind: v1alpha1.Kind(k.Text), Name: n.Text}, APIVersion: av.Text, Root: d.Root, Start: d.Start})
		}
	}
	return stagesFG(t, rs, table), table
}

// stagesFG runs the rendered schema (stage F) on each resource, then stage
// G, failing on any finding.
func stagesFG(t testing.TB, rs []*tree.Resource, table *tree.FileTable) *hub.Bundle {
	t.Helper()
	s := pipeline(t)
	for _, res := range rs {
		if ds, ok := s.views.Validate(res, table); !ok || len(ds) > 0 {
			t.Fatalf("stage F on %s: %s", res.ID, diagText(t, ds))
		}
	}
	b, ds, err := s.g.Run(t.Context(), rs, table, 2)
	if err != nil || len(ds) > 0 || b == nil {
		t.Fatalf("stage G: %v\n%s", err, diagText(t, ds))
	}
	return b
}

// goldenCorpus loads the configuration conformance corpus once for all
// tests: the one golden corpus of pipeline outputs, which holds the
// example Bundle (docs/engineering/03-testing-and-quality-strategy.md
// "Golden tests").
var goldenCorpus = sync.OnceValues(corpus.Default)

// shopEntry is the corpus entry of the Configuration model's example
// Bundle, examples/shop-bundle.
const shopEntry = "golden/shop-bundle"

// shopEdit changes the example Bundle before a test loads it: in file,
// the text old, which the file holds once, becomes repl.
type shopEdit struct{ file, old, repl string }

// loadShop loads the example Bundle of the golden corpus as its render
// without --env does: stages A to E through the loader, over the entry's
// files with edits applied and an empty variable set, then stages F and G
// (stagesFG). Every finding fails t.
func loadShop(t testing.TB, edits ...shopEdit) (*hub.Bundle, *tree.FileTable) {
	t.Helper()
	c, err := goldenCorpus()
	if err != nil {
		t.Fatalf("load the golden corpus: %v", err)
	}
	e, ok := c.Entry(shopEntry)
	if !ok {
		t.Fatalf("the golden corpus has no entry %s", shopEntry)
	}
	b, ok := e.Bundle(corpus.BundleDir)
	if !ok {
		t.Fatalf("%s has no %s/", shopEntry, corpus.BundleDir)
	}
	fsys := fstest.MapFS{}
	for _, f := range b.Files {
		fsys[f.Name] = &fstest.MapFile{Data: bytes.Clone(f.Data)}
	}
	for _, ed := range edits {
		f, ok := fsys[ed.file]
		if !ok || bytes.Count(f.Data, []byte(ed.old)) != 1 {
			t.Fatalf("%s/%s does not hold %q once", shopEntry, ed.file, ed.old)
		}
		f.Data = bytes.Replace(f.Data, []byte(ed.old), []byte(ed.repl), 1)
	}
	res, err := pipeline(t).loader.Load(t.Context(), loader.Request{Source: loader.Source{FS: fsys, Path: "."}, Variables: subst.Map{}})
	if err != nil {
		t.Fatalf("load %s: %v", shopEntry, err)
	}
	if len(res.Diagnostics) > 0 || res.Stopped {
		t.Fatalf("stages A to E on %s:\n%s", shopEntry, diagText(t, res.Diagnostics))
	}
	return stagesFG(t, res.Resources, res.Files), res.Files
}

// loadDirs loads the .yaml files of dirs (see readDirs).
func loadDirs(t testing.TB, dirs ...string) (*hub.Bundle, *tree.FileTable) {
	t.Helper()
	return loadFiles(t, readDirs(t, dirs...))
}

// stageH runs v on b with the file table and workers, failing on an error.
func stageH(t testing.TB, v *Validator, b *hub.Bundle, files *tree.FileTable, workers int) (Result, diag.List) {
	t.Helper()
	res, ds, err := v.Run(t.Context(), b, files, workers)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res, ds
}

// diagText returns the text form of ds.
func diagText(t testing.TB, ds diag.List) string {
	t.Helper()
	var buf bytes.Buffer
	if err := diag.WriteText(&buf, ds); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// golden compares got with the file want, or rewrites it under -update.
func golden(t *testing.T, want, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(want, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(want) //nolint:gosec // G304: a file under testdata.
	if err != nil {
		t.Fatalf("read %s (run with -update to create it): %v", want, err)
	}
	if string(b) != got {
		t.Errorf("%s differs:\n--- got\n%s--- want\n%s", want, got, b)
	}
}
