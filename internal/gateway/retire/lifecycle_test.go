// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Tests for spec 04 req 51 (only resources close, at zero pins, unless a
// newer snapshot shares them), req 52 (K = 2 retired, one closing or
// ending, 30 s grace, RZ-RT-014 counted, snapshot_ending_overdue after the
// ending bound, ruralz_config_retired_snapshots), req 54 (activation waits
// while K are retired and one is closing or ending) and 09 req 56 with
// R-58 (Binding.Retire at retirement, Binding.Release at zero pins); test
// plan item 12, all on clocktest.Fake.

// TestReq52KTransitionsGraceAndEnding walks one snapshot through retired,
// closing (third retirement), 30 s grace, ending (RZ-RT-014 marking and
// counting) and freed.
func TestReq52KTransitionsGraceAndEnding(t *testing.T) {
	x := newHarness(t, Config{}, true)
	a, b, c, d := newSnap("a", x.log), newSnap("b", x.log), newSnap("c", x.log), newSnap("d", x.log)

	x.publish(a)
	pa := x.pin(0)
	x.publish(b)
	pb := x.pin(1)
	x.publish(c)
	pc := x.pin(2)
	if got := x.h.Counts(); got != (Counts{Active: true, Retired: 2}) {
		t.Fatalf("counts after c = %+v, want 2 retired", got)
	}
	if !x.h.CanActivate() {
		t.Fatal("CanActivate with 2 retired and none closing = false, want true (req 54)")
	}

	x.publish(d)
	if got := x.h.Counts(); got != (Counts{Active: true, Retired: 2, Closing: 1}) {
		t.Fatalf("counts after the third retirement = %+v, want 2 retired and 1 closing", got)
	}
	if x.state(a) != int(StateClosing) {
		t.Fatalf("oldest snapshot state = %d, want closing", x.state(a))
	}
	if x.h.CanActivate() {
		t.Fatal("CanActivate with K retired and one closing = true, want false (req 54)")
	}
	if _, err := x.h.Publish(newSnap("e", nil)); !errors.Is(err, ErrBusy) {
		t.Fatalf("Publish while busy = %v, want ErrBusy", err)
	}

	x.clock.Advance(29 * time.Second)
	x.idle()
	if x.state(a) != int(StateClosing) || ended(pa.rec) != snapshot.EndNone {
		t.Fatal("grace cut short before 30 s")
	}
	x.clock.Advance(time.Second)
	x.waitState(a, int(StateEnding))
	waitFor(t, "grace-end marking", func() bool { return ended(pa.rec) == snapshot.EndGrace })
	waitFor(t, "root context cancel", func() bool { return pa.cancels.Load() == 1 })
	waitFor(t, "ended counter", func() bool { return x.ended.n.Load() == 1 })
	if ended(pb.rec) != snapshot.EndNone || ended(pc.rec) != snapshot.EndNone {
		t.Fatal("the ending protocol touched a request of a retired snapshot")
	}

	x.unpin(pa)
	x.waitState(a, freed)
	if !x.h.CanActivate() {
		t.Fatal("CanActivate after the ending snapshot was freed = false")
	}
	x.unpin(pb)
	x.unpin(pc)
	x.waitState(b, freed)
	x.waitState(c, freed)
	if got := x.h.Counts(); got != (Counts{Active: true}) {
		t.Fatalf("counts after every unpin = %+v, want only the active", got)
	}
	if n := x.ended.n.Load(); n != 1 {
		t.Fatalf("ruralz_snapshot_retirement_ended_total = %d, want 1", n)
	}
}

