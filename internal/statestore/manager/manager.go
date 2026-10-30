// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"bytes"
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

// Errors of Acquire and of a rotation.
var (
	// ErrClosed reports Acquire after Close.
	ErrClosed = errors.New("manager: closed")
	// ErrNoOpener reports a driver without an opener.
	ErrNoOpener = errors.New("manager: no opener for the State Store driver")
	// ErrEmptyURL reports a rotated State Store url secret whose value is
	// empty once trimmed; the connection keeps its client.
	ErrEmptyURL = errors.New("manager: the rotated State Store url is empty")
)

// OpenError is an opener's refusal of one connection's configuration,
// which Acquire returns: Role tells the main connection (Gateway
// spec.stateStore) from the cache connection (spec.stateStore.cache), and
// Err carries the opener's RZ code (statestore.Opener).
type OpenError struct {
	// Role is the connection whose driver did not open.
	Role statestore.Role
	// Driver is the driver the configuration names.
	Driver statestore.DriverKind
	// Err is the opener's error.
	Err error
}

// Error returns "manager: open the <role> <driver> driver: <cause>".
func (e *OpenError) Error() string {
	return fmt.Sprintf("manager: open the %s %s driver: %v", roleName(e.Role), driverName(e.Driver), e.Err)
}

// Unwrap returns the opener's error.
func (e *OpenError) Unwrap() error { return e.Err }

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
	// Zone is the Node's RURALZ_ZONE (setting.Settings.Zone), empty
	// without one.
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

// Handle is a snapshot's reference to a State Store connection: Store and
// Enqueuer are valid until Release, which the snapshot calls once when
// it is freed. Later Release calls do nothing.
type Handle struct {
	m *Manager
	e *entry
	// view is the snapshot's view of a connection whose driver can change
	// under it (a rotated url secret) or whose calls carry the zone label
	// (statestore.ZoneSetter); nil for any other, such as the memory
	// driver.
	view *view
	// fallback, limits, caches and shared record what this snapshot's
	// Revision uses (degraded reasons and warnings): limits a ratelimit or
	// quota Policy on this connection, caches a cache Policy on it.
	fallback, limits, caches, shared bool
	released                         atomic.Bool
}

// Store returns the connection's driver, or, for a connection whose
// driver a rotated url secret can replace or whose calls carry the zone
// label, the snapshot's view of it: each call goes to the driver serving
// the connection then, and the first call hands the driver the snapshot's
// zone map (view). One handle always returns the same value.
func (h *Handle) Store() statestore.Store {
	if h.view != nil {
		return h.view
	}
	return h.e.gen.Load().driver
}

// Enqueuer returns the connection's Enqueuer: each write queues for the
// driver serving the connection when it is enqueued.
func (h *Handle) Enqueuer() statestore.Enqueuer { return h.e.enq }

// Release drops the snapshot's reference; the connection retires once no
// snapshot holds it, and its driver closes once no queued write or call
// holds that.
func (h *Handle) Release() {
	if !h.released.CompareAndSwap(false, true) {
		return
	}
	m := h.m
	m.mu.Lock()
	m.countLocked(h, -1)
	m.mu.Unlock()
	h.e.drop()
	m.refresh()
}

// key identifies a shareable connection: memory by role only, redis by
// role, topology, URL and the stateStore.tls references. Rotations of the
// TLS values keep the driver, which follows them itself; a rotation of
// the URL value moves the connection to the new value's key.
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

// urlSource is the secret a connection's URL was read from: the reference
// and the Revision's secret table holding it; zero without one (memory).
type urlSource struct {
	ref     secret.Ref
	secrets secret.Store
}

// set reports a reference the Manager can watch.
func (s urlSource) set() bool { return s.secrets != nil && s.ref != (secret.Ref{}) }

