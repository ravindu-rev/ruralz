// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/coalesce"
	"github.com/ravindu-rev/ruralz/internal/filter/cache/revalidate"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeCache

// Codes, each registered in internal/errcode.
const (
	// CodeOnlyIfCached is a request with only-if-cached that the cache
	// cannot answer from a stored response (504).
	CodeOnlyIfCached = "RZ-RT-018"
	// CodeRange is a Range request the stored response cannot satisfy
	// (416).
	CodeRange = "RZ-RT-026"
	// CodeConfig is a config of another type.
	CodeConfig = "RZ-CFG-005"
	// CodeCEL is a config.key with a syntax, type or unavailable-variable
	// error.
	CodeCEL = "RZ-CFG-014"
	// CodeCost is a config.key whose static cost estimate exceeds the
	// ceiling.
	CodeCost = "RZ-CFG-015"
	// CodeScope is a placement in which the Policy cannot work: not at
	// Route scope, or without onRequestHeaders and onResponse.
	CodeScope = "RZ-CFG-020"
)

// Build errors without a code: wiring faults of the snapshot compiler.
var (
	// ErrNoStore reports a Build without a State Store
	// (BuildEnv.CacheStore or StateStore).
	ErrNoStore = errors.New("cache: no state store")
	// ErrNoQueue reports a Build without a post-commit queue
	// (BuildEnv.CacheEnqueuer or Enqueuer).
	ErrNoQueue = errors.New("cache: no post-commit queue")
	// ErrNoCEL reports a config.key without a CEL builder (BuildEnv.CEL).
	ErrNoCEL = errors.New("cache: no CEL builder")
	// ErrNoRevalidator reports a Factory without the Node's revalidation
	// workers.
	ErrNoRevalidator = errors.New("cache: no revalidator")
)

// Options configure a Factory. Zero fields take the targets; only tests
// set the bounds.
type Options struct {
	// Clock is the Node clock; nil uses clock.Real().
	Clock clock.Clock
	// Rand draws generation and fill lease tokens; nil uses the
	// math/rand/v2 runtime generator. The Factory serializes its calls.
	Rand rand.Source
	// Logger receives debug logs of bypasses and failed stores; nil
	// discards them.
	Logger *slog.Logger
	// HotBytes, HotHold and LearnedURIs replace the hot-entry layer's and
	// the Vary-name LRU's bounds when positive.
	HotBytes    int64
	HotHold     time.Duration
	LearnedURIs int
	// Flights configures miss coalescing (its Clock defaults to Clock).
	Flights coalesce.Options
}

// Factory builds cache Filters (filter.Factory) and holds the Response
// Cache's Node-wide state, which outlives snapshots: the revalidation
// workers (a filter.Component the supervisor runs), the miss coalescing
// table, the hot-entry layer and the Vary-name LRU. internal/filter/builtin
// constructs it once per process. It is safe for concurrent use.
type Factory struct {
	clk     clock.Clock
	log     *slog.Logger
	rv      *revalidate.Revalidator
	flights *coalesce.Table[*published]
	hot     *hotLayer
	names   *nameLRU
	states  sync.Pool
	results [emit.NumCacheResults]string

	rmu sync.Mutex
	rnd rand.Source

	// onWait, set by in-package tests before any request, is called by a
	// coalesced follower just before it waits for its leader.
	onWait func()
}

var _ filter.Factory = (*Factory)(nil)

// NewFactory returns the cache Factory over the Node's revalidation
// workers rv, which the supervisor runs.
func NewFactory(rv *revalidate.Revalidator, o Options) (*Factory, error) {
	if rv == nil {
		return nil, ErrNoRevalidator
	}
	results := catalog.CacheResults()
	if len(results) != emit.NumCacheResults {
		return nil, fmt.Errorf("cache: %s has %d results, the Filter %d", catalog.CacheRequestsTotal, len(results), emit.NumCacheResults)
	}
	f := &Factory{clk: o.Clock, log: o.Logger, rv: rv, rnd: o.Rand}
	copy(f.results[:], results)
	if f.clk == nil {
		f.clk = clock.Real()
	}
	if f.log == nil {
		f.log = slog.New(slog.DiscardHandler)
	}
	if f.rnd == nil {
		f.rnd = runtimeSource{}
	}
	hotBytes, hold, learned := int64(HotBytes), HotHold, MaxLearnedURIs
	if o.HotBytes > 0 {
		hotBytes = o.HotBytes
	}
	if o.HotHold > 0 {
		hold = o.HotHold
	}
	if o.LearnedURIs > 0 {
		learned = o.LearnedURIs
	}
	f.hot = newHotLayer(hotBytes, hold)
	f.names = newNameLRU(learned)
	fo := o.Flights
	if fo.Clock == nil {
		fo.Clock = f.clk
	}
	f.flights = coalesce.NewTable[*published](fo)
	f.states.New = func() any { return new(state) }
	return f, nil
}

// runtimeSource draws from math/rand/v2's runtime generator.
type runtimeSource struct{}

// Uint64 returns a uniform 64-bit value.
func (runtimeSource) Uint64() uint64 {
	return rand.Uint64() //nolint:gosec // G404: tokens must be unique, not unpredictable; the entry MAC protects entries.
}

// uint64 returns a nonzero random value.
func (f *Factory) uint64() uint64 {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	for {
		if v := f.rnd.Uint64(); v != 0 {
			return v
		}
	}
}

