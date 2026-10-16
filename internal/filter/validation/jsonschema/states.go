// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"cmp"
	"encoding/binary"
	"encoding/json"
	"slices"
	"strings"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"
)

// The counts of each body value's own path (architecture R-77, package
// documentation "Working set"). The depth bound of the analysis (workBound)
// holds, at every depth, the largest counts of any member class at that
// depth, so a costly union in one member would make every value at the
// same depth expensive, whatever its member path or JSON kind. The model
// therefore also follows the body itself: at compile time it builds the
// states of an automaton, each the multiset of schemas the library
// evaluates at a value reached along some path of member names and
// indexes, with the transitions from it by member name, by index and to a
// member name under propertyNames; the scan before validation walks them
// with the body and charges each value its own state's counts, never more
// than the depth bound (both are upper bounds, so their smaller one is).
//
// A state is an upper bound of the evaluations at its values: a member or
// item gets every subschema that can apply to it, both then and else
// apply, a $dynamicRef applies each of its possible targets, a member name
// no properties keyword declares meets every patternProperties schema and
// the additionalProperties (or unevaluatedProperties) schema, and an item
// past the prefixItems of a schema meets its items or unevaluatedItems.
// The states of one document are bounded by maxStates, maxTransitions and
// stateBudget; past them a transition is unknown and the values below it
// are charged the depth bound.

// JSON kinds of a body value, for the error units a value of each kind may
// hold: a keyword of another kind cannot fail on it.
const (
	kindObject = iota
	kindArray
	kindString
	kindNumber
	kindBoolean
	kindNull
	// kindInteger is a number whose literal has neither a fraction nor an
	// exponent: an integer or number type admits it, which the scan knows
	// from the literal (kindNumber may be any number, which an integer type
	// may refuse).
	kindInteger
	kinds
)

// kindOf returns the JSON kind of a decoded value.
func kindOf(v any) int {
	switch t := v.(type) {
	case map[string]any:
		return kindObject
	case []any:
		return kindArray
	case string:
		return kindString
	case bool:
		return kindBoolean
	case nil:
		return kindNull
	case json.Number:
		if !strings.ContainsAny(string(t), ".eE") {
			return kindInteger
		}
		return kindNumber
	default:
		return kindNumber
	}
}

// typeNames returns the type keyword name of each JSON kind.
func typeNames() [kinds]string {
	return [kinds]string{"object", "array", "string", "number", "boolean", "null", "integer"}
}

// maxStates bounds the states of one document and maxTransitions their
// transitions by name and index, which the compiled Schema keeps (about 5
// MB at the bounds); stateBudget bounds the schemas, multiset entries,
// in-place edges, programs of the schemas' pattern reaches and declared
// names building them may touch (part of BenchmarkEvaluationAnalysis;
// review finding: the programs merged for each state were not charged).
// Matching the declared names against the patterns of other schemas
// spends what newGNode left of the document's matchBudget.
const (
	maxStates      = 1 << 12
	maxTransitions = 1 << 16
	stateBudget    = 1 << 20
)

// unknownState marks a transition past the bounds: the values it reaches
// are charged the depth bound.
const unknownState = -1

