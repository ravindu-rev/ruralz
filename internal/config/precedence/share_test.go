// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// sameArray reports whether two non-empty slices start at one element.
func sameArray(a, b []hub.Entry) bool {
	return len(a) > 0 && len(b) > 0 && &a[0] == &b[0]
}

// materialized counts the entries in the distinct backing arrays of every
// client and leg Phase of chains: what the chains hold in memory, however
// many Routes share each slice.
func materialized(chains map[string]*hub.Chain) int {
	seen := map[*hub.Entry]bool{}
	n := 0
	count := func(l []hub.Entry) {
		if len(l) > 0 && !seen[&l[0]] {
			seen[&l[0]] = true
			n += len(l)
		}
	}
	for _, c := range chains {
		for p := range phase.Count {
			count(c.Client[p])
			for _, l := range c.Legs {
				count(l.Phases[p])
			}
		}
	}
	return n
}

// TestClientPhaseSharing covers the WP-38 S2 round-2 finding that every
// Route copied every remaining Gateway entry into its own chain, so chain
// memory grew with Gateway Policies times Routes, and the class of that
// copy: each client Phase a Route changes nothing in shares the Gateway's
// slice (inherit only, and the untouched Phases of a Route with
// exclusions, replacements or own Policies elsewhere), and each Phase
// changed only by removals shares one slice among the Routes removing the
// same Gateway Policies from it, whether by exclusion or by replacement
// from another Phase (02 req 35, 42), judged by the removals that run in
// that Phase alone. Shared slices have capacity equal to their length, so
// appending through one chain never writes another's.
func TestClientPhaseSharing(t *testing.T) {
	b := newBuilder()
	b.policy("cors-g", v1alpha1.PolicyTypeCORS, nil)                                         // onRequestHeaders, onResponse
	b.policy("rl-g", v1alpha1.PolicyTypeRateLimit, nil)                                      // onRequestHeaders
	b.policy("hdr-g", v1alpha1.PolicyTypeHeaders, headersCfg(false, true), slot("security")) // onResponse
	b.policy("quota-g", v1alpha1.PolicyTypeQuota, nil)                                       // onRequestHeaders, onLog
	b.policy("hdr-r", v1alpha1.PolicyTypeHeaders, headersCfg(false, true))                   // onResponse
	b.policy("sec-r", v1alpha1.PolicyTypeHeaders, headersCfg(true, false), slot("security")) // onRequestHeaders
	b.policy("cors-r", v1alpha1.PolicyTypeCORS, nil)                                         // onRequestHeaders, onResponse
	b.gateway("cors-g", "rl-g", "hdr-g", "quota-g")
	b.route("inherit-a", nil, nil, nil)
	b.route("inherit-b", nil, nil, nil)
	b.route("excl-cors-a", nil, []string{"cors-g"}, nil)
	b.route("excl-cors-b", nil, []string{"cors-g", "absent"}, nil)
	b.route("excl-rl", nil, []string{"rl-g"}, nil)
	b.route("own-resp", []string{"hdr-r"}, nil, nil)
	b.route("repl-security", []string{"sec-r"}, nil, nil)
	b.route("excl-hdr", nil, []string{"hdr-g"}, nil)
	// A replacement in the replaced Policy's own Phases: the Route both
	// removes a Gateway entry from a Phase and adds its own to it, so the
	// built Phase is sized by both (WP-38 S2 round-2 minor, mutant A51).
	b.route("repl-cors", []string{"cors-r"}, nil, nil)
	// Equal removals that run in different Phases (quota-g in
	// onRequestHeaders and onLog, which no other Route removes, and hdr-g
	// in onResponse): each Phase's content is keyed and compared by the
	// removals that run in it alone, so every Phase is shared (the memo;
	// mutant F11c, which recorded every removal of the Route, shared none).
	b.route("excl-two-a", nil, []string{"quota-g", "hdr-g"}, nil)
	b.route("excl-two-b", nil, []string{"hdr-g", "quota-g"}, nil)
	bundle := b.bundle()
	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			chains, ds, err := mustNew(t, nil, Options{}).Run(t.Context(), bundle, nil, workers)
			if err != nil || len(ds) != 0 {
				t.Fatalf("Run: %v %v", err, ds)
			}
			checkInvariants(t, bundle, chains, ds)
			c := func(route string, p phase.Phase) []hub.Entry { return chains[route].Client[p] }
			const (
				hdr  = phase.OnRequestHeaders
				resp = phase.OnResponse
				log  = phase.OnLog
			)
			tests := []struct {
				name   string
				a, b   []hub.Entry
				shared bool
			}{
				{"inherit only: onRequestHeaders", c("inherit-a", hdr), c("inherit-b", hdr), true},
				{"inherit only: onResponse", c("inherit-a", resp), c("inherit-b", resp), true},
				{"inherit only: onLog", c("inherit-a", log), c("inherit-b", log), true},
				{"same exclusion: onRequestHeaders", c("excl-cors-a", hdr), c("excl-cors-b", hdr), true},
				{"same exclusion: onResponse", c("excl-cors-a", resp), c("excl-cors-b", resp), true},
				{"exclusion, untouched Phase: onLog", c("excl-cors-a", log), c("inherit-a", log), true},
				{"exclusion differs from inherit", c("excl-cors-a", hdr), c("inherit-a", hdr), false},
				{"other exclusion differs", c("excl-rl", hdr), c("excl-cors-a", hdr), false},
				{"other exclusion, untouched Phase: onResponse", c("excl-rl", resp), c("inherit-a", resp), true},
				{"own Policy elsewhere, untouched Phase: onRequestHeaders", c("own-resp", hdr), c("inherit-a", hdr), true},
				{"own Policy elsewhere, untouched Phase: onLog", c("own-resp", log), c("inherit-a", log), true},
				{"own Policy's Phase is the Route's", c("own-resp", resp), c("inherit-a", resp), false},
				{"replacement from another Phase equals exclusion: onResponse", c("repl-security", resp), c("excl-hdr", resp), true},
				{"replacing Policy's Phase is the Route's", c("repl-security", hdr), c("inherit-a", hdr), false},
				{"replacement, untouched Phase: onLog", c("repl-security", log), c("inherit-a", log), true},
				{"equal removals across Phases: onRequestHeaders", c("excl-two-a", hdr), c("excl-two-b", hdr), true},
				{"equal removals across Phases: onResponse", c("excl-two-a", resp), c("excl-two-b", resp), true},
				{"a Phase's own removals decide: onResponse as excl-hdr", c("excl-two-a", resp), c("excl-hdr", resp), true},
				{"a Phase's own removals decide: onRequestHeaders differs from excl-rl", c("excl-two-a", hdr), c("excl-rl", hdr), false},
			}
			for _, tc := range tests {
				if got := sameArray(tc.a, tc.b); got != tc.shared {
					t.Errorf("%s: shared = %v, want %v (%q, %q)", tc.name, got, tc.shared, names(tc.a), names(tc.b))
				}
			}
			if got := names(c("repl-security", resp)); !slices.Equal(got, []string{"cors-g"}) {
				t.Errorf("repl-security onResponse = %q, want [cors-g]", got)
			}
			if got := names(c("own-resp", resp)); !slices.Equal(got, []string{"hdr-r", "hdr-g", "cors-g"}) {
				t.Errorf("own-resp onResponse = %q", got)
			}
			if got := names(c("repl-cors", hdr)); !slices.Equal(got, []string{"cors-r", "rl-g", "quota-g"}) {
				t.Errorf("repl-cors onRequestHeaders = %q", got)
			}
			if got := names(c("repl-cors", resp)); !slices.Equal(got, []string{"hdr-g", "cors-r"}) {
				t.Errorf("repl-cors onResponse = %q", got)
			}
			for _, r := range []string{"excl-two-a", "excl-two-b"} {
				if h, l := names(c(r, hdr)), c(r, log); !slices.Equal(h, []string{"cors-g", "rl-g"}) || l != nil {
					t.Errorf("%s: onRequestHeaders %q, onLog %q; want [cors-g rl-g] and nil", r, h, names(l))
				}
			}
			for name, ch := range chains {
				for p := range phase.Count {
					if l := ch.Client[p]; cap(l) != len(l) {
						t.Errorf("%s %s: cap %d, len %d", name, p, cap(l), len(l))
					}
				}
			}
			// Appending through one chain leaves the others as they were.
			before := names(c("inherit-b", hdr))
			grown := append(c("inherit-a", hdr), hub.Entry{Policy: "extra"})
			if got := names(c("inherit-b", hdr)); !slices.Equal(got, before) || sameArray(grown, c("inherit-b", hdr)) {
				t.Errorf("append through inherit-a changed inherit-b: %q", got)
			}
		})
	}
}

