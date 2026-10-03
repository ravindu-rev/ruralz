// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"net/http"
	"net/netip"
	"reflect"
	"testing"
)

// TestRewrite is the WP-15 "header rewrite table test" for 04 req 18 and
// 06 req 63: overwrite from untrusted peers, append from trusted ones, and
// X-Forwarded-Host carrying the client authority (R-28).
func TestRewrite(t *testing.T) {
	untrusted := netip.MustParseAddr("203.0.113.50")
	trusted := netip.MustParseAddr("10.0.0.1")
	tests := []struct {
		name string
		in   http.Header
		f    Forwarding
		want http.Header
	}{
		{
			name: "req18 untrusted peer without forwarding fields",
			in:   http.Header{"Accept": {"*/*"}},
			f:    Forwarding{Peer: untrusted, Scheme: "https", Host: "api.shop.example"},
			want: http.Header{
				"Accept":            {"*/*"},
				"X-Forwarded-For":   {"203.0.113.50"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"api.shop.example"},
			},
		},
		{
			name: "req63 untrusted peer overwrites, never appends, and removes Forwarded",
			in: http.Header{
				"X-Forwarded-For":   {"1.1.1.1, 10.0.0.9", "2.2.2.2"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"evil.example"},
				"Forwarded":         {"for=1.1.1.1;proto=https"},
			},
			f: Forwarding{Peer: untrusted, Scheme: "http", Host: "api.shop.example:8080"},
			want: http.Header{
				"X-Forwarded-For":   {"203.0.113.50"},
				"X-Forwarded-Proto": {"http"},
				"X-Forwarded-Host":  {"api.shop.example:8080"},
			},
		},
		{
			name: "req18 trusted peer appends to X-Forwarded-For and keeps the rest",
			in: http.Header{
				"X-Forwarded-For":   {"1.1.1.1"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"shop.example"},
				"Forwarded":         {"for=1.1.1.1;proto=https"},
			},
			f: Forwarding{Peer: trusted, PeerTrusted: true, Scheme: "http", Host: "internal.example"},
			want: http.Header{
				"X-Forwarded-For":   {"1.1.1.1, 10.0.0.1"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"shop.example"},
				"Forwarded":         {"for=1.1.1.1;proto=https"},
			},
		},
		{
			name: "req18 trusted peer joins field lines before appending",
			in:   http.Header{"X-Forwarded-For": {"1.1.1.1, 6.6.6.6", " ", "10.0.0.9 "}},
			f:    Forwarding{Peer: trusted, PeerTrusted: true, Scheme: "https", Host: "api.shop.example"},
			want: http.Header{
				"X-Forwarded-For":   {"1.1.1.1, 6.6.6.6, 10.0.0.9, 10.0.0.1"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"api.shop.example"},
			},
		},
		{
			name: "req18 trusted peer without prior fields",
			in:   http.Header{"X-Forwarded-For": {""}},
			f:    Forwarding{Peer: trusted, PeerTrusted: true, Scheme: "https", Host: "api.shop.example"},
			want: http.Header{
				"X-Forwarded-For":   {"10.0.0.1"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"api.shop.example"},
			},
		},
		{
			name: "req18 trusted peer without any X-Forwarded-For",
			in:   http.Header{"Forwarded": {"for=1.1.1.1"}},
			f:    Forwarding{Peer: trusted, PeerTrusted: true, Scheme: "https", Host: "api.shop.example"},
			want: http.Header{
				"Forwarded":         {"for=1.1.1.1"},
				"X-Forwarded-For":   {"10.0.0.1"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"api.shop.example"},
			},
		},
		{
			name: "req18 IPv6 peer written without brackets",
			in:   http.Header{},
			f:    Forwarding{Peer: netip.MustParseAddr("2001:db8::7%eth0"), Scheme: "https", Host: "[2001:db8::1]:8443"},
			want: http.Header{
				"X-Forwarded-For":   {"2001:db8::7"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"[2001:db8::1]:8443"},
			},
		},
		{
			name: "req17 IPv4-mapped trusted peer written unmapped",
			in:   http.Header{"X-Forwarded-For": {"1.1.1.1"}},
			f:    Forwarding{Peer: netip.MustParseAddr("::ffff:10.0.0.1"), PeerTrusted: true, Scheme: "http", Host: "a"},
			want: http.Header{
				"X-Forwarded-For":   {"1.1.1.1, 10.0.0.1"},
				"X-Forwarded-Proto": {"http"},
				"X-Forwarded-Host":  {"a"},
			},
		},
		{
			name: "req63 unknown peer removes client-supplied fields",
			in: http.Header{
				"X-Forwarded-For": {"1.1.1.1"},
				"Forwarded":       {"for=1.1.1.1"},
			},
			f:    Forwarding{PeerTrusted: true, Scheme: "http", Host: "a"},
			want: http.Header{"X-Forwarded-Proto": {"http"}, "X-Forwarded-Host": {"a"}},
		},
		{
			name: "req63 empty scheme and host remove the fields",
			in: http.Header{
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"evil.example"},
			},
			f:    Forwarding{Peer: untrusted},
			want: http.Header{"X-Forwarded-For": {"203.0.113.50"}},
		},
		{
			name: "req63 non-canonical spellings are overwritten too",
			in: http.Header{
				"x-forwarded-for":   {"1.1.1.1"},
				"X-FORWARDED-HOST":  {"evil.example"},
				"forwarded":         {"for=1.1.1.1"},
				"x-forwarded-proto": {"https"},
				"X-Custom":          {"kept"},
			},
			f: Forwarding{Peer: untrusted, Scheme: "http", Host: "api.shop.example"},
			want: http.Header{
				"X-Forwarded-For":   {"203.0.113.50"},
				"X-Forwarded-Proto": {"http"},
				"X-Forwarded-Host":  {"api.shop.example"},
				"X-Custom":          {"kept"},
			},
		},
		{
			name: "req18 non-canonical spellings are joined when trusted",
			in: http.Header{
				"X-Forwarded-For": {"1.1.1.1"},
				"x-forwarded-for": {"10.0.0.9"},
			},
			f: Forwarding{Peer: trusted, PeerTrusted: true, Scheme: "https", Host: "api.shop.example"},
			want: http.Header{
				"X-Forwarded-For":   {"1.1.1.1, 10.0.0.9, 10.0.0.1"},
				"X-Forwarded-Proto": {"https"},
				"X-Forwarded-Host":  {"api.shop.example"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.in.Clone()
			Rewrite(h, tt.f)
			if !reflect.DeepEqual(h, tt.want) {
				t.Errorf("Rewrite =\n  %v\nwant\n  %v", h, tt.want)
			}
		})
	}
}

