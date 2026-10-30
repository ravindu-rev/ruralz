// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestBeginBuildsPreRouteActivation checks that Begin builds the
// activation match.when reads before routing (configuration model
// "Variables": request without body, source, now) and the Router's input:
// the host as received, which the Router normalizes, and the normalized
// path; route, consumer, auth and response are null.
func TestBeginBuildsPreRouteActivation(t *testing.T) {
	f := newFixture(t)
	r := serverRequest(http.MethodGet, "/v1/orders?b=2&a=1&a=3", "", http.Header{"X-Tenant": {"t1"}})
	r.Host = "API.Example.com:8443"
	x := f.begin(r)
	defer f.pool.Put(x)

	q := x.MatchRequest()
	if q.Host != "API.Example.com:8443" || q.Path != "/v1/orders" || q.Method != http.MethodGet {
		t.Errorf("MatchRequest() = %q %q %q, want the received host, normalized path and method", q.Host, q.Path, q.Method)
	}
	v := x.Exchange().Vars()
	if q.Vars != v {
		t.Error("MatchRequest().Vars is not the request's activation")
	}
	if got := v.Request.Host; got != "api.example.com" {
		t.Errorf("request.host = %q, want the normalized host", got)
	}
	if got := v.Request.Query()["a"]; got != "1,3" {
		t.Errorf("request.query.a = %q, want 1,3", got)
	}
	if got, ok := expr.JoinedHeader(v.Request.Header, "x-tenant"); !ok || got != "t1" {
		t.Errorf("request.headers[x-tenant] = %q, %v", got, ok)
	}
	if v.Request.Body != nil {
		t.Error("request.body is set before routing")
	}
	if v.Source.IP.String() != "192.0.2.7" || v.Source.Port != 51000 || v.Source.TLSVersion != tls.VersionTLS13 {
		t.Errorf("source = %+v", v.Source)
	}
	if !v.Now.Equal(testStart()) || v.Now.Location() != time.UTC {
		t.Errorf("now = %v, want the request start in UTC", v.Now)
	}
	if v.Route != nil || v.Consumer != nil || v.Auth != nil || v.Response != nil || v.Upstream != nil {
		t.Errorf("activation has post-routing variables: %+v", v)
	}
	xe := x.Exchange()
	if xe.Method() != http.MethodGet || xe.Scheme() != "https" || xe.Host() != "api.example.com" || xe.Path() != "/v1/orders" {
		t.Errorf("Exchange request = %s %s %s %s", xe.Method(), xe.Scheme(), xe.Host(), xe.Path())
	}
	if xe.Listener() != "public" || !xe.Now().Equal(testStart()) || xe.Stripe() != testStripe || xe.TLS() == nil {
		t.Error("Exchange connection data differs from Start")
	}
}

// TestSetRouteFixesRouteAndBudget checks that SetRoute sets CEL route and
// request.pathParams, and the State Store budget of the Route's largest
// stateStoreTimeout on the request stripe (pack 8.7 rule 2); an unmatched
// request has no budget.
func TestSetRouteFixesRouteAndBudget(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/v1/orders/42", "", nil))
	defer f.pool.Put(x)
	rt := testRoute("orders", snapshot.BodyNeeds{})
	params := make([]expr.Param, 1, 4)
	params[0] = expr.Param{Name: "orderId", Value: "42"}
	x.SetRoute(rt, params)

	v := x.Exchange().Vars()
	if v.Route != rt.View || x.Exchange().Route() != rt.View || x.Route() != rt {
		t.Error("route view not set")
	}
	if !slices.Equal(v.Request.PathParams, params) {
		t.Errorf("request.pathParams = %v", v.Request.PathParams)
	}
	if cap(x.ParamBuf()) < 4 || len(x.ParamBuf()) != 0 {
		t.Errorf("ParamBuf() len %d cap %d, want empty storage of the captures' capacity", len(x.ParamBuf()), cap(x.ParamBuf()))
	}
	ctx := context.Background()
	b := x.Budget()
	if b != x.Exchange().StateBudget() || b.Stripe() != testStripe {
		t.Fatal("Budget and StateBudget differ or lack the request stripe")
	}
	if d, ok := b.CallTimeout(ctx, testStart(), time.Second); !ok || d != rt.StateStoreMax {
		t.Errorf("CallTimeout() = %v, %v, want the Route's %v", d, ok, rt.StateStoreMax)
	}

	y := f.begin(serverRequest(http.MethodGet, "/nowhere", "", nil))
	defer f.pool.Put(y)
	y.SetRoute(nil, nil)
	if _, ok := y.Budget().CallTimeout(ctx, testStart(), time.Second); ok {
		t.Error("an unmatched request has a State Store budget")
	}
}

