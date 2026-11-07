// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwtsign"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// signKeyRef is the signing key reference of the interoperation test.
func signKeyRef() v1alpha1.SecretRef {
	return v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderEnv, Name: "RURALZ_SECRET_SIGNING_KEY"}
}

// oneKey is a resolved table holding one signing key.
type oneKey struct{ v secret.Value }

func (s oneKey) Get(r secret.Ref) (secret.Value, bool) { return s.v, r == secret.RefOf(signKeyRef()) }

func (oneKey) Watch(secret.Ref, func(secret.Value) error) func() { return func() {} }

// TestVerifiesJWTSignTokens: a token auth.jwt-sign mints, for each
// algorithm both types offer, is a token auth.jwt parses (its JOSE header
// naming the alg and kid, its claims the signed object) and verifies with
// jwx under the signing key's public key, so a Route can hand clients
// tokens another Route of the Node accepts.
func TestVerifiesJWTSignTokens(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ed := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	c, err := newCodec()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	claims := `{"iss":"https://edge.shop.example","aud":"shop-api","exp":` + strconv.FormatInt(now.Add(time.Hour).Unix(), 10) + `}`
	for _, tc := range []struct {
		alg  v1alpha1.JWTSignAlgorithm
		priv crypto.Signer
	}{
		{v1alpha1.JWTSignAlgorithmRS256, rsaKey},
		{v1alpha1.JWTSignAlgorithmPS256, rsaKey},
		{v1alpha1.JWTSignAlgorithmES256, p256},
		{v1alpha1.JWTSignAlgorithmEdDSA, ed},
	} {
		t.Run(string(tc.alg), func(t *testing.T) {
			der, err := x509.MarshalPKCS8PrivateKey(tc.priv)
			if err != nil {
				t.Fatal(err)
			}
			f, err := jwtsign.Factory{}.Build(t.Context(), filter.BuildEnv{
				Policy: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "sign"}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthJWTSign}},
				Config: &v1alpha1.AuthJWTSignConfig{
					Paths: []string{"token"}, Algorithm: tc.alg, KeyID: "edge-1",
					SigningKey: v1alpha1.SecretValue{SecretRef: signKeyRef()},
				},
				Scope: phase.ScopeRoute, Phases: phase.Of(phase.OnResponse),
				Secrets: oneKey{v: secret.NewValue(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))},
				Limits:  filter.Limits{MaxResponseBodyBytes: 1 << 20},
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			defer func() { _ = f.(filter.Closer).Close() }()
			m := &filtertest.Message{Hdr: http.Header{"Content-Type": {"application/json"}}, Raw: []byte(`{"token":` + claims + `}`), Code: http.StatusOK}
			x := &filtertest.Exchange{MethodV: http.MethodGet, Start: now, Msg: m}
			if res := f.Handle(context.Background(), phase.OnResponse, x); res.Outcome != filter.Continue {
				t.Fatalf("Handle = %+v", res)
			}
			var body struct{ Token string }
			if err := json.Unmarshal(m.Raw, &body); err != nil {
				t.Fatal(err)
			}
			s := c.getScratch()
			defer c.putScratch(s)
			p, err := c.parse(body.Token, s)
			if err != nil {
				t.Fatalf("auth.jwt refuses the token: %v", err)
			}
			if p.alg != string(tc.alg) || p.kid != "edge-1" {
				t.Errorf("alg %q, kid %q; want %s and edge-1", p.alg, p.kid, tc.alg)
			}
			if iss, _ := p.claims.StringClaim("iss"); iss != "https://edge.shop.example" {
				t.Errorf("claims = %v", p.claims)
			}
			if err := c.verifier(p.alg).Verify(tc.priv.Public(), s.input(&p), s.sig); err != nil {
				t.Errorf("jwx does not verify the token: %v", err)
			}
		})
	}
}
