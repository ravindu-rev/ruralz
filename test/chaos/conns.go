// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/l4lb"
)

// Drain timeline of a stop signal, from the SIGTERM
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Drain
// timeline defaults"): the accept window, then the K = 8 server shards of
// each listener shut down one at a time across the jitter window, the
// Drain deadline 20 s after stop-accepting, and the exit bound 30 s after
// the SIGTERM (target).
const (
	AcceptWindow  = 5 * time.Second
	WaveCount     = 8
	JitterWindow  = 10 * time.Second
	DrainDeadline = 20 * time.Second
	ExitBound     = 30 * time.Second
	// MinHeldConns is CE-2's least number of connections the Node holds at
	// stop-accepting (target).
	MinHeldConns = 100
)

// DefaultConnWatchEvery is how often a ConnWatch lists the balancers'
// open connections.
const DefaultConnWatchEvery = 50 * time.Millisecond

// ConnSource is an L4 balancer whose connections a ConnWatch follows
// (internal/testkit/topology.Balancer).
type ConnSource interface {
	// Listener names the balanced listener, such as "http".
	Listener() string
	// Conns returns the open client connections.
	Conns() []l4lb.ConnInfo
	// ConnOf returns the newest connection from a client address, open
	// or recently closed.
	ConnOf(client string) (l4lb.ConnInfo, bool)
}

// Conn is one client connection a balancer carried.
type Conn struct {
	// Listener names the balancer's listener.
	Listener string
	l4lb.ConnInfo
	// Seen is the last listing that showed the connection open, and Gone
	// the first listing that no longer did, zero while it stays open: a
	// close the balancer's history no longer holds, as when the client
	// address was reused, lies between them.
	Seen, Gone time.Time
}

// CloseSpan returns the real instants between which the connection
// closed: its Closed time twice when the balancer recorded it, else the
// listings around its disappearance; ok is false for a connection still
// open at the last listing.
func (c Conn) CloseSpan() (lo, hi time.Time, ok bool) {
	switch {
	case !c.Closed.IsZero():
		return c.Closed, c.Closed, true
	case !c.Gone.IsZero():
		return c.Seen, c.Gone, true
	default:
		return time.Time{}, time.Time{}, false
	}
}

// ConnWatch lists the open connections of balancers on a period and
// keeps every connection it saw, so a run can tell which connections a
// Node held at an instant and when each closed, as the L4 balancer saw
// them (CE-2). Its methods are safe for concurrent use.
type ConnWatch struct {
	srcs  []ConnSource
	every time.Duration

	mu   sync.Mutex
	seen map[connKey]Conn

	stop chan struct{}
	done chan struct{}
}

type connKey struct {
	listener string
	id       uint64
}

// StartConnWatch starts listing srcs every period (DefaultConnWatchEvery
// when 0) until Stop or the end of ctx.
func StartConnWatch(ctx context.Context, every time.Duration, srcs ...ConnSource) *ConnWatch {
	if every <= 0 {
		every = DefaultConnWatchEvery
	}
	w := &ConnWatch{srcs: srcs, every: every, seen: map[connKey]Conn{}, stop: make(chan struct{}), done: make(chan struct{})}
	go w.loop(ctx)
	return w
}

func (w *ConnWatch) loop(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.every)
	defer ticker.Stop()
	for {
		w.Poll()
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Poll lists every balancer's open connections once, marking those seen
// open before that the listing no longer shows.
func (w *ConnWatch) Poll() {
	for _, s := range w.srcs {
		if s == nil {
			continue
		}
		conns := s.Conns()
		now := time.Now()
		open := make(map[uint64]bool, len(conns))
		w.mu.Lock()
		for _, c := range conns {
			open[c.ID] = true
			w.seen[connKey{s.Listener(), c.ID}] = Conn{Listener: s.Listener(), ConnInfo: c, Seen: now}
		}
		for k, c := range w.seen {
			if k.listener == s.Listener() && !open[k.id] && c.Gone.IsZero() {
				c.Gone = now
				w.seen[k] = c
			}
		}
		w.mu.Unlock()
	}
}

// Stop stops the watch after a last listing.
func (w *ConnWatch) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
	w.Poll()
}

// Open returns the connections open at the last listing that each
// backend carries, by backend name.
func (w *ConnWatch) Open() map[string]int {
	out := map[string]int{}
	for _, s := range w.srcs {
		if s == nil {
			continue
		}
		for _, c := range s.Conns() {
			if c.Backend != "" {
				out[c.Backend]++
			}
		}
	}
	return out
}

