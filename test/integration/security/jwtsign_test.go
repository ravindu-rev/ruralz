// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
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

	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// JWT signing on a running Node (docs/architecture/08-security-and-
// identity.md "JWT signing"): the Node resolves an auth.jwt-sign key
// through a file secretRef and replaces the claims object the Upstream
// returns with a compact JWS that verifies under the key's public key; a
// rotated key signs from the next response; a refused rotation keeps the
// last key, counts in ruralz_config_secret_rotation_failures_total and
// raises secret_rotation_failed until a valid key returns; claims without
// exp are refused with 502 RZ-RT-012, so no unsigned claims reach the
// client.

// refSigningKey is the signing key's reference.
const refSigningKey = "file:/etc/ruralz/jwt/signing.pem"

// signBundle signs the token object of /token responses with ES256.
func signBundle(addr string) []byte {
	return bundle(
		gatewayDoc(webListener),
		upstreamDoc("backend", addr, ""),
		policyDoc("sign", "auth.jwt-sign", "    paths: [token]\n    algorithm: ES256\n    keyId: edge-1\n    signingKey: "+fileRef("/etc/ruralz/jwt/signing.pem")+"\n"),
		routeDoc("sign", "/token", "backend", "sign"),
	)
}

// signingKey returns a new P-256 key and its PKCS #8 PEM carrying a
// canary.
func signingKey(t *testing.T, top *topology.Topology) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return k, top.WithCanary(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// verifiesUnder reports whether the ES256 token tok verifies under pub
// and carries kid edge-1 and the claims want.
func verifiesUnder(tok string, pub *ecdsa.PublicKey, want string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	dec := base64.RawURLEncoding.Strict()
	h, err := dec.DecodeString(parts[0])
	if err != nil || string(h) != `{"alg":"ES256","kid":"edge-1"}` {
		return false
	}
	payload, err := dec.DecodeString(parts[1])
	if err != nil || string(payload) != want {
		return false
	}
	sig, err := dec.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

func TestJWTSigning(t *testing.T) {
	top := newTopology(t, topology.Config{})
	claims := `{"sub":"alice","exp":` + strconv.FormatInt(top.Now().Add(time.Hour).Unix(), 10) + `}`
	mock := startMock(t, top, mockup.Config{LogSize: 256, Behavior: mockup.Behavior{
		Body: []byte(`{"id":7,"token":` + claims + `}`), ContentType: "application/json",
	}})
	if err := mock.SetRoute("/token/no-exp", mockup.Behavior{Body: []byte(`{"token":{"sub":"alice"}}`), ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	first, firstPEM := signingKey(t, top)
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
		Bundle: signBundle(mock.Addr()), Secrets: map[string][]byte{refSigningKey: firstPEM},
	})
	if err != nil {
		t.Fatal(err)
	}
	n, c := nodes[0], top.HTTPClient()
	keyPath, _ := n.SecretPath(refSigningKey)
	token := func() (string, reply) {
		t.Helper()
		r := get(t, c, n.URL("web")+"/token")
		var doc struct{ Token string }
		if r.status != http.StatusOK || json.Unmarshal(r.body, &doc) != nil {
			return "", r
		}
		return doc.Token, r
	}

	t.Run("signs with the resolved key", func(t *testing.T) {
		tok, r := token()
		if !verifiesUnder(tok, &first.PublicKey, claims) {
			t.Fatalf("GET /token = %v; the token does not verify under the signing key", r)
		}
		if got := r.header.Get("Content-Length"); got != strconv.Itoa(len(r.body)) {
			t.Errorf("Content-Length = %q for %d bytes", got, len(r.body))
		}
	})

	second, secondPEM := signingKey(t, top)
	t.Run("a rotated key signs from the next response", func(t *testing.T) {
		replaceFile(t, keyPath, secondPEM)
		advance(t, top, pollInterval)
		waitFor(t, "the rotated signing key", func() (bool, string) {
			tok, r := token()
			return verifiesUnder(tok, &second.PublicKey, claims), r.String()
		})
	})

	t.Run("a refused rotation keeps the last key", func(t *testing.T) {
		failures := freshCounter(t, top, n, "ruralz_config_secret_rotation_failures_total", "provider", "file")
		der, err := x509.MarshalPKIXPublicKey(&second.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		replaceFile(t, keyPath, top.WithCanary(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
		advance(t, top, pollInterval)
		waitRotationFailures(t, top, n, failures+1)
		waitDegraded(t, n, "secret_rotation_failed", true)
		if tok, r := token(); !verifiesUnder(tok, &second.PublicKey, claims) {
			t.Fatalf("GET /token = %v after a refused rotation; want the last key's token", r)
		}
		third, thirdPEM := signingKey(t, top)
		replaceFile(t, keyPath, thirdPEM)
		advance(t, top, pollInterval)
		waitFor(t, "the valid key after the refused one", func() (bool, string) {
			tok, r := token()
			return verifiesUnder(tok, &third.PublicKey, claims), r.String()
		})
		waitDegraded(t, n, "secret_rotation_failed", false)
	})

	t.Run("claims without exp fail the response", func(t *testing.T) {
		r := get(t, c, n.URL("web")+"/token/no-exp")
		wantCode(t, "claims without exp", r, http.StatusBadGateway, "RZ-RT-012")
		if strings.Contains(string(r.body), "alice") {
			t.Errorf("the unsigned claims reached the client: %s", r.body)
		}
	})
	wantRevision(t, n, n.TestDigest())
}
