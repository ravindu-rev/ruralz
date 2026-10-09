// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package statestoretest is the State Store test kit (spec 08 section 6):
// Fake, a failure-injecting statestore.Driver for Filter, post-commit
// queue and Manager tests; Model, the reference arithmetic of the GCRA
// and Quota scripts; and RunConformance, the driver conformance suite the
// memory driver runs in its unit tests and the redis driver runs against
// local and CI servers. It is imported by tests only (depguard
// testsupport).
package statestoretest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Method names a Store entry point a Rule or Trip refers to.
type Method uint8

// Store entry points.
const (
	// MethodAny matches every entry point in a Rule.
	MethodAny Method = iota
	// MethodConsume is Store.Consume.
	MethodConsume
	// MethodRead is Store.Read.
	MethodRead
	// MethodWrite is Store.Write.
	MethodWrite
)

// Rule injects a failure, a latency or a scripted answer into the round
// trips it matches. Rules are tried in the order they were injected; the
// first match applies.
type Rule struct {
	// Method limits the rule to one entry point; MethodAny matches all.
	Method Method
	// Op matches round trips carrying this kind; 0 matches every kind.
	Op statestore.OpKind
	// Err fails every call of a matched round trip with this sentinel
	// (statestore.ErrTimeout, ErrFailed, ErrBreakerOpen, ErrNotAttempted
	// or ErrUnsupported), after the unsupported and per-request deadline
	// checks, which keep their precedence (spec 08 req 33). Nil injects
	// only Latency or Answer.
	Err *statestore.Error
	// Latency is the simulated round-trip time reported in Call.Elapsed
	// and passed to Fake.Advance. A latency at or above the round trip's
	// timeout fails it with RZ-STS-001 at the timeout.
	Latency time.Duration
	// Apply lets the answering store still apply a round trip that
	// timed out, as a server may (spec 08 req 34: a timed-out
	// consumptive call can over-charge, never under-charge).
	Apply bool
	// Times is how many round trips the rule affects; 0 means until
	// Clear.
	Times int
	// Answer, when set, answers the calls of a matched Consume or Read
	// round trip in place of the answering store; the Fake marks each
	// answered call Done. It must set the outcome of every call it is
	// given. In Consume, as on the real drivers (spec 08 reqs 29, 38 and
	// 44), the group stops at the first call Answer leaves denied (a GCRA
	// or Quota call with Allowed false): later calls are not passed to
	// Answer and stay !Done with a nil Err.
	Answer func(c *statestore.Call)
	// AnswerWrite, when set, answers every write of a matched Write
	// batch; the Fake marks it Applied unless it sets Err.
	AnswerWrite func(w *statestore.Write)

	hits int
}

// Trip records one Consume or Read round trip, or one Write batch.
type Trip struct {
	Method Method
	// Ops are the kinds carried, in order.
	Ops []statestore.OpKind
	// Timeout is the round trip's timeout after the per-request deadline
	// clamp; 0 when it was not attempted.
	Timeout time.Duration
	// Err is the failure every call of the round trip got, or nil.
	Err *statestore.Error
}

