// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// Clock is the one controllable clock of a topology's Nodes
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope"): it holds the command socket of every running Node process,
// handover successors included, and keeps their clocks equal. Its methods
// are safe for concurrent use; commands run one at a time.
type Clock struct {
	enabled bool
	// timeout bounds each answer when the caller's context has no earlier
	// deadline; an answer normally takes milliseconds, so it only guards
	// against a Node that hangs.
	timeout time.Duration

	mu      sync.Mutex // serializes every command exchange
	now     time.Time
	set     bool
	members []*member
	broken  error // a command a running Node did not answer in order
}

// member is one Node process's command socket.
type member struct {
	name    string
	proc    *proc.Process
	conn    net.Conn
	r       *bufio.Reader
	held    bool      // stopped by SIGSTOP: skipped by advances
	reading time.Time // its clock's last answer
}

func newClock(enabled bool, timeout time.Duration) *Clock {
	return &Clock{enabled: enabled, timeout: timeout}
}

// Enabled reports whether the Nodes follow the clock (Config.TestClock).
func (c *Clock) Enabled() bool { return c.enabled }

// Now returns the clock's reading, the time every running Node's clock
// shows; the zero time before the first Node process starts, and always
// on a real-time topology.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock of every running Node forward by d and returns
// once each Node answered that every timer due by the new time has fired;
// a Node held by SIGSTOP catches up when resumed. What a fired timer
// starts in a Node then happens in real time, so a test waits for its
// effect with a condition poll. Advance fails on a real-time topology
// (ErrNoTestClock), for a negative d, before the first Node process
// starts, and when a running Node does not answer in time, which breaks
// the clock for good; a Node that exited meanwhile leaves the clock.
func (c *Clock) Advance(ctx context.Context, d time.Duration) error {
	if !c.enabled {
		return ErrNoTestClock
	}
	if d < 0 {
		return fmt.Errorf("topology: advance by %v: the clock never moves backwards", d)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken != nil {
		return c.broken
	}
	if !c.set {
		return errors.New("topology: the clock has no reading before a Node process starts")
	}
	target := c.now.Add(d)
	if err := c.advanceLocked(ctx, c.activeLocked(nil), d, target); err != nil {
		return err
	}
	c.now = target
	return nil
}

// attach adds a started process's command socket and starts its clock at
// the topology's time with the start command, which the process waits
// for before it reads the time, so a process started while the clock
// leads real time, as after a walk, arms no timer from an earlier time.
// The first process's start, the real time of its attach, sets the
// clock's reading. No advance runs between the reading and the start, as
// both hold the clock.
func (c *Clock) attach(ctx context.Context, name string, p *proc.Process, conn net.Conn) error {
	m := &member{name: name, proc: p, conn: conn, r: bufio.NewReaderSize(conn, clock.MaxCommandBytes)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken != nil {
		return c.broken
	}
	start := c.now
	if !c.set {
		start = time.Now().UTC().Round(0)
	}
	got, err := c.exchange(ctx, m, startLine(start))
	if err == nil && !got.Equal(start) {
		err = fmt.Errorf("answered %v, want %v", got, start)
	}
	if err != nil {
		return fmt.Errorf("topology: starting the clock of %s at the topology's time: %w", name, err)
	}
	m.reading = got
	c.now, c.set = start, true
	c.members = append(c.members, m)
	return nil
}

// detach removes p's socket once p has exited.
func (c *Clock) detach(p *proc.Process) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.members {
		if m.proc == p {
			c.dropLocked(m)
			return
		}
	}
}

// hold makes advances skip p, which SIGSTOP is about to stop.
func (c *Clock) hold(p *proc.Process) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.members {
		if m.proc == p {
			m.held = true
		}
	}
}

// release brings p, which SIGCONT resumed, to the topology's time and
// lets advances reach it again.
func (c *Clock) release(ctx context.Context, p *proc.Process) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken != nil {
		return c.broken
	}
	for _, m := range c.members {
		if m.proc != p || !m.held {
			continue
		}
		m.held = false
		if behind := c.now.Sub(m.reading); behind > 0 {
			return c.advanceLocked(ctx, []*member{m}, behind, c.now)
		}
	}
	return nil
}

// close closes every socket.
func (c *Clock) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.members {
		_ = m.conn.Close()
	}
	c.members = nil
}

// activeLocked returns the members advances reach, but skip.
func (c *Clock) activeLocked(skip *member) []*member {
	var out []*member
	for _, m := range c.members {
		if !m.held && m != skip {
			out = append(out, m)
		}
	}
	return out
}

func (c *Clock) dropLocked(m *member) {
	_ = m.conn.Close()
	for i, x := range c.members {
		if x == m {
			c.members = append(c.members[:i], c.members[i+1:]...)
			return
		}
	}
}

// advanceLocked sends "advance d" to ms at once and checks that each
// answers want. A member whose process exited leaves the clock; any other
// failure breaks it.
func (c *Clock) advanceLocked(ctx context.Context, ms []*member, d time.Duration, want time.Time) error {
	line := advanceLine(d)
	errs := make([]error, len(ms))
	var wg sync.WaitGroup
	for i, m := range ms {
		wg.Go(func() {
			got, err := c.exchange(ctx, m, line)
			if err == nil && !got.Equal(want) {
				err = fmt.Errorf("answered %v, want %v", got, want)
			}
			if err == nil {
				m.reading = got
			}
			errs[i] = err
		})
	}
	wg.Wait()
	var failed []error
	for i, m := range ms {
		switch {
		case errs[i] == nil:
		case c.exited(m, errs[i]):
			c.dropLocked(m)
		default:
			failed = append(failed, fmt.Errorf("%s: %w", m.name, errs[i]))
		}
	}
	if len(failed) > 0 {
		c.broken = fmt.Errorf("topology: clock broken: %w", errors.Join(failed...))
		return c.broken
	}
	return nil
}

// exited reports whether err comes from m's process having exited: the
// process closes its command socket only by exiting, so an end of stream
// waits, within the answer timeout, for the process to be reaped.
func (c *Clock) exited(m *member, err error) bool {
	select {
	case <-m.proc.Done():
		return true
	default:
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !isConnReset(err) {
		return false
	}
	select {
	case <-m.proc.Done():
		return true
	case <-time.After(c.timeout):
		return false
	}
}

// exchange sends one command and returns the time answered.
func (c *Clock) exchange(ctx context.Context, m *member, line string) (time.Time, error) {
	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = m.conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = m.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if _, err := io.WriteString(m.conn, line+"\n"); err != nil {
		return time.Time{}, err
	}
	answer, err := m.r.ReadString('\n')
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}, context.Cause(ctx)
		}
		return time.Time{}, err
	}
	answer = strings.TrimSuffix(answer, "\n")
	if strings.HasPrefix(answer, clock.AnswerError+" ") {
		return time.Time{}, fmt.Errorf("%q refused: %s", line, answer)
	}
	at, err := time.Parse(time.RFC3339Nano, answer)
	if err != nil {
		return time.Time{}, fmt.Errorf("answer to %q: %w", line, err)
	}
	return at, nil
}

// advanceLine is the advance command for d.
func advanceLine(d time.Duration) string { return clock.CommandAdvance + " " + d.String() }

// startLine is the start command for at.
func startLine(at time.Time) string {
	return clock.CommandStart + " " + at.UTC().Format(clock.CommandTimeLayout)
}
