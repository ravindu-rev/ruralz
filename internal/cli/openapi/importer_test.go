// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"bytes"
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

// importDoc loads src as a.yaml and imports it.
func importDoc(t *testing.T, src string) *Bundle {
	t.Helper()
	return Import(mustLoad(t, "a.yaml", map[string]string{"a.yaml": src}))
}

// get returns the scalar text at the dot path p of n, "" when absent.
func get(n *tree.Node, p string) string {
	cur := n
	for _, k := range strings.Split(p, ".") {
		if cur == nil {
			return ""
		}
		if cur.Kind == tree.KindList {
			if len(cur.Items) == 0 {
				return ""
			}
			cur = cur.Items[0]
		}
		cur, _ = cur.Get(k)
	}
	if cur == nil {
		return ""
	}
	return cur.Text
}

// summary is one line per resource: kind, name and the members a test
// reads, the Routes' path and method and the Upstream they name, the
// Upstreams' address and whether they set tls.
func summary(b *Bundle) []string {
	var out []string
	for _, r := range b.Resources {
		spec, _ := r.Doc.Get("spec")
		line := string(r.Kind) + "/" + r.Name
		switch r.Kind {
		case v1alpha1.KindRoute:
			path, _ := spec.At(diag.Path{diag.Field("match"), diag.Field("path")})
			form := path.Members[0]
			methods, _ := spec.At(diag.Path{diag.Field("match"), diag.Field("methods")})
			var ms []string
			for _, m := range methods.Items {
				ms = append(ms, m.Text)
			}
			line += " " + form.Key + "=" + form.Value.Text + " " + strings.Join(ms, ",") + " -> " + get(spec, "upstreams.name")
		case v1alpha1.KindUpstream:
			line += " " + get(spec, "endpoints.address")
			if _, ok := spec.Get("tls"); ok {
				line += " tls"
			}
		default:
		}
		out = append(out, line)
	}
	return out
}

// warnings returns the warnings of b as lines.
func warnings(b *Bundle) []string {
	var out []string
	for _, w := range b.Warnings {
		out = append(out, w.String())
	}
	return out
}

// TestImportPetstore: a Gateway edge with an http listener on 8080, one
// Upstream per server, one Route per operation, in canonical order.
func TestImportPetstore(t *testing.T) {
	b := importDoc(t, petstore)
	want := []string{
		"Gateway/edge",
		"Upstream/petstore-example petstore.example:443 tls",
		"Route/create-pets exact=/v1/pets POST -> petstore-example",
		"Route/list-pets exact=/v1/pets GET -> petstore-example",
		"Route/show-pet-by-id template=/v1/pets/{petId} GET -> petstore-example",
	}
	if got := summary(b); !slices.Equal(got, want) {
		t.Errorf("resources = %q, want %q", got, want)
	}
	if b.Operations != 3 || b.Routes != 3 || len(b.Warnings) != 0 {
		t.Errorf("operations %d routes %d warnings %q", b.Operations, b.Routes, warnings(b))
	}
	gw := b.Resources[0].Doc
	if get(gw, "spec.listeners.name") != "http" || get(gw, "spec.listeners.protocol") != "http" || get(gw, "spec.listeners.port") != "8080" {
		t.Errorf("Gateway = %+v", gw)
	}
}

// TestImportServers: an operation uses its own servers, else its path
// item's, else the document's; the first absolute http or https server of
// that list is its Upstream, and a list with other servers is warned about
// once; each scheme, host and port is one Upstream, named after the host
// and a port other than the scheme's.
func TestImportServers(t *testing.T) {
	b := importDoc(t, `openapi: 3.0.3
servers:
  - url: /relative
  - url: https://api.example.com
  - url: https://sandbox.example.com
paths:
  /a:
    get: {operationId: rootServer}
    put: {operationId: rootServerAgain}
  /b:
    servers: [{url: "http://item.example:8080/base/"}]
    get: {operationId: itemServer}
    post:
      operationId: opServer
      servers: [{url: "http://api.example.com"}]
`)
	want := []string{
		"Gateway/edge",
		"Upstream/api-example-com api.example.com:443 tls",
		"Upstream/api-example-com-2 api.example.com:80",
		"Upstream/item-example-8080 item.example:8080",
		"Route/item-server exact=/base/b GET -> item-example-8080",
		"Route/op-server exact=/b POST -> api-example-com-2",
		"Route/root-server exact=/a GET -> api-example-com",
		"Route/root-server-again exact=/a PUT -> api-example-com",
	}
	if got := summary(b); !slices.Equal(got, want) {
		t.Errorf("resources = %q, want %q", got, want)
	}
	if got := warnings(b); !slices.Equal(got, []string{`a.yaml:3:10: only server "https://api.example.com" of this servers list is imported; 2 more are not`}) {
		t.Errorf("warnings = %q", got)
	}
}

