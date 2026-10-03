// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tlsconf

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/netip"
	"strings"
)

// minRSABits is the smallest accepted RSA modulus (spec 06 requirement 75).
const minRSABits = 2048

// Fixed parse errors: none repeats secret content (spec 06 requirements 89
// and 91).
var (
	// ErrNoCertificate reports a certificate value without a parsable PEM
	// CERTIFICATE block.
	ErrNoCertificate = errors.New("tlsconf: no PEM certificate parses")
	// ErrKeyPair reports a private key that does not parse or does not
	// match the certificate.
	ErrKeyPair = errors.New("tlsconf: private key does not parse or does not match the certificate")
	// ErrKeyType reports a certificate key other than ECDSA P-256 or P-384,
	// RSA of at least 2048 bits or Ed25519.
	ErrKeyType = errors.New("tlsconf: certificate key is not ECDSA P-256 or P-384, RSA of at least 2048 bits or Ed25519")
	// ErrCABundle reports a CA bundle with no certificate or with a
	// CERTIFICATE block that does not parse.
	ErrCABundle = errors.New("tlsconf: CA bundle holds no certificate or a certificate that does not parse")
)

// parseKeyPair parses a PEM certificate chain and its private key, checks
// that they match and that the leaf key is an accepted type, and returns
// the pair with its Leaf set.
func parseKeyPair(certPEM, keyPEM []byte) (*tls.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		if !hasCertificate(certPEM) {
			return nil, ErrNoCertificate
		}
		return nil, ErrKeyPair
	}
	if pair.Leaf == nil {
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, ErrNoCertificate
		}
		pair.Leaf = leaf
	}
	if err := checkKey(pair.Leaf.PublicKey); err != nil {
		return nil, err
	}
	return &pair, nil
}

// hasCertificate reports whether b holds a CERTIFICATE block that parses.
func hasCertificate(b []byte) bool {
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			return false
		}
		if block.Type == "CERTIFICATE" {
			if _, err := x509.ParseCertificate(block.Bytes); err == nil {
				return true
			}
		}
	}
}

// checkKey accepts ECDSA P-256 and P-384, RSA of at least 2048 bits and
// Ed25519 public keys (spec 06 requirement 75).
func checkKey(pub any) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve == elliptic.P256() || k.Curve == elliptic.P384() {
			return nil
		}
	case *rsa.PublicKey:
		if k.N.BitLen() >= minRSABits {
			return nil
		}
	case ed25519.PublicKey:
		return nil
	}
	return ErrKeyType
}

// parseCertPool parses a PEM CA bundle: every CERTIFICATE block must parse
// and at least one must be present; other block types are skipped.
func parseCertPool(b []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	n := 0
	for {
		var block *pem.Block
		block, b = pem.Decode(b)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, ErrCABundle
		}
		pool.AddCert(c)
		n++
	}
	if n == 0 {
		return nil, ErrCABundle
	}
	return pool, nil
}

// serverName turns an Endpoint or URL host into a tls.Config ServerName:
// brackets and an IPv6 zone are removed, a trailing dot dropped and the
// name lowercased. An IP literal stays an IP literal, which crypto/tls
// verifies against IP SANs and never sends as SNI.
func serverName(host string) string {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if a, err := netip.ParseAddr(host); err == nil {
		return a.WithZone("").String()
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}
