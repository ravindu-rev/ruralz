// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// compile is step 10 of the validation gate: the Gateway settings, the
// metric label sets of what some Route reaches, the Upstream runtimes,
// the Filters, the Routes and the Routers. Every refusal of the step is a
// finding, so the step returns them all, sorted; ctx's error or a fault
// ends it at once.
func (b *build) compile(ctx context.Context) (diag.List, error) {
	b.prepare()
	plan, err := b.c.d.Meter.Admit(b.shape())
	if err != nil {
		return nil, fmt.Errorf("compile: admit the metric label sets: %w", err)
	}
	b.plan = plan
	if err := b.compileGateway(); err != nil {
		return nil, err
	}
	cfgs, ok, err := b.upstreamConfigs()
	if err != nil {
		return nil, err
	}
	if err := b.buildFilters(ctx); err != nil {
		return nil, err
	}
	if ok {
		set, err := b.c.d.Upstreams.Compile(ctx, upstream.Input{Upstreams: cfgs, Secrets: b.secrets, TrustedProxies: b.gateway.TrustedProxies})
		switch {
		case err == nil:
			b.set, b.opened.set = set, set
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return nil, err
		default:
			// upstream.Check passed every Upstream: a refusal left is
			// reported at the Gateway, its message naming the Upstream.
			if err := b.refuse(refusal{res: b.gwRes, path: specPath(), err: err}); err != nil {
				return nil, err
			}
		}
	}
	b.legChains = map[string]*snapshot.LegChain{}
	whens, err := b.compileRoutes(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.compileRouters(whens); err != nil {
		return nil, err
	}
	if len(b.findings) > 0 {
		b.findings.Sort()
		// A Policy refused alike at two attachments is one finding.
		return slices.CompactFunc(b.findings, sameFinding), nil
	}
	b.invalidators()
	b.listeners = b.compileListeners()
	b.warnings = b.warningsOf()
	b.upstreamConns = b.connectionsOf()
	return nil, nil
}

// sameFinding reports whether two findings read the same.
func sameFinding(a, b diag.Diagnostic) bool { return bytes.Equal(a.AppendText(nil), b.AppendText(nil)) }

// upstreamConfigs returns the configuration of every reached Upstream,
// with its compiled hashKey, retryOn and failureWhen and its metric
// handles, checked as the Upstream layer would compile it
// (upstream.Check). ok is false when it recorded a refusal, which leaves
// the Upstream set uncompiled.
func (b *build) upstreamConfigs() (cfgs []upstream.Config, ok bool, err error) {
	ok = true
	for _, name := range b.upstreams {
		res, _ := b.b.Get(hub.ID{Kind: v1alpha1.KindUpstream, Name: name})
		u, isUp := hub.Object[v1alpha1.Upstream](res)
		if !isUp || u == nil {
			return nil, false, fmt.Errorf("compile: Upstream %q has no typed view", name)
		}
		cfg := upstream.Config{Upstream: u, Metrics: b.plan.Upstream(name)}
		compiled := true
		program := func(place expr.PlaceID, src string, path diag.Path) expr.Program {
			prg, iss := b.builder.Compile(expr.Site{Place: place}, src)
			if prg == nil {
				compiled = false
				if rerr := b.refuse(refusal{res: res, path: path, err: celError(iss)}); rerr != nil {
					err = rerr
				}
			}
			return prg
		}
		if lb := u.Spec.LoadBalancing; lb != nil && lb.HashKey != "" {
			cfg.HashKey = program(expr.PlaceHashKey, lb.HashKey, specPath("loadBalancing", "hashKey"))
		}
		if r := u.Spec.Retries; r != nil && r.RetryOn != "" {
			cfg.RetryOn = program(expr.PlaceRetryOn, r.RetryOn, specPath("retries", "retryOn"))
		}
		if cb := u.Spec.CircuitBreaker; cb != nil && cb.FailureWhen != "" {
			cfg.FailureWhen = program(expr.PlaceFailureWhen, cb.FailureWhen, specPath("circuitBreaker", "failureWhen"))
		}
		if err != nil {
			return nil, false, err
		}
		if !compiled {
			ok = false
			continue
		}
		if cerr := upstream.Check(&cfg, b.secrets); cerr != nil {
			ok = false
			if rerr := b.refuse(refusal{res: res, path: specPath(), err: cerr}); rerr != nil {
				return nil, false, rerr
			}
			continue
		}
		cfgs = append(cfgs, cfg)
	}
	return cfgs, ok, nil
}

// result assembles the compiled snapshot. Its Resources hold the Upstream
// set and every Filter that holds resources, those the previous snapshot
// shares included, so each stays open while a snapshot holding it lives;
// the retirer closes each instance once (internal/gateway/retire).
func (b *build) result() *Compiled {
	hs := b.opened.handles
	res := []io.Closer{b.set}
	res = append(res, b.closers()...)
	s := &snapshot.Snapshot{
		Revision: b.v.Revision, Validated: b.v, Gateway: b.gateway, Listeners: b.listeners,
		Routers: b.routers, Routes: b.routes, Consumers: b.consumers, Programs: b.builder.Build(),
		Metrics: b.plan, LegChains: b.legChains, Legs: b.set,
		StateStore: hs.Main, CacheStore: hs.Cache, Resources: res,
	}
	return &Compiled{
		Snapshot: s, Secrets: b.secrets, Upstreams: b.set, Warnings: b.warnings,
		UpstreamConnections: b.upstreamConns, resolver: b.c.d.Secrets, opened: b.opened,
	}
}
