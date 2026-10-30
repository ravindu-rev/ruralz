// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// scalarPos returns the file position of the character at byte offset off
// of the string n holds, text being the bytes of n's file as the load read
// them, when n is a plain scalar, or a single-quoted, double-quoted or JSON
// scalar on one line (docs/architecture/02-configuration-model.md
// "Diagnostics and source map"). In a quoted scalar the column counts the
// escapes written before the character, and a character an escape writes
// sits at its backslash; a plain scalar holds no escape, and one over
// several lines is folded back to them (plainPos). off may be the length
// of the value, past its last character, which a quoted scalar places at
// its closing quote and a plain one after its last character. false for a
// block scalar, a quoted scalar whose closing quote is on a later line, a
// value a default wrote, and text that does not hold n's value where n's
// position says, which only a file changed since it was read can do.
func scalarPos(n *tree.Node, off int, text []byte) (tree.Pos, bool) {
	if n == nil || n.Kind != tree.KindString || !n.Pos.Known() || n.Pos.Column < 1 || off < 0 || off > len(n.Text) {
		return tree.Pos{}, false
	}
	i, ok := lineStart(text, int(n.Pos.Line))
	if !ok {
		return tree.Pos{}, false
	}
	col := 1
	for col < int(n.Pos.Column) {
		if i >= len(text) || isBreak(text[i]) {
			return tree.Pos{}, false
		}
		_, size := utf8.DecodeRune(text[i:])
		i += size
		col++
	}
	// A tagged node starts at its tag: the scalar follows it on the line.
	if i < len(text) && text[i] == '!' {
		for i < len(text) && text[i] != ' ' && text[i] != '\t' && !isBreak(text[i]) {
			_, size := utf8.DecodeRune(text[i:])
			i += size
			col++
		}
		for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
			i++
			col++
		}
	}
	at := func(c int) (tree.Pos, bool) {
		return tree.Pos{File: n.Pos.File, Line: n.Pos.Line, Column: int32(c)}, true //nolint:gosec // G115: a column within a line of a file of at most 64 MiB.
	}
	switch n.Style {
	case tree.StylePlain:
		line, c, ok := plainPos(text, i, col, n.Pos.Line, n.Text, off)
		if !ok {
			return tree.Pos{}, false
		}
		return tree.Pos{File: n.Pos.File, Line: line, Column: int32(c)}, true //nolint:gosec // G115: a column within a line of a file of at most 64 MiB.
	case tree.StyleSingleQuoted:
		c, ok := quotedColumn(text, i, col, '\'', n.Text, off)
		if !ok {
			return tree.Pos{}, false
		}
		return at(c)
	case tree.StyleDoubleQuoted, tree.StyleJSON:
		c, ok := quotedColumn(text, i, col, '"', n.Text, off)
		if !ok {
			return tree.Pos{}, false
		}
		return at(c)
	default:
		return tree.Pos{}, false
	}
}

// lineStart returns the offset of the first byte of the 1-based line in
// text, where LF, CR LF and a lone CR each end a line, as the YAML and
// JSON front ends count lines.
func lineStart(text []byte, line int) (int, bool) {
	if line < 1 {
		return 0, false
	}
	i := 0
	for l := 1; l < line; l++ {
		j := bytes.IndexAny(text[i:], "\r\n")
		if j < 0 {
			return 0, false
		}
		i += j
		if text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n' {
			i++
		}
		i++
	}
	return i, true
}

// isBreak reports a line break byte.
func isBreak(c byte) bool { return c == '\n' || c == '\r' }

// lineEnd returns the offset of the line break that ends the line holding
// offset i of text, or the length of text.
func lineEnd(text []byte, i int) int {
	if j := bytes.IndexAny(text[i:], "\r\n"); j >= 0 {
		return i + j
	}
	return len(text)
}

// afterBreak returns the offset just past the line break at offset i of
// text: one byte, or two for CR LF.
func afterBreak(text []byte, i int) int {
	if text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n' {
		return i + 2
	}
	return i + 1
}

// plainPos returns the line and column of byte offset off of value, a
// plain scalar whose text starts at offset i of text, in column col of
// line. A plain scalar folds as YAML 1.2 does (sections 6.5 and 7.3.3): the
// white space that ends a line and starts the next is dropped, and the
// line break between two lines with content becomes one space, or one
// line feed per empty line between them; within a line the value is the
// text itself. A character the folding wrote sits right after the content
// of the line it ends, and off at the end of the value right after the
// last character. false when text does not hold value so folded.
func plainPos(text []byte, i, col int, line int32, value string, off int) (int32, int, bool) {
	var (
		pos              int
		start, startCol  = i, col
		cur              = line
		found            bool
		foundLn          int32
		foundCol         int
		breakLn          int32
		breakCol, newCol int
	)
	for {
		end := lineEnd(text, start)
		content := bytes.TrimRight(text[start:end], " \t")
		rest := value[pos:]
		if len(rest) <= len(content) && string(content[:len(rest)]) == rest {
			// The value ends on this line.
			if !found {
				foundLn, foundCol, found = cur, startCol+utf8.RuneCount(content[:off-pos]), true
			}
			return foundLn, foundCol, true
		}
		if !strings.HasPrefix(rest, string(content)) {
			return 0, 0, false
		}
		if !found && off < pos+len(content) {
			foundLn, foundCol, found = cur, startCol+utf8.RuneCount(content[:off-pos]), true
		}
		pos += len(content)
		breakLn, breakCol = cur, startCol+utf8.RuneCount(content)
		// The next line with content, counting the empty lines before it.
		empty, next := 0, end
		for {
			if next >= len(text) {
				return 0, 0, false
			}
			next = afterBreak(text, next)
			cur++
			j := next
			for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
				j++
			}
			if j < len(text) && !isBreak(text[j]) {
				start, newCol = j, 1+j-next
				break
			}
			empty++
			next = j
		}
		startCol = newCol
		sep := " "
		if empty > 0 {
			sep = strings.Repeat("\n", empty)
		}
		if !strings.HasPrefix(value[pos:], sep) {
			return 0, 0, false
		}
		if !found && off < pos+len(sep) {
			foundLn, foundCol, found = breakLn, breakCol, true
		}
		pos += len(sep)
	}
}

