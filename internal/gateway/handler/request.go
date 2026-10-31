// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/exchange"
	"github.com/ravindu-rev/ruralz/internal/gateway/executor"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/tap"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// state is one request's pooled handler state. It belongs to the request
// goroutine between Get and Put, except what the 103 relay reads under
// wmu and what the ending protocol reaches through rec.
type state struct {
	h *Handler

	// rec is the pinned request the ending protocol acts on, and rc the
	// controller it moves the connection deadlines through; the handler
	// commits under rec.Mu.
	rec snapshot.PinnedRequest
	rc  http.ResponseController

	// wmu orders the 103 relays against the commit and the handler's end:
	// it guards gen, committed and done, and w's header map and writes
	// until commit. gen moves on with every request, so a relay that holds
	// an earlier request's token writes nothing.
	wmu       sync.Mutex
	gen       uint64
	committed bool
	done      bool

	w    http.ResponseWriter
	r    *http.Request
	conn Conn
	// start is the request start on the handler's clock; wallStart the
	// same instant on the wall clock, which dates the socket deadlines.
	start         time.Time
	wallStart     time.Time
	stripe        emit.Stripe
	cancel        context.CancelCauseFunc
	cancelFn      context.CancelFunc
	deadlineTimer clock.Timer

	snap         *snapshot.Snapshot
	view         *snapView
	lis          *snapshot.Listener
	lm           *emit.ListenerMetrics
	x            *exchange.Request
	run          *executor.Run
	route        *snapshot.Route
	decision     emit.Decision
	span         emit.Span
	spanEnded    bool
	tls          filter.ConnTLS
	src          filter.Source
	resp         filter.Response
	deadline     time.Time
	listenerName string
	scheme       string
	path         string
	pathCode     string
	hints        bool
	// connOpts are the client's Connection field values, read before the
	// admission strip removes them: they name hop-by-hop fields of the
	// request's trailer section too.
	connOpts []string

	// The outcome the commit and the copy leave for finish.
	status       int
	code         string
	generated    bool
	fromUpstream bool
	abort        bool
	respBytes    int64
	upstream     string
	endpoint     string
	attempts     int
	gwEnd        time.Time
	stream       io.ReadCloser
	upTrailer    http.Header
	trailerKeys  []string
	// up is the Excluder the Upstream layer marks its I/O on.
	up upstreamClock
	// reached records that an attempt reached an Upstream (Outbound.Reached).
	reached atomic.Bool

	buf     []byte
	scratch []byte
	ev      tap.Event
}

// newState returns a state for h; its cancel hook is made once.
func newState(h *Handler) *state {
	st := &state{h: h}
	st.cancelFn = st.cancelRoot
	return st
}

// cancelRoot cancels the request's root context: the ending protocol's
// rec.Cancel.
func (st *state) cancelRoot() {
	if c := st.cancel; c != nil {
		c(nil)
	}
}

// reset drops everything st holds for the next request, keeping its
// buffers.
func (st *state) reset() {
	st.rec.Committed, st.rec.Ended, st.rec.Cancel, st.rec.RC = false, snapshot.EndNone, nil, nil
	st.rc = http.ResponseController{}
	st.w, st.r, st.conn, st.start, st.wallStart, st.stripe, st.cancel, st.deadlineTimer = nil, nil, nil, time.Time{}, time.Time{}, 0, nil, nil
	st.snap, st.view, st.lis, st.lm, st.x, st.run, st.route = nil, nil, nil, nil, nil, nil, nil
	st.decision, st.span, st.spanEnded, st.tls, st.src, st.resp = emit.Decision{}, nil, false, filter.ConnTLS{}, filter.Source{}, filter.Response{}
	st.deadline, st.listenerName, st.scheme, st.path, st.pathCode, st.hints, st.connOpts = time.Time{}, "", "", "", "", false, nil
	st.status, st.code, st.generated, st.fromUpstream, st.abort, st.respBytes = 0, "", false, false, false, 0
	st.upstream, st.endpoint, st.attempts, st.gwEnd, st.stream, st.upTrailer = "", "", 0, time.Time{}, nil, nil
	clear(st.trailerKeys)
	st.trailerKeys = st.trailerKeys[:0]
	st.scratch = st.scratch[:0]
	st.ev = tap.Event{}
}

