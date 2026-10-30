// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"errors"
	"maps"
	"math/rand/v2"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// TestCacheSizes covers NewCache: the target sizes by default, about
// 4 MiB of 64-byte entries in 64 shards, a random seed when none is
// given, and ErrCacheSize for fewer entries than shards.
func TestCacheSizes(t *testing.T) {
	c, err := NewCache(CacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.shards) != DefaultCacheShards || len(c.shards[0].entries)*len(c.shards) != DefaultCacheEntries || c.seed == 0 {
		t.Errorf("default cache: %d shards of %d entries, seed %d", len(c.shards), len(c.shards[0].entries), c.seed)
	}
	if n := len(c.shards[0].index); n < 2*len(c.shards[0].entries) || n&(n-1) != 0 {
		t.Errorf("index of %d slots for %d entries, want a power of two of at least twice", n, len(c.shards[0].entries))
	}
	if _, err := NewCache(CacheOptions{Entries: 3, Shards: 4}); !errors.Is(err, ErrCacheSize) {
		t.Errorf("3 entries in 4 shards: %v, want ErrCacheSize", err)
	}
	if c.Name() != CacheName {
		t.Errorf("Name() = %q", c.Name())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Run(ctx); err != nil {
		t.Errorf("Run = %v", err)
	}
}

// TestCacheEntrySize checks the entry layout behind the 4 MiB target.
func TestCacheEntrySize(t *testing.T) {
	if n := reflect.TypeFor[entry]().Size(); n != 64 {
		t.Errorf("entry is %d bytes, want 64", n)
	}
}

// TestCacheLife covers a cached denial: it answers its units and window
// end until min(stored + DenialLife, end), then is removed; a denial
// stored at or past its end is not kept; storing again replaces it in
// place, and forget removes it.
func TestCacheLife(t *testing.T) {
	c, err := NewCache(CacheOptions{Entries: 8, Shards: 1, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	d := statestore.DigestOf("acme")
	tag := c.tag(1, time.Hour, false, 5)
	now := testStart().UnixNano()
	end := now + int64(time.Hour)
	c.store(tag, &d, 2, end, now)
	if units, e, ok := c.lookup(tag, &d, now+int64(DenialLife)-1); !ok || units != 2 || e != end {
		t.Fatalf("lookup before 60 s = %d, %d, %v", units, e, ok)
	}
	if _, _, ok := c.lookup(tag, &d, now+int64(DenialLife)); ok || c.held() != 0 {
		t.Fatalf("a denial 60 s old answered or stayed (%d held)", c.held())
	}
	near := end - int64(10*time.Second)
	c.store(tag, &d, 0, end, near)
	if _, _, ok := c.lookup(tag, &d, end); ok {
		t.Error("a denial answered past its window's end")
	}
	c.store(tag, &d, 1, end, end)
	if c.held() != 0 {
		t.Error("a denial stored at its window's end was kept")
	}
	c.store(tag, &d, 4, end, now)
	c.store(tag, &d, 3, end, now)
	if units, _, ok := c.lookup(tag, &d, now); !ok || units != 3 || c.held() != 1 {
		t.Errorf("replaced denial = %d, %v with %d held; want 3 in one entry", units, ok, c.held())
	}
	c.forget(tag, &d)
	if _, _, ok := c.lookup(tag, &d, now); ok || c.held() != 0 {
		t.Error("forget left the denial")
	}
	c.forget(tag, &d)
}

// TestCacheTag covers the identity beside the key digest: another quota
// name, window, calendar month or limit is another denial.
func TestCacheTag(t *testing.T) {
	c, err := NewCache(CacheOptions{Entries: 8, Shards: 1, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	base := c.tag(1, time.Hour, false, 5)
	if base != c.tag(1, time.Hour, false, 5) {
		t.Fatal("tag is not stable")
	}
	for name, other := range map[string]uint64{
		"name": c.tag(2, time.Hour, false, 5), "window": c.tag(1, 2*time.Hour, false, 5),
		"month": c.tag(1, 0, true, 5), "month and window": c.tag(1, time.Hour, true, 5), "limit": c.tag(1, time.Hour, false, 6),
	} {
		if other == base {
			t.Errorf("another %s has the same tag", name)
		}
	}
	d := statestore.DigestOf("acme")
	now := testStart().UnixNano()
	c.store(base, &d, 0, now+int64(time.Hour), now)
	if _, _, ok := c.lookup(c.tag(1, time.Hour, false, 6), &d, now); ok {
		t.Error("a raised limit read the earlier limit's denial")
	}
}

// TestCacheClockEviction covers a full shard: the hand takes the first
// entry without a reference bit, clearing the bits it passes, so a denial
// requests find again survives; an ended entry goes first, referenced or
// not.
func TestCacheClockEviction(t *testing.T) {
	now := testStart().UnixNano()
	end := now + int64(time.Hour)
	keys := make([]statestore.Digest, 5)
	for i := range keys {
		keys[i] = statestore.DigestOf("k" + strconv.Itoa(i))
	}
	fill := func(ends ...int64) *Cache {
		c, err := NewCache(CacheOptions{Entries: 4, Shards: 1, Seed: 9})
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range ends {
			c.store(7, &keys[i], int64(i), e, now)
		}
		return c
	}
	present := func(c *Cache, at int64, want ...int) {
		t.Helper()
		for _, i := range want {
			if _, _, ok := c.lookup(7, &keys[i], at); !ok {
				t.Errorf("denial %d was evicted", i)
			}
		}
	}

	c := fill(end, end, end, end)
	c.lookup(7, &keys[0], now)
	c.store(7, &keys[4], 4, end, now)
	if _, _, ok := c.lookup(7, &keys[1], now); ok {
		t.Error("CLOCK kept the first denial without a reference")
	}
	present(c, now, 0, 2, 3, 4)

	c = fill(end, now+int64(time.Second), end, end)
	c.lookup(7, &keys[0], now)
	c.lookup(7, &keys[1], now)
	later := now + int64(2*time.Second)
	c.lookup(7, &keys[2], later)
	c.lookup(7, &keys[3], later)
	c.store(7, &keys[4], 4, end, later)
	if c.held() != 4 {
		t.Errorf("%d held, want the shard's 4", c.held())
	}
	present(c, later, 0, 2, 3, 4)
}

// TestCacheIndexMatchesModel drives random stores, lookups and forgets
// against a map: the cache holds exactly the model's denials with their
// units, each reachable through the index, and a store loses one other
// denial only when its shard is full.
func TestCacheIndexMatchesModel(t *testing.T) {
	const capacity = 16
	c, err := NewCache(CacheOptions{Entries: capacity, Shards: 1, Seed: 21})
	if err != nil {
		t.Fatal(err)
	}
	model := map[statestore.Digest]int64{}
	rng := rand.New(rand.NewPCG(1, 2))
	keys := make([]statestore.Digest, 40)
	for i := range keys {
		keys[i] = statestore.DigestOf("key-" + strconv.Itoa(i))
	}
	now := testStart().UnixNano()
	end := now + int64(time.Hour)
	sh := &c.shards[0]
	// heldNow returns the shard's denials by key, each checked reachable.
	heldNow := func(step int) map[statestore.Digest]int64 {
		out := map[statestore.Digest]int64{}
		for pos := range sh.used {
			e := &sh.entries[pos]
			if e.expires == 0 {
				continue
			}
			if _, at := c.find(sh, e.tag, &e.digest); at != pos {
				t.Fatalf("step %d: entry %d is not reachable through the index", step, pos)
			}
			out[e.digest] = e.units
		}
		return out
	}
	for step := range 4000 {
		k := keys[rng.IntN(len(keys))]
		switch rng.IntN(3) {
		case 0:
			_, had := model[k]
			full := c.held() == capacity
			units := int64(rng.IntN(100))
			c.store(5, &k, units, end, now)
			model[k] = units
			if !had && full {
				lost := 0
				got := heldNow(step)
				for d := range model {
					if _, ok := got[d]; !ok {
						delete(model, d)
						lost++
					}
				}
				if lost != 1 {
					t.Fatalf("step %d: a store into a full shard lost %d denials, want 1", step, lost)
				}
			}
		case 1:
			c.forget(5, &k)
			delete(model, k)
		default:
			units, e, ok := c.lookup(5, &k, now)
			if want, in := model[k]; ok != in || (ok && (units != want || e != end)) {
				t.Fatalf("step %d: lookup = %d, %d, %v; model %d, %v", step, units, e, ok, want, in)
			}
		}
		if got := heldNow(step); !maps.Equal(got, model) {
			t.Fatalf("step %d: cache holds %v, model %v", step, got, model)
		}
	}
}

// TestCacheJitter covers the Retry-After spread: whole milliseconds from 0
// to its limit, reaching both halves, the same sequence for one seed, and
// none for a limit below 1 ms.
func TestCacheJitter(t *testing.T) {
	a, _ := NewCache(CacheOptions{Entries: 1, Shards: 1, Seed: 5})
	b, _ := NewCache(CacheOptions{Entries: 1, Shards: 1, Seed: 5})
	low, high := false, false
	for range 1000 {
		j := a.jitter(36 * time.Second)
		if j < 0 || j > 36*time.Second || j%time.Millisecond != 0 {
			t.Fatalf("jitter %v outside [0, 36s] in whole milliseconds", j)
		}
		if b.jitter(36*time.Second) != j {
			t.Fatal("two caches of one seed drew apart")
		}
		low, high = low || j < 18*time.Second, high || j > 18*time.Second
	}
	if !low || !high {
		t.Errorf("1000 draws reached the lower half %v and the upper %v", low, high)
	}
	if a.jitter(time.Microsecond) != 0 || a.jitter(0) != 0 {
		t.Error("a limit below 1 ms spread")
	}
}
