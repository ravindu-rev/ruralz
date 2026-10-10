// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"

	"github.com/goccy/go-yaml/scanner"
	"github.com/goccy/go-yaml/token"
)

// longBlankRun is the run of blank lines from which a document takes the
// keep-chomping rewrite. goccy clips a block scalar's trailing line breaks
// one at a time, copying the scalar each time, so a scalar of L characters
// followed by m blank lines costs O(m·L): a megabyte of blank lines after a
// literal block takes hours. Runs shorter than this keep that cost linear.
const longBlankRun = 8

// blankLine reports a blank line: spaces only, or empty. A line holding a
// tab is not blank: inside a block scalar goccy reads a tab as content or
// refuses it, never as indentation of an empty line.
func blankLine(l string) bool {
	return strings.TrimLeft(l, " \n") == ""
}

// blankRuns reports whether text holds a run of at least n consecutive
// blank lines.
func blankRuns(text string, n int) bool {
	run := 0
	for line := range strings.Lines(text) {
		if blankLine(line) {
			if run++; run >= n {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// keepChomping rewrites the clipping block scalar headers that a long run
// of blank lines ends, so goccy scans the document in linear time, and
// returns the rewritten text and the number of headers rewritten. The
// values of block scalars are read from the source (literal.go), so the
// rewrite changes no value; it only spares goccy its own clipping. A
// scalar with content takes keep chomping ("|" becomes "|+", ">2" becomes
// ">+2"): goccy keeps its value whole. A scalar of blank lines only takes
// strip chomping: keep chomping would turn it into a line break, which
// goccy's scanner reads as a pending value. The headers and their scalars'
// content are found by a first scan of the text with every blank run cut
// (cutBlankRuns), which keeps that scan linear and leaves every header
// where it was. The rewrite only inserts a character after a header's
// indicator, which it finds in the text (headerIndex), so every line and
// every position before it on its line is unchanged and goccy reports the
// rewritten header at the position it reported the original at.
func keepChomping(text string) (string, int) {
	short, lineOf := cutBlankRuns(text, 2)
	// rewrite maps a header's line to its goccy column and new chomping
	// indicator; a line holds at most one header.
	rewrite := map[int]headerRewrite{}
	func() {
		// A panic in goccy ends the search; the full scan reports it.
		defer func() { _ = recover() }()
		var s scanner.Scanner
		s.Init(short)
		var all token.Tokens
		for {
			tks, err := s.Scan()
			all = append(all, tks...)
			if err != nil || len(tks) == 0 {
				break
			}
		}
		blank := blankBefore(text)
		for i, tk := range all {
			if (tk.Type != token.LiteralType && tk.Type != token.FoldedType) || strings.ContainsAny(tk.Value, "+-") {
				continue
			}
			// The content token, then the token that ends the scalar.
			// Comments are skipped only before the content: the comment
			// on the header's line. After the content, the next token of
			// any type ends the scalar, a comment included, since YAML
			// ends a block scalar at a less indented comment line and
			// goccy clips every blank line before it.
			mode, content, end := byte('-'), -1, len(blank)
			for j := i + 1; j < len(all); j++ {
				if content < 0 {
					if all[j].Type == token.CommentType {
						continue
					}
					content = j
					if all[j].Type == token.StringType && all[j].Value != "" {
						mode = '+'
					}
					continue
				}
				if l := all[j].Position.Line; l >= 1 && l <= len(lineOf) {
					end = lineOf[l-1]
				}
				break
			}
			// Only a scalar that a long blank run ends is rewritten.
			if tk.Position.Line < 1 || tk.Position.Line > len(lineOf) {
				continue
			}
			line := lineOf[tk.Position.Line-1]
			if _, dup := rewrite[line]; !dup && end >= 1 && end <= len(blank) && blank[end-1] >= longBlankRun {
				rewrite[line] = headerRewrite{col: tk.Position.Column, mode: mode}
			}
		}
	}()
	if len(rewrite) == 0 {
		return text, 0
	}
	n := 0
	var b strings.Builder
	b.Grow(len(text) + len(rewrite))
	line := 1
	for l := range strings.Lines(text) {
		if r, ok := rewrite[line]; ok {
			if at := headerIndex(l, r.col); at >= 0 {
				b.WriteString(l[:at+1])
				b.WriteByte(r.mode)
				l = l[at+1:]
				n++
			}
		}
		b.WriteString(l)
		line++
	}
	return b.String(), n
}

// headerRewrite is the rewrite of one block scalar header: goccy's column
// of the header token and the chomping indicator to insert.
type headerRewrite struct {
	col  int
	mode byte
}

// headerIndex returns the byte index in line of the block scalar indicator
// ('|' or '>') of a header goccy reports at code-point column col, or -1.
// goccy reports the header of a tagged scalar ("!!str |") at the blank
// before it, so the indicator is the first character at or after col that
// is not a space or a tab.
func headerIndex(line string, col int) int {
	c := 0
	for i, r := range line {
		if c++; c < col {
			continue
		}
		switch r {
		case ' ', '\t':
			continue
		case '|', '>':
			return i
		default:
			return -1
		}
	}
	return -1
}

// blankBefore returns, for every line of text and for the position after
// its last line, the number of blank lines just before it.
func blankBefore(text string) []int {
	out := []int{0}
	run := 0
	for l := range strings.Lines(text) {
		if blankLine(l) {
			run++
		} else {
			run = 0
		}
		out = append(out, run)
	}
	return out
}

// cutBlankRuns returns text with every run of more than keep blank lines
// cut, and the original line number of each line of the result. A cut run
// keeps its first keep lines, its longest line and its last line, in
// order, so the scan of the result sees what goccy's block scalars read
// from the whole run: whether any of its lines is long enough to be
// content (a line of spaces past a scalar's indentation adds spaces to its
// value), and the last line before the content that follows, whose
// indentation goccy checks. At most keep+2 lines of a run remain, which
// keeps goccy's clipping of the result linear.
func cutBlankRuns(text string, keep int) (string, []int) {
	var b strings.Builder
	b.Grow(len(text))
	lineOf := make([]int, 0, strings.Count(text, "\n")+1)
	emit := func(l string, n int) {
		b.WriteString(l)
		lineOf = append(lineOf, n)
	}
	width := func(l string) int { return len(strings.TrimSuffix(l, "\n")) }
	var (
		run, keptWidth int
		long, last     string
		longAt, lastAt int
	)
	flush := func() {
		if run > keep {
			if longAt != lastAt && width(long) > keptWidth {
				emit(long, longAt)
			}
			emit(last, lastAt)
		}
		run, keptWidth, longAt = 0, 0, 0
	}
	line := 0
	for l := range strings.Lines(text) {
		line++
		if !blankLine(l) {
			flush()
			emit(l, line)
			continue
		}
		if run++; run <= keep {
			emit(l, line)
			keptWidth = max(keptWidth, width(l))
			continue
		}
		if longAt == 0 || width(l) > width(long) {
			long, longAt = l, line
		}
		last, lastAt = l, line
	}
	flush()
	if strings.HasSuffix(text, "\n") || text == "" {
		// The empty line after a final line break.
		lineOf = append(lineOf, line+1)
	}
	return b.String(), lineOf
}

// stripEmptyKeep rewrites to strip chomping the keep-chomping block scalar
// headers of text whose content is blank lines only, and returns the text
// and the number of headers rewritten (01 req 8, 12). goccy's scanner
// reads the line break such a scalar keeps as a value still pending and
// refuses the next token ("keep: |+\n\n# c" is "could not find multi-line
// content"), which YAML 1.2 accepts as {keep: "\n"}, so the scalar was
// accepted at the end of a document and refused before a comment or an
// entry. The token pass reads the value from the source (literal.go), and
// the rewrite replaces one character of the header, so no position
// changes; keepChomping gives such scalars strip chomping for the same
// reason.
//
// goccy's scan of text stops at the first such scalar, so the headers are
// found in a scan of text with every keep-chomping header written with
// strip chomping, which reads every one of them, and with its long blank
// runs cut (cutBlankRuns), which keeps that scan linear and removes no
// content line. A '+' that is no header there, in a plain, quoted or
// comment text, keeps the structure of the scan and is never rewritten.
func stripEmptyKeep(text string) (string, int) {
	helper := []byte(text)
	found := false
	for i := 1; i < len(text); i++ {
		if text[i] != '+' {
			continue
		}
		j := i - 1
		if j > 0 && text[j] >= '1' && text[j] <= '9' {
			j--
		}
		if text[j] == '|' || text[j] == '>' {
			helper[i] = '-'
			found = true
		}
	}
	if !found {
		return text, 0
	}
	short, lineOf := cutBlankRuns(string(helper), 2)
	var tks token.Tokens
	func() {
		// A panic in goccy ends the search; the full scan reports it.
		defer func() { _ = recover() }()
		var sc scanner.Scanner
		sc.Init(short)
		for {
			sub, err := sc.Scan()
			tks = append(tks, sub...)
			if err != nil || len(sub) == 0 {
				return
			}
		}
	}()
	var b []byte
	n := 0
	cur := newCursor(text)
	for i, tk := range tks {
		if (tk.Type != token.LiteralType && tk.Type != token.FoldedType) || tk.Position == nil ||
			tk.Position.Line < 1 || tk.Position.Line > len(lineOf) || !blankContent(tks, i) {
			continue
		}
		start := cur.offset(lineOf[tk.Position.Line-1], 1)
		if start < 0 {
			continue
		}
		line := text[start:lineEnd(text, start)]
		at := headerIndex(line, tk.Position.Column)
		if at < 0 {
			continue
		}
		// The chomping indicator, after the indentation indicator if any.
		k := at + 1
		if k < len(line) && line[k] >= '1' && line[k] <= '9' {
			k++
		}
		if k < len(line) && line[k] == '+' {
			if b == nil {
				b = []byte(text)
			}
			b[start+k] = '-'
			n++
		}
	}
	if n == 0 {
		return text, 0
	}
	return string(b), n
}

// blankContent reports whether the block scalar whose header is token i
// holds no line of content: no string token follows its header, comments
// on the header's line aside, on a later line, or that token holds only
// white space.
func blankContent(tks token.Tokens, i int) bool {
	j := i + 1
	for j < len(tks) && tks[j].Type == token.CommentType && tks[j].Position.Line == tks[i].Position.Line {
		j++
	}
	return j == len(tks) || tks[j].Type != token.StringType || strings.Trim(tks[j].Value, " \n") == ""
}
