// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/filter"
)

// spyLoader records every URL the compiler asks for and refuses it, so a
// test proves that no file or network access is attempted.
type spyLoader struct {
	mu   sync.Mutex
	urls []string
}

func (l *spyLoader) Load(u string) (any, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.urls = append(l.urls, u)
	return nil, fmt.Errorf("%w: %s", ErrRemoteReference, u)
}

// TestCompileSchemaRefusesRemoteReferences checks that a $ref to http,
// https or file URLs, a relative reference and a bundled meta-schema are
// RZ-CFG-005 at the reference, found before the compiler runs, so the
// loader is never asked.
func TestCompileSchemaRefusesRemoteReferences(t *testing.T) {
	for _, tc := range []struct {
		name, schema, ptr string
	}{
		{"http", `{"$ref":"http://example.com/s.json"}`, "/$ref"},
		{"https", `{"properties":{"a":{"$ref":"https://example.com/s.json#/x"}}}`, "/properties/a/$ref"},
		{"file", `{"$ref":"file:///etc/passwd"}`, "/$ref"},
		{"relative", `{"$ref":"common.json"}`, "/$ref"},
		{"dynamicRef", `{"$dynamicRef":"https://example.com/d"}`, "/$dynamicRef"},
		{"unreferenced definition", `{"$defs":{"x":{"$ref":"https://example.com/x"}}}`, "/$defs/x/$ref"},
		{"meta-schema 2020-12", `{"$ref":"https://json-schema.org/draft/2020-12/schema"}`, "/$ref"},
		{"meta-schema draft-07", `{"items":{"$ref":"http://json-schema.org/draft-07/schema#"}}`, "/items/$ref"},
		{"relative to an embedded $id", `{"$defs":{"a":{"$id":"https://example.com/a/","$ref":"b.json"}}}`, "/$defs/a/$ref"},
		// The library compiles $recursiveRef as a reference under draft
		// 2020-12.
		{"recursiveRef https", `{"$recursiveRef":"https://example.com/x"}`, "/$recursiveRef"},
		{"recursiveRef file", `{"items":{"$recursiveRef":"file:///etc/passwd"}}`, "/items/$recursiveRef"},
		{"recursiveRef meta-schema 2020-12", `{"$recursiveRef":"https://json-schema.org/draft/2020-12/schema"}`, "/$recursiveRef"},
		{"recursiveRef meta-schema 2019-09", `{"$recursiveRef":"https://json-schema.org/draft/2019-09/schema"}`, "/$recursiveRef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &spyLoader{}
			s, _, issues := compileRaw([]byte(tc.schema), spy)
			if s != nil || len(issues) == 0 {
				t.Fatalf("compile(%s) = %v, %v, want a finding", tc.schema, s, issues)
			}
			if issues[0].Pointer != tc.ptr || !strings.Contains(issues[0].Message, "outside the schema document") {
				t.Errorf("issues = %+v, want one at %s about a reference outside the document", issues, tc.ptr)
			}
			if len(spy.urls) > 0 {
				t.Errorf("loader saw %q, want no load", spy.urls)
			}
		})
	}
	if _, err := (refusingLoader{}).Load("file:///etc/passwd"); !errors.Is(err, ErrRemoteReference) {
		t.Errorf("refusingLoader.Load = %v, want ErrRemoteReference", err)
	}
}

// TestCompilerLoaderBackstop skips the walk to prove the compiler itself
// asks only the refusing loader, and that a refused load is reported at
// every reference that leaves the document.
func TestCompilerLoaderBackstop(t *testing.T) {
	const schema = `{"properties":{"a":{"$ref":"https://example.com/a"},"b":{"$ref":"file:///etc/passwd"},"c":{"$ref":"#/$defs/c"}},"$defs":{"c":true}}`
	p, issues := prepare([]byte(schema))
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	w := newWalker(p.doc)
	w.walk()
	if len(w.issues) != 2 {
		t.Fatalf("walk issues = %+v, want the two remote references", w.issues)
	}
	w.issues = nil
	spy := &spyLoader{}
	s, issues, _ := compileWalked(p, w, spy, nil, newRegexpEngine())
	if s != nil || len(spy.urls) != 1 {
		t.Fatalf("compile = %v, loads %q, want one refused load", s, spy.urls)
	}
	want := []Issue{
		{Pointer: "/properties/a/$ref", Message: `reference "https://example.com/a" resolves outside the schema document`},
		{Pointer: "/properties/b/$ref", Message: `reference "file:///etc/passwd" resolves outside the schema document`},
	}
	if !slices.Equal(issues, want) {
		t.Errorf("issues = %+v, want %+v", issues, want)
	}
	if got := newWalker(nil).loadIssues("urn:x"); len(got) != 1 || got[0].Pointer != "" || !strings.Contains(got[0].Message, `"urn:x"`) {
		t.Errorf("loadIssues without sites = %+v", got)
	}
}

