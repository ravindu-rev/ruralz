// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reuseport

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"testing"
)

// TestParseAddr checks the bind address rule: an IP literal without zone
// or an empty host, and a port from 0 to 65535; host names are refused so
// binding never waits on name resolution.
func TestParseAddr(t *testing.T) {
	tests := []struct {
		addr    string
		wantIP  string // "" means every interface
		port    int
		wantErr bool
	}{
		{addr: ":8443", port: 8443},
		{addr: ":0", port: 0},
		{addr: "127.0.0.1:8080", wantIP: "127.0.0.1", port: 8080},
		{addr: "[::1]:9901", wantIP: "::1", port: 9901},
		{addr: "[::ffff:127.0.0.1]:80", wantIP: "127.0.0.1", port: 80},
		{addr: ":65535", port: 65535},
		{addr: "localhost:8080", wantErr: true},
		{addr: "api.shop.example:443", wantErr: true},
		{addr: "127.0.0.1", wantErr: true},
		{addr: ":65536", wantErr: true},
		{addr: ":-1", wantErr: true},
		{addr: ":http", wantErr: true},
		{addr: "[fe80::1%eth0]:80", wantErr: true},
		{addr: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got, err := parseAddr(tt.addr)
			if tt.wantErr {
				if !errors.Is(err, ErrAddress) {
					t.Fatalf("parseAddr(%q) error = %v, want ErrAddress", tt.addr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAddr(%q) error = %v", tt.addr, err)
			}
			var want netip.Addr
			if tt.wantIP != "" {
				want = netip.MustParseAddr(tt.wantIP)
			}
			if got.ip != want || got.port != tt.port {
				t.Errorf("parseAddr(%q) = %v port %d, want %v port %d", tt.addr, got.ip, got.port, want, tt.port)
			}
		})
	}
}

// TestListenServesConnections checks that Listen returns a listening TCP
// socket on the address, with the port the kernel chose for port 0.
func TestListenServesConnections(t *testing.T) {
	ln, err := Listen(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().(*net.TCPAddr)
	if addr.Port == 0 || !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("Addr() = %v, want 127.0.0.1 and a chosen port", addr)
	}
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = c.Close() }()
		_, err = c.Write([]byte("ok"))
		done <- err
	}()
	var d net.Dialer
	c, err := d.DialContext(t.Context(), "tcp", addr.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "ok" {
		t.Fatalf("read = %q, %v; want \"ok\"", b, err)
	}
	if err := <-done; err != nil {
		t.Fatalf("accept side: %v", err)
	}
}

// TestListenAndBindRefuseHostNames checks that Listen and Bind refuse a
// host name before any socket call.
func TestListenAndBindRefuseHostNames(t *testing.T) {
	if _, err := Listen(t.Context(), "localhost:0"); !errors.Is(err, ErrAddress) {
		t.Errorf("Listen(localhost:0) error = %v, want ErrAddress", err)
	}
	if _, err := Bind(t.Context(), "localhost:0"); !errors.Is(err, ErrAddress) {
		t.Errorf("Bind(localhost:0) error = %v, want ErrAddress", err)
	}
}

// TestSecondSocketOnPort checks the platform's port sharing: where
// SharedPorts holds, a second socket binds and listens on the port of a
// listening one (the Hot Reload replacement); elsewhere it fails to bind.
func TestSecondSocketOnPort(t *testing.T) {
	first, err := Listen(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = first.Close() }()
	addr := "127.0.0.1:" + strconv.Itoa(first.Addr().(*net.TCPAddr).Port)
	second, err := Listen(t.Context(), addr)
	if SharedPorts() {
		if err != nil {
			t.Fatalf("second Listen(%s) error = %v, want a shared port", addr, err)
		}
		_ = second.Close()
		return
	}
	if err == nil {
		_ = second.Close()
		t.Fatalf("second Listen(%s) succeeded on a platform without SO_REUSEPORT", addr)
	}
}

// TestBindAndSteerUnsupportedOffLinux checks that binding without
// listening and BPF steering report errors.ErrUnsupported where the
// platform lacks them.
func TestBindAndSteerUnsupportedOffLinux(t *testing.T) {
	if Steering() {
		t.Skip("this platform steers")
	}
	if _, err := Bind(t.Context(), "127.0.0.1:0"); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Bind error = %v, want errors.ErrUnsupported", err)
	}
	ln, err := Listen(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if err := Steer(ln, 0); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Steer error = %v, want errors.ErrUnsupported", err)
	}
}

// TestGroupOrder checks Group's model of the kernel's reuseport array: Add
// appends, Remove puts the last socket in the freed position, and adding
// or removing twice changes nothing.
func TestGroupOrder(t *testing.T) {
	lns := make([]*net.TCPListener, 4)
	for i := range lns {
		ln, err := Listen(t.Context(), "127.0.0.1:0")
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		defer func() { _ = ln.Close() }()
		lns[i] = ln
	}
	a, b, c, d := lns[0], lns[1], lns[2], lns[3]
	var g Group
	g.Add(a)
	g.Add(b)
	g.Add(c)
	g.Add(b)
	order := func() []int {
		return []int{g.Index(a), g.Index(b), g.Index(c), g.Index(d)}
	}
	if got, want := order(), []int{0, 1, 2, -1}; !slices.Equal(got, want) {
		t.Fatalf("after adds: positions = %v, want %v", got, want)
	}
	g.Remove(a)
	if got, want := order(), []int{-1, 1, 0, -1}; !slices.Equal(got, want) {
		t.Fatalf("after removing the first: positions = %v, want %v (the last moves into its slot)", got, want)
	}
	g.Remove(a)
	g.Remove(d)
	g.Add(d)
	if got, want := order(), []int{-1, 1, 0, 2}; !slices.Equal(got, want) {
		t.Fatalf("after re-removal and an add: positions = %v, want %v", got, want)
	}
	g.Remove(d)
	if got, want := order(), []int{-1, 1, 0, -1}; !slices.Equal(got, want) {
		t.Fatalf("after removing the last: positions = %v, want %v", got, want)
	}
	if g.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", g.Len())
	}
	if err := g.Pin(a); err == nil {
		t.Error("Pin of a socket outside the group succeeded")
	}
	var empty Group
	if err := empty.SteerTo(0); err != nil {
		t.Errorf("SteerTo on an empty group = %v, want nil", err)
	}
}
