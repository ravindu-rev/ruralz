// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// ClusterSpec is the spec of a Cluster. Planned (M2).
type ClusterSpec struct {
	// Environment is the metadata.name of the Cluster's Environment.
	// +ruralz:required
	// +ruralz:ref=Environment
	Environment string `json:"environment"`
	// Region is the Cluster's Region.
	Region string `json:"region,omitempty"`
	// Rollout configures how Revisions reach the Cluster.
	Rollout *ClusterRollout `json:"rollout,omitempty"`
}

// RolloutStrategy is how a Rollout reaches a Cluster's Nodes.
type RolloutStrategy string

// Rollout strategies.
const (
	// RolloutStrategyAllAtOnce delivers to every Node at once.
	RolloutStrategyAllAtOnce RolloutStrategy = "all-at-once"
	// RolloutStrategyCanary delivers to a percentage of Nodes first.
	RolloutStrategyCanary RolloutStrategy = "canary"
)

// ClusterRollout configures Rollouts to a Cluster.
type ClusterRollout struct {
	// Strategy is all-at-once or canary.
	Strategy *RolloutStrategy `json:"strategy,omitempty"`
	// Canary configures the canary step.
	Canary *Canary `json:"canary,omitempty"`
	// AutoRollback ends a failing Rollout in rolled-back; when false it enters paused.
	AutoRollback *bool `json:"autoRollback,omitempty"`
}

// Canary configures the canary step of a Rollout.
type Canary struct {
	// Percent is the share of Nodes in the canary step.
	// +ruralz:minimum=0
	// +ruralz:maximum=100
	Percent *int32 `json:"percent,omitempty"`
	// Bake is how long the canary runs before the Rollout progresses.
	Bake *Duration `json:"bake,omitempty"`
}
