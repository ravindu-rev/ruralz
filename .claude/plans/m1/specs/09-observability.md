# M1 spec, area 9 of 11: Observability

Design reader output for milestone M1 "Core gateway". Sources read in full for this area: `docs/architecture/10-observability.md` (OBS), `docs/adr/0010-telemetry-opentelemetry-first.md` (ADR10), `docs/_meta/foundation-pack.md` section 2 (PACK), `docs/roadmap/01-roadmap-and-milestones.md` "M1 Core gateway" (RM), plus the telemetry-relevant parts of Data plane (DP), Configuration model (CM), Security and identity (SEC), System overview (SO), Performance budgets and benchmarking (PERF), Testing and quality strategy (TEST), Repository layout and conventions (LAYOUT), Tech stack and libraries (TECH), Feature catalog (FC), Zero-downtime upgrades (ZDU) and CLI and API surface (CLI). Existing code read: `pkg/config/v1alpha1/gateway.go` (`Telemetry`, `OTLP`, `AccessLog`), `internal/errcode`, `internal/buildinfo`, `internal/gateway`, `internal/tool/{repocheck,depgate}`, `.golangci.yml`, `Makefile`, `go.mod`. Sibling specs consulted for interface alignment: 03-cel, 04-dataplane-core, 05-upstream-traffic, 06-security, 08-statestore.

Tags: "(target)" and "(hypothesis)" are the docs' own; "(proposed here)" marks a value or rule this spec adds because the docs leave it open (each is repeated in section 9).

## 1. Scope

Every M1 item in this area, with the section it comes from.

