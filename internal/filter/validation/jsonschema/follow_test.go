// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/filter"
)

// The tests of the walk's model of which positions the library compiles,
// with which base and in which order (walker): the walk mirrors the
// library and adds no refusal of its own.

// typeBomb is the 21-literal type list that makes the library's
// meta-schema check panic (math/big's nil result dereferenced).
func typeBomb() string { return `{"type":[` + literals(21, "e9999999") + `]}` }

// typeBombIssues is the walk's findings on typeBomb at ptr: the first
// MaxIssues entries.
func typeBombIssues(ptr string) []Issue {
	out := make([]Issue, MaxIssues)
	for i := range out {
		out[i] = Issue{fmt.Sprintf("%s/type/%d", ptr, i), typeEntryMessage}
	}
	return out
}

// nameIssues is the walk's findings on a list of names at ptr whose
// entries are all of the JSON kind: the first MaxIssues entries.
func nameIssues(ptr, kind string) []Issue {
	out := make([]Issue, MaxIssues)
	for i := range out {
		out[i] = Issue{fmt.Sprintf("%s/%d", ptr, i), "invalid JSON Schema: got " + kind + ", want string"}
	}
	return out
}

// numberIssue is the walk's number-bound finding at ptr.
func numberIssue(ptr string) Issue {
	return Issue{ptr, "number is out of range: at most 128 characters and a decimal exponent within ±1000"}
}

// metaIssue is the meta-schema's finding text for a multipleOf of 0.
const msgMultipleOfZero = "invalid JSON Schema: exclusiveMinimum: got 0, want 0"

// patternTooLarge is the walk's finding for a pattern past
// MaxPatternInstructions.
func patternTooLarge(ptr string) Issue { return Issue{ptr, tooLarge("pattern")} }

// refIssue is the walk's finding for the reference ref at ptr, by the
// status it resolves to.
func refIssue(ptr, ref string, status refStatus) Issue {
	w := newWalker(nil)
	w.checkRef(refSite{ptr: ptr, value: ref, res: resolution{status: status}})
	return w.issues[0]
}

// acceptedThrough checks that schema compiles through the compile,
// CompileSchema, Check and Factory.Build, without asking the loader, and
// returns the compiled Schema.
func acceptedThrough(t *testing.T, schema string) *Schema {
	t.Helper()
	spy := &spyLoader{}
	var s *Schema
	noPanic(t, "compile", func() {
		var issues []Issue
		if s, _, issues = compileRaw([]byte(schema), spy); s == nil || len(issues) > 0 {
			t.Errorf("compile(%.120s) = %+v, want it compiled", schema, issues)
		}
	})
	if len(spy.urls) > 0 {
		t.Errorf("compile(%.120s) asked the loader for %q", schema, spy.urls)
	}
	if _, err := CompileSchema([]byte(schema)); err != nil {
		t.Errorf("CompileSchema(%.120s) = %v", schema, err)
	}
	if got := runCheck(policyResource("body", schema)); len(got) > 0 {
		t.Errorf("Check(%.120s) = %+v, want none", schema, got)
	}
	if _, err := (Factory{}).Build(t.Context(), filter.BuildEnv{Config: configOf(schema), Limits: filter.Limits{MaxRequestBodyBytes: testLimit}}); err != nil {
		t.Errorf("Build(%.120s) = %v", schema, err)
	}
	return s
}

// followCase is a test document: refused with exactly want, or accepted
// (want nil), passing the bodies valid and failing invalid. An
// accepted case is also decided as the library alone decides it, compiling
// the document without the walk, unless ordered: the library alone then
// collects the targets in the order it meets their references, which can
// make it resolve a reference elsewhere or not at all, while the compile
// collects them in pointer order, an enclosing target first (see "Compiled
// set" in doc.go).
type followCase struct {
	name, schema   string
	want           []Issue
	valid, invalid []string
	ordered        bool
}

// runFollowCases runs each case through the compile, CompileSchema, Check
// and Factory.Build. A refused case is refused before the library compiles
// anything: by the walk, whose findings are then the compile's, or, where
// the walk finds nothing, by the compile's own meta-schema check of what
// the library compiles below additionalItems (checkApart), never by a
// backstop after the library compiled (refusedBeforeLibrary).
func runFollowCases(t *testing.T, cases []followCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want != nil {
				refusedBeforeLibrary(t, tc.schema, tc.want)
				refusedThrough(t, tc.schema, tc.want)
				return
			}
			s := acceptedThrough(t, tc.schema)
			if s == nil {
				return
			}
			var lib *sjs.Schema
			if !tc.ordered {
				lib = libraryCompile(t, tc.schema)
			}
			for _, b := range append(slices.Clone(tc.valid), tc.invalid...) {
				want := slices.Contains(tc.valid, b)
				if _, got := s.Validate(decodeInstance(t, b)); got != want {
					t.Errorf("Validate(%s) = %v, want %v", b, got, want)
				}
				if lib == nil {
					continue
				}
				inst, err := sjs.UnmarshalJSON(strings.NewReader(b))
				if err != nil {
					t.Fatal(err)
				}
				if alone := lib.Validate(inst) == nil; alone != want {
					t.Errorf("the library alone: Validate(%s) = %v, want %v", b, alone, want)
				}
			}
		})
	}
}

// refusedBeforeLibrary checks that the walk refuses schema with exactly
// want, or, finding nothing, leaves positions below additionalItems to the
// compile's meta-schema check, which comes before the library compiles.
func refusedBeforeLibrary(t *testing.T, schema string, want []Issue) {
	t.Helper()
	p, issues := prepare([]byte(schema))
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	w := newWalker(p.doc)
	w.walk()
	switch {
	case len(w.issues) > 0:
		if !slices.Equal(w.issues, want) {
			t.Errorf("the walk refused %.120s with %+v, want %+v", schema, w.issues, want)
		}
	case len(w.uncheckedPositions()) == 0:
		t.Errorf("the walk passed %.120s, and nothing comes before the library compiles it", schema)
	}
}

