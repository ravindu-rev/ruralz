// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// bodyOracle stands in for the CEL compiler's ReferencesRequestBody: a
// rule reads the body when it mentions request.body.
type bodyOracle struct{}

func (bodyOracle) ReferencesRequestBody(src string) (bool, error) {
	return strings.Contains(src, "request.body"), nil
}

// newObject returns a new typed object of a kind.
func newObject(k v1alpha1.Kind) (any, bool) {
	switch k {
	case v1alpha1.KindGateway:
		return new(v1alpha1.Gateway), true
	case v1alpha1.KindRoute:
		return new(v1alpha1.Route), true
	case v1alpha1.KindUpstream:
		return new(v1alpha1.Upstream), true
	case v1alpha1.KindPolicy:
		return new(v1alpha1.Policy), true
	case v1alpha1.KindPlugin:
		return new(v1alpha1.Plugin), true
	case v1alpha1.KindConsumer:
		return new(v1alpha1.Consumer), true
	case v1alpha1.KindAIProvider:
		return new(v1alpha1.AIProvider), true
	case v1alpha1.KindAIModel:
		return new(v1alpha1.AIModel), true
	default:
		return nil, false
	}
}

// loadBundle reads every .yaml file under dirs (a file of a later dir
// replaces the file of the same relative path in an earlier one), parses
// it with the restricted profile and builds the hub resources the way
// stage G does: the typed object (unknown fields rejected), the registry
// defaults materialized on a Policy and its config decoded into the
// registry's config type.
func loadBundle(t testing.TB, dirs ...string) (*hub.Bundle, *tree.FileTable) {
	t.Helper()
	files := map[string]string{}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".yaml") {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			files[filepath.ToSlash(rel)] = p
			return err
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	slices.Sort(rels)
	table := &tree.FileTable{}
	reg := registry.New()
	var rs []*hub.Resource
	for _, rel := range rels {
		src, err := os.ReadFile(files[rel])
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, parseFile(t, table, reg, rel, src)...)
	}
	return hub.NewBundle(rs), table
}

// parseFile parses one YAML file into hub resources.
func parseFile(t testing.TB, table *tree.FileTable, reg *registry.Registry, rel string, src []byte) []*hub.Resource {
	t.Helper()
	id := table.Add(tree.File{Path: rel, Role: tree.RoleBase})
	docs, ds, err := profile.Parse(t.Context(), src, id, rel, profile.FormatYAML, profile.Options{})
	if err != nil || len(ds) > 0 {
		t.Fatalf("parse %s: %v %v", rel, err, ds)
	}
	var out []*hub.Resource
	for _, doc := range docs {
		kind, _ := doc.Root.Get("kind")
		apiVersion, _ := doc.Root.Get("apiVersion")
		meta, _ := doc.Root.Get("metadata")
		name, _ := meta.Get("name")
		obj, ok := newObject(v1alpha1.Kind(kind.Text))
		if !ok {
			t.Fatalf("%s: unknown kind %q", rel, kind.Text)
		}
		raw, err := json.Marshal(doc.Root.JSONValue())
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(obj); err != nil {
			t.Fatalf("%s: decode %s/%s: %v", rel, kind.Text, name.Text, err)
		}
		r := &hub.Resource{
			ID:     hub.ID{Kind: v1alpha1.Kind(kind.Text), Name: name.Text},
			Tree:   doc.Root,
			Object: obj,
			Source: hub.Source{File: rel, APIVersion: apiVersion.Text, Start: table.Location(doc.Start)},
		}
		if p, ok := obj.(*v1alpha1.Policy); ok {
			reg.Materialize(p)
			r.Config = decodeConfig(t, reg, p)
		}
		out = append(out, r)
	}
	return out
}

// decodeConfig decodes spec.config into the registry's config type.
func decodeConfig(t testing.TB, reg *registry.Registry, p *v1alpha1.Policy) any {
	t.Helper()
	cfg, err := reg.NewConfig(p.Spec.Type)
	if err != nil {
		t.Fatalf("Policy %s: %v", p.Metadata.Name, err)
	}
	if cfg != nil && len(p.Spec.Config) > 0 {
		if err := json.Unmarshal(p.Spec.Config, cfg); err != nil {
			t.Fatalf("Policy %s config: %v", p.Metadata.Name, err)
		}
	}
	return cfg
}

// stageI runs Run with the test oracle and the file table.
func stageI(t testing.TB, b *hub.Bundle, files *tree.FileTable, workers int) (map[string]*hub.Chain, diag.List) {
	t.Helper()
	chains, ds, err := New(registry.New(), Options{Body: bodyOracle{}, Files: files}).Run(t.Context(), b, workers)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return chains, ds
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

// table writes rows the way render --effective does (02 req 51):
// text/tabwriter with minwidth 0, tabwidth 8, padding 2, space padding.
func table(t testing.TB, rows []Row) string {
	t.Helper()
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "PHASE\tLEG\tPOLICY\tFROM\tREASON")
	for _, r := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.Phase, r.Leg, r.Policy, r.From, r.Reason)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// rowsJSON returns rows as indented JSON without HTML escaping.
func rowsJSON(t testing.TB, rows []Row) string {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rows); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// golden compares got with the file at path, or rewrites it with -update.
func golden(t *testing.T, path, got string) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: golden paths under testdata.
	if err != nil {
		t.Fatalf("read golden (run with -update): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got\n%s--- want\n%s", path, got, want)
	}
}
