// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
)

// syntheticYAML returns n Route-shaped documents in one stream, the shape
// of the PB synthetic ladder.
func syntheticYAML(n int) []byte {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteString("---\n")
		}
		id := strconv.Itoa(i)
		b.WriteString("apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: route-" + id + "\n  labels:\n    team: shop\n" +
			"spec:\n  match:\n    hosts: [shop.example.com]\n    paths:\n      - prefix: /api/v1/items/" + id + "\n    methods: [GET, POST]\n" +
			"  upstreams:\n    - name: items-" + id + "\n      weight: 100\n  policies:\n    - name: ratelimit-global\n    - name: auth-jwt\n" +
			"  timeout: 15s\n  retries:\n    attempts: 2\n    retryOn: \"response.status >= 500\"\n  # traffic notes\n  description: |\n    Items API, shard " + id + ".\n")
	}
	return []byte(b.String())
}

// syntheticJSON returns one Route-shaped JSON document.
func syntheticJSON() []byte {
	return []byte(`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "route-1", "labels": {"team": "shop"}},
 "spec": {"match": {"hosts": ["shop.example.com"], "paths": [{"prefix": "/api/v1/items"}], "methods": ["GET", "POST"]},
  "upstreams": [{"name": "items", "weight": 100}], "policies": [{"name": "ratelimit-global"}, {"name": "auth-jwt"}],
  "timeout": "15s", "retries": {"attempts": 2, "retryOn": "response.status >= 500"}, "description": "Items API.\n"}}`)
}

// BenchmarkParseYAML measures the restricted YAML profile in MB/s and
// allocations per resource (01 test plan, BenchmarkParseYAML).
func BenchmarkParseYAML(b *testing.B) {
	src := syntheticYAML(1000)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	ctx := context.Background()
	for b.Loop() {
		docs, diags, err := Parse(ctx, src, 0, "bench.yaml", FormatYAML, Options{})
		if err != nil || len(diags) != 0 || len(docs) != 1000 {
			b.Fatal(err, codes(diags), len(docs))
		}
	}
	b.ReportMetric(float64(testing.AllocsPerRun(1, func() {
		_, _, _ = Parse(ctx, src, 0, "bench.yaml", FormatYAML, Options{})
	}))/1000, "allocs/resource")
}

// BenchmarkParseYAMLWideMapping measures the split parse of one mapping of
// 20,000 entries (linear, where goccy's own parse is quadratic; 11 req
// 26).
func BenchmarkParseYAMLWideMapping(b *testing.B) {
	var sb strings.Builder
	sb.WriteString("spec:\n  variables:\n")
	for i := range 20000 {
		sb.WriteString("    VAR_" + strconv.Itoa(i) + ": value-" + strconv.Itoa(i) + "\n")
	}
	src := []byte(sb.String())
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		if _, diags, err := Parse(context.Background(), src, 0, "wide.yaml", FormatYAML, Options{}); err != nil || len(diags) != 0 {
			b.Fatal(err, codes(diags))
		}
	}
}

// BenchmarkParseJSON measures the JSON front end (01 req 15; 01 test
// plan, benchmarks).
func BenchmarkParseJSON(b *testing.B) {
	src := syntheticJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		if docs, diags, err := Parse(context.Background(), src, 0, "bench.json", FormatJSON, Options{}); err != nil || len(diags) != 0 || len(docs) != 1 {
			b.Fatal(err, codes(diags))
		}
	}
}

// BenchmarkEncode measures the restricted-profile emitter (02 req 44-47;
// 01 test plan, benchmarks).
func BenchmarkEncode(b *testing.B) {
	docs, diags, err := Parse(context.Background(), syntheticYAML(1000), 0, "bench.yaml", FormatYAML, Options{})
	if err != nil || len(diags) != 0 {
		b.Fatal(err, codes(diags))
	}
	trees := roots(docs)
	var out bytes.Buffer
	if err := Encode(&out, trees); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(out.Len()))
	b.ReportAllocs()
	for b.Loop() {
		out.Reset()
		if err := Encode(&out, trees); err != nil {
			b.Fatal(err)
		}
	}
}
