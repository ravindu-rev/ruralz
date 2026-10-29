// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

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

// worker is the yield timer of one goroutine of a load. The stages call
// yield every 256 units of their work and the pool between two items, so
// a load shares the processor with live traffic during activation. A
// worker belongs to one goroutine.
type worker struct {
	now     func() time.Time
	after   time.Duration
	started bool
	last    time.Time
	// yields counts the yields, for tests.
	yields int
}

// newWorker returns a yield timer; it reads no clock until its first yield.
func (l *Loader) newWorker() *worker { return &worker{now: l.now, after: l.yieldAfter} }

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
var errGoexit = errors.New("loader: runtime.Goexit ended a worker")

// unitPanic is a panic, or a runtime.Goexit, inside one work item, raised
// again on the goroutine that called the load. It wraps the panic value
// when that value is an error.
type unitPanic struct {
	value any
	item  int
	stack []byte
}

// Error returns the panic value, the item and the worker's stack.
func (p *unitPanic) Error() string {
	return fmt.Sprintf("loader: work item %d panicked: %v\n\n%s", p.item, p.value, p.stack)
}

// Unwrap returns the panic value when it is an error.
func (p *unitPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// parallel calls fn(w, i) for every i in [0, n) on at most l.workers
// goroutines it starts and waits for, each with its own yield timer. Items
// are handed out in order. When ctx ends no new item starts and parallel
// returns ctx.Err(). A panic or runtime.Goexit in fn stops the hand-out,
// and once every goroutine has returned it is raised again here as a
// *unitPanic, whatever the worker count.
func (l *Loader) parallel(ctx context.Context, n int, fn func(w *worker, i int)) error {
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
	record := func(p *unitPanic) {
		stop.Store(true)
		mu.Lock()
		if first == nil {
			first = p
		}
		mu.Unlock()
	}
	for range min(l.workers, n) {
		wg.Go(func() {
			w := l.newWorker()
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

// runUnit runs item i and reports whether it returned normally. A panic
// is recovered and recorded; a runtime.Goexit is recorded by the deferred
// call, and the goroutine then ends.
func runUnit(w *worker, i int, fn func(w *worker, i int), record func(*unitPanic)) (ok bool) {
	done := false
	defer func() {
		if done {
			return
		}
		p := &unitPanic{value: recover(), item: i, stack: debug.Stack()}
		if p.value == nil {
			p.value = errGoexit
		}
		record(p)
	}()
	fn(w, i)
	done = true
	return true
}
