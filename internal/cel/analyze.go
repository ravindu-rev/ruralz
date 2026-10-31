// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"slices"
	"strconv"

	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/overloads"
	"cel.dev/cel-go/common/types"

	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// analysis is what one walk of a checked expression finds: the static
// facts of expr.Refs, the site findings, and the inputs of the cost
// estimators.
type analysis struct {
	refs expr.Refs
	// findings are RZ-CFG-014 site findings, in walk order.
	findings []finding
	// containers are the IDs of expressions used as a comprehension range
	// or an in container; the nominal estimator sizes a dyn one as a list.
	containers map[int64]bool
	// folds reports a comprehension.
	folds bool
	// jsonEncode reports a json.encode call, whose cost cel-go cannot bound.
	jsonEncode bool
}

// finding is one site finding at an expression ID.
type finding struct {
	id   int64
	msg  string
	hint string
}

// walker walks one checked AST.
type walker struct {
	a  *ast.AST
	se siteEnv
	an *analysis
	// shadow counts the comprehension variables in scope by name, so a
	// local named like a Ruralz variable is not that variable.
	shadow map[string]int
	// steps and stepBodies collect the constant steps keys.
	steps, stepBodies map[string]bool
	// stepsAt is the first reference to the steps variable; 0 when none.
	stepsAt int64
}

// analyze walks the checked AST a of a source compiled at se.
func analyze(a *ast.AST, se siteEnv) *analysis {
	w := &walker{
		a:          a,
		se:         se,
		an:         &analysis{containers: map[int64]bool{}},
		shadow:     map[string]int{},
		steps:      map[string]bool{},
		stepBodies: map[string]bool{},
	}
	w.walk(a.Expr(), false)
	w.an.refs.Steps = sortedKeys(w.steps)
	w.an.refs.StepBodies = sortedKeys(w.stepBodies)
	if se.composition && w.stepsAt != 0 && se.mode != v1alpha1.CompositionModeSequential {
		// Only sequential compositions run steps before others; in
		// aggregate and conditional ones no earlier result exists.
		w.an.findings = append(w.an.findings, finding{
			id:   w.stepsAt,
			msg:  "steps is available only in sequential compositions",
			hint: "this composition is " + strconv.Quote(string(se.mode)),
		})
	}
	return w.an
}

// walk visits e. keyed is true when the parent reads e by a constant or
// computed key (a selection, an index or an in test), so a steps map used
// there is not read whole.
func (w *walker) walk(e ast.Expr, keyed bool) {
	if e == nil {
		return
	}
	if !keyed && isStepsType(w.a.GetType(e.ID())) {
		// Iterated, sized, compared or passed on: every earlier step counts
		// as read.
		w.an.refs.AnyStep = true
	}
	switch e.Kind() {
	case ast.IdentKind:
		w.ident(e)
	case ast.SelectKind:
		w.selection(e)
	case ast.CallKind:
		w.call(e)
	case ast.ComprehensionKind:
		w.comprehension(e)
	case ast.ListKind:
		for _, el := range e.AsList().Elements() {
			w.walk(el, false)
		}
	case ast.MapKind:
		for _, ent := range e.AsMap().Entries() {
			me := ent.AsMapEntry()
			w.walk(me.Key(), false)
			w.walk(me.Value(), false)
		}
	case ast.StructKind:
		st := e.AsStruct()
		if celtypes.IsObjectType(st.TypeName()) {
			w.add(e.ID(), st.TypeName()+" values cannot be constructed", "the data plane builds the Ruralz views")
		}
		for _, f := range st.Fields() {
			w.walk(f.AsStructField().Value(), false)
		}
	case ast.LiteralKind, ast.UnspecifiedExprKind:
	}
}

// ident records a reference to a Ruralz variable.
func (w *walker) ident(e ast.Expr) {
	name := e.AsIdent()
	if w.shadow[name] > 0 {
		return
	}
	bit, ok := varBit(name)
	if !ok {
		return
	}
	w.an.refs.Vars |= bit
	if bit == expr.VarSteps && w.stepsAt == 0 {
		w.stepsAt = e.ID()
	}
}

// selection handles x.f and has(x.f).
func (w *walker) selection(e ast.Expr) {
	sel := e.AsSelect()
	op := sel.Operand()
	opType := w.a.GetType(op.ID())
	field := sel.FieldName()
	switch {
	case isStepsType(opType):
		w.stepKey(e.ID(), field)
		w.walk(op, true)
		return
	case field == "body" && isObject(opType, celtypes.TypeRequest):
		w.an.refs.RequestBody = true
		if !w.se.reqBody {
			w.add(e.ID(), "request.body is not available here", "the request body is read only in body Phases and composition")
		}
	case field == "body" && isObject(opType, celtypes.TypeResponse):
		w.an.refs.ResponseBody = true
		if !w.se.respBody {
			w.add(e.ID(), "response.body is not available here", "the response body is read only by transform.response")
		}
	case field == "body" && isObject(opType, celtypes.TypeStep):
		if key, ok := w.constStepKey(op); ok {
			w.stepBodies[key] = true
		} else {
			w.an.refs.AnyStep = true
		}
	}
	w.walk(op, false)
}

