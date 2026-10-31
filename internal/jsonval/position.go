// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import "unicode/utf8"

// Position is a source position: Line and Column are 1-based and Column
// counts Unicode code points, as diag.Location and tree.Pos do. Line breaks
// are LF, CR LF and a lone CR; an invalid UTF-8 byte counts as one column.
type Position struct {
	// Line is the 1-based line.
	Line int
	// Column is the 1-based column in code points.
	Column int
}

// Locator converts byte offsets of one input into positions. Queries in
// increasing offset order, as a Scanner yields tokens, cost O(n) in total;
// a query below the previous one restarts from the beginning.
type Locator struct {
	data []byte
	off  int
	line int
	col  int
}

// NewLocator returns a Locator over data.
func NewLocator(data []byte) *Locator {
	l := new(Locator)
	l.Reset(data)
	return l
}

// Reset starts over on data.
func (l *Locator) Reset(data []byte) {
	l.data = data
	l.off = 0
	l.line = 1
	l.col = 1
}

// Position returns the position of the byte at offset off. An offset inside
// a multi-byte character maps to that character; offsets past the end map
// to the position just after the last byte.
func (l *Locator) Position(off int) Position {
	if off < l.off {
		l.Reset(l.data)
	}
	off = min(off, len(l.data))
	for l.off < off {
		c := l.data[l.off]
		size := 1
		switch {
		case c == '\n':
			// LF after CR belongs to the CR's line break.
			if l.off == 0 || l.data[l.off-1] != '\r' {
				l.line++
				l.col = 1
			}
		case c == '\r':
			l.line++
			l.col = 1
		case c < utf8.RuneSelf:
			l.col++
		default:
			_, size = utf8.DecodeRune(l.data[l.off:])
			if l.off+size > off {
				// off is inside this character.
				return Position{Line: l.line, Column: l.col}
			}
			l.col++
		}
		l.off += size
	}
	return Position{Line: l.line, Column: l.col}
}

// PositionOf returns the position of offset off in data.
func PositionOf(data []byte, off int) Position {
	var l Locator
	l.Reset(data)
	return l.Position(off)
}