// evalState is one state: the counts the schemas evaluated at a value add
// up to, and the transitions to the states of its children.
type evalState struct {
	c vec
	// full[k] are the error units the evaluations may hold at a value of
	// kind k in the location pass and names[k] the units its name lists
	// add; decide[k] the decision pass's, at an object those of the
	// evaluations without member keywords of their own, and groups[k] the
	// evaluations that may add a group error in it.
	full, names, decide, groups [kinds]int
	// members counts the evaluations that loop over an object's members
	// with member keywords of their own (properties, patternProperties,
	// additionalProperties): the decision pass keeps the errors of one
	// member at most for each of them, as loops details by their units
	// (when it fits maxLoops) and loose sums, the larger of each one's two
	// exclusive outcomes without the member (units.first and
	// units.after).
	members int
	loops   []loop
	loose   int
	// lengths details, by their units, the evaluations with minLength or
	// maxLength, whose failure the scan decides from a string's length;
	// their string units are not in full, names, decide and groups. When
	// they do not fit maxLoops, the list is nil and they are counted there
	// as if both keywords fail.
	lengths []lengthLoop
	// uneval is set when some evaluation has unevaluatedProperties, which
	// keeps the errors of every member it evaluates.
	uneval bool
	// compares is set when some evaluation compares the whole value with
	// a const or enum value holding arrays or objects, which parses the
	// numbers nested in the value.
	compares bool
	// lists counts the evaluations with additionalProperties false, each
	// of which lists the names it does not declare while it loops.
	lists int
	// matches counts the evaluations with contains, each of which lists the
	// indexes of the items that match it while it loops, and kept those
	// with minContains or maxContains, whose error keeps the list in the
	// location pass.
	matches, kept int
	// propNames counts the evaluations with propertyNames, each of which
	// validates every member name on its own.
	propNames int
	// chain is the longest chain of in-place evaluations at a value, which
	// the library holds on its call stack at once (evalGraph.chain): the
	// evaluations of the state run one chain at a time.
	chain int
	// match is what matching one member name against the
	// patternProperties of every evaluation costs: work units (dimWork)
	// and work units per byte of the name (dimScan).
	match vec
	// valuePats is what may match the value's string, the patterns of
	// every evaluation, and namePats what may match a member's name, their
	// patternProperties: the scan charges a matcher and the backtracker's
	// job stack only for these (review finding: every pattern of the
	// document was charged to every string a pattern or a uniqueItems pass
	// reached).
	valuePats, namePats patternReach
	// multiples counts the evaluations with multipleOf, and divNum and
	// divDen sum the digits of their divisors' numerators and denominators
	// (gNode.divNum), for the work of their checks (quoWork).
	multiples, divNum, divDen int
	// byName, other, idx, rest and key are the transitions: a member by
	// declared name, any other member, an item by index below len(idx),
	// any other item, and a member name under propertyNames.
	byName map[string]int
	other  int
	idx    []int
	rest   int
	key    int
}

// loop is the evaluations of a state with the same exclusive outcomes:
// n of them, each holding first units, or after units and one member's
// errors.
type loop struct{ n, first, after int }

// lengthLoop is the evaluations of a state with the same length units: n
// of them, each with units u.
type lengthLoop struct {
	n int
	u lengthUnits
}

// maxLoops bounds the loops and length loops a state details.
const maxLoops = 8

// member returns the state of the member named k.
func (s *evalState) member(k string) int {
	if t, ok := s.byName[k]; ok {
		return t
	}
	return s.other
}

// item returns the state of the item at index i.
func (s *evalState) item(i int) int {
	if i < len(s.idx) {
		return s.idx[i]
	}
	return s.rest
}

// entry is one schema of a multiset with its count of evaluations.
type entry struct{ node, n int }

// stateBuilder builds the states of one graph, breadth first from the
// root's.
type stateBuilder struct {
	g *evalGraph
	// pos orders the schemas so that every schema comes before the
	// schemas it applies in place.
	pos    []int
	states []evalState
	sets   [][]entry
	index  map[string]int
	// budget is what is left of stateBudget, transitions of
	// maxTransitions, and matches of the document's matchBudget (work
	// units, matchWork).
	budget, transitions, matches int
	// count and seen are scratch indexed by schema, count valid where
	// seen is gen; nodes, stack, out and key are scratch buffers.
	count []int
	seen  []uint32
	gen   uint32
	nodes []int
	stack []int
	out   []entry
	key   []byte
	// values and names merge the pattern reaches of a state's schemas,
	// whose lists of programs lists interns.
	values, names reachMerger
	lists         reachLists
}

// buildStates returns the states of the graph whose in-place order (every
// in-place target before the schemas applying it) is order, within budget
// (stateBudget for a document); states[0] is the root's. It returns nil
// when even the root's state is past the bounds.
func (g *evalGraph) buildStates(order []int, budget int) []evalState {
	return newStateBuilder(g, order, budget).build()
}

// newStateBuilder returns a builder of the states of the graph in the
// in-place order order, within budget and what is left of the document's
// matchBudget.
func newStateBuilder(g *evalGraph, order []int, budget int) *stateBuilder {
	b := &stateBuilder{
		g: g, pos: make([]int, len(g.nodes)), index: map[string]int{}, budget: budget, transitions: maxTransitions,
		matches: g.matches, count: make([]int, len(g.nodes)), seen: make([]uint32, len(g.nodes)),
	}
	for i, v := range order {
		b.pos[v] = len(order) - 1 - i
	}
	return b
}

