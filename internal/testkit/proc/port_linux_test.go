// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package proc

import (
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"
)

// On Linux FreePort holds its port: a socket without SO_REUSEADDR, as a
// connection's or another test's, cannot bind it, while a listener that
// sets SO_REUSEADDR binds and listens.
func TestFreePortHoldsThePort(t *testing.T) {
	port := FreePort(t)
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("a bind without SO_REUSEADDR to the held port %d = %v, want EADDRINUSE", port, err)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("a listener on the held port: %v", err)
	}
	_ = ln.Close()
}
