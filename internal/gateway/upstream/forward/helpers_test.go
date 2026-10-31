// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"context"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
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
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

// pcg returns a deterministic source.
func pcg(seed uint64) *rand.PCG { return rand.NewPCG(seed, seed^0x9e3779b97f4a7c15) }

// lowSource always draws 2: every bounded draw maps to 0 (the first
// weighted candidate, a zero backoff) without the rejection loop a 0 draw
// can spin in.
type lowSource struct{}

func (lowSource) Uint64() uint64 { return 2 }

// noDraw fails the test when a pick draws, for picks that need no
// randomness.
type noDraw struct{ t *testing.T }

func (s noDraw) Uint64() uint64 {
	s.t.Helper()
	s.t.Error("the pick drew a random value")
	return 0
}

// refusal returns the leg error of a leg gate g refused before any
// attempt, as upstream.Upstream.Leg returns it.
func refusal(name string, g resilience.Gate) *upstream.LegError {
	return &upstream.LegError{Upstream: name, Err: g.Err()}
}

// fakeLeg is a scripted leg: it returns resp or err, and counts its calls
// in its own counter and in the shared call log, when set.
type fakeLeg struct {
	name  string
	resp  *snapshot.UpstreamResponse
	err   error
	calls atomic.Int64
	log   *callLog
}

func (l *fakeLeg) Leg(context.Context, *snapshot.Outbound, upstream.Target) (*snapshot.UpstreamResponse, error) {
	l.calls.Add(1)
	if l.log != nil {
		l.log.add(l.name)
	}
	if l.err != nil {
		return nil, l.err
	}
	return l.resp, nil
}

// callLog records the order legs ran in.
type callLog struct {
	mu    sync.Mutex
	names []string
}

func (c *callLog) add(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, name)
}

func (c *callLog) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.names)
}

func (c *callLog) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = c.names[:0]
}

// serving returns a fake leg answering 200 for name.
func serving(name string) *fakeLeg {
	return &fakeLeg{name: name, resp: &snapshot.UpstreamResponse{Status: http.StatusOK, Upstream: name, Attempts: 1}}
}

// refusing returns a fake leg refused by g before any attempt.
func refusing(name string, g resilience.Gate) *fakeLeg {
	return &fakeLeg{name: name, err: refusal(name, g)}
}

// forwarder returns a Forwarder over fake legs with weights.
func forwarder(legs []*fakeLeg, weights []uint32, src rand.Source) *Forwarder {
	lr := make([]upstreamLeg, len(legs))
	for i, l := range legs {
		lr[i] = l
	}
	return newForwarder(lr, weights, src)
}

// codeOf returns the RZ code of err.
func codeOf(err error) string {
	c, _ := errcode.CodeOf(err)
	return c
}

// exchange returns a client exchange for method and path from peer
// 203.0.113.7.
func exchange(method, path string) *filtertest.Exchange {
	return &filtertest.Exchange{
		MethodV: method,
		SchemeV: "https",
		HostV:   "api.shop.example",
		PathV:   path,
		Hdr:     http.Header{"Accept": {"application/json"}},
		SourceV: filter.Source{IP: netip.MustParseAddr("203.0.113.7"), Peer: netip.MustParseAddrPort("203.0.113.7:51000")},
		Msg:     &filtertest.Message{},
		VarsV:   &expr.Vars{Request: &expr.Request{Method: method, Path: path}},
	}
}

// outbound returns a GET Outbound with an empty body, a Route deadline 15 s
// after the fake clock's start and hooks h (nil runs no leg Phase).
func outbound(h snapshot.LegHooks) *snapshot.Outbound {
	o := &snapshot.Outbound{X: exchange(http.MethodGet, "/v1/orders"), Body: &body.Request{}, Deadline: epoch().Add(15 * time.Second)}
	if h != nil {
		o.Hooks = h
	}
	return o
}

// hooks records the legs whose Phases began.
type hooks struct {
	mu    sync.Mutex
	begun []string
}

