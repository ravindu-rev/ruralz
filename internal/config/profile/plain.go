// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Messages of the plain scalar checks.
const (
	msgPlainColon         = "syntax error: mapping values are not allowed in this context"
	msgPlainComment       = "syntax error: a plain scalar cannot continue after a comment"
	msgPlainSpace         = "syntax error: unexpected white space around a plain scalar"
	msgPlainStart         = "syntax error: cannot find where the plain scalar starts"
	msgExplicitKeyLine    = "an explicit key ('?') must start on the line of its '?'"
	msgPlainFlowIndicator = "syntax error: a flow indicator cannot be inside a plain scalar in a flow collection"
)

// plain checks the plain scalar token idx (01 req 8, 12, 14), which is not
// the content of a block scalar.
//
// A plain scalar holding a tab is read again from the source (tabbedPlain):
// goccy's scanner leaves the tabs inside a plain scalar out of its value,
// so "x: 1\t2" read as the integer 12 and the keys "bc" and "b\tc" as one
// (01 req 11, 12, 15). A plain scalar over several lines is read again
// from the source (foldPlain). goccy's scanner reads a line of one that
// starts with '-' right of the last ':' or '-' as the start of a folded
// block ("raw folded" text): from there on it keeps comments and ": " in
// the value, keeps the line breaks and spaces a folded block scalar keeps,
// and reports the scalar at a later line ("description: run the job\n  -v
// # verbose" read as "run the job -v # verbose" at 2:9); and it folds two
// empty lines of spaces into one line feed ("a\n  \n  \n  c" read as
// "a\nc").
//
// Every other plain token must hold what a YAML 1.2 plain scalar can: not
// empty, no white space at either end or around a line break, no comment
// ("#" after white space), no ": ", no flow indicator inside a flow
// collection, and a first character that is not an indicator, nor '?' or
// '-' before white space. goccy breaks these rules only where it misreads
// the source: after an empty block scalar and a blank line it scans the
// next line's first node as plain text ("x: |\n\n&a q: 1" gives the key
// "&a q"), and it scans "?" at the end of a line, with its key on the
// next, as the text "? a". Such a token is RZ-CFG-001, except that one
// starting with '&' or '*' is RZ-CFG-003 and one starting with a tag other
// than a core tag is RZ-CFG-004, as the anchor, alias or tag written there
// would be. Like the other checks that only keep goccy from misreading a
// file, the RZ-CFG-001 cases are skipped once the file has a finding.
func (t *tokenPass) plain(idx int) {
	tk := t.tks[idx]
	value := tk.Value
	switch {
	case t.errs > 0:
	case strings.Contains(strings.TrimSpace(tk.Origin), "\n"):
		f, at, msg := t.foldPlain(idx)
		if msg != "" {
			if !at.Known() {
				at = t.posOf(tk)
			}
			t.fail(at, msg)
			return
		}
		t.fix(tk, f)
		value = f.text
	case strings.IndexByte(tk.Origin, '\t') >= 0:
		text, ok := t.tabbedPlain(idx)
		if !ok {
			t.fail(t.posOf(tk), msgPlainStart)
			return
		}
		if text != value {
			t.fix(tk, scalarFix{text: text, set: true})
			value = text
		}
	}
	at := t.posOf(tk)
	if value == "" || strings.TrimSpace(value) != value {
		t.misread(at, msgPlainSpace)
		return
	}
	switch c := value[0]; {
	case value == "?" && !t.colonFollows(idx):
		// goccy scans a '?' that a line break or a flow indicator
		// follows as the plain scalar "?", where YAML 1.2 reads an explicit
		// key indicator with an empty key ("b:\n?" is {b: null, null:
		// null}), as "? # c" is, and refuses "x: ?" and "[?]". Before ':'
		// ("?: x") it is a valid plain scalar.
		t.misread(at, msgEmptyExplicitKey)
		return
	case c == '&':
		t.report(codeAnchor, at, "anchors are not allowed ("+clip(firstWord(value))+")")
		return
	case c == '*':
		t.report(codeAnchor, at, "aliases are not allowed ("+clip(firstWord(value))+")")
		return
	case c == '!':
		if tag := firstWord(value); !isCoreTag(tag) {
			t.report(codeTag, at, "tag "+clip(tag)+" is not allowed; only the YAML 1.2 core tags are")
			return
		}
		t.misread(at, "syntax error: unexpected "+strconv.QuoteRune(rune(c)))
		return
	case strings.IndexByte(",[]{}#|>'\"%@`", c) >= 0:
		t.misread(at, "syntax error: unexpected "+strconv.QuoteRune(rune(c)))
		return
	case (c == '?' || c == '-') && len(value) > 1 && isWhite(value[1]):
		msg := "syntax error: unexpected " + strconv.QuoteRune(rune(c))
		if c == '?' {
			msg = msgExplicitKeyLine
		}
		t.misread(at, msg)
		return
	}
	switch {
	case containsAny(value, " #", "\t#", "\n#"):
		t.misread(at, msgPlainComment)
	case containsAny(value, ": ", ":\t", ":\n"):
		t.misread(at, msgPlainColon)
	case containsAny(value, "\n ", "\n\t", " \n", "\t\n"):
		t.misread(at, msgPlainSpace)
	case t.flow > 0 && strings.ContainsAny(value, ",[]{}"):
		t.misread(at, msgPlainFlowIndicator)
	case t.flow > 0:
		t.flowColonEnd(idx, at, value)
	}
}

