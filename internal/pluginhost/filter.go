// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/pluginhost/configschema"
	"github.com/ravindu-rev/ruralz/internal/redact"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Codes of the plugin Factory's own refusals.
const (
	// codeSecret is a secret or key field this Node cannot resolve.
	codeSecret = "RZ-CFG-026" //nolint:gosec // G101: an RZ code, not a credential.
	// codeSchema is a config its Plugin's configSchema refuses.
	codeSchema = "RZ-CFG-021"
)

// Factory builds the Filter of a plugin Policy (foundation pack section
// 10): its Plugin's artifact fetched, verified, checked against
// Plugin.spec and compiled once per digest, its pool shared by every
// Policy of equal digest, configured state and limits, warmed before the
// snapshot publishes. ruralzd builds it in internal/filter/builtin with
// the Node's Engine.
type Factory struct {
	// Engine is the Node's Plugin host.
	Engine *Engine
	// Environment is the State Store namespace of the Node's Environment
	// (rzplg:<Environment>:).
	Environment string
	// Telemetry returns a Policy's Plugin telemetry; nil records none.
	Telemetry func(policy, plugin string) Telemetry
}

// Build implements filter.Factory.
func (f Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	e := f.Engine
	if e == nil {
		return nil, errors.New("pluginhost: the plugin Factory has no Engine")
	}
	pol := env.Policy
	if pol == nil || env.Bundle == nil {
		return nil, errors.New("pluginhost: the plugin Factory needs the Policy and its Bundle")
	}
	pl, ok := env.Bundle.Plugin(pol.Spec.Plugin)
	if !ok || pl == nil {
		return nil, errcode.Errorf("RZ-CFG-009", "Policy %q names no Plugin %q", pol.Metadata.Name, pol.Spec.Plugin)
	}
	if gw := env.Bundle.Gateway(); gw != nil && gw.Spec.Limits != nil && gw.Spec.Limits.MaxPluginMemoryBytes != nil {
		e.setCap(int64(*gw.Spec.Limits.MaxPluginMemoryBytes))
	} else {
		e.setCap(DefaultNodeCap)
	}
	memBytes, timeout := limitsOf(&pl.Spec)
	pages := uint32(memBytes / pageBytes) //nolint:gosec // G115: limits.memoryBytes is far below 2^48.
	caps := parseCapabilities(pl.Spec.Capabilities)
	state := caps.has(capStateRead) || caps.has(capStateWrite)
	e.mu.Lock()
	nodeCap := e.nodeCap
	e.mu.Unlock()
	n := instanceLimit(nodeCap, memBytes, state)
	if n < 1 {
		return nil, fail(CodeReservation, "Plugin %q: limits.memoryBytes %d is above its pool's share of the Node Plugin memory cap (%d bytes)", pl.Metadata.Name, memBytes, shareOf(nodeCap, state))
	}

	cfg := pluginConfig(env.Config)
	fields, err := configschema.Fields(pl.Spec.ConfigSchema)
	if err != nil {
		return nil, errcode.Wrap(codeSchema, fmt.Errorf("plugin %q configSchema: %w", pl.Metadata.Name, err))
	}
	conf, err := configureInput(fields, cfg, env.Secrets)
	if err != nil {
		return nil, errcode.Wrap(codeSecret, fmt.Errorf("plugin Policy %q: %w", pol.Metadata.Name, err))
	}

	d, art, err := e.digest(ctx, pl.Metadata.Name, &pl.Spec, pages)
	if err != nil {
		return nil, err
	}
	logger := env.Logger
	if logger == nil {
		logger = e.log
	}
	tel := Telemetry(nopTelemetry{})
	if f.Telemetry != nil {
		if t := f.Telemetry(pol.Metadata.Name, pl.Metadata.Name); t != nil {
			tel = t
		}
	}
	class := phase.ClassCustom
	if fc := pol.Spec.FilterClass; fc != nil {
		if c, ok := phase.ParseClass(*fc); ok {
			class = c
		}
	}
	creds, hideAuth := credentialsOf(env.Bundle, pol.Metadata.Name)
	clk := env.Clock
	if clk == nil {
		clk = e.clk
	}
	b := &binding{
		policy: pol.Metadata.Name, plugin: pl.Metadata.Name, class: class, caps: caps,
		creds: creds, hideAuthorization: hideAuth, limits: env.Limits,
		events: &eventNames{}, logs: &logLimiter{clk: clk}, logger: logger, tel: tel,
	}
	if state {
		b.state = &stateBinding{
			namespace: f.Environment, store: env.StateStore, enqueuer: env.Enqueuer,
			timeout: env.StateStoreTimeout, keys: newKeyRate(clk),
		}
	}
	p, err := e.pool(ctx, poolSpec{
		digest: d, art: art, pages: pages, memBytes: memBytes, timeout: timeout, state: state, n: n,
		conf: conf, binding: b, previous: previousPool(env.Previous), policy: pol.Metadata.Name,
	})
	if err != nil {
		return nil, err
	}
	return &Filter{pool: p, b: b, upstream: env.Scope == phase.ScopeUpstream}, nil
}

// limitsOf returns a Plugin's limits.memoryBytes and limits.timeout, the
// defaults when unset.
func limitsOf(spec *v1alpha1.PluginSpec) (int64, time.Duration) {
	memBytes, timeout := int64(DefaultMemoryBytes), DefaultTimeout
	if l := spec.Limits; l != nil {
		if l.MemoryBytes != nil {
			memBytes = int64(*l.MemoryBytes)
		}
		if l.Timeout != nil {
			timeout = time.Duration(*l.Timeout)
		}
	}
	return memBytes, timeout
}

