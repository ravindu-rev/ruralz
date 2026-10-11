// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// The schema shapes of the evaluation bound: a recursive reference reached
// through two paths per level of nesting (refused), and recursive schemas
// whose work stays linear in the body (accepted).
const (
	oneOfTwice  = `{"oneOf":[{"items":{"$ref":"#"}},{"items":{"$ref":"#"}}]}`
	allOfTwice  = `{"allOf":[{"items":{"$ref":"#"}},{"items":{"$ref":"#"}}]}`
	anyOfFailed = `{"anyOf":[{"items":{"$ref":"#"},"minItems":2},{"items":{"$ref":"#"}}]}`
	linearTree  = `{"oneOf":[{"type":"string"},{"type":"array","items":{"$ref":"#"}}]}`
	linearAnyOf = `{"anyOf":[{"type":"null"},{"items":{"$ref":"#"}}]}`
	mixins      = `{"$defs":{"base":{"properties":{"owner":{"$ref":"#/$defs/person"}}},` +
		`"person":{"properties":{"pets":{"items":{"$ref":"#"}}}}},` +
		`"allOf":[{"$ref":"#/$defs/base"},{"properties":{"friends":{"items":{"$ref":"#"}}}}]}`
	// taggedUnion recurses through then and else, of which the library
	// evaluates one (07 req 75 review: linear, accepted).
	taggedUnion = `{"if":{"properties":{"kind":{"const":"a"}}},` +
		`"then":{"properties":{"children":{"items":{"$ref":"#"}}}},` +
		`"else":{"properties":{"children":{"items":{"$ref":"#"}}}}}`
	// ifAndThen recurses through if and then, which both apply.
	ifAndThen = `{"if":{"items":{"$ref":"#"}},"then":{"items":{"$ref":"#"}}}`
	// mixinDefs opens a schema with the definitions of mixins; mixinAllOf
	// applies them, so the coarse pass crosses and the precise one decides.
	mixinDefs  = `{"$defs":{"base":{"properties":{"owner":{"$ref":"#/$defs/person"}}},"person":{"properties":{"pets":{"items":{"$ref":"#"}}}}}`
	mixinAllOf = `{"allOf":[{"$ref":"#/$defs/base"},{"properties":{"friends":{"items":{"$ref":"#"}}}}]}`
)

// operators returns a tagged union of n operators written as a nested
// if/else chain, each recursing through args, the last one in the final
// else.
func operators(n int) string {
	branch := `{"properties":{"args":{"items":{"$ref":"#"}}}}`
	s := branch
	for i := n - 1; i >= 1; i-- {
		s = fmt.Sprintf(`{"if":{"properties":{"op":{"const":"op%d"}}},"then":%s,"else":%s}`, i, branch, s)
	}
	return s
}

// children returns depth objects nested through "children" arrays,
// alternating the kind so both then and else apply.
func children(depth int) string {
	var b strings.Builder
	for i := range depth {
		fmt.Fprintf(&b, `{"kind":"%c","children":[`, 'a'+i%2)
	}
	b.WriteString(`{}`)
	b.WriteString(strings.Repeat(`]}`, depth))
	return b.String()
}

// chain returns $defs d0 to d<n-1> where each applies the next through
// link (with %d for the next index), the last being true, and a root
// referring to d0.
func chain(n int, link string) string {
	var b strings.Builder
	b.WriteString(`{"$ref":"#/$defs/d0","$defs":{`)
	for i := range n {
		fmt.Fprintf(&b, `"d%d":`+link+`,`, i, i+1, i+1)
	}
	fmt.Fprintf(&b, `"d%d":true}}`, n)
	return b.String()
}

// nested returns depth empty arrays nested in each other.
func nested(depth int) string {
	return strings.Repeat("[", depth) + strings.Repeat("]", depth)
}

// friends returns depth objects nested through "friends" arrays, a body
// that recurses through mixins.
func friends(depth int) string {
	return strings.Repeat(`{"friends":[`, depth) + `{}` + strings.Repeat(`]}`, depth)
}

