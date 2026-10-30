// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"context"
	"crypto/tls"
	"net/http"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestTLSMinimumVersion checks tls.minVersion: the default admits TLS 1.3
// only, "1.2" admits a TLS 1.2 client too.
func TestTLSMinimumVersion(t *testing.T) {
	p := newTestPKI(t)
	cert := p.cert(t, "api", "127.0.0.1")
	tls12 := &snapshot.Listener{Name: "v12", Protocol: v1alpha1.ListenerProtocolHTTPS, TLS: &snapshot.ListenerTLS{MinVersion: tls.VersionTLS12, Certificates: []snapshot.Certificate{cert}}}
	byDefault := &snapshot.Listener{Name: "v13", Protocol: v1alpha1.ListenerProtocolHTTPS, TLS: &snapshot.ListenerTLS{Certificates: []snapshot.Certificate{cert}}}
	h := newHarness(t)
	h.activate(t, snap(tls12, byDefault), p.secrets)
	old := &tls.Config{RootCAs: p.roots(), MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12} //nolint:gosec // G402: the client offers only TLS 1.2 to probe minVersion.

	d := tls.Dialer{Config: old}
	c, err := d.DialContext(t.Context(), "tcp", h.addr(t, "v12"))
	if err != nil {
		t.Fatalf("TLS 1.2 client on minVersion 1.2: %v", err)
	}
	if v := c.(*tls.Conn).ConnectionState().Version; v != tls.VersionTLS12 {
		t.Errorf("negotiated %#04x, want TLS 1.2", v)
	}
	_ = c.Close()
	if c, err := d.DialContext(t.Context(), "tcp", h.addr(t, "v13")); err == nil {
		_ = c.Close()
		t.Fatal("TLS 1.2 client accepted by a listener with the default minimum")
	}
}

// TestCertificateBySNI checks the certificate choice: the SNI selects its
// certificate, and a handshake without SNI gets the first certificate in
// name order.
func TestCertificateBySNI(t *testing.T) {
	p := newTestPKI(t)
	h := newHarness(t)
	h.activate(t, snap(httpsListener("api", false,
		p.cert(t, "b-cert", "b.example"),
		p.cert(t, "a-cert", "a.example", "127.0.0.1"),
	)), p.secrets)
	addr := h.addr(t, "api")
	for _, tt := range []struct {
		sni, want string
	}{
		{sni: "b.example", want: "b-cert"},
		{sni: "a.example", want: "a-cert"},
		{sni: "", want: "a-cert"}, // dialing an IP sends no SNI
	} {
		c := dialTLS(t, addr, &tls.Config{RootCAs: p.roots(), ServerName: tt.sni, MinVersion: tls.VersionTLS12})
		if cn := c.Conn.(*tls.Conn).ConnectionState().PeerCertificates[0].Subject.CommonName; cn != tt.want {
			t.Errorf("SNI %q: served %q, want %q", tt.sni, cn, tt.want)
		}
	}
}

// TestTLSReloadReachesNewHandshakesOnly checks that a Hot Reload changing
// only TLS settings keeps the socket and its connections: an open
// connection keeps its negotiated certificate, new handshakes get the new
// one.
func TestTLSReloadReachesNewHandshakesOnly(t *testing.T) {
	p := newTestPKI(t)
	h := newHarness(t)
	h.activate(t, snap(httpsListener("api", false, p.cert(t, "first", "127.0.0.1"))), p.secrets)
	addr := h.addr(t, "api")
	cfg := &tls.Config{RootCAs: p.roots(), MinVersion: tls.VersionTLS12}
	before := dialTLS(t, addr, cfg)
	before.mustRoundTrip(t)

	h.activate(t, snap(httpsListener("api", false, p.cert(t, "second", "127.0.0.1"))), p.secrets)
	if got := h.addr(t, "api"); got != addr {
		t.Fatalf("address after the reload = %s, want the same socket %s", got, addr)
	}
	if r := before.mustRoundTrip(t); r.status != http.StatusOK {
		t.Errorf("open connection after the reload: status %d", r.status)
	}
	if cn := before.Conn.(*tls.Conn).ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "first" {
		t.Errorf("open connection: certificate %q, want first", cn)
	}
	after := dialTLS(t, addr, cfg)
	if cn := after.Conn.(*tls.Conn).ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "second" {
		t.Errorf("new connection: certificate %q, want second", cn)
	}
}

// TestClientCertificateRequest checks the auth.mtls request mode: a
// listener with RequestClientCert asks for an (unverified) client
// certificate and records the request on the connection; one without it
// asks for none.
func TestClientCertificateRequest(t *testing.T) {
	p := newTestPKI(t)
	h := newHarness(t)
	h.activate(t, snap(
		httpsListener("mtls", true, p.cert(t, "m", "127.0.0.1")),
		httpsListener("plain", false, p.cert(t, "p", "127.0.0.1")),
	), p.secrets)
	cfg := &tls.Config{RootCAs: p.roots(), MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{p.client(t)}}
	c, _ := client(t, cfg, true, true, false)
	for _, tt := range []struct {
		name, requested, certs string
	}{
		{name: "mtls", requested: "true", certs: "1"},
		{name: "plain", requested: "false", certs: "0"},
	} {
		_, body := get(t, c, "https://"+h.addr(t, tt.name)+"/")
		if field(body, 2) != tt.requested || field(body, 3) != tt.certs {
			t.Errorf("%s: %q, want requested %s with %s client certificates", tt.name, body, tt.requested, tt.certs)
		}
	}
}

