# Limits and Caps

## Summary

This document catalogs the limits and caps a Ruralz Gateway Node enforces: the connection, request, buffer, configuration, state and telemetry ceilings that bound its memory and work, each with its value, what happens when it is reached and the document that owns it. It decides no value: each owning document does, and wins on conflict. Operators read it before sizing a Node; contributors read it before adding a queue, buffer, pool or table, whose ceiling joins its owning document and this catalog.

## Scope and non-goals

In scope: the per-Node limits and caps of Ruralz Gateway, Planned (M1), and those that the owning documents fix for later milestones, each tagged with its milestone. "Pack 8.7" names a section of the [foundation pack](../_meta/foundation-pack.md).

Non-goals, with owners:

- Sizing formulas and coefficients: [Capacity planning](../operations/03-capacity-planning.md).
- Accuracy bounds of shared state: [Scalability and distributed state](11-scalability-and-distributed-state.md#consistency-and-accuracy-bounds).
- SLOs, metric names and alert rules: [Observability](10-observability.md#slo-definitions).
- Per-Plugin limits and Plugin pools: [WASM plugin system](05-wasm-plugin-system.md#default-limits); Ruralz Control's limits: [Control plane and GitOps](04-control-plane-and-gitops.md).
- Kinds and fields, such as the Gateway `limits` block: the [Configuration model](02-configuration-model.md).

## Limit catalog

Every value is the owning document's, with its tag. A Gateway `limits` field is the Revision's value; every other ceiling is fixed in the release (configurability: OQ-data-plane-1, OQ-scalability-and-distributed-state-4).

| Limit | Value | When reached | Owner | Milestone |
|---|---|---|---|---|
| Client connections per Node | 20,000 (target) | Each listener holds the connection it accepted until a slot frees; the kernel queues the rest | [Data plane: Bounded resources](03-data-plane.md#bounded-resources) | Planned (M1) |
| In-flight units per Node | 20,000 (target): one per request, parallel step and stream pump | 503 `RZ-RT-005` at once, never queued | [Data plane: Bounded resources](03-data-plane.md#bounded-resources) | Planned (M1) |
| Pre-routing response writes | 2,000 at once, each with a 5 s write deadline (target) | A further handler aborts without writing | [Data plane: Bounded resources](03-data-plane.md#bounded-resources) | Planned (M1) |
| `ReadHeaderTimeout`; `IdleTimeout` | 10 s; 120 s (target) | The connection closes | [Data plane: Bounded resources](03-data-plane.md#bounded-resources) | Planned (M1) |
| Body read and response write deadlines | 10 s from admission, then the Route `timeout` plus 5 s (target) | The HTTP/2 stream resets or the HTTP/1.1 connection closes | [Data plane: Bounded resources](03-data-plane.md#bounded-resources) | Planned (M1) |
| HTTP/2 streams per connection | 250 (target) | The 251st concurrent stream is refused | [Data plane: Bounded resources](03-data-plane.md#bounded-resources) | Planned (M1) |
| HTTP/2 unread request data | 256 KiB per connection, 64 KiB per stream (target) | Flow control holds the client's data | [Data plane: Bounded resources](03-data-plane.md#bounded-resources) | Planned (M1) |
| Header block, the request target included | 256 KiB (target) process ceiling; `limits.maxRequestHeaderBytes`, default 64 KiB, and 256 field lines (target) below it | A plain 431 above the ceiling; 431 `RZ-RT-002` above the Revision's limit | [Data plane: Listeners and protocols](03-data-plane.md#listeners-and-protocols) | Planned (M1) |
| Upstream response header block | 256 KiB (target) | The attempt fails as a protocol error | [Data plane: Bounded resources](03-data-plane.md#bounded-resources) | Planned (M1) |
| One streamed chunk | 1 MiB (target), reserved from the stream share | `RZ-RT-013` | [Data plane: Bounded resources](03-data-plane.md#bounded-resources) | Planned (M1) |
| Request and response bodies | `limits.maxRequestBodyBytes` and `limits.maxResponseBodyBytes`, default 10 MiB each; a step's `maxBodyBytes`, by default an equal share of `maxResponseBodyBytes` | 413 `RZ-RT-003` for a request body; a gated response or step body fails, and a tee stops copying and skips its store | [Configuration model: Body buffering and limits](02-configuration-model.md#body-buffering-and-limits) | Planned (M1) |
| Buffered bytes per Node | `limits.maxBufferedBytes`, default 512 MiB (target), reserved as bytes arrive | 503 `RZ-RT-004`; tees skip | [Configuration model: Body buffering and limits](02-configuration-model.md#body-buffering-and-limits) | Planned (M1) |
| Retired snapshots | K = 2 (target), plus at most one closing or ending, each with a 30 s grace period (target) | The oldest snapshot's streams end at once, and work still pinned after its grace ends with `RZ-RT-014` | [Data plane: Why no in-flight request is dropped](03-data-plane.md#why-no-in-flight-request-is-dropped) | Planned (M1) |
| `/tap` subscribers | 4, each with a 1 MiB buffer (target) | A fifth subscriber gets 503 `RZ-RT-019` | [Data plane: Admin endpoints](03-data-plane.md#admin-endpoints) | Planned (M1) |
| Configuration load | 64 MiB of source text, 20,000 resources, 64 nesting levels, 1,000,000 YAML nodes per document and 200 nodes per resource, 4,000,000 at the default (target) | RZ-CFG-001 stops the load | [Configuration model: Restricted YAML profile](02-configuration-model.md#restricted-yaml-profile) | Planned (M1) |
| CEL cost | 10,000 units at nominal sizes at validation; 1,000,000 units at runtime (target) | RZ-CFG-015; at runtime, a runtime error under the place's rule | [Configuration model: Limits](02-configuration-model.md#limits) | Planned (M1) |
| CEL comprehension stop | 50 ms or the request deadline, whichever is first (target), for expressions that can reach the runtime limit | A runtime error under the place's rule | [Configuration model: Limits](02-configuration-model.md#limits) | Planned (M1) |
| Activation | One compile deadline of 60 s (target) for every gate step | RZ-CFG-044 | [Zero-downtime upgrades: Validation gate](../operations/02-zero-downtime-upgrades-and-hot-reload.md#validation-gate) | Planned (M1) |
| Rate-limit key table | 1,048,576 entries, about 64 MiB (target) | CLOCK eviction | [Traffic management: Decision path](09-traffic-management-and-resilience.md#decision-path) | Planned (M1) |
| Local-only key segment | 131,072 entries (target) | CLOCK eviction | [Traffic management: Decision path](09-traffic-management-and-resilience.md#decision-path) | Planned (M1) |
| First-seen GCRA budget | 200 keys per second per Node in file mode (target); min(500, B / N_published) per second with a published count, Planned (M2) | Later keys take 10 s local-only entries | [Traffic management: Decision path](09-traffic-management-and-resilience.md#decision-path) | Planned (M1) |
| Retry budget per Upstream | In-flight retries at most max(3, 20% of in-flight originals) (target) | The original failure returns | [Traffic management: Retry budget](09-traffic-management-and-resilience.md#retry-budget) | Planned (M1) |
| Balancer structures | 256 MiB per Node (target) | Weighted `random` fallback | [Traffic management: Load balancing](09-traffic-management-and-resilience.md#load-balancing) | Planned (M1) |
| Response Cache hot-entry layer | 64 MiB (target) | LRU eviction | [Traffic management: Response caching](09-traffic-management-and-resilience.md#response-caching) | Planned (M1) |
| Quota denial cache | 65,536 entries, about 4 MiB (target) | CLOCK eviction | [Scalability: Per-Node limits](11-scalability-and-distributed-state.md#per-node-limits) | Planned (M1) |
| Post-commit write queue | 64,000 items and 85 MiB: 16, 1, 64 and 4 MiB per class (target) | Drops within the full class, counted | [Scalability: Post-commit write queue](11-scalability-and-distributed-state.md#post-commit-write-queue) | Planned (M1) |
| State Store calls in flight | 8,192 (target) | `failureMode` without a call; `RZ-STS-004` under `closed` | [Scalability: Per-Node limits](11-scalability-and-distributed-state.md#per-node-limits) | Planned (M1) |
| State Store connections per shard | 2 pipelined plus 8 dedicated (target) | Dedicated calls skipped | [Scalability: Per-Node limits](11-scalability-and-distributed-state.md#per-node-limits) | Planned (M1) |
| Span queue | 8,192 spans (target) | Dropped with `queue_full` | [Observability: Telemetry pipeline](10-observability.md#telemetry-pipeline) | Planned (M1) |
| OTLP log queue | 8,192 records and 4 MiB (target) | Dropped with `queue_full` | [Observability: Telemetry pipeline](10-observability.md#telemetry-pipeline) | Planned (M1) |
| Process-log queue | 4,096 records and 1 MiB (target) | Dropped with `queue_full` | [Observability: Process logs](10-observability.md#process-logs) | Planned (M1) |
| Access-log queue | 8,192 records and a 4 MiB byte budget, each record at most 4 KiB (target) | Dropped with `queue_full`; a longer record is cut and flagged | [Observability: Access logs](10-observability.md#access-logs) | Planned (M1) |
| Concurrent `/metrics` encodings | 4 (target) | A fifth scrape waits up to 10 s, then gets 503 | [Observability: Telemetry pipeline](10-observability.md#telemetry-pipeline) | Planned (M1) |
| Node Plugin memory | Gateway `limits.maxPluginMemoryBytes`, default 2 GiB (target) | New instance refused: `RZ-PLG-004` | [WASM plugin system: Default limits](05-wasm-plugin-system.md#default-limits) | Planned (M2) |
| Compiled Plugin code per Node | 512 MiB (target) | RZ-CFG-028 when the Revision's code alone exceeds it | [WASM plugin system: Default limits](05-wasm-plugin-system.md#default-limits) | Planned (M2) |
| HTTP/3 connections and streams | Each QUIC connection takes 16 client connection slots, so at most 1,250; 32 bidirectional streams and a 1 MiB receive window per connection (target) | A new QUIC connection is refused before any stream, and its client stays on TCP | [Data plane: Bounded resources](03-data-plane.md#bounded-resources) | Planned (M3) |

Data plane sums the worst-case memory these ceilings allow at about 19 GiB plus snapshots on a 32 GiB Node (hypothesis); [Capacity planning](../operations/03-capacity-planning.md) sizes containers from it.

## Open questions

| ID | Question | Options | Owner | Blocking? |
|---|---|---|---|---|
| OQ-performance-budgets-and-benchmarking-11 | Should overload keep the fixed in-flight ceiling, or move to an adaptive limiter? | (a) Fixed in-flight ceiling (current); (b) an adaptive limiter | data-plane | No |
| OQ-performance-budgets-and-benchmarking-14 | Should listeners bound concurrent TLS handshakes, so a burst of new connections cannot slow established traffic? | (a) `GOMAXPROCS` at once (proposed); (b) none; (c) a listener field | data-plane | No |
