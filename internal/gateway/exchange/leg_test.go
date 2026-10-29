// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// legPolicy is an Upstream-scope Policy.
func legPolicy() *snapshot.Policy {
	return testPolicy("up", 0, phase.ClassTransform, phase.ScopeUpstream, 0)
}

// outRequest returns an attempt's outgoing request with a query.
func outRequest(tb testing.TB, x *Request, query string) *http.Request {
	tb.Helper()
	r := outgoing(tb, x)
	r.URL.RawQuery = query
	r.Header.Set("X-Out", "1")
	return r
}

// TestLegActivation checks a leg's Vars: a copy of the client activation
// (route, consumer, auth, source, now) with upstream (name and Endpoint),
// attempt, error and response replaced, and request describing the
// attempt's outgoing request (its headers and query); the client's
// activation is unchanged.
func TestLegActivation(t *testing.T) {
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/v1/orders?q=client", "", nil), testRoute("orders", snapshot.BodyNeeds{}))
	defer f.pool.Put(x)
	_ = x.Exchange().SetIdentity(&filter.Identity{Method: filter.MethodJWT})
	l := x.Leg("orders", "")
	defer l.Release()
	out := outRequest(t, x, "q=leg")
	l.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 2, Endpoint: "10.0.0.1:8080", Request: out})
	l.Enter(legPolicy(), phase.OnUpstreamRequest, nil)
	v := l.Exchange().Vars()
	cv := x.Exchange().Vars()
	if v.Upstream == nil || v.Upstream.Name != "orders" || v.Upstream.Endpoint != "10.0.0.1:8080" || v.Attempt != 2 {
		t.Errorf("leg upstream %+v attempt %d", v.Upstream, v.Attempt)
	}
	if v.Route != cv.Route || v.Auth != cv.Auth || v.Source != cv.Source || !v.Now.Equal(cv.Now) || v.Error != nil || v.Response != nil {
		t.Error("the leg activation does not copy the client's")
	}
	if v.Request.Header.Get("X-Out") != "1" || v.Request.Query()["q"] != "leg" || v.Request.Path != "/v1/orders" {
		t.Errorf("leg request = headers %v query %v path %q", v.Request.Header, v.Request.Query(), v.Request.Path)
	}
	if cv.Upstream != nil || cv.Attempt != 0 || cv.Request.Query()["q"] != "client" {
		t.Error("the leg changed the client activation")
	}

	resp, _ := upstreamResponse(http.StatusServiceUnavailable, "")
	defer func() { _ = resp.Body.Close() }()
	l.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 2, Endpoint: "10.0.0.1:8080", Request: out, Response: resp, ErrorKind: "reset"})
	l.Enter(legPolicy(), phase.OnUpstreamResponseHeaders, nil)
	v = l.Exchange().Vars()
	if v.Response == nil || v.Response.Status != http.StatusServiceUnavailable || v.Error == nil || v.Error.Kind != "reset" {
		t.Errorf("leg response %+v error %+v", v.Response, v.Error)
	}
}

