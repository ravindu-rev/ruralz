// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"log/slog"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// memberState is where one Consumptive member of a batch stands.
type memberState uint8

const (
	// pending: Prepare filled the member's call; it waits for a round trip.
	pending memberState = iota
	// admitted: Prepare decided locally and the member continued.
	admitted
	// settled: the member is decided (Complete ran, or it failed open).
	settled
)

// member is one Consumptive Policy of a batch; its call is Run.calls and
// its span context the batch's ctxs element at the member's index.
type member struct {
	p    *snapshot.Policy
	c    filter.Consumptive
	span emit.Span
	// whenErr is admit's mark for span (spec 03 req 43).
	whenErr string
	d       time.Duration
	state   memberState
}

// batchContexts is how many members' span contexts a batch keeps on the
// stack; a longer run of Consumptive Policies allocates its slice.
const batchContexts = 8

// batch runs a run of consecutive Consumptive Policies of onRequestHeaders
// (pack 8.7 rule 3; spec 05 reqs 64 and 91; spec 08 reqs 27 to 29 and 38):
//
//  1. In chain order, each admitted member's Prepare runs every Node-local
//     step on its own PolicyState. A member that decides locally is
//     settled at once; a local deny or a closed failure ends the batch
//     with zero State Store commands, and the members still waiting get
//     Undo. Those members come before the denier in chain order, and their
//     round trips are never sent; the filter.Consumptive.Undo doc names
//     only "an earlier member denied", and a contract request to the
//     filter SPI owner extends it to this case (spec 05 req 64 covers the
//     members after the deny).
//  2. The prepared calls go to Store.Consume in chain order with the
//     request's RequestBudget: one round trip for the longest prefix whose
//     keys share a hash slot, repeated for the rest, so each member makes
//     at most one blocking round trip.
//  3. Complete settles each member in chain order; the first deny or
//     closed failure (every call of a failed round trip carries the same
//     failure, and each member applies its own failureMode) ends the
//     request, and every later member that took local tokens gets Undo.
//
// Prepare, Complete and Undo get the member's ruralz.filter.<name> span
// context, as Handle does. The contexts live in a local array, never in
// the pooled Run (a context is never stored in a struct).
func (r *Run) batch(ctx context.Context, list []*snapshot.Policy) *filter.Response {
	const ph = phase.OnRequestHeaders
	if cap(r.calls) < len(list) {
		r.calls = make([]statestore.Call, len(list))
	}
	r.calls = r.calls[:len(list)]
	ms := r.members[:0]
	defer r.endBatch()
	var buf [batchContexts]context.Context
	ctxs := buf[:0]
	if len(list) > len(buf) {
		ctxs = make([]context.Context, 0, len(list))
	}

	for _, p := range list {
		run, whenErr := r.admit(ctx, p, ph)
		if !run {
			continue
		}
		sctx, span := r.e.startSpan(ctx, p, ph)
		call := &r.calls[len(ms)]
		*call = statestore.Call{Batch: -1}
		r.rs.Enter(p, ph, span)
		t0 := r.e.clock.Now()
		res, done := r.e.prepare(sctx, p, p.Consumptive, r.x, call)
		d := r.e.clock.Since(t0)
		m := member{p: p, c: p.Consumptive, span: span, whenErr: whenErr, d: d, state: pending}
		if !done {
			if call.Timeout <= 0 {
				call.Timeout = p.StateStoreTimeout
			}
			ms, ctxs = append(ms, m), append(ctxs, sctx)
			r.members = ms
			continue
		}
		v, resp := r.settle(ctx, p, ph, res, span, d, false, whenErr)
		if v == respond {
			// A local deny sends nothing: the members waiting for a
			// round trip return what they took.
			r.undo(ms, ctxs, false) //nolint:contextcheck // ctxs hold the members' span contexts, derived from ctx by startSpan.
			return resp
		}
		m.state = admitted
		if r.isFailed(p) {
			m.state = settled
		}
		ms, ctxs = append(ms, m), append(ctxs, sctx)
		r.members = ms
	}

	ptrs, pend := r.ptrs[:0], r.pend[:0]
	for i := range ms {
		if ms[i].state == pending {
			ptrs = append(ptrs, &r.calls[i])
			pend = append(pend, i)
		}
	}
	r.ptrs, r.pend = ptrs, pend
	budget := r.rs.Budget()
	if budget == nil {
		budget = &r.noBudget
	}
	for k := 0; k < len(ptrs); {
		rest := ptrs[k:]
		t0 := r.e.clock.Now()
		n := len(rest)
		if r.store != nil {
			n = min(max(r.store.Consume(ctx, budget, rest), 1), len(rest))
		} else {
			for _, c := range rest {
				c.Err = statestore.ErrNotAttempted
			}
		}
		trip := r.e.clock.Since(t0)
		for q := k; q < k+n; q++ {
			i := pend[q]
			m := &ms[i]
			call := ptrs[q]
			m.d += trip
			if !call.Done && call.Err == nil {
				// Not decided although no earlier member denied: the
				// member applies its failureMode (RZ-STS-004).
				call.Err = statestore.ErrNotAttempted
			}
			r.stateAttrs(m.span, call, q, k, n)
			r.rs.Enter(m.p, ph, m.span)
			t1 := r.e.clock.Now()
			res := r.e.complete(ctxs[i], m.p, m.c, r.x, call) //nolint:contextcheck // ctxs[i] is the member's span context, derived from ctx by startSpan.
			m.d += r.e.clock.Since(t1)
			m.state = settled
			if v, resp := r.settle(ctx, m.p, ph, res, m.span, m.d, false, m.whenErr); v == respond {
				r.undo(ms[i+1:], ctxs[i+1:], true) //nolint:contextcheck // ctxs hold the members' span contexts, derived from ctx by startSpan.
				return resp
			}
		}
		k += n
	}
	return nil
}

