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

// Deadline arithmetic of 05 reqs 24 to 26. The bounds nest, each clamping
// those below it:
//
//	Route deadline   = request start + Route timeout (15 s by default)
//	leg deadline     = min(Route deadline, leg start + Upstream timeout)
//	attempt deadline = min(leg deadline, attempt start + perTryTimeout)
//	dial 1 s, TLS handshake 2 s (DialTimeout, TLSHandshakeTimeout)
//
// A zero time.Time is "no bound" in every function here.

// RouteDeadline returns the Route deadline of a request that started at
// start: start + timeout, or start + DefaultRouteTimeout when timeout is 0
// or negative (05 req 4).
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
// response headers (05 req 24).
func LegDeadline(route, start time.Time, timeout time.Duration) time.Time {
	if timeout <= 0 {
		return route
	}
	return Earliest(route, start.Add(timeout))
}

// PerTryTimeout returns the timeout of an attempt that starts at now:
// configured (retries.perTryTimeout) when positive, else the leg time left
// divided by the retries left plus one (05 req 4), computed at each attempt
// start. retriesLeft is the number of retries still allowed after this
// attempt. It returns 0, no per-try bound, when the leg has no deadline or
// no time left: the leg deadline alone then bounds the attempt.
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
// header byte (05 reqs 24 and 25).
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
// The Upstream core derives each attempt context with WithDeadline and
// calls Stop when response headers arrive, so the attempt and leg deadlines
// stop applying and a slow body is not cut (05 req 25; only the Route
// timeout bounds it after commit), and Cancel when the attempt ends (its
// response body closed, or no response). The timer is owned by the
// Deadline and stopped by Stop or Cancel; whichever of Stop and the
// deadline comes first decides, so a late timer never cancels a stopped
// Deadline's context.
type Deadline struct {
	cancel context.CancelCauseFunc
	cause  error
	timer  clock.Timer
	state  atomic.Uint32
}

// WithDeadline returns a child of parent that is canceled with cause when
// clk reaches deadline (at once when it already passed), and the Deadline
// that controls it. A zero deadline sets no timer.
func WithDeadline(parent context.Context, clk clock.Clock, deadline time.Time, cause error) (context.Context, *Deadline) {
	ctx, cancel := context.WithCancelCause(parent)
	d := &Deadline{cancel: cancel, cause: cause}
	if deadline.IsZero() {
		return ctx, d
	}
	wait := deadline.Sub(clk.Now())
	if wait <= 0 {
		d.fire()
		return ctx, d
	}
	d.timer = clk.AfterFunc(wait, d.fire)
	return ctx, d
}

// fire cancels the context at the deadline unless Stop came first.
func (d *Deadline) fire() {
	if d.state.CompareAndSwap(deadlineArmed, deadlineFired) {
		d.cancel(d.cause)
	}
}

// Stop disarms the deadline without canceling the context: response
// headers arrived. It reports whether the deadline had not fired.
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
