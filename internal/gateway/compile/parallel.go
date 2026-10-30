// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
)

// yieldAfter is how long a build worker runs before it yields the
// processor, so an activation shares the cores with live traffic
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md, "compile
// workers yield every 100 µs (target)").
const yieldAfter = 100 * time.Microsecond

// errGoexit is the value of a unitPanic whose work called
// runtime.Goexit.
var errGoexit = errors.New("compile: runtime.Goexit ended a build worker")

// unitPanic is a panic, or a runtime.Goexit, inside one work item, raised
// again on the goroutine that called parallel. It wraps the panic value
// when that value is an error, so errors.Is and errors.As reach it.
type unitPanic struct {
	value any
	item  int
	stack []byte
}

// Error returns the panic value, the item and the worker's stack.
func (p *unitPanic) Error() string {
	return fmt.Sprintf("compile: work item %d panicked: %v\n\n%s", p.item, p.value, p.stack)
}

// Unwrap returns the panic value when it is an error.
func (p *unitPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// parallel calls fn(i) for every i in [0, n) on at most workers
// goroutines, which it starts and waits for; each yields the processor
// after every yieldAfter of work by now. Items are handed out in order.
// Once ctx ends no item starts, and parallel returns ctx.Err(). A panic
// or runtime.Goexit in fn stops the hand-out and, once every goroutine has
// returned, is raised again here as a *unitPanic, so no goroutine
// outlives the call.
func parallel(ctx context.Context, n, workers int, now func() time.Time, fn func(i int)) error {
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
	for range min(max(workers, 1), n) {
		wg.Go(func() {
			last := now()
			for !stop.Load() && ctx.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= n || !runUnit(i, fn, record) {
					return
				}
				if t := now(); t.Sub(last) >= yieldAfter {
					runtime.Gosched()
					last = t
				}
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
func runUnit(i int, fn func(i int), record func(*unitPanic)) (ok bool) {
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
	fn(i)
	done = true
	return true
}

// shared memoizes per-snapshot artifacts across the Builds of one compile
// (filter.Shared), such as the Consumer credential index: build runs once
// per key, and every caller gets its value and error.
type shared struct {
	mu sync.Mutex
	m  map[any]*sharedEntry
}

// sharedEntry is one memoized value.
type sharedEntry struct {
	once sync.Once
	v    any
	err  error
}

var _ filter.Shared = (*shared)(nil)

// Get returns the value of key, building it once. key must be comparable.
func (s *shared) Get(key any, build func() (any, error)) (any, error) {
	s.mu.Lock()
	if s.m == nil {
		s.m = map[any]*sharedEntry{}
	}
	e := s.m[key]
	if e == nil {
		e = &sharedEntry{}
		s.m[key] = e
	}
	s.mu.Unlock()
	e.once.Do(func() { e.v, e.err = build() })
	return e.v, e.err
}
