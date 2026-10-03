// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"sync"
	"sync/atomic"
)

// Units is the Node's in-flight unit ceiling (spec 04 req 19): one unit per
// request, taken in ServeHTTP before anything else, plus one per parallel
// composition step and stream pump (spec 05 req 43). A full ceiling rejects
// at once with 503 RZ-RT-005 (CodeFull); nothing waits. Units is safe for
// concurrent use; the request path costs one atomic add and one load.
type Units struct {
	n       atomic.Int64
	ceiling atomic.Int64

	mu       sync.Mutex // serializes ceiling recomputation
	limit    int64
	external int64
}

// NewUnits returns a ceiling of limit units (DefaultUnits in ruralzd).
func NewUnits(limit int64) *Units {
	u := &Units{limit: max(0, limit)}
	u.ceiling.Store(u.limit)
	return u
}

// TryAcquire takes k units with one atomic add, or takes nothing and
// returns false when that would pass the effective ceiling. k <= 0 takes
// nothing and succeeds. A rejection never waits: near the ceiling, the add
// of a concurrent rejected caller may reject another caller until it is
// undone, which errs toward shedding and never lets the holders exceed the
// ceiling.
func (u *Units) TryAcquire(k int64) bool {
	if k <= 0 {
		return true
	}
	if u.n.Add(k) <= u.ceiling.Load() {
		return true
	}
	u.n.Add(-k)
	return false
}

// Acquire is TryAcquire returning ErrFull (RZ-RT-005) on rejection, for the
// composition engine's parallel steps.
func (u *Units) Acquire(k int64) error {
	if u.TryAcquire(k) {
		return nil
	}
	return ErrFull
}

// Release returns k units taken by a successful TryAcquire or Acquire.
func (u *Units) Release(k int64) {
	if k > 0 {
		u.n.Add(-k)
	}
}

// SetExternal lowers the ceiling by n units, the in-flight units the other
// process reported during a handover (spec 04 req 67); 0 restores the full
// ceiling when the other process exits. Units already held are kept: the
// lowered ceiling applies to new acquisitions.
func (u *Units) SetExternal(n int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.external = max(0, n)
	u.ceiling.Store(effective(u.limit, u.external))
}

// InUse returns the units held now (the handover usage report's
// inflightUnits, spec 04 req 67). It is biased upward: a caller rejected at
// the ceiling adds its units and takes them back (TryAcquire's one atomic
// add), so a read in between counts them although nobody holds them, at
// most the units of the callers being rejected at that instant. The other
// process then lowers its ceiling slightly more than needed, which errs
// toward shedding and never toward exceeding the shared ceiling.
func (u *Units) InUse() int64 { return max(0, u.n.Load()) }

// Ceiling returns the effective ceiling: the limit minus the external
// usage.
func (u *Units) Ceiling() int64 { return u.ceiling.Load() }
