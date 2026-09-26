// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package catalog is the Ruralz telemetry name catalog of
// docs/architecture/10-observability.md for M1: metric families with type,
// histogram bounds, unit and labels; degraded reasons; cleartext hops; span
// names and attribute keys; process-log keys. It imports only the standard
// library so repocheck can import it: a metric or span name literal outside
// this package is a finding.
package catalog

import "slices"

// Kind is an instrument type.
type Kind uint8

// Instrument types.
const (
	Counter Kind = iota + 1
	Gauge
	Histogram
)

// Bounds names one of the five histogram bound sets.
type Bounds uint8

// Histogram bound sets (13 bounds each).
const (
	BoundsNone Bounds = iota
	BoundsFast
	BoundsRequest
	BoundsControl
	BoundsBytes
	BoundsRatio
)

// Seconds returns the bounds in base units.
func (b Bounds) Seconds() []float64 {
	switch b {
	case BoundsFast:
		return []float64{0.00001, 0.000025, 0.00005, 0.0001, 0.00015, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.1}
	case BoundsRequest:
		return []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 60}
	case BoundsControl:
		return []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 3600}
	case BoundsBytes:
		return []float64{64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864, 268435456, 1073741824}
	case BoundsRatio:
		return []float64{0.5, 0.75, 0.9, 0.95, 0.98, 0.99, 1, 1.01, 1.02, 1.05, 1.1, 1.25, 1.5}
	default:
		return nil
	}
}

// Class is how a family's label sets are admitted.
type Class uint8

// Admission classes.
const (
	// ClassListener families (listener or enumeration-only) never fold.
	ClassListener Class = iota + 1
	// ClassRoute families fold per Route.
	ClassRoute
	// ClassUpstream families fold per Upstream.
	ClassUpstream
	// ClassPolicy families fold per Policy.
	ClassPolicy
)

// Label is one label of a family.
type Label struct {
	// Name is the label name.
	Name string
	// Values are the enumeration values; nil for resource names and codes.
	Values []string
	// Code is true when values are registered RZ codes.
	Code bool
}

// Family is one metric family.
type Family struct {
	// Name is the exact name, identical in OTLP and /metrics.
	Name string
	// Kind is the instrument type.
	Kind Kind
	// Bounds is the histogram set.
	Bounds Bounds
	// Unit is UCUM.
	Unit string
	// Labels in order.
	Labels []Label
	// Class is the admission class.
	Class Class
	// Striped marks hot label sets.
	Striped bool
}

