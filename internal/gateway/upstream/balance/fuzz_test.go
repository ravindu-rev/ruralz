// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"encoding/binary"
	"fmt"
	"slices"
	"testing"
)

// weightsFrom decodes up to max weights (uint16 each) from data.
func weightsFrom(data []byte, maxN int) []uint32 {
	n := min(len(data)/2, maxN)
	w := make([]uint32, n)
	for i := range w {
		w[i] = uint32(binary.LittleEndian.Uint16(data[2*i:]))
	}
	return w
}

// listFrom returns a normalized Endpoint list with the given weights.
func listFrom(w []uint32) []Endpoint {
	return eps(len(w), func(i int) uint32 { return w[i] })
}

// FuzzRoundRobinSchedule is 05 test plan item 11's round-robin property
// (05 req 12): for any weights the schedule has the apportioned length,
// every weighted Endpoint holds its quota and stays within one slot of its
// share in every prefix, and weight-0 Endpoints hold none.
func FuzzRoundRobinSchedule(f *testing.F) {
	f.Add([]byte{1, 0, 1, 0, 1, 0}, uint64(0))
	f.Add([]byte{0xe8, 0x03, 1, 0}, uint64(5))
	f.Add([]byte{0, 0, 7, 0, 0, 0, 3, 0}, uint64(9))
	f.Add([]byte{0, 0, 0, 0}, uint64(1))
	f.Fuzz(func(t *testing.T, data []byte, seed uint64) {
		w := weightsFrom(data, 64)
		if len(w) == 0 {
			return
		}
		p := BuildRoundRobin(listFrom(w), seed)
		if msg := scheduleProperties(p); msg != "" {
			t.Fatalf("weights %v: %s", w, msg)
		}
		for k, i := range p.sched {
			if p.w[i] == 0 {
				t.Fatalf("slot %d holds weight-0 Endpoint %d", k, i)
			}
		}
		if len(p.sched) > 0 && p.cursor.Load() >= uint64(len(p.sched)) {
			t.Fatal("cursor starts outside the schedule")
		}
	})
}

