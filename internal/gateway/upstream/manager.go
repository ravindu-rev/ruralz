// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// ErrRunning is returned by Run while another Run is active.
var ErrRunning = errors.New("upstream: manager is already running")

// Options configures a Manager. Zero values take the defaults.
type Options struct {
	// Clock drives deadlines, backoff, ejection, probes, discovery and
	// rebuilds; nil means clock.Real().
	Clock clock.Clock
	// Rand draws Endpoint picks, backoff, breaker and discovery jitter and
	// round-robin seeds; it must be safe for concurrent use. nil means the
	// math/rand/v2 runtime generator.
	Rand rand.Source
	// Guard refuses denied destination addresses at connect time for every
	// Endpoint connection and probe (docs/architecture/08-security-and-identity.md,
	// Secrets rule 6); nil allows only the default policy.
	Guard *egress.Guard
	// Dial replaces the Guard's dialer; tests only.
	Dial discovery.DialFunc
	// Resolver performs DNS discovery lookups; nil means
	// discovery.NewResolver().
	Resolver discovery.Resolver
	// Status receives the Node degraded reasons upstream_panic,
	// balancer_budget, discovery_stale and probes_skipped and the upstream
	// cleartext hop count; nil records none.
	Status emit.NodeStatus
	// Tracer starts the ruralz.upstream.<name> span of each attempt and
	// injects trace context; nil starts no span and leaves the trace
	// context fields as the request Phases left them.
	Tracer emit.Tracer
	// BalancerBudget is the Node's balancer memory budget;
	// 0 means balance.DefaultBudget.
	BalancerBudget int64
}

// Manager holds the Node's Upstream runtimes, one per Upstream name, for
// the Node's lifetime: Compile builds a snapshot's Set over them, Activate
// switches them to it, and Close releases them. Run drives the background
// work: the active Endpoint sources, the balancer rebuilds (at most 8 at
// a time) and the active health checks.
type Manager struct {
	clk      clock.Clock
	rand     rand.Source
	dial     discovery.DialFunc
	resolver discovery.Resolver
	status   emit.NodeStatus
	tracer   emit.Tracer
	budget   int64
	prober   *health.Prober
	gate     *balance.BuildGate
	wake     chan struct{}
	running  atomic.Bool
	// legs pools the legs' working state.
	legs sync.Pool
	// compileMu serializes Compile calls, which resolve new sources
	// without holding mu.
	compileMu sync.Mutex

	// mu serializes Compile's bookkeeping, Activate, Close and replanning;
	// it is taken before any runtime's mutex and never held across a DNS
	// lookup.
	mu       sync.Mutex
	runtimes map[string]*runtime
	active   *Set
	plan     balance.Plan
}

// NewManager returns a Manager with no Upstream.
func NewManager(o Options) *Manager {
	m := &Manager{
		clk:      o.Clock,
		rand:     o.Rand,
		dial:     o.Dial,
		resolver: o.Resolver,
		status:   o.Status,
		tracer:   o.Tracer,
		budget:   o.BalancerBudget,
		gate:     balance.NewBuildGate(balance.MaxConcurrentBuilds),
		wake:     make(chan struct{}, 1),
		runtimes: map[string]*runtime{},
	}
	if m.clk == nil {
		m.clk = clock.Real()
	}
	if m.dial == nil {
		m.dial = o.Guard.Dialer(resilience.DialTimeout).DialContext
	}
	if m.tracer == nil {
		m.tracer = nopTracer{}
	}
	m.prober = health.NewProber(health.ProberConfig{Clock: m.clk, Rand: m.rand, Status: m.status})
	return m
}

// setDegraded raises or clears a Node degraded reason.
func (m *Manager) setDegraded(r catalog.Reason, source string, on bool) {
	if m.status != nil {
		m.status.SetDegraded(r, source, on)
	}
}

// poke wakes Run to start or stop sources and schedule rebuilds.
func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// Run serves until ctx ends: the active Endpoint source of every runtime
// (one goroutine each), the balancer rebuilds when due (one goroutine per
// balancer at most, 8 building at a time per Node) and the Prober. It
// returns after every goroutine it started has stopped.
func (m *Manager) Run(ctx context.Context) error {
	if !m.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer m.running.Store(false)
	var wg sync.WaitGroup
	defer wg.Wait()
	runs := map[*discovery.Source]context.CancelFunc{}
	defer func() {
		for _, stop := range runs {
			stop()
		}
	}()
	wg.Go(func() { m.prober.Run(ctx) })
	var timer clock.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		m.syncSources(ctx, &wg, runs)
		next, ok := m.rebuild(ctx, &wg)
		var due <-chan time.Time
		if ok {
			d := max(next.Sub(m.clk.Now()), 0)
			if timer == nil {
				timer = m.clk.NewTimer(d)
			} else {
				timer.Stop()
				timer.Reset(d)
			}
			due = timer.C()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-m.wake:
		case <-due:
		}
	}
}

// syncSources runs the active source of every runtime that has names to
// resolve and stops the runs of sources no runtime uses any more.
func (m *Manager) syncSources(ctx context.Context, wg *sync.WaitGroup, runs map[*discovery.Source]context.CancelFunc) {
	type want struct {
		rt  *runtime
		src *discovery.Source
	}
	var wanted []want
	m.mu.Lock()
	for _, rt := range m.runtimes {
		rt.mu.Lock()
		if !rt.closed && rt.provider != nil && rt.provider.Dynamic() {
			wanted = append(wanted, want{rt, rt.provider})
		}
		rt.mu.Unlock()
	}
	m.mu.Unlock()
	keep := make(map[*discovery.Source]bool, len(wanted))
	for _, w := range wanted {
		keep[w.src] = true
		if runs[w.src] != nil {
			continue
		}
		sctx, stop := context.WithCancel(ctx)
		runs[w.src] = stop
		rt, src := w.rt, w.src
		wg.Go(func() {
			src.Run(sctx, func(ds *discovery.Set) { m.onDiscovery(rt, src, ds) })
		})
	}
	for src, stop := range runs {
		if !keep[src] {
			stop()
			delete(runs, src)
		}
	}
}

// onDiscovery publishes a source's new Endpoint set and replans the
// balancer budget (Endpoint-set changes lower v at once on overflow and
// raise it only when it can double).
func (m *Manager) onDiscovery(rt *runtime, src *discovery.Source, ds *discovery.Set) {
	if !rt.onDiscovery(src, ds) {
		return
	}
	m.mu.Lock()
	if s := m.active; s != nil {
		m.applyPlanLocked(s, m.planLocked(s))
	}
	m.mu.Unlock()
	m.poke()
}

// rebuild starts the due balancer rebuilds and returns when the next one is
// due.
func (m *Manager) rebuild(ctx context.Context, wg *sync.WaitGroup) (time.Time, bool) {
	var entries []*balancerEntry
	m.mu.Lock()
	for _, rt := range m.runtimes {
		rt.mu.Lock()
		entries = append(entries, rt.balancers...)
		rt.mu.Unlock()
	}
	m.mu.Unlock()
	now := m.clk.Now()
	var next time.Time
	for _, b := range entries {
		if b.building.Load() {
			continue
		}
		at, ok := b.bal.NextRebuild()
		if !ok {
			continue
		}
		if at.After(now) {
			if next.IsZero() || at.Before(next) {
				next = at
			}
			continue
		}
		b.building.Store(true)
		wg.Go(func() {
			defer m.poke()
			defer b.building.Store(false)
			_, _ = b.bal.Rebuild(ctx, m.gate, m.clk.Now())
		})
	}
	return next, !next.IsZero()
}

// Input is everything one snapshot's Upstreams compile from.
type Input struct {
	// Upstreams are the Revision's Upstreams.
	Upstreams []Config
	// Secrets is the Revision's resolved secret table (tls secretRefs).
	Secrets secret.Store
	// TrustedProxies is Gateway.spec.trustedProxies, which decides whether
	// the forwarding fields of a client request are kept and appended or
	// overwritten (clientaddr.Rewrite).
	TrustedProxies []netip.Prefix
}

// Compile builds the Set of one snapshot, off the request path: each
// Upstream's settings, its runtime (created for a new name, else shared
// with the live snapshots), its TLS entry and pools, a new Endpoint source
// when the Revision changed it (resolved now and switched in at
// activation), and its balancer, planned against the Node's balancer
// budget. A Set that is never activated must still be closed.
func (m *Manager) Compile(ctx context.Context, in Input) (*Set, error) {
	sts := make([]settings, len(in.Upstreams))
	seen := make(map[string]bool, len(in.Upstreams))
	for i := range in.Upstreams {
		st, err := parseSettings(&in.Upstreams[i])
		if err != nil {
			return nil, err
		}
		if seen[st.name] {
			return nil, fmt.Errorf("upstream: Upstream %q appears twice", st.name)
		}
		seen[st.name] = true
		sts[i] = st
	}
	m.compileMu.Lock()
	defer m.compileMu.Unlock()
	s := &Set{m: m, ups: make(map[string]*Upstream, len(sts)), fwd: newForwarding(in.TrustedProxies)}
	m.mu.Lock()
	for i := range sts {
		if err := m.prepareLocked(s, &sts[i], &in.Upstreams[i], in.Secrets); err != nil {
			s.releaseLocked()
			m.mu.Unlock()
			return nil, err
		}
	}
	m.mu.Unlock()
	// New sources resolve without the Manager's lock: a DNS outage must not
	// stall rebuilds, replans or Status.
	for _, u := range s.sorted() {
		if u.pending != nil {
			u.pending.Refresh(ctx)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range s.sorted() {
		if u.fresh {
			// The first source of a new Upstream serves at once: no snapshot
			// uses the runtime yet.
			u.rt.mu.Lock()
			u.rt.provider, u.pending, u.fresh = u.pending, nil, false
			u.rt.publishLocked(u.rt.provider.Current())
			u.rt.mu.Unlock()
		}
	}
	plan := m.planLocked(s)
	for _, u := range s.sorted() {
		v, fallback := plan.VirtualNodes(u.name)
		u.rt.mu.Lock()
		b, err := u.rt.balancerLocked(u.alg, v, fallback)
		if err == nil {
			b.refs++
			u.bal = b
		}
		u.rt.mu.Unlock()
		if err != nil {
			s.releaseLocked()
			return nil, err
		}
	}
	s.plan = plan
	return s, nil
}

// prepareLocked compiles one Upstream into s: its runtime reference, TLS
// entry and, when needed, a new Endpoint source, which Compile resolves
// next. m.mu is held.
func (m *Manager) prepareLocked(s *Set, st *settings, c *Config, secrets secret.Store) error {
	rt := m.runtimes[st.name]
	fresh := rt == nil
	if fresh {
		rt = newRuntime(m, st)
		m.runtimes[st.name] = rt
	}
	u := &Upstream{
		set:      s,
		rt:       rt,
		name:     st.name,
		scheme:   st.scheme,
		alg:      st.alg,
		res:      st.res,
		passive:  st.passive,
		active:   st.active,
		source:   st.source,
		hashKey:  c.HashKey,
		metrics:  withNoops(c.Metrics),
		spanName: catalog.UpstreamSpanName(st.name),
	}
	rt.mu.Lock()
	rt.refs++
	rt.mu.Unlock()
	s.ups[st.name] = u

	e, err := rt.tlsEntry(st, secrets)
	if err != nil {
		return fmt.Errorf("upstream %q: %w", st.name, err)
	}
	u.tls = e

	rt.mu.Lock()
	cur := rt.provider
	rt.mu.Unlock()
	switch {
	case cur == nil:
		src, err := m.newSource(st, nil)
		if err != nil {
			return err
		}
		u.pending, u.fresh = src, true
	case !cur.Spec().Equal(st.source):
		src, err := m.newSource(st, cur.Current())
		if err != nil {
			return err
		}
		u.pending = src
	}
	return nil
}

// newSource builds an Endpoint source at compile time; prev is the set of
// the source it will replace (nil for a new Upstream). Compile resolves it
// once; its Run starts once it serves and waits for the delay that first
// refresh set.
func (m *Manager) newSource(st *settings, prev *discovery.Set) (*discovery.Source, error) {
	src, err := discovery.New(st.source, discovery.Options{
		Upstream: st.name,
		Clock:    m.clk,
		Resolver: m.resolver,
		Rand:     m.rand,
		Status:   m.status,
		Previous: prev,
	})
	if err != nil {
		return nil, fmt.Errorf("upstream %q: %w", st.name, err)
	}
	return src, nil
}

// tlsEntry returns the runtime's TLS entry for st, building the client
// configuration when the key is new: an unchanged TLS setting keeps its
// entry, configuration and pools across a Hot Reload.
func (rt *runtime) tlsEntry(st *settings, secrets secret.Store) (*tlsEntry, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if e := rt.tls[st.poolKey]; e != nil {
		e.refs++
		return e, nil
	}
	e := &tlsEntry{key: st.poolKey, maxConns: st.res.Bulkhead.MaxConnections}
	if st.tls != nil {
		cfg, err := tlsconf.Upstream(*st.tls, secrets)
		if err != nil {
			return nil, err
		}
		e.cfg = cfg
	}
	e.refs = 1
	rt.tls[st.poolKey] = e
	return e, nil
}

// planLocked plans the balancer budget over the Upstreams of s, keeping the
// previous plan's virtual nodes unless they no longer fit or can double
// (balance.Replan). m.mu is held.
func (m *Manager) planLocked(s *Set) balance.Plan {
	rings := map[string]int{}
	var schedules int64
	for name, u := range s.ups {
		n := u.endpointCount()
		switch u.alg {
		case balance.RingHash:
			rings[name] = n
		case balance.RoundRobin:
			schedules += balance.ScheduleBytes(n)
		case balance.LeastRequest, balance.Random:
		}
	}
	return balance.Replan(m.plan, m.budget, schedules, rings)
}

// applyPlanLocked makes plan the Node's plan and applies it to the
// balancers of s, raising balancer_budget for the rings that fall back to
// weighted random. m.mu is held.
func (m *Manager) applyPlanLocked(s *Set, plan balance.Plan) {
	m.plan = plan
	for _, u := range s.sorted() {
		if u.alg != balance.RingHash || u.bal == nil {
			u.rt.setBudget(false)
			continue
		}
		v, fallback := plan.VirtualNodes(u.name)
		u.bal.bal.SetPlan(v, fallback)
		u.rt.setBudget(fallback)
	}
}

// Status returns the state of every Upstream runtime, by name, for
// /debug/upstreams.
func (m *Manager) Status() []Status {
	m.mu.Lock()
	rts := make([]*runtime, 0, len(m.runtimes))
	for _, rt := range m.runtimes {
		rts = append(rts, rt)
	}
	m.mu.Unlock()
	slices.SortFunc(rts, func(a, b *runtime) int { return cmp.Compare(a.name, b.name) })
	out := make([]Status, 0, len(rts))
	for _, rt := range rts {
		out = append(out, rt.status())
	}
	return out
}
