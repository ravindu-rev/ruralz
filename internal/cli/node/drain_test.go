// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// Tests of ruralz node drain over the fake host and clock, which run on
// every platform (docs/reference/01-cli-and-api-surface.md, "Rollout,
// promotion and Node verbs"); drain_linux_test.go runs it against real
// processes.

const drainedLine = "drained Node " + testNodeID + " (pid 4242) in 0.2 s\n"

// TestDrainSignalsVerifiedHolder: the command opens its handle on the
// recorded PID, checks the namespace, the process and the lock in that
// order, sends one SIGTERM through the handle, polls until the process is
// gone and prints the drained line on standard error only.
func TestDrainSignalsVerifiedHolder(t *testing.T) {
	s := newDrainSetup(t)
	run := s.drain(t.Context())
	if pid := awaitSignal(t, s.host); pid != testPID {
		t.Fatalf("SIGTERM went to pid %d, want %d", pid, testPID)
	}
	tick(t, s.clk, DrainPoll)
	armed(t, s.clk)
	s.host.set(func(h *fakeHost) { h.procs[testPID].reaped = true })
	s.clk.Advance(DrainPoll)
	r := run.wait(t)
	if r.code != command.ExitOK || r.stdout != "" || r.stderr != drainedLine {
		t.Fatalf("drain = exit %d, stdout %q, stderr %q; want 0, nothing, %q", r.code, r.stdout, r.stderr, drainedLine)
	}
	lock := lockPath(s.root)
	want := []string{
		"open 4242", "ns", "stat 4242", "inode " + lock, "locks", "terminate 4242",
		"stat 4242", "stat 4242", "release 4242",
	}
	if got := s.host.callLog(); !slices.Equal(got, want) {
		t.Errorf("host calls = %q, want %q", got, want)
	}
	if n := s.host.sigterms(testPID); n != 1 {
		t.Errorf("SIGTERMs = %d, want 1", n)
	}
}

// TestDrainJSON: with --output json the drain prints one object on
// standard output, seconds with millisecond precision, and nothing on
// standard error.
func TestDrainJSON(t *testing.T) {
	s := newDrainSetup(t)
	run := s.drain(t.Context(), "--output", "json")
	awaitSignal(t, s.host)
	tick(t, s.clk, DrainPoll)
	armed(t, s.clk)
	s.host.set(func(h *fakeHost) { h.procs[testPID].state = 'Z' })
	s.clk.Advance(DrainPoll)
	r := run.wait(t)
	want := "{\n  \"nodeId\": \"" + testNodeID + "\",\n  \"pid\": 4242,\n  \"seconds\": 0.200,\n  \"result\": \"drained\"\n}\n"
	if r.code != command.ExitOK || r.stdout != want || r.stderr != "" {
		t.Fatalf("drain --output json = exit %d, stdout %q, stderr %q; want 0, %q, nothing", r.code, r.stdout, r.stderr, want)
	}
}

// TestDrainPollsEvery100ms: the first check follows the signal by
// DrainPoll and the next ones come every DrainPoll; seconds count from the
// signal to the poll that saw the process gone.
func TestDrainPollsEvery100ms(t *testing.T) {
	s := newDrainSetup(t)
	run := s.drain(t.Context(), "--output", "json")
	awaitSignal(t, s.host)
	armed(t, s.clk)
	polls := func() int { return s.host.countCalls("stat") - 1 }
	s.clk.Advance(DrainPoll - time.Millisecond)
	if n := polls(); n != 0 {
		t.Fatalf("%d polls before %v", n, DrainPoll)
	}
	s.clk.Advance(time.Millisecond)
	armed(t, s.clk)
	if n := polls(); n != 1 {
		t.Fatalf("%d polls at %v, want 1", n, DrainPoll)
	}
	tick(t, s.clk, DrainPoll)
	armed(t, s.clk)
	if n := polls(); n != 2 {
		t.Fatalf("%d polls at %v, want 2", n, 2*DrainPoll)
	}
	// The process exits between two polls; the next poll sees it gone.
	s.clk.Advance(DrainPoll / 2)
	s.host.set(func(h *fakeHost) { h.procs[testPID].reaped = true })
	s.clk.Advance(DrainPoll / 2)
	r := run.wait(t)
	if r.code != command.ExitOK || !strings.Contains(r.stdout, `"seconds": 0.300,`) || polls() != 3 {
		t.Fatalf("drain = exit %d, stdout %q after %d polls; want 0.300 seconds after 3 polls", r.code, r.stdout, polls())
	}
}

