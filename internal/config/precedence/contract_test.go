// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// sourceFile is one parsed file of the package.
type sourceFile struct {
	path string
	*ast.File
}

// packageFiles parses the package's Go files with comments: the non-test
// files, and the test files too when tests is set.
func packageFiles(t *testing.T, tests bool) []sourceFile {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []sourceFile
	for _, p := range paths {
		if !tests && strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, sourceFile{path: p, File: f})
	}
	if len(out) == 0 {
		t.Fatal("no package files")
	}
	return out
}

// TestImportsMatchArchitectureRow pins the package's imports to its row
// in architecture section 1.2 (WP-38 S2 finding, applied as R-78): the
// standard library plus config/hub, config/registry, config/diag,
// config/tree, phase, expr and v1alpha1, and no third-party module.
func TestImportsMatchArchitectureRow(t *testing.T) {
	const module = "github.com/ravindu-rev/ruralz/"
	allowed := []string{
		"internal/config/hub", "internal/config/registry", "internal/config/diag", "internal/config/tree",
		"internal/phase", "internal/expr", "pkg/config/v1alpha1",
	}
	for _, f := range packageFiles(t, false) {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case strings.HasPrefix(path, module):
				if !slices.Contains(allowed, strings.TrimPrefix(path, module)) {
					t.Errorf("%s imports %s, which the 1.2 row of config/precedence does not list", f.path, path)
				}
			case strings.Contains(strings.Split(path, "/")[0], "."):
				t.Errorf("%s imports third-party module %s; the 1.2 row lists none", f.path, path)
			default:
			}
		}
	}
}

// TestNoWallClock covers the WP-38 S2 finding on TestScale and its class:
// no file of the package, tests included, reads the wall clock or waits on
// it (architecture section 0 item 4: time.Now only in clock.Real; tests
// never depend on wall-clock time). Costs are measured by benchmarks and
// growth by counters (TestScaleLinear).
func TestNoWallClock(t *testing.T) {
	banned := []string{"Now", "Since", "Until", "After", "AfterFunc", "Sleep", "Tick", "NewTimer", "NewTicker"}
	for _, f := range packageFiles(t, true) {
		local := ""
		for _, imp := range f.Imports {
			if imp.Path.Value == `"time"` {
				local = "time"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" {
			continue
		}
		ast.Inspect(f.File, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == local && slices.Contains(banned, sel.Sel.Name) {
					t.Errorf("%s calls time.%s", f.path, sel.Sel.Name)
				}
			}
			return true
		})
	}
}

// docOf returns the doc comment of the field or method name of the type
// recv (recv "" for a function or a type), with every run of white space
// one space.
func docOf(t *testing.T, files []sourceFile, recv, name string) string {
	t.Helper()
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.Name != name {
					continue
				}
				r := ""
				if d.Recv != nil && len(d.Recv.List) == 1 {
					if star, ok := d.Recv.List[0].Type.(*ast.StarExpr); ok {
						if id, ok := star.X.(*ast.Ident); ok {
							r = id.Name
						}
					}
				}
				if r == recv {
					return strings.Join(strings.Fields(d.Doc.Text()), " ")
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if ok && recv == "" && ts.Name.Name == name {
						return strings.Join(strings.Fields(d.Doc.Text()+ts.Doc.Text()), " ")
					}
					if !ok || ts.Name.Name != recv {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					for _, fld := range st.Fields.List {
						if slices.ContainsFunc(fld.Names, func(n *ast.Ident) bool { return n.Name == name }) {
							return strings.Join(strings.Fields(fld.Doc.Text()), " ")
						}
					}
				}
			default:
			}
		}
	}
	t.Fatalf("no declaration %s.%s", recv, name)
	return ""
}

