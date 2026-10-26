// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
)

// literals returns n distinct number literals <i><exp>, joined by commas.
func literals(n int, exp string) string {
	out := make([]string, n)
	for i := range out {
		out[i] = strconv.Itoa(i+1) + exp
	}
	return strings.Join(out, ",")
}

// noPanic runs f and reports a panic as a test error.
func noPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s panicked: %v", what, r)
		}
	}()
	f()
}

// TestNameListsRefused covers the lists of names: number literals inside
// the arrays of type, required, dependentRequired and dependencies escape
// the number bounds, and the library's meta-schema check parses them with
// math/big, past 20 entries panicking on a literal math/big refuses. Each
// shape is RZ-CFG-005 at the entry that is not a string, without a panic,
// through CompileSchema, Check and Factory.Build, at every schema position,
// a reference target and a member map read as a schema included.
func TestNameListsRefused(t *testing.T) {
	huge := literals(21, "e9999999")
	for _, tc := range []struct {
		name, schema, ptr string
	}{
		{"type", `{"type":[` + huge + `]}`, "/type/0"},
		{"required", `{"required":[` + huge + `]}`, "/required/0"},
		{"dependentRequired", `{"dependentRequired":{"a":[` + huge + `]}}`, "/dependentRequired/a/0"},
		{"dependencies", `{"dependencies":{"a":[` + huge + `]}}`, "/dependencies/a/0"},
		{"nested arrays", `{"type":[[1e9999999],[2e9999999],[3e9999999],[4e9999999],[5e9999999],[6e9999999],[7e9999999],[8e9999999],[9e9999999],[10e9999999],[11e9999999],[12e9999999],[13e9999999],[14e9999999],[15e9999999],[16e9999999],[17e9999999],[18e9999999],[19e9999999],[20e9999999],[21e9999999]]}`, "/type/0"},
		{"member map behind a reference", `{"$ref":"#/x/properties","x":{"properties":{"type":[` + huge + `]}}}`, "/x/properties/type/0"},
		// The library compiles $recursiveRef as a reference.
		{"behind a recursive reference", `{"$recursiveRef":"#/x","x":{"type":[` + huge + `]}}`, "/x/type/0"},
		{"required behind a recursive reference", `{"properties":{"a":{"$recursiveRef":"#/x"}},"x":{"required":[` + huge + `]}}`, "/x/required/0"},
		{"after a string", `{"properties":{"a":{"required":["id",` + huge + `]}}}`, "/properties/a/required/1"},
		{"slow type list", `{"type":[` + literals(20, "e999999") + `]}`, "/type/0"},
		{"in range", `{"$defs":{"a":{"type":[1,2,3,4]}}}`, "/$defs/a/type/0"},
		{"objects", `{"required":[{"a":1},{"a":2}]}`, "/required/0"},
		// The library compiles additionalItems only as a reference's target.
		{"additionalItems behind a reference", `{"$ref":"#/additionalItems","additionalItems":{"type":[1]}}`, "/additionalItems/type/0"},
		{"below additionalItems behind a reference", `{"$ref":"#/additionalItems/properties/a","additionalItems":{"properties":{"a":{"required":[` + huge + `]}}}}`, "/additionalItems/properties/a/required/0"},
		// Through an anchor or a resource, which no JSON Pointer names, and
		// a dynamic anchor, which the library compiles with its resource.
		{"additionalItems behind an anchor", `{"$ref":"#a","additionalItems":{"$anchor":"a","type":[1]}}`, "/additionalItems/type/0"},
		{"additionalItems behind a resource", `{"$ref":"urn:x","additionalItems":{"$id":"urn:x","type":[1]}}`, "/additionalItems/type/0"},
		{"dynamic anchor below additionalItems", `{"additionalItems":{"$dynamicAnchor":"d","required":[1]}}`, "/additionalItems/required/0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noPanic(t, "CompileSchema", func() {
				_, err := CompileSchema([]byte(tc.schema))
				var ce *CompileError
				if !errors.As(err, &ce) || len(ce.Issues) == 0 || ce.Issues[0].Pointer != tc.ptr || !strings.HasPrefix(ce.Issues[0].Message, "invalid JSON Schema: ") {
					t.Errorf("CompileSchema = %v, want the first finding at %s", err, tc.ptr)
				}
			})
			noPanic(t, "Check", func() {
				if got := runCheck(policyResource("body", tc.schema)); len(got) == 0 || got[0].Code != CodeConfig {
					t.Errorf("Check = %+v, want RZ-CFG-005", got)
				}
			})
			noPanic(t, "Factory.Build", func() {
				_, err := Factory{}.Build(t.Context(), filter.BuildEnv{Config: configOf(tc.schema)})
				if code, _ := errcode.CodeOf(err); code != CodeConfig {
					t.Errorf("Build = %v, want %s", err, CodeConfig)
				}
			})
		})
	}
}

