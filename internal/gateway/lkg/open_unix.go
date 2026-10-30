// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package lkg

import (
	"os"
	"syscall"
)

// openNoFollow opens path for reading without following a final symbolic
// link and without blocking on a FIFO, so a planted link or FIFO never
// stalls a boot or reads another file.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY|syscall.O_NOFOLLOW, 0) //nolint:gosec // G304: a fixed name under the data directory.
}
