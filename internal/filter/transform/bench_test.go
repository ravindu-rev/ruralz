// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"context"
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// keptMessage is a message whose SetBody only keeps the body, so an
// allocation count holds the Filter's own allocations alone.
type keptMessage struct {
	filtertest.Message
}

// SetBody keeps b.
func (m *keptMessage) SetBody(_ context.Context, b []byte) error {
	m.Raw = b
	return nil
}

// fixture is one Phase's call over a 1 KiB order, reset before each call.
type fixture struct {
	f  *Filter
	p  phase.Phase
	x  *filtertest.Exchange
	m  *keptMessage
	in []byte
}

// newFixture returns the call of cfg's Filter at scope in Phase p over the
// order in, with request.body or response.body decoded once.
func newFixture(tb testing.TB, cfg any, scope phase.Scope, p phase.Phase, in string) *fixture {
	tb.Helper()
	fx := &fixture{f: build(tb, cfg, scope), p: p, in: []byte(in)}
	h := jsonHeader()
	h.Set("X-Drop", "1")
	fx.m = &keptMessage{Message: filtertest.Message{Hdr: h, Raw: fx.in, Code: http.StatusOK}}
	v := baseVars(h, "/v1/orders/42", "")
	body := bodyValue(tb, h, in)
	if specOf(tb, cfg).Side == Request {
		v.Request.Body = body
	} else {
		v.Response = &expr.Response{Status: http.StatusOK, Header: h, Body: body}
	}
	if scope == phase.ScopeUpstream {
		v.Upstream = &expr.Upstream{Name: "orders", Endpoint: "10.0.0.7:8080"}
	}
	fx.x = &filtertest.Exchange{MethodV: http.MethodGet, Msg: fx.m, VarsV: v}
	if scope == phase.ScopeUpstream {
		fx.x.LegV = &filter.Leg{Upstream: "orders", Attempt: 1, Request: &http.Request{Method: http.MethodGet}}
	}
	return fx
}

// call resets the body and runs the Filter once.
func (fx *fixture) call(ctx context.Context) filter.Result {
	fx.m.Raw = fx.in
	return fx.f.Handle(ctx, fx.p, fx.x)
}

// literalOps returns the literal operations of the orders-shape example
// and a header removal, with the email masked by literal text.
func literalOps() *v1alpha1.TransformRequestConfig {
	return &v1alpha1.TransformRequestConfig{
		ArrayOps: []v1alpha1.ArrayOp{arrayOp("move", "items.*.sku", "skus")},
		Remove:   []v1alpha1.TransformRequestRemove{reqRemove("body", "internal"), reqRemove("header", "x-drop")},
		Replace:  []v1alpha1.Replace{literally("customer.email", "jane.doe", "***")},
	}
}

// TestLiteralOperationsAllocations covers the allocation budget of the
// literal operations over pooled buffers
// (docs/architecture/12-performance-budgets-and-benchmarking.md "Per-stage
// latency budget"): a call that rewrites the body allocates only the body
// the message keeps, and one that changes nothing allocates nothing.
func TestLiteralOperationsAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under the race detector")
	}
	in, _ := ordersBody()
	ctx := context.Background()
	rewrite := newFixture(t, literalOps(), phase.ScopeRoute, phase.OnRequestBody, in)
	if res := rewrite.call(ctx); res.Outcome != filter.Continue {
		t.Fatalf("Handle = %+v", res)
	}
	if n := testing.AllocsPerRun(200, func() { rewrite.call(ctx) }); n != 1 {
		t.Errorf("a body rewrite allocates %v times per call, want 1", n)
	}
	unchanged := newFixture(t, &v1alpha1.TransformRequestConfig{
		Remove:  []v1alpha1.TransformRequestRemove{reqRemove("body", "missing"), reqRemove("header", "x-drop")},
		Replace: []v1alpha1.Replace{literally("customer.email", "zzz", "***"), literally("", "zzz", "y")},
	}, phase.ScopeRoute, phase.OnRequestBody, in)
	if n := testing.AllocsPerRun(200, func() { unchanged.call(ctx) }); n != 0 {
		t.Errorf("a call that changes nothing allocates %v times, want 0", n)
	}
	headers := newFixture(t, &v1alpha1.TransformResponseConfig{
		Remove: []v1alpha1.TransformResponseRemove{respRemove("header", "x-drop")},
	}, phase.ScopeRoute, phase.OnResponse, in)
	if n := testing.AllocsPerRun(200, func() { headers.call(ctx) }); n != 0 {
		t.Errorf("a header removal allocates %v times, want 0", n)
	}
}

// BenchmarkOrdersShape measures the orders-shape example on a 1 KiB order
// in each Phase a transform runs in, the Transform Policy row of the
// per-stage latency budget.
func BenchmarkOrdersShape(b *testing.B) {
	in, _ := ordersBody()
	for _, tc := range []struct {
		side  Side
		scope phase.Scope
		p     phase.Phase
	}{
		{Request, phase.ScopeRoute, phase.OnRequestBody},
		{Request, phase.ScopeUpstream, phase.OnUpstreamRequest},
		{Response, phase.ScopeUpstream, phase.OnUpstreamResponseBody},
		{Response, phase.ScopeRoute, phase.OnResponse},
	} {
		b.Run(tc.p.String(), func(b *testing.B) {
			fx := newFixture(b, ordersShape(tc.side), tc.scope, tc.p, in)
			ctx := context.Background()
			b.ReportAllocs()
			b.SetBytes(int64(len(in)))
			for b.Loop() {
				if res := fx.call(ctx); res.Outcome != filter.Continue {
					b.Fatalf("Handle = %+v", res)
				}
			}
		})
	}
}

// BenchmarkLiteralOperations measures the literal operations on a 1 KiB
// order in onRequestBody.
func BenchmarkLiteralOperations(b *testing.B) {
	in, _ := ordersBody()
	fx := newFixture(b, literalOps(), phase.ScopeRoute, phase.OnRequestBody, in)
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(int64(len(in)))
	for b.Loop() {
		if res := fx.call(ctx); res.Outcome != filter.Continue {
			b.Fatalf("Handle = %+v", res)
		}
	}
}
