// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// Response Cache memory rule targets
// (docs/architecture/09-traffic-management-and-resilience.md "Response
// caching", rules 2 and 3).
const (
	fastAbovePct = 50
	skipAbovePct = 70
	growBlockPct = 10
	growClearPct = 2
	// storeSharePct of maxmemory per slow reading interval, divided by N_c.
	storeSharePct = 2
	maxStoreRate  = 4 << 20
	// countlessNodes is N_c without a published Node count, which no M1
	// Node has (the count arrives with Control mode in M2).
	countlessNodes = 1000
)

// Eviction policies: noeviction is the only maxmemory-policy limit keys
// tolerate; a Response Cache connection also tolerates volatile-ttl,
// which evicts the keys nearest expiry first, as an evicted generation key
// only turns its URI's entries into misses.
const (
	noEviction  = "noeviction"
	volatileTTL = "volatile-ttl"
)

// memory is one shard's latest INFO memory reading and the store bytes it
// still admits before the next.
type memory struct {
	mu sync.Mutex
	// read is set by the first reading.
	read bool
	// next is when the next reading is due.
	next time.Time
	// used and max are used_memory and maxmemory.
	used, max int64
	// policy is maxmemory_policy, "" when the reading lacks it.
	policy string
	// blocked holds stores while the shard grows too fast.
	blocked bool
	// budget is the store bytes left until the next reading.
	budget int64
}

// reading is one parsed INFO memory reply.
type reading struct {
	used, max int64
	policy    string
	ok        bool
}

// parseInfo parses the used_memory, maxmemory and maxmemory_policy fields
// of an INFO memory reply; ok is false without the first two.
func parseInfo(s string) reading {
	var r reading
	var hasUsed, hasMax bool
	for line := range strings.Lines(s) {
		line = strings.TrimRight(line, "\r\n")
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch k {
		case "used_memory":
			n, err := strconv.ParseInt(v, 10, 64)
			r.used, hasUsed = n, err == nil
		case "maxmemory":
			n, err := strconv.ParseInt(v, 10, 64)
			r.max, hasMax = n, err == nil
		case "maxmemory_policy":
			r.policy = v
		}
	}
	r.ok = hasUsed && hasMax && r.used >= 0 && r.max >= 0
	return r
}

// observe applies one reading taken at now: the growth since the previous
// reading blocks or clears stores, the next reading is due in the slow or
// fast interval, and the store budget refills for that interval.
func (m *memory) observe(now time.Time, r reading, slow, fast time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.read && r.max > 0 {
		switch grew := (r.used - m.used) * 100; {
		case grew > growBlockPct*r.max:
			m.blocked = true
		case grew < growClearPct*r.max:
			m.blocked = false
		}
	}
	every := slow
	if r.max > 0 && r.used*100 > fastAbovePct*r.max {
		every = fast
	}
	// Bytes per second: storeSharePct of maxmemory per slow interval,
	// divided by N_c, at most 4 MiB.
	slowSec := max(int64(slow/time.Second), 1)
	rate := min(r.max*storeSharePct/100/slowSec/countlessNodes, maxStoreRate)
	m.read, m.next = true, now.Add(every)
	m.used, m.max, m.policy = r.used, r.max, r.policy
	m.budget = rate * int64(every) / int64(time.Second)
}

// due reports whether a reading is due at now: at once before the first
// attempt, then at the time the last reading or failure set.
func (m *memory) due(now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !now.Before(m.next)
}

// postpone moves the next reading to now plus every, after a failed one.
func (m *memory) postpone(now time.Time, every time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next = now.Add(every)
}

// admit applies the store rules to n bytes.
func (m *memory) admit(n int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.read || m.max <= 0 || m.used*100 > skipAbovePct*m.max || m.blocked || int64(n) > m.budget {
		return false
	}
	m.budget -= int64(max(n, 0))
	return true
}

// eviction returns the reading's eviction policy: Unknown before the
// first reading or without the field.
func (m *memory) eviction() statestore.Tristate {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case !m.read || m.policy == "":
		return statestore.Unknown
	case m.policy == noEviction:
		return statestore.Yes
	default:
		return statestore.No
	}
}

// cacheEviction returns the reading's eviction policy as a Response Cache
// connection judges it: Yes for noeviction or volatile-ttl, Unknown before
// the first reading or without the field.
func (m *memory) cacheEviction() statestore.Tristate {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case !m.read || m.policy == "":
		return statestore.Unknown
	case m.policy == noEviction, m.policy == volatileTTL:
		return statestore.Yes
	default:
		return statestore.No
	}
}

// AdmitStoreBytes applies the Response Cache memory rules to the shard of
// key's partition: no store before its first reading, above 70% of
// maxmemory, while it grows too fast, or beyond the bytes the reading
// admits; a shard reporting maxmemory 0 admits none. Those rules (2 and 3
// of "Response caching") keep cache stores from filling the shards of
// limit keys, so a dedicated stateStore.cache connection (RoleCache)
// admits every store, filling its deployment up to maxmemory, where a
// noeviction server refuses writes and a volatile-ttl one evicts.
func (d *Driver) AdmitStoreBytes(key statestore.CacheKey, n int) bool {
	if d.closed.Load() {
		return false
	}
	if d.cfg.Role == statestore.RoleCache {
		return true
	}
	return d.shardOfSlot(keys.PartitionSlot(key)).mem.admit(n)
}
