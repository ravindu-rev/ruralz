// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// longName returns a name of n bytes, longer than any resource name stage
// F accepts when n exceeds maxRefBytes.
func longName(prefix string, n int) string { return prefix + strings.Repeat("x", n-len(prefix)) }

// totalWork runs Run on b on one worker and returns its work units.
func totalWork(t *testing.T, b *hub.Bundle) int {
	t.Helper()
	s, _, err := mustNew(t, nil, Options{Body: bodyOracle{}}).run(t.Context(), b, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, w := range s.pool.workers {
		n += w.total
	}
	return n
}

// TestLongReferencesUnresolved covers the WP-38 S2 round-2 finding that
// stage I hashed and compared reference names of any length at one work
// unit each (PolicyRef.name, RouteUpstream.name, CompositionStep.upstream
// and PolicySpec.plugin have no schema bound), so a 12 MiB name stalled a
// worker for milliseconds between two reads of the yield timer. A
// reference longer than maxRefBytes names no resource of a Bundle stage F
// accepted, so stage I treats it as unresolved (02 req 40) without
// reading it. One row per site that can resolve one, each with a resource
// of that long name that the builder (unlike stage F) lets exist, so the
// pre-fix code resolved it: a Gateway, a Route and an Upstream list entry,
// a plugin Policy's spec.plugin, spec.upstreams and composition steps.
// Each row asserts the reference changes nothing (no entry, leg or
// finding), and that the run's work units are the same for a name of 64
// bytes and of 1 MiB. The exclusion and tree-index sites, where nothing
// observable changes, are TestWorkCharged's siteLongExclude and
// siteLongLocate rows.
func TestLongReferencesUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(long string) *hub.Bundle
	}{
		{"Gateway spec.policies", func(long string) *hub.Bundle {
			b := newBuilder()
			b.policy(long, v1alpha1.PolicyTypeRateLimit, nil)
			b.gateway(long)
			b.route("r", nil, nil, nil)
			return b.bundle()
		}},
		{"Route spec.policies", func(long string) *hub.Bundle {
			b := newBuilder()
			b.policy(long, v1alpha1.PolicyTypeRateLimit, nil)
			b.route("r", []string{long}, nil, nil)
			return b.bundle()
		}},
		{"Upstream spec.policies", func(long string) *hub.Bundle {
			b := newBuilder()
			b.policy(long, v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
			b.upstream("u", long)
			b.route("r", nil, nil, []string{"u"})
			return b.bundle()
		}},
		{"plugin Policy spec.plugin", func(long string) *hub.Bundle {
			b := newBuilder()
			b.plugin(long, v1alpha1.PhaseOnRequestHeaders)
			b.policy("pp", v1alpha1.PolicyTypePlugin, nil, pluginOf(long, v1alpha1.FilterClassCustom))
			b.route("r", []string{"pp"}, nil, nil)
			return b.bundle()
		}},
		{"Route spec.upstreams", func(long string) *hub.Bundle {
			b := newBuilder()
			b.upstream(long)
			b.route("r", nil, nil, []string{long})
			return b.bundle()
		}},
		{"Route spec.composition.steps", func(long string) *hub.Bundle {
			b := newBuilder()
			b.upstream(long)
			rt := b.route("r", nil, nil, nil)
			rt.Spec.Composition = &v1alpha1.Composition{Mode: v1alpha1.CompositionModeSequential}
			rt.Spec.Composition.Steps = []v1alpha1.CompositionStep{{Name: "s", Upstream: long}}
			return b.bundle()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var work []int
			for _, n := range []int{maxRefBytes + 1, 1 << 20} {
				long := longName("l", n)
				b := tc.build(long)
				for _, workers := range []int{1, 4} {
					chains, ds, err := mustNew(t, nil, Options{Body: bodyOracle{}}).Run(t.Context(), b, nil, workers)
					if err != nil {
						t.Fatal(err)
					}
					c := chains["r"]
					entries := 0
					for p := range phase.Count {
						entries += len(c.Client[p])
						for _, l := range c.Legs {
							entries += len(l.Phases[p])
						}
					}
					if _, ok := c.Leg(long); ok {
						entries++
					}
					if entries != 0 || len(ds) != 0 {
						t.Errorf("name of %d bytes, workers %d: %d entries and legs, %d findings; want the reference unresolved", n, workers, entries, len(ds))
					}
				}
				work = append(work, totalWork(t, b))
			}
			if work[0] != work[1] {
				t.Errorf("work units %d for a name of %d bytes and %d for 1 MiB, want equal", work[0], maxRefBytes+1, work[1])
			}
		})
	}
	// The boundary: a name of exactly maxRefBytes still resolves.
	b := newBuilder()
	name := longName("l", maxRefBytes)
	b.policy(name, v1alpha1.PolicyTypeRateLimit, nil)
	b.route("r", []string{name}, nil, nil)
	if chains, _ := resolve(t, b.bundle()); len(chains["r"].Client[phase.OnRequestHeaders]) != 1 {
		t.Errorf("a name of %d bytes did not resolve", maxRefBytes)
	}
}

// TestAbsentReferencesTakeNoKeys covers the WP-38 S2 round-2 minor that
// stage I kept a selection (200 bytes), a map entry and a key for every
// distinct name it was asked to attach, a Policy or not, so references
// it skips (RZ-CFG-009 is stage H's, 02 req 40) cost about 309 bytes
// each: 1.2 GB for a 64 MiB source. Only names that resolve to a Policy
// take a key; with 50 Routes each listing 1,000 or 4,000 distinct absent
// names beside one Policy, the keys and selections stay one, on 1 and 4
// workers, and the Policy still attaches to every Route.
func TestAbsentReferencesTakeNoKeys(t *testing.T) {
	for _, n := range []int{1000, 4000} {
		b := newBuilder()
		b.policy("rl", v1alpha1.PolicyTypeRateLimit, nil)
		for i := range 50 {
			refs := []string{"rl"}
			for j := range n {
				refs = append(refs, fmt.Sprintf("absent-%d-%d", i, j))
			}
			b.route(fmt.Sprintf("r%02d", i), refs, nil, nil)
		}
		bundle := b.bundle()
		for _, workers := range []int{1, 4} {
			s, _, err := mustNew(t, nil, Options{}).run(t.Context(), bundle, nil, workers)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.keys) != 1 || len(s.keyRes) != 1 || len(s.sels) != 1 {
				t.Errorf("%d absent names per Route, workers %d: %d keys, %d selections, want 1", n, workers, len(s.keys), len(s.sels))
			}
			for name, c := range s.chains {
				if got := names(c.Client[phase.OnRequestHeaders]); len(got) != 1 || got[0] != "rl" {
					t.Errorf("%s: onRequestHeaders %q, want [rl]", name, got)
				}
			}
		}
	}
}
