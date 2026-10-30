// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Key table targets (docs/architecture/09-traffic-management-and-resilience.md
// "Decision path", docs/architecture/11-scalability-and-distributed-state.md
// "Per-Node limits").
const (
	// DefaultEntries is the Node-wide entry count of buckets, over-limit
	// and local-only entries together, about 64 MiB (target).
	DefaultEntries = 1 << 20
	// DefaultLocalOnlyEntries is the local-only segment, part of
	// DefaultEntries (target), so a key-rotation flood never evicts a key
	// with a GCRA answer.
	DefaultLocalOnlyEntries = 131_072
	// DefaultShards is the number of short-mutex shards (target).
	DefaultShards = 256
	// LocalOnlyLife is how long a first-seen key past the budget decides
	// on its local-only entry (target).
	LocalOnlyLife = 10 * time.Second
	// FirstSeenPerCell is B, the Cell-wide first-seen call budget per
	// second, 20% of a shard (hypothesis).
	FirstSeenPerCell = 20_000
	// FirstSeenPerNode caps a Node's first-seen budget with a Node count,
	// per second (target).
	FirstSeenPerNode = 500
	// FirstSeenNoCount is a Node's first-seen budget per second without a
	// Node count, as in file mode (target).
	FirstSeenNoCount = 200
)

// TableName is the Component name of the key table.
const TableName = "ratelimit-key-table"

// Entry flags.
const (
	// flagRef is the CLOCK reference bit.
	flagRef uint8 = 1 << iota
	// flagFailOpen puts a bucket on the fail-open refill: until is the
	// epoch-aligned window it accounts and last the allowance released
	// into it (-1 in the window of the switch).
	flagFailOpen
	// flagPinned keeps an entry an operation holds from eviction.
	flagPinned
)

// Segments of the key table, the segment label of
// ruralz_ratelimit_bucket_evictions_total.
const (
	segMain      = emit.BucketSegmentMain
	segLocalOnly = emit.BucketSegmentLocalOnly
)

// entry is one key and limits[] entry: its local token bucket and, in the
// main segment, its over-limit cache; 64 bytes. Its fields by role:
//
//   - tokens: whole tokens the bucket holds;
//   - last: the refill time (Unix ns), or on the fail-open refill the
//     allowance released into the window it accounts;
//   - until: in the main segment the over-limit cache's end (Unix ns, 0
//     when none), or on the fail-open refill the window it accounts; in the
//     local-only segment the entry's end.
type entry struct {
	digest statestore.Digest
	limit  uint32
	flags  uint8
	_      [3]byte
	tokens int64
	last   int64
	until  int64
}

// segment is one segment of a shard: its entries, allocated at their full
// count so an entry never moves, how many have been handed out, a CLOCK
// hand, and the positions a removal freed.
type segment struct {
	entries []entry
	used    int
	hand    int
	free    []uint32
}

// shard is one lock's worth of the table: the main segment, then the
// local-only segment, and an open-addressing index over both whose slots
// hold 0 for empty, else 1 + the entry's position (main first).
type shard struct {
	mu    sync.Mutex
	seg   [2]segment
	index []uint32
	mask  uint64
	_     [64]byte // keeps neighboring shard locks off one cache line
}

// NodeCount reports the published Node count a derived per-Node ceiling
// and the first-seen budget read (pack 8.8). File mode, the only mode of
// M1, has none.
type NodeCount interface {
	// Count returns the published count and whether the Node runs in
	// Control mode; n is 0 for a Control-mode Node without a count.
	Count() (n int64, control bool)
}

// FileMode is the NodeCount of file mode: no count, not Control mode.
type FileMode struct{}

// Count implements NodeCount: no count.
func (FileMode) Count() (int64, bool) { return 0, false }

// TableOptions size a Table; only tests change them.
type TableOptions struct {
	// Entries is the entry count of both segments (DefaultEntries).
	Entries int
	// LocalOnlyEntries is the local-only segment (DefaultLocalOnlyEntries).
	LocalOnlyEntries int
	// Shards is the shard count (DefaultShards).
	Shards int
	// Seed keys the shard and index hashes; 0 draws one from crypto/rand,
	// so a client cannot choose keys that pile onto one index cluster.
	Seed uint64
}

