// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"errors"
	"math"
	"slices"
	"testing"
)

// TestNormalize covers 05 req 8: Endpoints sorted by identity before any
// build, with duplicates merged so every Node derives the same list.
func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   []Endpoint
		want []Endpoint
	}{
		{"empty", nil, []Endpoint{}},
		{"sorted", []Endpoint{{"a", 1}, {"b", 2}}, []Endpoint{{"a", 1}, {"b", 2}}},
		{"shuffled", []Endpoint{{"c", 3}, {"a", 1}, {"b", 2}}, []Endpoint{{"a", 1}, {"b", 2}, {"c", 3}}},
		{"duplicates sum", []Endpoint{{"b", 2}, {"a", 1}, {"b", 5}}, []Endpoint{{"a", 1}, {"b", 7}}},
		{"saturating", []Endpoint{{"a", math.MaxUint32}, {"a", 9}}, []Endpoint{{"a", math.MaxUint32}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := slices.Clone(tt.in)
			got := Normalize(tt.in)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("Normalize = %v, want %v", got, tt.want)
			}
			if !slices.Equal(in, tt.in) {
				t.Fatalf("Normalize changed its input: %v", tt.in)
			}
			if err := checkNormalized(got); err != nil {
				t.Fatalf("checkNormalized(Normalize) = %v", err)
			}
		})
	}
}

func TestCheckNormalized(t *testing.T) {
	for _, bad := range [][]Endpoint{
		{{"b", 1}, {"a", 1}},
		{{"a", 1}, {"a", 2}},
	} {
		if err := checkNormalized(bad); !errors.Is(err, ErrNotNormalized) {
			t.Errorf("checkNormalized(%v) = %v, want ErrNotNormalized", bad, err)
		}
	}
}

// TestEffectiveWeights covers the package weight rule (05 req 6's SRV rule
// applied to every weight): 0 is never picked unless every weight is 0.
func TestEffectiveWeights(t *testing.T) {
	tests := []struct {
		in       []uint32
		want     []uint32
		total    uint64
		wantLive int
	}{
		{in: nil, want: []uint32{}, total: 0, wantLive: 0},
		{in: []uint32{0, 0, 0}, want: []uint32{1, 1, 1}, total: 3, wantLive: 3},
		{in: []uint32{0, 2, 3}, want: []uint32{0, 2, 3}, total: 5, wantLive: 2},
		{in: []uint32{math.MaxUint32, math.MaxUint32}, want: []uint32{math.MaxUint32, math.MaxUint32}, total: 2 * math.MaxUint32, wantLive: 2},
	}
	for _, tt := range tests {
		list := make([]Endpoint, len(tt.in))
		for i, w := range tt.in {
			list[i] = Endpoint{Weight: w}
		}
		w, total, live := effectiveWeights(list)
		if !slices.Equal(w, tt.want) || total != tt.total || live != tt.wantLive {
			t.Errorf("effectiveWeights(%v) = %v, %d, %d; want %v, %d, %d", tt.in, w, total, live, tt.want, tt.total, tt.wantLive)
		}
	}
}

