// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// uniformRings returns n rings of e Endpoints each.
func uniformRings(n, e int) map[string]int {
	rings := make(map[string]int, n)
	for i := range n {
		rings[fmt.Sprintf("up-%04d", i)] = e
	}
	return rings
}

// TestPlanBudgetFormula covers 05 req 17: v = clamp(floor((256 MiB − S −
// 16 MiB) ÷ (16 B × R)), 64, 1,024).
func TestPlanBudgetFormula(t *testing.T) {
	tests := []struct {
		name      string
		budget    int64
		schedules int64
		rings     map[string]int
		wantV     int
	}{
		{"no rings", 0, 0, nil, MaxVirtualNodes},
		{"empty ring", 0, 0, map[string]int{"a": 0}, MaxVirtualNodes},
		{"small ring clamps at 1,024", 0, 0, map[string]int{"a": 10}, MaxVirtualNodes},
		{"ten rings of 64 clamp at 1,024", 0, 0, uniformRings(10, 64), MaxVirtualNodes},
		// TMR "Load balancing" and 05 test plan item 11 quote v = 244 (about
		// 238 MiB) for 1,000 rings of 64 Endpoints; the normative formula
		// gives floor(240 MiB ÷ (16 B × 64,000)) = floor(245.76) = 245
		// (239.3 MiB), which this test pins.
		{"1,000 rings of 64", 0, 0, uniformRings(1000, 64), 245},
		{"schedules shrink v", DefaultBudget, 10 << 20, uniformRings(1000, 64), 235},
		{"explicit budget", 64 << 20, 0, map[string]int{"a": 3000, "b": 1000}, 786},
		// Caps: R × 64 × 16 B exceeds the room, but capped rings fit at 64.
		{"cap makes 64 fit", 16<<20 + 16*65536, 0, map[string]int{"huge": 5000}, MinVirtualNodes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := PlanBudget(tt.budget, tt.schedules, tt.rings)
			if p.V != tt.wantV || p.Degraded() || len(p.Warnings()) != 0 {
				t.Fatalf("PlanBudget: V %d (want %d), fallback %v, warnings %v", p.V, tt.wantV, p.Fallback, p.Warnings())
			}
			if !p.Fits() {
				t.Fatalf("plan does not fit: %+v", p)
			}
			var want int64
			for _, e := range tt.rings {
				want += RingBytes(p.V, e)
			}
			if p.RingBytes != want {
				t.Fatalf("RingBytes %d, want %d", p.RingBytes, want)
			}
			if tt.budget <= 0 && p.Budget != DefaultBudget {
				t.Fatalf("default budget %d", p.Budget)
			}
		})
	}
	if p := PlanBudget(0, 0, uniformRings(1000, 64)); p.RingBytes != 1000*16*245*64 {
		t.Fatalf("1,000 rings of 64 use %d bytes", p.RingBytes)
	}
}

// TestPlanBudgetFallback covers 05 req 17: when v = 64 does not fit, rings
// fall back to weighted random, largest first (ties by name), until the rest
// fits, and v is recomputed over the remaining rings.
func TestPlanBudgetFallback(t *testing.T) {
	const room = 16 * 64 * 120 // 64 virtual nodes for 120 Endpoints
	budget := CopyOnWriteReserve + room
	tests := []struct {
		name         string
		rings        map[string]int
		wantFallback []string
		wantV        int
	}{
		{"largest first", map[string]int{"a": 100, "b": 50, "c": 50, "d": 10}, []string{"a"}, 69},
		{"ties by name", map[string]int{"b": 70, "c": 70, "d": 40}, []string{"b"}, 69},
		{"several", map[string]int{"a": 200, "b": 150, "c": 130, "d": 100}, []string{"a", "b", "c"}, 76},
		{"nothing fits", map[string]int{"a": 200, "b": 150}, []string{"a", "b"}, MaxVirtualNodes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := PlanBudget(budget, 0, tt.rings)
			if len(p.Fallback) != len(tt.wantFallback) || p.V != tt.wantV {
				t.Fatalf("fallback %v V %d, want %v V %d", p.Fallback, p.V, tt.wantFallback, tt.wantV)
			}
			for _, name := range tt.wantFallback {
				if v, fb := p.VirtualNodes(name); !fb || v != p.V {
					t.Fatalf("%s not in fallback", name)
				}
			}
			if !p.Fits() || !p.Degraded() {
				t.Fatalf("plan %+v", p)
			}
			warnings := p.Warnings()
			if len(warnings) != len(tt.wantFallback) {
				t.Fatalf("warnings %v", warnings)
			}
			for i, w := range warnings {
				if w.Upstream != tt.wantFallback[i] || !strings.Contains(w.Message, "falls back to weighted random") {
					t.Fatalf("warning %d = %+v", i, w)
				}
			}
		})
	}
}

