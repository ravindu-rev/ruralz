// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// codeRequestTooLarge is 413: the streamed client body passed
// limits.maxRequestBodyBytes while an attempt sent it.
const codeRequestTooLarge = "RZ-RT-003"

// Leg errors without an Upstream cause.
var (
	// errRouteTimeout is the cause of RZ-RT-007: the Route deadline passed
	// before the leg's first attempt.
	errRouteTimeout = errors.New("upstream: Route timeout expired before any attempt")
	// errNoExchange reports a leg called without an Outbound exchange.
	errNoExchange = errors.New("upstream: leg without an Outbound exchange")
)

// ErrClientBody is the uncoded error of a leg whose attempt ended because
// reading the client body failed while it was sent: the client's fault,
// no Upstream failure, so the leg carries no RZ code and the request
// aborts.
var ErrClientBody = errors.New("upstream: reading the client body failed while an attempt sent it")

// Target is what one leg sends besides the client request: the
// composition step it serves and, for a step, its method and path.
type Target struct {
	// Step is the composition step name; "" for plain upstreams.
	Step string
	// Method replaces the client request method when set (a step's
	// method).
	Method string
	// Path replaces the client's normalized path and query when set (a
	// step's path or pathExpression result), sent as is: it holds no query
	// (a "?" in it is escaped); RawQuery is then the query sent.
	Path string
	// RawQuery is the query sent with Path.
	RawQuery string
	// MaxBodyBytes is a step's maxBodyBytes, the cap of the leg's response
	// body when a body Phase reads it whole (filter.Leg.MaxBodyBytes); 0
	// means limits.maxResponseBodyBytes.
	MaxBodyBytes int64
}

// LegError is the error of a leg that ended without a response. It wraps
// the *errcode.Error carrying the leg's code (errcode.CodeOf finds it), or
// ErrClientBody without a code when the client body broke, and
// names the Upstream, the last Endpoint tried, the attempts and the last
// error kind for the access log and the span.
type LegError struct {
	// Upstream is the Upstream's name.
	Upstream string
	// Endpoint is the identity of the last Endpoint an attempt reached; ""
	// when none did.
	Endpoint string
	// Attempts is the attempts that ran.
	Attempts int
	// Kind is the last attempt's error kind (KindNone for a gate).
	Kind resilience.Kind
	// Err is the coded error.
	Err error
}

// Error returns "upstream <name>: <coded error>".
func (e *LegError) Error() string { return "upstream " + e.Upstream + ": " + e.Err.Error() }

// Unwrap returns the coded error.
func (e *LegError) Unwrap() error { return e.Err }

// step is what the attempt loop does after one attempt.
type step uint8

const (
	// stepStop ends the leg with the last attempt's outcome.
	stepStop step = iota
	// stepRetry sleeps the backoff and starts the next attempt.
	stepRetry
	// stepDone returns the result the attempt produced.
	stepDone
)

// Leg runs one upstream leg to u: the attempt loop of
// docs/architecture/09-traffic-management-and-resilience.md "Timeouts,
// deadlines, retries and hedging" and docs/architecture/03-data-plane.md
// "Upstream layer". It runs on the caller's goroutine and starts none.
//
// A Route deadline already past is RZ-RT-007; an empty Endpoint set
// RZ-UP-008 and an open breaker RZ-UP-005, before any attempt. Each
// attempt takes a bulkhead slot (waiting in the pending queue within its
// attempt context; a full queue is RZ-UP-006), picks an Endpoint
// (ring-hash by hashKey, avoiding the Endpoints this leg tried), runs
// onUpstreamRequest and sends the request on the Endpoint's pool. After
// the attempt, failureWhen feeds passive ejection, then
// onUpstreamResponseHeaders (with the response, or without one and the
// attempt's error kind) and retryOn (or a Filter's retry) decide on a
// retry under the replay rule, the attempts, the deadlines, the breaker,
// Retry-After and the retry budget. The final response runs
// onUpstreamResponseBody (within t.MaxBodyBytes) and is returned
// unchanged; its Body releases the bulkhead slot when closed or read to
// the end. A leg without a response returns a *LegError with the code of
// the selection rule (504 RZ-UP-003, 502 RZ-UP-007, RZ-UP-001, RZ-UP-002
// or RZ-UP-004). Each attempt is one ruralz.upstream.<name> span and one
// ruralz_upstream_attempts_total count; its round trip and the reads of
// the leg's response body are excluded from gateway-added time through
// o.Excluder.
func (u *Upstream) Leg(ctx context.Context, o *snapshot.Outbound, t Target) (*snapshot.UpstreamResponse, error) {
	if o == nil || o.X == nil {
		return nil, errNoExchange
	}
	m := u.rt.m
	if !o.Deadline.IsZero() && !m.clk.Now().Before(o.Deadline) {
		return nil, &LegError{Upstream: u.name, Kind: resilience.KindTimeout, Err: errcode.Wrap(resilience.CodeRouteTimeout, errRouteTimeout)}
	}
	ls := m.getLeg()
	defer m.putLeg(ls)
	ls.init(u, o, t)
	return ls.run(ctx)
}

