// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/http2"
)

// listenersDoc is the document of the listener rules.
const listenersDoc = "docs/architecture/03-data-plane.md#listeners-and-protocols"

// TestListeners checks the rules of docs/architecture/03-data-plane.md
// "Listeners and protocols" on every client protocol.
func TestListeners(t *testing.T) {
	t.Parallel()
	g := startGateway(t, bundleOptions{})
	for _, c := range []struct {
		rule string
		run  func(t *testing.T, g *gateway)
	}{
		{"each protocol on its listener", caseProtocols},
		{"h2c by prior knowledge only", caseNoH2CUpgrade},
		{"Routes bind through listeners", caseRouteListeners},
		{"listener hostnames", caseListenerHostnames},
		{"loopback binding", caseLoopback},
		{"certificate by SNI", caseSNI},
		{"process header ceiling", caseProcessHeaderCeiling},
		{"maxRequestHeaderBytes as the RFC 9113 field-list size", caseHeaderLimit},
		{"256 field lines", caseFieldLines},
		{"conflicting framing", caseConflictingFraming},
		{"hop-by-hop fields", caseHopByHop},
		{"CONNECT matches no Route", caseConnect},
	} {
		t.Run(c.rule, func(t *testing.T) {
			t.Logf("rule: %s, %s", listenersDoc, c.rule)
			c.run(t, g)
		})
	}
}

// caseProtocols checks the protocol table: HTTP/1.1 by default, HTTP/2
// over TLS by ALPN h2 on https, h2c by prior knowledge on http; the
// Upstream sees the client hop's protocol in Via.
func caseProtocols(t *testing.T, g *gateway) {
	eachProto(t, protos(), func(t *testing.T, p proto) {
		id := g.origin.newCase()
		r := g.do(t, p, request{Target: upH1.path(bOK), Header: withCase(id)})
		wantOK(t, r)
		want := "HTTP/1.1"
		if p.isH2() {
			want = "HTTP/2.0"
		}
		if r.Proto != want {
			t.Errorf("answered over %s, want %s", r.Proto, want)
		}
		if via := g.origin.only(t, id).Header.Get("Via"); via != p.via() {
			t.Errorf("Via %q, want %q", via, p.via())
		}
	})
	t.Run("alpn-prefers-h2", func(t *testing.T) {
		c := g.dial(t, h2TLS, "h2", "http/1.1")
		defer func() { _ = c.Close() }()
		if got := c.(*tls.Conn).ConnectionState().NegotiatedProtocol; got != "h2" {
			t.Errorf("ALPN offering h2 and http/1.1 negotiated %q, want h2", got)
		}
	})
}

// caseNoH2CUpgrade checks that the http listener never upgrades to h2c:
// an HTTP/1.1 request with Upgrade: h2c is answered over HTTP/1.1, never
// with 101 Switching Protocols.
func caseNoH2CUpgrade(t *testing.T, g *gateway) {
	r := g.do(t, h1, request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase(),
		[2]string{"Connection", "Upgrade, HTTP2-Settings"}, [2]string{"Upgrade", "h2c"},
		[2]string{"HTTP2-Settings", "AAMAAABkAAQAAP__"})})
	wantOK(t, r)
	if r.Proto != "HTTP/1.1" || slices.Contains(interimStatuses(r), http.StatusSwitchingProtocols) {
		t.Errorf("response %v, want HTTP/1.1 without a switch", r)
	}
}

// caseRouteListeners checks that a Route binds through Route.spec.listeners
// (default all): /web-only/ serves on web alone, 404 RZ-RT-001 on secure.
func caseRouteListeners(t *testing.T, g *gateway) {
	eachProto(t, protos(), func(t *testing.T, p proto) {
		r := g.do(t, p, request{Target: "/web-only/" + bOK, Header: withCase(g.origin.newCase())})
		if p.isTLS() {
			wantProblem(t, r, http.StatusNotFound, "RZ-RT-001")
			return
		}
		wantOK(t, r)
	})
}

