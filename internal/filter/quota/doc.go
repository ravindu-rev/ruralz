// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package quota is the quota Policy
// (docs/architecture/09-traffic-management-and-resilience.md "Quotas"):
// a long-window Consumer allowance, a Consumer quota's limit per fixed
// window or per UTC calendar month, reserved before commit and refunded
// asynchronously. Filter is a Consumptive admission Filter of
// onRequestHeaders, at Gateway or Route scope, that also runs in onLog;
// Cache is the Node-wide denial cache every quota Filter shares.
//
// # Decision path
//
// In onRequestHeaders a request without a Consumer, or of one lacking
// config.consumerQuota or holding it with unit tokens, is denied 403
// RZ-RL-004 before config.key and config.cost are evaluated. config.key,
// by default the Consumer's name, partitions the quota; config.cost (CEL
// int, default 1) is the units the request reserves. A runtime error of
// either, or a cost below 1, applies failureMode: open admits unmetered,
// closed answers 503 RZ-RL-005.
//
// A request charges each counter, its Consumer quota, window and key
// value, once (filter.Exchange.Charge): a later quota Policy whose counter
// an earlier Policy of the request charged takes that Policy's decision
// without a call, an admission without a count of its own, and only the
// charging Policy refunds. Its decision counts in its own metrics, at once
// when the charging Policy already decided, else in Finish.
//
// The charging Policy asks the denial cache first: a denial held for the
// key, keyed with the quota's limit so a raised limit applies at once,
// answers 429 RZ-RL-003 only when the cost exceeds the units its window
// had left. Otherwise one script reserves the cost if the window has that
// many units left (statestore.Quota), batched with the request's other
// consumptive calls. An admission forgets the key's cached denial; a
// denial reserves nothing and is cached with the units left for at most
// 60 s, never past the window's end (target), so a refund applies within
// 60 s. A failed, timed-out or unattempted call, or an open breaker,
// applies failureMode: open admits unmetered, closed answers 503 with the
// call's RZ-STS code.
//
// # Refunds
//
// In onLog the charging Policy returns its reservation when the request
// ends refunded (filter.Final.Refunds): a later Filter rejected it, or no
// attempt reached an Upstream and the request did not end served, as for
// a Node rejection before any attempt or an Upstream every attempt failed
// to connect to or to finish TLS with; a cache hit is charged. The refund
// goes to the post-commit queue's settle class with the Policy's
// stateStoreTimeout; a dropped refund only over-charges.
//
// # Response fields and metrics
//
// Each quota Policy records one RateLimit field named after it: q is the
// quota's limit and w its window in seconds, rounded up, or the length of
// the calendar month. The charging Policy's admission reports r, the
// units left, and t, the seconds to the window's end; an admission
// without a count, unmetered or a follower's, and a failure report
// neither. A 429 carries Retry-After at the window's end plus a spread of
// up to 1% of the window, at most 60 s (target), and the field reports r,
// the units left, and t equal to Retry-After. Each decision counts in
// ruralz_quota_decisions_total: allow, deny, no_quota, fail_open and
// fail_closed.
//
// The configuration model registers config.cost and the Consumer's
// quotas[].calendar; the schema states the config and exactly one of
// window and calendar, and internal/config/validate a window of at least
// 1 s in whole seconds (RZ-CFG-005).
package quota
