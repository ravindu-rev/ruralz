// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package harness

import (
	"fmt"
	"syscall"
)

// InheritOpenFiles makes the processes the harness starts on its own host
// inherit its open-file limit, and returns that limit. Go raises its own
// soft limit to the hard limit at startup but gives the processes it
// starts the original soft limit, often 1,024, unless the program sets the
// limit itself; setting the raised limit unchanged gives a Node holding
// C1's, O1's or O2's tens of thousands of connections the most the host
// allows (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Reference hardware": LimitNOFILE for every benchmark process).
func InheritOpenFiles() (uint64, error) {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0, fmt.Errorf("harness: read the open-file limit: %w", err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0, fmt.Errorf("harness: set the open-file limit: %w", err)
	}
	return r.Cur, nil
}
