// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"math"
	"testing"
	"time"
)

// TestDefaultConfig_05Req4 holds the runtime defaults of 05 req 4 and
// OQ-traffic-management-and-resilience-5 (a).
func TestDefaultConfig_05Req4(t *testing.T) {
	c := DefaultConfig()
	want := Config{
		Retry: RetryConfig{Attempts: 1},
		Breaker: BreakerConfig{
			ConsecutiveFailures: 5,
			FailureRatio:        0.5,
			MinimumLegs:         20,
			HalfOpenSuccesses:   3,
			OpenDuration:        30 * time.Second,
		},
		Bulkhead: BulkheadConfig{MaxConnections: 1024, MaxPendingRequests: 256},
	}
	if c != want {
		t.Fatalf("DefaultConfig() = %+v, want %+v", c, want)
	}
	if c.Timeout != 0 || c.Retry.PerTryTimeout != 0 || c.Retry.RetryOn != nil || c.Breaker.FailureWhen != nil {
		t.Fatal("runtime rules (Upstream timeout, perTryTimeout, retryOn, failureWhen) must default to their zero values")
	}
}

// TestFixedTargets_05Req4 holds the fixed targets of 05 req 4 ("Fixed (not
// fields)") and reqs 24 to 37.
func TestFixedTargets_05Req4(t *testing.T) {
	tests := []struct {
		name      string
		got, want time.Duration
	}{
		{"Route timeout", DefaultRouteTimeout, 15 * time.Second},
		{"dial", DialTimeout, time.Second},
		{"TLS handshake", TLSHandshakeTimeout, 2 * time.Second},
		{"backoff base", BackoffBase, 25 * time.Millisecond},
		{"backoff cap", BackoffCap, 250 * time.Millisecond},
		{"max Retry-After", MaxRetryAfter, 10 * time.Second},
		{"breaker window", BreakerWindow, 10 * time.Second},
		{"breaker bucket", BreakerWindow / BreakerBuckets, time.Second},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	ints := []struct {
		name      string
		got, want int
	}{
		{"retry budget minimum", RetryBudgetMin, 3},
		{"retry budget percent", RetryBudgetPercent, 20},
		{"open jitter percent", OpenJitterPercent, 20},
		{"ejected majority percent", EjectedMajorityPercent, 50},
		{"Endpoint cap minimum", EndpointCapMin, 8},
		{"Endpoint cap share percent", EndpointCapSharePercent, 50},
		{"retry drain bytes", RetryDrainBytes, 64 << 10},
	}
	for _, tt := range ints {
		if tt.got != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

// TestNormalize covers the repair of hand-built configurations to the
// schema ranges and defaults of 05 req 4.
func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   Config
		want Config
	}{
		{
			name: "defaults stay",
			in:   DefaultConfig(),
			want: DefaultConfig(),
		},
		{
			name: "negative values",
			in: Config{
				Timeout:  -time.Second,
				Retry:    RetryConfig{Attempts: -1, PerTryTimeout: -time.Second},
				Breaker:  BreakerConfig{ConsecutiveFailures: -1, FailureRatio: -0.1, MinimumLegs: -1, HalfOpenSuccesses: -1, OpenDuration: -time.Second},
				Bulkhead: BulkheadConfig{MaxConnections: -1, MaxPendingRequests: -1},
			},
			want: Config{
				Breaker:  BreakerConfig{ConsecutiveFailures: 5, FailureRatio: 0.5, MinimumLegs: 20, HalfOpenSuccesses: 3},
				Bulkhead: BulkheadConfig{MaxConnections: 1024},
			},
		},
		{
			name: "zero values allowed by the schema stay",
			in: Config{
				Breaker:  BreakerConfig{ConsecutiveFailures: 1, FailureRatio: 0, MinimumLegs: 1, HalfOpenSuccesses: 1},
				Bulkhead: BulkheadConfig{MaxConnections: 1},
			},
			want: Config{
				Breaker:  BreakerConfig{ConsecutiveFailures: 1, FailureRatio: 0, MinimumLegs: 1, HalfOpenSuccesses: 1},
				Bulkhead: BulkheadConfig{MaxConnections: 1},
			},
		},
		{
			name: "ratio above one",
			in:   Config{Breaker: BreakerConfig{ConsecutiveFailures: 2, FailureRatio: 1.5, MinimumLegs: 2, HalfOpenSuccesses: 2, OpenDuration: time.Second}, Bulkhead: BulkheadConfig{MaxConnections: 2}},
			want: Config{Breaker: BreakerConfig{ConsecutiveFailures: 2, FailureRatio: 0.5, MinimumLegs: 2, HalfOpenSuccesses: 2, OpenDuration: time.Second}, Bulkhead: BulkheadConfig{MaxConnections: 2}},
		},
		{
			name: "NaN ratio",
			in:   Config{Breaker: BreakerConfig{ConsecutiveFailures: 2, FailureRatio: math.NaN(), MinimumLegs: 2, HalfOpenSuccesses: 2}, Bulkhead: BulkheadConfig{MaxConnections: 2}},
			want: Config{Breaker: BreakerConfig{ConsecutiveFailures: 2, FailureRatio: 0.5, MinimumLegs: 2, HalfOpenSuccesses: 2}, Bulkhead: BulkheadConfig{MaxConnections: 2}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.in
			c.Normalize()
			if c != tt.want {
				t.Fatalf("Normalize() = %+v, want %+v", c, tt.want)
			}
		})
	}
}
