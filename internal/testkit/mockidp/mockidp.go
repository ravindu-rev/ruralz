// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package mockidp is the mock identity provider of the test kit, for the
// auth.jwt, JWKS and auth.upstream-oauth2 tests, the process harness and
// the secret leak tests: one HTTP server, over
// TLS when a configuration is passed in (from testkit/pki) or cleartext
// for unit tests, serving
//
//   - a JWKS endpoint (JWKSURL) with RS256 and ES256 keys by default (PS256
//     and EdDSA on request), rotated by hand (Rotate), with a configurable
//     Cache-Control header;
//   - an OAuth2 client-credentials token endpoint (TokenURL, RFC 6749
//     section 4.4) that authenticates clients with client_secret_basic or
//     client_secret_post and issues signed JWT access tokens;
//   - JWT minting (Mint, MintWith, Key.Sign) on the standard library's
//     crypto only.
//
// Overrides script failures per endpoint (a status such as 429 with
// Retry-After, a replacement body, a delay). Every JWKS fetch and token
// request is recorded, bounded, with its answer and credentials: the
// token endpoint is a designated destination of the canary secret, so
// tests assert it arrives here and nowhere else.
//
// Goroutines: the http.Server's serve loop and its per-connection
// goroutines, owned by the Server; Close, or the end of the Start
// context, stops them and waits for every handler.
package mockidp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults and endpoint paths.
const (
	// JWKSPath is the JWKS endpoint's path.
	JWKSPath = "/.well-known/jwks.json"
	// TokenPath is the token endpoint's path.
	TokenPath = "/oauth2/token" //nolint:gosec // G101: a URL path, not a credential
	// DefaultCacheControl is the JWKS Cache-Control value.
	DefaultCacheControl = "public, max-age=3600"
	// DefaultTokenTTL is the access token lifetime (expires_in) and the
	// default lifetime of minted JWTs.
	DefaultTokenTTL = time.Hour
	// JWKSContentType is the JWKS response's media type (RFC 7517).
	JWKSContentType = "application/jwk-set+json"
	// maxRecorded bounds the recorded JWKS fetches and token requests
	// (the newest are kept).
	maxRecorded = 1024
	// maxTokenBody bounds a token request body.
	maxTokenBody = 64 << 10
	// readHeaderTimeout bounds reading request headers.
	readHeaderTimeout = 10 * time.Second
)

// Config configures a Server.
type Config struct {
	// Listen is the listen address; default "127.0.0.1:0".
	Listen string
	// TLS, when set, serves HTTPS with this configuration; nil serves
	// cleartext HTTP (auth.jwt and auth.upstream-oauth2 require https
	// URLs, RZ-CFG-037, so Node tests pass one).
	TLS *tls.Config
	// CacheControl is the JWKS Cache-Control value; default
	// DefaultCacheControl.
	CacheControl string
	// Clients maps client IDs to secrets for the token endpoint; when
	// empty, any client ID and secret is accepted (and recorded).
	Clients map[string]string
	// TokenTTL is the access token lifetime and the default lifetime of
	// minted JWTs; default DefaultTokenTTL. It must be a whole number of
	// seconds, at least one: expires_in, iat and exp are integer seconds,
	// so a shorter TTL would answer expires_in 0 and mint exp equal to
	// iat. Start rejects other values; a missing or zero expires_in is
	// scripted with SetTokenOverride instead.
	TokenTTL time.Duration
	// Now is the clock for token iat and exp (a fake clock's Now in
	// tests); default time.Now.
	Now func() time.Time
}

// Override scripts one endpoint's answers. The zero value answers
// normally. Delay is waited first; then, when Status or Body is set, the
// endpoint answers Status (default 200) with Header and Body instead of
// its normal answer; otherwise Header is added to the normal answer.
type Override struct {
	Status int
	Header http.Header
	Body   []byte
	Delay  time.Duration
}

func (o Override) canned() bool { return o.Status != 0 || o.Body != nil }

