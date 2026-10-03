// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package breaker is the per-shard State client breaker and reconnect
// pacer of the redis driver (spec 08 reqs 19 to 22,
// docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes"). Both read time from an injected clock.Clock,
// so tests drive them with clocktest.Fake.
//
// The Breaker is lock-free: admission in the closed state is one atomic
// load, completions add to a ring of ten packed (period, epoch, successes,
// failures) buckets with compare-and-swap, and every transition is one
// compare-and-swap on a packed state word. The Pacer guards dials, which
// are rare, with a short mutex.
package breaker

import (
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// State is a breaker state.
type State uint8

// Breaker states (spec 08 req 19).
const (
	// Closed admits every call and counts outcomes.
	Closed State = iota
	// Open skips every call (RZ-STS-003) until its random delay ends.
	Open
	// HalfOpen admits one probe call at a time.
	HalfOpen
)

// String returns the lowercase state name.
func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Config holds the breaker targets of spec 08 req 19
// (statestore.Limits carries the same values for the drivers).
type Config struct {
	// Window is the sliding failure window, kept as ten buckets (5 s).
	Window time.Duration
	// MinCalls is the fewest completed calls in Window that can open the
	// breaker (20).
	MinCalls int
	// FailureRatio opens the breaker when failures/calls reaches it (0.5).
	FailureRatio float64
	// ConnectFailures consecutive connect failures open the breaker (5).
	ConnectFailures int
	// OpenMin and OpenMax bound the uniformly random open delay (1 s, 3 s).
	OpenMin, OpenMax time.Duration
	// CloseAfter consecutive successful probes close the breaker (3; at
	// most 7).
	CloseAfter int
	// ProbeIdle is how long a half-open breaker waits for a request probe
	// before ProbeDue asks the driver to send PING (1 s).
	ProbeIdle time.Duration
	// OnChange, when set, is called synchronously by the goroutine that
	// made a transition (to log breaker open at WARN and close at INFO).
	// It must not block. The lazy Open to HalfOpen move is reported when
	// a call is first admitted as a probe.
	OnChange func(from, to State)
}

// DefaultConfig returns the targets of spec 08 req 19.
func DefaultConfig() Config {
	return Config{
		Window: 5 * time.Second, MinCalls: 20, FailureRatio: 0.5, ConnectFailures: 5,
		OpenMin: time.Second, OpenMax: 3 * time.Second, CloseAfter: 3, ProbeIdle: time.Second,
	}
}

// Rand returns a uniformly distributed duration in [lo, hi].
type Rand func(lo, hi time.Duration) time.Duration

// Uniform is the default Rand, on math/rand/v2.
func Uniform(lo, hi time.Duration) time.Duration {
	if hi <= lo {
		return lo
	}
	return lo + rand.N(hi-lo+1) //nolint:gosec // backoff jitter, not a secret
}

// Packed state word: state (2 bits), probe in flight (1), consecutive
// probe successes (3), epoch (16), open-until in milliseconds since the
// breaker's base time (42 bits, about 139 years).
const (
	stateMask  = 0x3
	probeBit   = 1 << 2
	succShift  = 3
	succMask   = 0x7
	epochShift = 6
	epochMask  = 0xffff
	untilShift = 22
)

// Packed bucket word: failures (13 bits), successes (13), epoch tag (8),
// period index (30 bits of the bucket number since the base time).
const (
	numBuckets  = 10
	countMax    = 1<<13 - 1
	succOff     = 13
	tagOff      = 26
	periodOff   = 34
	tagMask     = 0xff
	periodMask  = 1<<30 - 1
	maxClose    = succMask
	minBucketMs = 1
)

func stateOf(w uint64) State   { return State(w & stateMask) }
func succOf(w uint64) int      { return int(w >> succShift & succMask) }
func epochOf(w uint64) uint64  { return w >> epochShift & epochMask }
func untilOf(w uint64) int64   { return int64(w >> untilShift) }
func probing(w uint64) bool    { return w&probeBit != 0 }
func bucketFails(v uint64) int { return int(v & countMax) }
func bucketSuccs(v uint64) int { return int(v >> succOff & countMax) }
func bucketTag(v uint64) uint64 {
	return v >> tagOff & tagMask
}
func bucketPeriod(v uint64) uint64 { return v >> periodOff & periodMask }

func pack(s State, probe bool, succ int, epoch uint64, until int64) uint64 {
	w := uint64(s) | uint64(succ&succMask)<<succShift | (epoch&epochMask)<<epochShift | uint64(max(until, 0))<<untilShift
	if probe {
		w |= probeBit
	}
	return w
}

func packBucket(period, epoch uint64, succs, fails int) uint64 {
	return uint64(fails&countMax) | uint64(succs&countMax)<<succOff | (epoch&tagMask)<<tagOff | (period&periodMask)<<periodOff
}

// Ticket is one admitted call's claim on the breaker. The caller keeps it
// (usually in its pooled pending call) from Allow to Done; Done records
// the outcome at most once, so a timeout and a late reply racing on the
// same call count once (spec 08 req 20).
type Ticket struct {
	epoch    uint64
	probe    bool
	admitted bool
	done     atomic.Bool
}

// Probe reports whether the ticket's call is a half-open probe.
func (t *Ticket) Probe() bool { return t.probe }

func (t *Ticket) set(epoch uint64, probe bool) {
	t.epoch, t.probe, t.admitted = epoch, probe, true
	t.done.Store(false)
}

// Breaker is one shard's breaker. The zero value is not usable; call New.
type Breaker struct {
	clk      clock.Clock
	cfg      Config
	rnd      Rand
	base     time.Time
	bucketMs int64

	word      atomic.Uint64
	connFails atomic.Int64
	// probeAt is the millisecond (since base) of the last probe start or
	// end, the idle reference of ProbeDue.
	probeAt atomic.Int64
	buckets [numBuckets]atomic.Uint64
}

// New returns a closed breaker (breakers start closed after a restart,
// spec 08 req 19). A nil rnd uses Uniform; zero Config fields take
// DefaultConfig values.
func New(clk clock.Clock, cfg Config, rnd Rand) *Breaker {
	def := DefaultConfig()
	if cfg.Window <= 0 {
		cfg.Window = def.Window
	}
	if cfg.MinCalls <= 0 {
		cfg.MinCalls = def.MinCalls
	}
	if cfg.FailureRatio <= 0 {
		cfg.FailureRatio = def.FailureRatio
	}
	if cfg.ConnectFailures <= 0 {
		cfg.ConnectFailures = def.ConnectFailures
	}
	if cfg.OpenMin <= 0 {
		cfg.OpenMin = def.OpenMin
	}
	if cfg.OpenMax < cfg.OpenMin {
		cfg.OpenMax = max(def.OpenMax, cfg.OpenMin)
	}
	if cfg.CloseAfter <= 0 {
		cfg.CloseAfter = def.CloseAfter
	}
	cfg.CloseAfter = min(cfg.CloseAfter, maxClose)
	if cfg.ProbeIdle <= 0 {
		cfg.ProbeIdle = def.ProbeIdle
	}
	if rnd == nil {
		rnd = Uniform
	}
	return &Breaker{
		clk: clk, cfg: cfg, rnd: rnd, base: clk.Now(),
		bucketMs: max(cfg.Window.Milliseconds()/numBuckets, minBucketMs),
	}
}

// ms returns t in whole milliseconds since the base time, rounded down.
func (b *Breaker) ms(t time.Time) int64 { return max(t.Sub(b.base).Milliseconds(), 0) }

// msCeil returns t in milliseconds since the base time, rounded up.
func (b *Breaker) msCeil(t time.Time) int64 {
	d := max(t.Sub(b.base), 0)
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

func (b *Breaker) period(t time.Time) uint64 {
	return uint64(b.ms(t)/b.bucketMs) & periodMask //nolint:gosec // non-negative
}

func (b *Breaker) changed(from, to State) {
	if b.cfg.OnChange != nil {
		b.cfg.OnChange(from, to)
	}
}

// Allow reports whether a call may be sent and fills t for Done. Closed
// admits every call; Open admits none until its delay ends, then moves to
// HalfOpen admitting this call as the probe; HalfOpen admits one probe at
// a time (others get RZ-STS-003 from the driver).
func (b *Breaker) Allow(t *Ticket) bool {
	t.admitted = false
	for {
		w := b.word.Load()
		switch stateOf(w) {
		case Closed:
			t.set(epochOf(w), false)
			return true
		case Open:
			now := b.ms(b.clk.Now())
			if now < untilOf(w) {
				return false
			}
			nw := pack(HalfOpen, true, 0, epochOf(w)+1, 0)
			if b.word.CompareAndSwap(w, nw) {
				b.probeAt.Store(now)
				b.changed(Open, HalfOpen)
				t.set(epochOf(nw), true)
				return true
			}
		default:
			if probing(w) {
				return false
			}
			if b.word.CompareAndSwap(w, w|probeBit) {
				b.probeAt.Store(b.ms(b.clk.Now()))
				t.set(epochOf(w), true)
				return true
			}
		}
	}
}

// Done records the outcome of a call admitted with t: success is a
// well-formed reply (a denial included), failure a caller timeout
// (recorded at the timeout), connection error, error reply or unparsable
// reply (spec 08 req 20). It reports whether this was the ticket's first
// Done; later calls are ignored. Outcomes of calls admitted before the
// last transition do not count.
func (b *Breaker) Done(t *Ticket, success bool) bool {
	if !t.admitted || !t.done.CompareAndSwap(false, true) {
		return false
	}
	now := b.clk.Now()
	if t.probe {
		b.probeDone(now, t.epoch, success)
		return true
	}
	w := b.word.Load()
	if stateOf(w) != Closed || epochOf(w) != t.epoch {
		return true
	}
	p := b.period(now)
	b.add(p, t.epoch, success)
	if !success {
		b.evaluate(now, p, t.epoch)
	}
	return true
}

// add counts one outcome in period p's bucket for the closed epoch.
func (b *Breaker) add(p, epoch uint64, success bool) {
	slot := &b.buckets[p%numBuckets]
	for {
		v := slot.Load()
		var s, f int
		if bucketPeriod(v) == p && bucketTag(v) == epoch&tagMask {
			s, f = bucketSuccs(v), bucketFails(v)
		}
		if s == countMax || f == countMax {
			// Halving keeps the failure ratio at any call rate.
			s, f = s/2, f/2
		}
		if success {
			s++
		} else {
			f++
		}
		if slot.CompareAndSwap(v, packBucket(p, epoch, s, f)) {
			return
		}
	}
}

// counts sums the buckets of the closed epoch inside the window ending in
// period p.
func (b *Breaker) counts(p, epoch uint64) (calls, fails int) {
	for i := range b.buckets {
		v := b.buckets[i].Load()
		if bucketTag(v) != epoch&tagMask || (p-bucketPeriod(v))&periodMask >= numBuckets {
			continue
		}
		calls += bucketSuccs(v) + bucketFails(v)
		fails += bucketFails(v)
	}
	return calls, fails
}

// evaluate opens the breaker when at least MinCalls completed in the
// window and the failure share reached FailureRatio.
func (b *Breaker) evaluate(now time.Time, p, epoch uint64) {
	calls, fails := b.counts(p, epoch)
	if calls < b.cfg.MinCalls || float64(fails) < b.cfg.FailureRatio*float64(calls) {
		return
	}
	for {
		w := b.word.Load()
		if stateOf(w) != Closed || epochOf(w) != epoch || b.trip(w, now) {
			return
		}
	}
}

// probeDone applies a probe outcome: CloseAfter consecutive successes
// close the breaker, any failure reopens it with a new delay.
func (b *Breaker) probeDone(now time.Time, epoch uint64, success bool) {
	for {
		w := b.word.Load()
		if stateOf(w) != HalfOpen || epochOf(w) != epoch || !probing(w) {
			return
		}
		if !success {
			if b.trip(w, now) {
				return
			}
			continue
		}
		s := succOf(w) + 1
		if s >= b.cfg.CloseAfter {
			if b.word.CompareAndSwap(w, pack(Closed, false, 0, epoch+1, 0)) {
				b.connFails.Store(0)
				b.changed(HalfOpen, Closed)
				return
			}
			continue
		}
		if b.word.CompareAndSwap(w, pack(HalfOpen, false, s, epoch, 0)) {
			b.probeAt.Store(b.ms(now))
			return
		}
	}
}

// trip moves the breaker from word w to Open with a fresh random delay in
// [OpenMin, OpenMax]; false means w changed first.
func (b *Breaker) trip(w uint64, now time.Time) bool {
	delay := min(max(b.rnd(b.cfg.OpenMin, b.cfg.OpenMax), b.cfg.OpenMin), b.cfg.OpenMax)
	until := b.msCeil(now.Add(delay))
	if !b.word.CompareAndSwap(w, pack(Open, false, 0, epochOf(w)+1, until)) {
		return false
	}
	b.connFails.Store(0)
	b.changed(stateOf(w), Open)
	return true
}

// ConnectFailed records a dial or TLS handshake failure seen by the
// Ruralz dialer; ConnectFailures consecutive ones open a closed breaker.
// Dials refused by the Pacer are not connect failures (spec 08 req 22).
func (b *Breaker) ConnectFailed() {
	if b.connFails.Add(1) < int64(b.cfg.ConnectFailures) {
		return
	}
	now := b.clk.Now()
	for {
		w := b.word.Load()
		if stateOf(w) != Closed || b.trip(w, now) {
			return
		}
	}
}

// ConnectSucceeded resets the consecutive connect failure count.
func (b *Breaker) ConnectSucceeded() { b.connFails.Store(0) }

// State returns the current state; an Open breaker whose delay ended
// reads as HalfOpen. Degraded reason state_store_breaker_open is raised
// while any shard's breaker is not Closed (spec 08 req 21).
func (b *Breaker) State() State {
	w := b.word.Load()
	if stateOf(w) == Open && b.ms(b.clk.Now()) >= untilOf(w) {
		return HalfOpen
	}
	return stateOf(w)
}

// ProbeDue reports whether a half-open breaker has waited ProbeIdle
// without a probe in flight, so the driver should send PING as the probe
// (spec 08 req 19): it then calls Allow and Done like any call.
func (b *Breaker) ProbeDue() bool {
	w := b.word.Load()
	now := b.ms(b.clk.Now())
	idle := b.cfg.ProbeIdle.Milliseconds()
	switch stateOf(w) {
	case Open:
		return now >= untilOf(w)+idle
	case HalfOpen:
		return !probing(w) && now-b.probeAt.Load() >= idle
	default:
		return false
	}
}
