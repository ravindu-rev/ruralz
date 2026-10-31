// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Test fixtures: a Pool over a real buffer budget and the real CEL
// decoder (counted), server-side requests, Routes, Policies and a span
// that records attributes.

// testStart is the request start of every test request.
func testStart() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.FixedZone("x", 3600)) }

// testTraceID is the trace ID of every test request.
func testTraceID() [16]byte {
	return [16]byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
}

// testStripe is the metric stripe of every test request.
const testStripe = 3

// testLimits are small effective limits, so tests reach them cheaply.
func testLimits() snapshot.Limits {
	return snapshot.Limits{MaxRequestBodyBytes: 1 << 10, MaxResponseBodyBytes: 2 << 10, MaxBufferedBytes: 1 << 20}
}

// countingDecoder counts DecodeJSON calls.
type countingDecoder struct {
	d Decoder
	n atomic.Int64
}

func (c *countingDecoder) DecodeJSON(b []byte, budget int64) (expr.Value, int64, error) {
	c.n.Add(1)
	return c.d.DecodeJSON(b, budget)
}

// fixture is a Pool over a 1 MiB budget and the real decoder.
type fixture struct {
	pool *Pool
	bud  *body.Budget
	dec  *countingDecoder
}

func newFixture(tb testing.TB) *fixture {
	tb.Helper()
	return newFixtureBudget(tb, 1<<20)
}

func newFixtureBudget(tb testing.TB, total int64) *fixture {
	tb.Helper()
	bud := body.NewBudget(total, nil)
	dec := &countingDecoder{d: celtest.Compiler(tb)}
	return &fixture{pool: NewPool(Deps{Budget: bud, Decoder: dec}), bud: bud, dec: dec}
}

// serverRequest returns a request as net/http hands it to a handler:
// Host and URL from target, ContentLength from the body.
func serverRequest(method, target, payload string, h http.Header) *http.Request {
	u, err := url.ParseRequestURI(target)
	if err != nil {
		panic(err)
	}
	r := &http.Request{Method: method, URL: u, Host: "api.example.com", Header: http.Header{}, Body: http.NoBody}
	for k, vs := range h {
		r.Header[k] = vs
	}
	if payload != "" {
		r.Body = io.NopCloser(strings.NewReader(payload))
		r.ContentLength = int64(len(payload))
	}
	return r
}

// jsonHeader is a request or response header naming a JSON body.
func jsonHeader() http.Header { return http.Header{"Content-Type": {"application/json"}} }

// start returns the Start of a test request on path.
func start(path string) *Start {
	return &Start{
		TraceID: testTraceID(), Time: testStart(), Listener: "public", Scheme: "https", Path: path,
		Source: filter.Source{IP: netip.MustParseAddr("192.0.2.7"), Port: 51000, Peer: netip.MustParseAddrPort("192.0.2.7:51000")},
		TLS:    &filter.ConnTLS{State: &tls.ConnectionState{Version: tls.VersionTLS13}},
		Stripe: testStripe, Limits: testLimits(),
	}
}

// begin returns a started Request for r.
func (f *fixture) begin(r *http.Request) *Request {
	x := f.pool.Get()
	x.Begin(r, start(r.URL.Path))
	return x
}

// routed returns a started Request for r on rt.
func (f *fixture) routed(r *http.Request, rt *snapshot.Route) *Request {
	x := f.begin(r)
	x.SetRoute(rt, nil)
	return x
}

// testRoute returns a compiled Route with body needs.
func testRoute(name string, needs snapshot.BodyNeeds) *snapshot.Route {
	return &snapshot.Route{
		Name: name, View: &expr.Route{Name: name}, Protocol: v1alpha1.UpstreamProtocolHTTP,
		Timeout: 15 * time.Second, StateStoreMax: 20 * time.Millisecond, Body: needs,
	}
}

// testPolicy returns a compiled Policy at index in request order position.
func testPolicy(name string, index int, class phase.Class, scope phase.Scope, position int) *snapshot.Policy {
	return &snapshot.Policy{
		Index: index, Name: name, Class: class, Scope: scope, Position: position,
		FailureMode: v1alpha1.FailureModeClosed,
	}
}

// recSpan records attributes.
type recSpan struct {
	mu    sync.Mutex
	attrs map[string]string
}

func (s *recSpan) SetAttr(k string, v slog.Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attrs == nil {
		s.attrs = map[string]string{}
	}
	s.attrs[k] = v.String()
}

func (*recSpan) SpanID() [8]byte         { return [8]byte{} }
func (*recSpan) End(int, string, string) {}

func (s *recSpan) attr(k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attrs[k]
}

// closeBody is an upstream body that records Close.
type closeBody struct {
	io.Reader
	closed atomic.Int32
}

func (b *closeBody) Close() error {
	b.closed.Add(1)
	return nil
}

// upstreamResponse returns an attempt's response with a JSON body.
func upstreamResponse(status int, payload string) (*http.Response, *closeBody) {
	b := &closeBody{Reader: strings.NewReader(payload)}
	h := jsonHeader()
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	return &http.Response{StatusCode: status, Header: h, Body: b, ContentLength: int64(len(payload))}, b
}

// outgoing returns an attempt's outgoing request, as the Upstream layer
// builds it from the Outbound.
func outgoing(tb testing.TB, x *Request) *http.Request {
	tb.Helper()
	rd, err := x.RequestBody().Open()
	if err != nil {
		tb.Fatalf("Open() = %v", err)
	}
	u := &url.URL{Scheme: "http", Host: "10.0.0.1:8080", Path: x.path, RawQuery: x.req.RawQuery()}
	h := x.hdr.Clone()
	return &http.Request{Method: x.req.Method, URL: u, Header: h, Body: rd, ContentLength: x.RequestBody().ContentLength()}
}

// readAll reads rd whole; it stops the test on an error.
func readAll(tb testing.TB, rd io.Reader) string {
	tb.Helper()
	b, err := io.ReadAll(rd)
	if err != nil {
		tb.Fatalf("ReadAll() = %v", err)
	}
	return string(b)
}

// nativeOf returns v as encoding/json data, nil for nil.
func nativeOf(tb testing.TB, v expr.Value) any {
	tb.Helper()
	if v == nil {
		return nil
	}
	n, err := v.Native()
	if err != nil {
		tb.Fatalf("Native() = %v", err)
	}
	return n
}
