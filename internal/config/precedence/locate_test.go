// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestLocatorWithoutFiles covers a nil files argument (no tree.FileTable):
// positions in the resource's own file take hub.Resource.Source.File, as
// in a file-mode load; the example diagnostics keep their file, line and
// column.
func TestLocatorWithoutFiles(t *testing.T) {
	b, _ := loadBundle(t, shopBundle, "testdata/fixtures/019-exclude")
	_, ds, err := mustNew(t, nil, Options{Body: bodyOracle{}}).Run(t.Context(), b, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	const want = "routes/cart-grpc.yaml:11:7 error RZ-CFG-019 Route/cart-grpc spec.excludePolicies[name=ratelimit-global]: " +
		"cannot exclude a Policy with overridable: false (declared in policies/common.yaml:46:3)\n"
	if got := diagText(t, ds); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestLocatorFallbacks covers the nearest-ancestor and resource-start
// fallbacks: a missing field, a defaulted value (tree.StyleDefaulted
// points at its parent), a node from another file without a FileTable,
// a resource without a tree, canonical content without positions, the
// keyed-path fallback when tree and typed lists disagree (first match for
// a duplicate name, as the keyed path), and an item whose name member has
// no value.
func TestLocatorFallbacks(t *testing.T) {
	const self, other = tree.FileID(0), tree.FileID(1)
	pos := func(f tree.FileID, line, col int32) tree.Pos { return tree.Pos{File: f, Line: line, Column: col} }
	entry := func(name string, p tree.Pos) *tree.Node {
		return &tree.Node{Kind: tree.KindMap, Pos: p, Members: []tree.Member{
			{Key: "name", KeyPos: p, Value: &tree.Node{Kind: tree.KindString, Text: name, Pos: p}},
		}}
	}
	spec := &tree.Node{Kind: tree.KindMap, Pos: pos(self, 5, 3), Members: []tree.Member{
		{Key: "overridable", KeyPos: pos(self, 6, 3), Value: &tree.Node{Kind: tree.KindBool, Style: tree.StyleDefaulted}},
		{Key: "slot", KeyPos: pos(self, 7, 3), Value: &tree.Node{Kind: tree.KindString, Text: "s", Pos: pos(self, 7, 9)}},
		{Key: "policies", KeyPos: pos(self, 8, 3), Value: &tree.Node{Kind: tree.KindList, Pos: pos(self, 9, 5), Items: []*tree.Node{
			entry("a", pos(self, 9, 7)), entry("b", pos(other, 3, 7)), entry("c", tree.Pos{}),
			entry("dup", tree.Pos{}), entry("dup", pos(self, 11, 7)),
		}}},
		{Key: "broken", KeyPos: pos(self, 12, 3)},
	}}
	root := &tree.Node{Kind: tree.KindMap, Pos: pos(self, 1, 1), Members: []tree.Member{{Key: "spec", KeyPos: pos(self, 4, 1), Value: spec}}}
	start := diag.Location{File: "a.yaml", Line: 1, Column: 1}
	r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Tree: root, Source: hub.Source{File: "a.yaml", Start: start}}
	at := func(line, col int) diag.Location { return diag.Location{File: "a.yaml", Line: line, Column: col} }
	field := func(names ...string) diag.Path {
		p := make(diag.Path, len(names))
		for i, n := range names {
			p[i] = diag.Field(n)
		}
		return p
	}

	files := &tree.FileTable{}
	files.Add(tree.File{Path: "a.yaml"})
	files.Add(tree.File{Path: "overlays/prod/r.yaml"})
	l, lf := locator{}, locator{files: files}
	tests := []struct {
		name string
		got  diag.Location
		want diag.Location
	}{
		{"value", l.path(nil, r, field("spec", "slot"), false), at(7, 9)},
		{"key", l.path(nil, r, field("spec", "slot"), true), at(7, 3)},
		{"missing field takes the parent", l.path(nil, r, field("spec", "nope"), false), at(5, 3)},
		{"defaulted value takes the parent", l.path(nil, r, field("spec", "overridable"), true), at(5, 3)},
		{"member without value", l.path(nil, r, field("spec", "broken"), false), at(5, 3)},
		{"keyed entry", l.path(nil, r, entryPath(fieldPolicies, "a"), false), at(9, 7)},
		{"missing keyed entry", l.path(nil, r, entryPath(fieldPolicies, "zz"), false), at(9, 5)},
		{"entry by index", l.list(nil, r, fieldPolicies, nil).entry(0, "a"), at(9, 7)},
		{"entry from another file without a table", l.list(nil, r, fieldPolicies, nil).entry(1, "b"), start},
		{"entry from another file with a table", lf.list(nil, r, fieldPolicies, nil).entry(1, "b"), diag.Location{File: "overlays/prod/r.yaml", Line: 3, Column: 7}},
		{"entry without a position", l.list(nil, r, fieldPolicies, nil).entry(2, "c"), at(9, 5)},
		{"index and name disagree", l.list(nil, r, fieldPolicies, nil).entry(0, "b"), start},
		{"index past the list", l.list(nil, r, fieldPolicies, nil).entry(7, "a"), at(9, 7)},
		{"other list", l.list(nil, r, "excludePolicies", nil).entry(0, "a"), at(5, 3)},
		{"duplicate name by index", l.list(nil, r, fieldPolicies, nil).entry(4, "dup"), at(11, 7)},
		{"duplicate name off the index takes the first, as the keyed path", l.list(nil, r, fieldPolicies, nil).entry(9, "dup"), at(9, 5)},
		{"keyed path of a duplicate name", l.path(nil, r, entryPath(fieldPolicies, "dup"), false), at(9, 5)},
		{"no tree", l.path(nil, &hub.Resource{Source: hub.Source{Start: start}}, field("spec"), false), start},
		{"no resource", l.path(nil, nil, field("spec"), false), diag.Location{}},
		{"entry without tree", l.list(nil, &hub.Resource{Source: hub.Source{Start: start}}, fieldPolicies, nil).entry(0, "a"), start},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, tc.got, tc.want)
		}
	}

	// A name member without a value, which tree.Node.At's keyed search
	// dereferences (a contract request): the by-index check and the name
	// index skip it.
	nilName := &hub.Resource{Tree: listTree(self, 1, []string{"a", "b"}), Source: hub.Source{File: "a.yaml", Start: start}}
	nilSpec, _ := nilName.Tree.Get("spec")
	nilList := nilSpec.Members[0].Value
	nilList.Pos = pos(self, 9, 5)
	nilList.Items[0].Pos = tree.Pos{}
	nilList.Items[1].Members[0].Value = nil
	for _, tc := range []struct {
		i    int
		name string
	}{{1, "b"}, {5, "a"}, {0, "b"}} {
		if got := l.list(nil, nilName, fieldPolicies, nil).entry(tc.i, tc.name); got != at(9, 5) {
			t.Errorf("nil name member: entry(%d, %s) = %+v", tc.i, tc.name, got)
		}
	}

	// A spec or list item that is not a map has no members.
	scalarSpec := &hub.Resource{Tree: &tree.Node{Kind: tree.KindMap, Pos: pos(self, 1, 1), Members: []tree.Member{
		{Key: "spec", Value: &tree.Node{Kind: tree.KindString, Text: "x", Pos: pos(self, 2, 7)}},
	}}, Source: hub.Source{File: "a.yaml", Start: start}}
	if got := l.list(nil, scalarSpec, fieldPolicies, nil).entry(0, "a"); got != at(2, 7) {
		t.Errorf("scalar spec: %+v", got)
	}
	scalarItem := &hub.Resource{Tree: listTree(self, 1, nil), Source: hub.Source{File: "a.yaml", Start: start}}
	scalarSpecNode, _ := scalarItem.Tree.Get("spec")
	scalarSpecNode.Members[0].Value.Pos = pos(self, 9, 5)
	scalarSpecNode.Members[0].Value.Items = []*tree.Node{{Kind: tree.KindString, Text: "a", Pos: pos(self, 10, 7)}}
	if got := l.list(nil, scalarItem, fieldPolicies, nil).entry(0, "a"); got != at(9, 5) {
		t.Errorf("scalar list item: %+v", got)
	}

	// Canonical content: no positions and no file.
	canon := &hub.Resource{Tree: &tree.Node{Kind: tree.KindMap}}
	if got := l.path(nil, canon, field("spec"), false); got != (diag.Location{}) {
		t.Errorf("canonical: %+v", got)
	}
	// A FileTable that does not know the file falls back to the name.
	if got := (locator{files: &tree.FileTable{}}).path(nil, r, field("spec", "slot"), false); got != at(7, 9) {
		t.Errorf("unknown file ID: %+v", got)
	}
}

