// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/buildinfo"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/completion"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// Tests for the root: the command table against the CLI command registry
// and the reference's command table, the golden help of every level, the
// Planned rows, ruralz version and completion over the real table.

// update rewrites the golden files: go test ./internal/cli -update.
var update = flag.Bool("update", false, "rewrite testdata golden files")

// referenceDoc is the CLI and API surface reference, read in place.
const referenceDoc = "../../docs/reference/01-cli-and-api-surface.md"

// packDoc is the foundation pack, read in place.
const packDoc = "../../docs/_meta/foundation-pack.md"

// roadmapDoc holds the milestones not yet exited, the one in progress
// first.
const roadmapDoc = "../../docs/roadmap/01-roadmap-and-milestones.md"

// testVersion is the CLI build version of the tests.
const testVersion = "0.4.0"

func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// testOptions are the CLI's options on a fake clock.
func testOptions() Options { return Options{Clock: clocktest.New(epoch()), Version: testVersion} }

// result is what one command.Execute returned.
type result struct {
	code           int
	stdout, stderr string
}

// execute runs ruralz args... over Registry(o) on goos.
func execute(t *testing.T, o Options, goos string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	sio := command.NewIO(&out, &errOut, []string{"HOME=/home/test"}, epoch, goos)
	code := command.Execute(t.Context(), sio, Program, Registry(o), args)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// golden compares got with testdata/<name>, rewriting it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", filepath.FromSlash(name))
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata.
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run with -update to accept)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// TestRegistryIsValid checks the real table with command.Check.
func TestRegistryIsValid(t *testing.T) {
	if err := command.Check(Registry(testOptions())); err != nil {
		t.Fatalf("Check(Registry) = %v", err)
	}
}

// runnablePaths are the runnable commands: the rows of the reference's
// command table that carry no Planned tag of their own, and the commands
// of the milestone in progress this build already runs, whose rows keep
// their tag until the milestone exits.
func runnablePaths() [][]string {
	return [][]string{
		{"bundle", "audit"},
		{"bundle", "build"},
		{"bundle", "diff"},
		{"bundle", "export"},
		{"bundle", "import"},
		{"bundle", "render"},
		{"bundle", "validate"},
		{"completion"},
		{"dev", "run"},
		{"dev", "tap"},
		{"node", "drain"},
		{"node", "dump"},
		{"version"},
	}
}

// specIndex returns the specs of the table by name.
func specIndex(specs []command.Spec) map[string]command.Spec {
	out := map[string]command.Spec{}
	for _, s := range specs {
		out[s.Name()] = s
	}
	return out
}

// TestRegistryRunsTheReleasedCommands pins the runnable commands: exactly
// the rows of the reference's command table that carry no Planned tag of
// their own, and the commands of the milestone in progress already built
// (runnablePaths).
func TestRegistryRunsTheReleasedCommands(t *testing.T) {
	var got [][]string
	for _, s := range Registry(testOptions()) {
		if s.New != nil {
			got = append(got, s.Path)
		}
	}
	slices.SortFunc(got, slices.Compare)
	if want := runnablePaths(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("runnable commands = %q, want %q", got, want)
	}
}

// status is the Planned cell of a command row. An empty cell is a command
// the release runs; a cell that opens with Planned (Mn) is a command not
// built yet, recognized as Planned at Mn; a cell of later parts only, each
// "<part> Planned (Mn)", is a command the release runs whose parts are
// planned. Parts after the first follow "; ", as in "Planned (M2);
// TypeScript Planned (M3)" or "Revision sources Planned (M2)".
type status struct {
	runs      bool   // the cell carries no Planned tag of the command's own
	milestone string // "M2" of the command's own Planned (M2); empty when it runs
}

