// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"sync"
	"testing"
	"time"
)

// TestLoadCounterWindow covers 05 req 13: the score counts in-flight
// attempts plus failures of the last second, in 100 ms buckets.
func TestLoadCounterWindow(t *testing.T) {
	var c LoadCounter
	now := time.Unix(1_800_000_000, 0)
	c.Begin()
	c.Begin()
	c.End()
	if c.InFlight() != 1 {
		t.Fatalf("InFlight = %d", c.InFlight())
	}
	c.Fail(now)
	c.Fail(now.Add(50 * time.Millisecond))
	c.Fail(now.Add(550 * time.Millisecond))
	tests := []struct {
		at   time.Duration
		want int64
	}{
		{0, 2}, // the +50 ms failure shares the first bucket
		{60 * time.Millisecond, 2},
		{600 * time.Millisecond, 3},
		{999 * time.Millisecond, 3},
		{1000 * time.Millisecond, 1}, // the first bucket expired
		{1499 * time.Millisecond, 1},
		{1500 * time.Millisecond, 0},
		{time.Hour, 0},
	}
	for _, tt := range tests {
		if got := c.Failures(now.Add(tt.at)); got != tt.want {
			t.Errorf("Failures at +%v = %d, want %d", tt.at, got, tt.want)
		}
	}
	if got := c.Score(now.Add(600 * time.Millisecond)); got != 4 {
		t.Fatalf("Score = %d, want 4", got)
	}
	// A reused bucket forgets its old epoch.
	c.Fail(now.Add(2 * time.Second))
	if got := c.Failures(now.Add(2 * time.Second)); got != 1 {
		t.Fatalf("Failures after reuse = %d", got)
	}
	// Unbalanced End and times before the epoch stay sane.
	c.End()
	c.End()
	if c.InFlight() != 0 {
		t.Fatalf("InFlight below zero reads %d", c.InFlight())
	}
	var early LoadCounter
	early.Fail(time.Unix(-5, 0))
	if got := early.Failures(time.Unix(0, 0)); got != 1 {
		t.Fatalf("pre-epoch failure counted %d", got)
	}
}

// TestLoadCounterSaturates covers the 24-bit bucket count.
func TestLoadCounterSaturates(t *testing.T) {
	var c LoadCounter
	now := time.Unix(1_800_000_000, 0)
	c.fails[epoch(now)%failBuckets].Store(epoch(now)<<countBits | (countMask - 1))
	c.Fail(now)
	c.Fail(now)
	if got := c.Failures(now); got != countMask {
		t.Fatalf("saturated count %d, want %d", got, countMask)
	}
}

// TestLoadCounterConcurrent runs Fail and Begin/End from many goroutines
// (under -race) and checks nothing is lost.
func TestLoadCounterConcurrent(t *testing.T) {
	var c LoadCounter
	now := time.Unix(1_800_000_000, 0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				c.Begin()
				c.Fail(now)
				c.End()
			}
		})
	}
	wg.Wait()
	if c.Failures(now) != 8000 || c.InFlight() != 0 {
		t.Fatalf("failures %d, in flight %d", c.Failures(now), c.InFlight())
	}
}

// TestEndpointCap covers 05 req 36's per-Endpoint cap, the balancer's load
// bound: max(8, floor(min(2 × w_i/Σw, 0.5) × maxConnections)).
func TestEndpointCap(t *testing.T) {
	tests := []struct {
		w     uint32
		total uint64
		maxC  int
		want  int
	}{
		{1, 3, 1024, 512},  // 2/3 → 0.5
		{1, 4, 1024, 512},  // exactly 0.5
		{1, 10, 1024, 204}, // 0.2 × 1024 = 204.8
		{1, 100, 1024, 20}, // 0.02 × 1024 = 20.48
		{1, 1000, 1024, 8}, // floor of 8
		{0, 10, 1024, 8},   // weight 0
		{5, 0, 1024, 8},    // no total
		{1, 2, 0, 8},       // no connections
		{1, 3, 7, 8},       // 3 < 8
		{1<<31 - 1, 1 << 40, 1<<31 - 1, 1<<23 - 1},
	}
	for _, tt := range tests {
		if got := EndpointCap(tt.w, tt.total, tt.maxC); got != tt.want {
			t.Errorf("EndpointCap(%d, %d, %d) = %d, want %d", tt.w, tt.total, tt.maxC, got, tt.want)
		}
	}
}
