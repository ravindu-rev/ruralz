// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package exchange

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// gateRoute gates the request body.
func gateRoute() *snapshot.Route {
	return testRoute("orders", snapshot.BodyNeeds{RequestGate: true})
}

// gatedRequest returns a routed Request whose body PrepareBody read.
func (f *fixture) gatedRequest(tb testing.TB, payload string, h http.Header) *Request {
	tb.Helper()
	x := f.routed(serverRequest(http.MethodPost, "/v1/orders", payload, h), gateRoute())
	if err := x.PrepareBody(context.Background()); err != nil {
		tb.Fatalf("PrepareBody() = %v", err)
	}
	return x
}

// TestRequestBodyAvailability checks filter.Message.Body on the client
// request: the gated body in onRequestBody and onRoute, nil for an empty
// one; filter.ErrNotAvailable in onRequestHeaders and for a streamed body.
func TestRequestBodyAvailability(t *testing.T) {
	p := testPolicy("p", 0, phase.ClassValidation, phase.ScopeRoute, 0)
	for _, tc := range []struct {
		name    string
		gate    bool
		payload string
		ph      phase.Phase
		want    string
		err     error
	}{
		{"gated onRequestBody", true, `{"a":1}`, phase.OnRequestBody, `{"a":1}`, nil},
		{"gated onRoute", true, `{"a":1}`, phase.OnRoute, `{"a":1}`, nil},
		{"gated onRequestHeaders", true, `{"a":1}`, phase.OnRequestHeaders, "", filter.ErrNotAvailable},
		{"gated empty", true, "", phase.OnRequestBody, "", nil},
		{"streamed", false, `{"a":1}`, phase.OnRequestBody, "", filter.ErrNotAvailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			rt := testRoute("r", snapshot.BodyNeeds{RequestGate: tc.gate})
			x := f.routed(serverRequest(http.MethodPost, "/", tc.payload, jsonHeader()), rt)
			defer f.pool.Put(x)
			if err := x.PrepareBody(context.Background()); err != nil {
				t.Fatal(err)
			}
			x.Enter(p, tc.ph, nil)
			b, err := x.Exchange().Message().Body(context.Background())
			if !errors.Is(err, tc.err) || string(b) != tc.want {
				t.Errorf("Body() = %q, %v, want %q, %v", b, err, tc.want, tc.err)
			}
			if tc.payload == "" && tc.err == nil && b != nil {
				t.Errorf("Body() = %q for an empty body, want nil", b)
			}
		})
	}
}

// TestPrepareBodyLimits checks the request gate: a declared length over
// limits.maxRequestBodyBytes is filter.ErrTooLarge before any byte is read
// (413 RZ-RT-003 at once), a longer undeclared body is ErrTooLarge too,
// and a gate the budget cannot cover is filter.ErrBudget (503 RZ-RT-004).
func TestPrepareBodyLimits(t *testing.T) {
	long := strings.Repeat("x", int(testLimits().MaxRequestBodyBytes)+1)
	t.Run("declared", func(t *testing.T) {
		f := newFixture(t)
		r := serverRequest(http.MethodPost, "/", long, nil)
		cb := &closeBody{Reader: strings.NewReader(long)}
		r.Body = cb
		x := f.routed(r, gateRoute())
		defer f.pool.Put(x)
		if err := x.PrepareBody(context.Background()); !errors.Is(err, filter.ErrTooLarge) || body.Code(err, body.SideRequest) != "RZ-RT-003" {
			t.Errorf("PrepareBody() = %v, want ErrTooLarge", err)
		}
		if n := cb.Reader.(*strings.Reader).Len(); n != len(long) {
			t.Errorf("%d bytes read from a body declared over the limit", len(long)-n)
		}
	})
	t.Run("undeclared", func(t *testing.T) {
		f := newFixture(t)
		r := serverRequest(http.MethodPost, "/", long, nil)
		r.ContentLength = -1
		x := f.routed(r, gateRoute())
		defer f.pool.Put(x)
		if err := x.PrepareBody(context.Background()); !errors.Is(err, filter.ErrTooLarge) {
			t.Errorf("PrepareBody() = %v, want ErrTooLarge", err)
		}
	})
	t.Run("budget", func(t *testing.T) {
		f := newFixtureBudget(t, 0)
		x := f.routed(serverRequest(http.MethodPost, "/", "{}", nil), gateRoute())
		defer f.pool.Put(x)
		if err := x.PrepareBody(context.Background()); !errors.Is(err, filter.ErrBudget) {
			t.Errorf("PrepareBody() = %v, want ErrBudget", err)
		}
	})
}

