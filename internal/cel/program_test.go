// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// testVars returns an activation with every M1 variable set.
func testVars() *expr.Vars {
	req := &expr.Request{
		Method:     "GET",
		Scheme:     "https",
		Host:       "api.example.com",
		Path:       "/orders/42",
		PathParams: []expr.Param{{Name: "orderId", Value: "42"}},
		Header: http.Header{
			"X-Tenant": {"acme"},
			"X-User":   {"u1"},
			"Accept":   {"application/json", "text/plain"},
		},
		Body: celtypes.FromNative(map[string]any{"kind": "order", "id": json.Number("7"), "items": []any{"a", "b"}}),
	}
	req.SetRawQuery("tag=a&tag=b")
	return &expr.Vars{
		Request:  req,
		Source:   &expr.Source{IP: netip.MustParseAddr("192.0.2.1"), Port: 44321},
		Route:    &expr.Route{Name: "orders", Labels: map[string]string{"team": "core"}},
		Consumer: &expr.Consumer{Name: "acme", Tier: "gold", Quotas: []string{"daily-tokens"}},
		Auth:     &expr.Auth{Method: "jwt", Claims: celtypes.FromNative(map[string]any{"scope": "orders:read orders:write"})},
		Response: &expr.Response{Status: 503, Header: http.Header{"Retry-After": {"5"}}, Body: celtypes.FromNative(map[string]any{"total": json.Number("12.5")})},
		Upstream: &expr.Upstream{Name: "orders-v1", Endpoint: "10.0.0.7:8080"},
		Attempt:  1,
		Now:      time.Date(2026, 9, 26, 10, 30, 0, 0, time.UTC),
		Duration: 1500 * time.Millisecond,
	}
}

// mustProgram compiles src at site or stops the test.
func mustProgram(tb testing.TB, c *Compiler, site expr.Site, src string) *Program {
	tb.Helper()
	prg, iss := c.program(site, src)
	if prg == nil {
		tb.Fatalf("program(%q) = %+v", src, iss)
	}
	return prg
}

// TestEvalBoolAndString evaluates the documents' expressions in their places.
func TestEvalBoolAndString(t *testing.T) {
	c := newCompiler(t)
	bools := []struct {
		site expr.Site
		src  string
		want bool
	}{
		{at(expr.PlaceRouteMatchWhen), `request.headers["x-tenant"] == "acme"`, true},
		{at(expr.PlaceRouteMatchWhen), `request.query.tag == "a,b"`, true},
		{at(expr.PlaceRouteMatchWhen), `now.getHours("+02:00") == 12`, true},
		{at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read")`, true},
		{at(expr.PlaceAuthzCELRule), `consumer != null && "daily-tokens" in consumer.quotas`, true},
		{at(expr.PlaceAuthzCELRule), `request.body.items.map(i, i + "!") == ["a!", "b!"]`, true},
		{at(expr.PlaceAccessLogWhen), `response.status >= 400 || duration > duration("1s")`, true},
		{at(expr.PlaceRetryOn), `error != null ? error.kind in ["connect", "reset"] : response.status == 503`, true},
		{expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnResponse}, `response.status == 200`, false},
	}
	for _, tc := range bools {
		got, err := mustProgram(t, c, tc.site, tc.src).EvalBool(t.Context(), testVars())
		if err != nil || got != tc.want {
			t.Errorf("EvalBool(%q) = %v, %v; want %v", tc.src, got, err, tc.want)
		}
	}
	strs := []struct {
		site expr.Site
		src  string
		want string
	}{
		{at(expr.PlaceRateLimitKey), `consumer == null ? source.ip : consumer.name`, "acme"},
		{at(expr.PlaceHashKey), `request.headers["x-user"]`, "u1"},
		{at(expr.PlaceHeadersRequestValue), `request.headers["accept"]`, "application/json, text/plain"},
		{at(expr.PlaceTransformResponseValue), `string(response.body.total)`, "12.5"},
		{sequential(expr.PlaceStepPathExpression), `"/orders/" + request.pathParams.orderId`, "/orders/42"},
	}
	for _, tc := range strs {
		got, err := mustProgram(t, c, tc.site, tc.src).EvalString(t.Context(), testVars())
		if err != nil || got != tc.want {
			t.Errorf("EvalString(%q) = %q, %v; want %q", tc.src, got, err, tc.want)
		}
	}
}

// TestEvalValue checks a dyn place: a map result written as a transform
// body with sorted keys.
func TestEvalValue(t *testing.T) {
	c := newCompiler(t)
	prg := mustProgram(t, c, at(expr.PlaceTransformRequestBody), `{"id": request.body.id, "kind": request.body.kind}`)
	v, err := prg.EvalValue(t.Context(), testVars())
	if err != nil {
		t.Fatalf("EvalValue: %v", err)
	}
	body, err := v.AppendBody(nil)
	if err != nil || string(body) != `{"id":7,"kind":"order"}` {
		t.Errorf("AppendBody = %s, %v", body, err)
	}
}

