// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Paths of the catalog gates (09 req 74, 75).
const (
	obsDoc  = "docs/architecture/10-observability.md"
	archDir = "docs/architecture/"
)

// Headings of the OBS sections whose tables the code gate compares with
// the catalog (09 req 74).
const (
	gatewayMetricsHeading = "Ruralz Gateway metrics"
	degradedHeading       = "Degraded states"
)

// mdRow is one data row of a Markdown table.
type mdRow struct {
	line  int
	cells []string
}

// mdTable is one Markdown table: the heading of its section, its header
// cells and data rows.
type mdTable struct {
	heading string
	header  []string
	rows    []mdRow
}

// column returns the index of the header cell named name, or -1.
func (t mdTable) column(name string) int { return slices.Index(t.header, name) }

// splitRow splits a table line into trimmed cells at unescaped pipes.
func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	if strings.HasSuffix(line, "|") && !strings.HasSuffix(line, `\|`) {
		line = line[:len(line)-1]
	}
	var cells []string
	var cell strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cell.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
		default:
			cell.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

// isSeparator reports whether cells form a header separator row (---).
func isSeparator(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, ":- ") != "" || !strings.Contains(c, "-") {
			return false
		}
	}
	return len(cells) > 0
}

// parseTables returns the Markdown tables of a document, outside fenced
// code blocks, each with the text of the ATX heading it follows.
func parseTables(lines []string) []mdTable {
	var out []mdTable
	var cur *mdTable
	fenced := false
	heading := ""
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(trimmed, "#") {
			if text := strings.TrimLeft(trimmed, "#"); text == "" || text[0] == ' ' {
				heading = strings.TrimSpace(text)
			}
		}
		if fenced || !strings.HasPrefix(trimmed, "|") {
			cur = nil
			continue
		}
		cells := splitRow(trimmed)
		switch {
		case cur == nil:
			out = append(out, mdTable{heading: heading, header: cells})
			cur = &out[len(out)-1]
		case isSeparator(cells):
		default:
			cur.rows = append(cur.rows, mdRow{line: i + 1, cells: cells})
		}
	}
	return out
}

// obsMetric is a metric name of an OBS metrics table and the row holding it.
type obsMetric struct {
	line              int
	heading           string   // the table's section
	index             int      // position of the name among the row's names
	names             []string // every name of the row
	typ, unit, labels string   // the Type, Unit and Labels cells
	histogram         bool
}

// obsReason is a degraded reason of the OBS "Degraded states" table.
type obsReason struct {
	line int
	row  string // the whole row, for milestone tags
}

// obsCatalog is the Observability document's catalog: every metrics table
// (a table whose first column is "Metric"), the gateway metrics table among
// them, and the degraded reasons table (first column "`reason`" under
// "Degraded states").
type obsCatalog struct {
	metrics map[string]obsMetric
	reasons map[string]obsReason
}

