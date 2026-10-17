// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// assertingVocabulary is a root $vocabulary listing the format-assertion
// vocabulary: a resource whose $schema names the policy document takes its
// vocabularies from it (library audit CC-01).
const assertingVocabulary = `"$vocabulary":{"https://json-schema.org/draft/2020-12/vocab/core":true,` +
	`"https://json-schema.org/draft/2020-12/vocab/applicator":true,` +
	`"https://json-schema.org/draft/2020-12/vocab/validation":true,` +
	`"https://json-schema.org/draft/2020-12/vocab/format-assertion":true}`

// coreVocabulary is a root $vocabulary listing the core vocabulary alone,
// so a resource whose $schema names the policy document loses the
// validation and applicator keywords (library audit CC-04).
const coreVocabulary = `"$vocabulary":{"https://json-schema.org/draft/2020-12/vocab/core":true}`

// formatAssertionMeta is the bundled meta-schema whose $vocabulary lists
// the core and format-assertion vocabularies alone, so a resource under it
// also loses the validation and applicator keywords (library audit CC-04).
const formatAssertionMeta = "https://json-schema.org/draft/2020-12/meta/format-assertion"

// dynamicTarget is the walked resource of the dynamic-scope shapes, under
// the root's $defs: its $dynamicRef to its own $dynamicAnchor resolves, by
// the dynamic scope, to the outermost resource in scope that has one, the
// resource on an escaped route that applies it (validator.go:781-813).
const dynamicTarget = `"y":{"$id":"urn:y","$dynamicAnchor":"n","properties":{"a":{"$dynamicRef":"#n"}}}`

// escapedRoutes are the routes of library audit CC-01 to a position the
// library compiles and validation evaluates but the rule walk did not
// walk: a $recursiveRef target, which the walk follows since class GC-01
// was fixed, and a JSON Pointer target the library bases on an embedded
// resource the walk did not, which the walk walks with the library's base
// since class GC-02 was fixed; on both the walk's own rules now refuse
// what the compile's later checks refused (compiled.go).
func escapedRoutes() []string { return []string{recursiveRoute, mismatchRoute} }

// recursiveRoute is the escaped route of a $recursiveRef target (class
// GC-01).
const recursiveRoute = "$recursiveRef target"

// mismatchRoute is the base-mismatch route of onRoute (class GC-02): a JSON
// Pointer reference to a position under the embedded resource urn:r
// outside its keyword tree, which the library bases on urn:r, so the
// target's reference #/w names /$defs/r/w, not the root's /w.
const mismatchRoute = "base-mismatch target"

// onRoute places resource on route, with root holding the document root's
// other members and defs the other members of its $defs, and returns the
// document and the resource's pointer.
func onRoute(route, root, defs, resource string) (doc, at string) {
	if root != "" {
		root += ","
	}
	if route == recursiveRoute {
		if defs != "" {
			defs = `,"$defs":{` + defs + `}`
		}
		return `{` + root + `"$recursiveRef":"#/x","x":` + resource + defs + `}`, "/x"
	}
	if defs != "" {
		defs = "," + defs
	}
	return `{` + root + `"$defs":{"r":{"$id":"urn:r","x":{"$ref":"#/w"},"w":` + resource + `}` + defs + `},"w":true,"$ref":"#/$defs/r/x"}`, "/$defs/r/w"
}

// dialectSource is a way a $schema gives a resource a dialect other than
// Draft's: root is the document root's other members, url the $schema.
type dialectSource struct{ name, root, url string }

// assertingSources give a resource the format-assertion vocabulary
// (library audit CC-01).
func assertingSources() []dialectSource {
	return []dialectSource{
		{"root $vocabulary", assertingVocabulary, baseURL},
		{"bundled meta/format-assertion", "", formatAssertionMeta},
	}
}

// subsetSources give a resource a dialect without the validation and
// applicator vocabularies (library audit CC-04).
func subsetSources() []dialectSource {
	return []dialectSource{
		{"root $vocabulary of core", coreVocabulary, baseURL},
		{"bundled meta/format-assertion", "", formatAssertionMeta},
	}
}

// probedFormats are the formats library audit CC-01 probed on bodies.
func probedFormats() []string {
	return []string{"regex", "email", "hostname", "date-time", "uuid", "json-pointer"}
}

// dialectCase is a document refused at the compile with exactly want.
type dialectCase struct {
	name, schema string
	want         []Issue
}

