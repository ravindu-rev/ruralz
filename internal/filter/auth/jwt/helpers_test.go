// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"encoding/json"
	"math/big"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwt/jwks"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// t0 is the request time of every test.
var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// Issuers and audiences of the fixture.
const (
	issA   = "https://idp.example"
	issB   = "https://other-idp.example"
	audA   = "shop-api"
	policy = "jwt-default"
)

// signers are the fixture's keys, one per algorithm, generated once per
// test binary because RSA generation is slow.
type signers struct {
	rs, ps, es, ed *mockidp.Key
	err            error
}

// testSigners generates the keys on first use.
var testSigners = sync.OnceValue(func() signers {
	var s signers
	for _, k := range []struct {
		dst **mockidp.Key
		alg mockidp.Alg
	}{{&s.rs, mockidp.RS256}, {&s.ps, mockidp.PS256}, {&s.es, mockidp.ES256}, {&s.ed, mockidp.EdDSA}} {
		if *k.dst, s.err = mockidp.GenerateKey(k.alg); s.err != nil {
			return s
		}
	}
	return s
})

// keysFor returns the fixture signers or fails the test.
func keysFor(t testing.TB) signers {
	t.Helper()
	s := testSigners()
	if s.err != nil {
		t.Fatal(s.err)
	}
	return s
}

// keySet builds a key set of issA fetched a minute before t0, living an
// hour, from the public JWKs of keys.
func keySet(t testing.TB, keys ...any) *jwks.Keys {
	t.Helper()
	entries := make([]any, len(keys))
	for i, k := range keys {
		if mk, ok := k.(*mockidp.Key); ok {
			entries[i] = mk.JWK()
		} else {
			entries[i] = k
		}
	}
	doc, err := json.Marshal(map[string]any{"keys": entries})
	if err != nil {
		t.Fatal(err)
	}
	ks, err := jwks.NewKeys(doc, issA, nil, t0.Add(-time.Minute), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

// fakeSource is a keySource over a fixed key set.
type fakeSource struct {
	keys     atomic.Pointer[jwks.Keys]
	nudges   atomic.Int64
	releases atomic.Int64
}

func (s *fakeSource) Keys() *jwks.Keys { return s.keys.Load() }
func (s *fakeSource) Nudge(time.Time)  { s.nudges.Add(1) }
func (s *fakeSource) Release()         { s.releases.Add(1) }

// source returns a fakeSource serving ks.
func source(ks *jwks.Keys) *fakeSource {
	s := &fakeSource{}
	s.keys.Store(ks)
	return s
}

// decisions records ruralz_auth_decisions_total.
type decisions struct {
	mu    sync.Mutex
	allow int
	deny  map[string]int
}

func (d *decisions) Allow(emit.Stripe) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allow++
}

func (d *decisions) Deny(_ emit.Stripe, code string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deny == nil {
		d.deny = map[string]int{}
	}
	d.deny[code]++
}

// gauge records the last value set.
type gauge struct {
	v    atomic.Int64
	sets atomic.Int64
}

func (g *gauge) Add(emit.Stripe, int64) {}
func (g *gauge) Set(v int64) {
	g.v.Store(v)
	g.sets.Add(1)
}

// revoker revokes every token whose jti is listed and records the last
// call.
type revoker struct {
	mu                 sync.Mutex
	jtis               map[string]bool
	iss, kid, jti, sub string
	iat                time.Time
	calls              int
}

func (r *revoker) RevokedJWT(iss, kid, jti, sub string, iat time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.iss, r.kid, r.jti, r.sub, r.iat = iss, kid, jti, sub, iat
	r.calls++
	return r.jtis[jti]
}
func (r *revoker) RevokedKey(string, string) bool    { return false }
func (r *revoker) RevokedAPIKey([32]byte) bool       { return false }
func (r *revoker) RevokedBasic(string) bool          { return false }
func (r *revoker) RevokedCert([]byte, *big.Int) bool { return false }

// fixture is a Filter for issA (and optionally issB) over fake sources.
type fixture struct {
	t         testing.TB
	keys      signers
	src       *fakeSource
	srcB      *fakeSource
	decisions *decisions
	age       *gauge
	revoker   *revoker
	f         *Filter
}

// options adjusts a fixture.
type options struct {
	consumers []*v1alpha1.Consumer
	keys      *jwks.Keys // default: every fixture signer
	issB      bool
}

// newFixture builds a Filter whose issA serves the fixture's signers.
func newFixture(t testing.TB, o options) *fixture {
	t.Helper()
	fx := &fixture{t: t, keys: keysFor(t), decisions: &decisions{}, age: &gauge{}, revoker: &revoker{jtis: map[string]bool{}}}
	ks := o.keys
	if ks == nil {
		ks = keySet(t, fx.keys.rs, fx.keys.ps, fx.keys.es, fx.keys.ed)
	}
	fx.src = source(ks)
	cfg := &v1alpha1.AuthJWTConfig{Issuers: []v1alpha1.JWTIssuer{
		{Issuer: issA, JWKSURL: "https://idp.example/jwks", Audiences: []string{audA, "admin-api"}},
	}}
	sources := []keySource{fx.src}
	if o.issB {
		fx.srcB = &fakeSource{}
		cfg.Issuers = append(cfg.Issuers, v1alpha1.JWTIssuer{Issuer: issB, JWKSURL: "https://other-idp.example/jwks", Audiences: []string{audA}})
		sources = append(sources, fx.srcB)
	}
	env := buildEnv(cfg, o.consumers...)
	env.Metrics = &emit.PolicyMetrics{Auth: fx.decisions, JWKSAge: fx.age}
	ix, err := auth.AcquireIndex(env)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newCodec()
	if err != nil {
		t.Fatal(err)
	}
	fx.f = newFilter(env, cfg, ix, sources, fx.revoker, c)
	t.Cleanup(func() { _ = fx.f.Close() })
	return fx
}

// buildEnv returns the BuildEnv of the fixture Policy over consumers.
func buildEnv(cfg *v1alpha1.AuthJWTConfig, consumers ...*v1alpha1.Consumer) filter.BuildEnv {
	rs := make([]*hub.Resource, 0, len(consumers))
	for _, c := range consumers {
		rs = append(rs, &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindConsumer, Name: c.Metadata.Name}, Object: c})
	}
	return filter.BuildEnv{
		Policy:    &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: policy}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthJWT}},
		Config:    cfg,
		Bundle:    hub.NewBundle(rs),
		Consumers: identity.CompileConsumers(consumers).Map(),
	}
}

