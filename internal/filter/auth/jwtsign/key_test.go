// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the signing key (docs/architecture/08-security-and-identity.md
// "JWT signing"): a private JWK or an unencrypted PEM key, RSA of at least
// 2,048 bits, P-256 or Ed25519, never a public key, fitting the algorithm.

// keyCase is one signing key value.
type keyCase struct {
	name  string
	value []byte
	// fam is the family the value parses to; 0 when parseKey refuses it
	// with err (errors.Is), or with a JWK member error when member is set.
	fam    family
	err    error
	member string
}

// keyCases are the values of the parse tests.
func keyCases(t testing.TB) []keyCase {
	k := keys(t)
	rsaPrivate := rsaMembers(k.rsa)
	ec := ecMembers(t, k.p256)
	okp := okpMembers(k.ed)
	otherEC := ecMembers(t, keys(t).p256)
	otherEC["d"] = b(bytes.Repeat([]byte{1}, 32))
	wrongD := new(big.Int).Add(k.rsa.D, big.NewInt(2))
	pkcs1 := pemOf(pemPKCS1, x509.MarshalPKCS1PrivateKey(k.rsa))
	encrypted := pem.EncodeToMemory(&pem.Block{Type: pemPKCS1, Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-256-CBC,00"}, Bytes: []byte{1}})
	cert := pemOf(pemCertificate, []byte{0x30})
	return []keyCase{
		// PEM keys.
		{name: "PKCS #8 RSA", value: pkcs8(t, k.rsa), fam: familyRSA},
		{name: "PKCS #1 RSA", value: pkcs1, fam: familyRSA},
		{name: "PKCS #8 P-256", value: pkcs8(t, k.p256), fam: familyP256},
		{name: "SEC 1 P-256", value: sec1(t, k.p256), fam: familyP256},
		{name: "PKCS #8 Ed25519", value: pkcs8(t, k.ed), fam: familyEd25519},
		{name: "text outside the block is ignored", value: append([]byte("canary line\n"), pkcs8(t, k.ed)...), fam: familyEd25519},
		{name: "a certificate beside the key", value: append(append([]byte{}, cert...), pkcs8(t, k.p256)...), fam: familyP256},
		{name: "PEM public key", value: spki(t, k.rsa.Public()), err: errPublicKey},
		{name: "PEM certificate only", value: cert, err: errPublicKey},
		{name: "encrypted PKCS #8", value: pemOf(pemEncrypted, []byte{1}), err: errEncryptedKey},
		{name: "encrypted legacy PEM", value: encrypted, err: errEncryptedKey},
		{name: "two private keys", value: append(pkcs8(t, k.ed), pkcs8(t, k.p256)...), err: errTwoKeys},
		{name: "unparsable PKCS #8", value: pemOf(pemPKCS8, []byte("not DER")), err: errBadPEMKey},
		{name: "OpenSSH key", value: pemOf("OPENSSH PRIVATE KEY", []byte{1}), err: errBadPEMKey},
		{name: "P-384 key", value: pkcs8(t, k.p384), err: errKeyType},
		{name: "1,024-bit RSA key", value: pkcs8(t, k.rsaSmall), err: errRSASize},
		{name: "no key at all", value: []byte("hunter2"), err: errNotKey},
		{name: "empty", value: []byte(" \n"), err: errEmptyKey},
		// JWKs.
		{name: "RSA JWK with CRT values", value: jwk(t, rsaPrivate, nil), fam: familyRSA},
		{name: "RSA JWK without primes", value: jwk(t, rsaPrivate, map[string]any{"p": nil, "q": nil, "dp": nil, "dq": nil, "qi": nil}), fam: familyRSA},
		{name: "EC JWK", value: jwk(t, ec, nil), fam: familyP256},
		{name: "OKP JWK", value: jwk(t, okp, nil), fam: familyEd25519},
		{name: "JWK with use sig, key_ops sign and unknown members", value: jwk(t, okp, map[string]any{"use": "sig", "key_ops": []string{"verify", "sign"}, "kid": "k1", "x-canary": "c"}), fam: familyEd25519},
		{name: "JWK with whitespace around it", value: append(append([]byte("\n  "), jwk(t, okp, nil)...), '\n'), fam: familyEd25519},
		{name: "public RSA JWK", value: jwk(t, rsaPrivate, map[string]any{"d": nil, "p": nil, "q": nil, "dp": nil, "dq": nil, "qi": nil}), err: errPublicKey},
		{name: "public EC JWK", value: jwk(t, ec, map[string]any{"d": nil}), err: errPublicKey},
		{name: "public OKP JWK", value: jwk(t, okp, map[string]any{"d": nil}), err: errPublicKey},
		{name: "JWK Set", value: []byte(`{"keys":[` + string(jwk(t, okp, nil)) + `]}`), err: errJWKSet},
		{name: "use enc", value: jwk(t, okp, map[string]any{"use": "enc"}), err: errJWKUse},
		{name: "key_ops without sign", value: jwk(t, okp, map[string]any{"key_ops": []string{"verify"}}), err: errJWKOps},
		{name: "key_ops not strings", value: jwk(t, okp, map[string]any{"key_ops": []any{1}}), member: "key_ops"},
		{name: "kty absent", value: jwk(t, okp, map[string]any{"kty": nil}), member: "kty"},
		{name: "kty oct", value: []byte(`{"kty":"oct","k":"c2VjcmV0"}`), err: errKeyType},
		{name: "EC JWK on P-384", value: jwk(t, ec, map[string]any{"crv": "P-384"}), err: errKeyType},
		{name: "OKP JWK on X25519", value: jwk(t, okp, map[string]any{"crv": "X25519"}), err: errKeyType},
		{name: "RSA JWK with some CRT values", value: jwk(t, rsaPrivate, map[string]any{"qi": nil}), err: errJWKPrimes},
		{name: "RSA JWK with oth", value: jwk(t, rsaPrivate, map[string]any{"oth": []any{}}), err: errJWKOth},
		{name: "RSA JWK whose d is not its key's", value: jwk(t, rsaPrivate, map[string]any{"d": u(wrongD), "p": nil, "q": nil, "dp": nil, "dq": nil, "qi": nil}), err: errRSAKey},
		{name: "RSA JWK with an inconsistent CRT value", value: jwk(t, rsaPrivate, map[string]any{"dp": u(big.NewInt(3))}), err: errRSAKey},
		{name: "1,024-bit RSA JWK", value: jwk(t, rsaMembers(k.rsaSmall), nil), err: errRSASize},
		{name: "EC JWK whose x and y are not its d's", value: jwk(t, otherEC, nil), err: errJWKPublic},
		{name: "OKP JWK whose x is not its d's", value: jwk(t, okp, map[string]any{"x": b(bytes.Repeat([]byte{2}, 32))}), err: errJWKPublic},
		{name: "EC JWK with a short coordinate", value: jwk(t, ec, map[string]any{"x": b([]byte{1, 2})}), member: "x"},
		{name: "OKP JWK with a short seed", value: jwk(t, okp, map[string]any{"d": b([]byte{1})}), member: "d"},
		{name: "member not base64url", value: jwk(t, okp, map[string]any{"d": "a+b/"}), member: "d"},
		{name: "padded member", value: jwk(t, okp, map[string]any{"d": b(k.ed.Seed()) + "="}), member: "d"},
		{name: "member not a string", value: jwk(t, okp, map[string]any{"crv": 1}), member: "crv"},
		{name: "RSA exponent too large", value: jwk(t, rsaPrivate, map[string]any{"e": b(bytes.Repeat([]byte{0xff}, 8))}), member: "e"},
		{name: "duplicate member", value: []byte(`{"kty":"OKP","kty":"OKP"}`), err: errJWKNotObject},
		{name: "not JSON", value: []byte(`{"kty":`), err: errJWKNotObject},
	}
}

func TestParseKey(t *testing.T) {
	for _, c := range keyCases(t) {
		t.Run(c.name, func(t *testing.T) {
			k, err := parseKey(c.value)
			switch {
			case c.fam != 0:
				if err != nil {
					t.Fatalf("parseKey = %v, want a key", err)
				}
				if k.family != c.fam {
					t.Errorf("family = %v, want %v", k.family, c.fam)
				}
			case c.member != "":
				var me *jwkMemberError
				if !errors.As(err, &me) || me.member != c.member {
					t.Errorf("parseKey = %v, want an error naming member %q", err, c.member)
				}
			default:
				if !errors.Is(err, c.err) {
					t.Errorf("parseKey = %v, want %v", err, c.err)
				}
			}
		})
	}
}

// refusalTexts are the texts a refusal of parseKey may have: its fixed
// reasons, and a JWK member error naming a member parseKey reads with one
// of its fixed problems. No text holds any part of a value.
func refusalTexts() map[string]bool {
	out := map[string]bool{}
	for _, e := range []error{
		errEmptyKey, errNotKey, errPublicKey, errEncryptedKey, errTwoKeys, errBadPEMKey, errKeyType, errRSASize, errRSAKey,
		errJWKNotObject, errJWKSet, errJWKUse, errJWKOps, errJWKPrimes, errJWKOth, errJWKPublic,
	} {
		out[e.Error()] = true
	}
	members := slices.Concat([]string{jwkKty, jwkUse, jwkKeyOps, jwkAlg, jwkCrv, jwkX, jwkY, jwkD, jwkN, jwkE}, rsaCRT())
	problems := []string{
		"is absent", "is not a string", "is not an array of strings", "is not base64url", "does not decode to 32 bytes",
		"is not a public exponent", "is not a P-256 private scalar",
	}
	for _, m := range members {
		for _, p := range problems {
			out[(&jwkMemberError{member: m, problem: p}).Error()] = true
		}
	}
	return out
}

// TestKeyRefusalsNameNoValue: every refusal is one of refusalTexts, so no
// refusal repeats any part of the value it refuses, and a refusal is safe
// to log and to report.
func TestKeyRefusalsNameNoValue(t *testing.T) {
	texts := refusalTexts()
	for _, c := range keyCases(t) {
		if c.fam != 0 {
			continue
		}
		if _, err := parseKey(c.value); err == nil || !texts[err.Error()] {
			t.Errorf("%s: refusal %v is not one of the fixed refusals", c.name, err)
		}
	}
}

// TestKeyFits: a key fits RS256 and PS256 when it is RSA, ES256 when it
// is P-256 and EdDSA when it is Ed25519, and a JWK naming alg only that
// algorithm; the refusal names both, never the key.
func TestKeyFits(t *testing.T) {
	k := keys(t)
	rsaKey, err := parseKey(pkcs8(t, k.rsa))
	if err != nil {
		t.Fatal(err)
	}
	p256, err := parseKey(pkcs8(t, k.p256))
	if err != nil {
		t.Fatal(err)
	}
	ed, err := parseKey(pkcs8(t, k.ed))
	if err != nil {
		t.Fatal(err)
	}
	named, err := parseKey(jwk(t, okpMembers(k.ed), map[string]any{"alg": "EdDSA"}))
	if err != nil {
		t.Fatal(err)
	}
	misnamed, err := parseKey(jwk(t, rsaMembers(k.rsa), map[string]any{"alg": "RS256"}))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		k    *key
		alg  v1alpha1.JWTSignAlgorithm
		ok   bool
	}{
		{"RSA for RS256", rsaKey, v1alpha1.JWTSignAlgorithmRS256, true},
		{"RSA for PS256", rsaKey, v1alpha1.JWTSignAlgorithmPS256, true},
		{"RSA for ES256", rsaKey, v1alpha1.JWTSignAlgorithmES256, false},
		{"RSA for EdDSA", rsaKey, v1alpha1.JWTSignAlgorithmEdDSA, false},
		{"P-256 for ES256", p256, v1alpha1.JWTSignAlgorithmES256, true},
		{"P-256 for RS256", p256, v1alpha1.JWTSignAlgorithmRS256, false},
		{"P-256 for EdDSA", p256, v1alpha1.JWTSignAlgorithmEdDSA, false},
		{"Ed25519 for EdDSA", ed, v1alpha1.JWTSignAlgorithmEdDSA, true},
		{"Ed25519 for ES256", ed, v1alpha1.JWTSignAlgorithmES256, false},
		{"Ed25519 for PS256", ed, v1alpha1.JWTSignAlgorithmPS256, false},
		{"JWK alg EdDSA for EdDSA", named, v1alpha1.JWTSignAlgorithmEdDSA, true},
		{"JWK alg RS256 for PS256", misnamed, v1alpha1.JWTSignAlgorithmPS256, false},
		{"JWK alg RS256 for RS256", misnamed, v1alpha1.JWTSignAlgorithmRS256, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.k.fits(c.alg)
			if (err == nil) != c.ok {
				t.Fatalf("fits(%s) = %v, want ok %v", c.alg, err, c.ok)
			}
			if err != nil && !strings.Contains(err.Error(), string(c.alg)) {
				t.Errorf("refusal %q does not name the algorithm", err)
			}
		})
	}
}

