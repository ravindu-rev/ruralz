// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockidp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Server tests: JWKS endpoint with rotating keys and Cache-Control (06
// reqs 36 to 38, 11 req 60 FC-16), the client-credentials token endpoint
// (06 reqs 69 to 71), overrides and recording (11 req 39).

func serverTLS(t testing.TB) (*tls.Config, *x509.CertPool) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mockidp test"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf}}}, pool
}

func start(t testing.TB, c Config) *Server {
	t.Helper()
	if c.Keys == nil {
		c.Keys = []*Key{key(t, RS256), key(t, ES256)}
	}
	s, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func httpClient(t testing.TB, pool *x509.CertPool) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	if pool != nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{
		Transport: tr,
		Timeout:   20 * time.Second,
		// auth.upstream-oauth2 never follows redirects (06 req 69).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type answer struct {
	status int
	header http.Header
	body   []byte
}

func send(t testing.TB, c *http.Client, method, u string, body string, header http.Header) (answer, error) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, u, rd)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return answer{status: resp.StatusCode, header: resp.Header, body: b}, err
}

func mustSend(t testing.TB, c *http.Client, method, u string, body string, header http.Header) answer {
	t.Helper()
	a, err := send(t, c, method, u, body, header)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestJWKSEndpoint(t *testing.T) { // 06 req 37 fetch shape; 06 req 36 Cache-Control
	tlsConf, pool := serverTLS(t)
	s := start(t, Config{TLS: tlsConf})
	if !strings.HasPrefix(s.JWKSURL(), "https://127.0.0.1:") || s.JWKSURL() != s.URL()+JWKSPath || s.TokenURL() != s.URL()+TokenPath {
		t.Fatalf("URLs %s %s", s.JWKSURL(), s.TokenURL())
	}
	if s.Issuer() != s.URL() || s.Addr() == "" {
		t.Fatalf("issuer %q addr %q", s.Issuer(), s.Addr())
	}
	c := httpClient(t, pool)
	a := mustSend(t, c, http.MethodGet, s.JWKSURL(), "", http.Header{"Accept": {"application/jwk-set+json, application/json"}})
	if a.status != 200 || a.header.Get("Content-Type") != JWKSContentType || a.header.Get("Cache-Control") != DefaultCacheControl {
		t.Fatalf("answer %d %v", a.status, a.header)
	}
	keys, err := ParseJWKS(a.body)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].Alg != RS256 || keys[1].Alg != ES256 || string(a.body) != string(s.JWKS()) {
		t.Fatalf("keys = %+v", keys)
	}
	// HEAD has no body; other methods are refused.
	if a := mustSend(t, c, http.MethodHead, s.JWKSURL(), "", nil); a.status != 200 || len(a.body) != 0 {
		t.Fatalf("HEAD %d %q", a.status, a.body)
	}
	if a := mustSend(t, c, http.MethodPost, s.JWKSURL(), "x", nil); a.status != http.StatusMethodNotAllowed || a.header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST %d %v", a.status, a.header)
	}
	if a := mustSend(t, c, http.MethodGet, s.URL()+"/other", "", nil); a.status != http.StatusNotFound {
		t.Fatalf("other path %d", a.status)
	}
	// Cache-Control is programmable (the FC-16 clamp tests use max-age).
	for _, cc := range []string{"max-age=60", "no-cache", "no-store", "max-age=86400"} {
		s.SetCacheControl(cc)
		if got := mustSend(t, c, http.MethodGet, s.JWKSURL(), "", nil).header.Get("Cache-Control"); got != cc {
			t.Fatalf("Cache-Control %q, want %q", got, cc)
		}
	}
	s.SetCacheControl("")
	if got := mustSend(t, c, http.MethodGet, s.JWKSURL(), "", nil).header.Values("Cache-Control"); len(got) != 0 {
		t.Fatalf("Cache-Control %v, want absent", got)
	}
	fetches := s.JWKSFetches()
	if len(fetches) != 8 || fetches[0].Header.Get("Accept") != "application/jwk-set+json, application/json" || fetches[0].Status != 200 ||
		fetches[0].Method != http.MethodGet || fetches[0].RemoteAddr == "" || fetches[0].Time.IsZero() {
		t.Fatalf("fetches = %+v", fetches)
	}
	if st := s.Stats(); st.JWKSFetches != 8 {
		t.Fatalf("Stats = %+v", st)
	}
	s2 := start(t, Config{NoCacheControl: true})
	if got := mustSend(t, httpClient(t, nil), http.MethodGet, s2.JWKSURL(), "", nil).header.Values("Cache-Control"); len(got) != 0 {
		t.Fatalf("NoCacheControl: Cache-Control %v", got)
	}
	if !strings.HasPrefix(s2.URL(), "http://") {
		t.Fatalf("cleartext URL %q", s2.URL())
	}
}

