// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package apikey

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// policy is the name of the fixture Policy.
const policy = "partner-keys"

// API keys of the fixture: 32 base64url characters each, so they also
// pass the 22-byte floor of secretRef-held keys.
const (
	keyAcme   = "acme-0123456789abcdefghijklmnopq"
	keyGlobex = "globex-0123456789abcdefghijklmno"
	keyOld    = "old-0123456789abcdefghijklmnopqr"
	keyNew    = "new-0123456789abcdefghijklmnopqr"
)

// refPrevious is the secretRef of a rotated key.
var refPrevious = secret.Ref{Provider: v1alpha1.SecretProviderEnv, Name: "RURALZ_SECRET_PARTNER_KEY"}

// hashOf returns the stored form of key's digest.
func hashOf(key string) string {
	d := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(d[:])
}

// literal returns an apiKeys entry holding key's digest.
func literal(name, key string) v1alpha1.APIKey {
	return v1alpha1.APIKey{Name: name, Hash: hashOf(key)}
}

// held returns the apiKeys entry "previous" holding a secretRef.
func held(ref secret.Ref) v1alpha1.APIKey {
	return v1alpha1.APIKey{Name: "previous", SecretRef: &v1alpha1.SecretRef{Provider: ref.Provider, Name: ref.Name, Key: ref.Key}}
}

// consumer returns a Consumer named name of tier holding keys.
func consumer(name, tier string, keys ...v1alpha1.APIKey) *v1alpha1.Consumer {
	return &v1alpha1.Consumer{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.ConsumerSpec{Tier: tier, Credentials: v1alpha1.Credentials{APIKeys: keys}},
	}
}

// buildEnv returns the BuildEnv of the fixture Policy with cfg over
// consumers, with a secret table st (nil for none).
func buildEnv(cfg any, st secret.Store, consumers ...*v1alpha1.Consumer) filter.BuildEnv {
	rs := make([]*hub.Resource, 0, len(consumers))
	for _, c := range consumers {
		rs = append(rs, &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindConsumer, Name: c.Metadata.Name}, Object: c})
	}
	return filter.BuildEnv{
		Policy:    &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: policy}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthAPIKey}},
		Config:    cfg,
		Bundle:    hub.NewBundle(rs),
		Consumers: identity.CompileConsumers(consumers).Map(),
		Secrets:   st,
		Shared:    &shared{},
	}
}

// build builds a Filter from env or fails the test; the Filter is closed
// when the test ends.
func build(t testing.TB, env filter.BuildEnv) *Filter {
	t.Helper()
	flt, err := NewFactory(nil).Build(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := flt.(*Filter)
	if !ok {
		t.Fatalf("Build returned %T", flt)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// exchange returns a request carrying the field lines of h.
func exchange(h http.Header) *filtertest.Exchange {
	if h == nil {
		h = http.Header{}
	}
	return &filtertest.Exchange{Start: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Hdr: h}
}

// withKey returns a request carrying key in X-Api-Key.
func withKey(key string) *filtertest.Exchange {
	return exchange(http.Header{"X-Api-Key": {key}})
}

// wantDeny checks a deny with code: Respond, a 401 carrying challenge, no
// body or detail, and no identity set.
func wantDeny(t *testing.T, r filter.Result, x *filtertest.Exchange, code, challenge string) {
	t.Helper()
	if r.Outcome != filter.Respond || r.Response == nil || r.Response.Code != code {
		t.Fatalf("result = %+v (response %+v), want a deny with %s", r, r.Response, code)
	}
	if r.Response.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", r.Response.Status)
	}
	if got := r.Response.Header.Get("WWW-Authenticate"); got != challenge {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, challenge)
	}
	if r.Response.Body != nil || r.Response.Detail != "" {
		t.Fatal("a rejection carries a body or detail")
	}
	if x.Ident != nil {
		t.Fatal("a rejected request has an identity")
	}
}

// wantAllow checks Continue with an identity bound to the Consumer named
// consumer and returns the identity.
func wantAllow(t *testing.T, r filter.Result, x *filtertest.Exchange, consumer string) *filter.Identity {
	t.Helper()
	if r.Outcome != filter.Continue || x.Ident == nil {
		t.Fatalf("result = %+v (response %+v), identity %v, want Continue with an identity", r, r.Response, x.Ident)
	}
	if x.Ident.Consumer == nil || x.Ident.Consumer.Name != consumer {
		t.Fatalf("identity = %+v, want Consumer %q", x.Ident, consumer)
	}
	return x.Ident
}

// defaultChallenge is the challenge of the default header.
const defaultChallenge = `APIKey header="x-api-key"`

// shared memoizes like the snapshot compiler's filter.Shared.
type shared struct {
	mu   sync.Mutex
	vals map[any]any
	errs map[any]error
}

func (s *shared) Get(key any, build func() (any, error)) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vals == nil {
		s.vals, s.errs = map[any]any{}, map[any]error{}
	}
	if v, ok := s.vals[key]; ok {
		return v, s.errs[key]
	}
	v, err := build()
	s.vals[key], s.errs[key] = v, err
	return v, err
}

// store is a secret.Store over a value table that delivers rotations to
// its watches, as the resolver does for the active Store.
type store struct {
	mu      sync.Mutex
	values  map[secret.Ref]string
	watches map[secret.Ref]map[int]func(secret.Value) error
	next    int
}

func newStore(values map[secret.Ref]string) *store {
	return &store{values: values, watches: map[secret.Ref]map[int]func(secret.Value) error{}}
}

func (s *store) Get(r secret.Ref) (secret.Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[r]
	if !ok {
		return secret.Value{}, false
	}
	return secret.NewValue([]byte(v)), true
}

func (s *store) Watch(r secret.Ref, fn func(secret.Value) error) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.watches[r] == nil {
		s.watches[r] = map[int]func(secret.Value) error{}
	}
	id := s.next
	s.next++
	s.watches[r][id] = fn
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.watches[r], id)
	}
}

// rotate publishes a new value of r and calls its watches.
func (s *store) rotate(r secret.Ref, v string) {
	s.mu.Lock()
	s.values[r] = v
	fns := make([]func(secret.Value) error, 0, len(s.watches[r]))
	for _, fn := range s.watches[r] {
		fns = append(fns, fn)
	}
	s.mu.Unlock()
	for _, fn := range fns {
		_ = fn(secret.NewValue([]byte(v)))
	}
}

// live returns the number of registered watches.
func (s *store) live() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.watches {
		n += len(w)
	}
	return n
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

// revoker revokes the listed API key digests and counts its calls.
type revoker struct {
	mu      sync.Mutex
	digests map[[32]byte]bool
	calls   int
}

func (r *revoker) RevokedAPIKey(d [32]byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.digests[d]
}
func (*revoker) RevokedJWT(string, string, string, string, time.Time) bool { return false }
func (*revoker) RevokedKey(string, string) bool                            { return false }
func (*revoker) RevokedBasic(string) bool                                  { return false }
func (*revoker) RevokedCert([]byte, *big.Int) bool                         { return false }

var _ identity.Revoker = (*revoker)(nil)
