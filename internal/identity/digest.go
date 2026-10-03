// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// DigestPrefix starts the stored form of an API key digest.
const DigestPrefix = "sha256:"

// MinKeyBytes is the shortest secretRef-held API key accepted after
// normalization (Security and identity, "Consumers and tiers": rejected
// under 22 base64url characters; spec 06 requirement 13).
const MinKeyBytes = 22

// Errors of the key and digest helpers. They never contain key bytes.
var (
	// ErrDigestSyntax: not "sha256:" followed by 64 lowercase hex digits.
	ErrDigestSyntax = errors.New("identity: API key digest is not sha256:<64 lowercase hex>")
	// ErrShortKey: a secretRef-held API key under MinKeyBytes after
	// normalization.
	ErrShortKey = errors.New("identity: secretRef-held API key is shorter than 22 bytes")
)

// Digest returns the SHA-256 digest of an API key: the exact bytes of the
// single header value as net/http delivers it (spec 06 requirement 14).
func Digest(key []byte) [32]byte { return sha256.Sum256(key) }

// DigestString is Digest over a string. Keys up to 256 bytes are hashed
// from a stack copy, so the request path allocates nothing.
func DigestString(key string) [32]byte {
	var buf [256]byte
	if len(key) <= len(buf) {
		n := copy(buf[:], key)
		d := sha256.Sum256(buf[:n])
		clear(buf[:n])
		return d
	}
	return sha256.Sum256([]byte(key))
}

// ParseDigest parses the stored form "sha256:" + 64 lowercase hex (schema
// pattern ^sha256:[0-9a-f]{64}$).
func ParseDigest(s string) ([32]byte, error) {
	var d [32]byte
	hexPart, ok := strings.CutPrefix(s, DigestPrefix)
	if !ok || len(hexPart) != 2*len(d) {
		return d, ErrDigestSyntax
	}
	for i := range len(hexPart) {
		if c := hexPart[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return d, ErrDigestSyntax
		}
	}
	if _, err := hex.Decode(d[:], []byte(hexPart)); err != nil {
		return d, ErrDigestSyntax
	}
	return d, nil
}

// FormatDigest returns the stored form of d.
func FormatDigest(d [32]byte) string { return DigestPrefix + hex.EncodeToString(d[:]) }

// NormalizeKey trims leading and trailing SP, HTAB, CR and LF from a
// secretRef-held API key (spec 06 requirement 13: a header value can never
// carry them). The result aliases key.
func NormalizeKey(key []byte) []byte {
	isWS := func(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
	for len(key) > 0 && isWS(key[0]) {
		key = key[1:]
	}
	for len(key) > 0 && isWS(key[len(key)-1]) {
		key = key[:len(key)-1]
	}
	return key
}

// SecretKeyDigest normalizes a secretRef-held API key, rejects it under
// MinKeyBytes (ErrShortKey) and returns its digest. It clears raw before
// returning, so the caller keeps no copy of the key (spec 06
// requirement 12: the raw key is discarded after hashing).
func SecretKeyDigest(raw []byte) ([32]byte, error) {
	defer clear(raw)
	k := NormalizeKey(raw)
	if len(k) < MinKeyBytes {
		return [32]byte{}, ErrShortKey
	}
	return Digest(k), nil
}