// TestReq52ClosingFreedBeforeGraceIsNeverEnded frees a closing snapshot
// whose last request finished during grace; nothing is ended.
func TestReq52ClosingFreedBeforeGraceIsNeverEnded(t *testing.T) {
	x := newHarness(t, Config{K: 1}, true)
	a, b, c := newSnap("a", x.log), newSnap("b", x.log), newSnap("c", x.log)
	x.publish(a)
	pa := x.pin(0)
	x.publish(b)
	pb := x.pin(0)
	x.publish(c)
	if x.state(a) != int(StateClosing) {
		t.Fatalf("state(a) = %d, want closing with K = 1", x.state(a))
	}
	x.clock.Advance(10 * time.Second)
	x.unpin(pa)
	x.waitState(a, freed)
	x.clock.Advance(time.Minute)
	x.idle()
	if ended(pa.rec) != snapshot.EndNone || pa.cancels.Load() != 0 {
		t.Fatal("a request finished during grace was ended")
	}
	if n := x.ended.n.Load(); n != 0 {
		t.Fatalf("ended counter = %d, want 0", n)
	}
	x.unpin(pb)
	x.waitState(b, freed)
}

// TestReq52OverdueRaisesAndClearsDegraded keeps an ending snapshot pinned
// past 6 s: snapshot_ending_overdue is raised, the snapshot stays ending
// (never freed early) and the reason clears once it is freed.
func TestReq52OverdueRaisesAndClearsDegraded(t *testing.T) {
	x := newHarness(t, Config{K: 1}, true)
	a, b, c := newSnap("a", x.log), newSnap("b", x.log), newSnap("c", x.log)
	res := &fakeCloser{name: "pool-a"}
	a.Resources = []io.Closer{res}
	x.publish(a)
	pa := x.pin(0)
	x.publish(b)
	pb := x.pin(1)
	x.publish(c)
	x.clock.Advance(DefaultGrace)
	x.waitState(a, int(StateEnding))
	waitFor(t, "grace-end marking", func() bool { return ended(pa.rec) == snapshot.EndGrace })

	x.clock.Advance(DefaultEndingBound - time.Second)
	x.idle()
	if calls := x.status.get(); len(calls) != 0 {
		t.Fatalf("degraded raised before the ending bound: %+v", calls)
	}
	x.clock.Advance(time.Second)
	want := statusCall{catalog.ReasonSnapshotEndingOverdue, statusSource, true}
	waitFor(t, "snapshot_ending_overdue", func() bool { return slices.Contains(x.status.get(), want) })
	x.clock.Advance(time.Hour)
	x.idle()
	if x.state(a) != int(StateEnding) || res.isClosed() || bindingOf(a).releases.Load() != 0 {
		t.Fatal("an overdue snapshot was freed early")
	}

	x.unpin(pa)
	x.waitState(a, freed)
	cleared := statusCall{catalog.ReasonSnapshotEndingOverdue, statusSource, false}
	waitFor(t, "degraded cleared", func() bool { return slices.Contains(x.status.get(), cleared) })
	waitFor(t, "resource closed", res.isClosed)
	if calls := x.status.get(); len(calls) != 2 {
		t.Fatalf("degraded transitions = %+v, want one raise and one clear", calls)
	}
	x.unpin(pb)
}

// TestReq52RetiredSnapshotsGauge follows ruralz_config_retired_snapshots:
// every live snapshot that is not active.
func TestReq52RetiredSnapshotsGauge(t *testing.T) {
	x := newHarness(t, Config{}, true)
	gauge := func() int64 { return x.gauge.v.Load() }
	a, b, c, d := newSnap("a", nil), newSnap("b", nil), newSnap("c", nil), newSnap("d", nil)
	x.publish(a)
	if gauge() != 0 {
		t.Fatalf("gauge after the first publish = %d, want 0", gauge())
	}
	pa := x.pin(0)
	x.publish(b)
	if gauge() != 1 {
		t.Fatalf("gauge with one pinned retired = %d, want 1", gauge())
	}
	pb := x.pin(0)
	x.publish(c)
	pc := x.pin(0)
	x.publish(d)
	if gauge() != 3 {
		t.Fatalf("gauge with 2 retired and 1 closing = %d, want 3", gauge())
	}
	x.unpin(pb)
	waitFor(t, "gauge 2", func() bool { return gauge() == 2 })
	x.unpin(pa)
	x.unpin(pc)
	waitFor(t, "gauge 0", func() bool { return gauge() == 0 })
}

