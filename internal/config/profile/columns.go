// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"cmp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml/token"
)

// columnTable corrects goccy's columns to code-point columns of the source
// (01 req 14, 15). goccy's scanner counts its column short in two places:
// it steps over a tag's '!' without counting it, so every later token on
// the tag's line, the next tag included, is one column to the left for
// each tag before it ("!!str a: !!int 1" puts the second tag at 9, not
// 10); and it steps over most tabs outside a quoted scalar without
// counting them, so a token after a tab on its line is one column to the
// left per tab ("kind:\tRoute" puts Route at 6, not 7), while a tab it
// reads as part of a token or a quoted scalar is counted. It also counts
// long: a plain scalar that spaces end its line ("- a  ") is placed one
// column to the right per space, and so is the comment after a block
// scalar header. Which tabs goccy counts depends on the scanner's state,
// so the table does not predict them: every token is located in the
// source (columnBuilder.find), and the table records the correction that
// places it there. A token that cannot be located keeps goccy's column,
// corrected for the tags before it. The tokens keep goccy's columns,
// which its parser and the split parse compare with each other. Every
// position this package reports or uses for the depth algorithm goes
// through fix.
//
// The table holds, per line, the correction of goccy's columns from a
// column on, sorted by line and column: a token gets the correction of the
// last entry at or left of its column on its line. A tag adds one to the
// correction just past its column; a located token whose real column the
// correction so far misses adds an entry at its own column. goccy's columns
// do not decrease along a line (a tag takes at least two characters while
// it shifts later tokens by one), so the entries come in order.
type columnTable []colShift

// colShift corrects goccy's columns at and after col on line by delta.
type colShift struct{ line, col, delta int }

func compareShift(a, b colShift) int {
	if c := cmp.Compare(a.line, b.line); c != 0 {
		return c
	}
	return cmp.Compare(a.col, b.col)
}

// fix returns the real column of a token that goccy places at line and
// col.
func (ct columnTable) fix(line, col int) int {
	i, found := slices.BinarySearchFunc(ct, colShift{line: line, col: col}, compareShift)
	if !found {
		i--
	}
	if i < 0 || ct[i].line != line {
		return col
	}
	return col + ct[i].delta
}

// columnsOf returns the column table of a segment's tokens, whose source is
// text.
func columnsOf(tks token.Tokens, text string) columnTable {
	b := columnBuilder{text: text, cline: 1, ccol: 1}
	for _, tk := range tks {
		if tk.Position != nil {
			b.add(tk)
		}
	}
	b.endLine()
	if !slices.IsSortedFunc(b.ct, compareShift) {
		slices.SortStableFunc(b.ct, compareShift)
	}
	return b.ct
}

// columnBuilder builds a column table token by token.
type columnBuilder struct {
	ct columnTable
	// line is the line of the last token added, delta the correction of
	// its columns from the last entry on, and pending the corrections of
	// the tags on it that no token has reached yet.
	line, delta int
	pending     []colShift

	// The search cursor is at byte offset off, line cline and code-point
	// column ccol of text: just past the last token located. invalid is
	// set when a token of the cursor's line cannot be located or has no
	// known end, so the rest of that line keeps goccy's columns as
	// corrected for tags; header when the last token other than a comment
	// was a block scalar header.
	text            string
	off, cline      int
	ccol            int
	invalid, header bool
}

// add notes the next token.
func (b *columnBuilder) add(tk *token.Token) {
	line, col := tk.Position.Line, tk.Position.Column
	if line != b.line {
		b.endLine()
		b.line, b.delta = line, 0
	}
	for len(b.pending) > 0 && b.pending[0].col <= col {
		b.put(b.pending[0])
		b.pending = b.pending[1:]
	}
	if r, ok := b.find(tk); ok && r-col != b.delta {
		b.put(colShift{line: line, col: col, delta: r - col})
	}
	if tk.Type == token.TagType {
		b.pending = append(b.pending, colShift{line: line, col: col + 1, delta: b.delta + 1})
	}
}

// endLine records the tag corrections of the line no token reached, which
// still apply to positions past the tags (an implicit null after "? !!str").
func (b *columnBuilder) endLine() {
	for _, p := range b.pending {
		b.put(p)
	}
	b.pending = b.pending[:0]
}

// put records an entry; of two entries at one column, the first is kept.
func (b *columnBuilder) put(e colShift) {
	if n := len(b.ct); n > 0 && b.ct[n-1].line == e.line && b.ct[n-1].col >= e.col {
		return
	}
	b.ct = append(b.ct, e)
	b.delta = e.delta
}

