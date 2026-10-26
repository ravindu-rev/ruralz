# Ruralz Documentation

## Summary

This index is the entry point to the Ruralz design documentation. It explains how the folders under `docs/` are organized, gives five reading paths (Evaluator, Contributor, Operator, Plugin author, AI platform) that order the documents for each audience, lists every document, indexes the Architecture Decision Records and summarizes the conventions a reader needs to interpret tags, identifiers and names. It decides nothing about the product itself. Everyone should start here, pick the reading path that matches their role, and keep the glossary open.

## Scope and non-goals

In scope:

- The folder layout of `docs/` and what each folder owns.
- Five audience-specific reading paths.
- A list of every document.
- A summary index of all ADRs and a link to the full ADR index.
- The conventions a reader needs to interpret the documents.

Non-goals:

- Product design. Each linked document owns its own decisions, and this index never overrides them.
- The full style rules. The style guide owns them; Conventions only summarizes what readers need.
- Term definitions. The glossary owns them.
- The shared inputs under `docs/_meta/`: the foundation pack (canonical names and registries) and the style guide.

## Organization

The documentation is split by question. Each folder answers one kind of question, and every file inside a folder carries a two-digit prefix that gives its suggested reading order.

| Folder | Question it answers | Main audience |
|---|---|---|
| `docs/vision/` | Why Ruralz exists, who it serves, the four differentiators and the principles `P1` to `P10` | Evaluators, everyone |
| `docs/features/` | Which capabilities Ruralz plans, each with its milestone or a `Not planned` reason | Evaluators |
| `docs/architecture/` | How Ruralz Gateway, Ruralz Control, Ruralz Console, the State Store and the Control Store are designed, from the system overview to performance budgets | Architects, contributors |
| `docs/operations/` | How to deploy, upgrade, size and recover Ruralz in production | Operators |
| `docs/engineering/` | Which libraries are used, how the repository is laid out, how code is tested and how releases are versioned | Contributors |
| `docs/reference/` | The `ruralz` CLI, the admin APIs, the REST API and the Control Stream service | Operators, contributors, Plugin authors |
| `docs/roadmap/` | The order of milestones `M0` to `M5`, their scope and exit criteria, and the steps left in the current milestone | Evaluators, contributors |
| `docs/adr/` | Architecture Decision Records: one decision each, with context, options and consequences | Architects, contributors |
| `docs/` (root) | This index and the glossary of shared terms | Everyone |

Every design document follows the same frame: a Summary, then Scope and non-goals, then the body, and Open questions last; the step list is one table. Unresolved design points never hide in prose; they are listed in the Open questions table of the document that owns them.

## Reading paths

Each path lists documents in reading order. Paths overlap on purpose: the System Overview and the Configuration Model are the shared foundation for every technical role. Links in this section repeat documents that the Document index also links.

### Evaluator

For people deciding whether Ruralz fits their organization.

1. [Vision and Positioning](vision/01-vision-and-positioning.md): the four differentiators, the principles and the Apache-2.0 license with no feature gating.
2. [Feature Catalog](features/01-feature-catalog.md): every Ruralz capability mapped to a milestone or a `Not planned` reason.
3. [System Overview](architecture/01-system-overview.md): the two runtime components, the CLI and how they degrade.
4. [Roadmap and Milestones](roadmap/01-roadmap-and-milestones.md): what arrives in which milestone and how completion is measured.

### Contributor

For people who will write Ruralz code.

1. [Vision and Positioning](vision/01-vision-and-positioning.md): the principles every design inherits.
2. [System Overview](architecture/01-system-overview.md): component boundaries and the request path.
3. [Configuration Model](architecture/02-configuration-model.md): the ten kinds, their fields and the Policy type registry.
4. [Tech Stack and Libraries](engineering/01-tech-stack-and-libraries.md): the only libraries a design may assume.
5. [Repository Layout and Conventions](engineering/02-repository-layout-and-conventions.md): the monorepo tree, import rules, DCO and CI stages.
6. [Testing and Quality Strategy](engineering/03-testing-and-quality-strategy.md): the tests each change must bring.
7. [Data Plane](architecture/03-data-plane.md): the Filter Chain, Route matching and Hot Reload inside Ruralz Gateway.
8. [Release, Versioning and Compatibility](engineering/04-release-versioning-and-compatibility.md): version skew and compatibility rules.
9. [Roadmap and Milestones](roadmap/01-roadmap-and-milestones.md) and [Steps](roadmap/02-steps.md): where the next piece of work fits.

