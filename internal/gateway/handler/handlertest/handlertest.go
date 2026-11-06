// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package handlertest is test support for the request handler
// (internal/gateway/handler) and the suites that drive it: snapshot
// fixtures with compiled Routers and Filter Chains, canned Forwarders, a
// deterministic Tracer, a recording access log, recording metric handles
// and a fake connection. It never imports the handler, so the handler's
// own tests use it as well as the ruralzd wiring, conformance and
// benchmark suites. Production code never imports it.
package handlertest

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/router"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ListenerName is the name of the listener a Spec without listeners gets.
const ListenerName = "public"

// DefaultTimeout is the Route timeout of a Route without one.
const DefaultTimeout = 15 * time.Second

// Route is one Route of a test snapshot.
type Route struct {
	// Name is metadata.name.
	Name string
	// Match is Route.spec.match; nil matches every request.
	Match *v1alpha1.RouteMatch
	// When is the compiled match.when, set with Match.When.
	When expr.Program
	// Listeners are the bound listeners; empty binds every listener.
	Listeners []string
	// Forward answers the requests that reach the Upstream layer.
	Forward snapshot.Forwarder
	// Policies are the client Policies in request order (class, scope,
	// position), each with its Phases.
	Policies []*snapshot.Policy
	// Legs are the upstream-leg Policies by Upstream name.
	Legs map[string][]*snapshot.Policy
	// Timeout is the Route timeout; 0 means DefaultTimeout.
	Timeout time.Duration
	// Body are the compile-time body decisions.
	Body snapshot.BodyNeeds
	// Protocol is the Upstream protocol; "" means http.
	Protocol v1alpha1.UpstreamProtocol
	// Metrics are the Route's handles; nil records nothing.
	Metrics *emit.RouteMetrics
}

// Spec describes a test snapshot.
type Spec struct {
	// Listeners are the client listeners; empty means one http listener
	// named ListenerName.
	Listeners []*snapshot.Listener
	// Routes are the Routes, indexed in order.
	Routes []Route
	// Limits are the effective limits; zero fields take DefaultLimits'.
	Limits snapshot.Limits
	// TrustedProxies is Gateway.spec.trustedProxies.
	TrustedProxies []netip.Prefix
	// TraceSampling is the root sampling ratio.
	TraceSampling float64
	// AccessLogWhen is the compiled accessLog.when; nil selects every
	// request.
	AccessLogWhen expr.Program
	// Canonical is the Revision's canonical form, whose digest the
	// snapshot carries; empty gives a fixed one.
	Canonical string
	// Metrics is the snapshot's admission plan; nil records nothing.
	Metrics emit.Plan
	// StateStore is the main State Store handle; nil has none.
	StateStore snapshot.StoreHandle
	// Validated is the pipeline output the snapshot was compiled from,
	// whose Bundle names the auth.api-key fields /tap redacts; nil has
	// none.
	Validated *hub.Validated
	// Legs runs replayed legs to named Upstreams; nil has none.
	Legs snapshot.LegRunner
}

// DefaultLimits returns the limits of a Spec that sets none.
func DefaultLimits() snapshot.Limits {
	return snapshot.Limits{
		MaxRequestBodyBytes: 1 << 20, MaxResponseBodyBytes: 1 << 20, MaxBufferedBytes: 64 << 20,
		MaxCompositionSteps: 16, MaxRequestHeaderBytes: 64 << 10,
	}
}

// HTTPListener returns an http listener.
func HTTPListener(name string) *snapshot.Listener {
	return &snapshot.Listener{Name: name, Protocol: v1alpha1.ListenerProtocolHTTP, Port: 8080}
}