// Metric family names (M1). Code uses these constants, never literals.
const (
	HTTPRequestsTotal                   = "ruralz_http_requests_total"
	HTTPListenerRequestsTotal           = "ruralz_http_listener_requests_total"
	HTTPNodeResponsesTotal              = "ruralz_http_node_responses_total"
	HTTPRequestDurationSeconds          = "ruralz_http_request_duration_seconds"
	HTTPGatewayDurationSeconds          = "ruralz_http_gateway_duration_seconds"
	HTTPGatewayDurationSkippedTotal     = "ruralz_http_gateway_duration_skipped_total"
	HTTPRequestBodyBytes                = "ruralz_http_request_body_bytes"
	HTTPResponseBodyBytes               = "ruralz_http_response_body_bytes"
	HTTPActiveRequests                  = "ruralz_http_active_requests"
	ListenerOpenConnections             = "ruralz_listener_open_connections"
	ListenerConnectionsTotal            = "ruralz_listener_connections_total"
	ListenerTLSHandshakeDurationSeconds = "ruralz_listener_tls_handshake_duration_seconds"
	FilterDurationSeconds               = "ruralz_filter_duration_seconds"
	FilterShortCircuitsTotal            = "ruralz_filter_short_circuits_total"
	FilterFailuresTotal                 = "ruralz_filter_failures_total"
	AuthDecisionsTotal                  = "ruralz_auth_decisions_total"
	AuthJWKSAgeSeconds                  = "ruralz_auth_jwks_age_seconds"
	AuthUpstreamTokenAgeSeconds         = "ruralz_auth_upstream_token_age_seconds" //nolint:gosec // G101: a metric name, not a credential.
	AuthUpstreamRefreshFailuresTotal    = "ruralz_auth_upstream_refresh_failures_total"
	SecurityCleartextHops               = "ruralz_security_cleartext_hops"
	RateLimitDecisionsTotal             = "ruralz_ratelimit_decisions_total"
	RateLimitBucketEvictionsTotal       = "ruralz_ratelimit_bucket_evictions_total"
	QuotaDecisionsTotal                 = "ruralz_quota_decisions_total"
	CacheRequestsTotal                  = "ruralz_cache_requests_total"
	CacheStoreSkippedTotal              = "ruralz_cache_store_skipped_total"
	UpstreamAttemptsTotal               = "ruralz_upstream_attempts_total"
	UpstreamAttemptDurationSeconds      = "ruralz_upstream_attempt_duration_seconds"
	UpstreamRetriesTotal                = "ruralz_upstream_retries_total"
	UpstreamRetryBudgetExhaustedTotal   = "ruralz_upstream_retry_budget_exhausted_total"
	UpstreamBreakerStateInfo            = "ruralz_upstream_breaker_state_info"
	UpstreamEjectionsTotal              = "ruralz_upstream_ejections_total"
	UpstreamHealthyEndpoints            = "ruralz_upstream_healthy_endpoints"
	UpstreamProbesSkippedTotal          = "ruralz_upstream_probes_skipped_total"
	UpstreamDegradedInfo                = "ruralz_upstream_degraded_info"
	UpstreamCELErrorsTotal              = "ruralz_upstream_cel_errors_total"
	UpstreamPoolConnections             = "ruralz_upstream_pool_connections"
	StateCallDurationSeconds            = "ruralz_state_call_duration_seconds"
	StateCallsTotal                     = "ruralz_state_calls_total"
	StateOpsTotal                       = "ruralz_state_ops_total"
	StateWritesDroppedTotal             = "ruralz_state_writes_dropped_total"
	StateWriteQueueItems                = "ruralz_state_write_queue_items"
	ConfigRevisionInfo                  = "ruralz_config_revision_info"
	ConfigActivationsTotal              = "ruralz_config_activations_total"
	ConfigActivationDurationSeconds     = "ruralz_config_activation_duration_seconds"
	ConfigRetiredSnapshots              = "ruralz_config_retired_snapshots"
	SnapshotRetirementEndedTotal        = "ruralz_snapshot_retirement_ended_total"
	ConfigSecretRotationFailuresTotal   = "ruralz_config_secret_rotation_failures_total" //nolint:gosec // G101: a metric name, not a credential.
	NodeDegradedInfo                    = "ruralz_node_degraded_info"
	NodeBufferedBytes                   = "ruralz_node_buffered_bytes"
	RuntimeGoroutines                   = "ruralz_runtime_goroutines"
	RuntimeHeapBytes                    = "ruralz_runtime_heap_bytes"
	RuntimeGCCyclesTotal                = "ruralz_runtime_gc_cycles_total"
	TelemetrySpansTotal                 = "ruralz_telemetry_spans_total"
	TelemetrySpansDroppedTotal          = "ruralz_telemetry_spans_dropped_total"
	TelemetryTracesUnsampledTotal       = "ruralz_telemetry_traces_unsampled_total"
	TelemetryLogsTotal                  = "ruralz_telemetry_logs_total"
	TelemetryLogsDroppedTotal           = "ruralz_telemetry_logs_dropped_total"
	TelemetryFoldedLabelSets            = "ruralz_telemetry_folded_label_sets"
	TelemetrySeries                     = "ruralz_telemetry_series"
	TapEventsDroppedTotal               = "ruralz_tap_events_dropped_total"
)