// TestClientCertModeChangeReplacesServer checks a Hot Reload changing the
// client-certificate request mode: the socket stays, new Server shards
// take new connections, which request the certificate, and the previous
// shards shut down: an idle HTTP/1.1 connection closes, a busy one gets
// Connection: close, an HTTP/2 connection gets GOAWAY, which ends it, so
// the next request dials anew. The GOAWAY goes out after the activation
// returns, so the next request waits until the client closed the old
// connection, which it does only once it processed the GOAWAY.
func TestClientCertModeChangeReplacesServer(t *testing.T) {
	p := newTestPKI(t)
	h := newHarness(t)
	cert := p.cert(t, "api", "127.0.0.1")
	h.activate(t, snap(httpsListener("api", false, cert)), p.secrets)
	addr := h.addr(t, "api")
	cfg := &tls.Config{RootCAs: p.roots(), MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{p.client(t)}}

	idle := dialTLS(t, addr, cfg)
	idle.mustRoundTrip(t)
	busy := dialTLS(t, addr, cfg)
	busyDone := make(chan reply, 1)
	go func() {
		r, err := busy.roundTrip(context.WithoutCancel(t.Context()), "/block", nil)
		if err != nil {
			t.Errorf("busy request: %v", err)
		}
		busyDone <- r
	}()
	recv(t, "the busy request", h.handler.entered)
	h2, dials := client(t, cfg, false, true, false)
	if _, body := get(t, h2, "https://"+addr+"/"); field(body, 2) != "false" {
		t.Fatalf("before the change: %q, want no certificate request", body)
	}

	h.activate(t, snap(httpsListener("api", true, cert)), p.secrets)
	if got := h.addr(t, "api"); got != addr {
		t.Fatalf("address after the change = %s, want the same socket %s", got, addr)
	}
	if !idle.closedByPeer(t) {
		t.Error("idle HTTP/1.1 connection still open after the mode change")
	}
	h.handler.unblock()
	if r := recv(t, "the busy response", busyDone); !r.close {
		t.Errorf("busy HTTP/1.1 response: %+v, want Connection: close", r)
	}
	recv(t, "GOAWAY to end the old HTTP/2 connection", dials.closed(t, 0))
	if _, body := get(t, h2, "https://"+addr+"/"); field(body, 0) != "HTTP/2.0" || field(body, 2) != "true" || field(body, 3) != "1" {
		t.Errorf("after the change: %q, want a new HTTP/2 connection requesting the certificate", body)
	}
	if n := dials.count(); n != 2 {
		t.Errorf("HTTP/2 dials = %d, want 2 (GOAWAY ended the first connection)", n)
	}
}

// TestTLSSettingsErrors checks the codes of https settings Prepare cannot
// build: no tls block or an unknown minimum version is RZ-CFG-005, an
// unresolved certificate RZ-CFG-026; nothing is bound then.
func TestTLSSettingsErrors(t *testing.T) {
	p := newTestPKI(t)
	missing := snapshot.Certificate{
		Name:        "missing",
		Certificate: secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/tls/missing.crt"},
		PrivateKey:  secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/tls/missing.key"},
	}
	good := p.cert(t, "good", "127.0.0.1")
	for _, tt := range []struct {
		name string
		l    *snapshot.Listener
		want string
	}{
		{name: "no tls", l: &snapshot.Listener{Name: "a", Protocol: v1alpha1.ListenerProtocolHTTPS}, want: "RZ-CFG-005"},
		{name: "unknown minimum", l: &snapshot.Listener{Name: "a", Protocol: v1alpha1.ListenerProtocolHTTPS, TLS: &snapshot.ListenerTLS{MinVersion: tls.VersionTLS11, Certificates: []snapshot.Certificate{good}}}, want: "RZ-CFG-005"},
		{name: "unresolved certificate", l: httpsListener("a", false, missing), want: "RZ-CFG-026"},
		{name: "unknown protocol", l: &snapshot.Listener{Name: "a", Protocol: "tcp"}, want: "RZ-CFG-005"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			_, err := h.set.Prepare(t.Context(), snap(tt.l), p.secrets)
			if code, _ := errcode.CodeOf(err); code != tt.want {
				t.Fatalf("Prepare error = %v, want %s", err, tt.want)
			}
			if p.secrets.watches.Load() != 0 {
				t.Errorf("%d secret watches left after the failed Prepare", p.secrets.watches.Load())
			}
		})
	}
}

// TestTLSStateCarriedOver checks that a Hot Reload with unchanged TLS
// settings keeps the listener's TLS state (and its secret watches)
// instead of building another.
func TestTLSStateCarriedOver(t *testing.T) {
	p := newTestPKI(t)
	h := newHarness(t)
	cert := p.cert(t, "api", "127.0.0.1")
	h.activate(t, snap(httpsListener("api", false, cert)), p.secrets)
	before := h.endpoint(t, "api").tls.Load()
	watches := p.secrets.watches.Load()
	h.activate(t, snap(httpsListener("api", false, cert)), p.secrets)
	if h.endpoint(t, "api").tls.Load() != before || p.secrets.watches.Load() != watches {
		t.Fatal("unchanged TLS settings built a new TLS state")
	}
	h.activate(t, snap(httpsListener("api", false, cert, p.cert(t, "other", "other.example"))), p.secrets)
	if st := h.endpoint(t, "api").tls.Load(); st == before || !slices.ContainsFunc(st.certs, func(c snapshot.Certificate) bool { return c.Name == "other" }) {
		t.Fatal("changed certificates kept the old TLS state")
	}
	if got := p.secrets.watches.Load(); got != 4 {
		t.Errorf("secret watches = %d, want 4 (the replaced state stopped its 2)", got)
	}
}
