// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import "strings"

// Messages of the block scalar checks.
const (
	msgBlockLeadingSpaces = "syntax error: a leading empty line of a block scalar holds more spaces than its first line of content"
	msgBlockInFlow        = "syntax error: a block scalar is not allowed inside a flow collection"
	msgBlockHeaderRest    = "syntax error: a block scalar header holds at most one chomping and one indentation indicator, then white space or a comment"
	msgBlockScalarPlace   = "syntax error: a block scalar ('|' or '>') follows its key's ':', a '-' or a '?', on that line or on a later line indented past the entry's collection"
)

// headerValid reports whether the block scalar header whose indicator is at
// byte offset at of text is one YAML 1.2 accepts (c-b-block-header): the
// indicator, at most one indentation indicator (1 to 9) and one chomping
// indicator in either order, then only white space, or white space and a
// comment, up to the line break ("k: |--" is refused).
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
// byte offset at of text as YAML 1.2 reads it (YAML 1.2.2 section 8.1),
// where n is the indentation of the block collection around it, -1 at the
// document level. It returns the value, the number of lines after the
// header line the scalar holds (its content and the empty lines after it),
// and a message when YAML 1.2 refuses the scalar.
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
// 1.2 refuses. A line indented past n is content even when it starts with
// '#' (YAML Test Suite S98Z), where libyaml-family parsers read a comment.
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
			if sp > n && sp < most {
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
