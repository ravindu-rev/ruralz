// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// corpusSchemas reads testdata/schemas, sorted by name.
func corpusSchemas(t testing.TB) [][]byte {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "schemas", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("schema corpus: %v, %d files", err, len(files))
	}
	out := make([][]byte, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: a corpus file under testdata.
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

// bodySeeds are request bodies shared by the body fuzz targets.
func bodySeeds() []string {
	return []string{
		`{"id":"ord_0123abcd","customer":{"email":"a@b"},"items":[{"sku":"x","qty":2,"price":1.5}],"total":3.00}`,
		`{"id":"ord_x","items":[]}`,
		`["a",1,true,null,2.5]`,
		`["a",1,false]`,
		`{"kind":"x","x":1}`,
		`{"n_a":1,"s_b":"x","card":1,"cvv":2,"ok":true}`,
		`{"value":1,"children":[{"value":2,"children":[]}],"meta":{"k":"v"}}`,
		`"äbc"`, `-100`, `99.5`, `null`, `[1,2]`, `{"k":false}`,
		`12345678901234567890`, `1e400`, `1e1001`, `{"a":1,"a":2}`, `{} {}`, "\xff", ``,
	}
}

// FuzzCompileSchema is 07 F6: compiling any document never asks the loader (no
// file or network access is even attempted) and never panics, not even into the
// recovered finding of compileWith (msgPanicked); every finding's pointer maps
// to a path. The walk keeps the first two on every route, since it walks live
// exactly what the library compiles, through each of its reference keywords
// (audit class GC-01), with its base URIs and in its order (class GC-02, see
// "Compiled set" in doc.go), so every position the library compiled was walked
// live and the walked-position backstop never fires (no msgUnwalked or
// msgUnlisted finding); and two walks of a document give the same model and
// findings, whatever the order of its members. That two compiles give the same
// findings also where the library's own error is the finding is a property of
// class GC-19, whose fix adds it here: an unparseable $id is still reported in
// the library's map order (REFS-14). F6's other property, a compile returning
// within 1 s, is not a wall-clock assertion here: the budgets that bound a
// compile are pinned by deterministic tests (TestQueueBoundBudget,
// TestClassRepsBudget, TestCompileMatchBudget, TestStatesBudget,
// TestEvaluationBoundBudget, and for the walk's fixpoint
// TestFollowFixpointCost), and so are the bounds of the pattern programs and of
// their parsing (TestPatternProgramLimits, TestPatternParsesBounded) and of the
// library's meta-schema checks (TestReferenceLadder, TestMetaChecksBound, and
// for the lists of names it compares TestNameListsRefusedByWalk), and the
// states' work per schema (TestStatesReach, TestStatesLengthLoops); its time is
// measured by BenchmarkCompileAnalysisWorstCase. A schema the meta-schema
// refuses is refused wherever the library compiles it
// (TestAdditionalItemsChecked), and a compiled schema at a position the walk
// did not walk live, which none of the walk's rules covered, is refused
// (TestUnwalkedBackstop), so validating a compiled one never panics. Whatever
// the outcome, the compile's regexp engine is frozen when it returns, and
// validating against a compiled schema never calls it: only an asserted format
// 'regex' would, and the compile refuses the $schema that would assert one
// wherever the library reads it, and an asserted format at every schema the
// library compiled (audit class GC-21).
func FuzzCompileSchema(f *testing.F) {
	for _, s := range corpusSchemas(f) {
		f.Add(s)
	}
	for _, s := range []string{
		`{"$ref":"file:///etc/passwd"}`, `{"$ref":"https://example.com/x"}`, `{"$ref":"#/x","x":{"$ref":"y.json"}}`,
		`{"$schema":"http://json-schema.org/draft-07/schema#"}`, `{"pattern":"(?=a)"}`, nestedAnyOf(11),
		`{"$defs":{"a":{"$id":"urn:a","$anchor":"x"}},"$ref":"urn:a#x"}`, `{"$dynamicRef":"#n","$dynamicAnchor":"n"}`,
		`{"$ref":"#"}`, `{"allOf":[{"$ref":"#"}]}`, `{"minimum":1e-999999}`, `{"$id":"%%"}`, `{"$ref":"#/%zz"}`,
		oneOfTwice, linearTree, mixins, `{"dependencies":{"a":["b"],"c":{"$ref":"#"}}}`,
		`{"pattern":"(?i)[\\x{42}-\\x{1e942}][\\pL[:alpha:]\\d]\\Q[\\E"}`, `{"patternProperties":{"[a-z]{1000}[a-z]{1000}":true,"\\p{Greek}+":false}}`,
		`{"pattern":"(?i:a)((?-i)[\\x{42}-\\x{1e942}])(?P<n>[\\pL])^\\pL{110}"}`, `{"pattern":"[\\pL\\pN\\p{Greek}]{3}(?is-mU)[a-z]"}`,
		`{"$defs":{"a":{"$ref":"#/x/if/if"},"b":{"$ref":"#/x/if"},"c":{"$ref":"#/x"}},"x":{"if":{"if":{"pattern":"^a"}}}}`,
		`{"$ref":"#/x/properties","$defs":{"a":{"$ref":"#/x/properties/items/properties"}},"x":{"properties":{"items":{"properties":{"items":true}}}}}`,
		`{"$ref":"#/x~1y/a%25b","x/y":{"a%b":{"type":"string"}}}`,
		// Lists of names the meta-schema compares (review finding: the
		// library parsed their numbers with math/big, for seconds or until
		// it panicked).
		`{"type":[` + literals(21, "e9999999") + `]}`, `{"required":["a",` + literals(20, "e999999") + `]}`,
		`{"dependentRequired":{"a":[1,{"b":2},[3],null,true]}}`, `{"dependencies":{"a":[[1e9999999]],"b":{"type":[1]}}}`,
		`{"$ref":"#/x/properties","x":{"properties":{"type":[1e999999,2e999999],"required":[{}]}}}`,
		// Below additionalItems, which no meta-schema check of the library
		// covers (review finding: a multipleOf of 0 compiled, and the
		// library panicked at every number reaching it).
		`{"additionalItems":{"multipleOf":0}}`, `{"$ref":"#/additionalItems","additionalItems":{"additionalItems":{"multipleOf":0}}}`,
	} {
		f.Add([]byte(s))
	}
	for _, tc := range additionalItemsCases() {
		f.Add([]byte(tc.schema))
	}
	// Dialects a $schema gives on a route the walk did not walk before
	// classes GC-01 and GC-02, with format asserted (library audit CC-01) or
	// validation and applicator keywords dropped (CC-04), and $dynamicAnchor
	// schemas only the dynamic scope reaches; the first is the check
	// finding's seed as it was written.
	f.Add([]byte(`{` + assertingVocabulary + `,"$recursiveRef":"#/x","x":{"$id":"urn:x","$schema":"` + baseURL + `",` +
		`"$defs":{"d":{"$dynamicAnchor":"n","format":"regex"}},"$ref":"urn:y"},"$defs":{"y":{"$id":"urn:y","$dynamicAnchor":"n","items":{"$dynamicRef":"#n"}}}}`))
	for _, tc := range append(assertedFormatCases(), vocabularySubsetCases()...) {
		f.Add([]byte(tc.schema))
	}
	// $recursiveRef, a reference keyword of the library under draft 2020-12
	// whose targets the walk did not follow (audit class GC-01): every
	// shape of its tests, but the counts, whose 120 KB slow every
	// execution, for which the smallest document past the bound stands
	// in; and the input this target found before the fix.
	for _, tc := range recursiveTwinCases() {
		if !strings.HasPrefix(tc.name, "count/") {
			f.Add([]byte(recursiveOf(tc.shape)))
		}
	}
	f.Add([]byte(`{"$recursiveRef":"#/x","x":{"allOf":[` + joinRepeat(`{}`, MaxSubschemas) + `]}}`))
	for _, tc := range recursiveAccepted() {
		f.Add([]byte(recursiveOf(tc.shape)))
	}
	f.Add([]byte(`{"$recursiveRef":"0"}`))
	// Every shape of class GC-02's tests (follow_test.go) but the few
	// whose size slows every execution, and the input this target found
	// before the fix (G02 attempt 3).
	for _, set := range [][]followCase{baseCases(), lateIDCases(), ancestorCases(), policyDialectCases(), livenessCases(), olderDraftCases(), visibilityCases()} {
		for _, tc := range set {
			if len(tc.schema) <= 16<<10 {
				f.Add([]byte(tc.schema))
			}
		}
	}
	f.Add([]byte(generations(3)))
	// The fixpoint's passes under a long member name, which hashed and
	// built its pointers again in every pass (check finding on the
	// fixpoint, passes times pointer bytes).
	f.Add([]byte(deepGenerations(3, 1000)))
	f.Add([]byte(`{"$ref":"#/x/y","x":{"$id":"https://x.test/","y":{"$ref":"#/x"},"x":{"type":"string"}}}`))
	f.Add([]byte(`{"$defs":{"r":{"$id":"0","x":{"$ref":"#/w"},"w":{"$ref":"00"}}},"w":true,"$ref":"#/$defs/r/x"}`))
	// An ordinary two-pass document of long-named targets outside the
	// keywords, whose placing among the targets found refused it (check
	// finding on the fixpoint's budget term), at a size every execution
	// affords.
	f.Add([]byte(flatTargets(20, 200)))
	f.Add([]byte(`{"$recursiveAnchor":true,"items":{"$recursiveRef":"#"}}`))
	for _, route := range escapedRoutes() {
		for _, anchor := range []string{
			`{"$dynamicAnchor":"n","minimum":0}`, `{"$dynamicAnchor":"n","$ref":"https://json-schema.org/draft/2020-12/schema"}`,
			`{"$dynamicAnchor":"n","properties":{"a":{"allOf":[{"$dynamicRef":"#n"},{"$dynamicRef":"#n"}]}}}`,
		} {
			doc, _ := dynamicOnly(route, anchor)
			f.Add([]byte(doc))
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		spy := &spyLoader{}
		eng := newRegexpEngine()
		s, doc, issues := compileRawWith(raw, spy, eng)
		if len(spy.urls) > 0 {
			t.Fatalf("compile asked the loader for %q", spy.urls)
		}
		// The walk refuses what would make the compile panic before the
		// library runs, and walks live every position the library compiles,
		// so the backstops never fire.
		if slices.ContainsFunc(issues, func(is Issue) bool {
			return is.Message == msgPanicked || is.Message == msgUnwalked || is.Message == msgUnlisted
		}) {
			t.Fatalf("a backstop of the compile fired: %+v", issues)
		}
		if doc != nil {
			a, b := newWalker(doc), newWalker(doc)
			a.walk()
			b.walk()
			if !slices.Equal(a.positions, b.positions) || !slices.Equal(a.checkOrder, b.checkOrder) || !slices.Equal(a.issues, b.issues) || !maps.Equal(a.seen, b.seen) {
				t.Fatalf("two walks differ: %+v and %+v", a.issues, b.issues)
			}
		}
		if doc != nil && (!eng.frozen || eng.cache != nil) {
			t.Fatalf("after the compile the regexp engine is frozen %v with cache %v", eng.frozen, eng.cache)
		}
		calls := eng.calls.Load()
		if (s == nil) == (len(issues) == 0) || len(issues) > MaxIssues {
			t.Fatalf("compile = %v, %d issues", s, len(issues))
		}
		for _, is := range issues {
			if is.Message == "" || strings.ContainsAny(is.Message, "\r\n") {
				t.Errorf("issue %+v", is)
			}
			_ = schemaPath(configSchemaPath(), doc, is.Pointer)
		}
		if s != nil {
			for _, b := range []string{`{}`, `[1,"a"]`, `"x"`, `5`, `{"price":19.99}`} {
				_, _ = s.Validate(decodeInstance(t, b))
			}
			if n := eng.calls.Load() - calls; n > 0 {
				t.Fatalf("validation called the regexp engine %d times", n)
			}
		}
	})
}

// FuzzCompilePattern checks the library's regexp engine against Go's
// regexp (07 req 75, architecture R-77): for any expression whose parsing
// work and program the walk measures within MaxPatternInstructions, and
// any input up to 1 KiB, compilePattern fails exactly when regexp.Compile
// does, with the same error, and otherwise names the expression and
// matches as Go's regexp does; the empty group that keeps Go from building
// its one-pass matcher changes no match.
func FuzzCompilePattern(f *testing.F) {
	for _, tc := range costPatterns() {
		if len(tc.pattern) <= 256 {
			// A long seed slows the minimization of what grows from it.
			f.Add(tc.pattern, tc.input)
		}
	}
	for _, expr := range []string{`{2}a`, `*`, `^a|^b`, `|^a`, `^(?:a|bb)$`, `(?i)^k$`, `\Aa\z`, `(?U)^a+?`, `^\pL{1,3}$`, `{a`, `^[^a]$`} {
		f.Add(expr, "a")
	}
	f.Fuzz(func(t *testing.T, expr, input string) {
		if len(input) > 1<<10 || newWalker(nil).scanPattern(expr).parseWork() > MaxPatternInstructions {
			return
		}
		if re, err := syntax.Parse(expr, syntax.Perl); err == nil && programSize(re) > MaxPatternInstructions {
			return
		}
		want, wantErr := regexp.Compile(expr)
		got, err := compilePattern(expr)
		if (err != nil) != (wantErr != nil) || err != nil && err.Error() != wantErr.Error() {
			t.Fatalf("compilePattern(%q) error = %v, want %v", expr, err, wantErr)
		}
		if err != nil {
			return
		}
		if got.String() != expr || got.MatchString(input) != want.MatchString(input) {
			t.Fatalf("compilePattern(%q): String %q, MatchString(%q) = %v, want %v", expr, got.String(), input, got.MatchString(input), want.MatchString(input))
		}
	})
}

// checkResult applies the 07 req 18 and T14 properties to one Handle
// result: a refusal is 400 RZ-RT-009 whose detail is a fixed string or a
// keyword location present in the schema document, so it never carries
// request content; only the SPI sentinels are cannot decide.
func checkResult(t *testing.T, doc any, r filter.Result) {
	t.Helper()
	switch r.Outcome {
	case filter.Continue:
	case filter.Respond:
		resp := r.Response
		if resp.Status != 400 || resp.Code != CodeRejected || resp.Body != nil || resp.Header != nil {
			t.Fatalf("response %+v", resp)
		}
		switch resp.Detail {
		case DetailContentType, DetailEmpty, DetailInvalid, DetailNumber, DetailMismatch:
		default:
			ptr, ok := strings.CutPrefix(resp.Detail, DetailMismatch+" at ")
			if !ok {
				t.Fatalf("detail %q", resp.Detail)
			}
			if _, found := lookup(doc, ptr); !found {
				t.Fatalf("detail %q names no location of the schema", resp.Detail)
			}
		}
	case filter.CannotDecide:
		if !errors.Is(r.Err, filter.ErrTooLarge) && !errors.Is(r.Err, filter.ErrBudget) {
			t.Fatalf("cannot decide: %v", r.Err)
		}
	default:
		t.Fatalf("outcome %d", r.Outcome)
	}
}

// FuzzValidate is 07 F7: corpus schemas against arbitrary bodies never
// panic, leave the body untouched, release every reservation, and answer
// within the properties of checkResult.
func FuzzValidate(f *testing.F) {
	schemas := corpusSchemas(f)
	filters := make([]*Filter, len(schemas))
	docs := make([]any, len(schemas))
	for i, s := range schemas {
		filters[i] = buildFilter(f, string(s), 4096)
		docs[i], _, _ = jsonval.Decode(s, jsonval.Options{MapObjects: true})
	}
	for i := range schemas {
		for _, b := range bodySeeds() {
			f.Add(uint8(i), []byte(b))
		}
	}
	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		i := int(which) % len(filters)
		in := bytes.Clone(body)
		x, m := exchange("application/json", in)
		r := filters[i].Handle(t.Context(), phase.OnRequestBody, x)
		checkResult(t, docs[i], r)
		if !bytes.Equal(m.Raw, body) {
			t.Fatal("Handle changed the body")
		}
		if x.Budget.Reserved != 0 {
			t.Fatalf("reservation left: %d", x.Budget.Reserved)
		}
	})
}

