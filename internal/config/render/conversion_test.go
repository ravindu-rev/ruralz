// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// sourceFiles parses every configuration file of the Bundle at dir,
// overlays included, into SourceFiles, as the CLI hands a source Bundle to
// Convert: the files stage A discovers (discover), each parsed as authored
// by loader.Bundle.ParseFile.
func sourceFiles(t testing.TB, dir string) []SourceFile {
	t.Helper()
	b, _ := discover(t, dir)
	var out []SourceFile
	for _, f := range b.Files() {
		rs, ds, err := b.ParseFile(t.Context(), f.Path)
		if err != nil || len(ds) > 0 {
			t.Fatalf("parse %s: %v %v", f.Path, ds, err)
		}
		sf := SourceFile{Path: f.Path}
		for _, r := range rs {
			sf.Documents = append(sf.Documents, r.Root)
		}
		out = append(out, sf)
	}
	return out
}

// values returns the JSON data model of each document of sfs, by path.
func values(t testing.TB, sfs []SourceFile) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, sf := range sfs {
		var docs []any
		for _, d := range sf.Documents {
			docs = append(docs, d.JSONValue())
		}
		b, err := json.Marshal(docs)
		if err != nil {
			t.Fatal(err)
		}
		out[sf.Path] = string(b)
	}
	return out
}

// buildDir returns a BuildFunc that runs the test pipeline over the Bundle
// at dir for the staging or prod Environment of the --environments file.
func buildDir(t *testing.T, dir string) BuildFunc {
	vars := map[string]subst.Map{"staging": environment(t, "staging"), "prod": environment(t, "prod")}
	return func(ctx context.Context, env string) (revision.Digest, diag.List, error) {
		base, patches := readBundle(t, dir, env)
		r := pipelineCtx(ctx, t, base, patches, vars[env])
		if r.validated == nil {
			return revision.Digest{}, r.diags, nil
		}
		return r.validated.Revision.Digest, r.diags, nil
	}
}