// baseCases are the base-mismatch shapes: a JSON Pointer reference into
// an embedded resource outside its keyword tree, whose target the library
// bases on that resource, the longest pointer prefix it collected, and
// with it every reference there.
func baseCases() []followCase {
	const sub = `"$id":"https://a.example/dir/sub"`
	at := func(resource string) string {
		doc, _ := onRoute(mismatchRoute, "", "", resource)
		return doc
	}
	const w = "/$defs/r/w"
	props := make([]string, 15000)
	for i := range props {
		props[i] = fmt.Sprintf(`"%x":{}`, i)
	}
	return []followCase{
		// The target the library compiles is /$defs/x/zzz, not the root's
		// /zzz, and its type list makes the meta-schema check panic.
		{"library target type list", `{"$ref":"#/$defs/x/foo","zzz":{},"$defs":{"x":{` + sub + `,"foo":{"$ref":"#/zzz"},"zzz":` + typeBomb() + `}}}`, typeBombIssues("/$defs/x/zzz"), nil, nil, false},
		// The rules and counts at the library-only target.
		{"library target number", at(`{"minimum":1e99999}`), []Issue{numberIssue(w + "/minimum")}, nil, nil, false},
		{"library target pattern", at(`{"pattern":"` + strings.Repeat("[a-z]{1000}", 30) + `"}`), []Issue{patternTooLarge(w + "/pattern")}, nil, nil, false},
		{"library target name list", at(`{"required":[` + literals(21, "e9999999") + `]}`), nameIssues(w+"/required", "number"), nil, nil, false},
		{"library target MaxSubschemas", at(`{"properties":{` + strings.Join(props, ",") + `}}`), []Issue{{"", "schema has more than 4096 subschemas"}}, nil, nil, false},
		// The loader the library would ask.
		{
			"relative reference, root $id", `{"$id":"https://ex.test/a/","$ref":"#/$defs/b/x","$defs":{"b":{"$id":"https://other.test/z/","x":{"$ref":"y.json"}},"y":{"$id":"y.json"}}}`,
			[]Issue{refIssue("/$defs/b/x/$ref", "y.json", refOutside)},
			nil, nil, false,
		},
		{
			"relative reference, document's URL", `{"$ref":"#/$defs/x/foo","$defs":{"f":{"$id":"f.json"},"x":{` + sub + `,"foo":{"$ref":"f.json"}}}}`,
			[]Issue{refIssue("/$defs/x/foo/$ref", "f.json", refOutside)},
			nil, nil, false,
		},
		{"remote $ref", at(`{"$ref":"https://example.com/x"}`), []Issue{refIssue(w+"/$ref", "https://example.com/x", refOutside)}, nil, nil, false},
		{"remote $dynamicRef", at(`{"$dynamicRef":"https://example.com/x#d"}`), []Issue{refIssue(w+"/$dynamicRef", "https://example.com/x#d", refOutside)}, nil, nil, false},
		{"bundled meta-schema", at(`{"$ref":"https://json-schema.org/draft/2020-12/schema"}`), []Issue{refIssue(w+"/$ref", "https://json-schema.org/draft/2020-12/schema", refOutside)}, nil, nil, false},
		{"remote $schema", at(`{"$id":"urn:z2","$schema":"https://example.com/meta"}`), []Issue{{w + "/$schema", msgDialectText}}, nil, nil, false},
		// What no meta-schema check of the library covers, and a dialect a
		// $schema gives there.
		{
			"additionalItems through c.json", `{"$id":"https://ex.test/a/","$ref":"#/$defs/b/x","$defs":{"b":{"$id":"https://other.test/z/","x":{"$ref":"c.json#/additionalItems"}},` +
				`"c1":{"$id":"https://ex.test/a/c.json","additionalItems":true},"c2":{"$id":"https://other.test/z/c.json","additionalItems":{"multipleOf":0}}}}`,
			[]Issue{{"/$defs/c2/additionalItems/multipleOf", msgMultipleOfZero}},
			nil, nil, false,
		},
		{
			"additionalItems of the embedded resource", `{"$ref":"#/$defs/x/foo","additionalItems":{},"$defs":{"x":{` + sub + `,"foo":{"$ref":"#/additionalItems"},"additionalItems":{"multipleOf":0}}}}`,
			[]Issue{{"/$defs/x/additionalItems/multipleOf", msgMultipleOfZero}},
			nil, nil, false,
		},
		{"meta/core dialect", at(`{"$id":"urn:z2","$schema":"https://json-schema.org/draft/2020-12/meta/core","type":"string"}`), []Issue{{w + "/$schema", msgDialectText}}, nil, nil, false},
		{
			"invalid regex below additionalItems", `{"$ref":"#/$defs/b/x","$defs":{"b":{"$id":"urn:b","x":{"$ref":"#/additionalItems"},"additionalItems":{"pattern":"(?=a)"}}}}`,
			[]Issue{{"/$defs/b/additionalItems/pattern", "pattern is not a valid RE2 regular expression: error parsing regexp: invalid or unsupported Perl syntax: `(?=`"}},
			nil, nil, false,
		},
		// Anchors in the library's resource, and relative references
		// against its base.
		{
			"anchor of the wrong resource", `{"$ref":"#/$defs/b/x","allOf":[{"$ref":"#k"}],"$defs":{"b":{"$id":"urn:b","x":{"$anchor":"k"}}}}`,
			[]Issue{refIssue("/allOf/0/$ref", "#k", refNoAnchor)},
			nil, nil, false,
		},
		{
			"anchor of the right resource", `{"$ref":"#/$defs/b/x","properties":{"p":{"$ref":"urn:b#k"}},"$defs":{"b":{"$id":"urn:b","x":{"$anchor":"k","type":"string"}}}}`,
			nil,
			[]string{`"s"`},
			[]string{`5`, `{"p":"s"}`},
			false,
		},
		{
			"dynamic anchor of the right resource", `{"$ref":"#/$defs/b/x","properties":{"p":{"$dynamicRef":"urn:b#k"}},"$defs":{"b":{"$id":"urn:b","x":{"$dynamicAnchor":"k","type":"string"}}}}`,
			nil,
			[]string{`"s"`},
			[]string{`5`, `{"p":"s"}`},
			false,
		},
		{
			"relative reference the library resolves", `{"$ref":"#/$defs/x/foo","$defs":{"g":{"$id":"https://a.example/dir/g.json","type":"integer"},"x":{` + sub + `,"foo":{"$ref":"g.json"}}}}`,
			nil,
			[]string{`5`},
			[]string{`"s"`},
			false,
		},
		{
			"$id under the library's base", `{"$ref":"#/$defs/b/x","$defs":{"b":{"$id":"https://other.test/z/","x":{"$ref":"c.json"}},"c":{"$id":"https://other.test/z/c.json","type":"string"}}}`,
			nil,
			[]string{`"s"`},
			[]string{`5`},
			false,
		},
		// A relative $id: a target checked apart joins its own $id, and
		// those of its keyword tree, against the base of the resource the
		// library gives it, /$defs/x's, not the document's URL
		// (collectTarget). The reference by the resolved URL resolves; the
		// one by the URL the document's base would give names no resource,
		// so the walk refuses it at the reference and the library never asks
		// the loader for it.
		{
			"relative $id of a target", `{"$ref":"#/$defs/x/foo","$defs":{"x":{` + sub + `,"foo":{"$id":"rel.json","type":"string"}}},"allOf":[{"$ref":"https://a.example/dir/rel.json"}]}`,
			nil,
			[]string{`"s"`},
			[]string{`5`},
			false,
		},
		{
			"relative $id of a target by the document's URL", `{"$ref":"#/$defs/x/foo","$defs":{"x":{` + sub + `,"foo":{"$id":"rel.json","type":"string"}}},"allOf":[{"$ref":"rel.json"}]}`,
			[]Issue{refIssue("/allOf/0/$ref", "rel.json", refOutside)},
			nil, nil, false,
		},
		{
			"relative $id in a target's keyword tree", `{"$ref":"#/$defs/x/foo","$defs":{"x":{` + sub + `,"foo":{"$defs":{"n":{"$id":"n.json","type":"string"}}}}},"allOf":[{"$ref":"https://a.example/dir/n.json"}]}`,
			nil,
			[]string{`"s"`},
			[]string{`5`},
			false,
		},
		{
			"relative $id in a target's keyword tree by the document's URL", `{"$ref":"#/$defs/x/foo","$defs":{"x":{` + sub + `,"foo":{"$defs":{"n":{"$id":"n.json","type":"string"}}}}},"allOf":[{"$ref":"n.json"}]}`,
			[]Issue{refIssue("/allOf/0/$ref", "n.json", refOutside)},
			nil, nil, false,
		},
		{"missing pointer", at(`{"$ref":"#/nope"}`), []Issue{refIssue(w+"/$ref", "#/nope", refMissing)}, nil, nil, false},
		{
			"non-schema target", `{"$defs":{"r":{"$id":"urn:r","x":{"$ref":"#/w"},"w":{"$ref":"#/c"},"c":5}},"w":true,"$ref":"#/$defs/r/x"}`,
			[]Issue{refIssue(w+"/$ref", "#/c", refNotSchema)},
			nil, nil, false,
		},
		{"bad escape", at(`{"$ref":"#/~2"}`), []Issue{refIssue(w+"/$ref", "#/~2", refMissing)}, nil, nil, false},
		{"missing anchor", at(`{"$ref":"#nope"}`), []Issue{refIssue(w+"/$ref", "#nope", refNoAnchor)}, nil, nil, false},
		{
			"anchor of another resource", `{"$anchor":"k","$ref":"#/$defs/r/x","$defs":{"r":{"$id":"urn:r","x":{"$ref":"#k"}}}}`,
			[]Issue{refIssue("/$defs/r/x/$ref", "#k", refNoAnchor)},
			nil, nil, false,
		},
		// The root resource's URL is its own $id, and the document's URL
		// names the root apart (root.go:58-62), so a nested $id of the
		// document's URL is a duplicate only when the root has no $id.
		{
			"root $id beside the document's URL", `{"$id":"urn:root","$ref":"` + baseURL + `#/$defs/b","$defs":{"a":{"$id":"` + baseURL + `"},"b":{"type":"string"}}}`,
			nil,
			[]string{`"s"`},
			[]string{`5`},
			false,
		},
		{
			"the document's URL twice", `{"$defs":{"a":{"$id":"` + baseURL + `"}}}`,
			[]Issue{{"/$defs/a/$id", `$id "#" is already the identifier of the schema at ""`}},
			nil, nil, false,
		},
		// Duplicate anchors in the library's resource.
		{
			"duplicate in the library's resource", `{"$ref":"#/$defs/b/x","$defs":{"b":{"$id":"urn:b","$anchor":"k","x":{"$anchor":"k"}}}}`,
			[]Issue{{"/$defs/b/x/$anchor", `anchor "k" is already defined by the schema at "/$defs/b"`}},
			nil, nil, false,
		},
		{
			"same anchor in another resource", `{"$anchor":"k","$ref":"#/$defs/b/x","$defs":{"b":{"$id":"urn:b","x":{"$anchor":"k","type":"string"}}}}`,
			nil,
			[]string{`"s"`},
			[]string{`5`},
			false,
		},
		// A duplicate $id names the resource of the schema that holds it
		// first, the one the library's lookup of the base finds
		// (roots.go:150-159), so the anchors of the duplicate join that
		// resource, and an anchor both define is a duplicate too (collect).
		{
			"duplicate $id joins the first holder's resource", `{"$defs":{"a":{"$id":"urn:a","$anchor":"k"},"b":{"$id":"urn:a","$anchor":"k"}}}`,
			[]Issue{
				{"/$defs/b/$id", `$id "urn:a" is already the identifier of the schema at "/$defs/a"`},
				{"/$defs/b/$anchor", `anchor "k" is already defined by the schema at "/$defs/a"`},
			},
			nil, nil, false,
		},
		// A duplicate anchor defines nothing, so a $dynamicAnchor that
		// repeats a plain $anchor of its resource does not make the
		// $anchor's schema compile eagerly: below additionalItems that
		// schema stays inert, and no rule applies there (collect).
		{
			"duplicate $dynamicAnchor defines nothing", `{"$defs":{"r":{"$id":"urn:r","additionalItems":{"$anchor":"d","minimum":1e99999},"items":{"$dynamicAnchor":"d"}}}}`,
			[]Issue{{"/$defs/r/items/$dynamicAnchor", `anchor "d" is already defined by the schema at "/$defs/r/additionalItems"`}},
			nil, nil, false,
		},
	}
}