// legState is one leg's working state, pooled by the Manager so a leg
// allocates only what outlives it or what net/http keeps.
type legState struct {
	u    *Upstream
	o    *snapshot.Outbound
	t    Target
	base *expr.Vars
	// method, path and query are what every attempt sends.
	method, path, query string
	hooks               snapshot.LegRun
	// key is the ring-hash key; random is set when hashKey failed at run
	// time, so every attempt picks a weighted random Endpoint.
	key    uint64
	random bool
	tried  []string
	leg    resilience.Leg
	ha     health.Attempt
	view   attemptView
	// vars is the leg activation of retryOn and failureWhen; req stands in
	// for a missing client request view.
	vars expr.Vars
	req  expr.Request
	resp expr.Response
	errv expr.AttemptError
	upv  expr.Upstream
	fleg filter.Leg
	// trace counts the connection an attempt gets (GotConn).
	trace   httptrace.ClientTrace
	conn    atomic.Pointer[trackedConn]
	getBody func() (io.ReadCloser, error)
	// timer is the attempt deadline timer every attempt of the legs this
	// state runs reuses; armed is the Deadline it fires (onDeadline).
	timer clock.Timer
	armed atomic.Pointer[resilience.Deadline]
	// The last attempt that ran.
	endpoint  string
	kind      resilience.Kind
	failed    bool
	responded bool
	cause     error
	canceled  bool
	// over is set when the client body passed its limit during an attempt.
	over bool
	// broken is set when reading the client body failed during an
	// attempt.
	broken bool
	delay  time.Duration
	// park holds a response the leg decided to retry, kept until the retry
	// attempt runs: when the retry cannot start, the leg returns it.
	park parked
}

// parked is a retried response waiting for the next attempt to run.
type parked struct {
	resp *http.Response
	end  attemptEnd
	fleg filter.Leg
}

// getLeg returns a pooled leg state.
func (m *Manager) getLeg() *legState {
	ls, _ := m.legs.Get().(*legState)
	if ls == nil {
		ls = &legState{}
		ls.trace.GotConn = ls.gotConn
		ls.getBody = ls.reopen
	}
	return ls
}

// putLeg returns ls to the pool. Its health.Attempt and resilience.Leg
// are idle by then; the Attempt's stall timer is reused.
func (m *Manager) putLeg(ls *legState) {
	ls.u, ls.o, ls.base, ls.hooks = nil, nil, nil, nil
	ls.t = Target{}
	ls.method, ls.path, ls.query = "", "", ""
	ls.key, ls.random = 0, false
	clear(ls.tried)
	ls.tried = ls.tried[:0]
	ls.view = attemptView{}
	ls.vars = expr.Vars{}
	ls.req.Reset()
	ls.resp, ls.errv, ls.upv = expr.Response{}, expr.AttemptError{}, expr.Upstream{}
	ls.fleg = filter.Leg{}
	ls.conn.Store(nil)
	ls.armed.Store(nil)
	ls.endpoint, ls.kind, ls.failed, ls.responded = "", resilience.KindNone, false, false
	ls.cause, ls.canceled, ls.over, ls.broken, ls.delay = nil, false, false, false, 0
	ls.park = parked{}
	m.legs.Put(ls)
}

// init starts a leg of u for o.
func (ls *legState) init(u *Upstream, o *snapshot.Outbound, t Target) {
	ls.u, ls.o, ls.t = u, o, t
	ls.base = o.X.Vars()
	ls.method = t.Method
	if ls.method == "" {
		ls.method = o.X.Method()
	}
	if t.Path != "" {
		ls.path, ls.query = t.Path, t.RawQuery
		return
	}
	// The client request message holds the forwarded path and query, as
	// the request Phases left them: a transform.request set[] entry with
	// target path at Gateway or Route scope rewrites the path every plain
	// upstreams leg sends, while Exchange.Path keeps the client's
	// (docs/architecture/03-data-plane.md "Transform Policies").
	ls.path = o.X.Path()
	if msg := o.X.Message(); msg != nil {
		if p := msg.Path(); p != "" {
			ls.path = p
		}
		ls.query = msg.RawQuery()
	} else if ls.base != nil && ls.base.Request != nil {
		ls.query = ls.base.Request.RawQuery()
	}
}

// gotConn counts the attempt on the connection the transport gave it
// (httptrace.ClientTrace.GotConn), and records in Outbound.Reached that
// the request reached an Upstream; a transport retry on another
// connection moves the count.
func (ls *legState) gotConn(info httptrace.GotConnInfo) {
	if r := ls.o.Reached; r != nil {
		// A connection, connected and past TLS: the Upstream may serve
		// the attempt (filter.Final.UpstreamReached).
		r.Store(true)
	}
	c := trackedOf(info.Conn)
	if c == nil || !c.begin() {
		return
	}
	if old := ls.conn.Swap(c); old != nil {
		old.end()
	}
}

// reopen is the outgoing request's GetBody: a gated body reopens from the
// first byte, so net/http may resend it on a fresh connection.
func (ls *legState) reopen() (io.ReadCloser, error) { return ls.o.Body.Open() }

// deadlineTimer returns the state's attempt deadline timer, stopped; it is
// made once, on the Manager's clock, so an attempt allocates no timer.
func (ls *legState) deadlineTimer() clock.Timer {
	if ls.timer == nil {
		ls.timer = ls.u.rt.m.clk.AfterFunc(time.Hour, ls.onDeadline)
		ls.timer.Stop()
	}
	return ls.timer
}

// onDeadline is the timer's function: it fires the Deadline armed last,
// which ignores a run before its own deadline (resilience.Deadline.Fire).
func (ls *legState) onDeadline() {
	if d := ls.armed.Load(); d != nil {
		d.Fire()
	}
}