// TestNameListsRefusedByWalk shows that the rule walk itself refuses the
// entries, so the library never compares them, which for 20 literals of
// exponent 999,999 in a 241-byte type list would take 11 s: 380 math/big
// parses at about 30 ms each. It needs no clock: the walk reports every
// entry, and compilePrepared returns the walk's findings before the
// compiler runs.
func TestNameListsRefusedByWalk(t *testing.T) {
	for _, tc := range []struct {
		schema string
		want   []string
	}{
		{`{"type":[` + literals(20, "e999999") + `]}`, pointers("/type", 20)},
		{`{"properties":{"a":{"type":[1e999999,2e999999]},"b":{"required":[1e999998,2e999998]}}}`, []string{"/properties/a/type/0", "/properties/a/type/1", "/properties/b/required/0", "/properties/b/required/1"}},
		{`{"$defs":{"a":{"type":[1e999999,2e999999,3e999999,4e999999]}}}`, pointers("/$defs/a/type", 4)},
		{`{"dependencies":{"a":[1e999999,"b"],"c":{"required":[true]}},"dependentRequired":{"d":[null]}}`, []string{"/dependencies/a/0", "/dependentRequired/d/0", "/dependencies/c/required/0"}},
	} {
		p, issues := prepare([]byte(tc.schema))
		if len(issues) > 0 {
			t.Fatal(issues)
		}
		w := newWalker(p.doc)
		w.walk()
		var got []string
		for _, is := range w.issues {
			got = append(got, is.Pointer)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("walk(%.60s) found %q, want %q", tc.schema, got, tc.want)
		}
		if _, issues, _ := compilePrepared(p, refusingLoader{}, nil); !slices.Equal(issues, w.issues) {
			t.Errorf("compilePrepared(%.60s) = %+v, want the walk's findings", tc.schema, issues)
		}
	}
}

// pointers returns ptr/0 to ptr/n-1.
func pointers(ptr string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = ptr + "/" + strconv.Itoa(i)
	}
	return out
}

// TestNameListsMatchLibrary pins the walk's findings on the lists of names
// to the library's own meta-schema findings for the same entries, so a
// document gets the same diagnostics whichever finds them: each schema is
// walked, then compiled with the walk's findings dropped.
func TestNameListsMatchLibrary(t *testing.T) {
	var schemas []string
	for _, e := range []string{`7`, `-1.5e3`, `{"a":1}`, `[1]`, `true`, `null`} {
		schemas = append(schemas,
			fmt.Sprintf(`{"type":["string",%s]}`, e),
			fmt.Sprintf(`{"required":[%s,"a"]}`, e),
			fmt.Sprintf(`{"dependentRequired":{"a":["b",%s]}}`, e),
			fmt.Sprintf(`{"dependencies":{"a":[%s]}}`, e),
		)
	}
	schemas = append(schemas, `{"type":[7,8],"required":[1],"properties":{"a":{"dependencies":{"b":[false]}}}}`)
	for _, schema := range schemas {
		p, issues := prepare([]byte(schema))
		if len(issues) > 0 {
			t.Fatal(issues)
		}
		w := newWalker(p.doc)
		w.walk()
		walked := slices.Clone(w.issues)
		if len(walked) == 0 {
			t.Errorf("walk(%s) found nothing", schema)
			continue
		}
		w.issues = nil
		_, library, _ := compileWalked(p, w, refusingLoader{}, nil, newRegexpEngine())
		slices.SortFunc(walked, func(a, b Issue) int { return comparePointers(a.Pointer, b.Pointer) })
		if !slices.Equal(walked, library) {
			t.Errorf("walk(%s) = %+v, library %+v", schema, walked, library)
		}
	}
}

