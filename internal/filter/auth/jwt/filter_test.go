// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v4/jws"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwt/jwks"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestAcceptsEachAlgorithm checks that a token signed with RS256, PS256,
// ES256 or EdDSA by the issuer's key of its kid is accepted, and that the
// identity carries method jwt, the Policy and the verified claims.
func TestAcceptsEachAlgorithm(t *testing.T) {
	fx := newFixture(t, options{})
	for _, k := range []*mockidp.Key{fx.keys.rs, fx.keys.ps, fx.keys.es, fx.keys.ed} {
		t.Run(string(k.Alg), func(t *testing.T) {
			x := bearer(mint(t, k, nil, nil))
			id := wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
			if id.Method != filter.MethodJWT || id.Policy != policy || id.Consumer != nil {
				t.Fatalf("identity = %+v", id)
			}
			if sub, _ := id.Claims.(claims).StringClaim("sub"); sub != "user-1" {
				t.Fatalf("claims sub = %q", sub)
			}
		})
	}
}

// TestMissingCredential checks rule RZ-AUTH-001: no Authorization field,
// or one with a scheme other than Bearer, is "credential missing", with
// the Bearer challenge.
func TestMissingCredential(t *testing.T) {
	fx := newFixture(t, options{})
	for _, values := range [][]string{nil, {"Basic dXNlcjpwYXNz"}, {"Token abc"}, {"Bearerabc"}, {""}} {
		x := exchange(values...)
		wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeMissing)
	}
	if fx.decisions.deny[auth.CodeMissing] != 5 {
		t.Fatalf("decisions = %+v, want five RZ-AUTH-001 denies", fx.decisions.deny)
	}
}

// TestSchemeCaseInsensitive checks that the Bearer scheme name is matched
// case-insensitively and that spaces around the token are ignored.
func TestSchemeCaseInsensitive(t *testing.T) {
	fx := newFixture(t, options{})
	tok := mint(t, fx.keys.es, nil, nil)
	for _, v := range []string{"bearer " + tok, "BEARER " + tok, "Bearer   " + tok + " ", " Bearer " + tok} {
		x := exchange(v)
		wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
	}
}

// TestTwoAuthorizationLines checks that more than one Authorization field
// line, in any case, is RZ-AUTH-002 even when each is valid.
func TestTwoAuthorizationLines(t *testing.T) {
	fx := newFixture(t, options{})
	tok := mint(t, fx.keys.es, nil, nil)
	x := exchange("Bearer "+tok, "Bearer "+tok)
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
	x = bearer(tok)
	x.Hdr["authorization"] = []string{"Bearer " + tok}
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
}

// TestRejectedAlgorithms checks "RS256, PS256, ES256 or EdDSA, so HS256 is
// rejected; any other alg ... is RZ-AUTH-002": the header alg decides,
// whatever the key could verify.
func TestRejectedAlgorithms(t *testing.T) {
	fx := newFixture(t, options{})
	for _, alg := range []any{"HS256", "none", "RS512", "ES384", "Ed25519", "rs256", 256} {
		x := bearer(mint(t, fx.keys.rs, map[string]any{"alg": alg}, nil))
		wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
	}
	// No alg at all.
	x := bearer(mint(t, fx.keys.rs, map[string]any{"alg": nil}, nil))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
}

// TestCritHeaderRejected checks that a crit header, whatever it lists, is
// RZ-AUTH-002, as is a b64 other than true (RFC 7797 needs crit).
func TestCritHeaderRejected(t *testing.T) {
	fx := newFixture(t, options{})
	for _, h := range []map[string]any{
		{"crit": []string{"exp"}, "exp": 1},
		{"crit": []string{}},
		{"b64": false},
		{"b64": "true"},
	} {
		x := bearer(mint(t, fx.keys.es, h, nil))
		wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
	}
	x := bearer(mint(t, fx.keys.es, map[string]any{"b64": true}, nil))
	wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
}

