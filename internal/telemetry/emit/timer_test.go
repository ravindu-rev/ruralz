// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package emit

import (
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"
)

// epoch is the fixed start of every timer test.
func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// testRand returns a deterministic generator for property tests.
func testRand(a, b uint64) *rand.Rand { return rand.New(rand.NewPCG(a, b)) } //nolint:gosec // G404: reproducible test data, not security.

// timerEvent is one Enter (in) or Leave at an offset from the start.
type timerEvent struct {
	at time.Duration
	in bool
}

// Spec 09 req 53 and 54, test 15: nested sections, overlapping parallel
// legs, a section spanning the request end, a leave at depth 0 and a
// clock that went backwards.
func TestGatewayTimer_Test15(t *testing.T) {
	ms := time.Millisecond
	tests := []struct {
		name   string
		events []timerEvent
		end    time.Duration
		want   time.Duration
		ok     bool
	}{
		{"no sections", nil, 10 * ms, 10 * ms, true},
		{"one section", []timerEvent{{2 * ms, true}, {5 * ms, false}}, 10 * ms, 7 * ms, true},
		{"nested", []timerEvent{{1 * ms, true}, {2 * ms, true}, {3 * ms, false}, {6 * ms, false}}, 10 * ms, 5 * ms, true},
		{"parallel overlapping legs", []timerEvent{{1 * ms, true}, {3 * ms, true}, {4 * ms, false}, {8 * ms, false}}, 10 * ms, 3 * ms, true},
		{"disjoint", []timerEvent{{1 * ms, true}, {2 * ms, false}, {4 * ms, true}, {7 * ms, false}}, 10 * ms, 6 * ms, true},
		{"spanning the end", []timerEvent{{6 * ms, true}}, 10 * ms, 6 * ms, true},
		{"everything excluded", []timerEvent{{0, true}, {10 * ms, false}}, 10 * ms, 0, true},
		{"leave at depth 0", []timerEvent{{1 * ms, false}}, 10 * ms, 0, false},
		{"enter before start", []timerEvent{{-1 * ms, true}, {2 * ms, false}}, 10 * ms, 0, false},
		{"leave before its run", []timerEvent{{5 * ms, true}, {3 * ms, false}}, 10 * ms, 0, false},
		{"end before start", nil, -1 * ms, 0, false},
		{"open run after the end", []timerEvent{{12 * ms, true}}, 10 * ms, 0, false},
		{"beyond 48 bits", []timerEvent{{1 << 49, true}}, 1 << 50, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var g GatewayTimer
			start := epoch()
			g.Reset(start)
			for _, e := range tt.events {
				if e.in {
					g.Enter(start.Add(e.at))
				} else {
					g.Leave(start.Add(e.at))
				}
			}
			got, ok := g.Result(start.Add(tt.end))
			if ok != tt.ok || (ok && got != tt.want) {
				t.Errorf("Result = %v, %v; want %v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
	var g GatewayTimer
	g.Reset(epoch())
	for range maxDepth + 1 {
		g.Enter(epoch())
	}
	if _, ok := g.Result(epoch().Add(time.Second)); ok {
		t.Error("depth overflow not an anomaly")
	}
	g.Reset(epoch())
	if d, ok := g.Result(epoch().Add(time.Second)); !ok || d != time.Second {
		t.Errorf("after Reset = %v, %v", d, ok)
	}
}

// recorder is a Histogram and Counter that keeps what it was given.
type recorder struct {
	values []uint64
	adds   uint64
}

func (r *recorder) Record(_ Stripe, v uint64)                     { r.values = append(r.values, v) }
func (r *recorder) RecordExemplar(_ Stripe, v uint64, _ Exemplar) { r.values = append(r.values, v) }
func (r *recorder) Add(_ Stripe, n uint64)                        { r.adds += n }

// Observe records the result in nanoseconds, or counts a clock anomaly in
// the skipped counter instead (spec 09 req 54).
func TestGatewayTimerObserve(t *testing.T) {
	var h, skipped recorder
	var g GatewayTimer
	g.Reset(epoch())
	g.Enter(epoch().Add(time.Millisecond))
	g.Leave(epoch().Add(3 * time.Millisecond))
	if d, ok := g.Observe(epoch().Add(3100*time.Microsecond), 1, &h, &skipped); !ok || d != 1100*time.Microsecond {
		t.Errorf("Observe = %v, %v; want 1.1ms, true", d, ok)
	}
	g.Reset(epoch())
	g.Leave(epoch())
	if d, ok := g.Observe(epoch().Add(time.Millisecond), 1, &h, &skipped); ok || d != 0 {
		t.Errorf("Observe after a leave at depth 0 = %v, %v; want 0, false", d, ok)
	}
	if len(h.values) != 1 || h.values[0] != uint64(1100*time.Microsecond) || skipped.adds != 1 {
		t.Errorf("recorded %v, skipped %d; want [1100000], 1", h.values, skipped.adds)
	}
}

// Spec 09 req 54 with parallel legs: a caller reads the clock before its
// compare and swap lands, so swaps can land in another order than the
// reads. Each case applies the events in swap order, with the offsets
// the callers read; the result is still wall-clock time minus the union.
func TestGatewayTimerSwapOrder_Req54(t *testing.T) {
	ms := time.Millisecond
	tests := []struct {
		name   string
		events []timerEvent
		end    time.Duration
		want   time.Duration
	}{
		// Leg A read 15 and leg B 20, B swapped first: the run ends at
		// 20, not at A's 15. Union [1, 20].
		{"leaves swap in reverse read order", []timerEvent{{1 * ms, true}, {2 * ms, true}, {20 * ms, false}, {15 * ms, false}}, 30 * ms, 11 * ms},
		// Leg A read 5 and leg B 8, B opened the run: A moves its start
		// back to 5. Union [5, 12].
		{"enters swap in reverse read order", []timerEvent{{8 * ms, true}, {5 * ms, true}, {10 * ms, false}, {12 * ms, false}}, 20 * ms, 13 * ms},
		// A section read before the last run closed starts at that
		// run's end, never inside it. Union [10, 25].
		{"late section overlapping a closed run", []timerEvent{{10 * ms, true}, {20 * ms, false}, {15 * ms, true}, {25 * ms, false}}, 30 * ms, 15 * ms},
		// Union [10, 20]: the late section adds nothing.
		{"late section inside a closed run", []timerEvent{{10 * ms, true}, {20 * ms, false}, {12 * ms, true}, {18 * ms, false}}, 30 * ms, 20 * ms},
		// The start moves back only to the end of the last closed run.
		// Union [1, 10].
		{"late enter clamped to the last run", []timerEvent{
			{1 * ms, true}, {4 * ms, false}, {8 * ms, true}, {3 * ms, true}, {10 * ms, false}, {9 * ms, false},
		}, 20 * ms, 11 * ms},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var g GatewayTimer
			start := epoch()
			g.Reset(start)
			for _, e := range tt.events {
				if e.in {
					g.Enter(start.Add(e.at))
				} else {
					g.Leave(start.Add(e.at))
				}
			}
			if got, ok := g.Result(start.Add(tt.end)); !ok || got != tt.want {
				t.Errorf("Result = %v, %v; want %v, true", got, ok, tt.want)
			}
		})
	}
}

// Spec 09 req 54 and test 15 (parallel aggregate legs): legs entering and
// leaving concurrently never corrupt the word, and whatever order their
// swaps land in, identical sections exclude exactly their length once.
// For distinct sections the excluded time lies between their common
// part and their hull: the run opened first spans the common part, and
// runs never overlap.
func TestGatewayTimerConcurrentLegs(t *testing.T) {
	ms := time.Millisecond
	start := epoch()
	for round := range 500 {
		var g GatewayTimer
		g.Reset(start)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				g.Enter(start.Add(ms))
				g.Leave(start.Add(9 * ms))
			})
		}
		wg.Wait()
		if w := g.word.Load(); w>>depthShift != 0 {
			t.Fatalf("round %d: word = %#x after every leg left", round, w)
		}
		if d, ok := g.Result(start.Add(10 * ms)); !ok || d != 2*ms {
			t.Fatalf("round %d: identical legs: Result = %v, %v; want 2ms, true", round, d, ok)
		}

		g.Reset(start)
		for i := range 8 {
			wg.Go(func() {
				g.Enter(start.Add(time.Duration(1+i) * ms))
				g.Leave(start.Add(time.Duration(20+i) * ms))
			})
		}
		wg.Wait()
		ex := time.Duration(g.excluded.Load())
		// Common part [8, 20], hull [1, 27].
		if g.word.Load()>>depthShift != 0 || g.anomaly.Load() || ex < 12*ms || ex > 26*ms {
			t.Fatalf("round %d: staggered legs excluded %v (anomaly %v), want within [12ms, 26ms]", round, ex, g.anomaly.Load())
		}
	}
}

