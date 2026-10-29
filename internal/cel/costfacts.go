// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"math/bits"
	"slices"

	"cel.dev/cel-go/checker"
	"cel.dev/cel-go/common"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/overloads"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/traits"
)

// costFacts are what the cost bounds of docs/architecture/02-configuration-model.md
// "Limits" need of a checked expression beyond cel-go's estimate: the
// values reached from a Ruralz variable, the only values an input cap
// bounds, and the field selections cel-go's estimator does not charge.
//
// cel-go's runtime charges one unit for every field selection, but its
// estimator charges none for selecting a field of a dyn value, such as a
// JSON body or the claims (checker's costSelect charges map, object and
// type-parameter operands only), so its estimate falls one unit short for
// each such selection evaluated. bound adds them back, so both the static
// estimate and the capped bound are upper bounds of the runtime cost.
type costFacts struct {
	// paths maps the ID of each expression whose value is reached from an
	// unshadowed Ruralz variable through field selections, indexing and
	// comprehension variables to its path, as checker.AstNode.Path builds
	// it: the variable, then field names, "@items" for a list entry,
	// "@keys" for a comprehension variable over a map or dyn value and
	// "@values" for an indexed map value. A selection of a field of any
	// other value, such as a call result, has no path here, even where
	// cel-go builds a partial one that starts with a variable's name.
	paths map[int64][]string
	// ranges maps each comprehension's ID to its range.
	ranges map[int64]ast.Expr
	// uncounted are the selections the estimator does not charge.
	uncounted []uncountedSelect
}

// uncountedSelect is one selection the estimator does not charge.
type uncountedSelect struct {
	// loops are the comprehensions whose loop evaluates the selection once
	// per iteration, outermost first.
	loops []int64
	// chain counts the uncounted selections from this one down through its
	// operands, itself included: in a.b.c over a dyn a, c's chain is two.
	chain uint64
}

// collectCostFacts walks the checked AST a.
func collectCostFacts(a *ast.AST) *costFacts {
	w := &factWalker{
		a:      a,
		facts:  &costFacts{paths: map[int64][]string{}, ranges: map[int64]ast.Expr{}},
		chains: map[int64]uint64{},
	}
	w.walk(a.Expr())
	return w.facts
}

// factWalker collects the costFacts of one checked AST.
type factWalker struct {
	a     *ast.AST
	facts *costFacts
	// locals are the comprehension variables in scope, innermost last.
	locals []local
	// loops are the comprehensions whose loop encloses the walk.
	loops []int64
	// chains holds the chain of each uncounted selection by ID.
	chains map[int64]uint64
}

// local is one comprehension variable in scope.
type local struct {
	name string
	// path is the variable's path; nil when its values are not reached from
	// a Ruralz variable, as for an accumulator.
	path []string
}

