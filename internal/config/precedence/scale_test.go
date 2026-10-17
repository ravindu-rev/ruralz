// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// listTree returns an envelope tree that starts at line of file and whose
// spec.policies lists names, one entry per line from line+9, as a loader
// would build it.
func listTree(file tree.FileID, line int32, names []string) *tree.Node {
	items := make([]*tree.Node, len(names))
	for i, n := range names {
		p := tree.Pos{File: file, Line: line + 9 + int32(i), Column: 7}
		items[i] = &tree.Node{Kind: tree.KindMap, Pos: p, Members: []tree.Member{
			{Key: "name", KeyPos: p, Value: &tree.Node{Kind: tree.KindString, Text: n, Pos: p}},
		}}
	}
	spec := &tree.Node{Kind: tree.KindMap, Pos: tree.Pos{File: file, Line: line + 4, Column: 3}, Members: []tree.Member{
		{Key: fieldPolicies, Value: &tree.Node{Kind: tree.KindList, Items: items}},
	}}
	return &tree.Node{Kind: tree.KindMap, Pos: tree.Pos{File: file, Line: line, Column: 1}, Members: []tree.Member{{Key: "spec", Value: spec}}}
}

// scaleBundle builds a Gateway, one Route and one Upstream that each list
// n Policies (with positioned trees), plus routes Routes that inherit the
// Gateway list and exclude every Gateway Policy in turn.
func scaleBundle(n, routes int) *hub.Bundle {
	b := newBuilder()
	gw, rt, up := make([]string, n), make([]string, n), make([]string, n)
	for i := range n {
		gw[i], rt[i], up[i] = fmt.Sprintf("g%05d", i), fmt.Sprintf("r%05d", i), fmt.Sprintf("u%05d", i)
		b.policy(gw[i], v1alpha1.PolicyTypeRateLimit, nil)
		b.policy(rt[i], v1alpha1.PolicyTypeQuota, nil)
		b.policy(up[i], v1alpha1.PolicyTypeHeaders, headersCfg(true, true))
	}
	b.gateway(gw...)
	b.rs[len(b.rs)-1].Tree = listTree(1, 1, gw)
	b.upstream("up", up...)
	b.rs[len(b.rs)-1].Tree = listTree(2, 1, up)
	b.route("big", rt, nil, []string{"up"})
	b.rs[len(b.rs)-1].Tree = listTree(3, 1, rt)
	for i := range routes {
		b.route(fmt.Sprintf("route-%05d", i), nil, []string{gw[i%n]}, []string{"up"})
	}
	return b.bundle()
}

// scaleFiles returns the FileTable of scaleBundle's trees.
func scaleFiles() *tree.FileTable {
	files := &tree.FileTable{}
	for _, f := range []string{"x.yaml", "gateway.yaml", "upstream.yaml", "route.yaml"} {
		files.Add(tree.File{Path: f})
	}
	return files
}

// TestScale resolves lists of a few thousand members and thousands of
// Routes (S1 note on linear scans) and checks the far end of every list.
// It reads no clock (architecture section 0 item 4): BenchmarkResolve
// measures the cost, and TestScaleLinear pins its growth with counters.
func TestScale(t *testing.T) {
	n, routes := 3000, 100
	if testing.Short() {
		n, routes = 300, 20
	}
	b := scaleBundle(n, routes)
	chains, ds, err := mustNew(t, nil, Options{}).Run(t.Context(), b, scaleFiles(), runtime.GOMAXPROCS(0))
	if err != nil || len(ds) != 0 {
		t.Fatalf("Run: %v %v", err, ds)
	}
	big := chains["big"]
	if got := len(big.Client[phase.OnRequestHeaders]); got != 2*n {
		t.Errorf("big onRequestHeaders = %d entries, want %d", got, 2*n)
	}
	if got := len(big.Client[phase.OnLog]); got != n {
		t.Errorf("big onLog = %d entries, want %d", got, n)
	}
	last := big.Client[phase.OnLog][n-1]
	if last.Policy != fmt.Sprintf("r%05d", n-1) || last.Ref.File != "route.yaml" || last.Ref.Line != n-1+10 {
		t.Errorf("last onLog entry = %+v", last)
	}
	leg := big.Legs[0].Phases
	if len(leg[phase.OnUpstreamRequest]) != n || leg[phase.OnUpstreamResponseHeaders][0].Policy != fmt.Sprintf("u%05d", n-1) {
		t.Errorf("leg sizes %d, first response entry %s", len(leg[phase.OnUpstreamRequest]), leg[phase.OnUpstreamResponseHeaders][0].Policy)
	}
	r := chains["route-00007"]
	if len(r.Removed) != 1 || r.Removed[0].Policy != "g00007" || len(r.Client[phase.OnRequestHeaders]) != n-1 {
		t.Errorf("route-00007: removed %+v, %d entries", r.Removed, len(r.Client[phase.OnRequestHeaders]))
	}
}

