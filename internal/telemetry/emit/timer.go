// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package emit

import (
	"sync/atomic"
	"time"
)

// Excluded-section word: the depth in the high 16 bits, and in the low
// 48 bits (nanoseconds since the request start, about 78 hours) the start
// of the current excluded run, or at depth 0 the end of the last one.
const (
	depthShift = 48
	depthOne   = uint64(1) << depthShift
	offsetMask = depthOne - 1
	maxDepth   = 1<<16 - 1
)

// GatewayTimer measures gateway-added time (spec 09 req 53 and 54): wall
// clock time minus the union of excluded sections (client reads and
// writes, upstream I/O, State Store round trips, declared remote calls).
// One word packs the excluded depth and the start of the current excluded
// run and is updated by compare and swap, so sections nest and parallel
// legs overlap safely; the zero value is ready after Reset. It implements
// Excluder and is meant to live in a pooled per-request structure.
//
// A caller reads the clock before its compare and swap lands, so with
// parallel legs the swaps can land in another order than the clock
// reads. The timer keeps the result the union anyway: a run ends no
// earlier than the latest offset any Leave of it read (maxLeave, raised
// before each Leave's swap); a run starts no earlier than the end of the
// last closed run, which the closing swap leaves in the word, so the
// swap that opens the next run checks it atomically; and a section whose
// Enter read the clock before the run's start but swapped after it moves
// the start back to its own offset, never below floor (the end of the
// last closed run, raised before the closing swap). Runs therefore never
// overlap and stay within the sections' hull. The one residual error is
// an under-count: a section that enters and leaves entirely while the
// leg closing the run is preempted between its maxLeave read and its
// swap may end the run early.
type GatewayTimer struct {
	start    time.Time
	word     atomic.Uint64
	maxLeave atomic.Uint64
	floor    atomic.Uint64
	excluded atomic.Int64
	anomaly  atomic.Bool
}

var _ Excluder = (*GatewayTimer)(nil)

// Reset starts a new measurement at start; call it before any Enter.
func (g *GatewayTimer) Reset(start time.Time) {
	g.start = start
	g.word.Store(0)
	g.maxLeave.Store(0)
	g.floor.Store(0)
	g.excluded.Store(0)
	g.anomaly.Store(false)
}

// offset returns now as nanoseconds since the start, false when it is
// before the start or beyond the 48-bit range.
func (g *GatewayTimer) offset(now time.Time) (uint64, bool) {
	d := now.Sub(g.start)
	if d < 0 || d > time.Duration(offsetMask) {
		return 0, false
	}
	return uint64(d), true
}

// storeMax raises a to v unless it already holds at least v.
func storeMax(a *atomic.Uint64, v uint64) {
	for {
		o := a.Load()
		if v <= o || a.CompareAndSwap(o, v) {
			return
		}
	}
}

// Enter opens an excluded section at now; the outermost one starts a run.
func (g *GatewayTimer) Enter(now time.Time) {
	off, ok := g.offset(now)
	if !ok {
		g.anomaly.Store(true)
		return
	}
	for {
		old := g.word.Load()
		var next uint64
		switch depth := old >> depthShift; depth {
		case 0:
			// The low bits are the end of the last run.
			next = depthOne | max(off, old&offsetMask)
		case maxDepth:
			g.anomaly.Store(true)
			return
		default:
			next = old + depthOne
			// floor is read after the word: the closing swap before
			// this run raised it first.
			if at := max(off, g.floor.Load()); at < old&offsetMask {
				next = (depth+1)<<depthShift | at
			}
		}
		if g.word.CompareAndSwap(old, next) {
			return
		}
	}
}

// Leave closes an excluded section at now; the outermost one adds its run
// to the excluded total. A leave at depth 0, or a run that would end
// before it started, marks the measurement as a clock anomaly.
func (g *GatewayTimer) Leave(now time.Time) {
	off, ok := g.offset(now)
	if !ok {
		g.anomaly.Store(true)
		return
	}
	// Published before the swap, so the swap that closes the run sees it.
	storeMax(&g.maxLeave, off)
	for {
		old := g.word.Load()
		depth := old >> depthShift
		if depth == 0 {
			g.anomaly.Store(true)
			return
		}
		if depth > 1 {
			if g.word.CompareAndSwap(old, old-depthOne) {
				return
			}
			continue
		}
		run := old & offsetMask
		end := max(off, g.maxLeave.Load())
		storeMax(&g.floor, end)
		if !g.word.CompareAndSwap(old, end) {
			continue
		}
		if end < run {
			g.anomaly.Store(true)
			return
		}
		g.excluded.Add(int64(end - run)) //nolint:gosec // G115: below 2^48.
		return
	}
}

// Result returns the gateway-added time of a request that ended at end,
// read once after its sections closed; a section still open counts as
// excluded up to end. It returns false for a clock anomaly (a result
// below 0 or above wall-clock time, or a leave at depth 0): the caller
// counts ruralz_http_gateway_duration_skipped_total{reason="clock_anomaly"}
// instead of recording.
func (g *GatewayTimer) Result(end time.Time) (time.Duration, bool) {
	wall := end.Sub(g.start)
	if wall < 0 || g.anomaly.Load() {
		return 0, false
	}
	excluded := time.Duration(g.excluded.Load())
	if w := g.word.Load(); w>>depthShift != 0 {
		open := wall - time.Duration(w&offsetMask)
		if open < 0 {
			return 0, false
		}
		excluded += open
	}
	d := wall - excluded
	if d < 0 || d > wall {
		return 0, false
	}
	return d, true
}

// Observe records the result into h on stripe s, or counts skipped when
// it is a clock anomaly.
func (g *GatewayTimer) Observe(end time.Time, s Stripe, h Histogram, skipped Counter) (time.Duration, bool) {
	d, ok := g.Result(end)
	if !ok {
		skipped.Add(s, 1)
		return 0, false
	}
	h.Record(s, uint64(d)) //nolint:gosec // G115: d is not negative.
	return d, true
}
