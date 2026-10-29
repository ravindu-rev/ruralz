// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package httpfield holds the RFC 9110 field rules shared by the headers and
// transform Policies, the request handler, the Upstream layer, the Response
// Cache and, from M2, the Plugin host, so that no two of them can drift.
//
// # Names and values
//
// [ValidName] and [CheckName] test a field name: an RFC 9110 token
// (1*tchar) of at most [MaxNameBytes] bytes. [ValidValue], [CheckValue] and
// [NormalizeValue] test a field value: no control byte but HTAB (CR, LF and
// NUL included), no leading or trailing SP or HTAB, and at most
// [MaxValueBytes] bytes, literal or computed (a computed value is trimmed
// first). A transform's query parameter edits take the same caps,
// [MaxQueryNameBytes] and [MaxQueryValueBytes]. The byte tables are
// constants; nothing here allocates on success.
//
// # Protected names
//
// [Protect] classifies the names the Node manages itself, which no Policy
// may write: pseudo-header names, host, the hop-by-hop fields,
// content-length, expect and the trace context fields traceparent and
// tracestate. The forwarding fields X-Forwarded-For, X-Forwarded-Proto,
// X-Forwarded-Host and Forwarded are deliberately unprotected.
//
// # Hop-by-hop fields
//
// [IsHopByHop] is the fixed hop-by-hop list (RFC 9110 section 7.6.1):
// Connection, Keep-Alive, Proxy-Connection, TE, Trailer, Transfer-Encoding,
// Upgrade, Proxy-Authenticate and Proxy-Authorization. [ConnectionOptions]
// lists the fields a Connection field names, and [StripHopByHop] removes
// both sets from a message before it is forwarded, dropping Expect and
// keeping "TE: trailers" toward Upstreams. Its cost is linear in the header
// size however long a client's Connection field is, and keys match without
// regard to ASCII case.
//
// A Connection field names the fields present when the strip runs, so a
// client could name a field a Policy sets for the Upstream. The request
// handler therefore calls [StripConnectionOptions] at admission, before the
// request Phases run: it removes Connection and the end-to-end fields it
// names and leaves the fixed fields, TE included, to [StripHopByHop] at leg
// build. An Upstream response is stripped before the response Phases run.
//
// # Query editor
//
// [Query] edits a raw query string without reordering or re-encoding the
// pairs it does not touch: keys compare after percent-decoding and "+" to
// space, "&" separates pairs and ";" is data. url.Values is not used
// because its Encode reorders.
//
// The package imports nothing outside the standard library.
package httpfield
