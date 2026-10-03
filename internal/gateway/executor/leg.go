// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// BeginLeg starts one upstream leg (snapshot.LegHooks, R-42): an Upstream
// leg of plain upstreams (step "") or one composition step. The leg runs
// its Upstream's leg chain on its own leg state (RequestState.Leg: its own
// Leg, Message, Vars copy, PolicyState slots and when decisions), so
// concurrent legs of an aggregate composition share nothing mutable; the
// client request is read-only meanwhile. BeginLeg is safe for concurrent
// use. An Upstream without leg Policies gets a LegRun that does nothing.
func (r *Run) BeginLeg(_ context.Context, upstream, step string) snapshot.LegRun {
	lc := r.legs[upstream]
	if lc == nil || !hasLegPolicies(lc) {
		return noLeg{}
	}
	r.legMu.Lock()
	ls := r.rs.Leg(upstream, step)
	r.legMu.Unlock()
	l, _ := r.e.legs.Get().(*legRun)
	if l == nil {
		l = new(legRun)
	}
	l.bind(r.e, ls)
	l.step, l.track = step != "", true
	l.chain = lc
	return l
}

// hasLegPolicies reports whether any leg Phase of lc has a subscriber.
func hasLegPolicies(lc *snapshot.LegChain) bool {
	for ph := range lc.Phases {
		if len(lc.Phases[ph]) > 0 {
			return true
		}
	}
	return false
}

// legRun runs one leg's Phases on its leg state; its methods are called
// from the leg's goroutine only (snapshot.LegRun).
type legRun struct {
	cursor
	chain *snapshot.LegChain
}

var _ snapshot.LegRun = (*legRun)(nil)

// OnUpstreamRequest runs onUpstreamRequest for one attempt (spec 04 reqs
// 39 and 42). A non-nil response ends the leg without retry: a Filter's
// Respond, or a closed failure (503 RZ-RT-011, 401 RZ-AUTH-020 for
// upstream-auth, spec 07 req 16).
func (l *legRun) OnUpstreamRequest(ctx context.Context, leg *filter.Leg) *filter.Response {
	const ph = phase.OnUpstreamRequest
	l.rs.SetLeg(leg)
	for _, p := range l.chain.Phases[ph] {
		run, whenErr := l.admit(ctx, p, ph)
		if !run {
			continue
		}
		if v, resp := l.invoke(ctx, p, ph, false, whenErr); v == respond {
			return resp
		}
	}
	return nil
}

// OnUpstreamResponseHeaders runs onUpstreamResponseHeaders for one attempt,
// in the compiled (reverse) order. A Filter's Retry (R-44) skips the rest
// of the Phase for this attempt and returns retry true; the Upstream layer
// still applies every other retry condition (spec 05 req 33). A closed
// failure returns the leg's replacement response (502 RZ-RT-012), which is
// final and triggers no retry (spec 07 req 16).
func (l *legRun) OnUpstreamResponseHeaders(ctx context.Context, leg *filter.Leg) (retryAttempt bool, replacement *filter.Response) {
	const ph = phase.OnUpstreamResponseHeaders
	l.rs.SetLeg(leg)
	for _, p := range l.chain.Phases[ph] {
		run, whenErr := l.admit(ctx, p, ph)
		if !run {
			continue
		}
		switch v, resp := l.invoke(ctx, p, ph, false, whenErr); v {
		case retry:
			return true, nil
		case replace:
			return false, resp
		case next, respond:
		}
	}
	return false, nil
}

// OnUpstreamResponseBody runs onUpstreamResponseBody once per leg; a
// closed failure returns the leg's replacement response.
func (l *legRun) OnUpstreamResponseBody(ctx context.Context, leg *filter.Leg) *filter.Response {
	const ph = phase.OnUpstreamResponseBody
	l.rs.SetLeg(leg)
	for _, p := range l.chain.Phases[ph] {
		run, whenErr := l.admit(ctx, p, ph)
		if !run {
			continue
		}
		if v, resp := l.invoke(ctx, p, ph, false, whenErr); v == replace {
			return resp
		}
	}
	return nil
}

// End finishes the leg: Finish on every leg Policy that ran and implements
// filter.Finisher, in request order (class, scope, position), then the leg
// state is released and the LegRun returned to its pool. End must be called
// exactly once, after the leg's last attempt.
func (l *legRun) End(ctx context.Context) {
	if l.rs == nil {
		return
	}
	slices.SortStableFunc(l.ran, requestOrder)
	l.finishRan(ctx, l.ran, false)
	l.rs.Release()
	e := l.e
	l.reset()
	l.chain = nil
	e.legs.Put(l)
}

// requestOrder orders Policies as a request Phase runs them: Filter class,
// then scope, then list position (FP 8.12).
func requestOrder(a, b *snapshot.Policy) int {
	switch {
	case a.Class != b.Class:
		return int(a.Class) - int(b.Class)
	case a.Scope != b.Scope:
		return int(a.Scope) - int(b.Scope)
	default:
		return a.Position - b.Position
	}
}

// noLeg is the LegRun of an Upstream without leg Policies.
type noLeg struct{}

// OnUpstreamRequest does nothing.
func (noLeg) OnUpstreamRequest(context.Context, *filter.Leg) *filter.Response { return nil }

// OnUpstreamResponseHeaders does nothing.
func (noLeg) OnUpstreamResponseHeaders(context.Context, *filter.Leg) (bool, *filter.Response) {
	return false, nil
}

// OnUpstreamResponseBody does nothing.
func (noLeg) OnUpstreamResponseBody(context.Context, *filter.Leg) *filter.Response { return nil }

// End does nothing.
func (noLeg) End(context.Context) {}
