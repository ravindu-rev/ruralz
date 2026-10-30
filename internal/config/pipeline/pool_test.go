// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestParallelRunsEveryItem covers the pool: every item runs once, on at
// most the configured workers.
func TestParallelRunsEveryItem(t *testing.T) {
	for _, workers := range []int{1, 3, 16} {
		p := &Pipeline{workers: workers}
		var (
			mu   sync.Mutex
			seen = map[int]int{}
			busy atomic.Int32
			peak atomic.Int32
		)
		err := p.parallel(t.Context(), 50, func(_ *worker, i int) {
			n := busy.Add(1)
			for {
				m := peak.Load()
				if n <= m || peak.CompareAndSwap(m, n) {
					break
				}
			}
			mu.Lock()
			seen[i]++
			mu.Unlock()
			busy.Add(-1)
		})
		if err != nil {
			t.Fatalf("parallel: %v", err)
		}
		for i := range 50 {
			if seen[i] != 1 {
				t.Errorf("%d workers: item %d ran %d times", workers, i, seen[i])
			}
		}
		if int(peak.Load()) > workers {
			t.Errorf("%d workers: %d items ran at once", workers, peak.Load())
		}
	}
}

// errItem is the panic value of TestParallelRaisesPanic.
var errItem = errors.New("item failed")

// TestParallelRaisesPanic covers a panic in an item and a runtime.Goexit:
// each surfaces on the calling goroutine as a *unitPanic, the panic value
// wrapped, once every worker stopped.
func TestParallelRaisesPanic(t *testing.T) {
	for name, fail := range map[string]func(){
		"panic":  func() { panic(errItem) },
		"Goexit": runtime.Goexit,
	} {
		t.Run(name, func(t *testing.T) {
			p := &Pipeline{workers: 4}
			var running atomic.Int32
			defer func() {
				u, ok := recover().(*unitPanic)
				if !ok {
					t.Fatalf("recovered %T, want *unitPanic", u)
				}
				if name == "panic" && (!errors.Is(u, errItem) || u.item != 7) {
					t.Errorf("unitPanic %v, want item 7 wrapping errItem", u)
				}
				if name == "Goexit" && !errors.Is(u, errGoexit) {
					t.Errorf("unitPanic %v, want errGoexit", u)
				}
				if n := running.Load(); n != 0 {
					t.Errorf("%d items still running after the panic surfaced", n)
				}
			}()
			_ = p.parallel(t.Context(), 20, func(_ *worker, i int) {
				running.Add(1)
				defer running.Add(-1)
				if i == 7 {
					fail()
				}
			})
			t.Error("parallel returned")
		})
	}
}

// TestParallelCanceled covers the end of ctx: no item starts after it, and
// parallel returns ctx.Err().
func TestParallelCanceled(t *testing.T) {
	p := &Pipeline{workers: 1}
	ctx, cancel := context.WithCancel(t.Context())
	ran := 0
	err := p.parallel(ctx, 10, func(_ *worker, i int) {
		ran++
		if i == 2 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || ran != 3 {
		t.Errorf("parallel = %v after %d items, want context.Canceled after 3", err, ran)
	}
	if err := p.parallel(ctx, 0, func(*worker, int) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("parallel of no item = %v, want context.Canceled", err)
	}
}

// TestWorkerYields covers the yield timer: no clock never yields, the
// first call starts the interval, and a call once YieldAfter has passed
// yields and starts the next.
func TestWorkerYields(t *testing.T) {
	now := time.Unix(0, 0)
	w := &worker{now: func() time.Time { return now }, after: 100 * time.Microsecond}
	w.yield()
	now = now.Add(50 * time.Microsecond)
	w.yield()
	if w.yields != 0 {
		t.Errorf("%d yields before YieldAfter passed", w.yields)
	}
	now = now.Add(60 * time.Microsecond)
	w.yield()
	if w.yields != 1 {
		t.Errorf("%d yields after YieldAfter passed, want 1", w.yields)
	}
	idle := &worker{}
	idle.yield()
	if idle.yields != 0 || idle.started {
		t.Error("a worker without a clock yielded")
	}
}
