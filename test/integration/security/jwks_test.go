// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// JWKS rotation (docs/architecture/08-security-and-identity.md "JWT and
// OIDC"), every case in controlled time: keys live for max-age clamped
// to 5 minutes to 6 hours and refresh at half-life; an unknown kid fails
// with RZ-AUTH-005 and triggers at most one refresh per issuer per 30 s,
// honoring Retry-After; a failed fetch is retried after 30 s; Nodes
// prefetch issuers when compiling a Revision and activate even if a fetch
// fails.

// The JWKS timing targets the cases advance the clock by.
const (
	// refreshInterval is the most often an unknown kid triggers a fetch.
	refreshInterval = 30 * time.Second
	// halfLife is when a key set served with max-age=300 refreshes.
	halfLife = 150 * time.Second
	// maxAge300 is the Cache-Control of the cases' JWKS documents.
	maxAge300 = "public, max-age=300"
)

// jwtNode is one Node serving /orders behind auth.jwt for one mock IdP.
type jwtNode struct {
	top  *topology.Topology
	idp  *mockidp.Server
	node *topology.Node
	url  string
	c    *http.Client
}

// startJWT starts the IdP with c (Now is the topology's clock), lets
// prepare script it, then starts a Node whose Route /orders verifies its
// tokens, and waits until the Node is ready.
func startJWT(t *testing.T, c mockidp.Config, prepare func(*mockidp.Server)) *jwtNode {
	t.Helper()
	top := newTopology(t, topology.Config{})
	c.Now = top.Now
	idp, err := top.StartIdP(c)
	if err != nil {
		t.Fatal(err)
	}
	if prepare != nil {
		prepare(idp)
	}
	mock := startMock(t, top, mockup.Config{})
	b := bundle(
		gatewayDoc(webListener),
		upstreamDoc("backend", mock.Addr(), ""),
		policyDoc("jwt", "auth.jwt", fmt.Sprintf("    issuers:\n      - issuer: %q\n        jwksUrl: %q\n        audiences: [api]\n", idp.Issuer(), idp.JWKSURL())),
		routeDoc("orders", "/orders", "backend", "jwt"),
	)
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: b})
	if err != nil {
		t.Fatal(err)
	}
	return &jwtNode{top: top, idp: idp, node: nodes[0], url: nodes[0].URL("web") + "/orders", c: top.HTTPClient()}
}

