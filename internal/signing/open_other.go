// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package signing

import "os"

// openFile opens path read-only; elsewhere than Unix there is no
// O_NONBLOCK open, and the caller refuses anything but a regular file
// before reading.
func openFile(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: the operator-configured credential file.
}
