// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/serviceaccount"
)

// The fixed addresses of Google's keyless token sources
// (docs/architecture/08-security-and-identity.md "Upstream
// authentication"): the metadata server, which Compute Engine and the GKE
// metadata server answer on its link-local IPv4 address over cleartext
// HTTP, and the IAM Service Account Credentials API, where token-exchange
// impersonates config.serviceAccount.
const (
	// MetadataServerURL is the metadata server's address, which only a
	// RURALZ_FETCH_ALLOW entry holding 169.254.169.254 lets a Node reach.
	MetadataServerURL = "http://169.254.169.254"
	// IAMCredentialsURL is the IAM Service Account Credentials API.
	IAMCredentialsURL = "https://iamcredentials.googleapis.com" //nolint:gosec // G101: a public API address, not a credential
)

// Paths, headers and protocol values of the Google sources.
const (
	metadataTokenPath    = "/computeMetadata/v1/instance/service-accounts/default/token"
	metadataIdentityPath = "/computeMetadata/v1/instance/service-accounts/default/identity"
	// headerMetadataFlavor marks a request as meant for the metadata
	// server, which refuses one without it.
	headerMetadataFlavor = "Metadata-Flavor"
	metadataFlavorGoogle = "Google"

	grantTypeJWTBearer     = "urn:ietf:params:oauth:grant-type:jwt-bearer"     //nolint:gosec // G101: an RFC 7523 grant type URI, not a credential
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // G101: an RFC 8693 grant type URI, not a credential
	// tokenTypeAccessToken is the RFC 8693 type of the token an exchange
	// requests and must issue.
	tokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token" //nolint:gosec // G101: an RFC 8693 token type URI, not a credential
	// scopeCloudPlatform is the scope of the federated token an
	// impersonating exchange requests, which generateAccessToken and
	// generateIdToken accept; the impersonated token gets config.scopes.
	scopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"

	// assertionLifetime is the jwt-bearer assertion's exp after its iat:
	// one hour, the most Google accepts.
	assertionLifetime = time.Hour
	algRS256          = "RS256"
	typJWT            = "JWT"
)

// Failures of the Google sources; fixed messages that never echo a
// response, a key or a token.
var (
	errBadKey       = errors.New("config.serviceAccountKey does not hold a usable service-account key")
	errNoIDToken    = errors.New("token response has no ID token")
	errIDTokenChars = errors.New("token response ID token is not visible ASCII")
	errIssuedType   = errors.New("token exchange issued a token that is not an access token")
	errSign         = errors.New("signing the jwt-bearer assertion failed")
	errNoSubject    = errors.New("config.subjectToken resolved to an empty token")
)

// assertionHeader is the JOSE header of a jwt-bearer assertion.
type assertionHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid,omitempty"`
	Typ string `json:"typ"`
}

// assertionClaims are the claims of a jwt-bearer assertion: Google's
// service-account claim set, scope for an access token, or sub and
// target_audience for an ID token.
type assertionClaims struct {
	Iss            string `json:"iss"`
	Sub            string `json:"sub,omitempty"`
	Scope          string `json:"scope,omitempty"`
	TargetAudience string `json:"target_audience,omitempty"`
	Aud            string `json:"aud"`
	Iat            int64  `json:"iat"`
	Exp            int64  `json:"exp"`
}

// assertion returns the compact RS256 JWS the jwt-bearer grant sends
// (RFC 7523 section 2.1): issued by the key's service account at iat, for
// the tokenUrl as audience, expiring assertionLifetime later, with the
// key's ID as kid; it asks for scope, or with audience for an ID token
// for it.
func assertion(key *serviceaccount.Key, tokenURL, scope, audience string, iat time.Time) (string, error) {
	h, err := json.Marshal(assertionHeader{Alg: algRS256, Kid: key.KeyID, Typ: typJWT})
	if err != nil {
		return "", errSign
	}
	c := assertionClaims{Iss: key.Email, Aud: tokenURL, Iat: iat.Unix(), Exp: iat.Add(assertionLifetime).Unix()}
	if audience != "" {
		c.Sub, c.TargetAudience = key.Email, audience
	} else {
		c.Scope = scope
	}
	cb, err := json.Marshal(c)
	if err != nil {
		return "", errSign
	}
	enc := base64.RawURLEncoding
	input := enc.EncodeToString(h) + "." + enc.EncodeToString(cb)
	sum := sha256.Sum256([]byte(input))
	// RSASSA-PKCS1-v1_5 signing is deterministic; it reads no randomness.
	sig, err := rsa.SignPKCS1v15(nil, key.Private, crypto.SHA256, sum[:])
	if err != nil {
		return "", errSign
	}
	return input + "." + enc.EncodeToString(sig), nil
}

// jwtBearer runs the JWT bearer grant (RFC 7523) with the service-account
// key of h's secret table: an assertion signed at start, sent to the
// tokenUrl, whose answer is an access token, or with config.audience the
// ID token of its id_token member.
func (e *entry) jwtBearer(ctx context.Context, h *holder, start time.Time) (*token, error) {
	raw, err := e.credential(h)
	if err != nil {
		return nil, err
	}
	key, err := serviceaccount.Parse(raw)
	clear(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBadKey, err)
	}
	a, err := assertion(key, e.key.endpoint, e.key.scope, e.key.audience, start)
	if err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {grantTypeJWTBearer}, "assertion": {a}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.key.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	req.Header.Set("Content-Type", contentTypeForm)
	req.Header.Set("Accept", mediaJSON)
	body, err := roundTrip(e.client, req)
	if err != nil {
		return nil, err
	}
	if e.key.audience != "" {
		return memberToken(body, "id_token", errNoIDToken, start)
	}
	return parseToken(body, start)
}

