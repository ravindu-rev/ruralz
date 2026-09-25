---
title: Glossary
status: reviewed
owner: ruralz-core
last_updated: 2026-09-25
depends_on:
  - docs/_meta/foundation-pack.md
  - docs/_meta/style-guide.md
  - docs/architecture/01-system-overview.md
  - docs/architecture/02-configuration-model.md
  - docs/architecture/03-data-plane.md
  - docs/architecture/04-control-plane-and-gitops.md
  - docs/architecture/05-wasm-plugin-system.md
  - docs/architecture/06-ai-llm-gateway.md
  - docs/architecture/09-traffic-management-and-resilience.md
  - docs/architecture/11-scalability-and-distributed-state.md
  - docs/architecture/12-performance-budgets-and-benchmarking.md
  - docs/operations/02-zero-downtime-upgrades-and-hot-reload.md
adrs: []
milestone_tags_used: []
---

# Glossary

## Summary

This glossary fixes the meaning, canonical spelling and forbidden aliases of every Ruralz term that the documentation set shares. Each definition restates the foundation pack glossary in 40 words or fewer and links to the section of the document that owns the term, where the full rules live. The Forbidden aliases section lists the script-checked aliases and the escape comment for unavoidable quotations. Every reader should keep it open while reading other documents, and every writer and reviewer MUST use these spellings.

## Scope and non-goals

In scope:

- Every term in the manifest `glossary_terms` list, in one alphabetical table.
- The forbidden aliases of the foundation pack, with their replacements.

Non-goals:

- Kind fields, Policy types and their `config` schemas: the [Configuration model](architecture/02-configuration-model.md#kind-catalog) owns them.
- CLI commands and the REST API: [CLI and API surface](reference/01-cli-and-api-surface.md) owns them.
- Normative rules. A definition here summarizes; when it seems to differ from the owning section, the owning section wins and the difference is a defect to fix here.

## Terms

Terms sort alphabetically, ignoring case. "Canonical spelling" gives the exact capitalization to use in prose; kinds also appear in code formatting. "Forbidden aliases" lists the names never to use for the term; aliases that the verifier checks are also listed under [Forbidden aliases](#forbidden-aliases).

| Term | Definition | Canonical spelling | Forbidden aliases | Defined in |
|---|---|---|---|---|
| active Revision | The Revision whose compiled snapshot a Node serves now; replaced only by an atomic swap after validation and kept while the Node is detached from Ruralz Control. | active Revision (lower-case "active") | current config, live config | [System overview: Compile before swap](architecture/01-system-overview.md#compile-before-swap) |
| AIModel | A Bundle kind naming a virtual model that clients request, mapped to ordered provider candidates with a strategy, token limits and cache settings. | AIModel (kind `AIModel`) | None | [AI/LLM gateway: AIProvider, AIModel and dialect translation matrix](architecture/06-ai-llm-gateway.md#aiprovider-aimodel-and-dialect-translation-matrix) |
| AIProvider | A Bundle kind for one LLM provider account: dialect, base URL, credentials through `secretRef`, Region and pricing table. | AIProvider (kind `AIProvider`) | None | [AI/LLM gateway: AIProvider, AIModel and dialect translation matrix](architecture/06-ai-llm-gateway.md#aiprovider-aimodel-and-dialect-translation-matrix) |
| Bundle | A source directory rooted at `ruralz.yaml` holding the resources delivered to Nodes, with optional `overlays/<env>/`; it renders into one Revision per Environment. | Bundle | krakend.json, config tree, manifest | [Configuration model: Bundle layout and merge](architecture/02-configuration-model.md#bundle-layout-and-merge) |
| Capability | A named permission in `Plugin.spec.capabilities` that lets a Plugin call one class of Host Functions; anything not granted is denied. | Capability | None | [WASM plugin system: Capabilities and sandboxing](architecture/05-wasm-plugin-system.md#capabilities-and-sandboxing) |
| Cell | The blast-radius unit: one Cluster and the State Store it uses in one Region, optionally with a regional Ruralz Control; no request crosses Regions to reach a State Store. | Cell | shard, partition | [Scalability and distributed state: Cells and blast radius](architecture/11-scalability-and-distributed-state.md#cells-and-blast-radius) |
| Cluster | A control-plane-only kind: a set of enrolled Nodes in one Environment that share one Revision and form one Rollout target. | Cluster (kind `Cluster`) | fleet, pool | [Configuration model: Cluster](architecture/02-configuration-model.md#cluster) |
| Consumer | A Bundle kind for a caller identity: hashed API keys, JWT subject or claim binding, OAuth clients, a Tier, Quotas and tags. | Consumer (kind `Consumer`) | None | [Configuration model: Consumer](architecture/02-configuration-model.md#consumer) |
| Control Store | Ruralz Control's persistence: Raft-replicated metadata (or `postgres`) plus digest-addressed Revision content and audit segments on Ruralz Control replicas. | Control Store | database, CP DB | [Control plane and GitOps: Control Store and high availability](architecture/04-control-plane-and-gitops.md#control-store-and-high-availability) |
| Control Stream | The mTLS gRPC stream `ruralz.control.v1.ControlStream` each Node dials to `ruralz-control` on 8091: snapshots, deltas and promoted digests down; ACK, NACK and heartbeats up. | Control Stream | xDS, sync protocol, config push protocol <!-- alias-ok --> | [Control plane and GitOps: Control Stream](architecture/04-control-plane-and-gitops.md#control-stream) |
| Drain | Graceful shutdown of a Node: readiness fails, listeners stop accepting, HTTP/2 GOAWAY is sent, and in-flight requests finish within a bounded time. | Drain (verb and noun) | None | [Zero-downtime upgrades and hot reload: Drain timeline defaults](operations/02-zero-downtime-upgrades-and-hot-reload.md#drain-timeline-defaults) |
| Drift | Any difference between the digest or `/config/dump` a Node reports and the Revision Ruralz Control assigned from Git; Ruralz Control reports it. | Drift | None | [Control plane and GitOps: Drift detection](architecture/04-control-plane-and-gitops.md#drift-detection) |
| Endpoint | One network address of an Upstream, listed in `endpoints` or found by discovery; never a published API path, which is a Route. | Endpoint (field `endpoints`) | None | [Configuration model: Upstream](architecture/02-configuration-model.md#upstream) |
| Enrollment | The one-time, off-request-path exchange in which a Node obtains its per-Node mTLS certificate from Ruralz Control; the identity persists under `${RURALZ_DATA_DIR}/identity/`. | Enrollment | None | [Control plane and GitOps: Enrollment and mTLS](architecture/04-control-plane-and-gitops.md#enrollment-and-mtls) |
| Environment | A control-plane-only kind that groups Clusters, selects an overlay and variables for rendering, and gates promotion of source commits. | Environment (kind `Environment`) | stage, tier (Tier is a Consumer concept) | [Configuration model: Environment](architecture/02-configuration-model.md#environment) |
| Filter | A built-in Go implementation that a Policy configures and the Filter Chain runs in one or more Phases. | Filter | None | [Data plane: Filter Chain execution](architecture/03-data-plane.md#filter-chain-execution) |
| Filter Chain | The per-Route ordered Filters and Plugins, resolved at validation time from attached Policies and run through the fixed Phases. | Filter Chain | None | [Data plane: Filter Chain execution](architecture/03-data-plane.md#filter-chain-execution) |
| Filter class | A position within a Phase that orders Policies: cors, auth, authz, admission, validation, cache, upstream-auth, transform, custom. | Filter class (lower-case "class"; field `filterClass`) | None | [Configuration model: Policy](architecture/02-configuration-model.md#policy) |
| Host Function | A function the Plugin host exports to Plugins under Plugin ABI v1, callable only with the matching Capability. | Host Function | None | [WASM plugin system: Host Function table](architecture/05-wasm-plugin-system.md#host-function-table) |
| Hot Reload | One Node activating a new Revision without a restart: compile off the request path, then one atomic snapshot swap; in-flight requests keep their snapshot. | Hot Reload | None | [Zero-downtime upgrades and hot reload: Configuration hot reload](operations/02-zero-downtime-upgrades-and-hot-reload.md#configuration-hot-reload) |
| Last-Known-Good | The persisted Revision under `${RURALZ_DATA_DIR}/lkg/` that a Node boots when it cannot obtain configuration; promoted on activation in file mode, at the promoted digest in Control mode. | Last-Known-Good | fallback config, cached config | [System overview: Last-Known-Good](architecture/01-system-overview.md#last-known-good) |
| Node | One running `ruralzd` process, identified by `node.id` (a ULID); it persists only its enrollment identity, Last-Known-Good and disposable caches. | Node | instance, replica, gateway node, pod (outside Kubernetes context) | [System overview: Inside a Node](architecture/01-system-overview.md#inside-a-node) |
| Performance Budget | A latency, allocation or resource ceiling tagged (target) or (hypothesis) that the benchmark suite verifies; the performance document owns the values. | Performance Budget | None | [Performance budgets and benchmarking: Budget catalog and SLO ties](architecture/12-performance-budgets-and-benchmarking.md#budget-catalog-and-slo-ties) |
| Phase | One fixed hook of the Filter Chain, from `onRequestHeaders` to `onLog`, plus the streaming hook `onChunk`. | Phase | None | [Data plane: Phases](architecture/03-data-plane.md#phases) |
| Plugin | A Bundle kind and the sandboxed WASM artifact it pins by digest, running under Plugin ABI v1 with deny-by-default Capabilities and declared limits. | Plugin (kind `Plugin`) | WASM extension, WASM module <!-- alias-ok --> | [WASM plugin system: Packaging](architecture/05-wasm-plugin-system.md#packaging) |
| Plugin ABI | The guest-host contract `ruralz.plugin.v1` (Plugin ABI v1): Extism-style memory and Host Function conventions with Capability checks. | Plugin ABI v1 (identifier `ruralz.plugin.v1`) | proxy-wasm (allowed only for the compatibility adapter) | [WASM plugin system: Plugin ABI v1](architecture/05-wasm-plugin-system.md#plugin-abi-v1) |
| Policy | A named, reusable Bundle resource of one registered `type` that configures a Filter or Plugin and attaches to a Gateway, Route or Upstream. | Policy (kind `Policy`) | None | [Configuration model: Policy](architecture/02-configuration-model.md#policy) |
| promoted digest | The Revision digest of a Cluster's last `complete` Rollout, sent on every snapshot, delta and heartbeat; a Node promotes to Last-Known-Good when its active digest matches. | promoted digest (lower case) | None | [Control plane and GitOps: Rollout states](architecture/04-control-plane-and-gitops.md#rollout-states) |
| Prompt Cache | Provider-side prompt caching, such as Anthropic `cache_control`, configured on `AIModel` and preserved by native passthrough. | Prompt Cache | None | [AI/LLM gateway: Prompt Cache](architecture/06-ai-llm-gateway.md#prompt-cache) |
| Provider Fallback | Moving an AI request to the next `AIModel` candidate after a defined failure class, only before the response is committed. | Provider Fallback | None | [AI/LLM gateway: Provider Fallback](architecture/06-ai-llm-gateway.md#provider-fallback) |
| Quota | A long-window Consumer allowance (unit requests or tokens) enforced by `quota` or `ai.token-budget`: checked before commit, settled asynchronously. | Quota | None | [Traffic management and resilience: Quotas](architecture/09-traffic-management-and-resilience.md#quotas) |
| Rate Limit | A `ratelimit` Policy bounding requests per key and window: a local token bucket at the per-Node ceiling plus GCRA in the State Store; fails open by default. | Rate Limit | None | [Traffic management and resilience: Rate limiting](architecture/09-traffic-management-and-resilience.md#rate-limiting) |
| Region | A cloud or data-center region; its Nodes use their own State Store, and no request crosses a Region to reach one. | Region | None | [Scalability and distributed state: Multi-region](architecture/11-scalability-and-distributed-state.md#multi-region) |
| Response Cache | The `cache` Policy: HTTP-semantics response caching in the State Store, looked up before the Upstream call and stored asynchronously after commit. | Response Cache | None | [Traffic management and resilience: Response caching](architecture/09-traffic-management-and-resilience.md#response-caching) |
| Revision | The immutable SHA-256 digest of one Bundle rendered for one Environment in `ruralz.canonical.v1` form; displayed as `rev-<12 hex>`, verified in full. | Revision (display `rev-<12 hex>`, wire `sha256:<64 hex>`) | version, release (for configuration) | [Configuration model: Canonical form and Revision](architecture/02-configuration-model.md#canonical-form-and-revision) |
| Rollout | Ruralz Control delivering one Revision to one Cluster under the Cluster's `spec.rollout` plan with per-Node ACK/NACK; file-mode delivery is not a Rollout. | Rollout | deploy, push | [Control plane and GitOps: Rollout plan, batches and gates](architecture/04-control-plane-and-gitops.md#rollout-plan-batches-and-gates) |
| Route | A Bundle kind that matches requests and sends them through attached Policies to one or more Upstreams, directly or by composition. | Route (kind `Route`) | endpoint in the KrakenD sense, API endpoint <!-- alias-ok --> | [Configuration model: Route](architecture/02-configuration-model.md#route) |
| Ruralz Console | The React and TypeScript web UI embedded in `ruralz-control`, served at `/console` on port 8090. | Ruralz Console | dashboard, UI, admin panel <!-- alias-ok --> | [Control plane and GitOps: Ruralz Console](architecture/04-control-plane-and-gitops.md#ruralz-console) |
| Ruralz Control | The optional control plane, binary `ruralz-control`: builds Revisions from Git, runs Rollouts over the Control Stream, and hosts Ruralz Console, RBAC, audit log and Drift detection. | Ruralz Control (binary `ruralz-control`) | controller, management plane, control server <!-- alias-ok --> | [Control plane and GitOps: Responsibilities and non-responsibilities](architecture/04-control-plane-and-gitops.md#responsibilities-and-non-responsibilities) |
| Ruralz Gateway | The stateless data plane, binary `ruralzd`; each running process is a Node that terminates client protocols, runs the Filter Chain and forwards to Upstreams. | Ruralz Gateway (binary `ruralzd`) | the proxy, data plane binary | [System overview: Component map](architecture/01-system-overview.md#component-map) |
| Semantic Cache | Gateway-side cache of AI responses matched by embedding similarity, enabled per Route by an `ai.semantic-cache` Policy and stored on the State Store deployment. | Semantic Cache | None | [AI/LLM gateway: Semantic Cache](architecture/06-ai-llm-gateway.md#semantic-cache) |
| slot | A Policy's precedence key: a Route Policy replaces a Gateway Policy in the same slot unless that one is `overridable: false`; different slots stack. | slot (lower case; field `slot`) | None | [Configuration model: Resolution rules](architecture/02-configuration-model.md#resolution-rules) |
| State Store | The external store for shared runtime state (Rate Limits, Quotas, Token Budgets, caches, sessions): driver `memory` for one Node or `redis` for Redis, Valkey or Dragonfly. | State Store (drivers `memory`, `redis`) | cache cluster, Redis as a concept name <!-- alias-ok --> | [Scalability and distributed state: Distributed state catalog](architecture/11-scalability-and-distributed-state.md#distributed-state-catalog) |
| Tier | A free-form Consumer label (`spec.tier`) that Policies read to select limits or behavior. | Tier (field `spec.tier`) | None | [Configuration model: Consumer](architecture/02-configuration-model.md#consumer) |
| Token Budget | An `ai.token-budget` Policy that atomically reserves estimated input plus capped output tokens, guards the stream locally and settles with provider-reported usage. | Token Budget | None | [AI/LLM gateway: Token Budget](architecture/06-ai-llm-gateway.md#token-budget) |
| Upstream | A Bundle kind for a target service: Endpoints or discovery, protocol, load balancing, health checks, retries, circuit breaking, TLS and upstream-leg Policies. | Upstream (kind `Upstream`) | backend <!-- alias-ok --> | [Configuration model: Upstream](architecture/02-configuration-model.md#upstream) |
| upstream leg | One Upstream call made for a Route, a weighted target or a composition step; Upstream-scoped Policies run only in that leg's Phases. | upstream leg (lower case) | None | [Configuration model: Resolution rules](architecture/02-configuration-model.md#resolution-rules) |
| Zero-Downtime Upgrade | Replacing the `ruralzd` binary: the new process binds with `SO_REUSEPORT` and reports ready, then the old process Drains. | Zero-Downtime Upgrade | None | [Zero-downtime upgrades and hot reload: Binary upgrades](operations/02-zero-downtime-upgrades-and-hot-reload.md#binary-upgrades) |

## Forbidden aliases

`scripts/verify-docs.mjs` matches these aliases case-insensitively, at word boundaries, in prose, inline code and Mermaid blocks of every document under `docs/` outside `docs/_meta/`. Other fenced code, such as a KrakenD JSON sample, is not checked.

| Forbidden | Use instead | Notes |
|---|---|---|
| backend (as a Ruralz concept) <!-- alias-ok --> | Upstream | For a State Store or Control Store implementation, write "State Store backend" or "driver" <!-- alias-ok --> |
| endpoint in the KrakenD sense: a published API path <!-- alias-ok --> | Route | Bare "Endpoint" is the glossary term for an Upstream target address and is not flagged |
| controller, management plane <!-- alias-ok --> | Ruralz Control | "Kubernetes controller" and "Ingress controller" are exempt <!-- alias-ok --> |
| dashboard <!-- alias-ok --> | Ruralz Console | Grafana dashboards and third-party product names are exempt <!-- alias-ok --> |
| sync protocol, config push protocol <!-- alias-ok --> | Control Stream | Applies to any name for the protocol between Ruralz Control and Nodes |
| cache cluster <!-- alias-ok --> | State Store | Applies to any name for shared runtime state |
| extension, module (for WASM) <!-- alias-ok --> | Plugin | The WASM specification term "module" inside runtime internals needs the escape comment |

**Escape comment.** When a line must quote a forbidden alias, for example KrakenD terminology in a migration mapping or a third-party product name, add the HTML comment `<!-- alias-ok -->` on the same line. The verifier then skips that whole line, and the comment does not render. Use it only for quotation or disambiguation, never to name a Ruralz concept.

Forbidden aliases in the Terms table that this table does not list, such as "fleet" for Cluster, come from the foundation pack canonical names and are enforced by review rather than by the verifier.

## Open questions

None.
