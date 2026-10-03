// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for the WP-26 pki scope (test CA, server and client certificates,
// ECDSA P-256 and RSA 2,048, PEM files, SSL_CERT_FILE bundle; 11 section
// 3) as used by TLS listeners, auth.mtls (06) and TLS State Stores (11 E
// 29).

// handshake runs a TLS handshake over a loopback TCP connection.
func handshake(t *testing.T, server, client *tls.Config) (serverErr, clientErr error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = c.Close() }()
		s := tls.Server(c, server)
		err = s.HandshakeContext(ctx)
		if err == nil {
			// Hold the connection until the client closes it.
			_, _ = io.Copy(io.Discard, s)
		}
		done <- err
	}()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := tls.Client(raw, client)
	clientErr = c.HandshakeContext(ctx)
	if clientErr == nil {
		// TLS 1.3 servers verify the client certificate after the client
		// finishes; a read surfaces the server's verdict.
		_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			clientErr = err
		}
	}
	_ = c.Close()
	return <-done, clientErr
}

func TestServerCertificates(t *testing.T) {
	for _, kt := range []KeyType{ECDSAP256, RSA2048} {
		t.Run(kt.String(), func(t *testing.T) {
			ca, err := NewCA(CAOptions{KeyType: kt})
			if err != nil {
				t.Fatal(err)
			}
			if !ca.Cert.IsCA || ca.Cert.Subject.CommonName != "Ruralz Test CA" || ca.Root() != ca {
				t.Fatalf("CA = %+v", ca.Cert.Subject)
			}
			leaf, err := ca.Issue(LeafOptions{CommonName: "node", DNSNames: []string{"localhost"}, KeyType: kt, Server: true})
			if err != nil {
				t.Fatal(err)
			}
			switch kt {
			case ECDSAP256:
				if _, ok := leaf.Key.(*ecdsa.PrivateKey); !ok {
					t.Fatalf("key = %T", leaf.Key)
				}
			case RSA2048:
				k, ok := leaf.Key.(*rsa.PrivateKey)
				if !ok || k.N.BitLen() != 2048 {
					t.Fatalf("key = %T", leaf.Key)
				}
				if leaf.Cert.KeyUsage&x509.KeyUsageKeyEncipherment == 0 {
					t.Fatal("RSA leaf lacks key encipherment")
				}
			}
			srv, err := leaf.ServerTLS(nil)
			if err != nil {
				t.Fatal(err)
			}
			cli, err := ClientTLS(ca, nil)
			if err != nil {
				t.Fatal(err)
			}
			cli.ServerName = "localhost"
			if se, ce := handshake(t, srv, cli); se != nil || ce != nil {
				t.Fatalf("handshake: server %v, client %v", se, ce)
			}
			cli.ServerName = "other.example"
			if _, ce := handshake(t, srv, cli); ce == nil {
				t.Fatal("wrong server name: want a verification error")
			}
		})
	}
}

func TestServerDefaultsAndHosts(t *testing.T) {
	ca, err := NewCA(CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	l, err := ca.Server()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(l.Cert.DNSNames, ",") != "localhost" || len(l.Cert.IPAddresses) != 2 {
		t.Fatalf("default SANs = %v %v", l.Cert.DNSNames, l.Cert.IPAddresses)
	}
	for _, name := range []string{"localhost", "127.0.0.1", "::1"} {
		if err := l.Cert.VerifyHostname(name); err != nil {
			t.Errorf("VerifyHostname(%s): %v", name, err)
		}
	}
	l, err = ca.Server("idp.test", "10.1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if l.Cert.Subject.CommonName != "idp.test" || l.Cert.VerifyHostname("10.1.2.3") != nil || l.Cert.VerifyHostname("idp.test") != nil {
		t.Fatalf("hosts = %v %v", l.Cert.DNSNames, l.Cert.IPAddresses)
	}
	if len(l.Cert.ExtKeyUsage) != 1 || l.Cert.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Fatalf("server EKU = %v", l.Cert.ExtKeyUsage)
	}
}

