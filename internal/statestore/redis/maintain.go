// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"errors"
	"log/slog"
	"math/bits"
	"net"
	"time"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// errNotCluster reports a cluster seed that answered as a standalone
// server; rueidis would fall back to a single client, which the topology
// forbids.
var errNotCluster = errors.New("redis: the cluster seed is not a cluster")

// options returns the rueidis options of the State client: the URL's
// addresses, credentials, database, dial and write timeouts, client name
// and master set, with every connection dialed by the driver,
// auto-pipelining always on, client-side caching and retries off, and the
// per-shard connection bounds. A sentinel client asks the sentinels for
// the primary at start, on their +switch-master event, when its sentinel
// connection closes and every ShardsRefresh, authenticating to them with
// the URL's credentials, which a sentinel without a password accepts for
// its default user.
func (d *Driver) options() rueidis.ClientOption {
	opt := d.ep.opt
	opt.InitAddress = append([]string(nil), d.ep.opt.InitAddress...)
	opt.TLSConfig = nil
	if d.ep.tls {
		// rueidis reads the TLS ports of CLUSTER SHARDS when set; the
		// driver's dialer builds each server's own configuration.
		host, _, _ := net.SplitHostPort(opt.InitAddress[0])
		opt.TLSConfig = d.tlsFor(host)
	}
	opt.DialFn = nil
	opt.DialCtxFn = d.dial
	opt.DisableCache = true
	opt.DisableRetry = true
	opt.AlwaysPipelining = true
	opt.AlwaysRESP2 = false
	opt.PipelineMultiplex = multiplexOf(d.lim.PipelinedPerShard)
	opt.BlockingPoolSize = max(d.lim.DedicatedPerShard, 1)
	switch d.topo {
	case statestore.TopologyCluster:
		opt.Sentinel = rueidis.SentinelOption{}
		opt.ForceSingleClient = false
		opt.ClusterOption = rueidis.ClusterOption{ShardsRefreshInterval: d.lim.ShardsRefresh, MaxMovedRedirections: MaxRedirects}
	case statestore.TopologySentinel:
		opt.ForceSingleClient = false
		opt.Sentinel = rueidis.SentinelOption{
			Dialer:                  opt.Dialer,
			TLSConfig:               d.sentinelMark,
			Username:                d.ep.opt.Username,
			Password:                d.ep.opt.Password,
			MasterSet:               d.ep.opt.Sentinel.MasterSet,
			TopologyRefreshInterval: d.lim.ShardsRefresh,
		}
	default:
		opt.Sentinel = rueidis.SentinelOption{}
		opt.ForceSingleClient = true
	}
	return opt
}

// multiplexOf returns the rueidis PipelineMultiplex of n pipelined
// connections per shard: log2(n), rounding n down to a power of two.
func multiplexOf(n int) int {
	if n <= 1 {
		return 0
	}
	return bits.Len(uint(n)) - 1
}

// maintain runs the maintenance loop until ctx ends: it creates the State
// client, keeps a cluster's slot map, probes half-open breakers, reads
// INFO memory and retries pending script loads.
func (d *Driver) maintain(ctx context.Context) {
	defer close(d.maintDone)
	t := d.clk.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
		case <-d.wake:
		}
		d.tick(ctx)
		t.Reset(d.opts.Tick)
	}
}

// tick runs one maintenance pass.
func (d *Driver) tick(ctx context.Context) {
	c := d.loadClient()
	if c == nil {
		if d.unmapped != nil && d.unmapped.brk.ProbeDue() {
			d.probeUnmapped(ctx)
		} else {
			d.connectClient(ctx)
		}
		if c = d.loadClient(); c == nil {
			return
		}
	}
	if d.unmapped != nil {
		now := d.clk.Now()
		m := d.slots.Load()
		switch {
		case d.unmapped.brk.ProbeDue():
			// Probe until the breaker closes, even once the map is full
			// again, so it never keeps a half-open state.
			d.probeUnmapped(ctx)
		case m == nil || now.Sub(m.at) >= d.lim.ShardsRefresh,
			d.refresh.Load() && now.Sub(d.lastRefresh) >= minRefreshGap:
			d.refresh.Store(false)
			d.lastRefresh = now
			if err := d.refreshSlots(ctx, c); err != nil && d.clientLog.allow(now) {
				d.log.WarnContext(ctx, "state store slot map refresh failed", slog.String(catalog.KeyErrorType, errType(err)))
			}
		}
	}
	var buf [16]*shard
	for _, sh := range d.active(buf[:0]) {
		if sh == d.unmapped {
			continue
		}
		if sh.brk.ProbeDue() {
			d.probeShard(ctx, sh)
		}
		if sh.brk.State() == breaker.Open {
			continue
		}
		now := d.clk.Now()
		if !sh.mem.due(now) {
			continue
		}
		if sh.pending.Load() {
			d.requestScripts(ctx, sh)
		}
		d.readMemory(ctx, c, sh, now)
	}
}

