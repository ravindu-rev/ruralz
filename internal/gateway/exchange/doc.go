// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package exchange is the pooled per-request state of the data plane
// (docs/architecture/03-data-plane.md "Process and concurrency model" and
// "Filter Chain execution"). A Request is one client request: the
// snapshot.RequestState the executor drives, the filter.Exchange a Filter
// sees, and the filter.Message of each fixed Phase (the client request in
// onRequestHeaders, onRequestBody and onRoute, a leg's outgoing request in
// onUpstreamRequest, a leg's response in onUpstreamResponseHeaders and
// onUpstreamResponseBody, the client response in onResponse).
//
// # Lifecycle
//
// The request goroutine takes a Request from a Pool and follows "Anatomy
// of a request": Begin with the request and what the handler knows before
// routing; MatchRequest and ParamBuf for the Router; SetRoute; PrepareBody
// after onRequestHeaders, which reads the request gate (BodyNeeds.RequestGate)
// before onRequestBody; Outbound for the Forwarder, whose Excluder is the
// request's gateway-added timer (Timer), so the Upstream layer excludes its
// I/O; SetResponse or
// SetGenerated before onResponse and GateResponse for the response gate
// (BodyNeeds.ResponseGate); Reply, Tees and RateLimitFields to commit;
// SetFinal before onLog; FillAccess for the access record; Pool.Put after
// the record is submitted. Nothing on that path allocates once the pools
// are warm (PB-8, docs/architecture/12-performance-budgets-and-benchmarking.md),
// except formatting RequestID on demand, normalizing a host that is not
// lower-case, and the body work of gates, tees, rewrites and decodes.
//
// # Filter calls
//
// Enter selects the Policy, Phase and span of the next Filter call, so
// Message, Vars, PolicyState and Annotate refer to the Policy being run.
// PolicyState slots and spec.when decisions are indexed by Policy.Index
// and grow to the largest index seen, keeping their memory across reuse.
// Vars is the CEL activation of the current Phase: the pre-route variables
// (request, source, now) from Begin, route from SetRoute, consumer and
// auth from SetIdentity (Security and identity rule 2: a second binding is
// filter.ErrSecondBinding), response and upstream from SetResponse, and
// duration from SetFinal.
//
// # Upstream legs
//
// Leg returns a fresh handle to a pooled leg state for one upstream leg:
// the client request read-only, with its own Leg, outgoing request and
// response messages, Vars copy (upstream, attempt, response and the
// outgoing request view replaced), PolicyState slots, spec.when decisions,
// buffer account and a copy of the State Store budget, so concurrent legs
// of an aggregate composition share nothing mutable. Release merges the
// leg's records into the request and returns the leg's memory to its pool.
// The memory is reused, the handle never is: once released, by Release or
// by Pool.Put for a leg still live then, the handle and every view and
// message taken from it do nothing, so a second or stale Release, or any
// call through a stale handle, never reaches the memory's next owner, and
// a leg released after its request was put back merges nothing into it.
// Handles come from pooled chunks of handleChunk, so a fresh one per leg
// costs no allocation per request. onUpstreamResponseHeaders
// also runs for an attempt that got no response: CEL response is null and
// error.kind is the attempt's error kind (filter.Leg.ErrorKind), and the
// message is an empty response whose header writes a new attempt clears.
//
// # Bodies
//
// Bodies follow docs/architecture/02-configuration-model.md "Body buffering
// and limits". The request body is the request's body.Request: gated by
// PrepareBody and readable in onRequestBody, onRoute and, as the input of
// every attempt, in onUpstreamRequest. The client response is gated within
// limits.maxResponseBodyBytes in onResponse, and a leg response in
// onUpstreamResponseBody within the leg's filter.Leg.MaxBodyBytes (a
// composition step's maxBodyBytes; 0 means limits.maxResponseBodyBytes),
// where the gated body replaces the leg's response body and keeps its
// buffer until that body is closed. SetBody reserves the
// new body from limits.maxBufferedBytes, sets Content-Length, removes
// Content-Encoding and the body digests, and invalidates the decoded body
// and its CEL view. A body is decoded once per version through the Decoder,
// charged to the request (or leg) at the size the decoder builds, and fails
// as filter.ErrTooLarge past filter.DecodedLimitFactor times the raw limit
// it arrived under; a body that is empty, not application/json or +json, or
// not valid JSON is not JSON: Decoded returns nil and CEL reads null. The
// request body's decoded view is shared with the legs, which read it
// concurrently, so it is built once under a lock.
//
// A Policy without a body need runs while the body streams on: the client
// bodies are gated only as the Route's BodyNeeds say, and in
// onUpstreamResponseBody a leg's response is gated for CEL only once a
// Filter read it, or for a Policy whose spec.when reads response.body, so a
// transform.response that edits only headers leaves it streaming
// (docs/architecture/02-configuration-model.md "Body buffering and
// limits").
//
// # Paths
//
// The client request message holds the forwarded path, CEL request.path:
// the normalized path until a transform.request set[] entry with target
// path rewrites it (filter.Message.SetPath), after which every plain
// upstreams leg sends it and later Policies and composition
// pathExpressions read it; Exchange.Path, the Router's match, the access
// record and /tap keep the client's path. A leg's request.path is its
// attempt's outgoing path, a composition step's own included, and a
// rewrite at Upstream scope changes only that attempt's URL.
//
// # Records
//
// RecordShortCircuit and RecordFailure feed the access record
// (docs/architecture/10-observability.md "Access logs"): short_circuit is
// the first Policy that responded, and failure_modes lists the undecided
// Policies with the failureMode applied, at most 8 (target), keeping one
// more so the writer flags the list as truncated. cache is the result the
// Response Cache recorded (filter.Exchange.SetCacheResult). The exchange,
// not the cache Filter, counts ruralz_cache_store_skipped_total when it
// refuses a tee over its limit (size_limit); a tee it started counts its
// own later stop. Leg states record their
// own and merge them at Release. They also decide Final.RejectedAfter for
// quota refunds (docs/architecture/09-traffic-management-and-resilience.md
// "Quotas": refunds after a later Filter rejection; cache hits are charged):
// a Policy later in request order ended the request in a request Phase with
// a short-circuit of status 400 or more, or failed there under closed. A
// leg of plain upstreams counts like the client, since its response is the
// request's; a composition step's leg does not, since its step's outcome is
// decided by the composition, and Final.Code tells the result.
package exchange
