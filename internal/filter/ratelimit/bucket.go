// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"math"
	"math/bits"
	"time"
)

// Local token bucket arithmetic, in whole tokens and Unix nanoseconds, so
// a decision never depends on floating-point rounding
// (docs/architecture/09-traffic-management-and-resilience.md "Decision
// path" and "Per-Node ceiling").

// failOpenSteps is how many equal steps release a fail-open allowance for
// a window past failOpenStepped (target).
const failOpenSteps = 60

// failOpenStepped is the longest window whose fail-open allowance arrives
// whole at the window start.
const failOpenStepped = time.Minute

// mulDiv returns a × b / d rounded down, and false when the quotient does
// not fit 63 bits. a, b and d are positive.
func mulDiv(a, b, d uint64) (uint64, bool) {
	hi, lo := bits.Mul64(a, b)
	if hi >= d {
		return 0, false
	}
	q, _ := bits.Div64(hi, lo, d)
	if q > math.MaxInt64 {
		return 0, false
	}
	return q, true
}

// mulDivUp returns a × b / d rounded up, saturating at the largest int64.
func mulDivUp(a, b, d uint64) int64 {
	hi, lo := bits.Mul64(a, b)
	if hi >= d {
		return math.MaxInt64
	}
	q, r := bits.Div64(hi, lo, d)
	if r != 0 {
		q++
	}
	if q > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(q)
}

// refill brings a continuously refilled bucket to now: perWindow tokens
// arrive per window nanoseconds, the k-th whole token since *last at *last
// + ceil(k × window / perWindow), up to capacity. *last advances by the
// time the added tokens took, rounded up to the nanosecond, so the
// fraction of the next token carries over and a bucket never runs ahead
// of its rate: it runs late by at most a nanosecond per refill that adds
// tokens. A full bucket keeps no fraction.
func refill(tokens, last *int64, now, perWindow, window, capacity int64) {
	if *tokens >= capacity {
		*tokens, *last = capacity, now
		return
	}
	if now <= *last || perWindow <= 0 || window <= 0 {
		return
	}
	n, ok := mulDiv(uint64(now-*last), uint64(perWindow), uint64(window)) //nolint:gosec // G115: positive operands, checked above.
	if !ok || n >= uint64(capacity-*tokens) {                             //nolint:gosec // G115: capacity exceeds tokens here.
		*tokens, *last = capacity, now
		return
	}
	if n == 0 {
		return
	}
	*tokens += int64(n) //nolint:gosec // G115: n is below capacity - tokens.
	*last += mulDivUp(n, uint64(window), uint64(perWindow))
}

// fillTime returns how long after now a continuously refilled bucket
// holding tokens, last refilled at last, holds want tokens: the
// (want - tokens)-th token arrives at last + ceil((want - tokens) × window
// / perWindow). It is 0 when the bucket already holds want.
func fillTime(tokens, last, now, want, perWindow, window int64) time.Duration {
	if tokens >= want || perWindow <= 0 || window <= 0 {
		return 0
	}
	at := last + mulDivUp(uint64(want-tokens), uint64(window), uint64(perWindow)) //nolint:gosec // G115: want exceeds tokens; perWindow and window are positive.
	if at <= now {
		return 0
	}
	return time.Duration(at - now)
}

// floorDiv is floor(a / b) for b > 0.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}

// alignDown returns the start of the epoch-aligned window holding now.
func alignDown(now, window int64) int64 { return floorDiv(now, window) * window }

// released returns how much of a fail-open allowance a window has
// released into its span: the whole allowance for a window up to a
// minute, else allowance × (k + 1) / 60 at step k of 60 equal steps.
func released(into, window, allowance int64) int64 {
	if window <= int64(failOpenStepped) {
		return allowance
	}
	k, ok := mulDiv(uint64(max(into, 0)), failOpenSteps, uint64(window)) //nolint:gosec // G115: non-negative operands.
	if !ok || k >= failOpenSteps {
		k = failOpenSteps - 1
	}
	r, ok := mulDiv(uint64(allowance), k+1, failOpenSteps) //nolint:gosec // G115: allowance is positive.
	if !ok {
		return allowance
	}
	return int64(r) //nolint:gosec // G115: r is at most allowance.
}

// stepStart returns the offset of step k of a stepped window,
// ceil(k × window / 60).
func stepStart(k, window int64) int64 {
	return mulDivUp(uint64(k), uint64(window), failOpenSteps) //nolint:gosec // G115: non-negative operands.
}

// failOpenRefill brings a bucket on the fail-open refill to now. The
// bucket keeps, in its entry fields, the epoch-aligned window it accounts
// (start) and the allowance released into it so far (rel), with rel -1
// in the window of the switch, which releases nothing. A new window brings
// the allowance released into it so far, without carry-over.
func failOpenRefill(tokens, rel, start *int64, now, window, allowance int64) {
	ws := alignDown(now, window)
	if ws != *start {
		r := released(now-ws, window, allowance)
		*tokens, *rel, *start = r, r, ws
		return
	}
	if *rel < 0 {
		return
	}
	if r := released(now-ws, window, allowance); r > *rel {
		*tokens += r - *rel
		*rel = r
	}
}

// failOpenWait returns how long after now a bucket on the fail-open
// refill holds want tokens, after failOpenRefill brought it to now: a
// later step of this window, or else the first step of a later window that
// releases enough. A want above the allowance never fits a window: the
// wait is then to the next window start.
func failOpenWait(tokens, rel, start, now, window, allowance, want int64) time.Duration {
	if tokens >= want {
		return 0
	}
	next := start + window
	if rel >= 0 && window > int64(failOpenStepped) {
		// Within this window, step k releases allowance × (k + 1) / 60.
		need := rel + want - tokens
		if k, ok := firstStep(need, allowance); ok {
			if at := start + stepStart(k, window); at > now {
				return time.Duration(at - now)
			}
		}
	}
	if want > allowance || window <= int64(failOpenStepped) {
		return time.Duration(next - now)
	}
	k, _ := firstStep(want, allowance)
	return time.Duration(next + stepStart(k, window) - now)
}

// firstStep returns the first step k of 60 whose cumulative release,
// allowance × (k + 1) / 60, reaches need; false when none does.
func firstStep(need, allowance int64) (int64, bool) {
	if need > allowance {
		return 0, false
	}
	if need <= 0 {
		return 0, true
	}
	// allowance × (k + 1) / 60 >= need  <=>  k + 1 >= ceil(60 × need / allowance).
	k := mulDivUp(uint64(need), failOpenSteps, uint64(allowance)) - 1 //nolint:gosec // G115: positive operands.
	return max(k, 0), true
}