// run is the attempt loop.
func (ls *legState) run(ctx context.Context) (*snapshot.UpstreamResponse, error) {
	u := ls.u
	if g := ls.leg.Begin(u.rt.res, &u.res, ls.o.Deadline, u.bal.bal.Len()); g != resilience.GateNone {
		return nil, &LegError{Upstream: u.name, Err: g.Err()}
	}
	ls.hashKey(ctx)
loop:
	for {
		a, g := ls.leg.StartAttempt()
		if g != resilience.GateNone {
			break
		}
		if ls.leg.Expired() {
			ls.leg.AbandonAttempt(resilience.GateNone)
			break
		}
		next, resp, err := ls.attempt(ctx, &a)
		switch next {
		case stepDone:
			return resp, err
		case stepRetry:
			if ls.sleep(ctx) != nil {
				break loop
			}
		case stepStop:
			if err != nil && ls.leg.Attempts() == 0 {
				// No attempt ran: the error is the leg's cause (RZ-UP-004).
				ls.cause = err
			}
			break loop
		}
	}
	return ls.end(ctx)
}

// hashKey evaluates the ring-hash key once per leg; retries reuse it. A
// runtime error picks weighted random Endpoints for the whole leg and is
// counted in ruralz_upstream_cel_errors_total{field="hashKey"}.
func (ls *legState) hashKey(ctx context.Context) {
	u := ls.u
	if u.alg != balance.RingHash || u.hashKey == nil {
		return
	}
	k, err := u.hashKey.EvalString(ctx, ls.base)
	if err != nil {
		ls.random = true
		u.metrics.CELErrors[emit.CELHashKey].Add(ls.o.Stripe, 1)
		return
	}
	ls.key = balance.HashKey(k)
}

// sleep waits the retry delay on the clock.
func (ls *legState) sleep(ctx context.Context) error {
	if ls.delay <= 0 {
		return ctx.Err()
	}
	return ls.u.rt.m.clk.Sleep(ctx, ls.delay)
}

// attempt runs one attempt StartAttempt counted: the bulkhead slot, the
// Endpoint pick, onUpstreamRequest and the round trip.
func (ls *legState) attempt(ctx context.Context, a *resilience.Attempt) (step, *snapshot.UpstreamResponse, error) {
	u, rt := ls.u, ls.u.rt
	m := rt.m
	// The attempt context is the attempt's record, the one allocation of
	// an attempt besides its cancellation: a child of the leg context
	// whose Deadline is armed in place on the state's reused timer.
	rec := &attemptRecord{probe: m.traceProbe}
	cctx, cancel := context.WithCancelCause(ctx)
	rec.Context = cctx
	ls.armed.Store(&rec.dl)
	rec.dl.Init(cancel, m.clk, a.Deadline, a.Cause, ls.deadlineTimer())
	actx := context.Context(rec)
	end := attemptEnd{rt: rt, rec: rec, dl: &rec.dl, metrics: u.metrics, stripe: ls.o.Stripe, trace: ls.o.Trace}
	if err := rt.res.Bulkhead().Acquire(actx, &u.res.Bulkhead); err != nil {
		if errors.Is(err, resilience.ErrBulkheadFull) {
			end.finish()
			ls.leg.AbandonAttempt(resilience.GateBulkhead)
			return stepStop, nil, nil
		}
		// The attempt context ended while the attempt waited for a slot: a
		// timeout before any Endpoint, or the client went away.
		return ls.onError(ctx, actx, a, err, &end, false, "")
	}
	// A slot granted at or after the attempt deadline, before the timer's
	// function ran (or with the cancellation), is a timeout before any
	// Endpoint as well: fire a due deadline (Fire ignores an early call)
	// and hand the slot on, so an attempt never reaches an Endpoint past
	// its deadline.
	if rec.dl.Fire(); actx.Err() != nil {
		rt.res.Bulkhead().Release()
		return ls.onError(ctx, actx, a, fmt.Errorf("resilience: waiting for a bulkhead slot: %w", context.Cause(actx)), &end, false, "")
	}
	end.slot = true
	set := u.bal.bal.Load()
	tab, _ := set.Payload().(*table)
	ch, ok := ls.choose(set, tab)
	if !ok {
		end.finish()
		ls.leg.AbandonAttempt(resilience.GateNoEndpoint)
		return stepStop, nil, nil
	}
	ep := tab.eps[ch.Index].st
	body, cl, err := ls.openBody()
	if err != nil {
		// The body cannot be sent (a streamed body another reader took):
		// the attempt does not run.
		end.finish()
		ls.leg.AbandonAttempt(resilience.GateNone)
		return stepStop, nil, fmt.Errorf("upstream: open the request body: %w", err)
	}
	// This attempt runs: the response parked by the previous one is
	// discarded.
	ls.unpark()
	ls.tried = append(ls.tried, ep.identity)
	sctx, span := m.tracer.StartUpstream(actx, u.spanName, emit.UpstreamAttrs{Attempt: a.Number, Endpoint: ep.identity, Step: ls.t.Step})
	end.span = span
	rctx := ls.traced(sctx, rec)
	end.out = m.getOut()
	req := ls.newRequest(rctx, rec, end.out, ep, body, cl)
	m.tracer.Inject(sctx, ls.o.Trace, req.Header)
	ls.fleg = filter.Leg{Upstream: u.name, Step: ls.t.Step, Attempt: a.Number, Endpoint: ep.identity, Request: req, MaxBodyBytes: ls.t.MaxBodyBytes}
	if ls.hooks == nil && ls.o.Hooks != nil {
		ls.hooks = ls.o.Hooks.BeginLeg(ctx, u.name, ls.t.Step)
	}
	if ls.hooks != nil {
		if gen := ls.hooks.OnUpstreamRequest(sctx, &ls.fleg); gen != nil {
			closeBody(req)
			end.status = gen.Status
			end.finish()
			ls.leg.AbandonAttempt(resilience.GateNone)
			return stepDone, ls.generated(ctx, gen, resilience.LegNotCounted), nil
		}
		if r := ls.fleg.Request; r != nil && r != req {
			req = r
			if req.Context() != rctx {
				req = req.WithContext(rctx)
			}
		}
	}
	if a.Number > 1 {
		u.metrics.Retries.Add(ls.o.Stripe, 1)
	}
	end.ep = ep
	ep.inflight.Add(1)
	end.pool = rt.poolFor(ep, u.tls)
	end.start = m.clk.Now()
	tracked := false
	if j, ok := ls.view.health(ch.Index); ok {
		tracked = ls.view.hv.Begin(&ls.ha, j, end.start)
	}
	// The round trip is Upstream I/O, out of gateway-added time.
	ex := ls.o.Excluder
	if ex != nil {
		ex.Enter(end.start)
	}
	resp, err := end.pool.send.RoundTrip(req)
	if ex != nil {
		ex.Leave(m.clk.Now())
	}
	end.conn = ls.conn.Swap(nil)
	if err != nil {
		return ls.onError(ctx, actx, a, err, &end, tracked, ep.identity)
	}
	return ls.onResponse(ctx, sctx, a, resp, &end, tracked, ep.identity)
}