// TestReq58BindingCallsInOrder checks every snapshot gets exactly one
// Retire at retirement, then exactly one Release at zero pins, in that
// order, and a Release never precedes the last Unpin.
func TestReq58BindingCallsInOrder(t *testing.T) {
	x := newHarness(t, Config{}, true)
	a, b, c := newSnap("a", x.log), newSnap("b", x.log), newSnap("c", x.log)
	var unpinned atomic.Bool
	bindingOf(a).onRel = func() {
		if !unpinned.Load() {
			t.Error("a.Release ran while a was pinned")
		}
	}
	x.publish(a)
	pa := x.pin(3)
	x.publish(b)
	x.publish(c)
	waitFor(t, "b released", func() bool { return bindingOf(b).releases.Load() == 1 })
	unpinned.Store(true)
	x.unpin(pa)
	waitFor(t, "a released", func() bool { return bindingOf(a).releases.Load() == 1 })
	want := []string{"a.retire", "b.retire", "b.release", "a.release"}
	if got := x.log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("binding calls = %v, want %v", got, want)
	}
	if bindingOf(c).retires.Load() != 0 || bindingOf(c).releases.Load() != 0 {
		t.Fatal("the active snapshot's Binding was called")
	}
	if err := x.h.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	want = append(want, "c.retire", "c.release")
	if got := x.log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("binding calls after Close = %v, want %v", got, want)
	}
}

// TestReq54WaitActivate blocks while K snapshots are retired and one is
// closing, and returns once the closing snapshot is freed.
func TestReq54WaitActivate(t *testing.T) {
	x := newHarness(t, Config{K: 1}, true)
	a, b, c := newSnap("a", nil), newSnap("b", nil), newSnap("c", nil)
	x.publish(a)
	pa := x.pin(0)
	x.publish(b)
	pb := x.pin(0)
	x.publish(c)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := x.h.WaitActivate(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitActivate while busy = %v, want DeadlineExceeded", err)
	}

	done := make(chan error, 1)
	go func() { done <- x.h.WaitActivate(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("WaitActivate returned %v while busy", err)
	case <-time.After(20 * time.Millisecond):
	}
	x.unpin(pa)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitActivate = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WaitActivate did not return after the closing snapshot was freed")
	}
	x.publish(newSnap("d", nil))
	x.unpin(pb)
}

// TestReq54RetiredOnlyWhileGraceRuns allows an activation with fewer than
// K retired even while one snapshot is closing (the gate is "retired < K
// or none closing or ending").
func TestReq54RetiredOnlyWhileGraceRuns(t *testing.T) {
	x := newHarness(t, Config{}, true)
	snaps := []*snapshot.Snapshot{newSnap("a", nil), newSnap("b", nil), newSnap("c", nil), newSnap("d", nil)}
	var ps []pinned
	for _, s := range snaps {
		x.publish(s)
		ps = append(ps, x.pin(0))
	}
	// a closing, b and c retired, d active.
	x.unpin(ps[1])
	x.waitState(snaps[1], freed)
	if !x.h.CanActivate() {
		t.Fatal("CanActivate with 1 retired and 1 closing = false, want true")
	}
	e := newSnap("e", nil)
	x.publish(e)
	if got := x.h.Counts(); got != (Counts{Active: true, Retired: 2, Closing: 1}) {
		t.Fatalf("counts = %+v, want 2 retired and 1 closing", got)
	}
	for _, p := range []pinned{ps[0], ps[2], ps[3]} {
		x.unpin(p)
	}
}

