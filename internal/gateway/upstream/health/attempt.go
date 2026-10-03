// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Attempt phases, in the low two bits of Attempt.state; the rest is the
// attempt's generation, which grows with each reuse.
const (
	phaseIdle uint64 = iota
	phaseRunning
	phaseStalled
	phaseSettled
	phaseMask uint64 = 3
)

// Attempt tracks one attempt to one Endpoint for stall and suspect
// detection and passive ejection (05 reqs 18 to 21). The attempt loop keeps
// one Attempt per leg (pooled with the leg) and reuses it for every
// attempt: View.Begin arms its stall timer, Headers records response
// headers, End or Cancel finishes it. The timer is created once and reset
// per attempt, so tracking allocates nothing after the first use.
//
// Begin, Headers, End and Cancel must be called from one goroutine at a
// time (the attempt's); only the stall timer runs concurrently with them.
// The stall timer comes from the clock of the first Tracker the Attempt
// serves, so every Tracker it serves must share that clock (the Node's).
type Attempt struct {
	// state is generation<<2 | phase; tr, ep and start are published
	// before state, so the stall callback reads a consistent attempt.
	// start is an instant of tr (Tracker.at).
	state atomic.Uint64
	start atomic.Int64
	tr    atomic.Pointer[Tracker]
	ep    atomic.Pointer[endpoint]

	timer clock.Timer
	ended bool
}

// begin starts an attempt to e at now, finishing any previous one first.
func (a *Attempt) begin(t *Tracker, e *endpoint, now time.Time) {
	if !a.ended && a.state.Load()&phaseMask != phaseIdle {
		a.Cancel()
	}
	a.ended = false
	gen := a.state.Load()>>2 + 1
	a.tr.Store(t)
	a.ep.Store(e)
	a.start.Store(t.at(now))
	a.state.Store(gen<<2 | phaseRunning)
	if a.timer == nil {
		a.timer = t.clk.AfterFunc(StallAfter, a.onStall)
		return
	}
	a.timer.Reset(StallAfter)
}

// onStall is the stall timer: an attempt still waiting for headers
// StallAfter after it started becomes stalled (05 req 21). A late firing
// armed for an earlier generation finds the current attempt younger than
// StallAfter and does nothing; the current generation's own firing
// follows. The age is elapsed time, like the timer's, so a wall-clock step
// neither suppresses nor hastens a stall.
func (a *Attempt) onStall() {
	s := a.state.Load()
	if s&phaseMask != phaseRunning {
		return
	}
	start, t, e := a.start.Load(), a.tr.Load(), a.ep.Load()
	if a.state.Load() != s || t == nil || e == nil {
		return
	}
	if t.elapsed()-start < int64(StallAfter) {
		return
	}
	if a.state.CompareAndSwap(s, s&^phaseMask|phaseStalled) {
		e.stalled.Add(1)
		t.stalled.Add(1)
	}
}

// settle moves a running or stalled attempt to settled, releasing its
// stall counts.
func (a *Attempt) settle() {
	for {
		s := a.state.Load()
		p := s & phaseMask
		if p != phaseRunning && p != phaseStalled {
			return
		}
		if a.state.CompareAndSwap(s, s&^phaseMask|phaseSettled) {
			if p == phaseStalled {
				a.ep.Load().stalled.Add(-1)
				a.tr.Load().stalled.Add(-1)
			}
			return
		}
	}
}

// Stalled reports whether the attempt is stalled now.
func (a *Attempt) Stalled() bool { return a.state.Load()&phaseMask == phaseStalled }

// Headers records that the attempt got response headers at now: it can no
// longer stall, and the Endpoint stops being suspect (05 req 21).
func (a *Attempt) Headers(now time.Time) {
	if a.ended || a.state.Load()&phaseMask == phaseIdle {
		return
	}
	a.timer.Stop()
	a.settle()
	e := a.ep.Load()
	n := a.tr.Load().at(now)
	for {
		old := e.lastHeaders.Load()
		if old >= n || e.lastHeaders.CompareAndSwap(old, n) {
			return
		}
	}
}

// End finishes the attempt at now with its outcome for passive ejection
// (05 reqs 18, 19 and 21) and the recent-failure count (05 req 13). It
// reports whether the outcome ejected the Endpoint. Calls after the first
// End or Cancel of an attempt do nothing.
func (a *Attempt) End(now time.Time, o Outcome) bool {
	if a.ended || a.state.Load()&phaseMask == phaseIdle {
		return false
	}
	a.ended = true
	e, t := a.ep.Load(), a.tr.Load()
	n := t.at(now)
	// Suspicion is judged with this attempt still counted: a timeout of
	// the attempt that made the Endpoint suspect ejects it.
	suspect := e.suspect(n)
	a.timer.Stop()
	a.settle()
	return t.observe(e, n, o, suspect)
}

// Cancel finishes the attempt without an outcome (the client went away or
// the leg was abandoned): no ejection bookkeeping.
func (a *Attempt) Cancel() {
	if a.ended || a.state.Load()&phaseMask == phaseIdle {
		return
	}
	a.ended = true
	a.timer.Stop()
	a.settle()
}
