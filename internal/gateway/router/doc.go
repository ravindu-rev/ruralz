// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package router is the Router of one listener
// (docs/architecture/03-data-plane.md "Router"): the snapshot.Router that
// maps a request to exactly one Route from pre-body data only.
//
// # Compiled structure
//
// Compile builds the Router from the Routes bound to the listener: a host
// table (routematch.HostTable: the exact host, then "*." suffixes longest
// first, then the Routes without hosts) with one path index per host
// entry. The index is a segment trie of exact paths, templates and
// prefixes, then the regex paths, then the Routes without a path. Compile
// adds the Routes in precedence order (routematch.Compare), so Match meets
// the candidates of a request in the total order of ranks 1 to 6 of
// "Precedence" whatever the order of its input. Compile also rejects what
// validation rejects for the listener: invalid criteria (RZ-CFG-005),
// identical criteria (RZ-CFG-023), and GraphQL and topic criteria, which
// need more than pre-body data (RZ-CFG-040).
//
// # Matching
//
// Match takes the normalized path (routematch.NormalizePath; the caller
// answers RZ-RT-017 before routing) and normalizes the host itself, which
// costs nothing for a normal host. Listener hostnames bound the listener:
// a host outside them reaches no Route. A candidate whose methods, headers
// or grpc criterion fails, or whose match.when is false, falls through to
// the next; none left is RZ-RT-001. A match.when runtime error ends
// matching with RZ-RT-006 and no fallthrough, so an erroring guard never
// diverts traffic to a less protected Route. match.when sees the
// candidate's template captures as request.pathParams; on return the
// activation holds the winner's captures, or none.
//
// Match allocates nothing when params has room for MaxParams captures,
// except to decode a percent-encoded capture, to join a repeated header
// for a regex criterion, and inside match.when. The caller records the
// ruralz.route.match span around it. A Router is immutable and safe for
// concurrent use.
package router
