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
// (the default) or cluster and the resolved url; timeout, 50 ms by
// default, where 0 attempts no call; and the optional cache connection, a
// second redis connection only the Response Cache uses, with the main
// timeout. Without stateStore the Node uses RURALZ_STATE_STORE_URL, when
// it resolved, as a standalone redis URL, else the memory driver.
//
// # Drivers and handles
//
// Acquire returns one new Handle per role for a snapshot, meeting the
// snapshot StoreHandle contract (Store, Enqueuer, Release). Drivers are
// shared by key: the role, the driver and, for redis, the topology and
// URL. A Hot Reload that keeps the State Store configuration therefore
// keeps its driver, with its connections, breakers and loaded scripts, and
// the memory driver with its counters. A new timeout or source keeps the
// driver too: each call carries its Policy's stateStoreTimeout, and
// Config.DefaultTimeout stays the value the driver opened with. A changed
// driver, topology or URL opens a new driver through the injected opener
// before the swap; the old driver closes once the last snapshot holding it
// is freed and the post-commit queue has written or dropped every write
// queued for it. Without a cache connection the Cache handle is the Main
// handle, released once. Every driver receives the Node's one in-flight
// ceiling (statestore.InFlight of Limits.InFlight, 8,192), so the main and
// cache connections, and the old and new drivers of a Hot Reload, share
// it.
//
// # Post-commit queue
//
// The Manager owns the Node's postcommit.Queue, and each driver's
// Enqueuer queues writes for that driver. Close flushes the queue for up
// to 5 s at Drain, then closes every driver.
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
