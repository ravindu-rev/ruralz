// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"runtime"
	"slices"
	"strings"
	"testing"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// The findings of the walked-position backstop and of a recovered panic,
// written out so a change of their wording fails the tests.
const (
	msgUnwalkedText = "the library compiles this schema position on a route the compile's rule walk does not follow, so the walk's rules never applied to it (move the schema a reference names here under $defs)"
	msgPanickedText = "the compile of this schema failed with an internal error, so it is refused"
)

// refKeyword is the placeholder of a shape for its reference keyword:
// recursiveOf puts $recursiveRef in its place, refTwin $ref.
const refKeyword = `"REF"`

// recursiveOf returns shape with $recursiveRef as its reference keyword.
func recursiveOf(shape string) string {
	return strings.ReplaceAll(shape, refKeyword, `"$recursiveRef"`)
}

// refTwin returns shape with $ref as its reference keyword.
func refTwin(shape string) string { return strings.ReplaceAll(shape, refKeyword, `"$ref"`) }

// recursivePointer maps a pointer of a shape's $ref twin to the shape
// with $recursiveRef: every $ref token is the placeholder's.
func recursivePointer(ptr string) string {
	toks := strings.Split(ptr, "/")
	for i, tok := range toks {
		if tok == "$ref" {
			toks[i] = "$recursiveRef"
		}
	}
	return strings.Join(toks, "/")
}

// joinRepeat returns n copies of s joined by commas.
func joinRepeat(s string, n int) string {
	return strings.TrimSuffix(strings.Repeat(s+",", n), ",")
}

// refusedThrough checks that schema is RZ-CFG-005 with exactly want
// through the compile, CompileSchema, Check and Factory.Build, each
// without a panic, and that the compile asked the loader for nothing.
func refusedThrough(t *testing.T, schema string, want []Issue) {
	t.Helper()
	if len(want) == 0 {
		t.Fatal("refusedThrough needs a finding")
	}
	noPanic(t, "compile", func() {
		spy := &spyLoader{}
		if s, _, issues := compileRaw([]byte(schema), spy); s != nil || !slices.Equal(issues, want) {
			t.Errorf("compile(%.120s) compiled %v with %+v, want %+v", schema, s != nil, issues, want)
		}
		if len(spy.urls) > 0 {
			t.Errorf("compile(%.120s) asked the loader for %q", schema, spy.urls)
		}
	})
	noPanic(t, "CompileSchema", func() {
		_, err := CompileSchema([]byte(schema))
		var ce *CompileError
		if !errors.As(err, &ce) || !slices.Equal(ce.Issues, want) {
			t.Errorf("CompileSchema(%.120s) = %v, want %+v", schema, err, want)
		}
	})
	noPanic(t, "Check", func() {
		p, _ := prepare([]byte(schema))
		got := runCheck(policyResource("body", schema))
		if len(got) != len(want) {
			t.Errorf("Check(%.120s) = %d diagnostics, want %d", schema, len(got), len(want))
			return
		}
		for i, d := range got {
			if path := schemaPath(configSchemaPath(), p.doc, want[i].Pointer).String(); d.Code != CodeConfig || d.Message != want[i].Message || d.Path.String() != path {
				t.Errorf("Check(%.120s)[%d] = %s %q at %s, want %s %q at %s", schema, i, d.Code, d.Message, d.Path, CodeConfig, want[i].Message, path)
			}
		}
	})
	noPanic(t, "Factory.Build", func() {
		f, err := Factory{}.Build(t.Context(), filter.BuildEnv{Config: configOf(schema), Limits: filter.Limits{MaxRequestBodyBytes: testLimit}})
		if code, _ := errcode.CodeOf(err); f != nil || code != CodeConfig || !strings.Contains(err.Error(), (&CompileError{Issues: want}).Error()) {
			t.Errorf("Build(%.120s) = %v, %v, want %s with %+v", schema, f, err, CodeConfig, want[0])
		}
	})
}

// recursiveCase is a shape whose reference keyword is the placeholder.
type recursiveCase struct{ name, shape, ptr string }

