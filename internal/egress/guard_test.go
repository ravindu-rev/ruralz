// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Tests for spec 06 section 2.13 (requirements 84 to 86): the deny table
// with its IPv4-mapped, NAT64 and IPv4-compatible forms, RFC 1918 left
// reachable, RURALZ_FETCH_ALLOW parsing (requirement 85) and the connect
// time check after DNS resolution (DNS rebinding).

// TestDeniedTableReq84 covers every listed range and its mapped, NAT64 and
// IPv4-compatible forms, plus addresses that stay reachable.
func TestDeniedTableReq84(t *testing.T) {
	tests := []struct {
		addr   string
		denied bool
	}{
		// 0.0.0.0/8
		{"0.0.0.0", true},
		{"0.255.255.255", true},
		// 127.0.0.0/8
		{"127.0.0.1", true},
		{"127.255.255.254", true},
		// 169.254.0.0/16 (link local, cloud metadata)
		{"169.254.169.254", true},
		{"169.254.0.1", true},
		// ::/128, ::1/128
		{"::", true},
		{"::1", true},
		// fe80::/10
		{"fe80::1", true},
		{"febf:ffff::1", true},
		{"fe80::1%eth0", true},
		// fd00:ec2::254/128 (AWS IMDS over IPv6)
		{"fd00:ec2::254", true},
		// IPv4-mapped forms
		{"::ffff:127.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"::ffff:0.0.0.1", true},
		// NAT64 64:ff9b::/96 and 64:ff9b:1::/48, low 32 bits as IPv4
		{"64:ff9b::7f00:1", true},
		{"64:ff9b::a9fe:a9fe", true},
		{"64:ff9b:1::a9fe:a9fe", true},
		{"64:ff9b:1:ffff::7f00:1", true},
		// IPv4-compatible ::/96 (whole range, proposed hardening)
		{"::7f00:1", true},
		{"::a9fe:a9fe", true},
		{"::a00:1", true},
		// Reachable: RFC 1918, public, other IPv6, NAT64 of public IPv4
		{"10.0.0.1", false},
		{"172.16.5.4", false},
		{"192.168.1.1", false},
		{"8.8.8.8", false},
		{"128.0.0.1", false},
		{"126.255.255.255", false},
		{"169.253.255.255", false},
		{"169.255.0.1", false},
		{"1.0.0.0", false},
		{"fd00:ec2::253", false},
		{"fd00::1", false},
		{"fec0::1", false},
		{"2001:db8::1", false},
		{"::1:0:0:1", false},
		{"64:ff9b::808:808", false},
		{"64:ff9b:2::7f00:1", false},
		{"::ffff:10.0.0.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			a := netip.MustParseAddr(tt.addr)
			if got := Denied(a); got != tt.denied {
				t.Fatalf("Denied(%s) = %v, want %v", tt.addr, got, tt.denied)
			}
			var g *Guard
			err := g.Check(a)
			if tt.denied != (err != nil) {
				t.Fatalf("nil Guard Check(%s) = %v, want denied %v", tt.addr, err, tt.denied)
			}
			if err != nil && !errors.Is(err, ErrDenied) {
				t.Fatalf("Check error %v does not wrap ErrDenied", err)
			}
			if g.Allowed(a) == tt.denied {
				t.Fatalf("Allowed(%s) = %v", tt.addr, !tt.denied)
			}
		})
	}
}