// metadataServer asks the metadata server for a token of the service
// account the workload runs as: an access token, for config.scopes when
// set (comma-separated, as the server takes them), or with config.audience
// the ID token its identity endpoint answers with as the whole body.
func (e *entry) metadataServer(ctx context.Context, start time.Time) (*token, error) {
	u := e.reg.metadataURL + metadataTokenPath
	q := url.Values{}
	if e.key.audience != "" {
		u = e.reg.metadataURL + metadataIdentityPath
		q.Set("audience", e.key.audience)
	} else if e.key.scope != "" {
		q.Set("scopes", strings.ReplaceAll(e.key.scope, " ", ","))
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	req.Header.Set(headerMetadataFlavor, metadataFlavorGoogle)
	body, err := roundTrip(e.client, req)
	if err != nil {
		return nil, err
	}
	if e.key.audience != "" {
		return idToken(string(bytes.TrimSpace(body)), start)
	}
	return parseToken(body, start)
}

// tokenExchange runs the token exchange (RFC 8693) of the subject token
// in h's secret table at the tokenUrl, Google's Security Token Service,
// for an access token whose audience is config.workloadIdentityProvider.
// Without config.serviceAccount that federated token is the token, for
// config.scopes; with it, it asks for the cloud-platform scope and
// impersonates the account (impersonate).
func (e *entry) tokenExchange(ctx context.Context, h *holder, start time.Time) (*token, error) {
	raw, err := e.credential(h)
	if err != nil {
		return nil, err
	}
	subject := string(bytes.TrimSpace(raw))
	clear(raw)
	if subject == "" {
		return nil, errNoSubject
	}
	scope := e.key.scope
	if e.key.serviceAccount != "" {
		scope = scopeCloudPlatform
	}
	form := url.Values{
		"grant_type":           {grantTypeTokenExchange},
		"audience":             {e.key.provider},
		"requested_token_type": {tokenTypeAccessToken},
		"subject_token":        {subject},
		"subject_token_type":   {e.key.subjectTokenType},
	}
	if scope != "" {
		form.Set("scope", scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.key.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	req.Header.Set("Content-Type", contentTypeForm)
	req.Header.Set("Accept", mediaJSON)
	body, err := roundTrip(e.client, req)
	if err != nil {
		return nil, err
	}
	var r tokenResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, errMalformed
	}
	if r.IssuedTokenType != "" && r.IssuedTokenType != tokenTypeAccessToken {
		return nil, errIssuedType
	}
	federated, err := accessToken(r, start)
	if err != nil || e.key.serviceAccount == "" {
		return federated, err
	}
	return e.impersonate(ctx, federated, start)
}

// generateAccessTokenRequest and generateIDTokenRequest are the bodies of
// the IAM Service Account Credentials calls.
type (
	generateAccessTokenRequest struct {
		Scope []string `json:"scope"`
	}
	generateIDTokenRequest struct {
		Audience     string `json:"audience"`
		IncludeEmail bool   `json:"includeEmail"`
	}
)

// impersonate asks the IAM Service Account Credentials API, with the
// federated token, for config.serviceAccount's access token for
// config.scopes (generateAccessToken), or with config.audience for its ID
// token, naming the account's email (generateIdToken).
func (e *entry) impersonate(ctx context.Context, federated *token, start time.Time) (*token, error) {
	name := e.reg.iamURL + "/v1/projects/-/serviceAccounts/" + url.PathEscape(e.key.serviceAccount)
	var (
		u, member string
		missing   error
		payload   any
	)
	if e.key.audience != "" {
		u, member, missing = name+":generateIdToken", "token", errNoIDToken
		payload = generateIDTokenRequest{Audience: e.key.audience, IncludeEmail: true}
	} else {
		u, member, missing = name+":generateAccessToken", "accessToken", errNoAccessToken
		payload = generateAccessTokenRequest{Scope: strings.Fields(e.key.scope)}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	req.Header.Set("Content-Type", mediaJSON)
	req.Header.Set("Accept", mediaJSON)
	req.Header.Set(headerAuthorization, federated.header)
	body, err := roundTrip(e.iam, req)
	if err != nil {
		return nil, err
	}
	return memberToken(body, member, missing, start)
}

// memberToken returns the token a JSON object response carries as the
// string member name, missing when it has none, without a lifetime of its
// own: ID token responses and impersonated access tokens state no
// expires_in, so the token lives DefaultLifetime.
func memberToken(body []byte, member string, missing error, start time.Time) (*token, error) {
	var r map[string]json.RawMessage
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, errMalformed
	}
	var v string
	if raw, ok := r[member]; !ok || json.Unmarshal(raw, &v) != nil || v == "" {
		return nil, missing
	}
	return idToken(v, start)
}

// idToken returns a token of value v, which a response or the metadata
// server's identity endpoint gave without a lifetime, living
// DefaultLifetime from start.
func idToken(v string, start time.Time) (*token, error) {
	switch {
	case v == "":
		return nil, errNoIDToken
	case !visibleASCII(v):
		return nil, errIDTokenChars
	}
	return &token{header: bearerPrefix + v, obtained: start, expires: start.Add(DefaultLifetime)}, nil
}
