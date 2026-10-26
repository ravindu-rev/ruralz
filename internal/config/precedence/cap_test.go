// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// capCase is one producer of stage I findings, built so that it alone
// raises more findings than the cap of the test.
type capCase struct {
	name string
	code string
	// build returns the Bundle and its FileTable (nil for positionless
	// resources, whose findings all share the empty location, so their
	// order rests on path, message and report order).
	build func(n int) (*hub.Bundle, *tree.FileTable)
}

// positioned gives each Route of b its own file and a positioned
// spec.policies tree, so its findings have distinct locations.
func positioned(b *builder) *tree.FileTable {
	files := &tree.FileTable{}
	files.Add(tree.File{Path: "unused.yaml"})
	for _, r := range b.rs {
		rt, ok := r.Object.(*v1alpha1.Route)
		if !ok {
			continue
		}
		var names []string
		for _, ref := range rt.Spec.Policies {
			names = append(names, ref.Name)
		}
		id := files.Add(tree.File{Path: "routes/" + r.Name + ".yaml"})
		r.Tree = listTree(id, 1, names)
		r.Source.File = "routes/" + r.Name + ".yaml"
	}
	return files
}

// capCases are the producers the diagnostic cap bounds: every list stage I
// checks and every finding it raises there, and CheckPolicies, each alone
// over the cap.
func capCases() []capCase {
	names := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%04d", prefix, i)
		}
		return out
	}
	return []capCase{
		{"Gateway list: Route-only type", CodeScope, func(n int) (*hub.Bundle, *tree.FileTable) {
			b := newBuilder()
			for _, p := range names("c", n) {
				b.policy(p, v1alpha1.PolicyTypeCache, nil, slot(p))
			}
			b.gateway(names("c", n)...)
			return b.bundle(), nil
		}},
		{"Gateway list: one slot", CodeSlot, func(n int) (*hub.Bundle, *tree.FileTable) {
			b := newBuilder()
			for _, p := range names("cors", n+1) {
				b.policy(p, v1alpha1.PolicyTypeCORS, nil)
			}
			b.gateway(names("cors", n+1)...)
			return b.bundle(), nil
		}},
		{"Upstream lists: client-leg type", CodeScope, func(n int) (*hub.Bundle, *tree.FileTable) {
			b := newBuilder()
			ps := names("rl", 4)
			for _, p := range ps {
				b.policy(p, v1alpha1.PolicyTypeRateLimit, nil)
			}
			for _, u := range names("up", (n+3)/4) {
				b.upstream(u, ps...)
			}
			return b.bundle(), nil
		}},
		{"Upstream lists: one slot", CodeSlot, func(n int) (*hub.Bundle, *tree.FileTable) {
			b := newBuilder()
			ps := names("oa", 5)
			for _, p := range ps {
				b.policy(p, v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
			}
			for _, u := range names("up", (n+3)/4) {
				b.upstream(u, ps...)
			}
			return b.bundle(), nil
		}},
		{"Route lists: Upstream-only type", CodeScope, func(n int) (*hub.Bundle, *tree.FileTable) {
			b := newBuilder()
			ps := names("oa", 8)
			for _, p := range ps {
				b.policy(p, v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
			}
			for _, r := range names("r", (n+7)/8) {
				b.route(r, ps, nil, nil)
			}
			return b.bundle(), nil
		}},
		{"Route lists: Upstream-only type, positioned", CodeScope, func(n int) (*hub.Bundle, *tree.FileTable) {
			b := newBuilder()
			ps := names("oa", 8)
			for _, p := range ps {
				b.policy(p, v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
			}
			for _, r := range names("r", (n+7)/8) {
				b.route(r, ps, nil, nil)
			}
			files := positioned(b)
			return b.bundle(), files
		}},
		{"Route lists: one slot", CodeSlot, func(n int) (*hub.Bundle, *tree.FileTable) {
			b := newBuilder()
			ps := names("cors", 5)
			for _, p := range ps {
				b.policy(p, v1alpha1.PolicyTypeCORS, nil)
			}
			for _, r := range names("r", (n+3)/4) {
				b.route(r, ps, nil, nil)
			}
			return b.bundle(), nil
		}},
		{"Route exclusions: locked Gateway Policy", CodeOverridable, func(n int) (*hub.Bundle, *tree.FileTable) {
			b := newBuilder()
			b.policy("gl", v1alpha1.PolicyTypeRateLimit, nil, locked)
			b.gateway("gl")
			for _, r := range names("r", n) {
				b.route(r, nil, []string{"gl"}, nil)
			}
			return b.bundle(), nil
		}},
		{"Route replacements: locked Gateway Policy", CodeOverridable, func(n int) (*hub.Bundle, *tree.FileTable) {
			b := newBuilder()
			b.policy("gc", v1alpha1.PolicyTypeCORS, nil, locked)
			b.policy("rc", v1alpha1.PolicyTypeCORS, nil)
			b.gateway("gc")
			for _, r := range names("r", n) {
				b.route(r, []string{"rc"}, nil, nil)
			}
			return b.bundle(), nil
		}},
		{"Route chains: cache guardrail", CodeCacheGuardrail, func(n int) (*hub.Bundle, *tree.FileTable) {
			b := newBuilder()
			b.policy("cache", v1alpha1.PolicyTypeCache, nil)
			b.policy("authz", v1alpha1.PolicyTypeAuthzCEL, authzCfg("request.body.ok"))
			for _, r := range names("r", n) {
				b.route(r, []string{"cache", "authz"}, nil, nil)
			}
			return b.bundle(), nil
		}},
		{"CheckPolicies: failureMode open", CodeFailureMode, func(n int) (*hub.Bundle, *tree.FileTable) {
			b := newBuilder()
			for _, p := range names("jwt", n) {
				b.policy(p, v1alpha1.PolicyTypeAuthJWT, nil, failOpen)
			}
			return b.bundle(), nil
		}},
	}
}

// TestDiagnosticCap covers the diagnostic cap of stage I for every
// producer: the Gateway list, every Upstream list, every Route (scope,
// slot, exclusion, replacement, guardrail) and CheckPolicies. For each
// producer alone, over a cap of 7 and on 1 and 4 workers: Run returns
// exactly cap+1 findings, the first cap+1 of the uncapped run in
// diag.List.Sort order with ties in report order, the same for both worker
// counts, sorted, and of the producer's code only; and the call's
// collector never held more than cap+1 of them nor a worker more than its
// batch (exact counters, never a sampled heap).
func TestDiagnosticCap(t *testing.T) {
	const maxDiagnostics, n = 7, 60
	for _, tc := range capCases() {
		t.Run(tc.name, func(t *testing.T) {
			b, files := tc.build(n)
			all := mustRun(t, b, files)
			if want := reportOrder(b, all); !reflect.DeepEqual(all, want) {
				t.Fatalf("uncapped findings are not in diag.List.Sort order with ties in report order:\n%s\nwant\n%s", diagText(t, all), diagText(t, want))
			}
			if len(all) <= maxDiagnostics+1 {
				t.Fatalf("the case raises %d findings, want more than %d", len(all), maxDiagnostics+1)
			}
			for _, d := range all {
				if d.Code != tc.code {
					t.Fatalf("the case raises %s, want only %s", d.Code, tc.code)
				}
			}
			var first diag.List
			for _, workers := range []int{1, 4} {
				r := mustNew(t, nil, Options{Body: bodyOracle{}, MaxDiagnostics: maxDiagnostics})
				s, ks, err := r.run(t.Context(), b, files, workers)
				if err != nil {
					t.Fatal(err)
				}
				ds := diagnostics(ks)
				if len(ds) != maxDiagnostics+1 {
					t.Errorf("workers %d: %d diagnostics, want %d", workers, len(ds), maxDiagnostics+1)
				}
				if !reflect.DeepEqual(ds, all[:min(len(all), maxDiagnostics+1)]) {
					t.Errorf("workers %d: kept\n%s\nwant the first of\n%s", workers, diagText(t, ds), diagText(t, all[:maxDiagnostics+1]))
				}
				sorted := slices.Clone(ds)
				sorted.Sort()
				if !reflect.DeepEqual(sorted, ds) {
					t.Errorf("workers %d: result not in diag.List.Sort order", workers)
				}
				if first == nil {
					first = ds
				} else if !reflect.DeepEqual(first, ds) {
					t.Errorf("workers %d differ from workers 1", workers)
				}
				offered := 0
				for j, w := range s.pool.workers {
					offered += w.offered
					if w.held > s.out.batch {
						t.Errorf("workers %d: worker %d buffered %d findings at once, want at most %d", workers, j, w.held, s.out.batch)
					}
				}
				if s.out.heap.peak > maxDiagnostics+1 {
					t.Errorf("workers %d: the collector held %d findings at once, want at most %d", workers, s.out.heap.peak, maxDiagnostics+1)
				}
				if offered == 0 {
					t.Errorf("workers %d: no finding was offered", workers)
				}
			}
		})
	}
}

// reportOrder returns ds in diag.List.Sort order with findings it leaves
// equal in report order, computed independently of compareKept and rank:
// CheckPolicies' findings (on Policies) first, then the Gateway's, the
// Upstreams' and the Routes', each in Bundle order. capCases never raise
// two equal findings in one resource, so that order is total for them.
func reportOrder(b *hub.Bundle, ds diag.List) diag.List {
	group := map[string]int{"Policy": 0, "Gateway": 1, "Upstream": 2, "Route": 3}
	index := map[diag.ResourceID]int{}
	for i, r := range b.Resources() {
		index[*r.ResourceID()] = i
	}
	equal := func(a, b diag.Diagnostic) bool {
		return a.Environment == b.Environment && a.Location == b.Location && a.Code == b.Code &&
			a.Path.String() == b.Path.String() && a.Message == b.Message
	}
	out := slices.Clone(ds)
	out.Sort()
	for i := 0; i < len(out); {
		j := i + 1
		for j < len(out) && equal(out[i], out[j]) {
			j++
		}
		slices.SortStableFunc(out[i:j], func(a, b diag.Diagnostic) int {
			if c := group[a.Resource.Kind] - group[b.Resource.Kind]; c != 0 {
				return c
			}
			return index[*a.Resource] - index[*b.Resource]
		})
		i = j
	}
	return out
}

// mustRun returns every finding of Run on one worker, with a cap far
// above what the tests raise.
func mustRun(t *testing.T, b *hub.Bundle, files *tree.FileTable) diag.List {
	t.Helper()
	_, ds, err := mustNew(t, nil, Options{Body: bodyOracle{}, MaxDiagnostics: 1 << 20}).Run(t.Context(), b, files, 1)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// capShape is a Bundle that floods the cap, at test size: p
// auth.upstream-oauth2 Policies and r Routes that each list every one of
// them (RZ-CFG-020 at Route scope, p per Route), each Route in its own
// file (positioned), so the findings of a Route sort after those of every
// Route before it and each Route reports its own in diag.List.Sort order.
func capShape(p, r int) (*hub.Bundle, *tree.FileTable) {
	b := newBuilder()
	ps := make([]string, p)
	for i := range ps {
		ps[i] = fmt.Sprintf("oa%04d", i)
		b.policy(ps[i], v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
	}
	for i := range r {
		b.route(fmt.Sprintf("r%04d", i), ps, nil, nil)
	}
	files := positioned(b)
	return b.bundle(), files
}

// TestDiagnosticCapBoundsMemory checks that the diagnostic cap bounds the
// findings held and built per call, not per worker: a worker that kept up
// to cap+1 findings for the whole call and rejected a finding only against
// its own last kept one would hold back nothing with as many workers as
// Routes. On capShape, for 1, 4 and r workers (one Route per worker), with
// exact counters, never a sampled heap:
//   - Run returns the same first cap+1 findings for every worker count;
//   - the call's collector holds at most cap+1 findings at once and each
//     worker at most its batch, at most (cap+1)/workers, so the findings
//     held stay under 2(cap+1)+workers whatever the worker count;
//   - the findings built (offered) are exactly cap+1 on one worker, and
//     at most (⌈(cap+1)/p⌉+workers-1)·p on four: a worker takes a Route
//     only after every Route before it but at most workers-1 has merged
//     its findings, and once ⌈(cap+1)/p⌉ earlier Routes have, the bound the
//     collector publishes rejects every finding of the Route before it is
//     built;
//   - Run's TotalAlloc on four workers exceeds one worker's by at most the
//     extra findings that bound allows, at the bytes one finding built
//     costs (measured on one worker as the TotalAlloc a doubled cap adds
//     per finding), plus 64 KiB for the three more workers and their
//     goroutines: the result is the same on every worker count, so what
//     more workers allocate beyond that is the findings they build and
//     drop.
func TestDiagnosticCapBoundsMemory(t *testing.T) {
	const p, routes, maxDiagnostics = 100, 60, 999
	const keep = maxDiagnostics + 1
	b, files := capShape(p, routes)
	all := mustRun(t, b, files)
	if len(all) != p*routes {
		t.Fatalf("%d findings, want %d", len(all), p*routes)
	}
	var allocs [2]uint64
	var perFinding uint64
	for k, workers := range []int{1, 4, routes} {
		r := mustNew(t, nil, Options{Body: bodyOracle{}, MaxDiagnostics: maxDiagnostics})
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		s, ks, err := r.run(t.Context(), b, files, workers)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		if got := diagnostics(ks); !reflect.DeepEqual(got, all[:keep]) {
			t.Fatalf("workers %d: kept findings differ from the first %d", workers, keep)
		}
		offered, buffered := 0, 0
		for j, w := range s.pool.workers {
			offered += w.offered
			buffered += w.held
			if w.held > s.out.batch || s.out.batch > max(1, keep/workers) {
				t.Errorf("workers %d: worker %d buffered %d findings, batch %d, want at most %d", workers, j, w.held, s.out.batch, max(1, keep/workers))
			}
		}
		if held := s.out.heap.peak + buffered; s.out.heap.peak > keep || held >= 2*keep+workers {
			t.Errorf("workers %d: the collector held %d and the workers %d findings at once, want at most %d and under %d in all", workers, s.out.heap.peak, buffered, keep, 2*keep+workers)
		}
		switch workers {
		case 1:
			if offered != keep {
				t.Errorf("workers 1: %d findings built, want exactly %d", offered, keep)
			}
			allocs[0] = after.TotalAlloc - before.TotalAlloc
		case 4:
			if most := ((keep+p-1)/p + workers - 1) * p; offered > most {
				t.Errorf("workers 4: %d findings built, want at most %d", offered, most)
			}
			allocs[1] = after.TotalAlloc - before.TotalAlloc
		default:
		}
		if k == 0 {
			// The bytes one finding built costs: one worker with twice the
			// cap builds twice as many findings and nothing else more.
			r2 := mustNew(t, nil, Options{Body: bodyOracle{}, MaxDiagnostics: 2*keep - 1})
			runtime.GC()
			runtime.ReadMemStats(&before)
			if _, _, err := r2.Run(t.Context(), b, files, 1); err != nil {
				t.Fatal(err)
			}
			runtime.ReadMemStats(&after)
			perFinding = (after.TotalAlloc - before.TotalAlloc - min(allocs[0], after.TotalAlloc-before.TotalAlloc)) / keep
		}
		t.Logf("workers %d: %d built, collector peak %d, buffered %d, batch %d", workers, offered, s.out.heap.peak, buffered, s.out.batch)
	}
	extra := uint64(((keep+p-1)/p+3)*p-keep) * perFinding
	if allocs[1] > allocs[0]+extra+64<<10 {
		t.Errorf("TotalAlloc %d B on 4 workers, %d B on 1, want at most %d B more (%d B per finding)", allocs[1], allocs[0], extra+64<<10, perFinding)
	}
	t.Logf("TotalAlloc: %d B on 1 worker, %d B on 4; %d B per finding", allocs[0], allocs[1], perFinding)
}

// TestRunMergesCheckPolicies covers Run's union of the CheckPolicies and
// Resolve findings, which one collector keeps: with both over the cap,
// Run returns the first cap+1 of their union in diag.List.Sort order, for
// 1 and 4 workers, and CheckPolicies and Resolve each return their own
// first cap+1.
func TestRunMergesCheckPolicies(t *testing.T) {
	b := newBuilder()
	for i := range 20 {
		b.policy(fmt.Sprintf("jwt%02d", i), v1alpha1.PolicyTypeAuthJWT, nil, failOpen)
	}
	b.policy("cache", v1alpha1.PolicyTypeCache, nil)
	b.policy("oa", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
	b.gateway("cache")
	for i := range 20 {
		b.route(fmt.Sprintf("r%02d", i), []string{"oa"}, nil, nil)
	}
	bundle := b.bundle()
	const maxDiagnostics = 9
	all := mustRun(t, bundle, nil)
	union := append(mustNew(t, nil, Options{MaxDiagnostics: 1 << 20}).CheckPolicies(bundle, nil), func() diag.List {
		_, ds, err := mustNew(t, nil, Options{MaxDiagnostics: 1 << 20}).Resolve(t.Context(), bundle, nil, 1)
		if err != nil {
			t.Fatal(err)
		}
		return ds
	}()...)
	union.Sort()
	if !reflect.DeepEqual(all, union) {
		t.Fatalf("uncapped Run differs from CheckPolicies and Resolve sorted together")
	}
	for _, workers := range []int{1, 4} {
		r := mustNew(t, nil, Options{MaxDiagnostics: maxDiagnostics})
		_, ds, err := r.Run(t.Context(), bundle, nil, workers)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ds, all[:maxDiagnostics+1]) {
			t.Errorf("workers %d: Run kept\n%s\nwant\n%s", workers, diagText(t, ds), diagText(t, all[:maxDiagnostics+1]))
		}
		if got := r.CheckPolicies(bundle, nil); len(got) != maxDiagnostics+1 {
			t.Errorf("CheckPolicies returned %d, want %d", len(got), maxDiagnostics+1)
		}
		_, rs, err := r.Resolve(t.Context(), bundle, nil, workers)
		if err != nil || len(rs) != maxDiagnostics+1 {
			t.Errorf("Resolve returned %d, %v; want %d", len(rs), err, maxDiagnostics+1)
		}
	}
}

// TestCompareKeptMatchesSort keeps compareKept equal to diag.List.Sort on
// every key it compares: random diagnostics that collide on each key in
// turn, sorted both ways (the kept ones ranked in list order, so ties
// keep it as the stable sort does), give one order.
func TestCompareKeptMatchesSort(t *testing.T) {
	// A fixed xorshift sequence: reproducible inputs, no math/rand.
	seed := uint64(0x9e3779b97f4a7c15)
	intN := func(n int) int {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return int(seed>>33) % n
	}
	pick := func(vs ...string) string { return vs[intN(len(vs))] }
	for range 200 {
		var ds diag.List
		for range 1 + intN(30) {
			ds = append(ds, diag.Diagnostic{
				Environment: pick("", "prod"),
				Location:    diag.Location{File: pick("", "a.yaml", "b.yaml"), Line: intN(3), Column: intN(3)},
				Code:        pick(CodeSlot, CodeScope),
				Path:        entryPath(pick(fieldPolicies, fieldExclude), pick("a", "b", "a b")),
				Message:     pick("m1", "m2"),
				Resource:    &diag.ResourceID{Kind: "Route", Name: fmt.Sprint(intN(1000))},
			})
		}
		want := slices.Clone(ds)
		want.Sort()
		ks := make([]kept, len(ds))
		for i, d := range ds {
			ks[i] = kept{d: d, path: d.Path.String(), rank: rank{n: i}}
		}
		slices.SortFunc(ks, func(a, b kept) int { return compareKept(&a, &b) })
		if got := diagnostics(ks); !reflect.DeepEqual(got, want) {
			t.Fatalf("compareKept order\n%s\ndiffers from diag.List.Sort\n%s", diagText(t, got), diagText(t, want))
		}
	}
}

// TestCollectorRanksTies covers the report order of findings
// diag.List.Sort leaves equal, which the collector keeps whichever worker
// offered them: two workers sharing one collector of keep 3, fed units in
// any order and interleaved, keep the three first units by rank; a
// finding equal on every sort key is admitted against a full collector
// for the full comparison, and the one of the lower group wins.
func TestCollectorRanksTies(t *testing.T) {
	d := diag.Diagnostic{Code: CodeOverridable, Path: entryPath(fieldExclude, "x"), Message: "m"}
	c := newCollector(3, 2)
	ws := []*worker{{out: c}, {out: c}}
	order := []int{5, 0, 4, 1, 3, 2}
	for k, unit := range order {
		w := ws[k%2]
		w.begin(groupRoutes, unit)
		dd := d
		dd.Resource = &diag.ResourceID{Kind: "Route", Name: fmt.Sprint(unit)}
		if !w.admits(dd.Location, dd.Code) {
			t.Fatalf("unit %d: a finding equal on location and code was not admitted", unit)
		}
		w.offer(dd)
	}
	for _, w := range ws {
		w.flush()
	}
	var got []string
	for _, kk := range c.sorted(nil) {
		got = append(got, kk.d.Resource.Name)
	}
	if !slices.Equal(got, []string{"0", "1", "2"}) {
		t.Errorf("kept %q, want the three first units", got)
	}
	if c.heap.peak != 3 || ws[0].offered+ws[1].offered != len(order) {
		t.Errorf("peak %d, offered %d", c.heap.peak, ws[0].offered+ws[1].offered)
	}

	one := newCollector(1, 1)
	w := &worker{out: one}
	for _, group := range []int{groupGateway, groupPolicies} {
		w.begin(group, 0)
		w.offer(d)
	}
	w.flush()
	if ks := one.sorted(nil); len(ks) != 1 || ks[0].rank.group != groupPolicies {
		t.Errorf("kept %+v, want the finding of the lower group", ks)
	}
}

// TestAdmitsAgainstCollector covers the mechanism of the per-call cap: a
// worker rejects a finding against the bound the call's collector publishes
// once full, whichever worker filled it. Before the collector is full every
// finding is admitted and merged at once; once another worker filled it, a
// finding whose location and code sort after the last kept one is rejected
// before it is built, one equal on them is admitted for the full
// comparison, and the bound only moves down.
func TestAdmitsAgainstCollector(t *testing.T) {
	c := newCollector(1, 2)
	filler, other := &worker{out: c}, &worker{out: c}
	at := func(file string, line, col int) diag.Location {
		return diag.Location{File: file, Line: line, Column: col}
	}
	if !other.admits(at("z.yaml", 99, 99), CodeScope) {
		t.Fatal("an empty collector must admit")
	}
	filler.offer(diag.Diagnostic{Location: at("b.yaml", 5, 3), Code: CodeScope})
	if len(filler.buf) != 0 || len(c.heap.h) != 1 {
		t.Fatalf("a finding offered to a collector that is not full was not merged at once: buffered %d, kept %d", len(filler.buf), len(c.heap.h))
	}
	for _, tc := range []struct {
		at   diag.Location
		code string
		want bool
	}{
		{at("a.yaml", 9, 0), CodeScope, true},
		{at("c.yaml", 0, 0), CodeScope, false},
		{at("b.yaml", 4, 0), CodeScope, true},
		{at("b.yaml", 6, 0), CodeScope, false},
		{at("b.yaml", 5, 2), CodeScope, true},
		{at("b.yaml", 5, 4), CodeScope, false},
		{at("b.yaml", 5, 3), CodeScope, true},
		{at("b.yaml", 5, 3), CodeFailureMode, false},
		{at("b.yaml", 5, 3), CodeSlot, true},
	} {
		if got := other.admits(tc.at, tc.code); got != tc.want {
			t.Errorf("admits(%+v, %s) = %v, want %v", tc.at, tc.code, got, tc.want)
		}
	}
	other.offer(diag.Diagnostic{Location: at("a.yaml", 1, 1), Code: CodeScope})
	other.flush()
	if other.admits(at("b.yaml", 5, 3), CodeScope) {
		t.Error("the bound did not move down to the smaller finding merged by the other worker")
	}
	if ks := c.sorted(nil); len(ks) != 1 || ks[0].d.File != "a.yaml" {
		t.Errorf("kept %+v, want the a.yaml finding", ks)
	}
}

// TestCollectorBatch covers what a worker holds once the collector is
// full: findings are buffered and merged in batches of at most
// collector.batch, at most (cap+1)/workers and maxBatch and at least 1,
// and flush empties the buffer; no clock is read while the collector's
// lock is held, so no worker yields holding it.
func TestCollectorBatch(t *testing.T) {
	for _, tc := range []struct{ keep, workers, batch int }{
		{10_001, 1, maxBatch}, {10_001, 400, 25}, {4, 1, 4}, {4, 8, 1}, {1, 1, 1}, {3, 0, 3},
	} {
		if got := newCollector(tc.keep, tc.workers).batch; got != tc.batch {
			t.Errorf("newCollector(%d, %d).batch = %d, want %d", tc.keep, tc.workers, got, tc.batch)
		}
	}
	c := newCollector(2, 1)
	c.batch = 3
	locked := 0
	w := &worker{out: c, after: time.Hour, now: func() time.Time {
		if !c.mu.TryLock() {
			locked++
		} else {
			c.mu.Unlock()
		}
		return time.Unix(0, 0)
	}}
	for i := range 2 + 3*yieldCheck {
		w.offer(diag.Diagnostic{Location: diag.Location{File: "a.yaml", Line: 1 + i}, Code: CodeScope})
		if len(w.buf) > c.batch {
			t.Fatalf("offer %d: %d findings buffered, batch %d", i, len(w.buf), c.batch)
		}
	}
	w.flush()
	if len(w.buf) != 0 || w.held != c.batch || locked != 0 {
		t.Errorf("buffered %d after flush, held %d, %d clock reads under the lock", len(w.buf), w.held, locked)
	}
	if w.sites[siteFlush] == 0 {
		t.Error("the merges were not charged")
	}
}

// TestNewRejectsNegativeOptions covers the option checks of New: a
// negative MaxDiagnostics or YieldAfter is an ErrOptions error, zero takes
// the defaults.
func TestNewRejectsNegativeOptions(t *testing.T) {
	for _, o := range []Options{{MaxDiagnostics: -1}, {YieldAfter: -1}} {
		if r, err := New(nil, o); r != nil || !errors.Is(err, ErrOptions) {
			t.Errorf("New(%+v) = %v, %v; want ErrOptions", o, r, err)
		}
	}
	r := mustNew(t, nil, Options{})
	if r.maxDiagnostics != DefaultMaxDiagnostics || r.yieldAfter != DefaultYieldAfter || r.now != nil {
		t.Errorf("defaults: %d %v", r.maxDiagnostics, r.yieldAfter)
	}
	if keepOf(1<<62) != 1<<62+1 || keepOf(int(^uint(0)>>1)) != int(^uint(0)>>1) {
		t.Error("keepOf overflows")
	}
}
