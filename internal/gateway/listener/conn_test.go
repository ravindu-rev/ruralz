// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// countingConn counts the writes that reach it.
type countingConn struct {
	net.Conn
	writes atomic.Int32
}

func (c *countingConn) Write(b []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(b)
}

// TestTimedOutWriteEndsWrites: once a write on a connection timed out,
// every later write fails at once with its error without reaching the
// socket, so closing a TLS connection after net/http's failed response
// write sends no close_notify, which a peer that reads nothing would hold
// for crypto/tls's 5 s deadline (docs/architecture/03-data-plane.md
// "Bounded resources").
func TestTimedOutWriteEndsWrites(t *testing.T) {
	p := newTestPKI(t)
	leaf, err := p.ca.Issue(pki.LeafOptions{CommonName: "node", IPAddresses: ipsOf([]string{"127.0.0.1"}), Server: true})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := leaf.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	type server struct {
		inner *countingConn
		rec   *Conn
		tls   *tls.Conn
		err   error
	}
	accepted := make(chan server, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			accepted <- server{err: err}
			return
		}
		inner := &countingConn{Conn: nc}
		rec := newConn(inner, "main", newFakeMetrics().m, 0, time.Time{}, true, false)
		srv := tls.Server(rec, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		accepted <- server{inner: inner, rec: rec, tls: srv, err: srv.HandshakeContext(t.Context())}
	}()
	cli, err := (&tls.Dialer{Config: &tls.Config{RootCAs: p.roots(), MinVersion: tls.VersionTLS12}}).DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	s := <-accepted
	if s.err != nil {
		t.Fatal(s.err)
	}
	// A deadline in the past times the next write out at once; the client
	// reads nothing from here on.
	if err := s.tls.SetWriteDeadline(time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.tls.Write([]byte("response")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write past its deadline = %v, want a timeout", err)
	}
	writes := s.inner.writes.Load()
	if err := s.rec.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.rec.Write([]byte("more")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("write after a timed-out write, deadline lifted = %v, want its timeout", err)
	}
	_ = s.tls.Close()
	if got := s.inner.writes.Load() - writes; got != 0 {
		t.Errorf("%d write(s) reached the socket after the timed-out one, want none: Close wrote close_notify", got)
	}
}

// TestCleartextConnectionMetrics checks the http listener's connection
// metrics: an HTTP/1.1 connection counts accepted http1, an h2c one
// accepted http2 and moves from the http1 to the http2 open connections;
// closing leaves the open connections.
func TestCleartextConnectionMetrics(t *testing.T) {
	m := newFakeMetrics()
	l := httpListener("web", 0)
	l.Metrics = m.m
	h := newHarness(t)
	h.activate(t, snap(l), nil)
	addr := h.addr(t, "web")

	c := dialRaw(t, addr, nil)
	c.mustRoundTrip(t)
	if m.openConns(emit.ProtoHTTP1) != 1 || m.count(emit.ProtoHTTP1, emit.ConnAccepted) != 1 {
		t.Fatalf("HTTP/1.1: open %d, accepted %d; want 1, 1", m.openConns(emit.ProtoHTTP1), m.count(emit.ProtoHTTP1, emit.ConnAccepted))
	}
	_ = c.Close()
	waitFor(t, "the closed connection to leave", func() bool { return m.openConns(emit.ProtoHTTP1) == 0 })

	h2c, _ := client(t, nil, false, false, true)
	if _, body := get(t, h2c, "http://"+addr+"/"); field(body, 0) != "HTTP/2.0" {
		t.Fatalf("h2c: %q", body)
	}
	if m.openConns(emit.ProtoHTTP2) != 1 || m.openConns(emit.ProtoHTTP1) != 0 || m.count(emit.ProtoHTTP2, emit.ConnAccepted) != 1 {
		t.Fatalf("h2c: open http2 %d, http1 %d, accepted http2 %d; want 1, 0, 1",
			m.openConns(emit.ProtoHTTP2), m.openConns(emit.ProtoHTTP1), m.count(emit.ProtoHTTP2, emit.ConnAccepted))
	}
	h2c.CloseIdleConnections()
	waitFor(t, "the h2c connection to leave", func() bool { return m.openConns(emit.ProtoHTTP2) == 0 })
	if got := m.count(emit.ProtoHTTP1, emit.ConnAccepted); got != 1 {
		t.Errorf("accepted http1 = %d, want 1: each connection counts once", got)
	}
}

// TestTLSConnectionMetrics checks the https listener's connection
// metrics: a completed handshake counts accepted with its ALPN protocol
// and observes the handshake duration; a handshake the client aborts and
// a cleartext request (answered by net/http's 400) count tls_failure under
// http1.
func TestTLSConnectionMetrics(t *testing.T) {
	p := newTestPKI(t)
	m := newFakeMetrics()
	l := httpsListener("api", false, p.cert(t, "api", "127.0.0.1"))
	l.Metrics = m.m
	h := newHarness(t)
	h.activate(t, snap(l), p.secrets)
	addr := h.addr(t, "api")

	h2, _ := client(t, &tls.Config{RootCAs: p.roots(), MinVersion: tls.VersionTLS12}, false, true, false)
	get(t, h2, "https://"+addr+"/")
	waitFor(t, "the handshake to count", func() bool { return m.count(emit.ProtoHTTP2, emit.ConnAccepted) == 1 })
	if n := m.handshake.n.Load(); n != 1 {
		t.Errorf("handshake observations = %d, want 1", n)
	}
	if m.openConns(emit.ProtoHTTP2) != 1 {
		t.Errorf("open http2 = %d, want 1", m.openConns(emit.ProtoHTTP2))
	}

	untrusting := tls.Dialer{Config: &tls.Config{MinVersion: tls.VersionTLS12}}
	if c, err := untrusting.DialContext(t.Context(), "tcp", addr); err == nil {
		_ = c.Close()
		t.Fatal("a client without the test CA completed the handshake")
	}
	waitFor(t, "the aborted handshake to count", func() bool { return m.count(emit.ProtoHTTP1, emit.ConnTLSFailure) == 1 })

	plain := dialRaw(t, addr, nil)
	r, err := plain.roundTrip(t.Context(), "/", nil)
	if err != nil || r.status != http.StatusBadRequest || !strings.Contains(r.body, "HTTP request to an HTTPS server") {
		t.Errorf("cleartext request to https: %d %q, %v; want net/http's 400", r.status, r.body, err)
	}
	waitFor(t, "the cleartext request to count", func() bool { return m.count(emit.ProtoHTTP1, emit.ConnTLSFailure) == 2 })
	if got := m.count(emit.ProtoHTTP1, emit.ConnAccepted) + m.count(emit.ProtoHTTP2, emit.ConnAccepted); got != 1 {
		t.Errorf("accepted = %d, want 1", got)
	}
	if n := m.handshake.n.Load(); n != 1 {
		t.Errorf("handshake observations = %d, want 1: failures observe no duration", n)
	}
}

// TestProxyProtocolListener checks proxyProtocol: true: a connection
// opening with a PROXY v2 header is served with the header's source as its
// peer; one without the header is closed without a response and counts
// refused, never accepted.
func TestProxyProtocolListener(t *testing.T) {
	m := newFakeMetrics()
	l := httpListener("edge", 0)
	l.ProxyProtocol = true
	l.Metrics = m.m
	h := newHarness(t)
	h.activate(t, snap(l), nil)
	addr := h.addr(t, "edge")

	src := netip.MustParseAddrPort("192.0.2.10:4321")
	withHeader := dialRaw(t, addr, proxyV2(src, netip.MustParseAddrPort("198.51.100.1:443")))
	if r := withHeader.mustRoundTrip(t); field(r.body, 4) != src.String() {
		t.Errorf("peer = %q, want the PROXY v2 source %s", field(r.body, 4), src)
	}

	bare := dialRaw(t, addr, nil)
	if _, err := bare.roundTrip(t.Context(), "/", nil); err == nil {
		t.Fatal("a connection without a PROXY v2 header got a response")
	}
	waitFor(t, "the refusal to count", func() bool { return m.count(emit.ProtoHTTP1, emit.ConnRefused) == 1 })
	_ = withHeader.Close()
	waitFor(t, "both connections to close", func() bool { return m.openConns(emit.ProtoHTTP1) == 0 })
	if got := m.count(emit.ProtoHTTP1, emit.ConnAccepted); got != 1 {
		t.Errorf("accepted = %d, want 1: a refused connection is not accepted", got)
	}
}

// TestProxyProtocolOverTLS checks proxyProtocol on an https listener: the
// header precedes the TLS handshake, whose connection then reports the
// header's source.
func TestProxyProtocolOverTLS(t *testing.T) {
	p := newTestPKI(t)
	l := httpsListener("edge", false, p.cert(t, "edge", "127.0.0.1"))
	l.ProxyProtocol = true
	h := newHarness(t)
	h.activate(t, snap(l), p.secrets)

	src := netip.MustParseAddrPort("192.0.2.20:5555")
	raw := dialRaw(t, h.addr(t, "edge"), proxyV2(src, netip.MustParseAddrPort("198.51.100.1:443")))
	tc := tls.Client(raw.Conn, &tls.Config{RootCAs: p.roots(), ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
	c := &rawConn{Conn: tc, br: bufio.NewReader(tc)}
	if r := c.mustRoundTrip(t); field(r.body, 4) != src.String() {
		t.Errorf("peer = %q, want the PROXY v2 source %s", field(r.body, 4), src)
	}
}

// TestConnectionCeilingSharedAcrossListeners checks that every client
// listener counts against one Node-wide ceiling: the connections of both
// listeners fill it, and a connection waiting at the ceiling on one
// listener takes the slot a connection of the other listener frees, since
// an idle listener's accept loop holds no slot (admission.ConnLimiter).
func TestConnectionCeilingSharedAcrossListeners(t *testing.T) {
	limiter := admission.NewConnLimiter(2)
	h := newHarness(t, func(o *Options) { o.Conns = limiter })
	h.activate(t, snap(httpListener("a", 0), httpListener("b", 0)), nil)
	ca := dialRaw(t, h.addr(t, "a"), nil)
	ca.mustRoundTrip(t)
	cb := dialRaw(t, h.addr(t, "b"), nil)
	cb.mustRoundTrip(t)
	if n := limiter.Open(); n != 2 {
		t.Fatalf("open connections = %d, want 2 across both listeners", n)
	}
	third := dialRaw(t, h.addr(t, "a"), nil)
	done := make(chan string, 1)
	go func() {
		r, err := third.roundTrip(context.WithoutCancel(t.Context()), "/", nil)
		if err != nil {
			r.body = err.Error()
		}
		done <- r.body
	}()
	_ = cb.Close()
	if body := recv(t, "the third connection's response", done); field(body, 1) != "a" {
		t.Errorf("third connection: %q, want served by a once b's connection freed a slot", body)
	}
}

// TestConnFromOutsideASet checks that a context no Set served has no
// connection record.
func TestConnFromOutsideASet(t *testing.T) {
	if c := ConnFrom(context.Background()); c != nil {
		t.Fatalf("ConnFrom(Background) = %v, want nil", c)
	}
}
