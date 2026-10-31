// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"errors"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeAuthzCEL

// Codes, each registered in internal/errcode.
const (
	// CodeDenied is a request the rule refuses (403).
	CodeDenied = "RZ-AUTH-010"
	// CodeConfig is a config of another type or an empty rule.
	CodeConfig = "RZ-CFG-005"
	// CodeCEL is a rule with a syntax, type or unavailable-variable error,
	// including a rule that reads request.body placed where the body is
	// not available.
	CodeCEL = "RZ-CFG-014"
	// CodeCost is a rule whose static cost estimate exceeds the ceiling.
	CodeCost = "RZ-CFG-015"
	// CodeScope is a placement in which the rule never runs.
	CodeScope = "RZ-CFG-020"
)

// Error types a Filter names for the span's error.type
// (filter.Result.Err).
const (
	errTypeCEL        = "cel_error"
	errTypeResultType = "result_type"
)

// ErrNoCEL reports a Build without a CEL builder (BuildEnv.CEL).
var ErrNoCEL = errors.New("authz.cel: no CEL builder")

// Factory builds authz.cel Filters (filter.Factory).
type Factory struct{}

var _ filter.Factory = Factory{}

// Build compiles config.rule at the Policy's site and fixes the Phase it
// decides in. A config of another type or an empty rule is RZ-CFG-005, a
// rule that does not compile RZ-CFG-014 or RZ-CFG-015 as the compiler
// reports it, and a placement where the rule would never decide RZ-CFG-014
// or RZ-CFG-020 (package documentation, "Configuration"). An ended ctx is
// its error, without a code; a Build without BuildEnv.CEL is ErrNoCEL.
func (Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.AuthzCELConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeConfig, "authz.cel: config is %T, want *v1alpha1.AuthzCELConfig", env.Config)
	}
	if cfg.Rule == "" {
		return nil, errcode.Errorf(CodeConfig, "authz.cel: config.rule is empty")
	}
	decide, err := decisionPhase(env)
	if err != nil {
		return nil, err
	}
	if env.CEL == nil {
		return nil, ErrNoCEL
	}
	prg, iss := env.CEL.Compile(site(env), cfg.Rule)
	if prg == nil {
		return nil, compileError(iss)
	}
	body := prg.Refs().RequestBody
	if body && decide != phase.OnRequestBody {
		return nil, errcode.Errorf(CodeCEL, "authz.cel: config.rule reads request.body, which %s does not provide", decide)
	}
	return &Filter{Decider: auth.NewAuthz(env.Metrics), rule: prg, decide: decide, body: body}, nil
}

// decisionPhase returns the Phase the rule decides in: onRequestBody when
// env subscribes the Policy to it, else onRequestHeaders. Upstream scope,
// or a Phase set with neither, is RZ-CFG-020: the Filter would never be
// called in a Phase where it decides, which would admit every request.
func decisionPhase(env filter.BuildEnv) (phase.Phase, error) {
	switch {
	case env.Scope == phase.ScopeUpstream:
		return 0, errcode.Errorf(CodeScope, "authz.cel: not allowed at Upstream scope (allowed: Gateway, Route)")
	case env.Phases.Has(phase.OnRequestBody):
		return phase.OnRequestBody, nil
	case env.Phases.Has(phase.OnRequestHeaders):
		return phase.OnRequestHeaders, nil
	default:
		return 0, errcode.Errorf(CodeScope, "authz.cel: subscribed to neither onRequestHeaders nor onRequestBody")
	}
}

// site returns the rule's site at env's attachment: env.Site when set,
// else the place refined by scope and type alone.
func site(env filter.BuildEnv) expr.Site {
	if env.Site != nil {
		return env.Site(expr.PlaceAuthzCELRule)
	}
	return expr.Site{Place: expr.PlaceAuthzCELRule, Scope: env.Scope, PolicyType: Type}
}

// compileError returns the Build error of a rule the builder refused: the
// first Issue's code, RZ-CFG-015 for the cost ceiling and RZ-CFG-014
// otherwise, with its message.
func compileError(iss []expr.Issue) error {
	if len(iss) == 0 {
		return errcode.Errorf(CodeCEL, "authz.cel: config.rule did not compile")
	}
	code := CodeCEL
	if iss[0].Code == CodeCost {
		code = CodeCost
	}
	return errcode.Errorf(code, "authz.cel: config.rule: %s", iss[0].Message)
}

// Filter is a compiled authz.cel Policy. The embedded Decider counts its
// decisions and builds its answers. It is immutable and safe for
// concurrent use.
type Filter struct {
	*auth.Decider
	// rule is the compiled config.rule.
	rule expr.Program
	// decide is the Phase the rule runs in.
	decide phase.Phase
	// body is true when the rule reads request.body.
	body bool
}

var _ filter.Filter = (*Filter)(nil)

// Handle decides in its Phase and continues in every other one: true
// admits, false is 403 RZ-AUTH-010 and a runtime error cannot decide (403
// RZ-AUTH-015 under closed). Before a body rule, a decoded body over its
// cap or a refused buffer reservation cannot decide with filter.ErrTooLarge
// or filter.ErrBudget, counted as no decision. Admitting allocates nothing
// beyond the rule's evaluation.
func (f *Filter) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != f.decide {
		return filter.Next()
	}
	if f.body {
		if err := bodyError(ctx, x); err != nil {
			return filter.Undecided("", err)
		}
	}
	ok, err := f.rule.EvalBool(ctx, x.Vars())
	switch {
	case err != nil:
		return f.Undecided(x, "", classify(err))
	case !ok:
		return f.Deny(x, CodeDenied)
	default:
		return f.Allow(x)
	}
}

// bodyError returns the decoded request body's filter.ErrTooLarge or
// filter.ErrBudget, the gate failures that decide the request's answer
// before the rule; nil otherwise, so a body that is not available or not
// JSON reaches the rule as CEL sees it.
func bodyError(ctx context.Context, x filter.Exchange) error {
	m := x.Message()
	if m == nil {
		return nil
	}
	_, err := m.Decoded(ctx)
	if errors.Is(err, filter.ErrTooLarge) || errors.Is(err, filter.ErrBudget) {
		return err
	}
	return nil
}

// ruleError is a runtime error of the rule. It names the span's error.type,
// result_type for a dyn result that is not a bool and cel_error otherwise;
// its text is the *expr.EvalError's, which never holds request data.
type ruleError struct {
	kind string
	err  error
}

// Error returns the cause's text.
func (e *ruleError) Error() string { return e.err.Error() }

// Unwrap returns the cause.
func (e *ruleError) Unwrap() error { return e.err }

// ErrorType returns the span's error.type.
func (e *ruleError) ErrorType() string { return e.kind }

// classify wraps a runtime error of the rule with its error.type.
func classify(err error) error {
	kind := errTypeCEL
	var ee *expr.EvalError
	if errors.As(err, &ee) && ee.Kind == expr.KindResultType {
		kind = errTypeResultType
	}
	return &ruleError{kind: kind, err: err}
}