// pluginConfig returns a plugin Policy's typed config as a JSON object.
func pluginConfig(c any) map[string]any {
	switch v := c.(type) {
	case *v1alpha1.PluginConfig:
		if v != nil {
			return map[string]any(*v)
		}
	case v1alpha1.PluginConfig:
		return map[string]any(v)
	case map[string]any:
		return v
	}
	return map[string]any{}
}

// credentialsOf returns the Revision's credential-header set (the fixed
// credential headers and every auth.api-key config.header), and whether
// some Upstream the Policy runs on also runs an auth.upstream-oauth2
// Policy, which sets authorization on each attempt.
func credentialsOf(b *hub.Bundle, policy string) (*redact.Set, bool) {
	var names []string
	for _, p := range b.Policies() {
		if p.Spec.Type != v1alpha1.PolicyTypeAuthAPIKey {
			continue
		}
		name := "x-api-key"
		if r, ok := b.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: p.Metadata.Name}); ok {
			if c, ok := r.Config.(*v1alpha1.AuthAPIKeyConfig); ok && c != nil && c.Header != nil && *c.Header != "" {
				name = *c.Header
			}
		}
		names = append(names, name)
	}
	hide := false
	for _, r := range b.Resources() {
		up, ok := hub.Object[v1alpha1.Upstream](r)
		if !ok || up == nil {
			continue
		}
		runs, signs := false, false
		for _, ref := range up.Spec.Policies {
			if ref.Name == policy {
				runs = true
			}
			if p, ok := b.Policy(ref.Name); ok && p.Spec.Type == v1alpha1.PolicyTypeAuthUpstreamOAuth2 {
				signs = true
			}
		}
		hide = hide || (runs && signs)
	}
	return redact.New(names...), hide
}

// previousPool returns the pool of a previous snapshot's Filter.
func previousPool(prev filter.Filter) *pool {
	if f, ok := prev.(*Filter); ok && f != nil {
		return f.pool
	}
	return nil
}

// jsonNumber returns n as a JSON number.
func jsonNumber(n int) json.Number { return json.Number(strconv.Itoa(n)) }

// Filter is a compiled plugin Policy at one attachment: its Policy binding
// and the pool its calls take instances from.
type Filter struct {
	pool *pool
	b    *binding
	// upstream marks an Upstream-scope Policy, whose per-request state is
	// request-wide, so later attempts and composition steps replay its
	// State Store result.
	upstream bool
}

var (
	_ filter.Filter   = (*Filter)(nil)
	_ filter.Finisher = (*Filter)(nil)
	_ filter.Closer   = (*Filter)(nil)
)

// Handle implements filter.Filter: one Phase call of the guest.
func (f *Filter) Handle(ctx context.Context, ph phase.Phase, x filter.Exchange) filter.Result {
	req, unlock := f.request(x)
	defer unlock()
	r := f.pool.invoke(ctx, f.b, x, ph, req)
	switch {
	case r.code != "":
		return filter.Undecided(r.code, pluginError{r.err})
	case r.result == resultRespond:
		return filter.Reply(r.resp)
	case r.result == resultEndStream:
		if ch, ok := x.(Chunker); ok {
			ch.EndStream()
		}
		return filter.Next()
	default:
		return filter.Next()
	}
}

// pluginError carries a guest call's failure as the span's error.type
// internal: Plugin failures name no request data.
type pluginError struct{ err error }

func (e pluginError) Error() string {
	if e.err == nil {
		return "plugin call failed"
	}
	return e.err.Error()
}
func (e pluginError) Unwrap() error   { return e.err }
func (pluginError) ErrorType() string { return "internal" }

// request returns the Policy's per-request state and the function that
// ends its use: the PolicyState slot of a Gateway or Route Policy, or the
// request-wide shared slot of an Upstream Policy, held under its lock, as
// aggregate legs run concurrently and serialize on it.
func (f *Filter) request(x filter.Exchange) (*perRequest, func()) {
	if f.upstream {
		s := x.SharedState()
		s.Lock()
		v := s.Value()
		r, _ := (*v).(*perRequest)
		if r == nil {
			r = &perRequest{}
			*v = r
		}
		return r, s.Unlock
	}
	slot := x.PolicyState()
	r, _ := (*slot).(*perRequest)
	if r == nil {
		r = &perRequest{}
		*slot = r
	}
	return r, func() {}
}

// Finish implements filter.Finisher: a Gateway or Route Policy's request
// context is freed after onLog.
func (f *Filter) Finish(_ context.Context, x filter.Exchange) {
	if !f.upstream {
		*x.PolicyState() = nil
	}
}

// Close implements filter.Closer: the pool closes when no live snapshot
// holds it, and the digest's code with its last pool.
func (f *Filter) Close() error {
	f.pool.e.releasePool(context.Background(), f.pool, f.b.tel)
	return nil
}

// parkedLow logs a state.* pool whose guaranteed parked bound is below 16
// and raises plugin_parked_bound_low.
func (e *Engine) parkedLow(ctx context.Context, p *pool, policy string, bound int) {
	e.log.LogAttrs(ctx, slog.LevelWarn, "plugin parked bound low",
		slog.String(catalog.KeyPolicy, policy), slog.Int(catalog.KeyBound, bound))
	if e.status != nil {
		e.status.SetDegraded(catalog.ReasonPluginParkedBoundLow, p.parkedSource(), true)
	}
}
