// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/interpreter"

	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Program is the expr.Program of a compiled expression. It is immutable and
// safe for concurrent evaluation.
type Program struct {
	place expr.PlaceID
	src   string
	refs  expr.Refs
	cost  expr.CostRange
	prg   celgo.Program
	// tracked programs run under the runtime cost limit.
	tracked bool
}

// Compile-time check of the contract.
var _ expr.Program = (*Program)(nil)

// plan builds the program of a compiled source, a tracked one under the
// runtime cost limit.
func (c *Compiler) plan(cc *compiled, tracked bool) (*Program, error) {
	opts := []celgo.ProgramOption{celgo.EvalOptions(celgo.OptOptimize)}
	if tracked {
		opts = append(opts, celgo.CostLimit(runtimeCostLimit))
	}
	prg, err := cc.env.Program(cc.ast, opts...)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	return &Program{
		place:   cc.se.place.ID,
		src:     cc.src,
		refs:    cc.an.refs,
		cost:    cc.cost,
		prg:     prg,
		tracked: tracked,
	}, nil
}

// Place returns the place the program was compiled for.
func (p *Program) Place() expr.PlaceID { return p.place }

// Source returns the expression text.
func (p *Program) Source() string { return p.src }

// Refs returns what the program reads.
func (p *Program) Refs() expr.Refs { return p.refs }

// Cost returns the static estimate at nominal sizes.
func (p *Program) Cost() expr.CostRange { return p.cost }

// EvalBool evaluates a bool place; any other result is KindResultType.
func (p *Program) EvalBool(v *expr.Vars) (bool, error) {
	out, err := p.eval(v)
	if err != nil {
		return false, err
	}
	b, ok := out.(types.Bool)
	if !ok {
		return false, p.resultError(out, "bool")
	}
	return bool(b), nil
}

// EvalString evaluates a string place; any other result is KindResultType.
func (p *Program) EvalString(v *expr.Vars) (string, error) {
	out, err := p.eval(v)
	if err != nil {
		return "", err
	}
	s, ok := out.(types.String)
	if !ok {
		return "", p.resultError(out, "string")
	}
	return string(s), nil
}

// EvalInt evaluates an int place; any other result is KindResultType.
func (p *Program) EvalInt(v *expr.Vars) (int64, error) {
	out, err := p.eval(v)
	if err != nil {
		return 0, err
	}
	n, ok := out.(types.Int)
	if !ok {
		return 0, p.resultError(out, "int")
	}
	return int64(n), nil
}

// EvalValue evaluates a dyn place, such as a transform body.
func (p *Program) EvalValue(v *expr.Vars) (expr.Value, error) {
	out, err := p.eval(v)
	if err != nil {
		return nil, err
	}
	return celtypes.FromVal(out), nil
}

// eval evaluates the program over v. A tracked program that passes the
// runtime cost limit ends with cel-go's cost-limit error, which no logical
// operator absorbs, as cel-go aborts the evaluation there.
func (p *Program) eval(v *expr.Vars) (ref.Val, error) {
	if v == nil {
		v = &expr.Vars{}
	}
	out, _, err := p.prg.Eval(celtypes.NewActivation(v))
	if err != nil {
		return nil, expr.NewEvalError(p.place, errorKind(err), err.Error())
	}
	return out, nil
}

// resultError is the KindResultType error of a result that is not want.
func (p *Program) resultError(out ref.Val, want string) error {
	return expr.NewEvalError(p.place, expr.KindResultType,
		"result has type "+out.Type().TypeName()+", want "+want)
}

// errorKind classifies an evaluation error: the cost limit first, then the
// celtypes sentinels, then cel-go's own error texts.
func errorKind(err error) expr.ErrorKind {
	var canceled interpreter.EvalCancelledError
	if errors.As(err, &canceled) && canceled.Cause == interpreter.CostLimitExceeded {
		return expr.KindCostLimit
	}
	switch {
	case errors.Is(err, celtypes.ErrNull):
		return expr.KindNull
	case errors.Is(err, celtypes.ErrBody):
		return expr.KindBody
	case errors.Is(err, celtypes.ErrNoSuchKey):
		return expr.KindNoSuchKey
	}
	var num *strconv.NumError
	if errors.As(err, &num) {
		return expr.KindConversion
	}
	msg := err.Error()
	switch {
	case containsAny(msg, "no such key", "no such field", "no such attribute", "index out of range"):
		return expr.KindNoSuchKey
	case containsAny(msg, "no such overload", "no matching overload"):
		return expr.KindNoSuchOverload
	case containsAny(msg, "division by zero", "modulus by zero", "overflow", "NaN values cannot be ordered"):
		return expr.KindArithmetic
	case containsAny(msg, "conversion", "invalid RFC 3339", "timezone offset", "invalid duration",
		"illegal base64", "cannot parse", "invalid syntax"):
		return expr.KindConversion
	default:
		return expr.KindOther
	}
}

// containsAny reports whether s contains one of subs.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
