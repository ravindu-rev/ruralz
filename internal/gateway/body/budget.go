// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package body

import (
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Kind says which part of the budget a reservation draws from.
type Kind uint8

// Kinds.
const (
	// Gate draws from the general budget only and can never take the stream
	// share: gates, tees, decoded values and rewritten bodies.
	Gate Kind = iota
	// Stream draws from the stream share first, then from free general
	// budget: a subscribed stream's 32 KiB before commit (M3 users).
	Stream
)

// counterMask masks one of the two 32-bit halves of a packed word.
const counterMask = 1<<32 - 1

// Budget is the Node buffer budget, limits.maxBufferedBytes (spec 04 req
// 47): one per Node, carried across Hot Reloads (SetTotal applies a new
// value to new reservations and keeps outstanding ones). It counts whole
// Increments: gates may hold at most the total minus the stream share
// (25%, rounded down to increments), streams may hold everything free, and
// the reserved total never exceeds the effective total. The gauge
// ruralz_node_buffered_bytes follows every change. Budget is safe for
// concurrent use and lock-free on the request path.
type Budget struct {
	// used packs the gate increments (high half) and stream increments
	// (low half); one compare-and-swap changes it.
	used atomic.Uint64
	// lim packs the effective total (high half) and the stream share (low
	// half), in increments.
	lim atomic.Uint64

	mu       sync.Mutex // serializes SetTotal and SetExternal
	total    int64
	external int64

	gauge emit.Gauge

	// inc1 and inc2 recycle the backing arrays of gates and tees whose
	// capacity is exactly one or two increments (at most PoolMax, spec 07
	// req 73), as *[Increment]byte and *[PoolMax]byte so Put does not
	// allocate. Only the arrays are pooled, never a Buffer or Tee, so a
	// stale handle cannot reach another request's buffer.
	inc1 sync.Pool
	inc2 sync.Pool
}

// NewBudget returns a budget of total bytes (DefaultMaxBufferedBytes unless
// the Revision sets limits.maxBufferedBytes) reporting to g, the
// ruralz_node_buffered_bytes gauge (nil records nothing).
func NewBudget(total int64, g emit.Gauge) *Budget {
	b := &Budget{gauge: g}
	b.SetTotal(total)
	return b
}

// SetTotal sets limits.maxBufferedBytes at an activation. Outstanding
// reservations are kept even when they now exceed the total; new
// reservations fail until enough is released.
func (b *Budget) SetTotal(total int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total = max(0, total)
	b.storeLimits()
}

// SetExternal lowers the effective total by n bytes, the buffered bytes the
// other process reported during a handover (spec 04 req 67); 0 restores the
// full total. The stream share stays a quarter of the configured total, so
// the reduction takes general budget first.
func (b *Budget) SetExternal(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.external = max(0, n)
	b.storeLimits()
}

func (b *Budget) storeLimits() {
	eff := min(max(0, b.total-b.external)/Increment, counterMask)
	share := min(b.total/4/Increment, counterMask)
	b.lim.Store(pack(eff, share))
}

// pack joins two counts clamped to [0, counterMask] into one word.
func pack(hi, lo int64) uint64 {
	return uint64(min(max(hi, 0), counterMask))<<32 | uint64(min(max(lo, 0), counterMask))
}

// Total returns the configured total in bytes.
func (b *Budget) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// Effective returns the bytes reservations may reach: the total minus the
// external usage, rounded down to increments.
func (b *Budget) Effective() int64 { return int64(b.lim.Load()>>32) * Increment }

// StreamShare returns the stream share in bytes (a quarter of the total,
// rounded down to increments).
func (b *Budget) StreamShare() int64 { return int64(b.lim.Load()&counterMask) * Increment }

// Used returns the bytes reserved now, gates and streams together (the
// handover usage report's bufferedBytes).
func (b *Budget) Used() int64 {
	g, s := unpack(b.used.Load())
	return (g + s) * Increment
}

// GateUsed and StreamUsed return the bytes reserved by each kind.
func (b *Budget) GateUsed() int64 {
	g, _ := unpack(b.used.Load())
	return g * Increment
}

// StreamUsed returns the bytes reserved by streams.
func (b *Budget) StreamUsed() int64 {
	_, s := unpack(b.used.Load())
	return s * Increment
}

func unpack(u uint64) (hi, lo int64) { return int64(u >> 32), int64(u & counterMask) }

// reserve takes k increments of kind, or nothing when they do not fit; the
// gauge records the change on stripe s.
func (b *Budget) reserve(kind Kind, k int64, s emit.Stripe) bool {
	if k <= 0 {
		return true
	}
	for {
		u := b.used.Load()
		g, st := unpack(u)
		eff, share := unpack(b.lim.Load())
		var next uint64
		switch kind {
		case Stream:
			if k > eff || g+st+k > eff {
				return false
			}
			next = u + uint64(k)
		default:
			if k > eff || g+k+max(st, share) > eff {
				return false
			}
			next = u + uint64(k)<<32
		}
		if b.used.CompareAndSwap(u, next) {
			b.record(k, s)
			return true
		}
	}
}

// reserveUpTo reserves most increments of kind or, when the budget refuses,
// halves the request down to least (at least 1); it returns the increments
// reserved, 0 when not even least fits. Gates and tees grow geometrically
// with it and fall back to smaller steps near the ceiling.
func (b *Budget) reserveUpTo(kind Kind, least, most int64, s emit.Stripe) int64 {
	least = max(least, 1)
	k := max(most, least)
	for {
		if b.reserve(kind, k, s) {
			return k
		}
		if k <= least {
			return 0
		}
		k = max(k/2, least)
	}
}

// release returns k increments of kind, recorded on stripe s; callers never
// release more than they reserved (Account, Buffer and Tee track their
// holdings), so a half never borrows from the other.
func (b *Budget) release(kind Kind, k int64, s emit.Stripe) {
	if k <= 0 {
		return
	}
	if kind == Stream {
		b.used.Add(^uint64(k - 1))
	} else {
		b.used.Add(^(uint64(k)<<32 - 1))
	}
	b.record(-k, s)
}

// record adds k increments to the gauge on the caller's stripe, so the
// request path never shares one gauge cache line Node-wide.
func (b *Budget) record(k int64, s emit.Stripe) {
	if b.gauge != nil {
		b.gauge.Add(s, k*Increment)
	}
}

// array returns an empty slice with capacity c, a whole number of
// increments: a pooled array when c is one or two increments.
func (b *Budget) array(c int64) []byte {
	switch c {
	case Increment:
		if p, ok := b.inc1.Get().(*[Increment]byte); ok {
			return p[:0]
		}
	case PoolMax:
		if p, ok := b.inc2.Get().(*[PoolMax]byte); ok {
			return p[:0]
		}
	}
	return make([]byte, 0, c)
}

// recycle returns an array obtained from array to its pool when its
// capacity is one or two increments; larger ones are left to the garbage
// collector (spec 07 req 73). The caller must hold no other slice of it.
func (b *Budget) recycle(a []byte) {
	switch cap(a) {
	case Increment:
		b.inc1.Put((*[Increment]byte)(a[:Increment]))
	case PoolMax:
		b.inc2.Put((*[PoolMax]byte)(a[:PoolMax]))
	}
}

// ReserveStream reserves n bytes for a subscribed stream (rounded up to
// increments) from the stream share, then from free general budget
// (spec 04 req 47, M3 users); ErrBudget when neither covers it. The gauge
// records the change on the stream's stripe s. The caller returns the same
// n with ReleaseStream.
func (b *Budget) ReserveStream(n int64, s emit.Stripe) error {
	if !b.reserve(Stream, increments(n), s) {
		return ErrBudget
	}
	return nil
}

// ReleaseStream returns a reservation of n bytes made by ReserveStream,
// recorded on stripe s.
func (b *Budget) ReleaseStream(n int64, s emit.Stripe) { b.release(Stream, increments(n), s) }

// Account charges one holder's bytes to a Budget in increments: the
// per-request decoded values and rewritten bodies (spec 07 reqs 69-71),
// a stream's chunks. The zero value is unbound and refuses every
// reservation; Bind sets the budget. An Account is used by one goroutine at
// a time.
type Account struct {
	b      *Budget
	kind   Kind
	stripe emit.Stripe
	n      int64 // bytes charged
	held   int64 // increments reserved
}

// Bind attaches a to b with kind after returning anything it still holds;
// the gauge records a's changes on stripe s, the holder's request stripe.
func (a *Account) Bind(b *Budget, kind Kind, s emit.Stripe) {
	a.ReleaseAll()
	a.b, a.kind, a.stripe = b, kind, s
}

// Reserve charges n more bytes, reserving the increments they need;
// ErrBudget (503 RZ-RT-004) when the budget refuses, charging nothing. A
// charge that would pass the int64 range is one no budget covers:
// ErrBudget. n <= 0 charges nothing.
func (a *Account) Reserve(n int64) error {
	if n <= 0 {
		return nil
	}
	if n > maxInt64-a.n {
		return ErrBudget
	}
	need := increments(a.n+n) - a.held
	if need > 0 {
		if a.b == nil || !a.b.reserve(a.kind, need, a.stripe) {
			return ErrBudget
		}
		a.held += need
	}
	a.n += n
	return nil
}

// ReserveDecoded charges a decoded value of built bytes whose body arrived
// under rawLimit: ErrTooLarge past DecodedCap(rawLimit) (413 RZ-RT-003,
// 502 RZ-UP-010 or RZ-RT-015 by side), else Reserve (spec 04 req 47, spec
// 07 req 15).
func (a *Account) ReserveDecoded(built, rawLimit int64) error {
	if built > DecodedCap(rawLimit) {
		return ErrTooLarge
	}
	return a.Reserve(built)
}

// Release uncharges n bytes (at most what is charged) and returns the
// increments no longer needed.
func (a *Account) Release(n int64) {
	if n <= 0 {
		return
	}
	a.n -= min(n, a.n)
	keep := increments(a.n)
	if free := a.held - keep; free > 0 {
		a.b.release(a.kind, free, a.stripe)
		a.held = keep
	}
}

// ReleaseAll uncharges everything, keeping the binding.
func (a *Account) ReleaseAll() {
	if a.held > 0 {
		a.b.release(a.kind, a.held, a.stripe)
	}
	a.n, a.held = 0, 0
}

// Charged returns the bytes charged.
func (a *Account) Charged() int64 { return a.n }

// Reserved returns the bytes reserved from the budget (whole increments).
func (a *Account) Reserved() int64 { return a.held * Increment }
