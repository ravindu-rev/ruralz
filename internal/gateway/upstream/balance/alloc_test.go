// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"testing"
)

// TestPickAllocations is the allocation gate of 05 test plan item 22: the
// onRoute pick (05 req 10) and every Endpoint selection (05 reqs 11 to 15)
// allocate nothing, including under exclusions, a stale structure and the
// keyless ring-hash pick; HashKey allocates nothing either.
func TestPickAllocations(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("coverage instrumentation changes allocation counts")
	}
	split := NewSplit([]uint32{95, 5})
	src := pcg(1)
	if a := testing.AllocsPerRun(1000, func() { split.Pick(src) }); a != 0 {
		t.Errorf("Split.Pick allocates %.1f", a)
	}
	var sink uint64
	if a := testing.AllocsPerRun(1000, func() { sink = HashKey("user-1234567") }); a != 0 || sink == 0 {
		t.Errorf("HashKey allocates %.1f", a)
	}
	v := &view{grades: []Grade{Down, Avoided, Eligible, Eligible, Eligible, Eligible, Eligible, Eligible}, loads: []int64{3, 1, 4, 1, 5, 9, 2, 6}}
	all := &view{grades: []Grade{Down, Down, Down, Down, Down, Down, Down, Down}}
	for _, alg := range allAlgorithms {
		b := mustNew(t, Config{Algorithm: alg, VirtualNodes: 64, Endpoints: eps(8, ones)})
		key := HashKey("k")
		for _, tv := range []View{nil, v, all} {
			if a := testing.AllocsPerRun(1000, func() { _, _ = b.Load().Select(key, src, tv) }); a != 0 {
				t.Errorf("%v Select allocates %.1f", alg, a)
			}
		}
		if a := testing.AllocsPerRun(1000, func() { _, _ = b.Load().SelectRandom(src, v) }); a != 0 {
			t.Errorf("%v SelectRandom allocates %.1f", alg, a)
		}
		if a := testing.AllocsPerRun(1000, func() { _, _ = b.Load().Select(key, nil, v) }); a != 0 {
			t.Errorf("%v Select with the runtime source allocates %.1f", alg, a)
		}
		// Stale structure: one Endpoint left, one joined.
		if err := b.SetEndpoints(append(eps(8, ones)[1:], Endpoint{"zz", 1}), nil); err != nil {
			t.Fatal(err)
		}
		if a := testing.AllocsPerRun(1000, func() { _, _ = b.Load().Select(key, src, all) }); a != 0 {
			t.Errorf("%v stale Select allocates %.1f", alg, a)
		}
	}
	// The degraded walk over a large structure: the linear pass, and the
	// resumed walk with its bitmap, when the only Eligible Endpoint holds one
	// slot.
	const n = 1000
	grades := make([]Grade, n)
	for i := range grades {
		grades[i] = Down
	}
	grades[777] = Eligible
	light := eps(n, func(i int) uint32 { return []uint32{1000, 1}[b2i(i == 777)] })
	for _, alg := range []Algorithm{RoundRobin, RingHash} {
		b := mustNew(t, Config{Algorithm: alg, VirtualNodes: MinVirtualNodes, Endpoints: light})
		set, v := b.Load(), &view{grades: grades}
		if a := testing.AllocsPerRun(100, func() { _, _ = set.Select(HashKey("k"), src, v) }); a != 0 {
			t.Errorf("%v degraded Select allocates %.1f", alg, a)
		}
	}
}
