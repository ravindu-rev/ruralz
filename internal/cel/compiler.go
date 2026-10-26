// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"errors"
	"fmt"
	"sync"
	"time"

	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/ext"

	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
)

// comprehensionStop bounds the evaluation of a tracked program holding a
// comprehension, with the request deadline (ADR-0019, target).
const comprehensionStop = 50 * time.Millisecond

// Compiler is the expr.Compiler on cel-go: it owns one environment per set
// of declared variables and the compiled defaults of retryOn and
// failureWhen. It is safe for concurrent use; a process builds one with
// New.
type Compiler struct {
	provider *celtypes.Provider
	// stop is comprehensionStop; tests shorten it.
	stop time.Duration

	mu   sync.Mutex
	envs map[expr.Var]*celgo.Env

	// defaults is read-only after New.
	defaults map[expr.PlaceID]*Program
}

// Compile-time check of the contract.
var _ expr.Compiler = (*Compiler)(nil)

// New returns a Compiler with the defaults of retryOn and failureWhen
// compiled.
func New() (*Compiler, error) {
	p, err := celtypes.NewProvider()
	if err != nil {
		return nil, fmt.Errorf("cel: %w", err)
	}
	c := &Compiler{
		provider: p,
		stop:     comprehensionStop,
		envs:     map[expr.Var]*celgo.Env{},
		defaults: map[expr.PlaceID]*Program{},
	}
	for _, d := range []struct {
		place expr.PlaceID
		src   string
	}{
		{expr.PlaceRetryOn, expr.DefaultRetryOn},
		{expr.PlaceFailureWhen, expr.DefaultFailureWhen},
	} {
		prg, iss := c.program(expr.Site{Place: d.place}, d.src)
		if prg == nil {
			return nil, fmt.Errorf("cel: default of %s: %s", d.place, firstMessage(iss))
		}
		c.defaults[d.place] = prg
	}
	return c, nil
}

// Check compiles and cost-checks src at site without keeping a program.
// An empty src is absent: no Issue.
func (c *Compiler) Check(site expr.Site, src string) []expr.Issue {
	if src == "" {
		return nil
	}
	_, iss := c.compile(site, src)
	return iss
}

// ReferencesRequestBody reports whether the authz.cel rule src selects
// request.body, which moves the Policy to onRequestBody. A rule that does
// not compile is an error.
func (c *Compiler) ReferencesRequestBody(src string) (bool, error) {
	se, _ := resolve(expr.Site{Place: expr.PlaceAuthzCELRule})
	cc, iss := c.checked(se, src)
	if cc == nil {
		return false, errors.New(firstMessage(iss))
	}
	return cc.an.refs.RequestBody, nil
}

// NewBuilder starts a snapshot's program set. prev, the active snapshot's
// set, lends its programs to identical sites and sources; nil is valid.
func (c *Compiler) NewBuilder(prev expr.ProgramSet) expr.Builder {
	b := &Builder{c: c, entries: map[programKey]*buildEntry{}}
	if ps, ok := prev.(*ProgramSet); ok && ps != nil {
		b.prev = ps.progs
	}
	return b
}

// Default returns the compiled default of expr.PlaceRetryOn or
// expr.PlaceFailureWhen (expr.DefaultRetryOn, expr.DefaultFailureWhen),
// used when the field is absent; nil for any other place.
func (c *Compiler) Default(p expr.PlaceID) expr.Program {
	if prg, ok := c.defaults[p]; ok {
		return prg
	}
	return nil
}

// PrepareRoute caches r's converted CEL values in r.Prepared.
func (c *Compiler) PrepareRoute(r *expr.Route) { celtypes.PrepareRoute(r) }

// PrepareConsumer caches c's converted CEL values in c.Prepared.
func (c *Compiler) PrepareConsumer(cons *expr.Consumer) { celtypes.PrepareConsumer(cons) }

// env returns the environment declaring vars, building it once.
func (c *Compiler) env(vars expr.Var) (*celgo.Env, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.envs[vars]; ok {
		return e, nil
	}
	opts := []celgo.EnvOption{
		celgo.CustomTypeProvider(c.provider),
		celgo.CustomTypeAdapter(c.provider),
		ext.Strings(ext.StringsVersion(5)),
		ext.Encoders(ext.EncodersVersion(1)),
		celgo.ASTValidators(
			celgo.ValidateDurationLiterals(),
			celgo.ValidateTimestampLiterals(),
			celgo.ValidateRegexLiterals(),
		),
	}
	for _, v := range celtypes.Variables(vars) {
		opts = append(opts, celgo.Variable(v.Name, v.Type))
	}
	e, err := celgo.NewEnv(opts...)
	if err != nil {
		return nil, fmt.Errorf("cel: environment: %w", err)
	}
	c.envs[vars] = e
	return e, nil
}

