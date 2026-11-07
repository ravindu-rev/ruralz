// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwtsign"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The auth.jwt-sign Filter through the request handler, the executor and
// the exchange (docs/architecture/08-security-and-identity.md "JWT
// signing"): the gated Upstream body gets its token with Content-Length
// and the validators following, a signing failure replaces the response
// with 502 RZ-RT-012 before commit, and a generated response passes
// unsigned.

// signingKeyRef is the reference of the signing key.
func signingKeyRef() v1alpha1.SecretRef {
	return v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderEnv, Name: "RURALZ_SECRET_SIGNING_KEY"}
}

// keyStore is a resolved table holding one signing key.
type keyStore struct{ v secret.Value }

func (s keyStore) Get(r secret.Ref) (secret.Value, bool) {
	return s.v, r == secret.RefOf(signingKeyRef())
}

func (keyStore) Watch(secret.Ref, func(secret.Value) error) func() { return func() {} }

// signer builds an ES256 auth.jwt-sign Filter signing the object at token
// and returns it with its public key.
func signer(t *testing.T) (filter.Filter, *ecdsa.PublicKey) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &v1alpha1.AuthJWTSignConfig{
		Paths: []string{"token"}, Algorithm: v1alpha1.JWTSignAlgorithmES256, KeyID: "edge-1",
		SigningKey: v1alpha1.SecretValue{SecretRef: signingKeyRef()},
	}
	f, err := jwtsign.Factory{}.Build(context.Background(), filter.BuildEnv{
		Policy:  &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "sign"}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthJWTSign}},
		Config:  cfg,
		Scope:   phase.ScopeRoute,
		Phases:  phase.Of(phase.OnResponse),
		Secrets: keyStore{v: secret.NewValue(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))},
		Limits:  filter.Limits{MaxResponseBodyBytes: 1 << 20},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = f.(filter.Closer).Close() })
	return f, &k.PublicKey
}

// signRoute returns the Route r running sign in onResponse, its client
// response gated, as the snapshot compiler compiles it, forwarding to an
// Upstream answering body with a strong ETag.
func signRoute(sign filter.Filter, body string, ps ...*snapshot.Policy) handlertest.Route {
	h := http.Header{"Content-Type": {"application/json"}, "Etag": {`"v1"`}, "Content-Length": {strconv.Itoa(len(body))}}
	ps = append(ps, handlertest.Policy("sign", phase.ClassAuth, v1alpha1.PolicyTypeAuthJWTSign, sign, phase.OnResponse))
	return handlertest.Route{
		Name: "r", Forward: handlertest.Respond("orders", http.StatusOK, h, body),
		Policies: ps, Body: snapshot.BodyNeeds{ResponseGate: true},
	}
}

func TestJWTSignThroughTheHandler(t *testing.T) {
	sign, pub := signer(t)
	exp := strconv.FormatInt(epoch().Add(time.Hour).Unix(), 10)
	claims := `{"sub":"alice","exp":` + exp + `}`
	k := newKit(t)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{signRoute(sign, `{"id":7,"token":`+claims+`}`)}})
	res := k.get("/")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", res.Code, res.Body.String())
	}
	var doc struct {
		ID    int    `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &doc); err != nil || doc.ID != 7 {
		t.Fatalf("body %s: %v", res.Body.String(), err)
	}
	parts := strings.Split(doc.Token, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", doc.Token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || string(payload) != claims {
		t.Errorf("payload = %s, %v; want %s", payload, err, claims)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err != nil || len(sig) != 64 || !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Error("the token does not verify under the signing key")
	}
	if got := res.Header().Get("Content-Length"); got != strconv.Itoa(res.Body.Len()) {
		t.Errorf("Content-Length = %q for a %d-byte body", got, res.Body.Len())
	}
	if got := res.Header().Get("Etag"); got != `W/"v1"` {
		t.Errorf("ETag = %q, want the weak form", got)
	}
}

func TestJWTSignFailureReplacesTheResponse(t *testing.T) {
	sign, _ := signer(t)
	k := newKit(t)
	// Claims without exp are refused: the closed Policy replaces the
	// response, so no unsigned claims reach the client.
	k.publish(handlertest.Spec{Routes: []handlertest.Route{signRoute(sign, `{"token":{"sub":"alice"}}`)}})
	res := k.get("/")
	wantProblem(t, res, http.StatusBadGateway, "RZ-RT-012")
	if strings.Contains(res.Body.String(), "alice") {
		t.Errorf("the claims reached the client: %s", res.Body.String())
	}
}

func TestJWTSignSkipsGeneratedResponses(t *testing.T) {
	sign, _ := signer(t)
	deny := filterFunc(func(context.Context, phase.Phase, filter.Exchange) filter.Result {
		return filter.Deny(http.StatusUnauthorized, "RZ-AUTH-001", nil)
	})
	k := newKit(t)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{signRoute(sign, `{"token":{"exp":1}}`,
		handlertest.Policy("jwt", phase.ClassAuth, v1alpha1.PolicyTypeAuthJWT, deny, phase.OnRequestHeaders))}})
	res := k.get("/")
	wantProblem(t, res, http.StatusUnauthorized, "RZ-AUTH-001")
}
