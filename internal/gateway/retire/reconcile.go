// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"time"
	"weak"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Run is the retirer (spec 04 reqs 51-53): it frees snapshots at zero
// pins, turns a closing snapshot into ending when its grace period ends and
// runs the ending protocol, and raises snapshot_ending_overdue for an
// ending snapshot still pinned past the ending bound. Unpin and Publish
// wake it at once; timers on the Holder's clock cover grace, the overdue
// bound and a fallback pin poll. Run returns nil when ctx ends, after every
// goroutine the Holder started has exited.
func (h *Holder) Run(ctx context.Context) error {
	defer h.waitTasks()
	for {
		next := h.reconcile(ctx)
		var (
			t    clock.Timer
			fire <-chan time.Time
		)
		if !next.IsZero() {
			t = h.clock.NewTimer(next.Sub(h.clock.Now()))
			fire = t.C()
		}
		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return nil
		case <-h.kick:
		case <-fire:
		}
		if t != nil {
			t.Stop()
		}
	}
}

// reconcile applies every due transition and returns the next time one
// can become due (zero for none). Walks it starts run with ctx.
func (h *Holder) reconcile(ctx context.Context) time.Time {
	now := h.clock.Now()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.freeLocked()
	advanced, next := h.advanceLocked(ctx, now)
	if advanced {
		h.broadcastLocked()
	}
	return next
}

// freeLocked frees every snapshot that is not active and has zero pins:
// Binding.Release at once (09 req 56), then a task closes the resources
// and releases the State Store handles no remaining live snapshot shares
// (spec 04 req 51). It updates the retired-snapshots gauge and wakes the
// waiters when it freed a snapshot.
//
// Zero pins proves no request runs on a snapshot that is no longer
// published: every valid pin was added before the swap, since its re-load
// saw the snapshot, and pins added after it are undone before use.
func (h *Holder) freeLocked() {
	active := h.cur.Load()
	var freed, kept []*tracked
	for _, t := range h.live {
		if t.s != active && t.s.Pins.Count() == 0 {
			freed = append(freed, t)
		} else {
			kept = append(kept, t)
		}
	}
	if len(freed) == 0 {
		return
	}
	h.live = kept
	for _, t := range freed {
		h.freed.add(t.s)
		if t.s.Binding != nil {
			t.s.Binding.Release()
		}
		h.log.Info("snapshot freed", catalog.KeyRevision, t.s.Revision.Digest.Short(), "state", t.state.String())
	}
	h.setDegradedLocked()
	closers, handles := unshared(freed, kept)
	if len(closers) > 0 || len(handles) > 0 {
		log := h.log
		h.startTaskLocked(func() { closeAll(log, closers, handles) })
	}
	h.updateGaugeLocked()
	h.broadcastLocked()
}

// advanceLocked moves a closing snapshot whose grace ended to ending and
// starts its ending walk, marks ending snapshots overdue, and returns
// whether anything changed plus the next due time.
func (h *Holder) advanceLocked(ctx context.Context, now time.Time) (bool, time.Time) {
	active := h.cur.Load()
	changed := false
	var next time.Time
	pinned := false
	for _, t := range h.live {
		switch t.state {
		case StateClosing:
			if now.Before(t.graceEndsAt) {
				next = earliest(next, t.graceEndsAt)
				break
			}
			t.state = StateEnding
			t.overdueAt = now.Add(h.endingBound)
			changed = true
			h.log.InfoContext(ctx, "snapshot ending", catalog.KeyRevision, t.s.Revision.Digest.Short(),
				"pins", t.s.Pins.Count())
			s := t.s
			h.startTaskLocked(func() { h.endPinned(ctx, s, snapshot.EndGrace) })
			next = earliest(next, t.overdueAt)
		case StateEnding:
			if t.overdue {
				break
			}
			if now.Before(t.overdueAt) {
				next = earliest(next, t.overdueAt)
				break
			}
			t.overdue = true
			changed = true
			h.log.WarnContext(ctx, "snapshot ending overdue", catalog.KeyRevision, t.s.Revision.Digest.Short(),
				"pins", t.s.Pins.Count(), catalog.KeyReason, catalog.ReasonSnapshotEndingOverdue.String())
		case StateActive, StateRetired:
		}
		if t.s != active && t.s.Pins.Count() > 0 {
			pinned = true
		}
	}
	if pinned {
		next = earliest(next, now.Add(h.poll))
	}
	h.setDegradedLocked()
	return changed, next
}

// setDegradedLocked raises snapshot_ending_overdue while an ending snapshot
// is overdue and clears it when none is (spec 04 req 52).
func (h *Holder) setDegradedLocked() {
	on := slices.ContainsFunc(h.live, func(t *tracked) bool { return t.overdue })
	if on == h.degraded {
		return
	}
	h.degraded = on
	if h.status != nil {
		h.status.SetDegraded(catalog.ReasonSnapshotEndingOverdue, statusSource, on)
	}
}