// recursiveTwinCases are the shapes of library audit class GC-01 (round-9
// finding 1 and its six shapes): each is refused under $ref, and must be
// refused alike under $recursiveRef, which the library compiles under
// draft 2020-12 as a static reference (objcompiler.go:56-59, 269) and
// whose targets the walk did not follow, so every rule of the walk was
// bypassed. ptr is the first finding's pointer under $recursiveRef.
func recursiveTwinCases() []recursiveCase {
	huge := literals(21, "e9999999")
	p30, p400 := strings.Repeat("[a-z]{1000}", 30), strings.Repeat("[a-z]{1000}", 400)
	var out []recursiveCase
	add := func(name, shape, ptr string) { out = append(out, recursiveCase{name, shape, ptr}) }
	// Numbers (VLU-51, CMP-34, VLU-16, VAL-49): every keyword whose value
	// the library parses, at a literal math/big refuses (the meta-schema
	// check panicked), one just past the exponent bound and one past the
	// length bound.
	for _, kw := range parsedKeywords() {
		for _, lit := range []struct{ name, lit string }{
			{"1e9999999", "1e9999999"}, {"1e1001", "1e1001"}, {"129 bytes", strings.Repeat("1", MaxNumberBytes+1)},
		} {
			v, at := lit.lit, "/x/"+kw
			if kw == "enum" {
				v, at = "["+lit.lit+"]", at+"/0"
			}
			add("number/"+kw+"/"+lit.name, `{"REF":"#/x","x":{"`+kw+`":`+v+`}}`, at)
		}
	}
	// A zero multipleOf at a target under a member that is no keyword
	// (VAL-49's other target): the library's meta-schema check of the
	// target it compiles refused it before the fix too, so this row pins
	// that, as its twin below additionalItems pins the compile's check.
	add("meta-schema/zero multipleOf", `{"REF":"#/x","x":{"multipleOf":0}}`, "/x/multipleOf")
	// Twenty literals of 1e999999, which the library parsed for 421 ms and
	// 56 MB (VLU-52).
	add("number/twenty slow literals", `{"REF":"#/x","x":{"allOf":[`+joinRepeat(`{"minimum":1e999999}`, 20)+`]}}`, "/x/allOf/0/minimum")
	// The lists of names the meta-schema check compares (CMP-21, CMP-22,
	// CX-22), and the round-9 shape under properties (REFS-10).
	add("names/type", `{"REF":"#/x","x":{"type":[`+huge+`]}}`, "/x/type/0")
	add("names/required", `{"REF":"#/x","x":{"required":[`+huge+`]}}`, "/x/required/0")
	add("names/dependentRequired", `{"REF":"#/x","x":{"dependentRequired":{"a":[`+huge+`]}}}`, "/x/dependentRequired/a/0")
	add("names/dependencies", `{"REF":"#/x","x":{"dependencies":{"a":[`+huge+`]}}}`, "/x/dependencies/a/0")
	add("names/reference under properties", `{"properties":{"a":{"REF":"#/x"}},"x":{"type":[`+huge+`]}}`, "/x/type/0")
	// Entries whose hashes collide (writeHash writes no lengths, so [[],[]]
	// and [[[]]] hash alike), which the meta-check compared pairwise (CC-06,
	// its $recursiveRef route).
	add("names/colliding nested arrays", `{"REF":"#/x","x":{"required":[`+joinRepeat(`[[],[]],[[[]]]`, 4000)+`]}}`, "/x/required/0")
	// Patterns (CMP-23, CMP-24, CMP-36, CMP-51, CX-27): the measure, as a
	// pattern and as a patternProperties name, the round-9 seven patterns
	// that kept 118 MB, and RE2.
	add("pattern/30 repetitions", `{"REF":"#/x","x":{"pattern":"`+p30+`"}}`, "/x/pattern")
	add("pattern/patternProperties name", `{"REF":"#/x","x":{"patternProperties":{"`+p30+`":true}}}`, "/x/patternProperties/"+p30)
	add("pattern/seven of 400 repetitions", `{"REF":"#/x","x":{"allOf":[`+joinRepeat(`{"pattern":"`+p400+`"}`, 7)+`]}}`, "/x/allOf/0/pattern")
	add("pattern/look-ahead", `{"REF":"#/x","x":{"pattern":"(?=a)"}}`, "/x/pattern")
	// MaxSubschemas (REFS-11, CX-21): the round-9 allOf of 40,000 took 17 s.
	add("count/12,000 properties", `{"REF":"#/x","x":{"properties":{`+props(12000)+`}}}`, "")
	add("count/allOf of 40,000", `{"REF":"#/x","x":{"allOf":[`+joinRepeat(`{}`, 40000)+`]}}`, "")
	// Below additionalItems, which no check of the library covers (CMP-26,
	// CMP-34, REFS-13, VAL-49): the round-8 fix, bypassed.
	add("additionalItems/zero multipleOf", `{"REF":"#/additionalItems","additionalItems":{"multipleOf":0}}`, "/additionalItems/multipleOf")
	for _, tc := range additionalItemsCases() {
		if strings.Contains(tc.schema, `"$ref"`) {
			add("additionalItems/"+tc.name, strings.ReplaceAll(tc.schema, `"$ref"`, refKeyword), tc.ptr)
		}
	}
	// VAL-49's other value kinds at an additionalItems target, which the
	// walk's rules refuse once it follows the reference: a numeric bound
	// past the number bounds, const and enum numbers past them, which the
	// library re-parsed at every comparison, and an unmeasured pattern.
	// Before the fix each compiled and was accepted.
	add("number/below additionalItems/minimum", `{"REF":"#/additionalItems","additionalItems":{"minimum":1e99999}}`, "/additionalItems/minimum")
	add("number/below additionalItems/const", `{"REF":"#/additionalItems","additionalItems":{"const":1e99999}}`, "/additionalItems/const")
	add("number/below additionalItems/enum", `{"REF":"#/additionalItems","additionalItems":{"enum":[1e99999]}}`, "/additionalItems/enum/0")
	add("pattern/below additionalItems", `{"REF":"#/additionalItems","additionalItems":{"pattern":"`+p30+`"}}`, "/additionalItems/pattern")
	// The loader and the findings at the reference (CMP-31, CMP-35,
	// REFS-12, REFS-13, REFS-25, CX-03): remote, relative and bundled
	// meta-schema URLs, which reached the loader or the bundled
	// meta-schemas with the finding at the root, the values of the check
	// finding, and the library's raw or misplaced findings.
	for _, v := range []string{
		"https://example.com/x", "file:///etc/passwd", "https://json-schema.org/draft/2020-12/schema",
		"https://json-schema.org/draft/2019-09/schema", "http://json-schema.org/draft-07/schema#", "0", "x", "y.json",
		"#nope", "#/missing", "#/%zz", "http://[::1",
	} {
		add("reference/"+v, `{"REF":"`+v+`"}`, "/$recursiveRef")
	}
	add("reference/not a schema", `{"REF":"#/required","required":["a"]}`, "/$recursiveRef")
	add("reference/custom $schema in the target", `{"REF":"#/x","x":{"$id":"urn:y","$schema":"https://example.com/meta"}}`, "/x/$schema")
	// The library loads a $schema before it sees there is no $id to make
	// it count (CMP-31).
	add("reference/custom $schema without $id", `{"REF":"#/x","x":{"$schema":"https://example.com/meta"}}`, "/x/$schema")
	add("reference/draft-07 target", `{"REF":"#/x","x":{"$id":"urn:y","$schema":"http://json-schema.org/draft-07/schema#"}}`, "/x/$schema")
	add("reference/meta/core target", `{"REF":"#/x","x":{"$id":"urn:x","$schema":"https://json-schema.org/draft/2020-12/meta/core","$recursiveAnchor":true}}`, "/x/$schema")
	// Older dialects, whose meta-schemas compare enum entries for
	// uniqueItems with math/big (CC-05, its $recursiveRef route).
	enum := `"enum":[` + literals(20, "") + `,1e9999999]`
	add("reference/draft-04 target with an enum", `{"REF":"#/x","x":{"id":"urn:y","$schema":"http://json-schema.org/draft-04/schema#",`+enum+`}}`, "/x/$schema")
	add("reference/draft-07 target with an enum", `{"REF":"#/x","x":{"$id":"urn:y","$schema":"http://json-schema.org/draft-07/schema#",`+enum+`}}`, "/x/$schema")
	add("reference/remote below the target", `{"REF":"#/x","x":{"REF":"y.json"}}`, "/x/$recursiveRef")
	add("reference/cycle", `{"REF":"#"}`, "/$recursiveRef")
	// The evaluation bound (VAL-47, REFS-32): a recursion reached through
	// two paths per level of nesting doubles the library's work.
	add("evaluation/doubling", strings.ReplaceAll(oneOfTwice, `"$ref"`, refKeyword), "/oneOf")
	// A boolean $recursiveAnchor, which would make the library resolve the
	// reference dynamically (CX-04, VAL-48, REFS-15).
	add("recursiveAnchor/behind the reference", `{"REF":"#/x","x":{"$recursiveAnchor":true}}`, "/x/$recursiveAnchor")
	add("recursiveAnchor/below additionalItems", `{"REF":"#/additionalItems","additionalItems":{"$recursiveAnchor":true}}`, "/additionalItems/$recursiveAnchor")
	return out
}

// props returns n empty-schema members p0 to p<n-1>, joined by commas.
func props(n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(`"p%d":{}`, i)
	}
	return strings.Join(out, ",")
}

