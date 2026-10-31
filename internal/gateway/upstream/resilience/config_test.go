// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"testing"
	"time"
)

// TestDefaultConfig holds the runtime defaults of
// docs/architecture/09-traffic-management-and-resilience.md "Deadlines".
func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	want := Config{
		Retry: RetryConfig{Attempts: 1},
		Breaker: BreakerConfig{
			ConsecutiveFailures: 5,
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

// TestFixedTargets holds the field defaults kept as constants (Route
// timeout, connectTimeout, tls.handshakeTimeout) and the fixed targets,
// the breaker guards among them: at least 50% failed of at least 20 legs,
// and 3 probe successes to close.
func TestFixedTargets(t *testing.T) {
	tests := []struct {
		name      string
		got, want time.Duration
	}{
		{"Route timeout", DefaultRouteTimeout, 15 * time.Second},
		{"connectTimeout", DefaultConnectTimeout, time.Second},
		{"tls.handshakeTimeout", DefaultHandshakeTimeout, 2 * time.Second},
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
		{"breaker minimum legs", BreakerMinimumLegs, 20},
		{"breaker failure percent", BreakerFailurePercent, 50},
		{"half-open successes", HalfOpenSuccesses, 3},
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
// schema ranges and defaults.
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
				Breaker:  BreakerConfig{ConsecutiveFailures: -1, OpenDuration: -time.Second},
				Bulkhead: BulkheadConfig{MaxConnections: -1, MaxPendingRequests: -1},
			},
			want: Config{
				Breaker:  BreakerConfig{ConsecutiveFailures: 5},
				Bulkhead: BulkheadConfig{MaxConnections: 1024},
			},
		},
		{
			name: "zero values allowed by the schema stay",
			in: Config{
				Breaker:  BreakerConfig{ConsecutiveFailures: 1},
				Bulkhead: BulkheadConfig{MaxConnections: 1},
			},
			want: Config{
				Breaker:  BreakerConfig{ConsecutiveFailures: 1},
				Bulkhead: BulkheadConfig{MaxConnections: 1},
			},
		},
		{
			name: "zero consecutive failures",
			in:   Config{Breaker: BreakerConfig{OpenDuration: time.Second}, Bulkhead: BulkheadConfig{MaxConnections: 2}},
			want: Config{Breaker: BreakerConfig{ConsecutiveFailures: 5, OpenDuration: time.Second}, Bulkhead: BulkheadConfig{MaxConnections: 2}},
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
