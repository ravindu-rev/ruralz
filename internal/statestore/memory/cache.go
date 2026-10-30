// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// Response Cache targets, in milliseconds.
const (
	fillLeaseMs       = 1000       // store fill lease, 1 s
	revalidateLeaseMs = 5000       // revalidation lease, 5 s
	generationTTLMs   = 93_600_000 // generation keys, 26 h after a creation, bump or store
	maxPartitionTTLMs = 90_000_000 // partitions, at most 25 h
)

// Generation step modes, as the library's n script takes them.
const (
	// genCreate creates the key holding the token (SET NX) and fails when
	// a key exists.
	genCreate uint8 = iota + 1
	// genHold checks that the key holds the token and refreshes its 26 h
	// TTL: a store, refresh or replacement under the token.
	genHold
	// genCheck only checks that the key holds the token: an end-stale.
	genCheck
)

// ceilMs returns d in milliseconds, rounded up.
func ceilMs(d time.Duration) int64 {
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

// entryTTL is a store's TTL in milliseconds within [1 ms, 25 h]: no key
// is ever written without a TTL.
func entryTTL(d time.Duration) int64 { return min(max(ceilMs(d), 1), maxPartitionTTLMs) }

// delayMs returns the receipt delay of a write whose response arrived at
// received on the Node clock, in whole milliseconds: the store time t:<V>
// is the server time less it.
func (d *Driver) delayMs(received time.Time) int64 {
	return int64(statestore.ReceiptDelay(received, d.node.Now()) / time.Millisecond)
}

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

// lookup is the pipelined lookup: GET rz:rc:{U}:gen (unless SkipToken),
// HMGET rz:rc:{U:P} n t:<V> e:<V> and TIME on the partition's shard, the
// injected server clock. The entry is copied into c.Entry's storage and
// checked against its MAC; a missing or wrong tag is a miss, never an
// error.
func (d *Driver) lookup(ctx context.Context, c *statestore.CacheLookup, now instant) {
	k := c.Key
	if !c.SkipToken {
		c.Token, c.HasToken = statestore.Token{}, false
		gs := d.shardOf(int(keys.DigestSlot(k.URI)))
		gs.mu.Lock()
		if e, held := gs.gen[k.URI]; held && !expired(e.exp, now.ms) {
			c.Token, c.HasToken = e.token, true
		}
		gs.mu.Unlock()
	}

	ps := d.shardOf(int(keys.PartitionSlot(k)))
	buf := c.Entry[:0]
	names, part, found, stored := "", false, false, int64(0)
	ps.mu.Lock()
	if p := d.livePartition(ps, k, now.ms); p != nil {
		names, part = p.names, true
		if i := p.find(c.Variant); i >= 0 {
			v := &p.variants[i]
			stored, found = v.t, true
			buf = append(buf, v.entry...)
		}
	}
	ps.mu.Unlock()

	c.Partition, c.Names, c.Found, c.Entry, c.Resident = part, names, false, buf[:0], 0
	if !found {
		return
	}
	c.Resident = time.Duration(max(now.ms-stored, 0)) * time.Millisecond
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

// generation runs the generation step (the library's n script) on uri's
// generation key: genCreate creates it holding token unless a key exists,
// genHold applies while it holds token and refreshes its 26 h TTL, and
// genCheck applies while it holds token. applied is false when the key
// does not qualify; ok is false for a full shard.
func (d *Driver) generation(uri statestore.Digest, token statestore.Token, mode uint8, now instant) (applied, ok bool) {
	s := d.shardOf(int(keys.DigestSlot(uri)))
	s.mu.Lock()
	defer s.mu.Unlock()
	e, held := s.gen[uri]
	live := held && !expired(e.exp, now.ms)
	switch mode {
	case genCreate:
		if live {
			return false, true
		}
		if !d.room(s, 1, now.ms) {
			return false, false
		}
	case genHold, genCheck:
		if !live || e.token != token {
			return false, true
		}
		if mode == genCheck {
			return true, true
		}
	default:
		return false, false
	}
	s.gen[uri] = genEntry{token: token, exp: now.ms + generationTTLMs}
	s.track(now.ms + generationTTLMs)
	return true, true
}

// store runs a store's two steps. The generation step creates the URI's
// generation key holding w.Generation (w.Create) or checks that it still
// holds it, refreshing its 26 h TTL; only then the partition step takes
// the 1 s fill lease or writes nothing, drops the partition when its names
// differ, writes names, the variant's store time (the server time less the
// write's receipt delay) and entry, keeps at most 8 variants in store
// order, dropping the oldest, and extends the partition's TTL to the
// larger of its remaining TTL and the entry TTL, capped at 25 h. applied is
// false when a step did not apply; ok is false for a full store.
func (d *Driver) store(w *statestore.CacheStore, now instant) (applied, ok bool) {
	mode := genHold
	if w.Create {
		mode = genCreate
	}
	if applied, ok := d.generation(w.Key.URI, w.Generation, mode, now); !applied || !ok {
		return false, ok
	}
	k := w.Key
	stored := d.sealEntry(k, w.Variant, w.Entry)
	at := now.ms - d.delayMs(w.Received)
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
	p.variants = append(p.variants, variant{v: w.Variant, t: at, entry: stored})
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

// lease runs a revalidation lease operation: take the 5 s lease (SET NX
// PX 5000; w.TTL does not change it, as the take sends none), rewrite a
// variant's entry with a freshened or replacing one (script f, which
// extends the partition by w.TTL like a store and records the store time
// less the receipt delay), or end the stale window by deleting the variant
// (script x). Refresh and end-stale first run the generation step, which
// applies only while the URI's generation key holds w.Generation (a
// refresh also renews its 26 h TTL), and then act only while the lease
// holds the caller's token. ok is false for an unknown mode or a full
// store.
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
		if applied, ok := d.generation(k.URI, w.Generation, genHold, now); !applied || !ok {
			return false, ok
		}
		stored := d.sealEntry(k, w.Variant, w.Entry)
		at := now.ms - d.delayMs(w.Received)
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
		v.entry, v.t = stored, at
		p.bytes += delta
		if w.TTL > 0 {
			p.exp = now.ms + min(max(p.exp-now.ms, entryTTL(w.TTL)), maxPartitionTTLMs)
			s.track(p.exp)
		}
		return true, true
	case statestore.LeaseEndStale:
		if applied, ok := d.generation(k.URI, w.Generation, genCheck, now); !applied || !ok {
			return false, ok
		}
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

// invalidate runs sub-operation i: the URI's generation key takes the
// fresh token the Node passes, for 26 h, so no entry written under an
// earlier token hits again. Generation bumps bypass the memory rules of
// AdmitStoreBytes.
func (d *Driver) invalidate(uri statestore.Digest, token statestore.Token, now instant) (applied, ok bool) {
	s := d.shardOf(int(keys.DigestSlot(uri)))
	s.mu.Lock()
	defer s.mu.Unlock()
	e, held := s.gen[uri]
	if live := held && !expired(e.exp, now.ms); !live && !d.room(s, 1, now.ms) {
		return false, false
	}
	s.gen[uri] = genEntry{token: token, exp: now.ms + generationTTLMs}
	s.track(now.ms + generationTTLMs)
	return true, true
}
