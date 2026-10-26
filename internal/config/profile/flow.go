// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// fctx is where a node is read: inFlow inside a flow collection, the
// indentation its later lines need (s-flow-line-prefix), inside a flow
// collection that collection's opening bracket and its closer, and key
// when the node is known to be a key before it is read (after '?', or a
// flow mapping's key), so a collection there is refused at its start
// before it is parsed.
type fctx struct {
	inFlow    bool
	minIndent int
	open      tree.Pos
	closer    byte
	key       bool
}

// props are a node's properties.
type props struct {
	any bool
	// at and off are the first property, as a position and a byte offset.
	at  tree.Pos
	off int
	// tag is the tag as written, "" for none.
	tag   string
	tagAt tree.Pos
	// anchor is set by an anchor, which the profile refuses (RZ-CFG-003).
	anchor   bool
	anchorAt tree.Pos
}

// pos returns where a node with these properties starts: at its tag, or at
// at.
func (pr props) pos(at tree.Pos) tree.Pos {
	if pr.tag != "" {
		return pr.tagAt
	}
	return at
}

// itemKind is the kind of node an item holds.
type itemKind uint8

const (
	itemEmpty itemKind = iota
	itemPlain
	itemQuoted
	itemSeq
	itemMap
	itemAlias
)

// item is a node that has been read up to the point where the parser
// knows whether it is an implicit key: a plain scalar's first line, a
// quoted scalar, a flow collection, an alias or nothing after properties.
type item struct {
	kind  itemKind
	props props
	// at and start are where the content starts, as a position and a byte
	// offset.
	at    tree.Pos
	start int
	node  *tree.Node
	text  string
	style tree.Style
	// multi marks a quoted scalar or flow collection over several lines.
	multi bool
	// ps and pe are the offsets of a plain scalar's first line.
	ps, pe int
}

// first returns where the item's first token is.
func (it item) first() tree.Pos {
	if it.props.any {
		return it.props.at
	}
	return it.at
}

// props reads the properties at the cursor: a tag, an anchor or both, in
// either order. Each is followed by white space, the end of its line or,
// in a flow collection, a flow indicator; in block context the properties
// are on one line and the white space after them is skipped, and in a
// flow collection the separation after them may span lines.
func (y *yamlParser) props(fc fctx) props {
	var pr props
	for {
		c := y.cur()
		if c != '!' && c != '&' {
			return pr
		}
		at := y.here()
		if !pr.any {
			pr.any, pr.at, pr.off = true, at, y.off
		}
		if c == '!' {
			tag := y.tagProperty()
			if pr.tag != "" {
				y.syntax(at, msgTagChain)
			}
			pr.tag, pr.tagAt = tag, at
		} else {
			y.anchorProperty()
			if pr.anchor {
				y.syntax(at, msgAnchorChain)
			}
			pr.anchor, pr.anchorAt = true, at
		}
		if !y.blankOrFlowAt(y.off, fc.inFlow) {
			y.syntax(y.here(), msgPropsSep)
		}
		if fc.inFlow {
			y.flowSep(fc)
		} else {
			y.skipWhite()
		}
	}
}

// tagProperty reads the tag at the cursor and returns it as written: a
// verbatim tag "!<...>" or a shorthand of tag characters and '!'. Only the
// seven YAML 1.2 core tags are accepted; any other tag is RZ-CFG-004, and
// the node is then read as if untagged.
func (y *yamlParser) tagProperty() string {
	at := y.here()
	t := y.text
	start := y.off
	i := start + 1
	if i < len(t) && t[i] == '<' {
		j := i + 1
		for j < len(t) && t[j] != '>' && !isBlank(t[j]) {
			j++
		}
		if j >= len(t) || t[j] != '>' || j == i+1 {
			y.syntax(at, msgVerbatimTag)
		}
		i = j + 1
	} else {
		for i < len(t) && isTagChar(t[i]) {
			i++
		}
	}
	y.skipTo(i)
	tag := t[start:i]
	if !isCoreTag(tag) {
		y.profileFinding(codeTag, at, "tag "+clip(tag)+" is not allowed; only the YAML 1.2 core tags are")
	}
	return tag
}

// isTagChar reports the characters of a shorthand tag: URI characters
// other than the flow indicators (ns-tag-char), and '!' for its handle.
func isTagChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("-#;/?:@&=+$_.~*'()!%", c) >= 0
}

