// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"sync"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// fakeResolver models the secret.Resolver contract of architecture 2.8 and
// R-55: values live in one Node-wide table; Watch registrations are
// Node-wide, keyed by Ref, and survive Activate of a later Store; a
// rotation fires the watches of a Ref only while the active Store
// references it; a watch fn error counts a rotation failure.
type fakeResolver struct {
	mu       sync.Mutex
	values   map[secret.Ref][]byte
	watches  map[secret.Ref]map[int]func(secret.Value) error
	next     int
	active   *fakeStore
	failures int
	// beforeWatch runs inside Watch before registering (to model a
	// rotation racing the registration).
	beforeWatch func(secret.Ref)
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		values:  map[secret.Ref][]byte{},
		watches: map[secret.Ref]map[int]func(secret.Value) error{},
	}
}

// store returns a Store holding refs, with the current values set first.
func (r *fakeResolver) store(vals map[secret.Ref]string) *fakeStore {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := &fakeStore{r: r, refs: map[secret.Ref]bool{}}
	for ref, v := range vals {
		r.values[ref] = []byte(v)
		s.refs[ref] = true
	}
	return s
}

// activate makes s the active Store; watches are kept.
func (r *fakeResolver) activate(s *fakeStore) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = s
}

// rotate sets a new value and fires the watches of ref when the active
// Store references it.
func (r *fakeResolver) rotate(ref secret.Ref, v string) {
	r.mu.Lock()
	r.values[ref] = []byte(v)
	var fns []func(secret.Value) error
	if r.active != nil && r.active.refs[ref] {
		for _, fn := range r.watches[ref] {
			fns = append(fns, fn)
		}
	}
	r.mu.Unlock()
	for _, fn := range fns {
		if err := fn(secret.NewValue([]byte(v))); err != nil {
			r.mu.Lock()
			r.failures++
			r.mu.Unlock()
		}
	}
}

// watchCount returns the number of live registrations.
func (r *fakeResolver) watchCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.watches {
		n += len(m)
	}
	return n
}

func (r *fakeResolver) failureCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failures
}

// fakeStore is one Revision's secret table.
type fakeStore struct {
	r    *fakeResolver
	refs map[secret.Ref]bool
}

var _ secret.Store = (*fakeStore)(nil)

func (s *fakeStore) Get(ref secret.Ref) (secret.Value, bool) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	if !s.refs[ref] {
		return secret.Value{}, false
	}
	return secret.NewValue(s.r.values[ref]), true
}

func (s *fakeStore) Watch(ref secret.Ref, fn func(secret.Value) error) func() {
	if s.r.beforeWatch != nil {
		s.r.beforeWatch(ref)
	}
	s.r.mu.Lock()
	defer s.r.mu.Unlock()
	id := s.r.next
	s.r.next++
	if s.r.watches[ref] == nil {
		s.r.watches[ref] = map[int]func(secret.Value) error{}
	}
	s.r.watches[ref][id] = fn
	return func() {
		s.r.mu.Lock()
		defer s.r.mu.Unlock()
		delete(s.r.watches[ref], id)
	}
}

// Test fixtures.

// testKey is a 43-byte key (base64url of 32 bytes).
const (
	testKeyA = "ZXhhbXBsZS1rZXktYWFhYWFhYWFhYWFhYWFhYWFhYWE"
	testKeyB = "ZXhhbXBsZS1rZXktYmJiYmJiYmJiYmJiYmJiYmJiYmI"
	testKeyC = "ZXhhbXBsZS1rZXktY2NjY2NjY2NjY2NjY2NjY2NjY2M"
)

// cfgBasicHash is the credentials.basic example of
// docs/architecture/02-configuration-model.md "Consumer".
const cfgBasicHash = "pbkdf2-sha256:p3QSJbD88BiOWUtS-PBuRA:s18LvDc4t_R93uVhQXgqaaBz1bwS7QkQFuZzsLHF95A"

func envRef(name string) secret.Ref {
	return secret.Ref{Provider: v1alpha1.SecretProviderEnv, Name: name}
}

// consumer builds a Consumer; edit applies options.
func consumer(name string, edit ...func(*v1alpha1.Consumer)) *v1alpha1.Consumer {
	c := &v1alpha1.Consumer{Metadata: v1alpha1.ObjectMeta{Name: name}}
	for _, e := range edit {
		e(c)
	}
	return c
}

func withHash(keyName, key string) func(*v1alpha1.Consumer) {
	return func(c *v1alpha1.Consumer) {
		c.Spec.Credentials.APIKeys = append(c.Spec.Credentials.APIKeys,
			v1alpha1.APIKey{Name: keyName, Hash: FormatDigest(DigestString(key))})
	}
}

func withSecretKey(keyName string, ref secret.Ref) func(*v1alpha1.Consumer) {
	return func(c *v1alpha1.Consumer) {
		c.Spec.Credentials.APIKeys = append(c.Spec.Credentials.APIKeys,
			v1alpha1.APIKey{Name: keyName, SecretRef: &v1alpha1.SecretRef{Provider: ref.Provider, Name: ref.Name, Key: ref.Key}})
	}
}

func withSubject(iss, sub string) func(*v1alpha1.Consumer) {
	return func(c *v1alpha1.Consumer) {
		c.Spec.Credentials.JWT = append(c.Spec.Credentials.JWT, v1alpha1.JWTBinding{Issuer: iss, Subject: sub})
	}
}

func withClaims(iss string, claims map[string]string) func(*v1alpha1.Consumer) {
	return func(c *v1alpha1.Consumer) {
		c.Spec.Credentials.JWT = append(c.Spec.Credentials.JWT, v1alpha1.JWTBinding{Issuer: iss, Claims: claims})
	}
}

func withClient(id string) func(*v1alpha1.Consumer) {
	return func(c *v1alpha1.Consumer) {
		c.Spec.Credentials.OAuthClients = append(c.Spec.Credentials.OAuthClients, v1alpha1.OAuthClient{ClientID: id})
	}
}

func withBasic(user, hash string, iterations *int32) func(*v1alpha1.Consumer) {
	return func(c *v1alpha1.Consumer) {
		c.Spec.Credentials.Basic = append(c.Spec.Credentials.Basic,
			v1alpha1.BasicCredential{Username: user, Hash: hash, Iterations: iterations})
	}
}

func withCert(name, subject, uri string) func(*v1alpha1.Consumer) {
	return func(c *v1alpha1.Consumer) {
		c.Spec.Credentials.Certificates = append(c.Spec.Credentials.Certificates,
			v1alpha1.CertificateCredential{Name: name, Subject: subject, URISAN: uri})
	}
}

func i32(v int32) *int32 { return &v }

func nameOf(c *expr.Consumer) string {
	if c == nil {
		return ""
	}
	return c.Name
}
