// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
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

// TestLongReferencesUnresolved checks that stage I never hashes or compares
// a reference name of any length at one work unit (PolicyRef.name,
// RouteUpstream.name, CompositionStep.upstream and PolicySpec.plugin have
// no schema bound), where a 12 MiB name would stall a worker for
// milliseconds between two reads of the yield timer. A reference longer
// than maxRefBytes names no resource of a Bundle stage F accepted, so
// stage I treats it as unresolved without reading it. One row per site
// that can resolve one, each with a resource of that long name that the
// builder (unlike stage F) lets exist, so code that read the name would
// resolve it: a Gateway, a Route and an Upstream list entry, a plugin
// Policy's spec.plugin, spec.upstreams and composition steps. Each row
// asserts the reference changes nothing (no entry, leg or finding), and
// that the run's work units are the same for a name of 64 bytes and of
// 1 MiB. The exclusion and tree-index sites, where nothing observable
// changes, are TestWorkCharged's siteLongExclude and siteLongLocate rows.
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

// TestAbsentReferencesTakeNoKeys checks that stage I keeps no selection
// (200 bytes), map entry or key for a distinct name it is asked to attach
// that is not a Policy, so the references it skips (RZ-CFG-009 is
// stage H's) never cost about 309 bytes each, 1.2 GB for a 64 MiB source.
// Only names that resolve to a Policy take a key; with 50 Routes each
// listing 1,000 or 4,000 distinct absent names beside one Policy, the keys
// and selections stay one, on 1 and 4 workers, and the Policy still
// attaches to every Route.
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

// unpositioned gives r a tree whose spec.<field> list holds names, with
// no position on any entry, so the locator finds an entry through its
// name index.
func unpositioned(files *tree.FileTable, r *hub.Resource, field string, names []string) {
	positionList(files, r, "routes", field, names)
	spec := member(r.Tree, "spec").Value
	for _, it := range member(spec, field).Value.Items {
		it.Pos = tree.Pos{}
	}
}

// TestAbsentReferencesAllocateNothing covers the rest of the memory of
// absent references (TestAbsentReferencesTakeNoKeys covers the keys): no
// Route list makes room for an entry that resolves to nothing (a list item
// and two index entries), the locator's name index holds no such name of a
// list whose tree it cannot read by index, and the leg order neither
// buffers nor indexes such an Upstream name; otherwise a reference that
// resolves to nothing would cost about 260 bytes (316 through the name
// index) or 71 bytes in each run, transient but at once for one list. One
// row per kind of reference: a Route's spec.policies, the same through the
// name index, spec.excludePolicies, the same through the name index,
// spec.upstreams and composition steps. 50 Routes each name n absent
// references beside one that resolves, on 1 and 4 workers: Run's TotalAlloc
// for n = 4,000 exceeds that for n = 1,000 by under 64 KiB in all (150,000
// more references, under half a byte each; a counter, never a sampled
// heap), and the reference that resolves still takes effect in every Route.
func TestAbsentReferencesAllocateNothing(t *testing.T) {
	absent := func(i, n int) []string {
		out := make([]string, n)
		for j := range out {
			out[j] = fmt.Sprintf("absent-%d-%d", i, j)
		}
		return out
	}
	type shape struct {
		name string
		// route adds Route name, naming present and n absent references.
		route func(b *builder, files *tree.FileTable, name string, refs []string)
		// check reports what the reference that resolves must do in c.
		check func(c *hub.Chain) bool
		// findings is the number of findings of the run.
		findings int
	}
	attached := func(c *hub.Chain) bool {
		return slices.Equal(names(c.Client[phase.OnRequestHeaders]), []string{"gl", "rl"})
	}
	leg := func(c *hub.Chain) bool { return len(c.Legs) == 1 && c.Legs[0].Upstream == "up" }
	excludedLocked := func(c *hub.Chain) bool { return slices.Equal(names(c.Client[phase.OnRequestHeaders]), []string{"gl"}) }
	for _, sh := range []shape{
		{"spec.policies", func(b *builder, _ *tree.FileTable, name string, refs []string) {
			b.route(name, append([]string{"rl"}, refs...), nil, nil)
		}, attached, 0},
		{"spec.policies through the name index", func(b *builder, files *tree.FileTable, name string, refs []string) {
			list := append([]string{"rl"}, refs...)
			b.route(name, list, nil, nil)
			unpositioned(files, b.rs[len(b.rs)-1], fieldPolicies, list)
		}, attached, 0},
		{"spec.excludePolicies", func(b *builder, _ *tree.FileTable, name string, refs []string) {
			b.route(name, nil, append([]string{"gl"}, refs...), nil)
		}, excludedLocked, 50},
		{"spec.excludePolicies through the name index", func(b *builder, files *tree.FileTable, name string, refs []string) {
			list := append([]string{"gl"}, refs...)
			b.route(name, nil, list, nil)
			unpositioned(files, b.rs[len(b.rs)-1], fieldExclude, list)
		}, excludedLocked, 50},
		{"spec.upstreams", func(b *builder, _ *tree.FileTable, name string, refs []string) {
			b.route(name, nil, nil, append([]string{"up"}, refs...))
		}, leg, 0},
		{"spec.composition.steps", func(b *builder, _ *tree.FileTable, name string, refs []string) {
			rt := b.route(name, nil, nil, nil)
			rt.Spec.Composition = &v1alpha1.Composition{Mode: v1alpha1.CompositionModeSequential}
			for j, u := range append([]string{"up"}, refs...) {
				rt.Spec.Composition.Steps = append(rt.Spec.Composition.Steps, v1alpha1.CompositionStep{Name: fmt.Sprintf("s%d", j), Upstream: u})
			}
		}, leg, 0},
	} {
		t.Run(sh.name, func(t *testing.T) {
			for _, workers := range []int{1, 4} {
				var total [2]uint64
				for k, n := range []int{1000, 4000} {
					b := newBuilder()
					b.policy("rl", v1alpha1.PolicyTypeRateLimit, nil)
					b.policy("gl", v1alpha1.PolicyTypeCORS, nil, locked)
					b.gateway("gl")
					b.upstream("up")
					files := &tree.FileTable{}
					for i := range 50 {
						sh.route(b, files, fmt.Sprintf("r%02d", i), absent(i, n))
					}
					bundle := b.bundle()
					r := mustNew(t, nil, Options{Body: bodyOracle{}})
					var before, after runtime.MemStats
					runtime.GC()
					runtime.ReadMemStats(&before)
					chains, ds, err := r.Run(t.Context(), bundle, files, workers)
					runtime.ReadMemStats(&after)
					if err != nil {
						t.Fatal(err)
					}
					total[k] = after.TotalAlloc - before.TotalAlloc
					if len(ds) != sh.findings {
						t.Errorf("n=%d workers %d: %d findings, want %d", n, workers, len(ds), sh.findings)
					}
					for name, c := range chains {
						if !sh.check(c) {
							t.Errorf("n=%d workers %d: %s lost the reference that resolves: onRequestHeaders %q, %d legs",
								n, workers, name, names(c.Client[phase.OnRequestHeaders]), len(c.Legs))
							break
						}
					}
					if len(chains) != 50 {
						t.Errorf("n=%d workers %d: %d chains, want 50", n, workers, len(chains))
					}
				}
				if total[1] > total[0]+64<<10 {
					t.Errorf("workers %d: TotalAlloc %d B for 4,000 absent references per Route, %d B for 1,000: %.1f B per added reference",
						workers, total[1], total[0], float64(total[1]-total[0])/(50*3000))
				}
			}
		})
	}
}
