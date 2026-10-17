// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	goparser "go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The structural tests of this file keep the work counter complete (01
// req 53; review finding: the detectors' regular expressions, copies and
// visits ran uncounted or in one bulk charge): outside meteredFiles, no
// file of the package uses a tree.Node or schemaidx method or function
// whose cost grows with the instance it reads, nor any function or method
// of the slices, maps, strings, sort, regexp, bytes or unicode/utf8
// packages but the strings.Builder methods that copy nothing, nor
// diag.Nearest. The type checking follows internal/config/defaults
// (TestLookupsCounted, WP-37).

const (
	treePkg      = "github.com/ravindu-rev/ruralz/internal/config/tree"
	schemaidxPkg = "github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	diagPkg      = "github.com/ravindu-rev/ruralz/internal/config/diag"
)

// scanning lists, by types.Func.FullName, the tree and schemaidx methods
// and functions whose cost grows with what they read; TestScanningInventory
// keeps the list complete.
func scanning() []string {
	var out []string
	for _, m := range []string{"Get", "Set", "Delete", "At", "Clone", "JSONValue"} {
		out = append(out, "(*"+treePkg+".Node)."+m)
	}
	for _, m := range []string{"Select", "SelectValue", "DispatchValues", "PolicyConfig"} {
		out = append(out, "(*"+schemaidxPkg+".Node)."+m)
	}
	for _, m := range []string{"Lookup", "SchemaPath", "Walk", "Fields", "FieldsSeq"} {
		out = append(out, "(*"+schemaidxPkg+".Index)."+m)
	}
	return append(out, schemaidxPkg+".ItemElem", schemaidxPkg+".AppendItemJSON", diagPkg+".Nearest")
}

// scanningPackages are the standard library packages whose functions and
// methods each loop over a whole slice, map or string.
func scanningPackages() []string {
	return []string{"slices", "maps", "strings", "sort", "regexp", "bytes", "unicode/utf8"}
}

// copyFree are the methods of scanning packages that read no text: a
// strings.Builder's Grow allocates, String and Len return what it holds.
func copyFree() []string {
	return []string{"(*strings.Builder).Grow", "(*strings.Builder).String", "(*strings.Builder).Len"}
}

// meteredFiles are the files allowed to use what scanning lists, each a
// counted implementation with its own tests: meter.go (the helpers),
// scan.go (the scanner, TestSearchWindow), detect.go (the detectors,
// TestDetectorsMatchSpec) and hint.go (the hint search, TestHintCost),
// and environ.go, whose Environ and Escape run outside stage E's units.
func meteredFiles() []string {
	return []string{"meter.go", "scan.go", "detect.go", "hint.go", "environ.go"}
}

// checked is the type-checked non-test Go code of one directory.
type checked struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
	pkg   *types.Package
}

// goflagsOverlay returns the file replacements of the -overlay file that
// GOFLAGS names, by absolute path, or nil, so a mutation proof run as
// GOFLAGS=-overlay=<file> go test checks the replaced sources.
func goflagsOverlay(t *testing.T) map[string]string {
	t.Helper()
	for _, f := range strings.Fields(os.Getenv("GOFLAGS")) {
		path, ok := strings.CutPrefix(strings.TrimLeft(f, "-"), "overlay=")
		if !ok {
			continue
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: the overlay file GOFLAGS names.
		if err != nil {
			t.Fatalf("GOFLAGS overlay: %v", err)
		}
		var o struct{ Replace map[string]string }
		if err := json.Unmarshal(data, &o); err != nil {
			t.Fatalf("GOFLAGS overlay %s: %v", path, err)
		}
		return o.Replace
	}
	return nil
}

// typeCheck parses the non-test Go files of dir and type-checks them
// against the export data of their imports, which "go list -export"
// builds, so a selector is judged by the type it selects from, not by
// its spelling.
func typeCheck(t *testing.T, dir string) checked {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no go command to build export data: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "go", "list", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}\t{{.DepOnly}}", ".")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -export in %s: %v", dir, err)
	}
	exports := map[string]string{}
	var path string
	for line := range strings.Lines(string(out)) {
		f := strings.Split(strings.TrimRight(line, "\n"), "\t")
		if len(f) != 3 {
			t.Fatalf("go list line %q", line)
		}
		exports[f[0]] = f[1]
		if f[2] == "false" {
			path = f[0]
		}
	}
	c := checked{fset: token.NewFileSet()}
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	overlay := goflagsOverlay(t)
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, name := range names {
		seen[filepath.Join(absDir, filepath.Base(name))] = true
	}
	for abs, repl := range overlay {
		if filepath.Dir(abs) == absDir && strings.HasSuffix(abs, ".go") && !seen[abs] && repl != "" {
			names = append(names, filepath.Join(dir, filepath.Base(abs)))
		}
	}
	slices.Sort(names)
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		var src any
		if repl, ok := overlay[filepath.Join(absDir, filepath.Base(name))]; ok {
			if repl == "" {
				continue
			}
			data, err := os.ReadFile(repl) //nolint:gosec // G304: a replacement the GOFLAGS overlay names.
			if err != nil {
				t.Fatal(err)
			}
			src = data
		}
		f, err := goparser.ParseFile(c.fset, name, src, goparser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		c.files = append(c.files, f)
	}
	imp := importer.ForCompiler(c.fset, "gc", func(p string) (io.ReadCloser, error) {
		file, ok := exports[p]
		if !ok || file == "" {
			return nil, fmt.Errorf("no export data for %s", p)
		}
		return os.Open(file) //nolint:gosec // G304: an export data file go list named.
	})
	c.info = &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	if c.pkg, err = (&types.Config{Importer: imp}).Check(path, c.fset, c.files, c.info); err != nil {
		t.Fatalf("type-check %s: %v", dir, err)
	}
	return c
}

