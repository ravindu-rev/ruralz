// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package coalesce is the Response Cache's miss coalescing
// (docs/architecture/09-traffic-management-and-resilience.md "Response
// caching"): concurrent misses for one key on one Node share the first
// miss's Upstream fetch. The cache Filter (internal/filter/cache) holds one
// Node-wide Table and keeps each miss's Ticket in its PolicyState.
//
// # Flights
//
// A flight is keyed like the entry it would fill, by the partition's URI
// and partition digests, plus the generation the miss's lookup read, so a
// miss that read a newer generation after an invalidation never reuses an
// older fetch. The first GET miss of a key leads the flight and fetches;
// later misses of the key follow it. A HEAD miss only follows, since a HEAD
// response has no body to give GET followers. At most MaxKeys keys (4,096,
// target) have a flight at once; a GET miss past that fetches alone.
//
// The leader resolves the flight exactly once: Publish when its response
// is storable and its body copy complete, Abandon otherwise (not storable,
// the copy stopped, the request failed or the client went away), at the
// latest in the Filter's Finish. A follower then reuses the published
// response like a hit, through its own response chain, when its own
// Vary-selected values under the published Vary names equal the leader's;
// otherwise it fetches alone. A follower that reuses the response records
// the result coalesced (emit.CacheCoalesced, catalog.CacheResults) on
// ruralz_cache_requests_total and the access log's cache field, through
// the Filter's Exchange, since only the Filter sees the request.
//
// # Bounded waits
//
// A follower waits for the leader's response headers as long as its own
// request lives, since a fetch of its own would wait on the same Upstream.
// Once the leader finds its response storable and starts the body copy
// (Streaming, given the copy's filter.Tee.Stopped channel), the flight
// takes no new followers, a later miss leading a new flight. Each follower
// then waits for the body until the copy completes, at most BodyWait (1 s,
// target) more, and stops at once when the copy stops without completing,
// past the copy limit or the buffer budget: a slow client of the leader or
// an oversized body never holds followers for long.
package coalesce

