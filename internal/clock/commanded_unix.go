// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package clock

import (
	"fmt"
	"os"
	"syscall"
)

// openCommandFD checks that fd is an open socket before anything owns it,
// since an *os.File closes its descriptor when collected, and returns it
// in non-blocking mode, so reads wait in the runtime's poller and Close
// ends a pending read.
func openCommandFD(fd uintptr) (*os.File, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(fd), &st); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoCommandDescriptor, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
		return nil, fmt.Errorf("%w: file descriptor %d is not a socket", ErrNoCommandDescriptor, fd)
	}
	if err := syscall.SetNonblock(int(fd), true); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoCommandDescriptor, err)
	}
	return os.NewFile(fd, "clock commands"), nil
}