// TestConcurrencyContractDocumented covers the WP-38 S2 finding that
// Options.Body is called from worker goroutines without saying so, and
// its class: every dependency Resolve shares between workers states what
// it must allow. Body and the FileTable are called concurrently, reg is
// shared by every call, and b is read concurrently. It also pins the S2
// round-2 minors on the same docs: a Resolver keeps no load's FileTable
// (one Resolver per process, spec 02 section 3), ctx is observed only
// between work units with an in-flight Body call waited for, and a panic
// in a work unit surfaces the same way for every worker count; and the
// round-3 findings: the diagnostic cap and what workers hold, the yield
// clock, what a panic stops (and that its unit's deferred calls may
// overlap other units), the per-Route cost of Phase content no other
// Route shares, and resources without a typed view; and the round-4
// findings: what the cap bounds and the size of one finding, the slot
// read once, who supplies the clock and where its panic surfaces, the
// zero Resolver, and printable Rows; and the S2 round-2 findings: one
// collector bounding the findings of all workers together, a
// runtime.Goexit in a unit raised like a panic, and references too long
// to resolve skipped unread.
func TestConcurrencyContractDocumented(t *testing.T) {
	files := packageFiles(t, false)
	for _, tc := range []struct {
		recv, name, want string
	}{
		{"Options", "Body", "Body must be safe for concurrent use: Resolve calls it from up to workers goroutines."},
		{"Options", "Body", "Body takes no context: Resolve observes the end of its ctx only between work units, so a Body call in flight when ctx ends always runs to completion and Resolve waits for it before it returns."},
		{"", "New", "Every call shares reg, which a registry.Registry allows: it is immutable and safe for concurrent use."},
		{"", "Resolver", "it keeps nothing of one call for the next: the tree.FileTable of a load is an argument of each call, so one Resolver serves every load of a process."},
		{"Resolver", "Resolve", "Those goroutines read b, call Options.Body and read files concurrently; b must not be modified until Resolve returns."},
		{"Resolver", "Resolve", "A FileTable indexes one load's files only, so each call passes its own; the Resolver keeps none."},
		{"Resolver", "Resolve", "ctx is checked between work units: once it ends, no further unit starts, every unit already started (an Options.Body call in flight included) runs to completion, and Resolve returns ctx.Err() after all of them have."},
		{"Resolver", "Resolve", "it is raised again in the calling goroutine, with the same value and the work unit's stack whatever the worker count"},
		{"Resolver", "Resolve", "Chains share slices and must be treated as read-only"},
		{"Options", "MaxDiagnostics", "The call keeps them in one collector that all its workers share, which holds at most MaxDiagnostics+1 at any time"},
		{"Options", "MaxDiagnostics", "stage I never appends RZ-CFG-001 itself"},
		{"Options", "Now", "Workers call it concurrently. Nil never yields and reads no clock."},
		{"Resolver", "Resolve", "The same findings come back for every worker count, and the call holds fewer than 2(MaxDiagnostics+1)+workers of them at any time, all workers together"},
		{"Resolver", "Resolve", "Once a work unit has failed, by a panic (in Options.Body, say) or by a runtime.Goexit, which ends its goroutine without a panic to recover, no further unit starts and dispatch ends"},
		{"Resolver", "Resolve", "as may the failing unit's own deferred calls, which run before the failure is recorded"},
		{"Resolver", "Resolve", "so a caller's recover sees one behavior for every worker count and no unit is ever left out of a result"},
		{"Resolver", "Resolve", "longer than 63 bytes, the longest metadata.name stage F accepts, is never hashed or compared"},
		{"Resolver", "Resolve", "A reference that resolves to no Policy takes no memory beyond its list entry."},
		{"Resolver", "Resolve", "every loop of a unit counts its elements as it reads them"},
		{"Resolver", "Resolve", "O(Gateway Policies) per Route"},
		{"Resolver", "Resolve", "is treated as absent, like an unresolved reference"},
		{"Options", "MaxDiagnostics", "That bounds the number of findings a run holds at once by fewer than 2(MaxDiagnostics+1)+workers, whatever the input and the worker count"},
		{"Options", "MaxDiagnostics", "every worker rejects a finding that sorts after its last kept one on location and code before building it"},
		{"Options", "Body", "a panic in it, or a runtime.Goexit (t.FailNow in a test oracle, say), ends that unit and reaches the caller as a work unit's failure"},
		{"Options", "Now", "A runtime.Goexit in it is a unit's failure the same way when a worker read the clock for a unit"},
		{"Options", "MaxDiagnostics", "at most 64 code points of a slot, whose length no schema rule bounds"},
		{"Options", "Now", "the pipeline passes the Now its caller supplies (clock.Real().Now in every binary)"},
		{"Options", "Now", "a panic in it reaches the caller in the calling goroutine for every worker count"},
		{"", "Resolver", "The zero value, like a nil *Resolver, behaves as New(nil, Options{}) returns"},
		{"Resolver", "Resolve", "A Policy's spec.slot is read once per attachment scope"},
		{"Resolver", "Rows", "Every field of a Row is printable text"},
	} {
		if doc := docOf(t, files, tc.recv, tc.name); !strings.Contains(doc, tc.want) {
			t.Errorf("doc of %s.%s lacks %q:\n%s", tc.recv, tc.name, tc.want, doc)
		}
	}
}

