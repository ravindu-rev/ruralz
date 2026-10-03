// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package proc starts, signals, waits for and samples the processes of a
// test topology (11 section 3): ruralzd Nodes, ruralz commands, redis-server
// and re-executed test binaries. Each Process captures stdout and stderr
// into optional log files plus bounded in-memory rings (up to 8 MiB each by
// default, allocated as output arrives), is started and reaped by one
// goroutine it owns, and is killed when the context given to Start ends, so
// no process outlives its test.
//
// On Linux, Sample reads CPU time, RSS and thread count from /proc (11 req
// 61, 68), and InNetNS re-executes a test inside a fresh network namespace
// holding only a loopback interface, with per-namespace sysctls such as
// net.ipv4.tcp_migrate_req (11 req 40, 44).
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultRingBytes bounds the in-memory copy of each output stream.
const DefaultRingBytes = 8 << 20

// ringInitialBytes is a ring's first allocation; it doubles as output
// arrives, up to the ring's bound, so a quiet process costs little heap.
const ringInitialBytes = 64 << 10

// MaxLineBytes bounds the unterminated last line WaitOutput matches again
// as it grows; a longer line is matched in pieces of at least this size.
const MaxLineBytes = 64 << 10

// WaitDelay bounds how long the reaper waits for the output pipes to
// close after the process exits (a leftover child may hold them).
const WaitDelay = 5 * time.Second

// Spec describes a process to start.
type Spec struct {
	// Name prefixes the log files, for example "node-3"; default the base
	// name of Path.
	Name string
	// Path is the binary to run.
	Path string
	Args []string
	// Env is the complete environment: nothing is inherited implicitly
	// (nil means an empty environment).
	Env []string
	// Dir is the working directory; default the caller's.
	Dir string
	// LogDir, when set, receives <Name>.out and <Name>.err (appended, so
	// a restarted process keeps its earlier output).
	LogDir string
	// RingBytes bounds each in-memory output ring; default
	// DefaultRingBytes. A ring starts at 64 KiB and grows to this bound
	// only as the process writes.
	RingBytes int
	// Stdin is the process's standard input; default none.
	Stdin io.Reader
}

// Usage is one resource sample.
type Usage struct {
	UserCPU, SysCPU time.Duration
	RSSBytes        int64
	Threads         int
	// Time is when the sample was taken.
	Time time.Time
}

// CPUShare returns the CPU used between two samples as a share of one
// CPU (1.0 = one core fully busy); 0 when no time passed.
func CPUShare(prev, cur Usage) float64 {
	wall := cur.Time.Sub(prev.Time)
	if wall <= 0 {
		return 0
	}
	used := (cur.UserCPU + cur.SysCPU) - (prev.UserCPU + prev.SysCPU)
	return float64(used) / float64(wall)
}

// Process is a started process.
type Process struct {
	name           string
	cmd            *exec.Cmd
	stdout, stderr *ring
	done           chan struct{}
	outPath        string
	errPath        string
	// Set by the reaper before done is closed.
	state   *os.ProcessState
	waitErr error
	mu      sync.Mutex // serializes signals with the reaper's exit
	exited  bool
	// scanned counts the output bytes WaitOutput handed to its regexps,
	// so tests can bound the cost of waiting on a chatty process.
	scanned atomic.Int64
}

// Start starts the process. When ctx ends, the process (and its process
// group on Linux) is killed; pass a context that outlives graceful
// shutdown (Signal, then Wait) when the test drains processes itself.
//
// On Linux the child also gets SIGKILL (Pdeathsig) when the test process
// dies. The kernel ties Pdeathsig to the OS thread that forked the child,
// not to the process, and the Go runtime ends a thread when a goroutine
// locked to it (runtime.LockOSThread, as setns helpers use) exits without
// unlocking. Start therefore forks from the Process's own goroutine, which
// keeps its thread locked until the child has exited, so no other
// goroutine can end that thread; code that starts children by other means
// must take the same care.
func Start(ctx context.Context, s Spec) (*Process, error) {
	if s.Path == "" {
		return nil, errors.New("proc: Spec.Path is empty")
	}
	name := s.Name
	if name == "" {
		name = filepath.Base(s.Path)
	}
	size := s.RingBytes
	if size <= 0 {
		size = DefaultRingBytes
	}
	p := &Process{name: name, done: make(chan struct{}), stdout: newRing(size), stderr: newRing(size)}
	if s.LogDir != "" {
		p.outPath = filepath.Join(s.LogDir, name+".out")
		p.errPath = filepath.Join(s.LogDir, name+".err")
		var err error
		if p.stdout.file, err = openLog(p.outPath); err != nil {
			return nil, err
		}
		if p.stderr.file, err = openLog(p.errPath); err != nil {
			_ = p.stdout.file.Close()
			return nil, err
		}
	}
	cmd := exec.CommandContext(ctx, s.Path, s.Args...) //nolint:gosec // G204: the harness runs binaries it built
	// Kill does nothing once the reaper has recorded the exit.
	cmd.Cancel = p.Kill
	cmd.Env = s.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = s.Dir
	if s.Stdin != nil {
		cmd.Stdin = s.Stdin
	}
	cmd.Stdout = p.stdout
	cmd.Stderr = p.stderr
	cmd.SysProcAttr = sysProcAttr()
	cmd.WaitDelay = WaitDelay
	p.cmd = cmd
	started := make(chan error, 1)
	go p.run(started)
	if err := <-started; err != nil {
		p.stdout.closeFile()
		p.stderr.closeFile()
		return nil, fmt.Errorf("proc: start %s: %w", name, err)
	}
	return p, nil
}

