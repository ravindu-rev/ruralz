// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestLoadModel: Load builds the model of a document: version, info,
// servers with variables, paths in authored order with their path item
// parameters resolved through components, each operation's members, its
// request body and the components as written.
func TestLoadModel(t *testing.T) {
	doc := mustLoad(t, "api/openapi.yaml", map[string]string{"api/openapi.yaml": petstore})
	if doc.Version != "3.0.3" || doc.Minor != 0 {
		t.Errorf("version = %q minor %d, want 3.0.3 and 0", doc.Version, doc.Minor)
	}
	if doc.Info.Title != "Petstore" || doc.Info.Version != "1.0.0" {
		t.Errorf("info = %+v", doc.Info)
	}
	if len(doc.Servers) != 1 || doc.Servers[0].URL != "https://petstore.example/v1" ||
		doc.Servers[0].At.File != "api/openapi.yaml" || doc.Servers[0].At.Line != 6 {
		t.Errorf("servers = %+v", doc.Servers)
	}
	var keys []string
	for _, p := range doc.Paths {
		keys = append(keys, p.Key)
	}
	if !slices.Equal(keys, []string{"/pets", "/pets/{petId}"}) {
		t.Fatalf("paths = %q", keys)
	}
	ops := doc.Paths[0].Item.Operations
	var got []string
	for _, op := range ops {
		got = append(got, op.Method+" "+op.OperationID)
	}
	if !slices.Equal(got, []string{"get listPets", "post createPets"}) {
		t.Fatalf("operations of /pets = %q", got)
	}
	rb := ops[1].RequestBody
	if rb == nil || !rb.Required || len(rb.Content) != 1 || rb.Content[0].Type != "application/json" || rb.Content[0].Schema == nil {
		t.Errorf("request body = %+v", rb)
	}
	if ops[0].Responses == nil {
		t.Error("responses are missing")
	}
	item := doc.Paths[1].Item
	if len(item.Parameters) != 1 || item.Parameters[0].Name != "petId" || item.Parameters[0].In != "path" ||
		!item.Parameters[0].Required || item.Parameters[0].Schema == nil {
		t.Errorf("path item parameters = %+v", item.Parameters)
	}
	if doc.Components == nil {
		t.Error("components are missing")
	}
	if !slices.Equal(doc.Files, []string{"api/openapi.yaml"}) {
		t.Errorf("files = %q", doc.Files)
	}
}

// TestLoadYAMLAndJSON: a document reads to the same model from YAML, from
// JSON, and from JSON text under a name without a known extension, which
// reads as YAML.
func TestLoadYAMLAndJSON(t *testing.T) {
	const asJSON = `{"openapi": "3.1.0", "info": {"title": "t", "version": "1"},
 "servers": [{"url": "https://a.example"}],
 "paths": {"/a": {"get": {"operationId": "getA"}}, "/b/{id}": {"put": {"operationId": "putB"}}}}
`
	const asYAML = `openapi: 3.1.0
info: {title: t, version: "1"}
servers: [{url: "https://a.example"}]
paths:
  /a: {get: {operationId: getA}}
  /b/{id}: {put: {operationId: putB}}
`
	summary := func(d *Document) string {
		var b strings.Builder
		b.WriteString(d.Version + " " + d.Servers[0].URL)
		for _, p := range d.Paths {
			for _, op := range p.Item.Operations {
				b.WriteString(" " + op.Method + " " + p.Key + " " + op.OperationID)
			}
		}
		return b.String()
	}
	want := summary(mustLoad(t, "a.yaml", map[string]string{"a.yaml": asYAML}))
	for _, name := range []string{"a.json", "a.yml", "openapi"} {
		content := asJSON
		if name == "a.yml" {
			content = asYAML
		}
		if got := summary(mustLoad(t, name, map[string]string{name: content})); got != want {
			t.Errorf("%s reads %q, want %q", name, got, want)
		}
	}
}

