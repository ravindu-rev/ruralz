// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// TestParallelRunsEveryItem covers parallel: every item runs exactly once
// on at most the worker count of goroutines at a time, and each worker
// reads the clock after every item, which decides its yields.
func TestParallelRunsEveryItem(t *testing.T) {
	const n, workers = 200, 3
	var (
		runs        [n]atomic.Int32
		active, top atomic.Int32
		reads       atomic.Int32
	)
	now := func() time.Time { reads.Add(1); return t0 }
	err := parallel(t.Context(), n, workers, now, func(i int) {
		cur := active.Add(1)
		for {
			old := top.Load()
			if cur <= old || top.CompareAndSwap(old, cur) {
				break
			}
		}
		runtime.Gosched()
		runs[i].Add(1)
		active.Add(-1)
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range runs {
		if c := runs[i].Load(); c != 1 {
			t.Errorf("item %d ran %d times, want 1", i, c)
		}
	}
	if m := top.Load(); m > workers {
		t.Errorf("%d items ran at once, want at most %d", m, workers)
	}
	if r := reads.Load(); r < n {
		t.Errorf("the clock was read %d times, want at least once per item, %d", r, n)
	}
	if err := parallel(t.Context(), 0, workers, now, func(int) { t.Error("an item of none ran") }); err != nil {
		t.Errorf("parallel over no items = %v", err)
	}
	if err := parallel(t.Context(), 2, 0, now, func(int) {}); err != nil {
		t.Errorf("parallel with zero workers = %v, want one worker", err)
	}
}

// TestParallelContext covers the context: once it ends no item starts and
// parallel returns its error; an ended context runs nothing.
func TestParallelContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var ran atomic.Int32
	err := parallel(ctx, 10, 1, time.Now, func(i int) {
		ran.Add(1)
		if i == 2 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || ran.Load() != 3 {
		t.Errorf("parallel = %v after %d items, want context.Canceled after 3", err, ran.Load())
	}
	ran.Store(0)
	if err := parallel(ctx, 10, 4, time.Now, func(int) { ran.Add(1) }); !errors.Is(err, context.Canceled) || ran.Load() != 0 {
		t.Errorf("parallel on an ended context = %v after %d items, want context.Canceled before any", err, ran.Load())
	}
	if err := parallel(ctx, 0, 4, time.Now, func(int) {}); !errors.Is(err, context.Canceled) {
		t.Errorf("parallel over no items on an ended context = %v, want context.Canceled", err)
	}
}

// TestParallelPanic covers a panicking item: the hand-out stops, every
// goroutine returns, and the panic is raised again on the caller's
// goroutine as a *unitPanic naming the item, wrapping an error value; a
// runtime.Goexit is raised the same way.
func TestParallelPanic(t *testing.T) {
	boom := errcode.Errorf("RZ-CFG-005", "boom")
	var ran atomic.Int32
	v := catch(func() {
		_ = parallel(t.Context(), 10, 1, time.Now, func(i int) {
			if i == 4 {
				panic(boom)
			}
			ran.Add(1)
		})
	})
	up, ok := v.(*unitPanic)
	if !ok || up.item != 4 || !errors.Is(up, boom) || !strings.Contains(up.Error(), "work item 4 panicked: RZ-CFG-005: boom") {
		t.Fatalf("recovered %v, want a *unitPanic of item 4 wrapping boom", v)
	}
	if n := ran.Load(); n != 4 {
		t.Errorf("%d items ran, want the 4 before the panic", n)
	}
	if v := catch(func() { _ = parallel(t.Context(), 3, 2, time.Now, func(int) { panic("text") }) }); v == nil || errors.Unwrap(v.(*unitPanic)) != nil {
		t.Errorf("recovered %v, want a *unitPanic wrapping no error", v)
	}
	v = catch(func() {
		_ = parallel(t.Context(), 3, 1, time.Now, func(i int) {
			if i == 1 {
				runtime.Goexit()
			}
		})
	})
	if up, ok := v.(*unitPanic); !ok || !errors.Is(up, errGoexit) || up.item != 1 {
		t.Errorf("recovered %v, want a *unitPanic of item 1 wrapping errGoexit", v)
	}
}

// catch runs fn and returns what it panicked with, or nil.
func catch(fn func()) (v any) {
	defer func() { v = recover() }()
	fn()
	return nil
}

// TestSharedMemo covers the Shared memo of one compile: build runs once
// per key however many Builds ask at once, every caller gets its value
// and error, and keys build apart.
func TestSharedMemo(t *testing.T) {
	var s shared
	var builds atomic.Int32
	fail := errors.New("index failed")
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			v, err := s.Get("index", func() (any, error) { builds.Add(1); return 42, nil })
			if v != 42 || err != nil {
				t.Errorf("Get = %v, %v; want 42", v, err)
			}
			if _, err := s.Get("broken", func() (any, error) { builds.Add(1); return nil, fail }); !errors.Is(err, fail) {
				t.Errorf("Get of a failing build = %v, want its error", err)
			}
		})
	}
	wg.Wait()
	if n := builds.Load(); n != 2 {
		t.Errorf("builds ran %d times, want once per key", n)
	}
}

// TestCauseText covers the message of a finding: the cause of the
// outermost errcode.Error, invalid UTF-8 replaced, cut to 512 bytes on a
// code point boundary with "..." appended.
func TestCauseText(t *testing.T) {
	long := strings.Repeat("x", 511) + "é" + strings.Repeat("y", 100)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"coded", errcode.Errorf("RZ-CFG-005", "the header is refused"), "the header is refused"},
		{"wrapped", errors.Join(errors.New("outer"), errcode.Errorf("RZ-CFG-005", "inner")), "inner"},
		{"plain", errors.New("plain"), "plain"},
		{"invalid UTF-8", errcode.Errorf("RZ-CFG-005", "bad \xff byte"), "bad � byte"},
		{"long", errcode.Errorf("RZ-CFG-005", "%s", long), strings.Repeat("x", 511) + "..."},
		{"exactly the bound", errcode.Errorf("RZ-CFG-005", "%s", strings.Repeat("z", 512)), strings.Repeat("z", 512)},
	} {
		if got := causeText(tc.err); got != tc.want {
			t.Errorf("%s: causeText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestFindingsWithoutFileTable covers locations without the run's file
// table: a finding still names the resource's own source file at the
// field's position.
func TestFindingsWithoutFileTable(t *testing.T) {
	h := newHarness(t)
	h.behave("headers", "hdr", behavior{err: errcode.Errorf("RZ-CFG-005", "refused")})
	res := h.validate(gateway(""), upstreamDoc("up", "127.0.0.1:9000", ""), routeDoc("r", "/", "up", attach("hdr")), policyDoc("hdr", "headers", headersConfig, ""))
	withTable := h.rejects(res)
	c, ds, err := h.comp.Compile(t.Context(), Input{Validated: res.Validated})
	if c != nil || err != nil || len(ds) != 1 {
		t.Fatalf("Compile = %v, %v, %s", c, err, text(ds))
	}
	if got := lines(ds); got[0] != withTable[0] {
		t.Errorf("finding without the file table = %q, want %q", got[0], withTable[0])
	}
}
