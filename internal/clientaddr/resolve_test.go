// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

// testTrusted is 10.0.0.0/8 and 2001:db8:ffff::/48.
func testTrusted() *Trusted {
	return New([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8:ffff::/48")})
}

// hdr builds a header from name, value pairs; repeated names add lines in
// order.
func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

// list joins n generated entries with ", ".
func list(n int, f func(i int) string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = f(i)
	}
	return strings.Join(parts, ", ")
}

// TestResolve covers 04 req 17 and 06 reqs 60 and 62 with the cases of the
// 04 (item 11) and 06 ("clientaddr") test plans.
func TestResolve(t *testing.T) {
	const (
		trustedPeer   = "10.0.0.1:443"
		untrustedPeer = "203.0.113.50:5555"
	)
	// 4 untrusted entries left of 16 trusted ones: the untrusted ones are
	// beyond the 16 parsed, so the leftmost parsed (trusted) one wins.
	twenty := list(4, func(i int) string { return fmt.Sprintf("1.1.1.%d", i+1) }) + ", " +
		list(16, func(i int) string { return fmt.Sprintf("10.0.0.%d", i+10) })
	sixteenth := "1.1.1.1, " + list(15, func(i int) string { return fmt.Sprintf("10.0.0.%d", i+10) })
	seventeenth := "1.1.1.1, " + list(16, func(i int) string { return fmt.Sprintf("10.0.0.%d", i+10) })
	fwdTwenty := list(4, func(i int) string { return fmt.Sprintf("for=1.1.1.%d", i+1) }) + ", " +
		list(16, func(i int) string { return fmt.Sprintf("for=10.0.0.%d", i+10) })

	tests := []struct {
		name    string
		peer    string
		h       http.Header
		want    string // source.ip
		port    uint16
		via     Via
		trusted bool
	}{
		// Peer handling.
		{name: "req60 untrusted peer ignores X-Forwarded-For", peer: untrustedPeer, h: hdr("X-Forwarded-For", "1.1.1.1"), want: "203.0.113.50", port: 5555, via: ViaPeer},
		{name: "req60 untrusted peer ignores Forwarded", peer: untrustedPeer, h: hdr("Forwarded", "for=1.1.1.1"), want: "203.0.113.50", port: 5555, via: ViaPeer},
		{name: "req60 trusted peer without headers", peer: trustedPeer, h: hdr(), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req17 IPv4-mapped trusted peer unmapped", peer: "[::ffff:10.0.0.1]:443", h: hdr("X-Forwarded-For", "1.1.1.1"), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},
		{name: "req17 IPv4-mapped untrusted peer unmapped", peer: "[::ffff:203.0.113.50]:80", h: hdr(), want: "203.0.113.50", port: 80, via: ViaPeer},
		{name: "req60 trusted IPv6 peer", peer: "[2001:db8:ffff::5]:443", h: hdr("X-Forwarded-For", "2001:db8::1"), want: "2001:db8::1", via: ViaXForwardedFor, trusted: true},

		// X-Forwarded-For walk.
		{name: "req60 rightmost untrusted X-Forwarded-For entry", peer: trustedPeer, h: hdr("X-Forwarded-For", "1.1.1.1, 10.0.0.2"), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},
		{name: "req60 client-supplied entries left of the client ignored", peer: trustedPeer, h: hdr("X-Forwarded-For", "6.6.6.6, 1.1.1.1, 10.0.0.2"), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},
		{name: "req60 spoofed trusted entry left of the client ignored", peer: trustedPeer, h: hdr("X-Forwarded-For", "10.9.9.9, 1.1.1.1, 10.0.0.2"), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},
		{name: "req17 all trusted gives the leftmost parsed", peer: trustedPeer, h: hdr("X-Forwarded-For", "10.0.0.3, 10.0.0.2"), want: "10.0.0.3", via: ViaXForwardedFor, trusted: true},
		{name: "req60 twenty entries, only 16 parsed", peer: trustedPeer, h: hdr("X-Forwarded-For", twenty), want: "10.0.0.10", via: ViaXForwardedFor, trusted: true},
		{name: "req60 client as the 16th entry", peer: trustedPeer, h: hdr("X-Forwarded-For", sixteenth), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},
		{name: "req17 client as the 17th entry is not parsed", peer: trustedPeer, h: hdr("X-Forwarded-For", seventeenth), want: "10.0.0.10", via: ViaXForwardedFor, trusted: true},
		{name: "req60 unknown entry gives its right neighbor", peer: trustedPeer, h: hdr("X-Forwarded-For", "1.1.1.1, unknown, 10.0.0.2"), want: "10.0.0.2", via: ViaXForwardedFor, trusted: true},
		{name: "req60 unknown rightmost entry gives the peer", peer: trustedPeer, h: hdr("X-Forwarded-For", "1.1.1.1, unknown"), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req17 garbage entry stops the walk", peer: trustedPeer, h: hdr("X-Forwarded-For", "1.1.1.1, not-an-ip, 10.0.0.2, 10.0.0.3"), want: "10.0.0.2", via: ViaXForwardedFor, trusted: true},
		{name: "req60 quoted entry is unparsable", peer: trustedPeer, h: hdr("X-Forwarded-For", `"1.1.1.1"`), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 field lines concatenated in order", peer: trustedPeer, h: hdr("X-Forwarded-For", "1.1.1.1, 10.0.0.9", "X-Forwarded-For", "10.0.0.2"), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},
		{name: "req60 later field line is to the right", peer: trustedPeer, h: hdr("X-Forwarded-For", "6.6.6.6", "X-Forwarded-For", "1.1.1.1, 10.0.0.2"), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},
		{name: "req60 all trusted across lines", peer: trustedPeer, h: hdr("X-Forwarded-For", "10.0.0.7", "X-Forwarded-For", "10.0.0.2"), want: "10.0.0.7", via: ViaXForwardedFor, trusted: true},
		{name: "req60 ports ignored", peer: trustedPeer, h: hdr("X-Forwarded-For", "1.1.1.1:1234, 10.0.0.2:80"), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},
		{name: "req60 IPv6 brackets and port stripped", peer: trustedPeer, h: hdr("X-Forwarded-For", "[2001:db8::1]:443"), want: "2001:db8::1", via: ViaXForwardedFor, trusted: true},
		{name: "req60 IPv6 brackets stripped", peer: trustedPeer, h: hdr("X-Forwarded-For", "[2001:db8::2]"), want: "2001:db8::2", via: ViaXForwardedFor, trusted: true},
		{name: "req60 IPv6 zone stripped", peer: trustedPeer, h: hdr("X-Forwarded-For", "fe80::1%eth0"), want: "fe80::1", via: ViaXForwardedFor, trusted: true},
		{name: "req17 IPv4-mapped entry unmapped", peer: trustedPeer, h: hdr("X-Forwarded-For", "::ffff:1.1.1.1"), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},
		{name: "req17 IPv4-mapped trusted entry skipped", peer: trustedPeer, h: hdr("X-Forwarded-For", "1.1.1.1, [::ffff:10.0.0.2]:80"), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},
		{name: "req60 empty elements skipped", peer: trustedPeer, h: hdr("X-Forwarded-For", "1.1.1.1,, 10.0.0.2 ,"), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},
		{name: "req60 only empty elements give the peer", peer: trustedPeer, h: http.Header{"X-Forwarded-For": {" , ", ""}}, want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 tabs are whitespace", peer: trustedPeer, h: hdr("X-Forwarded-For", "\t1.1.1.1\t,\t10.0.0.2"), want: "1.1.1.1", via: ViaXForwardedFor, trusted: true},

		// Forwarded walk (RFC 7239).
		{name: "req60 Forwarded quoted IPv6 with port", peer: trustedPeer, h: hdr("Forwarded", `for="[2001:db8::1]:4711"`), want: "2001:db8::1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded with other parameters", peer: trustedPeer, h: hdr("Forwarded", "for=192.0.2.60;proto=http;by=203.0.113.43"), want: "192.0.2.60", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded wins over X-Forwarded-For", peer: trustedPeer, h: hdr("Forwarded", "for=1.1.1.1", "X-Forwarded-For", "2.2.2.2"), want: "1.1.1.1", via: ViaForwarded, trusted: true},
		{name: "req60 whitespace-only Forwarded falls back to X-Forwarded-For", peer: trustedPeer, h: http.Header{"Forwarded": {" \t"}, "X-Forwarded-For": {"2.2.2.2"}}, want: "2.2.2.2", via: ViaXForwardedFor, trusted: true},
		{name: "req60 Forwarded rightmost untrusted element", peer: trustedPeer, h: hdr("Forwarded", "for=6.6.6.6, for=1.1.1.1, for=10.0.0.2"), want: "1.1.1.1", via: ViaForwarded, trusted: true},
		{name: "req17 Forwarded all trusted gives the leftmost parsed", peer: trustedPeer, h: hdr("Forwarded", "for=10.0.0.3, for=10.0.0.2"), want: "10.0.0.3", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded twenty elements, only 16 parsed", peer: trustedPeer, h: hdr("Forwarded", fwdTwenty), want: "10.0.0.10", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded parameter names are case-insensitive", peer: trustedPeer, h: hdr("Forwarded", `FOR="1.1.1.1", For=10.0.0.2`), want: "1.1.1.1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded quoted comma does not split", peer: trustedPeer, h: hdr("Forwarded", `for=1.1.1.1;host="a,b", for=10.0.0.2`), want: "1.1.1.1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded escaped quote inside a value", peer: trustedPeer, h: hdr("Forwarded", `for=1.1.1.1;host="a\",b", for=10.0.0.2`), want: "1.1.1.1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded escaped address characters", peer: trustedPeer, h: hdr("Forwarded", `for="1.1.1\.1"`), want: "1.1.1.1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded unknown rightmost gives the peer", peer: trustedPeer, h: hdr("Forwarded", "for=1.1.1.1, for=unknown"), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 Forwarded unknown gives its right neighbor", peer: trustedPeer, h: hdr("Forwarded", "for=1.1.1.1, for=UNKNOWN, for=10.0.0.2"), want: "10.0.0.2", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded obfuscated identifier ends the walk", peer: trustedPeer, h: hdr("Forwarded", `for=1.1.1.1, for="_hidden", for=10.0.0.2`), want: "10.0.0.2", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded element without for ends the walk", peer: trustedPeer, h: hdr("Forwarded", "for=1.1.1.1, proto=https"), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 Forwarded duplicate for is unparsable", peer: trustedPeer, h: hdr("Forwarded", "for=1.1.1.1;for=2.2.2.2"), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 Forwarded unterminated quote is unparsable", peer: trustedPeer, h: hdr("Forwarded", `for=1.1.1.1, for="10.0.0.2`), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 Forwarded unterminated quote resets per line", peer: trustedPeer, h: hdr("Forwarded", `for="1.1.1.1`, "Forwarded", "for=2.2.2.2"), want: "2.2.2.2", via: ViaForwarded, trusted: true},
		// A client's unterminated quoted string never swallows an element a
		// trusted proxy appended to the same line (review finding: the
		// whole line used to become one unparsable element, giving the
		// proxy address).
		{name: "req60 Forwarded proxy element after a client's unterminated quote", peer: trustedPeer, h: hdr("Forwarded", `for="1.1.1.1, for=203.0.113.9`), want: "203.0.113.9", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded proxy element after a client's dangling backslash", peer: trustedPeer, h: hdr("Forwarded", `x="\, for=203.0.113.9`), want: "203.0.113.9", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded quoted proxy element after a client's unterminated quote", peer: trustedPeer, h: hdr("Forwarded", `for="1.1.1.1, for="203.0.113.9:443";host="a,b"`), want: "203.0.113.9", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded proxy element with a quoted-pair after a client's unterminated quote", peer: trustedPeer, h: hdr("Forwarded", `for="1.1.1.1, for=203.0.113.9;host="a\",b"`), want: "203.0.113.9", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded proxy element with an escaped backslash", peer: trustedPeer, h: hdr("Forwarded", `for="1.1.1.1, for=203.0.113.9;host="a\\"`), want: "203.0.113.9", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded trusted hops right of the proxy element", peer: trustedPeer, h: hdr("Forwarded", `for="6.6.6.6, for=203.0.113.9, for=10.0.0.2`), want: "203.0.113.9", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded stray escaped quote leaves its element unparsable", peer: trustedPeer, h: hdr("Forwarded", `for=1.1.1.1, for=10.0.0.2\"`), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 Forwarded unterminated quote at the start of a line", peer: trustedPeer, h: hdr("Forwarded", `"x, for=203.0.113.9`), want: "203.0.113.9", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded pair without a value", peer: trustedPeer, h: hdr("Forwarded", "for=1.1.1.1;secure"), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 Forwarded empty for", peer: trustedPeer, h: hdr("Forwarded", "for="), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 Forwarded bad parameter name", peer: trustedPeer, h: hdr("Forwarded", "f r=1.1.1.1"), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 Forwarded bad token value", peer: trustedPeer, h: hdr("Forwarded", "for=1.1.1.1;host=a@b"), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 Forwarded quote in the middle of a value", peer: trustedPeer, h: hdr("Forwarded", `for=1.1.1.1;host=a"b"`), want: "10.0.0.1", port: 443, via: ViaPeer, trusted: true},
		{name: "req60 Forwarded quoted IPv4 with port", peer: trustedPeer, h: hdr("Forwarded", `for="1.1.1.1:8080"`), want: "1.1.1.1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded obfuscated port", peer: trustedPeer, h: hdr("Forwarded", `for="[2001:db8::1]:_abc"`), want: "2001:db8::1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded unquoted IPv4 with port accepted", peer: trustedPeer, h: hdr("Forwarded", "for=1.1.1.1:8080"), want: "1.1.1.1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded unquoted bracketed IPv6 accepted", peer: trustedPeer, h: hdr("Forwarded", "for=[2001:db8::1]"), want: "2001:db8::1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded bare IPv6 accepted", peer: trustedPeer, h: hdr("Forwarded", `for="2001:db8::1"`), want: "2001:db8::1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded zone stripped", peer: trustedPeer, h: hdr("Forwarded", `for="[fe80::1%eth0]"`), want: "fe80::1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded field lines concatenated in order", peer: trustedPeer, h: hdr("Forwarded", "for=1.1.1.1", "Forwarded", "for=10.0.0.2"), want: "1.1.1.1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded empty elements skipped", peer: trustedPeer, h: hdr("Forwarded", "for=1.1.1.1, , for=10.0.0.2;proto=https"), want: "1.1.1.1", via: ViaForwarded, trusted: true},
		{name: "req60 Forwarded empty pairs skipped", peer: trustedPeer, h: hdr("Forwarded", "for=1.1.1.1;;proto=https;"), want: "1.1.1.1", via: ViaForwarded, trusted: true},

		// Unknown peers.
		{name: "req59 unavailable peer gives no address", peer: "", h: hdr("X-Forwarded-For", "1.1.1.1"), want: "invalid IP", via: ViaPeer},
	}
	tr := testTrusted()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var peer netip.AddrPort
			if tt.peer != "" {
				peer = netip.MustParseAddrPort(tt.peer)
			}
			r := tr.Resolve(peer, tt.h)
			if r.IP.String() != tt.want || r.Port != tt.port || r.Via != tt.via || r.PeerTrusted != tt.trusted {
				t.Errorf("Resolve = {IP:%v Port:%d Via:%v PeerTrusted:%v}, want {%s %d %v %v}",
					r.IP, r.Port, r.Via, r.PeerTrusted, tt.want, tt.port, tt.via, tt.trusted)
			}
			if r.FromHeader() != (tt.via != ViaPeer) {
				t.Errorf("FromHeader = %v with Via %v", r.FromHeader(), r.Via)
			}
			if r.IP.Is4In6() || r.IP.Zone() != "" || r.Peer.Addr().Is4In6() {
				t.Errorf("Resolve returned a mapped or zoned address: %+v", r)
			}
		})
	}
}

// TestResolveNilTrusted: no trustedProxies means the peer is always the
// client.
func TestResolveNilTrusted(t *testing.T) {
	var tr *Trusted
	r := tr.Resolve(netip.MustParseAddrPort("10.0.0.1:1"), hdr("X-Forwarded-For", "1.1.1.1"))
	if r.IP != netip.MustParseAddr("10.0.0.1") || r.Port != 1 || r.FromHeader() || r.PeerTrusted {
		t.Errorf("Resolve = %+v", r)
	}
}

func TestViaString(t *testing.T) {
	for v, want := range map[Via]string{ViaPeer: "peer", ViaForwarded: "forwarded", ViaXForwardedFor: "x-forwarded-for", Via(9): "Via(9)"} {
		if got := v.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", v, got, want)
		}
	}
}

// TestResolveAllocations: source.ip is computed on every request without
// allocating (06 6.7 BenchmarkClientAddr budget).
func TestResolveAllocations(t *testing.T) {
	tr := testTrusted()
	peer := netip.MustParseAddrPort("10.0.0.1:443")
	for name, h := range map[string]http.Header{
		"x-forwarded-for 16": hdr("X-Forwarded-For", "1.1.1.1, "+list(15, func(i int) string { return fmt.Sprintf("10.0.0.%d", i+2) })),
		"forwarded 16":       hdr("Forwarded", "for=1.1.1.1, "+list(15, func(i int) string { return fmt.Sprintf(`for="10.0.0.%d:80";proto=https`, i+2) })),
		"forwarded 40":       hdr("Forwarded", list(40, func(i int) string { return fmt.Sprintf("for=10.0.%d.1", i) })),
		"ipv6 brackets":      hdr("X-Forwarded-For", "[2001:db8::1]:443, [2001:db8:ffff::2]:80"),
		"untrusted peer":     hdr("X-Forwarded-For", "1.1.1.1"),
		"unterminated quote": hdr("Forwarded", `for="1.1.1.1, for=203.0.113.9`),
		"unknown":            hdr("X-Forwarded-For", "1.1.1.1, unknown, 10.0.0.2"),
	} {
		if n := testing.AllocsPerRun(100, func() { _ = tr.Resolve(peer, h) }); n != 0 {
			t.Errorf("%s: %v allocations per Resolve, want 0", name, n)
		}
	}
}

// TestResolveAllocationBound pins the documented exceptions to the
// zero-allocation rule: one allocation per backslash-escaped Forwarded for
// value, and one for the entry netip.ParseAddr rejects, which ends the
// walk, so a client can never make Resolve allocate more than MaxEntries+1
// times.
func TestResolveAllocationBound(t *testing.T) {
	tr := testTrusted()
	peer := netip.MustParseAddrPort("10.0.0.1:443")
	escaped := list(40, func(i int) string { return fmt.Sprintf(`for="10.0.%d\.1"`, i) })
	tests := []struct {
		name string
		h    http.Header
		max  float64
	}{
		{"escaped for value", hdr("Forwarded", `for="1.1.1\.1"`), 1},
		{"escaped values beyond the 16 examined", hdr("Forwarded", escaped), MaxEntries},
		{"rejected hex entry", hdr("X-Forwarded-For", "1.1.1.1, deadbeef, 10.0.0.2"), 1},
		{"rejected dotted entry", hdr("X-Forwarded-For", "1.1.1.1.1"), 1},
		{"rejected colon entry", hdr("Forwarded", `for="::::"`), 1},
		{"escaped and rejected", hdr("Forwarded", `for="dead\beef"`), 2},
		{"16 rejected entries", hdr("X-Forwarded-For", list(16, func(int) string { return "deadbeef" })), 1},
	}
	for _, tt := range tests {
		if n := testing.AllocsPerRun(100, func() { _ = tr.Resolve(peer, tt.h) }); n > tt.max {
			t.Errorf("%s: %v allocations per Resolve, want at most %v", tt.name, n, tt.max)
		}
	}
}

func TestParseXFFEntry(t *testing.T) {
	tests := []struct {
		in, want string // want "" means unparsable
	}{
		{"1.2.3.4", "1.2.3.4"},
		{"1.2.3.4:80", "1.2.3.4"},
		{"1.2.3.4:65535", "1.2.3.4"},
		{"1.2.3.4:65536", ""},
		{"1.2.3.4:", ""},
		{"1.2.3.4:8a", ""},
		{"1.2.3.4:123456", ""},
		{"1.2.3.4:80:90", ""},
		{"2001:db8::1", "2001:db8::1"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"[2001:db8::1]:80", "2001:db8::1"},
		{"[2001:db8::1]80", ""},
		{"[2001:db8::1", ""},
		{"[1.2.3.4]", ""},
		{"[::ffff:1.2.3.4]:1", "1.2.3.4"},
		{"fe80::1%25eth0", "fe80::1"},
		{"host.example:80", ""},
		{"unknown", ""},
		{"_hidden", ""},
		{"", ""},
	}
	for _, tt := range tests {
		a, ok := parseXFFEntry(tt.in)
		got := ""
		if ok {
			got = a.String()
		}
		if got != tt.want {
			t.Errorf("parseXFFEntry(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseNode(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"192.0.2.43", "192.0.2.43"},
		{"192.0.2.43:47011", "192.0.2.43"},
		{"192.0.2.43:_port", "192.0.2.43"},
		{"192.0.2.43:_", ""},
		{"192.0.2.43:_a b", ""},
		{"[2001:db8:cafe::17]", "2001:db8:cafe::17"},
		{"[2001:db8:cafe::17]:4711", "2001:db8:cafe::17"},
		{"[2001:db8:cafe::17]:_x.y-z", "2001:db8:cafe::17"},
		{"[2001:db8:cafe::17]:", ""},
		{"2001:db8:cafe::17", "2001:db8:cafe::17"},
		{"unknown", ""},
		{"Unknown", ""},
		{"_gazonk", ""},
		{"", ""},
		{"example.com", ""},
		{"example.com:80", ""},
	}
	for _, tt := range tests {
		a, ok := parseNode(tt.in)
		got := ""
		if ok {
			got = a.String()
		}
		if got != tt.want {
			t.Errorf("parseNode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