// unionMeasure is the reference: the measure of the union of intervals,
// clipped to [0, end].
func unionMeasure(iv [][2]time.Duration, end time.Duration) time.Duration {
	iv = slices.Clone(iv)
	slices.SortFunc(iv, func(a, b [2]time.Duration) int { return int(a[0] - b[0]) })
	var total, curS, curE time.Duration
	open := false
	for _, x := range iv {
		s, e := x[0], min(x[1], end)
		if s >= end || e <= s {
			continue
		}
		if !open || s > curE {
			if open {
				total += curE - curS
			}
			curS, curE, open = s, e, true
			continue
		}
		curE = max(curE, e)
	}
	if open {
		total += curE - curS
	}
	return total
}

// runIntervals applies random intervals in time order and compares with
// the reference (spec 09 test 26).
func runIntervals(t *testing.T, iv [][2]time.Duration, end time.Duration) {
	t.Helper()
	var events []timerEvent
	for _, x := range iv {
		if x[0] > end {
			continue // starts after the request ended
		}
		events = append(events, timerEvent{x[0], true})
		if x[1] <= end {
			events = append(events, timerEvent{x[1], false})
		}
	}
	// Ties: enter before leave, so touching intervals merge.
	slices.SortStableFunc(events, func(a, b timerEvent) int {
		if a.at != b.at {
			return int(a.at - b.at)
		}
		if a.in == b.in {
			return 0
		}
		if a.in {
			return -1
		}
		return 1
	})
	var g GatewayTimer
	start := epoch()
	g.Reset(start)
	for _, e := range events {
		if e.in {
			g.Enter(start.Add(e.at))
		} else {
			g.Leave(start.Add(e.at))
		}
	}
	got, ok := g.Result(start.Add(end))
	want := end - unionMeasure(iv, end)
	if !ok || got != want {
		t.Fatalf("intervals %v end %v: Result = %v, %v; want %v", iv, end, got, ok, want)
	}
}

