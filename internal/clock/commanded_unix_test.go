// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package clock

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// socketPair returns the harness end as a net.Conn and the Node end as the
// descriptor a child would find at CommandFD.
func socketPair(t *testing.T) (net.Conn, uintptr) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	harness := os.NewFile(uintptr(fds[0]), "harness")
	conn, err := net.FileConn(harness)
	_ = harness.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, uintptr(fds[1])
}

// harnessStart is the time the tests' harness starts a process clock at,
// far from the real time.
func harnessStart() time.Time { return time.Date(2001, 2, 3, 4, 5, 6, 789, time.UTC) }

// answerOf reads one answer line from r.
func answerOf(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	answer, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(answer, "\n")
}

// OpenCommanded serves the commands of a socket descriptor, as a
// controllable-clock process serves CommandFD, from the start its first
// command sets.
func TestOpenCommandedServesASocket(t *testing.T) {
	conn, fd := socketPair(t)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second)) // a hang guard only
	if _, err := io.WriteString(conn, CommandStart+" "+harnessStart().Format(CommandTimeLayout)+"\n"); err != nil {
		t.Fatal(err)
	}
	c, err := OpenCommanded(fd)
	if err != nil {
		t.Fatalf("OpenCommanded(socket) = %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	r := bufio.NewReader(conn)
	if got := answerOf(t, r); got != harnessStart().Format(CommandTimeLayout) {
		t.Fatalf("answer to start = %q, want the start", got)
	}
	ask := func(line string) string {
		t.Helper()
		if _, err := io.WriteString(conn, line+"\n"); err != nil {
			t.Fatal(err)
		}
		return answerOf(t, r)
	}
	at := func(line string) time.Time {
		t.Helper()
		answer := ask(line)
		got, err := time.Parse(time.RFC3339Nano, answer)
		if err != nil {
			t.Fatalf("answer to %q = %q: %v", line, answer, err)
		}
		return got
	}
	if got := at("now"); !got.Equal(harnessStart()) {
		t.Fatalf("now = %v, want the start %v", got, harnessStart())
	}
	if got := at("advance 90s"); got.Sub(harnessStart()) != 90*time.Second {
		t.Fatalf("advance 90s moved the clock by %v", got.Sub(harnessStart()))
	}
	if got := ask(CommandStart + " " + harnessStart().Format(CommandTimeLayout)); !strings.HasPrefix(got, AnswerError+" ") {
		t.Errorf("a second start = %q, want it refused", got)
	}
}

// OpenCommanded returns only once the harness's start arrived, so the
// process's first reading is the harness's time even when the harness
// sends start after the process opened its socket: a process never reads
// a time the harness's clock has passed, nor arms a timer from one.
func TestOpenCommandedWaitsForStart(t *testing.T) {
	conn, fd := socketPair(t)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second)) // a hang guard only
	type opened struct {
		c     *Commanded
		first time.Time
		err   error
	}
	done := make(chan opened, 1)
	go func() {
		c, err := OpenCommanded(fd)
		var first time.Time
		if err == nil {
			first = c.Now()
		}
		done <- opened{c, first, err}
	}()
	if _, err := io.WriteString(conn, CommandStart+" "+harnessStart().Format(CommandTimeLayout)+"\n"); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("OpenCommanded = %v", got.err)
	}
	t.Cleanup(func() { _ = got.c.Close() })
	if !got.first.Equal(harnessStart()) {
		t.Errorf("the process's first reading = %v, want the harness's start %v", got.first, harnessStart())
	}
	if answer := answerOf(t, bufio.NewReader(conn)); answer != harnessStart().Format(CommandTimeLayout) {
		t.Errorf("answer to start = %q", answer)
	}
}

// A stream that ends, or begins with anything but start, starts no
// clock: OpenCommanded fails with ErrNoStart, answering a refused first
// command with an error line, and the process exits.
func TestOpenCommandedNeedsStart(t *testing.T) {
	tests := []struct {
		name, first, answer string
	}{
		{"stream ends", "", ""},
		{"now first", "now", "want \"start <time>\""},
		{"advance first", "advance 1s", "want \"start <time>\""},
		{"start without a time", "start", "want \"start <time>\""},
		{"malformed time", "start yesterday", "start time"},
		{"overlong line", "start " + strings.Repeat("1", MaxCommandBytes), "longer than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, fd := socketPair(t)
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second)) // a hang guard only
			if tt.first == "" {
				_ = conn.(*net.UnixConn).CloseWrite()
			} else if _, err := io.WriteString(conn, tt.first+"\n"); err != nil {
				t.Fatal(err)
			}
			c, err := OpenCommanded(fd)
			if !errors.Is(err, ErrNoStart) {
				if c != nil {
					_ = c.Close()
				}
				t.Fatalf("OpenCommanded after %q = %v, want ErrNoStart", tt.first, err)
			}
			if tt.answer == "" {
				return
			}
			if got := answerOf(t, bufio.NewReader(conn)); !strings.HasPrefix(got, AnswerError+" ") || !strings.Contains(got, tt.answer) {
				t.Errorf("answer to %q = %q, want %q... naming %q", tt.first, got, AnswerError, tt.answer)
			}
		})
	}
}

// OpenCommanded refuses a descriptor that is not open or not a socket, so
// a controllable-clock process started without its command socket exits
// 2 instead of reading another file.
func TestOpenCommandedRefusesOtherDescriptors(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "regular"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := OpenCommanded(f.Fd()); !errors.Is(err, ErrNoCommandDescriptor) {
		t.Errorf("OpenCommanded(regular file) = %v, want ErrNoCommandDescriptor", err)
	}
	// A refused descriptor stays its owner's: nothing took it over.
	if _, err := f.WriteString("still open"); err != nil {
		t.Errorf("write after a refused OpenCommanded = %v", err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = r.Close()
		_ = w.Close()
	}()
	if _, err := OpenCommanded(r.Fd()); !errors.Is(err, ErrNoCommandDescriptor) {
		t.Errorf("OpenCommanded(pipe) = %v, want ErrNoCommandDescriptor", err)
	}
	// A descriptor number above any open-file limit is never open.
	if _, err := OpenCommanded(1 << 30); !errors.Is(err, ErrNoCommandDescriptor) {
		t.Errorf("OpenCommanded(closed descriptor) = %v, want ErrNoCommandDescriptor", err)
	}
}
