// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

// lowSource always draws 2: every bounded draw maps to 0 (the first
// weighted candidate, a zero backoff) without the rejection loop a 0 draw
// can spin in, and least-request ties keep the first candidate.
type lowSource struct{}

func (lowSource) Uint64() uint64 { return 2 }

// counter is a test emit.Counter.
type counter struct{ n atomic.Uint64 }

func (c *counter) Add(_ emit.Stripe, n uint64) { c.n.Add(n) }
func (c *counter) get() uint64                 { return c.n.Load() }

// gauge is a test emit.Gauge.
type gauge struct{ v atomic.Int64 }

func (g *gauge) Add(_ emit.Stripe, d int64) { g.v.Add(d) }
func (g *gauge) Set(v int64)                { g.v.Store(v) }
func (g *gauge) get() int64                 { return g.v.Load() }

// histogram counts records and keeps exemplars.
type histogram struct {
	n  atomic.Int64
	mu sync.Mutex
	ex []emit.Exemplar
}

func (h *histogram) Record(emit.Stripe, uint64) { h.n.Add(1) }

func (h *histogram) RecordExemplar(s emit.Stripe, v uint64, ex emit.Exemplar) {
	h.Record(s, v)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ex = append(h.ex, ex)
}

func (h *histogram) exemplars() []emit.Exemplar {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.ex)
}

// attempts records ruralz_upstream_attempts_total.
type attempts struct {
	mu     sync.Mutex
	events []string
}

func (a *attempts) Inc(_ emit.Stripe, status, attemptErr int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if attemptErr == emit.ErrNone {
		a.events = append(a.events, fmt.Sprint(status))
		return
	}
	a.events = append(a.events, []string{"none", "connect", "timeout", "reset", "tls"}[attemptErr])
}

func (a *attempts) list() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.events)
}

// metrics are inspectable Upstream handles.
type metrics struct {
	m        *emit.UpstreamMetrics
	attempts *attempts
	duration *histogram
	retries  *counter
	budget   *counter
	healthy  *gauge
	breaker  [emit.NumBreakerStates]*gauge
	eject    [emit.NumEjectReasons]*counter
	degraded [emit.NumUpDegraded]*gauge
	cel      [emit.NumCELFields]*counter
	pool     [emit.NumPoolStates]*gauge
}

func newMetrics() *metrics {
	x := &metrics{attempts: &attempts{}, duration: &histogram{}, retries: &counter{}, budget: &counter{}, healthy: &gauge{}}
	x.m = &emit.UpstreamMetrics{
		Attempts:             x.attempts,
		AttemptDuration:      x.duration,
		Retries:              x.retries,
		RetryBudgetExhausted: x.budget,
		HealthyEndpoints:     x.healthy,
		ProbesSkipped:        &counter{},
	}
	for i := range x.breaker {
		x.breaker[i] = &gauge{}
		x.m.BreakerState[i] = x.breaker[i]
	}
	for i := range x.eject {
		x.eject[i] = &counter{}
		x.m.Ejections[i] = x.eject[i]
	}
	for i := range x.degraded {
		x.degraded[i] = &gauge{}
		x.m.Degraded[i] = x.degraded[i]
	}
	for i := range x.cel {
		x.cel[i] = &counter{}
		x.m.CELErrors[i] = x.cel[i]
	}
	for i := range x.pool {
		x.pool[i] = &gauge{}
		x.m.PoolConnections[i] = x.pool[i]
	}
	return x
}

// fakeStatus records Node degraded reasons and cleartext hops.
type fakeStatus struct {
	mu   sync.Mutex
	on   map[string]bool
	hops map[catalog.Hop]int
}

func newStatus() *fakeStatus {
	return &fakeStatus{on: map[string]bool{}, hops: map[catalog.Hop]int{}}
}

func (s *fakeStatus) SetDegraded(r catalog.Reason, source string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.on[r.String()+"/"+source] = on
}

func (s *fakeStatus) SetCleartextHops(h catalog.Hop, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hops[h] = n
}