// TestRequestSetBody checks filter.Message.SetBody on the client request:
// the new body replaces the gated one for later Policies and the
// Forwarder, is reserved from the buffer budget (filter.ErrBudget leaves
// the body unchanged), sets Content-Length, removes Content-Encoding and
// the body digests, and invalidates the decoded body and request.body.
func TestRequestSetBody(t *testing.T) {
	f := newFixture(t)
	h := jsonHeader()
	for _, k := range []string{"Content-Encoding", "Content-Digest", "Repr-Digest", "Digest", "Content-Md5"} {
		h.Set(k, "x")
	}
	x := f.gatedRequest(t, `{"a":1}`, h)
	defer f.pool.Put(x)
	ctx := context.Background()
	x.Enter(testPolicy("tr", 0, phase.ClassTransform, phase.ScopeRoute, 0), phase.OnRequestBody, nil)
	m := x.Exchange().Message()
	before, err := m.Decoded(ctx)
	if err != nil || nativeOf(t, before) == nil {
		t.Fatalf("Decoded() = %v, %v", before, err)
	}
	if err := m.SetBody(ctx, []byte(`{"b":22}`)); err != nil {
		t.Fatalf("SetBody() = %v", err)
	}
	if b, _ := m.Body(ctx); string(b) != `{"b":22}` {
		t.Errorf("Body() after SetBody = %q", b)
	}
	if got := m.Header().Get("Content-Length"); got != "8" {
		t.Errorf("Content-Length = %q, want 8", got)
	}
	for _, k := range []string{"Content-Encoding", "Content-Digest", "Repr-Digest", "Digest", "Content-Md5"} {
		if _, ok := m.Header()[k]; ok {
			t.Errorf("%s kept after SetBody", k)
		}
	}
	after, err := m.Decoded(ctx)
	if want := map[string]any{"b": "22"}; err != nil || !reflect.DeepEqual(numbersAsText(nativeOf(t, after)), want) {
		t.Errorf("Decoded() after SetBody = %v, %v, want %v", nativeOf(t, after), err, want)
	}
	if v := x.Exchange().Vars().Request.Body; v != after {
		t.Error("request.body is not the new decoded body")
	}
	if got := readAll(t, mustOpen(t, x.Outbound().Body)); got != `{"b":22}` || x.Outbound().Body.ContentLength() != 8 {
		t.Errorf("the Forwarder reads %q", got)
	}

	huge := make([]byte, 2<<20)
	if err := m.SetBody(ctx, huge); !errors.Is(err, filter.ErrBudget) {
		t.Errorf("SetBody(2 MiB) = %v, want ErrBudget", err)
	}
	if b, _ := m.Body(ctx); string(b) != `{"b":22}` {
		t.Errorf("a refused SetBody changed the body to %d bytes", len(b))
	}
	x.Enter(testPolicy("h", 1, phase.ClassTransform, phase.ScopeRoute, 1), phase.OnRequestHeaders, nil)
	if err := x.Exchange().Message().SetBody(ctx, []byte("x")); !errors.Is(err, filter.ErrNotAvailable) {
		t.Errorf("SetBody() in onRequestHeaders = %v, want ErrNotAvailable", err)
	}
}

// TestDecodedRequestBody checks the decoded request body and request.body:
// a JSON body (application/json or +json) decodes once per body version,
// shared by Decoded and CEL; a body that is empty, of another media type,
// with two Content-Type lines or not valid JSON is not JSON (Decoded
// returns nil, CEL reads null); past 4 times the raw limit it is
// filter.ErrTooLarge and a refused charge filter.ErrBudget, and CEL then
// reads no body (a runtime error).
func TestDecodedRequestBody(t *testing.T) {
	dense := "[" + strings.Repeat("0,", 400) + "0]"
	for _, tc := range []struct {
		name    string
		payload string
		h       http.Header
		json    bool
		null    bool
		err     error
		// calls is how many times the decoder runs: once per body version
		// it has to read, never for a body that cannot be JSON.
		calls int64
	}{
		{"object", `{"a":[1,2]}`, jsonHeader(), true, false, nil, 1},
		{"plus json", `{"a":1}`, http.Header{"Content-Type": {"application/vnd.acme+json; charset=utf-8"}}, true, false, nil, 1},
		{"other media type", `{"a":1}`, http.Header{"Content-Type": {"text/plain"}}, false, true, nil, 0},
		{"two content types", `{"a":1}`, http.Header{"Content-Type": {"application/json", "application/json"}}, false, true, nil, 0},
		{"invalid", `{"a":`, jsonHeader(), false, true, nil, 1},
		{"empty", "", jsonHeader(), false, true, nil, 0},
		{"dense", dense, jsonHeader(), false, false, filter.ErrTooLarge, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			x := f.gatedRequest(t, tc.payload, tc.h)
			defer f.pool.Put(x)
			x.Enter(testPolicy("p", 0, phase.ClassAuthz, phase.ScopeRoute, 0), phase.OnRequestBody, nil)
			ctx := context.Background()
			v, err := x.Exchange().Message().Decoded(ctx)
			if !errors.Is(err, tc.err) || (v != nil) != tc.json {
				t.Fatalf("Decoded() = %v, %v, want JSON %v, error %v", v, err, tc.json, tc.err)
			}
			cel := x.Exchange().Vars().Request.Body
			switch {
			case tc.json && cel != v:
				t.Error("request.body is not the decoded body")
			case tc.null && (cel == nil || !cel.IsNull()):
				t.Errorf("request.body = %v, want null", cel)
			case tc.err != nil && cel != nil:
				t.Errorf("request.body = %v after %v, want none", cel, tc.err)
			}
			_, _ = x.Exchange().Message().Decoded(ctx)
			if n := f.dec.n.Load(); n != tc.calls {
				t.Errorf("decoded %d times, want %d", n, tc.calls)
			}
		})
	}
}

