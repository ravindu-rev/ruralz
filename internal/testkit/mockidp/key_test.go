// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockidp

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"sync"
	"testing"
)

// Tests for WP-84 mockidp keys, minting and verification. "Done when":
// mockidp tokens verify with stdlib crypto (TestTokensVerifyWithStdlib).

// Keys are generated once per test binary (RSA generation is slow under
// -race); package-level state is allowed in tests.
var (
	rsKey = sync.OnceValues(func() (*Key, error) { return GenerateKey(RS256) })
	psKey = sync.OnceValues(func() (*Key, error) { return GenerateKey(PS256) })
	esKey = sync.OnceValues(func() (*Key, error) { return GenerateKey(ES256) })
	edKey = sync.OnceValues(func() (*Key, error) { return GenerateKey(EdDSA) })
	// rs2Key is a second RS256 key (a wrong key, a rotation target).
	rs2Key = sync.OnceValues(func() (*Key, error) { return GenerateKey(RS256) })
)

func key(t testing.TB, alg Alg) *Key {
	t.Helper()
	get := map[Alg]func() (*Key, error){RS256: rsKey, PS256: psKey, ES256: esKey, EdDSA: edKey}[alg]
	k, err := get()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func otherRSAKey(t testing.TB) *Key {
	t.Helper()
	k, err := rs2Key()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustB64(t testing.TB, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// stdlibVerify checks a token with direct standard library calls and no
// package code: the WP-84 "tokens verify with stdlib crypto" oracle.
func stdlibVerify(t *testing.T, alg Alg, pub crypto.PublicKey, token string) bool {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	input := []byte(parts[0] + "." + parts[1])
	sig := mustB64(t, parts[2])
	digest := sha256.Sum256(input)
	switch alg {
	case RS256:
		return rsa.VerifyPKCS1v15(pub.(*rsa.PublicKey), crypto.SHA256, digest[:], sig) == nil
	case PS256:
		return rsa.VerifyPSS(pub.(*rsa.PublicKey), crypto.SHA256, digest[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
	case ES256:
		if len(sig) != 64 {
			t.Fatalf("ES256 signature of %d bytes, want 64 (RFC 7518 r||s)", len(sig))
		}
		return ecdsa.Verify(pub.(*ecdsa.PublicKey), digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
	case EdDSA:
		return ed25519.Verify(pub.(ed25519.PublicKey), input, sig)
	}
	t.Fatalf("unknown alg %s", alg)
	return false
}

func TestTokensVerifyWithStdlib(t *testing.T) { // WP-84 Done when; 06 req 26 algorithms
	for _, alg := range []Alg{RS256, PS256, ES256, EdDSA} {
		t.Run(string(alg), func(t *testing.T) {
			k := key(t, alg)
			tok, err := k.Sign(nil, map[string]any{"iss": "https://idp.test", "sub": "alice", "exp": 2000000000})
			if err != nil {
				t.Fatal(err)
			}
			if !stdlibVerify(t, alg, k.Public(), tok) {
				t.Fatal("standard library verification failed")
			}
			var h map[string]any
			if err := json.Unmarshal(mustB64(t, strings.Split(tok, ".")[0]), &h); err != nil {
				t.Fatal(err)
			}
			if h["alg"] != string(alg) || h["kid"] != k.ID || h["typ"] != "JWT" || len(h) != 3 {
				t.Fatalf("header = %v", h)
			}
			// The published JWK carries the same key: parse and verify.
			doc, err := json.Marshal(map[string]any{"keys": []JWK{k.JWK()}})
			if err != nil {
				t.Fatal(err)
			}
			keys, err := ParseJWKS(doc)
			if err != nil || len(keys) != 1 {
				t.Fatalf("ParseJWKS = %v, %v", keys, err)
			}
			if !stdlibVerify(t, alg, keys[0].Key, tok) {
				t.Fatal("verification with the JWK-decoded key failed")
			}
			hdr, claims, err := Verify(tok, keys)
			if err != nil {
				t.Fatal(err)
			}
			if hdr["kid"] != k.ID || claims["sub"] != "alice" || claims["exp"] != json.Number("2000000000") {
				t.Fatalf("header %v claims %v", hdr, claims)
			}
			// A flipped payload byte fails every check.
			parts := strings.Split(tok, ".")
			bad := parts[0] + "." + b64([]byte(`{"sub":"mallory"}`)) + "." + parts[2]
			if stdlibVerify(t, alg, k.Public(), bad) {
				t.Fatal("tampered token verified")
			}
			if _, _, err := Verify(bad, keys); err == nil {
				t.Fatal("Verify accepted a tampered token")
			}
		})
	}
}

func TestJWKMembers(t *testing.T) {
	for _, tc := range []struct {
		alg      Alg
		kty, crv string
		xLen     int
	}{
		{RS256, "RSA", "", 0},
		{PS256, "RSA", "", 0},
		{ES256, "EC", "P-256", 32},
		{EdDSA, "OKP", "Ed25519", 32},
	} {
		k := key(t, tc.alg)
		j := k.JWK()
		if j.Kty != tc.kty || j.Crv != tc.crv || j.Kid != k.ID || j.Use != "sig" || j.Alg != string(tc.alg) {
			t.Errorf("%s: JWK = %+v", tc.alg, j)
		}
		if tc.kty == "RSA" && (len(mustB64(t, j.N)) != RSABits/8 || j.E != "AQAB") {
			t.Errorf("%s: n of %d bytes, e %q", tc.alg, len(mustB64(t, j.N)), j.E)
		}
		if tc.xLen > 0 && len(mustB64(t, j.X)) != tc.xLen {
			t.Errorf("%s: x of %d bytes", tc.alg, len(mustB64(t, j.X)))
		}
		if tc.kty == "EC" && len(mustB64(t, j.Y)) != 32 {
			t.Errorf("%s: y of %d bytes", tc.alg, len(mustB64(t, j.Y)))
		}
		if k.ID != k.Thumbprint() || len(k.ID) != 43 {
			t.Errorf("%s: kid %q is not the thumbprint %q", tc.alg, k.ID, k.Thumbprint())
		}
		if k.Signer() == nil {
			t.Errorf("%s: no signer", tc.alg)
		}
	}
	if j := publicJWK("not a key"); j.Kty != "" || j.N != "" || j.X != "" {
		t.Error("publicJWK of an unknown type is not empty")
	}
}

func TestThumbprintRFC7638(t *testing.T) { // RFC 7638 section 3.1 example
	j := JWK{
		Kty: "RSA",
		N:   "0vx7agoebGcQSuuPiLJXZptN9nndrQmbXEps2aiAFbWhM78LhWx4cbbfAAtVT86zwu1RK7aPFFxuhDR1L6tSoc_BJECPebWKRXjBZCiFV4n3oknjhMstn64tZ_2W-5JsGY4Hc5n9yBXArwl93lqt7_RN5w6Cf0h4QyQ5v-65YGjQR0_FDW2QvzqY368QQMicAtaSqzs8KJZgnYb9c7d0zgdAZHzu6qMQvRL5hajrn1n91CbOpbISD08qNLyrdkt-bFTWhAI4vMQFh6WeZu0fM4lFd2NcRwr3XPksINHaQ-G_xBniIqbw0Ls1jF44-csFCur-kEgU8awapJzKnqDKgw",
		E:   "AQAB",
	}
	if got, want := thumbprint(j), "NzbLsXh8uDCcd-6MNwXF4W_7noWXFZAfHkxZsRGC9Xs"; got != want {
		t.Fatalf("thumbprint = %s, want %s", got, want)
	}
}

func TestSignOverrides(t *testing.T) {
	k := key(t, ES256)
	keys := []PublicKey{{ID: k.ID, Alg: ES256, Key: k.Public()}}
	// No kid: Verify tries every key.
	tok, err := k.Sign(map[string]any{"kid": nil, "crit": []string{"exp"}, "jku": "https://attacker.test/jwks"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h, claims, err := Verify(tok, keys)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := h["kid"]; has || h["jku"] != "https://attacker.test/jwks" || h["crit"] == nil || len(claims) != 0 {
		t.Fatalf("header %v claims %v", h, claims)
	}
	// A header alg that differs from the signing key's.
	tok, err = k.Sign(map[string]any{"alg": "HS256"}, map[string]any{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(tok, keys); err == nil || !strings.Contains(err.Error(), `"HS256" not accepted`) {
		t.Fatalf("Verify(HS256 header) = %v", err)
	}
	// SignCompact signs arbitrary bytes: duplicate members, non-objects.
	tok, err = k.SignCompact([]byte(`{"alg":"ES256","alg":"ES256"}`), []byte(`{"sub":"a","sub":"b"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(tok, keys); err != nil {
		t.Fatalf("duplicate members (encoding/json keeps the last): %v", err)
	}
	tok, err = k.SignCompact([]byte(`{"alg":"ES256"}`), []byte(`[1,2]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(tok, keys); err == nil || !strings.Contains(err.Error(), "claims") {
		t.Fatalf("Verify(array payload) = %v", err)
	}
	if _, err := k.Sign(map[string]any{"x": make(chan int)}, nil); err == nil {
		t.Fatal("Sign encoded a channel header")
	}
	if _, err := k.Sign(nil, map[string]any{"x": make(chan int)}); err == nil {
		t.Fatal("Sign encoded channel claims")
	}
}

func TestVerifyRejects(t *testing.T) {
	rs, es := key(t, RS256), key(t, ES256)
	good, err := rs.Sign(nil, map[string]any{"sub": "a"})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, ".")
	keys := []PublicKey{{ID: rs.ID, Key: rs.Public()}, {ID: es.ID, Alg: ES256, Key: es.Public()}}
	wrongKey := []PublicKey{{ID: rs.ID, Key: otherRSAKey(t).Public()}}
	noneTok := b64([]byte(`{"alg":"none"}`)) + "." + parts[1] + "."
	for _, tc := range []struct {
		name  string
		token string
		keys  []PublicKey
		want  string
	}{
		{"two segments", parts[0] + "." + parts[1], keys, "not a JWS compact"},
		{"five segments", good + ".a.b", keys, "not a JWS compact"},
		{"padded header", parts[0] + "=." + parts[1] + "." + parts[2], keys, "header encoding"},
		{"header not an object", b64([]byte(`"x"`)) + "." + parts[1] + "." + parts[2], keys, "header"},
		{"header trailing data", b64([]byte(`{"alg":"RS256"} {}`)) + "." + parts[1] + "." + parts[2], keys, "trailing data"},
		{"alg none", noneTok, keys, `"none" not accepted`},
		{"bad signature encoding", parts[0] + "." + parts[1] + ".!!", keys, "signature encoding"},
		{"wrong key", good, wrongKey, "no key verifies"},
		{"unknown kid", good, []PublicKey{{ID: "other", Key: rs.Public()}}, "no key verifies"},
		{"key alg mismatch", good, []PublicKey{{ID: rs.ID, Alg: PS256, Key: rs.Public()}}, "no key verifies"},
		{"type mismatch", good, []PublicKey{{ID: rs.ID, Key: es.Public()}}, "no key verifies"},
		{"no keys", good, nil, "no key verifies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Verify(tc.token, tc.keys)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Verify = %v, want %q", err, tc.want)
			}
		})
	}
	// Signature-type mismatches for each algorithm.
	for _, alg := range []Alg{RS256, PS256, ES256, EdDSA} {
		if verifySignature(alg, "not a key", []byte("x"), []byte("y")) {
			t.Errorf("%s verified with a non-key", alg)
		}
	}
	if verifySignature(ES256, es.Public(), []byte("x"), make([]byte, 63)) {
		t.Error("ES256 accepted a 63-byte signature")
	}
	if verifySignature("HS256", rs.Public(), []byte("x"), nil) {
		t.Error("HS256 verified")
	}
	// A bad payload encoding behind a valid signature.
	bad, err := rs.SignCompact([]byte(`{"alg":"RS256"}`), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	bp := strings.Split(bad, ".")
	sig, err := rs.signInput([]byte(bp[0] + ".***"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(bp[0]+".***."+b64(sig), keys); err == nil || !strings.Contains(err.Error(), "payload encoding") {
		t.Fatalf("Verify(bad payload encoding) = %v", err)
	}
}

func TestNewKey(t *testing.T) {
	rsa1024, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // G403: a deliberately weak key for the 06 req 38 negative fixture
	if err != nil {
		t.Fatal(err)
	}
	k, err := NewKey(RS256, "small", rsa1024)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := k.Sign(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !stdlibVerify(t, RS256, k.Public(), tok) || k.ID != "small" {
		t.Fatal("RSA-1024 fixture does not verify")
	}
	// P-384 under ES256 (06 req 38: unusable at the gateway): the
	// signature halves follow the curve (48 bytes each).
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k384, err := NewKey(ES256, "", p384)
	if err != nil {
		t.Fatal(err)
	}
	tok, err = k384.Sign(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(mustB64(t, strings.Split(tok, ".")[2])); n != 96 {
		t.Fatalf("P-384 signature of %d bytes", n)
	}
	if k384.JWK().Crv != "P-384" {
		t.Fatalf("crv %q", k384.JWK().Crv)
	}
	doc, _ := json.Marshal(map[string]any{"keys": []JWK{k384.JWK()}})
	keys, err := ParseJWKS(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Verify(tok, keys); err != nil {
		t.Fatalf("P-384 token: %v", err)
	}
	for _, tc := range []struct {
		alg    Alg
		signer crypto.Signer
		want   string
	}{
		{"HS256", rsa1024, "unsupported algorithm"},
		{RS256, p384, "cannot sign RS256"},
		{ES256, rsa1024, "cannot sign ES256"},
		{EdDSA, p384, "cannot sign EdDSA"},
	} {
		if _, err := NewKey(tc.alg, "", tc.signer); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("NewKey(%s, %T) = %v, want %q", tc.alg, tc.signer, err, tc.want)
		}
	}
	if _, err := GenerateKey("RS384"); err == nil {
		t.Error("GenerateKey(RS384) succeeded")
	}
	bad := &Key{Alg: "HS256", signer: rsa1024}
	if _, err := bad.Sign(nil, nil); err == nil {
		t.Error("Sign with an unsupported alg succeeded")
	}
	mismatch := &Key{Alg: ES256, signer: rsa1024}
	if _, err := mismatch.Sign(nil, nil); err == nil {
		t.Error("ES256 Sign with an RSA key succeeded")
	}
}

func TestParseJWKS(t *testing.T) {
	rs, es, ed := key(t, RS256), key(t, ES256), key(t, EdDSA)
	priv := es.JWK()
	doc, err := json.Marshal(map[string]any{"keys": []any{
		rs.JWK(),
		map[string]any{"kty": "oct", "k": "c2VjcmV0"},
		map[string]any{"kty": "OKP", "crv": "X25519", "x": priv.X},
		map[string]any{"kty": "EC", "crv": "secp256k1", "x": priv.X, "y": priv.Y},
		map[string]any{"kty": "EC", "crv": "P-256", "x": priv.X, "y": priv.Y, "d": "private-ignored", "use": "sig"},
		ed.JWK(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ParseJWKS(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 || keys[0].ID != rs.ID || keys[0].Alg != RS256 || keys[1].ID != "" || keys[2].Alg != EdDSA {
		t.Fatalf("keys = %+v", keys)
	}
	if !keys[1].Key.(*ecdsa.PublicKey).Equal(es.Public()) {
		t.Fatal("EC key differs")
	}
	for _, tc := range []struct {
		name, doc, want string
	}{
		{"json", `{"keys":`, "parse JWKS"},
		{"rsa n", `{"keys":[{"kty":"RSA","n":"***","e":"AQAB"}]}`, "bad n"},
		{"rsa empty n", `{"keys":[{"kty":"RSA","n":"","e":"AQAB"}]}`, "bad n"},
		{"rsa e", `{"keys":[{"kty":"RSA","n":"AQAB","e":"AQABAQAB"}]}`, "bad e"},
		{"ec size", `{"keys":[{"kty":"EC","crv":"P-256","x":"AQAB","y":"AQAB"}]}`, "bad EC point encoding"},
		{"ec point", `{"keys":[{"kty":"EC","crv":"P-256","x":"` + b64(make([]byte, 32)) + `","y":"` + b64(make([]byte, 32)) + `"}]}`, "bad EC point"},
		{"ed25519 x", `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"AQAB"}]}`, "bad Ed25519 x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseJWKS([]byte(tc.doc)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseJWKS = %v, want %q", err, tc.want)
			}
		})
	}
	for _, c := range []string{"P-384", "P-521"} {
		k, err := ecdsa.GenerateKey(map[string]elliptic.Curve{"P-384": elliptic.P384(), "P-521": elliptic.P521()}[c], rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		j := publicJWK(&k.PublicKey)
		doc, _ := json.Marshal(map[string]any{"keys": []JWK{j}})
		keys, err := ParseJWKS(doc)
		if err != nil || len(keys) != 1 || !keys[0].Key.(*ecdsa.PublicKey).Equal(&k.PublicKey) {
			t.Fatalf("%s: %v %v", c, keys, err)
		}
	}
}

func FuzzVerify(f *testing.F) {
	k, err := esKey()
	if err != nil {
		f.Fatal(err)
	}
	tok, err := k.Sign(nil, map[string]any{"sub": "a"})
	if err != nil {
		f.Fatal(err)
	}
	keys := []PublicKey{{ID: k.ID, Alg: ES256, Key: k.Public()}}
	for _, seed := range []string{tok, "", "..", "a.b.c", tok + "x", strings.Replace(tok, ".", "..", 1)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, token string) {
		h, claims, err := Verify(token, keys)
		if err == nil && (h == nil || claims == nil) {
			t.Fatal("success without header and claims")
		}
	})
}

func BenchmarkSign(b *testing.B) {
	for _, alg := range []Alg{RS256, PS256, ES256, EdDSA} {
		b.Run(string(alg), func(b *testing.B) {
			k := key(b, alg)
			claims := map[string]any{"iss": "https://idp.test", "sub": "alice", "exp": 2000000000, "aud": "orders"}
			b.ReportAllocs()
			for b.Loop() {
				if _, err := k.Sign(nil, claims); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkVerifyRS256(b *testing.B) {
	k := key(b, RS256)
	tok, err := k.Sign(nil, map[string]any{"sub": "alice"})
	if err != nil {
		b.Fatal(err)
	}
	keys := []PublicKey{{ID: k.ID, Key: k.Public()}}
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := Verify(tok, keys); err != nil {
			b.Fatal(err)
		}
	}
}