// TestLoadVersion: only an openapi member 3.0.x or 3.1.x reads; a Swagger
// 2.0 document, another version, a number and no member at all are
// problems.
func TestLoadVersion(t *testing.T) {
	tests := []struct {
		name, head string
		minor      int
		want       string // a problem; empty for a document that loads
	}{
		{name: "3.0.0", head: "openapi: 3.0.0", minor: 0},
		{name: "3.0.3", head: "openapi: 3.0.3", minor: 0},
		{name: "3.1.0", head: "openapi: 3.1.0", minor: 1},
		{name: "a pre-release", head: "openapi: 3.1.0-rc1", minor: 1},
		{name: "3.2.0", head: "openapi: 3.2.0", want: `openapi "3.2.0" is not OpenAPI 3.0 or 3.1`},
		{name: "a version without patch", head: `openapi: "3.0"`, want: `openapi "3.0" is not OpenAPI 3.0 or 3.1`},
		{name: "a leading zero", head: "openapi: 3.0.01", want: "is not OpenAPI 3.0 or 3.1"},
		{name: "a number", head: "openapi: 3.1", want: "openapi is a number, not a version string"},
		{name: "Swagger 2.0", head: `swagger: "2.0"`, want: "a Swagger 2.0 document"},
		{name: "no openapi member", head: "title: x", want: "has no openapi member"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.head + "\ninfo: {title: t, version: \"1\"}\npaths: {}\n"
			doc, err := loadFiles(t, "a.yaml", map[string]string{"a.yaml": src}, Options{})
			if tc.want == "" {
				if err != nil || doc.Minor != tc.minor {
					t.Fatalf("Load = %v, minor %v; want minor %d", err, doc, tc.minor)
				}
				return
			}
			if ps := problems(t, err); !hasProblem(ps, tc.want) {
				t.Errorf("problems = %q, want one containing %q", ps, tc.want)
			}
		})
	}
}

// TestLoadRequiredMembers: a 3.0 document needs paths; a 3.1 document
// needs paths, webhooks or components, and webhooks alone suffice.
func TestLoadRequiredMembers(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{name: "3.0 without paths", src: "openapi: 3.0.3\ninfo: {title: t, version: \"1\"}\n", want: "an OpenAPI 3.0 document needs paths"},
		{name: "3.1 with nothing", src: "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\n", want: "needs paths, webhooks or components"},
		{name: "3.1 with webhooks only", src: "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\nwebhooks:\n  newPet: {post: {operationId: newPet}}\n"},
		{name: "3.1 with components only", src: "openapi: 3.1.0\ninfo: {title: t, version: \"1\"}\ncomponents: {schemas: {}}\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := loadFiles(t, "a.yaml", map[string]string{"a.yaml": tc.src}, Options{})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Load = %v", err)
				}
				if tc.name == "3.1 with webhooks only" && (len(doc.Webhooks) != 1 || doc.Webhooks[0].Key != "newPet") {
					t.Errorf("webhooks = %+v", doc.Webhooks)
				}
				return
			}
			if ps := problems(t, err); !hasProblem(ps, tc.want) {
				t.Errorf("problems = %q, want %q", ps, tc.want)
			}
		})
	}
}

// TestLoadProfileRules: YAML goes through the restricted profile, so an
// anchor, a duplicate key and a custom tag are problems with their
// position; and an OpenAPI file holds exactly one document.
func TestLoadProfileRules(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{name: "an anchor", src: minimal30 + "x: &a {}\ny: *a\n", want: "a.yaml:"},
		{name: "a duplicate key", src: minimal30 + "paths: {}\n", want: "a.yaml:4:1: "},
		{name: "a custom tag", src: minimal30 + "x: !include other.yaml\n", want: "a.yaml:"},
		{name: "two documents", src: minimal30 + "---\n" + minimal30, want: "a.yaml:4:1: the file holds 2 YAML documents; an OpenAPI file holds one"},
		{name: "no document", src: "# nothing\n", want: "a.yaml: the file holds no document"},
		{name: "a syntax error", src: "openapi: [3.0.3\n", want: "a.yaml:1:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFiles(t, "a.yaml", map[string]string{"a.yaml": tc.src}, Options{})
			if ps := problems(t, err); !hasProblem(ps, tc.want) {
				t.Errorf("problems = %q, want one containing %q", ps, tc.want)
			}
		})
	}
}