// serve runs one admitted request.
func (st *state) serve(w http.ResponseWriter, r *http.Request, conn Conn, start time.Time) {
	h := st.h
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	st.begin(w, r, conn, start, cancel)
	snap := h.snaps.Pin(st.stripe, &st.rec)
	if snap == nil {
		st.unpublished()
		return
	}
	defer h.snaps.Unpin(snap, st.stripe, &st.rec)
	defer st.release()
	defer st.recoverFault(ctx)
	st.snap = snap
	st.handle(ctx)
}

// begin records what the request knows before it pins a snapshot.
func (st *state) begin(w http.ResponseWriter, r *http.Request, conn Conn, start time.Time, cancel context.CancelCauseFunc) {
	h := st.h
	st.w, st.r, st.conn, st.start, st.cancel = w, r, conn, start, cancel
	st.wallStart = h.wall.Now()
	if conn != nil {
		st.listenerName = conn.Listener()
		// The sequence stands in for an HTTP/2 stream ID, which
		// RequestStripe halves.
		st.stripe = h.snaps.RequestStripe(conn.Stripe(), 2*conn.NextRequest())
	}
	st.rc = *http.NewResponseController(w)
	st.rec.RC, st.rec.Cancel = &st.rc, st.cancelFn
	st.up.up.Reset(start)
	st.reached.Store(false)
	st.hints = r.ProtoAtLeast(1, 1)
	st.wmu.Lock()
	st.gen++
	st.committed, st.done = false, false
	st.wmu.Unlock()
}

// unpublished answers a request that found no published snapshot,
// possible only around shutdown: 503 RZ-RT-016, without an access record
// or /tap event, since no snapshot selects or redacts them.
func (st *state) unpublished() {
	h := st.h
	h.tracer.Decide(st.r.Header, 0, &st.decision)
	_ = st.rc.SetWriteDeadline(st.wallStart.Add(AdmissionWindow))
	h.writeBareProblem(st.w, st.r, codeDrain, &st.decision)
	h.countBare(st.r, st.conn, codeDrain)
}

// release ends the request's use of its pooled parts, before the pin goes:
// no 103 relay writes from now on, the Route deadline timer stops, the
// Upstream body closes, and the executor run and the exchange return to
// their pools.
func (st *state) release() {
	st.wmu.Lock()
	st.done = true
	st.wmu.Unlock()
	if st.deadlineTimer != nil {
		st.deadlineTimer.Stop()
	}
	if st.stream != nil {
		_ = st.stream.Close()
		st.stream = nil
	}
	if st.run != nil {
		st.run.Release()
		st.run = nil
	}
	if st.x != nil {
		st.h.pool.Put(st.x)
		st.x = nil
	}
}

// recoverFault recovers a fault in request handling outside the Filter
// Chain (docs/architecture/03-data-plane.md "Goroutines"): it logs the
// panic with its stack and the trace ID, the requestId the client sees, at
// error, and answers 500 RZ-RT-025 with a problem document when nothing
// was written yet, else aborts with http.ErrAbortHandler, which resets the
// HTTP/2 stream or closes the HTTP/1.1 connection. The response counts in
// the Route (or _unmatched), listener and Node response metrics and ends
// the server span; onLog, the access record and /tap do not run on state a
// fault left. The deferred release and Unpin that follow free the pooled
// state and the pin, ServeHTTP the in-flight unit. An http.ErrAbortHandler
// panic is the handler's own abort and passes on.
func (st *state) recoverFault(ctx context.Context) {
	v := recover()
	if v == nil {
		return
	}
	if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(v)
	}
	h := st.h
	h.log.LogAttrs(ctx, slog.LevelError, "request handling panic recovered",
		slog.String(catalog.KeyListener, st.listenerName),
		slog.String(catalog.KeyTraceID, hex.EncodeToString(st.decision.TraceID[:])),
		slog.String(catalog.KeyPanicType, fmt.Sprintf("%T", v)),
		slog.String(catalog.KeyStack, string(debug.Stack())))
	committed := !st.commitBare(codeFault)
	status, code := h.statusOf(codeFault), codeFault
	if committed {
		// The stream ends with an error and has no status of its own.
		status = 0
	}
	if st.lm == nil {
		st.lm = h.listenerMetrics(st.conn, nil)
	}
	if status != 0 {
		rm := &h.noop.route
		switch {
		case st.route != nil && st.route.Metrics != nil:
			rm = st.route.Metrics
		case st.view != nil:
			rm = st.view.unmatched
		}
		rm.Requests.Inc(st.stripe, status)
		st.lm.Requests.Inc(st.stripe, protoIndex(st.r), status, emit.OriginNode)
	}
	h.node.NodeResponses.Inc(st.stripe, code)
	if st.span != nil && !st.spanEnded {
		st.spanEnded = true
		st.span.End(status, code, "")
	}
	if committed {
		panic(http.ErrAbortHandler)
	}
}

