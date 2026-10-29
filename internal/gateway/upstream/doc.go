// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package upstream is the Upstream layer of a Node
// (docs/architecture/03-data-plane.md "Upstream layer";
// docs/architecture/09-traffic-management-and-resilience.md): the
// Upstream runtimes that live for the Node's lifetime and the attempt loop
// of one upstream leg, which the plain-upstreams forwarder and the
// composition engine call.
//
// # Runtimes
//
// A Manager holds one runtime per Upstream name, so health, ejection,
// breaker and pool state survive Hot Reloads: the health Tracker (passive
// ejection, active checks through the Node's Prober, stall and suspect),
// the resilience Runtime (breaker, bulkhead, retry budget), the active
// Endpoint source (static endpoints or discovery.type dns; kubernetes is
// RZ-CFG-040 until M2), its published Endpoint table, one balancer per
// algorithm the live snapshots use, and the TLS entries with their
// connection pools.
//
// Compile builds a snapshot's Set off the request path: each Upstream's
// settings from Upstream.spec with the schema defaults, a new source when
// the Revision changed it (resolved now, switched in at activation), the
// TLS client configuration, and the balancers planned against the Node's
// 256 MiB balancer budget (a ring that does not fit at 64 virtual nodes
// falls back to weighted random with balancer_budget and a configuration
// warning). Activate switches the runtimes to the Set at the snapshot
// swap: sources, health thresholds, metric handles (which receive the
// current breaker state, degraded reasons and pool counts) and the plan,
// and sets the upstream cleartext hop count. Close, at the snapshot's zero
// pins, releases its references; the last one closes a runtime and clears
// its degraded reasons. The Set is the snapshot's snapshot.LegRunner.
//
// Run drives the background work, every goroutine owned and joined: one
// goroutine per active source that has names to resolve, whose changes
// publish a new Endpoint table (the Tracker's list first, then every
// balancer's, so selection and health always agree by identity) and
// replan the budget; one rebuild goroutine per balancer whose rebuild is
// due (balance.Balancer.NextRebuild: at most every 10 s), at most 8
// building per Node (balance.BuildGate); and the Prober.
//
// # Connection pools
//
// Pools are keyed by protocol, TLS setting (SNI and secret references,
// never values) and Endpoint, and carried across Hot Reloads that keep the
// key. A pool is an http.Transport whose dialer reaches only its Endpoint:
// the cached addresses in answer order within the 1 s dial budget, each
// through the egress guard; TLS within 2 s with the rotating client
// configuration, h2 or http/1.1 by ALPN; cleartext is HTTP/1.1. HTTP/2
// connections ping after 1 s of silence and close after 5 s unanswered
// (health.PingAfter, health.PingTimeout); a server's stream limit opens
// another connection. The transport never asks for or decodes compression,
// and caps a response header block at the client header ceiling, 256 KiB
// (MaxResponseHeaderBytes). Connections count in
// ruralz_upstream_pool_connections, active while an attempt uses them.
// Idle connections close after 90 s (IdleConnTimeout).
//
// # The attempt loop
//
// Upstream.Leg runs one leg on the caller's goroutine: a Route deadline
// already past is RZ-RT-007; an empty set RZ-UP-008 and an open breaker
// RZ-UP-005 refuse the leg. Each attempt takes a bulkhead slot (a full
// queue is RZ-UP-006; a waiter is bounded by its attempt context), picks an
// Endpoint (ring-hash by the leg's hashKey; tried, suspect, stalled and
// capped Endpoints avoided while another remains; ejected and unhealthy
// ones only in panic mode), runs onUpstreamRequest, and sends a fresh copy
// of the client request: hop-by-hop fields stripped, forwarding fields
// rewritten (clientaddr), trace context injected, the normalized path
// unchanged, the Endpoint authority as Host. The attempt deadline is
// retries.perTryTimeout or the leg time left divided by the retries left
// plus one; it stops at response headers, so a slow body is not cut.
// failureWhen feeds passive ejection and the breaker; the default retryOn
// and failureWhen run natively (TestNativeDefaultsEqualCEL holds them equal
// to their CEL sources). onUpstreamResponseHeaders runs for every attempt
// that was sent: with the response, or without one (filter.Leg.Response
// nil, filter.Leg.ErrorKind set) on the leg's context, so a Filter can ask
// for a retry or a Provider Fallback on a failed attempt too. After it,
// retryOn or a Filter's retry decides under the replay rule, the attempts,
// the leg deadline, the breaker, Retry-After and the retry budget, then
// the full-jitter backoff runs on the clock. A retried response is drained when
// the retry attempt runs, and returned unchanged when the retry cannot
// start. The final response runs onUpstreamResponseBody, its body capped by
// the Target's MaxBodyBytes (a composition step's maxBodyBytes,
// filter.Leg.MaxBodyBytes), and keeps its bulkhead slot until its body is
// closed or read to the end. The round trips and the reads of the leg's
// response body, drains included, are Upstream I/O, excluded from
// gateway-added time through snapshot.Outbound.Excluder. A leg without
// a response is a *LegError with the code selection rule's RZ-UP code. An
// attempt the caller canceled is no Upstream failure. Each attempt is one
// ruralz.upstream.<name> span and one ruralz_upstream_attempts_total
// count; retries, refused retries, CEL errors and attempt durations are
// counted too.
//
// Times come from the injected clock.Clock and randomness from the
// injected source, so tests run on clocktest.Fake. The package logs
// nothing: errors reach the caller, telemetry and /debug/upstreams
// (Manager.Status).
package upstream
