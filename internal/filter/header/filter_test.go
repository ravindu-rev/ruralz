// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package header

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// mustContinue runs Handle and stops the test unless it continues.
func mustContinue(t *testing.T, f *Filter, p phase.Phase, x filter.Exchange) {
	t.Helper()
	if r := f.Handle(t.Context(), p, x); r.Outcome != filter.Continue {
		t.Fatalf("Handle(%s) = %+v, want Continue", p, r)
	}
}

// equalHeader reports whether got and want hold the same keys and lines.
func equalHeader(got, want http.Header) bool {
	return maps.EqualFunc(got, want, slices.Equal)
}

// TestOperationOrder checks that a side applies remove, then set (one line
// per name, replacing every line), then add (one more line after the
// field's lines, a line set by the same side included), and creates a
// field add or set names that is absent.
func TestOperationOrder(t *testing.T) {
	cfg := &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Remove: []string{"x-a", "x-absent"},
		Set:    []v1alpha1.HeaderRequestSet{reqLit("x-b", "3"), reqLit("x-a", "set-after-remove")},
		Add:    []v1alpha1.HeaderRequestSet{reqLit("x-b", "4"), reqLit("x-c", "5"), reqLit("x-d", "d2")},
	}}
	f := build(t, cfg, phase.ScopeRoute)
	h := http.Header{"X-A": {"1"}, "X-B": {"2", "2b", "2c"}, "X-D": {"d1"}, "X-Keep": {"k"}}
	mustContinue(t, f, phase.OnRequestHeaders, onRequest(t, h))
	want := http.Header{
		"X-A": {"set-after-remove"}, "X-B": {"3", "4"}, "X-C": {"5"}, "X-D": {"d1", "d2"}, "X-Keep": {"k"},
	}
	if !equalHeader(h, want) {
		t.Errorf("header = %v, want %v", h, want)
	}
}

// TestNamesIgnoreCase checks that configured names match fields without
// regard to case and are written in canonical form.
func TestNamesIgnoreCase(t *testing.T) {
	cfg := &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Remove: []string{"x-INTERNAL"},
		Set:    []v1alpha1.HeaderRequestSet{reqLit("X-SHOP-consumer", "acme")},
		Add:    []v1alpha1.HeaderRequestSet{reqLit("x-trace_tag", "t")},
	}}
	f := build(t, cfg, phase.ScopeRoute)
	h := http.Header{"X-Internal": {"secret"}, "X-Shop-Consumer": {"forged"}}
	mustContinue(t, f, phase.OnRequestHeaders, onRequest(t, h))
	want := http.Header{"X-Shop-Consumer": {"acme"}, "X-Trace_tag": {"t"}}
	if !equalHeader(h, want) {
		t.Errorf("header = %v, want %v", h, want)
	}
}

// TestFoldsDifferentlyCasedKeys checks a header map that holds one field
// under keys differing in case, which net/http never produces: remove
// deletes every one, set leaves one line under the canonical key, and add
// appends after the canonical key's lines and then the other keys' in
// ascending byte order. Keys of fields the side does not name stay as they
// are.
func TestFoldsDifferentlyCasedKeys(t *testing.T) {
	cfg := &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Remove: []string{"x-internal"},
		Set:    []v1alpha1.HeaderRequestSet{reqLit("x-set", "s")},
		Add:    []v1alpha1.HeaderRequestSet{reqLit("x-add", "n")},
	}}
	f := build(t, cfg, phase.ScopeRoute)
	h := http.Header{
		"x-internal": {"a"}, "X-Internal": {"b"}, "X-INTERNAL": {"c"},
		"x-set": {"1"}, "X-SET": {"2"},
		"X-Add": {"c"}, "x-add": {"l"}, "X-add": {"m"},
		"x-other": {"o"},
	}
	mustContinue(t, f, phase.OnRequestHeaders, onRequest(t, h))
	want := http.Header{"X-Set": {"s"}, "X-Add": {"c", "m", "l", "n"}, "x-other": {"o"}}
	if !equalHeader(h, want) {
		t.Errorf("header = %v, want %v", h, want)
	}
	// A variant of a field add creates is folded even without a canonical
	// key, and variants without lines leave no key.
	h = http.Header{"x-add": {"l"}, "x-set": {}}
	mustContinue(t, f, phase.OnRequestHeaders, onRequest(t, h))
	want = http.Header{"X-Add": {"l", "n"}, "X-Set": {"s"}}
	if !equalHeader(h, want) {
		t.Errorf("header = %v, want %v", h, want)
	}
}

