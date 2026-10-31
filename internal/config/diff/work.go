// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// workPanic is a panic raised by a unit of work, carried to the goroutine
// that called Compare and raised there again once every goroutine of the
// call has returned, so the worker count never changes how a failure
// surfaces. It wraps the panic value when that value is an error.
type workPanic struct {
	value any
	stack []byte
}

// Error returns the panic value and the stack of the goroutine that
// panicked.
func (p *workPanic) Error() string {
	return fmt.Sprintf("diff: panic: %v\n\n%s", p.value, p.stack)
}

// Unwrap returns the panic value when it is an error, else nil.
func (p *workPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// capture calls fn and returns the panic it raised, or its error.
func capture(fn func() error) (wp *workPanic, err error) {
	defer func() {
		if v := recover(); v != nil {
			wp = &workPanic{value: v, stack: debug.Stack()}
		}
	}()
	return nil, fn()
}

// run calls fn(worker, i) for every i in [0, n) on at most workers
// goroutines that it owns and waits for; worker is in [0, workers), so
// fn can keep per-worker scratch. ctx is checked before each unit. Units
// start in index order, and once one fails or panics no further unit
// starts. The lowest failing unit decides the outcome: its error is
// returned, or its panic raised again on the calling goroutine, so the
// outcome does not depend on scheduling or on the worker count.
func run(ctx context.Context, n, workers int, fn func(worker, i int) error) error {
	one := func(w, i int) (*workPanic, error) {
		return capture(func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return fn(w, i)
		})
	}
	if workers <= 1 || n <= 1 {
		for i := range n {
			wp, err := one(0, i)
			if wp != nil {
				panic(wp)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	errs := make([]error, n)
	panics := make([]*workPanic, n)
	var next atomic.Int64
	var failed atomic.Bool
	var wg sync.WaitGroup
	for w := range min(workers, n) {
		wg.Go(func() {
			for !failed.Load() {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				wp, err := one(w, i)
				if err != nil || wp != nil {
					errs[i], panics[i] = err, wp
					failed.Store(true)
					return
				}
			}
		})
	}
	wg.Wait()
	// The lowest failing unit decides, as when the units run one by one.
	for i := range n {
		if panics[i] != nil {
			panic(panics[i])
		}
		if errs[i] != nil {
			return errs[i]
		}
	}
	return nil
}
