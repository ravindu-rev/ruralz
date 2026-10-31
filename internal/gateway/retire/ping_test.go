// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Tests for Ping, the retirer's watchdog probe (sdnotify.Probe): it
// answers once Run or Close has made a pass after the call began, and
// never while the retirer does not run.

// registeredPings returns the Ping calls waiting for a pass.
func registeredPings(h *Holder) int {
	h.pingMu.Lock()
	defer h.pingMu.Unlock()
	return len(h.pings)
}

func TestPingAnswersAfterRunPass(t *testing.T) {
	x := newHarness(t, Config{}, true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for i := range 3 {
		if err := x.h.Ping(ctx); err != nil {
			t.Fatalf("Ping #%d = %v, want nil once Run passes", i+1, err)
		}
	}
	if n := registeredPings(x.h); n != 0 {
		t.Errorf("%d pings still registered after their answers", n)
	}
}

func TestPingWithoutRunWaitsForContext(t *testing.T) {
	x := newHarness(t, Config{}, false)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- x.h.Ping(ctx) }()
	waitFor(t, "the Ping to register", func() bool { return registeredPings(x.h) == 1 })
	select {
	case err := <-done:
		t.Fatalf("Ping = %v without Run, want it to wait", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Ping = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Ping did not return when its context ended")
	}
	if n := registeredPings(x.h); n != 0 {
		t.Errorf("an abandoned Ping stays registered (%d)", n)
	}
}

func TestPingAnsweredByClose(t *testing.T) {
	x := newHarness(t, Config{}, false)
	done := make(chan error, 1)
	go func() { done <- x.h.Ping(t.Context()) }()
	waitFor(t, "the Ping to register", func() bool { return registeredPings(x.h) == 1 })
	if err := x.h.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Ping = %v, want nil after Close's pass", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not answer the Ping")
	}
}