// caseListenerHostnames checks that a listener's hostnames bound it: a host
// outside them reaches no Route, 404 RZ-RT-001.
func caseListenerHostnames(t *testing.T, g *gateway) {
	for _, p := range []proto{h1TLS, h2TLS} {
		t.Run(p.String(), func(t *testing.T) {
			named := &gateway{top: g.top, node: g.node, origin: g.origin, tls: g.tls.Clone(), creds: g.creds}
			named.tls.ServerName = namedHost
			addr := g.node.Addr("named")
			do := func(host string) *response {
				ctx := guard(t, exchangeGuard)
				req := request{Target: "/named/" + bOK, Host: host, Header: withCase(g.origin.newCase())}
				nc := named.dialAddr(t, p, addr)
				if p.isH2() {
					c := newH2Conn(t, nc, h2Options{})
					defer c.close()
					c.handshake(h2Options{})
					return c.roundTrip(ctx, p, req)
				}
				c := &h1Conn{t: t, c: nc, br: bufio.NewReader(nc)}
				defer c.close()
				return c.exchange(ctx, req.method(), req.h1Bytes(addr))
			}
			wantOK(t, do(namedHost))
			wantProblem(t, do("other.example"), http.StatusNotFound, "RZ-RT-001")
		})
	}
}

// dialAddr opens a TLS connection to addr with the ALPN protocol of p.
func (g *gateway) dialAddr(t testing.TB, p proto, addr string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), dialTimeout)
	defer cancel()
	cfg := g.tls.Clone()
	cfg.NextProtos = []string{"http/1.1"}
	if p.isH2() {
		cfg.NextProtos = []string{"h2"}
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{}, Config: cfg}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return c
}

// caseLoopback checks the loopback binding of RURALZ_BIND_LOOPBACK, which
// the process harness sets: every listener and the admin listener serve on
// 127.0.0.1 and, where the host has IPv6, on ::1.
func caseLoopback(t *testing.T, g *gateway) {
	if !hasIPv6Loopback(t) {
		t.Skip("the host has no IPv6 loopback; 127.0.0.1 is checked by every other case")
	}
	for _, l := range []string{plainListener, secureListener, "named"} {
		port, ok := g.node.Port(l)
		if !ok {
			t.Fatalf("no port for listener %s", l)
		}
		conn := dialTCP(t, net.JoinHostPort("::1", strconv.Itoa(port)))
		_ = conn.Close()
	}
	conn := dialTCP(t, net.JoinHostPort("::1", strconv.Itoa(g.node.AdminPort())))
	_ = conn.Close()
	ctx := guard(t, exchangeGuard)
	nc := dialTCP(t, net.JoinHostPort("::1", strconv.Itoa(mustPort(t, g, plainListener))))
	c := &h1Conn{t: t, c: nc, br: bufio.NewReader(nc)}
	defer c.close()
	req := request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}
	wantOK(t, c.exchange(ctx, req.method(), req.h1Bytes("[::1]")))
}

// hasIPv6Loopback reports whether the host can listen on ::1.
func hasIPv6Loopback(t testing.TB) bool {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp6", "[::1]:0")
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// dialTCP opens a TCP connection to addr or fails t.
func dialTCP(t testing.TB, addr string) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), dialTimeout)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	return c
}

// mustPort returns the port of listener l.
func mustPort(t testing.TB, g *gateway, l string) int {
	t.Helper()
	port, ok := g.node.Port(l)
	if !ok {
		t.Fatalf("no port for listener %s", l)
	}
	return port
}

// caseSNI checks that an https listener picks its certificate by SNI from
// tls.certificates.
func caseSNI(t *testing.T, g *gateway) {
	for _, tt := range []struct {
		sni, want string
	}{{altHost, altHost}, {"localhost", "localhost"}} {
		t.Run(tt.sni, func(t *testing.T) {
			cfg := g.tls.Clone()
			cfg.ServerName = tt.sni
			ctx, cancel := context.WithTimeout(t.Context(), dialTimeout)
			defer cancel()
			c, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp", g.addr(h1TLS))
			if err != nil {
				t.Fatalf("handshake with SNI %s: %v", tt.sni, err)
			}
			defer func() { _ = c.Close() }()
			leaf := c.(*tls.Conn).ConnectionState().PeerCertificates[0]
			if !slices.Contains(leaf.DNSNames, tt.want) {
				t.Errorf("SNI %s got the certificate for %v", tt.sni, leaf.DNSNames)
			}
		})
	}
}

