// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package property holds the cross-package property suites of
// docs/engineering/03-testing-and-quality-strategy.md "Required
// properties": one testing.F target per row of the Required properties
// table due by the current milestone, each driving the real packages end
// to end, with no property library. Plain go test runs the seeds, as
// pr-fast does; the nightly Fuzz job fuzzes each target longer
// (internal/tool/fuzzplan lists them).
//
// The configuration targets run the loader through the configuration
// pipeline facade (internal/config/pipeline) to the Revision, with the
// offline Policy checks every binary links (checks.Table()) and the CEL
// compiler, seeded from the golden corpus under test/conformance/config.
// Where a property names an exit code or the rows a command prints, the
// target runs the ruralz commands in process, dispatched through the
// root's command table (internal/cli.Registry) as cmd/ruralz dispatches
// them. The Rate Limit target runs the ratelimit Filter the snapshot
// compiler builds from a Bundle on N simulated Nodes, each with its own
// key table, through the Filter Chain executor over one shared State
// Store. The Last-Known-Good target runs internal/gateway/source, reload
// and lkg together over one data directory.
//
// The per-package targets stay as unit-level properties. A registry here
// (requiredTargets) maps each row's Area to its target, and TestRequiredProperties
// ties every row to it: it reads the current milestone from the first
// milestone section of docs/roadmap/01-roadmap-and-milestones.md and fails
// when a row due by that milestone has no registered target, when a
// registered target does not exist in this package, or when a registered
// Area names no row.
//
// Every target is hermetic: time comes from internal/clock fakes,
// randomness from the fuzzed seed, and nothing but loopback is reached.
// Waits for goroutines the tests start poll conditions under a real-time
// bound and never sleep for a fixed time.
package property

import (
	"bufio"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
)

// Documents TestRequiredProperties reads, relative to the repository root.
const (
	// strategyDoc holds the Required properties table.
	strategyDoc = "docs/engineering/03-testing-and-quality-strategy.md"
	// roadmapDoc holds the milestone sections, the current one first.
	roadmapDoc = "docs/roadmap/01-roadmap-and-milestones.md"
	// propertiesHeading is the section of strategyDoc that holds the table.
	propertiesHeading = "### Required properties"
)

// target ties one row of the Required properties table to its fuzz
// target in this package.
type target struct {
	// Area is the row's Area cell, verbatim.
	Area string
	// Fuzz is the name of the func(*testing.F) target.
	Fuzz string
}

// requiredTargets returns the Area of every row this package covers with its
// target. A row due by a later milestone joins when its milestone is the
// current one.
func requiredTargets() []target {
	return []target{
		{Area: "Canonical form", Fuzz: "FuzzCanonicalForm"},
		{Area: "Render", Fuzz: "FuzzRenderIdempotent"},
		{Area: "Conversion", Fuzz: "FuzzConversion"},
		{Area: "Substitution", Fuzz: "FuzzSubstitution"},
		{Area: "Precedence", Fuzz: "FuzzPrecedence"},
		{Area: "Effective chain", Fuzz: "FuzzEffectiveChain"},
		{Area: "Diff", Fuzz: "FuzzDiff"},
		{Area: "Rate Limit", Fuzz: "FuzzRateLimit"},
		{Area: "Last-Known-Good, file mode", Fuzz: "FuzzLastKnownGoodFileMode"},
	}
}

// row is one row of the Required properties table.
type row struct {
	// Area is the Area cell.
	Area string
	// Milestone is the number n of the Milestone cell "Planned (Mn)".
	Milestone int
}

