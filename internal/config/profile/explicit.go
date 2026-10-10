// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Messages of the explicit entry checks.
const (
	msgExplicitKey      = "an explicit key ('?') must be a single scalar node"
	msgEmptyExplicitKey = "an explicit key ('?') is empty"
	msgAfterExplicitKey = "expected the ':' of the explicit key ('?') or a new entry"
	msgExplicitKeyTag   = "an explicit key ('?') must have its tag and its content on one line"
)

// explicitEntry is an explicit mapping entry in block context, from its
// '?' until its ':' or until something at or left of the '?' column ends
// it (01 req 8, 9, 10; 11 req 17, 26).
//
// goccy pairs a '?' with the one token after it (or token group, such as a
// tag and its scalar on one line), wherever that token is, and a ':' with
// the one token before it; a token after the pair that is neither the
// entry's ':' nor a new entry becomes the entry's value, with no ':'.
// YAML 1.2 instead reads every node after the '?' on its line or indented
// past its column as the key. So wherever an explicit key is not one node,
// goccy reads another tree: "? \"q\"k" as {q: k}, "? k:" (the key
// {k: null}) as {k: null}, "? \"a\"\n  \"b\"\n:" as {a: {b: null}},
// "? a\nb" as {a: b}, and "? \nz: 1" as {z: 1}. The split parse follows
// YAML 1.2's frames and refuses each of these. Worse, goccy nests the node
// it groups the ':' with at that node's own column while the depth
// algorithm closes back to the '?' column, and a ladder of entries like
// "? \"a\"\n  - - \"b\"\n:" reached 33 times MaxDepth.
//
// The profile refuses every mapping key that is not a scalar (01 req 10),
// so the token pass holds the entry open and stops the file with
// RZ-CFG-001 when:
//   - the key holds more than one node, or a '-', '?' or ':' that makes it
//     a collection: anything after the '?' on its line or indented past
//     its column other than the key's first node (msgExplicitKey), or the
//     content of a tag that ends the '?' line, which goccy reads as the
//     entry's value (msgExplicitKeyTag);
//   - a node at or left of the '?' column on a later line is not an
//     implicit key, so it is neither the entry's ':' nor a new entry
//     (msgAfterExplicitKey);
//   - the key is empty (msgEmptyExplicitKey), which goccy refuses before
//     its ':' and otherwise pairs with the next entry's key.
//
// A finding about the key is reported at the entry's ':' when one comes,
// otherwise at the first token that broke the rule. A block scalar's
// content and a plain or quoted scalar over several lines are one token,
// so "? |\n  k\n: v" and "? a\n  b\n: v" hold one node. Like the other
// checks that only keep goccy from misreading a file, these stop the file
// only while it has no other finding: a file with a finding is never
// parsed, and the scan goes on collecting RZ-CFG-003 and RZ-CFG-004.
type explicitEntry struct {
	open bool
	at   tree.Pos // the '?'
	// nodes counts the nodes started in the key.
	nodes int
	// bad is the first token that makes the key more than one node;
	// unknown while there is none. msg is the finding's message.
	bad tree.Pos
	msg string
	// tagLine is set when the key's first node is a tag that ends its line:
	// goccy pairs the '?' with the tag alone and reads the content on a
	// later line as the entry's value, so that content is refused with
	// msgExplicitKeyTag (minor finding of the eighth WP-33 review).
	tagLine bool
}

// inKey reports whether a token at at, starting at column col (a ':' of
// an implicit key starts at its key), belongs to the open entry's key: on
// the '?' line, or indented past the '?'.
func (q *explicitEntry) inKey(at tree.Pos, col int32) bool {
	return at.Line == q.at.Line || col > q.at.Column
}

// explicitKey notes a '?' in block context at at. A '?' in the open
// entry's key makes the key a mapping; any other ends the open entry and
// opens its own.
func (t *tokenPass) explicitKey(at tree.Pos) {
	if t.q.open && t.q.inKey(at, at.Column) {
		t.violate(at)
		return
	}
	t.endExplicit()
	if t.fatal {
		return
	}
	t.q = explicitEntry{open: true, at: at}
}