// sharingBundle builds a Gateway listing g ratelimit Policies (each in its
// own slot, running in onRequestHeaders only), with a positioned tree,
// one Upstream and r Routes of shape: "inherit" (spec.upstreams only),
// "exclude" (all excluding the same Gateway Policy), "own elsewhere"
// (each attaching a headers Policy that runs only in onResponse, where
// the Gateway runs nothing), "distinct exclusions" (Route i excluding
// Gateway Policy i mod g) or "replacement from another Phase" (Route i
// attaching its own headers Policy, onResponse only, in the slot of
// Gateway Policy i mod g, which it replaces and so removes from
// onRequestHeaders).
func sharingBundle(shape string, g, r int) *hub.Bundle {
	b := newBuilder()
	gw := make([]string, g)
	for i := range g {
		gw[i] = fmt.Sprintf("g%05d", i)
		b.policy(gw[i], v1alpha1.PolicyTypeRateLimit, nil)
	}
	b.gateway(gw...)
	b.rs[len(b.rs)-1].Tree = listTree(1, 1, gw)
	b.policy("hdr-r", v1alpha1.PolicyTypeHeaders, headersCfg(false, true))
	b.upstream("up")
	for i := range r {
		name := fmt.Sprintf("r%05d", i)
		switch shape {
		case "exclude":
			b.route(name, nil, []string{gw[0]}, []string{"up"})
		case "own elsewhere":
			b.route(name, []string{"hdr-r"}, nil, []string{"up"})
		case "distinct exclusions":
			b.route(name, nil, []string{gw[i%g]}, []string{"up"})
		case "replacement from another Phase":
			h := fmt.Sprintf("hdr-%05d", i)
			b.policy(h, v1alpha1.PolicyTypeHeaders, headersCfg(false, true), slot(gw[i%g]))
			b.route(name, []string{h}, nil, []string{"up"})
		default:
			b.route(name, nil, nil, []string{"up"})
		}
	}
	return b.bundle()
}

