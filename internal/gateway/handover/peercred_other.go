// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package handover

import (
	"errors"
	"fmt"
	"net"
)

// maxSocketPath is the longest path of a Unix socket on the BSDs and
// macOS: sun_path holds 104 bytes with the terminating NUL.
const maxSocketPath = 103

// peerCredentials returns errors.ErrUnsupported: the in-place handover is
// Linux only (docs/adr/0015-zero-downtime-upgrades-so-reuseport.md), so
// without SO_PEERCRED every peer is refused.
func peerCredentials(*net.UnixConn) (uid int64, pid int, err error) {
	return 0, 0, fmt.Errorf("handover: peer credentials: %w", errors.ErrUnsupported)
}
