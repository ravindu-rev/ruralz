// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/ravindu-rev/ruralz/internal/netaddr"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// EnvStateStoreURL is the fallback State Store URL variable, resolved
// through the secret resolver's env provider (foundation pack section 2).
const EnvStateStoreURL = "RURALZ_STATE_STORE_URL"

// EnvRef returns the secret reference of the fallback URL; a Revision
// without stateStore resolves it whenever the variable is set.
func EnvRef() secret.Ref {
	return secret.Ref{Provider: v1alpha1.SecretProviderEnv, Name: EnvStateStoreURL}
}

// Errors of SpecOf.
var (
	// ErrNoURL reports driver redis without a url. The schema rejects that
	// configuration with RZ-CFG-005, so validated configuration never
	// reaches this defensive check.
	ErrNoURL = errors.New("manager: driver redis needs a url")
	// ErrUnresolved reports a url the Revision's secret table does not
	// hold.
	ErrUnresolved = errors.New("manager: the State Store url is not resolved")
	// ErrDriver reports a driver other than memory or redis.
	ErrDriver = errors.New("manager: unknown State Store driver")
	// ErrTopology reports a topology other than standalone, cluster or
	// sentinel.
	ErrTopology = errors.New("manager: unknown State Store topology")
	// ErrZones reports a zone map with a host that is neither a hostname
	// nor an IP literal, or one host named twice. Stage H rejects both
	// with RZ-CFG-005, so validated configuration never reaches this
	// defensive check.
	ErrZones = errors.New("manager: invalid State Store zone map")
)

// Spec is one Revision's State Store configuration.
type Spec struct {
	// Main configures the store of Rate Limits and Quotas; Acquire sets
	// its Role.
	Main statestore.Config
	// Cache configures the Response Cache connection, Gateway
	// spec.stateStore.cache; nil shares Main.
	Cache *statestore.Config
	// MainURL and CacheURL are the secret references the redis
	// connections' URLs were read from: stateStore.url and
	// stateStore.cache.url, or EnvRef for the fallback URL; nil for the
	// memory driver and without a cache connection. The Manager watches
	// each in Secrets, so a rotated value that differs from the URL in use
	// opens a new client for its connection at once.
	MainURL, CacheURL *secret.Ref
	// Secrets is the Revision's secret table holding those references.
	Secrets secret.Store
	// Limits reports a ratelimit or quota Policy in the Revision; they use
	// Main.
	Limits bool
	// Caches reports a cache Policy in the Revision.
	Caches bool
}

// SpecOf resolves Gateway spec.stateStore, with its url values read from
// secrets, into driver configurations; Limits and Caches stay for the
// caller. Without stateStore it uses the fallback URL when secrets holds
// EnvRef, else the memory driver (SourceFallback). URL values lose
// surrounding SP, HTAB, CR and LF, as the resolver's URL check ignores
// them. Each redis connection's url reference and secrets go into the
// Spec (MainURL, CacheURL, Secrets) for the Manager's rotation watch.
// stateStore.tls reaches the main connection and stateStore.cache.tls the
// cache connection, each as references with secrets, which the driver
// reads at Open; stateStore.zones reaches a redis main connection and
// stateStore.cache.zones the cache connection, each keyed by
// netaddr.CanonicalHost (zonesOf). Errors never contain a URL.
func SpecOf(ss *v1alpha1.StateStore, secrets secret.Store) (Spec, error) {
	main := statestore.Config{Role: statestore.RoleMain, DefaultTimeout: statestore.DefaultTimeout}
	if ss == nil {
		if u, ok := urlOf(secrets, EnvRef()); ok {
			main.Driver, main.Topology, main.URL, main.Source = statestore.DriverRedis, statestore.TopologyStandalone, u, statestore.SourceEnv
			ref := EnvRef()
			return Spec{Main: main, MainURL: &ref, Secrets: secrets}, nil
		}
		main.Driver, main.Source = statestore.DriverMemory, statestore.SourceFallback
		return Spec{Main: main}, nil
	}
	main.Source = statestore.SourceGateway
	if ss.Timeout != nil {
		main.DefaultTimeout = time.Duration(*ss.Timeout)
	}
	driver := v1alpha1.StateStoreDriverMemory
	if ss.Driver != nil {
		driver = *ss.Driver
	}
	switch driver {
	case v1alpha1.StateStoreDriverMemory:
		// topology and url apply to redis only.
		main.Driver = statestore.DriverMemory
	case v1alpha1.StateStoreDriverRedis:
		var err error
		main.Driver = statestore.DriverRedis
		if main.Topology, err = topologyOf(ss.Topology); err != nil {
			return Spec{}, err
		}
		if main.URL, err = resolveURL(ss.URL, secrets); err != nil {
			return Spec{}, fmt.Errorf("stateStore.url: %w", err)
		}
		main.TLS = tlsOf(ss.TLS, secrets)
		if main.Zones, err = zonesOf(ss.Zones); err != nil {
			return Spec{}, fmt.Errorf("stateStore.zones: %w", err)
		}
	default:
		return Spec{}, fmt.Errorf("%w %q", ErrDriver, driver)
	}
	spec := Spec{Main: main}
	if main.Driver == statestore.DriverRedis {
		ref := secret.RefOf(ss.URL.SecretRef)
		spec.MainURL, spec.Secrets = &ref, secrets
	}
	if c := ss.Cache; c != nil {
		cache := statestore.Config{
			Role: statestore.RoleCache, Driver: statestore.DriverRedis,
			DefaultTimeout: main.DefaultTimeout, Source: statestore.SourceGateway,
		}
		var err error
		if cache.Topology, err = topologyOf(c.Topology); err != nil {
			return Spec{}, err
		}
		if cache.URL, err = resolveURL(&c.URL, secrets); err != nil {
			return Spec{}, fmt.Errorf("stateStore.cache.url: %w", err)
		}
		cache.TLS = tlsOf(c.TLS, secrets)
		if cache.Zones, err = zonesOf(c.Zones); err != nil {
			return Spec{}, fmt.Errorf("stateStore.cache.zones: %w", err)
		}
		ref := secret.RefOf(c.URL.SecretRef)
		spec.Cache, spec.CacheURL, spec.Secrets = &cache, &ref, secrets
	}
	return spec, nil
}

