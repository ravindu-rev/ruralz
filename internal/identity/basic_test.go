// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// Tests for spec 06 requirements 19, 20 and 42 to 43 on the index side:
// the stored pbkdf2-sha256:<salt>:<key> form, the iteration bounds and
// default, and the credential digest the auth.basic success cache records.

func TestReq42ParseBasicHash(t *testing.T) {
	salt22 := strings.Repeat("A", 22)
	key43 := strings.Repeat("A", 43)
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"configuration model example", cfgBasicHash, true},
		{"zero salt and key", BasicHashPrefix + salt22 + ":" + key43, true},
		{"wrong prefix", "pbkdf2-sha512:" + salt22 + ":" + key43, false},
		{"no prefix", salt22 + ":" + key43, false},
		{"missing key", BasicHashPrefix + salt22, false},
		{"short salt", BasicHashPrefix + salt22[1:] + ":" + key43, false},
		{"long key", BasicHashPrefix + salt22 + ":" + key43 + "A", false},
		{"padded", BasicHashPrefix + salt22 + "==:" + key43, false},
		{"standard alphabet", BasicHashPrefix + "+" + salt22[1:] + ":" + key43, false},
		{"non-zero trailing salt bits", BasicHashPrefix + salt22[:21] + "B:" + key43, false},
		{"non-zero trailing key bits", BasicHashPrefix + salt22 + ":" + key43[:42] + "B", false},
		{"extra field", BasicHashPrefix + salt22 + ":" + key43 + ":x", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := ParseBasicHash(tt.in)
			if (err == nil) != tt.ok {
				t.Fatalf("ParseBasicHash(%q) = %v, want ok %v", tt.in, err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrBasicHashSyntax) {
				t.Fatalf("error %v is not ErrBasicHashSyntax", err)
			}
		})
	}
}

// A hash computed with crypto/pbkdf2 decodes to the salt and key used.
func TestReq42ParseBasicHashDecodes(t *testing.T) {
	salt := []byte("0123456789abcdef")
	key, err := pbkdf2.Key(sha256.New, "correct horse", salt, 1000, 32)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding
	s, k, err := ParseBasicHash(BasicHashPrefix + enc.EncodeToString(salt) + ":" + enc.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	if string(s[:]) != string(salt) || string(k[:]) != string(key) {
		t.Fatal("decoded salt or key differs")
	}
}

func TestReq20Iterations(t *testing.T) {
	if Iterations(nil) != DefaultIterations || Iterations(i32(700000)) != 700000 {
		t.Fatal("Iterations default or value wrong")
	}
	tests := []struct {
		n  int
		ok bool
	}{
		{599_999, false}, {600_000, true}, {800_000, true}, {1_000_000, true}, {1_000_001, false}, {0, false}, {-1, false},
	}
	for _, tt := range tests {
		if ValidIterations(tt.n) != tt.ok {
			t.Errorf("ValidIterations(%d) = %v, want %v", tt.n, !tt.ok, tt.ok)
		}
	}
}

func TestReq43CredentialDigest(t *testing.T) {
	a := CredentialDigest(cfgBasicHash, 600000)
	if a != CredentialDigest(cfgBasicHash, 600000) {
		t.Fatal("CredentialDigest is not deterministic")
	}
	if a == CredentialDigest(cfgBasicHash, 600001) {
		t.Fatal("an iteration change kept the credential digest")
	}
	other := BasicHashPrefix + strings.Repeat("A", 22) + ":" + strings.Repeat("A", 43)
	if a == CredentialDigest(other, 600000) {
		t.Fatal("a hash change kept the credential digest")
	}
	// The separator keeps "hash" + "6" and "hash6" + "" distinct.
	if CredentialDigest("x1", 23) == CredentialDigest("x", 123) {
		t.Fatal("digest input is ambiguous")
	}
}

// FuzzParseBasicHash: ParseBasicHash never panics, and anything it accepts
// re-encodes to the same text (one canonical form per credential).
func FuzzParseBasicHash(f *testing.F) {
	f.Add(cfgBasicHash)
	f.Add(BasicHashPrefix + ":")
	f.Add(BasicHashPrefix + strings.Repeat("_", 22) + ":" + strings.Repeat("-", 43))
	f.Fuzz(func(t *testing.T, s string) {
		salt, key, err := ParseBasicHash(s)
		if err != nil {
			return
		}
		enc := base64.RawURLEncoding
		if got := BasicHashPrefix + enc.EncodeToString(salt[:]) + ":" + enc.EncodeToString(key[:]); got != s {
			t.Fatalf("accepted %q, which re-encodes to %q", s, got)
		}
	})
}
