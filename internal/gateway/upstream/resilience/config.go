// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"math"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Field defaults of 05 req 4 (targets). The Upstream core applies them when
// a field or its parent object is absent, so the runtime values equal the
// schema markers of OQ-traffic-management-and-resilience-6.
const (
	// DefaultRouteTimeout is Route.spec.timeout for every M1 Route (a
	// runtime rule, not materialized).
	DefaultRouteTimeout = 15 * time.Second
	// DefaultAttempts is retries.attempts: one retry.
	DefaultAttempts = 1
	// DefaultMaxConnections is circuitBreaker.maxConnections.
	DefaultMaxConnections = 1024
	// DefaultMaxPendingRequests is circuitBreaker.maxPendingRequests.
	DefaultMaxPendingRequests = 256
	// DefaultConsecutiveFailures is circuitBreaker.consecutiveFailures.
	DefaultConsecutiveFailures = 5
	// DefaultOpenDuration is circuitBreaker.openDuration.
	DefaultOpenDuration = 30 * time.Second
	// DefaultMinimumLegs is circuitBreaker.minimumLegs
	// (OQ-traffic-management-and-resilience-5 (a)).
	DefaultMinimumLegs = 20
	// DefaultFailureRatio is circuitBreaker.failureRatio
	// (OQ-traffic-management-and-resilience-5 (a)).
	DefaultFailureRatio = 0.5
	// DefaultHalfOpenSuccesses is circuitBreaker.halfOpenSuccesses
	// (OQ-traffic-management-and-resilience-5 (a)).
	DefaultHalfOpenSuccesses = 3
)

// Fixed targets of 05 req 4 ("Fixed (not fields)") and reqs 24 to 37. They
// are constants, not fields (OQ-traffic-management-and-resilience-5 (a) and
// -6: backoff and retry budget stay fixed; dial, TLS and stall are fixed).
const (
	// DialTimeout bounds one dial; its expiry is error kind connect.
	DialTimeout = time.Second
	// TLSHandshakeTimeout bounds one TLS handshake; its expiry is error
	// kind tls.
	TLSHandshakeTimeout = 2 * time.Second
	// BackoffBase is the backoff ceiling of the first retry.
	BackoffBase = 25 * time.Millisecond
	// BackoffCap caps the backoff ceiling of every retry.
	BackoffCap = 250 * time.Millisecond
	// RetryBudgetMin is the retries in flight the budget always allows.
	RetryBudgetMin = 3
	// RetryBudgetPercent is the share of in-flight originals the budget
	// allows as retries in flight, rounded down.
	RetryBudgetPercent = 20
	// MaxRetryAfter is the longest Upstream Retry-After a retry honors.
	MaxRetryAfter = 10 * time.Second
	// RetryDrainBytes is how much of a retried response's body the
	// Upstream layer drains before closing it (proposed).
	RetryDrainBytes = 64 << 10
	// BreakerWindow is the breaker's rolling window.
	BreakerWindow = 10 * time.Second
	// BreakerBuckets is the number of 1 s buckets of BreakerWindow.
	BreakerBuckets = 10
	// OpenJitterPercent spreads openDuration by this share either way.
	OpenJitterPercent = 20
	// EjectedMajorityPercent is the share of ejected Endpoints above which
	// a leg ending in a connect error counts for the breaker.
	EjectedMajorityPercent = 50
	// EndpointCapMin is the smallest per-Endpoint in-flight cap.
	EndpointCapMin = 8
	// EndpointCapSharePercent caps an Endpoint's share of maxConnections.
	EndpointCapSharePercent = 50
)

// Config is the resilience configuration of one Upstream in one snapshot.
// The Upstream core builds it from Upstream.spec (starting from
// DefaultConfig and overriding the fields present) at snapshot compile;
// it is immutable afterwards and shared by every leg of the snapshot.
type Config struct {
	// Timeout is Upstream.spec.timeout, the leg bound; 0 means the
	// Route's timeout.
	Timeout time.Duration
	// Retry is Upstream.spec.retries.
	Retry RetryConfig
	// Breaker is Upstream.spec.circuitBreaker without the bulkhead fields.
	Breaker BreakerConfig
	// Bulkhead is circuitBreaker.maxConnections and maxPendingRequests.
	Bulkhead BulkheadConfig
}

// RetryConfig is Upstream.spec.retries.
type RetryConfig struct {
	// Attempts counts retries after the first attempt (attempts: 2 allows
	// three attempts); 0 disables retries.
	Attempts int
	// PerTryTimeout bounds each attempt; 0 means the leg time left divided
	// by the retries left plus one, computed at each attempt start.
	PerTryTimeout time.Duration
	// RetryOn is the compiled retryOn program; nil runs the native form of
	// expr.DefaultRetryOn (DefaultRetryOn). 05 req 4 admits the native
	// form only once a table test proves it equal to the CEL default over
	// the 7 × 5 × 6 matrix through the CEL engine (05 section 6 test 5),
	// which needs internal/cel (WP-34) and so lives outside this package
	// (WP-34 or WP-46). Until that test exists the Upstream core fills
	// RetryOn with Compiler.Default(expr.PlaceRetryOn) and never leaves it
	// nil.
	RetryOn expr.Program
}