// Snapshot builds the snapshot s describes, with a compiled Router per
// listener and fresh pins; it fails the test when a Router does not
// compile.
func Snapshot(t testing.TB, s Spec) *snapshot.Snapshot {
	t.Helper()
	lim := DefaultLimits()
	if s.Limits.MaxRequestBodyBytes != 0 {
		lim.MaxRequestBodyBytes = s.Limits.MaxRequestBodyBytes
	}
	if s.Limits.MaxResponseBodyBytes != 0 {
		lim.MaxResponseBodyBytes = s.Limits.MaxResponseBodyBytes
	}
	if s.Limits.MaxBufferedBytes != 0 {
		lim.MaxBufferedBytes = s.Limits.MaxBufferedBytes
	}
	if s.Limits.MaxCompositionSteps != 0 {
		lim.MaxCompositionSteps = s.Limits.MaxCompositionSteps
	}
	if s.Limits.MaxRequestHeaderBytes != 0 {
		lim.MaxRequestHeaderBytes = s.Limits.MaxRequestHeaderBytes
	}
	listeners := s.Listeners
	if len(listeners) == 0 {
		listeners = []*snapshot.Listener{HTTPListener(ListenerName)}
	}
	canonical := s.Canonical
	if canonical == "" {
		canonical = `{"apiVersion":"ruralz/v1alpha1"}`
	}
	snap := &snapshot.Snapshot{
		Revision:  revision.Revision{Digest: revision.Sum([]byte(canonical)), Content: []byte(canonical)},
		Gateway:   snapshot.Gateway{AdminPort: 9901, TrustedProxies: s.TrustedProxies, Limits: lim, TraceSampling: s.TraceSampling, AccessLogWhen: s.AccessLogWhen},
		Listeners: listeners,
		Routers:   map[string]snapshot.Router{},
		Metrics:   s.Metrics,
		LegChains: map[string]*snapshot.LegChain{},
		Pins:      snapshot.NewPins(1),
	}
	if s.StateStore != nil {
		snap.StateStore = s.StateStore
	}
	snap.Validated, snap.Legs = s.Validated, s.Legs
	bound := map[string][]router.Route{}
	for i := range s.Routes {
		r := &s.Routes[i]
		rt := &snapshot.Route{
			Index: int32(i), Name: r.Name, View: &expr.Route{Name: r.Name}, Listeners: r.Listeners,
			Protocol: r.Protocol, Timeout: r.Timeout, Forward: r.Forward, Body: r.Body, Metrics: r.Metrics,
		}
		if rt.Protocol == "" {
			rt.Protocol = v1alpha1.UpstreamProtocolHTTP
		}
		if rt.Timeout == 0 {
			rt.Timeout = DefaultTimeout
		}
		rt.Chain = *Chain(r.Policies, r.Legs)
		for name, lc := range rt.Chain.Legs {
			snap.LegChains[name] = lc
		}
		snap.Routes = append(snap.Routes, rt)
		names := r.Listeners
		if len(names) == 0 {
			for _, l := range listeners {
				names = append(names, l.Name)
			}
		}
		for _, n := range names {
			bound[n] = append(bound[n], router.Route{Index: int32(i), Name: r.Name, Match: r.Match, When: r.When})
		}
	}
	for _, l := range listeners {
		rr, err := router.Compile(l.Hostnames, bound[l.Name])
		if err != nil {
			t.Fatalf("handlertest: compile the Router of %s: %v", l.Name, err)
		}
		snap.Routers[l.Name] = rr
	}
	return snap
}

// Chain compiles client Policies, given in request order, and leg Policies
// by Upstream: each Phase lists its subscribers, response Phases in
// reverse; Index and Position follow the order given.
func Chain(ps []*snapshot.Policy, legs map[string][]*snapshot.Policy) *snapshot.Chain {
	c := &snapshot.Chain{Client: lists(ps), Policies: ps, Legs: map[string]*snapshot.LegChain{}}
	idx := 0
	for i, p := range ps {
		p.Index, p.Position = idx, i
		idx++
		if p.Class == phase.ClassAuth {
			c.AuthPolicies++
		}
	}
	names := make([]string, 0, len(legs))
	for up := range legs {
		names = append(names, up)
	}
	slices.Sort(names)
	for _, up := range names {
		for i, p := range legs[up] {
			p.Index, p.Position = idx, i
			idx++
		}
		c.Legs[up] = &snapshot.LegChain{Upstream: up, Phases: lists(legs[up])}
	}
	return c
}