// TestRequiredProperties ties every row of the Required properties table
// due by the current milestone to a registered fuzz target of this
// package, and every registered target to a row.
func TestRequiredProperties(t *testing.T) {
	root, err := corpus.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel))) //nolint:gosec // G304: a fixed document of the repository.
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	rows, err := requiredRows(read(strategyDoc))
	if err != nil {
		t.Fatalf("%s: %v", strategyDoc, err)
	}
	current, err := currentMilestone(read(roadmapDoc))
	if err != nil {
		t.Fatalf("%s: %v", roadmapDoc, err)
	}
	targets, err := fuzzTargets(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range checkRegistry(rows, current, requiredTargets(), targets) {
		t.Error(p)
	}
}

// checkRegistry returns every way reg fails to tie rows to targets: a row
// due by milestone current without a registered target, a registered
// target that is not among targets, a registered Area that names no row,
// and an Area listed twice in the table or the registry.
func checkRegistry(rows []row, current int, reg []target, targets map[string]bool) []string {
	var problems []string
	byArea := map[string]row{}
	for _, r := range rows {
		if _, dup := byArea[r.Area]; dup {
			problems = append(problems, fmt.Sprintf("the Required properties table lists Area %q twice", r.Area))
		}
		byArea[r.Area] = r
	}
	registered := map[string]bool{}
	for _, tg := range reg {
		if registered[tg.Area] {
			problems = append(problems, fmt.Sprintf("Area %q is registered twice", tg.Area))
		}
		registered[tg.Area] = true
		if _, ok := byArea[tg.Area]; !ok {
			problems = append(problems, fmt.Sprintf("registered Area %q names no row of the Required properties table", tg.Area))
		}
		if !targets[tg.Fuzz] {
			problems = append(problems, fmt.Sprintf("target %s, registered for Area %q, is no func(*testing.F) of test/property", tg.Fuzz, tg.Area))
		}
	}
	for _, r := range rows {
		if r.Milestone <= current && !registered[r.Area] {
			problems = append(problems, fmt.Sprintf("Area %q is due by M%d, the current milestone M%d, and has no registered target", r.Area, r.Milestone, current))
		}
	}
	return problems
}

// milestoneCell matches the Milestone cell of a row.
const milestoneCell = `^Planned \(M([1-9][0-9]*)\)$`

// requiredRows returns the rows of the first table of the Required
// properties section of the strategy document doc, read by the header
// cells Area and Milestone. A missing section, table or column, an empty
// Area or a Milestone cell other than "Planned (Mn)" is an error, since
// it would hide a row.
func requiredRows(doc string) ([]row, error) {
	lines := strings.Split(doc, "\n")
	start := slices.Index(lines, propertiesHeading)
	if start < 0 {
		return nil, fmt.Errorf("no %q section", propertiesHeading)
	}
	milestone := regexp.MustCompile(milestoneCell)
	var header []string
	var rows []row
	areaCol, milestoneCol := -1, -1
	for i := start + 1; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "#") {
			break
		}
		if !strings.HasPrefix(line, "|") {
			if header != nil {
				break
			}
			continue
		}
		cells := tableCells(line)
		if header == nil {
			header = cells
			areaCol, milestoneCol = slices.Index(cells, "Area"), slices.Index(cells, "Milestone")
			if areaCol < 0 || milestoneCol < 0 {
				return nil, fmt.Errorf("the first table of %q has no Area and Milestone columns: %q", propertiesHeading, line)
			}
			i++ // the delimiter row
			continue
		}
		if len(cells) != len(header) {
			return nil, fmt.Errorf("row %q has %d cells, the header %d", line, len(cells), len(header))
		}
		m := milestone.FindStringSubmatch(cells[milestoneCol])
		if m == nil {
			return nil, fmt.Errorf("row %q: Milestone %q is not \"Planned (Mn)\"", cells[areaCol], cells[milestoneCol])
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("row %q: %w", cells[areaCol], err)
		}
		if cells[areaCol] == "" {
			return nil, fmt.Errorf("row %q has an empty Area", line)
		}
		rows = append(rows, row{Area: cells[areaCol], Milestone: n})
	}
	if header == nil {
		return nil, fmt.Errorf("no table in %q", propertiesHeading)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("the table of %q has no rows", propertiesHeading)
	}
	return rows, nil
}

