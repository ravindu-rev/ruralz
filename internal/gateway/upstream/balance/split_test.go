// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"math"
	"testing"
)

// TestSplitDistribution covers 05 req 10 and test plan item 12: weighted
// splits within tolerance, weight 0 never picked.
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
			var total float64
			live := 0
			for _, w := range tt.weights {
				total += float64(w)
				if w > 0 {
					live++
				}
			}
			if s.Entries() != live {
				t.Fatalf("Entries = %d, want %d", s.Entries(), live)
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
				share := float64(got[i]) / picks
				if w == 0 && got[i] != 0 {
					t.Fatalf("weight-0 entry %d picked %d times", i, got[i])
				}
				if want := float64(w) / total; math.Abs(share-want) > 0.01 {
					t.Fatalf("entry %d share %.4f, want %.4f", i, share, want)
				}
			}
		})
	}
}

// TestSplitEdges covers every weight 0 (503 RZ-UP-008, 05 req 10) and the
// single-entry pick that needs no randomness.
func TestSplitEdges(t *testing.T) {
	for _, w := range [][]uint32{nil, {0}, {0, 0, 0}} {
		if i, ok := NewSplit(w).Pick(nil); ok || i != -1 {
			t.Fatalf("NewSplit(%v).Pick = %d, %v", w, i, ok)
		}
	}
	s := NewSplit([]uint32{0, 0, 3})
	src := &seqSource{vals: []uint64{1}}
	if i, ok := s.Pick(src); !ok || i != 2 || src.i != 0 {
		t.Fatalf("single entry = %d, %v after %d draws", i, ok, src.i)
	}
	if i, ok := NewSplit([]uint32{1, 1}).Pick(nil); !ok || i < 0 || i > 1 {
		t.Fatalf("runtime source pick = %d, %v", i, ok)
	}
}