func (s *fakeStatus) held(r catalog.Reason, source string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.on[r.String()+"/"+source]
}

func (s *fakeStatus) hopCount(h catalog.Hop) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hops[h]
}

// spanRecord is one ended upstream span.
type spanRecord struct {
	attrs   emit.UpstreamAttrs
	status  int
	errType string
}

// fakeTracer records upstream spans and injects a fixed traceparent.
type fakeTracer struct {
	nopTracer
	mu    sync.Mutex
	spans []spanRecord
}

// injected is the traceparent fakeTracer writes.
const injected = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

func (t *fakeTracer) StartUpstream(ctx context.Context, _ string, a emit.UpstreamAttrs) (context.Context, emit.Span) {
	sp := &fakeSpan{t: t, attrs: a}
	return context.WithValue(ctx, spanKey{}, sp), sp
}

func (*fakeTracer) ContextWithSpan(ctx context.Context, s emit.Span) context.Context {
	return context.WithValue(ctx, spanKey{}, s)
}

// spanKey is the context key of the span a fakeTracer context carries.
type spanKey struct{}

// spanUnder returns the attempt of the span ctx carries ("-" for none) and
// whether that span is still open.
func spanUnder(ctx context.Context) string {
	sp, ok := ctx.Value(spanKey{}).(*fakeSpan)
	if !ok {
		return "-"
	}
	if sp.ended.Load() {
		return strconv.Itoa(sp.attrs.Attempt) + " ended"
	}
	return strconv.Itoa(sp.attrs.Attempt)
}

func (*fakeTracer) Inject(_ context.Context, _ *emit.Decision, h http.Header) {
	h["Traceparent"] = []string{injected}
}

func (t *fakeTracer) list() []spanRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.spans)
}

// fakeSpan records its End.
type fakeSpan struct {
	t     *fakeTracer
	attrs emit.UpstreamAttrs
	ended atomic.Bool
}

func (*fakeSpan) SetAttr(string, slog.Value) {}
func (*fakeSpan) SpanID() [8]byte            { return [8]byte{1} }

func (s *fakeSpan) End(status int, _, errType string) {
	if s.ended.Swap(true) {
		panic("span ended twice")
	}
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	s.t.spans = append(s.t.spans, spanRecord{attrs: s.attrs, status: status, errType: errType})
}

// hooks is a scripted snapshot.LegHooks recording every call.
type hooks struct {
	mu     sync.Mutex
	events []string
	// legs are copies of the attempts onUpstreamRequest saw.
	legs []filter.Leg
	// under records each Phase call as "<phase> <leg attempt> under <span
	// attempt>", the attempt of the span its context carries.
	under []string
	// onRequest, onHeaders and onBody script the Phases; nil continues.
	onRequest func(leg *filter.Leg) *filter.Response
	onHeaders func(leg *filter.Leg) (bool, *filter.Response)
	onBody    func(leg *filter.Leg) *filter.Response
}

func (h *hooks) record(format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, fmt.Sprintf(format, args...))
}

func (h *hooks) list() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

func (h *hooks) recordUnder(ctx context.Context, ph string, attempt int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.under = append(h.under, fmt.Sprintf("%s %d under %s", ph, attempt, spanUnder(ctx)))
}

func (h *hooks) listUnder() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.under)
}

func (h *hooks) BeginLeg(_ context.Context, upstream, step string) snapshot.LegRun {
	h.record("begin %s %q", upstream, step)
	return &legRun{h: h}
}

// legRun is the LegRun of hooks.
type legRun struct {
	h     *hooks
	ended bool
}

func (r *legRun) OnUpstreamRequest(ctx context.Context, leg *filter.Leg) *filter.Response {
	r.h.record("request %d %s", leg.Attempt, leg.Endpoint)
	r.h.recordUnder(ctx, "request", leg.Attempt)
	r.h.mu.Lock()
	r.h.legs = append(r.h.legs, *leg)
	r.h.mu.Unlock()
	if r.h.onRequest != nil {
		return r.h.onRequest(leg)
	}
	return nil
}