// TestQuotas covers n_i = max(1, round(L × w_i / Σw)) of 05 reqs 12 and
// 14, apportioned to exactly L slots (05 test plan item 11: "size
// min(65,536, 64E)", "within one slot of n_i").
func TestQuotas(t *testing.T) {
	tests := []struct {
		w      []uint32
		length int
		want   []uint32
	}{
		{[]uint32{1, 1, 1}, 192, []uint32{64, 64, 64}},
		{[]uint32{1, 2, 3}, 192, []uint32{32, 64, 96}},
		{[]uint32{1, 1000}, 128, []uint32{1, 127}},    // max(1, round(0.128)) = 1; the heavy one yields the extra slot
		{[]uint32{0, 1, 1}, 128, []uint32{0, 64, 64}}, // weight 0 gets no slot
		{[]uint32{1, 2}, 3, []uint32{1, 2}},
		{[]uint32{1, 1, 1}, 100, []uint32{34, 33, 33}}, // 99 rounded; the tie goes to the lowest index
		{[]uint32{2, 1, 1}, 6, []uint32{3, 1, 2}},      // 3 + 2 + 2 = 7 rounded; the tie yields at the lowest index
		{[]uint32{1, 1, 2}, 2, []uint32{1, 1, 1}},      // more Endpoints than slots: one each
		{[]uint32{math.MaxUint32, 1}, MaxRingNodes, []uint32{MaxRingNodes - 1, 1}},
		{[]uint32{5}, 0, []uint32{0}},
		{[]uint32{0, 0}, 10, []uint32{0, 0}},
	}
	for _, tt := range tests {
		var total uint64
		for _, x := range tt.w {
			total += uint64(x)
		}
		if got := quotas(tt.w, total, tt.length); !slices.Equal(got, tt.want) {
			t.Errorf("quotas(%v, %d) = %v, want %v", tt.w, tt.length, got, tt.want)
		}
	}
}

// TestQuotasProperty checks over random weights that quotas sum to L, keep
// every weighted Endpoint at one slot or more, and stay within one slot of
// max(1, round(share)) whenever no Endpoint's share is below one slot.
func TestQuotasProperty(t *testing.T) {
	src := pcg(11)
	for range 3000 {
		e := 1 + intn(src, 300)
		w := make([]uint32, e)
		var total uint64
		for i := range w {
			w[i] = u32(intn(src, 5000))
			total += uint64(w[i])
		}
		if total == 0 {
			continue
		}
		length := min(MaxScheduleSlots, SlotsPerEndpoint*e) >> intn(src, 4)
		n := quotas(w, total, length)
		var sum, live int
		floor := true
		for i, x := range w {
			if x == 0 {
				if n[i] != 0 {
					t.Fatalf("weight 0 got %d slots", n[i])
				}
				continue
			}
			live++
			sum += int(n[i])
			exact := float64(length) * float64(x) / float64(total)
			if exact < 1 {
				floor = false
			}
			if n[i] < 1 {
				t.Fatalf("weighted Endpoint got no slot")
			}
		}
		if want := max(length, live); sum != want {
			t.Fatalf("quotas sum to %d, want %d", sum, want)
		}
		if !floor || live >= length {
			continue
		}
		for i, x := range w {
			if x == 0 {
				continue
			}
			rounded := max(1, math.Round(float64(length)*float64(x)/float64(total)))
			if math.Abs(float64(n[i])-rounded) > 1 {
				t.Fatalf("quota %d strays from %v by more than one slot", n[i], rounded)
			}
		}
	}
}

// TestNominalSizes covers the S term and ring sizes of 05 reqs 12, 14 and
// 17.
func TestNominalSizes(t *testing.T) {
	sched := []struct {
		endpoints int
		want      int64
	}{
		{0, 0},
		{3, 768}, // 05 req 12: 3 Endpoints use 768 bytes
		{1024, 4 * 65536},
		{5000, 4 * 65536}, // at most 256 KiB
		{70000, 4 * 70000},
	}
	for _, tt := range sched {
		if got := ScheduleBytes(tt.endpoints); got != tt.want {
			t.Errorf("ScheduleBytes(%d) = %d, want %d", tt.endpoints, got, tt.want)
		}
	}
	ring := []struct {
		v, endpoints int
		want         int64
	}{
		{64, 0, 0},
		{0, 5, 0},
		{64, 3, 16 * 192},
		{1024, 64, 16 * 65536}, // 1 MiB at most
		{244, 64, 16 * 244 * 64},
		{64, 70000, 16 * 70000},
	}
	for _, tt := range ring {
		if got := RingBytes(tt.v, tt.endpoints); got != tt.want {
			t.Errorf("RingBytes(%d, %d) = %d, want %d", tt.v, tt.endpoints, got, tt.want)
		}
	}
}