// TestKeySourceHeadersIgnored checks "jku, x5u, jwk and x5c are ignored":
// a token naming another key source still verifies with the issuer's
// JWKS, and a token signed by a key it embeds in jwk is not accepted.
func TestKeySourceHeadersIgnored(t *testing.T) {
	fx := newFixture(t, options{})
	elsewhere := map[string]any{
		"jku": "https://attacker.example/jwks", "x5u": "https://attacker.example/cert",
		"x5c": []string{"MIIB"}, "jwk": fx.keys.rs.JWK(),
	}
	x := bearer(mint(t, fx.keys.ed, elsewhere, nil))
	wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)

	attacker, err := mockidp.GenerateKey(mockidp.EdDSA)
	if err != nil {
		t.Fatal(err)
	}
	x = bearer(mint(t, attacker, map[string]any{"jwk": attacker.JWK(), "kid": fx.keys.ed.ID}, nil))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
	x = bearer(mint(t, attacker, map[string]any{"jwk": attacker.JWK()}, nil))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeUnknownKID)
}

// TestMalformedTokens checks that anything but a JWS compact token with a
// JSON object header and payload, strict base64url and JSON, at most
// MaxTokenBytes, is RZ-AUTH-002.
func TestMalformedTokens(t *testing.T) {
	fx := newFixture(t, options{})
	k := fx.keys.es
	good := mint(t, k, nil, nil)
	h, p, s := parts(good)
	signed := func(header, payload string) string {
		tok, err := k.SignCompact([]byte(header), []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	validHeader := `{"alg":"ES256","kid":"` + k.ID + `"}`
	validPayload := `{"iss":"` + issA + `","aud":"` + audA + `","exp":` + itoa(t0.Add(time.Hour).Unix()) + `}`
	tests := []struct {
		name, token string
	}{
		{"empty", ""},
		{"one segment", h},
		{"two segments", h + "." + p},
		{"four segments", good + ".x"},
		{"empty header", "." + p + "." + s},
		{"empty payload", h + ".." + s},
		{"empty signature", h + "." + p + "."},
		{"padding", h + "=." + p + "." + s},
		{"standard alphabet", h + "." + p + "." + strings.NewReplacer("-", "+", "_", "/").Replace(s) + "+/"},
		{"space inside", h + "." + p + " ." + s},
		{"non-canonical base64", h + "." + p + "." + noncanonical(s)},
		{"header not JSON", b64("alg") + "." + p + "." + s},
		{"header not an object", signed(`["ES256"]`, validPayload)},
		{"duplicate header member", signed(`{"alg":"ES256","alg":"ES256","kid":"`+k.ID+`"}`, validPayload)},
		{"kid not a string", signed(`{"alg":"ES256","kid":7}`, validPayload)},
		{"payload not an object", signed(validHeader, `"claims"`)},
		{"payload array", signed(validHeader, `[1]`)},
		{"duplicate claim", signed(validHeader, `{"iss":"`+issA+`","iss":"`+issA+`","aud":"`+audA+`"}`)},
		{"invalid UTF-8 claim", signed(validHeader, "{\"iss\":\""+issA+"\",\"x\":\"\xff\"}")},
		{"trailing data", signed(validHeader, validPayload+"{}")},
		{"over 16 KiB", mint(t, k, nil, map[string]any{"pad": strings.Repeat("a", MaxTokenBytes)})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := exchange("Bearer " + tt.token)
			wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
		})
	}
	// The same header and payload, well formed, are accepted.
	x := bearer(signed(validHeader, validPayload))
	wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
}

// parts splits a compact token.
func parts(tok string) (h, p, s string) {
	f := strings.Split(tok, ".")
	return f[0], f[1], f[2]
}

// b64 encodes s as base64url without padding.
func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// itoa formats n.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// noncanonical changes the unused low bits of the last character of a
// base64url segment whose length leaves padding bits.
func noncanonical(seg string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, seg[len(seg)-1])
	return seg[:len(seg)-1] + string(alphabet[last|1])
}

