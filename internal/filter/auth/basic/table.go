// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package basic

import (
	"sync"
	"time"
)

// Shards is the shard count of the success cache and of the username and
// source bucket tables (target).
const Shards = 64

// table is a sharded LRU of values keyed by HMAC-SHA-256 outputs under the
// throttle's per-process key: uniform and unknown to clients, so the first
// key byte picks the shard and no client can aim at one shard.
type table[V any] struct {
	shards [Shards]shard[V]
}

// shard is one shard: a map and a recency list, holding at most limit
// entries. Every method needs mu held.
type shard[V any] struct {
	mu    sync.Mutex
	m     map[[32]byte]*node[V]
	root  node[V] // sentinel of the recency list; root.next is the most recent
	limit int
}

// node is one entry. A node removed from its shard has nil links; a holder
// of its pointer may still update its value, which no lookup sees again.
type node[V any] struct {
	key        [32]byte
	val        V
	prev, next *node[V]
}

// init sizes t to capacity entries in all, spread over the shards so that
// their limits sum to capacity.
func (t *table[V]) init(capacity int) {
	for i := range t.shards {
		s := &t.shards[i]
		s.m = map[[32]byte]*node[V]{}
		s.root.prev, s.root.next = &s.root, &s.root
		s.limit = capacity / Shards
		if i < capacity%Shards {
			s.limit++
		}
	}
}

// shard returns the shard of k.
func (t *table[V]) shard(k [32]byte) *shard[V] { return &t.shards[int(k[0])%Shards] }

// len returns the number of entries; it locks every shard in turn.
func (t *table[V]) len() int {
	n := 0
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		n += len(s.m)
		s.mu.Unlock()
	}
	return n
}

// get returns the entry of k, marked most recent, or nil.
func (s *shard[V]) get(k [32]byte) *node[V] {
	n := s.m[k]
	if n != nil {
		s.unlink(n)
		s.pushFront(n)
	}
	return n
}

// add inserts a zero entry for k, which must be absent, as the most recent,
// first removing the least recent entry when the shard is full.
func (s *shard[V]) add(k [32]byte) *node[V] {
	if len(s.m) >= s.limit {
		if old := s.root.prev; old != &s.root {
			s.remove(old)
		}
	}
	n := &node[V]{key: k}
	s.m[k] = n
	s.pushFront(n)
	return n
}

// remove deletes n from the shard; removing a removed node does nothing.
func (s *shard[V]) remove(n *node[V]) {
	if n.next == nil {
		return
	}
	s.unlink(n)
	delete(s.m, n.key)
}

func (s *shard[V]) unlink(n *node[V]) {
	n.prev.next, n.next.prev = n.next, n.prev
	n.prev, n.next = nil, nil
}

func (s *shard[V]) pushFront(n *node[V]) {
	n.prev, n.next = &s.root, s.root.next
	s.root.next.prev = n
	s.root.next = n
}

// gcra is a token bucket in virtual-scheduling form (GCRA): one token
// refills every interval and the bucket holds burst tokens, so a take
// conforms while the theoretical arrival time tat is at most
// tolerance = (burst - 1) × interval ahead. A zero tat is a full bucket.
// Integer durations keep the arithmetic exact under a fake clock.
type gcra struct {
	interval, tolerance time.Duration
}

// newGCRA returns the bucket of burst tokens refilling one per interval.
func newGCRA(interval time.Duration, burst int) gcra {
	return gcra{interval: interval, tolerance: time.Duration(burst-1) * interval}
}

// wait returns how long after now the next token is available: 0 when one
// is.
func (g gcra) wait(tat, now time.Time) time.Duration {
	if !tat.After(now) {
		return 0
	}
	if d := tat.Sub(now); d > g.tolerance {
		return d - g.tolerance
	}
	return 0
}

// take takes the next token, available now or reserved for a waiter that
// sleeps wait first.
func (g gcra) take(tat *time.Time, now time.Time) {
	if tat.Before(now) {
		*tat = now
	}
	*tat = tat.Add(g.interval)
}

// refund returns a token taken earlier.
func (g gcra) refund(tat *time.Time) { *tat = tat.Add(-g.interval) }
