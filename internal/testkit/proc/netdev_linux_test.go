// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"
)

// Tests for AddDevice: the netlink encoding and the argument checks
// anywhere, and, inside a network namespace, a dummy device, the veth
// fallback and a kind without a driver, each device up with a non-loopback
// address a socket binds and dials from.

func TestRTAttr(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty":   nil,
		"aligned": {1, 2, 3, 4},
		"padded":  {1, 2, 3, 4, 5},
	} {
		b := rtAttr(7, data)
		if len(b)%4 != 0 || len(b) < 4+len(data) || len(b) > 4+len(data)+3 {
			t.Errorf("%s: rtAttr length %d for %d data bytes, want 4 + data padded to 4", name, len(b), len(data))
			continue
		}
		if l, typ := binary.NativeEndian.Uint16(b[0:2]), binary.NativeEndian.Uint16(b[2:4]); int(l) != 4+len(data) || typ != 7 {
			t.Errorf("%s: rtAttr header = length %d type %d, want %d and 7", name, l, typ, 4+len(data))
		}
		if !bytes.Equal(b[4:4+len(data)], data) || slices.ContainsFunc(b[4+len(data):], func(x byte) bool { return x != 0 }) {
			t.Errorf("%s: rtAttr = %v, want the data then zero padding", name, b)
		}
	}
}

func TestIfInfo(t *testing.T) {
	b := ifInfo(3, syscall.IFF_UP, syscall.IFF_UP|syscall.IFF_BROADCAST)
	if len(b) != syscall.SizeofIfInfomsg || b[0] != syscall.AF_UNSPEC {
		t.Fatalf("ifInfo = %v, want %d bytes of family AF_UNSPEC", b, syscall.SizeofIfInfomsg)
	}
	if index, flags, change := binary.NativeEndian.Uint32(b[4:8]), binary.NativeEndian.Uint32(b[8:12]), binary.NativeEndian.Uint32(b[12:16]); index != 3 || flags != syscall.IFF_UP || change != syscall.IFF_UP|syscall.IFF_BROADCAST {
		t.Fatalf("ifInfo = index %d flags %#x change %#x", index, flags, change)
	}
}

func TestCheckDevice(t *testing.T) {
	for _, tc := range []struct {
		name, device string
		prefix       netip.Prefix
		ok           bool
	}{
		{"short name", "rz0", netip.MustParsePrefix("10.199.0.1/24"), true},
		{"ten bytes", "abcdefghij", netip.MustParsePrefix("10.199.0.1/32"), true},
		{"eleven bytes leave no room for the veth peer", "abcdefghijk", netip.MustParsePrefix("10.199.0.1/24"), false},
		{"empty name", "", netip.MustParsePrefix("10.199.0.1/24"), false},
		{"IPv6", "rz0", netip.MustParsePrefix("2001:db8::1/64"), false},
		{"IPv4-mapped IPv6", "rz0", netip.MustParsePrefix("::ffff:10.199.0.1/120"), false},
		{"zero prefix", "rz0", netip.Prefix{}, false},
	} {
		if err := checkDevice(tc.device, tc.prefix); (err == nil) != tc.ok {
			t.Errorf("%s: checkDevice(%q, %s) = %v, want ok %v", tc.name, tc.device, tc.prefix, err, tc.ok)
		}
	}
}

// TestAddDeviceChild runs inside the namespace TestAddDevice creates.
func TestAddDeviceChild(t *testing.T) {
	if !NetNSChild(t) {
		t.Skip("run by TestAddDevice")
	}
	t.Run("dummy or veth", func(t *testing.T) {
		p := netip.MustParsePrefix("10.199.1.1/24")
		if err := AddDevice("rzt0", p); err != nil {
			t.Fatal(err)
		}
		wantDevice(t, "rzt0", p)
		if err := AddDevice("rzt0", netip.MustParsePrefix("10.199.9.1/24")); !errors.Is(err, syscall.EEXIST) {
			t.Fatalf("AddDevice of an existing device = %v, want EEXIST", err)
		}
		// Refused arguments create nothing.
		if err := AddDevice("rzt9", netip.MustParsePrefix("2001:db8::1/64")); err == nil {
			t.Fatal("AddDevice with an IPv6 prefix: want error")
		}
		if _, err := net.InterfaceByName("rzt9"); err == nil {
			t.Fatal("AddDevice created a device for refused arguments")
		}
	})
	t.Run("veth fallback", func(t *testing.T) {
		p := netip.MustParsePrefix("10.199.2.1/24")
		err := addDevice("rzt1", p, "rz-no-such-kind", "veth")
		if errors.Is(err, syscall.EOPNOTSUPP) {
			t.Skipf("the kernel has no veth driver: %v", err)
		}
		if err != nil {
			t.Fatal(err)
		}
		wantDevice(t, "rzt1", p)
		peer, err := net.InterfaceByName("rzt1" + PeerSuffix)
		if err != nil || peer.Flags&net.FlagUp == 0 {
			t.Fatalf("veth peer = %+v, %v; want up", peer, err)
		}
	})
	t.Run("no driver", func(t *testing.T) {
		err := addDevice("rzt2", netip.MustParsePrefix("10.199.3.1/24"), "rz-no-such-kind")
		if !errors.Is(err, syscall.EOPNOTSUPP) {
			t.Fatalf("addDevice of a kind without a driver = %v, want EOPNOTSUPP", err)
		}
		if _, err := net.InterfaceByName("rzt2"); err == nil {
			t.Fatal("addDevice created a device of a kind without a driver")
		}
	})
}

// wantDevice checks that the device name is up and carries the address of
// p, and that a socket bound to that address accepts a connection dialed
// from it.
func wantDevice(t *testing.T, name string, p netip.Prefix) {
	t.Helper()
	ifc, err := net.InterfaceByName(name)
	if err != nil || ifc.Flags&net.FlagUp == 0 {
		t.Fatalf("device %s = %+v, %v; want up", name, ifc, err)
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(addrs))
	for _, a := range addrs {
		got = append(got, a.String())
	}
	if !slices.Contains(got, p.String()) {
		t.Fatalf("addresses of %s = %v, want %s", name, got, p)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort(p.Addr().String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	c, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: p.Addr().AsSlice()}}).DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	s, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if from, _ := netip.ParseAddrPort(s.RemoteAddr().String()); from.Addr() != p.Addr() {
		t.Fatalf("connection from %s, want %s", s.RemoteAddr(), p.Addr())
	}
}

func TestAddDevice(t *testing.T) { // a non-loopback address inside a network namespace
	user := netnsUser(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var out bytes.Buffer
	_, err := RunInNetNS(ctx, NetNSOptions{TestName: "TestAddDeviceChild", User: user, Output: &out})
	if errors.Is(err, ErrChildSkipped) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatalf("RunInNetNS = %v\n%s", err, out.String())
	}
}

// netnsUser skips unless this process can create a network namespace,
// alone or inside a user namespace, and reports whether it needs the user
// namespace. It probes with real children (this test binary running no
// test).
func netnsUser(t *testing.T) bool {
	t.Helper()
	probe := func(attr *syscall.SysProcAttr) error {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$", "-test.count=1") //nolint:gosec // G204: this test binary
		cmd.SysProcAttr = attr
		return cmd.Run()
	}
	if probe(&syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET}) == nil {
		return false
	}
	err := probe(&syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWNET | syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	})
	if err != nil {
		t.Skipf("network namespaces unavailable: %v", err)
	}
	return true
}
