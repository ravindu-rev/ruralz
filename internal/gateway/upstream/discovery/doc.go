// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package discovery produces an Upstream's Endpoint set (spec 05 group B,
// requirements 5 to 9; docs/architecture/09-traffic-management-and-resilience.md
// "Service discovery"):
//
//   - static endpoints: an IP literal is one Endpoint; a host name is one
//     Endpoint whose identity is the configured address and whose IP list is
//     resolved at compile time and every 30 s, keeping the last good answer
//     (05 req 5);
//   - discovery.type dns with a numeric port: A and AAAA lookups of the
//     service, each IP one Endpoint ip:port of weight 1 (05 req 6);
//   - discovery.type dns with a named port: SRV _<port>._tcp.<service>, the
//     lowest-priority group only, each target one Endpoint target:port with
//     its SRV weight, resolved to IPs like a static host name (05 req 6).
//
// A Source owns one Upstream's lookups. Refresh runs them, publishes an
// immutable Set copy-on-write (05 req 8: Endpoints sorted by identity, so
// every Node given the same answer builds identical balancer structures)
// and returns the delay until the next refresh: 30 s with ±10% jitter
// after a good answer, full-jitter backoff from 1 s to 60 s after a
// failure (05 reqs 6 and 7). A failure keeps the last set and raises the
// discovery_stale degraded reason on the Upstream gauge and on the Node
// gauge; NXDOMAIN or an empty answer counts as a failure until 3
// consecutive refreshes repeat it, and only then empties the set, which the
// Upstream layer turns into 503 RZ-UP-008 (05 req 7). A good answer clears
// discovery_stale. The accepted empty answer clears it too, but its
// refreshes stay on the failure backoff, so the name is looked up again
// within 1 s to 60 s rather than 30 s while the set is empty; the 30 s
// schedule resumes with the next non-empty answer. An SRV answer from
// which the resolver dropped records with malformed target names is a good
// answer made of the remaining records. When it dropped every record, the
// lookup error stands: an ordinary failure that keeps the last set, unlike
// an answer whose records all name unusable targets (such as "."), which
// is an empty answer. net.DNSError marks malformed records only by its
// message, so the error cannot be told from other permanent failures, and
// a response the resolver cannot read whole more likely means a broken
// server than a withdrawn service.
//
// Each Source holds the Node reason under a source of its own (the
// Upstream name and an instance suffix), so a Source replaced by a Hot
// Reload and its successor raise and clear it independently while both
// live. The Upstream gauge is one series per Upstream: the activated
// Source writes it (SetMetrics, then its own transitions), the Source it
// replaced stops writing it at that activation, and Close leaves it alone.
//
// The refresh period is fixed rather than taken from record TTLs: the
// pure-Go net.Resolver (CGO_ENABLED=0, the Tech stack "Service discovery"
// row) does not expose TTLs, and 05 req 6 fixes the period; the only clamps
// are the jitter window and the 1 s to 60 s backoff bounds.
//
// The package never filters addresses. Every connection to an Endpoint is
// checked on its actual socket address by the dial function the Upstream
// layer injects (internal/egress: net.Dialer.Control refuses loopback,
// link-local, unspecified and metadata addresses unless RURALZ_FETCH_ALLOW
// lists them; 05 req 9), and Dial tries an Endpoint's cached IPs in answer
// order within the single 1 s dial budget (05 req 5), so such a refusal is
// a connect error like any other.
//
// The Upstream layer holds each Source as a Provider, the extension point
// kubernetes discovery (M2) implements with EndpointSlices (spec 05
// section 8). Activation (SetMetrics) and teardown (Close) never wait for
// a lookup in flight.
//
// Goroutines: Refresh resolves host names with at most 8 lookups at a time
// and joins them before it returns; Run is a blocking loop on the caller's
// goroutine that stops when its context ends. Run waits for the delay the
// last Refresh returned before it looks up again, so the compile-time
// Refresh followed by Run performs no back-to-back lookups. Neither starts
// anything that outlives the call. Schedules are measured on the Clock's
// elapsed time (Since), never on the wall clock.
package discovery