// listenerMetrics returns the handles of l, else of the connection's
// listener at accept, else no-op handles.
func (h *Handler) listenerMetrics(conn Conn, l *snapshot.Listener) *emit.ListenerMetrics {
	if l != nil && l.Metrics != nil {
		return l.Metrics
	}
	if conn != nil {
		if m := conn.Metrics(); m != nil {
			return m
		}
	}
	return &h.noop.listener
}

// handle runs the request on its pinned snapshot: the trace decision and
// server span, the admission deadlines, request hardening, the Router and
// the matched Route.
func (st *state) handle(ctx context.Context) {
	h, r, snap := st.h, st.r, st.snap
	st.view = h.viewOf(snap)
	st.lis = listenerOf(snap, st.listenerName)
	st.lm = h.listenerMetrics(st.conn, st.lis)
	st.lm.Active.Add(st.stripe, 1)
	defer st.lm.Active.Add(st.stripe, -1)
	st.scheme = "http"
	if r.TLS != nil || (st.lis != nil && st.lis.Protocol == v1alpha1.ListenerProtocolHTTPS) {
		st.scheme = "https"
	}

	h.tracer.Decide(r.Header, snap.Gateway.TraceSampling, &st.decision)
	sctx, span := h.tracer.StartServer(ctx, &st.decision, r.Method, emit.ServerAttrs{
		Listener: st.listenerName, Scheme: st.scheme, Host: r.Host, Path: routematch.RequestPath(r.URL),
		UserAgent: r.Header.Get("User-Agent"), Protocol: protoLabel(r), Port: listenerPort(st.lis),
	})
	st.span = span
	if !st.extendDeadlines(st.start.Add(AdmissionWindow)) {
		st.ended(sctx)
		return
	}

	st.path, st.pathCode = normalizePath(r)
	st.src = st.source()
	x := h.pool.Get()
	st.x = x
	x.Begin(r, &exchange.Start{
		TraceID: st.decision.TraceID, Time: st.start, Listener: st.listenerName, Scheme: st.scheme,
		Path: st.path, Source: st.src, TLS: st.connTLS(), Stripe: st.stripe, Limits: snap.Gateway.Limits,
	})
	st.up.gw = x.Timer()
	if code := st.harden(); code != "" {
		st.preRouting(sctx, code)
		return
	}
	// A Connection field names the fields present now, so the end-to-end
	// fields it lists go before any Policy can set one of them for the
	// Upstream; the fixed hop-by-hop fields go at leg build. Its values
	// stay for the trailer section, whose fields it names too (forward).
	st.connOpts = r.Header["Connection"]
	httpfield.StripConnectionOptions(r.Header)

	router := snap.Routers[st.listenerName]
	if router == nil {
		st.preRouting(sctx, codeNoRoute)
		return
	}
	mctx, mspan := h.tracer.StartRouteMatch(sctx)
	res := router.Match(mctx, x.MatchRequest(), x.ParamBuf())
	routeName := unmatchedRoute
	if res.Err == nil && res.Route >= 0 && int(res.Route) < len(snap.Routes) {
		routeName = snap.Routes[res.Route].Name
	}
	mspan.SetAttr(catalog.AttrRoute, slog.StringValue(routeName))
	switch {
	case res.Err != nil:
		mspan.End(0, codeWhen, "")
		st.preRouting(sctx, codeWhen)
		return
	case res.Route < 0 || int(res.Route) >= len(snap.Routes):
		mspan.End(0, codeNoRoute, "")
		st.preRouting(sctx, codeNoRoute)
		return
	}
	mspan.End(0, "", "")
	rt := snap.Routes[res.Route]
	x.SetRoute(rt, res.Params)
	span.SetAttr(catalog.AttrRoute, slog.StringValue(rt.Name))
	st.proxy(sctx, rt)
}

// listenerPort returns the listener's port, 0 when unknown.
func listenerPort(l *snapshot.Listener) int {
	if l == nil {
		return 0
	}
	return l.Port
}

// normalizePath returns the one normalized path of r
// (routematch.NormalizePath) or, when request hardening refuses it, the
// escaped path as received with the refusal's code: RZ-RT-017, or
// RZ-RT-001 for the asterisk form.
func normalizePath(r *http.Request) (path, code string) {
	raw := routematch.RequestPath(r.URL)
	p, err := routematch.NormalizePath(raw)
	if err != nil {
		code, _ = errcode.CodeOf(err)
		if code == "" {
			code = codeRejected
		}
		return raw, code
	}
	return p, ""
}

