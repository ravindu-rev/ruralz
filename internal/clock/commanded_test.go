// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for the Commanded clock: the command protocol of the
// controllable-clock build (docs/engineering/03-testing-and-quality-strategy.md
// "Hermetic rule scope"), its answers, refusals and stream end, and the
// timer semantics it shares with Real and clocktest.Fake.

var _ Clock = (*Commanded)(nil)

func epoch() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) }

// commander drives a Commanded clock over an in-memory stream, the way the
// process harness drives a Node over its socket pair.
type commander struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
	c    *Commanded
}

func newCommander(t *testing.T, start time.Time) *commander {
	t.Helper()
	harness, node := net.Pipe()
	c := NewCommanded(node, start)
	t.Cleanup(func() {
		_ = harness.Close()
		_ = c.Close()
	})
	return &commander{t: t, conn: harness, r: bufio.NewReader(harness), c: c}
}

// send writes one command line and reads its answer; the 10 s deadline
// only guards against a hang.
func (h *commander) send(line string) string {
	h.t.Helper()
	_ = h.conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(h.conn, line+"\n"); err != nil {
		h.t.Fatalf("write %q: %v", line, err)
	}
	answer, err := h.r.ReadString('\n')
	if err != nil {
		h.t.Fatalf("answer to %q: %v", line, err)
	}
	return strings.TrimSuffix(answer, "\n")
}

// at parses an answer as a time, failing the test on an error answer.
func (h *commander) at(line string) time.Time {
	h.t.Helper()
	answer := h.send(line)
	got, err := time.Parse(time.RFC3339Nano, answer)
	if err != nil {
		h.t.Fatalf("answer to %q = %q, want a time: %v", line, answer, err)
	}
	return got
}

// waitFor polls cond with a real-time bound; it only waits for goroutines
// the test started, never for the clock.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCommandedNowAndAdvance(t *testing.T) {
	h := newCommander(t, epoch())
	if got := h.at("now"); !got.Equal(epoch()) {
		t.Fatalf("now = %v, want the start %v", got, epoch())
	}
	if got := h.at("advance 1.5s"); !got.Equal(epoch().Add(1500 * time.Millisecond)) {
		t.Fatalf("advance 1.5s = %v, want start+1.5s", got)
	}
	if got := h.at("advance 0s"); !got.Equal(epoch().Add(1500 * time.Millisecond)) {
		t.Fatalf("advance 0s = %v, want the time unchanged", got)
	}
	if got := h.at("advance 2h3m"); !got.Equal(epoch().Add(2*time.Hour + 3*time.Minute + 1500*time.Millisecond)) {
		t.Fatalf("advance 2h3m = %v", got)
	}
	if got := h.c.Since(epoch()); got != 2*time.Hour+3*time.Minute+1500*time.Millisecond {
		t.Fatalf("Since(start) = %v", got)
	}
	if got := h.at("now"); !got.Equal(h.c.Now()) {
		t.Fatalf("now = %v, Now() = %v", got, h.c.Now())
	}
}

// Answers are RFC 3339 in UTC with nine fraction digits, whatever the
// start's location; CRLF line ends and surrounding blanks are accepted.
func TestCommandedAnswerLayout(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*3600)
	start := time.Date(2026, 10, 30, 17, 0, 0, 7, loc)
	h := newCommander(t, start)
	if got, want := h.send("now"), "2026-10-30T12:00:00.000000007Z"; got != want {
		t.Fatalf("now = %q, want %q", got, want)
	}
	if got, want := h.send("  advance   1ns  \r"), "2026-10-30T12:00:00.000000008Z"; got != want {
		t.Fatalf("advance 1ns = %q, want %q", got, want)
	}
	if got, want := h.send("now\r"), "2026-10-30T12:00:00.000000008Z"; got != want {
		t.Fatalf("now with CRLF = %q, want %q", got, want)
	}
}

// The clock keeps no monotonic reading, so its times compare by wall clock
// with times from other clocks.
func TestCommandedStripsMonotonicReading(t *testing.T) {
	c := NewCommanded(newIdleConn(), time.Now())
	defer func() { _ = c.Close() }()
	if s := c.Now().String(); strings.Contains(s, "m=") {
		t.Fatalf("Now() = %s, want no monotonic reading", s)
	}
}

func TestCommandedTimersFireInDeadlineOrder(t *testing.T) {
	h := newCommander(t, epoch())
	var mu sync.Mutex
	var order []string
	add := func(name string, d time.Duration) {
		h.c.AfterFunc(d, func() {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			if got, want := h.c.Now(), epoch().Add(d); !got.Equal(want) {
				t.Errorf("%s ran at %v, want its deadline %v", name, got, want)
			}
		})
	}
	add("c", 3*time.Second)
	add("a", time.Second)
	add("b", 2*time.Second)
	add("d1", 4*time.Second)
	add("d2", 4*time.Second)
	add("d3", 4*time.Second)
	add("late", time.Minute)
	if got := h.at("advance 4s"); !got.Equal(epoch().Add(4 * time.Second)) {
		t.Fatalf("advance 4s = %v", got)
	}
	// The callbacks ran on the command goroutine before the answer.
	mu.Lock()
	got := strings.Join(order, ",")
	mu.Unlock()
	if want := "a,b,c,d1,d2,d3"; got != want {
		t.Fatalf("fired %s, want %s (equal deadlines in arming order)", got, want)
	}
	if n := h.c.Pending(); n != 1 {
		t.Fatalf("Pending = %d, want 1 (the late timer)", n)
	}
}