// dialectShapes places body, the keywords of the schema whose dialect
// matters, on both escaped routes in every way a $schema reaches it that
// the walk did not walk, with src giving the dialect: the resource
// on the route itself; a $dynamicAnchor schema of it, which only the
// dynamic scope reaches (check finding: compiled eagerly with the
// resource, compiler.go:223-236, it was never inspected); the target of a
// reference out of such an anchor schema, outside the resource's keyword
// tree; and a resource the route reaches whose dialect comes from an
// enclosing resource the library collects but never compiles
// (roots.go:139-145). Each is refused at the $schema that gives the
// dialect, as the walk refuses it where it walks.
func dialectShapes(prefix string, src dialectSource, body string) []dialectCase {
	var out []dialectCase
	for _, route := range escapedRoutes() {
		add := func(name, defs, resource, below string) {
			doc, at := onRoute(route, src.root, defs, resource)
			out = append(out, dialectCase{route + "/" + src.name + "/" + prefix + name, doc, []Issue{{at + below + "/$schema", msgDialectText}}})
		}
		sch := `"$schema":"` + src.url + `"`
		add("at the resource", "", `{"$id":"urn:w",`+sch+`,`+body+`}`, "")
		add("in dynamic scope", dynamicTarget, `{"$id":"urn:w",`+sch+`,"$defs":{"d":{"$dynamicAnchor":"n",`+body+`}},"$ref":"urn:y"}`, "")
		add("behind a dynamic anchor", dynamicTarget,
			`{"$id":"urn:w","$defs":{"d":{"$dynamicAnchor":"n","$ref":"#/z"}},"z":{"$id":"urn:z",`+sch+`,`+body+`},"$ref":"urn:y"}`, "/z")
		add("inherited", "", `{"$defs":{"p":{"$id":"urn:p",`+sch+`,"$defs":{"c":{"$id":"urn:c",`+body+`}}}},"$ref":"urn:c"}`, "/$defs/p")
	}
	return out
}

// msgDialectText is the walk's $schema finding, written out so a change of
// its wording fails the tests.
const msgDialectText = "$schema must be https://json-schema.org/draft/2020-12/schema"

// assertedFormatCases are documents in which the library would compile
// format as an assertion, each refused at the $schema that gives the
// dialect: every format library audit CC-01 probed (the CC-02 and CC-03
// shapes are the regex and json-pointer rows) in every shape of
// dialectShapes under both format-assertion sources; a format in a
// subschema of the resource; a sibling type keyword under the bundled
// meta/format-assertion; the dynamic-scope seed of the check finding,
// whose $dynamicRef sits under items; and resources of the drafts before
// 2019-09, which assert every format (objcompiler.go:436-438), with the
// whole list of findings pinned (minor check finding: the list named the
// format-assertion vocabulary as the cause).
func assertedFormatCases() []dialectCase {
	var out []dialectCase
	for _, src := range assertingSources() {
		for _, f := range probedFormats() {
			out = append(out, dialectShapes(f+"/", src, `"format":"`+f+`"`)...)
		}
		for _, route := range escapedRoutes() {
			sch := `"$schema":"` + src.url + `"`
			doc, at := onRoute(route, src.root, "", `{"$id":"urn:w",`+sch+`,"properties":{"a":{"format":"email"}}}`)
			out = append(out, dialectCase{route + "/" + src.name + "/format in a subschema", doc, []Issue{{at + "/$schema", msgDialectText}}})
			doc, at = onRoute(route, src.root, "", `{"$id":"urn:w",`+sch+`,"format":"regex","type":"integer"}`)
			out = append(out, dialectCase{route + "/" + src.name + "/format beside type", doc, []Issue{{at + "/$schema", msgDialectText}}})
			doc, at = onRoute(route, src.root, `"y":{"$id":"urn:y","$dynamicAnchor":"n","items":{"$dynamicRef":"#n"}}`,
				`{"$id":"urn:x",`+sch+`,"$defs":{"d":{"$dynamicAnchor":"n","format":"regex"}},"$ref":"urn:y"}`)
			out = append(out, dialectCase{route + "/" + src.name + "/dynamic scope under items", doc, []Issue{{at + "/$schema", msgDialectText}}})
		}
	}
	for _, route := range escapedRoutes() {
		for _, d := range []struct{ name, resource string }{
			{"draft-04", `{"id":"urn:w","$schema":"http://json-schema.org/draft-04/schema#","format":"regex"}`},
			{"draft-06", `{"$id":"urn:w","$schema":"http://json-schema.org/draft-06/schema#","format":"regex"}`},
			{"draft-07", `{"$id":"urn:w","$schema":"http://json-schema.org/draft-07/schema#","format":"regex"}`},
			{"draft-07 email", `{"$id":"urn:w","$schema":"http://json-schema.org/draft-07/schema","format":"email"}`},
		} {
			doc, at := onRoute(route, "", "", d.resource)
			out = append(out, dialectCase{route + "/" + d.name, doc, []Issue{{at + "/$schema", msgDialectText}}})
		}
	}
	return out
}