// bodyOf returns the body that recurses through schema, depth levels deep.
func bodyOf(schema string, depth int) string {
	switch schema {
	case mixins:
		return friends(depth / 2)
	case taggedUnion:
		return children(depth / 2)
	}
	return nested(depth)
}

// TestEvaluationBound covers the evaluation bound (proposed, 07 req 75
// and section 9 item 21): a recursive reference reached through more than
// one path per level of nesting, through applicators, overlapping item
// and member keywords or shared definitions, crosses MaxEvaluations within
// MinNesting levels and is RZ-CFG-005 at the applicator where it crosses;
// recursive schemas whose work stays linear compile with no nesting limit.
func TestEvaluationBound(t *testing.T) {
	var props, pats strings.Builder
	for i := range 129 {
		fmt.Fprintf(&props, `"p%d":true,`, i)
		fmt.Fprintf(&pats, `"^q%d":true,`, i)
	}
	manyPatterns := `{"properties":{` + strings.TrimSuffix(props.String(), ",") + `},"patternProperties":{` + strings.TrimSuffix(pats.String(), ",") + `}}`
	for _, tc := range []struct {
		name, schema, ptr string
	}{
		{"oneOf twice", oneOfTwice, "/oneOf"},
		{"allOf twice", allOfTwice, "/allOf"},
		{"anyOf whose first branch fails after recursing", anyOfFailed, "/anyOf"},
		{"items and contains", `{"items":{"$ref":"#"},"contains":{"$ref":"#"}}`, "/contains"},
		{"overlapping patterns", `{"patternProperties":{"^a":{"$ref":"#"},"b$":{"$ref":"#"}}}`, "/patternProperties"},
		{"property and matching pattern", `{"properties":{"ab":{"$ref":"#"}},"patternProperties":{"^a":{"$ref":"#"}}}`, "/properties"},
		{"two definitions in one cycle", `{"$defs":{"a":{"items":{"$ref":"#"}}},"allOf":[{"$ref":"#/$defs/a"},{"$ref":"#/$defs/a"}]}`, "/allOf"},
		{"allOf branches sharing a recursive mixin", `{"$defs":{"m":{"properties":{"next":{"$ref":"#"}}}},"oneOf":[{"allOf":[{"$ref":"#/$defs/m"}]},{"allOf":[{"$ref":"#/$defs/m"}]}]}`, "/oneOf"},
		{"dependentSchemas sharing a definition", chain(13, `{"dependentSchemas":{"a":{"$ref":"#/$defs/d%d"},"b":{"$ref":"#/$defs/d%d"}}}`), "/$defs/d2/dependentSchemas"},
		{"$ref and not sharing a definition", chain(13, `{"$ref":"#/$defs/d%d","not":{"not":{"$ref":"#/$defs/d%d"}}}`), "/$defs/d2/not"},
		{"if and then both recursing", ifAndThen, "/if"},
		{"then and else recursing twice", `{"if":true,"then":{"$ref":"#/$defs/two"},"else":{"$ref":"#/$defs/two"},"$defs":{"two":{"allOf":[{"items":{"$ref":"#"}},{"items":{"$ref":"#"}}]}}}`, "/$defs/two/allOf"},

		{"linear tree", linearTree, ""},
		{"anyOf with one recursive branch", linearAnyOf, ""},
		{"two recursive members", `{"properties":{"a":{"$ref":"#"},"b":{"$ref":"#"}}}`, ""},
		{"recursive mixins by member name", mixins, ""},
		{"allOf of recursive members", `{"allOf":[{"properties":{"a":{"$ref":"#"}}},{"properties":{"b":{"$ref":"#"}}}]}`, ""},
		{"allOf of recursive tuples", `{"allOf":[{"prefixItems":[{"$ref":"#"}]},{"prefixItems":[true,{"$ref":"#"}]}]}`, ""},
		{"pattern or additional", `{"patternProperties":{"^x-":{"$ref":"#"}},"additionalProperties":{"$ref":"#"}}`, ""},
		{"evaluated members skip unevaluatedProperties", `{"properties":{"a":{"$ref":"#"}},"unevaluatedProperties":{"$ref":"#"}}`, ""},
		{"items skip unevaluatedItems", `{"items":{"$ref":"#"},"unevaluatedItems":{"$ref":"#"}}`, ""},
		{"contains without recursion", `{"items":{"$ref":"#"},"contains":{"type":"string"},"propertyNames":{"$ref":"#/$defs/n"},"$defs":{"n":{"anyOf":[{"maxLength":3},{"pattern":"^a"}]}}}`, ""},
		{"two recursive cycles in a row", `{"$defs":{"s":{"items":{"$ref":"#/$defs/s"}}},"items":{"$ref":"#"},"contains":{"$ref":"#/$defs/s"}}`, ""},
		{"dynamic reference", `{"$dynamicAnchor":"n","items":{"$dynamicRef":"#n"},"$defs":{"x":{"$id":"urn:x","$dynamicAnchor":"n","items":{"$dynamicRef":"#n"}}}}`, ""},
		{"properties against many patterns", manyPatterns, ""},
		{"tagged union through then and else", taggedUnion, ""},
		{"operators as an if/else chain", operators(7), ""},
		{"else without then", `{"if":{"type":"string"},"else":{"items":{"$ref":"#"}}}`, ""},
		{"mixins under then and else", mixinDefs + `,"if":{"required":["k"]},` +
			`"then":{"allOf":[{"$ref":"#/$defs/base"},{"properties":{"friends":{"items":{"$ref":"#"}}}}]},` +
			`"else":{"allOf":[{"$ref":"#/$defs/base"},{"properties":{"others":{"items":{"$ref":"#"}}}}]}}`, ""},
		{"mixins under then only", mixinDefs + `,"if":{"required":["k"]},"then":` + mixinAllOf + `,"else":{"minLength":1}}`, ""},
		{"mixins under else only", mixinDefs + `,"if":{"required":["k"]},"then":{"minLength":1},"else":` + mixinAllOf + `}`, ""},
		{"mixins with a dynamic reference", `{"$dynamicAnchor":"n","properties":{"kids":{"items":{"$dynamicRef":"#n"}}},` +
			`"$defs":{"x":{"$id":"urn:x","$dynamicAnchor":"n","items":{"$dynamicRef":"#n"}},` +
			`"base":{"properties":{"owner":{"$ref":"#/$defs/person"}}},"person":{"properties":{"pets":{"items":{"$ref":"#"}}}}},` +
			`"allOf":` + strings.TrimSuffix(strings.TrimPrefix(mixinAllOf, `{"allOf":`), `}`) + `}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := compileIssues(t, tc.schema)
			if tc.ptr == "" {
				if len(issues) > 0 {
					t.Fatalf("issues = %+v, want none", issues)
				}
				if limit := mustCompile(t, tc.schema).work.limit; limit != maxNesting+1 {
					t.Errorf("nesting limit %d, want none", limit)
				}
				return
			}
			if len(issues) != 1 || issues[0].Pointer != tc.ptr || !strings.Contains(issues[0].Message, "more than 4096 subschema evaluations") {
				t.Fatalf("issues = %+v, want one evaluation finding at %s", issues, tc.ptr)
			}
			_, err := Factory{}.Build(t.Context(), filter.BuildEnv{Config: configOf(tc.schema)})
			if code, _ := errcode.CodeOf(err); code != CodeConfig {
				t.Errorf("Build = %v, want %s", err, CodeConfig)
			}
		})
	}
}

// richText is a ProseMirror-style document schema (review finding): a
// block is one of four kinds, and a bullet list's items and a blockquote
// hold blocks again, so the work per value grows with the nesting along a
// Fibonacci curve, crossing MaxEvaluations at depth 28.
const richText = `{"type":"object","properties":{"type":{"const":"doc"},"content":{"type":"array","items":{"$ref":"#/$defs/block"}}},` +
	`"$defs":{"block":{"oneOf":[{"$ref":"#/$defs/paragraph"},{"$ref":"#/$defs/heading"},{"$ref":"#/$defs/bulletList"},{"$ref":"#/$defs/blockquote"}]},` +
	`"paragraph":{"type":"object","required":["type"],"properties":{"type":{"const":"paragraph"},"content":{"type":"array","items":{"$ref":"#/$defs/inline"}}}},` +
	`"heading":{"type":"object","required":["type"],"properties":{"type":{"const":"heading"},"attrs":{"type":"object","properties":{"level":{"type":"integer","minimum":1,"maximum":6}}},"content":{"type":"array","items":{"$ref":"#/$defs/inline"}}}},` +
	`"bulletList":{"type":"object","required":["type"],"properties":{"type":{"const":"bulletList"},"content":{"type":"array","items":{"$ref":"#/$defs/listItem"}}}},` +
	`"listItem":{"type":"object","required":["type"],"properties":{"type":{"const":"listItem"},"content":{"type":"array","items":{"$ref":"#/$defs/block"}}}},` +
	`"blockquote":{"type":"object","required":["type"],"properties":{"type":{"const":"blockquote"},"content":{"type":"array","items":{"$ref":"#/$defs/block"}}}},` +
	`"inline":{"type":"object","required":["type"],"properties":{"type":{"enum":["text","hardBreak"]},"text":{"type":"string"},"marks":{"type":"array","items":{"type":"object","properties":{"type":{"enum":["bold","italic","link"]}}}}}}}}`

// componentTree is a UI component tree (review finding): a node is text, a
// row or a column, and rows and columns hold nodes; its nesting limit is
// 20.
const componentTree = `{"$ref":"#/$defs/node","$defs":{"node":{"oneOf":[{"$ref":"#/$defs/text"},{"$ref":"#/$defs/row"},{"$ref":"#/$defs/column"}]},` +
	`"text":{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}},` +
	`"row":{"type":"object","required":["children"],"properties":{"kind":{"const":"row"},"children":{"type":"array","items":{"$ref":"#/$defs/node"}}}},` +
	`"column":{"type":"object","required":["children"],"properties":{"kind":{"const":"column"},"children":{"type":"array","items":{"$ref":"#/$defs/node"}}}}}}`

// quotes returns a rich-text document of n blockquotes nested in each
// other around a paragraph: its deepest value, the text, is 2n+5 deep.
func quotes(n int) string {
	return `{"type":"doc","content":[` + strings.Repeat(`{"type":"blockquote","content":[`, n) +
		`{"type":"paragraph","content":[{"type":"text","text":"x"}]}` + strings.Repeat(`]}`, n) + `]}`
}

// TestNestingLimit is the regression test of the review finding that the
// evaluation bound refused realistic recursive schemas (07 req 75 makes
// only the fan-out a compile rule): a schema whose evaluations cross
// MaxEvaluations at a depth of MinNesting or more compiles, with that depth
// as its nesting limit, and Handle refuses a body that reaches it as
// filter.ErrTooLarge (413) before the library runs, so the work per value
// stays bounded; a body nested less deep validates.
func TestNestingLimit(t *testing.T) {
	for _, tc := range []struct {
		schema string
		limit  int
	}{{richText, 28}, {componentTree, 20}} {
		f := buildFilter(t, tc.schema, 0)
		if got := f.schema.work.limit; got != tc.limit {
			t.Errorf("nesting limit %d, want %d", got, tc.limit)
		}
	}
	f := buildFilter(t, richText, 0)
	if r := handle(t, f, quotes(11)); r.Outcome != filter.Continue {
		t.Errorf("text 27 deep: %+v, want Continue", r)
	}
	if got := detailOf(t, handle(t, f, strings.Replace(quotes(11), `"text","text"`, `"txt","text"`, 1))); !strings.HasPrefix(got, DetailMismatch) {
		t.Errorf("a failing body 27 deep: %q, want a mismatch", got)
	}
	x, _ := exchange("application/json", []byte(quotes(12)))
	px := &peakExchange{Exchange: x}
	r := f.Handle(t.Context(), phase.OnRequestBody, px)
	if r.Outcome != filter.CannotDecide || !errors.Is(r.Err, errNestingTooDeep) || !errors.Is(r.Err, filter.ErrTooLarge) {
		t.Errorf("text 29 deep: %+v, want CannotDecide with the nesting ErrTooLarge", r)
	}
	if px.peak != 0 {
		t.Errorf("a refused body reserved %d", px.peak)
	}
	// A number out of range is still the deterministic 400, wherever it is.
	if got := detailOf(t, handle(t, f, strings.Replace(quotes(12), `"x"`, `"x","attrs":{"level":1e1001}`, 1))); got != DetailNumber {
		t.Errorf("a deep body with a number out of range: %q, want %q", got, DetailNumber)
	}
	if loc, ok := f.schema.Validate(decodeInstance(t, quotes(12))); ok || loc != "" {
		t.Errorf("Validate past the nesting limit = %q, %v, want not ok at the root", loc, ok)
	}
	// Minor review finding: a value exactly at the limit, where one value
	// may take more than MaxEvaluations evaluations, is refused; the
	// deepest value here is the 1 in place of a block 28 deep.
	deepest := `{"type":"doc","content":[` + strings.Repeat(`{"type":"blockquote","content":[`, 13) + `1` + strings.Repeat(`]}`, 13) + `]}`
	if r := handle(t, f, deepest); !errors.Is(r.Err, errNestingTooDeep) {
		t.Errorf("a block 28 deep: %+v, want CannotDecide with the nesting ErrTooLarge", r)
	}
	// Minor review finding: past the limit, a value whose own path the
	// states follow within MaxEvaluations validates, such as a free-form
	// member no subschema evaluates; the recursion still cannot go deeper.
	withData := strings.Replace(componentTree, `"properties":{"kind":{"const":"row"},`, `"properties":{"data":{},"tree":{"$ref":"#/$defs/tree"},"kind":{"const":"row"},`, 1)
	withData = strings.Replace(withData, `"$defs":{`, `"$defs":{"tree":{"items":{"$ref":"#/$defs/tree"}},`, 1)
	f = buildFilter(t, withData, 0)
	if f.schema.work.limit != 20 {
		t.Fatalf("nesting limit %d, want 20", f.schema.work.limit)
	}
	data := strings.Repeat(`{"a":`, 30) + `[1,"x",null]` + strings.Repeat(`}`, 30)
	if r := handle(t, f, `{"kind":"row","data":`+data+`,"children":[{"text":"x"}]}`); r.Outcome != filter.Continue {
		t.Errorf("free-form data 32 deep: %+v, want Continue", r)
	}
	// Past the limit only the states bound a value: a linear recursion 40
	// deep validates, and every level is charged its evaluation.
	tree := `{"kind":"row","tree":` + nested(40) + `,"children":[{"text":"x"}]}`
	if r := handle(t, f, tree); r.Outcome != filter.Continue {
		t.Errorf("a tree 41 deep: %+v, want Continue", r)
	}
	_, deep := scanBody(t, f.schema, tree)
	_, shallow := scanBody(t, f.schema, strings.Replace(tree, nested(40), nested(30), 1))
	if deep.work-shallow.work < 10*evalUnits {
		t.Errorf("a tree 41 deep is charged %d work units, one 31 deep %d: want at least the 10 evaluations between", deep.work, shallow.work)
	}
	rows := strings.Repeat(`{"kind":"row","children":[`, 11) + `{"text":"x"}` + strings.Repeat(`]}`, 11)
	if r := handle(t, f, rows); !errors.Is(r.Err, errNestingTooDeep) {
		t.Errorf("rows 23 deep: %+v, want CannotDecide with the nesting ErrTooLarge", r)
	}
}

// TestMinNesting pins the boundary of MinNesting (proposed, 07 req 75
// review): a recursion that doubles every two levels of nesting crosses
// MaxEvaluations at depth 16 and compiles with that nesting limit; one
// level deeper in the body, at 15, it is RZ-CFG-005 (minor review
// finding).
func TestMinNesting(t *testing.T) {
	doubling := func(ref string, extra int) string {
		inner := `{"items":{"items":{"$ref":"` + ref + `"}}}`
		return `{"oneOf":[` + inner + `,` + inner + strings.Repeat(`,true`, extra) + `]}`
	}
	if s := mustCompile(t, doubling("#", 13)); s.work.limit != MinNesting {
		t.Errorf("nesting limit %d, want MinNesting", s.work.limit)
	}
	issues := compileIssues(t, `{"items":{"$ref":"#/$defs/d"},"$defs":{"d":`+doubling("#/$defs/d", 29)+`}}`)
	if len(issues) != 1 || !strings.Contains(issues[0].Message, "nested 15 deep") {
		t.Errorf("issues = %+v, want the evaluation finding at depth 15", issues)
	}
}

// TestEvaluationBoundBudget covers the precise pass's budget (07 req 75
// review, nesting limit): recursive mixins need the precise pass, which
// finds no nesting limit; with its budget spent the coarse bound decides,
// which limits the nesting of mixins and refuses mixins recursing through
// two members per level.
func TestEvaluationBoundBudget(t *testing.T) {
	analysis := func(schema string, budget int) (*workBound, []Issue) {
		t.Helper()
		g, ok := newEvalGraph([]*sjs.Schema{libraryCompile(t, schema)})
		if !ok {
			t.Fatal("graph outside the document")
		}
		order, issues := g.inPlaceOrder()
		if len(issues) > 0 {
			t.Fatal(issues)
		}
		return g.evaluations(order, budget)
	}
	if wb, issues := analysis(mixins, coupledBudget); len(issues) > 0 || wb.limit != maxNesting+1 {
		t.Errorf("mixins with the budget: %+v, %+v, want no limit", wb, issues)
	}
	if wb, issues := analysis(mixins, 1); len(issues) > 0 || wb.limit != 26 {
		t.Errorf("mixins without the budget: %+v, %+v, want the coarse limit 26", wb, issues)
	}
	twoMembers := `{"allOf":[{"properties":{"a":{"$ref":"#"}}},{"properties":{"b":{"$ref":"#"}}}]}`
	if wb, issues := analysis(twoMembers, coupledBudget); len(issues) > 0 || wb.limit != maxNesting+1 {
		t.Errorf("two members with the budget: %+v, %+v, want no limit", wb, issues)
	}
	if _, issues := analysis(twoMembers, 1); len(issues) != 1 || issues[0].Pointer != "/allOf" {
		t.Errorf("two members without the budget: %+v, want the coarse finding at /allOf", issues)
	}
}

// TestRecursiveBodies covers the review regression of the evaluation
// bound (proposed, 07 req 75 and section 9 item 21) with bodies nested 30
// deep: the refused shapes never build, and an accepted recursive schema
// validates a body nested 30 or 60 deep with work linear in the nesting
// (allocations, not time).
func TestRecursiveBodies(t *testing.T) {
	for _, schema := range []string{oneOfTwice, allOfTwice, anyOfFailed} {
		if _, err := (Factory{}).Build(t.Context(), filter.BuildEnv{Config: configOf(schema)}); !errors.Is(err, ErrSchema) {
			t.Errorf("Build(%s) = %v, want a refused schema", schema, err)
		}
	}
	for _, schema := range []string{linearTree, linearAnyOf, mixins, taggedUnion} {
		f := buildFilter(t, schema, testLimit)
		for _, depth := range []int{30, 60} {
			if r := handle(t, f, bodyOf(schema, depth)); r.Outcome != filter.Continue {
				t.Errorf("schema %s, body nested %d deep: %+v", schema, depth, r)
			}
		}
		if ratio := allocRatio(t, f.schema.root, schema, 30); ratio > 3 {
			t.Errorf("schema %s: twice the nesting allocates %.1fx, want linear", schema, ratio)
		}
	}
}

// allocRatio returns the allocations of validating the body of schema
// nested 2*depth deep over one nested depth deep.
func allocRatio(t *testing.T, s *sjs.Schema, schema string, depth int) float64 {
	t.Helper()
	count := func(d int) float64 {
		v := decodeInstance(t, bodyOf(schema, d))
		return testing.AllocsPerRun(3, func() { _ = s.Validate(v) })
	}
	return count(2*depth) / count(depth)
}

// TestEvaluationBoundMatchesLibrary checks the evaluation bound
// (proposed, 07 section 9 item 21) against the library itself, compiled
// without it: the shapes it refuses take work that doubles with each level
// of nesting, the shapes it accepts work linear in the nesting.
func TestEvaluationBoundMatchesLibrary(t *testing.T) {
	for _, tc := range []struct {
		schema  string
		refused bool
	}{
		{oneOfTwice, true},
		{allOfTwice, true},
		{anyOfFailed, true},
		{`{"items":{"$ref":"#"},"contains":{"$ref":"#"}}`, true},
		{ifAndThen, true},
		{linearTree, false},
		{linearAnyOf, false},
		{mixins, false},
		{taggedUnion, false},
	} {
		root := libraryCompile(t, tc.schema)
		ratio := allocRatio(t, root, tc.schema, 6)
		if tc.refused && ratio < 16 {
			t.Errorf("refused schema %s: twice the nesting allocates only %.1fx", tc.schema, ratio)
		}
		if !tc.refused && (ratio > 3 || ratio < 1.5) {
			t.Errorf("accepted schema %s: twice the nesting allocates %.1fx", tc.schema, ratio)
		}
	}
}

// libraryCompile compiles schema with the library alone, without the
// rules of 07 req 75 or the evaluation bound.
func libraryCompile(t testing.TB, schema string) *sjs.Schema {
	t.Helper()
	return librarySchema(t, schema).root
}

// librarySchema compiles schema with the library alone into a Schema with
// both passes whose model bounds nothing: no nesting limit, no counts.
func librarySchema(t testing.TB, schema string) *Schema {
	t.Helper()
	doc, err := sjs.UnmarshalJSON(strings.NewReader(schema))
	if err != nil {
		t.Fatal(err)
	}
	c := sjs.NewCompiler()
	c.DefaultDraft(sjs.Draft2020)
	c.UseLoader(refusingLoader{})
	if err := c.AddResource(baseURL, doc); err != nil {
		t.Fatal(err)
	}
	if err := c.AddResource(decideURL, map[string]any{"not": map[string]any{"$ref": baseURL}}); err != nil {
		t.Fatal(err)
	}
	root, err := c.Compile(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	decide, err := c.Compile(decideURL)
	if err != nil {
		t.Fatal(err)
	}
	return &Schema{root: root, decide: decide, work: workingSet{limit: maxNesting + 1}}
}

// TestInPlaceCycles covers 07 req 13 at compile time: a reference cycle
// that never descends into the value would make every value reaching it
// an internal validator error, so it is RZ-CFG-005 at the reference.
func TestInPlaceCycles(t *testing.T) {
	for _, tc := range []struct{ name, schema, ptr string }{
		{"root", `{"$ref":"#"}`, "/$ref"},
		{"two definitions", `{"$defs":{"x":{"$ref":"#/$defs/y"},"y":{"$ref":"#/$defs/x"}},"$ref":"#/$defs/x"}`, "/$defs/x/$ref"},
		{"through anyOf", `{"$defs":{"t":{"anyOf":[{"type":"null"},{"items":{"$ref":"#/$defs/t"}},{"$ref":"#/$defs/t"}]}},"$ref":"#/$defs/t"}`, "/$defs/t/anyOf/2/$ref"},
		{"through then", `{"if":true,"then":{"$ref":"#"}}`, "/then/$ref"},
		{"through dependentSchemas", `{"dependentSchemas":{"a":{"$ref":"#"}}}`, "/dependentSchemas/a/$ref"},
		{"unreferenced definition", `{"$defs":{"x":{"not":{"$ref":"#/$defs/x"}}}}`, "/$defs/x/not/$ref"},
		{"dynamic reference", `{"$dynamicAnchor":"n","$dynamicRef":"#n"}`, "/$dynamicRef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := compileIssues(t, tc.schema)
			if len(issues) != 1 || issues[0].Pointer != tc.ptr || !strings.Contains(issues[0].Message, "reference cycle") {
				t.Fatalf("issues = %+v, want one reference cycle at %s", issues, tc.ptr)
			}
		})
	}
}

// TestValidatorErrorUndecided covers the backstop of 07 req 13: a schema
// that reaches a reference cycle anyway (compiled here without the rules)
// is cannot decide, never a 400 decision.
func TestValidatorErrorUndecided(t *testing.T) {
	s := librarySchema(t, `{"$defs":{"x":{"$ref":"#/$defs/y"},"y":{"$ref":"#/$defs/x"}},"$ref":"#/$defs/x"}`)
	f := newFilter(s, 400, bodyLimits{decoded: testLimit})
	x, _ := exchange("application/json", []byte(`{"a":1}`))
	r := f.Handle(t.Context(), phase.OnRequestBody, x)
	if r.Outcome != filter.CannotDecide || r.Code != "" || !errors.Is(r.Err, errValidator) {
		t.Fatalf("Handle = %+v, want CannotDecide with the validator error", r)
	}
	if strings.Contains(r.Err.Error(), `"a"`) {
		t.Errorf("error %q quotes the body", r.Err)
	}
	if x.Budget.Reserved != 0 {
		t.Errorf("reservation left: %d", x.Budget.Reserved)
	}
	if loc, ok := s.Validate(map[string]any{}); ok || loc != "" {
		t.Errorf("Validate = %q, %v, want not ok at the root", loc, ok)
	}
}

// TestLegacyItems covers the draft-07 items array and additionalItems,
// which the library sets only below draft 2020-12: CompileSchema refuses
// such a resource (07 req 75), yet the inspection walks it and the graph
// maps it, an item by index to items and past them to additionalItems, so
// both stay sound should a document reach them.
func TestLegacyItems(t *testing.T) {
	const schema = `{"$schema":"http://json-schema.org/draft-07/schema#","items":[{"type":"string"},{"type":"null"}],"additionalItems":{"type":"integer"}}`
	root := libraryCompile(t, schema)
	in := newInspector()
	in.visit(root, "")
	var kws []string
	for _, e := range in.nodes[0].edges {
		kws = append(kws, e.kw)
	}
	if len(in.issues) != 4 || in.issues[0].Pointer != "" || strings.Join(kws, ",") != "items,items,additionalItems" {
		t.Errorf("inspection: issues %+v, edges %v, want a draft finding per schema and both item keywords", in.issues, kws)
	}
	g, ok := newEvalGraph([]*sjs.Schema{root})
	if !ok {
		t.Fatal("graph outside the document")
	}
	n := &g.nodes[0]
	if len(n.prefix) != 2 || n.items < 0 || g.nodes[n.items].ptr != "/additionalItems" {
		t.Errorf("graph: prefix %v, items %d, want two prefix items and additionalItems", n.prefix, n.items)
	}
	if issues := compileIssues(t, schema); len(issues) == 0 {
		t.Error("a draft-07 document compiled")
	}
}
