// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwks

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// Fetch scheduling (Security and identity, "JWT and OIDC").
const (
	// PrefetchTimeout bounds how long a Revision compile waits for the
	// first fetches of its issuers: "Nodes prefetch issuers, 16 at once,
	// within 5 s (target) ... and activate even if a fetch fails".
	PrefetchTimeout = 5 * time.Second
	// MaxConcurrentFetches is the most fetches in flight at once.
	MaxConcurrentFetches = 16
	// FetchTimeout bounds one fetch, redirects and body read included: the
	// prefetch bound, so no fetch outlasts a compile's wait.
	FetchTimeout = PrefetchTimeout
	// RefreshInterval spaces the fetches an unknown kid triggers: "at most
	// one refresh per issuer per 30 s (target), honoring Retry-After".
	RefreshInterval = 30 * time.Second
)

// RetryInterval is the delay before a failed fetch is retried when the
// response named no later Retry-After: 30 s (target), the unknown-kid
// refresh interval.
const RetryInterval = RefreshInterval

// MaxRedirects bounds the https redirects a fetch follows, each hop
// re-checked by the egress guard: 3 (target).
const MaxRedirects = 3

// componentName names the Manager in logs and shutdown errors.
const componentName = "jwks"

// ErrRunning reports a second concurrent Run.
var ErrRunning = errors.New("jwks: manager already running")

// Source is one issuer's key set: the iss value tokens carry and the https
// jwksUrl its keys come from.
type Source struct {
	Issuer string
	URL    string
}

// Options configures a Manager.
type Options struct {
	// Guard is the egress guard (RURALZ_FETCH_ALLOW); nil applies the
	// default deny table.
	Guard *egress.Guard
	// TLS is the client configuration; nil means
	// tlsconf.IdentityProvider(). It is cloned and verification is never
	// disabled (egress.ClientOptions).
	TLS *tls.Config
	// Clock schedules refreshes; nil means clock.Real().
	Clock clock.Clock
	// Logger receives fetch failures and recoveries; nil discards them.
	Logger *slog.Logger
	// Status receives jwks_stale; nil records nothing.
	Status emit.NodeStatus
	// Revoker is consulted on every key load; nil means
	// identity.NopRevoker.
	Revoker identity.Revoker
}

// Manager fetches, caches and refreshes the key sets of every issuer the
// active snapshots name. It is safe for concurrent use.
type Manager struct {
	client  *http.Client
	clock   clock.Clock
	log     *slog.Logger
	status  emit.NodeStatus
	revoker identity.Revoker
	// wake has room for one signal: a new due fetch, a nudge or a
	// completed fetch.
	wake    chan struct{}
	running atomic.Bool

	mu      sync.Mutex
	entries map[Source]*entry
}

var _ filter.Component = (*Manager)(nil)

// entry is the shared state of one Source.
type entry struct {
	src Source
	// where names the Source in logs and errors, with any password in the
	// URL redacted.
	where string
	// holder is the entry's jwks_stale source name.
	holder string

	keys atomic.Pointer[Keys]
	// nudgeAt is the earliest time, in Unix nanoseconds, a Nudge may
	// schedule a fetch: RefreshInterval after the last fetch began.
	nudgeAt atomic.Int64

	// Guarded by Manager.mu.
	refs       int
	next       time.Time // when the next fetch is due
	inflight   bool
	retryAfter time.Time // no fetch before
	failing    bool      // the last fetch failed; jwks_stale is raised
	removed    bool
	// done is closed when the next fetch completes (or the entry is
	// removed), then replaced.
	done chan struct{}
}

