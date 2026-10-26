// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// BenchmarkResource measures stage G for one resource of each common shape
// (decode and re-validate about 40 µs per resource, hypothesis; stages G,
// I, L and M within 25% of validation).
func BenchmarkResource(b *testing.B) {
	s := newStage(b)
	for name, src := range map[string]string{
		"gateway": gateway(`{"listeners": [{"name": "https", "port": 8443, "protocol": "https", "hostnames": ["API.shop.example"]}, {"name": "http", "port": 8080, "protocol": "http"}],
			"admin": {}, "limits": {"maxRequestBodyBytes": "5Mi"}, "stateStore": {"timeout": "0.05s"}, "telemetry": {}, "trustedProxies": ["10.0.0.0/8", "192.168.0.0/16"],
			"policies": [{"name": "cors"}, {"name": "rl"}, {"name": "jwt"}]}`),
		"route": route(`{"match": {"hosts": ["api.shop.example"], "methods": ["POST", "GET"], "path": {"prefix": "/orders"}}, "timeout": "5s",
			"upstreams": [{"name": "orders"}, {"name": "legacy", "weight": 2}], "policies": [{"name": "a"}, {"name": "b"}]}`),
		"policy": policyDoc("ratelimit", `{"key": "source.ip", "limits": [{"requests": 1000, "window": "60s"}, {"requests": 50, "window": "1s", "burst": 10}]}`),
		"upstream": `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u"}, "spec": {"protocol": "http",
			"endpoints": [{"address": "b:80"}, {"address": "a:80", "weight": 2}], "retries": {}, "circuitBreaker": {}, "healthCheck": {"active": {}, "passive": {}}}}`,
	} {
		b.Run(name, func(b *testing.B) {
			root := mustParse(b, src)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				res := envelope(root.Clone())
				b.StartTimer()
				if h, ds := s.Resource(res, nil); h == nil || len(ds) != 0 {
					b.Fatalf("Resource: %q", texts(ds))
				}
			}
		})
	}
}

// BenchmarkRun measures Run over a synthetic Bundle of 1,151 resources
// (the second rung of the PB ladder: one Gateway, 500 Routes, 500
// Upstreams, 150 Policies) with four workers.
func BenchmarkRun(b *testing.B) {
	s := newStage(b)
	var roots []*tree.Node
	roots = append(roots, mustParse(b, gateway(`{"listeners": [{"name": "http", "port": 8080, "protocol": "http"}], "limits": {}}`)))
	for i := range 500 {
		n := strconv.Itoa(i)
		roots = append(roots,
			mustParse(b, `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r`+n+`"}, "spec": {"match": {"path": {"prefix": "/r`+n+`"}}, "timeout": "5s", "upstreams": [{"name": "u`+n+`"}]}}`),
			mustParse(b, `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u`+n+`"}, "spec": {"protocol": "http", "endpoints": [{"address": "h`+n+`:80"}], "retries": {}}}`))
	}
	for i := range 150 {
		roots = append(roots, mustParse(b, `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p`+strconv.Itoa(i)+`"}, "spec": {"type": "ratelimit", "config": {"limits": [{"requests": 10, "window": "1s"}]}}}`))
	}
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		rs := make([]*tree.Resource, len(roots))
		for i, r := range roots {
			rs[i] = envelope(r.Clone())
		}
		b.StartTimer()
		if bundle, ds, err := s.Run(b.Context(), rs, nil, 4); bundle == nil || len(ds) != 0 || err != nil {
			b.Fatalf("Run: %q %v", texts(ds), err)
		}
	}
}

// BenchmarkScale measures stage G on one large object or list of 1,500
// and 12,000 members, the shapes TestScaleLinear checks by work units and
// allocations: compare the two sizes' ns/op, which should differ by about
// eight times (a sort adds a logarithm), never by sixty-four.
func BenchmarkScale(b *testing.B) {
	s := newStage(b)
	for name, build := range scaleCases() {
		for _, n := range []int{1500, 12_000} {
			b.Run(name+"/"+strconv.Itoa(n), func(b *testing.B) {
				root := mustParse(b, build(n))
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					res := envelope(root.Clone())
					b.StartTimer()
					if h, ds := s.Resource(res, nil); h == nil || len(ds) != 0 {
						b.Fatalf("Resource: %q", texts(ds))
					}
				}
			})
		}
	}
}

// BenchmarkScaleErrors measures stage G on the failing shapes of
// TestScaleLinearErrors, one RZ-CFG-005 per member of a large list
// element, at 1,000 and 8,000 members (below the cap) and 96,000 (above
// it): ns/op should grow about eight times from 1,000 to 8,000, never
// sixty-four, and at most twelve times again to 96,000.
func BenchmarkScaleErrors(b *testing.B) {
	s := newStage(b)
	idx := setSchema(b)
	cases := errorScaleCases()
	cases["set element nulls"] = setElementNulls
	for name, build := range cases {
		for _, n := range []int{1000, 8000, 96_000} {
			b.Run(name+"/"+strconv.Itoa(n), func(b *testing.B) {
				root := mustParse(b, build(n))
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					res := envelope(root.Clone())
					b.StartTimer()
					var failed bool
					if name == "set element nulls" {
						_, prior := normalize(idx, res, nil, newWork())
						failed = prior != nil
					} else {
						h, _ := s.Resource(res, nil)
						failed = h == nil
					}
					if !failed {
						b.Fatal("Resource passed")
					}
				}
			})
		}
	}
}
