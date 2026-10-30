// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin

package harness

// InheritOpenFiles reports no open-file limit where the platform has no
// RLIMIT_NOFILE the harness reads: 0, which CheckOpenFiles treats as
// unknown.
func InheritOpenFiles() (uint64, error) { return 0, nil }
