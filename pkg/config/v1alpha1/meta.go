// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// APIVersion is the apiVersion of every resource in this package.
const APIVersion = "ruralz/v1alpha1"

// Kind names a resource kind.
type Kind string

// The ten fixed kinds (foundation pack section 3).
const (
	// KindGateway holds listeners, TLS, admin, telemetry, limits, the State Store and Gateway Policies.
	KindGateway Kind = "Gateway"
	// KindRoute matches requests and sends them to Upstreams.
	KindRoute Kind = "Route"
	// KindUpstream is a target service.
	KindUpstream Kind = "Upstream"
	// KindPolicy configures one Filter or Plugin.
	KindPolicy Kind = "Policy"
	// KindPlugin is a WASM Plugin artifact and what it may do.
	KindPlugin Kind = "Plugin"
	// KindConsumer is a caller identity.
	KindConsumer Kind = "Consumer"
	// KindAIProvider is one LLM provider account.
	KindAIProvider Kind = "AIProvider"
	// KindAIModel maps a virtual model name to ordered candidates.
	KindAIModel Kind = "AIModel"
	// KindEnvironment groups Clusters; control plane only.
	KindEnvironment Kind = "Environment"
	// KindCluster is a Rollout target; control plane only.
	KindCluster Kind = "Cluster"
)

// TypeMeta holds the apiVersion and kind of a resource.
type TypeMeta struct {
	// APIVersion is always ruralz/v1alpha1 for these types.
	APIVersion string `json:"apiVersion"`
	// Kind is the resource kind.
	Kind Kind `json:"kind"`
}

// ObjectMeta identifies a resource. Only name, labels and annotations
// exist; namespace and status are rejected in Bundle form.
type ObjectMeta struct {
	// Name is unique per kind within a rendered Bundle: an RFC 1123 label of 1 to 63 characters.
	// +ruralz:required
	// +ruralz:pattern=^[a-z0-9]([-a-z0-9]*[a-z0-9])?$
	// +ruralz:minLength=1
	// +ruralz:maxLength=63
	Name string `json:"name"`
	// Labels are free-form string pairs; the ruralz.io/ prefix is reserved.
	Labels map[string]string `json:"labels,omitempty"`
	// Annotations are free-form string pairs; the ruralz.io/ prefix is reserved.
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Gateway holds listeners, TLS, admin, telemetry, global limits, the State
// Store connection and Gateway-scoped Policies. Exactly one per Bundle, in
// ruralz.yaml.
type Gateway struct {
	TypeMeta
	// Metadata identifies the Gateway.
	Metadata ObjectMeta `json:"metadata"`
	// Spec is the Gateway specification.
	Spec GatewaySpec `json:"spec"`
}

// Route matches requests, attaches Policies and forwards to Upstreams,
// directly or through composition.
type Route struct {
	TypeMeta
	// Metadata identifies the Route.
	Metadata ObjectMeta `json:"metadata"`
	// Spec is the Route specification.
	Spec RouteSpec `json:"spec"`
}

// Upstream is a target service.
type Upstream struct {
	TypeMeta
	// Metadata identifies the Upstream.
	Metadata ObjectMeta `json:"metadata"`
	// Spec is the Upstream specification.
	Spec UpstreamSpec `json:"spec"`
}

// Policy is named, reusable configuration of one registered type for a
// Filter or a Plugin.
type Policy struct {
	TypeMeta
	// Metadata identifies the Policy.
	Metadata ObjectMeta `json:"metadata"`
	// Spec is the Policy specification.
	Spec PolicySpec `json:"spec"`
}

// Plugin is a WASM artifact and the Capabilities it may use.
type Plugin struct {
	TypeMeta
	// Metadata identifies the Plugin.
	Metadata ObjectMeta `json:"metadata"`
	// Spec is the Plugin specification.
	Spec PluginSpec `json:"spec"`
}

// Consumer is a caller identity with credentials, a Tier, Quotas and tags.
type Consumer struct {
	TypeMeta
	// Metadata identifies the Consumer.
	Metadata ObjectMeta `json:"metadata"`
	// Spec is the Consumer specification.
	Spec ConsumerSpec `json:"spec"`
}

// AIProvider is one LLM provider account.
type AIProvider struct {
	TypeMeta
	// Metadata identifies the AIProvider.
	Metadata ObjectMeta `json:"metadata"`
	// Spec is the AIProvider specification.
	Spec AIProviderSpec `json:"spec"`
}

// AIModel maps a virtual model name, its metadata.name, to ordered
// candidates.
type AIModel struct {
	TypeMeta
	// Metadata identifies the AIModel; its name is the model clients send.
	Metadata ObjectMeta `json:"metadata"`
	// Spec is the AIModel specification.
	Spec AIModelSpec `json:"spec"`
}

// Environment groups Clusters, defines a promotion path and holds
// non-secret variables. It is read by Ruralz Control and CLI renders and is
// never part of a Bundle.
type Environment struct {
	TypeMeta
	// Metadata identifies the Environment.
	Metadata ObjectMeta `json:"metadata"`
	// Spec is the Environment specification.
	Spec EnvironmentSpec `json:"spec"`
}

// Cluster is a Rollout target: enrolled Nodes that share one Revision. It is
// read only by Ruralz Control and is never part of a Bundle.
type Cluster struct {
	TypeMeta
	// Metadata identifies the Cluster.
	Metadata ObjectMeta `json:"metadata"`
	// Spec is the Cluster specification.
	Spec ClusterSpec `json:"spec"`
}
