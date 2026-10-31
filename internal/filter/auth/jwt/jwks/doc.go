// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package jwks is the Node-wide JWKS manager of auth.jwt
// (docs/architecture/08-security-and-identity.md "JWT and OIDC"): it
// fetches each issuer's JSON Web Key Set from its https jwksUrl, keeps the
// usable public keys in memory and refreshes them. The Manager is a
// filter.Component: internal/filter/builtin constructs one per process,
// hands it to the auth.jwt Factory, and the ruralzd supervisor runs it.
//
// # Fetch
//
// Every fetch goes through the SSRF-guarded IdP client of internal/egress
// with the TLS configuration of tlsconf.IdentityProvider (TB-10, "Secrets"
// rule 6): https only, verified TLS, the destination checked at connect
// time and on each https redirect hop (at most MaxRedirects), no cookies,
// the body capped at MaxDocumentBytes and the whole fetch at FetchTimeout.
// At most MaxConcurrentFetches fetches run at once.
//
// # Keys
//
// NewKeys keeps only the keys a token may be verified with: public RSA
// keys of at least MinRSABits, ECDSA P-256 keys and Ed25519 keys, whose
// "use", when present, is "sig" and whose "key_ops", when present, include
// "verify". A key the identity.Revoker lists by (issuer, kid) is not
// loaded. A document is a JWK Set of at most MaxKeys entries; one without
// a usable key is a failed fetch. Keys are looked up by kid and family
// (Lookup), and the first of two keys sharing both wins; a token without
// a kid tries every key of its family in document order (OfFamily), so
// never more than MaxKeys.
//
// # Lifetime and refresh
//
// A key set lives for its response's Cache-Control max-age clamped to
// MinLifetime..MaxLifetime, or DefaultLifetime when max-age is absent or
// no-cache is present (Lifetime), and is refreshed at half its lifetime. A
// failed fetch keeps the last key set until it expires (the issuer then
// has no usable keys, RZ-AUTH-006), raises the degraded state jwks_stale
// for the issuer until a fetch succeeds, and is retried after
// RetryInterval or, when the response carried Retry-After, no earlier than
// that, at most MaxRetryAfter later. A token with an unknown kid triggers
// at most one fetch per issuer per RefreshInterval (Handle.Nudge), again
// honoring Retry-After.
//
// # Sharing and prefetch
//
// Acquire returns a reference-counted Handle per (issuer, jwksUrl) Source,
// so every Policy and snapshot naming a Source shares one key set, and a
// Revision that keeps a Source reuses its keys. A Source without usable
// keys is fetched at once; Wait lets a Revision compile wait for those
// fetches until its Round ends, PrefetchTimeout after the round began (the
// auth.jwt Factory starts one per compile), and the Revision activates
// whatever they returned. When the last Handle of a Source is released its
// keys are dropped and its degraded state cleared.
//
// # Concurrency
//
// Run owns every goroutine the Manager starts: one fetch goroutine per
// fetch in flight, bounded by MaxConcurrentFetches, all canceled and
// waited for when Run's context ends. Handle.Keys is one atomic load; the
// other methods take the Manager's lock briefly and never block on the
// network. Nothing here logs a token or a claim.
package jwks
