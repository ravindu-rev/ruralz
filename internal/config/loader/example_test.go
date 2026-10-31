// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"slices"
	"testing"
)

// The complete annotated example Bundle of the configuration model, which
// the repository holds under examples/, beside its control/ directory.
const (
	examplesBundle       = "../../../examples/shop-bundle"
	examplesEnvironments = "../../../examples/control/environments.yaml"
)

// TestExampleBundle loads the example Bundle for each Environment of its
// control/environments.yaml: stages A to E find no error, prod applies
// overlays/prod/ and staging overlays/staging/, and
// ${OTEL_EXPORTER_OTLP_ENDPOINT} takes the Environment's value.
func TestExampleBundle(t *testing.T) {
	l := newLoader(t, Options{})
	base := []string{
		"Gateway/edge",
		"AIModel/support-chat", "AIProvider/anthropic-prod", "AIProvider/openai-prod",
		"Consumer/partner-acme", "Plugin/geo-block", "Policy/geo-block-default",
		"Policy/cors-default", "Policy/cors-partner", "Policy/jwt-default", "Policy/apikey-partner",
		"Policy/authz-orders", "Policy/ratelimit-global", "Policy/ratelimit-orders", "Policy/token-budget-partner",
		"Policy/semantic-cache-support", "Policy/headers-security", "Policy/headers-internal", "Policy/upstream-oauth",
		"Route/cart-grpc", "Route/orders-summary", "Route/support-chat",
		"Upstream/orders", "Upstream/inventory", "Upstream/cart", "Upstream/llm",
	}
	for _, c := range []struct {
		env, overlay, attempts, failures string
		candidates                       []string
	}{
		{"prod", "prod", "3", "10", []string{"primary", "secondary"}},
		{"staging", "staging", "2", "5", []string{"primary"}},
	} {
		r, err := l.Load(t.Context(), Request{
			Source:       Source{Path: examplesBundle},
			Environments: Source{Path: examplesEnvironments}, Environment: c.env,
		})
		if err != nil {
			t.Fatalf("%s: %v", c.env, err)
		}
		if r.HasErrors() || r.Stopped {
			t.Fatalf("%s: diagnostics\n%q", c.env, texts(r.Diagnostics))
		}
		registered(t, r.Diagnostics)
		if got := ids(r.Resources); !slices.Equal(got, base) {
			t.Errorf("%s: resources\n%q\nwant\n%q", c.env, got, base)
		}
		if r.Overlay != c.overlay {
			t.Errorf("%s: overlay %q", c.env, r.Overlay)
		}
		orders := find(t, r.Resources, "Upstream/orders").Root
		if a, f := at(t, orders, "spec.retries.attempts").Text, at(t, orders, "spec.circuitBreaker.consecutiveFailures").Text; a != c.attempts || f != c.failures {
			t.Errorf("%s: orders retries.attempts %s, consecutiveFailures %s", c.env, a, f)
		}
		if got := at(t, orders, "spec.retries.perTryTimeout").Text; got != "1s" {
			t.Errorf("%s: an overlay dropped retries.perTryTimeout: %q", c.env, got)
		}
		var names []string
		model := find(t, r.Resources, "AIModel/support-chat").Root
		for _, cand := range at(t, model, "spec.candidates").Items {
			names = append(names, at(t, cand, "name").Text)
		}
		if !slices.Equal(names, c.candidates) {
			t.Errorf("%s: candidates %q, want %q", c.env, names, c.candidates)
		}
		if c.env == "prod" {
			if got := at(t, model, "spec.candidates.1.when").Text; got != "ai.estimatedInputTokens < 50000" {
				t.Errorf("prod: the secondary candidate's when is %q, want the overlay's", got)
			}
		}
		if got := at(t, find(t, r.Resources, "Gateway/edge").Root, "spec.telemetry.otlp.endpoint").Text; got != "https://otel-collector.observability:4317" {
			t.Errorf("%s: otlp.endpoint %q", c.env, got)
		}
	}
}

// TestExampleBundleWithoutEnvironment loads the example Bundle without an
// Environment: no overlay, and the default of
// ${OTEL_EXPORTER_OTLP_ENDPOINT:-...} when the variable is unset.
func TestExampleBundleWithoutEnvironment(t *testing.T) {
	r, err := newLoader(t, Options{}).Load(t.Context(), Request{Source: Source{Path: examplesBundle}})
	if err != nil {
		t.Fatal(err)
	}
	if r.HasErrors() {
		t.Fatalf("diagnostics\n%q", texts(r.Diagnostics))
	}
	if got := at(t, find(t, r.Resources, "Gateway/edge").Root, "spec.telemetry.otlp.endpoint").Text; got != "https://otel-collector:4317" {
		t.Errorf("otlp.endpoint %q, want the default", got)
	}
	if r.Overlay != "" {
		t.Errorf("overlay %q", r.Overlay)
	}
}
