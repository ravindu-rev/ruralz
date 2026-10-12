// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// The evaluation bound (architecture R-77, package documentation). The
// library validates without memoizing: every subschema an applicator, a
// reference or a member and item keyword reaches is evaluated once per
// path that reaches it, so a recursive reference reached through two paths
// doubles the work at every level of body nesting. The compiled graph is
// checked for two things before a schema is accepted:
//
//   - no reference cycle applies schemas to the same value without
//     descending into it (the library answers every value that reaches one
//     with an internal error, kind.RefCycle);
//   - at any one value of a body, validation evaluates at most
//     MaxEvaluations subschemas: the first nesting depth at which the
//     count may exceed it is the schema's nesting limit, which must be at
//     least MinNesting, and a body value at or below it is refused at
//     runtime unless the state of its path bounds it (states.go).
//
// The count is an upper bound: every allOf, anyOf and oneOf branch
// applies, if applies with the costlier of then and else (the library
// evaluates exactly one of them), a dependentSchemas or dependencies
// member applies, a $dynamicRef applies its costliest possible target, an
// object member or array item gets every subschema that can apply to it
// (by member name for properties, by index for prefixItems), and
// uniqueItems counts one evaluation at every value below the array, which
// it hashes.
//
// The same analysis carries a vector of counts: besides the evaluations,
// each schema adds its weights in the other dimensions (error units, work
// units, number parses, scan work per byte, unevaluated copies, hashing
// passes, uniqueItems checks), and the runtime turns the bound at every
// depth into the memory it reserves and the work it allows (working.go),
// together with the counts of each body value's own path (states.go).

// maxNesting is the deepest a body nests: the decoder's default depth,
// under which the Filter decodes.
const maxNesting = jsonval.DefaultMaxDepth

// evalCap is the value at which evaluation counts saturate.
const evalCap = MaxEvaluations + 1

// countCap is the value at which the other counts saturate; a count that
// reaches it refuses every body that reaches the value. The counts are
// ints that hold it and its products with evaluation counts, so the
// package builds for 64-bit platforms only, the only ones Ruralz ships
// (ADR-0001): elsewhere countCap overflows int and the build fails (minor
// review finding).
const countCap = 1 << 40

// coupledBudget bounds the class entries the precise pass may touch, so a
// document built to defeat it still compiles in bounded time; past it the
// coarse bound decides.
const coupledBudget = 1 << 19

// maxPatternMatches bounds the properties-name and patternProperties
// pairs matched for one schema, which keep their matches; past it every
// pattern is assumed to match every name.
const maxPatternMatches = 1 << 14

// matchBudget bounds the work, in work units (matchWork), of matching
// member names against patterns at compile time, for the properties of a
// schema against its own patternProperties (newGNode) and for the names
// one state declares against the patterns of the others (memberEntries),
// all of one document together: about 34 ms of the regexp matchers at
// most (review finding: a pair count did not bound the work, which grows
// with the name's length times the pattern's run queue). Past it every
// pattern is assumed to match, which can only charge more.
const matchBudget = 1 << 25

// matchWork returns the work units of matching a name of n bytes against
// patterns whose match costs sum to m (gNode.match): the setup of each,
// and its work per byte.
func matchWork(m vec, n int) int {
	if m[dimScan] > 0 && n > (countCap-m[dimWork])/m[dimScan] {
		return countCap
	}
	return min(m[dimWork]+m[dimScan]*n, countCap)
}

// The dimensions of a count vector.
const (
	// dimEvals counts subschema evaluations; MaxEvaluations applies to it.
	dimEvals = iota
	// dimErrs counts error units (errorWeight): the memory the errors of
	// a failing evaluation may hold.
	dimErrs
	// dimWork counts the work units of an evaluation that do not grow
	// with the value (workWeight), and of matching a member's name against
	// the patternProperties of the schemas evaluated at its object.
	dimWork
	// dimParses counts how often the value's number is parsed and
	// compared (parsesOf).
	dimParses
	// dimScan counts work units per byte of the value's string and of its
	// member name: pattern matching and counting runes for minLength and
	// maxLength. Hashing for uniqueItems is counted per pass in dimHash.
	dimScan
	// dimUneval counts evaluations that may copy the unevaluated member
	// names or item indexes of the value (the library's unevaluated*
	// tracking).
	dimUneval
	// dimHash counts the uniqueItems passes that hash and compare the
	// value.
	dimHash
	// dimUnique counts the uniqueItems checks of the value itself, an
	// array the library hashes, or compares pairwise when it is short.
	dimUnique
	// dimObjects counts the objects of the const and enum values compared
	// with the value, at any depth (objectsIn): each compares with at most
	// one object of the value's, looking its member names up until one is
	// missing, which hashes that name.
	dimObjects
	// dims is the number of dimensions.
	dims
)

// vec is a count vector, indexed by dimension.
type vec [dims]int

