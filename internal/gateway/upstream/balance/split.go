// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import "math/rand/v2"

// linearSplit is the entry count up to which Split.Pick scans instead of
// searching.
const linearSplit = 8

// Split is the weighted pick of a Route's plain upstreams entries in onRoute
// (05 req 10): one entry per request by weighted random over the entries
// with weight > 0; the pick is final for the request. It is immutable, safe
// for concurrent use and allocation-free per pick.
type Split struct {
	idx   []int32  // entry index of each weighted entry
	cum   []uint64 // cumulative weights of the weighted entries
	total uint64
}

// NewSplit builds the split over the entries' weights (the caller applies
// the weight default 1).
func NewSplit(weights []uint32) *Split {
	s := &Split{}
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

// Entries returns the number of entries with weight > 0.
func (s *Split) Entries() int { return len(s.idx) }

// Pick returns the chosen entry's index, and false when every weight is 0
// (503 RZ-UP-008, 05 req 10). src supplies randomness (nil: the runtime
// generator); a single weighted entry needs none.
func (s *Split) Pick(src rand.Source) (int, bool) {
	switch len(s.idx) {
	case 0:
		return -1, false
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