// Enumerations used as label values.
func statusClasses() []string { return []string{"1xx", "2xx", "3xx", "4xx", "5xx"} }

// Ops returns the State Store op values (M1 subset first).
func Ops() []string {
	return []string{"gcra", "quota", "cache_get", "script_multi", "pipeline", "cache_set", "cache_invalidate", "refund"}
}

// WriteKinds returns the dropped-write kinds of M1.
func WriteKinds() []string { return []string{"refund", "cache_set", "cache_invalidate"} }

// Families returns the M1 metric families (docs/architecture/10-observability.md
// "Ruralz Gateway metrics"; spec 09 requirement 43).
func Families() []Family {
	route := Label{Name: "route"}
	up := Label{Name: "upstream"}
	pol := Label{Name: "policy"}
	lis := Label{Name: "listener"}
	proto := Label{Name: "protocol", Values: []string{"http1", "http2"}}
	sc := Label{Name: "status_class", Values: statusClasses()}
	ph := Label{Name: "phase", Values: []string{
		"onRequestHeaders", "onRequestBody", "onRoute", "onUpstreamRequest",
		"onUpstreamResponseHeaders", "onUpstreamResponseBody", "onResponse", "onLog", "onChunk",
	}}
	code := Label{Name: "code", Code: true}
	return []Family{
		{HTTPRequestsTotal, Counter, BoundsNone, "{requests}", []Label{route, sc}, ClassRoute, true},
		{HTTPListenerRequestsTotal, Counter, BoundsNone, "{requests}", []Label{lis, proto, sc, {Name: "origin", Values: []string{"upstream", "node", "dependency"}}}, ClassListener, true},
		{HTTPNodeResponsesTotal, Counter, BoundsNone, "{responses}", []Label{code}, ClassListener, false},
		{HTTPRequestDurationSeconds, Histogram, BoundsRequest, "s", []Label{route}, ClassRoute, true},
		{HTTPGatewayDurationSeconds, Histogram, BoundsFast, "s", []Label{lis}, ClassListener, true},
		{HTTPGatewayDurationSkippedTotal, Counter, BoundsNone, "{requests}", []Label{{Name: "reason", Values: []string{"clock_anomaly"}}}, ClassListener, false},
		{HTTPRequestBodyBytes, Histogram, BoundsBytes, "By", []Label{lis}, ClassListener, true},
		{HTTPResponseBodyBytes, Histogram, BoundsBytes, "By", []Label{lis}, ClassListener, true},
		{HTTPActiveRequests, Gauge, BoundsNone, "{requests}", []Label{lis}, ClassListener, true},
		{ListenerOpenConnections, Gauge, BoundsNone, "{connections}", []Label{lis, proto}, ClassListener, false},
		{ListenerConnectionsTotal, Counter, BoundsNone, "{connections}", []Label{lis, proto, {Name: "result", Values: []string{"accepted", "tls_failure", "refused"}}}, ClassListener, false},
		{ListenerTLSHandshakeDurationSeconds, Histogram, BoundsRequest, "s", []Label{lis}, ClassListener, false},
		{FilterDurationSeconds, Histogram, BoundsFast, "s", []Label{pol, ph}, ClassPolicy, true},
		{FilterShortCircuitsTotal, Counter, BoundsNone, "{responses}", []Label{pol, ph, sc}, ClassPolicy, true},
		{FilterFailuresTotal, Counter, BoundsNone, "{failures}", []Label{pol, ph, {Name: "mode", Values: []string{"open", "closed"}}}, ClassPolicy, false},
		{AuthDecisionsTotal, Counter, BoundsNone, "{decisions}", []Label{pol, {Name: "result", Values: []string{"allow", "deny"}}, code}, ClassPolicy, true},
		{AuthJWKSAgeSeconds, Gauge, BoundsNone, "s", []Label{pol}, ClassPolicy, false},
		{AuthUpstreamTokenAgeSeconds, Gauge, BoundsNone, "s", []Label{pol}, ClassPolicy, false},
		{AuthUpstreamRefreshFailuresTotal, Counter, BoundsNone, "{failures}", []Label{pol}, ClassPolicy, false},
		{SecurityCleartextHops, Gauge, BoundsNone, "{hops}", []Label{{Name: "hop", Values: HopNames()}}, ClassListener, false},
		{RateLimitDecisionsTotal, Counter, BoundsNone, "{decisions}", []Label{pol, {Name: "result", Values: []string{"allow", "deny_local", "deny_global", "fail_open"}}}, ClassPolicy, true},
		{RateLimitBucketEvictionsTotal, Counter, BoundsNone, "{entries}", []Label{pol}, ClassPolicy, false},
		{QuotaDecisionsTotal, Counter, BoundsNone, "{decisions}", []Label{pol, {Name: "result", Values: []string{"allow", "deny", "no_quota", "fail_open"}}}, ClassPolicy, true},
		{CacheRequestsTotal, Counter, BoundsNone, "{requests}", []Label{route, {Name: "result", Values: []string{"hit", "miss", "bypass", "stale", "stale_error"}}}, ClassRoute, false},
		{CacheStoreSkippedTotal, Counter, BoundsNone, "{stores}", []Label{pol, {Name: "reason", Values: []string{"size_limit", "buffer_budget", "memory"}}}, ClassPolicy, false},
		{UpstreamAttemptsTotal, Counter, BoundsNone, "{attempts}", []Label{up, sc, {Name: "error", Values: []string{"none", "connect", "timeout", "reset", "tls"}}}, ClassUpstream, true},
		{UpstreamAttemptDurationSeconds, Histogram, BoundsRequest, "s", []Label{up}, ClassUpstream, true},
		{UpstreamRetriesTotal, Counter, BoundsNone, "{retries}", []Label{up}, ClassUpstream, false},
		{UpstreamRetryBudgetExhaustedTotal, Counter, BoundsNone, "{retries}", []Label{up}, ClassUpstream, false},
		{UpstreamBreakerStateInfo, Gauge, BoundsNone, "1", []Label{up, {Name: "state", Values: []string{"closed", "open", "half_open"}}}, ClassUpstream, false},
		{UpstreamEjectionsTotal, Counter, BoundsNone, "{ejections}", []Label{up, {Name: "reason", Values: []string{"passive", "active"}}}, ClassUpstream, false},
		{UpstreamHealthyEndpoints, Gauge, BoundsNone, "{endpoints}", []Label{up}, ClassUpstream, false},
		{UpstreamProbesSkippedTotal, Counter, BoundsNone, "{probes}", []Label{up}, ClassUpstream, false},
		{UpstreamDegradedInfo, Gauge, BoundsNone, "1", []Label{up, {Name: "reason", Values: []string{"panic", "discovery_stale", "balancer_budget"}}}, ClassUpstream, false},
		{UpstreamCELErrorsTotal, Counter, BoundsNone, "{errors}", []Label{up, {Name: "field", Values: []string{"hashKey", "retryOn", "failureWhen"}}}, ClassUpstream, false},
		{UpstreamPoolConnections, Gauge, BoundsNone, "{connections}", []Label{up, {Name: "state", Values: []string{"idle", "active"}}}, ClassUpstream, false},
		{StateCallDurationSeconds, Histogram, BoundsFast, "s", []Label{{Name: "op", Values: Ops()}}, ClassListener, true},
		{StateCallsTotal, Counter, BoundsNone, "{calls}", []Label{{Name: "op", Values: Ops()}, {Name: "result", Values: []string{"ok", "error", "timeout", "skipped"}}}, ClassListener, true},
		{StateOpsTotal, Counter, BoundsNone, "{operations}", []Label{{Name: "kind", Values: Ops()}}, ClassListener, true},
		{StateWritesDroppedTotal, Counter, BoundsNone, "{writes}", []Label{{Name: "kind", Values: WriteKinds()}}, ClassListener, false},
		{StateWriteQueueItems, Gauge, BoundsNone, "{items}", nil, ClassListener, false},
		{ConfigRevisionInfo, Gauge, BoundsNone, "1", []Label{{Name: "revision"}, {Name: "role", Values: []string{"active", "lkg"}}}, ClassListener, false},
		{ConfigActivationsTotal, Counter, BoundsNone, "{activations}", []Label{{Name: "result", Values: []string{"activated", "rejected"}}, code}, ClassListener, false},
		{ConfigActivationDurationSeconds, Histogram, BoundsControl, "s", []Label{{Name: "stage", Values: []string{"verify", "plugin_compile", "compile", "swap", "total"}}, {Name: "size_class", Values: []string{"le1000", "le10000", "gt10000"}}}, ClassListener, false},
		{ConfigRetiredSnapshots, Gauge, BoundsNone, "{snapshots}", nil, ClassListener, false},
		{SnapshotRetirementEndedTotal, Counter, BoundsNone, "{requests}", nil, ClassListener, false},
		{ConfigSecretRotationFailuresTotal, Counter, BoundsNone, "{failures}", []Label{{Name: "provider", Values: []string{"env", "file", "kubernetes", "vault"}}}, ClassListener, false},
		{NodeDegradedInfo, Gauge, BoundsNone, "1", []Label{{Name: "reason", Values: ReasonNames()}}, ClassListener, false},
		{NodeBufferedBytes, Gauge, BoundsNone, "By", nil, ClassListener, false},
		{RuntimeGoroutines, Gauge, BoundsNone, "{goroutines}", nil, ClassListener, false},
		{RuntimeHeapBytes, Gauge, BoundsNone, "By", nil, ClassListener, false},
		{RuntimeGCCyclesTotal, Counter, BoundsNone, "{cycles}", nil, ClassListener, false},
		{TelemetrySpansTotal, Counter, BoundsNone, "{spans}", nil, ClassListener, false},
		{TelemetrySpansDroppedTotal, Counter, BoundsNone, "{spans}", []Label{{Name: "reason", Values: []string{"queue_full", "export_error"}}}, ClassListener, false},
		{TelemetryTracesUnsampledTotal, Counter, BoundsNone, "{traces}", []Label{{Name: "reason", Values: []string{"rate_cap_root", "rate_cap_parent"}}}, ClassListener, false},
		{TelemetryLogsTotal, Counter, BoundsNone, "{records}", []Label{{Name: "stream", Values: []string{"access", "process"}}}, ClassListener, false},
		{TelemetryLogsDroppedTotal, Counter, BoundsNone, "{records}", []Label{{Name: "stream", Values: []string{"access", "process"}}, {Name: "reason", Values: []string{"queue_full", "export_error"}}}, ClassListener, false},
		{TelemetryFoldedLabelSets, Gauge, BoundsNone, "{label_sets}", []Label{{Name: "instrument"}}, ClassListener, false},
		{TelemetrySeries, Gauge, BoundsNone, "{series}", []Label{{Name: "state", Values: []string{"live", "retiring"}}}, ClassListener, false},
		{TapEventsDroppedTotal, Counter, BoundsNone, "{events}", nil, ClassListener, false},
	}
}

