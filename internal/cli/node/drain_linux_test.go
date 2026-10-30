// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package node

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/ulid"
)

// Tests of ruralz node drain against real processes: a helper process, the
// test binary run again, takes the data-directory lock and writes
// holder.json as ruralzd does (internal/nodedir), and the drain reads /proc
// and signals it through its pidfd. The drain's poll and --timeout run on
// the fake clock; the helper runs in real time, awaited under deadlines
// with condition polls.

// Helper protocol: the test binary runs TestDrainHelperProcess with
// helperModeEnv naming the mode and helperRootEnv the data directory. The
// helper prints "ready" once set up and "sigterm" on each SIGTERM.
const (
	helperModeEnv = "RURALZ_TEST_NODE_HELPER"
	helperRootEnv = "RURALZ_TEST_NODE_ROOT"
)

// Helper modes.
const (
	// modeHolder takes the lock and writes holder.json; on SIGTERM it
	// drains: releases the lock and exits 0.
	modeHolder = "holder"
	// modeStubborn takes the lock; it counts SIGTERMs and exits only
	// once its standard input closes.
	modeStubborn = "stubborn"
	// modeIdle takes no lock; SIGTERM makes it exit 3.
	modeIdle = "idle"
)

// TestDrainHelperProcess is the helper body; it does nothing in a normal
// run.
func TestDrainHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		t.Skip("helper process only")
	}
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM)
	stdinClosed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(stdinClosed)
	}()
	var lk *nodedir.Lock
	if mode != modeIdle {
		d, err := nodedir.Open(os.Getenv(helperRootEnv))
		if err != nil {
			t.Fatal(err)
		}
		if lk, err = d.TryLock(); err != nil {
			t.Fatal(err)
		}
		h, err := nodedir.NewHolder(testHolder(t).NodeID, "helper")
		if err != nil {
			t.Fatal(err)
		}
		if err := lk.WriteHolder(h); err != nil {
			t.Fatal(err)
		}
	}
	release := func() {
		if lk != nil {
			if err := lk.Release(); err != nil {
				t.Fatal(err)
			}
		}
	}
	say(t, "ready")
	for {
		select {
		case <-sigs:
			say(t, "sigterm")
			switch mode {
			case modeHolder:
				release()
				return
			case modeIdle:
				os.Exit(3)
			}
		case <-stdinClosed:
			release()
			return
		}
	}
}

// say writes one protocol line to standard output.
func say(t *testing.T, line string) {
	t.Helper()
	if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
		t.Fatal(err)
	}
}

// helper is a running helper process.
type helper struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan string
	stderr *strings.Builder
	exited chan error
}

// pid returns the helper's process ID.
func (h *helper) pid() int { return h.cmd.Process.Pid }

// startHelper starts a helper in mode on the data directory root and waits
// until it is ready.
func startHelper(t *testing.T, mode, root string) *helper {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDrainHelperProcess$", "-test.count=1") //nolint:gosec // G204: the test binary itself.
	cmd.Env = append(os.Environ(), helperModeEnv+"="+mode, helperRootEnv+"="+root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	h := &helper{cmd: cmd, stdin: stdin, lines: make(chan string, 64), stderr: &strings.Builder{}, exited: make(chan error, 1)}
	cmd.Stderr = h.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reading := make(chan struct{})
	go func() {
		defer close(reading)
		defer close(h.lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			h.lines <- sc.Text()
		}
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		<-reading
		_ = h.wait(t)
	})
	h.expect(t, "ready")
	return h
}

// expect waits for line among the helper's output.
func (h *helper) expect(t *testing.T, line string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case got, ok := <-h.lines:
			if !ok {
				t.Fatalf("the helper ended before %q; stderr: %s", line, h.stderr)
			}
			if got == line {
				return
			}
		case <-deadline:
			t.Fatalf("the helper did not print %q", line)
		}
	}
}

// sigterms closes the helper's standard input, which ends it, and counts
// the SIGTERMs it reports in the rest of its output.
func (h *helper) sigterms(t *testing.T) int {
	t.Helper()
	_ = h.stdin.Close()
	n := 0
	deadline := time.After(30 * time.Second)
	for {
		select {
		case got, ok := <-h.lines:
			if !ok {
				return n
			}
			if got == "sigterm" {
				n++
			}
		case <-deadline:
			t.Fatal("the helper did not end")
		}
	}
}

