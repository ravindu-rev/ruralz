// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"bytes"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// FuzzProxyV2 is the WP-15 fuzz target (06 6.4 FuzzProxyV2; 04's
// FuzzProxyProtoRead): ReadHeader never panics, never reads past the
// header or MaxHeaderLen, fails only with ErrMalformed, returns a header
// consistent with its family, gives the same outcome however the bytes are
// chunked, and the listener Conn agrees with it.
func FuzzProxyV2(f *testing.F) {
	for _, seed := range [][]byte{
		proxyTCP4("203.0.113.7:51000", "192.0.2.1:443", nil),
		append(proxyTCP4("203.0.113.7:1", "192.0.2.1:2", tlv(0x01, []byte("h2"))), "GET / HTTP/1.1\r\n"...),
		proxyTCP6("[2001:db8::7]:4711", "[2001:db8::1]:8443", tlv(0x04, make([]byte, 20))),
		proxyTCP6("[::ffff:198.51.100.9]:1", "[::1]:2", nil),
		local(),
		v2(0x20, 0x00, []byte("junk")),
		v2(0x20, 0x11, make([]byte, 12)),
		v2(0x21, 0x12, make([]byte, 12)),
		v2(0x21, 0x31, make([]byte, 216)),
		v2(0x11, 0x11, make([]byte, 12)),
		v2(0x22, 0x11, make([]byte, 12)),
		v2(0x21, 0x00, nil),
		v2(0x21, 0x11, make([]byte, 11)),
		v2Len(0x21, 0x11, MaxVariableLen+1, nil),
		v2Len(0x21, 0x21, 200, make([]byte, 60)),
		[]byte("PROXY TCP4 203.0.113.7 192.0.2.1 51000 443\r\n"),
		[]byte("GET / HTTP/1.1\r\n\r\n"),
		{0x16, 0x03, 0x01, 0x00, 0xa5},
		[]byte(Signature),
		{},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := bytes.NewReader(data)
		h, err := ReadHeader(r)
		consumed := len(data) - r.Len()
		if consumed > MaxHeaderLen {
			t.Fatalf("read %d bytes, above MaxHeaderLen", consumed)
		}
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("error %v does not wrap ErrMalformed", err)
			}
		} else {
			checkHeader(t, data, h, consumed)
		}

		// Chunking never changes the outcome. One-byte reads are kept to
		// short inputs: their read count would otherwise make every input
		// size new coverage, and Go minimizes each such input in O(n^2).
		chunked := iotest.HalfReader(bytes.NewReader(data))
		if len(data) <= 64 {
			chunked = iotest.OneByteReader(bytes.NewReader(data))
		}
		h2, err2 := ReadHeader(chunked)
		if (err == nil) != (err2 == nil) || h != h2 {
			t.Fatalf("chunked reads: %+v, %v; whole reads: %+v, %v", h2, err2, h, err)
		}

		// The listener Conn reads the same header and then the rest.
		c := &Conn{Conn: &deadlineConn{r: bytes.NewReader(data)}, opts: &ListenerOptions{
			Clock: clocktest.New(time.Unix(1, 0)), Timeout: DefaultHeaderTimeout,
		}}
		buf := make([]byte, len(data)+1)
		n, cerr := c.Read(buf)
		if err != nil {
			if !errors.Is(cerr, ErrMalformed) {
				t.Fatalf("Conn.Read err = %v, ReadHeader err = %v", cerr, err)
			}
			return
		}
		if got, ok := c.Header(); !ok || got != h {
			t.Fatalf("Conn.Header = %+v, %v; want %+v", got, ok, h)
		}
		if rest := data[h.Len:]; len(rest) > 0 && (cerr != nil || !bytes.Equal(buf[:n], rest)) {
			t.Fatalf("Conn.Read after the header = %q, %v; want %q", buf[:n], cerr, rest)
		}
	})
}

// checkHeader asserts the invariants of a successfully read header.
func checkHeader(t *testing.T, data []byte, h Header, consumed int) {
	t.Helper()
	if h.Len != consumed || h.Len < fixedLen || !bytes.Equal(data[:len(Signature)], []byte(Signature)) {
		t.Fatalf("header %+v after %d bytes", h, consumed)
	}
	switch h.Command {
	case CommandLocal:
		if h.Source.IsValid() || h.Destination.IsValid() {
			t.Fatalf("LOCAL header with addresses: %+v", h)
		}
	case CommandProxy:
		switch h.Family {
		case FamilyTCP4:
			if !h.Source.Addr().Is4() || !h.Destination.Addr().Is4() || h.Len < fixedLen+tcp4AddrLen {
				t.Fatalf("TCP4 header %+v", h)
			}
		case FamilyTCP6:
			if !h.Source.Addr().Is6() || !h.Destination.Addr().Is6() || h.Len < fixedLen+tcp6AddrLen {
				t.Fatalf("TCP6 header %+v", h)
			}
		default:
			t.Fatalf("PROXY header with family %v", h.Family)
		}
	default:
		t.Fatalf("command %v accepted", h.Command)
	}
}