// claimsOf returns the default claims of a token of issA at t0 with
// overrides applied; a nil override removes the claim.
func claimsOf(overrides map[string]any) map[string]any {
	c := map[string]any{
		"iss": issA,
		"aud": audA,
		"sub": "user-1",
		"iat": t0.Unix(),
		"exp": t0.Add(time.Hour).Unix(),
	}
	for k, v := range overrides {
		if v == nil {
			delete(c, k)
			continue
		}
		c[k] = v
	}
	return c
}

// mint signs the default claims with overrides using k and header
// overrides (Key.Sign).
func mint(t testing.TB, k *mockidp.Key, header, overrides map[string]any) string {
	t.Helper()
	tok, err := k.Sign(header, claimsOf(overrides))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// exchange returns a request at t0 carrying Authorization values.
func exchange(values ...string) *filtertest.Exchange {
	x := &filtertest.Exchange{Start: t0, Hdr: http.Header{}}
	for _, v := range values {
		x.Hdr.Add("Authorization", v)
	}
	return x
}

// bearer returns a request at t0 carrying tok as a Bearer token.
func bearer(tok string) *filtertest.Exchange { return exchange("Bearer " + tok) }

// wantDeny checks a deny with code: Respond with the code's registered
// status and, on a 401, the auth.jwt challenge; no identity is set.
func wantDeny(t *testing.T, r filter.Result, x *filtertest.Exchange, code string) {
	t.Helper()
	if r.Outcome != filter.Respond || r.Response == nil || r.Response.Code != code {
		t.Fatalf("result = %+v (response %+v), want a deny with %s", r, r.Response, code)
	}
	if r.Response.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", r.Response.Status)
	}
	if got := r.Response.Header.Get("WWW-Authenticate"); got != auth.ChallengeJWT {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, auth.ChallengeJWT)
	}
	if r.Response.Body != nil || r.Response.Detail != "" {
		t.Fatal("a rejection carries a body or detail")
	}
	if x != nil && x.Ident != nil {
		t.Fatal("a rejected request has an identity")
	}
}

// wantAllow checks Continue with an identity set and returns it.
func wantAllow(t *testing.T, r filter.Result, x *filtertest.Exchange) *filter.Identity {
	t.Helper()
	if r.Outcome != filter.Continue || x.Ident == nil {
		t.Fatalf("result = %+v (response %+v), identity %v, want Continue with an identity", r, r.Response, x.Ident)
	}
	return x.Ident
}

// consumer returns a Consumer named name with credentials cr.
func consumer(name string, cr v1alpha1.Credentials) *v1alpha1.Consumer {
	return &v1alpha1.Consumer{Metadata: v1alpha1.ObjectMeta{Name: name}, Spec: v1alpha1.ConsumerSpec{Credentials: cr}}
}

// mustJSON encodes v or fails the test.
func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