// explicitNode notes a block node starting at token idx, at at. In the
// open entry's key only the first node is allowed. At or left of the '?'
// column on a later line, the node must be an implicit key, which starts
// a new entry and so ends this one.
func (t *tokenPass) explicitNode(at tree.Pos, idx int) {
	if !t.q.open {
		return
	}
	if t.q.inKey(at, at.Column) {
		t.q.nodes++
		switch {
		case t.q.nodes == 2 && t.q.tagLine && t.lastProperty:
			t.violateWith(at, msgExplicitKeyTag)
		case t.q.nodes > 1:
			t.violate(at)
		case t.tks[idx].Type == token.TagType && !t.contentAfter(idx):
			t.q.tagLine = true
		default:
			if n := t.ungroupedContent(idx); n >= 0 {
				t.violate(t.p.pos(int(at.Line), max(t.col(t.tks[n]), 1)))
			}
		}
		return
	}
	implicit := t.startsKey(idx)
	t.endExplicit()
	if !implicit && !t.fatal && t.errs == 0 {
		t.fail(at, msgAfterExplicitKey)
	}
}

// explicitBlock notes a block indicator at at: a '-' (seq) at column col,
// or the ':' of an implicit key that starts at column col. In the open
// entry's key it makes the key a collection, as does a '-' at the '?'
// column, which YAML 1.2 reads as a sequence in the key; left of the key it
// ends the entry. The entry's own ':' is explicitValue's.
func (t *tokenPass) explicitBlock(at tree.Pos, col int32, seq bool) {
	if !t.q.open {
		return
	}
	if t.q.inKey(at, col) || (seq && col == t.q.at.Column) {
		t.violate(at)
		return
	}
	t.endExplicit()
}

// explicitValue notes a ':' with no key on its line at at. At the '?'
// column on a later line it is the open entry's ':', which completes it,
// and explicitValue reports true; a key that is not one node is reported
// here. Otherwise it is a block indicator (explicitBlock).
func (t *tokenPass) explicitValue(at tree.Pos) bool {
	if !t.q.open {
		return false
	}
	if at.Line == t.q.at.Line || at.Column != t.q.at.Column {
		t.explicitBlock(at, at.Column, false)
		return false
	}
	t.q.open = false
	if t.errs > 0 {
		return true
	}
	switch {
	case t.q.bad.Known():
		t.fail(at, t.q.msg)
	case t.q.nodes == 0:
		t.fail(at, msgEmptyExplicitKey)
	}
	return true
}

// violate records the first token that makes the open entry's key more
// than one node.
func (t *tokenPass) violate(at tree.Pos) {
	t.violateWith(at, msgExplicitKey)
}

// violateWith records the first token that breaks the open entry's key,
// with its message.
func (t *tokenPass) violateWith(at tree.Pos, msg string) {
	if !t.q.bad.Known() {
		t.q.bad, t.q.msg = at, msg
	}
}

// endExplicit ends the open entry other than at its ':': at a new entry,
// at a token left of it, at a document marker or at the end of the
// tokens. A key that is not one node is reported at its first offending
// token, and an empty key at its '?'.
func (t *tokenPass) endExplicit() {
	if !t.q.open {
		return
	}
	t.q.open = false
	if t.errs > 0 {
		return
	}
	switch {
	case t.q.bad.Known():
		t.fail(t.q.bad, t.q.msg)
	case t.q.nodes == 0:
		t.fail(t.q.at, msgEmptyExplicitKey)
	}
}

// ungroupedContent returns the index of the content of the tag at token
// idx, on the tag's line, when goccy does not group the two: goccy groups
// only its scalar tags with a scalar after them, so after a '?' it pairs
// the tag alone with the '?' and reads the content as the entry's value
// ("? !!str {k: v}" as {"": {k: v}}). It returns -1 when token idx is not
// a tag, when nothing follows the tag on its line, and when goccy groups
// them.
func (t *tokenPass) ungroupedContent(idx int) int {
	tag := t.tks[idx]
	n := idx + 1
	if tag.Type != token.TagType || n == len(t.tks) || t.tks[n].Position.Line != tag.Position.Line || t.tks[n].Type == token.CommentType {
		return -1
	}
	switch tag.Value {
	case tagStr, tagInt, tagFloat, tagBool, tagNull:
		if c := t.tks[n]; isKeyScalar(c) || c.Type == token.LiteralType || c.Type == token.FoldedType {
			return -1
		}
	}
	return n
}

