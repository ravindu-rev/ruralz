// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package reuseport

import (
	"errors"
	"net"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// dials is the number of connections each routing check makes; a hashed
// group would spread them over its sockets.
const dials = 16

// listenOn listens on the port of addr with SO_REUSEPORT.
func listenOn(t *testing.T, addr string) *net.TCPListener {
	t.Helper()
	ln, err := Listen(t.Context(), addr)
	if err != nil {
		t.Fatalf("Listen(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// portAddr returns "127.0.0.1:<port of ln>".
func portAddr(ln *net.TCPListener) string {
	return "127.0.0.1:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// route dials addr dials times and returns, per dial, the index in lns of
// the listener that accepted it. Every listener accepts in its own
// goroutine until all dials are accepted; a past deadline then stops them
// and is cleared again. The wait is bounded only so a misrouted
// connection fails the test instead of hanging it.
func route(t *testing.T, addr string, lns ...*net.TCPListener) []int {
	t.Helper()
	type hit struct{ ln, port int }
	hits := make(chan hit, dials)
	var wg sync.WaitGroup
	for i, ln := range lns {
		wg.Go(func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				hits <- hit{ln: i, port: c.RemoteAddr().(*net.TCPAddr).Port}
				_ = c.Close()
			}
		})
	}
	stop := func() {
		for _, ln := range lns {
			_ = ln.SetDeadline(time.Unix(1, 0))
		}
		wg.Wait()
		for _, ln := range lns {
			_ = ln.SetDeadline(time.Time{})
		}
	}
	var d net.Dialer
	ports := make([]int, dials)
	for i := range dials {
		c, err := d.DialContext(t.Context(), "tcp", addr)
		if err != nil {
			stop()
			t.Fatalf("dial %d: %v", i, err)
		}
		defer func() { _ = c.Close() }()
		ports[i] = c.LocalAddr().(*net.TCPAddr).Port
	}
	byPort := make(map[int]int, dials)
	bound := time.After(10 * time.Second)
	for range dials {
		select {
		case h := <-hits:
			byPort[h.port] = h.ln
		case <-bound:
			stop()
			t.Fatalf("only %d of %d connections reached the listeners", len(byPort), dials)
		}
	}
	stop()
	out := make([]int, dials)
	for i, p := range ports {
		out[i] = byPort[p]
	}
	return out
}

// all returns dials copies of i.
func all(i int) []int { return slices.Repeat([]int{i}, dials) }

// TestBindHoldsPortWithoutListening checks "Bind, not listen" (ADR-0015):
// a bound socket refuses connections until Listen, and Bind fails on a
// port that a socket without SO_REUSEPORT listens on, so port errors
// surface before any listen.
func TestBindHoldsPortWithoutListening(t *testing.T) {
	s, err := Bind(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	defer func() { _ = s.Close() }()
	addr := s.Addr().String()
	var d net.Dialer
	if c, err := d.DialContext(t.Context(), "tcp", addr); err == nil {
		_ = c.Close()
		t.Fatal("dial to a bound socket that does not listen succeeded")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial error = %v, want ECONNREFUSED", err)
	}
	ln, err := s.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if got := route(t, addr, ln); !slices.Equal(got, all(0)) {
		t.Fatalf("after Listen: routed to %v, want all to the socket", got)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close after Listen = %v, want nil", err)
	}
	if _, err := s.Listen(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("second Listen error = %v, want net.ErrClosed", err)
	}

	var lc net.ListenConfig
	plain, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("plain listen: %v", err)
	}
	defer func() { _ = plain.Close() }()
	if s2, err := Bind(t.Context(), plain.Addr().String()); err == nil {
		_ = s2.Close()
		t.Fatal("Bind on a port a plain socket listens on succeeded")
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("Bind error = %v, want EADDRINUSE", err)
	}
}

// TestBoundSocketJoinsGroupAtListen checks that a socket bound beside a
// listening one receives nothing (it is not in the reuseport group) and
// joins the group at the end when it listens, where steering reaches it.
func TestBoundSocketJoinsGroupAtListen(t *testing.T) {
	l0 := listenOn(t, "127.0.0.1:0")
	addr := portAddr(l0)
	b, err := Bind(t.Context(), addr)
	if err != nil {
		t.Fatalf("Bind beside a listener: %v", err)
	}
	defer func() { _ = b.Close() }()
	if got := route(t, addr, l0); !slices.Equal(got, all(0)) {
		t.Fatalf("with a bound socket: routed to %v, want all to the listener", got)
	}
	l1, err := b.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = l1.Close() }()
	if err := Steer(l0, 1); err != nil {
		t.Fatalf("Steer: %v", err)
	}
	if got := route(t, addr, l0, l1); !slices.Equal(got, all(1)) {
		t.Fatalf("steered to position 1: routed to %v, want all to the new socket", got)
	}
}

// TestSteerSelectsPosition checks that Steer, attached through any socket
// of the group, sends every new connection to the socket at the position
// its program returns.
func TestSteerSelectsPosition(t *testing.T) {
	l0 := listenOn(t, "127.0.0.1:0")
	addr := portAddr(l0)
	l1 := listenOn(t, addr)
	if err := Steer(l0, 1); err != nil {
		t.Fatalf("Steer(l0, 1): %v", err)
	}
	if got := route(t, addr, l0, l1); !slices.Equal(got, all(1)) {
		t.Fatalf("Steer to 1: routed to %v", got)
	}
	if err := Steer(l1, 0); err != nil {
		t.Fatalf("Steer(l1, 0): %v", err)
	}
	if got := route(t, addr, l0, l1); !slices.Equal(got, all(0)) {
		t.Fatalf("Steer to 0: routed to %v", got)
	}
	if err := Steer(l0, -1); err == nil {
		t.Error("Steer to a negative position succeeded")
	}
}

// TestGroupMirrorsKernelOrder checks Group against the kernel: after the
// first of three sockets closes, the kernel moves the last one into its
// position, and Pin of each remaining socket still routes every new
// connection to that socket.
func TestGroupMirrorsKernelOrder(t *testing.T) {
	l0 := listenOn(t, "127.0.0.1:0")
	addr := portAddr(l0)
	l1 := listenOn(t, addr)
	l2 := listenOn(t, addr)
	var g Group
	for _, ln := range []*net.TCPListener{l0, l1, l2} {
		g.Add(ln)
	}
	if err := g.Pin(l0); err != nil {
		t.Fatalf("Pin(l0): %v", err)
	}
	if got := route(t, addr, l0, l1, l2); !slices.Equal(got, all(0)) {
		t.Fatalf("pinned to l0: routed to %v", got)
	}
	if err := l0.Close(); err != nil {
		t.Fatalf("close l0: %v", err)
	}
	g.Remove(l0)
	for i, ln := range []*net.TCPListener{l1, l2} {
		if err := g.Pin(ln); err != nil {
			t.Fatalf("Pin: %v", err)
		}
		if got := route(t, addr, l1, l2); !slices.Equal(got, all(i)) {
			t.Fatalf("pinned to socket %d after the close: routed to %v", i+1, got)
		}
	}
}

// TestSteerToSocketAfterGroup checks the handover steering of ADR-0015: a
// socket another process adds after this process's sockets sits at
// position Len, so SteerTo(Len) moves every new connection to it.
func TestSteerToSocketAfterGroup(t *testing.T) {
	holder := listenOn(t, "127.0.0.1:0")
	addr := portAddr(holder)
	var g Group
	g.Add(holder)
	if err := g.Pin(holder); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	b, err := Bind(t.Context(), addr)
	if err != nil {
		t.Fatalf("successor Bind: %v", err)
	}
	successor, err := b.Listen()
	if err != nil {
		t.Fatalf("successor Listen: %v", err)
	}
	defer func() { _ = successor.Close() }()
	if got := route(t, addr, holder, successor); !slices.Equal(got, all(0)) {
		t.Fatalf("pinned to the holder: routed to %v", got)
	}
	if err := g.SteerTo(g.Len()); err != nil {
		t.Fatalf("SteerTo: %v", err)
	}
	if got := route(t, addr, holder, successor); !slices.Equal(got, all(1)) {
		t.Fatalf("steered to the successor: routed to %v", got)
	}
}

// TestListenEveryInterfaceIsDualStack checks that an empty host binds
// every interface with one socket that accepts IPv4 and, where the host
// has IPv6 loopback, IPv6.
func TestListenEveryInterfaceIsDualStack(t *testing.T) {
	ln := listenOn(t, ":0")
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if got := route(t, "127.0.0.1:"+port, ln); !slices.Equal(got, all(0)) {
		t.Fatalf("IPv4: routed to %v", got)
	}
	var lc net.ListenConfig
	probe, err := lc.Listen(t.Context(), "tcp", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	_ = probe.Close()
	if got := route(t, "[::1]:"+port, ln); !slices.Equal(got, all(0)) {
		t.Fatalf("IPv6: routed to %v", got)
	}
}
