// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// verdict is what a Phase does after one Filter call.
type verdict uint8

const (
	// next runs the next Policy of the Phase.
	next verdict = iota
	// respond ends the Phase with a response: a short-circuit in a request
	// Phase (the leg's response in onUpstreamRequest), or the end of the
	// stream in onChunk.
	respond
	// replace replaces the response in a response Phase before commit.
	replace
	// retry asks the Upstream layer to retry the attempt
	// (onUpstreamResponseHeaders only, R-44).
	retry
)

// cursor is the executor's view of one RequestState: the client state of a
// Run or the leg state of a legRun. It belongs to the goroutine that owns
// the state.
type cursor struct {
	e      *Executor
	rs     snapshot.RequestState
	x      filter.Exchange
	stripe emit.Stripe
	// step is set on a composition step's leg (filter.ErrTooLarge mapping).
	step bool
	// track records admitted Policies in ran (legs: Finish order at End).
	track bool
	// failed lists Policies that failed open: skipped for every remaining
	// Phase of the request (the leg for a leg state), spec 04 req 43.
	failed []*snapshot.Policy
	ran    []*snapshot.Policy
}

// bind points c at rs.
func (c *cursor) bind(e *Executor, rs snapshot.RequestState) {
	c.e, c.rs, c.x = e, rs, rs.Exchange()
	c.stripe = c.x.Stripe()
}

// reset drops every reference so a pooled cursor retains nothing.
func (c *cursor) reset() {
	clear(c.failed)
	clear(c.ran)
	c.failed, c.ran = c.failed[:0], c.ran[:0]
	c.e, c.rs, c.x = nil, nil, nil
	c.stripe, c.step, c.track = 0, false, false
}

// isFailed reports whether p failed open earlier in this state.
func (c *cursor) isFailed(p *snapshot.Policy) bool {
	for _, f := range c.failed {
		if f == p {
			return true
		}
	}
	return false
}

// admit reports whether p runs now. On p's first encounter in this state
// it evaluates spec.when once and records the decision with SetWhen
// (spec 04 req 41): false skips p for the whole request (leg), a runtime
// error applies p's failureMode (whenFailed), and a Policy without when is
// recorded as running. A Policy that failed open never runs again. whenErr
// is the error.type of a spec.when runtime error that p runs after (under
// closed), for the span of the call that follows; "" otherwise.
func (c *cursor) admit(ctx context.Context, p *snapshot.Policy, ph phase.Phase) (run bool, whenErr string) {
	if len(c.failed) > 0 && c.isFailed(p) {
		return false, ""
	}
	decided, skip := c.rs.When(p)
	if decided {
		return !skip, ""
	}
	if p.When != nil {
		c.rs.Enter(p, ph, nil)
		ok, err := c.e.evalWhen(ctx, p, ph, c.x.Vars())
		if err != nil {
			skip, whenErr = c.whenFailed(ctx, p, ph, err)
		} else {
			skip = !ok
		}
	}
	c.rs.SetWhen(p, skip)
	if skip {
		return false, ""
	}
	if c.track {
		c.ran = append(c.ran, p)
	}
	return true, whenErr
}

// whenFailed applies p's failureMode to err, a runtime error of p's
// spec.when evaluated on its first encounter in ph (spec 03 req 43,
// Policy.spec.when row): closed runs p, open skips it for the request
// (leg). Either way it is a failure of p in ph, the Phase p is first
// reached in: RecordFailure feeds the access record's failure_modes and
// ruralz_filter_failures_total{mode} counts the applied mode (spec 03
// section 9 item 18). Under closed the span of p's call carries the
// returned error.type and the applied mode; under open, where no call
// follows, a span of its own records the skip. An auth-class Policy is
// closed only (modeOf), so an erroring when never counts as skipped for
// Security rule 1.
func (c *cursor) whenFailed(ctx context.Context, p *snapshot.Policy, ph phase.Phase, err error) (skip bool, errType string) {
	mode := modeOf(p)
	errType = errorType(err)
	c.rs.RecordFailure(p, ph, modeValue(mode))
	if m := p.Metrics; m != nil && m.Failures[ph][mode] != nil {
		m.Failures[ph][mode].Add(c.stripe, 1)
	}
	c.e.logUndecided(ctx, p, ph, errType, err)
	if mode == emit.ModeClosed {
		return false, errType
	}
	_, span := c.e.startSpan(ctx, p, ph)
	endSpan(span, emit.OutcomeSkipped, string(v1alpha1.FailureModeOpen), 0, "", errType)
	return true, ""
}

// invoke runs p's Filter for ph and settles its Result; gen says the
// response a response Phase acts on is already generated, and whenErr is
// admit's mark for the call's span.
func (c *cursor) invoke(ctx context.Context, p *snapshot.Policy, ph phase.Phase, gen bool, whenErr string) (verdict, *filter.Response) {
	sctx, span := c.e.startSpan(ctx, p, ph)
	c.rs.Enter(p, ph, span)
	t0 := c.e.clock.Now()
	r := c.e.handle(sctx, p, ph, c.x)
	return c.settle(ctx, p, ph, r, span, c.e.clock.Since(t0), gen, whenErr)
}

