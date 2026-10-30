// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"math"
	"slices"
	"testing"
)

// shareWithin fails when entry i's share of picks is off its weight's
// share of total by more than 0.01.
func shareWithin(t *testing.T, got []int, picks int, weights []uint32) {
	t.Helper()
	var total float64
	for _, w := range weights {
		total += float64(w)
	}
	for i, w := range weights {
		share := float64(got[i]) / float64(picks)
		if want := float64(w) / total; math.Abs(share-want) > 0.01 {
			t.Fatalf("entry %d share %.4f, want %.4f", i, share, want)
		}
	}
}

// TestSplitDistribution covers the weighted split of upstreams entries:
// shares within tolerance, weight 0 never picked while another entry
// weighs more.
func TestSplitDistribution(t *testing.T) {
	tests := []struct {
		name    string
		weights []uint32
	}{
		{"95/5", []uint32{95, 5}},
		{"blue-green", []uint32{0, 1}},
		{"canary three", []uint32{80, 0, 15, 5}},
		{"many (binary search)", []uint32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 0, 12}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSplit(tt.weights)
			live := 0
			for _, w := range tt.weights {
				if w > 0 {
					live++
				}
			}
			if s.Entries() != live || s.Len() != len(tt.weights) {
				t.Fatalf("Entries, Len = %d, %d; want %d, %d", s.Entries(), s.Len(), live, len(tt.weights))
			}
			const picks = 200000
			src := pcg(21)
			got := counts(len(tt.weights), picks, func() int {
				i, ok := s.Pick(src)
				if !ok {
					t.Fatal("Pick failed")
				}
				return i
			})
			for i, w := range tt.weights {
				if w == 0 && got[i] != 0 {
					t.Fatalf("weight-0 entry %d picked %d times", i, got[i])
				}
			}
			shareWithin(t, got, picks, tt.weights)
		})
	}
}

// TestSplitAllZeroWeighOne covers the weight rule's second half: when
// every entry weighs 0 they all weigh 1, so each is picked with equal
// chance and the pick never fails.
func TestSplitAllZeroWeighOne(t *testing.T) {
	for _, weights := range [][]uint32{{0, 0}, {0, 0, 0}, {0, 0, 0, 0, 0, 0, 0, 0, 0, 0}} {
		s := NewSplit(weights)
		const picks = 120000
		src := pcg(7)
		got := counts(len(weights), picks, func() int {
			i, ok := s.Pick(src)
			if !ok {
				t.Fatalf("NewSplit(%v).Pick failed", weights)
			}
			return i
		})
		ones := make([]uint32, len(weights))
		for i := range ones {
			ones[i] = 1
		}
		shareWithin(t, got, picks, ones)
	}
}

// TestSplitEdges covers a Split without entries (no pick) and the single
// candidates that need no randomness: one weighted entry among weight-0
// ones, and one entry of weight 0.
func TestSplitEdges(t *testing.T) {
	for _, w := range [][]uint32{nil, {}} {
		if i, ok := NewSplit(w).Pick(nil); ok || i != -1 {
			t.Fatalf("NewSplit(%v).Pick = %d, %v; want -1, false", w, i, ok)
		}
	}
	for _, tc := range []struct {
		weights []uint32
		want    int
	}{
		{[]uint32{0, 0, 3}, 2},
		{[]uint32{0}, 0},
		{[]uint32{9}, 0},
	} {
		src := &seqSource{vals: []uint64{1}}
		if i, ok := NewSplit(tc.weights).Pick(src); !ok || i != tc.want || src.i != 0 {
			t.Fatalf("NewSplit(%v).Pick = %d, %v after %d draws; want %d without a draw", tc.weights, i, ok, src.i, tc.want)
		}
	}
	if i, ok := NewSplit([]uint32{1, 1}).Pick(nil); !ok || i < 0 || i > 1 {
		t.Fatalf("runtime source pick = %d, %v", i, ok)
	}
	if i, ok := NewSplit([]uint32{0, 0}).Pick(nil); !ok || i < 0 || i > 1 {
		t.Fatalf("runtime source all-zero pick = %d, %v", i, ok)
	}
}

