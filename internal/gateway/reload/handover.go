// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"errors"
	"fmt"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/handover"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Hold waits until no activation runs, its candidate write included, then
// holds every later activation off until Release, and returns the active
// Revision and the candidate lkg/ names (handover.Activations): from a
// handover's accepted until its Drain the holder activates nothing, so a
// source change waits for the new process. Loads still arrive meanwhile
// and the latest becomes the pending Revision. It returns ctx's error,
// holding nothing, when ctx ends first.
func (l *Loader) Hold(ctx context.Context) (handover.Revisions, error) {
	l.mu.Lock()
	l.holds++
	if err := l.waitIdleLocked(ctx); err != nil {
		l.mu.Lock()
		l.holds--
		l.mu.Unlock()
		l.wake()
		return handover.Revisions{}, fmt.Errorf("reload: hold: %w", err)
	}
	revs := handover.Revisions{Active: l.active.Digest, Candidate: l.lkgCandidate}
	l.mu.Unlock()
	return revs, nil
}

// Release ends a Hold: activations run again, a pending one included.
func (l *Loader) Release() {
	l.mu.Lock()
	if l.holds > 0 {
		l.holds--
	}
	l.mu.Unlock()
	l.wake()
}

// Reload boots the holder's newer candidate of digest d through the full
// gate, replacing the Revision booted before and rebinding as it needs,
// so Active returns d (handover.SuccessorConfig.Reload). It runs as the
// loop's job once the boot ended, and returns the candidate's rejection
// as a *RejectedError, ErrStopped after Stop, or ctx's error when ctx
// ends first.
func (l *Loader) Reload(ctx context.Context, d revision.Digest) error {
	req := &request{digest: d, done: make(chan error, 1)}
	l.mu.Lock()
	if l.stopped {
		l.mu.Unlock()
		return ErrStopped
	}
	l.requests = append(l.requests, req)
	l.mu.Unlock()
	l.wake()
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("reload: %w", ctx.Err())
	}
}

// TookLock tells a handover successor's loader that the process took the
// data-directory lock: it watches the source from now on, which loads it
// at once and activates it when its digest differs from the active one,
// and writes the active Revision under lkg/ when lkg/ names another.
func (l *Loader) TookLock() {
	l.mu.Lock()
	l.lockTaken = true
	l.mu.Unlock()
	l.wake()
}

// Bound returns a handover successor's bound sockets
// (handover.Bound): Listen commits the switches its activations hold and
// makes every Listener Part listen; Close aborts them and stops every
// Stopper Part.
func (l *Loader) Bound() *Bound { return &Bound{l: l} }

// Bound is a handover successor's sockets, bound and not listening until
// its handover is accepted.
type Bound struct{ l *Loader }

// Listen makes every bound socket listen and serve, still pinned to the
// holder: it waits for any activation to end, commits the switches the
// last one holds, calls every Listener Part, and reports the listeners
// bound to /readyz. Later activations switch at once. A failure is
// returned after every switch was tried.
func (b *Bound) Listen(ctx context.Context) error {
	l := b.l
	l.mu.Lock()
	if err := l.waitIdleLocked(ctx); err != nil {
		return fmt.Errorf("reload: listen: %w", err)
	}
	held := l.held
	l.held, l.deferred = nil, false
	active := !l.active.Digest.IsZero()
	l.mu.Unlock()
	var errs []error
	var unbound []readiness.Issue
	for _, sw := range held {
		if err := sw.Commit(ctx); err != nil {
			errs = append(errs, err)
			if code, _ := errcode.CodeOf(err); code == codeBind {
				unbound = append(unbound, readiness.Issue{Code: code, Detail: err.Error()})
			}
		}
	}
	for _, p := range l.cfg.Parts {
		if ln, ok := p.(Listener); ok {
			if err := ln.Listen(ctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if active {
		l.cfg.Readiness.SetListeners(unbound...)
		l.syncStatus(ctx)
	}
	if err := errors.Join(errs...); err != nil {
		l.log.WarnContext(ctx, "handover successor sockets did not all listen", catalog.KeyError, err.Error())
		return fmt.Errorf("reload: listen: %w", err)
	}
	return nil
}

// Close closes every socket, bound or listening, when the new process
// exits without serving: the held switches abort and every Stopper Part
// stops accepting.
func (b *Bound) Close() error {
	l := b.l
	l.mu.Lock()
	held := l.held
	l.held = nil
	l.mu.Unlock()
	abortAll(held)
	for _, p := range l.cfg.Parts {
		if s, ok := p.(Stopper); ok {
			s.StopAccepting()
		}
	}
	return nil
}
