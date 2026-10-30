// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/profile"
)

// The suite reads these shapes of the documents (.claude/rules/docs.md
// lists them): in the configuration model, the captioned blocks of
// "## Complete annotated example Bundle" and "## REST example Bundle", the
// five-cell effective tables of those sections and of "### Worked
// example", the text and json blocks of "### Diagnostics and source map"
// and those of "### Diff semantics"; and the captioned blocks of
// "#### Quickstart" in Deployment topologies.

// docSection returns the text below the heading line heading of the
// document at rel (a repository path), such as "### Worked example", up to
// the next heading of its level or a higher one; lines inside fenced code
// blocks are never headings.
func docSection(t testing.TB, s *suite, rel, heading string) string {
	t.Helper()
	data, err := os.ReadFile(s.path(rel))
	if err != nil {
		t.Fatal(err)
	}
	level := strings.Index(heading, " ")
	lines := strings.Split(string(data), "\n")
	for i, l := range lines {
		if l != heading {
			continue
		}
		var out []string
		fence := false
		for _, l := range lines[i+1:] {
			if strings.HasPrefix(l, "```") {
				fence = !fence
			}
			if n := len(l) - len(strings.TrimLeft(l, "#")); !fence && n > 0 && n <= level && strings.HasPrefix(l[n:], " ") {
				break
			}
			out = append(out, l)
		}
		return strings.Join(out, "\n")
	}
	t.Fatalf("%s has no heading %q", rel, heading)
	return ""
}

// fenced is one fenced code block: its info string, its lines, each ending
// in a newline, and its caption, the last non-blank line before it.
type fenced struct{ caption, lang, body string }

// fencedBlocks returns the fenced code blocks of text in order.
func fencedBlocks(text string) []fenced {
	var out []fenced
	var cur *fenced
	var body strings.Builder
	caption := ""
	for _, l := range strings.Split(text, "\n") {
		switch {
		case cur == nil && strings.HasPrefix(l, "```"):
			cur = &fenced{caption: caption, lang: strings.TrimPrefix(l, "```")}
			body.Reset()
		case cur != nil && strings.HasPrefix(l, "```"):
			cur.body = body.String()
			out = append(out, *cur)
			cur = nil
		case cur != nil:
			body.WriteString(l + "\n")
		case strings.TrimSpace(l) != "":
			caption = l
		}
	}
	return out
}

// blocksOf returns the bodies of the blocks of a document section whose
// info string is lang.
func blocksOf(t testing.TB, s *suite, rel, heading, lang string) []string {
	t.Helper()
	var out []string
	for _, b := range fencedBlocks(docSection(t, s, rel, heading)) {
		if b.lang == lang {
			out = append(out, b.body)
		}
	}
	return out
}

// isExampleFile reports whether the documents show a file of this name in
// full: a configuration file discovery reads (profile.FormatOf) or a
// .ruralzignore.
func isExampleFile(name string) bool {
	base := path.Base(name)
	_, ok := profile.FormatOf(base)
	return ok || base == ".ruralzignore"
}

// captionFiles returns the files a caption names: the backticked file
// names before its first comma, such as ai/providers.yaml and
// ai/models.yaml in "`ai/providers.yaml` and `ai/models.yaml`:". A name
// after the comma, as in "`routes/x.draft.yaml`, which the `*.draft.yaml`
// pattern keeps out", belongs to the caption's prose.
func captionFiles(caption string) []string {
	head, _, _ := strings.Cut(caption, ",")
	var out []string
	parts := strings.Split(head, "`")
	for i := 1; i < len(parts); i += 2 {
		if isExampleFile(parts[i]) {
			out = append(out, parts[i])
		}
	}
	return out
}

// tableCells returns the cells of a Markdown table row with backticks
// removed; nil for any other line.
func tableCells(line string) []string {
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}
	cells := strings.Split(strings.Trim(line, "|"), "|")
	for i, c := range cells {
		cells[i] = strings.ReplaceAll(strings.TrimSpace(c), "`", "")
	}
	return cells
}

// chainTable returns the Phase, Leg, Policy and From columns of the
// effective-chain table of a document section, a "| Phase | Leg | Policy |
// From | Reason |" table, row by row.
func chainTable(t testing.TB, s *suite, rel, heading string) [][4]string {
	t.Helper()
	var rows [][4]string
	for _, line := range strings.Split(docSection(t, s, rel, heading), "\n") {
		c := tableCells(line)
		if len(c) != 5 || c[0] == "Phase" || strings.HasPrefix(c[0], "---") {
			continue
		}
		rows = append(rows, [4]string{c[0], c[1], c[2], c[3]})
	}
	if len(rows) == 0 {
		t.Fatalf("%q in %s has no effective-chain table", heading, rel)
	}
	return rows
}