// newToken returns a fresh generation token, never the zero Token.
func (f *Factory) newToken() statestore.Token {
	var t statestore.Token
	for t.IsZero() {
		a, b := f.uint64(), f.uint64()
		for i := range 8 {
			t[i], t[8+i] = byte(a>>(8*i)), byte(b>>(8*i))
		}
	}
	return t
}

// Build compiles one cache Policy at Route scope. A config of another
// type is RZ-CFG-005, a config.key that does not compile RZ-CFG-014 or
// RZ-CFG-015 as the compiler reports it, and another scope or a Phase set
// without onRequestHeaders and onResponse RZ-CFG-020. A ctx that has ended
// is its error, unchanged. The Filter uses BuildEnv.CacheStore and
// CacheEnqueuer, else the main StateStore and Enqueuer; without either it
// is ErrNoStore or ErrNoQueue. Without a Replayer every stale entry
// revalidates on the request path.
func (f *Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var cfg *v1alpha1.CacheConfig
	switch c := env.Config.(type) {
	case nil:
		cfg = &v1alpha1.CacheConfig{}
	case *v1alpha1.CacheConfig:
		cfg = c
	case v1alpha1.CacheConfig:
		cfg = &c
	default:
		return nil, errcode.Errorf(CodeConfig, "cache: config is %T, want *v1alpha1.CacheConfig", env.Config)
	}
	if cfg == nil {
		cfg = &v1alpha1.CacheConfig{}
	}
	switch {
	case env.Scope != phase.ScopeRoute:
		return nil, errcode.Errorf(CodeScope, "cache: not allowed at %s scope (allowed: Route)", env.Scope)
	case !env.Phases.Has(phase.OnRequestHeaders) || !env.Phases.Has(phase.OnResponse):
		return nil, errcode.Errorf(CodeScope, "cache: subscribed to %v, needs onRequestHeaders and onResponse", env.Phases)
	}
	flt := &Filter{
		f: f, store: env.CacheStore, queue: env.CacheEnqueuer, timeout: env.StateStoreTimeout,
		replayer: env.Replayer, credentials: credentialFields(env), clk: f.clk, log: f.log,
	}
	if env.Clock != nil {
		flt.clk = env.Clock
	}
	if env.Logger != nil {
		flt.log = env.Logger
	}
	if flt.store == nil {
		flt.store = env.StateStore
	}
	if flt.queue == nil {
		flt.queue = env.Enqueuer
	}
	switch {
	case flt.store == nil:
		return nil, ErrNoStore
	case flt.queue == nil:
		return nil, ErrNoQueue
	}
	if env.Policy != nil {
		flt.policy = env.Policy.Metadata.Name
	}
	if env.Metrics != nil {
		flt.skipped = &env.Metrics.StoreSkipped
	}
	if cfg.Key != "" {
		if env.CEL == nil {
			return nil, ErrNoCEL
		}
		prg, iss := env.CEL.Compile(site(env), cfg.Key)
		if prg == nil {
			return nil, compileError(iss)
		}
		flt.key = prg
	}
	return flt, nil
}

// site returns config.key's site at env's attachment.
func site(env filter.BuildEnv) expr.Site {
	if env.Site != nil {
		return env.Site(expr.PlaceCacheKey)
	}
	return expr.Site{Place: expr.PlaceCacheKey, Scope: env.Scope, PolicyType: Type}
}

// compileError returns the Build error of a config.key the builder
// refused: RZ-CFG-015 for the cost ceiling, RZ-CFG-014 otherwise.
func compileError(iss []expr.Issue) error {
	if len(iss) == 0 {
		return errcode.Errorf(CodeCEL, "cache: config.key did not compile")
	}
	code := CodeCEL
	if iss[0].Code == CodeCost {
		code = CodeCost
	}
	return errcode.Errorf(code, "cache: config.key: %s", iss[0].Message)
}

// Credential fields ("Response caching": a request carrying them stores a
// credentialed entry, and its saved request keeps none of them).
const (
	fieldAuthorization      = "Authorization"
	fieldProxyAuthorization = "Proxy-Authorization"
	fieldCookie             = "Cookie"
	// defaultAPIKeyHeader is auth.api-key's config.header default.
	defaultAPIKeyHeader = "X-Api-Key"
)

// credentialFields returns the canonical names of the credential fields:
// Authorization, Proxy-Authorization, Cookie and the config.header of
// every auth.api-key Policy of the Revision (a Filter is built per Policy,
// not per Route, so it cannot tell which of them guards a request).
func credentialFields(env filter.BuildEnv) []string {
	out := []string{fieldAuthorization, fieldProxyAuthorization, fieldCookie}
	add := func(name string) {
		if c := http.CanonicalHeaderKey(name); !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	if env.Bundle == nil {
		return out
	}
	for _, p := range env.Bundle.Policies() {
		if p.Spec.Type != v1alpha1.PolicyTypeAuthAPIKey {
			continue
		}
		name := defaultAPIKeyHeader
		if r, ok := env.Bundle.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: p.Metadata.Name}); ok {
			if c, ok := r.Config.(*v1alpha1.AuthAPIKeyConfig); ok && c != nil && c.Header != nil && *c.Header != "" {
				name = *c.Header
			}
		}
		add(name)
	}
	return out
}
