// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package manager owns a Node's State Store drivers across Hot Reloads
// (docs/architecture/11-scalability-and-distributed-state.md, the Gateway
// stateStore fields of docs/architecture/02-configuration-model.md, and
// docs/architecture/03-data-plane.md "Configuration snapshots and hot
// reload").
//
// # Configuration
//
// SpecOf resolves Gateway spec.stateStore into driver configurations:
// driver memory (the default) or redis; for redis, topology standalone
// (the default), cluster or sentinel, the resolved url, the TLS block and
// the zone map; timeout, 50 ms by default, where 0 attempts no call; and
// the optional cache connection, a second redis connection only the
// Response Cache uses, with its own TLS block and zone map and the main
// timeout. Without stateStore the Node uses RURALZ_STATE_STORE_URL, when
// it resolved, as a standalone redis URL without a zone map, else the
// memory driver.
//
// # Zones
//
// Each redis connection labels its calls' durations same, cross or
// unknown by comparing the zone of the server a call went to, from its
// zone map (stateStore.zones, stateStore.cache.zones, keyed by
// netaddr.CanonicalHost), with the Node's RURALZ_ZONE, which the ruralzd
// wiring passes in Config.Deps.Zone from internal/gateway/setting. The
// zone map is not part of a driver's key: a Hot Reload that changes only
// it keeps the driver, and the newer snapshot's handle gives the driver
// its map at the snapshot's first call, after the swap (Handle.Store), so
// the changed map labels calls from the swap on and a snapshot that is
// never published changes nothing.
//
// # Drivers and handles
//
// Acquire returns one new Handle per role for a snapshot, meeting the
// snapshot StoreHandle contract (Store, Enqueuer, Release). Drivers are
// shared by key: the role, the driver and, for redis, the topology, URL
// and TLS block references. A Hot Reload that keeps the State Store
// configuration therefore keeps its driver, with its connections, breakers
// and loaded scripts, and the memory driver with its counters. A new
// timeout, source or zone map keeps the driver too: each call carries its
// Policy's stateStoreTimeout, Config.DefaultTimeout stays the value the
// driver opened with, and the zone map moves as Zones says. A changed
// driver, topology, URL or TLS block reference opens a new driver through
// the injected opener before the swap; the old driver closes once the last
// snapshot holding it is freed and the post-commit queue has written or
// dropped every write queued for it. Without a cache connection the Cache
// handle is the Main handle, released once. Every driver receives the
// Node's one in-flight ceiling (statestore.InFlight of Limits.InFlight,
// 8,192), so the main and cache connections, and the old and new drivers
// of a Hot Reload or a rotation, share it.
//
// # Rotated URL secrets
//
// A redis connection watches the secret its URL was read from
// (Spec.MainURL, Spec.CacheURL): a rotated value that differs from the URL
// in use, which the resolver polls for a file secret every 2 s, opens a
// new driver for that connection at once, on the resolver's goroutine and
// off the request path, as a changed URL does at a Hot Reload. Each
// snapshot's handle keeps its Store and Enqueuer, which reach the driver
// serving the connection at each call and enqueue, so calls and later
// writes move to the new driver, which dials, authenticates and loads its
// scripts under the reconnect pacing; a call in progress finishes on the
// old driver, which closes once it and the writes queued for it are done.
// The connection moves to the rotated value's key, so a later Revision
// resolving that value shares it. A value the opener refuses keeps the old
// driver and fails the watch, a rotation failure the resolver counts. An
// env secret, such as RURALZ_STATE_STORE_URL, is never polled, so its URL
// changes only with a restart.
//
// # Post-commit queue
//
// The Manager owns the Node's postcommit.Queue, and each connection's
// Enqueuer queues writes for the driver serving it. Close flushes the
// queue for up to 5 s at Drain, then closes every driver.
//
// # Degraded states and warnings
//
// The Manager owns the State Store reasons of
// docs/architecture/10-observability.md "Degraded states" and the
// state_store cleartext hop count, derived from the snapshots' handles and
// the drivers' Status after every change and at the status poll every 1 s:
// state_store_memory_fallback while a snapshot uses the memory fallback,
// state_store_breaker_open while a driver reports a breaker that is not
// closed, state_store_eviction_policy while a store a ratelimit or quota
// Policy uses reports another maxmemory-policy than noeviction or a store
// a cache Policy uses, the dedicated cache connection or the shared main
// one, reports neither noeviction nor volatile-ttl, and
// state_store_unauthenticated. Drivers report through Status and leave
// these to the Manager. It logs a warning when the memory fallback starts,
// since memory multiplies every Rate Limit and Quota by the Node count,
// and when the Response Cache starts sharing the State Store that holds
// limit keys.
package manager
