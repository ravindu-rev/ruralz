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

	"github.com/ravindu-rev/ruralz/internal/config/defaults"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the want files under testdata")

// stages holds the schema indexes, the compiled schema views and stage G,
// built once for the package's tests.
type stages struct {
	idx   *schemaidx.Set
	views *schemaview.Set
	g     *defaults.Stage
	reg   *registry.Registry
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
		stagesVal = stages{idx: idx, views: views, g: g, reg: reg}
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
	s := pipeline(t)
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
			res := &tree.Resource{ID: tree.ID{Kind: v1alpha1.Kind(k.Text), Name: n.Text}, APIVersion: av.Text, Root: d.Root, Start: d.Start}
			if ds, ok := s.views.Validate(res, table); !ok || len(ds) > 0 {
				t.Fatalf("stage F on %s: %s", res.ID, diagText(t, ds))
			}
			rs = append(rs, res)
		}
	}
	b, ds, err := s.g.Run(t.Context(), rs, table, 2)
	if err != nil || len(ds) > 0 || b == nil {
		t.Fatalf("stage G: %v\n%s", err, diagText(t, ds))
	}
	return b, table
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
