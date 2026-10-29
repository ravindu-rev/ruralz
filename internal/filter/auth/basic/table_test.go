// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package basic

import (
	"testing"
	"time"
)

// TestTableCapacity checks the per-Node sizes: the success cache holds
// CacheEntries and the username LRU UserBuckets entries in all, spread over
// Shards shards whose limits differ by at most one.
func TestTableCapacity(t *testing.T) {
	for _, capacity := range []int{CacheEntries, UserBuckets, SourceBuckets} {
		var tb table[bucket]
		tb.init(capacity)
		sum, lo, hi := 0, capacity, 0
		for i := range tb.shards {
			l := tb.shards[i].limit
			sum, lo, hi = sum+l, min(lo, l), max(hi, l)
		}
		if sum != capacity || hi-lo > 1 {
			t.Errorf("capacity %d: limits sum to %d, range [%d, %d]", capacity, sum, lo, hi)
		}
	}
}

// keyIn returns a key of shard 0 numbered n.
func keyIn(n byte) [32]byte {
	var k [32]byte
	k[0], k[1] = Shards, n
	return k
}

// TestTableLRU checks the LRU of a full shard: adding evicts the least
// recently used entry, a lookup makes an entry the most recent, and an
// evicted key comes back as a new zero entry (an evicted bucket returns
// full).
func TestTableLRU(t *testing.T) {
	var tb table[bucket]
	tb.init(2 * Shards)
	a, b, c := keyIn(1), keyIn(2), keyIn(3)
	s := tb.shard(a)
	if s != tb.shard(b) || s.limit != 2 {
		t.Fatal("fixture keys are not in one shard of two entries")
	}
	s.add(a).val.tat = t0
	s.add(b)
	s.get(a)
	s.add(c)
	if s.get(b) != nil || s.get(a) == nil || s.get(c) == nil || len(s.m) != 2 {
		t.Fatal("the least recently used entry was not the one evicted")
	}
	evicted := s.m[a]
	s.add(b) // evicts a, now the least recent
	if s.get(a) != nil {
		t.Fatal("a was not evicted")
	}
	s.remove(evicted) // removing a removed node does nothing
	if len(s.m) != 2 || s.get(b) == nil || s.get(c) == nil {
		t.Fatal("removing an evicted node changed the shard")
	}
	s.remove(s.get(c))
	if n := s.add(a); !n.val.tat.IsZero() || len(s.m) != 2 {
		t.Fatal("an evicted key did not come back as a new zero entry")
	}
}

// TestGCRA checks the token bucket: burst tokens at once, then one per
// interval; a reserved token pushes the next one an interval later; a
// refund returns one; time refills the bucket.
func TestGCRA(t *testing.T) {
	g := newGCRA(time.Second, 10)
	var tat time.Time
	for i := range 10 {
		if w := g.wait(tat, t0); w != 0 {
			t.Fatalf("token %d: wait %v", i+1, w)
		}
		g.take(&tat, t0)
	}
	if w := g.wait(tat, t0); w != time.Second {
		t.Fatalf("empty bucket: wait %v, want 1s", w)
	}
	g.take(&tat, t0) // reserved by a waiter
	if w := g.wait(tat, t0); w != 2*time.Second {
		t.Fatalf("after a reservation: wait %v, want 2s", w)
	}
	g.refund(&tat)
	if w := g.wait(tat, t0); w != time.Second {
		t.Fatalf("after a refund: wait %v, want 1s", w)
	}
	if w := g.wait(tat, t0.Add(time.Second)); w != 0 {
		t.Fatalf("after one interval: wait %v, want 0", w)
	}
	if w := g.wait(time.Time{}, t0); w != 0 {
		t.Fatalf("zero bucket: wait %v, want 0", w)
	}
}