// Spec 09 test 26 (property): the result equals wall-clock time minus the
// measure of the union of random excluded intervals.
func TestGatewayTimerUnion_Test26(t *testing.T) {
	rng := testRand(26, 54)
	for range 2000 {
		n := rng.IntN(8)
		end := time.Duration(1 + rng.IntN(1000))
		var iv [][2]time.Duration
		for range n {
			s := time.Duration(rng.IntN(1100))
			iv = append(iv, [2]time.Duration{s, s + time.Duration(1+rng.IntN(300))})
		}
		runIntervals(t, iv, end)
	}
}

// FuzzGatewayTimer is spec 09 test 31 (26).
func FuzzGatewayTimer(f *testing.F) {
	f.Add([]byte{1, 5, 3, 9, 2, 4}, uint16(10))
	f.Add([]byte{0, 0, 0, 255}, uint16(1))
	f.Add([]byte{}, uint16(100))
	f.Add([]byte{10, 20, 15, 25, 30, 1}, uint16(22))
	f.Fuzz(func(t *testing.T, data []byte, end uint16) {
		if end == 0 {
			end = 1
		}
		var iv [][2]time.Duration
		for i := 0; i+1 < len(data) && len(iv) < 64; i += 2 {
			s := time.Duration(data[i]) * 4
			iv = append(iv, [2]time.Duration{s, s + time.Duration(data[i+1]) + 1})
		}
		runIntervals(t, iv, time.Duration(end%1100))
	})
}

// BenchmarkGatewayTimer measures one request's excluded sections.
func BenchmarkGatewayTimer(b *testing.B) {
	var g GatewayTimer
	start := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		g.Reset(start)
		g.Enter(start.Add(time.Microsecond))
		g.Leave(start.Add(2 * time.Microsecond))
		g.Enter(start.Add(3 * time.Microsecond))
		g.Leave(start.Add(4 * time.Microsecond))
		g.Result(start.Add(5 * time.Microsecond))
	}
}
