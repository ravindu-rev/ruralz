// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package raft

import "os"

// syncDir syncs a directory, so a rename into it survives a crash.
func syncDir(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // G304: a directory the store created under its data directory.
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
