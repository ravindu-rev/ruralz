// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// Internal bounds of the State client
// (docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes").
const (
	// MaintenanceTick is how often the maintenance goroutine checks probes,
	// memory readings, script loads and the slot map.
	MaintenanceTick = 250 * time.Millisecond
	// DefaultAdminTimeout bounds each SCRIPT LOAD, INFO memory and CLUSTER
	// SLOTS the maintenance goroutine sends.
	DefaultAdminTimeout = time.Second
	// MaxRedirects is the most MOVED or ASK replies one cluster command
	// follows.
	MaxRedirects = 3
	// minRefreshGap spaces the slot map refreshes that closed connections
	// request, and a shard's reconnects after READONLY replies.
	minRefreshGap = time.Second
)

// Options are test seams and the internal bounds; production uses
// DefaultOptions.
type Options struct {
	// RootCAs verifies rediss:// servers without a TLS block; nil means
	// the system roots, the only production setting.
	RootCAs *x509.CertPool
	// Rand draws breaker open delays and reconnect jitter; nil is
	// breaker.Uniform.
	Rand breaker.Rand
	// Tick is the maintenance period (MaintenanceTick).
	Tick time.Duration
	// AdminTimeout bounds maintenance commands (DefaultAdminTimeout).
	AdminTimeout time.Duration
}

// DefaultOptions returns the production options.
func DefaultOptions() Options {
	return Options{Tick: MaintenanceTick, AdminTimeout: DefaultAdminTimeout}
}

// clientRef holds the published rueidis client.
type clientRef struct{ c rueidis.Client }

// Driver is the redis statestore.Driver. It is safe for concurrent use.
type Driver struct {
	cfg     statestore.Config
	topo    statestore.Topology
	ep      endpoint
	clk     clock.Clock
	log     *slog.Logger
	metrics *emit.StateMetrics
	mac     *keys.MAC
	lim     statestore.Limits
	opts    Options
	gate    *statestore.InFlight
	sha     string
	// probe bounds a PING probe: the Gateway's stateStore.timeout, the
	// default of every Policy's stateStoreTimeout.
	probe time.Duration

	reg      registry
	single   *shard // standalone, sentinel: the primary
	unmapped *shard // cluster: slots without a known owner
	slots    atomic.Pointer[slotMap]

	// sentinels paces the sentinel dials (sentinel topology); its breaker
	// gates no call.
	sentinels *shard
	// sentinelMark is the rueidis Sentinel.TLSConfig, by which the dialer
	// tells a sentinel connection from a primary one; it configures no
	// handshake.
	sentinelMark *tls.Config
	// tlsc is the TLS block's configuration (stateStore.tls,
	// stateStore.cache.tls), nil without the block.
	tlsc *tlsconf.ClientConfig

	client    atomic.Pointer[clientRef]
	ready     chan struct{} // closed once a client is published
	readyOnce sync.Once

	closing chan struct{} // closed by Close
	closed  atomic.Bool
	conns   connSet

	// goroutines counts trip, script load and dial watch goroutines; idle
	// wakes Close when it reaches 0 after Close began.
	goroutines atomic.Int64
	idle       chan struct{}

	stop      context.CancelFunc
	maintDone chan struct{}
	wake      chan struct{}
	refresh   atomic.Bool
	// lastRefresh is when the maintenance goroutine last read the slot
	// map; it owns the field.
	lastRefresh time.Time

	trips sync.Pool

	skewLog, macLog, clientLog      rateLog
	evictionWarned, unlimitedWarned atomic.Bool
}

var _ statestore.Driver = (*Driver)(nil)

// Open is the statestore.Opener of the redis driver: New with
// DefaultOptions. It never dials.
func Open(ctx context.Context, cfg statestore.Config, deps statestore.Deps) (statestore.Driver, error) {
	d, err := New(ctx, cfg, deps, DefaultOptions())
	if err != nil {
		return nil, err
	}
	return d, nil
}

