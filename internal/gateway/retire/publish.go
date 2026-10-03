// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Publish errors. None reaches a client: activation logs them and keeps
// the running snapshot.
var (
	// ErrNilSnapshot rejects a nil snapshot.
	ErrNilSnapshot = errors.New("retire: nil snapshot")
	// ErrPublished rejects a snapshot that is live or was freed: every
	// Publish takes a freshly compiled snapshot.
	ErrPublished = errors.New("retire: snapshot already published")
	// ErrBusy rejects an activation while K snapshots are retired and one
	// is closing or ending (spec 04 req 54): grace is never cut short.
	ErrBusy = errors.New("retire: retirement bound reached")
	// ErrClosed rejects a Publish after Close.
	ErrClosed = errors.New("retire: holder closed")
)

// Publish makes next the active snapshot with one atomic pointer swap and
// retires the previous one, which it returns (nil for the first). Set
// next.Binding before the call; the previous snapshot's Binding.Retire runs
// here (09 req 56). When the retirement makes K + 1 retired snapshots, the
// oldest becomes closing: its streams end (OnClosing) and its grace period
// starts (spec 04 req 52). next.Pins is created with Stripes() stripes when
// nil. Publish fails with ErrBusy when CanActivate is false, so the
// activation loader waits first (WaitActivate).
//
// next must be freshly compiled: Publish fails with ErrPublished for a
// snapshot that is live or was freed, whose resources are closed and whose
// State Store handles and Binding are released. A StoreHandle instance
// carried over from a live snapshot is released once, when the last live
// snapshot holding it is freed, so compile must not Retain a carried-over
// instance for next (package documentation).
func (h *Holder) Publish(next *snapshot.Snapshot) (*snapshot.Snapshot, error) {
	if next == nil {
		return nil, ErrNilSnapshot
	}
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	if h.findLocked(next) != nil || h.freed.has(next) {
		return nil, ErrPublished
	}
	h.freeLocked()
	if !h.canActivateLocked() {
		return nil, ErrBusy
	}
	if next.Pins == nil {
		next.Pins = snapshot.NewPins(h.stripes)
	}
	prev := h.cur.Swap(next)
	h.live = append(h.live, &tracked{s: next, state: StateActive, activatedAt: now})
	h.log.Info("snapshot published", catalog.KeyRevision, next.Revision.Digest.Short())
	if prev != nil {
		h.retireLocked(prev, now)
	}
	h.updateGaugeLocked()
	h.broadcastLocked()
	h.wake()
	return prev, nil
}

// retireLocked moves the active snapshot s to retired, runs its
// Binding.Retire and, over K retired, makes the oldest retired snapshot
// closing.
func (h *Holder) retireLocked(s *snapshot.Snapshot, now time.Time) {
	t := h.findLocked(s)
	if t == nil {
		return
	}
	t.state = StateRetired
	t.retiredAt = now
	if s.Binding != nil {
		s.Binding.Retire()
	}
	h.log.Info("snapshot retired", catalog.KeyRevision, s.Revision.Digest.Short(), "pins", s.Pins.Count())
	if h.countLocked(StateRetired) <= h.k {
		return
	}
	for _, o := range h.live {
		if o.state != StateRetired {
			continue
		}
		o.state = StateClosing
		o.graceEndsAt = now.Add(h.grace)
		h.log.Info("snapshot closing", catalog.KeyRevision, o.s.Revision.Digest.Short(),
			"pins", o.s.Pins.Count(), "grace_ends_at", o.graceEndsAt)
		if h.onClosing != nil {
			closing, hook, log := o.s, h.onClosing, h.log
			h.startTaskLocked(func() { contain(log, opClosingHook, func() { hook(closing) }) })
		}
		return
	}
}

// CanActivate reports whether Publish would accept a new snapshot now:
// fewer than K snapshots are retired, or none is closing or ending (spec 04
// req 54). A snapshot at zero pins still counts until the retirer frees it,
// which Run does at once.
func (h *Holder) CanActivate() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.closed && h.canActivateLocked()
}

// canActivateLocked is the activation gate of spec 04 req 54, test plan
// item 12 and architecture 3.3 step 2: fewer than K retired, or none
// closing or ending. With "and" the activation that makes a (K + 1)th
// retirement could never run, so no snapshot would become closing (req
// 52, architecture 3.3 step 9).
func (h *Holder) canActivateLocked() bool {
	return h.countLocked(StateRetired) < h.k || h.countLocked(StateClosing)+h.countLocked(StateEnding) == 0
}

