// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"math"
	"testing"
)

// TestLeastRequestPrefersLowerScore covers 05 req 13 and test plan item 11
// ("P2C prefers the lower score"): of two distinct weighted random
// candidates the lower (in-flight + recent failures) ÷ weight wins.
func TestLeastRequestPrefersLowerScore(t *testing.T) {
	tests := []struct {
		name    string
		weights []uint32
		loads   []int64
		want    []float64 // expected pick shares
	}{
		// Two Endpoints are always both candidates: the lower score always wins.
		{"two", []uint32{1, 1}, []int64{10, 0}, []float64{0, 1}},
		// Score per weight: 4/1 = 4 against 10/4 = 2.5.
		{"weighted score", []uint32{1, 4}, []int64{4, 10}, []float64{0, 1}},
		// Three equal weights, loads 0 < 5 < 10: the busiest never wins, the
		// idlest wins whenever drawn (2 of 3 pairs).
		{"three", []uint32{1, 1, 1}, []int64{0, 5, 10}, []float64{2.0 / 3, 1.0 / 3, 0}},
		// Negative loads count as 0; equal scores split evenly.
		{"tie", []uint32{1, 1}, []int64{-3, 0}, []float64{0.5, 0.5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := BuildLeastRequest(eps(len(tt.weights), func(i int) uint32 { return tt.weights[i] }))
			v := &view{loads: tt.loads}
			src := pcg(77)
			const picks = 30000
			got := counts(len(tt.weights), picks, func() int { c, _ := p.Pick(0, src, v); return c.Index })
			for i, share := range tt.want {
				if s := float64(got[i]) / picks; math.Abs(s-share) > 0.02 {
					t.Fatalf("Endpoint %d share %.3f, want %.3f (counts %v)", i, s, share, got)
				}
			}
		})
	}
}

// TestLeastRequestExclusions covers 05 req 11 with least-request: both
// candidates come from the lowest grade present.
func TestLeastRequestExclusions(t *testing.T) {
	list := eps(5, ones)
	tests := []struct {
		name   string
		grades []Grade
		loads  []int64
		want   int
		grade  Grade
	}{
		// Endpoint 4 is idle but Down; of the healthy ones 2 is least loaded.
		{"down skipped", []Grade{Eligible, Eligible, Eligible, Eligible, Down}, []int64{5, 5, 1, 5, 0}, -1, Eligible},
		{"single eligible", []Grade{Down, Avoided, Eligible, Down, Down}, []int64{0, 0, 9, 0, 0}, 2, Eligible},
		{"relaxed pair", []Grade{Down, Avoided, Down, Avoided, Down}, []int64{0, 3, 0, 1, 0}, 3, Avoided},
		{"panic", []Grade{Down, Down, DownAvoided, DownAvoided, DownAvoided}, []int64{2, 1, 0, 0, 0}, 1, Down},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := BuildLeastRequest(list)
			v := &view{grades: tt.grades, loads: tt.loads}
			src := pcg(3)
			for range 3000 {
				c, ok := p.Pick(0, src, v)
				if !ok || c.Grade != tt.grade || (tt.want >= 0 && c.Index != tt.want) || (tt.want < 0 && c.Index == 4) {
					t.Fatalf("pick %+v, want %d with grade %d", c, tt.want, tt.grade)
				}
			}
		})
	}
	// A single weighted Endpoint needs no second candidate.
	p := BuildLeastRequest(eps(3, func(i int) uint32 { return []uint32{0, 7, 0}[i] }))
	if c, ok := p.Pick(0, pcg(1), nil); !ok || c.Index != 1 {
		t.Fatalf("single weighted pick %+v", c)
	}
	if _, ok := BuildLeastRequest(nil).Pick(0, pcg(1), nil); ok {
		t.Fatal("empty least-request table picked")
	}
}

// TestCompareScores covers the overflow-free score comparison.
func TestCompareScores(t *testing.T) {
	tests := []struct {
		la     uint64
		wa     uint32
		lb     uint64
		wb     uint32
		wantCm int
	}{
		{1, 1, 2, 1, -1},
		{4, 1, 10, 4, 1},
		{3, 3, 1, 1, 0},
		{math.MaxUint64, math.MaxUint32, math.MaxUint64, math.MaxUint32 - 1, -1},
	}
	for _, tt := range tests {
		if got := compareScores(tt.la, tt.wa, tt.lb, tt.wb); got != tt.wantCm {
			t.Errorf("compareScores(%d/%d, %d/%d) = %d, want %d", tt.la, tt.wa, tt.lb, tt.wb, got, tt.wantCm)
		}
	}
}
