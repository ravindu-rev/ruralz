// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"encoding/hex"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Start is what the handler knows when a request starts, before routing.
type Start struct {
	// TraceID is the request's trace ID (emit.Decision.TraceID); RequestID
	// is its 32-hex form.
	TraceID [16]byte
	// Time is the request start: Exchange.Now, CEL now (in UTC), the access
	// record's time and the gateway-added time origin.
	Time time.Time
	// Listener is the listener name.
	Listener string
	// Scheme is "http" or "https".
	Scheme string
	// Path is the one normalized path (routematch.NormalizePath).
	Path string
	// Source is the client address after trusted-proxy and PROXY v2
	// handling.
	Source filter.Source
	// TLS is the connection's TLS state; nil on cleartext.
	TLS *filter.ConnTLS
	// Stripe is the request's metric stripe.
	Stripe emit.Stripe
	// Limits are the pinned snapshot's effective limits.
	Limits snapshot.Limits
}

// Request is one client request's pooled state: the snapshot.RequestState
// the executor drives (its client state), whose Exchange is the
// filter.Exchange Filters see. It belongs to the request goroutine between
// Pool.Get and Pool.Put; while the Forwarder runs, its legs read it
// concurrently and only their release and the shared request body view
// write to it, under locks.
type Request struct {
	pool *Pool
	// live is set between Get and Put; gen moves on at each Put that ends
	// legs, so a leg issued before it merges nothing into the request after
	// it.
	live atomic.Bool
	gen  atomic.Uint64

	// legs are the handles of the legs issued since Get, which Put ends if
	// still live; legMu guards them against concurrent Leg calls, and
	// legsIssued tells Put whether there are any.
	legMu      sync.Mutex
	legs       []*legHandle
	legsIssued atomic.Bool

	// Request data from Begin.
	traceID  [16]byte
	idHex    [32]byte
	id       atomic.Pointer[string]
	start    time.Time
	listener string
	path     string
	hdr      http.Header
	src      filter.Source
	tls      *filter.ConnTLS
	stripe   emit.Stripe
	limits   snapshot.Limits
	hreq     *http.Request

	route  *snapshot.Route
	budget statestore.RequestBudget

	// The CEL activation and the views it points at.
	vars    expr.Vars
	req     expr.Request
	srcView expr.Source
	auth    expr.Auth
	respV   expr.Response
	up      expr.Upstream
	params  []expr.Param

	match snapshot.MatchRequest
	out   snapshot.Outbound
	timer emit.GatewayTimer

	// The Filter call Enter selected.
	cur   *snapshot.Policy
	ph    phase.Phase
	span  emit.Span
	whens decisions
	slots slots

	ident *filter.Identity
	// acct charges decoded values and Filter reservations.
	acct body.Account

	// The request body, whether PrepareBody gated it, and its decoded view.
	reqBody body.Request
	gated   bool
	bodyDec sharedDecoded

	// The client response and its gated or replaced body.
	resp     response
	respBody body.Request
	respDec  decoded
	// The leg response kept for the Response Cache store
	// (filter.Exchange.LegResponse, TeeResponse): its status, a copy of its
	// header on a Route that tees, and, once an onResponse Policy replaced
	// a gated body, a copy of the body the leg delivered, charged to acct.
	// legLost is set when that body can no longer be copied (a streamed
	// body replaced whole, or a refused reservation, legLostBudget).
	legStatus     int
	legHdr        http.Header
	legBody       []byte
	legKept       bool
	legLost       bool
	legLostBudget bool
	// genHdr are two header maps a generated response's fields are copied
	// into, alternately, so a replacement can be built from the current
	// one; genIdx is the one in use.
	genHdr [2]http.Header
	genIdx int

	rateLimits []filter.RateLimitField
	// rateLimitBy holds the Policy that recorded each rateLimits element.
	rateLimitBy []*snapshot.Policy
	tees        []*body.Tee
	// cache is the cache result a Filter recorded (SetCacheResult).
	cache string
	final filter.Final
	// charges are the Quota counters the request charged (Charge).
	charges charges

	rec record
}

var _ snapshot.RequestState = (*Request)(nil)

