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
	// +ruralz:impact=traffic
	LoadBalancing *LoadBalancing `json:"loadBalancing,omitempty"`
	// HealthCheck configures active and passive health checks; passive ejection runs even without it.
	// +ruralz:impact=traffic
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"`
	// Retries configures retries.
	// +ruralz:impact=traffic
	Retries *Retries `json:"retries,omitempty"`
	// CircuitBreaker configures the circuit breaker and the in-flight ceiling.
	// +ruralz:impact=traffic
	CircuitBreaker *CircuitBreaker `json:"circuitBreaker,omitempty"`
	// TLS configures TLS to the Endpoints; without it the Upstream is a cleartext hop.
	// +ruralz:impact=security
	TLS *UpstreamTLS `json:"tls,omitempty"`
	// Timeout is the per-leg deadline; default the Route's timeout.
	// +ruralz:impact=traffic
	Timeout *Duration `json:"timeout,omitempty"`
	// Messaging applies to kafka, nats and mqtt only. Planned (M4).
	Messaging *Messaging `json:"messaging,omitempty"`
	// AI applies to protocol ai only; its models replace endpoints. Planned (M3).
	// +ruralz:impact=ai
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
	// +ruralz:default=1
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
	// +ruralz:default=least-request
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

// ActiveHealthCheck probes Endpoints; its presence turns probing on. A probe
// is GET path, and a status of 200 to 399 within timeout succeeds.
type ActiveHealthCheck struct {
	// Path is the probe path.
	// +ruralz:default=/
	Path *string `json:"path,omitempty"`
	// Interval is the time between probes.
	// +ruralz:default=10s
	Interval *Duration `json:"interval,omitempty"`
	// Timeout is the probe deadline.
	// +ruralz:default=2s
	Timeout *Duration `json:"timeout,omitempty"`
	// HealthyThreshold is the consecutive successes that mark an Endpoint healthy.
	// +ruralz:default=2
	// +ruralz:minimum=1
	HealthyThreshold *int32 `json:"healthyThreshold,omitempty"`
	// UnhealthyThreshold is the consecutive failures that mark an Endpoint unhealthy.
	// +ruralz:default=3
	// +ruralz:minimum=1
	UnhealthyThreshold *int32 `json:"unhealthyThreshold,omitempty"`
}

// PassiveHealthCheck ejects Endpoints that fail requests.
type PassiveHealthCheck struct {
	// ConsecutiveErrors matching circuitBreaker.failureWhen eject an Endpoint.
	// +ruralz:default=5
	// +ruralz:minimum=1
	ConsecutiveErrors *int32 `json:"consecutiveErrors,omitempty"`
	// EjectionTime is multiplied by the Endpoint's ejection count.
	// +ruralz:default=30s
	EjectionTime *Duration `json:"ejectionTime,omitempty"`
}

// Retries configures retries.
type Retries struct {
	// Attempts counts retries after the first attempt.
	// +ruralz:default=1
	// +ruralz:minimum=0
	Attempts *int32 `json:"attempts,omitempty"`
	// PerTryTimeout is the deadline of each attempt; default the leg time left divided by the retries left plus one.
	PerTryTimeout *Duration `json:"perTryTimeout,omitempty"`
	// RetryOn decides whether an attempt is retried; default a method-dependent rule over connect and reset errors and status 503.
	// +ruralz:cel=request,response,error,attempt,upstream:bool
	RetryOn string `json:"retryOn,omitempty"`
}

// CircuitBreaker configures the circuit breaker and the bulkhead of an
// Upstream, each kept per Upstream per Node.
type CircuitBreaker struct {
	// MaxConnections caps in-flight attempts per Upstream per Node, HTTP/2 streams included; an attempt takes a slot before it is sent and holds it until its response body closes.
	// +ruralz:default=1024
	// +ruralz:minimum=1
	MaxConnections *int32 `json:"maxConnections,omitempty"`
	// MaxPendingRequests caps attempts waiting for an in-flight slot; when the queue is full an attempt gets 503 RZ-UP-006 at once.
	// +ruralz:default=256
	// +ruralz:minimum=0
	MaxPendingRequests *int32 `json:"maxPendingRequests,omitempty"`
	// ConsecutiveFailures is the run of failed legs that opens the breaker, together with failureRatio of at least minimumLegs legs in a rolling 10 s window.
	// +ruralz:default=5
	// +ruralz:minimum=1
	ConsecutiveFailures *int32 `json:"consecutiveFailures,omitempty"`
	// MinimumLegs is the number of legs a rolling 10 s window needs before the breaker may open.
	// +ruralz:default=20
	// +ruralz:minimum=1
	MinimumLegs *int32 `json:"minimumLegs,omitempty"`
	// FailureRatio is the share of failed legs in the window, 0 to 1, that the breaker also needs to open.
	// +ruralz:default=0.5
	// +ruralz:minimum=0
	// +ruralz:maximum=1
	FailureRatio *float64 `json:"failureRatio,omitempty"`
	// OpenDuration is how long the breaker stays open, jittered by 20% either way, before it lets one probe leg through.
	// +ruralz:default=30s
	OpenDuration *Duration `json:"openDuration,omitempty"`
	// HalfOpenSuccesses is the run of successful probe legs that closes a half-open breaker; a failed probe reopens it.
	// +ruralz:default=3
	// +ruralz:minimum=1
	HalfOpenSuccesses *int32 `json:"halfOpenSuccesses,omitempty"`
	// FailureWhen decides whether an attempt or leg counts as a failure; default any error, or status 502, 503 or 504.
	// +ruralz:cel=request,response,error,upstream:bool
	FailureWhen string `json:"failureWhen,omitempty"`
}

// UpstreamTLS configures the client side of a TLS connection, to an
// Upstream's Endpoints or to the OTLP collector: TLS 1.2 or newer, the
// server certificate always verified. Set clientCertificate and clientKey
// together, or neither.
type UpstreamTLS struct {
	// SNI is the server name sent in the TLS handshake and verified in the server certificate.
	SNI string `json:"sni,omitempty"`
	// CACertificate is a PEM bundle that verifies the server instead of the system roots.
	// +ruralz:secret
	// +ruralz:impact=security
	CACertificate *SecretValue `json:"caCertificate,omitempty"`
	// ClientCertificate is the PEM client certificate chain for mTLS.
	// +ruralz:secret
	// +ruralz:impact=security
	ClientCertificate *SecretValue `json:"clientCertificate,omitempty"`
	// ClientKey is the PEM client private key for mTLS.
	// +ruralz:secret
	// +ruralz:impact=security
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
