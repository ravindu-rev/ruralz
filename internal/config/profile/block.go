// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// indicator says what a block node follows, which decides the collections
// it may start on its first line (YAML 1.2.2 section 8.2).
type indicator uint8

const (
	// afterDoc is the document start: "---" or the stream (s-l+block-node).
	afterDoc indicator = iota
	// afterDash is a block sequence entry's '-' (s-l+block-indented):
	// a compact sequence or mapping may follow on its line.
	afterDash
	// afterQuestion is an explicit key's '?' (s-l+block-indented); the
	// profile refuses a collection there as a key.
	afterQuestion
	// afterExplicitColon is the ':' of an explicit entry's value, at the
	// start of its line (s-l+block-indented).
	afterExplicitColon
	// afterColon is the ':' after an implicit key (s-l+block-node): no
	// block collection starts on its line.
	afterColon
)

// compact reports whether a compact collection may start on the
// indicator's line.
func (ind indicator) compact() bool {
	return ind == afterDash || ind == afterQuestion || ind == afterExplicitColon
}

// blockOut reports whether the node is in block-out context, where a block
// sequence may sit at the indentation of the collection holding it
// (seq-spaces).
func (ind indicator) blockOut() bool {
	return ind == afterColon || ind == afterExplicitColon || ind == afterQuestion
}

// keyRead is a mapping key that has been read.
type keyRead struct {
	text string
	// at is the key's position: its tag, or its content (or, for an empty
	// key, where it would be).
	at tree.Pos
	// start is the entry's first token: a property, '?', ':' or the key.
	start tree.Pos
	// colon is the position of the ':' that follows the key.
	colon tree.Pos
	// json marks a quoted key, after which a ':' needs no white space in
	// a flow collection.
	json bool
}

// blockValue parses the node that follows an indicator on the cursor's
// line, or the document start; n is the indentation of the block
// collection holding the entry (-1 at the document level) and empty is
// where an empty node sits. It leaves the cursor at the start of the line
// after the node.
func (y *yamlParser) blockValue(n int, ind indicator, empty tree.Pos) *tree.Node {
	tabbed := y.skipWhite()
	pr := y.props(fctx{})
	key := ind == afterQuestion
	if y.atLineEnd() {
		y.endLine()
		return y.below(n, ind, pr, empty, key)
	}
	at := y.here()
	c := y.cur()
	switch {
	case c == '|' || c == '>':
		return y.blockScalar(n, pr, key)
	case (c == '-' || c == '?') && y.blankAt(y.off+1), c == ':' && y.blankAt(y.off+1) && !pr.any:
		y.compactAllowed(ind, at, pr.any, tabbed)
		m := y.col - 1
		if c == '-' {
			return y.blockSeq(m)
		}
		return y.blockMap(m, props{}, nil)
	}
	it := y.readContent(fctx{minIndent: n + 1, key: key}, pr)
	if k, ok := y.keyColon(it, false); ok {
		y.compactAllowed(ind, k.start, false, tabbed)
		return y.blockMap(int(k.start.Column)-1, props{}, &k)
	}
	node := y.finishItem(it, fctx{minIndent: n + 1}, key)
	y.endLine()
	return node
}

// compactAllowed checks a block collection that starts on its indicator's
// line at at.
func (y *yamlParser) compactAllowed(ind indicator, at tree.Pos, hasProps, tabbed bool) {
	switch {
	case ind == afterQuestion:
		y.syntax(at, msgComplexKey)
	case ind == afterDoc:
		y.syntax(at, msgSameLineDoc)
	case !ind.compact():
		y.syntax(at, msgSameLineValue)
	case hasProps:
		y.syntax(at, msgPropsBlock)
	case tabbed:
		y.syntax(at, msgTabCompact)
	}
}

// below parses the node of an entry whose indicator, and properties pr if
// any, end their line: the node starts on a later line, or the entry is
// empty. A node belongs to the entry when it is indented more than n, or,
// in block-out context, when it is a block sequence at n. After a tab only
// a flow node may start (YAML Test Suite DK95/00).
func (y *yamlParser) below(n int, ind indicator, pr props, empty tree.Pos, key bool) *tree.Node {
	li := y.peekContentLine()
	if !li.eof && !li.marker {
		switch {
		case li.tab && li.first-li.start > n, !li.tab && li.indent > n:
			return y.blockNodeAt(li, n, ind, pr)
		case !li.tab && li.indent == n && ind.blockOut() && y.text[li.first] == '-' && y.blankAt(li.first+1):
			y.skipTo(li.first)
			seq := y.blockSeq(n)
			if key {
				y.syntax(seq.Pos, msgComplexKey)
			}
			return y.tagCollection(seq, pr)
		}
	}
	return y.emptyNode(pr, empty, key)
}