// TestCompileSchemaFindings covers the other rules with the pointer each
// finding names.
func TestCompileSchemaFindings(t *testing.T) {
	for _, tc := range []struct {
		name, schema, ptr, msg string
	}{
		{"not JSON", `{"a":`, "", "not valid JSON"},
		{"duplicate member", `{"type":"string","type":"number"}`, "", "duplicate object member name"},
		{"array", `[]`, "", "object or a boolean"},
		{"string", `"x"`, "", "object or a boolean"},
		{"draft-07 at the root", `{"$schema":"http://json-schema.org/draft-07/schema#"}`, "/$schema", "$schema must be"},
		{"draft-07 nested", `{"properties":{"a":{"$schema":"http://json-schema.org/draft-07/schema#"}}}`, "/properties/a/$schema", "$schema must be"},
		{"2020-12 with a fragment", `{"$schema":"https://json-schema.org/draft/2020-12/schema#"}`, "/$schema", "$schema must be"},
		{"$schema not a string", `{"$schema":1}`, "/$schema", "$schema must be"},
		{"custom meta-schema", `{"$schema":"https://example.com/meta"}`, "/$schema", "$schema must be"},
		{"draft-07 resource behind a reference", `{"$ref":"#/x","x":{"$id":"urn:y","$schema":"http://json-schema.org/draft-07/schema#"}}`, "/x/$schema", "$schema must be"},
		// A $recursiveRef target is walked as a $ref target: a $schema
		// the library would load, and the other rules.
		{"custom meta-schema behind a recursive reference", `{"$recursiveRef":"#/x","x":{"$id":"urn:y","$schema":"https://example.com/meta"}}`, "/x/$schema", "$schema must be"},
		{"pattern behind a recursive reference", `{"$recursiveRef":"#/x","x":{"pattern":"(?=a)"}}`, "/x/pattern", "RE2"},
		{"number behind a recursive reference", `{"$recursiveRef":"#/x","x":{"minimum":1e999999}}`, "/x/minimum", "out of range"},
		{"look-ahead", `{"pattern":"(?=a)"}`, "/pattern", "RE2"},
		{"back-reference", `{"properties":{"a":{"pattern":"(a)\\1"}}}`, "/properties/a/pattern", "RE2"},
		{"look-behind in patternProperties", `{"patternProperties":{"(?<!x)y":true}}`, "/patternProperties/(?<!x)y", "RE2"},
		{"pattern behind a reference", `{"$ref":"#/x","x":{"pattern":"(?=a)"}}`, "/x/pattern", "RE2"},
		{"missing pointer target", `{"$ref":"#/$defs/nope"}`, "/$ref", "does not resolve"},
		{"pointer to a non-schema", `{"$ref":"#/required","required":["a"]}`, "/$ref", "not a schema"},
		{"bad fragment escape", `{"$ref":"#/%zz"}`, "/$ref", "not a valid URI"},
		{"missing anchor", `{"$ref":"#nope"}`, "/$ref", "anchor its schema resource does not define"},
		{"anchor of another resource", `{"$defs":{"a":{"$id":"urn:a","$anchor":"x"}},"$ref":"#x"}`, "/$ref", "anchor its schema resource does not define"},
		{"duplicate anchor", `{"$defs":{"a":{"$anchor":"x"},"b":{"$dynamicAnchor":"x"}}}`, "/$defs/b/$dynamicAnchor", `already defined by the schema at "/$defs/a"`},
		{"invalid reference", `{"$ref":"http://[::1"}`, "/$ref", "not a valid URI reference"},
		{"meta-schema violation", `{"allOf":[true,{"minLength":-1}]}`, "/allOf/1/minLength", "invalid JSON Schema"},
		{"wrong keyword type", `{"type":5}`, "/type", "invalid JSON Schema"},
		// A target outside the keywords is checked first (metacheck.go).
		{"meta-schema violation behind a reference", `{"$ref":"#/x","x":{"type":5}}`, "/x/type", "invalid JSON Schema"},
		{"number exponent", `{"minimum":1e-1001}`, "/minimum", "out of range"},
		{"number in const data", `{"const":{"a":[1e2000]}}`, "/const/a/0", "out of range"},
		{"number in enum data", `{"enum":["a",1e-2000]}`, "/enum/1", "out of range"},
		{"integer keyword", `{"properties":{"a":{"minLength":1e2000}}}`, "/properties/a/minLength", "out of range"},
		{"number behind a reference", `{"$ref":"#/x","x":{"maxItems":1e2000}}`, "/x/maxItems", "out of range"},
		{"long number", `{"maximum":` + strings.Repeat("1", MaxNumberBytes+1) + `}`, "/maximum", "out of range"},
		{"duplicate $id", `{"$defs":{"a":{"$id":"urn:a"},"b":{"$id":"urn:a"}}}`, "/$defs/b/$id", `already the identifier of the schema at "/$defs/a"`},
		{"root $id repeated", `{"$id":"https://example.com/s","$defs":{"a":{"$id":"https://example.com/s"}}}`, "/$defs/a/$id", "already the identifier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := compileIssues(t, tc.schema)
			if len(issues) == 0 {
				t.Fatalf("compile(%s) passed, want a finding", tc.schema)
			}
			if issues[0].Pointer != tc.ptr || !strings.Contains(issues[0].Message, tc.msg) {
				t.Errorf("issues = %+v, want the first at %q containing %q", issues, tc.ptr, tc.msg)
			}
			for _, is := range issues {
				if strings.ContainsAny(is.Message, "\n\r") || len(is.Message) > maxMessageBytes+3 || strings.Contains(is.Message, baseURL) {
					t.Errorf("message %q is not one short line free of the compile URL", is.Message)
				}
			}
			_, err := CompileSchema([]byte(tc.schema))
			var ce *CompileError
			if !errors.As(err, &ce) || !errors.Is(err, ErrSchema) || !slices.Equal(ce.Issues, issues) {
				t.Errorf("CompileSchema error = %v, want a *CompileError matching ErrSchema with the same issues", err)
			}
		})
	}
}

