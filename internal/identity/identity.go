// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package identity compiles the Consumers of a Revision for the data plane
// (docs/architecture/08-security-and-identity.md "Consumers and tiers";
// M1 spec 06 section 2.2): the read-only expr.Consumer views shared by CEL,
// quota keys, cache partitions and logs; the per-snapshot credential Index
// that auth Filters consult to bind a Consumer (API key digests, JWT issuer
// plus subject or claims, OAuth client identifiers, basic usernames,
// certificate subjects and URI SANs); the KeyIndex that holds the digests of
// secretRef-held API keys and follows their rotations copy-on-write; the
// non-secret principal keys; the static Consumer checks (RZ-CFG-035,
// RZ-CFG-036) the configuration validator runs in every binary; and the
// Revoker hook that M2's revocation list implements.
//
// Everything built here is immutable once published and read without
// locks. The package never logs, never resolves a secret by itself (it
// reads a secret.Store handed to it) and never keeps a raw API key: keys
// are hashed and their revealed bytes cleared at once.
package identity

import (
	"math/big"
	"time"
)

// Revoker is the M2 revocation hook (Security and identity, "Revocation";
// spec 06 requirements 31 and 40 and section 8). Every auth Filter consults
// it after verification and before binding, the auth.basic success cache
// on every hit, and the JWKS manager on every key load. M1 wires
// NopRevoker. Implementations are safe for concurrent use and never block.
type Revoker interface {
	// RevokedJWT reports whether a verified token is revoked by jti or by a
	// subject cut-off (iat is the zero time when the token has none).
	RevokedJWT(iss, kid, jti, sub string, iat time.Time) bool
	// RevokedKey reports whether the issuer's key kid is revoked.
	RevokedKey(iss, kid string) bool
	// RevokedAPIKey reports whether the API key with this SHA-256 digest is
	// revoked.
	RevokedAPIKey(digest [32]byte) bool
	// RevokedBasic reports whether the auth.basic username is revoked.
	RevokedBasic(username string) bool
	// RevokedCert reports whether the certificate with this issuer (the
	// DER RawIssuer) and serial is revoked outside the Policy's CRL.
	RevokedCert(issuer []byte, serial *big.Int) bool
}

// NopRevoker is the M1 Revoker: it never revokes anything (RZ-AUTH-004 is
// reserved for M2, apart from auth.mtls CRLs).
type NopRevoker struct{}

var _ Revoker = NopRevoker{}

// RevokedJWT returns false.
func (NopRevoker) RevokedJWT(string, string, string, string, time.Time) bool { return false }

// RevokedKey returns false.
func (NopRevoker) RevokedKey(string, string) bool { return false }

// RevokedAPIKey returns false.
func (NopRevoker) RevokedAPIKey([32]byte) bool { return false }

// RevokedBasic returns false.
func (NopRevoker) RevokedBasic(string) bool { return false }

// RevokedCert returns false.
func (NopRevoker) RevokedCert([]byte, *big.Int) bool { return false }