// processHeaderCeiling is MaxHeaderBytes of the client listeners, 256 KiB
// (docs/architecture/03-data-plane.md "Bounded resources", Header block).
const processHeaderCeiling = 256 << 10

// caseProcessHeaderCeiling checks that a header block above the 256 KiB
// process ceiling is refused by net/http before any handler, with a plain
// 431.
func caseProcessHeaderCeiling(t *testing.T, g *gateway) {
	eachProto(t, h1Protos(), func(t *testing.T, p proto) {
		id := g.origin.newCase()
		pad := strings.Repeat("a", 8000)
		header := withCase(id)
		for i := range processHeaderCeiling/8000 + 2 {
			header = append(header, [2]string{"X-Pad-" + strconv.Itoa(i), pad})
		}
		wantPlain(t, g.do(t, p, request{Target: upH1.path(bOK), Header: header}), http.StatusRequestHeaderFieldsTooLarge)
		if n := len(g.origin.requests(id)); n != 0 {
			t.Errorf("the origin saw %d requests", n)
		}
	})
	eachProto(t, h2Protos(), func(t *testing.T, p proto) {
		c := g.dialH2(t, p)
		defer c.close()
		limit, ok := c.settings[http2.SettingMaxHeaderListSize]
		if !ok || limit < processHeaderCeiling {
			t.Fatalf("SETTINGS_MAX_HEADER_LIST_SIZE %d (sent %t), want at least %d", limit, ok, processHeaderCeiling)
		}
		id := c.newStream()
		block := overCeilingBlock(p, g.addr(p), limit)
		if err := c.writeBlock(id, block, true); err != nil {
			t.Fatal(err)
		}
		r := c.collect(guard(t, exchangeGuard), id)
		wantPlain(t, r, http.StatusRequestHeaderFieldsTooLarge)
	})
}

// overCeilingBlock returns an HPACK block of literal fields, never indexed
// and without Huffman coding, whose header list passes limit by its last
// field only, with at least 9,000 bytes of the budget left before it: the
// crossing is decoded in the block's last frame, and every frame stays
// within the twice-the-remaining-budget bound net/http's Framer reads, so
// the server decodes the whole block and answers 431 instead of closing
// the connection as for a CONTINUATION flood.
func overCeilingBlock(p proto, authority string, limit uint32) []byte {
	var b []byte
	size := 0
	add := func(name, value string) {
		b = appendLiteral(b, name, value)
		size += len(name) + len(value) + 32
	}
	add(":method", http.MethodGet)
	add(":scheme", p.scheme())
	add(":authority", authority)
	add(":path", upH1.path(bOK))
	value := strings.Repeat("a", 4000)
	for i := 0; size+len(value)+64 <= int(limit)-9000; i++ {
		add("x-pad-"+strconv.Itoa(i), value)
	}
	add("x-last", strings.Repeat("b", int(limit)-size+100))
	return b
}

// appendLiteral appends an HPACK literal header field never indexed, with
// a literal name, both strings without Huffman coding (RFC 7541 section
// 6.2.3).
func appendLiteral(dst []byte, name, value string) []byte {
	dst = append(dst, 0x10)
	dst = appendHPACKString(dst, name)
	return appendHPACKString(dst, value)
}

// appendHPACKString appends s as an HPACK string literal without Huffman
// coding: its length as an integer with a 7-bit prefix, then its bytes
// (RFC 7541 sections 5.1 and 5.2). A length of 127 or more fills the
// prefix and continues in 7-bit groups, least significant first, each but
// the last with its high bit set: an unsigned varint.
func appendHPACKString(dst []byte, s string) []byte {
	if n := len(s); n < 127 {
		dst = append(dst, byte(n))
	} else {
		dst = binary.AppendUvarint(append(dst, 127), uint64(len(s))-127)
	}
	return append(dst, s...)
}

// fieldListSize returns the size request hardening counts for req on p
// (docs/architecture/03-data-plane.md "Listeners and protocols"): each
// field line's name and value plus 32 bytes, with :method, :scheme,
// :authority and :path counted as four field lines on every protocol.
func fieldListSize(p proto, req request, authority string) int {
	if req.Host != "" {
		authority = req.Host
	}
	size := len(":method") + len(req.method()) + len(":scheme") + len(p.scheme()) +
		len(":authority") + len(authority) + len(":path") + len(req.Target) + 4*32
	for _, f := range req.Header {
		size += len(f[0]) + len(f[1]) + 32
	}
	return size
}