// capOf returns the saturation value of dimension k.
func capOf(k int) int {
	if k == dimEvals {
		return evalCap
	}
	return countCap
}

// sat adds two count vectors, each dimension saturating.
func sat(a, b vec) vec {
	for k := range a {
		a[k] = min(a[k]+b[k], capOf(k))
	}
	return a
}

// larger returns the dimension-wise larger of two count vectors.
func larger(a, b vec) vec {
	for k := range a {
		a[k] = max(a[k], b[k])
	}
	return a
}

// isZero reports whether every dimension of v is zero.
func (v vec) isZero() bool { return v == vec{} }

// gEdge is one in-place member of a schema: a reference or applicator
// subschema evaluated against the same value. to holds its target, or for
// a $dynamicRef every schema the dynamic scope may resolve it to, of which
// one applies.
type gEdge struct {
	kw, sub string
	to      []int
	// alt marks else: it and the then member before it are alternatives,
	// of which the library evaluates one.
	alt bool
}

// gProp is one properties member and the patternProperties targets of the
// same schema whose pattern matches its name.
type gProp struct {
	name string
	to   int
	pats []int
}

// gNode is one compiled schema; member and item targets are -1 when
// absent.
type gNode struct {
	ptr   string
	in    []gEdge
	props []gProp
	// pats are the patternProperties targets and res their patterns,
	// ordered by pattern.
	pats []int
	res  []sjs.Regexp
	// allPats is set when properties times patterns is over
	// maxPatternMatches, or matching them over what is left of
	// matchBudget: every pattern is then assumed to match every properties
	// member, whose pats stay empty.
	allPats bool
	// match is what matching a member name against every pattern costs:
	// fixed work units and work units per byte of the name.
	match vec
	// valuePats is what may match the value's string, its pattern, and
	// namePats what may match a member's name, its patternProperties.
	valuePats, namePats patternReach
	// addl is the additionalProperties target; false is the synthetic
	// one-value schema, which charges the name the library lists.
	addl, uneProps, propNames int
	prefix                    []int
	items, contains, uneItems int
	// unique is the synthetic every-value schema when uniqueItems is set.
	unique int
	// units are the error units of one evaluation by JSON kind
	// (errorUnits).
	units units
	// compares is set when the schema has a const or enum value holding
	// arrays or objects.
	compares bool
	// keepsMatches is set when the schema has contains with minContains or
	// maxContains, whose error keeps the indexes of the matching items.
	keepsMatches bool
	// divNum and divDen are the digits of the numerator and the
	// denominator of the multipleOf divisor (ratDigits), 0 without one.
	divNum, divDen int
}

// evalGraph is the compiled graph of a document; nodes[0] is the root
// the Filter validates with, and the last two nodes are synthetic.
type evalGraph struct {
	nodes []gNode
	// weight is what one evaluation of each schema adds to each dimension.
	weight []vec
	flags  workFlags
	// one and every are the synthetic schemas: one value (the name
	// additionalProperties false lists) and every value below a uniqueItems
	// array (hashing).
	one, every int
	// matches is what newGNode left of matchBudget, for the states.
	matches int
	// chain is, for every schema, the longest chain of in-place
	// evaluations from it, itself included (inPlaceOrder).
	chain []int
}

// workFlags records document-wide facts the runtime model needs.
type workFlags struct {
	// names is set when some schema has propertyNames, so member names
	// are values of their own.
	names bool
	// rats is set when some schema has a numeric keyword, whose error
	// keeps the instance number as a math/big rational.
	rats bool
	// uneProps and uneItems are set when some schema has
	// unevaluatedProperties or unevaluatedItems, so evaluations counted
	// in dimUneval copy member names or item indexes.
	uneProps, uneItems bool
	// dynamic is set when some schema has $dynamicRef or $recursiveRef,
	// whose resolution walks the whole dynamic scope.
	dynamic bool
	// lists is set when some schema has additionalProperties false, which
	// lists the names it does not declare.
	lists bool
	// contains is set when some schema has contains, which lists the
	// indexes of the items that match it.
	contains bool
	// patterns is what may match a string or member name whose path the
	// scan does not know: every pattern of the document (patternReach).
	patterns patternReach
	// divNum and divDen are the most decimal digits of the numerator and
	// of the denominator of any multipleOf divisor (ratDigits), 0 without
	// one, for the checks of a number whose path the scan does not know.
	divNum, divDen int
	// maxPointer bounds the bytes of the location URL and keyword of any
	// error the location pass reports, which its walk reads (walkWork).
	maxPointer int
	// chain is the longest chain of in-place evaluations of the document,
	// which bounds the call stack at any one value (0 when unknown).
	chain int
}

// workBound is what the analysis allows at one value of a body, by the
// value's depth below the body root: at[d] holds the counts of every
// dimension, with at[d][dimEvals] at most MaxEvaluations for every depth
// below limit, the schema's nesting limit (maxNesting+1 when no depth
// crosses MaxEvaluations).
type workBound struct {
	at    [maxNesting + 1]vec
	flags workFlags
	limit int
}