// TestRecursiveRefTwins covers library audit class GC-01 (round-9 finding
// 1; rows CMP-21, CMP-22, CMP-23, CMP-24, CMP-26, CMP-31, CMP-33, CMP-34,
// CMP-35, CMP-36, CMP-51, VAL-49, VLU-16, VLU-51, VLU-52, REFS-10,
// REFS-11, REFS-12, REFS-13, REFS-15, REFS-25, CX-02, CX-03, CX-04,
// CX-21, CX-22, CX-27): $recursiveRef is a reference keyword of the walk
// (referenceKeywords), so every shape refused under $ref is refused alike
// under it, with the same findings at the same pointers (07 req 75:
// identical diagnostics), through CompileSchema, Check and Factory.Build,
// without a panic and without asking the loader (07 T15). Before the fix
// the walk never followed it: the shapes compiled, panicked, reached the
// loader or were reported at the root.
func TestRecursiveRefTwins(t *testing.T) {
	for _, tc := range recursiveTwinCases() {
		t.Run(tc.name, func(t *testing.T) {
			twin := compileIssues(t, refTwin(tc.shape))
			if len(twin) == 0 {
				t.Fatalf("the $ref twin of %.80s compiled", tc.shape)
			}
			want := make([]Issue, len(twin))
			for i, is := range twin {
				want[i] = Issue{recursivePointer(is.Pointer), is.Message}
			}
			if want[0].Pointer != tc.ptr {
				t.Fatalf("the $ref twin's first finding is at %q, want %q", want[0].Pointer, tc.ptr)
			}
			refusedThrough(t, recursiveOf(tc.shape), want)
		})
	}
}

// TestRecursiveRefRefusedByWalk shows that the walk itself refuses the
// shapes whose cost the library spends compiling them (audit class GC-01,
// rows CMP-33, CMP-36, VLU-52, REFS-11, REFS-12, CX-02, CX-03, CC-05 and
// CC-06 on the $recursiveRef route): the numbers, the lists of names,
// colliding ones included, the patterns, the counts, the references and
// the $schema of a target, every finding the walk's own, so the compile
// returns before the library compiles anything, below an additionalItems
// target too (VAL-49). It needs no clock. The other shapes are refused by
// the meta-schema checks (the library's, or the compile's below
// additionalItems) or by the analysis (a cycle, the doubling), which run
// after the walk.
func TestRecursiveRefRefusedByWalk(t *testing.T) {
	for _, tc := range recursiveTwinCases() {
		if strings.HasPrefix(tc.name, "additionalItems/") || strings.HasPrefix(tc.name, "meta-schema/") || strings.HasPrefix(tc.name, "recursiveAnchor/") ||
			strings.HasPrefix(tc.name, "evaluation/") || tc.name == "reference/cycle" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			schema := recursiveOf(tc.shape)
			p, issues := prepare([]byte(schema))
			if len(issues) > 0 {
				t.Fatal(issues)
			}
			w := newWalker(p.doc)
			w.walk()
			noPanic(t, "compile", func() {
				if _, compiled, _ := compilePrepared(p, refusingLoader{}, nil); len(w.issues) == 0 || !slices.Equal(compiled, w.issues) {
					t.Errorf("the walk found %+v, the compile %+v: want the walk's findings", w.issues, compiled)
				}
			})
		})
	}
}

// TestRecursiveRefFindings pins findings the $ref twins do not show (audit
// class GC-01): a $ref below a $recursiveRef target, refused at that $ref
// before it reaches the loader (CMP-35's shape); a boolean $recursiveAnchor
// at the root, under $defs, below an additionalItems whose $id a reference
// names, so the library compiles it (audit class GC-02, row CMP-27: an $id
// alone does not), and in the draft 2019-09 recursion idiom, each with the
// meta-schema's message (REFS-15, CX-04, VAL-48); and the fuzz input that
// reached the loader with the finding at the root (FuzzCompileSchema, G01
// attempt 3).
func TestRecursiveRefFindings(t *testing.T) {
	anchor := func(ptr string) []Issue { return []Issue{{ptr, msgRecursiveAnchor}} }
	for _, tc := range []struct {
		name, schema string
		want         []Issue
	}{
		{"$ref below the target", `{"$recursiveRef":"#/x","x":{"$ref":"y.json"}}`, []Issue{{"/x/$ref", `reference "y.json" resolves outside the schema document`}}},
		{"$recursiveAnchor at the root", `{"$recursiveAnchor":true}`, anchor("/$recursiveAnchor")},
		{"$recursiveAnchor under $defs", `{"$defs":{"a":{"$recursiveAnchor":true}}}`, anchor("/$defs/a/$recursiveAnchor")},
		{"$recursiveAnchor below additionalItems with an $id", `{"$ref":"urn:i","additionalItems":{"$id":"urn:i","$recursiveAnchor":true}}`, anchor("/additionalItems/$recursiveAnchor")},
		{"$recursiveAnchor in the draft 2019-09 idiom", `{"$recursiveAnchor":true,"items":{"$recursiveRef":"#"}}`, anchor("/$recursiveAnchor")},
		{"the fuzz input", `{"$recursiveRef":"0"}`, []Issue{{"/$recursiveRef", `reference "0" resolves outside the schema document`}}},
	} {
		t.Run(tc.name, func(t *testing.T) { refusedThrough(t, tc.schema, tc.want) })
	}
}

// recursiveAccepted are shapes the compile accepts under $ref and under
// $recursiveRef alike, with bodies each must accept (valid) or refuse.
func recursiveAccepted() []struct {
	name, shape   string
	valid, refuse []string
} {
	return []struct {
		name, shape   string
		valid, refuse []string
	}{
		{"definition", `{"REF":"#/$defs/a","$defs":{"a":{"type":"string"}}}`, []string{`"s"`}, []string{`5`}},
		{"linear recursion", strings.ReplaceAll(linearTree, `"$ref"`, refKeyword), []string{`[[["a"]],"b"]`}, []string{`[[1]]`}},
		{"outside the keywords", `{"REF":"#/x","x":{"type":"string","minLength":2}}`, []string{`"ab"`}, []string{`5`, `"a"`}},
		{"anchor", `{"REF":"#a","$defs":{"x":{"$anchor":"a","type":"string"}}}`, []string{`"s"`}, []string{`5`}},
		{"resource", `{"REF":"urn:x","$defs":{"x":{"$id":"urn:x","type":"integer"}}}`, []string{`1`}, []string{`1.5`}},
		{"relative to an embedded resource", `{"$id":"https://example.com/root.json","$defs":{"a":{"$id":"a.json","type":"integer"}},"properties":{"x":{"REF":"a.json"}}}`, []string{`{"x":1}`}, []string{`{"x":"1"}`}},
		{
			"a tree with a string $recursiveAnchor", `{"$recursiveAnchor":"node","type":"object","properties":{"kids":{"type":"array","items":{"REF":"#"}},"n":{"type":"integer"}}}`,
			[]string{`{"kids":[{"kids":[{"n":1}]}]}`},
			[]string{`{"kids":[{"n":"x"}]}`},
		},
		// Never compiled: a reference below an additionalItems no reference
		// reaches, and an unreferenced boolean $recursiveAnchor there (07
		// req 76).
		{"inert remote reference", `{"additionalItems":{"REF":"https://example.com/s"}}`, []string{`[1]`}, nil},
		{"inert $recursiveAnchor", `{"additionalItems":{"$recursiveAnchor":true,"multipleOf":0}}`, []string{`5`}, nil},
		{"inert $recursiveAnchor beside an $id", `{"additionalItems":{"$id":"urn:i","$recursiveAnchor":true}}`, []string{`5`}, nil},
		{"below a referenced additionalItems", `{"REF":"#/additionalItems","additionalItems":{"additionalItems":{"multipleOf":0}}}`, []string{`5`}, nil},
	}
}