// directUses returns the uses, as "file:line: use", of a scanning method
// or function or of a function or method of a scanning package in the
// non-test Go files of dir other than allowed, and the number of files
// checked. A use is any selector that resolves to one by the type it
// selects from: a call, a method or function value, a method expression,
// a promoted method, and an interface method a scanning method satisfies.
func directUses(t *testing.T, dir string, allowed []string) (uses []string, checkedFiles int) {
	t.Helper()
	c := typeCheck(t, dir)
	scan := scanning()
	scanFuncs := scanningFuncs(c.pkg, scan)
	for _, file := range c.files {
		name := c.fset.Position(file.Pos()).Filename
		if slices.Contains(allowed, filepath.Base(name)) {
			continue
		}
		checkedFiles++
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			var fn *types.Func
			if s, ok := c.info.Selections[sel]; ok {
				fn, _ = s.Obj().(*types.Func)
			} else {
				fn, _ = c.info.Uses[sel.Sel].(*types.Func)
			}
			if fn == nil {
				return true
			}
			inPkg := fn.Pkg() != nil && slices.Contains(scanningPackages(), fn.Pkg().Path()) && !slices.Contains(copyFree(), fn.FullName())
			if inPkg || slices.Contains(scan, fn.FullName()) || viaInterface(fn, scanFuncs) {
				p := c.fset.Position(sel.Pos())
				uses = append(uses, filepath.Base(p.Filename)+":"+strconv.Itoa(p.Line)+": "+fn.FullName())
			}
			return true
		})
	}
	return uses, checkedFiles
}

// scanningFuncs resolves the scanning methods among the packages pkg
// imports, directly or not.
func scanningFuncs(pkg *types.Package, scan []string) []*types.Func {
	var out []*types.Func
	for _, f := range exportedFuncs(pkg) {
		if slices.Contains(scan, f.FullName()) && f.Signature().Recv() != nil {
			out = append(out, f)
		}
	}
	return out
}

// viaInterface reports a method of an interface type with the name and
// signature of a scanning method, which a tree.Node or schemaidx value
// would satisfy.
func viaInterface(fn *types.Func, scan []*types.Func) bool {
	recv := fn.Signature().Recv()
	if recv == nil || !types.IsInterface(recv.Type()) {
		return false
	}
	for _, s := range scan {
		if s.Name() == fn.Name() && types.Identical(withoutRecv(s.Signature()), withoutRecv(fn.Signature())) {
			return true
		}
	}
	return false
}

// withoutRecv returns sig without its receiver.
func withoutRecv(sig *types.Signature) *types.Signature {
	return types.NewSignatureType(nil, nil, nil, sig.Params(), sig.Results(), sig.Variadic())
}

// exportedFuncs returns the exported package functions and the exported
// methods of the exported named types of the tree, schemaidx and diag
// packages among the imports of pkg, directly or not.
func exportedFuncs(pkg *types.Package) []*types.Func {
	var out []*types.Func
	seen := map[*types.Package]bool{}
	var visit func(p *types.Package)
	visit = func(p *types.Package) {
		if seen[p] {
			return
		}
		seen[p] = true
		for _, q := range p.Imports() {
			visit(q)
		}
		if p.Path() != treePkg && p.Path() != schemaidxPkg && p.Path() != diagPkg {
			return
		}
		for _, name := range p.Scope().Names() {
			switch obj := p.Scope().Lookup(name).(type) {
			case *types.Func:
				if obj.Exported() {
					out = append(out, obj)
				}
			case *types.TypeName:
				named, ok := obj.Type().(*types.Named)
				if !ok || !obj.Exported() {
					continue
				}
				for m := range named.Methods() {
					if m.Exported() {
						out = append(out, m)
					}
				}
			default:
			}
		}
	}
	visit(pkg)
	return out
}

