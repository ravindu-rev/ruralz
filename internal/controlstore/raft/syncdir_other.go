// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package raft

// syncDir is a no-op where directories cannot be synced; the CLI that
// restores on such a host still renames objects into place atomically.
func syncDir(string) error { return nil }
