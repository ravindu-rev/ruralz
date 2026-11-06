// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"slices"
	"testing"
)

// TestParseCPUList parses cpuset lists in their kernel and systemd form
// and refuses empty, reversed and malformed ones.
func TestParseCPUList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []int
	}{
		{"0-3", []int{0, 1, 2, 3}},
		{"2,4-5", []int{2, 4, 5}},
		{"7", []int{7}},
		{"1,3\n", []int{1, 3}}, // as cpuset.cpus reads, its newline included
	} {
		if got, ok := ParseCPUList(tc.in); !ok || !slices.Equal(got, tc.want) {
			t.Errorf("ParseCPUList(%q) = %v, %v; want %v", tc.in, got, ok, tc.want)
		}
	}
	for _, in := range []string{"", "3-1", "x", "-1", "1-x", ","} {
		if got, ok := ParseCPUList(in); ok {
			t.Errorf("ParseCPUList(%q) = %v, accepted", in, got)
		}
	}
}