// isAnchorChar reports the bytes of an anchor or alias name: any
// character but white space, line breaks and flow indicators
// (ns-anchor-char).
func isAnchorChar(c byte) bool {
	return !isBlank(c) && !isFlowIndicator(c)
}

// anchorProperty reads the anchor at the cursor: RZ-CFG-003.
func (y *yamlParser) anchorProperty() {
	at := y.here()
	name := y.anchorName()
	if name == "" {
		y.syntax(at, msgAnchorName)
	}
	y.profileFinding(codeAnchor, at, "anchors are not allowed (&"+clip(name)+")")
}

// alias reads the alias at the cursor: RZ-CFG-003.
func (y *yamlParser) alias() {
	at := y.here()
	name := y.anchorName()
	if name == "" {
		y.syntax(at, msgAliasName)
	}
	y.profileFinding(codeAnchor, at, "aliases are not allowed (*"+clip(name)+")")
}

// anchorName reads the name after the '&' or '*' at the cursor.
func (y *yamlParser) anchorName() string {
	t := y.text
	i := y.off + 1
	for i < len(t) && isAnchorChar(t[i]) {
		i++
	}
	name := t[y.off+1 : i]
	y.skipTo(i)
	return name
}

// readItem reads a node's properties and content inside a flow collection.
func (y *yamlParser) readItem(fc fctx) item {
	return y.readContent(fc, y.props(fc))
}

// readContent reads a node's content after its properties pr: a flow
// collection or a quoted scalar whole, a plain scalar's first line, an
// alias, or nothing.
func (y *yamlParser) readContent(fc fctx, pr props) item {
	it := item{props: pr, at: y.here(), start: y.off}
	c := y.cur()
	switch {
	case c == '[' || c == '{':
		if fc.key {
			y.syntax(pr.pos(it.at), msgComplexKey)
		}
		line := y.line
		if c == '[' {
			it.kind, it.node = itemSeq, y.flowSeq(fc.minIndent, pr)
		} else {
			it.kind, it.node = itemMap, y.flowMap(fc.minIndent, pr)
		}
		it.multi = y.line != line
		y.afterNode(fc)
	case c == '"' || c == '\'':
		it.kind = itemQuoted
		it.text, it.style, it.multi = y.quoted(fc.minIndent)
		y.afterNode(fc)
	case c == '*':
		if pr.any {
			y.syntax(it.at, msgAliasProps)
		}
		it.kind = itemAlias
		y.alias()
		y.afterNode(fc)
	case c == '\n' || c == 0, fc.inFlow && (c == ',' || c == ']' || c == '}'), c == ':' && y.blankOrFlowAt(y.off+1, fc.inFlow):
		it.kind = itemEmpty
	case y.plainFirst(fc.inFlow):
		it.kind = itemPlain
		it.ps = y.off
		it.pe = y.plainLine(y.off, fc.inFlow)
		y.skipTo(it.pe)
	default:
		y.syntax(it.at, y.badStart(c, fc.inFlow))
	}
	return it
}

// afterNode checks the character after a quoted scalar, flow collection
// or alias: white space, a line break, a ':' or, in a flow collection, a
// flow indicator.
func (y *yamlParser) afterNode(fc fctx) {
	c := y.cur()
	switch {
	case c == 0, isBlank(c), c == ':', fc.inFlow && (c == ',' || c == ']' || c == '}'):
	case c == '#':
		y.syntax(y.here(), msgCommentSpace)
	case fc.inFlow:
		y.syntax(y.here(), msgAfterFlowNode)
	default:
		y.syntax(y.here(), msgAfterNode)
	}
}

// badStart returns the message for a character that starts no node.
func (y *yamlParser) badStart(c byte, inFlow bool) string {
	switch {
	case c == '@' || c == '`':
		return "syntax error: '" + string(c) + "'" + msgReserved
	case c == '%':
		return msgPercent
	case c == '-' && inFlow && y.blankAt(y.off+1):
		return msgFlowDash
	case c == '-' && inFlow:
		return msgDashAlone
	case c == '?' && inFlow && y.blankAt(y.off+1):
		return msgExplicitPlace
	case c == '?':
		return msgQuestionSpace
	case c == '|' || c == '>':
		if inFlow {
			return msgBlockInFlow
		}
		return msgBlockScalarPlace
	case c == '#':
		return msgCommentSpace
	}
	r, _ := utf8.DecodeRuneInString(y.text[y.off:])
	return msgBadStart + "'" + string(r) + "'"
}

