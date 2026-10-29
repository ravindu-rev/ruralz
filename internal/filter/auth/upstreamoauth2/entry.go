// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// msgFetchFailed is the warning each failed fetch logs.
const msgFetchFailed = "upstream token fetch failed"

// Waiting and lifecycle failures.
var (
	errHoldoff = errors.New("token endpoint failed less than the minimum fetch interval ago")
	errWait    = errors.New("request ended while waiting for the token endpoint")
	errStopped = errors.New("token registry stopped")
	errClosed  = errors.New("token registry entry closed")
)

// entryKey identifies an entry: one per Policy name and fetch parameters
// (Security and identity, "Upstream authentication": one single-flight
// fetch per Policy and Node).
type entryKey struct {
	policy string
	settings
}

// holder is one live Filter's view of its snapshot: the secret table the
// fetches read and the telemetry they record into. The newest holder of an
// entry serves its fetches.
type holder struct {
	secrets  secret.Store
	log      *slog.Logger
	age      emit.Gauge
	failures emit.Counter
}

// setAge records a token age in whole seconds.
func (h *holder) setAge(age time.Duration) { h.age.Set(int64(age / time.Second)) }

// flight is one fetch: pending until Run starts it, then running. done
// closes once the result is set; tok and err are read only after that.
type flight struct {
	done    chan struct{}
	started bool               // guarded by entry.mu
	cancel  context.CancelFunc // set when started; guarded by entry.mu
	tok     *token
	err     error
}

func newFlight() *flight { return &flight{done: make(chan struct{})} }

// settle publishes the result; the caller holds entry.mu, and each flight
// settles once.
func (f *flight) settle(tok *token, err error) {
	f.tok, f.err = tok, err
	close(f.done)
}

// entry is the token state of one Policy on this Node.
type entry struct {
	reg *Registry
	key entryKey
	// form is the request body; client the destination-bound client.
	form   string
	client *http.Client

	// tok is the latest token, read lock-free on the request path; it is
	// replaced only by a successful fetch.
	tok atomic.Pointer[token]

	mu sync.Mutex
	// holders are the live Filters, oldest first.
	holders []*holder
	// flight is the pending or running fetch; nil when none.
	flight *flight
	// refreshAt is when Run starts the next background fetch; zero when
	// none is scheduled (no token, or it expires first).
	refreshAt time.Time
	// lastStart is when the latest fetch started; lastErr its failure,
	// nil after a success.
	lastStart time.Time
	lastErr   error
	// closed is set when the last holder released the entry.
	closed bool
}

// newEntry returns the entry of k with its request body and its client:
// bound to the tokenUrl origin, following no redirect, capped at
// MaxResponseBytes and bounded by the timeout.
func newEntry(r *Registry, k entryKey) *entry {
	form := url.Values{"grant_type": {grantClientCredentials}}
	if k.scope != "" {
		form.Set("scope", k.scope)
	}
	return &entry{
		reg:  r,
		key:  k,
		form: form.Encode(),
		client: r.guard.Client(egress.ClientOptions{
			TLS:          r.tls,
			Destination:  k.origin,
			Timeout:      k.timeout,
			MaxBodyBytes: MaxResponseBytes,
		}),
	}
}

// get returns a token valid now and the Registry clock's now: the held
// token without a lock or a wait, else the result of the fetch a miss
// joins or starts, waited for within ctx.
func (e *entry) get(ctx context.Context) (*token, time.Time, error) {
	now := e.reg.clock.Now()
	if t := e.tok.Load(); t != nil && now.Before(t.expires) {
		return t, now, nil
	}
	return e.miss(ctx)
}

// miss joins the entry's fetch or creates one for Run to start, then waits
// for its result or the end of ctx. Within MinFetchInterval of a failed
// fetch, and once Run returned, it fails at once.
func (e *entry) miss(ctx context.Context) (*token, time.Time, error) {
	e.mu.Lock()
	now := e.reg.clock.Now()
	if t := e.tok.Load(); t != nil && now.Before(t.expires) {
		e.mu.Unlock()
		return t, now, nil
	}
	f := e.flight
	if f == nil {
		var err error
		switch {
		case e.closed:
			err = errClosed
		case e.reg.state.Load() == stateStopped:
			err = errStopped
		case e.lastErr != nil && now.Before(e.lastStart.Add(MinFetchInterval)):
			err = fmt.Errorf("%w: %w", errHoldoff, e.lastErr)
		}
		if err != nil {
			e.mu.Unlock()
			return nil, now, err
		}
		f = newFlight()
		e.flight = f
		e.mu.Unlock()
		e.reg.kick()
	} else {
		e.mu.Unlock()
	}
	select {
	case <-f.done:
		if f.err != nil {
			return nil, now, f.err
		}
		return f.tok, e.reg.clock.Now(), nil
	case <-ctx.Done():
		return nil, now, fmt.Errorf("%w: %w", errWait, context.Cause(ctx))
	}
}

