// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"unsafe"
)

// ringOwner returns the identity owning key on ring p with every Endpoint
// Eligible.
func ringOwner(p *Picker, key uint64) string {
	c, _ := p.Pick(key, nil, nil)
	return p.Identity(c.Index)
}

// keys returns n distinct ring keys.
func keys(n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = HashKey(fmt.Sprintf("user-%d", i))
	}
	return out
}

// TestRingStructure covers the ring layout of 05 req 14: 16-byte virtual
// nodes, min(65,536, v × E) of them, Endpoint i owning max(1, round(v × E ×
// w_i / Σw)) at positions FNV-1a-64(identity + "#" + j) through splitmix64,
// sorted by position.
func TestRingStructure(t *testing.T) {
	if size := unsafe.Sizeof(vnode{}); size != VirtualNodeBytes {
		t.Fatalf("virtual node is %d bytes, want 16", size)
	}
	tests := []struct {
		name      string
		list      []Endpoint
		v         int
		wantNodes int
	}{
		{"three at 64", eps(3, ones), 64, 192},
		{"weighted at 1,024", eps(4, func(i int) uint32 { return u32(1 << i) }), 1024, 4096},
		{"capped", eps(100, ones), 1024, 65536}, // 655.36 each: 36 get 656, 64 get 655
		{"default v", eps(2, ones), 0, 2048},
		{"zero weight", eps(3, u32), 64, 128},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := BuildRing(tt.list, tt.v)
			if len(p.ring) != tt.wantNodes || p.Bytes() != int64(16*tt.wantNodes) {
				t.Fatalf("nodes = %d (%d bytes), want %d", len(p.ring), p.Bytes(), tt.wantNodes)
			}
			v := tt.v
			if v <= 0 {
				v = MaxVirtualNodes
			}
			if p.VirtualNodes() != v {
				t.Fatalf("VirtualNodes = %d, want %d", p.VirtualNodes(), v)
			}
			want := quotas(p.w, p.total, min(MaxRingNodes, v*p.live))
			got := make([]uint32, len(p.w))
			for k, n := range p.ring {
				got[n.idx]++
				if k > 0 && p.ring[k-1].pos > n.pos {
					t.Fatal("ring not sorted by position")
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("virtual nodes per Endpoint %v, want %v", got, want)
			}
			// Each Endpoint's positions are exactly j = 0..n_i-1.
			for i, q := range want {
				pos := map[uint64]bool{}
				for _, n := range p.ring {
					if int(n.idx) == i {
						pos[n.pos] = true
					}
				}
				for j := range int(q) {
					if !pos[stdHash(fmt.Sprintf("%s#%d", p.Identity(i), j))] {
						t.Fatalf("Endpoint %d lacks virtual node %d", i, j)
					}
				}
			}
		})
	}
}

// TestRingDeterministicAcrossNodes covers cross-Node consistency (05 req 8
// and test plan item 11): builds from shuffled inputs are identical.
func TestRingDeterministicAcrossNodes(t *testing.T) {
	list := eps(50, func(i int) uint32 { return u32(1 + i%7) })
	a := BuildRing(list, 256)
	src := pcg(3)
	for range 5 {
		shuffled := slices.Clone(list)
		shuffle(src, shuffled)
		b := BuildRing(shuffled, 256)
		if !slices.Equal(a.ring, b.ring) || !slices.Equal(a.ids, b.ids) {
			t.Fatal("rings differ across shuffled inputs")
		}
	}
}

// TestRingLookup covers 05 req 14's lookup: the first position at or after
// the key, wrapping past the last position.
func TestRingLookup(t *testing.T) {
	p := BuildRing(eps(5, ones), 64)
	for k, n := range p.ring {
		if got := p.ringStart(n.pos); p.ring[got].pos != n.pos {
			t.Fatalf("exact position %d starts at %d", k, got)
		}
		if n.pos > 0 {
			if got := p.ringStart(n.pos - 1); p.ring[got].pos < n.pos-1 || (got > 0 && p.ring[got-1].pos >= n.pos-1) {
				t.Fatalf("key before position %d starts at %d", k, got)
			}
		}
	}
	last := p.ring[len(p.ring)-1].pos
	if last < math.MaxUint64 && p.ringStart(last+1) != 0 {
		t.Fatal("a key past the last position does not wrap")
	}
	c, ok := p.Pick(p.ring[3].pos, nil, nil)
	if !ok || c.Index != int(p.ring[3].idx) {
		t.Fatalf("pick at a position = %+v, want Endpoint %d", c, p.ring[3].idx)
	}
}