// parseStatus reads the Planned cell of a command row.
func parseStatus(cell string) (status, error) {
	cell = strings.TrimSpace(cell)
	st := status{runs: true}
	if cell == "" {
		return st, nil
	}
	own := regexp.MustCompile(`^Planned \((M[0-9])\)$`)
	part := regexp.MustCompile(`^\S.* Planned \(M[0-9]\)$`)
	for i, p := range strings.Split(cell, "; ") {
		if m := own.FindStringSubmatch(p); m != nil && i == 0 {
			st = status{milestone: m[1]}
			continue
		}
		if !part.MatchString(p) || strings.Count(p, "Planned (") != 1 {
			return status{}, fmt.Errorf("the Planned cell %q is neither empty, Planned (Mn) nor later parts each tagged Planned (Mn)", cell)
		}
	}
	return st, nil
}

// checkStatus checks the registered command s against the status of its
// row: a row without a Planned tag of its own runs it, and a Planned (Mn)
// tag leaves it Planned at Mn.
//
// current is the milestone in progress: a command of that milestone may
// already run, since its row keeps the Planned tag until the milestone
// exits, while a command of a later milestone never runs.
func checkStatus(st status, s command.Spec, current string) error {
	switch {
	case st.runs:
		if s.New == nil {
			return errors.New("does not run, but its row carries no Planned tag of its own")
		}
	case s.New != nil && st.milestone == current:
	case s.New != nil:
		return fmt.Errorf("runs, but its row is Planned (%s), after %s, the milestone in progress", st.milestone, current)
	case s.Planned != st.milestone:
		return fmt.Errorf("is Planned (%s), but its row is Planned (%s)", s.Planned, st.milestone)
	}
	return nil
}

// milestoneInProgress returns the roadmap's milestone in progress: "Mn" of
// its first "## Mn " heading, or "" when it holds none.
func milestoneInProgress(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(roadmapDoc)
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`(?m)^## (M[0-9]+) `).FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

// commandRow is one row of a command table: the reference's command table
// or the CLI command registry of the foundation pack.
type commandRow struct {
	command string // "ruralz bundle export openapi"
	purpose string // backticks removed
	status  status // the last cell
}

// commandRows parses the command table in section: its lines of exactly
// columns cells whose first cell is a ruralz command. doc names the
// document in failures.
func commandRows(t *testing.T, doc, section string, columns int) []commandRow {
	t.Helper()
	var rows []commandRow
	for _, line := range strings.Split(section, "\n") {
		cells := strings.Split(strings.Trim(line, "|"), "|")
		cmd := strings.Trim(strings.TrimSpace(cells[0]), "`")
		if len(cells) != columns || !strings.HasPrefix(cmd, Program+" ") {
			continue
		}
		st, err := parseStatus(cells[columns-1])
		if err != nil {
			t.Fatalf("%s: row %q: %v", doc, line, err)
		}
		rows = append(rows, commandRow{
			command: cmd,
			purpose: strings.ReplaceAll(strings.TrimSpace(cells[1]), "`", ""),
			status:  st,
		})
	}
	if len(rows) < 30 {
		t.Fatalf("%s: parsed %d rows of the command table", doc, len(rows))
	}
	return rows
}

// docSection returns the text of the document doc after its heading line,
// up to next, the start of the following heading.
func docSection(t *testing.T, doc, heading, next string) string {
	t.Helper()
	data, err := os.ReadFile(doc) //nolint:gosec // G304: a fixed document of the repository.
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(data), heading+"\n")
	if !ok {
		t.Fatalf("%s has no %q section", doc, heading)
	}
	section, _, _ = strings.Cut(section, next)
	return section
}

// referenceRows parses the "### Command table" section of the reference.
func referenceRows(t *testing.T) []commandRow {
	t.Helper()
	return commandRows(t, referenceDoc, docSection(t, referenceDoc, "### Command table", "\n### "), 4)
}

// packRows parses the "## 9. CLI command registry" section of the
// foundation pack.
func packRows(t *testing.T) []commandRow {
	t.Helper()
	return commandRows(t, packDoc, docSection(t, packDoc, "## 9. CLI command registry", "\n## "), 3)
}