// TestPhasesByScope checks where each side edits: at Gateway and Route
// scope the request in onRequestHeaders and the response in onResponse;
// at Upstream scope the attempt's request in onUpstreamRequest and its
// response in onUpstreamResponseHeaders; and nothing in any other Phase.
func TestPhasesByScope(t *testing.T) {
	cfg := &v1alpha1.HeadersConfig{
		Request:  &v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{reqLit("x-req", "1")}},
		Response: &v1alpha1.HeaderResponseOps{Set: []v1alpha1.HeaderResponseSet{respLit("x-resp", "1")}},
	}
	for _, tt := range []struct {
		scope         phase.Scope
		request, resp phase.Phase
	}{
		{phase.ScopeGateway, phase.OnRequestHeaders, phase.OnResponse},
		{phase.ScopeRoute, phase.OnRequestHeaders, phase.OnResponse},
		{phase.ScopeUpstream, phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders},
	} {
		f := build(t, cfg, tt.scope)
		for p := range phase.Count {
			h := http.Header{}
			x := onRequest(t, h)
			x.LegV = &filter.Leg{Upstream: "orders", Attempt: 1, Response: &http.Response{StatusCode: http.StatusOK}}
			mustContinue(t, f, p, x)
			var want http.Header
			switch p {
			case tt.request:
				want = http.Header{"X-Req": {"1"}}
			case tt.resp:
				want = http.Header{"X-Resp": {"1"}}
			default:
				want = http.Header{}
			}
			if !equalHeader(h, want) {
				t.Errorf("%s scope, %s: header = %v, want %v", tt.scope, p, h, want)
			}
		}
	}
}

// TestAttemptWithoutResponseIsLeftAlone checks that in
// onUpstreamResponseHeaders an attempt that got no response is neither
// edited nor evaluated, so an expression over response cannot fail the
// leg, while an attempt with a response is edited.
func TestAttemptWithoutResponseIsLeftAlone(t *testing.T) {
	cfg := &v1alpha1.HeadersConfig{Response: &v1alpha1.HeaderResponseOps{
		Set: []v1alpha1.HeaderResponseSet{respExpr("x-status", `string(response.status)`)},
	}}
	f := build(t, cfg, phase.ScopeUpstream)
	h := http.Header{}
	x := onResponse(t, 0, h)
	x.VarsV.Response = nil
	x.LegV = &filter.Leg{Upstream: "orders", Attempt: 1, ErrorKind: "connect"}
	mustContinue(t, f, phase.OnUpstreamResponseHeaders, x)
	if len(h) != 0 {
		t.Errorf("attempt without a response: header = %v, want it untouched", h)
	}
	x = onResponse(t, 503, h)
	x.LegV = &filter.Leg{Upstream: "orders", Attempt: 2, Response: &http.Response{StatusCode: http.StatusServiceUnavailable, Header: h}}
	mustContinue(t, f, phase.OnUpstreamResponseHeaders, x)
	if want := (http.Header{"X-Status": {"503"}}); !equalHeader(h, want) {
		t.Errorf("attempt with a response: header = %v, want %v", h, want)
	}
}

// TestComputedValues checks valueExpressions over the base variables: the
// result is trimmed of SP and HTAB, may keep an inner HTAB, and an empty
// result sets an empty field.
func TestComputedValues(t *testing.T) {
	cfg := &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Set: []v1alpha1.HeaderRequestSet{
			reqExpr("x-shop-consumer", `consumer == null ? "anonymous" : consumer.name`),
			reqExpr("x-padded", `" \tpadded value\t "`),
			reqExpr("x-tab", `"a\tb"`),
			reqExpr("x-empty", `"  "`),
			reqExpr("x-route", `route.name + "/" + route.labels.team`),
		},
		Add: []v1alpha1.HeaderRequestSet{reqExpr("x-sub", `string(auth.claims.sub)`)},
	}}
	f := build(t, cfg, phase.ScopeRoute)
	h := http.Header{"X-Sub": {"first"}}
	mustContinue(t, f, phase.OnRequestHeaders, onRequest(t, h))
	want := http.Header{
		"X-Shop-Consumer": {"acme"}, "X-Padded": {"padded value"}, "X-Tab": {"a\tb"},
		"X-Empty": {""}, "X-Route": {"orders/core"}, "X-Sub": {"first", "u-7"},
	}
	if !equalHeader(h, want) {
		t.Errorf("header = %q, want %q", h, want)
	}
	// An anonymous request takes the other branch.
	h = http.Header{}
	x := onRequest(t, h)
	x.VarsV.Consumer, x.VarsV.Auth = nil, nil
	f = build(t, &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Set: []v1alpha1.HeaderRequestSet{reqExpr("x-shop-consumer", `consumer == null ? "anonymous" : consumer.name`)},
	}}, phase.ScopeUpstream)
	mustContinue(t, f, phase.OnUpstreamRequest, x)
	if want := (http.Header{"X-Shop-Consumer": {"anonymous"}}); !equalHeader(h, want) {
		t.Errorf("anonymous header = %v, want %v", h, want)
	}
}

