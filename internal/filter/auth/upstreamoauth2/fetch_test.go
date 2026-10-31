// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestTokenRequest checks the client credentials request (RFC 6749
// sections 4.4.2 and 2.3.1): a POST to tokenUrl, query kept, with a form
// body of grant_type=client_credentials and the scope parameter, JSON
// accepted, and client_secret_basic credentials whose clientId and secret
// are form-encoded before Basic encoding; the secret is not in the body.
func TestTokenRequest(t *testing.T) {
	h := running(t, issue(300))
	b := h.build(testPolicy, h.config(func(c *v1alpha1.AuthUpstreamOAuth2Config) {
		c.TokenURL += "?tenant=shop"
		c.Scopes = []string{"orders.read", "orders.write", "orders.read"}
	}))
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	req := h.ts.request(t, 1)
	if req.method != http.MethodPost || req.path != "/oauth2/token" || req.rawQuery != "tenant=shop" {
		t.Errorf("request = %s %s?%s, want POST /oauth2/token?tenant=shop", req.method, req.path, req.rawQuery)
	}
	if ct := req.header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", ct)
	}
	if a := req.header.Get("Accept"); a != "application/json" {
		t.Errorf("Accept = %q", a)
	}
	if !req.basic || req.user != testClientID || req.pass != testSecret {
		t.Errorf("Basic credentials = %v %q, want the client and its secret", req.basic, req.user)
	}
	if req.rawUser != url.QueryEscape(testClientID) || req.rawPass != url.QueryEscape(testSecret) {
		t.Errorf("Basic user %q and its password are not form-encoded", req.rawUser)
	}
	if got := req.form.Encode(); got != "grant_type=client_credentials&scope=orders.read+orders.write" {
		t.Errorf("form = %q", got)
	}
	if req.form.Has("client_secret") || req.form.Has("client_id") {
		t.Error("credentials sent in the body as well")
	}
}

// TestTokenRequestWithoutScopes checks that a config without scopes sends
// no scope parameter.
func TestTokenRequestWithoutScopes(t *testing.T) {
	h := running(t, issue(300))
	b := h.build(testPolicy, h.config(func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.Scopes = nil }))
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	if got := h.ts.request(t, 1).form.Encode(); got != "grant_type=client_credentials" {
		t.Errorf("form = %q, want grant_type only", got)
	}
}

// TestParseToken checks the 200 response rules: a JSON object with a
// non-empty visible-ASCII access_token, a Bearer token_type in any case
// or none, and expires_in a positive integer number or decimal string
// (DefaultLifetime when absent or null, clamped when beyond a Duration);
// anything else fails with a fixed error.
func TestParseToken(t *testing.T) {
	start := testStart()
	const maxLife = time.Duration(maxLifetimeSeconds) * time.Second
	for _, tc := range []struct {
		name, body string
		life       time.Duration
		err        error
	}{
		{"bearer", `{"access_token":"abc.def-ghi_~+/=","token_type":"Bearer","expires_in":3600}`, time.Hour, nil},
		{"lower-case type", `{"access_token":"a","token_type":"bearer","expires_in":60}`, time.Minute, nil},
		{"no type", `{"access_token":"a","expires_in":60}`, time.Minute, nil},
		{"string expires_in", `{"access_token":"a","expires_in":"120"}`, 2 * time.Minute, nil},
		{"no expires_in", `{"access_token":"a","token_type":"Bearer"}`, DefaultLifetime, nil},
		{"null expires_in", `{"access_token":"a","expires_in":null}`, DefaultLifetime, nil},
		{"huge expires_in", `{"access_token":"a","expires_in":99999999999999999999}`, maxLife, nil},
		{"extra members", `{"access_token":"a","expires_in":1,"scope":"x","refresh_token":"r"}`, time.Second, nil},
		{"not JSON", `access_token=a&token_type=bearer`, 0, errMalformed},
		{"array", `[{"access_token":"a"}]`, 0, errMalformed},
		{"numeric token", `{"access_token":12}`, 0, errMalformed},
		{"no token", `{"token_type":"Bearer","expires_in":60}`, 0, errNoAccessToken},
		{"empty token", `{"access_token":"","expires_in":60}`, 0, errNoAccessToken},
		{"token with space", `{"access_token":"a b","expires_in":60}`, 0, errTokenChars},
		{"token with CR LF", `{"access_token":"a\r\nX-Injected: 1","expires_in":60}`, 0, errTokenChars},
		{"non-ASCII token", `{"access_token":"tök","expires_in":60}`, 0, errTokenChars},
		{"mac token", `{"access_token":"a","token_type":"mac","expires_in":60}`, 0, errTokenType},
		{"DPoP token", `{"access_token":"a","token_type":"DPoP","expires_in":60}`, 0, errTokenType},
		{"zero expires_in", `{"access_token":"a","expires_in":0}`, 0, errExpiresIn},
		{"negative expires_in", `{"access_token":"a","expires_in":-5}`, 0, errExpiresIn},
		{"fractional expires_in", `{"access_token":"a","expires_in":1.5}`, 0, errExpiresIn},
		{"text expires_in", `{"access_token":"a","expires_in":"soon"}`, 0, errExpiresIn},
		{"boolean expires_in", `{"access_token":"a","expires_in":true}`, 0, errExpiresIn},
		{"huge negative expires_in", `{"access_token":"a","expires_in":-99999999999999999999}`, 0, errExpiresIn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := parseToken([]byte(tc.body), start)
			if tc.err != nil {
				if !errors.Is(err, tc.err) || tok != nil {
					t.Fatalf("parseToken(%s) = %v, %v; want %v", tc.body, tok, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseToken(%s): %v", tc.body, err)
			}
			if !tok.obtained.Equal(start) || tok.expires.Sub(start) != tc.life {
				t.Errorf("parseToken(%s) lifetime = %v, want %v", tc.body, tok.expires.Sub(start), tc.life)
			}
			var r tokenResponse
			_ = json.Unmarshal([]byte(tc.body), &r)
			if tok.header != "Bearer "+r.AccessToken {
				t.Errorf("parseToken(%s) header is not Bearer plus the access token", tc.body)
			}
		})
	}
}

// TestFetchStatusFailure checks that an answer other than 200 fails the
// fetch with its status, naming the RFC 6749 error code only when it is a
// known one, so nothing a server wrote reaches the error.
func TestFetchStatusFailure(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"invalid client", `{"error":"invalid_client"}`, http.StatusUnauthorized, "token endpoint answered status 401 with error invalid_client"},
		{"invalid scope", `{"error":"invalid_scope","error_description":"no"}`, http.StatusBadRequest, "token endpoint answered status 400 with error invalid_scope"},
		{"unknown code", `{"error":"stolen secret here"}`, http.StatusBadRequest, "token endpoint answered status 400"},
		{"not JSON", `<html>down</html>`, http.StatusServiceUnavailable, "token endpoint answered status 503"},
		{"created", tokenBody("tok", 60), http.StatusCreated, "token endpoint answered status 201"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := running(t, func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, tc.status, tc.body) })
			b := h.build(testPolicy, h.config())
			r, _ := handle(t.Context(), b)
			var se *statusError
			wantUndecided(t, r, nil)
			if !errors.As(r.Err, &se) || se.status != tc.status || se.Error() != tc.want {
				t.Errorf("error = %v, want %q", r.Err, tc.want)
			}
		})
	}
}

