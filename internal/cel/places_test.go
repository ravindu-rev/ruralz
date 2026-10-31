// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/expr"
)

// schemaPlace maps each x-ruralz-cel field of the schema, as "Definition.property",
// to its place. The headers places name the shared set[] definition of each
// side, which covers its add[] entries too.
func schemaPlace() map[string]expr.PlaceID {
	return map[string]expr.PlaceID{
		"RouteMatch.when":                      expr.PlaceRouteMatchWhen,
		"PolicySpec.when":                      expr.PlacePolicyWhen,
		"CompositionStep.pathExpression":       expr.PlaceStepPathExpression,
		"CompositionStep.when":                 expr.PlaceStepWhen,
		"AuthzCELConfig.rule":                  expr.PlaceAuthzCELRule,
		"RateLimitConfig.key":                  expr.PlaceRateLimitKey,
		"RateLimitConfig.cost":                 expr.PlaceRateLimitCost,
		"RateLimit.when":                       expr.PlaceRateLimitWhen,
		"QuotaConfig.key":                      expr.PlaceQuotaKey,
		"QuotaConfig.cost":                     expr.PlaceQuotaCost,
		"HeaderRequestSet.valueExpression":     expr.PlaceHeadersRequestValue,
		"HeaderResponseSet.valueExpression":    expr.PlaceHeadersResponseValue,
		"CacheConfig.key":                      expr.PlaceCacheKey,
		"TransformRequestConfig.body":          expr.PlaceTransformRequestBody,
		"TransformRequestSet.valueExpression":  expr.PlaceTransformRequestValue,
		"TransformResponseConfig.body":         expr.PlaceTransformResponseBody,
		"TransformResponseSet.valueExpression": expr.PlaceTransformResponseValue,
		"LoadBalancing.hashKey":                expr.PlaceHashKey,
		"Retries.retryOn":                      expr.PlaceRetryOn,
		"CircuitBreaker.failureWhen":           expr.PlaceFailureWhen,
		"AccessLog.when":                       expr.PlaceAccessLogWhen,
		"AISemanticCacheConfig.key":            expr.PlaceSemanticCacheKey,
		"AIModelCandidate.when":                expr.PlaceCandidateWhen,
		"Messaging.key":                        expr.PlaceMessagingKey,
	}
}

// celAnnotation is one x-ruralz-cel keyword.
type celAnnotation struct {
	Variables []string `json:"variables"`
	Result    string   `json:"result"`
}

// annotations returns every x-ruralz-cel keyword of a schema view by
// "Definition.property".
func annotations(t *testing.T, view []byte) map[string]celAnnotation {
	t.Helper()
	var doc struct {
		Defs map[string]struct {
			Properties map[string]struct {
				CEL *celAnnotation `json:"x-ruralz-cel"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(view, &doc); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	out := map[string]celAnnotation{}
	for def, d := range doc.Defs {
		for prop, p := range d.Properties {
			if p.CEL != nil {
				out[def+"."+prop] = *p.CEL
			}
		}
	}
	return out
}

// resultName returns the x-ruralz-cel spelling of a result type.
func resultName(r expr.Result) string {
	switch r {
	case expr.ResultBool:
		return "bool"
	case expr.ResultString:
		return "string"
	case expr.ResultDyn:
		return "dyn"
	case expr.ResultInt:
		return "int"
	default:
		return ""
	}
}

// TestPlacesMatchSchemaAnnotations maps every x-ruralz-cel field of both
// schema views to exactly one place with the same result and variables,
// and every place to exactly one field.
func TestPlacesMatchSchemaAnnotations(t *testing.T) {
	views := map[string][]byte{"authoring": schema.AuthoringV1alpha1(), "rendered": schema.RenderedV1alpha1()}
	mapping := schemaPlace()
	for name, view := range views {
		t.Run(name, func(t *testing.T) {
			got := annotations(t, view)
			seen := map[expr.PlaceID]string{}
			for field, a := range got {
				id, ok := mapping[field]
				if !ok {
					t.Errorf("x-ruralz-cel on %s maps to no place", field)
					continue
				}
				if prev, dup := seen[id]; dup {
					t.Errorf("place %s maps to %s and %s", id, prev, field)
				}
				seen[id] = field
				p, ok := expr.LookupPlace(id)
				if !ok {
					t.Errorf("place %s of %s is not in expr.Places", id, field)
					continue
				}
				if w := resultName(p.Result); a.Result != w {
					t.Errorf("%s result = %q, place %s has %q", field, a.Result, id, w)
				}
				// The variables are a set: the schema keeps its field order.
				gotVars, w := slices.Sorted(slices.Values(a.Variables)), slices.Sorted(slices.Values(p.Vars.Names()))
				if !slices.Equal(gotVars, w) {
					t.Errorf("%s variables = %v, place %s has %v", field, a.Variables, id, p.Vars.Names())
				}
			}
			for _, p := range expr.Places() {
				if _, ok := seen[p.ID]; !ok {
					t.Errorf("place %s has no x-ruralz-cel field in the %s view", p.ID, name)
				}
			}
		})
	}
}

// TestEveryPlaceResolves checks that every place of expr.Places resolves to
// an environment whose variables stay within the place's union.
func TestEveryPlaceResolves(t *testing.T) {
	for _, p := range expr.Places() {
		se, ok := resolve(expr.Site{Place: p.ID})
		if !ok {
			t.Errorf("resolve(%s) failed", p.ID)
			continue
		}
		if se.vars&^p.Vars != 0 {
			t.Errorf("resolve(%s).vars = %v, outside the place's %v", p.ID, se.vars.Names(), p.Vars.Names())
		}
	}
	if _, ok := resolve(expr.Site{Place: "Route.spec.bogus"}); ok {
		t.Error("resolve of an unknown place succeeded")
	}
}
