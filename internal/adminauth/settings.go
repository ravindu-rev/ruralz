// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminauth

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Process settings holding the admin credentials (spec 06 requirement 93,
// OQ-security-and-identity-7 (a)). They are never Bundle fields and never
// change a Revision.
//
//nolint:gosec // G101: environment variable names, not credentials.
const (
	// EnvTokenFile names the operator token file (every path).
	EnvTokenFile = "RURALZ_ADMIN_TOKEN_FILE"
	// EnvMetricsTokenFile names the metrics token file (/metrics only).
	EnvMetricsTokenFile = "RURALZ_ADMIN_METRICS_TOKEN_FILE"
	// EnvTLSDir names the admin TLS directory; its ca.crt also admits
	// client certificates.
	EnvTLSDir = "RURALZ_ADMIN_TLS_DIR"
)

// Files of the admin TLS directory (the names internal/tlsconf.Admin
// reads).
const (
	// TLSCertFile is the admin server certificate chain (required).
	TLSCertFile = "tls.crt"
	// TLSKeyFile is the admin server private key (required).
	TLSKeyFile = "tls.key"
	// ClientCAFile is the optional client CA bundle admitting client
	// certificates.
	ClientCAFile = "ca.crt"
)

// Settings errors; ruralzd refuses to start (exit 2) on any of them.
var (
	// ErrRelativePath reports an admin setting that is not an absolute
	// path.
	ErrRelativePath = errors.New("adminauth: an admin setting must be an absolute path")
	// ErrTLSFileMissing reports a TLS directory without tls.crt or
	// tls.key.
	ErrTLSFileMissing = errors.New("adminauth: the admin TLS directory lacks tls.crt or tls.key")
	// ErrNotDirectory reports an admin TLS directory that is not one.
	ErrNotDirectory = errors.New("adminauth: the admin TLS directory is not a directory")
	// ErrClientCA reports a ca.crt that holds no certificate or a
	// certificate that does not parse.
	ErrClientCA = errors.New("adminauth: ca.crt holds no certificate or a certificate that does not parse")
	// ErrClientCALarge reports a ca.crt over 1 MiB.
	ErrClientCALarge = errors.New("adminauth: ca.crt is larger than 1 MiB")
)

// maxClientCABytes caps the ca.crt read (internal/tlsconf.Admin's cap).
const maxClientCABytes = 1 << 20

// Settings are the admin credential settings; an empty field is unset.
type Settings struct {
	// TokenFile is RURALZ_ADMIN_TOKEN_FILE, the operator token file.
	TokenFile string
	// MetricsTokenFile is RURALZ_ADMIN_METRICS_TOKEN_FILE.
	MetricsTokenFile string
	// TLSDir is RURALZ_ADMIN_TLS_DIR.
	TLSDir string
}

// LoadSettings reads the three settings through getenv (os.Getenv in
// ruralzd). An unset or empty variable leaves its field empty; a set one
// must be an absolute path and is cleaned. Every invalid setting is
// reported.
func LoadSettings(getenv func(string) string) (Settings, error) {
	var s Settings
	var errs []error
	for _, f := range []struct {
		env string
		dst *string
	}{
		{EnvTokenFile, &s.TokenFile},
		{EnvMetricsTokenFile, &s.MetricsTokenFile},
		{EnvTLSDir, &s.TLSDir},
	} {
		v := getenv(f.env)
		if v == "" {
			continue
		}
		if !filepath.IsAbs(v) {
			errs = append(errs, fmt.Errorf("%s: %w", f.env, ErrRelativePath))
			continue
		}
		*f.dst = filepath.Clean(v)
	}
	if err := errors.Join(errs...); err != nil {
		return Settings{}, err
	}
	return s, nil
}

// TLS reports whether the admin server serves TLS. Without it the admin
// hop is cleartext on every interface (ruralz_security_cleartext_hops
// hop="admin", spec 06 requirement 95).
func (s Settings) TLS() bool { return s.TLSDir != "" }

// Guarded returns the set paths, which RURALZ_SECRET_ROOT must not
// contain (spec 06 requirement 88).
func (s Settings) Guarded() []string {
	var out []string
	for _, p := range []string{s.TokenFile, s.MetricsTokenFile, s.TLSDir} {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// checkTLSDir checks that dir holds tls.crt and tls.key and returns the
// anchors of its ca.crt (nil without one). Parsing tls.crt and tls.key,
// and building the TLS configuration, is internal/tlsconf.Admin's.
func checkTLSDir(dir string) (anchors map[string]struct{}, err error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", EnvTLSDir, dir, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s %s: %w", EnvTLSDir, dir, ErrNotDirectory)
	}
	for _, name := range []string{TLSCertFile, TLSKeyFile} {
		if ok, err := regularFile(filepath.Join(dir, name)); err != nil || !ok {
			return nil, fmt.Errorf("%s %s: %w", EnvTLSDir, dir, errors.Join(ErrTLSFileMissing, err))
		}
	}
	ca := filepath.Join(dir, ClientCAFile)
	ok, err := regularFile(ca)
	if err == nil && ok {
		anchors, err = loadClientCA(ca)
	}
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", EnvTLSDir, dir, err)
	}
	return anchors, nil
}

// loadClientCA parses the CERTIFICATE blocks of the ca.crt at p, as
// internal/tlsconf.Admin does (other blocks are skipped), and returns the
// DER of each as a set.
func loadClientCA(p string) (map[string]struct{}, error) {
	b, _, err := readRegular(p, maxClientCABytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if len(b) > maxClientCABytes {
		return nil, fmt.Errorf("%s: %w", p, ErrClientCALarge)
	}
	anchors := make(map[string]struct{})
	for {
		var block *pem.Block
		if block, b = pem.Decode(b); block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, ErrClientCA)
		}
		anchors[string(c.Raw)] = struct{}{}
	}
	if len(anchors) == 0 {
		return nil, fmt.Errorf("%s: %w", p, ErrClientCA)
	}
	return anchors, nil
}

// regularFile reports whether p is a regular file (symlinks followed); a
// missing p is (false, nil).
func regularFile(p string) (bool, error) {
	fi, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !fi.Mode().IsRegular():
		return false, fmt.Errorf("%s: %w", p, ErrNotRegular)
	}
	return true, nil
}