// Begin starts x for r, whose Body and ContentLength PrepareBody reads
// later. It builds the pre-route activation (request without body, source,
// now, no deadline) and the Router's input with the request start, binds
// the buffer account and resets the gateway-added timer. Until
// PrepareBody, the request body is empty.
func (x *Request) Begin(r *http.Request, s *Start) {
	x.hreq = r
	x.traceID = s.TraceID
	hex.Encode(x.idHex[:], s.TraceID[:])
	x.start, x.listener, x.path = s.Time, s.Listener, s.Path
	x.src, x.tls, x.stripe, x.limits = s.Source, s.TLS, s.Stripe, s.Limits
	x.hdr = r.Header
	if x.hdr == nil {
		x.hdr = http.Header{}
		r.Header = x.hdr
	}
	x.budget = statestore.NewRequestBudget(0, s.Stripe)

	x.req = expr.Request{
		Method: r.Method, Scheme: s.Scheme, Host: routematch.NormalizeHost(r.Host),
		Path: s.Path, PathParams: x.params[:0], Header: x.hdr,
	}
	if r.URL != nil {
		x.req.SetRawQuery(r.URL.RawQuery)
	}
	x.srcView = expr.Source{IP: s.Source.IP, Port: int(s.Source.Port), TLSVersion: tlsVersion(s.TLS)}
	x.vars = expr.Vars{Request: &x.req, Source: &x.srcView, Now: s.Time.UTC()}
	x.match = snapshot.MatchRequest{Host: r.Host, Path: s.Path, Method: r.Method, Header: x.hdr, Vars: &x.vars, Start: s.Time}
	x.timer.Reset(s.Time)

	x.acct.Bind(x.pool.budget, body.Gate, s.Stripe)
	x.out = snapshot.Outbound{X: (*view)(x), Body: &x.reqBody, Stripe: s.Stripe, Excluder: &x.timer}
}

// tlsVersion returns the negotiated TLS version, 0 on cleartext.
func tlsVersion(t *filter.ConnTLS) uint16 {
	if t == nil || t.State == nil {
		return 0
	}
	return t.State.Version
}

// MatchRequest returns the Router's input: the request host as received
// (the Router normalizes it), the normalized path, method, headers, the
// pre-route activation, whose request.pathParams and request deadline
// Match sets, and the request start, from which each candidate's Route
// deadline counts.
func (x *Request) MatchRequest() *snapshot.MatchRequest { return &x.match }

// ParamBuf returns empty storage for the Router's captures. Its capacity
// grows to the most captures a match on x returned, so Match stores them
// without allocating once x is warm.
func (x *Request) ParamBuf() []expr.Param { return x.params[:0] }

// SetRoute fixes the matched Route rt and its template captures (the
// MatchResult's Params): CEL route and request.pathParams, the Route
// deadline, the Route's metric handles and the State Store budget of its
// largest stateStoreTimeout. The Route deadline, the request start plus
// rt's timeout as the handler and a Response Cache replay date it, is the
// activation's request deadline (expr.Vars.Deadline): every later CEL
// evaluation of the request, on the client and in its legs, onLog's
// included, stops a tracked comprehension at it
// (docs/architecture/02-configuration-model.md "Limits"). A nil rt leaves
// the request unmatched, without a deadline, as does a Route without a
// timeout, which the snapshot compiler never builds.
func (x *Request) SetRoute(rt *snapshot.Route, params []expr.Param) {
	x.route = rt
	x.req.PathParams = params
	if cap(params) > cap(x.params) {
		x.params = params[:0]
	}
	x.vars.Route, x.vars.Deadline = nil, time.Time{}
	maxWait := time.Duration(0)
	if rt != nil {
		x.vars.Route = rt.View
		if rt.Timeout > 0 {
			x.vars.Deadline = x.start.Add(rt.Timeout)
		}
		maxWait = rt.StateStoreMax
	}
	x.budget = statestore.NewRequestBudget(maxWait, x.stripe)
}

// Route returns the matched Route, nil when none.
func (x *Request) Route() *snapshot.Route { return x.route }

// PrepareBody sets the request body up after onRequestHeaders, for
// onRequestBody and the Forwarder (body.Request.Prepare): when the Route
// gates it (BodyNeeds.RequestGate) the whole body is read now within
// limits.maxRequestBodyBytes, otherwise it streams, cut at that limit. A
// declared length over the limit is filter.ErrTooLarge before any byte is
// read; body.Code maps the errors with body.SideRequest.
func (x *Request) PrepareBody(ctx context.Context) error {
	gate := x.route != nil && x.route.Body.RequestGate
	var (
		rd       = x.hreq.Body
		declared = x.hreq.ContentLength
	)
	x.invalidateRequestView()
	x.gated = false
	if err := x.reqBody.Prepare(ctx, rd, declared, gate, x.limits.MaxRequestBodyBytes, x.pool.budget, x.stripe); err != nil {
		return err
	}
	x.gated = gate
	return nil
}

// RequestBody returns the request body, the snapshot.RequestBody of
// Outbound.Body.
func (x *Request) RequestBody() *body.Request { return &x.reqBody }

// Outbound returns the Forwarder's input with X, Body, Stripe and Excluder
// (the request's gateway-added timer, which the Upstream layer marks with
// its I/O) set; the handler sets Trace, Deadline and Hooks. It is x's
// memory, valid until Put.
func (x *Request) Outbound() *snapshot.Outbound { return &x.out }

