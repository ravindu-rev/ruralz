// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package header is the headers Policy type: it removes, sets and adds
// request and response header fields
// (docs/architecture/02-configuration-model.md "Policy",
// docs/architecture/03-data-plane.md "Headers Policy"). It is a
// transform-class Filter whose failureMode defaults to closed and may be
// open, and it never calls the State Store.
//
// # Phases
//
// config.request edits the request: the client request in onRequestHeaders
// at Gateway and Route scope, each attempt's outgoing request in
// onUpstreamRequest at Upstream scope. config.response edits the
// response: the client response in onResponse at Gateway and Route scope,
// generated responses included, so a 401 carries the security headers;
// each attempt's response in onUpstreamResponseHeaders at Upstream scope.
// An attempt that got no response is left alone, since its message is
// empty and edits reach nothing. The registry subscribes a Policy only to
// the Phases of the sides that hold an entry.
//
// # Operations
//
// A side applies remove, then set, then add. remove deletes every line of
// each named field, set replaces every line of each named field with one
// line, and add appends one line after the field's lines, a line set by
// the same side included. Names compare without regard to ASCII case and
// are written in canonical form (http.CanonicalHeaderKey). A header map
// can hold one field under keys that differ in case, which net/http never
// produces; before editing, the lines of every field the side names are
// folded into its canonical key, the canonical key's lines first, then the
// other keys' in ascending byte order of the key, so remove leaves no line
// behind and add appends after every line.
//
// Every valueExpression of the side is evaluated before the first edit,
// over the message as it reached the Policy, so no entry reads another's
// output and a failure leaves the message as it was. A computed value is
// trimmed of SP and HTAB and must then hold no control byte but HTAB and
// at most httpfield.MaxComputedValueBytes bytes (target); an empty value
// sets an empty field. A CEL runtime error or an invalid computed value
// cannot decide: under closed, 503 RZ-RT-011 in a request Phase and 502
// RZ-RT-012 in a response Phase; under open the Policy is skipped.
//
// # Configuration
//
// Each finding below is RZ-CFG-005, in Build and in Check, the offline
// check every binary runs: a name that is not a field name
// (httpfield.CheckName) or that the Node manages (httpfield.Protect: host,
// content-length, expect, the hop-by-hop fields, traceparent and
// tracestate); two entries of one list whose names are equal ignoring
// case; a literal value httpfield.CheckValue refuses; an entry with neither
// a value nor a non-empty valueExpression; and more than MaxEntries
// entries across all lists of the Policy. Check leaves to the schema the
// findings it reports itself: an entry with both a value and a
// valueExpression, and two entries of a set or remove list whose names are
// byte for byte equal. The schema also refuses an empty valueExpression
// (minLength 1), which Check still reports as a backstop. Build also
// compiles every valueExpression at its
// site through BuildEnv.CEL: RZ-CFG-014 or RZ-CFG-015, as stage J reports
// them.
//
// # Cost
//
// Literal values are field-value slices built once and shared between
// requests, with length equal to capacity, so a later append copies them;
// nothing writes into them in place, which no http.Header method does.
// When every key of the message is canonical, a side of literal values
// allocates nothing, the computed values of one call share one allocation,
// and add to a field that has lines allocates its longer line slice
// (docs/architecture/12-performance-budgets-and-benchmarking.md "Per-stage
// latency budget": headers setting two values in onUpstreamRequest, 2
// allocations). A Filter is immutable and safe for concurrent use.
package header