// caseHeaderLimit checks limits.maxRequestHeaderBytes below the process
// ceiling: a header block of exactly the limit, counted as RFC 9113's
// field-list size with the request target, passes, and one byte more is
// 431 RZ-RT-002.
func caseHeaderLimit(t *testing.T, g *gateway) { headerLimitCases(t, g, headerLimit) }

// headerLimitCases checks the handler's header limit of g, limit bytes: a
// header block of exactly limit bytes of field-list size passes, and one
// byte more is 431 RZ-RT-002.
func headerLimitCases(t *testing.T, g *gateway, limit int) {
	eachProto(t, protos(), func(t *testing.T, p proto) {
		for _, tt := range []struct {
			name  string
			extra int
			want  int
		}{{"at the limit", 0, http.StatusOK}, {"one byte over", 1, http.StatusRequestHeaderFieldsTooLarge}} {
			id := g.origin.newCase()
			req := request{Target: upH1.path(bOK) + "?q=1", Header: withCase(id, [2]string{"X-Pad", ""})}
			pad := limit + tt.extra - fieldListSize(p, req, g.addr(p))
			req.Header[1][1] = strings.Repeat("p", pad)
			if got := fieldListSize(p, req, g.addr(p)); got != limit+tt.extra {
				t.Fatalf("built a %d-byte block, want %d", got, limit+tt.extra)
			}
			r := g.do(t, p, req)
			switch tt.want {
			case http.StatusOK:
				wantOK(t, r)
			default:
				wantProblem(t, r, tt.want, "RZ-RT-002")
				if n := len(g.origin.requests(id)); n != 0 {
					t.Errorf("%s: the origin saw %d requests", tt.name, n)
				}
			}
		}
	})
}

// TestHeaderLimitCapped checks that a limits.maxRequestHeaderBytes above
// the 256 KiB process ceiling is capped, as the degraded state
// header_limit_capped (docs/architecture/03-data-plane.md "Listeners and
// protocols"; docs/architecture/10-observability.md "Degraded states"):
// the Node raises it, and a header block one byte over the ceiling, which
// net/http lets through, is 431 RZ-RT-002 though the Revision allows 512
// KiB.
func TestHeaderLimitCapped(t *testing.T) {
	t.Parallel()
	t.Logf("rule: %s, a larger Revision value is capped", listenersDoc)
	g := startGateway(t, bundleOptions{headerLimit: "512Ki"})
	re := regexp.MustCompile(`"msg":"degraded state raised".*"reason":"header_limit_capped"`)
	if _, err := g.node.Process().WaitOutput(guard(t, exchangeGuard), re); err != nil {
		t.Fatalf("no header_limit_capped degraded state: %v", err)
	}
	headerLimitCases(t, g, processHeaderCeiling)
}

// caseFieldLines checks the 256 field line cap (target): 256 lines pass,
// 257 are 431 RZ-RT-002, pseudo-header fields excluded, and HTTP/2 cookie
// crumbs, which net/http joins into one Cookie, count as one line.
func caseFieldLines(t *testing.T, g *gateway) {
	lines := func(id string, n int, name string) [][2]string {
		h := withCase(id)
		for i := len(h); i < n; i++ {
			h = append(h, [2]string{name + strconv.Itoa(i), "v"})
		}
		return h
	}
	eachProto(t, protos(), func(t *testing.T, p proto) {
		wantOK(t, g.do(t, p, request{Target: upH1.path(bOK), Header: lines(g.origin.newCase(), 256, "X-L-")}))
		wantProblem(t, g.do(t, p, request{Target: upH1.path(bOK), Header: lines(g.origin.newCase(), 257, "X-L-")}),
			http.StatusRequestHeaderFieldsTooLarge, "RZ-RT-002")
	})
	t.Run("cookie-crumbs", func(t *testing.T) {
		crumbs := func(id string) [][2]string {
			h := withCase(id)
			for i := range 300 {
				h = append(h, [2]string{"Cookie", "c" + strconv.Itoa(i) + "=1"})
			}
			return h
		}
		eachProto(t, h2Protos(), func(t *testing.T, p proto) {
			wantOK(t, g.do(t, p, request{Target: upH1.path(bOK), Header: crumbs(g.origin.newCase())}))
		})
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			wantProblem(t, g.do(t, p, request{Target: upH1.path(bOK), Header: crumbs(g.origin.newCase())}),
				http.StatusRequestHeaderFieldsTooLarge, "RZ-RT-002")
		})
	})
}

