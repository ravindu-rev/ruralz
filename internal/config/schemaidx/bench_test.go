// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
)

// The loader walks every resource with the index (01 req 53 budgets 10,000
// resources in 2 s, 02 req 78 gives stages G to M 25% of it), so Walk and
// Lookup must not allocate per visited node beyond the dispatch they
// perform.

const benchPolicy = `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"rl","labels":{"team":"a"}},
"spec":{"type":"ratelimit","failureMode":"open","config":{"key":"consumer.name","limits":[{"requests":100,"window":"1m"}]}}}`

const benchRoute = `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"orders"},
"spec":{"match":{"path":{"prefix":"/v1/orders"},"methods":["GET","POST"],"hosts":["api.example.com"]},
"upstreams":[{"name":"orders","weight":90},{"name":"orders-canary","weight":10}],
"policies":[{"name":"jwt"},{"name":"rl"}],"timeout":"5s"}}`

func BenchmarkLoad(b *testing.B) {
	data := schema.RenderedV1alpha1()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Load("ruralz/v1alpha1", data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWalk(b *testing.B) {
	x := v1(b)
	for _, bc := range []struct{ kind, src string }{{"Policy", benchPolicy}, {"Route", benchRoute}} {
		res := parseTree(b, bc.src)
		b.Run(bc.kind, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				n := 0
				if err := x.Walk(bc.kind, res, func(*Cursor) error { n++; return nil }); err != nil || n == 0 {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkLookup(b *testing.B) {
	x := v1(b)
	res := parseTree(b, benchPolicy)
	path := pathOf("spec", "config", "limits", 0, "window")
	b.ReportAllocs()
	for b.Loop() {
		if info, ok := x.Lookup("Policy", res, path); !ok || !info.Known() {
			b.Fatal("lookup failed")
		}
	}
}

func BenchmarkSelectPolicyConfig(b *testing.B) {
	spec, _ := v1(b).Spec("Policy")
	inst, _ := parseTree(b, benchPolicy).Get("spec")
	b.ReportAllocs()
	for b.Loop() {
		if spec.Select(inst) == spec {
			b.Fatal("no dispatch")
		}
	}
}