import (
	"context"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Bounds ("Response caching", "Stateless data plane").
const (
	// MaxKeys bounds the keys with a flight per Node (target).
	MaxKeys = 4096
	// BodyWait bounds a follower's wait for the leader's body after
	// Streaming (target).
	BodyWait = time.Second
)

// Key identifies a flight.
type Key struct {
	// Cache is the partition: the URI and partition digests.
	Cache statestore.CacheKey
	// Generation is the URI's generation the miss's lookup read.
	Generation int64
}

// Role is a miss's part in its flight.
type Role uint8

// Roles.
const (
	// Alone means no flight: the miss fetches by itself.
	Alone Role = iota
	// Leader fetches for its flight and resolves it.
	Leader
	// Follower waits for the leader's result.
	Follower
)

// String returns the name of r.
func (r Role) String() string {
	switch r {
	case Alone:
		return "alone"
	case Leader:
		return "leader"
	case Follower:
		return "follower"
	default:
		return "unknown"
	}
}

// Outcome is how a follower's wait ended.
type Outcome uint8

// Outcomes. Every one but Reuse sends the follower to fetch alone, or
// ends it with its request.
const (
	// Reuse: the leader published a response whose Vary-selected values
	// match the follower's; serve it like a hit.
	Reuse Outcome = iota + 1
	// Mismatch: the follower's Vary-selected values differ.
	Mismatch
	// Abandoned: the leader stores nothing, or the Ticket was no
	// follower's.
	Abandoned
	// TimedOut: the body did not complete within BodyWait of Streaming.
	TimedOut
	// Canceled: the follower's context ended first.
	Canceled
	// Stopped: the leader's body copy stopped without completing (past
	// the copy limit or the buffer budget), so the leader stores nothing.
	Stopped
)

// String returns the name of o.
func (o Outcome) String() string {
	switch o {
	case Reuse:
		return "reuse"
	case Mismatch:
		return "mismatch"
	case Abandoned:
		return "abandoned"
	case TimedOut:
		return "timed_out"
	case Canceled:
		return "canceled"
	case Stopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Selector gives a follower's variant digest under Vary names, computed
// as the cache Filter computes a lookup's variant.
type Selector interface {
	Variant(names string) statestore.Digest
}

// Options configure a Table.
type Options struct {
	// Clock times BodyWait; nil uses clock.Real().
	Clock clock.Clock
	// MaxKeys and BodyWait replace the bounds when positive; only tests
	// set them.
	MaxKeys  int
	BodyWait time.Duration
}

// Table is a Node's set of flights, shared across snapshots; it is safe
// for concurrent use.
type Table[V any] struct {
	clk      clock.Clock
	maxKeys  int
	bodyWait time.Duration

	mu      sync.Mutex
	flights map[Key]*flight[V]
}

// flight states.
const (
	open uint8 = iota
	streaming
	resolved
)

// flight is one leader's fetch and its followers' rendezvous.
type flight[V any] struct {
	t   *Table[V]
	key Key
	// started closes at Streaming, done at resolution; the fields below
	// are written under t.mu before the close that publishes them.
	started, done chan struct{}
	state         uint8
	streamAt      time.Time
	// stopped is the leader's copy's filter.Tee.Stopped channel, set at
	// Streaming; nil never stops.
	stopped   <-chan struct{}
	published bool
	names     string
	variant   statestore.Digest
	value     V
}

// Ticket is a miss's handle on its flight. The zero Ticket is Alone; a
// Ticket is a small value the Filter keeps in its PolicyState.
type Ticket[V any] struct {
	f    *flight[V]
	role Role
}

// NewTable returns an empty Table.
func NewTable[V any](o Options) *Table[V] {
	t := &Table[V]{clk: o.Clock, maxKeys: MaxKeys, bodyWait: BodyWait, flights: map[Key]*flight[V]{}}
	if t.clk == nil {
		t.clk = clock.Real()
	}
	if o.MaxKeys > 0 {
		t.maxKeys = o.MaxKeys
	}
	if o.BodyWait > 0 {
		t.bodyWait = o.BodyWait
	}
	return t
}

// Lead joins the flight of k as a follower, or starts it with the caller
// as leader; Alone when MaxKeys flights exist. GET misses use it.
func (t *Table[V]) Lead(k Key) Ticket[V] {
	t.mu.Lock()
	defer t.mu.Unlock()
	if f := t.flights[k]; f != nil {
		return Ticket[V]{f: f, role: Follower}
	}
	if len(t.flights) >= t.maxKeys {
		return Ticket[V]{}
	}
	f := &flight[V]{t: t, key: k, started: make(chan struct{}), done: make(chan struct{})}
	t.flights[k] = f
	return Ticket[V]{f: f, role: Leader}
}

// Follow joins the flight of k as a follower, or returns Alone when there
// is none it can join; it never starts one. HEAD misses use it.
func (t *Table[V]) Follow(k Key) Ticket[V] {
	t.mu.Lock()
	defer t.mu.Unlock()
	if f := t.flights[k]; f != nil {
		return Ticket[V]{f: f, role: Follower}
	}
	return Ticket[V]{}
}

// Len returns the number of keys whose flight still takes followers.
func (t *Table[V]) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.flights)
}

// detach removes f from the Table, so a later miss of its key leads a new
// flight. t.mu is held.
func (t *Table[V]) detach(f *flight[V]) {
	if t.flights[f.key] == f {
		delete(t.flights, f.key)
	}
}

// Role returns the Ticket's role.
func (tk Ticket[V]) Role() Role { return tk.role }

// Streaming tells the followers that the leader's response is storable
// and its body copy started: the flight takes no new followers, and each
// follower waits at most BodyWait more, stopping at once when stopped, the
// copy's filter.Tee.Stopped channel, closes (nil never does). It does
// nothing on a non-leader Ticket, after a resolution or when called again.
func (tk Ticket[V]) Streaming(stopped <-chan struct{}) {
	if tk.role != Leader {
		return
	}
	f := tk.f
	t := f.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if f.state != open {
		return
	}
	f.state, f.streamAt, f.stopped = streaming, t.clk.Now(), stopped
	t.detach(f)
	close(f.started)
}

// Publish resolves the flight with the leader's complete, storable
// response v, stored under the Vary names names (normalized as the
// partition stores them, "" without Vary) for the leader's variant
// digest variant. v is shared by every follower that reuses it, which
// must not modify it. It does nothing on a non-leader Ticket or after a
// resolution.
func (tk Ticket[V]) Publish(names string, variant statestore.Digest, v V) {
	if tk.role != Leader {
		return
	}
	f := tk.f
	t := f.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if f.state == resolved {
		return
	}
	f.state, f.published = resolved, true
	f.names, f.variant, f.value = names, variant, v
	t.detach(f)
	close(f.done)
}

// Abandon resolves the flight without a response: every follower fetches
// alone. It does nothing on a non-leader Ticket or after a resolution, so
// the Filter's Finish may call it whatever happened before.
func (tk Ticket[V]) Abandon() {
	if tk.role != Leader {
		return
	}
	f := tk.f
	t := f.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if f.state == resolved {
		return
	}
	f.state = resolved
	t.detach(f)
	close(f.done)
}

// Wait blocks a follower until its leader resolves the flight, the
// leader's body copy stops without completing, BodyWait passes after
// Streaming, or ctx ends, and returns the published value with Reuse when
// sel's variant under the published names equals the leader's (any
// follower matches a response without Vary). Every other outcome returns
// the zero value; a Ticket that is not a follower's returns Abandoned at
// once. A resolution that arrives with a stop or the timer wins.
func (tk Ticket[V]) Wait(ctx context.Context, sel Selector) (V, Outcome) {
	var zero V
	if tk.role != Follower {
		return zero, Abandoned
	}
	f := tk.f
	select {
	case <-f.done:
		return f.result(sel)
	case <-f.started:
	case <-ctx.Done():
		return zero, Canceled
	}
	tm := f.t.clk.NewTimer(f.streamAt.Add(f.t.bodyWait).Sub(f.t.clk.Now()))
	defer tm.Stop()
	select {
	case <-f.done:
		return f.result(sel)
	case <-f.stopped:
		return f.resultOr(sel, Stopped)
	case <-tm.C():
		return f.resultOr(sel, TimedOut)
	case <-ctx.Done():
		return zero, Canceled
	}
}

// resultOr returns the flight's answer when it is already resolved, else
// the zero value and o.
func (f *flight[V]) resultOr(sel Selector, o Outcome) (V, Outcome) {
	select {
	case <-f.done:
		return f.result(sel)
	default:
		var zero V
		return zero, o
	}
}

// result returns the resolved flight's answer for sel; done is closed.
func (f *flight[V]) result(sel Selector) (V, Outcome) {
	var zero V
	switch {
	case !f.published:
		return zero, Abandoned
	case f.names == "":
		return f.value, Reuse
	case sel == nil || sel.Variant(f.names) != f.variant:
		return zero, Mismatch
	default:
		return f.value, Reuse
	}
}
