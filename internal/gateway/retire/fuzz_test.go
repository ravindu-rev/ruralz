// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"context"
	"errors"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// FuzzRetirementLifecycle is the pin/retire property test of test plan
// item 12 driven by random operation sequences (publish, pin, unpin,
// advance the fake clock, reconcile). After every step it checks the
// invariants of spec 04 reqs 50-54 and 09 req 56:
//
//   - at most K retired snapshots and at most one closing or ending;
//   - Publish succeeds exactly when CanActivate holds (retired < K or none
//     closing or ending);
//   - a pinned snapshot is never freed: no Binding.Release, no resource
//     closed;
//   - a freed snapshot has zero pins, one Retire then one Release, and its
//     unshared resources closed exactly once;
//   - every request of an ending snapshot is marked EndGrace;
//   - the retired-snapshots gauge counts the live snapshots that are not
//     active, and snapshot_ending_overdue is raised exactly while an
//     ending snapshot is past the ending bound.
func FuzzRetirementLifecycle(f *testing.F) {
	f.Add([]byte{0, 0, 1, 0, 1, 0, 1, 0, 3 | 30<<3, 4, 3 | 6<<3, 2, 2, 2, 0})
	f.Add([]byte{1, 0, 9, 0, 17, 0, 25, 0, 0, 3 | 31<<3, 3 | 31<<3, 2, 10, 18, 0, 0})
	f.Add([]byte{2, 0, 1, 1, 0, 1, 0, 1, 0, 1, 0, 3 | 7<<3, 3 | 31<<3, 4, 2, 2, 2, 2, 2})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 1, 2, 1, 2, 3 | 1<<3})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) == 0 {
			return
		}
		ops = ops[:min(len(ops), 200)]
		k := 1 + int(ops[0]%3)
		x := newHarness(t, Config{K: k}, false)
		m := &lifecycleModel{x: x, k: k}
		for i, op := range ops[1:] {
			m.step(op)
			m.check(i, op)
		}
		for len(m.pins) > 0 {
			m.unpin(0)
		}
		m.settle()
		m.check(-1, 0)
		if c := x.h.Counts(); c.Retired+c.Closing+c.Ending != 0 {
			t.Fatalf("snapshots left after every unpin: %+v", c)
		}
		if err := x.h.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		m.check(-1, 0)
	})
}

// lifecycleModel is the fuzz target's view of what it published and
// pinned.
type lifecycleModel struct {
	x      *harness
	k      int
	snaps  []*snapshot.Snapshot
	own    map[*snapshot.Snapshot]*fakeCloser
	pools  map[*fakeCloser][]*snapshot.Snapshot
	pins   []pinned
	next   int
	shared *fakeCloser
}

func (m *lifecycleModel) step(op byte) {
	ctx := context.Background()
	arg := int(op >> 3)
	switch op % 5 {
	case 0: // publish
		m.x.h.reconcile(ctx)
		m.x.idle()
		can := m.x.h.CanActivate()
		s := m.newSnapshot(arg)
		_, err := m.x.h.Publish(s)
		switch {
		case can && err != nil:
			m.x.t.Fatalf("Publish = %v while CanActivate", err)
		case !can && !errors.Is(err, ErrBusy):
			m.x.t.Fatalf("Publish = %v while !CanActivate, want ErrBusy", err)
		case err == nil:
			m.snaps = append(m.snaps, s)
			pool := s.Resources[1].(*fakeCloser)
			m.own[s] = s.Resources[0].(*fakeCloser)
			m.pools[pool] = append(m.pools[pool], s)
		}
	case 1: // pin
		if m.x.h.Active() != nil {
			m.pins = append(m.pins, m.x.pin(emit.Stripe(arg)))
		}
	case 2: // unpin
		if len(m.pins) > 0 {
			m.unpin(arg % len(m.pins))
		}
	case 3: // advance the clock
		m.x.clock.Advance(time.Duration(arg) * time.Second)
	case 4:
	}
	m.settle()
}

