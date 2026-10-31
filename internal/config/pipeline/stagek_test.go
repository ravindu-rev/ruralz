// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"testing"
)

// TestStageKOutputCap covers RZ-CFG-034: an AIModel without
// limits.maxOutputTokens that a Route whose effective chain holds an
// ai.token-budget Policy reaches through an ai Upstream it names, in
// upstreams or a composition step, reported once at its limits or, without
// them, at the resource, with every such Route as a related location in
// canonical order; nothing for a model with the cap or a Route without
// the Policy.
func TestStageKOutputCap(t *testing.T) {
	const inherit = "  policies:\n    - name: budget\n"
	composition := "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: agg\nspec:\n  match:\n    path: {prefix: /agg}\n" + inherit +
		"  composition:\n    mode: sequential\n    steps:\n      - name: ask\n        upstream: llm\n        path: /v1/chat/completions\n"
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"model without limits", map[string]string{"ai.yaml": aiFiles("")}, []string{
			`ai.yaml:9:1 error RZ-CFG-034 AIModel/chat spec.limits.maxOutputTokens: AIModel "chat" sets no limits.maxOutputTokens, but a Route with an ai.token-budget Policy reaches it (set limits.maxOutputTokens: a Token Budget reserves it as the output cap when a client sends none) (reached from Route "chat" in ai.yaml:54:1)`,
		}},
		{"limits without the output cap", map[string]string{"ai.yaml": aiFiles("  limits: {maxInputTokens: 100}\n")}, []string{
			`ai.yaml:20:11 error RZ-CFG-034 AIModel/chat spec.limits.maxOutputTokens: AIModel "chat" sets no limits.maxOutputTokens, but a Route with an ai.token-budget Policy reaches it (set limits.maxOutputTokens: a Token Budget reserves it as the output cap when a client sends none) (reached from Route "chat" in ai.yaml:55:1)`,
		}},
		{"model with the output cap", map[string]string{"ai.yaml": aiFiles("  limits: {maxOutputTokens: 100}\n")}, nil},
		{"several Routes and a composition step", map[string]string{
			"ai.yaml": aiFiles(""),
			"r.yaml":  docs(routeDoc("other", "llm", inherit), routeDoc("plain", "llm", ""), composition),
		}, []string{
			`ai.yaml:9:1 error RZ-CFG-034 AIModel/chat spec.limits.maxOutputTokens: AIModel "chat" sets no limits.maxOutputTokens, but a Route with an ai.token-budget Policy reaches it (set limits.maxOutputTokens: a Token Budget reserves it as the output cap when a client sends none) (reached from Route "agg" in r.yaml:22:1) (reached from Route "chat" in ai.yaml:54:1) (reached from Route "other" in r.yaml:1:1)`,
		}},
	}
	p := newPipeline(t, Options{})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.files["ruralz.yaml"] = gatewayDoc
			res := run(t, p, bundle(tc.files), Request{})
			wantTexts(t, tc.name, res.Diagnostics, tc.want...)
		})
	}
}

// TestStageKGatewayBudget covers an ai.token-budget Policy attached at the
// Gateway: every Route inherits it, so each Route that reaches the model
// is related, and one that excludes the Policy is not.
func TestStageKGatewayBudget(t *testing.T) {
	res := run(t, newPipeline(t, Options{}), bundle(map[string]string{
		"ruralz.yaml": gatewayDoc + "  policies:\n    - name: budget\n",
		"ai.yaml":     aiFiles(""),
		"r.yaml":      docs(routeDoc("free", "llm", "  excludePolicies:\n    - name: budget\n"), routeDoc("paid", "llm", "")),
	}), Request{})
	wantTexts(t, "run", res.Diagnostics,
		`ai.yaml:9:1 error RZ-CFG-034 AIModel/chat spec.limits.maxOutputTokens: AIModel "chat" sets no limits.maxOutputTokens, but a Route with an ai.token-budget Policy reaches it (set limits.maxOutputTokens: a Token Budget reserves it as the output cap when a client sends none) (reached from Route "chat" in ai.yaml:54:1) (reached from Route "paid" in r.yaml:12:1)`,
	)
}