// traced returns the request context of the attempt whose span context is
// sctx, with the attempt's ClientTrace hooks: GotConn, and, on a plain
// upstreams leg whose Outbound relays Early Hints, the Got1xxResponse that
// relays each 103 with the token the attempt takes now
// (attemptRecord.got1xx). The pooled hook set serves an attempt without
// the relay; the relay's hook set is rec's own. When the span added
// nothing to the attempt context and no ClientTrace is there already, the
// attempt context itself answers httptrace's key (attemptRecord.Value);
// otherwise httptrace.WithClientTrace composes the hooks into sctx, on a
// hook set of the attempt's own, since composing writes into it.
func (ls *legState) traced(sctx context.Context, rec *attemptRecord) context.Context {
	t := &ls.trace
	if h := ls.o.Hints; h != nil && ls.t.Step == "" {
		rec.hints, rec.token = h, h.HintsToken()
		rec.trace = httptrace.ClientTrace{GotConn: ls.trace.GotConn, Got1xxResponse: rec.got1xx}
		t = &rec.trace
	}
	if sctx == context.Context(rec) && httptrace.ContextClientTrace(rec.Context) == nil {
		rec.tr = t
		return rec
	}
	if t == &ls.trace {
		t = &httptrace.ClientTrace{GotConn: ls.gotConn}
	}
	return httptrace.WithClientTrace(sctx, t)
}

// got1xx relays a 103 Early Hints response the attempt received to the
// client before the final response (docs/architecture/03-data-plane.md
// "Upstream layer"); every other 1xx response stays on its hop. It runs on
// a transport goroutine, possibly after the attempt ended, which is why
// the relay checks the token it was given; it never fails the attempt.
func (r *attemptRecord) got1xx(code int, h textproto.MIMEHeader) error {
	if code == http.StatusEarlyHints {
		r.hints.RelayHints(r.token, http.Header(h))
	}
	return nil
}

// choose picks the attempt's Endpoint on set, whose payload is tab, with
// the per-attempt view; false when the set is empty (503 RZ-UP-008).
func (ls *legState) choose(set *balance.Set, tab *table) (balance.Choice, bool) {
	if tab == nil || set.Len() == 0 {
		return balance.Choice{}, false
	}
	u := ls.u
	ls.view = attemptView{tab: tab, hv: u.rt.tracker.View(ls.now()), tried: ls.tried, maxConns: u.res.Bulkhead.MaxConnections}
	src := u.rt.m.rand
	var (
		ch  balance.Choice
		err error
	)
	if ls.random {
		ch, err = set.SelectRandom(src, &ls.view)
	} else {
		ch, err = set.Select(ls.key, src, &ls.view)
	}
	return ch, err == nil
}

// openBody opens the request body for one attempt.
func (ls *legState) openBody() (io.ReadCloser, int64, error) {
	b := ls.o.Body
	if b == nil {
		return http.NoBody, 0, nil
	}
	rc, err := b.Open()
	if err != nil {
		return nil, 0, err
	}
	if rc == http.NoBody {
		return rc, 0, nil
	}
	return rc, b.ContentLength(), nil
}

// replayable reports whether another attempt can send the body.
func (ls *legState) replayable() bool { return ls.o.Body == nil || ls.o.Body.Replayable() }

// bodyOver reports whether an attempt ended because the client body passed
// limits.maxRequestBodyBytes while it was sent: the body reader's
// filter.ErrTooLarge, which a transport may return wrapped or replace, or
// the body's own record of it (body.Request.Exceeded).
func (ls *legState) bodyOver(err error) bool {
	if errors.Is(err, filter.ErrTooLarge) {
		return true
	}
	ex, ok := ls.o.Body.(interface{ Exceeded() bool })
	return ok && ex.Exceeded()
}

// bodyBroken reports whether an attempt ended because reading the client
// body failed while it was sent (body.Request.Broken): its framing broke,
// it ended short of its declared length or its read deadline passed.
func (ls *legState) bodyBroken() bool {
	b, ok := ls.o.Body.(interface{ Broken() bool })
	return ok && b.Broken()
}

