// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// rateWindow is the range of every dashboard rate: Grafana's interval that
// always spans at least four scrapes.
const rateWindow = "[$__rate_interval]"

// dashboards returns the eight M1 dashboards of OBS "Grafana dashboard
// pack" (09 req 73), in the table's order.
func dashboards() []dashboardDef {
	return []dashboardDef{
		overviewDashboard(),
		routeDashboard(),
		policiesDashboard(),
		upstreamsDashboard(),
		stateDashboard(),
		nodeDashboard(),
		telemetryDashboard(),
		sloDashboard(),
	}
}

// dashboardTitles returns the titles of the pack.
func dashboardTitles() []string {
	var out []string
	for _, d := range dashboards() {
		out = append(out, d.title)
	}
	return out
}

func overviewDashboard() dashboardDef {
	return dashboardDef{
		uid: "ruralz-gateway-overview", title: dashOverview,
		description: "Request rate, errors by origin, gateway-added latency and degraded states of every Node.",
		panels: []panelDef{
			ts("Requests by status class", unitReqps,
				q(`sum by (status_class) (rate(ruralz_http_listener_requests_total`+rateWindow+`))`, "{{status_class}}")),
			ts("5xx responses by origin", unitReqps,
				q(`sum by (origin) (rate(ruralz_http_listener_requests_total{status_class="5xx"}`+rateWindow+`))`, "{{origin}}")).
				withDescription("upstream: passed through from an Upstream; dependency: Node-generated for an Upstream failure; node: every other Node-generated response (SLO-GW-1)."),
			ts("Node-generated responses by code", unitReqps,
				q(`sum by (code) (rate(ruralz_http_node_responses_total`+rateWindow+`))`, "{{code}}")),
			ts("Gateway-added time", unitSeconds,
				q(`histogram_quantile(0.5, sum by (le) (rate(ruralz_http_gateway_duration_seconds_bucket`+rateWindow+`)))`, "p50"),
				q(`histogram_quantile(0.99, sum by (le) (rate(ruralz_http_gateway_duration_seconds_bucket`+rateWindow+`)))`, "p99")).
				withDescription("Wall-clock time minus client I/O, upstream I/O and State Store round trips (SLO-GW-2, SLO-GW-3)."),
			ts("Requests within the gateway-added time objectives", unitPercent,
				q(`sum(rate(ruralz_http_gateway_duration_seconds_bucket{le="0.001"}`+rateWindow+`)) / sum(rate(ruralz_http_gateway_duration_seconds_count`+rateWindow+`))`, "within 1 ms (SLO-GW-2)"),
				q(`sum(rate(ruralz_http_gateway_duration_seconds_bucket{le="0.00015"}`+rateWindow+`)) / sum(rate(ruralz_http_gateway_duration_seconds_count`+rateWindow+`))`, "within 150 µs (SLO-GW-3)")),
			ts("Active requests", unitShort,
				q(`sum by (listener) (ruralz_http_active_requests)`, "{{listener}}")),
			ts("Degraded reasons", unitShort,
				q(`max by (instance, reason) (ruralz_node_degraded_info{reason!="cleartext_hop"}) > 0`, "{{instance}} {{reason}}")).
				withDescription("Reasons currently raised; cleartext_hop is charted apart."),
			ts("Cleartext hop", unitShort,
				q(`max by (instance) (ruralz_node_degraded_info{reason="cleartext_hop"})`, "{{instance}}")).
				withDescription("1 while any hop runs in cleartext; the Node runtime and configuration dashboard breaks it down by hop."),
			ts("Open connections", unitShort,
				q(`sum by (listener, protocol) (ruralz_listener_open_connections)`, "{{listener}} {{protocol}}")),
			ts("Accepted and refused connections", unitOps,
				q(`sum by (listener, result) (rate(ruralz_listener_connections_total`+rateWindow+`))`, "{{listener}} {{result}}")),
		},
	}
}

