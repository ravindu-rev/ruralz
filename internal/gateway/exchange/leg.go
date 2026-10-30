// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
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
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// noAttempt is the attempt number of a leg before its first SetLeg.
const noAttempt = -1

// handleChunk is how many leg handles one allocation holds. Handles are
// never reused, so each leg takes a fresh one; carving them out of
// pooled chunks keeps the request path at one allocation per handleChunk
// legs, which rounds to none per request (PB-8).
const handleChunk = 64

// errClosed is returned by a leg response body read after Close.
var errClosed = errors.New("exchange: response body closed")

// leg is one upstream leg's pooled memory: the client request read-only,
// with the leg's own attempt, messages, Vars copy, PolicyState slots,
// spec.when decisions, buffer account, State Store budget copy and
// records. Only its live legHandle reaches it, from the leg's goroutine.
type leg struct {
	pool   *Pool
	parent *Request
	// pgen is the parent's generation when the leg was issued: a release
	// after the parent was put back merges nothing into it.
	pgen uint64
	step string

	// vars copies the client activation with the upstream fields replaced;
	// req is the outgoing request's view (headers and query of the
	// attempt's request, the leg's current body).
	vars  expr.Vars
	req   expr.Request
	up    expr.Upstream
	errV  expr.AttemptError
	respV expr.Response

	cur   *snapshot.Policy
	ph    phase.Phase
	span  emit.Span
	whens decisions
	slots slots

	attempt  *filter.Leg
	attemptN int
	budget   statestore.RequestBudget
	acct     body.Account

	// This attempt's rewritten request body and its decoded view.
	rewritten bool
	reqBytes  []byte
	reqDec    decoded
	// This attempt's gated response body (or the gate's error) and its
	// decoded view.
	rb      *legBody
	rbErr   error
	respDec decoded

	// scratch serves a message whose attempt has no request or response.
	scratch http.Header

	rec record
}

// legHandle is the snapshot.RequestState of one leg, and the identity of
// its Filter view and messages. A handle is issued once per Leg call and
// never reused, while its leg's memory is pooled: once the handle is
// released (Release, or Pool.Put of its request), it and every view and
// message taken from it do nothing, so a stale or second Release, or any
// call through a stale handle, never reaches the leg's next owner.
type legHandle struct {
	// l is set when the handle is issued and never changes.
	l *leg
	// done is set once, by the one release that wins it.
	done atomic.Bool
}

// handleChunks is a block of handles taken one by one, each once.
type handleChunks struct {
	hs   [handleChunk]legHandle
	next int
}

var _ snapshot.RequestState = (*legHandle)(nil)

// newLeg returns a fresh handle to a pooled leg state for one leg of x to
// upstream; on a request already put back, a released handle.
func (p *Pool) newLeg(x *Request, upstream, step string) *legHandle {
	if !x.live.Load() {
		return released(&leg{pool: p})
	}
	l, _ := p.legs.Get().(*leg)
	if l == nil {
		l = &leg{pool: p, attemptN: noAttempt}
	}
	return p.issue(x, l, upstream, step)
}

// issue binds l to x for one leg to upstream and returns its new handle,
// which x lists so Pool.Put can end the legs still live.
func (p *Pool) issue(x *Request, l *leg, upstream, step string) *legHandle {
	l.parent, l.pgen, l.step = x, x.gen.Load(), step
	l.vars = x.vars
	l.up = expr.Upstream{Name: upstream}
	l.vars.Upstream = &l.up
	l.vars.Response, l.vars.Error, l.vars.Attempt = nil, nil, 0
	l.copyRequestView()
	l.vars.Request = &l.req
	// A copy: a leg's State Store calls never race the client's or another
	// leg's.
	l.budget = x.budget
	l.acct.Bind(p.budget, body.Gate, x.stripe)
	h := p.handle(l)
	x.legMu.Lock()
	x.legs = append(x.legs, h)
	x.legsIssued.Store(true)
	x.legMu.Unlock()
	return h
}

// handle returns a never-used handle for l from the pooled chunks.
func (p *Pool) handle(l *leg) *legHandle {
	c, _ := p.chunks.Get().(*handleChunks)
	if c == nil {
		c = new(handleChunks)
	}
	h := &c.hs[c.next]
	h.l = l
	c.next++
	if c.next < len(c.hs) {
		p.chunks.Put(c)
	}
	return h
}