// TestRewriteVariantOrder: several non-canonical spellings fold in byte
// order of the spelling, whatever the map order (04 req 18), and the fold
// never writes into a backing array the input header shares.
func TestRewriteVariantOrder(t *testing.T) {
	shared := make([]string, 1, 4)
	shared[0] = "1.1.1.1"
	for range 50 {
		in := http.Header{
			"X-Forwarded-For": shared,
			"x-forwarded-for": {"10.0.0.8"},
			"X-FORWARDED-FOR": {"10.0.0.9"},
			"x-Forwarded-For": {"10.0.0.7"},
		}
		Rewrite(in, Forwarding{Peer: netip.MustParseAddr("10.0.0.1"), PeerTrusted: true, Scheme: "https", Host: "a"})
		if got, want := in["X-Forwarded-For"], []string{"1.1.1.1, 10.0.0.9, 10.0.0.7, 10.0.0.8, 10.0.0.1"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("X-Forwarded-For = %q, want %q", got, want)
		}
		for _, v := range shared[len(shared):cap(shared)] {
			if v != "" {
				t.Fatalf("fold wrote %q into the shared backing array", v)
			}
		}
	}
}

// TestTrustedRewrite decides trust with the compiled set.
func TestTrustedRewrite(t *testing.T) {
	tr := testTrusted()
	h := http.Header{"X-Forwarded-For": {"1.1.1.1"}, "Forwarded": {"for=1.1.1.1"}}
	tr.Rewrite(h, netip.MustParseAddr("10.0.0.1"), "https", "api.shop.example")
	if got := h.Get("X-Forwarded-For"); got != "1.1.1.1, 10.0.0.1" || h.Get("Forwarded") == "" {
		t.Errorf("trusted Rewrite = %v", h)
	}
	h = http.Header{"X-Forwarded-For": {"1.1.1.1"}, "Forwarded": {"for=1.1.1.1"}}
	tr.Rewrite(h, netip.MustParseAddr("203.0.113.50"), "https", "api.shop.example")
	if got := h.Get("X-Forwarded-For"); got != "203.0.113.50" || h.Get("Forwarded") != "" {
		t.Errorf("untrusted Rewrite = %v", h)
	}
	var none *Trusted
	h = http.Header{"X-Forwarded-For": {"1.1.1.1"}}
	none.Rewrite(h, netip.MustParseAddr("10.0.0.1"), "http", "a")
	if got := h.Get("X-Forwarded-For"); got != "10.0.0.1" {
		t.Errorf("nil set Rewrite = %v", h)
	}
}

