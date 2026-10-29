// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// upstreamReply returns an Upstream response with a JSON body for
// SetResponse.
func upstreamReply(status int, payload string) (*snapshot.UpstreamResponse, *closeBody) {
	cb := &closeBody{Reader: strings.NewReader(payload)}
	h := jsonHeader()
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	return &snapshot.UpstreamResponse{
		Status: status, Header: h, Body: cb, Upstream: "orders", Endpoint: "10.0.0.1:8080", Attempts: 1,
	}, cb
}

// responder enters an onResponse Policy.
func responder(x *Request) {
	x.Enter(testPolicy("resp", 0, phase.ClassTransform, phase.ScopeRoute, 0), phase.OnResponse, nil)
}

// TestSetResponseActivation checks that SetResponse makes an Upstream
// response the client response onResponse runs on, with CEL response
// (status and headers) and upstream (the last leg's Upstream and
// Endpoint), and that Reply keeps its stream for the handler.
func TestSetResponseActivation(t *testing.T) {
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/", "", nil), testRoute("r", snapshot.BodyNeeds{}))
	defer f.pool.Put(x)
	u, cb := upstreamReply(http.StatusCreated, `{"id":1}`)
	x.SetResponse(u)
	responder(x)
	v := x.Exchange().Vars()
	if v.Response == nil || v.Response.Status != http.StatusCreated || v.Response.Header.Get("Content-Type") == "" {
		t.Errorf("response = %+v", v.Response)
	}
	if v.Upstream == nil || v.Upstream.Name != "orders" || v.Upstream.Endpoint != "10.0.0.1:8080" {
		t.Errorf("upstream = %+v", v.Upstream)
	}
	if v.Response.Body != nil {
		t.Error("response.body is set for a Route that does not gate the response")
	}
	m := x.Exchange().Message()
	if m.Status() != http.StatusCreated || m.Generated() {
		t.Errorf("Message() status %d generated %v", m.Status(), m.Generated())
	}
	if _, err := m.Body(context.Background()); !errors.Is(err, filter.ErrNotAvailable) {
		t.Errorf("Body() of an ungated response = %v, want ErrNotAvailable", err)
	}
	got := x.Reply()
	if got.Status != http.StatusCreated || got.Buffered || got.Stream != cb || got.Generated {
		t.Errorf("Reply() = %+v", got)
	}
}