// TestDrainTimeout: when --timeout passes first, the drain exits 2 naming
// the PID that still runs, sends no second signal and, with --output json,
// prints the object with result timeout.
func TestDrainTimeout(t *testing.T) {
	const msg = "ruralz node drain: timed out after 1s (--timeout): Node " + testNodeID + " (pid 4242) still runs; " +
		"no second signal was sent, so its Drain goes on to its own deadline\n"
	for _, tc := range []struct {
		name   string
		args   []string
		stdout string
	}{
		{"text", nil, ""},
		{"json", []string{"--output", "json"}, "{\n  \"nodeId\": \"" + testNodeID + "\",\n  \"pid\": 4242,\n  \"seconds\": 1.000,\n  \"result\": \"timeout\"\n}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDrainSetup(t)
			run := s.drain(t.Context(), append([]string{"--timeout", "1s"}, tc.args...)...)
			awaitSignal(t, s.host)
			for range 10 {
				tick(t, s.clk, DrainPoll)
			}
			r := run.wait(t)
			if r.code != command.ExitNoResult || r.stdout != tc.stdout || r.stderr != msg {
				t.Fatalf("drain = exit %d, stdout %q, stderr %q; want 2, %q, %q", r.code, r.stdout, r.stderr, tc.stdout, msg)
			}
			if n := s.host.countCalls("terminate"); n != 1 {
				t.Errorf("%d SIGTERMs sent, want 1", n)
			}
			if n := s.host.countCalls("stat") - 1; n != 10 {
				t.Errorf("%d polls in 1s, want 10", n)
			}
		})
	}
}

// TestDrainTimeoutBetweenPolls: a --timeout that is not a multiple of
// DrainPoll ends with a last poll at the deadline.
func TestDrainTimeoutBetweenPolls(t *testing.T) {
	s := newDrainSetup(t)
	run := s.drain(t.Context(), "--timeout", "250ms", "--output", "json")
	awaitSignal(t, s.host)
	tick(t, s.clk, DrainPoll)
	tick(t, s.clk, DrainPoll)
	tick(t, s.clk, DrainPoll/2)
	r := run.wait(t)
	if r.code != command.ExitNoResult || !strings.Contains(r.stdout, `"seconds": 0.250,`) || !strings.Contains(r.stdout, `"result": "timeout"`) {
		t.Fatalf("drain = exit %d, stdout %q; want 2 and a timeout at 0.250 s", r.code, r.stdout)
	}
	if n := s.host.countCalls("stat") - 1; n != 3 {
		t.Errorf("%d polls, want 3", n)
	}
}

// TestDrainGoneWhenPIDReused: a PID that another process takes while the
// drain waits means the signaled process is gone.
func TestDrainGoneWhenPIDReused(t *testing.T) {
	s := newDrainSetup(t)
	run := s.drain(t.Context())
	awaitSignal(t, s.host)
	armed(t, s.clk)
	s.host.set(func(h *fakeHost) { h.procs[testPID] = &fakeProc{pid: testPID, start: testStart + 50, state: 'S'} })
	s.clk.Advance(DrainPoll)
	r := run.wait(t)
	if want := "drained Node " + testNodeID + " (pid 4242) in 0.1 s\n"; r.code != command.ExitOK || r.stderr != want {
		t.Fatalf("drain = exit %d, stderr %q; want 0, %q", r.code, r.stderr, want)
	}
	if n := s.host.sigterms(testPID); n != 0 {
		t.Errorf("the process that took the PID got %d SIGTERMs", n)
	}
}

