// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/interpreter"

	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
)

// interruptCheckFrequency is the number of comprehension iterations between
// checks of the evaluation context of a stoppable program.
const interruptCheckFrequency = 100

// Program is the expr.Program of a compiled expression. It is immutable and
// safe for concurrent evaluation.
type Program struct {
	place expr.PlaceID
	src   string
	refs  expr.Refs
	cost  expr.CostRange
	prg   celgo.Program
	// tracked programs run under the runtime cost limit; stoppable ones,
	// tracked with a comprehension, also under stop or the deadline, timed
	// by the Compiler's stops.
	tracked, stoppable bool
	stop               time.Duration
	stops              *stopPool
}

// Compile-time check of the contract.
var _ expr.Program = (*Program)(nil)

// plan builds the program of a compiled source: a tracked one under the
// runtime cost limit, a stoppable one, tracked with a comprehension, with
// interrupt checks.
func (c *Compiler) plan(cc *compiled, tracked bool) (*Program, error) {
	opts := []celgo.ProgramOption{celgo.EvalOptions(celgo.OptOptimize)}
	stoppable := tracked && cc.an.folds
	if tracked {
		opts = append(opts, celgo.CostLimit(runtimeCostLimit))
	}
	if stoppable {
		opts = append(opts, celgo.InterruptCheckFrequency(interruptCheckFrequency))
	}
	prg, err := cc.env.Program(cc.ast, opts...)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	return &Program{
		place:     cc.se.place.ID,
		src:       cc.src,
		refs:      cc.an.refs,
		cost:      cc.cost,
		prg:       prg,
		tracked:   tracked,
		stoppable: stoppable,
		stop:      c.stop,
		stops:     c.stops,
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
func (p *Program) EvalBool(ctx context.Context, v *expr.Vars) (bool, error) {
	out, err := p.eval(ctx, v)
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
func (p *Program) EvalString(ctx context.Context, v *expr.Vars) (string, error) {
	out, err := p.eval(ctx, v)
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
func (p *Program) EvalInt(ctx context.Context, v *expr.Vars) (int64, error) {
	out, err := p.eval(ctx, v)
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
func (p *Program) EvalValue(ctx context.Context, v *expr.Vars) (expr.Value, error) {
	out, err := p.eval(ctx, v)
	if err != nil {
		return nil, err
	}
	return celtypes.FromVal(out), nil
}

// eval evaluates the program over v. A stoppable program runs under a
// pooled stop that fires at the smaller of v.Deadline, the request
// deadline, and the stop, on the Compiler's clock, or at its first
// interrupt check when the deadline has passed or ctx has already ended
// (stopPool); any other program cannot run past the runtime limit and
// ignores both.
func (p *Program) eval(ctx context.Context, v *expr.Vars) (ref.Val, error) {
	if v == nil {
		v = &expr.Vars{}
	}
	act := celtypes.NewActivation(v)
	var (
		out ref.Val
		err error
	)
	if p.stoppable {
		out, err = p.evalStoppable(ctx, v.Deadline, act)
	} else {
		out, _, err = p.prg.Eval(act)
	}
	if err != nil {
		return nil, expr.NewEvalError(p.place, errorKind(err), err.Error())
	}
	return out, nil
}

// errStopped is the runtime error of an evaluation its stop ended.
var errStopped = interpreter.EvalCancelledError{Message: "operation interrupted", Cause: interpreter.ContextCancelled}

// evalStoppable evaluates a stoppable program in an execution frame whose
// context is the stop at the earlier of deadline and p.stop, as
// ContextEval does, and keeps the frame until it has read whether an
// interrupt check saw the stop. cel-go ends an interrupted comprehension
// with an error value, which a logical operator absorbs when its other
// operand decides, as in `list.exists(...) || b`; a stop is a runtime
// error (docs/architecture/02-configuration-model.md "Limits"), so an
// evaluation whose interrupt check saw its stop ends with errStopped
// whatever value it produced. An evaluation that reached no interrupt
// check after its stop keeps its result, as the stop acts only through
// those checks. A cost-limit abort keeps its own error.
func (p *Program) evalStoppable(ctx context.Context, deadline time.Time, act interpreter.Activation) (ref.Val, error) {
	frame, err := interpreter.NewExecutionFrame(act)
	if err != nil {
		return nil, err
	}
	sctx, st := p.stops.start(ctx, deadline, p.stop)
	defer p.stops.end(st)
	if err := frame.SetContext(sctx, interruptCheckFrequency); err != nil {
		frame.Close()
		return nil, err
	}
	out, _, err := p.prg.Eval(frame)
	// CheckInterrupt reports a check that saw the stop; it is read only
	// once the stop has passed, as before then no check can have seen it.
	stopped := sctx.Err() != nil && frame.CheckInterrupt()
	frame.Close()
	if !stopped {
		return out, err
	}
	var canceled interpreter.EvalCancelledError
	if errors.As(err, &canceled) && canceled.Cause == interpreter.CostLimitExceeded {
		return nil, err
	}
	return nil, errStopped
}

// resultError is the KindResultType error of a result that is not want.
func (p *Program) resultError(out ref.Val, want string) error {
	return expr.NewEvalError(p.place, expr.KindResultType,
		"result has type "+out.Type().TypeName()+", want "+want)
}

// errorKind classifies an evaluation error: the stops first, then the
// celtypes sentinels, then cel-go's own error texts.
func errorKind(err error) expr.ErrorKind {
	var canceled interpreter.EvalCancelledError
	if errors.As(err, &canceled) {
		if canceled.Cause == interpreter.CostLimitExceeded {
			return expr.KindCostLimit
		}
		return expr.KindDeadline
	}
	switch {
	case errors.Is(err, interpreter.InterruptError{}),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return expr.KindDeadline
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