// lists places ps, in request order, into per-Phase lists.
func lists(ps []*snapshot.Policy) [phase.Count][]*snapshot.Policy {
	var out [phase.Count][]*snapshot.Policy
	for ph := range phase.Count {
		for _, p := range ps {
			if p.Phases.Has(ph) {
				out[ph] = append(out[ph], p)
			}
		}
		if ph.IsResponse() {
			slices.Reverse(out[ph])
		}
	}
	return out
}

// Policy returns a compiled Policy of class running f in phases, at Route
// scope, closed, of type headers unless typ is set.
func Policy(name string, class phase.Class, typ v1alpha1.PolicyType, f filter.Filter, phases ...phase.Phase) *snapshot.Policy {
	if typ == "" {
		typ = v1alpha1.PolicyTypeHeaders
	}
	set := phase.Of(phases...)
	first, _ := set.First()
	p := &snapshot.Policy{
		Name: name, Type: typ, Class: class, Scope: phase.ScopeRoute, FailureMode: v1alpha1.FailureModeClosed,
		FirstPhase: first, Phases: set, Filter: f, SpanName: catalog.FilterSpanName(name),
		StateStoreTimeout: 20 * time.Millisecond,
	}
	if c, ok := f.(filter.Consumptive); ok {
		p.Consumptive = c
	}
	return p
}

// FilterFunc adapts a function to filter.Filter.
type FilterFunc func(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result

// Handle calls f.
func (f FilterFunc) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	return f(ctx, p, x)
}

// ForwardFunc adapts a function to snapshot.Forwarder.
type ForwardFunc func(ctx context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error)

// Forward calls f.
func (f ForwardFunc) Forward(ctx context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
	return f(ctx, o)
}

// Respond returns a Forwarder that answers every request with status,
// header and body from the named Upstream, running no leg Phase.
func Respond(upstream string, status int, header http.Header, body string) snapshot.Forwarder {
	return ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		return &snapshot.UpstreamResponse{
			Status: status, Header: header.Clone(), Body: io.NopCloser(strings.NewReader(body)),
			Upstream: upstream, Endpoint: "127.0.0.1:9", Attempts: 1,
		}, nil
	})
}

// Conn is a fake connection of the handler.
type Conn struct {
	// Name is the listener name and Stripe the connection's stripe.
	Name    string
	StripeV emit.Stripe
	// PeerV is the connection peer.
	PeerV netip.AddrPort
	// CertRequested reports a handshake that requested a client
	// certificate.
	CertRequested bool
	// MetricsV are the listener handles at accept.
	MetricsV *emit.ListenerMetrics

	seq   atomic.Uint32
	cache filter.ConnCache
}

// NewConn returns a connection on listener name from peer.
func NewConn(name string, peer string) *Conn {
	return &Conn{Name: name, PeerV: netip.MustParseAddrPort(peer)}
}

// Listener returns the listener name.
func (c *Conn) Listener() string { return c.Name }

// Stripe returns the connection's stripe.
func (c *Conn) Stripe() emit.Stripe { return c.StripeV }

// Peer returns the connection peer.
func (c *Conn) Peer() netip.AddrPort { return c.PeerV }

// Cache returns the connection's memo.
func (c *Conn) Cache() *filter.ConnCache { return &c.cache }

// ClientCertRequested reports CertRequested.
func (c *Conn) ClientCertRequested() bool { return c.CertRequested }

// Metrics returns MetricsV.
func (c *Conn) Metrics() *emit.ListenerMetrics { return c.MetricsV }

// NextRequest numbers the connection's requests from 0.
func (c *Conn) NextRequest() uint32 { return c.seq.Add(1) - 1 }

// Tracer is a deterministic emit.Tracer: a valid traceparent is honored
// with its sampled flag, any other request gets the next trace ID of a
// counter, sampled when the ratio is 1 or more; it records every span.
type Tracer struct {
	next  atomic.Uint64
	mu    sync.Mutex
	spans []*Span
}

// Decide implements emit.Tracer.
func (t *Tracer) Decide(h http.Header, ratio float64, d *emit.Decision) {
	*d = emit.Decision{}
	n := t.next.Add(1)
	binary.BigEndian.PutUint64(d.ServerSpanID[:], n)
	if vs := h["Traceparent"]; len(vs) == 1 {
		if tid, sampled, ok := parseTraceparent(vs[0]); ok {
			d.TraceID, d.Remote, d.Sampled = tid, true, sampled
			return
		}
	}
	binary.BigEndian.PutUint64(d.TraceID[8:], n)
	d.Sampled = ratio >= 1
}

