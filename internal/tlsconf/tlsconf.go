// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package tlsconf is the single TLS policy place of a Node (Security and
// identity, "Transport security"; spec 06 sections 2.11 and 2.12). It
// builds every *tls.Config ruralzd uses:
//
//   - Server: https listeners, TLS 1.3 by default and 1.2 on request with
//     exactly the six ECDHE AEAD suites, ALPN h2 and http/1.1, certificate
//     choice by SNI from a CertIndex that follows secret rotations, and the
//     client-certificate request mode of auth.mtls (requirements 74 to 77,
//     46);
//   - Upstream and OTLP: TLS 1.2 or newer, server always verified against
//     tls.caCertificate or the system roots, optional client certificate,
//     rotated values reaching new connections (requirements 79, 81);
//   - IdentityProvider and StateStore: TLS 1.2 or newer against the system roots, which
//     honor SSL_CERT_FILE and SSL_CERT_DIR (requirements 80, 82);
//   - Admin: the admin server from RURALZ_ADMIN_TLS_DIR (requirement 95).
//
// No builder sets InsecureSkipVerify and no field disables verification.
// Parse failures carry fixed messages that never contain secret bytes.
package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// HandshakeTimeout bounds a listener's TLS handshake: the listener sets it
// as http.Server.ReadHeaderTimeout, which net/http applies to the
// handshake while ReadTimeout and WriteTimeout are unset (spec 06
// requirement 77, target).
const HandshakeTimeout = 10 * time.Second

// Listener tls.minVersion values (Gateway listeners[].tls.minVersion).
const (
	// Version12 allows TLS 1.2 with the TLS12Suites.
	Version12 = "1.2"
	// Version13 is the default: TLS 1.3 only.
	Version13 = "1.3"
)

// ALPN protocol identifiers served on https listeners.
const (
	// ProtoH2 is HTTP/2 over TLS.
	ProtoH2 = "h2"
	// ProtoHTTP11 is HTTP/1.1.
	ProtoHTTP11 = "http/1.1"
)

// Codes returned by the builders.
const (
	// codeSchema is a configuration shape a builder refuses (both-or-neither
	// client certificate and key, an unknown minVersion, no certificates).
	codeSchema = "RZ-CFG-005"
	// codeResolution is a secretRef that is unresolved or whose content
	// does not parse (spec 06 requirement 51).
	codeResolution = "RZ-CFG-026"
)

// ErrNoSystemRoots reports that the process has no usable system CA roots,
// so JWKS, token, Upstream and State Store verification against the system
// roots fails (OQ-tech-stack-and-libraries-25; ruralzd logs a startup
// warning).
var ErrNoSystemRoots = errors.New("tlsconf: no system CA roots; set SSL_CERT_FILE or SSL_CERT_DIR")

// TLS12Suites returns a fresh copy of the only TLS 1.2 cipher suites a
// Node negotiates, ECDHE with AES-GCM or ChaCha20-Poly1305 (spec 06
// requirement 74). TLS 1.3 suites are not configurable in Go.
func TLS12Suites() []uint16 {
	return []uint16{
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
		tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	}
}

// NextProtos returns the ALPN list of https listeners: h2, then http/1.1.
func NextProtos() []string { return []string{ProtoH2, ProtoHTTP11} }

// ParseMinVersion maps a listener tls.minVersion to a crypto/tls version:
// "" and "1.3" give TLS 1.3 (the default), "1.2" gives TLS 1.2; anything
// else is RZ-CFG-005.
func ParseMinVersion(v string) (uint16, error) {
	switch v {
	case "", Version13:
		return tls.VersionTLS13, nil
	case Version12:
		return tls.VersionTLS12, nil
	default:
		return 0, errcode.Errorf(codeSchema, "tls.minVersion %q is not %q or %q", v, Version12, Version13)
	}
}

// Client returns the base client configuration of every Node TLS client:
// TLS 1.2 or newer with the TLS12Suites, no renegotiation, the server
// verified against roots (nil means the system roots). Callers set
// ServerName (or let crypto/tls infer it from the dialed address).
func Client(roots *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion:    tls.VersionTLS12,
		MaxVersion:    tls.VersionTLS13,
		CipherSuites:  TLS12Suites(),
		RootCAs:       roots,
		Renegotiation: tls.RenegotiateNever,
	}
}

// IdentityProvider returns the client configuration of jwksUrl and
// tokenUrl fetches (IdP, TB-10): TLS 1.2 or newer against the system
// roots; private CAs come from SSL_CERT_FILE and SSL_CERT_DIR, never from
// a Bundle field (spec 06 requirement 82). Pass it as
// egress.ClientOptions.TLS.
func IdentityProvider() *tls.Config { return Client(nil) }

// StateStore returns the client configuration of a rediss:// State Store:
// TLS 1.2 or newer, verified against the system roots for host (spec 06
// requirement 80). The rueidis wrapper passes it to its dialer; host is
// the URL host without port (an IP literal verifies IP SANs and sends no
// SNI).
func StateStore(host string) *tls.Config {
	c := Client(nil)
	c.ServerName = serverName(host)
	return c
}

// CheckSystemRoots returns ErrNoSystemRoots when the system CA pool cannot
// be loaded or is empty.
func CheckSystemRoots() error { return checkRoots(x509.SystemCertPool) }

// checkRoots is CheckSystemRoots over an injectable loader.
func checkRoots(load func() (*x509.CertPool, error)) error {
	pool, err := load()
	if err != nil {
		return errors.Join(ErrNoSystemRoots, err)
	}
	if pool == nil || pool.Equal(x509.NewCertPool()) {
		return ErrNoSystemRoots
	}
	return nil
}
