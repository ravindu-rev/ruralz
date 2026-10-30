// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwt/check"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwt/jwks"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// node is a running JWKS manager on a fake clock and a mock IdP over TLS
// on loopback whose tokens are minted at t0.
type node struct {
	idp     *mockidp.Server
	keys    *jwks.Manager
	factory *Factory
}

// newNode starts the IdP and the manager until the test ends.
func newNode(t *testing.T) *node {
	t.Helper()
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
	idp, err := mockidp.Start(context.Background(), mockidp.Config{TLS: srvTLS, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idp.Close() })
	guard, err := egress.ParseAllow("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	m := jwks.New(jwks.Options{Guard: guard, TLS: tlsconf.Client(ca.Pool()), Clock: clocktest.New(t0)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
	})
	f, err := NewFactory(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &node{idp: idp, keys: m, factory: f}
}

// config is an auth.jwt config naming the node's IdP.
func (n *node) config() *v1alpha1.AuthJWTConfig {
	return &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{{Issuer: n.idp.Issuer(), JWKSURL: n.idp.JWKSURL(), Audiences: []string{audA}}}}
}

// build builds the config's Filter.
func (n *node) build(t *testing.T, env filter.BuildEnv) *Filter {
	t.Helper()
	flt, err := n.factory.Build(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := flt.(*Filter)
	if !ok {
		t.Fatalf("Build returned %T", flt)
	}
	return f
}

// token mints an RS256 token of the IdP for audA.
func (n *node) token(t *testing.T) string {
	t.Helper()
	tok, err := n.idp.Mint(mockidp.RS256, map[string]any{"aud": audA, "sub": "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// TestBuildPrefetchesIssuers checks "When compiling a Revision, Nodes
// prefetch issuers": the built Filter verifies a token at once, and a
// second Build of the same issuer reuses the keys without a fetch.
func TestBuildPrefetchesIssuers(t *testing.T) {
	n := newNode(t)
	f := n.build(t, buildEnv(n.config()))
	x := bearer(n.token(t))
	id := wantAllow(t, f.Handle(t.Context(), phase.OnRequestHeaders, x), x)
	if id.Principal != "jwt:"+n.idp.Issuer()+"#user-1" {
		t.Fatalf("Principal = %q", id.Principal)
	}
	g := n.build(t, buildEnv(n.config()))
	x = bearer(n.token(t))
	wantAllow(t, g.Handle(t.Context(), phase.OnRequestHeaders, x), x)
	if got := n.idp.Stats().JWKSFetches; got != 1 {
		t.Fatalf("fetches = %d, want 1: the second Build refetched", got)
	}
	_ = f.Close()
	_ = g.Close()
}

// TestBuildActivatesWhenFetchFails checks "activate even if a fetch
// fails": Build returns a Filter, which cannot decide with RZ-AUTH-006
// until keys arrive.
func TestBuildActivatesWhenFetchFails(t *testing.T) {
	n := newNode(t)
	n.idp.SetJWKSOverride(mockidp.Override{Status: http.StatusServiceUnavailable})
	f := n.build(t, buildEnv(n.config()))
	defer func() { _ = f.Close() }()
	x := bearer(n.token(t))
	r := f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if r.Outcome != filter.CannotDecide || r.Code != auth.CodeNoKeys {
		t.Fatalf("result = %+v, want CannotDecide with RZ-AUTH-006", r)
	}
}

// TestCloseReleasesIssuers checks that Close releases the Filter's key
// sets: an issuer every Filter released is fetched again by the next
// Build, while one still held is not.
func TestCloseReleasesIssuers(t *testing.T) {
	n := newNode(t)
	a := n.build(t, buildEnv(n.config()))
	b := n.build(t, buildEnv(n.config()))
	_ = a.Close()
	c := n.build(t, buildEnv(n.config()))
	if got := n.idp.Stats().JWKSFetches; got != 1 {
		t.Fatalf("fetches = %d while a Filter held the issuer", got)
	}
	_ = b.Close()
	_ = c.Close()
	d := n.build(t, buildEnv(n.config()))
	defer func() { _ = d.Close() }()
	if got := n.idp.Stats().JWKSFetches; got != 2 {
		t.Fatalf("fetches = %d, want a new fetch after every Filter closed", got)
	}
}

// TestBuildRejectsConfig checks the Factory's own config rules: a jwksUrl
// that is not an https URL with a host is RZ-CFG-037; a missing, empty or
// repeated issuer, an issuer without audiences and a config of another
// type are RZ-CFG-005.
func TestBuildRejectsConfig(t *testing.T) {
	n := newNode(t)
	iss := func(issuer, url string, auds ...string) v1alpha1.JWTIssuer {
		return v1alpha1.JWTIssuer{Issuer: issuer, JWKSURL: url, Audiences: auds}
	}
	const ok = "https://idp.example/jwks"
	tests := []struct {
		name   string
		config any
		code   string
	}{
		{"nil config", nil, codeSchema},
		{"another type", &v1alpha1.AuthAPIKeyConfig{}, codeSchema},
		{"nil auth.jwt config", (*v1alpha1.AuthJWTConfig)(nil), codeSchema},
		{"no issuers", &v1alpha1.AuthJWTConfig{}, codeSchema},
		{"empty issuer", &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{iss("", ok, audA)}}, codeSchema},
		{"repeated issuer", &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{iss(issA, ok, audA), iss(issA, ok, audA)}}, codeSchema},
		{"no audiences", &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{iss(issA, ok)}}, codeSchema},
		{"http", &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{iss(issA, "http://idp.example/jwks", audA)}}, CodeHTTPS},
		{"no scheme", &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{iss(issA, "idp.example/jwks", audA)}}, CodeHTTPS},
		{"no host", &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{iss(issA, "https:///jwks", audA)}}, CodeHTTPS},
		{"opaque", &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{iss(issA, "https:idp.example", audA)}}, CodeHTTPS},
		{"not a URL", &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{iss(issA, "https://[::1", audA)}}, CodeHTTPS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := buildEnv(nil)
			env.Config = tt.config
			flt, err := n.factory.Build(t.Context(), env)
			if code, _ := errcode.CodeOf(err); code != tt.code || flt != nil {
				t.Fatalf("Build = %v, %v, want %s", flt, err, tt.code)
			}
		})
	}
	// An upper-case scheme is still https.
	cfg := n.config()
	cfg.Issuers[0].JWKSURL = "HTTPS" + cfg.Issuers[0].JWKSURL[len("https"):]
	f := n.build(t, buildEnv(cfg))
	_ = f.Close()
}

