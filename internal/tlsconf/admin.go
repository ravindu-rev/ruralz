// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// Admin TLS directory files (RURALZ_ADMIN_TLS_DIR, spec 06 requirement 93).
const (
	// AdminCertFile is the admin server certificate chain.
	AdminCertFile = "tls.crt"
	// AdminKeyFile is the admin server private key.
	AdminKeyFile = "tls.key"
	// AdminClientCAFile is the optional client CA bundle admitting client
	// certificates.
	AdminClientCAFile = "ca.crt"
)

// maxAdminFileBytes caps each admin TLS file (the 1 MiB secret cap).
const maxAdminFileBytes = 1 << 20

// Admin errors; messages never contain file content.
var (
	// ErrAdminTLSMissing reports a missing tls.crt or tls.key; ruralzd
	// refuses to start.
	ErrAdminTLSMissing = errors.New("tlsconf: admin TLS directory lacks tls.crt or tls.key")
	// ErrAdminFileTooLarge reports an admin TLS file above 1 MiB.
	ErrAdminFileTooLarge = errors.New("tlsconf: admin TLS file exceeds 1 MiB")
	// ErrClientAuthEKU reports an admin client certificate whose leaf does
	// not list the clientAuth extended key usage.
	ErrClientAuthEKU = errors.New("tlsconf: client certificate leaf lacks the clientAuth extended key usage")
)

// Admin builds the admin server configuration from RURALZ_ADMIN_TLS_DIR,
// read once at start (rotation by restart, spec 06 requirement 93):
// tls.crt and tls.key are required; with ca.crt, client certificates are
// verified when given (tls.VerifyClientCertIfGiven) and the leaf must list
// clientAuth explicitly (requirement 95), so adminauth admits a request
// whose connection has verified chains. TLS 1.2 or newer with the
// TLS12Suites, ALPN h2 and http/1.1.
func Admin(dir string) (*tls.Config, error) {
	certPEM, err := readAdminFile(dir, AdminCertFile)
	if err != nil {
		return nil, err
	}
	keyPEM, err := readAdminFile(dir, AdminKeyFile)
	if err != nil {
		return nil, err
	}
	pair, err := parseKeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("admin %s and %s: %w", AdminCertFile, AdminKeyFile, err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS13,
		CipherSuites: TLS12Suites(),
		NextProtos:   NextProtos(),
		Certificates: []tls.Certificate{*pair},
		ClientAuth:   tls.NoClientCert,
	}
	caPEM, err := readAdminFile(dir, AdminClientCAFile)
	switch {
	case errors.Is(err, ErrAdminTLSMissing):
		return cfg, nil
	case err != nil:
		return nil, err
	}
	pool, err := parseCertPool(caPEM)
	if err != nil {
		return nil, fmt.Errorf("admin %s: %w", AdminClientCAFile, err)
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.VerifyClientCertIfGiven
	cfg.VerifyConnection = requireClientAuthEKU
	return cfg, nil
}

// requireClientAuthEKU rejects a presented client certificate whose leaf
// does not list clientAuth: crypto/tls treats a leaf without extended key
// usages as valid for any usage.
func requireClientAuthEKU(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return nil
	}
	if !slices.Contains(cs.PeerCertificates[0].ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		return ErrClientAuthEKU
	}
	return nil
}

// readAdminFile reads one admin TLS file with the size cap; a missing file
// is ErrAdminTLSMissing.
func readAdminFile(dir, name string) ([]byte, error) {
	p := filepath.Join(dir, name)
	f, err := os.Open(p) //nolint:gosec // The path is RURALZ_ADMIN_TLS_DIR, an operator process setting.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrAdminTLSMissing, p)
	}
	if err != nil {
		return nil, fmt.Errorf("admin TLS file %s: %w", p, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxAdminFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("admin TLS file %s: %w", p, err)
	}
	if len(b) > maxAdminFileBytes {
		return nil, fmt.Errorf("%w: %s", ErrAdminFileTooLarge, p)
	}
	return b, nil
}