// TestLoadJSONRules: a .json file is strict JSON: a duplicate name and a
// trailing comma are problems at their position.
func TestLoadJSONRules(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{name: "a duplicate name", src: `{"openapi": "3.0.3", "openapi": "3.0.3", "paths": {}}`, want: "a.json:1:22: "},
		{name: "a trailing comma", src: `{"openapi": "3.0.3", "paths": {},}`, want: "a.json:1:"},
		{name: "a top-level list", src: `[]`, want: "a.json:1:1: "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFiles(t, "a.json", map[string]string{"a.json": tc.src}, Options{})
			if ps := problems(t, err); !hasProblem(ps, tc.want) {
				t.Errorf("problems = %q, want one containing %q", ps, tc.want)
			}
		})
	}
}

// TestLoadReadError: a root document that does not exist, or that is not
// a regular file, is a *ReadError naming it, never a problem.
func TestLoadReadError(t *testing.T) {
	_, err := loadFiles(t, "missing.yaml", map[string]string{}, Options{})
	var re *ReadError
	if !errors.As(err, &re) || re.Path != "missing.yaml" || !errors.Is(err, fs.ErrNotExist) ||
		!strings.HasPrefix(err.Error(), "cannot read missing.yaml: ") {
		t.Errorf("Load(missing) = %v (%T), want a *ReadError of missing.yaml", err, err)
	}
	dir := t.TempDir()
	_, err = Load(t.Context(), dir, Options{})
	if !errors.As(err, &re) || !errors.Is(err, ErrNotRegular) {
		t.Errorf("Load(directory) = %v (%T), want ErrNotRegular", err, err)
	}
}

// TestLoadOSFS: the default FS reads the operating system's files, the
// references relative to the referring file's directory.
func TestLoadOSFS(t *testing.T) {
	dir := t.TempDir()
	write := func(p, c string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("api/openapi.yaml", "openapi: 3.0.3\npaths:\n  /a: {$ref: 'items/a.yaml'}\n")
	write("api/items/a.yaml", "get: {operationId: getA}\n")
	doc, err := Load(t.Context(), filepath.Join(dir, "api", "openapi.yaml"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Paths) != 1 || doc.Paths[0].Item.Operations[0].OperationID != "getA" || len(doc.Files) != 2 {
		t.Errorf("paths = %+v, files %q", doc.Paths, doc.Files)
	}
}

// TestLoadCanceled: a canceled context ends the load with its error.
func TestLoadCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Load(ctx, "a.yaml", Options{FS: newMemFS(map[string]string{"a.yaml": minimal30})})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Load = %v, want context.Canceled", err)
	}
}

