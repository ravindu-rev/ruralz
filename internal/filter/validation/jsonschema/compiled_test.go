// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"
)

// libraryCompiler adds schema as the document and compiles its root, and
// first a check wrapper whose allOf names checked, as compileWalked does.
func libraryCompiler(t *testing.T, schema string, checked ...string) *sjs.Compiler {
	t.Helper()
	p, issues := prepare([]byte(schema))
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	c := sjs.NewCompiler()
	c.DefaultDraft(sjs.Draft2020)
	c.UseLoader(refusingLoader{})
	if err := c.AddResource(baseURL, p.doc); err != nil {
		t.Fatal(err)
	}
	if len(checked) > 0 {
		refs := make([]any, len(checked))
		for i, ptr := range checked {
			refs[i] = map[string]any{"$ref": baseURL + "#" + encodeFragment(ptr)}
		}
		if err := c.AddResource(checkURL, map[string]any{"allOf": refs}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Compile(checkURL); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Compile(baseURL); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestCompiledSchemas pins the listing of what the library compiled
// (compiledSchemas, audit class GC-21, rows CC-01 and CC-04): every schema
// of the document the library compiled, the schema of a resource's
// $dynamicAnchor that no edge reaches included, in pointer order, a
// non-canonical index after its canonical twin, each with the address of
// the schema the library's Compile returns for it; the check wrapper's
// schemas and a bundled meta-schema a reference reached are left out, and
// a position nothing compiled is absent. A library change that moves the
// table fails here.
func TestCompiledSchemas(t *testing.T) {
	const schema = `{"$ref":"#/$defs/a","allOf":[{"$ref":"#/allOf/00"}],"not":{"$ref":"https://json-schema.org/draft/2020-12/meta/core"},` +
		`"$defs":{"a":{"$id":"urn:a","$defs":{"d":{"$dynamicAnchor":"n","type":"string"},"e":{"type":"null"}},"properties":{"p":{"$ref":"#/$defs/e"}}},` +
		`"b":{"type":"null"},"u":{"type":"null"}}}`
	c := libraryCompiler(t, schema, "/$defs/b")
	want := []string{"", "/$defs/a", "/$defs/a/$defs/d", "/$defs/a/$defs/e", "/$defs/a/properties/p", "/$defs/b", "/allOf/0", "/allOf/00", "/not"}
	// The table is a map: each listing reads it in another order, and
	// every one must come out the same.
	for range 20 {
		got, ok := compiledSchemas(c)
		if !ok {
			t.Fatal("compiledSchemas() failed")
		}
		var ptrs []string
		for _, cs := range got {
			ptrs = append(ptrs, cs.ptr)
			s, err := c.Compile(baseURL + "#" + encodeFragment(cs.ptr))
			if err != nil || reflect.ValueOf(s).Pointer() != cs.addr {
				t.Errorf("Compile(%q) = %v, %v, not the listed schema", cs.ptr, s, err)
			}
		}
		if !slices.Equal(ptrs, want) {
			t.Fatalf("compiled = %q, want %q", ptrs, want)
		}
	}
	// The anchor schema is compiled, but no edge leads to it.
	root, _ := c.Compile(baseURL)
	in := newInspector()
	in.visit(root, "")
	for _, n := range in.nodes {
		if n.ptr == "/$defs/a/$defs/d" {
			t.Error("the root reaches the anchor schema; the shape no longer shows the listing's need")
		}
	}
}

// TestCompiledInRefuses covers the fail-closed listing (audit class GC-21):
// a compiler value whose table is not a map from location to *Schema, or
// holds a nil schema, is not listed, and a well-formed one is.
func TestCompiledInRefuses(t *testing.T) {
	s := &sjs.Schema{Location: baseURL + "#/q"}
	for _, tc := range []struct {
		name string
		v    reflect.Value
		want []string
	}{
		{"not a struct", reflect.ValueOf(5), nil},
		{"no table", reflect.ValueOf(struct{ other int }{}), nil},
		{"table not a map", reflect.ValueOf(struct{ schemas []int }{}), nil},
		{"table of other values", reflect.ValueOf(struct{ schemas map[string]int }{}), nil},
		{"nil schema", reflect.ValueOf(struct{ schemas map[string]*sjs.Schema }{map[string]*sjs.Schema{"a": nil}}), nil},
		{"well formed", reflect.ValueOf(struct{ schemas map[string]*sjs.Schema }{map[string]*sjs.Schema{
			"a": s, "b": {Location: checkURL + "#"}, "c": {Location: baseURL + "#"},
		}}), []string{"", "/q"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := compiledIn(tc.v)
			if ok != (tc.want != nil) {
				t.Fatalf("compiledIn() = %v, %v", got, ok)
			}
			var ptrs []string
			for _, cs := range got {
				ptrs = append(ptrs, cs.ptr)
			}
			if !slices.Equal(ptrs, tc.want) {
				t.Errorf("compiledIn() = %q, want %q", ptrs, tc.want)
			}
		})
	}
}

// TestInspectCompiledFailsClosed covers the fail-closed paths of
// inspectCompiled (audit class GC-21): when the listing failed, or lists
// a schema the library's Compile does not return as listed (a position it
// had not compiled, or one that does not resolve), the document is refused
// with msgUnlisted, so no compiled schema goes unchecked; the true listing
// passes and adds the schemas the starts do not reach.
func TestInspectCompiledFailsClosed(t *testing.T) {
	const schema = `{"$ref":"#/$defs/a","$defs":{"a":{"$id":"urn:a","$defs":{"d":{"$dynamicAnchor":"n"}}},"b":{"type":"null"}}}`
	unlisted := []Issue{{Message: msgUnlisted}}
	for _, tc := range []struct {
		name     string
		compiled func([]compiledSchema) []compiledSchema
		listed   bool
		want     []Issue
	}{
		{"listing failed", func(l []compiledSchema) []compiledSchema { return l }, false, unlisted},
		{"not compiled", func(l []compiledSchema) []compiledSchema { return append(l, compiledSchema{"/$defs/b", 1}) }, true, unlisted},
		{"wrong address", func(l []compiledSchema) []compiledSchema { return append(l, compiledSchema{"/$defs/a/$defs/d", 1}) }, true, unlisted},
		{"does not resolve", func(l []compiledSchema) []compiledSchema { return append(l, compiledSchema{"/nope", 1}) }, true, unlisted},
		{"true listing", func(l []compiledSchema) []compiledSchema { return l }, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := libraryCompiler(t, schema)
			root, _ := c.Compile(baseURL)
			list, _ := compiledSchemas(c)
			p, _ := prepare([]byte(schema))
			w := newWalker(p.doc)
			w.walk()
			in, starts, issues := inspectCompiled(c, w, []*sjs.Schema{root}, tc.compiled(list), tc.listed)
			if !slices.Equal(issues, tc.want) {
				t.Fatalf("issues = %+v, want %+v", issues, tc.want)
			}
			if tc.want != nil {
				return
			}
			var ptrs []string
			for _, s := range starts {
				ptr, _ := pointerOf(s.Location)
				ptrs = append(ptrs, ptr)
			}
			if want := []string{"", "/$defs/a/$defs/d"}; !slices.Equal(ptrs, want) || in == nil {
				t.Errorf("starts = %q, want %q", ptrs, want)
			}
		})
	}
}

// TestCheckCollected covers the $schema rule at the positions the library
// reads a $schema the walk did not collect (checkCollected, the backstop of
// audit classes GC-21, rows CC-01 and CC-04, and GC-02; 07 req 75), with
// the walk's own finding: every form the walk refuses (another draft, the
// policy document, the draft URL with an empty fragment, a value that is
// not a string, at a position with no $id), each once although several
// compiled positions hold it, in pointer order within one compiled
// position's tree and across them, and at most MaxIssues; and nothing the
// library never reads: a $schema of the draft itself, one in a const
// value, and one of a position whose subschema a reference names but which
// the library never collects. Each document places its schema on the
// base-mismatch route. Injected: a walker that has not collected the
// route, as before class GC-02 the walk did not, so the backstop finds
// it. Live: the walk now collects the route itself (class GC-02), so the
// compile refuses each with the same findings before the library runs,
// and accepts the accepted rows (decision 24). Every $schema here is one
// the library resolves without the loader.
func TestCheckCollected(t *testing.T) {
	const policy = `"$schema":"` + baseURL + `"`
	var many []string
	for i := range MaxIssues + 5 {
		many = append(many, fmt.Sprintf(`"p%02d":{%s}`, i, policy))
	}
	var manyWant []string
	for i := range MaxIssues {
		manyWant = append(manyWant, fmt.Sprintf("/properties/p%02d/$schema", i))
	}
	for _, tc := range []struct {
		name, resource string
		// want lists the pointers of the rule's findings below the
		// position on the route; none means the rule passes it.
		want []string
	}{
		{"another draft", `{"$id":"urn:w","$schema":"https://json-schema.org/draft/2019-09/schema"}`, []string{"/$schema"}},
		{"the policy document", `{"$id":"urn:w",` + policy + `,"type":"integer"}`, []string{"/$schema"}},
		{"empty fragment", `{"$id":"urn:w","$schema":"` + Draft + `#"}`, []string{"/$schema"}},
		{"not a string", `{"$id":"urn:w","$schema":5}`, []string{"/$schema"}},
		{"no $id", `{"properties":{"a":{` + policy + `}}}`, []string{"/properties/a/$schema"}},
		{"once below nested compiled positions", `{"properties":{"a":{"properties":{"b":{` + policy + `}}}}}`, []string{"/properties/a/properties/b/$schema"}},
		{"pointer order and the cap", `{"properties":{` + strings.Join(many, ",") + `}}`, manyWant},
		{"pointer order across compiled positions", `{"$id":"urn:x","$ref":"#/foo","properties":{"a":{` + policy + `}},"foo":{` + policy + `}}`, []string{"/foo/$schema", "/properties/a/$schema"}},
		{"the draft", `{"$id":"urn:w","$schema":"` + Draft + `","type":"integer"}`, nil},
		{"in a const value", `{"const":{` + policy + `}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, at := onRoute(mismatchRoute, "", "", tc.resource)
			p, issues := prepare([]byte(doc))
			if len(issues) > 0 {
				t.Fatal(issues)
			}
			var want []Issue
			for _, ptr := range tc.want {
				want = append(want, Issue{at + ptr, msgDialectText})
			}
			listed := []compiledSchema{{ptr: at}}
			if strings.Contains(tc.resource, `"#/foo"`) {
				listed = append(listed, compiledSchema{ptr: at + "/foo"})
			}
			if got := newWalker(p.doc).checkCollected(listed); !slices.Equal(got, want) {
				t.Errorf("injected: checkCollected = %+v, want %+v", got, want)
			}
			s, _, live := compileRaw([]byte(doc), refusingLoader{})
			slices.SortFunc(live, func(a, b Issue) int { return comparePointers(a.Pointer, b.Pointer) })
			if (s != nil) != (want == nil) || !slices.Equal(live, want) {
				t.Errorf("live: compile = %v, %+v, want the walk's %+v", s != nil, live, want)
			}
		})
	}
	// A $schema the library never reads: the reference names a subschema
	// of /x, whose keyword tree the library never collects.
	const neverCollected = `{"$ref":"#/x/y","x":{"$id":"urn:x","$schema":"http://json-schema.org/draft-07/schema#","y":{"type":"string"}}}`
	s, _, issues := compileRaw([]byte(neverCollected), refusingLoader{})
	if s == nil || len(issues) > 0 {
		t.Fatalf("compile(never collected) = %+v", issues)
	}
	if _, ok := s.Validate(decodeInstance(t, `"s"`)); !ok {
		t.Error(`never collected: Validate("s") = false, want true`)
	}
}

// TestCheckCollectedSkipsWalked pins that checkCollected leaves a listed
// position the walk walked to the walk, whose own $schema rule checked its
// keyword tree, and checks one the walk did not (audit class GC-21).
func TestCheckCollectedSkipsWalked(t *testing.T) {
	p, _ := prepare([]byte(`{"x":{"$schema":"urn:other"}}`))
	listed := []compiledSchema{{ptr: "/x"}}
	w := newWalker(p.doc)
	if got := w.checkCollected(listed); !slices.Equal(got, []Issue{{"/x/$schema", msgDialectText}}) {
		t.Errorf("not walked: %+v", got)
	}
	for _, how := range []uint8{walkedInert, walkedLive} {
		w := newWalker(p.doc)
		w.seen["/x"] = how
		if got := w.checkCollected(listed); len(got) > 0 {
			t.Errorf("walked %d: %+v, want none", how, got)
		}
	}
}