// TestNameListsAccepted covers what the rule leaves alone: lists of
// strings, a dependencies member that is a schema, numbers in annotations
// and unknown keywords, and members named like the keywords. Below
// additionalItems, an unknown keyword under draft 2020-12 that the library
// neither compiles nor checks against the meta-schema, no rule applies
// unless the library compiles a schema there: a reference it compiles
// names it, by pointer, $anchor or $id, or it is a $dynamicAnchor schema
// of a resource the library compiles. A property name or a const, enum or
// examples value merely holding an $id or an anchor makes nothing there
// compile, nor does an $anchor or $id no reference names, and a reference
// there is checked only when the library compiles it, once, so one leaving
// the document that it never compiles is accepted.
func TestNameListsAccepted(t *testing.T) {
	for _, schema := range []string{
		`{"type":["string","null"],"required":["a","b"],"dependentRequired":{"a":["b"]},"dependencies":{"a":["b"],"c":{"examples":[1e9999999]}}}`,
		`{"x-list":{"type":[1e9999999],"required":[2e9999999]},"examples":[[1e9999999]],"default":{"required":[3]}}`,
		`{"properties":{"type":{"const":1},"required":{"type":"integer"},"dependencies":{"enum":[1,2]}}}`,
		`{"type":"string","dependentRequired":{}}`,
		`{"additionalItems":{"type":[1]}}`,
		`{"additionalItems":{"required":[1],"dependentRequired":{"a":[null]}}}`,
		`{"additionalItems":{"minimum":1e5000,"pattern":"(?=a)","properties":{"a":{"patternProperties":{"[":true}}}}}`,
		// A reference inside it is never compiled, so its target is not
		// compiled for it either, and one leaving the document is never
		// loaded.
		`{"additionalItems":{"$ref":"#/$defs/a"},"$defs":{"a":true},"x":{"type":[1]},"y":{"$ref":"#/x"}}`,
		`{"additionalItems":{"$ref":"#/x"},"x":{"type":[1]}}`,
		`{"additionalItems":{"$ref":"https://example.com/s"}}`,
		`{"additionalItems":{"$dynamicRef":"https://example.com/s#d","items":{"$ref":"#/nowhere"}}}`,
		// Identifiers that are no schema's: a const value, a property name
		// and an examples value.
		`{"additionalItems":{"const":{"$id":"x"},"pattern":"(?=a)"}}`,
		`{"additionalItems":{"properties":{"$anchor":true},"minimum":1e5000}}`,
		`{"additionalItems":{"examples":[{"$dynamicAnchor":"d"}],"required":[1]}}`,
		// Identifiers that are not strings, which the library ignores.
		`{"additionalItems":{"$anchor":5,"$id":{"a":1},"pattern":"(?=a)"}}`,
		// An $anchor no reference names, so nothing compiles the reference
		// below it.
		`{"additionalItems":{"$anchor":"a","items":{"$ref":"#/nope"}}}`,
	} {
		spy := &spyLoader{}
		if _, _, issues := compileRaw([]byte(schema), spy); len(issues) > 0 || len(spy.urls) > 0 {
			t.Errorf("compile(%s) = %+v, loaded %q", schema, issues, spy.urls)
		}
	}
	// A reference the library compiles is checked once, also when its
	// schema was walked inert first.
	for _, tc := range []struct{ schema, ptr string }{
		{`{"$ref":"#/additionalItems","additionalItems":{"$ref":"#/nope"}}`, "/additionalItems/$ref"},
		{`{"$ref":"#/additionalItems","additionalItems":{"$ref":"https://example.com/s"}}`, "/additionalItems/$ref"},
		{`{"$ref":"#a","additionalItems":{"$anchor":"a","items":{"$ref":"#/nope"}}}`, "/additionalItems/items/$ref"},
	} {
		if issues := compileIssues(t, tc.schema); len(issues) != 1 || issues[0].Pointer != tc.ptr {
			t.Errorf("CompileSchema(%s) = %+v, want one finding at %s", tc.schema, issues, tc.ptr)
		}
	}
}
