// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

// Per-Node ceilings and fail-open allowances
// (docs/architecture/09-traffic-management-and-resilience.md "Per-Node
// ceiling", foundation pack section 8.8).

// Derivation constants (targets).
const (
	// ceilingFloor is the least derived ceiling, for pinned clients.
	ceilingFloor = 10
	// ceilingSpread is the factor that absorbs uneven spread over Nodes.
	ceilingSpread = 2
	// countlessDivisor gives a Control-mode Node without a count its
	// fail-open allowance, requests / 100.
	countlessDivisor = 100
)

// ceilingOf returns one limits[] entry's per-Node ceiling, its bucket
// capacity and refill per window, and its fail-open allowance per
// epoch-aligned window:
//
//   - a declared perNodeCeiling is both;
//   - with a published count n (Control mode), the ceiling is min(requests,
//     max(10, ceil(2 × requests / n))) and the allowance max(1, 2 ×
//     requests / n), without the floor, which would multiply fail-open
//     admission;
//   - a Control-mode Node without a count uses the full limit, and holds
//     max(1, requests / 100) while failing open;
//   - file mode uses the full limit for both, not degraded.
func ceilingOf(requests, perNode, n int64, control bool) (ceiling, allowance int64) {
	switch {
	case perNode > 0:
		return perNode, perNode
	case control && n > 0:
		ceiling = min(requests, max(ceilingFloor, (ceilingSpread*requests+n-1)/n))
		return ceiling, max(1, ceilingSpread*requests/n)
	case control:
		return requests, max(1, requests/countlessDivisor)
	default:
		return requests, requests
	}
}
