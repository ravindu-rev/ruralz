// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// childStopGrace is how long a supervised child has to exit after SIGTERM
// before SIGKILL.
const childStopGrace = 10 * time.Second

// dockerTimeout bounds one docker kill of a container child.
const dockerTimeout = 30 * time.Second

// maxReadyLine bounds the unterminated output a ready line is matched
// against.
const maxReadyLine = 64 << 10

// ChildConfig configures a supervised child: a process that does not speak
// the lab protocol, such as Valkey on RH-1S, tied to the session so it
// stops when the harness closes standard input or the ssh session drops.
type ChildConfig struct {
	// Argv is the command and its arguments.
	Argv []string
	// ReadyAddr is a TCP address the child serves on; the Ready line is
	// written once it accepts a connection.
	ReadyAddr string
	// ReadyLine, when set, is a regular expression a line of the child's
	// output matches once it serves, such as Valkey's "Ready to accept
	// connections": the Ready line then also waits for it, since a port
	// accepting connections could be another process's.
	ReadyLine string
	// Container names the container the child runs, as docker run --name
	// does: signals then reach the container's process through docker
	// kill, since the docker client passes neither SIGKILL nor SIGSTOP
	// on, and a stop ends the container even when it is frozen.
	Container string
	// ReadyTimeout bounds the wait for the Ready line's conditions;
	// default 60 s.
	ReadyTimeout time.Duration
	// Stderr receives the child's output.
	Stderr io.Writer
}

// ChildExit is how a supervised child ended.
type ChildExit struct {
	// Code is the exit status, -1 when a signal ended the process.
	Code int `json:"code"`
	// Signal names the signal that ended the process, such as "SIGKILL";
	// a container's process killed by one ends its docker client with
	// status 128 plus the signal's number instead.
	Signal string `json:"signal,omitempty"`
}

// Child is a supervised child process.
type Child struct {
	cmd       *exec.Cmd
	addr      string
	container string
	cpu       *childCPU
	done      chan struct{}
	once      sync.Once
	err       error
	ready     *lineWatch

	mu sync.Mutex
	// ended reports a terminating signal the session delivered, after
	// which the child's end is expected.
	ended bool
}

