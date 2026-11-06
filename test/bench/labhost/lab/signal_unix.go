// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package lab

import "syscall"

// signals are the signals the run service delivers, by name: the
// terminating ones, and SIGSTOP and SIGCONT, which freeze a State Store
// server with its sockets open and resume it.
func signals() map[string]syscall.Signal {
	return map[string]syscall.Signal{
		"SIGHUP": syscall.SIGHUP, "SIGINT": syscall.SIGINT, "SIGQUIT": syscall.SIGQUIT, "SIGKILL": syscall.SIGKILL,
		"SIGTERM": syscall.SIGTERM, "SIGSTOP": syscall.SIGSTOP, "SIGCONT": syscall.SIGCONT,
	}
}