func (r *legRun) OnUpstreamResponseHeaders(ctx context.Context, leg *filter.Leg) (bool, *filter.Response) {
	r.h.recordUnder(ctx, "headers", leg.Attempt)
	if leg.Response == nil {
		r.h.record("headers %d error %s", leg.Attempt, leg.ErrorKind)
	} else {
		r.h.record("headers %d %d", leg.Attempt, leg.Response.StatusCode)
	}
	if r.h.onHeaders != nil {
		return r.h.onHeaders(leg)
	}
	return false, nil
}

// OnUpstreamResponseBody records the span its context carries once
// onBody, which may read the body to its end, has run.
func (r *legRun) OnUpstreamResponseBody(ctx context.Context, leg *filter.Leg) *filter.Response {
	r.h.record("body %d", leg.Response.StatusCode)
	var resp *filter.Response
	if r.h.onBody != nil {
		resp = r.h.onBody(leg)
	}
	r.h.recordUnder(ctx, "body", leg.Attempt)
	return resp
}

func (r *legRun) End(context.Context) {
	if r.ended {
		panic("leg ended twice")
	}
	r.ended = true
	r.h.record("end")
}

// secretStore is a static secret.Store.
type secretStore map[secret.Ref][]byte

func (s secretStore) Get(r secret.Ref) (secret.Value, bool) {
	b, ok := s[r]
	if !ok {
		return secret.Value{}, false
	}
	return secret.NewValue(b), true
}

func (secretStore) Watch(secret.Ref, func(secret.Value) error) func() { return func() {} }

// caRef is the secret reference of a test CA bundle.
func caRef() secret.Ref {
	return secret.Ref{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/ca.pem"}
}

// caStore returns a store holding the certificate of ts as the CA bundle.
func caStore(ts *httptest.Server) secretStore {
	return secretStore{caRef(): pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw})}
}

// failingDial refuses the addresses in refused and dials the others; it
// counts dials per address.
type failingDial struct {
	mu      sync.Mutex
	refused map[string]bool
	dials   map[string]int
}

func newDial(refused ...string) *failingDial {
	d := &failingDial{refused: map[string]bool{}, dials: map[string]int{}}
	for _, a := range refused {
		d.refused[a] = true
	}
	return d
}

func (d *failingDial) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.dials[address]++
	refused := d.refused[address]
	d.mu.Unlock()
	if refused {
		return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, address)
}

func (d *failingDial) count(address string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials[address]
}

// fixture is a Manager on a fake clock with recording telemetry.
type fixture struct {
	t      *testing.T
	clk    *clocktest.Fake
	status *fakeStatus
	tracer *fakeTracer
	dial   *failingDial
	m      *Manager
}

// newFixture returns a Manager on a fake clock whose dials go through
// newDial(refused...): loopback is allowed, the refused addresses fail as
// connection refused.
func newFixture(t *testing.T, refused ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, clk: clocktest.New(epoch()), status: newStatus(), tracer: &fakeTracer{}, dial: newDial(refused...)}
	f.m = NewManager(Options{Clock: f.clk, Rand: lowSource{}, Dial: f.dial.dial, Status: f.status, Tracer: f.tracer})
	return f
}

// compile compiles and activates the configs; the Set closes at cleanup.
func (f *fixture) compile(secrets secret.Store, cfgs ...Config) *Set {
	f.t.Helper()
	s, err := f.m.Compile(f.t.Context(), Input{Upstreams: cfgs, Secrets: secrets})
	if err != nil {
		f.t.Fatalf("Compile: %v", err)
	}
	f.t.Cleanup(func() { _ = s.Close() })
	s.Activate()
	return s
}

// upstream returns the compiled Upstream name of s.
func (f *fixture) upstream(s *Set, name string) *Upstream {
	f.t.Helper()
	u, ok := s.Upstream(name)
	if !ok {
		f.t.Fatalf("Upstream %q not compiled", name)
	}
	return u
}

