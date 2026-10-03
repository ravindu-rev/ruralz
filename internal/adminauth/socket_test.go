// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminauth

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/problem"
)

// Real-socket tests for spec 06 requirements 94 and 95: the middleware in
// front of an admin handler served over TLS with client certificates
// verified when given (tls.VerifyClientCertIfGiven against ca.crt), over
// loopback cleartext, and over cleartext on a non-loopback interface.

// adminHandler answers 200 with the principal kind.
func adminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := PrincipalFrom(r.Context())
		_, _ = io.WriteString(w, p.Kind.String())
	})
}

// get performs a GET and returns status and body.
func get(t *testing.T, c *http.Client, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// clientWith returns a client of srv's TLS roots presenting certs; the
// server's own Client is shared, so each variant gets a cloned transport.
func clientWith(t *testing.T, srv *httptest.Server, certs ...tls.Certificate) *http.Client {
	t.Helper()
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.Certificates = certs
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

func TestSocketsTLSWithClientCertificatesReq95(t *testing.T) {
	ca := newCA(t, "operators")
	_, clientCert := ca.leaf(t, "alice", x509.ExtKeyUsageClientAuth)
	logs := &logBuffer{}
	a, err := New(Settings{
		TokenFile:        writeFile(t, t.TempDir(), "o", operatorToken),
		MetricsTokenFile: writeFile(t, t.TempDir(), "m", metricsToken),
		TLSDir:           tlsDir(t, ca, true),
	}, Options{Logger: newLogger(logs)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(a.Middleware(adminHandler()))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientCAs: ca.pool(), ClientAuth: tls.VerifyClientCertIfGiven}
	srv.StartTLS()
	defer srv.Close()

	plain := clientWith(t, srv)
	withCert := clientWith(t, srv, clientCert)

	tests := []struct {
		name   string
		c      *http.Client
		path   string
		token  string
		status int
		body   string
	}{
		{name: "certificate on /tap", c: withCert, path: "/tap", status: 200, body: "certificate"},
		{name: "certificate on /metrics", c: withCert, path: "/metrics", status: 200, body: "certificate"},
		{name: "operator token on /config/dump", c: plain, path: "/config/dump", token: operatorToken, status: 200, body: "operator"},
		{name: "metrics token on /metrics", c: plain, path: "/metrics", token: metricsToken, status: 200, body: "metrics"},
		{name: "metrics token on /tap", c: plain, path: "/tap", token: metricsToken, status: 401},
		{name: "nothing on /metrics", c: plain, path: "/metrics", status: 401},
		{name: "nothing on /healthz", c: plain, path: "/healthz", status: 200, body: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, body := get(t, tt.c, srv.URL+tt.path, tt.token)
			if status != tt.status || (tt.body != "" && body != tt.body) {
				t.Fatalf("GET %s = %d %q, want %d %q", tt.path, status, body, tt.status, tt.body)
			}
			if status == 401 {
				if _, err := problem.Decode([]byte(body)); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	if n := len(logs.lines("admin access")); n != 2 {
		t.Fatalf("%d access records:\n%s", n, logs.String())
	}

	// A certificate from another CA is not sent by default (it does not
	// match the acceptable CAs), so the request has no credential; forced
	// onto the connection, it fails the handshake.
	other := newCA(t, "strangers")
	_, strangerCert := other.leaf(t, "mallory", x509.ExtKeyUsageClientAuth)
	if status, _ := get(t, clientWith(t, srv, strangerCert), srv.URL+"/tap", ""); status != 401 {
		t.Fatalf("a certificate from another CA: %d", status)
	}
	stranger := clientWith(t, srv)
	stranger.Transport.(*http.Transport).TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return &strangerCert, nil
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/tap", nil)
	if resp, err := stranger.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a certificate from another CA was accepted: %d", resp.StatusCode)
	}
}

func TestSocketsLoopbackCleartextReq95(t *testing.T) {
	a, err := New(Settings{TokenFile: writeFile(t, t.TempDir(), "o", operatorToken)}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Middleware(adminHandler()))
	defer srv.Close()
	if status, body := get(t, srv.Client(), srv.URL+"/tap", operatorToken); status != 200 || body != "operator" {
		t.Fatalf("loopback cleartext = %d %q", status, body)
	}
}

// nonLoopbackIP returns an address of a non-loopback interface, or false.
func nonLoopbackIP() (net.IP, bool) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
			return n.IP, true
		}
	}
	return nil, false
}

func TestSocketsNonLoopbackCleartextReq95(t *testing.T) {
	ip, ok := nonLoopbackIP()
	if !ok {
		t.Skip("no non-loopback IPv4 interface")
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", ip, err)
	}
	a, err := New(Settings{
		TokenFile:        writeFile(t, t.TempDir(), "o", operatorToken),
		MetricsTokenFile: writeFile(t, t.TempDir(), "m", metricsToken),
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(a.Middleware(adminHandler()))
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	// The peer address is the interface address, not loopback: every
	// token is refused, /healthz still answers.
	for _, tok := range []string{operatorToken, metricsToken} {
		status, body := get(t, srv.Client(), srv.URL+"/metrics", tok)
		doc, err := problem.Decode([]byte(body))
		if status != 401 || err != nil || doc.Code != CodeInvalid {
			t.Fatalf("token over non-loopback cleartext = %d %s", status, body)
		}
	}
	if status, _ := get(t, srv.Client(), srv.URL+"/healthz", ""); status != 200 {
		t.Fatalf("/healthz = %d", status)
	}
}
