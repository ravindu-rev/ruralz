// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"cmp"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// DefaultMaxDiagnostics is the cap of one call when Options.MaxDiagnostics
// is zero: the loader's Limits.MaxDiagnostics default (10,000 per run,
// proposed), as stage G's defaults.DefaultMaxDiagnostics.
const DefaultMaxDiagnostics = 10_000

// DefaultYieldAfter is the work a worker does before it yields the
// processor when Options.YieldAfter is zero (100 µs).
const DefaultYieldAfter = 100 * time.Microsecond

// yieldCheck is the number of work units between two reads of the yield
// timer inside a work unit (checked every 256 nodes).
const yieldCheck = 256

// byteUnit is the number of bytes of authored text one work unit stands
// for where stage I reads text whose length no schema rule bounds (a
// slot it interns), as stage E's subst.byteUnit.
const byteUnit = 64

// maxRefBytes is the length of the longest reference that can resolve:
// stage F accepts a metadata.name of at most 63 characters of [a-z0-9-]
// (the ObjectMeta.name maxLength and pattern), so 63 bytes, and stage I
// runs only after it. No schema rule bounds a reference itself
// (PolicyRef.name, RouteUpstream.name, CompositionStep.upstream,
// PolicySpec.plugin), so stage I treats a longer one as unresolved
// (RZ-CFG-009 is stage H's) before it hashes or compares it.
const maxRefBytes = 63

// worker is the state one goroutine of a call carries from one work unit
// to the next: the yield timer, the work counter it reads, and the
// findings it has built and not yet merged into the call's collector. A
// worker belongs to one goroutine at a time: forEach hands worker j to
// its goroutine j, and the calling goroutine uses worker 0 outside
// forEach.
//
// A work unit is one element that a loop of stage I reads: a resource, a
// list entry, an entry placed in, copied into or compared within a Phase,
// a removal the memo hashes, compares or records, a tree item the locator
// reads, a reference too long to resolve, a diagnostic offered or merged,
// or byteUnit bytes of a slot interned. Every loop whose length depends
// on the input counts its elements at its own site as it reads them, so
// no stretch of work between two counts makes more than a fixed number of
// comparisons, hashes and calls: stage I sorts with its own heap sort,
// which counts every comparison and does a fixed number of moves per
// comparison, and orders Phases by counting placements instead of
// sorting. Every map it reads or writes is keyed by an index, an
// interned slot id, a fixed-size phaseKey, a Policy type (an enum value
// stage F checked) or a name of at most maxRefBytes (a reference it
// resolved, or a resource name, which stage F bounds the same), so no
// hash or key comparison grows with the input but the one of each slot
// interned: stage I never hashes or compares a reference longer than
// maxRefBytes, and reads a slot's text once per attachment scope, charged
// per byteUnit before the read. A merge into the call's collector does at
// most batch insertions under the collector's lock and is charged when the
// lock is released, so no worker yields while it holds the lock. The time
// between two reads of the timer is then bounded, except for that one hash
// of a slot (no schema rule bounds a slot), the calls of Options.Body, and
// the memory the runtime allocates, clears or copies in one call (a buffer
// made for a Phase, a slice an append grows), which is proportional to
// units already counted; total and sites measure the stage's work
// independently of the machine (TestWorkCharged pins each site,
// TestNoBulkCharges the spacing of the reads).
type worker struct {
	// now is the clock of the yield timer (Options.Now); nil never yields.
	now func() time.Time
	// after is the work between two yields (Options.YieldAfter).
	after time.Duration
	// last is when the worker last yielded, or started.
	last time.Time
	// units counts the work units since the timer was last read.
	units int
	// total counts every work unit of the worker, for tests.
	total int
	// sites counts the work units of each site, for tests.
	sites [siteCount]int
	// yields counts the yields, for tests.
	yields int
	// out is the collector of the call; buf holds the findings the worker
	// offered since it last merged them into out, at most out.batch.
	out *collector
	buf []kept
	// rank is the rank of the next finding offered.
	rank rank
	// offered counts the findings offered (built), and held the most buf
	// held at once, for tests.
	offered, held int
}

// newWorker returns a worker of r that merges its findings into out and
// whose yield timer starts now.
func (r *Resolver) newWorker(out *collector) *worker {
	w := &worker{now: r.now, after: r.yieldAfter, out: out}
	if w.now != nil {
		w.last = w.now()
	}
	return w
}

