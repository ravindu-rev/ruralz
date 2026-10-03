// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package memory is the memory State Store driver (spec 08 section 2.9,
// docs/architecture/11-scalability-and-distributed-state.md): TATs, Quota
// counters, Response Cache generations, partitions and leases held in the
// Node with the semantics of the ruralz_v1.lua library (spec 08 reqs 41 to
// 45), the same float64 arithmetic and 2026 epoch, and the injected clock
// as the server's TIME, so a differential test against the redis driver
// gets identical decisions and TATs for identical inputs (spec 08 req 46).
//
// State lives in 256 mutex-guarded shards selected by the Redis Cluster
// hash slot of each key's tag, so every limit of one call and every call
// of one merged script (script_multi) runs under one lock, atomically.
// Maps are keyed by comparable structs, so no key string is built. Bounds:
// 1,048,576 counter, lease and cache keys and 64 MiB of cache entry bytes;
// a full shard refuses new keys with RZ-STS-002, like a noeviction server
// refusing writes (spec 08 req 58). One sweeper goroutine, stopped by
// Close, reclaims expired keys within 10 s of expiry.
//
// memory never reports breaker, in-flight or timeout failures and its time
// stays gateway-added (RoundTrips is false): calls count in
// ruralz_state_calls_total and ruralz_state_ops_total on the request's
// stripe, never in ruralz_state_call_duration_seconds (spec 08 reqs 59
// and 60). A single-limit GCRA call makes no heap allocation.
package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Bounds of spec 08 req 58 (proposed; D11 gives none).
const (
	// DefaultShards is the number of mutex-guarded shards.
	DefaultShards = 256
	// DefaultMaxEntries bounds counter, lease and cache keys.
	DefaultMaxEntries = 1 << 20
	// DefaultMaxCacheBytes bounds cache entry bytes; it is also the
	// memory reading's maxmemory (policy noeviction).
	DefaultMaxCacheBytes = 64 << 20
	// DefaultSweepInterval reclaims expired keys within 10 s of expiry.
	DefaultSweepInterval = 5 * time.Second
)

// ErrWrongDriver is returned by New for a configuration naming another
// driver.
var ErrWrongDriver = errors.New("memory: the configuration names another State Store driver")

// Options are the driver's fixed bounds; only tests change them.
type Options struct {
	// Shards is the number of shards (DefaultShards).
	Shards int
	// MaxEntries bounds keys of every kind, split evenly over the shards
	// (DefaultMaxEntries).
	MaxEntries int
	// MaxCacheBytes bounds cache entry bytes (DefaultMaxCacheBytes).
	MaxCacheBytes int64
	// SweepInterval is the sweeper period (DefaultSweepInterval).
	SweepInterval time.Duration
	// ServerClock plays the server's TIME: GCRA and Quota decisions, key
	// expiry and generation values read it. Nil means Deps.Clock, the
	// production setting; a differential test sets it apart from the Node
	// clock that picks Quota window candidates (spec 08 req 42).
	ServerClock clock.Clock
}

// DefaultOptions returns the bounds of spec 08 req 58.
func DefaultOptions() Options {
	return Options{
		Shards: DefaultShards, MaxEntries: DefaultMaxEntries,
		MaxCacheBytes: DefaultMaxCacheBytes, SweepInterval: DefaultSweepInterval,
	}
}

// Driver is the memory statestore.Driver. It is safe for concurrent use.
type Driver struct {
	node, server clock.Clock
	// split is true when ServerClock differs from the Node clock.
	split   bool
	metrics *emit.StateMetrics
	logger  *slog.Logger
	mac     *keys.MAC

	shards   []shard
	perShard int
	maxBytes int64
	bytes    atomic.Int64 // cache entry bytes held
	admit    admission

	skewLog, macLog rateLog

	closed atomic.Bool
	stop   context.CancelFunc
	done   chan struct{}
}

var _ statestore.Driver = (*Driver)(nil)

// Open is the statestore.Opener of the memory driver (resolution R-54):
// New with DefaultOptions.
func Open(ctx context.Context, cfg statestore.Config, deps statestore.Deps) (statestore.Driver, error) {
	d, err := New(ctx, cfg, deps, DefaultOptions())
	if err != nil {
		return nil, err
	}
	return d, nil
}

