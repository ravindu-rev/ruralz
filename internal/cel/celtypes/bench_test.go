// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"fmt"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Benchmarks of the adapters (03 I requirements 50 and 52): typical field,
// header and body selections, worst-case fixtures at the Gateway caps (a 64
// KiB header value, 128 headers), and the transform body writers on 1 KiB.
// Each reports ns/op and allocs/op.

// benchEval benchmarks one expression over v.
func benchEval(b *testing.B, src string, v *expr.Vars) {
	b.Helper()
	prg := compile(b, testEnv(b), src)
	a := NewActivation(v)
	if _, _, err := prg.Eval(a); err != nil {
		b.Fatalf("%s: %v", src, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _, _ = prg.Eval(a)
	}
}

func BenchmarkResolveRequest(b *testing.B) {
	a := NewActivation(fullVars())
	b.ReportAllocs()
	for b.Loop() {
		_, _ = a.ResolveName("request")
	}
}

func BenchmarkEvalMethod(b *testing.B) {
	benchEval(b, `request.method == "GET"`, fullVars())
}

func BenchmarkEvalHeaderLookup(b *testing.B) {
	benchEval(b, `request.headers["x-tenant"] == "acme"`, fullVars())
}

func BenchmarkEvalHeaderPresence(b *testing.B) {
	benchEval(b, `"x-api-key" in request.headers`, fullVars())
}

func BenchmarkEvalRateLimitKey(b *testing.B) {
	benchEval(b, `consumer == null ? source.ip : consumer.name`, fullVars())
}

func BenchmarkEvalGold(b *testing.B) {
	benchEval(b, `consumer != null && consumer.tier == "gold"`, fullVars())
}

func BenchmarkEvalBodySelect(b *testing.B) {
	benchEval(b, `request.body.items[1] == "y"`, fullVars())
}

func BenchmarkEvalQuery(b *testing.B) {
	benchEval(b, `request.query.tag == "a,b"`, fullVars())
}

func BenchmarkEvalDefaultRetryOn(b *testing.B) {
	benchEval(b, expr.DefaultRetryOn, fullVars())
}

// capVars has 128 headers, one of them a 64 KiB comma list (the
// maxRequestHeaderBytes cap).
func capVars() *expr.Vars {
	v := fullVars()
	for i := range 127 {
		v.Request.Header.Set(fmt.Sprintf("X-Filler-%03d", i), "v")
	}
	var sb strings.Builder
	for sb.Len() < 64<<10-3 {
		sb.WriteString("ab,")
	}
	v.Request.Header.Set("X-List", sb.String())
	return v
}

func BenchmarkEvalHeaderLookupAtCap(b *testing.B) {
	benchEval(b, `request.headers["x-list"].size() > 0`, capVars())
}

func BenchmarkEvalHeaderIterateAtCap(b *testing.B) {
	benchEval(b, `request.headers.exists(k, k == "x-zzz")`, capVars())
}

func BenchmarkEvalHeaderSplitAtCap(b *testing.B) {
	benchEval(b, `request.headers["x-list"].split(",").exists(s, s == "zz")`, capVars())
}

func BenchmarkJoinedHeader(b *testing.B) {
	h := capVars().Request.Header
	b.ReportAllocs()
	for b.Loop() {
		_, _ = joinedHeader(h, "x-tenant")
	}
}

// body1KiB is a decoded JSON document of about 1 KiB.
func body1KiB() map[string]any {
	items := make([]any, 0, 16)
	for i := range 16 {
		items = append(items, map[string]any{
			"id": jsonNum(fmt.Sprint(1000 + i)), "sku": fmt.Sprintf("SKU-%04d", i),
			"price": jsonNum("12.50"), "tags": []any{"a", "b"},
		})
	}
	return map[string]any{"order": "o-1", "customer": map[string]any{"id": "c-9", "tier": "gold"}, "items": items}
}

func BenchmarkAppendBody1KiB(b *testing.B) {
	v := FromNative(body1KiB())
	buf, _ := v.AppendBody(nil)
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	for b.Loop() {
		buf, _ = v.AppendBody(buf[:0])
	}
}

func BenchmarkNative1KiB(b *testing.B) {
	v := FromNative(body1KiB())
	b.ReportAllocs()
	for b.Loop() {
		_, _ = v.Native()
	}
}

func BenchmarkAppendBodyResult1KiB(b *testing.B) {
	env := testEnv(b)
	vars := fullVars()
	vars.Response.Body = FromNative(body1KiB())
	prg := compile(b, env, `{"order": response.body.order, "count": response.body.items.size(), "items": response.body.items.map(i, {"id": i.id, "price": i.price})}`)
	out, _, err := prg.Eval(NewActivation(vars))
	if err != nil {
		b.Fatal(err)
	}
	v := FromVal(out)
	buf, _ := v.AppendBody(nil)
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	for b.Loop() {
		buf, _ = v.AppendBody(buf[:0])
	}
}

// BenchmarkNewProvider measures the per-process cost of the declarations.
func BenchmarkNewProvider(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := NewProvider(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEnvWithProvider measures environment construction with the
// Provider and every variable.
func BenchmarkEnvWithProvider(b *testing.B) {
	p, err := NewProvider()
	if err != nil {
		b.Fatal(err)
	}
	opts := []cel.EnvOption{cel.CustomTypeProvider(p), cel.CustomTypeAdapter(p)}
	for _, v := range Variables(allVars) {
		opts = append(opts, cel.Variable(v.Name, v.Type))
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := cel.NewEnv(opts...); err != nil {
			b.Fatal(err)
		}
	}
}

// TestCapFixture keeps capVars within the header cap it models.
func TestCapFixture(t *testing.T) {
	n := 0
	for k, vs := range capVars().Request.Header {
		n += len(k)
		for _, v := range vs {
			n += len(v)
		}
	}
	if n > 64<<10+4<<10 {
		t.Errorf("fixture has %d header bytes", n)
	}
}