// TestCompileSchemaAccepts covers what the compile accepts: internal
// references, embedded resources, anchors, dynamic references, formats as
// annotations, unknown and x-ruralz-* keywords, boolean schemas.
func TestCompileSchemaAccepts(t *testing.T) {
	for _, tc := range []struct {
		name, schema, valid, invalid string
	}{
		{"true", `true`, `{"anything":[1]}`, ""},
		{"false", `false`, "", `{}`},
		{"empty object", `{}`, `null`, ""},
		{"explicit draft", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`, `{}`, `[]`},
		{"$defs reference", `{"$defs":{"s":{"type":"string"}},"properties":{"a":{"$ref":"#/$defs/s"}}}`, `{"a":"x"}`, `{"a":1}`},
		{"definitions reference", `{"definitions":{"s":{"type":"string"}},"items":{"$ref":"#/definitions/s"}}`, `["x"]`, `[1]`},
		{"embedded $id", `{"$id":"https://example.com/root.json","$defs":{"a":{"$id":"a.json","type":"integer"}},"properties":{"x":{"$ref":"a.json"}}}`, `{"x":1}`, `{"x":"1"}`},
		{"anchor", `{"$defs":{"n":{"$anchor":"num","type":"number"}},"items":{"$ref":"#num"}}`, `[1.5]`, `["a"]`},
		{"recursive", `{"type":"object","properties":{"kids":{"type":"array","items":{"$ref":"#"}},"n":{"type":"integer"}}}`, `{"kids":[{"kids":[{"n":1}]}]}`, `{"kids":[{"n":"x"}]}`},
		{"dynamic reference", `{"$dynamicAnchor":"node","type":"object","properties":{"next":{"$dynamicRef":"#node"}}}`, `{"next":{"next":{}}}`, `{"next":1}`},
		{"format is an annotation", `{"format":"email"}`, `"not an email"`, ""},
		// Every format is an annotation where the walk walks, the asserted
		// format 'regex' included: the inspector refuses only a format the
		// library compiled as an assertion, never the keyword.
		{"format regex is an annotation", `{"properties":{"a":{"format":"regex"}}}`, `{"a":"("}`, ""},
		{"format uri is an annotation", `{"format":"uri"}`, `"::"`, ""},
		{"format json-pointer is an annotation", `{"items":{"format":"json-pointer"}}`, `["x"]`, ""},
		{"formats behind a reference are annotations", `{"$ref":"#/$defs/d","$defs":{"d":{"allOf":[{"format":"date-time"},{"format":"uuid"},{"format":"hostname"}]}}}`, `"-x-"`, ""},
		{"unknown format", `{"format":"x-ruralz-unknown"}`, `"x"`, ""},
		{"content is an annotation", `{"contentMediaType":"application/json","contentEncoding":"base64"}`, `"%%%"`, ""},
		{"contentSchema is an annotation", `{"contentMediaType":"application/json","contentSchema":{"type":"integer"}}`, `"x"`, ""},
		{"unknown keywords", `{"x-ruralz-ref":"Route","x-ruralz-secret":true,"foo":{"$ref":"https://example.com"},"type":"string"}`, `"x"`, `1`},
		{"reference into an unknown keyword", `{"$ref":"#/components/s","components":{"s":{"type":"boolean"}}}`, `true`, `1`},
		{"Go regexp", `{"pattern":"^\\p{Lu}[a-z]+$"}`, `"Ab"`, `"ab"`},
		{"exact numbers", `{"maximum":1e400,"multipleOf":0.1}`, `0.3`, `1e401`},
		// Realistic recursive schemas: accepted with a nesting limit.
		{"rich text", richText, quotes(3), `{"type":"doc","content":[{"type":"table"}]}`},
		{"component tree", componentTree, `{"kind":"row","children":[{"text":"a"},{"kind":"column","children":[]}]}`, `{"kind":"row"}`},
		// Annotations and unknown keywords are never parsed.
		{"annotation numbers", `{"x-ruralz-note":1e2000,"examples":[` + strings.Repeat("9", 130) + `],"default":1e-5000,"type":"string"}`, `"x"`, `1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustCompile(t, tc.schema)
			f := newFilter(s, 400, bodyLimits{})
			if tc.valid != "" {
				if r := handle(t, f, tc.valid); r.Outcome != filter.Continue {
					t.Errorf("valid body %s: %+v", tc.valid, r.Response)
				}
			}
			if tc.invalid != "" {
				detailOf(t, handle(t, f, tc.invalid))
			}
		})
	}
}

