// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Denial cache targets (docs/architecture/09-traffic-management-and-resilience.md
// "Quotas", docs/architecture/11-scalability-and-distributed-state.md
// "Stateless data plane", docs/architecture/12-performance-budgets-and-benchmarking.md
// "Quota denial cache").
const (
	// DefaultCacheEntries is the Node-wide entry count, about 4 MiB
	// (target).
	DefaultCacheEntries = 65_536
	// DefaultCacheShards is the number of short-mutex shards.
	DefaultCacheShards = 64
	// DenialLife is how long a Node keeps a denial, never past its
	// window's end (target).
	DenialLife = 60 * time.Second
)

// CacheName is the Component name of the denial cache.
const CacheName = "quota-denial-cache"

// ErrCacheSize reports CacheOptions too small for their shard count.
var ErrCacheSize = errors.New("quota: denial cache too small for its shards")

// entry is one cached denial, 64 bytes: the key digest and a tag over
// the Consumer quota's name, window and limit identify it; units is what
// the window had left, end the window's end and expires when the entry
// stops answering (Unix ns).
type entry struct {
	digest  statestore.Digest
	tag     uint64
	units   int64
	end     int64
	expires int64
}

// shard is one lock's worth of the cache: entries allocated at their full
// count, so an entry never moves, their CLOCK reference bits, a CLOCK
// hand, the positions a removal freed, and an open-addressing index whose
// slots hold 0 for empty, else 1 + an entry's position.
type shard struct {
	mu      sync.Mutex
	entries []entry
	ref     []bool
	used    int
	hand    int
	free    []uint32
	index   []uint32
	mask    uint64
	_       [64]byte // keeps neighboring shard locks off one cache line
}

// CacheOptions size a Cache; only tests change them.
type CacheOptions struct {
	// Entries is the entry count (DefaultCacheEntries).
	Entries int
	// Shards is the shard count (DefaultCacheShards).
	Shards int
	// Seed keys the tag, shard and index hashes and the Retry-After
	// jitter; 0 draws one from crypto/rand, which tests replace with a
	// fixed value.
	Seed uint64
}

// DefaultCacheOptions returns the target sizes.
func DefaultCacheOptions() CacheOptions {
	return CacheOptions{Entries: DefaultCacheEntries, Shards: DefaultCacheShards}
}

// Cache is the Node-wide Quota denial cache
// (docs/architecture/09-traffic-management-and-resilience.md "Quotas"):
// each entry holds the units a denied window had left, keyed with the
// Consumer quota's limit so a raised limit applies at once, for at most
// DenialLife and never past the window's end; a request whose cost
// exceeds those units is denied without a script. CLOCK evicts in each
// shard, so an evicted or lost denial costs one more script. It outlives
// snapshots and is safe for concurrent use; it is a filter.Component whose
// Run does no work.
type Cache struct {
	seed   uint64
	shards []shard
	// jit counts jitter draws, a splitmix64 sequence from seed.
	jit atomic.Uint64
}

// NewCache returns an empty cache sized by o, zero fields taking the
// target sizes.
func NewCache(o CacheOptions) (*Cache, error) {
	def := DefaultCacheOptions()
	if o.Entries <= 0 {
		o.Entries = def.Entries
	}
	if o.Shards <= 0 {
		o.Shards = def.Shards
	}
	per := o.Entries / o.Shards
	if per < 1 || per >= 1<<31 {
		return nil, ErrCacheSize
	}
	seed := o.Seed
	if seed == 0 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		seed = binary.LittleEndian.Uint64(b[:]) | 1
	}
	c := &Cache{seed: seed, shards: make([]shard, o.Shards)}
	slots := 1
	for slots < 2*per {
		slots <<= 1
	}
	for i := range c.shards {
		sh := &c.shards[i]
		sh.entries = make([]entry, per)
		sh.ref = make([]bool, per)
		sh.index = make([]uint32, slots)
		sh.mask = uint64(len(sh.index)) - 1
	}
	return c, nil
}

// Name implements filter.Component.
func (*Cache) Name() string { return CacheName }

// Run implements filter.Component: the cache needs no goroutine, since
// CLOCK evicts on insert and entries end when read.
func (*Cache) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// mix is the splitmix64 finalizer.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// golden is the splitmix64 increment.
const golden = 0x9e3779b97f4a7c15

// tag returns the identity of one Consumer quota's denials beside the key
// digest: the quota name's hash name, its window (0 with month) and its
// limit, so a changed window or limit never reads an earlier denial.
func (c *Cache) tag(name uint64, window time.Duration, month bool, limit int64) uint64 {
	h := mix(name ^ c.seed)
	h = mix(h ^ uint64(window)) //nolint:gosec // G115: a bit pattern, not a quantity.
	if month {
		h = mix(h ^ golden)
	}
	return mix(h ^ uint64(limit)) //nolint:gosec // G115: as above.
}

// shardOf returns the shard of a denial.
func (c *Cache) shardOf(tag uint64, d *statestore.Digest) *shard {
	h := mix(binary.LittleEndian.Uint64(d[8:16]) ^ tag ^ c.seed)
	return &c.shards[h%uint64(len(c.shards))]
}

