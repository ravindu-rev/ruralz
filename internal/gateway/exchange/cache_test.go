// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for what the exchange keeps and does for the Response Cache
// (docs/architecture/09-traffic-management-and-resilience.md "Response
// caching"; docs/architecture/03-data-plane.md "Short-circuit"): the
// request as received, the leg response kept for the store, its final
// status, responses the cache serves, and the tee of the leg's body.

// teeRoute is a Route that tees, and gates when gate is set.
func teeRoute(gate bool) *snapshot.Route {
	return testRoute("cached", snapshot.BodyNeeds{ResponseTee: true, ResponseGate: gate})
}

// cachePolicy enters the cache Policy in ph, counting its skipped stores.
func cachePolicy(x *Request, ph phase.Phase) (size, budget *counterFake) {
	p := testPolicy("cache", 1, phase.ClassCache, phase.ScopeRoute, 1)
	p.Metrics = &emit.PolicyMetrics{}
	size, budget = &counterFake{}, &counterFake{}
	p.Metrics.StoreSkipped[emit.SkipSizeLimit] = size
	p.Metrics.StoreSkipped[emit.SkipBufferBudget] = budget
	x.Enter(p, ph, nil)
	return size, budget
}

func TestReceived(t *testing.T) {
	// Received is the client request as it arrived: the Host it named,
	// port included, its raw query even after a Policy rewrote the query,
	// and its declared body length, -1 for a body of unknown length; a
	// leg view sees the same.
	f := newFixture(t)
	r := serverRequest(http.MethodPost, "/v1/items?b=2&a=%41", `{"x":1}`, nil)
	r.Host = "API.example.com:8443"
	x := f.routed(r, testRoute("r", snapshot.BodyNeeds{}))
	defer f.pool.Put(x)
	x.Enter(testPolicy("tr", 0, phase.ClassTransform, phase.ScopeRoute, 0), phase.OnRequestBody, nil)
	x.Exchange().Message().SetRawQuery("rewritten=1")
	want := filter.Received{Authority: "API.example.com:8443", RawQuery: "b=2&a=%41", ContentLength: 7}
	if got := x.Exchange().Received(); got != want {
		t.Fatalf("Received() = %+v, want %+v", got, want)
	}
	l := x.Leg("orders", "")
	defer l.Release()
	if got := l.Exchange().Received(); got != want {
		t.Fatalf("leg Received() = %+v, want %+v", got, want)
	}
	chunked := serverRequest(http.MethodGet, "/", "", nil)
	chunked.ContentLength, chunked.Body = -1, io.NopCloser(strings.NewReader("x"))
	y := f.begin(chunked)
	defer f.pool.Put(y)
	if got := y.Exchange().Received().ContentLength; got != -1 {
		t.Fatalf("Received().ContentLength of a chunked body = %d, want -1", got)
	}
}

func TestLegResponseKeptForTheStore(t *testing.T) {
	// On a Route that tees, the leg response's status and a copy of its
	// header taken before onResponse are kept: edits by onResponse
	// Policies never reach the copy, and Final.LegStatus reports the
	// status even when the client went away before commit. A Route that
	// does not tee keeps the status only; a leg Filter's generated
	// response is no leg response.
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodPost, "/", "", nil), teeRoute(false))
	defer f.pool.Put(x)
	u, _ := upstreamReply(http.StatusCreated, "0123456789")
	u.Header.Set("Cache-Control", "max-age=60")
	x.SetResponse(u)
	responder(x)
	x.Exchange().Message().Header().Set("Cache-Control", "no-store")
	x.Exchange().Message().Header().Set("X-Added", "1")
	status, h := x.Exchange().LegResponse()
	if status != http.StatusCreated || h.Get("Cache-Control") != "max-age=60" || h.Get("X-Added") != "" {
		t.Fatalf("LegResponse() = %d %v, want 201 with the leg's own header", status, h)
	}
	x.SetFinal(filter.Final{Status: 0}, time.Second)
	x.Enter(testPolicy("cache", 1, phase.ClassCache, phase.ScopeRoute, 1), phase.OnLog, nil)
	if fin := x.Exchange().Final(); fin.LegStatus != http.StatusCreated || fin.Status != 0 {
		t.Fatalf("Final() = %+v, want leg status 201 with no client status", fin)
	}

	plain := f.routed(serverRequest(http.MethodGet, "/", "", nil), testRoute("r", snapshot.BodyNeeds{}))
	defer f.pool.Put(plain)
	plain.SetResponse(u)
	if status, h := plain.Exchange().LegResponse(); status != http.StatusCreated || h != nil {
		t.Fatalf("LegResponse() on a Route that does not tee = %d %v, want 201 without a header", status, h)
	}
	gen := f.routed(serverRequest(http.MethodGet, "/", "", nil), teeRoute(false))
	defer f.pool.Put(gen)
	gen.SetResponse(&snapshot.UpstreamResponse{Generated: &filter.Response{Status: 401, Code: "RZ-AUTH-020"}, Upstream: "orders"})
	if status, _ := gen.Exchange().LegResponse(); status != 0 || gen.Exchange().Final().LegStatus != 0 {
		t.Fatalf("a leg Filter's generated response reported leg status %d", status)
	}
	l := x.Leg("orders", "")
	defer l.Release()
	if status, h := l.Exchange().LegResponse(); status != 0 || h != nil {
		t.Fatal("a leg view reported a leg response")
	}
}

