// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import "strings"

// segment is one document of a YAML stream as source text: its directive
// prefix, its "---" line when present, its content and its "..." line when
// present. Lines and code-point columns inside it equal those of the file
// once startLine-1 is added to the line.
type segment struct {
	text      string
	startLine int
}

// markerLine reports whether line is a document marker: "---" or "..." at
// column 1 followed by a space, a tab or the end of the line. goccy's
// scanner reads "---" by the same rule, but "..." at column 1 whatever
// follows it (scanDocumentEnd checks only the column and the three dots):
// the token pass refuses a "..." token on any other line (tokenPass.run).
func markerLine(line, marker string) bool {
	if !strings.HasPrefix(line, marker) {
		return false
	}
	rest := line[len(marker):]
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}

// blankOrComment reports whether a line holds only spaces and tabs,
// optionally followed by a comment.
func blankOrComment(line string) bool {
	t := strings.TrimLeft(line, " \t")
	return t == "" || t[0] == '#'
}

// splitDocuments cuts LF-normalized YAML text into documents at its
// document markers, so each document is tokenized and parsed on its own and
// peak token memory is one document's (01 req 9; risk 1). goccy's
// scanner.Scanner returns the whole remaining input from one Scan call, so
// feeding it one document at a time is what keeps the token pass
// incremental.
//
// A line that starts with "---" or "..." followed by white space or its
// end is a document marker wherever it appears: YAML 1.2 forbids such a
// line inside any scalar (production c-forbidden), so the cut never splits
// a value of a valid stream. goccy's scanner reads "---" markers by the
// same rule, and "..." at column 1 whatever follows it outside a quoted
// scalar: the token pass refuses such a "..." on a line that is no marker
// ("...x: 1", "...#"), and any token after a "..." but a comment, so every
// segment it passes holds one document and goccy's markers lie on its own
// marker lines (01 req 9). A "---" line starts a document; the directives,
// comments and blank lines that follow a "..." line (or the stream start)
// belong to the next document; a "..." line ends its document. Segments
// holding only blank and comment lines are dropped.
func splitDocuments(text string) []segment {
	var segs []segment
	segStart, segLine := 0, 1
	hasBody := false // the segment holds "---" or a content line
	hasDirective := false
	prefix := true // after the stream start or a "..." line
	closeAt := func(end, nextLine int) {
		if hasBody || hasDirective {
			segs = append(segs, segment{text: text[segStart:end], startLine: segLine})
		}
		segStart, segLine = end, nextLine
		hasBody, hasDirective = false, false
	}
	line := 1
	for start := 0; start < len(text); line++ {
		end := strings.IndexByte(text[start:], '\n')
		next := len(text)
		if end < 0 {
			end = len(text)
		} else {
			end += start
			next = end + 1
		}
		l := text[start:end]
		switch {
		case markerLine(l, "---"):
			if hasBody {
				closeAt(start, line)
			}
			hasBody, prefix = true, false
		case markerLine(l, "..."):
			hasBody = true
			closeAt(next, line+1)
			prefix = true
		case prefix && strings.HasPrefix(l, "%"):
			hasDirective = true
		case blankOrComment(l):
		default:
			hasBody, prefix = true, false
		}
		start = next
	}
	closeAt(len(text), line)
	return segs
}

// splitChars are the characters around which goccy may end a token inside
// a run of non-blank characters: the indicators of YAML 1.2 other than '-'
// and '?', which start a token only when a blank follows.
const splitChars = ",[]{}:\"'<#&*!|>%@`"

// estimateTokens returns an upper bound of the number of goccy tokens in a
// document, from its bytes alone (01 risk 1), so a flood of one-character
// tokens is refused before any of them is materialized. Every goccy token
// holds a non-blank character, apart from the content of an empty block
// scalar and an invalid token for a tab that indents a line, and goccy
// ends a token inside a run of non-blank characters only next to an
// indicator. So a run holds one token, plus one for each boundary next to
// a character of splitChars (two adjacent ones share one), plus one before
// a trailing '-' or '?' that follows one of them. Each '|' and '>' may add
// two content tokens of blanks, a "..." that starts a line ends a token
// without a blank, and a tab indenting a line may add one invalid token
// (it ends the scan). The bound is exact for flow collections and close
// for block mappings and sequences; quoted and multi-word scalars, block
// scalar content and indicators inside scalars are over-counted. When the
// bound passes limit, estimateTokens returns the byte offset of the
// character at which it did; otherwise -1.
func estimateTokens(text string, limit int) (n, overAt int) {
	runStart := -1
	lineStart, tabbed := true, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\n':
			lineStart = true
		case c == '\t' && lineStart && !tabbed:
			tabbed = true
			n++
		case c != ' ' && c != '\t':
			lineStart = false
		}
		if c == ' ' || c == '\t' || c == '\n' {
			runStart = -1
		} else {
			if runStart < 0 {
				runStart = i
				n++
				if c == '.' && strings.HasPrefix(text[i:], "...") && (i == 0 || text[i-1] == '\n') {
					n++
				}
			}
			first := i == runStart
			last := i+1 == len(text) || text[i+1] == ' ' || text[i+1] == '\t' || text[i+1] == '\n'
			// Two adjacent indicators share the boundary between them.
			afterSplit := !first && strings.IndexByte(splitChars, text[i-1]) >= 0
			switch {
			case strings.IndexByte(splitChars, c) >= 0:
				if !first && !afterSplit {
					n++
				}
				if !last {
					n++
				}
				if c == '|' || c == '>' {
					n += 2
				}
			case (c == '-' || c == '?') && last && afterSplit:
				n++
			}
		}
		if n > limit {
			return n, i
		}
	}
	return n, -1
}