// settle applies r, the Result of p's call in ph that took d: it records
// the Filter metrics on the request's stripe, feeds the access record and
// ends the call's span (spec 04 req 44). whenErr, admit's mark, puts the
// spec.when error's error.type and the applied closed mode on a span the
// call itself does not fail (spec 03 req 43).
func (c *cursor) settle(ctx context.Context, p *snapshot.Policy, ph phase.Phase, r filter.Result, span emit.Span, d time.Duration, gen bool, whenErr string) (verdict, *filter.Response) {
	m := p.Metrics
	if m != nil && m.Duration[ph] != nil {
		m.Duration[ph].Record(c.stripe, nanos(d))
	}
	wm := whenMode(whenErr)
	switch {
	case r.Outcome == filter.Continue:
		endSpan(span, emit.OutcomeContinue, wm, 0, "", whenErr)
		return next, nil
	case r.Outcome == filter.Respond && ph.CanShortCircuit() && r.Response != nil && validStatus(r.Response.Status):
		status := r.Response.Status
		c.rs.RecordShortCircuit(p, ph, status)
		if m != nil && m.ShortCircuits[ph] != nil {
			m.ShortCircuits[ph].Inc(c.stripe, status)
		}
		endSpan(span, emit.OutcomeRespond, wm, status, r.Response.Code, whenErr)
		return respond, r.Response
	case r.Outcome == filter.Retry && ph == phase.OnUpstreamResponseHeaders:
		endSpan(span, emit.OutcomeContinue, wm, 0, "", whenErr)
		return retry, nil
	case r.Outcome != filter.CannotDecide:
		// Respond outside a request Phase or without a final status,
		// Retry outside onUpstreamResponseHeaders (R-44), an unknown
		// outcome.
		r = filter.Undecided(r.Code, ErrInvalidResult)
	}
	return c.fail(ctx, p, ph, r, span, gen)
}

// fail applies failureMode to a cannot decide (spec 04 req 43, spec 07
// reqs 14 to 17, spec 08 req 36): the SPI sentinels first (ErrBudget is
// 503 RZ-RT-004 and ErrTooLarge 413 RZ-RT-003, 502 RZ-UP-010 or 502
// RZ-RT-015, under either failureMode); onLog failures reach telemetry
// only; open skips p for the rest of the request; closed responds in a
// request Phase with the class status and the Filter's code or the class
// default, ends the stream in onChunk, and replaces an Upstream response
// in a response Phase with 502 (the cache store is skipped instead). A
// response that is already generated is never replaced again.
func (c *cursor) fail(ctx context.Context, p *snapshot.Policy, ph phase.Phase, r filter.Result, span emit.Span, gen bool) (verdict, *filter.Response) {
	var (
		mode     = modeOf(p)
		status   int
		code     = r.Code
		sentinel bool
		act      = next
	)
	switch {
	case r.Err == nil:
	case errors.Is(r.Err, filter.ErrBudget):
		sentinel, status, code = true, c.e.budgetStatus, CodeBudget
	case errors.Is(r.Err, filter.ErrTooLarge):
		sentinel = true
		status, code = c.e.tooLarge(ph, c.step)
	}
	if sentinel {
		mode = emit.ModeClosed
	}
	switch {
	case ph == phase.OnLog:
		// Telemetry only (FP 4, 8.10).
	case mode == emit.ModeOpen:
		c.failed = append(c.failed, p)
	case ph.CanShortCircuit():
		if !sentinel {
			rule := c.e.rule(p.Class)
			status = rule.reqStatus
			if code == "" {
				code = rule.reqCode
			}
		}
		act = respond
	case ph == phase.OnChunk:
		// After commit: closed ends the stream (M3 sources).
		if !sentinel {
			rule := c.e.rule(p.Class)
			if code == "" {
				code = rule.respCode
			}
		}
		act = respond
	default:
		// A response Phase before commit.
		if !sentinel {
			rule := c.e.rule(p.Class)
			if rule.respSkip {
				break
			}
			status = rule.respStatus
			if code == "" {
				code = rule.respCode
			}
		}
		if !gen {
			act = replace
		}
	}
	c.rs.RecordFailure(p, ph, modeValue(mode))
	if m := p.Metrics; m != nil && m.Failures[ph][mode] != nil {
		m.Failures[ph][mode].Add(c.stripe, 1)
	}
	errType := errorType(r.Err)
	c.e.logUndecided(ctx, p, ph, errType, r.Err)
	if act == next {
		endSpan(span, emit.OutcomeCannotDecide, string(modeValue(mode)), 0, r.Code, errType)
		return next, nil
	}
	endSpan(span, emit.OutcomeCannotDecide, string(modeValue(mode)), status, code, errType)
	return act, &filter.Response{Status: status, Code: code}
}

// validStatus reports a final HTTP status a Filter may respond with.
func validStatus(s int) bool { return s >= 200 && s <= 599 }

// whenMode is the failureMode a span carries for admit's mark whenErr:
// closed, the mode under which p runs after a spec.when error, or "".
func whenMode(whenErr string) string {
	if whenErr == "" {
		return ""
	}
	return string(v1alpha1.FailureModeClosed)
}

// endSpan sets the outcome attributes (spec 04 req 44: outcome and the
// applied failureMode; spec 06 rule 11: only the RZ code) and ends span.
func endSpan(span emit.Span, outcome, mode string, status int, code, errType string) {
	if span == nil {
		return
	}
	span.SetAttr(catalog.AttrOutcome, slog.StringValue(outcome))
	if mode != "" {
		span.SetAttr(catalog.AttrFailureMode, slog.StringValue(mode))
	}
	span.End(status, code, errType)
}

// finishRan calls Finish on every Policy of ps that ran in this state and
// implements filter.Finisher, in the order of ps (R-40).
func (c *cursor) finishRan(ctx context.Context, ps []*snapshot.Policy, skipUpstream bool) {
	for _, p := range ps {
		if skipUpstream && p.Scope == phase.ScopeUpstream {
			continue
		}
		f, ok := p.Filter.(filter.Finisher)
		if !ok {
			continue
		}
		if decided, skip := c.rs.When(p); !decided || skip {
			continue
		}
		c.rs.Enter(p, phase.OnLog, nil)
		c.e.finish(ctx, p, f, c.x)
	}
}
