// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"context"
	"errors"
	"os"
	"runtime"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Tests for spec 10 req 21: SignalContext, with injected os/signal hooks.

// fakeSignals records the hook calls and delivers signals by hand.
type fakeSignals struct {
	mu      sync.Mutex
	ch      chan<- os.Signal
	watched []os.Signal
	stopped bool
	resets  [][]os.Signal
}

func (f *fakeSignals) hooks() signalHooks {
	return signalHooks{
		notify: func(c chan<- os.Signal, sig ...os.Signal) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.ch, f.watched = c, sig
		},
		stop: func(c chan<- os.Signal) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if c == f.ch {
				f.stopped = true
			}
		},
		reset: func(sig ...os.Signal) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.resets = append(f.resets, sig)
		},
	}
}

func (f *fakeSignals) send(s os.Signal) {
	f.mu.Lock()
	ch := f.ch
	f.mu.Unlock()
	ch <- s
}

func (f *fakeSignals) resetCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resets)
}

func waitDone(ctx context.Context, t *testing.T) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context not canceled")
	}
}

func TestSignalContextSIGINTReq21(t *testing.T) {
	var fs fakeSignals
	ctx, stop := signalContext(context.Background(), fs.hooks())
	defer stop()
	if !slices.Equal(fs.watched, []os.Signal{os.Interrupt, syscall.SIGTERM}) {
		t.Fatalf("watched %v, want SIGINT and SIGTERM", fs.watched)
	}
	if cap(fs.ch) != 1 {
		t.Errorf("channel capacity %d, want 1", cap(fs.ch))
	}
	fs.send(os.Interrupt)
	waitDone(ctx, t)
	in, ok := errors.AsType[Interrupted](context.Cause(ctx))
	if !ok || in.Signal != os.Interrupt {
		t.Fatalf("cause = %v, want Interrupted{SIGINT}", context.Cause(ctx))
	}
	if Code(ctx, ctx.Err()) != ExitInterrupted {
		t.Errorf("Code = %d, want 130", Code(ctx, ctx.Err()))
	}
	// The second SIGINT restores the default handling.
	fs.send(os.Interrupt)
	deadline := time.Now().Add(5 * time.Second)
	for fs.resetCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	fs.mu.Lock()
	resets := slices.Clone(fs.resets)
	fs.mu.Unlock()
	if len(resets) != 1 || !slices.Equal(resets[0], []os.Signal{os.Interrupt}) {
		t.Fatalf("resets = %v, want one reset of SIGINT", resets)
	}
}

func TestSignalContextSIGTERMReq21(t *testing.T) {
	var fs fakeSignals
	ctx, stop := signalContext(context.Background(), fs.hooks())
	fs.send(syscall.SIGTERM)
	waitDone(ctx, t)
	fs.send(os.Interrupt) // a later signal keeps the first cause
	stop()
	in, ok := errors.AsType[Interrupted](context.Cause(ctx))
	if !ok || in.Signal != syscall.SIGTERM {
		t.Fatalf("cause = %v, want Interrupted{SIGTERM}", context.Cause(ctx))
	}
	if fs.resetCount() != 0 {
		t.Errorf("SIGTERM then one SIGINT reset the handling")
	}
}

func TestSignalContextStopReq21(t *testing.T) {
	before := runtime.NumGoroutine()
	var fs fakeSignals
	ctx, stop := signalContext(context.Background(), fs.hooks())
	stop()
	stop() // idempotent
	if !fs.stopped {
		t.Error("stop did not unregister the channel")
	}
	waitDone(ctx, t)
	if _, ok := errors.AsType[Interrupted](context.Cause(ctx)); ok {
		t.Errorf("stop set cause %v", context.Cause(ctx))
	}
	if Code(ctx, nil) != ExitOK {
		t.Errorf("Code after stop = %d, want 0", Code(ctx, nil))
	}
	// The goroutine has exited once stop returns.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Errorf("goroutines %d after stop, %d before", n, before)
	}
}

func TestSignalContextParent(t *testing.T) {
	var fs fakeSignals
	parent, cancel := context.WithCancel(context.Background())
	ctx, stop := signalContext(parent, fs.hooks())
	defer stop()
	cancel()
	waitDone(ctx, t)
	if Code(ctx, ctx.Err()) != ExitNoResult {
		t.Errorf("parent cancel is not an interruption")
	}
}

func TestInterruptedError(t *testing.T) {
	cases := map[string]os.Signal{
		"interrupted by SIGINT":                     os.Interrupt,
		"interrupted by SIGTERM":                    syscall.SIGTERM,
		"interrupted by signal":                     nil,
		"interrupted by " + syscall.SIGHUP.String(): syscall.SIGHUP,
	}
	for want, s := range cases {
		if got := (Interrupted{Signal: s}).Error(); got != want {
			t.Errorf("Interrupted{%v}.Error() = %q, want %q", s, got, want)
		}
	}
}
