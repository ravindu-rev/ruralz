// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// benchBundle is a 5,000-Route Bundle shaped like the example: five
// Gateway Policies (two not overridable), per-Route rate limits, a shared
// authz.cel rule and a cors replacement on every tenth Route, and two
// Upstreams with leg Policies.
func benchBundle(routes int) *hub.Bundle {
	b := newBuilder()
	b.policy("cors-default", v1alpha1.PolicyTypeCORS, nil)
	b.policy("cors-partner", v1alpha1.PolicyTypeCORS, nil)
	b.policy("jwt-default", v1alpha1.PolicyTypeAuthJWT, nil)
	b.policy("ratelimit-global", v1alpha1.PolicyTypeRateLimit, nil, locked)
	b.policy("headers-security", v1alpha1.PolicyTypeHeaders, headersCfg(false, true), locked)
	b.policy("authz-orders", v1alpha1.PolicyTypeAuthzCEL, authzCfg(`auth.claims.scope == "orders:read"`))
	b.policy("upstream-oauth", v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil)
	b.policy("headers-internal", v1alpha1.PolicyTypeHeaders, headersCfg(true, false))
	b.gateway("cors-default", "jwt-default", "ratelimit-global", "headers-security", "authz-orders")
	b.upstream("orders", "upstream-oauth", "headers-internal")
	b.upstream("inventory", "headers-internal")
	for i := range routes {
		rl := fmt.Sprintf("ratelimit-%05d", i)
		b.policy(rl, v1alpha1.PolicyTypeRateLimit, nil)
		policies := []string{rl}
		if i%10 == 0 {
			policies = append(policies, "cors-partner")
		}
		b.route(fmt.Sprintf("route-%05d", i), policies, nil, []string{"orders", "inventory"})
	}
	return b.bundle()
}

// BenchmarkResolve measures Resolver.Run over 5,000 Routes (02 section 6
// benchmarks: "Resolver.Resolve for 5,000 Routes"), inline and on
// GOMAXPROCS workers (OQ-performance-budgets-and-benchmarking-6).
func BenchmarkResolve(b *testing.B) {
	bundle := benchBundle(5000)
	r := New(nil, Options{Body: bodyOracle{}})
	for _, workers := range []int{1, runtime.GOMAXPROCS(0)} {
		b.Run(fmt.Sprintf("routes=5000/workers=%d", workers), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				chains, ds, err := r.Run(b.Context(), bundle, workers)
				if err != nil || len(ds) != 0 || len(chains) != 5000 {
					b.Fatalf("Run: %v %v %d", err, ds, len(chains))
				}
			}
		})
	}
}

// BenchmarkRows measures the effective table of one Route.
func BenchmarkRows(b *testing.B) {
	bundle := benchBundle(10)
	chains, _, err := New(nil, Options{Body: bodyOracle{}}).Run(b.Context(), bundle, 1)
	if err != nil {
		b.Fatal(err)
	}
	c := chains["route-00000"]
	b.ReportAllocs()
	for b.Loop() {
		if len(Rows(c, bundle)) == 0 {
			b.Fatal("no rows")
		}
	}
}
