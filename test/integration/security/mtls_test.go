// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// mTLS (docs/architecture/08-security-and-identity.md "Mutual TLS",
// "Basic and mTLS schemas", "Certificate rotation"): a listener requests
// an unverified client certificate when one of its Routes attaches
// auth.mtls, and the Filter verifies the chain against its own anchors,
// its CRL and its subject rules; an auth.mtls Route reached over a
// connection that requested no certificate is 421 RZ-AUTH-008; a leaf
// binds the Consumer naming its subject; the CA bundle rotates by watch,
// and a CA a rotation removed stops matching at once, on an open
// connection too.

// pollInterval is how often a Node polls its file secrets (2 s target).
const pollInterval = 2 * time.Second

// Secret references of the TLS cases.
const (
	refListenerCert = "file:/etc/ruralz/tls/tls.crt"
	refListenerKey  = "file:/etc/ruralz/tls/tls.key"
	refAnyCA        = "file:/etc/ruralz/mtls/any-ca.pem"
	refAnyCRL       = "file:/etc/ruralz/mtls/any-crl.pem"
	refAliceCA      = "file:/etc/ruralz/mtls/alice-ca.pem"
)

// secureListeners are the cleartext listener web and the https listener
// secure on 8443 serving refListenerCert and refListenerKey.
const secureListeners = webListener + "    - name: secure\n      protocol: https\n      port: 8443\n      tls:\n        certificates:\n          - name: main\n" +
	"            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.crt}}\n" +
	"            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.key}}\n"

// listenerSecrets returns the secure listener's certificate and key from
// the harness's CA, each carrying a canary.
func listenerSecrets(t *testing.T, top *topology.Topology) map[string][]byte {
	t.Helper()
	cert, key, err := top.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{refListenerCert: cert, refListenerKey: key}
}

// tlsClient returns a client trusting the harness's CA that presents
// leaf when it is not nil; keepAlive keeps its connection open across
// requests. The bodies it reads are captured for the leak scan.
func tlsClient(t *testing.T, top *topology.Topology, leaf *pki.Leaf, keepAlive bool) *http.Client {
	t.Helper()
	cfg := top.ClientTLS()
	if leaf != nil {
		cert, err := leaf.TLSCertificate()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	tr := &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: !keepAlive, ForceAttemptHTTP2: true}
	t.Cleanup(tr.CloseIdleConnections)
	return capturing(top, tr)
}