func TestDefaultKeys(t *testing.T) {
	s, err := Start(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	keys := s.Keys()
	if len(keys) != 2 || keys[0].Alg != RS256 || keys[1].Alg != ES256 {
		t.Fatalf("default keys = %v", keys)
	}
}

func TestRotation(t *testing.T) { // 06 test plan: key rotation at the IdP (new kid)
	es := key(t, ES256)
	s := start(t, Config{Keys: []*Key{es}})
	c := httpClient(t, nil)
	fetch := func() []PublicKey {
		t.Helper()
		keys, err := ParseJWKS(mustSend(t, c, http.MethodGet, s.JWKSURL(), "", nil).body)
		if err != nil {
			t.Fatal(err)
		}
		return keys
	}
	before, err := s.Mint(ES256, map[string]any{"sub": "a"})
	if err != nil {
		t.Fatal(err)
	}
	k2, err := s.Rotate(ES256, true)
	if err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.Key(ES256); cur != k2 || k2.ID == es.ID {
		t.Fatalf("signing key %v after rotation to %v", cur, k2)
	}
	after, err := s.Mint(ES256, map[string]any{"sub": "a"})
	if err != nil {
		t.Fatal(err)
	}
	keys := fetch()
	if len(keys) != 2 {
		t.Fatalf("published %d keys after a keeping rotation", len(keys))
	}
	for _, tok := range []string{before, after} {
		if _, _, err := Verify(tok, keys); err != nil {
			t.Fatalf("token after keeping rotation: %v", err)
		}
	}
	h, _, _ := Verify(after, keys)
	if h["kid"] != k2.ID {
		t.Fatalf("new token kid %v", h["kid"])
	}
	// Without keeping, the old kid disappears: old tokens fail (auth.jwt
	// answers RZ-AUTH-005 and refreshes).
	k3, err := s.Rotate(ES256, false)
	if err != nil {
		t.Fatal(err)
	}
	keys = fetch()
	if len(keys) != 1 || keys[0].ID != k3.ID {
		t.Fatalf("keys after replacing rotation = %+v", keys)
	}
	if _, _, err := Verify(before, keys); err == nil {
		t.Fatal("token of a removed key verified")
	}
	// AddKey and RemoveKey; duplicate kids are kept.
	dup, err := NewKey(ES256, k3.ID, key(t, ES256).Signer())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey(dup); err != nil {
		t.Fatal(err)
	}
	if n := len(fetch()); n != 2 {
		t.Fatalf("published %d keys with a duplicate kid", n)
	}
	if n := s.RemoveKey(k3.ID); n != 2 {
		t.Fatalf("RemoveKey removed %d", n)
	}
	if n := len(fetch()); n != 0 {
		t.Fatalf("published %d keys after removal", n)
	}
	if _, err := s.Mint(ES256, nil); err == nil {
		t.Fatal("Mint without a key succeeded")
	}
	if err := s.AddKey(nil); err == nil {
		t.Fatal("AddKey(nil) succeeded")
	}
	if _, err := s.Rotate("HS256", true); err == nil {
		t.Fatal("Rotate(HS256) succeeded")
	}
	if st := s.Stats(); st.Rotations != 2 {
		t.Fatalf("Stats = %+v", st)
	}
}

func TestPeriodicRotation(t *testing.T) { // rotating keys (Config.RotateEvery)
	es := key(t, ES256)
	s := start(t, Config{Keys: []*Key{es}, RotateEvery: 20 * time.Millisecond})
	waitFor(t, func() bool { return s.Stats().Rotations >= 3 })
	keys := s.Keys()
	if len(keys) != 2 || keys[0].ID == es.ID || keys[1].ID == es.ID || keys[0].ID == keys[1].ID {
		t.Fatalf("keys after periodic rotation = %v", keys)
	}
	cur, _ := s.Key(ES256)
	if cur != keys[1] {
		t.Fatal("the newest key does not sign")
	}
	_ = s.Close()
	n := s.Stats().Rotations
	time.Sleep(60 * time.Millisecond)
	if s.Stats().Rotations != n {
		t.Fatal("rotation continued after Close")
	}
}

func TestJWKSOverride(t *testing.T) { // FC-16 and 06 req 36: 429 Retry-After, failed fetches, slow IdP
	s := start(t, Config{})
	c := httpClient(t, nil)
	s.SetJWKSOverride(Override{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"120"}}})
	a := mustSend(t, c, http.MethodGet, s.JWKSURL(), "", nil)
	if a.status != 429 || a.header.Get("Retry-After") != "120" || len(a.body) != 0 {
		t.Fatalf("answer %d %v %q", a.status, a.header, a.body)
	}
	many := `{"keys":[` + strings.TrimSuffix(strings.Repeat(`{"kty":"oct","k":"AA"},`, 257), ",") + `]}`
	s.SetJWKSOverride(Override{Body: []byte(many)})
	a = mustSend(t, c, http.MethodGet, s.JWKSURL(), "", nil)
	if a.status != 200 || string(a.body) != many || a.header.Get("Content-Type") != JWKSContentType {
		t.Fatalf("body override %d %v", a.status, a.header)
	}
	s.SetJWKSOverride(Override{Delay: 50 * time.Millisecond, Header: http.Header{"X-Extra": {"1"}}})
	began := time.Now()
	a = mustSend(t, c, http.MethodGet, s.JWKSURL(), "", nil)
	if time.Since(began) < 50*time.Millisecond || a.status != 200 || a.header.Get("X-Extra") != "1" || string(a.body) != string(s.JWKS()) {
		t.Fatalf("delayed answer %d %v after %v", a.status, a.header, time.Since(began))
	}
	s.SetJWKSOverride(Override{})
	if a := mustSend(t, c, http.MethodGet, s.JWKSURL(), "", nil); a.status != 200 || a.header.Get("X-Extra") != "" {
		t.Fatalf("after reset %d %v", a.status, a.header)
	}
	fetches := s.JWKSFetches()
	if fetches[0].Status != 429 {
		t.Fatalf("recorded status %d", fetches[0].Status)
	}
}

