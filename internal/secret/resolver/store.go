// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"cmp"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/secret"
)

// cell holds the current value of one Ref. Every Store resolved while the
// Ref was active shares the active cell, so rotations reach all of them;
// a cell is never replaced for a Ref the active Store holds.
type cell struct {
	ref secret.Ref
	cur atomic.Pointer[cellValue]

	// Poll state, guarded by Resolver.mu (file references only).
	fp     fingerprint // of the last examination (read or failure)
	racy   bool        // the file changed too close to that read to trust fp
	failed bool        // the last examination failed; the value is the last good one
}

// cellValue is one immutable value with its Node-wide version.
type cellValue struct {
	val     secret.Value
	version uint64
}

// pendingValue is a refused rotation a Revision's uses accept: the value
// Resolve read for a shared cell whose last examination failed, applied to
// the cell when that Revision's Store is activated (spec 01 requirement
// 46; see Resolver.settleLocked).
type pendingValue struct {
	cv   *cellValue  // the value, at a version allocated by Resolve
	base uint64      // the cell's version when Resolve read the file
	fp   fingerprint // the file state Resolve read
	at   time.Time   // when Resolve read it
}

// newCell returns a cell holding b at version v.
func newCell(ref secret.Ref, b []byte, v uint64) *cell {
	c := &cell{ref: ref}
	c.set(b, v)
	return c
}

// set publishes b (copied) at version v.
func (c *cell) set(b []byte, v uint64) {
	c.cur.Store(&cellValue{val: secret.NewValue(b), version: v})
}

// load returns the current value.
func (c *cell) load() *cellValue { return c.cur.Load() }

// examined records a successful examination of the file state fp at now.
func (c *cell) examined(fp fingerprint, now time.Time) {
	c.fp = fp
	c.racy = fp.racyAt(now)
	c.failed = false
}

// useCheck is one use's check: its kind and the consumer check.
type useCheck struct {
	kind  secret.Kind
	check func([]byte) error
}

// fileGroup is the file references of one Store sharing one file.
type fileGroup struct {
	name  string       // cleaned absolute slash path
	refs  []secret.Ref // sorted
	limit int64        // largest cap of the uses of refs
}

// store is the secret.Store of one resolved Revision. Its cells, uses,
// refs and files are immutable after Resolve returns; Get reads them and
// two atomic pointers.
type store struct {
	res   *Resolver
	cells map[secret.Ref]*cell
	uses  map[secret.Ref][]useCheck
	refs  []secret.Ref // sorted
	files []fileGroup  // sorted by name

	// pending holds the refused rotations this Store's uses accept, which
	// Get returns instead of the shared cell's value until Activate
	// settles them; nil when there are none or once settled. The map is
	// immutable.
	pending atomic.Pointer[map[secret.Ref]*pendingValue]

	// checked is the version of each file reference's value last checked
	// against this Store's uses; guarded by Resolver.mu.
	checked map[secret.Ref]uint64
}

var _ secret.Store = (*store)(nil)

// Get returns the current value of r, rotations included; false when r is
// not one of the Store's uses. It takes no lock and does not allocate. A
// new value is returned as soon as it is published: within one cycle the
// poll publishes every changed reference of the active Store
// (refreshLocked) before fanout calls any watcher, so a watcher can Get
// the other half of a pair and see this poll's value. A watcher's error or
// panic never withdraws a published value: Get keeps returning it, and
// only that watcher keeps its last value and counts a failure. A file
// value that fails a use check of the active Store is never published by
// a poll of that Store (refreshLocked): Get keeps the last good value and
// secret_rotation_failed is raised. A value published before Activate, by
// a poll of the previously active Store, that s's uses refuse stays
// served, as the only value the reference has; it counts a rotation
// failure and raises secret_rotation_failed until a value they accept is
// read (Resolver.Activate, recheckLocked).
func (s *store) Get(r secret.Ref) (secret.Value, bool) {
	c, ok := s.cells[r]
	if !ok {
		return secret.Value{}, false
	}
	return s.value(r, c).val, true
}

// value returns the current value of r, held in c: the pending value of r
// until Activate settles it, else c's value.
func (s *store) value(r secret.Ref, c *cell) *cellValue {
	if pm := s.pending.Load(); pm != nil {
		if p, ok := (*pm)[r]; ok {
			return p.cv
		}
	}
	return c.load()
}

// Watch registers fn for rotations of r, Node-wide (R-55): the
// registration survives Activate of later Stores, fires on the resolver
// goroutine whenever the active Store holds a value of r the registration
// has not seen and stays silent while no active Store holds r. fn must not
// block; an fn error keeps the watcher's last value and counts a rotation
// failure. fn runs with no Resolver lock held, so it may call Watch and
// any stop function, its own included: a Watch made inside fn starts at
// the current version of r in s and does not receive the value being
// delivered, and once the running watcher is stopped its result is
// ignored. Register the watch before reading the initial value with Get,
// so no rotation is lost between them. stop unregisters and may be called
// more than once; fn may still run once if stop races a delivery already
// under way.
func (s *store) Watch(r secret.Ref, fn func(secret.Value) error) (stop func()) {
	var seen uint64
	if c, ok := s.cells[r]; ok {
		seen = s.value(r, c).version
	}
	return s.res.watch(r, fn, seen)
}

// check runs every use check of r on b; the first failure is returned.
func (s *store) check(r secret.Ref, b []byte) error {
	for _, u := range s.uses[r] {
		if err := runCheck(u, b); err != nil {
			return err
		}
	}
	return nil
}

// compareRefs orders references by provider, name and key.
func compareRefs(a, b secret.Ref) int {
	return cmp.Or(
		cmp.Compare(a.Provider, b.Provider),
		cmp.Compare(a.Name, b.Name),
		cmp.Compare(a.Key, b.Key),
	)
}
