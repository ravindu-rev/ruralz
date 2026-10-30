// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"syscall"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// portAttempts bounds the search for a free loopback port.
const portAttempts = 100

// freeLoopbackPort returns a TCP port free on 127.0.0.1 and, where the
// host has an IPv6 loopback, on ::1, the addresses ruralzd binds under
// RURALZ_BIND_LOOPBACK, and that taken does not report.
func freeLoopbackPort(ctx context.Context, taken func(int) bool) (int, error) {
	var lc net.ListenConfig
	for range portAttempts {
		ln, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
		if err != nil {
			return 0, noResult("find a free loopback port", err)
		}
		addr, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			_ = ln.Close()
			return 0, command.NoResultf("find a free loopback port: unexpected address %v", ln.Addr())
		}
		port := addr.Port
		busy := ipv6Busy(ctx, port)
		_ = ln.Close()
		if !busy && !taken(port) {
			return port, nil
		}
	}
	return 0, command.NoResultf("find a free loopback port: none free in %d attempts", portAttempts)
}

// ipv6Busy reports whether port is taken on ::1. Any failure other than a
// taken address means the host has no IPv6 loopback, so ruralzd binds
// 127.0.0.1 alone.
func ipv6Busy(ctx context.Context, port int) bool {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
	if err == nil {
		_ = ln.Close()
		return false
	}
	return errors.Is(err, syscall.EADDRINUSE)
}

// CheckPortsFree test-binds every port of r on 127.0.0.1 and ::1, as
// ruralzd will bind them under RURALZ_BIND_LOOPBACK, and exits 2 naming
// the first port that is busy (docs/reference/01-cli-and-api-surface.md
// "Local ruralzd launched by the CLI", Ports): without the rewrite the CLI
// checks before the launch.
func CheckPortsFree(ctx context.Context, r *Render) error {
	ports := make([]Port, 0, len(r.Listeners)+1)
	for _, l := range r.Listeners {
		ports = append(ports, l.Port)
	}
	ports = append(ports, r.Admin)
	for i, p := range ports {
		if err := portFree(ctx, p.TestPort); err != nil {
			what := "listener " + p.Name
			if i == len(ports)-1 {
				what = "the admin port"
			}
			return command.NoResultf("%s: port %d is busy: %v; free it or use --ephemeral-ports", what, p.TestPort, err)
		}
	}
	return nil
}

// portFree test-binds port on 127.0.0.1, then on ::1 where the host has
// one.
func portFree(ctx context.Context, port int) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	if ipv6Busy(ctx, port) {
		return fmt.Errorf("listen tcp6 [::1]:%d: %w", port, syscall.EADDRINUSE)
	}
	return nil
}
