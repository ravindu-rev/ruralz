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
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/expr"
)

// comprehensionStop bounds the evaluation of a tracked program holding a
// comprehension, with the request deadline (ADR-0019, target).
const comprehensionStop = 50 * time.Millisecond

// Compiler is the expr.Compiler on cel-go: it owns one environment per set
// of declared variables, the compiled defaults of retryOn and failureWhen,
// and the comprehension stops its programs share. It is safe for
// concurrent use; a process builds one with New.
type Compiler struct {
	provider *celtypes.Provider
	// stop is comprehensionStop; tests shorten it.
	stop time.Duration
	// stops times the comprehension stops of every program planned here.
	stops *stopPool

	mu   sync.Mutex
	envs map[expr.Var]*celgo.Env

	// defaults is read-only after New.
	defaults map[expr.PlaceID]*Program
}

// Compile-time check of the contract.
var _ expr.Compiler = (*Compiler)(nil)

// New returns a Compiler with the defaults of retryOn and failureWhen
// compiled, timing comprehension stops on the wall clock: for validation,
// whose programs never evaluate a request.
func New() (*Compiler, error) { return NewWithClock(clock.Real()) }

// NewWithClock is New with the clock that times comprehension stops.
// ruralzd passes its process clock, as for every Node deadline, so the
// controllable-clock test build stops a comprehension only when the
// harness moves the clock past the stop
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope").
func NewWithClock(clk clock.Clock) (*Compiler, error) {
	p, err := celtypes.NewProvider()
	if err != nil {
		return nil, fmt.Errorf("cel: %w", err)
	}
	c := &Compiler{
		provider: p,
		stop:     comprehensionStop,
		stops:    newStopPool(clk),
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
		prg, iss := c.program(expr.Site{Place: d.place}, d.src, expr.InputCaps{})
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

// References returns what src reads when checked at site, without
// planning a program: the variables, whether it selects request.body or
// response.body, and the steps it names (expr.Refs). It serves decisions
// taken before Filters are built, such as whether a transform's CEL field
// reads the body it acts on, which makes the Policy a gate
// (docs/architecture/03-data-plane.md "Transform Policies"). A source that
// does not parse or type-check at site is an error; the site's other
// findings, such as its result type, are Check's.
func (c *Compiler) References(site expr.Site, src string) (expr.Refs, error) {
	se, ok := resolve(site)
	if !ok {
		return expr.Refs{}, fmt.Errorf("cel: %s is not a CEL place", site.Place)
	}
	cc, iss := c.checked(se, src)
	if cc == nil {
		return expr.Refs{}, errors.New(firstMessage(iss))
	}
	return cc.an.refs, nil
}

// NewBuilder starts a snapshot's program set under the snapshot's input
// caps, which decide each program's cost tracking (cappedEstimator). prev,
// the active snapshot's set, lends its programs to identical sites,
// sources and caps; nil is valid.
func (c *Compiler) NewBuilder(prev expr.ProgramSet, caps expr.InputCaps) expr.Builder {
	b := &Builder{c: c, caps: caps, entries: map[programKey]*buildEntry{}}
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
		celgo.Lib(urlLibrary{}),
		celgo.Lib(netLibrary{}),
		// After ext.Strings, so these replace its split estimates.
		celgo.CostEstimatorOptions(splitEstimates()...),
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
	env   *celgo.Env
	ast   *celgo.Ast
	an    *analysis
	facts *costFacts
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
	return &checkedExpr{env: env, ast: a, an: analyze(a.NativeRep(), se), facts: collectCostFacts(a.NativeRep())}, nil
}

// compiled is a source that passed every check at its site.
type compiled struct {
	checkedExpr
	se   siteEnv
	src  string
	cost expr.CostRange
}

// tracked reports whether cc can pass the runtime cost limit with its
// inputs at caps, so it evaluates under cost tracking: its bound under
// the capped estimator, which leaves every uncapped input unbounded,
// exceeds the limit (docs/architecture/02-configuration-model.md
// "Limits").
func (cc *compiled) tracked(caps expr.InputCaps) (bool, error) {
	bound, err := cc.cappedBound(caps)
	if err != nil {
		return false, err
	}
	return bound > runtimeCostLimit, nil
}

// cappedBound returns the most an evaluation of cc can cost with its
// inputs within caps: cel-go's estimate under the capped estimator plus
// the selections it does not charge (costFacts.bound).
func (cc *compiled) cappedBound(caps expr.InputCaps) (uint64, error) {
	e := cappedEstimator{caps: caps, clientRequest: cc.se.clientRequest, paths: cc.facts.paths}
	est, err := cc.env.EstimateCost(cc.ast, e)
	if err != nil {
		return 0, err
	}
	return cc.facts.bound(cc.ast.NativeRep(), est, e), nil
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
	if se.badResult {
		return nil, []expr.Issue{wholeIssue(codeInvalid, se,
			"the site's result type does not narrow the place's "+resultTypeName(se.place.Result)+" result", "")}
	}
	cc, iss := c.checked(se, src)
	if cc == nil {
		return nil, iss
	}
	iss = findingIssues(se, src, cc.ast.NativeRep(), cc.an.findings)
	if msg, hint, bad := resultMismatch(se.result, cc.ast.OutputType()); bad {
		iss = append(iss, wholeIssue(codeInvalid, se, msg, hint))
	}
	nominal := nominalEstimator{containers: cc.an.containers}
	est, err := cc.env.EstimateCost(cc.ast, nominal)
	if err != nil {
		return nil, append(iss, wholeIssue(codeCost, se, "cost estimate: "+err.Error(), ""))
	}
	est.Max = cc.facts.bound(cc.ast.NativeRep(), est, nominal)
	if msg, hint, bad := costFinding(est, cc.an); bad {
		iss = append(iss, wholeIssue(codeCost, se, msg, hint))
	}
	if len(iss) > 0 {
		return nil, iss
	}
	return &compiled{
		checkedExpr: *cc,
		se:          se,
		src:         src,
		cost:        expr.CostRange{Min: est.Min, Max: est.Max},
	}, nil
}

// program compiles src at site and plans its program, tracked unless its
// cost with its inputs at caps stays within the runtime limit.
func (c *Compiler) program(site expr.Site, src string, caps expr.InputCaps) (*Program, []expr.Issue) {
	cc, iss := c.compile(site, src)
	if cc == nil {
		return nil, iss
	}
	tracked, err := cc.tracked(caps)
	if err != nil {
		return nil, []expr.Issue{wholeIssue(codeCost, cc.se, "cost estimate: "+err.Error(), "")}
	}
	prg, err := c.plan(cc, tracked)
	if err != nil {
		return nil, []expr.Issue{wholeIssue(codeInvalid, cc.se, err.Error(), "")}
	}
	return prg, nil
}

// resultMismatch reports an output type the place cannot take: a bool
// place takes bool or dyn, a string place string or dyn, an int place int
// or dyn, a dyn place any.
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
	case expr.ResultInt:
		if got.Kind() != types.IntKind {
			return "expression has type " + got.String() + ", want int", "convert it with int(...)", true
		}
	case expr.ResultDyn:
	}
	return "", "", false
}

// resultTypeName spells a result type as x-ruralz-cel does.
func resultTypeName(r expr.Result) string {
	switch r {
	case expr.ResultBool:
		return "bool"
	case expr.ResultString:
		return "string"
	case expr.ResultDyn:
		return "dyn"
	default:
		return "unknown"
	}
}

// firstMessage returns the message of the first Issue.
func firstMessage(iss []expr.Issue) string {
	if len(iss) == 0 {
		return "cel: compile failed"
	}
	return iss[0].Message
}
