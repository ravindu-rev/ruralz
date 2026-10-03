// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package executor runs a Route's compiled Filter Chain (snapshot.Chain)
// over one request's snapshot.RequestState (architecture R-42): the fixed
// Phases in the order the compiled chain gives (class, scope, position;
// response Phases reversed; onLog in request order), each Policy's
// spec.when once per request (per leg for Upstream-scope Policies), the
// auth rule 1 (every auth-class Policy skipped is 401 RZ-AUTH-001),
// Result handling, the failureMode table by Filter class and Phase, the
// short-circuit rules, consumptive batching of admission Filters into
// shared State Store round trips, onLog, Finish, and the per-leg hooks
// the Upstream layer calls (snapshot.LegHooks and snapshot.LegRun).
//
// A request uses one Run: Executor.Begin, Run.Request for onRequestHeaders,
// onRequestBody and onRoute, the Run as snapshot.Outbound.Hooks while the
// Forwarder runs its legs, Run.Response for onResponse, Run.Log for onLog
// and Finish, then Run.Release. Generated responses carry an RZ code and no
// body; the handler writes their problem documents.
//
// The executor records the ruralz_filter_* metrics on the request's
// stripe and starts one ruralz.filter.<name> span per Policy Phase call of
// a sampled request (none in onLog); RequestState.RecordShortCircuit and
// RecordFailure only feed the access record and /tap. Every Filter call is
// preceded by RequestState.Enter for its Policy (Handle and spec.when with
// the Phase being run, Prepare, Complete and Undo with onRequestHeaders,
// Finish with onLog), so the Exchange's Message, Vars, PolicyState and
// Annotate refer to the Policy being run, and a panic in a Filter is
// recovered as cannot decide (spec 04 req 43).
//
// Specs: 03 req 43 (Policy.spec.when runtime errors) and section 9 item
// 18; 04 group F reqs 39 to 45; 05 req 33 (Filter retry) and reqs 64 and
// 91 (consumptive calls); 06 section 2.1 rules 1 and 11; 07 group B reqs
// 10 to 19 and req 86; 08 section 2.3 reqs 27 to 29 and section 2.4 reqs
// 36 to 38.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Registered codes the executor generates (spec 04 req 43, spec 07 reqs 14
// and 15, spec 06 rule 1, OQ-data-plane-8). Statuses are looked up once in
// New through errcode.Status, never written as literals.
const (
	// CodeAuthSkipped is Security rule 1: every auth-class Policy skipped.
	CodeAuthSkipped = "RZ-AUTH-001"
	// CodeAuthUndecided is the auth class default when an auth Filter
	// cannot decide without supplying a code (a recovered panic).
	CodeAuthUndecided = "RZ-AUTH-002"
	// CodeAuthzUndecided is the authz class default.
	CodeAuthzUndecided = "RZ-AUTH-015"
	// CodeUpstreamAuth is the upstream-auth class code (OQ-data-plane-8 (a)).
	CodeUpstreamAuth = "RZ-AUTH-020"
	// CodeRequestTooLarge is filter.ErrTooLarge on the request side.
	CodeRequestTooLarge = "RZ-RT-003"
	// CodeBudget is filter.ErrBudget in any Phase before commit.
	CodeBudget = "RZ-RT-004"
	// CodeUndecided is the request-Phase default of the classes without a
	// registered code of their own.
	CodeUndecided = "RZ-RT-011"
	// CodeResponseFailed is the response-Phase default.
	CodeResponseFailed = "RZ-RT-012"
	// CodeStepFailed is filter.ErrTooLarge on a composition step's leg.
	CodeStepFailed = "RZ-RT-015"
	// CodeResponseTooLarge is filter.ErrTooLarge on a plain upstreams response.
	CodeResponseTooLarge = "RZ-UP-010"
	// CodePluginFault is the custom class default (M2 Plugins).
	CodePluginFault = "RZ-PLG-001"
)

// Error types put on a span as error.type (never request data): the
// executor's own words, and the fixed set a Filter may name through an
// ErrorType method (spec 07 req 86).
const (
	errTypeInternal  = "internal"
	errTypeCEL       = "cel_error"
	errTypeState     = "state_store"
	errTypeBudget    = "buffer_budget"
	errTypeTooLarge  = "too_large"
	errTypeBadResult = "invalid_result"

	errTypeInvalidValue = "invalid_value"
	errTypeNullBody     = "null_body"
	errTypePath         = "path_error"
	errTypeOutputCap    = "output_cap"
	errTypeResultType   = "result_type"
)

