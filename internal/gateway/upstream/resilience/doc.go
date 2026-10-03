// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package resilience holds the resilience primitives of the Upstream layer
// (spec 05 groups E, G, H and I, requirements 24 to 26 and 32 to 40;
// docs/architecture/09-traffic-management-and-resilience.md "Timeouts,
// deadlines, retries and hedging", "Circuit breakers" and "Error codes this
// document owns"):
//
//   - Config, RetryConfig, BreakerConfig and BulkheadConfig: one Upstream's
//     settings, filled by the Upstream core (internal/gateway/upstream)
//     from Upstream.spec, with the defaults of 05 req 4 (DefaultConfig) and
//     the fixed targets as constants (OQ-traffic-management-and-resilience-5
//     (a): backoff and retry budget stay fixed, the breaker reads
//     minimumLegs, failureRatio and halfOpenSuccesses);
//   - deadline arithmetic (RouteDeadline, LegDeadline, PerTryTimeout,
//     AttemptDeadline) and clock-driven attempt contexts (WithDeadline,
//     whose Stop keeps a slow body from being cut once headers arrive;
//     05 reqs 24 to 26, ExpiryCode);
//   - the retry rule of one leg (Leg.Decide): retryOn (EvalRetryOn, the
//     native form of expr.DefaultRetryOn when absent) or a Filter retry
//     request, replayable body, attempts, leg deadline after the delay,
//     breaker, Retry-After (ParseRetryAfter) and the retry budget; full
//     jitter backoff uniform(0, min(250 ms, 25 ms × 2^(n−1))) (Backoff;
//     05 reqs 32 to 35); the breaker gate again when each attempt starts
//     (Leg.StartAttempt; 05 req 11 step e);
//   - the retry budget: in-flight retries at most max(3, floor(20% ×
//     in-flight originals)) per Upstream per Node, every retry counted from
//     its backoff start to its leg's end (RetryBudget; 05 req 34);
//   - the circuit breaker (Breaker; 05 req 37, Figure 2 of the design
//     document): consecutiveFailures together with failureRatio of at least
//     minimumLegs legs in a rolling 10 s window of ten 1 s buckets, a
//     jittered openDuration, one half-open probe leg at a time (a retry
//     needs Breaker.Admits: closed, or this leg the probe) and
//     halfOpenSuccesses probe successes to close; failureWhen
//     (EvalFailureWhen, the native form of expr.DefaultFailureWhen when
//     absent) decides a leg's failure and BreakerResult whether it counts;
//     Breaker.Stats reads it for /debug/upstreams (05 req 96);
//   - the bulkhead: maxConnections in-flight attempts with a FIFO of at
//     most maxPendingRequests waiters (Bulkhead), and the per-Endpoint
//     in-flight cap (EndpointCap; 05 reqs 36 and 38);
//   - error kinds (Classify, ClassifyStage, StageTrace), the split of an
//     ended attempt context into a deadline expiry and a cancellation that
//     is no Upstream failure (Expired, Canceled), and the RZ-UP code
//     selection of a leg that ends without a response (SelectCode,
//     Outcome.Err; 05 reqs 39 and 40).
//
// Runtime groups one Upstream's Breaker, Bulkhead and RetryBudget; it
// lives for the Node's lifetime and is carried across Hot Reloads by
// Upstream name (05 req 2). Every call takes the configuration of the
// caller's snapshot, so new thresholds apply from the new snapshot on
// while in-flight legs keep their own snapshot's settings. Leg is the
// per-leg bookkeeping over a Runtime.
//
// The package imports internal/clock, internal/expr and internal/errcode
// only. It performs no I/O and records no telemetry: the Upstream core
// counts ruralz_upstream_retries_total for each retry attempt that starts
// (05 req 33: retries made), ruralz_upstream_retry_budget_exhausted_total
// from Leg.Decide's ReasonBudget, ruralz_upstream_cel_errors_total from
// the errors EvalRetryOn and EvalFailureWhen return, and sets
// ruralz_upstream_breaker_state_info from the Breaker's change callback;
// Kind and State values index the emit attempt-error and breaker-state
// arrays directly. Time comes from an injected clock.Clock and randomness
// from an injected math/rand/v2 Source, so tests are deterministic with
// clocktest.Fake and fixed draws. The only goroutines are the clock's
// timer callbacks of WithDeadline, owned and stopped by their Deadline.
package resilience