// source returns the client address after trusted-proxy and PROXY v2
// handling (internal/clientaddr) with the snapshot's trusted proxies.
func (st *state) source() filter.Source {
	var peer netip.AddrPort
	if st.conn != nil {
		peer = st.conn.Peer()
	} else if ap, err := netip.ParseAddrPort(st.r.RemoteAddr); err == nil {
		peer = ap
	}
	res := st.view.trusted.Resolve(peer, st.r.Header)
	return filter.Source{IP: res.IP, Port: res.Port, Peer: res.Peer, FromHeader: res.FromHeader()}
}

// connTLS returns the request's TLS state, nil on cleartext.
func (st *state) connTLS() *filter.ConnTLS {
	if st.r.TLS == nil {
		return nil
	}
	st.tls = filter.ConnTLS{State: st.r.TLS}
	if c := st.conn; c != nil {
		st.tls.ClientCertRequested, st.tls.Cache = c.ClientCertRequested(), c.Cache()
	}
	return &st.tls
}

// extendDeadlines moves the connection's body read and response write
// deadlines to t, an instant on the handler's clock, under rec.Mu, unless
// the ending protocol already ended the request (it moved them itself);
// false then. The deadlines are socket deadlines the Go runtime enforces,
// so they are dated on the wall clock: t's distance from the request
// start, from the request's wall-clock start.
func (st *state) extendDeadlines(t time.Time) bool {
	st.rec.Mu.Lock()
	defer st.rec.Mu.Unlock()
	if st.rec.Ended != snapshot.EndNone {
		return false
	}
	wt := st.wallStart.Add(t.Sub(st.start))
	// A writer without deadlines (http.ErrNotSupported) is served without
	// them.
	_ = st.rc.SetReadDeadline(wt)
	_ = st.rc.SetWriteDeadline(wt)
	return true
}

// ended answers a request the ending protocol ended before any work, such
// as one that pinned after the Drain deadline: 503 with the end code.
func (st *state) ended(ctx context.Context) {
	st.path, _ = normalizePath(st.r)
	st.src = st.source()
	x := st.h.pool.Get()
	st.x = x
	x.Begin(st.r, &exchange.Start{
		TraceID: st.decision.TraceID, Time: st.start, Listener: st.listenerName, Scheme: st.scheme,
		Path: st.path, Source: st.src, Stripe: st.stripe, Limits: st.snap.Gateway.Limits,
	})
	code := st.endCode()
	x.SetGenerated(st.nodeResponse(code))
	st.commitBare(code)
	st.finish(ctx)
}

// endCode returns the code of the end reason the ending protocol set.
func (st *state) endCode() string {
	st.rec.Mu.Lock()
	defer st.rec.Mu.Unlock()
	if c := retire.Code(st.rec.Ended); c != "" {
		return c
	}
	return codeDrain
}

// preRouting answers a request before or at routing: RZ-RT-001, RZ-RT-002,
// RZ-RT-006 or RZ-RT-017. No Filter Chain runs; the response gets a 5 s
// write deadline through the pre-routing limiter, which aborts the request
// without writing past its bound, and the request still gets an access
// record, selected by the snapshot's accessLog.when, and a /tap event.
func (st *state) preRouting(ctx context.Context, code string) {
	st.x.SetGenerated(st.nodeResponse(code))
	st.h.pre.Do(&st.rc, func() { st.commitBare(code) })
	st.finish(ctx)
}

// nodeResponse returns the Node-generated response with code at its
// status, in st's memory (SetGenerated copies it).
func (st *state) nodeResponse(code string) *filter.Response {
	st.resp = filter.Response{Status: st.h.statusOf(code), Code: code}
	return &st.resp
}