// released returns a handle to l that is already released.
func released(l *leg) *legHandle {
	h := &legHandle{l: l}
	h.done.Store(true)
	return h
}

// leg returns h's leg state, nil once h is released.
func (h *legHandle) leg() *leg {
	if h.done.Load() {
		return nil
	}
	return h.l
}

// end releases h's leg once: the release that sets done resets the leg
// and returns it to its pool; merge says whether its records join the
// request's (Release) or not (Pool.Put, which drops the request's).
func (h *legHandle) end(merge bool) {
	if !h.done.CompareAndSwap(false, true) {
		return
	}
	l := h.l
	if merge {
		x := l.parent
		x.rec.mu.Lock()
		// A leg released after its request was put back would merge into a
		// request that is gone, or that already serves another one: it
		// merges nothing.
		if x.gen.Load() == l.pgen {
			x.rec.merge(&l.rec, l.step == "")
		}
		x.rec.mu.Unlock()
	}
	l.reset()
	l.pool.legs.Put(l)
}

// Exchange implements snapshot.RequestState: the leg's Filter view.
func (h *legHandle) Exchange() filter.Exchange { return (*legView)(h) }

// Enter implements snapshot.RequestState. In response Phases CEL response
// is the attempt's response.
func (h *legHandle) Enter(p *snapshot.Policy, ph phase.Phase, span emit.Span) {
	if l := h.leg(); l != nil {
		l.enter(p, ph, span)
	}
}

// SetLeg implements snapshot.RequestState: the attempt a's Phases follow.
func (h *legHandle) SetLeg(a *filter.Leg) {
	if l := h.leg(); l != nil {
		l.setLeg(a)
	}
}

// When implements snapshot.RequestState: p's decision on this leg.
func (h *legHandle) When(p *snapshot.Policy) (decided, skip bool) {
	if l := h.leg(); l != nil {
		return l.whens.get(p.Index)
	}
	return false, false
}

// SetWhen implements snapshot.RequestState.
func (h *legHandle) SetWhen(p *snapshot.Policy, skip bool) {
	if l := h.leg(); l != nil {
		l.whens.set(p.Index, skip)
	}
}

// RecordShortCircuit implements snapshot.RequestState; Release merges it
// into the request.
func (h *legHandle) RecordShortCircuit(p *snapshot.Policy, ph phase.Phase, status int) {
	if l := h.leg(); l != nil {
		l.rec.shortCircuit(p, ph, status)
	}
}

// RecordFailure implements snapshot.RequestState; Release merges it into
// the request.
func (h *legHandle) RecordFailure(p *snapshot.Policy, ph phase.Phase, mode v1alpha1.FailureMode) {
	if l := h.leg(); l != nil {
		l.rec.failure(p, ph, mode)
	}
}

// Budget implements snapshot.RequestState: the leg's copy of the request's
// State Store budget; a released handle's is its own empty one.
func (h *legHandle) Budget() *statestore.RequestBudget {
	if l := h.leg(); l != nil {
		return &l.budget
	}
	return new(statestore.RequestBudget)
}

// Leg implements snapshot.RequestState: a sibling leg of the same request;
// on a released handle, that handle.
func (h *legHandle) Leg(upstream, step string) snapshot.RequestState {
	l := h.leg()
	if l == nil {
		return h
	}
	return l.pool.newLeg(l.parent, upstream, step)
}

// Release implements snapshot.RequestState: the leg's records join the
// request's, its reservations are returned and its memory goes back to its
// pool. Only the first release of a handle acts: a second one, a release
// racing it, and one after Pool.Put ended the leg do nothing. A gated
// response body keeps its own buffer until it is closed.
func (h *legHandle) Release() { h.end(true) }

// copyRequestView sets the leg's request view from the client's, with the
// attempt's outgoing headers, query and path when it has a request: in
// onUpstreamRequest CEL request describes the attempt's outgoing request,
// such as a composition step's own path (docs/architecture/03-data-plane.md
// "Transform Policies").
func (l *leg) copyRequestView() {
	pr := &l.parent.req
	l.req = expr.Request{
		Method: pr.Method, Scheme: pr.Scheme, Host: pr.Host, Path: pr.Path,
		PathParams: pr.PathParams, Header: pr.Header,
	}
	l.req.SetRawQuery(pr.RawQuery())
	if a := l.attempt; a != nil && a.Request != nil {
		if a.Request.Header != nil {
			l.req.Header = a.Request.Header
		}
		if u := a.Request.URL; u != nil {
			l.req.SetRawQuery(u.RawQuery)
			if u.Path != "" || u.RawPath != "" {
				l.req.Path = routematch.RequestPath(u)
			}
		}
	}
}