// TestDrainRefusals: no holder, an unreadable or stale record, another PID
// namespace, a failed check and a denied signal exit 2 with one line on
// standard error, and print no object even with --output json.
func TestDrainRefusals(t *testing.T) {
	holder := func(t *testing.T, s *drainSetup, mutate func(*nodedir.Holder)) {
		t.Helper()
		h := testHolder(t)
		mutate(&h)
		writeHolder(t, s.root, h)
	}
	raw := func(t *testing.T, s *drainSetup, content string) {
		t.Helper()
		writeFile(t, filepath.Join(s.root, nodedir.HolderFile), content)
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, s *drainSetup)
		// stderr is the line after "ruralz node drain: "; <root> and
		// <lock> stand for the data directory and its lock file.
		stderr string
		// prefix matches only the start of the line.
		prefix bool
		// attempted reports a SIGTERM attempt that delivered nothing.
		attempted bool
	}{
		{
			name:   "no holder record",
			setup:  func(t *testing.T, s *drainSetup) { removeFile(t, filepath.Join(s.root, nodedir.HolderFile)) },
			stderr: "no lock holder recorded in <root>",
		},
		{
			name:   "unparsable record",
			setup:  func(t *testing.T, s *drainSetup) { raw(t, s, "not json\n") },
			stderr: "unreadable holder record in <root>: ", prefix: true,
		},
		{
			name: "another format",
			setup: func(t *testing.T, s *drainSetup) {
				raw(t, s, `{"format":"ruralz.holder.v2","pid":4242,"startTime":1234567,"nodeId":"`+testNodeID+`","version":"0.4.0"}`)
			},
			stderr: `unreadable holder record in <root>: format "ruralz.holder.v2", want "ruralz.holder.v1"`,
		},
		{
			name: "no PID",
			setup: func(t *testing.T, s *drainSetup) {
				raw(t, s, `{"format":"ruralz.holder.v1","nodeId":"`+testNodeID+`"}`)
			},
			stderr: "unreadable holder record in <root>: pid 0",
		},
		{
			name:   "no node.id",
			setup:  func(t *testing.T, s *drainSetup) { raw(t, s, `{"format":"ruralz.holder.v1","pid":4242}`) },
			stderr: "unreadable holder record in <root>: no nodeId",
		},
		{
			name: "holder record not a regular file",
			setup: func(t *testing.T, s *drainSetup) {
				removeFile(t, filepath.Join(s.root, nodedir.HolderFile))
				if err := os.Mkdir(filepath.Join(s.root, nodedir.HolderFile), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			stderr: "unreadable holder record in <root>: " + filepath.Join("<root>", nodedir.HolderFile) + " is not a regular file",
			prefix: true,
		},
		{
			name:   "another PID namespace",
			setup:  func(_ *testing.T, s *drainSetup) { s.host.ns = "pid:[4026532999]" },
			stderr: "the lock holder (pid 4242) runs in another PID namespace (pid:[4026531836], not this process's pid:[4026532999])",
		},
		{
			name:   "record without a PID namespace",
			setup:  func(t *testing.T, s *drainSetup) { holder(t, s, func(h *nodedir.Holder) { h.PIDNamespace = "" }) },
			stderr: "cannot verify the lock holder (pid 4242): its record names no PID namespace",
		},
		{
			name: "own PID namespace unreadable",
			setup: func(_ *testing.T, s *drainSetup) {
				s.host.nsErr = errors.New("read this process's PID namespace: permission denied")
			},
			stderr: "cannot verify the lock holder (pid 4242): read this process's PID namespace: permission denied",
		},
		{
			name:   "no process",
			setup:  func(_ *testing.T, s *drainSetup) { delete(s.host.procs, testPID) },
			stderr: "stale holder record: no process has pid 4242",
		},
		{
			name:   "reaped process",
			setup:  func(_ *testing.T, s *drainSetup) { s.host.procs[testPID].reaped = true },
			stderr: "stale holder record: no process has pid 4242",
		},
		{
			name:   "reused PID",
			setup:  func(_ *testing.T, s *drainSetup) { s.host.procs[testPID].start = 999 },
			stderr: "stale holder record: pid 4242 belongs to another process (start time 999, recorded 1234567)",
		},
		{
			name:   "exited process",
			setup:  func(_ *testing.T, s *drainSetup) { s.host.procs[testPID].state = 'Z' },
			stderr: "stale holder record: pid 4242 has exited",
		},
		{
			name: "stat unreadable",
			setup: func(_ *testing.T, s *drainSetup) {
				s.host.statErr[testPID] = errors.New("read /proc/4242/stat: input/output error")
			},
			stderr: "cannot verify the lock holder (pid 4242): read /proc/4242/stat: input/output error",
		},
		{
			name:   "stat malformed",
			setup:  func(_ *testing.T, s *drainSetup) { s.host.statText[testPID] = "4242 ruralzd S 1\n" },
			stderr: "cannot verify the lock holder (pid 4242): /proc/4242/stat: malformed /proc stat line: no command name",
		},
		{
			name:   "no lock file",
			setup:  func(_ *testing.T, s *drainSetup) { delete(s.host.inodes, lockPath(s.root)) },
			stderr: "stale holder record: <lock> does not exist",
		},
		{
			name: "lock file unreadable",
			setup: func(_ *testing.T, s *drainSetup) {
				s.host.inodeErr = errors.New("stat lock: permission denied")
			},
			stderr: "cannot verify the lock holder (pid 4242): stat lock: permission denied",
		},
		{
			name: "/proc/locks unreadable",
			setup: func(_ *testing.T, s *drainSetup) {
				s.host.locksErr = errors.New("read /proc/locks: permission denied")
			},
			stderr: "cannot verify the lock holder (pid 4242): read /proc/locks: permission denied",
		},
		{
			name:   "no lock held",
			setup:  func(_ *testing.T, s *drainSetup) { s.host.locksText = "" },
			stderr: "stale holder record: pid 4242 holds no FLOCK WRITE lock on <lock>",
		},
		{
			name: "waiting for the lock",
			setup: func(_ *testing.T, s *drainSetup) {
				s.host.locksText = flockLine(1, 99, testInode) + "1: -> FLOCK  ADVISORY  WRITE 4242 00:2e:1001 0 EOF\n"
			},
			stderr: "stale holder record: pid 4242 holds no FLOCK WRITE lock on <lock>",
		},
		{
			name:   "lock on another file",
			setup:  func(_ *testing.T, s *drainSetup) { s.host.locksText = flockLine(1, testPID, testInode+1) },
			stderr: "stale holder record: pid 4242 holds no FLOCK WRITE lock on <lock>",
		},
		{
			name: "handle refused",
			setup: func(_ *testing.T, s *drainSetup) {
				s.host.openErr = errors.New("open process 4242: too many open files")
			},
			stderr: "open process 4242: too many open files",
		},
		{
			name:   "signal denied",
			setup:  func(_ *testing.T, s *drainSetup) { s.host.procs[testPID].deny = true },
			stderr: "cannot send SIGTERM to pid 4242: operation not permitted", attempted: true,
		},
		{
			name: "exited before the signal",
			setup: func(_ *testing.T, s *drainSetup) {
				s.host.afterLocks = func(h *fakeHost) { h.procs[testPID].reaped = true }
			},
			stderr: "stale holder record: pid 4242 exited before the signal", attempted: true,
		},
	}
	for _, tc := range cases {
		for _, mode := range []string{"text", "json"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				s := newDrainSetup(t)
				tc.setup(t, s)
				r := execute(t.Context(), s.opts, "linux", nil, "node", "drain", "--data-dir", s.root, "--output", mode)
				want := "ruralz node drain: " + strings.NewReplacer("<lock>", lockPath(s.root), "<root>", s.root).Replace(tc.stderr)
				ok := r.stderr == want+"\n"
				if tc.prefix {
					ok = strings.HasPrefix(r.stderr, want) && strings.Count(r.stderr, "\n") == 1
				}
				if r.code != command.ExitNoResult || r.stdout != "" || !ok {
					t.Fatalf("drain = exit %d, stdout %q, stderr %q; want 2, nothing, %q", r.code, r.stdout, r.stderr, want)
				}
				if n := s.host.sigterms(testPID); n != 0 {
					t.Errorf("%d SIGTERMs delivered", n)
				}
				if n := s.host.countCalls("terminate"); (n == 1) != tc.attempted {
					t.Errorf("%d SIGTERM attempts, want attempted %v", n, tc.attempted)
				}
				if s.clk.Pending() != 0 {
					t.Error("a poll timer outlived the command")
				}
			})
		}
	}
}

