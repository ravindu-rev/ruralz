// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// MaxShards bounds the shards one driver tracks by address: primaries,
// the replicas and seeds rueidis dials to read the slot map, and the
// addresses redirects name (a Cell holds at most 16 shards). Addresses
// beyond it share one overflow entry.
const MaxShards = 256

// shard is one server the driver dials: its breaker and reconnect pacer,
// its script and memory state. Primaries serve calls; other addresses only
// pace their dials.
type shard struct {
	// id is ss-<8 hex>, the only name logs use.
	id string
	// addr is host:port as rueidis dials it; never logged.
	addr string
	// host is the netaddr.CanonicalHost form of the shard's server host,
	// which its calls' zone label looks up in the zone map: addr's host,
	// or for the sentinel topology's primary shard the host of the
	// primary the sentinels named last; nil without a server (the
	// unmapped and overflow shards, a sentinel primary not yet named).
	host atomic.Pointer[string]

	brk  *breaker.Breaker
	pace *breaker.Pacer

	// scripts tracks SCRIPT LOAD: loading is set while a load runs,
	// again while a newer request waits, noScripts once the server
	// answered that the command is unknown or not permitted.
	loading   atomic.Bool
	again     atomic.Bool
	pending   atomic.Bool
	noScripts atomic.Bool

	mem memory

	// reconnects spaces the READONLY reconnects (minRefreshGap).
	reconnects rateLog

	connLog, scriptLog, reconnectLog rateLog
}

// shardID returns ss- and the first 8 hex digits of the SHA-256 of addr.
func shardID(addr string) string {
	sum := sha256.Sum256([]byte(addr))
	return "ss-" + hex.EncodeToString(sum[:4])
}

// newShard returns the shard of addr with a closed breaker and a pacer
// without failures.
func (d *Driver) newShard(addr string) *shard {
	sh := &shard{id: shardID(addr), addr: addr}
	lim := d.lim
	sh.brk = breaker.New(d.clk, breaker.Config{
		Window: lim.BreakerWindow, MinCalls: lim.BreakerMinCalls, FailureRatio: lim.BreakerFailureRatio,
		ConnectFailures: lim.BreakerConnectFailures, OpenMin: lim.BreakerOpenMin, OpenMax: lim.BreakerOpenMax,
		CloseAfter: lim.BreakerCloseAfter,
		OnChange:   func(from, to breaker.State) { d.breakerChanged(sh, from, to) },
	}, d.opts.Rand)
	sh.pace = breaker.NewPacer(d.clk, breaker.PacerConfig{
		Base: lim.ReconnectBase, Cap: lim.ReconnectCap, PerSecond: lim.ConnectsPerSecond,
	}, d.opts.Rand)
	return sh
}

// breakerChanged logs a breaker transition: open at WARN, close at INFO,
// half-open at DEBUG.
func (d *Driver) breakerChanged(sh *shard, from, to breaker.State) {
	switch to {
	case breaker.Open:
		d.log.Warn("state store breaker opened", slog.String(catalog.KeyShard, sh.id), slog.String("from", from.String()))
	case breaker.Closed:
		d.log.Info("state store breaker closed", slog.String(catalog.KeyShard, sh.id))
	default:
		d.log.Debug("state store breaker half-open", slog.String(catalog.KeyShard, sh.id))
	}
}

// registry holds the shards by address, at most MaxShards.
type registry struct {
	mu       sync.Mutex
	byAddr   map[string]*shard
	overflow *shard
}

// shardAt returns the shard of addr, creating it; beyond MaxShards it
// returns the overflow shard.
func (d *Driver) shardAt(addr string) *shard {
	r := &d.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	if sh := r.byAddr[addr]; sh != nil {
		return sh
	}
	if len(r.byAddr) >= MaxShards {
		if r.overflow == nil {
			r.overflow = d.newShard("overflow")
		}
		return r.overflow
	}
	sh := d.newShard(addr)
	sh.setHost(addr)
	r.byAddr[addr] = sh
	return sh
}

// slotMap is one reading of a cluster's slot map: owner[s] indexes
// shards, 0 being the unmapped shard.
type slotMap struct {
	owner  [keys.Slots]uint16
	shards []*shard
	// full is true when every slot has an owner.
	full bool
	// at is when the map was read, on the driver's clock.
	at time.Time
}

// shardOfSlot returns the shard serving slot: the standalone server, or
// the cluster primary owning it, else the unmapped shard.
func (d *Driver) shardOfSlot(slot uint16) *shard {
	if d.single != nil {
		return d.single
	}
	m := d.slots.Load()
	if m == nil {
		return d.unmapped
	}
	return m.shards[m.owner[slot%keys.Slots]]
}

// active returns the shards that serve calls: the standalone server, or a
// cluster's primaries, with the unmapped shard while some slot has no
// known owner.
func (d *Driver) active(dst []*shard) []*shard {
	if d.single != nil {
		return append(dst, d.single)
	}
	m := d.slots.Load()
	if m == nil {
		return append(dst, d.unmapped)
	}
	if !m.full {
		dst = append(dst, d.unmapped)
	}
	return append(dst, m.shards[1:]...)
}

// rateLog lets one event through per interval.
type rateLog struct{ next atomic.Int64 }

// logEvery is the rate limit of repeated warnings.
const logEvery = 10 * time.Second

// allow lets one log line through per logEvery.
func (r *rateLog) allow(now time.Time) bool { return r.allowEvery(now, logEvery) }

// allowEvery lets one event through per every.
func (r *rateLog) allowEvery(now time.Time, every time.Duration) bool {
	n, t := r.next.Load(), now.UnixNano()
	return t >= n && r.next.CompareAndSwap(n, t+int64(every))
}