// startsKey reports whether the block node starting at token idx is an
// implicit key: its properties, then one scalar or one flow collection,
// then a ':', all on one line. It reads at most to the end of the line,
// once per explicit entry.
func (t *tokenPass) startsKey(idx int) bool {
	tks := t.tks
	line := tks[idx].Position.Line
	on := func(j int) bool { return j < len(tks) && tks[j].Position.Line == line }
	j := idx
	for on(j) && (tks[j].Type == token.TagType || tks[j].Type == token.AnchorType) {
		if tks[j].Type == token.AnchorType {
			j++ // the anchor name
		}
		j++
	}
	switch {
	case on(j) && (tks[j].Type == token.SequenceStartType || tks[j].Type == token.MappingStartType):
		depth := 0
		for on(j) {
			switch tks[j].Type {
			case token.SequenceStartType, token.MappingStartType:
				depth++
			case token.SequenceEndType, token.MappingEndType:
				depth--
			default:
			}
			j++
			if depth == 0 {
				break
			}
		}
	case on(j) && isKeyScalar(tks[j]):
		j++
	}
	return on(j) && tks[j].Type == token.MappingValueType
}

// flowKey is the explicit entry ('?') open in a flow collection, from its
// '?' to its ':', the ',' that ends its entry or the closing bracket (01 req
// 8, 10, 12). goccy pairs the '?' with the one node after it, and a tag
// with its content only when both are on one line and goccy groups them
// (ungroupedContent): "{? !!str\n  x}" reads as {"": x}, and with a node
// after it the key becomes a mapping ("{p: q,\n ? !!str\n  x\n  r: s}" as
// {p: q, "": {"x r": s}}), where YAML 1.2 reads the key x and the key
// "x r". As in block context, a key of more than one node, or a tag goccy
// does not group with its content, is RZ-CFG-001 msgExplicitKey at the
// offending node, while the file has no other finding.
type flowKey struct {
	open  bool
	nodes int
}

// flowKey notes a '?' at at, token idx, directly in the innermost flow
// collection: a '?' inside an open key is a node of that key; any other
// opens a key.
func (t *tokenPass) flowKey(at tree.Pos, idx int) {
	if k := &t.stack[len(t.stack)-1].key; k.open {
		t.flowKeyNode(at, idx)
	} else {
		t.flowShapeKey(at)
		*k = flowKey{open: true}
	}
	t.flowNode(-1)
}

// flowKeyNode notes a node starting at token idx, at at, directly in the
// innermost flow collection: a scalar, a property that no property before
// it started the node of, or a nested collection. In an open explicit key,
// a second node is RZ-CFG-001, and a tag that starts the key must be
// grouped with its content (flowKeyTag). The node then takes its place in
// the entry (flowShapeNode).
func (t *tokenPass) flowKeyNode(at tree.Pos, idx int) {
	if t.lastProperty {
		return
	}
	if k := &t.stack[len(t.stack)-1].key; k.open {
		k.nodes++
		switch {
		case k.nodes > 1:
			t.misread(at, msgExplicitKey)
		case t.tks[idx].Type == token.TagType:
			t.flowKeyTag(idx)
		}
		if t.fatal {
			return
		}
	}
	t.flowShapeNode(at)
}

// flowKeyTag checks the tag at token idx that starts an explicit key in a
// flow collection: its content, if any, must be on its line and grouped
// with it by goccy. A ',', ':' or closing bracket after it leaves the
// tagged node empty, which goccy reads as YAML 1.2 does.
func (t *tokenPass) flowKeyTag(idx int) {
	j := idx + 1
	for j < len(t.tks) && t.tks[j].Type == token.CommentType {
		j++
	}
	if j == len(t.tks) {
		return
	}
	switch t.tks[j].Type {
	case token.CollectEntryType, token.MappingValueType, token.SequenceEndType, token.MappingEndType:
		return
	default:
	}
	if t.tks[j].Position.Line == t.tks[idx].Position.Line && t.ungroupedContent(idx) < 0 {
		return
	}
	t.misread(t.posOf(t.tks[j]), msgExplicitKey)
}