// refEntry is one entry of the reference walk.
type refEntry struct {
	addr  netip.Addr
	ok    bool
	empty bool
}

// refWalk is a direct reading of 04 req 17 and 06 req 60 over a slice.
func refWalk(t *Trusted, entries []refEntry) (netip.Addr, bool) {
	if len(entries) > MaxEntries {
		entries = entries[len(entries)-MaxEntries:]
	}
	var last netip.Addr
	have := false
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		switch {
		case e.empty:
			continue
		case !e.ok:
			return last, have
		case !t.Contains(e.addr):
			return e.addr, true
		}
		last, have = e.addr, true
	}
	return last, have
}

// refXFF splits X-Forwarded-For lines with strings.Split.
func refXFF(lines []string) []refEntry {
	var out []refEntry
	for _, l := range lines {
		for _, e := range strings.Split(l, ",") {
			e = strings.Trim(e, " \t")
			a, ok := parseXFFEntry(e)
			out = append(out, refEntry{addr: a, ok: ok, empty: e == ""})
		}
	}
	return out
}

// refForwarded splits Forwarded lines into elements by building strings,
// each line from its right end: a '"' toggles quoting unless an odd run of
// backslashes precedes it, a ',' outside quoting ends an element, and the
// leftmost element of a line still inside quoting is unparsable.
func refForwarded(lines []string) []refEntry {
	var out []refEntry
	for _, l := range lines {
		var elems []string // right to left
		var cur []byte     // reversed
		quoted := false
		for i := len(l) - 1; i >= 0; i-- {
			c := l[i]
			if c == '"' && (len(l[:i])-len(strings.TrimRight(l[:i], `\`)))%2 == 0 {
				quoted = !quoted
			}
			if c == ',' && !quoted {
				elems = append(elems, reversed(cur))
				cur = cur[:0]
				continue
			}
			cur = append(cur, c)
		}
		elems = append(elems, reversed(cur))
		bad := quoted
		for k := len(elems) - 1; k >= 0; k-- {
			e := strings.Trim(elems[k], " \t")
			switch {
			case k == len(elems)-1 && bad:
				out = append(out, refEntry{})
			case e == "":
				out = append(out, refEntry{empty: true})
			default:
				a, ok := parseForwardedElement(e)
				out = append(out, refEntry{addr: a, ok: ok})
			}
		}
	}
	return out
}

// reversed returns b reversed as a string.
func reversed(b []byte) string {
	r := make([]byte, len(b))
	for i, c := range b {
		r[len(b)-1-i] = c
	}
	return string(r)
}

// appearsIn reports whether a is spelled in s: some maximal run of address
// characters (hex digits, "." and ":") of s, or such a run without its
// last ":" and what follows (an IPv4 port), parses with netip.ParseAddr to
// a once unmapped. It shares no code with the package's splitters and
// parsers, so a bug they have in common with a reference cannot hide a
// fabricated address.
func appearsIn(a netip.Addr, s string) bool {
	runs := strings.FieldsFunc(s, func(r rune) bool {
		return !strings.ContainsRune("0123456789abcdefABCDEF.:", r)
	})
	for _, run := range runs {
		for _, c := range []string{run, run[:max(0, strings.LastIndexByte(run, ':'))]} {
			if p, err := netip.ParseAddr(c); err == nil && p.Unmap() == a {
				return true
			}
		}
	}
	return false
}

// withLast returns lines with suffix appended to the last line (a proxy
// appending to the client's field line) and, separately, with suffix as a
// field line of its own.
func withLast(lines []string, suffix string) (same, own []string) {
	same = append([]string(nil), lines...)
	same[len(same)-1] += suffix
	own = append(append([]string(nil), lines...), strings.TrimPrefix(suffix, ", "))
	return same, own
}

// checkResult compares Resolve with the reference and checks the address
// form.
func checkResult(t *testing.T, r Result, peer netip.AddrPort, want netip.Addr, fromHeader bool, via Via) {
	t.Helper()
	if !fromHeader {
		if r.IP != peer.Addr() || r.Port != peer.Port() || r.Via != ViaPeer {
			t.Fatalf("Resolve = %+v, want the peer %v", r, peer)
		}
		return
	}
	if r.IP != want || r.Port != 0 || r.Via != via {
		t.Fatalf("Resolve = %+v, want %v via %v", r, want, via)
	}
	if !r.IP.IsValid() || r.IP.Is4In6() || r.IP.Zone() != "" {
		t.Fatalf("Resolve returned %v", r.IP)
	}
}

// FuzzForwardedFor (06 6.4; 04's FuzzSourceAddr): Resolve over arbitrary
// X-Forwarded-For lines equals the reference walk.
func FuzzForwardedFor(f *testing.F) {
	for _, s := range []string{
		"1.1.1.1, 10.0.0.2",
		"6.6.6.6, 1.1.1.1, 10.0.0.2",
		"10.0.0.3, 10.0.0.2",
		"1.1.1.1, unknown, 10.0.0.2",
		"1.1.1.1\n10.0.0.2",
		"[2001:db8::1]:443, fe80::1%eth0, ::ffff:10.0.0.2",
		"1.1.1.1,, 10.0.0.2 ,",
		strings.Repeat("10.0.0.1, ", 20) + "10.0.0.2",
		"1.2.3.4:65536, [::1",
		"",
	} {
		f.Add(s)
	}
	tr := testTrusted()
	peer := netip.MustParseAddrPort("10.0.0.1:443")
	client := netip.MustParseAddr("198.51.100.7")
	f.Fuzz(func(t *testing.T, s string) {
		lines := strings.Split(s, "\n")
		r := tr.Resolve(peer, http.Header{"X-Forwarded-For": lines})
		want, ok := refWalk(tr, refXFF(lines))
		checkResult(t, r, peer, want, ok, ViaXForwardedFor)

		// Properties that use no code of the package (06 req 60):
		// an address taken from the header is spelled in it, ...
		if r.FromHeader() && !appearsIn(r.IP, s) {
			t.Fatalf("%q resolved to %v, which it does not contain", s, r.IP)
		}
		// ... and whatever a client sent left of a trusted proxy's
		// entries, the rightmost untrusted literal entry is the client,
		// on the client's line or on a line of its own.
		for _, suffix := range []string{", 198.51.100.7", ", 198.51.100.7, 10.0.0.2", ", [::ffff:198.51.100.7]:80, 10.0.0.2"} {
			same, own := withLast(lines, suffix)
			for _, l := range [][]string{same, own} {
				if r := tr.Resolve(peer, http.Header{"X-Forwarded-For": l}); r.IP != client || r.Via != ViaXForwardedFor {
					t.Fatalf("X-Forwarded-For %q resolved to %v via %v, want %v", l, r.IP, r.Via, client)
				}
			}
		}
	})
}

// FuzzForwarded (06 6.4): Resolve over arbitrary Forwarded lines equals
// the reference walk, and a Forwarded field with content always wins over
// X-Forwarded-For.
func FuzzForwarded(f *testing.F) {
	for _, s := range []string{
		`for="[2001:db8::1]:4711"`,
		"for=192.0.2.60;proto=http;by=203.0.113.43",
		"for=6.6.6.6, for=1.1.1.1, for=10.0.0.2",
		`for=1.1.1.1;host="a,b", for=10.0.0.2`,
		`for=1.1.1.1;host="a\",b", for=10.0.0.2`,
		`for="1.1.1\.1"`,
		"for=1.1.1.1, for=unknown, for=10.0.0.2",
		`for=1.1.1.1, for="_hidden"`,
		"for=1.1.1.1, proto=https",
		"for=1.1.1.1;for=2.2.2.2",
		"for=\"1.1.1.1\nfor=2.2.2.2",
		`for="1.1.1.1, for=203.0.113.9`,
		`x="\, for=203.0.113.9`,
		`for="1.1.1.1, for=203.0.113.9;host="a\",b"`,
		`for=10.0.0.2\"`,
		"for=1.1.1.1:8080, for=[2001:db8::1], , ;;",
		strings.Repeat("for=10.0.0.1, ", 20) + "for=10.0.0.2",
		" ",
	} {
		f.Add(s)
	}
	tr := testTrusted()
	peer := netip.MustParseAddrPort("10.0.0.1:443")
	client := netip.MustParseAddr("203.0.113.9")
	f.Fuzz(func(t *testing.T, s string) {
		lines := strings.Split(s, "\n")

		// Properties that use no code of the package (06 req 60, 04 req
		// 17): whatever the client sent, an element a trusted proxy
		// appends (to the client's line or on its own line, quoted
		// commas and quoted-pairs included) is found, ...
		for _, suffix := range []string{
			", for=203.0.113.9",
			`, for=203.0.113.9;host="a,b"`,
			`, for="203.0.113.9:443";host="a\",b\\"`,
			`, for="[::ffff:203.0.113.9]";proto=https, for=10.0.0.2`,
		} {
			same, own := withLast(lines, suffix)
			for _, l := range [][]string{same, own} {
				if r := tr.Resolve(peer, http.Header{"Forwarded": l}); r.IP != client || r.Via != ViaForwarded {
					t.Fatalf("Forwarded %q resolved to %v via %v, want %v", l, r.IP, r.Via, client)
				}
			}
		}

		r := tr.Resolve(peer, http.Header{"Forwarded": lines, "X-Forwarded-For": {"2.2.2.2"}})
		if !hasContent(lines) {
			checkResult(t, r, peer, netip.MustParseAddr("2.2.2.2"), true, ViaXForwardedFor)
			return
		}
		// ... and an address taken from the header is spelled in it
		// (quoted-pair backslashes removed).
		if r.FromHeader() && !appearsIn(r.IP, strings.ReplaceAll(s, `\`, "")) {
			t.Fatalf("%q resolved to %v, which it does not contain", s, r.IP)
		}
		want, ok := refWalk(tr, refForwarded(lines))
		checkResult(t, r, peer, want, ok, ViaForwarded)
	})
}
