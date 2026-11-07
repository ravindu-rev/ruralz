// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Google Cloud authentication (docs/architecture/08-security-and-identity.md
// "Upstream authentication"): the jwt-bearer grant with a service-account
// key, the metadata server and the token exchange with impersonation,
// against in-process mock servers.

// The test service account and its key.
const (
	testAccount  = "orders@shop.iam.gserviceaccount.com"
	testKeyID    = "key-1"
	testSubject  = "eyJhbGciOiJSUzI1NiJ9.subject.signature"
	testAudience = "https://orders.shop.example"
	testScope    = "https://www.googleapis.com/auth/devstorage.read_only"
)

// testKey returns the RSA key of the test service account, one per test
// binary.
func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	keyOnce.Do(func() { keyVal, keyErr = rsa.GenerateKey(rand.Reader, 2048) })
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	return keyVal
}

// The test key, generated once.
var (
	keyOnce sync.Once
	keyVal  *rsa.PrivateKey
	keyErr  error
)

// serviceAccountJSON returns the service-account JSON key of key.
func serviceAccountJSON(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]string{ //nolint:gosec // G101: a test key built from a generated private key, not a credential
		"type": "service_account", "project_id": "shop", "private_key_id": testKeyID,
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": testAccount, "client_id": "1234567890", "token_uri": "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// mdRequest is one request the metadata mock received.
type mdRequest struct {
	method, path, rawQuery string
	header                 http.Header
}

// mdServer is a cleartext metadata server mock on loopback.
type mdServer struct {
	srv     *httptest.Server
	respond responder
	mu      sync.Mutex
	reqs    []mdRequest
}

func newMDServer(t *testing.T, r responder) *mdServer {
	t.Helper()
	m := &mdServer{respond: r}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.reqs = append(m.reqs, mdRequest{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone()})
		n := len(m.reqs)
		m.mu.Unlock()
		m.respond(n, w, r)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// requests returns the requests received.
func (m *mdServer) requests() []mdRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mdRequest(nil), m.reqs...)
}

// googleHarness is a harness whose Registry also trusts an IAM Service
// Account Credentials mock and reaches a metadata server mock, both on
// loopback, which RURALZ_FETCH_ALLOW admits.
type googleHarness struct {
	*harness
	iam *tokenServer
	md  *mdServer
}

