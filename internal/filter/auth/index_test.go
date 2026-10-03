// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for spec 06 requirement 12 (one credential index per snapshot,
// shared by the auth Filters of that snapshot) and R-55 (the KeyIndex of
// secretRef-held keys carries over through BuildEnv.Previous with its
// watches).

const testKey = "ZXhhbXBsZS1rZXktYWFhYWFhYWFhYWFhYWFhYWFhYWE"

var keyRef = secret.Ref{Provider: v1alpha1.SecretProviderEnv, Name: "RURALZ_SECRET_KEY"}

// store is a secret.Store over fixed values that counts live watches.
type store struct {
	mu      sync.Mutex
	values  map[secret.Ref]string
	watches int
}

func (s *store) Get(r secret.Ref) (secret.Value, bool) {
	v, ok := s.values[r]
	if !ok {
		return secret.Value{}, false
	}
	return secret.NewValue([]byte(v)), true
}

func (s *store) Watch(secret.Ref, func(secret.Value) error) func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watches++
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.watches--
	}
}

func (s *store) live() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watches
}

// shared memoizes like the snapshot compiler's filter.Shared.
type shared struct {
	mu     sync.Mutex
	vals   map[any]any
	errs   map[any]error
	builds int
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
	s.builds++
	v, err := build()
	s.vals[key], s.errs[key] = v, err
	return v, err
}

// holder is an auth Filter keeping its index.
type holder struct{ ix *identity.Index }

func (h holder) Handle(context.Context, phase.Phase, filter.Exchange) filter.Result {
	return filter.Next()
}
func (h holder) CredentialIndex() *identity.Index { return h.ix }

func bundleWith(cs ...*v1alpha1.Consumer) *hub.Bundle {
	rs := make([]*hub.Resource, 0, len(cs))
	for _, c := range cs {
		rs = append(rs, &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindConsumer, Name: c.Metadata.Name}, Object: c})
	}
	return hub.NewBundle(rs)
}

func keyConsumer(name, tier string) *v1alpha1.Consumer {
	return &v1alpha1.Consumer{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.ConsumerSpec{Tier: tier, Credentials: v1alpha1.Credentials{APIKeys: []v1alpha1.APIKey{
			{Name: "k", SecretRef: &v1alpha1.SecretRef{Provider: keyRef.Provider, Name: keyRef.Name}},
		}}},
	}
}

func TestReq12AcquireIndexSharedPerSnapshot(t *testing.T) {
	st := &store{values: map[secret.Ref]string{keyRef: testKey}}
	compiled := identity.CompileConsumers([]*v1alpha1.Consumer{keyConsumer("acme", "gold")})
	env := filter.BuildEnv{
		Bundle: bundleWith(keyConsumer("acme", "gold")), Consumers: compiled.Map(),
		Secrets: st, Shared: &shared{},
	}
	a, err := AcquireIndex(env)
	if err != nil {
		t.Fatal(err)
	}
	b, err := AcquireIndex(env)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || env.Shared.(*shared).builds != 1 {
		t.Fatal("the snapshot's Filters do not share one index")
	}
	want, _ := compiled.Get("acme")
	if c, _ := a.APIKey(identity.DigestString(testKey)); c != want {
		t.Fatal("index does not bind through the snapshot's Consumer views")
	}
	// Two acquisitions hold two references: the watch stops after both.
	a.Release()
	if st.live() != 1 {
		t.Fatal("first Release stopped the watch")
	}
	b.Release()
	if st.live() != 0 {
		t.Fatal("last Release kept the watch")
	}
}

func TestR55AcquireIndexCarriesKeyIndexOver(t *testing.T) {
	st := &store{values: map[secret.Ref]string{keyRef: testKey}}
	first, err := AcquireIndex(filter.BuildEnv{Bundle: bundleWith(keyConsumer("acme", "")), Secrets: st, Shared: &shared{}})
	if err != nil {
		t.Fatal(err)
	}
	// Next snapshot: the tier changed, the key binding did not.
	env := filter.BuildEnv{
		Bundle: bundleWith(keyConsumer("acme", "gold")), Secrets: st, Shared: &shared{},
		Previous: holder{first},
	}
	second, err := AcquireIndex(env)
	if err != nil {
		t.Fatal(err)
	}
	if second == first || second.KeyIndex() != first.KeyIndex() {
		t.Fatal("KeyIndex not carried over into the new snapshot's index")
	}
	if c, _ := second.APIKey(identity.DigestString(testKey)); c == nil || c.Tier != "gold" {
		t.Fatal("new snapshot's index binds an old Consumer view")
	}
	if st.live() != 1 {
		t.Fatalf("watches = %d, want the carried-over one", st.live())
	}
	first.Release() // the old snapshot retires
	if st.live() != 1 {
		t.Fatal("old snapshot's release stopped the carried-over watch")
	}
	second.Release()
	if st.live() != 0 {
		t.Fatal("watch outlived every snapshot")
	}
}

func TestAcquireIndexWithoutShared(t *testing.T) {
	ix, err := AcquireIndex(filter.BuildEnv{})
	if err != nil || len(ix.Consumers()) != 0 {
		t.Fatalf("AcquireIndex(empty env) = %v, %v", ix, err)
	}
	ix.Release()
	// A Previous that is not an IndexHolder, or holds no index, is ignored.
	for _, prev := range []filter.Filter{nil, holder{}} {
		if _, err := AcquireIndex(filter.BuildEnv{Previous: prev}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAcquireIndexErrors(t *testing.T) {
	bad := &v1alpha1.Consumer{
		Metadata: v1alpha1.ObjectMeta{Name: "bad"},
		Spec:     v1alpha1.ConsumerSpec{Credentials: v1alpha1.Credentials{APIKeys: []v1alpha1.APIKey{{Name: "k", Hash: "sha256:nope"}}}},
	}
	for _, sh := range []filter.Shared{nil, &shared{}} {
		_, err := AcquireIndex(filter.BuildEnv{Bundle: bundleWith(bad), Shared: sh})
		if code, _ := errcode.CodeOf(err); code != "RZ-CFG-005" {
			t.Fatalf("error %v, want RZ-CFG-005", err)
		}
	}
	// An unresolved secretRef key rejects the Revision with RZ-CFG-026.
	_, err := AcquireIndex(filter.BuildEnv{Bundle: bundleWith(keyConsumer("a", "")), Secrets: &store{}})
	if code, _ := errcode.CodeOf(err); code != "RZ-CFG-026" {
		t.Fatalf("error %v, want RZ-CFG-026", err)
	}

	sh := &shared{vals: map[any]any{indexKey{}: "not an index"}, errs: map[any]error{}}
	if _, err := AcquireIndex(filter.BuildEnv{Shared: sh}); err == nil {
		t.Fatal("a foreign shared value was accepted")
	}

	// Every holder released the index before a late acquisition.
	st := &store{values: map[secret.Ref]string{keyRef: testKey}}
	env := filter.BuildEnv{Bundle: bundleWith(keyConsumer("a", "")), Secrets: st, Shared: &shared{}}
	ix, err := AcquireIndex(env)
	if err != nil {
		t.Fatal(err)
	}
	ix.Release()
	if _, err := AcquireIndex(env); !errors.Is(err, ErrIndexClosed) {
		t.Fatalf("late acquisition error %v, want ErrIndexClosed", err)
	}
}
