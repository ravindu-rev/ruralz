// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package replay is the Response Cache's filter.Replayer: it sends a saved
// request through one Upstream's leg, or a composition Route's whole
// composition, outside any client request, for background revalidation
// (docs/architecture/03-data-plane.md "Upstream layer";
// docs/architecture/09-traffic-management-and-resilience.md "Response
// caching").
//
// A replay takes one in-flight unit, pins the published snapshot and
// registers with the ending protocol, holding all three until the caller
// closes the response body. It resolves the Route and the Upstream by
// name in the pinned snapshot: a Route the snapshot no longer holds as the
// entry knew it (a composition Route for an entry without an Upstream, a
// Route with upstreams otherwise), an Upstream it no longer holds, a full
// in-flight ceiling, no published snapshot, or a snapshot the ending
// protocol already ended is filter.ErrNotAvailable.
//
// The replay runs on a synthetic exchange: CEL request is the saved
// request, route the named Route, source empty, consumer and auth null. On
// a Route with upstreams it runs one leg to the named Upstream through the
// snapshot's snapshot.LegRunner, with that Upstream's upstream-leg
// Policies, breaker, bulkhead and retries; on a composition Route it runs
// the Route's composition, every step under the composition engine's
// rules. Every leg removes the forwarding fields of the saved request and
// adds none, and adds no Via entry, since a replay has no client hop
// (snapshot.Outbound.Replay). The deadline is the call's start plus the
// Route timeout on the Replayer's clock; the caller's context, the
// revalidation worker's own bound, still cancels it, and so does the
// ending protocol.
//
// A replay starts a new, unsampled root trace, whatever traceparent the
// saved request carries, and writes no access record and no Route request
// metrics: only the Upstream metrics count it. A response the Node
// generates instead of an Upstream, such as a leg Filter's 401 or a closed
// failure's 502, returns as an error (ErrGenerated, with the response's
// code), as a replay without a response does, so the caller keeps its
// entry.
package replay