// Fetch is one recorded JWKS request.
type Fetch struct {
	// Status is the answer's status.
	Status int
}

// TokenRequest is one recorded token endpoint request. A body refused as
// too large (413) leaves the form fields empty, but the record still
// carries the Basic credentials.
type TokenRequest struct {
	// AuthMethod is "client_secret_basic", "client_secret_post" or ""
	// (none, or both: Basic credentials together with a client_secret
	// form field). A client_id form field alongside Basic credentials
	// only identifies the client (RFC 6749 section 3.2.1) and is not a
	// second method.
	AuthMethod string
	// ClientID and ClientSecret are the presented credentials, decoded:
	// the Basic user and password form-decoded (RFC 6749 section 2.3.1),
	// else the client_id and client_secret form fields. With both
	// methods they hold the Basic credentials. A Basic part that is not
	// validly form-encoded (a raw "%zz") is kept as sent and
	// MalformedCredentials is set.
	ClientID, ClientSecret string
	// MalformedCredentials reports Basic credentials that are not validly
	// form-encoded; the request is answered 401 invalid_client.
	MalformedCredentials bool
	GrantType, Scope     string
	// Status is the answer's status; AccessToken the issued token, ""
	// when none was issued.
	Status      int
	AccessToken string
}

// Stats counts the Server's work.
type Stats struct {
	// JWKSFetches and TokenRequests count requests to each endpoint.
	JWKSFetches, TokenRequests int64
}

// Server is a running mock identity provider.
type Server struct {
	cfg     Config
	addr    string
	url     string
	srv     *http.Server
	closing chan struct{}

	mu        sync.Mutex // guards the fields below
	keys      []*Key
	doc       []byte
	jwksOver  Override
	tokenOver Override
	fetches   []Fetch
	tokenReqs []TokenRequest
	closed    bool
	stopAfter func() bool

	clients   map[string]string // immutable after Start
	handlers  sync.WaitGroup
	serveDone chan struct{}
	closeOnce sync.Once

	jwksCount, tokenCount atomic.Int64
}

// Start listens on c.Listen and serves until Close is called or ctx ends.
func Start(ctx context.Context, c Config) (*Server, error) {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:0"
	}
	if c.TokenTTL == 0 {
		c.TokenTTL = DefaultTokenTTL
	}
	if c.TokenTTL < time.Second || c.TokenTTL%time.Second != 0 {
		return nil, fmt.Errorf("mockidp: TokenTTL %v is not a positive whole number of seconds", c.TokenTTL)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.CacheControl == "" {
		c.CacheControl = DefaultCacheControl
	}
	var keys []*Key
	for _, alg := range []Alg{RS256, ES256} {
		k, err := GenerateKey(alg)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	if c.TLS != nil {
		c.TLS = c.TLS.Clone()
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.Listen)
	if err != nil {
		return nil, fmt.Errorf("mockidp: listen %s: %w", c.Listen, err)
	}
	s := &Server{
		cfg:       c,
		addr:      ln.Addr().String(),
		closing:   make(chan struct{}),
		keys:      keys,
		clients:   maps.Clone(c.Clients),
		serveDone: make(chan struct{}),
	}
	s.url = "http://" + s.addr
	if c.TLS != nil {
		s.url = "https://" + s.addr
	}
	s.rebuildLocked()
	mux := http.NewServeMux()
	mux.HandleFunc(JWKSPath, s.serveJWKS)
	mux.HandleFunc(TokenPath, s.serveToken)
	s.srv = &http.Server{
		Handler:           s.admit(mux),
		TLSConfig:         c.TLS,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(slog.DiscardHandler, slog.LevelError),
		// Handlers outlive neither Close nor ctx (the AfterFunc below
		// closes the Server), so they carry ctx's values, not its end.
		BaseContext: func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	go func() {
		defer close(s.serveDone)
		if c.TLS != nil {
			_ = s.srv.ServeTLS(ln, "", "")
			return
		}
		_ = s.srv.Serve(ln)
	}()
	// The callback may run at once (ctx already done); Close then waits
	// for s.mu, so stopAfter is assigned before it is read.
	s.mu.Lock()
	s.stopAfter = context.AfterFunc(ctx, func() { _ = s.Close() })
	s.mu.Unlock()
	return s, nil
}

// admit counts handlers so Close can wait for them; requests arriving
// while closing are aborted.
func (s *Server) admit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			panic(http.ErrAbortHandler)
		}
		s.handlers.Add(1)
		s.mu.Unlock()
		defer s.handlers.Done()
		next.ServeHTTP(w, r)
	})
}