// TestSetRouteGivesTheRouteDeadline checks that the activation carries
// the request deadline every CEL evaluation stops a tracked comprehension
// at (docs/architecture/02-configuration-model.md "Limits"): none before
// the match, with the request start in the Router's input for the
// candidates' own deadlines; the Route deadline, the request start plus
// the Route timeout, once SetRoute fixed the Route, in the client
// activation of every Phase, onLog's included, and in a leg's; none for
// an unmatched request, a Route without a timeout, or the next request on
// the pooled memory.
func TestSetRouteGivesTheRouteDeadline(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/v1/orders", "", nil))
	if d := x.Exchange().Vars().Deadline; !d.IsZero() {
		t.Errorf("deadline before the match = %v, want none", d)
	}
	if s := x.MatchRequest().Start; !s.Equal(testStart()) {
		t.Errorf("MatchRequest().Start = %v, want the request start %v", s, testStart())
	}
	rt := testRoute("orders", snapshot.BodyNeeds{})
	x.SetRoute(rt, nil)
	want := testStart().Add(rt.Timeout)
	if d := x.Exchange().Vars().Deadline; !d.Equal(want) {
		t.Errorf("deadline after SetRoute = %v, want the Route deadline %v", d, want)
	}
	x.Enter(nil, phase.OnLog, nil)
	if d := x.Exchange().Vars().Deadline; !d.Equal(want) {
		t.Errorf("onLog deadline = %v, want the Route deadline %v", d, want)
	}
	l := x.Leg("orders", "")
	if d := l.Exchange().Vars().Deadline; !d.Equal(want) {
		t.Errorf("leg deadline = %v, want the Route deadline %v", d, want)
	}
	l.Release()
	x.SetRoute(nil, nil)
	if d := x.Exchange().Vars().Deadline; !d.IsZero() {
		t.Errorf("deadline of an unmatched request = %v, want none", d)
	}
	untimed := testRoute("untimed", snapshot.BodyNeeds{})
	untimed.Timeout = 0
	x.SetRoute(untimed, nil)
	if d := x.Exchange().Vars().Deadline; !d.IsZero() {
		t.Errorf("deadline of a Route without a timeout = %v, want none", d)
	}
	x.SetRoute(rt, nil)
	f.pool.Put(x)

	y := f.begin(serverRequest(http.MethodGet, "/v1/orders", "", nil))
	defer f.pool.Put(y)
	if d := y.Exchange().Vars().Deadline; !d.IsZero() {
		t.Errorf("deadline of the next request before its match = %v, want none", d)
	}
}

