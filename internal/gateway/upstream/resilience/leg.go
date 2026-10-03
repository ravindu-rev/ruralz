// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"math/rand/v2"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Runtime is the resilience state of one Upstream on one Node: its
// circuit breaker, bulkhead and retry budget. It lives for the Node's
// lifetime and is carried across Hot Reloads by Upstream metadata.name
// (05 req 2); every leg passes its own snapshot's Config.
type Runtime struct {
	clk      clock.Clock
	src      rand.Source
	breaker  *Breaker
	bulkhead Bulkhead
	budget   RetryBudget
}

// NewRuntime returns the runtime of one Upstream. clk supplies time; src
// the backoff and open-jitter draws, and must be safe for concurrent use
// because legs draw from their own goroutines (nil means the math/rand/v2
// global source); onBreakerChange is the Breaker's change callback (see
// NewBreaker).
func NewRuntime(clk clock.Clock, src rand.Source, onBreakerChange func(from, to State)) *Runtime {
	return &Runtime{clk: clk, src: src, breaker: NewBreaker(clk, src, onBreakerChange)}
}

// Breaker returns the Upstream's circuit breaker.
func (rt *Runtime) Breaker() *Breaker { return rt.breaker }

// Bulkhead returns the Upstream's in-flight ceiling.
func (rt *Runtime) Bulkhead() *Bulkhead { return &rt.bulkhead }

// Budget returns the Upstream's retry budget.
func (rt *Runtime) Budget() *RetryBudget { return &rt.budget }

// Clock returns the runtime's clock.
func (rt *Runtime) Clock() clock.Clock { return rt.clk }

// Attempt is the timing of one attempt, returned by Leg.StartAttempt.
type Attempt struct {
	// Number counts attempts from 1 (the attempt CEL variable).
	Number int
	// Start is when the attempt started.
	Start time.Time
	// PerTry is the attempt's timeout: retries.perTryTimeout, or the leg
	// time left divided by the retries left plus one; 0 when the leg
	// deadline alone bounds the attempt.
	PerTry time.Duration
	// Deadline is the attempt deadline, min(leg deadline, Start + PerTry);
	// zero for no bound.
	Deadline time.Time
	// Cause is the context cause WithDeadline should use: ErrLegTimeout
	// when the leg deadline is the binding one, else ErrAttemptTimeout.
	Cause error
}

// Reason says why a retry decision did or did not retry.
type Reason uint8

// Retry decision reasons, in the order Leg.Decide checks them.
const (
	// ReasonRetry: every condition holds; the leg retries.
	ReasonRetry Reason = iota
	// ReasonNotWanted: retryOn was false (or failed at run time) and no
	// Filter asked for a retry.
	ReasonNotWanted
	// ReasonCommitted: something already reached the client.
	ReasonCommitted
	// ReasonNotReplayable: the request body cannot be sent again.
	ReasonNotReplayable
	// ReasonAttempts: no retries left.
	ReasonAttempts
	// ReasonRetryAfter: the Upstream's Retry-After is over MaxRetryAfter.
	ReasonRetryAfter
	// ReasonDeadline: the leg deadline would pass during the backoff (or
	// the Retry-After delay).
	ReasonDeadline
	// ReasonBreakerOpen: the Upstream's breaker is open, or half-open and
	// the leg is not its current probe (Breaker.Admits).
	ReasonBreakerOpen
	// ReasonBudget: the retry budget is exhausted; the caller counts
	// ruralz_upstream_retry_budget_exhausted_total and the leg returns its
	// original failure (05 req 34).
	ReasonBudget
)

// String returns a lowercase name for logs and tests.
func (r Reason) String() string {
	switch r {
	case ReasonRetry:
		return "retry"
	case ReasonNotWanted:
		return "not_wanted"
	case ReasonCommitted:
		return "committed"
	case ReasonNotReplayable:
		return "not_replayable"
	case ReasonAttempts:
		return "attempts"
	case ReasonRetryAfter:
		return "retry_after"
	case ReasonDeadline:
		return "deadline"
	case ReasonBreakerOpen:
		return "breaker_open"
	case ReasonBudget:
		return "budget"
	default:
		return "unknown"
	}
}

