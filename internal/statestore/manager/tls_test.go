// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestSpecOfSentinel: topology sentinel reaches the main and the cache
// connection as statestore.TopologySentinel.
func TestSpecOfSentinel(t *testing.T) {
	redis, sentinel := v1alpha1.StateStoreDriverRedis, v1alpha1.StateStoreTopologySentinel
	ss := &v1alpha1.StateStore{
		Driver: &redis, Topology: &sentinel, URL: &v1alpha1.SecretValue{SecretRef: mainRef},
		Cache: &v1alpha1.StateStoreCache{Topology: &sentinel, URL: v1alpha1.SecretValue{SecretRef: cacheRef}},
	}
	got, err := SpecOf(ss, fixtureSecrets())
	if err != nil {
		t.Fatalf("SpecOf = %v", err)
	}
	if got.Main.Topology != statestore.TopologySentinel || got.Cache == nil || got.Cache.Topology != statestore.TopologySentinel {
		t.Fatalf("topologies = %d, %+v; want sentinel for both", got.Main.Topology, got.Cache)
	}
	if topologyName(statestore.TopologySentinel) != "sentinel" {
		t.Errorf("topologyName(sentinel) = %q", topologyName(statestore.TopologySentinel))
	}
}

// TestSpecOfTLS: each connection takes its own TLS block, as references
// with the Revision's secrets: stateStore.tls the main connection and
// stateStore.cache.tls the cache connection, which never falls back to
// the main block; a memory main connection takes none, and without the
// blocks both are nil.
func TestSpecOfTLS(t *testing.T) {
	redis, memory := v1alpha1.StateStoreDriverRedis, v1alpha1.StateStoreDriverMemory
	ref := func(name string) secret.Ref {
		return secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/redis/" + name}
	}
	ca, crt, key := ref("ca.crt"), ref("tls.crt"), ref("tls.key")
	cacheCA, cacheCrt, cacheKey := ref("cache-ca.crt"), ref("cache.crt"), ref("cache.key")
	value := func(r secret.Ref) *v1alpha1.SecretValue {
		return &v1alpha1.SecretValue{SecretRef: v1alpha1.SecretRef{Provider: r.Provider, Name: r.Name, Key: r.Key}}
	}
	block := &v1alpha1.StateStoreTLS{CACertificate: value(ca), ClientCertificate: value(crt), ClientKey: value(key)}
	cacheBlock := &v1alpha1.StateStoreTLS{CACertificate: value(cacheCA), ClientCertificate: value(cacheCrt), ClientKey: value(cacheKey)}
	sec := fixtureSecrets()
	check := func(t *testing.T, name string, got *statestore.TLS, ca, crt, key secret.Ref) {
		t.Helper()
		switch {
		case got == nil:
			t.Fatalf("%s TLS = nil", name)
		case got.CACertificate == nil || *got.CACertificate != ca,
			got.ClientCertificate == nil || *got.ClientCertificate != crt,
			got.ClientKey == nil || *got.ClientKey != key:
			t.Errorf("%s TLS references = %v %v %v", name, got.CACertificate, got.ClientCertificate, got.ClientKey)
		case got.Secrets == nil:
			t.Errorf("%s TLS has no secrets", name)
		}
	}
	mainURL := &v1alpha1.SecretValue{SecretRef: mainRef}

	spec, err := SpecOf(&v1alpha1.StateStore{
		Driver: &redis, URL: mainURL, TLS: block,
		Cache: &v1alpha1.StateStoreCache{URL: v1alpha1.SecretValue{SecretRef: cacheRef}, TLS: cacheBlock},
	}, sec)
	if err != nil {
		t.Fatalf("SpecOf = %v", err)
	}
	check(t, "main", spec.Main.TLS, ca, crt, key)
	check(t, "cache", spec.Cache.TLS, cacheCA, cacheCrt, cacheKey)

	spec, err = SpecOf(&v1alpha1.StateStore{
		Driver: &redis, URL: mainURL, TLS: block,
		Cache: &v1alpha1.StateStoreCache{URL: v1alpha1.SecretValue{SecretRef: cacheRef}},
	}, sec)
	if err != nil {
		t.Fatalf("SpecOf = %v", err)
	}
	check(t, "main beside a cache without tls", spec.Main.TLS, ca, crt, key)
	if spec.Cache.TLS != nil {
		t.Error("the cache connection took stateStore.tls")
	}

	spec, err = SpecOf(&v1alpha1.StateStore{
		Driver: &memory, TLS: block,
		Cache: &v1alpha1.StateStoreCache{URL: v1alpha1.SecretValue{SecretRef: cacheRef}, TLS: cacheBlock},
	}, sec)
	if err != nil {
		t.Fatalf("SpecOf = %v", err)
	}
	if spec.Main.TLS != nil {
		t.Error("the memory driver got stateStore.tls")
	}
	check(t, "cache beside memory", spec.Cache.TLS, cacheCA, cacheCrt, cacheKey)

	only := &v1alpha1.StateStoreTLS{CACertificate: value(ca)}
	spec, err = SpecOf(&v1alpha1.StateStore{Driver: &redis, URL: mainURL, TLS: only}, sec)
	if err != nil {
		t.Fatalf("SpecOf = %v", err)
	}
	if got := spec.Main.TLS; got == nil || got.CACertificate == nil || got.ClientCertificate != nil || got.ClientKey != nil {
		t.Errorf("CA-only TLS = %+v", got)
	}

	spec, err = SpecOf(&v1alpha1.StateStore{Driver: &redis, URL: mainURL, Cache: &v1alpha1.StateStoreCache{URL: v1alpha1.SecretValue{SecretRef: cacheRef}}}, sec)
	if err != nil {
		t.Fatalf("SpecOf = %v", err)
	}
	if spec.Main.TLS != nil || spec.Cache.TLS != nil {
		t.Error("TLS set without a TLS block")
	}
}

