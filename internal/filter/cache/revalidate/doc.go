// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package revalidate is the Response Cache's freshness, stale-serving and
// revalidation model and its background revalidation workers
// (docs/architecture/09-traffic-management-and-resilience.md "Response
// caching"). The cache Filter (internal/filter/cache) builds on it; the
// stored entry's encoding stays the Filter's, behind Entry.
//
// # Freshness and stale serving
//
// [Parse] reads the Cache-Control directives the cache uses (RFC 9111
// section 5.2, RFC 5861). [FreshnessOf] gives a stored response's
// freshness lifetime, from s-maxage, max-age, then Expires minus Date, at
// most [MaxFreshness] (24 h, target), and its stale-while-revalidate and
// stale-if-error windows, each at most [MaxStaleWindow] (1 h, target).
// must-revalidate, proxy-revalidate and s-maxage (which implies
// proxy-revalidate, RFC 9111 section 5.2.2.10) forbid stale serving
// (section 4.2.4), so both windows are zero under them, overriding RFC
// 5861. [InitialAge] and [CurrentAge] are the age calculation of RFC 9111
// section 4.2.3, and [FormatAge] the Age field a hit carries.
//
// [Freshness.Use] says how a stored response serves a request: fresh, as
// a hit; stale inside stale-while-revalidate or the request's max-stale,
// at once while a background revalidation is queued; otherwise only after
// the Upstream answers a conditional request with 304, or under
// stale-if-error ([Freshness.ServesOnError], for the statuses of
// [IsError]). A request's no-cache or max-age=0 always takes the Upstream
// path, a larger max-age bounds the age it accepts and min-fresh the
// freshness it wants left, neither accepting stale without max-stale, and
// a request stale-if-error widens the window for that request; the stale
// limits hold against every request directive. [Storable] is the
// "Storable" rule, and [Freshness.TTL] the State Store TTL of a store or
// refresh: the freshness lifetime plus the larger stale window, minus the
// age.
//
// # Validation requests and 304 responses
//
// [Conditional] turns a saved request into a revalidation of a stored
// response (RFC 9111 section 4.3.1): If-None-Match with its entity tag,
// If-Modified-Since with its Last-Modified or, with neither, its Date,
// after removing the request's own preconditions and Range. [Freshen]
// applies a 304: it updates the stored header fields (section 3.2) when
// the 304's validators select the stored response (section 4.3.4), or when
// it carries none, since the conditional request named only that
// response's validators. The Age and Date of the result describe the 304,
// so the age restarts at the revalidation.
//
// [NotModified] is the other direction (section 4.3.2): a client
// validating its own copy against a stored response gets a 304 with the
// fields of [NotModifiedHeader] when its If-None-Match matches the stored
// entity tag or, without one, its If-Modified-Since is no earlier than the
// stored modification time.
//
// # Background revalidation
//
// The [Revalidator] is the Node-wide filter.Component that serves stale
// responses in the background: [Workers] (4, target) workers take [Job]
// values from a queue of [QueueSize] (256, target) that drops when full. A
// worker takes the partition's revalidation lease with a random token, so
// one Node in the Cell revalidates, then replays the saved request with
// its validators through the Upstream's leg (filter.Replayer: upstream-leg
// Policies, breaker, bulkhead and retries), or through the whole
// composition for a composition entry, whose Job names no Upstream, all
// within one [Deadline] (5 s, target), the life of the lease. An entry
// without validators, such as a merged aggregate response, replays
// unconditionally. A 304 that selects the stored response refreshes the
// entry under the lease (statestore.LeaseRefresh); a full response that
// the request-path store rules accept ([StorableStatus], [Storable], not
// [Partial], the same Vary names, TTL left, [MaxEntry] and the memory
// rules) replaces it under the lease, as the leg delivered it
// (Entry.Replace, statestore.LeaseRefresh); any other full response ends
// the stale window unstored (statestore.LeaseEndStale). A 5xx status, a
// replay that got no response or a body that could not be read leaves the
// entry as it is, so stale-if-error can still serve it, also while the
// breaker is open. Every write after the take names the entry's
// generation token (Entry.Token), so none applies once an invalidation
// moved the URI's generation key on. The worker sends its lease writes
// itself, off the request path, each with the Policy's stateStoreTimeout
// cut to the time left before the Deadline.
//
// A Node keeps at most one revalidation per partition queued or running,
// and none for [Deadline] after its lease attempt, whatever the outcome,
// so each Node makes at most one lease call per partition per lease
// period. It tracks at most [MaxHeld] partitions (4,096, target), dropping
// new jobs past it, and drops a job that waited longer than
// [MaxQueueWait] (one Deadline, target) unrun, so a saturated queue does
// not replay against a slow Upstream long after the stale hits.
//
// # Telemetry
//
// Each job that ran counts ruralz_cache_revalidations_total on the
// queuing Route's handles (Job.Metrics): refreshed for a refresh or a
// replacement, ended, kept, or skipped when it changed nothing. A
// replacement the size cap or the memory rules refuse counts size_limit or
// memory on the queuing Policy's ruralz_cache_store_skipped_total handles
// (Job.StoreSkipped). ruralz_cache_revalidation_drops_total
// counts the Node's drops (Options.Drops, [DropCounters]): queue_full for
// the full queue, queue_wait for a job that waited too long and
// partitions_full for the MaxHeld tracked partitions. The
// cache_revalidation_saturated degraded reason, which tickets, is raised
// while those drops exceed [SaturatedPercent] (10%, target) of the
// revalidations offered over the last [SaturatedWindow] (1 minute,
// target); Run re-checks it every [SaturatedCheck] (1 s, target), so it
// clears once drops stop, and clears it when it stops.
package revalidate
