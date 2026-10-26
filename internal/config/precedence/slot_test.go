// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// slotCase is one stage I path that could read or copy a Policy's spec.slot
// per Route, per Upstream or per finding. No schema rule bounds that slot,
// so a slot of length L would cost L per finding if RZ-CFG-018 and
// RZ-CFG-019 quoted it whole, and per Route if the per-list slot indexes
// hashed it.
type slotCase struct {
	name string
	// code is the code of every finding, "" for a case without findings.
	code string
	// findings is the number of findings for units Routes or Upstreams.
	findings func(units int) int
	// keys is the number of attachKeys whose Policy has the long slot:
	// the attachments selectAll interns it for.
	keys int
	// build returns the Bundle with slot big.
	build func(big string, units int) *hub.Bundle
}

// slotCases returns every such path: RZ-CFG-018 in the Gateway list, in
// each Upstream list and in each Route list; RZ-CFG-019 for a Route
// replacing a Gateway Policy with overridable: false; and the paths with
// no finding that compare slots per Route (a Route list, and a
// replacement of an overridable Gateway Policy).
func slotCases() []slotCase {
	routes := func(b *builder, units int, policies ...string) {
		for i := range units {
			b.route(fmt.Sprintf("r%04d", i), policies, nil, nil)
		}
	}
	return []slotCase{
		{"Gateway list: RZ-CFG-018", CodeSlot, func(int) int { return 2 }, 3, func(big string, units int) *hub.Bundle {
			b := newBuilder()
			for _, p := range []string{"a", "b", "c"} {
				b.policy(p, v1alpha1.PolicyTypeRateLimit, nil, slot(big))
			}
			b.gateway("a", "b", "c")
			routes(b, units)
			return b.bundle()
		}},
		{"Upstream lists: RZ-CFG-018", CodeSlot, func(units int) int { return units }, 2, func(big string, units int) *hub.Bundle {
			b := newBuilder()
			for _, p := range []string{"a", "b"} {
				b.policy(p, v1alpha1.PolicyTypeHeaders, headersCfg(true, false), slot(big))
			}
			for i := range units {
				b.upstream(fmt.Sprintf("u%04d", i), "a", "b")
			}
			return b.bundle()
		}},
		{"Route lists: RZ-CFG-018", CodeSlot, func(units int) int { return units }, 2, func(big string, units int) *hub.Bundle {
			b := newBuilder()
			for _, p := range []string{"a", "b"} {
				b.policy(p, v1alpha1.PolicyTypeRateLimit, nil, slot(big))
			}
			routes(b, units, "a", "b")
			return b.bundle()
		}},
		{"Route replacement of a locked Gateway Policy: RZ-CFG-019", CodeOverridable, func(units int) int { return units }, 2, func(big string, units int) *hub.Bundle {
			b := newBuilder()
			b.policy("g", v1alpha1.PolicyTypeRateLimit, nil, slot(big), locked)
			b.policy("x", v1alpha1.PolicyTypeRateLimit, nil, slot(big))
			b.gateway("g")
			routes(b, units, "x")
			return b.bundle()
		}},
		{"Route lists: no finding", "", func(int) int { return 0 }, 1, func(big string, units int) *hub.Bundle {
			b := newBuilder()
			b.policy("g", v1alpha1.PolicyTypeRateLimit, nil)
			b.policy("x", v1alpha1.PolicyTypeRateLimit, nil, slot(big))
			b.gateway("g")
			routes(b, units, "x")
			return b.bundle()
		}},
		{"Route replacement of an overridable Gateway Policy: no finding", "", func(int) int { return 0 }, 2, func(big string, units int) *hub.Bundle {
			b := newBuilder()
			b.policy("g", v1alpha1.PolicyTypeRateLimit, nil, slot(big))
			b.policy("x", v1alpha1.PolicyTypeRateLimit, nil, slot(big))
			b.gateway("g")
			routes(b, units, "x")
			return b.bundle()
		}},
	}
}

// Slot lengths of the long-slot tests: 1 KiB and 1 MiB.
const (
	shortSlot = 1 << 10
	longSlot  = 1 << 20
)

// TestSlotFindingsBounded covers the findings and memory of long slots, for
// every member of slotCases on 1 and 4 workers: each finding quotes the
// first 64 code points of the slot and "...", so its message stays under
// 200 bytes, and a Run with a 1 MiB slot allocates less than 64 KiB more
// than one with a 1 KiB slot (TotalAlloc, an exact count of the bytes the
// run allocated, measured on 1 worker so the collector's growth does not
// depend on scheduling) and less than one copy of the slot in all. A
// finding that quoted the whole slot would hold about three copies of it
// (fmt's %q buffer growing to it) and fail the second bound by about 3 MiB
// per finding.
func TestSlotFindingsBounded(t *testing.T) {
	const units = 8
	quoted := `"` + strings.Repeat("s", 64) + `..."`
	for _, tc := range slotCases() {
		t.Run(tc.name, func(t *testing.T) {
			var allocs [2]uint64
			for i, n := range []int{shortSlot, longSlot} {
				big := strings.Repeat("s", n)
				b := tc.build(big, units)
				for _, workers := range []int{1, 4} {
					r := mustNew(t, nil, Options{Body: bodyOracle{}})
					var before, after runtime.MemStats
					runtime.GC()
					runtime.ReadMemStats(&before)
					_, ds, err := r.Run(t.Context(), b, nil, workers)
					runtime.ReadMemStats(&after)
					if err != nil {
						t.Fatal(err)
					}
					if workers == 1 {
						allocs[i] = after.TotalAlloc - before.TotalAlloc
					}
					if len(ds) != tc.findings(units) {
						t.Fatalf("slot %d B, workers %d: %d findings, want %d:\n%s", n, workers, len(ds), tc.findings(units), diagText(t, ds))
					}
					for _, d := range ds {
						if d.Code != tc.code || len(d.Message) > 200 || !strings.Contains(d.Message, quoted) {
							t.Errorf("slot %d B, workers %d: %s message of %d bytes, want %s quoting %s... in under 200 bytes", n, workers, d.Code, len(d.Message), tc.code, quoted[:9])
							break
						}
					}
				}
			}
			if allocs[1] > allocs[0]+64<<10 || allocs[1] >= longSlot {
				t.Errorf("Run allocated %d B with a 1 KiB slot and %d B with a 1 MiB slot, want under 64 KiB more and under one slot in all", allocs[0], allocs[1])
			}
		})
	}
}
