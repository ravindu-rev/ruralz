// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// errGoexit is the value of the unitPanic of a work unit that ended
// without returning and without a panic to recover: something it called
// ran runtime.Goexit (t.FailNow in a test oracle, say), or panicked with
// nil under GODEBUG=panicnil=1, which recover cannot tell apart.
var errGoexit = errors.New("precedence: runtime.Goexit ended a work unit")

// unitPanic is a panic raised, or a runtime.Goexit called (value
// errGoexit), by one work unit of forEach, carried to the goroutine that
// called forEach. It is an error that wraps the panic value when that
// value is an error, so errors.Is and errors.As reach it, and its text
// holds the value and the stack of the unit.
type unitPanic struct {
	// value is what the unit passed to panic, or errGoexit.
	value any
	// unit is the index of the unit.
	unit int
	// stack is the unit's stack at the panic or the Goexit.
	stack []byte
	// goexit reports a runtime.Goexit.
	goexit bool
}

// Error returns the panic value, the unit and its stack.
func (p *unitPanic) Error() string {
	what := "panicked"
	if p.goexit {
		what = "stopped"
	}
	return fmt.Sprintf("precedence: work unit %d %s: %v\n\n%s", p.unit, what, p.value, p.stack)
}

// Unwrap returns the panic value when it is an error, else nil.
func (p *unitPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// pool is the bounded worker pool of one call: at most size goroutines
// per forEach, each with its own worker, created on first use and kept
// for the whole call so a worker's yield timer carries from one forEach
// to the next. Worker 0 also serves the calling goroutine outside
// forEach. A pool belongs to one call.
type pool struct {
	// size is the most workers, at least 1.
	size int
	// newWorker returns a fresh worker.
	newWorker func() *worker
	// workers are the workers created so far.
	workers []*worker
	// took, admitted and stopped are test hooks, nil otherwise: a worker
	// received unit from the queue; it will run unit; the panic or Goexit
	// of unit was recorded. forEach calls them under the lock that orders
	// admissions against the recording of a failure, so a test sees one
	// order of these events whatever the scheduling.
	took, admitted, stopped func(unit int)
	// stack captures the stack of a failing unit; nil means debug.Stack.
	// Tests set it to check that the stop is recorded first.
	stack func() []byte
}

// worker returns worker j, creating it and every worker before it.
func (p *pool) worker(j int) *worker {
	for len(p.workers) <= j {
		p.workers = append(p.workers, p.newWorker())
	}
	return p.workers[j]
}

// hook calls h(unit) when h is set.
func hook(h func(int), unit int) {
	if h != nil {
		h(unit)
	}
}

// run reads w's yield timer, calls fn(w, i) and merges the findings w
// buffered into the call's collector; the timer read and the merge are
// part of the unit, so a panic in Options.Now there is recovered like
// one in fn. When the unit panics, or ends through runtime.Goexit (which
// no recover sees: the unit simply never sets done), the deferred
// function calls stop first, before the stack is captured, so dispatch
// and admission end as early as the failure can be observed, then hands
// fail the unitPanic; after a Goexit the goroutine then ends, so fail is
// the only way the failure reaches the caller. fn's own deferred calls
// run before any recovery, while other units may still start.
func (p *pool) run(fn func(w *worker, i int), w *worker, i int, stop func(), fail func(*unitPanic)) {
	done := false
	defer func() {
		if done {
			return
		}
		v := recover()
		stop()
		capture := p.stack
		if capture == nil {
			capture = debug.Stack
		}
		up := &unitPanic{value: v, unit: i, stack: capture()}
		if v == nil {
			up.value, up.goexit = errGoexit, true
		}
		fail(up)
	}()
	w.check()
	fn(w, i)
	w.flush()
	done = true
}

// forEach calls fn(w, i) for every i in [0, n) on at most p.size
// goroutines it starts and joins, worker j on goroutine j; the calling
// goroutine only dispatches and waits, so no unit ever runs on it and a
// unit's runtime.Goexit can only end a pool goroutine. Work is fed
// through a channel bounded by the worker count, and each worker reads
// its yield timer before every unit, inside the unit's recovered region
// (run).
//
// A unit starts when its worker admits it. No unit is admitted once ctx
// has ended (a unit already queued is drained without running) or once a
// unit has failed (panicked, or ended through runtime.Goexit): the
// recovery records the stop first, under the lock every admission takes,
// then dispatch ends, also when every worker is gone. Units admitted
// before that run to completion, and the failing unit's own deferred
// calls, which run before its failure is recorded, may overlap other
// units. forEach waits for every started unit before it returns, so no
// goroutine outlives it. It returns ctx.Err() when ctx ended before every
// unit started, and nil when every unit ran. A failure is raised again
// in the calling goroutine once every started unit has returned, as a
// *unitPanic (the first recorded when several units fail; for a Goexit,
// one wrapping errGoexit), the same way for every worker count, so the
// worker count never changes how a failure surfaces and no failing unit
// is ever left out of the result silently. fn must write only state
// owned by its index or its worker.
func (p *pool) forEach(ctx context.Context, n int, fn func(w *worker, i int)) error {
	if n <= 0 {
		return nil
	}
	nw := min(p.size, n)
	p.worker(nw - 1)
	jobs := make(chan int, nw)
	var (
		// gate orders every admission against the recording of a failure;
		// it guards first, failed and skipped.
		gate    sync.Mutex
		stop    atomic.Bool
		halted  = make(chan struct{})
		first   = -1
		failed  *unitPanic
		skipped bool
	)
	admit := func(i int) bool {
		gate.Lock()
		defer gate.Unlock()
		hook(p.took, i)
		if stop.Load() {
			return false
		}
		if ctx.Err() != nil {
			skipped = true
			return false
		}
		hook(p.admitted, i)
		return true
	}
	fail := func(up *unitPanic) {
		gate.Lock()
		defer gate.Unlock()
		if up.unit == first {
			failed = up
		}
	}
	var wg sync.WaitGroup
	wg.Add(nw)
	for j := range nw {
		w := p.workers[j]
		go func() {
			defer wg.Done()
			for i := range jobs {
				// After a failure, or once ctx has ended, every further
				// unit is drained without running, so dispatch never
				// blocks on a live worker and no unit starts.
				if !admit(i) {
					continue
				}
				p.run(fn, w, i, func() {
					gate.Lock()
					defer gate.Unlock()
					if !stop.Load() {
						stop.Store(true)
						first = i
						hook(p.stopped, i)
						close(halted)
					}
				}, fail)
			}
		}()
	}
	// Dispatch stops at the first unit after a failure was recorded or
	// ctx ended; a send waits only until a worker takes a unit, which
	// every live worker does, running it or draining it, or until a
	// failure is recorded, which a Goexit that ended the last worker
	// waiting to receive also does.
	var err error
dispatch:
	for i := range n {
		if stop.Load() {
			break
		}
		if err = ctx.Err(); err != nil {
			break
		}
		select {
		case jobs <- i:
		case <-halted:
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	if failed != nil {
		panic(failed)
	}
	if err == nil && skipped {
		err = ctx.Err()
	}
	return err
}
