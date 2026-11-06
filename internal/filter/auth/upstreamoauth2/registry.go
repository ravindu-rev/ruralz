// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// ComponentName is the Registry's filter.Component name.
const ComponentName = "auth.upstream-oauth2 token registry"

// MinFetchInterval is the shortest time between the starts of two fetches
// of one entry: 1 s. It floors the refresh and retry times, and
// a miss within it of a failed fetch fails at once instead of starting
// another, so a failing token endpoint gets at most one request per
// MinFetchInterval per Policy and Node however many requests arrive.
const MinFetchInterval = time.Second

// ErrRunning reports a Run while another Run of the same Registry is
// active.
var ErrRunning = errors.New("upstreamoauth2: registry already running")

// Registry states.
const (
	stateIdle int32 = iota
	stateRunning
	stateStopped
)

// Options configure a Registry.
type Options struct {
	// Guard is the egress guard of the token endpoint connections
	// (egress.FromEnv); nil applies the default deny table alone.
	Guard *egress.Guard
	// TLS is the token endpoint client configuration; nil uses
	// tlsconf.IdentityProvider(). The guarded client never disables
	// verification.
	TLS *tls.Config
	// Clock drives token lifetimes, refreshes and MinFetchInterval; nil
	// uses clock.Real().
	Clock clock.Clock
	// Logger receives fetch failures when a Filter's BuildEnv has no
	// logger; nil discards them.
	Logger *slog.Logger
}

// Registry is the upstream token registry: the Node-wide filter.Component
// holding one token entry per Policy name and fetch parameters (package
// documentation "Token registry"). Factory builds its Filters on it.
type Registry struct {
	guard *egress.Guard
	tls   *tls.Config
	clock clock.Clock
	log   *slog.Logger
	// wake asks Run for a scheduling pass (capacity 1, sent without
	// blocking).
	wake chan struct{}
	// state is stateIdle before the first Run, stateRunning during one,
	// stateStopped after.
	state atomic.Int32

	mu      sync.Mutex // guards entries; taken before any entry.mu
	entries map[entryKey]*entry

	// fetchHook, set by in-package tests before Run, is called after
	// every fetch published its result.
	fetchHook func()
}

var _ filter.Component = (*Registry)(nil)

// NewRegistry returns an idle Registry; the supervisor runs it (Run).
func NewRegistry(o Options) *Registry {
	r := &Registry{
		guard:   o.Guard,
		tls:     o.TLS,
		clock:   o.Clock,
		log:     o.Logger,
		wake:    make(chan struct{}, 1),
		entries: map[entryKey]*entry{},
	}
	if r.tls == nil {
		r.tls = tlsconf.IdentityProvider()
	}
	if r.clock == nil {
		r.clock = clock.Real()
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	return r
}

// Name implements filter.Component.
func (*Registry) Name() string { return ComponentName }

// Run starts the fetches the entries need, a miss's at once and a refresh
// at its time, each on a goroutine it owns, until ctx is done. It then
// fails the misses no fetch serves yet, cancels the running fetches and
// returns nil after they exited; later misses fail at once. A Run while
// another is active returns ErrRunning.
func (r *Registry) Run(ctx context.Context) error {
	if !r.state.CompareAndSwap(stateIdle, stateRunning) && !r.state.CompareAndSwap(stateStopped, stateRunning) {
		return ErrRunning
	}
	var fetches sync.WaitGroup
	defer func() {
		r.state.Store(stateStopped)
		for _, e := range r.list() {
			e.abandon(errStopped)
		}
		fetches.Wait()
	}()
	for {
		next := r.dispatch(ctx, &fetches)
		var timer clock.Timer
		var fire <-chan time.Time
		if !next.IsZero() {
			timer = r.clock.NewTimerAt(next)
			fire = timer.C()
		}
		select {
		case <-ctx.Done():
		case <-r.wake:
		case <-fire:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// dispatch starts every due fetch and returns the earliest later refresh
// time, zero when none is scheduled.
func (r *Registry) dispatch(ctx context.Context, fetches *sync.WaitGroup) time.Time {
	now := r.clock.Now()
	var next time.Time
	for _, e := range r.list() {
		if at := e.dispatch(ctx, fetches, now); !at.IsZero() && (next.IsZero() || at.Before(next)) {
			next = at
		}
	}
	return next
}

// list returns the current entries.
func (r *Registry) list() []*entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Collect(maps.Values(r.entries))
}

// kick asks Run for a scheduling pass without blocking.
func (r *Registry) kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// acquire returns the entry of k, creating it, and registers h as its
// newest holder.
func (r *Registry) acquire(k entryKey, h *holder) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[k]
	if e == nil {
		e = newEntry(r, k)
		r.entries[k] = e
	}
	e.mu.Lock()
	e.holders = append(e.holders, h)
	e.mu.Unlock()
	return e
}

// release unregisters h from e; the last holder closes e: its refresh is
// unscheduled, a pending miss fails, a running fetch is canceled, and the
// entry leaves the Registry, so a later acquire of its key starts afresh.
func (r *Registry) release(e *entry, h *holder) {
	r.mu.Lock()
	e.mu.Lock()
	if i := slices.Index(e.holders, h); i >= 0 {
		e.holders = slices.Delete(e.holders, i, i+1)
	}
	last := len(e.holders) == 0 && !e.closed
	if last {
		e.closed = true
		e.refreshAt = time.Time{}
		if f := e.flight; f != nil {
			if f.started {
				f.cancel()
			} else {
				e.flight = nil
				f.settle(nil, errClosed)
			}
		}
	}
	e.mu.Unlock()
	if last && r.entries[e.key] == e {
		delete(r.entries, e.key)
	}
	r.mu.Unlock()
	if last {
		e.client.CloseIdleConnections()
	}
}
