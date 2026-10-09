// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestCodesRegistered checks that every code stage F raises is registered
// with its registered meaning (01 req 33; architecture section 0 item 2).
func TestCodesRegistered(t *testing.T) {
	for _, tc := range []struct {
		code, meaning string
	}{
		{CodeSchema, "JSON Schema violation"},
		{CodeUnknownField, "Unknown field"},
		{CodeLiteral, "Literal in a `SecretValue` field"},
	} {
		c, ok := errcode.Lookup(tc.code)
		if !ok {
			t.Errorf("errcode.Lookup(%q) not registered", tc.code)
			continue
		}
		if c.Meaning != tc.meaning {
			t.Errorf("errcode.Lookup(%q).Meaning = %q, want %q", tc.code, c.Meaning, tc.meaning)
		}
	}
}

// TestEmbedded compiles the embedded rendered view once and serves it by
// apiVersion (01 req 32).
func TestEmbedded(t *testing.T) {
	s, err := embedded()
	if err != nil {
		t.Fatalf("Embedded() error = %v", err)
	}
	if got, want := s.Served(), []string{"ruralz/v1alpha1"}; !slices.Equal(got, want) {
		t.Errorf("Served() = %v, want %v", got, want)
	}
	v, ok := s.View("ruralz/v1alpha1")
	if !ok {
		t.Fatal(`View("ruralz/v1alpha1") not found`)
	}
	if v.APIVersion() != "ruralz/v1alpha1" || v.Index().APIVersion() != "ruralz/v1alpha1" {
		t.Errorf("APIVersion() = %q, Index().APIVersion() = %q", v.APIVersion(), v.Index().APIVersion())
	}
	if _, ok := s.View("ruralz/v1beta1"); ok {
		t.Error(`View("ruralz/v1beta1") found, want unserved`)
	}
	kinds := []string{
		string(v1alpha1.KindGateway), string(v1alpha1.KindUpstream), string(v1alpha1.KindPlugin),
		string(v1alpha1.KindPolicy), string(v1alpha1.KindConsumer), string(v1alpha1.KindAIProvider),
		string(v1alpha1.KindAIModel), string(v1alpha1.KindRoute), string(v1alpha1.KindEnvironment),
		string(v1alpha1.KindCluster),
	}
	got := v.Kinds()
	slices.Sort(got)
	slices.Sort(kinds)
	if !slices.Equal(got, kinds) {
		t.Errorf("Kinds() = %v, want the ten kinds %v", got, kinds)
	}
	for _, k := range kinds {
		if v.kinds[k] == nil {
			t.Errorf("kind %s has no compiled definition", k)
		}
	}
}

// TestSetValidate selects the view by the resource's apiVersion.
func TestSetValidate(t *testing.T) {
	s, err := embedded()
	if err != nil {
		t.Fatal(err)
	}
	files := &tree.FileTable{}
	res := mustResource(t, files, "r.json", `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"}}`)
	got, ok := s.Validate(res, files)
	if !ok || len(got) != 1 || got[0].Message != "missing required field spec" {
		t.Errorf("Set.Validate() = %s, %v, want one missing-spec finding", texts(got), ok)
	}
	res.APIVersion = "ruralz/v1beta1"
	if got, ok := s.Validate(res, files); ok || got != nil {
		t.Errorf("Set.Validate(unserved) = %v, %v, want nil, false", got, ok)
	}
	if got, ok := s.Validate(nil, files); ok || got != nil {
		t.Errorf("Set.Validate(nil) = %v, %v, want nil, false", got, ok)
	}
}

// TestEmbeddedErrors covers the error paths of Embedded and NewSet.
func TestEmbeddedErrors(t *testing.T) {
	if _, err := Embedded(nil); err == nil {
		t.Error("Embedded(nil) error = nil, want an error")
	}
	idx, err := schemaidx.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compileFS(fstest.MapFS{}, idx); err == nil {
		t.Error("compileFS(empty) error = nil, want a missing-file error")
	}
	bad := fstest.MapFS{"ruralz/v1alpha1/rendered.schema.json": {Data: []byte("{")}}
	if _, err := compileFS(bad, idx); !errors.Is(err, ErrSchema) {
		t.Errorf("compileFS(invalid JSON) error = %v, want ErrSchema", err)
	}
	if _, err := NewSet(nil); err == nil {
		t.Error("NewSet(nil) error = nil, want an error")
	}
	v := testView(t)
	if _, err := NewSet(v, v); err == nil {
		t.Error("NewSet(v, v) error = nil, want a duplicate apiVersion error")
	}
}

// TestCompileErrors checks that a schema that does not compile, or that
// disagrees with its index, is ErrSchema (01 req 32).
func TestCompileErrors(t *testing.T) {
	synthetic, err := os.ReadFile(filepath.Join("testdata", "synthetic.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := schemaidx.Load("test/v1", synthetic)
	if err != nil {
		t.Fatal(err)
	}
	src := string(synthetic)
	for _, tc := range []struct {
		name   string
		schema string
	}{
		{"invalid JSON", "{"},
		{"draft violation", `{"type": 7}`},
		{"vocabulary violation", strings.Replace(src, `"x-ruralz-list": {"type": "set"}`, `"x-ruralz-list": {"type": "sets"}`, 1)},
		{"unresolvable reference", strings.Replace(src, `"spec": {"type": "object"}`, `"spec": {"$ref": "#/$defs/Missing"}`, 1)},
		{"kind without definition", strings.Replace(src, `"then": {"$ref": "#/$defs/Other"}`, `"then": {"required": ["spec"]}`, 1)},
		{"extra kind", strings.Replace(src,
			`{"if": {"properties": {"kind": {"const": "Other"}}`,
			`{"if": {"properties": {"kind": {"const": "Extra"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Other"}},
    {"if": {"properties": {"kind": {"const": "Other"}}`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compile([]byte(tc.schema), idx); !errors.Is(err, ErrSchema) {
				t.Errorf("Compile() error = %v, want ErrSchema", err)
			}
		})
	}
	if _, err := Compile(synthetic, nil); !errors.Is(err, ErrSchema) {
		t.Errorf("Compile(nil index) error = %v, want ErrSchema", err)
	}
}

// TestRefusingLoader checks that compiling never loads a URL: a remote
// $ref fails with ErrRemoteReference (01 req 32, risk 30).
func TestRefusingLoader(t *testing.T) {
	synthetic, err := os.ReadFile(filepath.Join("testdata", "synthetic.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := schemaidx.Load("test/v1", synthetic)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"http://localhost:1234/integer.json", "file:///etc/passwd", "https://schemas.example/s.json#/x"} {
		src := strings.Replace(string(synthetic), `"spec": {"type": "object"}`, `"spec": {"$ref": "`+ref+`"}`, 1)
		_, err := Compile([]byte(src), idx)
		if !errors.Is(err, ErrSchema) || !errors.Is(err, ErrRemoteReference) {
			t.Errorf("Compile($ref %s) error = %v, want ErrSchema and ErrRemoteReference", ref, err)
		}
	}
	if _, err := (refusingLoader{}).Load("file:///etc/passwd"); !errors.Is(err, ErrRemoteReference) {
		t.Errorf("Load() error = %v, want ErrRemoteReference", err)
	}
}

// TestFormatsNotAsserted checks that format stays an annotation (01 req
// 32): an invalid email validates.
func TestFormatsNotAsserted(t *testing.T) {
	c, err := newCompiler()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddResource("urn:test:format", map[string]any{"format": "email"}); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("urn:test:format")
	if err != nil {
		t.Fatal(err)
	}
	if err := sch.Validate("not an email"); err != nil {
		t.Errorf("Validate() error = %v, want nil (format is an annotation)", err)
	}
}

// TestValidateNil covers the degenerate inputs of Validate.
func TestValidateNil(t *testing.T) {
	v := testView(t)
	if got := v.Validate(nil, nil); got != nil {
		t.Errorf("Validate(nil) = %v, want nil", got)
	}
	files := &tree.FileTable{}
	id := files.Add(tree.File{Path: "empty.yaml", Role: tree.RoleBase})
	res := &tree.Resource{ID: tree.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Start: tree.Pos{File: id, Line: 3, Column: 1}}
	got := v.Validate(res, files)
	if len(got) != 1 || got[0].Code != CodeSchema || got[0].File != "empty.yaml" || got[0].Line != 3 {
		t.Errorf("Validate(empty document) = %v, want one RZ-CFG-005 at empty.yaml:3", got)
	}
}

// TestValidateWithoutFiles reports line and column without a file table.
func TestValidateWithoutFiles(t *testing.T) {
	v := testView(t)
	root, err := parseJSON([]byte(`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"}}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := v.Validate(resourceOf(root), nil)
	want := diag.Diagnostic{
		Code: CodeSchema, Severity: diag.SeverityError, Location: diag.Location{Line: 1, Column: 1},
		Resource: &diag.ResourceID{Kind: "Route", Name: "r"}, Message: "missing required field spec",
	}
	if len(got) != 1 || got[0].Location != want.Location || got[0].Message != want.Message || *got[0].Resource != *want.Resource {
		t.Errorf("Validate() = %+v, want %+v", got, want)
	}
}

// TestValidResources accepts minimal valid resources of every M1 kind
// (01 req 32), and an unknown member of an open config (01 test plan,
// RZ-CFG-006 negative: cors config.maxAge).
func TestValidResources(t *testing.T) {
	for _, src := range []string{
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "edge", "labels": {"team": "a"}, "annotations": {"ruralz.io/conversion-data": "x"}},
		  "spec": {"listeners": [{"name": "http", "port": 8080, "protocol": "http"}], "limits": {"maxRequestBodyBytes": "10Mi", "maxResponseBodyBytes": 1024}}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"},
		  "spec": {"match": {"path": {"prefix": "/"}, "methods": ["GET", "POST"]}, "upstreams": [{"name": "u"}], "timeout": "1m30s"}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u"},
		  "spec": {"protocol": "http", "endpoints": [{"address": "10.0.0.1:80"}]}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "c"},
		  "spec": {"type": "cors", "config": {"allowOrigins": ["https://a.example"], "maxAge": 600}}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "c"},
		  "spec": {"credentials": {"apiKeys": [{"name": "k", "secretRef": {"provider": "env", "name": "RURALZ_SECRET_K"}}]}}}`,
	} {
		if got := validateJSON(t, src); len(got) != 0 {
			t.Errorf("Validate() = %s, want no diagnostics for\n%s", texts(got), src)
		}
	}
}

// TestConcurrentValidate shares one View across goroutines under -race
// (01 req 53: the pipeline's workers share the compiled view).
func TestConcurrentValidate(t *testing.T) {
	v := testView(t)
	src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"}, "spec": {"timout": "1s", "match": {"path": {"exact": "/", "prefix": "/"}}}}`
	files := &tree.FileTable{}
	res := mustResource(t, files, "r.json", src)
	want := texts(v.Validate(res, files))
	done := make(chan string, 8)
	for range 8 {
		go func() { done <- texts(v.Validate(res, files)) }()
	}
	for range 8 {
		if got := <-done; got != want {
			t.Errorf("concurrent Validate() = %q, want %q", got, want)
		}
	}
	if !strings.Contains(want, CodeUnknownField) {
		t.Errorf("Validate() = %q, want an unknown-field finding", want)
	}
}

// TestBaseURL keeps the compile base a name, never a fetchable URL.
func TestBaseURL(t *testing.T) {
	if got := baseURL("ruralz/v1alpha1"); got != "urn:ruralz:schema:ruralz:v1alpha1" {
		t.Errorf("baseURL() = %q", got)
	}
	if len(schema.RenderedV1alpha1()) == 0 {
		t.Error("RenderedV1alpha1() is empty")
	}
}