// Sentinel causes the executor records as a Result.Err.
var (
	// ErrPanic is the cause of cannot decide after a recovered Filter panic.
	ErrPanic = errors.New("executor: filter panicked")
	// ErrInvalidResult is the cause of cannot decide for an outcome the
	// Phase does not allow: Respond outside a request Phase, or without a
	// Response with a final status (200 to 599), Retry outside
	// onUpstreamResponseHeaders (R-44), or an unknown Outcome.
	ErrInvalidResult = errors.New("executor: outcome not allowed in this phase")
)

// Challenger is implemented by auth-class Filters that answer a missing
// credential with a WWW-Authenticate challenge (spec 06 rule 8). The 401
// RZ-AUTH-001 of Security rule 1 carries the challenge of every auth-class
// Policy in the chain.
type Challenger interface {
	// Challenge returns the WWW-Authenticate field value, "" for none.
	Challenge() string
}

// Deps are the executor's Node-wide dependencies.
type Deps struct {
	// Clock times Filter calls (ruralz_filter_duration_seconds); nil means
	// clock.Real().
	Clock clock.Clock
	// Tracer starts the ruralz.filter.<name> spans; nil starts none. It
	// returns no-op spans for unsampled requests.
	Tracer emit.Tracer
	// Logger comes from internal/telemetry; it records recovered panics
	// and, at debug level, the cause of every cannot decide. nil discards.
	Logger *slog.Logger
}

// classRule is one row of the failureMode table (spec 04 req 43).
type classRule struct {
	reqStatus  int
	reqCode    string
	respStatus int
	respCode   string
	// respSkip: a response-Phase failure never replaces the response (the
	// cache store is skipped under either failureMode).
	respSkip bool
}

// Executor runs Filter Chains. One Executor serves every snapshot of the
// process; it is safe for concurrent use.
type Executor struct {
	clock  clock.Clock
	tracer emit.Tracer
	logger *slog.Logger

	rules [phase.NumClasses]classRule
	// Statuses of the codes the executor generates outside the class table.
	authSkippedStatus   int
	budgetStatus        int
	requestLargeStatus  int
	responseLargeStatus int
	stepFailedStatus    int
	// opNames are the State Store op labels by emit.StateOp* index.
	opNames [emit.NumStateOps]string

	runs sync.Pool // *Run
	legs sync.Pool // *legRun
}

// New returns an Executor.
func New(d Deps) *Executor {
	e := &Executor{clock: d.Clock, tracer: d.Tracer, logger: d.Logger}
	if e.clock == nil {
		e.clock = clock.Real()
	}
	if e.logger == nil {
		e.logger = slog.New(slog.DiscardHandler)
	}
	undecided := statusOf(CodeUndecided, http.StatusServiceUnavailable)
	failed := statusOf(CodeResponseFailed, http.StatusBadGateway)
	row := func(reqStatus int, reqCode string) classRule {
		return classRule{reqStatus: reqStatus, reqCode: reqCode, respStatus: failed, respCode: CodeResponseFailed}
	}
	e.rules[phase.ClassCORS] = row(undecided, CodeUndecided)
	e.rules[phase.ClassAuth] = row(statusOf(CodeAuthUndecided, http.StatusUnauthorized), CodeAuthUndecided)
	e.rules[phase.ClassAuthz] = row(statusOf(CodeAuthzUndecided, http.StatusForbidden), CodeAuthzUndecided)
	e.rules[phase.ClassAdmission] = row(undecided, CodeUndecided)
	e.rules[phase.ClassValidation] = row(undecided, CodeUndecided)
	e.rules[phase.ClassCache] = row(undecided, CodeUndecided)
	e.rules[phase.ClassCache].respSkip = true
	e.rules[phase.ClassUpstreamAuth] = row(statusOf(CodeUpstreamAuth, http.StatusUnauthorized), CodeUpstreamAuth)
	e.rules[phase.ClassTransform] = row(undecided, CodeUndecided)
	e.rules[phase.ClassCustom] = classRule{reqStatus: undecided, reqCode: CodePluginFault, respStatus: failed, respCode: CodePluginFault}
	e.authSkippedStatus = statusOf(CodeAuthSkipped, http.StatusUnauthorized)
	e.budgetStatus = statusOf(CodeBudget, http.StatusServiceUnavailable)
	e.requestLargeStatus = statusOf(CodeRequestTooLarge, http.StatusRequestEntityTooLarge)
	e.responseLargeStatus = statusOf(CodeResponseTooLarge, http.StatusBadGateway)
	e.stepFailedStatus = statusOf(CodeStepFailed, http.StatusBadGateway)
	for i, op := range catalog.Ops() {
		if i < len(e.opNames) {
			e.opNames[i] = op
		}
	}
	return e
}

