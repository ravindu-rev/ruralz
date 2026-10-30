// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package reload

import (
	"net"
	"strconv"
	"testing"
)

// TestAdminSocketSuccessorListensOnlyWhenAccepted: a handover successor
// binds its admin port without listening, so nothing queues on a process
// that may exit; Listen serves it, and a move held until then serves the
// new port and closes the old one.
func TestAdminSocketSuccessorListensOnlyWhenAccepted(t *testing.T) {
	a, _ := newAdminSocket(t, false, nil)
	port := freePort(t)
	if err := a.Bind(t.Context(), port); err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if got := get(t, addr); got != "" {
		t.Fatalf("a successor's admin port answers %q before Listen", got)
	}
	newPort := freePort(t)
	sw, err := a.Prepare(t.Context(), adminCompiled(newPort))
	if err != nil {
		t.Fatal(err)
	}
	newAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(newPort))
	if got := get(t, newAddr); got != "" {
		t.Fatalf("the moved port answers %q before Listen", got)
	}
	// Bound.Listen commits the held switch, then listens on the current
	// socket.
	if err := sw.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.Listen(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := get(t, newAddr); got != "admin" {
		t.Fatalf("the moved port answers %q after Listen", got)
	}
	if err := a.Pin(); err != nil {
		t.Fatalf("Pin = %v", err)
	}
	if err := a.SteerToSuccessor(); err != nil {
		t.Fatalf("SteerToSuccessor = %v", err)
	}
}
