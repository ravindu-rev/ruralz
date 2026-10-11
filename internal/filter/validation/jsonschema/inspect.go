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
	id      map[*sjs.Schema]int
	nodes   []fanNode
	issues  []Issue
	numbers bool
	// steps bounds the exact fan-out of each component (fanOutSteps).
	steps int
}

// fanNode is one compiled schema of the inspection; edges is empty for a
// schema outside the document, which is reported and not followed.
type fanNode struct {
	ptr   string
	edges []fanEdge
}

// fanEdge is one subschema a schema applies, with the branch count its
// keyword multiplies the fan-out by.
type fanEdge struct {
	kw     string
	factor int
	to     int
}

// fanOutSteps bounds the steps the exact fan-out may take for the schemas
// of one strongly connected component of the graph (a group of schemas
// that reach each other through references); past it an upper bound of
// the component decides (see fanOut).
const fanOutSteps = 1 << 16

func newInspector() *inspector {
	return &inspector{id: map[*sjs.Schema]int{}, steps: fanOutSteps}
}

func (in *inspector) add(ptr, msg string) {
	if len(in.issues) < MaxIssues {
		in.issues = append(in.issues, Issue{Pointer: ptr, Message: message(msg)})
	}
}

// visit indexes s and every schema it reaches, depth first, checking each
// once. from is the pointer of the member that applies s, where a schema
// outside the document is reported.
func (in *inspector) visit(s *sjs.Schema, from string) int {
	if i, ok := in.id[s]; ok {
		return i
	}
	i := len(in.nodes)
	in.id[s] = i
	in.nodes = append(in.nodes, fanNode{})
	ptr, ok := pointerOf(s.Location)
	if !ok {
		in.add(from, "reference resolves outside the schema document")
		return i
	}
	in.nodes[i].ptr = ptr
	if s.DraftVersion != 2020 {
		in.add(ptr, "schema is not JSON Schema draft 2020-12; $schema must be "+Draft)
	}
	in.noteNumbers(s)
	var out []fanEdge
	for _, e := range edges(s) {
		at := appendToken(ptr, e.kw)
		if e.sub != "" {
			at = appendToken(at, e.sub)
		}
		out = append(out, fanEdge{kw: e.kw, factor: e.factor, to: in.visit(e.child, at)})
	}
	in.nodes[i].edges = out
	return i
}

// fanOut applies MaxFanOut to every schema visited. The fan-out of a
// schema is the largest product of the branch factors along a schema path
// from it that repeats no schema: a path that reaches a schema already on
// it ends there, so each $ref cycle counts once (07 req 75). The products
// are computed exactly, over the strongly connected components of the
// graph from the leaves up, so they do not depend on the order of members
// or of visits: within a component every path is followed from every
// schema of it, and a schema outside it contributes its own fan-out. A
// component whose paths take more than fanOutSteps steps gets an upper
// bound instead, the product of the largest factor of each of its schemas
// times the largest fan-out leaving it, which can only refuse more. Of the
// schemas where a product first exceeds MaxFanOut, the finding names the
// one whose pointer sorts first.
func (in *inspector) fanOut() {
	fan := make([]int, len(in.nodes))
	comp := in.components()
	var at string
	cross := func(v int, kw string) {
		p := appendToken(in.nodes[v].ptr, kw)
		if at == "" || comparePointers(p, at) < 0 {
			at = p
		}
	}
	for _, c := range comp.order {
		if len(c) == 1 && !in.selfLoop(c[0]) {
			v := c[0]
			best, kw, childMax := in.product(v, func(e fanEdge) int { return fan[e.to] })
			if best > MaxFanOut && childMax <= MaxFanOut {
				cross(v, kw)
			}
			fan[v] = best
			continue
		}
		in.componentFanOut(c, comp.of, fan, cross)
	}
	if at != "" {
		in.add(at, fmt.Sprintf("nested applicators fan out to more than %d branches (the product of allOf, anyOf, oneOf and if/then/else branch counts along one schema path)", MaxFanOut))
	}
}

