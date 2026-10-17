// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

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
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

const (
	treePkg      = "github.com/ravindu-rev/ruralz/internal/config/tree"
	schemaidxPkg = "github.com/ravindu-rev/ruralz/internal/config/schemaidx"
)

// scanning lists, by types.Func.FullName, the tree and schemaidx methods
// and functions whose cost grows with the instance value they look at:
// stage G calls them only through the counted methods of lookup.go.
// TestScanningInventory keeps the list complete.
func scanning() []string {
	var out []string
	for _, m := range []string{"Get", "Set", "Delete", "At", "Clone", "JSONValue"} {
		out = append(out, "(*"+treePkg+".Node)."+m)
	}
	for _, m := range []string{"Select", "SelectValue"} {
		out = append(out, "(*"+schemaidxPkg+".Node)."+m)
	}
	for _, m := range []string{"Lookup", "SchemaPath", "Walk"} {
		out = append(out, "(*"+schemaidxPkg+".Index)."+m)
	}
	return append(out, schemaidxPkg+".ItemElem", schemaidxPkg+".AppendItemJSON")
}

// checked is the type-checked non-test Go code of one directory.
type checked struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
	pkg   *types.Package
}

// goflagsOverlay returns the file replacements of the -overlay file that
// GOFLAGS names, by absolute path, or nil. go test applies GOFLAGS to the
// build of the test binary, and the go list typeCheck runs inherits it,
// so with GOFLAGS=-overlay=<file> the detector checks the replaced
// sources, which a mutation proof of TestLookupsCounted needs (WP-37
// review round 3). An -overlay flag on the go test command line reaches
// neither this process nor its go list, so the detector then reads the
// files on disk: prove it through GOFLAGS, with absolute paths as keys.
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
// builds (or finds in the build cache), so a selector is judged by the
// type it selects from, not by its spelling. A GOFLAGS overlay
// (goflagsOverlay) replaces, removes or adds files of dir as it does for
// the build.
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
		f, err := goparser.ParseFile(c.fset, name, src, 0)
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

