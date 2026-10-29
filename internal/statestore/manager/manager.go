// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/postcommit"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// DefaultStatusInterval is how often the Manager polls driver Status for
// the breaker, eviction-policy and credentials reasons and the cleartext
// hop count (docs/architecture/11-scalability-and-distributed-state.md
// "State client and RZ-STS error codes": every 1 s).
const DefaultStatusInterval = time.Second

// statusSource holds the State Store degraded reasons the Manager raises.
const statusSource = "statestore"

// Errors of Acquire.
var (
	// ErrClosed reports Acquire after Close.
	ErrClosed = errors.New("manager: closed")
	// ErrNoOpener reports a driver without an opener.
	ErrNoOpener = errors.New("manager: no opener for the State Store driver")
)

// Openers construct the drivers. Wiring injects them, so the Manager links
// neither driver.
type Openers struct {
	// Memory opens the memory driver (memory.Open).
	Memory statestore.Opener
	// Redis opens the redis driver.
	Redis statestore.Opener
}

// Config configures a Manager.
type Config struct {
	// Deps go to every driver and give the queue its clock, metrics and
	// logger. A nil Clock is the real clock, a nil Logger discards, zero
	// Limits are statestore.DefaultLimits; a nil Status records nothing.
	Deps statestore.Deps
	// Openers open the drivers.
	Openers Openers
	// Queue sizes the post-commit queue; zero fields take the defaults.
	Queue postcommit.Limits
	// StatusInterval is the Status poll period; 0 is
	// DefaultStatusInterval.
	StatusInterval time.Duration
}

// Handles are one snapshot's State Store handles.
type Handles struct {
	// Main serves Rate Limits and Quotas, and the Response Cache without a
	// cache connection.
	Main *Handle
	// Cache serves the Response Cache; it is Main without a cache
	// connection.
	Cache *Handle
}

// Release releases both handles, a shared one once: the compile step's
// undo for a snapshot that is never published.
func (hs Handles) Release() {
	if hs.Main != nil {
		hs.Main.Release()
	}
	if hs.Cache != nil {
		hs.Cache.Release()
	}
}

// Handle is a snapshot's reference to a State Store driver: Store and
// Enqueuer are valid until Release, which the snapshot calls once when
// it is freed. Later Release calls do nothing.
type Handle struct {
	m *Manager
	e *entry
	// fallback, limits and shared record what this snapshot's
	// Revision uses (degraded reasons and warnings).
	fallback, limits, shared bool
	released                 atomic.Bool
}

// Store returns the driver.
func (h *Handle) Store() statestore.Store { return h.e.driver }

// Enqueuer returns the post-commit queue's Enqueuer for the driver.
func (h *Handle) Enqueuer() statestore.Enqueuer { return h.e.bind }

// Release drops the snapshot's reference; the driver closes once no
// snapshot and no queued write holds it.
func (h *Handle) Release() {
	if !h.released.CompareAndSwap(false, true) {
		return
	}
	m := h.m
	m.mu.Lock()
	m.countLocked(h, -1)
	m.mu.Unlock()
	// Release has no caller context; the driver close inherits nothing.
	h.e.drop(context.Background())
	m.refresh()
}

// key identifies a shareable driver: memory by role only, redis by role,
// topology, URL and the stateStore.tls references. Rotations of the
// referenced values keep the driver, which follows them itself.
type key struct {
	role     statestore.Role
	driver   statestore.DriverKind
	topology statestore.Topology
	url      string
	tls      tlsKey
}

// tlsKey is the comparable form of a statestore.TLS: whether it is set,
// and its references, zero when unset.
type tlsKey struct {
	set                 bool
	ca, cert, clientKey secret.Ref
}

// keyOf returns cfg's key.
func keyOf(cfg statestore.Config) key {
	k := key{role: cfg.Role, driver: cfg.Driver}
	if cfg.Driver != statestore.DriverMemory {
		k.topology, k.url = cfg.Topology, cfg.URL
		if t := cfg.TLS; t != nil {
			k.tls = tlsKey{set: true, ca: refOrZero(t.CACertificate), cert: refOrZero(t.ClientCertificate), clientKey: refOrZero(t.ClientKey)}
		}
	}
	return k
}

// refOrZero returns *r, or the zero Ref for nil; no resolvable reference is
// the zero Ref.
func refOrZero(r *secret.Ref) secret.Ref {
	if r == nil {
		return secret.Ref{}
	}
	return *r
}