// TestDrainIgnoresUnknownHolderMembers: a holder.json reader ignores the
// members it does not know, so a newer holder's record still verifies.
func TestDrainIgnoresUnknownHolderMembers(t *testing.T) {
	s := newDrainSetup(t)
	writeFile(t, filepath.Join(s.root, nodedir.HolderFile), `{"format":"ruralz.holder.v1","pid":4242,"startTime":1234567,`+
		`"nodeId":"`+testNodeID+`","version":"9.0.0","pidNamespace":"pid:[4026531836]","handover":{"phase":"x"},"zone":"eu"}`)
	s.host.locksText = ""
	r := execute(t.Context(), s.opts, "linux", nil, "node", "drain", "--data-dir", s.root)
	want := "ruralz node drain: stale holder record: pid 4242 holds no FLOCK WRITE lock on " + lockPath(s.root) + "\n"
	if r.code != command.ExitNoResult || r.stderr != want {
		t.Fatalf("drain = exit %d, stderr %q; want 2, %q (the record read past its unknown members)", r.code, r.stderr, want)
	}
}

// TestDrainCheckOrder: the checks run in their documented order after the
// handle is opened, and the first that fails ends the command.
func TestDrainCheckOrder(t *testing.T) {
	cases := []struct {
		name  string
		setup func(s *drainSetup)
		calls []string
	}{
		{
			name: "namespace before process and lock",
			setup: func(s *drainSetup) {
				s.host.ns = "pid:[1]"
				delete(s.host.procs, testPID)
				s.host.locksText = ""
			},
			calls: []string{"open 4242", "ns", "release 4242"},
		},
		{
			name: "process before lock",
			setup: func(s *drainSetup) {
				s.host.procs[testPID].start = 1
				s.host.locksText = ""
			},
			calls: []string{"open 4242", "ns", "stat 4242", "release 4242"},
		},
		{
			name:  "lock last",
			setup: func(s *drainSetup) { s.host.locksText = "" },
			calls: []string{"open 4242", "ns", "stat 4242", "inode <lock>", "locks", "release 4242"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newDrainSetup(t)
			tc.setup(s)
			r := execute(t.Context(), s.opts, "linux", nil, "node", "drain", "--data-dir", s.root)
			if r.code != command.ExitNoResult {
				t.Fatalf("drain = exit %d, stderr %q", r.code, r.stderr)
			}
			want := slices.Clone(tc.calls)
			for i, c := range want {
				want[i] = strings.ReplaceAll(c, "<lock>", lockPath(s.root))
			}
			if got := s.host.callLog(); !slices.Equal(got, want) {
				t.Errorf("host calls = %q, want %q", got, want)
			}
		})
	}
}