// heapCost returns the heap allocations and bytes of one Run of b on one
// worker after a warm-up run: exact runtime counters, not a sampled heap
// or a clock.
func heapCost(t *testing.T, b *hub.Bundle, files *tree.FileTable) (allocs, bytes uint64) {
	t.Helper()
	r := mustNew(t, nil, Options{})
	run := func() {
		if _, ds, err := r.Run(t.Context(), b, files, 1); err != nil || len(ds) != 0 {
			t.Fatalf("Run: %v %v", err, ds)
		}
	}
	run()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	return after.Mallocs - before.Mallocs, after.TotalAlloc - before.TotalAlloc
}

// countingOracle counts ReferencesRequestBody calls; it is safe for
// concurrent use.
type countingOracle struct{ calls atomic.Int64 }

func (o *countingOracle) ReferencesRequestBody(src string) (bool, error) {
	o.calls.Add(1)
	return strings.Contains(src, "request.body"), nil
}

// TestScaleLinear asserts the claim TestScale used to log a time for:
// Resolve's cost is linear in each dimension of scaleBundle alone, with
// Phase selection shared by every Route that attaches a Policy (S1 note
// on linear scans). It counts instead of timing: doubling the list length
// n with the Route count fixed, or the Route count with n fixed, at most
// doubles the heap allocations and bytes of a one-worker run with positioned
// trees and a FileTable (a margin of 1.5 covers map and slice growth), so
// no step is quadratic in either one, and the body oracle runs once per
// attached (Policy, scope) however many Routes attach it.
//
// What it does not bound: scaleBundle's Routes each exclude a different
// Gateway Policy, so each already holds its own copy of the other n-1
// Gateway entries (n·routes entries in all, the output itself), and
// growing one dimension at a time cannot tell a per-Route copy from
// linear cost. TestScaleSharedLinear grows the Gateway list and the Route
// count together and pins which shapes share and which copy; allocation
// counters miss a search that allocates nothing, so TestListLocLinear
// counts the locator's item reads.
func TestScaleLinear(t *testing.T) {
	const n, routes, margin = 300, 40, 1.5
	baseA, baseB := heapCost(t, scaleBundle(n, routes), scaleFiles())
	for _, tc := range []struct {
		name     string
		n, route int
	}{
		{"lists doubled", 2 * n, routes},
		{"routes doubled", n, 2 * routes},
	} {
		a, b := heapCost(t, scaleBundle(tc.n, tc.route), scaleFiles())
		if float64(a) > 2*margin*float64(baseA) || float64(b) > 2*margin*float64(baseB) {
			t.Errorf("%s: %d allocations and %d bytes, base %d and %d: growth above linear", tc.name, a, b, baseA, baseB)
		}
	}

	for _, rs := range []int{1, 10, 100} {
		bb := newBuilder()
		var gw, rt []string
		for i := range 5 {
			gw = append(gw, fmt.Sprintf("g%d", i))
			bb.policy(gw[i], v1alpha1.PolicyTypeAuthzCEL, authzCfg("auth.ok"), slot(gw[i]))
			rt = append(rt, fmt.Sprintf("r%d", i))
			bb.policy(rt[i], v1alpha1.PolicyTypeAuthzCEL, authzCfg("request.body.ok"), slot(rt[i]))
		}
		bb.gateway(gw...)
		for i := range rs {
			bb.route(fmt.Sprintf("route-%03d", i), rt, nil, nil)
		}
		o := &countingOracle{}
		if _, ds, err := mustNew(t, nil, Options{Body: o}).Run(t.Context(), bb.bundle(), nil, 4); err != nil || len(ds) != 0 {
			t.Fatalf("Run: %v %v", err, ds)
		}
		if got := o.calls.Load(); got != 10 {
			t.Errorf("%d Routes: %d body oracle calls, want 10 (one per attached Policy and scope)", rs, got)
		}
	}
}