// entry is one open driver. refs counts live handles and queued writes;
// at zero the driver closes and the entry is never reused.
type entry struct {
	m      *Manager
	key    key
	id     string // main-1, cache-2: logs never carry the URL
	driver statestore.Driver
	bind   *postcommit.Binding
	refs   atomic.Int64
	// limits counts live handles whose Revision has a ratelimit or quota
	// Policy on this driver; closing is set once its close starts. Both
	// are guarded by m.mu.
	limits  int
	closing bool
}

// Write implements postcommit.Target.
func (e *entry) Write(ctx context.Context, batch []*statestore.Write) { e.driver.Write(ctx, batch) }

// Hold implements postcommit.Target: a reference is taken only while one
// is held, so a closing driver is never revived.
func (e *entry) Hold() bool {
	for {
		n := e.refs.Load()
		if n <= 0 {
			return false
		}
		if e.refs.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// Release implements postcommit.Target.
func (e *entry) Release() {
	// A queued write's release has no caller context.
	e.drop(context.Background())
}

// drop releases one reference and retires the entry at zero.
func (e *entry) drop(ctx context.Context) {
	if e.refs.Add(-1) == 0 {
		e.m.retire(ctx, e)
	}
}

// view is the State Store status the Manager last applied.
type view struct {
	fallback, breaker, eviction, unauth, shared bool
	hops                                        int
}

// probe is one live driver read by refresh.
type probe struct {
	driver statestore.Driver
	limits bool
}

// Manager owns the Node's State Store drivers and post-commit queue. It is
// safe for concurrent use.
type Manager struct {
	deps    statestore.Deps
	clk     clock.Clock
	log     *slog.Logger
	status  emit.NodeStatus
	openers Openers
	queue   *postcommit.Queue
	every   time.Duration

	mu      sync.Mutex
	closed  bool // Acquire refuses
	shut    bool // Close is closing every driver
	entries map[key]*entry
	seq     uint64
	// live counts drivers opened and not yet closed; drained closes when
	// it is 0 after shut.
	live          int
	drained       chan struct{}
	drainedClosed bool
	fallback      int // live handles on the memory fallback
	shared        int // live handles whose Response Cache shares the limit store
	cancels       map[uint64]context.CancelFunc
	nextCancel    uint64
	closeErrs     []error

	// statusMu serializes refresh; last and scratch belong to it.
	statusMu sync.Mutex
	last     view
	scratch  []probe

	stopWatch context.CancelFunc
	watchDone chan struct{}
	finished  chan struct{}
}

// New returns a Manager and starts its post-commit writers and status
// poll, which Close stops. ctx supplies values only.
func New(ctx context.Context, cfg Config) *Manager {
	deps := cfg.Deps
	if deps.Clock == nil {
		deps.Clock = clock.Real()
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Limits == (statestore.Limits{}) {
		deps.Limits = statestore.DefaultLimits()
	}
	if deps.InFlight == nil {
		// One ceiling for the Node: every driver shares it.
		deps.InFlight = statestore.NewInFlight(deps.Limits.InFlight)
	}
	every := cfg.StatusInterval
	if every <= 0 {
		every = DefaultStatusInterval
	}
	m := &Manager{
		deps: deps, clk: deps.Clock, log: deps.Logger, status: deps.Status, openers: cfg.Openers, every: every,
		entries: map[key]*entry{}, drained: make(chan struct{}), cancels: map[uint64]context.CancelFunc{},
		watchDone: make(chan struct{}), finished: make(chan struct{}),
	}
	m.queue = postcommit.New(ctx, deps, cfg.Queue)
	wctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	m.stopWatch = stop
	go m.watch(wctx)
	return m
}

// Acquire returns new handles for one snapshot of spec: each handle holds
// one reference, released once by the snapshot. A driver whose key is
// open is shared; another is opened through its opener, which never
// dials. On error no reference is left held.
func (m *Manager) Acquire(ctx context.Context, spec Spec) (Handles, error) {
	mainCfg := spec.Main
	mainCfg.Role = statestore.RoleMain
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Handles{}, ErrClosed
	}
	me, err := m.entryLocked(ctx, mainCfg)
	if err != nil {
		m.mu.Unlock()
		return Handles{}, err
	}
	ce := me
	if spec.Cache != nil {
		cacheCfg := *spec.Cache
		cacheCfg.Role = statestore.RoleCache
		if ce, err = m.entryLocked(ctx, cacheCfg); err != nil {
			m.mu.Unlock()
			me.drop(ctx)
			m.refresh()
			return Handles{}, err
		}
	}
	main := &Handle{
		m: m, e: me,
		fallback: mainCfg.Source == statestore.SourceFallback,
		limits:   spec.Limits,
		shared:   spec.Limits && spec.Caches && sameDeployment(me, ce),
	}
	hs := Handles{Main: main, Cache: main}
	if ce != me {
		hs.Cache = &Handle{m: m, e: ce}
	}
	m.countLocked(main, 1)
	m.mu.Unlock()
	m.refresh()
	return hs, nil
}

// sameDeployment reports whether the Response Cache's driver writes to
// the deployment holding limit keys: one driver, or two redis connections
// to one URL.
func sameDeployment(main, cache *entry) bool {
	if main == cache {
		return true
	}
	a, b := main.key, cache.key
	return a.driver != statestore.DriverMemory && a.driver == b.driver && a.topology == b.topology && a.url == b.url
}

// countLocked adds d to the counters h contributes to.
func (m *Manager) countLocked(h *Handle, d int) {
	if h.fallback {
		m.fallback += d
	}
	if h.shared {
		m.shared += d
	}
	if h.limits {
		h.e.limits += d
	}
}

// entryLocked returns cfg's open entry with a new reference, or opens one.
func (m *Manager) entryLocked(ctx context.Context, cfg statestore.Config) (*entry, error) {
	k := keyOf(cfg)
	if e := m.entries[k]; e != nil && e.Hold() {
		return e, nil
	}
	open := m.openerOf(cfg.Driver)
	if open == nil {
		return nil, fmt.Errorf("%w %s", ErrNoOpener, driverName(cfg.Driver))
	}
	d, err := open(ctx, cfg, m.deps)
	if err != nil {
		return nil, fmt.Errorf("manager: open the %s %s driver: %w", roleName(cfg.Role), driverName(cfg.Driver), err)
	}
	if d == nil {
		return nil, fmt.Errorf("manager: the %s opener returned no driver", driverName(cfg.Driver))
	}
	m.seq++
	e := &entry{m: m, key: k, id: roleName(cfg.Role) + "-" + strconv.FormatUint(m.seq, 10), driver: d}
	e.bind = m.queue.Bind(e)
	e.refs.Store(1)
	m.entries[k] = e
	m.live++
	m.log.InfoContext(ctx, "state store driver opened", slog.String("store", e.id), slog.String("driver", driverName(cfg.Driver)),
		slog.String("topology", topologyName(cfg.Topology)), slog.String("config_source", sourceName(cfg.Source)))
	return e, nil
}

// openerOf returns the opener of driver d, or nil.
func (m *Manager) openerOf(d statestore.DriverKind) statestore.Opener {
	switch d {
	case statestore.DriverMemory:
		return m.openers.Memory
	case statestore.DriverRedis:
		return m.openers.Redis
	default:
		return nil
	}
}

// retire removes e, whose last reference is gone, and closes its driver
// on a goroutine the Manager owns.
func (m *Manager) retire(ctx context.Context, e *entry) {
	m.mu.Lock()
	if m.entries[e.key] == e {
		delete(m.entries, e.key)
	}
	if e.closing {
		m.mu.Unlock()
		return
	}
	e.closing = true
	m.closeLocked(ctx, e)
	m.mu.Unlock()
	m.refresh()
}

// closeLocked starts closing e's driver; Close cuts every close short
// when its context ends.
func (m *Manager) closeLocked(parent context.Context, e *entry) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	id := m.nextCancel
	m.nextCancel++
	m.cancels[id] = cancel
	go func() {
		err := e.driver.Close(ctx)
		if err != nil {
			m.log.WarnContext(ctx, "state store driver close failed", slog.String("store", e.id), slog.String(catalog.KeyError, err.Error()))
		} else {
			m.log.InfoContext(ctx, "state store driver closed", slog.String("store", e.id))
		}
		cancel()
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.cancels, id)
		if err != nil && m.shut {
			m.closeErrs = append(m.closeErrs, fmt.Errorf("manager: close %s: %w", e.id, err))
		}
		m.live--
		if m.shut && m.live == 0 {
			m.drainLocked()
		}
	}()
}

