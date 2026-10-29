// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// The tracking rule of docs/architecture/02-configuration-model.md
// "Limits": a program evaluates without cost tracking only when its
// estimate with its inputs at the snapshot's caps (expr.InputCaps) stays
// within the runtime cost limit, and every input no cap bounds counts as
// unbounded.

const (
	ordersRule = `auth.claims.scope.split(" ").exists(s, s == "orders:read")`
	quotaRule  = `consumer != null && "daily-tokens" in consumer.quotas`
	// headerLiteral reads one header value against a literal: bounded by
	// the header cap.
	headerLiteral = `request.headers["x-a"].contains("b")`
	// headerPair compares two header values: (65536 × 0.1)² units at the
	// 64 KiB header cap, above the limit.
	headerPair = `request.headers["a"].contains(request.headers["b"])`
	// routePair searches a Route label for the Route's name: bounded by the
	// Routes' caps, unbounded without them.
	routePair = `route.labels.team.contains(route.name)`
)

// routeSite is the site of a Route-scope Policy field of place.
func routeSite(place expr.PlaceID) expr.Site {
	return expr.Site{Place: place, Scope: phase.ScopeRoute}
}

// TestTrackingFollowsTheCaps checks the decision for each kind of input:
// a capped input lets a rule run untracked when its bound stays within the
// limit; a rule that could pass the limit under the caps, a rule over an
// input no cap bounds, and an outgoing request stay tracked.
func TestTrackingFollowsTheCaps(t *testing.T) {
	caps := exampleCaps()
	unknown := exampleCaps()
	unknown.Consumers = expr.ConsumerCaps{}
	unknownRoutes := exampleCaps()
	unknownRoutes.Routes = expr.RouteCaps{}
	longRoutes := exampleCaps()
	longRoutes.Routes = expr.RouteCaps{Known: true, NameBytes: 64 << 10, Labels: 1, LabelBytes: 64 << 10}
	manyQuotas := exampleCaps()
	manyQuotas.Consumers.Quotas = 2 << 20
	noHeaderCap := exampleCaps()
	noHeaderCap.RequestHeaderBytes = 0
	unboundedEdits := exampleCaps()
	unboundedEdits.AddedHeaderBytes = -1
	added := exampleCaps()
	added.AddedHeaderBytes = 16 << 20
	rule := routeSite(expr.PlaceAuthzCELRule)
	tests := []struct {
		name    string
		site    expr.Site
		src     string
		caps    expr.InputCaps
		tracked bool
	}{
		{"orders rule, inputs unbounded", rule, ordersRule, expr.InputCaps{}, true},
		{"orders rule, claims at 4 × 16 KiB", rule, ordersRule, caps, false},
		{"quota rule, inputs unbounded", rule, quotaRule, expr.InputCaps{}, true},
		{"quota rule, Consumers at their compiled size", rule, quotaRule, caps, false},
		{"quota rule, Consumers not known", rule, quotaRule, unknown, true},
		{"quota rule, a Consumer with 2 Mi quotas", rule, quotaRule, manyQuotas, true},
		{"path concatenation, inputs unbounded", sequential(expr.PlaceStepPathExpression), `"/orders/" + request.pathParams.orderId`, expr.InputCaps{}, true},
		{"path concatenation, at the header cap", sequential(expr.PlaceStepPathExpression), `"/orders/" + request.pathParams.orderId`, caps, false},
		{"one header value, at the header cap", rule, headerLiteral, caps, false},
		{"one header value, no header cap", rule, headerLiteral, noHeaderCap, true},
		{"one header value, unbounded Policy edits", rule, headerLiteral, unboundedEdits, true},
		{"one header value, 16 MiB of Policy edits", rule, headerLiteral, added, true},
		{"two header values, at the header cap", rule, headerPair, caps, true},
		{"two claims, at the claims cap", rule, `auth.claims.a.contains(auth.claims.b)`, caps, true},
		{"a comprehension over the header names", at(expr.PlaceRouteMatchWhen), `request.headers.exists(k, k == "x-a")`, caps, false},
		{"nested comprehensions over the header names", at(expr.PlaceRouteMatchWhen), `request.headers.exists(k, request.headers.exists(j, j == "x-a"))`, caps, true},
		{"split, map and exists at the header cap", at(expr.PlaceRouteMatchWhen), `request.headers["x-tenant"].split(",").map(s, s).exists(s, s == "zz")`, caps, true},
		{"a comprehension over the body", rule, `request.body.items.exists(i, i == "z")`, caps, true},
		{"a Route label and name, inputs unbounded", rule, routePair, expr.InputCaps{}, true},
		{"a Route label and name, Routes at their compiled size", rule, routePair, caps, false},
		{"a Route label and name, Routes not known", rule, routePair, unknownRoutes, true},
		{"a Route label and name, a Route of 64 KiB", rule, routePair, longRoutes, true},
		{"a comprehension over the Route labels", rule, `route.labels.exists(k, k.startsWith("te"))`, caps, false},
		{"a Consumer tag", rule, `consumer.tags.exists(t, t.contains("eu"))`, caps, false},
		{"Route-scope request header value", routeSite(expr.PlaceHeadersRequestValue), headerLiteral + ` ? "y" : "n"`, caps, false},
		{"Upstream-scope request header value", expr.Site{Place: expr.PlaceHeadersRequestValue, Scope: phase.ScopeUpstream}, headerLiteral + ` ? "y" : "n"`, caps, true},
		{"when first evaluated in onUpstreamRequest", expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnUpstreamRequest}, headerLiteral, caps, true},
		{"when first evaluated in onRequestHeaders", expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnRequestHeaders}, headerLiteral, caps, false},
		{"retryOn, over the attempt's request", at(expr.PlaceRetryOn), headerLiteral, caps, true},
	}
	c := newCompiler(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc, iss := c.compile(tt.site, tt.src)
			if cc == nil {
				t.Fatalf("compile(%q) = %+v", tt.src, iss)
			}
			bound, err := cc.cappedBound(tt.caps)
			if err != nil {
				t.Fatal(err)
			}
			prg := mustCapped(t, c, tt.site, tt.src, tt.caps)
			if prg.tracked != tt.tracked {
				t.Errorf("%q tracked %v with a bound of %d units, want %v", tt.src, prg.tracked, bound, tt.tracked)
			}
			if prg.stoppable != (tt.tracked && cc.an.folds) {
				t.Errorf("%q stoppable %v, want %v: only a tracked comprehension stops", tt.src, prg.stoppable, tt.tracked && cc.an.folds)
			}
		})
	}
}