// newGoogleHarness returns a running harness: the token endpoint (OAuth2
// token or Security Token Service) answering tokens, the IAM mock iam and
// the metadata mock md.
func newGoogleHarness(t *testing.T, tokens, iam, md responder) *googleHarness {
	t.Helper()
	g := &googleHarness{harness: &harness{
		t:        t,
		clk:      clocktest.New(testStart()),
		ts:       newTokenServer(t, tokens),
		secrets:  newSecrets(testSecret),
		logs:     &syncBuffer{},
		fetchSig: make(chan struct{}, 1),
	}}
	g.iam = newTokenServer(t, iam)
	g.md = newMDServer(t, md)
	guard, err := egress.ParseAllow("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(g.ts.srv.Certificate())
	pool.AddCert(g.iam.srv.Certificate())
	g.reg = NewRegistry(Options{
		Guard: guard, TLS: tlsconf.Client(pool), Clock: g.clk,
		Logger: slog.New(slog.NewJSONHandler(g.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	g.reg.metadataURL, g.reg.iamURL = g.md.srv.URL, g.iam.srv.URL
	g.reg.fetchHook = func() {
		g.fetched.Add(1)
		select {
		case g.fetchSig <- struct{}{}:
		default:
		}
	}
	g.secrets.set(secret.RefOf(keyRef()), serviceAccountJSON(t, testKey(t)))
	g.secrets.set(secret.RefOf(subjectRef()), testSubject+"\n")
	t.Cleanup(func() { _ = g.stop() })
	g.start()
	return g
}

// answer answers every request with status and body.
func answer(status int, body string) responder {
	return func(_ int, w http.ResponseWriter, _ *http.Request) { writeJSON(w, status, body) }
}

// none fails the test on any request.
func none(t *testing.T) responder {
	return func(_ int, w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusTeapot)
	}
}

// decodeAssertion splits a compact JWS, verifies its RS256 signature with
// the test key and decodes header and claims.
func decodeAssertion(t *testing.T, jws string) (header, claims map[string]any) {
	t.Helper()
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		t.Fatalf("assertion %q is not a compact JWS", jws)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey(t).PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("assertion signature: %v", err)
	}
	for i, out := range []*map[string]any{&header, &claims} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
	}
	return header, claims
}

// wantMembers fails unless m holds exactly want.
func wantMembers(t *testing.T, what string, m, want map[string]any) {
	t.Helper()
	got, _ := json.Marshal(m)
	w, _ := json.Marshal(want)
	if string(got) != string(w) {
		t.Errorf("%s = %s, want %s", what, got, w)
	}
}

// TestJWTBearerAssertion checks the jwt-bearer grant (RFC 7523): one form
// POST to the tokenUrl without client authentication, whose assertion is
// an RS256 JWS signed with the service-account key, naming its key ID, for
// the account as issuer, the tokenUrl as audience and the scopes, issued
// at the clock's time and expiring an hour later; the answer is the access
// token sent.
func TestJWTBearerAssertion(t *testing.T) {
	g := newGoogleHarness(t, issue(3600), none(t), none(t))
	cfg := jwtConfig(g.ts.url())
	cfg.Scopes = []string{testScope, "https://www.googleapis.com/auth/pubsub"}
	b := g.build("gcp-jwt", cfg)
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	req := g.ts.request(t, 1)
	if req.method != http.MethodPost || req.form.Get("grant_type") != grantTypeJWTBearer || req.basic {
		t.Errorf("token request = %s grant %q basic %v, want a POST of the jwt-bearer grant without client authentication", req.method, req.form.Get("grant_type"), req.basic)
	}
	if ct := req.header.Get("Content-Type"); ct != contentTypeForm {
		t.Errorf("Content-Type = %q", ct)
	}
	header, claims := decodeAssertion(t, req.form.Get("assertion"))
	wantMembers(t, "assertion header", header, map[string]any{"alg": "RS256", "kid": testKeyID, "typ": "JWT"})
	iat := float64(testStart().Unix())
	wantMembers(t, "assertion claims", claims, map[string]any{
		"iss": testAccount, "scope": testScope + " https://www.googleapis.com/auth/pubsub",
		"aud": g.ts.url(), "iat": iat, "exp": iat + 3600,
	})
}

// TestJWTBearerIDToken checks that config.audience asks for an ID token:
// the assertion carries the account as subject and target_audience, no
// scope, and the id_token answered is sent; it lives the default lifetime,
// so it is refreshed at two thirds of 5 minutes.
func TestJWTBearerIDToken(t *testing.T) {
	g := newGoogleHarness(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"id_token":"id-`+strconv.Itoa(n)+`"}`)
	}, none(t), none(t))
	cfg := jwtConfig(g.ts.url())
	cfg.Scopes, cfg.Audience = nil, testAudience
	b := g.build("gcp-id", cfg)
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "id-1")
	_, claims := decodeAssertion(t, g.ts.request(t, 1).form.Get("assertion"))
	iat := float64(testStart().Unix())
	wantMembers(t, "assertion claims", claims, map[string]any{
		"iss": testAccount, "sub": testAccount, "target_audience": testAudience,
		"aud": g.ts.url(), "iat": iat, "exp": iat + 3600,
	})
	if at, _ := entryState(b.e); !at.Equal(testStart().Add(200 * time.Second)) {
		t.Fatalf("refresh at %v, want two thirds of the 5-minute default lifetime", at.Sub(testStart()))
	}
	g.advanceArmed(200 * time.Second)
	g.waitFetches(2)
	r, auth = handle(t.Context(), b)
	wantBearer(t, r, auth, "id-2")
	_, claims = decodeAssertion(t, g.ts.request(t, 2).form.Get("assertion"))
	if claims["iat"] != iat+200 || claims["exp"] != iat+200+3600 {
		t.Errorf("refresh assertion iat %v exp %v, want a new assertion signed at the clock's time", claims["iat"], claims["exp"])
	}
}

// TestJWTBearerKeyFailures checks that a key that does not parse fails the
// fetch with RZ-AUTH-020 and nothing is sent, and that a token endpoint's
// refusal names its RFC 6749 error code.
func TestJWTBearerKeyFailures(t *testing.T) {
	g := newGoogleHarness(t, fail(http.StatusBadRequest, "invalid_grant"), none(t), none(t))
	b := g.build("gcp-jwt", jwtConfig(g.ts.url()))
	r, _ := handle(t.Context(), b)
	var se *statusError
	if wantUndecided(t, r, nil); !errors.As(r.Err, &se) || se.code != "invalid_grant" {
		t.Fatalf("refused grant = %v, want the endpoint's invalid_grant", r.Err)
	}
	g.secrets.set(secret.RefOf(keyRef()), `{"type":"service_account"}`)
	g.clk.Advance(MinFetchInterval)
	r, _ = handle(t.Context(), b)
	wantUndecided(t, r, errBadKey)
	if n := g.ts.count(); n != 1 {
		t.Errorf("token endpoint got %d requests, want none for the bad key", n)
	}
}

// TestMetadataServerAccessToken checks the metadata-server source: a GET
// of the token endpoint of the default service account with
// Metadata-Flavor: Google and the scopes comma-separated, through the
// egress guard over cleartext to the one origin it is bound to; the
// answer's access token is sent and refreshed at two thirds of expires_in.
func TestMetadataServerAccessToken(t *testing.T) {
	g := newGoogleHarness(t, none(t), none(t), issue(3600))
	cfg := metadataConfig()
	cfg.Scopes = []string{testScope, "https://www.googleapis.com/auth/pubsub"}
	b := g.build("gcp-md", cfg)
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	reqs := g.md.requests()
	if len(reqs) != 1 {
		t.Fatalf("metadata server got %d requests", len(reqs))
	}
	m := reqs[0]
	q, err := url.ParseQuery(m.rawQuery)
	if err != nil || m.method != http.MethodGet || m.path != metadataTokenPath || len(q) != 1 || q.Get("scopes") != testScope+",https://www.googleapis.com/auth/pubsub" {
		t.Errorf("metadata request = %s %s?%s, want a GET of the token path with the scopes comma-separated", m.method, m.path, m.rawQuery)
	}
	if m.header.Get("Metadata-Flavor") != "Google" || m.header.Get("Authorization") != "" {
		t.Errorf("metadata request header = %v, want Metadata-Flavor: Google and no credential", m.header)
	}
	if at, _ := entryState(b.e); !at.Equal(testStart().Add(2400 * time.Second)) {
		t.Errorf("refresh at %v, want two thirds of an hour", at.Sub(testStart()))
	}
}

// TestMetadataServerIDToken checks that config.audience reads an ID token
// from the identity endpoint, which answers with the token as the whole
// body.
func TestMetadataServerIDToken(t *testing.T) {
	g := newGoogleHarness(t, none(t), none(t), func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "eyJhbGciOiJSUzI1NiJ9.identity.sig\n")
	})
	cfg := metadataConfig()
	cfg.Audience = testAudience
	b := g.build("gcp-md-id", cfg)
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "eyJhbGciOiJSUzI1NiJ9.identity.sig")
	m := g.md.requests()[0]
	q, err := url.ParseQuery(m.rawQuery)
	if err != nil || m.path != metadataIdentityPath || len(q) != 1 || q.Get("audience") != testAudience || m.header.Get("Metadata-Flavor") != "Google" {
		t.Errorf("identity request = %s?%s with %v, want the identity path for the audience", m.path, m.rawQuery, m.header)
	}
	if at, _ := entryState(b.e); !at.Equal(testStart().Add(200 * time.Second)) {
		t.Errorf("refresh at %v, want two thirds of the default lifetime", at.Sub(testStart()))
	}
}

// TestMetadataServerNeedsTheAllowList checks that the metadata server is
// reached only through the egress guard: without a RURALZ_FETCH_ALLOW
// entry, a fetch at its fixed link-local address fails with RZ-AUTH-020
// before any connection, and so does one of the mock on loopback, which
// the other tests reach only because their allow list holds 127.0.0.1.
func TestMetadataServerNeedsTheAllowList(t *testing.T) {
	if MetadataServerURL != "http://169.254.169.254" {
		t.Fatalf("MetadataServerURL = %q", MetadataServerURL)
	}
	h := newHarness(t, none(t))
	h.reg.guard = nil
	h.start()
	b := h.build("gcp-md", metadataConfig())
	r, _ := handle(t.Context(), b)
	wantUndecided(t, r, egress.ErrDenied)

	md := newMDServer(t, none(t))
	h2 := newHarness(t, none(t))
	h2.reg.guard, h2.reg.metadataURL = nil, md.srv.URL
	h2.start()
	b2 := h2.build("gcp-md", metadataConfig())
	r, _ = handle(t.Context(), b2)
	wantUndecided(t, r, egress.ErrDenied)
	if n := len(md.requests()); n != 0 {
		t.Errorf("metadata mock got %d requests without an allow entry", n)
	}
}

// TestTokenExchange checks the token-exchange source (RFC 8693): one form
// POST of the subject token, trimmed of the newline a token file ends with,
// to the Security Token Service at the tokenUrl, without client
// authentication, for an access token whose audience is the workload
// identity provider and whose scope is config.scopes; the federated token
// it issues is sent.
func TestTokenExchange(t *testing.T) {
	g := newGoogleHarness(t, answer(http.StatusOK, `{"access_token":"fed-1","issued_token_type":"`+tokenTypeAccessToken+`","token_type":"Bearer","expires_in":3600}`), none(t), none(t))
	cfg := exchangeConfig(g.ts.url())
	cfg.Scopes = []string{testScope}
	b := g.build("gcp-wif", cfg)
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "fed-1")
	req := g.ts.request(t, 1)
	want := map[string]string{
		"grant_type": grantTypeTokenExchange, "audience": testProvider, "scope": testScope,
		"requested_token_type": tokenTypeAccessToken, "subject_token": testSubject, "subject_token_type": DefaultSubjectTokenType,
	}
	for k, v := range want {
		if got := req.form.Get(k); got != v {
			t.Errorf("exchange %s = %q, want %q", k, got, v)
		}
	}
	if len(req.form) != len(want) || req.basic || req.header.Get("Authorization") != "" {
		t.Errorf("exchange form %v basic %v, want exactly the RFC 8693 parameters without client authentication", req.form, req.basic)
	}
}

// TestTokenExchangeRefusals checks the exchange's failures: an issued
// token that is not an access token, an empty subject token and a refusal
// of the service are RZ-AUTH-020.
func TestTokenExchangeRefusals(t *testing.T) {
	g := newGoogleHarness(t, answer(http.StatusOK, `{"access_token":"x","issued_token_type":"urn:ietf:params:oauth:token-type:id_token","token_type":"Bearer"}`), none(t), none(t))
	b := g.build("gcp-wif", exchangeConfig(g.ts.url()))
	r, _ := handle(t.Context(), b)
	wantUndecided(t, r, errIssuedType)
	g.secrets.set(secret.RefOf(subjectRef()), " \n")
	g.clk.Advance(MinFetchInterval)
	r, _ = handle(t.Context(), b)
	wantUndecided(t, r, errNoSubject)
	g.secrets.set(secret.RefOf(subjectRef()), testSubject)
	g.ts.setResponder(fail(http.StatusBadRequest, "invalid_grant"))
	g.clk.Advance(MinFetchInterval)
	r, _ = handle(t.Context(), b)
	var se *statusError
	if wantUndecided(t, r, nil); !errors.As(r.Err, &se) || se.code != "invalid_grant" {
		t.Errorf("refused exchange = %v, want invalid_grant", r.Err)
	}
}

// iamRequest decodes the n-th request of the IAM mock: its path,
// Authorization and JSON body.
func iamRequest(t *testing.T, g *googleHarness, n int) (path, auth string, body map[string]any) {
	t.Helper()
	req := g.iam.request(t, n)
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatalf("IAM request body %q: %v", req.body, err)
	}
	if ct := req.header.Get("Content-Type"); ct != mediaJSON || req.method != http.MethodPost {
		t.Errorf("IAM request = %s with Content-Type %q, want a JSON POST", req.method, ct)
	}
	return req.path, req.header.Get("Authorization"), body
}

// TestTokenExchangeImpersonation checks impersonation: the exchange asks
// for the cloud-platform scope, then the federated token calls the IAM
// Service Account Credentials API's generateAccessToken for the account
// with config.scopes; the impersonated token is sent and lives the default
// lifetime.
func TestTokenExchangeImpersonation(t *testing.T) {
	g := newGoogleHarness(t, answer(http.StatusOK, `{"access_token":"fed","issued_token_type":"`+tokenTypeAccessToken+`","token_type":"Bearer","expires_in":3600}`),
		answer(http.StatusOK, `{"accessToken":"impersonated","expireTime":"2026-03-04T06:06:07Z"}`), none(t))
	cfg := exchangeConfig(g.ts.url())
	cfg.ServiceAccount, cfg.Scopes = testAccount, []string{testScope, "https://www.googleapis.com/auth/pubsub"}
	b := g.build("gcp-wif-sa", cfg)
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "impersonated")
	if s := g.ts.request(t, 1).form.Get("scope"); s != scopeCloudPlatform {
		t.Errorf("exchange scope = %q, want %q", s, scopeCloudPlatform)
	}
	path, bearer, body := iamRequest(t, g, 1)
	if path != "/v1/projects/-/serviceAccounts/"+testAccount+":generateAccessToken" || bearer != "Bearer fed" {
		t.Errorf("IAM request = %s with %q", path, bearer)
	}
	wantMembers(t, "generateAccessToken body", body, map[string]any{"scope": []any{testScope, "https://www.googleapis.com/auth/pubsub"}})
	if at, _ := entryState(b.e); !at.Equal(testStart().Add(200 * time.Second)) {
		t.Errorf("refresh at %v, want two thirds of the default lifetime", at.Sub(testStart()))
	}
}

// TestTokenExchangeImpersonatedIDToken checks that config.audience with
// config.serviceAccount asks generateIdToken for an ID token naming the
// account's email, which is sent.
func TestTokenExchangeImpersonatedIDToken(t *testing.T) {
	g := newGoogleHarness(t, answer(http.StatusOK, `{"access_token":"fed","issued_token_type":"`+tokenTypeAccessToken+`","token_type":"Bearer","expires_in":3600}`),
		answer(http.StatusOK, `{"token":"id-token"}`), none(t))
	cfg := exchangeConfig(g.ts.url())
	cfg.ServiceAccount, cfg.Audience = testAccount, testAudience
	b := g.build("gcp-wif-id", cfg)
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "id-token")
	path, bearer, body := iamRequest(t, g, 1)
	if path != "/v1/projects/-/serviceAccounts/"+testAccount+":generateIdToken" || bearer != "Bearer fed" {
		t.Errorf("IAM request = %s with %q", path, bearer)
	}
	wantMembers(t, "generateIdToken body", body, map[string]any{"audience": testAudience, "includeEmail": true})
}

// TestImpersonationRefusals checks that the IAM API's refusal, and an
// answer without the token, fail the fetch with RZ-AUTH-020.
func TestImpersonationRefusals(t *testing.T) {
	g := newGoogleHarness(t, answer(http.StatusOK, `{"access_token":"fed","token_type":"Bearer","expires_in":3600}`),
		answer(http.StatusForbidden, `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`), none(t))
	cfg := exchangeConfig(g.ts.url())
	cfg.ServiceAccount = testAccount
	b := g.build("gcp-wif-sa", cfg)
	r, _ := handle(t.Context(), b)
	var se *statusError
	if wantUndecided(t, r, nil); !errors.As(r.Err, &se) || se.status != http.StatusForbidden || se.code != "" {
		t.Errorf("refused impersonation = %v, want status 403 without an error code", r.Err)
	}
	g.iam.setResponder(answer(http.StatusOK, `{"expireTime":"x"}`))
	g.clk.Advance(MinFetchInterval)
	r, _ = handle(t.Context(), b)
	wantUndecided(t, r, errNoAccessToken)
}

// TestGoogleSourcesNeverLeak checks that neither the key, the subject
// token nor a token reaches a log or an error.
func TestGoogleSourcesNeverLeak(t *testing.T) {
	g := newGoogleHarness(t, fail(http.StatusUnauthorized, "invalid_client"), none(t), none(t))
	for _, cfg := range []*v1alpha1.AuthUpstreamOAuth2Config{jwtConfig(g.ts.url()), exchangeConfig(g.ts.url())} {
		b := g.build("gcp-"+string(*cfg.GrantType), cfg)
		r, _ := handle(t.Context(), b)
		out := r.Err.Error() + string(g.logs.Bytes())
		for _, s := range []string{testSubject, "PRIVATE KEY", "MII"} {
			if strings.Contains(out, s) {
				t.Errorf("%s: output holds %q: %s", *cfg.GrantType, s, out)
			}
		}
	}
}
