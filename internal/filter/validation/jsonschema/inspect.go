// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"
)

// edge is one subschema a compiled schema applies.
type edge struct {
	// kw is the keyword; sub the member name or index under it, if any.
	kw, sub string
	// factor is the branch count the keyword multiplies the fan-out by:
	// the size of allOf, anyOf and oneOf, the number of if, then and else
	// members present, and 1 for every other keyword.
	factor int
	child  *sjs.Schema
}

// edges lists the subschemas s applies, in a fixed order (map members
// sorted), so the inspection is deterministic.
func edges(s *sjs.Schema) []edge {
	var out []edge
	one := func(kw string, c *sjs.Schema) {
		if c != nil {
			out = append(out, edge{kw: kw, factor: 1, child: c})
		}
	}
	list := func(kw string, cs []*sjs.Schema, factor int) {
		for i, c := range cs {
			if c != nil {
				out = append(out, edge{kw: kw, sub: strconv.Itoa(i), factor: factor, child: c})
			}
		}
	}
	keyed := func(kw string, m map[string]*sjs.Schema) {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			out = append(out, edge{kw: kw, sub: k, factor: 1, child: m[k]})
		}
	}
	one("$ref", s.Ref)
	if s.DynamicRef != nil {
		one("$dynamicRef", s.DynamicRef.Ref)
	}
	one("$recursiveRef", s.RecursiveRef)
	one("not", s.Not)
	list("allOf", s.AllOf, len(s.AllOf))
	list("anyOf", s.AnyOf, len(s.AnyOf))
	list("oneOf", s.OneOf, len(s.OneOf))
	cond := 0
	for _, c := range []*sjs.Schema{s.If, s.Then, s.Else} {
		if c != nil {
			cond++
		}
	}
	for _, c := range []struct {
		kw string
		s  *sjs.Schema
	}{{"if", s.If}, {"then", s.Then}, {"else", s.Else}} {
		if c.s != nil {
			out = append(out, edge{kw: c.kw, factor: cond, child: c.s})
		}
	}
	keyed("properties", s.Properties)
	pats := make([]sjs.Regexp, 0, len(s.PatternProperties))
	for r := range s.PatternProperties {
		pats = append(pats, r)
	}
	slices.SortFunc(pats, func(a, b sjs.Regexp) int { return cmp.Compare(a.String(), b.String()) })
	for _, r := range pats {
		out = append(out, edge{kw: "patternProperties", sub: r.String(), factor: 1, child: s.PatternProperties[r]})
	}
	if c, ok := s.AdditionalProperties.(*sjs.Schema); ok {
		one("additionalProperties", c)
	}
	one("propertyNames", s.PropertyNames)
	deps := map[string]*sjs.Schema{}
	for k, v := range s.Dependencies {
		if c, ok := v.(*sjs.Schema); ok {
			deps[k] = c
		}
	}
	keyed("dependencies", deps)
	keyed("dependentSchemas", s.DependentSchemas)
	one("unevaluatedProperties", s.UnevaluatedProperties)
	one("contains", s.Contains)
	switch it := s.Items.(type) {
	case *sjs.Schema:
		one("items", it)
	case []*sjs.Schema:
		list("items", it, 1)
	}
	if c, ok := s.AdditionalItems.(*sjs.Schema); ok {
		one("additionalItems", c)
	}
	list("prefixItems", s.PrefixItems, 1)
	one("items", s.Items2020)
	one("unevaluatedItems", s.UnevaluatedItems)
	one("contentSchema", s.ContentSchema)
	return out
}

// inspector checks the compiled graph: every schema reached lies in the
// document and is draft 2020-12, and the nested applicator fan-out stays
// within MaxFanOut. It also notes whether any schema compares numbers.
type inspector struct {
	state   map[*sjs.Schema]uint8
	fan     map[*sjs.Schema]int
	issues  []Issue
	numbers bool
	// crossed is set once the fan-out finding is recorded.
	crossed bool
}

func newInspector() *inspector {
	return &inspector{state: map[*sjs.Schema]uint8{}, fan: map[*sjs.Schema]int{}}
}

func (in *inspector) add(ptr, msg string) {
	if len(in.issues) < MaxIssues {
		in.issues = append(in.issues, Issue{Pointer: ptr, Message: message(msg)})
	}
}

// visit returns the fan-out of s, at most MaxFanOut+1. from is the pointer
// of the member that applies s, where a schema outside the document is
// reported. A schema already on the visit path counts 1: each $ref cycle is
// counted once, as 07 req 75 defines the fan-out; what a cycle repeats at
// each level of body nesting is the evaluation bound's (work.go).
func (in *inspector) visit(s *sjs.Schema, from string) int {
	switch in.state[s] {
	case 1:
		return 1
	case 2:
		return in.fan[s]
	}
	ptr, ok := pointerOf(s.Location)
	if !ok {
		in.add(from, "reference resolves outside the schema document")
		in.state[s], in.fan[s] = 2, 1
		return 1
	}
	if s.DraftVersion != 2020 {
		in.add(ptr, "schema is not JSON Schema draft 2020-12; $schema must be "+Draft)
	}
	in.noteNumbers(s)
	in.state[s] = 1
	best, bestKw, childMax := 1, "", 1
	for _, e := range edges(s) {
		at := appendToken(ptr, e.kw)
		if e.sub != "" {
			at = appendToken(at, e.sub)
		}
		f := in.visit(e.child, at)
		childMax = max(childMax, f)
		if prod := min(e.factor*f, MaxFanOut+1); prod > best {
			best, bestKw = prod, e.kw
		}
	}
	if best > MaxFanOut && childMax <= MaxFanOut && !in.crossed {
		in.crossed = true
		in.add(appendToken(ptr, bestKw), fmt.Sprintf("nested applicators fan out to more than %d branches (the product of allOf, anyOf, oneOf and if/then/else branch counts along one schema path)", MaxFanOut))
	}
	in.state[s], in.fan[s] = 2, best
	return best
}

// noteNumbers sets numbers when validating against s parses instance
// numbers.
func (in *inspector) noteNumbers(s *sjs.Schema) {
	if s.Minimum != nil || s.Maximum != nil || s.ExclusiveMinimum != nil || s.ExclusiveMaximum != nil ||
		s.MultipleOf != nil || s.Const != nil || s.Enum != nil || s.UniqueItems {
		in.numbers = true
		return
	}
	if s.Types != nil && slices.Contains(s.Types.ToStrings(), "integer") {
		in.numbers = true
	}
}
