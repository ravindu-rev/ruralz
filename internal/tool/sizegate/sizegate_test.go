// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// childEnv makes the test binary act as an idle process: it waits for
// SIGTERM and exits 0, standing in for a ruralzd with no Revision.
const childEnv = "SIZEGATE_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "idle" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		<-ctx.Done()
		stop()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// sparse creates a file of exactly size bytes without writing them.
func sparse(t *testing.T, size int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ruralzd")
	f, err := os.Create(p) //nolint:gosec // Test creates its own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSizeBoundary: a binary of exactly 160 MiB passes and one byte more
// fails (11 req 68; test plan item 2).
func TestSizeBoundary(t *testing.T) {
	for _, tc := range []struct {
		size     int64
		wantCode int
		wantOut  string
	}{
		{defaultMaxSize - 1, 0, "pass"},
		{defaultMaxSize, 0, "pass"},
		{defaultMaxSize + 1, 1, "FAIL"},
	} {
		p := sparse(t, tc.size)
		r, err := checkSize(p, defaultMaxSize)
		if err != nil {
			t.Fatal(err)
		}
		if r.failed() != (tc.wantCode == 1) {
			t.Errorf("checkSize(%d bytes).failed() = %v", tc.size, r.failed())
		}
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), clock.Real(), []string{"-binary", p}, &stdout, &stderr)
		if code != tc.wantCode {
			t.Errorf("size %d: exit %d, want %d\n%s%s", tc.size, code, tc.wantCode, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), tc.wantOut) || !strings.Contains(stdout.String(), "167772160 bytes") {
			t.Errorf("size %d: output %q", tc.size, stdout.String())
		}
	}
	if defaultMaxSize != 167772160 || defaultMaxRSS != 93323264 {
		t.Errorf("limits = %d, %d; want 167772160 and 93323264", defaultMaxSize, defaultMaxRSS)
	}
}

func TestCheckSizeErrors(t *testing.T) {
	if _, err := checkSize(filepath.Join(t.TempDir(), "absent"), 1); err == nil {
		t.Error("absent binary: want an error")
	}
	if _, err := checkSize(t.TempDir(), 1); err == nil {
		t.Error("directory: want an error")
	}
}

// TestPlatformOf reads GOOS/GOARCH from a Go binary's build information:
// the test binary itself.
func TestPlatformOf(t *testing.T) {
	got, err := platformOf(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := runtime.GOOS + "/" + runtime.GOARCH; got != want {
		t.Errorf("platformOf = %s, want %s", got, want)
	}
	if _, err := platformOf(sparse(t, 10)); err == nil {
		t.Error("not a Go binary: want an error")
	}
}

func writeStatus(t *testing.T, root string, pid, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, pid), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, pid, "status"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReadVmRSS parses a fake /proc tree (11 test plan item 2).
func TestReadVmRSS(t *testing.T) {
	root := t.TempDir()
	writeStatus(t, root, "4242", "Name:\truralzd\nVmPeak:\t  200000 kB\nVmRSS:\t   91136 kB\nThreads:\t9\n")
	writeStatus(t, root, "1", "Name:\tkthreadd\n")
	writeStatus(t, root, "2", "VmRSS:\t12 MB\n")
	writeStatus(t, root, "3", "VmRSS:\tx kB\n")
	got, err := readVMRSS(root, 4242)
	if err != nil {
		t.Fatal(err)
	}
	if got != 91136*1024 {
		t.Errorf("VmRSS = %d, want %d", got, 91136*1024)
	}
	for _, pid := range []int{1, 2, 3, 99} {
		if _, err := readVMRSS(root, pid); err == nil {
			t.Errorf("pid %d: want an error", pid)
		}
	}
}

// TestSampleIdleTiming drives the settle with a fake clock: nothing is read
// before settle-window, then one read per interval up to the settle end
// (11 req 68: 120 s settle, every second over the last 10 s; test plan
// item 2).
func TestSampleIdleTiming(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	clk := clocktest.New(start)
	var mu sync.Mutex
	var at []time.Duration
	read := func() (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, clk.Now().Sub(start))
		return int64(len(at)) * 1000, nil
	}
	type out struct {
		samples []int64
		err     error
	}
	res := make(chan out, 1)
	go func() {
		s, err := sampleIdle(context.Background(), clk, make(chan struct{}), read, defaultSettle, defaultWindow, defaultInterval)
		res <- out{s, err}
	}()
	for i := range 11 { // one settle-window wait, then ten interval waits
		waitPending(t, clk)
		mu.Lock()
		n := len(at)
		mu.Unlock()
		if i == 0 {
			if n != 0 {
				t.Fatalf("read before the settle-window wait ended")
			}
			clk.Advance(defaultSettle - defaultWindow - time.Nanosecond)
			if clk.Pending() != 1 {
				t.Fatal("the settle-window wait ended early")
			}
			clk.Advance(time.Nanosecond)
			continue
		}
		clk.Advance(defaultInterval)
	}
	o := <-res
	if o.err != nil {
		t.Fatal(o.err)
	}
	if len(o.samples) != 10 || o.samples[9] != 10000 {
		t.Fatalf("samples = %v", o.samples)
	}
	for i, d := range at {
		if want := 111*time.Second + time.Duration(i)*time.Second; d != want {
			t.Errorf("read %d at %v, want %v", i, d, want)
		}
	}
}

