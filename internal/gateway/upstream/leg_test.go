// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"errors"
	"io"
	"net/http"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestLegForwardsRequest checks the outgoing request and the response of
// one attempt (docs/architecture/03-data-plane.md "Upstream layer" and
// "Listeners and protocols"): the method, the normalized path sent
// unchanged with the raw query, the Endpoint authority as Host, the client
// header without hop-by-hop fields, Expect or the fields Connection names,
// the forwarding fields overwritten for an untrusted peer, the injected
// traceparent; the response returned unchanged without its hop-by-hop
// fields, one attempt counted, one span and every leg Phase in order.
func TestLegForwardsRequest(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "X-Up-Private")
		w.Header().Set("X-Up-Private", "secret")
		w.Header().Set("X-Up", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hello")
	})
	f := newFixture(t)
	met := newMetrics()
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr), Metrics: met.m})
	x := exchange(http.MethodGet, "/v1/orders/a%2Fb")
	x.Msg = &filtertest.Message{Query: "x=1&y=%20"}
	x.Hdr.Set("Connection", "keep-alive, X-Client-Hop")
	x.Hdr.Set("X-Client-Hop", "1")
	x.Hdr.Set("Keep-Alive", "timeout=5")
	x.Hdr.Set("Proxy-Authorization", "Basic Zm9v")
	x.Hdr.Set("Expect", "100-continue")
	x.Hdr.Set("Te", "trailers, deflate")
	x.Hdr.Set("X-Forwarded-For", "198.51.100.1")
	x.Hdr.Set("Forwarded", "for=198.51.100.1")
	h := &hooks{}

	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(x, h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if got := readAll(t, r); got != "hello" {
		t.Errorf("body = %q, want hello", got)
	}
	if r.Status != http.StatusOK || r.Upstream != "orders" || r.Endpoint != srv.addr || r.Attempts != 1 || r.Generated != nil {
		t.Errorf("response = %d %q %q %d %v, want 200 orders %s 1 nil", r.Status, r.Upstream, r.Endpoint, r.Attempts, r.Generated, srv.addr)
	}
	if r.Header.Get("X-Up") != "1" || r.Header.Get("X-Up-Private") != "" || r.Header.Get("Connection") != "" {
		t.Errorf("response header = %v, want X-Up without the Connection-named field", r.Header)
	}

	got := srv.last()
	if got.Method != http.MethodGet || got.RequestURI != "/v1/orders/a%2Fb?x=1&y=%20" || got.Host != srv.addr {
		t.Errorf("request line = %s %s Host %s, want GET /v1/orders/a%%2Fb?x=1&y=%%20 Host %s", got.Method, got.RequestURI, got.Host, srv.addr)
	}
	for _, name := range []string{"Keep-Alive", "Proxy-Authorization", "Expect", "X-Client-Hop", "Forwarded"} {
		if v := got.Header.Get(name); v != "" {
			t.Errorf("forwarded %s: %q, want none", name, v)
		}
	}
	want := map[string]string{
		"Accept":            "application/json",
		"User-Agent":        "client/1.0",
		"Te":                "trailers",
		"X-Forwarded-For":   "203.0.113.7",
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "api.shop.example",
		"Traceparent":       injected,
	}
	for name, v := range want {
		if g := got.Header.Get(name); g != v {
			t.Errorf("forwarded %s = %q, want %q", name, g, v)
		}
	}
	if a := met.attempts.list(); !slices.Equal(a, []string{"200"}) {
		t.Errorf("attempts = %v, want [200]", a)
	}
	if met.duration.n.Load() != 1 {
		t.Errorf("attempt durations recorded = %d, want 1", met.duration.n.Load())
	}
	spans := f.tracer.list()
	if len(spans) != 1 || spans[0].attrs.Attempt != 1 || spans[0].attrs.Endpoint != srv.addr || spans[0].status != 200 || spans[0].errType != "" {
		t.Errorf("spans = %+v, want one attempt 1 to %s with status 200", spans, srv.addr)
	}
	wantEvents := []string{`begin orders ""`, "request 1 " + srv.addr, "headers 1 200", "body 200", "end"}
	if ev := h.list(); !slices.Equal(ev, wantEvents) {
		t.Errorf("leg Phases = %q, want %q", ev, wantEvents)
	}
}