// TestImportWithoutServer: an operation without an absolute server is
// left out with a warning naming why: no servers at all, or a relative
// one; the other operations are imported.
func TestImportWithoutServer(t *testing.T) {
	b := importDoc(t, `openapi: 3.0.3
paths:
  /a:
    get: {operationId: none}
  /b:
    servers: [{url: /v1}]
    get: {operationId: relative}
  /c:
    servers: [{url: "https://c.example"}]
    get: {operationId: absolute}
`)
	if got := summary(b); !slices.Equal(got, []string{"Gateway/edge", "Upstream/c-example c.example:443 tls", "Route/absolute exact=/c GET -> c-example"}) {
		t.Errorf("resources = %q", got)
	}
	want := []string{
		"a.yaml:4:5: GET /a is not imported: the document names no server, so no Upstream address",
		`a.yaml:7:5: GET /b is not imported: server URL "/v1" is relative: it names no host`,
	}
	if got := warnings(b); !slices.Equal(got, want) {
		t.Errorf("warnings = %q, want %q", got, want)
	}
	if b.Operations != 3 || b.Routes != 1 {
		t.Errorf("operations %d routes %d", b.Operations, b.Routes)
	}
}

// TestRoutePath: a path without parameters is exact; one whose parameters
// each fill a segment is a template, names made template names; one with
// a parameter beside text in a segment is a regex; a path the Router
// cannot match is a reason.
func TestRoutePath(t *testing.T) {
	tests := []struct {
		name, path string
		want       v1alpha1.PathMatch
		why        string
	}{
		{name: "the root", path: "/", want: v1alpha1.PathMatch{Exact: "/"}},
		{name: "a literal path", path: "/pets", want: v1alpha1.PathMatch{Exact: "/pets"}},
		{name: "a trailing slash", path: "/pets/", want: v1alpha1.PathMatch{Exact: "/pets/"}},
		{name: "a parameter", path: "/pets/{petId}", want: v1alpha1.PathMatch{Template: "/pets/{petId}"}},
		{name: "a parameter and a trailing slash", path: "/pets/{petId}/", want: v1alpha1.PathMatch{Template: "/pets/{petId}/"}},
		{name: "a hyphenated name", path: "/pets/{pet-id}", want: v1alpha1.PathMatch{Template: "/pets/{pet_id}"}},
		{name: "a dotted name", path: "/u/{user.name}", want: v1alpha1.PathMatch{Template: "/u/{user_name}"}},
		{name: "a leading digit", path: "/v/{1st}", want: v1alpha1.PathMatch{Template: "/v/{_1st}"}},
		{name: "a non-ASCII name", path: "/v/{ид}", want: v1alpha1.PathMatch{Template: "/v/{__}"}},
		{name: "names that collide", path: "/{a-b}/{a_b}", want: v1alpha1.PathMatch{Template: "/{a_b}/{a_b_2}"}},
		{name: "a parameter beside text", path: "/files/{name}.json", want: v1alpha1.PathMatch{Regex: `/files/[^/]+\.json`}},
		{name: "two parameters in a segment", path: "/{a}-{b}", want: v1alpha1.PathMatch{Regex: `/[^/]+-[^/]+`}},
		{name: "a regex with an empty segment", path: "/a//{b}", want: v1alpha1.PathMatch{Regex: `/a//[^/]+`}},
		{name: "regex metacharacters", path: "/a+b/{c}(d)", want: v1alpha1.PathMatch{Regex: `/a\+b/[^/]+\(d\)`}},
		{name: "an unbalanced brace", path: "/a/{b", why: "has an unbalanced { or } or an empty {}"},
		{name: "a closing brace", path: "/a/b}", why: "has an unbalanced"},
		{name: "an empty name", path: "/a/{}", why: "an empty {}"},
		{name: "a NUL escape", path: "/a/%00", why: "the Router cannot match the path"},
		{name: "a NUL escape beside a parameter", path: "/a/{b}%00", why: `the piece "%00" is not a valid path segment`},
		{name: "a dot segment beside a parameter", path: "/a/./{b}x", why: `the piece "." is not a valid path segment`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, why := routePath(tc.path)
			if tc.why != "" {
				if !strings.Contains(why, tc.why) {
					t.Errorf("routePath(%q) reason = %q, want one containing %q", tc.path, why, tc.why)
				}
				return
			}
			if why != "" || got != tc.want {
				t.Errorf("routePath(%q) = %+v, %q; want %+v", tc.path, got, why, tc.want)
			}
		})
	}
}

// TestLabel: names are RFC 1123 labels: lower case, a hyphen where other
// characters stand and where a lower-case letter or digit meets an upper
// case one or an acronym ends, none at either end, at most 63 characters.
func TestLabel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"getPetById", "get-pet-by-id"},
		{"HTTPServer", "http-server"},
		{"listPetsV1", "list-pets-v1"},
		{"GetUserByID", "get-user-by-id"},
		{"api.v1.users.list", "api-v1-users-list"},
		{"  --x--  ", "x"},
		{"get /pets/{petId}", "get-pets-pet-id"},
		{"snake_case_name", "snake-case-name"},
		{"日本語", ""},
		{"", ""},
		{strings.Repeat("a", 70), strings.Repeat("a", 63)},
		{strings.Repeat("a", 62) + "-b", strings.Repeat("a", 62)},
	}
	re := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	for _, tc := range tests {
		got := label(tc.in)
		if got != tc.want {
			t.Errorf("label(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got != "" && (!re.MatchString(got) || len(got) > maxName) {
			t.Errorf("label(%q) = %q is no RFC 1123 label", tc.in, got)
		}
	}
}

// TestImportNames: a Route takes its operationId as a label, else its
// method and path; a name taken already takes -2, -3 within 63
// characters; a name that is no label at all falls back.
func TestImportNames(t *testing.T) {
	long := strings.Repeat("x", 70)
	b := importDoc(t, `openapi: 3.0.3
servers: [{url: "https://a.example"}]
paths:
  /a:
    get: {operationId: same}
    put: {operationId: same}
    post: {operationId: Same}
    delete: {}
  /b/{id}:
    get: {operationId: `+long+`}
    put: {operationId: `+long+`}
    post: {operationId: "日本"}
`)
	var names []string
	for _, r := range b.Resources {
		if r.Kind == v1alpha1.KindRoute {
			names = append(names, r.Name)
		}
	}
	slices.Sort(names)
	want := []string{"delete-a", "post-b-id", "same", "same-2", "same-3", strings.Repeat("x", 61) + "-2", strings.Repeat("x", 63)}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("names = %q, want %q", names, want)
	}
}

// TestImportDuplicateCriteria: an operation whose method and joined path
// an earlier Route already matches, as when a server's base path makes two
// paths one, is left out with a warning.
func TestImportDuplicateCriteria(t *testing.T) {
	b := importDoc(t, `openapi: 3.0.3
servers: [{url: "https://a.example"}]
paths:
  /v1/x:
    get: {operationId: first}
  /x:
    servers: [{url: "https://a.example/v1"}]
    get: {operationId: second}
    put: {operationId: third}
`)
	if got := summary(b); !slices.Equal(got, []string{
		"Gateway/edge", "Upstream/a-example a.example:443 tls",
		"Route/first exact=/v1/x GET -> a-example", "Route/third exact=/v1/x PUT -> a-example",
	}) {
		t.Errorf("resources = %q", got)
	}
	if got := warnings(b); !slices.Equal(got, []string{`a.yaml:8:5: GET /x is not imported: Route "first" already matches the same requests`}) {
		t.Errorf("warnings = %q", got)
	}
}

// TestImportUnmatchablePath: an operation whose path the Router cannot
// match is left out with the reason.
func TestImportUnmatchablePath(t *testing.T) {
	b := importDoc(t, `openapi: 3.0.3
servers: [{url: "https://a.example"}]
paths:
  /a/{b:
    get: {operationId: broken}
`)
	if b.Routes != 0 || len(b.Warnings) != 1 || !strings.Contains(b.Warnings[0].Message, `GET /a/{b is not imported: the path "/a/{b" has an unbalanced`) {
		t.Errorf("routes %d warnings %q", b.Routes, warnings(b))
	}
}

// TestImportWebhooks: webhooks are requests the API sends, never Routes;
// the import warns once and leaves them out.
func TestImportWebhooks(t *testing.T) {
	b := importDoc(t, `openapi: 3.1.0
webhooks:
  newPet: {post: {operationId: newPet}}
  oldPet: {post: {operationId: oldPet}}
`)
	if b.Routes != 0 || b.Operations != 0 || len(b.Resources) != 1 {
		t.Errorf("resources = %q", summary(b))
	}
	if got := warnings(b); !slices.Equal(got, []string{"a.yaml:3:3: webhooks are not imported: they are requests the API sends, not requests a Route receives"}) {
		t.Errorf("warnings = %q", got)
	}
}

// TestBundleFiles: Files lays the Bundle out as ruralz.yaml with the
// Gateway and one file per Upstream and Route; Stream is the same
// documents as one YAML stream; both parse back through the restricted
// profile to the same documents.
func TestBundleFiles(t *testing.T) {
	b := importDoc(t, petstore)
	files, err := b.Files()
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	var all bytes.Buffer
	for i, f := range files {
		paths = append(paths, f.Path)
		if i > 0 {
			all.WriteString("---\n")
		}
		all.Write(f.Content)
	}
	want := []string{"ruralz.yaml", "upstreams/petstore-example.yaml", "routes/create-pets.yaml", "routes/list-pets.yaml", "routes/show-pet-by-id.yaml"}
	if !slices.Equal(paths, want) {
		t.Errorf("files = %q, want %q", paths, want)
	}
	stream, err := b.Stream()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stream, all.Bytes()) {
		t.Errorf("stream =\n%s\nwant the files joined:\n%s", stream, all.Bytes())
	}
	docs, ds, err := profile.Parse(t.Context(), stream, 0, "stream.yaml", profile.FormatYAML, profile.Options{})
	if err != nil || len(ds) > 0 || len(docs) != len(b.Resources) {
		t.Fatalf("Parse(stream) = %d documents, %v, %v", len(docs), ds, err)
	}
	if got := get(docs[4].Root, "spec.match.path.template"); got != "/v1/pets/{petId}" {
		t.Errorf("template read back = %q", got)
	}
}

// TestImportedRoutesMatch: every imported Route's path criterion passes
// the Router's own checks.
func TestImportedRoutesMatch(t *testing.T) {
	b := importDoc(t, `openapi: 3.0.3
servers: [{url: "https://a.example/base"}]
paths:
  /:
    get: {operationId: root}
  /files/{name}.{ext}:
    get: {operationId: file}
  /users/{user-id}/keys/{key.id}:
    get: {operationId: key}
`)
	for _, r := range b.Resources {
		if r.Kind != v1alpha1.KindRoute {
			continue
		}
		spec, _ := r.Doc.Get("spec")
		p, _ := spec.At(diag.Path{diag.Field("match"), diag.Field("path")})
		form, value := p.Members[0].Key, p.Members[0].Value.Text
		var err error
		switch form {
		case "exact":
			err = routematch.CheckPath(value)
		case "template":
			err = routematch.CheckTemplate(value)
		default:
			_, err = routematch.CompileRegex(value)
		}
		if err != nil {
			t.Errorf("Route %s %s %q: %v", r.Name, form, value, err)
		}
	}
	if got := summary(b); !slices.Contains(got, "Route/root exact=/base/ GET -> a-example") {
		t.Errorf("resources = %q, want the root path under the base", got)
	}
}