// TestResponseGate checks the response gate (BodyNeeds.ResponseGate): the
// Upstream body is read whole within limits.maxResponseBodyBytes, readable
// and decoded in onResponse only, and Reply sends the buffered body; a
// longer body is filter.ErrTooLarge (502 RZ-UP-010).
func TestResponseGate(t *testing.T) {
	rt := testRoute("r", snapshot.BodyNeeds{ResponseGate: true})
	t.Run("gated", func(t *testing.T) {
		f := newFixture(t)
		x := f.routed(serverRequest(http.MethodGet, "/", "", nil), rt)
		defer f.pool.Put(x)
		u, cb := upstreamReply(http.StatusOK, `{"total":3}`)
		x.SetResponse(u)
		ctx := context.Background()
		if err := x.GateResponse(ctx); err != nil {
			t.Fatalf("GateResponse() = %v", err)
		}
		responder(x)
		m := x.Exchange().Message()
		if b, err := m.Body(ctx); err != nil || string(b) != `{"total":3}` {
			t.Errorf("Body() = %q, %v", b, err)
		}
		v, err := m.Decoded(ctx)
		if err != nil || !reflect.DeepEqual(numbersAsText(nativeOf(t, v)), map[string]any{"total": "3"}) {
			t.Errorf("Decoded() = %v, %v", nativeOf(t, v), err)
		}
		if x.Exchange().Vars().Response.Body != v {
			t.Error("response.body is not the decoded body")
		}
		r := x.Reply()
		if !r.Buffered || string(r.Body) != `{"total":3}` || r.Stream != cb {
			t.Errorf("Reply() = %+v", r)
		}
		x.Enter(testPolicy("log", 1, phase.ClassAdmission, phase.ScopeRoute, 0), phase.OnLog, nil)
		if _, err := x.Exchange().Message().Body(ctx); !errors.Is(err, filter.ErrNotAvailable) {
			t.Errorf("Body() in onLog = %v, want ErrNotAvailable", err)
		}
	})
	t.Run("lazy", func(t *testing.T) {
		f := newFixture(t)
		x := f.routed(serverRequest(http.MethodGet, "/", "", nil), rt)
		defer f.pool.Put(x)
		u, _ := upstreamReply(http.StatusOK, `{"a":true}`)
		x.SetResponse(u)
		responder(x)
		// A spec.when reading response.body before any Filter read it.
		if got := nativeOf(t, x.Exchange().Vars().Response.Body); !reflect.DeepEqual(got, map[string]any{"a": true}) {
			t.Errorf("response.body = %v", got)
		}
	})
	t.Run("too large", func(t *testing.T) {
		f := newFixture(t)
		x := f.routed(serverRequest(http.MethodGet, "/", "", nil), rt)
		defer f.pool.Put(x)
		u, _ := upstreamReply(http.StatusOK, strings.Repeat("x", int(testLimits().MaxResponseBodyBytes)+1))
		x.SetResponse(u)
		err := x.GateResponse(context.Background())
		if !errors.Is(err, filter.ErrTooLarge) || body.Code(err, body.SideResponse) != "RZ-UP-010" {
			t.Errorf("GateResponse() = %v, want ErrTooLarge", err)
		}
		if err2 := x.GateResponse(context.Background()); !errors.Is(err2, filter.ErrTooLarge) {
			t.Errorf("a second GateResponse() = %v, want the first error", err2)
		}
	})
}

