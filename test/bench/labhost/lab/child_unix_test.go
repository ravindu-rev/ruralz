// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package lab

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// waitState waits until /proc shows the process in a state of states,
// where /proc tells: a signal takes effect asynchronously.
func waitState(t *testing.T, pid int, states string) {
	t.Helper()
	stat := "/proc/" + strconv.Itoa(pid) + "/stat"
	if _, err := os.Stat(stat); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second) // a hang guard only
	defer cancel()
	for {
		b, err := os.ReadFile(stat) //nolint:gosec // G304: the /proc file of the test's child
		if err != nil {
			t.Fatal(err)
		}
		if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) && strings.IndexByte(states, b[i+2]) >= 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("process %d never showed a state of %q: %s", pid, states, b)
		case <-time.After(time.Millisecond):
		}
	}
}

// TestChildSignals: the run service delivers SIGSTOP and SIGCONT to its
// child, freezing and resuming it, and SIGKILL, after which wait answers
// the child's end by that signal and Close reports the end as expected;
// an unknown signal and a signal to an exited child are refused.
func TestChildSignals(t *testing.T) {
	c, err := StartChild(t.Context(), helperChild(t, freeAddr(t), &syncBuffer{}))
	if err != nil {
		t.Fatal(err)
	}
	pid := c.Ready().PID
	ctx := t.Context()
	if r := c.Handle(ctx, Request{Op: OpSignal, Name: "SIGSTOP"}); !r.OK {
		t.Fatalf("SIGSTOP: %+v", r)
	}
	waitState(t, pid, "Tt")
	if r := c.Handle(ctx, Request{Op: OpSignal, Name: "SIGCONT"}); !r.OK {
		t.Fatalf("SIGCONT: %+v", r)
	}
	waitState(t, pid, "RS")
	if r := c.Handle(ctx, Request{Op: OpSignal, Name: "SIGBOGUS"}); r.OK || !strings.Contains(r.Error, "not one the run service delivers") {
		t.Errorf("SIGBOGUS answered %+v", r)
	}
	if r := c.Handle(ctx, Request{Op: OpSignal, Name: "SIGKILL"}); !r.OK {
		t.Fatalf("SIGKILL: %+v", r)
	}
	r := c.Handle(ctx, Request{Op: OpWait})
	if !r.OK || r.Exit == nil || r.Exit.Signal != "SIGKILL" || r.Exit.Code != -1 {
		t.Fatalf("wait = %+v (exit %+v), want the end by SIGKILL", r, r.Exit)
	}
	if r := c.Handle(ctx, Request{Op: OpSignal, Name: "SIGCONT"}); r.OK || !strings.Contains(r.Error, "exited") {
		t.Errorf("a signal to the exited child answered %+v", r)
	}
	if r := c.Handle(ctx, Request{Op: OpSignal, Name: "SIGKILL"}); !r.OK {
		t.Errorf("killing the exited child answered %+v, want no error, as killing an exited process is none", r)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close after the delivered SIGKILL = %v, want the expected end", err)
	}
}

// TestChildCloseResumesAFrozenChild: a stop resumes a child left frozen,
// so it ends by the SIGTERM that follows rather than by SIGKILL after the
// grace period.
func TestChildCloseResumesAFrozenChild(t *testing.T) {
	c, err := StartChild(t.Context(), helperChild(t, freeAddr(t), &syncBuffer{}))
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Handle(t.Context(), Request{Op: OpSignal, Name: "SIGSTOP"}); !r.OK {
		t.Fatalf("SIGSTOP: %+v", r)
	}
	waitState(t, c.Ready().PID, "Tt")
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	if e := c.exit(); e.Signal != "SIGTERM" {
		t.Errorf("the frozen child ended by %+v, want SIGTERM", e)
	}
}

// fakeDocker is a docker whose kill subcommand records its arguments into
// $LAB_TEST_DOCKER_LOG and signals the process $LAB_TEST_CONTAINER_PID,
// standing in for the container's.
const fakeDocker = `#!/bin/sh
printf '%s\n' "$*" >>"$LAB_TEST_DOCKER_LOG"
[ "$1" = kill ] && [ "$2" = --signal ] && kill -s "${3#SIG}" "$LAB_TEST_CONTAINER_PID"
`

// TestChildContainerSignals: a child that runs a container takes its
// signals through docker kill on the container, which reach the
// container's process although the docker client passes neither SIGSTOP
// nor SIGKILL on, and its stop resumes and ends the container too.
func TestChildContainerSignals(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDocker), 0o700); err != nil { //nolint:gosec // G306: the fake must be executable by its owner
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "docker.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LAB_TEST_DOCKER_LOG", log)
	cfg := helperChild(t, freeAddr(t), &syncBuffer{})
	cfg.Container = "ruralz-chaos-primary"
	c, err := StartChild(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	pid := c.Ready().PID
	t.Setenv("LAB_TEST_CONTAINER_PID", strconv.Itoa(pid))
	if r := c.Handle(t.Context(), Request{Op: OpSignal, Name: "SIGSTOP"}); !r.OK {
		t.Fatalf("SIGSTOP: %+v", r)
	}
	waitState(t, pid, "Tt")
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	b, err := os.ReadFile(log) //nolint:gosec // G304: the fake's record in the test's directory
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"kill --signal SIGSTOP ruralz-chaos-primary",
		"kill --signal SIGCONT ruralz-chaos-primary",
		"kill --signal SIGTERM ruralz-chaos-primary",
	}
	if got := strings.Split(strings.TrimSpace(string(b)), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("docker ran %q, want %q", got, want)
	}
	if e := c.exit(); e.Signal != "SIGTERM" {
		t.Errorf("the container's process ended by %+v, want SIGTERM through docker kill", e)
	}
}
