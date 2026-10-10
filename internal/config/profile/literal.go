// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"

	"github.com/goccy/go-yaml/token"
)

// Messages of the block scalar checks.
const (
	msgBlockLeadingSpaces = "syntax error: a leading empty line of a block scalar holds more spaces than its first line of content"
	msgBlockInFlow        = "syntax error: a block scalar is not allowed inside a flow collection"
	msgBlockScalar        = "syntax error: the block scalar does not end at the first line indented less than its content"
	msgBlockHeader        = "syntax error: cannot find the block scalar's '|' or '>'"
	msgBlockHeaderRest    = "syntax error: a block scalar header holds at most one chomping and one indentation indicator, then white space or a comment"
)

// headerValid reports whether the block scalar header whose indicator is at
// byte offset at of text is one YAML 1.2 accepts (c-b-block-header): the
// indicator, at most one indentation indicator (1 to 9) and one chomping
// indicator in either order, then only white space, or white space and a
// comment, up to the line break. goccy reads two indicators and ignores the
// rest of the line ("k: |--" read as {k: "a"}; minor finding of the eighth
// WP-33 review).
func headerValid(text string, at int) bool {
	i := at + 1
	indent, chomp := false, false
	for ; i < len(text); i++ {
		switch c := text[i]; {
		case c >= '1' && c <= '9' && !indent:
			indent = true
			continue
		case (c == '+' || c == '-') && !chomp:
			chomp = true
			continue
		}
		break
	}
	rest := text[i:lineEnd(text, i)]
	trimmed := strings.TrimLeft(rest, " \t")
	return trimmed == "" || (trimmed[0] == '#' && len(trimmed) < len(rest))
}

// readBlockScalar reads the block scalar whose indicator ('|' or '>') is at
// byte offset at of text as YAML 1.2 reads it (01 req 12; YAML 1.2.2
// section 8.1), where n is the indentation of the block collection around
// it, -1 at the document level. goccy's own value drops the trailing white
// space of the last line with strip chomping, keeps a line break for an
// empty scalar with keep chomping and reads lines of spaces otherwise, so
// the value is taken from here. It returns the value, the number of lines
// after the header line the scalar holds (its content and the empty lines
// after it), and a message when YAML 1.2 refuses the scalar.
//
// The rules are those of the YAML Test Suite's reference reading, which
// PyYAML and libyaml share: the content indentation is n plus an explicit
// indentation indicator, or that of the first line with content, at least
// n+1; a line of spaces only up to that indentation is empty and gives a
// line break; a line holding more is content; the first other line
// indented less ends the scalar, as does a document marker. Folded lines
// join with a space unless empty lines or a more indented line come
// between them; strip chomping drops the last line break and the empty
// lines after it, clip keeps the line break and keep both. Every line ends
// with a line break, as Parse ends the text with one.
func readBlockScalar(text string, at, n int) (value string, lines int, msg string) {
	folded := text[at] == '>'
	indent, chomp := 0, byte(0)
	i := at + 1
header:
	for range 2 {
		if i >= len(text) {
			break
		}
		switch c := text[i]; {
		case c >= '1' && c <= '9' && indent == 0:
			indent = int(c - '0')
		case (c == '+' || c == '-') && chomp == 0:
			chomp = c
		default:
			break header
		}
		i++
	}
	body := len(text)
	if end := lineEnd(text, i); end < len(text) {
		body = end + 1
	}
	ind := n + indent
	if indent == 0 {
		var ok bool
		if ind, ok = detectIndent(text[body:], n); !ok {
			return "", 0, msgBlockLeadingSpaces
		}
	}
	var b strings.Builder
	breaks := 0
	first, prevPlain := true, false
scan:
	for off := body; off < len(text); {
		end := lineEnd(text, off)
		line := text[off:end]
		if markerAt(line) {
			break
		}
		sp := leadingSpaces(line)
		switch {
		case sp == len(line) && sp <= ind:
			breaks++
		case sp < ind:
			// A line indented less than the content ends the scalar.
			break scan
		default:
			content := line[ind:]
			plain := content != "" && content[0] != ' ' && content[0] != '\t'
			if !first {
				switch {
				case !folded || !prevPlain || !plain:
					b.WriteByte('\n')
				case breaks == 0:
					b.WriteByte(' ')
				}
			}
			for ; breaks > 0; breaks-- {
				b.WriteByte('\n')
			}
			b.WriteString(content)
			first, prevPlain = false, plain
		}
		lines++
		off = end + 1
	}
	if !first && chomp != '-' {
		b.WriteByte('\n')
	}
	if chomp == '+' {
		for ; breaks > 0; breaks-- {
			b.WriteByte('\n')
		}
	}
	return b.String(), lines, ""
}