// TestRemovalKeysDistinct covers the WP-38 S2 round-2 minor that nothing
// pinned that only equal removals share a removal-only Phase: the memo's
// text key without its separator (mutant A44) made a Route removing
// positions 1 and 23 and one removing 1, 2 and 3 ("123" both) share one
// slice, so the second still ran p02 and p03. The memo now keys by a hash
// and compares positions (TestMemoKeyFixedSize forces collisions). With
// 24 Gateway ratelimit Policies, on 1 and 4 workers, each such Route runs
// exactly the Gateway Policies it did not exclude, the two slices are
// distinct, and a third Route with the first one's removals shares its
// slice.
func TestRemovalKeysDistinct(t *testing.T) {
	b := newBuilder()
	gw := make([]string, 24)
	for i := range gw {
		gw[i] = fmt.Sprintf("p%02d", i)
		b.policy(gw[i], v1alpha1.PolicyTypeRateLimit, nil)
	}
	b.gateway(gw...)
	b.route("a", nil, []string{"p01", "p23"}, nil)
	b.route("b", nil, []string{"p01", "p02", "p03"}, nil)
	b.route("c", nil, []string{"p23", "p01"}, nil)
	bundle := b.bundle()
	without := func(ex ...string) []string {
		return slices.DeleteFunc(slices.Clone(gw), func(p string) bool { return slices.Contains(ex, p) })
	}
	for _, workers := range []int{1, 4} {
		chains, ds, err := mustNew(t, nil, Options{}).Run(t.Context(), bundle, nil, workers)
		if err != nil || len(ds) != 0 {
			t.Fatalf("Run: %v %v", err, ds)
		}
		hdr := func(r string) []hub.Entry { return chains[r].Client[phase.OnRequestHeaders] }
		if got := names(hdr("a")); !slices.Equal(got, without("p01", "p23")) {
			t.Errorf("workers %d: a runs %q", workers, got)
		}
		if got := names(hdr("b")); !slices.Equal(got, without("p01", "p02", "p03")) {
			t.Errorf("workers %d: b runs %q", workers, got)
		}
		if sameArray(hdr("a"), hdr("b")) {
			t.Errorf("workers %d: a and b share one slice for different removals", workers)
		}
		if !sameArray(hdr("a"), hdr("c")) {
			t.Errorf("workers %d: a and c do not share one slice for equal removals", workers)
		}
	}
}

