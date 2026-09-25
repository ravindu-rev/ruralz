---
title: Zero-Downtime Upgrades and Hot Reload
status: draft
owner: ruralz-core
last_updated: 2026-09-25
depends_on:
  - docs/_meta/foundation-pack.md
  - docs/_meta/style-guide.md
  - docs/architecture/01-system-overview.md
  - docs/architecture/02-configuration-model.md
  - docs/architecture/04-control-plane-and-gitops.md
  - docs/engineering/04-release-versioning-and-compatibility.md
  - docs/architecture/11-scalability-and-distributed-state.md
  - docs/architecture/12-performance-budgets-and-benchmarking.md
  - docs/operations/01-deployment-topologies.md
adrs: [ADR-0005, ADR-0006, ADR-0007, ADR-0015, ADR-0016, ADR-0017]
milestone_tags_used: [M1, M2, M3, M4]
---

# Zero-Downtime Upgrades and Hot Reload

## Summary

This document decides how Ruralz changes what runs without dropping traffic: Hot Reload of a Revision, the Zero-Downtime Upgrade of `ruralzd` through `SO_REUSEPORT` and Drain ([ADR-0015](../adr/0015-zero-downtime-upgrades-so-reuseport.md)), Raft rolling upgrades of Ruralz Control within N-1 skew, State Store upgrades per Cell, apiVersion migrations, rollback in three steps or fewer and verification, with the validation gate, drain timeline defaults and long-lived connection strategy. Nothing is implemented: Hot Reload and the in-place handover are Planned (M1); Plugin hot-swap and Kubernetes, edge and Ruralz Control upgrades Planned (M2). Operators read it before changing production.

## Scope and non-goals

In scope: procedures, defaults and checks for changing a running deployment. "Pack 8.2" names a [foundation pack](../_meta/foundation-pack.md) section.

Non-goals, with owners:

- Snapshot pinning, retirement and ending: [Data plane](../architecture/03-data-plane.md#configuration-snapshots-and-hot-reload).
- Control Stream, Rollouts, gates and fencing: [Control plane and GitOps](../architecture/04-control-plane-and-gitops.md) ([ADR-0007](../adr/0007-control-stream-protocol.md)).
- Versions, skew policy and upgrade order: [Release, versioning and compatibility](../engineering/04-release-versioning-and-compatibility.md), which this document follows.
- Restores and Region failover: [High availability and disaster recovery](04-high-availability-and-disaster-recovery.md).
- Budget values: [Performance budgets and benchmarking](../architecture/12-performance-budgets-and-benchmarking.md); Helm objects: [Deployment topologies](01-deployment-topologies.md); fields: [Configuration model](../architecture/02-configuration-model.md).
- Listener socket passing: Not planned; ADR-0015 keeps processes independent.

## Guarantees

These follow from P2, P4, P9 and P10 ([Vision and positioning](../vision/01-vision-and-positioning.md#principles)); the [Verification runbook](#verification-runbook) tests them. Owned glossary terms (pack section 11):

| Term | Meaning |
|---|---|
| Hot Reload | One Node activating a new Revision without a restart: compile off the request path, then one atomic snapshot swap; in-flight requests keep their snapshot |
| Drain | Graceful shutdown of a Node: readiness fails, listeners stop accepting, HTTP/2 GOAWAY is sent, and in-flight requests finish within a bounded time |
| Zero-Downtime Upgrade | Replacing the `ruralzd` binary: the new process binds with `SO_REUSEPORT` and reports ready, then the old process Drains |

In place it is a handover; the Kubernetes (T4) Drain and restart of Pods keeps the Cluster serving but is not a per-Node Zero-Downtime Upgrade.

| ID | Guarantee | Verified by |
|---|---|---|
| ZG-1 | A Hot Reload ends no in-flight request unless K + 1 = 3 further activations occur during it, K = 2 (target): its streams then get a going-away close at once, and a request still pinned 30 s later (target) ends with `RZ-RT-014` | V-1, V-2 |
| ZG-2 | A Revision failing the validation gate never changes what a Node serves | V-1 |
| ZG-3 | An in-place handover refuses no new TCP connection, queues none on a process that may exit, and keeps the Node's `/readyz` at 200 throughout | V-3 |
| ZG-4 | A Drain signals every long-lived connection first and ends all work within 30 s of SIGTERM (target) | V-4 |
| ZG-5 | A Ruralz Control upgrade changes no Node's active Revision or readiness and stays within N-1 skew | V-5 |
| ZG-6 | A State Store upgrade degrades only its Cell, under each Policy's `failureMode` | V-7 |
| ZG-7 | An apiVersion migration yields the same Revision digest and an empty `ruralz bundle diff` | Golden corpus |
| ZG-8 | Every rollback this document owns takes three steps or fewer, chained paths included | Runbook drills, V-6 |

Not guaranteed: QUIC connections on UDP 8443 surviving steering (OQ-system-overview-18; HTTP/3 Planned (M3)); long-lived connections migrating; Node-local state, such as token buckets, surviving a new process, which admits at most one extra ceiling per key (target) ([Stateless data plane](../architecture/11-scalability-and-distributed-state.md#stateless-data-plane)).

## Configuration hot reload

Hot Reload is Planned (M1), Plugin hot-swap Planned (M2). File-mode Nodes reload on a directory replacement, a settled change or a new OCI digest (Planned (M2)), Control-mode Nodes on a `Snapshot` or `Delta`, through one gate and swap off the request path.

### Validation gate

The Node re-runs the [Configuration model](../architecture/02-configuration-model.md#validation-and-diff-semantics) checks with CI's library, then Node-only checks; the first failure rejects the Revision. Control mode sends a `Nack`, transient when Ruralz Control cannot reproduce it; file mode logs it in `ruralz_config_activations_total{result="rejected"}`, after parse and schema checks (RZ-CFG-001 to RZ-CFG-013). Steps 2 and 3 skip an unsigned watched directory.

| Step | Check | Code on failure | Class in Control mode |
|---|---|---|---|
| 1 | Encoded size within the Node's limit, never below the CLI default | RZ-CFG-001 | Deterministic |
| 2 | Content matches the full `sha256:<64 hex>` digest, a `Delta` after patching | RZ-CFG-027 | Deterministic when reproduced, else transient |
| 3 | Ruralz Control's signature against Enrollment anchors, or Sigstore on OCI ([ADR-0017](../adr/0017-artifact-signing.md)) | RZ-CFG-033 | Deterministic |
| 4 | `storeEpoch` and `assignmentSeq` at or above the persisted pair; Environment and Cluster match the `EnrollResponse` | Lower pair: transient `Nack`; wrong binding: RZ-CFG-033 | Transient; deterministic |
| 5 | Every apiVersion served, every field at or below the Node's schema level | RZ-CFG-007, RZ-CFG-024 | Deterministic |
| 6 | Re-validation from reference resolution onward | Any Configuration model code from that stage | Deterministic |
| 7 | Plugin artifacts fetched by digest and signed; ABI and exported Phases equal `spec.abi` and `spec.phases`, requested Capabilities a subset of `spec.capabilities`, ABI level at or below the Node's ([ADR-0005](../adr/0005-plugin-abi-v1.md)) | RZ-CFG-028, RZ-CFG-033 | Deterministic for a spec mismatch; transient for a fetch failure or a level above the Node's, not reproducible ([rule 2](../engineering/04-release-versioning-and-compatibility.md#plugin-abi-versioning)) |
| 8 | Every `secretRef` resolves on this Node | RZ-CFG-026 | Transient, not reproducible |
| 9 | Compile Routers, Filter Chains, CEL and Plugins once per digest | The failing check's code | Deterministic; a compile timeout transient |
| 10 | Bind added or changed listeners; on a reused port, CBPF steers to the new socket before the old one closes | OQ-zero-downtime-upgrades-and-hot-reload-4 | Transient |
| 11 | Warm new connection and Plugin pools | None: a pool that does not fit warms to 0 | Not applicable |

Operators MUST NOT roll out a Plugin needing an ABI level some Node of the Cluster lacks: such Nodes are quarantined.

### Atomic swap

After the gate the loader follows [Compile before swap](../architecture/01-system-overview.md#compile-before-swap): carry over unchanged pools and token buckets, publish with one atomic pointer store, ACK (active, not durable), write the candidate under `${RURALZ_DATA_DIR}/lkg/` and retire the old snapshot. The candidate becomes Last-Known-Good on activation in file mode, at the promoted digest in Control mode (pack 8.2).

Requests pin their starting snapshot until `onLog`. Beyond K = 2 retired snapshots (target) the oldest becomes closing: its streams end at once (WebSocket 1001, gRPC `UNAVAILABLE`, SSE end with a retry hint), and work still pinned 30 s later (target) ends with `RZ-RT-014` ([Data plane](../architecture/03-data-plane.md#configuration-snapshots-and-hot-reload)). The latest pending Revision waits until that snapshot is freed (rule 6 there): up to 37 s plus the largest Plugin `limits.timeout` (target), inside the 60 s ACK timeout (target) while that timeout stays under 23 s.

An `all-at-once` Rollout, or file-mode Nodes sharing one source, reach a third activation together, after as little as a rollback and a re-rollout: up to 20,000 streams per Node (target) close in the same second, 20 million across a 1,000-Node Cell (hypothesis), until OQ-zero-downtime-upgrades-and-hot-reload-10 jitters them.

For 5,000 Routes on an idle reference Node, activation takes 500 ms or less, PB-7 (target): verify 25 ms, compile 400 ms and swap 25 ms (target) ([Hot Reload stages](../architecture/12-performance-budgets-and-benchmarking.md#hot-reload-stages)).
Under half-saturation load it takes 1 s or less (target) with gateway-added p99 at 1.5 ms or less (hypothesis), since compile workers yield every 100 µs (target).

### Partial failure

A Node never activates part of a Revision:

| Where | What happens | What the operator sees |
|---|---|---|
| One Node, before the swap | Rejected; the active Revision keeps serving | The `Nack` code in `ruralz rollout status` |
| One Node, after the swap | The candidate write fails, as on a full disk | Drift of kind Last-Known-Good |
| Some Nodes, Control mode | A deterministic NACK fails the gate; transient NACKs retry, then quarantine; over max(1, 5% of the batch) lagging fails it (target) | `rolled-back` with `autoRollback: true`, else `paused` |
| Some Nodes, file mode | Rejecting Nodes keep the old Revision | `ruralz_config_revision_info{role="active"}` differs |
| Plugin pools at the memory cap | A pool that does not fit `limits.maxPluginMemoryBytes` warms to 0 | `RZ-PLG` rejections under `failureMode` |

### Plugin hot-swap

A Plugin changes only through a new Revision, so its swap is this Hot Reload, Planned (M2) ([Hot-swap lifecycle](../architecture/05-wasm-plugin-system.md#hot-swap-lifecycle), [ADR-0005](../adr/0005-plugin-abi-v1.md)): fetched by digest, signature first, compiled once per digest and memory limit; replacement pools warm to the predecessor's 1-minute peak busy count plus headroom (target), auth and authz first, inside the Node-wide cap and its 10% swap headroom (target); earlier requests take every later Phase, `onChunk` and `onLog` included, from their own snapshot's pools, which close at zero pins. A cold compile of a 2 MiB Plugin takes up to 1 s on one core (hypothesis), delaying the ACK, never a request.

### Hot Reload versus restart

| Change | How it takes effect |
|---|---|
| Any Bundle resource, including Gateway listeners, TLS certificates and limits | Hot Reload of a new Revision |
| A `secretRef` value from `file`, `kubernetes` or `vault` | Rotation in place ([secretRef](../architecture/02-configuration-model.md#secretref)) |
| `env` secret values, `RURALZ_*` settings, file-mode trust policy | A new process: handover, or Drain and restart |
| Gateway `spec.stateStore` | Hot Reload, planned as a [State Store upgrade](#state-store-upgrades) |
| The `ruralzd` binary | A Zero-Downtime Upgrade in place; Drain and restart on Kubernetes |

## Binary upgrades

A Zero-Downtime Upgrade follows [ADR-0015](../adr/0015-zero-downtime-upgrades-so-reuseport.md): `SO_REUSEPORT`, drain and readiness gating, and no listener socket passing. [Upgrade order](../engineering/04-release-versioning-and-compatibility.md#upgrade-order) moves Ruralz Control first, then Nodes from N-1 to N.

| Path | Where | Mechanism | Capacity during the upgrade |
|---|---|---|---|
| In-place handover, Planned (M1); on T6 Planned (M2) | VMs with systemd (T5), file-mode hosts (T2), edge sites (T6) | A second process on the same host and `${RURALZ_DATA_DIR}`; `SO_REUSEPORT` with CBPF pinning and steering | Unchanged; both processes share the Node's ceilings |
| Drain and restart, Planned (M2); not a Zero-Downtime Upgrade | Kubernetes (T4) | StatefulSet rolling update: preStop, Drain, new Pod | Minus up to `maxUnavailable` Pods, set to the PodDisruptionBudget: one zone's share at minimum scale (target) |

HAProxy measured 155 failed connections per million over 180 reloads with `SO_REUSEPORT` alone, from sockets closed with queued connections ([source](https://www.haproxy.com/blog/truly-seamless-reloads-with-haproxy-no-more-hacks)); CBPF steering, which empties the closing accept queue, closes that gap without socket passing.

### In-place handover

*Figure 1: the `SO_REUSEPORT` handoff from the old to the new `ruralzd` process, followed by the old process's Drain.*

```mermaid
sequenceDiagram
    autonumber
    participant PM as Operator or process manager
    participant Old as Old ruralzd, lock holder
    participant K as Kernel reuseport groups
    participant New as New ruralzd
    participant C as Clients and balancer probes
    participant CT as Ruralz Control
    Old->>K: at startup and after every bind, CBPF program selecting its own sockets
    PM->>New: start the target binary with the same RURALZ_DATA_DIR
    New->>New: live lock holder found, enter handover
    New->>New: verify and compile the holder's candidate, resolve secretRef values
    New->>K: bind listener and admin ports with SO_REUSEPORT, no listen yet
    New->>New: warm pools, readiness checks pass
    New->>Old: ready message on the Unix socket with version, format markers, active digest
    alt unknown format marker, older candidate, or 60 s timeout
        Old-->>New: refused with a reason, New exits holding no listening socket
    else accepted
        Old-->>New: accepted, with live connection, unit, buffer, Plugin memory and RSS usage
        New->>K: listen on TCP ports, still pinned to Old
        Old->>K: swap each CBPF program to select the new sockets, 9901 included
        Old->>Old: close idle admin connections, Connection close on admin responses
        Old->>Old: accept the queued connections, close listeners, QUIC flows lost
        Old->>CT: last Heartbeat with draining, close the Control Stream
        Old-->>New: file lock released at Drain start
        New->>CT: dial 8091, Hello, retry RZ-CP-002 with backoff
        CT-->>New: nothing, a Delta or a Snapshot
        C->>K: new connections and probes
        K->>New: every new connection
        Old-->>C: GOAWAY, WebSocket close 1001 and SSE end, jittered
        Old->>Old: finish in-flight work by the deadline, flush, exit
    end
```

1. **Start** with the same `RURALZ_CONFIG`, `RURALZ_DATA_DIR` and effective user ID, as `SO_REUSEPORT` requires ([source](https://man7.org/linux/man-pages/man7/socket.7.html)); a live lock holder means handover ([handover rules](../architecture/01-system-overview.md#hot-reload-rollout-and-zero-downtime-upgrade)).
2. **Boot** the holder's active Revision without the boot wait (pack 8.2), through the full gate; an unknown format marker refuses the handover ([Versioned surfaces](../engineering/04-release-versioning-and-compatibility.md#versioned-surfaces)).
3. **Bind, not listen.** Binding each `listeners` `port` and `admin.port` (8080, 8443 and 9901 by default) catches port errors; listening only once accepted queues nothing on a process that may exit. The holder's `SO_ATTACH_REUSEPORT_CBPF` programs, one per group with UDP 8443 included, select its own sockets until step 5.
4. **Gate.** Over the owner-only Unix socket under `${RURALZ_DATA_DIR}`, the holder accepts only its active digest: a newer candidate is reloaded and reported again; an older one, from a failed candidate write, is refused at once. Meanwhile probes reach only the holder, `/readyz` 200. Unaccepted after 60 s (target), the new process exits; if the holder exits first, the new process takes the lock and listens.
5. **Steer** through `golang.org/x/sys/unix` (`net.ipv4.tcp_migrate_req` is the alternative); QUIC connections are lost here, not at bind.
6. **Move readiness.** Closing idle admin connections and answering in-flight admin requests with `Connection: close` moves keep-alive health checkers to the new process; the old one never serves a 503 `/readyz`.
7. **Move identity.** The new process retries `RZ-CP-002` from a replica still holding the old stream with full jitter, base 1 s, cap 60 s (target), staying ready (pack 8.5; OQ-zero-downtime-upgrades-and-hot-reload-11). Only the lock holder writes Last-Known-Good (pack 8.11).
8. **Drain** on the handover timeline below.

The handover carries no Node-local state, answering OQ-traffic-management-and-resilience-22 with option (c) for Planned (M1): both processes keep full local token buckets within the one-extra-ceiling bound, and until the new process's first `HeartbeatReply`, within 15 s (target), derived ceilings use the full limit under the fail-open clamp max(1, `requests` / 100) (target), reason `node_count_unknown` (OQ-zero-downtime-upgrades-and-hot-reload-5).

Node-wide ceilings are shared: the old process reports its connections, in-flight units, buffered bytes, Plugin memory and RSS at acceptance and every second (target); the new one admits each ceiling minus that usage, with a soft memory limit of 90% of the limit minus the old RSS (target), rising as the old drains. Hosts MUST keep the overlap extra free, and State Store client limits MUST cover both processes (OQ-zero-downtime-upgrades-and-hot-reload-6):

```text
Overlap extra   second base RSS 100 MB + 2 KB per Route, its (K + 2) snapshots
                and state tables of 475 MiB: about 0.6 GiB plus snapshots         (hypothesis)
Shared          20,000 TLS connections × 96 KiB = 1.9 GiB, never doubled          (target)
State clients   2 pipelined + 8 dedicated per shard per process; server limit
                10 × (N_serving + Nodes in handover) per shard                    (target)
```

This answers OQ-deployment-topologies-7 with option (a), a systemd unit and helper shipped with releases (OQ-zero-downtime-upgrades-and-hot-reload-7); until then, `systemctl stop` then start is a Drain and restart.

### Node count during upgrades

Every handover, Drain and replica replacement restarts a Control Stream, and `clusterNodeCount` counts only Nodes connected 10 minutes (target) ([Replica roles](../architecture/04-control-plane-and-gitops.md#replica-roles)): a `draining` Heartbeat drops a Node at once, which rejoins after 10 minutes plus a 10%-per-minute ramp (target). A falling count raises derived ceilings and hot-key GCRA calls until breakers open. Until OQ-scalability-and-distributed-state-14 (a) and OQ-zero-downtime-upgrades-and-hot-reload-11 keep qualification per `node.id`, Clusters with derived ceilings MUST upgrade at most 10% of their Nodes per 10 minutes (target), about 100 minutes per Cluster (hypothesis); Clusters declaring per-Node ceilings ([T4](01-deployment-topologies.md#t4-kubernetes-with-helm-hpa-and-crds)) need no pacing.

### Drain timeline defaults

A Drain starts on SIGTERM, `ruralz node drain` (signaling the local lock holder) or an accepted handover, with defaults fixed in Planned (M1) (OQ-zero-downtime-upgrades-and-hot-reload-1). A handover skips the 503 (step 6) and the accept window, counting from steering: its Drain deadline falls 20 s later (target).

| Offset from SIGTERM | Step | Default |
|---|---|---|
| 0 s | Readiness failure: `/readyz` returns 503; heartbeats set `draining` | At once (target) |
| 0 to 5 s | Accept window while balancers observe `/readyz` | 5 s (target) |
| 5 s | Stop accepting; first HTTP/2 GOAWAY (last stream ID 2^31-1, `NO_ERROR`); `Connection: close` on HTTP/1.1; idle connections close | End of the accept window (target) |
| 5 to 6 s | Final GOAWAY after one PING round trip; until OQ-zero-downtime-upgrades-and-hot-reload-3 closes, Planned (M1) sends one GOAWAY, option (a) | 1 s at most (target) |
| 5 to 15 s | Going-away signals to long-lived connections, spread uniformly | 10 s jitter window (target) |
| 25 s | Drain deadline: remaining work ends through the Data plane ending protocol | 20 s after stop-accepting (target) |
| 25 to 30 s | Flush the post-commit queue and telemetry, then exit | 5 s at most; exit bound 30 s after SIGTERM (target) |
| Before SIGTERM, Kubernetes | preStop `sleep` hook | 5 s (target) |
| Kubernetes | `terminationGracePeriodSeconds` | 45 s (target) |
| systemd | `TimeoutStopSec` | 40 s (target) |

The GOAWAY pair follows RFC 9113 ([source](https://www.rfc-editor.org/rfc/rfc9113.html)); `Server.Shutdown` ignores hijacked connections such as WebSockets ([source](https://pkg.go.dev/net/http#Server.Shutdown)), so the Drain tracks them.

Kubernetes runs preStop before SIGTERM inside the grace period ([source](https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/)), 30 s by default ([source](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/)), too short here:
`terminationGracePeriodSeconds` MUST exceed the preStop wait plus the exit bound (Drain deadline plus flush), 45 s against 5 s plus 30 s (target), as `TimeoutStopSec` exceeds 30 s (target).
preStop covers EndpointSlice updates, which by analysis run concurrently with termination ([source](https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/)).
External balancers MUST detect a failed `/readyz` within the accept window, probe interval times unhealthy threshold at most 5 s (target); slower ones need OQ-zero-downtime-upgrades-and-hot-reload-1 (b) or (c) to lengthen it.
Routes with a `timeout` above the 20 s request deadline (target) are cut at it.

*Figure 2: drain timeline defaults on Kubernetes, in seconds from Pod deletion; every value is a target.*

```mermaid
gantt
    title Drain timeline defaults, seconds after Pod deletion (target)
    dateFormat X
    axisFormat %M:%S
    section Kubernetes
    preStop sleep while EndpointSlices update :k1, 0, 5s
    terminationGracePeriodSeconds             :k2, 0, 45s
    section Readiness
    Ready and serving                         :r1, 0, 5s
    readyz returns 503 after SIGTERM          :r2, 5, 30s
    section Listeners
    Accept window for balancer probes         :l1, 5, 5s
    Stopped accepting                         :l2, 10, 25s
    section HTTP/2 and HTTP/1.1
    First GOAWAY and Connection close         :milestone, g1, 10, 0s
    Final GOAWAY after one round trip         :g2, 10, 1s
    section Long-lived connections
    WebSocket 1001 and SSE end, jittered      :s1, 10, 10s
    section In-flight work
    Requests finish before the deadline       :w1, 10, 20s
    Flush post-commit queue and telemetry     :w2, 30, 5s
    section Margin
    Exited, margin before SIGKILL             :m1, 35, 10s
```

### Long-lived WebSocket and SSE connections

Long-lived connections never migrate, so they end early with a jittered reconnect signal ([Termination per protocol](../architecture/07-multi-protocol.md#termination-per-protocol)). These protocols are Planned (M3), the MQTT broker Planned (M4).

| Traffic | At stop-accepting | Within the 10 s jitter window (target) | At the drain deadline |
|---|---|---|---|
| WebSocket sessions | Keep flowing | Close 1001 Going Away to client and Upstream ([source](https://www.iana.org/assignments/websocket/websocket.xhtml)) | Closed |
| SSE subscriptions on non-`ai` Upstreams | Keep flowing | End with a `retry` jittered from 1 to 10 s (target); clients resume with `Last-Event-ID` ([source](https://html.spec.whatwg.org/multipage/server-sent-events.html)) | Ended |
| AI completions over SSE from `ai` Upstreams | Keep flowing: a cut without usage charges the full reservation (pack 8.9) | Keep flowing | Ended; charged per pack 8.9 |
| gRPC streams | GOAWAY; new calls go elsewhere | Keep flowing | Trailers with `UNAVAILABLE` |
| MQTT embedded broker sessions | Keep flowing | MQTT 5 DISCONNECT 0x8B; 3.1.1 connection closed | Closed |
| Other requests | GOAWAY; `Connection: close` | Finish | 503 with an `RZ-RT` code that Data plane registers (OQ-zero-downtime-upgrades-and-hot-reload-2) |

A Node holding 20,000 sessions sends about 2,000 reconnects per second over the window (hypothesis), so a step of M Nodes adds M × 2,000 per second to the others (hypothesis) (close 1012: OQ-zero-downtime-upgrades-and-hot-reload-8).

### Kubernetes Drain and restart

On T4, Planned (M2), an image change replaces Pods ([T4](01-deployment-topologies.md#t4-kubernetes-with-helm-hpa-and-crds)): the preStop `sleep` handler, beside exec and HTTP ([source](https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/)), waits 5 s (target), SIGTERM Drains within 30 s (target), and the new Pod boots from the same `${RURALZ_DATA_DIR}` volume by pack 8.2; `/readyz` gates the next step.

Each step replaces up to `maxUnavailable` Pods, M, one zone's share at minimum scale (target); a PodDisruptionBudget limits evictions, not the StatefulSet's own rolling update. Before a T4 upgrade, operators MUST add M Pods or confirm spare capacity for them, and halt the update on a zone incident, so a step and a zone loss together keep survivors under 80% CPU (target). Clusters with derived ceilings also pace as [Node count during upgrades](#node-count-during-upgrades) requires.

```text
Step         M of N Nodes out for 40 to 45 s; update takes ceil(N / M) × 45 s      (hypothesis)
Example      N = 1,000, minReplicas 300, three zones: M = 100, 10 steps, 7.5 min   (hypothesis)
Reconnects   M × 2,000 per second: 200,000 per second onto 900 Nodes              (hypothesis)
```

### Node lifecycle during an upgrade

*Figure 3: one Node, identified by `node.id`, through an in-place handover or a Kubernetes Drain and restart; the target binary is N for an upgrade and N-1 for a rollback.*

```mermaid
stateDiagram-v2
    state "Serving on the running binary, one process holds the lock" as ServingOld
    state "Overlap, target process verifying, compiling and warming, not listening" as Overlap
    state "Handing over, new connections steered to the target process" as Handover
    state "Serving on the target binary, any old process finishing its Drain" as ServingNew
    state "Draining before replacement" as Draining
    state "New Pod booting on the target binary" as Booting
    state "Detached, serving Last-Known-Good" as Detached
    state "Not ready" as NotReady
    [*] --> ServingOld
    ServingOld --> ServingOld: Hot Reload of a new Revision
    ServingOld --> Overlap: in-place upgrade or rollback, target process started
    Overlap --> ServingOld: refused or timed out, target process exits
    Overlap --> Handover: ready message accepted, listen, CBPF swapped
    Handover --> ServingNew: readiness, lock and Control Stream moved
    ServingOld --> Draining: Kubernetes rolling update, preStop then SIGTERM
    Draining --> Booting: old Pod exited
    Booting --> ServingNew: Revision loaded, Control Stream up
    Booting --> Detached: Control Stream absent after the boot wait
    Booting --> NotReady: no usable configuration, or RZ-CFG-024 on an N-1 target
    Detached --> ServingNew: Control Stream up
    NotReady --> ServingNew: Revision without N-only fields activated
    ServingNew --> [*]
```

### Skew and the binary rollback limit

Nodes of one Cluster may mix N and N-1 indefinitely, never newer than the Control Store version ([version skew](../engineering/04-release-versioning-and-compatibility.md#control-plane-and-data-plane-version-skew)); Ruralz Control refuses Rollouts with fields newer than the oldest Node serves (RZ-CFG-024). Going back to N-1 works only while the active Revision and Last-Known-Good use no N-only field; otherwise the N-1 process NACKs with RZ-CFG-024 and stays not ready, and a handover is refused ([Binary rollback limit](../engineering/04-release-versioning-and-compatibility.md#binary-rollback-limit)). Operators SHOULD soak N before a Bundle adopts a new field, and SHOULD NOT overlap Node upgrades with a Rollout to the same Cluster.

## Control plane upgrades

Ruralz Control upgrades follow [Ruralz Control upgrades](../engineering/04-release-versioning-and-compatibility.md#ruralz-control-upgrades) for the `raft` Control Store ([ADR-0006](../adr/0006-control-store-raft-boltdb.md), proposed), Planned (M2). N is the newest binary minor; the Control Store version stays N-1 until finalize, so replicas write N-1 formats and render at N-1 schema levels until then. One minor at a time:

1. **Prepare.** Every replica reported a snapshot at the Control Store version, and every Node runs N-1 (upgrade any N-2 Node to N-1 first); take `ruralz control backup` and pause `ruralz bundle push`.
2. **One replica at a time.** Move leadership off (OQ-deployment-topologies-6), replace the binary with N, and await `/readyz` on 9902, Raft catch-up and a recovered `ruralz_control_connected_nodes`; three voters keep a quorum of two. A failing replica returns to N-1 and the upgrade stops.
3. **Relays.** Then each regional relay, Planned (M4), one at a time; their Nodes stay on N-1.
4. **Finalize.** After every replica and relay reports N and a 24-hour soak (target), an operator finalizes (OQ-release-versioning-and-compatibility-8); migrations run as log entries and replicas snapshot.
5. **After finalize.** Apply `ruralz-crds.yaml` if it adds a served version ([ADR-0016](../adr/0016-kubernetes-helm-and-crds.md), proposed), pin the CI CLI to N, resume pushes, then upgrade Nodes one Cluster at a time.

A replica more than one minor above the Control Store version, or meeting an unknown entry type, stops before serving, so skipped minors never serve. With `postgres`, Planned (M4), finalize and migrations are transactions.

| Pair | In policy (N and N-1) |
|---|---|
| Ruralz Control and Nodes | N-1; N once the Control Store version is N |
| Replicas and relays | N-1 and N until finalize; N after |
| `ruralz` CLI | N or N-1; `ruralz bundle push` at the Control Store version |
| Nodes within one Cluster | Any mix of N and N-1, never newer than the Control Store version |

A replica replacement is a reconnect: the replica fails `/readyz` and sends `Reconnect` (`drain`), and its Nodes redial another with full jitter, base 1 s, cap 60 s (target), staying ready (pack 8.5, [ADR-0007](../adr/0007-control-stream-protocol.md)). Rollouts resume from the persisted plan, and ceil(Nodes / 5,000) + 1 replicas (hypothesis) keep one replica out from causing shedding. Until OQ-scalability-and-distributed-state-14 closes, a replacement unqualifies every Node it served, so Clusters with derived ceilings SHOULD declare per-Node ceilings first.

## State Store upgrades

Ruralz never upgrades the State Store, but every change to it runs under [State Store availability](../architecture/11-scalability-and-distributed-state.md#state-store-availability): at most one deadline per request, then `failureMode`, then no calls once the breaker opens.

| Change | Procedure | Effect on the Cell |
|---|---|---|
| Server patch or minor upgrade | One shard at a time, off peak: upgrade its replica, await near-zero replication lag (target), fail over, upgrade the old primary; only while another replica or the primary's zone is healthy | `failureMode` for about 15 s per failover (hypothesis): Rate Limits fail open, `closed` Token Budgets return 503 `RZ-STS`; scripts reload on reconnect |
| New deployment, such as an engine change | A parallel Cluster (new Cell) on the new State Store, traffic shifted by DNS or balancer weights | Counters start empty: one extra limit per open window (target) |
| Ruralz layout change in a release | Automatic ([State Store layout changes](../engineering/04-release-versioning-and-compatibility.md#state-store-layout-changes)) | None with mixed N-1 and N Nodes; up to 2x for one window after a rollback past lazy migration (hypothesis) |

A planned failover keeps [Scalability's bounds](../architecture/11-scalability-and-distributed-state.md#consistency-and-accuracy-bounds) for replication lag L:

```text
Per Cell        shards × 15 s of failureMode: 4 minutes at 16 shards              (hypothesis)
Rate Limit      L × a key's admitted rate extra; up to requests for a key
                first written within the lag                                     (hypothesis)
Token Budget    overspend up to L × reserved tokens per second per key           (hypothesis)
```

Around each change, `noeviction` MUST stay set where limit, Quota or Token Budget keys live, persistence SHOULD stay on for windows over one day, and TLS MUST stay on. `ai.semantic-cache`, Planned (M3), needs vector commands; without them it bypasses under its default `open` and fails as `RZ-STS-005` under `closed` (pack 7). Every Node of a Cluster MUST resolve the same State Store ([Cells and blast radius](../architecture/11-scalability-and-distributed-state.md#cells-and-blast-radius)), so changing `stateStore` or its `secretRef` value follows the new-Cell row (OQ-zero-downtime-upgrades-and-hot-reload-9).

## apiVersion migrations

A Bundle moves from `ruralz/v1alpha1` to `ruralz/v1beta1`, Planned (M3), by hub conversion, which MUST yield the same Revision and an empty `ruralz bundle diff` ([Hub-and-spoke conversion](../architecture/02-configuration-model.md#hub-and-spoke-conversion)), so Nodes receive nothing new.

1. **Upgrade** Ruralz Control, then Nodes, to a release serving the target apiVersion; a new CRD version is served only after finalize ([Migration tooling](../engineering/04-release-versioning-and-compatibility.md#migration-tooling)).
2. **Convert.** `ruralz bundle render --api-version ruralz/v1beta1 --output-dir <dir> --environments <file>` converts base resources and overlays together, since an overlay must match its base's apiVersion (RZ-CFG-030), and exits 1 unless every Environment's diff is empty; comments are dropped (OQ-configuration-model-7).
3. **Prove.** CI repeats that proof with `ruralz bundle build` per Environment and `ruralz bundle diff` against its Revision; merge only on an equal digest and an empty diff.

A Bundle MAY mix apiVersions meanwhile; `ruralz/v1alpha1` stays served at least 2 minor releases, about 6 months, after its successor ships (target), with RZ-CFG-025 warnings, then fails with RZ-CFG-007. Adopt a new field within an apiVersion only after every target Node serves its schema level ([Version skew](../architecture/02-configuration-model.md#version-skew)), since the first N-only field ends binary rollback. Plugin ABI and Control Stream majors overlap for 4 minor releases, about 12 months (target).

## Rollback

Every rollback below takes three steps or fewer, a chained path being one row; Figure 4 picks the row.

*Figure 4: choosing a rollback path by what changed.*

```mermaid
flowchart TD
    q1{"What changed?"}
    q2{"Control mode Rollout still active?"}
    q3{"File mode source?"}
    q4{"Revision and Last-Known-Good free of N-only fields?"}
    q5{"Ruralz Control finalized?"}
    q6{"Nodes already on N?"}
    r1["ruralz rollout rollback"]
    r2["Revert Rollout of the replaced Revision"]
    r3["Replace directory or republish the OCI Revision"]
    r4["Handover or image back to N-1"]
    r5["Row: binary past the rollback limit"]
    r6["Replace replicas back to N-1, one at a time"]
    r8["Row: Ruralz Control after finalize, Nodes at N"]
    r7["Restore: High availability and disaster recovery"]
    q1 -- "a Revision" --> q3
    q3 -- "no" --> q2
    q3 -- "yes" --> r3
    q2 -- "yes" --> r1
    q2 -- "no, complete" --> r2
    q1 -- "the ruralzd binary" --> q4
    q4 -- "yes" --> r4
    q4 -- "no" --> r5
    q1 -- "Ruralz Control" --> q5
    q5 -- "no" --> r6
    q5 -- "yes" --> q6
    q6 -- "yes" --> r8
    q6 -- "no" --> r7
```

| Changed | Step 1 | Step 2 | Step 3 |
|---|---|---|---|
| Revision; Rollout `canary`, `progressing` or `paused` | `ruralz rollout status prod-eu-west` finds the Rollout | `ruralz rollout rollback <rollout-id>` | `ruralz rollout status <rollout-id>` shows `rolled-back` |
| Revision; Rollout `complete` | `ruralz rollout start --cluster prod-eu-west --revision sha256:<previous>` with step-up TOTP | Another `approver` runs `ruralz rollout approve` if held with `RZ-CP-006` | `ruralz rollout status <rollout-id> --wait` reaches `complete` |
| Revision in a watched directory | Replace the directory atomically with the previous `ruralz bundle render --env <env>` output | Check each Node's digest with `ruralz node dump` | None |
| Revision from OCI | `ruralz bundle push --env <env> --oci <reference>` from the previous commit | After one poll interval, check digests | None |
| Binary, in-place | Start the N-1 binary beside the N process: a reverse handover | Confirm `/readyz` and the logged version | None |
| Binary on Kubernetes | Set the Node image back to the N-1 digest in the chart values | Watch every Pod replaced and ready | None |
| Binary past the rollback limit | `ruralz rollout start --cluster <name> --revision sha256:<last digest without N-only fields>`, approval included; file mode: that commit's directory or OCI Revision | `ruralz rollout status <rollout-id> --wait` reaches `complete`, promoting Last-Known-Good; file mode: digests match | Reverse handover or N-1 image |
| Ruralz Control before finalize | Replace each N replica with N-1, one at a time, leadership moved off first | Confirm `/readyz` on 9902 and Raft catch-up | None |
| Ruralz Control after finalize, Nodes at N | On the N deployment, steps 1 and 2 of the row above in every Cluster | Every Node back to N-1 by reverse handover or the N-1 image | Restore, re-applying N-1's `ruralz-crds.yaml` first on Kubernetes and redeploying relays at N-1 |

Control-mode rows are Planned (M2), directory rollback Planned (M1). The restore is rule 5 of [Ruralz Control upgrades](../engineering/04-release-versioning-and-compatibility.md#ruralz-control-upgrades): an empty N-1 deployment that loses later writes and leaves Nodes detached until a revocation list arrives, a recovery, not a rollback, which the 24-hour soak (target) exists to avoid. Rule 6 orders it once Nodes run N: this document's Rollout and handover steps, then the restore that [High availability and disaster recovery](04-high-availability-and-disaster-recovery.md) owns, so no Node runs newer than the restored Control Store version or boots a Last-Known-Good with N-only fields.

## Verification runbook

Commands carry their [CLI and API surface](../reference/01-cli-and-api-surface.md) milestones; metrics come from [Observability](../architecture/10-observability.md).

| When | Check | Pass |
|---|---|---|
| Before a Ruralz Control upgrade | `ruralz version`; `ruralz node list --cluster <name>`; `ruralz control backup --output-file <file>` | Every Node at N-1; digests equal the promoted digest; backup off the replicas; pushes paused |
| Before a Node upgrade, after finalize | `ruralz node list --cluster <name>` | Every Node at N-1 or N; digests equal the promoted digest |
| Before | `terminationGracePeriodSeconds` or `TimeoutStopSec`; balancer probe interval times unhealthy threshold | 45 s and 40 s; detection within 5 s (target) |
| During a Hot Reload or Rollout | `ruralz rollout status <cluster> --wait`; `ruralz_config_activation_duration_seconds` | No deterministic NACK; activation within budget for the size class (target) |
| During a handover or Drain | Balancer view of `/readyz`; 5xx, `RZ-RT-014` and Drain-deadline code counts; the old process's exit log | 200 throughout a handover; no connection left at the deadline |
| During any upgrade | `ruralz_control_cluster_nodes`; `ruralz_node_degraded_info{reason="state_store_breaker_open"}` | Drops at most 10% (target); no breaker opens |
| During a Ruralz Control upgrade | `/readyz` on 9902; `ruralz_control_connected_nodes` | Every Node reconnected, none shed, before the next replica |
| After | `ruralz node list`; `ruralz_control_drift_nodes`; `ruralz_node_degraded_info` | No Drift and no new degraded reason |

[Testing and quality strategy](../engineering/03-testing-and-quality-strategy.md) places these lab gates.

| ID | Test | Pass | Milestone |
|---|---|---|---|
| V-1 | R1 reload ladder; Revisions failing each gate step; three Rollouts in quick succession over 20,000 sessions (target) | Hot Reload budgets per step (target); rejected Revisions change nothing; reconnects spread over 10 s (target) once OQ-zero-downtime-upgrades-and-hot-reload-10 closes | Planned (M1) |
| V-2 | Plugin swap at 20,000 requests per second (target) | No `RZ-PLG-005`; each request stays on its version | Planned (M2) |
| V-3 | 1,000 handovers under S1 at half saturation (target), a keep-alive health checker on 9901, plus 1,000 refused or timed-out handovers | Zero refused or reset connections and failed requests; every probe 200; no OOM kill or State Store connection refusal; count drops at most 10% (target) | Planned (M1) |
| V-4 | CE-2: SIGTERM a Node holding HTTP/2 connections, WebSockets and SSE | Zero failed new requests; exit within 30 s (target) | Planned (M1); WebSockets and SSE Planned (M3) |
| V-5 | Ruralz Control N-1 to N with 100 Nodes under load (target) | Zero request errors; no Node unready; count drops at most 10% (target) and no breaker opens, as CE-7 and CE-10, given OQ-scalability-and-distributed-state-14 (a) | Planned (M2) |
| V-6 | Handover to N-1 past the binary rollback limit, then that three-step row | Refused, N keeps serving; the row ends on N-1 with no Node unready | Planned (M1) |
| V-7 | CE-5: State Store primary killed under at least 100 Nodes (target) | Every Node reconnected within 10 s and `failureMode` for at most 15 s (target); extra admissions within the lag bound | Planned (M1) |

## Open questions

| ID | Question | Options | Owner | Blocking? |
|---|---|---|---|---|
| OQ-zero-downtime-upgrades-and-hot-reload-1 | Where are the Drain timings and handover timeout set? | (a) Fixed defaults (current); (b) `RURALZ_*` process settings by pack amendment; (c) Gateway `spec` fields | configuration-model | No |
| OQ-zero-downtime-upgrades-and-hot-reload-2 | Which code ends work still running at the drain deadline? | (a) Data plane widens `RZ-RT-014`'s registered meaning to the Drain deadline; (b) A new `RZ-RT` code (recommended) | data-plane | No |
| OQ-zero-downtime-upgrades-and-hot-reload-3 | Can `net/http` send the first GOAWAY before `Server.Shutdown`, so the two-phase shutdown works? | (a) `Shutdown` alone, one GOAWAY (Planned (M1)); (b) Ruralz framing through non-deprecated `x/net/http2` APIs; (c) An upstream change | data-plane | No |
| OQ-zero-downtime-upgrades-and-hot-reload-4 | Which code NACKs a listener that cannot bind during a Hot Reload? | (a) A new transient `RZ-CFG` code; (b) An `RZ-RT` code | configuration-model | No |
| OQ-zero-downtime-upgrades-and-hot-reload-5 | Should a handover pass the published Node count, reopening OQ-traffic-management-and-resilience-22? | (a) Count only (proposed); (b) Count and rate-limit key table; (c) Neither (current) | zero-downtime-upgrades-and-hot-reload | No |
| OQ-zero-downtime-upgrades-and-hot-reload-6 | How do `GOMEMLIMIT` and host memory cover the two-process overlap of an in-place handover? | (a) Hosts sized for two full processes; (b) Shared ceilings, a soft limit reduced by the old RSS and free memory for the overlap extra (current); (c) Handover only on hosts without a memory limit | capacity-planning | Yes, for Planned (M1) in-place handover |
| OQ-zero-downtime-upgrades-and-hot-reload-7 | Which systemd unit directives keep the new process as the service's main process across a handover? | (a) Research, then a shipped unit and helper; (b) Documentation only | release-versioning-and-compatibility | Yes, for Planned (M1) handover on T5 |
| OQ-zero-downtime-upgrades-and-hot-reload-8 | Should Drain send WebSocket close 1012 Service Restart instead of 1001? | (a) 1001 everywhere (current, as Multi-protocol); (b) 1012 on Drain, 1001 on snapshot retirement | multi-protocol | No |
| OQ-zero-downtime-upgrades-and-hot-reload-9 | What does a Node do when a rotated `stateStore.url` value names another deployment? | (a) Keep the old connection until restart and warn; (b) Redial at once; (c) Require a Revision change | scalability-and-distributed-state | No |
| OQ-zero-downtime-upgrades-and-hot-reload-10 | Should a closing snapshot's stream ends spread over the 10 s jitter window (target), as the Drain does? | (a) Spread (proposed); (b) At once (current) | data-plane | No |
| OQ-zero-downtime-upgrades-and-hot-reload-11 | Should a handover keep the Node's stream and qualification: a `Hello` from the same certificate supersedes a stream whose last `Heartbeat` carried `draining`, and neither resets the 10-minute qualification? | (a) Both (proposed); (b) Supersede only; (c) Neither: retry `RZ-CP-002` and pace upgrades (current) | control-plane-and-gitops | No |
