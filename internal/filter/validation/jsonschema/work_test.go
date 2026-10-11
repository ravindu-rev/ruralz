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
// and member keywords or shared definitions, is RZ-CFG-005 at the
// applicator; recursive schemas whose work stays linear compile.
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
		{"dependentSchemas sharing a definition", chain(13, `{"dependentSchemas":{"a":{"$ref":"#/$defs/d%d"},"b":{"$ref":"#/$defs/d%d"}}}`), "/dependentSchemas"},
		{"$ref and not sharing a definition", chain(13, `{"$ref":"#/$defs/d%d","not":{"not":{"$ref":"#/$defs/d%d"}}}`), "/not"},
		{"if and then both recursing", ifAndThen, "/if"},
		{"then and else recursing twice", `{"if":true,"then":{"$ref":"#/$defs/two"},"else":{"$ref":"#/$defs/two"},"$defs":{"two":{"allOf":[{"items":{"$ref":"#"}},{"items":{"$ref":"#"}}]}}}`, "/allOf"},

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
				return
			}
			if len(issues) != 1 || !strings.HasSuffix(issues[0].Pointer, tc.ptr) || !strings.Contains(issues[0].Message, "more than 4096 subschema evaluations") {
				t.Fatalf("issues = %+v, want one evaluation finding at %s", issues, tc.ptr)
			}
			_, err := Factory{}.Build(t.Context(), filter.BuildEnv{Config: configOf(tc.schema)})
			if code, _ := errcode.CodeOf(err); code != CodeConfig {
				t.Errorf("Build = %v, want %s", err, CodeConfig)
			}
		})
	}
}

// TestEvaluationBoundBudget covers the precise pass's budget: recursive
// mixins need the precise pass, and with its budget spent the coarse bound
// decides and refuses them.
func TestEvaluationBoundBudget(t *testing.T) {
	g, ok := newEvalGraph([]*sjs.Schema{libraryCompile(t, mixins)})
	if !ok {
		t.Fatal("graph outside the document")
	}
	order, issues := g.inPlaceOrder()
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	if _, issues := g.evaluations(order, coupledBudget); len(issues) > 0 {
		t.Errorf("with the budget: %+v, want none", issues)
	}
	_, issues = g.evaluations(order, 1)
	if len(issues) != 1 || issues[0].Pointer != "/allOf" {
		t.Errorf("without the budget: %+v, want the coarse finding at /allOf", issues)
	}
}

// TestRecursiveBodies covers the review regression of the evaluation
// bound with bodies nested 30 deep: the refused shapes never build, and
// an accepted recursive schema validates a body nested 30 or 60 deep with
// work linear in the nesting (allocations, not time).
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

// TestEvaluationBoundMatchesLibrary checks the bound against the library
// itself, compiled without it: the shapes it refuses take work that
// doubles with each level of nesting, the shapes it accepts work linear
// in the nesting.
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
	s, err := c.Compile(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	return s
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
	s := &Schema{root: libraryCompile(t, `{"$defs":{"x":{"$ref":"#/$defs/y"},"y":{"$ref":"#/$defs/x"}},"$ref":"#/$defs/x"}`)}
	f := newFilter(s, 400, testLimit, 0)
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