// call handles function calls and operators.
func (w *walker) call(e ast.Expr) {
	c := e.AsCall()
	args := c.Args()
	switch fn := c.FunctionName(); {
	case fn == operators.Index && len(args) == 2 && isStepsType(w.a.GetType(args[0].ID())):
		if key, ok := stringLiteral(args[1]); ok {
			w.stepKey(e.ID(), key)
		} else {
			w.an.refs.AnyStep = true
		}
		w.walk(args[0], true)
		w.walk(args[1], false)
		return
	case (fn == operators.In || fn == operators.OldIn) && len(args) == 2:
		w.an.containers[args[1].ID()] = true
		if isStepsType(w.a.GetType(args[1].ID())) {
			if key, ok := stringLiteral(args[0]); ok {
				w.stepKey(e.ID(), key)
			} else {
				w.an.refs.AnyStep = true
			}
			w.walk(args[0], false)
			w.walk(args[1], true)
			return
		}
	case fn == "json.encode":
		w.an.jsonEncode = true
	}
	if w.withTimeZone(e.ID()) && len(args) > 0 {
		if zone := args[len(args)-1]; !literalZone(zone) {
			w.add(zone.ID(), `the time zone must be the literal "UTC" or a UTC offset such as "+02:00"`,
				"a named time zone reads zone data at every evaluation")
		}
	}
	if c.IsMemberFunction() {
		w.walk(c.Target(), false)
	}
	for _, a := range args {
		w.walk(a, false)
	}
}

// comprehension handles the macros: all, exists, exists_one, map, filter.
func (w *walker) comprehension(e ast.Expr) {
	comp := e.AsComprehension()
	w.an.folds = true
	w.an.containers[comp.IterRange().ID()] = true
	w.walk(comp.IterRange(), false)
	w.walk(comp.AccuInit(), false)
	locals := []string{comp.IterVar(), comp.AccuVar()}
	if comp.HasIterVar2() {
		locals = append(locals, comp.IterVar2())
	}
	for _, n := range locals {
		w.shadow[n]++
	}
	w.walk(comp.LoopCondition(), false)
	w.walk(comp.LoopStep(), false)
	// The result sees the accumulator only.
	w.shadow[comp.IterVar()]--
	if comp.HasIterVar2() {
		w.shadow[comp.IterVar2()]--
	}
	w.walk(comp.Result(), false)
	w.shadow[comp.AccuVar()]--
}

// stepKey records a constant steps key read at id, which in a sequential
// composition must name an earlier step.
func (w *walker) stepKey(id int64, key string) {
	w.steps[key] = true
	if w.se.composition && w.se.mode == v1alpha1.CompositionModeSequential && !slices.Contains(w.se.earlier, key) {
		w.add(id, "step "+strconv.Quote(key)+" is not an earlier step of this composition",
			"steps holds the steps that ran before this one, in list order")
	}
}

// constStepKey returns the key of e when e reads one step by a constant
// key: steps.x, steps["x"].
func (w *walker) constStepKey(e ast.Expr) (string, bool) {
	switch e.Kind() {
	case ast.SelectKind:
		sel := e.AsSelect()
		if !sel.IsTestOnly() && isStepsType(w.a.GetType(sel.Operand().ID())) {
			return sel.FieldName(), true
		}
	case ast.CallKind:
		c := e.AsCall()
		if c.FunctionName() == operators.Index && len(c.Args()) == 2 && isStepsType(w.a.GetType(c.Args()[0].ID())) {
			return stringLiteral(c.Args()[1])
		}
	default:
	}
	return "", false
}

// withTimeZone reports a timestamp accessor called with a time zone.
func (w *walker) withTimeZone(id int64) bool {
	for _, o := range w.a.GetOverloadIDs(id) {
		switch o {
		case overloads.TimestampToYearWithTz, overloads.TimestampToMonthWithTz,
			overloads.TimestampToDayOfYearWithTz, overloads.TimestampToDayOfMonthZeroBasedWithTz,
			overloads.TimestampToDayOfMonthOneBasedWithTz, overloads.TimestampToDayOfWeekWithTz,
			overloads.TimestampToHoursWithTz, overloads.TimestampToMinutesWithTz,
			overloads.TimestampToSecondsWithTz, overloads.TimestampToMillisecondsWithTz:
			return true
		}
	}
	return false
}

// add records a finding at id.
func (w *walker) add(id int64, msg, hint string) {
	w.an.findings = append(w.an.findings, finding{id: id, msg: msg, hint: hint})
}

// literalZone reports a string literal "UTC" or ±HH:MM with HH at most 23
// and MM at most 59: the zones cel-go resolves without reading zone data.
func literalZone(e ast.Expr) bool {
	s, ok := stringLiteral(e)
	if !ok {
		return false
	}
	if s == "UTC" {
		return true
	}
	if len(s) != 6 || (s[0] != '+' && s[0] != '-') || s[3] != ':' {
		return false
	}
	for _, i := range []int{1, 2, 4, 5} {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	hours := int(s[1]-'0')*10 + int(s[2]-'0')
	minutes := int(s[4]-'0')*10 + int(s[5]-'0')
	return hours <= 23 && minutes <= 59
}

// stringLiteral returns the value of a string literal.
func stringLiteral(e ast.Expr) (string, bool) {
	if e.Kind() != ast.LiteralKind {
		return "", false
	}
	s, ok := e.AsLiteral().(types.String)
	return string(s), ok
}

// isObject reports the Ruralz object type name.
func isObject(t *types.Type, name string) bool {
	return t != nil && t.Kind() == types.StructKind && t.TypeName() == name
}

// isStepsType reports map(string, ruralz.Step), the type of steps.
func isStepsType(t *types.Type) bool {
	if t == nil || t.Kind() != types.MapKind {
		return false
	}
	ps := t.Parameters()
	return len(ps) == 2 && isObject(ps[1], celtypes.TypeStep)
}

// varBit returns the expr.Var of a variable name.
func varBit(name string) (expr.Var, bool) {
	for b := expr.VarRequest; b <= expr.VarSelf; b <<= 1 {
		if n := b.Names(); len(n) == 1 && n[0] == name {
			return b, true
		}
	}
	return 0, false
}

// sortedKeys returns the keys of m in ascending order; nil when empty.
func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