// New returns a Manager; call Run to start fetching.
func New(o Options) *Manager {
	tlsCfg := o.TLS
	if tlsCfg == nil {
		tlsCfg = tlsconf.IdentityProvider()
	}
	m := &Manager{
		client: o.Guard.Client(egress.ClientOptions{
			TLS:          tlsCfg,
			MaxRedirects: MaxRedirects,
			Timeout:      FetchTimeout,
			MaxBodyBytes: MaxDocumentBytes,
		}),
		clock:   o.Clock,
		log:     o.Logger,
		status:  o.Status,
		revoker: o.Revoker,
		wake:    make(chan struct{}, 1),
		entries: map[Source]*entry{},
	}
	if m.clock == nil {
		m.clock = clock.Real()
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	if m.revoker == nil {
		m.revoker = identity.NopRevoker{}
	}
	return m
}

// Name implements filter.Component.
func (m *Manager) Name() string { return componentName }

// Run fetches due key sets until ctx is done, then cancels the fetches in
// flight and returns after their goroutines exited. Only one Run may be
// active at a time (ErrRunning).
func (m *Manager) Run(ctx context.Context) error {
	if !m.running.CompareAndSwap(false, true) {
		return ErrRunning
	}
	defer m.running.Store(false)
	var wg sync.WaitGroup
	slots := make(chan struct{}, MaxConcurrentFetches)
	defer func() {
		wg.Wait()
		m.client.CloseIdleConnections()
	}()
	for {
		wait, timed := m.dispatch(ctx, &wg, slots)
		var (
			t  clock.Timer
			tc <-chan time.Time
		)
		if timed {
			t = m.clock.NewTimer(wait)
			tc = t.C()
		}
		select {
		case <-ctx.Done():
		case <-m.wake:
		case <-tc:
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// dispatch starts the due fetches that fit in the free slots, in due order,
// and returns the delay until the next entry not yet due (timed false when
// none). Due entries left without a slot wait for a completion's wake.
func (m *Manager) dispatch(ctx context.Context, wg *sync.WaitGroup, slots chan struct{}) (wait time.Duration, timed bool) {
	m.mu.Lock()
	now := m.clock.Now()
	var due []*entry
	var earliest time.Time
	for _, e := range m.entries {
		switch {
		case e.inflight:
		case !e.next.After(now):
			due = append(due, e)
		case earliest.IsZero() || e.next.Before(earliest):
			earliest = e.next
		}
	}
	slices.SortFunc(due, func(a, b *entry) int {
		return cmp.Or(a.next.Compare(b.next), cmp.Compare(a.src.Issuer, b.src.Issuer), cmp.Compare(a.src.URL, b.src.URL))
	})
	started := due[:0]
	for _, e := range due {
		if len(slots) == cap(slots) {
			break
		}
		slots <- struct{}{}
		e.inflight = true
		e.nudgeAt.Store(now.Add(RefreshInterval).UnixNano())
		started = append(started, e)
	}
	m.mu.Unlock()
	for _, e := range started {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.complete(ctx, e, m.fetch(ctx, e))
			<-slots
			m.signal()
		}()
	}
	if earliest.IsZero() {
		return 0, false
	}
	return earliest.Sub(now), true
}

// signal wakes Run without blocking.
func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// result is one fetch's outcome.
type result struct {
	keys       *Keys
	err        error
	retryAfter time.Time
}

// fetch gets and parses e's document.
func (m *Manager) fetch(ctx context.Context, e *entry) result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.src.URL, http.NoBody)
	if err != nil {
		return result{err: e.fetchError(err)}
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return result{err: e.fetchError(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return result{
			err:        e.fetchError(fmt.Errorf("status %d", resp.StatusCode)),
			retryAfter: retryAfter(resp.Header, m.clock.Now()),
		}
	}
	doc, err := io.ReadAll(resp.Body)
	if err != nil {
		return result{err: e.fetchError(err)}
	}
	ks, err := NewKeys(doc, e.src.Issuer, m.revoker, m.clock.Now(), Lifetime(resp.Header))
	if err != nil {
		return result{err: e.fetchError(err)}
	}
	return result{keys: ks}
}

// fetchError names the Source in a fetch failure.
func (e *entry) fetchError(err error) error {
	return fmt.Errorf("jwks: %s: %w", e.where, err)
}

// newEntry returns the entry of s, due for a fetch at now.
func newEntry(s Source, now time.Time) *entry {
	u := s.URL
	if p, err := url.Parse(s.URL); err == nil {
		u = p.Redacted()
	}
	return &entry{
		src:    s,
		where:  fmt.Sprintf("issuer %q from %s", s.Issuer, u),
		holder: "jwks " + s.Issuer + " " + u,
		next:   now,
		done:   make(chan struct{}),
	}
}

// complete records a fetch's outcome: a new key set refreshes at half its
// lifetime; a failure keeps the last set, raises jwks_stale and retries
// after RetryInterval or Retry-After, whichever is later. A fetch ended by
// Run's shutdown records nothing.
func (m *Manager) complete(ctx context.Context, e *entry, r result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.inflight = false
	if e.removed {
		return
	}
	switch {
	case ctx.Err() != nil:
		e.next = m.clock.Now()
	case r.err == nil:
		e.keys.Store(r.keys)
		e.next = r.keys.Fetched().Add(r.keys.Expires().Sub(r.keys.Fetched()) / 2)
		e.retryAfter = time.Time{}
		if e.failing {
			e.failing = false
			m.setStale(e, false)
		}
	default:
		now := m.clock.Now()
		e.retryAfter = r.retryAfter
		e.next = now.Add(RetryInterval)
		if r.retryAfter.After(e.next) {
			e.next = r.retryAfter
		}
		if !e.failing {
			e.failing = true
			m.setStale(e, true)
			m.log.WarnContext(ctx, "jwks fetch failed", slog.String(catalog.KeyComponent, componentName),
				slog.String(catalog.KeyError, r.err.Error()))
		} else {
			m.log.DebugContext(ctx, "jwks fetch failed again", slog.String(catalog.KeyComponent, componentName),
				slog.String(catalog.KeyError, r.err.Error()))
		}
	}
	close(e.done)
	e.done = make(chan struct{})
}

// setStale raises or clears jwks_stale for e. Callers hold m.mu.
func (m *Manager) setStale(e *entry, on bool) {
	if m.status != nil {
		m.status.SetDegraded(catalog.ReasonJWKSStale, e.holder, on)
	}
}

// Handle is one reference to a Source's key set, held by a Filter from
// Build to Close.
type Handle struct {
	m *Manager
	e *entry
	// wait is the fetch Acquire scheduled for a Source without usable
	// keys; nil when there is nothing to wait for.
	wait     <-chan struct{}
	released atomic.Bool
}

// Acquire returns one Handle per source, in order, each holding a
// reference until Release. A Source not yet known gets an entry; one
// without usable keys at the Manager's current time is fetched at once,
// or as soon as its Retry-After allows, and its Handle waits for that
// fetch (Wait).
func (m *Manager) Acquire(srcs []Source) []*Handle {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now()
	hs := make([]*Handle, len(srcs))
	wake := false
	for i, s := range srcs {
		e := m.entries[s]
		if e == nil {
			e = newEntry(s, now)
			m.entries[s] = e
		}
		e.refs++
		h := &Handle{m: m, e: e}
		if ks := e.keys.Load(); ks == nil || ks.Expired(now) {
			if !e.inflight && now.Before(e.next) && !now.Before(e.retryAfter) {
				e.next = now
			}
			if e.inflight || !now.Before(e.next) {
				h.wait = e.done
				wake = true
			}
		}
		hs[i] = h
	}
	if wake {
		m.signal()
	}
	return hs
}

// Round bounds the waits of one Revision compile: every Wait of the round
// ends by the same deadline.
type Round struct{ deadline time.Time }

// NewRound starts a round ending PrefetchTimeout from now.
func (m *Manager) NewRound() *Round { return &Round{deadline: m.clock.Now().Add(PrefetchTimeout)} }

// Wait blocks until the fetch each Handle waits for has completed, the
// round has ended or ctx is done; Handles with nothing to wait for return
// at once. The outcome of the fetches does not matter: the compile goes on
// either way.
func (m *Manager) Wait(ctx context.Context, r *Round, hs []*Handle) {
	var t clock.Timer
	defer func() {
		if t != nil {
			t.Stop()
		}
	}()
	for _, h := range hs {
		if h.wait == nil {
			continue
		}
		select {
		case <-h.wait:
			continue
		default:
		}
		if t == nil {
			d := r.deadline.Sub(m.clock.Now())
			if d <= 0 {
				return
			}
			t = m.clock.NewTimer(d)
		}
		tc := t.C()
		select {
		case <-h.wait:
		case <-tc:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Source returns the handle's Source.
func (h *Handle) Source() Source { return h.e.src }

// Keys returns the Source's current key set; nil before the first
// successful fetch. The set may have expired (Keys.Expired).
func (h *Handle) Keys() *Keys { return h.e.keys.Load() }

// Nudge asks for a fetch because a token named an unknown kid, or the
// issuer had no usable keys, at now: it schedules one at once unless a
// fetch began less than RefreshInterval ago, one is in flight, or the last
// response's Retry-After has not passed. It never blocks.
func (h *Handle) Nudge(now time.Time) {
	e := h.e
	if now.UnixNano() < e.nudgeAt.Load() {
		return
	}
	m := h.m
	m.mu.Lock()
	if e.removed || e.inflight || now.UnixNano() < e.nudgeAt.Load() || now.Before(e.retryAfter) {
		m.mu.Unlock()
		return
	}
	e.nudgeAt.Store(now.Add(RefreshInterval).UnixNano())
	if now.Before(e.next) {
		e.next = now
	}
	m.mu.Unlock()
	m.signal()
}

// Release drops the handle's reference; the last one removes the Source's
// keys, stops its refreshes and clears its jwks_stale. It is idempotent.
func (h *Handle) Release() {
	if !h.released.CompareAndSwap(false, true) {
		return
	}
	m, e := h.m, h.e
	m.mu.Lock()
	defer m.mu.Unlock()
	e.refs--
	if e.refs > 0 {
		return
	}
	if m.entries[e.src] == e {
		delete(m.entries, e.src)
	}
	e.removed = true
	close(e.done)
	e.done = make(chan struct{})
	if e.failing {
		e.failing = false
		m.setStale(e, false)
	}
}