// waitPending waits until the sampler has armed its timer.
func waitPending(t *testing.T, clk *clocktest.Fake) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for clk.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the sampler never armed a timer")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSampleIdleStops(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	read := func() (int64, error) { return 1, nil }

	done := make(chan struct{})
	close(done)
	if _, err := sampleIdle(context.Background(), clk, done, read, time.Minute, time.Second, time.Second); !errors.Is(err, errExited) {
		t.Errorf("exited process: %v, want errExited", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sampleIdle(ctx, clk, make(chan struct{}), read, time.Minute, time.Second, time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v, want context.Canceled", err)
	}
	for _, p := range [][3]time.Duration{{time.Minute, time.Second, 0}, {time.Minute, time.Second, 2 * time.Second}, {time.Second, time.Minute, time.Second}} {
		if _, err := sampleIdle(context.Background(), clk, make(chan struct{}), read, p[0], p[1], p[2]); err == nil {
			t.Errorf("settle %v, window %v, interval %v: want an error", p[0], p[1], p[2])
		}
	}
	// A read error while the process runs is reported; after it exited,
	// errExited wins.
	boom := errors.New("boom")
	failing := func() (int64, error) { return 0, boom }
	if _, err := sampleIdle(context.Background(), clk, make(chan struct{}), failing, 0, 0, time.Nanosecond); err == nil {
		t.Error("window 0: want a parameter error")
	}
	res := make(chan error, 1)
	go func() {
		_, err := sampleIdle(context.Background(), clk, make(chan struct{}), failing, 2*time.Second, time.Second, time.Second)
		res <- err
	}()
	waitPending(t, clk)
	clk.Advance(time.Second)
	waitPending(t, clk)
	clk.Advance(time.Second)
	if err := <-res; !errors.Is(err, boom) {
		t.Errorf("read error: %v, want boom", err)
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 4}
	for _, s := range []string{"ab", "cd", "ef"} {
		if _, err := b.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if got := b.String(); got != "cdef" {
		t.Errorf("tail = %q, want cdef", got)
	}
}

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ruralzd")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { //nolint:gosec // The test script must be executable.
		t.Fatal(err)
	}
	return p
}