// vocabularySubsetCases are documents in which the library would compile
// a schema without its validation and applicator keywords, with no format
// anywhere, each refused at the $schema that gives the dialect, in every
// shape of dialectShapes under both sources (library audit CC-04: the
// first three of the check finding's documents compiled and accepted the
// body "s" or {"a":1}).
func vocabularySubsetCases() []dialectCase {
	var out []dialectCase
	for _, src := range subsetSources() {
		out = append(out, dialectShapes("", src, `"type":"integer","properties":{"a":false}`)...)
	}
	return out
}

// refusedEverywhere checks that schema is RZ-CFG-005 with exactly the
// findings want through the compile, CompileSchema, Check and
// Factory.Build, and that nothing was loaded.
func refusedEverywhere(t *testing.T, schema string, want []Issue) {
	t.Helper()
	spy := &spyLoader{}
	s, doc, issues := compileRaw([]byte(schema), spy)
	if s != nil || !slices.Equal(issues, want) {
		t.Fatalf("compile(%s) compiled %v with %+v, want %+v", schema, s != nil, issues, want)
	}
	if len(spy.urls) > 0 {
		t.Errorf("loader saw %q, want no load", spy.urls)
	}
	_, err := CompileSchema([]byte(schema))
	var ce *CompileError
	if !errors.As(err, &ce) || !slices.Equal(ce.Issues, want) {
		t.Errorf("CompileSchema = %v, want %+v", err, want)
	}
	type diagnostic struct{ code, message, path string }
	var got, wantDiags []diagnostic
	for _, d := range runCheck(policyResource("body", schema)) {
		got = append(got, diagnostic{d.Code, d.Message, d.Path.String()})
	}
	for _, is := range want {
		wantDiags = append(wantDiags, diagnostic{CodeConfig, is.Message, schemaPath(configSchemaPath(), doc, is.Pointer).String()})
	}
	if !slices.Equal(got, wantDiags) {
		t.Errorf("Check = %+v, want %+v", got, wantDiags)
	}
	f, err := Factory{}.Build(t.Context(), filter.BuildEnv{Config: configOf(schema), Limits: filter.Limits{MaxRequestBodyBytes: testLimit}})
	if code, _ := errcode.CodeOf(err); f != nil || code != CodeConfig || !strings.Contains(err.Error(), want[0].Message+" at "+want[0].Pointer) {
		t.Errorf("Build = %v, %v, want %s at %s", f, err, CodeConfig, want[0].Pointer)
	}
}

// TestFormatAssertionRefused covers audit class GC-21 (rows CC-01, CC-02,
// CC-03, CC-04, VLU-41, CMP-58, VAL-13, CX-28; 07 req 75): format stays an
// annotation on every route, so a document in which the library would
// compile it as an assertion, whatever route and dialect, is RZ-CFG-005
// through CompileSchema, Check and Factory.Build, without a load, at the
// $schema that gives the dialect (checkCollected applies the walk's rule
// there, check finding: the $dynamicAnchor schemas of a resource on an
// escaped route escaped the inspection), so no Filter is built and no
// request runs a format validator or reaches the compile's regexp engine.
// The findings are the walk's own, so the rows hold unchanged on both
// routes, which the walk walks since classes GC-01 and GC-02.
func TestFormatAssertionRefused(t *testing.T) {
	for _, tc := range assertedFormatCases() {
		t.Run(tc.name, func(t *testing.T) { refusedEverywhere(t, tc.schema, tc.want) })
	}
}

