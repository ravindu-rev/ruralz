// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package clientaddr computes the client address of a request: the PROXY
// protocol v2 reader and listener wrapper, the trusted-proxy resolution of
// source.ip from Forwarded and X-Forwarded-For, and the forwarding-header
// rewrite toward Upstreams (R-11: the one package for 04 reqs 16-18 and 06
// reqs 60-63; OQ-security-and-identity-6 (c): Gateway.spec.trustedProxies
// and listeners[].proxyProtocol together; docs/architecture/08-security-
// and-identity.md "IP filtering and GeoIP", Client address).
//
// The package is a leaf: it imports only the standard library, starts no
// goroutine and keeps no state beyond the values it returns.
//
// # PROXY protocol v2
//
// [ReadHeader] reads exactly one v2 header from a reader, never a byte past
// it, so the next byte is the TLS ClientHello or the HTTP request line (04
// req 16, 06 req 61). It accepts version 2 with the PROXY command and the
// TCP4 or TCP6 family, or with the LOCAL command and any family byte, whose
// address block it skips (R-65, overriding the literal 04 req 16, as the
// PROXY v2 text has receivers discard it). It reads and discards TLVs
// without parsing them, and rejects everything else with an error wrapping
// [ErrMalformed]: a missing or wrong signature (a v1 text header included),
// another version or command, a PROXY header with another family or
// transport (UDP, UNIX, AF_UNSPEC), a variable part above [MaxVariableLen]
// bytes or too short for its addresses, and a truncated header.
//
// [NewListener] wraps a net.Listener for a listener with proxyProtocol:
// true. Accept never reads: each [Conn] reads its header on its first Read
// (or Write), on the goroutine serving the connection, within the header
// timeout (10 s by default, ReadHeaderTimeout). A connection whose header is
// missing, malformed or late is closed, reported once to
// [ListenerOptions].OnRefused (ruralz_listener_connections_total
// {result="refused"}), and its reads fail with an error net/http treats as a
// common read error, so nothing is written back. After a PROXY header,
// [Conn.Peer] and [Conn.RemoteAddr] return the header's source; after LOCAL
// they return the TCP peer. net/http records Request.RemoteAddr before the
// first Read, so request handlers take the peer from [PeerOf] on the
// connection instead.
//
// # Trusted-proxy resolution
//
// [Trusted] is the compiled Gateway.spec.trustedProxies set (a per-family
// binary trie, O(address bits), no allocation). [Trusted.Resolve] computes
// source.ip once per request before routing (04 req 17, 06 reqs 60 and 62):
//
//   - The peer is the PROXY v2 source on opted-in listeners, else the TCP
//     peer. IPv4-mapped IPv6 addresses are unmapped and zones stripped,
//     here and in every header entry, so a trusted IPv4 prefix matches an
//     IPv4 client reached over a dual-stack socket.
//   - An untrusted peer is the client: source.ip is the peer address and
//     source.port the peer port. Forwarding headers are ignored.
//   - Behind a trusted peer, Forwarded is used when it has any content,
//     else X-Forwarded-For. Field lines are concatenated in order and the
//     entries walked right to left, examining at most [MaxEntries] (16)
//     entries. The first parsable address outside the trusted set is the
//     client. When every parsed entry is trusted, the leftmost parsed one
//     is used. An unparsable entry (unknown, an obfuscated _identifier, a
//     Forwarded element without exactly one for parameter, malformed
//     syntax) ends the walk and the last parsed address, the one to its
//     right, is used, or the peer when none was parsed. Empty list elements
//     are skipped (RFC 9110 section 5.6.1) but count toward the 16. Ports
//     in entries are ignored, and source.port is 0 for an address taken
//     from a header.
//
// Forwarded elements are delimited from the right end of each field line,
// treating a '"' preceded by an odd run of backslashes as a quoted-pair.
// Only the leftmost element of a line can be left inside an unterminated
// quoted string, which makes it unparsable, so an element a trusted proxy
// appended to a client's malformed text (or an intermediary merged into
// the same line) keeps its boundaries and is still found.
//
// Within a header the trusted proxies manage, a client's own entries sit
// left of the address the first trusted proxy recorded, so the walk stops
// there, and an unparsable entry the walk does reach collapses the result
// to a proxy address rather than to a client-supplied one (04 section 9
// risk 8). That does not hold for a header a trusted proxy passes through
// untouched. Because Forwarded wins whenever it has content (06 req 60,
// proposed), a client behind a proxy that manages only X-Forwarded-For,
// the default of many load balancers and reverse proxies, chooses
// source.ip outright with its own Forwarded field ("for=10.0.0.5"), or
// collapses it to the proxy with one that has no usable for parameter
// ("proto=https", ","). Operators of such proxies must make them remove or
// overwrite a client's Forwarded field.
//
// # Forwarding headers toward Upstreams
//
// [Rewrite] (and [Trusted.Rewrite]) builds the forwarding fields of an
// outgoing Upstream request (04 req 18, 06 req 63, R-28). From an untrusted
// peer the fields are overwritten, never appended: X-Forwarded-For becomes
// the peer address, X-Forwarded-Proto the listener scheme, X-Forwarded-Host
// the client authority, and Forwarded is removed. From a trusted peer the
// existing X-Forwarded-For lines are kept, joined, with ", <peer>"
// appended, and X-Forwarded-Proto, X-Forwarded-Host and Forwarded are kept
// when present (the first two are set when absent). Case variants of these
// field names that bypassed canonicalization are folded into the canonical
// field, so none can slip past the rule. The caller applies Rewrite once
// per attempt to a fresh copy of the client header, after removing the
// hop-by-hop fields and the fields Connection lists (otherwise a client's
// "Connection: X-Forwarded-For" would strip the field Rewrite set).
package clientaddr
