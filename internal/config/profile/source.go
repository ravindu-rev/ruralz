// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// scalarFix is the YAML 1.2 reading of a scalar token whose value or
// position goccy's scanner gets wrong (01 req 12, 14): the token pass reads
// quoted and block scalars, and plain scalars goccy scans as folded text,
// from the source (quoted.go, literal.go, plain.go) and records the result
// here, keyed by the token (or a block scalar's header). The converter
// takes the value and position from the record when there is one.
type scalarFix struct {
	// text is the value; set says it replaces goccy's.
	text string
	set  bool
	// pos replaces goccy's position when known.
	pos tree.Pos
}

// fixes maps tokens to their YAML 1.2 readings, for one document segment.
type fixes map[*token.Token]scalarFix

// srcCursor maps the 1-based lines and code-point columns of a segment's
// tokens to byte offsets in its text. Tokens come in source order, so a
// segment's lookups move forward and cost one pass over it; a lookup
// behind the cursor walks back to the start of its line.
type srcCursor struct {
	text string
	// off is the byte offset of the 1-based line and code-point column
	// (line, col).
	off, line, col int
}

// newCursor returns a cursor at the start of text.
func newCursor(text string) srcCursor {
	return srcCursor{text: text, line: 1, col: 1}
}

// offset returns the byte offset of a line and code-point column, or -1
// when the text has no such position.
func (c *srcCursor) offset(line, col int) int {
	if line < 1 || col < 1 {
		return -1
	}
	if line < c.line || (line == c.line && col < c.col) {
		// Back to the start of the cursor's line, then up to the line.
		c.off, c.col = strings.LastIndexByte(c.text[:c.off], '\n')+1, 1
		for c.line > line {
			c.off = strings.LastIndexByte(c.text[:c.off-1], '\n') + 1
			c.line--
		}
	}
	for c.line < line {
		nl := strings.IndexByte(c.text[c.off:], '\n')
		if nl < 0 {
			return -1
		}
		c.off, c.line, c.col = c.off+nl+1, c.line+1, 1
	}
	for c.col < col {
		if c.off >= len(c.text) || c.text[c.off] == '\n' {
			return -1
		}
		_, size := utf8.DecodeRuneInString(c.text[c.off:])
		c.off += size
		c.col++
	}
	return c.off
}

// advance returns the line and code-point column reached from (line, col)
// at byte offset from after reading text up to byte offset to.
func advance(text string, from, to, line, col int) (int, int) {
	for i := from; i < to; {
		if text[i] == '\n' {
			line, col = line+1, 1
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		col++
		i += size
	}
	return line, col
}

// lineEnd returns the offset of the line break that ends the line holding
// byte offset off, or the length of the text.
func lineEnd(text string, off int) int {
	if nl := strings.IndexByte(text[off:], '\n'); nl >= 0 {
		return off + nl
	}
	return len(text)
}

// leadingSpaces returns the number of spaces that start s.
func leadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	return n
}

// markerAt reports whether line is a document marker ("---" or "...").
func markerAt(line string) bool {
	return markerLine(line, "---") || markerLine(line, "...")
}
