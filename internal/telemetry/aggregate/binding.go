// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Binding states.
const (
	bindLive uint8 = iota
	bindRetired
	bindReleased
)

// Binding tracks the label sets one snapshot references (emit.Binding,
// held in snapshot.Snapshot.Binding, R-58). Retire and Release run off
// the request path and are idempotent.
type Binding struct {
	r     *Registry
	p     *Plan
	state uint8
}

// noopBinding is returned for a Plan this registry did not make.
type noopBinding struct{}

// Retire implements emit.Binding.
func (noopBinding) Retire() {}

// Release implements emit.Binding.
func (noopBinding) Release() {}

// Bind implements emit.Meter. At the swap the plan's label sets become
// live: a pending group is published (or joins a group another plan
// published meanwhile), a live or retiring one gains a reference, and one
// released or folded since Admit is replaced by a fresh group that starts
// from 0 (spec 09 req 56). Binding a plan twice returns the same Binding.
func (r *Registry) Bind(pl emit.Plan) emit.Binding {
	p, ok := pl.(*Plan)
	if !ok || p.r != r {
		return noopBinding{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.binding != nil {
		return p.binding
	}
	now := r.clk.Now()
	for _, e := range p.entries {
		r.resolve(e, now)
		g := e.g
		g.refs++
		g.live++
		if g.state == stRetiring {
			r.leaveRetiring(g)
		}
		g.state = stLive
		e.pos = len(g.entries)
		g.entries = append(g.entries, e)
	}
	for _, f := range r.fams {
		f.activeFolded = p.folded[f.idx]
	}
	r.tidyQueue()
	p.binding = &Binding{r: r, p: p}
	return p.binding
}

// resolve points e at the current group of its key.
func (r *Registry) resolve(e *planEntry, now time.Time) {
	g := e.g
	f := g.fam
	switch g.state {
	case stLive, stRetiring, stFixed:
		return
	case stPending:
		if cur := f.groups[g.key]; cur != nil {
			g.state = stEnded
			e.g = cur
			e.hs.retarget(cur)
			return
		}
		f.groups[g.key] = g
		f.dirty = true
	case stFolded, stEnded:
		cur := f.groups[g.key]
		if cur == nil {
			cur = r.newGroup(f, groupSpec{key: g.key, striped: g.striped, codes: g.codeList(), start: now})
			cur.state = stPending
			f.groups[g.key] = cur
			f.dirty = true
		}
		e.g = cur
		e.hs.retarget(cur)
	}
}

// leaveRetiring takes a group that stops retiring (bound again, released
// or folded) out of the retiring accounting and clears its queue item.
func (r *Registry) leaveRetiring(g *group) {
	r.retiringSeries -= g.ceil
	if g.qpos >= 0 {
		r.queue[g.qpos] = retireItem{}
		g.qpos = -1
		r.stale++
	}
}

// Retire implements emit.Binding: the snapshot is retired or closing. A
// label set no live snapshot references becomes retiring; past the
// Node-wide retiring ceiling the oldest retiring sets fold into _overflow.
func (b *Binding) Retire() {
	r := b.r
	r.mu.Lock()
	defer r.mu.Unlock()
	b.retire()
	r.tidyQueue()
}

func (b *Binding) retire() {
	if b.state != bindLive {
		return
	}
	b.state = bindRetired
	r := b.r
	for _, e := range b.p.entries {
		g := e.g
		g.live--
		if g.live > 0 || g.state != stLive {
			continue
		}
		g.state = stRetiring
		// Every retiring series counts against the ceiling, listener ones
		// included; only foldable groups queue to be folded (listener
		// families never fold, req 55).
		r.retiringSeries += g.ceil
		if g.foldable() {
			g.qpos = len(r.queue)
			r.queue = append(r.queue, retireItem{g: g})
		}
	}
	r.enforceCeiling(r.clk.Now())
}

// Release implements emit.Binding: the snapshot was freed. Label sets no
// other snapshot references end and leave both exports; a returning name
// restarts from 0.
func (b *Binding) Release() {
	r := b.r
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.tidyQueue()
	b.retire()
	if b.state == bindReleased {
		return
	}
	b.state = bindReleased
	for _, e := range b.p.entries {
		g := e.g
		last := len(g.entries) - 1
		if e.pos <= last && g.entries[e.pos] == e {
			g.entries[e.pos] = g.entries[last]
			g.entries[e.pos].pos = e.pos
			g.entries[last] = nil
			g.entries = g.entries[:last]
		}
		g.refs--
		if g.refs > 0 {
			continue
		}
		switch g.state {
		case stRetiring:
			r.leaveRetiring(g)
			delete(g.fam.groups, g.key)
			g.fam.dirty = true
		case stFolded:
			g.fam.ceilingFolded -= g.sets
		case stPending, stLive, stEnded, stFixed:
		}
		g.state = stEnded
	}
}

// enforceCeiling folds the oldest retiring groups while the retiring
// series exceed the ceiling (spec 09 req 56). Folding redirects every
// handle of the group to _overflow; its accumulated values are dropped,
// never carried.
func (r *Registry) enforceCeiling(now time.Time) {
	for r.retiringSeries > r.limits.Retiring && r.queueHead < len(r.queue) {
		g := r.queue[r.queueHead].g
		r.queue[r.queueHead] = retireItem{}
		r.queueHead++
		if g == nil {
			r.stale--
			continue
		}
		g.qpos = -1
		r.retiringSeries -= g.ceil
		og := r.overflowGroup(g.fam, g.key.phase, now)
		for _, e := range g.entries {
			e.hs.retarget(og)
		}
		g.state = stFolded
		delete(g.fam.groups, g.key)
		g.fam.dirty = true
		g.fam.ceilingFolded += g.sets
	}
}

// queueSlack is how many cleared items and consumed head slots the
// retiring queue keeps before it is compacted.
const queueSlack = 64

// tidyQueue bounds the retiring queue after every Bind, Retire and
// Release: it drops cleared items at the head, and compacts the queue once
// more than queueSlack items are cleared or consumed, so it never holds
// more than queueSlack items besides those of the retiring foldable
// groups and never keeps an ended group reachable.
func (r *Registry) tidyQueue() {
	for r.queueHead < len(r.queue) && r.queue[r.queueHead].g == nil {
		r.queueHead++
		r.stale--
	}
	if r.queueHead < len(r.queue) && r.stale <= queueSlack && r.queueHead <= queueSlack {
		return
	}
	n := 0
	for _, it := range r.queue[r.queueHead:] {
		if it.g != nil {
			it.g.qpos = n
			r.queue[n] = it
			n++
		}
	}
	clear(r.queue[n:])
	r.queue = r.queue[:n]
	r.queueHead, r.stale = 0, 0
	// A burst of retirements may have grown the array far beyond what
	// is left; give the excess back.
	if cap(r.queue) > 4*n+queueSlack {
		r.queue = append(make([]retireItem, 0, 2*n), r.queue...)
	}
}
