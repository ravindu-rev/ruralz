// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"cmp"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The tests of the walk's integer model (model.go): each mirror it keeps
// of what the walk computed from pointer strings before the check finding
// on the fixpoint (passes times pointer bytes) gives what the strings gave.

// orderDoc is a document whose positions exercise the order of pointers:
// decimal tokens and their aliases, escaped tokens, empty tokens, prefixes
// and positions below a long shared member name.
func orderDoc(t *testing.T) any {
	t.Helper()
	return decodeInstance(t, `{"a":{"0":{},"00":{},"1":{},"10":{},"2":{},"b":{},"a~b":{},"a/b":{},"x":{"y":{}}},`+
		`"allOf":[{},{},{},{},{},{},{},{},{},{},{}],"`+strings.Repeat("L", 300)+`":{"p":{},"q":{}},"":{"":{}}}`)
}

// orderPointers are the positions of orderDoc the order tests name.
func orderPointers() []string {
	long := "/" + strings.Repeat("L", 300)
	return []string{
		"", "/a", "/a/0", "/a/00", "/a/1", "/a/10", "/a/2", "/a/b", "/a/a~0b", "/a/a~1b", "/a/x", "/a/x/y",
		"/allOf/2", "/allOf/10", "/allOf/02", "/allOf/0", "/allOf/00", long, long + "/p", long + "/q", "/", "//",
	}
}

// nodesOf returns the node of each pointer of ptrs in w's document,
// failing the test when one names no schema or keeps another pointer.
func nodesOf(t *testing.T, w *walker, ptrs []string) []int {
	t.Helper()
	out := make([]int, len(ptrs))
	for i, p := range ptrs {
		n := w.pointerTarget(rootNode, w.intern(p))
		if n < 0 || w.nodes[n].ptr != p {
			t.Fatalf("pointerTarget(%q) = %d", p, n)
		}
		out[i] = n
	}
	return out
}

// TestComparePositions covers the model's order of positions
// (comparePositions, by which insertKnown keeps the targets found): for
// every pair of orderPointers it orders as comparePointers orders their
// pointers, which the walk sorted every pass before the fix: decimal
// tokens by value, aliases such as 0 and 00 alike, other tokens bytewise,
// escaped ones as written, a pointer before every pointer it prefixes, and
// positions below a shared member by the tokens below it.
func TestComparePositions(t *testing.T) {
	ptrs := orderPointers()
	w := newWalker(orderDoc(t))
	nodes := nodesOf(t, w, ptrs)
	for i := range ptrs {
		for j := range ptrs {
			got := cmp.Compare(w.comparePositions(nodes[i], nodes[j]), 0)
			if want := cmp.Compare(comparePointers(ptrs[i], ptrs[j]), 0); got != want {
				t.Errorf("comparePositions(%.20q, %.20q) = %d, want %d", ptrs[i], ptrs[j], got, want)
			}
		}
	}
}

// TestInsertKnown covers how the walk adds the targets a pass found to the
// targets found before (insertKnown): as the stable sort by
// comparePointers of the list with the new ones appended, which the walk
// made every pass before the fix, so aliases that order alike keep the
// order in which they were found. Placing them charges nothing to the
// walk-time compile budget, even with the fixpoint charging and no budget
// left, under a member name of 300 bytes (check finding: placing them
// charged both tokens of every comparison, and the steps of their paths,
// which refused ordinary two-pass documents; TestFollowFixpointCost), and
// counts each target once.
func TestInsertKnown(t *testing.T) {
	w := newWalker(orderDoc(t))
	long := "/" + strings.Repeat("L", 300)
	batches := [][]string{
		{"/a/1", "/allOf/00", "/a/00"},
		{"/a/0", "/allOf/10", "/a", "/allOf/0"},
		{"/allOf/02", "/allOf/2", "", "//", "/"},
		{long + "/q", long, long + "/p"},
	}
	var known []foundTarget
	var appended []string
	for _, batch := range batches {
		nodes := nodesOf(t, w, batch)
		found := make([]foundTarget, len(batch))
		for i, n := range nodes {
			found[i] = foundTarget{node: n, site: i}
		}
		// The fixpoint charging, with no budget left.
		w.charging, w.budget, w.spent, w.steps, w.bytes, w.stopped = true, 0, 0, 0, 0, false
		placed := w.placed
		known = w.insertKnown(known, found)
		if w.spent != 0 || w.steps != 0 || w.bytes != 0 || w.stopped || w.placed-placed != len(batch) {
			t.Errorf("placing %q spent %d units (%d steps, %d bytes), stopped %v, placed %d, want nothing spent and each placed once", batch, w.spent, w.steps, w.bytes, w.stopped, w.placed-placed)
		}
		w.charging = false
		appended = append(appended, batch...)
		want := slices.Clone(appended)
		slices.SortStableFunc(want, comparePointers)
		got := make([]string, len(known))
		for i, k := range known {
			got[i] = w.nodes[k.node].ptr
		}
		if !slices.Equal(got, want) {
			t.Errorf("after %.40q: %.40q, want %.40q", batch, got, want)
		}
	}
}

// followGen builds small random documents over a narrow vocabulary of
// identifiers, references, subschema keywords and members that are no
// keywords, so that resources, anchors, aliases and targets outside the
// keyword tree meet often.
type followGen struct {
	r     *rand.Rand
	paths []string
}

func (g *followGen) ref() string {
	refs := []string{"#a", "#d", "urn:a", "urn:b#/x", "x.json", "#", "y.json#/items", "https://h.test/d/#/x/y", "#/items/00", "#/a~1b", "urn:a#a"}
	if i := g.r.IntN(len(refs) + 3); i < len(refs) {
		return refs[i]
	}
	if len(g.paths) > 0 {
		return "#" + g.paths[g.r.IntN(len(g.paths))]
	}
	return "#/x"
}