// TestTokenSizeLimit checks the 16 KiB bound: a token of exactly
// MaxTokenBytes is read, one byte more is RZ-AUTH-002.
func TestTokenSizeLimit(t *testing.T) {
	fx := newFixture(t, options{})
	k := fx.keys.ed
	// A padding claim grows the token by one or two characters per byte;
	// a padding header member shifts it by one, so one of three header
	// paddings lands on the limit exactly.
	var exact, over string
	for hp := 0; hp < 3 && exact == ""; hp++ {
		h := map[string]any{"x": strings.Repeat("h", hp)}
		for n := MaxTokenBytes*3/4 - 400; n < MaxTokenBytes; n++ {
			tok := mint(t, k, h, map[string]any{"pad": strings.Repeat("a", n)})
			if len(tok) == MaxTokenBytes {
				exact = tok
				over = mint(t, k, h, map[string]any{"pad": strings.Repeat("a", n+1)})
			}
			if len(tok) >= MaxTokenBytes {
				break
			}
		}
	}
	if exact == "" {
		t.Fatal("no token of exactly MaxTokenBytes")
	}
	x := bearer(exact)
	wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
	x = bearer(over)
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
}

// TestUnknownIssuer checks that a token whose iss names no configured
// issuer, or carries no string iss, is RZ-AUTH-003 ("wrong issuer").
func TestUnknownIssuer(t *testing.T) {
	fx := newFixture(t, options{})
	for _, iss := range []any{"https://evil.example", "https://idp.example/", nil, 42} {
		x := bearer(mint(t, fx.keys.ed, nil, map[string]any{"iss": iss}))
		wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeClaims)
	}
	if fx.src.nudges.Load() != 0 {
		t.Fatal("an unknown issuer nudged a JWKS fetch")
	}
}

// TestNoUsableKeys checks that an issuer without keys, never fetched or
// past their lifetime, cannot decide: CannotDecide with RZ-AUTH-006, which
// failureMode closed turns into 401 RZ-AUTH-006, and a fetch is nudged.
func TestNoUsableKeys(t *testing.T) {
	fx := newFixture(t, options{issB: true})
	tok := mint(t, fx.keys.ed, nil, map[string]any{"iss": issB})
	x := bearer(tok)
	r := fx.f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if r.Outcome != filter.CannotDecide || r.Code != auth.CodeNoKeys || !errors.Is(r.Err, errNoKeys) || x.Ident != nil {
		t.Fatalf("never fetched: %+v", r)
	}
	if fx.srcB.nudges.Load() != 1 {
		t.Fatal("no fetch nudged for an issuer without keys")
	}
	if strings.Contains(r.Err.Error(), tok) {
		t.Fatal("the cause echoes the token")
	}
	// Expired keys: the last set served until its lifetime ended.
	expired, err := jwks.NewKeys([]byte(`{"keys":[`+string(mustJSON(t, fx.keys.ed.JWK()))+`]}`), issB, nil, t0.Add(-2*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fx.srcB.keys.Store(expired)
	x = bearer(tok)
	r = fx.f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if r.Outcome != filter.CannotDecide || r.Code != auth.CodeNoKeys {
		t.Fatalf("expired keys: %+v", r)
	}
	if fx.decisions.deny[auth.CodeNoKeys] != 2 {
		t.Fatalf("decisions = %+v, want two RZ-AUTH-006", fx.decisions.deny)
	}
}

// TestUnknownKID checks that a kid the issuer's keys lack, or the kid of a
// key of another family, is RZ-AUTH-005 and nudges a fetch.
func TestUnknownKID(t *testing.T) {
	fx := newFixture(t, options{})
	x := bearer(mint(t, fx.keys.ed, map[string]any{"kid": "rotated-away"}, nil))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeUnknownKID)
	if fx.src.nudges.Load() != 1 {
		t.Fatalf("nudges = %d, want 1", fx.src.nudges.Load())
	}
	x = bearer(mint(t, fx.keys.ed, map[string]any{"kid": fx.keys.es.ID}, nil))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeUnknownKID)
	if fx.src.nudges.Load() != 2 {
		t.Fatalf("nudges = %d, want 2", fx.src.nudges.Load())
	}
}

