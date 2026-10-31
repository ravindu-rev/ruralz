// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/secret"
)

// MaxResponseBytes caps a token response body after content decoding at
// 64 KiB; a larger one fails the fetch.
const MaxResponseBytes = 64 << 10

// DefaultLifetime is the lifetime of a token whose response carries no
// expires_in: 5 minutes. RFC 6749 leaves it to the server, so
// it is short: an unknown lifetime is refreshed after 3 m 20 s, which
// bounds how long an expired token could be sent.
const DefaultLifetime = 5 * time.Minute

// maxLifetimeSeconds is the largest expires_in a time.Duration holds;
// larger values are clamped to it.
const maxLifetimeSeconds = math.MaxInt64 / int64(time.Second)

// Protocol values.
const (
	grantClientCredentials = "client_credentials"
	headerAuthorization    = "Authorization"
	bearerPrefix           = "Bearer "
	typeBearer             = "bearer"
	contentTypeForm        = "application/x-www-form-urlencoded"
	mediaJSON              = "application/json"
)

// Fetch failures. Their messages are fixed: a token response is never
// echoed, since it may carry the token, and the secret is never part of
// one.
var (
	errNoSecret      = errors.New("config.clientSecret has no resolved value")
	errMalformed     = errors.New("token response is not a JSON object")
	errNoAccessToken = errors.New("token response has no access_token")
	errTokenChars    = errors.New("token response access_token is not visible ASCII")
	errTokenType     = errors.New("token response token_type is not Bearer")
	errExpiresIn     = errors.New("token response expires_in is not a positive integer")
	errExpired       = errors.New("token expired before its response arrived")
	errPanic         = errors.New("token fetch panicked")
)

// token is an obtained access token. It never prints its value.
type token struct {
	// header is the Authorization field value, "Bearer <access_token>".
	header string
	// obtained is when the token request was sent; expires is obtained
	// plus the lifetime.
	obtained, expires time.Time
}

// String returns secret.Redacted, so a token never reaches output.
func (token) String() string { return secret.Redacted }

// GoString returns secret.Redacted.
func (token) GoString() string { return secret.Redacted }

// LogValue returns secret.Redacted for log/slog.
func (token) LogValue() slog.Value { return slog.StringValue(secret.Redacted) }

// statusError is a token endpoint answer other than 200.
type statusError struct {
	status int
	// code is the RFC 6749 section 5.2 error code when it is one of
	// knownErrorCodes; "" otherwise.
	code string
}

func (e *statusError) Error() string {
	msg := "token endpoint answered status " + strconv.Itoa(e.status)
	if e.code != "" {
		msg += " with error " + e.code
	}
	return msg
}

// knownErrorCodes returns the RFC 6749 error codes a fetch failure may
// name (section 5.2, and server_error and temporarily_unavailable of
// section 4.1.2.1 that servers also answer); only these are repeated, so
// no value a server chose reaches a log.
func knownErrorCodes() []string {
	return []string{
		"invalid_request", "invalid_client", "invalid_grant", "unauthorized_client",
		"unsupported_grant_type", "invalid_scope", "server_error", "temporarily_unavailable",
	}
}

// fetch runs one client credentials grant for e with the secret table of
// h; start is the Registry clock's time of sending, the start of the
// token's lifetime.
func (e *entry) fetch(ctx context.Context, h *holder, start time.Time) (*token, error) {
	v, ok := h.secrets.Get(e.key.ref)
	if !ok {
		return nil, errNoSecret
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.key.endpoint, strings.NewReader(e.form))
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	req.Header.Set("Content-Type", contentTypeForm)
	req.Header.Set("Accept", mediaJSON)
	req.Header.Set(headerAuthorization, basicCredentials(e.key.clientID, v))
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{status: resp.StatusCode, code: errorCodeOf(body)}
	}
	return parseToken(body, start)
}

// basicCredentials returns the client_secret_basic Authorization value:
// clientId and the secret each form-encoded (RFC 6749 section 2.3.1),
// joined by a colon, in base64.
func basicCredentials(clientID string, v secret.Value) string {
	b := v.Reveal()
	defer clear(b)
	pair := url.QueryEscape(clientID) + ":" + url.QueryEscape(string(b))
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(pair))
}

// tokenResponse is the part of an RFC 6749 section 5.1 response the
// Filter uses.
type tokenResponse struct {
	AccessToken string          `json:"access_token"`
	TokenType   string          `json:"token_type"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
}

// parseToken decodes a 200 response body into a token whose lifetime
// starts at start.
func parseToken(body []byte, start time.Time) (*token, error) {
	var r tokenResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, errMalformed
	}
	switch {
	case r.AccessToken == "":
		return nil, errNoAccessToken
	case !visibleASCII(r.AccessToken):
		return nil, errTokenChars
	case r.TokenType != "" && !strings.EqualFold(r.TokenType, typeBearer):
		return nil, errTokenType
	}
	life, err := lifetimeOf(r.ExpiresIn)
	if err != nil {
		return nil, err
	}
	return &token{header: bearerPrefix + r.AccessToken, obtained: start, expires: start.Add(life)}, nil
}

// lifetimeOf returns the lifetime expires_in states: a positive integer
// as a JSON number or a decimal string, clamped to maxLifetimeSeconds;
// DefaultLifetime when absent or null.
func lifetimeOf(raw json.RawMessage) (time.Duration, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return DefaultLifetime, nil
	}
	if s[0] == '"' {
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, errExpiresIn
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if errors.Is(err, strconv.ErrRange) && n > 0 {
		// Above the int64 range ParseInt returns its maximum: clamp.
		err = nil
	}
	if err != nil || n <= 0 {
		return 0, errExpiresIn
	}
	return time.Duration(min(n, maxLifetimeSeconds)) * time.Second, nil
}

// visibleASCII reports a token made only of the characters 0x21 to 0x7E,
// which a header field value carries unchanged and which cannot split
// "Bearer <token>".
func visibleASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// errorCodeOf returns the error member of an RFC 6749 error response when
// it is a known code; "" for any other body.
func errorCodeOf(body []byte) string {
	var r struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &r) != nil || !slices.Contains(knownErrorCodes(), r.Error) {
		return ""
	}
	return r.Error
}
