// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Set is the Upstream layer of one snapshot: its compiled Upstreams over
// the Node's runtimes. It implements snapshot.LegRunner and io.Closer (a
// snapshot resource, closed at zero pins). It is immutable after Compile
// and safe for concurrent use.
type Set struct {
	m    *Manager
	ups  map[string]*Upstream
	fwd  forwarding
	plan balance.Plan
	// activated and closed are guarded by Manager.mu.
	activated bool
	closed    bool
}

var _ snapshot.LegRunner = (*Set)(nil)

// Upstream returns the compiled Upstream of name.
func (s *Set) Upstream(name string) (*Upstream, bool) {
	u, ok := s.ups[name]
	return u, ok
}

// RunLeg runs one leg to the named Upstream outside any Route's forwarding
// plan (snapshot.LegRunner, Response Cache revalidation). An Upstream the
// snapshot does not hold is filter.ErrNotAvailable.
func (s *Set) RunLeg(ctx context.Context, upstream string, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
	u, ok := s.ups[upstream]
	if !ok {
		return nil, unknownUpstream(upstream, filter.ErrNotAvailable)
	}
	return u.Leg(ctx, o, Target{})
}

// Warnings returns the configuration warnings of the balancer budget plan
// (OQ-traffic-management-and-resilience-23 (b): a warning, no code).
func (s *Set) Warnings() []balance.Warning { return s.plan.Warnings() }

// Activate switches the Node's runtimes to s at the snapshot swap: changed
// Endpoint sources take over, health thresholds and metric handles switch,
// the balancer budget plan applies, and the upstream cleartext hop count is
// set. Calls after the first, or after Close, do nothing.
func (s *Set) Activate() {
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.activated || s.closed {
		return
	}
	s.activated = true
	cleartext := 0
	for _, u := range s.sorted() {
		u.rt.activate(u)
		if u.tls.cfg == nil {
			cleartext++
		}
	}
	m.applyPlanLocked(s, m.planLocked(s))
	m.active = s
	if m.status != nil {
		m.status.SetCleartextHops(catalog.HopUpstream, cleartext)
	}
	m.poke()
}

// Close releases the snapshot's references; runtimes no live snapshot
// references close, and sources compiled for a Set never activated close
// too. It is idempotent and returns nil.
func (s *Set) Close() error {
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.releaseLocked()
	if m.active == s {
		m.active = nil
	}
	m.poke()
	return nil
}

// releaseLocked drops every reference s holds. Manager.mu is held.
func (s *Set) releaseLocked() {
	for _, u := range s.sorted() {
		u.releaseLocked()
	}
}

// sorted returns the Upstreams by name, so plans and activations run in a
// deterministic order.
func (s *Set) sorted() []*Upstream {
	out := make([]*Upstream, 0, len(s.ups))
	for _, u := range s.ups {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b *Upstream) int { return cmp.Compare(a.name, b.name) })
	return out
}

// Upstream is one Upstream compiled for one snapshot: the snapshot's
// settings over the Node-lifetime runtime of its name. Its Leg is the
// attempt loop the plain-upstreams forwarder and the composition engine
// call.
type Upstream struct {
	set     *Set
	rt      *runtime
	name    string
	scheme  string
	alg     balance.Algorithm
	res     resilience.Config
	passive health.PassivePolicy
	active  *health.ActivePolicy
	source  discovery.Spec
	hashKey expr.Program
	metrics *emit.UpstreamMetrics
	// spanName is ruralz.upstream.<name>.
	spanName string
	tls      *tlsEntry
	bal      *balancerEntry
	// pending is a source compiled for this snapshot and switched in at
	// activation; nil when the source is unchanged. fresh marks the first
	// source of a new runtime, which Compile installs once resolved.
	// Guarded by Manager.mu.
	pending *discovery.Source
	fresh   bool
}

// Name returns the Upstream's metadata.name.
func (u *Upstream) Name() string { return u.name }

// endpointCount returns the size of the Endpoint set the snapshot will
// serve: the compiled source's when it replaces the current one.
func (u *Upstream) endpointCount() int {
	if u.pending != nil {
		return u.pending.Current().Len()
	}
	u.rt.mu.Lock()
	defer u.rt.mu.Unlock()
	return len(u.rt.table.eps)
}

// releaseLocked drops u's references to its runtime, balancer and TLS
// entry; the last reference to the runtime closes it. Manager.mu is held.
func (u *Upstream) releaseLocked() {
	rt := u.rt
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if u.pending != nil {
		u.pending.Close()
		u.pending = nil
	}
	if u.bal != nil {
		rt.releaseBalancerLocked(u.bal)
		u.bal = nil
	}
	if u.tls != nil {
		rt.releaseTLSLocked(u.tls)
		u.tls = nil
	}
	if rt.refs--; rt.refs > 0 {
		if t := rt.target.Load(); t != nil && t.up == u {
			// The active snapshot let go of the Upstream while a snapshot not
			// yet activated holds it: nothing is active to probe through
			// until that one activates.
			rt.target.Store(nil)
			rt.tracker.SetPolicy(u.passive, nil, u.res.Bulkhead.MaxConnections)
		}
		return
	}
	rt.shutdownLocked()
	if rt.m.runtimes[rt.name] == rt {
		delete(rt.m.runtimes, rt.name)
	}
}

// nopTracer is the tracer of a Manager without one: no span, no injection.
type nopTracer struct{}

func (nopTracer) Decide(http.Header, float64, *emit.Decision) {}

func (nopTracer) StartServer(ctx context.Context, _ *emit.Decision, _ string, _ emit.ServerAttrs) (context.Context, emit.Span) {
	return ctx, nopSpan{}
}

func (nopTracer) StartRouteMatch(ctx context.Context) (context.Context, emit.Span) {
	return ctx, nopSpan{}
}

func (nopTracer) StartFilter(ctx context.Context, _ string, _ emit.FilterAttrs) (context.Context, emit.Span) {
	return ctx, nopSpan{}
}

func (nopTracer) StartUpstream(ctx context.Context, _ string, _ emit.UpstreamAttrs) (context.Context, emit.Span) {
	return ctx, nopSpan{}
}

func (nopTracer) Inject(context.Context, *emit.Decision, http.Header) {}

// nopSpan records nothing.
type nopSpan struct{}

func (nopSpan) SetAttr(string, slog.Value) {}
func (nopSpan) SpanID() [8]byte            { return [8]byte{} }
func (nopSpan) End(int, string, string)    {}
