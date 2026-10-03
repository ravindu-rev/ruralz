// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package resolver

import (
	"io/fs"
	"os"
	"syscall"
)

// openFlags opens without blocking on a FIFO swapped in after the Stat and
// without acquiring a controlling terminal.
const openFlags = os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOCTTY

// ownerModeChecked reports that owner and mode bits are meaningful here.
const ownerModeChecked = true

// fileIdentity returns the device, inode and owner of fi.
func fileIdentity(fi fs.FileInfo) (dev, ino uint64, uid uint32, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), st.Uid, true //nolint:unconvert // Dev and Ino widths differ across Unix platforms.
}