// onError handles an attempt that ended without response headers. An
// attempt that was sent (it has an Endpoint) runs onUpstreamResponseHeaders
// too, with no response and its error kind, on the leg's context (the
// attempt's has ended) under the attempt's span, which ends after the
// Phase: a Filter may ask for a retry, which retryOn then does not decide
// (Provider Fallback), or fail closed, which ends the leg.
func (ls *legState) onError(ctx, actx context.Context, a *resilience.Attempt, err error, end *attemptEnd, tracked bool, endpoint string) (step, *snapshot.UpstreamResponse, error) {
	u := ls.u
	if ls.bodyOver(err) {
		// The streamed client body passed limits.maxRequestBodyBytes while
		// the attempt sent it: the attempt ends without retry and counts as
		// no Upstream failure, in neither the breaker nor passive ejection;
		// the leg fails with 413 RZ-RT-003
		// (docs/architecture/03-data-plane.md "Streaming").
		if tracked {
			ls.ha.Cancel()
		}
		end.finish()
		ls.over, ls.cause, ls.endpoint = true, err, endpoint
		return stepStop, nil, nil
	}
	if ls.bodyBroken() {
		// Reading the client body failed while the attempt sent it: the
		// client's fault, so the attempt ends without retry (the body is
		// spent) and counts as no Upstream failure, in neither the
		// breaker nor passive ejection; the request aborts.
		if tracked {
			ls.ha.Cancel()
		}
		end.finish()
		ls.broken, ls.cause, ls.endpoint = true, err, endpoint
		return stepStop, nil, nil
	}
	kind, cause := classify(actx, err)
	end.errType = kind.String()
	if resilience.Canceled(actx) {
		// The client went away or a sibling step canceled the leg: no
		// Upstream failure, so no failureWhen, ejection, count or retry.
		if tracked {
			ls.ha.Cancel()
		}
		end.finish()
		ls.canceled, ls.cause, ls.kind = true, err, resilience.KindTimeout
		return stepStop, nil, nil
	}
	v := ls.legVars(a.Number, nil, kind, endpoint)
	failed := ls.failureWhen(ctx, v)
	if tracked {
		ls.ha.End(ls.now(), health.Outcome{Failed: failed, Cause: cause, Passive: u.passive, Stripe: ls.o.Stripe})
	}
	u.metrics.Attempts.Inc(ls.o.Stripe, 0, int(kind))
	// Upstream-scoped Policies are children of their attempt's span
	// (docs/architecture/10-observability.md "Span model"), so the span of
	// a sent attempt stays open through its onUpstreamResponseHeaders while
	// the rest of the attempt ends now.
	headersPhase := endpoint != "" && ls.hooks != nil
	var span emit.Span
	if headersPhase {
		span, end.span = end.span, nil
	}
	end.finish()
	ls.endpoint, ls.kind, ls.failed, ls.responded, ls.cause = endpoint, kind, failed, false, err
	in := resilience.RetryInput{Replayable: ls.replayable()}
	if headersPhase {
		ls.fleg.Response, ls.fleg.ErrorKind = nil, kind.String()
		retry, replace := ls.hooks.OnUpstreamResponseHeaders(u.rt.m.tracer.ContextWithSpan(ctx, span), &ls.fleg)
		endSpan(span, end.status, end.errType)
		if replace != nil {
			return stepDone, ls.generated(ctx, replace, ls.breakerResult(ls.leg.Outcome(false, kind))), nil
		}
		in.FilterRetry = retry
	}
	if !in.FilterRetry {
		in.RetryOn = ls.retryOn(ctx, v)
	}
	return ls.decide(in), nil, nil
}

// breakerResult returns how the leg, ending with o, counts for the
// breaker: a connect error counts only while most Endpoints are ejected
// (resilience.BreakerResult), so their count is read for it alone.
func (ls *legState) breakerResult(o resilience.Outcome) resilience.LegResult {
	ejected, total := 0, 0
	if !o.Responded && o.Kind == resilience.KindConnect && o.Attempts > 0 {
		healthy, n := ls.u.rt.tracker.Healthy(ls.now())
		ejected, total = n-healthy, n
	}
	return resilience.BreakerResult(ls.failed, o, ejected, total)
}

// onResponse handles an attempt that got response headers.
func (ls *legState) onResponse(ctx, sctx context.Context, a *resilience.Attempt, resp *http.Response, end *attemptEnd, tracked bool, endpoint string) (step, *snapshot.UpstreamResponse, error) {
	u := ls.u
	// Headers arrived: the attempt and leg deadlines stop applying, so a
	// slow body is not cut (only the Route timeout bounds it).
	end.dl.Stop()
	now := ls.now()
	if tracked {
		ls.ha.Headers(now)
	}
	// A Connection option names hop-by-hop fields of the whole response
	// (RFC 9110 section 7.6.1): the declared trailer fields it names go
	// with the header fields now, and the body drops the undeclared ones
	// net/http adds at its end (finalBody.Trailer).
	end.connOpts = resp.Header["Connection"]
	httpfield.StripHopByHop(resp.Header, httpfield.TowardClient)
	httpfield.StripNamed(resp.Trailer, end.connOpts)
	end.status = resp.StatusCode
	v := ls.legVars(a.Number, resp, resilience.KindNone, endpoint)
	failed := ls.failureWhen(ctx, v)
	if tracked {
		ls.ha.End(now, health.Outcome{Failed: failed, Passive: u.passive, Stripe: ls.o.Stripe})
	}
	u.metrics.Attempts.Inc(ls.o.Stripe, resp.StatusCode, emit.ErrNone)
	ls.endpoint, ls.kind, ls.failed, ls.responded, ls.cause = endpoint, resilience.KindNone, failed, true, nil
	ls.fleg.Response = resp
	var filterRetry bool
	var replace *filter.Response
	if ls.hooks != nil {
		filterRetry, replace = ls.hooks.OnUpstreamResponseHeaders(sctx, &ls.fleg)
	}
	if replace != nil {
		// A failed response Phase replaced the response (502 RZ-RT-012):
		// final, no retry.
		_ = resp.Body.Close()
		end.finish()
		end.recycle()
		result := resilience.BreakerResult(failed, ls.leg.Outcome(true, resilience.KindNone), 0, 0)
		return stepDone, ls.generated(ctx, replace, result), nil
	}
	in := resilience.RetryInput{FilterRetry: filterRetry, Replayable: ls.replayable(), RetryAfter: resp.Header.Get("Retry-After")}
	if !filterRetry {
		in.RetryOn = ls.retryOn(ctx, v)
	}
	if ls.decide(in) == stepRetry {
		// The slot and the in-flight count go now; the body is drained
		// once the retry attempt runs, or returned if it cannot start.
		end.release()
		ls.park = parked{resp: resp, end: *end, fleg: ls.fleg}
		return stepRetry, nil, nil
	}
	return stepDone, ls.final(ctx, resp, *end), nil
}

