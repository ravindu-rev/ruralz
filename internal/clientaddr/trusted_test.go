// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"errors"
	"net/netip"
	"slices"
	"testing"
)

// TestParsePrefix covers the trustedProxies entry forms (CFG "Gateway":
// a set of CIDRs) and the IPv4-mapped rule shared with 06 req 58.
func TestParsePrefix(t *testing.T) {
	tests := []struct {
		in, want string
		err      bool
	}{
		{in: "10.0.0.0/8", want: "10.0.0.0/8"},
		{in: "10.1.2.3/8", want: "10.0.0.0/8"},
		{in: "192.0.2.7", want: "192.0.2.7/32"},
		{in: "2001:db8::/32", want: "2001:db8::/32"},
		{in: "2001:db8::1", want: "2001:db8::1/128"},
		{in: "::ffff:10.0.0.0/104", want: "10.0.0.0/8"},
		{in: "::ffff:192.0.2.7", want: "192.0.2.7/32"},
		{in: "::ffff:0.0.0.0/96", want: "0.0.0.0/0"},
		{in: "::/80", want: "::/80"},
		{in: "0.0.0.0/0", want: "0.0.0.0/0"},
		{in: "fe80::1%eth0", err: true},
		{in: "fe80::/10%eth0", err: true},
		{in: "10.0.0.0/33", err: true},
		{in: "not-an-address", err: true},
		{in: "10.0.0.0/x", err: true},
		{in: "", err: true},
	}
	for _, tt := range tests {
		got, err := ParsePrefix(tt.in)
		if tt.err {
			if !errors.Is(err, ErrPrefix) {
				t.Errorf("ParsePrefix(%q) = %v, %v; want ErrPrefix", tt.in, got, err)
			}
			continue
		}
		if err != nil || got.String() != tt.want {
			t.Errorf("ParsePrefix(%q) = %v, %v; want %s", tt.in, got, err, tt.want)
		}
	}
}

func TestParse(t *testing.T) {
	tr, err := Parse([]string{"10.0.0.0/8", "10.1.0.0/16", "10.0.0.0/8", "2001:db8::/32", "::ffff:172.16.0.0/108"})
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("10.1.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("2001:db8::/32"),
	}
	if got := tr.Prefixes(); !slices.Equal(got, want) {
		t.Errorf("Prefixes = %v, want %v", got, want)
	}
	if _, err := Parse([]string{"10.0.0.0/8", "bogus"}); !errors.Is(err, ErrPrefix) {
		t.Errorf("Parse with a bad entry = %v", err)
	}
	empty, err := Parse(nil)
	if err != nil || empty.Contains(netip.MustParseAddr("10.0.0.1")) {
		t.Errorf("Parse(nil) = %v, %v; want a set trusting nothing", empty, err)
	}
	var nilSet *Trusted
	if nilSet.Prefixes() != nil || nilSet.Contains(netip.MustParseAddr("10.0.0.1")) {
		t.Error("nil *Trusted must trust nothing")
	}
}

// TestContains covers family separation, unmapping, zones and prefix
// boundaries (04 req 17: IPv4-mapped IPv6 is unmapped).
func TestContains(t *testing.T) {
	tr := New([]netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("192.0.2.128/25"),
		netip.MustParsePrefix("198.51.100.7/32"),
		netip.MustParsePrefix("2001:db8:1::/48"),
		netip.MustParsePrefix("::/80"),
		{}, // invalid entries are ignored
	})
	tests := []struct {
		addr string
		want bool
	}{
		{"10.0.0.0", true},
		{"10.255.255.255", true},
		{"11.0.0.0", false},
		{"9.255.255.255", false},
		{"::ffff:10.1.2.3", true},
		{"192.0.2.127", false},
		{"192.0.2.128", true},
		{"192.0.2.255", true},
		{"198.51.100.7", true},
		{"198.51.100.6", false},
		{"2001:db8:1::1", true},
		{"2001:db8:1:ffff:ffff:ffff:ffff:ffff", true},
		{"2001:db8:2::1", false},
		{"fe80::1%eth0", false},
		{"::1", true},             // inside ::/80
		{"0.0.0.1", false},        // IPv6 prefixes never match IPv4
		{"::ffff:0.0.0.1", false}, // unmapped first, so ::/80 does not apply
	}
	for _, tt := range tests {
		if got := tr.Contains(netip.MustParseAddr(tt.addr)); got != tt.want {
			t.Errorf("Contains(%s) = %v, want %v", tt.addr, got, tt.want)
		}
	}
	if tr.Contains(netip.Addr{}) {
		t.Error("the invalid address is trusted")
	}
	all := New([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")})
	if !all.Contains(netip.MustParseAddr("203.0.113.1")) || all.Contains(netip.MustParseAddr("2001:db8::1")) {
		t.Error("0.0.0.0/0 must cover every IPv4 address and no IPv6 address")
	}
	zoned := New([]netip.Prefix{netip.PrefixFrom(netip.MustParseAddr("fe80::1%eth0"), 64)})
	if !zoned.Contains(netip.MustParseAddr("fe80::2%eth1")) {
		t.Error("zones are dropped from prefixes and addresses")
	}
}

// TestContainsMatchesLinearScan checks the trie against a linear scan
// over random prefix sets, in both insertion orders (06 req 59: the trie is
// O(address bits)).
func TestContainsMatchesLinearScan(t *testing.T) {
	rng := testRNG(15, 60)
	randAddr := func(v4 bool) netip.Addr {
		if v4 {
			return netip.AddrFrom4([4]byte{10, randByte(rng, 0, 4), randByte(rng, 0, 256), randByte(rng, 0, 256)})
		}
		var b [16]byte
		b[0], b[1], b[2] = 0x20, 0x01, randByte(rng, 0, 4)
		for i := 3; i < 16; i++ {
			b[i] = randByte(rng, 0, 256)
		}
		return netip.AddrFrom16(b)
	}
	for round := range 200 {
		var ps []netip.Prefix
		for range rng.IntN(12) {
			v4 := rng.IntN(2) == 0
			a := randAddr(v4)
			ps = append(ps, netip.PrefixFrom(a, rng.IntN(a.BitLen()+1)).Masked())
		}
		fwd := New(ps)
		back := slices.Clone(ps)
		slices.Reverse(back)
		rev := New(back)
		for range 200 {
			a := randAddr(rng.IntN(2) == 0)
			want := false
			for _, p := range ps {
				want = want || p.Contains(a)
			}
			if got := fwd.Contains(a); got != want {
				t.Fatalf("round %d: Contains(%v) = %v, linear scan %v over %v", round, a, got, want, ps)
			}
			if got := rev.Contains(a); got != want {
				t.Fatalf("round %d: reversed Contains(%v) = %v, linear scan %v over %v", round, a, got, want, ps)
			}
		}
	}
}

// TestTrieInsertCoverage: inserting a covering prefix after a longer one
// still matches everything the covering prefix holds.
func TestTrieInsertCoverage(t *testing.T) {
	var tr trie
	a := [4]byte{10, 1, 0, 0}
	tr.insert(a[:], 16)
	b := [4]byte{10, 0, 0, 0}
	tr.insert(b[:], 8)
	c := [4]byte{10, 2, 3, 4}
	if !tr.contains(c[:]) {
		t.Error("10.0.0.0/8 inserted after 10.1.0.0/16 must cover 10.2.3.4")
	}
	var empty trie
	if empty.contains(c[:]) {
		t.Error("an empty trie matches nothing")
	}
}