// earliest returns the earlier of a and b, treating zero as none.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

// unshared returns the resources and State Store handles of the freed
// snapshots that no kept snapshot references, each once. Resources and
// handles are compared by identity (pointer or comparable value); a value
// that cannot be compared is treated as unshared. A StoreHandle instance
// is therefore released once, when the last live snapshot holding it is
// freed, however many snapshots carried it over (package documentation).
func unshared(freed, kept []*tracked) ([]io.Closer, []snapshot.StoreHandle) {
	inUse := map[any]struct{}{}
	for _, t := range kept {
		for _, c := range t.s.Resources {
			if c != nil && hashable(c) {
				inUse[c] = struct{}{}
			}
		}
		for _, sh := range handlesOf(t.s) {
			if hashable(sh) {
				inUse[sh] = struct{}{}
			}
		}
	}
	seen := map[any]struct{}{}
	fresh := func(v any) bool {
		if !hashable(v) {
			return true
		}
		if _, ok := inUse[v]; ok {
			return false
		}
		if _, ok := seen[v]; ok {
			return false
		}
		seen[v] = struct{}{}
		return true
	}
	var closers []io.Closer
	var handles []snapshot.StoreHandle
	for _, t := range freed {
		// Close in reverse order: later resources may depend on earlier ones.
		for i := len(t.s.Resources) - 1; i >= 0; i-- {
			if c := t.s.Resources[i]; c != nil && fresh(c) {
				closers = append(closers, c)
			}
		}
		for _, sh := range handlesOf(t.s) {
			if fresh(sh) {
				handles = append(handles, sh)
			}
		}
	}
	return closers, handles
}

// handlesOf returns the non-nil State Store handles of s.
func handlesOf(s *snapshot.Snapshot) []snapshot.StoreHandle {
	var out []snapshot.StoreHandle
	for _, sh := range []snapshot.StoreHandle{s.StateStore, s.CacheStore} {
		if sh != nil {
			out = append(out, sh)
		}
	}
	return out
}

// hashable reports whether v can be a map key without panicking.
func hashable(v any) bool { return reflect.ValueOf(v).Comparable() }

// closeAll closes the resources, then releases the State Store handles
// (Filters may still use their store while closing). A panicking Close or
// Release (a typed-nil io.Closer, a broken Filter) is logged and the
// remaining resources and handles are still closed and released.
func closeAll(log *slog.Logger, closers []io.Closer, handles []snapshot.StoreHandle) {
	for _, c := range closers {
		contain(log, opResourceClose, func() {
			if err := c.Close(); err != nil {
				log.Warn("snapshot resource close failed", catalog.KeyError, err.Error())
			}
		})
	}
	for _, sh := range handles {
		contain(log, opHandleRelease, func() { sh.Release() })
	}
}

// Operations contain names in its log record.
const (
	opResourceClose = "snapshot resource close"
	opHandleRelease = "state store handle release"
	opClosingHook   = "snapshot closing hook"
)

// contain runs f and logs a panic instead of crashing the Node: resource
// Close, StoreHandle.Release and the OnClosing hook run other packages'
// code on goroutines the Holder owns. op names the operation.
func contain(log *slog.Logger, op string, f func()) {
	defer func() {
		if v := recover(); v != nil {
			log.Error("retirement operation panicked", "operation", op, catalog.KeyError, fmt.Sprint(v))
		}
	}()
	f()
}

// tombstones remembers the freed snapshots that are still reachable, so
// Publish rejects one instead of serving closed resources (and the Pin
// re-load never sees a freed pointer again). It holds weak pointers only:
// the runtime drops an entry once its snapshot is collected, so the set
// never keeps a snapshot alive and stays as small as the snapshots some
// caller still references.
type tombstones struct {
	mu  sync.Mutex
	set map[weak.Pointer[snapshot.Snapshot]]struct{}
}

// add records the freed snapshot s.
func (ts *tombstones) add(s *snapshot.Snapshot) {
	wp := weak.Make(s)
	ts.mu.Lock()
	if ts.set == nil {
		ts.set = map[weak.Pointer[snapshot.Snapshot]]struct{}{}
	}
	ts.set[wp] = struct{}{}
	ts.mu.Unlock()
	runtime.AddCleanup(s, ts.drop, wp)
}

// drop forgets a collected snapshot; it runs on a runtime cleanup
// goroutine.
func (ts *tombstones) drop(wp weak.Pointer[snapshot.Snapshot]) {
	ts.mu.Lock()
	delete(ts.set, wp)
	ts.mu.Unlock()
}

// has reports whether s was freed.
func (ts *tombstones) has(s *snapshot.Snapshot) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	_, ok := ts.set[weak.Make(s)]
	return ok
}

// count returns the number of remembered snapshots.
func (ts *tombstones) count() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.set)
}