// detectIndent returns the content indentation of a block scalar without
// an indentation indicator, whose lines are body, inside a collection
// indented n: the larger of n+1 and the spaces of its leading empty lines
// and of its first line with content. It reports false when a leading
// empty line holds more spaces than a first line of content, which YAML
// 1.2 refuses; a comment line there is no content but a comment after the
// scalar, as PyYAML and libyaml read it.
func detectIndent(body string, n int) (int, bool) {
	most := 0
	for off := 0; off < len(body); {
		end := lineEnd(body, off)
		line := body[off:end]
		if markerAt(line) {
			break
		}
		sp := leadingSpaces(line)
		if sp < len(line) {
			if sp > n && sp < most && line[sp] != '#' {
				return 0, false
			}
			most = max(most, sp)
			break
		}
		most = max(most, sp)
		off = end + 1
	}
	return max(n+1, most), true
}

// blockScalar reads the block scalar whose header is token idx from the
// source (readBlockScalar), records its value for the converter, and
// checks that goccy's tokens end the scalar where YAML 1.2 does (01 req 8,
// 12): the token after its content must start on the line that ends it,
// which is recorded for foldPlain (blockNext).
// goccy's scanner adds an empty plain scalar after the content of a block
// scalar followed by blank lines ("? >\n  \n:" scans as Folded, String "",
// String "\n", MappingValue); such a token holds no value and becomes a
// comment, which every parse skips. A header goccy reports elsewhere than
// at its indicator, a block scalar in a flow collection and content goccy
// reads to another line are RZ-CFG-001. Like the other checks that only
// keep goccy from misreading a file, it is skipped once the file has a
// finding.
func (t *tokenPass) blockScalar(idx int) {
	if t.errs > 0 {
		return
	}
	tk := t.tks[idx]
	at := t.posOf(tk)
	if t.flow > 0 {
		t.fail(at, msgBlockInFlow)
		return
	}
	// goccy reports the header of a tagged scalar at a blank before its
	// indicator when white space other than one space comes between them.
	off := t.src.offset(tk.Position.Line, t.col(tk))
	for off >= 0 && off < len(t.src.text) && (t.src.text[off] == ' ' || t.src.text[off] == '\t') {
		off++
	}
	if off < 0 || off == len(t.src.text) || (t.src.text[off] != '|' && t.src.text[off] != '>') {
		t.fail(at, msgBlockHeader)
		return
	}
	if !headerValid(t.src.text, off) {
		t.fail(at, msgBlockHeaderRest)
		return
	}
	n := -1
	if len(t.stack) > 0 {
		n = t.stack[len(t.stack)-1].col - 1
	}
	value, lines, msg := readBlockScalar(t.src.text, off, n)
	if msg != "" {
		t.fail(at, msg)
		return
	}
	t.fix(tk, scalarFix{text: value, set: true})
	// The content token, after a comment on the header's line.
	j := idx + 1
	for j < len(t.tks) && t.tks[j].Type == token.CommentType && t.tks[j].Position.Line == tk.Position.Line {
		j++
	}
	if j < len(t.tks) && t.tks[j].Type == token.StringType {
		j++
	}
	for j < len(t.tks) && t.tks[j].Type == token.StringType && strings.Trim(t.tks[j].Value, " \t\n") == "" {
		t.tks[j].Type = token.CommentType
		j++
	}
	end := tk.Position.Line + 1 + lines
	if j < len(t.tks) {
		if t.tks[j].Position.Line != end {
			t.fail(at, msgBlockScalar)
		}
		t.next = blockNext{idx: j, line: end}
		return
	}
	// Nothing follows: the scalar runs to the end of the text, apart from
	// a document marker.
	if rest := t.src.offset(end, 1); rest >= 0 && rest < len(t.src.text) && !markerAt(t.src.text[rest:lineEnd(t.src.text, rest)]) {
		t.fail(at, msgBlockScalar)
	}
}

// blockNext is the first token after a block scalar's content and the
// segment line it starts on, where the scalar ends (blockScalar). goccy's
// value of the content token is not its source text, so the plain scalar
// that may start there is found from this line (foldPlain): goccy's value
// drops the indentation of every line after the first, and the
// keep-chomping rewrite (chomp.go) adds the blank lines after the content,
// which put the scalar at another place with the rewrite than without it.
type blockNext struct {
	idx, line int
}
