// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix || solaris || aix

package nodedir

import (
	"errors"
	"fmt"
	"os"
)

// lockFile is unsupported without BSD flock: ruralzd runs on Linux and
// macOS, and the ruralz CLI never locks.
func lockFile(path string) (*os.File, error) {
	return nil, fmt.Errorf("nodedir: locking %s: %w", path, errors.ErrUnsupported)
}

// unlockFile closes f; lockFile never returns one here.
func unlockFile(f *os.File) error { return f.Close() }