// TestRecursiveRefAccepted covers what the class fix keeps (audit class
// GC-01, rows CMP-37, VAL-47; 07 reqs 75 and 76, decision 24: no new
// refusals): every $recursiveRef the walk now follows that resolves inside
// the document compiles as its $ref twin does, and the library enforces it
// statically, as $ref: the same bodies pass and fail, at the same keyword
// location, and none panics. Before the fix the target outside the
// keywords compiled unwalked, which the walked-position backstop now
// refuses unless the walk follows the reference.
func TestRecursiveRefAccepted(t *testing.T) {
	for _, tc := range recursiveAccepted() {
		t.Run(tc.name, func(t *testing.T) {
			spy := &spyLoader{}
			s, _, issues := compileRaw([]byte(recursiveOf(tc.shape)), spy)
			if s == nil || len(issues) > 0 || len(spy.urls) > 0 {
				t.Fatalf("compile = %+v, loads %q", issues, spy.urls)
			}
			twin := mustCompile(t, refTwin(tc.shape))
			for _, body := range append(slices.Clone(tc.valid), tc.refuse...) {
				noPanic(t, "Validate", func() {
					v := decodeInstance(t, body)
					loc, ok := s.Validate(v)
					tloc, tok := twin.Validate(v)
					if ok != slices.Contains(tc.valid, body) || ok != tok || loc != tloc {
						t.Errorf("Validate(%s) = %q, %v; the $ref twin %q, %v", body, loc, ok, tloc, tok)
					}
				})
			}
		})
	}
}

