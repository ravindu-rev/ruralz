// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// Microbenchmarks of docs/engineering/03-testing-and-quality-strategy.md
// "Benchmarks": typical match and key expressions run under 2 µs at p99
// (target, docs/architecture/02-configuration-model.md "Limits"), and the
// worst-case fixtures of ADR-0019 end within 50 ms, finished or stopped.
// Every program is planned under exampleCaps, the input caps a default
// Gateway's snapshot passes, so each runs tracked or untracked as it does
// in the data plane.

// snapshotVars returns testVars with its Route and Consumer prepared, as
// the snapshot compiler prepares them before any evaluation.
func snapshotVars() *expr.Vars {
	v := testVars()
	celtypes.PrepareRoute(v.Route)
	celtypes.PrepareConsumer(v.Consumer)
	return v
}

// benchBool reports the evaluation of a bool program.
func benchBool(b *testing.B, site expr.Site, src string, v *expr.Vars) {
	benchBoolCtx(context.Background(), b, site, src, v)
}

// benchBoolCtx reports the evaluation of a bool program under ctx.
func benchBoolCtx(ctx context.Context, b *testing.B, site expr.Site, src string, v *expr.Vars) {
	prg := mustCapped(b, newCompiler(b), site, src, exampleCaps())
	b.ReportAllocs()
	for b.Loop() {
		_, _ = prg.EvalBool(ctx, v)
	}
}

// benchString reports the evaluation of a string program.
func benchString(b *testing.B, site expr.Site, src string, v *expr.Vars) {
	prg := mustCapped(b, newCompiler(b), site, src, exampleCaps())
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = prg.EvalString(ctx, v)
	}
}

func BenchmarkMatchWhen(b *testing.B) {
	benchBool(b, at(expr.PlaceRouteMatchWhen), `request.headers["x-tenant"] == "acme"`, snapshotVars())
}

func BenchmarkRateLimitKey(b *testing.B) {
	benchString(b, at(expr.PlaceRateLimitKey), `consumer == null ? source.ip : consumer.name`, snapshotVars())
}

// The authz.cel rules of the documents and the field kinds such rules
// read (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Per-stage latency budget": authz.cel, 2 µs at p50, 0 allocations).

func BenchmarkAuthzGold(b *testing.B) {
	benchBool(b, at(expr.PlaceAuthzCELRule), `consumer != null && consumer.tier == "gold"`, snapshotVars())
}

func BenchmarkAuthzQuota(b *testing.B) {
	benchBool(b, at(expr.PlaceAuthzCELRule), `consumer != null && "daily-tokens" in consumer.quotas`, snapshotVars())
}

func BenchmarkAuthzRouteLabel(b *testing.B) {
	benchBool(b, at(expr.PlaceAuthzCELRule), `route.labels.team == "core" && route.name == "orders"`, snapshotVars())
}

func BenchmarkAuthzMethod(b *testing.B) {
	benchBool(b, at(expr.PlaceAuthzCELRule), `request.method in ["GET", "HEAD"] && request.scheme == "https"`, snapshotVars())
}

func BenchmarkAuthzOrders(b *testing.B) {
	benchBool(b, at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read")`, snapshotVars())
}

// BenchmarkAuthzOrdersDeadline is BenchmarkAuthzOrders under a request
// context with a deadline, as the data plane evaluates it.
func BenchmarkAuthzOrdersDeadline(b *testing.B) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	benchBoolCtx(ctx, b, at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read")`, snapshotVars())
}

// mapClaims is a JWT payload as auth.jwt holds it: a map that CEL reads in
// place through expr.Tree, and that Native copies.
type mapClaims map[string]any

func (mapClaims) IsNull() bool { return false }

func (c mapClaims) AppendBody(dst []byte) ([]byte, error) {
	return jsonval.Append(dst, map[string]any(c))
}

func (c mapClaims) Native() (any, error) { return jsonval.Clone(map[string]any(c)), nil }

func (c mapClaims) Tree() any { return map[string]any(c) }

// BenchmarkAuthzOrdersJWTClaims is BenchmarkAuthzOrders over a verified
// token's claims as auth.jwt sets them, eight registered and custom
// members, under a request deadline.
func BenchmarkAuthzOrdersJWTClaims(b *testing.B) {
	v := snapshotVars()
	v.Auth = &expr.Auth{Method: "jwt", Claims: mapClaims{
		"iss": "https://auth.shop.example", "sub": "u-7", "aud": []any{"shop-api"},
		"exp": json.Number("1793318400"), "iat": json.Number("1793314800"), "jti": "8f1c",
		"azp": "portal", "scope": "orders:read orders:write",
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	benchBoolCtx(ctx, b, at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read")`, v)
}

func BenchmarkAccessLogWhen(b *testing.B) {
	benchBool(b, at(expr.PlaceAccessLogWhen), `response.status >= 400 || duration > duration("1s")`, snapshotVars())
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
		if prg, iss := c.program(at(expr.PlaceAuthzCELRule), src, expr.InputCaps{}); prg == nil {
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

// BenchmarkWorstCaseUntracked runs the authz-orders rule over a scope at
// the 4 × 16 KiB claims cap, 32,763 entries: the bound under the caps keeps
// it within the cost limit, so it runs untracked and without a stop, and
// its time is the most a comprehension without a stop can take on such a
// scan.
func BenchmarkWorstCaseUntracked(b *testing.B) {
	v := snapshotVars()
	v.Auth.Claims = celtypes.FromNative(map[string]any{"scope": strings.Repeat("x ", 32<<10-6) + "orders:read"})
	benchBool(b, at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read")`, v)
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