// build adds the root's state and every state its transitions reach, and
// returns them, or nil when even the root's state is past the bounds.
func (b *stateBuilder) build() []evalState {
	if b.next([]entry{{node: 0, n: 1}}) != 0 {
		return nil
	}
	for i := 0; i < len(b.states); i++ {
		b.link(i)
	}
	return b.states
}

// next returns the state of a value whose member or item keywords bring
// the schemas of entries: the multiset of each of them with every schema
// it applies in place, counted once per evaluation that applies it. It
// adds the state when it is new and the bounds allow, or returns
// unknownState; a nil entries (its own bound spent) is unknown too.
func (b *stateBuilder) next(entries []entry) int {
	if entries == nil || b.budget < 0 {
		return unknownState
	}
	b.gen++
	b.nodes, b.stack = b.nodes[:0], b.stack[:0]
	for _, e := range entries {
		b.stack = append(b.stack, e.node)
	}
	edges, progs := 0, 0
	for len(b.stack) > 0 {
		v := b.stack[len(b.stack)-1]
		b.stack = b.stack[:len(b.stack)-1]
		if b.seen[v] == b.gen {
			continue
		}
		b.seen[v], b.count[v] = b.gen, 0
		b.nodes = append(b.nodes, v)
		n := &b.g.nodes[v]
		progs += len(n.valuePats.progs) + len(n.namePats.progs)
		for _, e := range n.in {
			edges += len(e.to)
			b.stack = append(b.stack, e.to...)
		}
	}
	b.budget -= len(entries) + 2*len(b.nodes) + edges + progs + 1
	if b.budget < 0 {
		return unknownState
	}
	for _, e := range entries {
		b.count[e.node] = min(b.count[e.node]+e.n, evalCap)
	}
	slices.SortFunc(b.nodes, func(x, y int) int { return b.pos[x] - b.pos[y] })
	for _, v := range b.nodes {
		c := b.count[v]
		for _, e := range b.g.nodes[v].in {
			for _, t := range e.to {
				b.count[t] = min(b.count[t]+c, evalCap)
			}
		}
	}
	slices.Sort(b.nodes)
	b.key = b.key[:0]
	for _, v := range b.nodes {
		b.key = binary.AppendVarint(b.key, int64(v))
		b.key = binary.AppendVarint(b.key, int64(b.count[v]))
	}
	if i, ok := b.index[string(b.key)]; ok {
		return i
	}
	if len(b.states) >= maxStates {
		return unknownState
	}
	set := make([]entry, len(b.nodes))
	for j, v := range b.nodes {
		set[j] = entry{node: v, n: b.count[v]}
	}
	i := len(b.states)
	b.index[string(b.key)] = i
	b.states = append(b.states, b.weigh(set))
	b.sets = append(b.sets, set)
	return i
}

// weigh returns the counts of multiset set, without transitions. Its work
// is linear in the schemas of set: the lists it details are bounded by
// maxLoops, past which they are summed (review finding: the length loops
// grew with every distinct pair of lengths, each added after a scan of
// the list, so 1,000 minLength values under 500 members compiled for 1 s),
// and the pattern reaches merge in a table.
func (b *stateBuilder) weigh(set []entry) evalState {
	s := evalState{other: unknownState, rest: unknownState, key: unknownState}
	detailed := true
	for _, e := range set {
		n := &b.g.nodes[e.node]
		w := b.g.weight[e.node]
		for k := range w {
			w[k] = min(w[k]*e.n, capOf(k))
		}
		s.c = sat(s.c, w)
		u := &n.units
		for k := range kinds {
			if k == kindString && u.length != nil {
				if detailed {
					s.lengths = addLength(s.lengths, lengthLoop{n: e.n, u: *u.length})
					detailed = len(s.lengths) <= maxLoops
					if !detailed {
						// Past maxLoops kinds, every one counts as if both
						// keywords fail.
						for _, l := range s.lengths {
							s.addWorstLength(l)
						}
						s.lengths = nil
					}
				} else {
					s.addWorstLength(lengthLoop{n: e.n, u: *u.length})
				}
				continue
			}
			s.addUnits(k, e.n, u.full[k], u.names[k], u.decide[k], u.groups[k])
		}
		if len(n.props) > 0 || len(n.pats) > 0 || n.addl >= 0 {
			s.members = min(s.members+e.n, countCap)
			s.loose = min(s.loose+u.decide[kindObject]*e.n, countCap)
			s.loops = addLoop(s.loops, loop{n: e.n, first: u.first, after: u.after})
		} else {
			s.decide[kindObject] = min(s.decide[kindObject]+u.decide[kindObject]*e.n, countCap)
		}
		s.uneval = s.uneval || n.uneProps >= 0
		s.compares = s.compares || n.compares
		if e.node < len(b.g.chain) {
			s.chain = max(s.chain, b.g.chain[e.node])
		}
		if n.addl == b.g.one {
			s.lists = min(s.lists+e.n, countCap)
		}
		if n.contains >= 0 {
			s.matches = min(s.matches+e.n, countCap)
			if n.keepsMatches {
				s.kept = min(s.kept+e.n, countCap)
			}
		}
		if n.propNames >= 0 {
			s.propNames = min(s.propNames+e.n, countCap)
		}
		b.values.add(n.valuePats)
		b.names.add(n.namePats)
		if n.divNum > 0 {
			s.multiples = min(s.multiples+e.n, countCap)
			s.divNum = min(s.divNum+n.divNum*e.n, countCap)
			s.divDen = min(s.divDen+n.divDen*e.n, countCap)
		}
		m := n.match
		for k := range m {
			m[k] = min(m[k]*e.n, countCap)
		}
		s.match = sat(s.match, m)
	}
	s.valuePats, s.namePats = b.values.reach(&b.lists), b.names.reach(&b.lists)
	return s
}

