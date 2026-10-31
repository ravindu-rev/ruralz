// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/diff"
)

// ANSI color sequences of human output.
const (
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiReset  = "\x1b[0m"
)

// colors reports whether human output written to w is colored
// (docs/reference/01-cli-and-api-surface.md "Output formats and exit
// codes"): NO_COLOR is unset or empty, TERM is not dumb, and w is a
// terminal that takes ANSI color. A file or a pipe never is.
func (inv *invocation) colors(w io.Writer) bool {
	if v, ok := inv.lookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}
	if v, _ := inv.lookupEnv("TERM"); v == "dumb" {
		return false
	}
	return inv.opts.isTerminalFunc()(w)
}

// diagnosticLines returns the text line of each diagnostic of ds after
// prefix, its severity colored with color: error red, warning yellow.
func diagnosticLines(ds diag.List, prefix string, color bool) []byte {
	var dst []byte
	for _, d := range ds {
		dst = append(dst, prefix...)
		line := d.AppendText(nil)
		if color {
			line = colorSeverity(line, d)
		}
		dst = append(dst, line...)
		dst = append(dst, '\n')
	}
	return dst
}

// colorSeverity returns line, the text form of d, with its severity word
// wrapped in its color. The word follows the location and one space; a
// line of another shape is returned as it is.
func colorSeverity(line []byte, d diag.Diagnostic) []byte {
	sev := d.Severity.String()
	at := len(locationText(d.Location)) + 1
	if at+len(sev) > len(line) || line[at-1] != ' ' || string(line[at:at+len(sev)]) != sev {
		return line
	}
	c := ansiRed
	if d.Severity == diag.SeverityWarning {
		c = ansiYellow
	}
	out := make([]byte, 0, len(line)+len(c)+len(ansiReset))
	out = append(out, line[:at]...)
	out = append(out, c...)
	out = append(out, sev...)
	out = append(out, ansiReset...)
	return append(out, line[at+len(sev):]...)
}

// locationText is the location of a diagnostic's text form: the file, then
// ":line" when known and ":column" when known too; "-" without a file.
func locationText(l diag.Location) string {
	if l.File == "" {
		return "-"
	}
	s := l.File
	if l.Line > 0 {
		s += ":" + strconv.Itoa(l.Line)
		if l.Column > 0 {
			s += ":" + strconv.Itoa(l.Column)
		}
	}
	return s
}

// stderrDiagnostics prints ds as text lines on standard error, each after
// prefix, colored when standard error is a terminal that takes color:
// render, build and diff print their diagnostics this way in every mode.
func (inv *invocation) stderrDiagnostics(ds diag.List, prefix string) {
	if len(ds) == 0 {
		return
	}
	_, _ = inv.sio.Stderr.Write(diagnosticLines(ds, prefix, inv.colors(inv.sio.Stderr)))
}

// diagnosticsJSON returns the JSON array of ds in the configuration
// model's form, [] when empty, indented by two spaces and ending with a
// newline (diag.WriteJSON).
func diagnosticsJSON(ds diag.List) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := diag.WriteJSON(&buf, ds); err != nil {
		return nil, fmt.Errorf("encode diagnostics: %w", err)
	}
	return buf.Bytes(), nil
}

// sideDiagnostics are the diagnostics of one side of a diff.
type sideDiagnostics struct {
	side string
	ds   diag.List
}

// sideDiagnosticsJSON returns one JSON array of the diagnostics of every
// side in order, each in the configuration model's form with a last member
// side naming its side, "from" or "to".
func sideDiagnosticsJSON(sides ...sideDiagnostics) ([]json.RawMessage, error) {
	items := []json.RawMessage{}
	for _, s := range sides {
		arr, err := diagnosticsJSON(s.ds)
		if err != nil {
			return nil, err
		}
		var objs []json.RawMessage
		if err := json.Unmarshal(arr, &objs); err != nil {
			return nil, fmt.Errorf("decode diagnostics: %w", err)
		}
		member, err := json.Marshal(s.side)
		if err != nil {
			return nil, fmt.Errorf("encode diagnostics: %w", err)
		}
		for _, o := range objs {
			var c bytes.Buffer
			if err := json.Compact(&c, o); err != nil {
				return nil, fmt.Errorf("encode diagnostics: %w", err)
			}
			obj := bytes.TrimSuffix(c.Bytes(), []byte("}"))
			obj = append(obj, `,"side":`...)
			obj = append(obj, member...)
			items = append(items, append(obj, '}'))
		}
	}
	return items, nil
}

// writeStdout writes data to standard output; a failure exits 2.
func (inv *invocation) writeStdout(data []byte) error {
	if _, err := inv.sio.Stdout.Write(data); err != nil {
		return command.NoResultf("write standard output: %w", err)
	}
	return nil
}

// writeJSON writes v as one JSON document to standard output
// (command.WriteJSON); a failure exits 2.
func (inv *invocation) writeJSON(v any) error {
	var buf bytes.Buffer
	if err := command.WriteJSON(&buf, v); err != nil {
		return command.NoResult(err)
	}
	return inv.writeStdout(buf.Bytes())
}

// colorDiff returns text, the human form of d (diff.Diff.AppendText), with
// each resource line, field operation line and effective change line in
// the color of its change: + green, - red, ~ and > yellow. The header, the
// section line and the summary stay plain. The human form writes one line
// per entry of d, whatever the authored text, so the lines follow d's
// entries; text of another shape is returned plain.
func colorDiff(text []byte, d *diff.Diff) []byte {
	want := []string{"", ""} // the header and the blank line after it
	if len(d.Resources) > 0 {
		for _, r := range d.Resources {
			want = append(want, changeColor(r.Change))
			for _, op := range r.Ops {
				want = append(want, opColor(op.Kind))
			}
		}
		want = append(want, "")
	}
	if len(d.Effective) > 0 {
		want = append(want, "")
		for _, c := range d.Effective {
			want = append(want, opColor(c.Op))
		}
		want = append(want, "")
	}
	want = append(want, "") // the summary
	lines := bytes.SplitAfter(text, []byte("\n"))
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	if len(lines) != len(want) {
		return text
	}
	out := make([]byte, 0, len(text)+len(lines)*(len(ansiYellow)+len(ansiReset)))
	for i, line := range lines {
		if want[i] == "" {
			out = append(out, line...)
			continue
		}
		body := bytes.TrimSuffix(line, []byte("\n"))
		out = append(out, want[i]...)
		out = append(out, body...)
		out = append(out, ansiReset...)
		out = append(out, line[len(body):]...)
	}
	return out
}

// changeColor is the color of a resource line: added green, removed red,
// modified yellow.
func changeColor(c diff.Change) string {
	switch c {
	case diff.Added:
		return ansiGreen
	case diff.Removed:
		return ansiRed
	case diff.Modified:
		return ansiYellow
	default:
		return ""
	}
}

// opColor is the color of an operation line: add green, remove red,
// replace and move yellow.
func opColor(k diff.OpKind) string {
	switch k {
	case diff.OpAdd:
		return ansiGreen
	case diff.OpRemove:
		return ansiRed
	case diff.OpReplace, diff.OpMove:
		return ansiYellow
	default:
		return ""
	}
}