// nestedAnyOf returns anyOf of two branches nested depth deep.
func nestedAnyOf(depth int) string {
	s := `{"type":"string"}`
	for range depth {
		s = `{"anyOf":[` + s + `,` + s + `]}`
	}
	return s
}

// TestFanOut covers the nested applicator bound: 2 branches nested
// 10 deep pass (1,024), 11 deep fail at the outer anyOf; $ref
// cycles count once.
func TestFanOut(t *testing.T) {
	branches := func(n int, kw string) string {
		items := make([]string, n)
		for i := range items {
			items[i] = fmt.Sprintf(`{"const":%d}`, i)
		}
		return `{"` + kw + `":[` + strings.Join(items, ",") + `]}`
	}
	for _, tc := range []struct {
		name, schema, ptr string
	}{
		{"anyOf 2 nested 10", nestedAnyOf(10), ""},
		{"anyOf 2 nested 11", nestedAnyOf(11), "/anyOf"},
		{"allOf 1024 wide", branches(1024, "allOf"), ""},
		{"oneOf 1025 wide", branches(1025, "oneOf"), "/oneOf"},
		{"anyOf 32 under oneOf 32", `{"oneOf":[` + strings.Repeat(branches(32, "anyOf")+",", 31) + branches(32, "anyOf") + `]}`, ""},
		{"anyOf 33 under oneOf 32", `{"oneOf":[` + strings.Repeat(branches(32, "anyOf")+",", 31) + branches(33, "anyOf") + `]}`, "/oneOf"},
		{"if then else nested", strings.Repeat(`{"if":{"type":"string"},"then":true,"else":`, 6) + `true` + strings.Repeat(`}`, 6), ""},
		{"if then else nested deeper", strings.Repeat(`{"if":{"type":"string"},"then":true,"else":`, 7) + `true` + strings.Repeat(`}`, 7), "/else"},
		{"if true drops else", strings.Repeat(`{"if":true,"then":true,"else":`, 7) + `true` + strings.Repeat(`}`, 7), ""},
		{"fan-out through a reference", `{"$defs":{"d":` + nestedAnyOf(10) + `},"allOf":[{"$ref":"#/$defs/d"},true]}`, "/allOf"},
		// The inspection follows a $recursiveRef as a $ref.
		{"fan-out through a recursive reference", `{"$defs":{"d":` + nestedAnyOf(10) + `},"allOf":[{"$recursiveRef":"#/$defs/d"},true]}`, "/allOf"},
		// And a $dynamicRef to its static target, the third member of
		// referenceKeywords that edges loops over.
		{"fan-out through a dynamic reference", `{"$defs":{"d":` + nestedAnyOf(10) + `},"allOf":[{"$dynamicRef":"#/$defs/d"},true]}`, "/allOf"},
		{"recursive anyOf counted once", `{"$defs":{"t":{"anyOf":[{"type":"null"},{"items":{"$ref":"#/$defs/t"}},{"items":{"type":"string"}}]}},"$ref":"#/$defs/t"}`, ""},
		{"unreferenced definition", `{"$defs":{"bomb":` + nestedAnyOf(11) + `}}`, "/$defs/bomb/anyOf"},
		// The fan-out of a path through a cycle does not depend on the
		// order of members. /allOf/1 -> b -> /properties/x -> a -> /anyOf/1
		// -> 4^4 repeats no schema: 2*2*2*256.
		{"cycle entered first through a", cycleDefs(`{"$ref":"#/$defs/a"},{"$ref":"#/$defs/b"}`), "/allOf"},
		{"cycle entered first through b", cycleDefs(`{"$ref":"#/$defs/b"},{"$ref":"#/$defs/a"}`), "/allOf"},
		{"cycle and a sibling", cycleDefs(`{"$ref":"#/$defs/b"},true`), "/allOf"},
		{"cycle and a sibling reference", wideCycle(`{"$ref":"#/$defs/a","properties":{"y":{"$ref":"#/$defs/b"}}`), "/$defs/b/anyOf"},
		{"cycle alone", wideCycle(`{"properties":{"y":{"$ref":"#/$defs/b"}}`), "/$defs/b/anyOf"},
		// Of two independent crossings, the one whose pointer sorts first,
		// though b is visited, and crosses, first.
		{"two crossings", `{"$ref":"#/$defs/b","$defs":{"a":` + branches(1025, "oneOf") + `,"b":` + branches(1025, "anyOf") + `}}`, "/$defs/a/oneOf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := compileIssues(t, tc.schema)
			if tc.ptr == "" {
				if len(issues) > 0 {
					t.Fatalf("issues = %+v, want none", issues)
				}
				return
			}
			if len(issues) != 1 || issues[0].Pointer != tc.ptr || !strings.Contains(issues[0].Message, "fan out to more than 1024") {
				t.Fatalf("issues = %+v, want one fan-out finding at %s", issues, tc.ptr)
			}
		})
	}
}

