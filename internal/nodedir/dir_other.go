// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package nodedir

import (
	"io/fs"
	"os"
)

// checkDir accepts every directory: ownership and Unix permission bits do
// not apply (ruralzd runs on Linux; the CLI reads holder.json only).
func checkDir(fs.FileInfo) error { return nil }

// openNoFollow opens a file under the data directory for reading. There
// is no O_NOFOLLOW or O_NONBLOCK open here; the caller refuses anything
// but a regular file before reading.
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: a fixed name under the data directory.
}

// checkOneLink accepts every file: link counts are not checked here.
func checkOneLink(fs.FileInfo) error { return nil }

// syncDir is a no-op: directories cannot be fsynced here.
func syncDir(string) error { return nil }
