// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package transform compiles and runs the transform.request and
// transform.response Policies, which rewrite bodies, headers, the query
// string and the request path with CEL and regular expressions
// (docs/architecture/03-data-plane.md "Transform Policies",
// docs/architecture/02-configuration-model.md "Registered from feature
// documents"). The type packages, transform/request and
// transform/response, map their configs onto one Spec and use Build and
// Report; dotpath parses the dot paths and edits JSON documents. Both
// types are transform-class Filters whose failureMode defaults to closed
// and may be open, and they never call the State Store.
//
// # Phases and body need
//
// transform.request runs once in onRequestBody at Gateway and Route
// scope, on the client request, and in onUpstreamRequest at Upstream
// scope, on each attempt's outgoing request, whose body is the same
// buffered input on every attempt until a Filter sets that attempt's body.
// transform.response runs in onResponse at Gateway and Route scope, after
// any composition merge and never on a generated response (the executor
// skips it there), and in onUpstreamResponseBody at Upstream scope, on
// each leg's response.
//
// A config needs the body when it sets body, holds an arrayOps or replace
// entry or a set or remove entry with target body, or has a set
// valueExpression that reads request.body (transform.request) or
// response.body (transform.response); the registry decides the same
// (registry.Registry.TransformBody), so the snapshot compiler gates
// exactly the bodies a Filter reads. Only such a Filter reads the body
// (filter.Message.Body), before it evaluates an expression; any other runs
// while the body streams on.
//
// # Operations
//
// A call first evaluates every CEL field, over the message as it reached
// the Policy, so no entry reads another's output. It then applies body,
// then arrayOps, set, remove and replace, each list in order, and commits
// the result only when every step succeeded, so a failure edits nothing.
//
// body writes its result as the body: a top-level string as its UTF-8
// bytes, bytes as they are, any other value as JSON in the transform body
// form of the CEL module (map keys in ascending byte order, decoded
// numbers verbatim). It sets Content-Type to contentType, default
// application/json. Under a JSON contentType, application/json or a +json
// type, a string or bytes result must be one strict JSON value
// (dotpath.Editor.Valid).
//
// The body operations edit a JSON document in the transform form
// (dotpath): each object keeps its member order and each number its text,
// a new member goes to the end of its object, and the document is written
// compactly without HTML escaping. A set entry with target body writes its
// valueExpression's result as a nested JSON value (expr.NestedJSON). move
// takes the value at from, or the array of a * path's matches, out of the
// body and writes it at to, replacing what to held; append adds the value
// or each match at from to the end of the array at to, creating it when
// missing and keeping from; delete removes what from names, array elements
// letting later ones move up. A from that matches nothing moves or
// appends nothing. replace rewrites the strings at path, or the raw body,
// JSON or not, when path is absent, with an RE2 pattern whose replacement
// expands $1 and $name, or with literal text. The body is rewritten only
// when an operation changed it; then the message gets the new bytes
// (filter.Message.SetBody recomputes Content-Length and drops
// Content-Encoding and the body digests).
//
// A body is JSON when the message has exactly one Content-Type field line
// naming JSON and the bytes are strict JSON as CEL decodes a body: valid
// UTF-8, no duplicate member names, at most 64 levels deep; after body, by
// contentType and the bytes it wrote. A JSON operation on a body that is
// not JSON but not empty cannot decide (null_body), while header, query
// and path operations, replace without path and a body that ignores the
// body still apply. On an empty body remove, replace with path, move and
// delete do nothing, set and append start from an empty object, and a body
// they write takes Content-Type application/json.
//
// # Absent bodies and validators
//
// On a response that cannot have content, to a HEAD request or with
// status 1xx, 204 or 304, and on a 206 or a response with Content-Range,
// whose body is never rewritten, body, the set entries with target body
// and every body operation are skipped; header operations still apply. A
// response whose body a transform rewrote turns each strong ETag weak,
// prefixing W/, and loses Accept-Ranges.
//
// # Headers, query and path
//
// A set or remove entry with target header follows the headers Policy:
// names compare without regard to ASCII case and are written in canonical
// form, the lines a header map holds for one field under keys that differ
// only in case are folded into the canonical key before the edits, set
// leaves one field line and remove none, and a computed value is trimmed
// of SP and HTAB and must then hold no control byte but HTAB and at most
// httpfield.MaxComputedValueBytes bytes (invalid_value); an empty value
// sets an empty field. A query entry compares names after decoding, as
// request.query reads them: remove drops every occurrence and set replaces
// every occurrence with one parameter at the first one's place, else
// appends it, written percent-encoded per RFC 3986 (httpfield.Query); a
// computed value over httpfield.MaxQueryValueBytes is invalid_value. The
// query is committed only when it changed.
//
// A set entry with target path rewrites the forwarded path
// (filter.Message.SetPath) with its string result, which must start with
// "/", hold no "?" or "#", have no "." or ".." segment, also spelled with
// %2E, pass the hardening of the client path (routematch.NormalizePath)
// and be at most MaxPathBytes long, else invalid_value; the forwarded
// path is the result normalized as the Router normalizes a client path.
//
// # Configuration
//
// Each finding below is RZ-CFG-005, in Build and in Report, the offline
// check every binary runs: more than MaxEntries entries across the four
// lists; a contentType that is not an RFC 9110 media type a field value
// may hold; a header name that is not a field name (httpfield.CheckName)
// or that the Node manages (httpfield.Protect), or, in a Policy that
// rewrites the body, Content-Encoding or a body digest field; a query name
// that is not a field name; a body dot path dotpath.Parse refuses; a move
// or append without from or to, a delete without from or with to, and a
// to holding a * segment; a replace pattern over MaxPatternBytes bytes or,
// unless literal, not valid RE2; a set entry with target path beside a
// name, a second one, and a remove entry with target path; an empty
// valueExpression. Report leaves to the schema the findings it reports
// itself: a target or op outside its enum, and a pattern over
// MaxPatternBytes characters. Build also compiles every CEL field at its
// site through BuildEnv.CEL: RZ-CFG-014 or RZ-CFG-015, as stage J reports
// them.
//
// # Failures
//
// A CEL runtime error (cel_error, or result_type for a result of the wrong
// type or without a JSON form), an invalid result (invalid_value), a path
// error (path_error), a JSON operation on a body that is not JSON
// (null_body) or a body over its output cap (output_cap) cannot decide:
// under closed, 503 RZ-RT-011 in a request Phase and 502 RZ-RT-012 in a
// response Phase; under open the Policy is skipped and the message passes
// unchanged. The output cap is limits.maxRequestBodyBytes for
// transform.request and limits.maxResponseBodyBytes for
// transform.response, or a composition step's maxBodyBytes on its leg; it
// bounds every operation's output. A spent buffer budget, when the body is
// read, decoded for CEL or replaced, stays 503 RZ-RT-004 (filter.ErrBudget),
// and a body over its limit filter.ErrTooLarge.
//
// # Cost
//
// The operations edit over pooled buffers through a dotpath.Editor, and
// the Content-Type lines a Filter writes are built once and shared,
// read-only, between requests. A call of literal operations (remove,
// literal replace and arrayOps entries) allocates only the rewritten body
// the message keeps; a pattern replace allocates as the regexp package
// does once a string matches, a query edit its new query string, and CEL
// entries their results
// (docs/architecture/12-performance-budgets-and-benchmarking.md "Per-stage
// latency budget"). A Filter is immutable and safe for concurrent use.
package transform
