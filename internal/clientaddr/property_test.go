// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

// hop is one generated forwarding entry.
type hop struct {
	addr    netip.Addr
	trusted bool
}

// randByte draws a byte in [lo, hi) with 0 <= lo < hi <= 256.
func randByte(rng *rand.Rand, lo, hi int) byte {
	return byte(lo + rng.IntN(hi-lo)) //nolint:gosec // G115: the result is below 256 by construction
}

// testRNG returns a deterministic generator for reproducible tests.
func testRNG(a, b uint64) *rand.Rand {
	return rand.New(rand.NewPCG(a, b)) //nolint:gosec // G404: deterministic test data, not secrets
}

// genHop draws a parsable address, trusted or not, in one of the spellings
// senders use.
func genHop(rng *rand.Rand, trusted bool) hop {
	var a netip.Addr
	switch v6 := rng.IntN(3) == 0; {
	case trusted && v6:
		a = netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0xff, 0xff, 14: randByte(rng, 0, 256), 15: randByte(rng, 0, 256)})
	case trusted:
		a = netip.AddrFrom4([4]byte{10, randByte(rng, 0, 256), randByte(rng, 0, 256), randByte(rng, 0, 256)})
	case v6:
		a = netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, 0x00, randByte(rng, 1, 201), 14: randByte(rng, 0, 256), 15: randByte(rng, 0, 256)})
	default:
		a = netip.AddrFrom4([4]byte{randByte(rng, 11, 211), randByte(rng, 0, 256), randByte(rng, 0, 256), randByte(rng, 0, 256)})
	}
	return hop{addr: a, trusted: trusted}
}

// xffSpelling renders a hop as an X-Forwarded-For entry.
func xffSpelling(rng *rand.Rand, h hop) string {
	a := h.addr
	switch rng.IntN(4) {
	case 0:
		if a.Is4() {
			return netip.AddrFrom16(a.As16()).String() // IPv4-mapped
		}
		return "[" + a.String() + "]"
	case 1:
		return netip.AddrPortFrom(a, uint16(rng.IntN(65536))).String() //nolint:gosec // G115: IntN(65536) fits
	default:
		return a.String()
	}
}

// forwardedSpelling renders a hop as a Forwarded element.
func forwardedSpelling(rng *rand.Rand, h hop) string {
	v := h.addr.String()
	switch {
	case h.addr.Is6():
		v = `"[` + v + `]"`
	case rng.IntN(2) == 0:
		v = `"` + netip.AddrPortFrom(h.addr, 8080).String() + `"`
	}
	switch rng.IntN(3) {
	case 0:
		return "for=" + v + ";proto=https"
	case 1:
		return `by="_node";for=` + v
	}
	return "for=" + v
}

// render spreads entries over one to three field lines.
func render(rng *rand.Rand, hops []hop, spell func(*rand.Rand, hop) string) []string {
	parts := make([]string, len(hops))
	for i, h := range hops {
		parts[i] = spell(rng, h)
	}
	var lines []string
	for len(parts) > 0 {
		n := 1 + rng.IntN(len(parts))
		if len(lines) == 2 {
			n = len(parts)
		}
		lines = append(lines, strings.Join(parts[:n], ", "))
		parts = parts[n:]
	}
	return lines
}

// expected is the specified client for parsable hops behind a trusted
// peer: the rightmost untrusted hop within the 16 rightmost, else the
// leftmost of those 16.
func expected(hops []hop) netip.Addr {
	win := hops[max(0, len(hops)-MaxEntries):]
	for i := len(win) - 1; i >= 0; i-- {
		if !win[i].trusted {
			return win[i].addr
		}
	}
	return win[0].addr
}

// TestResolveProperties checks the 06 section 6.3 properties over random
// chains of parsable entries, for both header forms: the result is never
// a trusted address when an untrusted entry exists within the 16
// rightmost, and appending trusted hops (within the window) never changes
// the result.
func TestResolveProperties(t *testing.T) {
	tr := testTrusted()
	peer := netip.MustParseAddrPort("10.0.0.1:443")
	rng := testRNG(6, 3)
	for _, form := range []struct {
		name  string
		field string
		spell func(*rand.Rand, hop) string
	}{
		{"x-forwarded-for", "X-Forwarded-For", xffSpelling},
		{"forwarded", "Forwarded", forwardedSpelling},
	} {
		t.Run(form.name, func(t *testing.T) {
			for iter := range 3000 {
				hops := make([]hop, 1+rng.IntN(24))
				for i := range hops {
					hops[i] = genHop(rng, rng.IntN(3) != 0)
				}
				lines := render(rng, hops, form.spell)
				r := tr.Resolve(peer, http.Header{form.field: lines})
				want := expected(hops)
				if r.IP != want || !r.FromHeader() || r.Port != 0 {
					t.Fatalf("iter %d: %v resolved to %v (via %v), want %v", iter, lines, r.IP, r.Via, want)
				}
				win := hops[max(0, len(hops)-MaxEntries):]
				hasUntrusted := false
				for _, h := range win {
					hasUntrusted = hasUntrusted || !h.trusted
				}
				if hasUntrusted && tr.Contains(r.IP) {
					t.Fatalf("iter %d: %v resolved to trusted %v although an untrusted entry is within the 16 rightmost", iter, lines, r.IP)
				}

				// Append k trusted hops while the client stays in the window.
				pos := -1 // distance of the rightmost untrusted hop from the right
				for i := len(hops) - 1; i >= 0; i-- {
					if !hops[i].trusted {
						pos = len(hops) - i
						break
					}
				}
				if pos < 0 || pos > MaxEntries {
					continue
				}
				k := rng.IntN(MaxEntries - pos + 1)
				more := append([]hop(nil), hops...)
				for range k {
					more = append(more, genHop(rng, true))
				}
				r2 := tr.Resolve(peer, http.Header{form.field: render(rng, more, form.spell)})
				if r2.IP != r.IP {
					t.Fatalf("iter %d: appending %d trusted hops changed %v to %v", iter, k, r.IP, r2.IP)
				}
			}
		})
	}
}

// TestRewriteChainProperty: a chain of trusted Nodes each rewriting with
// Rewrite keeps resolving to the client an untrusted edge saw.
func TestRewriteChainProperty(t *testing.T) {
	tr := testTrusted()
	rng := testRNG(18, 63)
	for iter := range 500 {
		client := genHop(rng, false).addr
		h := http.Header{"X-Forwarded-For": {fmt.Sprintf("%s, %s", genHop(rng, false).addr, genHop(rng, true).addr)}}
		// The edge Node sees the client as an untrusted peer.
		peer := netip.AddrPortFrom(client, 40000)
		r := tr.Resolve(peer, h)
		Rewrite(h, Forwarding{Peer: r.Peer.Addr(), PeerTrusted: r.PeerTrusted, Scheme: "https", Host: "a"})
		for hopN := range rng.IntN(MaxEntries - 1) {
			next := genHop(rng, true).addr
			r = tr.Resolve(netip.AddrPortFrom(next, 443), h)
			if r.IP != client {
				t.Fatalf("iter %d hop %d: resolved %v, want %v (%v)", iter, hopN, r.IP, client, h)
			}
			Rewrite(h, Forwarding{Peer: next, PeerTrusted: true, Scheme: "https", Host: "a"})
		}
	}
}