// New returns a memory driver and starts its sweeper, which Close stops.
// cfg.URL and cfg.Topology are ignored (spec 08 req 4). It fails only for
// a configuration naming another driver or a MAC key under 32 bytes.
func New(ctx context.Context, cfg statestore.Config, deps statestore.Deps, opts Options) (*Driver, error) {
	if cfg.Driver != 0 && cfg.Driver != statestore.DriverMemory {
		return nil, ErrWrongDriver
	}
	mac, err := keys.NewMAC(deps.MACKey)
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	def := DefaultOptions()
	if opts.Shards <= 0 {
		opts.Shards = def.Shards
	}
	opts.Shards = min(opts.Shards, keys.Slots)
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = def.MaxEntries
	}
	if opts.MaxCacheBytes <= 0 {
		opts.MaxCacheBytes = def.MaxCacheBytes
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = def.SweepInterval
	}
	node := deps.Clock
	if node == nil {
		node = clock.Real()
	}
	server := opts.ServerClock
	if server == nil {
		server = node
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	d := &Driver{
		node: node, server: server, split: opts.ServerClock != nil, metrics: deps.Metrics, logger: logger, mac: mac,
		shards: make([]shard, opts.Shards), perShard: max(opts.MaxEntries/opts.Shards, 1),
		maxBytes: opts.MaxCacheBytes, done: make(chan struct{}),
	}
	for i := range d.shards {
		d.shards[i].init()
	}
	sweepCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	d.stop = stop
	go d.sweepLoop(sweepCtx, opts.SweepInterval)
	return d, nil
}

// Close stops the sweeper and waits for it, or for ctx. Later calls fail
// with RZ-STS-002, like calls on a closed connection.
func (d *Driver) Close(ctx context.Context) error {
	if d.closed.CompareAndSwap(false, true) {
		d.stop()
	}
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("memory: close: %w", ctx.Err())
	}
}

// Status reports a noeviction store without breakers, TLS or credentials
// concerns (spec 08 req 58).
func (d *Driver) Status() statestore.Status {
	return statestore.Status{EvictionPolicy: statestore.Yes}
}

// Supports reports the script command set; M1 has no vector search.
func (d *Driver) Supports(c statestore.Capability) bool { return c&^statestore.CapScripts == 0 }

// RoundTrips is false: memory time stays gateway-added (spec 08 req 59).
func (d *Driver) RoundTrips() bool { return false }

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

// precheck applies the pre-call checks memory can fail (spec 08 reqs 33
// and 60): per-request deadline or Policy timeout spent (RZ-STS-004), then
// a closed driver (RZ-STS-002). With a nil budget the Policy timeout and
// the context deadline alone apply.
func (d *Driver) precheck(ctx context.Context, rb *statestore.RequestBudget, timeout time.Duration) *statestore.Error {
	if ctx.Err() != nil {
		return statestore.ErrNotAttempted
	}
	now := d.node.Now()
	if rb != nil {
		if _, ok := rb.CallTimeout(ctx, now, timeout); !ok {
			return statestore.ErrNotAttempted
		}
	} else if timeout <= 0 {
		return statestore.ErrNotAttempted
	} else if dl, ok := ctx.Deadline(); ok && !dl.After(now) {
		return statestore.ErrNotAttempted
	}
	if d.closed.Load() {
		return statestore.ErrFailed
	}
	return nil
}

