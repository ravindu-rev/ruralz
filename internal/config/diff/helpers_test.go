// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/defaults"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/precedence"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// stages holds the process-wide inputs of the test pipeline, built once.
type stages struct {
	set    *schemaidx.Set
	idx    *schemaidx.Index
	views  *schemaview.Set
	g      *defaults.Stage
	i      *precedence.Resolver
	loader *loader.Loader
}

// bodyOracle stands in for the CEL compiler: a rule reads the body when it
// mentions request.body.
type bodyOracle struct{}

func (bodyOracle) ReferencesRequestBody(src string) (bool, error) {
	return strings.Contains(src, "request.body"), nil
}

var testStages = sync.OnceValues(func() (*stages, error) {
	set, err := schemaidx.Embedded()
	if err != nil {
		return nil, err
	}
	idx, ok := set.Index(v1alpha1.APIVersion)
	if !ok {
		return nil, errors.New("no hub index")
	}
	views, err := schemaview.Embedded(set)
	if err != nil {
		return nil, err
	}
	reg := registry.New()
	g, err := defaults.New(defaults.Options{Schemas: set, Registry: reg})
	if err != nil {
		return nil, err
	}
	i, err := precedence.New(reg, precedence.Options{Body: bodyOracle{}})
	if err != nil {
		return nil, err
	}
	ld, err := loader.New(loader.Options{Schemas: set, Views: views})
	if err != nil {
		return nil, err
	}
	return &stages{set: set, idx: idx, views: views, g: g, i: i, loader: ld}, nil
})

// pipe returns the test pipeline.
func pipe(t testing.TB) *stages {
	t.Helper()
	s, err := testStages()
	if err != nil {
		t.Fatalf("test stages: %v", err)
	}
	return s
}

// hubIndex returns the ruralz/v1alpha1 index.
func hubIndex(t testing.TB) *schemaidx.Index { return pipe(t).idx }

// source is one file of a Bundle: its slash path and content.
type source struct {
	path string
	src  string
}

// rendered runs stages A to E through the loader over the Bundle srcs
// holds, as the pipeline facade does: discovery, which leaves out what a
// .ruralzignore excludes, parsing, the overlay of the diff example's
// Environment and the substitution of its variables, from the corpus
// entry's environments.yaml (diffEnv). A finding that blocks a Revision
// fails t.
func rendered(t testing.TB, srcs []source) ([]*tree.Resource, *tree.FileTable) {
	t.Helper()
	fsys := fstest.MapFS{}
	for _, s := range srcs {
		fsys[s.path] = &fstest.MapFile{Data: []byte(s.src)}
	}
	envs := diffCorpusEntry(t).Environments
	if envs == nil {
		t.Fatalf("%s has no %s", diffEntry, corpus.EnvironmentsFile)
	}
	res, err := pipe(t).loader.Load(context.Background(), loader.Request{
		Source:       loader.Source{FS: fsys, Path: "."},
		Environments: loader.Source{FS: fstest.MapFS{corpus.EnvironmentsFile: {Data: envs}}, Path: corpus.EnvironmentsFile},
		Environment:  diffEnv,
	})
	if err != nil {
		t.Fatalf("stages A to E: %v", err)
	}
	if res.HasErrors() || res.Stopped {
		t.Fatalf("stages A to E:\n%s", texts(res.Diagnostics))
	}
	return res.Resources, res.Files
}

// render runs the test pipeline over sources: stages A to E (rendered),
// then schema validation (F), defaults (G), effective chains (I) and the
// canonical form (L), as the pipeline facade does.
func render(t testing.TB, srcs []source) *hub.Validated {
	t.Helper()
	rs, files := rendered(t, srcs)
	return finish(t, rs, files)
}

// finish runs stages F, G, I and L over parsed or decoded resources.
func finish(t testing.TB, rs []*tree.Resource, files *tree.FileTable) *hub.Validated {
	t.Helper()
	s := pipe(t)
	ctx := context.Background()
	for _, r := range rs {
		if ds, ok := s.views.Validate(r, files); !ok || len(ds) > 0 {
			t.Fatalf("stage F %s: %s", r.ID, texts(ds))
		}
	}
	b, ds, err := s.g.Run(ctx, rs, files, 2)
	if err != nil || ds.HasErrors() {
		t.Fatalf("stage G: %v %s", err, texts(ds))
	}
	chains, ds, err := s.i.Run(ctx, b, files, 2)
	if err != nil || ds.HasErrors() {
		t.Fatalf("stage I: %v %s", err, texts(ds))
	}
	rev, err := canonical.NewEncoder(s.idx).Encode(ctx, b, canonical.Options{})
	if err != nil {
		t.Fatalf("stage L: %v", err)
	}
	return &hub.Validated{Bundle: b, Chains: chains, Revision: rev}
}

// dumped re-enters v's canonical content the way a /config/dump does:
// decoded, then stages F to L run again.
func dumped(t testing.TB, v *hub.Validated) *hub.Validated {
	t.Helper()
	s := pipe(t)
	files := &tree.FileTable{}
	rs, err := canonical.NewDecoder(s.idx, canonical.DecoderOptions{}).Decode(context.Background(), v.Revision.Content, canonical.Source{Name: "dump.json", Files: files})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	ptrs := make([]*tree.Resource, len(rs))
	for i := range rs {
		ptrs[i] = &rs[i]
	}
	return finish(t, ptrs, files)
}

// texts returns the one-line forms of ds.
func texts(ds diag.List) string {
	var b strings.Builder
	for _, d := range ds {
		b.Write(d.AppendText(nil))
		b.WriteByte('\n')
	}
	return b.String()
}

