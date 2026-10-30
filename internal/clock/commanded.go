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
// time. Answers use CommandTimeLayout; a refused command gets an
// AnswerError line and leaves the clock where it is. Like Real, a zero or
// negative timer duration fires at once: NewTimer's channel is already
// filled and AfterFunc runs its callback in a new goroutine.
//
// One goroutine, started by NewCommanded, reads the commands; it ends when
// the stream ends or fails, or at Close, and the clock then stays at its
// last time. Methods are safe for concurrent use.
type Commanded struct {
	conn io.ReadWriteCloser
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
	c := &Commanded{conn: conn, done: make(chan struct{}), now: start.Round(0)}
	go c.serve()
	return c
}

// OpenCommanded returns a Commanded clock starting at the current real
// time that reads its commands on file descriptor fd, CommandFD in a
// controllable-clock process, and owns fd from then on. It returns
// ErrNoCommandDescriptor, leaving fd untouched, when fd is not open or not
// a socket, so it never takes over a descriptor the Go runtime or the
// process opened for something else; on platforms without file
// descriptors (Windows) it always does.
func OpenCommanded(fd uintptr) (*Commanded, error) {
	f, err := openCommandFD(fd)
	if err != nil {
		return nil, err
	}
	return NewCommanded(f, Real().Now()), nil
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
// it. t.at is written only under c.mu, and the fire time is read under it
// too, so a Reset on one goroutine never races an advance on the command
// goroutine.
func (c *Commanded) arm(t *commandedTimer, d time.Duration) *commandedTimer {
	c.mu.Lock()
	t.at = c.now.Add(max(d, 0))
	at := t.at
	if d > 0 {
		c.seq++
		t.seq = c.seq
		heap.Push(&c.timers, t)
	}
	c.mu.Unlock()
	if d <= 0 {
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
	r := bufio.NewReaderSize(c.conn, MaxCommandBytes)
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