// TestConvertKeepsSources checks the rewrite of render --api-version on the
// example Bundle: every base and overlay file comes back at its own path in
// its own format, holding the same values, with ${VAR} expressions and
// $patch entries as authored and comments dropped; the files its
// .ruralzignore excludes are no source.
func TestConvertKeepsSources(t *testing.T) {
	src := sourceFiles(t, shopDir(t))
	files, ds, err := Convert(t.Context(), src, v1alpha1.APIVersion, convert.Default())
	if err != nil || len(ds) > 0 {
		t.Fatalf("Convert = %v, %v", ds, err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	want := []string{
		"ai/models.yaml", "ai/providers.yaml", "consumers/partner-acme.yaml", "overlays/prod/scale.yaml",
		"overlays/staging/models.yaml", "plugins/geo-block.yaml", "policies/common.yaml", "routes/cart-grpc.yaml",
		"routes/orders-summary.yaml", "routes/support-chat.yaml", "ruralz.yaml", "upstreams/services.yaml",
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths = %q\nwant %q", paths, want)
	}
	dir := t.TempDir()
	if err := WriteTree(filepath.Join(dir, "out"), files, TreeOptions{}); err != nil {
		t.Fatal(err)
	}
	back := sourceFiles(t, filepath.Join(dir, "out"))
	if got, want := values(t, back), values(t, src); !mapsEqual(got, want) {
		t.Errorf("converted files hold other values:\n got %v\nwant %v", got, want)
	}
	byPath := map[string]string{}
	for _, f := range files {
		byPath[f.Path] = string(f.Content)
	}
	if s := byPath["ruralz.yaml"]; !strings.Contains(s, `endpoint: "${OTEL_EXPORTER_OTLP_ENDPOINT:-https://otel-collector:4317}"`) || strings.Contains(s, "$${") {
		t.Errorf("ruralz.yaml does not keep the ${VAR} expression verbatim:\n%s", s)
	}
	if s := byPath["overlays/staging/models.yaml"]; !strings.Contains(s, `"$patch": delete`) {
		t.Errorf("the staging overlay lost its $patch entry:\n%s", s)
	}
	bundle, ok := shopCorpusEntry(t).Bundle(corpus.BundleDir)
	if !ok {
		t.Fatalf("%s has no %s/", shopEntry, corpus.BundleDir)
	}
	for p, comment := range map[string]string{
		"ruralz.yaml":                  "# HTTP/3 on UDP 8443",
		"upstreams/services.yaml":      "# upstream leg only",
		"overlays/staging/models.yaml": "# Kubernetes strategic-merge directive",
	} {
		if src, _ := bundle.File(p); !bytes.Contains(src, []byte(comment)) {
			t.Fatalf("%s no longer has the comment %q the test looks for", p, comment)
		}
		if strings.Contains(byPath[p], comment) {
			t.Errorf("%s keeps the comment %q", p, comment)
		}
	}
}

// mapsEqual reports whether two string maps are equal.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestConversionKeepsRevision is the M1 rule "render --api-version
// conversion MUST yield the same Revision": the example Bundle converted
// to ruralz/v1alpha1, the identity through the hub, builds the same
// Revision as its source for every Environment of the --environments
// file, so every comparison is Equal.
func TestConversionKeepsRevision(t *testing.T) {
	files, ds, err := Convert(t.Context(), sourceFiles(t, shopDir(t)), v1alpha1.APIVersion, convert.Default())
	if err != nil || len(ds) > 0 {
		t.Fatalf("Convert = %v, %v", ds, err)
	}
	out := filepath.Join(t.TempDir(), "converted")
	if err := WriteTree(out, files, TreeOptions{SourceRoot: shopDir(t)}); err != nil {
		t.Fatal(err)
	}
	cs, err := VerifyConversion(t.Context(), []string{"staging", "prod"}, buildDir(t, shopDir(t)), buildDir(t, out))
	if err != nil {
		t.Fatal(err)
	}
	if !AllEqual(cs) {
		t.Errorf("conversion changed a Revision: %+v", cs)
	}
	for _, c := range cs {
		if c.Source.Digest.IsZero() {
			t.Errorf("%s: no Revision built", c.Environment)
		}
	}
	if cs[0].Source.Digest == cs[1].Source.Digest {
		t.Error("staging and prod built one Revision, so the overlays were not compared")
	}
}

// TestConvertJSONVerbatim checks a .json source file: it stays one JSON
// object indented by two spaces, with ${VAR} text and $${ escapes as
// authored, since substitution has not run.
func TestConvertJSONVerbatim(t *testing.T) {
	data := []byte(`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "web",
  "annotations": {"shop.example/note": "costs $${PRICE}"}},
  "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "backend", "weight": "${WEIGHT}"}]}}`)
	docs, ds, err := profile.Parse(t.Context(), data, 0, "routes/web.json", profile.FormatJSON, profile.Options{})
	if err != nil || len(ds) > 0 {
		t.Fatal(ds, err)
	}
	files, ds, err := Convert(t.Context(), []SourceFile{{Path: "routes/web.json", Documents: []*tree.Node{docs[0].Root}}}, v1alpha1.APIVersion, convert.Default())
	if err != nil || len(ds) > 0 || len(files) != 1 {
		t.Fatalf("Convert = %v, %v, %v", files, ds, err)
	}
	got := string(files[0].Content)
	for _, want := range []string{"{\n  \"apiVersion\": \"ruralz/v1alpha1\",\n  \"kind\": \"Route\",", `"costs $${PRICE}"`, `"weight": "${WEIGHT}"`} {
		if !strings.Contains(got, want) {
			t.Errorf("converted JSON lacks %s:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "}\n") {
		t.Errorf("converted JSON does not end with a newline:\n%q", got)
	}
}

// TestConvertMultiDocumentOrder checks that a YAML file keeps its
// documents in stream order, and an empty file stays empty.
func TestConvertMultiDocumentOrder(t *testing.T) {
	src := []SourceFile{
		{Path: "x.yaml", Documents: []*tree.Node{
			resource(t, &tree.FileTable{}, "x.yaml", "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata: {name: z}\nspec: {protocol: http}\n").Root,
			resource(t, &tree.FileTable{}, "x.yaml", "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata: {name: a}\nspec: {protocol: http}\n").Root,
		}},
		{Path: "empty.yml"},
	}
	files, ds, err := Convert(t.Context(), src, v1alpha1.APIVersion, convert.Default())
	if err != nil || len(ds) > 0 {
		t.Fatal(ds, err)
	}
	if files[0].Path != "empty.yml" || len(files[0].Content) != 0 {
		t.Errorf("empty file = %q %q", files[0].Path, files[0].Content)
	}
	got := string(files[1].Content)
	if strings.Index(got, "name: z") > strings.Index(got, "name: a") || !strings.Contains(got, "\n---\n") {
		t.Errorf("document order changed:\n%s", got)
	}
}

// TestConvertUnservedTarget checks that a target apiVersion this release
// does not serve is one RZ-CFG-007 diagnostic naming it, with no files.
func TestConvertUnservedTarget(t *testing.T) {
	files, ds, err := Convert(t.Context(), sourceFiles(t, shopDir(t)), "ruralz/v1beta1", convert.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) > 0 || len(ds) != 1 || ds[0].Code != CodeUnserved || !strings.Contains(ds[0].Message, `target apiVersion "ruralz/v1beta1"`) {
		t.Errorf("Convert(v1beta1) = %d files, %+v; want one RZ-CFG-007", len(files), ds)
	}
	if !strings.Contains(ds[0].Hint, v1alpha1.APIVersion) {
		t.Errorf("hint = %q, want the served apiVersions", ds[0].Hint)
	}
}

// TestConvertUnservedSource checks that a document whose apiVersion is
// not served is RZ-CFG-007 at that apiVersion, located in its file and
// naming its resource, and that no file is returned.
func TestConvertUnservedSource(t *testing.T) {
	files := &tree.FileTable{}
	good := resource(t, files, "ruralz.yaml", minimalGateway)
	bad := resource(t, files, "routes/web.yaml", "apiVersion: ruralz/v9\nkind: Route\nmetadata: {name: web}\nspec: {}\n")
	out, ds, err := Convert(t.Context(), []SourceFile{
		{Path: "ruralz.yaml", Documents: []*tree.Node{good.Root}},
		{Path: "routes/web.yaml", Documents: []*tree.Node{bad.Root}},
	}, v1alpha1.APIVersion, convert.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 0 || len(ds) != 1 {
		t.Fatalf("Convert = %d files, %+v; want one diagnostic and no files", len(out), ds)
	}
	d := ds[0]
	if d.Code != CodeUnserved || d.File != "routes/web.yaml" || d.Line != 1 || d.Column != 13 || d.Resource == nil || d.Resource.Name != "web" || d.Path.String() != "apiVersion" {
		t.Errorf("diagnostic = %+v, want RZ-CFG-007 at routes/web.yaml:1:13 apiVersion of Route/web", d)
	}
}

// TestConvertInputErrors checks the paths and documents Convert refuses.
func TestConvertInputErrors(t *testing.T) {
	doc := resource(t, &tree.FileTable{}, "ruralz.yaml", minimalGateway).Root
	one := []*tree.Node{doc}
	cases := []struct {
		name  string
		files []SourceFile
		want  error
	}{
		{"parent", []SourceFile{{Path: "../ruralz.yaml", Documents: one}}, ErrPath},
		{"absolute", []SourceFile{{Path: "/ruralz.yaml", Documents: one}}, ErrPath},
		{"backslash", []SourceFile{{Path: `routes\web.yaml`, Documents: one}}, ErrPath},
		{"unclean", []SourceFile{{Path: "routes//web.yaml", Documents: one}}, ErrPath},
		{"dot", []SourceFile{{Path: "./ruralz.yaml", Documents: one}}, ErrPath},
		{"empty", []SourceFile{{Path: "", Documents: one}}, ErrPath},
		{"not configuration", []SourceFile{{Path: "README.md", Documents: one}}, ErrPath},
		{"duplicate", []SourceFile{{Path: "a.yaml", Documents: one}, {Path: "a.yaml", Documents: one}}, ErrPath},
		{"nil document", []SourceFile{{Path: "a.yaml", Documents: []*tree.Node{nil}}}, ErrInput},
		{"scalar document", []SourceFile{{Path: "a.yaml", Documents: []*tree.Node{{Kind: tree.KindString}}}}, ErrInput},
		{"two JSON documents", []SourceFile{{Path: "a.json", Documents: []*tree.Node{doc, doc}}}, ErrInput},
		{"no JSON document", []SourceFile{{Path: "a.json"}}, ErrInput},
	}
	for _, tc := range cases {
		if _, _, err := Convert(t.Context(), tc.files, v1alpha1.APIVersion, convert.Default()); !errors.Is(err, tc.want) {
			t.Errorf("%s: Convert error = %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, _, err := Convert(t.Context(), nil, v1alpha1.APIVersion, nil); !errors.Is(err, ErrOptions) {
		t.Errorf("Convert(nil registry) error = %v, want ErrOptions", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := Convert(ctx, []SourceFile{{Path: "a.yaml", Documents: one}}, v1alpha1.APIVersion, convert.Default()); !errors.Is(err, context.Canceled) {
		t.Errorf("Convert(canceled) error = %v", err)
	}
}

// TestConvertLeavesInputUnchanged checks that Convert never rewrites the
// source trees, which the loader may still use.
func TestConvertLeavesInputUnchanged(t *testing.T) {
	src := sourceFiles(t, shopDir(t))
	before := values(t, src)
	conv, err := convert.NewRegistry(convert.Lifecycle{}, renaming{})
	if err != nil {
		t.Fatal(err)
	}
	files, ds, err := Convert(t.Context(), src, v1alpha1.APIVersion, conv)
	if err != nil || len(ds) > 0 {
		t.Fatal(ds, err)
	}
	if after := values(t, src); !mapsEqual(after, before) {
		t.Error("Convert changed its input trees")
	}
	if !bytes.Contains(files[0].Content, []byte("ruralz.io/converted")) {
		t.Errorf("the converter's change is missing:\n%s", files[0].Content)
	}
}

// renaming is a ruralz/v1alpha1 converter that marks every document it
// converts from the hub, in place, to show Convert hands it a copy.
type renaming struct{}

func (renaming) APIVersion() string { return v1alpha1.APIVersion }

func (renaming) ToHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) { return doc, nil }

func (renaming) FromHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	meta, ok := doc.Get("metadata")
	if !ok {
		return doc, nil
	}
	meta.Set("annotations", tree.Pos{}, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
		{Key: "ruralz.io/converted", Value: &tree.Node{Kind: tree.KindString, Text: "true"}},
	}})
	return doc, nil
}
