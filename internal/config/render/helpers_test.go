// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/defaults"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/overlay"
	"github.com/ravindu-rev/ruralz/internal/config/precedence"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// goldenCorpus loads the configuration conformance corpus once for all
// tests: the one golden corpus of pipeline outputs, which holds the
// example Bundle, its Environments and the rendered Bundles and effective
// tables it renders to (docs/engineering/03-testing-and-quality-strategy.md
// "Golden tests").
var goldenCorpus = sync.OnceValues(corpus.Default)

// shopEntry is the corpus entry of the example Bundle of
// docs/architecture/02-configuration-model.md ("Complete annotated example
// Bundle"), examples/shop-bundle: its Bundle with the overlays, and the
// Environments of its control/environments.yaml.
const shopEntry = "golden/shop-bundle"

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

// shopDir returns the operating system path of the example Bundle's
// directory in the golden corpus.
func shopDir(t testing.TB) string {
	t.Helper()
	c, err := goldenCorpus()
	if err != nil {
		t.Fatalf("load the golden corpus: %v", err)
	}
	return c.Path(shopEntry + "/" + corpus.BundleDir)
}

// shopLevel returns the outputs of the example Bundle at the hub schema's
// level, the level this release renders.
func shopLevel(t testing.TB) *corpus.Level {
	t.Helper()
	n := mustStages(t).hub.Level()
	l, ok := shopCorpusEntry(t).Level(n)
	if !ok {
		t.Fatalf("%s has no %s", shopEntry, corpus.LevelDir(n))
	}
	return l
}

// renderName names a render of the example Bundle as the golden corpus
// does: "-" without an Environment, else the Environment.
func renderName(env string) string {
	if env == "" {
		return "-"
	}
	return env
}

// sameAsCorpus compares got with the file name of the example Bundle's
// outputs in the golden corpus.
func sameAsCorpus(t *testing.T, name string, got []byte) {
	t.Helper()
	want, ok := shopLevel(t).File(name)
	if !ok {
		t.Fatalf("%s has no output %s", shopEntry, name)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden corpus\n--- got\n%s--- want\n%s", name, got, want)
	}
}

// schemas holds the embedded schema indexes, compiled rendered views and
// the loader over them, built once for all tests.
var schemas = sync.OnceValues(func() (*stages, error) {
	idx, err := schemaidx.Embedded()
	if err != nil {
		return nil, err
	}
	views, err := schemaview.Embedded(idx)
	if err != nil {
		return nil, err
	}
	hubIdx, ok := idx.Index(v1alpha1.APIVersion)
	if !ok {
		return nil, errors.New("no ruralz/v1alpha1 index")
	}
	g, err := defaults.New(defaults.Options{Schemas: idx, Registry: registry.New()})
	if err != nil {
		return nil, err
	}
	r, err := precedence.New(registry.New(), precedence.Options{Body: bodyOracle{}})
	if err != nil {
		return nil, err
	}
	ld, err := loader.New(loader.Options{Schemas: idx, Views: views})
	if err != nil {
		return nil, err
	}
	return &stages{idx: idx, views: views, hub: hubIdx, g: g, resolver: r, loader: ld}, nil
})

// stages are the process-wide inputs of the test pipeline.
type stages struct {
	idx      *schemaidx.Set
	views    *schemaview.Set
	hub      *schemaidx.Index
	g        *defaults.Stage
	resolver *precedence.Resolver
	// loader discovers a Bundle's files as stage A does.
	loader *loader.Loader
}

// mustStages returns the shared stages.
func mustStages(t testing.TB) *stages {
	t.Helper()
	s, err := schemas()
	if err != nil {
		t.Fatalf("schemas: %v", err)
	}
	return s
}

// bodyOracle stands in for the CEL compiler's ReferencesRequestBody, as
// the precedence tests do.
type bodyOracle struct{}

func (bodyOracle) ReferencesRequestBody(src string) (bool, error) {
	return strings.Contains(src, "request.body"), nil
}

// source is one file handed to the test pipeline.
type source struct {
	path string
	data []byte
}

// run is the output of the test pipeline for one Environment.
type run struct {
	// files is the load's file table.
	files *tree.FileTable
	// rendered holds copies of the stage E trees, taken after stage F
	// and before stage G: the input of Documents and WriteBundle.
	rendered []*tree.Resource
	// validated is the pipeline output.
	validated *hub.Validated
	// diags holds every finding.
	diags diag.List
}