// TestReferenceKeywords pins referenceKeywords to the library (audit class
// GC-01, the class fix's list): of every keyword the library's bundled
// meta-schemas define, for drafts 4 to 2020-12, exactly those of the list
// make it compile the schema a string value names under draft 2020-12
// (its enqueueRef call sites, objcompiler.go:90, 269, 319); it compiles
// them in the list's order, the first that fails ending the compile with
// its error; compiledReference reads each one's compiled target; and
// isReference, the walk's reference sites and the inspection's edges name
// it, the walk walking its target live. A library change that adds, drops
// or reorders a reference keyword fails here, as does a site of the list
// that uses part of it.
func TestReferenceKeywords(t *testing.T) {
	mc := sjs.NewCompiler()
	mc.UseLoader(refusingLoader{})
	names := map[string]bool{}
	seen := map[*sjs.Schema]bool{}
	var visit func(s *sjs.Schema)
	visit = func(s *sjs.Schema) {
		if seen[s] {
			return
		}
		seen[s] = true
		for k := range s.Properties {
			names[k] = true
		}
		for _, e := range edges(s) {
			visit(e.child)
		}
	}
	for _, u := range []string{
		"https://json-schema.org/draft/2020-12/schema", "https://json-schema.org/draft/2019-09/schema",
		"http://json-schema.org/draft-07/schema", "http://json-schema.org/draft-06/schema", "http://json-schema.org/draft-04/schema",
	} {
		s, err := mc.Compile(u)
		if err != nil {
			t.Fatalf("Compile(%s) = %v", u, err)
		}
		visit(s)
	}
	for _, kw := range []string{"$ref", "$recursiveRef", "$dynamicRef", "$recursiveAnchor", "dependencies", "additionalItems", "unevaluatedItems"} {
		if !names[kw] {
			t.Fatalf("the meta-schemas define no %s: %d keywords", kw, len(names))
		}
	}
	libraryCompile := func(doc map[string]any) (*sjs.Compiler, *sjs.Schema, error) {
		c := sjs.NewCompiler()
		c.DefaultDraft(sjs.Draft2020)
		c.UseLoader(refusingLoader{})
		if err := c.AddResource(baseURL, doc); err != nil {
			return nil, nil, err
		}
		root, err := c.Compile(baseURL)
		return c, root, err
	}
	var refs []string
	for _, kw := range slices.Sorted(maps.Keys(names)) {
		c, _, err := libraryCompile(map[string]any{kw: "#/zz", "zz": map[string]any{"type": "string"}})
		if err != nil {
			continue
		}
		list, ok := compiledSchemas(c)
		if !ok {
			t.Fatal("compiledSchemas failed")
		}
		if slices.ContainsFunc(list, func(cs compiledSchema) bool { return cs.ptr == "/zz" }) {
			refs = append(refs, kw)
		}
	}
	if want := slices.Sorted(slices.Values(referenceKeywords())); !slices.Equal(refs, want) {
		t.Errorf("the library compiles as references %q, referenceKeywords has %q", refs, want)
	}
	list := referenceKeywords()
	for i := range list {
		doc := map[string]any{"zz": true}
		for j, kw := range list {
			doc[kw] = "#/zz"
			if j >= i {
				doc[kw] = fmt.Sprintf("#nope%d", j)
			}
		}
		_, _, err := libraryCompile(doc)
		for j := range list {
			if err == nil || strings.Contains(err.Error(), fmt.Sprintf("nope%d", j)) != (j == i) {
				t.Errorf("with %s the first failing reference, the compile failed with %v", list[i], err)
			}
		}
	}
	for _, kw := range list {
		_, root, err := libraryCompile(map[string]any{kw: "#/zz", "zz": map[string]any{"type": "string"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, other := range list {
			got := compiledReference(root, other)
			if other == kw && (got == nil || got.Location != baseURL+"#/zz") {
				t.Errorf("compiledReference(%s) = %v, want the schema at /zz", kw, got)
			}
			if other != kw && got != nil {
				t.Errorf("compiledReference(%s) of a schema with %s alone = %v", other, kw, got.Location)
			}
		}
		if !isReference(kw) {
			t.Errorf("isReference(%s) = false", kw)
		}
		// The walk records the site and walks its target live.
		w := newWalker(map[string]any{kw: "#/zz", "zz": map[string]any{"type": "string"}})
		w.walk()
		if len(w.issues) > 0 || len(w.sites) != 1 || w.sites[0].ptr != "/"+kw || w.sites[0].value != "#/zz" || w.seen["/zz"] != walkedLive {
			t.Errorf("the walk of a schema with %s alone: sites %+v, issues %+v, /zz walked %d", kw, w.sites, w.issues, w.seen["/zz"])
		}
		// The inspection's edges list the target once, named by kw, so its
		// fan-out, numbers and patterns are inspected through every member
		// of the list (a check finding: edges looping over part of the
		// list dropped $dynamicRef and no test failed).
		if e := edges(root); len(e) != 1 || e[0].kw != kw || e[0].sub != "" || e[0].factor != 1 || e[0].child.Location != baseURL+"#/zz" {
			t.Errorf("edges of a schema with %s alone = %+v, want one %s edge to /zz", kw, e, kw)
		}
	}
	if isReference("not") || compiledReference(&sjs.Schema{Ref: &sjs.Schema{}}, "not") != nil {
		t.Error("a keyword that is no reference counts as one")
	}
}

// TestRecursiveRefWalkAllocates is VLU-52's compile-cost row (audit class
// GC-01): twenty literals of 1e999999 behind $recursiveRef, which the
// library parsed for 421 ms and 56 MB before the walk followed the
// reference, are refused by the walk before the library runs, allocating
// under 4 MiB (about 41 KB measured); allocated bytes, a counter, so the
// test needs no clock.
func TestRecursiveRefWalkAllocates(t *testing.T) {
	schema := `{"$recursiveRef":"#/x","x":{"allOf":[` + joinRepeat(`{"minimum":1e999999}`, 20) + `]}}`
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, issues := compileRaw([]byte(schema), refusingLoader{})
	runtime.ReadMemStats(&after)
	if len(issues) != MaxIssues || issues[0].Pointer != "/x/allOf/0/minimum" {
		t.Fatalf("issues = %+v, want the walk's twenty number findings", issues)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 4<<20 {
		t.Errorf("the compile allocated %d bytes, want at most 4 MiB", n)
	}
}

// TestRecursiveRefMetaChecks covers the bounds of the library's meta-schema
// checks behind $recursiveRef (audit class GC-01, rows CX-21, CX-29,
// REFS-11): its targets are listed and ordered as $ref's (metaChecks), so
// the aligned ladder is checked once from its top, the misaligned ladder's
// repeated subschemas, the copies of the bookkeeping and the bytes of a
// ladder of 64 KB anchors that the checks repeat are each refused at the
// $recursiveRef of the target that crosses the bound, and the largest
// document under each bound compiles; the aligned ladder allocates at
// most twice what one reference does.
func TestRecursiveRefMetaChecks(t *testing.T) {
	recursive := func(s string) string { return strings.ReplaceAll(s, `"$ref"`, `"$recursiveRef"`) }
	members := func(n int) string {
		return ladderBottom(n, func(int) string { return `{"type":"string","minLength":1}` })
	}
	anchorLadder := func(levels int) string {
		return misalignedLadder(levels, `{"$anchor":"a`+strings.Repeat("b", 64<<10)+`"}`)
	}
	for _, tc := range []struct {
		name, fits, refused, msg string
	}{
		{"repeated subschemas", misalignedLadder(30, members(100)), misalignedLadder(30, members(125)), "more than 4096 subschemas"},
		// At another size than TestMetaChecksBound's, at the bound exactly.
		{"copies", outsideTargets(157, 3000), outsideTargets(158, 3000), "more than 524288 entries"},
		{"repeated bytes of long anchors", anchorLadder(4), anchorLadder(5), "more than 262144 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, schema := range []string{tc.refused, recursive(tc.refused)} {
				issues := compileIssues(t, schema)
				kw := "/$ref"
				if schema != tc.refused {
					kw = "/$recursiveRef"
				}
				if len(issues) != 1 || !strings.HasPrefix(issues[0].Pointer, "/$defs/r") || !strings.HasSuffix(issues[0].Pointer, kw) || !strings.Contains(issues[0].Message, tc.msg) {
					t.Errorf("issues = %+v, want one at a %s containing %q", issues, kw, tc.msg)
				}
			}
			mustCompile(t, recursive(tc.fits))
		})
	}
	ladder := recursive(refLadder(58, 59, members(1000)))
	w := newWalker(decodeInstance(t, ladder))
	w.walk()
	if len(w.issues) > 0 || !slices.Equal(w.checkOrder, []string{"/x"}) {
		t.Errorf("ladder: issues %+v, checked apart %q, want only /x", w.issues, w.checkOrder)
	}
	if l, s := compileAlloc(t, ladder), compileAlloc(t, recursive(refLadder(58, 1, members(1000)))); l > 2*s {
		t.Errorf("the ladder allocates %d bytes, one reference %d: want at most twice", l, s)
	}
}

// scanTotals is what a scan charges a body.
type scanTotals struct{ decide, locate, path, transient, work, walk, extra, compare, objects int64 }

// totalsOf returns the charges of scan.
func totalsOf(scan *bodyScan) scanTotals {
	return scanTotals{scan.decide, scan.locate, scan.path, scan.transient, scan.work, scan.walk, scan.extra, scan.compare, scan.objects}
}

// TestRecursiveRefCharges covers the runtime model of $recursiveRef (audit
// class GC-01, rows VAL-47, REFS-32, REFS-33, REFS-34): without a
// $recursiveAnchor of true, which the compile refuses, the library
// evaluates the target of a $recursiveRef as it does a $ref's
// (validator.go:795-799). Where no keyword sits beside the reference, the
// one difference, when the library evaluates it, is moot, so every charge
// of a body, passing or failing, in place, below items and recursive,
// equals its $ref twin's: its evaluation and in-place edge (work.go), its
// error unit (working.go keywords and errorUnits), and no walk of the
// whole dynamic scope, which the model charged it before the fix as for
// $dynamicRef. Beside a keyword of the value the charges differ, as
// TestDecisionPassOrder pins: the library evaluates $recursiveRef with
// $dynamicRef and the applicators after the value's own keywords, and its
// decision pass skips them once one of those failed.
func TestRecursiveRefCharges(t *testing.T) {
	for _, tc := range []struct {
		shape  string
		bodies []string
	}{
		{`{"REF":"#/$defs/x","$defs":{"x":{"type":"string"}}}`, []string{`"abc"`, `1`, `[1,2]`}},
		{`{"items":{"REF":"#/$defs/x"},"$defs":{"x":{"minimum":0}}}`, []string{`[1,-1,2.5]`, `[]`}},
		{`{"type":"object","properties":{"a":{"REF":"#"},"n":{"type":"integer"}}}`, []string{`{"a":{"a":{"n":1}}}`, `{"a":{"a":{"n":"x"}}}`}},
		{`{"allOf":[{"REF":"#/$defs/x"},{"REF":"#/$defs/y"}],"$defs":{"x":{"required":["a"]},"y":{"REF":"#/$defs/x"}}}`, []string{`{}`, `{"a":1}`}},
	} {
		r, twin := mustCompile(t, recursiveOf(tc.shape)), mustCompile(t, refTwin(tc.shape))
		for _, body := range tc.bodies {
			_, a := scanBody(t, r, body)
			_, b := scanBody(t, twin, body)
			if totalsOf(a) != totalsOf(b) {
				t.Errorf("%s with %s: charged %+v, the $ref twin %+v", recursiveOf(tc.shape), body, totalsOf(a), totalsOf(b))
			}
		}
	}
}

// TestDecisionPassOrder covers the group errorUnits counts each reference
// keyword and applicator in (audit class GC-01, rows VAL-47 and REFS-33;
// a check finding: moving $recursiveRef to $ref's group failed no test).
// The library evaluates $ref before the value's own keywords and
// $recursiveRef, $dynamicRef, not, allOf, anyOf and oneOf after them, and
// its decision pass skips the latter once one of the former failed
// (validator.go:148-175). Each keyword sits beside a minLength of 3, its
// target or branch a probe format that counts the library's calls: at the
// body "a", where minLength fails, the decision pass calls it for $ref
// alone, and at "abc" for every keyword, as the location pass does at
// both. The model charges the decision pass at "a" three error units for
// $ref (its error and minLength's, both kept, and their group) and one,
// minLength's or the keyword's, for each other; the location pass, which
// evaluates every keyword, the same two errors for each.
func TestDecisionPassOrder(t *testing.T) {
	shape := func(kw, target string) string {
		switch kw {
		case "not":
			return `{"not":` + target + `,"minLength":3}`
		case "allOf", "anyOf", "oneOf":
			return `{"` + kw + `":[` + target + `],"minLength":3}`
		}
		return `{"` + kw + `":"#/$defs/s","minLength":3,"$defs":{"s":` + target + `}}`
	}
	for _, kw := range slices.Concat(referenceKeywords(), []string{"not", "allOf", "anyOf", "oneOf"}) {
		t.Run(kw, func(t *testing.T) {
			doc, err := sjs.UnmarshalJSON(strings.NewReader(shape(kw, `{"format":"probe"}`)))
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			c := sjs.NewCompiler()
			c.DefaultDraft(sjs.Draft2020)
			c.UseLoader(refusingLoader{})
			c.AssertFormat()
			c.RegisterFormat(&sjs.Format{Name: "probe", Validate: func(any) error { calls++; return nil }})
			for url, d := range map[string]any{baseURL: doc, decideURL: map[string]any{"not": map[string]any{"$ref": baseURL}}} {
				if err := c.AddResource(url, d); err != nil {
					t.Fatal(err)
				}
			}
			locate, err := c.Compile(baseURL)
			if err != nil {
				t.Fatal(err)
			}
			decide, err := c.Compile(decideURL)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				body                string
				decideCalls, locate int
			}{{"a", 0, 1}, {"abc", 1, 1}} {
				if kw == "$ref" {
					tc.decideCalls = 1
				}
				calls = 0
				_ = decide.Validate(tc.body)
				if calls != tc.decideCalls {
					t.Errorf("the decision pass at %q called the format %d times, want %d", tc.body, calls, tc.decideCalls)
				}
				calls = 0
				_ = locate.Validate(tc.body)
				if calls != tc.locate {
					t.Errorf("the location pass at %q called the format %d times, want %d", tc.body, calls, tc.locate)
				}
			}
			want := int64(decideErrorBytes)
			if kw == "$ref" {
				want = 3 * decideErrorBytes
			}
			_, scan := scanBody(t, mustCompile(t, shape(kw, `{"type":"string"}`)), `"a"`)
			if scan.decide != want || scan.locate != 2*errorBytes {
				t.Errorf("decide, locate = %d, %d, want %d, %d", scan.decide, scan.locate, want, 2*errorBytes)
			}
		})
	}
}

// libraryAnalyzed compiles schema with the library alone, without the walk
// or the inspection, and gives it the runtime model of what it compiled,
// so a schema the walk refuses can be charged.
func libraryAnalyzed(t *testing.T, schema string) *Schema {
	t.Helper()
	s := librarySchema(t, schema)
	ws, issues := analyze([]*sjs.Schema{s.root})
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	in := newInspector()
	in.visit(s.root, "")
	s.work, s.numbers = ws, in.numbers
	return s
}

// TestCompiledNumberWork covers the defense in depth of audit class GC-01
// (rows VLU-16 and VAL-49): the schema side of number work comes from the
// schema's own literals and compiled values, not from the number bounds
// the walk applies. Within the bounds, literalWork is numberWork and
// boundWork nothing, at every edge of the bounds, so no schema the compile
// accepts is charged more; past them, a const or enum literal is charged
// its length and exponent and a numeric bound its digits, growing with
// their square: each of the four numeric bounds, its digits in the
// numerator or the denominator. A schema compiled without the walk, with
// const [1e99999] or a numeric bound of 1e99999 at a $recursiveRef target
// (VLU-16's probe: const [1e99999] was charged 1,540,592 units and ran at
// 29.7 ns a unit), is then over the work limit at once: 413 before the
// library runs.
func TestCompiledNumberWork(t *testing.T) {
	rat := func(lit string) *big.Rat {
		r, ok := new(big.Rat).SetString(lit)
		if !ok {
			t.Fatalf("SetString(%s)", lit)
		}
		return r
	}
	for _, lit := range []string{
		"0", "-0.5", "12345678901234567890", "1e1000", "-1e-1000", "1E+1000", "1e0001000",
		strings.Repeat("9", 123) + "e1000", "-" + strings.Repeat("9", 122) + "e1000", "0." + strings.Repeat("0", 119) + "1e-1000",
		"1." + strings.Repeat("7", 120) + "e-1000",
	} {
		if !numberInRange(lit) {
			t.Fatalf("%s is out of range", lit)
		}
		if got, want := literalWork(lit), numberWork(lit); got != want {
			t.Errorf("literalWork(%s) = %d, want numberWork's %d", lit, got, want)
		}
		if got := boundWork(rat(lit)); got != 0 {
			t.Errorf("boundWork(%s) = %d, want 0", lit, got)
		}
	}
	for _, tc := range []struct {
		lit         string
		lit2, bound int64
	}{
		// 1e1001 is past the exponent's bound, its 1,002 digits within the
		// digits a literal in range gives.
		{"1e1001", numberUnits + 6*numberByteUnits + numberExpUnits*(1001*1001/MaxNumberExponent), 0},
		// The digits ratDigits gives: 1,129 for 1e1128, the most a bound may
		// have for nothing more; 1,130 for 1e1129, 2,001 for 1e2000 and
		// 100,000 for 1e99999, over 1,129 digits squared.
		{"1e1128", numberUnits + 6*numberByteUnits + numberExpUnits*(1128*1128/MaxNumberExponent), 0},
		{"1e1129", numberUnits + 6*numberByteUnits + numberExpUnits*(1129*1129/MaxNumberExponent), numberExpUnits * (1131 + 1)},
		{"1e2000", numberUnits + 6*numberByteUnits + numberExpUnits*(2000*2000/MaxNumberExponent), numberExpUnits * (3546 + 1)},
		{"1e99999", numberUnits + 7*numberByteUnits + numberExpUnits*(99999*99999/MaxNumberExponent), numberExpUnits * (8857395 + 1)},
		{"1e-99999", numberUnits + 8*numberByteUnits + numberExpUnits*(99999*99999/MaxNumberExponent), numberExpUnits * (1 + 8857395)},
		{strings.Repeat("1", 129), numberUnits + numberByteUnits*(129*129/MaxNumberBytes), 0},
	} {
		if got := literalWork(tc.lit); got != tc.lit2 || got <= numberWork(tc.lit) {
			t.Errorf("literalWork(%.20s) = %d, want %d, above numberWork's %d", tc.lit, got, tc.lit2, numberWork(tc.lit))
		}
		if got := boundWork(rat(tc.lit)); got != tc.bound {
			t.Errorf("boundWork(%.20s) = %d, want %d", tc.lit, got, tc.bound)
		}
	}
	// An exponent past countCap saturates, its digits summed up to the
	// limit (exponentUpTo's clamp): exponents that would wrap an int64
	// without it, to 0, to 1, to a negative, or to a value past countCap
	// that pastBound would saturate anyway, and the same below zero (a
	// check finding: only the last was pinned, so dropping the clamp
	// charged 1e18446744073709551617 as an in-range literal).
	for _, lit := range []string{
		"1e18446744073709551616", "1e18446744073709551617", "1e9300000000000000000",
		"1e99999999999999999999", "-1e-18446744073709551617", "1E+9300000000000000000",
	} {
		if got, want := literalWork(lit), numberUnits+int64(len(lit))*numberByteUnits+numberExpUnits*countCap; got != want {
			t.Errorf("literalWork(%s) = %d, want it saturated at %d", lit, got, want)
		}
	}
	if got := exponentUpTo("1e18446744073709551617", MaxNumberExponent); got != MaxNumberExponent {
		t.Errorf("exponentUpTo of an exponent that wraps an int64 = %d, want the limit", got)
	}
	if pastBound(1<<30, 1000) != countCap || pastBound(countCap, 1000) != countCap || pastBound(1000, 1000) != 1000 || pastBound(1001, 1000) != 1002 {
		t.Error("pastBound is not x within the bound, its square over the bound past it, at most countCap")
	}
	// The schema side of one evaluation at the body 1, exactly: each of the
	// four numeric bounds (workWeight's loop over them), with its digits
	// in the numerator and in the denominator, the body passing it and
	// failing it.
	const kw = evalUnits + keywordUnits
	const parse = numberUnits + numberByteUnits
	type workCase struct {
		schema string
		want   int64
	}
	cases := []workCase{
		{`{"const":1e99999}`, kw + compareUnits + literalWork("1e99999") + scopeUnits + parse},
		{`{"enum":[1e99999]}`, kw + compareUnits + literalWork("1e99999") + scopeUnits + parse},
	}
	for _, bound := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
		for _, lit := range []string{"1e99999", "1e-99999"} {
			cases = append(cases, workCase{`{"` + bound + `":` + lit + `}`, kw + boundWork(rat(lit)) + scopeUnits + parse})
		}
	}
	for _, tc := range cases {
		if _, scan := scanBody(t, libraryAnalyzed(t, tc.schema), `1`); scan.work != tc.want {
			t.Errorf("%s: work = %d, want %d", tc.schema, scan.work, tc.want)
		}
	}
	// The same behind $recursiveRef at run time: each literal is over the
	// work limit at once.
	items := `[` + joinRepeat(`[1]`, 50) + `]`
	schemas := []string{
		`{"items":{"$recursiveRef":"#/x"},"x":{"const":[1e99999]}}`,
		`{"items":{"$recursiveRef":"#/x"},"x":{"items":{"enum":[1e99999]}}}`,
	}
	for _, bound := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum"} {
		schemas = append(schemas, `{"items":{"$recursiveRef":"#/x"},"x":{"items":{"`+bound+`":1e99999}}}`)
	}
	for _, schema := range schemas {
		f := newFilter(libraryAnalyzed(t, schema), errcode.Status(CodeRejected), limitsOf(0))
		x, _ := exchange("application/json", []byte(items))
		r := f.Handle(t.Context(), phase.OnRequestBody, x)
		if r.Outcome != filter.CannotDecide || !errors.Is(r.Err, errWorkTooLarge) {
			t.Errorf("%s: Handle = %+v, want the work limit's 413", schema, r)
		}
	}
}

