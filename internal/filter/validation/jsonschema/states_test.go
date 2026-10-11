// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"strings"
	"testing"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"
)

// stateGraph returns the evaluation graph of schema, compiled by the
// library alone, with its in-place order and its depth bound; ok is false
// when the evaluation bound refuses it.
func stateGraph(t testing.TB, schema string) (g *evalGraph, order []int, wb *workBound, ok bool) {
	t.Helper()
	g, inDoc := newEvalGraph([]*sjs.Schema{libraryCompile(t, schema)})
	if !inDoc {
		t.Fatal("graph outside the document")
	}
	order, issues := g.inPlaceOrder()
	if len(issues) > 0 {
		return nil, nil, nil, false
	}
	wb, issues = g.evaluations(order, coupledBudget)
	return g, order, wb, len(issues) == 0
}

// scanStates returns the scan of v under the depth bound wb and states.
func scanStates(wb *workBound, states []evalState, v any) *bodyScan {
	ws := newWorkingSet(wb, states)
	scan := &bodyScan{ws: &ws, workLimit: maxWorkLimit}
	scan.scan(v)
	return scan
}

// checkTransitions fails the test at the first declared name or prefix
// index of a built state that falls to the transition of the names or
// items no schema of the state declares: a declared member or item has a
// transition of its own, or the state's members or items are unknown and
// charged the depth bound.
func checkTransitions(t testing.TB, b *stateBuilder) {
	t.Helper()
	for i, set := range b.sets {
		s := &b.states[i]
		for _, e := range set {
			n := &b.g.nodes[e.node]
			for _, p := range n.props {
				if _, ok := s.byName[p.name]; !ok && s.other != unknownState {
					t.Fatalf("state %d declares %q, which falls to the other names' state %d", i, p.name, s.other)
				}
			}
			if len(n.prefix) > len(s.idx) && s.rest != unknownState {
				t.Fatalf("state %d declares %d prefix items, which fall to the other items' state %d", i, len(n.prefix), s.rest)
			}
		}
	}
}

// costlyMembers returns a schema whose member a fails at every item of an
// array, among n members that never fail.
func costlyMembers(n int) string {
	var b strings.Builder
	b.WriteString(`{"properties":{"a":{"items":` + oneOfConsts(8) + `}`)
	for i := range n {
		fmt.Fprintf(&b, `,"k%02d":true`, i)
	}
	b.WriteString(`}}`)
	return b.String()
}