### Operator

For people who will deploy and run Ruralz in production.

1. [System Overview](architecture/01-system-overview.md): what runs where and what keeps working when a neighbor fails.
2. [Deployment Topologies](operations/01-deployment-topologies.md): ten topologies, ports and three reference architectures.
3. [CLI and API Surface](reference/01-cli-and-api-surface.md): the `ruralz` CLI, admin APIs and exit codes.
4. [Observability](architecture/10-observability.md): metrics, traces, logs, alerts and SLOs.
5. [Zero-Downtime Upgrades and Hot Reload](operations/02-zero-downtime-upgrades-and-hot-reload.md): changing configuration and binaries without dropping traffic.
6. [Capacity Planning](operations/03-capacity-planning.md): turning workload figures into Node counts and State Store sizes.
7. [Scalability and Distributed State](architecture/11-scalability-and-distributed-state.md): Cells, Regions and accuracy bounds of shared state.
8. [High Availability and Disaster Recovery](operations/04-high-availability-and-disaster-recovery.md): degraded modes, backups and runbooks.

### Plugin author

For people who will extend Ruralz Gateway with WASM Plugins.

1. [System Overview](architecture/01-system-overview.md): where Plugins run and how a Revision delivers them.
2. [Configuration Model](architecture/02-configuration-model.md): the `Plugin` and `Policy` kinds and slot precedence.
3. [Data Plane](architecture/03-data-plane.md): the fixed Phases of the Filter Chain and short-circuit rules.
4. [WASM Plugin System](architecture/05-wasm-plugin-system.md): Plugin ABI v1, Host Functions, Capabilities, limits and packaging.
5. [Security and Identity](architecture/08-security-and-identity.md): the Plugin threat model and artifact signing.
6. [Testing and Quality Strategy](engineering/03-testing-and-quality-strategy.md): the Plugin ABI conformance suite and the Plugin SDK harness.
7. [CLI and API Surface](reference/01-cli-and-api-surface.md): the commands that build, test and publish Plugins.
8. [Release, Versioning and Compatibility](engineering/04-release-versioning-and-compatibility.md): how Plugin ABI v1 evolves.

### AI platform

For AI platform engineers routing LLM traffic through Ruralz Gateway.

1. [System Overview](architecture/01-system-overview.md): the request path and the State Store.
2. [Configuration Model](architecture/02-configuration-model.md): the `AIProvider` and `AIModel` kinds.
3. [AI/LLM Gateway](architecture/06-ai-llm-gateway.md): the OpenAI-compatible facade, Provider Fallback, Token Budgets, Prompt Cache and Semantic Cache.
4. [Traffic Management and Resilience](architecture/09-traffic-management-and-resilience.md): Rate Limits, Quotas, retries and circuit breaking.
5. [Security and Identity](architecture/08-security-and-identity.md): secrets through `secretRef`, authorization and data residency controls.
6. [Observability](architecture/10-observability.md): `gen_ai` telemetry and cost attribution signals.
7. [Scalability and Distributed State](architecture/11-scalability-and-distributed-state.md): how Token Budgets and caches share state across Nodes.
8. [Capacity Planning](operations/03-capacity-planning.md): sizing Nodes and the State Store for AI workloads.

## Document index