// Fake is a failure-injecting statestore.Driver. Calls that no Rule
// fails go to Inner, typically the memory driver; with a nil Inner every
// consumptive call is allowed (Remaining 0), every lookup misses and
// every write applies. Adjacent consumptive calls merge into one
// script_multi round trip while they share a hash slot (with Slot set,
// as on the real drivers) or a config.key digest (without it, a subset
// of same-slot merging). The zero value is ready to use; Fake is safe
// for concurrent use.
type Fake struct {
	// Inner answers what no Rule intercepts; nil answers by default.
	Inner statestore.Store
	// Slot, when set, returns the hash slot of a call's keys, or -1 for a
	// call without one (tests pass keys.SlotOf): adjacent calls then merge
	// while their slots are equal, as on the real drivers, so round-trip
	// counts match them. Nil merges adjacent consumptive calls with an
	// equal config.key digest. Slot must not merge calls that Inner
	// answers in separate round trips: when one of Inner's chunks fails,
	// the later calls of the Fake's group stay not done without an error,
	// where a real driver fails every call of the group (RZ-STS-002).
	// keys.SlotOf with the memory driver as Inner groups exactly as Inner
	// does.
	Slot func(c *statestore.Call) int
	// Clock is the Node clock of per-request deadlines; nil means
	// clock.Real().
	Clock clock.Clock
	// Advance, when set, is called with each simulated latency, so a
	// fake clock moves with the round trip.
	Advance func(d time.Duration)
	// Caps are the supported command sets; 0 means CapScripts. A
	// consumptive call without CapScripts fails with RZ-STS-005.
	Caps statestore.Capability
	// Local makes RoundTrips report false, like the memory driver.
	Local bool
	// Admit decides AdmitStoreBytes; nil asks Inner, else admits.
	Admit func(k statestore.CacheKey, n int) bool

	mu     sync.Mutex
	rules  []*Rule
	trips  []Trip
	status statestore.Status
	closed bool
}

var _ statestore.Driver = (*Fake)(nil)

// Inject adds r and returns the stored copy, whose Hits Fake.Hits reads.
func (f *Fake) Inject(r Rule) *Rule {
	f.mu.Lock()
	defer f.mu.Unlock()
	stored := r
	stored.hits = 0
	f.rules = append(f.rules, &stored)
	return &stored
}

// Hits returns how many round trips r affected.
func (f *Fake) Hits(r *Rule) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return r.hits
}

// Clear removes every rule.
func (f *Fake) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = nil
}

// Trips returns the recorded round trips, oldest first.
func (f *Fake) Trips() []Trip {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.trips)
}

// ResetTrips forgets the recorded round trips.
func (f *Fake) ResetTrips() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trips = nil
}

// SetStatus sets what Status reports (degraded reasons, cleartext).
func (f *Fake) SetStatus(s statestore.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
}

// Status implements statestore.Driver.
func (f *Fake) Status() statestore.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// Close implements statestore.Driver; later calls fail with RZ-STS-002.
// It does not close Inner, which the test owns.
func (f *Fake) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// Closed reports whether Close was called.
func (f *Fake) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// Supports implements statestore.Store.
func (f *Fake) Supports(c statestore.Capability) bool {
	caps := f.Caps
	if caps == 0 {
		caps = statestore.CapScripts
	}
	return caps&c == c
}

// RoundTrips implements statestore.Store.
func (f *Fake) RoundTrips() bool { return !f.Local }

// AdmitStoreBytes implements statestore.Store.
func (f *Fake) AdmitStoreBytes(k statestore.CacheKey, n int) bool {
	switch {
	case f.Admit != nil:
		return f.Admit(k, n)
	case f.Inner != nil:
		return f.Inner.AdmitStoreBytes(k, n)
	default:
		return true
	}
}

func (f *Fake) now() time.Time {
	if f.Clock == nil {
		return clock.Real().Now()
	}
	return f.Clock.Now()
}

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

// match returns the first rule matching method and ops, counting the
// hit; the caller holds f.mu.
func (f *Fake) match(m Method, ops []statestore.OpKind) *Rule {
	for i, r := range f.rules {
		if r.Method != MethodAny && r.Method != m {
			continue
		}
		if r.Op != 0 && !slices.Contains(ops, r.Op) {
			continue
		}
		r.hits++
		if r.Times > 0 && r.hits >= r.Times {
			f.rules = slices.Delete(f.rules, i, i+1)
		}
		return r
	}
	return nil
}

// outcome is what one round trip's pre-checks and rules decided.
type outcome struct {
	err     *statestore.Error
	timeout time.Duration
	elapsed time.Duration
	rule    *Rule
	apply   bool // the answering store still runs the calls
}