// TestRingWalksPastExcluded covers 05 req 14: the lookup walks forward past
// excluded Endpoints, for at most one full ring, keeping the best grade.
func TestRingWalksPastExcluded(t *testing.T) {
	p := BuildRing(eps(6, ones), 64)
	for _, key := range keys(500) {
		first, _ := p.Pick(key, nil, nil)
		grades := make([]Grade, 6)
		grades[first.Index] = Down
		c, _ := p.Pick(key, nil, &view{grades: grades})
		// The expected owner is the next virtual node of another Endpoint.
		k := p.ringStart(key)
		for int(p.ring[k].idx) == first.Index {
			k = (k + 1) % len(p.ring)
		}
		if c.Index != int(p.ring[k].idx) || c.Grade != Eligible {
			t.Fatalf("key %#x: pick %+v, want %d", key, c, p.ring[k].idx)
		}
	}
	// Nothing eligible: one full ring, then the first of the lowest grade.
	v := &view{grades: []Grade{DownAvoided, Down, DownAvoided, Down, DownAvoided, Avoided}}
	c, _ := p.Pick(12345, nil, v)
	if c.Index != 5 || c.Grade != Avoided {
		t.Fatalf("relaxed pick %+v, want Endpoint 5 Avoided", c)
	}
	if calls := v.calls.Load(); calls > 6 {
		t.Fatalf("graded %d times for 6 Endpoints", calls)
	}
}

// movedFraction returns the fraction of keys whose owner differs between two
// rings.
func movedFraction(a, b *Picker, ks []uint64) (moved float64, toNew map[string]int) {
	toNew = map[string]int{}
	var n int
	for _, k := range ks {
		if oa, ob := ringOwner(a, k), ringOwner(b, k); oa != ob {
			n++
			toNew[ob]++
		}
	}
	return float64(n) / float64(len(ks)), toNew
}

// TestRingStabilityOnAdd covers ring stability under churn (05 test plan
// item 11): adding one Endpoint moves at most 1/E + ε of the keys, and
// without the 65,536 cap every moved key moves to the new Endpoint.
func TestRingStabilityOnAdd(t *testing.T) {
	ks := keys(100000)
	tests := []struct {
		name    string
		e, v    int
		epsilon float64
		onlyNew bool
	}{
		{"uncapped", 10, 1024, 0.01, true},
		{"low v", 20, 64, 0.02, true},
		{"capped", 100, 1024, 0.02, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := BuildRing(eps(tt.e, ones), tt.v)
			after := BuildRing(eps(tt.e+1, ones), tt.v)
			moved, toNew := movedFraction(before, after, ks)
			if limit := 1/float64(tt.e+1) + tt.epsilon; moved > limit {
				t.Fatalf("moved %.4f of keys, limit %.4f", moved, limit)
			}
			newID := after.Identity(tt.e)
			if tt.onlyNew && len(toNew) != 1 {
				t.Fatalf("keys moved to %v, want only %s", toNew, newID)
			}
			if toNew[newID] == 0 {
				t.Fatal("no key moved to the new Endpoint")
			}
		})
	}
}

// TestRingStabilityOnRemove covers churn the other way: removing one
// Endpoint moves only its keys.
func TestRingStabilityOnRemove(t *testing.T) {
	ks := keys(50000)
	full := eps(12, ones)
	before := BuildRing(full, 512)
	removed := full[4].Identity
	after := BuildRing(slices.Delete(slices.Clone(full), 4, 5), 512)
	for _, k := range ks {
		if oa, ob := ringOwner(before, k), ringOwner(after, k); oa != ob && oa != removed {
			t.Fatalf("key %#x moved from %s to %s", k, oa, ob)
		}
	}
}

// TestRingWeightShare covers test plan item 11: at v = 1,024 each
// Endpoint's share of virtual nodes is within 5% of its weight share, and
// the share of keys it receives within 10% of it (relative, so the lightest
// Endpoints, at a 5% share, are held to ±0.5 percentage points).
func TestRingWeightShare(t *testing.T) {
	list := eps(8, func(i int) uint32 { return u32(1 + i%4) })
	p := BuildRing(list, 1024)
	nodes := make([]int, len(list))
	for _, n := range p.ring {
		nodes[n.idx]++
	}
	ks := keys(200000)
	got := make([]int, len(list))
	for _, k := range ks {
		c, _ := p.Pick(k, nil, nil)
		got[c.Index]++
	}
	for i := range list {
		share := float64(p.w[i]) / float64(p.total)
		if vn := float64(nodes[i]) / float64(len(p.ring)); math.Abs(vn-share) > 0.05*share {
			t.Errorf("Endpoint %d: virtual node share %.4f, weight share %.4f", i, vn, share)
		}
		if ks := float64(got[i]) / float64(len(ks)); math.Abs(ks-share) > 0.1*share {
			t.Errorf("Endpoint %d: key share %.4f, weight share %.4f", i, ks, share)
		}
	}
}
