// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/ulid"
)

// Test identities of the lock holder.
const (
	testNodeID    = "01ARYZ6S41TSV4RRFFQ69G5FAV"
	testPID       = 4242
	testStart     = 1234567
	testNamespace = "pid:[4026531836]"
	testInode     = 1001
	testVersion   = "0.4.0"
)

func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// testHolder returns the record of the test holder.
func testHolder(t *testing.T) nodedir.Holder {
	t.Helper()
	id, err := ulid.Parse(testNodeID)
	if err != nil {
		t.Fatal(err)
	}
	return nodedir.Holder{
		Format: nodedir.HolderFormat, PID: testPID, StartTime: testStart, NodeID: id,
		Version: testVersion, PIDNamespace: testNamespace,
	}
}

// writeHolder writes h as holder.json in root.
func writeHolder(t *testing.T, root string, h nodedir.Holder) {
	t.Helper()
	data, err := h.Encode()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, nodedir.HolderFile), string(data))
}

// writeFile writes content to path with mode 0600.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// statLine returns a /proc/<pid>/stat line of a process named comm in
// state with start time start: fields 3 to 21, then field 22 and two more.
func statLine(pid int, comm string, state byte, start uint64) string {
	return fmt.Sprintf("%d (%s) %c 1 %d %d 0 -1 4194560 100 0 0 0 5 3 0 0 20 0 12 0 %d 123456 789\n",
		pid, comm, state, pid, pid, start)
}

// flockLine is a granted FLOCK WRITE line of /proc/locks.
func flockLine(n, pid int, ino uint64) string {
	return fmt.Sprintf("%d: FLOCK  ADVISORY  WRITE %d 00:2e:%d 0 EOF\n", n, pid, ino)
}

// fakeProc is one process of a fakeHost.
type fakeProc struct {
	pid   int
	start uint64
	// state is the stat state: S while it runs, Z once it exited and its
	// parent has not reaped it.
	state byte
	// reaped removes the process: its PID has no /proc entry.
	reaped bool
	// deny refuses signals with a permission error.
	deny bool
	// sigterms counts the SIGTERMs delivered.
	sigterms int
}

// fakeHost is a process table and procfs in memory. Its handles behave as
// pidfds: each refers to the process that had the PID when it was opened.
type fakeHost struct {
	mu      sync.Mutex
	ns      string
	nsErr   error
	procs   map[int]*fakeProc // the process that has each PID now
	statErr map[int]error
	// statText replaces the stat line of a PID.
	statText  map[int]string
	locksText string
	locksErr  error
	inodes    map[string]uint64
	inodeErr  error
	openErr   error
	// afterLocks runs at the end of a locks call, the last check, with
	// the host locked; tests change the process table there.
	afterLocks func(h *fakeHost)
	calls      []string
	// signaled receives the PID of each SIGTERM delivered.
	signaled chan int
}

// newFakeHost returns a host where the test holder runs in its namespace
// and holds the lock on root's lock file.
func newFakeHost(root string) *fakeHost {
	return &fakeHost{
		ns:        testNamespace,
		procs:     map[int]*fakeProc{testPID: {pid: testPID, start: testStart, state: 'S'}},
		statErr:   map[int]error{},
		statText:  map[int]string{},
		locksText: "1: POSIX  ADVISORY  READ 77 08:01:5 0 EOF\n" + flockLine(2, testPID, testInode),
		inodes:    map[string]uint64{lockPath(root): testInode},
		signaled:  make(chan int, 16),
	}
}

func (h *fakeHost) record(format string, args ...any) {
	h.calls = append(h.calls, fmt.Sprintf(format, args...))
}

func (h *fakeHost) pidNamespace() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("ns")
	return h.ns, h.nsErr
}

func (h *fakeHost) procStat(pid int) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("stat %d", pid)
	if err := h.statErr[pid]; err != nil {
		return nil, err
	}
	if line, ok := h.statText[pid]; ok {
		return []byte(line), nil
	}
	p := h.procs[pid]
	if p == nil || p.reaped {
		return nil, fmt.Errorf("open /proc/%d/stat: %w", pid, fs.ErrNotExist)
	}
	return []byte(statLine(p.pid, "ruralzd", p.state, p.start)), nil
}

func (h *fakeHost) locks() ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("locks")
	if h.afterLocks != nil {
		defer h.afterLocks(h)
	}
	return []byte(h.locksText), h.locksErr
}

