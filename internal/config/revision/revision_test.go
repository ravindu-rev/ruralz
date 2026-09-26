// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package revision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Tests for architecture section 2.6 (WP-01) and spec 02 section 2.6
// (requirements 29 to 34): the digest is SHA-256 over the exact canonical
// bytes, written sha256:<64 lowercase hex>; rev-<12 hex> is display only.

// emptySum is SHA-256 of no bytes.
const emptySum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestSum(t *testing.T) {
	d := Sum(nil)
	if d.Hex() != emptySum {
		t.Fatalf("Sum(nil).Hex() = %s", d.Hex())
	}
	if d.String() != "sha256:"+emptySum {
		t.Fatalf("String = %s", d.String())
	}
	if d.Short() != "rev-e3b0c44298fc" {
		t.Fatalf("Short = %s", d.Short())
	}
	if d.IsZero() {
		t.Fatal("the digest of empty content is the zero Digest")
	}
	content := []byte(`{"apiVersion":"ruralz/v1alpha1"}`)
	want := sha256.Sum256(content)
	got := Sum(content)
	if got.Bytes() != want || got.Hex() != hex.EncodeToString(want[:]) {
		t.Fatalf("Sum = %s, want %x", got, want)
	}
	// Bytes returns a copy.
	b := got.Bytes()
	b[0] ^= 0xff
	if got.Bytes() != want {
		t.Fatal("Bytes aliased the digest")
	}
}

func TestShortFormat(t *testing.T) {
	// Short is "rev-" plus the first 12 lowercase hex characters.
	for _, content := range []string{"", "a", "ruralz", strings.Repeat("x", 4096)} {
		d := Sum([]byte(content))
		s := d.Short()
		if len(s) != len("rev-")+12 || !strings.HasPrefix(s, "rev-") || s[4:] != d.Hex()[:12] {
			t.Fatalf("Short = %q for %s", s, d)
		}
		if h, err := ParseDisplay(s); err != nil || h != d.Hex()[:12] {
			t.Fatalf("ParseDisplay(%q) = %q, %v", s, h, err)
		}
	}
}

func TestZero(t *testing.T) {
	var d Digest
	if !d.IsZero() {
		t.Fatal("zero Digest is not IsZero")
	}
	if d.String() != "sha256:"+strings.Repeat("0", 64) {
		t.Fatalf("zero String = %s", d.String())
	}
}

func TestParse(t *testing.T) {
	valid := "sha256:" + emptySum
	d, err := Parse(valid)
	if err != nil || d != Sum(nil) {
		t.Fatalf("Parse(%q) = %v, %v", valid, d, err)
	}
	bad := []struct {
		name, in string
	}{
		{"empty", ""},
		{"missing prefix", emptySum},
		{"display form", "rev-e3b0c44298fc"},
		{"upper-case prefix", "SHA256:" + emptySum},
		{"other algorithm", "sha512:" + emptySum},
		{"upper-case hex", "sha256:" + strings.ToUpper(emptySum)},
		{"one upper-case digit", "sha256:E" + emptySum[1:]},
		{"63 hex", "sha256:" + emptySum[:63]},
		{"65 hex", "sha256:" + emptySum + "0"},
		{"non-hex", "sha256:g" + emptySum[1:]},
		{"space", "sha256: " + emptySum[1:]},
		{"trailing newline", valid + "\n"},
		{"prefix only", "sha256:"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			d, err := Parse(tt.in)
			if !errors.Is(err, ErrSyntax) {
				t.Fatalf("Parse(%q) error = %v, want ErrSyntax", tt.in, err)
			}
			if !d.IsZero() {
				t.Fatalf("Parse(%q) returned %v with its error", tt.in, d)
			}
		})
	}
}

func TestParseDisplay(t *testing.T) {
	if h, err := ParseDisplay("rev-162af81f5de4"); err != nil || h != "162af81f5de4" {
		t.Fatalf("ParseDisplay = %q, %v", h, err)
	}
	for _, in := range []string{"", "162af81f5de4", "rev-162AF81F5DE4", "rev-162af81f5de", "rev-162af81f5de45", "rev-162af81f5dez", "sha256:" + emptySum} {
		if h, err := ParseDisplay(in); err == nil || h != "" {
			t.Errorf("ParseDisplay(%q) = %q, %v; want an error", in, h, err)
		}
	}
}

func TestTextRoundTrip(t *testing.T) {
	d := Sum([]byte("canonical"))
	text, err := d.MarshalText()
	if err != nil || string(text) != d.String() {
		t.Fatalf("MarshalText = %s, %v", text, err)
	}
	var back Digest
	if err := back.UnmarshalText(text); err != nil || back != d {
		t.Fatalf("UnmarshalText = %v, %v", back, err)
	}
	// A failed UnmarshalText leaves the value unchanged.
	if err := back.UnmarshalText([]byte("rev-" + d.Hex()[:12])); !errors.Is(err, ErrSyntax) {
		t.Fatalf("UnmarshalText(display) = %v, want ErrSyntax", err)
	}
	if back != d {
		t.Fatal("failed UnmarshalText modified the digest")
	}

	// Through encoding/json, as /debug/snapshots and pointers carry it.
	type doc struct {
		Digest Digest `json:"digest"`
	}
	b, err := json.Marshal(doc{Digest: d})
	if err != nil || string(b) != `{"digest":"`+d.String()+`"}` {
		t.Fatalf("json.Marshal = %s, %v", b, err)
	}
	var got doc
	if err := json.Unmarshal(b, &got); err != nil || got.Digest != d {
		t.Fatalf("json.Unmarshal = %v, %v", got.Digest, err)
	}
	if err := json.Unmarshal([]byte(`{"digest":"sha256:ABC"}`), &got); err == nil {
		t.Fatal("json.Unmarshal accepted a malformed digest")
	}
}

func TestRevision(t *testing.T) {
	content := []byte(`{"kind":"Gateway"}`)
	r := Revision{Digest: Sum(content), Content: content}
	if r.Digest != Sum(r.Content) {
		t.Fatal("Revision digest does not cover its content")
	}
}
