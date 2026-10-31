// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// The Node-local steps of the decision path
// (docs/architecture/09-traffic-management-and-resilience.md "Decision
// path", steps 2 to 4, and "Per-Node ceiling"), run on one shard under its
// lock: the over-limit cache, the first-seen budget, the local token
// buckets, and the transitions a GCRA answer or failure makes.

// limitSpec is one applied limits[] entry of a request as the table needs
// it.
type limitSpec struct {
	// id is the limit's table id.
	id uint32
	// ceiling is the per-Node ceiling: bucket capacity and refill per
	// window.
	ceiling int64
	// allowance is the fail-open allowance per epoch-aligned window.
	allowance int64
	// window is the window in nanoseconds.
	window int64
}

// verdict is what the Node-local steps decided.
type verdict uint8

// Verdicts.
const (
	// verdictGCRA admitted locally; GCRA decides.
	verdictGCRA verdict = iota
	// verdictLocal admitted on local buckets alone: a local-only entry or
	// a localOnly Policy.
	verdictLocal
	// verdictLocalDeny: a bucket lacks the cost (429 RZ-RL-001).
	verdictLocalDeny
	// verdictOverLimit: the over-limit cache denies (429 RZ-RL-002).
	verdictOverLimit
)

// local is the outcome of the Node-local steps for one request.
type local struct {
	verdict verdict
	// denied is the applied index of the denying limit and wait the time
	// until it admits, for the two denials.
	denied int
	wait   time.Duration
	// failOpen reports an applied bucket on the fail-open refill, whose
	// count is not reported.
	failOpen bool
	// remaining and reset are each applied bucket's tokens after the take
	// and the time until it is whole again, for verdictLocal.
	remaining [MaxLimits]int64
	reset     [MaxLimits]time.Duration
}

// worse reports whether a wait at index i replaces the worst so far: the
// longest wait wins, the lowest index on ties.
func worse(wait, worst time.Duration, i, at int) bool {
	return at < 0 || wait > worst || (wait == worst && i < at)
}

// take runs steps 2 to 4 for the applied limits specs of one Policy key d
// at now (Unix ns), taking cost tokens from every bucket or from none. A
// localOnly Policy skips the over-limit cache and the first-seen budget.
// Evictions are counted on stripe.
func (t *Table) take(policySeed uint64, d *statestore.Digest, specs []limitSpec, cost int64, localOnly bool, now int64, stripe emit.Stripe, out *local) {
	*out = local{denied: -1}
	sh := t.shardOf(policySeed, d)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	var vs [MaxLimits]ref
	found, onLocal, expiry := 0, false, int64(0)
	for i, s := range specs {
		slot, v := t.find(sh, s.id, d)
		if v == 0 {
			continue
		}
		e := sh.entry(v)
		if seg, _ := sh.at(v); seg == segLocalOnly {
			if e.until <= now {
				// A local-only entry ends after its life: the key is
				// first-seen again.
				t.remove(sh, slot, v)
				continue
			}
			onLocal, expiry = true, e.until
		}
		e.flags |= flagPinned | flagRef
		vs[i] = v
		found++
	}
	defer unpin(sh, vs[:len(specs)])

	// Step 2: a key whose last GCRA answer denied stays denied until its
	// retry-after, without a bucket or a call.
	if !localOnly {
		for i, v := range vs[:len(specs)] {
			if v == 0 {
				continue
			}
			if seg, _ := sh.at(v); seg != segMain {
				continue
			}
			e := sh.entry(v)
			if e.flags&flagFailOpen != 0 || e.until <= now {
				continue
			}
			if w := time.Duration(e.until - now); worse(w, out.wait, i, out.denied) {
				out.denied, out.wait = i, w
			}
		}
		if out.denied >= 0 {
			out.verdict = verdictOverLimit
			return
		}
	}

	// Step 3: a key with no local entry is first-seen. Within the budget
	// it gets full buckets; past it, local-only entries for LocalOnlyLife.
	seg := segMain
	switch {
	case found == 0 && !localOnly && !t.takeFirstSeen(now):
		seg, onLocal, expiry = segLocalOnly, true, now+int64(LocalOnlyLife)
	case onLocal:
		seg = segLocalOnly
	}
	for i, s := range specs {
		if vs[i] != 0 {
			continue
		}
		// A new entry starts without its reference bit, so keys seen once,
		// as in a key-rotation flood, go before keys seen again.
		v := t.insert(sh, seg, s.id, d, stripe)
		e := sh.entry(v)
		e.tokens, e.last, e.flags = s.ceiling, now, flagPinned
		if seg == segLocalOnly {
			e.until = expiry
		}
		vs[i] = v
	}

	// Step 4: every applied bucket must hold cost tokens, else 429 by the
	// bucket that waits longest.
	for i, s := range specs {
		e := sh.entry(vs[i])
		var w time.Duration
		if e.flags&flagFailOpen != 0 {
			out.failOpen = true
			failOpenRefill(&e.tokens, &e.last, &e.until, now, s.window, s.allowance)
			w = failOpenWait(e.tokens, e.last, e.until, now, s.window, s.allowance, cost)
		} else {
			refill(&e.tokens, &e.last, now, s.ceiling, s.window, s.ceiling)
			// A cost above the ceiling never fits: report the wait until
			// the bucket is whole.
			w = fillTime(e.tokens, e.last, now, min(cost, s.ceiling), s.ceiling, s.window)
			if cost > s.ceiling && w == 0 {
				w = time.Duration(s.window)
			}
		}
		if e.tokens < cost && worse(w, out.wait, i, out.denied) {
			out.denied, out.wait = i, w
		}
	}
	if out.denied >= 0 {
		out.verdict = verdictLocalDeny
		return
	}
	for i, s := range specs {
		e := sh.entry(vs[i])
		e.tokens -= cost
		out.remaining[i] = e.tokens
		if e.flags&flagFailOpen == 0 {
			out.reset[i] = fillTime(e.tokens, e.last, now, s.ceiling, s.ceiling, s.window)
		}
	}
	if localOnly || onLocal {
		out.verdict = verdictLocal
	}
}

