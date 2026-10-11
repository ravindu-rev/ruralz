// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// TestEscapeToken covers the RFC 6901 escapes of the keyword locations in
// details and findings (07 reqs 75 and 81).
func TestEscapeToken(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"a", "a"},
		{"a/b", "a~1b"},
		{"m~n", "m~0n"},
		{"~/~1", "~0~1~01"},
	} {
		if got := escapeToken(tc.in); got != tc.want {
			t.Errorf("escapeToken(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if back, ok := unescapeToken(tc.want); !ok || back != tc.in {
			t.Errorf("unescapeToken(%q) = %q, %v, want %q", tc.want, back, ok, tc.in)
		}
	}
	for _, bad := range []string{"~", "a~", "~2", "x~a"} {
		if got, ok := unescapeToken(bad); ok {
			t.Errorf("unescapeToken(%q) = %q, true, want false", bad, got)
		}
	}
}

// TestSplitPointer covers splitting a JSON Pointer into its tokens, for
// the keyword locations of 07 reqs 75 and 81.
func TestSplitPointer(t *testing.T) {
	if toks, ok := splitPointer(""); !ok || toks != nil {
		t.Errorf(`splitPointer("") = %v, %v`, toks, ok)
	}
	if toks, ok := splitPointer("/a//b"); !ok || !slices.Equal(toks, []string{"a", "", "b"}) {
		t.Errorf(`splitPointer("/a//b") = %q, %v`, toks, ok)
	}
	if _, ok := splitPointer("a/b"); ok {
		t.Error(`splitPointer("a/b") ok, want false`)
	}
}

// TestEncodeFragmentAndPointerOf covers the mapping between JSON Pointers
// and the library's schema locations (07 reqs 75 and 81).
func TestEncodeFragmentAndPointerOf(t *testing.T) {
	for _, p := range []string{"", "/a", "/properties/a b", "/properties/é%2F", "/patternProperties/^x~1y$", "/$defs/#"} {
		frag := encodeFragment(p)
		got, ok := pointerOf(baseURL + "#" + frag)
		if !ok || got != p {
			t.Errorf("pointerOf(encodeFragment(%q) = %q) = %q, %v", p, frag, got, ok)
		}
	}
	for _, loc := range []string{"https://json-schema.org/draft/2020-12/schema#", baseURL, baseURL + "#a", baseURL + "#%zz"} {
		if got, ok := pointerOf(loc); ok {
			t.Errorf("pointerOf(%q) = %q, true, want false", loc, got)
		}
	}
}

// TestLookup covers resolving a JSON Pointer in a schema document as the
// library does, for references (07 req 75).
func TestLookup(t *testing.T) {
	var doc any
	if err := json.Unmarshal([]byte(`{"a":{"b/c":[10,{"~":true}]},"":1}`), &doc); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		p  string
		ok bool
	}{
		{"", true},
		{"/a", true},
		{"/a/b~1c/0", true},
		{"/a/b~1c/1/~0", true},
		{"/", true},
		{"/a/b~1c/2", false},
		{"/a/b~1c/-1", false},
		{"/a/b~1c/x", false},
		{"/a/missing", false},
		{"/a/b~1c/0/deeper", false},
		{"/a/~2", false},
		{"a", false},
	} {
		if _, ok := lookup(doc, tc.p); ok != tc.ok {
			t.Errorf("lookup(%q) ok = %v, want %v", tc.p, ok, tc.ok)
		}
	}
}

// TestComparePointers covers the pointer order that picks the first
// failing keyword deterministically (07 req 81).
func TestComparePointers(t *testing.T) {
	ordered := []string{"", "/additionalProperties", "/allOf/2/type", "/allOf/10/type", "/allOf/10/type/x", "/allOf/x", "/properties/a", "/properties/b/type", "/required"}
	for i, a := range ordered {
		for j, b := range ordered {
			got := comparePointers(a, b)
			switch {
			case i < j && got >= 0, i > j && got <= 0, i == j && got != 0:
				t.Errorf("comparePointers(%q, %q) = %d", a, b, got)
			}
		}
	}
	if comparePointers("/a/007", "/a/7") != 0 {
		t.Error("decimal tokens with leading zeros compare by value")
	}
}

// TestSchemaPath covers the key-aware diagnostic paths under
// spec.config.schema that Check reports (07 req 75).
func TestSchemaPath(t *testing.T) {
	var doc any
	if err := json.Unmarshal([]byte(`{"allOf":[{"properties":{"a.b":{"pattern":"("}}}],"items":[true]}`), &doc); err != nil {
		t.Fatal(err)
	}
	prefix := configSchemaPath()
	for _, tc := range []struct{ p, want string }{
		{"", "spec.config.schema"},
		{"/allOf/0/properties/a.b/pattern", `spec.config.schema.allOf[0].properties["a.b"].pattern`},
		{"/items/0", "spec.config.schema.items[0]"},
		{"/items/9", `spec.config.schema.items["9"]`},
		{"/allOf/0/missing/0", `spec.config.schema.allOf[0].missing["0"]`},
		{"/$schema", "spec.config.schema.$schema"},
		{"/a~1b", `spec.config.schema["a/b"]`},
	} {
		got := schemaPath(prefix, doc, tc.p)
		if got.String() != tc.want {
			t.Errorf("schemaPath(%q) = %s, want %s", tc.p, got, tc.want)
		}
	}
	if len(prefix) != 3 {
		t.Errorf("schemaPath modified its prefix: %v", prefix)
	}
	if got := schemaPath(diag.Path{}, nil, "/x"); got.String() != "x" {
		t.Errorf("schemaPath over a nil document = %s", got)
	}
}
