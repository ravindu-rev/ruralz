// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package resolver

import (
	"io/fs"
	"os"
)

// openFlags opens read-only.
const openFlags = os.O_RDONLY

// ownerModeChecked is false: owner and mode bits are synthesized here.
const ownerModeChecked = false

// fileIdentity reports no identity outside Unix.
func fileIdentity(fs.FileInfo) (dev, ino uint64, uid uint32, ok bool) { return 0, 0, 0, false }