// Lookup returns the family named name.
func Lookup(name string) (Family, bool) {
	fs := Families()
	i := slices.IndexFunc(fs, func(f Family) bool { return f.Name == name })
	if i < 0 {
		return Family{}, false
	}
	return fs[i], true
}

// Reason is a ruralz_node_degraded_info reason.
type Reason uint8

// M1 degraded reasons, all exported at 0 from process start.
const (
	ReasonLKGBoot Reason = iota + 1
	ReasonLKGWriteFailed
	ReasonStateStoreMemoryFallback
	ReasonStateStoreUnauthenticated
	ReasonStateStoreBreakerOpen
	ReasonStateStoreEvictionPolicy
	ReasonUpstreamPanic
	ReasonDiscoveryStale
	ReasonBalancerBudget
	ReasonProbesSkipped
	ReasonHeaderLimitCapped
	ReasonSnapshotEndingOverdue
	ReasonSecretRotationFailed
	ReasonJWKSStale
	ReasonCleartextHop
	ReasonTelemetryExportFailing
	ReasonCRLStale
	NumReasons
)

// String returns the label value.
func (r Reason) String() string {
	if r == 0 || r >= NumReasons {
		return ""
	}
	return ReasonNames()[r-1]
}

// ReasonNames returns the label values in Reason order.
func ReasonNames() []string {
	return []string{
		"lkg_boot", "lkg_write_failed", "state_store_memory_fallback", "state_store_unauthenticated",
		"state_store_breaker_open", "state_store_eviction_policy", "upstream_panic", "discovery_stale",
		"balancer_budget", "probes_skipped", "header_limit_capped", "snapshot_ending_overdue",
		"secret_rotation_failed", "jwks_stale", "cleartext_hop", "telemetry_export_failing", "crl_stale",
	}
}