// TestExpressionsReadTheMessageAsItReached checks that every
// valueExpression is evaluated before the first edit: an expression reads
// a field another entry of the side removes or sets as the message
// reached the Policy.
func TestExpressionsReadTheMessageAsItReached(t *testing.T) {
	cfg := &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Remove: []string{"x-a"},
		Set: []v1alpha1.HeaderRequestSet{
			reqLit("x-c", "new"),
			reqExpr("x-d", `request.headers["x-c"]`),
			reqExpr("x-e", `"x-a" in request.headers ? request.headers["x-a"] : "gone"`),
		},
		Add: []v1alpha1.HeaderRequestSet{reqExpr("x-f", `"x-d" in request.headers ? "seen" : "absent"`)},
	}}
	f := build(t, cfg, phase.ScopeRoute)
	h := http.Header{"X-A": {"orig-a"}, "X-C": {"orig-c"}}
	mustContinue(t, f, phase.OnRequestHeaders, onRequest(t, h))
	want := http.Header{"X-C": {"new"}, "X-D": {"orig-c"}, "X-E": {"orig-a"}, "X-F": {"absent"}}
	if !equalHeader(h, want) {
		t.Errorf("header = %v, want %v", h, want)
	}
}

// TestFailureLeavesMessageUntouched checks that a valueExpression that
// fails cannot decide with the transform class default (no code of its
// own) and that no edit of the side is applied, so an open Policy is
// skipped cleanly; the cause names the span's error.type: cel_error for a
// runtime error, result_type for a dyn result that is not a string, and
// invalid_value for a computed value that is not a field value. No cause
// holds the value.
func TestFailureLeavesMessageUntouched(t *testing.T) {
	big := strings.Repeat("v", httpfield.MaxComputedValueBytes+1)
	tests := []struct {
		name    string
		src     string
		errType string
		edit    func(v *expr.Vars)
		cause   error
	}{
		{"null variable", `consumer.name`, errTypeCEL, func(v *expr.Vars) { v.Consumer = nil }, nil},
		{"missing claim", `string(auth.claims.missing)`, errTypeCEL, nil, nil},
		{"dyn result not a string", `auth.claims.n`, errTypeResultType, nil, nil},
		{"line feed", `"bad\nvalue"`, errTypeInvalidValue, nil, httpfield.ErrValueControl},
		{"carriage return", `"bad\rvalue"`, errTypeInvalidValue, nil, httpfield.ErrValueControl},
		{"NUL", `"bad\x00value"`, errTypeInvalidValue, nil, httpfield.ErrValueControl},
		{"DEL", `"bad\x7fvalue"`, errTypeInvalidValue, nil, httpfield.ErrValueControl},
		{"over the computed value limit", `request.headers["x-big"]`, errTypeInvalidValue, nil, httpfield.ErrValueTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
				Remove: []string{"x-a"},
				Set:    []v1alpha1.HeaderRequestSet{reqLit("x-b", "1"), reqExpr("x-c", tt.src)},
				Add:    []v1alpha1.HeaderRequestSet{reqLit("x-d", "2")},
			}}
			f := build(t, cfg, phase.ScopeRoute)
			h := http.Header{"X-A": {"keep"}, "X-Big": {big}}
			before := h.Clone()
			x := onRequest(t, h)
			if tt.edit != nil {
				tt.edit(x.VarsV)
			}
			r := f.Handle(t.Context(), phase.OnRequestHeaders, x)
			if r.Outcome != filter.CannotDecide || r.Code != "" || r.Response != nil {
				t.Fatalf("Handle = %+v, want CannotDecide with the class default code", r)
			}
			if !equalHeader(h, before) {
				t.Errorf("header = %v after a failure, want it untouched", h)
			}
			var typed interface{ ErrorType() string }
			if !errors.As(r.Err, &typed) || typed.ErrorType() != tt.errType {
				t.Errorf("cause %v names error.type %v, want %s", r.Err, typed, tt.errType)
			}
			if tt.cause != nil && !errors.Is(r.Err, tt.cause) {
				t.Errorf("cause %v, want %v", r.Err, tt.cause)
			}
			var ee *expr.EvalError
			if tt.cause == nil && !errors.As(r.Err, &ee) {
				t.Errorf("cause %v, want an *expr.EvalError", r.Err)
			}
			if msg := r.Err.Error(); !strings.Contains(msg, "X-C") || strings.Contains(msg, "bad") || strings.Contains(msg, "vvvv") {
				t.Errorf("cause text %q, want the field name and no value", msg)
			}
		})
	}
}