// TestLegAddsNoUserAgent checks that a client request without User-Agent
// reaches the Upstream without one: net/http's default is not added.
func TestLegAddsNoUserAgent(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, ""))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	x := exchange(http.MethodGet, "/")
	x.Hdr.Del("User-Agent")
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(x, nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if _, ok := srv.last().Header["User-Agent"]; ok {
		t.Errorf("User-Agent = %q, want none", srv.last().Header.Get("User-Agent"))
	}
}

// TestLegForwardsBody checks that a gated request body is sent with its
// length.
func TestLegForwardsBody(t *testing.T) {
	var got []byte
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	})
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	o := outbound(exchange(http.MethodPost, "/orders"), nil)
	o.Body = gatedBody(t, "payload")
	r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if string(got) != "payload" || srv.last().ContentLength != 7 {
		t.Errorf("Upstream got %q with Content-Length %d, want payload with 7", got, srv.last().ContentLength)
	}
}

// TestLegTargetOverridesMethodAndPath checks a composition step's leg: its
// method, path and query replace the client's, and the step names the leg
// Phases and the span.
func TestLegTargetOverridesMethodAndPath(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, ""))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	h := &hooks{}
	tgt := Target{Step: "order", Method: http.MethodPost, Path: "/orders/42", RawQuery: "a=1"}
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/v1/x"), h), tgt)
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if got := srv.last(); got.Method != http.MethodPost || got.RequestURI != "/orders/42?a=1" {
		t.Errorf("request = %s %s, want POST /orders/42?a=1", got.Method, got.RequestURI)
	}
	if ev := h.list(); len(ev) == 0 || ev[0] != `begin orders "order"` {
		t.Errorf("leg Phases = %q, want to begin with the step", ev)
	}
	if sp := f.tracer.list(); len(sp) != 1 || sp[0].attrs.Step != "order" {
		t.Errorf("spans = %+v, want one with step order", sp)
	}
}

// TestLegRetriesConnectErrorElsewhere checks that a connect error is
// retried (default retryOn, any method) on an Endpoint the leg has not
// tried, counting the retry and both attempts.
func TestLegRetriesConnectErrorElsewhere(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, "ok"))
	const refused = "127.0.0.1:1"
	f := newFixture(t, refused)
	met := newMetrics()
	s := f.compile(nil, Config{Upstream: newUpstream("orders", refused, srv.addr), Metrics: met.m})
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodPost, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if got := readAll(t, r); got != "ok" || r.Attempts != 2 || r.Endpoint != srv.addr {
		t.Errorf("response %q after %d attempts from %s, want ok after 2 from %s", got, r.Attempts, r.Endpoint, srv.addr)
	}
	if a := met.attempts.list(); !slices.Equal(a, []string{"connect", "200"}) {
		t.Errorf("attempts = %v, want [connect 200]", a)
	}
	if met.retries.get() != 1 || f.dial.count(refused) != 1 {
		t.Errorf("retries = %d, dials to the refused Endpoint = %d, want 1 and 1", met.retries.get(), f.dial.count(refused))
	}
}

// TestLegCodeSelection checks the code of a leg that ends without a
// response (docs/architecture/09-traffic-management-and-resilience.md
// "Error codes this document owns"): the last kind without a retry
// (RZ-UP-001, RZ-UP-004), RZ-UP-007 naming the kind after a retry, and a
// reset retried only for an idempotent method.
func TestLegCodeSelection(t *testing.T) {
	reset := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no Hijacker")
			return
		}
		c, _, err := hj.Hijack()
		if err == nil {
			_ = c.Close()
		}
	})
	const r1, r2 = "127.0.0.1:1", "127.0.0.1:2"
	tests := []struct {
		name     string
		eps      []string
		method   string
		attempts int32
		code     string
		kind     resilience.Kind
		tries    int
	}{
		{"connect without retry", []string{r1}, http.MethodGet, 0, "RZ-UP-001", resilience.KindConnect, 1},
		{"connect after a retry", []string{r1, r2}, http.MethodGet, 1, "RZ-UP-007", resilience.KindConnect, 2},
		{"reset on POST", []string{reset.addr}, http.MethodPost, 1, "RZ-UP-004", resilience.KindReset, 1},
		{"reset retried on GET", []string{reset.addr}, http.MethodGet, 1, "RZ-UP-007", resilience.KindReset, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, r1, r2)
			u := newUpstream("orders", tc.eps...)
			u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr(tc.attempts)}
			s := f.compile(nil, Config{Upstream: u})
			_, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(tc.method, "/"), nil), Target{})
			var le *LegError
			if !errors.As(err, &le) {
				t.Fatalf("Leg error = %v, want a *LegError", err)
			}
			if codeOf(err) != tc.code || le.Kind != tc.kind || le.Attempts != tc.tries {
				t.Errorf("Leg = %s kind %v after %d attempts, want %s kind %v after %d", codeOf(err), le.Kind, le.Attempts, tc.code, tc.kind, tc.tries)
			}
			var ke *resilience.KindError
			if !errors.As(err, &ke) || ke.Kind != tc.kind {
				t.Errorf("KindError = %v, want kind %v", ke, tc.kind)
			}
		})
	}
}

