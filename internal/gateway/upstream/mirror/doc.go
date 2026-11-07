// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package mirror runs the mirror legs of a Node's Routes
// (docs/architecture/09-traffic-management-and-resilience.md "Mirroring";
// docs/architecture/03-data-plane.md "Goroutines" and "Bounded
// resources"): copies of what the first attempt of a Route's upstreams leg
// sends, to the Upstreams of Route.spec.mirrors, with their responses
// discarded. A mirror never delays or fails the request it copies: nothing
// on the request's path waits for a mirror leg, and a leg's outcome never
// reaches the request.
//
// # Offers
//
// The Upstream layer offers the first attempt that a plain upstreams leg
// sends, after its onUpstreamRequest Policies and before it goes out, to
// the request handler's snapshot.MirrorSink, which hands it to
// Runner.Offer on the request's goroutine. For each mirror, in Upstream
// name order:
//
//  1. The request is sampled: a draw from the injected random source below
//     percent of 100 (percent 100 always, 0 never, without a draw). A
//     request not sampled is not counted.
//  2. The mirror takes one of its maxInFlight slots on the Node
//     (snapshot.Mirror.InFlight, carried across Hot Reloads), or the copy is
//     dropped.
//  3. The mirrors that took a slot share one copy of the attempt's body,
//     reserved from limits.maxBufferedBytes as its bytes arrive
//     (body.Account, Gate kind, so it never takes the stream share): a
//     buffered body (the gate, or a body an onUpstreamRequest Policy wrote)
//     is copied at once through the request's GetBody; a streamed one as
//     the attempt sends it, through a reader the offer puts in place of the
//     request's body, which never blocks on a mirror. A reservation the
//     budget refuses drops every mirror of the copy, and so does a streamed
//     body the attempt did not send to its end (it failed or was cut at
//     limits.maxRequestBodyBytes, or the Upstream answered before reading
//     it), since a mirror sends only whole requests.
//  4. Each mirror pins the request's snapshot once more for its leg
//     (Pinner.PinLeg), so the snapshot, its Upstream layer and its metric
//     handles outlive the request for as long as the leg runs, a Hot
//     Reload that retired the snapshot meanwhile included, and the ending
//     protocol ends the leg with the request's other work; past the Drain
//     deadline nothing pins, and the copy is dropped.
//
// Every offer copies the attempt's method, path, query, header fields
// (forwarding and trace context fields included, as the attempt sends
// them) and trailer section, and evaluates the ring-hash key of each
// mirror Upstream (snapshot.MirrorLegs.MirrorKey) while the request's
// variables are live; nothing of the request is read after Offer returns
// but the streamed body's copy, which the attempt fills.
//
// # Legs
//
// Each mirror that pinned runs one goroutine of the Runner, which owns
// them from New on: once Run's context ends, Run ends them all, starts no
// other and returns when every leg has. A leg waits for a streamed body's
// copy to complete, within the request's Route deadline, then sends one
// attempt through
// snapshot.MirrorLegs.Mirror: the mirror Upstream's balancing, health,
// breaker, bulkhead without queueing, pools and timeouts, no Policy, no
// retry and no Node in-flight unit. The Route deadline, from the request
// start, bounds the leg as it bounds the request's own legs.
//
// Each sampled copy ends with exactly one result of
// ruralz_upstream_mirrors_total, on the Route and the mirror's Upstream:
// sent when the leg got response headers, whatever their status; dropped
// when the Node never handed it to the mirror's Upstream (past
// maxInFlight, a body copy that could not complete, the Drain deadline
// passed, the leg ended before its copy completed); failed when the
// mirror's Upstream got no response (no Endpoint, an open breaker, a full
// bulkhead, a connect failure, a timeout, a reset, the leg ended by the
// ending protocol or the Node's shutdown), a recovered panic included,
// which is logged with its stack at error.
package mirror
