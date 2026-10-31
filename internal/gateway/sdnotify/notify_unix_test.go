// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package sdnotify

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

// fillQueue fills the never-read queue of s: each new socket writes
// without waiting until a write would block, and the queue is full once a
// fresh socket, whose send buffer is empty, cannot write at all.
func fillQueue(t *testing.T, s *sink) {
	t.Helper()
	for range 10_000 {
		c, err := dialSocket(t.Context(), s.path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		uc, ok := c.(*net.UnixConn)
		if !ok {
			t.Fatalf("datagram socket is a %T", c)
		}
		rc, err := uc.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		for written := 0; ; written++ {
			var werr error
			if err := rc.Write(func(fd uintptr) bool {
				_, werr = syscall.Write(int(fd), []byte("WATCHDOG=1"))
				return true
			}); err != nil {
				t.Fatal(err)
			}
			if errors.Is(werr, syscall.EAGAIN) {
				if written == 0 {
					return
				}
				break
			}
			if werr != nil {
				t.Fatal(werr)
			}
		}
	}
	t.Fatal("the queue never filled")
}

// TestSendBlockedOnFullQueueEndsWithItsContext checks that a send the
// service manager's full queue blocks returns once its context ends, so a
// stuck systemd never stalls a Drain: the context ends after the socket is
// connected, and the write fails with its deadline.
func TestSendBlockedOnFullQueueEndsWithItsContext(t *testing.T) {
	s := newSink(t)
	fillQueue(t, s)
	n, _ := openNotifier(t, s, false)
	dialed := make(chan struct{}, 1)
	n.dial = func(ctx context.Context, socket string) (net.Conn, error) {
		c, err := dialSocket(ctx, socket)
		dialed <- struct{}{}
		return c, err
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- n.Watchdog(ctx) }()
	recv(t, "the socket to connect", dialed)
	cancel()
	if err := recv(t, "the blocked send to return", done); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("send into a full queue = %v, want the write deadline", err)
	}
}
