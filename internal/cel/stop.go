// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// stopPool hands out the comprehension stops of tracked programs holding a
// comprehension (ADR-0019): contexts that a timer cancels once the stop
// passes. A stop is reused, so an evaluation that finishes in time builds
// no context, timer or closure: a fresh context.WithTimeout costs a timer,
// a context and two closures per evaluation, and its cancellation hook
// would cost cel-go's child context a children map and a Done channel. One
// pool serves a Compiler and every Program it plans.
type stopPool struct {
	clock clock.Clock
	pool  sync.Pool // *stopper
	// ended is canceled when the pool is made: the stop of an evaluation
	// whose request context has ended or whose request deadline has
	// passed, which stops at the first interrupt check, as an expired
	// context.WithTimeout would.
	ended context.Context
}

// stopper is one reusable stop: a context that its timer cancels. Its
// timer is nil until the first use.
type stopper struct {
	ctx    context.Context
	cancel context.CancelFunc
	timer  clock.Timer
}

// newStopPool returns the stop pool of a Compiler, timing stops on clk.
func newStopPool(clk clock.Clock) *stopPool {
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	return &stopPool{clock: clk, ended: ended}
}

// start returns the stop context of one evaluation under ctx with the
// request deadline (expr.Vars.Deadline; the zero time is none): canceled
// after the smaller of d and the time left before deadline on the pool's
// clock, or already canceled when ctx has ended or no time is left, so an
// evaluation that starts at or after its deadline stops at its first
// interrupt check. ctx's own deadline is not read: it is a wall-clock
// instant, while the request deadline runs on the pool's clock, as every
// Node deadline does. The returned stopper, nil for the ended context,
// goes back through end. A ctx canceled after start no longer reaches the
// evaluation, which then still stops at d or the deadline.
func (s *stopPool) start(ctx context.Context, deadline time.Time, d time.Duration) (context.Context, *stopper) {
	if ctx.Err() != nil {
		return s.ended, nil
	}
	if !deadline.IsZero() {
		d = min(d, deadline.Sub(s.clock.Now()))
	}
	if d <= 0 {
		return s.ended, nil
	}
	st, _ := s.pool.Get().(*stopper)
	if st == nil {
		st = &stopper{}
		// The stopper's timer calls cancel when the stop fires; a stopper
		// that never fires is reused, and its context, derived from
		// Background, starts no goroutine and holds only memory.
		st.ctx, st.cancel = context.WithCancel(context.Background()) //nolint:gosec // G118: cancel runs from the stopper's timer, not here.
		// cel-go reads Done on every evaluation; make the channel once.
		st.ctx.Done()
	}
	if st.timer == nil {
		st.timer = s.clock.AfterFunc(d, st.cancel)
	} else {
		st.timer.Reset(d)
	}
	return st.ctx, st
}

// end releases st after its evaluation. A stopper whose timer had not
// fired goes back to the pool; one that fired, or is firing, has canceled
// its context for good and is dropped.
func (s *stopPool) end(st *stopper) {
	if st != nil && st.timer.Stop() {
		s.pool.Put(st)
	}
}