// TestCapOf checks the cap of each input path: the client request's
// fields at the header cap plus the Policy edits, the claims at their cap,
// the Consumer fields and their elements at the Consumers' maxima, and
// nothing for an outgoing request, an unknown or negative cap, or any
// other variable.
func TestCapOf(t *testing.T) {
	caps := expr.InputCaps{
		RequestHeaderBytes: 1000, AddedHeaderBytes: 24, ClaimsBytes: 4096,
		Routes: expr.RouteCaps{Known: true, NameBytes: 10, Labels: 11, LabelBytes: 12},
		Consumers: expr.ConsumerCaps{
			Known: true, NameBytes: 5, TierBytes: 6, Tags: 3, TagBytes: 7,
			Labels: 4, LabelBytes: 8, Quotas: 2, QuotaNameBytes: 9,
		},
	}
	tests := []struct {
		path   string
		client bool
		caps   expr.InputCaps
		want   uint64
		capped bool
	}{
		{"request.method", true, caps, 1024, true},
		{"request.scheme", true, caps, 1024, true},
		{"request.host", true, caps, 1024, true},
		{"request.path", true, caps, 1024, true},
		{"request.pathParams.@values", true, caps, 1024, true},
		{"request.query.@keys", true, caps, 1024, true},
		{"request.headers", true, caps, 1024, true},
		{"request.headers.@values", true, caps, 1024, true},
		{"request.headers", false, caps, 0, false},
		{"request.body", true, caps, 0, false},
		{"request.body.items.@items", true, caps, 0, false},
		{"request", true, caps, 0, false},
		{"auth.claims", true, caps, 4096, true},
		{"auth.claims.scope", false, caps, 4096, true},
		{"auth.claims.aud.@items", true, caps, 4096, true},
		{"auth.method", true, caps, 0, false},
		{"consumer.name", true, caps, 5, true},
		{"consumer.tier", true, caps, 6, true},
		{"consumer.tags", true, caps, 3, true},
		{"consumer.tags.@items", true, caps, 7, true},
		{"consumer.labels", true, caps, 4, true},
		{"consumer.labels.@keys", true, caps, 8, true},
		{"consumer.labels.@values", true, caps, 8, true},
		{"consumer.quotas", true, caps, 2, true},
		{"consumer.quotas.@items", true, caps, 9, true},
		{"route.name", true, caps, 10, true},
		{"route.labels", true, caps, 11, true},
		{"route.labels.@keys", true, caps, 12, true},
		{"route.labels.@values", true, caps, 12, true},
		{"route.labels.team", true, caps, 12, true},
		{"route.name", true, expr.InputCaps{}, 0, false},
		{"route.labels", true, expr.InputCaps{Routes: expr.RouteCaps{Known: true, Labels: -1}}, 0, false},
		{"route.other", true, caps, 0, false},
		{"source.ip", true, caps, 0, false},
		{"response.headers", true, caps, 0, false},
		{"steps.order.body", true, caps, 0, false},
		{"@items", true, caps, 0, false},
		{"request.headers", true, expr.InputCaps{AddedHeaderBytes: 24}, 0, false},
		{"request.headers", true, expr.InputCaps{RequestHeaderBytes: 1000, AddedHeaderBytes: -1}, 0, false},
		{"auth.claims", true, expr.InputCaps{}, 0, false},
		{"consumer.name", true, expr.InputCaps{}, 0, false},
		{"consumer.name", true, expr.InputCaps{Consumers: expr.ConsumerCaps{Known: true}}, 0, true},
		{"consumer.tags", true, expr.InputCaps{Consumers: expr.ConsumerCaps{Known: true, Tags: -1}}, 0, false},
	}
	for _, tt := range tests {
		e := cappedEstimator{caps: tt.caps, clientRequest: tt.client}
		got, ok := e.capOf(strings.Split(tt.path, "."))
		if got != tt.want || ok != tt.capped {
			t.Errorf("capOf(%s, client %v) = %d, %v; want %d, %v", tt.path, tt.client, got, ok, tt.want, tt.capped)
		}
	}
}