// backticked returns the code spans of s matching pattern (one group).
func backticked(s, pattern string) []string {
	re := regexp.MustCompile("`(" + pattern + ")`")
	var out []string
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// parseObsCatalog reads the metrics and degraded reasons tables of OBS.
func parseObsCatalog(lines []string) (obsCatalog, []finding) {
	c := obsCatalog{metrics: map[string]obsMetric{}, reasons: map[string]obsReason{}}
	var out []finding
	gatewayTables, reasonTables := 0, 0
	for _, t := range parseTables(lines) {
		if len(t.header) == 0 {
			continue
		}
		switch t.header[0] {
		case "Metric":
			if t.heading == gatewayMetricsHeading {
				gatewayTables++
			}
			typ, unit, labels := t.column("Type"), t.column("Unit"), t.column("Labels")
			if typ < 0 || unit < 0 || labels < 0 {
				out = append(out, finding{path: obsDoc, line: max(1, len(t.rows)), msg: "a metrics table lacks a Type, Unit or Labels column"})
				continue
			}
			for _, r := range t.rows {
				if len(r.cells) != len(t.header) {
					out = append(out, finding{path: obsDoc, line: r.line, msg: fmt.Sprintf("row has %d cells, the header %d", len(r.cells), len(t.header))})
					continue
				}
				names := backticked(r.cells[0], `ruralz_[a-z0-9_]+`)
				for i, n := range names {
					if prev, dup := c.metrics[n]; dup {
						out = append(out, finding{path: obsDoc, line: r.line, msg: fmt.Sprintf("%s is listed twice (first on line %d)", n, prev.line)})
					}
					c.metrics[n] = obsMetric{
						line: r.line, heading: t.heading, index: i, names: names,
						typ: r.cells[typ], unit: r.cells[unit], labels: r.cells[labels],
						histogram: strings.Contains(strings.ToLower(r.cells[typ]), "histogram"),
					}
				}
			}
		case "`reason`":
			if t.heading != degradedHeading {
				continue
			}
			reasonTables++
			for _, r := range t.rows {
				for _, n := range backticked(r.cells[0], `[a-z0-9_]+`) {
					c.reasons[n] = obsReason{line: r.line, row: strings.Join(r.cells, " | ")}
				}
			}
		}
	}
	if gatewayTables == 0 {
		out = append(out, finding{path: obsDoc, msg: fmt.Sprintf("no metrics table (first column \"Metric\") under the heading %q found", gatewayMetricsHeading)})
	}
	if reasonTables == 0 {
		out = append(out, finding{path: obsDoc, msg: fmt.Sprintf("no degraded states table (first column \"`reason`\") under the heading %q found", degradedHeading)})
	}
	return c, out
}

// pick returns the part of a cell that describes names[index] in a row
// listing several names. A cell without any of seps describes every name.
// Split at the first of seps it holds, it gives either one part per name
// ("operations, writes") or a value for every name followed by exceptions
// of the form "<value> for <word>" for the names containing word ("Gauge;
// Counter for cycles"). Any other split is ambiguous: ok is false.
func pick(cell string, names []string, index int, seps ...string) (value string, ok bool) {
	if len(names) < 2 {
		return strings.TrimSpace(cell), true
	}
	for _, sep := range seps {
		parts := strings.Split(cell, sep)
		switch {
		case len(parts) == 1:
			continue
		case len(parts) == len(names):
			return strings.TrimSpace(parts[index]), true
		}
		value = strings.TrimSpace(parts[0])
		for _, p := range parts[1:] {
			v, word, found := strings.Cut(strings.TrimSpace(p), " for ")
			if !found || word == "" || !slices.ContainsFunc(names, func(n string) bool { return strings.Contains(n, word) }) {
				return "", false
			}
			if strings.Contains(names[index], word) {
				value = strings.TrimSpace(v)
			}
		}
		return value, true
	}
	return strings.TrimSpace(cell), true
}

// kindWord and boundsWord name catalog types as OBS writes them.
func kindWord(k catalog.Kind) string {
	switch k {
	case catalog.Counter:
		return "counter"
	case catalog.Gauge:
		return "gauge"
	case catalog.Histogram:
		return "histogram"
	default:
		return "unknown"
	}
}

func boundsWord(b catalog.Bounds) string {
	switch b {
	case catalog.BoundsFast:
		return "fast"
	case catalog.BoundsRequest:
		return "request"
	case catalog.BoundsControl:
		return "control"
	case catalog.BoundsBytes:
		return "bytes"
	case catalog.BoundsRatio:
		return "ratio"
	default:
		return "none"
	}
}

// docUnit is the Unit column spelling of a UCUM unit: s is seconds, By
// bytes, 1 info (ratio for _ratio families), {label_sets} "label sets".
func docUnit(f catalog.Family) string {
	switch f.Unit {
	case "s":
		return "seconds"
	case "By":
		return "bytes"
	case "1":
		if strings.HasSuffix(f.Name, "_ratio") {
			return "ratio"
		}
		return "info"
	default:
		return strings.ReplaceAll(strings.Trim(f.Unit, "{}"), "_", " ")
	}
}

// laterMilestone matches an availability tag after M1.
func laterMilestone() *regexp.Regexp { return regexp.MustCompile(`Planned \(M[2-5]\)`) }

// checkObservabilityDoc runs the catalog gates over the documents:
// the code gate (09 req 74) compares every catalog family and degraded
// reason with its OBS row, and the doc gate (09 req 75) requires every
// ruralz_* name and ruralz_node_degraded_info reason under
// docs/architecture/ to be in the OBS catalog.
func checkObservabilityDoc(r *repo, cv catalogView) ([]finding, error) {
	if !slices.Contains(r.files, obsDoc) {
		return []finding{{path: obsDoc, msg: "missing: the telemetry catalog gates read its metrics and degraded states tables"}}, nil
	}
	data, err := r.read(obsDoc)
	if err != nil {
		return nil, err
	}
	obs, out := parseObsCatalog(strings.Split(string(data), "\n"))
	out = append(out, codeGate(obs, cv)...)
	for _, rel := range r.files {
		if !strings.HasPrefix(rel, archDir) || !strings.HasSuffix(rel, ".md") {
			continue
		}
		data, err := r.read(rel)
		if err != nil {
			return nil, err
		}
		out = append(out, docGate(rel, strings.Split(string(data), "\n"), obs)...)
	}
	return out, nil
}

// codeGate is 09 req 74: every catalog family appears in the OBS "Ruralz
// Gateway metrics" table with the same type, histogram set, unit and label
// names, and every catalog reason in the "Degraded states" table, not
// tagged for a later milestone.
func codeGate(obs obsCatalog, cv catalogView) []finding {
	var out []finding
	at := func(line int, format string, args ...any) {
		out = append(out, finding{path: obsDoc, line: line, msg: fmt.Sprintf(format, args...)})
	}
	ambiguous := map[string]bool{} // "<line> <column>" reported once per row
	cell := func(m obsMetric, column, value string, seps ...string) (string, bool) {
		v, ok := pick(value, m.names, m.index, seps...)
		if key := fmt.Sprint(m.line, column); !ok && !ambiguous[key] {
			ambiguous[key] = true
			at(m.line, "the %s cell %q gives neither one value per name nor a value with \"<value> for <word>\" exceptions for %s",
				column, value, strings.Join(m.names, ", "))
		}
		return strings.ToLower(v), ok
	}
	families := map[string]catalog.Family{}
	for _, f := range cv.families {
		families[f.Name] = f
	}
	for _, f := range cv.families {
		m, ok := obs.metrics[f.Name]
		switch {
		case !ok:
			at(0, "catalog family %s is missing from the metrics catalog tables", f.Name)
			continue
		case m.heading != gatewayMetricsHeading:
			at(m.line, "catalog family %s is listed under %q, not in the %q table", f.Name, m.heading, gatewayMetricsHeading)
			continue
		}
		typ, typOK := cell(m, "Type", m.typ, ";")
		switch {
		case !typOK: // reported once for the row
		case !strings.Contains(typ, kindWord(f.Kind)):
			at(m.line, "%s is a %s in the catalog, %q here", f.Name, kindWord(f.Kind), typ)
		case f.Kind == catalog.Histogram && !strings.Contains(typ, boundsWord(f.Bounds)):
			at(m.line, "%s uses the %s bounds in the catalog, %q here", f.Name, boundsWord(f.Bounds), typ)
		}
		if unit, ok := cell(m, "Unit", m.unit, ";", ","); ok && unit != docUnit(f) {
			at(m.line, "%s has unit %s (%s) in the catalog, %q here", f.Name, f.Unit, docUnit(f), unit)
		}
		docLabels := backticked(m.labels, `[a-z][a-z0-9_]*`)
		for _, l := range f.Labels {
			if !slices.Contains(docLabels, l.Name) {
				at(m.line, "%s has label %s in the catalog, missing here", f.Name, l.Name)
			}
		}
		// Labels the row lists must belong to one of its names, when every
		// name of the row is a catalog family.
		if m.index != 0 || slices.ContainsFunc(m.names, func(n string) bool { _, ok := families[n]; return !ok }) {
			continue
		}
		for _, dl := range docLabels {
			if !slices.ContainsFunc(m.names, func(n string) bool {
				return slices.ContainsFunc(families[n].Labels, func(l catalog.Label) bool { return l.Name == dl })
			}) {
				at(m.line, "label %s of %s is not in the catalog", dl, strings.Join(m.names, ", "))
			}
		}
	}
	for _, r := range cv.reasons {
		o, ok := obs.reasons[r]
		switch {
		case !ok:
			at(0, "degraded reason %s is missing from the degraded states table", r)
		case laterMilestone().MatchString(o.row):
			at(o.line, "degraded reason %s is registered in M1 but tagged %s here", r, laterMilestone().FindString(o.row))
		}
	}
	return out
}

// docGate is 09 req 75 for one document under docs/architecture/.
func docGate(rel string, lines []string, obs obsCatalog) []finding {
	token := regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(ruralz_[a-z0-9_]*)`)
	selector := regexp.MustCompile(`ruralz_node_degraded_info\{([^}]*)\}`)
	matcher := regexp.MustCompile(`reason\s*(?:=~|!~|!=|=)\s*"([^"]*)"`)
	phrase := regexp.MustCompile("(?i)degraded (?:reasons?|states?)\\s+((?:`[a-z0-9_]+`(?:\\s*,\\s*(?:and\\s+|or\\s+)?|\\s+(?:and|or)\\s+)?)+)")
	infoReason := regexp.MustCompile("degraded_info`?\\s*\\(reason\\s+`([a-z0-9_]+)`")
	plainValues := regexp.MustCompile(`^[a-z0-9_]+(\|[a-z0-9_]+)*$`)
	var out []finding
	for i, line := range lines {
		for _, m := range token.FindAllStringSubmatchIndex(line, -1) {
			name, end := line[m[2]:m[3]], m[3]
			if end+1 < len(line) && line[end] == '.' && line[end+1] >= 'a' && line[end+1] <= 'z' {
				continue // a file name such as ruralz_v1.lua
			}
			if !obs.knowsMetric(name) {
				out = append(out, finding{
					path: rel, line: i + 1,
					msg: fmt.Sprintf("%s is not in the metrics catalog of %s", name, obsDoc),
				})
			}
		}
		var reasons []string
		for _, sel := range selector.FindAllStringSubmatch(line, -1) {
			for _, m := range matcher.FindAllStringSubmatch(sel[1], -1) {
				// A plain alternation of values; other regular expressions
				// are not checked.
				if v := strings.ReplaceAll(m[1], `\|`, "|"); plainValues.MatchString(v) {
					reasons = append(reasons, strings.Split(v, "|")...)
				}
			}
		}
		for _, m := range phrase.FindAllStringSubmatch(line, -1) {
			reasons = append(reasons, backticked(m[1], `[a-z0-9_]+`)...)
		}
		for _, m := range infoReason.FindAllStringSubmatch(line, -1) {
			reasons = append(reasons, m[1])
		}
		for _, r := range reasons {
			if _, ok := obs.reasons[r]; !ok {
				out = append(out, finding{
					path: rel, line: i + 1,
					msg: fmt.Sprintf("degraded reason %s is not in the degraded states table of %s", r, obsDoc),
				})
			}
		}
	}
	return out
}

// knowsMetric reports whether a ruralz_* token of a document is in the OBS
// catalog: a listed name; a histogram's _bucket, _sum or _count series; or a
// component prefix (ruralz_ai, ruralz_plugin_) of listed names.
func (c obsCatalog) knowsMetric(name string) bool {
	if _, ok := c.metrics[name]; ok {
		return true
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, suffix); ok && c.metrics[base].histogram {
			return true
		}
	}
	if strings.HasSuffix(name, "_") || strings.Count(name, "_") == 1 {
		prefix := strings.TrimSuffix(name, "_") + "_"
		for n := range c.metrics {
			if strings.HasPrefix(n, prefix) {
				return true
			}
		}
	}
	return false
}
