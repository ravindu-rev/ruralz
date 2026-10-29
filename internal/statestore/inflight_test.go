// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package statestore

import (
	"sync"
	"testing"
)

// TestInFlightCeiling checks the Node-wide in-flight ceiling: TryAcquire
// admits up to the limit, refuses the next call, and admits again once a
// slot is released.
func TestInFlightCeiling(t *testing.T) {
	g := NewInFlight(3)
	for i := range 3 {
		if !g.TryAcquire() {
			t.Fatalf("TryAcquire %d refused below the limit", i+1)
		}
	}
	if g.TryAcquire() {
		t.Fatal("TryAcquire admitted a call beyond the limit")
	}
	if g.Len() != 3 {
		t.Fatalf("Len = %d, want 3", g.Len())
	}
	g.Release()
	if !g.TryAcquire() {
		t.Fatal("TryAcquire refused after a Release")
	}
}

// TestInFlightDefaultLimit checks that a zero limit takes the 8,192 target.
func TestInFlightDefaultLimit(t *testing.T) {
	g := NewInFlight(0)
	for i := range DefaultLimits().InFlight {
		if !g.TryAcquire() {
			t.Fatalf("TryAcquire %d refused below the default limit", i+1)
		}
	}
	if g.TryAcquire() {
		t.Fatalf("TryAcquire admitted call %d", DefaultLimits().InFlight+1)
	}
}

// TestInFlightConcurrent checks that concurrent callers never hold more
// slots than the limit.
func TestInFlightConcurrent(t *testing.T) {
	const limit = 16
	g := NewInFlight(limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	held, peak := 0, 0
	for range 64 {
		wg.Go(func() {
			for range 200 {
				if !g.TryAcquire() {
					continue
				}
				mu.Lock()
				held++
				peak = max(peak, held)
				mu.Unlock()
				mu.Lock()
				held--
				mu.Unlock()
				g.Release()
			}
		})
	}
	wg.Wait()
	if peak > limit {
		t.Fatalf("peak %d slots held, limit %d", peak, limit)
	}
	if g.Len() != 0 {
		t.Fatalf("Len = %d after every release, want 0", g.Len())
	}
}