// walk visits e and its operands.
func (w *factWalker) walk(e ast.Expr) {
	switch e.Kind() {
	case ast.IdentKind:
		w.ident(e)
	case ast.SelectKind:
		w.selection(e)
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
			if p, ok := w.facts.paths[args[0].ID()]; ok {
				entry := "@items"
				if t := w.a.GetType(args[0].ID()); t.Kind() == types.MapKind {
					entry = "@values"
				}
				w.facts.paths[e.ID()] = appendPath(p, entry)
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
// path of its range's entries.
func (w *factWalker) ident(e ast.Expr) {
	name := e.AsIdent()
	for i := len(w.locals) - 1; i >= 0; i-- {
		if w.locals[i].name == name {
			if p := w.locals[i].path; p != nil {
				w.facts.paths[e.ID()] = p
			}
			return
		}
	}
	if _, ok := varBit(name); ok {
		w.facts.paths[e.ID()] = []string{name}
	}
}

// selection extends its operand's path and records a selection the
// estimator does not charge. A presence test is charged as at runtime.
func (w *factWalker) selection(e ast.Expr) {
	sel := e.AsSelect()
	op := sel.Operand()
	w.walk(op)
	if sel.IsTestOnly() {
		return
	}
	if p, ok := w.facts.paths[op.ID()]; ok {
		w.facts.paths[e.ID()] = appendPath(p, sel.FieldName())
	}
	if charged(w.a.GetType(op.ID())) {
		return
	}
	chain := w.chains[op.ID()] + 1
	w.chains[e.ID()] = chain
	w.facts.uncounted = append(w.facts.uncounted, uncountedSelect{loops: slices.Clone(w.loops), chain: chain})
}

// comprehension records the range and scopes the variables: the iteration
// variable and the accumulator in the loop, the accumulator alone in the
// result.
func (w *factWalker) comprehension(e ast.Expr) {
	comp := e.AsComprehension()
	rng := comp.IterRange()
	w.walk(rng)
	w.walk(comp.AccuInit())
	w.facts.ranges[e.ID()] = rng
	var iter []string
	if p, ok := w.facts.paths[rng.ID()]; ok && !comp.HasIterVar2() {
		entry := "@keys"
		if t := w.a.GetType(rng.ID()); t.Kind() == types.ListKind {
			entry = "@items"
		}
		iter = appendPath(p, entry)
	}
	n := len(w.locals)
	w.locals = append(w.locals, local{name: comp.AccuVar()}, local{name: comp.IterVar(), path: iter})
	if comp.HasIterVar2() {
		w.locals = append(w.locals, local{name: comp.IterVar2()})
	}
	w.loops = append(w.loops, e.ID())
	w.walk(comp.LoopCondition())
	w.walk(comp.LoopStep())
	w.loops = w.loops[:len(w.loops)-1]
	w.locals = w.locals[:n+1]
	w.walk(comp.Result())
	w.locals = w.locals[:n]
}

// charged reports whether cel-go's estimator charges a selection of a
// field of a value of type t.
func charged(t *types.Type) bool {
	switch t.Kind() {
	case types.MapKind, types.StructKind, types.TypeParamKind:
		return true
	default:
		return false
	}
}

// appendPath returns p with one more element, never sharing p's array.
func appendPath(p []string, elem string) []string {
	return append(slices.Clip(p), elem)
}

// bound returns the most an evaluation can cost: est, cel-go's estimate
// with sizes from e, plus one unit per evaluation of an uncounted
// selection. A selection inside loops whose ranges e sizes is evaluated at
// most once per iteration of each. For one inside a loop e cannot size,
// the base its chain selects from, an identifier, a charged selection or a
// call, costs at least one unit per evaluation in est, so the chain's
// length times est bounds those evaluations.
func (f *costFacts) bound(a *ast.AST, est checker.CostEstimate, e checker.CostEstimator) uint64 {
	if len(f.uncounted) == 0 {
		return est.Max
	}
	s := &sizer{a: a, facts: f, est: e, iters: map[int64]sized{}}
	var known, chain uint64
	for _, u := range f.uncounted {
		evals, ok := uint64(1), true
		for _, id := range u.loops {
			n, sizedRange := s.iterations(id)
			if !sizedRange {
				ok = false
				break
			}
			evals = mulSat(evals, n)
		}
		if ok {
			known = addSat(known, evals)
		} else {
			chain = max(chain, u.chain)
		}
	}
	return addSat(mulSat(est.Max, addSat(chain, 1)), known)
}

// sizer bounds the ranges of one expression's comprehensions.
type sizer struct {
	a     *ast.AST
	facts *costFacts
	est   checker.CostEstimator
	iters map[int64]sized
}

// sized is a memoized size bound; ok is false when there is none.
type sized struct {
	n  uint64
	ok bool
}

// iterations returns the most iterations of comprehension id: the size of
// its range.
func (s *sizer) iterations(id int64) (uint64, bool) {
	if v, ok := s.iters[id]; ok {
		return v.n, v.ok
	}
	n, ok := s.size(s.facts.ranges[id])
	s.iters[id] = sized{n: n, ok: ok}
	return n, ok
}

// size returns the most entries of a list or map, or code points of a
// string, e can hold: exact for a literal, derived for a map or filter
// (at most one entry per iteration), split (one more entry than its
// string's size), concatenation, a conditional and dyn, and the
// estimator's size for a value reached from a Ruralz variable. Any other
// value has no bound here.
func (s *sizer) size(e ast.Expr) (uint64, bool) {
	switch e.Kind() {
	case ast.LiteralKind:
		if sz, ok := e.AsLiteral().(traits.Sizer); ok {
			if n, ok := sz.Size().(types.Int); ok && n >= 0 {
				return uint64(n), true
			}
		}
		return 0, false
	case ast.ListKind:
		return uint64(len(e.AsList().Elements())), true
	case ast.MapKind:
		return uint64(len(e.AsMap().Entries())), true
	case ast.ComprehensionKind:
		return s.iterations(e.ID())
	case ast.CallKind:
		if n, ok, derived := s.callSize(e.AsCall()); derived {
			return n, ok
		}
	default:
	}
	p, rooted := s.facts.paths[e.ID()]
	if !rooted {
		return 0, false
	}
	sz := s.est.EstimateSize(factNode{e: e, t: s.a.GetType(e.ID()), path: p})
	if sz == nil {
		return 0, false
	}
	return sz.Max, true
}

// callSize derives the size of a call's result; derived is false for a
// call it does not size.
func (s *sizer) callSize(c ast.CallExpr) (n uint64, ok, derived bool) {
	args := c.Args()
	switch c.FunctionName() {
	case "split":
		if c.IsMemberFunction() {
			n, ok = s.size(c.Target())
			return addSat(n, 1), ok, true
		}
	case operators.Add:
		if len(args) == 2 {
			l, lok := s.size(args[0])
			r, rok := s.size(args[1])
			return addSat(l, r), lok && rok, true
		}
	case operators.Conditional:
		if len(args) == 3 {
			l, lok := s.size(args[1])
			r, rok := s.size(args[2])
			return max(l, r), lok && rok, true
		}
	case overloads.TypeConvertDyn:
		if len(args) == 1 {
			n, ok = s.size(args[0])
			return n, ok, true
		}
	default:
	}
	return 0, false, false
}

// factNode is the checker.AstNode of a value reached from a Ruralz
// variable, for an estimator's EstimateSize.
type factNode struct {
	e    ast.Expr
	t    *types.Type
	path []string
}

func (n factNode) Path() []string                      { return n.path }
func (n factNode) Type() *types.Type                   { return n.t }
func (n factNode) Expr() ast.Expr                      { return n.e }
func (n factNode) ComputedSize() *checker.SizeEstimate { return nil }

// splitEstimates are the cost estimates of split, replacing ext.Strings'
// own, which size the result at the string's size: a string of n
// separators splits into n + 1 entries, so a comprehension over the
// result can run one iteration more than cel-go estimates.
func splitEstimates() []checker.CostOption {
	return []checker.CostOption{
		checker.OverloadCostEstimate("string_split_string", estimateSplit),
		checker.OverloadCostEstimate("string_split_string_int", estimateSplit),
	}
}

// estimateSplit is ext.Strings' split estimate, traversal plus list
// creation plus one unit per entry, with at most one entry more than the
// string's size.
func estimateSplit(e checker.CostEstimator, target *checker.AstNode, args []checker.AstNode) *checker.CallEstimate {
	if target == nil || len(args) < 1 {
		return nil
	}
	size := checker.SizeEstimate{Min: 0, Max: ^uint64(0)}
	if sz := (*target).ComputedSize(); sz != nil {
		size = *sz
	} else if sz := e.EstimateSize(*target); sz != nil {
		size = *sz
	}
	traversal := size.Add(checker.FixedSizeEstimate(1)).MultiplyByCostFactor(common.StringTraversalCostFactor)
	result := checker.SizeEstimate{Min: 0, Max: addSat(size.Max, 1)}
	cost := traversal.
		Add(result.MultiplyByCostFactor(1)).
		Add(checker.FixedCostEstimate(common.ListCreateBaseCost)).
		Add(checker.FixedCostEstimate(1))
	return &checker.CallEstimate{CostEstimate: cost, ResultSize: &result}
}

// addSat returns a + b, saturating at the largest uint64.
func addSat(a, b uint64) uint64 {
	s, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return ^uint64(0)
	}
	return s
}

// mulSat returns a × b, saturating at the largest uint64.
func mulSat(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	if hi != 0 {
		return ^uint64(0)
	}
	return lo
}
