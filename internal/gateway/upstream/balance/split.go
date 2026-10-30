// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"math/rand/v2"
	"slices"
)

// linearSplit is the entry count up to which Split.Pick scans instead of
// searching.
const linearSplit = 8

// Split is the weighted pick of a Route's plain upstreams entries in
// onRoute (docs/architecture/09-traffic-management-and-resilience.md "Load
// balancing"): one entry per pick by weighted random under the package
// weight rule, applied to the entries the pick may take. An entry of weight
// 0 is never picked while another of them weighs more, and when each of
// them weighs 0 they all weigh 1. Pick may take every entry; PickExcept
// takes only the entries a request has not tried, so a request whose leg
// was refused before any attempt picks again among the others, each entry
// at most once. A Split is immutable, safe for concurrent use and
// allocation-free per pick.
type Split struct {
	weights []uint32 // every entry's weight, by entry index
	idx     []int32  // entry index of each weighted entry
	cum     []uint64 // cumulative weights of the weighted entries
	total   uint64
}

// NewSplit builds the split over the entries' weights (the caller applies
// the weight default 1).
func NewSplit(weights []uint32) *Split {
	s := &Split{weights: slices.Clone(weights)}
	for i, w := range weights {
		if w == 0 {
			continue
		}
		s.total += uint64(w)
		s.idx = append(s.idx, int32(i))
		s.cum = append(s.cum, s.total)
	}
	return s
}

// Len returns the number of entries.
func (s *Split) Len() int { return len(s.weights) }

// Entries returns the number of entries with weight > 0.
func (s *Split) Entries() int { return len(s.idx) }

// Pick returns the index of an entry chosen by weight, or of any entry with
// equal chance when every weight is 0; it returns false only for a Split
// without entries. src supplies randomness (nil: the runtime generator); a
// single candidate needs none.
func (s *Split) Pick(src rand.Source) (int, bool) {
	switch len(s.idx) {
	case 0:
		return s.uniform(src)
	case 1:
		return int(s.idx[0]), true
	}
	r := uint64n(source(src), s.total)
	if len(s.cum) <= linearSplit {
		for k, c := range s.cum {
			if r < c {
				return int(s.idx[k]), true
			}
		}
	}
	lo, hi := 0, len(s.cum)
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if s.cum[m] <= r {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return int(s.idx[lo]), true
}

// uniform picks any entry with equal chance, the rule when every weight is
// 0.
func (s *Split) uniform(src rand.Source) (int, bool) {
	switch n := len(s.weights); n {
	case 0:
		return -1, false
	case 1:
		return 0, true
	default:
		return int(uint64n(source(src), uint64(n))), true //nolint:gosec // G115: the draw is below n, an int.
	}
}

// PickExcept is Pick over the entries tried does not hold: one of them by
// weight, or with equal chance when each of them weighs 0, so a weight-0
// entry is picked only once every weighted entry was tried. It never
// returns an entry tried holds, and returns false when tried holds every
// entry. A nil tried holds none. src is as for Pick; a single candidate
// needs no randomness.
func (s *Split) PickExcept(src rand.Source, tried *Tried) (int, bool) {
	var total, zero uint64
	weighted := 0
	for i, w := range s.weights {
		switch {
		case tried.Has(i):
		case w > 0:
			total += uint64(w)
			weighted++
		default:
			zero++
		}
	}
	switch {
	case weighted > 0:
		var r uint64
		if weighted > 1 {
			r = uint64n(source(src), total)
		}
		for i, w := range s.weights {
			if w == 0 || tried.Has(i) {
				continue
			}
			if r < uint64(w) {
				return i, true
			}
			r -= uint64(w)
		}
	case zero > 0:
		var r uint64
		if zero > 1 {
			r = uint64n(source(src), zero)
		}
		for i, w := range s.weights {
			if w != 0 || tried.Has(i) {
				continue
			}
			if r == 0 {
				return i, true
			}
			r--
		}
	}
	return -1, false
}

// triedLow is the number of entries a Tried holds without allocating.
const triedLow = 64

// Tried is the set of entries one request has tried, for Split.PickExcept.
// The zero value is empty, and entries below 64 take no allocation, so a
// request keeps its set on its own stack.
type Tried struct {
	low  uint64   // entries 0 to 63, one bit each
	high []uint64 // entries from 64, one bit each, 64 per word
}

// Add puts entry i in the set; a negative i is ignored.
func (t *Tried) Add(i int) {
	switch {
	case i < 0:
	case i < triedLow:
		t.low |= 1 << i
	default:
		w := (i - triedLow) / 64
		for len(t.high) <= w {
			t.high = append(t.high, 0)
		}
		t.high[w] |= 1 << ((i - triedLow) % 64)
	}
}

// Has reports whether entry i is in the set; a nil Tried holds none.
func (t *Tried) Has(i int) bool {
	switch {
	case t == nil || i < 0:
		return false
	case i < triedLow:
		return t.low&(1<<i) != 0
	default:
		w := (i - triedLow) / 64
		return w < len(t.high) && t.high[w]&(1<<((i-triedLow)%64)) != 0
	}
}
