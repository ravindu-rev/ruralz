// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Messages of the flow entry checks.
const (
	msgFlowSeqEntry = "syntax error: ',' or ']' must be specified"
	msgFlowMapEntry = "syntax error: ',' or '}' must be specified"
	msgFlowColon    = "syntax error: a ':' followed by a character other than white space or a flow indicator is part of a plain scalar; put a space after the ':' or quote the scalar"
	msgFlowColonEnd = "syntax error: a ':' directly before a flow indicator ends a plain scalar inside a flow collection; put a space after the ':' or quote the scalar"
)

// flowShape is the shape of the current entry of an open flow collection
// (01 req 8, 9, 10; 11 req 17, 26). A YAML 1.2 flow entry is an optional
// '?', at most one node, at most one ':' and at most one node, ended by a
// ',' or the closing bracket. goccy needs no ',' between entries: inside a
// flow sequence it parses "key:" entries with its block mapping code, which
// takes siblings and nesting from columns across lines, so "[\nk:\n k:\n
// k: v\n]" read as {k: {k: {k: v}}} and "[\n a: 1\n b: 2\n]" as one
// mapping, where YAML 1.2 refuses both. The token pass counts a flow
// collection as one level and charges one path and one inserted null per
// entry (flowcost.go), so goccy built every nested level and path before
// the converter's depth bound ran (a ladder of 1 MiB took 567 MiB), and
// parsed comma-less entries in time quadratic in their number (80,000 took
// 17.9 s and 24.9 GiB). An entry that holds more is RZ-CFG-001 at its first
// extra token, with goccy's own message for a missing ',', while the file
// has no other finding: then goccy's parse never sees more than one pair in
// an entry.
type flowShape struct {
	explicit bool // the '?' of an explicit key
	key      bool // a node before the ':'
	colon    bool // the ':'
	value    bool // a node after the ':'
}

// flowEntryMessage returns the message of an entry of the innermost flow
// collection that holds more than one entry's tokens.
func (t *tokenPass) flowEntryMessage() string {
	if t.stack[len(t.stack)-1].cost.seq {
		return msgFlowSeqEntry
	}
	return msgFlowMapEntry
}

// flowShapeNode notes a node starting at at directly in the innermost flow
// collection: the entry's key, or after its ':' its value.
func (t *tokenPass) flowShapeNode(at tree.Pos) {
	s := &t.stack[len(t.stack)-1].shape
	switch {
	case s.value, s.key && !s.colon:
		t.misread(at, t.flowEntryMessage())
	case s.colon:
		s.value = true
	default:
		s.key = true
	}
}

// flowShapeKey notes a '?' at at that opens an explicit key directly in the
// innermost flow collection: it must start its entry.
func (t *tokenPass) flowShapeKey(at tree.Pos) {
	s := &t.stack[len(t.stack)-1].shape
	if s.explicit || s.key || s.colon {
		t.misread(at, t.flowEntryMessage())
		return
	}
	s.explicit = true
}

// flowShapeColon notes the ':' token idx, at at, directly in the innermost
// flow collection. An entry has one ':'. goccy also makes a ':' a value
// indicator while a flow mapping is open whatever follows it, but in YAML
// 1.2 a ':' followed by a character a plain scalar can hold (ns-plain-safe:
// not white space nor a flow indicator) belongs to a plain scalar, unless
// it follows a JSON-like node (a quoted scalar or a flow collection): goccy
// reads "{app:web}" as {app: web} and "{x: [10:30]}" as {x: [{10: 30}]},
// where YAML 1.2 reads {"app:web": null} and {x: ["10:30"]}, so one flow
// sequence had two meanings depending on whether a flow mapping was open
// around it (01 req 8, 12). Such a ':' is RZ-CFG-001.
func (t *tokenPass) flowShapeColon(at tree.Pos, idx int) {
	if t.errs > 0 {
		return
	}
	if c, ok := t.charAfter(idx); ok && !isWhite(c) && strings.IndexByte(",[]{}", c) < 0 && !t.afterJSONNode(idx) {
		t.fail(at, msgFlowColon)
		return
	}
	s := &t.stack[len(t.stack)-1].shape
	if s.colon {
		t.fail(at, t.flowEntryMessage())
		return
	}
	s.colon = true
}

// charAfter returns the source character just after the one-character
// token idx, if there is one.
func (t *tokenPass) charAfter(idx int) (byte, bool) {
	tk := t.tks[idx]
	off := t.src.offset(tk.Position.Line, t.col(tk))
	if off < 0 || off+1 >= len(t.src.text) || t.src.text[off] != tk.Value[0] {
		return 0, false
	}
	return t.src.text[off+1], true
}

// afterJSONNode reports whether the token before token idx, comments
// aside, ends a JSON-like node: a quoted scalar or a flow collection, after
// which YAML 1.2 reads a ':' as a value indicator whatever follows it.
func (t *tokenPass) afterJSONNode(idx int) bool {
	for j := idx - 1; j >= 0; j-- {
		switch t.tks[j].Type {
		case token.CommentType:
			continue
		case token.DoubleQuoteType, token.SingleQuoteType, token.SequenceEndType, token.MappingEndType:
			return true
		default:
			return false
		}
	}
	return false
}

// flowColonEnd checks a plain scalar token idx in a flow collection whose
// text, value, ends with ':' (01 req 8, 12). Outside a flow mapping goccy
// keeps a ':' that a flow indicator follows in the scalar ("[a:]" as
// ["a:"]), where YAML 1.2 reads a value indicator ([{a: null}]): such a
// scalar is RZ-CFG-001 at the ':'.
func (t *tokenPass) flowColonEnd(idx int, at tree.Pos, value string) {
	if !strings.HasSuffix(value, ":") {
		return
	}
	tk := t.tks[idx]
	off := t.src.offset(tk.Position.Line, t.col(tk))
	if off < 0 {
		return
	}
	end := plainEnd(t.src.text, off, tk.Value)
	if end < 0 || end >= len(t.src.text) || strings.IndexByte(",[]{}", t.src.text[end]) < 0 {
		return
	}
	at.Column += int32(utf8.RuneCountInString(t.src.text[off : end-1])) //nolint:gosec // G115: a column of a file under MaxBytes fits in int32
	t.misread(at, msgFlowColonEnd)
}
