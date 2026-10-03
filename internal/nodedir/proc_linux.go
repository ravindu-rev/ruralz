// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package nodedir

import (
	"fmt"
	"os"
)

// processStartTime returns the calling process's start time, field 22 of
// /proc/self/stat.
func processStartTime() (uint64, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, fmt.Errorf("start time: %w", err)
	}
	return parseStatStartTime(data)
}

// processPIDNamespace returns readlink(/proc/self/ns/pid).
func processPIDNamespace() (string, error) {
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return "", fmt.Errorf("pid namespace: %w", err)
	}
	return ns, nil
}