// fourWay returns anyOf of four branches nested depth deep: a fan-out of
// 4^depth.
func fourWay(depth int) string {
	s := `{"type":"string"}`
	for range depth {
		s = `{"anyOf":[` + strings.Repeat(s+",", 3) + s + `]}`
	}
	return s
}

// cycleDefs returns a schema applying members in an allOf, with a and b
// definitions referring to each other through a two-branch anyOf each, a
// also to a fan-out of 256.
func cycleDefs(members string) string {
	return `{"allOf":[` + members + `],"$defs":{"a":{"anyOf":[{"$ref":"#/$defs/b"},` + fourWay(4) + `]},` +
		`"b":{"anyOf":[{"properties":{"x":{"$ref":"#/$defs/a"}}},true]}}}`
}

// wideCycle returns root members with definitions a and b in one cycle: b
// an anyOf of 512 branches whose first refers to a, a an anyOf whose
// second branch fans out to 512, so the path through b's first branch and
// a's second fans out to 524,288 without repeating a schema.
func wideCycle(root string) string {
	branches := make([]string, 511)
	for i := range branches {
		branches[i] = fmt.Sprintf(`{"const":%d}`, i)
	}
	return root + `,"$defs":{"b":{"anyOf":[{"$ref":"#/$defs/a"},` + strings.Join(branches, ",") + `]},` +
		`"a":{"anyOf":[{"properties":{"x":{"$ref":"#/$defs/b"}}},` + nestedAnyOf(9) + `]}}}`
}