// run is the one goroutine a Process owns: it starts the process, reports
// the start on started and reaps it, holding its OS thread from the fork
// until the exit (see Start). It ends when the process exits or fails to
// start.
func (p *Process) run(started chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := p.cmd.Start(); err != nil {
		started <- err
		return
	}
	started <- nil
	p.reap()
}

func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: log path under the caller's LogDir
	if err != nil {
		return nil, fmt.Errorf("proc: %w", err)
	}
	return f, nil
}

// reap waits for the process to exit and records how it ended.
func (p *Process) reap() {
	err := p.cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) || errors.Is(err, exec.ErrWaitDelay) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		err = nil // the exit state says how it ended
	}
	p.mu.Lock()
	p.exited = true
	p.state = p.cmd.ProcessState
	p.waitErr = err
	p.mu.Unlock()
	p.stdout.finish()
	p.stderr.finish()
	close(p.done)
}

// Name returns the process name.
func (p *Process) Name() string { return p.name }

// PID returns the process ID.
func (p *Process) PID() int { return p.cmd.Process.Pid }

// Signal sends sig to the process; after exit it returns
// os.ErrProcessDone.
func (p *Process) Signal(sig os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return os.ErrProcessDone
	}
	if err := p.cmd.Process.Signal(sig); err != nil {
		return fmt.Errorf("proc: signal %s: %w", p.name, err)
	}
	return nil
}

// Kill sends SIGKILL to the process and, on Linux, to its whole process
// group. Killing an exited process is not an error.
func (p *Process) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return nil
	}
	killGroup(p.cmd.Process.Pid)
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("proc: kill %s: %w", p.name, err)
	}
	return nil
}

// Done is closed once the process has exited and its output is captured.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait waits for the process to exit and returns its exit code (-1 when a
// signal ended it). The error is ctx's when ctx ends first, or a failure
// to wait; a non-zero exit is not an error.
func (p *Process) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
		return p.state.ExitCode(), p.waitErr
	case <-ctx.Done():
		return -1, fmt.Errorf("proc: wait for %s: %w", p.name, ctx.Err())
	}
}

// State returns the exit state once Done is closed, else nil.
func (p *Process) State() *os.ProcessState {
	select {
	case <-p.done:
		return p.state
	default:
		return nil
	}
}

// Output returns copies of the retained tails of stdout and stderr.
func (p *Process) Output() (stdout, stderr []byte) {
	return p.stdout.bytes(), p.stderr.bytes()
}

// Written returns the total bytes the process wrote to stdout and stderr,
// including bytes the rings no longer hold.
func (p *Process) Written() (stdout, stderr int64) {
	return p.stdout.written(), p.stderr.written()
}

// LogPaths returns the stdout and stderr log files ("" without LogDir).
func (p *Process) LogPaths() (stdout, stderr string) { return p.outPath, p.errPath }

// WaitOutput waits until a line of stdout or stderr matches re and returns
// the submatches of the first match (stdout searched first). It starts
// with the output the rings still hold. re is matched against each line,
// newline included, on its own, and against the unterminated last line as
// it grows, so a match never spans lines; a line longer than MaxLineBytes
// is matched in pieces. Each byte is read and matched once (the last line
// again as it grows), so waiting costs time in proportion to the output
// written, not to the ring size, and does not slow a chatty process. It
// fails when ctx ends or the process exits without a match.
func (p *Process) WaitOutput(ctx context.Context, re *regexp.Regexp) ([]string, error) {
	ms := []*lineMatcher{{r: p.stdout, re: re}, {r: p.stderr, re: re}}
	defer func() {
		for _, m := range ms {
			p.scanned.Add(m.scanned)
		}
	}()
	for {
		outCh, errCh := p.stdout.changed(), p.stderr.changed()
		exited := false
		select {
		case <-p.done:
			exited = true
		default:
		}
		for _, m := range ms {
			if sm := m.scan(); sm != nil {
				return sm, nil
			}
		}
		if exited {
			return nil, fmt.Errorf("proc: %s exited (%v) before its output matched %s", p.name, p.state, re)
		}
		select {
		case <-outCh:
		case <-errCh:
		case <-p.done:
		case <-ctx.Done():
			return nil, fmt.Errorf("proc: %s output never matched %s: %w", p.name, re, ctx.Err())
		}
	}
}

