// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/render"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
)

// Annotations FuzzRenderIdempotent writes on the Gateway, and the variable
// its value comes from.
const (
	renderVariable   = "PROPERTY_VALUE"
	annotateLiteral  = "property.ruralz.test/literal"
	annotateValue    = "property.ruralz.test/value"
	annotateMixed    = "property.ruralz.test/mixed"
	renderedFileName = "rendered.yaml"
)

// FuzzRenderIdempotent is the Render row of the Required properties:
// rendering a rendered Bundle again yields the same Revision, including
// $${ escapes (docs/architecture/02-configuration-model.md "Rendered
// Bundle"). The Gateway of a golden corpus Bundle carries three
// annotations: any literal text authored with $${ escapes, ${VAR} alone,
// and ${VAR:-default} inside other text beside a $${ escape, VAR holding
// any value, set in the process environment of the render without --env
// and in every Environment's spec.variables. For every render that builds,
// the rendered Bundle ruralz bundle render writes, loaded as the one-file
// Bundle file-mode ruralzd reads, builds the source's Revision and renders
// to the same bytes; the --output json form, its resources written one
// per file, builds the same Revision and renders to the same YAML and JSON
// bytes.
func FuzzRenderIdempotent(f *testing.F) {
	for i, s := range seeds(f) {
		a := corpus.Archive{Comment: []byte(s.name + "\n"), Files: s.files}
		pair := [][2]string{
			{"costs ${PRICE} and $${X}", "a${b}c $${d}"},
			{"multi\nline\ttab \"quoted\" \\ back: - [x]", "{a: b}\n- c"},
			{"", "${" + renderVariable + "}"},
			{"é ✓ \u2028 \u0085 \ufeff", "yes"},
		}[i%4]
		f.Add(a.Format(), s.envs, pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, archive, envs []byte, literal, value string) {
		if !utf8.ValidString(literal) || !utf8.ValidString(value) {
			t.Skip("the restricted profile reads UTF-8 only")
		}
		tc := tools(t)
		m, ok := annotated(t, tc, newMemBundle(corpus.ParseArchive(archive).Files, envs), literal, value)
		if !ok {
			t.Skip("the Bundle has no Gateway to annotate")
		}
		built := 0
		for _, env := range tc.renders(t, m) {
			src := tc.run(t, m, env, subst.Map{renderVariable: value}, true)
			if src.Validated == nil || src.HasErrors() {
				continue
			}
			first, ok := writeRendered(t, tc, src, render.FormatYAML)
			if !ok {
				continue
			}
			built++
			want := src.Validated.Revision.Digest
			again := runRendered(t, tc, fstest.MapFS{renderedFileName: {Data: first, Mode: 0o600}}, loader.Source{Path: renderedFileName})
			if again.Validated == nil || again.HasErrors() {
				t.Fatalf("render %q: the rendered Bundle does not load:\n%s\n%s", env, texts(again.Diagnostics), first)
			}
			if got := again.Validated.Revision.Digest; got != want {
				t.Fatalf("render %q: the rendered Bundle builds %s, the source %s:\n%s", env, got.Short(), want.Short(), first)
			}
			if second, _ := writeRendered(t, tc, again, render.FormatYAML); !bytes.Equal(second, first) {
				t.Fatalf("render %q: rendering the rendered Bundle changed its bytes:\n%s\nthen\n%s", env, first, second)
			}
			checkJSONForm(t, tc, env, src, first)
		}
		if built == 0 {
			t.Skip("no render of the Bundle builds: the negative corpus covers invalid input")
		}
	})
}

// annotated returns m with three annotations on its Gateway in
// ruralz.yaml: literal escaped (subst.Escape), so substitution reads it
// back as authored; ${renderVariable} alone; and ${renderVariable:-none}
// between other text and a $${ escape. Each Environment of m's
// environments file sets renderVariable to value. false when ruralz.yaml
// holds no Gateway or stages A to C refuse it.
func annotated(t *testing.T, tc *toolchain, m memBundle, literal, value string) (memBundle, bool) {
	t.Helper()
	src, ok := tc.parseSource(t, m)
	if !ok {
		return m, false
	}
	files := m.files()
	found := false
	for _, sf := range src {
		if sf.path != loader.RootFile {
			continue
		}
		for _, d := range sf.docs {
			if stringAt(d, "kind") != "Gateway" {
				continue
			}
			ann := ensureMap(ensureMap(d, "metadata"), "annotations")
			ann.Set(annotateLiteral, tree.Pos{}, str(subst.Escape(literal)))
			ann.Set(annotateValue, tree.Pos{}, str("${"+renderVariable+"}"))
			ann.Set(annotateMixed, tree.Pos{}, str("a-${"+renderVariable+":-none}-b $${HOME}"))
			found = true
		}
		files[loader.RootFile] = yamlOf(t, sf.docs)
	}
	if !found {
		return m, false
	}
	out := m.withFiles(files)
	if m.hasEnvs {
		envs, ok := withVariable(t, m.fs[envsFile].Data, renderVariable, value)
		if !ok {
			out.hasEnvs = false
			delete(out.fs, envsFile)
		} else {
			out.fs[envsFile] = &fstest.MapFile{Data: envs, Mode: 0o600}
		}
	}
	return out, true
}