// rendezvousOracle blocks each ReferencesRequestBody call until two calls
// are in flight at once.
type rendezvousOracle struct {
	arrived atomic.Int32
	both    chan struct{}
}

func (o *rendezvousOracle) ReferencesRequestBody(string) (bool, error) {
	if o.arrived.Add(1) == 2 {
		close(o.both)
	}
	<-o.both
	return false, nil
}

// TestBodyOracleCalledConcurrently pins the behavior the Options.Body doc
// states: with two workers and two authz.cel attachments, both oracle
// calls are in flight at once, so an oracle that is not safe for
// concurrent use would race. Each call blocks until the other arrives,
// which completes whatever the scheduling (the blocked worker cannot take
// the second unit, so the other worker does). A Resolve that serialized
// the calls would deadlock here, and go test -timeout reports it with
// every stack.
func TestBodyOracleCalledConcurrently(t *testing.T) {
	b := newBuilder()
	b.policy("a", v1alpha1.PolicyTypeAuthzCEL, authzCfg("auth.a"), slot("a"))
	b.policy("b", v1alpha1.PolicyTypeAuthzCEL, authzCfg("auth.b"), slot("b"))
	b.gateway("a", "b")
	o := &rendezvousOracle{both: make(chan struct{})}
	if _, ds, err := mustNew(t, nil, Options{Body: o}).Run(t.Context(), b.bundle(), nil, 2); err != nil || len(ds) != 0 {
		t.Fatalf("Run: %v %v", err, ds)
	}
	if n := o.arrived.Load(); n != 2 {
		t.Errorf("%d oracle calls, want 2", n)
	}
}

// blockingOracle blocks every ReferencesRequestBody call until release
// closes, counting the calls started and those still in flight.
type blockingOracle struct {
	started, inflight atomic.Int32
	release           chan struct{}
}

func (o *blockingOracle) ReferencesRequestBody(string) (bool, error) {
	o.started.Add(1)
	o.inflight.Add(1)
	defer o.inflight.Add(-1)
	<-o.release
	return false, nil
}

// TestResolveCancelWaitsForBody pins the cancellation contract the
// Resolve and Options.Body docs state (WP-38 S2 round-2 minor): ctx is
// observed between work units, so canceling it while Body calls are in
// flight does not end Resolve until those calls return, and Resolve then
// returns ctx.Err(). The calls are held for a long run of yields after
// the cancel; a Resolve that returned without waiting would deliver its
// result during them.
func TestResolveCancelWaitsForBody(t *testing.T) {
	for _, tc := range []struct {
		workers, started int32
	}{{1, 1}, {4, 2}} {
		t.Run(fmt.Sprintf("workers=%d", tc.workers), func(t *testing.T) {
			b := newBuilder()
			b.policy("a1", v1alpha1.PolicyTypeAuthzCEL, authzCfg("x"))
			b.policy("a2", v1alpha1.PolicyTypeAuthzCEL, authzCfg("y"))
			b.gateway("a1")
			b.route("r", []string{"a2"}, nil, nil)
			o := &blockingOracle{release: make(chan struct{})}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type result struct {
				err      error
				inflight int32
			}
			done := make(chan result, 1)
			go func() {
				_, _, err := mustNew(t, nil, Options{Body: o}).Resolve(ctx, b.bundle(), nil, int(tc.workers))
				done <- result{err: err, inflight: o.inflight.Load()}
			}()
			for o.started.Load() < tc.started {
				runtime.Gosched()
			}
			cancel()
			early := false
			for range 20000 {
				select {
				case <-done:
					early = true
				default:
				}
				if early {
					break
				}
				runtime.Gosched()
			}
			close(o.release)
			if early {
				t.Fatal("Resolve returned while a Body call was in flight")
			}
			r := <-done
			if !errors.Is(r.err, context.Canceled) || r.inflight != 0 {
				t.Errorf("Resolve = %v with %d Body calls in flight, want context.Canceled and none", r.err, r.inflight)
			}
			if got := o.started.Load(); got != tc.started {
				t.Errorf("%d Body calls started, want %d: a unit started after the cancel", got, tc.started)
			}
		})
	}
}