// find locates token tk in the source and returns its real column: the
// first character after the previous token, white space skipped, which
// must be the token's first character. It moves the cursor past the token.
func (b *columnBuilder) find(tk *token.Token) (int, bool) {
	line := tk.Position.Line
	header := b.header
	if tk.Type != token.CommentType {
		b.header = tk.Type == token.LiteralType || tk.Type == token.FoldedType
	}
	switch {
	case line < b.cline:
		return 0, false
	case line > b.cline:
		b.toLine(line)
	}
	switch {
	case tk.Type == token.StringType && strings.Trim(tk.Value, " \t\n") == "":
		// An empty scalar goccy adds for or after a block scalar's
		// content, which it may place on the line of the next key: no
		// source text of its own.
		return 0, false
	case tk.Type == token.StringType && header:
		// A block scalar's content: the next token starts a later line.
		b.invalid = true
		return 0, false
	}
	if b.invalid {
		return 0, false
	}
	for b.off < len(b.text) && (b.text[b.off] == ' ' || b.text[b.off] == '\t') {
		b.off++
		b.ccol++
	}
	first, ok := firstChar(tk)
	if !ok || b.off >= len(b.text) || b.text[b.off] != first {
		b.invalid = true
		return 0, false
	}
	col := b.ccol
	end := tokenEnd(tk, b.text, b.off)
	if end < 0 {
		b.invalid = true
		return col, true
	}
	for b.off < end {
		if b.text[b.off] == '\n' {
			b.off++
			b.cline, b.ccol = b.cline+1, 1
			continue
		}
		_, size := utf8.DecodeRuneInString(b.text[b.off:])
		b.off += size
		b.ccol++
	}
	return col, true
}

// toLine moves the cursor to the start of a later line.
func (b *columnBuilder) toLine(line int) {
	b.invalid = false
	for b.cline < line {
		nl := strings.IndexByte(b.text[b.off:], '\n')
		if nl < 0 {
			b.invalid = true
			return
		}
		b.off += nl + 1
		b.cline++
	}
	b.ccol = 1
}

// firstChar returns the character a token starts with in the source.
func firstChar(tk *token.Token) (byte, bool) {
	switch tk.Type {
	case token.CommentType:
		return '#', true
	case token.DoubleQuoteType:
		return '"', true
	case token.SingleQuoteType:
		return '\'', true
	case token.LiteralType:
		return '|', true
	case token.FoldedType:
		return '>', true
	case token.InvalidType:
		return 0, false
	default:
		if tk.Value == "" {
			return 0, false
		}
		return tk.Value[0], true
	}
}

// tokenEnd returns the byte offset just past the source text of token tk,
// which starts at offset off of text, or -1 when it is not known.
func tokenEnd(tk *token.Token, text string, off int) int {
	switch tk.Type {
	case token.CommentType:
		return lineEnd(text, off)
	case token.DoubleQuoteType, token.SingleQuoteType:
		return quoteEnd(text, off)
	case token.LiteralType, token.FoldedType:
		// The indicator and its chomping and indentation indicators (the
		// keep-chomping rewrite may have added one goccy reports).
		i := off + 1
		for i < len(text) && strings.IndexByte("+-0123456789", text[i]) >= 0 {
			i++
		}
		return i
	default:
	}
	if strings.HasPrefix(text[off:], tk.Value) {
		return off + len(tk.Value)
	}
	if end := plainEnd(text, off, tk.Value); end >= 0 {
		return end
	}
	return foldedEnd(tk, text, off)
}

// foldedEnd returns the offset just past a plain scalar over several lines
// that starts at offset off, or -1. goccy folds its value, but its Origin
// holds the source text, whose last line, with its indentation, starts the
// scalar's last line in the source.
func foldedEnd(tk *token.Token, text string, off int) int {
	origin := strings.TrimRight(strings.TrimLeft(tk.Origin, " \t\n"), " \t\n")
	nl := strings.LastIndexByte(origin, '\n')
	if nl < 0 || !strings.HasPrefix(text[off:], origin[:strings.IndexByte(origin, '\n')]) {
		return -1
	}
	// The start of the scalar's last line.
	i := off
	for range strings.Count(origin, "\n") {
		j := strings.IndexByte(text[i:], '\n')
		if j < 0 {
			return -1
		}
		i += j + 1
	}
	if last := origin[nl+1:]; strings.HasPrefix(text[i:], last) {
		return i + len(last)
	}
	return -1
}

// quoteEnd returns the offset just past the closing quote of the quoted
// scalar whose opening quote is at offset off, or -1.
func quoteEnd(text string, off int) int {
	q := text[off]
	for i := off + 1; i < len(text); i++ {
		switch c := text[i]; {
		case c == '\\' && q == '"':
			i++
		case c == q && q == '\'' && i+1 < len(text) && text[i+1] == '\'':
			i++
		case c == q:
			return i + 1
		}
	}
	return -1
}

// plainEnd returns the offset just past the plain scalar of value v that
// starts at offset off, where goccy left out the tabs inside the scalar,
// or -1 when the source holds other text.
func plainEnd(text string, off int, v string) int {
	i := off
	for j := 0; j < len(v); {
		switch {
		case i >= len(text):
			return -1
		case text[i] == v[j]:
			i++
			j++
		case text[i] == '\t':
			i++
		default:
			return -1
		}
	}
	return i
}
