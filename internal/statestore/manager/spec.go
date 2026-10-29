// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"bytes"
	"errors"
	"fmt"
	"time"

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
	// ErrTopology reports a topology other than standalone or cluster.
	ErrTopology = errors.New("manager: unknown State Store topology")
)

// Spec is one Revision's State Store configuration.
type Spec struct {
	// Main configures the store of Rate Limits and Quotas; Acquire sets
	// its Role.
	Main statestore.Config
	// Cache configures the Response Cache connection, Gateway
	// spec.stateStore.cache; nil shares Main.
	Cache *statestore.Config
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
// them. Errors never contain a URL.
func SpecOf(ss *v1alpha1.StateStore, secrets secret.Store) (Spec, error) {
	main := statestore.Config{Role: statestore.RoleMain, DefaultTimeout: statestore.DefaultTimeout}
	if ss == nil {
		if u, ok := urlOf(secrets, EnvRef()); ok {
			main.Driver, main.Topology, main.URL, main.Source = statestore.DriverRedis, statestore.TopologyStandalone, u, statestore.SourceEnv
		} else {
			main.Driver, main.Source = statestore.DriverMemory, statestore.SourceFallback
		}
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
	default:
		return Spec{}, fmt.Errorf("%w %q", ErrDriver, driver)
	}
	spec := Spec{Main: main}
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
		spec.Cache = &cache
	}
	return spec, nil
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
	default:
		return 0, fmt.Errorf("%w %q", ErrTopology, *t)
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
