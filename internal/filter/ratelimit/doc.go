// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package ratelimit is the ratelimit Policy type: a local token bucket per
// key and limit at a per-Node ceiling, then GCRA in the State Store
// (docs/architecture/09-traffic-management-and-resilience.md "Rate
// limiting" and "Per-Node ceiling", ADR-0008). It is an admission-class
// Filter in onRequestHeaders at Gateway or Route scope, failureMode open
// by default, and a filter.Consumptive whose GCRA call the executor
// batches with the request's other consumptive calls (pack 8.7 rule 3).
//
// # Decision path
//
// Prepare evaluates config.key, config.cost (default 1) and each
// limits[].when once. A runtime error in any of them, or a cost below 1,
// applies failureMode without a bucket: open admits unmetered, closed
// answers 503 RZ-RL-005. Limits whose when is false take no part, and a
// Policy left with none admits. Then, on the Node-wide key table:
//
//  1. A key whose last GCRA answer denied one of its applied limits stays
//     denied from the over-limit cache until that answer's retry-after:
//     429 RZ-RL-002 with no State Store call.
//  2. A key with no entry for any applied limit is first-seen and spends
//     a unit of the Node's first-seen budget, which every Policy shares:
//     min(500, B / N_published) per second with a Node count, else 200,
//     B being 20,000 per Cell. Within the budget its buckets start full;
//     past it they are local-only entries, in their own segment, that
//     decide without GCRA for 10 s.
//  3. Every applied limit's bucket, of capacity and refill per window
//     equal to the per-Node ceiling, must hold cost tokens; the request
//     takes them from every bucket or from none, else 429 RZ-RL-001 by the
//     bucket that admits last. A cost above a ceiling never fits.
//  4. A local-only entry or a localOnly Policy admits there. Otherwise
//     Prepare fills one GCRA call for every applied limit, charging cost
//     emission intervals.
//
// Complete applies the reply: an allow admits; a deny caches it on the
// denying limit's entry and answers 429 RZ-RL-002; a failed, timed-out
// or unattempted call, or an open breaker, applies failureMode: open
// admits within the local buckets, closed answers 503 with the call's
// RZ-STS code. Undo returns the tokens Prepare took when an earlier or
// later member of the batch ended the request.
//
// # Per-Node ceiling and failing open
//
// A declared limits[].perNodeCeiling is the ceiling; otherwise, with the
// Node count Ruralz Control publishes (Control mode, Planned (M2)), it is
// min(requests, max(10, ceil(2 × requests / N))); file mode, and a
// Control-mode Node without a count, use the full limit. From a key's
// first failed or unattempted call under failureMode open until its next
// GCRA answer, its buckets refill per epoch-aligned window instead of
// continuously: each window brings the fail-open allowance without
// carry-over, in 60 equal steps for a window past a minute, and at the
// switch a bucket keeps the tokens it holds, at most the allowance, for
// the rest of the window. The allowance is the ceiling, except max(1, 2 ×
// requests / N) for a derived ceiling and max(1, requests / 100) for a
// Control-mode Node without a count.
//
// # Key table
//
// Buckets, over-limit and local-only entries share the Node-wide Table, a
// filter.Component that outlives snapshots: 1,048,576 entries of 64 bytes
// in 256 short-mutex shards, of which 131,072 form the local-only segment,
// each segment evicted by CLOCK so a key-rotation flood never evicts a
// key with a GCRA answer. All limits of one Policy and key share a shard.
// Entries are keyed by the limit, the Policy name with its requests and
// window, and the SHA-256 of the key value, so an unchanged limit keeps
// its buckets across a Hot Reload and a changed one starts fresh. Bucket
// arithmetic is in whole tokens and nanoseconds. Evictions count in
// ruralz_ratelimit_bucket_evictions_total by segment for the Policy whose
// entry was evicted.
//
// # Response fields
//
// With config.responseHeaders ietf, the default, the Policy records each
// applied limit for the RateLimit-Policy and RateLimit fields
// (filter.Exchange.AddRateLimitField): its name, suffixed .1, .2 by
// position when the Policy holds several limits, requests and the window
// in seconds rounded up; on an admission, the remaining count and the
// seconds until the limit is whole again, from the GCRA reply or, for a
// local-only entry or a localOnly Policy, the Node's bucket; nothing more
// for an admission without a count. On a 429 the denied limit carries r=0
// and t equal to Retry-After, marked Denied. Every 429 carries
// Retry-After, responseHeaders none included.
//
// # Metrics and validation
//
// Each decision counts in ruralz_ratelimit_decisions_total: allow,
// deny_local, deny_global, fail_open, fail_closed and allow_local. Check
// is the offline check of stage H (RZ-CFG-005 for the limits rules of the
// Configuration model), which Build enforces too; the RZ-CFG-043 warning
// about a constant key is stage J's (internal/config/pipeline).
package ratelimit