// TestPlanBudgetSchedulesOverBudget covers S alone exceeding the budget:
// every ring falls back and a Node-wide warning names the schedules.
func TestPlanBudgetSchedulesOverBudget(t *testing.T) {
	p := PlanBudget(20<<20, 8<<20, map[string]int{"a": 5, "b": 3})
	if len(p.Fallback) != 2 || p.Fits() {
		t.Fatalf("plan %+v", p)
	}
	w := p.Warnings()
	if len(w) != 3 || w[0].Upstream != "" || !strings.Contains(w[0].Message, "round-robin schedules") || w[1].Upstream != "a" || w[2].Upstream != "b" {
		t.Fatalf("warnings %+v", w)
	}
	if p := PlanBudget(0, -5, nil); p.ScheduleBytes != 0 {
		t.Fatalf("negative schedules kept: %d", p.ScheduleBytes)
	}
}

// TestReplan covers 05 req 17: Endpoint-set changes lower v at once on
// overflow and raise it only when it can double.
func TestReplan(t *testing.T) {
	prev := PlanBudget(0, 0, uniformRings(1000, 64))
	if prev.V != 245 {
		t.Fatalf("baseline V %d", prev.V)
	}
	tests := []struct {
		name  string
		rings map[string]int
		wantV int
	}{
		{"overflow lowers at once", uniformRings(1000, 70), 224},
		{"small shrink keeps v", uniformRings(1000, 60), 245},
		{"room to double raises", uniformRings(1000, 30), 524},
		{"unchanged", uniformRings(1000, 64), 245},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Replan(prev, 0, 0, tt.rings)
			if p.V != tt.wantV || !p.Fits() {
				t.Fatalf("Replan V %d fits %v, want %d", p.V, p.Fits(), tt.wantV)
			}
		})
	}
	if p := Replan(Plan{}, 0, 0, uniformRings(1000, 64)); p.V != 245 {
		t.Fatalf("Replan from nothing V %d", p.V)
	}
}

// TestReplanFallback covers 05 req 17's fallback under Endpoint-set
// changes: rings fall back at once when v = 64 no longer fits and return as
// soon as the fresh plan needs fewer fallbacks at v ≥ 2 × 64, whatever the
// previous V; meanwhile v follows the doubling rule over the remaining
// rings. The room holds 64 virtual nodes for 120 Endpoints.
func TestReplanFallback(t *testing.T) {
	budget := CopyOnWriteReserve + 16*64*120
	base := map[string]int{"a": 100, "b": 50, "c": 50, "d": 10} // a falls back, V 69
	tests := []struct {
		name         string
		prev, rings  map[string]int
		wantFallback []string
		wantV        int
	}{
		// Every ring fell back (V 1,024 with no ring built); the sets shrink
		// until all fit at 768.
		{"all fell back, then shrink", map[string]int{"a": 200, "b": 150}, map[string]int{"a": 5, "b": 5}, nil, 768},
		// Every ring still falls back after a change: V carries no baseline.
		{"all fell back, ring added", map[string]int{"a": 200, "b": 150}, map[string]int{"a": 200, "b": 150, "c": 10}, []string{"a", "b"}, 768},
		// One large ring fell back while the rest ran at 384; it shrinks and
		// returns at 192.
		{"large ring returns", map[string]int{"big": 500, "s1": 10, "s2": 10}, map[string]int{"big": 20, "s1": 10, "s2": 10}, nil, 192},
		// The same from V 768: a previous V above 512 cannot double, yet the
		// ring returns.
		{"large ring returns over v > 512", map[string]int{"big": 500, "s1": 5, "s2": 5}, map[string]int{"big": 10, "s1": 5, "s2": 5}, nil, 384},
		// Restoring would leave v below 128: the fallback and V stay.
		{"return below 2 x 64 waits", base, map[string]int{"a": 5, "b": 50, "c": 50, "d": 10}, []string{"a"}, 69},
		{"return at 2 x 64", base, map[string]int{"a": 5, "b": 5, "c": 5}, nil, 512},
		// The fallback stays, the remaining rings shrink: v doubles.
		{"raise while a ring stays back", base, map[string]int{"a": 100, "b": 10, "c": 10, "d": 10}, []string{"a"}, 256},
		{"no raise below double", base, map[string]int{"a": 100, "b": 40, "c": 40, "d": 10}, []string{"a"}, 69},
		// v = 64 no longer fits: one more ring falls back at once.
		{"overflow falls back at once", base, map[string]int{"a": 100, "b": 80, "c": 50, "d": 10}, []string{"a", "b"}, 128},
		// A fallback ring removed from the Revision leaves the plan.
		{"removed ring", base, map[string]int{"b": 50, "c": 50, "d": 10}, nil, 69},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := PlanBudget(budget, 0, tt.prev)
			p := Replan(prev, budget, 0, tt.rings)
			got := slices.Sorted(maps.Keys(p.Fallback))
			if !slices.Equal(got, tt.wantFallback) || p.V != tt.wantV || !p.Fits() {
				t.Fatalf("Replan from %v (V %d): fallback %v V %d fits %v; want %v V %d",
					slices.Sorted(maps.Keys(prev.Fallback)), prev.V, got, p.V, p.Fits(), tt.wantFallback, tt.wantV)
			}
			if p.Degraded() != (len(tt.wantFallback) > 0) {
				t.Fatalf("Degraded %v with fallback %v", p.Degraded(), got)
			}
		})
	}
}