// plainFirst reports whether the cursor's character can start a plain
// scalar (ns-plain-first): no indicator, except '-', '?' and ':' before a
// character that is not white space and, in a flow collection, not a flow
// indicator.
func (y *yamlParser) plainFirst(inFlow bool) bool {
	c := y.cur()
	switch c {
	case '-', '?', ':':
		return !y.blankOrFlowAt(y.off+1, inFlow)
	case ',', '[', ']', '{', '}', '#', '&', '*', '!', '|', '>', '\'', '"', '%', '@', '`', ' ', '\t', '\n', 0:
		return false
	}
	return true
}

// plainLine returns the offset just past the last character of the plain
// scalar that runs on its line from byte offset from: it ends before a
// ':' followed by white space, the end of the line or, in a flow
// collection, a flow indicator; before a '#' that follows white space; at
// a flow indicator in a flow collection; and at the end of the line, with
// the white space before it left out (ns-plain-one-line).
func (y *yamlParser) plainLine(from int, inFlow bool) int {
	t := y.text
	last := from
	for i := from; i < len(t); {
		switch c := t[i]; {
		case c == '\n':
			return last
		case c == ' ' || c == '\t':
			i++
		case c == ':':
			if y.blankOrFlowAt(i+1, inFlow) {
				return last
			}
			i++
			last = i
		case c == '#':
			if i > from && isWhite(t[i-1]) {
				return last
			}
			i++
			last = i
		case inFlow && isFlowIndicator(c):
			return last
		case c < utf8.RuneSelf:
			i++
			last = i
		default:
			_, size := utf8.DecodeRuneInString(t[i:])
			i += size
			last = i
		}
	}
	return last
}

// plainMore continues the plain scalar whose first line is [start, end)
// over the lines after it (ns-plain-multi-line) and returns its value and
// the offset past its last character. A later line continues the scalar
// when it is indented at least minIndent spaces, is no document marker,
// and does not start with a comment, a ':' that ends the scalar or, in a
// flow collection, a flow indicator. Lines fold: a single line break
// becomes a space and each empty line a line feed, and the white space
// around a line break is left out.
func (y *yamlParser) plainMore(start, end int, inFlow bool, minIndent int) (string, int) {
	t := y.text
	var b []byte
	for {
		j := end
		for j < len(t) && isWhite(t[j]) {
			j++
		}
		if j >= len(t) || t[j] != '\n' {
			break
		}
		next, breaks := -1, 0
		for s := j + 1; s < len(t); {
			e := lineEnd(t, s)
			ls := leadingSpaces(t[s:e])
			w := s + ls
			for w < e && isWhite(t[w]) {
				w++
			}
			if w == e {
				breaks++
				s = e + 1
				continue
			}
			c := t[w]
			if (ls == 0 && markerAt(t[s:e])) || ls < minIndent || c == '#' || (inFlow && isFlowIndicator(c)) || (c == ':' && y.blankOrFlowAt(w+1, inFlow)) {
				break
			}
			next = w
			break
		}
		if next < 0 {
			break
		}
		if b == nil {
			b = append(b, t[start:end]...)
		}
		if breaks == 0 {
			b = append(b, ' ')
		}
		for ; breaks > 0; breaks-- {
			b = append(b, '\n')
		}
		end = y.plainLine(next, inFlow)
		b = append(b, t[next:end]...)
	}
	if b == nil {
		return strings.Clone(t[start:end]), end
	}
	return string(b), end
}

// quoted reads the quoted scalar at the cursor (readQuoted) and moves the
// cursor past its closing quote. Every line after its first that holds
// content is indented at least minIndent spaces, and none is a document
// marker (c-forbidden).
func (y *yamlParser) quoted(minIndent int) (string, tree.Style, bool) {
	at := y.here()
	t := y.text
	off := y.off
	value, end, msg := readQuoted(y.buf, t, off)
	if msg != "" {
		y.syntax(at, msg)
	}
	y.buf = value
	multi := false
	for i := off; ; {
		nl := strings.IndexByte(t[i:end], '\n')
		if nl < 0 {
			break
		}
		multi = true
		s := i + nl + 1
		e := lineEnd(t, s)
		line := t[s:e]
		if markerAt(line) {
			y.syntax(y.posOf(s), msgQuotedMarker)
		}
		if ls := leadingSpaces(line); ls < minIndent && strings.TrimLeft(line, " \t") != "" {
			w := s + ls
			for w < e && isWhite(t[w]) {
				w++
			}
			y.syntax(y.posOf(w), msgQuotedIndent)
		}
		i = s
	}
	style := tree.StyleDoubleQuoted
	if t[off] == '\'' {
		style = tree.StyleSingleQuoted
	}
	y.skipTo(end)
	return string(value), style, multi
}

