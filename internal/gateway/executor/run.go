// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"net/http"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ResponseKind says what produced the client response onResponse runs on
// (spec 04 reqs 36 and 42, spec 05 req 87).
type ResponseKind uint8

// Response kinds.
const (
	// ResponseUpstream is an Upstream response: plain upstreams (a 5xx
	// included) or a merged composition. Every onResponse Policy runs.
	ResponseUpstream ResponseKind = iota
	// ResponseGenerated is a Filter short-circuit (including an
	// onUpstreamRequest one that became the response) or a Node-generated
	// response that is not an Upstream failure, such as 413 RZ-RT-003 or
	// 504 RZ-RT-007. transform.response, cache and ai.semantic-cache do not
	// run on it (spec 04 req 42).
	ResponseGenerated
	// ResponseUpstreamError is Node-generated after the Upstream leg failed
	// (an RZ-UP code, the breaker included). transform.response and
	// ai.semantic-cache do not run on it; cache does, so stale-if-error can
	// replace it (spec 05 req 87).
	ResponseUpstreamError
)

// Run is one request's Filter Chain execution over its client
// RequestState. It is used by the request goroutine only, except BeginLeg,
// which concurrent legs may call. Run implements snapshot.LegHooks, so the
// handler passes it as snapshot.Outbound.Hooks.
//
// The handler calls Request for onRequestHeaders, onRequestBody and
// onRoute, BeginLeg through the Forwarder, Response for onResponse, Log
// for onLog and Finish, and Release after every leg has ended.
type Run struct {
	cursor

	chain *snapshot.Chain
	legs  map[string]*snapshot.LegChain
	store statestore.Store
	// generated is set once a request Phase produced the response.
	generated bool
	logged    bool

	// legMu serializes RequestState.Leg for concurrent legs.
	legMu sync.Mutex

	// Consumptive batching scratch (reused through the pool).
	members []member
	calls   []statestore.Call
	ptrs    []*statestore.Call
	pend    []int
	// noBudget stands in when the RequestState has no budget.
	noBudget statestore.RequestBudget
}

var _ snapshot.LegHooks = (*Run)(nil)

// Begin starts the execution of route chain ch over rs, the request's
// client state. store is the main State Store (snapshot.Snapshot.StateStore)
// the consumptive batch calls; nil when the snapshot has none, which makes
// every prepared call RZ-STS-004. ch may be nil (no Policy).
func (e *Executor) Begin(rs snapshot.RequestState, ch *snapshot.Chain, store statestore.Store) *Run {
	r, _ := e.runs.Get().(*Run)
	if r == nil {
		r = new(Run)
	}
	r.bind(e, rs)
	r.chain, r.store = ch, store
	if ch != nil {
		r.legs = ch.Legs
	}
	return r
}

// BeginLegs starts an execution that only runs upstream legs, over legs
// (snapshot.Snapshot.LegChains): the replay path (R-43), whose synthetic
// exchange has no client Phases.
func (e *Executor) BeginLegs(rs snapshot.RequestState, legs map[string]*snapshot.LegChain) *Run {
	r := e.Begin(rs, nil, nil)
	r.legs = legs
	return r
}

// Release returns r to its pool. Every LegRun must have ended; the handler
// releases the client RequestState itself.
func (r *Run) Release() {
	e := r.e
	r.reset()
	r.chain, r.legs, r.store = nil, nil, nil
	r.generated, r.logged = false, false
	r.noBudget = statestore.RequestBudget{}
	if e != nil {
		e.runs.Put(r)
	}
}

// Request runs one client request Phase: onRequestHeaders, onRequestBody
// or onRoute (other Phases do nothing). It returns the response that
// short-circuits the request (a Filter's Respond, a closed failure, an SPI
// sentinel, or Security rule 1), after which the handler goes to
// onResponse; nil lets the request continue (spec 04 reqs 39 to 43).
//
// In onRequestHeaders, a run of consecutive Consumptive Policies is
// batched into shared State Store round trips (pack 8.7 rule 3), and once
// the auth-class Policies are past, a chain whose every auth-class Policy
// was skipped by spec.when fails with 401 RZ-AUTH-001 carrying each auth
// Policy's challenge (Security rule 1).
func (r *Run) Request(ctx context.Context, ph phase.Phase) *filter.Response {
	if r.chain == nil || !ph.CanShortCircuit() || !ph.ClientLeg() {
		return nil
	}
	list := r.chain.Client[ph]
	if len(list) == 0 {
		return nil
	}
	authPending := ph == phase.OnRequestHeaders && r.chain.AuthPolicies > 0
	for i := 0; i < len(list); {
		p := list[i]
		if authPending && p.Class > phase.ClassAuth {
			authPending = false
			if resp := r.authRule(); resp != nil {
				return r.shortCircuit(resp)
			}
		}
		if ph == phase.OnRequestHeaders && p.Consumptive != nil {
			j := i + 1
			for j < len(list) && list[j].Consumptive != nil {
				j++
			}
			if resp := r.batch(ctx, list[i:j]); resp != nil {
				return r.shortCircuit(resp)
			}
			i = j
			continue
		}
		i++
		run, whenErr := r.admit(ctx, p, ph)
		if !run {
			continue
		}
		if v, resp := r.invoke(ctx, p, ph, false, whenErr); v == respond {
			return r.shortCircuit(resp)
		}
	}
	if authPending {
		if resp := r.authRule(); resp != nil {
			return r.shortCircuit(resp)
		}
	}
	return nil
}

