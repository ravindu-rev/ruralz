// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"context"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
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

// gaugeValue is an emit.Gauge holding one value.
type gaugeValue struct {
	mu sync.Mutex
	v  int64
}

func (g *gaugeValue) Add(_ emit.Stripe, d int64) { g.mu.Lock(); g.v += d; g.mu.Unlock() }
func (g *gaugeValue) Set(v int64)                { g.mu.Lock(); g.v = v; g.mu.Unlock() }
func (g *gaugeValue) get() int64                 { g.mu.Lock(); defer g.mu.Unlock(); return g.v }

// TestInFlightCeilingReportsTheGauge checks that the Node's one in-flight
// ceiling, which the Manager builds for its drivers, reports its slots in
// use as ruralz_state_inflight_calls through the State Store metrics the
// Manager receives (docs/architecture/10-observability.md "Ruralz Gateway
// metrics"): calls of the main and the cache connection both count.
func TestInFlightCeilingReportsTheGauge(t *testing.T) {
	var mu sync.Mutex
	var seen []*statestore.InFlight
	base := &opener{}
	capture := func(ctx context.Context, cfg statestore.Config, deps statestore.Deps) (statestore.Driver, error) {
		mu.Lock()
		seen = append(seen, deps.InFlight)
		mu.Unlock()
		return base.open(ctx, cfg, deps)
	}
	gauge := &gaugeValue{}
	m := New(t.Context(), Config{
		Deps:    statestore.Deps{Clock: clocktest.New(epoch()), Status: &nodeStatus{}, Metrics: &emit.StateMetrics{InFlight: gauge}},
		Openers: Openers{Memory: capture, Redis: capture},
	})
	t.Cleanup(func() { _ = m.Close(t.Context()) })
	spec := redisSpec("redis://127.0.0.1:6379")
	spec.Cache = cacheConfig("redis://127.0.0.1:6380")
	hs, err := m.Acquire(t.Context(), spec)
	if err != nil {
		t.Fatalf("Acquire = %v", err)
	}
	defer hs.Release()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != seen[1] {
		t.Fatalf("drivers got ceilings %v, want one shared ceiling", seen)
	}
	seen[0].TryAcquire()
	seen[1].TryAcquire()
	if got := gauge.get(); got != 2 {
		t.Fatalf("ruralz_state_inflight_calls = %d with a main and a cache call in flight, want 2", got)
	}
	seen[0].Release()
	seen[1].Release()
	if got := gauge.get(); got != 0 {
		t.Fatalf("ruralz_state_inflight_calls = %d once both calls replied, want 0", got)
	}
}
