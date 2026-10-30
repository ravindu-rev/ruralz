# Performance Budgets and Benchmarking

## Summary

This document owns every Ruralz Performance Budget: the latency, throughput, allocation, memory, startup and Hot Reload ceilings that Ruralz Gateway and Ruralz Control must meet on stated reference hardware, each tagged (target) or (hypothesis). It fixes the per-stage latency budget of the Filter Chain, the reference hardware, the benchmark scenarios and open-loop method, the CI regression gates, the profiling playbook and the reports that tie budgets to the SLOs of [Observability](10-observability.md). Each budget becomes a gate in the milestone that ships its component, from Planned (M1). Contributors, architects and operators read it before changing a hot path, sizing a Node or publishing a number.

## Scope and non-goals

In scope: budget values, reference hardware, scenarios, load tools, measurement rules, regression thresholds, delegated constants, profiling and reporting. "Pack 8.7" names a section of the [foundation pack](../_meta/foundation-pack.md). Values here win on conflict with [System overview](01-system-overview.md), [Data plane](03-data-plane.md#performance-budgets), [WASM plugin system](05-wasm-plugin-system.md#performance-model), [Observability](10-observability.md) and [Scalability and distributed state](11-scalability-and-distributed-state.md).

Non-goals, with owners:

- SLO objectives, metric names and alert rules: [Observability](10-observability.md#slo-definitions).
- CI stages, jobs and gate placement: [Testing and quality strategy](../engineering/03-testing-and-quality-strategy.md#benchmarks-and-regression-gates), which uses the placements below.
- Sizing formulas: [Capacity planning](../operations/03-capacity-planning.md), from the results published here.
- Accuracy bounds of shared state: [Scalability and distributed state](11-scalability-and-distributed-state.md#consistency-and-accuracy-bounds).
- Kinds and fields: the [Configuration model](02-configuration-model.md); benchmark Bundles use only its fields.

## Budget philosophy

These rules turn P10 ([Vision and positioning](../vision/01-vision-and-positioning.md#principles)) into budgets a pull request can fail.

| ID | Rule | Consequence |
|---|---|---|
| BP-1 | A value is a (target) we commit to, a (hypothesis) an estimate rests on, or a cited measurement | The first benchmark that measures a hypothesis replaces it with the cited measurement, linking its published record; a measurement that breaks a target changes the design |
| BP-2 | Latency budgets count gateway-added time only: wall-clock time minus client, upstream and State Store I/O and declared remote calls ([Gateway-added time](10-observability.md#gateway-added-time)) | Upstream and State Store latency get separate budgets |
| BP-3 | A Performance Budget is the lab ceiling on the metric and threshold of a production SLO, where one exists | Each budget names that SLO, a related one, or none |
| BP-4 | Budgets compose: stage p50 values sum to at most the end-to-end p50, and stage p99 values plus a runtime reserve sum to at most the end-to-end p99 | Percentiles do not add, so the sums plan a budget and the end-to-end values gate it |
| BP-5 | Pay only for what is attached: an unsubscribed Phase costs one nil check ([System overview](01-system-overview.md#design-principles)) | Scenarios add one feature at a time |
| BP-6 | Latency holds at a stated load, never at saturation | Every latency budget names its offered rate |
| BP-7 | A budget gates from the milestone that ships its component; changing a value is a `perf` pull request here with benchmark evidence | Budgets move deliberately, never by drift |

### Seed targets

The binding seeds, from which the rest derives, are PB-1, PB-4, PB-5, PB-6 and PB-7:

| Seed | Value |
|---|---|
| Gateway-added p99, plain proxying at 10k rps per core | 1 ms or less (target) |
| Plain proxying on 4 vCPU | At least 50k rps (target) |
| WASM Plugin call, p99 | 50 µs or less (target) |
| Base RSS | 100 MB plus about 2 KB per Route (target) |
| Hot Reload of 5,000 Routes | 500 ms or less (target) |

### Budget catalog and SLO ties

| ID | Performance Budget | Value | Measured by | SLO in [Observability](10-observability.md#slo-definitions) | Milestone |
|---|---|---|---|---|---|
| PB-1 | Gateway-added p99, S1 at 10,000 rps per core on RH-1 | 1 ms or less (target) | Access-log `gateway_duration`; `ruralz_http_gateway_duration_seconds` | SLO-GW-2 | Planned (M1) |
| PB-2 | Gateway-added p99, S2 at half saturation (SM-4) | 1 ms or less (target) | Same | SLO-GW-2 | Planned (M1) |
| PB-3 | Gateway-added p50, S2 at half saturation (SM-5) | 150 µs or less (target) | Same | SLO-GW-3 | Planned (M1) |
| PB-4 | S1 saturation throughput on 4 vCPU | 50,000 rps or more (target) | Rate ladder | None | Planned (M1) |
| PB-5 | Plugin Phase call overhead, pooled, p99 (SM-6) | 50 µs or less (target) | `ruralz_plugin_call_overhead_seconds` | SLO-GW-4 | Planned (M2) |
| PB-6 | Base RSS with R Routes in the active Revision | 100 MB + 2 KB × R or less (target) | RSS after idle settle | None | Planned (M1) |
| PB-7 | Hot Reload of 5,000 Routes on an idle Node | 500 ms or less (target) | `ruralz_config_activation_duration_seconds{stage="total"}` | Related: SLO-GW-6, compile stage only | Planned (M1) |
| PB-8 | Ruralz-owned allocations per S1 request, `net/http` excluded | 30 or fewer (target) | `testing.AllocsPerRun` over one whole S1 request ([alloc/op gate](#regression-policy-and-gates)) | None | Planned (M1) |
| PB-9 | Same-zone GCRA round trip p99, S5 | 1 ms or less (hypothesis) | `ruralz_state_call_duration_seconds{op="gcra",zone="same"}` | SLO-GW-5 | Planned (M1) |
| PB-10 | Telemetry CPU at defaults, half saturation | 5% or less of Node CPU (target) | S2 with telemetry on and off ([Scenarios](#scenarios)), plus the measured per-request CPU of metric recording | Related: SLO-GW-7 watches drops | Planned (M1) |
| PB-11 | Goodput in O1b at 150% offered load | 90% or more of saturation, excess only as `RZ-RT-005` (target) | Generator; `ruralz_http_node_responses_total` | Related: SLO-GW-1, which `RZ-RT-005` burns | Planned (M1) |
| PB-12 | Gateway-added time to first token, bodies up to 16 KiB, p99, excluding State Store, embedding, provider and estimator time | 1 ms or less (target) | `ruralz_http_gateway_duration_seconds` in S6n, ending at header commit | None (OQ-performance-budgets-and-benchmarking-9) | Planned (M3) |
| PB-13 | Rollout convergence, 100 Nodes, `all-at-once`, p95 (SM-9) | 30 s or less (target) | `ruralz_control_rollout_duration_seconds` | SLO-CP-1 | Planned (M2) |
| PB-14 | Cross-zone GCRA round trip p99, S5x | 2 ms or less (hypothesis) | `ruralz_state_call_duration_seconds{op="gcra",zone="cross"}` | None: SLO-GW-5 counts only `zone="same"` calls | Planned (M1) |
| PB-15 | Gateway-added p99, S2 at 60% of saturation | 1 ms or less (target) | As PB-2 | SLO-GW-2 | Planned (M1) |
| PB-16 | Gateway-added p99, S2 at 80% of saturation | 2 ms or less (target) | As PB-2 | Related: SLO-GW-2 burns (OQ-performance-budgets-and-benchmarking-13) | Planned (M1) |

PB-12 restates the [AI/LLM gateway](06-ai-llm-gateway.md#streaming) value, PB-13 [SM-9](../vision/01-vision-and-positioning.md#success-metrics), and PB-14 the [cross-zone round trip](11-scalability-and-distributed-state.md#latency-budgets) paid outside the primary's zone. PB-15 covers the four-zone [autoscaling threshold](11-scalability-and-distributed-state.md#autoscaling-signals), PB-16 zone-loss survivors, whose load burns SLO-GW-2.

## Per-stage latency budget

The table splits PB-2 and PB-3 (S2), and PB-1 (S1, without the S2 Filters), into gateway-added CPU microseconds on RH-1 at half saturation, excluding blocked time; stage names follow pack 2's spans. The `onRoute`, `onUpstreamResponseHeaders` and `onResponse` rows are built-in engine work at that Phase boundary, not Filter work. A Route above the 400 µs stage ceiling (target) gets its own measured budget.

| Stage | Work in S2, or when attached | p50 µs | p99 µs | Ruralz-owned allocations |
|---|---|---|---|---|
| Header read and parse | HTTP/1.1 in `net/http` ([ADR-0009](../adr/0009-http-stack-net-http-quic-go.md)) | 12 (target) | 50 (target) | 0, `net/http` internals excluded (target) |
| Snapshot pin and Route match | Pin stripe, host table, path trie | 3 (target) | 12 (target) | 0 (target) |
| `onRequestHeaders`: `auth.jwt` | RS256 with a 2,048-bit key from the cached JWKS | 45 (target) | 100 (target) | Reported, not gated (hypothesis) |
| `onRequestHeaders`: `ratelimit` | Local token bucket shard, then GCRA on the `memory` driver ([ADR-0008](../adr/0008-rate-limiting-local-bucket-and-gcra.md)) | 2 (target) | 8 (target) | 0 (target) |
| `onRequestHeaders`: other Filters | `cors`, `authz.cel` or `authz.ip`, when attached | 2 each (target) | 5 each (target) | 0 each (target) |
| `onRequestBody` | One nil check; `validation.json-schema` on 1 KiB when attached | 0; 25 attached (target) | 0; 80 attached (target) | 0 (target) |
| `onRoute` | Single Upstream leg selected | 1 (target) | 4 (target) | 0 (target) |
| `onUpstreamRequest` | None; `headers` setting two values when attached | 0; 2 attached (target) | 0; 8 attached (target) | 0; 2 attached (target) |
| Upstream layer | Endpoint pick, breaker, pooled connection, request write; a pooled attempt context and a copy-free forwarding writer | 12 (target) | 50 (target) | 6 (target) |
| `onUpstreamResponseHeaders` | Breaker and retry accounting | 2 (target) | 8 (target) | 0 (target) |
| `onUpstreamResponseBody` | None | 0 (target) | 0 (target) | 0 (target) |
| Transform Policy, any Phase | None; when attached, `transform.request` or `transform.response` running the [`orders-shape`](03-data-plane.md#transform-policies) example on 1 KiB, in `onRequestBody`, `onUpstreamRequest`, `onUpstreamResponseBody` or `onResponse` | 0; 30 attached (target) | 0; 100 attached (target) | 0 for the literal operations, `remove[]`, `replace[]` and `arrayOps[]`, over pooled buffers (target); CEL entries reported, not gated |
| `onResponse` | Header finalization; `cors` response side when attached | 2 (target) | 8 (target) | 2 (target) |
| Response write | 1 KiB through the 32 KiB buffer | 8 (target) | 30 (target) | 2 (target) |
| `onLog` | Metrics, trace decision, one access-log record ([ADR-0018](../adr/0018-telemetry-opentelemetry-prometheus-exporter.md)) | 5 (target) | 15 (target) | 10: 0 metrics, 2 tracing, 8 access log (target) |
| `onChunk` | One built-in Filter per chunk, such as the Token Budget guard and its count | 3 per chunk (target) | 15 per chunk (target) | 0 (target) |
| Streaming chunk, all-in (S6) | Upstream read and wakeup 3 (hypothesis), SSE parse 1, `onChunk` 3, client write and flush 3 (hypothesis), bookkeeping 2 | 12 per chunk (target) | 40 per chunk (target) | 0 (target) |
| Token estimate (S6) | `tiktoken-go/tokenizer`, M3 tokenizer benchmark | 25 per KiB (hypothesis) | 50 per KiB (hypothesis) | Reported |
| `RZ-RT-005` rejection (O1) | Header decode, admission check, 503 write; mean CPU 16 or less, component benchmark (target) | 12 (target) | 40 (target) | 2 (target) |
| Plugin Phase call | S4, pooled, deadline interruption on ([ADR-0004](../adr/0004-wasm-runtime-wazero.md), [ADR-0005](../adr/0005-plugin-abi-v1.md)) | 20 overhead (target) | 50 overhead, 100 with guest logic (target) | Buffer copies (target) |
| Go runtime reserve | GC assists, scheduler delay and queueing | 58 (target) | 715 (target) | Not applicable |
| Total, S2 | Sum of the S2 rows plus the reserve | 150 (target) | 1,000 (target) | S1 rows sum to 20 of PB-8's 30 (target) |

Rules for the table:

1. The S2 stage p99 values sum to 285 µs (target); the sum MUST stay at or below 400 µs (target), so the reserve keeps at least 600 µs (target) for the Go tail latency that [Vision and positioning](../vision/01-vision-and-positioning.md#where-ruralz-does-not-lead) names as a risk.
2. The TLS 1.3 handshake costs 1 ms or less of CPU at p99 with ECDSA P-256 (target), measured in S3 and C2.
3. Restated: CEL expressions under 2 µs at p99 (target) ([Configuration model](02-configuration-model.md#limits)); `authz.opa` and `authz.cedar` under 100 µs (hypothesis), Planned (M2) ([Security and identity](08-security-and-identity.md)).

```text
S1 stage sums: 45 µs at p50 and 177 µs at p99, leaving PB-1 823 µs of reserve          (target)
RS256: about 40 µs of CPU, measured in M1; algorithm: OQ-performance-budgets-and-benchmarking-8 (hypothesis)
Token estimate of a 16 KiB prompt: about 400 µs, which is why PB-12 excludes it        (hypothesis)
```

*Figure 1: the S2 per-stage latency budget in microseconds at p99, with the Go runtime reserve that closes PB-2.*

```mermaid
flowchart TD
    a["Header read and parse: 50 µs p99 (target)"]
    b["Snapshot pin and Route match: 12 µs p99 (target)"]
    c["onRequestHeaders, auth.jwt RS256: 100 µs p99 (target)"]
    d["onRequestHeaders, ratelimit local bucket: 8 µs p99 (target)"]
    e["onRequestBody, not subscribed: 0 µs (target)"]
    f["onRoute: 4 µs p99 (target)"]
    g["onUpstreamRequest, no leg Policies: 0 µs (target)"]
    h["Upstream layer, upstream I/O excluded: 50 µs p99 (target)"]
    i["onUpstreamResponseHeaders: 8 µs p99 (target)"]
    j["onUpstreamResponseBody, not subscribed: 0 µs (target)"]
    k["onResponse: 8 µs p99 (target)"]
    l["Response write, 1 KiB: 30 µs p99 (target)"]
    m["onLog, metrics, trace decision, access log: 15 µs p99 (target)"]
    s["Stage sum: 285 µs p99, ceiling 400 µs (target)"]
    r["Go runtime reserve: 715 µs p99 (target)"]
    t["PB-2 total gateway-added: 1,000 µs p99, PB-3 150 µs p50 (target)"]
    a --> b --> c --> d --> e --> f --> g --> h --> i --> j --> k --> l --> m --> s
    s --> t
    r --> t
```

## Throughput and resources per core

### Reference hardware

Latency, throughput and memory budgets gate on RH-1, plus RH-2 from M2, except idle RSS (stage 9, shared runners, both architectures) and fan-out and accounting rows PB-13, F2 and SM-10 (Testing's dedicated general runners).

| ID | Role | Specification | Use |
|---|---|---|---|
| RH-1 | Reference machine | Bare-metal linux/amd64, one socket, 16 or more physical cores at a fixed 3.0 GHz or more, turbo, deep C-states and SMT off, 64 GiB of RAM, a 25 Gbit/s NIC, Debian stable with its longterm kernel (target) | `ruralzd` in a cgroup v2 cpuset of 4 cores on the NIC's NUMA node, `GOMAXPROCS=4`; interrupts elsewhere; the runner and the harness |
| RH-1L, RH-1U, RH-1S | Load generator; mock and Collector sink; State Store | Same specification and switch; round trip p99 of 50 µs or less (hypothesis) | RH-1L runs the load tools; RH-1U the mock Upstream, the mock IdP and the OTLP sink; RH-1S one Valkey primary in a 4-core cpuset (target) and S5x's fault proxy |
| RH-2 | linux/arm64 ([tech stack](../engineering/01-tech-stack-and-libraries.md#static-builds)) | Same rules | Reported once provisioned; gates from M2 |
| RH-3 | Reproduction profile | A cloud VM with 4 dedicated vCPUs for the Node (target), plus helper VMs for the load tools, the mock and sink, and the State Store | Published, never gated; one RH-3 run publishes with `0.1.0` |

RH-1 is rented bare metal meeting this specification; Revington-owned hardware stays the long-term home. `deploy/benchhost` holds provider-agnostic, idempotent setup scripts that run over SSH on each host, one inventory file per profile (RH-1, RH-3 and loopback) and a runbook; the owner rents the machines, registers the runner and sets the repository variable `RH1_PROVISIONED` to `true`. The Latency job, the release soak job and the [weekly `chaos-scale.yml`](../engineering/03-testing-and-quality-strategy.md#chaos-testing) run on `rh-1`, serialized through the `rh-1` concurrency group, once `RH1_PROVISIONED` is `true`; until then each fails with an "RH-1 not provisioned" annotation and no RH-1 budget is measured.

The `rh-1` runner is an ephemeral just-in-time runner on RH-1, outside `ruralzd`'s cpuset: a service on RH-1 holds a GitHub App's key, registers one runner at repository level through the App, lets it take one job, then registers the next. If the repository moves to an organization, the runner joins an organization runner group limited to this repository. It runs only jobs of protected branches and tags (scheduled, dispatched, or called by `release.yml`), never pull-request code: every `rh-1` job first checks that its ref is protected and its event is not `pull_request`, and fork workflows of every outside contributor need approval ([Repository layout](../engineering/02-repository-layout-and-conventions.md)). The App key never leaves RH-1.

The harness runs on RH-1 and drives the other hosts over SSH, as a dedicated user whose key `deploy/benchhost` provisions. For each run it builds the test-support helper `test/bench/labhost`, which serves the mock Upstream, the mock IdP and the OTLP sink on RH-1U, runs `internal/testkit/loadgen` on RH-1L and the fault proxy on RH-1S; it copies the helper and the load tools to their hosts and starts each in a `systemd-run` scope on its cpuset. Benchmark runs use the release build of `ruralzd` and real time by design, never the test-only build whose clock a correctness suite controls ([Hermetic rule scope](../engineering/03-testing-and-quality-strategy.md#hermetic-rule-scope)). `chaos-scale.yml` drives the same hosts the same way: its Nodes run on RH-1, each with `GOMAXPROCS=1` in one cpuset they share, and RH-1S holds a Valkey 9 primary with its replica and sentinel, or three primaries with one replica each for CE-5's `cluster` run ([Scale profiles](../engineering/03-testing-and-quality-strategy.md#scale-profiles)).

Every host runs one settings baseline, which `deploy/benchhost` applies and each record names; changing a value is a `perf` pull request here:

| Setting | Value | Reason |
|---|---|---|
| Operating system | Debian stable with its longterm kernel on all four hosts; packages pinned, automatic updates off, updates applied only in a recorded maintenance window | A night's A/B runs share one kernel, and every record names it |
| CPU | SMT off (`nosmt`); turbo off; the `performance` governor with the minimum frequency at the fixed maximum, 3.0 GHz or more (target); idle states deeper than C1 disabled | RH-1's specification: CPU per request holds still between runs |
| cpusets (cgroup v2) | RH-1: `ruralzd` on 4 cores of the NIC's NUMA node, the runner and the harness on other cores; RH-1L: the load tools on 8 cores; RH-1U: the mock on 8 cores, the OTLP sink and the IdP on 4; RH-1S: Valkey on 4 cores, the fault proxy on 2 (target) | No measured process shares a core, and the generator and mock stay below 70% CPU at S1 saturation (hypothesis) |
| Interrupts | `irqbalance` off; NIC queue interrupts pinned to cores outside each host's measured cpusets, on the NIC's NUMA node | Interrupt work never lands on `ruralzd`'s cores |
| NIC | Ring buffers at the NIC's maximum; adaptive interrupt coalescing off, `rx-usecs` and `tx-usecs` 0 | Bursts never overflow a ring, and no packet waits for a coalescing timer |
| sysctls | `net.ipv4.ip_local_port_range` 1024 65535; `net.core.somaxconn` and `net.ipv4.tcp_max_syn_backlog` 65535; `net.core.netdev_max_backlog` 65536; `net.ipv4.tcp_tw_reuse` 1 on RH-1 and RH-1L; `vm.swappiness` 0 with swap off (target) | O1a's 20,000 upstream connections leave 20,000 ports free, and C2's 10,000 handshakes in 2 s fit the accept queues (target); reconnects reuse `TIME_WAIT` ports; RSS never counts swapped pages |
| File descriptors | `LimitNOFILE` 1,048,576 for every benchmark process (target) | C1, O1 and O2 hold tens of thousands of connections per process (target) |
| Transparent huge pages | `madvise` | RSS stays comparable with common production settings |
| Valkey on RH-1S | The `valkey/valkey` 9.0.7 image pinned in `internal/statestore/statestoretest/redisserver/images.go`, started empty by the harness for each run, with host networking, `io-threads` 4, persistence off, `maxclients` 20,000 (target), TLS from the run's CA and one ACL user | S5, S5x, O2 and G1 measure one shard; G1's 1,000-Node variant opens 10,000 connections (target); off loopback a Node accepts only TLS with credentials (RZ-CFG-042) |

Results record the CPU microarchitecture and never compare across generations: Green Tea GC gains about 10% more on Ice Lake and Zen 4 or newer ([source](https://go.dev/doc/go1.26)). With SMT off, "per core" and "per vCPU" name one figure. The seeds measure 4 vCPU together:

```text
10,000 rps per core = 40,000 rps on the 4-core cpuset, not a single-core Node          (target)
A single core at 80% utilization would push p99 past 1 ms from queueing alone          (hypothesis)
```

### Throughput targets

Saturation is the highest open-loop step whose achieved rate stays within 0.5% of the offered rate, with end-to-end p99 of 10 ms or less and errors at 0.01% or less (target).

| Scenario | Saturation on 4 vCPU | Per vCPU | Latency point | Milestone |
|---|---|---|---|---|
| S1 plain proxying | 50,000 rps or more (target), PB-4 | 12,500 rps or more (target) | PB-1 at 40,000 rps (target) | Planned (M1) |
| S2 reference scenario | 32,000 rps or more (target) | 8,000 rps or more (target) | PB-2, PB-3 at 16,000; PB-15 at 19,200; PB-16 at 25,600 rps (target) | Planned (M1) |
| S3 HTTP/2 over TLS 1.3 | 28,000 rps or more (target) | 7,000 rps or more (target) | 1 ms p99 at half saturation (target) | Planned (M1); HTTP/3 variant Planned (M3) |
| S4 one header-only Plugin | 36,000 rps or more (target) | 9,000 rps or more (target) | PB-5 at half saturation (target) | Planned (M2) |
| S5 GCRA in a `redis` State Store | 40,000 rps or more (target) | 10,000 rps or more (target) | PB-9 at half saturation (hypothesis) | Planned (M1) |
| S6 AI streaming | 1,000 streams at 100 chunks per second on 1.2 vCPU or less (target) | Reported | PB-12 in S6n (target) | Planned (M3) |
| S7 gRPC unary (`connectrpc.com/connect`) | 30,000 rps or more (target) | 7,500 rps or more (target) | 1 ms p99 at half saturation (target) | Planned (M3) |
| S8 event ingress | Reported | Reported | Set with the event protocols | Planned (M4) |

```text
S4 per 10,000 rps: 0.8 vCPU S1 work + 0.2 Phase call overhead + 0.1 guest logic = 1.1 vCPU (target)
                   4 vCPU / 1.1 × 10,000 ≈ 36,000 rps (target)
S6: 1,000 streams × 100 chunks/s × 12 µs = 1.2 vCPU (target); an onChunk Plugin adds 20 µs per chunk (target)
S3 per request: S2's 125 µs + TLS 1.3 record encrypt and decrypt 4 µs     (hypothesis)
                + HTTP/2 framing, HPACK and flow control 5 µs            (hypothesis)
                + stream handoff and serialized writes 8 µs              (hypothesis)
                = 142 µs; multiplexing's saved syscalls are not credited             (hypothesis)
                4 vCPU / 142 µs ≈ 28,169 → 28,000 rps or more, below S2's 32,000     (target)
```

G1 measures the per-shard GCRA figure behind Scalability's [Cell sizing](11-scalability-and-distributed-state.md#cell-sizing), about 100,000 calls per second (hypothesis): no published EVAL throughput exists, and Valkey 8's 1.19 million `SET` per second is a different command mix ([source](https://valkey.io/blog/unlock-one-million-rps-part2/)). Under P10, S1's 12,500 rps per vCPU (target) becomes a claim only when a published run meets it.

### Resources per core

At the S1 latency point of 40,000 rps on RH-1 (target):

| Resource | Budget |
|---|---|
| CPU per 10,000 rps, S1 | 0.8 vCPU or less (target) |
| CPU per 10,000 rps, S2 | 1.25 vCPU or less (target) |
| CPU per 10,000 rps, S3 | 1.42 vCPU or less (target) |
| Garbage collector CPU share | 10% or less of Node CPU at `GOGC=100` (target) |
| Telemetry CPU at defaults | 5% or less of Node CPU, PB-10 (target) |
| Loaded RSS with 256 keep-alive connections | 256 MiB or less (target) |
| Upstream requests on pooled connections | 99.9% or more after warm-up (target) |
| New TLS 1.3 handshakes, ECDSA P-256 | 2,000 or more per second per vCPU (hypothesis) |

The GC share comes from `ruralz_runtime_gc_cpu_seconds_total` and a macro run's alloc/op from `ruralz_runtime_allocations_total` ([Observability](10-observability.md#ruralz-gateway-metrics)), both over the measured window.

## Memory budget

RSS comes from cgroup v2 accounting in RH-1 records, as `memory.stat` `anon` plus `file_mapped`, the cgroup counterpart of `VmRSS`, with `memory.current` recorded beside it; the stage 9 gate on shared runners reads `VmRSS`, and live heap comes from `ruralz_runtime_heap_bytes`. "Idle settle" means no connections for 120 s (target); at `GOGC=100` the runtime keeps freed heap up to about the heap goal, twice the live heap (hypothesis), so each RSS slope is twice its live slope.

| Component | Budget | Basis |
|---|---|---|
| Idle RSS, no Revision, telemetry at defaults | 89 MiB or less (target) | Fits PB-6 with every sharded label set (below); [Tech stack](../engineering/01-tech-stack-and-libraries.md#library-catalog) states the same budget |
| Base RSS with R Routes (PB-6) | 100 MB + 2 KB × R or less (target) | Seed; 100 to 17,000 Routes |
| Sharded label sets, 4 stripes | Up to 5.4 MiB of RSS, 2.7 MiB live (target): listener and enumeration 0.6 MiB, Gateway Policies 0.9 MiB, 2 KiB per Route and per Upstream for the first 1,000 of each | Half of [Observability](10-observability.md#overhead-per-signal)'s 8-stripe figures (hypothesis), OQ-performance-budgets-and-benchmarking-10 |
| Live heap per Route | 1 KB or less: compiled snapshot 0.5 KB, unsharded label sets 0.5 KB (target) | A histogram label set and a counter series, 300 and 120 bytes (hypothesis) |
| RSS slope, 1,000 to 17,000 Routes | 2 KB or less per Route, twice the live slope (target) | Fitted over R1 |
| Peak configuration memory during a Hot Reload | (K + 2) × the snapshot, K = 2 (target) | [Data plane](03-data-plane.md#configuration-snapshots-and-hot-reload) hypothesis; R1 |
| Telemetry | 40 MiB or less live, up to 80 MiB of RSS (target) | Observability |
| Idle cleartext or pooled upstream connection | 24 KiB or less (target) | About 24 KB per idle Go WebSocket, derived ([source](https://www.freecodecamp.org/news/million-websockets-and-go-cc58418460bb/)) |
| Idle TLS connection; WebSocket session, Planned (M3) | 96 KiB or less (target) | 20,000 fit in about 1.9 GiB (target) |
| In-flight request | Header block plus 64 KiB of copy buffers (hypothesis) | [Data plane](03-data-plane.md#bounded-resources); O1 |
| Loaded RSS, S1 at 40,000 rps, 256 connections | 256 MiB or less (target) | Sum below |
| Container memory for 20,000 TLS connections | 2.2 GiB or more (target), without Plugins or full state tables | 20,000 × 96 KiB plus 256 MiB |
| Plugin memory; state tables at their ceilings | 2 GiB default cap, compiled code 512 MiB or less; about 475 MiB (target) | [WASM plugin system](05-wasm-plugin-system.md#default-limits); [Scalability](11-scalability-and-distributed-state.md#per-node-limits) |
| Worst-case Node | About 19 GiB plus snapshots, fitting 32 GiB (hypothesis) | Data plane; the sum of measured per-ceiling results (target) |

PB-6 closes at every checked size, with at least 1 MB of margin (target):

```text
PB-6 allowance      100,000,000 + 2,000 × R bytes                               (target)
Idle base           89 MiB = 93,323,264 bytes                                   (target)
Fixed sharded sets  0.6 + 0.9 MiB = 1,572,864 bytes; base B = 94,896,128        (target)
Route sharded sets  2,048 × min(R, 1,000) bytes                                 (hypothesis)
Upstream sharded    2,048 × min(R/10, 1,000) bytes, R/10 Upstreams in the ladder (hypothesis)
RSS slope           2 × (500 snapshot + 500 label sets) = 2,000 bytes per Route  (target)
R = 0        B                                                =  94,896,128 <= 100,000,000 (target)
R = 1,000    B + 2,048,000 +   204,800 +  2,000,000           =  99,148,928 <= 102,000,000 (target)
R = 10,000   B + 2,048,000 + 2,048,000 + 20,000,000           = 118,992,128 <= 120,000,000 (target)
R = 17,000   B + 2,048,000 + 2,048,000 + 34,000,000           = 132,992,128 <= 134,000,000 (target)
```

Budget runs use `GOGC=100` without `GOMEMLIMIT`, as the M1 telemetry benchmarks assume ([source](https://go.dev/doc/gc-guide)). An informational run in slot 3 holds C1 over TLS plus S2 in a 2.5 GiB container with `GOMEMLIMIT` at 90% (target), so the limit binds, and reports GC CPU.

The 89 MiB idle RSS budget (target) fits the 100 MB seed with every sharded label set; the loaded-RSS row and the block below check that the seeds fit together. Observability's 50,000 rps per Node (hypothesis) matches PB-4. A documented `GOMEMLIMIT` stays deployment guidance ([Scalability](11-scalability-and-distributed-state.md#scale-unit)).

```text
Loaded RSS, S1 at 40,000 rps on 4 vCPU: 89 MiB idle + 80 MiB telemetry               (target)
  + 6 MiB for 256 client + 6 MiB for 256 upstream connections                         (target)
  + 75 MiB in-flight buffers and GC headroom = 256 MiB                                 (target)
```

## Startup and reload time by configuration size

The ladder uses synthetic Bundles of R Routes, R/10 Upstreams and R/20 Policies plus one Gateway, no Plugins, under the 20,000-resource default limit (target) of the [Configuration model](02-configuration-model.md#restricted-yaml-profile). `test/bench/gen` generates them at run time, deterministically, and a test pins the two Revision digests of each size. Their shape:

- The Gateway attaches S2's `jwt-bench` and `ratelimit-bench` ([Scenarios](#scenarios)), two of the R/20 Policies; the other R/20 − 2 are `authz.cel` Policies whose `rule` is `request.method == "GET"`.
- Route i, for i from 0 to R − 1, has no `match.hosts`; an even i matches the exact path `/r/<i>`, an odd i the template path `/r/<i>/{id}`. Route 0 is S1's `bench-1k` Route instead, on Upstream 0 and with `spec.excludePolicies` naming both Gateway Policies, so the loaded runs offer S1's workload to it.
- Route i with i mod 20 = 19 attaches `authz.cel` Policy number ((i − 19) / 20) mod (R/20 − 2), round robin.
- Route i uses Upstream i mod R/10, and each Upstream has one Endpoint on the mock.

R1 alternates two Revisions: every Route's `timeout` is 5 s in one and 6 s in the other, and each Upstream j with j mod 10 = 0 moves its Endpoint between the mock's ports 8080 and 8081, so every Route recompiles and the swap warms R/100 new pools. The loaded runs offer S1's workload at 25,000 rps, half of PB-4 (target).

Snapshot live heap is the difference of the live heap across an activation, each value read after a forced collection (`/debug/pprof/heap?gc=1`, totals only). Peak configuration memory is cgroup v2 `memory.peak`, reset before each reload, above the RSS before it.

| Routes (resources) | Cold start to `/readyz` 200, rendered YAML | Cold start from Last-Known-Good | Hot Reload, idle Node | Snapshot live heap | Peak configuration memory |
|---|---|---|---|---|---|
| 100 (116) | 300 ms or less (target) | 200 ms or less (target) | 50 ms or less (target) | 50 KB or less (target) | 200 KB or less (target) |
| 1,000 (1,151) | 600 ms or less (target) | 300 ms or less (target) | 125 ms or less (target) | 500 KB or less (target) | 2 MB or less (target) |
| 5,000 (5,751) | 1.6 s or less (target) | 700 ms or less (target) | 500 ms or less, PB-7 (target) | 2.5 MB or less (target) | 10 MB or less (target) |
| 10,000 (11,501) | 3 s or less (target) | 1.3 s or less (target) | 1 s or less (target) | 5 MB or less (target) | 20 MB or less (target) |
| 17,000 (19,551) | 5 s or less (target) | 2.2 s or less (target) | 1.7 s or less (target) | 8.5 MB or less (target) | 34 MB or less (target) |

Cold start counts process start and listener binding, 150 ms or less (target), plus loading; a Last-Known-Good start re-runs checks from reference resolution ([Configuration model](02-configuration-model.md#validation-and-diff-semantics)). The pack 8.2 boot wait and external secret providers are excluded.

### Hot Reload stages

The stages match the `stage` label of `ruralz_config_activation_duration_seconds` and the steps of [Compile before swap](01-system-overview.md#compile-before-swap). For 5,000 Routes (target), idle RH-1 Node:

| Stage | Work | Budget |
|---|---|---|
| `verify` | Full sha256 digest and signature check | 25 ms or less (target) |
| `compile` | Decode and re-validate, about 40 µs per resource (hypothesis); build Routers, Filter Chains and CEL at 100 µs or less per Route (target); resolve changed secrets | 400 ms or less (target) |
| `plugin_compile` | None in the ladder | 0 ms (target) |
| `swap` | Warm the 50 new pools, then one atomic pointer store | 25 ms or less (target) |
| `total` | PB-7, with 50 ms of margin (target) | 500 ms or less (target) |

```text
compile, 5,000 Routes:   5,751 × 40 µs + 5,000 × 100 µs = 730 ms of work / W = 2 ≈ 365 ms   (target)
one core, 10,000 Routes: 11,501 × 40 µs + 10,000 × 100 µs ≈ 1.46 s, inside SLO-GW-6's 2 s (target)
under S1 at half saturation, 5,000 Routes: reload 1 s or less (target),
                         gateway-added p99 1.5 ms or less (hypothesis)
```

The loader's W = max(1, `GOMAXPROCS`/2) workers yield at least every 100 µs of work (target), so requests never wait behind a compile slice. A 2 MiB Plugin's cold compile takes up to 1 s on one core (hypothesis) ([WASM plugin system](05-wasm-plugin-system.md#performance-model)), so `plugin_compile` is outside PB-7 and SLO-GW-6.

### Control-plane scale timing

[Control plane and GitOps](04-control-plane-and-gitops.md#heartbeat-and-backpressure) sets these values:

| Measurement | Budget | Verified by |
|---|---|---|
| SM-9 Rollout convergence, 100 Nodes (PB-13) | 30 s or less at p95 (target) | F1 |
| Largest batch of a 10,000-Node Cluster | 5 minutes or less (target) | F2 |
| Control Streams per replica before shedding | 5,000 (hypothesis) | F2 |
| Snapshots per second per replica | 20 (hypothesis) | F2 |
| All Nodes reconnected after every replica restarts ([CE-10](11-scalability-and-distributed-state.md#chaos-experiments)) | 5 minutes or less (target) | CE-10 at 10 Nodes in Testing's Chaos job; F2 at 10,000 |

## Benchmark methodology

### Scenarios

Each scenario runs one Revision rendered from a checked-in Bundle under `test/bench/scenarios/`, so equal digests mean equal configuration (pack 8.1). The S2 Bundle below is that Bundle's base byte for byte, which a test keeps equal. Each host profile (RH-1, RH-3 and loopback) adds one Environment overlay holding its hosts' addresses, its TLS file paths, fixed under a fixed `RURALZ_SECRET_ROOT`, and `stateStore.tls`, so a profile's digests stay equal across runs; the loopback profile's Node also runs with `RURALZ_FETCH_ALLOW` naming the loopback addresses. A stage 5 test validates every scenario Bundle and overlay and pins each profile's Revision digests.

The harness starts `ruralzd` with `RURALZ_ZONE=lab-a`, and each profile's overlay maps, in `stateStore.zones`, Valkey's host to `lab-a` and S5x's fault proxy's host to `lab-b`. S5 and O2 calls then count as `zone="same"`, for PB-9, and S5x calls as `zone="cross"`, for PB-14 ([State client](11-scalability-and-distributed-state.md#state-client-and-rz-sts-error-codes)).

Telemetry stays at defaults, with `traceSampling` 0.01 set explicitly (default: OQ-observability-1), OTLP export to RH-1U and every request access-logged. PB-10's telemetry-off run sets no `telemetry.otlp`, `traceSampling: 0` and `accessLog.when: "false"`; metric recording has no switch, so PB-10 adds the per-request CPU that the metrics-recording component benchmark measures ([Suite layers](#suite-layers)).

| ID | Workload | Filter Chain | Protocol | Milestone |
|---|---|---|---|---|
| S1 | GET, 1 KiB response from the mock, 256 keep-alive connections (target) | None | HTTP/1.1 on 8080 | Planned (M1) |
| S2 | As S1; the SM-4 and SM-5 reference scenario ([Vision](../vision/01-vision-and-positioning.md#success-metrics)) | `auth.jwt` and `ratelimit` at the Gateway, `memory` driver | HTTP/1.1 on 8080 | Planned (M1) |
| S3 | As S2 with 64 connections of up to 64 streams (target) | As S2 | HTTP/2 over TLS 1.3 on 8443 | Planned (M1); HTTP/3 Planned (M3) |
| S4 | As S1 | A header-only Rust PDK Plugin in `onRequestHeaders` | HTTP/1.1 | Planned (M2) |
| S5 | As S1 | `ratelimit` on the `redis` driver, Valkey on RH-1S | HTTP/1.1 | Planned (M1) |
| S5x | As S5, 500 µs ± 100 µs of normal delay added each way on RH-1S (target) by the Ruralz fault proxy | As S5, with `stateStoreTimeout: 10ms` (target) | HTTP/1.1 | Planned (M1) |
| S6 | Mock `openai` provider, 100 chunks per second on 1,000 streams, 16 KiB prompts (target); passes only with zero `RZ-UP-006` | `ai.token-budget`, `memory` driver | SSE | Planned (M3) |
| S6n | S6 without `ai.token-budget`, `limits.maxInputTokens` or a `cost` strategy, so no estimate runs (hypothesis) | None | SSE | Planned (M3) |
| S7 | Unary calls with a 1 KiB message (target) | None | gRPC on 8443 | Planned (M3) |
| S8 | Kafka, NATS and MQTT ingress | None | Event protocols | Planned (M4) |

| ID | Workload | Pass criteria | Milestone |
|---|---|---|---|
| O1 | S3's chain on 120 connections of 250 streams (target). O1a: mock delay 1 s, 30,000 rps, 150% of what the in-flight ceiling admits. O1b: no delay, 42,000 rps, 150% of S3 saturation | Admitted plus `RZ-RT-005` within 0.5% of offered; zero `RZ-UP-006`; active requests plateau at 20,000; O1a's rejections at an internal p99 of 1 ms or less; O1b meets PB-11 (target) | Planned (M1) |
| O2 | S5 on 12,000 of C1's connections at 24,000 rps (target), `stateStoreTimeout: 1s`; a fault proxy delays every State Store reply 500 ms, without errors | Calls in flight, read from `ruralz_state_inflight_calls`, plateau at 8,192; the excess is skipped, counted in `ruralz_state_calls_total`; goroutines stay below one per open client connection, two per open upstream connection, 8,192 for the call ceiling and 1,000 more (target); skipped requests' internal p99 is at most 1 ms above that of the same night's S5 run (target) | Planned (M1) |
| C1 | 20,000 idle connections (target), cleartext and TLS runs | Within the connection memory rows | Planned (M1) |
| C2 | S3 at half saturation, then 10,000 new TLS connections within 2 s (target) | The existing traffic's external p99 of 5 ms or less; a handshake p99 of 1 s or less in `ruralz_listener_tls_handshake_duration_seconds` (target), OQ-performance-budgets-and-benchmarking-14 | Planned (M1) |
| R1 | The reload ladder, idle and under S1 at half saturation | Size-class and stage tables | Planned (M1) |
| G1 | Valkey on RH-1S, open-loop GCRA `EVALSHA` calls through the State client, keys cycling S2's 100,000 values: steps of 5,000 calls per second (5% of the 100,000 hypothesis) from 50,000, 60 s each, until p99 exceeds 1 ms or script CPU reaches 70% (target); a variant opens 10 connections per emulated Node for 1,000 Nodes | Per-shard calls per second at 70% script CPU, interpolated between steps; script CPU is Valkey's main-thread CPU per 1 s window (`INFO cpu` `used_cpu_sys_main_thread` plus `used_cpu_user_main_thread` deltas), recorded with `INFO commandstats` `cmdstat_evalsha` µs per second as an ungated breakdown (target) | Planned (M1) |
| F1 | SM-9: 100 `ruralzd` processes (target), three Ruralz Control replicas | PB-13 | Planned (M2) |
| F2 | A Control Stream driver emulating 10,000 Nodes (mTLS, 15 s heartbeats (target), ACK after simulated activation) against three replicas; then one replica stops, leaving two at 5,000 each (target); then one replica takes streams until it sheds | [Control-plane scale timing](#control-plane-scale-timing), from `ruralz_control_connected_nodes` and `ruralz_control_stream_reconnects_total{result="shed"}`; after the stop, no survivor sheds and every Node reconnects within 5 minutes (target); the shedding onset replaces the 5,000 hypothesis | Planned (M2) |

```text
O1b goodput G, with C = 4 vCPU, offered O, admitted cost s, mean rejection cost r (target):
      G = (C − O × r) / (s − r)                                                       (target)
      = (4,000,000 − 42,000 × 16) / (142 − 16) µs ≈ 26,413 rps                        (target)
      = 94% of 28,000 rps, PB-11 met; 15,587 rejections/s                             (target)
      PB-11 still holds at r = 24 µs: 2,992,000 / 118 ≈ 25,356 rps, 90.6%           (hypothesis)
O1a: 20,000 × 142 µs + 10,000 × 16 µs = 3.0 vCPU of 4 → rejections never queue        (target)
O1a upstream: 20,000 attempts in flight on one mock Endpoint, whose share is       (target)
      50% of maxConnections 65,536 = 32,768 → zero RZ-UP-006; the mock holds      (target)
      20,000 connections and the local port range leaves 20,000 ports free            (target)
O1b queueing: the fixed ceiling admits 20,000 units, so admitted requests wait about (target)
      20,000 / 26,413 rps ≈ 0.76 s                                                    (hypothesis)
      (PB-11 and shedding: OQ-performance-budgets-and-benchmarking-11)
O2:  8,192 calls / 0.5 s = 16,384 calls/s held; the other 7,616/s are skipped           (target)
      (CE-3's 200 ms would need 41,000 calls/s to fill 8,192 (hypothesis);
      the 1 s timeout keeps the breaker closed)                                     (target)
S5x: 500 µs ± 100 µs normal delay each way → injected round trip mean 1 ms,        (hypothesis)
      p99 1 + 2.33 × 0.14 ≈ 1.33 ms; + RH-1 50 µs + script ≈ 1.5 ms ≤ PB-14 2 ms    (hypothesis)
      stateStoreTimeout 10 ms, five times PB-14                                       (target)
```

The S2 Bundle ([Configuration model](02-configuration-model.md) fields only) carries the scenario rules in comments:

```yaml
# S1 drops the Gateway policies; S3, S7, O1 and C2 add an https listener on 8443.
apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: bench
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
  admin:
    port: 9901
  telemetry:
    otlp: {endpoint: "https://otel-sink.bench.example:4317"}   # TLS client settings: telemetry.otlp.tls
    traceSampling: 0.01              # (target), set explicitly; accessLog.when unset: every request is logged
  stateStore:
    driver: memory                   # GCRA in process, no round trip: SM-4's "no State Store Policy";
                                     # this GCRA counts as gateway-added CPU
  policies:
    - name: jwt-bench
    - name: ratelimit-bench
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: mock
spec:
  protocol: http
  endpoints:
    - address: "upstream-mock.bench.example:8080"
  loadBalancing: {algorithm: round-robin}
  circuitBreaker:                    # defaults cap one Endpoint at 512 attempts (hypothesis)
    maxConnections: 65536            # above S1's 50,000 attempts/s; one Endpoint's 50%: 32,768 (target)
    maxPendingRequests: 1024         # S6's provider Upstream sets the same values (target)
  timeout: 2s                        # (target)
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: bench-1k
spec:
  match:
    path: {exact: /bench/1k}
    methods: [GET]
  upstreams:
    - name: mock
  timeout: 5s                        # (target)
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: jwt-bench
spec:
  type: auth.jwt                     # generator rotates 10,000 tokens (target) from two keys; no verified-token cache
  config:
    issuers:
      - issuer: https://idp.bench.example
        jwksUrl: https://idp.bench.example/.well-known/jwks.json
        audiences: [bench]
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: ratelimit-bench
spec:
  type: ratelimit
  config:
    key: 'request.headers["x-bench-key"]'   # the generator cycles 100,000 values uniformly (target)
    limits: [{requests: 20000, window: 1s}] # under 25,000/s per key (target); never denies at the offered rate
# At 40,000 rps each key sees about 0.4 requests/s (hypothesis), so S5 runs GCRA on every request and denies none.
# S5 and O2 overlays set stateStore {driver: redis, url: secretRef env RURALZ_STATE_STORE_URL};
# O2's overlay also sets the Policy's stateStoreTimeout: 1s; S5x's sets 10ms (target).
# A Zipf variant keeps its hottest key under 25,000 calls/s (target); single-key runs belong to CE-12.
```

The Zipf variant is one single S5 run at half saturation, 20,000 rps (target), whose keys follow a Zipf distribution with exponent 2 over S2's 100,000 values: the hottest key takes about 61% of calls, about 12,000 calls per second (hypothesis), under 25,000 even at S5's 40,000 rps (target) and below Scalability's [`localOnly` threshold](11-scalability-and-distributed-state.md#distributed-rate-limits). It is judged on S5's absolute criteria.

### Open-loop load and coordinated omission

A closed-loop generator starts a request only after the previous one finishes, so it slows down with the system and never records the requests it failed to send: coordinated omission ([source](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/)). wrk2's README shows a corrected p99 of 10.52 ms against 5.43 ms uncorrected ([source](https://github.com/giltene/wrk2)).

Every Ruralz macro run is open-loop:

- The generator offers a fixed arrival rate and measures each latency from the intended send time.
- Tools: the test kit's `internal/testkit/loadgen` ([Testing](../engineering/03-testing-and-quality-strategy.md#load-generator-and-l4-balancer)) drives every scenario whose requests carry rotating keys or bearer tokens, S2, S3, S5, S5x, O1, O2, the PB-15 and PB-16 runs and their ladders, since oha sends only fixed headers. oha, MIT-licensed at v1.16.0, drives S1 and its ladder with a fixed rate (`-q`) and `--latency-correction` ([source](https://github.com/hatoo/oha)) ([source](https://github.com/hatoo/oha/releases)); vegeta, MIT-licensed and open-loop through `-rate`, cross-checks S1 ([source](https://github.com/tsenart/vegeta)); fortio, Apache-2.0 with a fixed `-qps` and gRPC, drives S7 ([source](https://github.com/fortio/fortio)). C1's idle connections and C2's handshake burst come from harness drivers. oha and vegeta have [CI tooling](../engineering/01-tech-stack-and-libraries.md#ci-tooling) rows; fortio's lands with S7.
- oha runs each phase as its own invocation: the 60 s warm-up, each 30 s extension, then the 180 s measured run (target), each with `--latency-correction --output-format csv`. The harness builds the HdrHistograms, the 10 s warm-up windows and the V2 interval logs from the per-request lines, as loadgen does for its own runs.
- HdrHistogram's `recordValueWithExpectedInterval` back-fills samples when a tool records raw service times ([source](https://github.com/HdrHistogram/HdrHistogram)).
- vegeta's cross-check is one S1 run at 40,000 rps (target) in slot 4 of each rotation; when its external p99 differs from oha's pooled S1 p99 by more than 10% (target), that night's S1 comparison is inconclusive and an issue opens.
- A run is valid only if the achieved rate stays within 0.5% of the offered rate (target), counting `RZ-RT-005` in O1, the generator and mock stay below 70% CPU (target), and the mock's p99 beyond its configured delay stays at 200 µs or less (target).

### Warm-up, duration and percentiles

- **Warm-up.** Each run first offers its rate for 60 s (target), discarded, extended in 30 s steps up to 180 s until the p99 of three consecutive 10 s windows varies by 5% or less (target); extensions draw on the Latency job's nightly reserve.
- **Duration.** Five measured runs of 180 s per scenario and commit (target), interleaved with the baseline in A, B, A, B order on the same hosts.
- **Offered rates.** Fixed, such as S1 at 40,000 and S2 at 16,000 rps (target), so a ladder result never moves a baseline.
- **Percentiles.** p50, p90, p99, p99.9 and maximum, per run and pooled, from HdrHistogram at three significant digits; an S2 run holds about 2.9 million samples (hypothesis).
- **Rate ladder.** Open-loop steps of 5% of the expected saturation from 50% to 110%, 60 s each (target).

### Measurement points

Both views gate, because each misses something the other sees:

1. **External.** End-to-end p99 through the Node, head against baseline, including kernel queue time the Node never sees; a direct-to-mock run in each pre-run check is a floor check, never subtracted, since percentiles do not subtract.
2. **Internal.** The access log's `gateway_duration` gives exact per-request gateway-added time, and `ruralz_http_gateway_duration_seconds` has bounds at 1 ms and 150 µs (target), so PB-2 and PB-3 gate exactly at the SLO-GW-2 and SLO-GW-3 thresholds. The OTLP sink on RH-1U, `internal/testkit/otlpsink` in the RH-1U helper, folds each access-log `gateway_duration` into the run's internal histogram as records arrive, instead of keeping them. Any `ruralz_telemetry_logs_dropped_total` increase invalidates a run.

A criterion that names only a percentile reads the view of what it measures. Gateway costs read the internal view: PB-1 to PB-3, S3's 1 ms point, O1a's rejections and O2's skipped requests. What clients see reads the external view: C2's existing traffic and saturation's end-to-end 10 ms. TLS handshake criteria read `ruralz_listener_tls_handshake_duration_seconds` as wall time.

### Reproducibility

- Each result records the hardware fingerprint, kernel, sysctls including the local port range, NIC settings, cpuset, `GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`, the toolchain (go1.27.1, pack 7; [ADR-0001](../adr/0001-implementation-language-go.md)), commit, Revision digest, tool versions and command lines.
- Hosts run nothing else. Before every scenario a pre-run check confirms each host's fixed frequency, unchanged thermal-throttle counters and a round trip p99 of 50 µs or less (hypothesis) between the hosts, and runs the direct-to-mock floor check. A failure stops the job with an "RH-1 host check failed" annotation, which opens the nightly-failure issue, and writes no records.
- Each run makes a fresh PKI with `internal/testkit/pki`: the listener certificate (ECDSA P-256), the IdP's, the OTLP sink's and Valkey's, which the harness hands to each host at the profile's fixed paths. `ruralzd` starts with `SSL_CERT_FILE` naming the run's CA, which also verifies the IdP.
- The baseline side is built with its own commit's `go.mod` toolchain line, so a Go upgrade shows in the comparison. Both sides load the Revision the head renders from the head's scenario Bundle; a baseline that refuses it makes the comparison inconclusive.
- Baseline p99 values across five runs MUST vary by a coefficient of variation of 2% or less (target), or the comparison is inconclusive.
- Anyone can re-run every scenario on RH-3 with its inventory and the runbook in `deploy/benchhost`; floor-toolchain builds (Go 1.26 with `GOEXPERIMENT=jsonv2`, [Version floor](../engineering/01-tech-stack-and-libraries.md#version-floor)) report for information only.

## Benchmark suite and CI gating

### Suite layers

Every layer runs in a stage or `nightly` job of [Testing and quality strategy](../engineering/03-testing-and-quality-strategy.md#test-pyramid); `release` re-runs them all.

| Layer | Contents | Runs on | Milestone |
|---|---|---|---|
| Microbenchmarks | `go test -bench -benchmem` over every hot-path benchmark that `test/bench/allocgate.json` lists ([Regression policy and gates](#regression-policy-and-gates)), PB-8's `testing.AllocsPerRun` over one whole S1 request among them, and the Plugin Phase call | Shared runners, `pr-full` | Planned (M1); Plugin Phase call Planned (M2) |
| Component benchmarks | Loader compile ladder, JWT per algorithm, token bucket shards, `RZ-RT-005` rejection, metric aggregates at 4 P and 32 P (target), metric recording per request for PB-10, typical match and key CEL expressions; tokenizer, Planned (M3). Each benchmark times every call into an HdrHistogram inside `ruralzd`'s cpuset, in five runs interleaved with its baseline. The loader's 2 s for parsing an input of up to 1 MiB (target) ([ADR-0003](../adr/0003-configuration-format.md#confirmation)) is measured by the nightly Fuzz job instead and never asserted in a plain test run, whose tests assert only its 256 MiB heap bound (target) | RH-1, `nightly` Latency job; the 1 MiB parse in the `nightly` Fuzz job | Planned (M1) |
| Scenarios | S1 to S8, S5x, S6n, O1, O2, C1, C2, R1, G1, constants tests | RH-1 hosts, Latency job: S1 and S2 nightly, the rest in the slot rotation | Planned (M1) to Planned (M4) |
| Scenario smoke run | Every M1 scenario on the loopback profile at 100 rps for 5 s (target): it starts, serves and writes a valid record, with no numeric criteria, within 5 minutes in all (target) | Shared runners, `pr-full` stage 8 | Planned (M1) |
| Functional variants | Every M1 scenario and constants run at reduced scale on the loopback profile, asserting only criteria that hold on any hardware (below) | General runners, `nightly` Functional job | Planned (M1) |
| Control-plane scale and accounting | F1 (SM-9), F2, SM-10 | Dedicated general runners, `nightly` Scale job | Planned (M2) to Planned (M3) |
| Soak | S2 at half saturation for 2 hours (target), under the soak rule below | RH-1, `release` soak job | Planned (M1) |
| Release trend | The M4 bench suite: S1, S2, S5 and S6 of each release against the budgets and the previous releases, on the same hosts and Bundles | RH-1, `release` | Planned (M4) |

The Latency job runs alone on RH-1 within Testing's 3-hour limit (target):

```text
A slot holds at most one A/B scenario, filled with single runs judged on absolute criteria.
A/B scenario: 10 runs × (60 s warm-up + 180 s measured)                    40 min (target)
Single run with absolute criteria, no A/B pair: 60 s warm-up + 180 s          4 min (target)
Ladder: 60 s warm-up + 13 steps of 60 s, 50% to 110% of expected saturation 14 min (target)

Nightly     S1 and S2 A/B 80 + component benchmarks 15 + one slot 75      170 min (target)
Reserve     pre-run host checks, warm-up extensions, re-runs of single invalid runs  10 min (target)
Worst case  80 + 15 + 75 + 10                                             180 min (target)
When the reserve is spent, a run still unsettled after 60 s of warm-up is invalid (target);
its scenario, or an inconclusive A/B comparison, re-queues. Re-queued items run first the
next night; the items of that night's slot that no longer fit within 75 min move to the
night after (target).

Slot  Contents                                                           Minutes (target)
1     S3 A/B 40; O1a, O1b 8; C2 4; S3 ladder 14; S5 Zipf variant 4             70 (target)
2     S5 A/B 40; O2 4; G1 15; S5 ladder 14                                     73 (target)
3     S5x A/B 40; C1 cleartext and TLS 10; PB-15 and PB-16 runs 8;
      PB-10 telemetry-off S2 4; GOMEMLIMIT run, C1 over TLS plus S2, 9         71 (target)
4     S1 and S2 ladders 28; R1 idle and loaded 16; constants runs 25;
      vegeta S1 cross-check 4                                                  73 (target)
5     S4 A/B 40; S4 ladder 14; Plugin constants runs 12 (M2)                   66 (target)
6     S6 A/B 40 (M3)                                                           40 (target)
7     S6n A/B 40 (M3)                                                          40 (target)
8     S7 A/B 40; S7 ladder 14 (M3)                                             54 (target)
9     S3 HTTP/3 variant A/B 40 (M3)                                            40 (target)
Rotation: every item runs every four nights at M1, five at M2, nine at M3 (target).
The 2-hour soak (target) exceeds a slot, so it runs in release only.
```

The re-queued items and the rotation position live on the `bench-records` branch beside the records ([Publication](#publication)), so each night starts where the last one stopped.

In `release`, `release.yml` calls `nightly.yml` with its `rotation-all` input. One Latency job on `rh-1` then runs S1, S2, the component benchmarks and every M1 slot without the 3-hour limit, comparing with the previous release's signed linux/amd64 archive, or against absolute budgets only for `0.1.0`. A separate soak job on `rh-1` follows, serialized through the `rh-1` concurrency group. Both upload `ruralz.bench.v1` records ([Release pipeline](../engineering/04-release-versioning-and-compatibility.md#release-pipeline)).

The soak samples RSS and goroutines every 10 s (target). Its RSS drift is the median of the last 10 minutes against the median of minutes 10 to 20, 2% or less of the latter (target); its goroutines are flat when those two medians differ by at most 1% or 50, whichever is larger (target). The soak also meets S2's validity rules, exports with zero telemetry drops, and meets PB-2 and PB-3 over its whole run.

The functional variants in `test/bench/functional/` run every M1 scenario and constants run at reduced scale on one Linux host over loopback, with the process harness and loadgen, in the nightly Functional job on general runners. They assert only criteria that hold on any hardware: O1's admitted plus `RZ-RT-005` accounting and its 20,000 in-flight plateau, O2's plateau at 8,192 calls in flight, reached with a longer injected delay at a lower rate, and its skip counting, C1 and C2 holding every connection at reduced counts, the frame limits, the pre-routing writes, the body flood, K = 2 under activations and the queue bounds. Their numbers are never published as budget results. Like every benchmark run, the functional variants and the `pr-full` smoke run use the release build and real time ([Hermetic rule scope](../engineering/03-testing-and-quality-strategy.md#hermetic-rule-scope)): a Node whose clock stood still could not serve sustained load, since its rate limits would never refill.

### Regression policy and gates

A change fails when it regresses p99 latency by more than 5% (target) or alloc/op by more than 3% (target) against its gate's baseline, or when it exceeds any budget in the [Budget catalog and SLO ties](#budget-catalog-and-slo-ties).

| Gate | Stage and job | Baseline | Fails when | Milestone |
|---|---|---|---|---|
| alloc/op | `pr-full` | Merge base, interleaved, 10 runs each (target), `GOGC=off`, fixed `GOMAXPROCS` | Median alloc/op above the base by more than 3% (target), or a cap of `test/bench/allocgate.json` exceeded | Planned (M1) |
| Macro p99 | Latency job, each scenario on its night; `release` | The commit of the same scenario's last valid passing run, re-run interleaved; for `release`, the previous release's signed linux/amd64 archive | Median-of-runs p99, external or internal, above the baseline by more than 5% (target), with four of five pairs slower | Planned (M1) |
| Component benchmarks | Latency job; `release` | The same benchmark's last valid passing run, interleaved, five runs each (target) | A target the documents tie to the benchmark missed: CEL expressions under 2 µs at p99, the `RZ-RT-005` rejection's 16 µs mean CPU (target); or a median p99 above the baseline by more than 5% (target), with four of five pairs slower. Other per-stage values are reported | Planned (M1) |
| Absolute budgets and criteria | Latency job; `release` | Budget catalog, throughput targets, scenario criteria, constants table | Any missed. A budget's result is the pooled histogram of the five head runs; each run's value is reported | Planned (M1) |
| Size and idle RSS | `pr-full` | Fixed values | Stripped release binary above 160 MiB, or idle `VmRSS` after the settle above 89 MiB (target) | Planned (M1) |
| Control-plane scale and accounting | Scale job; `release` | PB-13, the F2 rows, SM-10 | Any missed | Planned (M2) to Planned (M3) |

The 3% alloc/op threshold is deliberately strict: at PB-8's 30 allocations (target), one extra allocation is 3.3% and fails. A failing `pr-full` gate blocks merge; a failing `nightly` gate opens an issue and blocks the next release ([Repository layout and conventions](../engineering/02-repository-layout-and-conventions.md)). An override needs recorded maintainer approval: in `pr-full` it is the `perf-override` label applied by a maintainer (repository role admin or maintain), after which the gates job reports failures without failing (`scripts/ci-approvals.sh perf-override`); loosening a value edits this document (BP-7). Applying the label does not re-run `pr-full`; a maintainer re-runs its gates job, whose summary says so.

Failed, invalid and inconclusive runs never advance a baseline. A maintainer accepts a deliberate macro regression through `nightly.yml`'s maintainer-only `workflow_dispatch` input naming the scenario and the accepted commit; the next record names that commit as its baseline, and the open issue records the acceptance.

`test/bench/allocgate.json` lists every hot-path benchmark, each at the 3% rule: the Router, the Filter Chain executor, the exchange, the Upstream leg, the body layer, CEL, canonicalization, validation, the built-in Filters and telemetry. It caps PB-8's benchmark at 30, and each benchmark that has its own per-stage or per-signal allocation target at that target: for example the Upstream layer at 6, `onResponse` and the response write at 2, the ratelimit `memory` path and the other attached Filters at 0, an unsampled trace at 2, an access-log record at 8 and a sampled 15-span trace at 90 ([Observability](10-observability.md#overhead-per-signal)) (target). `auth.jwt` stays reported, not gated, and a CEL rule that tracks `split` keeps only the 3% rule. PB-8's benchmark lives in `internal/gateway/handler`: one whole S1 request, from a pre-parsed HTTP/1.1 request through the exchange, the Router, the Filter Chain executor and the Upstream leg on a canned transport, to the response write into a discard `ResponseWriter` and `onLog` with metrics, an unsampled trace and the access log.

*Figure 2: the benchmark pipeline from load generator to CI gate and report.*

```mermaid
flowchart LR
    src["Head and baseline commits"]
    build["Release toolchain build of ruralzd"]
    rev["Benchmark Bundles rendered to fixed Revisions"]
    subgraph lab["RH-1 hosts on one switch"]
        gen["Load generator host: open-loop loadgen, oha or vegeta at a fixed rate"]
        gw["Ruralz Gateway under test: 4-core cpuset (target)"]
        mock["Upstream mock and Collector sink host"]
    end
    ext["External histograms from intended send time"]
    int["Internal: access-log gateway_duration and SLI histogram"]
    prof["CPU profile and RSS samples"]
    valid{"Run valid: rate, drops, noise?"}
    cmp["A/B comparison and budget check"]
    gate{"CI gate: 5% p99 (target), 3% alloc/op (target), budget catalog"}
    pass["Pass: baseline advances"]
    fail["Fail: block merge or next release, open issue"]
    report["Report: result record, raw histograms, profiles"]
    src --> build --> rev --> gw
    gen -->|"requests"| gw
    gw -->|"upstream requests"| mock
    gen --> ext
    gw --> int
    gw --> prof
    ext --> valid
    int --> valid
    valid -->|"no, re-run once"| gen
    valid -->|"yes"| cmp
    cmp --> gate
    prof --> report
    gate --> pass
    gate --> fail
    pass --> report
    fail --> report
```

### Statistics and runners

Alloc/op gates run on shared runners, because allocation counts do not depend on the machine under `GOGC=off` with fixed `GOMAXPROCS`, and latency gates only on RH-1 bare metal. Comparisons use medians across interleaved runs plus the four-of-five pair rule, which needs no distribution assumption. An inconclusive run (noise above 2%) (target) re-runs once in the next night's slot, then opens an issue.

F1 runs 100 `ruralzd` processes, each in its own network namespace as [Scalability and distributed state](11-scalability-and-distributed-state.md#scale-unit) requires, on four dedicated runners (target), with three Ruralz Control replicas on separate runners (OQ-testing-and-quality-strategy-10). SM-9 measures control-plane fan-out, so no request traffic runs, and kind is excluded because Kubernetes scheduling adds noise.

### Constants the suite verifies

Constants delegated by [Data plane](03-data-plane.md#performance-budgets), [Scalability](11-scalability-and-distributed-state.md#per-node-limits) and [WASM plugin system](05-wasm-plugin-system.md#performance-model):

| Constant | Value | Owner | Test and pass criterion |
|---|---|---|---|
| Client connections; in-flight units | 20,000 each (target) | Data plane | C1 and C2 hold all; O1 plateaus, never queues |
| HTTP/2 streams per connection; header block; streamed chunk | 250; 256 KiB; 1 MiB (target) | Data plane | The frame-limit run of the [protocol suite](../engineering/03-testing-and-quality-strategy.md#conformance-suites)'s HTTP/2 frame cases: stream 251 refused, plain 431, `RZ-RT-013` |
| `ReadHeaderTimeout`; `IdleTimeout`; body read and write deadlines; pre-routing write deadline | 10 s; 120 s; 10 s; 5 s (target) | Data plane | The protocol suite's timeout cases, in real time: each expiry between its value and its value plus 2 s (target) |
| Pre-routing writes | 2,000, 5 s deadline (target) | Data plane | 2,500 clients never reading `RZ-RT-001`: extra handlers abort |
| Per-connection memory | About 592 KiB (hypothesis) | Data plane | Slow-header and HTTP/2-window flood, 20,000 connections |
| Retired snapshots; grace period | K = 2; 30 s (target) | Data plane | Ten activations per second (target) under S2: zero failed unary requests, peak within (K + 2) × snapshot |
| Buffered bytes | 512 MiB (target) | Configuration model | Body flood: `RZ-RT-004`, no OOM |
| Rate-limit key table; local-only segment; first-seen budget | 1,048,576 entries, about 64 MiB; 131,072; min(500, B / N_published) per second (target) | [Traffic management and resilience](09-traffic-management-and-resilience.md#decision-path) | CE-15; CE-12 with `localOnly` keys |
| Balancer structures; hot-entry layer | 256 MiB; 64 MiB (target) | Traffic management and resilience | `ring-hash` Upstream ladder; cache key flood |
| Quota denial cache | 65,536 entries, about 4 MiB (target) | Scalability and distributed state | 100,000 denied Quota keys |
| Post-commit write queue | 64,000 items and 85 MiB (target) | Scalability and distributed state | CE-16 |
| State Store calls in flight; connections per shard | 8,192; 2 plus 8 (target) | Scalability and distributed state | O2; G1 and O2 connection counts |
| Span and access-log queues | 8,192 spans; 8,192 records and 4 MiB (target) | Observability | S2 at both trace caps |
| Plugin instance wait | 1 ms (target) | WASM plugin system | S4 burst at twice the busy count: wait p99 within 1 ms, then `RZ-PLG` |
| `state.read` pool ramp; long guest calls during GC | 1 to 25 instances in 250 ms; gateway-added p99 of 1 ms or less (target) | WASM plugin system | S4 variants: a load step; a long-running guest |

A row whose test names a CE experiment, the rate-limit key table and the post-commit write queue, is verified by that experiment's runs in Testing's Chaos job and `chaos-scale.yml` ([Chaos testing](../engineering/03-testing-and-quality-strategy.md#chaos-testing)); slot 4's constants runs cover the other rows.

### Milestone plan

| Milestone | Joins the suite |
|---|---|
| Planned (M1) | S1, S2, S3, S5, S5x; O1, O2, C1, C2, R1, G1; microbenchmarks; component benchmarks; the scenario smoke run and the functional variants; alloc/op, size and idle RSS gates; soak; constants tests except Plugin rows |
| Planned (M2) | S4 and PB-5 (SM-6); F1 (SM-9), F2; Plugin constants rows; `authz.opa` and `authz.cedar` costs; reload with Plugins |
| Planned (M3) | S6, S6n (PB-12), the tokenizer benchmark, S7, the HTTP/3 variant of S3; WebSocket and SSE session memory; SM-10 |
| Planned (M4) | The release-trend bench suite; S8; the `default.pgo` build ([tech stack](../engineering/01-tech-stack-and-libraries.md#version-floor)) |
| Planned (M5) | The FIPS build, with P1's feature set, runs every scenario but HTTP/3 under the same budgets (target), except S3 and C2 handshakes, reported until measured since they may cost more (hypothesis) |

## Profiling playbook

### From symptom to profile

| Symptom | First signal | Capture | Usual cause |
|---|---|---|---|
| p99 over budget while CPU stays below half | `ruralz_http_gateway_duration_seconds`, `ruralz_runtime_gc_cycles_total` | CPU and goroutine profiles under the same load | GC assists, lock waits, scheduler delay |
| CPU per request above its budget | Resources per core | 30-second CPU profile, diffed against the release baseline | New work on the hot path |
| alloc/op gate failure | The gate's benchmark | `-benchmem` and a heap profile by `alloc_objects` | A value newly escaping to the heap |
| RSS or goroutine growth | `ruralz_runtime_heap_bytes`, `ruralz_runtime_goroutines` | Two `inuse_space` heap profiles 10 minutes apart; goroutine profile | Retained snapshots, label sets or pools; blocked handlers |
| Throughput stops scaling with cores | `ruralz_filter_duration_seconds` at 32 P (target) | Mutex and block profiles | Shared shards, pools or counters |
| Plugin calls over PB-5 | `ruralz_plugin_call_overhead_seconds`, `ruralz_plugin_pool_wait_seconds` | CPU profile showing wazero frames | Pool too small, loop-heavy guest |

### Capturing profiles

Profiles come from `/debug/pprof/` on admin port 9901 ([Data plane](03-data-plane.md#admin-endpoints)), which requires a token or mTLS (TB-9; bind and scheme: OQ-system-overview-6). Mutex and block profiling run only during a `?seconds=` request ([Observability](10-observability.md#debugging-tools)).

```bash
# 30-second CPU profile from one Node under load; a bearer token is shown, mTLS works too
curl -sS -H "Authorization: Bearer ${ADMIN_TOKEN}" -o cpu.pprof \
  "https://node-1.ops.internal:9901/debug/pprof/profile?seconds=30"
go tool pprof -top -nodecount=30 cpu.pprof
go tool pprof -diff_base=release-baseline-cpu.pprof -top cpu.pprof

# Heap: live bytes, then allocation sites
curl -sS -H "Authorization: Bearer ${ADMIN_TOKEN}" -o heap.pprof \
  "https://node-1.ops.internal:9901/debug/pprof/heap"
go tool pprof -sample_index=inuse_space -top heap.pprof
go tool pprof -sample_index=alloc_objects -top heap.pprof

# Contention, sampled only for the requested window
curl -sS -H "Authorization: Bearer ${ADMIN_TOKEN}" -o mutex.pprof \
  "https://node-1.ops.internal:9901/debug/pprof/mutex?seconds=30"

# Allocation benchmarks as the alloc/op gate runs them
GOGC=off go test -run '^$' -bench . -benchmem -count 10 ./internal/...
```

Profile one Node at a time; CPU profiling costs about 5% of its CPU (hypothesis). Heap profiles can hold request data, so reports publish CPU profiles only. The Latency job takes each scenario's 30 s CPU profile right after the last head run's measured window, at the same offered rate, so no measured window is ever profiled.

### GOGC and GOMEMLIMIT

- `GOGC` defaults to 100; the heap target is live heap plus (live heap + GC roots) × `GOGC`/100, and doubling `GOGC` roughly halves GC CPU cost ([source](https://go.dev/doc/gc-guide)) ([source](https://pkg.go.dev/runtime)).
- `GOMEMLIMIT` is a soft limit; GC CPU is capped at roughly 50% over 2 × `GOMAXPROCS` CPU-seconds, containers keep 5 to 10% headroom, and `GOGC=off` with a limit suits only a process that owns its memory ([source](https://go.dev/doc/gc-guide)).
- Since Go 1.25, `GOMAXPROCS` follows cgroup CPU limits ([source](https://go.dev/doc/go1.25)); Go 1.26 makes Green Tea GC the default, expecting 10 to 40% less GC overhead ([source](https://go.dev/doc/go1.26)).

Ruralz rules:

1. Budgets are verified at `GOGC=100` without `GOMEMLIMIT`; operators SHOULD set `GOMEMLIMIT` to about 90% of the container limit (target), as [Scalability](11-scalability-and-distributed-state.md#scale-unit) advises.
2. When GC CPU exceeds 10% of Node CPU (target) and memory allows, raise `GOGC` to 200 and confirm with S2.
3. Operators SHOULD NOT set `GOGC=off`: without `GOMEMLIMIT` the heap never collects and the process is OOM-killed; with it, an overloaded Node spends up to the GC CPU cap collecting (hypothesis).

### Profile-guided optimization

`default.pgo` for `cmd/ruralzd` is Planned (M4). Go reports 2 to 14% gains, with CPU pprof profiles driving inlining ([source](https://go.dev/doc/pgo)). The profile merges the previous release's S2 and S4 CPU profiles; the PGO build passes the same gates, and no budget assumes its gain.

## Reporting

### Result record

Every run writes one `ruralz.bench.v1` record of kind `macro` or `component`, and every scenario's comparison, ladder or soak adds one record of kind `summary`. Each is checked against `test/bench/record.schema.json`, which `go generate` writes from the schema `internal/testkit/benchrecord` embeds, and stage 3 diffs:

| Field | Content |
|---|---|
| Identity | Scenario, kind, commit, baseline, Revision digest, toolchain |
| Environment | Hardware profile and microarchitecture, kernel, cpuset, `GOMAXPROCS`, `GOGC`, `GOMEMLIMIT`, sysctls, NIC settings |
| Load | Tool, version, command line, offered and achieved rate, connections, key cardinality and distribution, injected delay distribution |
| Latency | External and internal percentiles, raw HdrHistogram V2 interval logs |
| Resources | CPU per request, GC CPU share, RSS at load and at idle settle, live heap, alloc/op; for State Store scenarios, Valkey's main-thread CPU share and `cmdstat_evalsha` µs per second, ungated |
| Validity and budgets | Rate error, generator and mock CPU, drops, noise, the run's verdict; each budget's value and this run's result |
| Summary (kind `summary`) | Pooled percentiles of the head runs and of the baseline runs, slower pairs, baseline noise, the A/B verdict, and each budget's pooled result, SLO and verdict; `releasekit notes` and `releasegate` read summaries |

### Publication

- Nightly and release runs publish their records, raw histograms and CPU profiles from this repository (P10), Planned (M1). The Latency and soak jobs upload them, and a records job on a GitHub-hosted runner, the only job besides a release's publish job that holds `contents: write`, commits them to the orphan `bench-records` branch, so the `rh-1` jobs keep read-only tokens. The branch also holds each scenario's baseline and the rotation state, and a 90-day workflow artifact keeps a copy. A release also attaches its records as the release asset `ruralz_<version>_bench.tar.gz`, listed in the manifest and `SHA256SUMS` ([Release artifacts](../engineering/04-release-versioning-and-compatibility.md#release-artifacts)). Release notes list every budget with its result from the summary records.
- One RH-3 run publishes with `0.1.0`, beside RH-1's results and never gated.
- Other documents quote Ruralz performance only as a tagged target or a published result.
- S1 and S2 nightly and release results publish from Planned (M1) (SM-4, SM-5); from Planned (M4) the bench suite publishes S1, S2, S5 and S6 for each release beside the budgets and the previous releases' results.
- Every result carries its Ruralz release, commit and configuration.

### Production comparison

Operators compare budgets and SLOs on the same metrics in the SLO burn rates Grafana dashboard ([Observability](10-observability.md#grafana-dashboard-pack)); a production p99 above a budget at comparable load means a regression or an environment difference. [Capacity planning](../operations/03-capacity-planning.md) takes CPU per request, connection and Route memory, S6's per-chunk CPU and G1's per-shard calls from the latest release record.

## Open questions

| ID | Question | Options | Owner | Blocking? |
|---|---|---|---|---|
| OQ-performance-budgets-and-benchmarking-8 | Which JWT algorithm defines SM-4, whose cost it dominates? | (a) RS256, 2,048-bit (current); (b) ES256; (c) both, gating the slower | vision-and-positioning | No |
| OQ-performance-budgets-and-benchmarking-9 | Should Observability add a time-to-first-token SLO (PB-12) and an estimator duration metric? | (a) Both; (b) lab budget on S6n only (current) | observability | No |
| OQ-performance-budgets-and-benchmarking-10 | Sharded label sets cost about 2 KiB per Route and per Upstream at 8 stripes, 5.4 MiB in all (hypothesis); should PB-6 scale with stripes? | (a) No, the 89 MiB base holds 4 stripes (current); (b) a per-stripe allowance | observability | No |
| OQ-performance-budgets-and-benchmarking-11 | How does PB-11 change with load shedding, given about 0.76 s of queueing in O1b (hypothesis)? | (a) Fixed in-flight ceiling (current); (b) revise with an adaptive limiter | data-plane | No |
| OQ-performance-budgets-and-benchmarking-13 | PB-16 allows 2 ms (target) at survivor load; must zone sizing drop to 60% of saturation, where PB-15 proves 1 ms? | (a) Accept the SLO-GW-2 burn during a zone loss (current); (b) size survivors to 60%; (c) exclude declared zone losses | scalability-and-distributed-state | No |
| OQ-performance-budgets-and-benchmarking-14 | Should listeners bound concurrent TLS handshakes so C2's existing traffic holds its p99? | (a) `GOMAXPROCS` at once (proposed); (b) none; (c) a listener field | data-plane | No |