// TestFanOutBound covers the upper bound the fan-out takes when the paths
// within a component of mutually referring schemas take more than its step
// budget: the product of each schema's largest factor and the largest
// fan-out leaving the component, which can only refuse more. Here a hub
// refers to three anyOf schemas of 32 branches whose only way on is back to
// the hub: no path takes more than two of them (1,024, at the bound), the
// upper bound all three.
func TestFanOutBound(t *testing.T) {
	branches := strings.TrimSuffix(strings.Repeat(`{"type":"null"},`, 31), ",")
	schema := `{"$ref":"#/$defs/h","$defs":{"h":{"properties":{"a":{"$ref":"#/$defs/s1"},"b":{"$ref":"#/$defs/s2"},"c":{"$ref":"#/$defs/s3"}}},` +
		`"s1":{"anyOf":[{"$ref":"#/$defs/h"},` + branches + `]},"s2":{"anyOf":[{"$ref":"#/$defs/h"},` + branches + `]},"s3":{"anyOf":[{"$ref":"#/$defs/h"},` + branches + `]}}}`
	if issues := compileIssues(t, schema); len(issues) > 0 {
		t.Fatalf("exact: %+v, want none", issues)
	}
	// The bound counts the fan-out leaving the component. Here the factors
	// inside it multiply to 2, and the path out through its anyOf to a
	// fan-out of 1,024 makes 2,048: refused exactly and by the bound alike.
	exit := `{"$ref":"#/$defs/h","$defs":{"h":{"properties":{"a":{"$ref":"#/$defs/s"}}},"s":{"anyOf":[{"$ref":"#/$defs/h"},` + nestedAnyOf(10) + `]}}}`
	for _, tc := range []struct {
		schema string
		exact  bool
	}{{schema, false}, {exit, true}} {
		root := libraryCompile(t, tc.schema)
		for _, steps := range []int{fanOutSteps, 0} {
			in := newInspector()
			in.steps = steps
			in.visit(root, "")
			in.fanOut()
			refused := len(in.issues) == 1 && strings.Contains(in.issues[0].Message, "fan out")
			if refused != (tc.exact || steps == 0) {
				t.Errorf("%.40s with %d steps: %+v", tc.schema, steps, in.issues)
			}
		}
	}
}

// TestSizeBound covers the 256 KiB canonical size: exactly at the bound
// passes, one byte more fails; whitespace does not count.
func TestSizeBound(t *testing.T) {
	doc := func(n int) string {
		// {"const":"<n bytes>"} is 12 + n bytes canonical.
		return `{"const":"` + strings.Repeat("x", n-12) + `"}`
	}
	if issues := compileIssues(t, doc(MaxSchemaBytes)); len(issues) > 0 {
		t.Errorf("schema of exactly %d bytes: %+v", MaxSchemaBytes, issues)
	}
	issues := compileIssues(t, doc(MaxSchemaBytes+1))
	if len(issues) != 1 || issues[0].Pointer != "" || !strings.Contains(issues[0].Message, "larger than 262144 bytes") {
		t.Errorf("schema of %d bytes: %+v", MaxSchemaBytes+1, issues)
	}
	padded := `{"const"  :  "` + strings.Repeat("x", MaxSchemaBytes-12) + `"` + strings.Repeat(" ", 1000) + `}`
	if issues := compileIssues(t, padded); len(issues) > 0 {
		t.Errorf("whitespace counted toward the canonical size: %+v", issues)
	}
	if canonicalWithin(map[string]any{"a": json.Number("1e400")}, 10) {
		t.Error("canonicalWithin ignored the limit for a number a double cannot hold")
	}
	if !canonicalWithin(map[string]any{"a": json.Number("1e400")}, 100) {
		t.Error("canonicalWithin refused a verbatim number within the limit")
	}
}

// TestSubschemaBound covers MaxSubschemas (proposed): positions include
// the root, and references into unknown keywords count too.
func TestSubschemaBound(t *testing.T) {
	props := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"properties":{`)
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%x":true`, i)
		}
		b.WriteString(`}}`)
		return b.String()
	}
	if issues := compileIssues(t, props(MaxSubschemas-1)); len(issues) > 0 {
		t.Errorf("%d subschemas: %+v", MaxSubschemas, issues)
	}
	for _, schema := range []string{
		props(MaxSubschemas),
		`{"$ref":"#/hidden","hidden":` + props(MaxSubschemas-1) + `}`,
	} {
		issues := compileIssues(t, schema)
		if len(issues) != 1 || !strings.Contains(issues[0].Message, "more than 4096 subschemas") {
			t.Errorf("issues = %+v, want the subschema bound", issues)
		}
	}
}

