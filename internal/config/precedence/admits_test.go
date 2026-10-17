// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// positionList gives r its own file under dir and a tree whose
// spec.<field> list holds names, entry i at line 10+i, so the findings of
// r sort by entry and after those of every resource with an earlier file.
func positionList(files *tree.FileTable, r *hub.Resource, dir, field string, names []string) {
	path := dir + "/" + r.Name + ".yaml"
	id := files.Add(tree.File{Path: path})
	items := make([]*tree.Node, len(names))
	for i, n := range names {
		p := tree.Pos{File: id, Line: 10 + int32(i), Column: 7}
		items[i] = &tree.Node{Kind: tree.KindMap, Pos: p, Members: []tree.Member{
			{Key: "name", KeyPos: p, Value: &tree.Node{Kind: tree.KindString, Text: n, Pos: p}},
		}}
	}
	spec := &tree.Node{Kind: tree.KindMap, Pos: tree.Pos{File: id, Line: 4, Column: 3}, Members: []tree.Member{
		{Key: field, Value: &tree.Node{Kind: tree.KindList, Items: items}},
	}}
	r.Tree = &tree.Node{Kind: tree.KindMap, Pos: tree.Pos{File: id, Line: 1, Column: 1}, Members: []tree.Member{{Key: "spec", Value: spec}}}
	r.Source.File = path
}

// positionFailureMode gives the Policy r its own file and a positioned
// spec.failureMode.
func positionFailureMode(files *tree.FileTable, r *hub.Resource) {
	path := "policies/" + r.Name + ".yaml"
	id := files.Add(tree.File{Path: path})
	p := tree.Pos{File: id, Line: 6, Column: 16}
	spec := &tree.Node{Kind: tree.KindMap, Pos: tree.Pos{File: id, Line: 4, Column: 3}, Members: []tree.Member{
		{Key: "failureMode", KeyPos: p, Value: &tree.Node{Kind: tree.KindString, Text: "open", Pos: p}},
	}}
	r.Tree = &tree.Node{Kind: tree.KindMap, Pos: tree.Pos{File: id, Line: 1, Column: 1}, Members: []tree.Member{{Key: "spec", Value: spec}}}
	r.Source.File = path
}