// WaitActivate blocks until CanActivate holds, freeing snapshots that
// reached zero pins on the way; the activation loader calls it before it
// compiles its latest pending Revision, so grace is never cut short (spec
// 04 req 54). It returns ErrClosed after Close and the context's error when
// ctx ends first.
func (h *Holder) WaitActivate(ctx context.Context) error {
	for {
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			return ErrClosed
		}
		h.freeLocked()
		ok := h.canActivateLocked()
		ch := h.changed
		h.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("retire: wait to activate: %w", ctx.Err())
		case <-ch:
		}
	}
}

// Counts is the number of live snapshots per state.
type Counts struct {
	// Active is true while a snapshot is published.
	Active                   bool
	Retired, Closing, Ending int
}

// Counts returns the live snapshots per state.
func (h *Holder) Counts() Counts {
	h.mu.Lock()
	defer h.mu.Unlock()
	return Counts{
		Active:  h.cur.Load() != nil,
		Retired: h.countLocked(StateRetired),
		Closing: h.countLocked(StateClosing),
		Ending:  h.countLocked(StateEnding),
	}
}

// Close retires the active snapshot (Binding.Retire), so Pin returns nil
// from then on and Publish fails with ErrClosed, then frees every snapshot
// as it reaches zero pins and waits for the Holder's goroutines. The
// supervisor calls it after EndAll at the Drain deadline, before it
// cancels the Node-wide filter.Components (architecture 3.7); EndAll first
// ends every pinned request and every later pin, so Close does not wait
// for work that would otherwise run until its own context ends. It returns
// ctx's error when snapshots are still pinned or goroutines still run when
// ctx ends; calling it again resumes the wait.
func (h *Holder) Close(ctx context.Context) error {
	now := h.clock.Now()
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		if prev := h.cur.Swap(nil); prev != nil {
			if t := h.findLocked(prev); t != nil {
				t.state = StateRetired
				t.retiredAt = now
				if prev.Binding != nil {
					prev.Binding.Retire()
				}
				h.log.InfoContext(ctx, "snapshot retired", catalog.KeyRevision, prev.Revision.Digest.Short(),
					"pins", prev.Pins.Count())
			}
		}
		h.updateGaugeLocked()
		h.broadcastLocked()
	}
	h.mu.Unlock()
	for {
		h.reconcile(ctx)
		h.mu.Lock()
		done := len(h.live) == 0 && h.tasks == 0
		ch := h.changed
		h.mu.Unlock()
		if done {
			return nil
		}
		t := h.clock.NewTimer(h.poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("retire: close: %w", ctx.Err())
		case <-ch:
		case <-h.kick:
		case <-t.C():
		}
		t.Stop()
	}
}

// findLocked returns the live record of s, or nil.
func (h *Holder) findLocked(s *snapshot.Snapshot) *tracked {
	for _, t := range h.live {
		if t.s == s {
			return t
		}
	}
	return nil
}

// countLocked returns the live snapshots in state st.
func (h *Holder) countLocked(st State) int {
	n := 0
	for _, t := range h.live {
		if t.state == st {
			n++
		}
	}
	return n
}

// updateGaugeLocked sets ruralz_config_retired_snapshots to the live
// snapshots that are not active.
func (h *Holder) updateGaugeLocked() {
	n := len(h.live)
	if h.cur.Load() != nil {
		n--
	}
	h.retiredG.Set(int64(max(n, 0)))
}

// broadcastLocked wakes every WaitActivate, Close and task waiter.
func (h *Holder) broadcastLocked() {
	close(h.changed)
	h.changed = make(chan struct{})
}

// startTaskLocked runs f on a goroutine the Holder owns; Run and Close
// wait for it.
func (h *Holder) startTaskLocked(f func()) {
	h.tasks++
	go func() {
		defer h.taskDone()
		f()
	}()
}

func (h *Holder) taskDone() {
	h.mu.Lock()
	h.tasks--
	h.broadcastLocked()
	h.mu.Unlock()
	h.wake()
}

// waitTasks blocks until no task runs.
func (h *Holder) waitTasks() {
	for {
		h.mu.Lock()
		n, ch := h.tasks, h.changed
		h.mu.Unlock()
		if n == 0 {
			return
		}
		<-ch
	}
}
