// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// Response Cache targets of spec 08 req 45, in milliseconds.
const (
	fillLeaseMs       = 1000       // store fill lease, 1 s
	revalidateLeaseMs = 5000       // revalidation lease, 5 s
	generationTTLMs   = 93_600_000 // generation keys, 26 h after the last bump
	maxPartitionTTLMs = 90_000_000 // partitions, at most 25 h
)

// ceilMs returns d in milliseconds, rounded up.
func ceilMs(d time.Duration) int64 {
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

// entryTTL is a store's TTL in milliseconds within [1 ms, 25 h]: no key
// is ever written without a TTL (spec 08 req 50).
func entryTTL(d time.Duration) int64 { return min(max(ceilMs(d), 1), maxPartitionTTLMs) }

// reserveBytes takes n cache entry bytes from the 64 MiB bound; false
// means the store is full (a noeviction refusal, RZ-STS-002).
func (d *Driver) reserveBytes(n int64) bool {
	if n <= 0 {
		d.bytes.Add(n)
		return true
	}
	for {
		cur := d.bytes.Load()
		if cur+n > d.maxBytes {
			return false
		}
		if d.bytes.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

// livePartition returns k's partition, reclaiming it when expired. The
// caller holds s.mu.
func (d *Driver) livePartition(s *shard, k statestore.CacheKey, nowMs int64) *partition {
	p := s.part[k]
	if p != nil && expired(p.exp, nowMs) {
		delete(s.part, k)
		d.bytes.Add(-p.bytes)
		return nil
	}
	return p
}

// sealEntry returns the stored form of entry under k's partition and
// variant field e:<V>: a copy of entry, tagged when a MAC key is set.
func (d *Driver) sealEntry(k statestore.CacheKey, v statestore.Digest, entry []byte) []byte {
	if d.mac == nil {
		return append(make([]byte, 0, len(entry)), entry...)
	}
	var kb [keys.CachePartitionLen]byte
	var fb [keys.FieldLen]byte
	dst := make([]byte, 0, len(entry)+keys.TagSize)
	return d.mac.Seal(dst, keys.AppendCachePartition(kb[:0], k), keys.AppendField(fb[:0], keys.FieldEntry, v), entry)
}

// lookup is the pipelined lookup (spec 08 req 45): GET rz:rc:{U}:gen and
// HMGET rz:rc:{U:P} n g:<V> e:<V>. The entry is copied into c.Entry's
// storage and checked against its MAC; a missing or wrong tag is a miss,
// never an error (spec 08 req 68).
func (d *Driver) lookup(ctx context.Context, c *statestore.CacheLookup, now instant) {
	k := c.Key
	gs := d.shardOf(int(keys.DigestSlot(k.URI)))
	var gen int64
	gs.mu.Lock()
	if e, held := gs.gen[k.URI]; held && !expired(e.exp, now.ms) {
		gen = e.n
	}
	gs.mu.Unlock()

	ps := d.shardOf(int(keys.PartitionSlot(k)))
	buf := c.Entry[:0]
	names, found, entryGen := "", false, int64(0)
	ps.mu.Lock()
	if p := d.livePartition(ps, k, now.ms); p != nil {
		names = p.names
		if i := p.find(c.Variant); i >= 0 {
			v := &p.variants[i]
			entryGen, found = v.gen, true
			buf = append(buf, v.entry...)
		}
	}
	ps.mu.Unlock()

	c.Generation, c.Names, c.EntryGen, c.Found, c.Entry = gen, names, entryGen, false, buf[:0]
	if !found {
		return
	}
	entry := buf
	if d.mac != nil {
		var ok bool
		if entry, ok = d.openEntry(k, c.Variant, buf); !ok {
			if d.macLog.allow(d.node.Now()) {
				d.logger.WarnContext(ctx, "state store cache entry failed its MAC check; served as a miss")
			}
			return
		}
	}
	c.Found, c.Entry = true, entry
}

// openEntry checks a stored entry's MAC under k's partition key and the
// variant's e:<V> field.
func (d *Driver) openEntry(k statestore.CacheKey, v statestore.Digest, stored []byte) ([]byte, bool) {
	var kb [keys.CachePartitionLen]byte
	var fb [keys.FieldLen]byte
	return d.mac.Open(keys.AppendCachePartition(kb[:0], k), keys.AppendField(fb[:0], keys.FieldEntry, v), stored)
}

// store runs sub-operation s (spec 08 req 45): take the 1 s fill lease or
// write nothing; drop the partition when its names differ; write names,
// the variant's generation, store time and entry; keep at most 8 variants
// in store order, dropping the oldest; extend the partition's TTL to the
// larger of its remaining TTL and the entry TTL, capped at 25 h. applied
// is false when the lease was held; ok is false for a full store.
func (d *Driver) store(w *statestore.CacheStore, now instant) (applied, ok bool) {
	k := w.Key
	stored := d.sealEntry(k, w.Variant, w.Entry)
	s := d.shardOf(int(keys.PartitionSlot(k)))
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, held := s.lease[k]; held && !expired(l.exp, now.ms) {
		return false, true
	}
	p := d.livePartition(s, k, now.ms)
	keep := p != nil && p.names == w.Names
	// Size the write before changing anything, so a refusal writes nothing.
	var freed int64
	switch {
	case p != nil && !keep:
		freed = p.bytes
	case keep:
		if i := p.find(w.Variant); i >= 0 {
			freed = int64(len(p.variants[i].entry))
		} else if len(p.variants) >= maxVariants {
			freed = int64(len(p.variants[0].entry))
		}
	}
	// The lease is new or expired: one key, plus the partition when new.
	fresh := 1
	if p == nil {
		fresh++
	}
	delta := int64(len(stored)) - freed
	if delta > 0 && !d.reserveBytes(delta) {
		return false, false
	}
	if !d.room(s, fresh, now.ms) {
		d.bytes.Add(-max(delta, 0))
		return false, false
	}
	if delta <= 0 {
		d.bytes.Add(delta)
	}
	s.lease[k] = leaseEntry{token: w.Token, exp: now.ms + fillLeaseMs}
	s.track(now.ms + fillLeaseMs)
	ttl := entryTTL(w.TTL)
	if !keep {
		// A partition stored under other names is deleted first.
		if p != nil {
			delete(s.part, k)
		}
		p = &partition{names: w.Names}
		s.part[k] = p
	} else {
		ttl = min(max(p.exp-now.ms, ttl), maxPartitionTTLMs)
	}
	if i := p.find(w.Variant); i >= 0 {
		p.remove(i)
	}
	p.variants = append(p.variants, variant{v: w.Variant, gen: w.Generation, t: now.ms, entry: stored})
	p.bytes += int64(len(stored))
	for len(p.variants) > maxVariants {
		p.remove(0)
	}
	p.exp = now.ms + ttl
	s.track(p.exp)
	return true, true
}

// holds reports whether k's lease is live and holds token.
func holds(s *shard, k statestore.CacheKey, token uint64, nowMs int64) bool {
	l, held := s.lease[k]
	return held && !expired(l.exp, nowMs) && l.token == token
}

// lease runs a revalidation lease operation (spec 08 req 45): take the
// 5 s lease (SET NX PX 5000; w.TTL does not change it, as the script
// takes none), refresh a variant's entry after a 304 (script f, which
// extends the partition by w.TTL like a store), or end the stale window
// by deleting the variant (script x); refresh and end-stale act only
// while the lease holds the caller's token. ok is false for an unknown
// mode or a full store.
func (d *Driver) lease(w *statestore.CacheLease, now instant) (applied, ok bool) {
	k := w.Key
	s := d.shardOf(int(keys.PartitionSlot(k)))
	switch w.Mode {
	case statestore.LeaseTake:
		s.mu.Lock()
		defer s.mu.Unlock()
		if l, held := s.lease[k]; held && !expired(l.exp, now.ms) {
			return false, true
		}
		if !d.room(s, 1, now.ms) {
			return false, false
		}
		s.lease[k] = leaseEntry{token: w.Token, exp: now.ms + revalidateLeaseMs}
		s.track(now.ms + revalidateLeaseMs)
		return true, true
	case statestore.LeaseRefresh:
		stored := d.sealEntry(k, w.Variant, w.Entry)
		s.mu.Lock()
		defer s.mu.Unlock()
		if !holds(s, k, w.Token, now.ms) {
			return false, true
		}
		p := d.livePartition(s, k, now.ms)
		if p == nil {
			return false, true
		}
		i := p.find(w.Variant)
		if i < 0 {
			return false, true
		}
		v := &p.variants[i]
		delta := int64(len(stored) - len(v.entry))
		if !d.reserveBytes(delta) {
			return false, false
		}
		v.entry, v.t = stored, now.ms
		p.bytes += delta
		if w.TTL > 0 {
			p.exp = now.ms + min(max(p.exp-now.ms, entryTTL(w.TTL)), maxPartitionTTLMs)
			s.track(p.exp)
		}
		return true, true
	case statestore.LeaseEndStale:
		s.mu.Lock()
		defer s.mu.Unlock()
		if !holds(s, k, w.Token, now.ms) {
			return false, true
		}
		if p := d.livePartition(s, k, now.ms); p != nil {
			if i := p.find(w.Variant); i >= 0 {
				d.bytes.Add(-p.remove(i))
			}
		}
		return true, true
	default:
		return false, false
	}
}

// invalidate runs sub-operation i (spec 08 req 45): the URI's generation
// becomes the server time in microseconds since the Unix epoch, or the
// stored value plus one when that is not greater, so a generation is
// never reused; the key expires 26 h after the bump. Generation bumps
// bypass the memory rules of AdmitStoreBytes.
func (d *Driver) invalidate(uri statestore.Digest, now instant) (applied, ok bool) {
	s := d.shardOf(int(keys.DigestSlot(uri)))
	s.mu.Lock()
	defer s.mu.Unlock()
	e, held := s.gen[uri]
	live := held && !expired(e.exp, now.ms)
	var cur int64
	if live {
		cur = e.n
	}
	g := max(now.unixUs, cur+1)
	if !live && !d.room(s, 1, now.ms) {
		return false, false
	}
	s.gen[uri] = countEntry{n: g, exp: now.ms + generationTTLMs}
	s.track(now.ms + generationTTLMs)
	return true, true
}