// quotedColumn reads the quoted scalar whose opening quote q is at offset
// i of text, in column col, and returns the column where the character at
// byte offset off of its value starts: the closing quote's for off at the
// end. It decodes the escapes of a double-quoted YAML scalar, which hold
// those of a JSON string, or the doubled quote of a single-quoted one, and
// fails at a line break before the closing quote, at an escape it cannot
// read, and when the value it decodes is not want.
func quotedColumn(text []byte, i, col int, q byte, want string, off int) (int, bool) {
	if i >= len(text) || text[i] != q {
		return 0, false
	}
	i++
	col++
	dec := make([]byte, 0, len(want))
	found := -1
	for {
		if i >= len(text) || isBreak(text[i]) {
			return 0, false
		}
		if found < 0 && len(dec) == off {
			found = col
		}
		c := text[i]
		switch {
		case q == '\'' && c == '\'' && i+1 < len(text) && text[i+1] == '\'':
			dec = append(dec, '\'')
			i += 2
			col += 2
		case c == q:
			if string(dec) != want || found < 0 {
				return 0, false
			}
			return found, true
		case q == '"' && c == '\\':
			r, size, ok := escape(text[i+1:])
			if !ok {
				return 0, false
			}
			dec = utf8.AppendRune(dec, r)
			// Escape sequences are ASCII: one column per byte.
			i += 1 + size
			col += 1 + size
		default:
			_, size := utf8.DecodeRune(text[i:])
			dec = append(dec, text[i:i+size]...)
			i += size
			col++
		}
	}
}

// escape decodes the escape sequence that s starts after its backslash, as
// YAML 1.2 double-quoted scalars and JSON strings write them, and returns
// the character and the bytes it used; false for an escaped line break,
// which continues the scalar on the next line, and for any escape a YAML
// or JSON reader refuses.
func escape(s []byte) (rune, int, bool) {
	if len(s) == 0 {
		return 0, 0, false
	}
	switch s[0] {
	case '0':
		return 0, 1, true
	case 'a':
		return 7, 1, true
	case 'b':
		return 8, 1, true
	case 't', '\t':
		return '\t', 1, true
	case 'n':
		return '\n', 1, true
	case 'v':
		return 11, 1, true
	case 'f':
		return 12, 1, true
	case 'r':
		return '\r', 1, true
	case 'e':
		return 0x1B, 1, true
	case ' ', '"', '/', '\\':
		return rune(s[0]), 1, true
	case 'N':
		return 0x85, 1, true
	case '_':
		return 0xA0, 1, true
	case 'L':
		return 0x2028, 1, true
	case 'P':
		return 0x2029, 1, true
	case 'x':
		return codePoint(s, 2)
	case 'u':
		return codePoint(s, 4)
	case 'U':
		return codePoint(s, 8)
	default:
		return 0, 0, false
	}
}

// codePoint decodes a \x, \u or \U escape of digits hexadecimal digits
// whose letter starts s; a \u high surrogate followed by a \u low
// surrogate is the pair's character.
func codePoint(s []byte, digits int) (rune, int, bool) {
	v, ok := hexValue(s[1:], digits)
	if !ok {
		return 0, 0, false
	}
	size := 1 + digits
	switch {
	case digits == 4 && v >= 0xD800 && v <= 0xDBFF:
		rest := s[size:]
		if len(rest) < 6 || rest[0] != '\\' || rest[1] != 'u' {
			return 0, 0, false
		}
		low, ok := hexValue(rest[2:], 4)
		if !ok || low < 0xDC00 || low > 0xDFFF {
			return 0, 0, false
		}
		v = 0x10000 + (v-0xD800)<<10 + (low - 0xDC00)
		size += 6
	case v >= 0xD800 && v <= 0xDFFF, v > utf8.MaxRune:
		return 0, 0, false
	}
	return rune(v), size, true //nolint:gosec // G115: v is at most utf8.MaxRune here.
}

// hexValue returns the value of the first digits hexadecimal digits of s.
func hexValue(s []byte, digits int) (uint32, bool) {
	if len(s) < digits {
		return 0, false
	}
	var v uint32
	for _, c := range s[:digits] {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint32(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint32(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | uint32(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}