| # | M1 item | Source |
|---|---|---|
| S1 | OpenTelemetry traces: SDK tracer, span model `ruralz.route.match`, `ruralz.filter.<name>`, `ruralz.upstream.<name>`, server span; HTTP conventions minus `url.full`/`url.query` | RM "M1 scope" Observability row; OBS "Tracing" > "Span model"; ADR10 "Decision outcome" rows Traces, Conventions; PACK §2 "Trace spans"; FC "Observability" rows "OpenTelemetry tracing", "Spans per Filter and upstream leg" |
| S2 | W3C Trace Context extract, one sampling decision (`traceSampling`, root and parent caps), `traceparent`/`tracestate` injection with the decision into every HTTP upstream leg, trace ID as `requestId` | RM Observability row ("traceparent injection with its sampling decision"); OBS "Propagation"; DP "Error response format"; FC "Trace context propagation" |
| S3 | Export: OTLP only when `Gateway.spec.telemetry.otlp.endpoint` is set, over TLS (TB-12), bounded span/log queues counting `queue_full`/`export_error`, `telemetry_export_failing` | RM Observability row ("export"), Security row ("TLS to ... OTLP (TB-12)"); OBS "Telemetry pipeline"; ADR10 row Export; SO "Trust boundaries" TB-12; SEC "Transport security"; FC "Delivery to any backend through the Collector" |
| S4 | Resource: `service.name`, `service.version`, `service.instance.id` = `node.id` (ULID); never the Revision | RM Observability row ("resource", "ULID `node.id`"); OBS "Telemetry pipeline"; ADR10 row Resource; PACK §2 "Gateway instance", §8.11 |
| S5 | `os.Unsetenv` of every `OTEL_*` before any SDK constructor; `${OTEL_...}` substitution from the pre-removal environment in file-mode `ruralzd` | ADR10 row Export; ADR10 "Confirmation" (Environment golden test); CM "Environment substitution" |
| S6 | Metrics catalog (M1 rows), naming/label/unit rules, histogram sets, Ruralz-owned aggregates, striping, collection, gateway-added time | RM Observability row; OBS "Observability principles" (aggregate paragraphs), "Metrics catalog" (all subsections), "Gateway-added time"; ADR10 rows Metrics, Exemplars |
| S7 | `/metrics` exposition through the OpenTelemetry Prometheus exporter (decided, OQ-tech-stack-and-libraries-16), including `ruralz_listener_tls_handshake_duration_seconds`; OpenMetrics exemplars; identical OTLP and `/metrics` names | RM Observability row; OBS "Observability principles" (exporter paragraph); ADR10 "Consequences" (exporter bullet), "Confirmation" (catalog gates); DP "Admin endpoints"; FC "Prometheus metrics" |
| S8 | `ruralz_node_degraded_info` with the M1 reasons (such as `lkg_boot`, `state_store_breaker_open`), cleartext hop gauge | RM Observability row; OBS "Degraded states"; FC "Degraded-state reporting"; SEC "Transport security" |
| S9 | Process logs: `log/slog` JSON lines on stdout, OTLP logs through `otelslog`, non-blocking handler and bounded queue, `RURALZ_LOG_LEVEL` | RM Observability row ("logs"); OBS "Process logs"; ADR10 row Logs; FC "Structured process logs" |
| S10 | Access logs: one record per request in `onLog`, `accessLog.when` (CEL), pooled records, 4 KiB cap, 8,192-record and 4 MiB bounded queue, field table | OBS "Access logs"; ADR10 row Logs; CM "CEL expressions and allowed places" (`accessLog.when` row); RM CEL row; FC "Selectable access logs" |
| S11 | Cardinality budget: compile-time admission, `_overflow` folding, retiring ceiling, cardinality CI test (1,000 Hot Reloads) | RM Observability row ("cardinality test"); OBS "Cardinality budget"; ADR10 "Confirmation" (Cardinality test) |
| S12 | Catalog gates: code names and reasons in the catalog; doc-level name and reason check; dashboard panels on listed metrics; identical OTLP and `/metrics` names; repocheck metric and span name checks | RM Observability row ("catalog gates"); ADR10 "Confirmation" (Lint, Catalog gates); OBS "Ruralz Gateway metrics" (CI check), "Grafana dashboard pack"; LAYOUT "Ruralz-specific checks" |
| S13 | Grafana dashboard pack (M1 dashboards), SLO definitions with M1 SLIs, alert rules generated from the catalog, PromQL parse check | FC "Grafana dashboard pack" (Planned (M1); RM exit criterion 6 counts it); OBS "Grafana dashboard pack", "SLO definitions", "Alert rules" |
| S14 | Overhead budgets and telemetry benchmarks (PB-10, onLog stage budget, blocked-stdout test, 4 P and 32 P contention benchmark) | OBS "Overhead per signal"; ADR10 "Confirmation" (Benchmark gate, Blocked stdout test); PERF "Budget catalog" PB-10, "Per-stage latency budget" `onLog` row, "Memory budget" Telemetry row |
| S15 | Redaction in every signal (O4): no secret, credential header, query string or body in metrics, spans, logs or `/tap` | OBS "Observability principles" O4; SEC "Secrets" rule 2; RM Security row ("redaction unit tests; secret leak tests"); TEST "Security scanning" |
| S16 | `/tap` hook cost and `ruralz_tap_events_dropped_total` (the hub itself is area 4's `internal/gateway/tap`) | OBS "Debugging tools", "Overhead per signal"; CLI "Plugin, AI and development verbs" (`/tap` limits); RM Ruralz Gateway row (admin `/tap`) |
| S17 | End-to-end test in the file-mode Docker Compose environment with an OpenTelemetry Collector | ADR10 "Confirmation" (End-to-end test); TEST "End-to-end tests" |

## 2. Normative requirements

Reference abbreviations: OBS = `docs/architecture/10-observability.md`; ADR10 = `docs/adr/0010-telemetry-opentelemetry-first.md`; PACK = `docs/_meta/foundation-pack.md`; DP = `docs/architecture/03-data-plane.md`; CM = `docs/architecture/02-configuration-model.md`; SEC = `docs/architecture/08-security-and-identity.md`; SO = `docs/architecture/01-system-overview.md`; PERF = `docs/architecture/12-performance-budgets-and-benchmarking.md`; TEST = `docs/engineering/03-testing-and-quality-strategy.md`; LAYOUT = `docs/engineering/02-repository-layout-and-conventions.md`; TECH = `docs/engineering/01-tech-stack-and-libraries.md`; FC = `docs/features/01-feature-catalog.md`; ZDU = `docs/operations/02-zero-downtime-upgrades-and-hot-reload.md`; CLI = `docs/reference/01-cli-and-api-surface.md`.

### 2.1 Principles that bind every requirement

1. Traces and metrics MUST go through the OpenTelemetry Go SDK; logs through `log/slog` and the `otelslog` bridge; packages call only stable APIs and library churn stays inside `internal/telemetry`. [OBS "Observability principles" O1; ADR10 "Decision drivers"]
2. Telemetry MUST never block, slow or fail a request: no request goroutine waits on a lock held across I/O, a pipe or an exporter; every span and log queue is bounded and drops with a counter. [OBS O2; ADR10 "Decision drivers"]
3. Every degraded state MUST be a metric: `ruralz_node_degraded_info` with a fixed `reason`. [OBS O3]
4. No resolved secret, credential header, query string (`url.full`, `url.query`) or body reaches any signal or `/tap`. Resolved secrets arrive as area 6's `secret.Value`, whose `LogValue` is `[REDACTED]`; the slog handler MUST NOT call `Reveal`. [OBS O4; SEC "Secrets" rule 2]
5. Label values are Revision-bounded names or enumerations admitted at compile time. Never a label: Consumer, credential, client address, method, path, query, host, trace ID, `node.id`, any user-supplied string. Tier is allowed. [OBS O5, "Naming and label rules"]
6. One correlation key: (`trace_id`, server `span_id`) links logs, spans and exemplars; `requestId` carries the 32-hex trace ID (span ID in `requestId` is OQ-observability-13, not in M1). [OBS O6; DP "Error response format"]
7. Unsampled requests create no spans; `/tap` without a subscriber costs one atomic load. [OBS O7]
8. No phone-home; the only telemetry egress is the operator-configured OTLP endpoint. [OBS O8]

### 2.2 Process setup, environment and resource

9. `ruralzd` MUST, as the first action of `gateway.Run` and before any OpenTelemetry SDK constructor, snapshot the process environment and then `os.Unsetenv` every variable whose name starts with `OTEL_` (byte-exact prefix). All SDK and exporter options are passed explicitly; nothing reads `OTEL_*`. [ADR10 row Export]
10. The pre-removal snapshot MUST be passed to the config loader as the lookup for `${VAR}` substitution when file-mode `ruralzd` loads an unrendered source Bundle (area 1's `Options.Variables`), so `${OTEL_EXPORTER_OTLP_ENDPOINT:-https://otel-collector:4317}` still resolves; a `secretRef` with `provider: env` reads the live environment, so it cannot name an `OTEL_*` variable (it fails with RZ-CFG-026). [ADR10 row Export; CM "Environment substitution"]
11. Resource attributes are exactly: `service.name` = `ruralzd` (proposed here; `ruralz-control` in M2), `service.version` = `buildinfo.Get().Version`, `service.instance.id` = `node.id`, built with `resource.NewWithAttributes(semconv.SchemaURL, ...)` from `go.opentelemetry.io/otel/semconv/v1.43.0` (the schema URL the v1.46.0 SDK uses, so `Merge` never conflicts). No `resource.Default()`, host or process detectors, never the active Revision. Cluster and Environment are added only in Control mode (M2). [OBS "Telemetry pipeline"; ADR10 row Resource]
12. `node.id` is a ULID (26 uppercase Crockford base32 characters; 48-bit millisecond time plus 80 bits from `crypto/rand`, own code, OQ-tech-stack-and-libraries-15 decided), created once at first boot under `${RURALZ_DATA_DIR}/identity/` and read on every later boot; a present but unparsable file refuses start (never silently regenerated). Implemented by area 4 (`internal/ulid`, `internal/nodedir`, file `identity/node-id`); this area consumes the string. [PACK §2 "Gateway instance", §8.11; DP "Process and concurrency model"; OPS topologies "file-mode Node"]
13. The same resource MUST be used by the tracer, meter (OTLP) and logger providers. Providers rebuild once after Enrollment (M2); aggregates are independent of providers so counters never reset on a rebuild. [OBS "Telemetry pipeline"; ADR10 row Resource]
14. `otel.SetErrorHandler` and `otel.SetLogger` MUST be set once at setup to adapters that log through the Ruralz slog handler (component `telemetry`, rate-limited to one record per signal per export interval), because the SDK default writes to stderr through `log`; `grpclog.SetLoggerV2` likewise routes grpc-go logs to slog at WARN and above. No other OpenTelemetry global (`SetTracerProvider`, `SetMeterProvider`, `SetTextMapPropagator`, `otel/log/global`) is set; providers are passed explicitly. (proposed here) [ADR10 "Consequences" (global logger deprecation); LAYOUT "Code conventions" (State)]

### 2.3 OTLP export pipeline

15. A Node pushes OTLP only when the active Revision sets `Gateway.spec.telemetry.otlp.endpoint`. Without it: `/metrics` on 9901 still serves metrics, logs go to stdout as JSON lines, and trace context (the decision) still propagates, but no span is created. [OBS "Telemetry pipeline"]
16. Transport is OTLP/gRPC (proposed here; the docs' examples use port 4317): one `grpc.ClientConn` per endpoint configuration shared by `otlptracegrpc`, `otlpmetricgrpc` and `otlploggrpc` through `WithGRPCConn`; no compression (proposed here, CPU budget); exporter timeout 10 s and retry `{InitialInterval 1s, MaxInterval 5s, MaxElapsedTime 10s}` (proposed here).
17. Endpoint syntax (validated at compile time, off the request path): absolute URL, scheme `https` or `http`, non-empty host, optional port (default 4317), empty path or `/`, no userinfo, query or fragment (userinfo would put a credential in the Revision). A violation is RZ-CFG-005 through a schema `pattern` the configuration area adds, `^https?://[^/?#@\s]+/?$` (proposed here). [CM "Validation and diff semantics" > "Error codes"]
18. `https`: TLS 1.2 or newer, server certificate always verified (no field disables it), TLS 1.2 suites fixed to ECDHE with AES-GCM or ChaCha20-Poly1305, built by area 6's `tlsconf.Client`; system roots unless `otlp.tls.caCertificate` is set; `otlp.tls.clientCertificate`/`clientKey` (both or neither) enable mTLS via `GetClientCertificate` reading the current `secret.Store` value; `otlp.tls.sni` overrides the server name (OQ-observability-2 option (a), section 7). An unresolvable reference is RZ-CFG-026 and the Revision is rejected. [SEC "Transport security"; SO TB-12]
19. `http`: the Node still exports, sets `ruralz_security_cleartext_hops{hop="telemetry"}` = 1 and raises `cleartext_hop`; loopback and Unix sockets get no exception in M1 (OQ-observability-20 current option (b)). [OBS "Telemetry pipeline"; SEC "Transport security"]
20. Spans pass a Ruralz `sdktrace.SpanProcessor` (not `BatchSpanProcessor`): `OnEnd` increments `ruralz_telemetry_spans_total` and enqueues without blocking into a bounded queue of 8,192 spans (target, at most 8 MiB); a full queue drops with `ruralz_telemetry_spans_dropped_total{reason="queue_full"}`. One worker exports batches of up to 512 spans or every 1 s (proposed here) and on error adds the batch length to `ruralz_telemetry_spans_dropped_total{reason="export_error"}`. [OBS "Telemetry pipeline"; OBS "Overhead per signal"; ADR10 row Export]
21. OTLP logs pass a Ruralz `sdklog.Processor`: `OnEmit` clones the record (`Record.Clone`) into a bounded queue (8,192 records and 4 MiB, proposed here to match the access queue); worker and error accounting as in req 20 with `ruralz_telemetry_logs_dropped_total{stream, reason}`. [OBS "Telemetry pipeline"]
22. Span limits (proposed here, bound the 8 MiB queue): 32 attributes, attribute values truncated at 256 bytes, 8 events, 0 links per span, via `sdktrace.WithRawSpanLimits`.
23. `telemetry_export_failing` is raised when any signal's exports have failed continuously for longer than one metric export interval (15 s, proposed here) and cleared by that signal's next successful export. Export failures are logged at WARN at most once per signal per interval. [OBS "Degraded states"]
24. Endpoint, TLS material and `traceSampling` come from the active Revision and MUST apply on every Hot Reload without touching the request path: sampling ratio by one atomic store; an endpoint or TLS change builds the new connection and exporters off the request path, swaps them atomically into the processors and the OTLP metric reader, then shuts the old ones down with a 5 s bound (proposed here). A Revision without an endpoint stops OTLP and span creation at the swap. [DP "Activation" (compile before swap)]
25. At Drain, telemetry flushes spans, logs and a final metric export within 5 s, after the post-commit queue flush, then exits; anything left is dropped and counted. [ZDU drain timeline "25 to 30 s flush the post-commit queue and telemetry, 5 s at most"]
26. OTLP metric export: a `sdkmetric.PeriodicReader` with interval 15 s and timeout 10 s (proposed here), cumulative temporality, registered only while an endpoint is set, reading Ruralz aggregates through the Producer of req 51. [ADR10 row Metrics; OQ-observability-16 (a)]

### 2.4 Trace propagation and sampling

27. Extraction: parse `traceparent` per W3C Trace Context: version `00` is exactly 55 bytes `00-<32 lowercase hex>-<16 lowercase hex>-<2 hex>`; trace ID and parent ID not all zeros; version `ff` invalid; a higher version is parsed on its first 55 bytes when byte 55 is `-` or absent. Any failure means no valid parent and a new trace starts. `tracestate` is kept as the raw header value (at most 512 bytes and 32 list members, proposed here; otherwise dropped) and re-injected unchanged. Baggage is never propagated (`propagation.Baggage` is not installed). [OBS "Propagation" steps 1 and 3]
28. Decision, made once per request: a valid parent's sampled flag (bit 0 of flags); else the root ratio `traceSampling` (default 0.01 when absent, OQ-observability-1 (a)) applied deterministically to the trace ID as in `TraceIDRatioBased` (low 8 bytes `>> 1` below `uint64(ratio × 2^63)`; 0 never, 1 always). [OBS "Propagation" step 2]
29. Caps: two per-Node token buckets admit sampled decisions, root 1,000 per second and parent 500 per second, bursts equal to rates (target), implemented lock-free as GCRA on one `atomic.Int64` (emission interval T = 1 s / rate, tolerance τ = burst × T; admit iff `max(tat, now) + T - now ≤ τ`, CAS to install). A decision over its bucket becomes unsampled, counted in `ruralz_telemetry_traces_unsampled_total{reason="rate_cap_root"|"rate_cap_parent"}`; the request is never dropped. Untrusted clients' flags are honored within the parent cap (OQ-observability-4 (a)). Caps and buckets survive Hot Reloads. [OBS "Propagation"; OBS "Debugging tools" (only a sampled `traceparent` forces a trace)]
30. IDs are needed even unsampled: a new trace ID when no valid parent, and always a server span ID, from a lock-free random source (req 88); installed into the SDK through `sdktrace.WithIDGenerator`, whose `NewIDs`/`NewSpanID` return the IDs pre-chosen for the server span (stashed in the context) so the exported server span, the access log and `requestId` agree. [OBS "Propagation"; ADR10 row Traces]
31. Injection into every HTTP upstream leg attempt (gRPC M3; Kafka, NATS, MQTT M4): delete client-supplied `traceparent` and `tracestate` from the outbound header, then set `traceparent: 00-<trace-id>-<parent-id>-<flags>` where parent-id is the attempt's `ruralz.upstream.<name>` span ID when spans exist, else the server span ID, and flags is `01` when sampled else `00`; set `tracestate` when non-empty. [OBS "Propagation" step 3; RM ("traceparent injection with its sampling decision")]
32. The trace ID (32 lowercase hex) is `requestId` in every problem document; no trace ID response header (OQ-observability-9 (a)); no per-Route sampling (OQ-observability-5 (a)). [OBS "Propagation" step 4; DP "Error response format"]

### 2.5 Span model

33. A sampled request with an OTLP endpoint has one SERVER span named `<method> <route>`, where method is the request method if it is one of GET, HEAD, POST, PUT, DELETE, CONNECT, OPTIONS, TRACE, PATCH, else `_OTHER` (with `http.request.method_original`), and route is the matched Route name; unmatched requests use `<method>` alone (proposed here; the `_unmatched` value stays in attributes). Attributes: semconv v1.43 HTTP server attributes (`http.request.method`, `http.response.status_code`, `url.scheme`, `url.path`, `server.address`, `server.port`, `client.address`, `network.protocol.version`, `user_agent.original`, `error.type`) minus `url.full`, `url.query` and every `http.request.header.*`/`http.response.header.*`; plus `ruralz.route`, `ruralz.listener`, `ruralz.revision` (`rev-<12 hex>`), `ruralz.consumer`, `ruralz.tier`, `ruralz.code` (names proposed here). Status Error for 5xx only. [OBS "Span model"]
34. `ruralz.route.match` (INTERNAL) wraps Route matching before `onRequestHeaders`; attribute `ruralz.route` = matched Route or `_unmatched`. The server span name is set after the match. [OBS "Span model"; PACK §2 "Trace spans"]
35. `ruralz.filter.<name>` (INTERNAL, `<name>` = Policy `metadata.name`) once per Policy Phase call; attributes `ruralz.policy.type`, `ruralz.phase` (Phase name verbatim), `ruralz.outcome` (`continue`, `respond`, `cannot_decide`, `skipped`), `ruralz.failure_mode` (`open`/`closed`, only when applied), State Store `ruralz.state.op` and `ruralz.state.duration` (seconds). A shared State Store round trip is recorded on the first Filter's span and the others carry `ruralz.state.batch` = that Filter's name. `onLog` creates no span; `headers` and `transform` create spans only in the Phases their config uses; upstream-scoped Policies are children of their leg span; children follow pack 8.12 order. Span names are precomputed per snapshot (no per-request concatenation). [OBS "Span model"; PACK §8.12]
36. `ruralz.upstream.<name>` (CLIENT, `<name>` = Upstream `metadata.name`) once per leg attempt; attributes `ruralz.upstream.attempt` (from 1), `server.address`/`server.port` of the Endpoint, `http.response.status_code` or `error.type` (`connect`, `timeout`, `reset`, `tls`), `ruralz.composition.step` when the leg is a step. Status Error for 4xx/5xx or error. [OBS "Span model"]
37. `onChunk` spans (one per subscribed Policy per stream with chunk count) are M3; the helper exists but no M1 caller. [OBS "Span model"]
38. Sampled-trace cost: at most 30 µs CPU (export excluded) and 60 allocations for the 15-span `orders-summary` trace (target); unsampled at most 1 µs and 2 allocations (target). [OBS "Overhead per signal"]

### 2.6 Metric naming, units, labels and histograms

39. Names follow `ruralz_<component>_<name>_<unit>`: base units, counted noun for gauges, `_total` for counters, `_ratio` for fractions, `_info` for 1-or-0 gauges. OTLP instrument names and `/metrics` family names MUST be identical; `/metrics` MUST NOT append suffixes. [PACK §2 "Metrics"; OBS "Naming and label rules"; ADR10 "Decision drivers" (One name set)]
40. Units registered as UCUM (proposed mapping): seconds `s`, bytes `By`, info and ratio `1`, every counted noun as an annotation of the catalog Unit column with spaces as `_` (`{requests}`, `{label_sets}`). [OBS "Naming and label rules"]
41. Resource labels `route`, `upstream`, `policy`, `plugin`, `listener` carry `metadata.name` or listener name; `route="_unmatched"` without a match; unadmitted resources fold into `_overflow` (names are DNS labels, so `_` values never collide); `phase` is the Phase name; `code` a registered RZ code; `status_class` `1xx` to `5xx`; `protocol` `http1` or `http2` in M1. [OBS "Naming and label rules"]
42. Histograms use exactly these five sets of 13 bounds (16 series per label set with `+Inf`, `_sum`, `_count`); bucket selection compares the raw integer (nanoseconds or bytes) against bounds in the same integer unit, so SLO thresholds are exact:

```yaml
fast:    [0.00001, 0.000025, 0.00005, 0.0001, 0.00015, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.1]  # s
request: [0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 60]   # s
control: [0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 3600]          # s
bytes:   [64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864, 268435456, 1073741824]
ratio:   [0.5, 0.75, 0.9, 0.95, 0.98, 0.99, 1, 1.01, 1.02, 1.05, 1.1, 1.25, 1.5]
```

`_sum` is an integer in nanoseconds (`fast`), microseconds (`request`, `control`, rounded to nearest), bytes, or millionths (`ratio`), added with `atomic.AddUint64` (no float CAS on the request path); collection converts to float64 base units. [OBS "Naming and label rules"; OBS "Observability principles" (histogram `_sum` paragraph)]

43. M1 families (all other catalog rows are deferred, section 8). Scope classes: L = listener or enumeration-only (admitted first, never fold), R = Route, U = Upstream, P = Policy (resource families, foldable). "Striped" per req 49.

| Metric | Type (set) | Unit | Labels and values | Class | Striped |
|---|---|---|---|---|---|
| `ruralz_http_requests_total` | Counter | `{requests}` | `route`, `status_class`; includes sessions | R | first 1,000 Routes |
| `ruralz_http_listener_requests_total` | Counter | `{requests}` | `listener`, `protocol`, `status_class`, `origin` = `upstream`, `node`, `dependency` | L | yes |
| `ruralz_http_node_responses_total` | Counter | `{responses}` | `code` (Node-generated responses) | L | no |
| `ruralz_http_request_duration_seconds` | Histogram (request) | `s` | `route`; unary exchanges | R | first 1,000 Routes |
| `ruralz_http_gateway_duration_seconds` | Histogram (fast) | `s` | `listener` | L | yes |
| `ruralz_http_gateway_duration_skipped_total` | Counter | `{requests}` | `reason` = `clock_anomaly` | L | no |
| `ruralz_http_request_body_bytes`, `ruralz_http_response_body_bytes` | Histogram (bytes) | `By` | `listener` | L | yes |
| `ruralz_http_active_requests` | Gauge | `{requests}` | `listener` | L | yes |
| `ruralz_listener_open_connections` | Gauge | `{connections}` | `listener`, `protocol` | L | no |
| `ruralz_listener_connections_total` | Counter | `{connections}` | `listener`, `protocol`, `result` = `accepted`, `tls_failure`, `refused` | L | no |
| `ruralz_listener_tls_handshake_duration_seconds` | Histogram (request) | `s` | `listener`; successful TLS handshakes over TCP (QUIC M3) | L | no |
| `ruralz_filter_duration_seconds` | Histogram (fast) | `s` | `policy`, `phase`; one Filter call | P | Gateway-scoped Policies |
| `ruralz_filter_short_circuits_total` | Counter | `{responses}` | `policy`, `phase`, `status_class` | P | Gateway-scoped |
| `ruralz_filter_failures_total` | Counter | `{failures}` | `policy`, `phase`, `mode` = `open`, `closed` | P | no |
| `ruralz_auth_decisions_total` | Counter | `{decisions}` | `policy`, `result` = `allow`, `deny`, `code` | P | Gateway-scoped |
| `ruralz_auth_jwks_age_seconds`, `ruralz_auth_upstream_token_age_seconds` | Gauge | `s` | `policy` | P | no |
| `ruralz_auth_upstream_refresh_failures_total` | Counter | `{failures}` | `policy` | P | no |
| `ruralz_security_cleartext_hops` | Gauge | `{hops}` | `hop` = `client`, `upstream`, `state_store`, `telemetry`, `admin` | L | no |
| `ruralz_ratelimit_decisions_total` | Counter | `{decisions}` | `policy`, `result` = `allow`, `deny_local`, `deny_global`, `fail_open` | P | Gateway-scoped |
| `ruralz_ratelimit_bucket_evictions_total` | Counter | `{entries}` | `policy` | P | no |
| `ruralz_quota_decisions_total` | Counter | `{decisions}` | `policy`, `result` = `allow`, `deny`, `no_quota`, `fail_open` | P | Gateway-scoped |
| `ruralz_cache_requests_total` | Counter | `{requests}` | `route`, `result` = `hit`, `miss`, `bypass`, `stale`, `stale_error` | R | no |
| `ruralz_cache_store_skipped_total` | Counter | `{stores}` | `policy`, `reason` = `size_limit`, `buffer_budget`, `memory` | P | no |
| `ruralz_upstream_attempts_total` | Counter | `{attempts}` | `upstream`, `status_class`, `error` = `none`, `connect`, `timeout`, `reset`, `tls` | U | first 1,000 Upstreams, `error="none"` only |
| `ruralz_upstream_attempt_duration_seconds` | Histogram (request) | `s` | `upstream`; connect to last byte | U | first 1,000 Upstreams |
| `ruralz_upstream_retries_total`, `ruralz_upstream_retry_budget_exhausted_total` | Counter | `{retries}` | `upstream` | U | no |
| `ruralz_upstream_breaker_state_info` | Gauge | `1` | `upstream`, `state` = `closed`, `open`, `half_open` (exactly one is 1) | U | no |
| `ruralz_upstream_ejections_total` | Counter | `{ejections}` | `upstream`, `reason` = `passive`, `active` | U | no |
| `ruralz_upstream_healthy_endpoints` | Gauge | `{endpoints}` | `upstream` | U | no |
| `ruralz_upstream_probes_skipped_total` | Counter | `{probes}` | `upstream` | U | no |
| `ruralz_upstream_degraded_info` | Gauge | `1` | `upstream`, `reason` = `panic`, `discovery_stale`, `balancer_budget` | U | no |
| `ruralz_upstream_cel_errors_total` | Counter | `{errors}` | `upstream`, `field` = `hashKey`, `retryOn`, `failureWhen` | U | no |
| `ruralz_upstream_pool_connections` | Gauge | `{connections}` | `upstream`, `state` = `idle`, `active` | U | no |
| `ruralz_state_call_duration_seconds` | Histogram (fast) | `s` | `op` | L | yes |
| `ruralz_state_calls_total` | Counter | `{calls}` | `op`, `result` = `ok`, `error`, `timeout`, `skipped` | L | yes |
| `ruralz_state_ops_total` | Counter | `{operations}` | `kind` (single-operation `op`) | L | yes |
| `ruralz_state_writes_dropped_total` | Counter | `{writes}` | `kind` (write kind) | L | no |
| `ruralz_state_write_queue_items` | Gauge | `{items}` | none | L | no |
| `ruralz_config_revision_info` | Gauge | `1` | `revision` (`rev-<12 hex>`), `role` = `active`, `lkg`; at most 2 series | L | no |
| `ruralz_config_activations_total` | Counter | `{activations}` | `result` = `activated`, `rejected`; `code` (empty for `activated`) | L | no |
| `ruralz_config_activation_duration_seconds` | Histogram (control) | `s` | `stage` = `verify`, `plugin_compile`, `compile`, `swap`, `total`; `size_class` = `le1000`, `le10000`, `gt10000` Routes | L | no |
| `ruralz_config_retired_snapshots` | Gauge | `{snapshots}` | none | L | no |
| `ruralz_snapshot_retirement_ended_total` | Counter | `{requests}` | none (ended by RZ-RT-014) | L | no |
| `ruralz_config_secret_rotation_failures_total` | Counter | `{failures}` | `provider` = `env`, `file`, `kubernetes`, `vault` | L | no |
| `ruralz_node_degraded_info` | Gauge | `1` | `reason` (req 57) | L | no |
| `ruralz_node_buffered_bytes` | Gauge | `By` | none (of `limits.maxBufferedBytes`) | L | no |
| `ruralz_runtime_goroutines`; `ruralz_runtime_heap_bytes`; `ruralz_runtime_gc_cycles_total` | Gauge; Gauge; Counter | `{goroutines}`; `By`; `{cycles}` | none | L | no |
| `ruralz_telemetry_spans_total`; `ruralz_telemetry_spans_dropped_total` | Counter | `{spans}` | none; `reason` = `queue_full`, `export_error` | L | no |
| `ruralz_telemetry_traces_unsampled_total` | Counter | `{traces}` | `reason` = `rate_cap_root`, `rate_cap_parent` | L | no |
| `ruralz_telemetry_logs_total`; `ruralz_telemetry_logs_dropped_total` | Counter | `{records}` | `stream` = `access`, `process`; plus `reason` = `queue_full`, `export_error` when dropped | L | no |
| `ruralz_telemetry_folded_label_sets` | Gauge | `{label_sets}` | `instrument` = name of a foldable family | L | no |
| `ruralz_telemetry_series` | Gauge | `{series}` | `state` = `live`, `retiring` | L | no |
| `ruralz_tap_events_dropped_total` | Counter | `{events}` | none | L | no |

[OBS "Ruralz Gateway metrics"; RM Observability row]

44. Enumerations: `op` = `gcra`, `quota`, `budget`, `cache_get`, `semantic_get`, `script_multi`, `pipeline`, `cache_set`, `semantic_set`, `cache_invalidate`, `refund`, `settle`; `kind` = a write kind (`cache_set`, `semantic_set`, `cache_invalidate`, `refund`, `settle`) for dropped writes, any single-operation `op` otherwise; `phase` = the eight Phases plus `onChunk`. `origin`: `upstream` when the status came from an Upstream response; `dependency` for Node-generated responses with an `RZ-UP-*` code or `RZ-AI-004`, `RZ-AI-005`, `RZ-AI-013`; `node` for every other Node-generated response (RZ-STS included). [OBS "Ruralz Gateway metrics" (paragraph under the table)]
45. Every enumeration label set an alert rule references MUST exist at 0 from process start: all `ruralz_node_degraded_info` reasons of req 57, every `hop`, every `kind` of `ruralz_state_writes_dropped_total`, `ruralz_telemetry_folded_label_sets{instrument}` for every foldable family; listener families exist at 0 once the listener is admitted. Resource label sets without a `code` dimension are pre-created at 0 on admission; label sets with a `code` dimension are created on first record (proposed here). Every foldable family exports its `_overflow` label set (every resource label `_overflow`, each non-`code` enumeration) at 0 from its first admission. [OBS "Alert rules"; OBS "Cardinality budget"]
46. Runtime metrics are read at collection from `runtime/metrics`: `/sched/goroutines:goroutines`, `/gc/heap/live:bytes`, `/gc/cycles/total:gc-cycles`, never with SDK runtime names (OQ-observability-15 (a)). [OBS "Ruralz Gateway metrics" (runtime paragraph)]

### 2.7 Ruralz-owned aggregates

47. Metric values live in Ruralz-owned aggregates, not SDK synchronous instruments, keyed by (family, label values) and kept by name across Hot Reloads; each operation aggregates once and feeds both exporters. Recording costs at most 2 µs CPU and 0 allocations per request (target). [OBS "Observability principles"; OBS "Overhead per signal"; ADR10 row Metrics]
48. The request path records through handles resolved at snapshot compile time (per-Route, per-Upstream, per-Policy and per-listener structs of pointers); no map lookup, no string building, no lock. Folding redirects a handle with one atomic pointer load. [OBS "Observability principles"]
49. Striping: hot label sets (every request on a Node may record into them) shard into S = min(`GOMAXPROCS` at start, 8) stripes padded to 64-byte lines, within caps (target): listener and enumeration-only families at most 1,000 counter or gauge series and 64 histogram label sets; Gateway-scoped Policy label sets at most 1,024 counter series and 256 histogram label sets; the `ruralz_http_requests_total`/`ruralz_http_request_duration_seconds` label sets of the first 1,000 admitted Routes and the `ruralz_upstream_attempts_total`/`ruralz_upstream_attempt_duration_seconds` label sets of the first 1,000 admitted Upstreams, one 64-byte line per stripe holding up to 8 counters of one Route or Upstream (its status classes); rarer error series unsharded. Other label sets use one unpadded atomic per value. The stripe is assigned round robin per connection at accept, offset per request on multiplexed connections by a per-connection request sequence (`net/http` exposes no HTTP/2 stream ID; proposed here), and kept in the pooled request context; no runtime internal is read. [OBS "Observability principles"]
50. Counters: collection adds each stripe's delta since the previous collection, modulo 2^64, to a float64 total, so a wrap never shows. Gauges: sum of stripes (int64). [OBS "Observability principles"]
51. Collection (OQ-observability-16 option (a)): the registry implements `sdkmetric.Producer`, registered on the OTLP `PeriodicReader` (`sdkmetric.WithProducer`) and on the Prometheus exporter (`prometheus.WithProducer`). At most one collection in flight per process; the OTLP reader and every scrape arriving meanwhile share it; a result is reused for up to 1 s (target); the next collection reuses the pooled structure of the last once its readers release it (release points: the Ruralz wrapper around the OTLP metric exporter after `Export` returns, the `/metrics` handler after `Gather` returns; a structure held more than 30 s is abandoned, proposed here). Per-series `StartTime` is the admission time of the label set; a released and re-admitted name restarts from 0 with a new `StartTime`. [OBS "Observability principles"]
52. Exemplars: only sampled requests write, one slot per histogram label set per collection, under a sequence lock, with value, time, trace ID and the span ID of the span timing the value (server span for Route and listener durations, attempt span for Upstream durations, filter span for Filter durations); unsampled requests never touch a slot (not even a load). Exported in `metricdata` and as OpenMetrics exemplars on `/metrics`. [ADR10 row Exemplars; ADR10 "Confirmation" (Benchmark gate)]
53. Gateway-added time (`ruralz_http_gateway_duration_seconds`, access log `gateway_duration`) is wall-clock time minus the union of excluded sections: client reads and writes; upstream I/O (pooled-connection wait, dial, TLS, awaiting headers, blocked body reads and writes); State Store round trips; declared remote calls. Filters and copy work during a leg stay included. Unary exchanges end at the last byte; `text/event-stream` responses at header commit; sessions are not measured. [OBS "Gateway-added time"]
54. Union algorithm: one 64-bit word per request packs an excluded depth (high 16 bits) and the start of the current excluded run (low 48 bits, monotonic nanoseconds since request start), updated by CAS: entering increments the depth and the 0-to-1 transition stores the run start; leaving decrements it and the 1-to-0 transition adds the run to the request's excluded total with `atomic.AddInt64`; the result is wall-clock minus the total, read once at the end. A result below 0 or above wall-clock (or a leave at depth 0) is skipped and counted in `ruralz_http_gateway_duration_skipped_total{reason="clock_anomaly"}`, the only use of that counter. [OBS "Gateway-added time"]

### 2.8 Cardinality budget, folding and retiring

55. Admission at compile time, deterministic on every Node: listener and enumeration families first (never fold); then resources in (kind bytes, `metadata.name` bytes) order, and for each resource each family of that kind in catalog order, admitting the resource's label sets (static fan-out: product of enumeration sizes, where a `code` dimension counts the codes the Policy type can emit) while the family stays within 6,000 label sets per counter or gauge family and 2,000 per histogram family, and the Revision within 100,000 series (target); anything else folds into `_overflow`. Admission ignores retiring label sets. Limits are not configurable (OQ-observability-10 (a)). [OBS "Cardinality budget"]
56. Folding never carries accumulated values: a folded label set's series end (they leave both exports), only records after the fold go to `_overflow`; the same rule applies when a Hot Reload moves a live label set past a limit. A label set referenced only by retired or closing snapshots is retiring; a Node-wide ceiling of 25,000 retiring series (target) folds the oldest retiring sets beyond it, capping a Node at 125,000 series. A retiring set is released when its last snapshot is freed; a returning name restarts from 0. `ruralz_telemetry_series{state}` and `ruralz_telemetry_folded_label_sets{instrument}` report the counts. [OBS "Cardinality budget"]

### 2.9 Degraded states and cleartext hops

57. M1 reasons, all pre-created at 0: `lkg_boot`, `lkg_write_failed`, `state_store_memory_fallback`, `state_store_unauthenticated`, `state_store_breaker_open`, `state_store_eviction_policy`, `upstream_panic`, `discovery_stale`, `balancer_budget`, `probes_skipped` (more than 10% of probes skipped for 1 minute, target), `header_limit_capped`, `snapshot_ending_overdue`, `secret_rotation_failed`, `jwks_stale`, `cleartext_hop`, `telemetry_export_failing`. The value is 1 while at least one source holds the reason. Raise and clear transitions log at WARN and INFO. M2+ reasons (`detached`, `revision_signature_off`, `plugin_signature_off`, `plugin_pool_degraded`, `plugin_parked_bound_low`, `plugin_memlimit`, `state_store_memory_multi_node`, `node_count_unknown`, `revocation_sequence_gap`; M3 `semantic_cache_unsupported`) are not registered in M1. File mode without a declared ceiling is not degraded. [OBS "Degraded states"]
58. `ruralz_security_cleartext_hops{hop}` counts cleartext hops per kind (set by their owners: listener area `client`, Upstream area `upstream`, State Store area `state_store`, this area `telemetry`, admin area `admin`); `cleartext_hop` is raised while any hop is above 0. [OBS "Degraded states"; SEC "Transport security"]

### 2.10 `/metrics` exposition

59. `/metrics` on 9901 (mounted by the admin area behind the metrics token, operator token or client certificate, OQ-security-and-identity-7) serves only `ruralz_*` families from a private `prometheus.Registry` holding only the OpenTelemetry Prometheus exporter, configured with `WithRegisterer(reg)`, `WithProducer(registry)`, `WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithoutSuffixes)`, `WithoutScopeInfo()`, `WithoutTargetInfo()` (so the name set equals OTLP's). Never `prometheus.DefaultRegisterer`. [OBS "Observability principles"; DP "Admin endpoints"; CLI "Ruralz Gateway admin API"]
60. The handler negotiates `expfmt.NegotiateIncludingOpenMetrics`; OpenMetrics carries exemplars and `# EOF`. At most 4 scrapes encode at once (target); a fifth waits on its request context up to 10 s, then gets 503 (proposed here); the `Gather` result is shared read-only by concurrent scrapes for up to 1 s (proposed here, keeps memory flat with scraper count); each scrape encodes into a pooled 64 KiB `bufio.Writer` flushed chunk by chunk, with a 10 s write deadline via `http.ResponseController`; a scrape still writing after 10 s (target) is cancelled. [OBS "Observability principles"]

### 2.11 Process logs

61. Both binaries log through `log/slog` only via `internal/telemetry`; JSON lines on stdout and, with an OTLP endpoint, OTLP logs through `otelslog` (`NewHandler("github.com/ravindu-rev/ruralz", WithLoggerProvider(lp))`). No request goroutine runs a writing handler: the Ruralz handler checks the level, clones the record (`Record.Clone`) with its derived-handler state and trace context, and enqueues it without blocking into a bounded process-log queue (4,096 records and 1 MiB, proposed here; OBS gives 1 MiB memory); a full queue drops with `ruralz_telemetry_logs_dropped_total{stream="process", reason="queue_full"}`. [OBS "Process logs"; ADR10 row Logs]
62. The single stdout-owning worker encodes each record once for stdout (a `slog.JSONHandler` over a 64 KiB `bufio.Writer`, flushed when the queue drains) and once for the bridge. A stdout write error counts `export_error` for that stream. [ADR10 row Logs]
63. Record keys (snake_case, constants in `internal/telemetry`): `time` (RFC 3339 UTC with microseconds), `level` (`DEBUG`, `INFO`, `WARN`, `ERROR`), `msg`, `component`, `node_id`, `revision` (`rev-<12 hex>`, absent before the first activation), `trace_id`, `span_id` (when the context carries a valid span context), `code` (RZ code when one applies), `error`. [OBS "Process logs"]
64. `RURALZ_LOG_LEVEL` = `debug`, `info`, `warn`, `error`; unset or empty means `info`; any other value refuses start with exit code 2 and a message naming the allowed values (proposed here). The level is fixed for the process (OQ-observability-6 (a)) and never changes a Revision. At `info`, request handling emits no process log record (0 allocations, target). [OBS "Process logs"; PACK §2 "Env vars"]
65. `ruralz_telemetry_logs_total{stream}` counts records produced; each record increments `ruralz_telemetry_logs_dropped_total` at most once, at the first sink that loses it (stdout queue, stdout write, OTLP queue, OTLP export). (proposed accounting) [OBS "SLO definitions" SLO-GW-7]

### 2.12 Access logs

66. A Node writes one access log record per request in `onLog` (per session at close, M3). `Gateway.spec.telemetry.accessLog.when` (CEL over `request`, `source`, `route`, `consumer`, `auth`, `now`, `response`, `upstream`, `duration`; compiled by the CEL area with RZ-CFG-014/RZ-CFG-015 at validation) selects records: absent selects every request; `true` writes; a runtime error writes the entry. Destination, format and sampling are fixed (OQ-observability-3 (a)): stdout JSON lines plus OTLP logs when an endpoint is set. [OBS "Access logs"; CM "Allowed places"]
67. The request goroutine evaluates `when`, then copies every field, truncated, into a pooled record buffer; a record MUST NOT reference request memory. A record is at most 4 KiB (target); fields past that are cut and named in `truncated`. The queue is bounded twice (target): 8,192 records, and each record reserves its actual size from a 4 MiB byte budget released after encoding; either exhausted drops the record with `ruralz_telemetry_logs_dropped_total{stream="access", reason="queue_full"}`. Cost at most 2 µs at p99 including `when` and 8 allocations per logged request (target). [OBS "Access logs"; OBS "Overhead per signal"]
68. Fields, in this order (`time`, `level`, `msg` framing proposed here so stdout lines are uniformly slog-shaped: `"level":"INFO","msg":"access"`; OTLP LogRecord timestamp = request start, observed timestamp = emit time, `event.name` = `ruralz.access`):

| Field | Content and format |
|---|---|
| `time` | Request start, RFC 3339 UTC with microseconds |
| `trace_id`, `span_id`, `sampled` | 32 hex; server span ID 16 hex; bool |
| `node_id`, `revision` | ULID; pinned Revision `rev-<12 hex>` |
| `listener`, `protocol`, `route` | Listener name; `http1`/`http2`; Route name or `_unmatched` |
| `method`, `host`, `path`, `truncated` | No query string; `host` cut at 256 bytes, `path` at 1,024 bytes (target); `truncated` = JSON array of cut field names, omitted when empty (format proposed here) |
| `status`, `code` | Final HTTP status; RZ code, omitted when none |
| `duration`, `request_bytes`, `response_bytes` | Total time (seconds, JSON number with microsecond precision, proposed here); body bytes |
| `gateway_duration`, `upstream_duration`, `state_store_duration` | Req 53 value (omitted when skipped); upstream I/O; State Store round trips (seconds) |
| `client_address` | `source.ip` after trusted-proxy handling, full (OQ-observability-7 (a)) |
| `user_agent`, `tls_version` | `User-Agent` cut at 256 bytes (target); `1.2`/`1.3`, omitted on cleartext |
| `consumer`, `tier`, `auth_method` | When authenticated |
| `upstream`, `endpoint`, `attempts` | Last upstream leg |
| `short_circuit`, `failure_modes` | `{"policy","phase"}` of the responding Policy; array of `{"policy","phase","mode"}`, at most 8 (target) |
| `cache` | Response Cache result (`hit`, `miss`, `bypass`, `stale`, `stale_error`) |
| `messages_in`, `messages_out`, `ai` | Sessions and AI, M3; never emitted in M1 |

Bodies, header values other than `User-Agent` and `Host`, query strings, secrets and prompt text never appear; strings are JSON-escaped with invalid UTF-8 replaced by U+FFFD. [OBS "Access logs"]

### 2.13 `/tap` hook

69. With no subscriber, the `onLog` tap check costs one atomic load, under 10 ns and 0 allocations (target); with subscribers, at most 2 µs and 1 allocation per event, 1 MiB per subscriber, at most 4 (target); events lost to a slow subscriber count in `ruralz_tap_events_dropped_total`. Events are the redacted access-log view of trace-sampled requests (proposed here). The hub and stream are area 4's `internal/gateway/tap`. [OBS "Overhead per signal", "Debugging tools"; CLI (`/tap` limits)]

### 2.14 Budgets

70. Budgets per request on RH-1, telemetry at defaults minus off: metrics 2 µs, 0 allocations, 24 MiB live heap; unsampled tracing 1 µs, 2 allocations; sampled 15-span trace 30 µs, 60 allocations, 8,192-span queue at most 8 MiB; logged request 2 µs p99, 8 allocations, 8,192 records and 4 MiB; process logs at info none, 0, 1 MiB; all telemetry at defaults at most 5% of Node CPU at half saturation (PB-10) and 40 MiB live heap, up to 80 MiB RSS at `GOGC=100` (all target). The `onLog` stage (metrics, trace decision, one access-log record) is 5 µs p50, 15 µs p99, 10 allocations: 0 metrics, 2 tracing, 8 access log (target). [OBS "Overhead per signal"; PERF "Budget catalog", "Per-stage latency budget", "Memory budget"]

### 2.15 SLOs, alert rules and dashboards (M1 parts)

71. M1 SLIs, windows 30 days: SLO-GW-1 `1 − ruralz_http_listener_requests_total{status_class="5xx",origin="node"} / all` 99.95%; SLO-GW-2 `ruralz_http_gateway_duration_seconds` bucket `le="0.001"` / `_count` 99%; SLO-GW-3 same metric `le="0.00015"` 50%; SLO-GW-5 `ruralz_state_call_duration_seconds{op="gcra"}` `le="0.001"` / `_count` (`script_multi` excluded) 99% (hypothesis); SLO-GW-6 `ruralz_config_activation_duration_seconds{stage="compile",size_class!="gt10000"}` `le="2"` 99%; SLO-GW-7 `1 − dropped / produced` over `ruralz_telemetry_spans_*` and `ruralz_telemetry_logs_*` 99.9% (all target unless noted). [OBS "SLO definitions"]
72. Alert rules are generated from the catalog: burn-rate rules for every SLO objective of 90% or more (page when the 1 h and 5 min windows both burn at 14× the budget rate; ticket at 6× over 6 h and 30 min, target), plus the M1 rules `RuralzNodeDegradedPage`, `RuralzNodeDegraded`, `RuralzCleartextHop`, `RuralzGatewayP50`, `RuralzStateWritesDropped`, `RuralzSeriesFolded` with the exact expressions, `for` durations and severities of OBS "Alert rules"; Control and AI rules are M2/M3. Rare-event rules add `or (x > 0 unless x offset <window>)`. Every rule parses with a PromQL parser in CI, and a test shows each alerted series exists at 0 after start and each rule fires on a first event recorded before the first scrape. [OBS "Alert rules"]
73. M1 Grafana dashboards (Apache-2.0 JSON in the monorepo): Ruralz Gateway overview; Route detail; Policies and Filter Chain; Upstreams and resilience; State Store, Rate Limits and Quotas; Node runtime and configuration; Telemetry health; SLO burn rates. CI fails on a panel expression using a metric not in the catalog (stripping `_bucket`, `_sum`, `_count`). [OBS "Grafana dashboard pack"; FC "Grafana dashboard pack"]

### 2.16 Catalog gates and repocheck

74. Code gate: every `ruralz_*` family and degraded reason the code registers MUST appear in OBS "Ruralz Gateway metrics" and "Degraded states" with the same type, histogram set, unit and label names (test parsing the OBS tables). [ADR10 "Confirmation" (Catalog gates)]
75. Doc gate: a `ruralz_*` name or `ruralz_node_degraded_info` reason appearing anywhere under `docs/architecture/` but missing from the OBS catalog fails CI. [OBS "Ruralz Gateway metrics"]
76. Name identity gate: a golden run exports the same registry through OTLP (in-process collector) and `/metrics`; the two name sets MUST be equal. [ADR10 "Decision drivers", "Confirmation"]
77. repocheck (stage 1, standard-library program) gains the metric and span name checks that start with `internal/telemetry` in M1: (a) a string literal that is exactly a metric name (`^ruralz_[a-z0-9_]+$`) outside `internal/telemetry/catalog` is a finding ("use the catalog constant"); (b) a literal starting with `ruralz.route.`, `ruralz.filter.` or `ruralz.upstream.` outside the catalog is a finding; (c) a call `X.Start(ctx, "<literal>", ...)` with a string literal name is a finding (span names come from the catalog); (d) `catalog.Validate()` findings: pack grammar `^ruralz_[a-z][a-z0-9]*(_[a-z0-9]+)+$`, counters end `_total`, histograms end `_seconds`, `_bytes` or `_ratio`, gauges never end `_total`, unit `1` families end `_info` or `_ratio`, unique names, span names exactly `ruralz.route.match` or prefixes `ruralz.filter.`/`ruralz.upstream.`. Test files and `testdata` are exempt from (a) to (c). [LAYOUT "Ruralz-specific checks"; PACK §2; ADR10 "Confirmation" (Lint)]
78. Lint: `sloglint` gains `no-raw-keys: true`, `key-naming-case: snake`, `static-msg: true`; `spancheck` covers `internal/telemetry`; depguard confines `go.opentelemetry.io/otel*`, `go.opentelemetry.io/contrib/bridges/otelslog`, `github.com/prometheus/*` and (until M3) `google.golang.org/grpc` to `**/internal/telemetry/**`, and allows only `$gostd` in `**/internal/telemetry/catalog/**` (proposed amendment of ADR10 "More information"). [ADR10 "Confirmation", "More information"]

## 3. Proposed Go packages and API

All under `internal/`, lower-case singular names (LAYOUT "Code conventions"); refines the proposed `internal/telemetry/` entry of LAYOUT "Monorepo tree". No `init()`, no mutable package variables; registries are functions returning tables (as `errcode.All`). Every exported function that blocks takes `context.Context` first. OpenTelemetry, Prometheus and grpc-go imports stay inside `internal/telemetry/...`; other areas see only Ruralz types.

| Package | Contents | Third-party imports |
|---|---|---|
| `internal/telemetry` | `Runtime` (setup, env scrub, resource, providers, export pipelines, `Apply`, `Shutdown`), slog handler and stdout worker, degraded set, cleartext hops, `/metrics` handler, `GatewayTimer`, log keys | otel, sdk, sdk/metric, sdk/log, otlp*grpc, exporters/prometheus, otelslog, client_golang, common/expfmt, otlptranslator, grpc |
| `internal/telemetry/catalog` | Families, units, bounds, enumerations, reasons, hops, span names, attribute keys, alert rule table, `Validate`; imported by repocheck and generators | none (stdlib only) |
| `internal/telemetry/aggregate` | Registry, admission `Plan`, `Binding`, striped counters, gauges, histograms, exemplar slots, fold and retire, collection, `sdkmetric.Producer` | sdk/metric, sdk/metric/metricdata, sdk/instrumentation |
| `internal/telemetry/tracing` | W3C parse and format, `Decision`, caps, ID generator, span helpers, span processor, `Inject` | otel, trace, sdk/trace, semconv/v1.43.0 |
| `internal/telemetry/accesslog` | Pooled `Record`, field setters with truncation, `when` evaluation, JSON encoder | none (imports `internal/cel`) |
| `internal/tool/telemetrygen` | Generates `deploy/grafana/rules/ruralz.rules.yaml` from `catalog.AlertRules()`; checks dashboards; stage 3 drift | none |

Dashboards and rules live in `deploy/grafana/dashboards/*.json` and `deploy/grafana/rules/` (proposed; LAYOUT tags `deploy/` Planned (M2), section 9 item 16). `internal/ulid`, `internal/nodedir` and `internal/gateway/tap` are area 4's.

```go
package catalog // internal/telemetry/catalog — standard library only

type Kind uint8   // KindCounter, KindGauge, KindHistogram
type Bounds uint8 // BoundsNone, BoundsFast, BoundsRequest, BoundsControl, BoundsBytes, BoundsRatio
type Class uint8  // ClassListener (listener or enumeration-only, never folds), ClassRoute, ClassUpstream, ClassPolicy, ClassPlugin (M2)

type FamilyID uint16 // dense constants, one per M1 family, e.g. HTTPRequestsTotal, ListenerTLSHandshakeDurationSeconds

type Label struct {
	Name   string   // "route", "status_class", ...
	Values []string // enumeration values; nil for resource names and `code`
	Code   bool     // values are registered RZ codes (created on first record)
}

type Family struct {
	ID        FamilyID
	Name      string // exact pack-2 name
	Kind      Kind
	Bounds    Bounds
	Unit      string // UCUM: "s", "By", "1", "{requests}"
	Labels    []Label
	Class     Class
	Striped   bool   // hot by reach (req 49)
	Milestone string // "M1"; later rows are added with their milestone
	Help      string
}

func Families() []Family
func Lookup(name string) (Family, bool)
func (b Bounds) Seconds() []float64    // the 13 bounds in base units
func (b Bounds) Integers() []uint64    // the same bounds in ns (fast, request, control), bytes or millionths
func (b Bounds) SumScale() float64     // 1e-9, 1e-6, 1, 1e-6

type Reason uint8 // ReasonLKGBoot ... ReasonTelemetryExportFailing (16 M1 reasons)
func Reasons() []ReasonInfo            // {Reason, Name, Pages bool}
type Hop uint8                         // HopClient, HopUpstream, HopStateStore, HopTelemetry, HopAdmin

const (
	SpanRouteMatch     = "ruralz.route.match"
	SpanFilterPrefix   = "ruralz.filter."
	SpanUpstreamPrefix = "ruralz.upstream."
	AttrRoute          = "ruralz.route"
	AttrListener       = "ruralz.listener"
	AttrRevision       = "ruralz.revision"
	AttrConsumer       = "ruralz.consumer"
	AttrTier           = "ruralz.tier"
	AttrCode           = "ruralz.code"
	AttrPolicyType     = "ruralz.policy.type"
	AttrPhase          = "ruralz.phase"
	AttrOutcome        = "ruralz.outcome"
	AttrFailureMode    = "ruralz.failure_mode"
	AttrStateOp        = "ruralz.state.op"
	AttrStateDuration  = "ruralz.state.duration"
	AttrStateBatch     = "ruralz.state.batch"
	AttrAttempt        = "ruralz.upstream.attempt"
	AttrCompStep       = "ruralz.composition.step"
)

func FilterSpanName(policy string) string     // called at snapshot compile time only
func UpstreamSpanName(upstream string) string // idem

type AlertRule struct{ Name, Expr, For, Severity, Dashboard string }
func AlertRules() []AlertRule                  // M1 rules and generated burn-rate rules
func Validate() []error                        // req 77 (d)
```

```go
package aggregate // internal/telemetry/aggregate

type Stripe uint8

type Options struct {
	Stripes       int // min(GOMAXPROCS at start, 8)
	Clock         func() time.Time
	FamilyLimits  Limits // 6,000 / 2,000 / 100,000 / 25,000 retiring; tests only override
}

type Registry struct{ /* families, label-set table, retiring list, collection cache */ }

func NewRegistry(o Options) *Registry
func (r *Registry) StripeForConn() Stripe                    // round robin at accept
func (s Stripe) Offset(seq uint64, n int) Stripe             // per request on multiplexed conns

// Shape is what admission needs from a compiled Revision (built by the loader).
type Shape struct {
	Listeners []ListenerShape // name, protocols
	Routes    []string        // metadata.name
	Upstreams []string
	Policies  []PolicyShape   // name, type, scope (G, R, U), phases, codes it can emit
}

// Plan is the admission result; built off the request path, deterministic.
func (r *Registry) Admit(s Shape) (*Plan, error)
func (p *Plan) Route(name string) *RouteMetrics       // nil never: folded names get _overflow handles
func (p *Plan) Upstream(name string) *UpstreamMetrics
func (p *Plan) Policy(name string) *PolicyMetrics
func (p *Plan) Listener(name string) *ListenerMetrics

// Binding tracks which label sets a snapshot references (live, retiring, released).
func (r *Registry) Bind(p *Plan) *Binding // at the swap: live
func (b *Binding) Retire()                // snapshot retired or closing: its sets may become retiring
func (b *Binding) Release()               // snapshot freed: unreferenced sets end

type Counter struct{ /* *labelSet with atomic redirect */ }
func (c *Counter) Add(s Stripe, n uint64)
type StatusCounters struct{ /* 5 counters of one resource on one 64-byte line per stripe */ }
func (c *StatusCounters) Inc(s Stripe, status int)
type Gauge struct{ /* int64 per stripe or one atomic */ }
func (g *Gauge) Add(s Stripe, d int64)
func (g *Gauge) Set(v int64)
type AgeGauge struct{ /* unix nanos; value = now - t at collection */ }
func (g *AgeGauge) Touch(t time.Time)
type Histogram struct{ /* buckets per stripe, integer sum, exemplar slot */ }
func (h *Histogram) Record(s Stripe, v uint64)                       // ns, bytes or millionths
func (h *Histogram) RecordSampled(s Stripe, v uint64, ex Exemplar)   // only for sampled requests
type Exemplar struct{ TraceID [16]byte; SpanID [8]byte; Time time.Time }
type CodeCounters struct{ /* lazy per-code slots, CAS-installed */ }
func (c *CodeCounters) Inc(s Stripe, code string)

type RouteMetrics struct {
	Requests *StatusCounters // ruralz_http_requests_total
	Duration *Histogram      // ruralz_http_request_duration_seconds
	Cache    [5]*Counter     // ruralz_cache_requests_total by result
}
type UpstreamMetrics struct{ /* attempts, duration, retries, budget, breaker, ejections, healthy, probes, degraded, cel errors, pool */ }
type PolicyMetrics struct{ /* per phase: duration, short circuits, failures; auth, ratelimit, quota, cache store, ages */ }
type ListenerMetrics struct{ /* requests[protocol][class][origin], gateway duration, body bytes, active, conns, tls handshake */ }

func (r *Registry) Node() *NodeMetrics // enumeration families: state, config, node, runtime, telemetry, tap
func (r *Registry) GaugeFunc(id catalog.FamilyID, labels []string, f func() int64) // read at collection

// Producer implements go.opentelemetry.io/otel/sdk/metric.Producer.
func (r *Registry) Producer() *Producer
func (p *Producer) Produce(ctx context.Context) ([]metricdata.ScopeMetrics, error)
func (p *Producer) Release(gen uint64) // called by the OTLP exporter wrapper and the /metrics handler
```

```go
package tracing // internal/telemetry/tracing

type Decision struct {
	TraceID      [16]byte
	ServerSpanID [8]byte
	ParentSpanID [8]byte // remote parent when Remote
	Remote       bool
	Sampled      bool
	TraceState   string // raw, validated; re-injected unchanged
}

type Tracer struct{ /* provider, caps, ratio atomic, spansEnabled atomic, counters */ }

func ParseTraceparent(v string) (traceID [16]byte, parent [8]byte, flags byte, ok bool) // 0 allocs
func AppendTraceparent(dst []byte, traceID [16]byte, spanID [8]byte, sampled bool) []byte

func (t *Tracer) Decide(h http.Header, ratio float64, d *Decision) // extract, sample, caps; 0 allocs
func (t *Tracer) StartServer(ctx context.Context, d *Decision, method string, attrs ServerAttrs) (context.Context, Span)
func (t *Tracer) StartRouteMatch(ctx context.Context) (context.Context, Span)
func (t *Tracer) StartFilter(ctx context.Context, spanName string, a FilterAttrs) (context.Context, Span)
func (t *Tracer) StartUpstream(ctx context.Context, spanName string, a UpstreamAttrs) (context.Context, Span)
func Inject(ctx context.Context, d *Decision, h http.Header) // deletes client values, sets traceparent/tracestate
func AppendTraceID(dst []byte, d *Decision) []byte           // requestId, access log

type Span struct{ s trace.Span } // zero value is a no-op
func (s Span) SetRoute(route, spanName string)
func (s Span) SpanID() [8]byte
func (s Span) End(e EndInfo) // status code, RZ code, error type; records error status per semconv

// IDGenerator implements sdktrace.IDGenerator; returns IDs stashed by StartServer.
type IDGenerator struct{}
// Processor implements sdktrace.SpanProcessor with the bounded queue of req 20.
type Processor struct{ /* ring, worker, atomic exporter pointer, counters */ }
```

```go
package accesslog // internal/telemetry/accesslog

type Record struct{ /* fixed fields + 4 KiB arena + truncated bitset */ }

func (r *Record) Reset()
func (r *Record) SetPath(p string)       // copies, cuts at 1,024 bytes, flags truncated
func (r *Record) SetHost(h string)       // 256 bytes
func (r *Record) SetUserAgent(ua string) // 256 bytes
// ... one setter per field of req 68; numeric fields by value
func AppendJSON(dst []byte, r *Record, nodeID string) []byte

type Writer struct{ /* sync.Pool of *Record, MPSC ring 8,192, byte budget 4 MiB, counters, tap */ }
func (w *Writer) Selected(ctx context.Context, when *cel.Program, v *cel.Vars) bool // nil program: true; error: true
func (w *Writer) Acquire() *Record
func (w *Writer) Submit(r *Record) // non-blocking; drops with queue_full; publishes to tap when Active
```

```go
package telemetry // internal/telemetry

type Service uint8 // ServiceGateway; ServiceControl reserved (M2)

// Environ is the pre-removal environment snapshot.
type Environ struct{ /* map */ }
func ScrubOTELEnvironment() Environ        // snapshot, then os.Unsetenv every OTEL_*
func (e Environ) Lookup(name string) (string, bool)

func ParseLogLevel(s string) (slog.Level, error)

type Options struct {
	Service  Service
	NodeID   string    // ULID from internal/nodedir
	Version  string    // buildinfo.Get().Version
	Stdout   io.Writer // gateway.Run's stdout
	Level    slog.Level
	Clock    func() time.Time
	Stripes  int
}

type Runtime struct{ /* registry, tracer, access writer, log worker, pipelines, degraded */ }

func New(ctx context.Context, o Options) (*Runtime, error) // starts the stdout worker; no network
func (r *Runtime) Logger(component string) *slog.Logger
func (r *Runtime) Handler() slog.Handler                    // for slog.NewLogLogger(http.Server.ErrorLog)
func (r *Runtime) Metrics() *aggregate.Registry
func (r *Runtime) Tracer() *tracing.Tracer
func (r *Runtime) AccessLog() *accesslog.Writer
func (r *Runtime) Degraded() *Degraded
func (r *Runtime) SetCleartextHops(h catalog.Hop, n int)
func (r *Runtime) MetricsHandler() http.Handler            // mounted by the admin server
func (r *Runtime) SetActiveRevision(display string)        // at swap; logs carry it

// Settings is compiled from Gateway.spec.telemetry off the request path.
type Settings struct {
	Endpoint      *url.URL     // nil: no OTLP
	TLS           *tls.Config  // from tlsconf.Client plus otlp.tls (OQ-observability-2 (a))
	TraceSampling float64      // 0.01 when absent
}
func CompileSettings(t *v1alpha1.Telemetry, secrets secret.Store) (Settings, error) // RZ-CFG-005, RZ-CFG-026 via callers
func (r *Runtime) Apply(ctx context.Context, s Settings) error // at the swap; exporter changes off path
func (r *Runtime) Shutdown(ctx context.Context) error          // final flush within ctx (5 s at Drain)

type Degraded struct{ /* mutex-protected sources per reason; atomic values exported */ }
func (d *Degraded) Set(reason catalog.Reason, source string, on bool)

type GatewayTimer struct{ /* start, packed word, excluded total */ }
func (g *GatewayTimer) Reset(start time.Time)
func (g *GatewayTimer) Enter(now time.Time)
func (g *GatewayTimer) Leave(now time.Time)
func (g *GatewayTimer) Result(end time.Time) (time.Duration, bool) // false: clock_anomaly counted

const (
	KeyComponent = "component"; KeyNodeID = "node_id"; KeyRevision = "revision"
	KeyTraceID = "trace_id"; KeySpanID = "span_id"; KeyCode = "code"; KeyError = "error"
)
```

Concurrency model:

- Request goroutines use atomics only: striped counters, histogram buckets and sums, exemplar sequence lock (sampled only), GCRA caps (one CAS), access-log ring enqueue (bounded lock-free MPSC ring with per-slot sequence numbers; a buffered channel is an acceptable first cut if the budgets of req 70 hold), byte-budget reservation, tap `Active()` load, `GatewayTimer` CAS.
- Goroutines, each owned by `Runtime` and cancelled and awaited by `Shutdown`: one stdout worker (both log streams); one span export worker and one OTLP log export worker per active exporter set; the SDK `PeriodicReader` goroutine while an endpoint is set; at most one exporter-retirement goroutine at a time during `Apply`. Collection runs on the caller (reader or scrape) under a single-flight mutex. No goroutine is started per request.
- Locks off the request path: admission and binding (activation), collection single-flight, `Degraded` sources, tap subscription (area 4).
- `Apply` is called by the loader after the snapshot swap and before retirement; it never blocks the swap on network I/O (exporter construction dials lazily through `grpc.NewClient`).

Exports for other areas: `Runtime.Logger`, metric handle structs from `Plan` and `Registry.Node()`, `GaugeFunc`, `Degraded.Set`, `SetCleartextHops`, `tracing.Tracer` helpers and `Inject`, `AppendTraceID` for `requestId`, `accesslog.Writer` and `Record`, `GatewayTimer`, `MetricsHandler`, `ScrubOTELEnvironment`/`Environ`, `ParseLogLevel`, `catalog` constants for names, reasons, hops and span names. Area 8's `statestore.Recorder` and similar per-area recorder interfaces are implemented by thin adapters in the owning area over these handles (keeps `internal/telemetry` free of area imports).

## 4. Dependencies on other areas

| Direction | Area | Contract |
|---|---|---|
| Needs | 1 (config load) | `Options.Variables` fed from `Environ` for file-mode substitution; secret resolution of `otlp.tls` references (RZ-CFG-026); schema `pattern` for `otlp.endpoint` (RZ-CFG-005); schema default `traceSampling: 0.01` (OQ-observability-1) and the new `otlp.tls` fields in `pkg/config/v1alpha1` (OQ-observability-2 (a)); the loader builds `aggregate.Shape` and calls `Admit` at compile, `Bind`/`Retire`/`Release` at swap, retirement and free, `Apply` and `SetActiveRevision` after the swap; records `ruralz_config_*` |
| Needs | 2 (canonical, precedence) | Effective chain per Route to derive `PolicyShape` Phases and Gateway-scoped status (striping); `revision.Digest.Short()` for `rev-<12 hex>` |
| Needs | 3 (CEL) | `*cel.Program` for `Gateway.spec.telemetry.accessLog.when` (`PlaceAccessLogWhen`, runtime cost limit 1,000,000), `EvalBool(ctx, *cel.Vars)`; the Vars view with `response`, `upstream`, `duration` |
| Needs | 4 (data plane core) | `internal/ulid`, `internal/nodedir` (`node.id`); calls `Decide`, `StartServer`, `StartRouteMatch`, `StartFilter`, `Inject` via the Upstream layer, `AppendTraceID` for `requestId`; fills and submits access records in `onLog`; stripe per connection and request; `GatewayTimer` enter and leave around client I/O; records listener, request, node-response and snapshot metrics; `/metrics` mount; `internal/gateway/tap` hub; Drain calls `Shutdown` with 5 s |
| Needs | 5 (upstream and traffic) | Attempt spans, `Inject` per attempt, `GatewayTimer` around upstream I/O, Upstream, ratelimit, quota and cache metrics, degraded reasons `upstream_panic`, `discovery_stale`, `balancer_budget`, `probes_skipped`, hop `upstream` |
| Needs | 6 (security) | `tlsconf.Client` for OTLP (req 18), `secret.Store`/`secret.Value` (redacting `LogValue`), `redact` helpers for `/tap`, `adminauth` in front of `/metrics`, auth metrics, `jwks_stale`, `secret_rotation_failed`, hop `admin`; egress policy for the OTLP dialer if OQ-security-and-identity-22 rule 6 covers it |
| Needs | 7 (transform) | Filter spans and metrics through the executor (no own names) |
| Needs | 8 (State Store) | `statestore.Recorder` adapter over `Node()` handles (`op`, `result`, `kind`, queue items), `GatewayTimer` around round trips, filter-span State Store attributes, reasons `state_store_*`, hop `state_store` |
| Needs | `internal/errcode` | `All()` for `code` label validation and origin classification; no new code |
| Needs | CI and release | `promtool` pinned binary in `scripts/install-tools.sh` (PromQL parse and rule unit tests); depguard edits (req 78); Docker Compose Collector (stage 10) |
| Provides | All areas | Loggers, metric handles, span helpers, degraded and cleartext setters, catalog constants, `requestId`, access log API, `/metrics` handler, env scrub |

## 5. Libraries

Resolved together with `go mod tidy` on go1.27.1 in a scratch module (not the repository), 2026-09-25. All Apache-2.0 except `golang.org/x/*` and protobuf (BSD-3-Clause) and grpc-gateway (BSD-3-Clause); all declare `go 1.25.0` or lower (floor-safe); no linked package path contains `crypto` outside the standard library (G3 passes).

| Module | Version | Direct import | Why |
|---|---|---|---|
| `go.opentelemetry.io/otel`, `/trace`, `/metric`, `/sdk`, `/sdk/metric` | v1.46.0 | yes | TECH "Telemetry" row v1.46.x; stable traces and metrics; `sdkmetric.Producer`, `WithIDGenerator`, `WithRawSpanLimits` |
| `go.opentelemetry.io/otel/semconv/v1.43.0` (package in otel) | v1.46.0 | yes | Schema URL equal to the SDK's resource schema |
| `go.opentelemetry.io/otel/log`, `/sdk/log` | v0.22.0 | yes | Pre-stable Logs SDK behind the bridge (ADR10) |
| `go.opentelemetry.io/contrib/bridges/otelslog` | v0.20.1 | yes | TECH row v0.20.x; declares `go 1.25.0` in this release |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`, `/otlpmetric/otlpmetricgrpc` | v1.46.0 | yes | OTLP/gRPC export on 4317 |
| `go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc` | v0.22.0 | yes | OTLP logs |
| `go.opentelemetry.io/otel/exporters/prometheus` | v0.68.0 | yes | `/metrics` exporter decided (OQ-tech-stack-and-libraries-16); `WithProducer`, `WithTranslationStrategy` |
| `github.com/prometheus/client_golang` | v1.24.1 | yes (`prometheus.NewRegistry`) | Required by the exporter; private registry avoids the global default registerer |
| `github.com/prometheus/common` (`expfmt`) | v0.71.0 | yes | Streaming encoder and OpenMetrics negotiation for the bounded handler |
| `github.com/prometheus/otlptranslator` | v1.0.0 | yes | `UnderscoreEscapingWithoutSuffixes` constant |
| `google.golang.org/grpc` (+ `credentials`, `credentials/insecure`) | v1.84.0 (pin; exporters ask v1.83.1) | yes | Shared `ClientConn`, TLS credentials; TECH grpc-go row v1.84.x (first use moves from M3 to M1 here) |
| Transitive: `go.opentelemetry.io/proto/otlp` v1.11.0, `google.golang.org/protobuf` v1.36.12, `github.com/grpc-ecosystem/grpc-gateway/v2` v2.30.0, `github.com/cenkalti/backoff/v5` v5.0.3, `google.golang.org/genproto/googleapis/{api,rpc}`, `golang.org/x/net` v0.58.0, `golang.org/x/text` v0.41.0, `golang.org/x/sys` v0.47.0 (raised to v0.48.x by area 4), `github.com/prometheus/{client_model v0.6.2, procfs v0.21.1}`, `github.com/beorn7/perks`, `github.com/cespare/xxhash/v2`, `github.com/munnerz/goautoneg`, `github.com/go-logr/{logr,stdr}`, `github.com/google/uuid`, `go.opentelemetry.io/auto/sdk`, `go.yaml.in/yaml/v2`/`v3` | as resolved | no | Pulled by the above. OTLP/HTTP exporters link the same 66 grpc packages (through `go.opentelemetry.io/proto/otlp`), so gRPC transport costs nothing extra |
| Test only: `go.opentelemetry.io/otel/sdk/trace/tracetest`, `go.opentelemetry.io/proto/otlp/collector/{trace,metrics,logs}/v1` (in-process collector), `expfmt.TextParser` | as above | test | Golden and integration tests without Docker |
| CI binary: `promtool` (Prometheus, Apache-2.0), pinned and checksum-verified by `make tools` | Prometheus 3.x release current at M1 | no (binary) | PromQL parse and `promtool test rules`; build tools stay out of `go.mod` (LAYOUT "Toolchain and modules") |

Not used: `prometheus/client_golang/promhttp` (its in-flight limit rejects instead of waiting), `BatchSpanProcessor` and SDK `BatchProcessor` (no drop counters), OTel global providers, any vendor SDK, a third-party ULID library.

## 6. Test plan

Hermetic unit tests (fake clock, fake randomness, `-race`, shuffled, also `CGO_ENABLED=0`), stage 5 unless noted.

### 6.1 Unit and table tests

1. `catalog`: `Validate()` returns nothing; every M1 family of req 43 present with type, set, unit and labels; each FamilyID dense and unique; `FilterSpanName("jwt-default") == "ruralz.filter.jwt-default"`; bounds in integers equal bounds in seconds × scale exactly; 16 M1 reasons, M2+ names absent.
2. Doc gate (req 74, 75): parse OBS "Ruralz Gateway metrics" and "Degraded states" tables; every code family matches its row (name, type, histogram set, label names); every `ruralz_*` token and reason under `docs/architecture/*.md` is in the OBS tables. Negative fixture: a temp doc with `ruralz_bogus_total` fails.
3. Env scrub: set `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_TRACES_SAMPLER=always_off`, `OTEL_SERVICE_NAME`, `OTEL_GO_X_EXEMPLAR`; after `ScrubOTELEnvironment` each `os.Getenv` is empty, `Environ.Lookup` returns the old values, a variable named `XOTEL_A` is untouched.
4. `ParseLogLevel`: `debug`, `info`, `warn`, `error`, empty → levels; `INFO`, `trace`, `warning` → error (exit 2 at startup).
5. Endpoint validation table: `https://c:4317` ok; `http://127.0.0.1:4317` ok plus hop `telemetry` = 1 and `cleartext_hop`; `grpc://c`, `https://u:p@c`, `https://c/v1/traces`, `https://c?x=1`, `c:4317`, empty host → RZ-CFG-005.
6. OTLP TLS: CA replaces system roots; client cert pair both-or-neither; unresolvable `secretRef` → RZ-CFG-026 and the previous Revision keeps serving; rotation of the client key reaches the next handshake without rebuilding exporters; `MinVersion` is TLS 1.2 and `InsecureSkipVerify` false in every config.
7. `traceparent` parser table: valid version 00 sampled and unsampled; uppercase hex, all-zero trace ID, all-zero parent ID, version `ff`, length 54 and 56, version `01` with a suffix (accepted on 55 bytes), non-hex → invalid; `tracestate` over 512 bytes or 33 members dropped, otherwise passed through byte-exact.
8. Sampling: parent sampled within cap → sampled; parent unsampled → unsampled without counter; no parent with ratio 0, 1, 0.01; parent cap exhausted → unsampled plus `rate_cap_parent`; root cap exhausted → `rate_cap_root`; client-forced flags never consume root tokens; ratio change through `Apply` takes effect on the next request.
9. Injection: client `traceparent` and `tracestate` replaced; parent-id is the attempt span ID when spans exist, the server span ID otherwise; flags `01`/`00`; no `baggage` added; `AppendTraceID` equals the server span's trace ID and the access log `trace_id`.
10. Span model with `tracetest.InMemoryExporter`: names, kinds, parents and attributes; `_OTHER` for `PROPFIND` with `http.request.method_original`; no `url.full`, `url.query` or header attribute on any span; `onLog` creates no span; server span Error on 5xx only; client span Error on 4xx/5xx and on each `error.type`.
11. Aggregates: counter, gauge and histogram arithmetic per stripe; bucket choice at exact bounds (`le="0.00015"` receives 150,000 ns, not 150,001 ns); `_sum` scale per set; wrap of a stripe near 2^64 keeps the float total monotonic; `StatusCounters` for statuses 100 to 599 map to classes.
12. Origin classification over `errcode.All()`: every `RZ-UP-*` and `RZ-AI-004`, `-005`, `-013` → `dependency`; any other code → `node`; a passed-through Upstream status → `upstream`; recording an unregistered code into `ruralz_http_node_responses_total` creates no series and logs once at DEBUG (asserted here; repocheck already rejects unregistered code literals, and no build tag changes the behavior).
13. Pre-creation (req 45): after `New` with no Revision, `/metrics` shows every M1 reason at 0, the five hops at 0, every write `kind` of `ruralz_state_writes_dropped_total` at 0, `ruralz_telemetry_traces_unsampled_total` reasons at 0.
14. Degraded: two sources raising the same reason keep 1 until both clear; transitions log once each.
15. `GatewayTimer` table: nested sections, parallel `aggregate` legs overlapping, section spanning request end, leave at depth 0, clock jump backwards → `clock_anomaly`.
16. Process logs: keys and order of req 63; `secret.Value` logs `[REDACTED]`; context span IDs appear; `revision` absent before activation; debug records at `info` cost 0 allocations (`testing.AllocsPerRun`).
17. Access log encoder: field order and formats of req 68; truncation of `host` at 256, `path` at 1,024, `user_agent` at 256 with `truncated` listing them; 4 KiB total cap; invalid UTF-8 → U+FFFD; control characters escaped; `failure_modes` capped at 8; no query string even when the request has one.
18. `accessLog.when`: absent → logged; `response.status >= 400` false → not logged; runtime error (`int(request.headers["x"]) > 1` with `x: a`) → logged.
19. repocheck (`internal/tool/repocheck/testdata/bad`): a `"ruralz_http_requests_total"` literal outside the catalog, a `"ruralz.filter.x"` literal, `tr.Start(ctx, "custom")` → findings; the same in `_test.go` and in `internal/telemetry/catalog` → none; a catalog fixture with `ruralz_x_count` (counter without `_total`) → finding.
20. Alert rules: `telemetrygen` output equals the committed YAML (stage 3); every metric in every rule and dashboard panel exists in the catalog (dashboard fixture with `ruralz_unknown_total` fails).

### 6.2 Property tests (`testing.F`, seeds in `pr-fast`, long runs nightly)

21. Admission determinism: any permutation of a `Shape`'s input order yields an identical `Plan` (same admitted and folded sets); limits never exceeded; listener and enumeration families never fold.
22. Folding: for random record streams interleaved with Hot Reloads moving label sets past limits, the family sum over exported series never increases by more than the records recorded since the previous collection (no jump from a folded set's accumulated value).
23. Histogram invariants: cumulative buckets non-decreasing; `_count` equals the `+Inf` bucket; `_sum` exact.
24. GCRA caps: over any window W, admitted ≤ rate × W + burst, under concurrent callers (race detector on).
25. Ratio sampling: deterministic per trace ID; over 10^6 random IDs the sampled fraction is within 4 standard deviations of the ratio.
26. `GatewayTimer`: result equals wall-clock minus the measure of the union of random excluded intervals from a reference implementation (FuzzGatewayTimer).

### 6.3 Fuzz targets (stage 11 nightly; seeds in stage 5)

27. `FuzzTraceparent`: never panics; accepted input re-formats to the same lowercase bytes; differential against `propagation.TraceContext{}` extraction on the same header.
28. `FuzzTracestate`: pass-through or drop, never a modified value.
29. `FuzzAccessLogRecord`: arbitrary field bytes → exactly one line that `encoding/json` parses, no raw newline, at most 4 KiB plus framing.
30. `FuzzAdmission`: random shapes (names, counts up to the limits) → no panic, determinism of 21.
31. `FuzzGatewayTimer` (26).

### 6.4 Golden tests (`testdata/`, `eol=lf`)

32. `/metrics` text 0.0.4 and OpenMetrics output of a fixed registry (two listeners, three Routes, two Upstreams, the five Gateway Policies of the CM worked example, one folded Route) with a fake clock, including exemplars in OpenMetrics only; `le` rendering pinned (`1e-05`, `0.00015`, `2` in text; `2.0` in OpenMetrics).
33. OTLP golden: the same registry through the in-process collector, `ResourceMetrics` normalized with `protojson` (sorted); name set equal to 32's family set (req 76); resource byte-identical with and without the `OTEL_*` variables of test 3 (ADR10 Environment golden test).
34. Span tree golden for the CM `orders-summary` Route: the 15 spans of OBS Figure 2 (names, kinds, parent links, order).
35. Access log and process log line goldens.

### 6.5 Integration tests (`-tags integration`, stage 8; no Docker needed)

36. In-process OTLP gRPC collector over TLS with a test CA: traces, metrics (15 s interval shortened in test via options) and logs arrive with the resource; headers carry nothing (no `OTEL_EXPORTER_OTLP_HEADERS` leakage).
37. Collector down: spans `export_error` counted per batch record; after one interval `telemetry_export_failing` = 1; recovery clears it.
38. Collector stalls: span queue fills; `queue_full` drops counted; request latency unaffected (p99 delta within the unsampled budget on a load loop).
39. Blocked stdout (ADR10 Blocked stdout test): stdout is a pipe nobody reads; 100,000 requests through a `ruralzd` built in-process keep `onLog` within req 70 budgets and count access and process `queue_full` drops; `Shutdown` returns within its 5 s context.
40. Hot Reload of telemetry settings: endpoint A → B → none: exports move, no span is created after "none", old connection closed within 5 s; `traceSampling` change applies.
41. Cardinality test (req 56, OBS "Cardinality budget"; stage 8): 1,000 Hot Reloads each renaming every Route of a 1,000-Route Bundle with streams pinning snapshots (K = 2 plus one closing): `ruralz_telemetry_series{state="retiring"}` ≤ 25,000 throughout, only retiring sets fold, no `_overflow` series rises by a folded set's accumulated value, the live count returns to the single-Revision count once old snapshots are freed, and at most 2 `ruralz_config_revision_info` series ever coexist.
42. State Store round-trip exclusion with the local `redis-server` 7.0.15: `gateway_duration` excludes the GCRA round trip (a fault proxy delays replies 5 ms; gateway-added time stays under 1 ms).
43. Alert rules: `promtool check rules` and `promtool test rules` with series that start at 1 before the first scrape for each rare-event rule and with pre-created zero series for degraded reasons.

### 6.6 Benchmarks and gates (stage 9; RH-1 macro gates when OQ-performance-budgets-and-benchmarking-1/-2 close)

44. `BenchmarkMetricsPerRequest` (15 operations, SM-4 scenario) and `...10Filters` (40 operations): 0 allocs/op, ns/op reported; `BenchmarkUnsampledTrace` ≤ 2 allocs, ≤ 1 µs; `BenchmarkSampledTrace15Spans` ≤ 60 allocs, ≤ 30 µs; `BenchmarkAccessLogWhen` ≤ 8 allocs; `BenchmarkTapNoSubscriber` < 10 ns, 0 allocs; `BenchmarkOnLogStage` ≤ 10 allocs.
45. Contention: one hot Route through the five Gateway Policies at `-cpu 4,32` (32 P runs oversubscribed on the 4-CPU environment, correctness and scaling trend only); unsampled requests never touch exemplar slots (instrumented counter in test build).
46. Memory: live heap of a registry at 125,000 series with S = 8 within the OBS model (about 8.3 MiB aggregates, 8.3 MiB collection, 5.4 MiB striping, hypothesis); `BenchmarkCollect125k` and `BenchmarkScrape4Concurrent` (heap does not grow with a fifth scraper).
47. PB-10: S2 with telemetry on versus off, at most 5% CPU at half saturation (release gate on RH-1).

### 6.7 End-to-end and security (stage 10)

48. Docker Compose file mode with an OpenTelemetry Collector: spans, metrics and logs arrive; `requestId` of a 404 finds its access log line and trace (not runnable in this environment: no Docker daemon).
49. Redaction unit tests (stage 5) and secret leak tests (stage 10): a canary secret in `otlp.tls.clientKey`, a `secretRef` State Store URL with password, an `Authorization` header and a query string never appear in stdout, OTLP payloads, `/metrics` or `/tap`.

Error-code paths covered: RZ-CFG-005 (endpoint, `traceSampling` range), RZ-CFG-014 and RZ-CFG-015 (`accessLog.when`, CEL area tests plus one loader test here), RZ-CFG-026 (OTLP TLS references), RZ-CFG-012 (literal in `otlp.tls` `SecretValue`), the `code` and `origin` labels for every registered code (test 12), RZ-RT-014 → `ruralz_snapshot_retirement_ended_total`, RZ-RT-005 → `ruralz_http_node_responses_total{code="RZ-RT-005"}` (PB-11).

## 7. Open questions blocking M1 in this area

Rows whose Blocking column names M1 and that touch this area (grep of every "Open questions" table on 2026-09-25), plus decided selections the code depends on.

| ID | Question | Adopt | What the code does |
|---|---|---|---|
| OQ-observability-2 (Yes, TLS export M1) | How does OTLP export get TLS client settings and authentication? | (a) `telemetry.otlp` fields, limited in M1 to TLS: `otlp.tls` with the `UpstreamTLS` shape (`sni`, `caCertificate`, `clientCertificate`, `clientKey`, all `SecretValue`); authentication headers stay Planned (M5) per FC "Authenticated OTLP export" | Config area adds the field (schema and canonical form change); `CompileSettings` builds `tls.Config` via `tlsconf.Client` plus these values; rotation through `GetClientCertificate`; OBS "Scope" sentence about three fields needs an amendment |
| OQ-observability-16 (Yes, M1 metrics) | Which SDK interface exports Ruralz aggregates and ends series? | (a) External producer (`sdkmetric.Producer`): (b) cannot express histograms (no observable histogram in the OTel API); (c) needs an ADR10 amendment and own encoders | `aggregate.Producer` registered on the OTLP `PeriodicReader` and the Prometheus exporter; series end by omission; per-series `StartTime` restarts on re-admission; fallback to (c) only if PB-10 or the scrape memory benchmarks fail |
| OQ-security-and-identity-7 (Yes, M1) | Which settings hold admin credentials? | (a) `RURALZ_ADMIN_METRICS_TOKEN_FILE` (`/metrics` only), `RURALZ_ADMIN_TOKEN_FILE`, `RURALZ_ADMIN_TLS_DIR` | Admin area guards `MetricsHandler`; without any credential `/metrics` returns 401; hop `admin` set when admin TLS is absent |
| OQ-security-and-identity-22 (Yes, M1) | How are secrets and Node connections restricted? | (a) as proposed | `otlp.tls` file references resolve only under `RURALZ_SECRET_ROOT`; whether the OTLP dialer falls under rule 6 (loopback collectors need `RURALZ_FETCH_ALLOW`) is decided by the security area; `Runtime` accepts an injected `grpc.WithContextDialer` so the egress guard can apply |
| OQ-performance-budgets-and-benchmarking-1, -2; OQ-testing-and-quality-strategy-2 (Yes, M1 macro and benchmark gates) | Load tools, RH-1 funding, statistics and runners | (a) proposed options of each | PB-10 and the telemetry macro checks run as release gates once RH-1 exists; component benchmarks of 6.6 gate alloc/op in stage 9 meanwhile |
| OQ-tech-stack-and-libraries-16 (decided) | `/metrics` exporter | OpenTelemetry Prometheus exporter v0.68.0 | Req 59; catalog row must also admit `client_golang`, `common/expfmt`, `otlptranslator` as direct imports |
| OQ-tech-stack-and-libraries-15 (decided) | ULID library | Own code on `crypto/rand` | Area 4's `internal/ulid` |

Non-blocking questions whose current option the code follows: OQ-observability-1 (a) 0.01; -3 (a) access log not configurable; -4 (a) honor client flags within the parent cap; -5 (a) no per-Route sampling; -6 (a) restart to change level; -7 (a) full client address; -9 (a) no trace ID header; -10 (a) limits, caps and queues fixed; -12 (a) `_overflow`; -13 open, `requestId` = trace ID only; -15 (a) `ruralz_runtime_*`; -17 answered (a); -20 current (b), loopback cleartext reported; -21 (a) ticket for `state_store`, `telemetry`, `admin` hops only.

## 8. Deferred (must NOT be built in M1) and extension points

| Deferred item | Milestone | Extension point left in M1 |
|---|---|---|
| `ruralz_plugin_*` families, `ruralz_plugin_guest_events_total` (16 names reserved per Plugin Policy, first come), SLO-GW-4, Plugins dashboard | M2 | `catalog.ClassPlugin`; `Shape` gains Plugin Policies; admission reserves fixed per-resource fan-out already |
| `ruralz_node_detached_seconds`, `ruralz_node_revocation_mark_age_seconds`, `ruralz_node_control_stream_reconnects_total`; reasons `detached`, `revision_signature_off`, `plugin_*`, `state_store_memory_multi_node`, `node_count_unknown`, `revocation_sequence_gap` | M2 | `catalog.Reason` is a dense enum with room; `Degraded.Set` unchanged |
| Ruralz Control telemetry (9902 `/metrics`, `ruralz_control_*`, R and L series, heartbeat counters, Control dashboards and alert rules), audit export (dedicated `sdklog.LoggerProvider`, retry-never-drop, OQ-observability-19) | M2 | `Service` enum (`ServiceControl`), `Options` independent of the gateway; `Runtime` has no gateway import; resource builder takes optional Cluster and Environment |
| Provider rebuild after Enrollment with Cluster and Environment resource attributes | M2 | `Runtime.rebuildProviders(resource)` internal seam; aggregates live outside providers |
| gRPC, Kafka, NATS, MQTT 5 propagation; QUIC handshake histogram; `ruralz_http_session_duration_seconds`, `ruralz_sse_buffered_total`; `onChunk` spans; access log `messages_in`/`messages_out`; per-session records | M3, M4 | `tracing.Carrier` interface (`Get`, `Set`) beside `Inject(http.Header)`; `Record` has reserved fields not encoded in M1; `StartChunk` helper declared, unused |
| AI telemetry: `gen_ai.*` attributes pinned to one commit, `ruralz_ai_*` families, access log `ai` object (1 KiB), SLO-AI-1, AI dashboard | M3 | Catalog rows added with `Milestone: "M3"`; `ratio` bounds already defined |
| `ruralz_ingress_*` | M4 | none needed |
| Authenticated OTLP export (headers), per-Route export selection, per-Upstream log records | M5 | `Settings` struct extensible; `otlp.tls` shape leaves room for `headers` |
| Live log level change (OQ-observability-6 (b)), configurable limits (OQ-observability-10 (b)), trace ID response header (OQ-observability-9) | not planned | none |
| Helm chart packaging of dashboards | M2 | JSON under `deploy/grafana/` reused by the chart |

## 9. Risks, contradictions and missing details

1. **Prometheus exporter materializes per scrape.** The OTel Prometheus exporter converts to `prometheus.Metric` and `Registry.Gather` to `dto.MetricFamily` (O(series) allocations, tens of MiB at 125,000 series), while OBS budgets "four scrape buffers add 0.25 MiB". Resolution: share one `Gather` result for up to 1 s across concurrent scrapes (req 60), measure with 6.6 test 46; if the memory or PB-10 budget fails, reopen OQ-observability-16 (c) with an ADR10 amendment.
2. **OTLP transport unspecified.** The docs never say gRPC or HTTP; every example uses port 4317. Resolution: OTLP/gRPC (req 16). grpc-go becomes a direct import in M1 (its catalog row says M3); binary size is unchanged because the HTTP exporters link the same grpc packages.
3. **`traceSampling` has no schema default.** Runtime default 0.01 (OQ-observability-1 (a)); the configuration area should add `+ruralz:default=0.01` before release 0.1.0 because materialized defaults change the canonical form and digest. CM's example uses 0.05 (OQ-observability-1 (b)); not adopted.
4. **OQ-observability-2 versus the Feature Catalog.** OQ-2 asks for TLS and authentication in M1; FC tags "Authenticated OTLP export" Planned (M5). Resolution: M1 ships TLS fields only (section 7). OBS "Scope" ("only three `Gateway.spec.telemetry` fields") needs an amendment for `otlp.tls`.
5. **Grafana dashboards are M1 in FC but absent from RM's Observability row.** Exit criterion 6 counts FC rows, so the M1 dashboards and generated alert rules are in scope (S13). `deploy/` is Planned (M2) in LAYOUT; propose `deploy/grafana/` now.
6. **`le` label rendering.** `expfmt` renders small and large bounds in `g` format (`1e-05`, `2.5e-05`, `5e-05`, `1.048576e+06`), integers as `2` in text and `2.0` in OpenMetrics, and Prometheus 3 normalizes `le` to the OpenMetrics form. OBS rules quote `le="2"` (SLO-GW-6) and, for M2, `le="0.00005"` (SLO-GW-4), which would never match. Resolution: the rule generator emits the value as the exporter renders it in OpenMetrics form and `promtool test rules` checks both scrape formats; amend OBS SLO text to name bounds by value.
7. **Shared `node.id` during a Zero-Downtime Upgrade.** Old and new processes run with the same `service.instance.id` for up to 25 s and both push cumulative OTLP metrics, which backends read as resets. Resolution (proposed): the old process performs a final metric export at Drain start and stops its `PeriodicReader`; spans and logs keep flushing. `/metrics` is fine because CBPF steers 9901 to one process.
8. **Access log format details are open.** Units of `duration` fields, the shape of `truncated`, `short_circuit` and `failure_modes`, and how stdout readers tell access lines from process lines are unspecified. Resolution: req 68 choices (seconds as JSON numbers, arrays and objects, `"msg":"access"` framing, OTLP `event.name` `ruralz.access`). CLI's OQ-cli-and-api-surface-6 (`ruralz ai cost` reading access records) depends on it.
9. **`status_class` without a status.** An attempt with `error` other than `none` has no response, and a client can abort before any status. Resolution (proposed): attempts without a response record `status_class="5xx"`; client-aborted requests record status 499 in the access log and `4xx` in counters. Needs the Observability owner's confirmation.
10. **`code` label on `result="activated"`.** No code applies; an empty value is exported (Prometheus treats it as absent, OTLP carries an empty attribute). Alternative `code="none"` would add an undocumented value.
11. **"Per-connection random source" versus HTTP/2.** Streams of one connection run on different goroutines, so a per-connection generator needs atomics or locks. Resolution: IDs from `math/rand/v2` top-level functions (runtime per-thread ChaCha8, lock-free, cryptographically seeded), or a per-connection key plus atomic counter mixed by a 64-bit finalizer; both meet "lock-free"; pick by benchmark. Stripe offsets likewise use a per-connection request sequence, because `net/http` does not expose stream IDs.
12. **Admission with a `code` dimension and across families.** OBS gives limits and order but not how code-labelled fan-out counts or which family wins when the 100,000-series Revision limit binds. Resolution: req 55 (static fan-out per Policy type, resource-major order, lazily created code series). Needs registry data from the Filters areas: the codes each type can emit.
13. **Values the docs leave open**, chosen here and marked "(proposed here)": metric export interval 15 s, export timeout 10 s, span batch 512 or 1 s, retry schedule, OTLP log queue 8,192 and 4 MiB, process log queue 4,096 and 1 MiB, span limits, `tracestate` limits, `/metrics` fifth-scrape wait 10 s then 503, collection structure abandonment after 30 s, `service.name` values, `RURALZ_LOG_LEVEL` rejection with exit 2, microsecond log timestamps.
14. **Library globals.** Replacing the OTel error handler and logger and the grpc logger mutates library globals, which LAYOUT's "no mutable package-level variables" does not cover but reviewers may question. Without it, export errors reach stderr through `log`, bypassing slog. Resolution: set once in `telemetry.New`, documented in the package comment.
15. **Environment read at package init.** Unsetting `OTEL_*` in `gateway.Run` cannot affect a dependency that reads the environment during package initialization. The ADR10 environment golden test (6.4 test 33) detects it. A grep of otel v1.46.0, sdk, sdk/metric, sdk/log v0.22.0, otlptracegrpc, the Prometheus exporter v0.68.0 and otelslog v0.20.1 finds `OTEL_*` reads only at call time (resource `Environment()`, sampler and span-limit env, `OTEL_GO_X_*` feature `Lookup`, metric exemplar filter and cardinality limit), so scrubbing before the first constructor suffices; grpc-go reads only `GRPC_*` variables at init.
16. **OTLP endpoint and the egress rules.** SEC rule 6 (OQ-security-and-identity-22) lists `jwksUrl`, `tokenUrl`, `baseUrl`, Endpoints and discovery, not the OTLP endpoint; applying it would force `RURALZ_FETCH_ALLOW` for every sidecar or agent Collector on loopback. Resolution: leave OTLP outside rule 6 unless the security area says otherwise; the dialer is injectable.
17. **Log drop accounting across two sinks.** SLO-GW-7 counts dropped over produced; a record can reach stdout and miss OTLP. Resolution: at most one drop increment per record (req 65), so the ratio stays at most 1.
18. **Access log for RZ-RT-005.** Area 4 proposes not access-logging RZ-RT-005 rejections to keep their 2-allocation budget, while OBS requires one record per request. Needs an OBS amendment or a zero-allocation minimal record path; the counter `ruralz_http_node_responses_total{code="RZ-RT-005"}` still counts them.
19. **Span attribute names.** PACK section 2 fixes span names only; the `ruralz.*` attribute keys of reqs 33 to 36 are proposals. Semconv `http.route` expects a path template, not a Route name, so the Route name goes to `ruralz.route` and `http.route` is not set.
20. **Blocked stdout at exit.** A write blocked on a full pipe cannot be interrupted (standard streams do not support deadlines), so `Shutdown` returns at its 5 s deadline, abandons the worker and the process exits; records still queued are lost and counted.
21. **PromQL parsing in CI.** No catalog row covers a PromQL parser. Resolution: pinned `promtool` binary through `make tools` (build tools stay out of `go.mod`), used by stage 3 or 8.
22. **Pre-stable Logs SDK.** `otel/log`, `sdk/log` and `otlploggrpc` are v0.22.0 and `otel/log/global` is deprecated in v1.47.0-rc.1; confined to `internal/telemetry` per ADR10, with v1.47.0 as the revisit trigger.
23. **Hot Reload of telemetry settings is implicit in the docs.** OBS covers provider rebuilds after Enrollment only; req 24 specifies swapping exporters at activation. A failing new endpoint never fails activation (exports fail and count instead).
24. **Tap event selection.** OBS says `/tap` streams "sampled" metadata without defining the sampling; area 4's hub takes a `sample` ratio per subscriber. Resolution: tap publishes only trace-sampled requests by default (req 69), which keeps its cost within the per-event budget at the trace caps; area 4 may add its own subsampling.