// A timer armed by a callback fires in the same advance when it is due by
// the new time, at its own deadline; one due later stays armed.
func TestCommandedTimerArmedDuringAdvance(t *testing.T) {
	h := newCommander(t, epoch())
	var fired atomic.Int32
	var nestedAt atomic.Pointer[time.Time]
	h.c.AfterFunc(time.Second, func() {
		h.c.AfterFunc(2*time.Second, func() {
			now := h.c.Now()
			nestedAt.Store(&now)
			fired.Add(1)
		})
		h.c.AfterFunc(time.Hour, func() { fired.Add(100) })
	})
	h.at("advance 5s")
	if got := fired.Load(); got != 1 {
		t.Fatalf("nested callbacks fired %d, want only the one due within the advance", got)
	}
	if at := nestedAt.Load(); at == nil || !at.Equal(epoch().Add(3*time.Second)) {
		t.Fatalf("nested callback saw %v, want start+3s", at)
	}
	if n := h.c.Pending(); n != 1 {
		t.Fatalf("Pending = %d, want 1", n)
	}
}

func TestCommandedChannelTimer(t *testing.T) {
	h := newCommander(t, epoch())
	tm := h.c.NewTimer(10 * time.Second)
	h.at("advance 9s")
	select {
	case at := <-tm.C():
		t.Fatalf("timer fired early at %v", at)
	default:
	}
	h.at("advance 1s")
	select {
	case at := <-tm.C():
		if !at.Equal(epoch().Add(10 * time.Second)) {
			t.Fatalf("timer delivered %v, want start+10s", at)
		}
	default:
		t.Fatal("timer did not fire by its deadline")
	}
}

func TestCommandedStopAndReset(t *testing.T) {
	h := newCommander(t, epoch())
	tm := h.c.NewTimer(time.Minute)
	if !tm.Stop() {
		t.Error("Stop of an armed timer reported inactive")
	}
	if tm.Stop() {
		t.Error("second Stop reported active")
	}
	if tm.Reset(time.Second) {
		t.Error("Reset of a stopped timer reported active")
	}
	if !tm.Reset(2 * time.Second) {
		t.Error("Reset of an armed timer reported inactive")
	}
	h.at("advance 1s")
	select {
	case <-tm.C():
		t.Fatal("reset timer fired at its old deadline")
	default:
	}
	h.at("advance 1s")
	select {
	case at := <-tm.C():
		if !at.Equal(epoch().Add(2 * time.Second)) {
			t.Fatalf("reset timer delivered %v", at)
		}
	default:
		t.Fatal("reset timer did not fire")
	}
	if tm.Reset(time.Second) {
		t.Error("Reset of a fired timer reported active")
	}
	var ran atomic.Bool
	f := h.c.AfterFunc(time.Second, func() { ran.Store(true) })
	if !f.Stop() {
		t.Error("Stop of a pending AfterFunc reported inactive")
	}
	h.at("advance 1h")
	if ran.Load() {
		t.Error("a stopped AfterFunc ran")
	}
}

// Like Real, a zero or negative duration fires at once, without a command.
func TestCommandedZeroDurationsFireAtOnce(t *testing.T) {
	c := NewCommanded(newIdleConn(), epoch())
	defer func() { _ = c.Close() }()
	select {
	case at := <-c.NewTimer(0).C():
		if !at.Equal(epoch()) {
			t.Errorf("NewTimer(0) delivered %v", at)
		}
	default:
		t.Error("NewTimer(0) channel empty")
	}
	select {
	case <-c.NewTimer(-time.Second).C():
	default:
		t.Error("NewTimer(-1s) channel empty")
	}
	var ran atomic.Bool
	c.AfterFunc(-time.Second, func() { ran.Store(true) })
	waitFor(t, "AfterFunc(-1s) to run", ran.Load)
	if err := c.Sleep(t.Context(), 0); err != nil {
		t.Errorf("Sleep(0) = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Sleep(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep(0) on a canceled context = %v", err)
	}
	if n := c.Pending(); n != 0 {
		t.Errorf("Pending = %d, want 0", n)
	}
}

