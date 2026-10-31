// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwks

import (
	"bytes"
	"crypto"
	"crypto/elliptic"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
)

const testIssuer = "https://idp.example"

// TestNewKeysKeepsUsableFamilies checks that RSA 2,048, P-256 and Ed25519
// public keys are kept and found by kid and family, and that a key is not
// found under another family.
func TestNewKeysKeepsUsableFamilies(t *testing.T) {
	rs, es, ed := genKey(t, mockidp.RS256), genKey(t, mockidp.ES256), genKey(t, mockidp.EdDSA)
	ks, err := NewKeys(doc(t, rs.JWK(), es.JWK(), ed.JWK()), testIssuer, nil, t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ks.Len() != 3 || ks.Unusable() != 0 {
		t.Fatalf("Len, Unusable = %d, %d, want 3, 0", ks.Len(), ks.Unusable())
	}
	for _, tc := range []struct {
		k *mockidp.Key
		f Family
	}{{rs, FamilyRSA}, {es, FamilyP256}, {ed, FamilyEd25519}} {
		got, ok := ks.Lookup(tc.k.ID, tc.f)
		if !ok || got.ID != tc.k.ID || got.Family != tc.f || got.Alg != string(tc.k.Alg) {
			t.Errorf("Lookup(%s, %v) = %+v, %v", tc.k.Alg, tc.f, got, ok)
			continue
		}
		if !publicEqual(got.Public, tc.k.Public()) {
			t.Errorf("Lookup(%s) returned another public key", tc.k.Alg)
		}
	}
	if _, ok := ks.Lookup(rs.ID, FamilyP256); ok {
		t.Error("an RSA key was found under P-256")
	}
	if _, ok := ks.Lookup(rs.ID, FamilyNone); ok {
		t.Error("a key was found under FamilyNone")
	}
}

// publicEqual compares public keys with their Equal method.
func publicEqual(a, b crypto.PublicKey) bool {
	e, ok := a.(interface{ Equal(x crypto.PublicKey) bool })
	return ok && e.Equal(b)
}

// TestNewKeysSkipsUnusableKeys checks "JWKS public keys only: RSA of 2,048
// bits or more, P-256 or Ed25519": every other entry is skipped, a key
// whose use is not sig or whose key_ops lack verify included, and the
// usable keys of the same document stay.
func TestNewKeysSkipsUnusableKeys(t *testing.T) {
	good := genKey(t, mockidp.EdDSA)
	small := wrapKey(t, mockidp.RS256, "small", rsaKey(t, 1024))
	p384 := wrapKey(t, mockidp.ES256, "p384", ecKey(t, elliptic.P384()))
	enc := jwkMap(t, wrapKey(t, mockidp.EdDSA, "enc", edKey(t)))
	enc["use"] = "enc"
	signOnly := jwkMap(t, wrapKey(t, mockidp.EdDSA, "sign-only", edKey(t)))
	signOnly["key_ops"] = []string{"sign"}
	tests := []struct {
		name  string
		entry any
		kid   string
	}{
		{"RSA 1024", small.JWK(), "small"},
		{"P-384", p384.JWK(), "p384"},
		{"private RSA", privateRSAJWK(rsaKey(t, 2048), "private"), "private"},
		{"symmetric", map[string]any{"kty": "oct", "kid": "oct", "k": b64([]byte("0123456789abcdef0123456789abcdef"))}, "oct"},
		{"X25519", map[string]any{"kty": "OKP", "kid": "x25519", "crv": "X25519", "x": b64(make([]byte, 32))}, "x25519"},
		{"use enc", enc, "enc"},
		{"key_ops without verify", signOnly, "sign-only"},
		{"unknown kty", map[string]any{"kty": "XYZ", "kid": "xyz"}, "xyz"},
		{"not an object", "a string", ""},
		{"null", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ks, err := NewKeys(doc(t, good.JWK(), tt.entry), testIssuer, nil, t0, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if ks.Len() != 1 || ks.Unusable() != 1 {
				t.Fatalf("Len, Unusable = %d, %d, want 1, 1", ks.Len(), ks.Unusable())
			}
			for f := FamilyRSA; f < numFamilies; f++ {
				if k, ok := ks.Lookup(tt.kid, f); ok && tt.kid != "" {
					t.Errorf("unusable key found: %+v", k)
				}
			}
			if _, ok := ks.Lookup(good.ID, FamilyEd25519); !ok {
				t.Error("the usable key was dropped")
			}
		})
	}
}

// TestNewKeysKeyOpsWithVerify checks that a key whose key_ops include
// verify and whose use is sig is kept.
func TestNewKeysKeyOpsWithVerify(t *testing.T) {
	k := genKey(t, mockidp.EdDSA)
	m := jwkMap(t, k)
	m["key_ops"] = []string{"sign", "verify"}
	ks, err := NewKeys(doc(t, m), testIssuer, nil, t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ks.Lookup(k.ID, FamilyEd25519); !ok {
		t.Fatal("key with key_ops verify was dropped")
	}
}

// TestNewKeysRejectsDocument checks that a document that is not a strict
// JWK Set, holds more than 256 keys or 1 MiB, or no usable key, is a
// failed fetch.
func TestNewKeysRejectsDocument(t *testing.T) {
	good := genKey(t, mockidp.EdDSA)
	entry, err := jsonText(good.JWK())
	if err != nil {
		t.Fatal(err)
	}
	many := make([]any, MaxKeys+1)
	for i := range many {
		many[i] = map[string]any{"kty": "OKP", "crv": "Ed25519", "kid": fmt.Sprint(i), "x": b64(make([]byte, 32))}
	}
	tests := []struct {
		name string
		doc  []byte
		want error
	}{
		{"not JSON", []byte("{"), ErrDocument},
		{"array", []byte("[]"), ErrDocument},
		{"no keys member", []byte(`{"other": []}`), ErrDocument},
		{"keys not an array", []byte(`{"keys": {}}`), ErrDocument},
		{"duplicate keys member", []byte(`{"keys": [], "keys": [` + entry + `]}`), ErrDocument},
		{"duplicate member in a key", []byte(`{"keys": [{"kty": "OKP", "kty": "OKP"}]}`), ErrDocument},
		{"invalid UTF-8", []byte("{\"keys\": [], \"x\": \"\xff\"}"), ErrDocument},
		{"trailing data", []byte(`{"keys": [` + entry + `]} {}`), ErrDocument},
		{"more than 256 keys", doc(t, many...), ErrTooLarge},
		{"over 1 MiB", append([]byte(`{"keys": [`+entry+`], "pad": "`), append(bytes.Repeat([]byte("a"), MaxDocumentBytes), `"}`...)...), ErrTooLarge},
		{"no usable key", doc(t, map[string]any{"kty": "oct", "k": "AAAA"}), ErrNoUsableKeys},
		{"empty keys", []byte(`{"keys": []}`), ErrNoUsableKeys},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ks, err := NewKeys(tt.doc, testIssuer, nil, t0, time.Hour)
			if !errors.Is(err, tt.want) || ks != nil {
				t.Fatalf("NewKeys = %v, %v, want %v", ks, err, tt.want)
			}
		})
	}
}

