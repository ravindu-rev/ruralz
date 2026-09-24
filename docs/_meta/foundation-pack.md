---
title: Ruralz Foundation Pack
version: v1
status: binding
last_updated: 2026-09-23
---

# Ruralz Foundation Pack

This file is **binding** for every document under `docs/`. Writers and reviewers MUST use these names, kinds, phases, Policy types, commands and facts verbatim; deviations are review-blocking. This version was frozen on 2026-09-23 after the System overview and Configuration model judge panels. Every amendment decision is logged in `docs/_meta/reviews/freeze-decisions.md`; later changes follow section 14.

## 1. Product facts

| Fact | Value |
|---|---|
| Product | **Ruralz**, an open-source API gateway written in Go |
| Positioning | "KrakenD Enterprise, but better, and fully free": every feature, including the control plane and console, is Apache-2.0 |
| Business model | Revenue only from **Ruralz Cloud** (managed control plane / hosting) and commercial support. No feature gating, no license keys, no "enterprise" build |
| Legal owner | **Revington** (https://revington.co). Copyright 2026 Revington. "Ruralz" is a trademark of Revington |
| Source | `https://github.com/ravindu-rev/ruralz` (monorepo); remote `git@github.com:ravindu-rev/ruralz.git` |
| License | Apache-2.0 for all components; DCO sign-off for contributions |
| Snapshot date | 2026-09-23 (competitor facts are as of this date) |
| Four differentiators (verbatim) | (1) **WASM plugin system**, (2) **AI/LLM gateway**, (3) **built-in control plane + GitOps**, (4) **multi-protocol native** |
| FIPS build | A build flavor (`GOFIPS140`, Planned (M5)) with the same features and license as the default build, published as separate artifacts; "no enterprise build" means no feature difference, not a single binary |
| Ruralz Cloud tenancy | One dedicated `ruralz-control` and Control Store per customer; provisioning and billing read only public interfaces (REST API, metrics, OpenTelemetry signals), so no cloud-only hook exists |

## 2. Canonical names

| Thing | Canonical name | Identifier / artifact | Never write |
|---|---|---|---|
| Product | Ruralz | — | RuralZ, RURALZ, ruralz (as product name) |
| Data plane | **Ruralz Gateway** | binary `ruralzd`; image `ghcr.io/ravindu-rev/ruralzd` | "the proxy", "gateway node" (use **Node**), "data plane binary" |
| Control plane | **Ruralz Control** | binary `ruralz-control`; image `ghcr.io/ravindu-rev/ruralz-control` | "controller", "management plane", "control server" |
| Web UI | **Ruralz Console** | served by `ruralz-control` at `/console` | "dashboard", "UI", "admin panel" |
| CLI | `ruralz` | `ruralz <noun> <verb>`; registry in section 9 | `ruralzctl`, `rz` |
| Go module | `github.com/ravindu-rev/ruralz` | `cmd/ruralzd`, `cmd/ruralz-control`, `cmd/ruralz`, `internal/`, `pkg/`, `api/`, `sdk/` | — |
| Config unit | **Bundle** (source directory rooted at `ruralz.yaml`) | `apiVersion: ruralz/v1alpha1`; mirrored CRDs use `ruralz.io/v1alpha1` | "krakend.json", "config tree", "manifest" |
| Immutable built config | **Revision** (content-addressed digest of a Bundle rendered for one Environment; section 8.1) | display `rev-<12 hex>`; wire and storage `sha256:<64 hex>` | "version", "release" (for config) |
| Configuration a Node serves now | **active Revision** (section 8.2) | the Node's current compiled snapshot | "current config", "live config" |
| Delivery of a Revision to a Cluster (Control mode only) | **Rollout** | states: `pending`, `canary`, `progressing`, `paused`, `complete`, `rolled-back`, `failed` (section 8.3) | "deploy", "push" |
| Plugin ABI | **Plugin ABI v1** | `ruralz.plugin.v1` | proxy-wasm (only when discussing the compatibility adapter) |
| CP-to-DP protocol | **Control Stream** | gRPC `ruralz.control.v1.ControlStream` over mTLS; every Node dials `ruralz-control` on 8091 | xDS, "sync protocol", "config push protocol" |
| Shared runtime state | **State Store** | drivers: `memory` (single Node), `redis` (Redis / Valkey / Dragonfly, RESP3) | "cache cluster", "Redis" as a concept name |
| Control-plane persistence | **Control Store** | Raft-replicated metadata (embedded `hashicorp/raft` with `raft-boltdb/v2` on bbolt, default; `postgres` optional, Planned (M4)) plus digest-addressed Revision content and audit segments on Ruralz Control replicas; `ruralz control backup` covers all of it | "database", "CP DB" |
| Gateway instance | **Node** | `node.id` (ULID) | "instance", "replica", "pod" (except in Kubernetes context) |
| Group of Nodes with one config | **Cluster** | control-plane kind `Cluster` | "fleet", "pool" |
| Group of Clusters | **Environment** | control-plane kind `Environment` (e.g., `dev`, `staging`, `prod`) | "stage", "tier" (tier is a Consumer concept) |
| Isolated blast-radius unit | **Cell** | one Cluster and its State Store in one Region, optionally with a regional Ruralz Control (section 8.13) | "shard", "partition" |
| Persisted boot configuration | **Last-Known-Good** | `${RURALZ_DATA_DIR}/lkg/` (section 8.2) | "fallback config", "cached config" |
| Default ports | `ruralzd`: **8080** HTTP, **8443** TLS (+ UDP 8443 for HTTP/3, Planned (M3)), **9901** admin (`/healthz`, `/readyz`, `/metrics`, `/debug/*`, `/config/dump`, `/tap`). `ruralz-control`: **8090** REST API + Console, **8091** Control Stream (mTLS, Nodes dial), **8092** Raft peer transport (mTLS), **9902** admin (`/healthz`, `/readyz`, `/metrics`, `/debug/*`). Details in section 8.4 | — | — |
| Env vars | `RURALZ_*`. Process settings that never change a Revision: `RURALZ_CONFIG`, `RURALZ_DATA_DIR`, `RURALZ_LOG_LEVEL`. `RURALZ_STATE_STORE_URL` is the fallback source for Gateway `spec.stateStore.url` when `stateStore` is absent (else `memory` with a startup warning) and the conventional `secretRef` name with `provider: env` | — | — |
| Metrics | `ruralz_<component>_<name>_<unit>` (e.g., `ruralz_http_request_duration_seconds`; counters end in `_total`); OTel semantic conventions for HTTP and `gen_ai.*` | — | — |
| Trace spans | `ruralz.filter.<name>`, `ruralz.upstream.<name>`, `ruralz.route.match` | — | — |
| Error codes | `RZ-<AREA>-<NNN>`; areas `CFG`, `RT`, `UP`, `AUTH`, `RL`, `PLG`, `AI`, `CP`, `STS`, with meanings and registry owners in section 8.6 | — | — |
| Principles | `P1`..`P10` (defined once in `docs/vision/01-vision-and-positioning.md`) | — | — |
| ADRs | `ADR-0001`..`ADR-0017`, files `docs/adr/NNNN-<slug>.md` | — | — |
| Open questions | `OQ-<docslug>-<n>` (e.g., `OQ-data-plane-3`) | — | "TBD", "TODO" |
| Milestones | **M0** Foundations · **M1** Core parity · **M2** WASM + Control/GitOps · **M3** AI gateway + gRPC/GraphQL/WS/SSE + HTTP/3 · **M4** Event protocols + multi-region + bench suite · **M5** Long-tail parity (SSO/SAML for Console, FIPS build, monetization hooks) | tag features `Planned (Mx)`; features whose design is complete in these docs are still tagged `Planned (Mx)` until implemented | "v1", "v2", "phase 2", "GA" as a milestone |

## 3. Resource model (fixed kinds)

All configuration is a set of resources: `apiVersion: ruralz/v1alpha1`, `kind`, `metadata` (`name`, `labels`, `annotations`), `spec`. `docs/architecture/02-configuration-model.md` owns the envelope, every kind's `spec` fields and the Policy type registry. Each Policy type's `spec.config` schema is authored by its feature document and registered and published by the Configuration model, which fixes the `config` fields it uses itself; a registered `config` field counts as defined in the Configuration model for style guide section 5. No document may invent any other field; proposals go to Open questions.

**Scope:** *Bundle* means delivered in a Bundle to the Nodes of every target Cluster and covered by the Revision digest. *Control plane only* means read by Ruralz Control (and by CLI renders) and never part of a Bundle or its digest (RZ-CFG-017).

| Kind | Scope | Purpose |
|---|---|---|
| `Gateway` | Bundle | Exactly one per Bundle, declared in `ruralz.yaml` (RZ-CFG-016). Listeners (`http`, `https`; `http3` on `https`), TLS, admin, telemetry, global limits (including the Node-wide Plugin memory cap `limits.maxPluginMemoryBytes`, section 8.11), State Store connection, Gateway-scoped Policies |
| `Route` | Bundle | Match (hosts, path, methods, headers, gRPC service/method, GraphQL operation, topic, CEL `when`) → attached Policies → one or more Upstreams, directly or through `composition` (`aggregate`, `sequential`, `conditional`). `topic` matching is Planned (M4); its ingress declaration is OQ-configuration-model-11 |
| `Upstream` | Bundle | Target service: Endpoints or discovery (`dns`, `kubernetes`), protocol (`http`, `grpc`, `graphql`, `websocket`, `kafka`, `nats`, `mqtt`, `ai`), load balancing, health checks, retries, circuit breaker, TLS, upstream-leg Policies |
| `Policy` | Bundle | Named, reusable configuration of one registered `type` (section 10) for a Filter or a Plugin. Attachable to `Gateway`, `Route`, `Upstream`. Precedence is per slot (section 8.12): a Route Policy replaces a Gateway Policy in the same slot unless that Gateway Policy sets `overridable: false`; Policies in different slots stack; Upstream-scoped Policies apply to the upstream leg only |
| `Plugin` | Bundle | WASM Plugin: OCI reference with required digest, `abi: ruralz.plugin.v1`, Phases it implements, requested Capabilities, config schema, resource limits |
| `Consumer` | Bundle | Caller identity: API keys (stored hashed; a `secretRef`-held key is hashed by the Node at load and only the hash is kept in memory), JWT subject/claim binding, OAuth client; tier, quotas, tags |
| `AIProvider` | Bundle | LLM provider: credentials (`secretRef`), base URL, dialect (`openai`, `anthropic`, `gemini`, `bedrock`, `mistral`, `ollama`), pricing table, region |
| `AIModel` | Bundle | Virtual model name → ordered candidates (provider + model) with strategy (`weighted`, `latency`, `cost`, `fallback`), token limits, cache settings |
| `Environment` | Control plane only (the CLI also reads it from `--environments` files) | Groups Clusters; selects `overlays/<env>/` and non-secret variables; promotion path for source commits (each Environment renders its own Revision) |
| `Cluster` | Control plane only | Rollout target: enrolled Nodes in one Environment sharing one Revision; `spec.rollout` (`all-at-once` or `canary`, `autoRollback`) |

**Vocabulary:** a **Policy** is configuration; a **Filter** (built-in, Go) or a **Plugin** (WASM) is the implementation that a Policy configures.

## 4. Filter Chain phases (fixed)

`onRequestHeaders → onRequestBody → onRoute → onUpstreamRequest → onUpstreamResponseHeaders → onUpstreamResponseBody → onResponse → onLog`

plus the streaming hook **`onChunk`**, invoked per SSE event, WebSocket message, or LLM token chunk in either direction.

- The header match fixes the Route before `onRequestHeaders`; body-dependent criteria choose only among its Upstream legs or `AIModel` candidates in `onRoute`.
- Filters and Plugins MAY short-circuit (produce a response) in any request Phase, `onRequestHeaders` through `onUpstreamRequest`, skipping to `onResponse` and `onLog`.
- After the response is committed, a failure in `onChunk` either ends the stream (`failureMode: closed`: SSE `error` event, WebSocket close 1011 or HTTP/2 `RST_STREAM`) or passes the chunk (`open`).
- `onLog` is read-only: it cannot modify the response, and a failure there is recorded in telemetry only.
- `onChunk` never calls the State Store (section 8.7).

## 5. Configuration format (ADR-0003)

- **YAML 1.2** with a published **JSON Schema (draft 2020-12)** in two generated views (authoring, rendered); JSON is accepted as a strict subset, through the same loader, yielding the same Revision digest. Kubernetes-style resource model (`apiVersion`/`kind`/`metadata`/`spec`).
- Restricted YAML profile: the loader rejects non-UTF-8 input and byte order marks (RZ-CFG-001), duplicate keys (RZ-CFG-002), anchors, aliases and merge keys (RZ-CFG-003) and custom tags (RZ-CFG-004); `yes` and `on` are strings.
- Env substitution: `${VAR}` and `${VAR:-default}`; `$${` escapes a literal. It runs once, after overlay merge and before schema validation, on parsed scalars, and is forbidden in keys, `apiVersion`, `kind`, `metadata.name`, references, `SecretValue` and CEL fields (RZ-CFG-011). Secrets ONLY via `secretRef` (providers `env`, `file`, `kubernetes`, `vault`), never inline.
- A Bundle is a directory rooted at `ruralz.yaml`. Base files form a union read in lexical byte order of their relative paths; a duplicate identity `(kind, metadata.name)` is RZ-CFG-008. Only `overlays/<env>/` patches resources, with Kubernetes strategic-merge semantics driven by the schema's `x-ruralz-list` types (`map`, `orderedMap`, `set`, `atomic`); at most one overlay applies per render.
- Rejected: HCL (weak schema/IDE story, no CRD path), JSON-only (no comments, poor diffs).
- YAML keys are `camelCase` (the overlay directive `$patch` is the only exception); kind names are `PascalCase`.

## 6. Architecture summary (quote verbatim where a summary is needed)

> Ruralz is an Apache-2.0 API gateway written in Go with two runtime components and one CLI. **Ruralz Gateway (`ruralzd`)** is the stateless data plane: it terminates HTTP/1.1, HTTP/2, HTTP/3, gRPC, GraphQL, WebSocket, SSE and event protocols (Kafka, NATS, MQTT), matches requests to **Routes**, runs an ordered **Filter Chain** of built-in Filters and sandboxed **WASM Plugins**, and forwards to **Upstreams** (including LLM providers) with load balancing, retries and circuit breaking. All shared or durable runtime state (Rate Limits, Quotas, Token Budgets, caches, sessions) lives in an external **State Store** (`redis` for Redis, Valkey or Dragonfly; `memory` for a single Node), and each Policy makes at most one blocking State Store round trip before the response is committed, so Nodes can be added or removed without coordination. **Ruralz Control (`ruralz-control`)** is the optional control plane: it validates Bundles from Git, renders them into immutable content-addressed **Revisions**, and delivers **Rollouts** to one or many **Clusters** over an mTLS gRPC **Control Stream** that each Node dials, with ACK/NACK semantics; it hosts the **Ruralz Console**, RBAC, audit log and Drift detection, and keeps its operational state in a Raft-replicated **Control Store**. Nodes keep serving their active Revision while Ruralz Control is unavailable, boot **Last-Known-Good** configuration after a restart, and can run without Ruralz Control by watching a Bundle directory or pulling a Revision from an OCI registry. Configuration is YAML (JSON accepted) using a Kubernetes-style resource model (`apiVersion: ruralz/v1alpha1`, `kind`, `metadata`, `spec`) validated by a published JSON Schema. The **`ruralz` CLI** validates, diffs, renders, imports, exports and tests Bundles, drives Rollouts, and builds, tests and publishes Plugins. Every feature, including Ruralz Control and Ruralz Console, is free and open source; revenue comes only from a managed cloud and support.

## 7. Technology decisions (fixed; details in `docs/engineering/01-tech-stack-and-libraries.md` and ADRs)

Rows marked **Corrected at freeze** were proven wrong in the previous pack by the tech stack document and are fixed here. Rows marked **Selected at freeze** record a library choice that `docs/_meta/research/tooling-and-licenses.md` resolved after the reviews; the tech stack document adds their catalog rows at conformance.

| Concern | Decision | ADR |
|---|---|---|
| Language / build | Go 1.26 floor (`go 1.26.0`), `CGO_ENABLED=0`, static binaries. Release and FIPS builds pin the newest Go release through the `toolchain` directive (go1.27.1 at the snapshot) and set no `GOEXPERIMENT`; floor builds on Go 1.26 need `GOEXPERIMENT=jsonv2` for jwx. FIPS variant via `GOFIPS140`. **Corrected at freeze** | ADR-0001 |
| License | Apache-2.0 everything; DCO; Revington trademark policy | ADR-0002 |
| Config | YAML 1.2 + JSON Schema, restricted YAML profile, Kubernetes-style resources, strategic-merge overlays | ADR-0003 |
| WASM runtime | wazero: repository `github.com/wazero/wazero`, Go module path `github.com/tetratelabs/wazero` (pure Go). **Corrected at freeze** | ADR-0004 |
| Plugin ABI | Custom capability-based Plugin ABI v1 with Extism-style memory/Host Function conventions; proxy-wasm compatibility adapter `Planned (M4)` | ADR-0005 |
| Control Store | Embedded `hashicorp/raft` with `github.com/hashicorp/raft-boltdb/v2` (v2.4.2 or newer) on `go.etcd.io/bbolt`; mutual-TLS peer transport on 8092; MPL-2.0 modules only as named exceptions. Research confirms `hashicorp/raft`, `raft-boltdb/v2` and, linked through `go-metrics`, `go-immutable-radix` and `golang-lru` as MPL-2.0, and `bbolt`, `go-msgpack/v2`, `go-hclog` and `go-metrics` as MIT (`docs/_meta/research/tooling-and-licenses.md` section 3); `postgres` optional behind an interface, Planned (M4). **Corrected at freeze** | ADR-0006 (proposed) |
| Control Stream | Own gRPC snapshot+delta protocol with xDS-style ACK/NACK, built with `connectrpc.com/connect` in gRPC mode on both ends; protos gated by `buf breaking`; NOT xDS, NOT go-control-plane | ADR-0007 |
| Rate limiting | Local token bucket per Node at a per-Node ceiling + GCRA in the State Store (single Lua EVAL); fail-open by default with over-admission bounded by N × per-Node ceiling, configurable per Policy (section 8.8) | ADR-0008 |
| HTTP stack | `net/http` (HTTP/1.1, HTTP/2, h2c via `Server.Protocols`) + `quic-go` for HTTP/3 (Planned (M3)); `golang.org/x/net/http2` only for non-deprecated low-level APIs, because its `Server` and `Transport` are deprecated in x/net v0.59.0; no fasthttp. **Corrected at freeze** | ADR-0009 |
| Telemetry | OpenTelemetry-first (traces, metrics); `slog` bridge (`otelslog`) for logs until the OTel Go Logs API is stable | ADR-0010 |
| Expressions / authz | CEL via cel-go, import path `cel.dev/cel-go` (`google/cel-go` is a read-only alias), inline; OPA (`opa/v1/rego`) and Cedar (`cedar-go`) as pluggable authz engines; no Lua. **Corrected at freeze** | ADR-0011 |
| GraphQL | `github.com/wundergraph/graphql-go-tools/v2` engine (federation versions 1 and 2, subscriptions) | ADR-0012 |
| Messaging | `twmb/franz-go` (Kafka), `nats.go/jetstream`, `eclipse-paho/paho.golang` (MQTT 5 client) + `mochi-mqtt` embedded broker mode `Planned (M4)` | ADR-0013 |
| AI surface | OpenAI-compatible façade **and** native passthrough (Anthropic Messages, Bedrock, Gemini); provider-reported usage is authoritative for billing; tokenizer counts only for pre-admission estimates and the per-request streaming guard; Token Budget reservation and settlement per section 8.9 | ADR-0014 |
| Upgrades | `SO_REUSEPORT` + graceful Drain + readiness gating (the new process reports ready over a Unix socket under `${RURALZ_DATA_DIR}`); HTTP/2 GOAWAY; `SO_ATTACH_REUSEPORT_CBPF` steering through `golang.org/x/sys/unix` empties the closing listener's accept queue (`net.ipv4.tcp_migrate_req` is an alternative); in-flight QUIC connections on UDP 8443 are lost and reconnect; no listener socket passing between processes | ADR-0015 |
| Kubernetes | Helm chart + CRDs mirroring the kinds `Planned (M2)`, API group `ruralz.io` (`ruralz.io/v1alpha1`, translated to the Bundle `ruralz/v1alpha1` by changing only the group); only Ruralz Control watches CRDs; Gateway API conformance deferred | ADR-0016 (proposed) |
| Artifact signing | Digest verification always; Ruralz Control signs Revisions; CI and Plugin publishers sign with Sigstore; Nodes verify by default (section 8.14) | ADR-0017 |
| gRPC ingress / upstream | `connectrpc.com/connect` (gRPC, gRPC-Web, Connect on `net/http`); `grpc-go` for upstream clients | (tech stack doc) |
| Redis client | `github.com/redis/rueidis`; client-side caching off by default | (tech stack doc) |
| JOSE / OIDC | `github.com/lestrrat-go/jwx/v4` + `github.com/coreos/go-oidc/v3` | (tech stack doc) |
| Revision and Plugin distribution | `oras.land/oras-go/v2` (v2.6.2 or newer) and `sigstore/sigstore-go` | (tech stack doc; ADR-0017) |
| Tokenizer | `tiktoken-go/tokenizer`, estimates only | (tech stack doc; ADR-0014) |
| Semantic cache | Redis 8 Vector Sets or valkey-search 1.2 on the same State Store deployment through dedicated connections, never the auto-pipelined rate-limit and quota connections. Servers without either (Dragonfly, Valkey below 9.0.1) cannot serve it: the Policy then behaves as a State Store failure under its `failureMode` (default `open`, cache bypassed) and the Node reports a degraded state. **Corrected at freeze** | (AI doc) |
| YAML 1.2 parser | `github.com/goccy/go-yaml` (MIT): the only researched candidate that claims YAML 1.2-only scalar resolution and rejects duplicate keys by default; the restricted profile's rejection of anchors, aliases, merge keys and custom tags is Ruralz code over its AST (`docs/_meta/research/tooling-and-licenses.md` section 6). **Selected at freeze** | (tech stack doc; ADR-0003) |
| JSON Schema validator | `github.com/santhosh-tekuri/jsonschema/v6` (Apache-2.0): claims draft 2020-12 test-suite compliance, offers a `Vocabulary` API for the `x-ruralz-*` keywords and instance locations for the source map, and builds on the Go 1.26 floor (`docs/_meta/research/tooling-and-licenses.md` section 7). **Selected at freeze** | (tech stack doc; ADR-0003) |
| Pending selections | Protobuf runtime, ULID, `/metrics` exporter, SigV4 signer, CLI framework, SAML, `postgres` driver (OQ-tech-stack-and-libraries-14 to -20; candidate licenses in `docs/_meta/research/tooling-and-licenses.md` section 8) and a MaxMind database reader for `authz.geoip` (not yet researched). A design MAY assume the capability but MUST NOT name a library until the tech stack document adds a catalog row backed by research | (tech stack doc) |

Other defaults: State Store drivers are `memory` and `redis` only; another driver needs an ADR. Console is a React/TypeScript SPA embedded in `ruralz-control`. No native Kafka/MQTT wire-protocol proxying before M4. KrakenD import (`ruralz bundle import krakend`) is best-effort with declared fidelity levels (`exact`, `equivalent`, `approximate`, `manual`), `Planned (M2)`. The managed cloud is described only in the vision document's Managed cloud section and one hybrid deployment topology; docs MUST NOT describe cloud-only hooks.

## 8. Definitions and system rules

### 8.1 Bundle and Revision

| Term | Definition |
|---|---|
| Bundle | The source: a directory rooted at `ruralz.yaml` holding `Gateway`, `Route`, `Upstream`, `Policy`, `Plugin`, `Consumer`, `AIProvider` and `AIModel` resources plus optional `overlays/<env>/`. One Bundle renders into one Revision per Environment |
| Rendered Bundle | Output of `ruralz bundle render --env <env>`: one YAML stream with the overlay merged and `${VAR}` substituted; itself a valid one-file Bundle, and the form file-mode Nodes SHOULD load when more than one Node reads it |
| Revision | The SHA-256 digest of one Bundle rendered for one Environment, serialized as `ruralz.canonical.v1`: each resource decoded under its own apiVersion, every schema and registry default (`slot`, `failureMode`, `filterClass`) materialized, hub form normalized and sorted, then RFC 8785 canonical JSON. Equal digests mean equal effective behavior on every Node that accepts them; only `secretRef` values and `RURALZ_*` process settings differ between Nodes |
| Identifier | Display form `rev-<12 hex>`, the first 12 hex characters of the digest (e.g., `rev-162af81f5de4`), in CLI output, Ruralz Console, logs and prose. The Control Stream, OCI pulls, Last-Known-Good and every verification carry and check the full `sha256:<64 hex>`; a mismatch is RZ-CFG-027 |
| Producers | Ruralz Control builds Revisions from Git, or from a source Bundle and expected digest sent by `ruralz bundle push`, which it re-renders with the Environment's variables (mismatch: RZ-CFG-027). `ruralz bundle build` builds locally. `ruralz bundle push` to an OCI registry publishes a rendered Revision that file-mode Nodes pull by digest |
| Promotion | Moves a source commit, not a digest: `Environment.spec.promotion.from` lets a commit roll out only after its Revision for the source Environment reached `complete` |

### 8.2 Active Revision and Last-Known-Good

| Rule | Statement |
|---|---|
| active Revision | The Revision whose compiled snapshot a Node serves now. It changes only by an atomic snapshot swap after the Node verifies the digest and compiles it (Hot Reload); each request stays on its starting snapshot. A detached Node keeps its active Revision and stays ready |
| Candidate | After activation (and the ACK in Control mode, which means active, not durable) the Node writes the Revision and its canonical source under `${RURALZ_DATA_DIR}/lkg/` as a candidate |
| Last-Known-Good | The persisted Revision a Node boots when it cannot obtain configuration at startup; configuration only, never resolved secrets |
| Promotion in file mode | On activation |
| Promotion in Control mode | Level-triggered: every snapshot, delta and heartbeat carries the Cluster's promoted digest (the Revision of its last `complete` Rollout); a Node promotes its candidate to Last-Known-Good whenever its active digest equals the promoted digest, including after a late reconnect, Enrollment or scale-out. A Revision that failed its canary never becomes Last-Known-Good |
| Boot order | (1) Upgrade handover: the predecessor's active Revision from its verified candidate, no boot wait. (2) File mode: a valid Bundle, else Last-Known-Good. (3) Control mode: wait up to 5 s (target) for the Control Stream, then Last-Known-Good (detached). (4) Otherwise not ready |
| Reporting | Heartbeats report each Node's active and Last-Known-Good digests; Drift detection flags laggards |

### 8.3 Rollout states

Rollouts exist only in Control mode; file-mode change delivery (CI replacing a directory or publishing a digest) is not a Rollout.

| State | Meaning | Next |
|---|---|---|
| `pending` | Recorded with its persisted per-Node plan (canary set, batches, each Node's desired digest); queued | `canary` or `progressing` |
| `canary` | `strategy: canary` only: `canary.percent` of Nodes run the Revision for `canary.bake` while gates evaluate (signal source: OQ-system-overview-9) | `progressing`, `paused`, `rolled-back` |
| `progressing` | Delivering batch by batch; follows `pending` directly under `all-at-once` | `complete`, `paused`, `rolled-back` |
| `paused` | Delivery stopped; Nodes stay on their current Revisions. Entered by `ruralz rollout pause`, or with `autoRollback: false` by a deterministic NACK, a failed gate or the lagging threshold | `ruralz rollout resume` continues the persisted plan; `ruralz rollout rollback` gives `rolled-back` |
| `complete` | Every non-quarantined Node ACKed; the Cluster's promoted digest advances | Terminal |
| `rolled-back` | The previous Revision was re-delivered to every Node that activated the new one, after a deterministic NACK, failed gate or lagging threshold with `autoRollback: true`, or after `ruralz rollout rollback` | Terminal |
| `failed` | Only the revert itself could not finish; pages an operator | Terminal |

A deterministic NACK is an `RZ-CFG-<NNN>` error that reproduces from the Revision; it fails the gate at once. A transient NACK (fetch, resource, timeout) is retried with backoff, then the Node is marked lagging and quarantined; the Rollout continues unless more than max(1, 5% of the batch) of a batch lags (target). A reconnecting Node receives the Revision its plan assigns, so reconnect storms cannot bypass a canary. Batch sizing and gate values belong to Control plane and GitOps.

### 8.4 Control Stream, ports and HTTP/3

Every Node dials `ruralz-control:8091` (gRPC `ruralz.control.v1.ControlStream`, mTLS with its per-Node enrollment certificate); Ruralz Control never dials a Node, so Nodes work behind NAT and egress-only firewalls. Snapshots, deltas and the promoted digest flow down; ACK or NACK (digest and code) and heartbeats (active and Last-Known-Good digests, schema level per apiVersion) flow up. Reconnects use exponential backoff with full jitter and present the active digest; Ruralz Control answers with nothing, a delta or a full snapshot, and MAY shed reconnects.

| Port | Process | Purpose | Authentication |
|---|---|---|---|
| 8080 | `ruralzd` | HTTP listener | Consumer credentials per Policy |
| 8443 TCP and UDP | `ruralzd` | TLS listener; UDP carries HTTP/3 when `http3: true`, Planned (M3) | TLS or QUIC; Consumer credentials |
| 9901 | `ruralzd` | Admin: `/healthz`, `/readyz`, `/metrics`, `/debug/*`, `/config/dump`, `/tap` | Only `/healthz` and `/readyz` MAY be unauthenticated; default bind: OQ-system-overview-6 |
| 8090 | `ruralz-control` | REST API and Ruralz Console | Sessions or tokens, RBAC, audit log |
| 8091 | `ruralz-control` | Control Stream server; Nodes dial it | mTLS, per-Node enrollment certificate |
| 8092 | `ruralz-control` | Raft peer transport (`raft.NetworkTransport` over a Ruralz mTLS stream layer); unused with `postgres` | mTLS with Ruralz Control peer certificates; Node certificates rejected |
| 9902 | `ruralz-control` | Admin: `/healthz`, `/readyz`, `/metrics`, `/debug/*` | Same rule as 9901 |

A dedicated Raft port, not ALPN multiplexing on 8091, keeps Nodes off peer traffic in firewall rules. HTTP/3 is Planned (M3) through quic-go, is off in FIPS builds (OQ-tech-stack-and-libraries-9), and loses in-flight QUIC connections on a binary upgrade (OQ-system-overview-18).

### 8.5 Readiness

| Endpoint | Meaning |
|---|---|
| `ruralzd` `/healthz` | The process responds |
| `ruralzd` `/readyz` | 200 only when an active validated Revision is loaded, every `secretRef` it uses has resolved, listeners are bound and the Node is not draining. It never fails because the Control Stream is lost or the Node is detached, for any duration, since that would pull every Node during a control-plane incident (an opt-in staleness limit is OQ-system-overview-13). A Node with no usable configuration stays not ready |
| `ruralz-control` `/healthz`, `/readyz` on 9902 | Semantics owned by Control plane and GitOps |

### 8.6 Error-code areas

| Area | Meaning | Registry owner |
|---|---|---|
| `CFG` | Configuration parse, validation, render, digest, secret resolution and Plugin artifact or signature checks (RZ-CFG-001 to RZ-CFG-032 today) | configuration-model |
| `RT` | Request handling on a Node before any Upstream: no matching Route, request size limits, Node buffer budget or overload, CORS and schema-validation rejections | data-plane |
| `UP` | Upstream legs: connect, TLS, timeout, reset, open breaker, retries exhausted, mid-stream failure | traffic-management-and-resilience |
| `AUTH` | Authentication and authorization decisions, including `authz.*` denials and upstream credential failures | security-and-identity |
| `RL` | Rate Limit and Quota decisions (429; 403 for a missing Consumer quota) | traffic-management-and-resilience |
| `PLG` | Plugin load, trap, limit, Capability and instance-pool failures | wasm-plugin-system |
| `AI` | Model resolution, provider errors, exhausted Provider Fallback, Token Budget and guardrail decisions | ai-llm-gateway |
| `CP` | Ruralz Control: REST API, Rollout, Enrollment, Control Stream and Control Store errors | control-plane-and-gitops |
| `STS` | A State Store call failed or timed out and the Policy applied `failureMode: closed` | scalability-and-distributed-state |

A rejection uses `STS` when a State Store call failed or timed out, `PLG` when a Plugin trapped or exceeded limits, and otherwise the area of the Policy type; a decision (such as a denial) never uses `STS`. Data plane owns the error response format.

### 8.7 State Store round trips

1. Before the response is committed, each Policy makes at most one blocking State Store round trip per request.
2. A call's timeout is the smaller of the Policy's `stateStoreTimeout` (default: Gateway `spec.stateStore.timeout`) and the time left in the per-request deadline (the Route's largest `stateStoreTimeout`). After it expires, remaining Policies apply `failureMode` without waiting; an open State client breaker skips calls entirely.
3. Consumptive calls (GCRA, Quota check, Token Budget reservation) run in chain order and stop at the first deny: one script when their keys share a hash slot, otherwise sequential round trips. Read-only calls that depend on no earlier Filter MAY share one pipelined batch.
4. Post-commit writes (Response Cache and Semantic Cache stores, Quota and Token Budget settlement) are asynchronous, pass through a bounded per-Node queue and are dropped with a counter when it is full; they never delay a response.
5. `onChunk` never calls the State Store.
6. Any other remote call before commit, such as a Semantic Cache embedding call to an `AIProvider`, is declared on the Policy with a timeout and `failureMode`.

Accuracy bounds for dropped writes belong to Scalability and distributed state.

### 8.8 Rate limiting (ADR-0008)

- Each Node runs a local token bucket per key at a **per-Node ceiling**: declared on the Policy (field authored by Traffic management and resilience), or derived by dividing the limit by the Node count Ruralz Control publishes for the Cluster; never learned from peers. With neither (file mode without a declared ceiling), the ceiling equals the full limit.
- Requests the local bucket denies never reach the State Store; each locally admitted request runs GCRA in one Lua `EVAL` in the State Store, which enforces the global limit. Leased allowances instead of per-request GCRA are OQ-system-overview-15.
- Fail-open over-admission bound: while the State Store is failing, admission per key is at most N × per-Node ceiling per window, where N is the number of serving Nodes (target; Scalability and distributed state owns the bound).
- `failureMode` defaults to `open` and is configurable per Policy. With the `memory` driver every limit multiplies by the Node count, and a Node warns when its Cluster reports more than one Node.

### 8.9 Token Budgets (ADR-0014)

| Step | Rule |
|---|---|
| Output cap C | The client's `max_tokens`, else `AIModel` `limits.maxOutputTokens`; the gateway writes C into the upstream request's output-token limit. An `AIModel` reachable from a Route with an `ai.token-budget` Policy MUST set `limits.maxOutputTokens` (a validation error the Configuration model registers) |
| Reservation R | Estimated input tokens plus C |
| Admission | In `onRequestBody`, one atomic State Store call reserves R only if the remaining budget covers all of R; otherwise the request is rejected with an `RZ-AI` code |
| Streaming guard | In `onChunk` the Node counts output tokens locally, with no State Store call, and ends the stream if the count exceeds C; local counts are never billed |
| Settlement | In `onLog`, asynchronously: charge provider-reported usage (authoritative) and release the rest of R. When usage is missing (interrupted stream, no usage reported), charge all of R and emit a degraded-state metric |
| Overshoot bound | At most the sum of input-estimate errors across concurrent requests (hypothesis; SM-10), owned by AI/LLM gateway |

The default `failureMode` is `closed`, subject to OQ-configuration-model-8; per-provider estimate accuracy is OQ-vision-and-positioning-15.

### 8.10 failureMode

- Values `open` and `closed`; the registry default of the Policy type applies unless the Policy sets one (section 10).
- `failureMode` is overridable only for non-security Policy types. Security types are closed only, and `open` is RZ-CFG-029: `auth.*` (including `auth.upstream-oauth2` and `auth.upstream-sigv4`), `authz.*`, and `plugin` Policies with `filterClass` `auth` or `authz`.
- It applies when a Filter or Plugin cannot decide: a State Store call fails or exceeds its timeout, a remote dependency such as a JWKS URL fails, a Plugin traps or exceeds limits, or one of the Policy's CEL expressions errors at runtime. For types without dependencies it governs only CEL errors.

| Failure in | `closed` | `open` |
|---|---|---|
| A request Phase, `onRequestHeaders` to `onUpstreamRequest` | 401 or 403 for auth and authz types, otherwise 503; code per section 8.6 | Skip the Policy for this request |
| A response Phase before commit | Replace the response with 502 | Skip; the response passes |
| `onChunk` after commit | End the stream (SSE `error` event, WebSocket 1011, HTTP/2 `RST_STREAM`) | Pass the chunk |
| `onLog` | Telemetry only | Telemetry only |

### 8.11 Node durable state

A Node persists only the following under `${RURALZ_DATA_DIR}`; all other shared or durable state lives in the State Store.

| State | Location | Notes |
|---|---|---|
| Enrollment identity | `${RURALZ_DATA_DIR}/identity/` | `node.id` (ULID, generated at first boot) and, in Control mode, the per-Node mTLS certificate and key issued at Enrollment; revoked with `ruralz node revoke` |
| Last-Known-Good and candidate | `${RURALZ_DATA_DIR}/lkg/` | Configuration only; encrypted secret persistence is OQ-configuration-model-15 |
| Disposable caches | Under `${RURALZ_DATA_DIR}`, layout owned by Data plane | Plugin artifacts and compiled modules; deleting them costs only warm-up |

- Only the process holding the file lock on `${RURALZ_DATA_DIR}` writes Last-Known-Good and holds the Control Stream for its `node.id` (ADR-0015 handover).
- Enrollment is one-time and off the request path; file mode needs none. In Control mode, a Node without Last-Known-Good stays not ready until it enrolls and receives its first Revision from Ruralz Control; seeding from an OCI-published Revision during an outage is OQ-system-overview-12.
- Gateway `spec.limits.maxPluginMemoryBytes` caps aggregate Plugin memory per Node; at the cap new instances are refused (`RZ-PLG` under the Policy's `failureMode`) and the Node never crashes. WASM plugin system owns the default; the Configuration model registers the field.

### 8.12 Policy precedence

Policies attach by forward reference in `spec.policies`; a Policy never names its targets. Each Route's effective Filter Chain is computed at validation time, never per request:

1. Take the Gateway's `policies` minus the Route's `excludePolicies`.
2. Add the Route's `policies`. A Route Policy in the same `slot` as a remaining Gateway Policy replaces it (Route over Gateway); other slots stack. All client authentication types share slot `auth`; additive types default to their own name as slot, so a Route `ratelimit` adds to the Gateway limit. One Policy per slot per scope (RZ-CFG-018).
3. A Gateway Policy with `overridable: false` cannot be excluded or replaced (RZ-CFG-019).
4. Upstream-scoped Policies form one set per Upstream, run only in that upstream leg's Phases and take no part in slot comparison; a type at a disallowed scope is RZ-CFG-020.
5. Within a Phase, order is Filter class (cors, auth, authz, admission, validation, cache, upstream-auth, transform, custom), then scope (Gateway, Route, Upstream), then list position; response Phases run in reverse and `onLog` keeps request order.

### 8.13 Cells, Regions and regional Ruralz Control

- A **Cell** is one Cluster plus the State Store it uses, in one Region: the blast-radius unit for shared runtime state. A State Store outage degrades only its Cell, and a Cell keeps serving while Ruralz Control is unavailable.
- No request crosses a Region to reach a State Store; each Region's Nodes resolve their own `stateStore.url` through `secretRef`, so Regions share one Revision.
- A **regional Ruralz Control** is a Ruralz Control deployment serving one Region's Clusters, Planned (M4). Whether it has its own Control Store, is a Raft non-voter or is a read-only relay is OQ-system-overview-16. Raft voters SHOULD stay in one Region, and a regional Ruralz Control is never on the request path.

### 8.14 Artifact signing (ADR-0017)

| Artifact | Signed by | Verified by | Default |
|---|---|---|---|
| Every Revision and Plugin artifact | Content addressing | Every fetch checks the full sha256 (RZ-CFG-027, RZ-CFG-028) | Always on; not configurable |
| Revision in Control mode | Ruralz Control, when it records the Revision; key rotation owned by Control plane and GitOps | Each Node before activation, against trust anchors received at Enrollment | Always verified; failure is a NACK |
| Revision published to OCI (file mode) | CI, at `ruralz bundle push`, with Sigstore (keyless OIDC or a key), stored as an OCI referrer | Each Node before activation | `enforce`; `off` must be set explicitly and is a degraded state (metric and warning) |
| Plugin artifact | Its publisher, at `ruralz plugin push`, with Sigstore | `ruralz bundle build` and `validate --online`, Ruralz Control at ingest, Nodes at activation | `enforce`; `ruralz dev run` uses `warn`; `off` is a degraded state |
| Watched Bundle directory (file mode) | Not signed | The operator's filesystem is the trust root | Not applicable |

The trust policy (trusted identities or keys) lives in Ruralz Control's configuration in Control mode, sent to Nodes over the Control Stream, and in the Node's process configuration in file mode; names belong to Security and identity. Verification is offline: trust roots are supplied locally, so air-gapped installs work. Signature failures use an `RZ-CFG` code the Configuration model registers. Release artifact signing (images, SBOM) is separate and owned by Release, versioning and compatibility.

## 9. CLI command registry

Nouns are fixed: `bundle`, `rollout`, `plugin`, `ai`, `dev`, `node`, `control`, `test`. `docs/reference/01-cli-and-api-surface.md` MUST list every command below; it MAY add verbs and flags under these nouns, and a new noun needs a pack amendment through its Open questions. `ruralz version` and `ruralz completion` are the only commands outside the noun-verb form. Flags proposed in OQ-configuration-model-10 belong to the CLI document.

| Command | Purpose | Planned |
|---|---|---|
| `ruralz bundle validate` | Offline validation with source-mapped diagnostics (`--output json`); `--online` adds the Plugin artifact check | Planned (M1) |
| `ruralz bundle render` | Render for one Environment (`--env`, `--environments`); `--effective --route` prints resolved Filter Chains; `--api-version` converts | Planned (M1) |
| `ruralz bundle diff` | Compare two rendered states (Bundle, Revision or a Node's `/config/dump`); exit 0, 1 or 2 | Planned (M1); Revision sources Planned (M2) |
| `ruralz bundle build` | Produce a Revision; runs the online Plugin check unless `--offline` | Planned (M1) |
| `ruralz bundle push` | Send a source Bundle and expected digest to Ruralz Control, or publish and sign a rendered Revision in an OCI registry | Planned (M2) |
| `ruralz bundle import krakend` | Best-effort import of a rendered KrakenD configuration with a fidelity report | Planned (M2) |
| `ruralz bundle import openapi` | Generate Routes and Upstreams from an OpenAPI 3.x document | Planned (M2) |
| `ruralz bundle export openapi` | Emit an OpenAPI 3.x document for a rendered Bundle's HTTP Routes | Planned (M2) |
| `ruralz bundle export postman` | Emit a Postman collection for the same Routes | Planned (M2) |
| `ruralz bundle export dot` | Emit a Graphviz DOT graph of Routes, Policies and Upstreams | Planned (M2) |
| `ruralz bundle audit` | Report security and best-practice findings, such as Routes without authentication or broad Capabilities | Planned (M2) |
| `ruralz test run` | End-to-end tests: declarative request and expected-response cases, kept outside the Bundle, run against a local `ruralzd` or a target URL | Planned (M2) |
| `ruralz rollout start` | Start a Rollout of a Revision to a Cluster | Planned (M2) |
| `ruralz rollout status` | Show state, batches, ACKs, NACKs and lagging Nodes | Planned (M2) |
| `ruralz rollout pause` | Enter `paused` | Planned (M2) |
| `ruralz rollout resume` | Continue a paused Rollout from its persisted plan | Planned (M2) |
| `ruralz rollout rollback` | Re-deliver the previous Revision (`rolled-back`) | Planned (M2) |
| `ruralz rollout approve` | Approve a promotion when `Environment.spec.promotion.requireApproval` is set | Planned (M2) |
| `ruralz plugin init` | Scaffold a Plugin project (plugin generator) | Planned (M2) |
| `ruralz plugin build` | Compile a Plugin to WASM | Planned (M2) |
| `ruralz plugin test` | Run Plugin tests on the same wazero host as `ruralzd` | Planned (M2) |
| `ruralz plugin push` | Publish a Plugin to an OCI registry and sign it | Planned (M2) |
| `ruralz plugin inspect` | Show ABI, Phases, Capabilities, digest and signatures | Planned (M2) |
| `ruralz ai cost` | Cost attribution from provider-reported usage and pricing tables | Planned (M3) |
| `ruralz ai models` | List `AIModel` resources and their candidates | Planned (M3) |
| `ruralz dev run` | Launch a local `ruralzd` on a Bundle directory with Hot Reload | Planned (M1) |
| `ruralz dev tap` | Stream redacted traffic from a Node's `/tap` on 9901 | Planned (M1) |
| `ruralz node list` | List Nodes with active and Last-Known-Good digests | Planned (M2) |
| `ruralz node drain` | Drain a Node | Planned (M1) |
| `ruralz node dump` | Save a Node's active configuration from `/config/dump`, secrets omitted | Planned (M1) |
| `ruralz node token` | Issue a one-time Enrollment token for a Cluster | Planned (M2) |
| `ruralz node revoke` | Revoke a Node's enrollment identity | Planned (M2) |
| `ruralz control serve` | Run a Ruralz Control replica (launches `ruralz-control`) | Planned (M2) |
| `ruralz control join` | Join a replica to the Raft cluster over 8092 | Planned (M2) |
| `ruralz control backup` | Snapshot the Control Store, including Revision content and audit segments | Planned (M2) |
| `ruralz control restore` | Restore a Control Store snapshot | Planned (M2) |

Serving an OpenAPI document from Ruralz Gateway (KrakenD's OpenAPI server) is not a CLI command and stays OQ-vision-and-positioning-11.

## 10. Policy type registry

These are the exact `Policy.spec.type` strings of the Configuration model registry; never write `rateLimit`, `rate-limit`, `apiKey`, `jwt`, `tokenBudget`, `ipfilter` or `geoip`. Scopes: G Gateway, R Route, U Upstream; slot `name` means the Policy's own name.

| `type` | Filter class | Phases | Scopes | Slot | `failureMode`: default; allowed | Planned |
|---|---|---|---|---|---|---|
| `auth.jwt`, `auth.api-key`, `auth.basic`, `auth.mtls` | auth | onRequestHeaders | G, R | `auth` | closed; closed only | Planned (M1) |
| `authz.cel` | authz | onRequestHeaders, onRequestBody | G, R | `name` | closed; closed only | Planned (M1) |
| `authz.opa`, `authz.cedar` | authz | onRequestHeaders, onRequestBody | G, R | `name` | closed; closed only | Planned (M2) |
| `authz.ip` (added at freeze) | authz | onRequestHeaders | G, R | `name` | closed; closed only | Planned (M1) |
| `authz.geoip` (added at freeze) | authz | onRequestHeaders | G, R | `name` | closed; closed only | Planned (M2) |
| `ratelimit` | admission | onRequestHeaders | G, R | `name` | open; either | Planned (M1) |
| `quota` | admission | onRequestHeaders, onLog | G, R | `name` | open; either | Planned (M1) |
| `validation.json-schema` | validation | onRequestBody | R | `validation` | closed; either | Planned (M1) |
| `cors` | cors | onRequestHeaders, onResponse | G, R | `cors` | closed; either | Planned (M1) |
| `cache` (Response Cache) | cache | onRequestHeaders, onResponse | R | `cache` | open; either | Planned (M1) |
| `headers` | transform | onRequestHeaders, onResponse; at U: onUpstreamRequest, onUpstreamResponseHeaders | G, R, U | `name` | closed; either | Planned (M1) |
| `transform.request` | transform | onRequestBody; at U: onUpstreamRequest | G, R, U | `name` | closed; either | Planned (M1) |
| `transform.response` | transform | onResponse; at U: onUpstreamResponseBody | G, R, U | `name` | closed; either | Planned (M1) |
| `auth.upstream-oauth2` | upstream-auth | onUpstreamRequest | U | `upstream-auth` | closed; closed only | Planned (M1) |
| `auth.upstream-sigv4` | upstream-auth | onUpstreamRequest | U | `upstream-auth` | closed; closed only | Planned (M2) |
| `ai.token-budget` | admission | onRequestBody, onChunk, onLog | G, R | `name` | closed; either | Planned (M3) |
| `ai.semantic-cache` (Semantic Cache) | cache | onRequestBody, onResponse | R | `semantic-cache` | open; either | Planned (M3) |
| `ai.guardrail` | validation | onRequestBody, onChunk, onResponse | G, R | `name` | closed; either | Planned (M3) |
| `plugin` | `filterClass` | The Plugin's `phases` | Scopes matching those Phases | `name` | closed; closed only for auth and authz classes | Planned (M2) |

IP filtering and GeoIP (OQ-vision-and-positioning-10) are built-in types. `authz.ip` allows or denies by CIDR on the client address; deriving that address behind trusted proxies belongs to Security and identity. `authz.geoip` allows or denies by ISO 3166 country from a local MaxMind-format database file and waits for a tech stack catalog row for the reader; header enrichment for Upstreams is an Open question of Security and identity. The Configuration model adds both rows at conformance; the `geo-block` Plugin example stays valid. The MCP Server surface stays OQ-vision-and-positioning-12, and the `quota` and `ai.token-budget` defaults stay OQ-configuration-model-8.

## 11. Glossary definitions

`docs/glossary.md` MUST use these definitions (rewording only if the meaning is unchanged) and add canonical spelling and forbidden aliases per term. "Defined in" names the owning document by manifest slug.

| Term | Definition | Defined in |
|---|---|---|
| active Revision | The Revision whose compiled snapshot a Node serves now; replaced only by an atomic swap after validation and kept while the Node is detached from Ruralz Control. | system-overview |
| AIModel | A Bundle kind naming a virtual model that clients request, mapped to ordered provider candidates with a strategy, token limits and cache settings. | ai-llm-gateway |
| AIProvider | A Bundle kind for one LLM provider account: dialect, base URL, credentials through `secretRef`, Region and pricing table. | ai-llm-gateway |
| Bundle | A source directory rooted at `ruralz.yaml` holding the resources delivered to Nodes, with optional `overlays/<env>/`; it renders into one Revision per Environment. | configuration-model |
| Capability | A named permission in `Plugin.spec.capabilities` that lets a Plugin call one class of Host Functions; anything not granted is denied. | wasm-plugin-system |
| Cell | The blast-radius unit: one Cluster and the State Store it uses in one Region, optionally with a regional Ruralz Control; no request crosses Regions to reach a State Store. | scalability-and-distributed-state |
| Cluster | A control-plane-only kind: a set of enrolled Nodes in one Environment that share one Revision and form one Rollout target. | configuration-model |
| Consumer | A Bundle kind for a caller identity: hashed API keys, JWT subject or claim binding, OAuth clients, a Tier, Quotas and tags. | configuration-model |
| Control Store | Ruralz Control's persistence: Raft-replicated metadata (or `postgres`) plus digest-addressed Revision content and audit segments on Ruralz Control replicas. | control-plane-and-gitops |
| Control Stream | The mTLS gRPC stream `ruralz.control.v1.ControlStream` each Node dials to `ruralz-control` on 8091: snapshots, deltas and promoted digests down; ACK, NACK and heartbeats up. | control-plane-and-gitops |
| Drain | Graceful shutdown of a Node: readiness fails, listeners stop accepting, HTTP/2 GOAWAY is sent, and in-flight requests finish within a bounded time. | zero-downtime-upgrades-and-hot-reload |
| Drift | Any difference between the digest or `/config/dump` a Node reports and the Revision Ruralz Control assigned from Git; Ruralz Control reports it. | control-plane-and-gitops |
| Endpoint | One network address of an Upstream, listed in `endpoints` or found by discovery; never a published API path, which is a Route. | configuration-model |
| Enrollment | The one-time, off-request-path exchange in which a Node obtains its per-Node mTLS certificate from Ruralz Control; the identity persists under `${RURALZ_DATA_DIR}/identity/`. | control-plane-and-gitops |
| Environment | A control-plane-only kind that groups Clusters, selects an overlay and variables for rendering, and gates promotion of source commits. | configuration-model |
| Filter | A built-in Go implementation that a Policy configures and the Filter Chain runs in one or more Phases. | data-plane |
| Filter Chain | The per-Route ordered Filters and Plugins, resolved at validation time from attached Policies and run through the fixed Phases. | data-plane |
| Filter class | A position within a Phase that orders Policies: cors, auth, authz, admission, validation, cache, upstream-auth, transform, custom. | configuration-model |
| Host Function | A function the Plugin host exports to Plugins under Plugin ABI v1, callable only with the matching Capability. | wasm-plugin-system |
| Hot Reload | One Node activating a new Revision without a restart: compile off the request path, then one atomic snapshot swap; in-flight requests keep their snapshot. | zero-downtime-upgrades-and-hot-reload |
| Last-Known-Good | The persisted Revision under `${RURALZ_DATA_DIR}/lkg/` that a Node boots when it cannot obtain configuration; promoted on activation in file mode, at the promoted digest in Control mode. | system-overview |
| Node | One running `ruralzd` process, identified by `node.id` (a ULID); it persists only its enrollment identity, Last-Known-Good and disposable caches. | system-overview |
| Performance Budget | A latency, allocation or resource ceiling tagged (target) or (hypothesis) that the benchmark suite verifies; the performance document owns the values. | performance-budgets-and-benchmarking |
| Phase | One fixed hook of the Filter Chain, from `onRequestHeaders` to `onLog`, plus the streaming hook `onChunk`. | data-plane |
| Plugin | A Bundle kind and the sandboxed WASM artifact it pins by digest, running under Plugin ABI v1 with deny-by-default Capabilities and declared limits. | wasm-plugin-system |
| Plugin ABI | The guest-host contract `ruralz.plugin.v1` (Plugin ABI v1): Extism-style memory and Host Function conventions with Capability checks. | wasm-plugin-system |
| Policy | A named, reusable Bundle resource of one registered `type` that configures a Filter or Plugin and attaches to a Gateway, Route or Upstream. | configuration-model |
| promoted digest | The Revision digest of a Cluster's last `complete` Rollout, sent on every snapshot, delta and heartbeat; a Node promotes to Last-Known-Good when its active digest matches. | control-plane-and-gitops |
| Prompt Cache | Provider-side prompt caching, such as Anthropic `cache_control`, configured on `AIModel` and preserved by native passthrough. | ai-llm-gateway |
| Provider Fallback | Moving an AI request to the next `AIModel` candidate after a defined failure class, only before the response is committed. | ai-llm-gateway |
| Quota | A long-window Consumer allowance (unit requests or tokens) enforced by `quota` or `ai.token-budget`: checked before commit, settled asynchronously. | traffic-management-and-resilience |
| Rate Limit | A `ratelimit` Policy bounding requests per key and window: a local token bucket at the per-Node ceiling plus GCRA in the State Store; fails open by default. | traffic-management-and-resilience |
| Region | A cloud or data-center region; its Nodes use their own State Store, and no request crosses a Region to reach one. | scalability-and-distributed-state |
| Response Cache | The `cache` Policy: HTTP-semantics response caching in the State Store, looked up before the Upstream call and stored asynchronously after commit. | traffic-management-and-resilience |
| Revision | The immutable SHA-256 digest of one Bundle rendered for one Environment in `ruralz.canonical.v1` form; displayed as `rev-<12 hex>`, verified in full. | configuration-model |
| Rollout | Ruralz Control delivering one Revision to one Cluster under the Cluster's `spec.rollout` plan with per-Node ACK/NACK; file-mode delivery is not a Rollout. | control-plane-and-gitops |
| Route | A Bundle kind that matches requests and sends them through attached Policies to one or more Upstreams, directly or by composition. | configuration-model |
| Ruralz Console | The React and TypeScript web UI embedded in `ruralz-control`, served at `/console` on port 8090. | control-plane-and-gitops |
| Ruralz Control | The optional control plane, binary `ruralz-control`: builds Revisions from Git, runs Rollouts over the Control Stream, and hosts Ruralz Console, RBAC, audit log and Drift detection. | control-plane-and-gitops |
| Ruralz Gateway | The stateless data plane, binary `ruralzd`; each running process is a Node that terminates client protocols, runs the Filter Chain and forwards to Upstreams. | system-overview |
| Semantic Cache | Gateway-side cache of AI responses matched by embedding similarity, enabled per Route by an `ai.semantic-cache` Policy and stored on the State Store deployment. | ai-llm-gateway |
| slot | A Policy's precedence key: a Route Policy replaces a Gateway Policy in the same slot unless that one is `overridable: false`; different slots stack. | configuration-model |
| State Store | The external store for shared runtime state (Rate Limits, Quotas, Token Budgets, caches, sessions): driver `memory` for one Node or `redis` for Redis, Valkey or Dragonfly. | scalability-and-distributed-state |
| Tier | A free-form Consumer label (`spec.tier`) that Policies read to select limits or behavior. | configuration-model |
| Token Budget | An `ai.token-budget` Policy that atomically reserves estimated input plus capped output tokens, guards the stream locally and settles with provider-reported usage. | ai-llm-gateway |
| Upstream | A Bundle kind for a target service: Endpoints or discovery, protocol, load balancing, health checks, retries, circuit breaking, TLS and upstream-leg Policies. | configuration-model |
| upstream leg | One Upstream call made for a Route, a weighted target or a composition step; Upstream-scoped Policies run only in that leg's Phases. | configuration-model |
| Zero-Downtime Upgrade | Replacing the `ruralzd` binary: the new process binds with `SO_REUSEPORT` and reports ready, then the old process Drains. | zero-downtime-upgrades-and-hot-reload |

**Forbidden aliases** (script-checked, case-insensitive, word boundary; wrap unavoidable quotations of KrakenD terminology with `<!-- alias-ok -->` on the same line):

| Forbidden | Use instead |
|---|---|
| backend (as a Ruralz concept) | Upstream |
| endpoint (KrakenD sense: a published API path) | Route |
| controller, management plane | Ruralz Control |
| dashboard | Ruralz Console |
| sync protocol, config push protocol | Control Stream |
| cache cluster | State Store |
| extension, module (for WASM) | Plugin |

## 12. Naming conventions

- Files: kebab-case with two-digit prefix inside each folder (`03-data-plane.md`). ADRs: `NNNN-<slug>.md`.
- CLI: `ruralz <noun> <verb>` from the section 9 registry; formats such as `krakend`, `openapi`, `postman` and `dot` are arguments, not verbs.
- Policy types: the dotted lower-case identifiers of section 10.
- Proto packages: `ruralz.control.v1`, `ruralz.plugin.v1`. Versioned formats: `ruralz.canonical.v1` (Revision serialization), `ruralz.diff.v1` (diff JSON). Go packages: lower-case singular.
- Kubernetes labels/annotations: `ruralz.io/<name>`; CRD API group `ruralz.io`.

## 13. KrakenD parity anchor

The KrakenD CE/EE feature table snapshot (2026-09-23, EE 2.13) lives in `docs/_meta/research/krakend-parity.md`. Every EE-only feature MUST appear in `docs/comparison/01-krakend-ee-parity-matrix.md` as free in Ruralz with a milestone, or be explicitly "Not planned" with a reason. KrakenD has no WASM, no control plane, no console, no GraphQL federation and no HTTP/3 in either edition; KrakenD CE 3.0 removes Go plugin support (announced 2026-06-04). The EE plugin generator, end-to-end testing tool, OpenAPI importer and exporter, Postman and DOT generators and dump to disk map to section 9; IP filtering and MaxMind GeoIP map to section 10.

## 14. Change control

- History: the previous version was authored on 2026-09-23, before the research files existed. This version was frozen on 2026-09-23 after the System overview and Configuration model judge panels and the Vision and Tech stack reviews; it decides 114 amendment proposals (61 distinct) as logged in `docs/_meta/reviews/freeze-decisions.md`, and its research actions and two library selections were completed against `docs/_meta/research/tooling-and-licenses.md` and the corrected section 11 of `docs/_meta/research/licensing-landscape.md`. All Wave 2 and Wave 3 documents are written against it, and a later step conforms the four foundation documents to it.
- After the freeze, changes require an ADR or an entry in the owning document's Open questions.

Open questions this freeze decides; owners close them during conformance:

| Open question | Decision |
|---|---|
| OQ-system-overview-1 | HTTP/3 is Planned (M3) |
| OQ-system-overview-2 | Level-triggered Last-Known-Good promotion (section 8.2) |
| OQ-system-overview-3 | ADR-0017 (section 8.14) |
| OQ-system-overview-7 | Raft on port 8092; `postgres` Planned (M4) |
| OQ-system-overview-17 | `paused` Rollout state; batch sizing stays with Control plane and GitOps |
| OQ-system-overview-20 | Local counts guard the stream within its reservation and are never billed (section 8.9) |
| OQ-configuration-model-1 | Option (a): CRDs `ruralz.io/v1alpha1`, Bundles `ruralz/v1alpha1` |
| OQ-configuration-model-9 | Option (a): feature documents author `config`; the Configuration model registers it |
| OQ-configuration-model-14 | Option (a): `secretRef`-held keys are hashed at load |
| OQ-vision-and-positioning-10 | Built-in `authz.ip` (M1) and `authz.geoip` (M2) |
| OQ-vision-and-positioning-11 | CLI import, export and test commands (section 9); serving stays open |
| OQ-vision-and-positioning-14 | Gateway `limits.maxPluginMemoryBytes` |
| OQ-tech-stack-and-libraries-1, -2, -3, -7 | Section 7 corrections |
| OQ-tech-stack-and-libraries-12, -13; OQ-configuration-model-16 | Section 7 selections `goccy/go-yaml` and `santhosh-tekuri/jsonschema/v6`: option (a) of OQ-configuration-model-16 |
| OQ-tech-stack-and-libraries-16 | `ruralz-control` exposes `/metrics` on 9902; the exporter stays open |

Research actions, required before the dependent work and not waived for CI-only tooling. Status as of 2026-09-23:

| Action | Status | Evidence | Unblocks |
|---|---|---|---|
| Add URLs and licenses for MADR 4.0, markdownlint-cli2, lychee and mermaid to `docs/_meta/research/` | Complete | `docs/_meta/research/tooling-and-licenses.md` section 1: MADR `MIT OR CC0-1.0`, markdownlint-cli2 `MIT`, lychee `Apache-2.0 OR MIT`, mermaid `MIT`, each with repository, LICENSE file and release URLs | OQ-tech-stack-and-libraries-5; tech stack acceptance criteria 1 and 2, closed when the tech stack document cites these rows at conformance |
| Confirm licenses of `raft-boltdb/v2`, `go-msgpack/v2`, `go.etcd.io/bbolt` and raft's other transitive modules | Complete | `docs/_meta/research/tooling-and-licenses.md` section 3 (summary in the section 7 Control Store row). Left to the tech stack document: name `go-immutable-radix` and `golang-lru` as MPL-2.0 exceptions, and decide whether its license gate reads `go mod graph`, which also lists the MPL-2.0 `go-cleanhttp` and `go-retryablehttp` (section 3.4); the Planned (M0) license gate confirms the linked set (Gaps) | OQ-tech-stack-and-libraries-10; ADR-0006; Control Store code |
| Correct `licensing-landscape.md` section 11: a DCO alone does not stop relicensing of Apache-2.0 contributions; P1 and ADR-0002 are the commitment | Complete | `docs/_meta/research/licensing-landscape.md` section 11: under Apache-2.0 Sections 2, 4 and 5, Revington, like any redistributor, may ship later Derivative Works under different terms; a binding commitment needs a pledge, foundation stewardship or trademark policy, pending legal review | ADR-0002 |
| Research candidates for the section 7 pending selections (OQ-tech-stack-and-libraries-12 to -20) | Complete | `docs/_meta/research/tooling-and-licenses.md` sections 6 (YAML), 7 (JSON Schema) and 8 (protobuf, ULID, `/metrics`, SigV4, CLI, SAML, `postgres`). The YAML parser and JSON Schema validator are selected in section 7; the other seven stay pending until the tech stack document evaluates them and checks their dependencies' licenses (`crewjam/saml` fails S1) | OQ-tech-stack-and-libraries-12 to -20 |
| Research a MaxMind-format database reader | Open | Not covered by any research file | `authz.geoip` catalog row |
