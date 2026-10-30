// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Certificate rotation (docs/architecture/08-security-and-identity.md
// "Certificate rotation"): listener and Upstream certificates from file
// rotate by watch without a Rollout, reaching new connections; a rotation
// that fails keeps the last value and counts in
// ruralz_config_secret_rotation_failures_total, raising
// secret_rotation_failed until a valid value returns.

// leafSerial returns the serial of the first certificate in a PEM file,
// the canary line before it skipped.
func leafSerial(t *testing.T, certPEM []byte) *big.Int {
	t.Helper()
	for rest := certPEM; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			t.Fatal("no certificate in the PEM file")
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c.SerialNumber
	}
}

func TestListenerCertificateRotation(t *testing.T) {
	top := newTopology(t, topology.Config{})
	mock := startMock(t, top, mockup.Config{})
	secrets := listenerSecrets(t, top)
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
		Bundle:  bundle(gatewayDoc(secureListeners), upstreamDoc("backend", mock.Addr(), ""), routeDoc("r", "/", "backend")),
		Secrets: secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	n := nodes[0]
	// served returns the serial of the certificate a new connection to
	// the secure listener is served.
	served := func() *big.Int {
		t.Helper()
		resp, err := tlsClient(t, top, nil, false).Get(n.URL("secure") + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK || resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
			t.Fatalf("GET over TLS = %d, TLS %v", resp.StatusCode, resp.TLS != nil)
		}
		return resp.TLS.PeerCertificates[0].SerialNumber
	}
	waitServed := func(want *big.Int) {
		t.Helper()
		waitFor(t, "the rotated listener certificate", func() (bool, string) {
			got := served()
			return got.Cmp(want) == 0, "serial " + got.String()
		})
	}
	if got, want := served(), leafSerial(t, secrets[refListenerCert]); got.Cmp(want) != 0 {
		t.Fatalf("served serial %v, want the first certificate's %v", got, want)
	}
	certPath, _ := n.SecretPath(refListenerCert)
	keyPath, _ := n.SecretPath(refListenerKey)
	rotate := func() *big.Int {
		t.Helper()
		cert, key, err := top.ServerCertificate()
		if err != nil {
			t.Fatal(err)
		}
		replaceFile(t, keyPath, key)
		replaceFile(t, certPath, cert)
		advance(t, top, pollInterval)
		return leafSerial(t, cert)
	}
	second := rotate()
	waitServed(second)

	failures := freshCounter(t, top, n, "ruralz_config_secret_rotation_failures_total", "provider", "file")
	replaceFile(t, certPath, top.WithCanary([]byte(notACertificate)))
	advance(t, top, pollInterval)
	waitCounter(t, top, n, failures+1, "ruralz_config_secret_rotation_failures_total", "provider", "file")
	waitDegraded(t, n, "secret_rotation_failed", true)
	if got := served(); got.Cmp(second) != 0 {
		t.Fatalf("served serial %v after a failed rotation, want the last valid %v", got, second)
	}

	third := rotate()
	waitServed(third)
	waitDegraded(t, n, "secret_rotation_failed", false)
	wantRevision(t, n, n.TestDigest())
}

// clientRecorder records the common name of every client certificate a
// TLS server verified.
type clientRecorder struct {
	mu    sync.Mutex
	names []string
}

func (r *clientRecorder) verify(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return fmt.Errorf("no client certificate")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, cs.PeerCertificates[0].Subject.CommonName)
	return nil
}

func (r *clientRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.names) == 0 {
		return ""
	}
	return r.names[len(r.names)-1]
}

// Secret references of the Upstream TLS case.
const (
	refUpstreamCA   = "file:/etc/ruralz/upstream/ca.pem"
	refUpstreamCert = "file:/etc/ruralz/upstream/client.crt"
	refUpstreamKey  = "file:/etc/ruralz/upstream/client.key"
)

func TestUpstreamCertificateRotation(t *testing.T) {
	top := newTopology(t, topology.Config{})
	clientCA := newCA(t, "Ruralz upstream client CA")
	serverTLS, err := top.ServerTLS()
	if err != nil {
		t.Fatal(err)
	}
	rec := &clientRecorder{}
	serverTLS.ClientAuth, serverTLS.ClientCAs, serverTLS.VerifyConnection = tls.RequireAndVerifyClientCert, clientCA.Pool(), rec.verify
	// Connection: close makes every request a new connection, which is
	// what a rotated client certificate reaches.
	mock := startMock(t, top, mockup.Config{TLS: serverTLS, Behavior: mockup.Behavior{
		Body: []byte("upstream says hello"), ContentType: "text/plain", Header: http.Header{"Connection": {"close"}},
	}})
	client := func(cn string) (certPEM, keyPEM []byte) {
		l := issue(t, clientCA, pki.LeafOptions{CommonName: cn, Client: true})
		return top.WithCanary(l.ChainPEM), top.WithCanary(l.KeyPEM)
	}
	cert, key := client("edge-1")
	tlsBlock := "  tls:\n    caCertificate: " + fileRef("/etc/ruralz/upstream/ca.pem") +
		"\n    clientCertificate: " + fileRef("/etc/ruralz/upstream/client.crt") +
		"\n    clientKey: " + fileRef("/etc/ruralz/upstream/client.key") + "\n"
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
		Bundle:  bundle(gatewayDoc(webListener), upstreamDoc("backend", mock.Addr(), tlsBlock), routeDoc("r", "/", "backend")),
		Secrets: map[string][]byte{refUpstreamCA: top.WithCanary(top.CA().CertPEM), refUpstreamCert: cert, refUpstreamKey: key},
	})
	if err != nil {
		t.Fatal(err)
	}
	n := nodes[0]
	c := top.HTTPClient()
	wantOK(t, "request over Upstream mTLS", get(t, c, n.URL("web")+"/"))
	if got := rec.last(); got != "edge-1" {
		t.Fatalf("the Upstream verified %q, want edge-1", got)
	}
	certPath, _ := n.SecretPath(refUpstreamCert)
	keyPath, _ := n.SecretPath(refUpstreamKey)
	cert, key = client("edge-2")
	replaceFile(t, keyPath, key)
	replaceFile(t, certPath, cert)
	advance(t, top, pollInterval)
	waitFor(t, "the rotated client certificate", func() (bool, string) {
		wantOK(t, "request during the rotation", get(t, c, n.URL("web")+"/"))
		return rec.last() == "edge-2", "presented " + rec.last()
	})

	// A key that pairs with no certificate keeps the last pair.
	failures := freshCounter(t, top, n, "ruralz_config_secret_rotation_failures_total", "provider", "file")
	_, stray := client("stray")
	replaceFile(t, keyPath, stray)
	advance(t, top, pollInterval)
	waitCounter(t, top, n, failures+1, "ruralz_config_secret_rotation_failures_total", "provider", "file")
	waitDegraded(t, n, "secret_rotation_failed", true)
	wantOK(t, "request after the failed rotation", get(t, c, n.URL("web")+"/"))
	if got := rec.last(); got != "edge-2" {
		t.Fatalf("the Upstream verified %q after a failed rotation, want edge-2", got)
	}
	wantRevision(t, n, n.TestDigest())
}