// discover runs stage A over the Bundle at dir through the loader: the
// configuration files of the Bundle, the base files and those of every
// overlay, in lexical order of their slash-separated paths, leaving out
// what a .ruralzignore excludes, on ctx. The Bundle's findings fail t.
func discover(ctx context.Context, t testing.TB, dir string) (*loader.Bundle, fs.FS) {
	t.Helper()
	fsys := os.DirFS(dir)
	b, err := mustStages(t).loader.Parse(ctx, loader.Source{FS: fsys, Path: "."})
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if ds := b.Diagnostics(); len(ds) > 0 {
		t.Fatalf("read %s:\n%s", dir, diagText(t, ds))
	}
	return b, fsys
}

// readBundle returns the base files of the Bundle at dir and the files of
// overlays/<overlayName>/ ("" for none), each in lexical order of its
// slash-separated path, as the loader discovers them (discover) on ctx.
func readBundle(ctx context.Context, t testing.TB, dir, overlayName string) (base, patches []source) {
	t.Helper()
	b, fsys := discover(ctx, t, dir)
	for _, f := range b.Files() {
		if f.Overlay != "" && f.Overlay != overlayName {
			continue
		}
		data, err := fs.ReadFile(fsys, f.Path)
		if err != nil {
			t.Fatalf("read %s: %v", f.Path, err)
		}
		if f.Overlay == "" {
			base = append(base, source{f.Path, data})
		} else {
			patches = append(patches, source{f.Path, data})
		}
	}
	return base, patches
}

// shopEnvironments parses the --environments file of the example Bundle,
// the corpus entry's environments.yaml, and returns its Environments in
// file order.
func shopEnvironments(t testing.TB) []*tree.Node {
	t.Helper()
	data := shopCorpusEntry(t).Environments
	if data == nil {
		t.Fatalf("%s has no %s", shopEntry, corpus.EnvironmentsFile)
	}
	docs, ds, err := profile.Parse(t.Context(), data, 0, corpus.EnvironmentsFile, profile.FormatYAML, profile.Options{})
	if err != nil || len(ds) > 0 {
		t.Fatalf("parse %s: %v %v", corpus.EnvironmentsFile, ds, err)
	}
	out := make([]*tree.Node, len(docs))
	for i, d := range docs {
		out[i] = d.Root
	}
	return out
}

// shopRenders returns the Environments of the example Bundle's
// --environments file in file order, after "", the render without one: the
// renders the golden corpus pins.
func shopRenders(t testing.TB) []string {
	t.Helper()
	out := []string{""}
	for _, env := range shopEnvironments(t) {
		out = append(out, text(env, "metadata", "name"))
	}
	return out
}

// environment returns the spec.variables of Environment name in the
// example Bundle's --environments file.
func environment(t testing.TB, name string) subst.Map {
	t.Helper()
	for _, env := range shopEnvironments(t) {
		if text(env, "metadata", "name") != name {
			continue
		}
		vars := subst.Map{}
		spec, _ := env.Get("spec")
		if v, ok := spec.Get("variables"); ok {
			for _, m := range v.Members {
				vars[m.Key] = m.Value.Text
			}
		}
		return vars
	}
	t.Fatalf("no Environment %q in %s/%s", name, shopEntry, corpus.EnvironmentsFile)
	return nil
}

// parse parses one file into resources, registering it in files.
func parse(ctx context.Context, t testing.TB, files *tree.FileTable, src source, role tree.Role) []*tree.Resource {
	t.Helper()
	f, ok := profile.FormatOf(src.path)
	if !ok {
		t.Fatalf("%s: not a configuration file", src.path)
	}
	id := files.Add(tree.File{Path: src.path, Role: role})
	docs, ds, err := profile.Parse(ctx, src.data, id, src.path, f, profile.Options{})
	if err != nil || len(ds) > 0 {
		t.Fatalf("parse %s: %v %v", src.path, ds, err)
	}
	rs := make([]*tree.Resource, 0, len(docs))
	for _, d := range docs {
		rs = append(rs, &tree.Resource{
			ID:         tree.ID{Kind: v1alpha1.Kind(text(d.Root, "kind")), Name: text(d.Root, "metadata", "name")},
			APIVersion: text(d.Root, "apiVersion"),
			Root:       d.Root,
			Start:      d.Start,
		})
	}
	return rs
}

// text returns the string at a member path of n, or "".
func text(n *tree.Node, keys ...string) string {
	for _, k := range keys {
		var ok bool
		if n, ok = n.Get(k); !ok || n == nil {
			return ""
		}
	}
	return n.Text
}

// pipeline runs the configuration pipeline over base and patches with the
// variables vars, as the pipeline facade does: parse (B), overlay merge
// (D), substitution (E), the rendered schema view (F), copies of the stage
// E trees for render, defaults and the hub (G), effective chains (I) and
// the canonical form and digest (L, M). It stops after the first stage
// with an error diagnostic, returning what it has.
func pipeline(t testing.TB, base, patches []source, vars subst.Source) *run {
	t.Helper()
	return pipelineCtx(t.Context(), t, base, patches, vars)
}