// proxy runs a matched Route: the Route deadline, the dispatch on the
// Upstream protocol, the request Phases, the request body, the Forwarder,
// the response Phases, the commit and onLog.
func (st *state) proxy(ctx context.Context, rt *snapshot.Route) {
	h, x := st.h, st.x
	st.route = rt
	st.deadline = st.start.Add(rt.Timeout)
	st.armDeadline(rt.Timeout)
	if !st.extendDeadlines(st.deadline) {
		st.respondEnded(ctx)
		return
	}
	st.run = h.exec.Begin(x, &rt.Chain, storeOf(st.snap))
	var gen *filter.Response
	switch {
	case rt.Protocol != "" && rt.Protocol != v1alpha1.UpstreamProtocolHTTP:
		// The serve check refuses every other protocol in M1; a snapshot
		// holding one is a fault, not a client error.
		gen = st.nodeResponse(codeFault)
	case upgradesToWebSocket(st.r):
		// Rejected after the match and before onRequestHeaders; the
		// response Phases run on it as on any generated response
		// (docs/architecture/07-multi-protocol.md "One listener, dispatch
		// after the match").
		gen = st.nodeResponse(codeWebSocket)
	default:
		gen = st.run.Request(ctx, phase.OnRequestHeaders)
		if gen == nil {
			gen = st.prepareBody(ctx)
		}
		if gen == nil && !st.abort {
			gen = st.run.Request(ctx, phase.OnRequestBody)
		}
		if gen == nil && !st.abort {
			gen = st.run.Request(ctx, phase.OnRoute)
		}
	}
	kind := executor.ResponseGenerated
	switch {
	case st.abort:
	case gen != nil:
		x.SetGenerated(gen)
	default:
		kind = st.forward(ctx, rt)
	}
	if !st.abort {
		st.run.Response(ctx, kind)
		st.commit()
	}
	st.finish(ctx)
	if st.abort {
		panic(http.ErrAbortHandler)
	}
}

// respondEnded answers a matched request the ending protocol ended before
// its Route work started.
func (st *state) respondEnded(ctx context.Context) {
	code := st.endCode()
	st.x.SetGenerated(st.nodeResponse(code))
	st.commitBare(code)
	st.finish(ctx)
}

// armDeadline cancels the request's root context with
// context.DeadlineExceeded once its Route timeout has passed from the
// request start, on the handler's clock: State Store calls, legs and body
// reads end then, and an Upstream attempt cut by it counts as timed out.
func (st *state) armDeadline(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	wait := st.deadline.Sub(st.h.clk.Now())
	cancel := st.cancel
	if wait <= 0 {
		cancel(context.DeadlineExceeded)
		return
	}
	st.deadlineTimer = st.h.clk.AfterFunc(wait, func() { cancel(context.DeadlineExceeded) })
}

// storeOf returns the snapshot's main State Store, nil without one.
func storeOf(s *snapshot.Snapshot) statestore.Store {
	if s.StateStore == nil {
		return nil
	}
	return s.StateStore.Store()
}

// upgradesToWebSocket reports whether r asks for a WebSocket upgrade: an
// Upgrade field listing the websocket token, in any case.
func upgradesToWebSocket(r *http.Request) bool {
	for _, v := range r.Header["Upgrade"] {
		for v != "" {
			var tok string
			tok, v, _ = strings.Cut(v, ",")
			tok = strings.TrimSpace(tok)
			if i := strings.IndexByte(tok, '/'); i >= 0 {
				tok = tok[:i]
			}
			if strings.EqualFold(tok, "websocket") {
				return true
			}
		}
	}
	return false
}

// prepareBody sets the request body up after onRequestHeaders
// (exchange.Request.PrepareBody): a gated body is read whole now, as
// client I/O outside gateway-added time. A declared or read body over
// limits.maxRequestBodyBytes is 413 RZ-RT-003 and a spent buffer budget
// 503 RZ-RT-004; a body that cannot be read (the client went away, its
// read deadline passed, its framing broke) leaves nothing to answer, so
// the request aborts.
func (st *state) prepareBody(ctx context.Context) *filter.Response {
	x := st.x
	gate := st.route.Body.RequestGate
	if gate {
		x.Timer().Enter(st.h.clk.Now())
	}
	err := x.PrepareBody(ctx)
	if gate {
		x.Timer().Leave(st.h.clk.Now())
	}
	if err == nil {
		return nil
	}
	if code := body.Code(err, body.SideRequest); code != "" {
		return st.nodeResponse(code)
	}
	st.abort = true
	return nil
}