// DefaultTableOptions returns the target sizes.
func DefaultTableOptions() TableOptions {
	return TableOptions{Entries: DefaultEntries, LocalOnlyEntries: DefaultLocalOnlyEntries, Shards: DefaultShards}
}

// Table is the Node-wide rate-limit key table: the local token buckets,
// over-limit cache and local-only entries of every ratelimit Policy in 256
// CLOCK-evicted shards, the first-seen budget all Policies share, and the
// limit registry. It outlives snapshots, so buckets survive Hot Reloads,
// and is safe for concurrent use. It is a filter.Component whose Run does
// no work.
type Table struct {
	clock clock.Clock
	count NodeCount
	seed  uint64

	shards []shard

	// budgetMu guards the first-seen budget bucket, which starts full.
	budgetMu     sync.Mutex
	budgetInit   bool
	budgetTokens int64
	budgetLast   int64

	// limMu guards lims and writes of ids; ids is read without it.
	limMu sync.Mutex
	lims  map[limitKey]*limitInfo
	ids   atomic.Pointer[[]*limitInfo]
	next  uint32
}

// limitKey names one limit: rz:rl:<policy>:<requests>/<window>. A changed
// requests or window is a new limit with fresh buckets.
type limitKey struct {
	policy   string
	requests int64
	window   time.Duration
}

// limitInfo is one registered limit: its id in entries, the Filters that
// hold it, and the eviction counters of its Policy's latest Filter.
type limitInfo struct {
	id        uint32
	key       limitKey
	refs      int
	evictions atomic.Pointer[[emit.NumBucketSegments]emit.Counter]
}

// ErrTableSize reports TableOptions that cannot hold one request's
// entries per segment and shard.
var ErrTableSize = errors.New("ratelimit: key table too small for one request's limits per shard")

// NewTable returns an empty key table timed by clk (nil means the real
// clock) and reading count (nil means FileMode).
func NewTable(clk clock.Clock, count NodeCount, o TableOptions) (*Table, error) {
	def := DefaultTableOptions()
	if o.Entries <= 0 {
		o.Entries = def.Entries
	}
	if o.LocalOnlyEntries <= 0 {
		o.LocalOnlyEntries = def.LocalOnlyEntries
	}
	if o.Shards <= 0 {
		o.Shards = def.Shards
	}
	main := (o.Entries - o.LocalOnlyEntries) / o.Shards
	local := o.LocalOnlyEntries / o.Shards
	// Each segment of a shard must hold one request's limits beside an
	// entry it can evict.
	if main <= MaxLimits || local <= MaxLimits || main+local >= 1<<31 {
		return nil, ErrTableSize
	}
	if clk == nil {
		clk = clock.Real()
	}
	if count == nil {
		count = FileMode{}
	}
	seed := o.Seed
	if seed == 0 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		seed = binary.LittleEndian.Uint64(b[:]) | 1
	}
	t := &Table{clock: clk, count: count, seed: seed, shards: make([]shard, o.Shards), lims: map[limitKey]*limitInfo{}}
	slots := 1
	for slots < 2*(main+local) {
		slots <<= 1
	}
	for i := range t.shards {
		sh := &t.shards[i]
		sh.seg[segMain].entries = make([]entry, main)
		sh.seg[segLocalOnly].entries = make([]entry, local)
		sh.index = make([]uint32, slots)
		sh.mask = uint64(slots - 1)
	}
	ids := []*limitInfo{nil} // id 0 marks an empty entry
	t.ids.Store(&ids)
	return t, nil
}

// Name implements filter.Component.
func (*Table) Name() string { return TableName }