func (h *hooks) BeginLeg(_ context.Context, name, _ string) snapshot.LegRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.begun = append(h.begun, name)
	return legRun{}
}

func (h *hooks) list() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.begun)
}

// legRun runs no Filter.
type legRun struct{}

func (legRun) OnUpstreamRequest(context.Context, *filter.Leg) *filter.Response { return nil }

func (legRun) OnUpstreamResponseHeaders(context.Context, *filter.Leg) (bool, *filter.Response) {
	return false, nil
}

func (legRun) OnUpstreamResponseBody(context.Context, *filter.Leg) *filter.Response { return nil }
func (legRun) End(context.Context)                                                  {}

// server is a loopback Upstream that counts requests.
type server struct {
	*httptest.Server
	addr string
	hits atomic.Int64
}

// newServer starts a loopback server answering code with text.
func newServer(t *testing.T, code int, text string) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.hits.Add(1)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, text)
	}))
	t.Cleanup(s.Close)
	s.addr = s.Listener.Addr().String()
	return s
}

// refusedAddr is the Endpoint address the fixture's dialer refuses.
const refusedAddr = "127.0.0.1:1"

// dial connects to loopback servers and refuses refusedAddr as a refused
// connection, without touching the network.
func dial(ctx context.Context, network, address string) (net.Conn, error) {
	if address == refusedAddr {
		return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
	}
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}

// fixture is an upstream.Manager on a fake clock.
type fixture struct {
	t   *testing.T
	clk *clocktest.Fake
	m   *upstream.Manager
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := clocktest.New(epoch())
	return &fixture{t: t, clk: clk, m: upstream.NewManager(upstream.Options{Clock: clk, Rand: lowSource{}, Dial: dial})}
}

// compile compiles the Upstreams into a Set that closes at cleanup; it is
// not activated.
func (f *fixture) compile(ups ...*v1alpha1.Upstream) *upstream.Set {
	f.t.Helper()
	cfgs := make([]upstream.Config, len(ups))
	for i, u := range ups {
		cfgs[i] = upstream.Config{Upstream: u}
	}
	s, err := f.m.Compile(f.t.Context(), upstream.Input{Upstreams: cfgs})
	if err != nil {
		f.t.Fatalf("Compile: %v", err)
	}
	f.t.Cleanup(func() { _ = s.Close() })
	return s
}

// activate compiles and activates the Upstreams.
func (f *fixture) activate(ups ...*v1alpha1.Upstream) *upstream.Set {
	f.t.Helper()
	s := f.compile(ups...)
	s.Activate()
	return s
}

// newUpstream returns an http Upstream with static Endpoints and no
// retries.
func newUpstream(name string, addrs ...string) *v1alpha1.Upstream {
	u := &v1alpha1.Upstream{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.UpstreamSpec{
			Protocol: v1alpha1.UpstreamProtocolHTTP,
			Retries:  &v1alpha1.Retries{Attempts: ptr[int32](0)},
		},
	}
	for _, a := range addrs {
		u.Spec.Endpoints = append(u.Spec.Endpoints, v1alpha1.Endpoint{Address: a})
	}
	return u
}

// breakerOnWindow makes u's breaker open on the first failed leg that
// completes the fixed window guard: resilience.BreakerMinimumLegs counted
// legs, at least resilience.BreakerFailurePercent of them failed.
func breakerOnWindow(u *v1alpha1.Upstream) *v1alpha1.Upstream {
	u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{ConsecutiveFailures: ptr[int32](1)}
	return u
}

// entry returns a Route.spec.upstreams entry; a negative weight leaves it
// absent.
func entry(name string, weight int32) v1alpha1.RouteUpstream {
	e := v1alpha1.RouteUpstream{Name: name}
	if weight >= 0 {
		e.Weight = ptr(weight)
	}
	return e
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// readAll reads and closes a leg response body.
func readAll(t *testing.T, r *snapshot.UpstreamResponse) string {
	t.Helper()
	if r == nil || r.Body == nil {
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
