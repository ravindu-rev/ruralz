// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// pathElems are path elements whose texts exercise the comparison: bare
// fields that are prefixes of each other or of a separator, quoted fields,
// indexes, keyed entries and set items of every value kind, and long
// segments with long common prefixes (memoized prefixes, 01 req 48).
func pathElems() []diag.PathElem {
	long := strings.Repeat("k", 2*lcpMin)
	return []diag.PathElem{
		diag.Field("a"), diag.Field("ab"), diag.Field("a-"), diag.Field("a_"), diag.Field("A"), diag.Field("$a"),
		diag.Field("a.b"), diag.Field(""), diag.Field("é"), diag.Field("1a"),
		diag.Field(long), diag.Field(long + "x"), diag.Field(long + "-"),
		diag.Index(0), diag.Index(2), diag.Index(10),
		diag.Keyed("name", "a"), diag.Keyed("name", "a b"), diag.Keyed("name", "a]"), diag.Keyed("id", "a"),
		diag.Keyed("name", long), diag.Keyed("name", long+"x"), diag.Keyed("name", long+" x"),
		diag.Item("x"), diag.Item("1"), diag.Item(json.Number("1")), diag.Item(json.Number("12")), diag.Item(true),
		diag.Item(json.RawMessage(`{"a":1}`)), diag.Item(json.RawMessage(`{"a":1,"b":[1,2]}`)), diag.Item(json.RawMessage(`[1]`)),
		diag.Item(json.RawMessage(`{"a":"` + long + `"}`)), diag.Item(json.RawMessage(`{"a":"` + long + `x"}`)),
	}
}

// testPaths returns every path of one and two elements of pathElems, and
// a fixed sample of deeper ones.
func testPaths() []diag.Path {
	elems := pathElems()
	var out []diag.Path
	for _, a := range elems {
		out = append(out, diag.Path{a})
		for _, b := range elems {
			out = append(out, diag.Path{a, b})
		}
	}
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: a fixed, reproducible sample.
	for range 400 {
		p := make(diag.Path, 3+r.IntN(4))
		for i := range p {
			p[i] = elems[r.IntN(len(elems))]
		}
		out = append(out, p, slices.Concat(p, diag.Path{elems[r.IntN(len(elems))]}))
	}
	return out
}

// build returns the path node of p, built element by element.
func (x *memo) build(p diag.Path) *pathNode {
	var n *pathNode
	for _, e := range p {
		n = x.child(n, e)
	}
	return n
}

// segmentsOf returns the concatenated segment texts of t.
func segmentsOf(t *textNode) string {
	if t == nil {
		return ""
	}
	return segmentsOf(t.parent) + t.text
}

// TestComparePaths checks that path nodes render, deduplicate and compare
// exactly as their Path.String texts do: the segments concatenate to
// Path.String, equal texts share one text node, and comparePaths has the
// sign of strings.Compare on the texts (01 reqs 48 and 50: diag.List.Sort
// order without rendering any path).
func TestComparePaths(t *testing.T) {
	paths := testPaths()
	x := &memo{}
	nodes := make([]*pathNode, len(paths))
	texts := make([]string, len(paths))
	for i, p := range paths {
		nodes[i] = x.build(p)
		texts[i] = p.String()
		if got := segmentsOf(nodes[i].text); got != texts[i] {
			t.Fatalf("segments of %q = %q", texts[i], got)
		}
		if got := nodes[i].path(); !slices.EqualFunc(got, p, samePathElem) {
			t.Fatalf("path() of %q = %q", texts[i], got.String())
		}
	}
	for i := range paths {
		for j := range paths {
			want := strings.Compare(texts[i], texts[j])
			if got := x.comparePaths(nodes[i].text, nodes[j].text); got != want {
				t.Fatalf("comparePaths(%q, %q) = %d, want %d", texts[i], texts[j], got, want)
			}
			if same := nodes[i].text == nodes[j].text; same != (want == 0) {
				t.Fatalf("text nodes of %q and %q shared = %v, want %v", texts[i], texts[j], same, want == 0)
			}
		}
	}
	if len(x.lcp) == 0 {
		t.Error("no common prefix of long segments was memoized")
	}
}

// samePathElem compares path elements, items by their JSON text.
func samePathElem(a, b diag.PathElem) bool {
	return diag.Path{a}.String() == diag.Path{b}.String() && a.Kind == b.Kind
}

// TestPathHelpers covers the empty path and the elements below an
// ancestor.
func TestPathHelpers(t *testing.T) {
	x := &memo{}
	var root *pathNode
	if root.path() != nil || root.depth() != 0 || root.textOf() != nil || root.isField("metadata") {
		t.Error("the empty path is not empty")
	}
	if c := x.comparePaths(nil, nil); c != 0 {
		t.Errorf("comparePaths(nil, nil) = %d", c)
	}
	meta := x.build(diag.Path{diag.Field("metadata")})
	if !meta.isField("metadata") || meta.isField("spec") {
		t.Error("isField(metadata) is wrong")
	}
	deep := x.build(diag.Path{diag.Field("metadata"), diag.Field("a"), diag.Index(1), diag.Field("b"), diag.Field("c"), diag.Field("d")})
	var buf [2]diag.PathElem
	if got := deep.appendBelow(buf[:0], meta).String(); got != "a[1].b.c.d" {
		t.Errorf("appendBelow(metadata) = %q, want a[1].b.c.d", got)
	}
	if got := meta.appendBelow(nil, deep); got != nil {
		t.Errorf("appendBelow(descendant) = %q, want nothing", got.String())
	}
	if c := x.comparePaths(nil, meta.text); c >= 0 {
		t.Errorf("comparePaths(empty, metadata) = %d, want < 0", c)
	}
}

// TestCompareStreams covers stream ends and segments split differently.
func TestCompareStreams(t *testing.T) {
	for _, tc := range []struct {
		a, b []string
		want int
	}{
		{nil, nil, 0},
		{[]string{"", "a"}, []string{"a", ""}, 0},
		{[]string{"ab", "c"}, []string{"a", "bc"}, 0},
		{[]string{"ab"}, []string{"a", "bc"}, -1},
		{[]string{"b"}, []string{"a", "bc"}, 1},
		{[]string{"a"}, nil, 1},
	} {
		got := compareStreams(&stream{rest: tc.a}, &stream{rest: tc.b})
		if got != tc.want {
			t.Errorf("compareStreams(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	if got := commonPrefix(strings.Repeat("x", 200)+"a", strings.Repeat("x", 200)+"b"); got != 200 {
		t.Errorf("commonPrefix() = %d, want 200", got)
	}
}
