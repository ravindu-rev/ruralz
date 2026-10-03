// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"math"
	"slices"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// epoch2026 is 2026-01-01T00:00:00Z in Unix seconds, the GCRA time base
// (spec 08 req 41): microseconds since it stay exact in a float64.
const epoch2026 = 1767225600

// instant is one reading of the server clock in the units the scripts
// use: GCRA microseconds since the 2026 epoch, Unix milliseconds for
// Quota windows and key expiry, Unix microseconds for generations.
type instant struct {
	us     float64
	ms     int64
	unixUs int64
}

// instantOf reads t as Redis TIME would report it (seconds and
// microseconds).
func instantOf(t time.Time) instant {
	sec, usec := t.Unix(), int64(t.Nanosecond()/1000)
	return instant{
		us:     float64((sec-epoch2026)*1_000_000 + usec),
		ms:     sec*1000 + usec/1000,
		unixUs: sec*1_000_000 + usec,
	}
}

// expired reports whether a key whose expiry is exp (Unix ms) is gone at
// nowMs; like Redis, a key lives through its expiry millisecond.
func expired(exp, nowMs int64) bool { return nowMs > exp }

// rlKey locates one GCRA TAT: rz:rl:<policy>:<requests>/<window>:{<digest>}.
type rlKey struct {
	policy   string
	requests int64
	window   time.Duration
	digest   statestore.Digest
}

// qtKey locates one Quota counter:
// rz:qt:<name>:<window>:<window start>:{<digest>}.
type qtKey struct {
	name   string
	window time.Duration
	start  int64 // Unix ms
	digest statestore.Digest
}

// tatEntry is a stored TAT (microseconds since the 2026 epoch).
type tatEntry struct {
	tat float64
	exp int64
}

// countEntry is a stored integer: a Quota count or a generation.
type countEntry struct {
	n   int64
	exp int64
}

// leaseEntry is a partition's fill or revalidation lease.
type leaseEntry struct {
	token uint64
	exp   int64
}

// maxVariants is the most variants a partition keeps (spec 08 req 45).
const maxVariants = 8

// variant is one stored variant: the fields g:<V>, t:<V> and e:<V>.
type variant struct {
	v     statestore.Digest
	gen   int64
	t     int64  // store time, server ms
	entry []byte // stored form: entry, then its MAC tag when keyed
}

// partition is the hash rz:rc:{<U>:<P>}: names n, variants in store order
// (field o, oldest first).
type partition struct {
	names    string
	variants []variant
	exp      int64
	bytes    int64
}

// find returns the index of variant v, or -1.
func (p *partition) find(v statestore.Digest) int {
	for i := range p.variants {
		if p.variants[i].v == v {
			return i
		}
	}
	return -1
}

// remove drops variant i and returns its bytes. slices.Delete zeroes the
// vacated tail slot, so the backing array keeps no reference to a dropped
// entry the byte accounting no longer counts (spec 08 req 58).
func (p *partition) remove(i int) int64 {
	n := int64(len(p.variants[i].entry))
	p.variants = slices.Delete(p.variants, i, i+1)
	p.bytes -= n
	return n
}

// shard is one lock's worth of keys. Keys are placed by the Redis Cluster
// slot of their hash tag, so one script's keys share a shard.
type shard struct {
	mu    sync.Mutex
	rl    map[rlKey]tatEntry
	qt    map[qtKey]countEntry
	gen   map[statestore.Digest]countEntry
	part  map[statestore.CacheKey]*partition
	lease map[statestore.CacheKey]leaseEntry
	// next is a lower bound of every held key's expiry (Unix ms),
	// math.MaxInt64 when none is held: writes lower it (track) and
	// reclaim recomputes it, so no key can have expired while the server
	// time is at or before it.
	next int64
	_    [64]byte // keeps neighboring shard locks off one cache line
}

func (s *shard) init() {
	s.rl = map[rlKey]tatEntry{}
	s.qt = map[qtKey]countEntry{}
	s.gen = map[statestore.Digest]countEntry{}
	s.part = map[statestore.CacheKey]*partition{}
	s.lease = map[statestore.CacheKey]leaseEntry{}
	s.next = math.MaxInt64
}

// track records a key expiry (Unix ms) just written; every write of an
// expiry calls it. The caller holds s.mu.
func (s *shard) track(exp int64) { s.next = min(s.next, exp) }

// keys returns the number of keys held, expired ones included.
func (s *shard) keys() int { return len(s.rl) + len(s.qt) + len(s.gen) + len(s.part) + len(s.lease) }

// reclaim deletes every expired key, recomputes s.next from the keys
// left, and returns the cache entry bytes freed; the caller holds s.mu
// and subtracts them from the driver total.
func (s *shard) reclaim(nowMs int64) int64 {
	next := int64(math.MaxInt64)
	for k, e := range s.rl {
		if expired(e.exp, nowMs) {
			delete(s.rl, k)
		} else {
			next = min(next, e.exp)
		}
	}
	for k, e := range s.qt {
		if expired(e.exp, nowMs) {
			delete(s.qt, k)
		} else {
			next = min(next, e.exp)
		}
	}
	for k, e := range s.gen {
		if expired(e.exp, nowMs) {
			delete(s.gen, k)
		} else {
			next = min(next, e.exp)
		}
	}
	for k, e := range s.lease {
		if expired(e.exp, nowMs) {
			delete(s.lease, k)
		} else {
			next = min(next, e.exp)
		}
	}
	var freed int64
	for k, p := range s.part {
		if expired(p.exp, nowMs) {
			freed += p.bytes
			delete(s.part, k)
		} else {
			next = min(next, p.exp)
		}
	}
	s.next = next
	return freed
}

// room reports whether the shard can take n new keys, reclaiming expired
// ones first when it is full (spec 08 req 58: a shard full of unexpired
// keys refuses new ones, like noeviction). The reclaim scan runs only
// once the server time passed s.next, when some key may have expired; a
// shard full of live keys therefore refuses in O(1), and since every
// expiry written is in the future, a full shard scans at most once per
// server millisecond under a flood of new keys.
func (d *Driver) room(s *shard, n int, nowMs int64) bool {
	if n <= 0 || s.keys()+n <= d.perShard {
		return true
	}
	if expired(s.next, nowMs) {
		d.bytes.Add(-s.reclaim(nowMs))
	}
	return s.keys()+n <= d.perShard
}

// shardOf returns the shard of a hash slot.
func (d *Driver) shardOf(slot int) *shard { return &d.shards[slot%len(d.shards)] }
