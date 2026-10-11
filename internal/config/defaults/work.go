// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// yieldCheck is the number of work units (nodes visited, sort
// comparisons) between two reads of the yield timer (01 req 53: checked
// every 256 nodes inside a unit).
const yieldCheck = 256

// yieldAfter is the work a worker does before it yields the processor
// (01 req 53).
const yieldAfter = 100 * time.Microsecond

// work is the state one goroutine carries through every step of stage G
// (Materialize, conversion, Normalize, Decode) and from one resource to
// the next: the yield timer of 01 req 53, one counter for all steps, and
// the diagnostic budget of 01 req 50, which Run shares among its workers.
// A work value belongs to one goroutine; only the budget is shared.
type work struct {
	// units counts the work units since the timer was last read.
	units int
	// last is when the worker last yielded, or started.
	last time.Time
	// budget is the number of diagnostics the run may still record. It
	// goes below zero once a diagnostic was dropped.
	budget *atomic.Int64
	// chunk, when positive, replaces decodeChunk, so tests can split
	// every value.
	chunk int
	// decodes counts the encoding/json calls, for tests.
	decodes int
}

// newWork returns the state of one worker drawing on budget, or on a
// budget of its own of MaxDiagnostics when budget is nil.
func newWork(budget *atomic.Int64) *work {
	if budget == nil {
		budget = newBudget()
	}
	// The yield timer is the one clock read 01 req 55 allows.
	return &work{last: time.Now(), budget: budget}
}

// newBudget returns a run-wide budget of MaxDiagnostics.
func newBudget() *atomic.Int64 {
	b := new(atomic.Int64)
	b.Store(MaxDiagnostics)
	return b
}

// tick counts one work unit and, every yieldCheck units, yields when due.
// A nil w does nothing, for the helpers tests call without a worker.
func (w *work) tick() {
	if w == nil {
		return
	}
	if w.units++; w.units >= yieldCheck {
		w.units = 0
		w.check()
	}
}

// check yields the processor once yieldAfter has passed since the last
// yield (01 req 53). Run calls it between resources, Decode before and
// after each encoding/json call, tick every yieldCheck units. A nil w
// does nothing.
func (w *work) check() {
	if w == nil {
		return
	}
	if time.Since(w.last) >= yieldAfter {
		runtime.Gosched()
		w.last = time.Now()
	}
}

// chunkLimit returns the most nodes one encoding/json call decodes.
func (w *work) chunkLimit() int {
	if w.chunk > 0 {
		return w.chunk
	}
	return decodeChunk
}

// take reserves room for one diagnostic. It returns false when the budget
// is spent: the caller then drops the diagnostic, and spent reports it.
func (w *work) take() bool { return w.budget.Add(-1) >= 0 }

// admit returns the prefix of ds the budget has room for.
func (w *work) admit(ds diag.List) diag.List {
	for i := range ds {
		if !w.take() {
			return ds[:i]
		}
	}
	return ds
}

// spent reports whether the budget dropped a diagnostic.
func (w *work) spent() bool { return w.budget.Load() < 0 }

// limit returns ds with the RZ-CFG-001 "too many diagnostics" error
// appended when the budget dropped a diagnostic, as the pipeline's
// diag.Collector does (01 req 50).
func limit(ds diag.List, spent bool) diag.List {
	if !spent {
		return ds
	}
	return append(ds, diag.Diagnostic{
		Code: CodeLimit, Severity: diag.SeverityError,
		Message: "too many diagnostics; stopped after " + strconv.Itoa(MaxDiagnostics),
	})
}