// TestLoadLimits: each limit of a load stops it with a problem: the source
// bytes and the files of every file read, the nesting depth, the nodes of
// one document, the node budget across the files and the problems cap.
func TestLoadLimits(t *testing.T) {
	withRef := map[string]string{
		"a.yaml":      "openapi: 3.0.3\npaths:\n  /a: {$ref: 'b.yaml'}\n",
		"b.yaml":      "get: {operationId: getA, summary: " + strings.Repeat("x", 200) + "}\n",
		"deep.yaml":   "openapi: 3.0.3\npaths: {}\nx: " + strings.Repeat("[", 10) + strings.Repeat("]", 10) + "\n",
		"many.yaml":   "openapi: 3.0.3\npaths:\n  /a: {$ref: 'b.yaml'}\n  /c: {$ref: 'c.yaml'}\n",
		"c.yaml":      "get: {operationId: getC}\n",
		"broken.yaml": "openapi: 3.0.3\npaths:\n  /a: 1\n  /b: 2\n  /c: 3\n",
	}
	tests := []struct {
		name   string
		root   string
		limits Limits
		want   string
	}{
		{name: "source bytes", root: "a.yaml", limits: Limits{MaxSourceBytes: 100}, want: "the load reads more than 100 bytes of source text"},
		{name: "files", root: "many.yaml", limits: Limits{MaxFiles: 2}, want: "the load reads more than 2 files"},
		{name: "depth", root: "deep.yaml", limits: Limits{MaxDepth: 5}, want: "deep.yaml:3:"},
		{name: "document nodes", root: "a.yaml", limits: Limits{MaxDocumentNodes: 5}, want: "a.yaml:"},
		{name: "node budget", root: "a.yaml", limits: Limits{MaxNodes: 12}, want: "the files of the load hold more than 12 YAML and JSON nodes"},
		{name: "problems cap", root: "broken.yaml", limits: Limits{MaxProblems: 1}, want: "path item"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFiles(t, tc.root, withRef, Options{Limits: tc.limits})
			ps := problems(t, err)
			if !hasProblem(ps, tc.want) {
				t.Errorf("problems = %q, want one containing %q", ps, tc.want)
			}
			if tc.name == "problems cap" && len(ps) != 2 {
				t.Errorf("problems = %q, want the cap of 1 plus the one that passed it", ps)
			}
		})
	}
	if _, err := loadFiles(t, "a.yaml", withRef, Options{Limits: Limits{MaxSourceBytes: 1 << 20, MaxFiles: 2, MaxDepth: 64}}); err != nil {
		t.Errorf("Load within the limits = %v", err)
	}
}

// TestLoadTypes: a member the model reads that has the wrong JSON type, or
// an object that lacks a member it needs, is a problem at its position;
// every problem of the document comes back in position order.
func TestLoadTypes(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{name: "paths not a mapping", body: "paths: []\n", want: "paths is a list, not a mapping"},
		{name: "a path item not a mapping", body: "paths:\n  /a: 1\n", want: `path item "/a" is a number, not a mapping`},
		{name: "a path without a slash", body: "paths:\n  a: {}\n", want: `paths member "a" does not begin with /`},
		{name: "an operation not a mapping", body: "paths:\n  /a: {get: []}\n", want: "get operation is a list, not a mapping"},
		{name: "an operationId number", body: "paths:\n  /a: {get: {operationId: 1}}\n", want: "operationId is a number, not a string"},
		{name: "tags not a list", body: "paths:\n  /a: {get: {tags: x}}\n", want: "tags is a string, not a list"},
		{name: "a tag number", body: "paths:\n  /a: {get: {tags: [1]}}\n", want: "a tags entry is a number, not a string"},
		{name: "deprecated a string", body: "paths:\n  /a: {get: {deprecated: 'yes'}}\n", want: "deprecated is a string, not a boolean"},
		{name: "parameters not a list", body: "paths:\n  /a: {parameters: {}}\n", want: "parameters is a mapping, not a list"},
		{name: "a parameter without name", body: "paths:\n  /a: {parameters: [{in: query}]}\n", want: "a parameter has no name"},
		{name: "a parameter without in", body: "paths:\n  /a: {parameters: [{name: x}]}\n", want: "a parameter has no in"},
		{name: "a parameter in body", body: "paths:\n  /a: {parameters: [{name: x, in: body}]}\n", want: `parameter in "body" is not query, header, path or cookie`},
		{name: "required a string", body: "paths:\n  /a: {parameters: [{name: x, in: query, required: optional}]}\n", want: "required is a string, not a boolean"},
		{name: "a schema list", body: "paths:\n  /a: {parameters: [{name: x, in: query, schema: []}]}\n", want: "schema is a list, not a Schema Object"},
		{name: "servers not a list", body: "servers: {}\npaths: {}\n", want: "servers is a mapping, not a list"},
		{name: "a server without url", body: "servers: [{description: x}]\npaths: {}\n", want: "a servers entry has no url"},
		{name: "a url number", body: "servers: [{url: 1}]\npaths: {}\n", want: "url is a number, not a string"},
		{name: "a variable without default", body: "servers: [{url: x, variables: {v: {enum: [a]}}}]\npaths: {}\n", want: `server variable "v" has no default`},
		{name: "a requestBody list", body: "paths:\n  /a: {post: {requestBody: []}}\n", want: "requestBody is a list, not a mapping"},
		{name: "content a list", body: "paths:\n  /a: {post: {requestBody: {content: []}}}\n", want: "content is a list, not a mapping"},
		{name: "responses a list", body: "paths:\n  /a: {get: {responses: []}}\n", want: "responses is a list, not a mapping"},
		{name: "components a list", body: "components: []\npaths: {}\n", want: "components is a list, not a mapping"},
		{name: "info a string", body: "info: x\npaths: {}\n", want: "info is a string, not a mapping"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadFiles(t, "a.yaml", map[string]string{"a.yaml": "openapi: 3.0.3\n" + tc.body}, Options{})
			if ps := problems(t, err); !hasProblem(ps, tc.want) {
				t.Errorf("problems = %q, want one containing %q", ps, tc.want)
			}
		})
	}
}

