// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Fixture secret references and values.
var (
	mainRef  = v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/state-store-url"}
	cacheRef = v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderEnv, Name: "RURALZ_SECRET_CACHE_URL"}
)

const (
	mainURL  = "rediss://ss.internal:6380/0"
	cacheURL = "rediss://cache.internal:6380"
	envURL   = "rediss://env.internal:6380"
)

// fixtureSecrets resolves both references and the fallback variable;
// file values end with a newline, as a file secret usually does.
func fixtureSecrets() secrets {
	return secrets{
		secret.RefOf(mainRef):  mainURL + "\n",
		secret.RefOf(cacheRef): cacheURL,
		EnvRef():               " " + envURL + "\r\n",
	}
}

func ptr[T any](v T) *T { return &v }

// TestEnvRef checks that the fallback URL is RURALZ_STATE_STORE_URL
// through the env provider (foundation pack section 2).
func TestEnvRef(t *testing.T) {
	want := secret.Ref{Provider: v1alpha1.SecretProviderEnv, Name: "RURALZ_STATE_STORE_URL"}
	if EnvRef() != want {
		t.Errorf("EnvRef() = %v, want %v", EnvRef(), want)
	}
}

// TestSpecOf checks the Gateway stateStore rules: driver memory by
// default, topology standalone by default for redis, timeout 50 ms by
// default and 0 kept (no call attempted), url and topology ignored by
// memory, the cache connection as a redis connection with the main
// timeout, and without stateStore RURALZ_STATE_STORE_URL as a
// standalone redis URL, else memory as the fallback. URL values lose
// surrounding whitespace.
func TestSpecOf(t *testing.T) {
	redis, memory := v1alpha1.StateStoreDriverRedis, v1alpha1.StateStoreDriverMemory
	cluster := v1alpha1.StateStoreTopologyCluster
	ms := func(d time.Duration) *v1alpha1.Duration { return ptr(v1alpha1.Duration(d)) }
	main := func(d statestore.DriverKind, top statestore.Topology, url string, timeout time.Duration, src statestore.Source) statestore.Config {
		return statestore.Config{Role: statestore.RoleMain, Driver: d, Topology: top, URL: url, DefaultTimeout: timeout, Source: src}
	}
	tests := []struct {
		name    string
		ss      *v1alpha1.StateStore
		secrets secrets
		want    Spec
	}{
		{
			name: "no stateStore, no fallback URL", ss: nil, secrets: secrets{},
			want: Spec{Main: main(statestore.DriverMemory, 0, "", 50*time.Millisecond, statestore.SourceFallback)},
		},
		{
			name: "no stateStore, fallback URL", ss: nil, secrets: fixtureSecrets(),
			want: Spec{Main: main(statestore.DriverRedis, statestore.TopologyStandalone, envURL, 50*time.Millisecond, statestore.SourceEnv)},
		},
		{
			name: "empty stateStore is memory", ss: &v1alpha1.StateStore{}, secrets: fixtureSecrets(),
			want: Spec{Main: main(statestore.DriverMemory, 0, "", 50*time.Millisecond, statestore.SourceGateway)},
		},
		{
			name: "memory ignores url and topology",
			ss:   &v1alpha1.StateStore{Driver: &memory, Topology: &cluster, URL: &v1alpha1.SecretValue{SecretRef: mainRef}, Timeout: ms(20 * time.Millisecond)},
			want: Spec{Main: main(statestore.DriverMemory, 0, "", 20*time.Millisecond, statestore.SourceGateway)},
		},
		{
			name: "redis standalone by default", ss: &v1alpha1.StateStore{Driver: &redis, URL: &v1alpha1.SecretValue{SecretRef: mainRef}},
			secrets: fixtureSecrets(),
			want:    Spec{Main: main(statestore.DriverRedis, statestore.TopologyStandalone, mainURL, 50*time.Millisecond, statestore.SourceGateway)},
		},
		{
			name:    "redis cluster, timeout 0",
			ss:      &v1alpha1.StateStore{Driver: &redis, Topology: &cluster, URL: &v1alpha1.SecretValue{SecretRef: mainRef}, Timeout: ms(0)},
			secrets: fixtureSecrets(),
			want:    Spec{Main: main(statestore.DriverRedis, statestore.TopologyCluster, mainURL, 0, statestore.SourceGateway)},
		},
		{
			name: "cache connection",
			ss: &v1alpha1.StateStore{
				Driver: &redis, URL: &v1alpha1.SecretValue{SecretRef: mainRef}, Timeout: ms(30 * time.Millisecond),
				Cache: &v1alpha1.StateStoreCache{Topology: &cluster, URL: v1alpha1.SecretValue{SecretRef: cacheRef}},
			},
			secrets: fixtureSecrets(),
			want: Spec{
				Main: main(statestore.DriverRedis, statestore.TopologyStandalone, mainURL, 30*time.Millisecond, statestore.SourceGateway),
				Cache: &statestore.Config{
					Role: statestore.RoleCache, Driver: statestore.DriverRedis, Topology: statestore.TopologyCluster,
					URL: cacheURL, DefaultTimeout: 30 * time.Millisecond, Source: statestore.SourceGateway,
				},
			},
		},
		{
			name:    "cache connection beside memory, standalone by default",
			ss:      &v1alpha1.StateStore{Cache: &v1alpha1.StateStoreCache{URL: v1alpha1.SecretValue{SecretRef: cacheRef}}},
			secrets: fixtureSecrets(),
			want: Spec{
				Main: main(statestore.DriverMemory, 0, "", 50*time.Millisecond, statestore.SourceGateway),
				Cache: &statestore.Config{
					Role: statestore.RoleCache, Driver: statestore.DriverRedis, Topology: statestore.TopologyStandalone,
					URL: cacheURL, DefaultTimeout: 50 * time.Millisecond, Source: statestore.SourceGateway,
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SpecOf(tt.ss, tt.secrets)
			if err != nil {
				t.Fatalf("SpecOf = %v", err)
			}
			if got.Main != tt.want.Main {
				t.Errorf("Main = %+v, want %+v", redact(got.Main), redact(tt.want.Main))
			}
			switch {
			case (got.Cache == nil) != (tt.want.Cache == nil):
				t.Errorf("Cache = %v, want %v", got.Cache != nil, tt.want.Cache != nil)
			case got.Cache != nil && *got.Cache != *tt.want.Cache:
				t.Errorf("Cache = %+v, want %+v", redact(*got.Cache), redact(*tt.want.Cache))
			}
			if got.Limits || got.Caches {
				t.Error("SpecOf set the Policy flags, which are the caller's")
			}
		})
	}
}

// redact hides a URL in failure messages.
func redact(c statestore.Config) statestore.Config {
	if c.URL != "" {
		c.URL = strings.Repeat("*", len(c.URL))
	}
	return c
}

// TestSpecOfErrors checks the configurations SpecOf refuses, each without
// the URL value in its message: redis without url, a url the secret table
// does not hold, an unknown driver or topology.
func TestSpecOfErrors(t *testing.T) {
	redis := v1alpha1.StateStoreDriverRedis
	unknownDriver := v1alpha1.StateStoreDriver("etcd")
	unknownTopology := v1alpha1.StateStoreTopology("sentinel")
	missing := v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/missing"}
	tests := []struct {
		name string
		ss   *v1alpha1.StateStore
		want error
	}{
		{"redis without url", &v1alpha1.StateStore{Driver: &redis}, ErrNoURL},
		{"unresolved url", &v1alpha1.StateStore{Driver: &redis, URL: &v1alpha1.SecretValue{SecretRef: missing}}, ErrUnresolved},
		{"unknown driver", &v1alpha1.StateStore{Driver: &unknownDriver}, ErrDriver},
		{"unknown topology", &v1alpha1.StateStore{Driver: &redis, Topology: &unknownTopology, URL: &v1alpha1.SecretValue{SecretRef: mainRef}}, ErrTopology},
		{"unresolved cache url", &v1alpha1.StateStore{Cache: &v1alpha1.StateStoreCache{URL: v1alpha1.SecretValue{SecretRef: missing}}}, ErrUnresolved},
		{"unknown cache topology", &v1alpha1.StateStore{Cache: &v1alpha1.StateStoreCache{Topology: &unknownTopology, URL: v1alpha1.SecretValue{SecretRef: cacheRef}}}, ErrTopology},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SpecOf(tt.ss, fixtureSecrets())
			if !errors.Is(err, tt.want) {
				t.Fatalf("SpecOf = %v, want %v", err, tt.want)
			}
			for _, u := range []string{mainURL, cacheURL, envURL} {
				if strings.Contains(err.Error(), u) {
					t.Errorf("error %q contains a URL value", err)
				}
			}
		})
	}
	// Without a secret table every redis url is unresolved and the
	// fallback is memory.
	if _, err := SpecOf(&v1alpha1.StateStore{Driver: &redis, URL: &v1alpha1.SecretValue{SecretRef: mainRef}}, nil); !errors.Is(err, ErrUnresolved) {
		t.Errorf("SpecOf without secrets = %v, want ErrUnresolved", err)
	}
	if spec, err := SpecOf(nil, nil); err != nil || spec.Main.Source != statestore.SourceFallback {
		t.Errorf("SpecOf(nil, nil) = %+v, %v; want the memory fallback", spec.Main, err)
	}
}
