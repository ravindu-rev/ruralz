// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// testPool returns a pool of at most workers workers (at least one)
// without a yield timer.
func testPool(workers int) *pool {
	return &pool{size: max(1, workers), newWorker: func() *worker { return &worker{} }}
}

// TestForEach covers the bounded worker pool: every index once, on one
// worker and on several, more workers than units, one goroutine per
// worker at a time, and cancellation before dispatch.
func TestForEach(t *testing.T) {
	for _, workers := range []int{0, 1, 3, 64} {
		for _, n := range []int{0, 1, 2, 17} {
			seen := make([]int32, n)
			p := testPool(workers)
			busy := map[*worker]*atomic.Bool{}
			for j := range max(1, workers) {
				busy[p.worker(j)] = &atomic.Bool{}
			}
			err := p.forEach(t.Context(), n, func(w *worker, i int) {
				b := busy[w]
				if b == nil || !b.CompareAndSwap(false, true) {
					t.Errorf("workers %d n %d: unit %d got a worker that is not the pool's or is in use", workers, n, i)
					return
				}
				defer b.Store(false)
				atomic.AddInt32(&seen[i], 1)
			})
			if err != nil {
				t.Fatalf("workers %d n %d: %v", workers, n, err)
			}
			for i, c := range seen {
				if c != 1 {
					t.Errorf("workers %d n %d: index %d called %d times", workers, n, i, c)
				}
			}
			if len(p.workers) > max(1, workers) {
				t.Errorf("workers %d n %d: %d workers created", workers, n, len(p.workers))
			}
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, workers := range []int{1, 4} {
		var calls atomic.Int32
		if err := testPool(workers).forEach(ctx, 10, func(*worker, int) { calls.Add(1) }); !errors.Is(err, context.Canceled) || calls.Load() != 0 {
			t.Errorf("workers %d: canceled before dispatch: err %v, %d calls", workers, err, calls.Load())
		}
	}
}

// TestForEachCancelDuringDispatch covers cancellation during dispatch
// without assuming that units 0 to 4 all ran before unit 5 canceled ctx,
// which forEach does not promise (a unit dequeued before the cancel may be
// drained). It asserts only bounds that hold under any scheduling: unit 5
// canceled ctx; at most 5 + workers units ran (units 0 to 4, unit 5, and
// one unit above 5 per other worker, each of which waits for the cancel so
// its worker cannot take another); at most 6 + 2*workers units were
// dispatched (the same, plus a full queue and the send in flight when ctx
// ended), which kills a dispatcher that ignores ctx; no unit runs after
// forEach returns.
func TestForEachCancelDuringDispatch(t *testing.T) {
	for _, workers := range []int{1, 2, 4} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			canceled := make(chan struct{})
			var calls, running, took atomic.Int32
			var ran5 atomic.Bool
			p := testPool(workers)
			p.took = func(int) { took.Add(1) }
			err := p.forEach(ctx, 1000, func(_ *worker, i int) {
				running.Add(1)
				defer running.Add(-1)
				calls.Add(1)
				switch {
				case i == 5:
					ran5.Store(true)
					cancel()
					close(canceled)
				case i > 5:
					<-canceled
				default:
				}
			})
			if !errors.Is(err, context.Canceled) {
				t.Errorf("err = %v, want context.Canceled", err)
			}
			if !ran5.Load() {
				t.Error("unit 5 did not run")
			}
			if c, most := int(calls.Load()), 5+workers; c > most {
				t.Errorf("%d calls, want at most %d: units started after the cancel", c, most)
			}
			if d, most := int(took.Load()), 6+2*workers; d > most {
				t.Errorf("%d units dispatched, want at most %d: dispatch went on after the cancel", d, most)
			}
			if r := running.Load(); r != 0 {
				t.Errorf("%d calls still running after forEach returned", r)
			}
			after := calls.Load()
			for range 1000 {
				runtime.Gosched()
			}
			if c := calls.Load(); c != after {
				t.Errorf("%d calls after forEach returned, %d when it returned", c, after)
			}
		})
	}
}

// errBoom is the panic value of the panic tests.
var errBoom = errors.New("boom")

// poolEvent is one hook call of a pool, in the order the pool's lock
// gives them.
type poolEvent struct {
	kind string
	unit int
}

// recordEvents sets hooks on p that log every took, admitted and stopped
// event. The pool calls them under the lock that orders admissions
// against the recording of a panic, so the log needs no lock of its own.
func recordEvents(p *pool) *[]poolEvent {
	var log []poolEvent
	p.took = func(i int) { log = append(log, poolEvent{"took", i}) }
	p.admitted = func(i int) { log = append(log, poolEvent{"admitted", i}) }
	p.stopped = func(i int) { log = append(log, poolEvent{"stopped", i}) }
	return &log
}

