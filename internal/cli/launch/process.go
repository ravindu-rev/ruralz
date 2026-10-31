// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Child is a running ruralzd launched by the CLI. Its methods are safe for
// concurrent use.
type Child struct {
	cmd   *exec.Cmd
	done  chan struct{}
	clock clock.Clock
	// kill cancels the context the command runs under, which sends
	// SIGKILL.
	kill context.CancelFunc
	logs []*logWriter

	mu        sync.Mutex
	err       error // Wait's result, once done is closed
	terms     int
	killTimer clock.Timer
	killed    bool
}

// StartOptions configure Start.
type StartOptions struct {
	// Binary is the ruralzd to run.
	Binary string
	// Environ is the child's environment (Environ).
	Environ []string
	// Logs receives the child's standard output and standard error.
	Logs io.Writer
	// Compact turns each JSON log line into its short form: Logs is a
	// terminal and JSON output is off.
	Compact bool
}

// Start runs ruralzd as so describes, with no arguments, in its own
// process group, so a Ctrl-C at the terminal reaches only the CLI, which
// forwards it as SIGTERM; it closes o.ExtraFiles, which the child then
// holds. The child does not end with ctx: the end of ctx, as Ctrl-C, starts
// its Drain through Terminate, and only Kill sends SIGKILL. A child that
// cannot start exits 2.
func Start(ctx context.Context, so StartOptions, o Options) (*Child, error) {
	runCtx, kill := context.WithCancel(context.WithoutCancel(ctx))
	cmd := exec.CommandContext(runCtx, so.Binary) //nolint:gosec // G204: the ruralzd FindRuralzd chose.
	cmd.Env = so.Environ
	logs := &syncWriter{w: so.Logs}
	stdout := newLogWriter(logs, so.Compact)
	stderr := newLogWriter(logs, so.Compact)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.ExtraFiles = o.ExtraFiles
	cmd.SysProcAttr = processGroup()
	err := cmd.Start()
	for _, f := range o.ExtraFiles {
		_ = f.Close()
	}
	if err != nil {
		kill()
		return nil, command.NoResultf("start %s: %w", so.Binary, err)
	}
	c := &Child{cmd: cmd, done: make(chan struct{}), clock: o.timer(), kill: kill, logs: []*logWriter{stdout, stderr}}
	go c.wait()
	return c, nil
}

// wait reaps the child, flushes its last log lines and closes done.
func (c *Child) wait() {
	err := c.cmd.Wait()
	for _, l := range c.logs {
		l.flush()
	}
	c.mu.Lock()
	c.err = err
	if c.killTimer != nil {
		c.killTimer.Stop()
	}
	c.mu.Unlock()
	c.kill()
	close(c.done)
}

// PID returns the child's process ID.
func (c *Child) PID() int { return c.cmd.Process.Pid }

// Done is closed once the child has exited and been reaped.
func (c *Child) Done() <-chan struct{} { return c.done }

// Exited reports whether the child has exited.
func (c *Child) Exited() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// Status describes how the child exited, such as "exit status 1" or
// "signal: killed"; call it once Done is closed.
func (c *Child) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		return "exit status 0"
	}
	var ee *exec.ExitError
	if errors.As(c.err, &ee) {
		return ee.String()
	}
	return c.err.Error()
}

// Terminate sends the child SIGTERM: the first starts its Drain and the
// exit bound, after which the child gets SIGKILL; the second jumps the
// Drain to its deadline. Later calls send nothing more. It reports how
// many SIGTERMs the child has received.
func (c *Child) Terminate() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.terms >= 2 || c.Exited() {
		return c.terms, nil
	}
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return c.terms, nil
		}
		return c.terms, fmt.Errorf("send SIGTERM to ruralzd (pid %d): %w", c.PID(), err)
	}
	c.terms++
	if c.terms == 1 {
		c.killTimer = c.clock.AfterFunc(ExitBound, c.Kill)
	}
	return c.terms, nil
}

// Kill sends the child SIGKILL.
func (c *Child) Kill() {
	c.mu.Lock()
	c.killed = true
	c.mu.Unlock()
	c.kill()
}

// Killed reports whether the child was sent SIGKILL.
func (c *Child) Killed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.killed
}
