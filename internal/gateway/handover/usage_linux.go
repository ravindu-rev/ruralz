// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package handover

import (
	"fmt"
	"os"
)

// residentBytes returns this process's resident set size from
// /proc/self/statm.
func residentBytes() (int64, error) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, fmt.Errorf("handover: %w", err)
	}
	return parseStatm(b, os.Getpagesize())
}

// cgroupMemoryMax returns memory.max of this process's cgroup v2.
func cgroupMemoryMax() (int64, bool, error) {
	return memoryMaxAt("/proc/self/cgroup", "/sys/fs/cgroup")
}