// decide applies the retry rule and counts a retry the budget refused.
func (ls *legState) decide(in resilience.RetryInput) step {
	dec := ls.leg.Decide(in)
	if dec.Reason == resilience.ReasonBudget {
		ls.u.metrics.RetryBudgetExhausted.Add(ls.o.Stripe, 1)
	}
	if !dec.Retry {
		return stepStop
	}
	ls.delay = dec.Delay
	return stepRetry
}

// final returns the leg's response: the breaker counts the leg,
// onUpstreamResponseBody runs, the leg's hooks end, and the body keeps
// the attempt open until it is closed or read to the end.
func (ls *legState) final(ctx context.Context, resp *http.Response, end attemptEnd) *snapshot.UpstreamResponse {
	u := ls.u
	o := ls.leg.Outcome(true, resilience.KindNone)
	ls.leg.End(resilience.BreakerResult(ls.failed, o, 0, 0))
	rec := end.rec
	fb := &rec.body
	fb.rc, fb.resp, fb.end, fb.ex, fb.clk = resp.Body, resp, end, ls.o.Excluder, ls.u.rt.m.clk
	resp.Body = fb
	ls.fleg.Response = resp
	var replace *filter.Response
	if ls.hooks != nil {
		// The body Phase runs on the leg context, since reading the body to
		// its end finishes the attempt and cancels the attempt context,
		// under the attempt's span, which stays open until the Phase ends:
		// upstream-scoped Policies are children of their attempt's span
		// (docs/architecture/10-observability.md "Span model"). The body is
		// read on this goroutine only until the leg returns.
		span := fb.end.span
		fb.end.span = nil
		replace = ls.hooks.OnUpstreamResponseBody(u.rt.m.tracer.ContextWithSpan(ctx, span), &ls.fleg)
		if fb.done.Load() {
			endSpan(span, fb.end.status, fb.end.errType)
		} else {
			fb.end.span = span
		}
	}
	if replace != nil {
		_ = fb.Close()
		ls.endHooks(ctx)
		rec.resp = snapshot.UpstreamResponse{Status: replace.Status, Generated: replace, Upstream: u.name, Endpoint: ls.endpoint, Attempts: o.Attempts}
		return &rec.resp
	}
	r := ls.fleg.Response
	if r == nil {
		r = resp
	}
	var body io.ReadCloser = fb
	if r.Body != fb {
		// A body Phase replaced the body: closing the replacement also ends
		// the attempt, if the Phase did not.
		body = &joinedBody{ReadCloser: r.Body, attempt: fb}
	}
	ls.endHooks(ctx)
	rec.resp = snapshot.UpstreamResponse{
		Status:   r.StatusCode,
		Header:   r.Header,
		Trailer:  r.Trailer,
		Body:     body,
		Upstream: u.name,
		Endpoint: ls.endpoint,
		Attempts: o.Attempts,
	}
	return &rec.resp
}

// generated ends the leg with a leg Filter's response.
func (ls *legState) generated(ctx context.Context, gen *filter.Response, result resilience.LegResult) *snapshot.UpstreamResponse {
	attempts := ls.leg.Attempts()
	ls.leg.End(result)
	ls.endHooks(ctx)
	return &snapshot.UpstreamResponse{Status: gen.Status, Generated: gen, Upstream: ls.u.name, Endpoint: ls.endpoint, Attempts: attempts}
}

// end finishes a leg whose loop stopped: a parked response is returned
// unchanged (its retry could not start); otherwise the leg fails with the
// code selection rule's code.
func (ls *legState) end(ctx context.Context) (*snapshot.UpstreamResponse, error) {
	if p := ls.park; p.resp != nil {
		ls.park = parked{}
		ls.fleg = p.fleg
		return ls.final(ctx, p.resp, p.end), nil
	}
	u := ls.u
	if ls.over {
		attempts := ls.leg.Attempts()
		ls.leg.End(resilience.LegNotCounted)
		ls.endHooks(ctx)
		return nil, &LegError{Upstream: u.name, Endpoint: ls.endpoint, Attempts: attempts, Err: errcode.Wrap(codeRequestTooLarge, ls.cause)}
	}
	if ls.broken {
		attempts := ls.leg.Attempts()
		ls.leg.End(resilience.LegNotCounted)
		ls.endHooks(ctx)
		err := ErrClientBody
		if ls.cause != nil {
			err = fmt.Errorf("%w: %w", ErrClientBody, ls.cause)
		}
		return nil, &LegError{Upstream: u.name, Endpoint: ls.endpoint, Attempts: attempts, Err: err}
	}
	if ls.canceled {
		attempts := ls.leg.Attempts()
		ls.leg.End(resilience.LegNotCounted)
		ls.endHooks(ctx)
		err := resilience.Outcome{Kind: resilience.KindTimeout, Attempts: max(attempts, 1)}.Err(ls.cause)
		return nil, &LegError{Upstream: u.name, Endpoint: ls.endpoint, Attempts: attempts, Kind: resilience.KindTimeout, Err: err}
	}
	o := ls.leg.Outcome(false, ls.kind)
	ls.leg.End(ls.breakerResult(o))
	ls.endHooks(ctx)
	return nil, &LegError{Upstream: u.name, Endpoint: ls.endpoint, Attempts: o.Attempts, Kind: o.Kind, Err: o.Err(ls.cause)}
}

