// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package cors is the cors Policy type: the CORS protocol of the Fetch
// standard answered by the Node (docs/architecture/03-data-plane.md
// "CORS", docs/architecture/02-configuration-model.md "Policy"). Its
// Filter class, cors, runs first in a Phase, so a preflight is answered
// before any auth-class Policy runs. It subscribes to onRequestHeaders and
// onResponse and never calls the State Store; with no dependencies and no
// CEL field of its own it always decides.
//
// # Requests
//
// A request without an Origin field is not a CORS request: it passes on.
//
// A CORS-preflight request is an OPTIONS request with Origin and
// Access-Control-Request-Method; the Router matches it to a Route by the
// requested method or by OPTIONS. The Policy answers it in
// onRequestHeaders. When the origin is allowed, the requested method is
// in allowMethods and every field name of Access-Control-Request-Headers
// is in allowHeaders (any field name when allowHeaders holds "*"), the
// answer is 204 with Access-Control-Allow-Origin, Access-Control-Allow-
// Methods (allowMethods), Access-Control-Allow-Headers (allowHeaders, when
// set; with "*", "*" followed by the requested Authorization name, since
// a browser never lets "*" cover Authorization),
// Access-Control-Allow-Credentials (when allowCredentials),
// Access-Control-Max-Age (maxAge in whole seconds, truncated, when set)
// and Vary: Origin, Access-Control-Request-Method,
// Access-Control-Request-Headers. Otherwise the answer is 403 RZ-RT-008,
// "CORS preflight rejected", without CORS fields
// (docs/architecture/03-data-plane.md "RZ-RT registry"). A browser lists
// only CORS-unsafe request headers in Access-Control-Request-Headers, so
// without allowHeaders only requests whose headers are all CORS-safelisted
// pass a preflight.
//
// Any other request with an Origin field is an actual CORS request. It
// passes on, whatever its origin: the CORS protocol leaves the response to
// the browser, which withholds it from the page unless the response
// allows the origin. When the origin is allowed and the request method is
// in allowMethods, the response gets Access-Control-Allow-Origin, replacing
// any value the Upstream sent, Access-Control-Allow-Credentials when
// allowCredentials, and Access-Control-Expose-Headers (exposeHeaders, or
// "*" when it holds "*") when set; otherwise the Policy adds no CORS
// field.
//
// An origin is allowed when allowOrigins lists it, byte for byte, or holds
// "*". Access-Control-Allow-Origin is "*" when allowOrigins holds "*",
// else the request's origin. Methods match byte for byte, header names
// ignoring case. More than one Origin or Access-Control-Request-
// Method field line matches nothing.
//
// # Response side
//
// onResponse runs on every response of the Route, generated ones
// included, so a 401 of an auth Policy and the preflight answers carry the
// CORS fields too (docs/architecture/03-data-plane.md "Short-circuit"). It
// sets the fields of an allowed actual request, recorded in the Policy's
// per-request slot in onRequestHeaders, and adds Origin to Vary on every
// response, CORS request or not, unless Vary already lists Origin or "*":
// whether the fields are present depends on the request's Origin, so
// caches must keep the variants apart (Fetch, "CORS protocol and HTTP
// caches").
//
// The field values the Filter sets are slices it builds once and shares
// between requests, with length equal to capacity, so an append made by a
// later Filter copies them; nothing may write into them in place, which no
// http.Header method does. An allowed actual request and its response
// therefore allocate nothing, except to add Origin to a Vary field the
// response already has
// (docs/architecture/12-performance-budgets-and-benchmarking.md "Per-stage
// latency budget": cors, 0 allocations in onRequestHeaders).
//
// # Configuration
//
// Each finding below is RZ-CFG-005, in Build and in Check, the offline
// check every binary runs: an allowOrigins entry that is neither "*" nor a
// serialized origin (serializedOrigin: "null", or scheme://host[:port] in
// lower case without a default port or a path); an allowMethods,
// allowHeaders or exposeHeaders entry that is not an RFC 9110 token; and
// allowCredentials true with "*" or "null" in allowOrigins, or with "*"
// in allowHeaders or exposeHeaders, which browsers do not honor for
// credentialed requests. allowCredentials is false when absent.
package cors
