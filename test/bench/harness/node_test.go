// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"testing"
)

// TestParseHeapAlloc covers the live heap R1 reads from a debug=1 heap
// profile after a forced collection: the "# HeapAlloc = N" total among
// the other runtime.MemStats lines, or an error without a usable one.
func TestParseHeapAlloc(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{"profile", "heap profile: 3: 4096 [10: 40960] @ heap/1048576\n1: 4096 [1: 4096] @ 0x1 0x2\n\n# runtime.MemStats\n# Alloc = 123456\n# TotalAlloc = 999999\n# HeapAlloc = 123456\n# HeapSys = 7340032\n", 123456, false},
		{"spaced", "#   HeapAlloc = 42 \n", 0, true},
		{"indented", "  # HeapAlloc = 42\n", 42, false},
		{"missing", "heap profile: 0: 0 [0: 0] @ heap/1048576\n# Alloc = 5\n", 0, true},
		{"not a number", "# HeapAlloc = many\n", 0, true},
		{"empty", "", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseHeapAlloc([]byte(c.in))
			if (err != nil) != c.wantErr || got != c.want {
				t.Errorf("ParseHeapAlloc(%q) = %d, %v; want %d, error %v", c.in, got, err, c.want, c.wantErr)
			}
		})
	}
}

// TestParseVMHWM covers the peak RSS R1 reads from /proc/<pid>/status
// without a scope: VmHWM in kB, returned in bytes, or an error for a
// missing or malformed line.
func TestParseVMHWM(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{"status", "Name:\truralzd\nVmPeak:\t 1300000 kB\nVmHWM:\t   52340 kB\nVmRSS:\t   51000 kB\n", 52340 << 10, false},
		{"missing", "Name:\truralzd\nVmRSS:\t   51000 kB\n", 0, true},
		{"no unit", "VmHWM:\t52340\n", 0, true},
		{"other unit", "VmHWM:\t52340 MB\n", 0, true},
		{"not a number", "VmHWM:\tlots kB\n", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseVMHWM([]byte(c.in))
			if (err != nil) != c.wantErr || got != c.want {
				t.Errorf("ParseVMHWM(%q) = %d, %v; want %d, error %v", c.in, got, err, c.want, c.wantErr)
			}
		})
	}
}