// wait reaps the helper once and returns its exit error.
func (h *helper) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.exited:
		h.exited <- err
		return err
	default:
	}
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case err := <-done:
		h.exited <- err
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("the helper did not exit")
		return nil
	}
}

// requireProcfs skips a test where /proc does not offer what the drain
// reads.
func requireProcfs(t *testing.T) {
	t.Helper()
	for _, p := range []string{"/proc/self/ns/pid", "/proc/locks", "/proc/self/stat"} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("no %s: %v", p, err)
		}
	}
}

// realStat reads the stat line of pid.
func realStat(t *testing.T, pid int) procStat {
	t.Helper()
	line, err := procHost{}.procStat(pid)
	if err != nil {
		t.Fatal(err)
	}
	st, err := parseProcStat(line)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// ownNamespace returns this process's PID namespace.
func ownNamespace(t *testing.T) string {
	t.Helper()
	ns, err := procHost{}.pidNamespace()
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

// TestDrainRealHolder: the drain verifies a live lock holder through
// /proc, signals it once, and reports it drained at the first poll after
// it is gone.
func TestDrainRealHolder(t *testing.T) {
	requireProcfs(t)
	root := filepath.Join(t.TempDir(), "data")
	h := startHelper(t, modeHolder, root)
	clk := clocktest.New(epoch())
	run := start(t.Context(), Options{Clock: clk}, "node", "drain", "--data-dir", root, "--output", "json")
	h.expect(t, "sigterm")
	if err := h.wait(t); err != nil {
		t.Fatalf("the helper did not drain cleanly: %v; stderr: %s", err, h.stderr)
	}
	tick(t, clk, DrainPoll)
	r := run.wait(t)
	want := fmt.Sprintf("{\n  \"nodeId\": %q,\n  \"pid\": %d,\n  \"seconds\": 0.100,\n  \"result\": \"drained\"\n}\n", testNodeID, h.pid())
	if r.code != command.ExitOK || r.stdout != want || r.stderr != "" {
		t.Fatalf("drain = exit %d, stdout %q, stderr %q; want 0, %q, nothing", r.code, r.stdout, r.stderr, want)
	}
	if _, err := nodedir.ReadHolderAt(root); !errors.Is(err, nodedir.ErrNoHolder) {
		t.Errorf("holder.json after the Drain: %v", err)
	}
}

// TestDrainRealZombie: a holder that exited but that its parent has not
// reaped yet is gone.
func TestDrainRealZombie(t *testing.T) {
	requireProcfs(t)
	root := filepath.Join(t.TempDir(), "data")
	h := startHelper(t, modeHolder, root)
	clk := clocktest.New(epoch())
	run := start(t.Context(), Options{Clock: clk}, "node", "drain", "--data-dir", root)
	h.expect(t, "sigterm")
	waitFor(t, "the helper to become a zombie", func() bool {
		line, err := procHost{}.procStat(h.pid())
		if err != nil {
			return false
		}
		st, err := parseProcStat(line)
		return err == nil && st.state == 'Z'
	})
	tick(t, clk, DrainPoll)
	r := run.wait(t)
	want := fmt.Sprintf("drained Node %s (pid %d) in 0.1 s\n", testNodeID, h.pid())
	if r.code != command.ExitOK || r.stdout != "" || r.stderr != want {
		t.Fatalf("drain = exit %d, stdout %q, stderr %q; want 0, nothing, %q", r.code, r.stdout, r.stderr, want)
	}
}

// TestDrainRealTimeoutSendsOneSignal: when --timeout passes first, the
// drain exits 2 naming the PID and the holder received exactly one
// SIGTERM.
func TestDrainRealTimeoutSendsOneSignal(t *testing.T) {
	requireProcfs(t)
	root := filepath.Join(t.TempDir(), "data")
	h := startHelper(t, modeStubborn, root)
	clk := clocktest.New(epoch())
	run := start(t.Context(), Options{Clock: clk}, "node", "drain", "--data-dir", root, "--timeout", "300ms")
	h.expect(t, "sigterm")
	for range 3 {
		tick(t, clk, DrainPoll)
	}
	r := run.wait(t)
	want := fmt.Sprintf("ruralz node drain: timed out after 300ms (--timeout): Node %s (pid %d) still runs; "+
		"no second signal was sent, so its Drain goes on to its own deadline\n", testNodeID, h.pid())
	if r.code != command.ExitNoResult || r.stdout != "" || r.stderr != want {
		t.Fatalf("drain = exit %d, stdout %q, stderr %q; want 2, nothing, %q", r.code, r.stdout, r.stderr, want)
	}
	if n := h.sigterms(t); n != 0 {
		t.Errorf("the holder received %d more SIGTERMs after the first", n)
	}
}

// TestDrainRealStaleRecords: a record whose process is gone, whose PID
// another process has, that names another PID namespace, or whose process
// holds no lock exits 2 and signals nothing.
func TestDrainRealStaleRecords(t *testing.T) {
	requireProcfs(t)
	t.Run("killed holder", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "data")
		h := startHelper(t, modeHolder, root)
		if err := h.cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		_ = h.wait(t)
		r := execute(t.Context(), Options{Clock: clocktest.New(epoch())}, "linux", nil, "node", "drain", "--data-dir", root)
		want := fmt.Sprintf("ruralz node drain: stale holder record: no process has pid %d\n", h.pid())
		if r.code != command.ExitNoResult || r.stderr != want {
			t.Fatalf("drain = exit %d, stderr %q; want 2, %q", r.code, r.stderr, want)
		}
	})
	idle := func(t *testing.T, mutate func(h *nodedir.Holder)) (*helper, string) {
		t.Helper()
		root := t.TempDir()
		p := startHelper(t, modeIdle, root)
		id, err := ulid.Parse(testNodeID)
		if err != nil {
			t.Fatal(err)
		}
		rec := nodedir.Holder{
			Format: nodedir.HolderFormat, PID: p.pid(), StartTime: realStat(t, p.pid()).startTime,
			NodeID: id, Version: "helper", PIDNamespace: ownNamespace(t),
		}
		mutate(&rec)
		writeHolder(t, root, rec)
		writeFile(t, lockPath(root), "")
		return p, root
	}
	for _, tc := range []struct {
		name   string
		mutate func(h *nodedir.Holder)
		want   func(h nodedir.Holder, root string) string
	}{
		{
			name:   "no lock held",
			mutate: func(*nodedir.Holder) {},
			want: func(h nodedir.Holder, root string) string {
				return fmt.Sprintf("stale holder record: pid %d holds no FLOCK WRITE lock on %s", h.PID, lockPath(root))
			},
		},
		{
			name:   "PID of another process",
			mutate: func(h *nodedir.Holder) { h.StartTime++ },
			want: func(h nodedir.Holder, _ string) string {
				return fmt.Sprintf("stale holder record: pid %d belongs to another process (start time %d, recorded %d)",
					h.PID, h.StartTime-1, h.StartTime)
			},
		},
		{
			name:   "another PID namespace",
			mutate: func(h *nodedir.Holder) { h.PIDNamespace = "pid:[1]" },
			want: func(h nodedir.Holder, _ string) string {
				return fmt.Sprintf("the lock holder (pid %d) runs in another PID namespace (pid:[1], not this process's %s)",
					h.PID, ownNamespace(t))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, root := idle(t, tc.mutate)
			rec, err := nodedir.ReadHolderAt(root)
			if err != nil {
				t.Fatal(err)
			}
			r := execute(t.Context(), Options{Clock: clocktest.New(epoch())}, "linux", nil, "node", "drain", "--data-dir", root)
			if want := "ruralz node drain: " + tc.want(rec, root) + "\n"; r.code != command.ExitNoResult || r.stderr != want {
				t.Fatalf("drain = exit %d, stderr %q; want 2, %q", r.code, r.stderr, want)
			}
			if n := p.sigterms(t); n != 0 {
				t.Errorf("the process got %d SIGTERMs", n)
			}
			if err := p.wait(t); err != nil {
				t.Errorf("the process ended with %v, want exit 0 without a signal", err)
			}
		})
	}
}