// Close stops the server and waits for its serve loop and every handler.
// It is idempotent; recorded requests stay readable.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		stopAfter := s.stopAfter
		s.mu.Unlock()
		close(s.closing)
		err = s.srv.Close()
		<-s.serveDone
		s.handlers.Wait()
		if stopAfter != nil {
			stopAfter()
		}
	})
	return err
}

// Issuer returns the iss of minted tokens, the base URL.
func (s *Server) Issuer() string { return s.url }

// JWKSURL returns the JWKS endpoint (an auth.jwt jwksUrl).
func (s *Server) JWKSURL() string { return s.url + JWKSPath }

// TokenURL returns the token endpoint (an auth.upstream-oauth2 tokenUrl).
func (s *Server) TokenURL() string { return s.url + TokenPath }

// rebuildLocked re-renders the JWKS document; the caller holds s.mu or
// owns s exclusively.
func (s *Server) rebuildLocked() {
	set := struct {
		Keys []JWK `json:"keys"`
	}{Keys: make([]JWK, 0, len(s.keys))}
	for _, k := range s.keys {
		set.Keys = append(set.Keys, k.JWK())
	}
	doc, err := json.Marshal(set)
	if err != nil {
		doc = []byte(`{"keys":[]}`)
	}
	s.doc = doc
}

// JWKS returns the JWKS document the endpoint serves (without overrides).
func (s *Server) JWKS() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.doc)
}

// Key returns the signing key of alg: the last published key with that
// algorithm.
func (s *Server) Key(alg Alg) (*Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signerLocked(alg)
}

func (s *Server) signerLocked(alg Alg) (*Key, bool) {
	for i := len(s.keys) - 1; i >= 0; i-- {
		if s.keys[i].Alg == alg {
			return s.keys[i], true
		}
	}
	return nil, false
}

// RemoveKey unpublishes every key with kid and returns how many it
// removed.
func (s *Server) RemoveKey(kid string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.keys)
	s.keys = slices.DeleteFunc(s.keys, func(k *Key) bool { return k.ID == kid })
	s.rebuildLocked()
	return n - len(s.keys)
}

// Rotate generates a key for alg and publishes it as the algorithm's
// signing key. With keepPrevious the other keys stay published (clients
// holding a cached set keep verifying old tokens); without it every other
// key of alg is removed, so tokens signed before carry an unknown kid.
func (s *Server) Rotate(alg Alg, keepPrevious bool) (*Key, error) {
	k, err := GenerateKey(alg)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !keepPrevious {
		s.keys = slices.DeleteFunc(s.keys, func(o *Key) bool { return o.Alg == alg })
	}
	s.keys = append(s.keys, k)
	s.rebuildLocked()
	return k, nil
}

// SetJWKSOverride scripts the JWKS endpoint; the zero Override restores
// normal answers.
func (s *Server) SetJWKSOverride(o Override) {
	o.Header, o.Body = o.Header.Clone(), slices.Clone(o.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jwksOver = o
}

// SetTokenOverride scripts the token endpoint; the zero Override restores
// normal answers.
func (s *Server) SetTokenOverride(o Override) {
	o.Header, o.Body = o.Header.Clone(), slices.Clone(o.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenOver = o
}

// JWKSFetches returns the recorded JWKS requests, oldest first.
func (s *Server) JWKSFetches() []Fetch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.fetches)
}

