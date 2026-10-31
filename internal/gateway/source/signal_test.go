// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

// countingReloader counts Reload calls.
type countingReloader struct{ n atomic.Int32 }

func (c *countingReloader) Reload() { c.n.Add(1) }

// fakeSignals records what notifySIGHUP registers and stops.
type fakeSignals struct {
	mu         sync.Mutex
	ch         chan<- os.Signal
	registered []os.Signal
	stops      int
}

func (f *fakeSignals) hooks() signalHooks {
	return signalHooks{
		notify: func(c chan<- os.Signal, sig ...os.Signal) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.ch, f.registered = c, append(f.registered, sig...)
		},
		stop: func(c chan<- os.Signal) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if c == f.ch {
				f.stops++
			}
		},
	}
}

// TestNotifySIGHUPForwardsToReload: SIGHUP, and nothing else, is
// registered and each one reaches Reload; stop unregisters once and is
// idempotent.
func TestNotifySIGHUPForwardsToReload(t *testing.T) {
	f := &fakeSignals{}
	r := &countingReloader{}
	stop := notifySIGHUP(r, f.hooks())
	f.mu.Lock()
	registered, ch := slices.Clone(f.registered), f.ch
	f.mu.Unlock()
	if !slices.Equal(registered, []os.Signal{syscall.SIGHUP}) {
		t.Fatalf("registered %v, want SIGHUP only", registered)
	}
	ch <- syscall.SIGHUP
	waitFor(t, "the first Reload", func() bool { return r.n.Load() == 1 })
	ch <- syscall.SIGHUP
	waitFor(t, "the second Reload", func() bool { return r.n.Load() == 2 })
	stop()
	stop()
	if f.stops != 1 {
		t.Fatalf("unregistered %d times, want 1", f.stops)
	}
}

// TestSIGHUPReloadsTheWatcher: a Watcher's Reload is the forwarded
// action, so SIGHUP makes Run load at once.
func TestSIGHUPReloadsTheWatcher(t *testing.T) {
	f := &fakeSignals{}
	r := newRig(t)
	stop := notifySIGHUP(r.w, f.hooks())
	defer stop()
	r.run()
	r.nextPoll()
	r.load()
	f.mu.Lock()
	ch := f.ch
	f.mu.Unlock()
	ch <- syscall.SIGHUP
	if tr := r.nextPoll(); tr != TriggerReload {
		t.Fatalf("poll after SIGHUP = %v, want reload", tr)
	}
	r.load()
}
