// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package geoip

import "os"

// openNoBlock opens path read-only. Outside unix there is no O_NONBLOCK
// open; the caller checks that the opened file is regular before reading.
func openNoBlock(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: the path is the operator's RURALZ_GEOIP_DATABASE_FILE, never request input.
}
