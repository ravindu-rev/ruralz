// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package topology

import (
	"os"
	"syscall"
)

// isStop reports SIGSTOP, which freezes a Node and its clock.
func isStop(sig os.Signal) bool { return sig == syscall.SIGSTOP }

// isCont reports SIGCONT, which resumes a frozen Node.
func isCont(sig os.Signal) bool { return sig == syscall.SIGCONT }
