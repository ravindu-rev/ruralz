// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// State is a circuit breaker state. The values follow the state label of
// ruralz_upstream_breaker_state_info (closed, open, half_open), so int(s)
// is the breaker-state index of internal/telemetry/emit (BreakerClosed to
// BreakerHalfOpen).
type State uint8

// Breaker states (Figure 2 of docs/architecture/09-traffic-management-and-resilience.md).
const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
	// NumStates is the number of states.
	NumStates = int(StateHalfOpen) + 1
)

// String returns the state label: "closed", "open" or "half_open".
func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	case StateClosed:
		return "closed"
	default:
		return "closed"
	}
}

// Transition reports whether from → to is an edge of Figure 2: closed →
// open, open → half-open, half-open → closed and half-open → open.
func Transition(from, to State) bool {
	switch from {
	case StateClosed:
		return to == StateOpen
	case StateOpen:
		return to == StateHalfOpen
	case StateHalfOpen:
		return to == StateClosed || to == StateOpen
	default:
		return false
	}
}

// LegResult is how a leg counts for the breaker (05 req 37).
type LegResult uint8

// Leg results.
const (
	// LegNotCounted: the leg does not count (a gate ended it, a connect
	// error with at most half of the Endpoints ejected, or no attempt ran).
	// A probe leg that is not counted frees the probe slot.
	LegNotCounted LegResult = iota
	// LegSuccess: failureWhen was false for the leg's final result.
	LegSuccess
	// LegFailure: failureWhen was true (or failed at run time).
	LegFailure
)

// BreakerResult returns how a leg that ended with o counts for the
// breaker (05 req 37): failed is failureWhen over the leg's final result
// (EvalFailureWhen), ejected and endpoints the Upstream's passively or
// actively ejected Endpoints and its Endpoint count when the leg ended.
//
//   - A leg a gate ended before any attempt (RZ-UP-005, RZ-UP-006,
//     RZ-UP-008) and a leg with no attempt are not counted.
//   - A leg whose final error kind is connect counts only while more than
//     EjectedMajorityPercent of the Endpoints are ejected, so a bad
//     Endpoint or a lost zone never opens the breaker.
//   - Otherwise the leg is a failure when failed, else a success.
func BreakerResult(failed bool, o Outcome, ejected, endpoints int) LegResult {
	switch {
	case o.Attempts <= 0:
		return LegNotCounted
	case !o.Responded && o.Kind == KindConnect && ejected*100 <= endpoints*EjectedMajorityPercent:
		return LegNotCounted
	case failed:
		return LegFailure
	default:
		return LegSuccess
	}
}

// Permit is a leg's admission by the breaker, returned by Allow and passed
// back to Record when the leg ends.
type Permit struct {
	gen   uint64
	probe bool
}

// Probe reports whether the leg is the half-open probe.
func (p Permit) Probe() bool { return p.probe }

// breakerBucket is one second of the rolling window.
type breakerBucket struct {
	sec            int64
	legs, failures int
}

// stateBits is how many low bits of Breaker.word hold the state; the
// generation takes the rest.
const (
	stateBits = 2
	stateMask = 1<<stateBits - 1
)

// Breaker is the circuit breaker of one Upstream on one Node (05 req 37,
// Figure 2), counting legs by their final result:
//
//   - Closed: every leg passes. The breaker opens when consecutiveFailures
//     consecutive counted legs failed and at least failureRatio of at least
//     minimumLegs counted legs in the rolling window (ten 1 s buckets)
//     failed; below minimumLegs legs per window it never opens.
//   - Open: every new leg is refused (RZ-UP-005, no retry) until
//     openDuration, jittered ±20%, has elapsed; then the next leg becomes
//     the half-open probe.
//   - Half-open: exactly one probe leg at a time, others refused
//     (RZ-UP-005); halfOpenSuccesses consecutive probe successes close the
//     breaker (with a fresh window), a probe failure reopens it.
//
// A leg admitted in one state period may start another attempt (a retry)
// only while Admits holds: the breaker is closed, or half-open and the leg
// is its current probe (05 req 11 step e). Leg checks it in Decide, before
// the retry delay, and again in StartAttempt, when the attempt starts,
// since the breaker can change during the delay. Results of legs admitted
// in an earlier state period (a leg that started before the breaker
// opened) are ignored. Every call takes the BreakerConfig of the caller's
// snapshot, so a Hot Reload applies new thresholds to later calls while
// the state carries over (05 req 2).
//
// A probe admitted with a lease (AllowLease) that has not ended by the
// lease's end is presumed lost, for example a leg dropped without End: the
// next leg becomes the probe in its place and the lost probe's permit goes
// stale, so it can neither retry nor count. The lease is the probe leg's
// deadline plus openDuration, after which the leg can start no attempt.
//
// Allow in the closed state is one atomic load, Admits always is; Record
// takes a mutex. The Breaker is safe for concurrent use and starts no
// goroutine.
type Breaker struct {
	clk      clock.Clock
	src      rand.Source
	onChange func(from, to State)
	base     time.Time

	// word holds gen<<stateBits | state for the lock-free closed path;
	// written under mu only.
	word atomic.Uint64

	mu          sync.Mutex
	consecutive int
	buckets     [BreakerBuckets]breakerBucket
	openedAt    time.Time
	openUntil   time.Time
	probing     bool
	probeUntil  time.Time // the probe's lease end; zero for no lease
	successes   int
}