// costlyBelow returns a schema whose member z.w declares m members that
// fail at every item of an array: the empty state of the other names is
// built and linked before w's.
func costlyBelow(m int) string {
	var b strings.Builder
	b.WriteString(`{"properties":{"z":{"properties":{"w":{"$ref":"#/$defs/w"}}}},"$defs":{"costly":{"items":` + oneOfConsts(8) + `},"w":{"properties":{`)
	for j := range m {
		if j > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"m%d":{"$ref":"#/$defs/costly"}`, j)
	}
	b.WriteString(`}}}}`)
	return b.String()
}

// TestStatesBudget is the regression test of the review finding on
// link: when stateBudget ran out between two states, the declared names of
// the second were not collected, yet its transition of the other names
// was set, so every declared name fell to the state of the names no schema
// declares, which evaluates nothing: a member failing at every item of a
// 1.6 MB array was charged 104 bytes of a 1.6 MB error tree, and Handle
// held 728 MB for 15 s of validation (07 reqs 80 to 82, section 9 item 21,
// architecture R-63). At every budget from none to what the states need,
// for flat and nested members and for prefix items, no declared name or
// index falls to the other names' or items' state, and the scan never
// charges less than with the whole budget. It also covers the fallbacks
// of a budget spent in each step: no root state, an unknown entry list, a
// multiset past the budget.
func TestStatesBudget(t *testing.T) {
	arr := denseArray(50, `"x"`, 0)
	for _, tc := range []struct{ name, schema, body string }{
		{"a costly member among cheap ones", costlyMembers(40), `{"a":` + arr + `}`},
		{"costly members below an empty state", costlyBelow(20), `{"z":{"w":{"m0":` + arr + `}}}`},
		{
			"prefix items", `{"prefixItems":[true,{"items":` + oneOfConsts(8) + `}],"items":{"properties":{"a":{"items":` + oneOfConsts(8) + `}}}}`,
			`[1,` + arr + `,{"a":` + arr + `}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, order, wb, ok := stateGraph(t, tc.schema)
			if !ok {
				t.Fatal("the evaluation bound refuses the schema")
			}
			full := newStateBuilder(g, order, stateBudget)
			states := full.build()
			checkTransitions(t, full)
			need := stateBudget - full.budget
			t.Logf("the states need %d of the budget", need)
			v := decodeInstance(t, tc.body)
			want := scanStates(wb, states, v)
			if got := scanStates(wb, g.buildStates(order, 0), v); got.decide < want.decide {
				t.Fatalf("no states charge %d, less than the states' %d", got.decide, want.decide)
			}
			for budget := range need + 1 {
				b := newStateBuilder(g, order, budget)
				got := scanStates(wb, b.build(), v)
				checkTransitions(t, b)
				if got.decide < want.decide || got.locate < want.locate || got.path < want.path || got.work < want.work {
					t.Fatalf("budget %d of %d: decide, locate, path, work = %d, %d, %d, %d, less than with the whole budget: %d, %d, %d, %d",
						budget, need, got.decide, got.locate, got.path, got.work, want.decide, want.locate, want.path, want.work)
				}
			}
		})
	}
}

// stateSchemas are the schemas whose states TestStatesDeclaredNames
// checks: the corpus and the schemas of the other tests and benchmarks.
func stateSchemas(t testing.TB) []string {
	t.Helper()
	var out []string
	for _, b := range corpusSchemas(t) {
		out = append(out, string(b))
	}
	return append(out, unionSchema(12), richText, componentTree, recordsSchema, lineSchema, mixins, taggedUnion,
		operators(7), costlyMembers(40), costlyBelow(60), allOfRefs(20, `{"minLength":0}`), unevaluatedChain(20, "unevaluatedProperties"),
		analysisWorstCase(200, 50, true), schema256KiB(), orderSchema(t),
		`{"allOf":[{"properties":{"ab":true}},{"patternProperties":{"^a":{"items":true}},"additionalProperties":false}]}`,
		`{"prefixItems":[true,{"prefixItems":[true]}],"allOf":[{"prefixItems":[true,true,true]}],"unevaluatedItems":false}`)
}

// TestStatesDeclaredNames checks the invariant of the review finding on
// link (07 reqs 80 to 82, architecture R-63) over the corpus and the test
// and benchmark schemas, with the whole budget and with parts of it: a
// declared member name or prefix index never falls to the transition of
// the names or items no schema declares.
func TestStatesDeclaredNames(t *testing.T) {
	for i, schema := range stateSchemas(t) {
		g, order, _, ok := stateGraph(t, schema)
		if !ok {
			continue
		}
		full := newStateBuilder(g, order, stateBudget)
		if full.build() == nil {
			t.Errorf("schema %d: no root state with the whole budget", i)
			continue
		}
		checkTransitions(t, full)
		need := stateBudget - full.budget
		for _, budget := range []int{need / 8, need / 3, need / 2, need - 1} {
			b := newStateBuilder(g, order, budget)
			b.build()
			checkTransitions(t, b)
		}
	}
}