// goldenCorpus loads the configuration conformance corpus once for all
// tests: the one golden corpus of pipeline outputs, which holds the
// configuration model's diff example, its two Bundles and its outputs
// (docs/engineering/03-testing-and-quality-strategy.md "Golden tests").
var goldenCorpus = sync.OnceValues(corpus.Default)

// diffEntry is the corpus entry of the configuration model's diff example
// ("Diff semantics"): its to/ is the example Bundle, examples/shop-bundle,
// and its from/ the earlier state the example compares it with.
const diffEntry = "golden/shop-bundle-diff"

// diffEnv is the Environment the diff example renders both sides for.
const diffEnv = "prod"

// diffCorpusEntry returns the corpus entry of the diff example.
func diffCorpusEntry(t testing.TB) *corpus.Entry {
	t.Helper()
	c, err := goldenCorpus()
	if err != nil {
		t.Fatalf("load the golden corpus: %v", err)
	}
	e, ok := c.Entry(diffEntry)
	if !ok {
		t.Fatalf("the golden corpus has no entry %s", diffEntry)
	}
	return e
}

// diffLevel returns the outputs of the diff example at the hub schema's
// level, the level this release renders.
func diffLevel(t testing.TB) *corpus.Level {
	t.Helper()
	n := hubIndex(t).Level()
	l, ok := diffCorpusEntry(t).Level(n)
	if !ok {
		t.Fatalf("%s has no %s", diffEntry, corpus.LevelDir(n))
	}
	return l
}

// sameAsCorpus compares got with the output name of the diff example in the
// golden corpus.
func sameAsCorpus(t *testing.T, name string, got []byte) {
	t.Helper()
	want, ok := diffLevel(t).File(name)
	if !ok {
		t.Fatalf("%s has no output %s", diffEntry, name)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden corpus:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

// shop returns the files of a side of the diff example in the golden
// corpus, corpus.FromDir or corpus.ToDir, the example Bundle being to/:
// the Bundle as authored, its overlays and the files its .ruralzignore
// excludes included, which render renders for prod.
func shop(t testing.TB, side string) []source {
	t.Helper()
	b, ok := diffCorpusEntry(t).Bundle(side)
	if !ok {
		t.Fatalf("%s has no %s/", diffEntry, side)
	}
	out := make([]source, 0, len(b.Files))
	for _, f := range b.Files {
		out = append(out, source{path: f.Name, src: string(f.Data)})
	}
	return out
}

// edit returns srcs with the first old in file replaced by repl.
func edit(t testing.TB, srcs []source, file, old, repl string) []source {
	t.Helper()
	out := slices.Clone(srcs)
	for i := range out {
		if out[i].path == file {
			if !strings.Contains(out[i].src, old) {
				t.Fatalf("edit %s: %q not found", file, old)
			}
			out[i].src = strings.Replace(out[i].src, old, repl, 1)
			return out
		}
	}
	t.Fatalf("edit: no file %s", file)
	return nil
}

// with returns srcs with one more file, or file replaced.
func with(srcs []source, file, src string) []source {
	out := slices.DeleteFunc(slices.Clone(srcs), func(s source) bool { return s.path == file })
	out = append(out, source{path: file, src: src})
	slices.SortFunc(out, func(a, b source) int { return strings.Compare(a.path, b.path) })
	return out
}

// engine returns an Engine over the hub schema.
func engine(t testing.TB, workers int) *Engine {
	t.Helper()
	e, err := New(hubIndex(t), Options{Workers: workers})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return e
}

// compare diffs two states with plain sides.
func compare(t testing.TB, from, to *hub.Validated) *Diff {
	t.Helper()
	d, err := engine(t, 1).Compare(context.Background(), Side{State: from}, Side{State: to})
	if err != nil {
		t.Fatalf("Compare() error = %v", err)
	}
	return d
}

// opLines returns the human operation lines of the resource id, trimmed.
func opLines(t testing.TB, d *Diff, id string) []string {
	t.Helper()
	for i := range d.Resources {
		r := &d.Resources[i]
		if r.ID.String() != id {
			continue
		}
		var out []string
		for j := range r.Ops {
			out = append(out, strings.TrimSpace(string(appendOp(nil, &r.Ops[j]))))
		}
		return out
	}
	t.Fatalf("no change to %s in:\n%s", id, d.AppendText(nil))
	return nil
}

// resourceOf returns the change of resource id, or nil.
func resourceOf(d *Diff, id string) *Resource {
	for i := range d.Resources {
		if d.Resources[i].ID.String() == id {
			return &d.Resources[i]
		}
	}
	return nil
}

// altered returns v with fn applied to a copy of resource id's tree and
// the Revision encoded again from the trees, as stage L would. It lets a
// test hand the engine trees the pipeline never builds.
func altered(t *testing.T, v *hub.Validated, id string, fn func(root *tree.Node)) *hub.Validated {
	t.Helper()
	var rs []*hub.Resource
	found := false
	for _, r := range v.Bundle.Resources() {
		if r.String() == id {
			cp := *r
			cp.Tree = r.Tree.Clone()
			fn(cp.Tree)
			r, found = &cp, true
		}
		rs = append(rs, r)
	}
	if !found {
		t.Fatalf("no resource %s", id)
	}
	b := hub.NewBundle(rs)
	rev, err := canonical.NewEncoder(hubIndex(t)).Encode(context.Background(), b, canonical.Options{})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return &hub.Validated{Bundle: b, Chains: v.Chains, Revision: rev}
}

// codeOf returns err's RZ code, or "".
func codeOf(err error) string {
	c, _ := errcode.CodeOf(err)
	return c
}

// chainLines returns the human effective change lines, trimmed.
func chainLines(d *Diff) []string {
	var out []string
	for i := range d.Effective {
		out = append(out, strings.TrimSpace(string(appendChainChange(nil, &d.Effective[i]))))
	}
	return out
}