// counter counts the verifications of the wrapped jws.Verifier.
type counter struct {
	v jws.Verifier
	n int
}

func (c *counter) Verify(key any, payload, signature []byte) error {
	c.n++
	return c.v.Verify(key, payload, signature)
}

// countEdDSA replaces fx's EdDSA verifier with a counter.
func countEdDSA(fx *fixture) *counter {
	c := &counter{v: fx.f.codec.eddsa}
	fx.f.codec.eddsa = c
	return c
}

// TestTokenWithoutKID checks that a token without a kid tries every key of
// its family in the issuer's set, in document order, and the first that
// verifies wins; it reaches the Revoker under that key's kid.
func TestTokenWithoutKID(t *testing.T) {
	keys := make([]*mockidp.Key, 3)
	entries := make([]any, len(keys))
	for i := range keys {
		k, err := mockidp.GenerateKey(mockidp.EdDSA)
		if err != nil {
			t.Fatal(err)
		}
		keys[i], entries[i] = k, k.JWK()
	}
	noKID := keys[2].JWK()
	noKID.Kid = ""
	entries[2] = noKID
	fx := newFixture(t, options{keys: keySet(t, append(entries, keysFor(t).rs)...)})
	c := countEdDSA(fx)
	for i, k := range keys {
		c.n = 0
		x := bearer(mint(t, k, map[string]any{"kid": nil}, map[string]any{"jti": "j"}))
		wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
		if c.n != i+1 {
			t.Errorf("key %d verified after %d tries, want %d", i, c.n, i+1)
		}
		want := k.ID
		if i == 2 {
			want = ""
		}
		if fx.revoker.kid != want {
			t.Errorf("key %d: the Revoker saw kid %q, want %q", i, fx.revoker.kid, want)
		}
	}
	if fx.src.nudges.Load() != 0 {
		t.Fatal("a verified token without a kid nudged a fetch")
	}
}

// TestTokenWithoutKIDFailures checks the codes of a token without a kid:
// no key of its family is RZ-AUTH-005 and nudges a fetch; keys of the
// family of which none verifies, or whose JWKs name another alg (never
// tried), are RZ-AUTH-002 without a nudge.
func TestTokenWithoutKIDFailures(t *testing.T) {
	keys := keysFor(t)
	fx := newFixture(t, options{keys: keySet(t, keys.ed)})
	x := bearer(mint(t, keys.es, map[string]any{"kid": nil}, nil))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeUnknownKID)
	if fx.src.nudges.Load() != 1 {
		t.Fatalf("nudges = %d, want 1", fx.src.nudges.Load())
	}
	x = bearer(mint(t, otherKey(t, keys.ed), map[string]any{"kid": nil}, nil))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
	// The set's only RSA key names RS256: a PS256 token never tries it.
	fy := newFixture(t, options{keys: keySet(t, keys.rs)})
	pss, err := mockidp.NewKey(mockidp.PS256, "", keys.rs.Signer())
	if err != nil {
		t.Fatal(err)
	}
	c := &counter{v: fy.f.codec.ps256}
	fy.f.codec.ps256 = c
	x = bearer(mint(t, pss, map[string]any{"kid": nil}, nil))
	wantDeny(t, fy.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
	if c.n != 0 || fx.src.nudges.Load() != 1 || fy.src.nudges.Load() != 0 {
		t.Fatalf("tries %d, nudges %d and %d", c.n, fx.src.nudges.Load(), fy.src.nudges.Load())
	}
}