// Hop is a cleartext hop kind.
type Hop uint8

// Hops.
const (
	HopClient Hop = iota
	HopUpstream
	HopStateStore
	HopTelemetry
	HopAdmin
	NumHops
)

// HopNames returns the label values in Hop order.
func HopNames() []string { return []string{"client", "upstream", "state_store", "telemetry", "admin"} }

// String returns the label value.
func (h Hop) String() string {
	if h >= NumHops {
		return ""
	}
	return HopNames()[h]
}

// Span names and prefixes (foundation pack section 2).
const (
	SpanRouteMatch     = "ruralz.route.match"
	SpanFilterPrefix   = "ruralz.filter."
	SpanUpstreamPrefix = "ruralz.upstream."
)

// FilterSpanName returns "ruralz.filter.<policy>"; call it at snapshot
// compile time only.
func FilterSpanName(policy string) string { return SpanFilterPrefix + policy }

// UpstreamSpanName returns "ruralz.upstream.<upstream>"; compile time only.
func UpstreamSpanName(upstream string) string { return SpanUpstreamPrefix + upstream }

// Span attribute keys.
const (
	AttrRoute         = "ruralz.route"
	AttrListener      = "ruralz.listener"
	AttrRevision      = "ruralz.revision"
	AttrConsumer      = "ruralz.consumer"
	AttrTier          = "ruralz.tier"
	AttrCode          = "ruralz.code"
	AttrPolicyType    = "ruralz.policy.type"
	AttrPhase         = "ruralz.phase"
	AttrOutcome       = "ruralz.outcome"
	AttrFailureMode   = "ruralz.failure_mode"
	AttrStateOp       = "ruralz.state.op"
	AttrStateDuration = "ruralz.state.duration"
	AttrStateBatch    = "ruralz.state.batch"
	AttrAttempt       = "ruralz.upstream.attempt"
	AttrCompStep      = "ruralz.composition.step"
)

// Process log keys (snake_case; sloglint no-raw-keys).
const (
	KeyComponent    = "component"
	KeyNodeID       = "node_id"
	KeyRevision     = "revision"
	KeyTraceID      = "trace_id"
	KeySpanID       = "span_id"
	KeyCode         = "code"
	KeyError        = "error"
	KeyFile         = "file"
	KeyLine         = "line"
	KeyColumn       = "column"
	KeyResourceKind = "resource_kind"
	KeyResourceName = "resource_name"
	KeyPath         = "path"
	KeyProvider     = "provider"
	KeyReference    = "reference"
	KeyShard        = "shard"
	KeyPolicy       = "policy"
	KeyUpstream     = "upstream"
	KeyListener     = "listener"
	KeyReason       = "reason"
)
