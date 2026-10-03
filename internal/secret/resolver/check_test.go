// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/secret"
)

// Tests for the per-kind checks and size caps (architecture 2.8 Kind,
// R-9): PEM certificate, key, CA bundle and CRL parsing (spec 06
// requirements 51 and 76), API key length after trimming (spec 06
// requirement 13) and the State Store URL credentials rule (spec 06
// requirement 80), each an RZ-CFG-026 at activation.

func TestSizeCap(t *testing.T) {
	for k := secret.KindOpaque; k <= secret.KindStateStoreURL+1; k++ {
		want := int64(MaxValueBytes)
		if k == secret.KindPEMCRL {
			want = MaxCRLBytes
		}
		if got := sizeCap(k); got != want {
			t.Errorf("sizeCap(%d) = %d, want %d", k, got, want)
		}
	}
}

func TestCheckKind(t *testing.T) {
	p := newPEM(t)
	pkcs8 := func(key any) string {
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	}
	ec, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}))
	garbled := func(typ string) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: []byte("garbage")}))
	}
	tests := []struct {
		name  string
		kind  secret.Kind
		value string
		want  error
	}{
		{"opaque anything", secret.KindOpaque, "\x00\xff", nil},
		{"opaque empty", secret.KindOpaque, "", nil},
		{"cert", secret.KindPEMCertificate, p.cert, nil},
		{"cert chain with text and key", secret.KindPEMCertificate, "leading text\n" + p.cert + p.cert + p.key, nil},
		{"cert none", secret.KindPEMCertificate, p.key, errNoCertificate},
		{"cert empty", secret.KindPEMCertificate, "", errNoCertificate},
		{"cert one bad in chain", secret.KindPEMCertificate, p.cert + garbled("CERTIFICATE"), errBadCertificate},
		{"key SEC 1", secret.KindPEMPrivateKey, p.key, nil},
		{"key PKCS #8 P-384", secret.KindPEMPrivateKey, pkcs8(ec), nil},
		{"key PKCS #8 Ed25519", secret.KindPEMPrivateKey, pkcs8(ed), nil},
		{"key PKCS #8 RSA", secret.KindPEMPrivateKey, pkcs8(rsaKey), nil},
		{"key PKCS #1", secret.KindPEMPrivateKey, pkcs1, nil},
		{"key after cert", secret.KindPEMPrivateKey, p.cert + p.key, nil},
		{"key none", secret.KindPEMPrivateKey, p.cert, errNoPrivateKey},
		{"key garbled", secret.KindPEMPrivateKey, garbled("PRIVATE KEY"), errBadPrivateKey},
		{"key encrypted", secret.KindPEMPrivateKey, garbled("ENCRYPTED PRIVATE KEY"), errBadPrivateKey},
		{"pool", secret.KindPEMCertPool, p.cert, nil},
		{"pool one good one bad", secret.KindPEMCertPool, garbled("CERTIFICATE") + p.cert, nil},
		{"pool none parses", secret.KindPEMCertPool, garbled("CERTIFICATE"), errNoPoolCert},
		{"pool empty", secret.KindPEMCertPool, "", errNoPoolCert},
		{"crl", secret.KindPEMCRL, p.crl + p.crl, nil},
		{"crl none", secret.KindPEMCRL, p.cert, errNoCRL},
		{"crl bad", secret.KindPEMCRL, p.crl + garbled("X509 CRL"), errBadCRL},
		{"api key 22", secret.KindAPIKey, strings.Repeat("k", 22), nil},
		{"api key 21", secret.KindAPIKey, strings.Repeat("k", 21), errShortAPIKey},
		{"api key 21 plus whitespace", secret.KindAPIKey, " \t" + strings.Repeat("k", 21) + "\r\n", errShortAPIKey},
		{"api key 22 plus newline", secret.KindAPIKey, strings.Repeat("k", 22) + "\n", nil},
		{"url loopback v4", secret.KindStateStoreURL, "redis://127.0.0.1:6379", nil},
		{"url loopback 127/8", secret.KindStateStoreURL, "redis://127.9.8.7:6379/2", nil},
		{"url loopback v6", secret.KindStateStoreURL, "redis://[::1]:6379", nil},
		{"url IPv4-mapped is not a loopback literal", secret.KindStateStoreURL, "redis://[::ffff:127.0.0.1]:6379", errURLNoCredentials}, // spec 08 req 8
		{"url IPv4-mapped addr", secret.KindStateStoreURL, "redis://127.0.0.1:7000?addr=[::ffff:127.0.0.1]:7001", errURLNoCredentials},
		{"url IPv4-mapped with credentials", secret.KindStateStoreURL, "redis://u:p@[::ffff:127.0.0.1]:6379", nil},
		{"url localhost", secret.KindStateStoreURL, "redis://LocalHost", nil},
		{"url loopback with newline", secret.KindStateStoreURL, "redis://127.0.0.1:6379\n", nil},
		{"url credentials", secret.KindStateStoreURL, "rediss://u:p@cache.internal:6380/2", nil},
		{"url password only", secret.KindStateStoreURL, "rediss://:p@cache.internal:6380", nil},
		{"url user only", secret.KindStateStoreURL, "rediss://default@cache.internal:6380", nil},
		{"url no credentials", secret.KindStateStoreURL, "redis://cache.internal:6379", errURLNoCredentials},
		{"url empty password", secret.KindStateStoreURL, "redis://:@cache.internal:6379", errURLNoCredentials},
		{"url name resolving to loopback", secret.KindStateStoreURL, "redis://localhost.example:6379", errURLNoCredentials},
		{"url non-loopback addr", secret.KindStateStoreURL, "redis://127.0.0.1:6379?addr=10.0.0.2:6379", errURLNoCredentials},
		{"url loopback addrs", secret.KindStateStoreURL, "redis://127.0.0.1:7000?addr=127.0.0.1:7001&addr=[::1]:7002&addr=localhost", nil},
		{"url no host", secret.KindStateStoreURL, "redis://", errURLNoCredentials},
		{"url unparsable", secret.KindStateStoreURL, "redis://[::1", errBadURL},
		{"url no scheme", secret.KindStateStoreURL, "cache.internal:6379", errBadURL},
		{"url relative", secret.KindStateStoreURL, "/path", errBadURL},
		{"unknown kind", secret.KindStateStoreURL + 1, "x", errUnknownKind},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkKind(tc.kind, []byte(tc.value)); !errors.Is(err, tc.want) {
				t.Errorf("checkKind = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRunCheckOrder(t *testing.T) {
	// The size cap runs first, then the kind check, then the consumer check.
	called := false
	u := useCheck{kind: secret.KindAPIKey, check: func([]byte) error { called = true; return nil }}
	if err := runCheck(u, make([]byte, MaxValueBytes+1)); !errors.Is(err, errTooLargeValue) || called {
		t.Errorf("over cap: %v, consumer called %v", err, called)
	}
	if err := runCheck(u, []byte("short")); !errors.Is(err, errShortAPIKey) || called {
		t.Errorf("short: %v, consumer called %v", err, called)
	}
	if err := runCheck(u, []byte(strings.Repeat("k", 30))); err != nil || !called {
		t.Errorf("valid: %v, consumer called %v", err, called)
	}
}

func TestLeaks(t *testing.T) {
	tests := []struct {
		msg, value string
		want       bool
	}{
		{"topology standalone refuses addr", "redis://u:p@h:1?addr=x", false},
		{"bad url redis://u:p@h", "redis://u:p@h:1", true},
		{"contains abc", " abc\n", true}, // short values: whole trimmed value
		{"nothing here", "abc", false},
		{"any message", "", false},
		{"short", "a-long-secret-value", false},
		{"...secret-v...", "a-long-secret-value", true}, // an 8-byte window
		{"...secret-...", "a-long-secret-value", false},
	}
	for _, tc := range tests {
		if got := leaks(tc.msg, []byte(tc.value)); got != tc.want {
			t.Errorf("leaks(%q, %q) = %v, want %v", tc.msg, tc.value, got, tc.want)
		}
	}
	if err := sanitize(errors.New("fine"), []byte("value-value")); err.Error() != "fine" {
		t.Errorf("sanitize kept = %v", err)
	}
}

// FuzzKindChecks checks every kind on arbitrary bytes: no panic, and a
// failure is always a fixed reason that never repeats the value (spec 06
// requirement 89, 6.4 FuzzPEMBundleAndCRL for the resolver's PEM parsing).
func FuzzKindChecks(f *testing.F) {
	p := newPEM(f)
	for k := range secret.KindStateStoreURL + 2 {
		f.Add(uint8(k), []byte(""))
		f.Add(uint8(k), []byte(p.cert+p.key+p.crl))
	}
	f.Add(uint8(secret.KindStateStoreURL), []byte("redis://u:p@h:6379?addr=[::1]:1"))
	f.Add(uint8(secret.KindAPIKey), []byte(" 0123456789012345678901 \n"))
	f.Add(uint8(secret.KindPEMCRL), []byte("-----BEGIN X509 CRL-----\nAAAA\n-----END X509 CRL-----\n"))
	fixed := map[error]bool{
		errNoCertificate: true, errBadCertificate: true, errNoPoolCert: true, errNoPrivateKey: true,
		errBadPrivateKey: true, errNoCRL: true, errBadCRL: true, errShortAPIKey: true, errBadURL: true,
		errURLNoCredentials: true, errUnknownKind: true,
	}
	f.Fuzz(func(t *testing.T, kind uint8, value []byte) {
		err := checkKind(secret.Kind(kind), value)
		if err == nil {
			return
		}
		if !fixed[err] {
			t.Fatalf("unexpected error %v", err)
		}
	})
}
