// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && unix

package config

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// clockSockets reports that a controllable-clock ruralzd can be started
// here: it reads its clock commands on an inherited descriptor.
const clockSockets = true

// commandSocket returns a connected stream socket pair: the suite's end as
// a net.Conn, and the end ruralzd inherits as descriptor 3
// (internal/clock.CommandFD). Both are close-on-exec here, so no other
// child inherits them.
func commandSocket() (net.Conn, *os.File, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, fmt.Errorf("clock socket: %w", err)
	}
	suite := os.NewFile(uintptr(fds[0]), "clock-suite")
	conn, err := net.FileConn(suite)
	_ = suite.Close()
	if err != nil {
		_ = syscall.Close(fds[1])
		return nil, nil, fmt.Errorf("clock socket: %w", err)
	}
	return conn, os.NewFile(uintptr(fds[1]), "clock-node"), nil
}