// RetryInput is what the Upstream core knows after one attempt, the input
// of Leg.Decide (05 reqs 32 and 33).
type RetryInput struct {
	// RetryOn is retryOn's result over the attempt (EvalRetryOn; false
	// after a runtime error).
	RetryOn bool
	// FilterRetry is true when an onUpstreamResponseHeaders Filter returned
	// filter.Retry; retryOn is then not consulted (RetryOn is ignored) but
	// every other condition applies.
	FilterRetry bool
	// Replayable is RequestBody.Replayable(): the body is empty, gated, or
	// streamed with no byte read yet (05 req 31).
	Replayable bool
	// Committed is true once anything of the response reached the client.
	Committed bool
	// RetryAfter is the attempt's Retry-After response field value; "" when
	// absent or when the attempt got no response.
	RetryAfter string
}

// Decision is the result of Leg.Decide.
type Decision struct {
	// Retry is true when the leg retries after Delay.
	Retry bool
	// Delay is the sleep before the retry: the full-jitter backoff, or the
	// Retry-After delay when longer.
	Delay time.Duration
	// Reason is ReasonRetry, or the first condition that failed.
	Reason Reason
}

// Leg is the resilience bookkeeping of one Upstream leg (05 groups E and
// G): its deadlines, attempt count, breaker permit and retry budget units.
// The zero value is an idle leg; Begin starts it and End returns it to
// idle, so a Leg can live in pooled per-request memory. A Leg belongs to
// the leg's goroutine.
//
// The Upstream core drives it as:
//
//	if g := leg.Begin(rt, cfg, routeDeadline, len(endpoints)); g != GateNone { return g.Err() }
//	defer leg.End(LegNotCounted) // a no-op once the leg ended below
//	for {
//		a, g := leg.StartAttempt() // GateBreaker: the breaker refused this attempt
//		if g != GateNone { break }
//		// Endpoint set empty, or bulkhead full: leg.AbandonAttempt(gate); break
//		ctx, d := WithDeadline(legCtx, rt.Clock(), a.Deadline, a.Cause)
//		// Endpoint, onUpstreamRequest, RoundTrip; d.Stop() at headers
//		// on error: ClassifyStage; Canceled(ctx): break, no accounting
//		// EvalFailureWhen, onUpstreamResponseHeaders, EvalRetryOn
//		dec := leg.Decide(in)
//		if !dec.Retry { break }
//		// drain and close the response; sleep dec.Delay on the clock
//	}
//	// responded, kind and failed describe the last attempt that ran
//	leg.End(BreakerResult(failed, leg.Outcome(responded, kind), ejected, endpoints))
//
// The breaker gate of 05 req 11 step e applies to every attempt: Begin
// admits the leg, Decide refuses a retry the breaker already refuses
// before the delay, and StartAttempt checks again when the attempt starts,
// because the breaker can open (or go half-open with another leg as its
// probe) during a backoff or Retry-After delay of up to MaxRetryAfter. A
// retry attempt that cannot start (breaker open or half-open and the leg
// not its probe, Endpoint set empty, bulkhead full) ends the leg with the
// previous attempt's outcome (05 req 35), and Outcome reports the refusing
// gate for a leg whose first attempt could not start.
//
// The Endpoint set is checked before the breaker (05 req 11: step a, then
// step e), which Begin does from its endpoints argument, so an empty set
// gives RZ-UP-008 even with the breaker open and never spends the half-open
// probe. An attempt whose context ended without a deadline expiry (the
// client went away, or a sibling composition step canceled the shared
// context; Canceled) is not an Upstream failure: the core skips
// failureWhen, passive ejection and the retry decision for it and ends the
// leg with End(LegNotCounted).
type Leg struct {
	rt       *Runtime
	cfg      *Config
	permit   Permit
	start    time.Time
	deadline time.Time
	attempts int
	retries  int
	// gate is the gate that refused the leg's last attempt that could not
	// start; GateNone when none.
	gate Gate
	// pending is true while the attempt StartAttempt last counted may still
	// be abandoned.
	pending bool
	active  bool
}