// entry is one connection: the role, driver and, for redis, topology, URL
// and TLS block references that snapshots share, and the driver serving it
// now (gen). refs counts live handles; at zero the entry retires: it leaves
// the Manager, stops watching its URL secret and drops its hold on its
// generation, whose driver then closes once no queued write or call holds
// it. A rotated URL secret value replaces the generation (rotate).
type entry struct {
	m   *Manager
	enq *enqueuer
	url urlSource
	// labels reports drivers whose calls carry the zone label
	// (statestore.ZoneSetter).
	labels bool
	gen    atomic.Pointer[generation]
	refs   atomic.Int64
	// key, cfg (the configuration a rotation reopens with, its URL the one
	// in use), limits, caches, retired and stopWatch are guarded by m.mu.
	// limits counts live handles whose Revision has a ratelimit or quota
	// Policy on this connection, caches those with a cache Policy on it.
	key            key
	cfg            statestore.Config
	limits, caches int
	retired        bool
	stopWatch      func()
	// zones is the zone map the current driver holds, and zoneSeq the
	// Acquire sequence number of the snapshot that handed it: the snapshot
	// that opened the connection, or the newest whose view handed the
	// driver its map since. zoneMu orders the handovers and the rotations,
	// which open the new driver with zones.
	zoneMu  sync.Mutex
	zones   statestore.Zones
	zoneSeq atomic.Uint64
}

// Hold takes a handle reference while one is held, so a retired entry is
// never revived.
func (e *entry) Hold() bool { return holdRef(&e.refs) }

// drop releases one handle reference and retires the entry at zero.
func (e *entry) drop() {
	if e.refs.Add(-1) == 0 {
		e.m.retire(e)
	}
}

// hold returns the generation serving the connection, holding it for one
// call or enqueue: the current one, or the next when a rotation retired it
// meanwhile. held is false only when every generation tried is closing,
// as after the entry retired; the caller then uses it unheld, and its
// closing driver answers as a closed driver does.
func (e *entry) hold() (g *generation, held bool) {
	g = e.gen.Load()
	if g.Hold() {
		return g, true
	}
	if next := e.gen.Load(); next != g {
		g = next
		if g.Hold() {
			return g, true
		}
	}
	return g, false
}

// holdRef increments n while it is above zero.
func holdRef(n *atomic.Int64) bool {
	for {
		v := n.Load()
		if v <= 0 {
			return false
		}
		if n.CompareAndSwap(v, v+1) {
			return true
		}
	}
}

// generation is one driver of a connection: the one its first snapshot
// opened, or one a rotation of its URL secret opened. refs counts the
// entry's hold while it is current, queued writes and calls in progress;
// at zero the driver closes on a goroutine the Manager owns.
type generation struct {
	m      *Manager
	id     string // main-1, cache-2: logs never carry the URL
	url    string // the URL the driver opened with, compared at a rotation
	driver statestore.Driver
	bind   *postcommit.Binding
	refs   atomic.Int64
	// closing is set once the close starts; guarded by m.mu.
	closing bool
}

// Write implements postcommit.Target.
func (g *generation) Write(ctx context.Context, batch []*statestore.Write) {
	g.driver.Write(ctx, batch)
}

// Hold implements postcommit.Target: a reference is taken only while one
// is held, so a closing driver is never revived.
func (g *generation) Hold() bool { return holdRef(&g.refs) }

// Release implements postcommit.Target, and ends a call's or the entry's
// hold: at zero the driver closes.
func (g *generation) Release() {
	if g.refs.Add(-1) == 0 {
		g.m.closeGen(g)
	}
}

// enqueuer is a connection's Enqueuer: each write queues for the
// generation serving the connection, held while it enqueues, so a
// rotation in between never drops it.
type enqueuer struct{ e *entry }

// Enqueue implements statestore.Enqueuer.
func (q *enqueuer) Enqueue(class statestore.WriteClass, w *statestore.Write) bool {
	g, held := q.e.hold()
	if held {
		defer g.Release()
	}
	return g.bind.Enqueue(class, w)
}

