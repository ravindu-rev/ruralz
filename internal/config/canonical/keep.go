// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"cmp"
	"errors"
	"math"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// DefaultMaxDiagnostics is the findings cap of one Decode, Verify or
// ReadDump when DecoderOptions leaves it zero: the per-run cap (10,000,
// proposed), which every other pipeline stage applies too.
const DefaultMaxDiagnostics = 10_000

// CodeTooMany is the code of the last diagnostic of a decode that met more
// findings than its cap: "too many diagnostics; stopped after N", as
// diag.Collector writes it. It is CodeLimit.
const CodeTooMany = CodeLimit

// Sentinels the parser stops with: the newer findings reached the cap, or
// a format member names another format.
var (
	errStop  = errors.New("canonical: the findings reached the cap")
	errOther = errors.New("canonical: the content names another format")
)

// order is the content order of a finding: the ordinal of its top-level
// member (-1 for a finding about the document itself), the index of its
// resource in the resources array and its index among the findings of
// that resource or member. Findings of one resource are collected in
// content order, so the order of two findings is the order of their
// positions in the content, without reading a position.
type order struct {
	member, res, seq int
}

func (a order) compare(b order) int {
	return cmp.Or(cmp.Compare(a.member, b.member), cmp.Compare(a.res, b.res), cmp.Compare(a.seq, b.seq))
}

// kept is one finding a keeper holds.
type kept struct {
	o order
	d diag.Diagnostic
}

// keeper keeps the first n findings of one code of a decode in content
// order, out of the findings the parser and every checker offer in any
// order. It holds fewer than 2n: once n were offered it sorts them, and
// from then on it drops at once a finding that comes after the last one
// kept, and sorts and cuts back to n whenever it reaches 2n. The kept set
// is therefore the same whatever order the findings arrive in, so it does
// not depend on scheduling, and f offers cost at most f/n+2 sorts of at
// most 2n entries, whatever n is: the test for 2n never multiplies, so no
// n overflows it. It is safe for concurrent use.
type keeper struct {
	mu       sync.Mutex
	n        int
	entries  []kept
	bound    order
	hasBound bool
	// prunes counts the sorts, which the tests of the keeper's cost read.
	prunes int
	// resources is the ordinal of the top-level resources member, set by
	// the parser before any resource finding is offered; -1 before.
	resources int
	// some is set by the first finding offered and full once n were, when
	// every finding still to come from the parser comes after the n kept.
	some, full atomic.Bool
	// last is the index of the last resource that can still hold a kept
	// finding: math.MaxInt64 until the bound falls in the resources array.
	// (A bound before the array stops the parser before any resource is
	// read.)
	last atomic.Int64
}

func newKeeper(n int) *keeper {
	k := &keeper{n: max(1, n), resources: -1}
	k.last.Store(math.MaxInt64)
	return k
}

// setResources records the ordinal of the resources member.
func (k *keeper) setResources(member int) {
	k.mu.Lock()
	k.resources = member
	k.mu.Unlock()
}

// any reports whether a finding was offered.
func (k *keeper) any() bool { return k.some.Load() }

// after reports whether every finding of resource i comes after the last
// finding kept, so none of them can be kept.
func (k *keeper) after(i int) bool { return int64(i) > k.last.Load() }

// add offers one finding; y is the offering goroutine's yield timer,
// charged for the work of the offer, as in addResource and list.
func (k *keeper) add(o order, d diag.Diagnostic, y *yielder) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.offer(o, d, y)
}

