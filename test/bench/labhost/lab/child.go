// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// childStopGrace is how long a supervised child has to exit after SIGTERM
// before SIGKILL.
const childStopGrace = 10 * time.Second

// ChildConfig configures a supervised child: a process that does not speak
// the lab protocol, such as Valkey on RH-1S, tied to the session so it
// stops when the harness closes standard input or the ssh session drops.
type ChildConfig struct {
	// Argv is the command and its arguments.
	Argv []string
	// ReadyAddr is a TCP address the child serves on; the Ready line is
	// written once it accepts a connection.
	ReadyAddr string
	// ReadyTimeout bounds the wait for ReadyAddr; default 60 s.
	ReadyTimeout time.Duration
	// Stderr receives the child's output.
	Stderr *os.File
}

// Child is a supervised child process.
type Child struct {
	cmd  *exec.Cmd
	addr string
	cpu  *childCPU
	done chan struct{}
	once sync.Once
	err  error
}

// StartChild starts the child and waits until ReadyAddr accepts a
// connection.
func StartChild(ctx context.Context, c ChildConfig) (*Child, error) {
	if len(c.Argv) == 0 {
		return nil, errors.New("lab: run: no command")
	}
	// The child outlives ctx's cancellation: Close stops it gracefully.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), c.Argv[0], c.Argv[1:]...) //nolint:gosec // the harness names the supervised command
	cmd.Stdout, cmd.Stderr = c.Stderr, c.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lab: run %s: %w", c.Argv[0], err)
	}
	ch := &Child{cmd: cmd, addr: c.ReadyAddr, done: make(chan struct{})}
	go func() { // owned by the Child: Close waits for done
		ch.err = cmd.Wait()
		close(ch.done)
	}()
	ch.cpu = &childCPU{pid: cmd.Process.Pid}
	ch.cpu.reset()
	if c.ReadyAddr == "" {
		return ch, nil
	}
	timeout := c.ReadyTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(rctx, "tcp", c.ReadyAddr)
		if err == nil {
			_ = conn.Close()
			return ch, nil
		}
		select {
		case <-ch.done:
			return nil, fmt.Errorf("lab: run %s exited before serving on %s: %v", c.Argv[0], c.ReadyAddr, ch.err)
		case <-rctx.Done():
			_ = ch.Close()
			return nil, fmt.Errorf("lab: run %s: not serving on %s: %w", c.Argv[0], c.ReadyAddr, rctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Ready describes the child.
func (c *Child) Ready() Ready {
	r := ready("run", c.addr)
	r.PID = c.cmd.Process.Pid
	return r
}

// Handle answers stats (the child's CPU) and reset.
func (c *Child) Handle(_ context.Context, r Request) Response {
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
	default:
		return unknown(r)
	}
}

// Close stops the child: SIGTERM, then SIGKILL after the grace period.
func (c *Child) Close() error {
	c.once.Do(func() {
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-c.done:
		case <-time.After(childStopGrace):
			_ = c.cmd.Process.Kill()
			<-c.done
		}
	})
	var exit *exec.ExitError
	if errors.As(c.err, &exit) && !exit.Exited() {
		return nil // ended by our signal
	}
	return c.err
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
