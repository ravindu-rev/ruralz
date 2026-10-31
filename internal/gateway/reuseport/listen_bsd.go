// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package reuseport

import (
	"context"
	"net"
	"os"
	"syscall"
)

const (
	sharedPorts = true
	steering    = false
)

// Listen opens a TCP socket listening on addr with SO_REUSEPORT, so a
// Hot Reload can bind a new socket on a port this one holds.
func Listen(ctx context.Context, addr string) (*net.TCPListener, error) {
	return listenTCP(ctx, net.ListenConfig{Control: setReusePort}, addr)
}

// setReusePort sets SO_REUSEPORT before bind.
func setReusePort(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	if serr != nil {
		return os.NewSyscallError("setsockopt", serr)
	}
	return nil
}
