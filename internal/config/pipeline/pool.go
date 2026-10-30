// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// worker is the yield timer of one goroutine of a run. The units of the
// stages this package runs itself call yield between pieces of work, so a
// Node's activation shares the processor with live traffic. A worker
// belongs to one goroutine.
type worker struct {
	now     func() time.Time
	after   time.Duration
	started bool
	last    time.Time
	// yields counts the yields, for tests.
	yields int
}

// newWorker returns a yield timer; it reads no clock until its first
// yield.
func (p *Pipeline) newWorker() *worker { return &worker{now: p.now, after: p.yieldAfter} }

// yield gives up the processor once w.after has passed since the last
// yield; the first call starts the interval. Without a clock it does
// nothing.
func (w *worker) yield() {
	if w.now == nil {
		return
	}
	if !w.started {
		w.started, w.last = true, w.now()
		return
	}
	if w.now().Sub(w.last) >= w.after {
		runtime.Gosched()
		w.yields++
		w.last = w.now()
	}
}

// errGoexit is the value of a unitPanic whose work called runtime.Goexit.
var errGoexit = errors.New("pipeline: runtime.Goexit ended a worker")

// unitPanic is a panic, or a runtime.Goexit, inside one work item, raised
// again on the goroutine that called the run. It wraps the panic value
// when that value is an error, so errors.Is and errors.As reach it.
type unitPanic struct {
	value any
	item  int
	stack []byte
}

// Error returns the panic value, the item and the worker's stack.
func (p *unitPanic) Error() string {
	return fmt.Sprintf("pipeline: work item %d panicked: %v\n\n%s", p.item, p.value, p.stack)
}

// Unwrap returns the panic value when it is an error.
func (p *unitPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// parallel calls fn(w, i) for every i in [0, n) on at most p.workers
// goroutines it starts and waits for, each with its own yield timer. Items
// are handed out in order. When ctx ends no new item starts and parallel
// returns ctx.Err(). A panic or runtime.Goexit in fn stops the hand-out,
// and once every goroutine has returned it is raised again here as a
// *unitPanic, whatever the worker count, so no goroutine outlives the
// call.
func (p *Pipeline) parallel(ctx context.Context, n int, fn func(w *worker, i int)) error {
	if n <= 0 {
		return ctx.Err()
	}
	var (
		next  atomic.Int64
		stop  atomic.Bool
		mu    sync.Mutex
		first *unitPanic
		wg    sync.WaitGroup
	)
	record := func(u *unitPanic) {
		stop.Store(true)
		mu.Lock()
		if first == nil {
			first = u
		}
		mu.Unlock()
	}
	for range min(p.workers, n) {
		wg.Go(func() {
			w := p.newWorker()
			for !stop.Load() && ctx.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= n || !runUnit(w, i, fn, record) {
					return
				}
				w.yield()
			}
		})
	}
	wg.Wait()
	if first != nil {
		panic(first)
	}
	return ctx.Err()
}

// runUnit runs item i and reports whether it returned normally. A panic is
// recovered and recorded; a runtime.Goexit is recorded by the deferred
// call, and the goroutine then ends.
func runUnit(w *worker, i int, fn func(w *worker, i int), record func(*unitPanic)) (ok bool) {
	done := false
	defer func() {
		if done {
			return
		}
		u := &unitPanic{value: recover(), item: i, stack: debug.Stack()}
		if u.value == nil {
			u.value = errGoexit
		}
		record(u)
	}()
	fn(w, i)
	done = true
	return true
}