// New validates cfg, returns a driver and starts its maintenance
// goroutine, which creates the State client off the caller's path; Close
// stops it. ctx supplies values only. It fails with RZ-CFG-026 for a URL
// that does not parse or fit the topology, RZ-CFG-042 for one the
// transport rules refuse, the TLS block codes of tlsconf (RZ-CFG-005,
// RZ-CFG-026), and for a MAC key under 32 bytes.
func New(ctx context.Context, cfg statestore.Config, deps statestore.Deps, opts Options) (*Driver, error) {
	if cfg.Driver != statestore.DriverRedis {
		return nil, configErr(ErrWrongDriver, "")
	}
	topo := cfg.Topology
	switch topo {
	case 0:
		topo = statestore.TopologyStandalone
	case statestore.TopologyStandalone, statestore.TopologyCluster, statestore.TopologySentinel:
	default:
		return nil, configErr(ErrTopology, "unknown topology")
	}
	ep, err := parseURL(cfg.URL, topo, cfg.TLS != nil)
	if err != nil {
		return nil, err
	}
	mac, err := keys.NewMAC(deps.MACKey)
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	var tlsc *tlsconf.ClientConfig
	if t := cfg.TLS; t != nil {
		spec := tlsconf.ClientSpec{CACertificate: t.CACertificate, ClientCertificate: t.ClientCertificate, ClientKey: t.ClientKey}
		if tlsc, err = tlsconf.StateStoreClient(spec, t.Secrets); err != nil {
			field := "stateStore.tls"
			if cfg.Role == statestore.RoleCache {
				field = "stateStore.cache.tls"
			}
			return nil, fmt.Errorf("redis: %s: %w", field, err)
		}
	}
	def := DefaultOptions()
	if opts.Tick <= 0 {
		opts.Tick = def.Tick
	}
	if opts.AdminTimeout <= 0 {
		opts.AdminTimeout = def.AdminTimeout
	}
	lim := fillLimits(deps.Limits)
	clk := deps.Clock
	if clk == nil {
		clk = clock.Real()
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	gate := deps.InFlight
	if gate == nil {
		gate = statestore.NewInFlight(lim.InFlight)
	}
	probe := cfg.DefaultTimeout
	if probe <= 0 {
		probe = statestore.DefaultTimeout
	}
	d := &Driver{
		cfg: cfg, topo: topo, ep: ep, clk: clk, log: logger, metrics: deps.Metrics, mac: mac, lim: lim,
		opts: opts, gate: gate, sha: scriptSHA(), probe: probe, tlsc: tlsc,
		reg:   registry{byAddr: map[string]*shard{}},
		ready: make(chan struct{}), closing: make(chan struct{}), idle: make(chan struct{}, 1),
		maintDone: make(chan struct{}), wake: make(chan struct{}, 1),
		conns: connSet{conns: map[*trackedConn]struct{}{}},
	}
	d.trips.New = func() any { return d.newTrip() }
	switch topo {
	case statestore.TopologyCluster:
		d.unmapped = d.newShard("unmapped")
	case statestore.TopologySentinel:
		// The primary moves between servers: one shard follows it.
		seeds := ep.opt.Sentinel.MasterSet + "@" + ep.opt.InitAddress[0]
		d.single = d.newShard("primary/" + seeds)
		d.sentinels = d.newShard("sentinels/" + seeds)
		d.sentinelMark = &tls.Config{MinVersion: tls.VersionTLS12}
	default:
		d.single = d.shardAt(ep.opt.InitAddress[0])
	}
	mctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	d.stop = stop
	go d.maintain(mctx)
	return d, nil
}

// Close closes the State client and every connection, stops the
// maintenance goroutine and waits for every goroutine the driver started,
// or for ctx. Later calls fail with RZ-STS-002, like calls on a closed
// connection; later Close calls wait the same way.
func (d *Driver) Close(ctx context.Context) error {
	if d.closed.CompareAndSwap(false, true) {
		close(d.closing)
		d.stop()
		d.conns.closeAll()
	}
	select {
	case <-d.maintDone:
	case <-ctx.Done():
		return fmt.Errorf("redis: close: %w", ctx.Err())
	}
	if c := d.loadClient(); c != nil {
		c.Close()
	}
	if d.tlsc != nil {
		d.tlsc.Close()
	}
	for d.goroutines.Load() > 0 {
		select {
		case <-d.idle:
		case <-ctx.Done():
			return fmt.Errorf("redis: close: %w", ctx.Err())
		}
	}
	return nil
}

// Status reports the breakers of the shards that serve calls, their
// latest eviction policies, a redis:// URL as cleartext and a loopback URL
// without credentials as unauthenticated.
func (d *Driver) Status() statestore.Status {
	st := statestore.Status{
		Cleartext:       !d.ep.tls,
		Unauthenticated: d.ep.unauthenticated,
		EvictionPolicy:  statestore.Yes,
	}
	var buf [16]*shard
	unknown, no := false, false
	for _, sh := range d.active(buf[:0]) {
		if sh.brk.State() != breaker.Closed {
			st.BreakerNotClosed = true
		}
		if sh == d.unmapped {
			unknown = true
			continue
		}
		switch sh.mem.eviction() {
		case statestore.No:
			no = true
		case statestore.Unknown:
			unknown = true
		case statestore.Yes:
		}
	}
	switch {
	case no:
		st.EvictionPolicy = statestore.No
	case unknown:
		st.EvictionPolicy = statestore.Unknown
	}
	return st
}

// Supports reports the script command set unless a shard that serves
// calls answered SCRIPT LOAD as unknown or not permitted; M1 has no vector
// search.
func (d *Driver) Supports(c statestore.Capability) bool {
	if c&^statestore.CapScripts != 0 {
		return false
	}
	if c&statestore.CapScripts == 0 {
		return true
	}
	var buf [16]*shard
	for _, sh := range d.active(buf[:0]) {
		if sh.noScripts.Load() {
			return false
		}
	}
	return true
}

// RoundTrips is true: State Store time is excluded from gateway-added
// time.
func (d *Driver) RoundTrips() bool { return true }

// fillLimits returns l with every zero field taken from
// statestore.DefaultLimits.
func fillLimits(l statestore.Limits) statestore.Limits {
	def := statestore.DefaultLimits()
	pick := func(v, d int) int {
		if v <= 0 {
			return d
		}
		return v
	}
	dur := func(v, d time.Duration) time.Duration {
		if v <= 0 {
			return d
		}
		return v
	}
	l.InFlight = pick(l.InFlight, def.InFlight)
	l.PipelinedPerShard = pick(l.PipelinedPerShard, def.PipelinedPerShard)
	l.DedicatedPerShard = pick(l.DedicatedPerShard, def.DedicatedPerShard)
	l.BreakerWindow = dur(l.BreakerWindow, def.BreakerWindow)
	l.BreakerMinCalls = pick(l.BreakerMinCalls, def.BreakerMinCalls)
	if l.BreakerFailureRatio <= 0 {
		l.BreakerFailureRatio = def.BreakerFailureRatio
	}
	l.BreakerConnectFailures = pick(l.BreakerConnectFailures, def.BreakerConnectFailures)
	l.BreakerOpenMin = dur(l.BreakerOpenMin, def.BreakerOpenMin)
	l.BreakerOpenMax = dur(l.BreakerOpenMax, def.BreakerOpenMax)
	l.BreakerCloseAfter = pick(l.BreakerCloseAfter, def.BreakerCloseAfter)
	l.ReconnectBase = dur(l.ReconnectBase, def.ReconnectBase)
	l.ReconnectCap = dur(l.ReconnectCap, def.ReconnectCap)
	l.ConnectsPerSecond = pick(l.ConnectsPerSecond, def.ConnectsPerSecond)
	l.ShardsRefresh = dur(l.ShardsRefresh, def.ShardsRefresh)
	l.MemoryPollSlow = dur(l.MemoryPollSlow, def.MemoryPollSlow)
	l.MemoryPollFast = dur(l.MemoryPollFast, def.MemoryPollFast)
	return l
}

// stripeOf returns the request's metric stripe.
func stripeOf(rb *statestore.RequestBudget) emit.Stripe {
	if rb == nil {
		return 0
	}
	return rb.Stripe()
}

// count adds one ruralz_state_calls_total observation.
func (d *Driver) count(s emit.Stripe, label int, r statestore.Result) {
	if d.metrics == nil || label < 0 || label >= emit.NumStateOps {
		return
	}
	if c := d.metrics.Calls[label][r]; c != nil {
		c.Add(s, 1)
	}
}

// countOps adds n to ruralz_state_ops_total{kind}.
func (d *Driver) countOps(s emit.Stripe, label int, n uint64) {
	if d.metrics == nil || label < 0 || label >= emit.NumStateOps {
		return
	}
	if c := d.metrics.Ops[label]; c != nil {
		c.Add(s, n)
	}
}

// observe records one round trip's duration.
func (d *Driver) observe(s emit.Stripe, label int, elapsed time.Duration) {
	if d.metrics == nil || label < 0 || label >= emit.NumStateOps {
		return
	}
	if h := d.metrics.CallDuration[label]; h != nil {
		h.Record(s, uint64(max(elapsed, 0)))
	}
}

// callTimeout returns a round trip's timeout: the Policy timeout clamped by
// the per-request deadline and ctx's deadline; ok is false when nothing is
// left (RZ-STS-004).
func (d *Driver) callTimeout(ctx context.Context, rb *statestore.RequestBudget, policy time.Duration) (time.Duration, bool) {
	if ctx.Err() != nil {
		return 0, false
	}
	now := d.clk.Now()
	if rb != nil {
		return rb.CallTimeout(ctx, now, policy)
	}
	if dl, ok := ctx.Deadline(); ok {
		policy = min(policy, dl.Sub(now))
	}
	return policy, policy > 0
}

// goStart counts a goroutine the driver is about to start; false after
// Close.
func (d *Driver) goStart() bool {
	d.goroutines.Add(1)
	if d.closed.Load() {
		d.goEnd()
		return false
	}
	return true
}

// goEnd counts a goroutine out and wakes Close at 0.
func (d *Driver) goEnd() {
	if d.goroutines.Add(-1) == 0 && d.closed.Load() {
		select {
		case d.idle <- struct{}{}:
		default:
		}
	}
}