// product returns the fan-out of node v given the fan-out each edge leads
// to, the keyword of its largest product, and the largest fan-out among
// its edges.
func (in *inspector) product(v int, sub func(fanEdge) int) (best int, kw string, childMax int) {
	best, childMax = 1, 1
	for _, e := range in.nodes[v].edges {
		f := sub(e)
		childMax = max(childMax, f)
		if p := min(e.factor*f, MaxFanOut+1); p > best {
			best, kw = p, e.kw
		}
	}
	return best, kw, childMax
}

// selfLoop reports whether node v applies itself.
func (in *inspector) selfLoop(v int) bool {
	for _, e := range in.nodes[v].edges {
		if e.to == v {
			return true
		}
	}
	return false
}

// componentFanOut sets the fan-out of every schema of component c (of maps
// each node to its component): every path that repeats no schema is
// followed from each of them, the schemas outside c contributing their
// fan-out; past fanOutSteps steps for the whole component, its upper bound
// decides for all of them.
func (in *inspector) componentFanOut(c []int, of []int, fan []int, cross func(int, string)) {
	self := of[c[0]]
	steps := 0
	onPath := make(map[int]bool, len(c))
	type crossing struct {
		v  int
		kw string
	}
	var found []crossing
	var walk func(v int) int
	walk = func(v int) int {
		best, kw, childMax := in.product(v, func(e fanEdge) int {
			steps++
			switch {
			case steps > in.steps:
				return 1
			case onPath[e.to]:
				return 1
			case of[e.to] == self:
				onPath[e.to] = true
				f := walk(e.to)
				delete(onPath, e.to)
				return f
			default:
				return fan[e.to]
			}
		})
		if best > MaxFanOut && childMax <= MaxFanOut {
			found = append(found, crossing{v, kw})
		}
		return best
	}
	values := make([]int, len(c))
	for i, v := range c {
		onPath[v] = true
		values[i] = walk(v)
		delete(onPath, v)
	}
	if steps <= in.steps {
		for i, v := range c {
			fan[v] = values[i]
		}
		for _, f := range found {
			cross(f.v, f.kw)
		}
		return
	}
	// The upper bound: a path visits each schema of c at most once, taking
	// one of its edges, and may leave c once at its end.
	bound, exit := 1, 1
	first, firstKw := -1, ""
	for _, v := range c {
		factor, kw := 1, ""
		for _, e := range in.nodes[v].edges {
			if e.factor > factor {
				factor, kw = e.factor, e.kw
			}
			if of[e.to] != self {
				exit = max(exit, fan[e.to])
			}
		}
		bound = min(bound*factor, MaxFanOut+1)
		if first < 0 || comparePointers(in.nodes[v].ptr, in.nodes[first].ptr) < 0 {
			first, firstKw = v, kw
		}
	}
	bound = min(bound*exit, MaxFanOut+1)
	for _, v := range c {
		fan[v] = bound
	}
	if bound > MaxFanOut {
		cross(first, firstKw)
	}
}

// sccs is the strongly connected components of the inspected graph: order
// lists them so that every component comes after the components it
// reaches; of maps each node to its component's index in order.
type sccs struct {
	order [][]int
	of    []int
}

// components returns the strongly connected components of the graph
// (Tarjan's algorithm).
func (in *inspector) components() sccs {
	n := len(in.nodes)
	index, low := make([]int, n), make([]int, n)
	onStack := make([]bool, n)
	for i := range index {
		index[i] = -1
	}
	out := sccs{of: make([]int, n)}
	var stack []int
	next := 0
	var connect func(v int)
	connect = func(v int) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, e := range in.nodes[v].edges {
			switch {
			case index[e.to] < 0:
				connect(e.to)
				low[v] = min(low[v], low[e.to])
			case onStack[e.to]:
				low[v] = min(low[v], index[e.to])
			}
		}
		if low[v] != index[v] {
			return
		}
		var c []int
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			out.of[w] = len(out.order)
			c = append(c, w)
			if w == v {
				break
			}
		}
		slices.Sort(c)
		out.order = append(out.order, c)
	}
	for v := range n {
		if index[v] < 0 {
			connect(v)
		}
	}
	return out
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