// TestLookupsMetered: outside meteredFiles, no file of the package uses a
// scanning method or function or a function of a scanning package, so
// every such cost is counted as it runs (TestYieldSpread). It reads the
// package's files from disk, or through the overlay GOFLAGS names, so a
// mutation proof runs as GOFLAGS=-overlay=<file> go test.
func TestLookupsMetered(t *testing.T) {
	uses, checkedFiles := directUses(t, ".", meteredFiles())
	for _, u := range uses {
		t.Errorf("%s is used directly; use the counted helpers of meter.go", u)
	}
	// retype.go, subst.go and walk.go.
	if checkedFiles < 3 {
		t.Errorf("checked %d files, want the package's 3 or more", checkedFiles)
	}
}

// TestDirectUsesFound checks the detector of TestLookupsMetered on
// testdata/metered: it finds exactly the uses whose line direct.go marks
// "// found", among them every scanning method and function and a
// function of each scanning package, through any form of use, and none
// of the same names on other types.
func TestDirectUsesFound(t *testing.T) {
	dir := filepath.Join("testdata", "metered")
	uses, checkedFiles := directUses(t, dir, nil)
	if checkedFiles != 1 {
		t.Fatalf("checked %d files, want direct.go only", checkedFiles)
	}
	src, err := os.ReadFile(filepath.Join(dir, "direct.go")) //nolint:gosec // G304: the fixture of this test.
	if err != nil {
		t.Fatal(err)
	}
	var want, got []string
	for i, line := range strings.Split(string(src), "\n") {
		if strings.HasSuffix(line, "// found") {
			want = append(want, "direct.go:"+strconv.Itoa(i+1))
		}
	}
	found := map[string]bool{}
	for _, u := range uses {
		at, name, _ := strings.Cut(u, ": ")
		got = append(got, at)
		found[name] = true
	}
	got = slices.Compact(got)
	if !slices.Equal(got, want) {
		t.Errorf("found uses on lines\n%s\nwant the lines marked found\n%s\nuses:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"), strings.Join(uses, "\n"))
	}
	for _, name := range scanning() {
		if !found[name] {
			t.Errorf("no use of %s found in testdata/metered/direct.go", name)
		}
	}
}

// TestScanningInventory keeps scanning() complete: every exported method
// of tree.Node and every exported tree or schemaidx function or method
// that takes a *tree.Node instance is in it, since each looks at the
// instance, and every name in it exists.
func TestScanningInventory(t *testing.T) {
	c := typeCheck(t, ".")
	scan := scanning()
	var node types.Type
	for _, f := range exportedFuncs(c.pkg) {
		if f.Pkg().Path() == treePkg {
			if obj := f.Pkg().Scope().Lookup("Node"); obj != nil {
				node = types.NewPointer(obj.Type())
			}
		}
	}
	if node == nil {
		t.Fatal("no tree.Node among the imports")
	}
	for _, f := range exportedFuncs(c.pkg) {
		if f.Pkg().Path() == diagPkg {
			continue
		}
		sig := f.Signature()
		takes := sig.Recv() != nil && types.Identical(sig.Recv().Type(), node)
		for v := range sig.Params().Variables() {
			takes = takes || types.Identical(v.Type(), node)
		}
		if takes && !slices.Contains(scan, f.FullName()) {
			t.Errorf("%s takes a *tree.Node but is not in scanning()", f.FullName())
		}
	}
	for _, name := range scan {
		if !slices.ContainsFunc(exportedFuncs(c.pkg), func(f *types.Func) bool { return f.FullName() == name }) {
			t.Errorf("scanning() names %s, which does not exist", name)
		}
	}
}

// countingNames are the calls that count work: the meters and every
// function or method of the package whose body calls one, by name.
func countingNames(files []*ast.File) map[string]bool {
	set := map[string]bool{"tick": true, "charge": true, "read": true, "meter": true}
	for changed := true; changed; {
		changed = false
		for _, f := range files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil || set[fn.Name.Name] {
					continue
				}
				if callsCounting(fn.Body, set) {
					set[fn.Name.Name] = true
					changed = true
				}
			}
		}
	}
	return set
}