// TestProcHost checks the Linux host against this process: its PID
// namespace, its stat line, inodes, and its own flock in /proc/locks.
func TestProcHost(t *testing.T) {
	requireProcfs(t)
	var hs procHost
	ns, err := hs.pidNamespace()
	want, rerr := os.Readlink("/proc/self/ns/pid")
	if err != nil || rerr != nil || ns != want || !strings.HasPrefix(ns, "pid:[") {
		t.Errorf("pidNamespace() = %q, %v; want %q (%v)", ns, err, want, rerr)
	}
	self, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	wantStat, err := parseProcStat(self)
	if err != nil {
		t.Fatal(err)
	}
	if got := realStat(t, os.Getpid()); got.startTime != wantStat.startTime || got.startTime == 0 || got.exited() {
		t.Errorf("procStat(self) = %+v, want start time %d", got, wantStat.startTime)
	}
	h, err := nodedir.NewHolder(testHolder(t).NodeID, "test")
	if err != nil || h.StartTime != wantStat.startTime || h.PIDNamespace != ns {
		t.Errorf("nodedir.NewHolder = %+v, %v; want start time %d and namespace %q, as the drain reads them", h, err, wantStat.startTime, ns)
	}
	if _, err := hs.procStat(1<<31 - 1); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("procStat(no such PID) = %v, want fs.ErrNotExist", err)
	}
	d, err := nodedir.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	lk, err := d.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	ino, err := hs.inode(d.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(d.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); ino != st.Ino {
		t.Errorf("inode() = %d, want %d", ino, st.Ino)
	}
	locks, err := hs.locks()
	if err != nil {
		t.Fatal(err)
	}
	if !holdsFlock(locks, os.Getpid(), ino) {
		t.Errorf("/proc/locks shows no FLOCK WRITE lock of pid %d on inode %d:\n%s", os.Getpid(), ino, locks)
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
	if locks, err = hs.locks(); err != nil || holdsFlock(locks, os.Getpid(), ino) {
		t.Errorf("/proc/locks still shows the released lock (%v):\n%s", err, locks)
	}
	if _, err := hs.inode(filepath.Join(d.Root(), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("inode(missing) = %v, want fs.ErrNotExist", err)
	}
}

// pidfds counts this process's open pidfds.
func pidfds(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	n := 0
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && target == "anon_inode:[pidfd]" {
			n++
		}
	}
	return n
}

// TestProcHostOpensPidfd: the handle on the holder is a pidfd where Go
// gets pidfds from the kernel (Linux 5.4 or newer), released with it.
func TestProcHostOpensPidfd(t *testing.T) {
	requireProcfs(t)
	before := pidfds(t)
	h := startHelper(t, modeIdle, t.TempDir())
	withChild := pidfds(t)
	if withChild == before {
		t.Skip("Go holds no pidfd for its child here, so this kernel or sandbox gives it none; the drain keeps the PID")
	}
	p, err := procHost{}.open(h.pid())
	if err != nil {
		t.Fatal(err)
	}
	if n := pidfds(t); n != withChild+1 {
		t.Errorf("pidfds after open = %d, want %d", n, withChild+1)
	}
	if err := p.release(); err != nil {
		t.Fatal(err)
	}
	if n := pidfds(t); n != withChild {
		t.Errorf("pidfds after release = %d, want %d", n, withChild)
	}
}

// TestProcHostHandleOutlivesProcess: a handle opened on a process that
// then exits reports os.ErrProcessDone instead of signaling anything.
func TestProcHostHandleOutlivesProcess(t *testing.T) {
	requireProcfs(t)
	h := startHelper(t, modeIdle, t.TempDir())
	p, err := procHost{}.open(h.pid())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.release() }()
	if err := h.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = h.wait(t)
	if err := p.terminate(); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("terminate() after the exit = %v, want os.ErrProcessDone", err)
	}
}

// TestProcHostTerminate: terminate delivers SIGTERM.
func TestProcHostTerminate(t *testing.T) {
	requireProcfs(t)
	h := startHelper(t, modeIdle, t.TempDir())
	p, err := procHost{}.open(h.pid())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.release() }()
	if err := p.terminate(); err != nil {
		t.Fatal(err)
	}
	h.expect(t, "sigterm")
	var ee *exec.ExitError
	if err := h.wait(t); !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("the helper ended with %v, want exit 3 after SIGTERM", err)
	}
}
