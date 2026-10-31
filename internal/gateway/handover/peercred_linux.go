// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package handover

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// maxSocketPath is the longest path of a Unix socket: sun_path holds 108
// bytes with the terminating NUL.
const maxSocketPath = 107

// peerCredentials returns the user ID and process ID of the process at
// the other end of c, as the kernel recorded them when the connection was
// made (SO_PEERCRED, unix(7)).
func peerCredentials(c *net.UnixConn) (uid int64, pid int, err error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, 0, fmt.Errorf("handover: peer credentials: %w", err)
	}
	var cred *unix.Ucred
	var cerr error
	if err := rc.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, fmt.Errorf("handover: peer credentials: %w", err)
	}
	if cerr != nil {
		return 0, 0, fmt.Errorf("handover: peer credentials: %w", os.NewSyscallError("getsockopt", cerr))
	}
	return int64(cred.Uid), int(cred.Pid), nil
}