// TestSplitKeepsWeights checks that the Split holds its own copy of the
// weights: changing the caller's slice afterwards changes no pick.
func TestSplitKeepsWeights(t *testing.T) {
	w := []uint32{0, 0, 1}
	s := NewSplit(w)
	w[0], w[2] = 5, 0
	var tried Tried
	tried.Add(2)
	if i, ok := s.PickExcept(&seqSource{vals: []uint64{0}}, &tried); !ok || i != 0 {
		t.Fatalf("PickExcept after the caller changed its weights = %d, %v; want 0, true", i, ok)
	}
}

// TestSplitPickExcept covers the pick again after a refused leg: only
// entries not tried, by weight among them; weight-0 entries only once
// every weighted entry was tried, then with equal chance; and no pick once
// every entry was tried.
func TestSplitPickExcept(t *testing.T) {
	weights := []uint32{1, 3, 0, 6, 0}
	s := NewSplit(weights)
	tests := []struct {
		name  string
		tried []int
		// want are the shares by entry; nil means no pick.
		want []uint32
	}{
		{"none tried is Pick", nil, []uint32{1, 3, 0, 6, 0}},
		{"heaviest tried", []int{3}, []uint32{1, 3, 0, 0, 0}},
		{"one weighted entry left", []int{0, 3}, []uint32{0, 1, 0, 0, 0}},
		{"weight-0 entries share equally", []int{0, 1, 3}, []uint32{0, 0, 1, 0, 1}},
		{"last weight-0 entry", []int{0, 1, 2, 3}, []uint32{0, 0, 0, 0, 1}},
		{"every entry tried", []int{0, 1, 2, 3, 4}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tried Tried
			for _, i := range tt.tried {
				tried.Add(i)
			}
			src := pcg(3)
			if tt.want == nil {
				if i, ok := s.PickExcept(src, &tried); ok || i != -1 {
					t.Fatalf("PickExcept = %d, %v; want -1, false", i, ok)
				}
				return
			}
			const picks = 100000
			got := counts(len(weights), picks, func() int {
				i, ok := s.PickExcept(src, &tried)
				if !ok {
					t.Fatal("PickExcept failed")
				}
				if slices.Contains(tt.tried, i) {
					t.Fatalf("PickExcept returned tried entry %d", i)
				}
				return i
			})
			shareWithin(t, got, picks, tt.want)
		})
	}
	// A single candidate takes no draw; a nil set holds none.
	var tried Tried
	tried.Add(1)
	tried.Add(3)
	src := &seqSource{vals: []uint64{1}}
	if i, ok := s.PickExcept(src, &tried); !ok || i != 0 || src.i != 0 {
		t.Fatalf("single weighted candidate = %d, %v after %d draws; want 0 without a draw", i, ok, src.i)
	}
	tried.Add(0)
	tried.Add(2)
	if i, ok := s.PickExcept(src, &tried); !ok || i != 4 || src.i != 0 {
		t.Fatalf("single weight-0 candidate = %d, %v after %d draws; want 4 without a draw", i, ok, src.i)
	}
	if i, ok := NewSplit([]uint32{0, 0, 7}).PickExcept(src, nil); !ok || i != 2 {
		t.Fatalf("PickExcept with a nil set = %d, %v; want 2, true", i, ok)
	}
	if i, ok := NewSplit(nil).PickExcept(src, nil); ok || i != -1 {
		t.Fatalf("PickExcept without entries = %d, %v; want -1, false", i, ok)
	}
}

// TestTried covers the tried-entry set: the zero value and a nil set are
// empty, entries below and from 64 are kept apart, and a negative entry
// is never held.
func TestTried(t *testing.T) {
	var nilSet *Tried
	if nilSet.Has(0) {
		t.Fatal("nil Tried holds entry 0")
	}
	var tr Tried
	in := []int{0, 5, 63, 64, 65, 127, 128, 300}
	for _, i := range in {
		if tr.Has(i) {
			t.Fatalf("empty Tried holds %d", i)
		}
		tr.Add(i)
	}
	tr.Add(-1)
	for i := -2; i <= 320; i++ {
		if got, want := tr.Has(i), slices.Contains(in, i); got != want {
			t.Fatalf("Has(%d) = %v, want %v", i, got, want)
		}
	}
}