// token mints a token for the Route's audience signed by k, with the
// header overrides of mockidp.Key.Sign (a nil kid removes it).
func (j *jwtNode) token(t *testing.T, k *mockidp.Key, header map[string]any) string {
	t.Helper()
	tok, err := j.idp.MintWith(k, header, map[string]any{"aud": "api", "sub": "alice"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// call sends a request with token to /orders.
func (j *jwtNode) call(t *testing.T, token string) reply {
	t.Helper()
	return get(t, j.c, j.url, bearer(token)...)
}

// fetches returns how many JWKS requests the IdP received.
func (j *jwtNode) fetches() int64 { return j.idp.Stats().JWKSFetches }

// waitFetches waits until the IdP received n JWKS requests and answered
// the last.
func (j *jwtNode) waitFetches(t *testing.T, n int64) {
	t.Helper()
	waitFor(t, fmt.Sprintf("JWKS fetch %d", n), func() (bool, string) {
		fs := j.idp.JWKSFetches()
		return int64(len(fs)) >= n && fs[len(fs)-1].Status != 0, fmt.Sprintf("%d fetches", len(fs))
	})
}

// wantFetches fails t unless the IdP received exactly n JWKS requests.
func (j *jwtNode) wantFetches(t *testing.T, what string, n int64) {
	t.Helper()
	if got := j.fetches(); got != n {
		t.Fatalf("%s: %d JWKS fetches, want %d", what, got, n)
	}
}

// unknownKid returns a token whose kid the IdP never published.
func (j *jwtNode) unknownKid(t *testing.T, name string) string {
	t.Helper()
	k, err := mockidp.GenerateKey(mockidp.RS256)
	if err != nil {
		t.Fatal(err)
	}
	return j.token(t, k, map[string]any{"kid": name})
}

// decisions returns the Node's auth decisions with code, past the
// metrics reuse window.
func (j *jwtNode) decisions(t *testing.T, code string) float64 {
	t.Helper()
	return freshCounter(t, j.top, j.node, "ruralz_auth_decisions_total", "code", code)
}

func TestJWKSNewKeyCostsOneUnknownKidAndOneRefresh(t *testing.T) {
	// A new key published and signing costs one RZ-AUTH-005 and one
	// refresh, then passes.
	j := startJWT(t, mockidp.Config{CacheControl: maxAge300}, nil)
	j.waitFetches(t, 1) // the prefetch when the Revision compiled
	old, _ := j.idp.Key(mockidp.RS256)
	wantOK(t, "token of the prefetched key", j.call(t, j.token(t, old, nil)))

	// The last fetch began at the activation; a refresh may start once
	// the 30 s interval passed.
	advance(t, j.top, refreshInterval)
	k, err := j.idp.Rotate(mockidp.RS256, true)
	if err != nil {
		t.Fatal(err)
	}
	wantCode(t, "first token of the new key", j.call(t, j.token(t, k, nil)), http.StatusUnauthorized, "RZ-AUTH-005")
	j.waitFetches(t, 2)

	// A token without a kid tries every RS256 key in document order: it
	// is RZ-AUTH-002 until the refreshed set holds the new key, and it
	// triggers no refresh, so it shows when the refresh landed.
	noKid := j.token(t, k, map[string]any{"kid": nil})
	waitFor(t, "the refreshed key set", func() (bool, string) {
		r := j.call(t, noKid)
		return r.status == http.StatusOK, r.String()
	})
	wantOK(t, "token of the new key after the refresh", j.call(t, j.token(t, k, nil)))
	wantOK(t, "token of the previous key", j.call(t, j.token(t, old, nil)))
	j.wantFetches(t, "after the new key verified", 2)
	if got := j.decisions(t, "RZ-AUTH-005"); got != 1 {
		t.Fatalf("%v RZ-AUTH-005 decisions, want 1", got)
	}
}

func TestJWKSRefreshAtMostOncePer30sHonoringRetryAfter(t *testing.T) {
	// An unknown kid triggers at most one refresh per issuer per 30 s,
	// and none before a failed fetch's Retry-After passes.
	j := startJWT(t, mockidp.Config{CacheControl: maxAge300}, nil)
	j.waitFetches(t, 1) // t0: the prefetch

	// t0+30 s: the first unknown kid refreshes; more within 30 s do not.
	advance(t, j.top, refreshInterval)
	wantCode(t, "unknown kid at t0+30s", j.call(t, j.unknownKid(t, "unknown-1")), http.StatusUnauthorized, "RZ-AUTH-005")
	j.waitFetches(t, 2)
	for i, at := range []time.Duration{0, 10 * time.Second, 19 * time.Second} {
		advance(t, j.top, at)
		wantCode(t, fmt.Sprintf("unknown kid %d within 30 s", i+2), j.call(t, j.unknownKid(t, fmt.Sprintf("unknown-%d", i+2))), http.StatusUnauthorized, "RZ-AUTH-005")
	}
	// t0+59 s: still within 30 s of the refresh that began at t0+30 s.
	wantCode(t, "unknown kid at t0+59s", j.call(t, j.unknownKid(t, "unknown-5")), http.StatusUnauthorized, "RZ-AUTH-005")
	// t0+60 s: the next refresh may start. Had any request since t0+30 s
	// fetched, its fetch would have been recorded before this one.
	advance(t, j.top, time.Second)
	wantCode(t, "unknown kid at t0+60s", j.call(t, j.unknownKid(t, "unknown-6")), http.StatusUnauthorized, "RZ-AUTH-005")
	j.waitFetches(t, 3)
	j.wantFetches(t, "one refresh per 30 s", 3)

	// t0+90 s: a refresh that fails with Retry-After: 120 defers every
	// fetch to t0+210 s, unknown kids included.
	j.idp.SetJWKSOverride(mockidp.Override{Status: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"120"}}})
	advance(t, j.top, refreshInterval)
	wantCode(t, "unknown kid at t0+90s", j.call(t, j.unknownKid(t, "unknown-7")), http.StatusUnauthorized, "RZ-AUTH-005")
	j.waitFetches(t, 4)
	waitDegraded(t, j.node, "jwks_stale", true)
	j.idp.SetJWKSOverride(mockidp.Override{})
	// The cached keys still serve while the refresh fails.
	k, _ := j.idp.Key(mockidp.RS256)
	wantOK(t, "cached key while the refresh fails", j.call(t, j.token(t, k, nil)))
	for _, step := range []time.Duration{refreshInterval, refreshInterval, 59 * time.Second} {
		advance(t, j.top, step)
		wantCode(t, "unknown kid before Retry-After", j.call(t, j.unknownKid(t, "unknown-before-retry")), http.StatusUnauthorized, "RZ-AUTH-005")
	}
	// t0+209 s: nothing fetched since the failure.
	j.wantFetches(t, "before Retry-After passed", 4)
	// t0+210 s: the deferred retry runs on its own.
	advance(t, j.top, time.Second)
	j.waitFetches(t, 5)
	waitDegraded(t, j.node, "jwks_stale", false)
	j.wantFetches(t, "after Retry-After", 5)
}

func TestJWKSFailedPrefetchStillActivates(t *testing.T) {
	// A failed prefetch at activation still activates: tokens get
	// RZ-AUTH-006 until the retry 30 s later fetches the keys.
	j := startJWT(t, mockidp.Config{CacheControl: maxAge300}, func(idp *mockidp.Server) {
		idp.SetJWKSOverride(mockidp.Override{Status: http.StatusInternalServerError})
	})
	// AddNodes returned once /readyz answered 200: the Revision is
	// active although its prefetch failed.
	j.waitFetches(t, 1)
	if fs := j.idp.JWKSFetches(); fs[0].Status != http.StatusInternalServerError {
		t.Fatalf("prefetch answered %d, want the scripted 500", fs[0].Status)
	}
	waitDegraded(t, j.node, "jwks_stale", true)
	k, _ := j.idp.Key(mockidp.RS256)
	tok := j.token(t, k, nil)
	wantCode(t, "token without usable keys", j.call(t, tok), http.StatusUnauthorized, "RZ-AUTH-006")
	j.idp.SetJWKSOverride(mockidp.Override{})
	// The issuer without usable keys nudges no fetch before the retry.
	advance(t, j.top, refreshInterval-time.Second)
	wantCode(t, "token before the retry", j.call(t, tok), http.StatusUnauthorized, "RZ-AUTH-006")
	j.wantFetches(t, "before the retry", 1)
	advance(t, j.top, time.Second)
	j.waitFetches(t, 2)
	waitFor(t, "keys after the retry", func() (bool, string) {
		r := j.call(t, tok)
		return r.status == http.StatusOK, r.String()
	})
	waitDegraded(t, j.node, "jwks_stale", false)
	j.wantFetches(t, "after the retry", 2)
}

func TestJWKSRemovedKeyStopsAtHalfLife(t *testing.T) {
	// A key removed from the document keeps verifying from the cached set
	// until the 150 s half-life of max-age=300 passes, advanced on the
	// test clock, then stops.
	var removed *mockidp.Key
	j := startJWT(t, mockidp.Config{CacheControl: maxAge300}, func(idp *mockidp.Server) {
		var err error
		if removed, err = idp.Rotate(mockidp.RS256, true); err != nil {
			t.Fatal(err)
		}
	})
	j.waitFetches(t, 1)
	tok := j.token(t, removed, nil)
	wantOK(t, "token of the published key", j.call(t, tok))
	if n := j.idp.RemoveKey(removed.ID); n != 1 {
		t.Fatalf("RemoveKey removed %d keys, want 1", n)
	}
	wantOK(t, "removed key before any refresh", j.call(t, tok))
	advance(t, j.top, halfLife-time.Second)
	wantOK(t, "removed key 1 s before the half-life", j.call(t, tok))
	j.wantFetches(t, "before the half-life", 1)

	advance(t, j.top, time.Second)
	j.waitFetches(t, 2)
	waitFor(t, "the removed key to stop verifying", func() (bool, string) {
		r := j.call(t, tok)
		return r.status == http.StatusUnauthorized && r.code == "RZ-AUTH-005", r.String()
	})
	kept, _ := j.idp.Key(mockidp.RS256)
	wantOK(t, "the key still published", j.call(t, j.token(t, kept, nil)))
	j.wantFetches(t, "after the half-life", 2)
}
