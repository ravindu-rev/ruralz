// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
)

// Endpoint is one member of an Upstream's Endpoint set as the balancer
// sees it.
type Endpoint struct {
	// Identity is the Endpoint's stable name: the configured address of a
	// static Endpoint, ip:port of an A/AAAA answer or target:port of an SRV
	// answer (05 reqs 5 and 6). Structures are ordered by it (05 req 8), and
	// ring positions derive from it (05 req 14).
	Identity string
	// Weight is endpoints[].weight or the SRV weight.
	Weight uint32
}

// ErrNotNormalized reports an Endpoint list that is not sorted by identity
// with unique identities; Normalize produces the accepted form.
var ErrNotNormalized = errors.New("balance: endpoints must be sorted by identity with unique identities")

// Normalize returns the canonical form of an Endpoint list: a new slice
// sorted by identity (05 req 8), with Endpoints of equal identity merged
// into one whose weight is their sum (saturating). Every Node given the same
// set, in any order, gets the same list and therefore builds identical
// structures.
func Normalize(eps []Endpoint) []Endpoint {
	out := slices.Clone(eps)
	slices.SortFunc(out, func(a, b Endpoint) int { return cmp.Compare(a.Identity, b.Identity) })
	merged := out[:0]
	for _, ep := range out {
		if n := len(merged); n > 0 && merged[n-1].Identity == ep.Identity {
			sum := uint64(merged[n-1].Weight) + uint64(ep.Weight)
			merged[n-1].Weight = uint32(min(sum, math.MaxUint32))
			continue
		}
		merged = append(merged, ep)
	}
	return slices.Clip(merged)
}

// checkNormalized reports whether eps is in Normalize's form.
func checkNormalized(eps []Endpoint) error {
	for i := 1; i < len(eps); i++ {
		if eps[i-1].Identity >= eps[i].Identity {
			return fmt.Errorf("%w: %q at %d follows %q", ErrNotNormalized, eps[i].Identity, i, eps[i-1].Identity)
		}
	}
	return nil
}

// effectiveWeights applies the package weight rule: weights are kept as
// configured unless all are 0, in which case each weighs 1. It returns the
// weights, their sum and the number of Endpoints weighing more than 0.
func effectiveWeights(eps []Endpoint) (w []uint32, total uint64, live int) {
	w = make([]uint32, len(eps))
	for i, ep := range eps {
		w[i] = ep.Weight
		total += uint64(ep.Weight)
	}
	if total == 0 {
		for i := range w {
			w[i] = 1
		}
		total = uint64(len(w))
	}
	for _, x := range w {
		if x > 0 {
			live++
		}
	}
	return w, total, live
}

// quotas apportions length slots over the weights (05 reqs 12 and 14).
// Each weighted Endpoint starts at n_i = max(1, round(length × w_i /
// total)); when those do not sum to length, the Endpoint most over-served
// relative to its exact share gives up a slot (never its last one), or the
// most under-served takes one, until they do. Without the max(1, …) floor
// forcing extra slots, every quota ends within one slot of n_i. When there
// are at least as many weighted Endpoints as slots, each gets one. Endpoints
// of weight 0 get none. Integer arithmetic keeps the result identical on
// every platform.
func quotas(w []uint32, total uint64, length int) []uint32 {
	n := make([]uint32, len(w))
	if total == 0 || length <= 0 {
		return n
	}
	shares := make([]share, 0, len(w))
	var sum, live int
	for i, x := range w {
		if x == 0 {
			continue
		}
		live++
		// length × w_i < 2^48, so the high word is 0 and Div64 cannot fault.
		hi, lo := bits.Mul64(uint64(length), uint64(x))
		q, r := bits.Div64(hi, lo, total)
		c := q
		if r >= total-r {
			c++
		}
		c = max(1, c)
		n[i] = uint32(min(c, math.MaxUint32))
		sum += int(n[i])
		shares = append(shares, share{idx: i, q: q, r: r})
	}
	if live >= length {
		for i, x := range w {
			if x > 0 {
				n[i] = 1
			}
		}
		return n
	}
	if sum == length {
		return n
	}
	over := sum > length
	h := shareHeap{n: n, over: over}
	for _, s := range shares {
		if !over || n[s.idx] > 1 {
			h.push(s)
		}
	}
	for ; sum != length && len(h.items) > 0; h.fix() {
		i := h.items[0].idx
		if over {
			n[i]--
			sum--
			if n[i] == 1 {
				h.pop()
			}
			continue
		}
		n[i]++
		sum++
	}
	return n
}

// share is an Endpoint's exact share q + r/total of the slots.
type share struct {
	idx  int
	q, r uint64
}

// shareHeap orders shares by how far their current quota strays from them:
// most over-served first when over, most under-served first otherwise, ties
// by index.
type shareHeap struct {
	items []share
	n     []uint32
	over  bool
}

// before reports whether a strays further than b. The quota minus the exact
// share is d − r/total with d = n − q and 0 ≤ r < total, so d decides and r
// breaks ties; d_a > d_b is compared as n_a + q_b > n_b + q_a to stay
// unsigned.
func (h *shareHeap) before(a, b share) bool {
	da, db := uint64(h.n[a.idx])+b.q, uint64(h.n[b.idx])+a.q
	if da != db {
		return (da > db) == h.over
	}
	if a.r != b.r {
		return (a.r < b.r) == h.over
	}
	return a.idx < b.idx
}

// push adds s.
func (h *shareHeap) push(s share) {
	h.items = append(h.items, s)
	h.up(len(h.items) - 1)
}

// pop removes the root.
func (h *shareHeap) pop() {
	last := len(h.items) - 1
	h.items[0] = h.items[last]
	h.items = h.items[:last]
	h.fix()
}

// up restores the order above i.
func (h *shareHeap) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !h.before(h.items[i], h.items[parent]) {
			return
		}
		h.items[i], h.items[parent] = h.items[parent], h.items[i]
		i = parent
	}
}

// fix restores the order below the root after its quota changed.
func (h *shareHeap) fix() {
	for i := 0; ; {
		m, l := i, 2*i+1
		if l < len(h.items) && h.before(h.items[l], h.items[m]) {
			m = l
		}
		if r := l + 1; r < len(h.items) && h.before(h.items[r], h.items[m]) {
			m = r
		}
		if m == i {
			return
		}
		h.items[i], h.items[m] = h.items[m], h.items[i]
		i = m
	}
}

// ScheduleBytes is the nominal size of a round-robin schedule over the
// given number of Endpoints: 4 bytes × min(65,536, 64 × Endpoints) (05 req
// 12). It is the S term of the budget plan (05 req 17).
func ScheduleBytes(endpoints int) int64 {
	if endpoints <= 0 {
		return 0
	}
	slots := max(int64(endpoints), min(int64(MaxScheduleSlots), int64(SlotsPerEndpoint)*int64(endpoints)))
	return SlotBytes * slots
}

// RingBytes is the nominal size of a ring-hash ring over the given number of
// Endpoints at v virtual nodes per Endpoint: 16 bytes × min(65,536, v ×
// Endpoints), never fewer than one virtual node per Endpoint (05 req 14).
func RingBytes(v, endpoints int) int64 {
	if endpoints <= 0 || v <= 0 {
		return 0
	}
	nodes := max(int64(endpoints), min(int64(MaxRingNodes), int64(v)*int64(endpoints)))
	return VirtualNodeBytes * nodes
}
