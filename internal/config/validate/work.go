// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"runtime"
	"time"
)

// DefaultYieldAfter is the work a worker does before it yields the
// processor when Options.YieldAfter is zero, as in stages G and I.
const DefaultYieldAfter = 100 * time.Microsecond

// yieldEvery is how many work units pass between two reads of the clock.
const yieldEvery = 256

// work is the yield timer of one goroutine: it counts work units (nodes
// visited, list entries read, checks run) and, every yieldEvery units,
// calls runtime.Gosched once after has passed since the last yield, so
// stage H shares the processor with live traffic during activation. A
// nil now never yields and reads no clock.
type work struct {
	now   func() time.Time
	after time.Duration
	last  time.Time
	units int
	// total counts every unit, and yields the calls of runtime.Gosched,
	// for tests.
	total  int
	yields int
}

// newWork returns the timer of one goroutine.
func (v *Validator) newWork() *work {
	return &work{now: v.now, after: v.yieldAfter}
}

// charge counts n units and checks the clock every yieldEvery units.
func (w *work) charge(n int) {
	w.total += n
	w.units += n
	if w.units >= yieldEvery {
		w.units = 0
		w.check()
	}
}

// check reads the clock and yields once after has passed since the last
// yield (or the first read).
func (w *work) check() {
	if w.now == nil {
		return
	}
	t := w.now()
	if w.last.IsZero() {
		w.last = t
		return
	}
	if t.Sub(w.last) >= w.after {
		runtime.Gosched()
		w.yields++
		w.last = w.now()
	}
}