// TestIssueCap checks that at most MaxIssues findings are reported, in
// document order.
func TestIssueCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"properties":{`)
	for i := range 30 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"p%02d":{"pattern":"(?=x)"}`, i)
	}
	b.WriteString(`}}`)
	issues := compileIssues(t, b.String())
	if len(issues) != MaxIssues || issues[0].Pointer != "/properties/p00/pattern" || issues[MaxIssues-1].Pointer != "/properties/p19/pattern" {
		t.Fatalf("issues = %+v", issues)
	}
	_, err := CompileSchema([]byte(b.String()))
	if want := fmt.Sprintf("(and %d more)", MaxIssues-1); err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("error = %v, want suffix %q", err, want)
	}
	if got := (&CompileError{}).Error(); got != ErrSchema.Error() {
		t.Errorf("empty CompileError = %q", got)
	}
}

// TestMetaIssuesDeterministic checks that meta-schema findings do not
// depend on the library's map iteration order.
func TestMetaIssuesDeterministic(t *testing.T) {
	const schema = `{"properties":{"a":{"type":5},"b":{"minLength":"x"},"c":{"required":"y"},"d":{"enum":3}}}`
	first := compileIssues(t, schema)
	if len(first) != 4 {
		t.Fatalf("issues = %+v, want 4", first)
	}
	for range 30 {
		if got := compileIssues(t, schema); !slices.Equal(got, first) {
			t.Fatalf("issues changed between runs:\n%+v\n%+v", got, first)
		}
	}
}

// TestSchemaRawAndSum checks the normalized document and its digest:
// formatting and member order do not change the identity, a literal
// does, and Raw returns a copy.
func TestSchemaRawAndSum(t *testing.T) {
	a := mustCompile(t, `{"type":"object", "properties": {"b": {}, "a": {"maximum": 12345678901234567890}}}`)
	b := mustCompile(t, "{\n  \"properties\": {\"a\": {\"maximum\": 12345678901234567890}, \"b\": {}},\n  \"type\": \"object\"\n}")
	c := mustCompile(t, `{"type":"object","properties":{"b":{},"a":{"maximum":12345678901234567891}}}`)
	if a.Sum() != b.Sum() {
		t.Error("equal documents have different sums")
	}
	if a.Sum() == c.Sum() {
		t.Error("documents that differ beyond double precision share a sum")
	}
	want := `{"properties":{"a":{"maximum":12345678901234567890},"b":{}},"type":"object"}`
	raw := a.Raw()
	if string(raw) != want {
		t.Errorf("Raw() = %s, want %s", raw, want)
	}
	raw[0] = 'X'
	if !bytes.Equal(a.Raw(), []byte(want)) {
		t.Error("Raw returned the Schema's own bytes")
	}
	if !a.ComparesNumbers() || mustCompile(t, `{"type":"string"}`).ComparesNumbers() {
		t.Error("ComparesNumbers does not follow the numeric keywords")
	}
}

// TestComparesNumbers pins the one source of the document-wide flag that
// applies the number bounds where the scan cannot follow a body path:
// every keyword through which the library parses a body number, wherever
// its schema is; a type list without integer and keywords that parse
// only schema numbers do not set it.
func TestComparesNumbers(t *testing.T) {
	for _, tc := range []struct {
		schema string
		want   bool
	}{
		{`{"const":"a"}`, true},
		{`{"enum":["a"]}`, true},
		{`{"uniqueItems":true}`, true},
		{`{"type":["string","integer"]}`, true},
		{`{"not":{"multipleOf":2}}`, true},
		{`{"$defs":{"unused":{"exclusiveMinimum":0}}}`, true},
		{`{"items":{"maximum":1}}`, true},
		{`{"type":["string","number"]}`, false},
		{`{"minLength":1,"maxItems":2,"uniqueItems":false}`, false},
	} {
		if got := mustCompile(t, tc.schema).ComparesNumbers(); got != tc.want {
			t.Errorf("ComparesNumbers(%s) = %v, want %v", tc.schema, got, tc.want)
		}
	}
}

