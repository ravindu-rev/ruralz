// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bytes"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for the child's two output streams.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// helperChild is the ChildConfig of TestHelperListener on addr.
func helperChild(t *testing.T, addr string, out *syncBuffer) ChildConfig {
	t.Helper()
	t.Setenv("LAB_TEST_LISTEN", addr)
	return ChildConfig{
		Argv: []string{os.Args[0], "-test.run", "^TestHelperListener$"}, ReadyAddr: addr, ReadyLine: `helper: listening`,
		ReadyTimeout: 30 * time.Second, Stderr: out,
	}
}

// freeAddr returns a loopback address free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestChildReadyLine: with a ready line the child is ready once it wrote
// it and its address accepts, its output passing on; a child whose port
// another process holds exits before writing the line and fails to start,
// although its address accepts the other process's connections.
func TestChildReadyLine(t *testing.T) {
	var out syncBuffer
	c, err := StartChild(t.Context(), helperChild(t, freeAddr(t), &out))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "helper: listening") {
		t.Errorf("output = %q, want the child's line passed on", out.String())
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}

	taken, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	var lost syncBuffer
	_, err = StartChild(t.Context(), helperChild(t, taken.Addr().String(), &lost))
	if err == nil || !strings.Contains(err.Error(), "exited before writing") {
		t.Errorf("StartChild on a taken port = %v, want the child's exit before its ready line", err)
	}
	if !strings.Contains(lost.String(), "address already in use") && !strings.Contains(lost.String(), "Only one usage") {
		t.Errorf("output = %q, want the bind failure", lost.String())
	}
	cfg := helperChild(t, freeAddr(t), &syncBuffer{})
	cfg.ReadyLine = "("
	if _, err := StartChild(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "ready line") {
		t.Errorf("StartChild with a bad ready line = %v", err)
	}
}

// writerFunc is an io.Writer of a function.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// TestLineWatchPassesTheReadyLineOnFirst: matched closes only once the
// output holding the ready line reached dst, so StartChild returns with
// the child's line passed on.
func TestLineWatchPassesTheReadyLineOnFirst(t *testing.T) {
	w := &lineWatch{re: regexp.MustCompile(`^ready`), matched: make(chan struct{})}
	var got strings.Builder
	w.dst = writerFunc(func(b []byte) (int, error) {
		select {
		case <-w.matched:
			if !strings.Contains(got.String(), "ready on :80\n") {
				t.Errorf("matched closed before the ready line reached dst, which holds %q and gets %q", got.String(), b)
			}
		default:
		}
		return got.Write(b)
	})
	for _, chunk := range []string{"starting\nrea", "dy on :80\n", "serving\n"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	select {
	case <-w.matched:
	default:
		t.Fatal("matched still open after the ready line")
	}
	if want := "starting\nready on :80\nserving\n"; got.String() != want {
		t.Errorf("dst holds %q, want %q", got.String(), want)
	}
}

// TestSignalNames: the run service delivers the signals it names and no
// others, and names them back.
func TestSignalNames(t *testing.T) {
	for name := range signals() {
		sig, ok := signalNamed(name)
		if !ok {
			t.Fatalf("signalNamed(%s) unknown", name)
		}
		if back, ok := SignalName(sig); !ok || back != name {
			t.Errorf("SignalName(%v) = %q, %v; want %s", sig, back, ok, name)
		}
	}
	if _, ok := signalNamed("SIGBOGUS"); ok {
		t.Error("signalNamed(SIGBOGUS) known")
	}
	for name, want := range map[string]bool{"SIGKILL": true, "SIGTERM": true, "SIGSTOP": false, "SIGCONT": false} {
		if terminating(name) != want {
			t.Errorf("terminating(%s) = %v, want %v", name, !want, want)
		}
	}
}