func TestMutualTLS(t *testing.T) { // auth.mtls (06): client certificates and URI SANs
	ca, err := NewCA(CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	server, err := ca.Server()
	if err != nil {
		t.Fatal(err)
	}
	client, err := ca.Client("consumer-a", "spiffe://example.org/ns/a")
	if err != nil {
		t.Fatal(err)
	}
	if len(client.Cert.URIs) != 1 || client.Cert.URIs[0].String() != "spiffe://example.org/ns/a" {
		t.Fatalf("URI SANs = %v", client.Cert.URIs)
	}
	if len(client.Cert.ExtKeyUsage) != 1 || client.Cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("client EKU = %v", client.Cert.ExtKeyUsage)
	}
	srv, err := server.ServerTLS(ca)
	if err != nil {
		t.Fatal(err)
	}
	withCert, err := ClientTLS(ca, client)
	if err != nil {
		t.Fatal(err)
	}
	withCert.ServerName = "localhost"
	if se, ce := handshake(t, srv, withCert); se != nil || ce != nil {
		t.Fatalf("mTLS handshake: server %v, client %v", se, ce)
	}
	noCert, err := ClientTLS(ca, nil)
	if err != nil {
		t.Fatal(err)
	}
	noCert.ServerName = "localhost"
	if se, _ := handshake(t, srv, noCert); se == nil {
		t.Fatal("missing client certificate: want a server error")
	}
	other, err := NewCA(CAOptions{CommonName: "Other CA"})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := other.Client("stranger")
	if err != nil {
		t.Fatal(err)
	}
	strangerCfg, err := ClientTLS(ca, stranger)
	if err != nil {
		t.Fatal(err)
	}
	strangerCfg.ServerName = "localhost"
	if se, _ := handshake(t, srv, strangerCfg); se == nil {
		t.Fatal("client certificate from another CA: want a server error")
	}
	// A client-only certificate does not serve.
	asServer, err := client.ServerTLS(nil)
	if err != nil {
		t.Fatal(err)
	}
	cli, _ := ClientTLS(ca, nil)
	cli.ServerName = "consumer-a"
	if _, ce := handshake(t, asServer, cli); ce == nil {
		t.Fatal("client certificate used as a server: want a verification error")
	}
}

func TestIntermediateChain(t *testing.T) {
	root, err := NewCA(CAOptions{KeyType: RSA2048})
	if err != nil {
		t.Fatal(err)
	}
	mid, err := root.Intermediate(CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if mid.Root() != root || mid.Cert.Subject.CommonName != "Ruralz Test Intermediate CA" {
		t.Fatalf("intermediate = %v", mid.Cert.Subject)
	}
	if err := mid.Cert.CheckSignatureFrom(root.Cert); err != nil {
		t.Fatal(err)
	}
	leaf, err := mid.Server()
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(leaf.ChainPEM), "BEGIN CERTIFICATE"); n != 2 {
		t.Fatalf("chain has %d certificates, want leaf and intermediate", n)
	}
	srv, err := leaf.ServerTLS(nil)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := ClientTLS(mid, nil) // trusts the root of mid's chain
	if err != nil {
		t.Fatal(err)
	}
	cli.ServerName = "localhost"
	if se, ce := handshake(t, srv, cli); se != nil || ce != nil {
		t.Fatalf("chain handshake: server %v, client %v", se, ce)
	}
}