// TestLegRetries503ForIdempotentMethods checks the default retryOn on a
// response: status 503 is retried for GET, HEAD, OPTIONS, PUT and DELETE
// only; other methods return the 503 unchanged.
func TestLegRetries503ForIdempotentMethods(t *testing.T) {
	for _, tc := range []struct {
		method   string
		status   int
		attempts int
	}{
		{http.MethodGet, 200, 2},
		{http.MethodDelete, 200, 2},
		{http.MethodPost, 503, 1},
		{http.MethodPatch, 503, 1},
	} {
		t.Run(tc.method, func(t *testing.T) {
			var n int
			srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				n++
				if n == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, "busy")
					return
				}
				w.WriteHeader(http.StatusOK)
			})
			f := newFixture(t)
			s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
			r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(tc.method, "/"), nil), Target{})
			if err != nil {
				t.Fatalf("Leg: %v", err)
			}
			body := readAll(t, r)
			if r.Status != tc.status || r.Attempts != tc.attempts {
				t.Errorf("Leg = %d after %d attempts, want %d after %d", r.Status, r.Attempts, tc.status, tc.attempts)
			}
			if tc.status == 503 && body != "busy" {
				t.Errorf("returned 503 body = %q, want it unchanged (busy)", body)
			}
		})
	}
}

// TestLegReturnsLastResponse checks that a leg whose retries ran out
// returns the last attempt's response unchanged.
func TestLegReturnsLastResponse(t *testing.T) {
	srv := newServer(t, status(http.StatusServiceUnavailable, "still busy"))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if body := readAll(t, r); r.Status != 503 || body != "still busy" || r.Attempts != 2 {
		t.Errorf("Leg = %d %q after %d attempts, want 503 still busy after 2", r.Status, body, r.Attempts)
	}
}

// TestLegNotReplayableIsNotRetried checks the replay rule: a streamed body
// the first attempt read is not sent again, so the 503 returns.
func TestLegNotReplayableIsNotRetried(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	o := outbound(exchange(http.MethodPut, "/"), nil)
	o.Body = streamBody("streamed")
	r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if r.Status != 503 || r.Attempts != 1 || srv.hits.Load() != 1 {
		t.Errorf("Leg = %d after %d attempts and %d requests, want 503 after 1 and 1", r.Status, r.Attempts, srv.hits.Load())
	}
}

// TestLegRouteDeadlinePassed checks RZ-RT-007: the Route deadline passed
// before any attempt, so nothing is sent and no leg Phase runs.
func TestLegRouteDeadlinePassed(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, ""))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	h := &hooks{}
	o := outbound(exchange(http.MethodGet, "/"), h)
	o.Deadline = epoch()
	_, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
	if codeOf(err) != "RZ-RT-007" || srv.hits.Load() != 0 || len(h.list()) != 0 {
		t.Errorf("Leg = %v with %d requests and Phases %q, want RZ-RT-007, none and none", err, srv.hits.Load(), h.list())
	}
}

// TestLegEmptySet checks RZ-UP-008 for an Upstream without Endpoints.
func TestLegEmptySet(t *testing.T) {
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders")})
	_, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if codeOf(err) != "RZ-UP-008" {
		t.Errorf("Leg = %v, want RZ-UP-008", err)
	}
}

