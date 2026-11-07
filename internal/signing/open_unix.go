// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package signing

import (
	"os"
	"syscall"
)

// openFile opens path read-only with O_NONBLOCK, so a FIFO or a device
// whose open waits for a peer cannot block a start, and O_NOCTTY, so a
// terminal never becomes the controlling one. The caller refuses anything
// but a regular file before reading; O_NONBLOCK does not change reads of
// a regular file. Symbolic links are followed: the path is an operator
// setting or flag.
func openFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0) //nolint:gosec // G304: the operator-configured credential file.
}
