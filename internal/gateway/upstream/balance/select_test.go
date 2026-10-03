// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"errors"
	"testing"
	"time"
)

// TestGradeOf covers the combination of 05 req 11 steps (b) and (c).
func TestGradeOf(t *testing.T) {
	tests := []struct {
		down, avoid bool
		want        Grade
		panic       bool
		relaxed     bool
	}{
		{false, false, Eligible, false, false},
		{false, true, Avoided, false, true},
		{true, false, Down, true, false},
		{true, true, DownAvoided, true, true},
	}
	for _, tt := range tests {
		g := GradeOf(tt.down, tt.avoid)
		c := Choice{Grade: g}
		if g != tt.want || c.Panic() != tt.panic || c.Relaxed() != tt.relaxed {
			t.Errorf("GradeOf(%v, %v) = %d (panic %v, relaxed %v)", tt.down, tt.avoid, g, c.Panic(), c.Relaxed())
		}
	}
	// Out-of-range grades from a View count as DownAvoided.
	g := grader{view: &view{grades: []Grade{9}}}
	if got := g.grade(0); got != DownAvoided {
		t.Errorf("grade 9 read as %d", got)
	}
}

// TestSelectOrder covers 05 req 11 steps (a) to (d) for every algorithm:
// (a) an empty set is ErrNoEndpoints (503 RZ-UP-008); (b) Down Endpoints are
// dropped unless none remains (panic mode); (c) Avoided Endpoints are
// excluded while another remains; (d) the algorithm picks among the rest.
func TestSelectOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	tests := []struct {
		name   string
		grades []Grade
		allow  map[int]bool
		grade  Grade
	}{
		{"all eligible", nil, map[int]bool{0: true, 1: true, 2: true, 3: true}, Eligible},
		{"step b drops down", []Grade{Down, Eligible, Down, Eligible}, map[int]bool{1: true, 3: true}, Eligible},
		{"step c drops tried", []Grade{Avoided, Eligible, Avoided, Down}, map[int]bool{1: true}, Eligible},
		{"step c while another remains", []Grade{Avoided, Down, Avoided, Down}, map[int]bool{0: true, 2: true}, Avoided},
		{"panic mode", []Grade{Down, Down, Down, Down}, map[int]bool{0: true, 1: true, 2: true, 3: true}, Down},
		{"panic then step c", []Grade{DownAvoided, Down, DownAvoided, DownAvoided}, map[int]bool{1: true}, Down},
		{"everything avoided", []Grade{DownAvoided, DownAvoided, DownAvoided, DownAvoided}, map[int]bool{0: true, 1: true, 2: true, 3: true}, DownAvoided},
	}
	for _, alg := range allAlgorithms {
		for _, tt := range tests {
			t.Run(alg.String()+"/"+tt.name, func(t *testing.T) {
				b, err := New(Config{Algorithm: alg, VirtualNodes: 64, Endpoints: eps(4, ones), Rand: pcg(1), Now: now})
				if err != nil {
					t.Fatal(err)
				}
				v := &view{grades: tt.grades}
				src := pcg(2)
				for k := range 500 {
					c, err := b.Load().Select(HashKey(string(rune('a'+k%26))), src, v)
					if err != nil || !tt.allow[c.Index] || c.Grade != tt.grade {
						t.Fatalf("Select = %+v, %v; want one of %v with grade %d", c, err, tt.allow, tt.grade)
					}
				}
			})
		}
		t.Run(alg.String()+"/empty", func(t *testing.T) {
			b, err := New(Config{Algorithm: alg, Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if c, err := b.Load().Select(1, nil, nil); !errors.Is(err, ErrNoEndpoints) || c.Index != -1 {
				t.Fatalf("Select on an empty set = %+v, %v", c, err)
			}
			if _, err := b.Load().SelectRandom(nil, nil); !errors.Is(err, ErrNoEndpoints) {
				t.Fatalf("SelectRandom on an empty set = %v", err)
			}
		})
	}
}