// jsonText encodes v as JSON text.
func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// TestNewKeysAcceptsMaxKeys checks that exactly MaxKeys entries and
// members other than keys are accepted.
func TestNewKeysAcceptsMaxKeys(t *testing.T) {
	base := genKey(t, mockidp.EdDSA)
	entries := make([]any, MaxKeys)
	for i := range entries {
		m := jwkMap(t, base)
		m["kid"] = fmt.Sprint("k", i)
		entries[i] = m
	}
	d := doc(t, entries...)
	d = append(d[:len(d)-1], []byte(`, "other": {"nested": [1, 2, {"a": null}]}}`)...)
	ks, err := NewKeys(d, testIssuer, nil, t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ks.Len() != MaxKeys {
		t.Fatalf("Len = %d, want %d", ks.Len(), MaxKeys)
	}
}

// TestNewKeysSkipsRevokedKID checks that a key the Revoker lists by
// (issuer, kid) is not loaded, and that the listing is per issuer.
func TestNewKeysSkipsRevokedKID(t *testing.T) {
	a, b := genKey(t, mockidp.EdDSA), genKey(t, mockidp.EdDSA)
	r := revoked{{testIssuer, a.ID}: true}
	ks, err := NewKeys(doc(t, a.JWK(), b.JWK()), testIssuer, r, t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ks.Lookup(a.ID, FamilyEd25519); ok {
		t.Error("revoked key loaded")
	}
	if _, ok := ks.Lookup(b.ID, FamilyEd25519); !ok {
		t.Error("key that is not revoked dropped")
	}
	other, err := NewKeys(doc(t, a.JWK()), "https://other.example", r, t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := other.Lookup(a.ID, FamilyEd25519); !ok {
		t.Error("a revocation of one issuer dropped another issuer's key")
	}
	// Every key revoked: no usable key.
	if _, err := NewKeys(doc(t, a.JWK()), testIssuer, r, t0, time.Hour); !errors.Is(err, ErrNoUsableKeys) {
		t.Errorf("all keys revoked: %v, want ErrNoUsableKeys", err)
	}
}

// TestNewKeysRepeatedKID checks that the first of two keys sharing kid and
// family wins, while keys sharing only the kid across families are both
// kept (RFC 7517 section 4.5).
func TestNewKeysRepeatedKID(t *testing.T) {
	first := wrapKey(t, mockidp.EdDSA, "same", edKey(t))
	second := wrapKey(t, mockidp.EdDSA, "same", edKey(t))
	ec := wrapKey(t, mockidp.ES256, "same", ecKey(t, elliptic.P256()))
	ks, err := NewKeys(doc(t, first.JWK(), second.JWK(), ec.JWK()), testIssuer, nil, t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ks.Len() != 2 || ks.Unusable() != 1 {
		t.Fatalf("Len, Unusable = %d, %d, want 2, 1", ks.Len(), ks.Unusable())
	}
	if k, _ := ks.Lookup("same", FamilyEd25519); k == nil || !publicEqual(k.Public, first.Public()) {
		t.Error("the first of two keys sharing kid and family did not win")
	}
	if _, ok := ks.Lookup("same", FamilyP256); !ok {
		t.Error("a key sharing only the kid was dropped")
	}
}

// TestOfFamily checks the keys a token without a kid tries: every usable
// key of its family, in document order, with or without a kid; none for a
// family the set lacks. An empty kid names no key.
func TestOfFamily(t *testing.T) {
	a, b := genKey(t, mockidp.EdDSA), genKey(t, mockidp.EdDSA)
	c := jwkMap(t, genKey(t, mockidp.EdDSA))
	delete(c, "kid")
	es := genKey(t, mockidp.ES256)
	ks, err := NewKeys(doc(t, a.JWK(), es.JWK(), c, b.JWK()), testIssuer, nil, t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got := ks.OfFamily(FamilyEd25519)
	if len(got) != 3 || got[0].ID != a.ID || got[1].ID != "" || got[2].ID != b.ID {
		t.Fatalf("OfFamily(Ed25519) = %v, want a, the key without kid, b", got)
	}
	if p256 := ks.OfFamily(FamilyP256); len(p256) != 1 || p256[0].ID != es.ID {
		t.Fatalf("OfFamily(P-256) = %v", p256)
	}
	if len(ks.OfFamily(FamilyRSA)) != 0 || ks.OfFamily(FamilyNone) != nil || ks.OfFamily(numFamilies) != nil {
		t.Fatal("OfFamily found keys of a family the set lacks")
	}
	if k, ok := ks.Lookup("", FamilyEd25519); ok {
		t.Fatalf("Lookup with an empty kid = %v", k)
	}
}

// TestNewKeysKeepsKeysWithoutKID checks that keys without a kid are all
// kept, however many share a family: the repeated (kid, family) rule
// applies to kids only.
func TestNewKeysKeepsKeysWithoutKID(t *testing.T) {
	var entries []any
	for range 3 {
		m := jwkMap(t, genKey(t, mockidp.EdDSA))
		delete(m, "kid")
		entries = append(entries, m)
	}
	ks, err := NewKeys(doc(t, entries...), testIssuer, nil, t0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if ks.Len() != 3 || ks.Unusable() != 0 || len(ks.OfFamily(FamilyEd25519)) != 3 {
		t.Fatalf("Len, Unusable, OfFamily = %d, %d, %d, want 3, 0, 3", ks.Len(), ks.Unusable(), len(ks.OfFamily(FamilyEd25519)))
	}
}

// TestKeysLifetime checks the fetch stamp: a set expires lifetime after it
// was fetched.
func TestKeysLifetime(t *testing.T) {
	ks, err := NewKeys(doc(t, genKey(t, mockidp.EdDSA).JWK()), testIssuer, nil, t0, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !ks.Fetched().Equal(t0) || !ks.Expires().Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("Fetched, Expires = %v, %v", ks.Fetched(), ks.Expires())
	}
	if ks.Expired(t0.Add(10*time.Minute-time.Nanosecond)) || !ks.Expired(t0.Add(10*time.Minute)) {
		t.Fatal("Expired does not switch at Expires")
	}
}

// TestFamilyString checks the family names used in messages.
func TestFamilyString(t *testing.T) {
	for f, want := range map[Family]string{FamilyNone: "none", FamilyRSA: "RSA", FamilyP256: "P-256", FamilyEd25519: "Ed25519", numFamilies: "none"} {
		if got := f.String(); got != want {
			t.Errorf("Family(%d).String() = %q, want %q", f, got, want)
		}
	}
}
