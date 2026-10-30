// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Deadline arithmetic. The bounds nest, each clamping those below it:
//
//	Route deadline   = request start + Route timeout (15 s by default)
//	leg deadline     = min(Route deadline, leg start + Upstream timeout)
//	attempt deadline = min(leg deadline, attempt start + perTryTimeout)
//	dial connectTimeout, TLS handshake tls.handshakeTimeout (1 s and 2 s
//	by default: DefaultConnectTimeout, DefaultHandshakeTimeout)
//
// A zero time.Time is "no bound" in every function here.

// RouteDeadline returns the Route deadline of a request that started at
// start: start + timeout, or start + DefaultRouteTimeout when timeout is 0
// or negative.
func RouteDeadline(start time.Time, timeout time.Duration) time.Time {
	if timeout <= 0 {
		timeout = DefaultRouteTimeout
	}
	return start.Add(timeout)
}

// LegDeadline returns the deadline of a leg that starts at start: the
// earlier of the Route deadline and start + timeout (Upstream.spec.timeout;
// 0 or negative means the Route's timeout, so the Route deadline alone
// bounds the leg). It covers every attempt and backoff up to the final
// response headers.
func LegDeadline(route, start time.Time, timeout time.Duration) time.Time {
	if timeout <= 0 {
		return route
	}
	return Earliest(route, start.Add(timeout))
}

// PerTryTimeout returns the timeout of an attempt that starts at now:
// configured (retries.perTryTimeout) when positive, else the leg time left
// divided by the retries left plus one, computed at each attempt start.
// retriesLeft is the number of retries still allowed after this attempt. It
// returns 0, no per-try bound, when the leg has no deadline or no time
// left: the leg deadline alone then bounds the attempt.
func PerTryTimeout(configured time.Duration, legDeadline, now time.Time, retriesLeft int) time.Duration {
	if configured > 0 {
		return configured
	}
	if legDeadline.IsZero() {
		return 0
	}
	left := legDeadline.Sub(now)
	if left <= 0 {
		return 0
	}
	return max(left/time.Duration(max(retriesLeft, 0)+1), time.Nanosecond)
}

// AttemptDeadline returns the deadline of an attempt that starts at start:
// the earlier of the leg deadline and start + perTry (no per-try bound when
// perTry is 0 or negative). It covers the attempt up to the last response
// header byte.
func AttemptDeadline(legDeadline, start time.Time, perTry time.Duration) time.Time {
	if perTry <= 0 {
		return legDeadline
	}
	return Earliest(legDeadline, start.Add(perTry))
}

// Earliest returns the earlier of two deadlines, where a zero time is no
// bound.
func Earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero(), a.Before(b):
		return a
	default:
		return b
	}
}

// Causes of an attempt context that a Deadline canceled; context.Cause
// returns them and RoundTrip returns them as its error.
var (
	// ErrAttemptTimeout is the expiry of the attempt deadline set by
	// perTryTimeout: error kind timeout, retryOn decides.
	ErrAttemptTimeout = errors.New("resilience: attempt deadline expired")
	// ErrLegTimeout is the expiry of the leg deadline (or the Route
	// deadline clamping it) during an attempt: RZ-UP-003.
	ErrLegTimeout = errors.New("resilience: leg deadline expired")
)

// Deadline states.
const (
	deadlineArmed uint32 = iota
	deadlineStopped
	deadlineFired
)

// Deadline cancels a context when the injected clock reaches a deadline.
// Its context carries no Deadline() value, because the clock may be a fake
// one while dialers and TLS read context deadlines against the wall clock;
// cancellation alone bounds the attempt.
//
// The Upstream core arms each attempt's Deadline in place, inside the
// attempt's own record, on a timer its pooled leg state reuses (Init and
// Fire), and calls Stop when response headers arrive, so the attempt and
// leg deadlines stop applying and a slow body is not cut (only the Route
// timeout bounds it after commit), and Cancel when the attempt ends (its
// response body closed, or no response). Whichever of Stop and the
// deadline comes first decides, so a late timer never cancels a stopped
// Deadline's context; only a Deadline still armed touches its timer, so
// once Stop or the deadline decided, the timer may serve another attempt.
type Deadline struct {
	cancel context.CancelCauseFunc
	cause  error
	clk    clock.Clock
	at     time.Time
	timer  clock.Timer
	state  atomic.Uint32
}

// WithDeadline returns a child of parent that is canceled with cause when
// clk reaches deadline (at once when it already passed), and the Deadline
// that controls it, with a timer of its own. A zero deadline sets no
// timer.
func WithDeadline(parent context.Context, clk clock.Clock, deadline time.Time, cause error) (context.Context, *Deadline) {
	ctx, cancel := context.WithCancelCause(parent)
	d := &Deadline{}
	d.Init(cancel, clk, deadline, cause, nil)
	return ctx, d
}

// Init arms d in place: cancel (the attempt context's
// context.CancelCauseFunc) runs with cause when clk reaches deadline, at
// once when it already passed; a zero deadline arms nothing. timer, when
// not nil, is a stopped timer whose function calls Fire on the Deadline
// armed with it, reset here to the deadline: a leg reuses one timer for
// all its attempts instead of allocating one per attempt. nil makes a
// timer of d's own. d must be the zero Deadline.
func (d *Deadline) Init(cancel context.CancelCauseFunc, clk clock.Clock, deadline time.Time, cause error, timer clock.Timer) {
	d.cancel, d.cause, d.clk, d.at = cancel, cause, clk, deadline
	if deadline.IsZero() {
		return
	}
	wait := deadline.Sub(clk.Now())
	if wait <= 0 {
		d.fire()
		return
	}
	if timer == nil {
		d.timer = clk.AfterFunc(wait, d.fire)
		return
	}
	d.timer = timer
	timer.Reset(wait)
}

// Fire cancels the context at the deadline unless Stop came first. A call
// before the deadline does nothing: a reused timer's function may still
// run for an earlier attempt after the timer was reset for this one.
func (d *Deadline) Fire() {
	if !d.at.IsZero() && d.clk.Now().Before(d.at) {
		return
	}
	d.fire()
}

// fire cancels the context unless Stop came first.
func (d *Deadline) fire() {
	if d.state.CompareAndSwap(deadlineArmed, deadlineFired) {
		d.cancel(d.cause)
	}
}

// Stop disarms the deadline without canceling the context: response
// headers arrived. It reports whether the deadline had not fired. Only the
// call that disarms stops the timer, which may serve another attempt
// afterwards.
func (d *Deadline) Stop() bool {
	if d.state.CompareAndSwap(deadlineArmed, deadlineStopped) {
		if d.timer != nil {
			d.timer.Stop()
		}
		return true
	}
	return d.state.Load() == deadlineStopped
}

// Cancel disarms the deadline and cancels the context with
// context.Canceled (the attempt ended); it is safe to call more than once
// and after Stop. Classify the attempt's error first: ClassifyStage reads
// any ended context as timeout.
func (d *Deadline) Cancel() {
	d.Stop()
	d.cancel(nil)
}

// Expired reports whether the deadline fired.
func (d *Deadline) Expired() bool { return d.state.Load() == deadlineFired }