// endHooks ends the leg's Phases once.
func (ls *legState) endHooks(ctx context.Context) {
	if ls.hooks != nil {
		ls.hooks.End(ctx)
		ls.hooks = nil
	}
}

// unpark drains and closes the parked response: up to
// resilience.RetryDrainBytes of its body, within the leg deadline, so an
// HTTP/1.1 connection can be reused.
func (ls *legState) unpark() {
	p := &ls.park
	if p.resp == nil {
		return
	}
	clk := ls.u.rt.m.clk
	var t clock.Timer
	if d := ls.leg.Deadline(); !d.IsZero() && p.end.dl != nil {
		t = clk.AfterFunc(max(d.Sub(clk.Now()), 0), p.end.dl.Cancel)
	}
	// The drain reads the Upstream: out of gateway-added time.
	ex := ls.o.Excluder
	if ex != nil {
		ex.Enter(clk.Now())
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(p.resp.Body, resilience.RetryDrainBytes))
	if ex != nil {
		ex.Leave(clk.Now())
	}
	if t != nil {
		t.Stop()
	}
	_ = p.resp.Body.Close()
	p.end.finish()
	p.end.recycle()
	ls.park = parked{}
}

// legVars returns the leg activation of retryOn and failureWhen: the
// client request (without body), the response or the error, the attempt
// and the upstream (name and Endpoint).
func (ls *legState) legVars(attempt int, resp *http.Response, kind resilience.Kind, endpoint string) *expr.Vars {
	v := &ls.vars
	if ls.base != nil {
		*v = *ls.base
	} else {
		*v = expr.Vars{}
	}
	if v.Request == nil {
		ls.req.Method = ls.method
		v.Request = &ls.req
	}
	v.Response, v.Error = nil, nil
	if resp != nil {
		ls.resp = expr.Response{Status: resp.StatusCode, Header: resp.Header}
		v.Response = &ls.resp
	} else {
		ls.errv = expr.AttemptError{Kind: kind.String()}
		v.Error = &ls.errv
	}
	v.Attempt = attempt
	ls.upv = expr.Upstream{Name: ls.u.name, Endpoint: endpoint}
	v.Upstream = &ls.upv
	return v
}

// failureWhen evaluates circuitBreaker.failureWhen; a runtime error counts
// as a failure and in ruralz_upstream_cel_errors_total.
func (ls *legState) failureWhen(ctx context.Context, v *expr.Vars) bool {
	failed, err := resilience.EvalFailureWhen(ctx, ls.u.res.Breaker.FailureWhen, v)
	if err != nil {
		ls.u.metrics.CELErrors[emit.CELFailureWhen].Add(ls.o.Stripe, 1)
	}
	return failed
}

// retryOn evaluates retries.retryOn; a runtime error means no retry and
// counts in ruralz_upstream_cel_errors_total.
func (ls *legState) retryOn(ctx context.Context, v *expr.Vars) bool {
	ok, err := resilience.EvalRetryOn(ctx, ls.u.res.Retry.RetryOn, v)
	if err != nil {
		ls.u.metrics.CELErrors[emit.CELRetryOn].Add(ls.o.Stripe, 1)
	}
	return ok
}

// now reads the clock.
func (ls *legState) now() time.Time { return ls.u.rt.m.clk.Now() }

// closeBody closes the body of a request that was not sent.
func closeBody(r *http.Request) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
}

// attemptRecord is the one allocation of an attempt besides its
// cancellation, everything of it that outlives the leg or that net/http
// may still read: the attempt context (it embeds the context.Context of
// context.WithCancelCause, so net/http's own contexts register with it
// directly), its Deadline, the outgoing request and its URL, and, when it
// is the leg's response, the body wrapper and the result. An attempt that
// relays Early Hints also keeps its hook set here, with the relay and the
// token it took, so a 1xx that arrives after the leg's pooled state was
// reused still names this attempt.
type attemptRecord struct {
	context.Context
	// tr is the hook set the context answers httptrace's key with; nil
	// answers through the embedded context.
	tr *httptrace.ClientTrace
	// probe tells httptrace's key from others (Manager.traceProbe).
	probe context.Context
	dl    resilience.Deadline
	req   http.Request
	url   url.URL
	body  finalBody
	resp  snapshot.UpstreamResponse
	trace httptrace.ClientTrace
	hints snapshot.EarlyHints
	token uint64
}

// Value answers httptrace's client trace key with the attempt's hook set,
// so the attempt needs no context.WithValue for it, and every other key
// through the embedded context: the cancellation keys reach the embedded
// cancel context, which net/http's contexts then register with.
func (r *attemptRecord) Value(key any) any {
	if r.tr != nil && r.probe.Value(key) != nil {
		return r.tr
	}
	return r.Context.Value(key)
}

