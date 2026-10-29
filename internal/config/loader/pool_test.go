// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// fakeClock advances by step on every reading; safe for concurrent use.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	step  time.Duration
	reads int
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	c.now = c.now.Add(c.step)
	return c.now
}

// TestWorkerYield covers the yield timer: the first call starts the
// interval, and a call yields once YieldAfter has passed since the last
// yield; without a clock it never reads one.
func TestWorkerYield(t *testing.T) {
	c := &fakeClock{step: 40 * time.Microsecond}
	w := &worker{now: c.Now, after: 100 * time.Microsecond}
	for range 10 {
		w.yield()
	}
	// Readings 1 (start), then every third call passes 100 µs.
	if w.yields != 3 {
		t.Errorf("yields = %d, want 3", w.yields)
	}
	idle := &worker{after: time.Microsecond}
	idle.yield()
	if idle.yields != 0 || idle.started {
		t.Errorf("a worker without a clock yielded: %+v", idle)
	}
}

// TestLoadYields covers the workers' yield timer in a load: with a clock
// every stage reads it, between items and inside them; without one, none
// does.
func TestLoadYields(t *testing.T) {
	m := overlayBundle()
	var big []string
	for i := range 40 {
		big = append(big, upstream("u"+strconv.Itoa(i)))
	}
	m["many.yaml"] = &fstest.MapFile{Data: []byte(docs(big...))}
	c := &fakeClock{step: time.Millisecond}
	r := loadEnv(t, newLoader(t, Options{Now: c.Now, Workers: 3}), m, envs, "prod")
	wantClean(t, "with a clock", r.Diagnostics)
	if c.reads < 40 {
		t.Errorf("the clock was read %d times", c.reads)
	}
}

// TestParallelPanic covers a panic in a work item: the hand-out stops and,
// once every goroutine has returned, it is raised on the calling
// goroutine as a *unitPanic wrapping the value, for every worker count.
func TestParallelPanic(t *testing.T) {
	boom := errors.New("boom")
	for _, workers := range []int{1, 4} {
		l := &Loader{workers: workers}
		var running atomic.Int64
		got := func() (p any) {
			defer func() { p = recover() }()
			_ = l.parallel(t.Context(), 100, func(_ *worker, i int) {
				running.Add(1)
				defer running.Add(-1)
				if i == 3 {
					panic(boom)
				}
			})
			return nil
		}()
		up, ok := got.(*unitPanic)
		if !ok || !errors.Is(up, boom) || up.item != 3 || len(up.stack) == 0 {
			t.Fatalf("workers %d: recovered %v, want a *unitPanic of item 3 wrapping boom", workers, got)
		}
		if n := running.Load(); n != 0 {
			t.Errorf("workers %d: %d items still running after the panic surfaced", workers, n)
		}
	}
}

// TestParallelGoexit covers runtime.Goexit in a work item, which ends the
// worker without a panic to recover: it surfaces as a *unitPanic wrapping
// errGoexit, so no item is ever left out silently.
func TestParallelGoexit(t *testing.T) {
	l := &Loader{workers: 2}
	got := func() (p any) {
		defer func() { p = recover() }()
		_ = l.parallel(t.Context(), 10, func(_ *worker, i int) {
			if i == 5 {
				runtime.Goexit()
			}
		})
		return nil
	}()
	if up, ok := got.(*unitPanic); !ok || !errors.Is(up, errGoexit) {
		t.Fatalf("recovered %v, want a *unitPanic wrapping errGoexit", got)
	}
}

// TestParallelCancel covers the end of ctx: no item starts after it, and
// parallel returns ctx.Err().
func TestParallelCancel(t *testing.T) {
	l := &Loader{workers: 1}
	ctx, cancel := context.WithCancel(t.Context())
	var ran []int
	err := l.parallel(ctx, 10, func(_ *worker, i int) {
		ran = append(ran, i)
		if i == 2 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || !slices.Equal(ran, []int{0, 1, 2}) {
		t.Errorf("err %v, ran %v", err, ran)
	}
}

// TestLoadCancel covers a canceled load: ctx's error and no result, at
// every entry point.
func TestLoadCancel(t *testing.T) {
	l := newLoader(t, Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	m := overlayBundle()
	if _, err := l.Load(ctx, Request{Source: Source{FS: m, Path: "."}}); !errors.Is(err, context.Canceled) {
		t.Errorf("Load: %v", err)
	}
	if _, err := l.Parse(ctx, Source{FS: m, Path: "."}); !errors.Is(err, context.Canceled) {
		t.Errorf("Parse: %v", err)
	}
	if _, err := l.ReadEnvironments(ctx, Source{FS: fstest.MapFS{"e": {Data: []byte(envs)}}, Path: "e"}); !errors.Is(err, context.Canceled) {
		t.Errorf("ReadEnvironments: %v", err)
	}
	b := newBundle(t, m)
	if _, err := b.Render(ctx, nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("Render: %v", err)
	}
}

// TestClockPanicSurfaces covers a panic in the injected clock during a
// load: it reaches the caller on the calling goroutine, never killing the
// process from a worker.
func TestClockPanicSurfaces(t *testing.T) {
	l := newLoader(t, Options{Workers: 4, Now: func() time.Time { panic("clock broken") }})
	got := func() (p any) {
		defer func() { p = recover() }()
		_ = load(t, l, overlayBundle())
		return nil
	}()
	if up, ok := got.(*unitPanic); !ok || up.value != "clock broken" {
		t.Fatalf("recovered %#v, want the clock's panic", got)
	}
}
