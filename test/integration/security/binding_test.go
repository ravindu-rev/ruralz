// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Secret-to-destination binding (docs/architecture/08-security-and-identity.md
// "Secrets" rule 8, docs/architecture/02-configuration-model.md
// "Secret-to-destination binding"): each secretRef use has one static
// destination, an auth.upstream-oauth2 clientSecret the origin of its
// tokenUrl and every other secret the Node itself, and one secret used for
// two destinations is RZ-CFG-041, so a Node never sends a Consumer API key
// to a token endpoint. Uses name one secret when the Node reads them from
// one source: a file by its path and key, a reference without key holding
// every key of its file.

// refOAuthSecret is the client secret of the binding cases.
const refOAuthSecret = "file:/etc/ruralz/oauth/client-secret" //nolint:gosec // G101: a secret reference, not a credential

// oauthPolicy is the auth.upstream-oauth2 Policy name against tokenURL
// with the client secret secretRef (a flow mapping).
func oauthPolicy(name, tokenURL, secretRef string) string {
	return policyDoc(name, "auth.upstream-oauth2", fmt.Sprintf("    tokenUrl: %q\n    clientId: gateway-edge\n    clientSecret: {secretRef: %s}\n", tokenURL, secretRef))
}

// apiKeyConsumer is the Consumer app whose one API key keyRef holds.
func apiKeyConsumer(keyRef string) string {
	return "apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: app\nspec:\n  credentials:\n    apiKeys:\n      - name: main\n        secretRef: " + keyRef + "\n"
}

func TestSecretToDestinationBinding(t *testing.T) {
	top := newTopology(t, topology.Config{})
	issuer, err := top.StartIDP(mockidp.Config{Now: top.Now})
	if err != nil {
		t.Fatal(err)
	}
	other, err := top.StartIDP(mockidp.Config{Now: top.Now})
	if err != nil {
		t.Fatal(err)
	}
	mock := startMock(t, top, mockup.Config{})
	secret := top.Canary()
	secretRef := rawFileRef("/etc/ruralz/oauth/client-secret")
	// with returns the Bundle of the Route /orders to the Upstream backend
	// whose leg carries the Policy oauth, plus the extra resources.
	with := func(oauth string, extra ...string) []byte {
		return bundle(append([]string{
			gatewayDoc(webListener),
			upstreamDoc("backend", mock.Addr(), "  policies:\n    - name: oauth\n"),
			oauth,
			routeDoc("orders", "/orders", "backend"),
		}, extra...)...)
	}
	base := with(oauthPolicy("oauth", issuer.TokenURL(), secretRef))
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: base, Secrets: map[string][]byte{refOAuthSecret: []byte(secret)}})
	if err != nil {
		t.Fatal(err)
	}
	n := nodes[0]
	c := top.HTTPClient()
	url := n.URL("web") + "/orders"
	wantOK(t, "request with the token of the bound secret", get(t, c, url))
	if reqs := issuer.TokenRequests(); len(reqs) != 1 || reqs[0].ClientSecret != secret {
		t.Fatalf("token requests = %+v, want one carrying the client secret to its tokenUrl", reqs)
	}

	t.Run("an API key holding the client secret", func(t *testing.T) {
		wantRejected(t, top, n, with(oauthPolicy("oauth", issuer.TokenURL(), secretRef), apiKeyConsumer(secretRef)), "RZ-CFG-041")
	})
	t.Run("one secret for two token endpoints", func(t *testing.T) {
		wantRejected(t, top, n, bundle(
			gatewayDoc(webListener),
			upstreamDoc("backend", mock.Addr(), "  policies:\n    - name: oauth\n"),
			upstreamDoc("second", mock.Addr(), "  policies:\n    - name: oauth-elsewhere\n"),
			oauthPolicy("oauth", issuer.TokenURL(), secretRef),
			oauthPolicy("oauth-elsewhere", other.TokenURL(), secretRef),
			routeDoc("orders", "/orders", "backend"),
			routeDoc("second", "/second", "second"),
		), "RZ-CFG-041")
	})
	t.Run("a whole file and one of its keys", func(t *testing.T) {
		// A reference without key reads the whole file, so it holds the
		// key the client secret names.
		keyed := rawFileRef("/etc/ruralz/shared.json")
		keyed = keyed[:len(keyed)-1] + ", key: oauth}"
		wantRejected(t, top, n, with(oauthPolicy("oauth", issuer.TokenURL(), keyed), apiKeyConsumer(rawFileRef("/etc/ruralz/shared.json"))), "RZ-CFG-041")
	})
	t.Run("one secret twice for one destination", func(t *testing.T) {
		// Two uses with the same tokenUrl origin share the destination.
		wantActivated(t, n, bundle(
			gatewayDoc(webListener),
			upstreamDoc("backend", mock.Addr(), "  policies:\n    - name: oauth\n"),
			upstreamDoc("second", mock.Addr(), "  policies:\n    - name: oauth-again\n"),
			oauthPolicy("oauth", issuer.TokenURL(), secretRef),
			oauthPolicy("oauth-again", issuer.TokenURL()+"?audience=second", secretRef),
			routeDoc("orders", "/orders", "backend"),
			routeDoc("second", "/second", "second"),
		))
		wantOK(t, "second Route", get(t, c, n.URL("web")+"/second"))
	})
	t.Run("a Node booting a bound secret twice", func(t *testing.T) {
		before := issuer.Stats().TokenRequests
		booted, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
			Bundle:  with(oauthPolicy("oauth", issuer.TokenURL(), secretRef), apiKeyConsumer(secretRef)),
			Secrets: map[string][]byte{refOAuthSecret: []byte(secret)},
			NoWait:  true,
		})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the boot rejection", func() (bool, string) {
			recs := logRecords(booted[0], "revision rejected", "code", "RZ-CFG-041")
			return len(recs) > 0, "none logged"
		})
		if doc, status, err := booted[0].Readyz(t.Context()); err != nil || status != http.StatusServiceUnavailable {
			t.Fatalf("/readyz = %d %+v, %v; want 503 without a Revision", status, doc, err)
		}
		if got := issuer.Stats().TokenRequests; got != before {
			t.Fatalf("the rejected Node fetched %d tokens", got-before)
		}
	})
	if got := other.Stats().TokenRequests; got != 0 {
		t.Fatalf("the other token endpoint got %d requests; the secret is bound to its own tokenUrl", got)
	}
	wantOK(t, "request after every rejection", get(t, c, url))
}
