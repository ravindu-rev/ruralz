// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dev

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// testVersion is the CLI build version of the tests.
const testVersion = "0.4.0"

// Test tokens: RFC 6750 b64token, at least 22 bytes; canaries, not
// credentials.
//
//nolint:gosec // G101: test canaries, not credentials.
const (
	testToken    = "op-canary-4fQ9x2LmZ7rT1vB8nK3sW6yH"
	rotatedToken = "op-canary-rotated-8Hs2Lq9Zp4Xw7Rt1"
)

func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// syncBuffer is a bytes.Buffer safe for the command's goroutine and the
// test's.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// invocation is one command run in a goroutine.
type invocation struct {
	stdout, stderr *syncBuffer
	done           chan int
	cancel         context.CancelCauseFunc
}

// start runs ruralz args... over Specs(o) with environ in a goroutine; the
// test ends it with interrupt or waits for it with wait.
func start(t *testing.T, o Options, environ []string, args ...string) *invocation {
	t.Helper()
	ctx, cancel := context.WithCancelCause(context.Background())
	inv := &invocation{stdout: &syncBuffer{}, stderr: &syncBuffer{}, done: make(chan int, 1), cancel: cancel}
	sio := command.NewIO(inv.stdout, inv.stderr, environ, epoch, "linux")
	go func() { inv.done <- command.Execute(ctx, sio, "ruralz", Specs(o), args) }()
	t.Cleanup(func() {
		cancel(nil)
		select {
		case <-inv.done:
		case <-time.After(30 * time.Second):
			t.Error("the command did not end")
		}
	})
	return inv
}

// interrupt cancels the invocation's context as a first signal does.
func (inv *invocation) interrupt(s os.Signal) { inv.cancel(command.Interrupted{Signal: s}) }

// wait returns the exit code, under a real-time deadline.
func (inv *invocation) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-inv.done:
		inv.done <- code
		return code
	case <-time.After(30 * time.Second):
		t.Fatalf("the command did not end\n--- stdout ---\n%s\n--- stderr ---\n%s", inv.stdout, inv.stderr)
		return 0
	}
}

// result is what one finished command returned.
type result struct {
	code           int
	stdout, stderr string
}

// execute runs ruralz args... to its end.
func execute(t *testing.T, o Options, environ []string, args ...string) result {
	t.Helper()
	inv := start(t, o, environ, args...)
	code := inv.wait(t)
	return result{code: code, stdout: inv.stdout.String(), stderr: inv.stderr.String()}
}

// want fails unless r has the exit code, standard output and standard
// error given.
func (r result) want(t *testing.T, what string, code int, stdout, stderr string) {
	t.Helper()
	if r.code != code || r.stdout != stdout || r.stderr != stderr {
		t.Errorf("%s = exit %d\n--- stdout ---\n%s\n--- stderr ---\n%s\nwant exit %d\n--- stdout ---\n%s\n--- stderr ---\n%s",
			what, r.code, r.stdout, r.stderr, code, stdout, stderr)
	}
}

// waitFor polls cond under a real-time deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// writeFile writes content to dir/name with mode 0600 and returns the
// path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// tokenFile writes a token file holding tok and a newline.
func tokenFile(t *testing.T, tok string) string {
	t.Helper()
	return writeFile(t, t.TempDir(), "token", tok+"\n")
}

// countingRand yields 1, 2, 3, ... so every traceparent and token differs.
type countingRand struct {
	mu sync.Mutex
	n  byte
}

func (r *countingRand) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range p {
		r.n++
		p[i] = r.n
	}
	return len(p), nil
}

// testOptions are the dev commands' options on a fake clock, with no
// terminal.
func testOptions(clk *clocktest.Fake) Options {
	return Options{
		Clock: clk, Rand: &countingRand{}, Version: testVersion,
		terminal: func(io.Writer) bool { return false },
	}
}

// lines splits output into lines without the last newline.
func lines(s string) []string { return strings.Split(strings.TrimSuffix(s, "\n"), "\n") }
