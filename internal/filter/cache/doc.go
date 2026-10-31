// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package cache is the Response Cache, the built-in Filter of the cache
// Policy type (docs/architecture/09-traffic-management-and-resilience.md
// "Response caching"): an RFC 9111 shared cache with RFC 5861
// stale-while-revalidate and stale-if-error, its entries on the Gateway's
// stateStore.cache connection, else the main State Store, in the layout of
// docs/architecture/11-scalability-and-distributed-state.md "Response
// Cache layout". Its subpackages hold the freshness model and background
// revalidation workers (revalidate) and miss coalescing (coalesce).
//
// # Keys
//
// A GET or HEAD is looked up in onRequestHeaders, after auth, authz and
// admission. [URIDigest] gives U from the request as it arrived, its
// query's escapes normalized; [PartitionDigest] gives P from the Route
// name and the partition value: config.key's result, else the principal
// of an authenticated request, else "". [Names] is a stored response's
// Vary names, the partition's n, and [Variant] the variant digest V of a
// request under them; V is computed from the names the Node last learned
// for the URI, kept in an LRU of [MaxLearnedURIs] URIs (65,536, target).
//
// # Lookup and serving
//
// A lookup is one pipelined State Store batch: the generation key, the
// partition's names, the variant's entry and store time, and the shard's
// TIME. A hot-entry layer of [HotBytes] (64 MiB, target) holds, for
// [HotHold] (1 s, target), the generation tokens and entries looked up
// twice within HotHold, skipping the batch, or its generation key read,
// while they are held. A variant serves only under the names V was
// computed from and while the generation key holds the token the entry
// records; otherwise the request misses, uri-miss or vary-miss, and the
// Node learns the partition's names.
//
// A stored response serves as revalidate.Freshness.Use decides: fresh as
// a hit; stale inside stale-while-revalidate or the request's max-stale
// at once, queuing a background revalidation (revalidate.Revalidator); or
// after a request-path validation, which forwards the stored validators in
// place of the client's own preconditions and Range. A credentialed entry
// is never revalidated in the background: it validates on the request
// path. A hit reserves its size from limits.maxBufferedBytes first, else
// the request bypasses the cache. A served response is filter.Response
// Cached, so onResponse runs every Policy on it: a 304 when the client's
// own preconditions validate its copy, a 206 for one satisfiable byte
// range, else the stored response, each with Age; an unsatisfiable range
// is 416 RZ-RT-026. After a validation, Substitute serves the stored
// response a selecting 304 freshened, whose refresh Finish queues, or,
// under stale-if-error, the stored response in place of a 500, 502, 503 or
// 504. A request with only-if-cached that no stored response serves gets
// 504 RZ-RT-018 and is never forwarded.
//
// A request with a body, no-store, a config.key that fails to evaluate or
// an authenticated request without a principal bypasses the cache. A
// failed lookup is the State Store's RZ-STS failure under the Policy's
// failureMode, recorded as a bypass.
//
// # Misses and stores
//
// Concurrent GET misses of one key on one Node share the first one's fetch
// through a coalesce.Table keyed by the partition and the token the
// lookup read; a HEAD or Range miss only follows, and a request asking
// for revalidation takes no part. In onResponse a forwarded GET's leg
// response, as the leg delivered it, is stored when it is storable with
// TTL left: the entry ([MaxEntry], 128 KiB, target) is encoded without its
// body and a tee copies the body into the room left; encoded header blocks
// and saved request alone past MaxEntry count size_limit. Finish publishes
// the complete response to the flight and queues the store on the
// post-commit queue, under the lookup's token or creating the generation
// key with a fresh one, when the memory rules admit its bytes (else
// memory). The saved request keeps no credentials: a request that carried
// Authorization, Proxy-Authorization, Cookie or an auth.api-key field, or
// that auth.api-key or auth.basic authenticated, stores a credentialed
// entry.
//
// # Invalidation
//
// Invalidate bumps the URI's generation key to a fresh token after a 2xx
// or 3xx leg response to an unsafe method, whatever spec.when decided, so
// no entry written under an earlier token hits again; hot-entry layers of
// other Nodes may serve the old generation for up to HotHold.
//
// # Telemetry
//
// Every GET or HEAD the Policy handled counts one result in
// ruralz_cache_requests_total, in Finish, and records it in the access
// log's cache field: hit (also a 206, a 416 or a client-validation 304),
// miss (also a request-path validation and an only-if-cached 504),
// bypass, stale, stale_error or coalesced. Its response carries one
// Cache-Status member (RFC 9211) with hit and ttl, fwd (uri-miss,
// vary-miss, request, stale or bypass) with fwd-status for a served
// substitute, stored and collapsed, never key or detail.
package cache
