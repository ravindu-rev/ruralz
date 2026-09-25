# Architecture Decision Records

This is the index of Ruralz architecture decision records (ADRs). Each ADR uses the MADR format described in the [style guide](../_meta/style-guide.md) and records one decision with its context, the options considered, the chosen option and how compliance is confirmed. The ID, title, status and date in each row are copied from the ADR's front matter. The owning document is the document that states the decision in full and links the ADR. An ADR's status is `proposed`, `accepted`, `deprecated` or `superseded-by ADR-NNNN`; of the 17 ADRs below, 15 are `accepted` and 2 are `proposed`.

The [documentation index](../README.md) lists every document and reading path.

| ID | Title | Status | Date | Owning document |
|---|---|---|---|---|
| [ADR-0001](0001-implementation-language-go.md) | Implementation language: Go with CGO_ENABLED=0 static binaries | accepted | 2026-09-23 | [Tech Stack and Libraries](../engineering/01-tech-stack-and-libraries.md) |
| [ADR-0002](0002-apache-2-license-no-feature-gating.md) | License: Apache-2.0 for all components, no feature gating, DCO and Revington trademark policy | accepted | 2026-09-23 | [Vision and Positioning](../vision/01-vision-and-positioning.md) |
| [ADR-0003](0003-configuration-format.md) | Configuration format: YAML with JSON Schema and a Kubernetes-style resource model | accepted | 2026-09-23 | [Configuration Model](../architecture/02-configuration-model.md) |
| [ADR-0004](0004-wasm-runtime-wazero.md) | WASM runtime: wazero | accepted | 2026-09-25 | [WASM Plugin System](../architecture/05-wasm-plugin-system.md) |
| [ADR-0005](0005-plugin-abi-v1.md) | Plugin ABI v1: capability-based with Extism-style conventions, proxy-wasm adapter Planned (M4) | accepted | 2026-09-25 | [WASM Plugin System](../architecture/05-wasm-plugin-system.md) |
| [ADR-0006](0006-control-store-raft-boltdb.md) | Control Store: embedded hashicorp/raft with raft-boltdb/v2 on bbolt, Postgres optional | proposed | 2026-09-25 | [Control Plane and GitOps](../architecture/04-control-plane-and-gitops.md) |
| [ADR-0007](0007-control-stream-protocol.md) | Control Stream: own gRPC snapshot and delta protocol with xDS-style ACK/NACK | accepted | 2026-09-25 | [Control Plane and GitOps](../architecture/04-control-plane-and-gitops.md) |
| [ADR-0008](0008-rate-limiting-local-bucket-and-gcra.md) | Rate limiting: local token bucket plus GCRA in the State Store, fail-open by default | accepted | 2026-09-25 | [Traffic Management and Resilience](../architecture/09-traffic-management-and-resilience.md) |
| [ADR-0009](0009-http-stack-net-http-quic-go.md) | HTTP stack: net/http and quic-go, no fasthttp | accepted | 2026-09-25 | [Data Plane](../architecture/03-data-plane.md) |
| [ADR-0010](0010-telemetry-opentelemetry-first.md) | Telemetry: OpenTelemetry-first with an slog bridge for logs | accepted | 2026-09-25 | [Observability](../architecture/10-observability.md) |
| [ADR-0011](0011-expressions-and-authorization-engines.md) | Expressions and authorization: CEL inline, OPA and Cedar engines, no Lua | accepted | 2026-09-25 | [Security and Identity](../architecture/08-security-and-identity.md) |
| [ADR-0012](0012-graphql-engine-graphql-go-tools.md) | GraphQL engine: wundergraph/graphql-go-tools v2 | accepted | 2026-09-25 | [Multi-Protocol Support](../architecture/07-multi-protocol.md) |
| [ADR-0013](0013-messaging-client-libraries.md) | Messaging clients: franz-go, nats.go JetStream, paho.golang and embedded mochi-mqtt | accepted | 2026-09-25 | [Multi-Protocol Support](../architecture/07-multi-protocol.md) |
| [ADR-0014](0014-ai-api-surface.md) | AI API surface: OpenAI-compatible facade plus native passthrough, provider usage authoritative | accepted | 2026-09-25 | [AI/LLM Gateway](../architecture/06-ai-llm-gateway.md) |
| [ADR-0015](0015-zero-downtime-upgrades-so-reuseport.md) | Zero-downtime upgrades: SO_REUSEPORT, drain and readiness gating, no socket passing | accepted | 2026-09-25 | [Zero-Downtime Upgrades and Hot Reload](../operations/02-zero-downtime-upgrades-and-hot-reload.md) |
| [ADR-0016](0016-kubernetes-helm-and-crds.md) | Kubernetes packaging: Helm chart and CRDs mirroring kinds, Gateway API deferred | proposed | 2026-09-25 | [Deployment Topologies](../operations/01-deployment-topologies.md) |
| [ADR-0017](0017-artifact-signing.md) | Artifact signing: Revisions and Plugins signed, verified by Nodes by default | accepted | 2026-09-25 | [Security and Identity](../architecture/08-security-and-identity.md) |
