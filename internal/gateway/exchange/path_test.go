// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the forwarded path (filter.Message.Path and SetPath, the
// transform.request target path) and for transforms that run while the
// body streams on (docs/architecture/03-data-plane.md "Transform
// Policies").

// TestRequestPathRewrite checks SetPath on the client request: the
// forwarded path and CEL request.path, which later Policies and the legs
// issued after it read, take the rewritten path, while Exchange.Path and
// the access record keep the client's.
func TestRequestPathRewrite(t *testing.T) {
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/v1/orders/42?a=1", "", nil), testRoute("orders", snapshot.BodyNeeds{}))
	defer f.pool.Put(x)
	x.Enter(testPolicy("tr", 0, phase.ClassTransform, phase.ScopeRoute, 0), phase.OnRequestBody, nil)
	m := x.Exchange().Message()
	if got := m.Path(); got != "/v1/orders/42" {
		t.Fatalf("Path() before a rewrite = %q, want the client's normalized path", got)
	}
	m.SetPath("/internal/orders/42")
	if got := m.Path(); got != "/internal/orders/42" {
		t.Errorf("Path() = %q, want the rewritten path", got)
	}
	if got := x.Exchange().Vars().Request.Path; got != "/internal/orders/42" {
		t.Errorf("CEL request.path = %q, want the rewritten path", got)
	}
	if got := x.Exchange().Path(); got != "/v1/orders/42" {
		t.Errorf("Exchange.Path() = %q, want the client's path", got)
	}
	l := x.Leg("orders", "")
	defer l.Release()
	if got := l.Exchange().Vars().Request.Path; got != "/internal/orders/42" {
		t.Errorf("leg request.path = %q, want the rewritten path", got)
	}
	var rec emit.AccessRecord
	x.SetFinal(filter.Final{Status: http.StatusOK}, 0)
	x.FillAccess(&rec)
	if rec.Path != "/v1/orders/42" {
		t.Errorf("access record path = %q, want the client's path", rec.Path)
	}
}

// TestLegRequestPath checks the leg's outgoing request: CEL request.path
// is the attempt's path, such as a composition step's own, SetPath
// rewrites the attempt's URL, keeping percent-encodings in its raw form,
// and a new attempt starts from its own request again.
func TestLegRequestPath(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/v1/orders", "", nil))
	defer f.pool.Put(x)
	l := x.Leg("inventory", "stock")
	defer l.Release()
	out := &http.Request{Header: http.Header{}, URL: &url.URL{Scheme: "http", Host: "10.0.0.1", Path: "/stock/7", RawPath: "/stock/7"}}
	l.SetLeg(&filter.Leg{Upstream: "inventory", Step: "stock", Attempt: 1, Request: out})
	l.Enter(legPolicy(), phase.OnUpstreamRequest, nil)
	m := l.Exchange().Message()
	if got := l.Exchange().Vars().Request.Path; got != "/stock/7" {
		t.Errorf("leg request.path = %q, want the attempt's /stock/7", got)
	}
	m.SetPath("/v2/stock/a%2Fb")
	if out.URL.RawPath != "/v2/stock/a%2Fb" || out.URL.Path != "/v2/stock/a/b" || out.URL.EscapedPath() != "/v2/stock/a%2Fb" {
		t.Errorf("outgoing URL = path %q raw %q, want the rewritten path kept encoded", out.URL.Path, out.URL.RawPath)
	}
	if m.Path() != "/v2/stock/a%2Fb" || l.Exchange().Vars().Request.Path != "/v2/stock/a%2Fb" {
		t.Errorf("leg Path() = %q, request.path = %q, want the rewritten path", m.Path(), l.Exchange().Vars().Request.Path)
	}
	if x.Exchange().Path() != "/v1/orders" || x.req.Path != "/v1/orders" {
		t.Error("a leg's rewrite reached the client request")
	}
	second := &http.Request{Header: http.Header{}, URL: &url.URL{Scheme: "http", Host: "10.0.0.2", Path: "/stock/7", RawPath: "/stock/7"}}
	l.SetLeg(&filter.Leg{Upstream: "inventory", Step: "stock", Attempt: 2, Request: second})
	l.Enter(legPolicy(), phase.OnUpstreamRequest, nil)
	if got := l.Exchange().Vars().Request.Path; got != "/stock/7" {
		t.Errorf("request.path of a new attempt = %q, want its own /stock/7", got)
	}
}

