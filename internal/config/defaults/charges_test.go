// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// foldCost is what folding the declared names of s costs: one unit and
// the bytes of each name.
func foldCost(s *schemaidx.Node) int {
	cost := 0
	for name := range s.Properties() {
		cost += 1 + len(name)
	}
	return cost
}

// TestStepCharges pins the work units of each charge of stage G's own
// loops, strings, member names, number literals and sort comparisons
// charged by their bytes, as TestLookupCharges does for the lookups. Each
// case names what it counts; removing or changing any one charge changes
// its count.
func TestStepCharges(t *testing.T) {
	idx := hubIndex(t)
	spec, _ := idx.Spec("Route")
	upstreams, _ := spec.Property("upstreams")
	match := memberSchema(spec, "match")
	hosts := memberSchema(match.Select(mustParse(t, `{}`)), "hosts")
	routeUpstream, _ := idx.Def("RouteUpstream")
	normalizerOf := func(w *work, depth int) *normalizer {
		z := &normalizer{res: &tree.Resource{ID: tree.ID{Kind: "Route", Name: "r"}}, checkRange: true, w: w}
		z.stack = append(z.stack, step{})
		for i := 1; i < depth; i++ {
			z.stack = append(z.stack, step{name: fmt.Sprint("m", i)})
		}
		return z
	}
	// members returns an object of n members k00, k01, ... of value 1.
	members := func(n int) *tree.Node {
		o := &tree.Node{Kind: tree.KindMap}
		for i := range n {
			o.Members = append(o.Members, tree.Member{Key: fmt.Sprintf("k%02d", i), Value: &tree.Node{Kind: tree.KindInt, Text: "1"}})
		}
		return o
	}
	tests := []struct {
		name string
		run  func(t *testing.T, w *work)
		cost int
	}{
		// Normalize.
		{"visit string: node and bytes", func(_ *testing.T, w *work) {
			normalizerOf(w, 1).visit(mustParse(t, `"abcd"`), nil)
		}, 1 + 4},
		{"visit member name: its bytes", func(_ *testing.T, w *work) {
			// The object, its dispatch over 1 member, the name, the value.
			normalizerOf(w, 1).visit(mustParse(t, `{"abc": true}`), nil)
		}, 1 + 1 + 3 + 1},
		{"visit number: literal bytes", func(_ *testing.T, w *work) {
			normalizerOf(w, 1).visit(mustParse(t, `12345`), nil)
		}, 1 + 5},
		{"host names: lookup and name bytes", func(_ *testing.T, w *work) {
			lowerHosts(w, mustParse(t, `{"hosts": ["AB", "c"]}`), match.Select(mustParse(t, `{}`)))
		}, (1 + len("hosts")) + 2 + 1},
		{"member sort: common prefix", func(_ *testing.T, w *work) {
			// Sorted names: IsSortedFunc compares the one pair.
			normalizerOf(w, 1).sortMembers(mustParse(t, `{"aab": 1, "aac": 2}`))
		}, 1 + 2},
		{"set: elements, sort and duplicate scan", func(_ *testing.T, w *work) {
			normalizerOf(w, 1).set(mustParse(t, `["aab", "aac"]`), hosts, []bool{false, false})
		}, 2*(1+len(`"aab"`)) + (1 + len(`"aa`)) + (1 + len(`"aa`))},
		{"map list: key lookups and sort", func(_ *testing.T, w *work) {
			normalizerOf(w, 1).keyed(mustParse(t, `[{"name": "aab"}, {"name": "aac"}]`), upstreams, "name", true)
		}, 2*(1+(1+4)+len(`"aab"`)) + (1 + 2)},
		{"member names: adjacent name comparison", func(_ *testing.T, w *work) {
			// The second name against the first: one unit and the common
			// prefix "aa".
			n := mustParse(t, `{"aab": 1, "aac": 2}`)
			normalizerOf(w, 1).memberName(n, 1, &memberNames{})
		}, 1 + 2},
		{"member names: declared names folded once, then the name's bytes", func(_ *testing.T, w *work) {
			z := normalizerOf(w, 1)
			names := &memberNames{sel: routeUpstream, structLike: true}
			z.foldsOnto(names, "Weight")
			z.foldsOnto(names, "NAME")
		}, foldCost(routeUpstream) + len("Weight") + len("NAME")},
		{"member names: a declared name is not folded", func(_ *testing.T, w *work) {
			normalizerOf(w, 1).foldsOnto(&memberNames{sel: routeUpstream, structLike: true}, "weight")
		}, 0},
		{"set duplicate text: its bytes", func(_ *testing.T, w *work) {
			normalizerOf(w, 1).shownJSON([]byte(`"abcd"`))
		}, len(`"abcd"`)},
		{"report: path depth", func(_ *testing.T, w *work) {
			normalizerOf(w, 4).report(nil, nil, "x")
		}, 3},
		{"pop: element name and the diagnostics below", func(t *testing.T, w *work) {
			z := normalizerOf(w, 1)
			z.push(step{node: mustParse(t, `{"name": "u"}`), item: true, list: upstreams, index: 0})
			z.push(step{name: "weight"})
			for range 3 {
				z.report(nil, nil, "x")
			}
			before := w.total
			z.pop()
			z.pop()
			if got, want := w.total-before, 1+3; got != want {
				t.Errorf("pop counted %d units, want %d: the name's lookup and the 3 diagnostics", got, want)
			}
		}, 3*2 + 1 + 3},
		// Decode.
		{"appendJSON string", func(_ *testing.T, w *work) { _, _ = appendJSON(nil, mustParse(t, `"abcd"`), nil, w) }, 1 + 4},
		{"appendJSON member name", func(_ *testing.T, w *work) { _, _ = appendJSON(nil, mustParse(t, `{"ab": 1}`), nil, w) }, 1 + 2 + (1 + 1)},
		{"appendJSON nil", func(_ *testing.T, w *work) { _, _ = appendJSON(nil, nil, nil, w) }, 1},
		{"chunker size: nodes up to the limit plus one", func(_ *testing.T, w *work) {
			(&chunker{w: w, limit: 4, bytes: decodeBytes}).size(mustParse(t, `[1, 2, 3, 4, 5, 6, 7, 8, 9, 10]`))
		}, 5},
		{"chunker fold counts: member and name bytes", func(_ *testing.T, w *work) {
			(&chunker{w: w, limit: 4, bytes: decodeBytes}).foldCounts(mustParse(t, `{"ab": 1, "c": 2}`), nil, false)
		}, (1 + 2) + (1 + 1)},
		{"chunker object batches: member names", func(_ *testing.T, w *work) {
			// size of the whole (3 nodes), then per member: its name, its
			// value's size, and its value's text in a batch of one.
			var m map[string]int
			c := &chunker{w: w, limit: 2, bytes: decodeBytes}
			if err := c.decode(mustParse(t, `{"ab": 1, "cd": 2}`), reflect.ValueOf(&m).Elem(), nil); err != nil || len(m) != 2 {
				t.Errorf("decode = %v, %v", m, err)
			}
		}, 3 + 2*(2+1+(1+1))},
		{"chunker large member: name read for the fold", func(_ *testing.T, w *work) {
			// size of the whole (limit+1), the member's name, its size
			// (limit+1), its name again for the fold, the fold counts, and
			// the direct decode: size 2 and text 1+1 per element.
			var v struct {
				L []int `json:"ab"`
			}
			c := &chunker{w: w, limit: 2, bytes: decodeBytes}
			if err := c.decode(mustParse(t, `{"ab": [1, 2]}`), reflect.ValueOf(&v).Elem(), nil); err != nil || len(v.L) != 2 {
				t.Errorf("decode = %v, %v", v, err)
			}
		}, 3 + 2 + 3 + 2 + (1 + 2) + (3 + 2*(1+(1+1)))},
		{"locator integer: literal bytes", func(_ *testing.T, w *work) {
			l := &locator{stack: stack{{}}, w: w, report: func(diag.Path, *tree.Node, string) bool { return true }}
			l.locateAll(reflect.TypeFor[int](), mustParse(t, `12345`), nil)
		}, 1 + 5},
		{"locator unsigned: literal bytes", func(_ *testing.T, w *work) {
			l := &locator{stack: stack{{}}, w: w, report: func(diag.Path, *tree.Node, string) bool { return true }}
			l.locateAll(reflect.TypeFor[uint16](), mustParse(t, `1234`), nil)
		}, 1 + 4},
		{"locator float: literal bytes", func(_ *testing.T, w *work) {
			l := &locator{stack: stack{{}}, w: w, report: func(diag.Path, *tree.Node, string) bool { return true }}
			l.locateAll(reflect.TypeFor[float64](), mustParse(t, `1.25`), nil)
		}, 1 + 4},
		{"stack path: depth and element names", func(_ *testing.T, w *work) {
			s := stack{{}, {name: "spec"}, {name: "upstreams"}, {node: mustParse(t, `{"name": "u"}`), item: true, list: upstreams}}
			s.path(w)
			s.path(w)
		}, 3 + 1 + 3},
		// Materialize.
		{"defaults of a large object: index of the names", func(_ *testing.T, w *work) {
			m := &materializer{w: w}
			n := members(17)
			m.defaults(n, routeUpstream)
			if _, ok := n.Get("weight"); !ok {
				t.Error("no weight default")
			}
		}, 17 + 17*3},
		{"registry defaults: type bytes", func(_ *testing.T, w *work) {
			m := &materializer{w: w}
			res := envelope(mustParse(t, `{"spec": {"type": "`+strings.Repeat("x", 1000)+`"}}`))
			m.registryDefaults(registry.New(), res)
		}, (1 + 4) + (1 + 4) + 1000},
		// Lookups through the finder of a large object.
		{"finder field: index once, then the name's bytes", func(t *testing.T, w *work) {
			f := w.newFinder()
			n := members(17)
			for range 2 {
				if _, ok := f.at(n, diag.Path{diag.Field("k05")}); ok {
					t.Error("unpositioned member found a position")
				}
			}
		}, (1 + 17 + 17*3 + 3) + (1 + 3)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWork()
			tc.run(t, w)
			if w.total != tc.cost {
				t.Errorf("counted %d units, want %d", w.total, tc.cost)
			}
		})
	}
}