// TestLibraryBase covers the base-mismatch shapes: each is RZ-CFG-005 with
// the walk's own finding at the library's position, through CompileSchema,
// Check and Factory.Build, without a panic and without asking the loader,
// or compiles and decides as the library alone does.
func TestLibraryBase(t *testing.T) { runFollowCases(t, baseCases()) }

// lateIDCases are the late-$id shapes: a reference naming an $id that
// only a reference target, collected when the library checks it apart,
// holds, and the order of those checks.
func lateIDCases() []followCase {
	return []followCase{
		// The $id in the target itself, and in its keyword tree.
		{"late $id in the target", `{"$ref":"urn:y#/p","$defs":{"r":{"$ref":"#/x/b"}},"x":{"b":{"$id":"urn:y","p":` + typeBomb() + `}}}`, typeBombIssues("/x/b/p"), nil, nil, false},
		{"late $id in its keyword tree", `{"$ref":"urn:y#/p","$defs":{"r":{"$ref":"#/x"}},"x":{"$defs":{"b":{"$id":"urn:y","p":` + typeBomb() + `}}}}`, typeBombIssues("/x/$defs/b/p"), nil, nil, false},
		// The rules and counts behind a late $id.
		{
			"late $id pattern", `{"$ref":"urn:y#/p","$defs":{"r":{"$ref":"#/x/b"}},"x":{"b":{"$id":"urn:y","p":{"pattern":"` + strings.Repeat("[a-z]{1000}", 30) + `"}}}}`,
			[]Issue{patternTooLarge("/x/b/p/pattern")},
			nil, nil, false,
		},
		{
			"late $id number", `{"$ref":"urn:y#/p","$defs":{"r":{"$ref":"#/x/b"}},"x":{"b":{"$id":"urn:y","p":{"maximum":1e99999}}}}`,
			[]Issue{numberIssue("/x/b/p/maximum")},
			nil, nil, false,
		},
		{
			"late $id MaxSubschemas", `{"$ref":"urn:y#/p","$defs":{"r":{"$ref":"#/x/b"}},"x":{"b":{"$id":"urn:y","p":{"allOf":[` + joinRepeat(`{}`, MaxSubschemas) + `]}}}}`,
			[]Issue{{"", "schema has more than 4096 subschemas"}},
			nil, nil, false,
		},
		// What no check of the library covers, and a dialect.
		{
			"late $id additionalItems", `{"$ref":"urn:y#/additionalItems","$defs":{"r":{"$ref":"#/x/b"}},"x":{"b":{"$id":"urn:y","additionalItems":{"multipleOf":0}}}}`,
			[]Issue{{"/x/b/additionalItems/multipleOf", msgMultipleOfZero}},
			nil, nil, false,
		},
		{
			"late $id meta/core dialect", `{"$ref":"urn:y#/p","$defs":{"r":{"$ref":"#/x/b"}},"x":{"b":{"$id":"urn:y","p":{"$id":"urn:q","$schema":"https://json-schema.org/draft/2020-12/meta/core","type":"string"}}}}`,
			[]Issue{{"/x/b/p/$schema", msgDialectText}},
			nil, nil, false,
		},
		// An $id a later target holds, which the library has not collected
		// when the earlier target's own reference compiles, and one an inert
		// reference's target holds, which it never collects.
		{
			"late $id check order", `{"allOf":[{"$ref":"#/a"},{"$ref":"#/b"}],"a":{"$ref":"urn:y"},"b":{"$id":"urn:y"}}`,
			[]Issue{refIssue("/a/$ref", "urn:y", refOutside)},
			nil, nil, false,
		},
		{
			"late $id of an inert target", `{"additionalItems":{"$ref":"#/b"},"b":{"$id":"urn:y"},"$ref":"urn:y"}`,
			[]Issue{refIssue("/$ref", "urn:y", refOutside)},
			nil, nil, false,
		},
		// The late $id the library does collect first resolves.
		{"late $id resolved", `{"$ref":"urn:y#/p","$defs":{"r":{"$ref":"#/x/b"}},"x":{"b":{"$id":"urn:y","p":{"type":"string"}}}}`, nil, []string{`"s"`}, []string{`5`}, true},
	}
}

// TestLateID covers the late-$id shapes: the walk follows to the library's
// fixpoint, so a reference naming an $id that a reference target holds
// reaches its rules, and refuses at the reference one the library would
// load.
func TestLateID(t *testing.T) { runFollowCases(t, lateIDCases()) }

// ancestorCases are the ancestor-target shapes: an ancestor target's $id,
// which the library collects when it checks the ancestor apart, first in
// pointer order, bases a descendant target outside its keyword tree; in
// both orders of the two references.
func ancestorCases() []followCase {
	var out []followCase
	for _, order := range []struct{ name, first, second string }{{"ancestor first", "#/x", "#/x/y"}, {"descendant first", "#/x/y", "#/x"}} {
		doc := func(z string) string {
			return `{"$ref":"` + order.first + `","$defs":{"r":{"$ref":"` + order.second + `"}},"x":{"$id":"urn:x","y":{"$ref":"#/z"},"z":` + z + `},"z":{}}`
		}
		out = append(out,
			followCase{order.name + "/type list", doc(typeBomb()), typeBombIssues("/x/z"), nil, nil, false},
			followCase{order.name + "/number", doc(`{"minimum":1e99999}`), []Issue{numberIssue("/x/z/minimum")}, nil, nil, false},
			followCase{
				order.name + "/additionalItems", `{"$ref":"` + order.first + `","$defs":{"r":{"$ref":"` + order.second + `"}},"x":{"$id":"urn:x","y":{"$ref":"#/additionalItems"},"additionalItems":{"multipleOf":0}},"additionalItems":{}}`,
				[]Issue{{"/x/additionalItems/multipleOf", msgMultipleOfZero}},
				nil, nil, false,
			},
			// The library alone collects the target it meets first.
			followCase{
				order.name + "/accepted", `{"allOf":[{"$ref":"` + order.first + `"},{"$ref":"` + order.second + `"}],"x":{"$id":"urn:x","y":{"$ref":"#/z"},"z":{"type":"string"}},"z":{}}`,
				nil,
				[]string{`"s"`},
				[]string{`5`},
				order.first == "#/x/y",
			},
		)
	}
	return out
}

// TestAncestorTargetBase covers ancestorCases: the walk bases a descendant
// target on the $id of the ancestor target that encloses it, whatever the
// order of the references.
func TestAncestorTargetBase(t *testing.T) { runFollowCases(t, ancestorCases()) }

// policyDialectCases are the shapes of a $schema naming the policy
// document or a wrapper of the compile at a position the library compiles
// only as a reference's target, which the library serves without the
// loader, the document's own $vocabulary then choosing the vocabularies.
func policyDialectCases() []followCase {
	var out []followCase
	for _, u := range []string{baseURL, checkURL, decideURL} {
		doc, at := onRoute(mismatchRoute, `"$vocabulary":{"https://json-schema.org/draft/2020-12/vocab/core":true}`, "", `{"$id":"urn:w","$schema":"`+u+`","type":"string"}`)
		out = append(out, followCase{"policy dialect " + u, doc, []Issue{{at + "/$schema", msgDialectText}}, nil, nil, false})
	}
	doc, at := onRoute(mismatchRoute, `"$vocabulary":{"urn:unknown":true}`, "", `{"$id":"urn:w","$schema":"`+baseURL+`"}`)
	out = append(out,
		followCase{"policy dialect, unknown vocabulary", doc, []Issue{{at + "/$schema", msgDialectText}}, nil, nil, false},
		// A root $vocabulary no $schema names has no effect.
		followCase{"root $vocabulary alone", `{"$vocabulary":{"urn:unknown":true},"$ref":"#/$defs/r/x","$defs":{"r":{"$id":"urn:r","x":{"$ref":"#/w"},"w":{"type":"string"}}}}`, nil, []string{`"s"`}, []string{`5`}, false},
	)
	return out
}

// TestPolicyAsDialect covers policyDialectCases: a $schema naming the
// policy document or a wrapper of the compile is refused at the $schema,
// and a root $vocabulary that no $schema names has no effect.
func TestPolicyAsDialect(t *testing.T) { runFollowCases(t, policyDialectCases()) }