// TestReq51ResourcesClosedOnlyAtZeroPinsAndUnshared checks resources and
// State Store handles: nothing closes while pinned, a resource the active
// snapshot still references stays open, each closes exactly once.
func TestReq51ResourcesClosedOnlyAtZeroPinsAndUnshared(t *testing.T) {
	x := newHarness(t, Config{}, true)
	own, shared, later := &fakeCloser{name: "own"}, &fakeCloser{name: "shared"}, &fakeCloser{name: "later"}
	store, cache := &fakeHandle{}, &fakeHandle{}
	a := newSnap("a", x.log, own, shared)
	a.StateStore, a.CacheStore = store, store
	b := newSnap("b", x.log, shared, later)
	b.StateStore, b.CacheStore = store, cache
	c := newSnap("c", x.log)

	x.publish(a)
	pa := x.pin(0)
	x.publish(b)
	x.idle()
	if own.isClosed() || shared.isClosed() || store.released.Load() != 0 {
		t.Fatal("a resource closed while its snapshot was pinned")
	}
	x.unpin(pa)
	x.waitState(a, freed)
	waitFor(t, "own closed", own.isClosed)
	x.idle()
	if shared.isClosed() {
		t.Fatal("a resource the active snapshot shares was closed")
	}
	if store.released.Load() != 0 {
		t.Fatal("a State Store handle the active snapshot shares was released")
	}

	x.publish(c)
	x.waitState(b, freed)
	waitFor(t, "shared and later closed", func() bool { return shared.isClosed() && later.isClosed() })
	x.idle()
	for _, r := range []*fakeCloser{own, shared, later} {
		if n := r.closed.Load(); n != 1 {
			t.Fatalf("%s closed %d times, want 1", r.name, n)
		}
	}
	if n := store.released.Load(); n != 1 {
		t.Fatalf("store handle released %d times, want 1", n)
	}
	if n := cache.released.Load(); n != 1 {
		t.Fatalf("cache handle released %d times, want 1", n)
	}
}

// TestReq51OutOfOrderFreeKeepsOlderSharer frees a newer retired snapshot
// before an older pinned one that shares its resource: the resource stays
// open until the older one is freed.
func TestReq51OutOfOrderFreeKeepsOlderSharer(t *testing.T) {
	x := newHarness(t, Config{}, true)
	pool := &fakeCloser{name: "pool"}
	a, b, c := newSnap("a", nil, pool), newSnap("b", nil, pool), newSnap("c", nil)
	x.publish(a)
	pa := x.pin(0)
	x.publish(b)
	x.publish(c)
	x.waitState(b, freed)
	x.idle()
	if pool.isClosed() {
		t.Fatal("a resource an older pinned snapshot shares was closed")
	}
	x.unpin(pa)
	x.waitState(a, freed)
	waitFor(t, "pool closed", pool.isClosed)
	x.idle()
	if n := pool.closed.Load(); n != 1 {
		t.Fatalf("pool closed %d times, want 1", n)
	}
}

// TestReq51ResourceCloseOrderAndErrors closes in reverse order, keeps going
// after a failing Close and logs it.
func TestReq51ResourceCloseOrderAndErrors(t *testing.T) {
	logs := &logRecords{}
	x := newHarness(t, Config{Logger: slog.New(logs)}, true)
	var order []string
	first := &fakeCloser{name: "first"}
	second := &fakeCloser{name: "second", err: errCloseFailed}
	first.onErr = func() { order = append(order, "first") }
	second.onErr = func() { order = append(order, "second") }
	unhashable := closerFunc(func() error { order = append(order, "func"); return nil })
	a := newSnap("a", nil, first, second, unhashable)
	x.publish(a)
	x.publish(newSnap("b", nil))
	x.waitState(a, freed)
	x.idle()
	if want := []string{"func", "second", "first"}; !slices.Equal(order, want) {
		t.Fatalf("close order = %v, want %v", order, want)
	}
	if !logs.has("snapshot resource close failed") {
		t.Fatal("a failing Close was not logged")
	}
}

// panicHandle is a State Store handle whose Release panics.
type panicHandle struct{ fakeHandle }

func (h *panicHandle) Release() {
	h.released.Add(1)
	panic("driver release")
}

