// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package redis is the redis State Store driver, the only rueidis importer
// (docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes"; the stateStore fields of
// docs/architecture/02-configuration-model.md; ADR-0008).
//
// # Configuration
//
// Open validates the connection's URL (stateStore.url or
// stateStore.cache.url) and builds its TLS block's configuration
// (stateStore.tls or stateStore.cache.tls) without dialing. It takes the
// rueidis URL form: redis:// or rediss:// (valkey:// and valkeys:// are
// the same), standalone with one endpoint and an optional /db, cluster
// with seeds as addr parameters, sentinel with sentinel seeds as addr
// parameters and master_set. A URL
// that does not parse or fit the topology (addr under standalone, a
// database under cluster, master_set outside sentinel, sentinel without
// it) is RZ-CFG-026. A URL the transport rules refuse is RZ-CFG-042:
// another scheme; a parameter rueidis does not read; the parameters that
// would break a design rule (skip_verify, client_cache other than 0,
// protocol other than 3, max_retries other than 0); conflicting ones (a
// parameter rueidis reads once given twice, a database in both the path
// and db); a cleartext scheme beside the connection's TLS block; a URL
// naming a host other than a loopback literal without a user name or a
// password. No error carries the URL or a parameter name from it.
// dial_timeout, write_timeout and client_name pass to rueidis.
//
// # State client
//
// One rueidis client per driver, created off the request path: standalone
// with ForceSingleClient, cluster reading the slot map from any seed and
// refreshing it every 10 s, after a redirect and after a closed
// connection, sentinel asking the sentinels for the master set's primary
// at start, on +switch-master, when its sentinel connection closes and
// every 10 s. The URL's credentials authenticate to the primary and to
// the sentinels, in the client's HELLO, and its database is the
// primary's; sentinels get no script load. The primary is one shard
// whatever its address. Commands always go through the auto-pipelined connections
// (2 per shard, rueidis PipelineMultiplex 1); up to 8 dedicated
// connections per shard open lazily and no M1 call uses one. Client-side
// caching and retries are off: no call makes a second round trip, and a
// cluster command follows at most MaxRedirects MOVED or ASK replies.
// rediss:// verifies every server, cluster node and sentinel for the host
// it dials, against its TLS block's CA and presenting its client
// certificate (tlsconf.StateStoreClient, which follows rotations until
// Close), else against the system roots (tlsconf.StateStore). Status
// reports a redis:// URL as a cleartext hop and a loopback URL without
// credentials as unauthenticated; logs name shards ss-<8 hex>, never an
// address or the URL.
//
// The driver's dialer opens every connection, to the URL's hosts and the
// servers their cluster or sentinels announce: it paces the dials of a
// shard (full jitter, base 100 ms, cap 5 s, at most 4 a second), and the
// sentinel dials together, so a dial the pacer refuses fails at once
// without touching the network; then, within the dial timeout, it
// connects, runs the TLS handshake and, except on a sentinel, sends AUTH
// and SCRIPT LOAD before rueidis takes the connection. Each dial, TLS or
// load I/O failure counts toward the shard breaker's 5 consecutive connect
// failures, and a sentinel dial's toward the primary's while no client
// exists.
//
// A READONLY reply, from a primary a failover demoted, closes the shard's
// connections, at most once per minRefreshGap: standalone redials reach
// the endpoint's new primary, a cluster's closes request a slot map
// refresh, and under sentinel the sentinel connections close too, so the
// client asks the sentinels again.
//
// # Calls
//
// Consume, Read and Write each make one round trip, which holds one slot
// of the Node's in-flight ceiling (8,192, statestore.InFlight) from its
// send until its reply or a connection reset, never its caller's timeout:
// a goroutine waits for rueidis while the caller waits for the smaller of
// its timeout and the per-request deadline. The pre-call checks run in
// this order: a command set the shard lacks (RZ-STS-005), the per-request
// deadline or a zero timeout (RZ-STS-004), a closed driver (RZ-STS-002),
// an open breaker (RZ-STS-003, no call), a full in-flight ceiling
// (RZ-STS-004). A round trip that misses its timeout is RZ-STS-001; a
// connection error, an error reply such as NOSCRIPT or OOM, and an
// unparsable reply are RZ-STS-002.
//
// Consume merges the longest prefix of calls sharing a hash slot into one
// EVALSHA of the ruralz_v1.lua library (script_multi when longer than one).
// Read pipelines a GET of the generation key and an HMGET of the partition
// per lookup. Write pipelines one EVALSHA per refund, store, refresh,
// end-stale and invalidation, and SET NX PX 5000 per revalidation lease
// take. GCRA and Quota outcomes come from the TATs, counts and window the
// script returns and the server's TIME, with the arithmetic of the memory
// driver, so both drivers decide identically.
//
// # Scripts
//
// The dialer loads the library on every new connection before its first
// EVALSHA, which covers connect, reconnect and failover. A NOSCRIPT reply,
// from a server that lost its scripts while the connection lived, fails
// its round trip and schedules a reload, single-flight per shard; the call
// is never retried with EVAL. A shard whose SCRIPT LOAD answers that the
// command is unknown or not permitted lacks the script command set:
// Supports reports it, its script calls and writes are RZ-STS-005 without
// a call, and the load is retried at each memory reading until it
// succeeds.
//
// # Breakers
//
// Each shard has a breaker (internal/statestore/breaker): a well-formed
// reply, a denial included, is a success; a timeout, connection error,
// error reply or unparsable reply is a failure. A half-open breaker that
// sees no request probe for 1 s is probed with PING, bounded by the
// Gateway's stateStore.timeout. In a cluster, a slot whose owner is not
// known yet uses the unmapped shard, whose probe is a client creation or
// slot map refresh. A timeout is recorded at the timeout, before the caller
// hands the round trip to its goroutine.
//
// # Memory readings
//
// Every 10 s, or every 1 s above 50% of maxmemory, the driver reads INFO
// memory per shard off the request path. AdmitStoreBytes skips a store
// before a shard's first reading, above 70% of maxmemory, while the shard
// grew over 10% of maxmemory between readings until growth falls under 2%,
// and beyond min(2% of maxmemory per 10 s / 1,000, 4 MiB per second) bytes
// between readings, 1,000 being N_c without a published Node count. A shard
// whose maxmemory is 0 admits no store. The readings also give Status its
// eviction policy.
package redis
