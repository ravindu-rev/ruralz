// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"context"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// TestDriversShareTheInFlightCeiling checks the per-Node in-flight
// ceiling (8,192, docs/architecture/11-scalability-and-distributed-state.md
// "State client and RZ-STS error codes"): every driver the Manager opens,
// the main and the cache connection alike, receives the same ceiling of
// the Node's Limits.InFlight.
func TestDriversShareTheInFlightCeiling(t *testing.T) {
	var mu sync.Mutex
	var seen []*statestore.InFlight
	base := &opener{}
	capture := func(ctx context.Context, cfg statestore.Config, deps statestore.Deps) (statestore.Driver, error) {
		mu.Lock()
		seen = append(seen, deps.InFlight)
		mu.Unlock()
		return base.open(ctx, cfg, deps)
	}
	f := newFixture(t, Openers{Memory: capture, Redis: capture})
	spec := redisSpec("redis://127.0.0.1:6379")
	spec.Cache = cacheConfig("redis://127.0.0.1:6380")
	hs := f.acquire(t, spec)
	defer hs.Release()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] == nil || seen[0] != seen[1] {
		t.Fatalf("drivers got ceilings %v, want one shared ceiling", seen)
	}
	for range statestore.DefaultLimits().InFlight {
		if !seen[0].TryAcquire() {
			t.Fatal("the shared ceiling is below the 8,192 target")
		}
	}
	if seen[1].TryAcquire() {
		t.Fatal("the cache connection's ceiling is not the main one's")
	}
}
