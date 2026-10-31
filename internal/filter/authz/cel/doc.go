// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package cel is the authz.cel Policy type: it authorizes a request with
// one CEL rule over the base variables request, source, route, consumer,
// auth and now (docs/architecture/08-security-and-identity.md
// "Authorization", ADR-0019). It is an authz-class Filter, failureMode
// closed only, and never calls the State Store.
//
// # Phase
//
// The rule runs once per request: in onRequestBody when it reads
// request.body, in onRequestHeaders otherwise, as the configuration
// registry selects the Phase (BuildEnv.Phases). onRequestBody makes the
// request body a gate, so before a body rule runs the Filter reads the
// decoded body: a body decoded past filter.DecodedLimitFactor times its raw
// limit is filter.ErrTooLarge (413 RZ-RT-003) and a refused buffer
// reservation filter.ErrBudget (503 RZ-RT-004), whatever the rule says
// (docs/architecture/02-configuration-model.md "Body buffering and
// limits"). A body that is not JSON reads as null.
//
// # Decision
//
// true admits the request; false denies it with 403 RZ-AUTH-010; a
// runtime error cannot decide, which failureMode closed answers with 403
// RZ-AUTH-015. Runtime errors include both stops of internal/cel: the cost
// limit, and for a tracked comprehension the smaller of the request
// deadline and 50 ms, since the rule is evaluated under the request's
// context. Each decision is counted in ruralz_auth_decisions_total with its
// code by the embedded auth.Decider; the body failures above are no
// decision and are not counted there. Rejections never echo request data.
//
// # Configuration
//
// Build compiles config.rule through BuildEnv.CEL at the Policy's site: a
// syntax, type or unavailable-variable error is RZ-CFG-014 and an estimate
// over the cost ceiling RZ-CFG-015, as stage J reports them. An empty rule
// is RZ-CFG-005: the schema refuses it (minLength 1), and Build and Check,
// the offline check every binary runs, refuse it again as a backstop.
// Build also refuses a placement where the rule would never decide,
// so a misplaced Policy cannot admit every request: a rule that reads
// request.body without onRequestBody (RZ-CFG-014), and a Phase set holding
// neither onRequestHeaders nor onRequestBody, as at Upstream scope
// (RZ-CFG-020). Programs are shared through the snapshot's ProgramSet, so
// nothing is carried over from BuildEnv.Previous.
//
// # Cost
//
// Handle allocates nothing beyond the program's own evaluation
// (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Per-stage latency budget": authz.cel, 0 allocations). A Filter is
// immutable and safe for concurrent use.
package cel
