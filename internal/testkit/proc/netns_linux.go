// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// RunInNetNS runs o.TestName in a new network namespace; see InNetNS.
//
// The child gets SIGKILL (Pdeathsig) when the test process dies. The
// kernel ties Pdeathsig to the OS thread that forked the child, and the Go
// runtime ends a thread when a goroutine locked to it exits without
// unlocking, so RunInNetNS keeps the calling goroutine locked to its
// thread until the child has exited (see Start).
func RunInNetNS(ctx context.Context, o NetNSOptions) (int, error) {
	if os.Getenv(EnvInNetNS) == "1" {
		return -1, errors.New("proc: InNetNS called inside an InNetNS child")
	}
	if o.TestName == "" {
		return -1, errors.New("proc: InNetNS needs a test name")
	}
	enc, err := encodeSysctls(o.Sysctls)
	if err != nil {
		return -1, err
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
	cmd.Env = append(os.Environ(), EnvInNetNS+"=1", EnvNetNSSysctls+"="+enc)
	cmd.Env = append(cmd.Env, o.Env...)
	tail := newRing(64 << 10)
	res := newTestResult(o.TestName)
	writers := []io.Writer{tail, res}
	if o.Output != nil {
		writers = append(writers, o.Output)
	}
	// One writer for both streams: exec then calls Write from one
	// goroutine at a time.
	out := io.MultiWriter(writers...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNET, Pdeathsig: syscall.SIGKILL}
	cmd.WaitDelay = WaitDelay
	runtime.LockOSThread()
	err = cmd.Run()
	runtime.UnlockOSThread()
	res.finish()
	return res.outcome(err, tail.bytes())
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

// SetupNetNS brings the loopback interface up (SIOCSIFFLAGS) and writes
// the sysctls, in name order. It is meant for a fresh namespace.
func SetupNetNS(sysctls map[string]string) error {
	if err := loopbackUp(); err != nil {
		return err
	}
	names := make([]string, 0, len(sysctls))
	for n := range sysctls {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if err := WriteSysctl(n, sysctls[n]); err != nil {
			return err
		}
	}
	return nil
}

// WriteSysctl writes one sysctl, for example
// WriteSysctl("net.ipv4.tcp_migrate_req", "1"). Network sysctls apply to
// the caller's network namespace.
func WriteSysctl(name, value string) error {
	path, err := sysctlPath(name)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // G304: a validated /proc/sys path
	if err != nil {
		return fmt.Errorf("proc: sysctl %s: %w", name, err)
	}
	if _, err := f.WriteString(value); err != nil {
		_ = f.Close()
		return fmt.Errorf("proc: sysctl %s=%q: %w", name, value, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("proc: sysctl %s: %w", name, err)
	}
	return nil
}

// ReadSysctl reads one sysctl without its trailing newline.
func ReadSysctl(name string) (string, error) {
	path, err := sysctlPath(name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: a validated /proc/sys path
	if err != nil {
		return "", fmt.Errorf("proc: sysctl %s: %w", name, err)
	}
	return string(bytes.TrimRight(b, "\n")), nil
}

// loopbackUp sets IFF_UP on "lo".
func loopbackUp() error {
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