// basic encodes client_secret_basic credentials (RFC 6749 section 2.3.1:
// form-urlencode each part, then HTTP Basic).
func basic(id, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(id)+":"+url.QueryEscape(secret)))
}

// rawBasic sends id and secret as HTTP Basic credentials without the
// form-encoding RFC 6749 section 2.3.1 asks for.
func rawBasic(id, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(id+":"+secret))
}

func TestTokenEndpoint(t *testing.T) { // 06 req 69 to 71: client credentials grant
	const (
		id     = "orders:svc%1"
		secret = "rzcanary-0123:secret%" //nolint:gosec // G101: a test client secret for the mock IdP
	)
	s := start(t, Config{Clients: map[string]string{id: secret}, TokenTTL: 10 * time.Minute, TokenAudience: "orders-api"})
	c := httpClient(t, nil)
	form := "application/x-www-form-urlencoded"
	for _, tc := range []struct {
		name       string
		method     string
		body       string
		header     http.Header
		wantStatus int
		wantError  string
	}{
		{"basic", http.MethodPost, "grant_type=client_credentials&scope=read+write", http.Header{"Authorization": {basic(id, secret)}, "Content-Type": {form}}, 200, ""},
		{"post", http.MethodPost, "grant_type=client_credentials&client_id=" + url.QueryEscape(id) + "&client_secret=" + url.QueryEscape(secret), http.Header{"Content-Type": {form + "; charset=utf-8"}}, 200, ""},
		{"both methods", http.MethodPost, "grant_type=client_credentials&client_secret=x", http.Header{"Authorization": {basic(id, secret)}, "Content-Type": {form}}, 400, "invalid_request"},
		{"no credentials", http.MethodPost, "grant_type=client_credentials", http.Header{"Content-Type": {form}}, 401, "invalid_client"},
		{"wrong secret", http.MethodPost, "grant_type=client_credentials", http.Header{"Authorization": {basic(id, "nope")}, "Content-Type": {form}}, 401, "invalid_client"},
		{"unknown client", http.MethodPost, "grant_type=client_credentials", http.Header{"Authorization": {basic("other", secret)}, "Content-Type": {form}}, 401, "invalid_client"},
		{"GET", http.MethodGet, "", http.Header{"Authorization": {basic(id, secret)}}, 405, "invalid_request"},
		{"content type", http.MethodPost, `{"grant_type":"client_credentials"}`, http.Header{"Authorization": {basic(id, secret)}, "Content-Type": {"application/json"}}, 400, "invalid_request"},
		{"malformed form", http.MethodPost, "grant_type=%zz", http.Header{"Authorization": {basic(id, secret)}, "Content-Type": {form}}, 400, "invalid_request"},
		{"no grant", http.MethodPost, "scope=a", http.Header{"Authorization": {basic(id, secret)}, "Content-Type": {form}}, 400, "invalid_request"},
		{"password grant", http.MethodPost, "grant_type=password", http.Header{"Authorization": {basic(id, secret)}, "Content-Type": {form}}, 400, "unsupported_grant_type"},
		{"too large", http.MethodPost, "grant_type=client_credentials&x=" + strings.Repeat("a", maxTokenBody), http.Header{"Authorization": {basic(id, secret)}, "Content-Type": {form}}, 413, "invalid_request"},
		// RFC 6749 section 3.2.1: client_id in the body identifies a Basic
		// client; it is not a second authentication method.
		{"basic with client_id in body", http.MethodPost, "grant_type=client_credentials&client_id=" + url.QueryEscape(id), http.Header{"Authorization": {basic(id, secret)}, "Content-Type": {form}}, 200, ""},
		// 06 test plan "Basic auth encoding": a secret sent without
		// form-encoding ("%" raw) is rejected but recorded as sent.
		{"unencoded secret", http.MethodPost, "grant_type=client_credentials", http.Header{"Authorization": {rawBasic(url.QueryEscape(id), secret)}, "Content-Type": {form}}, 401, "invalid_client"},
		{"post secret without id", http.MethodPost, "grant_type=client_credentials&client_secret=" + url.QueryEscape(secret), http.Header{"Content-Type": {form}}, 401, "invalid_client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := mustSend(t, c, tc.method, s.TokenURL(), tc.body, tc.header)
			if a.status != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", a.status, tc.wantStatus, a.body)
			}
			if a.header.Get("Cache-Control") != "no-store" || a.header.Get("Content-Type") != "application/json" {
				t.Fatalf("headers %v", a.header)
			}
			if tc.wantError != "" {
				var e tokenError
				if err := json.Unmarshal(a.body, &e); err != nil || e.Error != tc.wantError {
					t.Fatalf("error body %s (%v), want %s", a.body, err, tc.wantError)
				}
				if (tc.name == "wrong secret" || tc.name == "unencoded secret") && a.header.Get("WWW-Authenticate") == "" {
					t.Fatal("no WWW-Authenticate on a Basic failure")
				}
				// A request refused for its body size still records the
				// Basic credentials it presented (the canary destination
				// of 11 req 39).
				if tc.name == "too large" {
					reqs := s.TokenRequests()
					r := reqs[len(reqs)-1]
					if r.Status != http.StatusRequestEntityTooLarge || r.AuthMethod != "client_secret_basic" || r.ClientID != id || r.ClientSecret != secret ||
						r.RawClientSecret == "" || r.RawClientSecret != url.QueryEscape(secret) {
						t.Fatalf("413 record: status %d, method %q, id %q, secret %q, raw secret %q", r.Status, r.AuthMethod, r.ClientID, r.ClientSecret, r.RawClientSecret)
					}
				}
				return
			}
			var tr tokenResponse
			if err := json.Unmarshal(a.body, &tr); err != nil {
				t.Fatal(err)
			}
			if tr.TokenType != "Bearer" || tr.ExpiresIn != 600 || tr.AccessToken == "" {
				t.Fatalf("token response %+v", tr)
			}
			keys, err := ParseJWKS(s.JWKS())
			if err != nil {
				t.Fatal(err)
			}
			h, claims, err := Verify(tr.AccessToken, keys)
			if err != nil {
				t.Fatalf("access token does not verify: %v", err)
			}
			rs, _ := s.Key(RS256)
			if h["alg"] != "RS256" || h["kid"] != rs.ID {
				t.Fatalf("access token header %v", h)
			}
			if claims["sub"] != id || claims["client_id"] != id || claims["aud"] != "orders-api" || claims["iss"] != s.Issuer() || claims["jti"] == nil {
				t.Fatalf("claims %v", claims)
			}
			iat, _ := claims["iat"].(json.Number).Int64()
			exp, _ := claims["exp"].(json.Number).Int64()
			if exp-iat != 600 {
				t.Fatalf("lifetime %d s", exp-iat)
			}
		})
	}
	reqs := s.TokenRequests()
	if len(reqs) != 15 {
		t.Fatalf("recorded %d token requests", len(reqs))
	}
	first := reqs[0]
	if first.AuthMethod != "client_secret_basic" || first.ClientID != id || first.ClientSecret != secret ||
		first.GrantType != "client_credentials" || first.Scope != "read write" || first.Status != 200 || first.AccessToken == "" {
		t.Fatalf("first request %+v", first)
	}
	// The raw Basic parts show how the client encoded ':' and '%'.
	if first.RawClientID != "orders%3Asvc%251" || first.RawClientSecret != url.QueryEscape(secret) || first.MalformedCredentials {
		t.Fatalf("first request raw credentials %q %q malformed %v", first.RawClientID, first.RawClientSecret, first.MalformedCredentials)
	}
	if r := reqs[12]; r.AuthMethod != "client_secret_basic" || r.ClientID != id || r.Status != 200 {
		t.Fatalf("Basic with client_id in body: %+v", r)
	}
	// 11 req 39: the token endpoint is a designated canary destination, so
	// the record keeps a credential it could not decode.
	if r := reqs[13]; r.AuthMethod != "client_secret_basic" || !r.MalformedCredentials || r.ClientID != id ||
		r.ClientSecret != secret || r.RawClientSecret != secret || r.Status != 401 || r.AccessToken != "" {
		t.Fatalf("unencoded secret: %+v", r)
	}
	if r := reqs[14]; r.AuthMethod != "client_secret_post" || r.ClientID != "" || r.ClientSecret != secret || r.RawClientSecret != secret || r.Status != 401 {
		t.Fatalf("post secret without id: %+v", r)
	}
	if first.Header.Get("Authorization") == "" || first.Form.Get("scope") != "read write" || first.Method != http.MethodPost {
		t.Fatalf("first request header %v form %v", first.Header, first.Form)
	}
	if reqs[1].AuthMethod != "client_secret_post" || reqs[1].ClientSecret != secret || reqs[1].RawClientSecret != secret || reqs[2].AuthMethod != "" {
		t.Fatalf("auth methods %q %q", reqs[1].AuthMethod, reqs[2].AuthMethod)
	}
	claims := verifyClaims(t, s, first.AccessToken)
	if claims["scope"] != "read write" {
		t.Fatalf("scope claim %v", claims["scope"])
	}
	if st := s.Stats(); st.TokenRequests != 15 || st.TokensIssued != 3 {
		t.Fatalf("Stats = %+v", st)
	}
	// Any client when none is configured; SetClient adds one.
	open := start(t, Config{})
	a := mustSend(t, c, http.MethodPost, open.TokenURL(), "grant_type=client_credentials", http.Header{"Authorization": {basic("any", "thing")}, "Content-Type": {form}})
	if a.status != 200 {
		t.Fatalf("open IdP answered %d", a.status)
	}
	// Malformed Basic credentials fail even where any client is accepted.
	a = mustSend(t, c, http.MethodPost, open.TokenURL(), "grant_type=client_credentials", http.Header{"Authorization": {rawBasic("any", "raw%zz")}, "Content-Type": {form}})
	if a.status != 401 {
		t.Fatalf("open IdP answered %d to an unencoded secret", a.status)
	}
	if r := open.TokenRequests()[1]; r.ClientSecret != "raw%zz" || r.RawClientSecret != "raw%zz" || !r.MalformedCredentials {
		t.Fatalf("unencoded secret record %+v", r)
	}
	open.SetClient("only", "s3")
	a = mustSend(t, c, http.MethodPost, open.TokenURL(), "grant_type=client_credentials", http.Header{"Authorization": {basic("any", "thing")}, "Content-Type": {form}})
	if a.status != 401 {
		t.Fatalf("after SetClient an unknown client got %d", a.status)
	}
}

