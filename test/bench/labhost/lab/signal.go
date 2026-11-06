// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"os"
	"slices"
	"syscall"
)

// signalNamed returns the signal the run service delivers by name, such
// as "SIGSTOP".
func signalNamed(name string) (syscall.Signal, bool) {
	s, ok := signals()[name]
	return s, ok
}

// SignalName returns the name the run service knows sig by, such as
// "SIGSTOP" for syscall.SIGSTOP, for an OpSignal request.
func SignalName(sig os.Signal) (string, bool) {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return "", false
	}
	for name, v := range signals() {
		if v == s {
			return name, true
		}
	}
	return "", false
}

// terminating reports a signal that ends a process unless it handles it:
// one after which a supervised child's end is expected.
func terminating(name string) bool {
	return slices.Contains([]string{"SIGHUP", "SIGINT", "SIGQUIT", "SIGKILL", "SIGTERM"}, name)
}
