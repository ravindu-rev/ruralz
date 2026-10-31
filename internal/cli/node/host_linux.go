// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package node

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"syscall"
)

// newHost returns the Linux host: procfs and os.Process.
func newHost() host { return procHost{} }

// procHost reads /proc and signals through os.Process. os.FindProcess opens
// a pidfd (pidfd_open) where the kernel offers every pidfd call, Linux 5.4
// or newer, and Process.Signal then uses pidfd_send_signal; on an older
// kernel, or where a sandbox refuses those calls, it keeps the PID and
// signals with kill (docs/reference/01-cli-and-api-surface.md, "Rollout,
// promotion and Node verbs").
type procHost struct{}

func (procHost) pidNamespace() (string, error) {
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return "", fmt.Errorf("read this process's PID namespace: %w", err)
	}
	return ns, nil
}

// procStat reads /proc/<pid>/stat. A process that goes away between the
// open and the read fails the read with ESRCH, which counts as no process.
func (procHost) procStat(pid int) ([]byte, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, syscall.ESRCH) {
		return nil, fmt.Errorf("process %d: %w: %w", pid, fs.ErrNotExist, err)
	}
	return b, err
}

func (procHost) locks() ([]byte, error) {
	b, err := os.ReadFile("/proc/locks")
	if err != nil {
		return nil, fmt.Errorf("read /proc/locks: %w", err)
	}
	return b, nil
}

func (procHost) inode(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s: no inode number", path)
	}
	return st.Ino, nil
}

func (procHost) open(pid int) (process, error) {
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil, fmt.Errorf("open process %d: %w", pid, err)
	}
	return osProcess{p: p}, nil
}

// osProcess signals through an os.Process: its pidfd when it holds one.
type osProcess struct{ p *os.Process }

func (o osProcess) terminate() error { return o.p.Signal(syscall.SIGTERM) }

func (o osProcess) release() error { return o.p.Release() }