// TestTLSReferencesAreInTheKey: the driver key holds the connection's TLS
// references but not the secrets table, so a Hot Reload keeps the driver
// while the references are unchanged and opens a new one when one is
// added, changed or removed; an empty block differs from none, since a
// cleartext URL refuses it.
func TestTLSReferencesAreInTheKey(t *testing.T) {
	ca := secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/redis/ca.crt"}
	other := secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/redis/ca-2.crt"}
	crt := secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/redis/tls.crt"}
	clientKey := secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/redis/tls.key"}
	cfg := func(t *statestore.TLS) statestore.Config {
		c := redisSpec("rediss://127.0.0.1:6380").Main
		c.TLS = t
		return c
	}
	if keyOf(cfg(&statestore.TLS{CACertificate: &ca, Secrets: secrets{}})) != keyOf(cfg(&statestore.TLS{CACertificate: &ca, Secrets: fixtureSecrets()})) {
		t.Error("a new secrets table changed the key")
	}
	distinct := []*statestore.TLS{
		nil,
		{},
		{CACertificate: &ca},
		{CACertificate: &other},
		{ClientCertificate: &crt, ClientKey: &clientKey},
		{CACertificate: &ca, ClientCertificate: &crt, ClientKey: &clientKey},
		{ClientCertificate: &clientKey, ClientKey: &crt},
	}
	seen := map[key]int{}
	for i, d := range distinct {
		k := keyOf(cfg(d))
		if j, dup := seen[k]; dup {
			t.Errorf("TLS settings %d and %d share a key", j, i)
		}
		seen[k] = i
	}
	// The cache connection's key holds stateStore.cache.tls the same way.
	cache := cfg(nil)
	cache.Role = statestore.RoleCache
	withCA := cache
	withCA.TLS = &statestore.TLS{CACertificate: &ca}
	if keyOf(cache) == keyOf(withCA) {
		t.Error("stateStore.cache.tls is not in the cache connection's key")
	}

	o := &opener{}
	f := newFixture(t, o.openers())
	spec := func(ref *secret.Ref, sec secret.Store) Spec {
		s := redisSpec("rediss://127.0.0.1:6380")
		s.Main.TLS = &statestore.TLS{CACertificate: ref, Secrets: sec}
		return s
	}
	first := f.acquire(t, spec(&ca, secrets{}))
	same := f.acquire(t, spec(&ca, fixtureSecrets()))
	if n := len(o.drivers()); n != 1 || first.Main.Store() != same.Main.Store() {
		t.Fatalf("opened %d drivers; unchanged references must keep the driver", n)
	}
	changed := f.acquire(t, spec(&other, fixtureSecrets()))
	if n := len(o.drivers()); n != 2 || changed.Main.Store() == same.Main.Store() {
		t.Fatalf("opened %d drivers; a changed reference must open a new one", n)
	}
	if got := o.drivers()[1].cfg.TLS; got == nil || *got.CACertificate != other {
		t.Errorf("the new driver opened with %+v", got)
	}
}