// TestLegRequestBodyPerAttempt checks onUpstreamRequest's message: its
// body is the client's gated input at every attempt; SetBody makes the
// attempt's outgoing body (ContentLength, GetBody, body fields) and the
// leg's request.body, leaving the client's body for the next attempt; the
// reservation lasts until Release; SetRawQuery edits the outgoing query.
func TestLegRequestBodyPerAttempt(t *testing.T) {
	f := newFixture(t)
	x := f.gatedRequest(t, `{"a":1}`, jsonHeader())
	defer f.pool.Put(x)
	ctx := context.Background()
	base := f.bud.Used()
	l := x.Leg("orders", "")
	m := l.Exchange().Message
	out1 := outRequest(t, x, "")
	l.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 1, Request: out1})
	l.Enter(legPolicy(), phase.OnUpstreamRequest, nil)
	if b, err := m().Body(ctx); err != nil || string(b) != `{"a":1}` {
		t.Fatalf("attempt 1 Body() = %q, %v", b, err)
	}
	if err := m().SetBody(ctx, []byte(`{"b":22}`)); err != nil {
		t.Fatalf("SetBody() = %v", err)
	}
	if got := readAll(t, out1.Body); got != `{"b":22}` || out1.ContentLength != 8 || out1.Header.Get("Content-Length") != "8" {
		t.Errorf("outgoing body %q, length %d", got, out1.ContentLength)
	}
	rb, err := out1.GetBody()
	if err != nil || readAll(t, rb) != `{"b":22}` {
		t.Errorf("GetBody() = %v", err)
	}
	if v, _ := m().Decoded(ctx); !reflect.DeepEqual(numbersAsText(nativeOf(t, v)), map[string]any{"b": "22"}) {
		t.Errorf("Decoded() = %v", nativeOf(t, v))
	}
	if got := nativeOf(t, l.Exchange().Vars().Request.Body); !reflect.DeepEqual(numbersAsText(got), map[string]any{"b": "22"}) {
		t.Errorf("leg request.body = %v", got)
	}
	m().SetRawQuery("v=2")
	if out1.URL.RawQuery != "v=2" || m().RawQuery() != "v=2" || l.Exchange().Vars().Request.Query()["v"] != "2" {
		t.Error("SetRawQuery did not reach the outgoing request and the leg's request.query")
	}
	if b, _ := x.gatedRequest(); string(b) != `{"a":1}` {
		t.Errorf("the client's body changed to %q", b)
	}

	out2 := outRequest(t, x, "")
	l.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 2, Request: out2})
	l.Enter(legPolicy(), phase.OnUpstreamRequest, nil)
	if b, _ := m().Body(ctx); string(b) != `{"a":1}` {
		t.Errorf("attempt 2 Body() = %q, want the client's input again", b)
	}
	v2, _ := m().Decoded(ctx)
	if cv, _ := x.requestView(); v2 != cv.val {
		t.Error("attempt 2 does not read the client's shared decoded body")
	}
	l.Enter(legPolicy(), phase.OnUpstreamResponseHeaders, nil)
	if err := l.Exchange().Message().SetBody(ctx, nil); !errors.Is(err, filter.ErrNotAvailable) {
		t.Errorf("SetBody() in a response Phase = %v, want ErrNotAvailable", err)
	}
	if f.bud.Used() <= base {
		t.Error("the rewritten body was not reserved")
	}
	l.Release()
	if f.bud.Used() != base+decodedReserved(x) {
		t.Errorf("budget %d after Release, want the client's %d", f.bud.Used(), base+decodedReserved(x))
	}
}

// decodedReserved returns what the client's decoded body reserved beyond
// the gate (whole increments).
func decodedReserved(x *Request) int64 { return x.acct.Reserved() }

// TestLegRequestBodyStreamed checks that a leg has no body to read when
// the client's streams, while SetBody still installs one.
func TestLegRequestBodyStreamed(t *testing.T) {
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodPost, "/", "abc", nil), testRoute("r", snapshot.BodyNeeds{}))
	defer f.pool.Put(x)
	if err := x.PrepareBody(context.Background()); err != nil {
		t.Fatal(err)
	}
	l := x.Leg("orders", "")
	defer l.Release()
	out := &http.Request{Header: http.Header{}, URL: &url.URL{}, Body: http.NoBody}
	l.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 1, Request: out})
	l.Enter(legPolicy(), phase.OnUpstreamRequest, nil)
	if _, err := l.Exchange().Message().Body(context.Background()); !errors.Is(err, filter.ErrNotAvailable) {
		t.Errorf("Body() of a streamed client body = %v, want ErrNotAvailable", err)
	}
	if err := l.Exchange().Message().SetBody(context.Background(), nil); err != nil || out.Body != http.NoBody || out.ContentLength != 0 {
		t.Errorf("SetBody(nil) = %v, body %v", err, out.Body)
	}
}