// TestTokenWithoutKIDCostBound checks the cost bound of a token without a
// kid: at most jwks.MaxKeys verifications, the set's limit, whether the
// last key verifies or none does.
func TestTokenWithoutKIDCostBound(t *testing.T) {
	keys := make([]*mockidp.Key, jwks.MaxKeys)
	entries := make([]any, len(keys))
	for i := range keys {
		k, err := mockidp.GenerateKey(mockidp.EdDSA)
		if err != nil {
			t.Fatal(err)
		}
		keys[i], entries[i] = k, k.JWK()
	}
	fx := newFixture(t, options{keys: keySet(t, entries...)})
	c := countEdDSA(fx)
	x := bearer(mint(t, keys[jwks.MaxKeys-1], map[string]any{"kid": nil}, nil))
	wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
	if c.n != jwks.MaxKeys {
		t.Fatalf("the last key verified after %d tries, want %d", c.n, jwks.MaxKeys)
	}
	c.n = 0
	x = bearer(mint(t, otherKey(t, keys[0]), map[string]any{"kid": nil}, nil))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
	if c.n != jwks.MaxKeys {
		t.Fatalf("a token no key verifies took %d tries, want %d", c.n, jwks.MaxKeys)
	}
}

// TestKeyAlgorithmPinned checks that a JWK naming an alg verifies only
// tokens of that alg: an RS256 key does not verify a PS256 token, which is
// RZ-AUTH-002, while a JWK without alg verifies either.
func TestKeyAlgorithmPinned(t *testing.T) {
	fx := newFixture(t, options{})
	pss, err := mockidp.NewKey(mockidp.PS256, fx.keys.rs.ID, fx.keys.rs.Signer())
	if err != nil {
		t.Fatal(err)
	}
	x := bearer(mint(t, pss, nil, nil))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)

	noAlg := fx.keys.rs.JWK()
	noAlg.Alg = ""
	fy := newFixture(t, options{keys: keySet(t, noAlg)})
	for _, k := range []*mockidp.Key{fx.keys.rs, pss} {
		x := bearer(mint(t, k, nil, nil))
		wantAllow(t, fy.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
	}
}

// TestBadSignature checks that a token whose signature does not verify
// with the key of its kid is RZ-AUTH-002: a changed payload, another
// key's signature under the kid, and a signature of another length.
func TestBadSignature(t *testing.T) {
	fx := newFixture(t, options{})
	for _, k := range []*mockidp.Key{fx.keys.rs, fx.keys.ps, fx.keys.es, fx.keys.ed} {
		t.Run(string(k.Alg), func(t *testing.T) {
			h, _, s := parts(mint(t, k, nil, nil))
			_, p, _ := parts(mint(t, k, nil, map[string]any{"sub": "admin"}))
			x := bearer(h + "." + p + "." + s)
			wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)

			other := otherKey(t, k)
			x = bearer(mint(t, other, nil, nil))
			wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)

			h, p, s = parts(mint(t, k, nil, nil))
			x = bearer(h + "." + p + "." + s[:len(s)-4])
			wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
		})
	}
}