// lineMatcher matches a regexp against one ring's output line by line,
// reading only the bytes written since its previous scan (WaitOutput).
type lineMatcher struct {
	r       *ring
	re      *regexp.Regexp
	off     int64  // ring offset of the next byte to read
	line    []byte // the unterminated last line read so far
	scanned int64  // bytes handed to re
}

// scan reads the output written since the previous scan and returns the
// submatches of the first line matching, or nil.
func (m *lineMatcher) scan() []string {
	held := len(m.line)
	buf, from, end := m.r.since(m.line, m.off)
	if from != m.off {
		// The ring dropped bytes before they were read: the held line
		// does not continue into what follows.
		buf = buf[held:]
	}
	m.off = end
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		if sm := m.match(buf[:i+1]); sm != nil {
			return sm
		}
		buf = buf[i+1:]
	}
	if len(buf) == 0 {
		m.line = m.line[:0]
		return nil
	}
	sm := m.match(buf)
	if len(buf) >= MaxLineBytes {
		// Matched once; the rest of this long line starts a new piece.
		buf = nil
	}
	m.line = append(m.line[:0], buf...)
	return sm
}

func (m *lineMatcher) match(line []byte) []string {
	m.scanned += int64(len(line))
	// Match needs no capture bookkeeping; most lines do not match.
	if !m.re.Match(line) {
		return nil
	}
	sm := m.re.FindSubmatch(line)
	out := make([]string, len(sm))
	for i, s := range sm {
		out[i] = string(s)
	}
	return out
}

// ring keeps the last limit bytes written, optionally tees them to a file,
// and wakes waiters on every write. Its buffer starts small and doubles up
// to limit as output arrives.
type ring struct {
	mu      sync.Mutex
	buf     []byte
	limit   int // bound of len(buf)
	start   int // index of the oldest byte
	n       int // bytes held
	total   int64
	file    *os.File
	fileErr error
	notify  chan struct{} // closed on the next write; nil when nobody waits
	closed  bool
}

func newRing(limit int) *ring {
	return &ring{buf: make([]byte, min(limit, ringInitialBytes)), limit: limit}
}

// grow enlarges buf, keeping the held bytes in order, until it holds need
// bytes or reaches limit.
func (r *ring) grow(need int) {
	size := len(r.buf)
	if need <= size || size >= r.limit {
		return
	}
	for size < need && size < r.limit {
		size *= 2
	}
	nb := make([]byte, min(size, r.limit))
	k := copy(nb, r.buf[r.start:min(r.start+r.n, len(r.buf))])
	copy(nb[k:], r.buf[:r.n-k])
	r.buf, r.start = nb, 0
}

// Write never fails, so the child never blocks on a failed log file.
func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil && r.fileErr == nil {
		if _, err := r.file.Write(p); err != nil {
			r.fileErr = err
		}
	}
	r.total += int64(len(p))
	r.grow(r.n + len(p))
	c := len(r.buf)
	data := p
	if len(data) >= c {
		copy(r.buf, data[len(data)-c:])
		r.start, r.n = 0, c
	} else {
		end := (r.start + r.n) % c
		k := copy(r.buf[end:], data)
		copy(r.buf, data[k:])
		if over := r.n + len(data) - c; over > 0 {
			r.start = (r.start + over) % c
			r.n = c
		} else {
			r.n += len(data)
		}
	}
	if r.notify != nil {
		close(r.notify)
		r.notify = nil
	}
	return len(p), nil
}

// bytes returns a copy of the held bytes, oldest first.
func (r *ring) bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, r.n)
	k := copy(out, r.buf[r.start:min(r.start+r.n, len(r.buf))])
	copy(out[k:], r.buf[:r.n-k])
	return out
}

// since appends to dst the held bytes written at or after offset off and
// returns dst, the offset of the first byte appended (later than off when
// the ring dropped bytes since) and the total written. It copies only
// those bytes, so a waiter holds the lock for the new output alone.
func (r *ring) since(dst []byte, off int64) ([]byte, int64, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	oldest := r.total - int64(r.n)
	from := max(off, oldest)
	cnt := int(r.total - from)
	if cnt <= 0 {
		return dst, r.total, r.total
	}
	i := (r.start + int(from-oldest)) % len(r.buf)
	first := min(cnt, len(r.buf)-i)
	dst = append(dst, r.buf[i:i+first]...)
	dst = append(dst, r.buf[:cnt-first]...)
	return dst, from, r.total
}

func (r *ring) written() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// changed returns a channel closed by the next write (or at once after
// finish).
func (r *ring) changed() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	if r.notify == nil {
		r.notify = make(chan struct{})
	}
	return r.notify
}

// finish closes the log file and wakes every waiter for good.
func (r *ring) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.notify != nil {
		close(r.notify)
		r.notify = nil
	}
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
}

// closeFile closes the log file of a ring whose process never started.
func (r *ring) closeFile() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
}
