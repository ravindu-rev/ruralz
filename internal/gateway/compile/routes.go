// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/router"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/forward"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ctxEvery is how many Routes the compile handles between checks of its
// context, so an expired compile deadline stops a large Revision soon.
const ctxEvery = 256

// entryKey identifies a Policy within one chain: a Policy runs at most
// once per scope of a Route.
type entryKey struct {
	policy string
	scope  phase.Scope
}

// requestOrder orders entries as a request Phase runs them: Filter class,
// then scope, then position in its scope's spec.policies (foundation pack
// 8.12).
func requestOrder(a, b hub.Entry) int {
	return cmp.Or(cmp.Compare(a.Class, b.Class), cmp.Compare(a.Scope, b.Scope), cmp.Compare(a.Position, b.Position))
}

// distinct returns the entries of phases, each Policy and scope once, in
// request order.
func distinct(phases *[phase.Count][]hub.Entry) []hub.Entry {
	var out []hub.Entry
	seen := map[entryKey]bool{}
	for ph := range phase.Count {
		for _, e := range phases[ph] {
			k := entryKey{policy: e.Policy, scope: e.Scope}
			if !seen[k] {
				seen[k] = true
				out = append(out, e)
			}
		}
	}
	slices.SortStableFunc(out, requestOrder)
	return out
}

// policyOf returns the compiled Policy of entry e at position index of
// its chain, sharing its attachment's Filter, when and handles.
func (b *build) policyOf(e hub.Entry, index int) *snapshot.Policy {
	p := &snapshot.Policy{
		Index: index, Name: e.Policy, Type: e.Type, Class: e.Class, Scope: e.Scope, Position: e.Position,
		FailureMode: e.FailureMode, FirstPhase: e.FirstPhase(), Phases: e.Phases,
		SpanName: catalog.FilterSpanName(e.Policy),
	}
	if a := b.byKey[attachKey{policy: e.Policy, scope: e.Scope, phases: e.Phases}]; a != nil {
		p.When, p.StateStoreTimeout, p.Filter, p.Consumptive, p.Metrics = a.when, a.timeout, a.filter, a.consumptive, a.metrics
	}
	return p
}

// lists returns the per-Phase arrays of phases over the compiled Policies
// by entry; a Phase without a subscriber stays nil.
func lists(phases *[phase.Count][]hub.Entry, by map[entryKey]*snapshot.Policy) [phase.Count][]*snapshot.Policy {
	var out [phase.Count][]*snapshot.Policy
	for ph := range phase.Count {
		if len(phases[ph]) == 0 {
			continue
		}
		l := make([]*snapshot.Policy, len(phases[ph]))
		for i, e := range phases[ph] {
			l[i] = by[entryKey{policy: e.Policy, scope: e.Scope}]
		}
		out[ph] = l
	}
	return out
}

// chainOf compiles a Route's effective chain: its client Policies once
// each in request order, which Index points into, its per-Phase arrays in
// the chain's execution order, and the shared leg chain of each Upstream
// it reaches.
func (b *build) chainOf(ch *hub.Chain) snapshot.Chain {
	var c snapshot.Chain
	if ch == nil {
		return c
	}
	by := map[entryKey]*snapshot.Policy{}
	for i, e := range distinct(&ch.Client) {
		p := b.policyOf(e, i)
		by[entryKey{policy: e.Policy, scope: e.Scope}] = p
		c.Policies = append(c.Policies, p)
		if p.Class == phase.ClassAuth {
			c.AuthPolicies++
		}
	}
	c.Client = lists(&ch.Client, by)
	if len(ch.Legs) > 0 {
		c.Legs = make(map[string]*snapshot.LegChain, len(ch.Legs))
		for i := range ch.Legs {
			c.Legs[ch.Legs[i].Upstream] = b.legChain(&ch.Legs[i])
		}
	}
	return c
}

// legChain returns the leg chain of leg's Upstream, compiled once per
// snapshot: an Upstream's leg Policies are its own spec.policies whatever
// the Route, so every Route shares it. Its Policies are indexed within the
// leg, whose state holds their slots.
func (b *build) legChain(leg *hub.Leg) *snapshot.LegChain {
	if lc := b.legChains[leg.Upstream]; lc != nil {
		return lc
	}
	by := map[entryKey]*snapshot.Policy{}
	for i, e := range distinct(&leg.Phases) {
		by[entryKey{policy: e.Policy, scope: e.Scope}] = b.policyOf(e, i)
	}
	lc := &snapshot.LegChain{Upstream: leg.Upstream, Phases: lists(&leg.Phases, by)}
	b.legChains[leg.Upstream] = lc
	return lc
}

// compileRoutes compiles every Route, in name order, which Index follows:
// its match.when, chain, forwarder, body needs, timeout and State Store
// bound, recording each refusal. Without an Upstream set (an Upstream was
// refused) forwarders are left out. It returns the compiled match.when of
// each Route by name, for the Routers, or ctx's error.
func (b *build) compileRoutes(ctx context.Context) (map[string]expr.Program, error) {
	routes := b.b.Routes()
	whens := make(map[string]expr.Program, len(routes))
	b.routes = make([]*snapshot.Route, len(routes))
	// index counts Routes as Route.Index, an int32 a Revision's resource
	// limit keeps far from overflowing.
	var index int32
	for i, rt := range routes {
		if i%ctxEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		name := rt.Metadata.Name
		res, _ := b.b.Get(hub.ID{Kind: v1alpha1.KindRoute, Name: name})
		if src := rt.Spec.Match.When; src != "" {
			prg, iss := b.builder.Compile(pipeline.RouteSite(rt, expr.PlaceRouteMatchWhen, ""), src)
			if prg == nil {
				if err := b.refuse(refusal{res: res, path: specPath("match", "when"), err: celError(iss)}); err != nil {
					return nil, err
				}
			}
			whens[name] = prg
		}
		ch := b.v.Chains[name]
		out := &snapshot.Route{
			Index: index, Name: name, View: b.views[name], Listeners: b.boundListeners(rt),
			Protocol: b.protocolOf(rt), Timeout: routeTimeout(rt), Chain: b.chainOf(ch), Metrics: b.plan.Route(name),
		}
		compGate := false
		if b.set != nil {
			fwd, gate, r := b.forwarderOf(rt, res)
			if r != nil {
				if err := b.refuse(*r); err != nil {
					return nil, err
				}
			}
			out.Forward, compGate = fwd, gate
		}
		out.Body = b.bodyNeeds(ch, compGate)
		out.StateStoreMax = stateStoreMax(out.Chain.Policies)
		b.routes[i] = out
		index++
	}
	return whens, nil
}

// boundListeners returns the names of the listeners a Route binds:
// spec.listeners, else every Gateway listener, in name order.
func (b *build) boundListeners(rt *v1alpha1.Route) []string {
	if len(rt.Spec.Listeners) > 0 {
		return slices.Sorted(slices.Values(rt.Spec.Listeners))
	}
	out := make([]string, len(b.gw.Spec.Listeners))
	for i, l := range b.gw.Spec.Listeners {
		out[i] = l.Name
	}
	slices.Sort(out)
	return out
}

// protocolOf returns the protocol of a Route's Upstreams, the first
// Upstream its upstreams or composition steps name; the serve check
// refuses every protocol but http in M1.
func (b *build) protocolOf(rt *v1alpha1.Route) v1alpha1.UpstreamProtocol {
	first := ""
	switch {
	case len(rt.Spec.Upstreams) > 0:
		first = rt.Spec.Upstreams[0].Name
	case rt.Spec.Composition != nil && len(rt.Spec.Composition.Steps) > 0:
		first = rt.Spec.Composition.Steps[0].Upstream
	}
	if u, ok := b.b.Upstream(first); ok {
		return u.Spec.Protocol
	}
	return v1alpha1.UpstreamProtocolHTTP
}

// routeTimeout returns the effective Route timeout: spec.timeout, else,
// and for 0, the computed default of an http Route, 15 s
// (docs/architecture/09-traffic-management-and-resilience.md "Deadlines").
func routeTimeout(rt *v1alpha1.Route) time.Duration {
	if t := rt.Spec.Timeout; t != nil && *t > 0 {
		return time.Duration(*t)
	}
	return resilience.DefaultRouteTimeout
}

// forwarderOf returns a Route's forwarder over the snapshot's Upstream
// set: the plain-upstreams forwarder, or its composition, with whether the
// composition gates the client request body. A refusal is the forwarder's
// or the composition engine's, at spec.upstreams or spec.composition.
func (b *build) forwarderOf(rt *v1alpha1.Route, res *hub.Resource) (snapshot.Forwarder, bool, *refusal) {
	name := rt.Metadata.Name
	if comp := rt.Spec.Composition; comp != nil {
		c, err := b.c.d.Composition.Compile(composition.Input{
			Route: name, Composition: comp, Methods: rt.Spec.Match.Methods,
			Upstream: func(n string) (composition.Leg, bool) {
				u, ok := b.set.Upstream(n)
				if !ok {
					return nil, false
				}
				return u, true
			},
			Builder: b.builder, Limits: b.limits,
		})
		if err != nil {
			return nil, false, &refusal{res: res, path: specPath("composition"), err: err}
		}
		return c, c.RequestGate(), nil
	}
	f, err := forward.New(name, rt.Spec.Upstreams, b.set, b.c.d.Rand)
	if err != nil {
		return nil, false, &refusal{res: res, path: specPath("upstreams"), err: err}
	}
	return f, false, nil
}

// bodyNeeds returns a Route's body decisions
// (docs/architecture/02-configuration-model.md "Body buffering and
// limits"). The request body is gated when a client Policy gates it (one
// that runs in onRequestBody, a transform.request whose config needs the
// body), when an Upstream-scope transform.request needs it, as each of its
// attempts reads the same buffered input, or when the composition does
// (compGate). The client response is gated for a client transform.response
// whose config or when reads the body. It is teed for a cache Policy's
// store.
func (b *build) bodyNeeds(ch *hub.Chain, compGate bool) snapshot.BodyNeeds {
	n := snapshot.BodyNeeds{RequestGate: compGate}
	if ch == nil {
		return n
	}
	att := func(e hub.Entry) *attachment {
		return b.byKey[attachKey{policy: e.Policy, scope: e.Scope, phases: e.Phases}]
	}
	for _, e := range distinct(&ch.Client) {
		a := att(e)
		if a == nil {
			continue
		}
		n.RequestGate = n.RequestGate || a.gatesBody
		n.ResponseGate = n.ResponseGate || a.readsResponse
		n.ResponseTee = n.ResponseTee || e.Type == v1alpha1.PolicyTypeCache
	}
	for i := range ch.Legs {
		for _, e := range distinct(&ch.Legs[i].Phases) {
			if a := att(e); a != nil && a.gatesBody {
				n.RequestGate = true
			}
		}
	}
	return n
}

// stateStoreMax returns the largest effective stateStoreTimeout of the
// State Store Policies among ps: ratelimit, quota and cache in M1
// (statestore.RouteMax).
func stateStoreMax(ps []*snapshot.Policy) time.Duration {
	var ts []time.Duration
	for _, p := range ps {
		switch p.Type {
		case v1alpha1.PolicyTypeRateLimit, v1alpha1.PolicyTypeQuota, v1alpha1.PolicyTypeCache:
			ts = append(ts, p.StateStoreTimeout)
		default:
		}
	}
	return statestore.RouteMax(ts)
}

// compileRouters compiles one Router per listener from the Routes bound
// to it, with their compiled match.when and the Route timeout their
// match.when's Route deadline counts from, recording each Router the
// Router package refuses at its listener. A Route whose match.when was
// refused, a finding already, is left out, as the Revision is rejected
// anyway.
func (b *build) compileRouters(whens map[string]expr.Program) error {
	b.routers = make(map[string]snapshot.Router, len(b.gw.Spec.Listeners))
	for _, l := range b.gw.Spec.Listeners {
		var rs []router.Route
		for i, rt := range b.b.Routes() {
			if !slices.Contains(b.routes[i].Listeners, l.Name) {
				continue
			}
			when := whens[rt.Metadata.Name]
			if rt.Spec.Match.When != "" && when == nil {
				continue
			}
			rs = append(rs, router.Route{Index: b.routes[i].Index, Name: rt.Metadata.Name, Match: &rt.Spec.Match, When: when, Timeout: b.routes[i].Timeout})
		}
		r, err := router.Compile(l.Hostnames, rs)
		if err != nil {
			path := specPath("listeners").Append(diag.Keyed("name", l.Name))
			if err := b.refuse(refusal{res: b.gwRes, path: path, err: err}); err != nil {
				return err
			}
			continue
		}
		b.routers[l.Name] = r
	}
	return nil
}
