// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwtsign"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// signingKeyEnv is the env reference of the signing key of the cases.
const signingKeyEnv = "RURALZ_SECRET_SIGNING_KEY"

// signConfig is an auth.jwt-sign config signing the object at token with
// alg under the env signing key.
func signConfig(alg string) string {
	return `{"paths":["token"],"algorithm":"` + alg + `","signingKey":{"secretRef":{"provider":"env","name":"` + signingKeyEnv + `"}}}`
}

// withSigner builds auth.jwt-sign, the one Policy type of the cases, with
// its real Factory.
func withSigner(d *Deps) {
	d.Filters = filter.NewRegistry(map[v1alpha1.PolicyType]filter.Factory{v1alpha1.PolicyTypeAuthJWTSign: jwtsign.Factory{}})
}

// TestJWTSignAttachment: an auth.jwt-sign Policy, built by its real
// Factory over the real resolver, gates its Route's client response, so
// its Filter reads the whole Upstream body, and is no client
// authentication Policy for Security rule 1; its signingKey resolves
// through secretRef.
func TestJWTSignAttachment(t *testing.T) {
	ca, err := pki.NewCA(pki.CAOptions{NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, withSigner)
	h.env.set(signingKeyEnv, string(ca.KeyPEM))
	s := h.mustCompile(h.validate(
		gateway(""), upstreamDoc("orders", "127.0.0.1:9001", ""),
		routeDoc("r", "/", "orders", attach("sign")), policyDoc("sign", "auth.jwt-sign", signConfig("ES256"), ""),
	), nil).Snapshot
	rt := s.Routes[0]
	if want := (snapshot.BodyNeeds{ResponseGate: true}); rt.Body != want {
		t.Errorf("Body = %+v, want %+v", rt.Body, want)
	}
	if rt.Chain.AuthPolicies != 0 {
		t.Errorf("AuthPolicies = %d, want 0: auth.jwt-sign authenticates nobody", rt.Chain.AuthPolicies)
	}
	if len(rt.Chain.Policies) != 1 || rt.Chain.Policies[0].Type != v1alpha1.PolicyTypeAuthJWTSign {
		t.Fatalf("chain = %+v", rt.Chain.Policies)
	}
	if _, ok := rt.Chain.Policies[0].Filter.(*jwtsign.Filter); !ok {
		t.Errorf("Filter = %T, want the auth.jwt-sign Filter", rt.Chain.Policies[0].Filter)
	}
}

// TestJWTSignKeyRefusals: a signingKey a Node resolves to a public key or
// to a key of another family than config.algorithm, or cannot resolve,
// rejects the Revision with an RZ-CFG-026 finding at the Policy, as the
// Node's activation reports it, never repeating the value.
func TestJWTSignKeyRefusals(t *testing.T) {
	ca, err := pki.NewCA(pki.CAOptions{NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(ca.Key.Public())
	if err != nil {
		t.Fatal(err)
	}
	public := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	for _, c := range []struct {
		name, alg, value string
		set              bool
		want             string
	}{
		{"public key", "ES256", string(public), true, "public key"},
		{"P-256 key for RS256", "RS256", string(ca.KeyPEM), true, "signs with an RSA key"},
		{"P-256 key for EdDSA", "EdDSA", string(ca.KeyPEM), true, "signs with an Ed25519 key"},
		{"unresolved", "ES256", "", false, signingKeyEnv},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, withSigner)
			if c.set {
				h.env.set(signingKeyEnv, c.value)
			}
			got := h.rejects(h.validate(
				gateway(""), upstreamDoc("orders", "127.0.0.1:9001", ""),
				routeDoc("r", "/", "orders", attach("sign")), policyDoc("sign", "auth.jwt-sign", signConfig(c.alg), ""),
			))
			if len(got) != 1 || !strings.Contains(got[0], "error RZ-CFG-026") || !strings.Contains(got[0], c.want) {
				t.Fatalf("findings %q, want one RZ-CFG-026 naming %q", got, c.want)
			}
			if c.set && strings.Contains(got[0], strings.Split(strings.TrimSpace(c.value), "\n")[1]) {
				t.Errorf("finding %q repeats the value", got[0])
			}
		})
	}
}