// analyze applies the evaluation bound to the graph reachable from starts,
// starts[0] being the root, and returns the runtime model: the bound at
// every depth, the nesting limit and the states of the body paths. Every
// schema must lie in the document (the inspector reports one that does not
// first); when one does not, the model refuses every body, without
// findings.
func analyze(starts []*sjs.Schema) (workingSet, []Issue) {
	g, ok := newEvalGraph(starts)
	if !ok {
		return newWorkingSet(nil, nil), nil
	}
	order, issues := g.inPlaceOrder()
	if len(issues) > 0 {
		return workingSet{}, issues
	}
	wb, issues := g.evaluations(order, coupledBudget)
	if len(issues) > 0 {
		return workingSet{}, issues
	}
	return newWorkingSet(wb, g.buildStates(order, stateBudget)), nil
}

// newEvalGraph indexes every schema reachable from starts in breadth-first
// order, then appends the synthetic schemas; ok is false when one lies
// outside the document.
func newEvalGraph(starts []*sjs.Schema) (*evalGraph, bool) {
	id := map[*sjs.Schema]int{}
	var all []*sjs.Schema
	add := func(s *sjs.Schema) {
		if _, seen := id[s]; !seen {
			id[s] = len(all)
			all = append(all, s)
		}
	}
	for _, s := range starts {
		add(s)
	}
	anchors := map[string][]int{}
	for i := 0; i < len(all); i++ {
		s := all[i]
		if s.DynamicAnchor != "" {
			anchors[s.DynamicAnchor] = append(anchors[s.DynamicAnchor], i)
		}
		for _, e := range edges(s) {
			add(e.child)
		}
	}
	// one counts one evaluation at a value; every counts one at a value
	// and at each value below it.
	one, every := len(all), len(all)+1
	g := &evalGraph{nodes: make([]gNode, len(all)+2), weight: make([]vec, len(all)+2), one: one, every: every, matches: matchBudget}
	pc := newPatternCosts()
	for i, s := range all {
		ptr, ok := pointerOf(s.Location)
		if !ok {
			return nil, false
		}
		g.nodes[i] = newGNode(s, ptr, id, anchors, one, every, pc, &g.matches)
		g.weight[i] = vec{dimEvals: 1, dimErrs: errorWeight(s), dimWork: workWeight(s, pc), dimParses: parsesOf(s), dimScan: scanWeight(s, pc), dimObjects: comparedObjects(s)}
		if s.UniqueItems {
			g.weight[i][dimUnique] = 1
		}
		f := &g.flags
		f.names = f.names || s.PropertyNames != nil
		f.rats = f.rats || s.Minimum != nil || s.Maximum != nil || s.ExclusiveMinimum != nil || s.ExclusiveMaximum != nil || s.MultipleOf != nil
		f.uneProps = f.uneProps || s.UnevaluatedProperties != nil
		f.uneItems = f.uneItems || s.UnevaluatedItems != nil
		f.dynamic = f.dynamic || s.DynamicRef != nil || s.RecursiveRef != nil
		f.lists = f.lists || s.AdditionalProperties == false
		f.contains = f.contains || s.Contains != nil
		f.maxPointer = max(f.maxPointer, len(s.Location)+maxKeywordBytes+2*longestKey(s.Dependencies)+2*longestKey(s.DependentRequired))
		if s.MultipleOf != nil {
			c, d := ratDigits(s.MultipleOf)
			g.nodes[i].divNum, g.nodes[i].divDen = c, d
			f.divNum, f.divDen = max(f.divNum, c), max(f.divDen, d)
		}
	}
	g.flags.patterns = reachOf(slices.Collect(maps.Values(pc.cache))...)
	// The name additionalProperties false lists is held by the location
	// pass's error only; hashing holds no error.
	g.nodes[one] = gNode{addl: -1, uneProps: -1, propNames: -1, items: -1, contains: -1, uneItems: -1, unique: -1}
	for k := range kinds {
		g.nodes[one].units.names[k] = 1
	}
	g.nodes[every] = gNode{addl: every, uneProps: -1, propNames: -1, items: every, contains: -1, uneItems: -1, unique: -1}
	g.weight[one] = vec{dimEvals: 1, dimErrs: 1, dimWork: listUnits}
	// The hashing's work per byte is dimHash's (hashByteUnits), apart from
	// the patterns' in dimScan.
	g.weight[every] = vec{dimEvals: 1, dimWork: hashUnits, dimParses: 1, dimHash: 1}
	g.markUneval()
	return g, true
}

// maxKeywordBytes bounds the keyword names an error's keyword location
// adds to its schema's, with their slashes.
const maxKeywordBytes = 32