// forward runs the Route's Forwarder after the request Phases and makes
// its outcome the client response onResponse runs on; it returns the kind
// of that response.
func (st *state) forward(ctx context.Context, rt *snapshot.Route) executor.ResponseKind {
	x := st.x
	o := x.Outbound()
	o.Trace, o.Deadline, o.Hooks, o.Via = &st.decision, st.deadline, st.run, viaEntry(st.r)
	o.Excluder, o.Reached = &st.up, &st.reached
	if len(st.r.Trailer) > 0 {
		// The client's Connection field named hop-by-hop fields of the
		// whole request (RFC 9110 section 7.6.1), so a trailer field it
		// names is no more forwarded than a header field. The names go
		// before any leg declares the trailer section, and a gated body
		// was read whole already, so net/http adds none of them back that
		// a leg would forward.
		httpfield.StripNamed(st.r.Trailer, st.connOpts)
		o.Trailer = st.r.Trailer
	}
	if _, composed := rt.Forward.(*composition.Composition); st.hints && !composed {
		// Only a Route with upstreams relays 103s: its one leg's hints
		// describe the response the client gets, a composition step's do
		// not.
		o.Hints = st
	}
	u, err := rt.Forward.Forward(ctx, o)
	if u != nil {
		st.upstream, st.endpoint, st.attempts = u.Upstream, u.Endpoint, u.Attempts
	} else {
		st.legOf(err)
	}
	if x.RequestBody().Exceeded() {
		// The streamed body passed its limit while a leg sent it: 413
		// before commit (docs/architecture/03-data-plane.md "Streaming").
		if u != nil && u.Body != nil {
			_ = u.Body.Close()
		}
		x.SetGenerated(st.nodeResponse(codeBodyTooLarge))
		return executor.ResponseGenerated
	}
	if x.RequestBody().Broken() {
		// Reading the streamed body failed while a leg sent it: its
		// framing broke, it ended short of its declared length or its
		// read deadline passed. No Upstream failed and the client's
		// request cannot be read to its end, so nothing is answered:
		// the request aborts, which resets the HTTP/2 stream or closes
		// the HTTP/1.1 connection, as for a gated body that cannot be
		// read (prepareBody).
		if u != nil && u.Body != nil {
			_ = u.Body.Close()
		}
		st.abort = true
		return executor.ResponseGenerated
	}
	if err != nil {
		if st.clientGone() {
			st.abort = true
			return executor.ResponseGenerated
		}
		code, _ := errcode.CodeOf(err)
		if code == "" {
			code = codeFault
		}
		x.SetGenerated(st.nodeResponse(code))
		return kindOf(code)
	}
	x.SetResponse(u)
	if u.Generated != nil {
		return executor.ResponseGenerated
	}
	st.upTrailer = u.Trailer
	if rt.Body.ResponseGate {
		if gerr := x.GateResponse(ctx); gerr != nil {
			code := st.gateCode(ctx, gerr)
			if code == "" {
				st.abort = true
				return executor.ResponseGenerated
			}
			st.upTrailer = nil
			x.SetGenerated(st.nodeResponse(code))
			return kindOf(code)
		}
	}
	return executor.ResponseUpstream
}

// legOf records the Upstream, last Endpoint and attempts a failed leg's
// error names, for the access record.
func (st *state) legOf(err error) {
	var (
		le *upstream.LegError
		ce *composition.Error
	)
	switch {
	case errors.As(err, &le):
		st.upstream, st.endpoint, st.attempts = le.Upstream, le.Endpoint, le.Attempts
	case errors.As(err, &ce):
		st.upstream, st.endpoint, st.attempts = ce.Upstream, ce.Endpoint, ce.Attempts
	}
}

// gateCode maps a failed response gate: over limits.maxResponseBodyBytes
// 502 RZ-UP-010, a spent budget 503 RZ-RT-004, a body a deadline ended
// 504 RZ-UP-003 and any other failed read 502 RZ-UP-004; "" when the
// client went away and nothing is left to answer.
func (st *state) gateCode(ctx context.Context, err error) string {
	if code := body.Code(err, body.SideResponse); code != "" {
		return code
	}
	if st.clientGone() {
		return ""
	}
	if errors.Is(context.Cause(ctx), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return codeUpstreamTimeout
	}
	return codeUpstreamReset
}

// clientGone reports whether the client went away: its request context
// ended without the Route deadline or the ending protocol ending it.
func (st *state) clientGone() bool {
	if st.r.Context().Err() == nil {
		return false
	}
	st.rec.Mu.Lock()
	defer st.rec.Mu.Unlock()
	return st.rec.Ended == snapshot.EndNone
}

// kindOf returns the kind of a Node-generated response with code: an
// Upstream failure for an RZ-UP code, whose response the Response Cache may
// still replace under stale-if-error; generated otherwise.
func kindOf(code string) executor.ResponseKind {
	if strings.HasPrefix(code, upstreamArea) {
		return executor.ResponseUpstreamError
	}
	return executor.ResponseGenerated
}