func TestCommandedSleep(t *testing.T) {
	h := newCommander(t, epoch())
	done := make(chan error, 1)
	go func() { done <- h.c.Sleep(t.Context(), 5*time.Second) }()
	waitFor(t, "Sleep to arm its timer", func() bool { return h.c.Pending() == 1 })
	h.at("advance 4s")
	select {
	case err := <-done:
		t.Fatalf("Sleep returned %v before its duration", err)
	default:
	}
	h.at("advance 1s")
	if err := <-done; err != nil {
		t.Fatalf("Sleep = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() { done <- h.c.Sleep(ctx, time.Hour) }()
	waitFor(t, "Sleep to arm its timer", func() bool { return h.c.Pending() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Sleep = %v", err)
	}
	waitFor(t, "the canceled Sleep to stop its timer", func() bool { return h.c.Pending() == 0 })
}

// A refused command gets an error answer and leaves the clock and its
// timers as they were; the stream stays usable.
func TestCommandedRefusesBadCommands(t *testing.T) {
	tests := []struct {
		name, line, want string
	}{
		{"empty line", "", "unknown command"},
		{"unknown command", "rewind 1s", "unknown command"},
		{"advance without duration", "advance", "unknown command"},
		{"advance with two durations", "advance 1s 2s", "unknown command"},
		{"now with an argument", "now please", "unknown command"},
		{"malformed duration", "advance soon", "invalid duration"},
		{"negative duration", "advance -1s", "negative duration"},
		{"overlong line", "advance " + strings.Repeat("1", MaxCommandBytes), "longer than"},
	}
	h := newCommander(t, epoch())
	var fired atomic.Bool
	h.c.AfterFunc(time.Nanosecond, func() { fired.Store(true) })
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := h.send(tt.line)
			if !strings.HasPrefix(got, AnswerError+" ") || !strings.Contains(got, tt.want) {
				t.Errorf("answer to %q = %q, want %q... naming %q", tt.line, got, AnswerError, tt.want)
			}
			if now := h.at("now"); !now.Equal(epoch()) {
				t.Errorf("after %q the clock reads %v, want it unmoved", tt.line, now)
			}
		})
	}
	if fired.Load() {
		t.Error("a refused command fired a timer")
	}
	h.at("advance 1ns")
	if !fired.Load() {
		t.Error("the stream stopped serving after refusals")
	}
}

// When the stream ends the reader ends with io.EOF, and the clock keeps
// its time and timers, which nothing fires any more.
func TestCommandedStreamEnd(t *testing.T) {
	h := newCommander(t, epoch())
	h.c.NewTimer(time.Second)
	h.at("advance 500ms")
	if err := h.c.Err(); err != nil {
		t.Fatalf("Err before the end = %v", err)
	}
	_ = h.conn.Close()
	select {
	case <-h.c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("reader did not end with the stream")
	}
	if err := h.c.Err(); !errors.Is(err, io.EOF) {
		t.Fatalf("Err = %v, want io.EOF", err)
	}
	if got := h.c.Now(); !got.Equal(epoch().Add(500 * time.Millisecond)) {
		t.Fatalf("Now after the end = %v", got)
	}
	if n := h.c.Pending(); n != 1 {
		t.Fatalf("Pending after the end = %d, want 1", n)
	}
}

func TestCommandedClose(t *testing.T) {
	harness, node := net.Pipe()
	defer func() { _ = harness.Close() }()
	c := NewCommanded(node, epoch())
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Close returned before the reader ended")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if c.Err() == nil {
		t.Fatal("Err after Close = nil, want the read failure")
	}
}

// Arming, stopping and resetting timers on many goroutines while the clock
// advances is race-free and loses no timer.
func TestCommandedConcurrentUse(t *testing.T) {
	h := newCommander(t, epoch())
	const workers, perWorker = 8, 50
	var fired atomic.Int64
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range perWorker {
				d := time.Duration(1+(w*perWorker+i)%7) * time.Millisecond
				tm := h.c.AfterFunc(d, func() { fired.Add(1) })
				switch i % 3 {
				case 0:
					tm.Stop()
				case 1:
					tm.Reset(d)
				}
				_ = h.c.Now()
			}
		})
	}
	for range 20 {
		h.at("advance 1ms")
	}
	wg.Wait()
	h.at("advance 1h")
	// Every timer not stopped fires exactly once; a stopped one fires only
	// when an advance reached it before its Stop.
	stopped := int64(workers * ((perWorker + 2) / 3))
	if got, most := fired.Load(), int64(workers*perWorker); got > most || got < most-stopped {
		t.Fatalf("fired %d callbacks, want between %d and %d", got, most-stopped, most)
	}
	if n := h.c.Pending(); n != 0 {
		t.Fatalf("Pending = %d after advancing past every deadline", n)
	}
}

// idleConn is a stream that sends no command: reads block until Close.
type idleConn struct {
	closed chan struct{}
	once   sync.Once
}

func newIdleConn() *idleConn { return &idleConn{closed: make(chan struct{})} }

func (c *idleConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *idleConn) Write(b []byte) (int, error) { return len(b), nil }

func (c *idleConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}
