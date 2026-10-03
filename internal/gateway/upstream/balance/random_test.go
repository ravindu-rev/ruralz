// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"testing"
)

// TestRandomChiSquare covers 05 req 15 and test plan item 11: weighted
// random passes a chi-square test at p > 0.001 over 10^6 picks.
func TestRandomChiSquare(t *testing.T) {
	list := eps(5, func(i int) uint32 { return u32(i + 1) }) // weights 1..5
	p := BuildRandom(list)
	if p.Bytes() != 8*5 || p.Algorithm() != Random {
		t.Fatalf("table bytes %d", p.Bytes())
	}
	const picks = 1000000
	src := pcg(42)
	got := counts(5, picks, func() int { c, _ := p.Pick(0, src, nil); return c.Index })
	var chi2 float64
	for i, n := range got {
		expected := float64(picks) * float64(i+1) / 15
		d := float64(n) - expected
		chi2 += d * d / expected
	}
	// Critical value of chi-square with 4 degrees of freedom at p = 0.001.
	const critical = 18.467
	if chi2 > critical {
		t.Fatalf("chi-square %.3f > %.3f for counts %v", chi2, critical, got)
	}
}

// TestRandomExclusions covers 05 req 15: exclusions by rejection for up to
// 8 draws, then a weighted linear scan over the lowest grade present.
func TestRandomExclusions(t *testing.T) {
	list := eps(6, func(i int) uint32 { return u32(i + 1) })
	tests := []struct {
		name   string
		grades []Grade
		want   map[int]bool
		grade  Grade
	}{
		{"all eligible", nil, map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true}, Eligible},
		{"light one left", []Grade{Eligible, Down, Down, Down, Down, Avoided}, map[int]bool{0: true}, Eligible},
		{"avoided only", []Grade{Down, Avoided, Down, Avoided, Down, DownAvoided}, map[int]bool{1: true, 3: true}, Avoided},
		{"panic", []Grade{Down, DownAvoided, Down, DownAvoided, DownAvoided, DownAvoided}, map[int]bool{0: true, 2: true}, Down},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := BuildRandom(list)
			v := &view{grades: tt.grades}
			src := pcg(1)
			seen := map[int]bool{}
			for range 2000 {
				c, ok := p.Pick(0, src, v)
				if !ok || !tt.want[c.Index] || c.Grade != tt.grade {
					t.Fatalf("pick %+v, want one of %v with grade %d", c, tt.want, tt.grade)
				}
				seen[c.Index] = true
			}
			if len(seen) != len(tt.want) {
				t.Fatalf("picked %v, want every one of %v", seen, tt.want)
			}
		})
	}
	// The scan is weighted: Endpoints 1 and 3 (weights 2 and 4) split 1:2.
	p := BuildRandom(list)
	v := &view{grades: []Grade{Down, Eligible, Down, Eligible, Down, Down}}
	src := pcg(9)
	got := counts(6, 60000, func() int { c, _ := p.Pick(0, src, v); return c.Index })
	if ratio := float64(got[3]) / float64(got[1]); ratio < 1.9 || ratio > 2.1 {
		t.Fatalf("scan ratio %.3f, want about 2 (counts %v)", ratio, got)
	}
}

// TestRandomZeroWeight covers the weight rule: weight 0 is never drawn,
// not even in panic mode, while another Endpoint weighs more.
func TestRandomZeroWeight(t *testing.T) {
	list := eps(4, func(i int) uint32 { return []uint32{0, 3, 0, 1}[i] })
	p := BuildRandom(list)
	src := pcg(5)
	for _, grades := range [][]Grade{nil, {Eligible, Down, Eligible, Down}} {
		v := &view{grades: grades}
		for range 5000 {
			c, _ := p.Pick(0, src, v)
			if c.Index == 0 || c.Index == 2 {
				t.Fatalf("weight-0 Endpoint %d picked with grades %v", c.Index, grades)
			}
		}
	}
	if c, ok := BuildRandom(nil).Pick(0, src, nil); ok || c.Index != -1 {
		t.Fatalf("empty table picked %+v", c)
	}
}

// changingView alters grades between the scan's two passes.
type changingView struct{ n int }

func (v *changingView) Grade(int) Grade {
	v.n++
	if v.n <= 4 {
		return Eligible
	}
	return Down
}

func (v *changingView) Load(int) int64 { return 0 }

// TestRandomScanUnderConcurrentChange covers the scan's defense against
// grades changing between its passes: it still returns an Endpoint.
func TestRandomScanUnderConcurrentChange(t *testing.T) {
	p := BuildRandom(eps(4, ones))
	g := grader{view: &changingView{}}
	i, gr, ok := p.scan(pcg(1), &g, -1, gone)
	if !ok || i < 0 || i > 3 || gr != Eligible {
		t.Fatalf("scan = %d, %d, %v", i, gr, ok)
	}
}
