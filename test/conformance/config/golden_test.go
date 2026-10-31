// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diff"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/render"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The outputs of a level directory (docs/engineering/03-testing-and-
// quality-strategy.md "Golden tests"), per render: the canonical bytes,
// the rendered Bundle in both forms, the diagnostics in both forms, every
// Route's --effective table in both forms, and the diffs.
const (
	outCanonical   = "canonical"
	outRendered    = "rendered"
	outDiagnostics = "diagnostics"
	outEffective   = "effective"
	outDiff        = "diff"
)

// expectedDoc is a level's expected.json: the digests at level n, by
// render, or for a diff entry by side and render, from/<render> and
// to/<render>.
type expectedDoc struct {
	Level   int               `json:"level"`
	Digests map[string]string `json:"digests"`
}

// marshalExpected writes e as the corpus holds it: indented by two spaces,
// keys sorted, with a final newline.
func marshalExpected(e expectedDoc) ([]byte, error) {
	b, err := json.MarshalIndent(e, "", "  ")
	return append(b, '\n'), err
}

// entryName is the form of an entry's name: lower-case words of letters
// and digits joined by hyphens.
func entryName() *regexp.Regexp { return regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`) }

// TestGolden checks the golden corpus: the newest entry of each example
// holds its Bundle's files under examples/ (with -update, a changed
// example becomes a new entry), every entry has the layout, and every
// entry, rendered without --env and for each Environment of its
// environments.yaml at each schema level this release renders, reproduces
// its level's outputs byte for byte, on every path. A JSON twin renders to
// its Bundle's digests and canonical bytes, a rendered Bundle loads back
// to its digest and renders to its own bytes on every path, and the Node's
// file-mode load of the Bundle reports the CLI's diagnostics and digest.
func TestGolden(t *testing.T) {
	s := mustSuite(t)
	held := map[string]bool{}
	tag := ""
	if *update {
		var err error
		if tag, held, err = corpus.Held(t.Context(), s.root); err != nil {
			t.Fatalf("-update must know which files the newest release holds: %v", err)
		}
	}
	w := &writer{s: s, held: held, tag: tag}
	c := s.corpus
	if *update {
		w.addExampleEntries(t, s.golden())
		var err error
		if c, err = corpus.Load(s.corpusPath("")); err != nil {
			t.Fatal(err)
		}
	}
	var entries []*corpus.Entry
	for i := range c.Entries {
		if e := &c.Entries[i]; e.Kind == corpus.Golden {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		t.Fatal("the corpus holds no golden entry")
	}
	checkExampleEntries(t, s, entries)
	for _, e := range entries {
		t.Run(goldenName(e), func(t *testing.T) {
			checkEntry(t, s, w, e)
		})
	}
}

// checkEntry checks one golden entry at every level this release renders.
func checkEntry(t *testing.T, s *suite, w *writer, e *corpus.Entry) {
	if !entryName().MatchString(goldenName(e)) {
		t.Errorf("entry name %q is not lower-case words joined by hyphens", goldenName(e))
	}
	if len(e.Unknown) > 0 {
		t.Errorf("files outside the layout: %q", e.Unknown)
	}
	var names []string
	for _, b := range e.Bundles {
		names = append(names, b.Name)
	}
	switch {
	case e.IsDiff() && !slices.Equal(names, []string{corpus.FromDir, corpus.ToDir}):
		t.Fatalf("a diff entry holds from/ and to/ only, not %q", names)
	case !e.IsDiff() && !slices.Equal(names, []string{corpus.BundleDir}) && !slices.Equal(names, []string{corpus.BundleDir, corpus.BundleJSONDir}):
		t.Fatalf("an entry holds bundle/ and optionally bundle-json/, or from/ and to/, not %q", names)
	}
	levels := s.levels()
	for _, l := range e.Levels {
		if l.N > slices.Max(levels) {
			t.Errorf("level-%d is newer than the schema levels this release renders, %v", l.N, levels)
		}
	}
	for _, n := range levels {
		var out map[string][]byte
		if e.IsDiff() {
			out = diffOutputs(t, s, e, n)
		} else {
			out = entryOutputs(t, s, e, n)
		}
		w.level(t, e, n, out)
	}
}

// entryDir returns the directory of an entry and its environments file,
// "" when it has none.
func entryDir(s *suite, e *corpus.Entry) (dir, envFile string) {
	dir = s.corpusPath(e.Name)
	if e.Environments != nil {
		envFile = filepath.Join(dir, corpus.EnvironmentsFile)
	}
	return dir, envFile
}

// renderRun returns the run of the Bundle at bundle for render r.
func renderRun(bundle, envFile, r string) run {
	if r == noEnv {
		return run{bundle: bundle}
	}
	return run{bundle: bundle, environments: envFile, env: r}
}

// result runs r on p, keeping the rendered resources, and fails t when the
// run produced no result.
func result(t *testing.T, p *pipeline.Pipeline, r run) *pipeline.Result {
	t.Helper()
	req := requestOf(r)
	req.Render = true
	res, err := p.Run(t.Context(), req)
	if err != nil {
		t.Fatalf("run %s for %q: %v", r.bundle, r.env, err)
	}
	return res
}

// entryOutputs returns the files of level n of an entry with bundle/: per
// render, its outputs (renderOutputs), the digests of every render that
// reached one, and the diff between consecutive renders that both did,
// "-" to the first Environment, then each Environment to the next in file
// order. It also checks the entry's JSON twin and every path.
func entryOutputs(t *testing.T, s *suite, e *corpus.Entry, n int) map[string][]byte {
	dir, envFile := entryDir(s, e)
	bundle := filepath.Join(dir, corpus.BundleDir)
	renders := s.renders(t, envFile)
	out := map[string][]byte{}
	exp := expectedDoc{Level: n, Digests: map[string]string{}}
	results := make([]*pipeline.Result, len(renders))
	for i, r := range renders {
		res := result(t, s.cli, renderRun(bundle, envFile, r))
		results[i] = res
		renderOutputs(t, s, res, r, out)
		if res.Validated != nil {
			exp.Digests[r] = res.Validated.Revision.Digest.String()
		}
		if i > 0 && results[i-1].Validated != nil && res.Validated != nil {
			addDiff(t, s, results[i-1], renders[i-1], res, r, toLocation(goldenName(e)), out)
		}
	}
	data, err := marshalExpected(exp)
	if err != nil {
		t.Fatal(err)
	}
	out[corpus.ExpectedFile] = data
	if _, ok := e.Bundle(corpus.BundleJSONDir); ok {
		checkTwin(t, s, filepath.Join(dir, corpus.BundleJSONDir), envFile, renders, out, exp)
	}
	checkPaths(t, s, bundle, envFile, renders, out, exp)
	return out
}

// renderOutputs adds the outputs of render r of res to out: its
// diagnostics, and for a run that reached its digest the canonical bytes,
// the rendered Bundle and every Route's effective Filter Chain table.
func renderOutputs(t *testing.T, s *suite, res *pipeline.Result, r string, out map[string][]byte) {
	t.Helper()
	o, err := outcomeOf(res.Diagnostics, "")
	if err != nil {
		t.Fatal(err)
	}
	out[outDiagnostics+"/"+r+".txt"] = o.text
	out[outDiagnostics+"/"+r+".json"] = o.json
	if res.Validated == nil {
		return
	}
	out[outCanonical+"/"+r+".json"] = res.Validated.Revision.Content
	for _, f := range []render.Format{render.FormatYAML, render.FormatJSON} {
		var b bytes.Buffer
		ds, err := s.cli.WriteRendered(t.Context(), &b, res, f)
		if err != nil || len(ds) > 0 {
			t.Fatalf("render %s as %s: %v\n%s", r, f, err, texts(ds))
		}
		out[outRendered+"/"+r+"."+f.String()] = b.Bytes()
	}
	for _, route := range routeNames(res) {
		rows, err := s.cli.Effective(res, route)
		if err != nil {
			t.Fatalf("effective chain of %s in %s: %v", route, r, err)
		}
		for f, ext := range map[render.Format]string{render.FormatText: ".txt", render.FormatJSON: ".json"} {
			var b bytes.Buffer
			if err := render.WriteEffective(&b, rows, f); err != nil {
				t.Fatal(err)
			}
			out[outEffective+"/"+r+"/"+route+ext] = b.Bytes()
		}
	}
}

// routeNames returns the Route names of a run that reached its digest, in
// canonical order.
func routeNames(res *pipeline.Result) []string {
	var out []string
	for _, r := range res.Validated.Bundle.Resources() {
		if r.Kind == v1alpha1.KindRoute {
			out = append(out, r.Name)
		}
	}
	slices.Sort(out)
	return out
}

// envOf returns the Environment a render names to the diff engine: none
// for the render without --env.
func envOf(r string) string {
	if r == noEnv {
		return ""
	}
	return r
}

// addDiff adds the diff from render fromR of from to render toR of to as
// diff/<fromR>.<toR>.txt and .json: FROM as a Revision, TO as the source
// Bundle at loc, as ruralz bundle diff names them.
func addDiff(t *testing.T, s *suite, from *pipeline.Result, fromR string, to *pipeline.Result, toR, loc string, out map[string][]byte) {
	t.Helper()
	d, err := s.diff.Compare(t.Context(),
		diff.Side{State: from.Validated, Environment: envOf(fromR)},
		diff.Side{State: to.Validated, Origin: diff.OriginBundle, Location: loc, Environment: envOf(toR)})
	if err != nil {
		t.Fatalf("diff %s to %s: %v", fromR, toR, err)
	}
	out[outDiff+"/"+fromR+"."+toR+".txt"] = d.AppendText(nil)
	out[outDiff+"/"+fromR+"."+toR+".json"] = d.AppendJSON(nil)
}

// diffOutputs returns the files of level n of a diff entry: per render,
// both sides' digests and the diff from from/ to to/ rendered for it.
func diffOutputs(t *testing.T, s *suite, e *corpus.Entry, n int) map[string][]byte {
	dir, envFile := entryDir(s, e)
	out := map[string][]byte{}
	exp := expectedDoc{Level: n, Digests: map[string]string{}}
	for _, r := range s.renders(t, envFile) {
		sides := map[string]*pipeline.Result{}
		for _, side := range []string{corpus.FromDir, corpus.ToDir} {
			bundle := filepath.Join(dir, side)
			res := result(t, s.cli, renderRun(bundle, envFile, r))
			if res.Validated == nil {
				t.Fatalf("%s/ rendered for %s has no Revision:\n%s", side, r, texts(res.Diagnostics))
			}
			exp.Digests[side+"/"+r] = res.Validated.Revision.Digest.String()
			sides[side] = res
			if r == noEnv {
				checkNodeDigest(t, s, bundle, res)
			}
		}
		addDiff(t, s, sides[corpus.FromDir], r, sides[corpus.ToDir], r, toLocation(goldenName(e)), out)
	}
	data, err := marshalExpected(exp)
	if err != nil {
		t.Fatal(err)
	}
	out[corpus.ExpectedFile] = data
	return out
}

// checkNodeDigest checks that the Node's file-mode load of bundle reaches
// the digest of the CLI's render without --env.
func checkNodeDigest(t *testing.T, s *suite, bundle string, cli *pipeline.Result) {
	t.Helper()
	o, err := nodePath{s}.validate(t.Context(), run{bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if want := cli.Validated.Revision.Digest.String(); o.digest != want {
		t.Errorf("ruralzd loads %s to digest %q, ruralz renders %s", bundle, o.digest, want)
	}
}

// checkTwin checks a JSON twin: every render reaches its Bundle's digest
// and canonical bytes.
func checkTwin(t *testing.T, s *suite, twin, envFile string, renders []string, out map[string][]byte, exp expectedDoc) {
	t.Helper()
	for _, r := range renders {
		res := result(t, s.cli, renderRun(twin, envFile, r))
		got := ""
		if res.Validated != nil {
			got = res.Validated.Revision.Digest.String()
		}
		if got != exp.Digests[r] {
			t.Errorf("the JSON twin rendered for %s has digest %q, its Bundle %q\n%s", r, got, exp.Digests[r], texts(res.Diagnostics))
			continue
		}
		if res.Validated != nil && !bytes.Equal(res.Validated.Revision.Content, out[outCanonical+"/"+r+".json"]) {
			t.Errorf("the JSON twin rendered for %s has other canonical bytes than its Bundle", r)
		}
	}
}

// checkPaths runs an entry's Bundle on every path: each render a path can
// ask for (a file-mode Node renders without an Environment only) reports
// the corpus's diagnostics and digest, and each rendered Bundle loads back
// to its render's digest without an error; on the CLI's path it also
// renders to its own bytes (docs/architecture/02-configuration-model.md
// "Rendered Bundle": render is idempotent).
func checkPaths(t *testing.T, s *suite, bundle, envFile string, renders []string, out map[string][]byte, exp expectedDoc) {
	t.Helper()
	tmp := t.TempDir()
	for _, p := range s.runners(t) {
		for _, r := range renders {
			if r != noEnv && p.binary() == binaryNode {
				continue
			}
			o, err := p.validate(t.Context(), renderRun(bundle, envFile, r))
			if err != nil {
				t.Fatalf("%s: render %s: %v", p.binary(), r, err)
			}
			if !bytes.Equal(o.text, out[outDiagnostics+"/"+r+".txt"]) || !bytes.Equal(o.json, out[outDiagnostics+"/"+r+".json"]) {
				t.Errorf("%s: render %s reports\n%s\nruralz reports\n%s", p.binary(), r, o.text, out[outDiagnostics+"/"+r+".txt"])
			}
			if o.digest != exp.Digests[r] {
				t.Errorf("%s: render %s has digest %q, want %q", p.binary(), r, o.digest, exp.Digests[r])
			}
		}
		for _, r := range renders {
			yaml, ok := out[outRendered+"/"+r+".yaml"]
			if !ok {
				continue
			}
			file := filepath.Join(tmp, r+".yaml")
			if err := os.WriteFile(file, yaml, 0o600); err != nil {
				t.Fatal(err)
			}
			o, err := p.validate(t.Context(), run{bundle: file})
			if err != nil {
				t.Fatalf("%s: the rendered Bundle of %s: %v", p.binary(), r, err)
			}
			if o.digest != exp.Digests[r] {
				t.Errorf("%s: the rendered Bundle of %s loads to digest %q, want %q\n%s", p.binary(), r, o.digest, exp.Digests[r], o.text)
			}
		}
	}
	for _, r := range renders {
		yaml, ok := out[outRendered+"/"+r+".yaml"]
		if !ok {
			continue
		}
		res := result(t, s.cli, run{bundle: filepath.Join(tmp, r+".yaml")})
		var again bytes.Buffer
		if res.Validated == nil {
			continue
		}
		if ds, err := s.cli.WriteRendered(t.Context(), &again, res, render.FormatYAML); err != nil || len(ds) > 0 {
			t.Fatalf("render the rendered Bundle of %s: %v\n%s", r, err, texts(ds))
		}
		if !bytes.Equal(again.Bytes(), yaml) {
			t.Errorf("rendering the rendered Bundle of %s again changes it: %s", r, firstDiff(again.String(), string(yaml)))
		}
	}
}

// writer writes the corpus under -update and compares it otherwise. It
// never rewrites or deletes a file the newest release tag holds.
type writer struct {
	s    *suite
	held map[string]bool
	tag  string
}

// level compares the outputs of entry e at level n with its level
// directory: every output is there byte for byte and nothing else is.
// Under -update it writes what differs and deletes what is stale, refusing
// the files a release holds.
func (w *writer) level(t *testing.T, e *corpus.Entry, n int, out map[string][]byte) {
	t.Helper()
	rel := e.Name + "/" + corpus.LevelDir(n)
	dir := w.s.corpusPath(rel)
	have := map[string][]byte{}
	if _, err := os.Stat(dir); err == nil {
		have = readTree(t, dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, name := range sortedKeys(out) {
		got, want := out[name], have[name]
		if _, ok := have[name]; ok && bytes.Equal(got, want) {
			continue
		}
		switch _, ok := have[name]; {
		case *update:
			w.write(t, rel+"/"+name, got)
		case !ok:
			t.Errorf("%s/%s is missing; run go test ./test/conformance/config -run Golden -update", rel, name)
		default:
			t.Errorf("%s/%s differs from what this release produces (%s); a change that alters an output ships as a new schema level",
				rel, name, firstDiff(string(got), string(want)))
		}
	}
	for _, name := range sortedKeys(have) {
		if _, ok := out[name]; ok {
			continue
		}
		if *update {
			w.remove(t, rel+"/"+name)
			continue
		}
		t.Errorf("%s/%s is no output of the entry", rel, name)
	}
}

// refuse fails t and reports true when the newest release holds rel, a
// corpus path.
func (w *writer) refuse(t *testing.T, rel, what string) bool {
	t.Helper()
	if w.held[corpus.Dir+"/"+rel] {
		t.Errorf("refusing to %s %s: release %s holds it; add an entry or a schema level instead", what, rel, w.tag)
		return true
	}
	return false
}

// write writes data to the corpus path rel unless a release holds it.
func (w *writer) write(t *testing.T, rel string, data []byte) {
	t.Helper()
	if w.refuse(t, rel, "rewrite") {
		return
	}
	p := w.s.corpusPath(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", rel)
}

// remove deletes the corpus path rel unless a release holds it.
func (w *writer) remove(t *testing.T, rel string) {
	t.Helper()
	if w.refuse(t, rel, "delete") {
		return
	}
	if err := os.Remove(w.s.corpusPath(rel)); err != nil {
		t.Fatal(err)
	}
	t.Logf("deleted %s", rel)
}

// exampleBundles returns the Bundle roots under examples/, sorted: each is
// an example whose newest entry holds its files.
func exampleBundles(t testing.TB, s *suite) []string {
	t.Helper()
	ents, err := os.ReadDir(s.path(examplesDir))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range ents {
		if d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			if _, err := os.Stat(filepath.Join(s.path(examplesDir), d.Name(), bundleRoot)); err == nil {
				out = append(out, d.Name())
			}
		}
	}
	return out
}

// exampleFiles returns what an example entry holds for example x: bundle/
// the Bundle's files of examples/x, its configuration and .ruralzignore
// files (isExampleFile), so a file beside them such as the quickstart's
// quickstart.sh is no input; environments.yaml that of examples/control;
// and for the example Bundle its JSON twin.
func exampleFiles(t *testing.T, s *suite, x string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for name, data := range readTree(t, s.path(examplesDir+"/"+x)) {
		if isExampleFile(name) {
			out[corpus.BundleDir+"/"+name] = data
		}
	}
	env, err := os.ReadFile(s.path(examplesDir + "/" + controlDir + "/" + environmentsName))
	if err != nil {
		t.Fatal(err)
	}
	out[corpus.EnvironmentsFile] = env
	if x == shopBundle {
		for name, data := range jsonTwin(t, s, s.path(examplesDir+"/"+x)) {
			out[corpus.BundleJSONDir+"/"+name] = data
		}
	}
	return out
}

// entryInputs returns the inputs of an entry: its Bundle directories and
// environments file, by path relative to the entry directory.
func entryInputs(t *testing.T, s *suite, e *corpus.Entry) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	dir := s.corpusPath(e.Name)
	for _, b := range e.Bundles {
		for name, data := range readTree(t, filepath.Join(dir, b.Name)) {
			out[b.Name+"/"+name] = data
		}
	}
	if e.Environments != nil {
		out[corpus.EnvironmentsFile] = e.Environments
	}
	return out
}

// sameFiles describes the first difference between two file sets, "" when
// they are equal.
func sameFiles(got, want map[string][]byte) string {
	for _, name := range sortedKeys(want) {
		g, ok := got[name]
		switch {
		case !ok:
			return name + " is missing"
		case !bytes.Equal(g, want[name]):
			return name + " differs: " + firstDiff(string(g), string(want[name]))
		}
	}
	for _, name := range sortedKeys(got) {
		if _, ok := want[name]; !ok {
			return name + " is extra"
		}
	}
	return ""
}

// checkExampleEntries checks the entries of the examples: the newest entry
// of each example under examples/ holds its files byte for byte, and the
// newest diff entry of an example holds its Bundle in to/ and its
// Environments (docs/engineering/03-testing-and-quality-strategy.md
// "Golden tests").
func checkExampleEntries(t *testing.T, s *suite, entries []*corpus.Entry) {
	t.Helper()
	for _, x := range exampleBundles(t, s) {
		e := newest(entries, x)
		if e == nil {
			t.Errorf("examples/%s has no golden entry; run go test ./test/conformance/config -run Golden -update", x)
			continue
		}
		if d := sameFiles(entryInputs(t, s, e), exampleFiles(t, s, x)); d != "" {
			t.Errorf("examples/%s differs from %s, its newest entry (%s): entries never change, so run -update to add %s",
				x, e.Name, d, nextName(entries, x))
		}
		d := newest(entries, x+diffSuffix)
		if d == nil {
			continue
		}
		want := map[string][]byte{}
		for name, data := range exampleFiles(t, s, x) {
			if rest, ok := strings.CutPrefix(name, corpus.BundleDir+"/"); ok {
				want[corpus.ToDir+"/"+rest] = data
			} else if name == corpus.EnvironmentsFile {
				want[name] = data
			}
		}
		got := entryInputs(t, s, d)
		for name := range got {
			if strings.HasPrefix(name, corpus.FromDir+"/") {
				delete(got, name)
			}
		}
		if diff := sameFiles(got, want); diff != "" {
			t.Errorf("examples/%s differs from %s/to, the newest diff entry's TO (%s): add %s, its FROM the earlier state the "+
				"configuration model's diff example describes, and update that example", x, d.Name, diff, nextName(entries, x+diffSuffix))
		}
	}
}

// addExampleEntries adds, under -update, an entry for each example whose
// newest entry does not hold its files: <example> for the first,
// <example>-<k> after.
func (w *writer) addExampleEntries(t *testing.T, entries []*corpus.Entry) {
	t.Helper()
	for _, x := range exampleBundles(t, w.s) {
		want := exampleFiles(t, w.s, x)
		if e := newest(entries, x); e != nil && sameFiles(entryInputs(t, w.s, e), want) == "" {
			continue
		}
		name := corpus.GoldenDir + "/" + nextName(entries, x)
		for _, f := range sortedKeys(want) {
			w.write(t, name+"/"+f, want[f])
		}
	}
}

// jsonTwin returns the JSON twin of the Bundle at dir, by path relative to
// the twin's root: every configuration file the loader reads, base and
// overlay, written as JSON source with its ${VAR} text as authored
// (profile.EncodeJSON, Verbatim). A .json file holds one resource, so a
// file of several documents becomes one file per document, <name>-<k>.json
// with k counting from 1; ruralz.yaml keeps its name and its first
// document, in JSON, as the Bundle root, and the YAML parser reads it.
func jsonTwin(t *testing.T, s *suite, dir string) map[string][]byte {
	t.Helper()
	b, err := s.cli.Parse(context.Background(), loaderSource(dir))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range b.Files() {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Path))) //nolint:gosec // G304: a file of the example Bundle.
		if err != nil {
			t.Fatal(err)
		}
		format, _ := profile.FormatOf(f.Path)
		files := &tree.FileTable{}
		docs, ds, err := profile.Parse(context.Background(), data, files.Add(tree.File{Path: f.Path}), f.Path, format, profile.Options{})
		if err != nil || len(ds) > 0 {
			t.Fatalf("parse %s: %v\n%s", f.Path, err, texts(ds))
		}
		stem := strings.TrimSuffix(f.Path, path.Ext(f.Path))
		for i, doc := range docs {
			var buf bytes.Buffer
			if err := profile.EncodeJSON(&buf, doc.Root, profile.EncodeOptions{Verbatim: true}); err != nil {
				t.Fatal(err)
			}
			name := stem + ".json"
			switch {
			case f.Path == bundleRoot && i == 0:
				name = bundleRoot
			case len(docs) > 1 || f.Path == bundleRoot:
				name = fmt.Sprintf("%s-%d.json", stem, i+1)
			}
			out[name] = buf.Bytes()
		}
	}
	return out
}

// firstDiff describes the first line where got and want differ.
func firstDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := range max(len(g), len(w)) {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl || i >= len(g) || i >= len(w) {
			return fmt.Sprintf("line %d: got %q, want %q", i+1, gl, wl)
		}
	}
	return "no line differs"
}

// diagnosticsOf decodes the JSON form of diagnostics as the corpus holds
// it, for the cases that read single members.
func diagnosticsOf(t testing.TB, data []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode diagnostics: %v\n%s", err, data)
	}
	return out
}
