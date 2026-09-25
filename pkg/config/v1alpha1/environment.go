// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// EnvironmentSpec is the spec of an Environment. Every field is optional;
// CLI renders read only overlay and variables.
type EnvironmentSpec struct {
	// Overlay selects overlays/<overlay>/; default the Environment's metadata.name.
	Overlay string `json:"overlay,omitempty"`
	// Promotion is the promotion path into this Environment; chains MUST be acyclic (RZ-CFG-022).
	Promotion *Promotion `json:"promotion,omitempty"`
	// Variables are non-secret values for ${VAR} substitution.
	Variables map[string]string `json:"variables,omitempty"`
}

// Promotion is the promotion path into an Environment.
type Promotion struct {
	// From is the metadata.name of the Environment where a source commit must complete a Rollout first.
	// +ruralz:required
	// +ruralz:ref=Environment
	From string `json:"from"`
	// RequireApproval holds promotions for approval.
	RequireApproval *bool `json:"requireApproval,omitempty"`
}