// parseTraceparent reads a version 00 traceparent.
func parseTraceparent(v string) (tid [16]byte, sampled, ok bool) {
	parts := strings.Split(v, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return tid, false, false
	}
	if _, err := hex.Decode(tid[:], []byte(parts[1])); err != nil || tid == [16]byte{} {
		return tid, false, false
	}
	return tid, parts[3] == "01", true
}

// StartServer implements emit.Tracer.
func (t *Tracer) StartServer(ctx context.Context, d *emit.Decision, method string, a emit.ServerAttrs) (context.Context, emit.Span) {
	return ctx, t.add(&Span{Kind: "server", Name: method, Server: a, Sampled: d.Sampled})
}

// StartRouteMatch implements emit.Tracer.
func (t *Tracer) StartRouteMatch(ctx context.Context) (context.Context, emit.Span) {
	return ctx, t.add(&Span{Kind: "match", Name: catalog.SpanRouteMatch})
}

// StartFilter implements emit.Tracer.
func (t *Tracer) StartFilter(ctx context.Context, name string, _ emit.FilterAttrs) (context.Context, emit.Span) {
	return ctx, t.add(&Span{Kind: "filter", Name: name})
}

// StartUpstream implements emit.Tracer.
func (t *Tracer) StartUpstream(ctx context.Context, name string, _ emit.UpstreamAttrs) (context.Context, emit.Span) {
	return ctx, t.add(&Span{Kind: "upstream", Name: name})
}

// ContextWithSpan implements emit.Tracer: spans record no parent, so ctx
// is returned.
func (*Tracer) ContextWithSpan(ctx context.Context, _ emit.Span) context.Context { return ctx }

// Inject implements emit.Tracer: it writes the decision's traceparent.
func (*Tracer) Inject(_ context.Context, d *emit.Decision, h http.Header) {
	flags := "00"
	if d.Sampled {
		flags = "01"
	}
	h["Traceparent"] = []string{"00-" + hex.EncodeToString(d.TraceID[:]) + "-" + hex.EncodeToString(d.ServerSpanID[:]) + "-" + flags}
}

func (t *Tracer) add(s *Span) *Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	s.Attrs = map[string]string{}
	t.spans = append(t.spans, s)
	return s
}

// Spans returns the spans started so far, of kind when it is not empty.
func (t *Tracer) Spans(kind string) []*Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*Span
	for _, s := range t.spans {
		if kind == "" || s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// Span is one recorded span.
type Span struct {
	Kind, Name string
	Server     emit.ServerAttrs
	Sampled    bool

	mu     sync.Mutex
	Attrs  map[string]string
	Status int
	Code   string
	Ended  bool
}

// SetAttr implements emit.Span.
func (s *Span) SetAttr(key string, v slog.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Attrs[key] = v.String()
}

// SpanID implements emit.Span.
func (*Span) SpanID() [8]byte { return [8]byte{1} }

// End implements emit.Span; a second End fails loudly.
func (s *Span) End(status int, code, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Ended {
		panic("handlertest: span ended twice")
	}
	s.Status, s.Code, s.Ended = status, code, true
}

// Attr returns the attribute key.
func (s *Span) Attr(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Attrs[key]
}

// Done reports whether the span ended, with its status and code.
func (s *Span) Done() (ended bool, status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Ended, s.Status, s.Code
}

// AccessLog is a recording emit.AccessLog: Submit keeps a copy of each
// record, failure modes included.
type AccessLog struct {
	mu   sync.Mutex
	recs []emit.AccessRecord
}

// Acquire implements emit.AccessLog.
func (*AccessLog) Acquire() *emit.AccessRecord { return &emit.AccessRecord{} }

// Submit implements emit.AccessLog.
func (l *AccessLog) Submit(r *emit.AccessRecord) {
	c := *r
	c.FailureModes = slices.Clone(r.FailureModes)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, c)
}

// Records returns the submitted records.
func (l *AccessLog) Records() []emit.AccessRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.recs)
}
