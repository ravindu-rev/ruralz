// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// hostnamesSchema returns the schema of a Gateway listener's hostnames, a
// set of strings.
func hostnamesSchema(t *testing.T, v *View) *schemaidx.Node {
	t.Helper()
	n, ok := v.idx.Resource("Gateway")
	for _, name := range []string{"spec", "listeners", "", "hostnames"} {
		if !ok {
			t.Fatalf("no schema at %q", name)
		}
		if name == "" {
			n, ok = n.Items()
			continue
		}
		n, ok = n.Property(name)
	}
	if lt, _ := n.List(); !ok || lt != schemaidx.ListSet {
		t.Fatal("Gateway listener hostnames is not a set")
	}
	return n
}

// counting returns a memo whose work counts units.
func counting() *memo { return &memo{work: &work{yield: func() {}}} }

// TestTextCharges checks, for every span of stage F whose work grows with
// the length of one text, that the units it charges grow with that
// length: each case is run with an n-byte and a 2n-byte text, and the
// charge must grow by exactly growth(n) units. The cases cover a keyed
// key, field names, number and string set elements, the Walk's list
// elements, a duplicate key's canonical JSON, the scan of two long
// siblings and long member names, tokens, variable names and strings:
// charging any of them one unit or none would let a 16 MiB name run
// without a yield. quote reads a bounded prefix, so a long unknown
// member's message grows the charge only by its member lookup and its
// path segment. A growth of k*n is a span that reads the text k times: a
// duplicate key's canonical JSON for both entries and the duplicate's path
// segment, a reserved label key's member name in the Walk, its lookup for
// an earlier duplicate and its path segment.
func TestTextCharges(t *testing.T) {
	v := testView(t)
	set := hostnamesSchema(t, v)
	a := func(n int) string { return strings.Repeat("a", n) }
	digits := func(n int) *tree.Node { return &tree.Node{Kind: tree.KindInt, Text: "1" + strings.Repeat("0", n-1)} }
	str := func(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }
	// segment interns e below a one-element path and returns the units.
	segment := func(e diag.PathElem) int {
		x := counting()
		parent := x.intern(nil, diag.Field("spec"), nil)
		x.work.total = 0
		x.intern(parent, e, nil)
		return x.work.total
	}
	// checked runs the uniqueness and reserved-prefix checks on src.
	checked := func(src string) int {
		res := mustResource(t, &tree.FileTable{}, "r.json", src)
		m := newMapper(v, res, nil, MaxDiagnostics, Options{Yield: func() {}})
		m.checkLists()
		m.checkReserved()
		return m.work.total
	}
	for _, tc := range []struct {
		name   string
		units  func(n int) int
		growth func(n int) int
	}{
		{"keyed entry key", func(n int) int { return segment(diag.Keyed("name", a(n))) }, linear},
		{"bare field name", func(n int) int { return segment(diag.Field(a(n))) }, linear},
		{"quoted field name", func(n int) int { return segment(diag.Field(strings.Repeat("a ", n/2))) }, linear},
		{"field name at depth 0", func(n int) int {
			x := counting()
			x.intern(nil, diag.Field(a(n)), nil)
			return x.work.total
		}, linear},
		{"string set element segment", func(n int) int { return segment(diag.Item(a(n))) }, linear},
		{"number set element", func(n int) int {
			x := counting()
			x.itemElem(set, digits(n), 0)
			return x.work.total
		}, linear},
		{"object set element", func(n int) int {
			x := counting()
			x.itemElem(set, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "k", Value: str(a(n))}}}, 0)
			return x.work.total
		}, linear},
		{"two long siblings", func(n int) int {
			x := counting()
			x.commonPrefix(&textNode{text: a(n) + "1", depth: 1}, &textNode{text: a(n) + "2", depth: 1})
			return x.work.total
		}, linear},
		{"number list element in the Walk", func(n int) int { return walkUnits(digits(n), schemaidx.ItemElem(set, digits(n), 0)) }, linear},
		{"object list element in the Walk", func(n int) int {
			obj := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "k", Value: digits(n)}}}
			return walkUnits(obj, schemaidx.ItemElem(set, obj, 0))
		}, func(n int) int { return 2 * n }},
		{"member name in the Walk", func(n int) int { return walkUnits(str("v"), diag.Field(a(n))) }, linear},
		{"duplicate keyed-list key", func(n int) int {
			return checked(envelope("Gateway", `{"listeners": [{"name": "`+a(n)+`", "port": 1, "protocol": "http"}, {"name": "`+a(n)+`", "port": 2, "protocol": "http"}]}`))
		}, func(n int) int { return 3 * n }},
		{"duplicate set element", func(n int) int {
			return checked(envelope("Gateway", `{"listeners": [{"name": "l", "port": 1, "protocol": "http", "hostnames": ["`+a(n)+`", "`+a(n)+`"]}]}`))
		}, func(n int) int { return 3 * n }},
		{"duplicate number set element", func(n int) int {
			lit := digits(n).Text
			return checked(envelope("Gateway", `{"listeners": [{"name": "l", "port": 1, "protocol": "http", "hostnames": [`+lit+`, `+lit+`]}]}`))
		}, func(n int) int { return 8 * n }},
		{"reserved label key", func(n int) int {
			return checked(`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "labels": {"ruralz.io/` + a(n) + `": "v"}}, "spec": {}}`)
		}, func(n int) int { return 3 * n }},
		{"member name in the instance", func(n int) int {
			w, _ := counter()
			w.instance(&tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: a(n), Value: str("v")}}}, v.nums)
			return w.total
		}, linear},
		{"member name and string in the validator's span", func(n int) int {
			res := mustResource(t, &tree.FileTable{}, "r.json", envelope("Gateway", `{"listeners": [{"name": "`+a(n)+`", "port": 1, "protocol": "http"}], "`+a(n)+`": 1}`))
			w, _ := counter()
			_, _, _ = v.runSchema(res, w)
			return w.total
		}, func(n int) int { return 2*n + n }},
		{"token looked up", func(n int) int {
			x := counting()
			s := &step{}
			s.add(&step{tok: a(n)})
			x.step(s, a(n))
			return x.work.total
		}, linear},
		// A scan compares every child of the token's length in full, so
		// eight such siblings are charged eight lengths, not one.
		{"token among equal-length siblings", func(n int) int {
			x := counting()
			s := &step{}
			for i := range fewSteps {
				s.add(&step{tok: a(n-1) + strconv.Itoa(i)})
			}
			x.step(s, a(n-1)+strconv.Itoa(fewSteps-1))
			return x.work.total
		}, func(n int) int { return fewSteps * n }},
		// A new member step reads its token in the schema's members, the
		// object's member it compares and its path segment.
		{"token of a new member step", func(n int) int {
			res := mustResource(t, &tree.FileTable{}, "r.json", envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "`+a(n)+`": 1}`))
			m := newMapper(v, res, nil, MaxDiagnostics, Options{Yield: func() {}})
			m.locate([]string{"spec"})
			m.work.total = 0
			m.locate([]string{"spec", a(n)})
			return m.work.total
		}, func(n int) int { return 3 * n }},
		{"member name below a secret", func(n int) int {
			m := newMapper(v, mustResource(t, &tree.FileTable{}, "r.json", `{}`), nil, MaxDiagnostics, Options{Yield: func() {}})
			spec, _ := v.idx.Spec("Consumer")
			m.declaredPath(spec, diag.Path{diag.Field(a(n))})
			return m.work.total
		}, linear},
		{"member index of a large object", func(n int) int {
			x := counting()
			obj := &tree.Node{Kind: tree.KindMap}
			for i := range memberIndexMin {
				obj.Members = append(obj.Members, tree.Member{Key: string(rune('b' + i))})
			}
			obj.Members = append(obj.Members, tree.Member{Key: a(n)})
			x.index(obj, a(n))
			return x.work.total
		}, func(n int) int { return 2 * n }},
		{"member scan of a small object", func(n int) int {
			x := counting()
			x.index(&tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "b" + a(n-1)}, {Key: a(n)}}}, a(n))
			return x.work.total
		}, func(n int) int { return 2 * n }},
		{"sorted unknown names", func(n int) int {
			m := newMapper(v, mustResource(t, &tree.FileTable{}, "r.json", `{}`), nil, MaxDiagnostics, Options{Yield: func() {}})
			m.sortedNames([]string{a(n) + "2", a(n) + "1"})
			return m.work.total
		}, linear},
		{"variable name", func(n int) int {
			m := newMapper(v, mustResource(t, &tree.FileTable{}, "r.json", `{}`), nil, MaxDiagnostics, Options{Yield: func() {}})
			m.varsBelow(&tree.Node{Kind: tree.KindString, Vars: []string{a(n)}})
			return m.work.total
		}, linear},
		{"unknown member's message", func(n int) int {
			res := mustResource(t, &tree.FileTable{}, "r.json", envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "`+a(n)+`": 1}`))
			m := newMapper(v, res, nil, MaxDiagnostics, Options{Yield: func() {}})
			p := m.locate([]string{"spec"})
			m.memo.index(p.node, "")
			m.work.total = 0
			m.unknown(p, []string{a(n)})
			return m.work.total
		}, func(n int) int { return 2 * n }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const n = 4 << 10
			u1, u2 := tc.units(n), tc.units(2*n)
			if got, want := u2-u1, tc.growth(n); got != want {
				t.Errorf("charged %d units for %d bytes and %d for %d: growth %d, want %d", u1, n, u2, 2*n, got, want)
			}
		})
	}
}

func linear(n int) int { return n }