// TestReq51PanickingCloseIsContained frees a snapshot whose resources
// include a typed-nil io.Closer and a Close that panics, and whose State
// Store handle panics on Release: each panic is logged, the Node keeps
// running and the remaining resources and handles are still closed and
// released.
func TestReq51PanickingCloseIsContained(t *testing.T) {
	logs := &logRecords{}
	x := newHarness(t, Config{Logger: slog.New(logs)}, true)
	first := &fakeCloser{name: "first"}
	var typedNil *fakeCloser
	panicky := &fakeCloser{name: "panicky", onErr: func() { panic("pool close") }}
	bad, good := &panicHandle{}, &fakeHandle{}
	// Closed in reverse: panicky, the typed nil, then first.
	a := newSnap("a", nil, first, typedNil, panicky)
	a.StateStore, a.CacheStore = bad, good
	x.publish(a)
	x.publish(newSnap("b", nil))
	x.waitState(a, freed)
	x.idle()
	if !first.isClosed() || !panicky.isClosed() {
		t.Fatal("a resource after a panicking Close was not closed")
	}
	if bad.released.Load() != 1 || good.released.Load() != 1 {
		t.Fatalf("handle releases = %d, %d; want 1, 1", bad.released.Load(), good.released.Load())
	}
	for _, msg := range []string{
		"retirement operation panicked (" + opResourceClose + ")",
		"retirement operation panicked (" + opHandleRelease + ")",
	} {
		if !logs.has(msg) {
			t.Fatalf("%q was not logged", msg)
		}
	}
	x.publish(newSnap("c", nil)) // the Holder still works
}

// TestReq52PanickingClosingHookIsContained: a panicking OnClosing hook is
// logged and the snapshot still goes through grace and ending.
func TestReq52PanickingClosingHookIsContained(t *testing.T) {
	logs := &logRecords{}
	x := newHarness(t, Config{K: 1, Logger: slog.New(logs), OnClosing: func(*snapshot.Snapshot) { panic("stream registry") }}, true)
	a, b, c := newSnap("a", nil), newSnap("b", nil), newSnap("c", nil)
	x.publish(a)
	pa := x.pin(0)
	x.publish(b)
	x.publish(c)
	waitFor(t, "hook panic logged", func() bool { return logs.has("retirement operation panicked (" + opClosingHook + ")") })
	x.clock.Advance(DefaultGrace)
	waitFor(t, "grace-end marking", func() bool { return ended(pa.rec) == snapshot.EndGrace })
	x.unpin(pa)
	x.waitState(a, freed)
}

// TestPublishRejectsAFreedSnapshot: a freed snapshot (resources closed,
// Binding released) is never published again, so no request is served
// with closed resources and the Pin re-load never sees the pointer twice.
func TestPublishRejectsAFreedSnapshot(t *testing.T) {
	x := newHarness(t, Config{}, true)
	pool := &fakeCloser{name: "pool"}
	a := newSnap("a", x.log, pool)
	x.publish(a)
	x.publish(newSnap("b", x.log))
	x.waitState(a, freed)
	waitFor(t, "pool closed", pool.isClosed)
	if _, err := x.h.Publish(a); !errors.Is(err, ErrPublished) {
		t.Fatalf("Publish of a freed snapshot = %v, want ErrPublished", err)
	}
	if bindingOf(a).retires.Load() != 1 || bindingOf(a).releases.Load() != 1 {
		t.Fatal("the rejected Publish touched the freed snapshot's Binding")
	}
	if !x.h.CanActivate() {
		t.Fatal("a rejected Publish changed the retirement state")
	}
	x.publish(newSnap("c", x.log))
}

// TestFreedSnapshotsAreForgottenOnceCollected: the freed-snapshot set
// holds weak pointers only, so a snapshot no caller references is
// collected and its entry dropped.
func TestFreedSnapshotsAreForgottenOnceCollected(t *testing.T) {
	x := newHarness(t, Config{}, false)
	for i := range 4 {
		x.publish(newSnap("gc-"+strconv.Itoa(i), nil))
		x.h.reconcile(context.Background())
	}
	if n := x.h.freed.count(); n != 3 {
		t.Fatalf("freed set holds %d snapshots, want 3", n)
	}
	waitFor(t, "freed snapshots collected", func() bool {
		runtime.GC()
		return x.h.freed.count() == 0
	})
	if x.h.Active() == nil {
		t.Fatal("the active snapshot is gone")
	}
}