// positionlessList returns a Route resource whose spec.policies tree lists
// names with no item positions (the list and spec keep theirs), the shape
// of the WP-38 S2 round-2 minor on the locator's keyed fallback.
func positionlessList(names []string) *hub.Resource {
	return &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Tree: positionless(fieldPolicies, names), Source: hub.Source{File: "a.yaml"}}
}

// positionless returns an envelope tree whose list spec.<field> names
// names, with positions on the envelope, spec and the list (line 9,
// column 5) but none on the items.
func positionless(field string, names []string) *tree.Node {
	tr := listTree(1, 1, names)
	spec, _ := tr.Get("spec")
	spec.Members[0].Key = field
	spec.Members[0].Value.Pos = tree.Pos{File: 1, Line: 9, Column: 5}
	for _, it := range spec.Members[0].Value.Items {
		it.Pos = tree.Pos{}
	}
	return tr
}

// TestListLocLinear covers the WP-38 S2 round-2 minor that locating every
// entry of a list whose items have no position (or whose names disagree
// with the typed list) ran a keyed search per entry, quadratic in the
// list's length, and the class: every fallback of listLoc.entry, and the
// round-3 minor that this test missed a fallback through a keyed path
// (mutation M23), because the count covered listLoc's own reads only.
// Every item the locator reads, by listLoc or by locator.path's index and
// keyed steps, is a work unit of its worker, and the count is exact: one
// read per entry by index, one per item for the name index built once per
// list, and one for the index step of the fallback path, so locating all n
// entries reads at most 3n items, and the results equal the keyed path's
// (TestLocatorFallbacks).
func TestListLocLinear(t *testing.T) {
	names := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("p%05d", i)
		}
		return out
	}
	listAt := diag.Location{File: "a.yaml", Line: 9, Column: 5}
	for _, n := range []int{1, 1000, 4000} {
		ns := names(n)
		tests := []struct {
			name   string
			r      *hub.Resource
			lookup func(i int) (int, string)
			reads  int
			want   func(i int) diag.Location
		}{
			{
				// One read by index per entry.
				"positioned items, by index", &hub.Resource{Tree: listTree(1, 1, ns), Source: hub.Source{File: "a.yaml"}},
				func(i int) (int, string) { return i, ns[i] }, n,
				func(i int) diag.Location { return diag.Location{File: "a.yaml", Line: 10 + i, Column: 7} },
			},
			{
				// By index, the name index once, then the fallback's
				// index step: 2n + n.
				"items without positions", positionlessList(ns),
				func(i int) (int, string) { return i, ns[i] }, 3 * n,
				func(int) diag.Location { return listAt },
			},
			{
				// As above; with one item the reversed name is the item's
				// own, so one read by index locates it.
				"names disagree with the index", &hub.Resource{Tree: listTree(1, 1, ns), Source: hub.Source{File: "a.yaml"}},
				// n is 1 or even: else no item keeps its name.
				func(i int) (int, string) { return i, ns[n-1-i] }, map[bool]int{true: 1, false: 3 * n}[n == 1],
				func(i int) diag.Location { return diag.Location{File: "a.yaml", Line: 10 + n - 1 - i, Column: 7} },
			},
			{
				// No read by index, the name index once, the fallback's
				// index step: n + n.
				"index past the list", &hub.Resource{Tree: listTree(1, 1, ns), Source: hub.Source{File: "a.yaml"}},
				func(i int) (int, string) { return n + i, ns[i] }, 2 * n,
				func(i int) diag.Location { return diag.Location{File: "a.yaml", Line: 10 + i, Column: 7} },
			},
			{
				// By index, the name index once, and a fallback path that
				// stops at the list: n + n.
				"names the list lacks", positionlessList(ns),
				func(i int) (int, string) { return i, "absent" }, 2 * n,
				func(int) diag.Location { return listAt },
			},
		}
		for _, tc := range tests {
			w := &worker{}
			ll := locator{}.list(w, tc.r, fieldPolicies, nil)
			for i := range n {
				idx, name := tc.lookup(i)
				if got, want := ll.entry(idx, name), tc.want(i); got != want {
					t.Fatalf("n=%d %s: entry(%d, %s) = %+v, want %+v", n, tc.name, idx, name, got, want)
				}
			}
			if w.total != tc.reads {
				t.Errorf("n=%d %s: %d items read, want %d", n, tc.name, w.total, tc.reads)
			}
		}
	}
}

// TestLocatorPathReads pins what locator.path counts: one unit per index
// step, one per item a keyed step compares (the M23 shape, a keyed search
// per entry, counts every item before the match), none per field step,
// and no dereference of a key member without a value.
func TestLocatorPathReads(t *testing.T) {
	r := &hub.Resource{Tree: listTree(1, 1, []string{"a", "b", "c", "d"}), Source: hub.Source{File: "a.yaml"}}
	spec, _ := r.Tree.Get("spec")
	spec.Members[0].Value.Items[0].Members[0].Value = nil
	tests := []struct {
		name  string
		p     diag.Path
		reads int
		want  diag.Location
	}{
		{"fields only", diag.Path{diag.Field("spec"), diag.Field(fieldPolicies)}, 0, diag.Location{File: "a.yaml", Line: 5, Column: 3}},
		{"index step", diag.Path{diag.Field("spec"), diag.Field(fieldPolicies), diag.Index(3)}, 1, diag.Location{File: "a.yaml", Line: 13, Column: 7}},
		{"index past the list", diag.Path{diag.Field("spec"), diag.Field(fieldPolicies), diag.Index(9)}, 0, diag.Location{File: "a.yaml", Line: 5, Column: 3}},
		{"keyed step reads up to the match", entryPath(fieldPolicies, "c"), 3, diag.Location{File: "a.yaml", Line: 12, Column: 7}},
		{"keyed step without a match reads every item", entryPath(fieldPolicies, "zz"), 4, diag.Location{File: "a.yaml", Line: 5, Column: 3}},
		{"keyed step on a map", diag.Path{diag.Field("spec"), diag.Keyed("name", "a")}, 0, diag.Location{File: "a.yaml", Line: 5, Column: 3}},
		{"item step", diag.Path{diag.Field("spec"), diag.Field(fieldPolicies), diag.Item("x")}, 4, diag.Location{File: "a.yaml", Line: 5, Column: 3}},
	}
	for _, tc := range tests {
		w := &worker{}
		if got := (locator{}).path(w, r, tc.p, false); got != tc.want || w.total != tc.reads {
			t.Errorf("%s: path = %+v with %d reads, want %+v with %d", tc.name, got, w.total, tc.want, tc.reads)
		}
	}
}