// statusOf returns the registered status of code, or fallback when the
// registry fixes none (RZ-PLG codes).
func statusOf(code string, fallback int) int {
	if s := errcode.Status(code); s != 0 {
		return s
	}
	return fallback
}

// rule returns the failureMode row of class c; an unknown class takes the
// custom row.
func (e *Executor) rule(c phase.Class) classRule {
	if c < phase.NumClasses {
		return e.rules[c]
	}
	return e.rules[phase.ClassCustom]
}

// tooLarge maps filter.ErrTooLarge in ph (spec 07 req 15): the request side
// is 413 RZ-RT-003, a plain upstreams response 502 RZ-UP-010 and any Phase
// of a composition step's leg 502 RZ-RT-015.
func (e *Executor) tooLarge(ph phase.Phase, step bool) (int, string) {
	switch {
	case step && ph.UpstreamLeg():
		return e.stepFailedStatus, CodeStepFailed
	case ph.CanShortCircuit():
		return e.requestLargeStatus, CodeRequestTooLarge
	default:
		return e.responseLargeStatus, CodeResponseTooLarge
	}
}

// opName returns the ruralz.state.op value of a round trip.
func (e *Executor) opName(c *statestore.Call) string {
	if i := c.Trip.Label(c.Kind); i >= 0 && i < len(e.opNames) {
		return e.opNames[i]
	}
	return ""
}

// startSpan starts p's span for a call in ph; onLog creates none.
func (e *Executor) startSpan(ctx context.Context, p *snapshot.Policy, ph phase.Phase) (context.Context, emit.Span) {
	if e.tracer == nil || ph == phase.OnLog {
		return ctx, nil
	}
	return e.tracer.StartFilter(ctx, p.SpanName, emit.FilterAttrs{PolicyType: string(p.Type), Phase: ph})
}

// handle calls p's Filter, recovering a panic as cannot decide.
func (e *Executor) handle(ctx context.Context, p *snapshot.Policy, ph phase.Phase, x filter.Exchange) (r filter.Result) {
	defer func() {
		if v := recover(); v != nil {
			e.logPanic(ctx, p, ph, v)
			r = filter.Undecided("", ErrPanic)
		}
	}()
	return p.Filter.Handle(ctx, ph, x)
}

// prepare calls Consumptive.Prepare, recovering a panic as cannot decide.
func (e *Executor) prepare(ctx context.Context, p *snapshot.Policy, c filter.Consumptive, x filter.Exchange, call *statestore.Call) (r filter.Result, done bool) {
	defer func() {
		if v := recover(); v != nil {
			e.logPanic(ctx, p, phase.OnRequestHeaders, v)
			r, done = filter.Undecided("", ErrPanic), true
		}
	}()
	return c.Prepare(ctx, x, call)
}

// complete calls Consumptive.Complete, recovering a panic as cannot decide.
func (e *Executor) complete(ctx context.Context, p *snapshot.Policy, c filter.Consumptive, x filter.Exchange, call *statestore.Call) (r filter.Result) {
	defer func() {
		if v := recover(); v != nil {
			e.logPanic(ctx, p, phase.OnRequestHeaders, v)
			r = filter.Undecided("", ErrPanic)
		}
	}()
	return c.Complete(ctx, x, call)
}

// undo calls Consumptive.Undo, recovering a panic.
func (e *Executor) undo(ctx context.Context, p *snapshot.Policy, c filter.Consumptive, x filter.Exchange) {
	defer func() {
		if v := recover(); v != nil {
			e.logPanic(ctx, p, phase.OnRequestHeaders, v)
		}
	}()
	c.Undo(x)
}

// finish calls Finisher.Finish, recovering a panic.
func (e *Executor) finish(ctx context.Context, p *snapshot.Policy, f filter.Finisher, x filter.Exchange) {
	defer func() {
		if v := recover(); v != nil {
			e.logPanic(ctx, p, phase.OnLog, v)
		}
	}()
	f.Finish(ctx, x)
}