// Timer returns the request's gateway-added time measurement, reset at
// Begin to the request start (emit.GatewayTimer).
func (x *Request) Timer() *emit.GatewayTimer { return &x.timer }

// SetFinal records the request outcome Exchange.Final returns in onLog and
// Finish (status, code, committed, whether an attempt reached an
// Upstream; RejectedAfter is computed per calling Policy, LegStatus is the
// status SetResponse recorded) and the request
// duration, CEL duration.
func (x *Request) SetFinal(f filter.Final, d time.Duration) {
	x.final = filter.Final{Status: f.Status, Code: f.Code, Committed: f.Committed, UpstreamReached: f.UpstreamReached}
	x.vars.Duration = d
}

// received returns the client request as it arrived (filter.Received):
// the Host it named, its raw query before any Policy rewrote it, and its
// declared body length.
func (x *Request) received() filter.Received {
	r := x.hreq
	if r == nil {
		return filter.Received{}
	}
	out := filter.Received{Authority: r.Host, ContentLength: r.ContentLength}
	if r.URL != nil {
		out.RawQuery = r.URL.RawQuery
	}
	return out
}

// Exchange implements snapshot.RequestState: the client Filter view.
func (x *Request) Exchange() filter.Exchange { return (*view)(x) }

// Enter implements snapshot.RequestState: the next Filter call is p's, in
// ph, recording on span (nil when unsampled).
func (x *Request) Enter(p *snapshot.Policy, ph phase.Phase, span emit.Span) {
	x.cur, x.ph, x.span = p, ph, span
}

// SetLeg implements snapshot.RequestState; the client state has no leg.
func (*Request) SetLeg(*filter.Leg) {}

// When implements snapshot.RequestState: p's recorded spec.when decision.
func (x *Request) When(p *snapshot.Policy) (decided, skip bool) { return x.whens.get(p.Index) }

// SetWhen implements snapshot.RequestState: p's decision for the request.
func (x *Request) SetWhen(p *snapshot.Policy, skip bool) { x.whens.set(p.Index, skip) }

// Budget implements snapshot.RequestState: the request's State Store
// budget, the Route's largest stateStoreTimeout on the request stripe.
func (x *Request) Budget() *statestore.RequestBudget { return &x.budget }

// Leg implements snapshot.RequestState: a fresh handle to a pooled leg
// state for one leg to upstream (step "" for plain upstreams), ended by
// its Release, or by Pool.Put for a leg still live then. A released handle
// is inert for good, its memory reused only through new handles. It is
// safe for concurrent use.
func (x *Request) Leg(upstream, step string) snapshot.RequestState {
	return x.pool.newLeg(x, upstream, step)
}

// endLegs ends the request's legs before x is reused: the generation moves
// on, so a leg released from now on merges nothing into x, and every handle
// still live is released here, returning its reservations; its owner's
// later calls, Release included, do nothing. A request that issued no leg
// has nothing to end, and no merge can reach it.
func (x *Request) endLegs() {
	if !x.legsIssued.Load() {
		return
	}
	x.gen.Add(1)
	x.legMu.Lock()
	for _, h := range x.legs {
		h.end(false)
	}
	clear(x.legs)
	x.legs = x.legs[:0]
	x.legsIssued.Store(false)
	x.legMu.Unlock()
	// A merge that read the old generation holds the record's lock until
	// it is done; one that comes later reads the new generation and merges
	// nothing. Taking the lock once waits for the former, so reset clears a
	// record no merge writes to.
	x.rec.mu.Lock()
	x.rec.mu.Unlock() //nolint:staticcheck // SA2001: the empty critical section waits for a merge in flight.
}

// Release implements snapshot.RequestState; the handler releases the
// client state with Pool.Put.
func (*Request) Release() {}

// requestPhase reports whether the current Phase reads the request body:
// onRequestBody or onRoute.
func (x *Request) requestPhase() bool {
	return x.ph == phase.OnRequestBody || x.ph == phase.OnRoute
}

// gatedRequest returns the gated request body (nil when empty); ok is
// false when PrepareBody did not gate it.
func (x *Request) gatedRequest() (b []byte, ok bool) {
	if !x.gated {
		return nil, false
	}
	b, _ = x.reqBody.Gated()
	return b, true
}

