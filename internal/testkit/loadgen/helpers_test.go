// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

func epoch() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }

// waitFor polls a state condition; the 10 s guard only catches hangs, so
// no outcome depends on wall-clock time.
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

// progress is a running generator's counters, read from its state.
type progress struct {
	// scheduled counts requests whose intended time came, Missed ones
	// included; done those that ended.
	scheduled, done int64
	// missed counts requests never sent.
	missed int64
	// inFlight counts requests being sent; backlog those waiting.
	inFlight, backlog int
	// next is the intended send time of the next request to schedule.
	next time.Time
}

// progressOf returns g's counters now.
func progressOf(g *Generator) progress {
	g.mu.Lock()
	inFlight, backlog := g.inFlight, g.backlog
	g.mu.Unlock()
	st := g.rec.snapshot()
	var done int64
	for _, n := range st.Outcomes {
		done += n
	}
	return progress{
		scheduled: st.Offered, done: done, missed: st.Outcomes[Missed],
		inFlight: inFlight, backlog: backlog,
		next: g.start.Add(g.offset(uint64(st.Offered))), //nolint:gosec // G115: Offered counts dispatched requests, never negative
	}
}

// ended reports whether g's run is over.
func ended(g *Generator) bool {
	select {
	case <-g.done:
		return true
	default:
		return false
	}
}

// idle reports whether g has dispatched every request due by fk's time
// and every dispatched request has ended, or the run is over.
func idle(g *Generator, fk *clocktest.Fake) bool {
	if ended(g) {
		return true
	}
	p := progressOf(g)
	return p.done == p.scheduled && p.next.After(fk.Now())
}

// drive moves fk on by step until g's run ends, letting every due request
// end before each step. Each step is a Jump: with Advance, the generator,
// woken by the timer of its next request, could read any deadline inside
// a long step and send some of the requests the step makes late on time.
func drive(t *testing.T, g *Generator, fk *clocktest.Fake, step time.Duration) Stats {
	t.Helper()
	for {
		waitFor(t, "the generator to go idle", func() bool { return idle(g, fk) })
		if ended(g) {
			res, err := g.Wait()
			if err != nil {
				t.Fatalf("Wait = %v", err)
			}
			return res
		}
		fk.Jump(step)
	}
}

// targetFunc adapts a function to Target.
type targetFunc func(ctx context.Context) Response

// Do calls f.
func (f targetFunc) Do(ctx context.Context) Response { return f(ctx) }

// ok is a target answering 200 at once.
func ok() Target {
	return targetFunc(func(context.Context) Response { return Response{Status: 200} })
}

// seen records when, on its clock, a target was called, in call order.
type seen struct {
	clk *clocktest.Fake

	mu    sync.Mutex
	times []time.Time
}

// add records a call and returns its index, from 0.
func (s *seen) add() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.times = append(s.times, s.clk.Now())
	return len(s.times) - 1
}

func (s *seen) list() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.times...)
}

// start starts a run whose context the test's end cancels.
func start(t *testing.T, tgt Target, c Config) *Generator {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	g, err := Start(ctx, tgt, c)
	if err != nil {
		cancel()
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_, _ = g.Wait()
	})
	return g
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }
