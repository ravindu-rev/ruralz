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
// segments with long common prefixes, below lcpMemo (remembered per
// segment) and above it (memoized pairs).
func pathElems() []diag.PathElem {
	long := strings.Repeat("k", 2*lcpMin)
	huge := strings.Repeat("h", lcpMemo+lcpMin)
	return []diag.PathElem{
		diag.Field(huge), diag.Field(huge + "x"), diag.Keyed("name", huge), diag.Keyed("name", huge+" x"),
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
// sign of strings.Compare on the texts (diag.List.Sort order without
// rendering any path).
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
	for at, n := range x.texts {
		if n.lcp != nil && n.lcp.peer == nil {
			t.Errorf("segment %.20q has a cache without a peer", at.text)
		}
	}
}

// TestCommonPrefixMemo checks how the common prefix of two sibling
// segments is remembered, at and around the lcpMin and lcpMemo
// boundaries: a pair with a segment shorter than lcpMin is scanned per
// call and neither counted nor cached; a long pair is scanned once,
// counted in memo.scanned (the prefix and the first differing byte) and
// answered again from either segment's last comparison in either order;
// after both segments were compared with a third one, a pair whose prefix
// reaches lcpMemo is answered from memo.lcp without a scan, and any other
// pair is scanned again; memo.lcp holds exactly the pairs whose prefix
// reaches lcpMemo. Losing the memo would cost errors times segment
// length, and one entry per long pair compared would cost more than the
// scans it saves.
func TestCommonPrefixMemo(t *testing.T) {
	long := strings.Repeat("k", 2*lcpMin)
	at := strings.Repeat("k", lcpMin-1)
	m := func(n int) string { return strings.Repeat("m", n) }
	for _, tc := range []struct {
		name string
		a, b string
		// want is the common prefix length; scanned the bytes counted by
		// the first comparison; memoized whether the pair is in memo.lcp.
		want, scanned int
		memoized      bool
	}{
		{"short pair", "[name=a]", "[name=b]", 6, 0, false},
		{"short and long", ".a", ".a" + long, 2, 0, false},
		{"long and short", ".a" + long, ".a", 2, 0, false},
		{"one byte under lcpMin", at[1:] + "a", at + "b", lcpMin - 2, 0, false},
		{"both lcpMin bytes", at + "a", at + "b", lcpMin - 1, lcpMin, false},
		{"long pair", "[name=" + long + "a]", "[name=" + long + "b]", 6 + len(long), 7 + len(long), false},
		{"long pair, one a prefix", "." + long, "." + long + "x", 1 + len(long), 1 + len(long), false},
		{"long pair, nothing shared", "a" + long, "b" + long, 0, 1, false},
		{"prefix one byte under lcpMemo", m(lcpMemo-1) + "a", m(lcpMemo-1) + "b", lcpMemo - 1, lcpMemo, false},
		{"prefix of lcpMemo bytes", m(lcpMemo) + "a", m(lcpMemo) + "b", lcpMemo, lcpMemo + 1, true},
		{"lcpMemo bytes, a prefix of the other", m(lcpMemo), m(lcpMemo) + "x", lcpMemo, lcpMemo, true},
		{"long prefix", m(64<<10) + "a]", m(64<<10) + "b]", 64 << 10, 64<<10 + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := &memo{}
			a, b := &textNode{text: tc.a, depth: 1}, &textNode{text: tc.b, depth: 1}
			for i, pair := range [][2]*textNode{{a, b}, {a, b}, {b, a}} {
				if got := x.commonPrefix(pair[0], pair[1]); got != tc.want {
					t.Fatalf("call %d: commonPrefix() = %d, want %d", i, got, tc.want)
				}
				if x.scanned != tc.scanned {
					t.Fatalf("call %d: scanned = %d bytes, want %d", i, x.scanned, tc.scanned)
				}
			}
			if tc.scanned == 0 && (a.lcp != nil || b.lcp != nil) {
				t.Error("a short pair allocated a cache")
			}
			// A third long segment, differing from both at the first byte,
			// becomes both segments' last comparison.
			c := &textNode{text: strings.Repeat("\x01", lcpMin), depth: 1}
			x.commonPrefix(a, c)
			x.commonPrefix(c, b)
			before := x.scanned
			if got := x.commonPrefix(b, a); got != tc.want {
				t.Fatalf("after other comparisons: commonPrefix() = %d, want %d", got, tc.want)
			}
			rescanned := x.scanned - before
			if want := tc.scanned; tc.memoized {
				want = 0
				if rescanned != want {
					t.Errorf("a memoized pair rescanned %d bytes", rescanned)
				}
			} else if rescanned != want {
				t.Errorf("rescanned %d bytes, want %d", rescanned, want)
			}
			if got := len(x.lcp); got != boolInt(tc.memoized) {
				t.Errorf("memoized pairs = %d, want %d", got, boolInt(tc.memoized))
			}
		})
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
