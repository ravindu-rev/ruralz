// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"errors"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestParseRef: a $ref is a relative file, a JSON Pointer fragment or
// both, percent decoded, the pointer's ~1 and ~0 undone; everything that
// would leave the document's files is refused with its reason.
func TestParseRef(t *testing.T) {
	tests := []struct {
		name, ref string
		file      string
		pointer   []string
		err       error
	}{
		{name: "a local pointer", ref: "#/components/schemas/Pet", pointer: []string{"components", "schemas", "Pet"}},
		{name: "the root", ref: "#"},
		{name: "a file", ref: "common.yaml", file: "common.yaml"},
		{name: "a file and a pointer", ref: "../shared/common.yaml#/Pet", file: "../shared/common.yaml", pointer: []string{"Pet"}},
		{name: "a dot file", ref: "./a:b.yaml", file: "./a:b.yaml"},
		{name: "escapes in a pointer", ref: "#/a~1b~0c/~01", pointer: []string{"a/b~c", "~1"}},
		{name: "a percent-encoded pointer", ref: "#/paths/~1pets~1%7BpetId%7D", pointer: []string{"paths", "/pets/{petId}"}},
		{name: "a percent-encoded file", ref: "my%20file.yaml", file: "my file.yaml"},
		{name: "an empty token", ref: "#/a//b", pointer: []string{"a", "", "b"}},
		{name: "empty", ref: "", err: errRefEmpty},
		{name: "an http URL", ref: "http://example.com/a.yaml#/x", err: errRefScheme},
		{name: "an https URL", ref: "https://example.com/a.yaml", err: errRefScheme},
		{name: "a file URL", ref: "file:///etc/a.yaml", err: errRefScheme},
		{name: "a URN", ref: "urn:example:a", err: errRefScheme},
		{name: "a drive letter", ref: "C:/a.yaml", err: errRefScheme},
		{name: "a colon in the first segment", ref: "a:b.yaml", err: errRefScheme},
		{name: "an absolute path", ref: "/etc/a.yaml", err: errRefAbsolute},
		{name: "a network path", ref: "//host/a.yaml", err: errRefAbsolute},
		{name: "a backslash", ref: `schemas\a.yaml`, err: errRefBackslash},
		{name: "an encoded absolute path", ref: "%2Fetc%2Fa.yaml", err: errRefAbsolute},
		{name: "an encoded backslash", ref: "schemas%5Ca.yaml", err: errRefBackslash},
		{name: "an encoded drive letter", ref: "C%3A/a.yaml", err: errRefScheme},
		{name: "a query", ref: "a.yaml?v=1", err: errRefQuery},
		{name: "a bad file escape", ref: "a%zz.yaml", err: errRefEscape},
		{name: "a bad fragment escape", ref: "#/a%z", err: errRefEscape},
		{name: "a NUL byte", ref: "a%00.yaml", err: errRefNUL},
		{name: "a plain-name fragment", ref: "#Pet", err: errRefPlainName},
		{name: "a bad tilde", ref: "#/a~2b", err: errRefTilde},
		{name: "a trailing tilde", ref: "#/a~", err: errRefTilde},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRef(tc.ref)
			if !errors.Is(err, tc.err) {
				t.Fatalf("parseRef(%q) error = %v, want %v", tc.ref, err, tc.err)
			}
			if err == nil && (got.file != tc.file || !slices.Equal(got.pointer, tc.pointer)) {
				t.Errorf("parseRef(%q) = %+v, want file %q pointer %q", tc.ref, got, tc.file, tc.pointer)
			}
		})
	}
}

// TestPointerRoundTrip: pointerText writes the tokens parseRef reads back.
func TestPointerRoundTrip(t *testing.T) {
	for _, tokens := range [][]string{nil, {""}, {"a/b", "~", "~1", "x y"}, {"paths", "/pets/{id}"}} {
		r, err := parseRef("#" + pointerText(tokens))
		if err != nil || !slices.Equal(r.pointer, tokens) {
			t.Errorf("parseRef(#%s) = %q, %v; want %q", pointerText(tokens), r.pointer, err, tokens)
		}
	}
}