// enter selects the next Filter call (snapshot.RequestState.Enter).
func (l *leg) enter(p *snapshot.Policy, ph phase.Phase, span emit.Span) {
	l.cur, l.ph, l.span = p, ph, span
	switch ph {
	case phase.OnUpstreamRequest:
		l.vars.Response = nil
	case phase.OnUpstreamResponseHeaders, phase.OnUpstreamResponseBody:
		l.vars.Response = nil
		if a := l.attempt; a != nil && a.Response != nil {
			l.respV.Status, l.respV.Header = a.Response.StatusCode, a.Response.Header
			l.vars.Response = &l.respV
		}
	default:
	}
}

// setLeg makes a the current attempt (snapshot.RequestState.SetLeg). A new
// attempt number starts from the client's input again: no rewritten
// request body, no gated response, the outgoing request's view.
func (l *leg) setLeg(a *filter.Leg) {
	l.attempt = a
	if a == nil {
		return
	}
	if a.Attempt != l.attemptN {
		l.attemptN = a.Attempt
		l.newAttempt()
	}
	l.up.Endpoint = a.Endpoint
	l.vars.Attempt = a.Attempt
	l.vars.Error = nil
	if a.ErrorKind != "" {
		l.errV.Kind = a.ErrorKind
		l.vars.Error = &l.errV
	}
}

// newAttempt drops the previous attempt's body state and the header writes
// a Filter made on an attempt without a message (scratchHeader);
// reservations of a rewritten request body stay until Release, since a
// transport may still read it.
func (l *leg) newAttempt() {
	clear(l.scratch)
	l.rewritten, l.reqBytes = false, nil
	l.reqDec.drop(&l.acct)
	l.rb, l.rbErr = nil, nil
	l.respDec.drop(&l.acct)
	l.respV = expr.Response{}
	l.copyRequestView()
}

// reset drops everything l holds, keeping memory.
func (l *leg) reset() {
	l.acct.ReleaseAll()
	l.whens.reset()
	l.slots.reset()
	l.rec.reset()
	clear(l.scratch)
	l.parent, l.pgen, l.step = nil, 0, ""
	l.vars, l.req, l.up, l.errV, l.respV = expr.Vars{}, expr.Request{}, expr.Upstream{}, expr.AttemptError{}, expr.Response{}
	l.cur, l.ph, l.span = nil, 0, nil
	l.attempt, l.attemptN, l.budget = nil, noAttempt, statestore.RequestBudget{}
	l.rewritten, l.reqBytes, l.reqDec = false, nil, decoded{}
	l.rb, l.rbErr, l.respDec = nil, nil, decoded{}
}

// activation returns the leg's CEL activation of the current Phase:
// request.body is the leg's current body in onUpstreamRequest, and
// response.body the gated response in onUpstreamResponseBody.
//
// In onUpstreamResponseBody the response body is gated and decoded for
// CEL only when it is gated already, because a Filter read it, as a
// Policy that needs the body does before it evaluates an expression, or
// when the current Policy's spec.when reads response.body before any
// Filter did. A Policy without a body need, such as a transform.response
// that edits only headers, so runs while the body streams on
// (docs/architecture/02-configuration-model.md "Body buffering and
// limits").
func (l *leg) activation() *expr.Vars {
	switch l.ph {
	case phase.OnUpstreamRequest:
		if d, ok := l.requestDecoded(); ok {
			l.req.Body = d.cel()
		}
	case phase.OnUpstreamResponseBody:
		if l.rb != nil || l.rbErr != nil || l.whenReadsResponseBody() {
			// The gate is read here with no request context when spec.when
			// needs the body before any Filter read it; the Upstream
			// response body is bound to the leg's own context.
			l.respV.Body = l.responseDecoded(context.Background()).cel()
		}
	default:
	}
	return &l.vars
}

// whenReadsResponseBody reports whether the current Policy's spec.when
// selects response.body.
func (l *leg) whenReadsResponseBody() bool {
	return l.cur != nil && l.cur.When != nil && l.cur.When.Refs().ResponseBody
}