// TestEvalNilVars checks that a nil activation reads every variable as null.
func TestEvalNilVars(t *testing.T) {
	c := newCompiler(t)
	got, err := mustProgram(t, c, at(expr.PlaceAuthzCELRule), `consumer == null && request == null`).EvalBool(t.Context(), nil)
	if err != nil || !got {
		t.Errorf("EvalBool(nil) = %v, %v; want true", got, err)
	}
}

// TestEvalErrorKinds classifies each runtime failure and checks that the
// error text never carries request data.
func TestEvalErrorKinds(t *testing.T) {
	c := newCompiler(t)
	secret := func() *expr.Vars {
		v := testVars()
		v.Request.Header.Set("X-N", "secret-4242")
		return v
	}
	cases := []struct {
		name string
		site expr.Site
		src  string
		vars func() *expr.Vars
		bool bool
		want expr.ErrorKind
	}{
		{"null variable", at(expr.PlaceAuthzCELRule), `consumer.name == "x"`, func() *expr.Vars { v := testVars(); v.Consumer = nil; return v }, true, expr.KindNull},
		{"missing header", at(expr.PlaceRouteMatchWhen), `request.headers["x-nope"] == "x"`, nil, true, expr.KindNoSuchKey},
		{"missing body key", at(expr.PlaceAuthzCELRule), `request.body.nope == 1`, nil, true, expr.KindNoSuchKey},
		{"body not available", at(expr.PlaceAuthzCELRule), `request.body.kind == "x"`, func() *expr.Vars { v := testVars(); v.Request.Body = nil; return v }, true, expr.KindBody},
		{"conversion", at(expr.PlaceRouteMatchWhen), `int(request.headers["x-n"]) > 1`, secret, true, expr.KindConversion},
		{"division by zero", at(expr.PlaceRouteMatchWhen), `1 / (size(request.path) - size(request.path)) == 1`, nil, true, expr.KindArithmetic},
		{"no overload on dyn", at(expr.PlaceAuthzCELRule), `request.body.kind + 1 == 2`, nil, true, expr.KindNoSuchOverload},
		{"dyn result in a bool place", at(expr.PlaceAuthzCELRule), `request.body.kind`, nil, true, expr.KindResultType},
		{"dyn result in a string place", at(expr.PlaceTransformRequestValue), `request.body.id`, nil, false, expr.KindResultType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vars := testVars()
			if tc.vars != nil {
				vars = tc.vars()
			}
			prg := mustProgram(t, c, tc.site, tc.src)
			var err error
			if tc.bool {
				_, err = prg.EvalBool(t.Context(), vars)
			} else {
				_, err = prg.EvalString(t.Context(), vars)
			}
			var ee *expr.EvalError
			if !errors.As(err, &ee) {
				t.Fatalf("Eval(%q) error = %v, want *expr.EvalError", tc.src, err)
			}
			if ee.Kind != tc.want || ee.Place != tc.site.Place {
				t.Errorf("Eval(%q) = %s at %s, want %s at %s (detail %q)", tc.src, ee.Kind, ee.Place, tc.want, tc.site.Place, ee.Detail())
			}
			if want := "cel: " + string(tc.site.Place) + ": " + tc.want.String(); err.Error() != want {
				t.Errorf("Error() = %q, want %q", err.Error(), want)
			}
			if strings.Contains(err.Error(), "secret-4242") {
				t.Errorf("Error() carries request data: %q", err.Error())
			}
		})
	}
}

// longScope returns a JWT scope of n space-separated tokens, none of which
// is "orders:read", so the authz-orders comprehension visits all of them.
func longScope(n int) *expr.Vars {
	v := testVars()
	v.Auth.Claims = celtypes.FromNative(map[string]any{"scope": strings.TrimSpace(strings.Repeat("x ", n))})
	return v
}

