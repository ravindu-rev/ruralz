---
id: ADR-0007
title: "Control Stream: own gRPC snapshot and delta protocol with xDS-style ACK/NACK"
status: accepted
date: 2026-09-25
deciders: [ruralz-core]
related:
  - docs/architecture/04-control-plane-and-gitops.md
  - docs/_meta/foundation-pack.md
  - docs/engineering/01-tech-stack-and-libraries.md
  - docs/architecture/01-system-overview.md
  - docs/architecture/02-configuration-model.md
  - docs/engineering/02-repository-layout-and-conventions.md
  - docs/engineering/03-testing-and-quality-strategy.md
  - docs/engineering/04-release-versioning-and-compatibility.md
---

# ADR-0007: Control Stream: own gRPC snapshot and delta protocol with xDS-style ACK/NACK

## Context and problem statement

In Control mode every Node gets its configuration over one channel, the Control Stream: snapshots, deltas and the promoted digest flow down; ACK or NACK and heartbeats flow up (pack [section 8.4](../_meta/foundation-pack.md#84-control-stream-ports-and-http3)). [Control plane and GitOps](../architecture/04-control-plane-and-gitops.md#control-stream) owns the messages; this ADR decides the protocol family and RPC library.

The unit of delivery is a whole Revision: one full `sha256:<64 hex>` digest is what Ruralz Control signs, a Node verifies and swaps atomically, the ACK reports, Last-Known-Good persists and Drift compares ([Canonical form and Revision](../architecture/02-configuration-model.md#canonical-form-and-revision)). Rollouts need per-Node ACK/NACK tied to one delivery, NACK classes, and reconnects that present the active digest, so a Node receives only its planned Revision (pack 8.2 to 8.4).

Nodes dial 8091, working behind NAT, and keep serving without the stream (P9, [Vision](../vision/01-vision-and-positioning.md#principles)). Which protocol and library carry this in `CGO_ENABLED=0` binaries ([ADR-0001](0001-implementation-language-go.md)) on `net/http` ([ADR-0009](0009-http-stack-net-http-quic-go.md))? The Control Stream is Planned (M2); a regional relay serving it, Planned (M4).

## Decision drivers

- **Whole-Revision atomicity**: one digest per delivery, verified and swapped at once.
- **Rollout semantics**: ACK means active, not durable; NACK carries digest, code and class; stale replies are recognizable.
- **Node dials, fail static**: no inbound connection to a Node; stream loss never fails `/readyz` (pack 8.5).
- **Bounded fan-out**: 10,000 Nodes per deployment (target), shed reconnect storms, paced Snapshots.
- **One RPC library, pure Go**: gates G1 to G3 of the [selection criteria](../engineering/01-tech-stack-and-libraries.md#selection-criteria).
- **Evolvable contract**: `ruralz.control.v1` is a stable surface ([Control Stream versioning](../engineering/04-release-versioning-and-compatibility.md#control-stream-versioning)).

## Considered options

1. **Own `ruralz.control.v1.ControlStream` on connect in gRPC mode at both ends**: unary `Enroll` plus one bidirectional `Stream` per Node carrying Snapshots and deltas of whole Revisions with nonce-based ACK/NACK. connect serves gRPC on `net/http` handlers ([source](https://github.com/connectrpc/connect-go/blob/main/README.md)); `buf breaking` checks protos ([source](https://github.com/bufbuild/buf)).
2. **xDS through go-control-plane**: the xDS State-of-the-World or Incremental (Delta) discovery protocol, ACK/NACK through echoed `version_info` and `response_nonce` ([source](https://github.com/cncf/xds)).
3. **The same protocol on grpc-go at both ends**: its native HTTP/2 server ([source](https://github.com/grpc/grpc-go)) or `Server.ServeHTTP` ([source](https://github.com/grpc/grpc-go/blob/master/server.go)).
4. **Node polling**: Nodes long-poll the REST API for their digest, like Consul blocking queries ([source](https://developer.hashicorp.com/consul/api-docs/features/blocking)), pull content by digest like file mode's oras-go ([source](https://github.com/oras-project/oras-go)), and post ACK, NACK and heartbeats separately.

## Decision outcome

Chosen option: "own `ruralz.control.v1.ControlStream` on connect in gRPC mode at both ends", because only it delivers the signed whole Revision named by one digest, keeps one ordered, Node-dialed stream per Node for ACK/NACK, heartbeats and backpressure, and uses the catalog's connect for every Ruralz-authored RPC, needing no second HTTP/2 server in `ruralz-control`. This matches the foundation pack [section 7](../_meta/foundation-pack.md#7-technology-decisions-fixed-details-in-docsengineering01-tech-stack-and-librariesmd-and-adrs) Control Stream row (own snapshot plus delta protocol, xDS-style ACK/NACK, connect in gRPC mode on both ends, `buf breaking`, not go-control-plane) and the [library catalog](../engineering/01-tech-stack-and-libraries.md#library-catalog). Rules, all Planned (M2):

| Element | Rule |
|---|---|
| Service | `ruralz.control.v1.ControlStream` with exactly two RPCs: unary `Enroll` (one-time token, server CA pinned; pack 8.4 amendment pending, OQ-security-and-identity-31) and bidirectional `Stream` (Node certificate over mTLS, the only source of `node.id`); Ruralz Control never dials a Node |
| Transport | `connectrpc.com/connect` v1.21.x, gRPC mode, HTTP/2 over TLS on 8091; no `ReadTimeout` or `WriteTimeout` on the stream; `HTTP2Config` pings detect dead peers |
| Delivery | Each carries `promotion` and a signed assignment naming one Revision by full digest. `Snapshot` sends canonical content in chunks of at most 1 MiB (target); `Delta` (`baseDigest`, `ops`) serves changes encoding within 1 MiB (target), and a mismatching result is `Nack` RZ-CFG-027, after which, as after a `Hello` whose Last-Known-Good or active content fails re-hash, the next delivery is a `Snapshot` |
| ACK/NACK | Each configuration message carries a 128-bit random `nonce`; stale nonces are ignored. `Ack` means active, not durable, and counts per (`node.id`, `storeEpoch`, `assignmentSeq`), not per stream: a `Hello` matching the pending assignment's digest, `storeEpoch` and `assignmentSeq` is its `Ack`, and a Node re-ACKs a re-sent assignment it runs, so a lost `Ack` never makes a Node lag. `Nack` keeps the active Revision and carries `code` and `class`; one Ruralz Control reproduces from the Revision is deterministic (pack 8.3) and fails the gate, else transient |
| Flow control | One unacknowledged configuration message per Node, a chunked Snapshot counting as one; a newer assignment replaces an unsent one; messages cap at 2 MiB (target) |
| Reconnect | `Hello` presents active and Last-Known-Good digests, `lastNonce`, `storeEpoch` and `assignmentSeq`; the answer is nothing, a `Delta` or a `Snapshot`; full-jitter redials, base 1 s, cap 60 s (target); excess streams get `Reconnect` or `RZ-CP-010` |
| Heartbeat | Every 15 s (target) up with digests, readiness and per-Rollout counters; `HeartbeatReply` down with the signed promotion and `clusterNodeCount` |
| Trust | mTLS authenticates the peer; content trust comes from signatures fenced by `storeEpoch` and sequences ([ADR-0017](0017-artifact-signing.md), [Fencing stale replicas](../architecture/04-control-plane-and-gitops.md#fencing-stale-replicas)); relays Planned (M4) |
| Evolution | Additive changes stay in `ruralz.control.v1`; a breaking change needs `ruralz.control.v2` on distinct gRPC paths, served beside v1 for about 12 months (target) |

*Figure 1: one Node's Control Stream, dial to steady state.*

```mermaid
sequenceDiagram
    autonumber
    participant N as Node
    participant R as Ruralz Control replica on 8091
    participant L as Ruralz Control leader
    N->>R: dial Stream over mTLS with the Node certificate
    N->>R: Hello with active and Last-Known-Good digests, lastNonce, storeEpoch, assignmentSeq
    R->>R: compare with the committed assignment for this Node
    alt active digest and sequences already current
        R->>R: record the implicit Ack for this assignment
        R-->>N: nothing to send
    else change from the active digest encodes small
        R-->>N: Delta with nonce, baseDigest, ops, signature and promotion
    else otherwise
        R-->>N: Snapshot chunks with nonce, offset, totalBytes, signature and promotion
    end
    N->>N: verify digest and signatures, compile, atomic swap
    alt activated
        N-->>R: Ack with nonce and digest, meaning active
    else rejected before any swap
        N-->>R: Nack with nonce, digest, code and class
    end
    R->>L: status forwarding of the Ack or Nack
    loop every heartbeatInterval
        N-->>R: Heartbeat with digests, readiness and counters
        R-->>N: HeartbeatReply with signed promotion and clusterNodeCount
    end
    R-->>N: Reconnect with retryAfter when shedding or draining
```

*Figure 2: how a replica answers a Hello; the active Revision stays until a verified swap.*

```mermaid
flowchart TD
    open["Node opens Stream on 8091"]
    bucket{"Per-replica token bucket has room?"}
    shed["Reconnect with retryAfter, or RZ-CP-010"]
    hello["Hello arrives"]
    ident{"Certificate valid, unrevoked, matching Hello, and node.id on no other stream?"}
    refuse["RZ-CP-002 or RZ-CP-003, stream closed; the Node redials with full jitter"]
    anchor{"Node anchor-set version older?"}
    trust["TrustUpdate first"]
    stale{"Replica stale?"}
    staleans["Last committed assignment only to a Node with no active digest; others get nothing and HeartbeatReply with the last promotion; Reconnect rebalance after 30 s stale (target)"]
    same{"Active digest, storeEpoch and assignmentSeq equal the committed assignment?"}
    none["Record the implicit Ack, send nothing; HeartbeatReply carries the promotion"]
    rehash{"Last Delta to this Node NACKed RZ-CFG-027, or re-hash failed?"}
    small{"Change encodes within 1 MiB (target)?"}
    delta["Delta from the active digest"]
    snap["Chunked Snapshot"]
    open --> bucket
    bucket -- no --> shed
    bucket -- yes --> hello --> ident
    ident -- no --> refuse
    ident -- yes --> anchor
    anchor -- yes --> trust --> stale
    anchor -- no --> stale
    stale -- yes --> staleans
    stale -- no --> same
    same -- yes --> none
    same -- no --> rehash
    rehash -- yes --> snap
    rehash -- no --> small
    small -- yes --> delta
    small -- no --> snap
```

### Consequences

- Good, because one digest names the unit from signing to ACK, Last-Known-Good and Drift, with no per-resource versions.
- Good, because an atomic swap needs no cross-resource ordering, whereas xDS orders CDS, EDS, LDS, then RDS to avoid blackholing traffic.
- Good, because connect, the catalog's gRPC ingress library (Planned (M3)), enters both binaries with the Control Stream (Planned (M2)) and ingress reuses it; `ruralz-control` serves 8091 on `net/http`.
- Bad, because Ruralz owns the specification, both implementations and their tests; no xDS client can consume a Revision.
- Bad, because pacing is Ruralz code: with 32 concurrent Snapshots and 256 MiB in flight per replica (target) at 20 Snapshots/s (hypothesis), a 10,000-Node Cluster's largest batch takes up to 5 minutes (target).
- Bad, because liveness relies on HTTP/2 pings and heartbeats, and at about 5,000 streams per replica (hypothesis) 10,000 Nodes need N+1 replicas.
- Bad, because a proxy in front of 8091 MUST pass HTTP/2 and TLS through, since the Node certificate authenticates the stream.
- Bad, because the protobuf runtime is unselected (OQ-tech-stack-and-libraries-14, blocking Planned (M2)), and the `Delta` `ops` encoding is unspecified.

### Confirmation

- **`buf lint` (stage 2) and `buf breaking` (stage 3) of `pr-fast`**, Planned (M2) for `ruralz.control.v1`: `buf breaking` runs against `main` and the last tag of each supported line ([buf rules](../engineering/02-repository-layout-and-conventions.md#buf-rules)).
- **Dependency admission**: go-control-plane has no catalog row ([Alternatives not chosen](../engineering/01-tech-stack-and-libraries.md#alternatives-not-chosen)), so [banned imports](../engineering/02-repository-layout-and-conventions.md#banned-imports) rejects it.
- **Container-free integration tests**, Planned (M2): snapshot and delta delivery, ACK and NACK classification, promoted digests ([Integration tests](../engineering/03-testing-and-quality-strategy.md#integration-tests)), a stale-nonce `Ack` changing nothing, a mismatching `Delta` NACKed with RZ-CFG-027 then a `Snapshot` ACKed, a Node that missed an empty-`ops` re-sign `Delta` receiving it after reconnecting, and a Node still ACKed after the replica holding its forwarded `Ack` dies before the bitmap commit.
- **Fuzzing and chaos**, Planned (M2): one fuzz target per message type ([Fuzzing](../engineering/03-testing-and-quality-strategy.md#fuzzing)); after leader failover each reconnecting Node gets only its planned Revision ([Chaos testing](../engineering/03-testing-and-quality-strategy.md#chaos-testing)).
- **Review checklist item**: a new message, field or RPC MUST update the owning document's message table; a grpc-go server or second channel to Nodes MUST supersede this ADR.

## Pros and cons of the options

### Own `ruralz.control.v1.ControlStream` on connect in gRPC mode at both ends

- Good, because connect is Apache-2.0 without CGO ([source](https://github.com/connectrpc/connect-go)), and its v1 module stays "stable and supported indefinitely" ([source](https://github.com/connectrpc/connect-go/blob/main/README.md)).
- Good, because `WithRequestGate` authenticates before decompression or unmarshalling ([source](https://github.com/connectrpc/connect-go/releases/tag/v1.21.0)), useful for token-only `Enroll`.
- Bad, because connect had 29 commits in 90 days against grpc-go's 112 ([source](https://github.com/connectrpc/connect-go)) ([source](https://github.com/grpc/grpc-go)).

### xDS through go-control-plane

- Good, because ACK/NACK with `error_detail` and Incremental updates are already specified ([source](https://github.com/cncf/xds)).
- Bad, because its unit is the typed xDS resource, not a Revision under one digest, so signing, Last-Known-Good and Drift need a layer on top, and State-of-the-World resends every LDS and CDS resource.
- Bad, because unchunked messages grow with resource count, so large route sets push against default gRPC message size caps.

### The same protocol on grpc-go at both ends

- Good, because grpc-go has a native HTTP/2 transport, more activity ([source](https://github.com/grpc/grpc-go)), and `ruralzd` links it for upstream clients from Planned (M3).
- Bad, because `ruralz-control` would run a second HTTP/2 server on 8091, or `ServeHTTP`, which "does not support some gRPC features available through grpc-go's HTTP/2 server" ([source](https://github.com/grpc/grpc-go/blob/master/server.go)).

### Node polling

- Good, because requests are short-lived and stateless, and pulls reuse file mode's digest path ([source](https://github.com/oras-project/oras-go)).
- Bad, because latency follows the poll window, 5 minutes by default in Consul ([source](https://developer.hashicorp.com/consul/api-docs/features/blocking)), and each delivery needs an extra fetch.
- Bad, because Ruralz Control cannot pace deliveries or send `Reconnect`, so canary timing follows poll intervals.

## More information

- Owning document: [Control plane and GitOps](../architecture/04-control-plane-and-gitops.md#control-stream), with [Heartbeat and backpressure](../architecture/04-control-plane-and-gitops.md#heartbeat-and-backpressure); Node side in [Compile before swap](../architecture/01-system-overview.md#compile-before-swap).
- Related decisions: [ADR-0003](0003-configuration-format.md) (canonical digest), [ADR-0006](0006-control-store-raft-boltdb.md) (committed assignments) and [ADR-0017](0017-artifact-signing.md) (signing).
- Open questions: OQ-tech-stack-and-libraries-14, OQ-release-versioning-and-compatibility-4 (`binaryVersion` in `Hello`), OQ-security-and-identity-31 (`Enroll` credential), OQ-control-plane-and-gitops-25 (Snapshot limit) and OQ-zero-downtime-upgrades-and-hot-reload-11 (stream supersession).
- Proposed amendments: Control plane and GitOps SHOULD add the implicit `Ack` to its `Ack` row and an Open question on the `Delta` `ops` encoding over `ruralz.canonical.v1`; Repository layout and conventions SHOULD ban go-control-plane explicitly and grpc-go servers in `ruralz-control` via forbidigo.
