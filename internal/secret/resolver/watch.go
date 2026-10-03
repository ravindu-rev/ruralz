// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// errWatcherPanicked is the reason recorded for a watcher that panicked.
var errWatcherPanicked = errors.New("watcher panicked")

// watcher is one Node-wide registration for rotations of a Ref (R-55).
type watcher struct {
	ref    secret.Ref
	fn     func(secret.Value) error
	source string // degraded-reason source

	// seen is the last version delivered (or current at registration); it
	// is written at registration and then only by the resolver goroutine.
	seen uint64

	mu      sync.Mutex
	stopped bool // guarded by mu
	failed  bool // the last delivery failed; guarded by mu
}

// watch registers fn for ref with seen as the version it already knows
// and returns its stop function. A nil fn registers nothing.
func (r *Resolver) watch(ref secret.Ref, fn func(secret.Value) error, seen uint64) (stop func()) {
	if fn == nil {
		return func() {}
	}
	w := &watcher{ref: ref, fn: fn, seen: seen}
	r.watchMu.Lock()
	r.watcherID++
	w.source = "secret " + ref.String() + " watch " + strconv.FormatUint(r.watcherID, 10)
	r.watches[ref] = append(slices.Clip(r.watches[ref]), w)
	r.watchMu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { r.unwatch(w) }) }
}

// unwatch removes w and clears its degraded source.
func (r *Resolver) unwatch(w *watcher) {
	r.watchMu.Lock()
	ws := slices.DeleteFunc(slices.Clone(r.watches[w.ref]), func(x *watcher) bool { return x == w })
	if len(ws) == 0 {
		delete(r.watches, w.ref)
	} else {
		r.watches[w.ref] = ws
	}
	r.watchMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	if w.failed {
		w.failed = false
		r.setDegraded(w.source, false)
	}
}

// watchersOf returns the registrations of ref; the slice is never
// modified in place.
func (r *Resolver) watchersOf(ref secret.Ref) []*watcher {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	return r.watches[ref]
}

// watchCount returns the number of live registrations (tests).
func (r *Resolver) watchCount() int {
	r.watchMu.Lock()
	defer r.watchMu.Unlock()
	n := 0
	for _, ws := range r.watches {
		n += len(ws)
	}
	return n
}

// fanout delivers, for every reference st holds, the current value to
// each registration that has not seen its version, whichever Store the
// registration was made on (R-55). It runs on the resolver goroutine,
// without holding any Resolver lock while a watcher runs.
func (r *Resolver) fanout(ctx context.Context, st *store) {
	for _, ref := range st.refs {
		ws := r.watchersOf(ref)
		if len(ws) == 0 {
			continue
		}
		cv := st.value(ref, st.cells[ref])
		for _, w := range ws {
			if w.seen < cv.version {
				r.deliver(ctx, w, cv)
			}
		}
	}
}

// deliver calls w with cv. An error or a panic keeps the watcher's last
// value: it counts a rotation failure, logs a warning and raises
// secret_rotation_failed until the watcher accepts a later value or
// stops. The version is marked seen either way, so one bad value is
// counted once.
func (r *Resolver) deliver(ctx context.Context, w *watcher, cv *cellValue) {
	w.mu.Lock()
	stopped := w.stopped
	w.mu.Unlock()
	if stopped {
		return
	}
	err := callWatcher(w.fn, cv.val)
	w.seen = cv.version
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	if err == nil {
		if w.failed {
			w.failed = false
			r.setDegraded(w.source, false)
		}
		return
	}
	if !errors.Is(err, errWatcherPanicked) {
		b := cv.val.Reveal()
		err = sanitize(err, b)
		clear(b)
	}
	provider := string(w.ref.Provider)
	r.failureCounter(provider).Add(0, 1)
	r.log.WarnContext(ctx, msgWatcherFailed,
		slog.String(catalog.KeyProvider, provider),
		slog.String(catalog.KeyReference, w.ref.String()),
		slog.String(catalog.KeyError, err.Error()))
	if !w.failed {
		w.failed = true
		r.setDegraded(w.source, true)
	}
}

// callWatcher runs fn, turning a panic into errWatcherPanicked so one
// faulty watcher cannot stop the resolver goroutine.
func callWatcher(fn func(secret.Value) error, v secret.Value) (err error) {
	defer func() {
		if recover() != nil {
			err = errWatcherPanicked
		}
	}()
	return fn(v)
}
