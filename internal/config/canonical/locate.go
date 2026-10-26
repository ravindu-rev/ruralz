// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"encoding/binary"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// locator maps byte offsets to positions exactly as jsonval.Locator does
// (lines broken by LF, CR or CRLF, columns counted in code points, an
// offset inside a multi-byte character mapped to that character), but in
// constant time within the leading run of printable ASCII, where offset o
// is line 1, column o+1. Canonical content is one line of mostly ASCII,
// so the decoder's position per token costs next to nothing. Past that
// run it walks forward from the last query, eight bytes at a time through
// printable ASCII, so it is fastest for offsets in increasing order; it
// is not safe for concurrent use.
type locator struct {
	data []byte
	// regular is the length of the leading run of bytes in 0x20 to 0x7F.
	regular int
	off     int
	line    int
	col     int
}

func newLocator(data []byte) *locator {
	l := &locator{data: data, regular: printable(data)}
	l.restart()
	return l
}

// Word masks of the fast paths: a word whose bytes all lie in 0x20 to
// 0x7F has no line break and no multi-byte character, so each byte is one
// column. No byte below 0x20 survives subtracting 0x20 from every byte
// without setting its high bit, and no borrow crosses a byte that does
// not underflow, so the test is exact.
const (
	asciiLow  = 0x2020202020202020
	asciiHigh = 0x8080808080808080
)

// plain reports whether the word w holds only bytes in 0x20 to 0x7F.
func plain(w uint64) bool { return ((w-asciiLow)|w)&asciiHigh == 0 }

// printable returns the length of the leading run of data whose bytes lie
// in 0x20 to 0x7F.
func printable(data []byte) int {
	i := 0
	for len(data)-i >= 8 && plain(binary.LittleEndian.Uint64(data[i:])) {
		i += 8
	}
	for i < len(data) && data[i] >= 0x20 && data[i] < utf8.RuneSelf {
		i++
	}
	return i
}

// restart puts the walk at the end of the leading printable run.
func (l *locator) restart() { l.off, l.line, l.col = l.regular, 1, l.regular+1 }

// Position returns the position of the byte at offset off; offsets past
// the end map to the position just after the last byte, and negative ones
// to the first byte.
func (l *locator) Position(off int) jsonval.Position {
	off = max(0, min(off, len(l.data)))
	if off <= l.regular {
		return jsonval.Position{Line: 1, Column: off + 1}
	}
	if off < l.off {
		l.restart()
	}
	for l.off < off {
		if off-l.off >= 8 && plain(binary.LittleEndian.Uint64(l.data[l.off:])) {
			l.off += 8
			l.col += 8
			continue
		}
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
				return jsonval.Position{Line: l.line, Column: l.col}
			}
			l.col++
		}
		l.off += size
	}
	return jsonval.Position{Line: l.line, Column: l.col}
}
