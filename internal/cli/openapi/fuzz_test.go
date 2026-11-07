// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// fuzzLimits keep a fuzz input's load small: 1 MiB of source, 100,000
// nodes.
func fuzzLimits() Limits {
	return Limits{MaxSourceBytes: 1 << 20, MaxDocumentNodes: 100_000, MaxNodes: 100_000, MaxProblems: 100}
}

// corpusRoot is the vendored SM-8 corpus, whose small documents seed the
// fuzz targets.
const corpusRoot = "../../../test/fixtures/openapi-corpus"

// corpusSeeds returns the corpus documents of at most limit bytes.
func corpusSeeds(f *testing.F, limit int) [][]byte {
	f.Helper()
	var out [][]byte
	err := filepath.WalkDir(corpusRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.Contains(filepath.ToSlash(p), "/data/") {
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // G304: a corpus file under the repository's test fixtures.
		if err != nil {
			return err
		}
		if len(b) <= limit {
			out = append(out, b)
		}
		return nil
	})
	if err != nil {
		f.Fatal(err)
	}
	return out
}

// labelPattern is the RFC 1123 label every resource name matches.
func labelPattern() *regexp.Regexp { return regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`) }

// FuzzLoad: Load of any bytes, as YAML and as JSON, beside a second file a
// reference may name, never panics or hangs; it builds a model, or fails
// with an *Error whose problems each carry a message and name no file but
// one the load read. A model always imports into a Bundle whose stream
// the restricted profile reads back without a finding, whose names are
// RFC 1123 labels and whose Route paths pass the Router's checks.
func FuzzLoad(f *testing.F) {
	for _, s := range []string{minimal30, petstore, "openapi: 3.1.0\npaths:\n  /a: {$ref: 'other.yaml#/Item'}\n"} {
		f.Add([]byte(s), []byte("Item: {get: {operationId: x}}\n"), false)
	}
	f.Add([]byte(`{"openapi": "3.0.3", "paths": {"/a/{b}.json": {"get": {}}}, "servers": [{"url": "https://a.example"}]}`), []byte("{}"), true)
	for _, b := range corpusSeeds(f, 16<<10) {
		f.Add(b, []byte("x: {$ref: '#/x'}\n"), b[0] == '{')
	}
	f.Fuzz(func(t *testing.T, root, other []byte, asJSON bool) {
		name := "api/root.yaml"
		if asJSON {
			name = "api/root.json"
		}
		fsys := newMemFS(map[string]string{name: string(root), "api/other.yaml": string(other)})
		doc, err := Load(t.Context(), filepath.FromSlash(name), Options{FS: fsys, Limits: fuzzLimits()})
		if err != nil {
			var le *Error
			if !errors.As(err, &le) || len(le.Problems) == 0 {
				t.Fatalf("Load = %v (%T), want an *Error with problems", err, err)
			}
			read := map[string]bool{"": true}
			for p := range fsys.reads {
				read[displayName(p)] = true
			}
			for _, p := range le.Problems {
				if p.Message == "" || !read[p.File] {
					t.Fatalf("problem %+v names no file the load read (%v)", p, fsys.reads)
				}
			}
			return
		}
		checkImport(t, Import(doc))
	})
}

// checkImport checks the invariants of an imported Bundle.
func checkImport(t *testing.T, b *Bundle) {
	t.Helper()
	stream, err := b.Stream()
	if err != nil {
		t.Fatalf("Stream = %v", err)
	}
	docs, ds, err := profile.Parse(t.Context(), stream, 0, "stream.yaml", profile.FormatYAML, profile.Options{})
	if err != nil || len(ds) > 0 || len(docs) != len(b.Resources) {
		t.Fatalf("Parse(stream) = %d documents, %v, %v; want %d", len(docs), ds, err, len(b.Resources))
	}
	re := labelPattern()
	for _, r := range b.Resources {
		if !re.MatchString(r.Name) || len(r.Name) > maxName {
			t.Fatalf("%s name %q is no RFC 1123 label", r.Kind, r.Name)
		}
		if r.Kind != v1alpha1.KindRoute {
			continue
		}
		spec, _ := r.Doc.Get("spec")
		p, _ := spec.At(diag.Path{diag.Field("match"), diag.Field("path")})
		if p == nil || len(p.Members) != 1 {
			t.Fatalf("Route %s has no single path criterion", r.Name)
		}
		checkForm(t, p.Members[0].Key, p.Members[0].Value.Text)
	}
}

// checkForm fails unless value passes the Router's check of its form.
func checkForm(t *testing.T, form, value string) {
	t.Helper()
	var err error
	switch form {
	case "exact":
		err = routematch.CheckPath(value)
	case "template":
		err = routematch.CheckTemplate(value)
	case "regex":
		_, err = routematch.CompileRegex(value)
	default:
		t.Fatalf("path form %q", form)
	}
	if err != nil {
		t.Fatalf("%s %q: %v", form, value, err)
	}
}

// refFiles are the files the $ref fuzz target resolves among: a root, a
// file beside it, one above and one that references back.
func refFiles() map[string]string {
	return map[string]string{
		"api/root.yaml": `openapi: 3.0.3
paths:
  /a: {get: {operationId: a, parameters: [{name: x, in: query}]}}
  /b/~1c: {$ref: "common.yaml#/Item"}
components:
  list: [zero, one, {two: 2}]
  "a/b": {"~": tilde}
loop: {$ref: "#/loop"}
`,
		"api/common.yaml":  "Item: {get: {operationId: common}}\nBack: {$ref: 'root.yaml#/components'}\nUp: {$ref: '../shared/up.yaml'}\n",
		"shared/up.yaml":   "Down: {$ref: '../api/common.yaml#/Back'}\n",
		"api/not yaml.txt": "{[",
	}
}

// FuzzRef: parseRef of any value never panics, and a value it accepts
// keeps no scheme, absolute path or backslash in its file part, as
// written or decoded, and round-trips its pointer through pointerText.
// Resolving and following the value among a fixed set of files never
// panics or hangs: it names a node of a file the load read, or reports
// one problem.
func FuzzRef(f *testing.F) {
	for _, s := range []string{
		"#/components/list/2/two", "#/components/a~1b/~0", "common.yaml#/Item", "common.yaml#/Back",
		"../shared/up.yaml#/Down", "#/loop", "#", "common.yaml", "not%20yaml.txt", "%2Fetc", "http://x/y",
		"#/paths/~1b~1~01c", "C%3A/x", "a\\b", "#name", "#/a~2", "x.yaml?q",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, ref string) {
		r, err := parseRef(ref)
		if err == nil {
			if r.file != "" && checkFilePart(r.file) != nil {
				t.Fatalf("parseRef(%q) accepted file %q", ref, r.file)
			}
			back, err := parseRef("#" + pointerText(r.pointer))
			if err != nil || !slices.Equal(back.pointer, r.pointer) {
				t.Fatalf("pointer %q of %q does not round-trip: %q, %v", r.pointer, ref, back.pointer, err)
			}
		}
		l := newLoad(t.Context(), Options{FS: newMemFS(refFiles()), Limits: fuzzLimits()})
		root, oerr := l.open(filepath.Clean(filepath.FromSlash("api/root.yaml")), diag.Location{}, true)
		if oerr != nil || root.root == nil {
			t.Fatalf("open root = %v", oerr)
		}
		at := &tree.Node{Kind: tree.KindString, Text: ref, Pos: root.root.Pos}
		tg, ok, rerr := l.resolve(at)
		switch {
		case rerr != nil && !errors.Is(rerr, errStop):
			t.Fatalf("resolve(%q) = %v", ref, rerr)
		case ok && l.fileAt(tg.node) == nil:
			t.Fatalf("resolve(%q) named a node of no file of the load", ref)
		case !ok && rerr == nil && len(l.problems) == 0:
			t.Fatalf("resolve(%q) failed without a problem", ref)
		}
		n, derr := l.deref(mapNode("$ref", strNode(ref)))
		if derr != nil && !errors.Is(derr, errStop) {
			t.Fatalf("deref(%q) = %v", ref, derr)
		}
		if n != nil {
			if v, isRef := n.Get("$ref"); isRef && v.Kind == tree.KindString {
				t.Fatalf("deref(%q) stopped at a Reference Object", ref)
			}
		}
	})
}

// FuzzRoutePath: the Route criterion of any path is one form that passes
// the Router's check of that form, or a reason and no criterion.
func FuzzRoutePath(f *testing.F) {
	for _, s := range []string{"", "pets", "pets/{petId}", "files/{name}.json", "{a}-{b}", "a//{b}", "a/{b", "a/{}", "a/%00", "./{x}y", "{ид}"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		m, why := routePath("/" + p)
		set := 0
		for _, v := range []string{m.Exact, m.Template, m.Regex} {
			if v != "" {
				set++
			}
		}
		switch {
		case why != "" && set != 0:
			t.Fatalf("routePath(%q) = %+v with reason %q", p, m, why)
		case why == "" && set != 1:
			t.Fatalf("routePath(%q) = %+v, want one form", p, m)
		case m.Exact != "":
			checkForm(t, "exact", m.Exact)
		case m.Template != "":
			checkForm(t, "template", m.Template)
		case m.Regex != "":
			checkForm(t, "regex", m.Regex)
		}
	})
}

// FuzzLabel: a label is empty or an RFC 1123 label of at most 63
// characters, and labeling it again leaves it as it is.
func FuzzLabel(f *testing.F) {
	for _, s := range []string{"getPetById", "HTTPServer", "  --x--  ", "日本語", strings.Repeat("Ab", 40)} {
		f.Add(s)
	}
	re := labelPattern()
	f.Fuzz(func(t *testing.T, s string) {
		l := label(s)
		if l != "" && (!re.MatchString(l) || len(l) > maxName) {
			t.Fatalf("label(%q) = %q", s, l)
		}
		if again := label(l); again != l {
			t.Fatalf("label(label(%q)) = %q, want %q", s, again, l)
		}
	})
}