// TestRecursiveAnchorBackstop covers the inspection's refusal of a
// compiled $recursiveAnchor of true (audit class GC-01, fix (2); rows
// VAL-48, REFS-15, CX-04): the compiled schema gets the field injected, as
// the library sets it at a position no meta-schema check covers, at the
// root, in a member and at a $recursiveRef target, and the inspection
// refuses it at its keyword with the meta-schema's own message, the one
// the library's check gives at every position it covers.
func TestRecursiveAnchorBackstop(t *testing.T) {
	if got := compileIssues(t, `{"$recursiveAnchor":true}`); !slices.Equal(got, []Issue{{"/$recursiveAnchor", msgRecursiveAnchor}}) {
		t.Fatalf("the meta-schema's finding = %+v, want msgRecursiveAnchor", got)
	}
	for _, tc := range []struct {
		name string
		at   func(*Schema) *sjs.Schema
		ptr  string
	}{
		{"root", func(s *Schema) *sjs.Schema { return s.root }, "/$recursiveAnchor"},
		{"member", func(s *Schema) *sjs.Schema { return s.root.Properties["a"] }, "/properties/a/$recursiveAnchor"},
		{"$recursiveRef target", func(s *Schema) *sjs.Schema { return s.root.RecursiveRef }, "/$defs/d/$recursiveAnchor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustCompile(t, `{"$recursiveRef":"#/$defs/d","$defs":{"d":{"type":"string"}},"properties":{"a":{"type":"string"}}}`)
			in := newInspector()
			if in.visit(s.root, ""); len(in.issues) > 0 {
				t.Fatalf("before the injection: %+v", in.issues)
			}
			tc.at(s).RecursiveAnchor = true
			in = newInspector()
			in.visit(s.root, "")
			if want := []Issue{{tc.ptr, msgRecursiveAnchor}}; !slices.Equal(in.issues, want) {
				t.Errorf("issues = %+v, want %+v", in.issues, want)
			}
		})
	}
}