// longestKey returns the length of the longest key of m.
func longestKey[V any](m map[string]V) int {
	n := 0
	for k := range m {
		n = max(n, len(k))
	}
	return n
}

// markUneval weighs dimUneval for every schema that may copy the
// unevaluated names or indexes of its value: a schema with
// unevaluatedProperties or unevaluatedItems, and every schema it reaches
// in place, to which the library passes its copy on (callerNeeds).
func (g *evalGraph) markUneval() {
	var stack []int
	for v := range g.nodes {
		if g.nodes[v].uneProps >= 0 || g.nodes[v].uneItems >= 0 {
			stack = append(stack, v)
		}
	}
	for len(stack) > 0 {
		v := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if g.weight[v][dimUneval] == 1 {
			continue
		}
		g.weight[v][dimUneval] = 1
		for _, e := range g.nodes[v].in {
			stack = append(stack, e.to...)
		}
	}
}

// newGNode lists the in-place, member and item subschemas of s; one and
// every are the synthetic schemas, and matches is what is left of the
// document's matchBudget.
func newGNode(s *sjs.Schema, ptr string, id map[*sjs.Schema]int, anchors map[string][]int, one, every int, pc *patternCosts, matches *int) gNode {
	n := gNode{ptr: ptr, addl: -1, uneProps: -1, propNames: -1, items: -1, contains: -1, uneItems: -1, unique: -1}
	ref := func(c *sjs.Schema) int {
		if c == nil {
			return -1
		}
		return id[c]
	}
	in := func(kw, sub string, c *sjs.Schema, alt bool) {
		if c != nil {
			n.in = append(n.in, gEdge{kw: kw, sub: sub, to: []int{id[c]}, alt: alt})
		}
	}
	in("$ref", "", s.Ref, false)
	if d := s.DynamicRef; d != nil && d.Ref != nil {
		to := []int{id[d.Ref]}
		if d.Anchor != "" && d.Ref.DynamicAnchor == d.Anchor {
			for _, a := range anchors[d.Anchor] {
				if a != to[0] {
					to = append(to, a)
				}
			}
		}
		n.in = append(n.in, gEdge{kw: "$dynamicRef", to: to})
	}
	in("$recursiveRef", "", s.RecursiveRef, false)
	in("not", "", s.Not, false)
	for _, l := range []struct {
		kw string
		cs []*sjs.Schema
	}{{"allOf", s.AllOf}, {"anyOf", s.AnyOf}, {"oneOf", s.OneOf}} {
		for i, c := range l.cs {
			in(l.kw, strconv.Itoa(i), c, false)
		}
	}
	// The library compiles then and else only under if, and evaluates one
	// of them.
	in("if", "", s.If, false)
	in("then", "", s.Then, false)
	in("else", "", s.Else, s.Then != nil)
	for _, k := range slices.Sorted(maps.Keys(s.Dependencies)) {
		if c, ok := s.Dependencies[k].(*sjs.Schema); ok {
			in("dependencies", k, c, false)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(s.DependentSchemas)) {
		in("dependentSchemas", k, s.DependentSchemas[k], false)
	}

	n.units = errorUnits(s)
	n.compares = composite(s)
	pats := patternRegexps(s)
	n.res = pats
	costs := make([]patternCost, 0, len(pats))
	for _, r := range pats {
		n.pats = append(n.pats, id[s.PatternProperties[r]])
		c := pc.of(r.String())
		n.match = sat(n.match, vec{dimWork: c.fixed, dimScan: c.perByte})
		costs = append(costs, c)
	}
	if len(costs) > 0 {
		n.namePats = reachOf(costs...)
	}
	if s.Pattern != nil {
		n.valuePats = reachOf(pc.of(s.Pattern.String()))
	}
	n.allPats = len(s.Properties)*len(pats) > maxPatternMatches
	if !n.allPats && len(pats) > 0 {
		// Matching every name against every pattern, or none of it.
		w := 0
		for k := range s.Properties {
			w = min(w+matchWork(n.match, len(k)), countCap)
		}
		if w > *matches {
			n.allPats = true
		} else {
			*matches -= w
		}
	}
	for _, k := range slices.Sorted(maps.Keys(s.Properties)) {
		p := gProp{name: k, to: id[s.Properties[k]]}
		if !n.allPats {
			for i, r := range pats {
				if r.MatchString(k) {
					p.pats = append(p.pats, n.pats[i])
				}
			}
		}
		n.props = append(n.props, p)
	}
	switch a := s.AdditionalProperties.(type) {
	case *sjs.Schema:
		n.addl = id[a]
	case bool:
		if !a {
			n.addl = one
		}
	}
	n.uneProps = ref(s.UnevaluatedProperties)
	n.propNames = ref(s.PropertyNames)

	for _, c := range s.PrefixItems {
		n.prefix = append(n.prefix, id[c])
	}
	n.items = ref(s.Items2020)
	switch it := s.Items.(type) {
	case *sjs.Schema:
		n.items = id[it]
	case []*sjs.Schema:
		for _, c := range it {
			n.prefix = append(n.prefix, id[c])
		}
		if c, ok := s.AdditionalItems.(*sjs.Schema); ok {
			n.items = id[c]
		}
	}
	n.contains = ref(s.Contains)
	n.keepsMatches = s.Contains != nil && (s.MinContains != nil || s.MaxContains != nil)
	n.uneItems = ref(s.UnevaluatedItems)
	if s.UniqueItems {
		n.unique = every
	}
	return n
}

// edgePointer returns the pointer of in-place member e of node v.
func (g *evalGraph) edgePointer(v int, e gEdge) string {
	p := appendToken(g.nodes[v].ptr, e.kw)
	if e.sub != "" {
		p = appendToken(p, e.sub)
	}
	return p
}

// isReference reports whether kw is a reference keyword.
func isReference(kw string) bool {
	return kw == "$ref" || kw == "$dynamicRef" || kw == "$recursiveRef"
}

// inPlaceOrder returns the schemas ordered so that every in-place target
// comes before the schemas applying it, or one finding per in-place cycle,
// at the reference of the cycle whose pointer sorts first. Without a cycle
// it also records the chains of in-place evaluations (setChains).
func (g *evalGraph) inPlaceOrder() ([]int, []Issue) {
	type step struct{ from, edge int }
	state := make([]uint8, len(g.nodes))
	order := make([]int, 0, len(g.nodes))
	var (
		path   []step
		issues []Issue
		seen   = map[string]bool{}
	)
	var dfs func(v int)
	dfs = func(v int) {
		state[v] = 1
		for ei, e := range g.nodes[v].in {
			for _, w := range e.to {
				switch state[w] {
				case 0:
					path = append(path, step{v, ei})
					dfs(w)
					path = path[:len(path)-1]
				case 1:
					// The cycle runs from w, an ancestor on the path or v
					// itself, back to w through e.
					j := len(path)
					if w != v {
						for j > 0 && path[j-1].from != w {
							j--
						}
						j--
					}
					cycle := append(slices.Clone(path[j:]), step{v, ei})
					at, atRef := "", false
					for i, s := range cycle {
						e := g.nodes[s.from].in[s.edge]
						p, isRef := g.edgePointer(s.from, e), isReference(e.kw)
						if i == 0 || (isRef && !atRef) || (isRef == atRef && comparePointers(p, at) < 0) {
							at, atRef = p, isRef
						}
					}
					if !seen[at] && len(issues) < MaxIssues {
						seen[at] = true
						issues = append(issues, Issue{Pointer: at, Message: "reference cycle applies a schema to the same value again without descending into it, so the validator cannot decide any value that reaches it"})
					}
				}
			}
		}
		state[v] = 2
		order = append(order, v)
	}
	for v := range g.nodes {
		if state[v] == 0 {
			dfs(v)
		}
	}
	if len(issues) == 0 {
		g.setChains(order)
	}
	return order, issues
}

// setChains records, for every schema, the longest chain of in-place
// evaluations from it, itself included: the library evaluates the in-place
// members of a schema one after the other, each from a call of its own, so
// its call stack at one value holds one such chain, however many
// evaluations the value takes (review finding: every evaluation was
// charged a stack frame). In-place targets come first in order.
func (g *evalGraph) setChains(order []int) {
	g.chain = make([]int, len(g.nodes))
	for _, v := range order {
		c := 0
		for _, e := range g.nodes[v].in {
			for _, t := range e.to {
				c = max(c, g.chain[t])
			}
		}
		g.chain[v] = min(c+1, evalCap)
		g.flags.chain = max(g.flags.chain, g.chain[v])
	}
}

// pick returns the counts of in-place member e: its costliest target,
// dimension by dimension.
func pick(counts []vec, e gEdge) vec {
	var c vec
	for _, t := range e.to {
		c = larger(c, counts[t])
	}
	return c
}

// countOf returns counts[i], or zero for an absent target.
func countOf(counts []vec, i int) vec {
	if i < 0 {
		return vec{}
	}
	return counts[i]
}

// run is the analysis of one graph in one in-place order. rootIn marks
// the schemas the root applies in place, where a crossing of the root's
// count is named.
type run struct {
	g      *evalGraph
	order  []int
	rootIn []bool
	// names recycles the member maps of the precise pass's classes.
	names namesPool
}

// namesPool recycles the member maps of the precise pass's classes. The
// classes of one level are garbage once the next level is computed
// (preciseLevel keeps only their counts), so each level takes the maps of
// the one before, cleared, in the order it made them, which is the order
// of the schemas and so of their sizes (minor review finding: a
// worst-case compile allocated about 250 MB of them, and the collector's
// work slowed it).
type namesPool struct {
	spare, made []map[string]vec
	next        int
}

// get returns an empty map for about n names.
func (p *namesPool) get(n int) map[string]vec {
	var m map[string]vec
	if p.next < len(p.spare) {
		m = p.spare[p.next]
		p.spare[p.next] = nil
		p.next++
	} else {
		m = make(map[string]vec, n)
	}
	p.made = append(p.made, m)
	return m
}

// recycle clears the maps the last level made for the next one.
func (p *namesPool) recycle() {
	for _, m := range p.made {
		clear(m)
	}
	p.spare, p.made, p.next = p.made, p.spare[:0], 0
}

// inPlaceFrom marks node v and every schema it applies in place.
func (g *evalGraph) inPlaceFrom(v int) []bool {
	in := make([]bool, len(g.nodes))
	stack := []int{v}
	for len(stack) > 0 {
		u := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if in[u] {
			continue
		}
		in[u] = true
		for _, e := range g.nodes[u].in {
			stack = append(stack, e.to...)
		}
	}
	return in
}

// inPlace adds the counts of node v's in-place members to c: each
// member's, except that then and else, of which one applies, add the
// larger of the two.
func (r *run) inPlace(v int, c vec, counts []vec) vec {
	in := r.g.nodes[v].in
	for i := 0; i < len(in); i++ {
		n := pick(counts, in[i])
		if i+1 < len(in) && in[i+1].alt {
			i++
			n = larger(n, pick(counts, in[i]))
		}
		c = sat(c, n)
	}
	return c
}

// levelZero returns the counts at the value itself: each schema's own
// weights and its in-place members'.
func (r *run) levelZero() []vec {
	p := make([]vec, len(r.g.nodes))
	for _, v := range r.order {
		p[v] = r.inPlace(v, r.g.weight[v], p)
	}
	return p
}

// patterns returns what every patternProperties member of n adds to a
// name they all match.
func (r *run) patterns(n *gNode, prev []vec) vec {
	var c vec
	for _, t := range n.pats {
		c = sat(c, prev[t])
	}
	return c
}

// member returns the counts of properties member p of n: its schema and
// the patterns that match its name (all of them, pats, when n.allPats),
// plus the matching itself (match).
func (r *run) member(n *gNode, p *gProp, prev []vec, pats, match vec) vec {
	c := sat(prev[p.to], match)
	if n.allPats {
		return sat(c, pats)
	}
	for _, t := range p.pats {
		c = sat(c, prev[t])
	}
	return c
}

// otherName returns the counts of a member name no properties member
// names: it meets the patterns that match it, or else
// additionalProperties, or else unevaluatedProperties, plus the matching
// itself (match).
func (r *run) otherName(n *gNode, prev []vec, pats, match vec) vec {
	fallback := countOf(prev, n.addl)
	if n.addl < 0 {
		fallback = countOf(prev, n.uneProps)
	}
	return sat(larger(pats, fallback), match)
}

// matchCost returns what matching one member name against the patterns of
// n costs at level h: only at the members themselves (level 1).
func matchCost(n *gNode, h int) vec {
	if h != 1 {
		return vec{}
	}
	return n.match
}

// itemCounts returns the counts of an item past prefixItems, which meets
// items or else unevaluatedItems, and what every item meets besides:
// contains and the uniqueItems hashing.
func (r *run) itemCounts(n *gNode, prev []vec) (rest, every vec) {
	every = sat(countOf(prev, n.contains), countOf(prev, n.unique))
	rest = countOf(prev, n.items)
	if n.items < 0 {
		rest = countOf(prev, n.uneItems)
	}
	return sat(rest, every), every
}

// own returns the counts node v adds at level h through its member and
// item keywords, by class of child value (prev holds the counts of level
// h-1).
func (r *run) own(v int, prev []vec, h int) *classes {
	n := &r.g.nodes[v]
	pats, match := r.patterns(n, prev), matchCost(n, h)
	m := &classes{name: r.otherName(n, prev, pats, match)}
	if len(n.props) > 0 {
		m.names = r.names.get(len(n.props))
		for i := range n.props {
			m.names[n.props[i].name] = r.member(n, &n.props[i], prev, pats, match)
		}
	}
	rest, every := r.itemCounts(n, prev)
	m.index = rest
	for _, t := range n.prefix {
		m.idx = append(m.idx, sat(prev[t], every))
	}
	// A member name is a string value of its own: only level 1 reaches
	// it, and nothing nests below it.
	if h == 1 {
		m.key = countOf(prev, n.propNames)
	}
	m.setTop()
	return m
}

// ownTop returns own(v, prev, h).top without building the classes: the
// coarse pass needs only the costliest class, dimension by dimension, in
// O(members) time.
func (r *run) ownTop(v int, prev []vec, h int) vec {
	n := &r.g.nodes[v]
	pats, match := r.patterns(n, prev), matchCost(n, h)
	top := r.otherName(n, prev, pats, match)
	for i := range n.props {
		top = larger(top, r.member(n, &n.props[i], prev, pats, match))
	}
	rest, every := r.itemCounts(n, prev)
	top = larger(top, rest)
	for _, t := range n.prefix {
		top = larger(top, sat(prev[t], every))
	}
	if h == 1 {
		top = larger(top, countOf(prev, n.propNames))
	}
	return top
}

// contribution is one keyword's share of a schema's evaluation count, for
// naming the keyword a finding points at.
type contribution struct {
	kw string
	n  int
}

// ownContributions returns the member and item keywords of node v and the
// evaluations each adds at level h (prev holds the counts of level h-1).
func (r *run) ownContributions(v int, prev []vec, h int) []contribution {
	n := &r.g.nodes[v]
	var out []contribution
	add := func(kw string, c vec) {
		if c[dimEvals] > 0 {
			out = append(out, contribution{kw: kw, n: c[dimEvals]})
		}
	}
	var props vec
	for _, p := range n.props {
		props = larger(props, prev[p.to])
	}
	add("properties", props)
	add("patternProperties", r.patterns(n, prev))
	add("additionalProperties", countOf(prev, n.addl))
	if h == 1 {
		add("propertyNames", countOf(prev, n.propNames))
	}
	add("unevaluatedProperties", countOf(prev, n.uneProps))
	add("contains", countOf(prev, n.contains))
	add("uniqueItems", countOf(prev, n.unique))
	var prefix vec
	for _, t := range n.prefix {
		prefix = larger(prefix, prev[t])
	}
	add("prefixItems", prefix)
	add("items", countOf(prev, n.items))
	add("unevaluatedItems", countOf(prev, n.uneItems))
	return out
}

// classes holds the counts that reach the children of one value, by the
// class of the child: an object member by name (names, else name), an
// array item by index (idx, else index), and a member name itself (key).
// It is immutable once built.
type classes struct {
	name  vec
	names map[string]vec
	index vec
	idx   []vec
	key   vec
	top   vec
}

func (m *classes) nameOf(k string) vec {
	if c, ok := m.names[k]; ok {
		return c
	}
	return m.name
}

func (m *classes) indexOf(i int) vec {
	if i < len(m.idx) {
		return m.idx[i]
	}
	return m.index
}

func (m *classes) setTop() {
	m.top = larger(larger(m.name, m.index), m.key)
	for _, c := range m.names {
		m.top = larger(m.top, c)
	}
	for _, c := range m.idx {
		m.top = larger(m.top, c)
	}
}

// isZero reports whether m adds nothing to any class.
func (m *classes) isZero() bool { return m.top.isZero() }

// combine returns f applied class by class to a and b, charging the
// entries it touches to budget; its member map comes from pool.
func combine(a, b *classes, f func(vec, vec) vec, budget *int, pool *namesPool) *classes {
	m := &classes{name: f(a.name, b.name), index: f(a.index, b.index), key: f(a.key, b.key)}
	if len(a.names)+len(b.names) > 0 {
		m.names = pool.get(max(len(a.names), len(b.names)))
		for k := range a.names {
			m.names[k] = f(a.nameOf(k), b.nameOf(k))
		}
		for k := range b.names {
			m.names[k] = f(a.nameOf(k), b.nameOf(k))
		}
	}
	for i := range max(len(a.idx), len(b.idx)) {
		m.idx = append(m.idx, f(a.indexOf(i), b.indexOf(i)))
	}
	*budget -= 1 + len(a.names) + len(b.names) + len(m.idx)
	m.setTop()
	return m
}

// either returns the class-wise larger of the counts of two alternatives,
// of which one applies.
func either(a, b *classes, budget *int, pool *namesPool) *classes {
	switch {
	case b.isZero():
		return a
	case a.isZero():
		return b
	}
	return combine(a, b, larger, budget, pool)
}

// edgeClasses returns the classes of in-place member e: the class-wise
// larger of its possible targets.
func edgeClasses(byNode []*classes, e gEdge, budget *int, pool *namesPool) *classes {
	m := byNode[e.to[0]]
	for _, t := range e.to[1:] {
		m = either(m, byNode[t], budget, pool)
	}
	return m
}

// crossing names where the root's evaluation count exceeds MaxEvaluations
// at level h: the first schema in order among those the root applies in
// place whose count exceeds it (in-place targets come first in order, so
// each of its own targets stays within it), at the keyword adding the most
// to it.
func (r *run) crossing(cur, prev []vec, h int) Issue {
	for _, v := range r.order {
		if !r.rootIn[v] || cur[v][dimEvals] <= MaxEvaluations {
			continue
		}
		n := &r.g.nodes[v]
		var parts []contribution
		for _, e := range n.in {
			parts = append(parts, contribution{kw: e.kw, n: pick(cur, e)[dimEvals]})
		}
		if h > 0 {
			parts = append(parts, r.ownContributions(v, prev, h)...)
		}
		best := contribution{}
		for _, p := range parts {
			if p.n > best.n {
				best = p
			}
		}
		ptr := n.ptr
		if best.kw != "" {
			ptr = appendToken(ptr, best.kw)
		}
		return Issue{Pointer: ptr, Message: fmt.Sprintf("validating one value of a body nested %d deep can take more than %d subschema evaluations here, counting every applicator branch and every repetition of a recursive reference; a schema must allow bodies nested at least %d deep", h, MaxEvaluations, MinNesting)}
	}
	// Unreachable: the root itself is in place and exceeds.
	return Issue{Pointer: r.g.nodes[0].ptr, Message: fmt.Sprintf("validating one value of a body can take more than %d subschema evaluations", MaxEvaluations)}
}

// evaluations computes the bound at every depth and the root's nesting
// limit, given the in-place order and the budget of the precise pass.
// Level 0 counts the schemas evaluated at the value itself; level h those
// evaluated at its descendants h levels down, along the costliest path of
// member names and indexes. A coarse pass takes the costliest class of
// child at each schema on its own; only when the root's evaluation count
// exceeds MaxEvaluations at some level does a precise pass, which picks
// one class for all the schemas evaluated at the same value, decide. Both
// are upper bounds in every dimension, so the precise pass's counts and
// limit stand when it completes, and the coarse pass's otherwise. A
// nesting limit below MinNesting is RZ-CFG-005 at the crossing.
func (g *evalGraph) evaluations(order []int, budget int) (*workBound, []Issue) {
	r := &run{g: g, order: order, rootIn: g.inPlaceFrom(0)}
	p := r.levelZero()
	wb := &workBound{flags: g.flags}
	limit, is, _ := r.levels(p, r.coarseLevel, wb.at[:])
	if limit <= maxNesting {
		var precise [maxNesting + 1]vec
		b := budget
		pl, pis, complete := r.levels(p, func(prev []vec, h int) []vec { return r.preciseLevel(prev, h, &b) }, precise[:])
		if complete {
			wb.at, limit, is = precise, pl, pis
		}
	}
	if limit < MinNesting {
		return nil, []Issue{is}
	}
	wb.limit = limit
	return wb, nil
}

// levels runs level from 1 to maxNesting and records the root's counts of
// every level in out (out[0] from p), stopping at a fixed point, after
// which every level repeats the last, or at the first level whose root
// count exceeds MaxEvaluations: it returns that level (maxNesting+1 when
// none) and the finding naming it. complete is false when level returned
// nil (its budget is spent).
func (r *run) levels(p []vec, level func(prev []vec, h int) []vec, out []vec) (limit int, is Issue, complete bool) {
	out[0] = p[0]
	if p[0][dimEvals] > MaxEvaluations {
		return 0, r.crossing(p, nil, 0), true
	}
	prev, last := p, 0
	for h := 1; h <= maxNesting; h++ {
		cur := level(prev, h)
		if cur == nil {
			return 0, Issue{}, false
		}
		out[h], last = cur[0], h
		if cur[0][dimEvals] > MaxEvaluations {
			return h, r.crossing(cur, prev, h), true
		}
		// From level 2 on a level depends only on the one before it.
		if h >= 2 && slices.Equal(cur, prev) {
			break
		}
		prev = cur
	}
	for h := last + 1; h <= maxNesting; h++ {
		out[h] = out[last]
	}
	return maxNesting + 1, Issue{}, true
}

// coarseLevel returns the level-h counts, taking each schema's costliest
// class of child on its own.
func (r *run) coarseLevel(prev []vec, h int) []vec {
	cur := make([]vec, len(r.g.nodes))
	for _, v := range r.order {
		cur[v] = r.inPlace(v, r.ownTop(v, prev, h), cur)
	}
	return cur
}

// preciseLevel returns the level-h counts, choosing one class of child for
// all the schemas evaluated at the same value; it returns nil once budget
// is spent.
func (r *run) preciseLevel(prev []vec, h int, budget *int) []vec {
	// The classes of the level before are garbage: only prev, their
	// counts, remains.
	r.names.recycle()
	byNode := make([]*classes, len(r.g.nodes))
	cur := make([]vec, len(r.g.nodes))
	for _, v := range r.order {
		m := r.own(v, prev, h)
		in := r.g.nodes[v].in
		for i := 0; i < len(in); i++ {
			em := edgeClasses(byNode, in[i], budget, &r.names)
			if i+1 < len(in) && in[i+1].alt {
				i++
				em = either(em, edgeClasses(byNode, in[i], budget, &r.names), budget, &r.names)
			}
			// Maps are immutable, so a schema that adds nothing of its
			// own to another's counts, such as a lone $ref, shares them.
			switch {
			case em.isZero():
			case m.isZero():
				m = em
			default:
				m = combine(m, em, sat, budget, &r.names)
			}
		}
		if *budget < 0 {
			return nil
		}
		byNode[v], cur[v] = m, m.top
	}
	return cur
}
