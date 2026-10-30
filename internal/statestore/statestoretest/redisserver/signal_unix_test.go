// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package redisserver

import (
	"context"
	"os"
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
