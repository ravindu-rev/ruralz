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

// OpenCommanded serves the commands of a socket descriptor, as a
// controllable-clock process serves CommandFD.
func TestOpenCommandedServesASocket(t *testing.T) {
	conn, fd := socketPair(t)
	c, err := OpenCommanded(fd)
	if err != nil {
		t.Fatalf("OpenCommanded(socket) = %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second)) // a hang guard only
	r := bufio.NewReader(conn)
	ask := func(line string) time.Time {
		t.Helper()
		if _, err := io.WriteString(conn, line+"\n"); err != nil {
			t.Fatal(err)
		}
		answer, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		got, err := time.Parse(time.RFC3339Nano, strings.TrimSuffix(answer, "\n"))
		if err != nil {
			t.Fatalf("answer to %q = %q: %v", line, answer, err)
		}
		return got
	}
	start := ask("now")
	if got := ask("advance 90s"); got.Sub(start) != 90*time.Second {
		t.Fatalf("advance 90s moved the clock by %v", got.Sub(start))
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
