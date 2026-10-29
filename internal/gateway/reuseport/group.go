// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reuseport

import (
	"fmt"
	"net"
	"slices"
	"sync"
)

// Group mirrors one port's reuseport group as the kernel orders this
// process's listening sockets in it: Add appends a socket that just
// started listening, and Remove puts the last socket in the position of
// one that closed, as the kernel does. Positions count this process's
// sockets only, so they match the kernel's while no other process has a
// socket in the group, and a handover successor's socket, which listens
// after this process's sockets, sits at position Len. A Group is safe for
// concurrent use; the caller keeps Add and Remove in step with the
// sockets' listen and close.
type Group struct {
	mu      sync.Mutex
	members []*net.TCPListener
}

// Add records ln, which has just started listening on the group's port.
// Adding a recorded socket again does nothing.
func (g *Group) Add(ln *net.TCPListener) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !slices.Contains(g.members, ln) {
		g.members = append(g.members, ln)
	}
}

// Remove records that ln has closed: the last socket takes its position.
// Removing a socket that is not recorded does nothing.
func (g *Group) Remove(ln *net.TCPListener) {
	g.mu.Lock()
	defer g.mu.Unlock()
	i := slices.Index(g.members, ln)
	if i < 0 {
		return
	}
	last := len(g.members) - 1
	g.members[i] = g.members[last]
	g.members[last] = nil
	g.members = g.members[:last]
}

// Len returns the number of recorded sockets.
func (g *Group) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.members)
}

// Index returns the position of ln in the group, or -1.
func (g *Group) Index(ln *net.TCPListener) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Index(g.members, ln)
}

// Pin steers every new connection of the port to ln, a recorded socket.
func (g *Group) Pin(ln *net.TCPListener) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	i := slices.Index(g.members, ln)
	if i < 0 {
		return fmt.Errorf("reuseport: pin: socket %v is not in the group", ln.Addr())
	}
	return Steer(ln, i)
}

// SteerTo steers every new connection of the port to position index of the
// group, attaching the program through a recorded socket; index Len is
// the first socket another process added after this process's sockets.
// An empty group has no socket to attach through and steers nothing.
func (g *Group) SteerTo(index int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.members) == 0 {
		return nil
	}
	return Steer(g.members[0], index)
}