// TestDrainNeverSignalsProcessThatTookThePID: the signal goes through the
// handle opened before the checks, so a process that takes the PID after
// them is never signaled; the record is then stale.
func TestDrainNeverSignalsProcessThatTookThePID(t *testing.T) {
	s := newDrainSetup(t)
	successor := &fakeProc{pid: testPID, start: testStart + 1, state: 'S'}
	s.host.afterLocks = func(h *fakeHost) { h.procs[testPID] = successor }
	r := execute(t.Context(), s.opts, "linux", nil, "node", "drain", "--data-dir", s.root)
	want := "ruralz node drain: stale holder record: pid 4242 exited before the signal\n"
	if r.code != command.ExitNoResult || r.stderr != want {
		t.Fatalf("drain = exit %d, stderr %q; want 2, %q", r.code, r.stderr, want)
	}
	if successor.sigterms != 0 {
		t.Errorf("the process that took the PID got %d SIGTERMs", successor.sigterms)
	}
}

// TestDrainInterrupted: Ctrl-C while the drain waits exits 130 without an
// object or a message, and leaves the Node draining; one before the signal
// sends none.
func TestDrainInterrupted(t *testing.T) {
	t.Run("while waiting", func(t *testing.T) {
		for _, mode := range []string{"text", "json"} {
			s := newDrainSetup(t)
			ctx, cancel := context.WithCancelCause(t.Context())
			run := s.drain(ctx, "--output", mode)
			awaitSignal(t, s.host)
			armed(t, s.clk)
			cancel(command.Interrupted{Signal: os.Interrupt})
			r := run.wait(t)
			if r.code != command.ExitInterrupted || r.stdout != "" || r.stderr != "" {
				t.Errorf("%s: drain = exit %d, stdout %q, stderr %q; want 130 and nothing", mode, r.code, r.stdout, r.stderr)
			}
			if n := s.host.countCalls("terminate"); n != 1 {
				t.Errorf("%s: %d SIGTERMs, want 1", mode, n)
			}
		}
	})
	t.Run("before the signal", func(t *testing.T) {
		s := newDrainSetup(t)
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(command.Interrupted{Signal: os.Interrupt})
		r := execute(ctx, s.opts, "linux", nil, "node", "drain", "--data-dir", s.root)
		if r.code != command.ExitInterrupted || r.stdout != "" || r.stderr != "" {
			t.Errorf("drain = exit %d, stdout %q, stderr %q; want 130 and nothing", r.code, r.stdout, r.stderr)
		}
		if n := s.host.countCalls("terminate"); n != 0 {
			t.Errorf("%d SIGTERMs after Ctrl-C, want 0", n)
		}
	})
}

