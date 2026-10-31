// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package node

// host is the machine ruralz node drain inspects and signals: procfs and
// the process table. The Linux host reads /proc and signals through
// os.Process, which holds a pidfd on Linux 5.4 or newer and the PID on an
// older kernel; tests substitute a fake, so the drain's rules run on every
// platform the CLI's tests run on.
type host interface {
	// pidNamespace returns this process's PID namespace,
	// readlink(/proc/self/ns/pid), such as "pid:[4026531836]".
	pidNamespace() (string, error)
	// procStat returns the /proc/<pid>/stat line of pid, or an error that
	// matches fs.ErrNotExist when no process has that PID.
	procStat(pid int) ([]byte, error)
	// locks returns the content of /proc/locks.
	locks() ([]byte, error)
	// inode returns the inode number of the file at path, following a
	// final symbolic link as the holder's open of it did.
	inode(path string) (uint64, error)
	// open returns a handle on the process that has pid now: a pidfd
	// where the kernel has them, so the handle never reaches a process
	// that takes the PID later, else the PID.
	open(pid int) (process, error)
}

// process is a handle on one process.
type process interface {
	// terminate sends SIGTERM. It returns os.ErrProcessDone when the
	// process the handle refers to has exited, and an error matching
	// os.ErrPermission when the signal is denied.
	terminate() error
	// release frees the handle.
	release() error
}
