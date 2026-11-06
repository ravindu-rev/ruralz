// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// CommandFD is the file descriptor a controllable-clock process reads its
// clock commands on: the first extra file the process harness passes
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope"). It is one end of a stream socket pair, carrying commands in and
// answers out.
const CommandFD = 3

// CommandTimeLayout is the form of every time a Commanded clock answers
// with: RFC 3339 in UTC with all nine fraction digits, such as
// 2026-10-30T12:00:00.000000000Z. time.RFC3339Nano parses it.
const CommandTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// MaxCommandBytes bounds one command line, newline included; a longer line
// is answered with an error and skipped.
const MaxCommandBytes = 256

// Clock commands and the prefix of an error answer.
const (
	// CommandStart is "start <time>", the time in RFC 3339 form: the
	// first command of a process clock (OpenCommanded), which waits for
	// it and starts at that time, so a process started while the
	// harness's clock leads real time never reads a time before it; the
	// clock answers its start. Once the clock has started it refuses the
	// command.
	CommandStart = "start"
	// CommandAdvance is "advance <duration>", in Go duration syntax: the
	// clock moves forward by the duration and answers once every timer
	// due by the new time has fired.
	CommandAdvance = "advance"
	// CommandNow is "now": the clock answers its current time.
	CommandNow = "now"
	// AnswerError starts the answer to a command the clock refuses (an
	// unknown command, a malformed or negative duration, a line over
	// MaxCommandBytes): "error <reason>". The clock does not move.
	AnswerError = "error"
)

// ErrNoCommandDescriptor reports that the process was started without its
// clock command descriptor: CommandFD is not open, or is not a socket. A
// ruralz_testclock build of ruralzd exits 2 then.
var ErrNoCommandDescriptor = errors.New("clock: no clock command socket on file descriptor 3")

// ErrNoStart reports that a process clock's command stream ended, or
// began with another command, before its start (CommandStart).
var ErrNoStart = errors.New("clock: the command stream did not begin with start")

// Commanded is a Clock that starts at a given time and moves only on
// commands read from a stream, one line each, answered in order with one
// line each: the clock of the controllable-clock ruralzd build, which
// Process returns there, and of the process harness's stand-in Node.
//
// "advance <duration>" moves the clock forward and answers with the new
// time once every timer due by then has fired: timers fire in deadline
// order (equal deadlines in the order they were armed), each at its own
// deadline, which Now returns while it fires; a channel timer's value is
// sent and an AfterFunc callback runs to its end on the command goroutine
// before the next timer fires, and a timer armed meanwhile fires in the
// same advance when it is due by the new time. "now" answers the current
// time. "start <time>" begins the stream of a process clock, which
// OpenCommanded reads before it returns, and is refused afterwards.
// Answers use CommandTimeLayout; a refused command gets an AnswerError
// line and leaves the clock where it is. Like Real, a zero or negative
// timer duration fires at once: NewTimer's channel is already filled and
// AfterFunc runs its callback in a new goroutine.
//
// One goroutine, started by NewCommanded, reads the commands; it ends when
// the stream ends or fails, or at Close, and the clock then stays at its
// last time. Methods are safe for concurrent use.
type Commanded struct {
	conn io.ReadWriteCloser
	r    *bufio.Reader // conn's commands
	done chan struct{}
	err  error // why the reader ended; written before done closes

	mu     sync.Mutex
	now    time.Time
	timers timerHeap
	seq    uint64 // arming order, the tie-break of equal deadlines

	closeOnce sync.Once
}

// NewCommanded returns a Commanded clock reading start, without its
// monotonic reading, and starts the goroutine that serves the commands
// read from conn.
func NewCommanded(conn io.ReadWriteCloser, start time.Time) *Commanded {
	c := newCommanded(conn, bufio.NewReaderSize(conn, MaxCommandBytes), start)
	go c.serve()
	return c
}

// newCommanded returns a Commanded clock reading start whose commands r
// reads from conn, without serving them.
func newCommanded(conn io.ReadWriteCloser, r *bufio.Reader, start time.Time) *Commanded {
	return &Commanded{conn: conn, r: r, done: make(chan struct{}), now: start.Round(0)}
}