// TestLoadLenient: members the model does not read, extensions and
// unknown ones, are ignored, extension paths are left out, and info's
// scalars read as text whatever their type.
func TestLoadLenient(t *testing.T) {
	doc := mustLoad(t, "a.yaml", map[string]string{"a.yaml": `openapi: 3.0.3
x-vendor: {any: thing}
unknown: 1
info: {title: 7, version: 1.0}
paths:
  x-internal: {anything: goes}
  /a:
    x-rate: 5
    get: {operationId: getA, x-extra: true, unknownMember: [1]}
`})
	if doc.Info.Title != "7" || doc.Info.Version != "1.0" {
		t.Errorf("info = %+v, want the scalars' text", doc.Info)
	}
	if len(doc.Paths) != 1 || doc.Paths[0].Key != "/a" || doc.Paths[0].Item.Operations[0].OperationID != "getA" {
		t.Errorf("paths = %+v", doc.Paths)
	}
}

// TestLoadProblemsSorted: the problems of a load come back in file, line
// and column order, the root's and a referenced file's together.
func TestLoadProblemsSorted(t *testing.T) {
	_, err := loadFiles(t, "a.yaml", map[string]string{
		"a.yaml": "openapi: 3.0.3\npaths:\n  /b: {get: {operationId: 2}}\n  /a: {$ref: 'z.yaml'}\n  /c: {get: {operationId: 3}}\n",
		"z.yaml": "get: {operationId: 1}\n",
	}, Options{})
	got := problems(t, err)
	want := []string{
		"a.yaml:3:27: operationId is a number, not a string",
		"a.yaml:5:27: operationId is a number, not a string",
		"z.yaml:1:20: operationId is a number, not a string",
	}
	if !slices.Equal(got, want) {
		t.Errorf("problems = %q, want %q", got, want)
	}
	var le *Error
	if errors.As(err, &le) && le.Error() != want[0]+" (and 2 more problems)" {
		t.Errorf("Error() = %q", le.Error())
	}
}

// TestProblemString: a problem names its location as far as it is known.
func TestProblemString(t *testing.T) {
	tests := []struct {
		p    Problem
		want string
	}{
		{Problem{Message: "m"}, "m"},
		{Problem{Location: locationOf("a.yaml", 0, 0), Message: "m"}, "a.yaml: m"},
		{Problem{Location: locationOf("a.yaml", 3, 0), Message: "m"}, "a.yaml:3: m"},
		{Problem{Location: locationOf("a.yaml", 3, 7), Message: "m"}, "a.yaml:3:7: m"},
	}
	for _, tc := range tests {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
	if got := (&Error{}).Error(); got != "openapi: invalid document" {
		t.Errorf("empty Error() = %q", got)
	}
}
