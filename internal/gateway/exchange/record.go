// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"net/http"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// maxFailureModes caps the access record's failure_modes
// (docs/architecture/10-observability.md "Access logs": at most 8
// undecided Policies (target)). A record keeps one entry more, so the
// access log writer sees the list is longer and flags it as truncated.
const maxFailureModes = 8

// unmatchedRoute is the route of a request no Route matched
// (docs/architecture/10-observability.md "Naming and label rules" and
// "Access logs").
const unmatchedRoute = "_unmatched"

// rejectStatus is the least status of a short-circuit that rejects the
// request, for Final.RejectedAfter: a Filter's 4xx or 5xx denial. A Filter that serves the request, such as a cache hit
// (docs/architecture/09-traffic-management-and-resilience.md "Quotas":
// cache hits are charged) or a CORS preflight, does not reject it.
const rejectStatus = http.StatusBadRequest

// record is what RecordShortCircuit and RecordFailure recorded for one
// request or leg. mu guards a request's record against the legs merging
// into it; a leg's record belongs to its goroutine.
type record struct {
	mu sync.Mutex
	// sc is the Policy that short-circuited, in phase scPhase with
	// scStatus; nil when none did.
	sc       *snapshot.Policy
	scPhase  phase.Phase
	scStatus int
	// rej is the Policy that rejected the request in a request Phase: a
	// short-circuit of rejectStatus or more, or a closed failure; nil when
	// none did (Final.RejectedAfter).
	rej *snapshot.Policy
	// fails are the undecided Policies in record order; nfails counts every
	// one recorded, past the array too.
	fails  [maxFailureModes + 1]emit.FailureModeEntry
	nfails int
}

// shortCircuit records that p responded with status in ph. The first
// short-circuit is the record's: request Phases end at a short-circuit, so
// a request has at most one, and a leg at most one per attempt.
func (r *record) shortCircuit(p *snapshot.Policy, ph phase.Phase, status int) {
	if p == nil {
		return
	}
	if r.sc == nil {
		r.sc, r.scPhase, r.scStatus = p, ph, status
	}
	if r.rej == nil && status >= rejectStatus && ph.CanShortCircuit() {
		r.rej = p
	}
}

// failure records that p could not decide in ph and mode applied.
func (r *record) failure(p *snapshot.Policy, ph phase.Phase, mode v1alpha1.FailureMode) {
	if p == nil {
		return
	}
	r.add(emit.FailureModeEntry{Policy: p.Name, Phase: ph, Mode: string(mode)}, 1)
	if r.rej == nil && mode == v1alpha1.FailureModeClosed && ph.CanShortCircuit() {
		r.rej = p
	}
}

// add appends e while the array has room and counts n entries.
func (r *record) add(e emit.FailureModeEntry, n int) {
	if r.nfails < len(r.fails) {
		r.fails[r.nfails] = e
	}
	r.nfails += n
}

// failures returns the recorded entries, at most maxFailureModes + 1.
func (r *record) failures() []emit.FailureModeEntry {
	return r.fails[:min(r.nfails, len(r.fails))]
}

// merge adds a leg's record to r: its failures after r's, its
// short-circuit when r has none or the leg's Policy comes first in request
// order (concurrent legs merge in any order, the result does not depend on
// it), and its rejection only for a leg of plain upstreams (step "").
func (r *record) merge(l *record, plain bool) {
	if p := l.sc; p != nil && (r.sc == nil || before(p, r.sc)) {
		r.sc, r.scPhase, r.scStatus = p, l.scPhase, l.scStatus
	}
	if plain && r.rej == nil {
		r.rej = l.rej
	}
	kept := l.failures()
	for _, e := range kept {
		r.add(e, 1)
	}
	r.nfails += l.nfails - len(kept)
}

// reset forgets everything recorded.
func (r *record) reset() {
	r.sc, r.scPhase, r.scStatus, r.rej = nil, 0, 0, nil
	clear(r.fails[:])
	r.nfails = 0
}

// RecordShortCircuit implements snapshot.RequestState: p responded with
// status in ph (access record short_circuit).
func (x *Request) RecordShortCircuit(p *snapshot.Policy, ph phase.Phase, status int) {
	x.rec.mu.Lock()
	x.rec.shortCircuit(p, ph, status)
	x.rec.mu.Unlock()
}

// RecordFailure implements snapshot.RequestState: p could not decide in ph
// and mode applied (access record failure_modes).
func (x *Request) RecordFailure(p *snapshot.Policy, ph phase.Phase, mode v1alpha1.FailureMode) {
	x.rec.mu.Lock()
	x.rec.failure(p, ph, mode)
	x.rec.mu.Unlock()
}

// ShortCircuit returns the Policy name and Phase of the request's
// short-circuit and the status it responded with; ok is false when no
// Policy responded. A leg's short-circuit counts once its leg is released.
func (x *Request) ShortCircuit() (policy string, ph phase.Phase, status int, ok bool) {
	x.rec.mu.Lock()
	defer x.rec.mu.Unlock()
	if x.rec.sc == nil {
		return "", 0, 0, false
	}
	return x.rec.sc.Name, x.rec.scPhase, x.rec.scStatus, true
}

// FailureModes returns the undecided Policies with the failureMode applied,
// in record order, the request's own and then each released leg's: at most
// maxFailureModes, plus one more entry when more were recorded, so the
// access log writer flags the list as truncated. The slice is x's memory,
// valid until Put; read it once every leg is released, since a release
// appends to it.
func (x *Request) FailureModes() []emit.FailureModeEntry {
	x.rec.mu.Lock()
	defer x.rec.mu.Unlock()
	return x.rec.failures()
}

// FillAccess sets the access record fields x owns: start time, trace ID,
// listener, route (or _unmatched), method, host, normalized path, user
// agent, consumer, tier and authentication method, the cache result a
// Filter recorded, short_circuit and failure_modes. The handler sets the
// rest. r.FailureModes is x's memory:
// submit r before Put (the writer copies it at Submit).
func (x *Request) FillAccess(r *emit.AccessRecord) {
	r.Start = x.start
	r.TraceID = x.traceID
	r.Listener = x.listener
	r.Route = unmatchedRoute
	if x.route != nil {
		r.Route = x.route.Name
	}
	r.Method, r.Host, r.Path = x.req.Method, x.req.Host, x.path
	if lines := x.hdr["User-Agent"]; len(lines) > 0 {
		r.UserAgent = lines[0]
	}
	if id := x.ident; id != nil {
		r.AuthMethod = id.Method
		if c := id.Consumer; c != nil {
			r.Consumer, r.Tier = c.Name, c.Tier
		}
	}
	r.Cache = x.cache
	x.rec.mu.Lock()
	defer x.rec.mu.Unlock()
	if p := x.rec.sc; p != nil {
		r.ShortCircuitPolicy, r.ShortCircuitPhase = p.Name, x.rec.scPhase
	}
	r.FailureModes = x.rec.failures()
}

// rejectedAfter reports whether a Policy later than p in request order
// rejected the request (Final.RejectedAfter).
func (x *Request) rejectedAfter(p *snapshot.Policy) bool {
	if p == nil {
		return false
	}
	x.rec.mu.Lock()
	rej := x.rec.rej
	x.rec.mu.Unlock()
	return rej != nil && before(p, rej)
}