// TestWalk: a JSON Pointer names a mapping member or a list index written
// in decimal without a leading zero, and nothing past a scalar.
func TestWalk(t *testing.T) {
	root := mapNode("a", listNode(strNode("x"), strNode("y")), "b", mapNode("c", strNode("z")), "s", strNode("v"))
	tests := []struct {
		name   string
		tokens []string
		want   string // the string the pointer names; empty when it names nothing
		bad    int
	}{
		{name: "a member", tokens: []string{"b", "c"}, want: "z", bad: -1},
		{name: "an index", tokens: []string{"a", "1"}, want: "y", bad: -1},
		{name: "index zero", tokens: []string{"a", "0"}, want: "x", bad: -1},
		{name: "a leading zero", tokens: []string{"a", "01"}, bad: 1},
		{name: "a negative index", tokens: []string{"a", "-1"}, bad: 1},
		{name: "past the end", tokens: []string{"a", "2"}, bad: 1},
		{name: "a missing member", tokens: []string{"b", "d"}, bad: 1},
		{name: "past a scalar", tokens: []string{"s", "x"}, bad: 1},
		{name: "an index on a mapping", tokens: []string{"b", "0"}, bad: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n, bad := walk(root, tc.tokens)
			got := ""
			if n != nil {
				got = n.Text
			}
			if got != tc.want || bad != tc.bad {
				t.Errorf("walk(%q) = %q, %d; want %q, %d", tc.tokens, got, bad, tc.want, tc.bad)
			}
		})
	}
	if n, bad := walk(root, nil); n != root || bad != -1 {
		t.Errorf("walk(root) = %v, %d", n, bad)
	}
}

// TestResolveFiles: a reference names a node of its own file, of a file
// below or above the referring file's directory, and references inside a
// referenced file resolve from that file's directory; each file is read
// once however often it is named.
func TestResolveFiles(t *testing.T) {
	m := newMemFS(map[string]string{
		"api/openapi.yaml": `openapi: 3.0.3
paths:
  /local: {$ref: "#/components/pathItems/Local"}
  /below: {$ref: "items/below.yaml"}
  /above: {$ref: "../shared/above.yaml#/Item"}
  /again: {$ref: "items/below.yaml"}
components:
  pathItems:
    Local: {get: {operationId: local}}
`,
		"api/items/below.yaml":  "parameters: [{$ref: 'params.yaml#/Limit'}]\nget: {operationId: below}\n",
		"api/items/params.yaml": "Limit: {name: limit, in: query}\n",
		"shared/above.yaml":     "Item: {get: {operationId: above}}\n",
	})
	doc, err := Load(t.Context(), "api/openapi.yaml", Options{FS: m})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range doc.Paths {
		got = append(got, p.Key+"="+p.Item.Operations[0].OperationID)
	}
	if want := []string{"/local=local", "/below=below", "/above=above", "/again=below"}; !slices.Equal(got, want) {
		t.Errorf("paths = %q, want %q", got, want)
	}
	if ps := doc.Paths[1].Item.Parameters; len(ps) != 1 || ps[0].Name != "limit" {
		t.Errorf("parameters of /below = %+v, want limit from params.yaml beside below.yaml", ps)
	}
	if n := m.readsOf("api/items/below.yaml"); n != 1 {
		t.Errorf("below.yaml read %d times, want once", n)
	}
	want := []string{"api/openapi.yaml", "api/items/below.yaml", "api/items/params.yaml", "shared/above.yaml"}
	if !slices.Equal(doc.Files, want) {
		t.Errorf("files = %q, want %q", doc.Files, want)
	}
}