// misread fails the file with RZ-CFG-001 at a token goccy misread, unless
// the file has a finding already: it is never parsed then, and the scan
// goes on collecting RZ-CFG-003 and RZ-CFG-004 (01 req 8).
func (t *tokenPass) misread(at tree.Pos, msg string) {
	if t.errs == 0 {
		t.fail(at, msg)
	}
}

// isWhite reports a space, a tab or a line break.
func isWhite(c byte) bool { return c == ' ' || c == '\t' || c == '\n' }

// firstWord returns s up to its first white space.
func firstWord(s string) string {
	if i := strings.IndexAny(s, " \t\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// containsAny reports whether s contains any of subs.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// foldPlain reads the plain scalar token idx, which spans several lines,
// from the source as YAML 1.2 reads a plain scalar. The scalar starts at
// the first character after the token before it (or of the segment, for
// its first token, or of the line that ends a block scalar, for the token
// after its content), which must be the start of goccy's text, and spans the
// lines goccy's text spans, up to the next token when that starts on its
// last line. Its lines fold as a plain scalar's do: trimmed, joined by a
// space, or by one line feed per empty line between them. A comment ends
// the scalar, so a line of content after a comment is RZ-CFG-001, as is
// ": " or a ':' that ends a line, which makes a multi-line implicit key;
// the message comes with the position of the offending text, or the zero
// position when the scalar's start cannot be found.
func (t *tokenPass) foldPlain(idx int) (scalarFix, tree.Pos, string) {
	tk := t.tks[idx]
	text := t.src.text
	start, line, col := 0, 1, 1
	switch {
	case idx == t.next.idx:
		// The first token after a block scalar's content starts on the
		// line that ends the scalar (blockNext).
		line = t.next.line
		if start = t.src.offset(line, 1); start < 0 {
			return scalarFix{}, tree.Pos{}, msgPlainStart
		}
	case idx > 0:
		prev := t.tks[idx-1]
		line, col = prev.Position.Line, t.col(prev)
		if start = t.src.offset(line, col); start < 0 {
			return scalarFix{}, tree.Pos{}, msgPlainStart
		}
		if prev.Type == token.CommentType {
			// The scalar starts on a later line: the skip below steps
			// over the comment's line break.
			start = lineEnd(text, start)
		} else {
			start += len(prev.Value)
			col += utf8.RuneCountInString(prev.Value)
		}
	}
	if start > len(text) {
		return scalarFix{}, tree.Pos{}, msgPlainStart
	}
	for start < len(text) && isWhite(text[start]) {
		line, col = advance(text, start, start+1, line, col)
		start++
	}
	raw := strings.TrimSpace(tk.Origin)
	if first, _, _ := strings.Cut(raw, "\n"); !strings.HasPrefix(text[start:], strings.TrimRight(first, " \t")) {
		return scalarFix{}, tree.Pos{}, msgPlainStart
	}
	// posAt returns the file position of byte offset off of the scalar.
	posAt := func(off int) tree.Pos {
		l, c := advance(text, start, off, line, col)
		return t.p.pos(l+t.lineOff, c)
	}
	// The scalar ends at the next token when that starts on its last line,
	// as a ':' after a key or a ',' in a flow collection does.
	n := strings.Count(raw, "\n")
	stop := -1
	if idx+1 < len(t.tks) {
		if next := t.tks[idx+1]; next.Position.Line == line+n {
			stop = t.src.offset(next.Position.Line, t.col(next))
		}
	}
	var b strings.Builder
	breaks, comment := 0, false
	off := start
	for ; n >= 0; n-- {
		end := lineEnd(text, off)
		if n == 0 && stop >= off && stop < end {
			end = stop
		}
		lead := off + len(text[off:end]) - len(strings.TrimLeft(text[off:end], " \t"))
		s := strings.Trim(text[off:end], " \t")
		h := commentAt(s)
		if h >= 0 {
			s = strings.TrimRight(s[:h], " \t")
		}
		switch {
		case comment && s != "":
			return scalarFix{}, posAt(lead), msgPlainComment
		case strings.Contains(s, ": ") || strings.Contains(s, ":\t") || strings.HasSuffix(s, ":"):
			c := strings.Index(s+" ", ": ")
			if tab := strings.Index(s, ":\t"); tab >= 0 && (c < 0 || tab < c) {
				c = tab
			}
			return scalarFix{}, posAt(lead + c), msgPlainColon
		}
		comment = comment || h >= 0
		switch {
		case s == "":
			breaks++
		case b.Len() == 0:
			b.WriteString(s)
		default:
			if breaks == 0 {
				b.WriteByte(' ')
			}
			for ; breaks > 0; breaks-- {
				b.WriteByte('\n')
			}
			b.WriteString(s)
		}
		off = min(end+1, len(text))
	}
	return scalarFix{text: b.String(), set: true, pos: posAt(start)}, tree.Pos{}, ""
}

// tabbedPlain reads the plain scalar token idx, on one line, from the
// source: from its first character, with the tabs goccy left out of its
// value (plainEnd). It reports false when the source at the token holds
// other text.
func (t *tokenPass) tabbedPlain(idx int) (string, bool) {
	tk := t.tks[idx]
	off := t.src.offset(tk.Position.Line, t.col(tk))
	if off < 0 {
		return "", false
	}
	end := plainEnd(t.src.text, off, tk.Value)
	if end < 0 {
		return "", false
	}
	return t.src.text[off:end], true
}

// commentAt returns the offset of the '#' that starts a comment in a line
// of a plain scalar, trimmed: at its start or after white space; or -1.
func commentAt(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return i
		}
	}
	return -1
}