// StartChild starts the child and waits until it wrote ReadyLine, when
// set, and ReadyAddr accepts a connection.
func StartChild(ctx context.Context, c ChildConfig) (*Child, error) {
	if len(c.Argv) == 0 {
		return nil, errors.New("lab: run: no command")
	}
	var re *regexp.Regexp
	if c.ReadyLine != "" {
		var err error
		if re, err = regexp.Compile(c.ReadyLine); err != nil {
			return nil, fmt.Errorf("lab: run: ready line: %w", err)
		}
	}
	out := c.Stderr
	if out == nil {
		out = io.Discard
	}
	w := &lineWatch{dst: out, re: re, matched: make(chan struct{})}
	// The child outlives ctx's cancellation: Close stops it gracefully.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), c.Argv[0], c.Argv[1:]...) //nolint:gosec // the harness names the supervised command
	cmd.Stdout, cmd.Stderr = w, w
	// A leftover holder of the output pipes, such as a container's
	// shim, never keeps Wait from returning.
	cmd.WaitDelay = childStopGrace
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lab: run %s: %w", c.Argv[0], err)
	}
	ch := &Child{cmd: cmd, addr: c.ReadyAddr, container: c.Container, done: make(chan struct{}), ready: w}
	go func() { // owned by the Child: Close waits for done
		ch.err = cmd.Wait()
		close(ch.done)
	}()
	ch.cpu = &childCPU{pid: cmd.Process.Pid}
	ch.cpu.reset()
	timeout := c.ReadyTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if re != nil {
		select {
		case <-w.matched:
		case <-ch.done:
			return nil, ch.exitedBefore(c.Argv[0], "writing "+c.ReadyLine)
		case <-rctx.Done():
			_ = ch.stop(ctx)
			return nil, fmt.Errorf("lab: run %s: no line matching %q: %w", c.Argv[0], c.ReadyLine, rctx.Err())
		}
	}
	if c.ReadyAddr == "" {
		return ch, nil
	}
	for {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(rctx, "tcp", c.ReadyAddr)
		if err == nil {
			_ = conn.Close()
			return ch, nil
		}
		select {
		case <-ch.done:
			return nil, ch.exitedBefore(c.Argv[0], "serving on "+c.ReadyAddr)
		case <-rctx.Done():
			_ = ch.stop(ctx)
			return nil, fmt.Errorf("lab: run %s: not serving on %s: %w", c.Argv[0], c.ReadyAddr, rctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// exitedBefore returns the error of a child that exited before what.
func (c *Child) exitedBefore(name, what string) error {
	if c.err == nil { // exit status 0: Wait has no error to wrap
		return fmt.Errorf("lab: run %s exited before %s: exit status 0", name, what)
	}
	return fmt.Errorf("lab: run %s exited before %s: %w", name, what, c.err)
}

// Ready describes the child.
func (c *Child) Ready() Ready {
	r := ready("run", c.addr)
	r.PID = c.cmd.Process.Pid
	return r
}

// Handle answers stats (the child's CPU), reset, signal and wait.
func (c *Child) Handle(ctx context.Context, r Request) Response {
	switch r.Op {
	case OpReset:
		c.cpu.reset()
		return Response{OK: true}
	case OpStats:
		select {
		case <-c.done:
			return Response{Error: fmt.Sprintf("lab: the child exited: %v", c.err)}
		default:
		}
		return Response{OK: true, Process: c.cpu.stats()}
	case OpSignal:
		if err := c.Signal(ctx, r.Name); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true}
	case OpWait:
		select {
		case <-c.done:
			return Response{OK: true, Exit: c.exit()}
		case <-ctx.Done():
			return Response{Error: fmt.Sprintf("lab: wait: %v", ctx.Err())}
		}
	default:
		return unknown(r)
	}
}

// Signal delivers the signal named name, such as "SIGSTOP", to the child,
// or to its container's process through docker kill. A child that exited
// takes no signal, but killing it is not an error, as killing an exited
// process is not.
func (c *Child) Signal(ctx context.Context, name string) error {
	sig, ok := signalNamed(name)
	if !ok {
		return fmt.Errorf("lab: signal %q is not one the run service delivers", name)
	}
	select {
	case <-c.done:
		if name == "SIGKILL" {
			return nil
		}
		return fmt.Errorf("lab: signal %s: the child exited: %w", name, c.exitError())
	default:
	}
	if terminating(name) {
		c.mu.Lock()
		c.ended = true
		c.mu.Unlock()
	}
	return c.deliver(ctx, name, sig)
}

// deliver sends sig, named name, to the child or its container.
func (c *Child) deliver(ctx context.Context, name string, sig syscall.Signal) error {
	if c.container == "" {
		if err := c.cmd.Process.Signal(sig); err != nil {
			return fmt.Errorf("lab: signal %s: %w", name, err)
		}
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	out, err := exec.CommandContext(dctx, "docker", "kill", "--signal", name, c.container).CombinedOutput() //nolint:gosec // G204: a signal name of the table and the container the harness named
	if err != nil {
		return fmt.Errorf("lab: docker kill --signal %s %s: %w: %s", name, c.container, err, bytes.TrimSpace(out))
	}
	return nil
}

// exit describes how the child ended, once done is closed.
func (c *Child) exit() *ChildExit {
	st := c.cmd.ProcessState
	if st == nil {
		return &ChildExit{Code: -1}
	}
	e := &ChildExit{Code: st.ExitCode()}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		e.Signal, _ = SignalName(ws.Signal())
	}
	return e
}

// exitError returns the error the child's exit carries: Wait's, or for
// exit status 0, which has none, one naming that status.
func (c *Child) exitError() error {
	if c.err == nil {
		return errors.New("exit status 0")
	}
	return c.err
}

// Close stops the child unless it exited: SIGCONT, so a frozen child
// takes the next signal, SIGTERM, then SIGKILL after the grace period,
// each to its container's process too when it runs one. A child ended by
// a signal, as by these or by a terminating signal the session delivered,
// ended as expected.
func (c *Child) Close() error { return c.stop(context.Background()) }

// stop is Close; its deliveries carry ctx's values but outlive its end,
// so a child whose start timed out still stops.
func (c *Child) stop(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	c.once.Do(func() {
		select {
		case <-c.done:
			return
		default:
		}
		for _, s := range []string{"SIGCONT", "SIGTERM"} {
			if sig, ok := signalNamed(s); ok {
				_ = c.deliver(ctx, s, sig)
			}
		}
		select {
		case <-c.done:
		case <-time.After(childStopGrace):
			if c.container != "" {
				if sig, ok := signalNamed("SIGKILL"); ok {
					_ = c.deliver(ctx, "SIGKILL", sig)
				}
			}
			_ = c.cmd.Process.Kill()
			<-c.done
		}
		c.mu.Lock()
		c.ended = true
		c.mu.Unlock()
	})
	var exit *exec.ExitError
	if errors.As(c.err, &exit) && !exit.Exited() {
		return nil // ended by a signal
	}
	c.mu.Lock()
	ended := c.ended
	c.mu.Unlock()
	if ended && (c.err == nil || errors.As(c.err, &exit)) {
		return nil // a container's client reports its signal as a status
	}
	return c.err
}

// lineWatch passes a child's output on to dst and closes matched at the
// first line matching re; safe for concurrent use by the child's two
// output streams.
type lineWatch struct {
	dst     io.Writer
	re      *regexp.Regexp
	matched chan struct{}

	mu   sync.Mutex
	line []byte
	seen bool
}

func (w *lineWatch) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.re != nil && !w.seen {
		for _, c := range b {
			if c != '\n' {
				if len(w.line) < maxReadyLine {
					w.line = append(w.line, c)
				}
				continue
			}
			if w.re.Match(w.line) {
				w.seen = true
				close(w.matched)
				w.line = nil
				break
			}
			w.line = w.line[:0]
		}
	}
	_, _ = w.dst.Write(b)
	return len(b), nil
}

// childCPU measures another process's CPU since its last reset.
type childCPU struct {
	pid   int
	mu    sync.Mutex
	cpu   time.Duration
	start time.Time
}

func (m *childCPU) sample() time.Duration {
	u, err := proc.SampleProc("/proc", m.pid)
	if err != nil {
		return 0
	}
	return u.UserCPU + u.SysCPU
}

func (m *childCPU) reset() {
	m.mu.Lock()
	m.cpu, m.start = m.sample(), time.Now()
	m.mu.Unlock()
}

func (m *childCPU) stats() ProcessStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ProcessStats{CPUSeconds: (m.sample() - m.cpu).Seconds(), WallSeconds: time.Since(m.start).Seconds(), CPUs: newCPUMeter().stats().CPUs}
}