// TestParseStatus reads the Planned cells of command rows: empty, a
// Planned (Mn) tag of the command's own, alone or before tagged later
// parts, or tagged later parts only; anything else fails.
func TestParseStatus(t *testing.T) {
	runs := status{runs: true}
	tests := []struct {
		name string
		cell string
		want status
		err  bool
	}{
		{name: "an empty cell", cell: "", want: runs},
		{name: "a cell of spaces", cell: "  ", want: runs},
		{name: "a Planned tag", cell: " Planned (M2) ", want: status{milestone: "M2"}},
		{name: "a Planned tag before later parts", cell: "Planned (M2); TypeScript Planned (M3); C# Planned (M4)", want: status{milestone: "M2"}},
		{name: "a Planned tag of an exited milestone", cell: "Planned (M1)", want: status{milestone: "M1"}},
		{name: "a later part of a command that runs", cell: "Revision sources Planned (M2)", want: runs},
		{name: "later parts of a command that runs", cell: "Revision sources Planned (M2); OCI sources Planned (M3)", want: runs},
		{name: "a word", cell: "Shipped", err: true},
		{name: "a release", cell: "0.1.0", err: true},
		{name: "a release before later parts", cell: "0.1.0; Revision sources Planned (M2)", err: true},
		{name: "a bare milestone", cell: "M1", err: true},
		{name: "a part without a tag", cell: "Revision sources", err: true},
		{name: "a Planned tag run into later text", cell: "Planned (M2) HTTP", err: true},
		{name: "a later part without a tag", cell: "Planned (M2); TypeScript", err: true},
		{name: "a second tag of the command's own", cell: "Planned (M2); Planned (M3)", err: true},
		{name: "a tag of the command's own after a part", cell: "Revision sources Planned (M2); Planned (M3)", err: true},
		{name: "a part of two tags", cell: "Planned (M2) Planned (M3)", err: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStatus(tc.cell)
			if (err != nil) != tc.err || got != tc.want {
				t.Errorf("parseStatus(%q) = %+v, %v; want %+v, error %v", tc.cell, got, err, tc.want, tc.err)
			}
		})
	}
}

