// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"cmp"
	"fmt"
	"slices"
)

// Plan is the Node's balancer memory plan (05 req 17): every ring-hash
// Upstream builds its ring with V virtual nodes per Endpoint unless it is in
// Fallback, where it runs weighted random because no ring fits.
type Plan struct {
	// V is the virtual nodes per Endpoint of every ring, clamp(floor((budget
	// − S − 16 MiB) ÷ (16 B × R)), 64, 1,024), R being the Endpoints summed
	// over the rings that are not in Fallback.
	V int
	// Fallback holds the ring-hash Upstreams, by name, that run weighted
	// random for lack of budget (degraded reason balancer_budget).
	Fallback map[string]bool
	// Budget is the budget planned against, in bytes.
	Budget int64
	// ScheduleBytes is S, the bytes of every round-robin schedule.
	ScheduleBytes int64
	// RingBytes is the bytes of every planned ring at V.
	RingBytes int64
}

// Warning is a configuration warning the plan raises
// (OQ-traffic-management-and-resilience-23 (b): a warning only, no error
// code). Upstream is empty for a Node-wide warning.
type Warning struct {
	// Upstream is the metadata.name of the Upstream concerned.
	Upstream string
	// Message is the warning text.
	Message string
}

// PlanBudget plans the balancer budget when a Revision compiles (05 req
// 17). budget is the Node's balancer budget (≤ 0: DefaultBudget);
// schedules is S, the bytes of every round-robin schedule (ScheduleBytes
// per Upstream); rings maps each ring-hash Upstream to its Endpoint count
// (the current runtime set size for dns Upstreams). When v = 64 does not
// fit, rings fall back to weighted random, largest first (ties by name),
// until the rest fits, and V is recomputed over the remaining rings.
func PlanBudget(budget, schedules int64, rings map[string]int) Plan {
	return plan(budget, schedules, rings, nil)
}

// plan is PlanBudget with the rings of held in fallback from the start.
func plan(budget, schedules int64, rings map[string]int, held map[string]bool) Plan {
	if budget <= 0 {
		budget = DefaultBudget
	}
	schedules = max(schedules, 0)
	avail := budget - schedules - CopyOnWriteReserve
	p := Plan{Fallback: map[string]bool{}, Budget: budget, ScheduleBytes: schedules}
	order := make([]string, 0, len(rings))
	for _, name := range ringOrder(rings) {
		if held[name] {
			p.Fallback[name] = true
		} else {
			order = append(order, name)
		}
	}
	for k := 0; ; k++ {
		active := order[k:]
		var r int64
		for _, name := range active {
			r += int64(max(rings[name], 0))
		}
		if r == 0 {
			p.V = MaxVirtualNodes
			break
		}
		if avail > 0 && avail/(VirtualNodeBytes*r) >= MinVirtualNodes {
			p.V = int(min(avail/(VirtualNodeBytes*r), MaxVirtualNodes))
			break
		}
		if ringsBytes(rings, active, MinVirtualNodes) <= avail {
			p.V = MinVirtualNodes
			break
		}
		p.Fallback[order[k]] = true
	}
	p.RingBytes = ringsBytes(rings, activeRings(rings, p.Fallback), p.V)
	return p
}

// Replan updates a plan after Endpoint-set changes (05 req 17). Rings that
// fell back return as soon as the fresh plan (PlanBudget) needs fewer
// fallbacks and still gives every ring at least 2 × MinVirtualNodes, the
// doubling rule applied to a ring leaving weighted random; more rings fall
// back at once when 64 virtual nodes no longer fit. Otherwise the previous
// fallbacks stay and V, planned over the remaining rings, drops at once when
// the previous V no longer fits and rises only when it can double. A
// previous plan that built no ring (every ring in fallback, or none with
// Endpoints) sets no baseline for V. A zero prev plans afresh.
func Replan(prev Plan, budget, schedules int64, rings map[string]int) Plan {
	fresh := PlanBudget(budget, schedules, rings)
	if prev.V == 0 {
		return fresh
	}
	held := map[string]bool{}
	for name := range prev.Fallback {
		if _, ok := rings[name]; ok {
			held[name] = true
		}
	}
	if len(fresh.Fallback) < len(held) && fresh.V >= 2*MinVirtualNodes {
		return fresh
	}
	kept := plan(budget, schedules, rings, held)
	if len(kept.Fallback) > len(held) {
		return fresh
	}
	if prev.RingBytes == 0 || kept.V >= 2*prev.V {
		return kept
	}
	at := kept
	at.V = prev.V
	at.RingBytes = ringsBytes(rings, activeRings(rings, kept.Fallback), prev.V)
	if !at.Fits() {
		return kept
	}
	return at
}

// ringOrder returns the ring names largest first, ties by name.
func ringOrder(rings map[string]int) []string {
	names := make([]string, 0, len(rings))
	for name := range rings {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		if c := cmp.Compare(rings[b], rings[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	return names
}

// activeRings returns the ring names not in fallback.
func activeRings(rings map[string]int, fallback map[string]bool) []string {
	names := make([]string, 0, len(rings))
	for name := range rings {
		if !fallback[name] {
			names = append(names, name)
		}
	}
	return names
}

// ringsBytes sums RingBytes over the named rings at v.
func ringsBytes(rings map[string]int, names []string, v int) int64 {
	var sum int64
	for _, name := range names {
		sum += RingBytes(v, rings[name])
	}
	return sum
}

// Fits reports whether schedules, rings and the copy-on-write reserve fit
// the budget.
func (p Plan) Fits() bool {
	return p.ScheduleBytes+CopyOnWriteReserve+p.RingBytes <= p.Budget
}

// Degraded reports whether any ring falls back, raising degraded reason
// balancer_budget for the Node and those Upstreams.
func (p Plan) Degraded() bool { return len(p.Fallback) > 0 }

// VirtualNodes returns the plan for one ring-hash Upstream: its virtual
// nodes per Endpoint and whether it falls back (Config.VirtualNodes,
// Config.Fallback, Balancer.SetPlan).
func (p Plan) VirtualNodes(upstream string) (v int, fallback bool) {
	return p.V, p.Fallback[upstream]
}

// Warnings returns the plan's configuration warnings, sorted by Upstream:
// one per fallback ring, and one when the round-robin schedules alone
// exceed the budget.
func (p Plan) Warnings() []Warning {
	var out []Warning
	if p.ScheduleBytes+CopyOnWriteReserve > p.Budget {
		out = append(out, Warning{Message: fmt.Sprintf(
			"balancer budget: round-robin schedules need %d bytes plus the %d-byte copy-on-write reserve, over the %d-byte budget",
			p.ScheduleBytes, CopyOnWriteReserve, p.Budget)})
	}
	names := make([]string, 0, len(p.Fallback))
	for name := range p.Fallback {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		out = append(out, Warning{Upstream: name, Message: fmt.Sprintf(
			"balancer budget: ring-hash Upstream %q falls back to weighted random for lack of room for its ring in the %d-byte budget",
			name, p.Budget)})
	}
	return out
}