// TokenRequests returns the recorded token requests, oldest first.
func (s *Server) TokenRequests() []TokenRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tokenReqs)
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	return Stats{
		JWKSFetches:   s.jwksCount.Load(),
		TokenRequests: s.tokenCount.Load(),
	}
}

// Mint signs claims with the signing key of alg, filling the defaults of
// MintWith.
func (s *Server) Mint(alg Alg, claims map[string]any) (string, error) {
	k, ok := s.Key(alg)
	if !ok {
		return "", fmt.Errorf("mockidp: no %s key published", alg)
	}
	return s.MintWith(k, nil, claims)
}

// MintWith signs claims with k (published or not) and the header
// overrides of Key.Sign. Claims default to iss = Issuer, iat = now and
// exp = now + TokenTTL (NumericDate seconds, Config.Now); a nil value in
// claims removes a default (a token without exp).
func (s *Server) MintWith(k *Key, header, claims map[string]any) (string, error) {
	now := s.cfg.Now()
	full := map[string]any{
		"iss": s.url,
		"iat": now.Unix(),
		"exp": now.Add(s.cfg.TokenTTL).Unix(),
	}
	for name, v := range claims {
		if v == nil {
			delete(full, name)
			continue
		}
		full[name] = v
	}
	return k.Sign(header, full)
}

// wait sleeps d unless the request or the Server ends first.
func (s *Server) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-s.closing:
		return false
	}
}

func canned(w http.ResponseWriter, o Override) int {
	status := o.Status
	if status == 0 {
		status = http.StatusOK
	}
	for k, v := range o.Header {
		w.Header()[k] = slices.Clone(v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(o.Body)
	return status
}

func appendBounded[T any](s []T, v T) []T {
	if len(s) >= maxRecorded {
		s = slices.Delete(s, 0, len(s)-maxRecorded+1)
	}
	return append(s, v)
}

func (s *Server) serveJWKS(w http.ResponseWriter, r *http.Request) {
	s.jwksCount.Add(1)
	s.mu.Lock()
	o, doc := s.jwksOver, s.doc
	s.mu.Unlock()
	status := s.answerJWKS(w, r, o, doc)
	f := Fetch{Status: status}
	s.mu.Lock()
	s.fetches = appendBounded(s.fetches, f)
	s.mu.Unlock()
}

func (s *Server) answerJWKS(w http.ResponseWriter, r *http.Request, o Override, doc []byte) int {
	if !s.wait(r.Context(), o.Delay) {
		panic(http.ErrAbortHandler)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return http.StatusMethodNotAllowed
	}
	h := w.Header()
	h.Set("Content-Type", JWKSContentType)
	h.Set("Cache-Control", s.cfg.CacheControl)
	if o.canned() {
		return canned(w, o)
	}
	for k, v := range o.Header {
		h[k] = slices.Clone(v)
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(doc)
	}
	return http.StatusOK
}

// tokenError is an RFC 6749 section 5.2 error response.
type tokenError struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// tokenResponse is an RFC 6749 section 5.1 success response.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) int {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
	return status
}

func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	s.tokenCount.Add(1)
	s.mu.Lock()
	o := s.tokenOver
	s.mu.Unlock()
	var rec TokenRequest
	rec.Status = s.answerToken(w, r, o, &rec)
	s.mu.Lock()
	s.tokenReqs = appendBounded(s.tokenReqs, rec)
	s.mu.Unlock()
}