// evalWhen evaluates spec.when on p's first encounter in ph, recovering a
// panic as a runtime error; the log names ph, the Phase p is first reached
// in, which is not p.FirstPhase when an earlier Phase never ran.
func (e *Executor) evalWhen(ctx context.Context, p *snapshot.Policy, ph phase.Phase, v *expr.Vars) (ok bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			e.logPanic(ctx, p, ph, r)
			ok, err = false, ErrPanic
		}
	}()
	return p.When.EvalBool(ctx, v)
}

// logPanic records a recovered panic: the Policy, the Phase, the panic
// value's type (never its text, which may hold request data) and the stack.
func (e *Executor) logPanic(ctx context.Context, p *snapshot.Policy, ph phase.Phase, v any) {
	e.logger.LogAttrs(ctx, slog.LevelError, "filter panic recovered",
		slog.String(catalog.KeyPolicy, p.Name),
		slog.String("phase", ph.String()),
		slog.String("panic_type", fmt.Sprintf("%T", v)),
		slog.String("stack", string(debug.Stack())))
}

// logUndecided records the cause of a cannot decide at debug level.
func (e *Executor) logUndecided(ctx context.Context, p *snapshot.Policy, ph phase.Phase, errType string, err error) {
	if !e.logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	e.logger.LogAttrs(ctx, slog.LevelDebug, "filter could not decide",
		slog.String(catalog.KeyPolicy, p.Name),
		slog.String("phase", ph.String()),
		slog.String("error_type", errType),
		slog.Any(catalog.KeyError, err))
}

// errorType classifies a cannot-decide cause for the span's error.type.
func errorType(err error) string {
	var (
		eval  *expr.EvalError
		state *statestore.Error
		typed interface{ ErrorType() string }
	)
	switch {
	case err == nil:
		return ""
	case errors.As(err, &typed):
		if t := typed.ErrorType(); knownErrorType(t) {
			return t
		}
		return errTypeInternal
	case errors.Is(err, ErrPanic):
		return errTypeInternal
	case errors.Is(err, ErrInvalidResult):
		return errTypeBadResult
	case errors.Is(err, filter.ErrBudget):
		return errTypeBudget
	case errors.Is(err, filter.ErrTooLarge):
		return errTypeTooLarge
	case errors.As(err, &eval):
		return errTypeCEL
	case errors.As(err, &state):
		return errTypeState
	default:
		return errTypeInternal
	}
}

// knownErrorType reports whether a Filter-supplied error.type is in the
// fixed vocabulary (spec 07 req 86, spec 06 rule 11); anything else, which
// may hold request data, is replaced with "internal".
func knownErrorType(t string) bool {
	switch t {
	case errTypeInternal, errTypeCEL, errTypeState, errTypeBudget, errTypeTooLarge, errTypeBadResult,
		errTypeInvalidValue, errTypeNullBody, errTypePath, errTypeOutputCap, errTypeResultType:
		return true
	default:
		return false
	}
}

// modeOf returns the failureMode applied to p as an emit.Mode* index. The
// security classes auth, authz and upstream-auth are closed whatever
// p.FailureMode says (data plane "Security types are closed only
// (RZ-CFG-029)", spec 06 rule 1): validation rejects open there, and this
// keeps an auth Filter that cannot decide, or whose spec.when errs, from
// being skipped should open ever get through. Otherwise anything but open
// is closed.
func modeOf(p *snapshot.Policy) int {
	switch {
	case closedOnly(p.Class):
		return emit.ModeClosed
	case p.FailureMode == v1alpha1.FailureModeOpen:
		return emit.ModeOpen
	default:
		return emit.ModeClosed
	}
}

// closedOnly reports whether Filter class c is closed only.
func closedOnly(c phase.Class) bool {
	switch c {
	case phase.ClassAuth, phase.ClassAuthz, phase.ClassUpstreamAuth:
		return true
	default:
		return false
	}
}

// modeValue maps an emit.Mode* index to the configuration value.
func modeValue(m int) v1alpha1.FailureMode {
	if m == emit.ModeOpen {
		return v1alpha1.FailureModeOpen
	}
	return v1alpha1.FailureModeClosed
}

// nanos converts a measured duration to histogram nanoseconds.
func nanos(d time.Duration) uint64 {
	if d <= 0 {
		return 0
	}
	return uint64(d.Nanoseconds()) //nolint:gosec // G115: d is positive, checked above.
}
