// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Files of benchBundle's positioned trees, by tree.FileID.
const (
	benchGatewayFile tree.FileID = iota
	benchUpstreamFile
	benchRouteFile
)

// benchRouteLines is the number of lines each Route takes in routes.yaml.
const benchRouteLines = 20

// benchBundle is a Bundle of routes Routes shaped like the example: five
// Gateway Policies (two not overridable), per-Route rate limits, a shared
// authz.cel rule and a cors replacement on every tenth Route, and two
// Upstreams with leg Policies. As in the pipeline, the Gateway, every
// Upstream and every Route carry a positioned tree, and the returned
// FileTable names their files, so Resolve locates every attachment
// through the tree walk and FileTable.Location.
func benchBundle(routes int) (*hub.Bundle, *tree.FileTable) {
	b := newBuilder()
	b.policy("cors-default", v1alpha1.PolicyTypeCORS, nil)
	b.policy("cors-partner", v1alpha1.PolicyTypeCORS, nil)
	b.policy("jwt-default", v1alpha1.PolicyTypeAuthJWT, nil)
	b.policy("ratelimit-global", v1alpha1.PolicyTypeRateLimit, nil, locked)
	b.policy("headers-security", v1alpha1.PolicyTypeHeaders, headersCfg(false, true), locked)
	b.policy("authz-orders", v1alpha1.PolicyTypeAuthzCEL, authzCfg(`auth.claims.scope == "orders:read"`))
	b.policy("upstream-oauth", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
	b.policy("headers-internal", v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
	gw := []string{"cors-default", "jwt-default", "ratelimit-global", "headers-security", "authz-orders"}
	b.gateway(gw...)
	b.rs[len(b.rs)-1].Tree = listTree(benchGatewayFile, 1, gw)
	for i, u := range []struct {
		name     string
		policies []string
	}{
		{"orders", []string{"upstream-oauth", "headers-internal"}},
		{"inventory", []string{"headers-internal"}},
	} {
		b.upstream(u.name, u.policies...)
		b.rs[len(b.rs)-1].Tree = listTree(benchUpstreamFile, int32(1+i*benchRouteLines), u.policies)
	}
	for i := range routes {
		rl := fmt.Sprintf("ratelimit-%05d", i)
		b.policy(rl, v1alpha1.PolicyTypeRateLimit, nil)
		policies := []string{rl}
		if i%10 == 0 {
			policies = append(policies, "cors-partner")
		}
		b.route(fmt.Sprintf("route-%05d", i), policies, nil, []string{"orders", "inventory"})
		b.rs[len(b.rs)-1].Tree = listTree(benchRouteFile, int32(1+i*benchRouteLines), policies)
	}
	files := &tree.FileTable{}
	for _, f := range []string{"gateway.yaml", "upstreams.yaml", "routes.yaml"} {
		files.Add(tree.File{Path: f, Role: tree.RoleBase})
	}
	return b.bundle(), files
}

// TestBenchBundleLocated checks that BenchmarkResolve measures the
// production locator path (WP-38 S2 finding): every client and leg entry
// of a benchmark Route carries the file:line its positioned tree and the
// FileTable give it, not the zero location of a tree-less Bundle.
func TestBenchBundleLocated(t *testing.T) {
	bundle, files := benchBundle(20)
	chains, ds, err := mustNew(t, nil, Options{Body: bodyOracle{}}).Run(t.Context(), bundle, files, 4)
	if err != nil || len(ds) != 0 {
		t.Fatalf("Run: %v %v", err, ds)
	}
	want := map[string]string{
		"cors-partner":     "routes.yaml:211",
		"jwt-default":      "gateway.yaml:11",
		"ratelimit-global": "gateway.yaml:12",
		"ratelimit-00010":  "routes.yaml:210",
		"headers-security": "gateway.yaml:13",
		"authz-orders":     "gateway.yaml:14",
		"upstream-oauth":   "upstreams.yaml:10",
	}
	c := chains["route-00010"]
	seen := map[string]bool{}
	check := func(leg string, es []hub.Entry) {
		for _, e := range es {
			at := fmt.Sprintf("%s:%d", e.Ref.File, e.Ref.Line)
			if leg == "inventory" && e.Policy == "headers-internal" {
				if at != "upstreams.yaml:30" {
					t.Errorf("%s %s at %s, want upstreams.yaml:30", leg, e.Policy, at)
				}
				continue
			}
			if leg == "orders" && e.Policy == "headers-internal" {
				if at != "upstreams.yaml:11" {
					t.Errorf("%s %s at %s, want upstreams.yaml:11", leg, e.Policy, at)
				}
				continue
			}
			if w, ok := want[e.Policy]; !ok || at != w {
				t.Errorf("%s %s at %s, want %q", leg, e.Policy, at, w)
			}
			seen[e.Policy] = true
		}
	}
	for p := range phase.Count {
		check(LegClient, c.Client[p])
		for _, l := range c.Legs {
			check(l.Upstream, l.Phases[p])
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("no entry of %s in route-00010", name)
		}
	}
}

// BenchmarkResolve measures Resolver.Run over 5,000 Routes (02 section 6
// benchmarks: "Resolver.Resolve for 5,000 Routes"), on one worker and on
// GOMAXPROCS workers (OQ-performance-budgets-and-benchmarking-6), on the
// production path: positioned trees located through a shared FileTable,
// whose lock every worker takes once per located attachment.
func BenchmarkResolve(b *testing.B) {
	bundle, files := benchBundle(5000)
	r := mustNew(b, nil, Options{Body: bodyOracle{}})
	for _, workers := range []int{1, runtime.GOMAXPROCS(0)} {
		b.Run(fmt.Sprintf("routes=5000/workers=%d", workers), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				chains, ds, err := r.Run(b.Context(), bundle, files, workers)
				if err != nil || len(ds) != 0 || len(chains) != 5000 {
					b.Fatalf("Run: %v %v %d", err, ds, len(chains))
				}
			}
		})
	}
}

// BenchmarkRows measures the effective table of one Route.
func BenchmarkRows(b *testing.B) {
	bundle, files := benchBundle(10)
	r := mustNew(b, nil, Options{Body: bodyOracle{}})
	chains, _, err := r.Run(b.Context(), bundle, files, 1)
	if err != nil {
		b.Fatal(err)
	}
	c := chains["route-00000"]
	b.ReportAllocs()
	for b.Loop() {
		if len(r.Rows(c, bundle)) == 0 {
			b.Fatal("no rows")
		}
	}
}