// TestVocabularySubsetRefused covers audit class GC-21 (row CC-04, the
// vocabulary part; 07 req 75): a dialect without the validation or
// applicator vocabulary, from the bundled meta/format-assertion or from a
// root $vocabulary of core alone, would make the library drop type and
// properties at a position the walk did not walk before classes GC-01 and
// GC-02, so the body "s" or {"a":1} passed a schema that forbids it; each
// shape is RZ-CFG-005 at the $schema that gives the dialect through
// CompileSchema, Check and Factory.Build.
func TestVocabularySubsetRefused(t *testing.T) {
	for _, tc := range vocabularySubsetCases() {
		t.Run(tc.name, func(t *testing.T) { refusedEverywhere(t, tc.schema, tc.want) })
	}
}

// dynamicOnly places anchor, a $dynamicAnchor "n" schema of the default
// dialect, in a resource on route that the dynamic scope alone reaches:
// the walked dynamicTarget resolves its $dynamicRef to it.
func dynamicOnly(route, anchor string) (doc, at string) {
	doc, at = onRoute(route, "", dynamicTarget, `{"$id":"urn:w","$defs":{"d":`+anchor+`},"$ref":"urn:y"}`)
	return doc, at + "/$defs/d"
}

// TestDynamicScopeInspected covers the class path the check finding names
// beyond format (audit class GC-21, rows CC-01 and CC-03, with the
// compiled-graph backstop GC-01 and GC-02 share): a $dynamicAnchor schema
// of a resource on an escaped route, which the library compiles with the
// resource and validation reaches through the dynamic scope alone, is
// inspected and analyzed as every compiled schema is (visitCompiled). Each
// row failed before: a reference there to the bundled meta-schema
// compiled, which the library serves without the loader; a fan-out of
// 1,056 compiled; a doubling through two $dynamicRef in an allOf compiled
// with no nesting limit; and a minimum there left ComparesNumbers false
// and the states of /a without it, so the body {"a":1e1000001} reached
// math/big, whose nil result the library dereferenced (a panic in Handle).
// On both routes, which the walk walks (classes GC-01 and GC-02), the
// walk walks the anchor schema live, as the library compiles it with its
// resource, and refuses the reference with its own message before the
// library compiles, and the fan-out, the doubling and the numbers hold as
// for any walked schema (before class GC-02, on the base-mismatch route
// the inspection refused the reference and the fan-out, and the
// walked-position backstop the rest).
func TestDynamicScopeInspected(t *testing.T) {
	branches := strings.TrimSuffix(strings.Repeat(`{"oneOf":[`+strings.TrimSuffix(strings.Repeat(`{"type":"string"},`, 32), ",")+`]},`, 33), ",")
	const meta = "https://json-schema.org/draft/2020-12/schema"
	for _, route := range escapedRoutes() {
		t.Run(route+"/outside reference", func(t *testing.T) {
			doc, at := dynamicOnly(route, `{"$dynamicAnchor":"n","$ref":"`+meta+`"}`)
			refusedEverywhere(t, doc, []Issue{{at + "/$ref", `reference "` + meta + `" resolves outside the schema document`}})
		})
		t.Run(route+"/fan-out", func(t *testing.T) {
			doc, at := dynamicOnly(route, `{"$dynamicAnchor":"n","anyOf":[`+branches+`]}`)
			refusedEverywhere(t, doc, []Issue{{at + "/anyOf", fmt.Sprintf("nested applicators fan out to more than %d branches (the product of allOf, anyOf, oneOf and if/then/else branch counts along one schema path)", MaxFanOut)}})
		})
		t.Run(route+"/doubling", func(t *testing.T) {
			doc, _ := dynamicOnly(route, `{"$dynamicAnchor":"n","properties":{"a":{"allOf":[{"$dynamicRef":"#n"},{"$dynamicRef":"#n"}]}}}`)
			issues := compileIssues(t, doc)
			if len(issues) != 1 || issues[0].Pointer != "/$defs/y/properties" || !strings.HasPrefix(issues[0].Message, "validating one value of a body nested ") {
				t.Fatalf("compile = %+v, want the nesting limit refused at /$defs/y/properties", issues)
			}
		})
		t.Run(route+"/numbers", func(t *testing.T) {
			doc, _ := dynamicOnly(route, `{"$dynamicAnchor":"n","minimum":0}`)
			f := buildFilter(t, doc, testLimit)
			if !f.schema.ComparesNumbers() {
				t.Fatal("ComparesNumbers() = false, want true: the anchor schema compares numbers")
			}
			if detail := detailOf(t, handle(t, f, `{"a":1e1000001}`)); detail != DetailNumber {
				t.Errorf("{\"a\":1e1000001}: detail %q, want %q", detail, DetailNumber)
			}
			if r := handle(t, f, `{"a":-1}`); r.Outcome != filter.Respond {
				t.Errorf("{\"a\":-1} = %+v, want the minimum enforced through the dynamic scope", r)
			}
			if r := handle(t, f, `{"a":1}`); r.Outcome != filter.Continue {
				t.Errorf("{\"a\":1} = %+v, want Continue", r)
			}
		})
	}
}

