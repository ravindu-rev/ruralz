// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Microbenchmarks of docs/engineering/03-testing-and-quality-strategy.md
// "Benchmarks": typical match and key expressions run under 2 µs at p99
// (target, docs/architecture/02-configuration-model.md "Limits"), and the
// worst-case fixtures of ADR-0019 end within 50 ms, finished or stopped.

// benchBool reports the evaluation of a bool program.
func benchBool(b *testing.B, site expr.Site, src string, v *expr.Vars) {
	prg := mustProgram(b, newCompiler(b), site, src)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = prg.EvalBool(ctx, v)
	}
}

// benchString reports the evaluation of a string program.
func benchString(b *testing.B, site expr.Site, src string, v *expr.Vars) {
	prg := mustProgram(b, newCompiler(b), site, src)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = prg.EvalString(ctx, v)
	}
}

func BenchmarkMatchWhen(b *testing.B) {
	benchBool(b, at(expr.PlaceRouteMatchWhen), `request.headers["x-tenant"] == "acme"`, testVars())
}

func BenchmarkRateLimitKey(b *testing.B) {
	benchString(b, at(expr.PlaceRateLimitKey), `consumer == null ? source.ip : consumer.name`, testVars())
}

func BenchmarkAuthzGold(b *testing.B) {
	benchBool(b, at(expr.PlaceAuthzCELRule), `consumer != null && consumer.tier == "gold"`, testVars())
}

func BenchmarkAuthzOrders(b *testing.B) {
	benchBool(b, at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read")`, testVars())
}

func BenchmarkAccessLogWhen(b *testing.B) {
	benchBool(b, at(expr.PlaceAccessLogWhen), `response.status >= 400 || duration > duration("1s")`, testVars())
}

func BenchmarkDefaultRetryOn(b *testing.B) {
	prg := newCompiler(b).Default(expr.PlaceRetryOn)
	v := outcome{errKind: "reset"}.vars("GET")
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = prg.EvalBool(ctx, v)
	}
}

func BenchmarkDecodeJSON1KiB(b *testing.B) {
	items := make([]map[string]any, 12)
	for i := range items {
		items[i] = map[string]any{"sku": "SKU-0000" + string(rune('a'+i)), "qty": i, "price": 12.5}
	}
	data, err := json.Marshal(map[string]any{"id": 7, "customer": "acme", "items": items})
	if err != nil {
		b.Fatal(err)
	}
	c := newCompiler(b)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := c.DecodeJSON(data, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompile(b *testing.B) {
	c := newCompiler(b)
	src := `auth.claims.scope.split(" ").exists(s, s == "orders:read")`
	b.ReportAllocs()
	for b.Loop() {
		if prg, iss := c.program(at(expr.PlaceAuthzCELRule), src); prg == nil {
			b.Fatal(iss)
		}
	}
}

// BenchmarkWorstCaseSplit splits a 64 KiB header value
// (limits.maxRequestHeaderBytes) into 32,768 items and scans them with map
// and exists: an expression that passes RZ-CFG-015 at nominal sizes and
// runs as a tracked comprehension, finished or stopped at 50 ms.
func BenchmarkWorstCaseSplit(b *testing.B) {
	v := testVars()
	v.Request.Header.Set("X-Tenant", strings.Repeat("a,", 32<<10))
	benchBool(b, at(expr.PlaceRouteMatchWhen),
		`request.headers["x-tenant"].split(",").map(s, s).exists(s, s == "zz")`, v)
}

// BenchmarkWorstCaseNested runs a 32 × 32 nested comprehension over a body
// list that passes RZ-CFG-015, finished or stopped at 50 ms.
func BenchmarkWorstCaseNested(b *testing.B) {
	list := make([]any, 32)
	for i := range list {
		list[i] = json.Number("1")
	}
	v := testVars()
	v.Request.Body = celtypes.FromNative(map[string]any{"items": list})
	benchBool(b, at(expr.PlaceAuthzCELRule),
		`request.body.items.exists(i, request.body.items.exists(j, j == 2))`, v)
}