// tableCells splits one Markdown table line into its trimmed cells. A pipe
// escaped as \| stays inside its cell, as Markdown renders it.
func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	if strings.HasSuffix(line, "|") && !strings.HasSuffix(line, `\|`) {
		line = line[:len(line)-1]
	}
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line) && line[i+1] == '|':
			cur.WriteByte('|')
			i++
		case line[i] == '|':
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

// currentMilestone returns n of the first milestone section, "## Mn ...",
// of the roadmap doc: the roadmap holds milestones not yet exited, in
// order, so the first is the current one.
func currentMilestone(doc string) (int, error) {
	heading := regexp.MustCompile(`^## M([1-9][0-9]*) `)
	sc := bufio.NewScanner(strings.NewReader(doc))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if m := heading.FindStringSubmatch(sc.Text()); m != nil {
			return strconv.Atoi(m[1])
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("no milestone section \"## Mn ...\"")
}

// fuzzTargets returns the names of the fuzz targets of the Go test files
// in dir: the top-level functions FuzzXxx, Xxx not starting with a lower
// case letter, whose signature is func(*testing.F), as go test runs them.
func fuzzTargets(dir string) (map[string]bool, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if ok && fn.Recv == nil && isFuzzName(fn.Name.Name) && takesF(fn.Type) {
				out[fn.Name.Name] = true
			}
		}
	}
	return out, nil
}

// isFuzzName reports a name go test runs as a fuzz target: Fuzz, then
// nothing or a rune that is not a lower case letter.
func isFuzzName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Fuzz")
	if !ok {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || !unicode.IsLower(r)
}

// takesF reports a signature func(*testing.F) with no results.
func takesF(ft *ast.FuncType) bool {
	if ft.Results != nil && len(ft.Results.List) > 0 || ft.Params == nil || len(ft.Params.List) != 1 || len(ft.Params.List[0].Names) > 1 {
		return false
	}
	star, ok := ft.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "F" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "testing"
}