// shortCircuit marks the response generated and returns it.
func (r *Run) shortCircuit(resp *filter.Response) *filter.Response {
	r.generated = true
	return resp
}

// authRule applies Security rule 1 (spec 06 rule 1, spec 04 req 41): when
// the chain holds auth-class Policies and spec.when skipped every one, the
// request fails with 401 RZ-AUTH-001 and every auth Policy's challenge.
func (r *Run) authRule() *filter.Response {
	n := 0
	for _, p := range r.chain.Policies {
		if p.Class != phase.ClassAuth {
			continue
		}
		if decided, skip := r.rs.When(p); !decided || !skip {
			return nil
		}
		n++
	}
	if n == 0 {
		return nil
	}
	var h http.Header
	for _, p := range r.chain.Policies {
		if p.Class != phase.ClassAuth {
			continue
		}
		if ch, ok := p.Filter.(Challenger); ok {
			if v := ch.Challenge(); v != "" {
				if h == nil {
					h = make(http.Header, 1)
				}
				h.Add("WWW-Authenticate", v)
			}
		}
	}
	return &filter.Response{Status: r.e.authSkippedStatus, Code: CodeAuthSkipped, Header: h}
}

// Response runs onResponse, in the compiled (reverse) order, on the client
// response kind describes (spec 04 req 42, spec 07 req 17). A closed
// failure replaces an Upstream response with 502 (RZ-RT-012 or the Filter's
// code), passed to Exchange.ReplaceResponse, and the remaining Policies run
// on that generated response; a failure on a response that is already
// generated is counted and never replaces it again. It returns the
// replacement, or nil.
func (r *Run) Response(ctx context.Context, kind ResponseKind) *filter.Response {
	if r.chain == nil {
		return nil
	}
	list := r.chain.Client[phase.OnResponse]
	if len(list) == 0 {
		return nil
	}
	gen := kind != ResponseUpstream || r.generated
	upstreamErr := kind == ResponseUpstreamError && !r.generated
	var out *filter.Response
	for _, p := range list {
		if gen && excluded(p.Type, upstreamErr) {
			continue
		}
		run, whenErr := r.admit(ctx, p, phase.OnResponse)
		if !run {
			continue
		}
		if v, resp := r.invoke(ctx, p, phase.OnResponse, gen, whenErr); v == replace {
			r.x.ReplaceResponse(resp)
			out, gen, upstreamErr = resp, true, false
		}
	}
	return out
}

// excluded reports whether a Policy type never runs onResponse on a
// generated response: transform.response, ai.semantic-cache, and cache
// unless the response is an Upstream failure (stale-if-error).
func excluded(t v1alpha1.PolicyType, upstreamErr bool) bool {
	switch t {
	case v1alpha1.PolicyTypeTransformResponse, v1alpha1.PolicyTypeAISemanticCache:
		return true
	case v1alpha1.PolicyTypeCache:
		return !upstreamErr
	default:
		return false
	}
}

// Chunk runs the client onChunk subscribers for one chunk (M3 sources; the
// executor accepts subscribers and never calls the State Store from it).
// It reports whether the stream must end: a closed failure or an SPI
// sentinel after commit (FP 4).
func (r *Run) Chunk(ctx context.Context) (end bool) {
	if r.chain == nil {
		return false
	}
	for _, p := range r.chain.Client[phase.OnChunk] {
		run, whenErr := r.admit(ctx, p, phase.OnChunk)
		if !run {
			continue
		}
		if v, _ := r.invoke(ctx, p, phase.OnChunk, true, whenErr); v == respond {
			return true
		}
	}
	return false
}

// Log runs onLog in request order, read-only (failures reach telemetry
// only, no span), then calls Finish on every client Policy that ran and
// implements filter.Finisher, in request order (R-40), whether or not it
// subscribes to onLog. The handler calls it once, after the response
// finished or the client went away; later calls do nothing.
func (r *Run) Log(ctx context.Context) {
	if r.logged || r.chain == nil {
		return
	}
	r.logged = true
	for _, p := range r.chain.Client[phase.OnLog] {
		run, whenErr := r.admit(ctx, p, phase.OnLog)
		if !run {
			continue
		}
		r.invoke(ctx, p, phase.OnLog, true, whenErr)
	}
	r.finishRan(ctx, r.chain.Policies, true)
}