// TestMemoKeyFixedSize covers the class member of the WP-38 S2 round-2
// minor on uncharged passes that the memo of removal-only Phases held:
// its key was the removed positions as text, which the memo hashed, and
// compared on a hit, in one uncharged pass of up to 6 bytes per removal.
// The key now has a fixed size (no field whose size grows with the
// removals), and the contents under one key are told apart by their
// positions, each compared as a unit of work (TestWorkCharged). With
// every key forced to collide (the collide hook), on 1 and 4 workers:
// Routes removing distinct sets of equal size from one Phase get distinct
// contents, each running exactly the Gateway Policies it keeps; Routes
// removing equal sets, in any authored order, share one; and the slot of
// the colliding key holds one content per distinct set.
func TestMemoKeyFixedSize(t *testing.T) {
	kt := reflect.TypeFor[phaseKey]()
	for i := range kt.NumField() {
		switch f := kt.Field(i); f.Type.Kind() {
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		default:
			t.Errorf("phaseKey.%s is a %s, whose size can grow with the removals", f.Name, f.Type)
		}
	}
	b := newBuilder()
	gw := make([]string, 24)
	for i := range gw {
		gw[i] = fmt.Sprintf("p%02d", i)
		b.policy(gw[i], v1alpha1.PolicyTypeRateLimit, nil)
	}
	b.gateway(gw...)
	routes := map[string][]string{
		"a": {"p01", "p23"},
		"d": {"p01", "p02"},
		"e": {"p02", "p01"},
		"f": {"p05", "p07"},
		"g": {"p03", "p04", "p05"},
	}
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		b.route(name, nil, routes[name], nil)
	}
	bundle := b.bundle()
	for _, workers := range []int{1, 4} {
		s := newState(mustNew(t, nil, Options{}), bundle, nil, workers)
		s.memo.collide = true
		if err := s.run(t.Context()); err != nil {
			t.Fatal(err)
		}
		hdr := func(r string) []hub.Entry { return s.chains[r].Client[phase.OnRequestHeaders] }
		for name, ex := range routes {
			want := slices.DeleteFunc(slices.Clone(gw), func(p string) bool { return slices.Contains(ex, p) })
			if got := names(hdr(name)); !slices.Equal(got, want) {
				t.Errorf("workers %d: %s runs %q, want %q", workers, name, got, want)
			}
		}
		for _, pair := range [][2]string{{"a", "d"}, {"a", "f"}, {"d", "f"}, {"a", "g"}} {
			if sameArray(hdr(pair[0]), hdr(pair[1])) {
				t.Errorf("workers %d: %s and %s share one slice for different removals", workers, pair[0], pair[1])
			}
		}
		if !sameArray(hdr("d"), hdr("e")) {
			t.Errorf("workers %d: d and e do not share one slice for equal removals", workers)
		}
		if slot := s.memo.slots[phaseKey{p: phase.OnRequestHeaders, n: 2}]; slot == nil || len(slot.contents) != 3 {
			t.Errorf("workers %d: the colliding key holds %v, want 3 contents (a, d and f)", workers, slot)
		}
	}
}

// TestScaleSharedLinear is TestClientPhaseSharing at scale, growing the
// Gateway list and the Route count together, which TestScaleLinear's one
// dimension at a time missed (WP-38 S2 round-2 finding: about 330 B
// allocated per Gateway Policy and Route pair, quadratic in the resource
// count). Exact counters, never a clock or a sampled heap: the entries
// the chains hold (materialized) for each shape, and for the shapes that
// share, doubling g and r together at most doubles the heap allocations
// and bytes of a one-worker run (margin 1.5 for map and slice growth; the
// per-Route copy quadrupled them).
//
// Two shapes do not share, and the round-3 finding on chain size pins
// their cost here so it cannot drift from the contract request that
// bounds it (hub.Chain holds flat per-Phase slices, so a Phase content no
// other Route has is a copy of every remaining Gateway entry): "distinct
// exclusions" holds min(r, g)·(g-1) entries, one memoized onRequestHeaders
// slice per distinct removal set, and "replacement from another Phase"
// the same plus r one-entry onResponse slices. Doubling g and r
// quadruples both.
func TestScaleSharedLinear(t *testing.T) {
	const g, r, margin = 200, 200, 1.5
	for _, tc := range []struct {
		shape string
		// want is the number of materialized entries for g and r.
		want func(g, r int) int
		// shared is set when chain memory is linear in g + r.
		shared bool
	}{
		{"inherit", func(g, _ int) int { return g }, true},
		{"exclude", func(g, _ int) int { return g - 1 }, true},
		{"own elsewhere", func(g, r int) int { return g + r }, true},
		{"distinct exclusions", func(g, r int) int { return min(r, g) * (g - 1) }, false},
		{"replacement from another Phase", func(g, r int) int { return min(r, g)*(g-1) + r }, false},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			for _, n := range [][2]int{{g, r}, {2 * g, 2 * r}, {g, 8 * r}, {8 * g, r}} {
				chains, ds, err := mustNew(t, nil, Options{}).Run(t.Context(), sharingBundle(tc.shape, n[0], n[1]), nil, 4)
				if err != nil || len(ds) != 0 {
					t.Fatalf("Run: %v %v", err, ds)
				}
				if got, want := materialized(chains), tc.want(n[0], n[1]); got != want {
					t.Errorf("g=%d r=%d: %d entries held, want %d", n[0], n[1], got, want)
				}
			}
			if !tc.shared {
				return
			}
			baseA, baseB := heapCost(t, sharingBundle(tc.shape, g, r), nil)
			a, b := heapCost(t, sharingBundle(tc.shape, 2*g, 2*r), nil)
			if float64(a) > 2*margin*float64(baseA) || float64(b) > 2*margin*float64(baseB) {
				t.Errorf("g and r doubled: %d allocations and %d bytes, base %d and %d: growth above linear", a, b, baseA, baseB)
			}
		})
	}
}