// addWorstLength adds the string units of length loop l as if both its
// keywords fail.
func (s *evalState) addWorstLength(l lengthLoop) {
	full, decide, groups := l.u.worst()
	s.addUnits(kindString, l.n, full, l.u.names, decide, groups)
}

// addUnits adds n evaluations of the given units at kind k to the state's
// sums; at an object, decide holds those without member keywords only.
func (s *evalState) addUnits(k, n, full, names, decide, groups int) {
	s.full[k] = min(s.full[k]+full*n, countCap)
	s.names[k] = min(s.names[k]+names*n, countCap)
	s.groups[k] = min(s.groups[k]+groups*n, countCap)
	if k != kindObject {
		s.decide[k] = min(s.decide[k]+decide*n, countCap)
	}
}

// addLoop adds l to loops, merged with the loop of the same outcomes; past
// maxLoops loops it returns a nil list with a sentinel loop of n -1, after
// which the state's loops are not detailed.
func addLoop(loops []loop, l loop) []loop {
	if len(loops) == 1 && loops[0].n < 0 {
		return loops
	}
	for i := range loops {
		if loops[i].first == l.first && loops[i].after == l.after {
			loops[i].n = min(loops[i].n+l.n, countCap)
			return loops
		}
	}
	if len(loops) == maxLoops {
		return []loop{{n: -1}}
	}
	return append(loops, l)
}

// addLength adds l to lengths, merged with the length loop of the same
// units.
func addLength(lengths []lengthLoop, l lengthLoop) []lengthLoop {
	for i := range lengths {
		if lengths[i].u == l.u {
			lengths[i].n = min(lengths[i].n+l.n, countCap)
			return lengths
		}
	}
	return append(lengths, l)
}

// link sets the transitions of state i. A state whose declared names or
// indexes do not fit what is left of maxTransitions and stateBudget keeps
// its members or items unknown: a declared name must never fall to the
// transition of the names no schema declares.
func (b *stateBuilder) link(i int) {
	set := b.sets[i]
	declared, widest, prefix := 0, 0, 0
	for _, e := range set {
		n := &b.g.nodes[e.node]
		declared += len(n.props)
		widest = max(widest, len(n.props))
		prefix = max(prefix, len(n.prefix))
	}
	// The distinct names number at least the widest schema's and at most
	// their sum; past the transitions or the budget left, they are not
	// collected, and then neither are the transitions by name nor the one
	// of the other names (review finding: an uncollected list is empty, and
	// the other names' state would have taken every declared name).
	var names []string
	collected := widest <= b.transitions && declared <= b.budget
	if collected {
		b.budget -= declared
		names = make([]string, 0, declared)
		for _, e := range set {
			for _, p := range b.g.nodes[e.node].props {
				names = append(names, p.name)
			}
		}
		slices.Sort(names)
		names = slices.Compact(names)
	}
	if collected && len(names) <= b.transitions {
		b.transitions -= len(names)
		var byName map[string]int
		if len(names) > 0 {
			byName = make(map[string]int, len(names))
		}
		for _, k := range names {
			byName[k] = b.next(b.memberEntries(set, k, true))
		}
		b.states[i].byName = byName
		b.states[i].other = b.next(b.memberEntries(set, "", false))
	}
	if prefix <= b.transitions {
		b.transitions -= prefix
		if prefix > 0 {
			idx := make([]int, prefix)
			for j := range idx {
				idx[j] = b.next(b.itemEntries(set, j))
			}
			b.states[i].idx = idx
		}
		b.states[i].rest = b.next(b.itemEntries(set, prefix))
	}
	keys := b.out[:0]
	for _, e := range set {
		if t := b.g.nodes[e.node].propNames; t >= 0 {
			keys = append(keys, entry{node: t, n: e.n})
		}
	}
	b.out = keys
	if keys == nil {
		keys = []entry{}
	}
	b.states[i].key = b.next(keys)
}