func routeDashboard() dashboardDef {
	sel := `{route=~"$route"}`
	return dashboardDef{
		uid: "ruralz-route-detail", title: dashRoute,
		description: "Rate, errors, duration with exemplars and Response Cache results of the selected Routes.",
		vars:        []queryVar{{name: "route", label: "Route", metric: catalog.HTTPRequestsTotal}},
		panels: []panelDef{
			ts("Requests by status class", unitReqps,
				q(`sum by (route, status_class) (rate(ruralz_http_requests_total`+sel+rateWindow+`))`, "{{route}} {{status_class}}")),
			ts("5xx ratio", unitPercent,
				q(`sum by (route) (rate(ruralz_http_requests_total{route=~"$route",status_class="5xx"}`+rateWindow+`)) / sum by (route) (rate(ruralz_http_requests_total`+sel+rateWindow+`))`, "{{route}}")),
			ts("Duration", unitSeconds,
				targetDef{expr: `histogram_quantile(0.5, sum by (le, route) (rate(ruralz_http_request_duration_seconds_bucket` + sel + rateWindow + `)))`, legend: "p50 {{route}}", exemplar: true},
				targetDef{expr: `histogram_quantile(0.9, sum by (le, route) (rate(ruralz_http_request_duration_seconds_bucket` + sel + rateWindow + `)))`, legend: "p90 {{route}}", exemplar: true},
				targetDef{expr: `histogram_quantile(0.99, sum by (le, route) (rate(ruralz_http_request_duration_seconds_bucket` + sel + rateWindow + `)))`, legend: "p99 {{route}}", exemplar: true}).
				withDescription("Per Route, for unary exchanges; exemplars link sampled requests to their traces.").withWidth(gridColumns),
			ts("Response Cache results", unitReqps,
				q(`sum by (route, result) (rate(ruralz_cache_requests_total`+sel+rateWindow+`))`, "{{route}} {{result}}")),
			ts("Response Cache hit ratio", unitPercent,
				q(`sum by (route) (rate(ruralz_cache_requests_total{route=~"$route",result=~"hit|stale"}`+rateWindow+`)) / sum by (route) (rate(ruralz_cache_requests_total{route=~"$route",result!="bypass"}`+rateWindow+`))`, "{{route}}")),
		},
	}
}

func policiesDashboard() dashboardDef {
	sel := `{policy=~"$policy"}`
	return dashboardDef{
		uid: "ruralz-policies", title: dashPolicies,
		description: "Filter duration, short-circuits, failures and authentication decisions per Policy and Phase.",
		vars:        []queryVar{{name: "policy", label: "Policy", metric: catalog.FilterDurationSeconds + "_count"}},
		panels: []panelDef{
			ts("Filter calls", unitOps,
				q(`sum by (policy, phase) (rate(ruralz_filter_duration_seconds_count`+sel+rateWindow+`))`, "{{policy}} {{phase}}")),
			ts("Filter duration p99", unitSeconds,
				q(`histogram_quantile(0.99, sum by (le, policy, phase) (rate(ruralz_filter_duration_seconds_bucket`+sel+rateWindow+`)))`, "{{policy}} {{phase}}")),
			ts("Short-circuits by status class", unitReqps,
				q(`sum by (policy, phase, status_class) (rate(ruralz_filter_short_circuits_total`+sel+rateWindow+`))`, "{{policy}} {{phase}} {{status_class}}")),
			ts("Filter failures by mode", unitOps,
				q(`sum by (policy, phase, mode) (rate(ruralz_filter_failures_total`+sel+rateWindow+`))`, "{{policy}} {{phase}} {{mode}}")).
				withDescription("Failures applied with the Policy's failure mode: open lets the request continue, closed rejects it."),
			ts("Authentication decisions", unitOps,
				q(`sum by (policy, result) (rate(ruralz_auth_decisions_total`+sel+rateWindow+`))`, "{{policy}} {{result}}")),
			ts("Authentication denials by code", unitOps,
				q(`sum by (policy, code) (rate(ruralz_auth_decisions_total{policy=~"$policy",result="deny"}`+rateWindow+`))`, "{{policy}} {{code}}")),
			ts("JWKS and upstream token age", unitSeconds,
				q(`max by (policy) (ruralz_auth_jwks_age_seconds`+sel+`)`, "JWKS {{policy}}"),
				q(`max by (policy) (ruralz_auth_upstream_token_age_seconds`+sel+`)`, "token {{policy}}")),
			ts("Upstream token refresh failures", unitShort,
				q(`sum by (policy) (increase(ruralz_auth_upstream_refresh_failures_total`+sel+rateWindow+`))`, "{{policy}}")),
		},
	}
}