// TestForEachPanics checks that a panic in a pooled work unit reaches the
// caller's recover as the same panic inline does, and that no unit starts
// once a unit has panicked, whatever the timing. For workers 0, 1, 2, 4 and
// 64 and every kind of panic value, a panic in fn reaches the caller's
// recover as a *unitPanic that wraps the value (errors.Is, errors.As) or
// prints it, with the unit's stack, once every started unit has returned.
// The pool's hooks give one order of events whatever the scheduling: the
// panic of unit 3 is recorded once, no unit is admitted after it, and at
// most 2*workers units are taken from the queue after it (checkStopEvents),
// so dispatch ends; and the stack is captured only after the stop was
// recorded, which is the first step of the recovery. Units above 3 wait for
// the stop and unit 3 for every worker to be busy, which only makes queued
// units likely when it panics, so a pool that admits them anyway fails.
func TestForEachPanics(t *testing.T) {
	for _, workers := range []int{0, 1, 2, 4, 64} {
		for _, tc := range []struct {
			name  string
			value any
		}{
			{"error value", errBoom},
			{"string value", "plain boom"},
			{"runtime error", nil},
		} {
			t.Run(fmt.Sprintf("workers=%d/%s", workers, tc.name), func(t *testing.T) {
				const n = 1000
				p := testPool(workers)
				events := recordEvents(p)
				stopped := make(chan struct{})
				stop := p.stopped
				p.stopped = func(i int) {
					stop(i)
					close(stopped)
				}
				var took atomic.Int32
				logTook := p.took
				p.took = func(i int) {
					took.Add(1)
					logTook(i)
				}
				// The stack is captured after the stop is recorded.
				p.stack = func() []byte {
					select {
					case <-stopped:
					default:
						t.Error("the stack was captured before the stop was recorded")
					}
					return debug.Stack()
				}
				var running atomic.Int32
				runningAtRecover := int32(-1)
				v, err := func() (v any, err error) {
					defer func() {
						v = recover()
						runningAtRecover = running.Load()
					}()
					return nil, p.forEach(t.Context(), n, func(_ *worker, i int) {
						running.Add(1)
						defer running.Add(-1)
						switch {
						case i == 3:
							for k := 0; k < 1_000_000 && int(took.Load()) < 3+max(1, workers); k++ {
								runtime.Gosched()
							}
							for range 100 {
								runtime.Gosched()
							}
							if tc.value == nil {
								zero := i - 3
								_ = i / zero // integer divide by zero: a runtime.Error
							}
							panic(tc.value)
						case i > 3:
							<-stopped
						default:
						}
					})
				}()
				if err != nil {
					t.Fatalf("forEach returned %v instead of panicking", err)
				}
				up, ok := v.(*unitPanic)
				if !ok {
					t.Fatalf("recovered %T %v, want *unitPanic", v, v)
				}
				if up.unit != 3 {
					t.Errorf("unit %d, want 3", up.unit)
				}
				switch tc.value {
				case nil:
					var re runtime.Error
					if !errors.As(up, &re) {
						t.Errorf("%v does not wrap a runtime.Error", up)
					}
				case errBoom:
					if !errors.Is(up, errBoom) {
						t.Errorf("%v does not wrap errBoom", up)
					}
				default:
					if !strings.Contains(up.Error(), "plain boom") || up.Unwrap() != nil {
						t.Errorf("Error() = %q, Unwrap() = %v", up.Error(), up.Unwrap())
					}
				}
				if !strings.Contains(up.Error(), "TestForEachPanics") || !strings.Contains(up.Error(), "work unit 3 panicked") {
					t.Errorf("Error() lacks the unit's stack:\n%s", up.Error())
				}
				if runningAtRecover != 0 {
					t.Errorf("%d units still running when the panic reached the caller", runningAtRecover)
				}
				checkStopEvents(t, *events, 3, workers)
			})
		}
	}
}

// checkStopEvents asserts the pool's events around a recorded panic: one
// stop, of unit; no admission after it; at most 2*workers units taken
// after it (one each other worker received before the stop, a full
// queue, and the send in flight when it was recorded), the pool having at
// least one worker.
func checkStopEvents(t *testing.T, events []poolEvent, unit, workers int) {
	t.Helper()
	at := -1
	for i, e := range events {
		if e.kind == "stopped" {
			if at >= 0 {
				t.Errorf("a second stop recorded: %+v", e)
			}
			at = i
		}
	}
	if at < 0 || events[at].unit != unit {
		t.Fatalf("stop event at %d, want one for unit %d", at, unit)
	}
	tookAfter := 0
	for _, e := range events[at+1:] {
		switch e.kind {
		case "admitted":
			t.Errorf("unit %d admitted after the panic of unit %d was recorded", e.unit, unit)
		case "took":
			tookAfter++
		default:
		}
	}
	if most := 2 * max(1, workers); tookAfter > most {
		t.Errorf("%d units dispatched after the panic was recorded, want at most %d", tookAfter, 2*max(1, workers))
	}
}