// livenessCases are the additionalItems shapes: the library compiles a
// schema below additionalItems only when a reference it compiles names it,
// as the root of the resource of a schema it compiles, or as a
// $dynamicAnchor schema of a resource root it compiles, and collects no
// target of a reference it never compiles.
func livenessCases() []followCase {
	return []followCase{
		// Identifiers no reference names: nothing compiles them.
		{"unreferenced $id fragment", `{"items":{"type":"integer"},"additionalItems":{"$id":"#item","type":"string"}}`, nil, []string{`[1,2]`}, []string{`["s"]`, `["s",1]`}, false},
		{"unreferenced $anchor", `{"additionalItems":{"$anchor":"a","type":"strng"}}`, nil, []string{`5`}, nil, false},
		{"unreferenced $id", `{"additionalItems":{"$id":"x.json","type":"strng"}}`, nil, []string{`5`}, nil, false},
		{"unreferenced $anchor beside multipleOf 0", `{"additionalItems":{"$anchor":"a","multipleOf":0}}`, nil, []string{`5`, `[5]`}, nil, false},
		{"reference below an anchor", `{"additionalItems":{"$anchor":"a","items":{"$ref":"#/nope"}}}`, nil, []string{`5`}, nil, false},
		{"unreferenced, under items", `{"additionalItems":{"items":{"$anchor":"a","multipleOf":0}}}`, nil, []string{`5`, `[5]`}, nil, false},
		{"unreferenced, under allOf", `{"additionalItems":{"allOf":[{"$anchor":"a","multipleOf":0}]}}`, nil, []string{`5`, `[5]`}, nil, false},
		{"unreferenced $id, under items", `{"additionalItems":{"items":{"$id":"urn:n","multipleOf":0}}}`, nil, []string{`5`, `[5]`}, nil, false},
		{"unreferenced resource with a dynamic anchor", `{"additionalItems":{"$id":"urn:r","$dynamicAnchor":"d","multipleOf":0}}`, nil, []string{`5`, `[5]`}, nil, false},
		// The same identifiers named by a reference the library compiles.
		{
			"referenced $anchor", `{"$ref":"#a","additionalItems":{"$anchor":"a","type":"strng"}}`,
			[]Issue{{"/additionalItems/type", "invalid JSON Schema: got string, want array; or value must be one of 'array', 'boolean', 'integer', 'null', 'number', 'object', 'string'"}},
			nil, nil, false,
		},
		{"referenced $id", `{"$ref":"x.json","additionalItems":{"$id":"x.json","multipleOf":0}}`, []Issue{{"/additionalItems/multipleOf", msgMultipleOfZero}}, nil, nil, false},
		{"$id fragment named", `{"$ref":"#item","additionalItems":{"$id":"#item","type":"string"}}`, []Issue{refIssue("/$ref", "#item", refNoAnchor)}, nil, nil, false},
		{"referenced reference below an anchor", `{"$ref":"#a","additionalItems":{"$anchor":"a","items":{"$ref":"#/nope"}}}`, []Issue{refIssue("/additionalItems/items/$ref", "#/nope", refMissing)}, nil, nil, false},
		{"referenced, under items", `{"$ref":"#a","additionalItems":{"items":{"$anchor":"a","multipleOf":0}}}`, []Issue{{"/additionalItems/items/multipleOf", msgMultipleOfZero}}, nil, nil, false},
		{"referenced, under allOf", `{"$ref":"#a","additionalItems":{"allOf":[{"$anchor":"a","multipleOf":0}]}}`, []Issue{{"/additionalItems/allOf/0/multipleOf", msgMultipleOfZero}}, nil, nil, false},
		{"$dynamicAnchor under not/not", `{"$dynamicRef":"#d","additionalItems":{"not":{"not":{"$dynamicAnchor":"d","multipleOf":0}}}}`, []Issue{{"/additionalItems/not/not/multipleOf", msgMultipleOfZero}}, nil, nil, false},
		{"referenced $id, under items", `{"$ref":"urn:n","additionalItems":{"items":{"$id":"urn:n","multipleOf":0}}}`, []Issue{{"/additionalItems/items/multipleOf", msgMultipleOfZero}}, nil, nil, false},
		// A $dynamicAnchor the library compiles eagerly with its resource
		// root, which it compiles.
		{"dynamic anchor of the root", `{"additionalItems":{"$dynamicAnchor":"d","multipleOf":0}}`, []Issue{{"/additionalItems/multipleOf", msgMultipleOfZero}}, nil, nil, false},
		{
			"dynamic anchor of a compiled resource", `{"$ref":"urn:r","additionalItems":{"$id":"urn:r","additionalItems":{"$dynamicAnchor":"d","multipleOf":0}}}`,
			[]Issue{{"/additionalItems/additionalItems/multipleOf", msgMultipleOfZero}},
			nil, nil, false,
		},
		// The root of the resource of a schema the library compiles.
		{
			"resource root of a target", `{"$ref":"urn:r#/properties/a","additionalItems":{"$id":"urn:r","multipleOf":0,"properties":{"a":true}}}`,
			[]Issue{{"/additionalItems/multipleOf", msgMultipleOfZero}},
			nil, nil, false,
		},
		// An anchor that only an inert reference's target holds.
		{"inert anchor", `{"additionalItems":{"$ref":"#/y"},"y":{"$anchor":"a"},"$ref":"#a"}`, []Issue{refIssue("/$ref", "#a", refNoAnchor)}, nil, nil, false},
		// An inert reference's target is never collected, so it counts
		// against no bound.
		{"inert target uncounted", `{"additionalItems":{"$ref":"#/x"},"x":{"allOf":[` + joinRepeat(`{}`, MaxSubschemas) + `]}}`, nil, []string{`5`}, nil, false},
	}
}

// TestAdditionalItemsLiveness covers livenessCases: the walk walks live
// below additionalItems exactly what the library compiles there, so an
// identifier no reference names, nested or not, leaves its subtree alone
// and the walk refuses nothing there that nothing compiles, and a nested
// one a reference names is checked.
func TestAdditionalItemsLiveness(t *testing.T) { runFollowCases(t, livenessCases()) }

// olderDraftCases are the older-draft shapes: a draft-04 and a draft-07
// resource, each with an enum list ending in a literal math/big refuses,
// on the $recursiveRef route and the base-mismatch route, where the
// library would meta-check them with that draft's meta-schema, whose
// uniqueItems on enum would reach the writeHash nil-Rat panic.
func olderDraftCases() []followCase {
	var out []followCase
	for _, route := range escapedRoutes() {
		for _, d := range []struct{ name, resource string }{
			{"draft-04", `{"id":"urn:w","$schema":"http://json-schema.org/draft-04/schema#","enum":[` + literals(20, "") + `,1e9999999]}`},
			{"draft-07", `{"$id":"urn:w","$schema":"http://json-schema.org/draft-07/schema#","enum":[` + literals(20, "") + `,1e9999999]}`},
		} {
			doc, at := onRoute(route, "", "", d.resource)
			out = append(out, followCase{route + "/" + d.name, doc, []Issue{{at + "/$schema", msgDialectText}, numberIssue(at + "/enum/20")}, nil, nil, false})
		}
	}
	return out
}

// TestOlderDraftRoutes covers olderDraftCases: each is refused at the
// $schema, without a panic, before the library compiles.
func TestOlderDraftRoutes(t *testing.T) { runFollowCases(t, olderDraftCases()) }

// collidingArrays returns n distinct arrays the library's writeHash hashes
// alike: each holds eleven arrays, nested in a different way, and the hash
// writes one tag per array and no length.
func collidingArrays(n int) []string {
	var forests func(pairs int) []string
	forests = func(pairs int) []string {
		if pairs == 0 {
			return []string{""}
		}
		var out []string
		for inner := 0; inner < pairs && len(out) < n; inner++ {
			for _, a := range forests(inner) {
				for _, b := range forests(pairs - 1 - inner) {
					if b != "" {
						b = "," + b
					}
					out = append(out, "["+a+"]"+b)
					if len(out) == n {
						return out
					}
				}
			}
		}
		return out
	}
	trees := forests(10)
	out := make([]string, len(trees))
	for i, f := range trees {
		out[i] = "[" + f + "]"
	}
	return out
}

// TestHashCollisionRoutes covers colliding lists: a list of 8,000 arrays
// that collide in the library's meta-check hash (about 217 KB), as
// required on the $recursiveRef and base-mismatch routes and as an
// enum under a draft-07 $schema there, is refused with the walk's own
// finding before the library compiles, so the library never compares the
// colliding entries pairwise: the walk's findings are the compile's. The
// test asserts the findings and their pointers, never a time.
func TestHashCollisionRoutes(t *testing.T) {
	arrays := collidingArrays(8000)
	if len(arrays) != 8000 || len(slices.Compact(slices.Sorted(slices.Values(arrays)))) != 8000 {
		t.Fatalf("collidingArrays(8000) gave %d arrays", len(arrays))
	}
	list := strings.Join(arrays, ",")
	for _, route := range escapedRoutes() {
		for _, tc := range []struct{ name, resource, want string }{
			{"required", `{"required":[` + list + `]}`, "required"},
			{"draft-07 enum", `{"$id":"urn:w","$schema":"http://json-schema.org/draft-07/schema#","enum":[` + list + `]}`, "$schema"},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				doc, at := onRoute(route, "", "", tc.resource)
				want := nameIssues(at+"/required", "array")
				if tc.want == "$schema" {
					want = []Issue{{at + "/$schema", msgDialectText}}
				}
				p, issues := prepare([]byte(doc))
				if len(issues) > 0 {
					t.Fatal(issues)
				}
				w := newWalker(p.doc)
				w.walk()
				if !slices.Equal(w.issues, want) {
					t.Fatalf("walk = %+v, want %+v", w.issues, want)
				}
				refusedThrough(t, doc, want)
			})
		}
	}
}

