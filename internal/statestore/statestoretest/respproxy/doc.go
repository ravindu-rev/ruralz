// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package respproxy is the counting RESP proxy of the State Store suites
// (docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests"): a faultproxy.Proxy between a Node's State client and a State
// Store whose connections a Counter parses, so a test asserts the
// commands the Node sends, as the round-trip test of
// docs/adr/0008-rate-limiting-local-bucket-and-gcra.md "Confirmation"
// does: zero commands for local denials and over-limit cache hits, at
// most one blocking call per admitted request per Policy, and one script
// for consumptive keys sharing a hash slot.
//
// # Counting
//
// The Counter parses commands (arrays of bulk strings, or inline lines)
// flowing up and RESP2 or RESP3 replies flowing down, incrementally,
// whatever the chunk boundaries. A Snapshot counts commands by name,
// round trips, replies, error replies by code ("NOSCRIPT"), RESP3 pushes
// and desynchronized streams; Sub turns two Snapshots into the counts
// between them. The bounded command log keeps each command's name,
// connection, round trip and reply summary. A round trip starts with a
// command sent while its connection awaits no reply; commands pipelined
// behind it join it. Replies are matched to commands in order, one reply
// per command, which holds for every command a State client sends (no
// Pub/Sub, CLIENT REPLY OFF or MONITOR).
//
// # Faults
//
// The embedded faultproxy.Proxy injects TCP faults: drops and partitions
// (Blackhole), resets (Reset) and a scripted failover (SetUpstream). Reply
// delays add slow replies: the replies to a named command, or to every
// command, are held before they reach the client, each for its delay
// from the moment the proxy read it. The proxy goes on reading while
// replies are held and forwards them in the order read, so the replies to
// pipelined commands are held side by side, as a server answering each
// that much late would, never one hold after another; a reply read behind
// a held one waits for it. Bytes the fault proxy drops never reach a tap,
// so they are never counted: the counts are what the server and the
// client saw.
//
// # Goroutines
//
// A Counter starts none: its taps and holds run on the fault proxy's
// per-connection goroutines, which a hold never blocks, and a hold ends on
// a timer, at a change of the delays or when the Counter closes. Proxy
// closes the Counter before the fault proxy, so no hold outlives the
// proxy.
package respproxy
