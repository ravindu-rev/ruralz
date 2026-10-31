// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"strconv"
	"strings"
)

// ParseCPUList parses a cpuset list such as "0-3,8,10-11", the form of a
// cgroup v2 cpuset's cpuset.cpus, systemd's AllowedCPUs and
// /proc/<pid>/status Cpus_allowed_list, into its CPUs in list order; ok
// is false for an empty or malformed list.
func ParseCPUList(s string) ([]int, bool) {
	var out []int
	for part := range strings.SplitSeq(strings.TrimSpace(s), ",") {
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 {
			return nil, false
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return nil, false
			}
		}
		for c := a; c <= b; c++ {
			out = append(out, c)
		}
	}
	return out, len(out) > 0
}