// TestClientRequestSites checks which sites read the client request, whose
// header block the caps bound: the client-leg places, and not the
// outgoing request of an Upstream-scope Policy, an upstream-leg
// Policy.spec.when or an Upstream field.
func TestClientRequestSites(t *testing.T) {
	tests := []struct {
		site expr.Site
		want bool
	}{
		{routeSite(expr.PlaceAuthzCELRule), true},
		{at(expr.PlaceRouteMatchWhen), true},
		{expr.Site{Place: expr.PlaceRateLimitKey, Scope: phase.ScopeGateway}, true},
		{routeSite(expr.PlaceHeadersResponseValue), true},
		{at(expr.PlaceAccessLogWhen), true},
		{sequential(expr.PlaceStepPathExpression), true},
		{expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnRequestBody}, true},
		{expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnResponse}, true},
		{expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnChunk, Scope: phase.ScopeRoute}, true},
		{expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnChunk, Scope: phase.ScopeUpstream}, false},
		{expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnUpstreamRequest}, false},
		{expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnUpstreamResponseHeaders}, false},
		{expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnUpstreamResponseBody}, false},
		{expr.Site{Place: expr.PlaceHeadersRequestValue, Scope: phase.ScopeUpstream}, false},
		{expr.Site{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeUpstream}, false},
		{at(expr.PlaceHashKey), false},
		{at(expr.PlaceRetryOn), false},
		{at(expr.PlaceFailureWhen), false},
		{at(expr.PlaceMessagingKey), false},
	}
	for _, tt := range tests {
		se, ok := resolve(tt.site)
		if !ok {
			t.Fatalf("resolve(%+v) failed", tt.site)
		}
		if se.clientRequest != tt.want {
			t.Errorf("site %+v reads the client request: %v, want %v", tt.site, se.clientRequest, tt.want)
		}
	}
}