// attemptEnd is what an attempt holds until it ends: the bulkhead slot,
// the Endpoint's in-flight count, the connection count, the span and the
// attempt context.
type attemptEnd struct {
	rt   *runtime
	rec  *attemptRecord
	ep   *epState
	pool *pool
	conn *trackedConn
	span emit.Span
	dl   *resilience.Deadline
	// out is the request's pooled header, returned by recycle once the
	// response body was closed.
	out     *outHeader
	metrics *emit.UpstreamMetrics
	// trace is the request's sampling decision: a sampled request's
	// duration carries an exemplar (trace ID, server span ID).
	trace   *emit.Decision
	stripe  emit.Stripe
	start   time.Time
	status  int
	errType string
	slot    bool
	// connOpts holds the response's Connection field values, read before the
	// header strip: they name hop-by-hop fields of its trailer section
	// too, which net/http completes only as the body ends.
	connOpts []string
}

// release returns the bulkhead slot and the Endpoint's in-flight count:
// the attempt no longer loads the Upstream.
func (e *attemptEnd) release() {
	if e.slot {
		e.rt.res.Bulkhead().Release()
		e.slot = false
	}
	if e.ep != nil {
		e.ep.inflight.Add(-1)
		e.ep = nil
	}
}

// finish ends the attempt: release, the connection's count, the attempt
// duration (connect to last byte), the span and the attempt context.
func (e *attemptEnd) finish() {
	e.release()
	if e.conn != nil {
		e.conn.end()
		e.conn = nil
	}
	if !e.start.IsZero() {
		now := e.rt.m.clk.Now()
		d := uint64(max(now.Sub(e.start), 0))
		if t := e.trace; t != nil && t.Sampled {
			e.metrics.AttemptDuration.RecordExemplar(e.stripe, d, emit.Exemplar{TraceID: t.TraceID, SpanID: t.ServerSpanID, Time: now})
		} else {
			e.metrics.AttemptDuration.Record(e.stripe, d)
		}
		e.start = time.Time{}
	}
	if e.span != nil {
		e.span.End(e.status, "", e.errType)
		e.span = nil
	}
	if e.dl != nil {
		e.dl.Cancel()
		e.dl = nil
	}
	if e.pool != nil {
		e.pool.done()
		e.pool = nil
	}
}

// recycle returns the request's header to the pool once the caller closed
// the attempt's response body, after which net/http no longer reads the
// request (http.RoundTripper). An attempt without a response never calls
// it: a transport may still write the request then.
func (e *attemptEnd) recycle() {
	if e.out != nil {
		e.rt.m.putOut(e.out)
		e.out = nil
	}
}

// endSpan ends an attempt's span that a leg Phase kept open past the rest
// of the attempt, as attemptEnd.finish would have; nil ends nothing.
func endSpan(s emit.Span, status int, errType string) {
	if s != nil {
		s.End(status, "", errType)
	}
}

// finalBody is the body of the leg's response. The attempt ends when it is
// closed or read to its end (or fails), whichever is first: the bulkhead
// slot is held for the whole response, streaming included.
type finalBody struct {
	rc io.ReadCloser
	// resp is the leg's response, whose Trailer net/http completes as the
	// body ends.
	resp *http.Response
	end  attemptEnd
	// ex marks each read out of gateway-added time (Outbound.Excluder);
	// nil marks nothing.
	ex   emit.Excluder
	clk  clock.Clock
	done atomic.Bool
	// closed is set by the first Close, which recycles the request's
	// header.
	closed atomic.Bool
}

// Read reads the response body, as Upstream I/O; the first error (io.EOF
// included) ends the attempt.
func (b *finalBody) Read(p []byte) (int, error) {
	if b.ex != nil {
		b.ex.Enter(b.clk.Now())
	}
	n, err := b.rc.Read(p)
	if b.ex != nil {
		b.ex.Leave(b.clk.Now())
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			// net/http has completed the trailer section: drop the
			// undeclared fields the response's Connection field names.
			httpfield.StripNamed(b.resp.Trailer, b.end.connOpts)
		}
		b.finish()
	}
	return n, err
}

// Close closes the response body and ends the attempt; the first Close
// also recycles the request's header, which net/http no longer reads.
func (b *finalBody) Close() error {
	err := b.rc.Close()
	b.finish()
	if b.closed.CompareAndSwap(false, true) {
		b.end.recycle()
	}
	return err
}

// finish ends the attempt once.
func (b *finalBody) finish() {
	if b.done.CompareAndSwap(false, true) {
		b.end.finish()
	}
}

// Trailer returns the trailer section of the leg's response as it stands:
// complete once the body was read to its end, fields the response did not
// declare included, since net/http adds those only then, without the
// fields the response's Connection field names. Read it on the goroutine
// that read the body.
func (b *finalBody) Trailer() http.Header {
	httpfield.StripNamed(b.resp.Trailer, b.end.connOpts)
	return b.resp.Trailer
}

// joinedBody is a body a body Phase put in place of the Upstream's; its
// Close also ends the attempt.
type joinedBody struct {
	io.ReadCloser
	attempt *finalBody
}

// Close closes the replacement and ends the attempt.
func (b *joinedBody) Close() error {
	err := b.ReadCloser.Close()
	_ = b.attempt.Close()
	return err
}

// Trailer returns the trailer section of the Upstream's response
// (finalBody.Trailer).
func (b *joinedBody) Trailer() http.Header { return b.attempt.Trailer() }