// replaceFile writes data to path through a temporary file and a rename,
// as a secret rotation does.
func replaceFile(t *testing.T, path string, data []byte) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".new")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// issue signs a leaf with ca.
func issue(t *testing.T, ca *pki.CA, o pki.LeafOptions) *pki.Leaf {
	t.Helper()
	l, err := ca.Issue(o)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// newCA returns a root CA named cn.
func newCA(t *testing.T, cn string) *pki.CA {
	t.Helper()
	ca, err := pki.NewCA(pki.CAOptions{CommonName: cn})
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestMTLS(t *testing.T) {
	top := newTopology(t, topology.Config{})
	clientCA, otherCA := newCA(t, "Ruralz mTLS client CA"), newCA(t, "Ruralz mTLS other CA")
	alice := issue(t, clientCA, pki.LeafOptions{CommonName: "alice", Client: true})
	bob := issue(t, clientCA, pki.LeafOptions{CommonName: "bob", Client: true})
	mallory := issue(t, clientCA, pki.LeafOptions{CommonName: "mallory", Client: true})
	serverOnly := issue(t, clientCA, pki.LeafOptions{CommonName: "carol", Server: true})
	otherAlice := issue(t, otherCA, pki.LeafOptions{CommonName: "alice", Client: true})
	crl, err := clientCA.CRL([]*x509.Certificate{mallory.Cert}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	aliceSubject := alice.Cert.Subject.String() // CN=alice,O=Ruralz tests

	mock := startMock(t, top, mockup.Config{})
	secrets := listenerSecrets(t, top)
	secrets[refAnyCA] = top.WithCanary(clientCA.CertPEM)
	secrets[refAnyCRL] = top.WithCanary(crl)
	secrets[refAliceCA] = top.WithCanary(clientCA.CertPEM)
	b := bundle(
		gatewayDoc(secureListeners),
		upstreamDoc("backend", mock.Addr(), ""),
		policyDoc("mtls-any", "auth.mtls", "    caCertificate: "+fileRef("/etc/ruralz/mtls/any-ca.pem")+"\n    crl: "+fileRef("/etc/ruralz/mtls/any-crl.pem")+"\n"),
		policyDoc("mtls-alice", "auth.mtls", "    caCertificate: "+fileRef("/etc/ruralz/mtls/alice-ca.pem")+"\n    subjects:\n      - subject: \""+aliceSubject+"\"\n"),
		policyDoc("consumer-header", "headers", "    request:\n      set:\n        - name: x-consumer\n          valueExpression: 'consumer == null ? \"none\" : consumer.name'\n"),
		"apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: alice-app\nspec:\n  credentials:\n    certificates:\n      - name: alice\n        subject: \""+aliceSubject+"\"\n",
		routeDoc("any", "/any", "backend", "mtls-any", "consumer-header"),
		routeDoc("alice", "/alice", "backend", "mtls-alice"),
		routeDoc("open", "/open", "backend"),
	)
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: b, Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	n := nodes[0]
	secure, web := n.URL("secure"), n.URL("web")

	// lastConsumer returns the x-consumer header of the last request the
	// mock Upstream received.
	lastConsumer := func() string {
		rs := mock.Requests()
		if len(rs) == 0 {
			return ""
		}
		return rs[len(rs)-1].Header.Get("x-consumer")
	}

	t.Run("verified leaf binds its Consumer", func(t *testing.T) {
		wantOK(t, "alice on /any", get(t, tlsClient(t, top, alice, false), secure+"/any"))
		if got := lastConsumer(); got != "alice-app" {
			t.Fatalf("x-consumer at the Upstream = %q, want alice-app", got)
		}
		wantOK(t, "bob on /any", get(t, tlsClient(t, top, bob, false), secure+"/any"))
		if got := lastConsumer(); got != "none" {
			t.Fatalf("x-consumer for a leaf no Consumer names = %q, want none", got)
		}
	})
	t.Run("subject rules", func(t *testing.T) {
		wantOK(t, "alice on /alice", get(t, tlsClient(t, top, alice, false), secure+"/alice"))
		wantCode(t, "bob on /alice", get(t, tlsClient(t, top, bob, false), secure+"/alice"), http.StatusUnauthorized, "RZ-AUTH-002")
	})
	t.Run("refusals", func(t *testing.T) {
		wantCode(t, "revoked leaf", get(t, tlsClient(t, top, mallory, false), secure+"/any"), http.StatusUnauthorized, "RZ-AUTH-004")
		wantCode(t, "leaf of another CA", get(t, tlsClient(t, top, otherAlice, false), secure+"/any"), http.StatusUnauthorized, "RZ-AUTH-002")
		wantCode(t, "leaf without clientAuth", get(t, tlsClient(t, top, serverOnly, false), secure+"/any"), http.StatusUnauthorized, "RZ-AUTH-002")
		wantCode(t, "no certificate", get(t, tlsClient(t, top, nil, false), secure+"/any"), http.StatusUnauthorized, "RZ-AUTH-001")
		wantOK(t, "Route without auth.mtls, no certificate", get(t, tlsClient(t, top, nil, false), secure+"/open"))
	})
	t.Run("connection that requested no certificate", func(t *testing.T) {
		wantCode(t, "auth.mtls Route over cleartext", get(t, top.HTTPClient(), web+"/any"), http.StatusMisdirectedRequest, "RZ-AUTH-008")
		wantOK(t, "Route without auth.mtls over cleartext", get(t, top.HTTPClient(), web+"/open"))
	})
	t.Run("CA rotation", func(t *testing.T) {
		// A client keeping one connection open: its cached verdict ends
		// with the CA that the rotation removes.
		kept := tlsClient(t, top, alice, true)
		wantOK(t, "alice before the rotation", get(t, kept, secure+"/alice"))
		path, ok := n.SecretPath(refAliceCA)
		if !ok {
			t.Fatal("no file for the alice CA")
		}
		replaceFile(t, path, top.WithCanary(otherCA.CertPEM))
		advance(t, top, pollInterval)
		waitFor(t, "the rotated CA", func() (bool, string) {
			r := get(t, kept, secure+"/alice")
			return r.status == http.StatusUnauthorized && r.code == "RZ-AUTH-002", r.String()
		})
		wantOK(t, "leaf of the new CA", get(t, tlsClient(t, top, otherAlice, false), secure+"/alice"))
		// A bundle that does not parse keeps the last CA and counts a
		// rotation failure.
		failures := freshCounter(t, top, n, "ruralz_config_secret_rotation_failures_total", "provider", "file")
		replaceFile(t, path, top.WithCanary([]byte(notACertificate)))
		advance(t, top, pollInterval)
		waitCounter(t, top, n, failures+1, "ruralz_config_secret_rotation_failures_total", "provider", "file")
		waitDegraded(t, n, "secret_rotation_failed", true)
		wantOK(t, "new CA kept after the failed rotation", get(t, tlsClient(t, top, otherAlice, false), secure+"/alice"))
		wantCode(t, "old CA still refused", get(t, tlsClient(t, top, alice, false), secure+"/alice"), http.StatusUnauthorized, "RZ-AUTH-002")
		// Rotations need no Rollout: the Revision stays active.
		wantRevision(t, n, n.TestDigest())
	})
}

// notACertificate is a PEM block that parses as PEM but holds no
// certificate.
const notACertificate = "-----BEGIN CERTIFICATE-----\nbm90IGEgY2VydGlmaWNhdGU=\n-----END CERTIFICATE-----\n"

// wantRevision fails t unless /readyz answers 200 naming the test digest.
func wantRevision(t *testing.T, n *topology.Node, testDigest string) {
	t.Helper()
	doc, status, err := n.Readyz(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := topology.DisplayRevision(testDigest); status != http.StatusOK || doc.Revision != want {
		t.Fatalf("/readyz = %d %+v, want 200 with revision %s", status, doc, want)
	}
}
