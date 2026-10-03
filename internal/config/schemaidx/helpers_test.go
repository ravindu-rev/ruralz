// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// renderedEnv names an alternative rendered view for the tests, such as
// the schema of another commit (RURALZ_TEST_SCHEMAIDX_RENDERED=<file>):
// the markers every test checks are read from the schema at run time,
// never from a hard-coded list (R-62), so the suite holds for whichever
// WP-28 schema is committed.
const renderedEnv = "RURALZ_TEST_SCHEMAIDX_RENDERED"

func renderedBytes(t testing.TB) []byte {
	t.Helper()
	path := os.Getenv(renderedEnv)
	if path == "" {
		return schema.RenderedV1alpha1()
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: a test input chosen by the developer.
	if err != nil {
		t.Fatalf("%s: %v", renderedEnv, err)
	}
	return b
}

// embedded loads the rendered view once per test binary.
var embedded = sync.OnceValues(func() (*Index, error) {
	path := os.Getenv(renderedEnv)
	if path == "" {
		return Load("ruralz/v1alpha1", schema.RenderedV1alpha1())
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: a test input chosen by the developer.
	if err != nil {
		return nil, err
	}
	return Load("ruralz/v1alpha1", b)
})

func v1(t testing.TB) *Index {
	t.Helper()
	x, err := embedded()
	if err != nil {
		t.Fatalf("Load(embedded rendered view): %v", err)
	}
	return x
}

func mustLoad(t testing.TB, src string) *Index {
	t.Helper()
	x, err := Load("test/v1", []byte(src))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return x
}

// parseTree converts JSON text to a tree, keeping member order; integers
// become KindInt, other numbers KindFloat.
func parseTree(t testing.TB, src string) *tree.Node {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(src))
	dec.UseNumber()
	n, err := treeValue(dec)
	if err != nil {
		t.Fatalf("parseTree(%s): %v", src, err)
	}
	return n
}

func treeValue(dec *json.Decoder) (*tree.Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			n := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				val, err := treeValue(dec)
				if err != nil {
					return nil, err
				}
				n.Members = append(n.Members, tree.Member{Key: kt.(string), Value: val})
			}
			_, err := dec.Token()
			return n, err
		case '[':
			n := &tree.Node{Kind: tree.KindList, Items: []*tree.Node{}}
			for dec.More() {
				val, err := treeValue(dec)
				if err != nil {
					return nil, err
				}
				n.Items = append(n.Items, val)
			}
			_, err := dec.Token()
			return n, err
		}
	case string:
		return &tree.Node{Kind: tree.KindString, Text: v}, nil
	case json.Number:
		if isIntegerLiteral(string(v)) {
			return &tree.Node{Kind: tree.KindInt, Text: string(v)}, nil
		}
		return &tree.Node{Kind: tree.KindFloat, Text: string(v)}, nil
	case bool:
		return &tree.Node{Kind: tree.KindBool, Bool: v}, nil
	case nil:
		return &tree.Node{Kind: tree.KindNull}, nil
	}
	return nil, errors.New("unexpected token")
}

// rawDoc decodes the committed rendered view independently of the index.
func rawDoc(t testing.TB) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(renderedBytes(t)))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return doc
}

// rawSchemas calls fn for every schema object of doc with its JSON
// pointer, skipping the subschemas the index does not navigate: if
// conditions, not, and anyOf and oneOf branches.
func rawSchemas(doc any, fn func(ptr string, m map[string]any)) {
	var walk func(v any, ptr string)
	walk = func(v any, ptr string) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		fn(ptr, m)
		for k, val := range m {
			p := ptr + "/" + escapeToken(k)
			switch k {
			case "properties", "$defs":
				if mm, ok := val.(map[string]any); ok {
					for name, s := range mm {
						walk(s, p+"/"+escapeToken(name))
					}
				}
			case "items", "additionalProperties", "then", "else":
				walk(val, p)
			case "allOf":
				if l, ok := val.([]any); ok {
					for i, s := range l {
						walk(s, p+"/"+strconv.Itoa(i))
					}
				}
			default:
			}
		}
	}
	walk(doc, "#")
}