// TestBuildRefusesFirstFinding: Build's config error is the first finding
// of package check, with its code, for every config, so offline
// validation and activation never disagree.
func TestBuildRefusesFirstFinding(t *testing.T) {
	const ok = "https://idp.example/jwks"
	for _, cfg := range []*v1alpha1.AuthJWTConfig{
		{},
		{Issuers: []v1alpha1.JWTIssuer{{Issuer: "", JWKSURL: ok, Audiences: []string{audA}}}},
		{Issuers: []v1alpha1.JWTIssuer{{Issuer: issA, JWKSURL: ok, Audiences: []string{audA}}, {Issuer: issA, JWKSURL: ok, Audiences: []string{audA}}}},
		{Issuers: []v1alpha1.JWTIssuer{{Issuer: issA, JWKSURL: ok}}},
		{Issuers: []v1alpha1.JWTIssuer{{Issuer: issA, JWKSURL: "ftp://x", Audiences: []string{audA}}}},
		{Issuers: []v1alpha1.JWTIssuer{{Issuer: issA, JWKSURL: ok, Audiences: []string{audA}}}},
	} {
		err := validate(cfg)
		fs := check.Findings(cfg)
		switch {
		case len(fs) == 0 && err != nil:
			t.Errorf("%+v: Build refuses %v, the check reports nothing", cfg, err)
		case len(fs) > 0 && err == nil:
			t.Errorf("%+v: the check reports %v, Build accepts", cfg, fs)
		case len(fs) > 0:
			if code, _ := errcode.CodeOf(err); code != fs[0].Code || !strings.HasSuffix(err.Error(), fs[0].Message) {
				t.Errorf("%+v: Build refuses %v, the check's first finding is %s %s", cfg, err, fs[0].Code, fs[0].Message)
			}
		}
	}
}