// TestParsedKeysSign: every accepted form yields a key that signs, the
// RSA JWK without primes included, and the key a JWK yields is the key
// its members describe.
func TestParsedKeysSign(t *testing.T) {
	k := keys(t)
	noPrimes := jwk(t, rsaMembers(k.rsa), map[string]any{"p": nil, "q": nil, "dp": nil, "dq": nil, "qi": nil})
	for _, c := range []struct {
		name  string
		value []byte
		alg   v1alpha1.JWTSignAlgorithm
		pub   any
	}{
		{"RSA JWK without primes", noPrimes, v1alpha1.JWTSignAlgorithmRS256, k.rsa.Public()},
		{"RSA JWK", jwk(t, rsaMembers(k.rsa), nil), v1alpha1.JWTSignAlgorithmPS256, k.rsa.Public()},
		{"EC JWK", jwk(t, ecMembers(t, k.p256), nil), v1alpha1.JWTSignAlgorithmES256, k.p256.Public()},
		{"OKP JWK", jwk(t, okpMembers(k.ed), nil), v1alpha1.JWTSignAlgorithmEdDSA, k.ed.Public()},
		{"SEC 1", sec1(t, k.p256), v1alpha1.JWTSignAlgorithmES256, k.p256.Public()},
	} {
		t.Run(c.name, func(t *testing.T) {
			pk, err := parseKey(c.value)
			if err != nil {
				t.Fatal(err)
			}
			s := signer{alg: c.alg}
			if s.head, err = header(c.alg, ""); err != nil {
				t.Fatal(err)
			}
			tok, err := s.appendToken(nil, pk, []byte(`{"exp":1}`))
			if err != nil {
				t.Fatalf("appendToken: %v", err)
			}
			verify(t, string(tok), c.alg, c.pub)
			switch pub := pk.signer.Public().(type) {
			case *rsa.PublicKey:
				if !pub.Equal(c.pub) {
					t.Error("the parsed RSA key is not the JWK's")
				}
			case *ecdsa.PublicKey:
				if !pub.Equal(c.pub) {
					t.Error("the parsed P-256 key is not the JWK's")
				}
			case ed25519.PublicKey:
				if !pub.Equal(c.pub) {
					t.Error("the parsed Ed25519 key is not the JWK's")
				}
			}
		})
	}
}