// TestLegResponseGate checks onUpstreamResponseBody's message: the
// attempt's response body is gated within limits.maxResponseBodyBytes
// (not in onUpstreamResponseHeaders), read again by whoever streams the
// response, replaced by SetBody (ContentLength, body fields, response.body),
// and kept with its reservation after the leg ends until it is closed,
// which also closes the original body; a longer body is filter.ErrTooLarge.
func TestLegResponseGate(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	ctx := context.Background()
	base := f.bud.Used()
	l := x.Leg("orders", "")
	resp, orig := upstreamResponse(http.StatusOK, `{"n":1}`)
	resp.Header.Set("Content-Digest", "sha-256=:x:")
	leg := &filter.Leg{Upstream: "orders", Attempt: 1, Response: resp}
	l.SetLeg(leg)
	l.Enter(legPolicy(), phase.OnUpstreamResponseHeaders, nil)
	if _, err := l.Exchange().Message().Body(ctx); !errors.Is(err, filter.ErrNotAvailable) {
		t.Errorf("Body() in onUpstreamResponseHeaders = %v, want ErrNotAvailable", err)
	}
	l.Enter(legPolicy(), phase.OnUpstreamResponseBody, nil)
	m := l.Exchange().Message()
	if b, err := m.Body(ctx); err != nil || string(b) != `{"n":1}` {
		t.Fatalf("Body() = %q, %v", b, err)
	}
	if got := nativeOf(t, l.Exchange().Vars().Response.Body); !reflect.DeepEqual(numbersAsText(got), map[string]any{"n": "1"}) {
		t.Errorf("response.body = %v", got)
	}
	if err := m.SetBody(ctx, []byte(`{"n":22}`)); err != nil {
		t.Fatalf("SetBody() = %v", err)
	}
	if resp.ContentLength != 8 || resp.Header.Get("Content-Length") != "8" || resp.Header.Get("Content-Digest") != "" {
		t.Errorf("response fields after SetBody: %d %v", resp.ContentLength, resp.Header)
	}
	if got := nativeOf(t, l.Exchange().Vars().Response.Body); !reflect.DeepEqual(numbersAsText(got), map[string]any{"n": "22"}) {
		t.Errorf("response.body after SetBody = %v", got)
	}
	l.Release()
	if got := readAll(t, resp.Body); got != `{"n":22}` {
		t.Errorf("the response streams %q after the leg ended", got)
	}
	if f.bud.Used() == base {
		t.Error("the gated response holds no reservation before Close")
	}
	if err := resp.Body.Close(); err != nil || orig.closed.Load() != 1 {
		t.Errorf("Close() = %v, original closed %d times", err, orig.closed.Load())
	}
	if err := resp.Body.Close(); err != nil || orig.closed.Load() != 1 {
		t.Error("a second Close closed the original again")
	}
	if f.bud.Used() != base {
		t.Errorf("budget %d after Close, want %d", f.bud.Used(), base)
	}
	if _, err := resp.Body.Read(make([]byte, 1)); err == nil {
		t.Error("Read after Close succeeded")
	}

	l2 := x.Leg("orders", "")
	defer l2.Release()
	big, _ := upstreamResponse(http.StatusOK, strings.Repeat("x", int(testLimits().MaxResponseBodyBytes)+1))
	defer func() { _ = big.Body.Close() }()
	l2.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 1, Response: big})
	l2.Enter(legPolicy(), phase.OnUpstreamResponseBody, nil)
	if _, err := l2.Exchange().Message().Body(ctx); !errors.Is(err, filter.ErrTooLarge) {
		t.Errorf("Body() over the limit = %v, want ErrTooLarge", err)
	}
	if l2.Exchange().Vars().Response.Body != nil {
		t.Error("response.body is set after a failed gate")
	}
}

// TestLegViewReadsClient checks that a leg view reads the client request
// read-only (filter.Exchange: the same client request, with its own Leg)
// and that the client-only operations do nothing on it.
func TestLegViewReadsClient(t *testing.T) {
	f := newFixture(t)
	rt := testRoute("orders", snapshot.BodyNeeds{})
	x := f.routed(serverRequest(http.MethodGet, "/v1", "", http.Header{"X-In": {"1"}}), rt)
	defer f.pool.Put(x)
	x.SetFinal(filter.Final{Status: http.StatusOK}, time.Second)
	l := x.Leg("orders", "s1")
	defer l.Release()
	lv, cv := l.Exchange(), x.Exchange()
	if lv.Method() != cv.Method() || lv.Scheme() != cv.Scheme() || lv.Host() != cv.Host() || lv.Path() != cv.Path() ||
		lv.Listener() != cv.Listener() || !lv.Now().Equal(cv.Now()) || lv.Route() != cv.Route() || lv.Source() != cv.Source() ||
		lv.TLS() != cv.TLS() || lv.Stripe() != cv.Stripe() || lv.Header().Get("X-In") != "1" {
		t.Error("the leg view does not read the client request")
	}
	if lv.Final() != (filter.Final{}) {
		t.Errorf("leg Final() = %+v, want the zero value", lv.Final())
	}
	lv.ReplaceResponse(&filter.Response{Status: http.StatusTeapot})
	if x.resp.set {
		t.Error("a leg view replaced the client response")
	}
	if err := lv.Reserve(10); err != nil || l.(*leg).acct.Charged() != 10 || x.acct.Charged() != 0 {
		t.Error("a leg reservation is not the leg's own")
	}
	lv.Release(10)
	sib := l.Leg("inventory", "s2")
	defer sib.Release()
	if sib.(*leg).parent != x || sib.Exchange().Path() != cv.Path() {
		t.Error("Leg on a leg state is not a sibling leg of the same request")
	}
}