// OpenCommanded returns the Commanded clock of a controllable-clock
// process, which reads its commands on file descriptor fd, CommandFD
// there, and owns fd from then on. It waits for the stream's first
// command, "start <time>", and returns the clock reading that time once
// it answered it, so the process reads no time before the harness's
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope"); it fails with ErrNoStart, after an AnswerError line for a
// command other than start, when the stream ends or begins otherwise. It
// returns ErrNoCommandDescriptor, leaving fd untouched, when fd is not
// open or not a socket, so it never takes over a descriptor the Go
// runtime or the process opened for something else; on platforms without
// file descriptors (Windows) it always does.
func OpenCommanded(fd uintptr) (*Commanded, error) {
	f, err := openCommandFD(fd)
	if err != nil {
		return nil, err
	}
	c, err := startCommanded(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return c, nil
}

// startCommanded reads the first command of conn, which must be start,
// answers it and returns the clock it started, serving conn's later
// commands.
func startCommanded(conn io.ReadWriteCloser) (*Commanded, error) {
	r := bufio.NewReaderSize(conn, MaxCommandBytes)
	line, err := r.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		_, _ = fmt.Fprintf(conn, "%s command longer than %d bytes\n", AnswerError, MaxCommandBytes)
		return nil, fmt.Errorf("%w: its first line is longer than %d bytes", ErrNoStart, MaxCommandBytes)
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrNoStart, err)
	}
	start, err := parseStart(string(bytes.TrimRight(line, "\r\n")))
	if err != nil {
		_, _ = fmt.Fprintf(conn, "%s %v\n", AnswerError, err)
		return nil, fmt.Errorf("%w: %w", ErrNoStart, err)
	}
	c := newCommanded(conn, r, start)
	if _, err := io.WriteString(conn, c.Now().UTC().Format(CommandTimeLayout)+"\n"); err != nil {
		return nil, fmt.Errorf("clock: answer start: %w", err)
	}
	go c.serve()
	return c, nil
}

// parseStart returns the time of a "start <time>" line.
func parseStart(line string) (time.Time, error) {
	f := strings.Fields(line)
	if len(f) != 2 || f[0] != CommandStart {
		return time.Time{}, fmt.Errorf("first command %q: want %q", line, CommandStart+" <time>")
	}
	at, err := time.Parse(time.RFC3339Nano, f[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("start time %q: %w", f[1], err)
	}
	return at, nil
}

// Done is closed once the command reader has ended.
func (c *Commanded) Done() <-chan struct{} { return c.done }

// Err returns why the command reader ended (io.EOF when the stream
// ended), once Done is closed; nil before.
func (c *Commanded) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// Close closes the command stream and waits for the reader to end. The
// clock keeps its time and its timers, which no command fires any more.
func (c *Commanded) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.conn.Close() })
	<-c.done
	return err
}

// Now returns the clock's time.
func (c *Commanded) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Since returns the clock's time elapsed since t.
func (c *Commanded) Since(t time.Time) time.Duration { return c.Now().Sub(t) }

// NewTimer returns a timer whose channel receives when an advance passes
// d, or at once when d <= 0.
func (c *Commanded) NewTimer(d time.Duration) Timer {
	return c.arm(&commandedTimer{c: c, index: -1, ch: make(chan time.Time, 1)}, d)
}

// AfterFunc returns a timer that calls f when an advance passes d, on the
// command goroutine, or at once in a new goroutine when d <= 0.
func (c *Commanded) AfterFunc(d time.Duration, f func()) Timer {
	return c.arm(&commandedTimer{c: c, index: -1, fn: f}, d)
}

// NewTimerAt returns a timer whose channel receives when an advance
// reaches t, or at once when the clock already has: the deadline holds
// whatever advances came since the caller computed it.
func (c *Commanded) NewTimerAt(t time.Time) Timer {
	return c.armAt(&commandedTimer{c: c, index: -1, ch: make(chan time.Time, 1)}, func(time.Time) time.Time { return t })
}

// AfterFuncAt returns a timer that calls f when an advance reaches t, on
// the command goroutine, or at once in a new goroutine when the clock
// already has.
func (c *Commanded) AfterFuncAt(t time.Time, f func()) Timer {
	return c.armAt(&commandedTimer{c: c, index: -1, fn: f}, func(time.Time) time.Time { return t })
}

// Sleep blocks until an advance passes d or ctx is done; d <= 0 returns at
// once (ctx.Err() when ctx is already done).
func (c *Commanded) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := c.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C():
		return nil
	}
}

// Pending returns the number of armed timers.
func (c *Commanded) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// arm schedules t to fire after d; d <= 0 fires it at once without arming
// it.
func (c *Commanded) arm(t *commandedTimer, d time.Duration) *commandedTimer {
	return c.armAt(t, func(now time.Time) time.Time { return now.Add(d) })
}

// armAt schedules t to fire at the deadline deadline returns for the
// clock's time, read under c.mu; a deadline not after it fires t at once
// without arming it. t.at is written only under c.mu, and the fire time is
// read under it too, so a Reset on one goroutine never races an advance on
// the command goroutine.
func (c *Commanded) armAt(t *commandedTimer, deadline func(now time.Time) time.Time) *commandedTimer {
	c.mu.Lock()
	at := deadline(c.now)
	due := at.After(c.now)
	if !due {
		at = c.now
	}
	t.at = at
	if due {
		c.seq++
		t.seq = c.seq
		heap.Push(&c.timers, t)
	}
	c.mu.Unlock()
	if !due {
		t.fire(true, at)
	}
	return t
}

