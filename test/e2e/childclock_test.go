// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// childClock drives the process clock of a controllable-clock ruralzd
// the suite does not start itself: the one ruralz dev run launches. The
// suite hands ruralz the clock command socket as file descriptor 3, which
// is not close-on-exec in ruralz, so the ruralzd it launches inherits it
// there (docs/engineering/03-testing-and-quality-strategy.md "Hermetic
// rule scope": the commands are lines on file descriptor 3, the first
// extra file, a socket). Its methods are safe for concurrent use.
type childClock struct {
	mu   sync.Mutex
	conn net.Conn
	r    *bufio.Reader
}

// newChildClock returns the suite's end of a new clock command socket and
// the file to hand ruralz as its first extra file; the file closes with
// the test.
func newChildClock(t testing.TB) (*childClock, *os.File) {
	t.Helper()
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fds[0]), "clock-suite")
	conn, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	child := os.NewFile(uintptr(fds[1]), "clock-ruralzd")
	t.Cleanup(func() { _ = conn.Close(); _ = child.Close() })
	return &childClock{conn: conn, r: bufio.NewReaderSize(conn, clock.MaxCommandBytes)}, child
}

// do sends one command and returns the time answered, waiting at most
// waitTimeout of real time.
func (c *childClock) do(line string) (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetDeadline(time.Now().Add(waitTimeout))
	if _, err := io.WriteString(c.conn, line+"\n"); err != nil {
		return time.Time{}, err
	}
	answer, err := c.r.ReadString('\n')
	if err != nil {
		return time.Time{}, err
	}
	answer = strings.TrimSuffix(answer, "\n")
	if strings.HasPrefix(answer, clock.AnswerError) {
		return time.Time{}, fmt.Errorf("%q refused: %s", line, answer)
	}
	return time.Parse(time.RFC3339Nano, answer)
}

// Now returns the child's clock reading, the real time when the child
// does not answer.
func (c *childClock) Now() time.Time {
	at, err := c.do(clock.CommandNow)
	if err != nil {
		return time.Now()
	}
	return at
}

// Advance moves the child's clock by d; once the child exited, it does
// nothing.
func (c *childClock) Advance(d time.Duration) error {
	_, err := c.do(clock.CommandAdvance + " " + d.String())
	if errors.Is(err, io.EOF) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return nil
	}
	return err
}

// advancingChild runs wait while it moves c by step every
// topology.DrainStepWait of real time, so the child's timers, such as its
// source poll and its Drain, fire while the suite waits.
func advancingChild(t testing.TB, c *childClock, step time.Duration, wait func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !wait() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v", waitTimeout)
		}
		if err := c.Advance(step); err != nil {
			t.Fatalf("advance the child's clock: %v", err)
		}
		time.Sleep(topology.DrainStepWait)
	}
}
