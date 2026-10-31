// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"context"
	"net"
)

// Loopback hosts of RURALZ_BIND_LOOPBACK.
const (
	// LoopbackIPv4 is bound on every host.
	LoopbackIPv4 = "127.0.0.1"
	// LoopbackIPv6 is bound where the host has IPv6 loopback.
	LoopbackIPv6 = "::1"
)

// LoopbackHosts returns the hosts every client listener and the admin port
// bind under RURALZ_BIND_LOOPBACK=true, instead of every interface
// (docs/architecture/03-data-plane.md "Listeners and protocols"; pack
// section 2): 127.0.0.1 and, where the host has IPv6 loopback, ::1, one
// socket per address. The Node decides once at start, so every listener
// and the admin port bind the same addresses: ::1 counts as present when a
// socket can listen on it, which fails on a kernel without IPv6 and on a
// host whose loopback interface has IPv6 disabled.
func LoopbackHosts(ctx context.Context) []string {
	return loopbackHosts(ctx, listenIPv6Loopback)
}

// loopbackHosts returns LoopbackHosts with probe telling whether ::1 can
// listen.
func loopbackHosts(ctx context.Context, probe func(context.Context) error) []string {
	if probe(ctx) != nil {
		return []string{LoopbackIPv4}
	}
	return []string{LoopbackIPv4, LoopbackIPv6}
}

// listenIPv6Loopback listens on a free port of ::1 for an instant.
func listenIPv6Loopback(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp6", net.JoinHostPort(LoopbackIPv6, "0"))
	if err != nil {
		return err
	}
	return ln.Close()
}
