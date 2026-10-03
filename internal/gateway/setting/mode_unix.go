// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package setting

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// openKeyFile opens path read-only with O_NONBLOCK, so a FIFO or a device
// whose open waits for a peer cannot block the start, and O_NOCTTY, so a
// terminal never becomes the controlling one. The caller refuses anything
// but a regular file before reading; O_NONBLOCK does not change reads of
// a regular file. Symbolic links are followed: the path is an operator
// setting.
func openKeyFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0) //nolint:gosec // G304: the operator-configured key file.
}

// ownerOnly reports whether m grants no permission to group or others.
func ownerOnly(m fs.FileMode) bool { return m.Perm()&0o077 == 0 }

// checkOwner refuses a file owned by anyone but euid or root: its owner
// could rewrite the key or widen its mode. Root may rewrite any file
// anyway, so a root-owned key adds no writer.
func checkOwner(fi fs.FileInfo, euid int) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 || int64(st.Uid) == int64(euid) {
		return nil
	}
	return fmt.Errorf("owned by uid %d, not by the effective user (uid %d) or root", st.Uid, euid)
}