func (h *fakeHost) inode(path string) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("inode %s", path)
	if h.inodeErr != nil {
		return 0, h.inodeErr
	}
	ino, ok := h.inodes[path]
	if !ok {
		return 0, fmt.Errorf("stat %s: %w", path, fs.ErrNotExist)
	}
	return ino, nil
}

func (h *fakeHost) open(pid int) (process, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record("open %d", pid)
	if h.openErr != nil {
		return nil, h.openErr
	}
	return &fakeHandle{h: h, pid: pid, p: h.procs[pid]}, nil
}

// fakeHandle refers to the process that had pid when it was opened.
type fakeHandle struct {
	h   *fakeHost
	pid int
	p   *fakeProc
}

func (f *fakeHandle) terminate() error {
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	f.h.record("terminate %d", f.pid)
	switch {
	case f.p == nil || f.p.reaped || f.p.state == 'Z' || f.h.procs[f.pid] != f.p:
		return os.ErrProcessDone
	case f.p.deny:
		return errDenied{}
	}
	f.p.sigterms++
	f.h.signaled <- f.pid
	return nil
}

func (f *fakeHandle) release() error {
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	f.h.record("release %d", f.pid)
	return nil
}

// errDenied is a signal the kernel refuses, as EPERM.
type errDenied struct{}

func (errDenied) Error() string { return "operation not permitted" }

func (errDenied) Unwrap() error { return os.ErrPermission }

// set changes the process table under the host's lock.
func (h *fakeHost) set(fn func(h *fakeHost)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h)
}

// callLog returns the calls made so far.
func (h *fakeHost) callLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// sigterms returns the SIGTERMs delivered to the process that has pid.
func (h *fakeHost) sigterms(pid int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.procs[pid]; p != nil {
		return p.sigterms
	}
	return 0
}

// countCalls returns how many calls start with prefix.
func (h *fakeHost) countCalls(prefix string) int {
	n := 0
	for _, c := range h.callLog() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// result is what one command.Execute returned.
type result struct {
	code           int
	stdout, stderr string
}

// execute runs ruralz args... over Specs(o) on goos with environ.
func execute(ctx context.Context, o Options, goos string, environ []string, args ...string) result {
	var out, errOut bytes.Buffer
	sio := command.NewIO(&out, &errOut, environ, epoch, goos)
	code := command.Execute(ctx, sio, "ruralz", Specs(o), args)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

// started is a command run in a goroutine.
type started struct {
	done chan result
}

// start runs ruralz args... on Linux in a goroutine.
func start(ctx context.Context, o Options, args ...string) *started {
	s := &started{done: make(chan result, 1)}
	go func() { s.done <- execute(ctx, o, "linux", nil, args...) }()
	return s
}

// wait returns the result under a real-time safety deadline.
func (s *started) wait(t *testing.T) result {
	t.Helper()
	select {
	case r := <-s.done:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("the command did not return")
		return result{}
	}
}

// waitFor polls cond under a real-time safety deadline; the logic under
// test runs on the fake clock.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// awaitSignal waits for the next SIGTERM the host delivers.
func awaitSignal(t *testing.T, h *fakeHost) int {
	t.Helper()
	select {
	case pid := <-h.signaled:
		return pid
	case <-time.After(30 * time.Second):
		t.Fatal("no SIGTERM was delivered")
		return 0
	}
}

// armed waits until the drain's poll timer is armed: after the signal,
// and again after each poll that found the process running.
func armed(t *testing.T, clk *clocktest.Fake) {
	t.Helper()
	waitFor(t, "the poll timer", func() bool { return clk.Pending() == 1 })
}

// tick waits until the drain's poll timer is armed, then advances clk by
// d.
func tick(t *testing.T, clk *clocktest.Fake, d time.Duration) {
	t.Helper()
	armed(t, clk)
	clk.Advance(d)
}

// drainSetup is a data directory with the test holder's record, a fake
// host where that holder holds the lock, and a fake clock.
type drainSetup struct {
	root string
	host *fakeHost
	clk  *clocktest.Fake
	opts Options
}

func newDrainSetup(t *testing.T) *drainSetup {
	t.Helper()
	root := t.TempDir()
	writeHolder(t, root, testHolder(t))
	host := newFakeHost(root)
	clk := clocktest.New(epoch())
	return &drainSetup{root: root, host: host, clk: clk, opts: Options{Clock: clk, host: host}}
}

// drain runs ruralz node drain --data-dir root args... in a goroutine.
func (s *drainSetup) drain(ctx context.Context, args ...string) *started {
	return start(ctx, s.opts, append([]string{"node", "drain", "--data-dir", s.root}, args...)...)
}