// TestRegexpEngine checks the library's regexp engine of one compile: an
// expression compiles once, its compile shared, and a failure is a nil
// Regexp with its error every time (07 req 75 and F6: each distinct
// pattern compiles once per document, review finding B2, architecture
// R-77). Once frozen (audit class GC-21, rows CC-01 and CC-02), every call
// fails with errEngineFrozen, for an expression compiled before too,
// compiling and caching nothing, and calls from 8 goroutines at once write
// nothing but the atomic count: the engine before the fix wrote its map
// from every request, and concurrent requests ended the process with a
// concurrent map write.
func TestRegexpEngine(t *testing.T) {
	eng := newRegexpEngine()
	a, errA := eng.compile(`^a+$`)
	b, errB := eng.compile(`^a+$`)
	if errA != nil || errB != nil || a != b || len(eng.cache) != 1 {
		t.Errorf("compile twice = %v, %v, %v, %v, want one shared regexp", a, errA, b, errB)
	}
	for range 2 {
		if re, err := eng.compile(`(`); re != nil || err == nil || errors.Is(err, errEngineFrozen) {
			t.Errorf("compile(%q) = %v, %v, want a nil Regexp and the parse error", `(`, re, err)
		}
	}
	if len(eng.cache) != 2 || eng.calls.Load() != 4 || eng.frozen {
		t.Fatalf("before the freeze: %d cached, %d calls, frozen %v, want 2, 4, false", len(eng.cache), eng.calls.Load(), eng.frozen)
	}
	eng.freeze()
	for _, expr := range []string{`^a+$`, `^b$`, `(`} {
		if re, err := eng.compile(expr); re != nil || !errors.Is(err, errEngineFrozen) {
			t.Errorf("frozen compile(%q) = %v, %v, want errEngineFrozen", expr, re, err)
		}
	}
	const goroutines, each = 8, 500
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range each {
				if _, err := eng.compile(fmt.Sprintf("g%d-%d", g, i)); !errors.Is(err, errEngineFrozen) {
					t.Errorf("concurrent frozen compile = %v, want errEngineFrozen", err)
					return
				}
			}
		})
	}
	wg.Wait()
	if eng.cache != nil || !eng.frozen || eng.calls.Load() != 4+3+goroutines*each {
		t.Errorf("after the freeze: cache %v, frozen %v, %d calls, want nil, true, %d", eng.cache, eng.frozen, eng.calls.Load(), 4+3+goroutines*each)
	}
}

