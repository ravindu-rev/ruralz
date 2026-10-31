// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package reuseport

import (
	"context"
	"net"
)

const (
	sharedPorts = false
	steering    = false
)

// Listen opens a plain TCP socket listening on addr: this platform shares
// no port, so a second socket on the port fails to bind.
func Listen(ctx context.Context, addr string) (*net.TCPListener, error) {
	return listenTCP(ctx, net.ListenConfig{}, addr)
}