// TestResolveProblems: a reference that does not parse, a file that
// cannot be read, a pointer that names nothing, a chain that returns to a
// node it passed and a $ref that is not a string are each a problem at
// the reference; a file that fails is reported once.
func TestResolveProblems(t *testing.T) {
	tests := []struct {
		name, paths, want string
		extra             map[string]string
	}{
		{name: "a URL", paths: `/a: {$ref: "https://example.com/a.yaml"}`, want: `a.yaml:3:14: $ref "https://example.com/a.yaml" is a URI with a scheme`},
		{name: "a missing file", paths: `/a: {$ref: "missing.yaml"}`, want: "a.yaml:3:14: cannot read missing.yaml: "},
		{name: "a missing pointer", paths: `/a: {$ref: "#/components/pathItems/X"}`, want: `a.yaml:3:14: $ref "#/components/pathItems/X" names nothing: a.yaml holds no "/components"`},
		{name: "a missing pointer in a file", paths: `/a: {$ref: "b.yaml#/Item/get"}`, want: `$ref "b.yaml#/Item/get" names nothing: b.yaml holds no "/Item/get"`, extra: map[string]string{"b.yaml": "Item: {}\n"}},
		{name: "a cycle", paths: `/a: {$ref: "#/x"}`, want: `a.yaml:5:11: $ref "#/x" closes a reference cycle that names no object`},
		{name: "a self reference", paths: `/a: {$ref: "#/z"}`, want: `$ref "#/z" closes a reference cycle`},
		{name: "a ref number", paths: `/a: {$ref: 1}`, want: "a.yaml:3:14: $ref is a number, not a string"},
		{name: "a file that fails", paths: "/a: {$ref: \"bad.yaml\"}\n  /b: {$ref: \"bad.yaml\"}", want: "bad.yaml:1:", extra: map[string]string{"bad.yaml": "a: &x 1\n"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"a.yaml": "openapi: 3.0.3\npaths:\n  " + tc.paths + "\nx: {$ref: '#/y'}\ny: {$ref: '#/x'}\nz: {$ref: '#/z'}\n"}
			for k, v := range tc.extra {
				files[k] = v
			}
			_, err := loadFiles(t, "a.yaml", files, Options{})
			ps := problems(t, err)
			if !hasProblem(ps, tc.want) {
				t.Errorf("problems = %q, want one containing %q", ps, tc.want)
			}
			if tc.name == "a file that fails" && len(ps) != 1 {
				t.Errorf("problems = %q, want the failed file's problem once", ps)
			}
		})
	}
}

// TestDerefChain: a chain of Reference Objects ends at the object it
// leads to, members beside $ref ignored, and a node that is no Reference
// Object is its own target.
func TestDerefChain(t *testing.T) {
	doc := mustLoad(t, "a.yaml", map[string]string{"a.yaml": `openapi: 3.0.3
paths:
  /a: {$ref: "#/x", summary: ignored}
x: {$ref: "#/y"}
y: {$ref: "b.yaml"}
`, "b.yaml": "summary: target\nget: {operationId: getA}\n"})
	if item := doc.Paths[0].Item; item.Summary != "target" || item.Operations[0].OperationID != "getA" {
		t.Errorf("item = %+v, want the end of the chain", item)
	}
	l := newLoad(t.Context(), Options{})
	n := strNode("plain")
	if got, err := l.deref(n); got != n || err != nil {
		t.Errorf("deref(scalar) = %v, %v", got, err)
	}
	if got, err := l.deref(mapNode("a", strNode("b"))); got == nil || err != nil {
		t.Errorf("deref(mapping without $ref) = %v, %v", got, err)
	}
}

// TestResolveOutsideTheLoad: a reference written at a node no file of the
// load holds is a problem, never a read.
func TestResolveOutsideTheLoad(t *testing.T) {
	m := newMemFS(nil)
	l := newLoad(t.Context(), Options{FS: m})
	at := &tree.Node{Kind: tree.KindString, Text: "b.yaml", Pos: tree.Pos{File: 7, Line: 1, Column: 1}}
	if _, ok, err := l.resolve(at); ok || err != nil || len(l.problems) != 1 {
		t.Errorf("resolve = %v, %v; problems %+v", ok, err, l.problems)
	}
	if len(m.reads) != 0 {
		t.Errorf("reads = %v, want none", m.reads)
	}
}
