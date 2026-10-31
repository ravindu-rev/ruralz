// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package topology

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// A running Node that does not answer a command in time breaks the clock
// for good: its stream is out of step, so later commands fail too instead
// of reading a late answer. A Node stopped behind the harness's back, not
// through Node.Signal, shows it, once every thread of its process has
// stopped: SIGSTOP takes effect after kill returns, so until then the
// Node can still read the command and answer it.
func TestClockBreaksOnAnUnansweredCommand(t *testing.T) {
	topo := standinTopology(t, func(c *Config) { c.KillAfter = time.Second })
	p := addNodes(t, topo, 1, nodeOptions(t, topo))[0].Process()
	if err := p.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Signal(syscall.SIGCONT) }()
	awaitStopped(t, p)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := topo.Advance(ctx, time.Second)
	if err == nil || !strings.Contains(err.Error(), "clock broken") {
		t.Fatalf("Advance past a frozen Node = %v, want the clock broken", err)
	}
	if again := topo.Advance(t.Context(), time.Second); again == nil || again.Error() != err.Error() {
		t.Errorf("the next Advance = %v, want the same breakage", again)
	}
}

// awaitStopped waits until every thread of p's process is stopped, as
// /proc/<pid>/task/<tid>/stat shows each thread's state.
func awaitStopped(t *testing.T, p *proc.Process) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second) // a hang guard only
	defer cancel()
	tasks := filepath.Join("/proc", strconv.Itoa(p.PID()), "task")
	for {
		stopped, err := threadsStopped(tasks)
		switch {
		case err != nil:
			t.Fatalf("%s: %v", p.Name(), err)
		case stopped:
			return
		}
		select {
		case <-p.Done():
			t.Fatalf("%s exited instead of stopping", p.Name())
		case <-ctx.Done():
			t.Fatalf("%s did not stop after SIGSTOP: %v", p.Name(), ctx.Err())
		case <-time.After(PollInterval):
		}
	}
}

// threadsStopped reports whether every thread listed under the task
// directory tasks is stopped (state T); a thread that exited meanwhile
// counts for nothing.
func threadsStopped(tasks string) (bool, error) {
	entries, err := os.ReadDir(tasks)
	if err != nil {
		return false, err
	}
	seen := 0
	for _, e := range entries {
		stat, err := os.ReadFile(filepath.Join(tasks, e.Name(), "stat")) //nolint:gosec // G304: a /proc path built from a PID and a thread ID
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		// The state is the first field after the command name, which may
		// hold spaces and parentheses, so after its last ')'.
		i := bytes.LastIndexByte(stat, ')')
		if i < 0 || i+2 >= len(stat) {
			return false, fmt.Errorf("thread %s: stat without a state: %q", e.Name(), stat)
		}
		if stat[i+2] != 'T' {
			return false, nil
		}
		seen++
	}
	return seen > 0, nil
}
