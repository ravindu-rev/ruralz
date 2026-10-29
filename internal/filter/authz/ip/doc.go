// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package ip is the authz.ip Policy type: it allows or refuses a request in
// onRequestHeaders by CIDR on source.ip
// (docs/architecture/08-security-and-identity.md "IP filtering and GeoIP").
//
// # Lists
//
// config.allow and config.deny are sets of CIDRs; a bare address means /32
// or /128. Entries parse and match exactly as Gateway.spec.trustedProxies
// entries do (internal/clientaddr): a CIDR is masked, an IPv4-mapped IPv6
// prefix of at least 96 bits becomes its IPv4 prefix, a zone is refused,
// and the client address is unmapped before the lookup, so an
// IPv4-mapped IPv6 address matches as IPv4. A malformed entry is
// RZ-CFG-005, at Build and in Check, the offline check every binary runs;
// both lists empty is RZ-CFG-005 too.
//
// # Decision
//
// The address is source.ip after trusted-proxy and PROXY v2 handling
// (filter.Exchange.Source, Security and identity "Client address"). A deny
// match refuses; otherwise a non-empty allow list refuses every address
// outside it, and an empty one admits. A refusal is 403 RZ-AUTH-013 and
// is counted in ruralz_auth_decisions_total with its code, as an admit is
// without one. A request without a valid client address cannot be placed
// on either list, so the Policy cannot decide and fails closed with the
// authz class default, 403 RZ-AUTH-015 (authentication and authorization
// fail closed, Security and identity "Threat model and trust boundaries").
//
// Both lookups walk a per-family binary trie and allocate nothing, so an
// admitted request costs no allocation
// (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Per-stage latency budget": cors, authz.cel or authz.ip, 0 allocations).
// A Filter is immutable and safe for concurrent use.
package ip