func TestDeniedInvalidAddressReq84(t *testing.T) {
	if !Denied(netip.Addr{}) {
		t.Fatal("the zero Addr must be denied")
	}
	g := &Guard{}
	if err := g.Check(netip.Addr{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("Check(invalid) = %v, want ErrDenied", err)
	}
}

// TestAllowListReq84Req85 checks that RURALZ_FETCH_ALLOW admits exactly the
// listed addresses (mapped forms of listed IPv4 included).
func TestAllowListReq84Req85(t *testing.T) {
	g, err := ParseAllow("127.0.0.1/32, ::1, 169.254.169.254, fe80::/10")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		addr    string
		allowed bool
	}{
		{"127.0.0.1", true},
		{"::ffff:127.0.0.1", true},
		{"127.0.0.2", false},
		{"::1", true},
		{"::", false},
		{"169.254.169.254", true},
		{"169.254.169.253", false},
		{"fe80::1", true},
		{"fe80::1%lo", true},
		{"64:ff9b::7f00:1", false}, // a NAT64 form is a different destination
		{"::7f00:1", false},
		{"0.0.0.0", false},
		{"10.1.2.3", true},
	}
	for _, tt := range tests {
		if got := g.Allowed(netip.MustParseAddr(tt.addr)); got != tt.allowed {
			t.Errorf("Allowed(%s) = %v, want %v", tt.addr, got, tt.allowed)
		}
	}
}

// TestParseAllowReq85 covers the entry format: IP, CIDR, env-proxy, spaces,
// mapped prefixes, and every refused form.
func TestParseAllowReq85(t *testing.T) {
	tests := []struct {
		in       string
		want     string
		envProxy bool
		err      bool
	}{
		{in: "", want: ""},
		{in: "   ", want: ""},
		{in: "127.0.0.1", want: "127.0.0.1/32"},
		{in: "127.0.0.0/8,::1", want: "127.0.0.0/8,::1/128"},
		{in: " 10.0.0.1/8 , ::1 ", want: "10.0.0.0/8,::1/128"},
		{in: "::ffff:127.0.0.1", want: "127.0.0.1/32"},
		{in: "::ffff:127.0.0.0/104", want: "127.0.0.0/8"},
		{in: "env-proxy", want: "env-proxy", envProxy: true},
		{in: "127.0.0.1,env-proxy", want: "127.0.0.1/32,env-proxy", envProxy: true},
		{in: "fd00:ec2::254/128", want: "fd00:ec2::254/128"},
		{in: "0.0.0.0/0", want: "0.0.0.0/0"},
		{in: "localhost", err: true},
		{in: "127.0.0.1,", err: true},
		{in: ",127.0.0.1", err: true},
		{in: "127.0.0.1,,::1", err: true},
		{in: "127.0.0.1/33", err: true},
		{in: "127.0.0.1/", err: true},
		{in: "fe80::1%eth0", err: true},
		{in: "fe80::1%eth0/64", err: true},
		{in: "ENV-PROXY", err: true},
		{in: "*", err: true},
		{in: "127.0.0.1:8080", err: true},
		{in: "http://127.0.0.1", err: true},
		{in: strings.Repeat("x", 200), err: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			g, err := ParseAllow(tt.in)
			if tt.err {
				if err == nil {
					t.Fatalf("ParseAllow(%q) = %v, want error", tt.in, g)
				}
				if !errors.Is(err, ErrAllowEntry) {
					t.Fatalf("error %v does not wrap ErrAllowEntry", err)
				}
				if !strings.Contains(err.Error(), EnvFetchAllow) {
					t.Fatalf("error %q does not name the setting", err)
				}
				if len(err.Error()) > 300 {
					t.Fatalf("error message is %d bytes long", len(err.Error()))
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAllow(%q): %v", tt.in, err)
			}
			if got := g.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
			if g.EnvProxy() != tt.envProxy {
				t.Fatalf("EnvProxy() = %v, want %v", g.EnvProxy(), tt.envProxy)
			}
			if tt.envProxy && g.proxy == nil {
				t.Fatal("env-proxy must install the environment proxy function")
			}
			again, err := ParseAllow(g.String())
			if err != nil || again.String() != g.String() {
				t.Fatalf("String() does not round-trip: %q, %v", again, err)
			}
		})
	}
}