// pipelineCtx is pipeline on ctx.
func pipelineCtx(ctx context.Context, t testing.TB, base, patches []source, vars subst.Source) *run {
	t.Helper()
	s := mustStages(t)
	out := &run{files: &tree.FileTable{}}
	var rs, ps []*tree.Resource
	for _, src := range base {
		rs = append(rs, parse(ctx, t, out.files, src, tree.RoleBase)...)
	}
	for _, src := range patches {
		ps = append(ps, parse(ctx, t, out.files, src, tree.RoleOverlay)...)
	}
	opts := overlay.Options{Schemas: s.idx, Files: out.files}
	for _, r := range rs {
		out.diags = append(out.diags, overlay.CheckBase(r, opts)...)
	}
	merged, ds, err := overlay.Apply(ctx, rs, ps, opts)
	if err != nil {
		t.Fatal(err)
	}
	if out.diags = append(out.diags, ds...); out.diags.HasErrors() {
		return out
	}
	sub := subst.New(subst.Options{Schemas: s.idx, Files: out.files, Variables: vars})
	for _, r := range merged {
		out.diags = append(out.diags, sub.Apply(r)...)
	}
	if out.diags.HasErrors() {
		return out
	}
	for _, r := range merged {
		ds, _ := s.views.Validate(r, out.files)
		out.diags = append(out.diags, ds...)
	}
	if out.diags.HasErrors() {
		return out
	}
	for _, r := range merged {
		c := *r
		c.Root = r.Root.Clone()
		out.rendered = append(out.rendered, &c)
	}
	b, ds, err := s.g.Run(ctx, merged, out.files, 2)
	if err != nil {
		t.Fatal(err)
	}
	if out.diags = append(out.diags, ds...); out.diags.HasErrors() {
		return out
	}
	chains, ds, err := s.resolver.Run(ctx, b, out.files, 2)
	if err != nil {
		t.Fatal(err)
	}
	if out.diags = append(out.diags, ds...); out.diags.HasErrors() {
		return out
	}
	rev, err := canonical.NewEncoder(s.hub).Encode(ctx, b, canonical.Options{})
	if err != nil {
		t.Fatal(err)
	}
	out.validated = &hub.Validated{Bundle: b, Chains: chains, Revision: rev}
	return out
}

// mustPipeline is pipeline failing on any error diagnostic.
func mustPipeline(t testing.TB, base, patches []source, vars subst.Source) *run {
	t.Helper()
	r := pipeline(t, base, patches, vars)
	if r.diags.HasErrors() || r.validated == nil {
		t.Fatalf("pipeline:\n%s", diagText(t, r.diags))
	}
	return r
}

// shop runs the pipeline over the example Bundle of the golden corpus for
// Environment env, or with no overlay and no variables for "".
func shop(t testing.TB, env string) *run {
	t.Helper()
	if env == "" {
		base, _ := readBundle(t.Context(), t, shopDir(t), "")
		return mustPipeline(t, base, nil, subst.Map{})
	}
	base, patches := readBundle(t.Context(), t, shopDir(t), env)
	return mustPipeline(t, base, patches, environment(t, env))
}

// rendered loads a rendered Bundle, the output of WriteBundle, as file
// mode loads its ruralz.yaml: one base file, no overlay, no variables.
func rendered(t testing.TB, data []byte) *run {
	t.Helper()
	return mustPipeline(t, []source{{"ruralz.yaml", data}}, nil, subst.Map{})
}

// renderYAML renders r's stage E trees as YAML.
func renderYAML(t testing.TB, r *run) []byte {
	t.Helper()
	return renderAs(t, r, FormatYAML)
}

// renderAs renders r's stage E trees in format f.
func renderAs(t testing.TB, r *run, f Format) []byte {
	t.Helper()
	var buf bytes.Buffer
	ds, err := WriteBundle(t.Context(), &buf, r.rendered, f, Options{Schemas: mustStages(t).idx, Files: r.files})
	if err != nil || len(ds) > 0 {
		t.Fatalf("WriteBundle(%s) = %v, %v", f, ds, err)
	}
	return buf.Bytes()
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

// resource returns the one resource parsed from YAML text, registered in
// files under path.
func resource(t testing.TB, files *tree.FileTable, path, yaml string) *tree.Resource {
	t.Helper()
	rs := parse(t.Context(), t, files, source{path, []byte(yaml)}, tree.RoleBase)
	if len(rs) != 1 {
		t.Fatalf("%s: %d documents, want 1", path, len(rs))
	}
	return rs[0]
}