// TestRewriteThenResolve: the rewritten header, seen by an Upstream that
// trusts this Node, resolves to the same client (the chain of proxies
// stays consistent).
func TestRewriteThenResolve(t *testing.T) {
	tr := testTrusted()
	in := hdr("X-Forwarded-For", "6.6.6.6, 1.1.1.1, 10.0.0.9")
	peer := netip.MustParseAddrPort("10.0.0.1:443")
	first := tr.Resolve(peer, in)
	out := in.Clone()
	Rewrite(out, Forwarding{Peer: first.Peer.Addr(), PeerTrusted: first.PeerTrusted, Scheme: "https", Host: "a"})
	next := tr.Resolve(netip.MustParseAddrPort("10.0.0.2:1"), out)
	if first.IP != next.IP || first.IP != netip.MustParseAddr("1.1.1.1") {
		t.Errorf("client %v, next hop resolves %v", first.IP, next.IP)
	}
}

// TestRewriteNilHeader: a nil header is a caller bug that Rewrite reports
// with a clear panic instead of an "assignment to entry in nil map".
func TestRewriteNilHeader(t *testing.T) {
	for _, f := range []Forwarding{
		{Peer: netip.MustParseAddr("203.0.113.50"), Scheme: "https", Host: "a"},
		{Peer: netip.MustParseAddr("10.0.0.1"), PeerTrusted: true, Scheme: "https", Host: "a"},
	} {
		func() {
			defer func() {
				if got, want := recover(), "clientaddr: Rewrite called with a nil http.Header"; got != want {
					t.Errorf("Rewrite(nil, %+v) panicked with %v, want %q", f, got, want)
				}
			}()
			Rewrite(nil, f)
		}()
	}
}

// TestRewriteOncePerAttempt: the documented use, one Rewrite per attempt
// on a fresh clone of the client header, gives every attempt the same
// fields and never changes the client header (04 req 18), while a second
// Rewrite of the same header from a trusted peer appends the peer again,
// which is why the caller must not reuse it.
func TestRewriteOncePerAttempt(t *testing.T) {
	client := http.Header{
		"X-Forwarded-For":   {"1.1.1.1", "10.0.0.9"},
		"x-forwarded-proto": {"https"},
		"Forwarded":         {"for=1.1.1.1"},
	}
	orig := client.Clone()
	f := Forwarding{Peer: netip.MustParseAddr("10.0.0.1"), PeerTrusted: true, Scheme: "http", Host: "a"}
	var first http.Header
	for attempt := range 3 {
		h := client.Clone()
		Rewrite(h, f)
		if attempt == 0 {
			first = h
		} else if !reflect.DeepEqual(h, first) {
			t.Errorf("attempt %d: Rewrite = %v, want %v", attempt, h, first)
		}
	}
	if !reflect.DeepEqual(client, orig) {
		t.Errorf("client header changed to %v, want %v", client, orig)
	}
	Rewrite(first, f)
	if got, want := first["X-Forwarded-For"], []string{"1.1.1.1, 10.0.0.9, 10.0.0.1, 10.0.0.1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("second Rewrite of one header: X-Forwarded-For = %q, want %q", got, want)
	}
}
