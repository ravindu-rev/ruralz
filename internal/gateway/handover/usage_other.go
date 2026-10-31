// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package handover

// residentBytes returns 0: only Linux reports the resident set through
// /proc, and only Linux hands over.
func residentBytes() (int64, error) { return 0, nil }

// cgroupMemoryMax reports no cgroup limit: cgroups are Linux only.
func cgroupMemoryMax() (int64, bool, error) { return 0, false, nil }
