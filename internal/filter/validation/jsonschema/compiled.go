// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"reflect"
	"slices"
	"strings"

	sjs "github.com/santhosh-tekuri/jsonschema/v6"
)

// The compiled set (audit class GC-21, rows CC-01 to CC-04). The library
// compiles schemas that no exported field of a compiled schema leads to:
// with every schema, the root of the resource it belongs to, and with
// every resource root, eagerly, the schema of each $dynamicAnchor the
// resource holds (compiler.go:216-236). Validation reaches those anchor
// schemas through the dynamic scope alone, wherever a $dynamicRef's first
// target bears the same $dynamicAnchor (validator.go:781-813). The library
// also reads a $schema at every position it collects, the keyword tree of
// the document and of every position it compiles, and gives the resource
// there and the resources inside it that have none the dialect it names
// (roots.go:70-191, 193-235): a dialect with the format-assertion
// vocabulary asserts format, and one without the validation or applicator
// vocabulary drops those keywords. On a route the rule walk did not walk,
// a $recursiveRef target before class GC-01 made the walk follow it, or a
// reference the library based on a resource the walk did not before class
// GC-02 made the walk model the library's collection (walker), the walk's
// $schema rule never ran, and the inspection from the walked positions
// never met the anchor schemas: a request body had format asserted on it,
// or the schema's own type and properties ignored (library audit CC-01 and
// CC-04). The compile therefore lists every schema the library compiled,
// applies the walk's $schema rule to the keyword tree of each position the
// walk did not walk, inspects every compiled schema, the anchor schemas
// included, and then refuses any compiled schema at a position the walk
// did not walk live (unwalked), before the evaluation analysis
// (compileWalked). The walk now walks live every position the library
// compiles, so the $schema rule and the refusal are backstops, which fire
// only if the walk's model and the library part (TestCheckCollected,
// TestUnwalkedBackstop).

// msgDialect is the finding of the $schema rule (07 req 75), at the walk
// and at the compile alike.
const msgDialect = "$schema must be " + Draft

// msgUnlisted is the finding when the compile cannot list the schemas the
// library compiled: it then checks none of them and refuses the document
// (fail closed).
const msgUnlisted = "the compile cannot list the schemas the library compiled, so it checks none of them (an internal error)"

// msgUnwalked is the finding of the walked-position backstop (unwalked).
const msgUnwalked = "the library compiles this schema position on a route the compile's rule walk does not follow, so the walk's rules never applied to it (move the schema a reference names here under $defs)"

// badDialect reports whether the schema object obj has a $schema that is
// not exactly Draft (07 req 75).
func badDialect(obj map[string]any) bool {
	v, ok := obj["$schema"]
	if !ok {
		return false
	}
	s, isStr := v.(string)
	return !isStr || s != Draft
}

// compiledSchema is one schema the library compiled from the document: its
// pointer, and the address of its compiled form, by which the compile
// tells it from the schemas the inspection already reached.
type compiledSchema struct {
	ptr  string
	addr uintptr
}

// compiledSchemas lists every schema c has compiled from the document, in
// pointer order; ok is false when c's table of compiled schemas cannot be
// read.
//
// The library exports no list of what it compiled, so this reads the
// compiler's table, the unexported field schemas (a map from location to
// *Schema, compiler.go:11 and :210-212), through reflect, without unsafe
// and without writing: only each entry's address and its exported Location
// are read. The module is pinned at v6.0.3 (go.mod); TestCompiledSchemas
// pins the table's shape and content, so a library change that moves it
// fails there, and the compile fails closed (msgUnlisted) rather than
// check fewer schemas. Schemas of other URLs, the decision and check
// wrappers and any bundled meta-schema a reference reached, are left out:
// a reference to a meta-schema is refused at its member by the inspection
// of the compiled schema that holds it.
func compiledSchemas(c *sjs.Compiler) ([]compiledSchema, bool) {
	return compiledIn(reflect.ValueOf(c).Elem())
}

// compiledIn reads the table of compiled schemas of the compiler value c,
// as compiledSchemas does.
func compiledIn(c reflect.Value) ([]compiledSchema, bool) {
	if c.Kind() != reflect.Struct {
		return nil, false
	}
	m := c.FieldByName("schemas")
	if m.Kind() != reflect.Map || m.Type().Elem() != reflect.TypeFor[*sjs.Schema]() {
		return nil, false
	}
	out := make([]compiledSchema, 0, m.Len())
	for it := m.MapRange(); it.Next(); {
		s := it.Value()
		if s.IsNil() {
			return nil, false
		}
		if ptr, ok := pointerOf(s.Elem().FieldByName("Location").String()); ok {
			out = append(out, compiledSchema{ptr: ptr, addr: s.Pointer()})
		}
	}
	// Non-canonical indexes (/allOf/00 beside /allOf/0) compare equal as
	// pointers; their bytes order them.
	slices.SortFunc(out, func(a, b compiledSchema) int {
		if d := comparePointers(a.ptr, b.ptr); d != 0 {
			return d
		}
		return strings.Compare(a.ptr, b.ptr)
	})
	return out, true
}

