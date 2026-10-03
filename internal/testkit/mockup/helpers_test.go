// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testPKI is a self-signed certificate for 127.0.0.1 and localhost and the
// pool trusting it. The test kit's pki package is not imported: WP-84's
// packages import no Ruralz package.
type testPKI struct {
	cert tls.Certificate
	pool *x509.CertPool
}

func newTestPKI(t testing.TB) testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mockup test"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost", "upstream.test"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return testPKI{cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool: pool}
}

func (p testPKI) serverTLS() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{p.cert}}
}

// proto names a client protocol.
type proto string

const (
	h1    proto = "h1"
	h2c   proto = "h2c"
	h1TLS proto = "h1-tls"
	h2TLS proto = "h2-tls"
)

func (p proto) tls() bool { return p == h1TLS || p == h2TLS }

// wantProto is the request's Proto as the mock sees it.
func (p proto) wantProto() string {
	if p == h2c || p == h2TLS {
		return "HTTP/2.0"
	}
	return "HTTP/1.1"
}

// client returns an HTTP client speaking p; pki is used for TLS.
func client(t testing.TB, p proto, pki testPKI) *http.Client {
	t.Helper()
	protocols := new(http.Protocols)
	switch p {
	case h1, h1TLS:
		protocols.SetHTTP1(true)
	case h2c:
		protocols.SetUnencryptedHTTP2(true)
	case h2TLS:
		protocols.SetHTTP2(true)
	}
	tr := &http.Transport{Protocols: protocols, DisableCompression: true}
	if p.tls() {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pki.pool, ServerName: "upstream.test"}
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 20 * time.Second}
}

func start(t testing.TB, c Config) *Server {
	t.Helper()
	s, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// result is a completed exchange.
type result struct {
	resp    *http.Response
	body    []byte
	bodyErr error
	elapsed time.Duration
}

// do sends a request and reads the whole body; err is the round-trip
// error, bodyErr the body read error.
func do(t testing.TB, c *http.Client, method, url string, body string, header http.Header) (result, error) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	began := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return result{elapsed: time.Since(began)}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, bodyErr := io.ReadAll(resp.Body)
	return result{resp: resp, body: b, bodyErr: bodyErr, elapsed: time.Since(began)}, nil
}

func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