// settle runs the retirer once and waits for its tasks.
func (m *lifecycleModel) settle() {
	m.x.h.reconcile(context.Background())
	m.x.idle()
}

func (m *lifecycleModel) unpin(i int) {
	m.x.unpin(m.pins[i])
	m.pins = append(m.pins[:i], m.pins[i+1:]...)
}

// newSnapshot returns a snapshot with its own resource and, when arg is
// odd, the last pool (a new pool otherwise); step records both once the
// snapshot is published.
func (m *lifecycleModel) newSnapshot(arg int) *snapshot.Snapshot {
	if m.own == nil {
		m.own = map[*snapshot.Snapshot]*fakeCloser{}
		m.pools = map[*fakeCloser][]*snapshot.Snapshot{}
	}
	m.next++
	own := &fakeCloser{name: "own"}
	if m.shared == nil || arg%2 == 0 {
		m.shared = &fakeCloser{name: "pool"}
	}
	return newSnap("fuzz-"+strconv.Itoa(m.next), m.x.log, own, m.shared)
}

func (m *lifecycleModel) check(step int, op byte) {
	t := m.x.t
	h := m.x.h
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("step %d (op %d): "+format, append([]any{step, op}, args...)...)
	}
	c := h.Counts()
	if c.Retired > m.k || c.Closing+c.Ending > 1 {
		fail("retirement bound broken: %+v with K = %d", c, m.k)
	}
	if got := m.x.gauge.v.Load(); got != int64(c.Retired+c.Closing+c.Ending) {
		fail("retired gauge = %d, counts %+v", got, c)
	}
	active := h.Active()
	overdue := false
	now := m.x.clock.Now()
	for _, s := range m.snaps {
		st := m.x.state(s)
		b := bindingOf(s)
		pins := s.Pins.Count()
		switch st {
		case freed:
			if pins != 0 {
				fail("freed snapshot has %d pins", pins)
			}
			if b.retires.Load() != 1 || b.releases.Load() != 1 {
				fail("freed snapshot binding retire/release = %d/%d", b.retires.Load(), b.releases.Load())
			}
			if n := m.own[s].closed.Load(); n != 1 {
				fail("own resource of a freed snapshot closed %d times", n)
			}
		default:
			if b.releases.Load() != 0 || m.own[s].isClosed() {
				fail("a live snapshot was released or closed (state %d, pins %d)", st, pins)
			}
			if s == active && b.retires.Load() != 0 {
				fail("active snapshot retired")
			}
			if s != active && b.retires.Load() != 1 {
				fail("non-active snapshot retire count %d", b.retires.Load())
			}
			if st == int(StateEnding) {
				h.mu.Lock()
				tr := h.findLocked(s)
				if !now.Before(tr.overdueAt) {
					overdue = true
				}
				h.mu.Unlock()
			}
		}
	}
	for _, p := range m.pins {
		if m.x.state(p.s) == freed {
			fail("a pinned snapshot was freed")
		}
		if m.x.state(p.s) == int(StateEnding) && ended(p.rec) != snapshot.EndGrace {
			fail("a request of an ending snapshot was not ended")
		}
	}
	for pool, users := range m.pools {
		live := false
		for _, s := range users {
			if m.x.state(s) != freed {
				live = true
			}
		}
		switch n := pool.closed.Load(); {
		case live && n != 0:
			fail("a pool shared by a live snapshot was closed")
		case !live && n != 1:
			fail("a pool no live snapshot shares was closed %d times", n)
		}
	}
	on := false
	for _, call := range m.x.status.get() {
		if call.reason != catalog.ReasonSnapshotEndingOverdue {
			fail("unexpected degraded reason %v", call.reason)
		}
		on = call.on
	}
	if on != overdue {
		fail("snapshot_ending_overdue = %v, want %v", on, overdue)
	}
}

var _ io.Closer = (*fakeCloser)(nil)