// caseConflictingFraming checks that conflicting framing is never
// forwarded: on HTTP/1.1 net/http answers a coding other than chunked, or
// two Transfer-Encoding lines, with a plain 501, and reads a request with
// both Transfer-Encoding: chunked and Content-Length by its chunked
// framing alone, dropping Content-Length, so the Upstream request carries
// one framing of the Node's own; on HTTP/2 Transfer-Encoding makes the
// request malformed, a plain 400.
func caseConflictingFraming(t *testing.T, g *gateway) {
	eachProto(t, h1Protos(), func(t *testing.T, p proto) {
		for _, te := range []string{"Transfer-Encoding: gzip, chunked", "Transfer-Encoding: identity",
			"Transfer-Encoding: chunked\r\nTransfer-Encoding: chunked"} {
			id := g.origin.newCase()
			c := g.dialH1(t, p)
			raw := "POST " + upWire.path(bOK) + " HTTP/1.1\r\nHost: x\r\n" + caseHeader + ": " + id + "\r\n" + te + "\r\n\r\n5\r\nhello\r\n0\r\n\r\n"
			wantPlain(t, c.exchange(guard(t, exchangeGuard), http.MethodPost, []byte(raw)), http.StatusNotImplemented)
			c.close()
			if n := len(g.origin.requests(id)); n != 0 {
				t.Errorf("%q: the origin saw %d requests", te, n)
			}
		}
		// Content-Length: 3 would cut the body at "5\r\n"; the chunked
		// framing reads "hello".
		id := g.origin.newCase()
		c := g.dialH1(t, p)
		defer c.close()
		raw := "POST " + upWire.path(bOK) + " HTTP/1.1\r\nHost: x\r\n" + caseHeader + ": " + id +
			"\r\nContent-Length: 3\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n"
		r := c.exchange(guard(t, exchangeGuard), http.MethodPost, []byte(raw))
		if r.Status != http.StatusOK {
			t.Fatalf("response %v, want 200", r)
		}
		s := g.origin.only(t, id)
		var cl, te int
		for _, f := range headLines(s.RawHead) {
			switch f[0] {
			case "Content-Length":
				cl++
			case "Transfer-Encoding":
				te++
			}
		}
		if cl > 0 && te > 0 || cl > 1 || te > 1 {
			t.Errorf("the Upstream request head carries conflicting framing:\n%s", s.RawHead)
		}
		if string(s.Body) != "hello" {
			t.Errorf("the origin got body %q, want hello as the chunked framing delimits it", s.Body)
		}
	})
	// The framing crosses the hop: every Upstream transport gets the body
	// the chunked framing delimits.
	eachPairing(t, h1Protos(), func(t *testing.T, p proto, u upstream) {
		id := g.origin.newCase()
		c := g.dialH1(t, p)
		defer c.close()
		raw := "POST " + u.path(bOK) + " HTTP/1.1\r\nHost: x\r\n" + caseHeader + ": " + id +
			"\r\nContent-Length: 3\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n"
		wantOK(t, c.exchange(guard(t, exchangeGuard), http.MethodPost, []byte(raw)))
		if s := g.origin.only(t, id); string(s.Body) != "hello" {
			t.Errorf("the origin got body %q, want hello as the chunked framing delimits it", s.Body)
		}
	})
	eachProto(t, h2Protos(), func(t *testing.T, p proto) {
		id := g.origin.newCase()
		wantPlain(t, g.do(t, p, request{Method: http.MethodPost, Target: upWire.path(bOK), Body: []byte("hello"),
			Header: withCase(id, [2]string{"transfer-encoding", "chunked"})}), http.StatusBadRequest)
		if n := len(g.origin.requests(id)); n != 0 {
			t.Errorf("the origin saw %d requests", n)
		}
	})
}

