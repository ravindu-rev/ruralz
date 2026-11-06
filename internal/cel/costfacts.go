// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"slices"
	"strings"

	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
)

// costFacts are what the nominal estimator needs of a checked expression
// beyond its containers: how the expression uses the elements of each dyn
// value reached from a Ruralz variable. cel-go sizes an index result and a
// comprehension variable from their container's element size, which it
// asks of the estimator once per container path (nominalEstimator), so
// the elements of one container share a size: a container where every
// element the expression reads by index or as a comprehension variable is
// itself used as one, else a string.
type costFacts struct {
	// elements maps the element path (elementKey) of every dyn value read
	// by index or as a comprehension variable below a Ruralz variable to
	// whether each such value is used as a container.
	elements map[string]bool
}

// elementStep stands for one step into a container's elements in a walk
// path: cel-go's "@items", "@keys", "@values" and "@indices".
const elementStep = "@"

// pathSeparator joins the steps of an element key: no CEL field name
// holds it.
const pathSeparator = "\x00"

// elementKey returns the key of a path from a variable, cel-go's or a
// walk's, with every element step as elementStep, so cel-go's element
// paths and the walk's agree whatever step name each gives.
func elementKey(path []string) string {
	var b strings.Builder
	for i, p := range path {
		if i > 0 {
			b.WriteString(pathSeparator)
		}
		switch p {
		case "@items", "@keys", "@values", "@indices":
			b.WriteString(elementStep)
		default:
			b.WriteString(p)
		}
	}
	return b.String()
}

// collectCostFacts walks the checked AST a, whose expressions used as a
// list or map are containers (analysis.containers).
func collectCostFacts(a *ast.AST, containers map[int64]bool) *costFacts {
	w := &factWalker{
		a:          a,
		containers: containers,
		facts:      &costFacts{elements: map[string]bool{}},
		paths:      map[int64][]string{},
	}
	w.walk(a.Expr())
	return w.facts
}

// factWalker collects the costFacts of one checked AST. Its paths follow
// cel-go's: from an unshadowed Ruralz variable through field selections,
// indexes and comprehension variables, never through a call result.
type factWalker struct {
	a          *ast.AST
	containers map[int64]bool
	facts      *costFacts
	// paths holds the path of each expression reached from a variable.
	paths map[int64][]string
	// locals are the comprehension variables in scope, innermost last.
	locals []local
}

// local is one comprehension variable in scope.
type local struct {
	name string
	// path is the variable's path; nil when its values are not reached from
	// a Ruralz variable, as for an accumulator.
	path []string
	// dyn reports that cel-go sizes the variable's values as dyn values
	// (dynElements).
	dyn bool
}

// walk visits e and its operands.
func (w *factWalker) walk(e ast.Expr) {
	switch e.Kind() {
	case ast.IdentKind:
		w.ident(e)
	case ast.SelectKind:
		sel := e.AsSelect()
		w.walk(sel.Operand())
		if p, ok := w.paths[sel.Operand().ID()]; ok && !sel.IsTestOnly() {
			w.paths[e.ID()] = appendPath(p, sel.FieldName())
		}
	case ast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() {
			w.walk(c.Target())
		}
		args := c.Args()
		for _, arg := range args {
			w.walk(arg)
		}
		if c.FunctionName() == operators.Index && len(args) == 2 {
			if p, ok := w.paths[args[0].ID()]; ok {
				w.element(e, appendPath(p, elementStep), dynElements(w.a.GetType(args[0].ID()), false))
			}
		}
	case ast.ComprehensionKind:
		w.comprehension(e)
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			w.walk(el)
		}
	case ast.MapKind:
		for _, ent := range e.AsMap().Entries() {
			me := ent.AsMapEntry()
			w.walk(me.Key())
			w.walk(me.Value())
		}
	case ast.StructKind:
		for _, f := range e.AsStruct().Fields() {
			w.walk(f.AsStructField().Value())
		}
	case ast.LiteralKind, ast.UnspecifiedExprKind:
	}
}

// ident gives a Ruralz variable its path and a comprehension variable the
// path of its range's elements, which it reads.
func (w *factWalker) ident(e ast.Expr) {
	name := e.AsIdent()
	for i := len(w.locals) - 1; i >= 0; i-- {
		if l := w.locals[i]; l.name == name {
			if l.path != nil {
				w.element(e, l.path, l.dyn)
			}
			return
		}
	}
	if _, ok := varBit(name); ok {
		w.paths[e.ID()] = []string{name}
	}
}

// element records e, an element of a container at path, and its use when
// cel-go sizes the container's elements as dyn values: e's own checked
// type, which the checker may narrow from its use, does not change the
// size cel-go gives it.
func (w *factWalker) element(e ast.Expr, path []string, dyn bool) {
	w.paths[e.ID()] = path
	if !dyn {
		return
	}
	k := elementKey(path)
	all, seen := w.facts.elements[k]
	w.facts.elements[k] = w.containers[e.ID()] && (all || !seen)
}

// dynElements reports whether cel-go sizes the elements of a container of
// type t as dyn values: those of a dyn value, or of a list or map whose
// element, key or value type is dyn. keys selects the keys of a map, which
// a comprehension variable reads, rather than its values, which an index
// reads.
func dynElements(t *types.Type, keys bool) bool {
	ps := t.Parameters()
	switch t.Kind() {
	case types.DynKind, types.AnyKind, types.TypeParamKind:
		return true
	case types.ListKind:
		return len(ps) == 1 && isDyn(ps[0])
	case types.MapKind:
		if len(ps) != 2 {
			return false
		}
		if keys {
			return isDyn(ps[0])
		}
		return isDyn(ps[1])
	default:
		return false
	}
}

// isDyn reports a dyn type.
func isDyn(t *types.Type) bool {
	switch t.Kind() {
	case types.DynKind, types.AnyKind, types.TypeParamKind:
		return true
	default:
		return false
	}
}

// comprehension scopes the variables: the iteration variables, which read
// the elements of the range, and the accumulator in the loop, the
// accumulator alone in the result.
func (w *factWalker) comprehension(e ast.Expr) {
	comp := e.AsComprehension()
	rng := comp.IterRange()
	w.walk(rng)
	w.walk(comp.AccuInit())
	// A two-variable comprehension, which no enabled macro builds, records
	// nothing: its elements count as strings, the larger nominal size.
	var iter []string
	if p, ok := w.paths[rng.ID()]; ok && !comp.HasIterVar2() {
		iter = appendPath(p, elementStep)
	}
	t := w.a.GetType(rng.ID())
	n := len(w.locals)
	w.locals = append(w.locals, local{name: comp.AccuVar()},
		local{name: comp.IterVar(), path: iter, dyn: dynElements(t, t.Kind() == types.MapKind)})
	if comp.HasIterVar2() {
		w.locals = append(w.locals, local{name: comp.IterVar2()})
	}
	w.walk(comp.LoopCondition())
	w.walk(comp.LoopStep())
	w.locals = w.locals[:n+1]
	w.walk(comp.Result())
	w.locals = w.locals[:n]
}

// appendPath returns p with one more element, never sharing p's array.
func appendPath(p []string, elem string) []string {
	return append(slices.Clip(p), elem)
}