// TestCheckStatus: a row without a Planned tag of its own needs a runnable
// command, and a Planned (Mn) row a command Planned at Mn, whichever
// milestone Mn is.
func TestCheckStatus(t *testing.T) {
	runs := command.Spec{Path: []string{"bundle", "build"}, New: func() command.Command { return nil }}
	plannedM2 := command.Spec{Path: []string{"bundle", "push"}, Planned: "M2"}
	tests := []struct {
		name string
		st   status
		spec command.Spec
		want string // a substring of the error; empty for none
	}{
		{name: "an untagged row whose command runs", st: status{runs: true}, spec: runs},
		{name: "an untagged row whose command is Planned", st: status{runs: true}, spec: plannedM2, want: "does not run, but its row carries no Planned tag of its own"},
		{name: "a Planned row whose command is Planned at its milestone", st: status{milestone: "M2"}, spec: plannedM2},
		{name: "a row of the milestone in progress whose command already runs", st: status{milestone: "M2"}, spec: runs},
		{name: "a row of a later milestone whose command runs", st: status{milestone: "M3"}, spec: runs, want: "runs, but its row is Planned (M3), after M2, the milestone in progress"},
		{name: "a Planned (M1) row whose command runs", st: status{milestone: "M1"}, spec: runs, want: "runs, but its row is Planned (M1), after M2, the milestone in progress"},
		{name: "a Planned row whose command is Planned at another milestone", st: status{milestone: "M3"}, spec: plannedM2, want: "is Planned (M2), but its row is Planned (M3)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkStatus(tc.st, tc.spec, "M2")
			if tc.want == "" {
				if err != nil {
					t.Errorf("checkStatus() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("checkStatus() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// specPath returns the table path of a reference command: its noun and
// verb, or its one word; formats such as openapi are arguments.
func specPath(command string) string {
	words := strings.Fields(strings.TrimPrefix(command, Program+" "))
	if len(words) > 2 {
		words = words[:2]
	}
	return strings.Join(words, " ")
}

// TestRegistryMatchesReference checks every row of the reference's command
// table against the table: the command is registered, runnable exactly
// when the row's Planned cell carries no tag of the command's own, else
// Planned at the milestone of that Planned (Mn) tag, and a command that
// one row describes alone has the row's purpose as its summary.
func TestRegistryMatchesReference(t *testing.T) {
	specs := specIndex(Registry(testOptions()))
	rows := referenceRows(t)
	current := milestoneInProgress(t)
	perPath := map[string]int{}
	for _, r := range rows {
		perPath[specPath(r.command)]++
	}
	for _, r := range rows {
		p := specPath(r.command)
		s, ok := specs[p]
		if !ok {
			t.Errorf("%s is not registered", r.command)
			continue
		}
		if err := checkStatus(r.status, s, current); err != nil {
			t.Errorf("%s %v", r.command, err)
		}
		if perPath[p] == 1 && s.Summary != r.purpose {
			t.Errorf("%s: summary %q, want the reference's purpose %q", r.command, s.Summary, r.purpose)
		}
	}
	for name, s := range specs {
		if s.IsNoun() {
			continue
		}
		if !slices.ContainsFunc(rows, func(r commandRow) bool { return specPath(r.command) == name }) {
			t.Errorf("ruralz %s is registered but the reference's command table lacks it", name)
		}
	}
}

// TestRegistryCoversThePack checks that the table holds every command of
// the CLI command registry (foundation pack section 9), runnable exactly
// when the row's Planned cell carries no tag of the command's own, else
// Planned at the milestone of that Planned (Mn) tag, and only the
// registry's nouns.
func TestRegistryCoversThePack(t *testing.T) {
	specs := specIndex(Registry(testOptions()))
	current := milestoneInProgress(t)
	for _, r := range packRows(t) {
		s, ok := specs[specPath(r.command)]
		if !ok {
			t.Errorf("%s of the CLI command registry is not registered", r.command)
			continue
		}
		if err := checkStatus(r.status, s, current); err != nil {
			t.Errorf("%s of the CLI command registry %v", r.command, err)
		}
	}
	nouns := []string{"bundle", "rollout", "plugin", "ai", "dev", "node", "control", "test"}
	for name := range specs {
		word, _, two := strings.Cut(name, " ")
		if two && !slices.Contains(nouns, word) {
			t.Errorf("ruralz %s uses the noun %q, which the CLI command registry does not fix", name, word)
		}
		if !two && name != "version" && name != "completion" && !slices.Contains(nouns, word) {
			t.Errorf("ruralz %s is a one-word command besides version and completion", name)
		}
	}
}

// TestGoldenHelp pins the help of the root, every noun with a runnable
// verb and every runnable command; each help form prints the same text to
// standard output and exits 0.
func TestGoldenHelp(t *testing.T) {
	o := testOptions()
	type form struct {
		name  string
		forms [][]string
	}
	cases := []form{{"root", [][]string{{"--help"}, {"-h"}, {"help"}}}}
	for _, noun := range []string{"bundle", "dev", "node"} {
		cases = append(cases, form{noun, [][]string{{noun, "--help"}, {"help", noun}}})
	}
	for _, p := range runnablePaths() {
		cases = append(cases, form{strings.Join(p, "-"), [][]string{
			append(slices.Clone(p), "--help"),
			append([]string{"help"}, p...),
		}})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var first string
			for i, args := range c.forms {
				r := execute(t, o, "linux", args...)
				if r.code != ExitOK || r.stderr != "" {
					t.Fatalf("%q: exit %d, stderr %q", args, r.code, r.stderr)
				}
				if i == 0 {
					first = r.stdout
					golden(t, "help/"+c.name+".golden", r.stdout)
				} else if r.stdout != first {
					t.Errorf("%q printed different help:\n%s", args, r.stdout)
				}
			}
		})
	}
}

// TestBareInvocation: ruralz alone prints the root help to standard error
// and exits 2.
func TestBareInvocation(t *testing.T) {
	r := execute(t, testOptions(), "linux")
	help := execute(t, testOptions(), "linux", "--help")
	if r.code != ExitNoResult || r.stdout != "" || r.stderr != help.stdout {
		t.Fatalf("bare: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

// TestPlannedCommands: every Planned row exits 2 naming its milestone,
// help included, and no help lists it.
func TestPlannedCommands(t *testing.T) {
	o := testOptions()
	root := execute(t, o, "linux", "--help").stdout
	for _, s := range Registry(o) {
		if s.Planned == "" {
			continue
		}
		want := "ruralz " + s.Name() + " is Planned (" + s.Planned + ") and not in this build\n"
		for _, args := range [][]string{s.Path, append(slices.Clone(s.Path), "--help"), append([]string{"help"}, s.Path...)} {
			r := execute(t, o, "linux", args...)
			if r.code != ExitNoResult || r.stdout != "" || r.stderr != want {
				t.Errorf("%q = exit %d, stdout %q, stderr %q; want 2 and %q", args, r.code, r.stdout, r.stderr, want)
			}
		}
		if strings.Contains(root, "  "+s.Name()+" ") {
			t.Errorf("the root help lists the Planned %s", s.Name())
		}
	}
	for noun, m := range map[string]string{"test": "M2", "rollout": "M2", "plugin": "M2", "ai": "M3", "control": "M2"} {
		r := execute(t, o, "linux", noun)
		want := "ruralz " + noun + " is Planned (" + m + ") and not in this build\n"
		if r.code != ExitNoResult || r.stderr != want {
			t.Errorf("ruralz %s = exit %d, stderr %q; want 2 and %q", noun, r.code, r.stderr, want)
		}
	}
}

// TestPlatformGates: dev run is not planned on Windows, node drain on
// macOS and Windows, each exiting 2 with the reason; the others run there.
func TestPlatformGates(t *testing.T) {
	o := testOptions()
	cases := []struct {
		goos   string
		args   []string
		stderr string
	}{
		{"windows", []string{"dev", "run"}, "ruralz dev run is not planned on windows: there is no ruralzd build; use WSL\n"},
		{"darwin", []string{"node", "drain"}, "ruralz node drain is not planned on darwin: it needs /proc/locks\n"},
		{"windows", []string{"node", "drain"}, "ruralz node drain is not planned on windows: there is no ruralzd and no SIGTERM\n"},
	}
	for _, c := range cases {
		r := execute(t, o, c.goos, c.args...)
		if r.code != ExitNoResult || r.stdout != "" || r.stderr != c.stderr {
			t.Errorf("%s %q = exit %d, stdout %q, stderr %q; want 2 and %q", c.goos, c.args, r.code, r.stdout, r.stderr, c.stderr)
		}
	}
	if r := execute(t, o, "windows", "version"); r.code != ExitOK {
		t.Errorf("version on windows = exit %d, stderr %q", r.code, r.stderr)
	}
	if r := execute(t, o, "windows", "dev", "run", "--help"); r.code != ExitOK || !strings.HasPrefix(r.stdout, "Usage: ruralz dev run") {
		t.Errorf("dev run --help on windows = exit %d, %q", r.code, r.stdout)
	}
}

// TestVersionText checks the text form: aligned lines, the Version option
// replacing the build version, and the schema levels as
// <apiVersion>=<level>: level 1, which the fields added since 0.1.0's
// level 0 carry.
func TestVersionText(t *testing.T) {
	r := execute(t, testOptions(), "linux", "version")
	bi := buildinfo.Get()
	want := "version:      " + testVersion + "\n" +
		"commit:       " + bi.Commit + "\n" +
		"flavor:       " + bi.Flavor + "\n" +
		"apiVersions:  ruralz/v1alpha1\n" +
		"schemaLevels: ruralz/v1alpha1=1\n"
	if r.code != ExitOK || r.stdout != want || r.stderr != "" {
		t.Fatalf("version = exit %d\n%s\nstderr %q\nwant\n%s", r.code, r.stdout, r.stderr, want)
	}
}

// TestVersionJSON checks the JSON form: version, commit, flavor,
// apiVersions and schemaLevels in that order, and no pluginAbi or
// controlStream, which this build does not serve.
func TestVersionJSON(t *testing.T) {
	r := execute(t, testOptions(), "linux", "version", "--output", "json")
	if r.code != ExitOK || r.stderr != "" {
		t.Fatalf("version --output json = exit %d, stderr %q", r.code, r.stderr)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, r.stdout)
	}
	if string(got["version"]) != `"`+testVersion+`"` || string(got["apiVersions"]) == "" {
		t.Errorf("version = %s, apiVersions = %s", got["version"], got["apiVersions"])
	}
	var levels map[string]int
	if err := json.Unmarshal(got["schemaLevels"], &levels); err != nil || len(levels) != 1 || levels["ruralz/v1alpha1"] != 1 {
		t.Errorf("schemaLevels = %s, %v; want {\"ruralz/v1alpha1\": 1}", got["schemaLevels"], err)
	}
	for _, absent := range []string{"pluginAbi", "controlStream"} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s is printed, but this build serves no such contract", absent)
		}
	}
	order := []string{`"version"`, `"commit"`, `"flavor"`, `"apiVersions"`, `"schemaLevels"`}
	last := -1
	for _, k := range order {
		i := strings.Index(r.stdout, k)
		if i <= last {
			t.Fatalf("members out of order:\n%s", r.stdout)
		}
		last = i
	}
}

// TestVersionUsage: unsupported formats and arguments are usage errors.
func TestVersionUsage(t *testing.T) {
	for _, args := range [][]string{{"version", "--output", "yaml"}, {"version", "extra"}, {"version", "--no-such-flag"}} {
		r := execute(t, testOptions(), "linux", args...)
		if r.code != ExitNoResult || r.stdout != "" || r.stderr == "" {
			t.Errorf("%q = exit %d, stdout %q, stderr %q", args, r.code, r.stdout, r.stderr)
		}
	}
}

// TestCompletionCoversRegistry: the script of each shell holds every noun,
// verb and flag of the runnable commands, and no Planned noun.
func TestCompletionCoversRegistry(t *testing.T) {
	o := testOptions()
	specs := Registry(o)
	for _, sh := range completion.ShellNames() {
		r := execute(t, o, "linux", "completion", sh)
		if r.code != ExitOK || r.stderr != "" || r.stdout == "" {
			t.Fatalf("completion %s = exit %d, stderr %q", sh, r.code, r.stderr)
		}
		for _, s := range specs {
			if s.New == nil {
				continue
			}
			for _, w := range s.Path {
				if !strings.Contains(r.stdout, w) {
					t.Errorf("%s script lacks %q", sh, w)
				}
			}
			for _, f := range command.FlagSpecsOf(s) {
				if !strings.Contains(r.stdout, f.Name) {
					t.Errorf("%s script lacks --%s of %s", sh, f.Name, s.Name())
				}
			}
		}
		for _, planned := range []string{"rollout", "plugin", "control"} {
			if strings.Contains(r.stdout, planned) {
				t.Errorf("%s script completes the Planned noun %s", sh, planned)
			}
		}
	}
}

// TestRunUsesTheRealTable runs Run itself: it dispatches over Registry,
// with the process's streams.
func TestRunUsesTheRealTable(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"version", "--output", "json"}, &out, &errOut); code != ExitOK || errOut.Len() != 0 {
		t.Fatalf("Run(version) = %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"schemaLevels"`) {
		t.Errorf("Run(version) printed %q", out.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run(context.Background(), nil, &out, &errOut); code != ExitNoResult || out.Len() != 0 || !strings.HasPrefix(errOut.String(), "Usage: ruralz") {
		t.Errorf("Run() = %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
}

// TestExitCodes pins the root's exit codes, which never change once
// released.
func TestExitCodes(t *testing.T) {
	got := []int{ExitOK, ExitNegative, ExitNoResult, ExitWaiting, ExitInterrupted, ExitTerminated}
	if want := []int{0, 1, 2, 3, 130, 143}; !slices.Equal(got, want) {
		t.Fatalf("exit codes %v, want %v", got, want)
	}
}