// TestBuildCanceled checks that a compile whose context ended gets its
// context error, without a code, instead of a Filter.
func TestBuildCanceled(t *testing.T) {
	n := newNode(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	flt, err := n.factory.Build(ctx, buildEnv(n.config()))
	if !errors.Is(err, context.Canceled) || flt != nil {
		t.Fatalf("Build = %v, %v, want context.Canceled", flt, err)
	}
	if _, ok := errcode.CodeOf(err); ok {
		t.Fatal("a canceled compile carries an RZ code")
	}
}

// TestPrefetchRoundShared checks that the auth.jwt Builds of one compile
// (one BuildEnv.Shared) share one prefetch round, so their waits end
// together, while Builds without Shared get their own.
func TestPrefetchRoundShared(t *testing.T) {
	n := newNode(t)
	env := buildEnv(n.config())
	env.Shared = &shared{}
	r := n.factory.round(env)
	if n.factory.round(env) != r {
		t.Fatal("two Builds of one compile got different rounds")
	}
	other := buildEnv(n.config())
	other.Shared = &shared{}
	if n.factory.round(other) == r || n.factory.round(buildEnv(n.config())) == r {
		t.Fatal("another compile shares the round")
	}
}

// shared memoizes like the snapshot compiler's filter.Shared.
type shared struct {
	mu   sync.Mutex
	vals map[any]any
}

func (s *shared) Get(key any, build func() (any, error)) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vals == nil {
		s.vals = map[any]any{}
	}
	if v, ok := s.vals[key]; ok {
		return v, nil
	}
	v, err := build()
	if err == nil {
		s.vals[key] = v
	}
	return v, err
}

// TestCredentialIndexCarriesOver checks that the Filter is an
// auth.IndexHolder: the next snapshot's Build, given it as
// BuildEnv.Previous, keeps the KeyIndex of secretRef-held API keys (and
// its secret watches) instead of rebuilding it.
func TestCredentialIndexCarriesOver(t *testing.T) {
	n := newNode(t)
	ref := secret.Ref{Provider: v1alpha1.SecretProviderEnv, Name: "RURALZ_SECRET_KEY"}
	st := secretStore{ref: secret.NewValue([]byte("ZXhhbXBsZS1rZXktYWFhYWFhYWFhYWFhYWFhYWFhYWE"))}
	c := consumer("partner", v1alpha1.Credentials{APIKeys: []v1alpha1.APIKey{
		{Name: "k", SecretRef: &v1alpha1.SecretRef{Provider: ref.Provider, Name: ref.Name}},
	}})
	env := buildEnv(n.config(), c)
	env.Secrets = st
	first := n.build(t, env)
	defer func() { _ = first.Close() }()
	next := buildEnv(n.config(), c)
	next.Secrets = st
	next.Previous = first
	second := n.build(t, next)
	defer func() { _ = second.Close() }()
	if first.CredentialIndex().KeyIndex() == nil || second.CredentialIndex().KeyIndex() != first.CredentialIndex().KeyIndex() {
		t.Fatal("the KeyIndex was not carried over through BuildEnv.Previous")
	}
}

// secretStore is a secret.Store over fixed values.
type secretStore map[secret.Ref]secret.Value

func (s secretStore) Get(r secret.Ref) (secret.Value, bool) {
	v, ok := s[r]
	return v, ok
}

func (s secretStore) Watch(secret.Ref, func(secret.Value) error) func() { return func() {} }

// TestNewFactoryNeedsManager checks that a Factory needs the Node's JWKS
// manager.
func TestNewFactoryNeedsManager(t *testing.T) {
	if f, err := NewFactory(nil, nil); !errors.Is(err, ErrNoManager) || f != nil {
		t.Fatalf("NewFactory(nil) = %v, %v", f, err)
	}
}