// TestLegShortCircuitInOnUpstreamRequest checks that a leg Filter's
// response in onUpstreamRequest ends the leg without sending the attempt,
// and the leg's Phases end once.
func TestLegShortCircuitInOnUpstreamRequest(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, ""))
	f := newFixture(t)
	met := newMetrics()
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr), Metrics: met.m})
	gen := &filter.Response{Status: http.StatusUnauthorized, Code: "RZ-AUTH-020"}
	h := &hooks{onRequest: func(*filter.Leg) *filter.Response { return gen }}
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if r.Generated != gen || r.Status != http.StatusUnauthorized || srv.hits.Load() != 0 {
		t.Errorf("Leg = %+v with %d requests, want the generated 401 and none", r, srv.hits.Load())
	}
	want := []string{`begin orders ""`, "request 1 " + srv.addr, "end"}
	if ev := h.list(); !slices.Equal(ev, want) {
		t.Errorf("leg Phases = %q, want %q", ev, want)
	}
	if st := f.upstream(s, "orders").rt.res.Bulkhead().InFlight(); st != 0 || len(met.attempts.list()) != 0 {
		t.Errorf("bulkhead in flight = %d, attempts = %v, want 0 and none", st, met.attempts.list())
	}
}

// TestLegResponseHeadersReplace checks that a replacement from
// onUpstreamResponseHeaders (a failed response Phase) is final: no retry,
// the Upstream body closed and the slot released.
func TestLegResponseHeadersReplace(t *testing.T) {
	srv := newServer(t, status(http.StatusServiceUnavailable, "busy"))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	rep := &filter.Response{Status: http.StatusBadGateway, Code: "RZ-RT-012"}
	h := &hooks{onHeaders: func(*filter.Leg) (bool, *filter.Response) { return false, rep }}
	u := f.upstream(s, "orders")
	r, err := u.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if r.Generated != rep || r.Attempts != 1 || srv.hits.Load() != 1 || u.rt.res.Bulkhead().InFlight() != 0 {
		t.Errorf("Leg = %+v with %d requests and %d slots held, want the replacement, 1 and 0", r, srv.hits.Load(), u.rt.res.Bulkhead().InFlight())
	}
}

// TestLegFilterRetry checks a Filter's retry request in
// onUpstreamResponseHeaders: retryOn is not consulted, so a POST 200 is
// retried, while every other condition (here a replayable body) still
// applies.
func TestLegFilterRetry(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, "ok"))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	h := &hooks{onHeaders: func(leg *filter.Leg) (bool, *filter.Response) { return leg.Attempt == 1, nil }}
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodPost, "/"), h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if r.Attempts != 2 || srv.hits.Load() != 2 {
		t.Errorf("Leg after %d attempts and %d requests, want 2 and 2", r.Attempts, srv.hits.Load())
	}
	want := []string{`begin orders ""`, "request 1 " + srv.addr, "headers 1 200", "request 2 " + srv.addr, "headers 2 200", "body 200", "end"}
	if ev := h.list(); !slices.Equal(ev, want) {
		t.Errorf("leg Phases = %q, want %q", ev, want)
	}

	// A streamed body the first attempt read cannot be sent again.
	read := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	s2 := f.compile(nil, Config{Upstream: newUpstream("other", read.addr)})
	o := outbound(exchange(http.MethodPost, "/"), &hooks{onHeaders: func(*filter.Leg) (bool, *filter.Response) { return true, nil }})
	o.Body = streamBody("once")
	r, err = f.upstream(s2, "other").Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if r.Attempts != 1 {
		t.Errorf("Leg with a read stream after %d attempts, want 1", r.Attempts)
	}
}

// TestLegDurationExemplar checks ruralz_upstream_attempt_duration_seconds:
// one record per attempt when its body ends, with an exemplar of the
// correlation key (trace ID, server span ID) for a sampled request only.
func TestLegDurationExemplar(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, "ok"))
	f := newFixture(t)
	met := newMetrics()
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr), Metrics: met.m})
	up := f.upstream(s, "orders")
	d := &emit.Decision{TraceID: [16]byte{1, 2, 3}, ServerSpanID: [8]byte{4, 5, 6}}
	for _, sampled := range []bool{false, true} {
		d.Sampled = sampled
		o := outbound(exchange(http.MethodGet, "/"), nil)
		o.Trace = d
		r, err := up.Leg(t.Context(), o, Target{})
		if err != nil {
			t.Fatalf("Leg: %v", err)
		}
		if met.duration.n.Load() != map[bool]int64{false: 0, true: 1}[sampled] {
			t.Errorf("durations before the body ended = %d", met.duration.n.Load())
		}
		readAll(t, r)
	}
	ex := met.duration.exemplars()
	if met.duration.n.Load() != 2 || len(ex) != 1 || ex[0].TraceID != d.TraceID || ex[0].SpanID != d.ServerSpanID {
		t.Errorf("durations %d, exemplars %+v; want 2 and one with the trace and server span IDs", met.duration.n.Load(), ex)
	}
}
