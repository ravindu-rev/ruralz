// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package redisserver launches the State Store servers of the State Store
// suites and the process harness
// (docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests"), in two flavors that EnvStateStore (RURALZ_TEST_STATESTORE)
// selects:
//
//   - process: local redis-server processes, the binary EnvRedisServer
//     (RURALZ_TEST_REDIS_SERVER) names or the one in PATH. Kind Process is
//     a standalone server, with an optional replica for a scripted
//     failover (Options.Replica, Deployment.Promote), TLS (Options.TLS,
//     certificates from testkit/pki) and ACL users (Options.Users); kind
//     ProcessCluster is a three-primary cluster whose primaries have one
//     replica each (Deployment.Replicas); kind ProcessSentinel is a
//     primary, its replica and a redis-server --sentinel monitoring them
//     as MasterSet, failed over with SENTINEL FAILOVER
//     (Deployment.Failover) or by the sentinel itself once the primary is
//     down (Deployment.AwaitFailover). Servers bind loopback ports, keep
//     no data on disk, can be killed and restarted on their port
//     (Server.Kill, Server.Restart), and frozen with their sockets open
//     by SIGSTOP until SIGCONT (Server.Signal).
//   - container: the images of images.go, each pinned by digest, through
//     testcontainers-go, on the linux/amd64 container leg: Redis 8 and
//     Valkey 9.0.1 or newer (version checked at start) through
//     modules/redis and modules/valkey, Dragonfly as a generic container,
//     three-primary Redis and Valkey clusters of generic containers with
//     one replica each, formed over container IPs, and a Redis or Valkey
//     primary, replica and sentinel (Redis8Sentinel, Valkey9Sentinel),
//     which names container IPs; only a Linux host reaches either.
//
// A cluster failover is a SIGKILL of a shard's primary (Server.Kill)
// followed by its replica's promotion with CLUSTER FAILOVER TAKEOVER
// (Deployment.ClusterFailover); the killed primary, restarted, rejoins as
// the promoted replica's replica, as its cluster configuration file keeps
// its node identity.
//
// Kinds returns the kinds the variable selects and skips without it.
// When a kind's flavor cannot run on this machine (no redis-server; no
// Docker daemon, or a cluster or sentinel container off Linux), Start
// fails the test if the variable selects that flavor and skips it with the
// reason if not. A process kind runs wherever a redis-server is found, so the
// process harness needs no variable; a container kind runs only when
// selected. A broken configuration always fails.
//
// A Deployment gives the stateStore.url
// (docs/architecture/02-configuration-model.md: redis:// or rediss://,
// a cluster's seeds as addr parameters, a sentinel deployment's sentinel
// and master_set) and its topology; Options.Announce
// puts a proxy, such as respproxy's, in front of each primary, and a
// cluster then announces the proxies, so every command crosses them. A
// cluster's replicas reach their primaries at the announced addresses
// too, so such a proxy also carries each replica's replication link
// (PSYNC, REPLCONF and the replication stream).
//
// Each Server takes admin commands (Server.Do, Server.Info) over a small
// RESP2 client of its own, for setup and for the faults that come from
// redis commands, such as SCRIPT FLUSH or CONFIG SET maxmemory; the
// rueidis client stays in internal/statestore/redis. Everything a
// Deployment starts ends with the test that started it. The package is
// test support (depguard testsupport), and the only non-test importer of
// testcontainers-go.
package redisserver