// keepOf returns the number of diagnostics a call keeps for the cap
// maxDiagnostics: one more, so a collector of the same cap sees that it
// was reached, without overflowing int.
func keepOf(maxDiagnostics int) int {
	return min(maxDiagnostics, math.MaxInt-1) + 1
}

// site names one loop of stage I that charges work units.
type site uint8

// Sites, one per loop whose length depends on the input.
const (
	siteResource         site = iota // newState: a resource
	siteKey                          // addKeys: a list entry
	siteLongKey                      // addKeys: a reference too long to resolve
	siteSelected                     // selectAll: an attachKey's selection
	siteLongPlugin                   // selectOne: a spec.plugin too long to resolve
	siteSlotIntern                   // slotID: a slot interned, per byteUnit bytes
	siteListEntry                    // attachList: a list entry
	siteLongList                     // attachList: a reference too long to resolve
	siteLocate                       // locator: a tree item read
	siteLongLocate                   // listLoc: a tree name too long to index
	siteGatewayItem                  // attachGateway: a valid Gateway entry
	siteLegItem                      // attachUpstreams: a valid Upstream entry
	siteUpstream                     // attachUpstreams: an Upstream's leg
	siteFillCount                    // fill: an entry counted
	siteFillPlace                    // fill: an entry placed in a Phase
	siteShared                       // newSharedPhase: an onRequestBody entry scanned
	siteExclude                      // route: a spec.excludePolicies entry
	siteLongExclude                  // route: an exclusion too long to resolve
	siteKept                         // route: a valid Route entry
	siteRemoved                      // route: a removed Gateway entry
	siteRemovedSort                  // route: a comparison sorting removals
	siteRemovedList                  // route: a removal read building hub.Removed
	siteLeg                          // legs: a leg built
	siteKeptIn                       // client: a kept entry counted
	siteKeptOrder                    // client: a kept entry placed in class order
	siteRemovedPos                   // client: a removed position indexed
	siteRemovedIn                    // removedIn: a removal checked
	siteRemovalKey                   // phaseMemo.key: a removal hashed
	siteMemoCompare                  // phaseMemo.get: a removal compared or kept
	siteWithoutCount                 // without: an entry counted
	siteWithoutCopy                  // without: an entry copied
	siteCopyBase                     // client: a Gateway entry copied
	siteCopyKept                     // client: a kept entry copied
	siteGuardrailGateway             // cacheGuardrail: a Gateway cache entry
	siteGuardrailKept                // cacheGuardrail: a kept entry
	siteLegUpstream                  // legs: a spec.upstreams name
	siteLongLeg                      // legs: an Upstream name too long to resolve
	siteLegSort                      // legs: a comparison
	siteLegStep                      // legs: a composition step
	siteLegDedupe                    // legs: a name looked up and deduplicated
	siteChain                        // run: a chain indexed
	siteOffer                        // worker: a diagnostic offered
	siteFlush                        // flush: a diagnostic merged or a comparison merging it
	siteKeepSort                     // collector: a comparison sorting the kept ones
	sitePolicyCheck                  // checkPolicies: a resource
	siteCount
)

// charge counts k work units at site s and, each time they reach
// yieldCheck, reads the yield timer. A nil w does nothing.
func (w *worker) charge(s site, k int) {
	if w == nil || k <= 0 {
		return
	}
	w.sites[s] += k
	w.total += k
	if w.units += k; w.units >= yieldCheck {
		w.units = 0
		w.check()
	}
}

// check yields the processor once w.after has passed on w's clock since
// the last yield. forEach calls it between work units and charge every
// yieldCheck units. A nil w, or one without a clock, does nothing and
// reads no clock.
func (w *worker) check() {
	if w == nil || w.now == nil {
		return
	}
	if w.now().Sub(w.last) >= w.after {
		runtime.Gosched()
		w.yields++
		w.last = w.now()
	}
}

// resolvable reports whether the reference name can name a resource,
// reading only its length: one longer than maxRefBytes cannot, and a
// caller skips it as unresolved before any map operation or comparison,
// so the cost of a reference never depends on its length. The skip is
// one unit of w at s.
func (w *worker) resolvable(s site, name string) bool {
	if len(name) <= maxRefBytes {
		return true
	}
	w.charge(s, 1)
	return false
}

// rank orders findings that diag.List.Sort leaves equal (same location,
// code, path and message, such as findings without a position) as a
// stable sort of the stage's whole report leaves them: CheckPolicies
// first, then the Gateway list, the Upstream lists and the Routes, each
// in Bundle order, and within a unit in the order the unit reports them.
// So the kept set never depends on which worker saw which finding.
// Findings of two groups never tie today (RZ-CFG-018 and RZ-CFG-020 name
// the scope in their message, and every other code has one group); the
// group keeps the order defined should that change.
type rank struct {
	group, unit, n int
}