// withVariable returns the environments file data with name set to value
// in every Environment's spec.variables; false when it does not parse.
func withVariable(t *testing.T, data []byte, name, value string) ([]byte, bool) {
	t.Helper()
	docs, ds, err := profile.Parse(t.Context(), data, 0, envsFile, profile.FormatYAML, profile.Options{})
	if err != nil || ds.HasErrors() || len(docs) == 0 {
		return nil, false
	}
	var roots []*tree.Node
	for _, d := range docs {
		if stringAt(d.Root, "kind") == "Environment" {
			ensureMap(ensureMap(d.Root, "spec"), "variables").Set(name, tree.Pos{}, str(value))
		}
		roots = append(roots, d.Root)
	}
	return yamlOf(t, roots), true
}

// writeRendered writes the rendered Bundle of res in format f; false when
// render refuses a secret field (RZ-CFG-012), which writes nothing.
func writeRendered(t *testing.T, tc *toolchain, res *pipeline.Result, f render.Format) ([]byte, bool) {
	t.Helper()
	var buf bytes.Buffer
	refused, err := tc.pipe.WriteRendered(t.Context(), &buf, res, f)
	if err != nil {
		t.Fatalf("WriteRendered: %v", err)
	}
	if len(refused) > 0 {
		return nil, false
	}
	return buf.Bytes(), true
}

// runRendered runs a rendered Bundle in fsys as file-mode ruralzd loads
// one: no overlay and no variable, with Request.Render.
func runRendered(t *testing.T, tc *toolchain, fsys fstest.MapFS, src loader.Source) *pipeline.Result {
	t.Helper()
	src.FS = fsys
	res, err := tc.pipe.Run(t.Context(), pipeline.Request{Source: src, Variables: subst.Map{}, Render: true})
	if err != nil {
		t.Fatalf("pipeline.Run(rendered): %v", err)
	}
	return res
}

// checkJSONForm checks the --output json form of src's rendered Bundle:
// its resources, written one per file (the Gateway as JSON text in
// ruralz.yaml, yamlSafe), build src's Revision and render to the YAML
// bytes yaml and to the same JSON bytes.
func checkJSONForm(t *testing.T, tc *toolchain, env string, src *pipeline.Result, yaml []byte) {
	t.Helper()
	js, ok := writeRendered(t, tc, src, render.FormatJSON)
	if !ok {
		t.Fatalf("render %q: the JSON form refuses what the YAML form wrote", env)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(js, &items); err != nil {
		t.Fatalf("render %q: the JSON form is no array: %v\n%s", env, err, js)
	}
	fsys := fstest.MapFS{}
	for i, it := range items {
		var head struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(it, &head); err != nil {
			t.Fatal(err)
		}
		name, data := fmt.Sprintf("b/r/%04d.json", i), bytes.Clone(it)
		if head.Kind == "Gateway" {
			name, data = "b/"+loader.RootFile, yamlSafe(data)
		}
		fsys[name] = &fstest.MapFile{Data: append(data, '\n'), Mode: 0o600}
	}
	res := runRendered(t, tc, fsys, loader.Source{Path: "b"})
	if res.Validated == nil || res.HasErrors() {
		t.Fatalf("render %q: the JSON form's resources do not load:\n%s\n%s", env, texts(res.Diagnostics), js)
	}
	if got, want := res.Validated.Revision.Digest, src.Validated.Revision.Digest; got != want {
		t.Fatalf("render %q: the JSON form builds %s, the source %s:\n%s", env, got.Short(), want.Short(), js)
	}
	if again, _ := writeRendered(t, tc, res, render.FormatJSON); !bytes.Equal(again, js) {
		t.Fatalf("render %q: rendering the JSON form's resources changed the JSON:\n%s\nthen\n%s", env, js, again)
	}
	if again, _ := writeRendered(t, tc, res, render.FormatYAML); !bytes.Equal(again, yaml) {
		t.Fatalf("render %q: the JSON form's resources render other YAML:\n%s\nthen\n%s", env, yaml, again)
	}
}

// ensureMap returns the mapping member key of n, adding an empty one, or
// replacing a member of another kind, when needed.
func ensureMap(n *tree.Node, key string) *tree.Node {
	if v, ok := n.Get(key); ok && v.Kind == tree.KindMap {
		return v
	}
	v := &tree.Node{Kind: tree.KindMap}
	n.Set(key, tree.Pos{}, v)
	return v
}

// str returns a string node.
func str(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }
