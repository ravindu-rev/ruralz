// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// errNoProbeTarget is the probe result for an Endpoint that is no longer
// in the set or an Upstream not active yet.
var errNoProbeTarget = errors.New("upstream: probe target is not in the active Endpoint set")

// runtime is the state of one Upstream name on the Node, carried across Hot
// Reloads (docs/architecture/09-traffic-management-and-resilience.md "Load
// balancing": health, ejection and breaker state survive Hot Reloads): the
// health Tracker, the breaker, bulkhead and retry budget, the active
// Endpoint source and its published Endpoint table, the balancers built
// over it, and the TLS entries with their connection pools. Every live
// snapshot whose Revision holds the Upstream references it; the last
// reference closes it.
type runtime struct {
	m       *Manager
	name    string
	tracker *health.Tracker
	res     *resilience.Runtime
	// metrics are the active snapshot's handles; nil before the first
	// activation.
	metrics atomic.Pointer[emit.UpstreamMetrics]
	// target is what active checks send through: the active snapshot's
	// scheme and TLS entry; nil while no active snapshot holds the
	// Upstream.
	target atomic.Pointer[probeTarget]
	// pool counts the Upstream's open connections by emit.PoolIdle and
	// emit.PoolActive.
	pool [emit.NumPoolStates]atomic.Int64

	// statusMu serializes the degraded reasons and the breaker gauge; it
	// is a leaf lock (the breaker's callback takes it under the breaker's
	// mutex).
	statusMu sync.Mutex
	panicOn  bool
	budgetOn bool

	// mu guards the fields below; Manager.mu is taken before it.
	mu        sync.Mutex
	refs      int
	closed    bool
	provider  *discovery.Source
	table     *table
	balancers []*balancerEntry
	tls       map[string]*tlsEntry
}

// probeTarget is the active snapshot's Upstream as active checks reach its
// Endpoints.
type probeTarget struct {
	up     *Upstream
	scheme string
	tls    *tlsEntry
}

// balancerEntry is one balancer of a runtime: one per algorithm the live
// snapshots use (two only while a Hot Reload changes the algorithm).
type balancerEntry struct {
	alg balance.Algorithm
	bal *balance.Balancer
	// refs is guarded by the runtime's mutex.
	refs int
	// building is set while a rebuild goroutine runs.
	building atomic.Bool
}

// newRuntime returns the runtime of a new Upstream name, with st's passive
// health policy, attached to the Node's Prober. Active checks start at
// activation (SetPolicy), when the Upstream they probe through is active.
func newRuntime(m *Manager, st *settings) *runtime {
	rt := &runtime{m: m, name: st.name, table: &table{}, tls: map[string]*tlsEntry{}}
	rt.tracker = health.NewTracker(health.Config{
		Upstream:       st.name,
		Clock:          m.clk,
		Passive:        st.passive,
		MaxConnections: st.res.Bulkhead.MaxConnections,
		OnHealthy:      rt.onHealthy,
	})
	rt.res = resilience.NewRuntime(m.clk, m.rand, rt.onBreaker)
	m.prober.Attach(rt.tracker, rt.probe)
	return rt
}

// publishLocked makes ds the Endpoint set: a new table reusing the state
// of identities that remain, the health Tracker's list, then every
// balancer's, so a request that loads a balancer Set finds the Tracker
// already holding its identities. Endpoints that left retire their pools.
// rt.mu is held.
func (rt *runtime) publishLocked(ds *discovery.Set) {
	tab, gone := newTable(rt.table, ds)
	rt.table = tab
	for _, st := range gone {
		st.removed.Store(true)
		for _, p := range st.pools.Load().list() {
			p.retire()
		}
	}
	// Both lists come from one sorted discovery Set, so neither call can
	// fail on order.
	_ = rt.tracker.SetEndpoints(tab.identities())
	eps := tab.balanceEndpoints()
	for _, b := range rt.balancers {
		_ = b.bal.SetEndpoints(eps, tab)
	}
}

// onDiscovery applies a change of src's Endpoints when src is the active
// source; the Manager replans the balancer budget afterwards.
func (rt *runtime) onDiscovery(src *discovery.Source, ds *discovery.Set) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed || rt.provider != src {
		return false
	}
	rt.publishLocked(ds)
	return true
}

// balancerLocked returns the balancer entry of alg, building one over the
// current table when none exists (at compile time, off the request path).
// rt.mu is held.
func (rt *runtime) balancerLocked(alg balance.Algorithm, v int, fallback bool) (*balancerEntry, error) {
	for _, b := range rt.balancers {
		if b.alg == alg {
			return b, nil
		}
	}
	bal, err := balance.New(balance.Config{
		Algorithm:    alg,
		VirtualNodes: v,
		Fallback:     fallback,
		Endpoints:    rt.table.balanceEndpoints(),
		Payload:      rt.table,
		Rand:         rt.m.rand,
		Now:          rt.m.clk.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("upstream %q: %w", rt.name, err)
	}
	b := &balancerEntry{alg: alg, bal: bal}
	rt.balancers = append(rt.balancers, b)
	return b, nil
}

// releaseBalancerLocked drops one reference to b. rt.mu is held.
func (rt *runtime) releaseBalancerLocked(b *balancerEntry) {
	if b.refs--; b.refs > 0 {
		return
	}
	rt.balancers = slices.DeleteFunc(rt.balancers, func(x *balancerEntry) bool { return x == b })
}

// releaseTLSLocked drops one reference to e; the last one retires its
// pools and stops following its secret rotations. rt.mu is held.
func (rt *runtime) releaseTLSLocked(e *tlsEntry) {
	if e.refs--; e.refs > 0 {
		return
	}
	delete(rt.tls, e.key)
	for _, ep := range rt.table.eps {
		rt.dropPoolLocked(ep.st, e)
	}
	if e.cfg != nil {
		e.cfg.Close()
	}
}

// dropPoolLocked retires and unlists st's pool of e. rt.mu is held.
func (rt *runtime) dropPoolLocked(st *epState, e *tlsEntry) {
	l := st.pools.Load()
	p := l.find(e)
	if p == nil {
		return
	}
	next := &poolList{}
	for _, q := range l.pools {
		if q != p {
			next.pools = append(next.pools, q)
		}
	}
	st.pools.Store(next)
	p.retire()
}

// poolFor returns st's pool of e, building it on first use (once per
// Endpoint and TLS setting).
func (rt *runtime) poolFor(st *epState, e *tlsEntry) *pool {
	if p := st.pools.Load().find(e); p != nil {
		return p
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	l := st.pools.Load()
	if p := l.find(e); p != nil {
		return p
	}
	p := newPool(rt, st, e)
	if st.removed.Load() || e.refs <= 0 || rt.closed {
		// An attempt that selected the Endpoint just before it left keeps a
		// pool of its own, closed when it ends.
		p.retired.Store(true)
		return p
	}
	next := &poolList{pools: append(slices.Clip(l.list()), p)}
	st.pools.Store(next)
	return p
}

// list returns the pools of l.
func (l *poolList) list() []*pool {
	if l == nil {
		return nil
	}
	return l.pools
}

// lookup returns the epState of identity in the current table.
func (rt *runtime) lookup(identity string) (*epState, bool) {
	rt.mu.Lock()
	tab := rt.table
	rt.mu.Unlock()
	i, ok := slices.BinarySearchFunc(tab.eps, identity, func(e epInfo, id string) int {
		return cmp.Compare(e.st.identity, id)
	})
	if !ok {
		return nil, false
	}
	return tab.eps[i].st, true
}

// shutdownLocked closes the runtime once no snapshot references it: the
// Tracker and its Prober attachment, the source, every pool and TLS entry,
// and the degraded reasons it holds. rt.mu is held.
func (rt *runtime) shutdownLocked() {
	rt.closed = true
	rt.m.prober.Detach(rt.tracker)
	rt.tracker.Close()
	if rt.provider != nil {
		rt.provider.Close()
	}
	for _, ep := range rt.table.eps {
		for _, p := range ep.st.pools.Load().list() {
			p.retire()
		}
	}
	for _, e := range rt.tls {
		if e.cfg != nil {
			e.cfg.Close()
		}
	}
	clear(rt.tls)
	rt.statusMu.Lock()
	defer rt.statusMu.Unlock()
	if rt.panicOn {
		rt.m.setDegraded(catalog.ReasonUpstreamPanic, rt.name, false)
		rt.panicOn = false
	}
	if rt.budgetOn {
		rt.m.setDegraded(catalog.ReasonBalancerBudget, rt.name, false)
		rt.budgetOn = false
	}
}

// activate installs u, compiled by a newly activated snapshot: its source
// when the Revision changed it, its health thresholds and its metric
// handles, which then receive the current breaker state, degraded reasons
// and pool counts.
func (rt *runtime) activate(u *Upstream) {
	rt.mu.Lock()
	var replaced *discovery.Source
	if u.pending != nil {
		replaced = rt.provider
		rt.provider = u.pending
		u.pending = nil
		rt.publishLocked(rt.provider.Current())
	}
	src := rt.provider
	rt.mu.Unlock()

	rt.target.Store(&probeTarget{up: u, scheme: u.scheme, tls: u.tls})
	rt.metrics.Store(u.metrics)
	// The handles switch before the policy, so the first probe of a newly
	// enabled check counts on them.
	rt.tracker.SetMetrics(u.metrics)
	rt.tracker.SetPolicy(u.passive, u.active, u.res.Bulkhead.MaxConnections)
	// The activated source writes the Upstream gauge from now on; the one
	// it replaced stops writing it, then closes.
	src.SetMetrics(u.metrics)
	if replaced != nil {
		replaced.Close()
	}

	rt.statusMu.Lock()
	defer rt.statusMu.Unlock()
	m := u.metrics
	if m == nil {
		return
	}
	setGauge(m.Degraded[emit.UpDegradedPanic], rt.panicOn)
	setGauge(m.Degraded[emit.UpDegradedBalancerBudget], rt.budgetOn)
	state := rt.res.Breaker().State()
	for s := range m.BreakerState {
		setGauge(m.BreakerState[s], s == int(state))
	}
	for s := range rt.pool {
		publish(m.PoolConnections[s], &rt.pool[s])
	}
}

// setBudget raises or clears balancer_budget for the Upstream.
func (rt *runtime) setBudget(on bool) {
	rt.statusMu.Lock()
	defer rt.statusMu.Unlock()
	if on == rt.budgetOn {
		return
	}
	rt.budgetOn = on
	rt.m.setDegraded(catalog.ReasonBalancerBudget, rt.name, on)
	if m := rt.metrics.Load(); m != nil {
		setGauge(m.Degraded[emit.UpDegradedBalancerBudget], on)
	}
}

// onHealthy is the Tracker's callback: no healthy Endpoint in a non-empty
// set is panic mode, which raises upstream_panic for the Node and panic
// for the Upstream (docs/architecture/09-traffic-management-and-resilience.md
// "Health checking and outlier detection").
func (rt *runtime) onHealthy(healthy, total int) {
	rt.statusMu.Lock()
	defer rt.statusMu.Unlock()
	on := total > 0 && healthy == 0
	if on == rt.panicOn {
		return
	}
	rt.panicOn = on
	rt.m.setDegraded(catalog.ReasonUpstreamPanic, rt.name, on)
	if m := rt.metrics.Load(); m != nil {
		setGauge(m.Degraded[emit.UpDegradedPanic], on)
	}
}

// onBreaker is the breaker's change callback: it sets
// ruralz_upstream_breaker_state_info.
func (rt *runtime) onBreaker(from, to resilience.State) {
	rt.statusMu.Lock()
	defer rt.statusMu.Unlock()
	m := rt.metrics.Load()
	if m == nil {
		return
	}
	setGauge(m.BreakerState[from], false)
	setGauge(m.BreakerState[to], true)
}

// poolAdd changes one pool state count by d.
func (rt *runtime) poolAdd(state int, d int64) {
	rt.pool[state].Add(d)
	if m := rt.metrics.Load(); m != nil {
		publish(m.PoolConnections[state], &rt.pool[state])
	}
}

// poolMove moves one connection between pool states.
func (rt *runtime) poolMove(from, to int) {
	rt.pool[to].Add(1)
	rt.pool[from].Add(-1)
	if m := rt.metrics.Load(); m != nil {
		publish(m.PoolConnections[to], &rt.pool[to])
		publish(m.PoolConnections[from], &rt.pool[from])
	}
}

// publish sets g to c. Concurrent publishers each set the count they read
// and re-check it, so the last one to finish leaves the current count.
func publish(g emit.Gauge, c *atomic.Int64) {
	if g == nil {
		return
	}
	for {
		v := c.Load()
		g.Set(v)
		if c.Load() == v {
			return
		}
	}
}

// setGauge sets an info gauge to 1 or 0.
func setGauge(g emit.Gauge, on bool) {
	if g == nil {
		return
	}
	if on {
		g.Set(1)
		return
	}
	g.Set(0)
}

// probe is the runtime's health.ProbeFunc: GET path to the Endpoint on its
// pool for the active TLS setting (the Upstream's TLS settings, the egress
// guard), taking no bulkhead slot. The Prober judges the status.
func (rt *runtime) probe(ctx context.Context, p health.Probe) (int, error) {
	t := rt.target.Load()
	st, ok := rt.lookup(p.Endpoint)
	if t == nil || !ok {
		return 0, errNoProbeTarget
	}
	pl := rt.poolFor(st, t.tls)
	defer pl.done()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.scheme+"://"+st.net.Load().authority+p.Path, http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("upstream: probe request: %w", err)
	}
	// GotConn runs on this goroutine, during RoundTrip; a transport retry on
	// another connection moves the count.
	var conn *trackedConn
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		c := trackedOf(info.Conn)
		if c == nil || !c.begin() {
			return
		}
		if conn != nil {
			conn.end()
		}
		conn = c
	}}
	req = req.WithContext(httptrace.WithClientTrace(ctx, trace))
	resp, err := pl.tr.RoundTrip(req)
	if conn != nil {
		defer conn.end()
	}
	if err != nil {
		return 0, fmt.Errorf("upstream: probe: %w", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
