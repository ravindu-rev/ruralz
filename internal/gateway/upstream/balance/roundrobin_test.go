// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"context"
	"math"
	"testing"
)

// scheduleProperties checks a schedule against 05 req 12 and returns a
// description of the first violation: length Σ n_i with n_i the quotas;
// every prefix within one slot of each Endpoint's ideal count (smoothness,
// which also bounds runs).
func scheduleProperties(p *Picker) string {
	length := min(MaxScheduleSlots, SlotsPerEndpoint*p.live)
	want := quotas(p.w, p.total, length)
	var slots int
	for _, q := range want {
		slots += int(q)
	}
	if len(p.sched) != slots {
		return "schedule length differs from the quota sum"
	}
	// Between its own slots an Endpoint's count is flat while its ideal
	// count grows, so its extremes sit just before and just after its slots.
	seen := make([]int, len(p.w))
	for t, i := range p.sched {
		rate := float64(want[i]) / float64(slots)
		before := math.Abs(float64(seen[i]) - float64(t)*rate)
		seen[i]++
		after := math.Abs(float64(seen[i]) - float64(t+1)*rate)
		if max(before, after) > 1+1e-9 {
			return "an Endpoint strays more than one slot from its share"
		}
	}
	for j, q := range want {
		if seen[j] != int(q) {
			return "an Endpoint's slot count differs from its quota"
		}
	}
	return ""
}

// TestRoundRobinSchedule covers 05 req 12: L = min(65,536, 64 × E) slots,
// quotas max(1, round(w_i × L / Σw)), smooth interleaving and the 768-byte
// three-Endpoint schedule.
func TestRoundRobinSchedule(t *testing.T) {
	tests := []struct {
		name      string
		list      []Endpoint
		wantSlots int
	}{
		{"three equal (768 bytes)", eps(3, ones), 192},
		{"weighted 1:2:3", eps(3, func(i int) uint32 { return u32(i + 1) }), 192},
		{"one", eps(1, ones), 64},
		{"heavy and light", eps(2, func(i int) uint32 { return []uint32{1, 1000}[i] }), 128},
		{"zero weight", eps(3, u32), 128},
		{"all zero", eps(4, func(int) uint32 { return 0 }), 256},
		{"capped at 65,536", eps(2000, ones), 65536},
		{"many light", eps(20, func(i int) uint32 { return []uint32{50, 1}[min(i, 1)] }), 1280},
		{"floors force slots", eps(1001, func(i int) uint32 { return []uint32{1000000, 1}[min(i, 1)] }), 64064},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := BuildRoundRobin(tt.list, 0)
			if len(p.sched) != tt.wantSlots {
				t.Fatalf("slots = %d, want %d", len(p.sched), tt.wantSlots)
			}
			if p.Bytes() != int64(4*tt.wantSlots) || p.Slots() != tt.wantSlots {
				t.Fatalf("Bytes = %d, Slots = %d", p.Bytes(), p.Slots())
			}
			if msg := scheduleProperties(p); msg != "" {
				t.Fatal(msg)
			}
			if p.Algorithm() != RoundRobin || p.VirtualNodes() != 0 || p.Len() != len(tt.list) {
				t.Fatalf("metadata: %v %d %d", p.Algorithm(), p.VirtualNodes(), p.Len())
			}
		})
	}
	if p := BuildRoundRobin(eps(3, ones), 0); p.Bytes() != 768 {
		t.Fatalf("3 Endpoints use %d bytes, want 768 (05 req 12)", p.Bytes())
	}
	if p := BuildRoundRobin(nil, 5); p.Bytes() != 0 {
		t.Fatalf("empty schedule has %d bytes", p.Bytes())
	}
}

// TestRoundRobinLargeSet covers a set above 65,536 Endpoints, where each
// Endpoint keeps its one slot (n_i ≥ 1).
func TestRoundRobinLargeSet(t *testing.T) {
	p := BuildRoundRobin(eps(70000, ones), 0)
	if len(p.sched) != 70000 {
		t.Fatalf("slots = %d, want 70000", len(p.sched))
	}
	for k, i := range p.sched {
		if int(i) != k {
			t.Fatalf("slot %d holds %d", k, i)
		}
	}
}