// TestResponseSide checks the response side: expressions read response
// and upstream, a generated response is edited like any other, and the
// response side never touches the request.
func TestResponseSide(t *testing.T) {
	cfg := &v1alpha1.HeadersConfig{Response: &v1alpha1.HeaderResponseOps{
		Remove: []string{"server"},
		Set: []v1alpha1.HeaderResponseSet{
			respLit("strict-transport-security", "max-age=31536000"),
			respExpr("x-upstream", `upstream == null ? "none" : upstream.name`),
		},
		Add: []v1alpha1.HeaderResponseSet{respExpr("x-cache-status", `response.status >= 500 ? "error" : response.headers["x-origin"]`)},
	}}
	f := build(t, cfg, phase.ScopeGateway)
	for _, generated := range []bool{false, true} {
		h := http.Header{"Server": {"origin/1.0"}, "X-Origin": {"eu"}}
		x := onResponse(t, 401, h)
		x.Msg.(*filtertest.Message).Gen = generated
		req := x.VarsV.Request.Header
		mustContinue(t, f, phase.OnResponse, x)
		want := http.Header{
			"Strict-Transport-Security": {"max-age=31536000"}, "X-Upstream": {"orders"},
			"X-Origin": {"eu"}, "X-Cache-Status": {"eu"},
		}
		if !equalHeader(h, want) {
			t.Errorf("generated %v: response header = %v, want %v", generated, h, want)
		}
		if len(req) != 0 {
			t.Errorf("generated %v: request header = %v, want it untouched", generated, req)
		}
	}
}

// TestNoMessageCannotDecide checks that a Phase without a message, or one
// whose message has no header map, cannot decide instead of panicking.
func TestNoMessageCannotDecide(t *testing.T) {
	f := build(t, &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{Remove: []string{"x-a"}}}, phase.ScopeRoute)
	for _, msg := range []filter.Message{nil, &nilHeader{}} {
		x := onRequest(t, http.Header{})
		x.Msg = msg
		r := f.Handle(t.Context(), phase.OnRequestHeaders, x)
		if r.Outcome != filter.CannotDecide || !errors.Is(r.Err, errNoMessage) {
			t.Errorf("Handle with message %T = %+v, want CannotDecide with errNoMessage", msg, r)
		}
	}
	// An empty side never reads the message.
	x := onRequest(t, http.Header{})
	x.Msg = nil
	mustContinue(t, f, phase.OnResponse, x)
}

// TestLiteralValuesAreShared checks that a literal value is one shared
// slice of length equal to capacity: two requests get the same backing
// array, and a later append to one request's field copies it instead of
// changing the other's.
func TestLiteralValuesAreShared(t *testing.T) {
	f := build(t, &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Set: []v1alpha1.HeaderRequestSet{reqLit("x-a", "1")},
		Add: []v1alpha1.HeaderRequestSet{reqLit("x-b", "2")},
	}}, phase.ScopeRoute)
	h1, h2 := http.Header{}, http.Header{}
	mustContinue(t, f, phase.OnRequestHeaders, onRequest(t, h1))
	mustContinue(t, f, phase.OnRequestHeaders, onRequest(t, h2))
	for _, k := range []string{"X-A", "X-B"} {
		if &h1[k][0] != &h2[k][0] || len(h1[k]) != 1 || cap(h1[k]) != 1 {
			t.Errorf("%s: len %d, cap %d, shared %v; want one shared line", k, len(h1[k]), cap(h1[k]), &h1[k][0] == &h2[k][0])
		}
	}
	h1.Add("X-A", "later")
	h1["X-B"] = append(h1["X-B"], "later")
	if !slices.Equal(h2["X-A"], []string{"1"}) || !slices.Equal(h2["X-B"], []string{"2"}) {
		t.Errorf("an append to one request changed another: %v", h2)
	}
}

