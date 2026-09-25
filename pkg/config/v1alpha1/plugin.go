// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Phase is a Filter Chain Phase (foundation pack section 4).
type Phase string

// Filter Chain Phases, in order, plus the streaming hook onChunk.
const (
	// PhaseOnRequestHeaders runs after the Route is fixed by the header match.
	PhaseOnRequestHeaders Phase = "onRequestHeaders"
	// PhaseOnRequestBody runs on the buffered request body.
	PhaseOnRequestBody Phase = "onRequestBody"
	// PhaseOnRoute chooses among Upstream legs or AIModel candidates.
	PhaseOnRoute Phase = "onRoute"
	// PhaseOnUpstreamRequest runs once per upstream leg attempt.
	PhaseOnUpstreamRequest Phase = "onUpstreamRequest"
	// PhaseOnUpstreamResponseHeaders runs on upstream response headers.
	PhaseOnUpstreamResponseHeaders Phase = "onUpstreamResponseHeaders"
	// PhaseOnUpstreamResponseBody runs on the buffered upstream response body.
	PhaseOnUpstreamResponseBody Phase = "onUpstreamResponseBody"
	// PhaseOnResponse runs before the response is sent to the client.
	PhaseOnResponse Phase = "onResponse"
	// PhaseOnLog runs after the exchange; it is read-only.
	PhaseOnLog Phase = "onLog"
	// PhaseOnChunk runs per SSE event, WebSocket message or LLM token chunk.
	PhaseOnChunk Phase = "onChunk"
)

// Capability is a Plugin ABI v1 Capability; everything not granted is
// denied.
type Capability string

// Plugin ABI v1 Capabilities (docs/architecture/05-wasm-plugin-system.md).
const (
	// CapabilityCredentialsRead reads credentials; always flagged by audit.
	CapabilityCredentialsRead Capability = "credentials.read"
	// CapabilityRequestBodyRead reads the request body.
	CapabilityRequestBodyRead Capability = "request.body.read"
	// CapabilityRequestHeadersRead reads request headers.
	CapabilityRequestHeadersRead Capability = "request.headers.read"
	// CapabilityResponseBodyRead reads the response body.
	CapabilityResponseBodyRead Capability = "response.body.read"
	// CapabilityStreamRead reads stream chunks.
	CapabilityStreamRead Capability = "stream.read"
	// CapabilityResponseSend produces a response, which can deny traffic.
	CapabilityResponseSend Capability = "response.send"
	// CapabilityStateRead reads the State Store.
	CapabilityStateRead Capability = "state.read"
	// CapabilityStateWrite writes the State Store.
	CapabilityStateWrite Capability = "state.write"
	// CapabilityRequestHeadersWrite writes request headers.
	CapabilityRequestHeadersWrite Capability = "request.headers.write"
	// CapabilityRequestBodyWrite writes the request body.
	CapabilityRequestBodyWrite Capability = "request.body.write"
	// CapabilityConsumerRead reads the matched Consumer.
	CapabilityConsumerRead Capability = "consumer.read"
	// CapabilityResponseHeadersWrite writes response headers.
	CapabilityResponseHeadersWrite Capability = "response.headers.write"
	// CapabilityResponseBodyWrite writes the response body.
	CapabilityResponseBodyWrite Capability = "response.body.write"
	// CapabilityStreamWrite writes stream chunks.
	CapabilityStreamWrite Capability = "stream.write"
	// CapabilityLogWrite writes log records.
	CapabilityLogWrite Capability = "log.write"
	// CapabilityMetricsWrite writes metrics.
	CapabilityMetricsWrite Capability = "metrics.write"
	// CapabilityClockRead reads the clock.
	CapabilityClockRead Capability = "clock.read"
	// CapabilityRandomRead reads random bytes.
	CapabilityRandomRead Capability = "random.read"
	// CapabilityRequestMetadataRead reads request metadata.
	CapabilityRequestMetadataRead Capability = "request.metadata.read"
	// CapabilityResponseHeadersRead reads response headers.
	CapabilityResponseHeadersRead Capability = "response.headers.read"
	// CapabilityContextUse uses the per-request context.
	CapabilityContextUse Capability = "context.use"
	// CapabilityCryptoUse uses host cryptography.
	CapabilityCryptoUse Capability = "crypto.use"
)

// PluginABI names a Plugin ABI.
type PluginABI string

// Plugin ABIs.
const (
	// PluginABIV1 is Plugin ABI v1.
	PluginABIV1 PluginABI = "ruralz.plugin.v1"
)

// PluginSpec is the spec of a Plugin. Planned (M2).
type PluginSpec struct {
	// Image is the OCI reference; it MUST carry an @sha256 digest (RZ-CFG-021), and the tag is informational.
	// +ruralz:required
	// +ruralz:impact=plugin,security
	Image string `json:"image"`
	// ABI is the Plugin ABI the artifact implements.
	// +ruralz:required
	ABI PluginABI `json:"abi"`
	// Phases are the Filter Chain Phases the Plugin implements.
	// +ruralz:required
	// +ruralz:minItems=1
	// +ruralz:list=set
	Phases []Phase `json:"phases"`
	// Capabilities are the granted Capabilities; nothing else is granted.
	// +ruralz:list=set
	// +ruralz:impact=plugin,security
	Capabilities []Capability `json:"capabilities,omitempty"`
	// Limits are the per-Plugin resource limits.
	Limits *PluginLimits `json:"limits,omitempty"`
	// ConfigSchema is the JSON Schema for the config of plugin Policies using this Plugin; it may carry x-ruralz-validations.
	ConfigSchema map[string]any `json:"configSchema,omitempty"`
}

// PluginLimits are the per-Plugin resource limits.
type PluginLimits struct {
	// MemoryBytes is the linear memory limit per instance.
	// +ruralz:default=16Mi
	MemoryBytes *ByteSize `json:"memoryBytes,omitempty"`
	// Timeout is the wall-clock limit per call.
	// +ruralz:default=5ms
	Timeout *Duration `json:"timeout,omitempty"`
}
