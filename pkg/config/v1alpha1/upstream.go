// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// UpstreamProtocol is the protocol an Upstream speaks.
type UpstreamProtocol string

// Upstream protocols.
const (
	// UpstreamProtocolHTTP is Planned (M1).
	UpstreamProtocolHTTP UpstreamProtocol = "http"
	// UpstreamProtocolGRPC is Planned (M3).
	UpstreamProtocolGRPC UpstreamProtocol = "grpc"
	// UpstreamProtocolGraphQL is Planned (M3).
	UpstreamProtocolGraphQL UpstreamProtocol = "graphql"
	// UpstreamProtocolWebSocket is Planned (M3).
	UpstreamProtocolWebSocket UpstreamProtocol = "websocket"
	// UpstreamProtocolKafka is Planned (M4).
	UpstreamProtocolKafka UpstreamProtocol = "kafka"
	// UpstreamProtocolNATS is Planned (M4).
	UpstreamProtocolNATS UpstreamProtocol = "nats"
	// UpstreamProtocolMQTT is Planned (M4).
	UpstreamProtocolMQTT UpstreamProtocol = "mqtt"
	// UpstreamProtocolAI is Planned (M3).
	UpstreamProtocolAI UpstreamProtocol = "ai"
)

// UpstreamSpec is the spec of an Upstream. Endpoints are required unless
// discovery is set or protocol is ai.
// +ruralz:atMostOneOf=endpoints,discovery
type UpstreamSpec struct {
	// Protocol is the protocol the Upstream speaks.
	// +ruralz:required
	Protocol UpstreamProtocol `json:"protocol"`
	// Endpoints are static addresses.
	// +ruralz:list=map,key=address
	Endpoints []Endpoint `json:"endpoints,omitempty"`
	// Discovery finds Endpoints instead of listing them.
	Discovery *Discovery `json:"discovery,omitempty"`
	// LoadBalancing selects an Endpoint per attempt.
	LoadBalancing *LoadBalancing `json:"loadBalancing,omitempty"`
	// HealthCheck configures active and passive health checks.
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"`
	// Retries configures retries.
	Retries *Retries `json:"retries,omitempty"`
	// CircuitBreaker configures the circuit breaker.
	CircuitBreaker *CircuitBreaker `json:"circuitBreaker,omitempty"`
	// TLS configures TLS to the Endpoints.
	TLS *UpstreamTLS `json:"tls,omitempty"`
	// Timeout is the per-leg deadline.
	Timeout *Duration `json:"timeout,omitempty"`
	// Messaging applies to kafka, nats and mqtt only. Planned (M4).
	Messaging *Messaging `json:"messaging,omitempty"`
	// AI applies to protocol ai only; its models replace endpoints. Planned (M3).
	AI *UpstreamAI `json:"ai,omitempty"`
	// Policies attaches upstream-leg Policies in authored order.
	// +ruralz:list=orderedMap,key=name
	Policies []PolicyRef `json:"policies,omitempty"`
}

// Endpoint is one static address.
type Endpoint struct {
	// Address is host:port.
	// +ruralz:required
	Address string `json:"address"`
	// Weight is the relative load-balancing weight.
	// +ruralz:minimum=0
	Weight *int32 `json:"weight,omitempty"`
}

// DiscoveryType selects a service discovery mechanism.
type DiscoveryType string

// Discovery types.
const (
	// DiscoveryTypeDNS resolves DNS names. Planned (M1).
	DiscoveryTypeDNS DiscoveryType = "dns"
	// DiscoveryTypeKubernetes watches EndpointSlices. Planned (M2).
	DiscoveryTypeKubernetes DiscoveryType = "kubernetes"
)

// Discovery finds Endpoints.
type Discovery struct {
	// Type is dns or kubernetes.
	// +ruralz:required
	Type DiscoveryType `json:"type"`
	// Service is the service name.
	Service string `json:"service,omitempty"`
	// Namespace is the Kubernetes namespace.
	Namespace string `json:"namespace,omitempty"`
	// Port is a port name or number.
	Port *IntOrString `json:"port,omitempty"`
}

// LoadBalancingAlgorithm selects an Endpoint.
type LoadBalancingAlgorithm string

// Load-balancing algorithms.
const (
	// LoadBalancingRoundRobin cycles through Endpoints by weight.
	LoadBalancingRoundRobin LoadBalancingAlgorithm = "round-robin"
	// LoadBalancingLeastRequest picks the less loaded of two random candidates.
	LoadBalancingLeastRequest LoadBalancingAlgorithm = "least-request"
	// LoadBalancingRingHash hashes hashKey onto a ring.
	LoadBalancingRingHash LoadBalancingAlgorithm = "ring-hash"
	// LoadBalancingRandom picks a random Endpoint.
	LoadBalancingRandom LoadBalancingAlgorithm = "random"
)