// TestAddCopiesSharedCapacity checks that add never writes into spare
// capacity of a field's line slice, which another slice may share.
func TestAddCopiesSharedCapacity(t *testing.T) {
	f := build(t, &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Add: []v1alpha1.HeaderRequestSet{reqLit("x-b", "2")},
	}}, phase.ScopeRoute)
	backing := make([]string, 4)
	backing[0] = "1"
	h := http.Header{"X-B": backing[:1]}
	mustContinue(t, f, phase.OnRequestHeaders, onRequest(t, h))
	if !slices.Equal(h["X-B"], []string{"1", "2"}) {
		t.Errorf("X-B = %v, want [1 2]", h["X-B"])
	}
	if backing[1] != "" {
		t.Errorf("add wrote %q into the shared capacity", backing[1])
	}
}

// TestConcurrentHandle runs one Filter from many goroutines, each with its
// own message, under the race detector.
func TestConcurrentHandle(t *testing.T) {
	f := build(t, &v1alpha1.HeadersConfig{Request: &v1alpha1.HeaderRequestOps{
		Remove: []string{"x-internal"},
		Set:    []v1alpha1.HeaderRequestSet{reqLit("x-a", "1"), reqExpr("x-shop-consumer", `consumer.name`)},
		Add:    []v1alpha1.HeaderRequestSet{reqExpr("x-b", `route.name`)},
	}}, phase.ScopeUpstream)
	base := requestVars(t, nil)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			// Each goroutine owns its activation and request views; the
			// Consumer, Route and claims views are shared read-only.
			v := *base
			for range 50 {
				h := http.Header{"X-Internal": {"secret"}, "X-B": {"0"}}
				v.Request = &expr.Request{Method: "GET", Host: "api.shop.example", Path: "/v1/orders/42", Header: h}
				x := &filtertest.Exchange{Msg: &filtertest.Message{Hdr: h}, VarsV: &v}
				if r := f.Handle(context.Background(), phase.OnUpstreamRequest, x); r.Outcome != filter.Continue {
					t.Errorf("Handle = %+v, want Continue", r)
					return
				}
				want := http.Header{"X-A": {"1"}, "X-Shop-Consumer": {"acme"}, "X-B": {"0", "orders"}}
				if !equalHeader(h, want) {
					t.Errorf("header = %v, want %v", h, want)
					return
				}
			}
		})
	}
	wg.Wait()
}

// TestAddedRequestBytes checks a headers Policy's share of the request
// header bytes the snapshot's input caps add for Policy edits: each request
// set and add entry at its name, its literal value or 8 KiB for a computed
// one, plus 4 bytes; removes and the response side count nothing.
func TestAddedRequestBytes(t *testing.T) {
	lit := "v1"
	cfg := &v1alpha1.HeadersConfig{
		Request: &v1alpha1.HeaderRequestOps{
			Set:    []v1alpha1.HeaderRequestSet{{Name: "X-Api-Version", Value: &lit}},
			Add:    []v1alpha1.HeaderRequestSet{{Name: "X-Consumer", ValueExpression: "consumer.name"}},
			Remove: []string{"X-Debug"},
		},
		Response: &v1alpha1.HeaderResponseOps{Add: []v1alpha1.HeaderResponseSet{{Name: "X-Served-By", Value: &lit}}},
	}
	want := int64(len("X-Api-Version")+len(lit)+4) + int64(len("X-Consumer")+httpfield.MaxComputedValueBytes+4)
	if got := AddedRequestBytes(cfg); got != want {
		t.Errorf("AddedRequestBytes = %d, want %d", got, want)
	}
	for _, empty := range []*v1alpha1.HeadersConfig{nil, {}, {Response: cfg.Response}} {
		if got := AddedRequestBytes(empty); got != 0 {
			t.Errorf("AddedRequestBytes(%+v) = %d, want 0", empty, got)
		}
	}
}
