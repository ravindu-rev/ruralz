// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package routematch

import (
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// TestNormalizeHost covers 04 req 24 (test plan item 5).
func TestNormalizeHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"API.Shop.Example:8443", "api.shop.example"},
		{"[::1]:8080", "[::1]"},
		{"[::1]", "[::1]"},
		{"[2001:DB8::A]:443", "[2001:db8::a]"},
		{"[2001:db8:0::1]", "[2001:db8::1]"},
		{"[2001:0DB8:0000:0000:0000:0000:0000:0001]:8443", "[2001:db8::1]"},
		{"[0:0::1]", "[::1]"},
		{"[::FFFF:10.0.0.1]:80", "[::ffff:10.0.0.1]"},
		{"[::ffff:a00:1]", "[::ffff:10.0.0.1]"},
		{"[FE80::1%25ETH0]", "[fe80::1%25eth0]"},
		{"[10.0.0.1]", "[10.0.0.1]"},
		{"[ZZ]:80", "[zz]"},
		{"shop.example.", "shop.example"},
		{"shop.example.:80", "shop.example"},
		{"shop.example..", "shop.example."},
		{"shop.example:", "shop.example"},
		{"10.0.0.1:8080", "10.0.0.1"},
		{"::1", "::1"},
		{"", ""},
		{"[::1", "[::1"},
		{"api.shop.example", "api.shop.example"},
	}
	for _, tc := range cases {
		if got := NormalizeHost(tc.in); got != tc.want {
			t.Errorf("NormalizeHost(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
	for _, h := range []string{"api.shop.example", "api.shop.example:8443", "[::1]:8080", "[2001:db8::1]", "[::ffff:10.0.0.1]:443"} {
		if allocs := testing.AllocsPerRun(100, func() { _ = NormalizeHost(h) }); allocs != 0 {
			t.Errorf("NormalizeHost(%q): %v allocations", h, allocs)
		}
	}
}

// TestParseHostPattern covers the match.hosts rules: one leading "*." label
// and no other "*" (04 req 27, OQ-data-plane-2 (a), RZ-CFG-005).
func TestParseHostPattern(t *testing.T) {
	valid := []struct {
		in   string
		want HostPattern
	}{
		{"api.shop.example", HostPattern{Exact: "api.shop.example"}},
		{"API.Shop.Example.", HostPattern{Exact: "api.shop.example"}},
		{"*.shop.example", HostPattern{Suffix: ".shop.example"}},
		{"*.Shop.Example", HostPattern{Suffix: ".shop.example"}},
		{"*.example", HostPattern{Suffix: ".example"}},
		{"[::1]", HostPattern{Exact: "[::1]"}},
		{"[2001:DB8::1]", HostPattern{Exact: "[2001:db8::1]"}},
		{"[2001:db8:0::1]", HostPattern{Exact: "[2001:db8::1]"}},
		{"[2001:0db8:0000:0000:0000:0000:0000:0001]", HostPattern{Exact: "[2001:db8::1]"}},
		{"[::FFFF:0A00:0001]", HostPattern{Exact: "[::ffff:10.0.0.1]"}},
		{"[::ffff:10.0.0.1]", HostPattern{Exact: "[::ffff:10.0.0.1]"}},
		{"10.0.0.1", HostPattern{Exact: "10.0.0.1"}},
		{"a_b-c.example", HostPattern{Exact: "a_b-c.example"}},
		{"localhost", HostPattern{Exact: "localhost"}},
		{strings.Repeat("a", 63) + ".example", HostPattern{Exact: strings.Repeat("a", 63) + ".example"}},
	}
	for _, tc := range valid {
		got, err := ParseHostPattern(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseHostPattern(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
		if err := CheckHost(tc.in); err != nil {
			t.Errorf("CheckHost(%q) = %v", tc.in, err)
		}
	}
	invalid := []struct{ in, msg string }{
		{"", "empty"},
		{".", "empty"},
		{"api.*.example", `"*" is allowed only`},
		{"**.x", `"*" is allowed only`},
		{"*", `"*" is allowed only`},
		{"*.", `"*" is allowed only`},
		{"*.*.example", `"*" is allowed only`},
		{"example.*", `"*" is allowed only`},
		{"*shop.example", `"*" is allowed only`},
		{"*.", `"*" is allowed only`},
		{"*..", "empty label"},
		{"a..b", "empty label"},
		{".a", "empty label"},
		{"api.example:8443", "port"},
		{"[::1]:8443", "bracketed IPv6"},
		{"[zz]", "bracketed IPv6"},
		{"[fe80::1%eth0]", "bracketed IPv6"},
		{"[10.0.0.1]", "bracketed IPv6"},
		{"[::1", "bracketed IPv6"},
		{"*.[::1]", "character"},
		{"caf\xc3\xa9.example", "character"},
		{"a b.example", "character"},
		{"a/b", "character"},
		{strings.Repeat("a", 64) + ".example", "label longer"},
		{strings.Repeat("abcdefghi.", 26), "longer than 253"},
	}
	for _, tc := range invalid {
		_, err := ParseHostPattern(tc.in)
		if code, _ := errcode.CodeOf(err); code != CodeInvalid {
			t.Errorf("ParseHostPattern(%q) = %v; want %s", tc.in, err, CodeInvalid)
			continue
		}
		if !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("ParseHostPattern(%q) = %v; want a message with %q", tc.in, err, tc.msg)
		}
	}
}

// TestHostPatternMatch covers wildcard semantics (04 req 27; test plan item
// 5): one or more leading labels, never the bare suffix.
func TestHostPatternMatch(t *testing.T) {
	wild, _ := ParseHostPattern("*.shop.example")
	exact, _ := ParseHostPattern("api.shop.example")
	cases := []struct {
		p    HostPattern
		host string
		want bool
	}{
		{wild, "eu.shop.example", true},
		{wild, "a.b.shop.example", true},
		{wild, "shop.example", false},
		{wild, "xshop.example", false},
		{wild, ".shop.example", false},
		{wild, "shop.example.eu", false},
		{exact, "api.shop.example", true},
		{exact, "eu.api.shop.example", false},
		{exact, "api.shop.example.", false},
		{HostPattern{}, "anything", true},
		{HostPattern{}, "", true},
	}
	for _, tc := range cases {
		if got := tc.p.Match(tc.host); got != tc.want {
			t.Errorf("%q.Match(%q) = %v; want %v", tc.p, tc.host, got, tc.want)
		}
	}
}

// TestIPv6HostSpellings: every spelling of one IPv6 address, in a pattern
// or a request host, meets the same canonical text (04 req 24, 27), so an
// exact-host Route can be neither dodged nor reached by respelling it.
func TestIPv6HostSpellings(t *testing.T) {
	spellings := []string{"[2001:db8::1]", "[2001:DB8:0::1]", "[2001:0db8:0:0:0:0:0:1]", "[2001:db8:0000::0001]"}
	tab := buildHostTable(t, spellings[1])
	for _, pat := range spellings {
		p, err := ParseHostPattern(pat)
		if err != nil {
			t.Fatalf("ParseHostPattern(%q) = %v", pat, err)
		}
		for _, req := range spellings {
			host := NormalizeHost(req + ":8443")
			if !p.Match(host) {
				t.Errorf("pattern %q does not match request host %q (%q)", pat, req, host)
			}
			n := 0
			tab.Lookup(host, func(HostPattern, *hostEntry) bool { n++; return true })
			if n != 1 {
				t.Errorf("table lookup of %q yielded %d entries", host, n)
			}
		}
	}
}

func TestHostPatternTierString(t *testing.T) {
	cases := []struct {
		p    HostPattern
		tier HostTier
		s    string
		any  bool
	}{
		{HostPattern{Exact: "a.example"}, HostExact, "a.example", false},
		{HostPattern{Suffix: ".example"}, HostWildcard, "*.example", false},
		{HostPattern{}, HostAny, "", true},
	}
	for _, tc := range cases {
		if tc.p.Tier() != tc.tier || tc.p.String() != tc.s || tc.p.IsAny() != tc.any {
			t.Errorf("%+v: tier %v string %q any %v", tc.p, tc.p.Tier(), tc.p.String(), tc.p.IsAny())
		}
	}
}

// hostEntry is the value type of the test HostTable.
type hostEntry struct{ names []string }

func buildHostTable(t *testing.T, entries ...string) *HostTable[hostEntry] {
	t.Helper()
	var tab HostTable[hostEntry]
	for _, e := range entries {
		p := HostPattern{}
		if e != "" {
			var err error
			if p, err = ParseHostPattern(e); err != nil {
				t.Fatal(err)
			}
		}
		v := tab.Entry(p)
		v.names = append(v.names, e)
	}
	return &tab
}

// TestHostTableLookup covers the host tables and rank 1 order (04 req 27,
// req 31 rank 1): exact, wildcard with the longest suffix first, any host.
func TestHostTableLookup(t *testing.T) {
	tab := buildHostTable(t, "api.shop.example", "*.shop.example", "*.example", "*.api.shop.example", "", "*.other.example", "api.shop.example")
	if tab.Len() != 5+1 {
		t.Errorf("Len() = %d; want 6", tab.Len())
	}
	lookup := func(host string) []string {
		var got []string
		tab.Lookup(host, func(p HostPattern, v *hostEntry) bool {
			got = append(got, p.String()+"="+strings.Join(v.names, "+"))
			return true
		})
		return got
	}
	cases := []struct {
		host string
		want []string
	}{
		{"api.shop.example", []string{"api.shop.example=api.shop.example+api.shop.example", "*.shop.example=*.shop.example", "*.example=*.example", "="}},
		{"x.api.shop.example", []string{"*.api.shop.example=*.api.shop.example", "*.shop.example=*.shop.example", "*.example=*.example", "="}},
		{"shop.example", []string{"*.example=*.example", "="}},
		{"example", []string{"="}},
		{".example", []string{"="}},
		{"", []string{"="}},
		{"a.b.c.d.other.example", []string{"*.other.example=*.other.example", "*.example=*.example", "="}},
	}
	for _, tc := range cases {
		if got := lookup(tc.host); !slices.Equal(got, tc.want) {
			t.Errorf("Lookup(%q) = %q; want %q", tc.host, got, tc.want)
		}
	}
	// Early stop at each position.
	for stop := 1; stop <= 4; stop++ {
		n := 0
		tab.Lookup("api.shop.example", func(HostPattern, *hostEntry) bool { n++; return n < stop })
		if n != stop {
			t.Errorf("stop after %d: yielded %d", stop, n)
		}
	}
	// Lookup allocates nothing.
	count := 0
	yield := func(HostPattern, *hostEntry) bool { count++; return true }
	if allocs := testing.AllocsPerRun(100, func() { tab.Lookup("x.api.shop.example", yield) }); allocs != 0 {
		t.Errorf("Lookup: %v allocations", allocs)
	}
	// Entry returns the same entry for the same pattern.
	p, _ := ParseHostPattern("*.Shop.Example")
	if tab.Entry(p) != tab.Entry(HostPattern{Suffix: ".shop.example"}) {
		t.Error("Entry is not stable for one pattern")
	}
	var empty HostTable[hostEntry]
	empty.Lookup("a.example", func(HostPattern, *hostEntry) bool { t.Error("empty table yielded"); return true })
	if empty.Len() != 0 {
		t.Errorf("empty Len() = %d", empty.Len())
	}
}

// TestHostTableMatchesPatterns checks the table against HostPattern.Match
// sorted by rank 1, for hosts around every suffix length.
func TestHostTableMatchesPatterns(t *testing.T) {
	entries := []string{"a.example", "*.a.example", "*.b.a.example", "*.example", "*.xample", ""}
	tab := buildHostTable(t, entries...)
	for _, host := range []string{"a.example", "b.a.example", "c.b.a.example", "example", "xample", "e.xample", "a.b", "", "..example", "a..example"} {
		var got, want []string
		tab.Lookup(host, func(p HostPattern, _ *hostEntry) bool { got = append(got, p.String()); return true })
		var ranks []Rank
		for _, e := range entries {
			p := HostPattern{}
			if e != "" {
				p, _ = ParseHostPattern(e)
			}
			if p.Match(host) {
				ranks = append(ranks, Rank{Host: p})
			}
		}
		slices.SortFunc(ranks, Compare)
		for _, r := range ranks {
			want = append(want, r.Host.String())
		}
		if !slices.Equal(got, want) {
			t.Errorf("host %q: table %q; patterns %q", host, got, want)
		}
	}
}