// keyColon reports whether the item is an implicit key: a ':' follows it on
// its line, after white space only, followed by white space or the end of
// the line or, in a flow collection, by a flow indicator, or directly after
// a quoted key there. It then checks the key and moves the cursor past the
// ':'. An implicit key is one scalar on one line of at most 1,024
// characters (YAML 1.2.2 section 7.4); a collection key is RZ-CFG-001. A
// quoted key or a flow collection is JSON-like: in a flow collection a ':'
// may follow it directly (c-s-implicit-json-key).
func (y *yamlParser) keyColon(it item, inFlow bool) (keyRead, bool) {
	t := y.text
	i := y.off
	for i < len(t) && isWhite(t[i]) {
		i++
	}
	json := it.kind == itemQuoted || it.kind == itemSeq || it.kind == itemMap
	if i >= len(t) || t[i] != ':' || (!y.blankOrFlowAt(i+1, inFlow) && (!inFlow || !json)) {
		return keyRead{}, false
	}
	first := it.start
	if it.props.any {
		first = it.props.off
	}
	switch {
	case it.kind == itemSeq || it.kind == itemMap:
		y.syntax(it.props.pos(it.at), msgComplexKey)
	case it.multi:
		y.syntax(y.posOf(i), msgMultiLineKey)
	case i-first > maxImplicitKey && utf8.RuneCountInString(t[first:i]) > maxImplicitKey:
		y.syntax(it.first(), msgLongKey)
	}
	text := ""
	switch it.kind {
	case itemPlain:
		text = t[it.ps:it.pe]
	case itemQuoted:
		text = it.text
	default:
		// An empty key or an alias: the key is "".
	}
	k := keyRead{text: strings.Clone(text), at: it.props.pos(it.at), start: it.first(), json: json}
	y.keyNode(text, itemStyle(it), k.at, it.props)
	y.skipTo(i)
	k.colon = y.here()
	y.adv()
	return k, true
}

// itemStyle returns the scalar style of a plain or quoted item.
func itemStyle(it item) tree.Style {
	if it.kind == itemQuoted {
		return it.style
	}
	return tree.StylePlain
}

// finishItem builds the node of an item that is not an implicit key: a
// plain scalar continues over the lines after its first. As a key (an
// explicit or flow mapping key) the node is the key's text, untyped, and a
// collection is RZ-CFG-001.
func (y *yamlParser) finishItem(it item, fc fctx, key bool) *tree.Node {
	switch it.kind {
	case itemSeq, itemMap:
		if key {
			y.syntax(it.props.pos(it.at), msgComplexKey)
		}
		return it.node
	case itemAlias:
		return y.nullAt(it.at)
	case itemEmpty:
		return y.emptyNode(it.props, it.at, key)
	case itemQuoted:
		return y.scalarNode(it.text, it.style, it.at, it.props, key)
	default:
		text, end := y.plainMore(it.ps, it.pe, fc.inFlow, fc.minIndent)
		y.skipTo(end)
		return y.scalarNode(text, tree.StylePlain, it.at, it.props, key)
	}
}

// scalarNode builds a scalar node: a plain scalar is typed by the YAML 1.2
// core schema from its text, a quoted or block scalar is a string, and a
// core tag forces the type, a mismatch being RZ-CFG-001 at the tag. A
// tagged node starts at its tag. As a key the node is the text as written,
// a string; a merge key ("<<", plain and untagged) is RZ-CFG-003.
func (y *yamlParser) scalarNode(text string, style tree.Style, at tree.Pos, pr props, key bool) *tree.Node {
	if key {
		return y.keyNode(text, style, pr.pos(at), pr)
	}
	pos := pr.pos(at)
	y.built(pos)
	n := &tree.Node{Pos: pos, Style: style}
	var t typed
	switch {
	case pr.tag != "" && isCoreTag(pr.tag):
		t = resolveTagged(pr.tag, text)
	case style != tree.StylePlain:
		t = typed{kind: tree.KindString, text: text}
	default:
		t = resolvePlain(text)
	}
	if t.code != "" {
		y.semanticAt(t.code, pos, t.msg)
		n.Kind = tree.KindNull
		return n
	}
	n.Kind, n.Text, n.Bool = t.kind, t.text, t.b
	if n.Text != "" {
		n.Text = strings.Clone(n.Text)
	}
	return n
}