// NewBreaker returns a closed breaker. clk supplies time, src the open
// jitter (nil means the math/rand/v2 global source; the Breaker draws under
// its mutex, so src need not be safe for concurrent use), and onChange,
// when not nil, is called on every transition, under the Breaker's mutex
// and in transition order: it must not block or call the Breaker (it sets
// ruralz_upstream_breaker_state_info).
func NewBreaker(clk clock.Clock, src rand.Source, onChange func(from, to State)) *Breaker {
	return &Breaker{clk: clk, src: src, onChange: onChange, base: clk.Now()}
}

// State returns the current state. An open breaker whose openDuration has
// elapsed reads open until the next Allow lets the probe through.
func (b *Breaker) State() State { return State(b.word.Load() & stateMask) }

// Allow admits a new leg: ok is false when the breaker is open, or
// half-open with its probe in flight (05 req 11 step e: RZ-UP-005). The
// leg must pass the permit to Record exactly once when it ends. A probe
// admitted by Allow holds the probe slot until then, without a lease.
func (b *Breaker) Allow(cfg *BreakerConfig) (p Permit, ok bool) {
	return b.AllowLease(cfg, time.Time{})
}

// AllowLease is Allow for a leg whose attempts end by deadline (its leg
// deadline; zero for none). When the leg becomes the half-open probe, its
// probe slot is leased until deadline plus cfg.OpenDuration: a later leg
// that finds the probe still in flight after that takes the probe slot,
// and the old permit goes stale. A zero deadline leases nothing.
func (b *Breaker) AllowLease(cfg *BreakerConfig, deadline time.Time) (p Permit, ok bool) {
	if w := b.word.Load(); State(w&stateMask) == StateClosed {
		return Permit{gen: w >> stateBits}, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.word.Load()
	switch State(w & stateMask) {
	case StateClosed:
		return Permit{gen: w >> stateBits}, true
	case StateOpen:
		if b.clk.Now().Before(b.openUntil) {
			return Permit{}, false
		}
		b.setLocked(StateHalfOpen, cfg)
	case StateHalfOpen:
		if b.probing {
			if b.probeUntil.IsZero() || b.clk.Now().Before(b.probeUntil) {
				return Permit{}, false
			}
			// The probe outlived its lease: a new period of the same state
			// makes its permit stale. Not a transition: no callback.
			b.word.Store((w>>stateBits+1)<<stateBits | uint64(StateHalfOpen))
		}
	}
	b.probing = true
	b.probeUntil = time.Time{}
	if !deadline.IsZero() {
		b.probeUntil = deadline.Add(max(cfg.OpenDuration, 0))
	}
	return Permit{gen: b.word.Load() >> stateBits, probe: true}, true
}

// Admits reports whether a leg admitted with p may start another attempt,
// a retry (05 req 11 step e, req 33 condition 3): the breaker is closed, or
// half-open and p is its current probe. It is false while the breaker is
// open, for every other leg while it is half-open (a leg admitted before
// the breaker opened must not send a second attempt beside the probe;
// req 37), and for a probe whose lease another leg took over. It is one
// atomic load.
func (b *Breaker) Admits(p Permit) bool {
	w := b.word.Load()
	switch State(w & stateMask) {
	case StateClosed:
		return true
	case StateHalfOpen:
		return p.probe && p.gen == w>>stateBits
	default:
		return false
	}
}

// Record ends a leg admitted by Allow with its result (BreakerResult).
func (b *Breaker) Record(cfg *BreakerConfig, p Permit, r LegResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.word.Load()
	if p.gen != w>>stateBits {
		return
	}
	switch State(w & stateMask) {
	case StateClosed:
		b.recordClosedLocked(cfg, r)
	case StateHalfOpen:
		if !p.probe {
			return
		}
		b.probing = false
		switch r {
		case LegSuccess:
			b.successes++
			if b.successes >= max(cfg.HalfOpenSuccesses, 1) {
				b.setLocked(StateClosed, cfg)
			}
		case LegFailure:
			b.setLocked(StateOpen, cfg)
		case LegNotCounted:
		}
	case StateOpen:
	}
}

// recordClosedLocked counts a closed-state leg and opens the breaker when
// both conditions hold.
func (b *Breaker) recordClosedLocked(cfg *BreakerConfig, r LegResult) {
	if r == LegNotCounted {
		return
	}
	sec := b.secLocked()
	bk := &b.buckets[((sec%BreakerBuckets)+BreakerBuckets)%BreakerBuckets]
	if bk.sec != sec || bk.legs == 0 {
		*bk = breakerBucket{sec: sec}
	}
	bk.legs++
	if r == LegSuccess {
		b.consecutive = 0
		return
	}
	bk.failures++
	b.consecutive++
	if b.consecutive < max(cfg.ConsecutiveFailures, 1) {
		return
	}
	legs, failures := b.windowLocked(sec)
	if legs >= max(cfg.MinimumLegs, 1) && float64(failures)/float64(legs) >= cfg.FailureRatio {
		b.setLocked(StateOpen, cfg)
	}
}

// windowLocked sums the buckets of the rolling window ending in second
// sec.
func (b *Breaker) windowLocked(sec int64) (legs, failures int) {
	for i := range b.buckets {
		bk := &b.buckets[i]
		if bk.legs > 0 && bk.sec > sec-BreakerBuckets && bk.sec <= sec {
			legs += bk.legs
			failures += bk.failures
		}
	}
	return legs, failures
}

// setLocked moves to state to, starting a new period: permits of the old
// one no longer count.
func (b *Breaker) setLocked(to State, cfg *BreakerConfig) {
	w := b.word.Load()
	from := State(w & stateMask)
	b.word.Store(((w>>stateBits)+1)<<stateBits | uint64(to))
	b.probing = false
	b.probeUntil = time.Time{}
	b.successes = 0
	b.consecutive = 0
	switch to {
	case StateOpen:
		now := b.clk.Now()
		b.openedAt = now
		b.openUntil = now.Add(jitterOpen(cfg.OpenDuration, draw(b.src)))
	case StateClosed:
		b.openedAt = time.Time{}
		b.buckets = [BreakerBuckets]breakerBucket{}
	case StateHalfOpen:
	}
	if b.onChange != nil {
		b.onChange(from, to)
	}
}

// OpenUntil returns when an open breaker lets its probe through; the zero
// time when it is not open.
func (b *Breaker) OpenUntil() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.State() != StateOpen {
		return time.Time{}
	}
	return b.openUntil
}