// TestResponseSetBody checks filter.Message.SetBody on the client
// response: in onResponse the new body is sent buffered, reserved from the
// budget, with Content-Length set, Content-Encoding and the body digests
// removed, response.body invalidated, and started tees stopped;
// filter.ErrNotAvailable outside onResponse.
func TestResponseSetBody(t *testing.T) {
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/", "", nil), testRoute("r", snapshot.BodyNeeds{ResponseGate: true}))
	defer f.pool.Put(x)
	u, _ := upstreamReply(http.StatusOK, `{"a":1}`)
	u.Header.Set("Content-Encoding", "gzip")
	u.Header.Set("Repr-Digest", "sha-256=:x:")
	x.SetResponse(u)
	ctx := context.Background()
	responder(x)
	tee := x.Exchange().TeeResponse(1 << 10)
	if tee == nil {
		t.Fatal("TeeResponse() = nil")
	}
	m := x.Exchange().Message()
	if _, err := m.Decoded(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.SetBody(ctx, []byte(`{"a":2}`)); err != nil {
		t.Fatalf("SetBody() = %v", err)
	}
	r := x.Reply()
	if !r.Buffered || string(r.Body) != `{"a":2}` || r.Header.Get("Content-Length") != "7" ||
		r.Header.Get("Content-Encoding") != "" || r.Header.Get("Repr-Digest") != "" {
		t.Errorf("Reply() after SetBody = %+v", r)
	}
	if b, ok := tee.Result(); ok || b != nil {
		t.Error("a tee of the replaced body still has a copy")
	}
	v, _ := m.Decoded(ctx)
	if !reflect.DeepEqual(numbersAsText(nativeOf(t, v)), map[string]any{"a": "2"}) {
		t.Errorf("Decoded() after SetBody = %v", nativeOf(t, v))
	}
	x.Enter(testPolicy("log", 1, phase.ClassAdmission, phase.ScopeRoute, 0), phase.OnLog, nil)
	if err := x.Exchange().Message().SetBody(ctx, nil); !errors.Is(err, filter.ErrNotAvailable) {
		t.Errorf("SetBody() in onLog = %v, want ErrNotAvailable", err)
	}
}

// TestResponseSetBodyUngated checks that SetBody replaces a streamed
// response that was never gated: the new body is reserved from the budget
// and sent buffered, and the stream stays for the handler to close.
func TestResponseSetBodyUngated(t *testing.T) {
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/", "", nil), testRoute("r", snapshot.BodyNeeds{}))
	defer f.pool.Put(x)
	u, cb := upstreamReply(http.StatusOK, "stream")
	x.SetResponse(u)
	responder(x)
	base := f.bud.Used()
	if err := x.Exchange().Message().SetBody(context.Background(), []byte("new")); err != nil {
		t.Fatalf("SetBody() = %v", err)
	}
	r := x.Reply()
	if !r.Buffered || string(r.Body) != "new" || r.Stream != cb {
		t.Errorf("Reply() = %+v", r)
	}
	if f.bud.Used() <= base {
		t.Error("the new body was not reserved")
	}
}

// TestReplaceResponse checks filter.Exchange.ReplaceResponse: in
// onResponse it replaces the uncommitted response (status, fields, code,
// generated), the later Policies see it, the Upstream stream is kept for
// the handler to close, and a replacement may reuse the current fields;
// outside onResponse it does nothing.
func TestReplaceResponse(t *testing.T) {
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/", "", nil), testRoute("r", snapshot.BodyNeeds{}))
	defer f.pool.Put(x)
	u, cb := upstreamReply(http.StatusServiceUnavailable, "")
	x.SetResponse(u)
	responder(x)
	stale := http.Header{"Age": {"30"}, "Warning": {"110"}}
	x.Exchange().ReplaceResponse(&filter.Response{Status: http.StatusOK, Header: stale, Body: []byte("stale")})
	m := x.Exchange().Message()
	if m.Status() != http.StatusOK || !m.Generated() || m.Header().Get("Age") != "30" {
		t.Errorf("Message() after ReplaceResponse: %d %v %v", m.Status(), m.Generated(), m.Header())
	}
	m.Header().Add("Age", "31")
	if len(stale["Age"]) != 1 {
		t.Error("an edit of the replaced response reached the Filter's header map")
	}
	x.Exchange().ReplaceResponse(&filter.Response{Status: http.StatusBadGateway, Code: "RZ-RT-012", Header: m.Header()})
	r := x.Reply()
	if r.Status != http.StatusBadGateway || r.Code != "RZ-RT-012" || !r.Generated || r.Buffered || r.Stream != cb {
		t.Errorf("Reply() = %+v", r)
	}
	if got := r.Header.Values("Age"); len(got) != 2 {
		t.Errorf("Age = %v after a replacement built from the current fields", got)
	}
	if v := x.Exchange().Vars().Response; v.Status != http.StatusBadGateway {
		t.Errorf("response.status = %d", v.Status)
	}
	x.Enter(testPolicy("log", 1, phase.ClassAdmission, phase.ScopeRoute, 0), phase.OnLog, nil)
	x.Exchange().ReplaceResponse(&filter.Response{Status: http.StatusTeapot})
	if x.Reply().Status != http.StatusBadGateway {
		t.Error("ReplaceResponse after onResponse changed the response")
	}
}

// TestSetGeneratedResponse checks that a generated response is the client
// response with its literal body buffered, and that onResponse edits never
// reach the Filter's header map or its value arrays; a response a leg
// Filter generated keeps the Upstream stream and CEL upstream.
func TestSetGeneratedResponse(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	backing := make([]string, 4)
	backing[0] = "Origin"
	h := http.Header{"Vary": backing[:1]}
	x.SetGenerated(&filter.Response{Status: http.StatusNoContent, Header: h, Body: []byte{}})
	responder(x)
	x.Exchange().Message().Header().Add("Vary", "Accept")
	if backing[1] != "" || len(h["Vary"]) != 1 {
		t.Errorf("an onResponse edit wrote into the Filter's fields: %v", backing)
	}
	r := x.Reply()
	if r.Status != http.StatusNoContent || !r.Generated || !r.Buffered || len(r.Body) != 0 {
		t.Errorf("Reply() = %+v", r)
	}

	y := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(y)
	cb := &closeBody{Reader: strings.NewReader("")}
	y.SetResponse(&snapshot.UpstreamResponse{
		Generated: &filter.Response{Status: http.StatusUnauthorized, Code: "RZ-AUTH-020"},
		Body:      cb, Upstream: "orders",
	})
	r = y.Reply()
	if r.Status != http.StatusUnauthorized || r.Code != "RZ-AUTH-020" || !r.Generated || r.Stream != cb {
		t.Errorf("Reply() of a leg-generated response = %+v", r)
	}
	if v := y.Exchange().Vars().Upstream; v == nil || v.Name != "orders" {
		t.Errorf("upstream = %+v", v)
	}
}

// TestTeeResponse checks filter.Exchange.TeeResponse: in onResponse it
// starts a copy the handler feeds (Tees); it is nil, counting
// ruralz_cache_store_skipped_total{reason="size_limit"} on the calling
// Policy, when the response's known length is over the limit, nil outside
// onResponse, on a leg and without a budget; a replaced response's copy is
// never stored.
func TestTeeResponse(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	u, _ := upstreamReply(http.StatusOK, "0123456789")
	x.SetResponse(u)
	cache := testPolicy("cache", 0, phase.ClassCache, phase.ScopeRoute, 0)
	skipped := &counterFake{}
	cache.Metrics = &emit.PolicyMetrics{}
	cache.Metrics.StoreSkipped[emit.SkipSizeLimit] = skipped
	x.Enter(cache, phase.OnRequestHeaders, nil)
	if tee := x.Exchange().TeeResponse(100); tee != nil {
		t.Error("TeeResponse() outside onResponse")
	}
	x.Enter(cache, phase.OnResponse, nil)
	if tee := x.Exchange().TeeResponse(5); tee != nil || skipped.n != 1 {
		t.Errorf("TeeResponse(5) of a 10-byte response = %v, skipped %d", tee, skipped.n)
	}
	tee := x.Exchange().TeeResponse(100)
	if tee == nil || len(x.Tees()) != 1 {
		t.Fatal("TeeResponse(100) started no tee")
	}
	t0 := x.Tees()[0]
	_, _ = t0.Write([]byte("0123456789"))
	t0.Complete()
	if b, ok := tee.Result(); !ok || string(b) != "0123456789" {
		t.Errorf("Result() = %q, %v", b, ok)
	}
	l := x.Leg("orders", "")
	defer l.Release()
	if l.Exchange().TeeResponse(100) != nil {
		t.Error("a leg view started a tee")
	}

	nb := NewPool(Deps{})
	y := nb.Get()
	y.Begin(serverRequest(http.MethodGet, "/", "", nil), start("/"))
	defer nb.Put(y)
	y.SetResponse(u)
	responder(y)
	if y.Exchange().TeeResponse(100) != nil {
		t.Error("TeeResponse() without a budget")
	}
}

// TestRateLimitFields checks filter.Exchange.AddRateLimitField: the client
// records the applied limits in order for the handler (RateLimitFields);
// a leg view records nothing.
func TestRateLimitFields(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	a := filter.RateLimitField{Name: "global", Q: 1000, W: 1}
	b := filter.RateLimitField{Name: "gold.1", Q: 100, W: 1, R: 0, T: 1, Known: true}
	x.Exchange().AddRateLimitField(a)
	l := x.Leg("u", "")
	l.Exchange().AddRateLimitField(filter.RateLimitField{Name: "leg"})
	l.Release()
	x.Exchange().AddRateLimitField(b)
	if got := x.RateLimitFields(); !reflect.DeepEqual(got, []filter.RateLimitField{a, b}) {
		t.Errorf("RateLimitFields() = %+v", got)
	}
}

// counterFake counts Adds.
type counterFake struct{ n uint64 }

func (c *counterFake) Add(_ emit.Stripe, n uint64) { c.n += n }