// viewFor returns the view of e for the snapshot acquired as seq with zone
// map zones, or nil when e's driver never changes and labels no zones (the
// memory driver).
func (e *entry) viewFor(seq uint64, zones statestore.Zones) *view {
	if !e.labels && !e.url.set() {
		return nil
	}
	return &view{e: e, seq: seq, zones: zones}
}

// view is one snapshot's view of a connection. Each call goes to the
// generation serving the connection when it starts, held until it ends, so
// a rotated URL secret moves later calls to the new driver while a call
// in progress finishes on the old one
// (docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes").
//
// For drivers whose calls carry the zone label, a Hot Reload keeps the
// driver when only the zone map changes, and the changed map labels calls
// from the swap on. The first call through the view hands the driver the
// snapshot's zone map, unless the snapshot that opened the connection or a
// newer one already handed it its own: a snapshot's Filters and
// revalidation workers call the State Store only once it serves, so the
// map changes at the first call after the swap, a snapshot that is never
// published never changes it, and the calls of older snapshots still
// running after the swap keep the newer map.
type view struct {
	e     *entry
	seq   uint64
	zones statestore.Zones
}

var _ statestore.Store = (*view)(nil)

// Consume adopts the snapshot's zone map, then runs the serving driver's
// Consume.
func (v *view) Consume(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) int {
	v.adopt()
	g, held := v.e.hold()
	if held {
		defer g.Release()
	}
	return g.driver.Consume(ctx, rb, calls)
}

// Read adopts the snapshot's zone map, then runs the serving driver's
// Read.
func (v *view) Read(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) {
	v.adopt()
	g, held := v.e.hold()
	if held {
		defer g.Release()
	}
	g.driver.Read(ctx, rb, calls)
}

// Write adopts the snapshot's zone map, then runs the serving driver's
// Write.
func (v *view) Write(ctx context.Context, batch []*statestore.Write) {
	v.adopt()
	g, held := v.e.hold()
	if held {
		defer g.Release()
	}
	g.driver.Write(ctx, batch)
}

// AdmitStoreBytes runs the serving driver's AdmitStoreBytes.
func (v *view) AdmitStoreBytes(key statestore.CacheKey, n int) bool {
	return v.e.gen.Load().driver.AdmitStoreBytes(key, n)
}

// Supports runs the serving driver's Supports.
func (v *view) Supports(c statestore.Capability) bool { return v.e.gen.Load().driver.Supports(c) }

// RoundTrips runs the serving driver's RoundTrips.
func (v *view) RoundTrips() bool { return v.e.gen.Load().driver.RoundTrips() }

// adopt hands the serving driver the snapshot's zone map unless the
// connection holds the map of this snapshot or of a newer one: one atomic
// load once the handover is done, a lock only for the handover itself.
func (v *view) adopt() {
	e := v.e
	if !e.labels || e.zoneSeq.Load() >= v.seq {
		return
	}
	e.zoneMu.Lock()
	defer e.zoneMu.Unlock()
	if e.zoneSeq.Load() >= v.seq {
		return
	}
	if zs, ok := e.gen.Load().driver.(statestore.ZoneSetter); ok {
		zs.SetZones(v.zones)
	}
	e.zones = v.zones
	e.zoneSeq.Store(v.seq)
}

// status is the State Store status the Manager last applied.
type status struct {
	fallback, breaker, eviction, unauth, shared bool
	hops                                        int
}

// probe is one live driver read by refresh.
type probe struct {
	driver         statestore.Driver
	limits, caches bool
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

	mu     sync.Mutex
	closed bool // Acquire refuses, and URL rotations open nothing
	shut   bool // Close is closing every driver
	// entries maps keys to the connections snapshots share; conns holds
	// every connection not retired, mapped or not; gens holds every
	// generation whose driver is open or closing.
	entries map[key]*entry
	conns   map[*entry]struct{}
	gens    map[*generation]struct{}
	seq     uint64
	// acquires numbers the Acquire calls, so the handles of a newer
	// snapshot carry a greater sequence number (view).
	acquires uint64
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
	last     status
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
		entries: map[key]*entry{}, conns: map[*entry]struct{}{}, gens: map[*generation]struct{}{},
		drained: make(chan struct{}), cancels: map[uint64]context.CancelFunc{},
		watchDone: make(chan struct{}), finished: make(chan struct{}),
	}
	m.queue = postcommit.New(ctx, deps, cfg.Queue)
	wctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	m.stopWatch = stop
	go m.watch(wctx)
	return m
}

