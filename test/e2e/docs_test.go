// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
)

// The suite reads these shapes of the documents: the five-cell effective
// tables of "### Worked example" and "## REST example Bundle" in the
// configuration model, the command tables of "## 9. CLI command registry"
// in the foundation pack and of "### Command table" in the CLI reference,
// each row's last cell its Planned milestone, and the roadmap's first
// "## M<n>" heading, the milestone in progress.

// Documents the suite reads, relative to the repository root.
const (
	configModelDoc = "docs/architecture/02-configuration-model.md"
	packDoc        = "docs/_meta/foundation-pack.md"
	cliDoc         = "docs/reference/01-cli-and-api-surface.md"
	roadmapDoc     = "docs/roadmap/01-roadmap-and-milestones.md"
)

// docText returns the text of the document at rel.
func docText(t testing.TB, rel string) string {
	t.Helper()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return string(readFile(t, filepath.Join(root, filepath.FromSlash(rel))))
}

// docSection returns the text below the heading line heading of the
// document at rel, up to the next heading of its level or a higher one;
// lines inside fenced code blocks are never headings.
func docSection(t testing.TB, rel, heading string) string {
	t.Helper()
	level := strings.Index(heading, " ")
	all := strings.Split(docText(t, rel), "\n")
	for i, l := range all {
		if l != heading {
			continue
		}
		var out []string
		fence := false
		for _, l := range all[i+1:] {
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

// chainRow is one row of an effective Filter Chain: Phase, leg, Policy
// and where it attaches.
type chainRow struct {
	Phase, Leg, Policy, From string
}

// docChain returns the rows of the effective-chain table, a "| Phase |
// Leg | Policy | From | Reason |" table, of a section of the
// configuration model, skipping the Policies in skip.
func docChain(t testing.TB, heading string, skip ...string) []chainRow {
	t.Helper()
	var rows []chainRow
	for _, line := range strings.Split(docSection(t, configModelDoc, heading), "\n") {
		c := tableCells(line)
		if len(c) != 5 || c[0] == "Phase" || strings.HasPrefix(c[0], "---") {
			continue
		}
		skipped := false
		for _, s := range skip {
			skipped = skipped || c[2] == s
		}
		if !skipped {
			rows = append(rows, chainRow{Phase: c[0], Leg: c[1], Policy: c[2], From: c[3]})
		}
	}
	if len(rows) == 0 {
		t.Fatalf("%q in %s has no effective-chain table", heading, configModelDoc)
	}
	return rows
}
