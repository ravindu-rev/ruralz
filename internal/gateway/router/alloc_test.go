// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package router

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Allocation gate and benchmarks (docs/architecture/
// 12-performance-budgets-and-benchmarking.md "Per-stage latency budget":
// snapshot pin and Route match, 0 Ruralz-owned allocations; Routers and
// chains for 10,000 Routes built in 2 s or less on one core).

// allocSpecs are Routes of every path tier and criterion kind, without
// match.when, each reachable first through its own host entry.
func allocSpecs() []spec {
	return append(workedExample(),
		spec{"tier", v1alpha1.RouteMatch{
			Hosts: []string{"api.shop.example"}, Path: exactPath("/v1/tier"),
			Headers: []v1alpha1.HeaderMatch{{Name: "x-tier", Exact: "gold"}, {Name: "x-env", Present: present(false)}},
		}},
		spec{"rx", v1alpha1.RouteMatch{Hosts: []string{"rx.example"}, Path: regexPath("/v2/[a-z]+")}},
		spec{"grpc", v1alpha1.RouteMatch{Hosts: []string{"grpc.example"}, GRPC: &v1alpha1.GRPCMatch{Service: "pkg.Svc", Method: "Get"}}},
	)
}

// matchRequest returns the MatchRequest the handler builds for a request
// with an already normalized path.
func matchRequest(method, host, path string, h http.Header) *snapshot.MatchRequest {
	v := &expr.Vars{Request: &expr.Request{Method: method, Host: routematch.NormalizeHost(host), Path: path, Header: h}}
	return &snapshot.MatchRequest{Host: host, Path: path, Method: method, Header: h, Vars: v}
}

// TestMatchAllocatesNothing checks that Match allocates nothing for every
// path tier, header criteria, a grpc criterion, a capture, a miss and a
// host outside the listener hostnames. Hosts are lower-case, as the
// handler passes them; an upper-case host is lower-cased into a new
// string.
func TestMatchAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are meaningless under -race")
	}
	open := newFixture(t, nil, allocSpecs()...).r
	gated := newFixture(t, []string{"api.shop.example"}, workedExample()[:4]...).r
	cases := []struct {
		name string
		r    *Router
		q    *snapshot.MatchRequest
	}{
		{"exact", open, matchRequest("GET", "api.shop.example", "/v1/orders/export", nil)},
		{"template", open, matchRequest("GET", "api.shop.example", "/v1/orders/42/summary", nil)},
		{"prefix", open, matchRequest("DELETE", "api.shop.example", "/v1/orders", nil)},
		{"wildcard host", open, matchRequest("GET", "eu.shop.example:8443", "/v1/x", nil)},
		{"any host", open, matchRequest("GET", "other.example", "/x", nil)},
		{"headers", open, matchRequest("GET", "api.shop.example", "/v1/tier", http.Header{"X-Tier": {"gold"}})},
		{"regex", open, matchRequest("GET", "rx.example", "/v2/abc", nil)},
		{"grpc", open, matchRequest("POST", "grpc.example", "/pkg.Svc/Get", nil)},
		{"miss", gated, matchRequest("GET", "api.shop.example", "/v2", nil)},
		{"outside hostnames", gated, matchRequest("GET", "other.example", "/v1/orders", nil)},
	}
	ctx := t.Context()
	for _, tc := range cases {
		params := make([]expr.Param, 0, tc.r.MaxParams())
		if n := testing.AllocsPerRun(100, func() { tc.r.Match(ctx, tc.q, params) }); n != 0 {
			t.Errorf("%s: %v allocations per Match, want 0", tc.name, n)
		}
	}
}

// benchSpecs returns 10,000 Routes: 100 exact hosts with 100 Routes each,
// spread over the four path tiers, plus a wildcard host and a fallback.
func benchSpecs() []spec {
	specs := make([]spec, 0, 10_002)
	for h := range 100 {
		host := []string{"h" + strconv.Itoa(h) + ".shop.example"}
		for j := range 100 {
			s := "/svc" + strconv.Itoa(j)
			m := v1alpha1.RouteMatch{Hosts: host}
			switch j % 4 {
			case 0:
				m.Path = exactPath(s + "/items/export")
			case 1:
				m.Path = templatePath(s + "/items/{id}")
			case 2:
				m.Path = prefixPath(s)
			default:
				m.Path = regexPath(s + "/r/[0-9]+")
			}
			specs = append(specs, spec{"r" + strconv.Itoa(h) + "-" + strconv.Itoa(j), m})
		}
	}
	return append(specs,
		spec{"wild", v1alpha1.RouteMatch{Hosts: []string{"*.shop.example"}, Path: prefixPath("/")}},
		spec{"fallback", v1alpha1.RouteMatch{Path: prefixPath("/")}},
	)
}

func BenchmarkMatch(b *testing.B) {
	r, err := Compile(nil, compileSpecs(b, benchSpecs()))
	if err != nil {
		b.Fatal(err)
	}
	params := make([]expr.Param, 0, r.MaxParams())
	for _, bc := range []struct {
		name string
		q    *snapshot.MatchRequest
	}{
		{"exact", matchRequest("GET", "h50.shop.example", "/svc48/items/export", nil)},
		{"template", matchRequest("GET", "h50.shop.example", "/svc49/items/42", nil)},
		{"prefix", matchRequest("GET", "h50.shop.example", "/svc50/a/b", nil)},
		{"regex", matchRequest("GET", "h50.shop.example", "/svc51/r/7", nil)},
		{"wildcard", matchRequest("GET", "x.shop.example", "/svc1/items/export", nil)},
		{"fallback", matchRequest("GET", "other.example", "/x", nil)},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				r.Match(b.Context(), bc.q, params)
			}
		})
	}
}

func BenchmarkCompile(b *testing.B) {
	routes := compileSpecs(b, benchSpecs())
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Compile(nil, routes); err != nil {
			b.Fatal(err)
		}
	}
}