// keyNode builds the string node of a key, checking its tag and a merge
// key.
func (y *yamlParser) keyNode(text string, style tree.Style, pos tree.Pos, pr props) *tree.Node {
	y.built(pos)
	switch {
	case pr.tag == tagMap || pr.tag == tagSeq:
		y.semanticAt(codeParse, pr.tagAt, "value does not match tag "+pr.tag)
	case pr.tag != "" && isCoreTag(pr.tag):
		if t := resolveTagged(pr.tag, text); t.code != "" {
			y.semanticAt(t.code, pr.tagAt, t.msg)
		}
	case pr.tag == "" && style == tree.StylePlain && text == "<<":
		y.profileFinding(codeAnchor, pos, "merge keys (<<) are not allowed")
	}
	return &tree.Node{Kind: tree.KindString, Style: style, Pos: pos, Text: strings.Clone(text)}
}

// emptyNode builds the node of an entry with no content: null, or with a
// core tag the empty scalar typed by it; !!map and !!seq need content.
func (y *yamlParser) emptyNode(pr props, at tree.Pos, key bool) *tree.Node {
	if !pr.any {
		if key {
			return y.keyNode("", tree.StylePlain, at, pr)
		}
		return y.nullAt(at)
	}
	pos := pr.pos(pr.at)
	if !key && (pr.tag == tagMap || pr.tag == tagSeq) {
		y.built(pos)
		y.semanticAt(codeParse, pos, msgNoValue)
		return &tree.Node{Kind: tree.KindNull, Pos: pos}
	}
	return y.scalarNode("", tree.StylePlain, pos, pr, key)
}

// nullAt builds a null node.
func (y *yamlParser) nullAt(at tree.Pos) *tree.Node {
	y.built(at)
	return &tree.Node{Kind: tree.KindNull, Pos: at}
}

// tagCollection applies a collection's properties: it starts at its tag,
// and a core tag other than !!map for a mapping or !!seq for a sequence is
// RZ-CFG-001.
func (y *yamlParser) tagCollection(n *tree.Node, pr props) *tree.Node {
	if pr.tag == "" {
		return n
	}
	n.Pos = pr.tagAt
	want := tagMap
	if n.Kind == tree.KindList {
		want = tagSeq
	}
	if isCoreTag(pr.tag) && pr.tag != want {
		y.semanticAt(codeParse, pr.tagAt, "value does not match tag "+pr.tag)
	}
	return n
}

// addMember appends a member to a mapping, reporting a duplicate key at the
// second key with the first as related location; the first entry of a key
// is kept. Keys compare as written, so 1 and "1" are one key.
func (y *yamlParser) addMember(m *tree.Node, seen *map[string]tree.Pos, k keyRead, value *tree.Node) {
	first, dup := tree.Pos{}, false
	if *seen == nil && len(m.Members) < 8 {
		for _, mb := range m.Members {
			if mb.Key == k.text {
				first, dup = mb.KeyPos, true
				break
			}
		}
	} else {
		if *seen == nil {
			*seen = make(map[string]tree.Pos, 2*len(m.Members))
			for _, mb := range m.Members {
				(*seen)[mb.Key] = mb.KeyPos
			}
		}
		first, dup = (*seen)[k.text]
	}
	if dup {
		d := y.p.errorAt(codeDuplicate, k.at, "duplicate key "+quoteKey(k.text))
		d.Related = append(d.Related, relatedFirst(y.p, first))
		y.semantic(d)
		return
	}
	if *seen != nil {
		(*seen)[k.text] = k.at
	}
	m.Members = append(m.Members, tree.Member{Key: k.text, KeyPos: k.at, Value: value})
}

