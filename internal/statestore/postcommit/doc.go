// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package postcommit is the bounded post-commit write queue of a Node
// (docs/architecture/11-scalability-and-distributed-state.md "Post-commit
// write queue"; foundation pack section 8.7 rule 4): Quota refunds, Response
// Cache stores, lease writes and invalidations that Filters hand off after
// commit, written by a fixed pool of writers so they never delay a response.
//
// # Classes
//
// The queue holds four classes, each capped in items and in bytes of
// retained memory, so a full class drops only its own writes. A write's
// bytes (Size) are a fixed overhead, the Go type sizes of the write and
// its queue slot (ItemOverhead), plus its key and payload bytes; the
// invalidation class's byte cap therefore binds before its item cap:
//
//	class        items    bytes   turn            on a failed write
//	settle       32,000   16 MiB  first           kept failed (over-charge)
//	invalidate    6,400    1 MiB  second          one retry, then dropped
//	store        16,000   64 MiB  third, 4 of 5   kept failed (a miss)
//	plugin        9,600    4 MiB  third, 1 of 5   kept failed (M2)
//
// Enqueue never blocks and allocates nothing on the drop path. A write is
// dropped, and counted in ruralz_state_writes_dropped_total by kind, when
// its class is full, when the queue is closed, when its store answers
// RZ-STS-003 (the shard's breaker is open: dropped at once) or RZ-STS-004
// (not attempted, such as a zero timeout), when an invalidation fails
// twice, and when the Drain flush ends with the write still queued. A
// write the store attempted and failed (RZ-STS-001, RZ-STS-002) counts in
// ruralz_state_calls_total through the driver; only invalidations get a
// retry. ruralz_state_write_queue_items is the number of queued writes.
//
// # Writers
//
// Eight writers share one turn counter over a schedule of 15 turns:
// settlement, invalidations, then stores in four turns of five and Plugin
// writes in the fifth. A writer takes the next turn, skips classes that
// are empty, and writes up to 64 writes of one class as one batch per
// Target (statestore.Store.Write, one round trip). Under saturation an
// invalidation thus waits at most one settlement batch plus the batch in
// progress, and Plugin writes get one batch in 15. A panicking Target
// fails its batch and the writer goes on.
//
// # Targets and shutdown
//
// A Binding enqueues for one Target, the State Store driver the Policy
// used; each queued write holds a Target reference (Hold) until it is
// written or dropped, so the manager keeps a driver open while its writes
// are queued across Hot Reloads. Close flushes for up to 5 s at Drain,
// then stops the writers and drops what is left. New starts the writers;
// Close stops them and waits.
package postcommit