// Rank groups, in report order.
const (
	groupPolicies = iota
	groupGateway
	groupUpstreams
	groupRoutes
)

// compareRank orders ranks by group, unit, then report order.
func compareRank(a, b rank) int {
	if c := cmp.Compare(a.group, b.group); c != 0 {
		return c
	}
	if c := cmp.Compare(a.unit, b.unit); c != 0 {
		return c
	}
	return cmp.Compare(a.n, b.n)
}

// kept is one diagnostic a collector holds, with its path text rendered
// once for the comparisons.
type kept struct {
	d    diag.Diagnostic
	path string
	rank rank
}

// compareKept is the order of diag.List.Sort, by (environment, file,
// line, column, code, path, message), then rank: a total order, so every
// sort of kept values has one result. TestCompareKeptMatchesSort keeps it
// equal to diag.List.Sort's.
func compareKept(a, b *kept) int {
	if c := cmp.Compare(a.d.Environment, b.d.Environment); c != 0 {
		return c
	}
	if c := compareHead(a.d.Location, a.d.Code, b.d.Location, b.d.Code); c != 0 {
		return c
	}
	if c := cmp.Compare(a.path, b.path); c != 0 {
		return c
	}
	if c := cmp.Compare(a.d.Message, b.d.Message); c != 0 {
		return c
	}
	return compareRank(a.rank, b.rank)
}

// compareHead compares the file, line, column and code of two findings,
// the keys of diag.List.Sort after the environment, which stage I never
// sets.
func compareHead(at diag.Location, code string, bAt diag.Location, bCode string) int {
	if c := cmp.Compare(at.File, bAt.File); c != 0 {
		return c
	}
	if c := cmp.Compare(at.Line, bAt.Line); c != 0 {
		return c
	}
	if c := cmp.Compare(at.Column, bAt.Column); c != 0 {
		return c
	}
	return cmp.Compare(code, bCode)
}

// head is the location and code of a finding, the leading keys admits
// compares.
type head struct {
	at   diag.Location
	code string
}

// maxBatch bounds the findings a worker buffers between two merges into
// the collector once the collector is full, so a merge holds the lock for
// a bounded time.
const maxBatch = 64

// collector keeps the first keep findings of one call in compareKept
// order, from all of its workers (a call reports at most its cap plus
// one). Its keeper holds at most keep at any time. A worker merges the
// findings it offers into it at once while it is not full, then in
// batches of at most batch (fewer than maxBatch, and keep/workers when
// that is smaller) and at the end of each work unit; once it is full it
// publishes the location and code of its last kept finding, so every
// worker rejects a later finding that sorts after it before building it.
// The findings a call holds at once are therefore at most keep in the
// collector and batch per worker: fewer than 2·keep+workers, whatever the
// input and the worker count. It is safe for concurrent use.
type collector struct {
	// mu guards heap.
	mu   sync.Mutex
	heap keeper
	// root is the head of heap's root once heap is full, nil before; it
	// only ever moves down, so a stale value admits more, never less.
	root atomic.Pointer[head]
	// batch is the most findings a worker buffers once heap is full.
	batch int
}

// newCollector returns the collector of a call that keeps keep findings
// on at most workers workers.
func newCollector(keep, workers int) *collector {
	return &collector{heap: keeper{keep: keep}, batch: max(1, min(maxBatch, keep/max(1, workers)))}
}

// begin starts work unit unit of group: the findings w offers from now
// on rank after those of every earlier unit.
func (w *worker) begin(group, unit int) { w.rank = rank{group: group, unit: unit} }

// admits reports whether a finding with location at and code may be kept,
// before the caller builds its path and message: false only when the
// collector is full and the finding sorts after its last kept one on
// these leading keys alone. The root of a full collector only ever moves
// down, so a later finding equal to a rejected one is rejected too, and
// a skipped finding never changes the order of the others: ranks only
// order findings a unit offered, and a rejected one is never kept.
func (w *worker) admits(at diag.Location, code string) bool {
	r := w.out.root.Load()
	return r == nil || compareHead(at, code, r.at, r.code) <= 0
}

// offer buffers d, a finding admits accepted, charging one unit, and
// merges the buffer into the collector at once while the collector is
// not full, else when it holds batch findings.
func (w *worker) offer(d diag.Diagnostic) {
	w.charge(siteOffer, 1)
	w.offered++
	w.buf = append(w.buf, kept{d: d, path: d.Path.String(), rank: w.rank})
	w.rank.n++
	w.held = max(w.held, len(w.buf))
	if len(w.buf) >= w.out.batch || w.out.root.Load() == nil {
		w.flush()
	}
}

