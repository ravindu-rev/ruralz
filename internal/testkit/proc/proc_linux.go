// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"os"
	"syscall"
)

// sysProcAttr puts the child in its own process group (so Kill reaches
// its children and a terminal's signals do not) and kills it if the test
// process dies first.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// killGroup sends SIGKILL to the process group led by pid.
func killGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// Sample reads the process's CPU time, RSS and thread count from /proc.
func (p *Process) Sample() (Usage, error) { return SampleProc("/proc", p.PID()) }

// Signaled returns the signal that ended the process, once Done is closed.
func (p *Process) Signaled() (os.Signal, bool) {
	st := p.State()
	if st == nil {
		return nil, false
	}
	ws, ok := st.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return nil, false
	}
	return ws.Signal(), true
}
