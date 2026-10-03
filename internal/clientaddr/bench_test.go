// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"bytes"
	"fmt"
	"net/http"
	"net/netip"
	"testing"
)

// BenchmarkClientAddr is the 06 6.7 benchmark: source.ip with 16
// X-Forwarded-For entries behind a trusted peer (the full walk), plus the
// Forwarded form and the common short cases. Every case allocates nothing.
func BenchmarkClientAddr(b *testing.B) {
	tr := testTrusted()
	trusted := netip.MustParseAddrPort("10.0.0.1:443")
	cases := []struct {
		name string
		peer netip.AddrPort
		h    http.Header
	}{
		{"xff-16", trusted, hdr("X-Forwarded-For", "1.1.1.1, "+list(15, func(i int) string { return fmt.Sprintf("10.0.0.%d", i+2) }))},
		{"xff-16-ipv6", trusted, hdr("X-Forwarded-For", "[2001:db8::1]:1, "+list(15, func(i int) string { return fmt.Sprintf("[2001:db8:ffff::%x]:80", i+2) }))},
		{"forwarded-16", trusted, hdr("Forwarded", "for=1.1.1.1, "+list(15, func(i int) string { return fmt.Sprintf(`for="10.0.0.%d:80";proto=https`, i+2) }))},
		{"xff-1", trusted, hdr("X-Forwarded-For", "1.1.1.1")},
		{"trusted-no-header", trusted, hdr()},
		{"untrusted-peer", netip.MustParseAddrPort("203.0.113.50:5555"), hdr("X-Forwarded-For", "1.1.1.1")},
	}
	for _, bc := range cases {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = tr.Resolve(bc.peer, bc.h)
			}
		})
	}
}

// BenchmarkReadHeader measures the PROXY v2 reader per connection.
func BenchmarkReadHeader(b *testing.B) {
	for _, bc := range []struct {
		name string
		in   []byte
	}{
		{"tcp4", proxyTCP4("203.0.113.7:51000", "192.0.2.1:443", nil)},
		{"tcp6-tlvs", proxyTCP6("[2001:db8::7]:4711", "[2001:db8::1]:443", append(tlv(0x01, []byte("h2")), tlv(0x04, make([]byte, 200))...))},
		{"local", local()},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(bc.in)))
			r := bytes.NewReader(bc.in)
			for b.Loop() {
				r.Reset(bc.in)
				if _, err := ReadHeader(r); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRewrite measures the forwarding-header rewrite per leg attempt.
func BenchmarkRewrite(b *testing.B) {
	for _, bc := range []struct {
		name string
		f    Forwarding
	}{
		{"untrusted", Forwarding{Peer: netip.MustParseAddr("203.0.113.50"), Scheme: "https", Host: "api.shop.example"}},
		{"trusted", Forwarding{Peer: netip.MustParseAddr("10.0.0.1"), PeerTrusted: true, Scheme: "https", Host: "api.shop.example"}},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.ReportAllocs()
			base := http.Header{
				"Accept":            {"*/*"},
				"User-Agent":        {"bench"},
				"X-Forwarded-For":   {"1.1.1.1"},
				"X-Forwarded-Proto": {"https"},
			}
			h := base.Clone()
			for b.Loop() {
				h["X-Forwarded-For"] = base["X-Forwarded-For"]
				Rewrite(h, bc.f)
			}
		})
	}
}

// BenchmarkContains measures a trusted-set lookup over 1,000 prefixes.
func BenchmarkContains(b *testing.B) {
	ps := make([]netip.Prefix, 0, 1000)
	for i := range 1000 {
		ps = append(ps, netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(i >> 8), byte(i), 0, 0}), 24))
	}
	tr := New(ps)
	a := netip.MustParseAddr("3.231.0.9")
	b.ReportAllocs()
	for b.Loop() {
		_ = tr.Contains(a)
	}
}