// TestLegBudgetIsCopy checks that a leg's State Store budget is a copy of
// the request's: its first call starts the leg's own deadline, so
// concurrent legs never share one (statestore.RequestBudget needs no
// lock).
func TestLegBudgetIsCopy(t *testing.T) {
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/", "", nil), testRoute("r", snapshot.BodyNeeds{}))
	defer f.pool.Put(x)
	l := x.Leg("orders", "")
	defer l.Release()
	ctx := context.Background()
	if _, ok := l.Budget().CallTimeout(ctx, testStart(), time.Second); !ok {
		t.Fatal("the leg has no budget")
	}
	later := testStart().Add(15 * time.Millisecond)
	if d, _ := x.Budget().CallTimeout(ctx, later, time.Second); d != 20*time.Millisecond {
		t.Errorf("client CallTimeout() = %v, want the full 20ms: the leg started its own deadline", d)
	}
	if l.Exchange().StateBudget() != l.Budget() {
		t.Error("leg StateBudget and Budget differ")
	}
}

// TestLegResponseGateMaxBodyBytes checks the cap of a leg's gated response
// (filter.Leg.MaxBodyBytes): a composition step's maxBodyBytes replaces
// limits.maxResponseBodyBytes, lower or higher, and 0 keeps it.
func TestLegResponseGateMaxBodyBytes(t *testing.T) {
	limit := int(testLimits().MaxResponseBodyBytes)
	for _, tc := range []struct {
		name string
		max  int64
		size int
		err  error
	}{
		{"step cap below the body", 8, 9, filter.ErrTooLarge},
		{"step cap fits", 8, 8, nil},
		{"step cap above the Gateway limit", int64(limit) * 2, limit + 1, nil},
		{"no step cap: the Gateway limit", 0, limit + 1, filter.ErrTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
			defer f.pool.Put(x)
			l := x.Leg("orders", "step")
			defer l.Release()
			resp, _ := upstreamResponse(http.StatusOK, strings.Repeat("x", tc.size))
			defer func() { _ = resp.Body.Close() }()
			l.SetLeg(&filter.Leg{Upstream: "orders", Step: "step", Attempt: 1, Response: resp, MaxBodyBytes: tc.max})
			l.Enter(legPolicy(), phase.OnUpstreamResponseBody, nil)
			b, err := l.Exchange().Message().Body(context.Background())
			if !errors.Is(err, tc.err) || (err == nil && len(b) != tc.size) {
				t.Errorf("Body() = %d bytes, %v; want %d bytes, %v", len(b), err, tc.size, tc.err)
			}
		})
	}
}

// TestLegAttemptWithoutResponse checks onUpstreamResponseHeaders on an
// attempt that got no response: CEL response is null and error.kind the
// attempt's error kind, the message is an empty response (status 0) whose
// header map takes writes, and a new attempt starts with an empty one.
func TestLegAttemptWithoutResponse(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	l := x.Leg("orders", "")
	defer l.Release()
	l.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 1, Endpoint: "10.0.0.1:8080", ErrorKind: "connect"})
	l.Enter(legPolicy(), phase.OnUpstreamResponseHeaders, nil)
	v := l.Exchange().Vars()
	if v.Response != nil || v.Error == nil || v.Error.Kind != "connect" || v.Attempt != 1 {
		t.Errorf("activation: response %v, error %+v, attempt %d; want null, connect, 1", v.Response, v.Error, v.Attempt)
	}
	m := l.Exchange().Message()
	if m.Status() != 0 || l.Exchange().Leg().Response != nil {
		t.Errorf("message status %d, leg response %v; want 0 and nil", m.Status(), l.Exchange().Leg().Response)
	}
	m.Header().Set("X-Fallback", "next")
	l.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 2, Endpoint: "10.0.0.2:8080", ErrorKind: "reset"})
	l.Enter(legPolicy(), phase.OnUpstreamResponseHeaders, nil)
	if h := l.Exchange().Message().Header(); len(h) != 0 {
		t.Errorf("header of a new attempt without response = %v, want empty", h)
	}
	if v := l.Exchange().Vars(); v.Error == nil || v.Error.Kind != "reset" || v.Attempt != 2 {
		t.Errorf("error of attempt 2 = %+v, want reset", v.Error)
	}
}