// TestTrackedPrograms checks the tracking decision: a program whose cost is
// bounded within the runtime limit at any input size is untracked.
func TestTrackedPrograms(t *testing.T) {
	c := newCompiler(t)
	cases := []struct {
		site               expr.Site
		src                string
		tracked, stoppable bool
	}{
		{at(expr.PlaceRateLimitKey), `consumer == null ? source.ip : consumer.name`, false, false},
		{at(expr.PlaceRouteMatchWhen), `request.headers["x-tenant"] == "acme"`, false, false},
		{at(expr.PlaceRouteMatchWhen), `["GET", "HEAD"].exists(m, m == request.method)`, false, false},
		{at(expr.PlaceRouteMatchWhen), `request.headers["a"] == request.headers["b"]`, true, false},
		{at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read")`, true, true},
	}
	for _, tc := range cases {
		prg := mustProgram(t, c, tc.site, tc.src)
		if prg.tracked != tc.tracked || prg.stoppable != tc.stoppable {
			t.Errorf("%q: tracked %v, stoppable %v; want %v, %v", tc.src, prg.tracked, prg.stoppable, tc.tracked, tc.stoppable)
		}
	}
}

// TestStoppableProgramHonorsContext checks that a tracked comprehension stops
// at an ended context with KindDeadline, and that other programs ignore it.
func TestStoppableProgramHonorsContext(t *testing.T) {
	c := newCompiler(t)
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	orders := mustProgram(t, c, at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read")`)
	_, err := orders.EvalBool(ended, longScope(1000))
	var ee *expr.EvalError
	if !errors.As(err, &ee) || ee.Kind != expr.KindDeadline {
		t.Errorf("stoppable program under an ended context = %v, want KindDeadline", err)
	}
	// Few iterations finish before the first interrupt check.
	if got, err := orders.EvalBool(ended, testVars()); err != nil || !got {
		t.Errorf("short comprehension = %v, %v; want true", got, err)
	}
	for _, src := range []string{`request.headers["x-tenant"] == "acme"`, `["GET", "HEAD"].exists(m, m == request.method)`} {
		if got, err := mustProgram(t, c, at(expr.PlaceRouteMatchWhen), src).EvalBool(ended, testVars()); err != nil || !got {
			t.Errorf("%q under an ended context = %v, %v; want true", src, got, err)
		}
	}
}

// TestComprehensionStop checks that the stop bounds a tracked comprehension
// even when the request has no deadline. The stop is shortened to 1 ns, so
// it has passed before the first interrupt check.
func TestComprehensionStop(t *testing.T) {
	c := newCompiler(t)
	c.stop = time.Nanosecond
	prg := mustProgram(t, c, at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read")`)
	_, err := prg.EvalBool(context.Background(), longScope(1000))
	var ee *expr.EvalError
	if !errors.As(err, &ee) || ee.Kind != expr.KindDeadline {
		t.Errorf("EvalBool past the stop = %v, want KindDeadline", err)
	}
}

// TestRuntimeCostLimit checks that a tracked program whose actual cost
// passes 1,000,000 units fails with KindCostLimit: two 64 KiB header values
// make contains cost (65536 * 0.1)^2, about 43 million units.
func TestRuntimeCostLimit(t *testing.T) {
	c := newCompiler(t)
	prg := mustProgram(t, c, at(expr.PlaceRouteMatchWhen), `request.headers["a"].contains(request.headers["b"])`)
	if !prg.tracked {
		t.Fatal("contains over header values is untracked")
	}
	v := testVars()
	v.Request.Header.Set("A", strings.Repeat("a", 64<<10))
	v.Request.Header.Set("B", strings.Repeat("b", 64<<10))
	_, err := prg.EvalBool(t.Context(), v)
	var ee *expr.EvalError
	if !errors.As(err, &ee) || ee.Kind != expr.KindCostLimit {
		t.Errorf("EvalBool over capped headers = %v, want KindCostLimit", err)
	}
	// The same program over small values stays under the limit.
	v = testVars()
	v.Request.Header.Set("A", "abc")
	v.Request.Header.Set("B", "b")
	if got, err := prg.EvalBool(t.Context(), v); err != nil || !got {
		t.Errorf("EvalBool over small headers = %v, %v; want true", got, err)
	}
}

// TestConcurrentEvaluation evaluates one program from many goroutines, each
// with its own activation, under the race detector.
func TestConcurrentEvaluation(t *testing.T) {
	c := newCompiler(t)
	prg := mustProgram(t, c, at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read") && request.body.items.size() == 2`)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			v := testVars()
			for range 50 {
				ok, err := prg.EvalBool(context.Background(), v)
				if err != nil || !ok {
					errs <- errors.Join(err, errors.New("result false"))
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestProgramAccessors checks the facts a Program reports.
func TestProgramAccessors(t *testing.T) {
	c := newCompiler(t)
	src := `consumer == null ? source.ip : consumer.name`
	prg := mustProgram(t, c, at(expr.PlaceQuotaKey), src)
	if prg.Place() != expr.PlaceQuotaKey || prg.Source() != src {
		t.Errorf("Place, Source = %s, %q", prg.Place(), prg.Source())
	}
	if cost := prg.Cost(); cost.Max == 0 || cost.Max < cost.Min {
		t.Errorf("Cost = %+v", cost)
	}
}
