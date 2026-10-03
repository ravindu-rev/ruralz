// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package adminauth

import "os"

// openNoBlock opens path read-only. Outside unix there is no O_NONBLOCK
// open; the caller has already checked with a stat that path is a regular
// file, and checks the opened file again before reading.
func openNoBlock(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: the path is an operator process setting (RURALZ_ADMIN_*), never request input.
}
