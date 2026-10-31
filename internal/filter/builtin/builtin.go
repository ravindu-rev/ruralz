// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package builtin is the explicit table of the built-in Filters: the
// Factory of every Policy type this release serves and the Node-wide
// Components those Factories share (filter.Registry). ruralzd builds it
// once per process with New and injects it into the snapshot compiler,
// which builds every Policy some Route's effective Filter Chain runs with
// its type's Factory, once per attachment
// (docs/architecture/03-data-plane.md "Configuration snapshots and hot
// reload"); the ruralzd supervisor runs every Component of
// filter.Registry.Components on a goroutine it owns before the first
// activation and cancels it at shutdown after the last snapshot retired.
//
// The table maps each served type of foundation pack section 10 to the
// package its name maps to (docs/engineering/02-repository-layout-and-
// conventions.md "Where Go code goes"). A type the Policy type registry
// does not serve (internal/config/registry) has no Factory: a Node refuses
// it before compiling (RZ-CFG-040).
//
// The offline checks every binary links are the separate table of
// internal/filter/builtin/checks: this package links the Filters'
// runtimes, jwx among them, so only ruralzd imports it.
package builtin

import (
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/apikey"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/basic"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwt"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/jwt/jwks"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/mtls"
	"github.com/ravindu-rev/ruralz/internal/filter/auth/upstreamoauth2"
	authzcel "github.com/ravindu-rev/ruralz/internal/filter/authz/cel"
	"github.com/ravindu-rev/ruralz/internal/filter/authz/ip"
	"github.com/ravindu-rev/ruralz/internal/filter/cache"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/revalidate"
	"github.com/ravindu-rev/ruralz/internal/filter/cors"
	"github.com/ravindu-rev/ruralz/internal/filter/header"
	"github.com/ravindu-rev/ruralz/internal/filter/quota"
	"github.com/ravindu-rev/ruralz/internal/filter/ratelimit"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/request"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/response"
	"github.com/ravindu-rev/ruralz/internal/filter/validation/jsonschema"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// CounterFunc looks up the handle of one label set of a Node-wide counter
// by metric name and label values in catalog order: the telemetry
// registry's aggregate.Registry.Counter in ruralzd.
type CounterFunc func(name string, values ...string) (emit.Counter, error)

// Options configure New. Every field may be left zero.
type Options struct {
	// Clock is the clock of every Component and Factory: JWKS refreshes,
	// token lifetimes, throttle and rate-limit buckets, cache holds and
	// leases; nil uses clock.Real().
	Clock clock.Clock
	// Logger receives the Components' logs (JWKS and token fetch failures
	// and recoveries, failed revalidation jobs, cache bypasses); nil
	// discards them.
	Logger *slog.Logger
	// Status receives the Components' degraded reasons, jwks_stale and
	// cache_revalidation_saturated; nil reports none.
	Status emit.NodeStatus
	// Counter supplies the Node-wide handles of
	// ruralz_auth_basic_refusals_total and
	// ruralz_cache_revalidation_drops_total, looked up once by New; nil
	// records neither.
	Counter CounterFunc
	// Guard is the egress guard of jwksUrl and tokenUrl fetches
	// (egress.FromEnv, RURALZ_FETCH_ALLOW); nil applies the default deny
	// table alone.
	Guard *egress.Guard
	// TLS is the client configuration of jwksUrl and tokenUrl fetches; nil
	// uses tlsconf.IdentityProvider(). It is cloned, and verification is
	// never disabled.
	TLS *tls.Config
	// Revoker is consulted by every client authentication type and on
	// every JWKS key load; nil means identity.NopRevoker, the M1 wiring.
	Revoker identity.Revoker
	// Entropy supplies the auth.basic throttle's per-process HMAC key; nil
	// uses crypto/rand.
	Entropy io.Reader
	// Rand draws the Response Cache's generation and fill lease tokens; nil
	// uses the math/rand/v2 runtime generator. New serializes its calls,
	// so any Source will do.
	Rand rand.Source
}

// New returns the table of the built-in Filters: a Factory for every
// Policy type this release serves and the Node-wide Components, each
// constructed once here and shared by the Factories that use it, in
// foundation pack section 10 row order of their types, the order the
// supervisor starts them: the JWKS manager (auth.jwt), the auth.basic
// throttle, the rate-limit key table (ratelimit), the quota denial cache,
// the Response Cache revalidation workers (cache) and the upstream token
// registry (auth.upstream-oauth2). It fails when the throttle key cannot
// be read from Options.Entropy, when Options.Counter refuses a label set,
// or when a Component or Factory cannot be built; nothing has started
// then.
func New(o Options) (*filter.Registry, error) {
	revoker := o.Revoker
	if revoker == nil {
		revoker = identity.NopRevoker{}
	}
	var src rand.Source
	if o.Rand != nil {
		src = &lockedSource{src: o.Rand}
	}
	var refusals [basic.NumRefusalCauses]emit.Counter
	var drops [revalidate.NumDropReasons]emit.Counter
	if o.Counter != nil {
		var err error
		if refusals, err = basic.RefusalCounters(o.Counter); err != nil {
			return nil, fmt.Errorf("builtin: %w", err)
		}
		if drops, err = revalidate.DropCounters(o.Counter); err != nil {
			return nil, fmt.Errorf("builtin: %w", err)
		}
	}

	keys := jwks.New(jwks.Options{
		Guard: o.Guard, TLS: o.TLS, Clock: o.Clock, Logger: o.Logger, Status: o.Status, Revoker: revoker,
	})
	throttle, err := basic.NewThrottle(basic.Options{Clock: o.Clock, Rand: o.Entropy, Refusals: refusals})
	if err != nil {
		return nil, fmt.Errorf("builtin: %w", err)
	}
	// M1 Nodes run in file mode, which has no published Node count
	// (ratelimit.FileMode, the table's default).
	limits, err := ratelimit.NewTable(o.Clock, nil, ratelimit.TableOptions{})
	if err != nil {
		return nil, fmt.Errorf("builtin: %w", err)
	}
	denials, err := quota.NewCache(quota.CacheOptions{})
	if err != nil {
		return nil, fmt.Errorf("builtin: %w", err)
	}
	workers := revalidate.New(revalidate.Options{Clock: o.Clock, Rand: src, Logger: o.Logger, Drops: drops, Status: o.Status})
	tokens := upstreamoauth2.NewRegistry(upstreamoauth2.Options{Guard: o.Guard, TLS: o.TLS, Clock: o.Clock, Logger: o.Logger})

	jwtFactory, err := jwt.NewFactory(keys, revoker)
	if err != nil {
		return nil, fmt.Errorf("builtin: %w", err)
	}
	basicFactory, err := basic.NewFactory(throttle, revoker)
	if err != nil {
		return nil, fmt.Errorf("builtin: %w", err)
	}
	cacheFactory, err := cache.NewFactory(workers, cache.Options{Clock: o.Clock, Rand: src, Logger: o.Logger})
	if err != nil {
		return nil, fmt.Errorf("builtin: %w", err)
	}

	factories := map[v1alpha1.PolicyType]filter.Factory{
		v1alpha1.PolicyTypeAuthJWT:              jwtFactory,
		v1alpha1.PolicyTypeAuthAPIKey:           apikey.NewFactory(revoker),
		v1alpha1.PolicyTypeAuthBasic:            basicFactory,
		v1alpha1.PolicyTypeAuthMTLS:             mtls.NewFactory(revoker),
		v1alpha1.PolicyTypeAuthzCEL:             authzcel.Factory{},
		v1alpha1.PolicyTypeAuthzIP:              ip.Factory{},
		v1alpha1.PolicyTypeRateLimit:            ratelimit.Factory{Table: limits},
		v1alpha1.PolicyTypeQuota:                quota.Factory{Cache: denials},
		v1alpha1.PolicyTypeValidationJSONSchema: jsonschema.Factory{},
		v1alpha1.PolicyTypeCORS:                 cors.Factory{},
		v1alpha1.PolicyTypeCache:                cacheFactory,
		v1alpha1.PolicyTypeHeaders:              header.Factory{},
		v1alpha1.PolicyTypeTransformRequest:     request.Factory{},
		v1alpha1.PolicyTypeTransformResponse:    response.Factory{},
		v1alpha1.PolicyTypeAuthUpstreamOAuth2:   upstreamoauth2.Factory{Registry: tokens},
	}
	return filter.NewRegistry(factories, keys, throttle, limits, denials, workers, tokens), nil
}

// lockedSource serializes the calls of a rand.Source that the cache
// Factory and the revalidation workers share: the workers draw from
// several goroutines.
type lockedSource struct {
	mu  sync.Mutex
	src rand.Source
}

// Uint64 returns the next value of the wrapped Source.
func (s *lockedSource) Uint64() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.src.Uint64()
}