// connectClient creates the State client once: standalone, rueidis
// returns it even when the first dial fails and redials on demand; a
// cluster client needs a seed to answer CLUSTER SLOTS, and a sentinel
// client a sentinel naming a primary that answers ROLE as master, else the
// next tick tries again, its dials paced. It reports whether a client is
// published.
func (d *Driver) connectClient(ctx context.Context) bool {
	if d.loadClient() != nil {
		return true
	}
	c, err := rueidis.NewClient(d.options())
	if err != nil && d.topo != statestore.TopologyStandalone {
		// Only ForceSingleClient returns a live client beside an error;
		// the sentinel constructor, and a cluster's single-client fallback,
		// return a nil pointer inside the interface, which is not nil.
		c = nil
	}
	if c == nil {
		if d.clientLog.allow(d.clk.Now()) {
			d.log.WarnContext(ctx, "state store client not created; retrying", slog.String(catalog.KeyErrorType, errType(err)))
		}
		return false
	}
	if d.topo == statestore.TopologyCluster && c.Mode() != rueidis.ClientModeCluster {
		c.Close()
		d.unmapped.brk.ConnectFailed()
		if d.clientLog.allow(d.clk.Now()) {
			d.log.WarnContext(ctx, "state store cluster seed answered as a standalone server; retrying",
				slog.String(catalog.KeyErrorType, errType(errNotCluster)))
		}
		return false
	}
	if d.unmapped != nil {
		// Read the slot map before publishing the client, so no call is
		// charged to the unmapped shard once the client exists.
		now := d.clk.Now()
		d.lastRefresh = now
		if err := d.refreshSlots(ctx, c); err != nil && d.clientLog.allow(now) {
			d.log.WarnContext(ctx, "state store slot map refresh failed", slog.String(catalog.KeyErrorType, errType(err)))
		}
	}
	if d.closed.Load() {
		c.Close()
		return false
	}
	d.client.Store(&clientRef{c: c})
	d.readyOnce.Do(func() { close(d.ready) })
	return true
}

// probeUnmapped is the unmapped shard's probe: creating the cluster
// client, or reading the slot map, succeeds when every slot has an owner.
func (d *Driver) probeUnmapped(ctx context.Context) {
	var t breaker.Ticket
	sh := d.unmapped
	if !sh.brk.Allow(&t) {
		return
	}
	ok := false
	if c := d.loadClient(); c == nil {
		ok = d.connectClient(ctx)
	} else {
		d.refresh.Store(false)
		d.lastRefresh = d.clk.Now()
		ok = d.refreshSlots(ctx, c) == nil
	}
	if m := d.slots.Load(); ok && (m == nil || !m.full) {
		ok = false
	}
	sh.brk.Done(&t, ok)
}

// probeShard sends PING to a half-open shard as its probe, bounded by the
// Gateway's stateStore.timeout; PONG is a success.
func (d *Driver) probeShard(ctx context.Context, sh *shard) {
	if d.closed.Load() {
		return
	}
	tr := d.getTrip()
	if d.unmapped != nil {
		tr.node = sh.addr
	}
	shs := [1]*shard{sh}
	tr.setShards(shs[:])
	if err := d.admit(tr); err != nil {
		d.putTrip(tr)
		return
	}
	tr.cmd(0, "PING")
	if !d.launch(ctx, tr) {
		d.putTrip(tr)
		return
	}
	if out, _ := tr.wait(ctx, d.probe); out != replied {
		return
	}
	if s, err := tr.res[0].ToString(); err != nil || s != "PONG" {
		tr.fail(0)
	}
	tr.settle(true)
	d.putTrip(tr)
}

// readMemory reads INFO memory from sh. A failed or incomplete reading is
// retried after the fast interval. The first reading without
// maxmemory_policy logs one WARN (eviction policy unknown), and the first
// with maxmemory 0 one WARN (no store admitted).
func (d *Driver) readMemory(ctx context.Context, c rueidis.Client, sh *shard, now time.Time) {
	node := d.nodeClient(c, sh.addr)
	if node == nil {
		sh.mem.postpone(now, d.lim.MemoryPollFast)
		return
	}
	actx, cancel := context.WithTimeout(ctx, d.opts.AdminTimeout)
	defer cancel()
	s, err := node.Do(actx, node.B().Info().Section("memory").Build()).ToString()
	r := parseInfo(s)
	if err != nil || !r.ok {
		sh.mem.postpone(now, d.lim.MemoryPollFast)
		return
	}
	sh.mem.observe(now, r, d.lim.MemoryPollSlow, d.lim.MemoryPollFast)
	if r.policy == "" && d.evictionWarned.CompareAndSwap(false, true) {
		d.log.WarnContext(ctx, "state store reading lacks maxmemory_policy; eviction policy unknown", slog.String(catalog.KeyShard, sh.id))
	}
	if r.max == 0 && d.unlimitedWarned.CompareAndSwap(false, true) {
		d.log.WarnContext(ctx, "state store reports maxmemory 0; Response Cache stores skip", slog.String(catalog.KeyShard, sh.id))
	}
}