func TestCheckRegistry(t *testing.T) {
	rows := []row{{Area: "A", Milestone: 1}, {Area: "B", Milestone: 1}, {Area: "Later", Milestone: 2}}
	reg := []target{{Area: "A", Fuzz: "FuzzA"}, {Area: "B", Fuzz: "FuzzB"}}
	targets := map[string]bool{"FuzzA": true, "FuzzB": true}
	tests := []struct {
		name    string
		rows    []row
		current int
		reg     []target
		targets map[string]bool
		want    []string
	}{
		{name: "every due row registered, a later row without a target", rows: rows, current: 1, reg: reg, targets: targets},
		{
			name: "a due row without a target", rows: rows, current: 1, reg: reg[:1], targets: targets,
			want: []string{`Area "B" is due by M1, the current milestone M1, and has no registered target`},
		},
		{
			name: "the next milestone's rows become due", rows: rows, current: 2, reg: reg, targets: targets,
			want: []string{`Area "Later" is due by M2, the current milestone M2, and has no registered target`},
		},
		{
			name: "a registered target that does not exist", rows: rows, current: 1, reg: reg, targets: map[string]bool{"FuzzA": true},
			want: []string{`target FuzzB, registered for Area "B", is no func(*testing.F) of test/property`},
		},
		{
			name: "a registered Area that names no row", rows: rows, current: 1,
			reg: append(slices.Clone(reg), target{Area: "Gone", Fuzz: "FuzzA"}), targets: targets,
			want: []string{`registered Area "Gone" names no row of the Required properties table`},
		},
		{
			name: "an Area registered twice", rows: rows, current: 1,
			reg: append(slices.Clone(reg), target{Area: "A", Fuzz: "FuzzB"}), targets: targets,
			want: []string{`Area "A" is registered twice`},
		},
		{
			name: "an Area listed twice in the table", rows: append(slices.Clone(rows), row{Area: "A", Milestone: 1}), current: 1, reg: reg, targets: targets,
			want: []string{`the Required properties table lists Area "A" twice`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkRegistry(tc.rows, tc.current, tc.reg, tc.targets); !slices.Equal(got, tc.want) {
				t.Errorf("checkRegistry() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRequiredRows(t *testing.T) {
	table := func(header, rows string) string {
		return "# Doc\n\n" + propertiesHeading + "\n\nText.\n\n" + header + "\n|---|---|\n" + rows + "\nAfter.\n\n### Next\n\n| Area | Milestone |\n|---|---|\n| Other | Planned (M1) |\n"
	}
	tests := []struct {
		name    string
		doc     string
		want    []row
		wantErr string
	}{
		{
			name: "rows by Area and Milestone, the next section ignored",
			doc:  table("| Area | Property | Milestone |", "| Canonical form | a \\| b | Planned (M1) |\n| Rollout | c | Planned (M2) |"),
			want: []row{{Area: "Canonical form", Milestone: 1}, {Area: "Rollout", Milestone: 2}},
		},
		{
			name: "columns found by name",
			doc:  table("| Milestone | Area |", "| Planned (M3) | Token Budget |"),
			want: []row{{Area: "Token Budget", Milestone: 3}},
		},
		{name: "no section", doc: "# Doc\n\n| Area | Milestone |\n", wantErr: "no \"### Required properties\" section"},
		{name: "no table", doc: "# Doc\n\n" + propertiesHeading + "\n\nText only.\n\n## Next\n", wantErr: "no table"},
		{name: "no Milestone column", doc: table("| Area | Property |", "| A | b |"), wantErr: "no Area and Milestone columns"},
		{name: "a milestone that is not Planned (Mn)", doc: table("| Area | Milestone |", "| A | Shipped |"), wantErr: `Milestone "Shipped"`},
		{name: "a row with a missing cell", doc: table("| Area | Property | Milestone |", "| A | Planned (M1) |"), wantErr: "has 2 cells"},
		{name: "an empty Area", doc: table("| Area | Milestone |", "|  | Planned (M1) |"), wantErr: "empty Area"},
		{name: "a table without rows", doc: table("| Area | Milestone |", ""), wantErr: "no rows"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := requiredRows(tc.doc)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("requiredRows() error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("requiredRows() = %v, %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestCurrentMilestone(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    int
		wantErr bool
	}{
		{name: "the first milestone section", doc: "# Roadmap\n\n## Summary\n\n## M2 WASM\n\n### M2 scope\n\n## M3 AI\n", want: 2},
		{name: "a heading below level two is no section", doc: "# Roadmap\n\n### M1 scope\n\n## M4 Events\n", want: 4},
		{name: "no milestone section", doc: "# Roadmap\n\n## Sequencing\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := currentMilestone(tc.doc)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("currentMilestone() = %d, %v, want %d (error %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestFuzzTargets(t *testing.T) {
	dir := t.TempDir()
	src := `package x

import "testing"

func FuzzGood(f *testing.F) {}
func FuzzTwoArgs(f *testing.F, n int) {}
func FuzzWrongType(t *testing.T) {}
func FuzzResult(f *testing.F) error { return nil }
func (r recv) FuzzMethod(f *testing.F) {}
func helper(f *testing.F) {}
func Fuzzy(f *testing.F) {}
func Fuzz(f *testing.F) {}
`
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "y.go"), []byte("package x\n\nimport \"testing\"\n\nfunc FuzzNotATest(f *testing.F) {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := fuzzTargets(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"FuzzGood": true, "Fuzz": true}
	if len(got) != len(want) || !got["FuzzGood"] || !got["Fuzz"] {
		t.Errorf("fuzzTargets() = %v, want %v", got, want)
	}
}