// TestStatesMemberPatterns pins the member-name logic of the states
// (review finding: mutants that dropped any of it survived the suite).
// The error trees both passes hold, measured exactly, stay within what the
// scan charges with the states, and so does the work of matching the
// names, which the model weighs:
//
//   - a declared member meets its properties schema and the patterns of
//     the same schema that match its name;
//   - a name one schema declares meets the patterns of the others that
//     match it;
//   - and, where none matches, their additionalProperties;
//   - matching a member name against the patterns costs work per byte of
//     the name.
//
// 07 reqs 80 to 82 and architecture R-63 (the working set), 07 section 9
// item 21 (the work bound).
func TestStatesMemberPatterns(t *testing.T) {
	arr := `{"ab":` + denseArray(2000, `"x"`, 0) + `}`
	var pats []string
	for i := range 10 {
		pats = append(pats, fmt.Sprintf(`"^z%d":true`, i))
	}
	var names strings.Builder
	names.WriteByte('{')
	for i := range 2000 {
		if i > 0 {
			names.WriteByte(',')
		}
		fmt.Fprintf(&names, `"k%d%s":1`, i, strings.Repeat("q", 200))
	}
	names.WriteByte('}')
	for _, tc := range []struct{ name, schema, body string }{
		{"a declared member and its schema's pattern", `{"properties":{"ab":{"type":"array"}},"patternProperties":{"^a":{"items":` + oneOfConsts(8) + `}}}`, arr},
		{"a declared member and another schema's pattern", `{"allOf":[{"properties":{"ab":true}},{"patternProperties":{"^a":{"items":` + oneOfConsts(8) + `}}}]}`, arr},
		{"another schema's additionalProperties", `{"allOf":[{"properties":{"ab":true}},{"patternProperties":{"^z":true},"additionalProperties":{"items":` + oneOfConsts(8) + `}}]}`, arr},
		{"matching long names", `{"patternProperties":{` + strings.Join(pats, ",") + `}}`, names.String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := mustCompile(t, tc.schema)
			if len(s.work.states) == 0 {
				t.Fatal("no states")
			}
			v, scan := scanBody(t, s, tc.body)
			g, _, _, _ := stateGraph(t, tc.schema)
			matching := 0
			for k := range v.(map[string]any) {
				matching += matchWork(g.nodes[0].match, len(k))
			}
			if scan.work < int64(matching) {
				t.Errorf("the scan charges %d work units, matching the names takes %d", scan.work, matching)
			}
			if decide := decisionTree(t, tc.schema, v); decide > scan.decide+treeSlack {
				t.Errorf("the decision pass held an error tree of %d bytes, the scan charges %d", decide, scan.decide)
			}
			if locate := locationTree(s, v); locate > scan.locate+treeSlack {
				t.Errorf("the location pass held an error tree of %d bytes, the scan charges %d", locate, scan.locate)
			}
		})
	}
}

// longNames returns n properties named by runs of nameLen a, each true.
func longNames(n, nameLen int) string {
	props := make([]string, n)
	for i := range props {
		props[i] = fmt.Sprintf(`"%s%05d":true`, strings.Repeat("a", nameLen), i)
	}
	return strings.Join(props, ",")
}

// repeatPatterns returns n patternProperties members whose patterns keep
// a run queue of about rep entries on a run of letters, each true.
func repeatPatterns(n, rep int) string {
	pats := make([]string, n)
	for i := range pats {
		pats[i] = fmt.Sprintf(`"[a-z]{%d}x%d":true`, rep, i)
	}
	return strings.Join(pats, ",")
}