func (g *followGen) schema(depth int, ptr string) string {
	g.paths = append(g.paths, ptr)
	if depth == 0 || g.r.IntN(6) == 0 {
		return []string{"true", "{}"}[g.r.IntN(2)]
	}
	var m []string
	add := func(k, v string) { m = append(m, `"`+k+`":`+v) }
	if g.r.IntN(3) == 0 {
		ids := []string{"urn:a", "urn:b", "x.json", "https://h.test/d/", "y.json", "#f", "sub/", "../z.json", "urn:a#x"}
		add("$id", `"`+ids[g.r.IntN(len(ids))]+`"`)
	}
	if g.r.IntN(4) == 0 {
		add("$anchor", `"a"`)
	}
	if g.r.IntN(5) == 0 {
		add("$dynamicAnchor", `"d"`)
	}
	for _, kw := range referenceKeywords() {
		if g.r.IntN(4) == 0 {
			add(kw, `"`+g.ref()+`"`)
		}
	}
	for _, kw := range []string{"additionalItems", "not", "x", "y", "a/b"} {
		if g.r.IntN(4) == 0 {
			add(kw, g.schema(depth-1, ptr+"/"+escapeToken(kw)))
		}
	}
	if g.r.IntN(4) == 0 {
		add("items", `[`+g.schema(depth-1, ptr+"/items/0")+`,`+g.schema(depth-1, ptr+"/items/1")+`]`)
	}
	if g.r.IntN(4) == 0 {
		add("properties", `{"p":`+g.schema(depth-1, ptr+"/properties/p")+`,"$id":"urn:a"}`)
	}
	if g.r.IntN(3) == 0 {
		add("$defs", `{"p":`+g.schema(depth-1, ptr+"/$defs/p")+`,"q":`+g.schema(depth-1, ptr+"/$defs/q")+`}`)
	}
	return "{" + strings.Join(m, ",") + "}"
}

// sameSchema reports whether a and b are the same schema value: the same
// object, or equal booleans.
func sameSchema(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		return ok && reflect.ValueOf(x).Pointer() == reflect.ValueOf(y).Pointer()
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	}
	return false
}

// TestModelMirrorsPointers covers the model's integer mirrors over every
// document of the class's tests, the generations, and 2,000 random
// documents (a fixed seed): after the walk, every node's pointer is the
// path of its trie entry and names the node's schema, as lookup reads it;
// the resource of every position the last pass collected is the node of
// the longest prefix of its pointer that the pass recorded as a resource
// (resourceOf, which hashed every prefix before the fix); and the target
// of every reference resolved to a JSON Pointer is the pointer of the
// resource its URL names followed by the fragment (pointerTarget, which
// built and looked up that pointer every pass before the fix).
func TestModelMirrorsPointers(t *testing.T) {
	var docs []string
	for _, set := range [][]followCase{baseCases(), lateIDCases(), ancestorCases(), policyDialectCases(), livenessCases(), visibilityCases(), olderDraftCases()} {
		for _, tc := range set {
			docs = append(docs, tc.schema)
		}
	}
	docs = append(docs, generations(3), deepGenerations(3, 40), `{"$ref":"#/x/y","x":{"$id":"https://x.test/","y":{"$ref":"#/x"},"x":{"type":"string"}}}`)
	r := rand.New(rand.NewPCG(11, 13)) //nolint:gosec // G404: deterministic test data
	for range 2000 {
		g := &followGen{r: r}
		docs = append(docs, g.schema(5, ""))
	}
	checked := 0
	for _, schema := range docs {
		p, issues := prepare([]byte(schema))
		if len(issues) > 0 {
			t.Fatalf("%.80s: %+v", schema, issues)
		}
		w := newWalker(p.doc)
		w.walk()
		for n := range w.nodes {
			nd := &w.nodes[n]
			var toks []string
			for e := nd.entry; e > 0; e = w.entries[e].parent {
				toks = append(toks, w.entries[e].tok)
			}
			slices.Reverse(toks)
			path := ""
			if len(toks) > 0 {
				path = "/" + strings.Join(toks, "/")
			}
			if v, ok := lookup(p.doc, nd.ptr); path != nd.ptr || !ok || !sameSchema(v, nd.v) {
				t.Fatalf("%.80s: node %d at %q has the trie path %q, names a schema %v", schema, n, nd.ptr, path, ok)
			}
		}
		resources := map[string]bool{}
		for _, n := range w.order {
			if w.isResource(n) {
				resources[w.nodes[n].ptr] = true
			}
		}
		for _, n := range w.order {
			want := w.nodes[n].ptr
			for !resources[want] {
				want = want[:strings.LastIndexByte(want, '/')]
			}
			if got := w.nodes[w.resourceOf(n)].ptr; got != want {
				t.Errorf("%.80s: the resource of %q is %q, want %q", schema, w.nodes[n].ptr, got, want)
			}
		}
		for _, si := range w.passSites {
			s := w.siteTab[si]
			if s.pass != w.passes || s.res.status != refResolved || s.frag < 0 {
				continue
			}
			res := rootNode
			if s.res.url != baseURLIndex {
				res = w.idNode[s.res.url]
			}
			if got, want := w.nodes[s.res.target].ptr, w.nodes[res].ptr+w.strOf[s.frag]; got != want {
				t.Errorf("%.80s: %s resolved to %q, want %q", schema, s.ptr, got, want)
			}
			checked++
		}
	}
	if checked < 1000 {
		t.Errorf("only %d references resolved to JSON Pointers were checked", checked)
	}
}