// TestAdmitsAtEverySite covers the WP-38 S2 round-4 minor that no call
// site of admits was tested, so dropping the early rejection at
// any one of them (mutants m15 to m19) left the suite green while a
// capped run built every finding. One row per call site, plus the
// Gateway and Upstream lists that share attachList's two: positioned
// input whose findings of the site's code arrive in increasing
// diag.List.Sort order on 1 worker, many more than the cap. Once the
// collector is full every later finding sorts after the kept ones, so
// admits rejects it before it is built and offered: exactly cap+1
// findings are offered, and Run returns the first cap+1 of the uncapped
// run.
func TestAdmitsAtEverySite(t *testing.T) {
	const maxDiagnostics, n = 3, 12
	names := func(prefix string, k int) []string {
		out := make([]string, k)
		for i := range out {
			out[i] = fmt.Sprintf("%s%04d", prefix, i)
		}
		return out
	}
	// routes adds n Routes, each listing policies (or excluding them),
	// positioned under routes/.
	routes := func(b *builder, files *tree.FileTable, policies, excludes []string) {
		for _, r := range names("r", n) {
			b.route(r, policies, excludes, nil)
			field, list := fieldPolicies, policies
			if excludes != nil {
				field, list = fieldExclude, excludes
			}
			positionList(files, b.rs[len(b.rs)-1], "routes", field, list)
		}
	}
	for _, tc := range []struct {
		name, code string
		build      func(b *builder, files *tree.FileTable)
	}{
		{"attachList, scope: Route lists", CodeScope, func(b *builder, files *tree.FileTable) {
			ps := names("oa", 3)
			for _, p := range ps {
				b.policy(p, v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
			}
			routes(b, files, ps, nil)
		}},
		{"attachList, scope: Gateway list", CodeScope, func(b *builder, files *tree.FileTable) {
			ps := names("c", n)
			for _, p := range ps {
				b.policy(p, v1alpha1.PolicyTypeCache, nil, slot(p))
			}
			b.gateway(ps...)
			positionList(files, b.rs[len(b.rs)-1], "gateway", fieldPolicies, ps)
		}},
		{"attachList, scope: Upstream lists", CodeScope, func(b *builder, files *tree.FileTable) {
			b.policy("rl", v1alpha1.PolicyTypeRateLimit, nil)
			for _, u := range names("u", n) {
				b.upstream(u, "rl")
				positionList(files, b.rs[len(b.rs)-1], "upstreams", fieldPolicies, []string{"rl"})
			}
		}},
		{"attachList, slot: Route lists", CodeSlot, func(b *builder, files *tree.FileTable) {
			ps := names("cors", 3)
			for _, p := range ps {
				b.policy(p, v1alpha1.PolicyTypeCORS, nil)
			}
			routes(b, files, ps, nil)
		}},
		{"attachList, slot: Gateway list", CodeSlot, func(b *builder, files *tree.FileTable) {
			ps := names("cors", n+1)
			for _, p := range ps {
				b.policy(p, v1alpha1.PolicyTypeCORS, nil)
			}
			b.gateway(ps...)
			positionList(files, b.rs[len(b.rs)-1], "gateway", fieldPolicies, ps)
		}},
		{"attachList, slot: Upstream lists", CodeSlot, func(b *builder, files *tree.FileTable) {
			ps := names("oa", 2)
			for _, p := range ps {
				b.policy(p, v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
			}
			for _, u := range names("u", n) {
				b.upstream(u, ps...)
				positionList(files, b.rs[len(b.rs)-1], "upstreams", fieldPolicies, ps)
			}
		}},
		{"notOverridable: exclusion", CodeOverridable, func(b *builder, files *tree.FileTable) {
			b.policy("gl", v1alpha1.PolicyTypeRateLimit, nil, locked)
			b.gateway("gl")
			routes(b, files, nil, []string{"gl"})
		}},
		{"notOverridable: replacement", CodeOverridable, func(b *builder, files *tree.FileTable) {
			b.policy("gc", v1alpha1.PolicyTypeCORS, nil, locked)
			b.policy("rc", v1alpha1.PolicyTypeCORS, nil)
			b.gateway("gc")
			routes(b, files, []string{"rc"}, nil)
		}},
		{"cacheGuardrail", CodeCacheGuardrail, func(b *builder, files *tree.FileTable) {
			b.policy("cache", v1alpha1.PolicyTypeCache, nil)
			b.policy("authz", v1alpha1.PolicyTypeAuthzCEL, authzCfg("request.body.ok"))
			routes(b, files, []string{"cache", "authz"}, nil)
		}},
		{"checkPolicies", CodeFailureMode, func(b *builder, files *tree.FileTable) {
			for _, p := range names("jwt", n) {
				b.policy(p, v1alpha1.PolicyTypeAuthJWT, nil, failOpen)
				positionFailureMode(files, b.rs[len(b.rs)-1])
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, files := newBuilder(), &tree.FileTable{}
			tc.build(b, files)
			bundle := b.bundle()
			all := mustRun(t, bundle, files)
			if len(all) < 3*(maxDiagnostics+1) {
				t.Fatalf("%d findings, want at least %d", len(all), 3*(maxDiagnostics+1))
			}
			for i, d := range all {
				if d.Code != tc.code || d.File == "" || (i > 0 && compareHead(all[i-1].Location, all[i-1].Code, d.Location, d.Code) >= 0) {
					t.Fatalf("finding %d is %s at %s:%d:%d; want %s findings at increasing positions", i, d.Code, d.File, d.Line, d.Column, tc.code)
				}
			}
			r := mustNew(t, nil, Options{Body: bodyOracle{}, MaxDiagnostics: maxDiagnostics})
			s, ks, err := r.run(t.Context(), bundle, files, 1)
			if err != nil {
				t.Fatal(err)
			}
			offered := 0
			for _, w := range s.pool.workers {
				offered += w.offered
			}
			if offered != maxDiagnostics+1 {
				t.Errorf("%d findings offered, want %d: admits did not reject the later ones before they were built", offered, maxDiagnostics+1)
			}
			if got := diagnostics(ks); len(got) != maxDiagnostics+1 || diagText(t, got) != diagText(t, all[:maxDiagnostics+1]) {
				t.Errorf("kept\n%s\nwant\n%s", diagText(t, got), diagText(t, all[:maxDiagnostics+1]))
			}
		})
	}
}
