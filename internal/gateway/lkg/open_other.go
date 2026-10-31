// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package lkg

import "os"

// openNoFollow opens path for reading; elsewhere than Unix the platform
// has no O_NOFOLLOW, and the regular-file check after the open refuses
// anything else.
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: a fixed name under the data directory.
}
