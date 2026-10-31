// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// commandTests names the end-to-end tests that run each ruralz command
// Planned for the milestone in progress: every M1 command of foundation
// pack section 9 has an end-to-end test (docs/roadmap/01-roadmap-and-milestones.md
// "M1 exit criteria" 5), with the cases of
// docs/engineering/03-testing-and-quality-strategy.md "End-to-end tests".
func commandTests() map[string][]func(*testing.T) {
	return map[string][]func(*testing.T){
		"ruralz bundle validate": {TestBundleCommands},
		"ruralz bundle render":   {TestBundleCommands, TestRESTExampleBundle},
		"ruralz bundle diff":     {TestBundleCommands, TestBundleDiffAgainstNode},
		"ruralz bundle build":    {TestBundleCommands},
		"ruralz dev run":         {TestDevRun, TestDevRunJSON, TestDevRunBusyPort},
		"ruralz dev tap":         {TestDevTap},
		"ruralz node drain":      {TestNodeDrain},
		"ruralz node dump":       {TestNodeDump, TestBundleDiffAgainstNode},
		"ruralz version":         {TestVersion},
		"ruralz completion":      {TestCompletion},
	}
}

// commandTables are the documents and headings of the tables that plan
// the commands: the registry of foundation pack section 9, and the CLI
// reference's command table, which also plans ruralz version and ruralz
// completion, the commands outside the noun-verb form.
func commandTables() [][2]string {
	return [][2]string{{packDoc, "## 9. CLI command registry"}, {cliDoc, "### Command table"}}
}

// TestEveryCommandHasAnEndToEndTest: each command a command table plans
// for the milestone in progress, or an earlier one, has end-to-end tests
// in commandTests, and each of them reaches a call that runs the command;
// commandTests names no other command.
func TestEveryCommandHasAnEndToEndTest(t *testing.T) {
	current := currentMilestone(t)
	planned := map[string]string{}
	for _, table := range commandTables() {
		for _, command := range plannedCommands(t, table[0], table[1], current) {
			if _, ok := planned[command]; !ok {
				planned[command] = table[0]
			}
		}
	}
	tests := commandTests()
	suite := parseSuite(t)
	for _, command := range slices.Sorted(maps.Keys(planned)) {
		fns, ok := tests[command]
		if !ok {
			t.Errorf("%s plans %s for M%d or earlier, and no end-to-end test runs it", planned[command], command, current)
			continue
		}
		words := strings.Fields(command)[1:]
		for _, fn := range fns {
			if name := funcName(fn); !suite.reaches(name, words) {
				t.Errorf("%s, an end-to-end test of %s, never runs ruralz %s", name, command, strings.Join(words, " "))
			}
		}
	}
	for _, command := range slices.Sorted(maps.Keys(tests)) {
		if _, ok := planned[command]; !ok {
			t.Errorf("commandTests names %s, which no command table plans for M%d or earlier", command, current)
		}
	}
}

// currentMilestone returns the number of the roadmap's first milestone
// section, the milestone in progress: the roadmap holds only the
// milestones not yet exited.
func currentMilestone(t testing.TB) int {
	t.Helper()
	m := regexp.MustCompile(`(?m)^## M([1-9][0-9]*) `).FindStringSubmatch(docText(t, roadmapDoc))
	if m == nil {
		t.Fatalf("%s has no milestone section", roadmapDoc)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// plannedCommands returns the commands of the table under heading in doc
// whose Planned cell, the row's last, starts with milestone current or an
// earlier one.
func plannedCommands(t testing.TB, doc, heading string, current int) []string {
	t.Helper()
	plannedFor := regexp.MustCompile(`^Planned \(M([1-9][0-9]*)\)`)
	var commands []string
	rows := 0
	for _, line := range strings.Split(docSection(t, doc, heading), "\n") {
		c := tableCells(line)
		if len(c) < 2 || c[0] == "Command" || strings.HasPrefix(c[0], "---") {
			continue
		}
		rows++
		if !strings.HasPrefix(c[0], "ruralz ") {
			t.Errorf("%s %q: the row %q names no ruralz command", doc, heading, c[0])
			continue
		}
		m := plannedFor.FindStringSubmatch(c[len(c)-1])
		if m == nil {
			t.Errorf("%s %q: %s has the Planned cell %q, which names no milestone", doc, heading, c[0], c[len(c)-1])
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil && n <= current {
			commands = append(commands, c[0])
		}
	}
	if rows == 0 {
		t.Fatalf("%s %q has no command table", doc, heading)
	}
	return commands
}

// funcName returns the name of fn, a top-level function of the suite.
func funcName(fn func(*testing.T)) string {
	full := runtime.FuncForPC(reflect.ValueOf(fn).Pointer()).Name()
	return full[strings.LastIndex(full, ".")+1:]
}

// suiteFuncs indexes the suite's function and method declarations by
// name.
type suiteFuncs map[string][]*ast.FuncDecl

// parseSuite parses the suite's Go files, in the package directory where
// go test runs it.
func parseSuite(t testing.TB) suiteFuncs {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	s := suiteFuncs{}
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				s[fd.Name.Name] = append(s[fd.Name.Name], fd)
			}
		}
	}
	return s
}

// named returns the declarations called name: the functions, or with
// methods set, the methods.
func (s suiteFuncs) named(name string, methods bool) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	for _, fd := range s[name] {
		if (fd.Recv != nil) == methods {
			out = append(out, fd)
		}
	}
	return out
}

// reaches reports whether the function name, or a function or method it
// calls or passes on, directly or through others, holds words as
// consecutive string literals among a call's arguments or a composite
// literal's elements, as the argument list of a ruralz command line does.
// A method call resolves to every method of its name, which can only add
// functions.
func (s suiteFuncs) reaches(name string, words []string) bool {
	seen := map[*ast.FuncDecl]bool{}
	queue := s.named(name, false)
	for len(queue) > 0 {
		fd := queue[0]
		queue = queue[1:]
		if seen[fd] {
			continue
		}
		seen[fd] = true
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				switch f := n.Fun.(type) {
				case *ast.Ident:
					queue = append(queue, s.named(f.Name, false)...)
				case *ast.SelectorExpr:
					queue = append(queue, s.named(f.Sel.Name, true)...)
				}
				for _, a := range n.Args {
					if id, ok := a.(*ast.Ident); ok {
						queue = append(queue, s.named(id.Name, false)...)
					}
				}
				found = found || holds(n.Args, words)
			case *ast.CompositeLit:
				found = found || holds(n.Elts, words)
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}

// holds reports whether exprs hold words as consecutive string literals.
func holds(exprs []ast.Expr, words []string) bool {
	for i := 0; i+len(words) <= len(exprs); i++ {
		match := true
		for j, w := range words {
			lit, ok := exprs[i+j].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				match = false
				break
			}
			if v, err := strconv.Unquote(lit.Value); err != nil || v != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
