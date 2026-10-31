// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package netaddr

import (
	"strings"
	"testing"
)

// TestParseHostPort: a static Endpoint address is host:port with a port
// from 1 to 65535 and a host that is an IP address, unmapped, or a DNS
// name; every other shape is refused without quoting the address.
func TestParseHostPort(t *testing.T) {
	ok := []struct {
		in   string
		want HostPort
	}{
		{"orders.shop.svc:8080", HostPort{Host: "orders.shop.svc", Port: 8080}},
		{"API.internal:443", HostPort{Host: "API.internal", Port: 443}},
		{"api.internal.:80", HostPort{Host: "api.internal.", Port: 80}},
		{"_svc.example:80", HostPort{Host: "_svc.example", Port: 80}},
		{"x:65535", HostPort{Host: "x", Port: 65535}},
	}
	for _, tt := range ok {
		got, err := ParseHostPort(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParseHostPort(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
	for in, want := range map[string]string{
		"10.0.0.1:8080":        "10.0.0.1",
		"[2001:db8::1]:443":    "2001:db8::1",
		"[::ffff:10.0.0.1]:80": "10.0.0.1",
		"[fe80::1%eth0]:80":    "fe80::1%eth0",
	} {
		got, err := ParseHostPort(in)
		if err != nil || got.Host != "" || got.Addr.String() != want {
			t.Errorf("ParseHostPort(%q) = %+v, %v; want address %s", in, got, err, want)
		}
	}
	bad := map[string]string{
		"orders":                                "is not host:port",
		"orders:":                               "the port is not a decimal number",
		"orders:0":                              "the port is not a decimal number",
		"orders:65536":                          "the port is not a decimal number",
		"orders:+80":                            "the port is not a decimal number",
		"orders:http":                           "the port is not a decimal number",
		"or ders:80":                            "the host is neither",
		":80":                                   "the host is neither",
		"a..b:80":                               "a DNS label is 1 to 63 bytes",
		strings.Repeat("a", 64) + ".example:80": "a DNS label is 1 to 63 bytes",
	}
	for in, want := range bad {
		_, err := ParseHostPort(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseHostPort(%.20q) = %v, want an error saying %q", in, err, want)
		}
		if err != nil && len(in) > 4 && strings.Contains(err.Error(), in) {
			t.Errorf("ParseHostPort(%.20q): the error quotes the address: %v", in, err)
		}
	}
}

// TestCheckNameAndPortName: dns discovery's service is a DNS name and its
// named port an SRV service name.
func TestCheckNameAndPortName(t *testing.T) {
	long := strings.Repeat(strings.Repeat("a", 50)+".", 5) + "x"
	for _, n := range []string{"a", "orders.shop.svc", "orders.shop.svc.", "_srv._tcp.x", strings.Repeat("a", 63)} {
		if err := CheckName(n); err != nil {
			t.Errorf("CheckName(%q) = %v", n, err)
		}
	}
	for _, n := range []string{"", ".", "a..b", ".a", strings.Repeat("a", 64), long, "a b", "ä", "a\x00b"} {
		if err := CheckName(n); err == nil {
			t.Errorf("CheckName(%.20q) accepted", n)
		}
	}
	for _, n := range []string{"http", "h2c", "x-1", "a23456789012345"} {
		if err := CheckPortName(n); err != nil {
			t.Errorf("CheckPortName(%q) = %v", n, err)
		}
	}
	for _, n := range []string{"", "8080", "a234567890123456", "ht_tp", "-"} {
		if err := CheckPortName(n); err == nil {
			t.Errorf("CheckPortName(%q) accepted", n)
		}
	}
	for n, ok := range map[int64]bool{1: true, 80: true, 65535: true, 0: false, -1: false, 65536: false} {
		if err := CheckPort(n); (err == nil) != ok {
			t.Errorf("CheckPort(%d) = %v, want ok %v", n, err, ok)
		}
	}
}

// FuzzIsName: the allocation-free name test used on SRV answers agrees
// with CheckName, which validates configured names.
func FuzzIsName(f *testing.F) {
	for _, s := range []string{
		"a", "a.", "orders.svc", "orders.svc.", "_http._tcp.orders", "10.0.0.1", "::1", "",
		".", "..", "a..b", ".a", "bad name", "a\x00b", strings.Repeat("a", 63), strings.Repeat("a", 64),
		strings.Repeat("a.", 126) + "a", strings.Repeat("a.", 127) + "a",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := IsName(s), CheckName(s) == nil; got != want {
			t.Fatalf("IsName(%q) = %v, CheckName accepts %v", s, got, want)
		}
	})
}

// TestIsNameAllocates: IsName runs on the request path without
// allocating.
func TestIsNameAllocates(t *testing.T) {
	if n := testing.AllocsPerRun(100, func() { _ = IsName("orders.shop.svc.") }); n != 0 {
		t.Errorf("IsName allocates %.0f times", n)
	}
}

// TestParseOTLPEndpoint: an http or https URL with a host and an optional,
// non-empty port, the port 4317 when absent, and nothing else.
func TestParseOTLPEndpoint(t *testing.T) {
	ok := map[string]OTLPEndpoint{
		"https://collector:4317": {Scheme: "https", Target: "collector:4317"},
		"https://collector":      {Scheme: "https", Target: "collector:4317"},
		"https://collector/":     {Scheme: "https", Target: "collector:4317"},
		"http://127.0.0.1:9000":  {Scheme: "http", Target: "127.0.0.1:9000"},
		"https://[::1]:4317":     {Scheme: "https", Target: "[::1]:4317"},
	}
	for in, want := range ok {
		if got, err := ParseOTLPEndpoint(in); err != nil || got != want {
			t.Errorf("ParseOTLPEndpoint(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	bad := map[string]string{
		"http://[::1":                 "is not a URL",
		"https://host:abc":            "is not a URL",
		"grpc://collector":            "must use http or https",
		"collector:4317":              "must use http or https",
		"https://user@collector":      "has no user information, query or fragment",
		"https://collector?x=1":       "has no user information, query or fragment",
		"https://collector?":          "has no user information, query or fragment",
		"https://collector#f":         "has no user information, query or fragment",
		"https://collector/v1/traces": "has no path",
		"https://":                    "has no host",
		"https://:4317":               "has no host",
		"https://collector:":          "has an empty port",
	}
	for in, want := range bad {
		if _, err := ParseOTLPEndpoint(in); err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("ParseOTLPEndpoint(%q) = %v, want an error ending %q", in, err, want)
		}
	}
}

// TestCanonicalHost: a State Store zone map host is an IP literal in its
// canonical text or a DNS name lower-cased, so two spellings of one host
// compare equal and anything else, a port or brackets included, is
// refused without quoting the host.
func TestCanonicalHost(t *testing.T) {
	for in, want := range map[string]string{
		"valkey-0.state.internal":  "valkey-0.state.internal",
		"Valkey-0.STATE.internal":  "valkey-0.state.internal",
		"valkey-0.state.internal.": "valkey-0.state.internal.",
		"_cache.internal":          "_cache.internal",
		"10.0.2.15":                "10.0.2.15",
		"2001:DB8::1":              "2001:db8::1",
		"2001:0db8:0:0:0:0:0:1":    "2001:db8::1",
		"0:0:0:0:0:0:0:1":          "::1",
		"::ffff:10.0.0.1":          "::ffff:10.0.0.1",
		"fe80::1%eth0":             "fe80::1%eth0",
		"localhost":                "localhost",
	} {
		got, err := CanonicalHost(in)
		if err != nil || got != want {
			t.Errorf("CanonicalHost(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "valkey:6379", "10.0.2.15:6379", "[2001:db8::1]", "[::1]:6379", "a..b", "host/path", "a b",
		"user@host", strings.Repeat("a", 64) + ".internal", "ä.internal",
	} {
		got, err := CanonicalHost(in)
		if err == nil {
			t.Errorf("CanonicalHost(%q) = %q, want an error", in, got)
			continue
		}
		if !strings.Contains(err.Error(), "neither an IP literal nor a DNS name") {
			t.Errorf("CanonicalHost(%q) = %v", in, err)
		}
		if len(in) > 4 && strings.Contains(err.Error(), in) {
			t.Errorf("CanonicalHost(%q): the error quotes the host: %v", in, err)
		}
	}
}

// FuzzCanonicalHost: the canonical form is a fixed point, accepted again
// unchanged, so the zone map's keys and the hosts the State client looks
// up meet in one form.
func FuzzCanonicalHost(f *testing.F) {
	for _, s := range []string{"a", "A.b.", "10.0.0.1", "2001:DB8::1", "::ffff:1.2.3.4", "fe80::1%Eth0", "[::1]", "x:1", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, h string) {
		c, err := CanonicalHost(h)
		if err != nil {
			return
		}
		again, err := CanonicalHost(c)
		if err != nil || again != c {
			t.Fatalf("CanonicalHost(%q) = %q, then %q, %v", h, c, again, err)
		}
	})
}
