// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/exchange"
	"github.com/ravindu-rev/ruralz/internal/gateway/executor"
	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/tap"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Bounds of docs/architecture/03-data-plane.md "Bounded resources"
// (targets).
const (
	// AdmissionWindow is the body read and response write deadline a
	// request gets at admission, until its Route timeout replaces it.
	AdmissionWindow = 10 * time.Second
	// MaxFieldLines caps the field lines of a request header block,
	// pseudo-header fields excluded and a joined Cookie counted once
	// (431 RZ-RT-002).
	MaxFieldLines = 256
	// CopyBufferBytes is the buffer an unsubscribed response body is
	// copied through.
	CopyBufferBytes = 32 << 10
)

// ErrDeps reports a Deps without a required dependency.
var ErrDeps = errors.New("handler: missing dependency")

// Conn is what the handler needs from the connection a request arrived on;
// *listener.Conn implements it.
type Conn interface {
	// Listener returns the name of the listener that accepted it.
	Listener() string
	// Stripe returns its metric and pin stripe.
	Stripe() emit.Stripe
	// Peer returns the connection peer after PROXY v2 handling.
	Peer() netip.AddrPort
	// Cache returns the per-connection memo of auth.mtls.
	Cache() *filter.ConnCache
	// ClientCertRequested reports whether the handshake requested a client
	// certificate.
	ClientCertRequested() bool
	// Metrics returns the listener handles of the snapshot published at
	// accept, for a request answered before any snapshot is pinned.
	Metrics() *emit.ListenerMetrics
	// NextRequest numbers the connection's requests from 0.
	NextRequest() uint32
}

var _ Conn = (*listener.Conn)(nil)

// Deps are the Node-wide dependencies of the request handler.
type Deps struct {
	// Snapshots publishes the snapshot every request pins; required.
	Snapshots *retire.Holder
	// Units is the Node in-flight unit ceiling; required.
	Units *admission.Units
	// PreRouting bounds the pre-routing responses written at once;
	// required.
	PreRouting *admission.PreRouting
	// Exchanges pools the per-request state Filters see; required.
	Exchanges *exchange.Pool
	// Executor runs the Filter Chains; required.
	Executor *executor.Executor
	// Tracer makes each request's sampling decision, whose trace ID is the
	// requestId of every problem document, and starts the server and
	// ruralz.route.match spans; required.
	Tracer emit.Tracer
	// AccessLog receives the access records; nil writes none.
	AccessLog emit.AccessLog
	// Node holds the Node-wide metric handles; nil records none.
	Node *emit.NodeMetrics
	// Tap receives the finished exchanges /tap subscribers sample; nil
	// publishes none.
	Tap *tap.Hub
	// Clock times requests and sets the connection deadlines, so it must
	// track wall time; nil means clock.Real().
	Clock clock.Clock
	// Logger records recovered faults at error, with their stack; it comes
	// from internal/telemetry. nil discards.
	Logger *slog.Logger
	// Conn returns the connection of a request; nil means
	// listener.ConnFrom.
	Conn func(ctx context.Context) Conn
}

// Handler is the request handler of every client listener
// (docs/architecture/03-data-plane.md): the http.Handler the listener
// Set's Server shards share. It is safe for concurrent use.
type Handler struct {
	snaps  *retire.Holder
	units  *admission.Units
	pre    *admission.PreRouting
	pool   *exchange.Pool
	exec   *executor.Executor
	tracer emit.Tracer
	alog   emit.AccessLog
	node   *emit.NodeMetrics
	tap    *tap.Hub
	clk    clock.Clock
	log    *slog.Logger
	connOf func(ctx context.Context) Conn

	states sync.Pool // *state
	view   atomic.Pointer[snapView]
	date   dateCache
	noop   noopHandles
	// statuses is the registered status of each code with a single one
	// (statusTable), read-only after New.
	statuses map[string]int
}