// TestFetchFollowsNoRedirect checks the secret-to-destination binding: a
// token endpoint that redirects fails the fetch (egress.ErrRedirect) and
// the redirect target receives nothing, so clientSecret reaches only the
// tokenUrl origin.
func TestFetchFollowsNoRedirect(t *testing.T) {
	var hits atomic.Int64
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(other.Close)
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			h := running(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL+"/token", status)
			})
			b := h.build(testPolicy, h.config())
			r, _ := handle(t.Context(), b)
			wantUndecided(t, r, egress.ErrRedirect)
		})
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("redirect target got %d requests, want 0", n)
	}
}

// TestFetchBodyCap checks that a token response above MaxResponseBytes
// fails the fetch.
func TestFetchBodyCap(t *testing.T) {
	h := running(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"access_token":"`+strings.Repeat("a", MaxResponseBytes)+`"}`)
	})
	b := h.build(testPolicy, h.config())
	r, _ := handle(t.Context(), b)
	wantUndecided(t, r, egress.ErrBodyTooLarge)
}

// TestFetchGuarded checks the token endpoint hop's transport rules: a
// loopback endpoint outside RURALZ_FETCH_ALLOW is refused before any
// connection (Secrets rule 6), and a server certificate the client does
// not trust fails the fetch (TB-10: the server is always verified).
func TestFetchGuarded(t *testing.T) {
	t.Run("denied address", func(t *testing.T) {
		h := newHarness(t, issue(300))
		h.reg = NewRegistry(Options{TLS: h.ts.roots(), Clock: h.clk})
		h.reg.fetchHook = func() {}
		h.start()
		b := h.build(testPolicy, h.config())
		r, _ := handle(t.Context(), b)
		wantUndecided(t, r, egress.ErrDenied)
		if n := h.ts.count(); n != 0 {
			t.Errorf("denied endpoint got %d requests", n)
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		h := newHarness(t, issue(300))
		guard, err := egress.ParseAllow("127.0.0.1/32")
		if err != nil {
			t.Fatal(err)
		}
		h.reg = NewRegistry(Options{Guard: guard, Clock: h.clk})
		h.reg.fetchHook = func() {}
		h.start()
		b := h.build(testPolicy, h.config())
		r, _ := handle(t.Context(), b)
		wantUndecided(t, r, nil)
		if n := h.ts.count(); n != 0 {
			t.Errorf("untrusted endpoint got %d requests", n)
		}
	})
}

// TestMockIdPInterop checks the grant against the test kit's RFC 6749
// token endpoint over TLS: it authenticates the client by
// client_secret_basic, with credentials that need form-encoding, and the
// token it issues is the one sent upstream.
func TestMockIdPInterop(t *testing.T) {
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	srvTLS, err := leaf.ServerTLS(nil)
	if err != nil {
		t.Fatal(err)
	}
	clk := testStart()
	idp, err := mockidp.Start(t.Context(), mockidp.Config{
		TLS:      srvTLS,
		Clients:  map[string]string{testClientID: testSecret},
		TokenTTL: 10 * time.Minute,
		Now:      func() time.Time { return clk },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idp.Close() })
	guard, err := egress.ParseAllow("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, issue(300))
	h.reg = NewRegistry(Options{Guard: guard, TLS: tlsconf.Client(ca.Pool()), Clock: h.clk})
	h.reg.fetchHook = func() {}
	h.start()
	b := h.build(testPolicy, h.config(func(c *v1alpha1.AuthUpstreamOAuth2Config) {
		c.TokenURL = idp.TokenURL()
		c.Scopes = []string{"orders.read"}
	}))
	r, auth := handle(t.Context(), b)
	reqs := idp.TokenRequests()
	if len(reqs) != 1 {
		t.Fatalf("mock IdP got %d token requests, want 1", len(reqs))
	}
	got := reqs[0]
	if got.AuthMethod != "client_secret_basic" || got.MalformedCredentials || got.ClientID != testClientID || got.ClientSecret != testSecret {
		t.Fatalf("mock IdP saw method %q client %q (malformed %v), want client_secret_basic for %q", got.AuthMethod, got.ClientID, got.MalformedCredentials, testClientID)
	}
	if got.GrantType != "client_credentials" || got.Scope != "orders.read" || got.Status != http.StatusOK {
		t.Fatalf("mock IdP saw grant %q scope %q and answered %d", got.GrantType, got.Scope, got.Status)
	}
	wantBearer(t, r, auth, got.AccessToken)
	if exp := b.e.tok.Load().expires.Sub(testStart()); exp != 10*time.Minute {
		t.Errorf("token lifetime = %v, want the IdP's expires_in of 10m", exp)
	}
}

// TestTokenNeverPrinted checks that a token value never reaches output
// through formatting or log/slog.
func TestTokenNeverPrinted(t *testing.T) {
	const value = "Bearer tok-value-123"
	tok := &token{header: value, obtained: testStart(), expires: testStart().Add(time.Minute)}
	var logs syncBuffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("token", "token", tok, "copy", *tok)
	outputs := []string{
		fmt.Sprint(tok), fmt.Sprintf("%v %+v %#v %s %q", tok, tok, tok, tok, tok),
		fmt.Sprintf("%v %+v %#v", *tok, *tok, *tok), string(logs.Bytes()),
	}
	for _, out := range outputs {
		if strings.Contains(out, "tok-value-123") || !strings.Contains(out, secret.Redacted) {
			t.Errorf("output %q leaks the token or is not redacted", out)
		}
	}
}

// TestFetchPanicRecovered checks that a panic inside a fetch fails that
// fetch with RZ-AUTH-020 instead of ending the process, and the Registry
// keeps serving.
func TestFetchPanicRecovered(t *testing.T) {
	h := running(t, issue(300))
	b := h.build(testPolicy, h.config())
	var once atomic.Bool
	h.secrets.mu.Lock()
	h.secrets.getHook = func() {
		if once.CompareAndSwap(false, true) {
			panic("secret store failure")
		}
	}
	h.secrets.mu.Unlock()
	r, _ := handle(t.Context(), b)
	wantUndecided(t, r, errPanic)
	h.clk.Advance(MinFetchInterval)
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
}

// TestFetchNoSecret checks that a fetch whose secret table no longer holds
// the reference fails with RZ-AUTH-020 and sends nothing.
func TestFetchNoSecret(t *testing.T) {
	h := running(t, issue(300))
	b := h.build(testPolicy, h.config())
	h.secrets.mu.Lock()
	clear(h.secrets.vals)
	h.secrets.mu.Unlock()
	r, _ := handle(t.Context(), b)
	wantUndecided(t, r, errNoSecret)
	if n := h.ts.count(); n != 0 {
		t.Errorf("token server got %d requests without a secret", n)
	}
}

// TestFetchTimeout checks that config.timeout bounds a fetch: an endpoint
// that never answers fails it with the deadline, without any wait on the
// request side beyond it.
func TestFetchTimeout(t *testing.T) {
	g := newGate(t)
	h := running(t, held(g, issue(300)))
	b := h.build(testPolicy, h.config(func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.Timeout = duration(50 * time.Millisecond) }))
	r, _ := handle(t.Context(), b)
	wantUndecided(t, r, nil)
	var te interface{ Timeout() bool }
	if !errors.Is(r.Err, context.DeadlineExceeded) && (!errors.As(r.Err, &te) || !te.Timeout()) {
		t.Errorf("error = %v, want a timeout", r.Err)
	}
}
