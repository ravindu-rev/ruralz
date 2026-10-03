// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"fmt"
	"net"
	"testing"
)

// BenchmarkSelectSRV measures applying 05 req 6 (lowest-priority group,
// weight-0 rule, trailing-dot trim, merge and sort) to a 64-record SRV
// answer spread over four priorities.
func BenchmarkSelectSRV(b *testing.B) {
	recs := make([]*net.SRV, 64)
	for i := range recs {
		recs[i] = &net.SRV{
			Target:   fmt.Sprintf("t%02d.orders.svc.", 63-i),
			Port:     8080,
			Priority: uint16(i % 4),
			Weight:   uint16(i % 3),
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if got := selectSRV(recs); len(got) == 0 {
			b.Fatal("no target")
		}
	}
}

// BenchmarkRefreshDNS measures one A/AAAA refresh of a 32-address answer
// through a fake resolver: lookup, normalization (05 req 8), comparison
// with the current set and copy-on-write publication.
func BenchmarkRefreshDNS(b *testing.B) {
	res := newFakeResolver()
	ips := make([]string, 32)
	for i := range ips {
		ips[i] = fmt.Sprintf("10.0.%d.%d", i/8, i%8+1)
	}
	res.setHost("orders.svc", ips...)
	src, err := New(Spec{DNS: &DNSSpec{Service: "orders.svc", Port: 8080}}, Options{
		Upstream: "orders", Resolver: res, Rand: newRand(1),
	})
	if err != nil {
		b.Fatal(err)
	}
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		if set, _ := src.Refresh(ctx); set.Len() != len(ips) {
			b.Fatalf("%d Endpoints", set.Len())
		}
	}
}

// BenchmarkCurrent measures the request-path read of the published set.
func BenchmarkCurrent(b *testing.B) {
	src, err := New(Spec{Static: []StaticEndpoint{{Address: "10.0.0.1:80", Weight: 1}}}, Options{Upstream: "orders"})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if src.Current().Len() != 1 {
				b.Error("empty set")
				return
			}
		}
	})
}
