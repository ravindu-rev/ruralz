// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package basic is the auth.basic Policy type
// (docs/architecture/08-security-and-identity.md "Authentication", "Basic
// and mTLS schemas" and "Pre-authentication throttling"). In
// onRequestHeaders it reads Authorization: Basic (RFC 7617), verifies the
// password against the PBKDF2-HMAC-SHA-256 hash of the username's
// credentials.basic entry in the snapshot's credential index
// (internal/filter/auth.AcquireIndex), binds that entry's Consumer and
// sets the request identity. config has no fields. It is an auth-class
// Filter, failureMode closed only, and never calls the State Store.
//
// # Decisions
//
// Filter.Handle lists the RZ-AUTH code of every outcome ("RZ-AUTH
// decision codes"). Like auth.jwt, a missing Authorization field or
// another scheme is RZ-AUTH-001; a malformed Basic value is RZ-AUTH-002,
// as are a wrong password, an undeclared username, a username two
// Consumers declare (RZ-CFG-035 prevents it statically) and a second
// successful authentication (Security rule 2). A verified pair is checked
// against the revocation list (identity.Revoker, RZ-AUTH-004; M1 wires
// identity.NopRevoker) before the Consumer binds. 401s carry the challenge
// Basic realm="ruralz", charset="UTF-8"; a throttled attempt is 429
// RZ-AUTH-007 with Retry-After. No answer, log or label carries the
// username or password, and the Filter removes every Authorization line
// from the live request headers, so the credential never reaches the
// Upstream ("Request hardening").
//
// # Throttle
//
// Rate Limits run after auth and never see failed attempts, so the
// Node-wide Throttle, built once per process and shared by every
// auth.basic Policy, bounds the verifications that miss the success
// cache; its constants are the document's targets. Everything is keyed by
// HMAC-SHA-256 under a per-process key,
// so no username, password or address is kept, and a restart starts
// empty.
//
//   - Success cache: keyed by the username and password, recording the
//     credential digest it matched (identity.BasicCredential.Digest).
//     CacheEntries entries in Shards shards; an entry lasts CacheIdle after
//     its last hit and at most CacheMaxAge from verification, less a jitter
//     of up to CacheJitterPercent, fixed per username and password within
//     a process. A hit skips the throttle and the hash but still
//     passes the revocation check; an entry whose recorded digest is not
//     the current snapshot's misses and is removed, so a snapshot swap
//     evicts only changed or removed credentials.
//   - Source key: source.ip, an IPv6 address as its /64, in an LRU of
//     SourceBuckets; at most SourceFailures failed verifications per
//     SourceWindow, a successful verification counting no failure, and an
//     empty bucket refuses every uncached attempt.
//   - Username bucket: one per presented username, declared or not, in an
//     LRU of UserBuckets; UserBurst tokens refilling one per UserRefill. A
//     bucket is created only after the hash queue admits an attempt, at
//     most CreatesPerSecond per Node with a burst of CreateBurst.
//   - Delay: an empty bucket holds at most DelayWaitersPerBucket waiters,
//     DelayWaiters per Node, each for at most MaxDelay.
//   - Hash queue: max(1, GOMAXPROCS/4) slots cap the Node's hash rate,
//     with QueueWaitersPerSlot waiters per slot waiting at most
//     QueueDeadline.
//   - Undeclared usernames pass the same rules and run a dummy hash at the
//     default iteration count through the same queue, so neither timing
//     nor status reveals whether a username exists.
//
// Refusals (hash queue full or past its deadline, bucket-creation cap,
// delay overflow, empty source bucket) are 429 RZ-AUTH-007 before any
// hash, for declared and undeclared usernames alike. Retry-After is the
// time until the refusing bucket refills, at least 1 s; a hash-queue
// refusal gives 1 s. ruralz_auth_basic_refusals_total counts them by
// cause (queue, create, delay, source) through the handles of
// Options.Refusals, which RefusalCounters looks up in the telemetry
// registry. A refused or abandoned attempt returns the tokens it took.
package basic
