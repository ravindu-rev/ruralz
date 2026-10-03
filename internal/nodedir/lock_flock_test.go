// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package nodedir

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Helper process protocol: the test binary re-executes itself with
// helperEnv=holder and helperDirEnv=<root>; the helper takes the lock,
// writes holder.json, prints helperReady and holds the lock until its
// stdin closes, then releases and exits 0.
const (
	helperEnv    = "RURALZ_TEST_NODEDIR_HELPER"
	helperDirEnv = "RURALZ_TEST_NODEDIR_ROOT"
	helperReady  = "locked"
)

// TestHelperProcess is the helper body; it does nothing in a normal run.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "holder" {
		t.Skip("helper process only")
	}
	d, err := Open(os.Getenv(helperDirEnv))
	if err != nil {
		t.Fatal(err)
	}
	lk, err := d.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHolder(testULID(t), "helper")
	if err != nil {
		t.Logf("NewHolder: %v", err)
	}
	if err := lk.WriteHolder(h); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, helperReady); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
}

// holderProcess is a running helper.
type holderProcess struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
}

// startHolder starts a helper on root and waits until it holds the lock.
func startHolder(ctx context.Context, t *testing.T, root string) *holderProcess {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1") //nolint:gosec // G204: the test binary itself.
	cmd.Env = append(os.Environ(), helperEnv+"=holder", helperDirEnv+"="+root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &holderProcess{cmd: cmd, stdin: stdin}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	ready := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if sc.Text() == helperReady {
				ready <- nil
				_, _ = io.Copy(io.Discard, stdout)
				return
			}
		}
		ready <- fmt.Errorf("helper exited before locking: %w", sc.Err())
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("%v; stderr: %s", err, stderr.String())
		}
	case <-ctx.Done():
		t.Fatalf("helper did not lock: %v; stderr: %s", ctx.Err(), stderr.String())
	}
	return p
}

// TestLockContentionTwoProcesses is the work package's "lock contention
// test with two processes" (spec 04 requirement 7): while another process
// holds the lock, TryLock fails with ErrLocked and holder.json names that
// process; after it releases, the lock is free and holder.json is gone.
func TestLockContentionTwoProcesses(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	d, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	p := startHolder(ctx, t, d.Root())

	if lk, err := d.TryLock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("TryLock while held = %v, %v; want ErrLocked", lk, err)
	}
	h, err := ReadHolderAt(d.Root())
	if err != nil {
		t.Fatal(err)
	}
	if h.PID != p.cmd.Process.Pid || h.Version != "helper" || h.NodeID != testULID(t) {
		t.Fatalf("holder = %+v, want the helper (pid %d)", h, p.cmd.Process.Pid)
	}
	verifyHolderProcess(t, d, h)

	if err := p.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	if _, err := ReadHolderAt(d.Root()); !errors.Is(err, ErrNoHolder) {
		t.Errorf("holder.json after the helper released: %v", err)
	}
	lk, err := d.TryLock()
	if err != nil {
		t.Fatalf("TryLock after the holder released: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestLockReleasedWhenHolderDies: a killed holder drops the lock with its
// descriptors, leaving a stale holder.json that ruralz node drain detects
// by its start time (spec 10 requirement 96.4).
func TestLockReleasedWhenHolderDies(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	d, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	p := startHolder(ctx, t, d.Root())
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = p.cmd.Wait()
	lk, err := d.TryLock()
	if err != nil {
		t.Fatalf("TryLock after the holder died: %v", err)
	}
	defer func() { _ = lk.Release() }()
	h, err := d.ReadHolder()
	if err != nil || h.PID != p.cmd.Process.Pid {
		t.Fatalf("stale holder = %+v, %v; want the dead helper's record", h, err)
	}
}
