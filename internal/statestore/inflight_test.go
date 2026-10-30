// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestore

import (
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestInFlightCeiling checks the Node-wide in-flight ceiling: TryAcquire
// admits up to the limit, refuses the next call, and admits again once a
// slot is released.
func TestInFlightCeiling(t *testing.T) {
	g := NewInFlight(3, nil)
	for i := range 3 {
		if !g.TryAcquire() {
			t.Fatalf("TryAcquire %d refused below the limit", i+1)
		}
	}
	if g.TryAcquire() {
		t.Fatal("TryAcquire admitted a call beyond the limit")
	}
	if g.Len() != 3 {
		t.Fatalf("Len = %d, want 3", g.Len())
	}
	g.Release()
	if !g.TryAcquire() {
		t.Fatal("TryAcquire refused after a Release")
	}
}

// TestInFlightDefaultLimit checks that a zero limit takes the 8,192 target.
func TestInFlightDefaultLimit(t *testing.T) {
	g := NewInFlight(0, nil)
	for i := range DefaultLimits().InFlight {
		if !g.TryAcquire() {
			t.Fatalf("TryAcquire %d refused below the default limit", i+1)
		}
	}
	if g.TryAcquire() {
		t.Fatalf("TryAcquire admitted call %d", DefaultLimits().InFlight+1)
	}
}

// TestInFlightConcurrent checks that concurrent callers never hold more
// slots than the limit.
func TestInFlightConcurrent(t *testing.T) {
	const limit = 16
	g := NewInFlight(limit, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	held, peak := 0, 0
	for range 64 {
		wg.Go(func() {
			for range 200 {
				if !g.TryAcquire() {
					continue
				}
				mu.Lock()
				held++
				peak = max(peak, held)
				mu.Unlock()
				mu.Lock()
				held--
				mu.Unlock()
				g.Release()
			}
		})
	}
	wg.Wait()
	if peak > limit {
		t.Fatalf("peak %d slots held, limit %d", peak, limit)
	}
	if g.Len() != 0 {
		t.Fatalf("Len = %d after every release, want 0", g.Len())
	}
}

// countingGauge is an emit.Gauge that keeps its value, its peak and its
// Add calls.
type countingGauge struct {
	mu   sync.Mutex
	v    int64
	peak int64
	adds int
}

func (g *countingGauge) Add(_ emit.Stripe, d int64) {
	g.mu.Lock()
	g.v += d
	g.peak = max(g.peak, g.v)
	g.adds++
	g.mu.Unlock()
}

func (g *countingGauge) Set(v int64) {
	g.mu.Lock()
	g.v = v
	g.mu.Unlock()
}

func (g *countingGauge) value() (v, peak int64, adds int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.v, g.peak, g.adds
}

// TestInFlightGauge checks ruralz_state_inflight_calls: each slot taken
// adds one to the gauge and each release takes one away, while a refused
// call leaves it alone, so the gauge reads the slots in use and plateaus
// at the ceiling (O2, docs/architecture/12-performance-budgets-and-benchmarking.md
// "Scenarios").
func TestInFlightGauge(t *testing.T) {
	gauge := &countingGauge{}
	g := NewInFlight(2, &emit.StateMetrics{InFlight: gauge})
	g.TryAcquire()
	g.TryAcquire()
	if g.TryAcquire() {
		t.Fatal("TryAcquire admitted a call beyond the limit")
	}
	if v, peak, adds := gauge.value(); v != 2 || peak != 2 || adds != 2 {
		t.Fatalf("gauge after two slots and a refusal = %d (peak %d, %d adds), want 2 (peak 2, 2 adds)", v, peak, adds)
	}
	g.Release()
	if v, _, _ := gauge.value(); v != int64(g.Len()) || v != 1 {
		t.Fatalf("gauge after a release = %d, Len %d, want 1", v, g.Len())
	}
	g.Release()
	if v, _, _ := gauge.value(); v != 0 {
		t.Fatalf("gauge after every release = %d, want 0", v)
	}
}

// TestInFlightGaugeConcurrent checks that concurrent callers keep the
// gauge within the slots in use: never above the limit, and zero once
// every slot is released. A release that freed its slot before lowering
// the gauge let another call take the slot while the gauge still counted
// it, reading above the limit, which O2's plateau at 8,192 judges a miss.
func TestInFlightGaugeConcurrent(t *testing.T) {
	const limit = 8
	gauge := &countingGauge{}
	g := NewInFlight(limit, &emit.StateMetrics{InFlight: gauge})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 500 {
				if g.TryAcquire() {
					g.Release()
				}
			}
		})
	}
	wg.Wait()
	if v, peak, _ := gauge.value(); v != 0 || peak > limit {
		t.Fatalf("gauge = %d (peak %d) after every release, want 0 (peak at most %d)", v, peak, limit)
	}
}

// TestInFlightWithoutGauge checks that no metrics, and metrics without
// the gauge, leave the ceiling working.
func TestInFlightWithoutGauge(t *testing.T) {
	for _, m := range []*emit.StateMetrics{nil, {}} {
		g := NewInFlight(1, m)
		if !g.TryAcquire() || g.TryAcquire() {
			t.Fatalf("NewInFlight(1, %v) does not admit exactly one call", m)
		}
		g.Release()
		if g.Len() != 0 {
			t.Fatalf("Len = %d after the release, want 0", g.Len())
		}
	}
}
