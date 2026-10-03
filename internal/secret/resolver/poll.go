// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Log messages (static, sloglint).
const (
	msgRotated        = "secret rotated"
	msgRotationFailed = "secret rotation failed"
	msgWatcherFailed  = "secret watcher rejected a rotation"
)

// poll examines the active Store's file references and returns the Store
// it polled (spec 01 requirement 46): each file is stat'ed and re-read
// only when its fingerprint (size, modification time, inode, mode, owner)
// changed or the last read was too close to a change to trust it. A new
// value that passes every use check of the active Store replaces the
// value at a new version (a reference whose last examination failed is
// checked even when the file holds its value again); a failure keeps the
// last value, counts
// ruralz_config_secret_rotation_failures_total{provider="file"} once per
// distinct failed file state, logs a warning and raises
// secret_rotation_failed until the reference reads well again or leaves
// the active Store.
func (r *Resolver) poll(ctx context.Context) *store {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.active.Load()
	if len(st.files) > 0 {
		r.pollFilesLocked(ctx, st)
	}
	r.syncDegradedLocked(st)
	return st
}

// pollFilesLocked polls every file group of st.
func (r *Resolver) pollFilesLocked(ctx context.Context, st *store) {
	now := r.clock.Now()
	root, err := r.openRoot()
	if err != nil {
		fp := failedFingerprint(err)
		for _, g := range st.files {
			for _, ref := range g.refs {
				if c := st.cells[ref]; c.racy || c.fp != fp {
					r.failLocked(ctx, c, err, fp, now)
				}
			}
		}
		return
	}
	defer func() { _ = root.Close() }()
	resolved := r.resolvedRoot()
	for _, g := range st.files {
		if ctx.Err() != nil {
			return
		}
		r.pollGroupLocked(ctx, st, root, resolved, g, now)
	}
}

// pollGroupLocked polls the references of one file.
func (r *Resolver) pollGroupLocked(ctx context.Context, st *store, root *os.Root, resolved string, g fileGroup, now time.Time) {
	rel, err := r.relative(g.name, resolved)
	var fp fingerprint
	if err != nil {
		fp = failedFingerprint(err)
	} else {
		fp, err = statFile(root, rel)
	}
	var due []*cell
	for _, ref := range g.refs {
		if c := st.cells[ref]; c.racy || c.fp != fp {
			due = append(due, c)
		}
	}
	if len(due) == 0 {
		return
	}
	if err != nil {
		for _, c := range due {
			r.failLocked(ctx, c, err, fp, now)
		}
		return
	}
	content, rfp, err := r.readFile(root, rel, g.limit)
	if err != nil {
		for _, c := range due {
			r.failLocked(ctx, c, err, rfp, now)
		}
		return
	}
	defer clear(content)
	for _, c := range due {
		r.refreshLocked(ctx, st, c, content, rfp, now)
	}
}

// refreshLocked applies the file content read in state fp to c.
func (r *Resolver) refreshLocked(ctx context.Context, st *store, c *cell, content []byte, fp fingerprint, now time.Time) {
	val, owned, err := valueOf(content, c.ref.Key)
	if err != nil {
		r.failLocked(ctx, c, err, fp, now)
		return
	}
	if owned {
		defer clear(val)
	}
	cur := c.load().val.Reveal()
	same := bytes.Equal(cur, val)
	clear(cur)
	if same && !c.failed {
		c.examined(fp, now)
		return
	}
	// A failed cell holding the file's value again is checked too: the
	// value may be one the active Store's uses refuse (recheckLocked).
	if err := st.check(c.ref, val); err != nil {
		r.failLocked(ctx, c, err, fp, now)
		return
	}
	if same {
		c.examined(fp, now)
		return
	}
	c.set(val, r.nextVersionLocked())
	c.examined(fp, now)
	r.log.InfoContext(ctx, msgRotated,
		slog.String(catalog.KeyProvider, string(c.ref.Provider)),
		slog.String(catalog.KeyReference, c.ref.String()))
}

// failLocked records a failed examination of c in state fp: the value is
// kept; the failure is counted and logged unless it repeats the last
// failure of the same state.
func (r *Resolver) failLocked(ctx context.Context, c *cell, err error, fp fingerprint, now time.Time) {
	repeated := c.failed && c.fp == fp
	c.fp = fp
	c.racy = fp.racyAt(now)
	c.failed = true
	if repeated {
		return
	}
	provider := string(c.ref.Provider)
	r.failureCounter(provider).Add(0, 1)
	r.log.WarnContext(ctx, msgRotationFailed,
		slog.String(catalog.KeyProvider, provider),
		slog.String(catalog.KeyReference, c.ref.String()),
		slog.String(catalog.KeyError, err.Error()))
}

// syncDegradedLocked raises secret_rotation_failed for every reference of
// st whose last examination failed and clears it for every other one.
func (r *Resolver) syncDegradedLocked(st *store) {
	for _, g := range st.files {
		for _, ref := range g.refs {
			if st.cells[ref].failed && !r.raised[ref] {
				r.raised[ref] = true
				r.setDegraded(pollSource(ref), true)
			}
		}
	}
	for ref := range r.raised {
		if c, ok := st.cells[ref]; !ok || !c.failed {
			delete(r.raised, ref)
			r.setDegraded(pollSource(ref), false)
		}
	}
}

// setDegraded raises or clears secret_rotation_failed for source.
func (r *Resolver) setDegraded(source string, on bool) {
	if r.status != nil {
		r.status.SetDegraded(catalog.ReasonSecretRotationFailed, source, on)
	}
}

// pollSource is the degraded-reason source of a reference's poll.
func pollSource(ref secret.Ref) string { return "secret " + ref.String() }
