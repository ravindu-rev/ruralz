// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// errInjected is the value an injected panic raises.
var errInjected = errors.New("injected")

// goroutineID returns the id of the calling goroutine.
func goroutineID() string {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	return string(bytes.Fields(b)[1])
}

// injecting is a context whose Err panics with errInjected at its at-th
// call from a function whose name holds site, made from the goroutine
// mine when onMine, from any other when not. It injects a panic into one
// place of a decode or an encode without a hook in the code.
type injecting struct {
	context.Context
	site   string
	mine   string
	onMine bool
	at     int64
	calls  atomic.Int64
	fired  atomic.Bool
}

func (c *injecting) Err() error {
	pc := make([]uintptr, 1)
	if runtime.Callers(2, pc) == 0 {
		return nil
	}
	frame, _ := runtime.CallersFrames(pc).Next()
	if !strings.Contains(frame.Function, c.site) || (goroutineID() == c.mine) != c.onMine {
		return nil
	}
	if c.calls.Add(1) == c.at {
		c.fired.Store(true)
		panic(errInjected)
	}
	return nil
}

// recovered calls fn and returns what it panicked with.
func recovered(fn func()) (v any) {
	defer func() { v = recover() }()
	fn()
	return nil
}

// settles waits until no more goroutines exist than base, as the
// goroutines of a call that returned finish exiting.
func settles(t *testing.T, base int) {
	t.Helper()
	for range 5000 {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("%d goroutines remain, %d before the call", runtime.NumGoroutine(), base)
}

// panicCase is one place a panic is injected and how it must surface:
// wrapped in a *workPanic, or as raised.
type panicCase struct {
	name    string
	site    string
	onMine  bool
	at      int64
	workers []int
	wrapped bool
}

// checkSurfaced fails unless v is the injected panic as tc wants it.
func checkSurfaced(t *testing.T, tc panicCase, w int, v any) {
	t.Helper()
	if !tc.wrapped {
		err, ok := v.(error)
		if _, wrapped := v.(*workPanic); !ok || wrapped || !errors.Is(err, errInjected) {
			t.Errorf("%s, workers %d: recovered %v, want the injected panic as raised", tc.name, w, v)
		}
		return
	}
	wp, ok := v.(*workPanic)
	if !ok {
		t.Errorf("%s, workers %d: recovered %T %v, want a *workPanic", tc.name, w, v, v)
		return
	}
	if !errors.Is(wp, errInjected) || !strings.Contains(wp.Error(), "canonical: panic: injected") || len(wp.stack) == 0 {
		t.Errorf("%s, workers %d: workPanic = %v", tc.name, w, wp)
	}
}

// TestDecodePanicSurfaces: a panic in a decode reaches the caller, the
// same way whatever the worker count, and leaves no goroutine behind
// (section 0: every goroutine has an owner and a stop path; WP-39
// review: a worker's panic ended the process, and one on the calling
// goroutine after the workers started left them blocked). A panic in a
// check, on a worker or inline, is raised again as a *workPanic once
// every goroutine has returned; one in the parser is raised as it is,
// after the workers were stopped and waited for. The decoder works after.
func TestDecodePanicSurfaces(t *testing.T) {
	content := routes(300)
	for _, tc := range []panicCase{
		{"a check on a worker", "(*checker).run", false, 40, []int{4}, true},
		{"a check on the calling goroutine", "(*checker).run", true, 1, []int{1, 4}, true},
		// With four workers the calling goroutine checks at least the
		// last 12 resources, after every full chunk was handed over.
		{"a check on the calling goroutine late", "(*checker).run", true, 100, []int{1}, true},
		{"a check on the calling goroutine after the workers started", "(*checker).run", true, 5, []int{4}, true},
		{"the parser after the workers started", "(*Decoder).stream", true, 200, []int{1, 4}, false},
	} {
		for _, w := range tc.workers {
			dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: w})
			base := runtime.NumGoroutine()
			ctx := &injecting{Context: context.Background(), site: tc.site, mine: goroutineID(), onMine: tc.onMine, at: tc.at}
			v := recovered(func() { _, _ = dec.Decode(ctx, content, Source{}) })
			if !ctx.fired.Load() {
				t.Fatalf("%s, workers %d: the panic was not injected", tc.name, w)
			}
			checkSurfaced(t, tc, w, v)
			settles(t, base)
			if rs, err := dec.Decode(context.Background(), content, Source{}); err != nil || len(rs) != 300 {
				t.Errorf("%s, workers %d: the decode after: %d resources, %v", tc.name, w, len(rs), err)
			}
		}
	}
}

// TestEncodePanicSurfaces: a panic while encoding resources, inside the
// work of a chunk or between chunks, on a worker or inline, reaches the
// caller as a *workPanic once every goroutine has returned, with one
// worker and with four (WP-39 review: a worker's panic ended the
// process), and the encoder works after.
func TestEncodePanicSurfaces(t *testing.T) {
	b := largeBundle(t, 50)
	enc := NewEncoder(hubIndex(t))
	want, err := enc.Encode(context.Background(), b, Options{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []panicCase{
		{"a resource on a worker", "(*Encoder).encode.func", false, 30, []int{4}, true},
		{"a resource on the calling goroutine", "(*Encoder).encode.func", true, 30, []int{1}, true},
		{"between chunks on a worker", "canonical.parallel", false, 3, []int{4}, true},
		{"between chunks on the calling goroutine", "canonical.parallel", true, 1, []int{1}, true},
	} {
		for _, w := range tc.workers {
			base := runtime.NumGoroutine()
			ctx := &injecting{Context: context.Background(), site: tc.site, mine: goroutineID(), onMine: tc.onMine, at: tc.at}
			v := recovered(func() { _, _ = enc.Encode(ctx, b, Options{Workers: w}) })
			if !ctx.fired.Load() {
				t.Fatalf("%s, workers %d: the panic was not injected", tc.name, w)
			}
			checkSurfaced(t, tc, w, v)
			settles(t, base)
			got, err := enc.Encode(context.Background(), b, Options{Workers: w})
			if err != nil {
				t.Fatal(err)
			}
			sameBytes(t, tc.name+" then encode", got.Content, want.Content)
		}
	}
}