// Conns returns every connection seen, its close time refreshed from the
// balancer's history for one closed since the last listing, sorted by
// listener and ID.
func (w *ConnWatch) Conns() []Conn {
	w.mu.Lock()
	out := make([]Conn, 0, len(w.seen))
	for _, c := range w.seen {
		out = append(out, c)
	}
	w.mu.Unlock()
	src := map[string]ConnSource{}
	for _, s := range w.srcs {
		if s != nil {
			src[s.Listener()] = s
		}
	}
	for i, c := range out {
		if !c.Closed.IsZero() {
			continue
		}
		if s, ok := src[c.Listener]; ok {
			if ci, ok := s.ConnOf(c.Client); ok && ci.ID == c.ID {
				out[i].ConnInfo = ci
			}
		}
	}
	slices.SortFunc(out, func(a, b Conn) int {
		switch {
		case a.Listener != b.Listener:
			if a.Listener < b.Listener {
				return -1
			}
			return 1
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out
}

// HeldAt returns the connections backend carried at real instant t:
// opened at or before it and not surely closed by it.
func HeldAt(conns []Conn, backend string, t time.Time) []Conn {
	var out []Conn
	for _, c := range conns {
		if c.Backend != backend || c.Opened.After(t) {
			continue
		}
		if _, hi, closed := c.CloseSpan(); !closed || hi.After(t) {
			out = append(out, c)
		}
	}
	return out
}

// Span is an instant known within [Lo, Hi], as a real instant maps to
// Node time while an advance is in flight.
type Span struct{ Lo, Hi time.Duration }

// NeverClosed is the close offset of a connection still open at the end
// of a watch.
const NeverClosed = time.Duration(1<<63 - 1)

// WaveReport is CE-2's reconnect-wave analysis of the connections a Node
// held at stop-accepting.
type WaveReport struct {
	// Held is M, the connections held at stop-accepting.
	Held int
	// Eighths counts the held connections whose close falls in each eighth
	// of the jitter window; a close known only within a span counts in
	// every eighth the span reaches.
	Eighths [WaveCount]int
	// Empty counts the eighths no close reaches: each must close one
	// wave.
	Empty int
	// MaxSlice is the most closes any 1 s slice of the jitter window
	// holds, and SliceBound its bound, two waves of 2 × ⌈M / 8⌉
	// connections.
	MaxSlice, SliceBound int
	// LeftAtDeadline counts the held connections still open at the Drain
	// deadline.
	LeftAtDeadline int
}

// AnalyzeWaves analyzes the closes of the M held connections, each as an
// offset span from stop-accepting in Node time (NeverClosed when it never
// closed): each eighth of the jitter window must close one wave, no 1 s
// slice of it more than two waves, and none may outlive the Drain
// deadline (docs/architecture/11-scalability-and-distributed-state.md
// "Chaos experiments", CE-2).
func AnalyzeWaves(closes []Span) WaveReport {
	rep := WaveReport{Held: len(closes)}
	rep.SliceBound = 2 * ((len(closes) + WaveCount - 1) / WaveCount)
	eighth := JitterWindow / WaveCount
	var mids []time.Duration
	for _, c := range closes {
		if c.Lo >= DrainDeadline || c.Hi == NeverClosed {
			rep.LeftAtDeadline++
		}
		for i := range WaveCount {
			from, to := time.Duration(i)*eighth, time.Duration(i+1)*eighth
			if c.Hi >= from && c.Lo < to {
				rep.Eighths[i]++
			}
		}
		if c.Hi != NeverClosed {
			mid := c.Lo + (c.Hi-c.Lo)/2
			if mid >= 0 && mid < JitterWindow {
				mids = append(mids, mid)
			}
		}
	}
	for _, n := range rep.Eighths {
		if n == 0 {
			rep.Empty++
		}
	}
	rep.MaxSlice = maxInWindow(mids, time.Second)
	return rep
}

// maxInWindow returns the most points any half-open interval of length w
// holds.
func maxInWindow(points []time.Duration, w time.Duration) int {
	s := slices.Clone(points)
	slices.Sort(s)
	best, j := 0, 0
	for i := range s {
		for s[i]-s[j] >= w {
			j++
		}
		best = max(best, i-j+1)
	}
	return best
}
