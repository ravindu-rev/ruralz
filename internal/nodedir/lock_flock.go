// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package nodedir

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile opens (creating with FilePerm) the lock file and takes an
// exclusive, non-blocking BSD flock on it (R-26). A BSD lock belongs to
// the open file description, so a second open in the same process
// conflicts too, and /proc/locks shows it as FLOCK with the holder's PID,
// which ruralz node drain verifies (spec 10 requirement 96). The file is
// close-on-exec, so no child inherits the lock.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, FilePerm) //nolint:gosec // G304: the lock file under the data directory.
	if err != nil {
		return nil, fmt.Errorf("nodedir: %w", err)
	}
	if err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, path)
		}
		return nil, fmt.Errorf("nodedir: flock %s: %w", path, err)
	}
	return f, nil
}

// unlockFile drops the lock and closes the file.
func unlockFile(f *os.File) error {
	err := flock(f, syscall.LOCK_UN)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("nodedir: unlock: %w", err)
	}
	return nil
}

// flock applies how to f's descriptor, retrying on EINTR.
func flock(f *os.File, how int) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) {
		for {
			ferr = syscall.Flock(int(fd), how)
			if !errors.Is(ferr, syscall.EINTR) {
				return
			}
		}
	}); err != nil {
		return err
	}
	return ferr
}
