// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package proc

import (
	"os"
	"syscall"
)

func sysProcAttr() *syscall.SysProcAttr { return nil }

func killGroup(int) {}

// Signaled is supported on Linux only.
func (p *Process) Signaled() (os.Signal, bool) { return nil, false }