// flush merges the buffered findings into the collector under its lock
// and empties the buffer. The work it does under the lock (at most batch
// insertions, each of at most two comparisons per heap level) is charged
// after the lock is released, so no worker yields while it holds it.
func (w *worker) flush() {
	if len(w.buf) == 0 {
		return
	}
	c := w.out
	n := 0
	c.mu.Lock()
	for i := range w.buf {
		n += 1 + c.heap.add(&w.buf[i])
	}
	if len(c.heap.h) == c.heap.keep {
		root := &c.heap.h[0].d
		if old := c.root.Load(); old == nil || old.at != root.Location || old.code != root.Code {
			c.root.Store(&head{at: root.Location, code: root.Code})
		}
	}
	c.mu.Unlock()
	clear(w.buf)
	w.buf = w.buf[:0]
	w.charge(siteFlush, n)
}

// sorted returns the kept findings in compareKept order and empties the
// collector; each comparison is a unit of w. Every worker must have
// flushed and returned.
func (c *collector) sorted(w *worker) []kept {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.heap.h
	c.heap.h = nil
	heapSort(w, siteKeepSort, out, compareKept)
	return out
}

// keeper is the heap of a collector: the keep first, in compareKept
// order, of the findings added to it. It never holds more than keep at
// once: once full it is a max-heap whose root is the last one kept, which
// a smaller one replaces.
type keeper struct {
	// keep is the cap of the call plus one.
	keep int
	// h holds the kept findings, a max-heap by compareKept.
	h []kept
	// peak is the most held at once, for tests.
	peak int
}

// add keeps *c when it is among the keep first so far and returns the
// comparisons it made.
func (k *keeper) add(c *kept) int {
	if len(k.h) < k.keep {
		k.h = append(k.h, *c)
		k.peak = max(k.peak, len(k.h))
		return k.up(len(k.h) - 1)
	}
	if compareKept(c, &k.h[0]) >= 0 {
		return 1
	}
	k.h[0] = *c
	return 1 + siftDown(nil, 0, k.h, 0, len(k.h), compareKept)
}

// up restores the heap from i towards the root and returns the
// comparisons it made.
func (k *keeper) up(i int) int {
	n := 0
	for i > 0 {
		parent := (i - 1) / 2
		n++
		if compareKept(&k.h[i], &k.h[parent]) <= 0 {
			return n
		}
		k.h[i], k.h[parent] = k.h[parent], k.h[i]
		i = parent
	}
	return n
}

// heapSort sorts l into increasing order by compare, in place, charging
// w one unit at s for each comparison as it makes it: a heap sort makes
// at most two comparisons per element it moves down one level and one
// swap per such step, so no stretch of it between two charges does more
// than a fixed amount of work, unlike the library sorts, whose
// rotations and reversals move many elements between two comparisons. It
// is not stable; every caller sorts values of a total order or equal
// values that are identical.
func heapSort[T any](w *worker, s site, l []T, compare func(a, b *T) int) {
	n := len(l)
	for i := n/2 - 1; i >= 0; i-- {
		siftDown(w, s, l, i, n, compare)
	}
	for end := n - 1; end > 0; end-- {
		l[0], l[end] = l[end], l[0]
		siftDown(w, s, l, 0, end, compare)
	}
}

// siftDown restores the max-heap l[:n] from i towards the leaves and
// returns the comparisons it made, each charged to w at s (a nil w
// charges nothing).
func siftDown[T any](w *worker, s site, l []T, i, n int, compare func(a, b *T) int) int {
	made := 0
	for {
		big, left := i, 2*i+1
		if left >= n {
			return made
		}
		made++
		w.charge(s, 1)
		if compare(&l[left], &l[big]) > 0 {
			big = left
		}
		if right := left + 1; right < n {
			made++
			w.charge(s, 1)
			if compare(&l[right], &l[big]) > 0 {
				big = right
			}
		}
		if big == i {
			return made
		}
		l[i], l[big] = l[big], l[i]
		i = big
	}
}

// diagnostics returns the diagnostics of ks, or nil when there is none.
func diagnostics(ks []kept) diag.List {
	if len(ks) == 0 {
		return nil
	}
	out := make(diag.List, len(ks))
	for i := range ks {
		out[i] = ks[i].d
	}
	return out
}