// flowSep skips the separation inside a flow collection: white space,
// comments and line breaks. A comment starts at a '#' after white space or
// at the start of a line. After a line break, the line's first character
// other than white space is indented at least minIndent spaces unless it
// starts a comment (s-flow-line-prefix), and a document marker or the end
// of the text there leaves the collection unclosed.
func (y *yamlParser) flowSep(fc fctx) {
	t := y.text
	white := y.off == 0 || isBlank(t[y.off-1])
	for {
		if y.off >= len(t) {
			y.syntax(fc.open, unclosed(fc.closer))
		}
		switch c := t[y.off]; {
		case c == ' ' || c == '\t':
			y.adv()
			white = true
		case c == '#' && white:
			y.skipTo(lineEnd(t, y.off))
		case c == '\n':
			y.skipTo(y.off + 1)
			white = true
			s := y.off
			if s >= len(t) {
				y.syntax(fc.open, unclosed(fc.closer))
			}
			e := lineEnd(t, s)
			if markerAt(t[s:e]) {
				y.syntax(fc.open, msgFlowMarker)
			}
			ls := leadingSpaces(t[s:e])
			f := s + ls
			for f < e && isWhite(t[f]) {
				f++
			}
			if f < e && t[f] != '#' && ls < fc.minIndent {
				if t[f] == ']' || t[f] == '}' {
					y.syntax(y.posOf(f), msgFlowClose)
				}
				y.syntax(y.posOf(f), msgFlowIndent)
			}
		default:
			return
		}
	}
}

// unclosed returns the message for a flow collection left open.
func unclosed(closer byte) string {
	if closer == '}' {
		return msgFlowMapOpen
	}
	return msgFlowSeqOpen
}

// flowSeq parses the flow sequence at the cursor (c-flow-sequence), whose
// lines are indented at least minIndent spaces.
func (y *yamlParser) flowSeq(minIndent int, pr props) *tree.Node {
	open := y.here()
	y.enter(open)
	y.built(open)
	fc := fctx{inFlow: true, minIndent: minIndent, open: open, closer: ']'}
	list := &tree.Node{Kind: tree.KindList, Pos: open}
	y.adv()
	for {
		y.flowSep(fc)
		switch y.cur() {
		case ']':
			y.adv()
			y.leave()
			return y.tagCollection(list, pr)
		case ',':
			y.syntax(y.here(), msgFlowEmpty)
		}
		list.Items = append(list.Items, y.flowSeqEntry(fc))
		y.flowSep(fc)
		switch y.cur() {
		case ',':
			y.adv()
		case ']':
		default:
			y.syntax(y.here(), msgFlowSeqNext)
		}
	}
}

// flowSeqEntry parses one entry of a flow sequence: a flow node, or a flow
// pair, a single-pair mapping: an explicit "? key : value", ": value" with
// an empty key, or an implicit key on one line followed by ':'
// (ns-flow-seq-entry).
func (y *yamlParser) flowSeqEntry(fc fctx) *tree.Node {
	at := y.here()
	c := y.cur()
	switch {
	case c == '?' && y.blankAt(y.off+1):
		y.adv()
		y.flowSep(fc)
		return y.flowPair(y.explicitKey(fc, at), fc)
	case c == ':' && y.blankOrFlowAt(y.off+1, true):
		y.built(at)
		return y.flowPair(keyRead{at: at, start: at}, fc)
	}
	it := y.readItem(fc)
	if k, ok := y.keyColon(it, true); ok {
		y.enter(k.start)
		y.built(k.start)
		m := &tree.Node{Kind: tree.KindMap, Pos: k.start}
		m.Members = []tree.Member{{Key: k.text, KeyPos: k.at, Value: y.flowAfterColon(k, fc)}}
		y.leave()
		return m
	}
	node := y.finishItem(it, fc, false)
	if it.kind == itemPlain && y.off > it.pe {
		// A plain scalar over several lines before a ':' would be a
		// multi-line implicit key.
		i := y.off
		for i < len(y.text) && isWhite(y.text[i]) {
			i++
		}
		if i < len(y.text) && y.text[i] == ':' && y.blankOrFlowAt(i+1, true) {
			y.syntax(y.posOf(i), msgMultiLineKey)
		}
	}
	return node
}

// explicitKey reads the key after an explicit '?' at at in a flow
// collection: a node, or nothing.
func (y *yamlParser) explicitKey(fc fctx, at tree.Pos) keyRead {
	c := y.cur()
	if (c == ':' && y.blankOrFlowAt(y.off+1, true)) || c == ',' || c == fc.closer {
		pos := nextColumn(at)
		y.built(pos)
		return keyRead{at: pos, start: at}
	}
	kc := fc
	kc.key = true
	it := y.readItem(kc)
	kn := y.finishItem(it, fc, true)
	y.flowSep(fc)
	return keyRead{text: kn.Text, at: kn.Pos, start: at, json: it.kind == itemQuoted}
}