// Acquire returns new handles for one snapshot of spec: each handle holds
// one reference, released once by the snapshot. A connection whose key is
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
	m.acquires++
	seq := m.acquires
	me, err := m.entryLocked(ctx, mainCfg, sourceOf(spec.MainURL, spec.Secrets), seq)
	if err != nil {
		m.mu.Unlock()
		return Handles{}, err
	}
	ce := me
	var cacheZones statestore.Zones
	if spec.Cache != nil {
		cacheCfg := *spec.Cache
		cacheCfg.Role = statestore.RoleCache
		cacheZones = cacheCfg.Zones
		if ce, err = m.entryLocked(ctx, cacheCfg, sourceOf(spec.CacheURL, spec.Secrets), seq); err != nil {
			m.mu.Unlock()
			me.drop()
			m.refresh()
			return Handles{}, err
		}
	}
	main := &Handle{
		m: m, e: me, view: me.viewFor(seq, mainCfg.Zones),
		fallback: mainCfg.Source == statestore.SourceFallback,
		limits:   spec.Limits,
		caches:   spec.Caches && ce == me,
		shared:   spec.Limits && spec.Caches && sameDeployment(me, ce),
	}
	hs := Handles{Main: main, Cache: main}
	m.countLocked(main, 1)
	if ce != me {
		hs.Cache = &Handle{m: m, e: ce, view: ce.viewFor(seq, cacheZones), caches: spec.Caches}
		m.countLocked(hs.Cache, 1)
	}
	m.mu.Unlock()
	m.refresh()
	return hs, nil
}

// sourceOf returns the URL source of ref in secrets, zero without ref.
func sourceOf(ref *secret.Ref, secrets secret.Store) urlSource {
	if ref == nil {
		return urlSource{}
	}
	return urlSource{ref: *ref, secrets: secrets}
}

// sameDeployment reports whether the Response Cache's connection writes to
// the deployment holding limit keys: one connection, or two redis
// connections to one URL. m.mu is held.
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
	if h.caches {
		h.e.caches += d
	}
}

// entryLocked returns cfg's open connection with a new reference, or opens
// one for the snapshot acquired as seq, whose zone map (cfg.Zones) the new
// driver opens with. A connection whose URL came from a secret (src)
// watches it: the watch is registered before the URL's latest value is
// read, so a rotation since SpecOf read it opens the connection on the
// rotated value, and none is lost before the watch.
func (m *Manager) entryLocked(ctx context.Context, cfg statestore.Config, src urlSource, seq uint64) (*entry, error) {
	k := keyOf(cfg)
	if e := m.entries[k]; e != nil && e.Hold() {
		return e, nil
	}
	open := m.openerOf(cfg.Driver)
	if open == nil {
		return nil, fmt.Errorf("%w %s", ErrNoOpener, driverName(cfg.Driver))
	}
	e := &entry{m: m}
	if cfg.Driver != statestore.DriverMemory && src.set() {
		e.url = src
		e.stopWatch = src.secrets.Watch(src.ref, func(v secret.Value) error { return m.rotate(e, v) })
		if u, ok := urlOf(src.secrets, src.ref); ok && u != cfg.URL {
			cfg.URL = u
			k = keyOf(cfg)
			if x := m.entries[k]; x != nil && x.Hold() {
				e.retired = true
				e.stopWatch()
				return x, nil
			}
		}
	}
	d, err := open(ctx, cfg, m.deps)
	if err == nil && d == nil {
		err = fmt.Errorf("manager: the %s opener returned no driver", driverName(cfg.Driver))
	} else if err != nil {
		err = &OpenError{Role: cfg.Role, Driver: cfg.Driver, Err: err}
	}
	if err != nil {
		e.retired = true
		if e.stopWatch != nil {
			e.stopWatch()
		}
		return nil, err
	}
	_, e.labels = d.(statestore.ZoneSetter)
	e.enq = &enqueuer{e: e}
	e.key, e.cfg, e.zones = k, cfg, cfg.Zones
	e.zoneSeq.Store(seq)
	e.refs.Store(1)
	e.gen.Store(m.newGenLocked(cfg, d))
	m.entries[k] = e
	m.conns[e] = struct{}{}
	m.log.InfoContext(ctx, "state store driver opened", slog.String("store", e.gen.Load().id), slog.String("driver", driverName(cfg.Driver)),
		slog.String("topology", topologyName(cfg.Topology)), slog.String("config_source", sourceName(cfg.Source)))
	return e, nil
}