// zonesOf converts one connection's zone map (stateStore.zones,
// stateStore.cache.zones) into the driver's form: each host in
// netaddr.CanonicalHost form, the form the State client looks the hosts
// of its servers up in, so hosts compare case-insensitively and IP
// literals in canonical form. It returns nil without entries, and
// ErrZones for a host CanonicalHost refuses or one host named twice.
func zonesOf(entries []v1alpha1.StateStoreZone) (statestore.Zones, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	z := make(statestore.Zones, len(entries))
	for _, e := range entries {
		host, err := netaddr.CanonicalHost(e.Host)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrZones, err)
		}
		if _, dup := z[host]; dup {
			return nil, fmt.Errorf("%w: one host is named twice", ErrZones)
		}
		z[host] = e.Zone
	}
	return z, nil
}

// topologyOf maps a redis topology, standalone when unset.
func topologyOf(t *v1alpha1.StateStoreTopology) (statestore.Topology, error) {
	if t == nil {
		return statestore.TopologyStandalone, nil
	}
	switch *t {
	case v1alpha1.StateStoreTopologyStandalone:
		return statestore.TopologyStandalone, nil
	case v1alpha1.StateStoreTopologyCluster:
		return statestore.TopologyCluster, nil
	case v1alpha1.StateStoreTopologySentinel:
		return statestore.TopologySentinel, nil
	default:
		return 0, fmt.Errorf("%w %q", ErrTopology, *t)
	}
}

// tlsOf converts a connection's TLS block (stateStore.tls,
// stateStore.cache.tls) into a driver setting with secrets; nil without
// it.
func tlsOf(t *v1alpha1.StateStoreTLS, secrets secret.Store) *statestore.TLS {
	if t == nil {
		return nil
	}
	ref := func(v *v1alpha1.SecretValue) *secret.Ref {
		if v == nil {
			return nil
		}
		r := secret.RefOf(v.SecretRef)
		return &r
	}
	return &statestore.TLS{
		CACertificate: ref(t.CACertificate), ClientCertificate: ref(t.ClientCertificate), ClientKey: ref(t.ClientKey),
		Secrets: secrets,
	}
}

// resolveURL reads a url's value from secrets.
func resolveURL(v *v1alpha1.SecretValue, secrets secret.Store) (string, error) {
	if v == nil {
		return "", ErrNoURL
	}
	u, ok := urlOf(secrets, secret.RefOf(v.SecretRef))
	if !ok {
		return "", fmt.Errorf("%w (%s)", ErrUnresolved, secret.RefOf(v.SecretRef))
	}
	return u, nil
}

// urlOf returns the trimmed value of ref; false when secrets does not
// hold it or it is empty.
func urlOf(secrets secret.Store, ref secret.Ref) (string, bool) {
	if secrets == nil {
		return "", false
	}
	v, ok := secrets.Get(ref)
	if !ok {
		return "", false
	}
	b := v.Reveal()
	defer clear(b)
	u := string(bytes.Trim(b, " \t\r\n"))
	return u, u != ""
}