// hopByHopFields are the request fields an intermediary never forwards
// (RFC 9110 section 7.6.1), besides TE, which keeps its trailers member.
func hopByHopFields() [][2]string {
	return [][2]string{
		{"Connection", "X-Client-Private, keep-alive"}, {"X-Client-Private", "secret"},
		{"Keep-Alive", "timeout=5"}, {"Proxy-Connection", "keep-alive"},
		{"Proxy-Authorization", "Basic cHJveHk6cHJveHk="}, {"Upgrade", "x-conformance/1"},
	}
}

// caseHopByHop checks that hop-by-hop fields are never forwarded, in
// either direction: the fixed ones and those a Connection field names.
func caseHopByHop(t *testing.T, g *gateway) {
	t.Run("request", func(t *testing.T) {
		eachPairing(t, h1Protos(), func(t *testing.T, p proto, u upstream) {
			id := g.origin.newCase()
			wantOK(t, g.do(t, p, request{Target: u.path(bOK), Header: withCase(id, hopByHopFields()...)}))
			h := g.origin.only(t, id).Header
			for _, f := range hopByHopFields() {
				if v, ok := h[http.CanonicalHeaderKey(f[0])]; ok {
					t.Errorf("the Upstream got %s %q", f[0], v)
				}
			}
		})
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			if r := g.do(t, p, request{Target: upWire.path(bOK), Header: withCase(id, hopByHopFields()...)}); r.Status != http.StatusOK {
				t.Fatalf("response %v, want 200", r)
			}
			raw := g.origin.only(t, id).RawHead
			for _, f := range headLines(raw) {
				for _, h := range hopByHopFields() {
					if f[0] == http.CanonicalHeaderKey(h[0]) {
						t.Errorf("the Upstream request head carries %s: %s", f[0], f[1])
					}
				}
			}
		})
		eachPairing(t, h2Protos(), func(t *testing.T, p proto, u upstream) {
			// HTTP/2 carries no connection-specific field; the Proxy-*
			// fields are the hop-by-hop fields it can carry.
			id := g.origin.newCase()
			wantOK(t, g.do(t, p, request{Target: u.path(bOK), Header: withCase(id,
				[2]string{"proxy-authorization", "Basic cHJveHk6cHJveHk="})}))
			if v, ok := g.origin.only(t, id).Header["Proxy-Authorization"]; ok {
				t.Errorf("the Upstream got Proxy-Authorization %q", v)
			}
		})
	})
	t.Run("response", func(t *testing.T) {
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			r := g.do(t, p, request{Target: u.path(bHopByHop), Header: withCase(g.origin.newCase())})
			wantOK(t, r)
			dropped := []string{"Keep-Alive", "Proxy-Authenticate", "Proxy-Connection"}
			if u != upH2 {
				// HTTP/2 has no Connection field (RFC 9113 section 8.2.2):
				// net/http's server drops it, so the field it named
				// reaches the Node as an end-to-end field.
				dropped = append(dropped, "X-Origin-Private")
			}
			for _, f := range dropped {
				if v, ok := r.Header[f]; ok {
					t.Errorf("the client got %s %q", f, v)
				}
			}
			if v := r.Header.Get("Connection"); v != "" && !strings.EqualFold(v, "close") && !strings.EqualFold(v, "keep-alive") {
				t.Errorf("the client got the Upstream's Connection %q", v)
			}
			if r.Header.Get("X-Origin-Public") != "public" {
				t.Errorf("the end-to-end field X-Origin-Public did not pass: %v", r.Header)
			}
		})
	})
}

// caseConnect checks that CONNECT matches no Route: 404 RZ-RT-001.
func caseConnect(t *testing.T, g *gateway) {
	eachProto(t, h1Protos(), func(t *testing.T, p proto) {
		wantProblem(t, g.do(t, p, request{Method: http.MethodConnect, Target: "example.test:443", Host: "example.test:443"}),
			http.StatusNotFound, "RZ-RT-001")
	})
	eachProto(t, h2Protos(), func(t *testing.T, p proto) {
		wantProblem(t, g.do(t, p, request{Pseudo: [][2]string{{":method", http.MethodConnect}, {":authority", "example.test:443"}}}),
			http.StatusNotFound, "RZ-RT-001")
	})
}