// TestRoundRobinCursor covers 05 req 12's pick: an atomic cursor walking the
// schedule, started at the build seed.
func TestRoundRobinCursor(t *testing.T) {
	list := eps(3, func(i int) uint32 { return u32(i + 1) })
	p := BuildRoundRobin(list, 7)
	n := len(p.sched)
	for k := range 2 * n {
		c, ok := p.Pick(0, nil, nil)
		if !ok || c.Index != int(p.sched[(7+k)%n]) || c.Grade != Eligible {
			t.Fatalf("pick %d = %+v, want slot %d", k, c, (7+k)%n)
		}
	}
	// Different seeds start at different slots.
	a, b := BuildRoundRobin(list, 0), BuildRoundRobin(list, 1)
	if a.cursor.Load() == b.cursor.Load() {
		t.Fatal("cursor start ignores the seed")
	}
	// Over one full schedule, counts equal the quotas.
	got := counts(3, n, func() int { c, _ := p.Pick(0, nil, nil); return c.Index })
	for i, q := range quotas(p.w, p.total, 192) {
		if got[i] != int(q) {
			t.Fatalf("counts %v, quotas mismatch at %d", got, i)
		}
	}
}

// TestRoundRobinExclusions covers 05 req 12: a pick skips excluded slots
// for at most one pass, then takes the best Endpoint it saw (panic for the
// pick).
func TestRoundRobinExclusions(t *testing.T) {
	list := eps(4, ones)
	tests := []struct {
		name   string
		grades []Grade
		want   map[int]bool
		grade  Grade
	}{
		{"one down", []Grade{Down, Eligible, Eligible, Eligible}, map[int]bool{1: true, 2: true, 3: true}, Eligible},
		{"only one eligible", []Grade{Down, Avoided, Eligible, Down}, map[int]bool{2: true}, Eligible},
		{"avoided relaxed", []Grade{Down, Avoided, Down, Down}, map[int]bool{1: true}, Avoided},
		{"panic", []Grade{Down, DownAvoided, Down, Down}, map[int]bool{0: true, 2: true, 3: true}, Down},
		{"all down and avoided", []Grade{DownAvoided, DownAvoided, DownAvoided, DownAvoided}, map[int]bool{0: true, 1: true, 2: true, 3: true}, DownAvoided},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := BuildRoundRobin(list, 3)
			v := &view{grades: tt.grades}
			for range 3 * len(p.sched) {
				c, ok := p.Pick(0, nil, v)
				if !ok || !tt.want[c.Index] || c.Grade != tt.grade {
					t.Fatalf("pick %+v, want one of %v with grade %d", c, tt.want, tt.grade)
				}
			}
		})
	}
	// The walk stops once it graded every Endpoint: with every Endpoint Down,
	// a pick grades each once instead of walking 256 slots.
	p := BuildRoundRobin(list, 0)
	v := &view{grades: []Grade{Down, Down, Down, Down}}
	p.Pick(0, nil, v)
	if calls := v.calls.Load(); calls > 4 {
		t.Fatalf("graded %d times for 4 Endpoints", calls)
	}
}

// TestRoundRobinCursorPerBuild covers 05 req 12 "cursor start randomized
// per build" through the Balancer: Balancers given different Config.Rand
// sources start at different slots, each rebuild draws a new start, and
// every start lies inside the schedule.
func TestRoundRobinCursorPerBuild(t *testing.T) {
	list := eps(5, ones) // 320 slots
	starts := map[uint64]bool{}
	for seed := range uint64(16) {
		b := mustNew(t, Config{Algorithm: RoundRobin, Endpoints: list, Rand: pcg(seed + 1)})
		p := b.Load().p
		start := p.cursor.Load()
		if start >= uint64(len(p.sched)) {
			t.Fatalf("seed %d: cursor starts at %d of %d slots", seed, start, len(p.sched))
		}
		starts[start] = true
	}
	if len(starts) < 8 {
		t.Fatalf("16 Balancers share %d cursor starts: the build seed ignores Config.Rand", len(starts))
	}
	b := mustNew(t, Config{Algorithm: RoundRobin, Endpoints: list, Rand: pcg(99)})
	starts = map[uint64]bool{b.Load().p.cursor.Load(): true}
	now := t0
	for k := range 16 {
		if err := b.SetEndpoints(eps(6-k%2, ones), nil); err != nil {
			t.Fatal(err)
		}
		now = now.Add(RebuildInterval)
		if done, err := b.Rebuild(context.Background(), nil, now); !done || err != nil {
			t.Fatalf("Rebuild = %v, %v", done, err)
		}
		p := b.Load().p
		start := p.cursor.Load()
		if start >= uint64(len(p.sched)) {
			t.Fatalf("rebuild %d: cursor starts at %d of %d slots", k, start, len(p.sched))
		}
		starts[start] = true
	}
	if len(starts) < 8 {
		t.Fatalf("17 builds share %d cursor starts: rebuilds reuse the seed", len(starts))
	}
}