// home returns the index slot a denial hashes to.
func (c *Cache) home(sh *shard, tag uint64, d *statestore.Digest) uint64 {
	return mix(binary.LittleEndian.Uint64(d[0:8])^tag*golden^c.seed) & sh.mask
}

// find returns the index slot of a denial and its position, -1 when the
// shard holds none.
func (c *Cache) find(sh *shard, tag uint64, d *statestore.Digest) (uint64, int) {
	for i := c.home(sh, tag, d); ; i = (i + 1) & sh.mask {
		v := sh.index[i]
		if v == 0 {
			return 0, -1
		}
		if e := &sh.entries[v-1]; e.tag == tag && e.digest == *d {
			return i, int(v - 1)
		}
	}
}

// unlink removes index slot i, shifting the rest of its probe run back so
// no lookup stops early.
func (c *Cache) unlink(sh *shard, i uint64) {
	for j := (i + 1) & sh.mask; ; j = (j + 1) & sh.mask {
		v := sh.index[j]
		if v == 0 {
			break
		}
		e := &sh.entries[v-1]
		k := c.home(sh, e.tag, &e.digest)
		// Move v back to i when its home is not cyclically within (i, j].
		if (j > i && (k <= i || k > j)) || (j < i && k <= i && k > j) {
			sh.index[i] = v
			i = j
		}
	}
	sh.index[i] = 0
}

// remove drops the entry at index slot slot, position pos.
func (c *Cache) remove(sh *shard, slot uint64, pos int) {
	c.unlink(sh, slot)
	sh.entries[pos], sh.ref[pos] = entry{}, false
	sh.free = append(sh.free, uint32(pos)) //nolint:gosec // G115: positions fit 31 bits.
}

// lookup returns the units and window end of a live denial at now (Unix
// ns), marking it referenced; a denial past its end is removed.
func (c *Cache) lookup(tag uint64, d *statestore.Digest, now int64) (units, end int64, ok bool) {
	sh := c.shardOf(tag, d)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	slot, pos := c.find(sh, tag, d)
	if pos < 0 {
		return 0, 0, false
	}
	e := &sh.entries[pos]
	if now >= e.expires {
		c.remove(sh, slot, pos)
		return 0, 0, false
	}
	sh.ref[pos] = true
	return e.units, e.end, true
}

// store caches a denial at now: units the window had left, its end; it
// answers until min(now + DenialLife, end). A denial already held is
// replaced in place; a full shard evicts by CLOCK.
func (c *Cache) store(tag uint64, d *statestore.Digest, units, end, now int64) {
	expires := min(now+int64(DenialLife), end)
	if expires <= now {
		c.forget(tag, d)
		return
	}
	sh := c.shardOf(tag, d)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, pos := c.find(sh, tag, d); pos >= 0 {
		e := &sh.entries[pos]
		e.units, e.end, e.expires = units, end, expires
		return
	}
	var pos int
	switch {
	case len(sh.free) > 0:
		pos = int(sh.free[len(sh.free)-1])
		sh.free = sh.free[:len(sh.free)-1]
	case sh.used < len(sh.entries):
		pos = sh.used
		sh.used++
	default:
		pos = c.evict(sh, now)
	}
	sh.entries[pos] = entry{digest: *d, tag: tag, units: units, end: end, expires: expires}
	sh.ref[pos] = false
	i := c.home(sh, tag, d)
	for sh.index[i] != 0 {
		i = (i + 1) & sh.mask
	}
	sh.index[i] = uint32(pos + 1) //nolint:gosec // G115: positions fit 31 bits.
}

// forget removes a denial, as after an admission of its key.
func (c *Cache) forget(tag uint64, d *statestore.Digest) {
	sh := c.shardOf(tag, d)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if slot, pos := c.find(sh, tag, d); pos >= 0 {
		c.remove(sh, slot, pos)
	}
}

// evict frees one entry of a full shard by CLOCK: the hand takes the
// first entry that has ended, or whose reference bit it finds clear,
// clearing the bit of each referenced entry it passes. An entry gets its
// bit when a request finds it again, not when it is stored.
func (c *Cache) evict(sh *shard, now int64) int {
	for {
		pos := sh.hand
		sh.hand = (sh.hand + 1) % len(sh.entries)
		e := &sh.entries[pos]
		if now < e.expires && sh.ref[pos] {
			sh.ref[pos] = false
			continue
		}
		if slot, at := c.find(sh, e.tag, &e.digest); at == pos {
			c.unlink(sh, slot)
		}
		*e, sh.ref[pos] = entry{}, false
		return pos
	}
}

// jitter returns a uniform duration from 0 to limit in whole
// milliseconds, the spread of a denial's Retry-After past its window's
// end.
func (c *Cache) jitter(limit time.Duration) time.Duration {
	ms := limit.Milliseconds()
	if ms <= 0 {
		return 0
	}
	x := mix(c.seed + c.jit.Add(1)*golden)
	return time.Duration(x%uint64(ms+1)) * time.Millisecond //nolint:gosec // G115: below ms + 1, a positive int64.
}

// held returns the number of held entries, live or ended; tests read it.
func (c *Cache) held() int {
	n := 0
	for i := range c.shards {
		sh := &c.shards[i]
		sh.mu.Lock()
		n += sh.used - len(sh.free)
		sh.mu.Unlock()
	}
	return n
}