// Consume runs the longest prefix of calls whose keys share a hash slot
// as one script, in order, stopping at the first deny (spec 08 reqs 29,
// 38 and 44): every call of the group is Done up to and including the
// deny, calls after it stay !Done, and a failure fails every call of the
// group. The merged call's timeout is the smallest of the group, clamped
// by the per-request deadline.
func (d *Driver) Consume(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) int {
	if len(calls) == 0 {
		return 0
	}
	first := calls[0]
	slot := keys.SlotOf(first)
	n := 1
	if slot >= 0 {
		for n < len(calls) && keys.SlotOf(calls[n]) == slot {
			n++
		}
	}
	group := calls[:n]
	trip, batch := statestore.TripSingle, -1
	if n > 1 {
		trip, batch = statestore.TripScriptMulti, 0
	}
	timeout := first.Timeout
	for _, c := range group {
		c.Done, c.Err, c.Elapsed, c.Batch, c.Trip = false, nil, 0, batch, trip
		timeout = min(timeout, c.Timeout)
	}
	stripe := stripeOf(rb)
	if slot < 0 {
		// Not a consumptive call: an error reply.
		first.Err = statestore.ErrFailed
		d.count(stripe, first.Kind.Label(), statestore.ResultError)
		return n
	}
	if err := d.precheck(ctx, rb, timeout); err != nil {
		for _, c := range group {
			c.Err = err
			d.count(stripe, c.Kind.Label(), err.Result())
		}
		return n
	}
	now := instantOf(d.server.Now())
	nodeMs := now.ms
	if d.split {
		nodeMs = d.node.Now().UnixMilli()
	}
	sh := d.shardOf(slot)
	failed, skewed := false, -1
	sh.mu.Lock()
	for i, c := range group {
		var ok, allowed bool
		switch c.Kind {
		case statestore.OpGCRA:
			ok = d.gcra(sh, &c.GCRA, now)
			allowed = c.GCRA.Allowed
		default: // statestore.OpQuota: SlotOf is -1 for every other kind
			var skew bool
			ok, skew = d.reserve(sh, &c.Quota, now, nodeMs)
			if skew {
				skewed = i
			}
			allowed = c.Quota.Allowed
		}
		if !ok {
			failed = true
			break
		}
		c.Done = true
		if !allowed {
			break
		}
	}
	sh.mu.Unlock()
	res := statestore.ResultOK
	if failed {
		res = statestore.ResultError
		for _, c := range group {
			c.Done, c.Err = false, statestore.ErrFailed
		}
	}
	d.count(stripe, trip.Label(first.Kind), res)
	for _, c := range group {
		d.countOps(stripe, c.Kind.Label(), 1)
	}
	if skewed >= 0 && d.skewLog.allow(d.node.Now()) {
		q := &group[skewed].Quota
		d.logger.WarnContext(ctx, "state store clock skew beyond half a quota window; call failed",
			"code", statestore.ErrFailed.Code(), "quota", q.Name, "window", q.Window.String())
	}
	return n
}

// Read runs Response Cache lookups as one pipelined batch (spec 08 req
// 45): per lookup the generation and the partition's names, variant
// generation and MAC-verified entry. Every other kind is an error reply.
func (d *Driver) Read(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) {
	if len(calls) == 0 {
		return
	}
	batch := -1
	if len(calls) > 1 {
		batch = 0
	}
	timeout := calls[0].Timeout
	for _, c := range calls {
		c.Done, c.Err, c.Elapsed, c.Batch, c.Trip = false, nil, 0, batch, statestore.TripPipeline
		timeout = min(timeout, c.Timeout)
	}
	stripe := stripeOf(rb)
	if err := d.precheck(ctx, rb, timeout); err != nil {
		for _, c := range calls {
			c.Err = err
			d.count(stripe, c.Kind.Label(), err.Result())
		}
		return
	}
	now := instantOf(d.server.Now())
	res := statestore.ResultOK
	for _, c := range calls {
		if c.Kind != statestore.OpCacheGet {
			c.Err, res = statestore.ErrFailed, statestore.ResultError
			continue
		}
		d.lookup(ctx, &c.Lookup, now)
		c.Done = true
		d.countOps(stripe, emit.StateOpCacheGet, 2)
	}
	d.count(stripe, emit.StateOpPipeline, res)
}

// Write applies one post-commit batch (spec 08 reqs 43 and 45): refunds,
// cache stores, leases (take, refresh, end-stale) and invalidations. A
// write whose timeout is 0 or whose context ended is not attempted
// (RZ-STS-004); the per-request deadline does not apply (spec 08 req 56).
// The attempted writes are one round trip, labeled pipeline when there
// are several.
func (d *Driver) Write(ctx context.Context, batch []*statestore.Write) {
	now := instantOf(d.server.Now())
	attempted, label := 0, -1
	res := statestore.ResultOK
	for _, w := range batch {
		w.Err, w.Applied = nil, false
		if err := d.precheckWrite(ctx, w.Timeout); err != nil {
			w.Err = err
			d.count(0, w.Kind.Label(), err.Result())
			continue
		}
		attempted++
		label = w.Kind.Label()
		ok := true
		switch w.Kind {
		case statestore.OpRefund:
			w.Applied = d.refund(&w.Refund, now)
		case statestore.OpCacheSet:
			if w.Lease.Mode != 0 {
				w.Applied, ok = d.lease(&w.Lease, now)
			} else {
				w.Applied, ok = d.store(&w.Cache, now)
			}
		case statestore.OpCacheInvalidate:
			w.Applied, ok = d.invalidate(w.Invalidate, now)
		default:
			ok = false
		}
		if !ok {
			w.Err, res = statestore.ErrFailed, statestore.ResultError
		}
		d.countOps(0, label, 1)
	}
	if attempted == 0 {
		return
	}
	if attempted > 1 {
		label = emit.StateOpPipeline
	}
	if label < 0 {
		label = emit.StateOpPipeline
	}
	d.count(0, label, res)
}

