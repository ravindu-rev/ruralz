// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package nodedir

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkDir rejects an existing directory that the effective user does not
// own (spec 04 section 3), or that every user may write, sticky bit or
// not: either could replace or squat the lock, holder.json or the
// Last-Known-Good under the holder. A group-writable directory is
// accepted: the group is the operator's choice (systemd
// StateDirectoryMode=0770, a volume shared through its group ID).
func checkDir(fi fs.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int64(st.Uid) != int64(os.Geteuid()) {
		return fmt.Errorf("%w (uid %d, euid %d)", ErrNotOwner, st.Uid, os.Geteuid())
	}
	if fi.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%w (mode %v)", ErrWritableByOthers, fi.Mode().Perm())
	}
	return nil
}

// openNoFollow opens a file under the data directory for reading.
// O_NOFOLLOW refuses a final symbolic link, so a link planted in place of
// holder.json or node-id cannot make a reader (ruralz node drain, perhaps
// run as root) read another file; O_NONBLOCK keeps a FIFO from blocking
// the open; O_NOCTTY keeps a terminal from becoming the controlling one.
// The caller refuses anything but a regular file before reading.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY|syscall.O_NOFOLLOW, 0) //nolint:gosec // G304: a fixed name under the data directory.
}

// checkOneLink refuses a file with more than one hard link. holder.json is
// always written by rename (WriteFileAtomic), so it has exactly one; a
// hard link planted in its place passes O_NOFOLLOW and the regular-file
// check, and where fs.protected_hardlinks is off it could make a reader
// running as root (ruralz node drain) echo another file's content.
func checkOneLink(fi fs.FileInfo) error {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Nlink != 1 {
		return fmt.Errorf("has %d hard links, want 1", st.Nlink)
	}
	return nil
}

// syncDir fsyncs a directory so a rename or link in it is durable.
func syncDir(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // G304: a directory under the data directory.
	if err != nil {
		return fmt.Errorf("nodedir: %w", err)
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("nodedir: syncing %s: %w", dir, err)
	}
	return nil
}