// directLookups returns the uses of a scanning method or function (see
// scanning) in the non-test Go files of dir other than lookup.go, as
// "file:line: use", and the number of files checked. A use is any
// selector that resolves to one, by the type of what it selects from: a
// call, a method value or a function value taken for a later call, a
// method expression, a method promoted from an embedded field, and a
// method of an interface type that a scanning method satisfies, through
// which a tree.Node or schemaidx value could be called.
func directLookups(t *testing.T, dir string) (uses []string, checkedFiles int) {
	t.Helper()
	c := typeCheck(t, dir)
	scan := scanning()
	scanFuncs := scanningFuncs(t, c.pkg, scan)
	for _, file := range c.files {
		name := c.fset.Position(file.Pos()).Filename
		if filepath.Base(name) == "lookup.go" {
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
			if slices.Contains(scan, fn.FullName()) || viaInterface(fn, scanFuncs) {
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
func scanningFuncs(t *testing.T, pkg *types.Package, scan []string) []*types.Func {
	t.Helper()
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
// methods of the exported named types of the tree and schemaidx packages
// among the imports of pkg, directly or not.
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
		if p.Path() != treePkg && p.Path() != schemaidxPkg {
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

// TestLookupsCounted keeps the work counter complete (TestScaleLinear;
// 01 req 53): outside lookup.go, no file of stage G uses a scanning
// tree.Node or schemaidx method or function directly, so a per-member
// scan always shows in work.total. It reads the package's files from
// disk, or through the overlay GOFLAGS names (goflagsOverlay), so a
// mutation proof runs as GOFLAGS=-overlay=<file> go test.
func TestLookupsCounted(t *testing.T) {
	uses, checkedFiles := directLookups(t, ".")
	for _, u := range uses {
		t.Errorf("%s is used directly; use the counted lookups of lookup.go", u)
	}
	// chunk, decode, defaults, keep, materialize, normalize, scalar, work.
	if checkedFiles < 8 {
		t.Errorf("checked %d files, want the package's 8 or more", checkedFiles)
	}
}

// TestDirectLookupsFound checks the detector of TestLookupsCounted on
// testdata/lookups (WP-37 review: the detector matched receiver names, so
// a *tree.Node named v or a method value passed): it finds exactly the
// uses whose line direct.go marks "// found", among them every scanning
// method and function, calls through any variable name, method values,
// function values, method expressions, promoted methods and interface
// methods, and none of the same names on other types, nor any use in
// lookup.go or a test file.
func TestDirectLookupsFound(t *testing.T) {
	dir := filepath.Join("testdata", "lookups")
	uses, checkedFiles := directLookups(t, dir)
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
			t.Errorf("no use of %s found in testdata/lookups/direct.go", name)
		}
	}
}

// TestScanningInventory keeps scanning() complete: every exported method
// of tree.Node and every exported tree or schemaidx function or method
// that takes a *tree.Node instance is in it, since each looks at the
// instance (none today costs the same for every instance), and every
// name in it exists.
func TestScanningInventory(t *testing.T) {
	c := typeCheck(t, ".")
	scan := scanning()
	node := types.NewPointer(lookupType(t, c.pkg, treePkg, "Node"))
	for _, f := range exportedFuncs(c.pkg) {
		sig := f.Signature()
		takes := sig.Recv() != nil && types.Identical(sig.Recv().Type(), node)
		for v := range sig.Params().Variables() {
			takes = takes || types.Identical(v.Type(), node)
		}
		if !takes {
			continue
		}
		if !slices.Contains(scan, f.FullName()) {
			t.Errorf("%s takes a *tree.Node but is not in scanning()", f.FullName())
		}
	}
	for _, name := range scan {
		if !slices.ContainsFunc(exportedFuncs(c.pkg), func(f *types.Func) bool { return f.FullName() == name }) {
			t.Errorf("scanning() names %s, which does not exist", name)
		}
	}
}

// lookupType returns the named type name of the imported package path.
func lookupType(t *testing.T, pkg *types.Package, path, name string) types.Type {
	t.Helper()
	for _, f := range exportedFuncs(pkg) {
		if f.Pkg().Path() == path {
			if obj := f.Pkg().Scope().Lookup(name); obj != nil {
				return obj.Type()
			}
		}
	}
	t.Fatalf("no type %s.%s among the imports", path, name)
	return nil
}

// TestLookupCharges covers the counted lookups: each returns what the
// method it wraps returns and counts what that scans, and a nil worker
// counts nothing.
func TestLookupCharges(t *testing.T) {
	idx := hubIndex(t)
	spec, ok := idx.Spec("Route")
	if !ok {
		t.Fatal("no Route spec schema")
	}
	upstreams, _ := spec.Property("upstreams")
	hosts := memberSchema(mustSelect(t, idx, "Route", "match"), "hosts")
	obj := mustParse(t, `{"a": 1, "b": {"c": 2}, "d": [3]}`)
	entry := mustParse(t, `{"name": "u", "weight": 2}`)
	elem := mustParse(t, `{"k": [1, 2]}`)
	doc := mustParse(t, `{"b": {"c": 2}, "l": [{"name": "x"}, {"name": "y"}], "s": ["p", "q"], "o": [{"k": 1}]}`)
	tests := []struct {
		name string
		run  func(w *work) any
		want any
		cost int
	}{
		// get: the members, plus the key's bytes for each member name
		// of its length (a, b and d are all one byte long).
		{"get present", func(w *work) any { v, _ := w.get(obj, "b"); return dump(v) }, `{"c":2}`, 3 + 3},
		{"get absent", func(w *work) any { _, ok := w.get(obj, "z"); return ok }, false, 3 + 3},
		{"get long key", func(w *work) any { _, ok := w.get(obj, "bb"); return ok }, false, 3},
		{"get nil", func(w *work) any { _, ok := w.get(nil, "a"); return ok }, false, 0},
		{"get scalar", func(w *work) any { _, ok := w.get(mustParse(t, `1`), "a"); return ok }, false, 0},
		{"select", func(w *work) any { return w.selectSchema(spec, obj) == spec.Select(obj) }, true, 3},
		{"select nil", func(w *work) any { return w.selectSchema(spec, nil) == spec }, true, 0},
		{"keyed item", func(w *work) any { return diag.Path{w.itemElem(upstreams, entry, 4)}.String() }, "[name=u]", 2},
		{"set item", func(w *work) any { return diag.Path{w.itemElem(hosts, mustParse(t, `"a.example"`), 0)}.String() }, `[item=a.example]`, 0},
		{"set object item", func(w *work) any { return w.itemElem(hosts, elem, 0).Item }, json.RawMessage(`{"k":[1,2]}`), 1 + len(`{"k":[1,2]}`)},
		{"atomic item", func(w *work) any { return w.itemElem(nil, elem, 7) }, diag.Index(7), 1},
		{"nil item", func(w *work) any { return w.itemElem(upstreams, nil, 2) }, diag.Index(2), 0},
		{"item json", func(w *work) any { return string(w.itemJSON(elem)) }, `{"k":[1,2]}`, len(`{"k":[1,2]}`)},
		// finder: one unit per step, plus what each step scans or indexes
		// (the four-member doc is scanned as get does, 4 + 4; a keyed list
		// and a set are indexed on first use, with the bytes of the keys
		// and texts hashed, and the looked-up key or text).
		{"find member", func(w *work) any { return posText(w.newFinder().at(doc, diag.Path{diag.Field("b"), diag.Field("c")})) }, "1:13", 1 + 8 + 1 + 2},
		{"find keyed", func(w *work) any {
			return posText(w.newFinder().at(doc, diag.Path{diag.Field("l"), diag.Keyed("name", "y")}))
		}, "1:38", 1 + 8 + 1 + 2*(1+4) + 2*1 + 1},
		{"find index", func(w *work) any { return posText(w.newFinder().at(doc, diag.Path{diag.Field("l"), diag.Index(1)})) }, "1:38", 1 + 8 + 1},
		{"find item", func(w *work) any { return posText(w.newFinder().at(doc, diag.Path{diag.Field("s"), diag.Item("q")})) }, "1:65", 1 + 8 + 1 + 2 + 2*1 + 1},
		{"find object item", func(w *work) any {
			return posText(w.newFinder().at(doc, diag.Path{diag.Field("o"), diag.Item(json.RawMessage(`{"k":1}`))}))
		}, "1:77", 1 + 8 + 1 + 3*len(`{"k":1}`)},
		{"find absent", func(w *work) any { return posText(w.newFinder().at(doc, diag.Path{diag.Field("z")})) }, "none", 1 + 8},
		{"find empty", func(w *work) any { return posText(w.newFinder().at(doc, nil)) }, "none", 0},
		{"find nil", func(w *work) any { return posText(w.newFinder().at(nil, diag.Path{diag.Field("a")})) }, "none", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := newWork()
			got := tc.run(w)
			if !equalAny(got, tc.want) {
				t.Errorf("result = %#v, want %#v", got, tc.want)
			}
			if w.total != tc.cost {
				t.Errorf("counted %d units, want %d", w.total, tc.cost)
			}
			// A nil worker gives the same result and counts nothing.
			if got := tc.run(nil); !equalAny(got, tc.want) {
				t.Errorf("nil worker: result = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// posText renders a position found by finder.at as line:column, or none.
func posText(p tree.Pos, ok bool) string {
	if !ok {
		return "none"
	}
	return strconv.Itoa(int(p.Line)) + ":" + strconv.Itoa(int(p.Column))
}

// equalAny compares lookup results, path elements by their text.
func equalAny(a, b any) bool {
	if ea, ok := a.(diag.PathElem); ok {
		eb, ok := b.(diag.PathElem)
		return ok && diag.Path{ea}.String() == diag.Path{eb}.String()
	}
	if ra, ok := a.(json.RawMessage); ok {
		rb, ok := b.(json.RawMessage)
		return ok && string(ra) == string(rb)
	}
	return a == b
}

// mustSelect returns the schema of member name of kind's spec.
func mustSelect(t *testing.T, idx *schemaidx.Index, kind, name string) *schemaidx.Node {
	t.Helper()
	spec, ok := idx.Spec(kind)
	if !ok {
		t.Fatalf("no %s spec schema", kind)
	}
	m, ok := spec.Property(name)
	if !ok {
		t.Fatalf("no %s spec.%s schema", kind, name)
	}
	return m
}

// TestChargeYields covers work.charge (01 req 53): k units count as k
// ticks, so a charge that reaches yieldCheck reads the timer once, and a
// charge of nothing, or on a nil worker, does nothing. The clock is a
// fake (Options.Now) that moves past YieldAfter at every read.
func TestChargeYields(t *testing.T) {
	c := &fakeClock{t: time.Unix(0, 0)}
	w := stageWith(t, Options{Now: func() time.Time { c.advance(time.Second); return c.now() }, YieldAfter: time.Second}).newWork()
	start := c.count()
	w.charge(0)
	w.charge(-3)
	if w.total != 0 || w.units != 0 || c.count() != start {
		t.Fatalf("empty charges counted: total %d units %d reads %d", w.total, w.units, c.count()-start)
	}
	w.charge(yieldCheck - 1)
	if c.count() != start || w.units != yieldCheck-1 {
		t.Fatalf("timer read below yieldCheck units: reads %d units %d", c.count()-start, w.units)
	}
	w.charge(1)
	if w.yields != 1 || w.units != 0 {
		t.Fatalf("no yield at yieldCheck units: yields %d units %d", w.yields, w.units)
	}
	w.charge(10 * yieldCheck)
	if w.yields != 2 || w.units != 0 || w.total != 11*yieldCheck {
		t.Fatalf("large charge: yields %d units %d total %d; want one more yield", w.yields, w.units, w.total)
	}
	var none *work
	none.charge(yieldCheck)
	none.piece(5, 7)
	n := &tree.Node{Kind: tree.KindMap}
	if _, ok := none.get(n, "a"); ok {
		t.Error("nil worker found a member of an empty object")
	}
}