// precheckWrite is precheck for post-commit writes: the Policy timeout
// only.
func (d *Driver) precheckWrite(ctx context.Context, timeout time.Duration) *statestore.Error {
	if ctx.Err() != nil || timeout <= 0 {
		return statestore.ErrNotAttempted
	}
	if d.closed.Load() {
		return statestore.ErrFailed
	}
	return nil
}

// sweepLoop reclaims expired keys every interval until ctx ends.
func (d *Driver) sweepLoop(ctx context.Context, every time.Duration) {
	defer close(d.done)
	t := d.server.NewTimer(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
		}
		d.sweep(instantOf(d.server.Now()).ms)
		t.Reset(every)
	}
}

// sweep reclaims every expired key, one shard lock at a time.
func (d *Driver) sweep(nowMs int64) {
	for i := range d.shards {
		sh := &d.shards[i]
		sh.mu.Lock()
		d.bytes.Add(-sh.reclaim(nowMs))
		sh.mu.Unlock()
	}
}

// rateLog lets one log line through per interval.
type rateLog struct{ next atomic.Int64 }

// logEvery is the rate limit of the clock-skew and MAC warnings.
const logEvery = 10 * time.Second

func (r *rateLog) allow(now time.Time) bool {
	n, t := r.next.Load(), now.UnixNano()
	return t >= n && r.next.CompareAndSwap(n, t+int64(logEvery))
}

// admission applies the Response Cache memory rules to the one memory
// "shard" (spec 08 req 53): the reading reports maxmemory = MaxCacheBytes
// and used_memory = the cache entry bytes held.
type admission struct {
	mu      sync.Mutex
	read    bool
	at      time.Time
	used    int64
	blocked bool
	budget  int64
	every   time.Duration
}

// Memory rule targets (spec 08 reqs 52 and 53).
const (
	pollSlow      = 10 * time.Second
	pollFast      = time.Second
	fastAbovePct  = 50
	skipAbovePct  = 70
	growBlockPct  = 10
	growClearPct  = 2
	storeSharePct = 2 // of maxmemory per 10 s, divided by N_c
	maxStoreRate  = 4 << 20
	// storeNodes is N_c: 1,000 without a published Node count, always in
	// M1 (spec 08 req 53, spec 05 req 84), on the memory reading too
	// (spec 08 req 58: the cache rules apply unchanged).
	storeNodes = 1000
)

// AdmitStoreBytes applies the Response Cache memory rules (spec 08 req
// 53): skip above 70% of maxmemory, skip while the store grew by more than
// 10% of maxmemory between readings until growth falls under 2%, and
// otherwise allow at most min(2% of maxmemory per 10 s / N_c, 4 MiB per
// second) bytes between readings. Readings are taken every 10 s, or every
// 1 s above 50%.
func (d *Driver) AdmitStoreBytes(_ statestore.CacheKey, n int) bool {
	now := d.server.Now()
	a := &d.admit
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.read || now.Sub(a.at) >= a.every {
		used := d.bytes.Load()
		if a.read {
			switch grew := (used - a.used) * 100; {
			case grew > growBlockPct*d.maxBytes:
				a.blocked = true
			case grew < growClearPct*d.maxBytes:
				a.blocked = false
			}
		}
		a.every = pollSlow
		if used*100 > fastAbovePct*d.maxBytes {
			a.every = pollFast
		}
		rate := min(d.maxBytes*storeSharePct/100/int64(pollSlow/time.Second)/storeNodes, maxStoreRate)
		a.read, a.at, a.used = true, now, used
		a.budget = rate * int64(a.every/time.Second)
	}
	if a.used*100 > skipAbovePct*d.maxBytes || a.blocked || int64(n) > a.budget {
		return false
	}
	a.budget -= int64(max(n, 0))
	return true
}
