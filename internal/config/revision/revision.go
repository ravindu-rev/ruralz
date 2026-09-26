// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package revision is the Revision identity: the SHA-256 digest of the
// exact ruralz.canonical.v1 bytes. The wire and storage form is
// sha256:<64 lowercase hex>; the display form rev-<12 hex> appears only in
// human output, logs and metric labels, never as an expected digest.
package revision

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Digest is a Revision digest. The zero value means "none".
type Digest struct{ sum [sha256.Size]byte }

// Revision is canonical content and its digest.
type Revision struct {
	// Digest is SHA-256 over Content.
	Digest Digest
	// Content is the exact ruralz.canonical.v1 bytes (no trailing newline).
	Content []byte
}

// ErrSyntax reports a malformed digest string.
var ErrSyntax = errors.New("revision: want sha256:<64 lowercase hex>")

// Sum returns the digest of canonical bytes.
func Sum(canonical []byte) Digest { return Digest{sum: sha256.Sum256(canonical)} }

// Parse accepts exactly "sha256:" followed by 64 lowercase hex characters.
func Parse(s string) (Digest, error) {
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(h) != 2*sha256.Size || !lowerHex(h) {
		return Digest{}, ErrSyntax
	}
	var d Digest
	_, _ = hex.Decode(d.sum[:], []byte(h))
	return d, nil
}

// ParseDisplay accepts "rev-" followed by 12 lowercase hex characters and
// returns the 12-hex prefix (for M2 lookups by display form).
func ParseDisplay(s string) (string, error) {
	h, ok := strings.CutPrefix(s, "rev-")
	if !ok || len(h) != 12 || !lowerHex(h) {
		return "", errors.New("revision: want rev-<12 lowercase hex>")
	}
	return h, nil
}

// String returns "sha256:<64 hex>".
func (d Digest) String() string { return "sha256:" + d.Hex() }

// Short returns "rev-<first 12 hex>".
func (d Digest) Short() string { return "rev-" + d.Hex()[:12] }

// Hex returns the 64 lowercase hex characters.
func (d Digest) Hex() string { return hex.EncodeToString(d.sum[:]) }

// IsZero reports whether d is the zero value.
func (d Digest) IsZero() bool { return d == Digest{} }

// Bytes returns a copy of the raw sum.
func (d Digest) Bytes() [sha256.Size]byte { return d.sum }

// MarshalText encodes the wire form.
func (d Digest) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText parses the wire form.
func (d *Digest) UnmarshalText(b []byte) error {
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
