// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package reload

// fileLimit reports no limit: the platform has no RLIMIT_NOFILE.
func fileLimit() (uint64, bool) { return 0, false }