func TestPrefixesAndNilGuard(t *testing.T) {
	g, err := ParseAllow("10.0.0.0/8,::1")
	if err != nil {
		t.Fatal(err)
	}
	p := g.Prefixes()
	if len(p) != 2 || p[0].String() != "10.0.0.0/8" {
		t.Fatalf("Prefixes() = %v", p)
	}
	p[0] = netip.MustParsePrefix("127.0.0.0/8")
	if g.Allowed(netip.MustParseAddr("127.0.0.1")) {
		t.Fatal("Prefixes must return a copy")
	}
	var nilGuard *Guard
	if nilGuard.Prefixes() != nil || nilGuard.String() != "" || nilGuard.EnvProxy() {
		t.Fatal("a nil Guard has no entries")
	}
}

func TestFromEnvReq85(t *testing.T) {
	g, err := FromEnv(func(string) (string, bool) { return "", false })
	if err != nil || g.String() != "" {
		t.Fatalf("unset: %v, %v", g, err)
	}
	var asked string
	g, err = FromEnv(func(k string) (string, bool) {
		asked = k
		return "127.0.0.1", true
	})
	if err != nil || g.String() != "127.0.0.1/32" || asked != EnvFetchAllow {
		t.Fatalf("set: %v, %v, asked %q", g, err, asked)
	}
	if _, err := FromEnv(func(string) (string, bool) { return "bogus", true }); !errors.Is(err, ErrAllowEntry) {
		t.Fatalf("bad entry: %v", err)
	}
}

// TestControlReq84 checks the net.Dialer Control function on resolved
// addresses.
func TestControlReq84(t *testing.T) {
	g, err := ParseAllow("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		network, address string
		denied           bool
	}{
		{"tcp4", "127.0.0.1:80", false},
		{"tcp4", "127.0.0.2:80", true},
		{"tcp6", "[::1]:443", true},
		{"tcp6", "[fe80::1%eth0]:443", true},
		{"tcp6", "[::ffff:127.0.0.1]:443", false},
		{"tcp4", "169.254.169.254:80", true},
		{"tcp4", "93.184.216.34:443", false},
		{"tcp6", "[2001:db8::1]:443", false},
		{"unix", "/run/x.sock", true},
		{"tcp", "not an address", true},
	}
	for _, tt := range tests {
		err := g.Control(tt.network, tt.address, nil)
		if tt.denied != (err != nil) {
			t.Errorf("Control(%s, %s) = %v, want denied %v", tt.network, tt.address, err, tt.denied)
		}
		if err != nil && !errors.Is(err, ErrDenied) {
			t.Errorf("Control error %v does not wrap ErrDenied", err)
		}
	}
}

func TestCheckHost(t *testing.T) {
	var g *Guard
	for host, denied := range map[string]bool{
		"127.0.0.1":   true,
		"[::1]":       true,
		"::1":         true,
		"example.com": false,
		"10.0.0.1":    false,
		"":            false,
	} {
		if err := g.CheckHost(host); denied != (err != nil) {
			t.Errorf("CheckHost(%q) = %v, want denied %v", host, err, denied)
		}
	}
}

// TestDialerRefusesDeniedAddressReq84 dials a real loopback listener: refused
// without an allow entry, connected with one.
func TestDialerRefusesDeniedAddressReq84(t *testing.T) {
	ln := listen(t)
	var nilGuard *Guard
	if _, err := nilGuard.Dialer(0).DialContext(t.Context(), "tcp", ln.Addr().String()); !errors.Is(err, ErrDenied) {
		t.Fatalf("dial without allow = %v, want ErrDenied", err)
	}
	g, err := ParseAllow("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	c, err := g.Dialer(time.Second).DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial with allow: %v", err)
	}
	_ = c.Close()
	if d := g.Dialer(-1); d.Timeout != DefaultDialTimeout || d.Control == nil {
		t.Fatalf("Dialer defaults: timeout %v", d.Timeout)
	}
}