// newGenLocked returns the generation of d, opened for cfg, holding the
// entry's reference.
func (m *Manager) newGenLocked(cfg statestore.Config, d statestore.Driver) *generation {
	m.seq++
	g := &generation{m: m, id: roleName(cfg.Role) + "-" + strconv.FormatUint(m.seq, 10), url: cfg.URL, driver: d}
	g.bind = m.queue.Bind(g)
	g.refs.Store(1)
	m.gens[g] = struct{}{}
	m.live++
	return g
}

// rotate is the watch of a connection's URL secret: a rotated value v
// that differs from the URL in use opens a new driver for the connection
// at once, off the request path, as a changed URL does at a Hot Reload
// (docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes"). Calls move to the new driver, which dials,
// authenticates and loads its scripts under the reconnect pacing, and
// later writes queue for it; the old driver closes once the writes queued
// for it are written or dropped and its calls in progress end. The
// connection moves to the new URL's key, so a later Revision resolving the
// rotated value shares it. A value the opener refuses keeps the old
// driver, and the error, which never carries the URL, is the watcher's
// rotation failure.
func (m *Manager) rotate(e *entry, v secret.Value) error {
	b := v.Reveal()
	u := string(bytes.Trim(b, " \t\r\n"))
	clear(b)
	m.mu.Lock()
	if m.closed || e.retired || e.gen.Load() == nil {
		m.mu.Unlock()
		return nil
	}
	old := e.gen.Load()
	if u == old.url {
		m.mu.Unlock()
		return nil
	}
	if u == "" {
		m.mu.Unlock()
		return ErrEmptyURL
	}
	open := m.openerOf(e.cfg.Driver)
	if open == nil {
		m.mu.Unlock()
		return fmt.Errorf("%w %s", ErrNoOpener, driverName(e.cfg.Driver))
	}
	// The watch has no caller context; the opener reads values only.
	ctx := context.Background()
	e.zoneMu.Lock()
	cfg := e.cfg
	cfg.URL, cfg.Zones = u, e.zones
	d, err := open(ctx, cfg, m.deps)
	if err == nil && d == nil {
		err = fmt.Errorf("manager: the %s opener returned no driver", driverName(cfg.Driver))
	} else if err != nil {
		err = &OpenError{Role: cfg.Role, Driver: cfg.Driver, Err: err}
	}
	if err != nil {
		// The resolver logs the refusal, counts it in
		// ruralz_config_secret_rotation_failures_total and raises
		// secret_rotation_failed until a value opens.
		e.zoneMu.Unlock()
		m.mu.Unlock()
		return err
	}
	g := m.newGenLocked(cfg, d)
	e.gen.Store(g)
	e.zoneMu.Unlock()
	e.cfg.URL = u
	if m.entries[e.key] == e {
		delete(m.entries, e.key)
	}
	e.key.url = u
	if m.entries[e.key] == nil {
		m.entries[e.key] = e
	}
	m.mu.Unlock()
	m.log.InfoContext(ctx, "state store url rotated: a new driver serves the connection", slog.String("store", g.id),
		slog.String("replaces", old.id), slog.String("driver", driverName(cfg.Driver)), slog.String("topology", topologyName(cfg.Topology)))
	old.Release()
	m.refresh()
	return nil
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

// retire removes e, whose last handle is gone, stops its watch and drops
// its hold on its generation.
func (m *Manager) retire(e *entry) {
	m.mu.Lock()
	if e.retired {
		m.mu.Unlock()
		return
	}
	e.retired = true
	if m.entries[e.key] == e {
		delete(m.entries, e.key)
	}
	delete(m.conns, e)
	stop := e.stopWatch
	e.stopWatch = nil
	g := e.gen.Load()
	m.mu.Unlock()
	if stop != nil {
		stop()
	}
	g.Release()
	m.refresh()
}

// closeGen starts closing g's driver, whose last reference is gone.
func (m *Manager) closeGen(g *generation) {
	m.mu.Lock()
	if g.closing {
		m.mu.Unlock()
		return
	}
	g.closing = true
	m.closeLocked(context.Background(), g)
	m.mu.Unlock()
}

// closeLocked starts closing g's driver on a goroutine the Manager owns;
// Close cuts every close short when its context ends.
func (m *Manager) closeLocked(parent context.Context, g *generation) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	id := m.nextCancel
	m.nextCancel++
	m.cancels[id] = cancel
	go func() {
		err := g.driver.Close(ctx)
		if err != nil {
			m.log.WarnContext(ctx, "state store driver close failed", slog.String("store", g.id), slog.String(catalog.KeyError, err.Error()))
		} else {
			m.log.InfoContext(ctx, "state store driver closed", slog.String("store", g.id))
		}
		cancel()
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.cancels, id)
		delete(m.gens, g)
		if err != nil && m.shut {
			m.closeErrs = append(m.closeErrs, fmt.Errorf("manager: close %s: %w", g.id, err))
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
// stops the status poll and the URL watches, closes every driver, held
// handles' drivers and rotated-out ones included, and clears the State
// Store degraded reasons. When ctx ends first, driver closes are cut
// short and Close returns ctx's error with the close errors. Acquire then
// fails with ErrClosed; Release still works. Later calls wait for the
// first and return nil.
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
	var stops []func()
	for e := range m.conns {
		e.retired = true
		if e.stopWatch != nil {
			stops = append(stops, e.stopWatch)
			e.stopWatch = nil
		}
	}
	clear(m.conns)
	clear(m.entries)
	for g := range m.gens {
		if !g.closing {
			g.closing = true
			m.closeLocked(ctx, g)
		}
	}
	if m.live == 0 {
		m.drainLocked()
	}
	m.mu.Unlock()
	for _, stop := range stops {
		stop()
	}

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
	m.apply(status{})
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

// refresh recomputes the State Store status from the live handles and the
// drivers serving the live connections and applies the changes; it does
// nothing once Close began closing drivers.
func (m *Manager) refresh() {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	m.mu.Lock()
	if m.shut {
		m.mu.Unlock()
		return
	}
	v := status{fallback: m.fallback > 0, shared: m.shared > 0}
	list := m.scratch[:0]
	for e := range m.conns {
		list = append(list, probe{driver: e.gen.Load().driver, limits: e.limits > 0, caches: e.caches > 0})
	}
	m.mu.Unlock()
	for _, p := range list {
		st := p.driver.Status()
		v.breaker = v.breaker || st.BreakerNotClosed
		// Limit keys need noeviction; a connection a cache Policy uses
		// takes noeviction or volatile-ttl ("Response caching").
		v.eviction = v.eviction || (p.limits && st.EvictionPolicy == statestore.No) ||
			(p.caches && st.CacheEvictionPolicy == statestore.No)
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
// status, and logs the startup warnings when their condition begins.
func (m *Manager) apply(v status) {
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

// roleName names a role in logs and generation ids.
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