// requestBody returns this attempt's request body: the rewritten one, or
// the client's gated input; ok is false when neither exists.
func (l *leg) requestBody() (b []byte, ok bool) {
	if l.rewritten {
		return l.reqBytes, true
	}
	return l.parent.gatedRequest()
}

// requestDecoded returns the decoded view of this attempt's request body:
// the leg's own after a rewrite, else the client's shared one.
func (l *leg) requestDecoded() (d *decoded, ok bool) {
	if !l.rewritten {
		return l.parent.requestView()
	}
	if !l.reqDec.done {
		h := l.outHeader()
		l.reqDec = l.pool.decode(&l.acct, h, l.reqBytes, l.parent.limits.MaxRequestBodyBytes)
	}
	return &l.reqDec, true
}

// setRequestBody installs b as this attempt's outgoing request body
// (filter.Message.SetBody in onUpstreamRequest): reserved from the leg's
// account until Release, readable again through GetBody, with the body
// fields updated.
func (l *leg) setRequestBody(b []byte) error {
	a := l.attempt
	if a == nil || a.Request == nil {
		return filter.ErrNotAvailable
	}
	if err := l.acct.Reserve(int64(len(b))); err != nil {
		return err
	}
	b = slices.Clip(b)
	installBody(a.Request, b)
	setBodyFields(a.Request.Header, len(b))
	l.rewritten, l.reqBytes = true, b
	l.reqDec.drop(&l.acct)
	l.req.Body = nil
	return nil
}

// installBody makes b r's body, closing the body it replaces. GetBody
// reopens b for the transport's own retries; it holds b itself, never the
// pooled leg state.
func installBody(r *http.Request, b []byte) {
	if r.Body != nil && r.Body != http.NoBody {
		_ = r.Body.Close()
	}
	r.ContentLength = int64(len(b))
	if len(b) == 0 {
		r.Body = http.NoBody
		r.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
}

// gateResponse reads this attempt's response body whole within its cap
// (responseLimit) and puts the gated body in its place, once per attempt.
// The gated body owns its buffer and reservation until it is closed (the
// response may stream to the client after the leg ended); its Close closes
// the original body.
func (l *leg) gateResponse(ctx context.Context) (*legBody, error) {
	switch {
	case l.rb != nil:
		return l.rb, nil
	case l.rbErr != nil:
		return nil, l.rbErr
	}
	a := l.attempt
	if a == nil || a.Response == nil {
		return nil, filter.ErrNotAvailable
	}
	resp := a.Response
	lb := &legBody{orig: resp.Body}
	if err := lb.q.Prepare(ctx, resp.Body, resp.ContentLength, true, l.responseLimit(), l.pool.budget, l.parent.stripe); err != nil {
		l.rbErr = err
		return nil, err
	}
	resp.Body = lb
	l.rb = lb
	return lb, nil
}

// responseDecoded returns the decoded view of this attempt's gated
// response body, gating it first.
func (l *leg) responseDecoded(ctx context.Context) *decoded {
	if l.respDec.done {
		return &l.respDec
	}
	lb, err := l.gateResponse(ctx)
	if err != nil {
		l.respDec = decoded{done: true, err: err}
		return &l.respDec
	}
	l.respDec = l.pool.decode(&l.acct, l.attempt.Response.Header, lb.bytes(), l.responseLimit())
	return &l.respDec
}

// responseLimit returns the cap of this attempt's response body: the
// leg's MaxBodyBytes (a composition step's maxBodyBytes), else
// limits.maxResponseBodyBytes. A body over it is filter.ErrTooLarge.
func (l *leg) responseLimit() int64 {
	if a := l.attempt; a != nil && a.MaxBodyBytes > 0 {
		return a.MaxBodyBytes
	}
	return l.parent.limits.MaxResponseBodyBytes
}

// setResponseBody replaces this attempt's response body with b
// (filter.Message.SetBody in onUpstreamResponseBody).
func (l *leg) setResponseBody(ctx context.Context, b []byte) error {
	lb, err := l.gateResponse(ctx)
	if err != nil {
		return err
	}
	if err := lb.replace(b); err != nil {
		return err
	}
	resp := l.attempt.Response
	resp.ContentLength = int64(len(b))
	setBodyFields(resp.Header, len(b))
	l.respDec.drop(&l.acct)
	l.respV.Body = nil
	return nil
}

// outHeader returns the attempt's outgoing request header map.
func (l *leg) outHeader() http.Header {
	if a := l.attempt; a != nil && a.Request != nil && a.Request.Header != nil {
		return a.Request.Header
	}
	return l.scratchHeader()
}

// inHeader returns the attempt's response header map.
func (l *leg) inHeader() http.Header {
	if a := l.attempt; a != nil && a.Response != nil && a.Response.Header != nil {
		return a.Response.Header
	}
	return l.scratchHeader()
}

// scratchHeader returns an empty header map for a message without one, so
// a Filter never writes into a nil map.
func (l *leg) scratchHeader() http.Header {
	if l.scratch == nil {
		l.scratch = make(http.Header)
	}
	return l.scratch
}

// legBody is a leg response body the exchange gated or a Filter replaced:
// the bytes, read from the first byte. It owns them and their reservation
// until Close, independent of the leg state, because the response may
// stream to the client after the leg ended. Close also closes the original
// body. It is safe for concurrent use.
type legBody struct {
	mu     sync.Mutex
	q      body.Request
	orig   io.ReadCloser
	rd     io.ReadCloser
	closed bool
}

var _ io.ReadCloser = (*legBody)(nil)

// Read reads the body; errClosed after Close.
func (b *legBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, errClosed
	}
	if b.rd == nil {
		rd, err := b.q.Open()
		if err != nil {
			return 0, err
		}
		b.rd = rd
	}
	return b.rd.Read(p)
}

