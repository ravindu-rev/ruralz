// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package precedence is stage I of the configuration pipeline: it computes
// the effective Filter Chain (hub.Chain) of every Route from the Gateway,
// Route and Upstream attachments of a validated Bundle, and it is the only
// stage that raises RZ-CFG-018, RZ-CFG-019, RZ-CFG-020, RZ-CFG-029 and
// RZ-CFG-038 (architecture R-46; spec 02 section 2.7, requirements 35 to
// 42; foundation pack section 8.12).
//
// For each Route the chain follows the resolution rules of
// docs/architecture/02-configuration-model.md "Attachment and precedence":
//
//  1. The Gateway's spec.policies minus the Route's spec.excludePolicies;
//     excluding a Gateway Policy with overridable: false is RZ-CFG-019.
//  2. Plus the Route's spec.policies: a Route Policy in the slot of a
//     remaining Gateway Policy replaces it (RZ-CFG-019 when that Policy has
//     overridable: false), other slots stack.
//  3. One Policy per slot per scope: two entries of one spec.policies list
//     with equal slots are RZ-CFG-018 at the second. The rule applies to
//     the Gateway list once, to each Route list and to each Upstream list.
//  4. Upstream-scoped Policies form one set per Upstream, run only in
//     upstream-leg Phases and take no part in slot comparison. A type at a
//     disallowed scope is RZ-CFG-020 at the attaching entry
//     (registry.Registry.Phases decides, including Phase selection for
//     authz.cel through the injected body oracle, headers, transform.* and
//     plugin).
//  5. Within a Phase: Filter class, then scope (Gateway, Route, Upstream),
//     then zero-based position in that scope's spec.policies; response
//     Phases (and onChunk, spec 02 R-12) in exact reverse; onLog in request
//     order.
//
// A chain that holds a cache Policy and an onRequestBody Policy of class
// authz or validation, or a plugin Policy of class auth or authz in
// onRequestBody, is RZ-CFG-038 at the cache attachment
// (OQ-traffic-management-and-resilience-21 (a), spec 02 req 39). A Route's
// legs are its Upstreams in order of first appearance in spec.upstreams
// (by name, as map sorting leaves them) or spec.composition.steps
// (authored order), each once (req 41). failureMode open on a closed-only
// Policy is RZ-CFG-029, raised once per Policy whether attached or not
// (CheckPolicies, req 37).
//
// References that did not resolve (a missing Policy, Plugin or Upstream:
// RZ-CFG-009, raised at stage H) are skipped without further diagnostics,
// and every other attachment and Route is still processed, so one run
// reports every error (req 40). The chains are the single source of the
// data plane's per-Phase arrays and of render --effective (Rows), so the two
// cannot diverge (req 42, 04 req 40).
//
// A Resolver is immutable after New and safe for concurrent use. Resolve
// fans work out to a bounded worker pool that it owns and waits for; no
// goroutine outlives the call.
package precedence

import (
	"context"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// RZ codes this stage raises (architecture R-46); every one is registered
// in internal/errcode and defined in docs/architecture/02-configuration-model.md.
const (
	// CodeSlot is RZ-CFG-018: two Policies in one slot at one scope.
	CodeSlot = "RZ-CFG-018"
	// CodeOverridable is RZ-CFG-019: excluding or replacing a Gateway
	// Policy with overridable: false.
	CodeOverridable = "RZ-CFG-019"
	// CodeScope is RZ-CFG-020: a Policy type, or a Plugin's Phases, not
	// allowed at the attaching scope.
	CodeScope = registry.CodeScope
	// CodeFailureMode is RZ-CFG-029: failureMode open on a closed-only
	// Policy.
	CodeFailureMode = registry.CodeFailureMode
	// CodeCacheGuardrail is RZ-CFG-038: a cache Policy combined with an
	// onRequestBody authorization, validation or Plugin auth or authz
	// Policy in one effective chain.
	CodeCacheGuardrail = "RZ-CFG-038"
)

// Options configures a Resolver.
type Options struct {
	// Body decides whether an authz.cel rule references request.body,
	// which places the Policy in onRequestBody instead of onRequestHeaders
	// (02 req 13a). Every binary passes its CEL compiler (expr.Compiler
	// implements registry.BodyOracle). When Body is nil and an attached
	// authz.cel Policy has a rule, Resolve fails with
	// registry.ErrNoBodyOracle (a programming error, never a guess).
	Body registry.BodyOracle
	// Files maps tree positions to file names for diagnostic locations and
	// hub.Entry.Ref. When nil, a position in a resource's own file is
	// reported with hub.Resource.Source.File and any other position (an
	// overlay-added node) at the resource's start.
	Files *tree.FileTable
}

// Resolver computes effective chains. It is immutable after New and safe
// for concurrent use.
type Resolver struct {
	reg  *registry.Registry
	body registry.BodyOracle
	loc  locator
}

// New returns a Resolver over reg; a nil reg uses registry.New().
func New(reg *registry.Registry, o Options) *Resolver {
	if reg == nil {
		reg = registry.New()
	}
	return &Resolver{reg: reg, body: o.Body, loc: locator{files: o.Files}}
}

// Run is stage I: CheckPolicies and Resolve, with one sorted diagnostic
// list. The error is non-nil only for a programming error (see Resolve)
// or when ctx ends.
func (r *Resolver) Run(ctx context.Context, b *hub.Bundle, workers int) (map[string]*hub.Chain, diag.List, error) {
	chains, ds, err := r.Resolve(ctx, b, workers)
	if err != nil {
		return nil, nil, err
	}
	out := append(r.CheckPolicies(b), ds...)
	out.Sort()
	return chains, out, nil
}

// CheckPolicies reports failureMode open on every closed-only Policy of b
// as RZ-CFG-029 at spec.failureMode, once per Policy whether it is
// attached or not (02 req 37; foundation pack section 8.10): auth.*
// (auth.upstream-* included), authz.* and a plugin Policy whose
// filterClass is auth or authz. The list is sorted.
func (r *Resolver) CheckPolicies(b *hub.Bundle) diag.List {
	if b == nil {
		return nil
	}
	var out diag.List
	for _, res := range b.Resources() {
		if res.Kind != v1alpha1.KindPolicy {
			continue
		}
		p, ok := hub.Object[v1alpha1.Policy](res)
		if !ok {
			continue
		}
		d, bad := r.reg.CheckFailureMode(p)
		if !bad {
			continue
		}
		d.Resource = res.ResourceID()
		d.Location = r.loc.path(res, d.Path, false)
		out = append(out, d)
	}
	out.Sort()
	return out
}

// Resolve computes the effective chain of every Route of b, keyed by
// Route name, with the RZ-CFG-018, RZ-CFG-019, RZ-CFG-020 and RZ-CFG-038
// findings, sorted. The Gateway list and every Upstream list are checked
// once, whether or not a Route reaches them; each Route list once per
// Route. Chains share their leg slices (one per Upstream) and must be
// treated as read-only.
//
// Work is split over at most workers goroutines (one unit per attachment
// selection, Upstream or Route; workers below 2 run inline), and the
// result never depends on scheduling. Resolve fails, with no result, when
// ctx ends (ctx.Err()) or on a programming error that earlier stages
// exclude: a Policy type the registry does not list
// (registry.ErrUnknownType), an authz.cel rule without Options.Body
// (registry.ErrNoBodyOracle) or a hub.Resource.Config whose type is not
// the Policy type's config type.
func (r *Resolver) Resolve(ctx context.Context, b *hub.Bundle, workers int) (map[string]*hub.Chain, diag.List, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if b == nil {
		return map[string]*hub.Chain{}, nil, nil
	}
	s := newState(r, b)
	if err := s.selectAll(ctx, workers); err != nil {
		return nil, nil, err
	}
	s.gateway = s.attachGateway()
	if err := s.attachUpstreams(ctx, workers); err != nil {
		return nil, nil, err
	}
	results := make([]routeResult, len(s.routes))
	err := forEach(ctx, workers, len(s.routes), func(i int) {
		results[i] = s.route(s.routes[i])
	})
	if err != nil {
		return nil, nil, err
	}
	chains := make(map[string]*hub.Chain, len(s.routes))
	out := slices.Clone(s.gateway.diags)
	for _, u := range s.upstreamOrder {
		out = append(out, s.upstreams[u].diags...)
	}
	for _, res := range results {
		chains[res.chain.Route] = res.chain
		out = append(out, res.diags...)
	}
	out.Sort()
	return chains, out, nil
}