// visibilityCases are the shapes of when a reference compiles (the order
// of the checks apart, and the $dynamicAnchor schemas of compileValue): a
// target the compile checks apart compiles right after its own check,
// before the targets after it are collected, so its own references and, as
// a resource root, its $dynamicAnchor schemas see only what is collected
// up to it; every other position compiles after all of them.
func visibilityCases() []followCase {
	return []followCase{
		{"own reference to a later target's $id", `{"allOf":[{"$ref":"#/a"},{"$ref":"#/b"}],"a":{"$ref":"urn:y"},"b":{"$id":"urn:y"}}`, []Issue{refIssue("/a/$ref", "urn:y", refOutside)}, nil, nil, false},
		{"own reference to a later target's anchor", `{"allOf":[{"$ref":"#/a"},{"$ref":"#/b"}],"a":{"$ref":"#k"},"b":{"$anchor":"k"}}`, []Issue{refIssue("/a/$ref", "#k", refNoAnchor)}, nil, nil, false},
		{"own reference to an earlier target's $id", `{"allOf":[{"$ref":"#/b"},{"$ref":"#/a"}],"a":{"$id":"urn:y","type":"string"},"b":{"$ref":"urn:y"}}`, nil, []string{`"s"`}, []string{`5`}, true},
		// The library alone never collects /b, which only $defs/r names.
		{
			"a keyword's reference to a later target's $id", `{"allOf":[{"$ref":"#/a"}],"$defs":{"r":{"$ref":"#/b"}},"a":{"properties":{"p":{"$ref":"urn:y"}}},"b":{"$id":"urn:y","type":"string"}}`,
			nil,
			[]string{`{"p":"s"}`},
			[]string{`{"p":5}`},
			true,
		},
		// The resource /x compiles at its turn, before /x/y adds the
		// $dynamicAnchor below its additionalItems to /x's resource, so the
		// library never compiles that schema; a resource under $defs
		// compiles after every target, and does.
		{"dynamic anchor a later target adds", `{"allOf":[{"$ref":"#/x"},{"$ref":"#/x/y"}],"x":{"$id":"urn:x","y":{"additionalItems":{"$dynamicAnchor":"d","multipleOf":0}}}}`, nil, []string{`5`, `[1]`}, nil, false},
		{
			"dynamic anchor a target adds to a keyword resource", `{"$ref":"#/$defs/x/y","$defs":{"x":{"$id":"urn:x","y":{"additionalItems":{"$dynamicAnchor":"d","multipleOf":0}}}}}`,
			[]Issue{{"/$defs/x/y/additionalItems/multipleOf", msgMultipleOfZero}},
			nil, nil, false,
		},
		// A target a pass finds is collected after every target of the
		// pass's prelude, so none of them sees its $id (close). Here the
		// second pass collects /p first, the root's $ref then finds /p/f,
		// whose $id urn:f /p's own reference must not see: the compile
		// checks /p/f after it compiles /p, and the library would load
		// urn:f.
		{
			"a target found in a pass after the prelude", `{"$ref":"urn:p#/f","$dynamicRef":"#/p","p":{"$id":"urn:p","$ref":"urn:f#/g","f":{"$id":"urn:f","g":{"minimum":1e99999}}}}`,
			[]Issue{refIssue("/p/$ref", "urn:f#/g", refOutside)},
			nil, nil, false,
		},
		// inertAgain: the first pass finds /q, then /p, whose reference
		// then finds the anchor a /q's additionalItems holds and walks it
		// live; in the compile's order /p compiles before /q is collected,
		// so the last pass refuses /p's reference and leaves the anchor's
		// schema inert, its own reference unresolved (TestFollowFixpoint).
		{"own reference to a later target's anchor below additionalItems", inertAgain, []Issue{refIssue("/p/$ref", "#a", refNoAnchor)}, nil, nil, false},
	}
}

// inertAgain is a document whose last pass leaves inert a position an
// earlier pass walked live, with a reference (visibilityCases).
const inertAgain = `{"$ref":"#/q","$dynamicRef":"#/p","p":{"$ref":"#a"},"q":{"additionalItems":{"$anchor":"a","$ref":"#/q"}}}`

// TestReferenceVisibility covers when the library compiles a reference
// (the order of the checks apart).
func TestReferenceVisibility(t *testing.T) { runFollowCases(t, visibilityCases()) }

// generations returns a document whose fixpoint takes one more pass per
// generation: the target x of each generation's resource n holds a
// $dynamicRef to its own enclosing target a, which therefore is found
// after it, and a $ref that resolves to the next generation's target only
// against a's base, which the next pass gives it.
func generations(g int) string {
	return `{"$id":"https://g0.test/","$ref":"n#/a1/x","$defs":{` + generationDefs(g) + `}}`
}

// deepGenerations is generations(g) inside a $defs member whose name is
// long bytes, so that the pointer of every position but the root's carries
// the name (passes that hashed and built these pointers again would take
// 30 s and 31 GB for 228 passes under a 200,000-byte name).
func deepGenerations(g, long int) string {
	return `{"$id":"https://g0.test/","$ref":"n#/a1/x","$defs":{"` + strings.Repeat("L", long) + `":{"$defs":{` + generationDefs(g) + `}}}}`
}

// generationDefs returns the members of the $defs of generations(g).
func generationDefs(g int) string {
	defs := make([]string, g)
	for i := 1; i <= g; i++ {
		defs[i-1] = fmt.Sprintf(`"n%d":{"$id":"https://g%d.test/n","a%d":{"$id":"https://g%d.test/","x":{"$ref":"n#/a%d/x","$dynamicRef":"#/a%d"}}}`, i-1, i-1, i, i, i+1, i)
	}
	return strings.Join(defs, ",")
}

