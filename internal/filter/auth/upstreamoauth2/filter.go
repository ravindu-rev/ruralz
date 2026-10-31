// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Errors of the Factory and the Filter.
var (
	errNoRegistry = errors.New("auth.upstream-oauth2: factory has no registry")
	errNoRequest  = errors.New("no outgoing request in onUpstreamRequest")
)

// Factory builds auth.upstream-oauth2 Filters on a Registry
// (filter.Factory); internal/filter/builtin constructs the Registry once
// per process and lists it among the Components.
type Factory struct {
	// Registry holds the tokens; it must be set.
	Registry *Registry
}

var _ filter.Factory = Factory{}

// Build validates env.Config and returns a Filter on the entry of the
// Policy's name and fetch parameters, sharing it with the Filters of other
// live snapshots that have them. A config of the wrong type or with a
// timeout that is not positive is RZ-CFG-005, a tokenUrl that is not an
// https URL RZ-CFG-037, and a clientSecret missing from env.Secrets
// RZ-CFG-026. A ctx that has ended is its error, unchanged and without a
// code, as is a Factory without a Registry: these are failures of the
// snapshot compile, not findings about the Revision. Build fetches
// nothing: the first request of the Policy does. Close every Filter Build
// returns, including those of a snapshot that is never published, or its
// entry keeps refreshing.
func (fa Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fa.Registry == nil {
		return nil, errNoRegistry
	}
	cfg, ok := env.Config.(*v1alpha1.AuthUpstreamOAuth2Config)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeConfig, "auth.upstream-oauth2: config is %T, want *v1alpha1.AuthUpstreamOAuth2Config", env.Config)
	}
	s, err := settingsOf(cfg)
	if err != nil {
		return nil, fmt.Errorf("auth.upstream-oauth2: %w", err)
	}
	if env.Secrets == nil {
		return nil, errcode.Errorf(CodeUnresolved, "auth.upstream-oauth2: config.clientSecret %s: no secret table", s.ref)
	}
	if _, ok := env.Secrets.Get(s.ref); !ok {
		return nil, errcode.Errorf(CodeUnresolved, "auth.upstream-oauth2: config.clientSecret %s is not resolved", s.ref)
	}
	var name string
	if env.Policy != nil {
		name = env.Policy.Metadata.Name
	}
	h := &holder{secrets: env.Secrets, log: env.Logger, age: nopGauge{}, failures: nopCounter{}}
	if h.log == nil {
		h.log = fa.Registry.log
	}
	if m := env.Metrics; m != nil {
		if m.UpstreamTokenAge != nil {
			h.age = m.UpstreamTokenAge
		}
		if m.UpstreamRefreshes != nil {
			h.failures = m.UpstreamRefreshes
		}
	}
	f := &Filter{e: fa.Registry.acquire(entryKey{policy: name, settings: s}, h), h: h}
	f.lastAge.Store(-1)
	return f, nil
}

// Filter is a compiled auth.upstream-oauth2 Policy: it puts the Policy's
// token on every attempt of the legs of its Upstreams. It is safe for
// concurrent Handle calls.
type Filter struct {
	e *entry
	h *holder
	// lastAge is the token age, in whole seconds, the Filter last
	// recorded, so concurrent requests write the gauge once per second.
	lastAge atomic.Int64
	closed  atomic.Bool
}

var (
	_ filter.Filter = (*Filter)(nil)
	_ filter.Closer = (*Filter)(nil)
)

// Handle sets "Authorization: Bearer <token>" on the outgoing request in
// onUpstreamRequest, replacing any Authorization the request carries, and
// continues; every other Phase continues untouched. A token that cannot be
// obtained within ctx (package documentation "Token registry") is
// CannotDecide with RZ-AUTH-020, as is a leg view without an outgoing
// request.
func (f *Filter) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != phase.OnUpstreamRequest {
		return filter.Next()
	}
	h := outgoingHeader(x)
	if h == nil {
		return filter.Undecided(CodeUpstreamAuth, fmt.Errorf("auth.upstream-oauth2: %w", errNoRequest))
	}
	tok, now, err := f.e.get(ctx)
	if err != nil {
		return filter.Undecided(CodeUpstreamAuth, fmt.Errorf("auth.upstream-oauth2: %w", err))
	}
	h[headerAuthorization] = []string{tok.header}
	if age := int64(now.Sub(tok.obtained) / time.Second); f.lastAge.Load() != age {
		f.lastAge.Store(age)
		f.h.age.Set(age)
	}
	return filter.Next()
}

// outgoingHeader returns the header of the leg's outgoing request: the
// Phase's message, else the Leg's request; nil when there is neither.
func outgoingHeader(x filter.Exchange) http.Header {
	if m := x.Message(); m != nil {
		if h := m.Header(); h != nil {
			return h
		}
	}
	if l := x.Leg(); l != nil && l.Request != nil {
		if l.Request.Header == nil {
			l.Request.Header = http.Header{}
		}
		return l.Request.Header
	}
	return nil
}

// Close releases the Filter's entry; the last Filter of an entry closes
// it. It is idempotent and always returns nil.
func (f *Filter) Close() error {
	if f.closed.CompareAndSwap(false, true) {
		f.e.reg.release(f.e, f.h)
	}
	return nil
}

// nopGauge and nopCounter stand in for handles a BuildEnv leaves unset.
type (
	nopGauge   struct{}
	nopCounter struct{}
)

func (nopGauge) Add(emit.Stripe, int64)    {}
func (nopGauge) Set(int64)                 {}
func (nopCounter) Add(emit.Stripe, uint64) {}
