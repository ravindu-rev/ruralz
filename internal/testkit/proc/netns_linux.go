// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// RunInNetNS runs o.TestName in a new network namespace; see NetNSOptions.
// When the kernel refuses to create the namespaces the error wraps
// ErrNamespace.
//
// The child gets SIGKILL (Pdeathsig) when the test process dies. The
// kernel ties Pdeathsig to the OS thread that forked the child, and the Go
// runtime ends a thread when a goroutine locked to it exits without
// unlocking, so RunInNetNS keeps the calling goroutine locked to its
// thread until the child has exited (see Start).
func RunInNetNS(ctx context.Context, o NetNSOptions) (int, error) {
	if os.Getenv(EnvInNetNS) == "1" {
		return -1, errors.New("proc: RunInNetNS called inside a RunInNetNS child")
	}
	if o.TestName == "" {
		return -1, errors.New("proc: RunInNetNS needs a test name")
	}
	exe, err := os.Executable()
	if err != nil {
		return -1, fmt.Errorf("proc: %w", err)
	}
	// Verbose output carries the result line that tells a pass from a
	// skip. -test.v=true comes before o.Args so it is parsed even when
	// o.Args holds "--" or a positional argument (flag parsing stops
	// there), and a -test.v entry in o.Args is dropped so it cannot turn
	// verbose output off again.
	args := []string{"-test.run=" + RunPattern(o.TestName), "-test.count=1", "-test.v=true"}
	for _, a := range o.Args {
		if !isVerboseFlag(a) {
			args = append(args, a)
		}
	}
	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // G204: re-executing this test binary
	// EnvNetNSUser is always set, empty without User, so the child never
	// takes it from the environment it inherits.
	user := ""
	if o.User {
		user = "1"
	}
	cmd.Env = append(os.Environ(), EnvInNetNS+"=1", EnvNetNSUser+"="+user)
	cmd.Env = append(cmd.Env, o.Env...)
	tail := newRing(64 << 10)
	res := newTestResult(o.TestName)
	// One writer for both streams: exec then calls Write from one
	// goroutine at a time.
	out := io.MultiWriter(tail, res)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET, Pdeathsig: syscall.SIGKILL}
	if o.User {
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}
	}
	cmd.WaitDelay = WaitDelay
	runtime.LockOSThread()
	if err := cmd.Start(); err != nil {
		runtime.UnlockOSThread()
		if namespaceErr(err) {
			return -1, fmt.Errorf("%w for %s: %w", ErrNamespace, o.TestName, err)
		}
		return -1, fmt.Errorf("proc: run %s in a new network namespace: %w", o.TestName, err)
	}
	err = cmd.Wait()
	runtime.UnlockOSThread()
	res.finish()
	return res.outcome(err, tail.bytes())
}

// namespaceErr reports whether a failed start is the kernel refusing the
// child's namespaces (clone(2)): EPERM without the privilege, EINVAL
// without the namespace kind, ENOSPC or EUSERS past the namespace limits.
func namespaceErr(err error) bool {
	for _, e := range []error{syscall.EPERM, syscall.EINVAL, syscall.ENOSPC, syscall.EUSERS} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// isVerboseFlag reports whether the command-line argument a sets -test.v
// (-test.v, --test.v, -test.v=false and so on).
func isVerboseFlag(a string) bool {
	name, ok := strings.CutPrefix(a, "-")
	if !ok {
		return false
	}
	name = strings.TrimPrefix(name, "-")
	name, _, _ = strings.Cut(name, "=")
	return name == "test.v"
}

// SetupNetNS brings the loopback interface up, setting IFF_UP on "lo"
// (SIOCSIFFLAGS). It is meant for a fresh namespace.
func SetupNetNS() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("proc: loopback: socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return fmt.Errorf("proc: loopback: %w", err)
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("proc: loopback: SIOCGIFFLAGS: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("proc: loopback: SIOCSIFFLAGS: %w", err)
	}
	return nil
}