// unpin clears the pin of the entries an operation held.
func unpin(sh *shard, vs []ref) {
	for _, v := range vs {
		if v != 0 {
			sh.entry(v).flags &^= flagPinned
		}
	}
}

// each runs fn on every applied limit's entry the table still holds in
// the main segment, under the shard's lock.
func (t *Table) each(policySeed uint64, d *statestore.Digest, specs []limitSpec, fn func(i int, s limitSpec, e *entry)) {
	sh := t.shardOf(policySeed, d)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for i, s := range specs {
		_, v := t.find(sh, s.id, d)
		if v == 0 {
			continue
		}
		if seg, _ := sh.at(v); seg != segMain {
			continue
		}
		e := sh.entry(v)
		e.flags |= flagRef
		fn(i, s, e)
	}
}

// answered records a GCRA answer at now for the applied limits: a bucket
// on the fail-open refill returns to the continuous refill with the tokens
// it holds, and when the answer denied, the denying limit's entry caches
// the denial until now + retry (step 2). The over-limit cache of the
// other limits ends.
func (t *Table) answered(policySeed uint64, d *statestore.Digest, specs []limitSpec, now int64, denied int, retry time.Duration) {
	t.each(policySeed, d, specs, func(i int, _ limitSpec, e *entry) {
		if e.flags&flagFailOpen != 0 {
			e.flags &^= flagFailOpen
			e.last = now
		}
		e.until = 0
		if i == denied {
			e.until = now + int64(retry)
		}
	})
}

// failedOpen switches the applied limits' buckets to the fail-open refill
// at now, after a failed or unattempted GCRA call under failureMode open:
// each keeps the tokens it holds, at most its allowance, for the rest of
// the epoch-aligned window in progress, and later windows bring the
// allowance without carry-over. A bucket already on it stays on it.
func (t *Table) failedOpen(policySeed uint64, d *statestore.Digest, specs []limitSpec, now int64) {
	t.each(policySeed, d, specs, func(_ int, s limitSpec, e *entry) {
		if e.flags&flagFailOpen != 0 {
			return
		}
		refill(&e.tokens, &e.last, now, s.ceiling, s.window, s.ceiling)
		e.tokens = min(e.tokens, s.allowance)
		e.until, e.last = alignDown(now, s.window), -1
		e.flags |= flagFailOpen
	})
}

// returnTokens gives cost tokens back to the applied limits' buckets at
// now (Consumptive.Undo), each up to its capacity: the ceiling, or the
// allowance on the fail-open refill. Local-only entries take them back
// too.
func (t *Table) returnTokens(policySeed uint64, d *statestore.Digest, specs []limitSpec, cost, now int64) {
	sh := t.shardOf(policySeed, d)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for _, s := range specs {
		_, v := t.find(sh, s.id, d)
		if v == 0 {
			continue
		}
		e := sh.entry(v)
		if e.flags&flagFailOpen != 0 {
			failOpenRefill(&e.tokens, &e.last, &e.until, now, s.window, s.allowance)
			e.tokens = min(e.tokens+cost, s.allowance)
			continue
		}
		refill(&e.tokens, &e.last, now, s.ceiling, s.window, s.ceiling)
		e.tokens = min(e.tokens+cost, s.ceiling)
	}
}