// drainLocked closes drained once.
func (m *Manager) drainLocked() {
	if !m.drainedClosed {
		m.drainedClosed = true
		close(m.drained)
	}
}

// cancelCloses cuts every running driver close short.
func (m *Manager) cancelCloses() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cancel := range m.cancels {
		cancel()
	}
}

// Close flushes the post-commit queue (up to 5 s, or until ctx ends),
// stops the status poll, closes every driver, held handles' drivers
// included, and clears the State Store degraded reasons. When ctx ends
// first, driver closes are cut short and Close returns ctx's error with
// the close errors. Acquire then fails with ErrClosed; Release still
// works. Later calls wait for the first and return nil.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		select {
		case <-m.finished:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("manager: close: %w", ctx.Err())
		}
	}
	m.closed = true
	m.mu.Unlock()
	defer close(m.finished)

	var errs []error
	if err := m.queue.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	m.stopWatch()
	<-m.watchDone

	m.mu.Lock()
	m.shut = true
	for k, e := range m.entries {
		delete(m.entries, k)
		if !e.closing {
			e.closing = true
			m.closeLocked(ctx, e)
		}
	}
	if m.live == 0 {
		m.drainLocked()
	}
	m.mu.Unlock()

	select {
	case <-m.drained:
	case <-ctx.Done():
		m.cancelCloses()
		<-m.drained
		errs = append(errs, fmt.Errorf("manager: close: %w", ctx.Err()))
	}
	m.mu.Lock()
	errs = append(errs, m.closeErrs...)
	m.mu.Unlock()

	m.statusMu.Lock()
	m.apply(view{})
	m.statusMu.Unlock()
	return errors.Join(errs...)
}