// New returns the Handler over d.
func New(d Deps) (*Handler, error) {
	switch {
	case d.Snapshots == nil:
		return nil, errors.Join(ErrDeps, errors.New("handler: Snapshots is required"))
	case d.Units == nil:
		return nil, errors.Join(ErrDeps, errors.New("handler: Units is required"))
	case d.PreRouting == nil:
		return nil, errors.Join(ErrDeps, errors.New("handler: PreRouting is required"))
	case d.Exchanges == nil:
		return nil, errors.Join(ErrDeps, errors.New("handler: Exchanges is required"))
	case d.Executor == nil:
		return nil, errors.Join(ErrDeps, errors.New("handler: Executor is required"))
	case d.Tracer == nil:
		return nil, errors.Join(ErrDeps, errors.New("handler: Tracer is required"))
	}
	h := &Handler{
		snaps: d.Snapshots, units: d.Units, pre: d.PreRouting, pool: d.Exchanges, exec: d.Executor,
		tracer: d.Tracer, alog: d.AccessLog, node: d.Node, tap: d.Tap, clk: d.Clock, log: d.Logger, connOf: d.Conn,
		statuses: statusTable(),
	}
	if h.clk == nil {
		h.clk = clock.Real()
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.connOf == nil {
		h.connOf = connFrom
	}
	h.noop.init()
	if h.node == nil {
		h.node = &h.noop.node
	}
	h.states.New = func() any { return newState(h) }
	return h, nil
}

// connFrom is the default Conn lookup: the listener Set's record, or nil
// for a request a Set did not serve.
func connFrom(ctx context.Context) Conn {
	if c := listener.ConnFrom(ctx); c != nil {
		return c
	}
	return nil
}

// ServeHTTP handles one client request: the in-flight unit first, then the
// pinned snapshot, request hardening, the Router, the Filter Chain, the
// Upstream layer and the response, then onLog, the access record, /tap and
// the request metrics (package documentation).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := h.clk.Now()
	conn := h.connOf(r.Context())
	if !h.units.TryAcquire(1) {
		h.overload(w, r, conn)
		return
	}
	defer h.units.Release(1)
	st, _ := h.states.Get().(*state)
	defer h.putState(st)
	st.serve(w, r, conn, start)
}

// putState resets st and returns it to the pool.
func (h *Handler) putState(st *state) {
	st.reset()
	h.states.Put(st)
}

// overload answers a request the full in-flight ceiling refused: 503
// RZ-RT-005 at once, never queued, through the pre-routing limiter, with
// no snapshot pinned, no access record and no /tap event, so overload adds
// no log work; it still counts in the listener and Node metrics, and under
// route _unmatched (docs/architecture/10-observability.md "Access logs").
func (h *Handler) overload(w http.ResponseWriter, r *http.Request, conn Conn) {
	var d emit.Decision
	h.tracer.Decide(r.Header, 0, &d)
	rc := http.NewResponseController(w)
	h.pre.Do(rc, func() { h.writeBareProblem(w, r, codeFull, &d) })
	h.countBare(r, conn, codeFull)
}

// countBare counts a response written without a pinned snapshot in the
// listener metrics of the connection, the Node responses and, through the
// active snapshot's plan, under route _unmatched, whose handles live as
// long as the process.
func (h *Handler) countBare(r *http.Request, conn Conn, code string) {
	status := h.statusOf(code)
	s := emit.Stripe(0)
	lm := &h.noop.listener
	if conn != nil {
		s = conn.Stripe()
		if m := conn.Metrics(); m != nil {
			lm = m
		}
	}
	lm.Requests.Inc(s, protoIndex(r), status, emit.OriginOf(code, false))
	h.node.NodeResponses.Inc(s, code)
	if snap := h.snaps.Active(); snap != nil && snap.Metrics != nil {
		if rm := snap.Metrics.Route(unmatchedRoute); rm != nil {
			rm.Requests.Inc(s, status)
		}
	}
}