func verifyClaims(t *testing.T, s *Server, token string) map[string]any {
	t.Helper()
	keys, err := ParseJWKS(s.JWKS())
	if err != nil {
		t.Fatal(err)
	}
	_, claims, err := Verify(token, keys)
	if err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestTokenOverride(t *testing.T) { // 06 test plan: 500 → 020, 302 not followed, missing expires_in, timeout
	s := start(t, Config{})
	c := httpClient(t, nil)
	hdr := http.Header{"Authorization": {basic("a", "b")}, "Content-Type": {"application/x-www-form-urlencoded"}}
	body := "grant_type=client_credentials"
	s.SetTokenOverride(Override{Status: 500})
	if a := mustSend(t, c, http.MethodPost, s.TokenURL(), body, hdr); a.status != 500 {
		t.Fatalf("status %d", a.status)
	}
	s.SetTokenOverride(Override{Status: 302, Header: http.Header{"Location": {"https://elsewhere.test/token"}}})
	if a := mustSend(t, c, http.MethodPost, s.TokenURL(), body, hdr); a.status != 302 || a.header.Get("Location") == "" {
		t.Fatalf("redirect %d %v", a.status, a.header)
	}
	s.SetTokenOverride(Override{Body: []byte(`{"access_token":"x","token_type":"mac"}`)})
	if a := mustSend(t, c, http.MethodPost, s.TokenURL(), body, hdr); a.status != 200 || string(a.body) != `{"access_token":"x","token_type":"mac"}` {
		t.Fatalf("body override %d %s", a.status, a.body)
	}
	s.SetTokenOverride(Override{Header: http.Header{"X-Extra": {"1"}}})
	if a := mustSend(t, c, http.MethodPost, s.TokenURL(), body, hdr); a.status != 200 || a.header.Get("X-Extra") != "1" {
		t.Fatalf("header override %d %v", a.status, a.header)
	}
	// A slow endpoint: the client's deadline passes first.
	s.SetTokenOverride(Override{Delay: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.TokenURL(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = hdr
	if resp, err := c.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("slow token request succeeded")
	}
	s.SetTokenOverride(Override{})
	if a := mustSend(t, c, http.MethodPost, s.TokenURL(), body, hdr); a.status != 200 {
		t.Fatalf("after reset %d", a.status)
	}
}

func TestMintDefaults(t *testing.T) { // auth.jwt claim tests (06 req 30) mint against a fixed clock
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	s := start(t, Config{Issuer: "https://idp.test", TokenTTL: 5 * time.Minute, Now: func() time.Time { return now }})
	tok, err := s.Mint(RS256, map[string]any{"aud": []string{"orders"}, "sub": "alice"})
	if err != nil {
		t.Fatal(err)
	}
	claims := verifyClaims(t, s, tok)
	if claims["iss"] != "https://idp.test" || claims["iat"] != json.Number("1790424000") || claims["exp"] != json.Number("1790424300") {
		t.Fatalf("claims %v", claims)
	}
	tok, err = s.Mint(ES256, map[string]any{"exp": nil, "iat": nil, "nbf": now.Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	claims = verifyClaims(t, s, tok)
	if _, has := claims["exp"]; has || claims["nbf"] != json.Number("1790427600") {
		t.Fatalf("claims %v", claims)
	}
	if _, err := s.Mint(PS256, nil); err == nil {
		t.Fatal("Mint(PS256) without a PS256 key succeeded")
	}
	// MintWith an unpublished key and a header override.
	tok, err = s.MintWith(key(t, EdDSA), map[string]any{"kid": "rotated-away"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := ParseJWKS(s.JWKS())
	if _, _, err := Verify(tok, keys); err == nil {
		t.Fatal("token of an unpublished key verified")
	}
}

func TestAccessTokenFallbacks(t *testing.T) {
	c := httpClient(t, nil)
	hdr := http.Header{"Authorization": {basic("a", "b")}, "Content-Type": {"application/x-www-form-urlencoded"}}
	// Without an RS256 key the last published key signs.
	s := start(t, Config{Keys: []*Key{key(t, ES256)}})
	var tr tokenResponse
	if err := json.Unmarshal(mustSend(t, c, http.MethodPost, s.TokenURL(), "grant_type=client_credentials", hdr).body, &tr); err != nil {
		t.Fatal(err)
	}
	keys, _ := ParseJWKS(s.JWKS())
	if h, _, err := Verify(tr.AccessToken, keys); err != nil || h["alg"] != "ES256" {
		t.Fatalf("ES256 access token: %v %v", h, err)
	}
	// Without any key the token is opaque.
	s.RemoveKey(key(t, ES256).ID)
	if err := json.Unmarshal(mustSend(t, c, http.MethodPost, s.TokenURL(), "grant_type=client_credentials", hdr).body, &tr); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tr.AccessToken, "opaque-") {
		t.Fatalf("access token %q", tr.AccessToken)
	}
}

func TestRecordingBounded(t *testing.T) {
	s := start(t, Config{})
	c := httpClient(t, nil)
	for range MaxRecorded + 5 {
		mustSend(t, c, http.MethodGet, s.JWKSURL(), "", nil)
	}
	if n := len(s.JWKSFetches()); n != MaxRecorded {
		t.Fatalf("recorded %d fetches, want %d", n, MaxRecorded)
	}
	if st := s.Stats(); st.JWKSFetches != MaxRecorded+5 {
		t.Fatalf("Stats = %+v", st)
	}
	got := appendBounded([]int{1, 2}, 3)
	if !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("appendBounded = %v", got)
	}
}

func TestCloseAndContext(t *testing.T) {
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := Start(ctx, Config{Keys: []*Key{key(t, ES256)}, RotateEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s.SetJWKSOverride(Override{Delay: time.Hour})
	c := httpClient(t, nil)
	done := make(chan error, 1)
	go func() {
		_, err := send(t, c, http.MethodGet, s.JWKSURL(), "", nil)
		done <- err
	}()
	waitFor(t, func() bool { return s.Stats().JWKSFetches == 1 })
	cancel() // the Start context ends: the Server closes
	if err := <-done; err == nil {
		t.Fatal("delayed fetch succeeded")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Requests after Close are refused.
	if _, err := send(t, c, http.MethodGet, s.JWKSURL(), "", nil); err == nil {
		t.Fatal("request after Close succeeded")
	}
	c.CloseIdleConnections()
	waitFor(t, func() bool { return runtime.NumGoroutine() <= base })
}

func TestAdmitAfterClose(t *testing.T) {
	s := start(t, Config{})
	_ = s.Close()
	defer func() {
		if r := recover(); r != http.ErrAbortHandler { //nolint:errorlint // the panic value is compared, not an error chain
			t.Fatalf("recovered %v", r)
		}
	}()
	s.admit(http.NotFoundHandler()).ServeHTTP(nil, nil)
}

func TestStartErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Config
		want string
	}{
		{"rotate", Config{Keys: []*Key{{}}, RotateEvery: -1}, "negative RotateEvery"},
		// 06 req 70: expires_in is a positive integer of seconds.
		{"sub-second ttl", Config{TokenTTL: 500 * time.Millisecond}, "TokenTTL 500ms"},
		{"fractional ttl", Config{TokenTTL: 1500 * time.Millisecond}, "whole number of seconds"},
		{"negative ttl", Config{TokenTTL: -time.Second}, "TokenTTL -1s"},
		{"nil key", Config{Keys: []*Key{nil}}, "Config.Keys"},
		{"bare key", Config{Keys: []*Key{{ID: "x", Alg: RS256}}}, "Config.Keys"},
		{"listen", Config{Keys: []*Key{{}}, Listen: "256.0.0.1:0"}, "listen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "listen" {
				tc.c.Keys = []*Key{key(t, ES256)}
			}
			s, err := Start(context.Background(), tc.c)
			if err == nil {
				_ = s.Close()
				t.Fatal("Start succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