// TestFollowFixpoint covers the walk's fixpoint (walker.walk): a document
// without targets outside its keywords takes one pass, one with them two;
// a target an earlier pass found stays checked apart, and so compiled,
// when the last pass no longer reaches it; a document that makes every
// pass find one more target takes one pass per target, the passes after
// the first charged to the walk-time compile budget, past which it is
// refused at a reference (TestFollowFixpointCost); each pass starts its
// state anew; a walk refused past the budget or past MaxSubschemas
// positions reports the collection's findings of the pass it ended in
// ahead of the bound's; and two walks of a document give the same model.
func TestFollowFixpoint(t *testing.T) {
	walkOf := func(schema string) *walker {
		t.Helper()
		p, issues := prepare([]byte(schema))
		if len(issues) > 0 {
			t.Fatal(issues)
		}
		w := newWalker(p.doc)
		w.walk()
		return w
	}
	for _, tc := range []struct {
		name, schema string
		passes       int
	}{
		{"keywords only", `{"$ref":"#/$defs/a","$defs":{"a":{"$id":"urn:a","items":{"$ref":"urn:a"}}}}`, 1},
		{"a target outside the keywords", `{"$ref":"#/x","x":{"type":"string"}}`, 2},
		{"ancestor target, descendant first", ancestorCases()[4].schema, 2},
		{"ladder", refLadder(5, 6, `{"type":"string"}`), 2},
		{"one generation", generations(1), 2},
		{"three generations", generations(3), 4},
	} {
		if w := walkOf(tc.schema); w.passes != tc.passes {
			t.Errorf("%s: %d passes, want %d", tc.name, w.passes, tc.passes)
		}
	}
	// The stale target: /x/y's base is /x once /x is collected first, so
	// its reference names /x/x instead of /x; /x, found in the first pass,
	// stays checked apart and compiled.
	const stale = `{"$ref":"#/x/y","x":{"$id":"https://x.test/","y":{"$ref":"#/x"},"x":{"type":"string"}}}`
	w := walkOf(stale)
	if w.passes != 3 || !slices.Equal(w.checkOrder, []string{"/x", "/x/x", "/x/y"}) || w.seen["/x"] != walkedLive || len(w.issues) > 0 {
		t.Errorf("stale: %d passes, checked apart %q, /x walked %d, issues %+v", w.passes, w.checkOrder, w.seen["/x"], w.issues)
	}
	runFollowCases(t, []followCase{{name: "stale", schema: stale, valid: []string{`"s"`}, invalid: []string{`5`}, ordered: true}})
	// Each pass starts its state anew (touch): after the three passes of
	// the stale document, the root resource lists each of its
	// $dynamicAnchor names once, as the last pass recorded them.
	w = walkOf(strings.Replace(stale, `{"$ref"`, `{"$defs":{"d0":{"$dynamicAnchor":"a0"},"d1":{"$dynamicAnchor":"a1"}},"$ref"`, 1))
	var names []string
	for _, name := range w.nodes[rootNode].dynamic {
		names = append(names, w.strOf[name])
	}
	if w.passes != 3 || !slices.Equal(names, []string{"a0", "a1"}) || len(w.issues) > 0 {
		t.Errorf("stale with $dynamicAnchor schemas: %d passes, the root lists %q, issues %+v, want 3 passes and a0, a1", w.passes, names, w.issues)
	}
	// A reference at a position the last pass leaves inert is unresolved,
	// with no URL, whatever an earlier pass that walked it live resolved
	// it to (finish; refSite.res): the library never compiles it.
	w = walkOf(inertAgain)
	i := slices.IndexFunc(w.sites, func(s refSite) bool { return s.ptr == "/q/additionalItems/$ref" })
	if w.passes != 2 || i < 0 || w.seen["/q/additionalItems"] != walkedInert || w.sites[i].res.status != refUnresolved || w.sites[i].url != "" {
		t.Errorf("inertAgain: %d passes, sites %+v, /q/additionalItems walked %d, want 2 passes and its reference unresolved", w.passes, w.sites, w.seen["/q/additionalItems"])
	}
	first := newWalker(w.doc)
	first.beginPass()
	first.collect(rootNode, baseURLIndex, rootNode, stepKeywordTree)
	first.close()
	first.finish()
	if j := slices.IndexFunc(first.sites, func(s refSite) bool { return s.ptr == "/q/additionalItems/$ref" }); j < 0 || first.sites[j].res.status != refResolved {
		t.Errorf("inertAgain's first pass: sites %+v, want /q/additionalItems/$ref resolved", first.sites)
	}
	// Each pass finds one more generation: 229 generations take 230
	// passes, 400 more than the budget allows.
	if w := walkOf(generations(229)); w.passes != 230 || w.stopped {
		t.Errorf("generations(229): %d passes, stopped %v, want 230 within the budget", w.passes, w.stopped)
	}
	g := generations(400)
	w = walkOf(g)
	if len(w.issues) != 1 || w.issues[0].Message != message(fmt.Sprintf(msgFollowBudget, compileBudget)) || !generationRef(w.issues[0].Pointer) {
		t.Fatalf("generations(400) = %+v, want the budget at a generation's $ref", w.issues)
	}
	// The finding says what was charged, the passes after the first, and
	// no more.
	if want := "following the reference targets outside the schema's keywords takes passes after the first that spend more than the 1048576 units of the walk-time compile budget (move the schemas the references name under $defs)"; w.issues[0].Message != want {
		t.Errorf("generations(400): %q, want %q", w.issues[0].Message, want)
	}
	refusedThrough(t, g, w.issues)
	// Refused past the budget, the walk still reports what the pass it
	// stopped in collected before it stopped, a duplicate anchor of the
	// root's resource here, ahead of the budget's finding (walk).
	dupAnchors := `"k1":{"$anchor":"k"},"k2":{"$anchor":"k"},`
	gd := strings.Replace(g, `"$defs":{`, `"$defs":{`+dupAnchors, 1)
	dupAnchor := Issue{"/$defs/k2/$anchor", `anchor "k" is already defined by the schema at "/$defs/k1"`}
	w = walkOf(gd)
	if len(w.issues) != 2 || w.issues[0] != dupAnchor || w.issues[1].Message != message(fmt.Sprintf(msgFollowBudget, compileBudget)) || !generationRef(w.issues[1].Pointer) {
		t.Errorf("generations(400) with a duplicate anchor = %+v, want %+v and the budget at a generation's $ref", w.issues, dupAnchor)
	}
	refusedThrough(t, gd, w.issues)
	// Past MaxSubschemas positions the walk ends with the positions the
	// last pass collected.
	w = walkOf(`{"allOf":[` + joinRepeat(`{}`, MaxSubschemas) + `]}`)
	if len(w.issues) != 1 || w.issues[0].Message != fmt.Sprintf("schema has more than %d subschemas", MaxSubschemas) || len(w.positions) != MaxSubschemas+1 || w.positions[0] != "" {
		t.Errorf("MaxSubschemas: issues %+v, %d positions", w.issues, len(w.positions))
	}
	// And it reports what that pass collected before, ahead of the bound's
	// finding.
	over := `{"$defs":{` + dupAnchors[:len(dupAnchors)-1] + `},"allOf":[` + joinRepeat(`{}`, MaxSubschemas) + `]}`
	w = walkOf(over)
	if want := []Issue{{"/$defs/k2/$anchor", `anchor "k" is already defined by the schema at "/$defs/k1"`}, {"", fmt.Sprintf("schema has more than %d subschemas", MaxSubschemas)}}; !slices.Equal(w.issues, want) {
		t.Errorf("MaxSubschemas with a duplicate anchor: issues %+v, want %+v", w.issues, want)
	}
	refusedThrough(t, over, w.issues)
	// Deterministic: the same model twice.
	for _, schema := range []string{stale, generations(3), ancestorCases()[4].schema, livenessCases()[17].schema} {
		a, b := walkOf(schema), walkOf(schema)
		if !slices.Equal(a.positions, b.positions) || !slices.Equal(a.checkOrder, b.checkOrder) || !slices.Equal(a.issues, b.issues) || !maps.Equal(a.seen, b.seen) {
			t.Errorf("two walks of %.80s differ", schema)
		}
	}
}

// flatTargets returns a document of k references in allOf to k members of
// its root that are no keywords, each named by i in decimal, padded with
// zeros to l bytes: an ordinary document of two passes, whose targets'
// bases never change (charging both tokens of every comparison for placing
// its targets would spend 1.47 million units on flatTargets(500, 210),
// 227,511 bytes, and refuse it).
func flatTargets(k, l int) string {
	refs, members := make([]string, k), make([]string, k)
	for i := range k {
		name := fmt.Sprintf("%0*d", l, i)
		refs[i] = `{"$ref":"#/` + name + `"}`
		members[i] = `"` + name + `":{"type":"string"}`
	}
	return `{"allOf":[` + strings.Join(refs, ",") + `],` + strings.Join(members, ",") + `}`
}

// generationRef reports whether ptr is the $ref of a generation's target
// x, in generations or deepGenerations.
func generationRef(ptr string) bool {
	return regexp.MustCompile(`^/\$defs(/L+/\$defs)?/n[0-9]+/a[0-9]+/x/\$ref$`).MatchString(ptr)
}