func (s *Server) answerToken(w http.ResponseWriter, r *http.Request, o Override, rec *TokenRequest) int {
	// The Basic credentials are recorded before the body is read, so a
	// request refused for its body size still carries them.
	basicID, basicSecret, basic := r.BasicAuth()
	if basic {
		rec.AuthMethod = "client_secret_basic"
		var idOK, secretOK bool
		rec.ClientID, idOK = formDecode(basicID)
		rec.ClientSecret, secretOK = formDecode(basicSecret)
		rec.MalformedCredentials = !idOK || !secretOK
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTokenBody))
	if err != nil {
		return writeJSON(w, http.StatusRequestEntityTooLarge, tokenError{Error: "invalid_request", Description: "body too large"})
	}
	form, formErr := url.ParseQuery(string(body))
	rec.GrantType, rec.Scope = form.Get("grant_type"), form.Get("scope")
	_, postID := form["client_id"]
	_, postSecret := form["client_secret"]
	both := basic && postSecret
	if !basic && (postID || postSecret) {
		rec.AuthMethod = "client_secret_post"
		rec.ClientID, rec.ClientSecret = form.Get("client_id"), form.Get("client_secret")
	}
	if both {
		rec.AuthMethod = ""
	}

	if !s.wait(r.Context(), o.Delay) {
		panic(http.ErrAbortHandler)
	}
	if o.canned() {
		return canned(w, o)
	}
	for k, v := range o.Header {
		w.Header()[k] = slices.Clone(v)
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		return writeJSON(w, http.StatusMethodNotAllowed, tokenError{Error: "invalid_request", Description: "POST only"})
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		return writeJSON(w, http.StatusBadRequest, tokenError{Error: "invalid_request", Description: "want application/x-www-form-urlencoded"})
	}
	if formErr != nil {
		return writeJSON(w, http.StatusBadRequest, tokenError{Error: "invalid_request", Description: "malformed form"})
	}
	switch {
	case rec.GrantType == "":
		return writeJSON(w, http.StatusBadRequest, tokenError{Error: "invalid_request", Description: "grant_type missing"})
	case rec.GrantType != "client_credentials":
		return writeJSON(w, http.StatusBadRequest, tokenError{Error: "unsupported_grant_type"})
	case both:
		return writeJSON(w, http.StatusBadRequest, tokenError{Error: "invalid_request", Description: "more than one client authentication method"})
	case rec.MalformedCredentials:
		w.Header().Set("WWW-Authenticate", `Basic realm="mockidp"`)
		return writeJSON(w, http.StatusUnauthorized, tokenError{Error: "invalid_client", Description: "client credentials are not form-urlencoded"})
	case !s.authenticate(rec.ClientID, rec.ClientSecret, basic || postID || postSecret):
		if basic {
			w.Header().Set("WWW-Authenticate", `Basic realm="mockidp"`)
		}
		return writeJSON(w, http.StatusUnauthorized, tokenError{Error: "invalid_client"})
	}
	token, err := s.accessToken(rec.ClientID, rec.Scope)
	if err != nil {
		return writeJSON(w, http.StatusInternalServerError, tokenError{Error: "server_error"})
	}
	rec.AccessToken = token
	return writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   int64(s.cfg.TokenTTL / time.Second),
		Scope:       rec.Scope,
	})
}

// formDecode reverses the form-urlencoding of a client_secret_basic part
// (RFC 6749 section 2.3.1). A part that is not validly encoded is
// returned as sent, with ok false.
func formDecode(part string) (string, bool) {
	decoded, err := url.QueryUnescape(part)
	if err != nil {
		return part, false
	}
	return decoded, true
}

func (s *Server) authenticate(id, secret string, presented bool) bool {
	if !presented || id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) == 0 {
		return true
	}
	want, ok := s.clients[id]
	return ok && want == secret
}

// accessToken mints the access token: a JWT signed with the RS256 key (or
// the last published key) naming the client, or an opaque random token
// when no key is published.
func (s *Server) accessToken(clientID, scope string) (string, error) {
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", fmt.Errorf("mockidp: token id: %w", err)
	}
	s.mu.Lock()
	k, ok := s.signerLocked(RS256)
	if !ok && len(s.keys) > 0 {
		k, ok = s.keys[len(s.keys)-1], true
	}
	s.mu.Unlock()
	if !ok {
		return "opaque-" + hex.EncodeToString(jti[:]), nil
	}
	claims := map[string]any{
		"sub":       clientID,
		"client_id": clientID,
		"jti":       hex.EncodeToString(jti[:]),
	}
	if scope != "" {
		claims["scope"] = strings.Join(strings.Fields(scope), " ")
	}
	return s.MintWith(k, nil, claims)
}
