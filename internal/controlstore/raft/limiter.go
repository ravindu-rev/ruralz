// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"context"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// PeerRate is the most bytes per second this replica sends one peer over
// 8092, all classes together: 64 MiB/s (target)
// (docs/architecture/04-control-plane-and-gitops.md "Replica roles").
const PeerRate = 64 << 20

// limiter is one peer's send budget: a token bucket holding up to one
// second of PeerRate. Content yields to Raft within it (ADR-0006 "Content
// store"): a Raft write takes tokens whenever they are there, and a
// content write waits while any Raft write waits.
type limiter struct {
	clk  clock.Clock
	rate float64

	mu          sync.Mutex
	tokens      float64
	last        time.Time
	raftWaiting int
	// wake is closed and replaced whenever a Raft writer stops waiting.
	wake chan struct{}
}

// newLimiter returns a full bucket refilling at rate bytes per second.
func newLimiter(clk clock.Clock, rate float64) *limiter {
	return &limiter{clk: clk, rate: rate, tokens: rate, last: clk.Now(), wake: make(chan struct{})}
}

// refill adds the tokens earned since the last call; l.mu is held.
func (l *limiter) refill(now time.Time) {
	if d := now.Sub(l.last); d > 0 {
		l.tokens = min(l.rate, l.tokens+d.Seconds()*l.rate)
		l.last = now
	}
}

// wait blocks until n bytes may be sent, Raft writes first, or ctx ends.
// A write larger than the bucket waits for a full bucket and takes it
// all, going into debt, so it never waits forever.
func (l *limiter) wait(ctx context.Context, n int, raftClass bool) error {
	if n <= 0 {
		return nil
	}
	need := min(float64(n), l.rate)
	registered := false
	defer func() {
		if registered {
			l.mu.Lock()
			l.raftWaiting--
			close(l.wake)
			l.wake = make(chan struct{})
			l.mu.Unlock()
		}
	}()
	for {
		l.mu.Lock()
		l.refill(l.clk.Now())
		if (raftClass || l.raftWaiting == 0) && l.tokens >= need {
			l.tokens -= float64(n)
			l.mu.Unlock()
			return nil
		}
		var wait time.Duration
		var wake <-chan struct{}
		switch {
		case !raftClass && l.raftWaiting > 0:
			wake = l.wake
		default:
			wait = time.Duration((need - l.tokens) / l.rate * float64(time.Second))
			if wait <= 0 {
				wait = time.Millisecond
			}
		}
		if raftClass && !registered {
			l.raftWaiting++
			registered = true
		}
		l.mu.Unlock()
		if wake != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wake:
			}
			continue
		}
		if err := l.clk.Sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// limiters holds one limiter per peer server ID.
type limiters struct {
	clk  clock.Clock
	rate float64

	mu  sync.Mutex
	all map[controlstore.ServerID]*limiter
}

// newLimiters returns the per-peer budgets.
func newLimiters(clk clock.Clock, rate float64) *limiters {
	return &limiters{clk: clk, rate: rate, all: map[controlstore.ServerID]*limiter{}}
}

// of returns the limiter of one peer.
func (ls *limiters) of(id controlstore.ServerID) *limiter {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	l := ls.all[id]
	if l == nil {
		l = newLimiter(ls.clk, ls.rate)
		ls.all[id] = l
	}
	return l
}
