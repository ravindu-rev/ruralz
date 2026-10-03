// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package routematch holds the pure Route match primitives that the Router
// (internal/gateway/router), the request handler, configuration validation
// (internal/config/validate) and the CLI share (R-35; spec 04 sections C and
// D; docs/architecture/03-data-plane.md "Router").
//
// The package is a leaf: it imports only internal/errcode,
// pkg/config/v1alpha1 (for [CriteriaOf], R-68) and the standard library,
// allocates nothing on the request path for already-normal input, and
// keeps no state.
//
// # Request normalization
//
// [NormalizePath] turns the escaped request path ([RequestPath] of r.URL, the
// path as received; see there why r.URL.EscapedPath() alone loses %2F) into
// the one normalized path that the Router, CEL request.path, authz Filters,
// the access log, /tap and the forwarded request all use (04 req 23; SEC
// "Request hardening"). It rejects an encoded NUL, a raw or encoded
// backslash, any other raw or encoded control byte and invalid percent
// escapes with [ErrRejected], which carries RZ-RT-017
// (OQ-security-and-identity-21 (a)); decodes percent-escaped unreserved
// characters; upper-cases the hex digits of every other escape; keeps %2F
// encoded so it never splits a segment; removes dot segments (RFC 3986
// section 5.2.4); and keeps repeated and trailing slashes. The asterisk-form
// target "*" matches no Route: its error is [ErrAsteriskForm], which carries
// RZ-RT-001 (404), and the caller answers CONNECT the same way before
// normalizing. [NormalizeHost] lower-cases a request host, strips its port
// and one trailing dot, and gives an IPv6 literal its canonical text (04 req
// 24).
//
// # Host rules
//
// [ParseHostPattern] validates a match.hosts or listener hostnames entry: an
// exact host, or one leading "*." label (OQ-data-plane-2 (a)); any other "*"
// is RZ-CFG-005. "*.shop.example" matches one or more leading labels, never
// the bare suffix (04 req 27). [HostTable] is the exact, wildcard (longest
// suffix first) and any-host lookup the Router compiles per listener.
//
// # Paths and templates
//
// [ParseTemplate] and [CheckTemplate] enforce the template grammar at
// validation (04 req 29, RZ-CFG-005). [PrefixMatch] matches prefixes on
// segment boundaries (04 req 28). [Trie] is the segment trie for exact paths
// and templates; its lookup yields exact entries first, then templates with a
// literal segment before a parameter, which is precedence ranks 2 and 3.
// [NextSegment] and [DecodeParam] are the zero-allocation segment helpers.
// [CompileRegex] anchors path and header expressions to the whole input.
//
// # Candidate filters
//
// [MethodMatch], [JoinHeader] and [HeaderMatcher] are the methods and
// headers rules of 04 req 25 and 30: exact case-sensitive methods (HEAD does
// not imply GET), case-insensitive header names, exact and regex criteria
// against the joined field value, and present true or false.
//
// # Identity and precedence
//
// [MatchKey] is the identity of a Route's match criteria: equal keys are
// RZ-CFG-023 (04 req 31). [Compare] orders [Rank] values by the six
// precedence ranks, a total order (04 req 31).
//
// [MatchKey] takes [Criteria], a field-for-field mirror of
// v1alpha1.RouteMatch, so the key stays independent of the API types; the
// Router and validation build it with [CriteriaOf] (R-68, which overrides
// spec 04 section 3's MatchKey(*v1alpha1.RouteMatch)).
package routematch