// calleeName returns the name a call expression calls: f, x.f, x.f[T].
func calleeName(call *ast.CallExpr) string {
	fun := call.Fun
	if ix, ok := fun.(*ast.IndexExpr); ok {
		fun = ix.X
	}
	if ix, ok := fun.(*ast.IndexListExpr); ok {
		fun = ix.X
	}
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	default:
		return ""
	}
}

// callsCounting reports whether n holds a call of a counting name.
func callsCounting(n ast.Node, set map[string]bool) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && set[calleeName(call)] {
			found = true
		}
		return !found
	})
	return found
}

// unconditional returns the parts of statement s that every iteration of
// the loop holding it at its top level evaluates.
func unconditional(s ast.Stmt) []ast.Node {
	switch s := s.(type) {
	case *ast.IfStmt:
		return []ast.Node{s.Init, s.Cond}
	case *ast.SwitchStmt:
		return []ast.Node{s.Init, s.Tag}
	case *ast.ForStmt, *ast.RangeStmt, *ast.BlockStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
		return nil
	default:
		return []ast.Node{s}
	}
}

// uncountedLoops returns, as "file:line", the loops of the non-test Go
// files of dir that neither count every iteration (a statement at the top
// of the body that every iteration runs calls a counting name) nor range
// over a literal or a constant, nor carry a comment on their line or the
// line before that says why their length is bounded whatever the input
// ("// bounded:") or how they count their iterations in steps ("//
// counted:").
func uncountedLoops(t *testing.T, dir string) (loops []string, total int) {
	t.Helper()
	c := typeCheck(t, dir)
	set := countingNames(c.files)
	for _, f := range c.files {
		// bounded and counted hold the lines of the loops a comment
		// exempts: a bounded loop need not count, a counted one counts in
		// steps, so its body still calls a counting name somewhere.
		bounded, counted := map[int]bool{}, map[int]bool{}
		for _, g := range f.Comments {
			for _, cm := range g.List {
				line := c.fset.Position(cm.Pos()).Line
				switch {
				case strings.HasPrefix(cm.Text, "// bounded:"):
					bounded[line], bounded[line+1] = true, true
				case strings.HasPrefix(cm.Text, "// counted:"):
					counted[line], counted[line+1] = true, true
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			var body *ast.BlockStmt
			switch l := n.(type) {
			case *ast.ForStmt:
				body = l.Body
			case *ast.RangeStmt:
				body = l.Body
				switch x := l.X.(type) {
				case *ast.CompositeLit:
					return true
				case *ast.Ident, *ast.BasicLit:
					if tv, ok := c.info.Types[x]; ok && tv.Value != nil {
						return true
					}
				}
			default:
				return true
			}
			total++
			p := c.fset.Position(n.Pos())
			if bounded[p.Line] || (counted[p.Line] && callsCounting(body, set)) {
				return true
			}
			for _, s := range body.List {
				for _, part := range unconditional(s) {
					if part != nil && callsCounting(part, set) {
						return true
					}
				}
			}
			loops = append(loops, filepath.Base(p.Filename)+":"+strconv.Itoa(p.Line))
			return true
		})
	}
	return loops, total
}

// TestLoopsCount covers the class of the review finding on uncounted work
// (01 req 53): every loop of the package counts each iteration as it runs
// it, or ranges over a literal or a constant, or says why its length is
// bounded whatever the input ("// bounded:") or how it counts in steps
// ("// counted:"), so no pass over the input runs uncounted and none is
// charged before or after it.
func TestLoopsCount(t *testing.T) {
	loops, total := uncountedLoops(t, ".")
	for _, l := range loops {
		t.Errorf("%s: a loop that counts no iteration; count each (tick, read) or say why it is bounded", l)
	}
	if total < 20 {
		t.Errorf("checked %d loops, want the package's", total)
	}
}

// TestUncountedLoopsFound checks the detector of TestLoopsCount on
// testdata/loops: it reports exactly the loops whose line loops.go marks
// "// uncounted".
func TestUncountedLoopsFound(t *testing.T) {
	dir := filepath.Join("testdata", "loops")
	loops, _ := uncountedLoops(t, dir)
	src, err := os.ReadFile(filepath.Join(dir, "loops.go")) //nolint:gosec // G304: the fixture of this test.
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for i, line := range strings.Split(string(src), "\n") {
		if strings.HasSuffix(line, "// uncounted") {
			want = append(want, "loops.go:"+strconv.Itoa(i+1))
		}
	}
	if !slices.Equal(loops, want) {
		t.Errorf("uncounted loops on lines %q, want %q", loops, want)
	}
}