// TestRegexpEngineFrozen covers audit class GC-21 (rows CC-02, CC-03 and
// CR-24's note): whatever the compile's outcome, the engine it gave the
// library is frozen when it returns, its cache dropped, so a later call
// compiles and caches nothing, and the compile used that engine (a
// pattern, a patternProperties name and the meta-schema's format 'regex'
// call it).
func TestRegexpEngineFrozen(t *testing.T) {
	errStop := errors.New("stop")
	stopAt := func(n int) func() error {
		return func() error {
			if n--; n < 0 {
				return errStop
			}
			return nil
		}
	}
	// The base-mismatch route, which the walk walks since class GC-02, so
	// its $schema rule refuses the asserting dialect there before the
	// library compiles or calls the engine (before the fix the library
	// compiled, and called the engine, before the compile's backstop
	// refused it).
	escapedRegex, _ := onRoute(mismatchRoute, assertingVocabulary, "", `{"$id":"urn:w","$schema":"`+baseURL+`","format":"regex","pattern":"^a"}`)
	fanned := `{"pattern":"^a","anyOf":[` + strings.TrimSuffix(strings.Repeat(`{"oneOf":[`+strings.TrimSuffix(strings.Repeat(`{"minLength":1},`, 32), ",")+`]},`, 33), ",") + `]}`
	for _, tc := range []struct {
		name, schema string
		stop         func() error
		// compiled: the compile returns a Schema; calls: it called the
		// engine; err: it returns errStop.
		compiled, calls, err bool
	}{
		{"accepted", `{"pattern":"^a","properties":{"b":{"patternProperties":{"^c":true}}}}`, nil, true, true, false},
		{"refused by the walk", `{"pattern":"(?=a)"}`, nil, false, false, false},
		{"refused by the library", `{"$ref":"#/x","x":{"type":5},"pattern":"^a"}`, nil, false, true, false},
		{"refused when checked apart", `{"$ref":"#/additionalItems","additionalItems":{"pattern":"^a","multipleOf":0}}`, nil, false, true, false},
		{"refused at an escaped route's $schema", escapedRegex, nil, false, false, false},
		{"refused by the inspector", fanned, nil, false, true, false},
		{"refused by the analysis", `{"patternProperties":{"^a":{"$ref":"#"},"b$":{"$ref":"#"}}}`, nil, false, true, false},
		{"stopped before the library compiles", `{"pattern":"^a"}`, stopAt(0), false, false, true},
		{"stopped before the analysis", `{"pattern":"^a"}`, stopAt(1), false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, issues := prepare([]byte(tc.schema))
			if len(issues) > 0 {
				t.Fatal(issues)
			}
			eng := newRegexpEngine()
			s, issues, err := compileWith(p, refusingLoader{}, tc.stop, eng)
			if (s != nil) != tc.compiled || (len(issues) > 0) == (tc.compiled || tc.err) || errors.Is(err, errStop) != tc.err {
				t.Fatalf("compile compiled %v with %+v, %v", s != nil, issues, err)
			}
			n := eng.calls.Load()
			if (n > 0) != tc.calls {
				t.Errorf("compile called the engine %d times, want calls %v", n, tc.calls)
			}
			if !eng.frozen || eng.cache != nil {
				t.Fatalf("after the compile: frozen %v, cache %v, want frozen and no cache", eng.frozen, eng.cache)
			}
			if re, err := eng.compile(`^a`); re != nil || !errors.Is(err, errEngineFrozen) || eng.cache != nil || eng.calls.Load() != n+1 {
				t.Errorf("a later call = %v, %v, cache %v, want errEngineFrozen and nothing cached", re, err, eng.cache)
			}
		})
	}
}

// engineFilter compiles schema with a regexp engine the test keeps and
// builds its Filter under testLimit.
func engineFilter(t *testing.T, schema string) (*Filter, *regexpEngine) {
	t.Helper()
	p, issues := prepare([]byte(schema))
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	eng := newRegexpEngine()
	s, issues, err := compileWith(p, refusingLoader{}, nil, eng)
	if s == nil {
		t.Fatalf("compile(%.80s) = %+v, %v", schema, issues, err)
	}
	return newFilter(s, errcode.Status(CodeRejected), limitsOf(testLimit)), eng
}

// Handle runs from concurrentGoroutines goroutines, concurrentBodies
// bodies each: 2,000 distinct bodies, the count of library audit CC-02.
const concurrentGoroutines, concurrentBodies = 8, 250