// TestSetIdentityOnce checks Security and identity rule 2: a second
// successful authentication is filter.ErrSecondBinding and keeps the
// first; the first sets CEL consumer and auth, and auth.mtls sets
// source.clientCertSubject. A leg view cannot authenticate.
func TestSetIdentityOnce(t *testing.T) {
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/", "", nil), testRoute("r", snapshot.BodyNeeds{}))
	defer f.pool.Put(x)
	xe := x.Exchange()
	c := &expr.Consumer{Name: "acme", Tier: "gold"}
	first := &filter.Identity{Method: filter.MethodMTLS, Policy: "mtls", Consumer: c, CertSubject: "CN=acme"}
	if err := xe.SetIdentity(first); err != nil {
		t.Fatalf("first SetIdentity() = %v", err)
	}
	if err := xe.SetIdentity(&filter.Identity{Method: filter.MethodJWT}); !errors.Is(err, filter.ErrSecondBinding) {
		t.Errorf("second SetIdentity() = %v, want ErrSecondBinding", err)
	}
	if xe.Identity() != first {
		t.Error("the second binding replaced the first")
	}
	v := xe.Vars()
	if v.Consumer != c || v.Auth == nil || v.Auth.Method != filter.MethodMTLS || v.Source.ClientCertSubject != "CN=acme" {
		t.Errorf("activation after auth: consumer %v auth %+v subject %q", v.Consumer, v.Auth, v.Source.ClientCertSubject)
	}
	l := x.Leg("orders", "")
	defer l.Release()
	if err := l.Exchange().SetIdentity(first); !errors.Is(err, filter.ErrNotAvailable) {
		t.Errorf("leg SetIdentity() = %v, want ErrNotAvailable", err)
	}
	if l.Exchange().Identity() != first {
		t.Error("a leg view does not read the request's identity")
	}
}

// TestJWTIdentityLeavesClientCertSubject checks that only auth.mtls sets
// source.clientCertSubject (expr.Source).
func TestJWTIdentityLeavesClientCertSubject(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	if err := x.Exchange().SetIdentity(&filter.Identity{Method: filter.MethodJWT, CertSubject: "CN=x"}); err != nil {
		t.Fatal(err)
	}
	if got := x.Exchange().Vars().Source.ClientCertSubject; got != "" {
		t.Errorf("source.clientCertSubject = %q after auth.jwt", got)
	}
}

// TestRequestIDIsTraceIDHex checks that RequestID is the request's 32-hex
// trace ID (problem document requestId) and that concurrent legs read it
// safely (run under -race).
func TestRequestIDIsTraceIDHex(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	const want = "4bf92f3577b34da6a3ce929d0e0e4736"
	var wg sync.WaitGroup
	for range 8 {
		l := x.Leg("u", "")
		wg.Go(func() {
			defer l.Release()
			if got := l.Exchange().RequestID(); got != want {
				t.Errorf("leg RequestID() = %q", got)
			}
		})
	}
	wg.Wait()
	if got := x.Exchange().RequestID(); got != want {
		t.Errorf("RequestID() = %q, want %q", got, want)
	}
}

// TestRouteMetricsNeverNil checks that RouteMetrics returns the Route's
// handles, and handles that record nothing when there are none
// (filter.Exchange: never nil).
func TestRouteMetricsNeverNil(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	m := x.Exchange().RouteMetrics()
	if m == nil {
		t.Fatal("RouteMetrics() = nil")
	}
	m.Cache[emit.CacheHit].Add(testStripe, 1)
	m.Requests.Inc(testStripe, http.StatusOK)
	m.Duration.Record(testStripe, 1)
	rt := testRoute("r", snapshot.BodyNeeds{})
	rt.Metrics = &emit.RouteMetrics{}
	x.SetRoute(rt, nil)
	if x.Exchange().RouteMetrics() != rt.Metrics {
		t.Error("RouteMetrics() is not the Route's handles")
	}
	l := x.Leg("u", "")
	defer l.Release()
	if l.Exchange().RouteMetrics() != rt.Metrics {
		t.Error("a leg view does not record on the Route's handles")
	}
}