// otherKey returns a new key of k's algorithm under k's kid.
func otherKey(t *testing.T, k *mockidp.Key) *mockidp.Key {
	t.Helper()
	var n *mockidp.Key
	var err error
	switch k.Alg {
	case mockidp.RS256, mockidp.PS256:
		var priv *rsa.PrivateKey
		if priv, err = rsa.GenerateKey(rand.Reader, 2048); err == nil {
			n, err = mockidp.NewKey(k.Alg, k.ID, priv)
		}
	default:
		var g *mockidp.Key
		if g, err = mockidp.GenerateKey(k.Alg); err == nil {
			n, err = mockidp.NewKey(k.Alg, k.ID, g.Signer())
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRegisteredClaims checks the M1 claim rules: exp required, at most
// MaxExpiry ahead, ClockSkew on exp and nbf, aud holding a configured
// audience; iat not checked. Failures are RZ-AUTH-003.
func TestRegisteredClaims(t *testing.T) {
	fx := newFixture(t, options{})
	now := t0.Unix()
	tests := []struct {
		name   string
		claims map[string]any
		ok     bool
	}{
		{"valid", nil, true},
		{"exp missing", map[string]any{"exp": nil}, false},
		{"exp a string", map[string]any{"exp": "1793620800"}, false},
		{"exp null", map[string]any{"exp": jsonNull{}}, false},
		{"exp within skew", map[string]any{"exp": now - 59}, true},
		{"exp at skew", map[string]any{"exp": now - 60}, false},
		{"exp long past", map[string]any{"exp": now - 3600}, false},
		{"exp fractional", map[string]any{"exp": float64(now) + 0.5}, true},
		{"exp 24 h ahead", map[string]any{"exp": now + 24*3600}, true},
		{"exp over 24 h ahead", map[string]any{"exp": now + 24*3600 + 1}, false},
		{"exp huge", map[string]any{"exp": jsonNumber("1e400")}, false},
		{"nbf past", map[string]any{"nbf": now - 10}, true},
		{"nbf within skew", map[string]any{"nbf": now + 60}, true},
		{"nbf ahead", map[string]any{"nbf": now + 61}, false},
		{"nbf a string", map[string]any{"nbf": "0"}, false},
		{"aud missing", map[string]any{"aud": nil}, false},
		{"aud other", map[string]any{"aud": "billing-api"}, false},
		{"aud case differs", map[string]any{"aud": "Shop-API"}, false},
		{"aud second configured", map[string]any{"aud": "admin-api"}, true},
		{"aud array match", map[string]any{"aud": []any{"billing-api", audA}}, true},
		{"aud array no match", map[string]any{"aud": []any{"billing-api"}}, false},
		{"aud array of numbers", map[string]any{"aud": []any{1, 2}}, false},
		{"aud object", map[string]any{"aud": map[string]any{"aud": audA}}, false},
		{"iat in the future", map[string]any{"iat": now + 7200}, true},
		{"iat a string", map[string]any{"iat": "yesterday"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := bearer(mint(t, fx.keys.ed, nil, tt.claims))
			r := fx.f.Handle(t.Context(), phase.OnRequestHeaders, x)
			if tt.ok {
				wantAllow(t, r, x)
			} else {
				wantDeny(t, r, x, auth.CodeClaims)
			}
		})
	}
}

// jsonNull encodes as JSON null.
type jsonNull struct{}

func (jsonNull) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// jsonNumber encodes as its literal text.
type jsonNumber string

func (n jsonNumber) MarshalJSON() ([]byte, error) { return []byte(n), nil }

// TestRevokedToken checks that a verified token the Revoker lists is
// RZ-AUTH-004, after verification, and that the Revoker sees the issuer,
// kid, jti, sub and iat of the token.
func TestRevokedToken(t *testing.T) {
	fx := newFixture(t, options{})
	fx.revoker.jtis["leaked"] = true
	x := bearer(mint(t, fx.keys.es, nil, map[string]any{"jti": "leaked"}))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeRevoked)
	r := fx.revoker
	if r.iss != issA || r.kid != fx.keys.es.ID || r.jti != "leaked" || r.sub != "user-1" || !r.iat.Equal(t0) {
		t.Fatalf("Revoker saw %q %q %q %q %v", r.iss, r.kid, r.jti, r.sub, r.iat)
	}
	// A token failing verification never reaches the Revoker.
	calls := r.calls
	x = bearer(mint(t, otherKey(t, fx.keys.es), nil, map[string]any{"jti": "leaked"}))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
	if r.calls != calls {
		t.Fatal("the Revoker was consulted before verification")
	}
}

// TestConsumerBinding checks "JWTs by issuer plus subject or claims; OAuth
// clients by client_id, else azp, when iss is in that Consumer's
// credentials.jwt": the bound Consumer is the identity's, its name the
// principal; no match leaves the identity unbound.
func TestConsumerBinding(t *testing.T) {
	bySub := consumer("by-sub", v1alpha1.Credentials{JWT: []v1alpha1.JWTBinding{{Issuer: issA, Subject: "svc-1"}}})
	byClaims := consumer("by-claims", v1alpha1.Credentials{JWT: []v1alpha1.JWTBinding{{Issuer: issA, Claims: map[string]string{"tenant": "acme", "role": "batch"}}}})
	byClient := consumer("by-client", v1alpha1.Credentials{
		JWT:          []v1alpha1.JWTBinding{{Issuer: issA, Subject: "unused"}},
		OAuthClients: []v1alpha1.OAuthClient{{ClientID: "portal"}},
	})
	otherIss := consumer("other-iss", v1alpha1.Credentials{JWT: []v1alpha1.JWTBinding{{Issuer: issB, Subject: "svc-2"}}})
	fx := newFixture(t, options{consumers: []*v1alpha1.Consumer{bySub, byClaims, byClient, otherIss}})
	tests := []struct {
		name   string
		claims map[string]any
		want   string
	}{
		{"subject", map[string]any{"sub": "svc-1"}, "by-sub"},
		{"claims", map[string]any{"tenant": "acme", "role": "batch"}, "by-claims"},
		{"claims partly", map[string]any{"tenant": "acme"}, ""},
		{"client_id", map[string]any{"client_id": "portal"}, "by-client"},
		{"azp", map[string]any{"azp": "portal"}, "by-client"},
		{"subject of another issuer", map[string]any{"sub": "svc-2"}, ""},
		{"no match", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := bearer(mint(t, fx.keys.es, nil, tt.claims))
			id := wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
			switch {
			case tt.want == "" && id.Consumer != nil:
				t.Fatalf("bound %q, want unbound", id.Consumer.Name)
			case tt.want != "" && (id.Consumer == nil || id.Consumer.Name != tt.want || id.Principal != tt.want):
				t.Fatalf("identity = %+v, want Consumer %q", id, tt.want)
			}
		})
	}
}

// TestUnboundPrincipal checks the principal of an unbound token:
// "jwt:<iss>#<sub>" for a string sub, none without one.
func TestUnboundPrincipal(t *testing.T) {
	fx := newFixture(t, options{})
	x := bearer(mint(t, fx.keys.es, nil, nil))
	if id := wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x); id.Principal != "jwt:"+issA+"#user-1" {
		t.Fatalf("Principal = %q", id.Principal)
	}
	for _, sub := range []any{nil, 7} {
		x := bearer(mint(t, fx.keys.es, nil, map[string]any{"sub": sub}))
		if id := wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x); id.Principal != "" {
			t.Fatalf("Principal without a string sub = %q", id.Principal)
		}
	}
}

