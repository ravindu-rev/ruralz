// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package handover

import "testing"

// TestProcReaders checks the Linux readers the holder and the new process
// use by default: the resident set from /proc/self/statm is positive, and
// the cgroup's memory.max reads without an error, a limit or none.
func TestProcReaders(t *testing.T) {
	n, err := residentBytes()
	if err != nil || n <= 0 {
		t.Errorf("residentBytes() = %d, %v, want a positive size", n, err)
	}
	if limit, has, err := cgroupMemoryMax(); err != nil || (has && limit < 0) {
		t.Errorf("cgroupMemoryMax() = %d, %t, %v", limit, has, err)
	}
	u, err := Meter{}.Usage()
	if err != nil || u.RSSBytes <= 0 {
		t.Errorf("Meter{}.Usage() = %+v, %v, want the resident set", u, err)
	}
}