// checkedExpr is a source that parsed and type-checked at a site.
type checkedExpr struct {
	env *celgo.Env
	ast *celgo.Ast
	an  *analysis
}

// checked parses and type-checks src at se and analyzes the result. It
// returns nil and cel-go's errors when either step fails; the site
// findings are left in the analysis.
func (c *Compiler) checked(se siteEnv, src string) (*checkedExpr, []expr.Issue) {
	env, err := c.env(se.vars)
	if err != nil {
		return nil, []expr.Issue{wholeIssue(codeInvalid, se, err.Error(), "")}
	}
	a, iss := env.Compile(src)
	if iss.Err() != nil {
		return nil, celIssues(se, src, iss)
	}
	return &checkedExpr{env: env, ast: a, an: analyze(a.NativeRep(), se)}, nil
}

// compiled is a source that passed every check at its site.
type compiled struct {
	checkedExpr
	se   siteEnv
	src  string
	cost expr.CostRange
	// tracked reports a program whose cost can pass the runtime limit at
	// some input size, so it evaluates under cost tracking.
	tracked bool
}

// compile runs every check on src at site: parse and type-check, the site
// findings and the result type, then the cost estimates. It returns the
// compiled source, or nil and at least one Issue.
func (c *Compiler) compile(site expr.Site, src string) (*compiled, []expr.Issue) {
	se, ok := resolve(site)
	if !ok {
		return nil, []expr.Issue{{
			Code: codeInvalid, Offset: -1,
			Message: "CEL " + string(site.Place) + ": not a CEL place",
		}}
	}
	cc, iss := c.checked(se, src)
	if cc == nil {
		return nil, iss
	}
	iss = findingIssues(se, src, cc.ast.NativeRep(), cc.an.findings)
	if msg, hint, bad := resultMismatch(se.place.Result, cc.ast.OutputType()); bad {
		iss = append(iss, wholeIssue(codeInvalid, se, msg, hint))
	}
	est, err := cc.env.EstimateCost(cc.ast, nominalEstimator{containers: cc.an.containers})
	if err != nil {
		return nil, append(iss, wholeIssue(codeCost, se, "cost estimate: "+err.Error(), ""))
	}
	if msg, hint, bad := costFinding(est, cc.an); bad {
		iss = append(iss, wholeIssue(codeCost, se, msg, hint))
	}
	if len(iss) > 0 {
		return nil, iss
	}
	bound, err := cc.env.EstimateCost(cc.ast, unsizedEstimator{})
	if err != nil {
		return nil, []expr.Issue{wholeIssue(codeCost, se, "cost estimate: "+err.Error(), "")}
	}
	return &compiled{
		checkedExpr: *cc,
		se:          se,
		src:         src,
		cost:        expr.CostRange{Min: est.Min, Max: est.Max},
		tracked:     bound.Max > runtimeCostLimit,
	}, nil
}

// program compiles src at site and plans its program.
func (c *Compiler) program(site expr.Site, src string) (*Program, []expr.Issue) {
	cc, iss := c.compile(site, src)
	if cc == nil {
		return nil, iss
	}
	prg, err := c.plan(cc)
	if err != nil {
		return nil, []expr.Issue{wholeIssue(codeInvalid, cc.se, err.Error(), "")}
	}
	return prg, nil
}

// resultMismatch reports an output type the place cannot take: a bool
// place takes bool or dyn, a string place string or dyn, a dyn place any.
func resultMismatch(want expr.Result, got *types.Type) (msg, hint string, bad bool) {
	if got == nil {
		return "", "", false
	}
	switch got.Kind() {
	case types.DynKind, types.AnyKind, types.ErrorKind:
		return "", "", false
	default:
	}
	switch want {
	case expr.ResultBool:
		if got.Kind() != types.BoolKind {
			return "expression has type " + got.String() + ", want bool", "compare the value, as in x == \"y\"", true
		}
	case expr.ResultString:
		if got.Kind() != types.StringKind {
			return "expression has type " + got.String() + ", want string", "convert it with string(...)", true
		}
	case expr.ResultDyn:
	}
	return "", "", false
}

// firstMessage returns the message of the first Issue.
func firstMessage(iss []expr.Issue) string {
	if len(iss) == 0 {
		return "cel: compile failed"
	}
	return iss[0].Message
}