// endBatch clears the batch scratch so the pooled Run retains no Policy,
// span or Filter-owned slice.
func (r *Run) endBatch() {
	clear(r.members)
	clear(r.calls)
	clear(r.ptrs)
	r.members, r.ptrs, r.pend = r.members[:0], r.ptrs[:0], r.pend[:0]
}

// undo returns local tokens after a deny or closed failure: every pending
// member of ms gets Undo, with its span context from ctxs (indexed like
// ms), and its span ends as skipped; with all, members that were admitted
// locally get Undo too (they follow the first deny in chain order).
func (r *Run) undo(ms []member, ctxs []context.Context, all bool) {
	for i := range ms {
		m := &ms[i]
		switch m.state {
		case pending:
			r.rs.Enter(m.p, phase.OnRequestHeaders, m.span)
			r.e.undo(ctxs[i], m.p, m.c, r.x)
			if pm := m.p.Metrics; pm != nil && pm.Duration[phase.OnRequestHeaders] != nil {
				pm.Duration[phase.OnRequestHeaders].Record(r.stripe, nanos(m.d))
			}
			endSpan(m.span, emit.OutcomeSkipped, whenMode(m.whenErr), 0, "", m.whenErr)
			m.state = settled
		case admitted:
			if all {
				r.rs.Enter(m.p, phase.OnRequestHeaders, nil)
				r.e.undo(ctxs[i], m.p, m.c, r.x)
				m.state = settled
			}
		case settled:
		}
	}
}

// stateAttrs puts a round trip on the span of the member whose call records
// it and names that member in ruralz.state.batch on the others (spec 08
// req 65). call is ptrs[q] of a round trip over ptrs[k:k+n]; Call.Batch
// indexes that round trip's calls.
func (r *Run) stateAttrs(span emit.Span, call *statestore.Call, q, k, n int) {
	if span == nil {
		return
	}
	if b := call.Batch; b >= 0 && b < n && k+b != q {
		span.SetAttr(catalog.AttrStateBatch, slog.StringValue(r.members[r.pend[k+b]].p.Name))
		return
	}
	if op := r.e.opName(call); op != "" {
		span.SetAttr(catalog.AttrStateOp, slog.StringValue(op))
	}
	span.SetAttr(catalog.AttrStateDuration, slog.DurationValue(call.Elapsed))
}