// BreakerStats is a consistent read of a Breaker for the breaker object of
// /debug/upstreams (05 req 96).
type BreakerStats struct {
	// State is the current state.
	State State
	// OpenedAt is when the breaker last opened; it keeps its value while
	// half-open and is zero while closed.
	OpenedAt time.Time
	// OpenUntil is when an open breaker lets its probe through; zero when
	// not open.
	OpenUntil time.Time
	// WindowLegs is the counted legs in the rolling window ending now.
	WindowLegs int
	// WindowFailures is the failed legs among WindowLegs.
	WindowFailures int
	// ConsecutiveFailures is the current run of failed counted legs; every
	// transition resets it, so it is 0 unless closed.
	ConsecutiveFailures int
}

// Stats returns the breaker's state and counts, read under its mutex.
func (b *Breaker) Stats() BreakerStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := BreakerStats{State: b.State(), OpenedAt: b.openedAt, ConsecutiveFailures: b.consecutive}
	if st.State == StateOpen {
		st.OpenUntil = b.openUntil
	}
	st.WindowLegs, st.WindowFailures = b.windowLocked(b.secLocked())
	return st
}

// secLocked returns the window second of the current time.
func (b *Breaker) secLocked() int64 { return int64(b.clk.Now().Sub(b.base) / time.Second) }
