// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package setting

import (
	"syscall"
	"testing"
)

// mkfifo creates a named pipe at path and reports true.
func mkfifo(t *testing.T, path string) bool {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return true
}