// checkCollected applies the walk's $schema rule (walker.collect) to every
// schema position the library collected from a compiled position the walk
// did not collect: the position and its keyword tree, the positions whose
// $schema the library reads (roots.go:70-191). A position the walk
// collected had its keyword tree checked by the walk, and so had the
// document's own keyword tree, which the library collects first. The walk
// collects every position the library does, so this is a backstop. The
// findings come in pointer order, at most MaxIssues of them, and are the
// walk's own. Each object of the document is walked once, and a pointer is
// built only for a finding.
func (w *walker) checkCollected(compiled []compiledSchema) []Issue {
	var out []Issue
	done := map[uintptr]bool{}
	// segs holds the relative pointers from the start down to the object
	// being walked.
	var segs []string
	var walk func(v any, start string)
	walk = func(v any, start string) {
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		id := reflect.ValueOf(obj).Pointer()
		if done[id] {
			return
		}
		done[id] = true
		if badDialect(obj) {
			p := start + strings.Join(segs, "")
			out = append(out, Issue{Pointer: appendToken(p, "$schema"), Message: msgDialect})
		}
		eachSubschema(obj, "", func(sv any, at string) {
			segs = append(segs, at)
			walk(sv, start)
			segs = segs[:len(segs)-1]
		})
	}
	for _, cs := range compiled {
		if w.seen[cs.ptr] != 0 {
			continue
		}
		if v, ok := lookup(w.doc, cs.ptr); ok {
			walk(v, cs.ptr)
		}
	}
	slices.SortFunc(out, func(a, b Issue) int { return comparePointers(a.Pointer, b.Pointer) })
	return out[:min(len(out), MaxIssues)]
}

// unwalked refuses every schema of compiled at a position the walk did
// not walk live, the class backstop of library audit classes GC-01 and
// GC-02: the walk's rules (patterns, number bounds, the lists of names,
// MaxSubschemas, the meta-schema check bounds) and the compile's own
// meta-schema check of what no check of the library covers
// (uncheckedPositions) hold only at the positions the walk walks live, so
// a schema the library compiled anywhere else escaped them all, and the
// compiled Schema would keep it and validate with it (a multipleOf of 0
// there panicked at every number, an unbounded literal was parsed at
// every comparison). The walk walks live every position the library
// compiles (walker), so this never fires but if the two part. The findings
// come in pointer order, at the top-most such positions only, at most
// MaxIssues of them.
func (w *walker) unwalked(compiled []compiledSchema) []Issue {
	var out []Issue
	for _, cs := range compiled {
		if w.seen[cs.ptr] == walkedLive {
			continue
		}
		below := slices.ContainsFunc(out, func(is Issue) bool { return strings.HasPrefix(cs.ptr, is.Pointer+"/") })
		if below {
			continue
		}
		if len(out) == MaxIssues {
			break
		}
		out = append(out, Issue{Pointer: cs.ptr, Message: msgUnwalked})
	}
	return out
}

// visitCompiled inspects every schema of compiled that the inspection has
// not reached from the walked positions, in pointer order, and returns
// them, the starts the evaluation analysis adds: the library's own Compile
// returns each from its table, compiling nothing (compiler.go:153-157,
// 190-214). ok is false when one is not the schema the table holds, so the
// list did not come from the table.
func (in *inspector) visitCompiled(c *sjs.Compiler, compiled []compiledSchema) (extra []*sjs.Schema, ok bool) {
	known := make(map[uintptr]bool, len(in.schemas))
	mark := func(from int) int {
		for _, s := range in.schemas[from:] {
			known[reflect.ValueOf(s).Pointer()] = true
		}
		return len(in.schemas)
	}
	n := mark(0)
	for _, cs := range compiled {
		if known[cs.addr] {
			continue
		}
		s, err := c.Compile(baseURL + "#" + encodeFragment(cs.ptr))
		if err != nil || reflect.ValueOf(s).Pointer() != cs.addr {
			return nil, false
		}
		in.visit(s, "")
		n = mark(n)
		extra = append(extra, s)
	}
	return extra, true
}

// inspectCompiled checks what the library compiled from the document:
// compiled lists it (listed is false when the listing failed, and the
// document is refused), starts are the root and the walked positions. It
// applies the walk's $schema rule where the library read a $schema the
// walk did not (checkCollected), and only then, as the walk's findings
// would end the compile first, inspects every compiled schema: the starts,
// what they reach, and the rest of compiled (visitCompiled), whose fan-out
// it bounds; last it refuses every compiled schema at a position the walk
// did not walk live (unwalked), so the analysis never meets one. It
// returns the inspection and the starts of the evaluation analysis, every
// compiled schema the starts do not reach appended.
func inspectCompiled(c *sjs.Compiler, w *walker, starts []*sjs.Schema, compiled []compiledSchema, listed bool) (*inspector, []*sjs.Schema, []Issue) {
	if !listed {
		return nil, nil, []Issue{{Message: msgUnlisted}}
	}
	if issues := w.checkCollected(compiled); len(issues) > 0 {
		return nil, nil, issues
	}
	in := newInspector()
	for _, s := range starts {
		in.visit(s, "")
	}
	extra, ok := in.visitCompiled(c, compiled)
	if !ok {
		return nil, nil, []Issue{{Message: msgUnlisted}}
	}
	in.fanOut()
	if len(in.issues) > 0 {
		return nil, nil, in.issues
	}
	if issues := w.unwalked(compiled); len(issues) > 0 {
		return nil, nil, issues
	}
	return in, append(starts, extra...), nil
}