// dispatch starts the entry's pending fetch, or its refresh when due at
// now; it returns the refresh time when it is later, else zero.
func (e *entry) dispatch(ctx context.Context, fetches *sync.WaitGroup, now time.Time) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.closed:
	case e.flight != nil:
		if !e.flight.started {
			e.startLocked(ctx, fetches, e.flight, now)
		}
	case e.refreshAt.IsZero():
	case now.Before(e.refreshAt):
		return e.refreshAt
	default:
		e.flight = newFlight()
		e.startLocked(ctx, fetches, e.flight, now)
	}
	return time.Time{}
}

// startLocked runs f on a goroutine owned by Run (fetches), bounded by the
// timeout, with the newest holder's secret table; the caller holds e.mu
// and e is not closed, so it has a holder.
func (e *entry) startLocked(ctx context.Context, fetches *sync.WaitGroup, f *flight, now time.Time) {
	h := e.holders[len(e.holders)-1]
	fctx, cancel := context.WithTimeout(ctx, e.key.timeout)
	f.started, f.cancel = true, cancel
	e.lastStart, e.refreshAt = now, time.Time{}
	fetches.Go(func() {
		defer cancel()
		tok, err := e.fetchRecovered(fctx, h, now)
		e.finish(ctx, f, tok, err, now)
	})
}

// fetchRecovered is fetch with a panic turned into a failed fetch, so a
// fetch goroutine never takes the Node down.
func (e *entry) fetchRecovered(ctx context.Context, h *holder, start time.Time) (tok *token, err error) {
	defer func() {
		if p := recover(); p != nil {
			tok, err = nil, fmt.Errorf("%w: %T", errPanic, p)
		}
	}()
	return e.fetch(ctx, h, start)
}

// finish publishes the result of f, started at start, schedules the next
// fetch and records it: a success replaces the token and schedules the
// refresh at two thirds of its lifetime; a failure keeps the token and
// schedules the retry (retryTime). A token already expired counts as a
// failure. A closed entry keeps nothing, and a failure after ctx, Run's
// context, ended is the shutdown's, not the endpoint's: it is neither
// recorded nor scheduled.
func (e *entry) finish(ctx context.Context, f *flight, tok *token, err error, start time.Time) {
	now := e.reg.clock.Now()
	if err == nil && !now.Before(tok.expires) {
		tok, err = nil, errExpired
	}
	stopping := ctx.Err() != nil
	e.mu.Lock()
	if e.flight == f {
		e.flight = nil
	}
	closed := e.closed
	var h *holder
	if !closed {
		h = e.holders[len(e.holders)-1]
		switch {
		case err == nil:
			e.tok.Store(tok)
			e.lastErr = nil
			e.refreshAt = refreshTime(start, tok.expires)
		case !stopping:
			e.lastErr = err
			e.refreshAt = retryTime(now, start, e.tok.Load())
		}
	}
	held := e.tok.Load()
	f.settle(tok, err)
	e.mu.Unlock()
	e.reg.kick()
	switch {
	case closed:
		e.client.CloseIdleConnections()
	case err == nil:
		h.setAge(now.Sub(tok.obtained))
	case !stopping:
		h.failures.Add(0, 1)
		h.log.WarnContext(ctx, msgFetchFailed, catalog.KeyPolicy, e.key.policy, catalog.KeyError, err.Error())
		if held != nil && now.Before(held.expires) {
			h.setAge(now.Sub(held.obtained))
		}
	}
	if e.reg.fetchHook != nil {
		e.reg.fetchHook()
	}
}

// abandon fails a fetch Run has not started (Run is returning).
func (e *entry) abandon(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f := e.flight; f != nil && !f.started {
		e.flight = nil
		f.settle(nil, err)
	}
}

// refreshTime returns when a token obtained at start and expiring at
// expires is refreshed: at two thirds of its lifetime (target), at least
// MinFetchInterval after start.
func refreshTime(start, expires time.Time) time.Time {
	life := expires.Sub(start)
	return start.Add(max(life-life/3, MinFetchInterval))
}

// retryTime returns when a failed fetch started at start and ended at now
// is retried while the held token cur is still valid: at two thirds of
// its remaining lifetime, at least MinFetchInterval after start. It is
// zero when cur is nil or expires first, since the next miss then fetches.
func retryTime(now, start time.Time, cur *token) time.Time {
	if cur == nil || !now.Before(cur.expires) {
		return time.Time{}
	}
	left := cur.expires.Sub(now)
	at := now.Add(left - left/3)
	if floor := start.Add(MinFetchInterval); at.Before(floor) {
		at = floor
	}
	if !at.Before(cur.expires) {
		return time.Time{}
	}
	return at
}
