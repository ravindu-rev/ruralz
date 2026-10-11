// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// listTree returns an envelope tree whose spec.<field> lists names, one
// entry per line of file, as a loader would build it.
func listTree(file tree.FileID, field string, names []string) *tree.Node {
	items := make([]*tree.Node, len(names))
	for i, n := range names {
		p := tree.Pos{File: file, Line: int32(i + 10), Column: 7}
		items[i] = &tree.Node{Kind: tree.KindMap, Pos: p, Members: []tree.Member{
			{Key: "name", KeyPos: p, Value: &tree.Node{Kind: tree.KindString, Text: n, Pos: p}},
		}}
	}
	spec := &tree.Node{Kind: tree.KindMap, Pos: tree.Pos{File: file, Line: 5, Column: 3}, Members: []tree.Member{
		{Key: field, Value: &tree.Node{Kind: tree.KindList, Items: items}},
	}}
	return &tree.Node{Kind: tree.KindMap, Pos: tree.Pos{File: file, Line: 1, Column: 1}, Members: []tree.Member{{Key: "spec", Value: spec}}}
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
	b.rs[len(b.rs)-1].Tree = listTree(1, fieldPolicies, gw)
	b.upstream("up", up...)
	b.rs[len(b.rs)-1].Tree = listTree(2, fieldPolicies, up)
	b.route("big", rt, nil, []string{"up"})
	b.rs[len(b.rs)-1].Tree = listTree(3, fieldPolicies, rt)
	for i := range routes {
		b.route(fmt.Sprintf("route-%05d", i), nil, []string{gw[i%n]}, []string{"up"})
	}
	return b.bundle()
}

// TestScale resolves lists of a few thousand members and thousands of
// Routes (S1 note on linear scans): every lookup is by map or index, so the
// run stays linear in the size of its output.
func TestScale(t *testing.T) {
	n, routes := 3000, 100
	if testing.Short() {
		n, routes = 300, 20
	}
	b := scaleBundle(n, routes)
	files := &tree.FileTable{}
	for _, f := range []string{"x.yaml", "gateway.yaml", "upstream.yaml", "route.yaml"} {
		files.Add(tree.File{Path: f})
	}
	start := time.Now()
	chains, ds, err := New(nil, Options{Files: files}).Run(t.Context(), b, runtime.GOMAXPROCS(0))
	if err != nil || len(ds) != 0 {
		t.Fatalf("Run: %v %v", err, ds)
	}
	t.Logf("%d Policies per list, %d Routes: %v", n, routes+1, time.Since(start))
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
