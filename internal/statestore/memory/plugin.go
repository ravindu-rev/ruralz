// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"math"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// Plugin guest keys (docs/architecture/05-wasm-plugin-system.md "Host
// Function rules", State Store), with the semantics of the ruralz_v1.lua
// library's p operation: an increment adds Delta to the key's integer,
// creating it at 0, and gives a key without an expiry TTL; a read
// returns the decimal text an increment left.

// pluginGet reads a guest key into ps under its shard's lock.
func (d *Driver) pluginGet(ps *statestore.PluginState, now instant) {
	sh := d.shardOf(int(keys.SlotString(ps.Key)))
	sh.mu.Lock()
	defer sh.mu.Unlock()
	ps.Found, ps.Value = false, nil
	e, ok := sh.plugin[ps.Key]
	if !ok || expired(e.exp, now.ms) {
		return
	}
	ps.Found, ps.Value = true, strconv.AppendInt(nil, e.n, 10)
}

// pluginIncr applies an increment in sh, whose lock the caller holds; false
// is an error reply: an overflowing integer, a TTL below 1 ms or a full
// shard.
func (d *Driver) pluginIncr(sh *shard, ps *statestore.PluginState, now instant) bool {
	ttl := ps.TTL.Milliseconds()
	if ttl < 1 {
		return false
	}
	e, ok := sh.plugin[ps.Key]
	if ok && expired(e.exp, now.ms) {
		delete(sh.plugin, ps.Key)
		ok = false
	}
	if !ok {
		if !d.room(sh, 1, now.ms) {
			return false
		}
		e = countEntry{exp: math.MaxInt64}
	}
	if (ps.Delta > 0 && e.n > math.MaxInt64-ps.Delta) || (ps.Delta < 0 && e.n < math.MinInt64-ps.Delta) {
		return false
	}
	e.n += ps.Delta
	if e.exp == math.MaxInt64 {
		e.exp = now.ms + ttl
		sh.track(e.exp)
	}
	sh.plugin[ps.Key] = e
	ps.N = e.n
	return true
}
