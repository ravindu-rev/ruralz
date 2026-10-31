// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package header

import (
	"context"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// reqLit returns a request entry with the literal value v.
func reqLit(name, v string) v1alpha1.HeaderRequestSet {
	return v1alpha1.HeaderRequestSet{Name: name, Value: &v}
}

// reqExpr returns a request entry computed by src.
func reqExpr(name, src string) v1alpha1.HeaderRequestSet {
	return v1alpha1.HeaderRequestSet{Name: name, ValueExpression: src}
}

// respLit returns a response entry with the literal value v.
func respLit(name, v string) v1alpha1.HeaderResponseSet {
	return v1alpha1.HeaderResponseSet{Name: name, Value: &v}
}

// respExpr returns a response entry computed by src.
func respExpr(name, src string) v1alpha1.HeaderResponseSet {
	return v1alpha1.HeaderResponseSet{Name: name, ValueExpression: src}
}

// phasesAt returns the Phases the registry subscribes a headers Policy
// with both sides to at scope.
func phasesAt(scope phase.Scope) phase.Set {
	if scope == phase.ScopeUpstream {
		return phase.Of(phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders)
	}
	return phase.Of(phase.OnRequestHeaders, phase.OnResponse)
}

// buildEnv returns the BuildEnv of a headers Policy named headers-test at
// scope, compiling through a real CEL builder under the default Gateway's
// caps over the tests' Route and Consumer, with the bytes the Policy itself
// adds to the request, as a snapshot of a Route holding only it passes them.
func buildEnv(tb testing.TB, cfg *v1alpha1.HeadersConfig, scope phase.Scope) filter.BuildEnv {
	tb.Helper()
	caps := celtest.Caps([]*expr.Route{{Name: "orders", Labels: map[string]string{"team": "core"}}}, &expr.Consumer{Name: "acme", Tier: "gold"})
	caps.AddedHeaderBytes = AddedRequestBytes(cfg)
	return filter.BuildEnv{
		Policy: &v1alpha1.Policy{
			Metadata: v1alpha1.ObjectMeta{Name: "headers-test"},
			Spec:     v1alpha1.PolicySpec{Type: Type},
		},
		Config: cfg,
		Scope:  scope,
		Phases: phasesAt(scope),
		CEL:    celtest.Compiler(tb).NewBuilder(nil, caps),
		Site: func(p expr.PlaceID) expr.Site {
			return expr.Site{Place: p, Scope: scope, PolicyType: Type}
		},
	}
}

// build compiles cfg at scope, or stops the test.
func build(tb testing.TB, cfg *v1alpha1.HeadersConfig, scope phase.Scope) *Filter {
	tb.Helper()
	f, err := Factory{}.Build(context.Background(), buildEnv(tb, cfg, scope))
	if err != nil {
		tb.Fatalf("Build: %v", err)
	}
	return f.(*Filter)
}

// requestVars returns the activation of a request whose header map is h,
// the live map CEL reads, by Consumer acme with JWT claims.
func requestVars(tb testing.TB, h http.Header) *expr.Vars {
	tb.Helper()
	return &expr.Vars{
		Request: &expr.Request{
			Method: "GET", Scheme: "https", Host: "api.shop.example", Path: "/v1/orders/42", Header: h,
		},
		Source:   &expr.Source{IP: netip.MustParseAddr("192.0.2.1"), Port: 44321},
		Route:    celtest.Route("orders", map[string]string{"team": "core"}),
		Consumer: celtest.Consumer(expr.Consumer{Name: "acme", Tier: "gold"}),
		Auth:     &expr.Auth{Method: filter.MethodJWT, Claims: celtest.JSON(tb, `{"sub": "u-7", "n": 7}`)},
		Now:      time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC),
	}
}

// onRequest returns an Exchange of a request Phase over the header map h.
func onRequest(tb testing.TB, h http.Header) *filtertest.Exchange {
	tb.Helper()
	return &filtertest.Exchange{Hdr: h, Msg: &filtertest.Message{Hdr: h}, VarsV: requestVars(tb, h)}
}

// onResponse returns an Exchange of a response Phase whose response has
// status and the header map h, after a request without headers to the
// Upstream orders.
func onResponse(tb testing.TB, status int, h http.Header) *filtertest.Exchange {
	tb.Helper()
	v := requestVars(tb, http.Header{})
	v.Response = &expr.Response{Status: status, Header: h}
	v.Upstream = &expr.Upstream{Name: "orders", Endpoint: "10.0.0.7:8080"}
	return &filtertest.Exchange{Msg: &filtertest.Message{Hdr: h, Code: status}, VarsV: v}
}

// nilHeader is a message whose header map is nil.
type nilHeader struct{ filtertest.Message }

// Header returns nil.
func (*nilHeader) Header() http.Header { return nil }