| Document | Folder |
|---|---|
| Ruralz Documentation (this index) | `docs/` |
| [Glossary](glossary.md) | `docs/` |
| [Vision and Positioning](vision/01-vision-and-positioning.md) | `docs/vision/` |
| [Feature Catalog](features/01-feature-catalog.md) | `docs/features/` |
| [System Overview](architecture/01-system-overview.md) | `docs/architecture/` |
| [Configuration Model](architecture/02-configuration-model.md) | `docs/architecture/` |
| [Data Plane](architecture/03-data-plane.md) | `docs/architecture/` |
| [Control Plane and GitOps](architecture/04-control-plane-and-gitops.md) | `docs/architecture/` |
| [WASM Plugin System](architecture/05-wasm-plugin-system.md) | `docs/architecture/` |
| [AI/LLM Gateway](architecture/06-ai-llm-gateway.md) | `docs/architecture/` |
| [Multi-Protocol Support](architecture/07-multi-protocol.md) | `docs/architecture/` |
| [Security and Identity](architecture/08-security-and-identity.md) | `docs/architecture/` |
| [Traffic Management and Resilience](architecture/09-traffic-management-and-resilience.md) | `docs/architecture/` |
| [Observability](architecture/10-observability.md) | `docs/architecture/` |
| [Scalability and Distributed State](architecture/11-scalability-and-distributed-state.md) | `docs/architecture/` |
| [Performance Budgets and Benchmarking](architecture/12-performance-budgets-and-benchmarking.md) | `docs/architecture/` |
| [Deployment Topologies](operations/01-deployment-topologies.md) | `docs/operations/` |
| [Zero-Downtime Upgrades and Hot Reload](operations/02-zero-downtime-upgrades-and-hot-reload.md) | `docs/operations/` |
| [Capacity Planning](operations/03-capacity-planning.md) | `docs/operations/` |
| [High Availability and Disaster Recovery](operations/04-high-availability-and-disaster-recovery.md) | `docs/operations/` |
| [Tech Stack and Libraries](engineering/01-tech-stack-and-libraries.md) | `docs/engineering/` |
| [Repository Layout and Conventions](engineering/02-repository-layout-and-conventions.md) | `docs/engineering/` |
| [Testing and Quality Strategy](engineering/03-testing-and-quality-strategy.md) | `docs/engineering/` |
| [Release, Versioning and Compatibility](engineering/04-release-versioning-and-compatibility.md) | `docs/engineering/` |
| [CLI and API Surface](reference/01-cli-and-api-surface.md) | `docs/reference/` |
| [Roadmap and Milestones](roadmap/01-roadmap-and-milestones.md) | `docs/roadmap/` |
| [Steps](roadmap/02-steps.md) | `docs/roadmap/` |

## ADR index

The full ADR index, with the same columns, is [docs/adr/README.md](adr/README.md). ADRs use the MADR format with a Confirmation section; their status is `proposed` or `accepted`. The owning document is the one that states the decision in full and links the ADR.