// FuzzRingBuild is 05 test plan item 11's ring property (05 req 14): builds
// from any order are identical, sorted, and hold each Endpoint's quota.
func FuzzRingBuild(f *testing.F) {
	f.Add([]byte{1, 0, 2, 0, 3, 0}, uint64(0), uint16(64))
	f.Add([]byte{0, 0, 9, 0}, uint64(3), uint16(1024))
	f.Add([]byte{5, 0}, uint64(8), uint16(0))
	f.Fuzz(func(t *testing.T, data []byte, seed uint64, v uint16) {
		w := weightsFrom(data, 40)
		list := listFrom(w)
		a := BuildRing(list, int(v))
		shuffled := slices.Clone(list)
		shuffle(pcg(seed), shuffled)
		b := BuildRing(shuffled, int(v))
		if !slices.Equal(a.ring, b.ring) {
			t.Fatal("rings differ across input orders")
		}
		want := quotas(a.w, a.total, min(MaxRingNodes, a.v*a.live))
		got := make([]uint32, len(a.w))
		for k, n := range a.ring {
			got[n.idx]++
			if k > 0 && a.ring[k-1].pos > n.pos {
				t.Fatal("ring not sorted")
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("virtual nodes %v, want %v", got, want)
		}
	})
}

// FuzzSelectGrades is the selection property of 05 req 11: for any
// algorithm, weights, grades, key and set change, Select returns an
// Endpoint of the current set with effective weight > 0 and the lowest grade
// present among those Endpoints.
func FuzzSelectGrades(f *testing.F) {
	f.Add(uint8(0), []byte{1, 0, 1, 0, 1, 0, 1, 0}, []byte{0, 2, 1, 3}, uint64(7), uint64(0))
	f.Add(uint8(2), []byte{3, 0, 0, 0, 2, 0}, []byte{2, 2, 1}, uint64(1), uint64(5))
	f.Add(uint8(1), []byte{1, 0, 1, 0}, []byte{3, 3}, uint64(99), uint64(3))
	f.Add(uint8(3), []byte{9, 0, 1, 0, 4, 0}, []byte{1, 0, 2}, uint64(4), uint64(12))
	f.Fuzz(func(t *testing.T, algByte uint8, data, gradeBytes []byte, key, churn uint64) {
		w := weightsFrom(data, 24)
		if len(w) == 0 {
			return
		}
		alg := Algorithm(algByte % 4)
		b, err := New(Config{Algorithm: alg, VirtualNodes: 16, Endpoints: listFrom(w), Rand: pcg(key), Now: t0})
		if err != nil {
			t.Fatal(err)
		}
		cur := listFrom(w)
		if churn%3 != 0 {
			// Drop the Endpoints named by churn's bits and add one.
			var kept []Endpoint
			for i, ep := range cur {
				if churn>>(i%64)&1 == 0 {
					kept = append(kept, ep)
				}
			}
			kept = append(kept, Endpoint{Identity: fmt.Sprintf("zz-%d", churn%7), Weight: u32(int(churn % 5))})
			cur = kept
			if err := b.SetEndpoints(cur, nil); err != nil {
				t.Fatal(err)
			}
		}
		grades := make([]Grade, len(cur))
		for i := range grades {
			if len(gradeBytes) > 0 {
				grades[i] = Grade(gradeBytes[i%len(gradeBytes)] % 4)
			}
		}
		curW, _, _ := effectiveWeights(cur)
		best := gone
		for i, g := range grades {
			if curW[i] > 0 {
				best = min(best, g)
			}
		}
		v := &view{grades: grades, loads: []int64{int64(intn(pcg(key), 3)), int64(intn(pcg(churn), 4))}}
		src := pcg(churn)
		for range 8 {
			c, err := b.Load().Select(HashKey(fmt.Sprint(key)), src, v)
			if err != nil {
				t.Fatal(err)
			}
			if c.Index < 0 || c.Index >= len(cur) || curW[c.Index] == 0 {
				t.Fatalf("picked %d of %d (weights %v)", c.Index, len(cur), curW)
			}
			if c.Grade != best || grades[c.Index] != best {
				t.Fatalf("picked grade %d, lowest present %d", c.Grade, best)
			}
			key = key*6364136223846793005 + 1442695040888963407
		}
	})
}

// FuzzPlanBudget is the budget plan property of 05 req 17: V lies in [64,
// 1,024]; the plan fits whenever the schedules leave room; fallbacks are a
// prefix of the largest-first order and minimal (restoring the last one
// would not fit at 64).
func FuzzPlanBudget(f *testing.F) {
	f.Add(int64(1<<20), int64(0), []byte{100, 0, 50, 0, 50, 0, 10, 0})
	f.Add(int64(0), int64(4<<20), []byte{64, 0, 64, 0})
	f.Add(int64(1), int64(1<<30), []byte{1, 0})
	f.Fuzz(func(t *testing.T, room, schedules int64, data []byte) {
		sizes := weightsFrom(data, 32)
		rings := make(map[string]int, len(sizes))
		for i, s := range sizes {
			rings[fmt.Sprintf("r%02d", i)] = int(s)
		}
		room = max(0, min(room, 1<<40))
		schedules = max(0, min(schedules, 1<<40))
		budget := CopyOnWriteReserve + schedules + room
		p := PlanBudget(budget, schedules, rings)
		if p.V < MinVirtualNodes || p.V > MaxVirtualNodes {
			t.Fatalf("V %d out of range", p.V)
		}
		if !p.Fits() {
			t.Fatalf("plan does not fit: %+v", p)
		}
		order := ringOrder(rings)
		for k, name := range order {
			if p.Fallback[name] && k > 0 && !p.Fallback[order[k-1]] {
				t.Fatalf("fallback %s skips larger ring %s", name, order[k-1])
			}
		}
		if n := len(p.Fallback); n > 0 {
			restored := activeRings(rings, p.Fallback)
			restored = append(restored, order[n-1])
			if ringsBytes(rings, restored, MinVirtualNodes) <= room {
				t.Fatalf("fallback of %s was not needed", order[n-1])
			}
		}
	})
}

// FuzzReplan is the replan property of 05 req 17 after an Endpoint-set
// change: V lies in [64, 1,024], the plan fits whenever the schedules leave
// room, fallbacks name configured rings only, and no ring stays in fallback
// once the fresh plan needs none at v ≥ 2 × 64.
func FuzzReplan(f *testing.F) {
	f.Add(int64(16*64*120), []byte{200, 0, 150, 0}, []byte{5, 0, 5, 0})
	f.Add(int64(16*64*120), []byte{100, 0, 50, 0, 50, 0, 10, 0}, []byte{5, 0, 50, 0, 50, 0, 10, 0})
	f.Add(int64(16*64*120), []byte{0xf4, 1, 5, 0, 5, 0}, []byte{10, 0, 5, 0, 5, 0})
	f.Add(int64(1<<20), []byte{1, 0}, []byte{0xff, 0xff, 1, 0})
	f.Fuzz(func(t *testing.T, room int64, before, after []byte) {
		ringsOf := func(data []byte) map[string]int {
			rings := map[string]int{}
			for i, s := range weightsFrom(data, 32) {
				rings[fmt.Sprintf("r%02d", i)] = int(s)
			}
			return rings
		}
		room = max(0, min(room, 1<<40))
		budget := CopyOnWriteReserve + room
		prev := PlanBudget(budget, 0, ringsOf(before))
		rings := ringsOf(after)
		p := Replan(prev, budget, 0, rings)
		if p.V < MinVirtualNodes || p.V > MaxVirtualNodes || !p.Fits() {
			t.Fatalf("Replan from %+v: %+v", prev, p)
		}
		for name := range p.Fallback {
			if _, ok := rings[name]; !ok {
				t.Fatalf("fallback names unconfigured ring %s", name)
			}
		}
		if fresh := PlanBudget(budget, 0, rings); !fresh.Degraded() && fresh.V >= 2*MinVirtualNodes && p.Degraded() {
			t.Fatalf("fallback %v stuck though the fresh plan needs none at V %d", p.Fallback, fresh.V)
		}
	})
}

// FuzzSplit is the weighted leg pick property of 05 req 10: a pick never
// lands on weight 0 and fails only when every weight is 0.
func FuzzSplit(f *testing.F) {
	f.Add([]byte{95, 0, 5, 0}, uint64(1))
	f.Add([]byte{0, 0, 0, 0}, uint64(2))
	f.Add([]byte{0, 0, 1, 0, 0, 0, 1, 0, 2, 0, 3, 0, 4, 0, 5, 0, 6, 0, 7, 0}, uint64(3))
	f.Fuzz(func(t *testing.T, data []byte, seed uint64) {
		w := weightsFrom(data, 64)
		s := NewSplit(w)
		live := slices.ContainsFunc(w, func(x uint32) bool { return x > 0 })
		src := pcg(seed)
		for range 16 {
			i, ok := s.Pick(src)
			if ok != live {
				t.Fatalf("Pick ok %v with weights %v", ok, w)
			}
			if ok && w[i] == 0 {
				t.Fatalf("picked weight-0 entry %d", i)
			}
		}
	})
}
