// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package redisserver

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestSignalFreezesTheProcess checks Signal, the frozen State Store
// fault: SIGSTOP leaves the server's socket open but unanswered, SIGCONT
// resumes it, and a signal after the process exited fails.
func TestSignalFreezesTheProcess(t *testing.T) {
	d := startHelper(t, Process, Options{})
	s := d.Primary()
	mustDo(t, s, "PING")
	if err := s.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	pid := s.proc.(localProc).p.PID()
	s.mu.Unlock()
	waitStopped(t, pid)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if v, err := s.Do(ctx, "PING"); err == nil {
		t.Fatalf("PING to a stopped redis-server answered %+v", v)
	}
	if err := s.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if v := mustDo(t, s, "PING"); v.Str != "PONG" {
		t.Errorf("PING after SIGCONT = %+v, want PONG", v)
	}
	if err := s.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Signal(os.Interrupt); err == nil {
		t.Error("Signal to an exited redis-server succeeded")
	}
}

// TestSignalOnHost checks Signal on a server a Host runs: SIGSTOP and
// SIGCONT reach the server through its HostProcess, freezing and resuming
// it with its socket open, and a signal after Kill fails.
func TestSignalOnHost(t *testing.T) {
	d := StartOn(t, newLabHost(t), Process, Options{})
	s := d.Primary()
	if err := s.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	pid := s.proc.(hostProc).p.(*labHostProc).p.PID()
	s.mu.Unlock()
	waitStopped(t, pid)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if v, err := s.Do(ctx, "PING"); err == nil {
		t.Fatalf("PING to a stopped server on the host answered %+v", v)
	}
	if err := s.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	if v := mustDo(t, s, "PING"); v.Str != "PONG" {
		t.Errorf("PING after SIGCONT = %+v, want PONG", v)
	}
	if err := s.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Signal(syscall.SIGCONT); err == nil {
		t.Error("Signal to a killed server on the host succeeded")
	}
}

// waitStopped waits until the kernel shows the process pid stopped, where
// /proc tells it: SIGSTOP takes effect asynchronously, so a request sent
// the moment kill returns can still be answered.
func waitStopped(t *testing.T, pid int) {
	t.Helper()
	stat := "/proc/" + strconv.Itoa(pid) + "/stat"
	if _, err := os.Stat(stat); err != nil {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(stat) //nolint:gosec // G304: the /proc file of the process the test started.
		if err != nil {
			t.Fatal(err)
		}
		// The state follows the command, which ends with the last ')'.
		if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) && (b[i+2] == 'T' || b[i+2] == 't') {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("redis-server %d did not show stopped after SIGSTOP", pid)
}