// remove disarms t and reports whether it was armed.
func (c *Commanded) remove(t *commandedTimer) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.index < 0 {
		return false
	}
	heap.Remove(&c.timers, t.index)
	return true
}

// advance moves the clock forward by d >= 0, firing every timer due by the
// new time, and returns the new time. Only the command goroutine calls it.
func (c *Commanded) advance(d time.Duration) time.Time {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		if len(c.timers) == 0 || c.timers[0].at.After(target) {
			c.now = target
			c.mu.Unlock()
			return target
		}
		t, _ := heap.Pop(&c.timers).(*commandedTimer)
		c.now = t.at
		at := t.at
		c.mu.Unlock()
		t.fire(false, at)
	}
}

// serve reads command lines and writes one answer each, until the stream
// ends or fails.
func (c *Commanded) serve() {
	defer close(c.done)
	r := c.r
	for {
		line, err := r.ReadSlice('\n')
		var answer string
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			if err = skipLine(r); err != nil {
				c.err = err
				return
			}
			answer = fmt.Sprintf("%s command longer than %d bytes", AnswerError, MaxCommandBytes)
		case err != nil:
			c.err = err
			return
		default:
			answer = c.handle(string(bytes.TrimRight(line, "\r\n")))
		}
		if _, err := io.WriteString(c.conn, answer+"\n"); err != nil {
			c.err = err
			return
		}
	}
}

// skipLine discards the rest of an overlong line.
func skipLine(r *bufio.Reader) error {
	for {
		_, err := r.ReadSlice('\n')
		if !errors.Is(err, bufio.ErrBufferFull) {
			return err
		}
	}
}

// handle runs one command and returns its answer without the newline.
func (c *Commanded) handle(line string) string {
	f := strings.Fields(line)
	switch {
	case len(f) == 1 && f[0] == CommandNow:
		return c.Now().UTC().Format(CommandTimeLayout)
	case len(f) == 2 && f[0] == CommandAdvance:
		d, err := time.ParseDuration(f[1])
		if err != nil {
			return AnswerError + " " + err.Error()
		}
		if d < 0 {
			return fmt.Sprintf("%s negative duration %s: the clock never moves backwards", AnswerError, f[1])
		}
		return c.advance(d).UTC().Format(CommandTimeLayout)
	case len(f) > 0 && f[0] == CommandStart:
		return fmt.Sprintf("%s %s comes first only: the clock started", AnswerError, CommandStart)
	default:
		return fmt.Sprintf("%s unknown command %q: want %q or %q", AnswerError, line, CommandAdvance+" <duration>", CommandNow)
	}
}

// commandedTimer is a Commanded clock's timer.
type commandedTimer struct {
	c     *Commanded
	at    time.Time // guarded by c.mu
	seq   uint64    // guarded by c.mu
	index int       // position in c.timers, -1 when not armed; guarded by c.mu
	ch    chan time.Time
	fn    func()
}

// fire delivers the timer with its fire time at, which the caller read
// under c.mu: a channel send that never blocks, or the callback (in a new
// goroutine when async, as time.AfterFunc does).
func (t *commandedTimer) fire(async bool, at time.Time) {
	switch {
	case t.fn != nil && async:
		go t.fn()
	case t.fn != nil:
		t.fn()
	default:
		select {
		case t.ch <- at:
		default:
		}
	}
}

func (t *commandedTimer) C() <-chan time.Time { return t.ch }
func (t *commandedTimer) Stop() bool          { return t.c.remove(t) }

func (t *commandedTimer) Reset(d time.Duration) bool {
	active := t.c.remove(t)
	t.c.arm(t, d)
	return active
}

func (t *commandedTimer) ResetAt(at time.Time) bool {
	active := t.c.remove(t)
	t.c.armAt(t, func(time.Time) time.Time { return at })
	return active
}

// timerHeap orders armed timers by deadline, then by arming order; it is
// guarded by the clock's mu.
type timerHeap []*commandedTimer

func (h timerHeap) Len() int { return len(h) }

func (h timerHeap) Less(i, j int) bool {
	if c := h[i].at.Compare(h[j].at); c != 0 {
		return c < 0
	}
	return h[i].seq < h[j].seq
}

func (h timerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *timerHeap) Push(x any) {
	t, _ := x.(*commandedTimer)
	t.index = len(*h)
	*h = append(*h, t)
}

func (h *timerHeap) Pop() any {
	old := *h
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	t.index = -1
	*h = old[:n-1]
	return t
}
