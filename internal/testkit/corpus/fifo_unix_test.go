// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package corpus

import (
	"syscall"
	"testing"
)

// mkfifo creates a named pipe; reading it would block, so Load must skip it.
func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
}