// TestUnwalkedBackstop covers the class backstop audit classes GC-01 and
// GC-02 share (compiled.go unwalked): every schema the library compiled at
// a position the walk did not walk live is refused, at the top-most such
// positions only, in pointer order, at most MaxIssues; a walked-live
// listing passes. Injected: a listing of positions the walk never saw, or
// walked inert, and a compile whose walk is made to part from the library
// at one compiled position, which the compile refuses there. On the
// $recursiveRef route (class GC-01, row CMP-33) and the base-mismatch
// route (class GC-02, rows REFS-02 and CMP-70: minimum 1e99999 and 30 x
// [a-z]{1000} there compiled and were accepted, and later met this
// backstop), the walk now walks the target live, so its own rules refuse
// them first and the backstop never fires.
func TestUnwalkedBackstop(t *testing.T) {
	p, _ := prepare([]byte(`{"properties":{"a":{"properties":{"b":true}},"c":true},"x":{"y":true},"z":{"additionalItems":{"w":true}}}`))
	w := newWalker(p.doc)
	w.walk()
	w.seen["/properties/c"] = walkedInert
	listing := func(ptrs ...string) []compiledSchema {
		out := make([]compiledSchema, len(ptrs))
		for i, ptr := range ptrs {
			out[i] = compiledSchema{ptr: ptr}
		}
		return out
	}
	many := make([]string, MaxIssues+5)
	var manyWant []Issue
	for i := range many {
		many[i] = fmt.Sprintf("/x/m%02d", i)
		if i < MaxIssues {
			manyWant = append(manyWant, Issue{many[i], msgUnwalkedText})
		}
	}
	for _, tc := range []struct {
		name    string
		listing []compiledSchema
		want    []Issue
	}{
		{"walked live", listing("", "/properties/a", "/properties/a/properties/b"), nil},
		{"never walked", listing("", "/x", "/x/y"), []Issue{{"/x", msgUnwalkedText}}},
		{"walked inert", listing("", "/properties/c"), []Issue{{"/properties/c", msgUnwalkedText}}},
		{"top-most only, in pointer order", listing("", "/properties/c", "/x", "/x/y", "/z/additionalItems/w"), []Issue{
			{"/properties/c", msgUnwalkedText}, {"/x", msgUnwalkedText}, {"/z/additionalItems/w", msgUnwalkedText},
		}},
		{"below a walked position", listing("", "/properties/a/x"), []Issue{{"/properties/a/x", msgUnwalkedText}}},
		{"at most MaxIssues", listing(append([]string{""}, many...)...), manyWant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.unwalked(tc.listing); !slices.Equal(got, tc.want) {
				t.Errorf("unwalked = %+v, want %+v", got, tc.want)
			}
		})
	}
	t.Run("wired into the compile", func(t *testing.T) {
		p, _ := prepare([]byte(`{"$ref":"#/x","x":{"type":"string"}}`))
		w := newWalker(p.doc)
		w.walk()
		w.seen["/x"] = walkedInert
		if s, issues, _ := compileWalked(p, w, refusingLoader{}, nil, newRegexpEngine()); s != nil || !slices.Equal(issues, []Issue{{"/x", msgUnwalkedText}}) {
			t.Errorf("compile with /x made inert = %v, %+v, want the backstop at /x", s != nil, issues)
		}
	})
	for _, route := range escapedRoutes() {
		for _, tc := range []struct {
			resource, want string
		}{
			{`{"minimum":1e99999}`, "/minimum"},
			{`{"pattern":"` + strings.Repeat("[a-z]{1000}", 30) + `"}`, "/pattern"},
			{`{"type":"string"}`, ""},
		} {
			doc, at := onRoute(route, "", "", tc.resource)
			if tc.want == "" {
				mustCompile(t, doc)
			} else if issues := compileIssues(t, doc); len(issues) != 1 || issues[0].Pointer != at+tc.want {
				t.Errorf("compile(%.80s) = %+v, want the walk's finding at %s", doc, issues, at+tc.want)
			}
		}
	}
}