// TestPutDropsRequestState checks that Put releases every reservation and
// drops every per-request value, so a pooled Request never carries a
// previous request's identity, slots, decisions, records or response; a
// second Put does nothing.
func TestPutDropsRequestState(t *testing.T) {
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodPost, "/v1", `{"a":1}`, jsonHeader()), testRoute("r", snapshot.BodyNeeds{RequestGate: true}))
	if err := x.PrepareBody(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := testPolicy("p", 2, phase.ClassTransform, phase.ScopeRoute, 0)
	x.Enter(p, phase.OnRequestBody, nil)
	if _, err := x.Exchange().Message().Decoded(context.Background()); err != nil {
		t.Fatal(err)
	}
	*x.Exchange().PolicyState() = "state"
	x.SetWhen(p, true)
	x.RecordFailure(p, phase.OnRequestBody, v1alpha1.FailureModeOpen)
	x.RecordShortCircuit(p, phase.OnRequestBody, http.StatusBadRequest)
	_ = x.Exchange().SetIdentity(&filter.Identity{Method: filter.MethodAPIKey})
	x.Exchange().AddRateLimitField(filter.RateLimitField{Name: "rl"})
	x.SetGenerated(&filter.Response{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"1"}}})
	if f.bud.Used() == 0 {
		t.Fatal("the gate and decoded body reserved nothing")
	}

	f.pool.Put(x)
	if used := f.bud.Used(); used != 0 {
		t.Errorf("budget used %d after Put, want 0", used)
	}
	if x.live.Load() || x.ident != nil || x.route != nil || x.hreq != nil || x.hdr != nil || x.cur != nil {
		t.Error("Put kept request references")
	}
	if len(x.slots.s) != 0 || x.slots.spare != nil || len(x.whens) != 0 || x.rec.nfails != 0 || x.rec.sc != nil {
		t.Error("Put kept slots, decisions or records")
	}
	if slices.ContainsFunc(x.slots.s[:cap(x.slots.s)], func(a any) bool { return a != nil }) {
		t.Error("a slot past the length still holds a value")
	}
	if len(x.rateLimits) != 0 || x.resp.set || x.resp.header != nil || x.gated || x.bodyDec.ready.Load() {
		t.Error("Put kept rate limits, the response or the body view")
	}
	if x.vars != (expr.Vars{}) || !reflect.ValueOf(x.out).IsZero() {
		t.Error("Put kept the activation or the Outbound")
	}
	f.pool.Put(x) // a second Put does nothing
	if x.live.Load() {
		t.Error("a second Put revived x")
	}
}

// TestOutboundCarriesRequest checks that Outbound gives the Forwarder the
// client Exchange, the request body, the stripe and, as its Excluder, the
// request's gateway-added timer, so the Upstream layer's I/O is excluded
// from gateway-added time (snapshot.Outbound).
func TestOutboundCarriesRequest(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	o := x.Outbound()
	if o.X != x.Exchange() || o.Body != x.RequestBody() || o.Stripe != testStripe {
		t.Errorf("Outbound() = %+v", o)
	}
	if x.Timer() == nil {
		t.Fatal("Timer() = nil")
	}
	if d, ok := x.Timer().Result(testStart().Add(time.Millisecond)); !ok || d != time.Millisecond {
		t.Errorf("Timer().Result() = %v, %v, want the time since the request start", d, ok)
	}

	y := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(y)
	oy := y.Outbound()
	if oy.Excluder != y.Timer() {
		t.Fatalf("Outbound().Excluder = %v, want the request's timer", oy.Excluder)
	}
	// Upstream I/O the Forwarder marks is not gateway-added.
	oy.Excluder.Enter(testStart().Add(time.Millisecond))
	oy.Excluder.Leave(testStart().Add(3 * time.Millisecond))
	if d, ok := y.Timer().Result(testStart().Add(4 * time.Millisecond)); !ok || d != 2*time.Millisecond {
		t.Errorf("gateway-added time = %v, %v; want 2ms after 2ms of Upstream I/O", d, ok)
	}
}

// TestFinalRecordsOutcome checks that Final reports the outcome SetFinal
// recorded, and that SetFinal sets CEL duration (accessLog.when, onLog).
func TestFinalRecordsOutcome(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	x.SetFinal(filter.Final{Status: http.StatusBadGateway, Code: "RZ-RT-012", Committed: true, RejectedAfter: true}, 3*time.Millisecond)
	got := x.Exchange().Final()
	want := filter.Final{Status: http.StatusBadGateway, Code: "RZ-RT-012", Committed: true}
	if got != want {
		t.Errorf("Final() = %+v, want %+v (RejectedAfter is the exchange's own)", got, want)
	}
	if d := x.Exchange().Vars().Duration; d != 3*time.Millisecond {
		t.Errorf("duration = %v", d)
	}
}