// TestDialerDNSRebindingReq84 resolves a name through a fake DNS server that
// answers 127.0.0.1: the check runs after resolution, so the connect is
// refused whatever the name looked like.
func TestDialerDNSRebindingReq84(t *testing.T) {
	ln := listen(t)
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	res := fakeResolver(t, netip.MustParseAddr("127.0.0.1"))

	g := &Guard{resolver: res}
	_, err := g.Dialer(time.Second).DialContext(t.Context(), "tcp", net.JoinHostPort("rebind.test", port))
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("dial to a name resolving to loopback = %v, want ErrDenied", err)
	}

	allowed, err := ParseAllow("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	allowed.resolver = res
	c, err := allowed.Dialer(time.Second).DialContext(t.Context(), "tcp", net.JoinHostPort("rebind.test", port))
	if err != nil {
		t.Fatalf("dial with allow: %v", err)
	}
	_ = c.Close()
}

func TestOrigin(t *testing.T) {
	tests := map[string]string{
		"https://IdP.Example.com/jwks":       "https://idp.example.com:443",
		"https://idp.example.com:443/x":      "https://idp.example.com:443",
		"https://idp.example.com:8443/x?y=z": "https://idp.example.com:8443",
		"https://idp.example.com:08443":      "https://idp.example.com:8443",
		"HTTP://idp.example.com":             "http://idp.example.com:80",
		"https://[2001:DB8::1]/x":            "https://[2001:db8::1]:443",
		"https://[2001:db8::1]:9443":         "https://[2001:db8::1]:9443",
		"https://user@idp.example.com/":      "https://idp.example.com:443",
	}
	for in, want := range tests {
		u, err := url.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := Origin(u); got != want {
			t.Errorf("Origin(%s) = %q, want %q", in, got, want)
		}
	}
}

// listen opens a loopback TCP listener that accepts and closes connections
// until the test ends.
func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return ln
}

// fakeResolver returns a pure-Go resolver whose DNS server (a UDP socket
// owned by the test) answers every A query with addr and every other query
// with no records.
func fakeResolver(t *testing.T, addr netip.Addr) *net.Resolver {
	t.Helper()
	pc, err := new(net.ListenConfig).ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if resp := dnsAnswer(buf[:n], addr); resp != nil {
				_, _ = pc.WriteTo(resp, from)
			}
		}
	}()
	t.Cleanup(func() {
		_ = pc.Close()
		<-done
	})
	server := pc.LocalAddr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp4", server)
		},
	}
}

// dnsAnswer builds the response to one DNS query: the question copied, one
// A record for an A query, none otherwise.
func dnsAnswer(q []byte, addr netip.Addr) []byte {
	if len(q) < 12 {
		return nil
	}
	end := 12
	for end < len(q) && q[end] != 0 {
		end += int(q[end]) + 1
	}
	end += 5 // root label, QTYPE, QCLASS
	if end > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[end-4 : end-2])
	resp := make([]byte, 0, end+16)
	resp = append(resp, q[0], q[1], 0x81, 0x80, 0, 1)
	if qtype == 1 {
		resp = append(resp, 0, 1)
	} else {
		resp = append(resp, 0, 0)
	}
	resp = append(resp, 0, 0, 0, 0)
	resp = append(resp, q[12:end]...)
	if qtype == 1 {
		ip := addr.As4()
		resp = append(resp, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4)
		resp = append(resp, ip[:]...)
	}
	return resp
}

func BenchmarkCheck(b *testing.B) {
	g, err := ParseAllow("127.0.0.1/32,::1,10.0.0.0/8")
	if err != nil {
		b.Fatal(err)
	}
	addrs := []netip.Addr{
		netip.MustParseAddr("93.184.216.34"),
		netip.MustParseAddr("2001:db8::1"),
		netip.MustParseAddr("64:ff9b::7f00:1"),
		netip.MustParseAddr("127.0.0.1"),
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		_ = g.Allowed(addrs[i%len(addrs)])
		i++
	}
}

func BenchmarkControl(b *testing.B) {
	g, err := ParseAllow("127.0.0.1/32")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := g.Control("tcp4", "93.184.216.34:443", nil); err != nil {
			b.Fatal(err)
		}
	}
}