// TestResolvePositionlessListLinear is TestListLocLinear through Resolve,
// for each list stage I locates entries of: doubling a Route spec.policies
// list, or a Route spec.excludePolicies list naming locked Gateway
// Policies (one RZ-CFG-019 each), whose items have no position at most
// doubles the heap allocations and bytes of a one-worker run (margin 1.5),
// so stage I builds the name index once per list, not once per entry.
func TestResolvePositionlessListLinear(t *testing.T) {
	bundle := func(field string, n int) *hub.Bundle {
		b := newBuilder()
		ns := make([]string, n)
		for i := range ns {
			ns[i] = fmt.Sprintf("p%05d", i)
			if field == fieldExclude {
				b.policy(ns[i], v1alpha1.PolicyTypeRateLimit, nil, locked)
			} else {
				b.policy(ns[i], v1alpha1.PolicyTypeRateLimit, nil)
			}
		}
		if field == fieldExclude {
			b.gateway(ns...)
			b.route("r", nil, ns, nil)
		} else {
			b.route("r", ns, nil, nil)
		}
		b.rs[len(b.rs)-1].Tree = positionless(field, ns)
		return b.bundle()
	}
	const n, margin = 500, 1.5
	for _, field := range []string{fieldPolicies, fieldExclude} {
		cost := func(n int) (uint64, uint64) {
			bb := bundle(field, n)
			r := mustNew(t, nil, Options{})
			run := func() {
				_, ds, err := r.Run(t.Context(), bb, nil, 1)
				if want := map[string]int{fieldPolicies: 0, fieldExclude: n}[field]; err != nil || len(ds) != want {
					t.Fatalf("%s: Run: %v, %d diagnostics, want %d", field, err, len(ds), want)
				}
			}
			run()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			run()
			runtime.ReadMemStats(&after)
			return after.Mallocs - before.Mallocs, after.TotalAlloc - before.TotalAlloc
		}
		baseA, baseB := cost(n)
		a, b := cost(2 * n)
		if float64(a) > 2*margin*float64(baseA) || float64(b) > 2*margin*float64(baseB) {
			t.Errorf("%s doubled: %d allocations and %d bytes, base %d and %d: growth above linear", field, a, b, baseA, baseB)
		}
	}
}
