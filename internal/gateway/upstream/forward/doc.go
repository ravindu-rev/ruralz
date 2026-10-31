// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package forward is the plain-upstreams forwarder of a Route and the
// /debug/upstreams body of the admin server: the Upstream layer's part
// above the attempt loop (docs/engineering/02-repository-layout-and-conventions.md
// "Package layers", L4).
//
// # Forwarding
//
// A Forwarder is the snapshot.Forwarder of one Route with
// Route.spec.upstreams (docs/architecture/09-traffic-management-and-resilience.md
// "Load balancing"; docs/architecture/03-data-plane.md "Upstream layer"),
// built by the snapshot compiler over the snapshot's upstream.Set. For each
// request it picks one entry by weight under the Endpoint weight rule (an
// entry of weight 0 is never picked while another weighs more, and when
// every weight is 0 they all weigh 1; absent weights are 1) and runs one
// leg to that entry's Upstream (upstream.Upstream.Leg) on the request
// goroutine.
//
// When that leg is refused before any attempt is sent, 503 RZ-UP-005 (the
// breaker is open, or half-open with its probe in flight), RZ-UP-006 (the
// bulkhead and its pending queue are full) or RZ-UP-008 (the Endpoint set
// is empty), the request picks again by weight among the entries it has
// not tried, under the same rule, so a weight-0 entry serves once every
// weighted entry was refused. Each entry is tried at most once, and when
// none is left the request answers with the last refusal. A refused leg
// sent nothing: no leg Phase ran and the request body was not opened, so
// the next entry starts afresh. Every other outcome is final: a response,
// the Upstream's or a leg Filter's generated one; a leg error after an
// attempt was sent, a retry attempt the breaker, bulkhead or Endpoint set
// refused included (that leg ends with its previous attempt's outcome);
// RZ-RT-007 when the Route deadline passed before the leg's first attempt;
// and a leg the client canceled.
//
// Picks draw from the injected randomness, which must be safe for
// concurrent use; a Forwarder is immutable, safe for concurrent use and
// allocates nothing of its own per request (the onRoute row of
// docs/architecture/12-performance-budgets-and-benchmarking.md "Per-stage
// latency budget"), refusals included up to 64 entries. It records no
// metric and logs nothing: the leg counts its attempts, and the handler
// writes the response or the refusal's problem document.
//
// # Upstream debug
//
// Debug turns upstream.Manager.Status into the /debug/upstreams body of
// internal/adminapi (docs/architecture/03-data-plane.md "Admin
// endpoints"): one entry per Upstream runtime a live snapshot holds, in
// name order, with its Endpoint source, breaker, bulkhead, retry budget,
// panic mode, balancer structures and Endpoints; times in RFC 3339 UTC,
// null when unset; lists [] when empty. DebugHandler serves it; the
// ruralzd wiring injects that handler into the admin server, which
// authenticates, routes and refuses other methods first.
package forward
