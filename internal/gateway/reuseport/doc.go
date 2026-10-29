// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package reuseport opens the TCP listening sockets of ruralzd with
// SO_REUSEPORT and steers each port's reuseport group with a classic BPF
// program: the kernel side of a Zero-Downtime Upgrade
// (docs/adr/0015-zero-downtime-upgrades-so-reuseport.md) and of a Hot
// Reload that gives a listener a new socket on a port it keeps
// (docs/architecture/03-data-plane.md "Listeners and protocols").
// SO_REUSEPORT serves only these handovers, never scaling: two Nodes per
// network namespace are not planned
// (docs/architecture/11-scalability-and-distributed-state.md
// "SO_REUSEPORT and listeners").
//
// # Sockets
//
// [Listen] opens a socket listening on an address: an IP literal and a
// port, or an empty host for every interface (IPv6 dual-stack, or IPv4
// where the kernel has no IPv6). Host names are refused, so binding never
// waits on name resolution. [Bind] binds without listening: a bound TCP
// socket joins its port's reuseport group only when [Socket.Listen] is
// called, so it receives no connection before, while a port conflict
// already fails at Bind ("Bind, not listen" in ADR-0015).
//
// # Steering
//
// The kernel keeps the listening sockets of a reuseport group in an array:
// a socket that starts listening is appended, and a socket that closes is
// replaced by the last one. [Steer] attaches an SO_ATTACH_REUSEPORT_CBPF
// program that returns one array position, so every new connection of the
// port goes to the socket there; a position past the end falls back to
// the kernel's hash. A [Group] mirrors that array for one port's sockets
// of this process, so [Group.Pin] steers to a socket and [Group.SteerTo]
// to a position after them, the socket a handover successor added.
//
// # Platforms
//
// Linux supports all of it. On BSD and macOS (development only), Listen
// sets SO_REUSEPORT, so a second socket binds a port that a listening one
// holds; Bind and Steer return errors.ErrUnsupported. Elsewhere (Windows,
// which has no ruralzd) sockets are plain and a second socket on a port
// fails to bind. [SharedPorts] and [Steering] report the platform's case.
package reuseport
