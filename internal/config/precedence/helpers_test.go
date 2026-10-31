// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
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

// layer holds the YAML files of a Bundle by slash path relative to its
// root.
type layer map[string][]byte

// dirLayer reads every .yaml file under dir.
func dirLayer(t testing.TB, dir string) layer {
	t.Helper()
	l := layer{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".yaml") {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		src, err := os.ReadFile(p) //nolint:gosec // G304: a file under testdata.
		l[filepath.ToSlash(rel)] = src
		return err
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return l
}

// goldenCorpus loads the configuration conformance corpus once for all
// tests: the one golden corpus of pipeline outputs, which holds the
// example Bundle and its effective tables
// (docs/engineering/03-testing-and-quality-strategy.md "Golden tests").
var goldenCorpus = sync.OnceValues(corpus.Default)

// shopEntry is the corpus entry of the configuration model's example
// Bundle ("Complete annotated example Bundle"), examples/shop-bundle.
const shopEntry = "golden/shop-bundle"

// shopIgnored lists the files of the entry's bundle/ that its .ruralzignore
// keeps out of every render.
func shopIgnored() []string {
	return []string{"routes/orders-history.draft.yaml", "samples/orders-summary.json"}
}

// shopCorpusEntry returns the corpus entry of the example Bundle.
func shopCorpusEntry(t testing.TB) *corpus.Entry {
	t.Helper()
	c, err := goldenCorpus()
	if err != nil {
		t.Fatalf("load the golden corpus: %v", err)
	}
	e, ok := c.Entry(shopEntry)
	if !ok {
		t.Fatalf("the golden corpus has no entry %s", shopEntry)
	}
	return e
}

// shopLayer returns the base files of the example Bundle in the golden
// corpus: the YAML files of its bundle/ outside overlays/, which only a
// render for an Environment reads, less those of shopIgnored. Discovery is
// stage A, which internal/config/loader runs a layer above this package
// (docs/engineering/02-repository-layout-and-conventions.md "Package
// layers"), so the test names the files a render leaves out, and
// TestShopBaseFiles holds the rest to the resources of the entry's render
// without --env.
func shopLayer(t testing.TB) layer {
	t.Helper()
	b, ok := shopCorpusEntry(t).Bundle(corpus.BundleDir)
	if !ok {
		t.Fatalf("%s has no %s/", shopEntry, corpus.BundleDir)
	}
	ignored := shopIgnored()
	for _, name := range ignored {
		if _, ok := b.File(name); !ok {
			t.Fatalf("%s has no file %s to leave out", shopEntry, name)
		}
	}
	l := layer{}
	for _, f := range b.Files {
		if strings.HasSuffix(f.Name, ".yaml") && !strings.HasPrefix(f.Name, "overlays/") && !slices.Contains(ignored, f.Name) {
			l[f.Name] = f.Data
		}
	}
	return l
}

// shopLevel returns the outputs of the example Bundle at the hub schema's
// level, the level this release renders.
func shopLevel(t testing.TB) *corpus.Level {
	t.Helper()
	set, err := schemaidx.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	idx, ok := set.Index(v1alpha1.APIVersion)
	if !ok {
		t.Fatalf("no %s schema", v1alpha1.APIVersion)
	}
	l, ok := shopCorpusEntry(t).Level(idx.Level())
	if !ok {
		t.Fatalf("%s has no %s", shopEntry, corpus.LevelDir(idx.Level()))
	}
	return l
}

// edited returns a copy of l whose file has the text old, which it must
// hold once, replaced by repl.
func edited(t testing.TB, l layer, file, old, repl string) layer {
	t.Helper()
	src, ok := l[file]
	if !ok || bytes.Count(src, []byte(old)) != 1 {
		t.Fatalf("%s does not hold %q once", file, old)
	}
	out := maps.Clone(l)
	out[file] = bytes.Replace(src, []byte(old), []byte(repl), 1)
	return out
}

// loadLayers parses the files of layers (a file of a later layer replaces
// the file of the same path in an earlier one) with the restricted
// profile, in path order, and builds the hub resources the way stage G
// does: the typed object (unknown fields rejected), the registry defaults
// materialized on a Policy and its config decoded into the registry's
// config type.
func loadLayers(t testing.TB, layers ...layer) (*hub.Bundle, *tree.FileTable) {
	t.Helper()
	files := layer{}
	for _, l := range layers {
		maps.Copy(files, l)
	}
	table := &tree.FileTable{}
	reg := registry.New()
	var rs []*hub.Resource
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		rs = append(rs, parseFile(t, table, reg, rel, files[rel])...)
	}
	return hub.NewBundle(rs), table
}

// loadBundle loads the .yaml files under dirs (loadLayers).
func loadBundle(t testing.TB, dirs ...string) (*hub.Bundle, *tree.FileTable) {
	t.Helper()
	layers := make([]layer, len(dirs))
	for i, dir := range dirs {
		layers[i] = dirLayer(t, dir)
	}
	return loadLayers(t, layers...)
}

// loadShop loads the base files of the example Bundle (shopLayer).
func loadShop(t testing.TB) (*hub.Bundle, *tree.FileTable) {
	t.Helper()
	return loadLayers(t, shopLayer(t))
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
	chains, ds, err := mustNew(t, registry.New(), Options{Body: bodyOracle{}}).Run(t.Context(), b, files, workers)
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

// table writes rows the way render --effective does: text/tabwriter
// with minwidth 0, tabwidth 8, padding 2, space padding.
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

// mustNew returns New(reg, o), failing tb when New fails.
func mustNew(tb testing.TB, reg *registry.Registry, o Options) *Resolver {
	tb.Helper()
	r, err := New(reg, o)
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	return r
}
