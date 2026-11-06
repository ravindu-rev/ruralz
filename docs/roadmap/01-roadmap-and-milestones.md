# Roadmap and Milestones

## Summary

This document orders milestones `M1` to `M6` without dates, giving each its scope and measurable exit criteria; [Steps](02-steps.md) lists the work left in the current milestone. Readers: evaluators and contributors.

## Scope and non-goals

In scope: the [foundation pack](../_meta/foundation-pack.md) section 2 milestones not yet exited, `M1` to `M6`, and every item tagged `Planned (M1)` to `Planned (M6)`.

Non-goals: designs; moving a tag (only its owning document does); the capability-by-capability list ([Feature Catalog](../features/01-feature-catalog.md), which wins on conflict); performance values ([Performance budgets](../architecture/12-performance-budgets-and-benchmarking.md)); release numbering ([Release](../engineering/04-release-versioning-and-compatibility.md)); calendar dates (OQ-roadmap-and-milestones-2).

Codes by owner:

| Code | Owner |
|---|---|
| SM-n success metrics; P1 to P10 principles | [Vision](../vision/01-vision-and-positioning.md#success-metrics) |
| PB-n budgets; RH-1 reference machine; scenarios S1 to S8 and others | [Performance budgets](../architecture/12-performance-budgets-and-benchmarking.md#scenarios) |
| CE-n chaos experiments | [Scalability](../architecture/11-scalability-and-distributed-state.md#chaos-experiments) |
| GD-n game days | [High availability](../operations/04-high-availability-and-disaster-recovery.md#game-days) |
| TB-n trust boundaries | [System overview](../architecture/01-system-overview.md#trust-boundaries) |
| Topology T1 to T10 | [Deployment topologies](../operations/01-deployment-topologies.md#topology-catalog) |
| Threat T13, T20 | [Security and identity](../architecture/08-security-and-identity.md#threat-model-and-trust-boundaries) |
| Objective O8 | [Observability](../architecture/10-observability.md#observability-principles) |
| Gate G1 to G3 | [Tech stack](../engineering/01-tech-stack-and-libraries.md#selection-criteria) |

## M1 Core gateway

Pack name: **M1** Core gateway. M1 ships a file-mode Ruralz Gateway with core Policies, Hot Reload and release `0.1.0`. Serves: API team lead evaluating a gateway, Security engineer.

### M1 scope

| Area | Planned (M1) items |
|---|---|
| Ruralz Gateway | `ruralzd` Nodes; client listeners: HTTP/1.1 and h2c on 8080 TCP, HTTP/2 over TLS on 8443 TCP, HTTP/2 settings, deadlines, other servers (admin); REST, and plain HTTP for any other content (Multi-protocol "Anything else"); Router match and header handling; Filter Chain executor; Upstream layer; error format; load shedding; admin 9901 (TB-9): `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`, `/debug/snapshots`, `/debug/upstreams`, `/config/dump`, `/tap` |
| Configuration | `Gateway`, `Route`, `Upstream`, `Policy`, `Consumer`, and `Environment` for CLI renders; YAML 1.2 parser, JSON Schema validator; Bundle merge; restricted profile; env substitution, overlay merge; CLI verbs without `--env` when no Environment exists; `secretRef` `env` and `file`; canonicalization, Revision digest; golden corpus; Go test that meta-validates both schema files and `examples/`; JSON subset fixtures; loader conformance suite; hostile-input and upstream test suites; body buffering |
| CEL | Expressions: CEL module, compile and cost estimator in all CEL places: `Route.spec.match.when`, `Policy.spec.when`, `Route.spec.composition.steps[].pathExpression`, `Route.spec.composition.steps[].when`, `Upstream.spec.retries.retryOn`, `Upstream.spec.circuitBreaker.failureWhen`, `Upstream.spec.loadBalancing.hashKey`, `Gateway.spec.telemetry.accessLog.when`, `authz.cel` `config.rule`, `ratelimit`, `quota` and `cache` `config.key`, `ratelimit` and `quota` `config.cost`, `ratelimit` `config.limits[].when`, `headers` `valueExpression`, `transform.request` and `transform.response` `config.body` and `config.set[].valueExpression` |
| CLI | `ruralz bundle` verbs `validate`, `render` (`--api-version` conversion MUST yield the same Revision), `diff` from directory, file or admin URL, `build`; `ruralz dev` verbs `run`, `tap`; `ruralz node` verbs `drain`, `dump`; `ruralz version`, `ruralz completion`; `--env` and `--environments FILE` (local `Environment` files); CLI framework; declarative config and GitOps CLI |
| Lifecycle | File mode: topology T2 watching a rendered directory; Hot Reload, Last-Known-Good, Zero-Downtime Upgrade and Drain; topologies T1 development single binary, T5 VM with systemd in file mode |
| Security | `auth.jwt` (JWT, OpenID Connect, OAuth2; JOSE library), `auth.api-key` (removal in file mode), `auth.basic`, `auth.mtls`, `authz.cel` (Security Policies Engine, first part), `authz.ip`, `auth.upstream-oauth2` (client credentials), `cors`; TLS to IdP JWKS endpoint (TB-10), Upstreams, State Store, OTLP (TB-3, TB-4, TB-12; `AIProvider` from M3); digest checks on every Revision and Plugin artifact; redaction unit tests; secret leak tests |
| Traffic and state | `ratelimit` with local token bucket and GCRA ([ADR-0008](../adr/0008-rate-limiting-local-bucket-and-gcra.md)), distributed rate limiting with State Store-backed counters, `quota` in `requests`; API governance (`quota`, `overridable: false` Gateway Policies); `cache` (Response Cache), retries, circuit breaker, health checks, catch-all fallback, header and query string routing, wildcard routes, service discovery (`dns`, static `endpoints`) for `http` Upstreams, weighted splits, composition; `memory` and `redis` drivers (tested on Valkey 9.0.1 or newer, Dragonfly), single-Region Cells; `headers`, `transform.request`, `transform.response`, `validation.json-schema` |
| Observability | OpenTelemetry telemetry: traces, export, conventions, resource; traceparent injection with its sampling decision; metrics exposition (`/metrics`) including `ruralz_listener_tls_handshake_duration_seconds`; logs; catalog gates; cardinality test; ULID `node.id`; `ruralz_node_degraded_info` with M1 reasons such as `lkg_boot` and `state_store_breaker_open` |
| Quality | Property tests; protocol conformance suite in `pr-full` (HTTP/1.1, HTTP/2); integration tests; round-trip test; fuzzing; Docker Compose end-to-end test in file mode (quickstart, `--effective --route`, `ruralz bundle diff` against Node `/config/dump`, Zero-Downtime Upgrade); air-gapped start; chaos experiments CE-1 to CE-6, CE-12, CE-15, CE-16; CI stages 8. integration and conformance, 9. regression gates, 10. end-to-end, 11. nightly, 12. release |
| Budgets | PB-6 and PB-8; scenarios S1 plain proxying, S2 reference, S3, S5, S5x, O1, O2, C1, C2, R1, G1, with the benchmark harness, the scenario smoke run and the functional variants; microbenchmarks; component and CEL benchmarks; alloc/op, size and idle RSS gates; binary and CI size check. Their runs and budget gates on reference hardware are M6's |
| Release | Release `0.1.0`: binaries, container images, checksums, SBOM, signatures, provenance, notes, release audit per tag; `cmd/ruralzd`, `cmd/ruralz`, `test/`, `examples/` |

### M1 exit criteria

1. Release `0.1.0` ships `ruralzd` and `ruralz` for every production platform, with 100% of artifacts signed (target), per SM-12.
2. The scripted quickstart reaches a first proxied request in 10 minutes or less (target), per SM-7.
3. The golden corpus is byte-exact; conformance passes 100% of cases for shipped features (target); no fuzz crasher is open; every M1 command in pack section 9 has an end-to-end test; CE-1 to CE-6, CE-12, CE-15, CE-16 and GD-1 to GD-5 pass at the `nightly` chaos profile, whose `target` runs are M6's.
4. The Feature Catalog gives 100% of rows a milestone or a reasoned `Not planned` (target), per SM-2, and 100% of rows tagged Planned (M1) are implemented (target), per SM-3.
5. Every Open question blocking an M1 feature is closed: every row whose Blocking column names M1.

Release `0.1.0` completes the exit: its first candidate, `v0.1.0-rc.1`, is tagged only once criteria 2 to 5 hold and `t5-systemd.yml` has passed ([Release runbook](../engineering/04-release-versioning-and-compatibility.md#release-runbook)).

## M2 WASM and Control with GitOps

Pack name: **M2** WASM + Control/GitOps. M2 delivers differentiators (1) and (3), Kubernetes packaging and Bundle tooling commands. Serves: Plugin author, Platform engineer running many Clusters.

### M2 scope

| Area | Planned (M2) items |
|---|---|
| Ruralz Control | `cmd/ruralz-control` replicas; Control Store on Raft ([ADR-0006](../adr/0006-control-store-raft-boltdb.md), proposed): interface, consensus, snapshots, transport, content store, membership, telemetry; Rollouts with canary, gates (OQ-system-overview-9), `autoRollback`, promotions, skew checks; Drift; REST API with its `/api/v1/` OpenAPI description in `api/openapi/` (OQ-cli-and-api-surface-3); RBAC; audit log; published Node count; OTLP export like a Node; OQ-release-versioning-and-compatibility-4, -11, -12 |
| Control Stream | Control mode, Nodes enrolled with Ruralz Control ([ADR-0007](../adr/0007-control-stream-protocol.md)): service, delivery, ACK/NACK, flow control, reconnect, heartbeat, trust, evolution; `Enroll`; `Stream` with renewal; peer layer; forge webhook; TCP 8090 (TB-6), 8091 (TB-5), 8092 (TB-11); `ruralz-control` admin 9902: `/healthz`, `/readyz`, `/metrics`, `/debug/pprof/`; protobuf runtime and code generation; `buf lint`, `buf breaking` |
| Ruralz Console | Control plane with web console at `/console`, built from `console/` in CI stage 7: local accounts, TOTP, roles |
| WASM Plugins | `Plugin` kind and `plugin` Policy; WASM runtime wazero; Plugin ABI v1 and PDK conventions ([ADR-0005](../adr/0005-plugin-abi-v1.md)); Capabilities, Host Functions, limits; Plugin ABI conformance suite; Rust and Go (TinyGo) PDKs in `sdk/`; `api/proto/ruralz/plugin/v1/`, derived output (OQ-repository-layout-and-conventions-2); `rzplg:` State Store keys |
| CLI | `ruralz bundle` verbs `push`, `import openapi`, `export openapi`, `export postman`, `export dot`, `audit`; `ruralz test run`; `ruralz rollout` verbs `start`, `status`, `pause`, `resume`, `rollback`, `approve`, `reject`; `ruralz node` verbs `list`, `token`, `revoke`; `ruralz control` verbs `serve`, `join`, `backup`, `restore`; `ruralz plugin` verbs `init`, `build`, `test`, `push`, `inspect`; `--env` without `--environments FILE` fetches from Ruralz Control; `rev-<12 hex>` and `sha256:<64 hex>` Revision sources; OQ-cli-and-api-surface-7 |
| Signing and file mode | Topology T2 with OCI pull by `oci://REPOSITORY@sha256:<64 hex>`; signed Plugin artifact; signed Revision in Control mode ([ADR-0017](../adr/0017-artifact-signing.md)); OCI registry integration tests and interoperability job |
| Security | `authz.opa`, `authz.cedar` (authorization engines, engine configuration, engine memory; no Lua), `authz.geoip` refusing clients by country (MaxMind-format reader), `auth.upstream-sigv4`, `secretRef` `kubernetes` and `vault`, JWT signing, GCP authentication; signed revocation list and bloom filter: JWT by jti, subject's older tokens, IdP key, API key hash or `auth.basic` username, client certificate by issuer and serial; also revoked: API key in Control mode, Enrollment identity, operator session or token, Plugin publisher |
| Traffic | Built-in client redirects (OQ-traffic-management-and-resilience-13); `Route` mirroring (OQ-traffic-management-and-resilience-12); `kubernetes` discovery |
| Kubernetes | Topology T4 Kubernetes (Helm, HPA, CRDs): CRD mirror in `ruralz.io/v1alpha1`, CRDs serve every served version ([ADR-0016](../adr/0016-kubernetes-helm-and-crds.md), proposed); `Cluster`; Ruralz Control, including the CRD path (OQ-system-overview-8); `x-ruralz-validations[].rule`; Helm chart in `deploy/` (OQ-deployment-topologies-1); only Ruralz Control watches CRDs (OQ-configuration-model-2, option (a)); no Gateway API conformance (deferred) |
| Operations | Topologies T3 Cluster with Ruralz Control, T5 in Control mode, T6 edge, T7 per-team, T8 multi-cluster, T10 hybrid; degraded states `detached`, `plugin_pool_degraded`, `plugin_parked_bound_low`, `revision_signature_off`, `plugin_signature_off`, `revocation_sequence_gap`, `state_store_memory_multi_node`, `node_count_unknown` |
| Observability | Metrics `ruralz_plugin_*`, `ruralz_node_detached_seconds`, `ruralz_node_revocation_mark_age_seconds` and `ruralz_node_control_stream_reconnects_total` |
| Quality | Container-free integration tests; fencing test; end-to-end canary NACK and Ruralz Control outage cases; chaos tests of exit criterion 4 plus leader failover with every Node reconnecting; CE-7 to CE-10; fuzzing of Control Stream decoding and Plugin Host Functions; SM-6 microbenchmark gate; S4 one header-only Plugin; F1, F2; `authz.opa` and `authz.cedar` costs; reload with Plugins; control-plane scale and accounting; non-Go license gate |

### M2 exit criteria

1. A 100-Node `all-at-once` Rollout converges in 30 s or less at p95 (target), per SM-9 and PB-13; F2 emulates 10,000 Nodes (target) on three replicas and survives one replica stopping.
2. A pooled Plugin Phase call adds 50 µs or less at p99 (target), per SM-6 and PB-5.
3. `ruralz bundle import openapi` imports 90% or more of the SM-8 OpenAPI corpus without manual edits (target).
4. Chaos tests pass 100% of the System overview failure rows whose features are Planned (M2) or earlier (target): Ruralz Control replicas killed, Raft quorum lost; Node restarted during that outage; Revision with a bad digest or signature; State Store slower than `stateStoreTimeout`, then stopped; Upstream Endpoints reset or slowed; Plugin traps, loops or reaches `limits.maxPluginMemoryBytes`. CE-7 to CE-10, GD-6 to GD-11, GD-13, GD-15 and GD-16 pass. A Zero-Downtime Upgrade from the previous release fails no requests on TCP listeners (target).
5. OpenSSF Scorecard is 8.0 or higher (hypothesis), per SM-12; no Plugin sandbox escape stays unpatched past 30 days (target), per SM-13.
6. Every Open question blocking an M2 feature is closed: every row whose Blocking column names M2, plus OQ-tech-stack-and-libraries-24 (`authz.geoip`), OQ-security-and-identity-10 (`vault`) and -28 (`auth.upstream-sigv4`), OQ-traffic-management-and-resilience-12 (mirroring), among them OQ-control-plane-and-gitops-25, OQ-security-and-identity-4 (revocation list), OQ-traffic-management-and-resilience-13 (redirects) and OQ-observability-19.
7. 100% of Feature Catalog rows tagged Planned (M2) are implemented (target), per SM-3.

## M3 AI gateway, gRPC, GraphQL, WebSocket and SSE

Pack name: **M3** AI gateway + gRPC/GraphQL/WS/SSE + HTTP/3. M3 delivers differentiator (2) and most of (4); it is the largest milestone (risk R-1). Serves: AI platform engineer.

### M3 scope

| Area | Planned (M3) items |
|---|---|
| AI/LLM gateway | `AIProvider`, `AIModel`, `protocol: ai`; OpenAI-compatible façade (Chat Completions, embeddings, models) and native passthrough, streaming ([ADR-0014](../adr/0014-ai-api-surface.md)): surface decoder, native edits, usage, credentials; model routing, Provider Fallback and load balancing; `AIModel.spec.candidates[].when` |
| AI Policies and tools | Token accounting, Token Budgets (`ai.token-budget`), Rate Limits and Quotas, token-based limits in LLM input plus output tokens; `ai.semantic-cache`; Prompt Cache; `ai.guardrail` guardrail hooks; cost tracking and attribution with `ruralz ai cost` and `ruralz ai models`; MCP gateway and MCP Server with tools generated from existing Routes; A2A proxy as plain HTTP and SSE; `gen_ai` telemetry; data residency routing; tokenizer estimates |
| Protocols | gRPC ingress and gRPC proxying: `match.grpc` or `path` match to a `grpc` Upstream, gRPC-Web and Connect; GraphQL query and mutation, GraphQL subscription, `match.graphql`, other requests to a `graphql` Upstream, federation; `Upgrade: websocket`, direct and multiplexer; Server-Sent Events; REST and plain HTTP over HTTP/3 (HTTP/3 downstream, client UDP 8443), losing QUIC connections on upgrade (OQ-system-overview-18); the Route fixed by header match for every protocol; GraphQL module, limits and transport in `ruralzd`, `ruralz-control` and `ruralz`; TypeScript PDK in `sdk/` |
| Operations | QUIC handshakes in `ruralz_listener_tls_handshake_duration_seconds`; WebSocket session memory of 96 KiB or less (target); Kubernetes Node Service with UDP 8443; `ruralz/v1beta1`, and `ruralz-crds.yaml` changes serving `ruralz.io/v1beta1` with its conversion webhook (OQ-deployment-topologies-13); `ruralz_http_session_duration_seconds`, `ruralz_sse_buffered_total`; degraded reason `semantic_cache_unsupported`; traceparent injection into gRPC legs; threats T13 (cross-tenant cache leakage) and T20 (prompt injection) |
| Quality | AI stream parsers fuzzing, one target per dialect; AI dialect protocol conformance suite in `pr-full`; Token Budget property test; CE-13, CE-14; AI provider mock returns 429 before first byte; Hot Reload keeps open WebSocket and SSE streams on their starting snapshot; SM-10 scale job; component benchmarks; S6, S6n, S7 gRPC unary, HTTP/3 variant of S3; tokenizer benchmark |

Currency budgets are unscheduled until OQ-ai-llm-gateway-7 is decided; M3 offers token budgets plus cost reporting through `ruralz ai cost`.

### M3 exit criteria

1. Token Budget overshoot is 1% of B or less (hypothesis) in the SM-10 mock scenario.
2. Prompt Cache markers survive native passthrough in 100% of test cases (target), per SM-11.
3. Protocol conformance passes 100% of cases (target) for gRPC, GraphQL, WebSocket, SSE and HTTP/3.
4. Chaos tests pass the LLM provider throttling failure row, with CE-13, CE-14, GD-12 and AI provider mock 429 case.
5. The Open questions blocking M3 features are closed: 12 in AI/LLM gateway, 10 in Multi-protocol, OQ-tech-stack-and-libraries-21, OQ-system-overview-5 with OQ-multi-protocol-3, OQ-testing-and-quality-strategy-6 and -7, OQ-capacity-planning-1, and OQ-deployment-topologies-13 (conversion webhook port, pack 8.4 amendment).
6. 100% of Feature Catalog rows tagged Planned (M3) are implemented (target), per SM-3.

Measured and published at exit, not gating: SM-14 (hypothesis).

## M4 Event protocols and multi-region

Pack name: **M4** Event protocols + multi-region. M4 completes differentiator (4) and spreads Cells across Regions.

### M4 scope

| Area | Planned (M4) items |
|---|---|
| Event protocols | `kafka`, `nats` and `mqtt` Upstreams ([ADR-0013](../adr/0013-messaging-client-libraries.md)) with MQTT client and integration tests in `pr-full`; `Upstream.spec.messaging.key`; topic ingress (OQ-configuration-model-11) and async agents; embedded MQTT broker; event rows of the Multi-protocol milestone table; `ruralz_ingress_paused_partitions`, `ruralz_ingress_hold_expiries_total`; traceparent injection into Kafka, NATS and MQTT 5; no Kafka wire proxying; scenario S8 in the scenario smoke run |
| Multi-region | Cells across Regions; relay (TB-13): a read-only regional Ruralz Control, peer CA role relay on 8092, server CA on 8091; relay version skew; regional (default), divided and home Region Quota patterns; CE-11, CE-17; topology T9 multi-region with Cells |
| Other | Optional `postgres` Control Store; proxy-wasm adapter on the same host ([ADR-0005](../adr/0005-plugin-abi-v1.md)); C# PDK in `sdk/`; `surface: openai` Responses API; `surface: native` Bedrock `POST /model/{modelId}/invoke` and `/invoke-with-response-stream`, Claude-family bodies only (OQ-ai-llm-gateway-3); request hedging (Traffic management) |

Not planned: OQ-scalability-and-distributed-state-1 (GCRA leases; current: per request); Gateway API conformance, deferred beyond M5 by Deployment topologies (OQ-vision-and-positioning-4).

### M4 exit criteria

1. Messaging integration tests pass 100% of cases (target), and S8's scenario smoke run passes for Kafka, NATS and MQTT.
2. CE-11, CE-17 and GD-14 pass, and losing one Cell's State Store degrades only that Cell.
3. OQ-wasm-plugin-system-8 and -17, OQ-multi-protocol-7, -8, -9, -10 and -18, OQ-control-plane-and-gitops-22, OQ-deployment-topologies-17 (Region relay loss), OQ-system-overview-16 (regional Ruralz Control) and OQ-traffic-management-and-resilience-5 (M4 part: hedging) are closed.
4. 100% of Feature Catalog rows tagged Planned (M4) are implemented (target), per SM-3.

Measured and published at exit, not gating: SM-15 (hypothesis).

## M5 Enterprise hardening

Pack name: **M5** Enterprise hardening (SSO/SAML for Ruralz Console, FIPS build, monetization hooks). M5 closes the remaining Feature Catalog rows; the FIPS build keeps the same features and license (P1).

### M5 scope

| Area | Planned (M5) items |
|---|---|
| FIPS build | FIPS variant and build flavors from `GOFIPS140`; stage 4 FIPS job fails if quic-go appears; FIPS mode does not cover Wasm, so Plugin cryptography goes through `crypto.use`; OPA v1 gate G3 evidence; UDP 8443 off; the same OpenTelemetry signals (objective O8) |
| Console and product | Ruralz Console OIDC and SAML SSO (TB-6), `sso.config.changed`; monetization hooks, no full API management suite; A2A-aware gateway; no feature or edition tiers |
| Remaining catalog rows | OpenAPI server (OQ-vision-and-positioning-11), faster JSON decoding, gzip compression, JSON Schema response validation, static web server, SOAP integration, intermediary web proxy, OpenTelemetry SaaS authentication, exporter override, advanced logging: each implemented or marked Not planned |

Not planned today: produce-only Kafka proxying (M5 is option (b) of OQ-multi-protocol-11); `1.0.0` timing (OQ-release-versioning-and-compatibility-1).

### M5 exit criteria

1. The same tests pass on `GOFIPS140` artifacts except HTTP/3 cases; their benchmark budgets are M6's.
2. 100% of Feature Catalog rows not marked `Not planned` are implemented (target), per SM-3.
3. Ruralz Console signs in through OIDC and SAML in the one build, and SSO changes reach the audit log.
4. The Open questions blocking M5 features are closed: OQ-tech-stack-and-libraries-9 and every Feature Catalog Open question whose Blocking column names M5.

## M6 Reference hardware benchmarks

Pack name: **M6** Reference hardware benchmarks. M6 provisions the hardware the benchmarks measure on, RH-1 and RH-2 rented bare metal and the RH-3 cloud reproduction profile ([Reference hardware](../architecture/12-performance-budgets-and-benchmarking.md#reference-hardware)), and turns on the budget gates, the soak, the `target` chaos profile and the bench suite that run there. Until M6, the alloc/op, size and idle RSS gates, the scenario smoke run and the functional variants check performance. Serves: evaluators comparing published numbers, operators sizing Nodes.

### M6 scope

| Area | Planned (M6) items |
|---|---|
| Hardware | RH-1 with RH-1L, RH-1U and RH-1S, RH-2 and RH-3, set up with `deploy/benchhost`; the `rh-1` runner and its GitHub App; the repository variable `RH1_PROVISIONED` |
| Budgets | PB-1 to PB-4, PB-7, PB-9 to PB-12 and PB-14 to PB-16 gated on RH-1, and S4's PB-5 check; RH-2's gates; the Latency job: S1 and S2 nightly, the other scenarios of M1 to M5 in the slot rotation, component benchmarks; the macro p99, component and absolute budget gates; the 2-hour soak; the release's Latency and soak runs in the Benchmarks gate; the FIPS build under the same budgets |
| Bench suite | Reproducible runs on RH-1 compared with the Performance Budgets and the previous release: S1, S2, S5 and S6 for each release; S8 nightly; the `default.pgo` build; nightly and release records published (P10); one RH-3 reproduction run published with M6's release |
| Chaos | The `target` profile on the four RH-1 hosts in `chaos-scale.yml`: CE-3, CE-5 at 100 Nodes, CE-12, CE-15 and GD-2, latency and CPU criteria included |
| Capacity | The [Capacity planning](../operations/03-capacity-planning.md#coefficients) coefficients re-derived from the first published results |

### M6 exit criteria

1. On RH-1 in S2 at half saturation, gateway-added latency is 1 ms or less at p99 and 150 µs or less at p50 (target), per SM-4 and SM-5; S1 reaches 50,000 rps or more on 4 vCPU (target), per PB-4; a Hot Reload of 5,000 Routes takes 500 ms or less (target), per PB-7; all in the release-run records of the release that exits M6, whose Benchmarks gate no maintainer skips.
2. Gateway-added time to first token is 1 ms or less at p99 (target), per PB-12; S6 carries 1,000 streams at 100 chunks per second on 1.2 vCPU or less (target); S7 reaches 30,000 rps or more (target).
3. The bench suite publishes reproducible S1, S2 and S5 results on RH-1 for every release, each within its [Performance budgets](../architecture/12-performance-budgets-and-benchmarking.md) value (target), with regressions against the previous release flagged; S8 runs nightly for Kafka, NATS and MQTT with published records.
4. Every benchmark scenario but HTTP/3 meets the same budgets (target) on the `GOFIPS140` artifacts, except S3 and C2 TLS handshakes, reported until measured because FIPS handshakes may cost more (hypothesis), per [Performance budgets](../architecture/12-performance-budgets-and-benchmarking.md#milestone-plan).
5. CE-3, CE-5, CE-12, CE-15 and GD-2 pass at the `target` profile, their latency and CPU criteria included, and the 2-hour S2 soak meets its soak rule.
6. RH-2 runs the gating scenarios within their budgets (target), and one RH-3 run publishes with the release that exits M6.
7. 100% of Feature Catalog rows tagged Planned (M6) are implemented (target), per SM-3.

## Sequencing

Milestones exit in order; spikes and PDKs MAY start early. A slipping item blocks its milestone's exit unless its owning document moves it.

*Figure 1: relative milestone sequencing M1 to M6 with parallel tracks; axis ticks count relative effort units, not calendar dates.*

```mermaid
gantt
    title Relative sequencing of M1 to M6
    dateFormat X
    axisFormat %j
    todayMarker off
    section Milestones
    M1 Core gateway                                 :m1, 0, 8d
    M2 WASM + Control/GitOps                        :m2, after m1, 8d
    M3 AI gateway + gRPC/GraphQL/WS/SSE + HTTP/3    :m3, after m2, 9d
    M4 Event protocols + multi-region               :m4, after m3, 6d
    M5 Enterprise hardening                         :m5, after m4, 5d
    M6 Reference hardware benchmarks                :m6, after m5, 3d
    section Parallel tracks
    Plugin ABI and wazero spikes                    :t1, 0, 4d
    gRPC and GraphQL spikes only (R-1)              :t3, after m1, 4d
    FIPS G3 evidence                                :t4, after m1, 10d
    SAML library research                           :t6, after m3, 4d
```

## Risks

| ID | Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|---|
| R-1 | M3 bundles AI, four protocols and HTTP/3, and slips whole | High | High | gRPC and GraphQL spikes during M2; per-track exit (OQ-roadmap-and-milestones-3) |
| R-2 | gRPC waits for M3, so earlier evaluations needing it exclude Ruralz | High | Medium | As R-1 |
| R-3 | Go garbage collection breaks 1 ms p99 (target) | Medium | High | `GOMEMLIMIT` guidance; the macro gate on RH-1 and `default.pgo` in M6 |
| R-4 | Budgets are measured only in M6, so a design that misses one is found after M5 | Medium | High | The alloc/op, size and idle RSS gates and the functional variants from M1; the [Profiling playbook](../architecture/12-performance-budgets-and-benchmarking.md#profiling-playbook); rented bare metal in M6 ([Reference hardware](../architecture/12-performance-budgets-and-benchmarking.md#reference-hardware)) |
| R-6 | wazero has no fuel metering ([source](https://github.com/wazero/wazero/issues/422)); deadline interruption (`WithCloseOnContextDone`) costs 10 to 20x on loop-heavy guests ([source](https://github.com/wazero/wazero/issues/2466)), threatening SM-6 and PB-5 | Medium | High | Deadlines and Host Function call budgets (WASM plugin system's answer to OQ-tech-stack-and-libraries-8); S4 loop-heavy variant gates M2; revisit per [ADR-0004](../adr/0004-wasm-runtime-wazero.md) |
| R-7 | Non-OpenAI token estimates miss SM-10 | Medium | Medium | Provider-reported usage stays authoritative; calibration (OQ-ai-llm-gateway-2) |
| R-8 | Thin maintenance: paho.golang last tagged 2025-09-06 ([source](https://github.com/eclipse-paho/paho.golang)), mochi-mqtt 2025-03-01 ([source](https://github.com/mochi-mqtt/server)); cedar-go has no default-branch commit since 2026-06-01 ([source](https://github.com/cedar-policy/cedar-go)) | Medium | Medium | Internal interfaces; replace or fork if still stale |
| R-10 | Few outside contributors; no Plugin ecosystem yet | Medium | Medium | Contribution surface; PDKs and example Plugins in M2 |

## Contribution surface

Contributors sign off under the DCO.

| Milestone | Where outside contributors can help |
|---|---|
| M1 | Built-in Filters, golden and conformance cases, fuzz targets, Grafana dashboards |
| M2 | PDKs, example Plugins, SM-8 OpenAPI corpus, Ruralz Console pages, Helm chart |
| M3 | Dialect adapters, guardrail Plugins, TypeScript PDK, protocol conformance cases |
| M4 | Messaging tests, C# PDK, proxy-wasm adapter tests |
| M5 | FIPS test runs, SSO interoperability, monetization integrations, remaining Feature Catalog rows |
| M6 | Reproduction runs on RH-3, benchmark scenarios, published result reviews |

## Open questions

| ID | Question | Options | Owner | Blocking? |
|---|---|---|---|---|
| OQ-roadmap-and-milestones-2 | Should milestones carry dates, which the vision Summary places here? | (a) Relative order only (current), amending the vision; (b) target quarters; (c) dates per release only | ruralz-core | No |
| OQ-roadmap-and-milestones-3 | Should M3 exit per track (AI, gRPC and GraphQL, WebSocket and SSE, HTTP/3)? | (a) One exit (current); (b) per-track exits, amending pack section 2; (c) HTTP/3 to M4 | ruralz-core | No |