// newUpstream returns an http Upstream with static Endpoints.
func newUpstream(name string, addrs ...string) *v1alpha1.Upstream {
	u := &v1alpha1.Upstream{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.UpstreamSpec{Protocol: v1alpha1.UpstreamProtocolHTTP},
	}
	for _, a := range addrs {
		u.Spec.Endpoints = append(u.Spec.Endpoints, v1alpha1.Endpoint{Address: a})
	}
	return u
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// dur returns a configuration duration.
func dur(d time.Duration) *v1alpha1.Duration { return ptr(v1alpha1.Duration(d)) }

// server is a loopback Upstream that records requests.
type server struct {
	*httptest.Server
	addr string
	mu   sync.Mutex
	reqs []*http.Request
	hits atomic.Int64
}

// newServer starts a loopback server running h after recording the
// request.
func newServer(t testing.TB, h http.HandlerFunc) *server {
	t.Helper()
	return newServerWith(t, h, nil)
}

// newServerWith is newServer with mod applied to the http.Server before it
// starts.
func newServerWith(t testing.TB, h http.HandlerFunc, mod func(*http.Server)) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		h(w, r)
	}))
	if mod != nil {
		mod(s.Config)
	}
	s.Start()
	t.Cleanup(s.Close)
	s.addr = s.Listener.Addr().String()
	return s
}

// newTLSServer starts a loopback TLS server with HTTP/2 enabled.
func newTLSServer(t *testing.T, h http.HandlerFunc) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		h(w, r)
	}))
	s.EnableHTTP2 = true
	// Refused handshakes are expected; keep them out of the test output.
	s.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	s.StartTLS()
	t.Cleanup(s.Close)
	s.addr = s.Listener.Addr().String()
	return s
}

func (s *server) record(r *http.Request) {
	s.hits.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r.Clone(r.Context()))
}

func (s *server) last() *http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reqs) == 0 {
		return nil
	}
	return s.reqs[len(s.reqs)-1]
}

// status returns a handler answering code with body.
func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

// exchange returns a client exchange for method and path from peer
// 203.0.113.7, untrusted.
func exchange(method, path string) *filtertest.Exchange {
	return &filtertest.Exchange{
		MethodV: method,
		SchemeV: "https",
		HostV:   "api.shop.example",
		PathV:   path,
		Hdr:     http.Header{"Accept": {"application/json"}, "User-Agent": {"client/1.0"}},
		SourceV: filter.Source{IP: netip.MustParseAddr("203.0.113.7"), Peer: netip.MustParseAddrPort("203.0.113.7:51000")},
		Msg:     &filtertest.Message{},
		VarsV:   &expr.Vars{Request: &expr.Request{Method: method, Path: path}},
	}
}

// outbound returns an Outbound for x with an empty body and a deadline 15 s
// after the fake clock's start.
func outbound(x filter.Exchange, h snapshot.LegHooks) *snapshot.Outbound {
	return &snapshot.Outbound{X: x, Body: emptyBody(), Deadline: epoch().Add(15 * time.Second), Hooks: h}
}

// emptyBody is an empty request body.
func emptyBody() *body.Request { return &body.Request{} }

// gatedBody returns a gated (replayable) request body holding b.
func gatedBody(t *testing.T, b string) *body.Request {
	t.Helper()
	q := &body.Request{}
	bud := body.NewBudget(1<<20, &gauge{})
	if err := q.Prepare(t.Context(), strings.NewReader(b), int64(len(b)), true, 1<<20, bud, 0); err != nil {
		t.Fatal(err)
	}
	return q
}

// streamBody returns a streamed request body holding b.
func streamBody(b string) *body.Request {
	q := &body.Request{}
	q.SetStream(strings.NewReader(b), int64(len(b)), 1<<20)
	return q
}

// readAll reads and closes a leg response body.
func readAll(t *testing.T, r *snapshot.UpstreamResponse) string {
	t.Helper()
	if r.Body == nil {
		return ""
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := r.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	return string(b)
}

// codeOf returns the RZ code of a leg error.
func codeOf(err error) string {
	c, _ := errcode.CodeOf(err)
	return c
}

// waitFor polls cond with a real-time bound; it only waits for goroutines
// the test started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}
