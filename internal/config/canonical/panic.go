// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// workPanic is a panic raised while resources were checked or encoded,
// carried to the goroutine that called Decode, Verify, ReadDump, Encode
// or EncodeResources and raised there again once every goroutine of the
// call has returned. It is raised the same way whether the work ran on
// the calling goroutine or on a worker, so the worker count never changes
// how a failure surfaces, a recovering caller sees it, and no goroutine
// of the call is left behind. It is an error that wraps the panic value
// when that value is an error, so errors.Is and errors.As reach it, and
// its text holds the value and the stack of the goroutine that panicked.
type workPanic struct {
	// value is what the work passed to panic.
	value any
	// stack is the panicking goroutine's stack.
	stack []byte
}

// Error returns the panic value and its stack.
func (p *workPanic) Error() string {
	return fmt.Sprintf("canonical: panic: %v\n\n%s", p.value, p.stack)
}

// Unwrap returns the panic value when it is an error, else nil.
func (p *workPanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// capture calls fn and returns the panic it raised, or nil.
func capture(fn func()) (wp *workPanic) {
	defer func() {
		if v := recover(); v != nil {
			wp = &workPanic{value: v, stack: debug.Stack()}
		}
	}()
	fn()
	return nil
}

// panics records the first panic of the work of one call, from any of
// its goroutines.
type panics struct {
	// stop is set by the first panic, so no further work starts.
	stop  atomic.Bool
	mu    sync.Mutex
	first *workPanic
}

// run calls fn and records its panic, if any.
func (ps *panics) run(fn func()) {
	if wp := capture(fn); wp != nil {
		ps.stop.Store(true)
		ps.mu.Lock()
		if ps.first == nil {
			ps.first = wp
		}
		ps.mu.Unlock()
	}
}

// raise panics with the first recorded panic, if any. The call's
// goroutines must all have returned.
func (ps *panics) raise() {
	if ps.first != nil {
		panic(ps.first)
	}
}