// TestDecodedChargesBudget checks that a decoded body is charged to
// limits.maxBufferedBytes at its built size and that a refused charge is
// filter.ErrBudget (503 RZ-RT-004).
func TestDecodedChargesBudget(t *testing.T) {
	// Two increments, no stream share: the gate takes one.
	f := newFixtureBudget(t, 2*body.Increment)
	x := f.gatedRequest(t, `{"a":1}`, jsonHeader())
	defer f.pool.Put(x)
	x.Enter(testPolicy("p", 0, phase.ClassAuthz, phase.ScopeRoute, 0), phase.OnRequestBody, nil)
	var other body.Account
	other.Bind(f.bud, body.Gate, 0)
	if err := other.Reserve(1); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Exchange().Message().Decoded(context.Background()); !errors.Is(err, filter.ErrBudget) {
		t.Errorf("Decoded() with a spent budget = %v, want ErrBudget", err)
	}
	other.ReleaseAll()

	g := newFixtureBudget(t, 2*body.Increment)
	y := g.gatedRequest(t, `{"a":1}`, jsonHeader())
	defer g.pool.Put(y)
	y.Enter(testPolicy("p", 0, phase.ClassAuthz, phase.ScopeRoute, 0), phase.OnRequestBody, nil)
	usedBefore := g.bud.Used()
	if _, err := y.Exchange().Message().Decoded(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.bud.Used() <= usedBefore {
		t.Error("the decoded body was not charged to the budget")
	}
}

// TestRequestViewSharedByLegs checks that concurrent legs read the client's
// decoded body as request.body, decoded once (run under -race), and that
// the request body is not decoded for CEL once the response is set.
func TestRequestViewSharedByLegs(t *testing.T) {
	f := newFixture(t)
	x := f.gatedRequest(t, `{"sku":"a-1"}`, jsonHeader())
	defer f.pool.Put(x)
	p := testPolicy("tr", 0, phase.ClassTransform, phase.ScopeUpstream, 0)
	var wg sync.WaitGroup
	for range 8 {
		l := x.Leg("inventory", "s")
		wg.Go(func() {
			defer l.Release()
			l.SetLeg(&filter.Leg{Upstream: "inventory", Attempt: 1, Request: &http.Request{Header: http.Header{}}})
			l.Enter(p, phase.OnUpstreamRequest, nil)
			v := l.Exchange().Vars().Request.Body
			if got := nativeOf(t, v); !reflect.DeepEqual(got, map[string]any{"sku": "a-1"}) {
				t.Errorf("leg request.body = %v", got)
			}
		})
	}
	wg.Wait()
	if n := f.dec.n.Load(); n != 1 {
		t.Errorf("decoded %d times for 8 legs, want once", n)
	}

	y := f.gatedRequest(t, `{"a":1}`, jsonHeader())
	defer f.pool.Put(y)
	y.SetResponse(&snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{}})
	y.Enter(p, phase.OnResponse, nil)
	if y.Exchange().Vars().Request.Body != nil || f.dec.n.Load() != 1 {
		t.Error("the request body was decoded for CEL after the response was set")
	}
}

// numbersAsText turns decoded JSON numbers into their text, for
// comparisons.
func numbersAsText(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = numbersAsText(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = numbersAsText(e)
		}
		return out
	case interface{ String() string }:
		return t.String()
	default:
		return v
	}
}

// mustOpen opens rb for one attempt; the reader closes with the test.
func mustOpen(tb testing.TB, rb snapshot.RequestBody) io.Reader {
	tb.Helper()
	rd, err := rb.Open()
	if err != nil {
		tb.Fatalf("Open() = %v", err)
	}
	tb.Cleanup(func() { _ = rd.Close() })
	return rd
}
