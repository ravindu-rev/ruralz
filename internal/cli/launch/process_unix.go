// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package launch

import "syscall"

// processGroup puts the child in a process group of its own, so the
// terminal's signals reach only the CLI.
func processGroup() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