// addResource offers the findings of resource i, in content order.
func (k *keeper) addResource(i int, ds diag.List, y *yielder) {
	if len(ds) == 0 {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for j := range ds {
		k.offer(order{member: k.resources, res: i, seq: j}, ds[j], y)
	}
}

// offer records d at o; k.mu is held.
func (k *keeper) offer(o order, d diag.Diagnostic, y *yielder) {
	y.charge(siteOffer, 1)
	k.some.Store(true)
	if k.hasBound && o.compare(k.bound) >= 0 {
		return
	}
	k.entries = append(k.entries, kept{o: o, d: d})
	if (!k.hasBound && len(k.entries) >= k.n) || len(k.entries)-k.n >= k.n {
		k.prune(y)
	}
}

// prune sorts the entries and keeps the first n; k.mu is held. A yield
// while it is held only delays the other goroutines of the decode.
func (k *keeper) prune(y *yielder) {
	k.prunes++
	slices.SortFunc(k.entries, func(a, b kept) int {
		y.charge(siteKeepSort, 1)
		return a.o.compare(b.o)
	})
	if len(k.entries) < k.n {
		return
	}
	clear(k.entries[k.n:])
	k.entries = k.entries[:k.n]
	k.bound, k.hasBound = k.entries[k.n-1].o, true
	if k.resources >= 0 && k.bound.member == k.resources {
		k.last.Store(int64(k.bound.res))
	}
	k.full.Store(true)
}

// list returns the kept findings in content order: all of them when
// fewer than n were offered, else the first n.
func (k *keeper) list(y *yielder) diag.List {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.prune(y)
	if len(k.entries) == 0 {
		return nil
	}
	out := make(diag.List, len(k.entries))
	for i := range k.entries {
		y.charge(siteList, 1)
		out[i] = k.entries[i].d
	}
	return out
}

// run is the state one decode shares between its parser and its
// checkers: the newer and the form findings kept so far, each code with
// its own budget so a newer producer still wins over a form problem, the
// first resource whose re-encoding failed, and the finding of a format
// member that names another format.
type run struct {
	newer, form *keeper
	// rejected is the index of the first resource whose re-encoding
	// differs from the content or fails, math.MaxInt64 while there is
	// none. The resources after it can no longer change the outcome:
	// sequence reports the first failure by offset, which lies at or
	// before that resource's own.
	rejected atomic.Int64
	// other is written by the parser goroutine before it stops with
	// errOther.
	other diag.Diagnostic
	// mu guards canceled, the context error a checker met first.
	mu       sync.Mutex
	canceled error
	// started counts the goroutines the decode started and handed the
	// chunks it handed to them, both written by the parser goroutine;
	// inFlight counts the chunks the parser completed that no check has
	// finished, and peak the most there were at once. The tests of the
	// decode's goroutine and queue bounds read them (at most Workers-1
	// goroutines, never more than chunks handed over, and 2(Workers-1)+1
	// chunks in flight).
	started, handed int
	inFlight, peak  atomic.Int64
	// checked counts the elements the checks judged, for the tests of
	// what they skip.
	checked atomic.Int64
}

// busy adds k to the chunks in flight, recording the most.
func (r *run) busy(k int64) {
	n := r.inFlight.Add(k)
	for {
		p := r.peak.Load()
		if n <= p || r.peak.CompareAndSwap(p, n) {
			return
		}
	}
}

// cancel records the context error a checker met.
func (r *run) cancel(err error) {
	r.mu.Lock()
	if r.canceled == nil {
		r.canceled = err
	}
	r.mu.Unlock()
}

// canceledBy returns the context error a checker met, or nil.
func (r *run) canceledBy() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.canceled
}

// newRun returns the state of a decode whose cap is maxDiags, at most
// maxCount: each keeper holds one finding more, which tells that the cap
// was passed.
func newRun(maxDiags int) *run {
	r := &run{newer: newKeeper(maxDiags + 1), form: newKeeper(maxDiags + 1)}
	r.rejected.Store(math.MaxInt64)
	return r
}

// reject records that the re-encoding of resource i failed.
func (r *run) reject(i int) {
	for {
		cur := r.rejected.Load()
		if int64(i) >= cur || r.rejected.CompareAndSwap(cur, int64(i)) {
			return
		}
	}
}

// fails reports whether the decode is known to fail whatever resource i
// holds: a finding is known, or a resource before i failed its
// re-encoding. The checks then keep no tree of resource i (no tree is
// returned) and do not re-encode it.
func (r *run) fails(i int) bool {
	return r.newer.any() || r.form.any() || int64(i) > r.rejected.Load()
}

// found collects the findings of one resource in content order, at most
// limit of each code: a finding after the first limit of its resource
// cannot be among the first limit of the decode. wantForm is false when
// form findings of the resource cannot matter, so none is built.
type found struct {
	newer, form diag.List
	limit       int
	wantForm    bool
}

// reset prepares f for the next resource.
func (f *found) reset(wantForm bool) {
	clear(f.newer)
	clear(f.form)
	f.newer, f.form, f.wantForm = f.newer[:0], f.form[:0], wantForm
}

// takeNewer reports whether one more newer finding is kept.
func (f *found) takeNewer() bool { return len(f.newer) < f.limit }

// takeForm reports whether one more form finding is kept.
func (f *found) takeForm() bool { return f.wantForm && len(f.form) < f.limit }