func TestCachedResponseIsNoGeneratedResponse(t *testing.T) {
	// A response the Response Cache serves is no generated response: its
	// message reports Generated false, its literal body is the body
	// Policies read and rewrite and CEL decodes, the reply is Cached and
	// buffered, and it gets no tee.
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/", "", nil), teeRoute(true))
	defer f.pool.Put(x)
	x.SetGenerated(&filter.Response{
		Status: http.StatusOK, Body: []byte(`{"n":1}`), Cached: true, Code: "RZ-RT-" + "999",
		Header: http.Header{"Content-Type": {"application/json"}},
	})
	responder(x)
	m := x.Exchange().Message()
	if m.Generated() {
		t.Fatal("a Cached response reports Generated")
	}
	if b, err := m.Body(context.Background()); err != nil || string(b) != `{"n":1}` {
		t.Fatalf("Body() = %q, %v", b, err)
	}
	if v, err := m.Decoded(context.Background()); err != nil || v == nil {
		t.Fatalf("Decoded() = %v, %v; want the stored JSON", v, err)
	}
	cachePolicy(x, phase.OnResponse)
	if x.Exchange().TeeResponse(1<<20) != nil {
		t.Fatal("a Cached response got a tee")
	}
	responder(x)
	if err := m.SetBody(context.Background(), []byte(`{"n":2}`)); err != nil {
		t.Fatalf("SetBody: %v", err)
	}
	rep := x.Reply()
	if !rep.Cached || rep.Generated || rep.Code != "" || !rep.Buffered || string(rep.Body) != `{"n":2}` {
		t.Fatalf("Reply() = %+v", rep)
	}
}

func TestTeeCopiesTheLegBody(t *testing.T) {
	// The tee holds the body the leg delivered, whatever an earlier
	// onResponse Policy wrote: a gated body fills it at once, a copy is
	// kept when a Policy replaced the gated body before the cache ran,
	// and a tee already filled survives a later replacement.
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/", "", nil), teeRoute(true))
	defer f.pool.Put(x)
	u, _ := upstreamReply(http.StatusOK, "0123456789")
	x.SetResponse(u)
	if err := x.GateResponse(context.Background()); err != nil {
		t.Fatal(err)
	}
	responder(x)
	if err := x.Exchange().Message().SetBody(context.Background(), []byte("transformed")); err != nil {
		t.Fatal(err)
	}
	size, _ := cachePolicy(x, phase.OnResponse)
	tee := x.Exchange().TeeResponse(100)
	if tee == nil {
		t.Fatal("no tee")
	}
	if b, ok := tee.Result(); !ok || string(b) != "0123456789" {
		t.Fatalf("tee Result() = %q, %v; want the leg's body", b, ok)
	}
	if x.Exchange().TeeResponse(5) != nil || size.n != 1 {
		t.Fatalf("a 10-byte leg body teed under a 5-byte limit (size_limit %d)", size.n)
	}
	responder(x)
	if err := x.Exchange().Message().SetBody(context.Background(), []byte("again")); err != nil {
		t.Fatal(err)
	}
	if b, ok := tee.Result(); !ok || string(b) != "0123456789" {
		t.Fatalf("a filled tee lost its copy to a later replacement: %q, %v", b, ok)
	}
	if rep := x.Reply(); string(rep.Body) != "again" {
		t.Fatalf("client body %q", rep.Body)
	}
}

func TestTeeLegBodyLost(t *testing.T) {
	// A streamed leg body that a Policy replaced whole is never written,
	// so the store has nothing to copy (no tee, no skip counted), and a
	// gated one whose copy the budget refused is a buffer_budget skip.
	f := newFixture(t)
	x := f.routed(serverRequest(http.MethodGet, "/", "", nil), teeRoute(false))
	defer f.pool.Put(x)
	u, _ := upstreamReply(http.StatusOK, "0123456789")
	x.SetResponse(u)
	responder(x)
	if err := x.Exchange().Message().SetBody(context.Background(), []byte("whole")); err != nil {
		t.Fatal(err)
	}
	size, budget := cachePolicy(x, phase.OnResponse)
	if x.Exchange().TeeResponse(100) != nil || size.n != 0 || budget.n != 0 {
		t.Fatalf("a replaced streamed body got a tee (size %d, budget %d)", size.n, budget.n)
	}

	// Four 32 KiB increments, one of them the stream share gates cannot
	// take: the response gate holds one and the request's account two, so
	// the copy of the leg's body finds no increment left.
	tight := newFixtureBudget(t, 128<<10)
	y := tight.routed(serverRequest(http.MethodGet, "/", "", nil), teeRoute(true))
	defer tight.pool.Put(y)
	v, _ := upstreamReply(http.StatusOK, "0123456789")
	y.SetResponse(v)
	if err := y.GateResponse(context.Background()); err != nil {
		t.Fatal(err)
	}
	responder(y)
	if err := y.Exchange().Reserve(64 << 10); err != nil {
		t.Fatal(err)
	}
	if err := y.Exchange().Message().SetBody(context.Background(), []byte("new")); err == nil {
		t.Fatal("SetBody with the budget spent succeeded")
	}
	_, budget = cachePolicy(y, phase.OnResponse)
	if y.Exchange().TeeResponse(1<<20) != nil || !y.legLostBudget || budget.n != 1 {
		t.Fatalf("lost to the budget %v, buffer_budget skips %d; want no tee, true and 1", y.legLostBudget, budget.n)
	}
}
