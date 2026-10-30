// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package source

import (
	"os"
	"syscall"
	"testing"
)

// TestRealSIGHUPReloads sends SIGHUP to the test process, as an operator
// does to ruralzd: the process survives, since NotifySIGHUP registered it,
// and Reload runs.
func TestRealSIGHUPReloads(t *testing.T) {
	r := &countingReloader{}
	stop := NotifySIGHUP(r)
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "Reload after SIGHUP", func() bool { return r.n.Load() >= 1 })
}
