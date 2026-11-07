// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package statestoretest is the State Store test kit: Fake, a
// statestore.Driver that answers every call itself, for tests that need a
// driver but no State Store; Model, the reference arithmetic of the GCRA
// and Quota scripts; and RunConformance, the driver conformance suite the
// memory driver runs in its unit tests and the redis driver runs against
// local and CI servers. It is imported by tests only (depguard
// testsupport).
package statestoretest

import (
	"context"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Fake is a statestore.Driver that answers every call it attempts
// itself: every consumptive call is allowed (Remaining 0), every lookup
// misses and every write applies. It attempts a round trip only within
// its per-request deadline, on the real clock, failing its calls with
// RZ-STS-004 (statestore.ErrNotAttempted) otherwise, and with RZ-STS-002
// (statestore.ErrFailed) once closed. Adjacent consumptive calls with an
// equal config.key digest merge into one script_multi round trip. It
// supports scripts only. The zero value is ready to use; Fake is safe for
// concurrent use.
type Fake struct {
	mu     sync.Mutex
	closed bool
}

var _ statestore.Driver = (*Fake)(nil)

// Status implements statestore.Driver: no degraded reason.
func (f *Fake) Status() statestore.Status { return statestore.Status{} }

// Close implements statestore.Driver; later calls fail with RZ-STS-002.
func (f *Fake) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// Supports implements statestore.Store: scripts only.
func (f *Fake) Supports(c statestore.Capability) bool {
	return statestore.CapScripts&c == c
}

// RoundTrips implements statestore.Store.
func (f *Fake) RoundTrips() bool { return true }

// AdmitStoreBytes implements statestore.Store: every store fits.
func (f *Fake) AdmitStoreBytes(statestore.CacheKey, int) bool { return true }

// consumptiveDigest returns the config.key digest of a GCRA or Quota call.
func consumptiveDigest(c *statestore.Call) (statestore.Digest, bool) {
	switch c.Kind {
	case statestore.OpGCRA:
		return c.GCRA.Digest, true
	case statestore.OpQuota:
		return c.Quota.Digest, true
	default:
		return statestore.Digest{}, false
	}
}

// attempt reports the failure of every call of one round trip, nil when
// it is attempted: RZ-STS-004 once the per-request deadline, or the
// context's without a budget, leaves no time, and RZ-STS-002 once closed.
func (f *Fake) attempt(ctx context.Context, rb *statestore.RequestBudget, policy time.Duration) *statestore.Error {
	if !budget(ctx, rb, policy) {
		return statestore.ErrNotAttempted
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return statestore.ErrFailed
	}
	return nil
}

// budget reports whether the per-request deadline, or the context
// deadline alone without a budget, leaves the round trip time.
func budget(ctx context.Context, rb *statestore.RequestBudget, policy time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	now := clock.Real().Now()
	if rb != nil {
		_, ok := rb.CallTimeout(ctx, now, policy)
		return ok
	}
	if dl, ok := ctx.Deadline(); ok {
		policy = min(policy, dl.Sub(now))
	}
	return policy > 0
}

// Consume implements statestore.Store.
func (f *Fake) Consume(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) int {
	if len(calls) == 0 {
		return 0
	}
	n := groupLen(calls)
	group := calls[:n]
	trip, batch := statestore.TripSingle, -1
	if n > 1 {
		trip, batch = statestore.TripScriptMulti, 0
	}
	policy := group[0].Timeout
	for _, c := range group {
		policy = min(policy, c.Timeout)
	}
	err := f.attempt(ctx, rb, policy)
	now := clock.Real().Now()
	for _, c := range group {
		c.Batch, c.Trip, c.Elapsed = batch, trip, 0
		if err != nil {
			c.Done, c.Err = false, err
			continue
		}
		c.Done, c.Err = true, nil
		switch c.Kind {
		case statestore.OpGCRA:
			g := &c.GCRA
			g.Allowed, g.Denied, g.RetryAfter, g.ServerNow = true, -1, 0, now
			for i := range min(len(g.Out), len(g.Limits)) {
				g.Out[i] = statestore.GCRAOutcome{}
			}
		case statestore.OpQuota:
			q := &c.Quota
			q.Allowed, q.Used, q.RetryAfter, q.ServerNow = true, 1, 0, now
			q.WindowStart = now
			if wm := q.Window.Milliseconds(); wm > 0 {
				ms := now.UnixMilli()
				q.WindowStart = time.UnixMilli(ms - ms%wm).UTC()
			}
		default:
			c.Done, c.Err = false, statestore.ErrFailed
		}
	}
	return n
}

// groupLen returns the length of the longest prefix of calls (at least
// one) that merges into one round trip: consumptive calls with an equal
// config.key digest.
func groupLen(calls []*statestore.Call) int {
	n := 1
	if d0, ok := consumptiveDigest(calls[0]); ok {
		for n < len(calls) {
			d, ok := consumptiveDigest(calls[n])
			if !ok || d != d0 {
				break
			}
			n++
		}
	}
	return n
}

// Read implements statestore.Store: every lookup misses.
func (f *Fake) Read(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) {
	if len(calls) == 0 {
		return
	}
	batch := -1
	if len(calls) > 1 {
		batch = 0
	}
	policy := calls[0].Timeout
	for _, c := range calls {
		policy = min(policy, c.Timeout)
	}
	err := f.attempt(ctx, rb, policy)
	for _, c := range calls {
		c.Batch, c.Trip, c.Elapsed = batch, statestore.TripPipeline, 0
		if err != nil {
			c.Done, c.Err = false, err
			continue
		}
		c.Done, c.Err = true, nil
		l := &c.Lookup
		if !l.SkipToken {
			l.Token, l.HasToken = statestore.Token{}, false
		}
		l.Partition, l.Names, l.Found, l.Entry, l.Resident = false, "", false, l.Entry[:0], 0
	}
}

// Write implements statestore.Store: every write applies, but one with a
// zero timeout or an ended context, which is not attempted (RZ-STS-004),
// and every write once closed (RZ-STS-002).
func (f *Fake) Write(ctx context.Context, batch []*statestore.Write) {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	for _, w := range batch {
		w.Err, w.Applied = nil, false
		switch {
		case ctx.Err() != nil || w.Timeout <= 0:
			w.Err = statestore.ErrNotAttempted
		case closed:
			w.Err = statestore.ErrFailed
		default:
			w.Applied = true
		}
	}
}