// blockNodeAt parses a node that starts on a new line, li, indented more
// than the collection holding it (n), after the indicator ind and the
// properties pending, if any, on earlier lines.
func (y *yamlParser) blockNodeAt(li lineInfo, n int, ind indicator, pending props) *tree.Node {
	y.skipTo(li.first)
	m := li.indent
	key := ind == afterQuestion
	at := y.here()
	c := y.cur()
	collection := (c == '-' || c == '?' || c == ':') && y.blankAt(y.off+1)
	if li.tab && (collection || c == '|' || c == '>') {
		y.syntax(at, msgTabBlock)
	}
	switch {
	case collection && key:
		y.syntax(at, msgComplexKey)
	case c == '-' && collection:
		return y.tagCollection(y.blockSeq(m), pending)
	case collection:
		return y.blockMap(m, pending, nil)
	case c == '|' || c == '>':
		return y.blockScalar(n, pending, key)
	}
	pr := y.props(fctx{})
	if pr.any {
		if y.atLineEnd() {
			// Properties alone on their line: the node comes below them.
			merged := y.mergeProps(pending, pr)
			y.endLine()
			return y.below(n, ind, merged, merged.at, key)
		}
		switch c = y.cur(); {
		case c == '|' || c == '>':
			return y.blockScalar(n, y.mergeProps(pending, pr), key)
		case (c == '-' || c == '?') && y.blankAt(y.off+1):
			y.syntax(y.here(), msgPropsBlock)
		}
	}
	if c = y.cur(); c == '[' || c == '{' || c == '*' {
		// A flow collection or an alias takes the properties of the lines
		// before it too: they are its own, as neither can be an implicit
		// key of the profile, and an alias has none.
		pr = y.mergeProps(pending, pr)
		pending = props{}
	}
	it := y.readContent(fctx{minIndent: n + 1, key: key}, pr)
	if k, ok := y.keyColon(it, false); ok {
		switch {
		case li.tab:
			y.syntax(k.start, msgTabKey)
		case key:
			y.syntax(k.start, msgComplexKey)
		}
		return y.blockMap(m, pending, &k)
	}
	it.props = y.mergeProps(pending, pr)
	node := y.finishItem(it, fctx{minIndent: n + 1}, key)
	y.endLine()
	return node
}

// mergeProps joins the properties of one node read on two lines: a node
// has at most one tag and one anchor.
func (y *yamlParser) mergeProps(a, b props) props {
	switch {
	case !a.any:
		return b
	case !b.any:
		return a
	case a.tag != "" && b.tag != "":
		y.syntax(b.tagAt, msgTagChain)
	case a.anchor && b.anchor:
		y.syntax(b.anchorAt, msgAnchorChain)
	}
	if b.tag != "" {
		a.tag, a.tagAt = b.tag, b.tagAt
	}
	a.anchor = a.anchor || b.anchor
	return a
}

// blockMap parses a block mapping whose entries are at indentation m, with
// the properties pending from an earlier line. The cursor is at its first
// entry, or, when first is set, past the ':' of that entry's key.
func (y *yamlParser) blockMap(m int, pending props, first *keyRead) *tree.Node {
	at := y.here()
	if first != nil {
		at = first.start
	}
	y.enter(at)
	y.built(at)
	node := &tree.Node{Kind: tree.KindMap, Pos: at}
	var seen map[string]tree.Pos
	for {
		y.closedIndent = -2
		var k keyRead
		var value *tree.Node
		if first != nil {
			k = *first
			first = nil
			value = y.blockValue(m, afterColon, nextColumn(k.colon))
		} else {
			k, value = y.blockEntry(m)
		}
		y.addMember(node, &seen, k, value)
		li := y.peekContentLine()
		if li.eof || li.marker || li.indent < m {
			break
		}
		y.nextEntry(li, m, msgIndentMoreMap, msgTabKey)
		if y.text[li.first] == '-' && y.blankAt(li.first+1) {
			y.syntax(y.posOf(li.first), msgSeqInMap)
		}
		y.skipTo(li.first)
	}
	y.leave()
	y.closedIndent = m
	return y.tagCollection(node, pending)
}

// nextEntry checks the next line of a block collection at m, li, which is
// indented at least m.
func (y *yamlParser) nextEntry(li lineInfo, m int, more, tab string) {
	switch {
	case li.indent > m && y.closedIndent > li.indent:
		y.syntax(y.posOf(li.first), msgDedent)
	case li.indent > m:
		y.syntax(y.posOf(li.first), more)
	case li.tab:
		y.syntax(y.posOf(li.first), tab)
	}
}

