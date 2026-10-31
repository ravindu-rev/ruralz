// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestResolveRefinesBySite checks each refinement a site applies to its
// place: the response of Policy.spec.when by first Phase, the bodies by
// gate, upstream by scope, and the composition fields.
func TestResolveRefinesBySite(t *testing.T) {
	base := expr.VarsBase
	cases := []struct {
		name              string
		site              expr.Site
		vars              expr.Var
		reqBody, respBody bool
	}{
		{"match when", expr.Site{Place: expr.PlaceRouteMatchWhen}, expr.VarRequest | expr.VarSource | expr.VarNow, false, false},
		{"when at a request Phase", expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnRequestHeaders}, base, false, false},
		{"when of a body gate", expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnRequestBody, GatesBody: true}, base, true, false},
		{"when at a response Phase", expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnResponse, PolicyType: v1alpha1.PolicyTypeHeaders}, base | expr.VarResponse, false, false},
		{"when at onUpstreamResponseHeaders", expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnUpstreamResponseHeaders}, base | expr.VarResponse, false, false},
		{"when of transform.response", expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnResponse, PolicyType: v1alpha1.PolicyTypeTransformResponse}, base | expr.VarResponse, false, true},
		{"when of transform.response at a request Phase", expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnRequestHeaders, PolicyType: v1alpha1.PolicyTypeTransformResponse}, base, false, false},
		{"authz.cel rule", expr.Site{Place: expr.PlaceAuthzCELRule}, base, true, false},
		{"step path", expr.Site{Place: expr.PlaceStepPathExpression, Mode: v1alpha1.CompositionModeSequential}, base | expr.VarSteps, true, false},
		{"step when", expr.Site{Place: expr.PlaceStepWhen, Mode: v1alpha1.CompositionModeConditional}, base | expr.VarSteps, true, false},
		{"rate limit key", expr.Site{Place: expr.PlaceRateLimitKey, Scope: phase.ScopeGateway}, base, false, false},
		{"headers request at Upstream", expr.Site{Place: expr.PlaceHeadersRequestValue, Scope: phase.ScopeUpstream}, base, false, false},
		{"headers response", expr.Site{Place: expr.PlaceHeadersResponseValue, Scope: phase.ScopeRoute}, base | expr.VarResponse | expr.VarUpstream, false, false},
		{"transform.request at Route", expr.Site{Place: expr.PlaceTransformRequestBody, Scope: phase.ScopeRoute}, base, true, false},
		{"transform.request at Upstream", expr.Site{Place: expr.PlaceTransformRequestValue, Scope: phase.ScopeUpstream}, base | expr.VarUpstream, true, false},
		{"transform.response at Gateway", expr.Site{Place: expr.PlaceTransformResponseBody, Scope: phase.ScopeGateway}, base | expr.VarResponse, false, true},
		{"transform.response at Upstream", expr.Site{Place: expr.PlaceTransformResponseValue, Scope: phase.ScopeUpstream}, base | expr.VarResponse | expr.VarUpstream, false, true},
		{"retryOn", expr.Site{Place: expr.PlaceRetryOn}, expr.VarRequest | expr.VarResponse | expr.VarError | expr.VarAttempt | expr.VarUpstream, false, false},
		{"access log", expr.Site{Place: expr.PlaceAccessLogWhen}, base | expr.VarResponse | expr.VarUpstream | expr.VarDuration, false, false},
		{"semantic cache key", expr.Site{Place: expr.PlaceSemanticCacheKey}, base | expr.VarAI, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			se, ok := resolve(tc.site)
			if !ok {
				t.Fatalf("resolve(%+v) failed", tc.site)
			}
			if se.vars != tc.vars {
				t.Errorf("vars = %v, want %v", se.vars.Names(), tc.vars.Names())
			}
			if se.reqBody != tc.reqBody || se.respBody != tc.respBody {
				t.Errorf("bodies = request %v, response %v; want %v, %v", se.reqBody, se.respBody, tc.reqBody, tc.respBody)
			}
		})
	}
}

// TestResolveKeepsCompositionFields checks that only the composition places
// carry the mode and earlier steps into the site key.
func TestResolveKeepsCompositionFields(t *testing.T) {
	earlier := []string{"order", "stock"}
	se, _ := resolve(expr.Site{Place: expr.PlaceStepWhen, Mode: v1alpha1.CompositionModeSequential, EarlierSteps: earlier})
	if !se.composition || se.mode != v1alpha1.CompositionModeSequential || !slices.Equal(se.earlier, earlier) {
		t.Errorf("composition site = %+v", se)
	}
	other, _ := resolve(expr.Site{Place: expr.PlaceRouteMatchWhen, Mode: v1alpha1.CompositionModeSequential, EarlierSteps: earlier})
	if other.composition || other.key().steps != "" {
		t.Errorf("match site carries composition fields: %+v", other)
	}
	a, _ := resolve(expr.Site{Place: expr.PlaceStepWhen, Mode: v1alpha1.CompositionModeSequential, EarlierSteps: []string{"order"}})
	if a.key() == se.key() {
		t.Error("sites with different earlier steps share a key")
	}
}