// TestProcessGone covers the states that end the wait.
func TestProcessGone(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *fakeHost)
		want  bool
	}{
		{"running", func(*fakeHost) {}, false},
		{"sleeping in I/O", func(h *fakeHost) { h.procs[testPID].state = 'D' }, false},
		{"stopped", func(h *fakeHost) { h.procs[testPID].state = 'T' }, false},
		{"zombie", func(h *fakeHost) { h.procs[testPID].state = 'Z' }, true},
		{"dead", func(h *fakeHost) { h.procs[testPID].state = 'X' }, true},
		{"dead, old kernels", func(h *fakeHost) { h.procs[testPID].state = 'x' }, true},
		{"reaped", func(h *fakeHost) { h.procs[testPID].reaped = true }, true},
		{"PID reused", func(h *fakeHost) { h.procs[testPID].start = 7 }, true},
		{"stat unreadable", func(h *fakeHost) { h.statErr[testPID] = errors.New("input/output error") }, false},
		{"stat malformed", func(h *fakeHost) { h.statText[testPID] = "garbage" }, false},
	}
	for _, tc := range cases {
		h := newFakeHost(t.TempDir())
		tc.setup(h)
		if got := processGone(h, testHolder(t)); got != tc.want {
			t.Errorf("processGone(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDataDir: --data-dir wins, even over RURALZ_DATA_DIR, an empty one is
// a usage error, RURALZ_DATA_DIR stands in for it unless empty, and
// /var/lib/ruralz is the default; a relative path is made absolute.
func TestDataDir(t *testing.T) {
	abs := func(p string) string {
		t.Helper()
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	cases := []struct {
		name  string
		set   bool
		flag  string
		env   map[string]string
		want  string
		usage bool
	}{
		{name: "default", want: abs(nodedir.DefaultRoot)},
		{name: "environment", env: map[string]string{EnvDataDir: "/srv/ruralz"}, want: abs("/srv/ruralz")},
		{name: "empty environment", env: map[string]string{EnvDataDir: ""}, want: abs(nodedir.DefaultRoot)},
		{name: "flag over environment", set: true, flag: "/data", env: map[string]string{EnvDataDir: "/srv/ruralz"}, want: abs("/data")},
		{name: "relative flag", set: true, flag: "data", want: abs("data")},
		{name: "empty flag", set: true, flag: "", env: map[string]string{EnvDataDir: "/srv/ruralz"}, usage: true},
	}
	for _, tc := range cases {
		got, err := dataDir(tc.set, tc.flag, env(tc.env))
		if tc.usage {
			if _, ok := errors.AsType[*command.UsageError](err); !ok || command.Code(t.Context(), err) != command.ExitNoResult {
				t.Errorf("dataDir(%s) = %q, %v; want a usage error", tc.name, got, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("dataDir(%s) = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// TestDrainDataDirFromEnvironment: without --data-dir the command reads
// the holder of RURALZ_DATA_DIR.
func TestDrainDataDirFromEnvironment(t *testing.T) {
	s := newDrainSetup(t)
	s.host.locksText = ""
	r := execute(t.Context(), s.opts, "linux", []string{EnvDataDir + "=" + s.root}, "node", "drain")
	want := "ruralz node drain: stale holder record: pid 4242 holds no FLOCK WRITE lock on " + lockPath(s.root) + "\n"
	if r.code != command.ExitNoResult || r.stderr != want {
		t.Fatalf("drain = exit %d, stderr %q; want 2, %q", r.code, r.stderr, want)
	}
}

// TestDrainUsageErrors: flag and argument problems exit 2 before any host
// call.
func TestDrainUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"zero timeout", []string{"--timeout", "0"}, `invalid value "0" for --timeout: must be greater than zero`},
		{"bad timeout", []string{"--timeout", "soon"}, `invalid value "soon" for --timeout: want a duration such as 30s or 5m`},
		{"unsupported output", []string{"--output", "yaml"}, `unsupported --output "yaml" (want text or json)`},
		{"argument", []string{"extra"}, `unexpected argument "extra"`},
		{"empty data dir", []string{"--data-dir="}, "--data-dir: want the path of a data directory, not an empty one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newDrainSetup(t)
			r := execute(t.Context(), s.opts, "linux", nil, append([]string{"node", "drain"}, tc.args...)...)
			want := "ruralz node drain: " + tc.want + "\nRun 'ruralz node drain --help' for usage.\n"
			if r.code != command.ExitNoResult || r.stdout != "" || r.stderr != want {
				t.Fatalf("drain %q = exit %d, stdout %q, stderr %q; want 2, %q", tc.args, r.code, r.stdout, r.stderr, want)
			}
			if calls := s.host.callLog(); len(calls) != 0 {
				t.Errorf("host calls %q after a usage error", calls)
			}
		})
	}
}

// TestDrainPlatforms: the command runs only on Linux; elsewhere it exits 2
// naming the platform before it touches anything.
func TestDrainPlatforms(t *testing.T) {
	for _, tc := range []struct{ goos, reason string }{
		{"darwin", "it needs /proc/locks"},
		{"windows", "there is no ruralzd and no SIGTERM"},
		{"freebsd", "it needs /proc/locks"},
	} {
		s := newDrainSetup(t)
		r := execute(t.Context(), s.opts, tc.goos, nil, "node", "drain", "--data-dir", s.root, "--timeout", "1m")
		want := "ruralz node drain is not planned on " + tc.goos + ": " + tc.reason + "\n"
		if r.code != command.ExitNoResult || r.stdout != "" || r.stderr != want {
			t.Errorf("%s: drain = exit %d, stdout %q, stderr %q; want 2, %q", tc.goos, r.code, r.stdout, r.stderr, want)
		}
		if calls := s.host.callLog(); len(calls) != 0 {
			t.Errorf("%s: host calls %q", tc.goos, calls)
		}
	}
}

// TestSecondsFormats: the drained line rounds to one decimal, the JSON
// member to milliseconds.
func TestSecondsFormats(t *testing.T) {
	for _, tc := range []struct {
		d            time.Duration
		tenth, milli string
	}{
		{0, "0.0", "0.000"},
		{-time.Second, "0.0", "0.000"},
		{time.Millisecond, "0.0", "0.001"},
		{1499 * time.Microsecond, "0.0", "0.001"},
		{2500 * time.Microsecond, "0.0", "0.003"},
		{49 * time.Millisecond, "0.0", "0.049"},
		{50 * time.Millisecond, "0.1", "0.050"},
		{61234500 * time.Microsecond, "61.2", "61.235"},
		{5 * time.Minute, "300.0", "300.000"},
	} {
		if got := tenths(tc.d); got != tc.tenth {
			t.Errorf("tenths(%v) = %q, want %q", tc.d, got, tc.tenth)
		}
		if got := milliseconds(tc.d); string(got) != tc.milli {
			t.Errorf("milliseconds(%v) = %q, want %q", tc.d, got, tc.milli)
		}
	}
}

// removeFile removes path.
func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}