// ensureRequestView builds the request body's decoded view once per body
// version, while the body is gated and before the response is set (CEL
// reads request.body only in request Phases and in the composition fields
// the Forwarder evaluates), and puts it in request.body. Concurrent legs
// may call it; the first builds the view under the lock.
func (x *Request) ensureRequestView() {
	sd := &x.bodyDec
	if sd.ready.Load() || !x.gated || x.resp.set {
		return
	}
	sd.mu.Lock()
	defer sd.mu.Unlock()
	if sd.ready.Load() {
		return
	}
	b, _ := x.reqBody.Gated()
	sd.v = x.pool.decode(&x.acct, x.hdr, b, x.limits.MaxRequestBodyBytes)
	x.req.Body = sd.v.cel()
	sd.ready.Store(true)
}

// requestView returns the request body's decoded view; ok is false when
// none can be built.
func (x *Request) requestView() (d *decoded, ok bool) {
	x.ensureRequestView()
	if !x.bodyDec.ready.Load() {
		return nil, false
	}
	return &x.bodyDec.v, true
}

// invalidateRequestView drops the request body's decoded view and its
// charge after the body changed; only the request goroutine calls it, while
// no leg runs.
func (x *Request) invalidateRequestView() {
	x.bodyDec.v.drop(&x.acct)
	x.bodyDec.ready.Store(false)
	x.req.Body = nil
}

// activation returns the client CEL activation of the current Phase:
// request.body once the body is gated, response.body in onResponse when
// the response is gated or the Route gates it.
func (x *Request) activation() *expr.Vars {
	x.ensureRequestView()
	if x.ph == phase.OnResponse {
		x.ensureResponseView(context.Background())
	}
	return &x.vars
}

// setIdentity records a successful authentication (Security and identity
// rule 2): CEL consumer and auth, and source.clientCertSubject for
// auth.mtls; a second one is filter.ErrSecondBinding.
func (x *Request) setIdentity(id *filter.Identity) error {
	if id == nil {
		return nil
	}
	if x.ident != nil {
		return filter.ErrSecondBinding
	}
	x.ident = id
	x.auth = expr.Auth{Method: id.Method, Claims: id.Claims}
	x.vars.Auth = &x.auth
	x.vars.Consumer = id.Consumer
	if id.Method == filter.MethodMTLS {
		x.srcView.ClientCertSubject = id.CertSubject
	}
	return nil
}

// requestID returns the 32-hex trace ID, formatted once on first use.
func (x *Request) requestID() string {
	if s := x.id.Load(); s != nil {
		return *s
	}
	s := string(x.idHex[:])
	x.id.CompareAndSwap(nil, &s)
	return *x.id.Load()
}

// routeMetrics returns the Route's handles, or no-op handles when x has
// none (never nil).
func (x *Request) routeMetrics() *emit.RouteMetrics {
	if x.route != nil && x.route.Metrics != nil {
		return x.route.Metrics
	}
	return &x.pool.noop
}

// reset drops everything x holds for the next request, keeping memory;
// endLegs has ended its legs.
func (x *Request) reset() {
	x.reqBody.Reset()
	x.respBody.Reset()
	x.acct.ReleaseAll()
	for _, t := range x.tees {
		t.Release()
	}
	clear(x.tees)
	x.tees = x.tees[:0]
	clear(x.rateLimits)
	x.rateLimits = x.rateLimits[:0]
	clear(x.rateLimitBy)
	x.rateLimitBy = x.rateLimitBy[:0]
	clear(x.params[:cap(x.params)])
	x.params = x.params[:0]
	for _, h := range x.genHdr {
		clear(h)
	}
	x.genIdx = 0
	x.whens.reset()
	x.slots.reset()
	x.rec.reset()
	x.charges.reset()
	x.bodyDec.v = decoded{}
	x.bodyDec.ready.Store(false)
	x.id.Store(nil)

	x.traceID, x.idHex = [16]byte{}, [32]byte{}
	x.start, x.listener, x.path = time.Time{}, "", ""
	x.hdr, x.src, x.tls, x.stripe, x.limits, x.hreq = nil, filter.Source{}, nil, 0, snapshot.Limits{}, nil
	x.route, x.budget = nil, statestore.RequestBudget{}
	x.vars, x.req, x.srcView, x.auth = expr.Vars{}, expr.Request{}, expr.Source{}, expr.Auth{}
	x.respV, x.up = expr.Response{}, expr.Upstream{}
	x.match, x.out = snapshot.MatchRequest{}, snapshot.Outbound{}
	x.cur, x.ph, x.span, x.ident = nil, 0, nil, nil
	x.gated = false
	x.resp, x.respDec, x.final, x.cache = response{}, decoded{}, filter.Final{}, ""
	// A kept leg body's array stays with the pooled Request only up to
	// body.PoolMax, as the budget's pools keep no larger buffer.
	if cap(x.legBody) > body.PoolMax {
		x.legBody = nil
	}
	x.legStatus, x.legHdr, x.legBody = 0, nil, x.legBody[:0]
	x.legKept, x.legLost, x.legLostBudget = false, false, false
}
