// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/diff"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
)

// TestColorSeverity: the severity word of a diagnostic line is wrapped in
// its color, error red and warning yellow, whatever the location's shape;
// a line of another shape stays as it is.
func TestColorSeverity(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    diag.Diagnostic
		want string
	}{
		{
			"full location",
			diag.Diagnostic{
				Code: "RZ-CFG-009", Severity: diag.SeverityError,
				Location: diag.Location{File: "a b/error.yaml", Line: 3, Column: 7}, Message: "error here",
			},
			"a b/error.yaml:3:7 " + ansiRed + "error" + ansiReset + " RZ-CFG-009: error here",
		},
		{
			"line only",
			diag.Diagnostic{
				Code: "RZ-CFG-013", Severity: diag.SeverityWarning,
				Location: diag.Location{File: "x.yaml", Line: 3}, Message: "m",
			},
			"x.yaml:3 " + ansiYellow + "warning" + ansiReset + " RZ-CFG-013: m",
		},
		{
			"no file",
			diag.Diagnostic{Code: "RZ-CFG-007", Severity: diag.SeverityError, Message: "m"},
			"- " + ansiRed + "error" + ansiReset + " RZ-CFG-007: m",
		},
	} {
		got := string(colorSeverity(tc.d.AppendText(nil), tc.d))
		if got != tc.want {
			t.Errorf("%s: colorSeverity = %q, want %q", tc.name, got, tc.want)
		}
	}
	d := diag.Diagnostic{Severity: diag.SeverityError, Location: diag.Location{File: "f"}}
	for _, line := range []string{"", "f", "f warning x", "other error x"} {
		if got := string(colorSeverity([]byte(line), d)); got != line {
			t.Errorf("colorSeverity(%q) = %q, want it unchanged", line, got)
		}
	}
}

// TestDiagnosticLines: one line per diagnostic after its prefix.
func TestDiagnosticLines(t *testing.T) {
	ds := diag.List{
		{Code: "RZ-CFG-009", Severity: diag.SeverityError, Location: diag.Location{File: "a.yaml", Line: 1, Column: 2}, Message: "one"},
		{Code: "RZ-CFG-013", Severity: diag.SeverityWarning, Location: diag.Location{File: "b.yaml"}, Message: "two"},
	}
	got := string(diagnosticLines(ds, "to: ", false))
	want := "to: a.yaml:1:2 error RZ-CFG-009: one\nto: b.yaml warning RZ-CFG-013: two\n"
	if got != want {
		t.Errorf("diagnosticLines = %q, want %q", got, want)
	}
	if got := diagnosticLines(nil, "", true); len(got) != 0 {
		t.Errorf("diagnosticLines(nil) = %q", got)
	}
}

// TestSideDiagnosticsJSON: the diagnostics of both sides form one array of
// the configuration model's objects, each ending in a member side.
func TestSideDiagnosticsJSON(t *testing.T) {
	d := diag.Diagnostic{Code: "RZ-CFG-027", Severity: diag.SeverityError, Message: "m <&>"}
	items, err := sideDiagnosticsJSON(sideDiagnostics{"from", diag.List{d}}, sideDiagnostics{"to", nil}, sideDiagnostics{"to", diag.List{d}})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := command.WriteJSON(&buf, items); err != nil {
		t.Fatal(err)
	}
	want := `[
  {
    "code": "RZ-CFG-027",
    "severity": "error",
    "message": "m <&>",
    "side": "from"
  },
  {
    "code": "RZ-CFG-027",
    "severity": "error",
    "message": "m <&>",
    "side": "to"
  }
]
`
	if buf.String() != want {
		t.Errorf("sideDiagnosticsJSON =\n%s\nwant\n%s", buf.String(), want)
	}
	empty, err := sideDiagnosticsJSON()
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("no diagnostics = %v, %v; want an empty array", empty, err)
	}
}

// TestColorDiff: each resource, operation and effective change line takes
// the color of its change; the header, the section line and the summary
// stay plain; text of another shape stays plain.
func TestColorDiff(t *testing.T) {
	d := &diff.Diff{
		Resources: []diff.Resource{
			{Change: diff.Modified, Ops: []diff.Op{{Kind: diff.OpReplace}, {Kind: diff.OpAdd}, {Kind: diff.OpRemove}, {Kind: diff.OpMove}}},
			{Change: diff.Added},
			{Change: diff.Removed},
		},
		Effective: []diff.ChainChange{{Op: diff.OpAdd}, {Op: diff.OpRemove}, {Op: diff.OpMove}},
	}
	text := "H\n\nM\nR\nA\nD\nV\nN\nX\n\nEffective Filter Chain changes:\nE1\nE2\nE3\n\nS\n"
	want := "H\n\n" +
		ansiYellow + "M" + ansiReset + "\n" + ansiYellow + "R" + ansiReset + "\n" + ansiGreen + "A" + ansiReset + "\n" +
		ansiRed + "D" + ansiReset + "\n" + ansiYellow + "V" + ansiReset + "\n" + ansiGreen + "N" + ansiReset + "\n" +
		ansiRed + "X" + ansiReset + "\n\nEffective Filter Chain changes:\n" +
		ansiGreen + "E1" + ansiReset + "\n" + ansiRed + "E2" + ansiReset + "\n" + ansiYellow + "E3" + ansiReset + "\n\nS\n"
	if got := string(colorDiff([]byte(text), d)); got != want {
		t.Errorf("colorDiff =\n%q\nwant\n%q", got, want)
	}
	if got := string(colorDiff([]byte("H\n\nS\n"), &diff.Diff{})); got != "H\n\nS\n" {
		t.Errorf("colorDiff of no changes = %q", got)
	}
	if got := string(colorDiff([]byte("H\nextra\n\nS\n"), &diff.Diff{})); got != "H\nextra\n\nS\n" {
		t.Errorf("colorDiff of a mismatched text = %q, want it plain", got)
	}
}