// TestAmbiguousBinding checks that a token matching two Consumers is
// RZ-AUTH-002.
func TestAmbiguousBinding(t *testing.T) {
	a := consumer("a", v1alpha1.Credentials{JWT: []v1alpha1.JWTBinding{{Issuer: issA, Subject: "user-1"}}})
	b := consumer("b", v1alpha1.Credentials{JWT: []v1alpha1.JWTBinding{{Issuer: issA, Claims: map[string]string{"tenant": "acme"}}}})
	fx := newFixture(t, options{consumers: []*v1alpha1.Consumer{a, b}})
	x := bearer(mint(t, fx.keys.es, nil, map[string]any{"tenant": "acme"}))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeInvalid)
}

// TestSecondAuthentication checks Security rule 2: when an earlier
// auth-class Policy authenticated the request, a valid token is 401
// RZ-AUTH-002.
func TestSecondAuthentication(t *testing.T) {
	fx := newFixture(t, options{})
	x := bearer(mint(t, fx.keys.es, nil, nil))
	x.Ident = &filter.Identity{Method: filter.MethodAPIKey, Policy: "keys"}
	r := fx.f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if r.Outcome != filter.Respond || r.Response.Code != auth.CodeInvalid || x.Ident.Method != filter.MethodAPIKey {
		t.Fatalf("second authentication = %+v", r)
	}
}

// TestForwardsCredential checks "auth.jwt forwards it": the Authorization
// field is left on the request after a successful authentication.
func TestForwardsCredential(t *testing.T) {
	fx := newFixture(t, options{})
	tok := mint(t, fx.keys.es, nil, nil)
	x := bearer(tok)
	wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
	if got := x.Hdr.Get("Authorization"); got != "Bearer "+tok {
		t.Fatalf("Authorization after authentication = %q", got)
	}
}