func upstreamsDashboard() dashboardDef {
	sel := `{upstream=~"$upstream"}`
	return dashboardDef{
		uid: "ruralz-upstreams", title: dashUpstreams,
		description: "Attempts, retries, circuit breakers, Endpoints, ejections and connection pools of the selected Upstreams.",
		vars:        []queryVar{{name: "upstream", label: "Upstream", metric: catalog.UpstreamAttemptDurationSeconds + "_count"}},
		panels: []panelDef{
			ts("Attempts by status class", unitOps,
				q(`sum by (upstream, status_class) (rate(ruralz_upstream_attempts_total`+sel+rateWindow+`))`, "{{upstream}} {{status_class}}")),
			ts("Attempt errors", unitOps,
				q(`sum by (upstream, error) (rate(ruralz_upstream_attempts_total{upstream=~"$upstream",error!="none"}`+rateWindow+`))`, "{{upstream}} {{error}}")),
			ts("Attempt duration", unitSeconds,
				targetDef{expr: `histogram_quantile(0.5, sum by (le, upstream) (rate(ruralz_upstream_attempt_duration_seconds_bucket` + sel + rateWindow + `)))`, legend: "p50 {{upstream}}", exemplar: true},
				targetDef{expr: `histogram_quantile(0.99, sum by (le, upstream) (rate(ruralz_upstream_attempt_duration_seconds_bucket` + sel + rateWindow + `)))`, legend: "p99 {{upstream}}", exemplar: true}).
				withDescription("Connect to last byte of one attempt."),
			ts("Retries and exhausted retry budgets", unitOps,
				q(`sum by (upstream) (rate(ruralz_upstream_retries_total`+sel+rateWindow+`))`, "retries {{upstream}}"),
				q(`sum by (upstream) (rate(ruralz_upstream_retry_budget_exhausted_total`+sel+rateWindow+`))`, "budget exhausted {{upstream}}")),
			ts("Circuit breakers not closed", unitShort,
				q(`max by (upstream, state) (ruralz_upstream_breaker_state_info{upstream=~"$upstream",state!="closed"})`, "{{upstream}} {{state}}")),
			ts("Healthy Endpoints", unitShort,
				q(`min by (upstream) (ruralz_upstream_healthy_endpoints`+sel+`)`, "{{upstream}}")),
			ts("Ejections", unitShort,
				q(`sum by (upstream, reason) (increase(ruralz_upstream_ejections_total`+sel+rateWindow+`))`, "{{upstream}} {{reason}}")),
			ts("Upstream degraded states", unitShort,
				q(`max by (upstream, reason) (ruralz_upstream_degraded_info`+sel+`) > 0`, "{{upstream}} {{reason}}")),
			ts("Pool connections", unitShort,
				q(`sum by (upstream, state) (ruralz_upstream_pool_connections`+sel+`)`, "{{upstream}} {{state}}")),
			ts("Skipped probes and CEL errors", unitShort,
				q(`sum by (upstream) (increase(ruralz_upstream_probes_skipped_total`+sel+rateWindow+`))`, "probes skipped {{upstream}}"),
				q(`sum by (upstream, field) (increase(ruralz_upstream_cel_errors_total`+sel+rateWindow+`))`, "{{field}} errors {{upstream}}")),
		},
	}
}