// TestForEachFirstPanic covers the panic forEach raises when several
// units panic: the one whose stop was recorded first, the same unit the
// stopped hook names, whatever the scheduling.
func TestForEachFirstPanic(t *testing.T) {
	for _, workers := range []int{2, 4} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			p := testPool(workers)
			events := recordEvents(p)
			v := func() (v any) {
				defer func() { v = recover() }()
				_ = p.forEach(t.Context(), 100, func(_ *worker, i int) {
					if i == 1 || i == 2 {
						panic(fmt.Sprintf("unit %d", i))
					}
				})
				return nil
			}()
			up, ok := v.(*unitPanic)
			if !ok {
				t.Fatalf("recovered %T %v, want *unitPanic", v, v)
			}
			checkStopEvents(t, *events, up.unit, workers)
		})
	}
}

// panicOracle panics in every ReferencesRequestBody call.
type panicOracle struct{}

func (panicOracle) ReferencesRequestBody(string) (bool, error) { panic(errBoom) }

// TestResolvePanicRecoverable is TestForEachPanics through Resolve: a
// panicking Options.Body reaches the caller's recover with errBoom
// wrapped, on 1, 2 and 4 workers alike (the Node passes
// W = max(1, GOMAXPROCS/2), at least 2 on a four-core host).
func TestResolvePanicRecoverable(t *testing.T) {
	b := newBuilder()
	b.policy("a1", v1alpha1.PolicyTypeAuthzCEL, authzCfg("x"))
	b.policy("a2", v1alpha1.PolicyTypeAuthzCEL, authzCfg("y"))
	b.gateway("a1")
	b.route("r", []string{"a2"}, nil, nil)
	bundle := b.bundle()
	for _, workers := range []int{1, 2, 4} {
		v := func() (v any) {
			defer func() { v = recover() }()
			_, _, _ = mustNew(t, nil, Options{Body: panicOracle{}}).Run(t.Context(), bundle, nil, workers)
			return nil
		}()
		err, ok := v.(error)
		if !ok || !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "ReferencesRequestBody") {
			t.Errorf("workers %d: recovered %v, want an error wrapping errBoom with the oracle's stack", workers, v)
		}
	}
}

// errCountingCtx counts Err calls, which tells a test how far forEach's
// dispatch has gone: one call per unit before it is queued, and one per
// unit a worker takes.
type errCountingCtx struct {
	context.Context
	errs atomic.Int32
}

func (c *errCountingCtx) Err() error {
	c.errs.Add(1)
	return c.Context.Err()
}

// TestForEachCancelQueued covers the cancellation contract of forEach that
// Resolve documents: no unit starts once ctx has ended. Two workers hold
// units 0 and 1 and units 2 and 3 wait in the queue when ctx ends: the
// queued units never run; the dispatcher, waiting to queue unit 4 of 1000,
// stops there instead of dispatching the rest (Err is called a bounded
// number of times, not once per unit); and when every unit was already
// queued (n 4) forEach still returns ctx.Err(), because a unit did not run.
func TestForEachCancelQueued(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
	}{
		{"dispatcher waits on a full queue", 1000},
		{"every unit queued", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &errCountingCtx{Context: parent}
			gate := make(chan struct{})
			var calls atomic.Int32
			done := make(chan error, 1)
			go func() {
				done <- testPool(2).forEach(ctx, tc.n, func(*worker, int) {
					calls.Add(1)
					<-gate
				})
			}()
			for calls.Load() < 2 {
				runtime.Gosched()
			}
			// Both workers are held: let the dispatcher queue what fits.
			for range 10000 {
				runtime.Gosched()
			}
			cancel()
			close(gate)
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Errorf("forEach = %v, want context.Canceled", err)
			}
			if c := calls.Load(); c != 2 {
				t.Errorf("%d calls, want 2: a queued unit ran after ctx ended", c)
			}
			// About a dozen: the dispatcher's check per queued unit, the
			// workers' per unit taken, never one per unit of the 1000.
			if e := ctx.errs.Load(); e > 25 {
				t.Errorf("%d Err calls: dispatch went on after ctx ended", e)
			}
		})
	}
}