// TestIdleRSS runs the gate against real processes with a short settle:
// an idle process passes, a 1-byte limit fails, the override passes, the M0
// stub is skipped and a crash is a tool error (11 req 66, 68).
func TestIdleRSS(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("VmRSS needs /proc")
	}
	short := []string{"-settle", "300ms", "-window", "200ms", "-interval", "100ms"}
	stub := `echo "ruralzd 0.0.0-dev (commit unknown, flavor default): Ruralz Gateway is not implemented yet; it is Planned (M1)" >&2; exit 2`
	self := []string{"-binary", os.Args[0], "-idle-rss", "-env", childEnv + "=idle"}
	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
		wantOut  []string
	}{
		{"idle process passes", append(self, short...), 0, []string{"idle RSS of", "over 2 samples", "pass"}},
		{"limit exceeded fails", append(append(self, short...), "-max-rss", "1"), 1, []string{"FAIL"}},
		{"override passes", append(append(self, short...), "-max-rss", "1", "-override"), 0, []string{"FAIL", "failures accepted"}},
		{"M0 stub is skipped", append([]string{"-idle-rss", "-rss-binary", script(t, stub)}, short...), 0, []string{"skip idle RSS", "not implemented yet"}},
		{"M0 stub skip is annotated", append([]string{"-idle-rss", "-annotate", "-rss-binary", script(t, stub)}, short...), 0, []string{"skip idle RSS", "::warning title=Idle RSS gate skipped::"}},
		{"other exit 2 is a tool error", append([]string{"-idle-rss", "-rss-binary", script(t, `echo "listener: not implemented yet" >&2; exit 2`)}, short...), 2, nil},
		{"stub message with another status is a tool error", append([]string{"-idle-rss", "-rss-binary", script(t, `echo "Ruralz Gateway is not implemented yet; it is Planned (M1)" >&2; exit 1`)}, short...), 2, nil},
		{"crash is a tool error", append([]string{"-idle-rss", "-rss-binary", script(t, "echo crashed >&2; exit 3")}, short...), 2, nil},
		{"no host binary is a tool error", []string{"-binary", sparse(t, 10), "-idle-rss"}, 2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary := filepath.Join(t.TempDir(), "summary.md")
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), clock.Real(), append(tc.args, "-summary", summary), &stdout, &stderr)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tc.wantCode, stdout.String(), stderr.String())
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
				}
			}
			if annotated := slices.Contains(tc.args, "-annotate"); annotated != strings.Contains(stdout.String(), "::warning") {
				t.Errorf("-annotate %v, but stdout:\n%s", annotated, stdout.String())
			}
			if code != 2 {
				md, err := os.ReadFile(summary) //nolint:gosec // Test reads its own temporary file.
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(md), "Idle RSS") {
					t.Errorf("summary lacks the idle RSS row:\n%s", md)
				}
			}
		})
	}
}

// TestNotImplemented: only the exact M0 stub message with exit status 2
// skips the idle RSS gate (11 req 68), never any early exit that happens to
// say "not implemented yet".
func TestNotImplemented(t *testing.T) {
	for _, tc := range []struct {
		code   int
		output string
		want   bool
	}{
		{2, "ruralzd 1.0.0 (commit abc, flavor default): " + stubMessage + "\n", true},
		{2, stubMessage, true},
		{1, stubMessage, false},
		{0, stubMessage, false},
		{2, "ruralzd: listener not implemented yet\n", false},
		{2, "ruralz gateway is not implemented yet; it is planned (m1)", false},
		{2, "", false},
	} {
		if got := notImplemented(tc.code, tc.output); got != tc.want {
			t.Errorf("notImplemented(%d, %q) = %v, want %v", tc.code, tc.output, got, tc.want)
		}
	}
}

func TestEscapeWorkflowData(t *testing.T) {
	for in, want := range map[string]string{
		"plain":              "plain",
		"50% done":           "50%25 done",
		"a\nb\r\n::error::x": "a%0Ab%0D%0A::error::x",
	} {
		if got := escapeWorkflowData(in); got != want {
			t.Errorf("escapeWorkflowData(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"-nope"},
		{"-binary", "x", "extra"},
		{"-binary", "x", "-max-size", "0"},
		{"-binary", "x", "-env", "NOEQUALS"},
		{"-binary", filepath.Join(t.TempDir(), "absent")},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), clock.Real(), args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%q) = %d, want 2", args, code)
		}
	}
}
