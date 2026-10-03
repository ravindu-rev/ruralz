// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package health judges an Upstream's Endpoints on one Node (spec 05 group
// D, requirements 18 to 23; docs/architecture/09-traffic-management-and-resilience.md
// "Health checking and outlier detection"). Each Node judges alone.
//
// A Tracker holds the per-Endpoint state of one Upstream for the Node's
// lifetime, keyed by Endpoint identity so it survives Hot Reloads and
// Endpoint-set changes (05 req 2):
//
//   - passive ejection, always on: consecutiveErrors consecutive attempts
//     matching failureWhen eject an Endpoint for ejectionTime × its
//     ejection count, the count capped at 10 and decremented by one per
//     ejectionTime spent not ejected (05 req 18);
//   - the ejection cap: at most floor(50% × E) Endpoints passively ejected
//     at once; an ejection over it is skipped unless its trigger is a
//     connect error (dial error, dial timeout, HTTP/2 ping close) or an
//     attempt timeout on a suspect Endpoint (05 req 19). Ejections are
//     serialized per Upstream, so concurrent attempts never exceed it;
//   - active health from probe results: healthyThreshold consecutive
//     successes mark an Endpoint healthy, unhealthyThreshold consecutive
//     failures unhealthy; new Endpoints start healthy (05 req 20);
//   - stall and suspect: an attempt without response headers 1 s after it
//     started is stalled; an Endpoint holding a stalled attempt that has
//     delivered no response headers for 1 s is suspect until any attempt
//     to it gets headers; once stalled attempts hold 50% of maxConnections,
//     every Endpoint holding one is avoided (05 req 21);
//   - the ruralz_upstream_healthy_endpoints gauge (05 req 23) and the
//     attempts matching failureWhen in the last second, which least-request
//     adds to an Endpoint's load (05 req 13).
//
// The Upstream layer (internal/gateway/upstream) reads a View per attempt
// to grade Endpoints for package balance: Down is 05 req 11 step (b)
// (passively ejected or actively unhealthy), Avoid the suspect and stalled
// part of step (c). It tracks each attempt with an Attempt (Begin, Headers,
// End), which arms the pooled 1 s stall timer and feeds passive ejection.
// It raises upstream_panic itself from the Tracker's healthy count
// (Config.OnHealthy); this package raises no Upstream degraded reason.
//
// A Prober runs the active checks of every Upstream on the Node from one
// scheduling goroutine (Run): GET path to each Endpoint every interval ±10%
// through the injected ProbeFunc, which uses the Upstream's TLS settings
// and the egress-guarded dialer and takes no bulkhead slot; a status from
// 200 to 399 within timeout succeeds. Probe slots per Node are min(256,
// ceil(Σ probes per second × timeout)), shared across Upstreams; a probe
// that finds no slot is skipped and counted in
// ruralz_upstream_probes_skipped_total, and more than 10% skipped over the
// last minute raises the probes_skipped Node degraded reason (05 req 20).
//
// Every deadline and window here (ejection end and decay, stall and
// suspect ages, the failure window, the probe schedule and the skipped
// share) is elapsed time on the injected Clock, counted from the Tracker's
// or Prober's creation: the times callers pass are differenced against a
// Clock reading taken then (monotonic on clock.Real), and timers re-read
// Clock.Since. A wall-clock step (an NTP step at boot, a VM resume, a
// manual date change) therefore neither prolongs nor shortens an
// ejection, nor halts, nor bursts the probes.
//
// HTTP/2 connections to Endpoints ping after PingAfter of silence and close
// after PingTimeout unanswered (05 req 22); the Upstream layer configures
// its transports with those values and reports attempts reset by such a
// close with CauseConnect, so they bypass the ejection cap.
package health
