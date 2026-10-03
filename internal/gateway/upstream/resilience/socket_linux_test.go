// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package resilience

import (
	"context"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestClassifyBlackHoledDial_05Req39 dials a listener whose accept queue
// is full, so the SYN is dropped and the dial runs into the dial timeout:
// kind connect, never timeout (05 reqs 24 and 39).
func TestClassifyBlackHoledDial_05Req39(t *testing.T) {
	t.Parallel()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socket: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	in4, ok := sa.(*syscall.SockaddrInet4)
	if !ok {
		t.Fatalf("socket name %T", sa)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(in4.Port))
	// Fill the accept queue; nothing ever accepts.
	d := &net.Dialer{Timeout: 200 * time.Millisecond}
	for range 4 {
		c, err := d.DialContext(context.Background(), "tcp", addr)
		if err != nil {
			break
		}
		t.Cleanup(func() { _ = c.Close() })
	}
	tr := transport(t, nil, nil)
	tr.DialContext = (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext // 1 s in production
	start := time.Now()
	k, err := attempt(context.Background(), t, tr, "http://"+addr+"/")
	if err == nil {
		t.Fatal("the dial succeeded")
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Skipf("the kernel refused at once (%v); no black hole here", err)
	}
	if k != KindConnect {
		t.Fatalf("kind %v for %v", k, err)
	}
}
