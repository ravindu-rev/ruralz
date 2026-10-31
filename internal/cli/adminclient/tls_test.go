// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// Tests for the TLS credentials of docs/reference/01-cli-and-api-surface.md
// "Admin API" and "Global flags": --ca-file adds trust anchors to the
// system roots; --client-cert and --client-key come together and only
// with https, as unencrypted PEM, anything else a usage error (exit 2);
// unreadable files exit 2 too.

// isUsage reports whether err is a usage error with exit code 2.
func isUsage(t *testing.T, err error) bool {
	t.Helper()
	_, ok := errors.AsType[*command.UsageError](err)
	return ok && command.Code(t.Context(), err) == command.ExitNoResult
}

// TestTLSPolicy checks the configuration of an https Node: the Node's
// client policy, server verification always on, ALPN http/1.1 and the
// ServerName of the URL host.
func TestTLSPolicy(t *testing.T) {
	cases := map[string]string{
		"https://Node-A.shop.example.:9901": "node-a.shop.example",
		"https://127.0.0.1:9901":            "127.0.0.1",
		"https://[::1]:9901":                "::1",
		"https://[fe80::1%25eth0]:9901":     "fe80::1",
	}
	for raw, want := range cases {
		c, _ := newTestClient(t, Config{URL: raw})
		tc := c.tls
		if tc == nil || tc.ServerName != want {
			t.Fatalf("%s: TLS config %+v, want ServerName %q", raw, tc, want)
		}
		if tc.InsecureSkipVerify || tc.MinVersion != tls.VersionTLS12 || tc.MaxVersion != tls.VersionTLS13 ||
			!slices.Equal(tc.CipherSuites, tlsconf.TLS12Suites()) || tc.Renegotiation != tls.RenegotiateNever ||
			!slices.Equal(tc.NextProtos, []string{"http/1.1"}) || tc.RootCAs != nil || tc.GetClientCertificate != nil {
			t.Errorf("%s: TLS config = %+v", raw, tc)
		}
	}
	if c, _ := newTestClient(t, Config{URL: "http://127.0.0.1:9901"}); c.tls != nil {
		t.Error("an http Node has a TLS configuration")
	}
}