// flowPair parses the rest of a flow pair after its key k, at the ':' or
// where it would be.
func (y *yamlParser) flowPair(k keyRead, fc fctx) *tree.Node {
	y.enter(k.start)
	y.built(k.start)
	m := &tree.Node{Kind: tree.KindMap, Pos: k.start}
	m.Members = []tree.Member{{Key: k.text, KeyPos: k.at, Value: y.flowValue(k, fc)}}
	y.leave()
	return m
}

// flowValue parses the value after a flow mapping or pair key: the node
// after its ':', or null when no ':' or no node follows.
func (y *yamlParser) flowValue(k keyRead, fc fctx) *tree.Node {
	if y.cur() != ':' || (!y.blankOrFlowAt(y.off+1, true) && !k.json) {
		return y.nullAt(nextColumn(k.at))
	}
	k.colon = y.here()
	y.adv()
	return y.flowAfterColon(k, fc)
}

// flowAfterColon parses the value after the ':' of key k in a flow
// collection, the cursor being past it: the node after the separation, or
// null before ',' or the closing bracket. Only after a JSON-like key may
// the node follow the ':' without white space
// (c-ns-flow-map-adjacent-value): after another key "a:[]" is the key "a",
// an empty value and an unexpected '[' (c-ns-flow-map-separate-value).
func (y *yamlParser) flowAfterColon(k keyRead, fc fctx) *tree.Node {
	if c := y.cur(); !k.json && (c == '[' || c == '{') {
		y.syntax(y.here(), msgColonSpace)
	}
	y.flowSep(fc)
	if c := y.cur(); c == ',' || c == fc.closer {
		return y.nullAt(nextColumn(k.colon))
	}
	it := y.readItem(fc)
	return y.finishItem(it, fc, false)
}

// nextColumn returns the position one column after at, where an empty node
// after an indicator sits.
func nextColumn(at tree.Pos) tree.Pos {
	at.Column++
	return at
}

// quoteKey returns a key quoted for a message.
func quoteKey(k string) string {
	return strconv.Quote(clip(k))
}

// relatedFirst returns the related location of a duplicate key's first
// definition.
func relatedFirst(p *fileParser, first tree.Pos) diag.Related {
	return diag.Related{Location: p.location(first), Message: "first defined at"}
}

// flowMap parses the flow mapping at the cursor (c-flow-mapping), whose
// lines are indented at least minIndent spaces.
func (y *yamlParser) flowMap(minIndent int, pr props) *tree.Node {
	open := y.here()
	y.enter(open)
	y.built(open)
	fc := fctx{inFlow: true, minIndent: minIndent, open: open, closer: '}'}
	m := &tree.Node{Kind: tree.KindMap, Pos: open}
	var seen map[string]tree.Pos
	y.adv()
	for {
		y.flowSep(fc)
		switch y.cur() {
		case '}':
			y.adv()
			y.leave()
			return y.tagCollection(m, pr)
		case ',':
			y.syntax(y.here(), msgFlowEmpty)
		}
		k := y.flowMapKey(fc)
		y.addMember(m, &seen, k, y.flowValue(k, fc))
		y.flowSep(fc)
		switch y.cur() {
		case ',':
			y.adv()
		case '}':
		default:
			y.syntax(y.here(), msgFlowMapNext)
		}
	}
}

// flowMapKey reads the key of a flow mapping entry: after an explicit
// '?', an empty key before ':', or a node, which may span lines, and the
// separation after it, so the ':' may be on a later line
// (ns-flow-map-entry).
func (y *yamlParser) flowMapKey(fc fctx) keyRead {
	at := y.here()
	c := y.cur()
	switch {
	case c == '?' && y.blankAt(y.off+1):
		y.adv()
		y.flowSep(fc)
		return y.explicitKey(fc, at)
	case c == ':' && y.blankOrFlowAt(y.off+1, true):
		y.built(at)
		return keyRead{at: at, start: at}
	}
	kc := fc
	kc.key = true
	it := y.readItem(kc)
	kn := y.finishItem(it, fc, true)
	y.flowSep(fc)
	return keyRead{text: kn.Text, at: kn.Pos, start: kn.Pos, json: it.kind == itemQuoted}
}