// panicContext is a context whose Err panics from its calls'th call on.
type panicContext struct {
	context.Context
	calls int
}

// Err panics once the calls run out.
func (c *panicContext) Err() error {
	if c.calls--; c.calls < 0 {
		panic("injected")
	}
	return nil
}

// TestCompileRecovers covers the recovered panic (compileWith, the
// backstop audit classes GC-01 and GC-02 share): a panic during the
// compile is one finding at the document, RZ-CFG-005 through
// Factory.Build, with the compile's regexp engine frozen. Injected: a stop
// hook that panics before the library compiles and before the analysis,
// and a Factory.Build whose context panics between its stages. The
// shapes where the library's meta-schema check dereferenced math/big's nil
// result, a multipleOf of 1e9999999 (row VLU-51's panic, which ended stage
// H in every binary before the fix) and an enum under the draft-07
// dialect (row CC-05), on the $recursiveRef route (class GC-01) and the
// base-mismatch route (class GC-02, where this backstop caught them before
// the walk walked the route), are refused by the walk's own rules before
// the library compiles, through CompileSchema, Check and Factory.Build.
func TestCompileRecovers(t *testing.T) {
	want := []Issue{{"", msgPanickedText}}
	for _, tc := range []struct {
		name  string
		stops int
	}{{"before the library compiles", 0}, {"before the analysis", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := prepare([]byte(`{"pattern":"^a","properties":{"a":{"$recursiveRef":"#"}}}`))
			eng := newRegexpEngine()
			n := tc.stops
			s, issues, err := compileWith(p, refusingLoader{}, func() error {
				if n--; n < 0 {
					panic("injected")
				}
				return nil
			}, eng)
			if s != nil || err != nil || !slices.Equal(issues, want) {
				t.Errorf("compile = %v, %+v, %v, want %+v", s != nil, issues, err, want)
			}
			if !eng.frozen || eng.cache != nil {
				t.Errorf("after the panic the engine is frozen %v with cache %v", eng.frozen, eng.cache)
			}
		})
	}
	for _, calls := range []int{1, 2} {
		ctx := &panicContext{Context: t.Context(), calls: calls}
		f, err := Factory{}.Build(ctx, filter.BuildEnv{Config: configOf(`{"type":"string"}`)})
		if code, _ := errcode.CodeOf(err); f != nil || code != CodeConfig || !strings.Contains(err.Error(), msgPanickedText) {
			t.Errorf("Build with a context panicking at call %d = %v, %v, want %s", calls+1, f, err, CodeConfig)
		}
	}
	for _, route := range escapedRoutes() {
		doc, at := onRoute(route, "", "", `{"multipleOf":1e9999999}`)
		refusedThrough(t, doc, []Issue{numberIssue(at + "/multipleOf")})
		doc, at = onRoute(route, "", "", `{"$id":"urn:w","$schema":"http://json-schema.org/draft-07/schema#","enum":[`+literals(20, "")+`,1e9999999]}`)
		refusedThrough(t, doc, []Issue{{at + "/$schema", msgDialectText}, numberIssue(at + "/enum/20")})
	}
}
