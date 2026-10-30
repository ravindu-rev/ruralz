// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// stopGrace bounds a service's stop after its stdin closed.
const stopGrace = 30 * time.Second

// Runner runs commands on one lab host: locally on the harness's own host,
// over ssh as the profile's dedicated user on a remote one, each process
// in a systemd-run scope on its cpuset when the profile asks for scopes.
type Runner struct {
	p    *Profile
	role Role
	host Host
}

// Runner returns the runner of role r's host.
func (p *Profile) Runner(r Role) *Runner { return &Runner{p: p, role: r, host: p.Hosts[r]} }

// Remote reports a host reached over ssh.
func (r *Runner) Remote() bool { return r.host.SSH != "" }

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(c rune) bool {
		return !(c == '-' || c == '_' || c == '.' || c == '/' || c == '=' || c == ':' || c == ',' || c == '+' || c == '@' ||
			('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sshArgs are the options of every ssh and scp call.
func (r *Runner) sshArgs() []string {
	return []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + r.p.KnownHosts,
		"-o", "ServerAliveInterval=15", "-i", r.p.SSHKey}
}

// Argv returns the local command line that runs argv on the host, in a
// scope on cpus when the profile asks for scopes and cpus is set.
func (r *Runner) Argv(argv []string, cpus string) []string {
	if r.p.Scope && cpus != "" {
		argv = append([]string{"systemd-run", "--user", "--scope", "--quiet", "--collect", "-p", "AllowedCPUs=" + cpus, "--"}, argv...)
	}
	if !r.Remote() {
		return argv
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return append(append([]string{"ssh"}, r.sshArgs()...), "-l", r.p.SSHUser, r.host.SSH, "--", strings.Join(quoted, " "))
}

// Output runs argv on the host to its end with stdin, and returns its
// standard output; a failure carries the end of its standard error.
func (r *Runner) Output(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	return r.OutputOn(ctx, argv, stdin, "")
}

// OutputOn is Output in a scope on cpus when the profile asks for scopes.
func (r *Runner) OutputOn(ctx context.Context, argv []string, stdin []byte, cpus string) ([]byte, error) {
	a := r.Argv(argv, cpus)
	cmd := exec.CommandContext(ctx, a[0], a[1:]...) //nolint:gosec // the harness composes the lab's own commands
	cmd.Stdin = bytes.NewReader(stdin)
	var out bytes.Buffer
	errb := &tail{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = &out, errb
	cmd.WaitDelay = stopGrace
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("harness: %s on %s: %w: %s", argv[0], r.role, err, errb.String())
	}
	return out.Bytes(), nil
}

// Put copies the local file src to dst on the host, creating dst's
// directory.
func (r *Runner) Put(ctx context.Context, src, dst string, mode os.FileMode) error {
	if !r.Remote() {
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return err
		}
		b, err := os.ReadFile(src) //nolint:gosec // G304: a file of the run directory
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, mode)
	}
	if _, err := r.Output(ctx, []string{"mkdir", "-p", filepath.Dir(dst)}, nil); err != nil {
		return err
	}
	args := append(append([]string{"-q"}, r.sshArgs()...), src, r.p.SSHUser+"@"+r.host.SSH+":"+dst)
	cmd := exec.CommandContext(ctx, "scp", args...) //nolint:gosec // the harness composes the copy
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("harness: copy %s to %s:%s: %w: %s", src, r.role, dst, err, out)
	}
	_, err := r.Output(ctx, []string{"chmod", fmt.Sprintf("%o", mode), dst}, nil)
	return err
}

// Proc is a lab service the harness started, driven over its stdin and
// stdout through a lab.Client.
type Proc struct {
	*lab.Client
	name  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	errb  *tail
	done  chan struct{}
	err   error
}

// Start starts argv on the host as a lab service, in a scope on cpus, and
// waits for its ready line.
func (r *Runner) Start(ctx context.Context, name string, argv []string, cpus string) (*Proc, error) {
	a := r.Argv(argv, cpus)
	cmd := exec.CommandContext(ctx, a[0], a[1:]...) //nolint:gosec // the harness composes the lab's own commands
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &Proc{name: name, cmd: cmd, stdin: stdin, errb: &tail{max: 64 << 10}, done: make(chan struct{})}
	cmd.Stderr = p.errb
	cmd.WaitDelay = stopGrace
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("harness: start %s on %s: %w", name, r.role, err)
	}
	ready := make(chan error, 1)
	go func() { // owned by the Proc: it ends with the process
		c, err := lab.NewClient(stdin, stdout)
		p.Client = c
		ready <- err
		if err != nil {
			_, _ = io.Copy(io.Discard, stdout)
		}
	}()
	go func() { // owned by the Proc: Stop waits for done
		<-ready
		p.err = cmd.Wait()
		close(p.done)
	}()
	select {
	case err := <-ready:
		ready <- err // for the waiter
		if err != nil {
			_ = p.Kill()
			return nil, fmt.Errorf("harness: %s on %s: %w: %s", name, r.role, err, p.errb.String())
		}
		return p, nil
	case <-ctx.Done():
		_ = p.Kill()
		return nil, ctx.Err()
	}
}

// Stop closes the service's stdin, reads its final response and waits for
// it to exit, killing it after the grace period.
func (p *Proc) Stop() (lab.Response, error) {
	_ = p.stdin.Close()
	var last lab.Response
	var lerr error
	if p.Client != nil {
		last, lerr = p.Last()
	}
	select {
	case <-p.done:
	case <-time.After(stopGrace):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	if p.err != nil && !errors.Is(p.err, exec.ErrWaitDelay) {
		return last, fmt.Errorf("harness: %s: %w: %s", p.name, p.err, p.errb.String())
	}
	return last, lerr
}

// Kill ends the service at once and waits for it.
func (p *Proc) Kill() error {
	_ = p.stdin.Close()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(stopGrace):
	}
	return nil
}

// Stderr returns the end of the service's standard error.
func (p *Proc) Stderr() string { return p.errb.String() }

// tail keeps the last max bytes written; safe for concurrent use.
type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
