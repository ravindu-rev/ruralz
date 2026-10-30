// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"container/list"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Hot-entry layer bounds ("Response caching", rule 1; targets).
const (
	// HotBytes bounds the Node's hot-entry layer.
	HotBytes = 64 << 20
	// HotHold is how long the layer keeps a hot entry or generation token
	// after the State Store read that made it hot, and how close two
	// lookups must be for that read to make it hot.
	HotHold = time.Second
)

// hotOverhead is the memory charged per tracked key beyond its bytes: the
// key, the times and the list element.
const hotOverhead = 192

// hotKey identifies a generation key (gen, Key.URI only) or one variant
// of a partition.
type hotKey struct {
	key     statestore.CacheKey
	variant statestore.Digest
	gen     bool
}

// hotItem is one tracked key.
type hotItem struct {
	key hotKey
	// last is the key's latest lookup; until ends its hold, zero while not
	// held.
	last, until time.Time
	size        int64
	// token is a held generation key's token.
	token statestore.Token
	// names, entry and resident are a held variant's read: the partition's
	// Vary names, its entry (immutable, shared by every hit) and its
	// resident time at readAt, on the Node clock.
	names    string
	entry    []byte
	resident time.Duration
	readAt   time.Time
}

// hotEntry is a held variant as a lookup sees it.
type hotEntry struct {
	names    string
	entry    []byte
	resident time.Duration
}

// hotLayer is the Node's hot-entry layer ("Response caching", rule 1): it
// keeps, by LRU within HotBytes, the entries and generation tokens looked
// up twice within HotHold, for up to HotHold after the State Store read,
// so a hot entry or URI costs each Node about one read per HotHold. A key
// looked up within HotHold of its previous lookup is made hot by the read
// that follows, so a key that stays hot is read again as its hold ends.
// Only an existing generation key and a found variant are held. It is safe
// for concurrent use.
type hotLayer struct {
	mu   sync.Mutex
	max  int64
	hold time.Duration
	used int64
	ll   list.List
	m    map[hotKey]*list.Element
}

// newHotLayer returns an empty layer of at most maxBytes holding each key
// for hold.
func newHotLayer(maxBytes int64, hold time.Duration) *hotLayer {
	return &hotLayer{max: maxBytes, hold: hold, m: map[hotKey]*list.Element{}}
}

// token returns the held token of uri's generation key at now, and
// whether one is held; a held key records the lookup.
func (l *hotLayer) token(uri statestore.Digest, now time.Time) (statestore.Token, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	it := l.held(hotKey{key: statestore.CacheKey{URI: uri}, gen: true}, now)
	if it == nil {
		return statestore.Token{}, false
	}
	return it.token, true
}

// entry returns the held read of k's variant v at now, and whether one is
// held; a held key records the lookup.
func (l *hotLayer) entry(k statestore.CacheKey, v statestore.Digest, now time.Time) (hotEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	it := l.held(hotKey{key: k, variant: v}, now)
	if it == nil {
		return hotEntry{}, false
	}
	return hotEntry{names: it.names, entry: it.entry, resident: it.resident + now.Sub(it.readAt)}, true
}

// held returns the item of hk when it is held at now, recording the
// lookup; an expired hold is dropped. l.mu is held.
func (l *hotLayer) held(hk hotKey, now time.Time) *hotItem {
	e := l.m[hk]
	if e == nil {
		return nil
	}
	it := e.Value.(*hotItem)
	if it.until.IsZero() {
		return nil
	}
	if !now.Before(it.until) {
		l.unhold(it)
		return nil
	}
	it.last = now
	l.ll.MoveToFront(e)
	return it
}

// readToken records a State Store read of uri's generation key at now:
// tok when has, none otherwise.
func (l *hotLayer) readToken(uri statestore.Digest, tok statestore.Token, has bool, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	it, hot := l.read(hotKey{key: statestore.CacheKey{URI: uri}, gen: true}, now)
	if hot && has {
		it.token = tok
		it.until = now.Add(l.hold)
	}
}

// readEntry records a State Store read of k's variant v at now; a found
// entry is copied when it becomes hot.
func (l *hotLayer) readEntry(k statestore.CacheKey, v statestore.Digest, lk *statestore.CacheLookup, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	it, hot := l.read(hotKey{key: k, variant: v}, now)
	if !hot || !lk.Partition || !lk.Found {
		return
	}
	it.names, it.entry = lk.Names, append([]byte(nil), lk.Entry...)
	it.resident, it.readAt, it.until = lk.Resident, now, now.Add(l.hold)
	l.resize(it, hotOverhead+int64(len(it.names)+len(it.entry)))
}

// read records a read of hk at now and reports whether it makes hk hot:
// its previous lookup was within the hold. It drops any expired hold.
// l.mu is held.
func (l *hotLayer) read(hk hotKey, now time.Time) (*hotItem, bool) {
	if e := l.m[hk]; e != nil {
		it := e.Value.(*hotItem)
		if !it.until.IsZero() {
			l.unhold(it)
		}
		hot := now.Sub(it.last) < l.hold && !now.Before(it.last)
		it.last = now
		l.ll.MoveToFront(e)
		return it, hot
	}
	it := &hotItem{key: hk, last: now}
	l.m[hk] = l.ll.PushFront(it)
	l.resize(it, hotOverhead)
	return it, false
}

// unhold drops it's held value, keeping its last lookup. l.mu is held.
func (l *hotLayer) unhold(it *hotItem) {
	it.until, it.token = time.Time{}, statestore.Token{}
	it.names, it.entry = "", nil
	l.resize(it, hotOverhead)
}

// resize charges it size bytes and evicts the least recently used keys
// past the bound. l.mu is held.
func (l *hotLayer) resize(it *hotItem, size int64) {
	l.used += size - it.size
	it.size = size
	for l.used > l.max && l.ll.Len() > 0 {
		old := l.ll.Back()
		o := old.Value.(*hotItem)
		l.ll.Remove(old)
		delete(l.m, o.key)
		l.used -= o.size
	}
}

// dropToken forgets uri's generation key, after this Node bumped it.
func (l *hotLayer) dropToken(uri statestore.Digest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	hk := hotKey{key: statestore.CacheKey{URI: uri}, gen: true}
	if e := l.m[hk]; e != nil {
		l.ll.Remove(e)
		delete(l.m, hk)
		l.used -= e.Value.(*hotItem).size
	}
}

// bytes returns the charged memory.
func (l *hotLayer) bytes() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.used
}
