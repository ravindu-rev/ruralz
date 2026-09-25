// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// GatewaySpec is the spec of a Gateway.
type GatewaySpec struct {
	// Listeners are the client listeners; at least one.
	// +ruralz:required
	// +ruralz:minItems=1
	// +ruralz:list=map,key=name
	Listeners []Listener `json:"listeners"`
	// TrustedProxies are CIDRs whose forwarding headers set source.ip; default empty. Planned (M1).
	// +ruralz:list=set
	TrustedProxies []string `json:"trustedProxies,omitempty"`
	// Admin configures the admin listener.
	Admin *Admin `json:"admin,omitempty"`
	// Telemetry configures OTLP export, trace sampling and the access log.
	Telemetry *Telemetry `json:"telemetry,omitempty"`
	// Limits are Node-wide limits.
	Limits *Limits `json:"limits,omitempty"`
	// StateStore is the State Store connection. Without it a Node reads RURALZ_STATE_STORE_URL, else uses memory.
	StateStore *StateStore `json:"stateStore,omitempty"`
	// Policies attaches Gateway-scoped Policies in authored order.
	// +ruralz:list=orderedMap,key=name
	Policies []PolicyRef `json:"policies,omitempty"`
}

// ListenerProtocol is the protocol a Gateway listener terminates.
type ListenerProtocol string

// Listener protocols.
const (
	// ListenerProtocolHTTP serves HTTP/1.1 and h2c.
	ListenerProtocolHTTP ListenerProtocol = "http"
	// ListenerProtocolHTTPS serves HTTP/1.1 and HTTP/2 over TLS.
	ListenerProtocolHTTPS ListenerProtocol = "https"
)

// Listener is one client listener.
type Listener struct {
	// Name identifies the listener; Routes select listeners by name.
	// +ruralz:required
	Name string `json:"name"`
	// Protocol is http or https.
	// +ruralz:required
	Protocol ListenerProtocol `json:"protocol"`
	// Port is the TCP port.
	// +ruralz:required
	// +ruralz:minimum=1
	// +ruralz:maximum=65535
	Port int32 `json:"port"`
	// HTTP3 adds HTTP/3 on UDP on the same port; https only. Planned (M3); off in FIPS builds.
	HTTP3 *bool `json:"http3,omitempty"`
	// ProxyProtocol, when true, requires PROXY protocol v2 on this listener. Planned (M1).
	// +ruralz:default=false
	ProxyProtocol *bool `json:"proxyProtocol,omitempty"`
	// Hostnames limits the listener to these host names.
	// +ruralz:list=set
	Hostnames []string `json:"hostnames,omitempty"`
	// TLS configures TLS termination; required for https.
	TLS *ListenerTLS `json:"tls,omitempty"`
}

// TLSVersion is a minimum TLS protocol version.
type TLSVersion string

// TLS versions.
const (
	// TLSVersion12 is TLS 1.2.
	TLSVersion12 TLSVersion = "1.2"
	// TLSVersion13 is TLS 1.3.
	TLSVersion13 TLSVersion = "1.3"
)

// ListenerTLS configures TLS on a listener.
type ListenerTLS struct {
	// MinVersion is the lowest accepted TLS version.
	// +ruralz:default="1.3"
	MinVersion *TLSVersion `json:"minVersion,omitempty"`
	// Certificates are the served certificates; required for https.
	// +ruralz:list=map,key=name
	Certificates []Certificate `json:"certificates,omitempty"`
}

// Certificate is one served certificate and its private key.
type Certificate struct {
	// Name identifies the certificate.
	// +ruralz:required
	Name string `json:"name"`
	// Certificate is the PEM certificate chain.
	// +ruralz:required
	// +ruralz:secret
	Certificate SecretValue `json:"certificate"`
	// PrivateKey is the PEM private key.
	// +ruralz:required
	// +ruralz:secret
	PrivateKey SecretValue `json:"privateKey"`
}

