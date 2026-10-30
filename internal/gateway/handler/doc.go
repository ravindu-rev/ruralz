// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package handler is the request handler of Ruralz Gateway
// (docs/architecture/03-data-plane.md): the http.Handler every client
// listener's Server shards share, which takes a request from its admission
// to its last onLog call. The ruralzd wiring builds one Handler with New
// and passes it to the listener Set (listener.Options.Handler).
//
// # Admission
//
// ServeHTTP first takes one in-flight unit (admission.Units). A full
// ceiling answers 503 RZ-RT-005 at once through the pre-routing limiter,
// before any snapshot is pinned, with no access record and no /tap event,
// so overload adds no log work; the response still counts in the listener
// and Node metrics and under route _unmatched. The request then pins the
// published snapshot (retire.Holder.Pin) with its pooled
// snapshot.PinnedRequest, whose Cancel cancels the request's root context
// and whose RC moves its connection deadlines, and keeps the pin until
// onLog is done. A request that finds no published snapshot, possible
// only around shutdown, gets 503 RZ-RT-016; one the ending protocol ends
// as it pins, after the Drain deadline, gets 503 RZ-RT-016 without any
// work. Body reads and response writes get 10 s deadlines at admission
// (AdmissionWindow) and the Route timeout once the Route is fixed, set
// through http.ResponseController under rec.Mu and never over deadlines
// the ending protocol moved. The pin and metric stripe is the
// connection's, offset by its request sequence.
//
// # Request hardening and routing
//
// Before routing, in order: CONNECT is 404 RZ-RT-001; a header block over
// the snapshot's limits.maxRequestHeaderBytes or MaxFieldLines is 431
// RZ-RT-002, counted as RFC 9113's field-list size for every protocol, the
// request target included; conflicting framing, an absolute-form target
// whose scheme is not the listener's, and an HTTP/2 Host naming another
// host than :authority are 400 RZ-RT-017; a path NormalizePath refuses is
// 400 RZ-RT-017, and OPTIONS * is 404 RZ-RT-001 (the listener Servers set
// DisableGeneralOptionsHandler). The Connection field and the end-to-end
// fields it names are then stripped (httpfield.StripConnectionOptions).
// The listener's Router fixes the Route inside a ruralz.route.match span:
// none is 404 RZ-RT-001, a match.when runtime error 500 RZ-RT-006. These
// pre-routing responses run no Filter Chain, take a 5 s write deadline
// from the pre-routing limiter, which aborts without writing past 2,000 at
// once, and still get an access record and a /tap event.
//
// # The matched Route
//
// The Route timeout starts at the request start: on the handler's clock it
// cancels the root context with context.DeadlineExceeded, which ends State
// Store calls, legs and body reads, and an Upstream attempt it cuts counts
// as timed out. M1 serves Upstream protocol http only; a Route with
// another is an internal fault, 500 RZ-RT-025. A WebSocket upgrade is 400
// RZ-RT-023 after the match and before onRequestHeaders, and the response
// Phases run on it as on any generated response. Otherwise the executor
// runs onRequestHeaders; the request body is prepared
// (exchange.Request.PrepareBody: a gated body read whole as client I/O, a
// declared or read length over limits.maxRequestBodyBytes 413 RZ-RT-003, a
// spent buffer budget 503 RZ-RT-004, a body that cannot be read aborts the
// request); onRequestBody and onRoute run; then the Route's Forwarder
// (plain upstreams or composition) runs with the executor's leg hooks, the
// request's sampling decision, the Route deadline, the client hop's Via
// entry, the client's request trailers and, on a Route with upstreams for
// an HTTP/1.1 or later client, the 103 Early Hints relay; a composition
// keeps Ruralz-Partial on the response it builds when an optional step
// was left out. A streamed body cut at its limit
// while a leg sent it is 413 RZ-RT-003. A leg failure becomes the problem
// document of its RZ code; one with an RZ-UP code is an Upstream failure,
// on which the Response Cache may still serve stale-if-error. A Route that
// gates its response reads the Upstream body whole before onResponse: over
// limits.maxResponseBodyBytes 502 RZ-UP-010, a spent budget 503 RZ-RT-004,
// a body a deadline ended 504 RZ-UP-003, another failed read 502
// RZ-UP-004. onResponse runs on the response (executor.Run.Response).
//
// # The response
//
// The handler commits the response onResponse left under the request's
// wmu, which orders it against 103 relays, and rec.Mu, so the ending
// protocol sees the commit: a request it ended first gets 503 with its end
// code (RZ-RT-014 or RZ-RT-016) instead. The header section takes, after
// onResponse, the RateLimit-Policy and RateLimit fields the ratelimit and
// quota Policies recorded; the Node's Date on every generated response
// and on a forwarded one without its own; no sniffed Content-Type. A
// generated error carries the RFC 9457 problem document of its code with
// the request's trace ID as requestId; a HEAD request, 1xx, 204 and 304
// get the header section only. A buffered body is written whole; an
// Upstream body is copied as it arrives through one 32 KiB buffer
// (CopyBufferBytes), feeding the Response Cache tees, flushed after every
// read when it has no Content-Length or is an event stream; client writes
// are outside gateway-added time. The Upstream's declared trailer fields
// are announced before commit (chunked on HTTP/1.1) and every forwardable
// trailer field follows the body; an HTTP/1.0 client gets none. An
// Upstream body that fails after commit, or a streamed request body that
// passes its limit after commit, aborts the request with
// http.ErrAbortHandler, which resets the HTTP/2 stream or closes the
// HTTP/1.1 connection, so a cut body never reads as whole; its code is the
// read's or RZ-UP-009.
//
// # Early Hints
//
// The handler is the snapshot.EarlyHints of a plain upstreams leg: each
// request has its own token, so a 103 the transport delivers after the
// attempt, or after the request, writes nothing. A relayed 103 carries the
// Upstream's fields without hop-by-hop ones, which leave the header map
// again after it, and nothing is relayed once the final response is
// committed.
//
// # onLog, the access record and /tap
//
// After the response, the handler records the outcome (Final: status, 0
// when the client went away before any response, code, committed) and
// runs onLog and every Finish on a context the client's departure does
// not cancel. It counts ruralz_http_requests_total and
// ruralz_http_request_duration_seconds on the Route or _unmatched,
// ruralz_http_listener_requests_total with the response's origin,
// ruralz_http_node_responses_total for a Node-generated response with a
// code, ruralz_http_gateway_duration_seconds (gateway-added time to the
// last byte, or to header commit for an event stream), the body sizes and
// ruralz_http_active_requests; a request without a status (the client left
// first) counts in no status family. Gateway.spec.telemetry.accessLog.when
// selects the access record over the onLog variables; a runtime error
// writes it and counts ruralz_telemetry_access_log_cel_errors_total. The
// record holds the handler's fields beside the exchange's
// (exchange.Request.FillAccess): span, Revision, protocol, status, code,
// durations (upstream_duration from the Upstream layer's sections), body
// sizes, client address, TLS version and the last leg. /tap gets the
// exchange, redacted with the Revision's credential headers, while a
// subscriber listens.
//
// # Faults
//
// A panic in request handling outside the Filter Chain is recovered: it is
// logged at error with its stack and the trace ID, the pin, the in-flight
// unit and the pooled state are released, and the request answers 500
// RZ-RT-025 when nothing was written, else aborts with
// http.ErrAbortHandler. The 500 counts in the Route, listener and Node
// response metrics and ends the server span; onLog, the access record and
// /tap do not run on the state a fault left.
//
// # Statuses
//
// A Node-generated response's status comes from the code registry, read
// once at New into a table: errcode.Status builds the registry per call,
// which the request path does not pay.
//
// Times come from the injected clock, which ruralzd sets to the process
// clock: the request start, the Route deadline the Upstream layer compares
// with its own process clock, the deadline timer, durations and the Date
// field share it, so in the controllable-clock test build they all move
// only on the harness's commands. The connection's read and write
// deadlines, which the Go runtime enforces, are dated on the injected wall
// clock instead, at the same distance from the request start
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope"); tests drive the handler with
// internal/gateway/handler/handlertest.
package handler
