// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package sfv serializes and parses RFC 9651 Structured Field Values for
// HTTP: Lists, Inner Lists, Items, Parameters and the eight bare item types
// (Integer, Decimal, String, Token, Byte Sequence, Boolean, Date and Display
// String).
//
// Its data-plane use is the RateLimit-Policy and RateLimit response fields
// of draft-ietf-httpapi-ratelimit-headers-11: the request handler appends
// one List member per applied limit with [AppendRateLimitPolicy] and
// [AppendRateLimit] after onResponse, from the limits Filters recorded
// through filter.Exchange.AddRateLimitField. Both helpers write into a
// caller-owned buffer and allocate nothing.
//
// The general serializer ([AppendList], [AppendMember], [AppendItem],
// [AppendBareItem], [AppendParams]) follows RFC 9651 section 4.1 and fails,
// leaving dst as it was, on input the grammar cannot express. The parser
// ([ParseList], [ParseItem]) follows section 4.2; it serves tests and the
// fuzz oracle that every serialized field reparses to the same value.
//
// The package imports nothing outside the standard library.
package sfv