// Close returns the buffer and reservation and closes the original body;
// a second Close does nothing.
func (b *legBody) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed, b.rd = true, nil
	b.q.Reset()
	orig := b.orig
	b.orig = nil
	b.mu.Unlock()
	if orig == nil {
		return nil
	}
	return orig.Close()
}

// bytes returns the body; nil for an empty one.
func (b *legBody) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, _ := b.q.Gated()
	return v
}

// replace makes p the body (body.Request.Replace: reserved, copied when it
// shares the gate's memory); a reader not yet started reads p.
func (b *legBody) replace(p []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errClosed
	}
	if err := b.q.Replace(p); err != nil {
		return err
	}
	b.rd = nil
	return nil
}

// legView is a leg's handle as the filter.Exchange of its upstream-leg
// Phases: the client request read-only (filter.Exchange documents what a
// leg view does not do). Once the handle is released, every method does
// nothing and reports zero values: a view kept past its leg never reaches
// the leg's next owner.
type legView legHandle

var _ filter.Exchange = (*legView)(nil)

// state returns the leg state, nil once the handle is released.
func (v *legView) state() *leg { return (*legHandle)(v).leg() }

// parent returns the leg's request, nil once the handle is released.
func (v *legView) parent() *Request {
	if l := v.state(); l != nil {
		return l.parent
	}
	return nil
}

// RequestID implements filter.Exchange.
func (v *legView) RequestID() string {
	if x := v.parent(); x != nil {
		return x.requestID()
	}
	return ""
}

// Now implements filter.Exchange.
func (v *legView) Now() time.Time {
	if x := v.parent(); x != nil {
		return x.start
	}
	return time.Time{}
}

// Listener implements filter.Exchange.
func (v *legView) Listener() string {
	if x := v.parent(); x != nil {
		return x.listener
	}
	return ""
}

// Route implements filter.Exchange.
func (v *legView) Route() *expr.Route {
	if x := v.parent(); x != nil {
		return x.vars.Route
	}
	return nil
}

// Method implements filter.Exchange.
func (v *legView) Method() string {
	if x := v.parent(); x != nil {
		return x.req.Method
	}
	return ""
}

// Scheme implements filter.Exchange.
func (v *legView) Scheme() string {
	if x := v.parent(); x != nil {
		return x.req.Scheme
	}
	return ""
}

// Host implements filter.Exchange.
func (v *legView) Host() string {
	if x := v.parent(); x != nil {
		return x.req.Host
	}
	return ""
}

// Path implements filter.Exchange.
func (v *legView) Path() string {
	if x := v.parent(); x != nil {
		return x.path
	}
	return ""
}

// Received implements filter.Exchange: the client request as it arrived.
func (v *legView) Received() filter.Received {
	if x := v.parent(); x != nil {
		return x.received()
	}
	return filter.Received{}
}

// Header implements filter.Exchange: the client request headers, read-only
// on a leg (the outgoing request's are Message().Header()).
func (v *legView) Header() http.Header {
	if x := v.parent(); x != nil {
		return x.hdr
	}
	return nil
}