// memberEntries returns the schemas the evaluations of set apply to a
// member: named k when declared, or any member no evaluation of set
// declares. Per evaluation, a declared member meets its properties schema
// and the patterns that match its name; any other name meets the patterns
// that match it or, when none does, additionalProperties or else
// unevaluatedProperties. A name whose patterns are not matched (any other
// name, or one whose matching, matchWork, is past what is left of the
// document's matchBudget) meets every pattern and the fallback. It returns
// nil once the budget is spent; the result lives until the next call.
func (b *stateBuilder) memberEntries(set []entry, k string, declared bool) []entry {
	out := b.out[:0]
	add := func(t, n int) {
		if t >= 0 {
			out = append(out, entry{node: t, n: n})
		}
	}
	for _, e := range set {
		if b.budget < 0 {
			return nil
		}
		n := &b.g.nodes[e.node]
		b.budget -= 1 + len(n.pats)
		if declared {
			if j, ok := slices.BinarySearchFunc(n.props, k, func(p gProp, k string) int { return cmp.Compare(p.name, k) }); ok {
				p := &n.props[j]
				add(p.to, e.n)
				pats := p.pats
				if n.allPats {
					pats = n.pats
				}
				for _, t := range pats {
					add(t, e.n)
				}
				continue
			}
		}
		matchedAny, known := false, false
		if w := matchWork(n.match, len(k)); declared && w <= b.matches {
			b.matches -= w
			known = true
			for j, r := range n.res {
				if r.MatchString(k) {
					add(n.pats[j], e.n)
					matchedAny = true
				}
			}
		} else {
			for _, t := range n.pats {
				add(t, e.n)
			}
		}
		if !known || !matchedAny {
			fallback := n.addl
			if fallback < 0 {
				fallback = n.uneProps
			}
			add(fallback, e.n)
		}
	}
	b.out = out
	if out == nil {
		return []entry{}
	}
	return out
}

// itemEntries returns the schemas the evaluations of set apply to the
// item at index i: prefixItems below their length, else items or else
// unevaluatedItems, and contains and the uniqueItems hashing of every
// item. It returns nil once the budget is spent; the result lives until
// the next call.
func (b *stateBuilder) itemEntries(set []entry, i int) []entry {
	out := b.out[:0]
	add := func(t, n int) {
		if t >= 0 {
			out = append(out, entry{node: t, n: n})
		}
	}
	b.budget -= len(set)
	if b.budget < 0 {
		return nil
	}
	for _, e := range set {
		n := &b.g.nodes[e.node]
		switch {
		case i < len(n.prefix):
			add(n.prefix[i], e.n)
		case n.items >= 0:
			add(n.items, e.n)
		default:
			add(n.uneItems, e.n)
		}
		add(n.contains, e.n)
		add(n.unique, e.n)
	}
	b.out = out
	if out == nil {
		return []entry{}
	}
	return out
}

// patternRegexps returns the patternProperties of s ordered by pattern,
// as gNode.pats lists their targets.
func patternRegexps(s *sjs.Schema) []sjs.Regexp {
	pats := make([]sjs.Regexp, 0, len(s.PatternProperties))
	for r := range s.PatternProperties {
		pats = append(pats, r)
	}
	slices.SortFunc(pats, func(a, b sjs.Regexp) int { return cmp.Compare(a.String(), b.String()) })
	return pats
}
