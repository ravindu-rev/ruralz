// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"math/rand/v2"
	"testing"
)

// fullWalk is the reference answer of a round-robin or ring-hash pick: the
// first slot of the lowest grade along one pass from start.
func fullWalk(p *Picker, start int, grades []Grade) (int, Grade) {
	order := make([]int, 0, p.Slots())
	for _, i := range p.sched {
		order = append(order, int(i))
	}
	for _, n := range p.ring {
		order = append(order, int(n.idx))
	}
	best, bestG := -1, gone
	for _, i := range append(order[start:], order[:start]...) {
		if grades[i] < bestG {
			best, bestG = i, grades[i]
		}
	}
	return best, bestG
}

// gradesOf returns n grades given by f.
func gradesOf(n int, f func(i int) Grade) []Grade {
	out := make([]Grade, n)
	for i := range out {
		out[i] = f(i)
	}
	return out
}

// TestDegradedWalk is the degraded-pick gate of 05 reqs 11, 12 and 14 (PBB
// Upstream-layer budget): with nothing Eligible in the probe, a round-robin
// or ring-hash pick grades O(E) Endpoints, at most probeGrades + 2 × E
// instead of every slot (38,400 at 600 Endpoints and v = 64), and still
// returns the first Endpoint of the lowest grade along its pass, the answer
// of a full walk. Over a stale structure, Endpoints that left the set are
// skipped by the probe and the linear pass alike (05 req 16).
func TestDegradedWalk(t *testing.T) {
	const n, light = 600, 377
	pick := func(i int, a, b Grade) Grade { return []Grade{a, b}[b2i(i == light)] }
	for _, alg := range []Algorithm{RoundRobin, RingHash} {
		uniform := build(alg, eps(n, ones), MinVirtualNodes, 0)
		// Endpoint 377 weighs 1 against 1,000: it holds one slot.
		skewed := build(alg, eps(n, func(i int) uint32 { return []uint32{1000, 1}[b2i(i == light)] }), MinVirtualNodes, 0)
		tracked := build(alg, eps(300, ones), MinVirtualNodes, 0)
		cases := []struct {
			name   string
			p      *Picker
			grades []Grade
		}{
			{"all down", uniform, gradesOf(n, func(int) Grade { return Down })},
			{"all down, tracked size", tracked, gradesOf(300, func(int) Grade { return Down })},
			{"one eligible", uniform, gradesOf(n, func(i int) Grade { return pick(i, Down, Eligible) })},
			{"one light eligible", skewed, gradesOf(n, func(i int) Grade { return pick(i, Down, Eligible) })},
			{"one light down among down-avoided", skewed, gradesOf(n, func(i int) Grade { return pick(i, DownAvoided, Down) })},
			{"few down among down-avoided", uniform, gradesOf(n, func(i int) Grade { return []Grade{DownAvoided, Down}[b2i(i%97 == 5)] })},
			{"avoided in panic", uniform, gradesOf(n, func(i int) Grade { return []Grade{Down, Avoided}[b2i(i%250 == 3)] })},
		}
		for _, tc := range cases {
			t.Run(alg.String()+"/"+tc.name, func(t *testing.T) {
				p, size := tc.p, len(tc.grades)
				for k, key := range keys(16) {
					v := &view{grades: tc.grades}
					start := p.ringStart(key)
					if alg == RoundRobin {
						start = int(p.cursor.Load() % uint64(len(p.sched))) //nolint:gosec // G115: the remainder is below the slot count.
					}
					c, ok := p.Pick(key, nil, v)
					wantI, wantG := fullWalk(p, start, tc.grades)
					if !ok || c.Index != wantI || c.Grade != wantG {
						t.Fatalf("pick %d = %+v, %v; full walk %d grade %d", k, c, ok, wantI, wantG)
					}
					if calls := v.calls.Load(); calls > int64(probeGrades+2*size) {
						t.Fatalf("pick %d graded %d times for %d Endpoints", k, calls, size)
					}
				}
			})
		}
		t.Run(alg.String()+"/stale", func(t *testing.T) {
			remap := make([]int32, n)
			for i := range remap {
				remap[i] = -1
			}
			g := grader{remap: remap}
			if i, gr, ok := uniform.pick(HashKey("k"), pcg(1), &g); ok || i != -1 || gr != gone {
				t.Fatalf("pick over a structure with nothing left = %d, %d, %v", i, gr, ok)
			}
			// Only Endpoint 421 remains, Down.
			remap[421] = 0
			g = grader{remap: remap, view: &view{grades: []Grade{Down}}}
			if i, gr, ok := uniform.pick(HashKey("k"), pcg(1), &g); !ok || i != 421 || gr != Down {
				t.Fatalf("pick = %d, %d, %v; want 421 Down", i, gr, ok)
			}
		})
	}
}

// TestDegradedWalkRandom is the randomized differential check of the
// degraded pick against fullWalk (05 reqs 11, 12, 14 and 16): structures of
// more than probeGrades Endpoints, alternately of a size the probe tracks
// (65 to seenCap) and of one it does not (seenCap + 1 to 1,500), with
// random weights and mostly Down or DownAvoided grades, so the probe
// usually ends without an Eligible Endpoint and the walk resumes. Every
// third case is stale: about a quarter of the structure's Endpoints left
// the set, and the view grades the remaining ones under their current
// indices. The pick returns the full walk's answer and grades at most
// probeGrades + 2 × E times.
func TestDegradedWalkRandom(t *testing.T) {
	r := rand.New(pcg(42)) //nolint:gosec // G404: deterministic test cases, not secrets.
	for _, alg := range []Algorithm{RoundRobin, RingHash} {
		for c := range 40 {
			n := probeGrades + 1 + r.IntN(seenCap-probeGrades) // tracked
			if c%2 == 1 {
				n = seenCap + 1 + r.IntN(1500-seenCap) // untracked
			}
			p := build(alg, eps(n, func(int) uint32 { return u32(1 + r.IntN(1000)) }), MinVirtualNodes, r.Uint64())
			// Mostly Down or DownAvoided, rarely Avoided or Eligible.
			structGrades := gradesOf(n, func(int) Grade {
				switch x := r.IntN(4 * n); {
				case x == 0:
					return Eligible
				case x < 3:
					return Avoided
				case x < 2*n:
					return Down
				default:
					return DownAvoided
				}
			})
			stale := c%3 == 2
			g := grader{}
			cur := structGrades
			if stale {
				g.remap = make([]int32, n)
				cur = nil
				var next int32
				for i := range g.remap {
					if r.IntN(4) == 0 {
						g.remap[i], structGrades[i] = -1, gone
						continue
					}
					g.remap[i] = next
					next++
					cur = append(cur, structGrades[i])
				}
			}
			v := &view{grades: cur}
			g.view = v
			key := r.Uint64()
			start := p.ringStart(key)
			if alg == RoundRobin {
				start = int(p.cursor.Load() % uint64(len(p.sched))) //nolint:gosec // G115: the remainder is below the slot count.
			}
			i, gr, ok := p.pick(key, nil, &g)
			wantI, wantG := fullWalk(p, start, structGrades)
			if ok != (wantI >= 0) || i != wantI || gr != wantG {
				t.Fatalf("%s case %d (%d Endpoints, stale %v): pick %d grade %d ok %v; full walk %d grade %d",
					alg, c, n, stale, i, gr, ok, wantI, wantG)
			}
			if calls := v.calls.Load(); calls > int64(probeGrades+2*n) {
				t.Fatalf("%s case %d: graded %d times for %d Endpoints", alg, c, calls, n)
			}
		}
	}
}

// b2i converts a bool to an index.
func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
