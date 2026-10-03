// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

// Tests for spec 06 requirements 13 (normalization and the 22-byte floor
// of secretRef-held keys; the raw key is discarded) and 14 (SHA-256 over
// the exact header bytes, stored as sha256:<64 lowercase hex>).

func TestReq14ParseDigest(t *testing.T) {
	good := "sha256:" + strings.Repeat("0123456789abcdef", 4)
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"valid", good, true},
		{"uppercase hex", strings.ToUpper(good[:7]) + strings.ToUpper(good[7:]), false},
		{"uppercase digit", good[:10] + "A" + good[11:], false},
		{"63 hex", good[:len(good)-1], false},
		{"65 hex", good + "0", false},
		{"no prefix", good[7:], false},
		{"sha512 prefix", "sha512:" + good[7:], false},
		{"display form", "rev-0123456789ab", false},
		{"non-hex", good[:20] + "g" + good[21:], false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := ParseDigest(tt.in)
			if (err == nil) != tt.ok {
				t.Fatalf("ParseDigest(%q) error = %v, want ok %v", tt.in, err, tt.ok)
			}
			if !tt.ok {
				if !errors.Is(err, ErrDigestSyntax) {
					t.Fatalf("error %v is not ErrDigestSyntax", err)
				}
				return
			}
			if FormatDigest(d) != tt.in {
				t.Fatalf("FormatDigest(ParseDigest(%q)) = %q", tt.in, FormatDigest(d))
			}
		})
	}
}

func TestReq14DigestIsSHA256OfExactBytes(t *testing.T) {
	for _, key := range []string{"", " padded ", testKeyA, strings.Repeat("k", 256), strings.Repeat("k", 257), strings.Repeat("x", 4096)} {
		want := sha256.Sum256([]byte(key))
		if Digest([]byte(key)) != want || DigestString(key) != want {
			t.Errorf("digest of %d-byte key differs from SHA-256", len(key))
		}
	}
	// The header value is hashed as delivered: surrounding spaces count.
	if DigestString(" "+testKeyA) == DigestString(testKeyA) {
		t.Error("the header digest trimmed its value")
	}
}

func TestReq14DigestStringAllocatesNothing(t *testing.T) {
	var sink [32]byte
	if n := testing.AllocsPerRun(100, func() { sink = DigestString(testKeyA) }); n != 0 {
		t.Fatalf("DigestString allocates %v times", n)
	}
	_ = sink
}

func TestReq13NormalizeKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{testKeyA, testKeyA},
		{" \t" + testKeyA + "\r\n", testKeyA},
		{"\n\n", ""},
		{"a b", "a b"},
		{"\va\v", "\va\v"}, // only SP, HTAB, CR and LF are trimmed
		{"", ""},
	}
	for _, tt := range tests {
		if got := string(NormalizeKey([]byte(tt.in))); got != tt.want {
			t.Errorf("NormalizeKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReq13SecretKeyDigest(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want [32]byte
		err  error
	}{
		{"trimmed before hashing", "  " + testKeyA + "\n", DigestString(testKeyA), nil},
		{"22 bytes is the floor", strings.Repeat("k", 22), DigestString(strings.Repeat("k", 22)), nil},
		{"21 bytes is short", strings.Repeat("k", 21), [32]byte{}, ErrShortKey},
		{"whitespace does not count", " " + strings.Repeat("k", 21) + "\t", [32]byte{}, ErrShortKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(tt.raw)
			d, err := SecretKeyDigest(raw)
			if !errors.Is(err, tt.err) || d != tt.want {
				t.Fatalf("SecretKeyDigest = %x, %v; want %x, %v", d, err, tt.want, tt.err)
			}
			// Requirement 12: the raw key is discarded after hashing.
			if !bytes.Equal(raw, make([]byte, len(raw))) {
				t.Fatal("SecretKeyDigest left the raw key in its input")
			}
			if err != nil && strings.Contains(err.Error(), "kkk") {
				t.Fatal("the error carries key bytes")
			}
		})
	}
}

// FuzzParseAPIKeyDigest: ParseDigest never panics, and accepts exactly the
// schema pattern ^sha256:[0-9a-f]{64}$, round-tripping through FormatDigest.
func FuzzParseAPIKeyDigest(f *testing.F) {
	f.Add("sha256:" + strings.Repeat("ab", 32))
	f.Add("sha256:" + strings.Repeat("AB", 32))
	f.Add("sha256:")
	f.Add("rev-0123456789ab")
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDigest(s)
		if err != nil {
			return
		}
		if FormatDigest(d) != s {
			t.Fatalf("accepted %q, which does not round-trip", s)
		}
	})
}