// TestFollowFixpointCost covers what the walk's fixpoint costs and what it
// charges to the walk-time compile budget (walker.walk, model.go): passes
// that hashed every prefix of every pointer and built every target's
// pointer again would cost passes times pointer bytes, so that
// deepGenerations(229, 200000) would take 30 s and 31 GB for 228 passes
// under its 200,000-byte member name. The first pass charges nothing and
// every later one its steps and the bytes it meets anew; placing the
// targets a pass found charges nothing, since charging both tokens of
// every comparison would refuse ordinary two-pass documents near
// MaxSchemaBytes with hundreds of long-named targets outside the keywords,
// flatTargets(500, 210) and the like, at up to 1.47 million units. The
// steps do not depend on the length of the pointers: under a member name
// of 1 to 4,000 bytes the passes take the same steps, and the bytes grow
// by the name's length only once for each position and reference a later
// pass meets anew (two of each for every generation after the first),
// while they grow with the generations, not with the passes times the
// generations; and a two-pass document spends its second pass's steps
// only, however long its targets' names, each target placed once. The
// budget holds to the unit: a walk within it is the unbounded walk, and
// one unit less refuses it at the reference to the first new target of the
// pass before the one the budget ends in; past it the walk stops at once.
// At the default budget the generation documents are refused, or walked as
// without it, as they spend, the ordinary two-pass documents are accepted,
// and the library alone refuses the generations too; and what the walk
// allocates grows with its pointers, not with its passes times its
// pointers (a counter, never a time).
func TestFollowFixpointCost(t *testing.T) {
	docs := map[string]any{}
	walkWith := func(schema string, budget int) *walker {
		t.Helper()
		doc, ok := docs[schema]
		if !ok {
			p, issues := prepare([]byte(schema))
			if len(issues) > 0 {
				t.Fatal(issues)
			}
			doc, docs[schema] = p.doc, p.doc
		}
		w := newWalker(doc)
		w.budget = budget
		w.walk()
		return w
	}
	const unbounded = math.MaxInt
	if w := walkWith(`{"$ref":"#/$defs/a","$defs":{"a":{"type":"string"}}}`, unbounded); w.passes != 1 || w.spent != 0 {
		t.Errorf("one pass: %d passes spent %d, want 1 and nothing", w.passes, w.spent)
	}
	if w := walkWith(`{"$ref":"#/x","x":{"type":"string"}}`, unbounded); w.passes != 2 || w.steps == 0 || w.bytes != 0 || w.spent != w.steps {
		t.Errorf("two passes: %d passes, spent %d: %d steps and %d bytes, want 2, the second pass's steps alone", w.passes, w.spent, w.steps, w.bytes)
	}
	// A two-pass document of k targets outside the keywords spends the same
	// steps whatever the length of their names, and no byte: its second
	// pass repeats the first, and each target is placed once.
	for _, kl := range [][2]int{{1, 100000}, {2, 60000}, {60, 2000}, {500, 210}} {
		k := kl[0]
		short := walkWith(flatTargets(k, 1), unbounded)
		for _, l := range []int{1, 2, kl[1]} {
			w := walkWith(flatTargets(k, l), unbounded)
			if w.passes != 2 || w.bytes != 0 || w.spent != w.steps || w.steps != short.steps || w.placed != k || len(w.checkOrder) != k || len(w.issues) > 0 {
				t.Errorf("flatTargets(%d, %d): %d passes, %d steps (%d with 1-byte names), %d bytes, %d placed, %d checked apart, issues %.200v, want 2 passes, the same steps, no byte and each target once", k, l, w.passes, w.steps, short.steps, w.bytes, w.placed, len(w.checkOrder), w.issues)
			}
		}
	}
	for _, g := range []int{1, 3, 50} {
		base := walkWith(deepGenerations(g, 1), unbounded)
		for _, long := range []int{1000, 4000} {
			w := walkWith(deepGenerations(g, long), unbounded)
			if w.passes != g+1 || w.steps != base.steps || w.bytes-base.bytes != 4*(g-1)*(long-1) {
				t.Errorf("deepGenerations(%d, %d): %d passes, %d steps and %d bytes, want %d passes, %d steps and %d bytes", g, long, w.passes, w.steps, w.bytes, g+1, base.steps, base.bytes+4*(g-1)*(long-1))
			}
			// Each generation's two targets, x and its a, placed once.
			if w.placed != 2*g {
				t.Errorf("deepGenerations(%d, %d): placed %d targets, want each of the %d once", g, long, w.placed, 2*g)
			}
		}
	}
	// MaxSubschemas positions holding three references each to one target
	// outside the keywords: the second pass's steps, about 61,000 units.
	if w := walkWith(`{"allOf":[`+joinRepeat(`{"$ref":"#/x","$dynamicRef":"#/x","$recursiveRef":"#/x"}`, MaxSubschemas-2)+`],"x":{}}`, unbounded); w.passes != 2 || w.bytes != 0 || len(w.positions) != MaxSubschemas || w.spent < 58000 || w.spent > 64000 || len(w.issues) > 0 {
		t.Errorf("three references at each of MaxSubschemas positions: %d passes, %d positions, spent %d (%d bytes), issues %.200v, want 2 passes and about 61,000 steps", w.passes, len(w.positions), w.spent, w.bytes, w.issues)
	}
	if b50, b100 := walkWith(generations(50), unbounded).bytes, walkWith(generations(100), unbounded).bytes; b100 > 5*b50/2 {
		t.Errorf("generations: %d bytes for 50, %d for 100, want them to grow with the generations", b50, b100)
	}
	// The budget to the unit, and the reference at which it refuses.
	const stale = `{"$ref":"#/x/y","x":{"$id":"https://x.test/","y":{"$ref":"#/x"},"x":{"type":"string"}}}`
	for _, tc := range []struct {
		name, schema string
		refs         []string
	}{
		{"a target outside the keywords", `{"$ref":"#/x","x":{"type":"string"}}`, []string{"/$ref"}},
		{"stale", stale, []string{"/$ref", "/x/y/$ref"}},
		{"generations(3)", generations(3), []string{"/$ref", "/$defs/n0/a1/x/$ref", "/$defs/n1/a2/x/$ref"}},
	} {
		free := walkWith(tc.schema, unbounded)
		if w := walkWith(tc.schema, free.spent); w.stopped || !slices.Equal(w.issues, free.issues) || w.spent != free.spent {
			t.Errorf("%s within the budget %d: stopped %v, spent %d, issues %+v, want %+v", tc.name, free.spent, w.stopped, w.spent, w.issues, free.issues)
		}
		// The unbounded walk's marks: what it had spent at the end of each
		// pass, nothing at the end of the first. A budget that ends in pass
		// k, at or past mark k-2 and below mark k-1, refuses at the
		// reference to the first target pass k-1 found, tc.refs[k-2].
		if len(free.marks) != free.passes || free.marks[0] != 0 || len(tc.refs) != free.passes-1 {
			t.Fatalf("%s: marks %v for %d passes", tc.name, free.marks, free.passes)
		}
		seen := map[string]bool{}
		for budget := range free.spent {
			w := walkWith(tc.schema, budget)
			if !w.stopped || w.spent <= budget || len(w.issues) != 1 || w.issues[0].Message != message(fmt.Sprintf(msgFollowBudget, budget)) {
				t.Fatalf("%s at budget %d: stopped %v, spent %d, issues %+v", tc.name, budget, w.stopped, w.spent, w.issues)
			}
			m := slices.IndexFunc(free.marks, func(spent int) bool { return spent > budget })
			if want := tc.refs[m-1]; w.issues[0].Pointer != want {
				t.Errorf("%s at budget %d (mark %d of %v): refused at %q, want %q", tc.name, budget, m, free.marks, w.issues[0].Pointer, want)
			}
			seen[w.issues[0].Pointer] = true
		}
		if len(seen) != len(tc.refs) {
			t.Errorf("%s: refused at %d references as the budget grows, want %q", tc.name, len(seen), tc.refs)
		}
	}
	// Past the budget the walk stops: what it spends after is the rest of
	// the position or reference in hand and the calls that then return at
	// once, never the rest of a pass, of the targets it goes through or
	// collects apart, of the positions it walks live, of the $dynamicAnchor
	// schemas of a resource or of the references of a position. A pass of
	// generations(20) spends about 400 to 850 units, the second pass of
	// each other document 1,000 to 2,500; each bound below is above the
	// most any budget leaves, 58 units for the generations, whose later
	// passes meet a reference's URL or fragment anew in one charge, and 3
	// for each other document, and the rows together cover the stops of
	// collect and of its members, of the loop over the targets found, of
	// close's queue, of makeLive, of the $dynamicAnchor schemas and of the
	// references.
	var refs, members, props, dynamic, threes []string
	for i := range 300 {
		props = append(props, fmt.Sprintf(`"p%d":{}`, i))
		dynamic = append(dynamic, fmt.Sprintf(`"d%d":{"$dynamicAnchor":"a%d"}`, i, i))
		if i < 60 {
			refs, members = append(refs, fmt.Sprintf(`{"$ref":"#/t%d"}`, i)), append(members, fmt.Sprintf(`"t%d":{}`, i))
		}
		if i < 100 {
			threes = append(threes, `{"$ref":"#/x","$dynamicRef":"#/x","$recursiveRef":"#/x"}`)
		}
	}
	for _, tc := range []struct {
		name, schema string
		every, most  int
	}{
		{"generations(20)", generations(20), 13, 72},
		{"60 targets outside the keywords", `{"allOf":[` + strings.Join(refs, ",") + `],` + strings.Join(members, ",") + `}`, 1, 8},
		{"a target of 300 members", `{"$ref":"#/x","x":{"properties":{` + strings.Join(props, ",") + `}}}`, 1, 8},
		{"a resource of 300 $dynamicAnchor schemas", `{"$defs":{` + strings.Join(dynamic, ",") + `},"$ref":"#/x","x":{}}`, 1, 8},
		{"100 positions of three references", `{"allOf":[` + strings.Join(threes, ",") + `],"x":{}}`, 1, 6},
	} {
		over, total := 0, walkWith(tc.schema, unbounded).spent
		for budget := 0; budget < total; budget += tc.every {
			w := walkWith(tc.schema, budget)
			if !w.stopped {
				t.Fatalf("%s at budget %d: not stopped", tc.name, budget)
			}
			over = max(over, w.spent-w.stoppedAt)
		}
		if over > tc.most {
			t.Errorf("%s: spent up to %d units after it stopped, want at most %d", tc.name, over, tc.most)
		}
	}
	// The library alone refuses the generations too: compiling from the
	// root, it bases each generation's target on the resource enclosing it
	// before it collects the target that encloses it, and finds no pointer
	// there (doc.go, "Compiled set").
	for _, g := range []int{1, 3, 229} {
		doc, err := sjs.UnmarshalJSON(strings.NewReader(generations(g)))
		if err != nil {
			t.Fatal(err)
		}
		c := sjs.NewCompiler()
		c.DefaultDraft(sjs.Draft2020)
		c.UseLoader(refusingLoader{})
		if err := c.AddResource(baseURL, doc); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Compile(baseURL); err == nil || !strings.Contains(err.Error(), "/$defs/n0/a2/x") {
			t.Errorf("the library alone on generations(%d): %v, want the pointer /$defs/n0/a2/x not found", g, err)
		}
	}
	// The default budget.
	for _, tc := range []struct {
		name, schema string
		refused      bool
	}{
		{"generations(229)", generations(229), false},
		{"generations(400)", generations(400), true},
		{"deepGenerations(200, 100)", deepGenerations(200, 100), false},
		{"deepGenerations(232, 100)", deepGenerations(232, 100), false},
		{"deepGenerations(240, 100)", deepGenerations(240, 100), true},
		{"deepGenerations(3, 50000)", deepGenerations(3, 50000), false},
		{"deepGenerations(3, 200000)", deepGenerations(3, 200000), true},
		{"deepGenerations(229, 200000)", deepGenerations(229, 200000), true},
	} {
		w, free := walkWith(tc.schema, compileBudget), walkWith(tc.schema, unbounded)
		if !tc.refused {
			if w.stopped || free.spent > compileBudget || !slices.Equal(w.issues, free.issues) {
				t.Errorf("%s: stopped %v, spent %d of %d, issues %.200v", tc.name, w.stopped, free.spent, compileBudget, w.issues)
			}
			continue
		}
		if !w.stopped || free.spent <= compileBudget || len(w.issues) != 1 || w.issues[0].Message != message(fmt.Sprintf(msgFollowBudget, compileBudget)) || !generationRef(w.issues[0].Pointer) {
			t.Errorf("%s: stopped %v, spent %d of %d, issues %.200v, want the budget at a generation's $ref", tc.name, w.stopped, free.spent, compileBudget, w.issues)
		}
		// The walk ends with the model of the pass it stopped in.
		if len(w.positions) == 0 || w.positions[0] != "" || w.seen[""] == 0 {
			t.Errorf("%s: refused with %d positions", tc.name, len(w.positions))
		}
		if len(tc.schema) < 64<<10 {
			refusedThrough(t, tc.schema, w.issues)
		}
	}
	// Ordinary two-pass documents near MaxSchemaBytes, each 227 to 251 KB
	// with hundreds of long-named targets outside the keywords, are
	// accepted at the default budget and spend no byte.
	for _, kl := range [][2]int{{500, 210}, {450, 240}, {400, 280}, {350, 330}, {300, 400}, {250, 480}, {200, 600}} {
		w := walkWith(flatTargets(kl[0], kl[1]), compileBudget)
		if w.stopped || len(w.issues) > 0 || w.passes != 2 || w.bytes != 0 || len(w.checkOrder) != kl[0] {
			t.Errorf("flatTargets(%d, %d) at the default budget: stopped %v, %d passes, spent %d (%d bytes), checked apart %d, issues %.200v, want accepted in two passes without a byte", kl[0], kl[1], w.stopped, w.passes, w.spent, w.bytes, len(w.checkOrder), w.issues)
		}
	}
	if _, err := CompileSchema([]byte(flatTargets(500, 210))); err != nil {
		t.Errorf("CompileSchema(flatTargets(500, 210)) = %.300v", err)
	}
	// The allocation: 51 passes under a 4,000-byte name allocate about 1.5
	// times the bytes of the pointers kept, where building the pointers
	// again every pass would allocate 34 times them (35 MB); so do they
	// under a name of 2,000 slashes, each pointer token escaped to 4,000
	// bytes, which no pass unescapes and escapes again, and next to 20
	// duplicate anchors under the name, whose findings no pass renders
	// again.
	slashes := strings.Replace(deepGenerations(50, 2000), strings.Repeat("L", 2000), strings.Repeat("/", 2000), 1)
	var anchors []string
	for i := range 20 {
		anchors = append(anchors, fmt.Sprintf(`"d%d":{"$anchor":"z"}`, i))
	}
	dup := strings.Replace(deepGenerations(50, 4000), `":{"$defs":{`, `":{"$defs":{`+strings.Join(anchors, ",")+`,`, 1)
	for _, tc := range []struct{ name, schema string }{
		{"deepGenerations(50, 4000)", deepGenerations(50, 4000)},
		{"deepGenerations(50, 2000) under slashes", slashes},
		{"deepGenerations(50, 4000) with duplicate anchors", dup},
	} {
		p, _ := prepare([]byte(tc.schema))
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		w := newWalker(p.doc)
		w.walk()
		runtime.ReadMemStats(&after)
		kept := 0
		for _, ptr := range w.positions {
			kept += len(ptr)
		}
		for _, s := range w.sites {
			kept += len(s.ptr)
		}
		if n := after.TotalAlloc - before.TotalAlloc; w.passes != 51 || w.stopped || n > uint64(2*kept+2<<20) {
			t.Errorf("%s: %d passes (stopped %v) allocated %d bytes for %d bytes of pointers, want 51 within %d", tc.name, w.passes, w.stopped, n, kept, 2*kept+2<<20)
		}
	}
}

