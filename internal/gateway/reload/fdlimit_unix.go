// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package reload

import "syscall"

// fileLimit returns the process's soft RLIMIT_NOFILE.
func fileLimit() (uint64, bool) {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0, false
	}
	return uint64(r.Cur), true //nolint:unconvert // Rlimit.Cur is int64 on some platforms.
}