// TestInspectorBackstop skips the walk to prove the compiled-graph check
// still refuses a bundled meta-schema and a non-2020-12 resource, and
// checks that it refuses a format or content keyword compiled as an
// assertion, each at its keyword, at every schema it visits (the compile
// keeps them annotations): a compiled schema gets the field injected, as
// the library sets it under a dialect with the format-assertion
// vocabulary or of a draft before 2019-09, or under AssertContent. The
// format message names no one cause: naming the vocabulary would be
// wrong for a draft-07 resource.
func TestInspectorBackstop(t *testing.T) {
	for _, tc := range []struct{ schema, ptr, msg string }{
		{`{"properties":{"a":{"$ref":"https://json-schema.org/draft/2020-12/schema"}}}`, "/properties/a/$ref", "outside the schema document"},
		// The library compiles the bundled meta-schema a $recursiveRef names
		// too.
		{`{"properties":{"a":{"$recursiveRef":"https://json-schema.org/draft/2020-12/schema"}}}`, "/properties/a/$recursiveRef", "outside the schema document"},
		{`{"$ref":"#/x","x":{"$id":"urn:y","$schema":"http://json-schema.org/draft-07/schema#"}}`, "/x", "not JSON Schema draft 2020-12"},
	} {
		p, issues := prepare([]byte(tc.schema))
		if len(issues) > 0 {
			t.Fatal(issues)
		}
		w := newWalker(p.doc)
		w.walk()
		if len(w.issues) == 0 {
			t.Fatalf("the walk passed %s", tc.schema)
		}
		w.issues = nil
		s, issues, _ := compileWalked(p, w, refusingLoader{}, nil, newRegexpEngine())
		if s != nil || len(issues) == 0 || issues[0].Pointer != tc.ptr || !strings.Contains(issues[0].Message, tc.msg) {
			t.Errorf("compile(%s) = %v, %+v, want a finding at %s", tc.schema, s, issues, tc.ptr)
		}
	}

	const (
		formatMsg  = "format is an annotation, but the library compiled it as an assertion under this schema's dialect; $schema must be https://json-schema.org/draft/2020-12/schema"
		contentMsg = " is an annotation, but the library compiled it as an assertion"
	)
	format := func(s *sjs.Schema) { s.Format = &sjs.Format{Name: "email", Validate: func(any) error { return nil }} }
	encoding := func(s *sjs.Schema) { s.ContentEncoding = &sjs.Decoder{Name: "base64"} }
	mediaType := func(s *sjs.Schema) { s.ContentMediaType = &sjs.MediaType{Name: "application/json"} }
	root := func(s *Schema) *sjs.Schema { return s.root }
	member := func(s *Schema) *sjs.Schema { return s.root.Properties["a"] }
	target := func(s *Schema) *sjs.Schema { return s.root.Ref }
	for _, tc := range []struct {
		name   string
		at     func(*Schema) *sjs.Schema
		inject func(*sjs.Schema)
		want   []Issue
	}{
		{"format at the root", root, format, []Issue{{"/format", formatMsg}}},
		{"format in a member", member, format, []Issue{{"/properties/a/format", formatMsg}}},
		{"format behind a reference", target, format, []Issue{{"/$defs/d/format", formatMsg}}},
		{"contentEncoding", root, encoding, []Issue{{"/contentEncoding", "contentEncoding" + contentMsg}}},
		{"contentMediaType in a member", member, mediaType, []Issue{{"/properties/a/contentMediaType", "contentMediaType" + contentMsg}}},
		{"contentSchema", root, func(s *sjs.Schema) { s.ContentSchema = s.Properties["a"] }, []Issue{{"/contentSchema", "contentSchema" + contentMsg}}},
		{
			"all four", member, func(s *sjs.Schema) { format(s); encoding(s); mediaType(s); s.ContentSchema = s },
			[]Issue{
				{"/properties/a/format", formatMsg},
				{"/properties/a/contentEncoding", "contentEncoding" + contentMsg},
				{"/properties/a/contentMediaType", "contentMediaType" + contentMsg},
				{"/properties/a/contentSchema", "contentSchema" + contentMsg},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustCompile(t, `{"$ref":"#/$defs/d","$defs":{"d":{"type":"string"}},"properties":{"a":{"type":"string"}}}`)
			in := newInspector()
			if in.visit(s.root, ""); len(in.issues) > 0 {
				t.Fatalf("before the injection: %+v", in.issues)
			}
			tc.inject(tc.at(s))
			in = newInspector()
			in.visit(s.root, "")
			if !slices.Equal(in.issues, tc.want) {
				t.Errorf("issues = %+v, want %+v", in.issues, tc.want)
			}
		})
	}
}

// TestMessage checks that findings are one line without the compile URL,
// cut at a rune boundary.
func TestMessage(t *testing.T) {
	if got := message("a\n b\t\tc " + baseURL + "#/x \"" + baseURL + "\""); got != `a b c #/x "#"` {
		t.Errorf("message = %q", got)
	}
	long := strings.Repeat("é", maxMessageBytes)
	got := message(long)
	if !strings.HasSuffix(got, "...") || len(got) > maxMessageBytes+3 || !utf8.ValidString(got) {
		t.Errorf("message(long) = %q (%d bytes)", got, len(got))
	}
}
