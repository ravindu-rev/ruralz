// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package composition is the composition engine of a Node
// (docs/architecture/03-data-plane.md "Composition engine"): the
// snapshot.Forwarder of a Route with composition instead of upstreams,
// which calls several Upstreams for one request. One step executor runs
// every mode; each step is one upstream leg (upstream.Upstream.Leg) with
// its Upstream's leg Policies, retries and breaker, under the Route
// timeout.
//
// # Compile
//
// Engine.Compile builds a Route's Composition for one snapshot off the
// request path: each step's Upstream, its method, its literal path (split
// at the first "?" and checked with the hardening of the client path,
// routematch.CheckForwardPath) or its compiled pathExpression, its when,
// its maxBodyBytes (explicit, else an equal share of what
// limits.maxResponseBodyBytes leaves), its target (jsonval.ParseDotPath),
// select, rename, collection and group. It refuses, with the codes
// validation reports first, a composition validation would refuse: more
// steps than limits.maxCompositionSteps (RZ-CFG-031), step bodies that do
// not fit limits.maxResponseBodyBytes (RZ-CFG-032), an Upstream the
// snapshot does not hold (RZ-CFG-009), a when on an aggregate step, a
// literal path or target that breaks its syntax, or collection without
// group (RZ-CFG-005), and an expression that does not compile (RZ-CFG-014
// or RZ-CFG-015).
//
// A step gates its response body, reading it whole within maxBodyBytes,
// when it merges (every aggregate step, and a step that sets target,
// select, rename or group) or when a later step's expression reads its
// body through steps (expr.Refs.StepBodies; a computed key gates every
// earlier step). Composition.RequestGate tells the snapshot compiler
// whether the Route gates the client request body: when two or more steps
// may send it in one request, since a streamed body is sent once (a
// conditional composition runs one step), or when an expression reads
// request.body (docs/architecture/02-configuration-model.md "Body
// buffering and limits").
//
// # Steps
//
// A step's method is its own, else the incoming one; a step whose method
// is the incoming one sends the client body and any other sends none. Its
// target is its path or pathExpression result: the first "?" starts the
// raw query, sent as it is, and without one the client's raw query (after
// the request Phases) is forwarded. A computed path that is not a
// hardened path, or a computed query holding a raw control byte, fails
// the step with RZ-RT-015. A step's when and pathExpression evaluate over
// a copy of the client's Base variables under the Route deadline
// snapshot.Outbound.Deadline, at which a tracked comprehension stops
// (expr.Vars.Deadline; docs/architecture/02-configuration-model.md
// "Limits").
//
// A step fails on a leg without a response (its RZ-UP code, or RZ-RT-007
// for a step started past the Route deadline), a CEL runtime error, a
// refused path, a declared Content-Length or a body over its maxBodyBytes,
// a body the merge rules cannot take (RZ-RT-015), a spent buffer budget
// (RZ-RT-004), a recovered fault (RZ-RT-015), or a non-2xx result of a
// merged or non-final step: an Upstream's fails it with RZ-UP-011, a leg
// Filter's generated response with that response's status and code. A
// body that fails while it is read whole is an Upstream error: RZ-UP-003
// when a deadline ended it, else RZ-UP-004.
//
// # Modes
//
// aggregate runs every step at once, one goroutine each, after taking one
// in-flight unit per step (admission.Units; a full ceiling is 503
// RZ-RT-005 before any step starts). A failed step that is not optional
// cancels its siblings through their shared context; the failures those
// cancellations cause report nothing, and among the others the first in
// time decides, ties going to list order. The response merges every body.
//
// sequential runs the steps in list order, skipping a step whose when is
// false; later steps read earlier results through the CEL variable steps.
// A step that got a response joins steps whatever its status, so a later
// when can test it; when a later step runs, the earlier one is non-final,
// and a non-2xx result of a step that merges nothing fails it then. The
// response is the last executed step's own, streamed unless that step
// merges; then every executed step that merges merges, in list order.
//
// conditional runs the first step whose when is true; none true is 404
// RZ-RT-010, as a sequential composition that ran no step is. The
// response is that step's own, streamed, unless it merges.
//
// A failed optional step is left out and the response carries
// ruralz-partial: true; an optional step whose when errs counts as false.
// A final step that streams answers its own failure even when optional.
// A request failure is an *Error carrying its code, or, for a step that
// failed with a generated response, that response.
//
// # Merge rules
//
// A merged body is decoded under the Transform Policies rule (a body that
// is empty, not valid JSON or not JSON by its one Content-Type field line
// fails the step) within filter.DecodedLimitFactor times maxBodyBytes,
// charged to limits.maxBufferedBytes. target unwraps a nested value: a
// missing value or a path through a scalar fails the step, and a "*"
// segment collects, as a transform read does, the array of the matches,
// over a missing array none. With collection the value must be an array
// of objects, else an object; select keeps the named members and rename
// renames them; the value then merges under group, or member by member at
// the top level. When two steps write one key, the later in list order
// wins and the key keeps the position of its first write; members are
// written in step list order, then in each body's order.
//
// The merged response has status 200, Content-Type: application/json,
// Content-Length, ruralz-partial: true when a step was left out, and the
// most restrictive Cache-Control and Vary of the steps it merges (RFC 9111
// freshness less each step's current age); no other step field reaches
// it. The merged body is capped by limits.maxResponseBodyBytes: a larger
// one fails the request with 502 RZ-RT-015. The access log's upstream,
// endpoint and attempts describe the last step in list order whose leg
// ran (snapshot.UpstreamResponse, Error).
//
// # Bodies and resources
//
// The final step that streams keeps its leg's body, cut where it passes
// maxBodyBytes with an error carrying RZ-UP-009; every other step body is
// gated through body.ReadGate and released when the request no longer
// needs it. The merged body holds its reservation until it is closed.
// Times come from the injected clock, so tests run on clocktest.Fake; the
// engine starts goroutines only for aggregate steps, and joins every one
// before Forward returns.
package composition