| ID | Title | Status | Owning document |
|---|---|---|---|
| [ADR-0001](adr/0001-implementation-language-go.md) | Implementation language: Go with CGO_ENABLED=0 static binaries | accepted | Tech Stack and Libraries |
| [ADR-0002](adr/0002-apache-2-license-no-feature-gating.md) | License: Apache-2.0 for all components, no feature gating, DCO and Revington trademark policy | accepted | Vision and Positioning |
| [ADR-0003](adr/0003-configuration-format.md) | Configuration format: YAML with JSON Schema, a Kubernetes-style resource model and a Ruralz parser | accepted | Configuration Model |
| [ADR-0004](adr/0004-wasm-runtime-wazero.md) | WASM runtime: wazero | accepted | WASM Plugin System |
| [ADR-0005](adr/0005-plugin-abi-v1.md) | Plugin ABI v1: capability-based with Extism-style conventions, proxy-wasm adapter Planned (M4) | accepted | WASM Plugin System |
| [ADR-0006](adr/0006-control-store-raft-boltdb.md) | Control Store: embedded hashicorp/raft with raft-boltdb/v2 on bbolt, Postgres optional | proposed | Control Plane and GitOps |
| [ADR-0007](adr/0007-control-stream-protocol.md) | Control Stream: own gRPC snapshot and delta protocol with xDS-style ACK/NACK | accepted | Control Plane and GitOps |
| [ADR-0008](adr/0008-rate-limiting-local-bucket-and-gcra.md) | Rate limiting: local token bucket plus GCRA in the State Store, fail-open by default | accepted | Traffic Management and Resilience |
| [ADR-0009](adr/0009-http-stack-net-http-quic-go.md) | HTTP stack: net/http and quic-go, no fasthttp | accepted | Data Plane |
| [ADR-0012](adr/0012-graphql-engine-graphql-go-tools.md) | GraphQL engine: wundergraph/graphql-go-tools v2 | accepted | Multi-Protocol Support |
| [ADR-0013](adr/0013-messaging-client-libraries.md) | Messaging clients: franz-go, nats.go JetStream, paho.golang and embedded mochi-mqtt | accepted | Multi-Protocol Support |
| [ADR-0014](adr/0014-ai-api-surface.md) | AI API surface: OpenAI-compatible facade plus native passthrough, provider usage authoritative | accepted | AI/LLM Gateway |
| [ADR-0015](adr/0015-zero-downtime-upgrades-so-reuseport.md) | Zero-downtime upgrades: SO_REUSEPORT, drain and readiness gating, no socket passing | accepted | Zero-Downtime Upgrades and Hot Reload |
| [ADR-0016](adr/0016-kubernetes-helm-and-crds.md) | Kubernetes packaging: Helm chart and CRDs mirroring kinds, Gateway API deferred | proposed | Deployment Topologies |
| [ADR-0017](adr/0017-artifact-signing.md) | Artifact signing: Revisions and Plugins signed, verified by Nodes by default | accepted | Security and Identity |
| [ADR-0018](adr/0018-telemetry-opentelemetry-prometheus-exporter.md) | Telemetry: OpenTelemetry-first with an slog bridge and the OpenTelemetry Prometheus exporter | accepted | Observability |
| [ADR-0019](adr/0019-expressions-authorization-comprehension-stop.md) | Expressions and authorization: CEL inline with a comprehension stop, OPA and Cedar engines, no Lua | accepted | Security and Identity |

## Conventions

These rules come from the [style guide](_meta/style-guide.md) and the [foundation pack](_meta/foundation-pack.md). Readers need the following subset.

| Convention | What it means for a reader |
|---|---|
| Milestone tags | `Planned (Mx)` names the milestone that delivers a capability: `M0` Foundations, `M1` Core gateway, `M2` WASM + Control/GitOps, `M3` AI gateway + gRPC/GraphQL/WS/SSE + HTTP/3, `M4` Event protocols + multi-region + bench suite, `M5` Enterprise hardening. `Not planned` always carries a reason. No capability is described as supported or shipped. |
| Number tags | Every performance or scale figure carries `(target)` (a design goal to be met) or `(hypothesis)` (an estimate to be measured) on the same line, unless that line cites a measurement. No untagged figure is a measured result. |
| Open question IDs | Unresolved points are listed in the Open questions table of the owning document, with IDs `OQ-<docslug>-<n>` (for example `OQ-data-plane-3`) and a Blocking? column. A document that cites an unresolved point refers to its ID; an answered question leaves the table and its answer moves into the text. |
| ADR IDs | Decisions are cited as `ADR-NNNN`; an ADR is edited in place when its decision changes. |
| Canonical names | The data plane is Ruralz Gateway (binary `ruralzd`), the control plane is Ruralz Control (binary `ruralz-control`) and the web UI is Ruralz Console. The [glossary](glossary.md) defines every shared term, its exact spelling and its forbidden aliases. |
| Kinds and fields | Kinds appear in `PascalCase` code formatting (`Route`, `Upstream`, `Policy`) and YAML keys in `camelCase`. Only the Configuration Model defines kinds, fields and Policy types. |
| CLI commands | Commands appear as `ruralz <noun> <verb>`, and every command mentioned exists in the CLI and API Surface reference. |
| Normative words | `MUST`, `SHOULD` and `MAY` in capitals carry their RFC 2119 meaning. |
| Dates | Dates are ISO `YYYY-MM-DD`. |

## Open questions

None.