// benignTwin returns schema with every payload of the follow cases that a
// rule of the walk refuses, and every $schema, replaced by one no rule
// refuses, so the differential test also runs the library on the routes
// of the refused shapes.
func benignTwin(schema string) string {
	pairs := []string{
		typeBomb(), `{"type":"string"}`,
		`"minimum":1e99999`, `"minimum":1`,
		`"maximum":1e99999`, `"maximum":1`,
		strings.Repeat("[a-z]{1000}", 30), `^a`,
		`"multipleOf":0`, `"multipleOf":1`,
		`"required":[1]`, `"required":["a"]`,
		`"type":"strng"`, `"type":"string"`,
		`"pattern":"(?=a)"`, `"pattern":"a"`,
		joinRepeat(`{}`, MaxSubschemas), `{}`,
	}
	out := strings.NewReplacer(pairs...).Replace(schema)
	for {
		i := strings.Index(out, `"$schema":"`)
		if i < 0 {
			return out
		}
		j := strings.IndexByte(out[i+len(`"$schema":"`):], '"')
		out = out[:i] + `"$comment":"x"` + out[i+len(`"$schema":"`)+j+1:]
	}
}

// TestWalkMatchesLibrary is the differential test of the walk's model,
// over every follow case and its benign twin: the library, compiling in
// the compile's order what the walk walked live (compileLibrary), never
// asks the loader for a document the walk accepts and compiles exactly the
// positions the walk walked live; and for a document the walk refuses only
// at references, which the library would load or fail to resolve, the
// library fails too, loading at most the URLs the walk refused. A shape
// the walk refuses by another rule, which protects the library, is left to
// its twin.
func TestWalkMatchesLibrary(t *testing.T) {
	var cases []followCase
	for _, set := range [][]followCase{baseCases(), lateIDCases(), ancestorCases(), policyDialectCases(), livenessCases(), visibilityCases(), olderDraftCases()} {
		cases = append(cases, set...)
	}
	cases = append(cases, followCase{name: "stale", schema: `{"$ref":"#/x/y","x":{"$id":"https://x.test/","y":{"$ref":"#/x"},"x":{"type":"string"}}}`})
	ran := 0
	for _, tc := range cases {
		for _, schema := range slices.Compact([]string{tc.schema, benignTwin(tc.schema)}) {
			if differential(t, tc.name, schema) {
				ran++
			}
		}
	}
	if ran < len(cases) {
		t.Errorf("the differential ran %d documents for %d cases", ran, len(cases))
	}
}

// differential runs one document of TestWalkMatchesLibrary and reports
// whether the library ran on it.
func differential(t *testing.T, name, schema string) bool {
	t.Helper()
	p, issues := prepare([]byte(schema))
	if len(issues) > 0 {
		t.Fatalf("%s: %+v", name, issues)
	}
	w := newWalker(p.doc)
	w.walk()
	// The references the walk refuses, and the URLs the library would load
	// for them. A reference to a bundled meta-schema the library serves
	// without the loader, so only the compile refuses it.
	refused := map[string]bool{}
	var urls []string
	bundled := 0
	for _, s := range w.sites {
		if w.seen[s.pos] != walkedLive || s.res.status == refResolved {
			continue
		}
		refused[s.ptr] = true
		if s.res.status == refOutside {
			u := s.url
			urls = append(urls, u)
			if strings.HasPrefix(u, "https://json-schema.org/") {
				bundled++
			}
		}
	}
	for _, is := range w.issues {
		if !refused[is.Pointer] {
			return false
		}
	}
	spy := &spyLoader{}
	var c *sjs.Compiler
	var libIssues []Issue
	noPanic(t, name+": compileLibrary", func() {
		c, _, libIssues = compileLibrary(p, w, spy, newRegexpEngine())
	})
	for _, u := range spy.urls {
		if !slices.Contains(urls, u) {
			t.Errorf("%s: the library loaded %q, which no reference the walk refused names (%q)", name, u, urls)
		}
	}
	if len(w.issues) > 0 {
		if libIssues == nil && bundled < len(w.issues) {
			t.Errorf("%s: the library compiled %.120s, whose references %+v the walk refuses", name, schema, w.issues)
		}
		return true
	}
	if c == nil {
		// The library, or the compile's own check of what the library
		// leaves unchecked, refused the document's content.
		return true
	}
	list, ok := compiledSchemas(c)
	if !ok {
		t.Fatalf("%s: compiledSchemas failed", name)
	}
	var compiled, live []string
	for _, cs := range list {
		compiled = append(compiled, cs.ptr)
	}
	for _, ptr := range w.positions {
		if w.seen[ptr] == walkedLive {
			live = append(live, ptr)
		}
	}
	slices.SortFunc(live, func(a, b string) int {
		if d := comparePointers(a, b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	if !slices.Equal(compiled, live) {
		t.Errorf("%s: the library compiled %q, the walk walked live %q", name, compiled, live)
	}
	return true
}