// closerFunc is a resource whose dynamic type is not comparable.
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// TestReq52OnClosingHook ends the streams of a snapshot that becomes
// closing (the M3 hook; M1 has no streams).
func TestReq52OnClosingHook(t *testing.T) {
	got := make(chan *snapshot.Snapshot, 1)
	x := newHarness(t, Config{K: 1, OnClosing: func(s *snapshot.Snapshot) { got <- s }}, true)
	a, b, c := newSnap("a", nil), newSnap("b", nil), newSnap("c", nil)
	x.publish(a)
	pa := x.pin(0)
	x.publish(b)
	pb := x.pin(0)
	x.publish(c)
	select {
	case s := <-got:
		if s != a {
			t.Fatal("OnClosing got the wrong snapshot")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OnClosing was not called")
	}
	x.unpin(pa)
	x.unpin(pb)
}

func TestPublishErrors(t *testing.T) {
	x := newHarness(t, Config{}, false)
	a := newSnap("a", nil)
	if _, err := x.h.Publish(nil); !errors.Is(err, ErrNilSnapshot) {
		t.Fatalf("Publish(nil) = %v, want ErrNilSnapshot", err)
	}
	prev, err := x.h.Publish(a)
	if err != nil || prev != nil {
		t.Fatalf("first Publish = %v, %v", prev, err)
	}
	if _, err := x.h.Publish(a); !errors.Is(err, ErrPublished) {
		t.Fatalf("Publish of the active snapshot = %v, want ErrPublished", err)
	}
	pa := x.pin(0)
	b := newSnap("b", nil)
	if prev, err := x.h.Publish(b); err != nil || prev != a {
		t.Fatalf("second Publish = %v, %v; want a", prev, err)
	}
	if _, err := x.h.Publish(a); !errors.Is(err, ErrPublished) {
		t.Fatalf("Publish of a retired snapshot = %v, want ErrPublished", err)
	}
	x.unpin(pa)
}

// TestCloseRetiresTheActiveAndWaits closes with a pinned request: Close
// waits for it, Pin returns nil and Publish and WaitActivate fail.
func TestCloseRetiresTheActiveAndWaits(t *testing.T) {
	x := newHarness(t, Config{}, true)
	pool := &fakeCloser{name: "pool"}
	a := newSnap("a", x.log, pool)
	x.publish(a)
	pa := x.pin(0)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := x.h.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close with a pinned request = %v, want DeadlineExceeded", err)
	}
	if s := x.h.Pin(0, &snapshot.PinnedRequest{}); s != nil {
		t.Fatal("Pin after Close returned a snapshot")
	}
	if _, err := x.h.Publish(newSnap("b", nil)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close = %v, want ErrClosed", err)
	}
	if err := x.h.WaitActivate(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("WaitActivate after Close = %v, want ErrClosed", err)
	}
	if x.h.CanActivate() {
		t.Fatal("CanActivate after Close = true")
	}

	done := make(chan error, 1)
	go func() { done <- x.h.Close(context.Background()) }()
	x.unpin(pa)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the last unpin")
	}
	if !pool.isClosed() {
		t.Fatal("Close returned before the resources closed")
	}
	if got := x.h.Counts(); got != (Counts{}) {
		t.Fatalf("counts after Close = %+v, want none", got)
	}
	if want := []string{"a.retire", "a.release"}; !slices.Equal(x.log.snapshot(), want) {
		t.Fatalf("binding calls = %v, want %v", x.log.snapshot(), want)
	}
}

// TestCloseWithoutRun frees snapshots through Close's own reconcile loop.
func TestCloseWithoutRun(t *testing.T) {
	x := newHarness(t, Config{}, false)
	x.publish(newSnap("a", nil))
	x.publish(newSnap("b", nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := x.h.Close(ctx); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := x.h.Close(ctx); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

// TestRunReturnsOnCancel checks Run's stop path.
func TestRunReturnsOnCancel(t *testing.T) {
	x := newHarness(t, Config{}, true)
	x.publish(newSnap("a", nil))
	p := x.pin(0)
	x.publish(newSnap("b", nil))
	x.stop()
	x.unpin(p)
}

func TestStateString(t *testing.T) {
	tests := []struct {
		s    State
		want string
	}{
		{StateActive, "active"},
		{StateRetired, "retired"},
		{StateClosing, "closing"},
		{StateEnding, "ending"},
		{State(9), ""},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("State(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
	}
}