// TestColorsDecision: color needs a terminal, TERM other than dumb and an
// unset or empty NO_COLOR.
func TestColorsDecision(t *testing.T) {
	for _, tc := range []struct {
		environ  []string
		terminal bool
		want     bool
	}{
		{nil, true, true},
		{nil, false, false},
		{[]string{"TERM=xterm-256color"}, true, true},
		{[]string{"TERM=dumb"}, true, false},
		{[]string{"NO_COLOR=1"}, true, false},
		{[]string{"NO_COLOR="}, true, true},
	} {
		o := Options{terminal: func(io.Writer) bool { return tc.terminal }}
		inv := &invocation{sio: command.NewIO(io.Discard, io.Discard, tc.environ, epoch, "linux"), opts: o}
		if got := inv.colors(io.Discard); got != tc.want {
			t.Errorf("colors with %q, terminal %v = %v, want %v", tc.environ, tc.terminal, got, tc.want)
		}
	}
	inv := &invocation{sio: command.IO{Stdout: io.Discard}, opts: Options{terminal: func(io.Writer) bool { return true }}}
	if !inv.colors(io.Discard) {
		t.Error("an IO without an environment lookup refuses color on a terminal")
	}
}

// TestIsTerminal: a buffer, a regular file and a nil file are not
// terminals.
func TestIsTerminal(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var nilFile *os.File
	for name, w := range map[string]io.Writer{"buffer": &bytes.Buffer{}, "regular file": f, "nil file": nilFile} {
		if isTerminal(w) {
			t.Errorf("isTerminal(%s) = true", name)
		}
	}
}

// TestOneFile: the file system of standard input holds one regular file,
// "-"; every other name does not exist and an invalid one is refused.
func TestOneFile(t *testing.T) {
	src := stdinSource([]byte("data"))
	if src.Path != "-" {
		t.Fatalf("Path = %q", src.Path)
	}
	fi, err := fs.Stat(src.FS, "-")
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != 4 || fi.Name() != "-" || fi.IsDir() || fi.Sys() != nil || !fi.ModTime().IsZero() {
		t.Fatalf("Stat(-) = %v, %v", fi, err)
	}
	b, err := fs.ReadFile(src.FS, "-")
	if err != nil || string(b) != "data" {
		t.Errorf("ReadFile(-) = %q, %v", b, err)
	}
	if _, err := src.FS.Open("other"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(other) = %v, want ErrNotExist", err)
	}
	if _, err := src.FS.Open("../-"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("Open(../-) = %v, want ErrInvalid", err)
	}
	if src == (loader.Source{}) {
		t.Error("the source is zero")
	}
}

// TestStartsObject: a /config/dump document starts with "{" after JSON
// white space.
func TestStartsObject(t *testing.T) {
	for in, want := range map[string]bool{
		`{"a":1}`: true, " \t\r\n{": true, "": false, "   ": false, "apiVersion: x": false, "[{}]": false, "\f{": false,
	} {
		if got := startsObject([]byte(in)); got != want {
			t.Errorf("startsObject(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestReadLimited: a reader is read to its end, or to one byte past the
// limit.
func TestReadLimited(t *testing.T) {
	got, err := readLimited(strings.NewReader("abc"), 8)
	if err != nil || string(got) != "abc" {
		t.Errorf("readLimited(abc, 8) = %q, %v", got, err)
	}
	got, err = readLimited(endless{}, 8)
	if err != nil || len(got) != 9 {
		t.Errorf("readLimited(endless, 8) read %d bytes, %v; want 9", len(got), err)
	}
}

// TestDedupe: a finding repeated by ParseFile is kept once, sorted.
func TestDedupe(t *testing.T) {
	a := diag.Diagnostic{Code: "RZ-CFG-001", Severity: diag.SeverityError, Location: diag.Location{File: "b.yaml", Line: 2}, Message: "x"}
	b := diag.Diagnostic{Code: "RZ-CFG-007", Severity: diag.SeverityError, Location: diag.Location{File: "a.yaml", Line: 1}, Message: "y"}
	hinted := a
	hinted.Hint = "h"
	got := dedupe(diag.List{a, b, hinted, a, b})
	if len(got) != 3 || got[0].File != "a.yaml" {
		t.Errorf("dedupe = %+v", got)
	}
}

// TestQuoted: an authored value is quoted, at most 64 code points of it.
func TestQuoted(t *testing.T) {
	if got := quoted("ruralz/v9"); got != `"ruralz/v9"` {
		t.Errorf("quoted = %s", got)
	}
	long := strings.Repeat("é", 70)
	if got := quoted(long); got != `"`+strings.Repeat("é", 64)+`..."` {
		t.Errorf("quoted(long) = %s", got)
	}
	var v string
	if err := json.Unmarshal([]byte(quoted("a\"b")), &v); err != nil || v != `a"b` {
		t.Errorf("quoted(a\"b) = %s", quoted("a\"b"))
	}
}
