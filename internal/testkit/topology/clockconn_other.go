// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package topology

import (
	"errors"
	"fmt"
	"net"
	"os"
)

// commandSocket fails: a process inherits extra file descriptors on Unix
// only, so the controllable-clock build runs there.
func commandSocket() (net.Conn, *os.File, error) {
	return nil, nil, fmt.Errorf("topology: clock socket: %w", errors.ErrUnsupported)
}

// isConnReset reports a write or read failure that means the peer closed.
func isConnReset(error) bool { return false }
