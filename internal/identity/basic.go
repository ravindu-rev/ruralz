// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// PBKDF2 iteration bounds of credentials.basic[].iterations (Security and
// identity, "Basic and mTLS schemas"; spec 06 requirements 20 and 42).
const (
	// MinIterations is the lowest accepted count.
	MinIterations = 600_000
	// MaxIterations is the highest accepted count.
	MaxIterations = 1_000_000
	// DefaultIterations applies when iterations is absent; the throttle's
	// dummy hash uses it too.
	DefaultIterations = 600_000
)

// BasicHashPrefix starts a stored auth.basic hash.
const BasicHashPrefix = "pbkdf2-sha256:"

// ErrBasicHashSyntax rejects a stored hash that is not
// pbkdf2-sha256:<salt>:<key> with a 16-byte salt and a 32-byte key in
// unpadded base64url.
var ErrBasicHashSyntax = errors.New("identity: basic hash is not pbkdf2-sha256:<22 base64url>:<43 base64url>")

// BasicCredential is one decoded credentials.basic entry.
type BasicCredential struct {
	// Consumer is the Consumer the username binds.
	Consumer *expr.Consumer
	// Username is the byte-exact username.
	Username string
	// Salt is the PBKDF2 salt.
	Salt [16]byte
	// Key is the expected PBKDF2-HMAC-SHA-256 output.
	Key [32]byte
	// Iterations is the PBKDF2 iteration count.
	Iterations int
	// Digest identifies the credential for the auth.basic success cache
	// (spec 06 requirement 43): CredentialDigest(hash, iterations). A cache
	// entry whose recorded digest differs from the current snapshot's is a
	// miss, so a swap evicts only changed or removed credentials.
	Digest [32]byte
}

// ParseBasicHash decodes "pbkdf2-sha256:<salt>:<key>": base64url without
// padding of a 16-byte salt (22 characters) and a 32-byte key (43
// characters).
func ParseBasicHash(s string) (salt [16]byte, key [32]byte, err error) {
	rest, ok := strings.CutPrefix(s, BasicHashPrefix)
	if !ok {
		return salt, key, ErrBasicHashSyntax
	}
	saltText, keyText, ok := strings.Cut(rest, ":")
	enc := base64.RawURLEncoding.Strict()
	if !ok || len(saltText) != enc.EncodedLen(len(salt)) || len(keyText) != enc.EncodedLen(len(key)) {
		return salt, key, ErrBasicHashSyntax
	}
	if n, err := enc.Decode(salt[:], []byte(saltText)); err != nil || n != len(salt) {
		return salt, key, ErrBasicHashSyntax
	}
	if n, err := enc.Decode(key[:], []byte(keyText)); err != nil || n != len(key) {
		return salt, key, ErrBasicHashSyntax
	}
	return salt, key, nil
}

// CredentialDigest is the success-cache identity of a basic credential
// (spec 06 requirement 43, proposed definition): SHA-256 over the stored
// hash text, one zero byte, and the decimal iteration count.
func CredentialDigest(hash string, iterations int) [32]byte {
	h := sha256.New()
	_, _ = h.Write([]byte(hash))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(strconv.AppendInt(nil, int64(iterations), 10))
	var d [32]byte
	h.Sum(d[:0])
	return d
}

// Iterations returns the effective iteration count of a credential:
// DefaultIterations when absent.
func Iterations(n *int32) int {
	if n == nil {
		return DefaultIterations
	}
	return int(*n)
}

// ValidIterations reports whether n lies in [MinIterations, MaxIterations].
func ValidIterations(n int) bool { return n >= MinIterations && n <= MaxIterations }