func stateDashboard() dashboardDef {
	return dashboardDef{
		uid: "ruralz-state-store", title: dashState,
		description: "State Store round trips, breaker and fallbacks, dropped writes, and Rate Limit, Quota and Response Cache decisions.",
		panels: []panelDef{
			ts("Round trips by op", unitOps,
				q(`sum by (op) (rate(ruralz_state_calls_total`+rateWindow+`))`, "{{op}}")),
			ts("Failed and skipped round trips", unitOps,
				q(`sum by (op, result) (rate(ruralz_state_calls_total{result!="ok"}`+rateWindow+`))`, "{{op}} {{result}}")),
			ts("Round-trip duration p99", unitSeconds,
				q(`histogram_quantile(0.99, sum by (le, op) (rate(ruralz_state_call_duration_seconds_bucket`+rateWindow+`)))`, "{{op}}")),
			ts("GCRA round trips within 1 ms", unitPercent,
				q(`sum(rate(ruralz_state_call_duration_seconds_bucket{op="gcra",le="0.001"}`+rateWindow+`)) / sum(rate(ruralz_state_call_duration_seconds_count{op="gcra"}`+rateWindow+`))`, "SLO-GW-5")),
			ts("Operations by kind", unitOps,
				q(`sum by (kind) (rate(ruralz_state_ops_total`+rateWindow+`))`, "{{kind}}")),
			ts("State Store degraded states", unitShort,
				q(`max by (instance, reason) (ruralz_node_degraded_info{reason=~"state_store_breaker_open|state_store_memory_fallback|state_store_unauthenticated|state_store_eviction_policy"}) > 0`, "{{instance}} {{reason}}")),
			ts("Dropped writes and write queue", unitShort,
				q(`sum by (kind) (increase(ruralz_state_writes_dropped_total`+rateWindow+`))`, "dropped {{kind}}"),
				q(`sum(ruralz_state_write_queue_items)`, "queued")),
			ts("Rate Limit decisions", unitOps,
				q(`sum by (policy, result) (rate(ruralz_ratelimit_decisions_total`+rateWindow+`))`, "{{policy}} {{result}}")),
			ts("Rate Limit and Quota fail-open", unitOps,
				q(`sum by (policy) (rate(ruralz_ratelimit_decisions_total{result="fail_open"}`+rateWindow+`))`, "rate limit {{policy}}"),
				q(`sum by (policy) (rate(ruralz_quota_decisions_total{result="fail_open"}`+rateWindow+`))`, "quota {{policy}}")),
			ts("Quota decisions", unitOps,
				q(`sum by (policy, result) (rate(ruralz_quota_decisions_total`+rateWindow+`))`, "{{policy}} {{result}}")),
			ts("Local bucket evictions", unitOps,
				q(`sum by (policy) (rate(ruralz_ratelimit_bucket_evictions_total`+rateWindow+`))`, "{{policy}}")),
			ts("Response Cache stores skipped", unitOps,
				q(`sum by (policy, reason) (rate(ruralz_cache_store_skipped_total`+rateWindow+`))`, "{{policy}} {{reason}}")),
		},
	}
}

func nodeDashboard() dashboardDef {
	return dashboardDef{
		uid: "ruralz-node-runtime", title: dashNode,
		description: "Go runtime, buffered bytes, Revision activations, retired snapshots, secret rotation and cleartext hops per Node.",
		panels: []panelDef{
			ts("Goroutines", unitShort, q(catalog.RuntimeGoroutines, "{{instance}}")),
			ts("Live heap", unitBytes, q(catalog.RuntimeHeapBytes, "{{instance}}")),
			ts("GC cycles", unitOps, q(`rate(ruralz_runtime_gc_cycles_total`+rateWindow+`)`, "{{instance}}")),
			ts("Buffered body bytes", unitBytes, q(catalog.NodeBufferedBytes, "{{instance}}")).
				withDescription("Bytes held against limits.maxBufferedBytes."),
			ts("Active and Last-Known-Good Revisions", unitShort,
				q(`max by (instance, role, revision) (ruralz_config_revision_info) > 0`, "{{instance}} {{role}} {{revision}}")),
			ts("Activations", unitShort,
				q(`sum by (result, code) (increase(ruralz_config_activations_total`+rateWindow+`))`, "{{result}} {{code}}")),
			ts("Activation stage duration p99", unitSeconds,
				q(`histogram_quantile(0.99, sum by (le, stage) (rate(ruralz_config_activation_duration_seconds_bucket`+rateWindow+`)))`, "{{stage}}")),
			ts("Retired snapshots", unitShort,
				q(catalog.ConfigRetiredSnapshots, "pinned {{instance}}"),
				q(`increase(ruralz_snapshot_retirement_ended_total`+rateWindow+`)`, "requests ended {{instance}}")),
			ts("Secret rotation failures", unitShort,
				q(`sum by (provider) (increase(ruralz_config_secret_rotation_failures_total`+rateWindow+`))`, "{{provider}}")),
			ts("Cleartext hops", unitShort,
				q(`max by (instance, hop) (ruralz_security_cleartext_hops)`, "{{instance}} {{hop}}")).
				withDescription("state_store, telemetry and admin hops raise a ticket; client and upstream hops behind a TLS-terminating load balancer chart here only."),
			ts("TLS handshake duration p99", unitSeconds,
				q(`histogram_quantile(0.99, sum by (le, listener) (rate(ruralz_listener_tls_handshake_duration_seconds_bucket`+rateWindow+`)))`, "{{listener}}")),
			ts("Body sizes p99", unitBytes,
				q(`histogram_quantile(0.99, sum by (le) (rate(ruralz_http_request_body_bytes_bucket`+rateWindow+`)))`, "request"),
				q(`histogram_quantile(0.99, sum by (le) (rate(ruralz_http_response_body_bytes_bucket`+rateWindow+`)))`, "response")),
		},
	}
}

