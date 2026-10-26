// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// ladder is the Route counts R of the PBB ladder (PBB "Startup and
// reload time by configuration size"): R Routes, R/10 Upstreams, R/20
// Policies and one Gateway, 116, 1,151, 5,751 and 11,501 resources.
var ladder = []int{100, 1000, 5000, 10000}

// pbbBundle is the PBB ladder Bundle of r Routes (r a multiple of 20 of
// at least 60): r/10 Upstreams, r/20 Policies and one Gateway, exact and
// template paths, auth.jwt and ratelimit at the Gateway, and authz.cel
// on one Route in twenty, no Plugins (PBB "Startup and reload time by
// configuration size").
func pbbBundle(t testing.TB, r int) *hub.Bundle {
	t.Helper()
	upstreams, authz := r/10, r/20-2
	rs := []*hub.Resource{
		resourceOf(t, jsonTree(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "edge", "labels": {"team": "platform"}}, "spec": {"listeners": [{"name": "http", "port": 8080, "protocol": "http"}], "policies": [{"name": "jwt-default"}, {"name": "ratelimit-global"}]}}`)),
		resourceOf(t, jsonTree(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "jwt-default"}, "spec": {"type": "auth.jwt", "slot": "auth", "failureMode": "closed", "config": {"issuers": [{"issuer": "https://auth.shop.example", "jwksUrl": "https://auth.shop.example/.well-known/jwks.json", "audiences": ["shop-api"]}]}}}`)),
		resourceOf(t, jsonTree(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "ratelimit-global"}, "spec": {"type": "ratelimit", "slot": "ratelimit-global", "failureMode": "open", "overridable": false, "config": {"key": "source.ip", "limits": [{"requests": 1000, "window": "1s"}], "localOnly": false}}}`)),
	}
	for i := range authz {
		s := strconv.Itoa(i)
		rs = append(rs, resourceOf(t, jsonTree(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "authz-`+s+`"}, "spec": {"type": "authz.cel", "slot": "authz-`+s+`", "failureMode": "closed", "config": {"rule": "auth.claims.scope.split(\" \").exists(s, s == \"orders:read`+s+`\")"}}}`)))
	}
	for i := range upstreams {
		s := strconv.Itoa(i)
		rs = append(rs, resourceOf(t, jsonTree(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "upstream-`+s+`"}, "spec": {"protocol": "http", "endpoints": [{"address": "orders-`+s+`.shop.svc:8080", "weight": 1}], "loadBalancing": {"algorithm": "least-request"}, "retries": {"attempts": 2, "retryOn": "error != null ? error.kind in [\"connect\"] : response.status == 503"}, "timeout": "3s"}}`)))
	}
	for i := range r {
		s := strconv.Itoa(i)
		path := `{"exact": "/v1/items/` + s + `"}`
		if i%2 == 1 {
			path = `{"template": "/v1/orders/{id}/` + s + `"}`
		}
		policies := ""
		if i%20 == 0 {
			policies = `, "policies": [{"name": "authz-` + strconv.Itoa(i/20%authz) + `"}]`
		}
		rs = append(rs, resourceOf(t, jsonTree(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "route-`+s+`", "labels": {"team": "t`+strconv.Itoa(i%7)+`", "tier": "gold"}}, "spec": {"match": {"hosts": ["api.shop.example"], "path": `+path+`, "methods": ["GET", "POST"]}`+policies+`, "upstreams": [{"name": "upstream-`+strconv.Itoa(i%upstreams)+`", "weight": 1}], "timeout": "4s"}}`)))
	}
	return hub.NewBundle(rs)
}

// BenchmarkEncode canonicalizes and hashes the ladder Bundles, part of the
// validation budget (target) and of the compile stage of a watched
// directory.
func BenchmarkEncode(b *testing.B) {
	for _, n := range ladder {
		bundle := pbbBundle(b, n)
		for _, w := range []int{1, max(1, runtime.GOMAXPROCS(0)/2)} {
			b.Run(strconv.Itoa(len(bundle.Resources()))+"/workers-"+strconv.Itoa(w), func(b *testing.B) {
				enc := NewEncoder(hubIndex(b))
				b.ReportAllocs()
				var size int
				for b.Loop() {
					rev, err := enc.Encode(context.Background(), bundle, Options{Workers: w})
					if err != nil {
						b.Fatal(err)
					}
					size = len(rev.Content)
				}
				b.SetBytes(int64(size))
			})
		}
	}
}

// BenchmarkVerify is Decoder.Verify on the ladder Revisions, with one
// worker, the Node's max(1, GOMAXPROCS/2) and the CLI's GOMAXPROCS. The
// 5,751-resource one misses the 25 ms verify stage (PBB "Hot Reload
// stages", target), because Verify also decodes and re-validates, which
// PBB counts in its compile stage; PBB's verify stage is the digest and
// signature check. BenchmarkSum is the digest part alone.
func BenchmarkVerify(b *testing.B) {
	procs := runtime.GOMAXPROCS(0)
	workers := slices.Compact([]int{1, max(1, procs/2), procs})
	for _, n := range ladder {
		bundle := pbbBundle(b, n)
		content := mustEncodeBundle(b, bundle)
		want := revision.Sum(content)
		resources := strconv.Itoa(len(bundle.Resources()))
		for _, w := range workers {
			b.Run(resources+"/workers-"+strconv.Itoa(w), func(b *testing.B) {
				dec := NewDecoder(hubIndex(b), DecoderOptions{Workers: w})
				b.ReportAllocs()
				b.SetBytes(int64(len(content)))
				for b.Loop() {
					if _, err := dec.Verify(context.Background(), content, want, Source{Name: "lkg.json"}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkSum is the SHA-256 digest of the ladder Revisions, the part
// of Verify that PBB's verify stage names, alone and in the pieces a
// decode with a yield clock hashes.
func BenchmarkSum(b *testing.B) {
	for _, n := range ladder {
		content := mustEncodeBundle(b, pbbBundle(b, n))
		now, after := due()
		for _, tc := range []struct {
			name string
			y    *yielder
		}{{"whole", nil}, {"pieces", newYielder(now, after)}} {
			b.Run(strconv.Itoa(n)+"/"+tc.name, func(b *testing.B) {
				b.SetBytes(int64(len(content)))
				for b.Loop() {
					_ = sum(content, tc.y)
				}
			})
		}
	}
}

// BenchmarkReadDump reads the /config/dump of the 5,751-resource Revision.
func BenchmarkReadDump(b *testing.B) {
	content := mustEncodeBundle(b, pbbBundle(b, 5000))
	rev := revision.Revision{Digest: revision.Sum(content), Content: content}
	body := dumpOf(b, nil, rev, rev.Digest)
	dec := NewDecoder(hubIndex(b), DecoderOptions{Workers: max(1, runtime.GOMAXPROCS(0)/2)})
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if _, _, err := dec.ReadDump(context.Background(), bytes.NewReader(body), 0, Source{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAppendDump writes the /config/dump document of the
// 5,751-resource Revision, the admin handler's work per request.
func BenchmarkAppendDump(b *testing.B) {
	content := mustEncodeBundle(b, pbbBundle(b, 5000))
	rev := revision.Revision{Digest: revision.Sum(content), Content: content}
	var buf []byte
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	for b.Loop() {
		buf, _ = AppendDump(buf[:0], rev, rev.Digest)
	}
}