// TestOtherPhasesContinue checks that the Filter acts only in
// onRequestHeaders.
func TestOtherPhasesContinue(t *testing.T) {
	fx := newFixture(t, options{})
	for _, p := range []phase.Phase{phase.OnRequestBody, phase.OnRoute, phase.OnResponse, phase.OnLog} {
		x := exchange()
		if r := fx.f.Handle(t.Context(), p, x); r.Outcome != filter.Continue || x.Ident != nil {
			t.Fatalf("Handle(%v) = %+v", p, r)
		}
	}
}

// TestDecisionsCounted checks that ruralz_auth_decisions_total counts an
// allow per authenticated request and a deny per rejection by code.
func TestDecisionsCounted(t *testing.T) {
	fx := newFixture(t, options{})
	x := bearer(mint(t, fx.keys.es, nil, nil))
	fx.f.Handle(t.Context(), phase.OnRequestHeaders, x)
	x = bearer(mint(t, fx.keys.es, nil, map[string]any{"aud": "nope"}))
	fx.f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if fx.decisions.allow != 1 || fx.decisions.deny[auth.CodeClaims] != 1 {
		t.Fatalf("decisions = %d allow, %v deny", fx.decisions.allow, fx.decisions.deny)
	}
}

// TestJWKSAgeGauge checks that ruralz_auth_jwks_age_seconds reports the
// age of the Policy's oldest key set at most once per second, leaving
// out issuers never fetched.
func TestJWKSAgeGauge(t *testing.T) {
	fx := newFixture(t, options{issB: true})
	tok := mint(t, fx.keys.es, nil, nil)
	x := bearer(tok)
	wantAllow(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
	if fx.age.v.Load() != 60 || fx.age.sets.Load() != 1 {
		t.Fatalf("age = %d after %d sets, want 60 after 1", fx.age.v.Load(), fx.age.sets.Load())
	}
	older, err := jwks.NewKeys(mustJSON(t, map[string]any{"keys": []any{fx.keys.ed.JWK()}}), issB, nil, t0.Add(-10*time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fx.srcB.keys.Store(older)
	x = bearer(tok)
	fx.f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if fx.age.sets.Load() != 1 {
		t.Fatal("the gauge was set twice within one second")
	}
	x = bearer(tok)
	x.Start = t0.Add(time.Second)
	fx.f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if fx.age.v.Load() != 601 {
		t.Fatalf("age = %d, want the oldest set's 601", fx.age.v.Load())
	}
}

// TestCloseReleases checks that Close releases every key set once and is
// idempotent.
func TestCloseReleases(t *testing.T) {
	fx := newFixture(t, options{issB: true})
	if err := fx.f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fx.f.Close(); err != nil {
		t.Fatal(err)
	}
	if fx.src.releases.Load() != 1 || fx.srcB.releases.Load() != 1 {
		t.Fatalf("releases = %d, %d, want 1, 1", fx.src.releases.Load(), fx.srcB.releases.Load())
	}
	if fx.f.CredentialIndex() == nil || fx.f.Challenge() != auth.ChallengeJWT {
		t.Fatal("CredentialIndex or Challenge missing")
	}
}

// TestP384KeyNotUsed checks that a token verifiable only by a P-384 key
// is not accepted even when the JWKS names that kid: the key is never
// loaded.
func TestP384KeyNotUsed(t *testing.T) {
	keys := keysFor(t)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := mockidp.NewKey(mockidp.ES256, "p384", p384)
	if err != nil {
		t.Fatal(err)
	}
	fx := newFixture(t, options{keys: keySet(t, keys.ed, k.JWK())})
	x := bearer(mint(t, k, nil, nil))
	wantDeny(t, fx.f.Handle(t.Context(), phase.OnRequestHeaders, x), x, auth.CodeUnknownKID)
}