// Run implements filter.Component: the table needs no goroutine, since
// CLOCK evicts on insert and local-only entries end when read.
func (*Table) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// register returns the id of limit k for a new Filter, taking a
// reference, and points its eviction counts at evictions.
func (t *Table) register(k limitKey, evictions *[emit.NumBucketSegments]emit.Counter) uint32 {
	t.limMu.Lock()
	defer t.limMu.Unlock()
	info := t.lims[k]
	if info == nil {
		t.next++
		info = &limitInfo{id: t.next, key: k}
		t.lims[k] = info
		old := *t.ids.Load()
		ids := make([]*limitInfo, len(old)+1)
		copy(ids, old)
		ids[info.id] = info
		t.ids.Store(&ids)
	}
	info.refs++
	if evictions != nil {
		info.evictions.Store(evictions)
	}
	return info.id
}

// release drops a Filter's reference to limit k; the last one forgets the
// limit, whose entries then age out unread. Ids are never reused.
func (t *Table) release(k limitKey) {
	t.limMu.Lock()
	defer t.limMu.Unlock()
	info := t.lims[k]
	if info == nil {
		return
	}
	if info.refs--; info.refs > 0 {
		return
	}
	delete(t.lims, k)
	old := *t.ids.Load()
	ids := make([]*limitInfo, len(old))
	copy(ids, old)
	ids[info.id] = nil
	t.ids.Store(&ids)
}

