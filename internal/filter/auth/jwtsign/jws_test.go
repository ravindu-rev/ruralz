// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/asn1"
	"encoding/base64"
	"math/big"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the token form (docs/architecture/08-security-and-identity.md
// "JWT signing"; RFC 7515, RFC 7518, RFC 8037).

// algCase is one algorithm with a key that fits it.
type algCase struct {
	alg   v1alpha1.JWTSignAlgorithm
	value []byte
	pub   any
}

// algCases returns every algorithm with a PEM key that fits it.
func algCases(t *testing.T) []algCase {
	k := keys(t)
	return []algCase{
		{v1alpha1.JWTSignAlgorithmRS256, pkcs8(t, k.rsa), k.rsa.Public()},
		{v1alpha1.JWTSignAlgorithmPS256, pkcs8(t, k.rsa), k.rsa.Public()},
		{v1alpha1.JWTSignAlgorithmES256, pkcs8(t, k.p256), k.p256.Public()},
		{v1alpha1.JWTSignAlgorithmEdDSA, pkcs8(t, k.ed), k.ed.Public()},
	}
}

// TestHeader: the JOSE header holds alg, then kid when set, written
// compactly, base64url without padding and followed by its dot.
func TestHeader(t *testing.T) {
	cases := []struct {
		alg  v1alpha1.JWTSignAlgorithm
		kid  string
		want string
	}{
		{v1alpha1.JWTSignAlgorithmRS256, "", `{"alg":"RS256"}`},
		{v1alpha1.JWTSignAlgorithmES256, "edge-2026-11", `{"alg":"ES256","kid":"edge-2026-11"}`},
		{v1alpha1.JWTSignAlgorithmEdDSA, `k"1\`, `{"alg":"EdDSA","kid":"k\"1\\"}`},
	}
	for _, c := range cases {
		h, err := header(c.alg, c.kid)
		if err != nil {
			t.Fatalf("header(%s, %q) = %v", c.alg, c.kid, err)
		}
		if !bytes.HasSuffix(h, []byte(".")) || bytes.ContainsAny(h, "=+/") {
			t.Errorf("header(%s, %q) = %s, want unpadded base64url and a dot", c.alg, c.kid, h)
		}
		got, err := base64.RawURLEncoding.Strict().DecodeString(string(h[:len(h)-1]))
		if err != nil || string(got) != c.want {
			t.Errorf("header(%s, %q) decodes to %s, %v; want %s", c.alg, c.kid, got, err, c.want)
		}
	}
	if _, err := header("HS256", ""); err == nil {
		t.Error("header accepted HS256")
	}
	if _, err := header(v1alpha1.JWTSignAlgorithmRS256, "\xff"); err == nil {
		t.Error("header accepted a kid that is not UTF-8")
	}
}

// TestEachAlgorithmSignsVerifiably: a token of each algorithm verifies
// with the standard library under the key's public key, carries the kid
// and the claims byte for byte, and takes the JWS compact form.
func TestEachAlgorithmSignsVerifiably(t *testing.T) {
	claims := []byte(`{"sub":"alice","exp":1.7629E9,"scope":["a","b"]}`)
	for _, c := range algCases(t) {
		t.Run(string(c.alg), func(t *testing.T) {
			k, err := parseKey(c.value)
			if err != nil {
				t.Fatal(err)
			}
			head, err := header(c.alg, "k1")
			if err != nil {
				t.Fatal(err)
			}
			s := signer{alg: c.alg, head: head}
			tok, err := s.appendToken([]byte("prefix:"), k, claims)
			if err != nil {
				t.Fatalf("appendToken: %v", err)
			}
			if !bytes.HasPrefix(tok, []byte("prefix:")) {
				t.Fatalf("appendToken dropped dst: %s", tok)
			}
			tok = tok[len("prefix:"):]
			if bytes.ContainsAny(tok, "=+/\"") {
				t.Errorf("token %s is not unpadded base64url", tok)
			}
			p := verify(t, string(tok), c.alg, c.pub)
			if p.header["kid"] != "k1" || len(p.header) != 2 {
				t.Errorf("header = %v, want alg and kid k1", p.header)
			}
			if !bytes.Equal(p.payload, claims) {
				t.Errorf("payload = %s, want the claims %s", p.payload, claims)
			}
			// RS256 and EdDSA are deterministic: the same claims sign the
			// same token.
			again, err := s.appendToken(nil, k, claims)
			if err != nil {
				t.Fatal(err)
			}
			deterministic := c.alg == v1alpha1.JWTSignAlgorithmRS256 || c.alg == v1alpha1.JWTSignAlgorithmEdDSA
			if deterministic != bytes.Equal(again, tok) {
				t.Errorf("signing twice gave equal tokens %v, want %v", bytes.Equal(again, tok), deterministic)
			}
			verify(t, string(again), c.alg, c.pub)
		})
	}
}

// TestSignRefusesAnotherFamily: a signer never signs with a key of
// another family, and the failed token leaves dst's length.
func TestSignRefusesAnotherFamily(t *testing.T) {
	k, err := parseKey(pkcs8(t, keys(t).p256))
	if err != nil {
		t.Fatal(err)
	}
	head, _ := header(v1alpha1.JWTSignAlgorithmRS256, "")
	s := signer{alg: v1alpha1.JWTSignAlgorithmRS256, head: head}
	out, err := s.appendToken([]byte("x"), k, []byte(`{}`))
	if err == nil || string(out) != "x" {
		t.Errorf("appendToken with a P-256 key for RS256 = %q, %v; want an error and dst kept", out, err)
	}
	if _, err := s.sign(nil, nil); err == nil {
		t.Error("sign with no key succeeded")
	}
}

// TestRawECDSA: an ASN.1 ECDSA signature becomes R and S as 32-byte
// big-endian integers, short values left-padded; malformed ones are
// refused.
func TestRawECDSA(t *testing.T) {
	type sig struct{ R, S *big.Int }
	der, err := asn1.Marshal(sig{big.NewInt(1), new(big.Int).Lsh(big.NewInt(1), 255)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := rawECDSA(der)
	if err != nil || len(got) != 64 || got[31] != 1 || got[32] != 0x80 || !bytes.Equal(got[:31], make([]byte, 31)) {
		t.Fatalf("rawECDSA = %x, %v", got, err)
	}
	tooBig, _ := asn1.Marshal(sig{new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)})
	zero, _ := asn1.Marshal(sig{big.NewInt(0), big.NewInt(1)})
	for name, in := range map[string][]byte{
		"R over 256 bits": tooBig,
		"zero R":          zero,
		"trailing bytes":  append(append([]byte{}, der...), 0),
		"not ASN.1":       []byte("nope"),
	} {
		if _, err := rawECDSA(in); err == nil {
			t.Errorf("rawECDSA(%s) accepted", name)
		}
	}
	// A real P-256 signature converts to a form ecdsa.Verify accepts.
	k := keys(t).p256
	digest := bytes.Repeat([]byte{9}, 32)
	asn, err := ecdsa.SignASN1(rand.Reader, k, digest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := rawECDSA(asn)
	if err != nil {
		t.Fatal(err)
	}
	if !ecdsa.Verify(&k.PublicKey, digest, new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:])) {
		t.Error("the converted signature does not verify")
	}
	if k.Curve != elliptic.P256() || !strings.Contains(familyP256.String(), "P-256") {
		t.Fatal("the test key is not P-256")
	}
}