// TestResponseMessagesHaveNoPath checks that response messages report no
// path and ignore SetPath.
func TestResponseMessagesHaveNoPath(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/v1", "", nil))
	defer f.pool.Put(x)
	x.SetResponse(&snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{}})
	x.Enter(testPolicy("p", 0, phase.ClassTransform, phase.ScopeRoute, 0), phase.OnResponse, nil)
	m := x.Exchange().Message()
	m.SetPath("/elsewhere")
	if m.Path() != "" || x.req.Path != "/v1" {
		t.Errorf("response Path() = %q, client path %q", m.Path(), x.req.Path)
	}
	l := x.Leg("orders", "")
	defer l.Release()
	resp, _ := upstreamResponse(http.StatusOK, `{}`)
	defer func() { _ = resp.Body.Close() }()
	l.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 1, Response: resp})
	l.Enter(legPolicy(), phase.OnUpstreamResponseBody, nil)
	lm := l.Exchange().Message()
	lm.SetPath("/elsewhere")
	if lm.Path() != "" {
		t.Errorf("leg response Path() = %q, want none", lm.Path())
	}
}

// TestLegResponseStreamsWithoutBodyNeed checks onUpstreamResponseBody for
// a Policy that reads no body, such as a transform.response that edits
// only headers: its activation leaves the response body unread and
// response.body unset. A Policy whose spec.when reads response.body gets
// the body gated for it, and once a Filter gated the body, the activation
// decodes it.
func TestLegResponseStreamsWithoutBodyNeed(t *testing.T) {
	f := newFixture(t)
	x := f.begin(serverRequest(http.MethodGet, "/", "", nil))
	defer f.pool.Put(x)
	ctx := context.Background()

	l := x.Leg("orders", "")
	defer l.Release()
	resp, orig := upstreamResponse(http.StatusOK, `{"n":1}`)
	l.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 1, Response: resp})
	l.Enter(legPolicy(), phase.OnUpstreamResponseBody, nil)
	v := l.Exchange().Vars()
	if v.Response == nil || v.Response.Status != http.StatusOK || v.Response.Body != nil {
		t.Fatalf("activation response = %+v, want the status without a body", v.Response)
	}
	if resp.Body != orig || f.dec.n.Load() != 0 {
		t.Fatal("the activation gated or decoded a body no Policy needs")
	}
	// A Filter that needs the body reads it; the activation then decodes it.
	if b, err := l.Exchange().Message().Body(ctx); err != nil || string(b) != `{"n":1}` {
		t.Fatalf("Body() = %q, %v", b, err)
	}
	if got := nativeOf(t, l.Exchange().Vars().Response.Body); !reflect.DeepEqual(numbersAsText(got), map[string]any{"n": "1"}) {
		t.Errorf("response.body after the gate = %v", got)
	}

	// A spec.when that reads response.body gates the body itself.
	when := celtest.Program(t, expr.Site{
		Place: expr.PlacePolicyWhen, Scope: phase.ScopeUpstream, PolicyType: v1alpha1.PolicyTypeTransformResponse,
		FirstPhase: phase.OnUpstreamResponseBody,
	}, `response.body.n == 2`)
	p := legPolicy()
	p.When = when
	l2 := x.Leg("orders", "")
	defer l2.Release()
	resp2, orig2 := upstreamResponse(http.StatusOK, `{"n":2}`)
	l2.SetLeg(&filter.Leg{Upstream: "orders", Attempt: 1, Response: resp2})
	l2.Enter(p, phase.OnUpstreamResponseBody, nil)
	if got := nativeOf(t, l2.Exchange().Vars().Response.Body); !reflect.DeepEqual(numbersAsText(got), map[string]any{"n": "2"}) {
		t.Errorf("response.body for a when that reads it = %v", got)
	}
	if resp2.Body == orig2 {
		t.Error("the body a when reads was not gated")
	}
}