// Begin starts a leg of an Upstream with runtime rt and the caller's
// snapshot configuration cfg, bounded by the Route deadline route (zero:
// none); endpoints is the size of the Upstream's Endpoint set. The gates
// run in the order of 05 req 11: GateNoEndpoint when endpoints is 0 (503
// RZ-UP-008), then the breaker: GateBreaker means it is open or half-open
// with its probe in flight (503 RZ-UP-005). A gated leg did not start: no
// retry, not counted, and End is a no-op. With GateNone the leg counts as
// an in-flight original of the retry budget until End, and when it is the
// half-open probe its probe slot is leased until its leg deadline plus
// openDuration (Breaker.AllowLease). cfg must stay unchanged until End.
// Begin on a Leg that is still active first ends it with LegNotCounted, so
// a pooled Leg reused without End frees its probe slot and budget units.
func (l *Leg) Begin(rt *Runtime, cfg *Config, route time.Time, endpoints int) Gate {
	if l.active {
		l.End(LegNotCounted)
	}
	*l = Leg{}
	if endpoints <= 0 {
		return GateNoEndpoint
	}
	now := rt.clk.Now()
	deadline := LegDeadline(route, now, cfg.Timeout)
	permit, ok := rt.breaker.AllowLease(&cfg.Breaker, deadline)
	if !ok {
		return GateBreaker
	}
	*l = Leg{
		rt:       rt,
		cfg:      cfg,
		permit:   permit,
		start:    now,
		deadline: deadline,
		active:   true,
	}
	rt.budget.BeginLeg()
	return GateNone
}

// StartAttempt starts the leg's next attempt. It first applies the breaker
// half of the per-attempt gate (05 req 11 step e): when the breaker does
// not admit the leg (Breaker.Admits: it is open, or half-open and the leg
// is not its current probe, for example because the breaker opened during
// the retry delay or another leg took over a lost probe's lease), it counts
// no attempt and returns GateBreaker. The leg then ends with the previous
// attempt's outcome (05 reqs 35 and 37: no retry while open, one probe at
// a time), or with RZ-UP-005 when no attempt ran, which Outcome reports.
// Otherwise it counts the attempt and returns GateNone with its timing
// (05 reqs 4 and 24): the per-try timeout is computed now, from the leg
// time left and the retries left after this attempt.
func (l *Leg) StartAttempt() (Attempt, Gate) {
	if !l.rt.breaker.Admits(l.permit) {
		l.gate = GateBreaker
		l.pending = false
		return Attempt{}, GateBreaker
	}
	l.attempts++
	l.pending = true
	now := l.rt.clk.Now()
	retriesLeft := l.cfg.Retry.Attempts - (l.attempts - 1)
	perTry := PerTryTimeout(l.cfg.Retry.PerTryTimeout, l.deadline, now, retriesLeft)
	deadline := AttemptDeadline(l.deadline, now, perTry)
	cause := ErrAttemptTimeout
	if !l.deadline.IsZero() && deadline.Equal(l.deadline) {
		cause = ErrLegTimeout
	}
	return Attempt{Number: l.attempts, Start: now, PerTry: perTry, Deadline: deadline, Cause: cause}, GateNone
}

// AbandonAttempt uncounts the attempt StartAttempt last counted when one
// of the Upstream core's own gates refused it before it ran: g is
// GateNoEndpoint for an empty Endpoint set or GateBulkhead for a full
// bulkhead (05 req 11 steps a and e). The breaker half of step e is
// StartAttempt's own: an attempt it refused was not counted, and
// AbandonAttempt after it, after Decide or a second time changes nothing.
// The leg then ends with the previous attempt's outcome (05 req 35), or
// with g's code when no attempt ran, which Outcome reports.
func (l *Leg) AbandonAttempt(g Gate) {
	if !l.pending {
		return
	}
	l.pending = false
	l.attempts--
	if g != GateNone {
		l.gate = g
	}
}