// decide runs the pre-call checks in the order of spec 08 req 33
// (unsupported, per-request deadline, then injected breaker, in-flight,
// timeout and error failures) and records the trip.
func (f *Fake) decide(ctx context.Context, m Method, rb *statestore.RequestBudget, ops []statestore.OpKind, policy time.Duration, scripts bool) outcome {
	var o outcome
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	switch {
	case scripts && !f.Supports(statestore.CapScripts):
		o.err = statestore.ErrUnsupported
	default:
		if t, ok := f.budget(ctx, rb, policy); ok {
			o.timeout = t
		} else {
			o.err = statestore.ErrNotAttempted
		}
	}
	if o.err == nil {
		f.mu.Lock()
		o.rule = f.match(m, ops)
		f.mu.Unlock()
	}
	o.apply = o.err == nil
	if r := o.rule; r != nil {
		o.elapsed = r.Latency
		switch {
		case r.Err == statestore.ErrTimeout || (r.Err == nil && r.Latency >= o.timeout):
			o.err, o.elapsed, o.apply = statestore.ErrTimeout, o.timeout, r.Apply
		case r.Err != nil:
			o.err, o.apply = r.Err, false
			if r.Err.Result() == statestore.ResultSkipped {
				o.elapsed = 0 // skipped calls never reach the network
			}
		}
	}
	if o.err == nil && closed {
		o.err, o.apply = statestore.ErrFailed, false
	}
	if o.elapsed > 0 && f.Advance != nil {
		f.Advance(o.elapsed)
	}
	f.mu.Lock()
	f.trips = append(f.trips, Trip{Method: m, Ops: ops, Timeout: o.timeout, Err: o.err})
	f.mu.Unlock()
	return o
}

// budget clamps the round trip's timeout by the per-request deadline, or
// by the context deadline alone without a budget.
func (f *Fake) budget(ctx context.Context, rb *statestore.RequestBudget, policy time.Duration) (time.Duration, bool) {
	if ctx.Err() != nil {
		return 0, false
	}
	now := f.now()
	if rb != nil {
		return rb.CallTimeout(ctx, now, policy)
	}
	if dl, ok := ctx.Deadline(); ok {
		policy = min(policy, dl.Sub(now))
	}
	return policy, policy > 0
}

// Consume implements statestore.Store.
func (f *Fake) Consume(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) int {
	if len(calls) == 0 {
		return 0
	}
	n := f.groupLen(calls)
	group := calls[:n]
	trip, batch := statestore.TripSingle, -1
	if n > 1 {
		trip, batch = statestore.TripScriptMulti, 0
	}
	ops := make([]statestore.OpKind, n)
	policy := group[0].Timeout
	for i, c := range group {
		ops[i] = c.Kind
		policy = min(policy, c.Timeout)
	}
	_, consumptive := consumptiveDigest(group[0])
	o := f.decide(ctx, MethodConsume, rb, ops, policy, consumptive)
	if o.apply {
		f.answerConsume(ctx, rb, group, o.rule)
	}
	for _, c := range group {
		c.Batch, c.Trip, c.Elapsed = batch, trip, o.elapsed
		if o.err != nil {
			c.Done, c.Err = false, o.err
		}
	}
	return n
}

