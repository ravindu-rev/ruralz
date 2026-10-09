// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"cmp"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// pathNode is one key-aware path (01 req 48): its last element, the
// path above it and the interned text of the whole path. Every position
// at or below a path shares its node, so a large element, such as a keyed
// entry's key or a set element's canonical JSON, is held and rendered
// once, never once per diagnostic. nil is the empty path.
type pathNode struct {
	parent *pathNode
	elem   diag.PathElem
	text   *textNode
}

// textNode is the Path.String text of a path as the chain of the texts
// its elements add, interned per Validate call by (parent, text): two
// paths render the same text exactly when they have the same textNode,
// because the 01 req 48 grammar renders every element self-delimited (a
// bracketed form closed by "]" outside JSON strings, or "." and a bare
// field name). Diagnostics are deduplicated and compared by their
// textNode, never by a rendered path. nil is the empty text.
type textNode struct {
	parent *textNode
	// text is what the element adds: the element with its separator,
	// such as ".spec", "[name=a]" or "[item={...}]".
	text  string
	depth int
}

// textAt is the interning key of a textNode.
type textAt struct {
	parent *textNode
	text   string
}

// lcpMin is the segment length from which the common prefix of two
// distinct segments is memoized: comparing two long segments with a long
// common prefix once per diagnostic pair would cost their length per
// comparison.
const lcpMin = 64

// textPair is a pair of distinct sibling textNodes.
type textPair struct {
	a, b *textNode
}

// depthOf returns the number of elements of a path text.
func depthOf(t *textNode) int {
	if t == nil {
		return 0
	}
	return t.depth
}

// textOf returns the text of p; nil for the empty path.
func (p *pathNode) textOf() *textNode {
	if p == nil {
		return nil
	}
	return p.text
}

// depth returns the number of elements of p.
func (p *pathNode) depth() int { return depthOf(p.textOf()) }

// isField reports whether p is the one-element path of field name.
func (p *pathNode) isField(name string) bool {
	return p != nil && p.parent == nil && p.elem.Kind == diag.ElemField && p.elem.Name == name
}

// path returns p as a diag.Path; nil for the empty path.
func (p *pathNode) path() diag.Path {
	if p == nil {
		return nil
	}
	return p.appendBelow(make(diag.Path, 0, p.depth()), nil)
}

// appendBelow appends to dst the elements of p under its ancestor top
// (nil for the root), in path order.
func (p *pathNode) appendBelow(dst diag.Path, top *pathNode) diag.Path {
	n := p.depth() - top.depth()
	if n <= 0 {
		return dst
	}
	start := len(dst)
	dst = slices.Grow(dst, n)[:start+n]
	for i := start + n - 1; i >= start; i-- {
		dst[i] = p.elem
		p = p.parent
	}
	return dst
}

// segmentText returns the text element e adds to Path.String at depth
// (its index in the path), rendered by diag itself so the concatenated
// segments of a path always equal its Path.String: "_" is a bare field
// that renders as itself, so what follows it is e with its separator.
func segmentText(e diag.PathElem, depth int) string {
	if depth == 0 {
		return diag.Path{e}.String()
	}
	return diag.Path{diag.Field("_"), e}.String()[1:]
}

// child returns the path parent extended by e. The element's text is
// rendered here, once per call; callers memoize per instance position.
func (x *memo) child(parent *pathNode, e diag.PathElem) *pathNode {
	return &pathNode{parent: parent, elem: e, text: x.intern(parent.textOf(), e, nil)}
}

// intern returns the text node of parent extended by e, storing a new one
// in spare when spare is not nil.
func (x *memo) intern(parent *textNode, e diag.PathElem, spare *textNode) *textNode {
	at := textAt{parent: parent, text: segmentText(e, depthOf(parent))}
	if t, ok := x.texts[at]; ok {
		return t
	}
	if spare == nil {
		spare = new(textNode)
	}
	*spare = textNode{parent: parent, text: at.text, depth: depthOf(parent) + 1}
	if x.texts == nil {
		x.texts = map[textAt]*textNode{}
	}
	x.texts[at] = spare
	return spare
}

// comparePaths compares the Path.String texts of two paths as
// cmp.Compare compares strings, without rendering them: shared ancestors
// are skipped by identity, and the comparison starts at the first
// segments that differ.
func (x *memo) comparePaths(a, b *textNode) int {
	if a == b {
		return 0
	}
	ua, ub := a, b
	for depthOf(ua) > depthOf(ub) {
		ua = ua.parent
	}
	for depthOf(ub) > depthOf(ua) {
		ub = ub.parent
	}
	if ua == ub {
		// One text is a prefix of the other; every segment is non-empty.
		return cmp.Compare(depthOf(a), depthOf(b))
	}
	for ua.parent != ub.parent {
		ua, ub = ua.parent, ub.parent
	}
	// ua and ub are siblings, so their segments differ.
	n := x.commonPrefix(ua, ub)
	if n < len(ua.text) && n < len(ub.text) {
		return cmp.Compare(ua.text[n], ub.text[n])
	}
	// One segment is a prefix of the other: the segments below decide.
	return compareStreams(
		&stream{cur: ua.text[n:], rest: segmentsBelow(ua, a)},
		&stream{cur: ub.text[n:], rest: segmentsBelow(ub, b)},
	)
}

// commonPrefix returns the length of the common prefix of two sibling
// segments, memoized for long ones.
func (x *memo) commonPrefix(a, b *textNode) int {
	if len(a.text) < lcpMin || len(b.text) < lcpMin {
		return commonPrefix(a.text, b.text)
	}
	if n, ok := x.lcp[textPair{a, b}]; ok {
		return n
	}
	if n, ok := x.lcp[textPair{b, a}]; ok {
		return n
	}
	n := commonPrefix(a.text, b.text)
	if x.lcp == nil {
		x.lcp = map[textPair]int{}
	}
	x.lcp[textPair{a, b}] = n
	return n
}

// commonPrefix returns the length of the common prefix of a and b.
func commonPrefix(a, b string) int {
	const block = 64
	n := min(len(a), len(b))
	i := 0
	for i+block <= n && a[i:i+block] == b[i:i+block] {
		i += block
	}
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// segmentsBelow returns the segment texts of t below its ancestor top,
// in path order.
func segmentsBelow(top, t *textNode) []string {
	out := make([]string, depthOf(t)-depthOf(top))
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = t.text
		t = t.parent
	}
	return out
}

// stream is the rest of a path text: cur, then rest.
type stream struct {
	cur  string
	rest []string
}

// fill makes cur non-empty unless the stream has ended.
func (s *stream) fill() bool {
	for s.cur == "" {
		if len(s.rest) == 0 {
			return false
		}
		s.cur, s.rest = s.rest[0], s.rest[1:]
	}
	return true
}

// compareStreams compares two text streams as cmp.Compare compares the
// strings they spell.
func compareStreams(a, b *stream) int {
	for {
		okA, okB := a.fill(), b.fill()
		if !okA || !okB {
			return cmp.Compare(boolInt(okA), boolInt(okB))
		}
		n := min(len(a.cur), len(b.cur))
		if c := strings.Compare(a.cur[:n], b.cur[:n]); c != 0 {
			return c
		}
		a.cur, b.cur = a.cur[n:], b.cur[n:]
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