// Source implements filter.Exchange.
func (v *legView) Source() filter.Source {
	if x := v.parent(); x != nil {
		return x.src
	}
	return filter.Source{}
}

// TLS implements filter.Exchange.
func (v *legView) TLS() *filter.ConnTLS {
	if x := v.parent(); x != nil {
		return x.tls
	}
	return nil
}

// Message implements filter.Exchange: the outgoing request in
// onUpstreamRequest, the attempt's response in the response Phases, and in
// Finish the response when one arrived, else the request.
func (v *legView) Message() filter.Message {
	h := (*legHandle)(v)
	l := h.leg()
	if l == nil {
		return (*legRequestMessage)(h)
	}
	switch l.ph {
	case phase.OnUpstreamRequest:
		return (*legRequestMessage)(h)
	case phase.OnUpstreamResponseHeaders, phase.OnUpstreamResponseBody:
		return (*legResponseMessage)(h)
	default:
		if a := l.attempt; a != nil && a.Response != nil {
			return (*legResponseMessage)(h)
		}
		return (*legRequestMessage)(h)
	}
}

// Leg implements filter.Exchange: the current attempt.
func (v *legView) Leg() *filter.Leg {
	if l := v.state(); l != nil {
		return l.attempt
	}
	return nil
}

// Vars implements filter.Exchange.
func (v *legView) Vars() *expr.Vars {
	if l := v.state(); l != nil {
		return l.activation()
	}
	return new(expr.Vars)
}

// Identity implements filter.Exchange: the request's identity.
func (v *legView) Identity() *filter.Identity {
	if x := v.parent(); x != nil {
		return x.ident
	}
	return nil
}

// SetIdentity implements filter.Exchange: filter.ErrNotAvailable on a leg.
func (*legView) SetIdentity(*filter.Identity) error { return filter.ErrNotAvailable }

// StateBudget implements filter.Exchange.
func (v *legView) StateBudget() *statestore.RequestBudget { return (*legHandle)(v).Budget() }

// Reserve implements filter.Exchange: charged to the leg's account;
// filter.ErrNotAvailable once the leg ended.
func (v *legView) Reserve(n int64) error {
	if l := v.state(); l != nil {
		return l.acct.Reserve(n)
	}
	return filter.ErrNotAvailable
}

// Release implements filter.Exchange.
func (v *legView) Release(n int64) {
	if l := v.state(); l != nil {
		l.acct.Release(n)
	}
}

// LegResponse implements filter.Exchange: nothing on a leg, which ends
// before the request's leg response is known.
func (*legView) LegResponse() (int, http.Header) { return 0, nil }

// TeeResponse implements filter.Exchange: nil on a leg.
func (*legView) TeeResponse(int64) filter.Tee { return nil }

// ReplaceResponse implements filter.Exchange: nothing on a leg.
func (*legView) ReplaceResponse(*filter.Response) {}

// AddRateLimitField implements filter.Exchange: nothing on a leg.
func (*legView) AddRateLimitField(filter.RateLimitField) {}

// Charge implements filter.Exchange: a leg keeps no record, so the caller
// charges as the first.
func (*legView) Charge(filter.Counter) (*filter.Charge, bool) { return nil, true }

// SetCacheResult implements filter.Exchange: nothing on a leg.
func (*legView) SetCacheResult(string) {}

// Annotate implements filter.Exchange.
func (v *legView) Annotate(key string, val slog.Value) {
	if l := v.state(); l != nil && l.span != nil {
		l.span.SetAttr(key, val)
	}
}

// Final implements filter.Exchange: the zero value on a leg, which ends
// before the request does.
func (*legView) Final() filter.Final { return filter.Final{} }

// PolicyState implements filter.Exchange: the calling Policy's slot on
// this leg; once the leg ended, a slot of its own that nothing reads.
func (v *legView) PolicyState() *any {
	if l := v.state(); l != nil {
		return l.slots.at(l.cur)
	}
	return new(any)
}

// Stripe implements filter.Exchange.
func (v *legView) Stripe() emit.Stripe {
	if x := v.parent(); x != nil {
		return x.stripe
	}
	return 0
}

// RouteMetrics implements filter.Exchange; never nil.
func (v *legView) RouteMetrics() *emit.RouteMetrics {
	if x := v.parent(); x != nil {
		return x.routeMetrics()
	}
	return &v.l.pool.noop
}