// instanceFor builds a minimal resource tree and instance path reaching
// schema path f.Path, with every dispatch condition of f.Where set on its
// object.
func instanceFor(f Field) (*tree.Node, diag.Path) {
	root := &tree.Node{Kind: tree.KindMap}
	objects := map[string]*tree.Node{"": root}
	var path diag.Path
	cur, sp := root, ""
	for _, seg := range splitSchemaPath(f.Path) {
		var next *tree.Node
		switch seg {
		case "[]":
			next = &tree.Node{Kind: tree.KindMap}
			cur.Kind, cur.Items = tree.KindList, []*tree.Node{next}
			path = append(path, diag.Index(0))
			sp += "[]"
		case "{}":
			next = &tree.Node{Kind: tree.KindMap}
			cur.Kind = tree.KindMap
			cur.Set("k", tree.Pos{}, next)
			path = append(path, diag.Field("k"))
			sp += "{}"
		default:
			next = &tree.Node{Kind: tree.KindMap}
			cur.Kind = tree.KindMap
			cur.Set(seg, tree.Pos{}, next)
			path = append(path, diag.Field(seg))
			sp = joinPath(sp, seg)
		}
		objects[sp] = next
		cur = next
	}
	for _, d := range f.Where {
		obj := objects[d.Path]
		for _, c := range d.Conditions {
			obj.Kind = tree.KindMap
			obj.Set(c.Member, tree.Pos{}, scalarTree(c.Value))
		}
	}
	return root, path
}

func scalarTree(v any) *tree.Node {
	switch t := v.(type) {
	case string:
		return &tree.Node{Kind: tree.KindString, Text: t}
	case bool:
		return &tree.Node{Kind: tree.KindBool, Bool: t}
	case json.Number:
		return &tree.Node{Kind: tree.KindFloat, Text: string(t)}
	default:
		return &tree.Node{Kind: tree.KindNull}
	}
}

// plainNames reports whether every member name the index declares is
// non-empty and free of the schema path punctuation ".[]{}", so that
// splitSchemaPath recovers each schema path. Generated schemas declare Go
// JSON field names; a fuzzed one may declare "" or "a.b", whose joined
// paths are ambiguous.
func plainNames(x *Index) bool {
	for _, n := range x.memo {
		for _, name := range n.names {
			if name == "" || strings.ContainsAny(name, ".[]{}") {
				return false
			}
		}
	}
	return true
}

// joinPath appends member name to schema path base.
func joinPath(base, name string) string {
	if base == "" {
		return name
	}
	return base + "." + name
}

// splitSchemaPath splits "spec.a[].b{}" into "spec", "a", "[]", "b", "{}".
func splitSchemaPath(p string) []string {
	var out []string
	var name strings.Builder
	flush := func() {
		if name.Len() > 0 {
			out = append(out, name.String())
			name.Reset()
		}
	}
	for i := 0; i < len(p); i++ {
		switch {
		case strings.HasPrefix(p[i:], "[]"):
			flush()
			out = append(out, "[]")
			i++
		case strings.HasPrefix(p[i:], "{}"):
			flush()
			out = append(out, "{}")
			i++
		case p[i] == '.':
			flush()
		default:
			name.WriteByte(p[i])
		}
	}
	flush()
	return out
}

func partPtrs(n *Node) []string {
	out := make([]string, len(n.parts))
	for i, p := range n.parts {
		out[i] = p.ptr
	}
	return out
}

// pathOf builds a path: a string is a member, an int an index.
func pathOf(elems ...any) diag.Path {
	var p diag.Path
	for _, e := range elems {
		switch v := e.(type) {
		case string:
			p = append(p, diag.Field(v))
		case int:
			p = append(p, diag.Index(v))
		case diag.PathElem:
			p = append(p, v)
		}
	}
	return p
}
