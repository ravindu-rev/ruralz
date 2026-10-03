// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package adminauth

import (
	"os"
	"syscall"
)

// openNoBlock opens path read-only with O_NONBLOCK, so a FIFO or a device
// whose open waits for a peer cannot block the caller; the caller checks
// that the opened file is regular before reading (O_NONBLOCK does not
// change reads of a regular file).
func openNoBlock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the path is an operator process setting (RURALZ_ADMIN_*), never request input.
}