// handleConcurrently runs body(g, i) through f from concurrentGoroutines
// goroutines, concurrentBodies bodies each, and reports every result whose
// outcome or detail is not what want returns.
func handleConcurrently(t *testing.T, f *Filter, body func(g, i int) string, want func(g, i int) (filter.Outcome, string)) {
	t.Helper()
	errs := make(chan string, 16)
	var wg sync.WaitGroup
	for g := range concurrentGoroutines {
		wg.Go(func() {
			for i := range concurrentBodies {
				b := body(g, i)
				x, _ := exchange("application/json", []byte(b))
				r := f.Handle(context.Background(), phase.OnRequestBody, x)
				outcome, detail := want(g, i)
				if r.Outcome != outcome || (detail != "" && (r.Response == nil || r.Response.Detail != detail)) {
					select {
					case errs <- fmt.Sprintf("body %s: %+v", b, r):
					default:
					}
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestHandleNeverCallsEngine covers audit class GC-21 (rows VAL-68 and
// CC-01; 07 req 91): validation never calls the regexp engine of the
// compile. Handle runs from 8 goroutines on a Filter whose schema has
// patterns and patternProperties names at the root, in properties, items,
// a reference target, an allOf branch and propertyNames, and the engine's
// call count does not move once the compile has returned (run under -race
// in CI, beside TestHandleConcurrent).
func TestHandleNeverCallsEngine(t *testing.T) {
	const schema = `{"type":"object","propertyNames":{"pattern":"^[a-z0-9-]+$"},` +
		`"properties":{"name":{"type":"string","pattern":"^[a-z]+$"},"tags":{"type":"array","items":{"type":"string","pattern":"^#"}},"ref":{"$ref":"#/$defs/code"}},` +
		`"patternProperties":{"^x-":{"type":"string","pattern":"v$"}},"allOf":[{"patternProperties":{"^n[0-9]+$":{"type":"string","minLength":1}}}],` +
		`"$defs":{"code":{"type":"string","pattern":"^[A-Z]{3}$"}}}`
	f, eng := engineFilter(t, schema)
	n := eng.calls.Load()
	if n == 0 {
		t.Fatal("the compile never called the engine")
	}
	bodies := []struct {
		body    string
		outcome filter.Outcome
	}{
		{`{"name":"abc","tags":["#a%d"],"ref":"ABC","x-a":"%dv","n%d":"z"}`, filter.Continue},
		{`{"name":"Abc%d"}`, filter.Respond},
		{`{"tags":["a%d"]}`, filter.Respond},
		{`{"ref":"abc%d"}`, filter.Respond},
		{`{"x-a":"w%d"}`, filter.Respond},
		{`{"Bad%d":1}`, filter.Respond},
		{`{"n%d":""}`, filter.Respond},
	}
	handleConcurrently(t, f, func(g, i int) string {
		b := bodies[(g+i)%len(bodies)].body
		k := strings.Count(b, "%d")
		args := make([]any, k)
		for j := range args {
			args[j] = g*1000 + i
		}
		return fmt.Sprintf(b, args...)
	}, func(g, i int) (filter.Outcome, string) { return bodies[(g+i)%len(bodies)].outcome, "" })
	if got := eng.calls.Load(); got != n {
		t.Errorf("validation called the engine %d times, want none", got-n)
	}
	if eng.cache != nil {
		t.Errorf("the engine keeps %d compiles after the compile", len(eng.cache))
	}
}

// TestHandleFrozenEngine covers audit class GC-21 (rows CC-01, CC-02,
// CC-03 and VAL-68): were an asserted format 'regex' to escape the
// compile's $schema rule and its inspection, the library would call the
// compile's regexp engine on every body string evaluated there
// (validator.go:136-142). The test injects the library's own binding of
// format 'regex' to the engine (objcompiler.go:188-192, which the compile
// refuses) and runs Handle from 8 goroutines with 2,000 distinct strings,
// then once with a 180 KB string whose compile as a regexp library audit
// CC-03 measured at 125 ms and 76 MB: the frozen engine fails every call,
// so the body is refused at the format keyword (fail closed), nothing is
// compiled or cached, and under -race no write races; before the fix the
// engine wrote its map from every request, which ended the process with a
// concurrent map write and kept every body string compiled.
func TestHandleFrozenEngine(t *testing.T) {
	f, eng := engineFilter(t, `{"type":"object","properties":{"a":{"type":"string"}}}`)
	bind := sjs.NewCompiler()
	bind.AssertFormat()
	bind.UseRegexpEngine(eng.compile)
	if err := bind.AddResource("urn:bind", map[string]any{"format": "regex"}); err != nil {
		t.Fatal(err)
	}
	bound, err := bind.Compile("urn:bind")
	if err != nil || bound.Format == nil {
		t.Fatalf("binding = %v, %v", bound, err)
	}
	f.schema.root.Properties["a"].Format = bound.Format
	n := eng.calls.Load()
	handleConcurrently(t, f, func(g, i int) string { return fmt.Sprintf(`{"a":"g%d-%d"}`, g, i) },
		func(int, int) (filter.Outcome, string) {
			return filter.Respond, DetailMismatch + " at /properties/a/format"
		})
	if got := eng.calls.Load() - n; got < concurrentGoroutines*concurrentBodies {
		t.Errorf("the injected format called the engine %d times, want one call at least per body", got)
	}
	big := `{"a":"` + strings.Repeat(`(a|b)*`, 30000) + `"}`
	if detail := detailOf(t, handle(t, f, big)); detail != DetailMismatch+" at /properties/a/format" {
		t.Errorf("180 KB string: detail %q", detail)
	}
	if eng.cache != nil {
		t.Errorf("the frozen engine cached %d request strings", len(eng.cache))
	}
}
