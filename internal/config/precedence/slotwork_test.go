// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestSlotWorkOnce covers the work of long slots, for every member of
// slotCases: growing the slot from 1 KiB to 1 MiB changes no site's work
// units but the interning one, which grows by exactly the slot bytes added,
// per byteUnit, once per attachKey of the Policies with that slot, and not
// with the number of Routes or Upstreams (8 and 32). Stage I reads the slot
// text only there; every list compares interned ids (TestSlotIDs), so the
// per-Route cost does not depend on the slot, even for 10,000 Routes on a
// 16 MiB slot.
func TestSlotWorkOnce(t *testing.T) {
	for _, tc := range slotCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, units := range []int{8, 32} {
				short := siteCounts(t, tc.build(strings.Repeat("s", shortSlot), units), nil, true)
				long := siteCounts(t, tc.build(strings.Repeat("s", longSlot), units), nil, true)
				for s := range siteCount {
					want := short[s]
					if s == siteSlotIntern {
						want += tc.keys * (longSlot - shortSlot) / byteUnit
					}
					if long[s] != want {
						t.Errorf("%d units: site %d has %d units with a 1 MiB slot and %d with a 1 KiB slot, want %d", units, s, long[s], short[s], want)
					}
				}
			}
		})
	}
}

// TestSlotIDs pins the interning that keeps slot work bounded: selectAll
// gives every selected attachKey the id of its slot text, equal slots one
// id and distinct slots distinct ids, whatever their scope; each list
// indexes its entries by those ids; and a Policy that is missing, or
// rejected at its scope, interns nothing.
func TestSlotIDs(t *testing.T) {
	b := newBuilder()
	long := strings.Repeat("s", 1000)
	b.policy("rl1", v1alpha1.PolicyTypeRateLimit, nil, slot(long))
	b.policy("rl2", v1alpha1.PolicyTypeRateLimit, nil, slot(long))
	b.policy("rl3", v1alpha1.PolicyTypeRateLimit, nil, slot(long+"t"))
	b.policy("hdr", v1alpha1.PolicyTypeHeaders, headersCfg(true, false), slot(long))
	b.policy("oa", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil, slot("never-interned"))
	b.gateway("rl1", "rl3")
	b.upstream("up", "hdr")
	b.route("r", []string{"rl2", "ghost", "oa"}, nil, nil)
	s := newState(mustNew(t, nil, Options{}), b.bundle(), nil, 1)
	if err := s.selectAll(t.Context()); err != nil {
		t.Fatal(err)
	}
	id := func(p string, scope phase.Scope) int { return s.sels[attachKey{policy: p, scope: scope}].slot }
	if id("rl1", phase.ScopeGateway) != id("rl2", phase.ScopeRoute) || id("rl1", phase.ScopeGateway) != id("hdr", phase.ScopeUpstream) {
		t.Error("equal slots at Gateway, Route and Upstream scope have distinct ids")
	}
	if id("rl1", phase.ScopeGateway) == id("rl3", phase.ScopeGateway) {
		t.Error("distinct slots share an id")
	}
	if len(s.slotIDs) != 2 {
		t.Errorf("%d slots interned, want 2 (a missing or scope-rejected Policy interns nothing)", len(s.slotIDs))
	}
	s.attachGateway()
	for j, it := range s.gateway.items {
		if s.gateway.bySlot[it.slot] != j || s.slotIDs[it.entry.Slot] != it.slot {
			t.Errorf("Gateway entry %d: slot id %d, index %v", j, it.slot, s.gateway.bySlot)
		}
	}
}

// TestClip covers the shortening of a slot quoted in a message: at most
// maxQuoted code points and "...", an invalid UTF-8 byte counting as one
// and kept as it is, and a slot of maxQuoted code points or fewer
// unchanged. A clipped slot costs one allocation and a short one none, and
// an invalid byte is never turned into U+FFFD, so a message's %q writes it
// as \xff whatever the slot's length.
func TestClip(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"short", "auth", "auth"},
		{"exactly maxQuoted", strings.Repeat("a", maxQuoted), strings.Repeat("a", maxQuoted)},
		{"one more", strings.Repeat("a", maxQuoted+1), strings.Repeat("a", maxQuoted) + "..."},
		{"multibyte runes", strings.Repeat("é", maxQuoted+5), strings.Repeat("é", maxQuoted) + "..."},
		{"invalid UTF-8 in the kept prefix, kept as is", "\xff" + strings.Repeat("a", maxQuoted), "\xff" + strings.Repeat("a", maxQuoted-1) + "..."},
		{"invalid UTF-8 at the cut", strings.Repeat("a", maxQuoted) + "\xff", strings.Repeat("a", maxQuoted) + "..."},
		{"short invalid UTF-8 kept as is", "a\xffb", "a\xffb"},
	} {
		got := clip(tc.in)
		if got != tc.want {
			t.Errorf("%s: clip = %q, want %q", tc.name, got, tc.want)
		}
		if n := utf8.RuneCountInString(strings.TrimSuffix(got, "...")); n > maxQuoted {
			t.Errorf("%s: %d code points kept", tc.name, n)
		}
	}
	long := strings.Repeat("s", 1<<20)
	var sink string
	if n := testing.AllocsPerRun(10, func() { sink = clip(long) }); n != 1 {
		t.Errorf("clip of a 1 MiB slot: %v allocations, want 1", n)
	}
	if n := testing.AllocsPerRun(10, func() { sink = clip("auth") }); n != 0 {
		t.Errorf("clip of a short slot: %v allocations, want none", n)
	}
	_ = sink
	for _, in := range []string{"\xff" + strings.Repeat("a", maxQuoted-1), "\xff" + strings.Repeat("a", maxQuoted+1)} {
		if q := fmt.Sprintf("%q", clip(in)); !strings.HasPrefix(q, `"\xffaaa`) {
			t.Errorf("slot of %d bytes quoted as %.12s..., want the invalid byte as \\xff", len(in), q)
		}
	}
}