// groupLen returns the length of the longest prefix of calls (at least
// one) that merges into one round trip: equal hash slots with Slot set,
// else equal config.key digests of consumptive calls.
func (f *Fake) groupLen(calls []*statestore.Call) int {
	n := 1
	if f.Slot != nil {
		if s0 := f.Slot(calls[0]); s0 >= 0 {
			for n < len(calls) && f.Slot(calls[n]) == s0 {
				n++
			}
		}
		return n
	}
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

// denied reports whether an answered consumptive call was denied, which
// ends its merged group.
func denied(c *statestore.Call) bool {
	switch c.Kind {
	case statestore.OpGCRA:
		return !c.GCRA.Allowed
	case statestore.OpQuota:
		return !c.Quota.Allowed
	default:
		return false
	}
}

// answerConsume answers an attempted group: a rule's Answer, Inner or
// the default allow.
func (f *Fake) answerConsume(ctx context.Context, rb *statestore.RequestBudget, group []*statestore.Call, r *Rule) {
	switch {
	case r != nil && r.Answer != nil:
		for _, c := range group {
			c.Done, c.Err = false, nil
		}
		for _, c := range group {
			r.Answer(c)
			c.Done = true
			if denied(c) {
				break
			}
		}
	case f.Inner != nil:
		// Inner merges at least the calls Fake merges unless a custom Slot
		// merges more; then the group still stops at a deny or failure,
		// and the calls after a failed chunk carry no error (see Slot).
		for _, c := range group {
			c.Done, c.Err = false, nil
		}
		for taken := 0; taken < len(group); {
			k := max(f.Inner.Consume(ctx, rb, group[taken:]), 1)
			chunk := group[taken : taken+k]
			taken += k
			if slices.ContainsFunc(chunk, func(c *statestore.Call) bool { return !c.Done || denied(c) }) {
				break
			}
		}
	default:
		now := f.now()
		for _, c := range group {
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
	}
}

// Read implements statestore.Store.
func (f *Fake) Read(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) {
	if len(calls) == 0 {
		return
	}
	batch := -1
	if len(calls) > 1 {
		batch = 0
	}
	ops := make([]statestore.OpKind, len(calls))
	policy := calls[0].Timeout
	for i, c := range calls {
		ops[i] = c.Kind
		policy = min(policy, c.Timeout)
	}
	o := f.decide(ctx, MethodRead, rb, ops, policy, false)
	if o.apply {
		switch r := o.rule; {
		case r != nil && r.Answer != nil:
			for _, c := range calls {
				c.Done, c.Err = false, nil
				r.Answer(c)
				c.Done = true
			}
		case f.Inner != nil:
			f.Inner.Read(ctx, rb, calls)
		default:
			for _, c := range calls {
				c.Done, c.Err = true, nil
				c.Lookup.Generation, c.Lookup.Names, c.Lookup.Found = 0, "", false
				c.Lookup.EntryGen, c.Lookup.Entry = 0, c.Lookup.Entry[:0]
			}
		}
	}
	for _, c := range calls {
		c.Batch, c.Trip, c.Elapsed = batch, statestore.TripPipeline, o.elapsed
		if o.err != nil {
			c.Done, c.Err = false, o.err
		}
	}
}

// Write implements statestore.Store. Rules apply per write; a write with
// a zero timeout is not attempted (RZ-STS-004).
func (f *Fake) Write(ctx context.Context, batch []*statestore.Write) {
	if len(batch) == 0 {
		return
	}
	var pass []*statestore.Write
	var answered []*statestore.Write
	var rules []*Rule
	f.mu.Lock()
	closed := f.closed
	ops := make([]statestore.OpKind, len(batch))
	for i, w := range batch {
		ops[i] = w.Kind
		w.Err, w.Applied = nil, false
		switch {
		case ctx.Err() != nil || w.Timeout <= 0:
			w.Err = statestore.ErrNotAttempted
			continue
		case closed:
			w.Err = statestore.ErrFailed
			continue
		}
		r := f.match(MethodWrite, []statestore.OpKind{w.Kind})
		switch {
		case r == nil:
			pass = append(pass, w)
		case r.Err != nil || r.Latency >= w.Timeout:
			w.Err = r.Err
			if w.Err == nil {
				w.Err = statestore.ErrTimeout
			}
		case r.AnswerWrite != nil:
			answered = append(answered, w)
			rules = append(rules, r)
		default:
			pass = append(pass, w)
		}
	}
	f.trips = append(f.trips, Trip{Method: MethodWrite, Ops: ops})
	f.mu.Unlock()
	for i, w := range answered {
		rules[i].AnswerWrite(w)
		w.Applied = w.Err == nil
	}
	if len(pass) == 0 {
		return
	}
	if f.Inner != nil {
		f.Inner.Write(ctx, pass)
		return
	}
	for _, w := range pass {
		w.Applied = true
	}
}