// TestCAFile covers --ca-file: the system roots plus its certificates; a
// file without a certificate or with one that does not parse is a usage
// error; a missing file exits 2; an http Node ignores it.
func TestCAFile(t *testing.T) {
	p := newPKI(t)
	c, _ := newTestClient(t, Config{URL: "https://127.0.0.1:9901", CAFile: p.caFile})
	if c.tls.RootCAs == nil {
		t.Fatal("no RootCAs with --ca-file")
	}
	leaf, err := p.ca.Server("node-a.shop.example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Cert.Verify(x509.VerifyOptions{Roots: c.tls.RootCAs, DNSName: "node-a.shop.example"}); err != nil {
		t.Errorf("a certificate of the --ca-file CA does not verify: %v", err)
	}

	dir := t.TempDir()
	notPEM := writeFile(t, dir, "der", "\x30\x82\x01\x0a")
	keyOnly := writeFile(t, dir, "key", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")
	broken := writeFile(t, dir, "broken", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not DER")})))
	for path, want := range map[string]string{
		notPEM:  "--ca-file " + notPEM + ": holds no PEM certificate",
		keyOnly: "--ca-file " + keyOnly + ": holds no PEM certificate",
		broken:  "--ca-file " + broken + ": a PEM certificate does not parse",
	} {
		_, err := New(Config{URL: "https://127.0.0.1:9901", CAFile: path}, Options{})
		if !isUsage(t, err) || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("New(--ca-file %s) = %v, want usage error %q", filepath.Base(path), err, want)
		}
	}
	_, err = New(Config{URL: "https://127.0.0.1:9901", CAFile: filepath.Join(dir, "absent")}, Options{})
	if isUsage(t, err) || command.Code(t.Context(), err) != command.ExitNoResult || !errors.Is(err, fs.ErrNotExist) ||
		!strings.HasPrefix(err.Error(), "--ca-file: ") {
		t.Errorf("New(missing --ca-file) = %v, want exit 2", err)
	}
	if _, err := New(Config{URL: "http://127.0.0.1:9901", CAFile: filepath.Join(dir, "absent")}, Options{}); err != nil {
		t.Errorf("an http Node read --ca-file: %v", err)
	}
}

// TestClientCertificateForms covers the pairing and forms of --client-cert
// and --client-key.
func TestClientCertificateForms(t *testing.T) {
	p := newPKI(t)
	other, err := p.ca.Client("other")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	otherKey := writeFile(t, dir, "other.key", string(other.KeyPEM))
	derKey := writeFile(t, dir, "der.key", "\x30\x81\x87\x02\x01\x00")
	pkcs8Encrypted := writeFile(t, dir, "enc8.key", string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte{1, 2, 3}})))
	legacyEncrypted := writeFile(t, dir, "legacy.key", string(pem.EncodeToMemory(&pem.Block{
		Type:    "EC PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-256-CBC,00112233445566778899AABBCCDDEEFF"},
		Bytes:   []byte{1, 2, 3},
	})))
	certAsKey := writeFile(t, dir, "cert-as.key", string(other.CertPEM))
	keyAsCert := writeFile(t, dir, "key-as.crt", string(other.KeyPEM))
	https := "https://127.0.0.1:9901"

	cases := []struct {
		name       string
		url        string
		cert, key  string
		wantPrefix string
	}{
		{"certificate without key", https, p.clientCert, "", "--client-cert and --client-key come together"},
		{"key without certificate", https, "", p.clientKey, "--client-cert and --client-key come together"},
		{"over http", "http://127.0.0.1:9901", p.clientCert, p.clientKey, "--client-cert and --client-key need an https admin URL; --admin is http://127.0.0.1:9901"},
		{"certificate file without a certificate", https, keyAsCert, p.clientKey, "--client-cert " + keyAsCert + ": holds no PEM certificate"},
		{"DER key", https, p.clientCert, derKey, "--client-key " + derKey + ": holds no PEM private key"},
		{"certificate in the key file", https, p.clientCert, certAsKey, "--client-key " + certAsKey + ": holds no PEM private key"},
		{"PKCS #8 encrypted key", https, p.clientCert, pkcs8Encrypted, "--client-key " + pkcs8Encrypted + ": holds an encrypted private key"},
		{"legacy encrypted key", https, p.clientCert, legacyEncrypted, "--client-key " + legacyEncrypted + ": holds an encrypted private key"},
		{"key of another certificate", https, p.clientCert, otherKey, "--client-cert and --client-key do not form a key pair"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := New(Config{URL: c.url, ClientCert: c.cert, ClientKey: c.key}, Options{})
			if !isUsage(t, err) || !strings.HasPrefix(err.Error(), c.wantPrefix) {
				t.Fatalf("New = %v, want usage error %q", err, c.wantPrefix)
			}
		})
	}

	for _, missing := range []Config{
		{URL: https, ClientCert: filepath.Join(dir, "absent.crt"), ClientKey: p.clientKey},
		{URL: https, ClientCert: p.clientCert, ClientKey: filepath.Join(dir, "absent.key")},
	} {
		_, err := New(missing, Options{})
		if isUsage(t, err) || !errors.Is(err, fs.ErrNotExist) || command.Code(t.Context(), err) != command.ExitNoResult {
			t.Errorf("New(%+v) = %v, want exit 2 for an unreadable file", missing, err)
		}
	}
}

// TestClientCertificatePresented checks that a valid pair is presented
// whatever certificate authorities the Node names.
func TestClientCertificatePresented(t *testing.T) {
	p := newPKI(t)
	c, _ := newTestClient(t, Config{URL: "https://127.0.0.1:9901", ClientCert: p.clientCert, ClientKey: p.clientKey})
	if c.tls.GetClientCertificate == nil {
		t.Fatal("no client certificate")
	}
	got, err := c.tls.GetClientCertificate(&tls.CertificateRequestInfo{AcceptableCAs: [][]byte{[]byte("another CA")}})
	if err != nil || got == nil || len(got.Certificate) == 0 {
		t.Fatalf("GetClientCertificate = %v, %v", got, err)
	}
	want, err := tls.LoadX509KeyPair(p.clientCert, p.clientKey)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(got.Certificate, want.Certificate, func(a, b []byte) bool { return string(a) == string(b) }) {
		t.Error("the presented chain is not the --client-cert chain")
	}
	// Names from a side of ruralz bundle diff reach the messages.
	_, err = New(Config{URL: "http://127.0.0.1:9901", ClientCert: p.clientCert, ClientKey: p.clientKey, Names: Names{
		URL: "TO", ClientCert: "--to-client-cert", ClientKey: "--to-client-key",
	}}, Options{})
	if err == nil || err.Error() != "--to-client-cert and --to-client-key need an https admin URL; TO is http://127.0.0.1:9901" {
		t.Errorf("New = %v", err)
	}
}