// limits returns the number of registered limits.
func (t *Table) limits() int {
	t.limMu.Lock()
	defer t.limMu.Unlock()
	return len(t.lims)
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

// shardOf returns the shard of a Policy's key: every limit of one Policy
// and key shares it, so one decision takes one lock.
func (t *Table) shardOf(policySeed uint64, d *statestore.Digest) *shard {
	h := mix(binary.LittleEndian.Uint64(d[8:16]) ^ policySeed ^ t.seed)
	return &t.shards[h%uint64(len(t.shards))]
}

// home returns the index slot a key hashes to.
func (t *Table) home(sh *shard, id uint32, d *statestore.Digest) uint64 {
	return mix(binary.LittleEndian.Uint64(d[0:8])^t.seed^uint64(id)*0x9e3779b97f4a7c15) & sh.mask
}

// ref is an index value: 1 + an entry's position, the main segment's
// positions first.
type ref uint32

// ref returns the index value of position pos of segment seg.
func (sh *shard) ref(seg, pos int) ref {
	if seg == segLocalOnly {
		pos += len(sh.seg[segMain].entries)
	}
	return ref(pos + 1) //nolint:gosec // G115: positions fit the table's 31 bits.
}

// at returns the segment and position an index value names.
func (sh *shard) at(v ref) (seg, pos int) {
	pos = int(v) - 1
	if m := len(sh.seg[segMain].entries); pos >= m {
		return segLocalOnly, pos - m
	}
	return segMain, pos
}

// entry returns the entry an index value names.
func (sh *shard) entry(v ref) *entry {
	seg, pos := sh.at(v)
	return &sh.seg[seg].entries[pos]
}

// find returns the index slot and value of a key; the value is 0 when the
// table holds no entry for it.
func (t *Table) find(sh *shard, id uint32, d *statestore.Digest) (uint64, ref) {
	for i := t.home(sh, id, d); ; i = (i + 1) & sh.mask {
		v := ref(sh.index[i])
		if v == 0 {
			return 0, 0
		}
		if e := sh.entry(v); e.limit == id && e.digest == *d {
			return i, v
		}
	}
}

// unlink removes index slot i, shifting the rest of its probe run back so
// no lookup stops early.
func (t *Table) unlink(sh *shard, i uint64) {
	for j := (i + 1) & sh.mask; ; j = (j + 1) & sh.mask {
		v := ref(sh.index[j])
		if v == 0 {
			break
		}
		e := sh.entry(v)
		k := t.home(sh, e.limit, &e.digest)
		// Move v back to i when its home is not cyclically within (i, j].
		if (j > i && (k <= i || k > j)) || (j < i && k <= i && k > j) {
			sh.index[i] = uint32(v)
			i = j
		}
	}
	sh.index[i] = 0
}

// remove drops the entry at index slot slot, value v, and frees its
// position.
func (t *Table) remove(sh *shard, slot uint64, v ref) {
	t.unlink(sh, slot)
	seg, pos := sh.at(v)
	sh.seg[seg].entries[pos] = entry{}
	sh.seg[seg].free = append(sh.seg[seg].free, uint32(pos)) //nolint:gosec // G115: positions fit the table's 31 bits.
}

// insert adds a new entry for a key to segment seg and returns its index
// value, evicting with CLOCK when the segment is full and counting the
// eviction for the evicted entry's Policy on stripe. Entries never move,
// so pointers to other entries stay valid.
func (t *Table) insert(sh *shard, seg int, id uint32, d *statestore.Digest, stripe emit.Stripe) ref {
	s := &sh.seg[seg]
	var pos int
	switch {
	case len(s.free) > 0:
		pos = int(s.free[len(s.free)-1])
		s.free = s.free[:len(s.free)-1]
	case s.used < len(s.entries):
		pos = s.used
		s.used++
	default:
		pos = t.evict(sh, seg, stripe)
	}
	s.entries[pos] = entry{digest: *d, limit: id}
	v := sh.ref(seg, pos)
	i := t.home(sh, id, d)
	for sh.index[i] != 0 {
		i = (i + 1) & sh.mask
	}
	sh.index[i] = uint32(v)
	return v
}

// evict frees one entry of a full segment by CLOCK: the hand clears the
// reference bit of each entry it passes and takes the first entry without
// one. An entry gets its bit when a request finds it again, not when it
// is inserted. A pinned entry, held by the operation in progress, is
// passed over; a segment always holds more entries than one operation
// pins.
func (t *Table) evict(sh *shard, seg int, stripe emit.Stripe) int {
	s := &sh.seg[seg]
	for {
		pos := s.hand
		s.hand = (s.hand + 1) % len(s.entries)
		e := &s.entries[pos]
		switch {
		case e.flags&flagPinned != 0:
			continue
		case e.flags&flagRef != 0:
			e.flags &^= flagRef
			continue
		}
		t.countEviction(e.limit, seg, stripe)
		if slot, v := t.find(sh, e.limit, &e.digest); v == sh.ref(seg, pos) {
			t.unlink(sh, slot)
		}
		*e = entry{}
		return pos
	}
}

// countEviction counts one evicted entry of limit id for its Policy.
func (t *Table) countEviction(id uint32, seg int, stripe emit.Stripe) {
	ids := *t.ids.Load()
	if int(id) >= len(ids) || ids[id] == nil {
		return
	}
	if c := ids[id].evictions.Load(); c != nil && c[seg] != nil {
		c[seg].Add(stripe, 1)
	}
}

// takeFirstSeen spends one unit of the Node's first-seen budget at now,
// all Policies sharing it: min(500, B / N_published) per second with a
// count, else 200 (target); false when it is spent.
func (t *Table) takeFirstSeen(now int64) bool {
	perWindow, window, capacity := t.firstSeenRate()
	t.budgetMu.Lock()
	defer t.budgetMu.Unlock()
	if !t.budgetInit {
		t.budgetInit, t.budgetTokens, t.budgetLast = true, capacity, now
	}
	refill(&t.budgetTokens, &t.budgetLast, now, perWindow, window, capacity)
	if t.budgetTokens < 1 {
		return false
	}
	t.budgetTokens--
	return true
}

// firstSeenRate returns the first-seen budget as perWindow units per
// window nanoseconds and its burst capacity: with a count N, min(500,
// B / N) per second, written min(500 N, B) per N seconds, its capacity the
// per-second rate rounded up; without one 200 per second.
func (t *Table) firstSeenRate() (perWindow, window, capacity int64) {
	n, _ := t.count.Count()
	if n <= 0 {
		return FirstSeenNoCount, int64(time.Second), FirstSeenNoCount
	}
	perWindow = min(FirstSeenPerNode*n, FirstSeenPerCell)
	window = n * int64(time.Second)
	capacity = max(1, (perWindow+n-1)/n)
	return perWindow, window, capacity
}