// FuzzDifferential is 07 P7: for bodies the strict decoder accepts (one
// value, no duplicate names, valid UTF-8), the Filter accepts exactly when
// the library's own decoder and Schema.Validate accept, over the corpus.
func FuzzDifferential(f *testing.F) {
	schemas := corpusSchemas(f)
	type pair struct {
		filter *Filter
		lib    *sjs.Schema
	}
	pairs := make([]pair, len(schemas))
	for i, s := range schemas {
		doc, err := sjs.UnmarshalJSON(bytes.NewReader(s))
		if err != nil {
			f.Fatal(err)
		}
		c := sjs.NewCompiler()
		c.DefaultDraft(sjs.Draft2020)
		c.UseLoader(refusingLoader{})
		if err := c.AddResource("https://example.com/s.json", doc); err != nil {
			f.Fatal(err)
		}
		lib, err := c.Compile("https://example.com/s.json")
		if err != nil {
			f.Fatal(err)
		}
		pairs[i] = pair{buildFilter(f, string(s), 0), lib}
	}
	for i := range schemas {
		for _, b := range bodySeeds() {
			f.Add(uint8(i), []byte(b))
		}
	}
	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		p := pairs[int(which)%len(pairs)]
		v, _, err := jsonval.Decode(body, jsonval.Options{MapObjects: true})
		if err != nil || !numbersInRange(v) {
			// Out-of-range numbers are refused before validation; the
			// library itself panics on exponents past what math/big parses.
			return
		}
		inst, err := sjs.UnmarshalJSON(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("strict decoder accepted what encoding/json refuses: %v", err)
		}
		want := p.lib.Validate(inst) == nil
		r := handle(t, p.filter, string(body))
		if got := r.Outcome == filter.Continue; got != want {
			t.Fatalf("Filter accepts = %v, library = %v (%+v)", got, want, r.Response)
		}
	})
}
