// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package drain

import (
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"
)

// fakeSignals records what notify registers, stops and ignores.
type fakeSignals struct {
	mu         sync.Mutex
	ch         chan<- os.Signal
	registered []os.Signal
	ignored    []os.Signal
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
		ignore: func(sig ...os.Signal) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.ignored = append(f.ignored, sig...)
		},
	}
}

// TestNotifyForwardsStopSignals checks the signal handling of "Drain
// timeline defaults": SIGTERM and SIGINT, and nothing else, reach Signal,
// the first starting the Drain and the second jumping to its deadline;
// SIGPIPE is ignored; stop unregisters once and is idempotent.
func TestNotifyForwardsStopSignals(t *testing.T) {
	f := &fakeSignals{}
	r := newRig(t)
	r.run(t.Context())
	stop := notify(r.d, f.hooks())
	f.mu.Lock()
	registered, ignored, ch := slices.Clone(f.registered), slices.Clone(f.ignored), f.ch
	f.mu.Unlock()
	if len(registered) != 2 || !slices.Contains(registered, os.Signal(syscall.SIGTERM)) || !slices.Contains(registered, os.Interrupt) {
		t.Errorf("registered %v, want SIGTERM and SIGINT only", registered)
	}
	if !slices.Equal(ignored, []os.Signal{syscall.SIGPIPE}) {
		t.Errorf("ignored %v, want SIGPIPE", ignored)
	}

	ch <- syscall.SIGTERM
	recv(t, "the Drain to start", r.d.Started())
	r.waitArmed(t)
	ch <- os.Interrupt
	r.finished(t)
	if !r.ev.has("endall@0s") {
		t.Errorf("events %q, want the second signal's jump to the deadline", r.ev.all())
	}
	stop()
	stop()
	if f.stops != 1 {
		t.Errorf("unregistered %d times, want 1", f.stops)
	}
}
