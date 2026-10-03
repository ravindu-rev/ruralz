// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package setting

import (
	"io/fs"
	"os"
)

// openKeyFile opens path read-only. There is no O_NONBLOCK open here; the
// caller refuses anything but a regular file before reading.
func openKeyFile(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: the operator-configured key file.
}

// ownerOnly accepts every mode: Unix permission bits do not apply here,
// and ruralzd runs on Linux and macOS only.
func ownerOnly(fs.FileMode) bool { return true }

// checkOwner accepts every file: Unix owners do not apply here.
func checkOwner(fs.FileInfo, int) error { return nil }