// watch polls driver Status every interval until ctx ends.
func (m *Manager) watch(ctx context.Context) {
	defer close(m.watchDone)
	t := m.clk.NewTimer(m.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			m.refresh()
			t.Reset(m.every)
		}
	}
}

// refresh recomputes the State Store status from the live handles and
// drivers and applies the changes; it does nothing once Close began
// closing drivers.
func (m *Manager) refresh() {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	m.mu.Lock()
	if m.shut {
		m.mu.Unlock()
		return
	}
	v := view{fallback: m.fallback > 0, shared: m.shared > 0}
	list := m.scratch[:0]
	for _, e := range m.entries {
		list = append(list, probe{driver: e.driver, limits: e.limits > 0})
	}
	m.mu.Unlock()
	for _, p := range list {
		st := p.driver.Status()
		v.breaker = v.breaker || st.BreakerNotClosed
		v.eviction = v.eviction || (p.limits && st.EvictionPolicy == statestore.No)
		v.unauth = v.unauth || st.Unauthenticated
		if st.Cleartext {
			v.hops++
		}
	}
	clear(list)
	m.scratch = list[:0]
	m.apply(v)
}

// apply sets every reason and the hop count that changed since the last
// view, and logs the startup warnings when their condition begins.
func (m *Manager) apply(v view) {
	last := m.last
	m.last = v
	if v.fallback && !last.fallback {
		m.log.Warn("no State Store is configured: the memory driver multiplies every Rate Limit and Quota by the Node count")
	}
	if v.shared && !last.shared {
		m.log.Warn("the Response Cache shares the State Store that holds Rate Limit and Quota keys")
	}
	if m.status == nil {
		return
	}
	reasons := [...]struct {
		r        catalog.Reason
		was, now bool
	}{
		{catalog.ReasonStateStoreMemoryFallback, last.fallback, v.fallback},
		{catalog.ReasonStateStoreBreakerOpen, last.breaker, v.breaker},
		{catalog.ReasonStateStoreEvictionPolicy, last.eviction, v.eviction},
		{catalog.ReasonStateStoreUnauthenticated, last.unauth, v.unauth},
	}
	for _, r := range reasons {
		if r.was != r.now {
			m.status.SetDegraded(r.r, statusSource, r.now)
		}
	}
	if v.hops != last.hops {
		m.status.SetCleartextHops(catalog.HopStateStore, v.hops)
	}
}

// driverName returns the configuration name of d.
func driverName(d statestore.DriverKind) string {
	switch d {
	case statestore.DriverMemory:
		return "memory"
	case statestore.DriverRedis:
		return "redis"
	default:
		return "unknown"
	}
}

// topologyName returns the configuration name of t, "" for none.
func topologyName(t statestore.Topology) string {
	switch t {
	case statestore.TopologyStandalone:
		return "standalone"
	case statestore.TopologyCluster:
		return "cluster"
	case statestore.TopologySentinel:
		return "sentinel"
	default:
		return ""
	}
}

// roleName names a role in logs and entry ids.
func roleName(r statestore.Role) string {
	switch r {
	case statestore.RoleMain:
		return "main"
	case statestore.RoleCache:
		return "cache"
	default:
		return "unknown"
	}
}

// sourceName names where a configuration came from.
func sourceName(s statestore.Source) string {
	switch s {
	case statestore.SourceGateway:
		return "gateway"
	case statestore.SourceEnv:
		return "env"
	case statestore.SourceFallback:
		return "fallback"
	default:
		return "unknown"
	}
}