func telemetryDashboard() dashboardDef {
	return dashboardDef{
		uid: "ruralz-telemetry-health", title: dashTelemetry,
		description: "Dropped spans and log records, rate-capped traces, folded and retiring series and export health.",
		panels: []panelDef{
			ts("Spans offered and dropped", unitOps,
				q(`sum(rate(ruralz_telemetry_spans_total`+rateWindow+`))`, "offered"),
				q(`sum by (reason) (rate(ruralz_telemetry_spans_dropped_total`+rateWindow+`))`, "dropped {{reason}}")),
			ts("Traces left unsampled by the rate caps", unitOps,
				q(`sum by (reason) (rate(ruralz_telemetry_traces_unsampled_total`+rateWindow+`))`, "{{reason}}")),
			ts("Log records produced and dropped", unitOps,
				q(`sum by (stream) (rate(ruralz_telemetry_logs_total`+rateWindow+`))`, "{{stream}}"),
				q(`sum by (stream, reason) (rate(ruralz_telemetry_logs_dropped_total`+rateWindow+`))`, "dropped {{stream}} {{reason}}")),
			ts("Export failing", unitShort,
				q(`max by (instance) (ruralz_node_degraded_info{reason="telemetry_export_failing"})`, "{{instance}}")),
			ts("Folded label sets", unitShort,
				q(`sum by (instrument) (ruralz_telemetry_folded_label_sets) > 0`, "{{instrument}}")).
				withDescription("Label sets folded into _overflow by the cardinality budget."),
			ts("Series", unitShort,
				q(`sum by (state) (ruralz_telemetry_series)`, "{{state}}")),
			ts("Dropped /tap events", unitOps,
				q(`sum(rate(ruralz_tap_events_dropped_total`+rateWindow+`))`, "dropped")),
			ts("Gateway-added time skipped", unitOps,
				q(`sum by (reason) (rate(ruralz_http_gateway_duration_skipped_total`+rateWindow+`))`, "{{reason}}")),
		},
	}
}

// sloDashboard charts, for every M1 SLO, the error budget left over 30 days
// and the burn rate over the alerting windows. SLOs with burn-rate rules
// draw the ticket and page burn rates as threshold lines; SLO-GW-3, whose
// 50% budget caps its burn rate at 2, draws the line at 1 where its
// RuralzGatewayP50 threshold rule tickets.
func sloDashboard() dashboardDef {
	d := dashboardDef{
		uid: "ruralz-slo-burn-rates", title: dashSLO,
		description: "Error budget remaining over 30 days and burn rate of every M1 SLO; 1 is the rate that spends the budget in exactly 30 days.",
	}
	for _, s := range slos() {
		budget := s.budget()
		pct := strconv.FormatFloat(float64(s.objective)*100/objectiveScale, 'f', -1, 64)
		thresholds := []float64{ticketFactor, pageFactor}
		burnDesc := fmt.Sprintf("Lines at %d (ticket when the 6 h and 30 min windows both exceed it) and %d (page when the 1 h and 5 min windows both exceed it).",
			ticketFactor, pageFactor)
		if s.objective < burnMinimum {
			maxBurn := strconv.FormatFloat(float64(objectiveScale)/float64(objectiveScale-s.objective), 'f', -1, 64)
			thresholds = []float64{1}
			burnDesc = fmt.Sprintf("No burn-rate rule: a %s%% objective caps the burn rate at %s. The line at 1 is the %s threshold, which tickets when the 1 h burn rate stays above 1 for 30 minutes.",
				pct, maxBurn, s.thresholdRule)
		}
		d.panels = append(d.panels,
			stat(fmt.Sprintf("%s budget remaining", s.id), unitPercent,
				q(fmt.Sprintf("1 - (%s) / %s", s.errorRatio("30d"), budget), "remaining")).
				withDescription(fmt.Sprintf("%s: %s, objective %s%% over 30 days.", s.id, s.title, pct)),
			panelDef{
				title: fmt.Sprintf("%s burn rate", s.id), description: burnDesc, kind: panelTimeseries, unit: unitShort, width: 18,
				thresholds: thresholds,
				targets: []targetDef{
					q(fmt.Sprintf("(%s) / %s", s.errorRatio("5m"), budget), "5 min"),
					q(fmt.Sprintf("(%s) / %s", s.errorRatio("1h"), budget), "1 h"),
					q(fmt.Sprintf("(%s) / %s", s.errorRatio("6h"), budget), "6 h"),
				},
			},
		)
	}
	return d
}