// TestRuleWithinTheCapsIgnoresTheStop checks that the orders rule, untracked
// under the caps because its bound proves it ends within the cost limit,
// has no comprehension stop: an ended context does not stop it, and it
// decides over a scope at the claims cap. Without caps it is tracked and
// stops (TestStoppableProgramHonorsContext).
func TestRuleWithinTheCapsIgnoresTheStop(t *testing.T) {
	c := newCompiler(t)
	prg := mustCapped(t, c, at(expr.PlaceAuthzCELRule), ordersRule, exampleCaps())
	if prg.tracked || prg.stoppable {
		t.Fatalf("orders rule under the caps: tracked %v, stoppable %v; want neither", prg.tracked, prg.stoppable)
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	// 32,762 one-byte scopes and a final orders:read: a 65,535-byte scope,
	// within the 64 KiB claims cap.
	v := testVars()
	v.Auth.Claims = celtypes.FromNative(map[string]any{"scope": strings.Repeat("x ", 32<<10-6) + "orders:read"})
	if got, err := prg.EvalBool(ended, v); err != nil || !got {
		t.Errorf("EvalBool under an ended context = %v, %v; want true", got, err)
	}
}

// TestRuleThatCanPassTheLimitUnderTheCapsIsTracked checks the guarantee of
// the cost limit: two header values compared by contains could cost about
// 43 million units at the 64 KiB header cap, so the rule stays tracked
// under the caps and fails with KindCostLimit at those sizes.
func TestRuleThatCanPassTheLimitUnderTheCapsIsTracked(t *testing.T) {
	c := newCompiler(t)
	prg := mustCapped(t, c, at(expr.PlaceRouteMatchWhen), headerPair, exampleCaps())
	if !prg.tracked {
		t.Fatal("contains over two capped header values is untracked")
	}
	v := testVars()
	v.Request.Header.Set("A", strings.Repeat("a", 32<<10))
	v.Request.Header.Set("B", strings.Repeat("b", 32<<10))
	_, err := prg.EvalBool(t.Context(), v)
	var ee *expr.EvalError
	if !errors.As(err, &ee) || ee.Kind != expr.KindCostLimit {
		t.Errorf("EvalBool at the header cap = %v, want KindCostLimit", err)
	}
}

// TestTrackedComprehensionUnderTheCapsStops checks the guarantee of the
// comprehension stop: a comprehension over the body, which no cap bounds,
// stays tracked and stoppable under the caps and stops at an ended
// context with KindDeadline.
func TestTrackedComprehensionUnderTheCapsStops(t *testing.T) {
	c := newCompiler(t)
	prg := mustCapped(t, c, at(expr.PlaceAuthzCELRule), `request.body.items.exists(i, i == "z")`, exampleCaps())
	if !prg.tracked || !prg.stoppable {
		t.Fatalf("body comprehension under the caps: tracked %v, stoppable %v; want both", prg.tracked, prg.stoppable)
	}
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	items := make([]any, 1000)
	for i := range items {
		items[i] = "a"
	}
	v := testVars()
	v.Request.Body = celtypes.FromNative(map[string]any{"items": items})
	_, err := prg.EvalBool(ended, v)
	var ee *expr.EvalError
	if !errors.As(err, &ee) || ee.Kind != expr.KindDeadline {
		t.Errorf("EvalBool under an ended context = %v, want KindDeadline", err)
	}
}

// TestBuilderKeysProgramsByCaps checks that a reload reuses a program only
// under equal caps: the same source under new caps is planned again, with
// the tracking those caps decide, and under equal caps it is reused.
func TestBuilderKeysProgramsByCaps(t *testing.T) {
	c := newCompiler(t)
	site := at(expr.PlaceAuthzCELRule)
	b1 := c.NewBuilder(nil, expr.InputCaps{})
	unbounded, _ := b1.Compile(site, ordersRule)
	set1 := b1.Build()
	b2 := c.NewBuilder(set1, exampleCaps())
	capped, _ := b2.Compile(site, ordersRule)
	if capped == unbounded {
		t.Fatal("new caps reused the program planned without caps")
	}
	if !unbounded.(*Program).tracked || capped.(*Program).tracked {
		t.Errorf("tracked %v without caps and %v with caps, want true and false", unbounded.(*Program).tracked, capped.(*Program).tracked)
	}
	set2 := b2.Build()
	again, _ := c.NewBuilder(set2, exampleCaps()).Compile(site, ordersRule)
	if again != capped {
		t.Errorf("equal caps recompiled %q: %p, want %p", ordersRule, again, capped)
	}
	// Caps that differ only in the Routes plan the Route rule again.
	b3 := c.NewBuilder(nil, exampleCaps())
	routeCapped, _ := b3.Compile(site, routePair)
	noRoutes := exampleCaps()
	noRoutes.Routes = expr.RouteCaps{}
	routeUnbounded, _ := c.NewBuilder(b3.Build(), noRoutes).Compile(site, routePair)
	if routeUnbounded == routeCapped {
		t.Fatal("caps without Routes reused the program planned with them")
	}
	if routeCapped.(*Program).tracked || !routeUnbounded.(*Program).tracked {
		t.Errorf("Route rule tracked %v with Route caps and %v without, want false and true", routeCapped.(*Program).tracked, routeUnbounded.(*Program).tracked)
	}
}