// Decide applies the retry rule of 05 req 33 after an attempt: a retry
// needs retryOn true or a Filter request, and all of (1) a replayable body
// and nothing committed, (2) retries left and the leg deadline still ahead
// after the delay, (3) the breaker admitting another attempt of this leg
// (closed, or half-open with this leg its current probe: Breaker.Admits)
// and room in the retry budget, (4) any Retry-After at most MaxRetryAfter
// and ending before the leg deadline. The delay is Backoff for this retry
// with a fresh draw, or the Retry-After delay when longer. Every retry
// takes one retry budget unit, from its backoff start until End (05 req
// 34); the budget is checked last, so a retry refused for another reason
// takes nothing. A refused retry ends the leg with the attempt result it
// has (05 req 35). The breaker can change during the delay, so the retry
// attempt still passes StartAttempt's breaker gate when it starts.
func (l *Leg) Decide(in RetryInput) Decision {
	l.pending = false // the attempt ran: AbandonAttempt keeps it
	switch {
	case !l.active:
		return Decision{Reason: ReasonNotWanted}
	case !in.RetryOn && !in.FilterRetry:
		return Decision{Reason: ReasonNotWanted}
	case in.Committed:
		return Decision{Reason: ReasonCommitted}
	case !in.Replayable:
		return Decision{Reason: ReasonNotReplayable}
	case l.attempts > l.cfg.Retry.Attempts:
		return Decision{Reason: ReasonAttempts}
	}
	now := l.rt.clk.Now()
	delay := Backoff(l.attempts, draw(l.rt.src))
	if in.RetryAfter != "" {
		if ra, ok := ParseRetryAfter(in.RetryAfter, now); ok {
			if ra > MaxRetryAfter {
				return Decision{Reason: ReasonRetryAfter}
			}
			delay = max(delay, ra)
		}
	}
	if !l.deadline.IsZero() && !now.Add(delay).Before(l.deadline) {
		return Decision{Reason: ReasonDeadline}
	}
	if !l.rt.breaker.Admits(l.permit) {
		return Decision{Reason: ReasonBreakerOpen}
	}
	if !l.rt.budget.TryRetry() {
		return Decision{Reason: ReasonBudget}
	}
	l.retries++
	return Decision{Retry: true, Delay: delay, Reason: ReasonRetry}
}

// Outcome returns the leg's outcome for code selection: responded and
// kind describe the last attempt that ran, Attempts is the attempts that
// ran, Expired is true when the leg deadline has passed, and Gate is the
// gate that refused the last attempt that could not start (StartAttempt or
// AbandonAttempt), which decides the code only when no attempt ran.
func (l *Leg) Outcome(responded bool, kind Kind) Outcome {
	return Outcome{Responded: responded, Kind: kind, Attempts: l.attempts, Expired: l.Expired(), Gate: l.gate}
}

// Expired reports whether the leg deadline has passed.
func (l *Leg) Expired() bool {
	return l.active && !l.deadline.IsZero() && !l.rt.clk.Now().Before(l.deadline)
}

// End finishes the leg: it records r with the breaker (a probe that is not
// counted frees the probe slot), releases the leg's original and retries
// from the retry budget and returns the Leg to idle. It is a no-op on an
// idle Leg.
func (l *Leg) End(r LegResult) {
	if !l.active {
		return
	}
	l.rt.breaker.Record(&l.cfg.Breaker, l.permit, r)
	l.rt.budget.EndLeg(l.retries)
	*l = Leg{}
}

// Active reports whether the leg has begun and not ended.
func (l *Leg) Active() bool { return l.active }

// Attempts returns the attempts counted so far.
func (l *Leg) Attempts() int { return l.attempts }

// Deadline returns the leg deadline; zero when unbounded.
func (l *Leg) Deadline() time.Time { return l.deadline }

// Start returns when the leg began.
func (l *Leg) Start() time.Time { return l.start }

// Retries returns the retries Decide admitted, which is the retry budget
// units the leg holds until End.
func (l *Leg) Retries() int { return l.retries }

// Probe reports whether the leg is the breaker's half-open probe.
func (l *Leg) Probe() bool { return l.permit.probe }
