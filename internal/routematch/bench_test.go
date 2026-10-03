// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"strconv"
	"testing"
)

// The benchmarks report allocations: the request-path primitives allocate
// nothing on already-normal input (04 req 33; WP-05 "Done when").

func BenchmarkNormalizePath(b *testing.B) {
	for _, bc := range []struct{ name, path string }{
		{"normal", "/v1/orders/42/items/7"},
		{"normal_escapes", "/v1/files/a%20b%2Fc.txt"},
		{"rewrite", "/v1/%6Frders/./42/../43"},
		{"reject", "/v1/%00"},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = NormalizePath(bc.path)
			}
		})
	}
}

func BenchmarkNormalizeHost(b *testing.B) {
	for _, bc := range []struct{ name, host string }{
		{"normal", "api.shop.example"},
		{"port", "api.shop.example:8443"},
		{"upper", "API.Shop.Example:8443"},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = NormalizeHost(bc.host)
			}
		})
	}
}

func BenchmarkHostTableLookup(b *testing.B) {
	var tab HostTable[int]
	for i := range 1000 {
		*tab.Entry(HostPattern{Exact: "h" + strconv.Itoa(i) + ".shop.example"}) = i
		*tab.Entry(HostPattern{Suffix: ".t" + strconv.Itoa(i) + ".shop.example"}) = i
	}
	*tab.Entry(HostPattern{Suffix: ".shop.example"}) = -1
	*tab.Entry(HostPattern{}) = -2
	n := 0
	yield := func(HostPattern, *int) bool { n++; return true }
	b.ReportAllocs()
	for b.Loop() {
		tab.Lookup("eu.t500.shop.example", yield)
	}
}

func BenchmarkTrieLookup(b *testing.B) {
	var tr Trie[int]
	for i := range 1000 {
		s := strconv.Itoa(i)
		tr.InsertExact("/v1/r"+s+"/export", i)
		tp, err := ParseTemplate("/v1/r" + s + "/{id}/items/{item}")
		if err != nil {
			b.Fatal(err)
		}
		tr.InsertTemplate(tp, i)
	}
	tp, _ := ParseTemplate("/{a}/{b}/{c}/{d}/{e}")
	tr.InsertTemplate(tp, -1)
	buf := make([]string, 0, 8)
	n := 0
	yield := func(int, []string) bool { n++; return true }
	b.ReportAllocs()
	for b.Loop() {
		tr.Lookup("/v1/r500/42/items/7", buf, yield)
	}
}

func BenchmarkTemplateMatch(b *testing.B) {
	tp, err := ParseTemplate("/v1/orders/{orderId}/items/{item}")
	if err != nil {
		b.Fatal(err)
	}
	buf := make([]string, 0, 4)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = tp.Match("/v1/orders/42/items/7", buf)
	}
}

func BenchmarkPrefixMatch(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = PrefixMatch("/v1/orders", "/v1/orders/42/items/7")
	}
}