// Admin configures the admin listener.
type Admin struct {
	// Port is the admin TCP port.
	// +ruralz:default=9901
	// +ruralz:minimum=1
	// +ruralz:maximum=65535
	Port *int32 `json:"port,omitempty"`
}

// Telemetry configures OpenTelemetry export, trace sampling and the access
// log.
type Telemetry struct {
	// OTLP configures OTLP export.
	OTLP *OTLP `json:"otlp,omitempty"`
	// TraceSampling is the head sampling ratio, 0 to 1.
	// +ruralz:minimum=0
	// +ruralz:maximum=1
	TraceSampling *float64 `json:"traceSampling,omitempty"`
	// AccessLog configures the access log.
	AccessLog *AccessLog `json:"accessLog,omitempty"`
}

// OTLP configures OTLP export.
type OTLP struct {
	// Endpoint is the OTLP collector URL.
	Endpoint string `json:"endpoint,omitempty"`
}

// AccessLog configures the access log.
type AccessLog struct {
	// When writes an entry only when the expression is true.
	// +ruralz:cel=request,source,route,consumer,auth,now,response,upstream,duration:bool
	When string `json:"when,omitempty"`
}

// Limits are Node-wide limits. Limits count bytes after content decoding.
type Limits struct {
	// MaxRequestBodyBytes caps a request body; above it the request gets 413 RZ-RT-003.
	MaxRequestBodyBytes *ByteSize `json:"maxRequestBodyBytes,omitempty"`
	// MaxRequestHeaderBytes caps a request header block; above it the request gets 431 RZ-RT-002.
	MaxRequestHeaderBytes *ByteSize `json:"maxRequestHeaderBytes,omitempty"`
	// MaxResponseBodyBytes caps one buffered response, and a Route's step bodies together.
	// +ruralz:default=10Mi
	MaxResponseBodyBytes *ByteSize `json:"maxResponseBodyBytes,omitempty"`
	// MaxCompositionSteps caps composition.steps per Route.
	// +ruralz:default=16
	// +ruralz:minimum=1
	MaxCompositionSteps *int32 `json:"maxCompositionSteps,omitempty"`
	// MaxBufferedBytes is the Node-wide budget for all buffered bodies, decoded values included.
	// +ruralz:default=512Mi
	MaxBufferedBytes *ByteSize `json:"maxBufferedBytes,omitempty"`
	// MaxPluginMemoryBytes caps aggregate Plugin memory per Node.
	// +ruralz:default=2Gi
	MaxPluginMemoryBytes *ByteSize `json:"maxPluginMemoryBytes,omitempty"`
}

// StateStoreDriver selects the State Store implementation.
type StateStoreDriver string

// State Store drivers.
const (
	// StateStoreDriverMemory keeps state in one Node; Rate Limits multiply by the Node count.
	StateStoreDriverMemory StateStoreDriver = "memory"
	// StateStoreDriverRedis uses Redis, Valkey or Dragonfly over RESP3.
	StateStoreDriverRedis StateStoreDriver = "redis"
)

// StateStoreTopology is the Redis deployment shape.
type StateStoreTopology string

// State Store topologies.
const (
	// StateStoreTopologyStandalone is one endpoint that follows the primary through failover.
	StateStoreTopologyStandalone StateStoreTopology = "standalone"
	// StateStoreTopologyCluster is a Redis Cluster seed list.
	StateStoreTopologyCluster StateStoreTopology = "cluster"
)

// StateStore is the State Store connection.
type StateStore struct {
	// Driver is memory or redis.
	// +ruralz:default=memory
	Driver *StateStoreDriver `json:"driver,omitempty"`
	// Topology applies to redis only. Planned (M1).
	// +ruralz:default=standalone
	Topology *StateStoreTopology `json:"topology,omitempty"`
	// URL is the rueidis connection URL; it may carry credentials.
	// +ruralz:secret
	URL *SecretValue `json:"url,omitempty"`
	// Timeout is the per-operation default for Policies.
	Timeout *Duration `json:"timeout,omitempty"`
}