func TestValidityWindow(t *testing.T) {
	ca, err := NewCA(CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(ca.Cert.NotAfter); d < DefaultValidity-2*DefaultBackdate || d > DefaultValidity {
		t.Fatalf("default validity ends in %v", d)
	}
	past := time.Now().Add(-48 * time.Hour)
	expired, err := ca.Issue(LeafOptions{DNSNames: []string{"localhost"}, NotBefore: past, NotAfter: past.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := expired.ServerTLS(nil)
	if err != nil {
		t.Fatal(err)
	}
	cli, _ := ClientTLS(ca, nil)
	cli.ServerName = "localhost"
	_, ce := handshake(t, srv, cli)
	var inv x509.CertificateInvalidError
	if !errors.As(ce, &inv) || inv.Reason != x509.Expired {
		t.Fatalf("expired certificate: client error %v", ce)
	}
}

func TestCRL(t *testing.T) { // auth.mtls CRLs (06)
	ca, err := NewCA(CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := ca.Client("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ca.Client("b")
	if err != nil {
		t.Fatal(err)
	}
	pemCRL, err := ca.CRL([]*x509.Certificate{a.Cert}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(pemCRL)
	if block == nil || block.Type != "X509 CRL" {
		t.Fatalf("CRL PEM = %q", pemCRL)
	}
	crl, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := crl.CheckSignatureFrom(ca.Cert); err != nil {
		t.Fatal(err)
	}
	if len(crl.RevokedCertificateEntries) != 1 || crl.RevokedCertificateEntries[0].SerialNumber.Cmp(a.Cert.SerialNumber) != 0 {
		t.Fatalf("revoked = %v", crl.RevokedCertificateEntries)
	}
	if a.Cert.SerialNumber.Cmp(b.Cert.SerialNumber) == 0 {
		t.Fatal("serial numbers repeat")
	}
	if !crl.NextUpdate.After(crl.ThisUpdate) {
		t.Fatal("NextUpdate not after ThisUpdate")
	}
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	ca, err := NewCA(CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewCA(CAOptions{KeyType: RSA2048})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server()
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath, err := leaf.WriteFiles(dir, "node")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(certPath) != "node.crt" || filepath.Base(keyPath) != "node.key" {
		t.Fatalf("paths = %s %s", certPath, keyPath)
	}
	st, err := os.Stat(keyPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v, %v", st.Mode(), err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.crt")
	if err := ca.WriteFile(caPath); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle.pem")
	if err := WriteBundle(bundle, ca, other); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(bundle) //nolint:gosec // G304: a file the test wrote
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) || strings.Count(string(data), "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("bundle = %q", data)
	}
	if err := WriteBundle(bundle); err == nil {
		t.Fatal("WriteBundle without CAs: want error")
	}
	missing := filepath.Join(dir, "no", "such")
	if _, _, err := leaf.WriteFiles(missing, "x"); err == nil {
		t.Fatal("WriteFiles into a missing directory: want error")
	}
	if err := ca.WriteFile(filepath.Join(missing, "ca.crt")); err == nil {
		t.Fatal("WriteFile into a missing directory: want error")
	}
	if err := WriteBundle(filepath.Join(missing, "b.pem"), ca); err == nil {
		t.Fatal("WriteBundle into a missing directory: want error")
	}
	if Env(bundle) != "SSL_CERT_FILE="+bundle {
		t.Fatal("Env")
	}
}

// TestSSLCertFileBundle checks that a child process trusts the test CA
// through SSL_CERT_FILE, the way the harness makes ruralzd and ruralz
// trust mocks and the IdP.
func TestSSLCertFileBundle(t *testing.T) {
	if os.Getenv("PKI_HELPER_LEAF") != "" {
		t.Skip("helper mode")
	}
	dir := t.TempDir()
	ca, err := NewCA(CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server()
	if err != nil {
		t.Fatal(err)
	}
	certPath, _, err := leaf.WriteFiles(dir, "leaf")
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle.pem")
	if err := WriteBundle(bundle, ca); err != nil {
		t.Fatal(err)
	}
	emptyDir := t.TempDir()
	for _, tc := range []struct {
		env  string
		pass bool
	}{{Env(bundle), true}, {"SSL_CERT_FILE=" + filepath.Join(dir, "missing.pem"), false}} {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHelperSystemRoots$", "-test.count=1") //nolint:gosec // G204: re-executing this test binary
		cmd.Env = append(os.Environ(), "PKI_HELPER_LEAF="+certPath, tc.env, "SSL_CERT_DIR="+emptyDir)
		out, err := cmd.CombinedOutput()
		if (err == nil) != tc.pass {
			t.Fatalf("%s: helper err = %v, want pass %v\n%s", tc.env, err, tc.pass, out)
		}
	}
}

// TestHelperSystemRoots runs in the child of TestSSLCertFileBundle.
func TestHelperSystemRoots(t *testing.T) {
	path := os.Getenv("PKI_HELPER_LEAF")
	if path == "" {
		t.Skip("run by TestSSLCertFileBundle")
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path from the parent test
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "localhost"}); err != nil {
		t.Fatal(err)
	}
}

func TestErrors(t *testing.T) {
	if _, err := NewCA(CAOptions{KeyType: KeyType(7)}); err == nil {
		t.Fatal("unknown key type: want error")
	}
	if KeyType(7).String() != "KeyType(7)" {
		t.Fatal("KeyType.String")
	}
	ca, err := NewCA(CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Issue(LeafOptions{KeyType: KeyType(9)}); err == nil {
		t.Fatal("unknown leaf key type: want error")
	}
	if _, err := ca.Issue(LeafOptions{URIs: []string{"%zz"}}); err == nil {
		t.Fatal("bad URI SAN: want error")
	}
	bad := &Leaf{ChainPEM: []byte("x"), KeyPEM: []byte("y")}
	if _, err := bad.TLSCertificate(); err == nil {
		t.Fatal("bad key pair: want error")
	}
	if _, err := bad.ServerTLS(nil); err == nil {
		t.Fatal("bad key pair: want ServerTLS error")
	}
	if _, err := ClientTLS(ca, bad); err == nil {
		t.Fatal("bad key pair: want ClientTLS error")
	}
	// Explicit validity windows are honored: a not-yet-valid leaf fails
	// verification.
	now := time.Now()
	l, err := ca.Issue(LeafOptions{DNSNames: []string{"localhost"}, NotBefore: now.Add(time.Hour), NotAfter: now.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Cert.Verify(x509.VerifyOptions{Roots: ca.Pool(), DNSName: "localhost"}); err == nil {
		t.Fatal("not-yet-valid certificate verified")
	}
}

func BenchmarkIssueECDSA(b *testing.B) {
	ca, err := NewCA(CAOptions{})
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := ca.Server(); err != nil {
			b.Fatal(err)
		}
	}
}