// LoadBalancing selects an Endpoint per attempt.
type LoadBalancing struct {
	// Algorithm is round-robin, least-request, ring-hash or random.
	Algorithm *LoadBalancingAlgorithm `json:"algorithm,omitempty"`
	// HashKey is the ring-hash key; required when algorithm is ring-hash.
	// +ruralz:cel=request,source,route,consumer,auth,now:string
	HashKey string `json:"hashKey,omitempty"`
}

// HealthCheck configures health checks.
type HealthCheck struct {
	// Active probes Endpoints.
	Active *ActiveHealthCheck `json:"active,omitempty"`
	// Passive ejects Endpoints that fail requests.
	Passive *PassiveHealthCheck `json:"passive,omitempty"`
}

// ActiveHealthCheck probes Endpoints.
type ActiveHealthCheck struct {
	// Path is the probe path.
	Path string `json:"path,omitempty"`
	// Interval is the time between probes.
	Interval *Duration `json:"interval,omitempty"`
	// Timeout is the probe deadline.
	Timeout *Duration `json:"timeout,omitempty"`
	// HealthyThreshold is the consecutive successes that mark an Endpoint healthy.
	// +ruralz:minimum=1
	HealthyThreshold *int32 `json:"healthyThreshold,omitempty"`
	// UnhealthyThreshold is the consecutive failures that mark an Endpoint unhealthy.
	// +ruralz:minimum=1
	UnhealthyThreshold *int32 `json:"unhealthyThreshold,omitempty"`
}

// PassiveHealthCheck ejects Endpoints that fail requests.
type PassiveHealthCheck struct {
	// ConsecutiveErrors matching circuitBreaker.failureWhen eject an Endpoint.
	// +ruralz:minimum=1
	ConsecutiveErrors *int32 `json:"consecutiveErrors,omitempty"`
	// EjectionTime is multiplied by the Endpoint's ejection count.
	EjectionTime *Duration `json:"ejectionTime,omitempty"`
}

// Retries configures retries.
type Retries struct {
	// Attempts counts retries after the first attempt.
	// +ruralz:minimum=0
	Attempts *int32 `json:"attempts,omitempty"`
	// PerTryTimeout is the deadline of each attempt.
	PerTryTimeout *Duration `json:"perTryTimeout,omitempty"`
	// RetryOn decides whether an attempt is retried.
	// +ruralz:cel=request,response,error,attempt,upstream:bool
	RetryOn string `json:"retryOn,omitempty"`
}

// CircuitBreaker configures the circuit breaker and bulkhead.
type CircuitBreaker struct {
	// MaxConnections caps connections per Endpoint set.
	// +ruralz:minimum=1
	MaxConnections *int32 `json:"maxConnections,omitempty"`
	// MaxPendingRequests caps queued requests.
	// +ruralz:minimum=0
	MaxPendingRequests *int32 `json:"maxPendingRequests,omitempty"`
	// ConsecutiveFailures opens the breaker.
	// +ruralz:minimum=1
	ConsecutiveFailures *int32 `json:"consecutiveFailures,omitempty"`
	// OpenDuration is how long the breaker stays open.
	OpenDuration *Duration `json:"openDuration,omitempty"`
	// FailureWhen decides whether an attempt counts as a failure.
	// +ruralz:cel=request,response,error,upstream:bool
	FailureWhen string `json:"failureWhen,omitempty"`
}

// UpstreamTLS configures TLS to Endpoints.
type UpstreamTLS struct {
	// SNI is the server name sent in the TLS handshake.
	SNI string `json:"sni,omitempty"`
	// CACertificate verifies the Endpoints.
	// +ruralz:secret
	CACertificate *SecretValue `json:"caCertificate,omitempty"`
	// ClientCertificate is the client certificate for mTLS.
	// +ruralz:secret
	ClientCertificate *SecretValue `json:"clientCertificate,omitempty"`
	// ClientKey is the client private key for mTLS.
	// +ruralz:secret
	ClientKey *SecretValue `json:"clientKey,omitempty"`
}

// Messaging configures kafka, nats and mqtt Upstreams. Planned (M4).
type Messaging struct {
	// Topic is the topic or subject.
	Topic string `json:"topic,omitempty"`
	// Key computes the message key.
	// +ruralz:cel=request,source,route,consumer,auth,now:string
	Key string `json:"key,omitempty"`
}

// AISurface is the API surface an ai Upstream exposes.
type AISurface string

// AI surfaces.
const (
	// AISurfaceOpenAI is the OpenAI-compatible facade.
	AISurfaceOpenAI AISurface = "openai"
	// AISurfaceNative is native provider passthrough.
	AISurfaceNative AISurface = "native"
)

// UpstreamAI configures an ai Upstream. Planned (M3).
type UpstreamAI struct {
	// Surface is openai or native.
	Surface *AISurface `json:"surface,omitempty"`
	// Models are the AIModels this Upstream serves.
	// +ruralz:list=map,key=name
	Models []AIModelRef `json:"models,omitempty"`
}

// AIModelRef names an AIModel.
type AIModelRef struct {
	// Name is the metadata.name of an AIModel.
	// +ruralz:required
	// +ruralz:ref=AIModel
	Name string `json:"name"`
}
