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
	// lcp is what commonPrefix remembers about a segment of at least
	// lcpMin bytes, allocated on its first such comparison; nil otherwise.
	lcp *lcpCache
}

// lcpCache is what commonPrefix remembers about one long segment: the
// sibling it was last compared with and their common prefix length, and
// whether the segment is in a pair of memo.lcp.
type lcpCache struct {
	peer     *textNode
	n        int
	memoized bool
}

// textAt is the interning key of a textNode.
type textAt struct {
	parent *textNode
	text   string
}

// lcpMin is the segment length from which the common prefix of two
// distinct sibling segments is remembered: comparing two long segments
// with a long common prefix once per diagnostic pair would cost their
// length per comparison. A pair with a shorter segment is scanned per
// comparison, reading fewer than lcpMin bytes.
const lcpMin = 64

// lcpMemo is the common prefix length from which a pair of sibling
// segments is memoized in memo.lcp, a map entry of a few dozen bytes that
// a pair's first comparison pays: below it, a comparison that is not the
// pair's last on either segment (lcpCache.peer) rescans the prefix, at
// most lcpMemo bytes, about what the map entry costs to store and look
// up. A map entry per pair compared from lcpMin up cost more than the
// scans it saved when most pairs are compared once, and grew with the
// comparisons, not the document (review finding: 6.6 million entries for
// 400,000 listeners with 64-byte names in descending order). The map
// holds at most memo.scanned/lcpMemo entries.
const lcpMemo = 1 << 10

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
// parent is the path the caller extends, so a sweep that this call runs
// keeps it (memo.sweepIfGrown).
func (x *memo) child(parent *pathNode, e diag.PathElem) *pathNode {
	x.sweepIfGrown(parent)
	return &pathNode{parent: parent, elem: e, text: x.intern(parent.textOf(), e, nil)}
}

// intern returns the text node of parent extended by e, storing a new one
// in spare when spare is not nil. Rendering the segment and hashing it
// are a span that cannot yield (Options.Yield), charged with the
// segment's bytes after it: a keyed entry's key, a field name or a set
// element can be as long as the document. A new text counts toward the
// next sweep (memo.grow).
func (x *memo) intern(parent *textNode, e diag.PathElem, spare *textNode) *textNode {
	at := textAt{parent: parent, text: segmentText(e, depthOf(parent))}
	x.work.charge(len(at.text))
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
	x.grow()
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
// segments. A pair with a segment shorter than lcpMin is scanned per
// call, reading fewer than lcpMin bytes. A long pair is answered from
// either segment's last comparison when it was with the other
// (lcpCache.peer), so the comparisons of a sort that meets one pair again
// and again, such as many diagnostics below each of two siblings, read
// it once; else from memo.lcp, where pairs whose common prefix reaches
// lcpMemo are memoized; else it is scanned, up to lcpMemo bytes before a
// pair is memoized. x.scanned counts every byte scanned.
func (x *memo) commonPrefix(a, b *textNode) int {
	if len(a.text) < lcpMin || len(b.text) < lcpMin {
		return commonPrefix(a.text, b.text)
	}
	ca, cb := x.cache(a), x.cache(b)
	if ca.peer == b {
		return ca.n
	}
	if cb.peer == a {
		return cb.n
	}
	n, ok := 0, false
	if ca.memoized && cb.memoized {
		n, ok = x.lcp[textPair{a, b}]
		if !ok {
			n, ok = x.lcp[textPair{b, a}]
		}
	}
	if !ok {
		n = x.scan(a.text[:min(len(a.text), lcpMemo)], b.text[:min(len(b.text), lcpMemo)])
		if n == lcpMemo {
			n += x.scan(a.text[n:], b.text[n:])
			if x.lcp == nil {
				x.lcp = map[textPair]int{}
			}
			x.lcp[textPair{a, b}] = n
			ca.memoized, cb.memoized = true, true
		}
	}
	ca.peer, ca.n = b, n
	cb.peer, cb.n = a, n
	return n
}

// cache returns the lcpCache of long segment t, allocating it on first
// use.
func (x *memo) cache(t *textNode) *lcpCache {
	if t.lcp == nil {
		t.lcp = &lcpCache{}
	}
	return t.lcp
}

// scan returns the length n of the common prefix of a and b and adds
// min(n+1, len(a), len(b)) to x.scanned, charging it on x.work: the
// prefix and the first byte that differs, when both have one. That is a
// model of the bytes read, not an exact count: commonPrefix compares
// 64-byte blocks and then rescans the block that differs byte by byte,
// so it reads up to one 64-byte block more than the model.
func (x *memo) scan(a, b string) int {
	n := commonPrefix(a, b)
	read := min(n+1, len(a), len(b))
	x.scanned += read
	x.work.charge(read)
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
