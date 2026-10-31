// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package proc

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func sysProcAttr() *syscall.SysProcAttr { return nil }

func killGroup(int) {}

// Sample needs /proc and is supported on Linux only.
func (p *Process) Sample() (Usage, error) {
	return Usage{}, fmt.Errorf("proc: sample: %w", errors.ErrUnsupported)
}

// Signaled is supported on Linux only.
func (p *Process) Signaled() (os.Signal, bool) { return nil, false }
