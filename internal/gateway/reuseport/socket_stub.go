// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package reuseport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
)

// Socket is a bound socket that does not listen yet; Bind creates one on
// Linux only.
type Socket struct{}

// Bind returns errors.ErrUnsupported: binding without listening, and the
// handover it serves, are Linux only.
func Bind(_ context.Context, addr string) (*Socket, error) {
	if _, err := parseAddr(addr); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("reuseport: bind without listen: %w", errors.ErrUnsupported)
}

// Addr returns nil: no Socket exists here.
func (*Socket) Addr() net.Addr { return nil }

// Listen returns errors.ErrUnsupported.
func (*Socket) Listen() (*net.TCPListener, error) {
	return nil, fmt.Errorf("reuseport: listen: %w", errors.ErrUnsupported)
}

// Close does nothing.
func (*Socket) Close() error { return nil }

// Steer returns errors.ErrUnsupported: classic BPF steering is Linux only.
func Steer(syscall.Conn, int) error {
	return fmt.Errorf("reuseport: steer: %w", errors.ErrUnsupported)
}

// Probe checks that no socket listens on addr's port with a plain socket,
// without SO_REUSEPORT, closed at once. Binding without listening is
// Linux only, so the probe listens for that instant; this platform runs
// ruralzd for development only.
func Probe(ctx context.Context, addr string) error {
	ln, err := listenTCP(ctx, net.ListenConfig{}, addr)
	if err != nil {
		return err
	}
	return ln.Close()
}

// listenTCP listens on addr through lc.
func listenTCP(ctx context.Context, lc net.ListenConfig, addr string) (*net.TCPListener, error) {
	if _, err := parseAddr(addr); err != nil {
		return nil, err
	}
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		_ = ln.Close()
		return nil, fmt.Errorf("reuseport: %T is not a TCP listener", ln)
	}
	return tl, nil
}
