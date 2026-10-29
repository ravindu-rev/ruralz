// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
)

// spec.when decision states, by Policy.Index.
const (
	undecided uint8 = iota
	runs
	skipped
)

// decisions records each Policy's spec.when decision by Policy.Index. It
// grows to the largest index seen and keeps its memory across reuse.
type decisions []uint8

// get returns the decision of index i: decided is false until set.
func (d decisions) get(i int) (decided, skip bool) {
	if i < 0 || i >= len(d) {
		return false, false
	}
	s := d[i]
	return s != undecided, s == skipped
}

// set records the decision of index i; a negative index records nothing.
func (d *decisions) set(i int, skip bool) {
	if i < 0 {
		return
	}
	*d = grow(*d, i)
	if skip {
		(*d)[i] = skipped
	} else {
		(*d)[i] = runs
	}
}

// reset clears every decision, keeping the memory.
func (d *decisions) reset() {
	clear(*d)
	*d = (*d)[:0]
}

// slots are the PolicyState slots by Policy.Index; spare serves a call
// outside any Policy (no Enter yet, or a negative index), so PolicyState
// is never nil.
type slots struct {
	s     []any
	spare any
}

// at returns the slot of p, growing the slots to hold its index.
func (sl *slots) at(p *snapshot.Policy) *any {
	if p == nil || p.Index < 0 {
		return &sl.spare
	}
	sl.s = grow(sl.s, p.Index)
	return &sl.s[p.Index]
}

// reset drops every value the slots hold, keeping the memory.
func (sl *slots) reset() {
	clear(sl.s)
	sl.s = sl.s[:0]
	sl.spare = nil
}

// grow returns s with at least i+1 elements. Elements between len and cap
// are always zero (reset clears the whole length before truncating), so
// the new elements read as zero.
func grow[T any](s []T, i int) []T {
	if i < len(s) {
		return s
	}
	if i < cap(s) {
		return s[:i+1]
	}
	n := make([]T, i+1, max(2*cap(s), i+1))
	copy(n, s)
	return n
}

// before reports whether a precedes b in request order: Filter class,
// then scope, then list position (foundation pack 8.12).
func before(a, b *snapshot.Policy) bool {
	switch {
	case a.Class != b.Class:
		return a.Class < b.Class
	case a.Scope != b.Scope:
		return a.Scope < b.Scope
	default:
		return a.Position < b.Position
	}
}
