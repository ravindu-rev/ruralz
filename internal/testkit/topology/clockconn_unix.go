// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package topology

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// commandSocket returns a connected stream socket pair: the harness's end
// as a net.Conn, which takes deadlines, and the Node's end as the file the
// process inherits as descriptor 3 (internal/clock.CommandFD). Both are
// close-on-exec here, so no other child inherits them.
func commandSocket() (net.Conn, *os.File, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, fmt.Errorf("topology: clock socket: %w", err)
	}
	harness := os.NewFile(uintptr(fds[0]), "clock-harness")
	conn, err := net.FileConn(harness)
	_ = harness.Close()
	if err != nil {
		_ = syscall.Close(fds[1])
		return nil, nil, fmt.Errorf("topology: clock socket: %w", err)
	}
	return conn, os.NewFile(uintptr(fds[1]), "clock-node"), nil
}

// isConnReset reports a write or read failure that means the peer closed.
func isConnReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