// BreakerConfig is Upstream.spec.circuitBreaker: the breaker thresholds
// and failureWhen.
type BreakerConfig struct {
	// ConsecutiveFailures is the run of failed counted legs that opens the
	// breaker, together with the window condition.
	ConsecutiveFailures int
	// FailureRatio is the share of failed legs, 0 to 1, the rolling window
	// also needs to open the breaker.
	FailureRatio float64
	// MinimumLegs is the number of counted legs the rolling window needs
	// before the breaker may open.
	MinimumLegs int
	// HalfOpenSuccesses is the run of successful probe legs that closes a
	// half-open breaker.
	HalfOpenSuccesses int
	// OpenDuration is the open time before one probe leg is let through,
	// jittered by OpenJitterPercent either way; 0 lets the probe through at
	// once.
	OpenDuration time.Duration
	// FailureWhen is the compiled failureWhen program; nil runs the native
	// form of expr.DefaultFailureWhen (DefaultFailureWhen). As for
	// RetryConfig.RetryOn, the Upstream core fills it with
	// Compiler.Default(expr.PlaceFailureWhen) until the CEL-engine equality
	// test of 05 section 6 test 5 exists.
	FailureWhen expr.Program
}

// BulkheadConfig is the in-flight ceiling of circuitBreaker.
type BulkheadConfig struct {
	// MaxConnections bounds in-flight attempts per Upstream per Node,
	// HTTP/2 streams included.
	MaxConnections int
	// MaxPendingRequests bounds the attempts waiting for a slot; 0 fails
	// at once when every slot is taken.
	MaxPendingRequests int
}

// DefaultConfig returns the configuration of an Upstream whose timeout,
// retries and circuitBreaker are absent (05 req 4).
func DefaultConfig() Config {
	return Config{
		Retry:    DefaultRetryConfig(),
		Breaker:  DefaultBreakerConfig(),
		Bulkhead: DefaultBulkheadConfig(),
	}
}

// DefaultRetryConfig returns the retries defaults: one retry, derived
// perTryTimeout, default retryOn.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{Attempts: DefaultAttempts}
}

// DefaultBreakerConfig returns the breaker defaults: 5 consecutive
// failures, 50% of at least 20 legs, 3 probe successes, 30 s open, default
// failureWhen.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		ConsecutiveFailures: DefaultConsecutiveFailures,
		FailureRatio:        DefaultFailureRatio,
		MinimumLegs:         DefaultMinimumLegs,
		HalfOpenSuccesses:   DefaultHalfOpenSuccesses,
		OpenDuration:        DefaultOpenDuration,
	}
}

// DefaultBulkheadConfig returns 1,024 in-flight attempts and 256 waiters.
func DefaultBulkheadConfig() BulkheadConfig {
	return BulkheadConfig{MaxConnections: DefaultMaxConnections, MaxPendingRequests: DefaultMaxPendingRequests}
}

// Normalize replaces values outside the schema ranges with safe ones:
// negative durations and counts that allow 0 become 0, and fields that
// must be positive (or a ratio outside [0, 1], NaN included) take their
// defaults. A validated Revision never needs it; it keeps a hand-built
// Config from wedging a breaker or a bulkhead. An openDuration of 0 is
// kept: the breaker lets a probe leg through as soon as it opens.
func (c *Config) Normalize() {
	c.Timeout = max(c.Timeout, 0)
	c.Retry.Attempts = max(c.Retry.Attempts, 0)
	c.Retry.PerTryTimeout = max(c.Retry.PerTryTimeout, 0)
	b := &c.Breaker
	b.OpenDuration = max(b.OpenDuration, 0)
	if b.ConsecutiveFailures < 1 {
		b.ConsecutiveFailures = DefaultConsecutiveFailures
	}
	if math.IsNaN(b.FailureRatio) || b.FailureRatio < 0 || b.FailureRatio > 1 {
		b.FailureRatio = DefaultFailureRatio
	}
	if b.MinimumLegs < 1 {
		b.MinimumLegs = DefaultMinimumLegs
	}
	if b.HalfOpenSuccesses < 1 {
		b.HalfOpenSuccesses = DefaultHalfOpenSuccesses
	}
	if c.Bulkhead.MaxConnections < 1 {
		c.Bulkhead.MaxConnections = DefaultMaxConnections
	}
	c.Bulkhead.MaxPendingRequests = max(c.Bulkhead.MaxPendingRequests, 0)
}
