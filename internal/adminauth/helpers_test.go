// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminauth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test tokens: RFC 6750 b64token, at least 22 bytes; canaries, not
// credentials.
//
//nolint:gosec // G101: test canaries, not credentials.
const (
	operatorToken = "op-canary-4fQ9x2LmZ7rT1vB8nK3sW6yH"
	metricsToken  = "metrics-canary-8Hs2Lq9Zp4Xw7Rt1Vn"
	wrongToken    = "wrong-token-000000000000000000000"
)

// writeFile writes b to dir/name and returns the path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// testCA is a generated certificate authority.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// leaf issues a leaf certificate with the given extended key usages.
func (ca *testCA) leaf(t *testing.T, cn string, eku ...x509.ExtKeyUsage) (*x509.Certificate, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"Ruralz Operators"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  eku,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}
}

func (ca *testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// tlsDir writes an admin TLS directory; withCA adds ca.crt.
func tlsDir(t *testing.T, ca *testCA, withCA bool) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, TLSCertFile, "-----BEGIN CERTIFICATE-----\n")
	writeFile(t, dir, TLSKeyFile, "-----BEGIN PRIVATE KEY-----\n")
	if withCA {
		writeFile(t, dir, ClientCAFile, string(ca.pem))
	}
	return dir
}

// logBuffer is a concurrency-safe JSON log sink.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lines returns the records whose msg is msg.
func (b *logBuffer) lines(msg string) []string {
	var out []string
	for l := range strings.Lines(b.String()) {
		if strings.Contains(l, `"msg":"`+msg+`"`) {
			out = append(out, l)
		}
	}
	return out
}

func newLogger(b *logBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