// blockEntry parses one entry of a block mapping at m, at the cursor: an
// explicit entry ('?', then an optional ':' line at m), an entry with an
// empty key (':') or an implicit key on one line followed by ':'.
func (y *yamlParser) blockEntry(m int) (keyRead, *tree.Node) {
	at := y.here()
	c := y.cur()
	switch {
	case c == '?' && y.blankAt(y.off+1):
		y.adv()
		kn := y.blockValue(m, afterQuestion, nextColumn(at))
		k := keyRead{text: kn.Text, at: kn.Pos, start: at, json: kn.Style == tree.StyleSingleQuoted || kn.Style == tree.StyleDoubleQuoted}
		li := y.peekContentLine()
		if !li.eof && !li.marker && li.indent == m && !li.tab && y.text[li.first] == ':' && y.blankAt(li.first+1) {
			y.skipTo(li.first)
			k.colon = y.here()
			y.adv()
			return k, y.blockValue(m, afterExplicitColon, nextColumn(k.colon))
		}
		return k, y.nullAt(nextColumn(k.at))
	case c == ':' && y.blankAt(y.off+1):
		y.adv()
		y.built(at)
		return keyRead{at: at, start: at, colon: at}, y.blockValue(m, afterColon, nextColumn(at))
	}
	pr := y.props(fctx{})
	if pr.any && y.atLineEnd() {
		y.syntax(pr.at, msgNoColon)
	}
	it := y.readContent(fctx{minIndent: m + 1}, pr)
	k, ok := y.keyColon(it, false)
	if !ok {
		start := it.at
		if pr.any {
			start = pr.at
		}
		y.syntax(start, msgNoColon)
	}
	return k, y.blockValue(m, afterColon, nextColumn(k.colon))
}

// blockSeq parses a block sequence whose entries are at indentation m; the
// cursor is at its first '-'.
func (y *yamlParser) blockSeq(m int) *tree.Node {
	at := y.here()
	y.enter(at)
	y.built(at)
	list := &tree.Node{Kind: tree.KindList, Pos: at}
	for {
		y.closedIndent = -2
		dash := y.here()
		y.adv()
		list.Items = append(list.Items, y.blockValue(m, afterDash, nextColumn(dash)))
		li := y.peekContentLine()
		if li.eof || li.marker || li.indent < m {
			break
		}
		y.nextEntry(li, m, msgIndentMoreSeq, msgTabEntry)
		if y.text[li.first] != '-' || !y.blankAt(li.first+1) {
			break
		}
		y.skipTo(li.first)
	}
	y.leave()
	y.closedIndent = m
	return list
}

// blockScalar reads the literal or folded scalar whose indicator is at the
// cursor, in a block collection at n (readBlockScalar), and leaves the
// cursor at the start of the line after it. A tab where the lines after
// the scalar allow only spaces (chompedTab) is refused when the document
// goes on after it: such a line can only be a comment before the next
// document (l-document-prefix), not part of this one (YAML Test Suite
// Y79Y/000).
func (y *yamlParser) blockScalar(n int, pr props, key bool) *tree.Node {
	at := y.here()
	t := y.text
	off := y.off
	if !headerValid(t, off) {
		y.syntax(at, msgBlockHeaderRest)
	}
	value, lines, msg := readBlockScalar(t, off, n)
	if msg != "" {
		y.syntax(at, msg)
	}
	style := tree.StyleLiteral
	if t[off] == '>' {
		style = tree.StyleFolded
	}
	end := min(lineEnd(t, off)+1, len(t))
	for range lines {
		end = min(lineEnd(t, end)+1, len(t))
	}
	y.skipTo(end)
	if tab := chompedTab(t, end); tab >= 0 {
		tabAt := y.posOf(tab)
		if li := y.peekContentLine(); !li.eof && !li.marker {
			y.syntax(tabAt, msgBlockScalarTab)
		}
	}
	return y.scalarNode(value, style, at, pr, key)
}

// chompedTab returns the offset of the first tab in the lines from byte
// offset s of text, which follow a block scalar, where YAML 1.2 allows
// only spaces, or -1: the empty lines that end the scalar are spaces only
// (l-chomped-empty), and so is the indentation of the first comment after
// them (l-trail-comments); the comment and white lines after that comment
// take any white space (l-comment).
func chompedTab(text string, s int) int {
	for s < len(text) {
		e := lineEnd(text, s)
		f := s
		for f < e && isWhite(text[f]) {
			f++
		}
		if f < e && text[f] != '#' {
			return -1
		}
		if i := strings.IndexByte(text[s:f], '\t'); i >= 0 {
			return s + i
		}
		if f < e {
			return -1
		}
		s = e + 1
	}
	return -1
}
