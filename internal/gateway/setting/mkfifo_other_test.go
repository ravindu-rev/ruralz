// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix || solaris || aix

package setting

import "testing"

// mkfifo reports false: package syscall has no Mkfifo here, so the FIFO
// cases are skipped.
func mkfifo(t *testing.T, _ string) bool {
	t.Helper()
	return false
}
