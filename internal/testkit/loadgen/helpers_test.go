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

// idle reports whether g has dispatched every request due by fk's time
// and every dispatched request has ended, or the run is over.
func idle(g *Generator, fk *clocktest.Fake) bool {
	select {
	case <-g.Done():
		return true
	default:
	}
	p := g.Progress()
	return p.Done == p.Scheduled && p.Next.After(fk.Now())
}

// drive advances fk by step(i) until g's run ends, letting every due
// request end before each step, so latencies depend on the steps alone.
func drive(t *testing.T, g *Generator, fk *clocktest.Fake, step func(i int) time.Duration) *Result {
	t.Helper()
	for i := 0; ; i++ {
		waitFor(t, "the generator to go idle", func() bool { return idle(g, fk) })
		select {
		case <-g.Done():
			res, err := g.Wait()
			if err != nil {
				t.Fatalf("Wait = %v", err)
			}
			return res
		default:
		}
		fk.Advance(step(i))
	}
}

func every(d time.Duration) func(int) time.Duration { return func(int) time.Duration { return d } }

// ok is a target answering 200 at once.
func ok() Target {
	return TargetFunc(func(context.Context, Request) Response { return Response{Status: 200} })
}

// recorder collects requests a target saw.
type seen struct {
	mu   sync.Mutex
	reqs []Request
}

func (s *seen) add(r Request) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r)
	s.mu.Unlock()
}

func (s *seen) list() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.reqs...)
}

func start(t *testing.T, tgt Target, c Config) *Generator {
	t.Helper()
	g, err := Start(t.Context(), tgt, c)
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() {
		g.Stop()
		_, _ = g.Wait()
	})
	return g
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }
