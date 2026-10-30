// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package binaries

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// The controllable-clock build exits 2 without its command socket, and
// with it runs on the commanded clock from the start the harness sends
// first: a 10 s timer fires only once the harness advances the clock past
// it.
func TestTestClockBuildFollowsCommands(t *testing.T) {
	t.Setenv(EnvBinDir, "")
	set, err := Build(t.Context(), clockFixture(t), filepath.Join(t.TempDir(), "bin"), BuildOptions{TestClock: true, Commands: []string{Ruralzd}})
	if err != nil {
		t.Fatal(err)
	}

	code, out := exitCode(t, set.Ruralzd)
	if code != 2 || !strings.Contains(out, "no clock command socket") {
		t.Fatalf("started without descriptor 3: exit %d, output %q; want exit 2 naming the missing socket", code, out)
	}

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	harnessFile := os.NewFile(uintptr(fds[0]), "harness")
	nodeFile := os.NewFile(uintptr(fds[1]), "node")
	conn, err := net.FileConn(harnessFile)
	_ = harnessFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var stdout lockedBuffer
	cmd := exec.CommandContext(t.Context(), set.Ruralzd) //nolint:gosec // G204: a binary this test built
	cmd.ExtraFiles = []*os.File{nodeFile}
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = nodeFile.Close()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	_ = conn.SetDeadline(time.Now().Add(30 * time.Second)) // a hang guard only
	r := bufio.NewReader(conn)
	ask := func(line string) time.Time {
		t.Helper()
		if _, err := io.WriteString(conn, line+"\n"); err != nil {
			t.Fatal(err)
		}
		answer, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("answer to %q: %v (output %q)", line, err, stdout.String())
		}
		at, err := time.Parse(time.RFC3339Nano, strings.TrimSuffix(answer, "\n"))
		if err != nil {
			t.Fatalf("answer to %q = %q: %v", line, answer, err)
		}
		return at
	}
	start := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if got := ask(clock.CommandStart + " " + start.Format(clock.CommandTimeLayout)); !got.Equal(start) {
		t.Fatalf("start answered %v, want %v", got, start)
	}
	if got := ask("now"); !got.Equal(start) {
		t.Fatalf("now = %v, want the start %v", got, start)
	}
	if got := ask("advance 9s"); got.Sub(start) != 9*time.Second {
		t.Fatalf("advance 9s moved the clock by %v", got.Sub(start))
	}
	select {
	case err := <-waited:
		t.Fatalf("the probe exited (%v) before its timer was due: %q", err, stdout.String())
	default:
	}
	if got := ask("advance 1s"); got.Sub(start) != 10*time.Second {
		t.Fatalf("advance 1s moved the clock to %v after the start", got.Sub(start))
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("probe: %v\n%s", err, stdout.String())
		}
	case <-time.After(30 * time.Second): // a hang guard only
		t.Fatalf("the probe did not exit once its timer fired: %q", stdout.String())
	}
	if got, want := stdout.String(), "testclock true\nfired after 10s\n"; got != want {
		t.Fatalf("probe output = %q, want %q", got, want)
	}
}

// lockedBuffer is a bytes.Buffer safe for the copying goroutines of
// exec.Cmd and the test's reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