// TestCompileMatchBudget is the regression test of the review finding
// that compile-time name matching was bounded by the count of matches, not
// their work: 128 names of 600 bytes against 128 patterns of the same
// schema compiled for 18 s, and 300 names of 800 bytes against 200
// patterns of a sibling for 41 s, in Factory.Build and again in Check (07
// F6, a compile returns within 1 s). Matching is charged its work, a
// name's length times what each pattern costs per byte, against one
// matchBudget per document; past it every pattern is assumed to match,
// which charges more at runtime, never less. Deterministic: the test
// checks what the budget decided, BenchmarkCompileAnalysisWorstCase the
// time.
func TestCompileMatchBudget(t *testing.T) {
	// The schema's own patterns: all of them assumed to match.
	g, _, _, ok := stateGraph(t, `{"properties":{`+longNames(128, 600)+`},"patternProperties":{`+repeatPatterns(128, 300)+`}}`)
	if !ok || !g.nodes[0].allPats || g.matches != matchBudget {
		t.Errorf("own patterns: allPats %v, budget left %d of %d, want every pattern assumed without matching", g.nodes[0].allPats, g.matches, matchBudget)
	}
	// A sibling's patterns: a declared name meets all of them.
	g, order, _, ok := stateGraph(t, `{"allOf":[{"properties":{`+longNames(300, 800)+`}},{"patternProperties":{`+repeatPatterns(200, 100)+`}}]}`)
	if !ok {
		t.Fatal("the evaluation bound refuses the schema")
	}
	b := newStateBuilder(g, order, stateBudget)
	states := b.build()
	m := states[0].member(strings.Repeat("a", 800) + "00000")
	if m < 0 || len(b.sets[m]) != 1+200 || b.matches != g.matches {
		t.Errorf("sibling patterns: member state %d, budget left %d of %d, want the declaring true and all 200 patterns without matching", m, b.matches, g.matches)
	}
	// Within the budget the names are matched: ab meets ^a, not ^z.
	g, order, _, _ = stateGraph(t, `{"allOf":[{"properties":{"ab":true}},{"patternProperties":{"^a":{"type":"array"},"^z":{"type":"null"}}}]}`)
	b = newStateBuilder(g, order, stateBudget)
	states = b.build()
	if m := states[0].member("ab"); m < 0 || len(b.sets[m]) != 2 || b.matches >= g.matches {
		t.Errorf("cheap patterns: member state %d, budget left %d of %d, want the declaring true and the one matching pattern", m, b.matches, g.matches)
	}
	// The budget is the document's: schemas that each fit it share it,
	// and the later ones assume every pattern.
	var branches []string
	for i := range 16 {
		branches = append(branches, `{"properties":{`+strings.ReplaceAll(longNames(20, 100), `"a`, fmt.Sprintf(`"%c`, 'a'+i))+`},"patternProperties":{`+repeatPatterns(2, 100)+`}}`)
	}
	g, _, _, _ = stateGraph(t, `{"allOf":[`+strings.Join(branches, ",")+`]}`)
	matched, assumed := 0, 0
	for i := range g.nodes {
		switch n := &g.nodes[i]; {
		case len(n.props) == 0:
		case n.allPats:
			assumed++
		default:
			matched++
		}
	}
	if matched == 0 || assumed == 0 || g.matches < 0 {
		t.Errorf("shared budget: %d schemas matched, %d assumed, %d left, want both", matched, assumed, g.matches)
	}
	if got := matchWork(vec{dimWork: 10, dimScan: countCap / 2}, 3); got != countCap {
		t.Errorf("matchWork overflow = %d, want %d", got, countCap)
	}
}

// TestStatesLengthLoops covers the evaluations with minLength or
// maxLength that a state details by their units, deciding their failure
// from the string's length (07 reqs 80 and 82, the working set): up to
// maxLoops kinds of them a string that satisfies them is charged less
// than one that fails them all; past it they are charged as if both
// keywords fail, whatever the string.
func TestStatesLengthLoops(t *testing.T) {
	for _, n := range []int{maxLoops, maxLoops + 1} {
		branches := make([]string, n)
		for i := range branches {
			branches[i] = fmt.Sprintf(`{"minLength":%d}`, i+1)
		}
		s := mustCompile(t, `{"allOf":[`+strings.Join(branches, ",")+`]}`)
		_, holds := scanBody(t, s, `"`+strings.Repeat("x", n)+`"`)
		_, fails := scanBody(t, s, `""`)
		detailed := n <= maxLoops
		if (holds.decide < fails.decide) != detailed || (holds.locate < fails.locate) != detailed || holds.decide > fails.decide {
			t.Errorf("%d length kinds: decide %d and %d, locate %d and %d for a string that holds and one that fails, want detailed %v",
				n, holds.decide, fails.decide, holds.locate, fails.locate, detailed)
		}
	}
}
