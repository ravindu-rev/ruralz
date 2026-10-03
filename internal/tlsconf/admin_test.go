// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for spec 06 requirements 93 (RURALZ_ADMIN_TLS_DIR files tls.crt,
// tls.key, optional ca.crt; missing tls.crt or tls.key refuses start) and
// 95 (client certificates verified when given against ca.crt, leaf with an
// explicit clientAuth extended key usage).

type adminFixture struct {
	ca       *testCA
	clientCA *testCA
	dir      string
}

func newAdminFixture(t *testing.T, withCA bool) *adminFixture {
	t.Helper()
	f := &adminFixture{ca: newCA(t, "admin-ca"), clientCA: newCA(t, "operator-ca"), dir: t.TempDir()}
	srv := f.ca.issue(t, leafOpts{cn: "admin", dns: []string{"admin.local"}})
	writeFile(t, f.dir, AdminCertFile, srv.certPEM)
	writeFile(t, f.dir, AdminKeyFile, srv.keyPEM)
	if withCA {
		writeFile(t, f.dir, AdminClientCAFile, f.clientCA.pem)
	}
	return f
}

func (f *adminFixture) client(t *testing.T, cl *leaf) *tls.Config {
	t.Helper()
	c := clientFor(f.ca, "admin.local")
	if cl != nil {
		pair, err := tls.X509KeyPair(cl.certPEM, cl.keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		// Always present the pair, even when its issuer is not among the
		// server's acceptable CAs.
		c.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &pair, nil }
	}
	return c
}

func TestAdminWithoutClientCAReq93(t *testing.T) {
	f := newAdminFixture(t, false)
	cfg, err := Admin(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.NoClientCert || cfg.ClientCAs != nil || cfg.MinVersion != tls.VersionTLS12 {
		t.Fatalf("admin config: %+v", cfg)
	}
	cr, sr := handshake(t, cfg, f.client(t, nil))
	if cr.err != nil || sr.err != nil {
		t.Fatalf("handshake: %v %v", cr.err, sr.err)
	}
}

func TestAdminClientCertificatesReq95(t *testing.T) {
	f := newAdminFixture(t, true)
	cfg, err := Admin(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Fatalf("ClientAuth = %v", cfg.ClientAuth)
	}
	good := f.clientCA.issue(t, leafOpts{cn: "operator", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	noEKU := f.clientCA.issue(t, leafOpts{cn: "no-eku", noEKU: true})
	serverOnly := f.clientCA.issue(t, leafOpts{cn: "server-only", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	stranger := newCA(t, "stranger").issue(t, leafOpts{cn: "stranger", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})

	// No certificate: the handshake passes (tokens still apply).
	if cr, sr := handshake(t, cfg, f.client(t, nil)); cr.err != nil || sr.err != nil || len(sr.state.PeerCertificates) != 0 {
		t.Fatalf("no certificate: %v %v", cr.err, sr.err)
	}
	// A clientAuth leaf from ca.crt is verified.
	cr, sr := handshake(t, cfg, f.client(t, &good))
	if cr.err != nil || sr.err != nil || len(sr.state.VerifiedChains) == 0 {
		t.Fatalf("operator certificate: %v %v", cr.err, sr.err)
	}
	// A leaf without extended key usage is refused although crypto/tls
	// would accept it for any usage.
	if _, sr := handshake(t, cfg, f.client(t, &noEKU)); !errors.Is(sr.err, ErrClientAuthEKU) {
		t.Fatalf("leaf without EKU: %v, want ErrClientAuthEKU", sr.err)
	}
	// serverAuth only and a foreign CA fail verification.
	for name, l := range map[string]*leaf{"server-only": &serverOnly, "stranger": &stranger} {
		if _, sr := handshake(t, cfg, f.client(t, l)); sr.err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestAdminErrorsReq93(t *testing.T) {
	good := newAdminFixture(t, false)
	crt, err := os.ReadFile(filepath.Join(good.dir, AdminCertFile))
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(good.dir, AdminKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	keyBody := string(bytes.Split(key, []byte("\n"))[1])
	tests := []struct {
		name  string
		files map[string][]byte
		dirs  []string
		is    error
	}{
		{"missing tls.crt", map[string][]byte{AdminKeyFile: key}, nil, ErrAdminTLSMissing},
		{"missing tls.key", map[string][]byte{AdminCertFile: crt}, nil, ErrAdminTLSMissing},
		{"empty dir", nil, nil, ErrAdminTLSMissing},
		{"garbage tls.crt", map[string][]byte{AdminCertFile: []byte("junk"), AdminKeyFile: key}, nil, ErrNoCertificate},
		{"garbage tls.key", map[string][]byte{AdminCertFile: crt, AdminKeyFile: []byte("junk")}, nil, ErrKeyPair},
		{"garbage ca.crt", map[string][]byte{AdminCertFile: crt, AdminKeyFile: key, AdminClientCAFile: []byte("junk")}, nil, ErrCABundle},
		{"oversized tls.crt", map[string][]byte{AdminCertFile: bytes.Repeat([]byte("a"), maxAdminFileBytes+1), AdminKeyFile: key}, nil, ErrAdminFileTooLarge},
		{"tls.key is a directory", map[string][]byte{AdminCertFile: crt}, []string{AdminKeyFile}, nil},
		{"ca.crt is a directory", map[string][]byte{AdminCertFile: crt, AdminKeyFile: key}, []string{AdminClientCAFile}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, b := range tt.files {
				writeFile(t, dir, name, b)
			}
			for _, d := range tt.dirs {
				if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Admin(dir)
			if err == nil {
				t.Fatal("Admin succeeded")
			}
			if tt.is != nil && !errors.Is(err, tt.is) {
				t.Fatalf("err %v, want %v", err, tt.is)
			}
			if strings.Contains(err.Error(), keyBody) {
				t.Fatal("the error repeats key material")
			}
		})
	}
	if _, err := Admin(filepath.Join(good.dir, "absent")); !errors.Is(err, ErrAdminTLSMissing) {
		t.Fatalf("absent dir: %v", err)
	}
	// A directory path that is a file: an open error other than not-exist.
	if _, err := Admin(filepath.Join(good.dir, AdminCertFile)); err == nil || errors.Is(err, ErrAdminTLSMissing) {
		t.Fatalf("file as dir: %v", err)
	}
}
