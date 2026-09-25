# Go Protocol and Library Landscape

| Field | Value |
|---|---|
| Topic | Go libraries for GraphQL, Kafka, NATS, MQTT, WebSocket, SSE, tokenizers, Redis/Valkey vector search, GCRA rate limiting, testing, service discovery, OCI plugin distribution |
| Snapshot date | 2026-09-23 |
| Method | Primary sources only: GitHub READMEs, release pages and go.mod files, pkg.go.dev version tabs and vulnerability database, vendor docs (redis.io, valkey.io, developer.hashicorp.com, kubernetes.io, golang.testcontainers.org). Release dates are taken from pkg.go.dev version tabs where GitHub release pages and pkg.go.dev disagreed (see Gaps). "Health" is a snapshot judgment based on the date of the latest tag and stated maintenance notes, and is labelled as such. |

Per-library fields used throughout: latest version, date (ISO), license, health, CGO, limits.

## 1. GraphQL

| Library | Latest | Date | License | Health (snapshot) | CGO | Key limits |
|---|---|---|---|---|---|---|
| wundergraph/graphql-go-tools v2 | v2.22.1 | 2026-09-21 | MIT | Very active (7 tags 2026-09-08..2026-09-21) | No C deps in go.mod | Library, not a server |
| 99designs/gqlgen | v0.17.95 | 2026-09-01 | MIT | Active | Not stated | Code-gen server framework, not a gateway |
| movio/bramble | v1.4.23 | 2026-09-14 | MIT | Active, low cadence | Not stated | No subscriptions; own federation spec |
| graphql-go/graphql | v0.8.1 | 2023-04-10 | MIT | Stale (no tag since 2023) | Not stated | Older versions carry GO-2022-0942 |

Sources per row: graphql-go-tools (https://github.com/wundergraph/graphql-go-tools) (https://github.com/wundergraph/graphql-go-tools/releases); gqlgen (https://github.com/99designs/gqlgen) (https://github.com/99designs/gqlgen/releases); bramble (https://github.com/movio/bramble) (https://pkg.go.dev/github.com/movio/bramble?tab=versions); graphql-go (https://github.com/graphql-go/graphql) (https://pkg.go.dev/github.com/graphql-go/graphql?tab=versions).

### wundergraph/graphql-go-tools v2
- The v2 module is the only supported module; v1 is "deprecated and retracted" (https://github.com/wundergraph/graphql-go-tools).
- v2 contains lexer, parser, AST, validation, normalization, datasources, query planner and resolver (https://github.com/wundergraph/graphql-go-tools).
- Supports GraphQL Federation v1 and v2 with built-in batching of federation entity calls (https://github.com/wundergraph/graphql-go-tools).
- Subscriptions over graphql-ws, graphql-transport-ws and SSE (https://github.com/wundergraph/graphql-go-tools); the `subscription` package "implements GraphQL Subscriptions over WebSockets and SSE" (https://github.com/wundergraph/graphql-go-tools/tree/master/v2).
- Package layout: `ast`, `astvalidation`, `astnormalization`, `engine/plan`, `engine/resolve`, `engine/datasource/graphql_datasource` (federation-aware), `engine/datasource/staticdatasource` (https://github.com/wundergraph/graphql-go-tools/tree/master/v2).
- Stated limit: "graphql-go-tools is not a GraphQL server by itself"; it is a library for building routers and gateways (https://github.com/wundergraph/graphql-go-tools/tree/master/v2).
- It is the foundation of WunderGraph Cosmo Router (https://github.com/wundergraph/graphql-go-tools); Cosmo Router's go.mod requires `github.com/wundergraph/graphql-go-tools/v2 v2.22.1` with `go 1.25.0` (https://raw.githubusercontent.com/wundergraph/cosmo/main/router/go.mod).
- v2 go.mod declares `go 1.25.0` and depends on `github.com/coder/websocket v1.8.14`, `github.com/gorilla/websocket v1.5.1` and `connectrpc.com/connect v1.19.2` (https://raw.githubusercontent.com/wundergraph/graphql-go-tools/master/v2/go.mod).
- Maintenance is funded by paying WunderGraph customers, who steer features (https://github.com/wundergraph/graphql-go-tools).
- Recent changes: v2.22.0 (2026-09-20) added private-key support for response caching and cache tag invalidation; v2.22.1 (2026-09-21) fixed SSE transport context callbacks; v2.21.0 (2026-09-16) added caching in multi fetches (https://github.com/wundergraph/graphql-go-tools/releases).
- Repo stars at snapshot: 835 (https://github.com/wundergraph/graphql-go-tools).

### gqlgen
- Schema-first code generation from GraphQL SDL; 10.8k stars (https://github.com/99designs/gqlgen).
- v0.17.94 (2026-07-09) added opt-in multi-resolver entity optimization to the federation plugin and per-field computed `@requires` via `@computedRequires` (https://github.com/99designs/gqlgen/releases).
- v0.17.93 (2026-06-25) added per-event subscription context via `@subscriptionContext` (https://github.com/99designs/gqlgen/releases).
- v0.17.95 (2026-09-01) includes Go 1.27 compatibility adjustments and an `omit_resolver_embedding` option (https://github.com/99designs/gqlgen/releases).

### bramble
- Uses its own federation approach, not Apollo Federation; the README calls the Apollo federation specification "more complex than necessary" (https://github.com/movio/bramble).
- Documented limits: no subscription support; no shared unions, interfaces, scalars, enums or inputs across services (https://github.com/movio/bramble).
- Features: namespaces, field-level permissions, plugins (JWT, CORS), hot config reload, stateless horizontal scaling (https://github.com/movio/bramble).
- v1.4.23 (2026-09-14) is a dependency-bump release (gorilla/websocket 1.5.0 to 1.5.3, gRPC 1.83.x); the client timeout option (`WithTimeout`) dates from v1.4.18 (https://github.com/movio/bramble/releases/tag/v1.4.23) (https://pkg.go.dev/github.com/movio/bramble?tab=versions).

### graphql-go/graphql
- Port of graphql-js; supports queries, mutations and subscriptions; 10.1k stars (https://github.com/graphql-go/graphql).
- Versions v0.4.18 through v0.8.0 are affected by GO-2022-0942 (parser infinite recursion); v0.8.1 is the fix (https://pkg.go.dev/github.com/graphql-go/graphql?tab=versions).
- No federation support is documented (https://github.com/graphql-go/graphql).

## 2. Kafka

| Library | Latest | Date | License | Health (snapshot) | CGO | Kafka coverage |
|---|---|---|---|---|---|---|
| twmb/franz-go | v1.22.0 | 2026-09-18 | BSD-3-Clause | Very active | Pure Go | 0.8.0 through 4.4+ |
| IBM/sarama | v1.61.0 | 2026-09-22 | MIT | Active | Pure Go | "2 releases + 2 months" policy |
| segmentio/kafka-go | v0.4.51 | 2026-04-23 | MIT | Slow (4 tags since 2023-12) | Not stated | Tested 0.10.1.0 to 2.7.1 |

Sources per row: franz-go (https://github.com/twmb/franz-go) (https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo?tab=versions); sarama (https://github.com/IBM/sarama) (https://github.com/IBM/sarama/releases); kafka-go (https://github.com/segmentio/kafka-go) (https://pkg.go.dev/github.com/segmentio/kafka-go?tab=versions).

### Feature comparison

| Feature | franz-go | sarama | kafka-go |
|---|---|---|---|
| Idempotent + transactional producer / EOS | Yes, full EOS; `GroupTransactSession` for consume-modify-produce | Yes, via `Producer.Transaction` / idempotent config | Transaction protocol files present (`addoffsetstotxn.go`, `endtxn.go`); high-level transactional writer not verified |
| Compression | gzip, snappy, lz4, zstd | None, GZIP, Snappy, LZ4, ZSTD | Snappy, Gzip, LZ4, Zstd |
| SASL | GSSAPI/Kerberos, PLAIN, SCRAM, OAUTHBEARER | PLAIN, SCRAM-SHA-256, SCRAM-SHA-512, GSSAPI, OAUTHBEARER | PLAIN, SCRAM-SHA-256/512 |
| Consumer groups | Eager (roundrobin, range, sticky) and cooperative-sticky, rack-aware | Full; cooperative rebalancing (v1.61.0; cooperative-sticky assignor added in v1.60.1) | Yes; `SetOffset`, `Offset`, `Lag`, `ReadLag` unavailable in group mode |
| KIP-848 (next-gen group protocol) | ServerSideBalancer in v1.22.0 | ConsumerGroupDescribe API in v1.61.0 | Not documented |
| KIP-932 share groups | Yes (v1.21.0) | Not documented | Not documented |
| Admin / schema registry | `kadm`, `sr` subpackages | Cluster admin APIs, mocks subpackage | Low-level `Conn` API |

Sources: franz-go features (https://github.com/twmb/franz-go); franz-go EOS guidance (https://github.com/twmb/franz-go/blob/master/docs/producing-and-consuming.md); franz-go KIP history (https://raw.githubusercontent.com/twmb/franz-go/master/CHANGELOG.md); sarama constants (https://pkg.go.dev/github.com/IBM/sarama); sarama changes (https://github.com/IBM/sarama/releases); kafka-go features and limits (https://github.com/segmentio/kafka-go).

### Details
- franz-go claims support for "theoretically every non-Java-specific client-facing KIP" and says it is "consistently among the fastest and most cpu and memory efficient Kafka clients in Go"; the README gives no benchmark hardware (https://github.com/twmb/franz-go).
- franz-go changelog: v1.19.0 added Kafka 4.0, memory pooling, custom compression (`WithCompressor`/`WithDecompressor`), KIP-714 metrics, KIP-890 transactions and hidden KIP-848; v1.20.0 added Kafka 4.1 and switched the default producer linger from 0 ms to 10 ms; v1.21.0 added Kafka 4.2, KIP-932 share groups and KIP-881; v1.22.0 added Kafka 4.3/4.4, streaming compression, rack-aware partitioning and the KIP-848 ServerSideBalancer (https://raw.githubusercontent.com/twmb/franz-go/master/CHANGELOG.md).
- franz-go publishes no GitHub Releases; versions are git tags only (https://github.com/twmb/franz-go/releases) (https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo?tab=versions).
- franz-go producer default: the producing guide still says there is no linger and records are sent as soon as produced, but the v1.20.0 changelog switched the default to a 10 ms linger (`kgo.ProducerLinger(0)` restores the old behaviour); `ManualFlushing` with `MaxBufferedRecords` is available for batching (https://github.com/twmb/franz-go/blob/master/docs/producing-and-consuming.md) (https://raw.githubusercontent.com/twmb/franz-go/master/CHANGELOG.md).
- sarama supports "the two latest stable releases of Kafka and Go" with a 2-month grace period; 12.5k stars (https://github.com/IBM/sarama).
- sarama v1.60.1 (2026-07-29) added a cooperative sticky assignor and `TransactionClusterAdmin` for KIP-664 (https://github.com/IBM/sarama/releases).
- kafka-go requires Go 1.15+ per its README and states that newer Kafka API features "may not yet be implemented" (https://github.com/segmentio/kafka-go).
- Cosmo Router pins `github.com/twmb/franz-go v1.16.1` (https://raw.githubusercontent.com/wundergraph/cosmo/main/router/go.mod).

## 3. NATS: nats.go and jetstream

| Field | Value | Source |
|---|---|---|
| Latest | v1.54.0, 2026-09-18 | (https://pkg.go.dev/github.com/nats-io/nats.go?tab=versions) |
| Previous | v1.53.1 and v1.53.0 (2026-08-11), v1.52.0 (2026-05-07), v1.51.0 (2026-04-14) | (https://pkg.go.dev/github.com/nats-io/nats.go?tab=versions) |
| License | Apache-2.0 | (https://github.com/nats-io/nats.go) |
| Go support | "at least 2 latest minor Go versions"; v1.54.0 raised minimum Go to 1.26 | (https://github.com/nats-io/nats.go) (https://github.com/nats-io/nats.go/releases) |
| CGO | Not required per README (no C deps mentioned) | (https://github.com/nats-io/nats.go) |
| Server requirement for jetstream pkg | nats-server >= 2.9.0 | (https://github.com/nats-io/nats.go/blob/main/jetstream/README.md) |

- The `jetstream` package "replaces the legacy JetStream API" in the `nats` package (https://github.com/nats-io/nats.go).
- Consumption modes: `Fetch`/`FetchNoWait` (single batch RPC; documented as slower for continuous retrieval), `Messages()` (iterator), `Consume()` (callback with pre-buffering) (https://github.com/nats-io/nats.go/blob/main/jetstream/README.md).
- Pull consumers are the recommended default; ordered consumers are pull-only; push consumers remain mainly for migration and are callback-only (https://github.com/nats-io/nats.go/blob/main/jetstream/README.md).
- Pre-buffer setting (checked 2026-09-25 against v1.54.0): `PullMaxMessages` "limits the number of messages to be buffered in the client. If not provided, a default of 500 messages will be used"; it is exclusive with `PullMaxBytes` and configures both `Consume` and `Messages`. `PullThresholdMessages` triggers the next pull request and "Defaults to 50% of MaxMessages" (https://pkg.go.dev/github.com/nats-io/nats.go/jetstream). The source sets `DefaultMaxMessages = 500` and rounds the threshold up (https://raw.githubusercontent.com/nats-io/nats.go/main/jetstream/pull.go).
- `Consume()` calls the callback synchronously and only then decrements the pending count and checks the threshold for the next pull, so with `PullMaxMessages(1)` the next pull request is sent after the callback returns (https://raw.githubusercontent.com/nats-io/nats.go/main/jetstream/pull.go).
- The README recommends `Messages()` with `PullMaxMessages(1)` for one-by-one work-queue fetching "without optimizations and pre-buffering (to avoid redeliveries when processing messages at slow rate)" (https://github.com/nats-io/nats.go/blob/main/jetstream/README.md).
- In-progress acknowledgment: `Msg.InProgress()` "tells the server that this message is being worked on. It resets the redelivery timer on the server"; `Term()` stops redelivery "regardless of the value of MaxDeliver" (https://pkg.go.dev/github.com/nats-io/nats.go/jetstream) (https://raw.githubusercontent.com/nats-io/nats.go/main/jetstream/message.go). The wire acknowledgment is `+WPI` (https://raw.githubusercontent.com/nats-io/nats.go/main/jetstream/message.go).
- `ConsumerConfig` defaults: `AckWait` 30 s when unset; `MaxDeliver` -1 (unlimited) when unset; `BackOff` overrides `AckWait` (https://pkg.go.dev/github.com/nats-io/nats.go/jetstream).
- The server starts the `AckWait` timer when it sends a message to the client; `Ack()` or `Term()` deletes it and `inProgress()` resets it; on expiry the message is redelivered to any subscribed instance, with no guarantee of which (https://raw.githubusercontent.com/nats-io/nats.docs/master/using-nats/developing-with-nats/js/consumers.md). `MaxDeliver` -1 means "redeliver until acknowledged", and messages at the maximum delivery count stay in the stream; `MaxAckPending` defaults to 1000 (https://raw.githubusercontent.com/nats-io/nats.docs/master/nats-concepts/jetstream/consumers.md). The docs.nats.io renderings of these pages were not reachable from the research environment; the nats.docs repository sources were used.
- KeyValue and ObjectStore are built on JetStream streams, with watchers and history (https://github.com/nats-io/nats.go/blob/main/jetstream/README.md).
- v1.54.0 added `MultipathTCP()` and `ConnectedDomain()`, and fixed a header-parsing panic and a `Messages()` iterator issue after reconnects (https://github.com/nats-io/nats.go/releases).
- v1.53.0 added `WithPublishAsyncAckHandler` and fixed WebSocket connections with custom paths (https://github.com/nats-io/nats.go/releases).
- Cosmo Router pins `github.com/nats-io/nats.go v1.50.0` (https://raw.githubusercontent.com/wundergraph/cosmo/main/router/go.mod).

## 4. MQTT

| Library | Role | Latest | Date | License | Health (snapshot) | Protocol |
|---|---|---|---|---|---|---|
| eclipse/paho.golang | Client | v0.23.0 | 2025-09-06 | EPL-2.0 / EDL-1.0 | Pre-1.0, ~1 tag/year | MQTT v5 only |
| eclipse/paho.mqtt.golang | Client | v1.5.1 | 2025-09-16 | EPL-2.0 / EDL-1.0 | Maintenance | MQTT 3.1 / 3.1.1 |
| mochi-mqtt/server/v2 | Embedded broker | v2.7.9 | 2025-03-01 | MIT | No tag for ~18 months | v5, v3.1.1, v3.0.0 |

Sources per row: paho.golang (https://github.com/eclipse-paho/paho.golang) (https://pkg.go.dev/github.com/eclipse/paho.golang?tab=versions); paho.mqtt.golang (https://github.com/eclipse-paho/paho.mqtt.golang) (https://pkg.go.dev/github.com/eclipse/paho.mqtt.golang?tab=versions); mochi (https://github.com/mochi-mqtt/server) (https://pkg.go.dev/github.com/mochi-mqtt/server/v2?tab=versions).

### paho.golang (v5)
- The Go module path is still `github.com/eclipse/paho.golang` although the repo now lives under eclipse-paho (https://pkg.go.dev/github.com/eclipse/paho.golang?tab=versions).
- Packages: `paho` (low-level) and `autopaho` (connection management, reconnect, queue); new users are told to "begin with autopaho" (https://github.com/eclipse-paho/paho.golang).
- v0.20 introduced session state persistence and full QoS 1/2 support, with breaking changes from v0.12 (https://github.com/eclipse-paho/paho.golang).
- Documented limits: topic aliases in queued messages do not persist across reconnects; the client does not enforce the server's Receive Maximum for inbound messages; Session Expiry handling is limited; `ACK()` after connection loss is unpredictable; the `Router` in `ClientConfig` is slated for removal in favour of `OnPublishReceived` (https://github.com/eclipse-paho/paho.golang).
- autopaho `ClientConfig` fields include `ServerUrls`, `KeepAlive`, `ReconnectBackoff` (replacing the deprecated `ConnectRetryDelay`), `OnConnectionUp`, `OnConnectError`, `CleanStartOnInitialConnection`, `SessionExpiryInterval` and `Queue`; documented schemes include `mqtt` and `tls`, with WebSocket via `WebSocketCfg` (https://pkg.go.dev/github.com/eclipse/paho.golang/autopaho).

### paho.mqtt.golang (v3)
- Implements MQTT 3.1 and 3.1.1 and points to paho.golang for v5 (https://github.com/eclipse-paho/paho.mqtt.golang).
- Stores: memory, ordered memory, file (https://github.com/eclipse-paho/paho.mqtt.golang).
- The README warns that reusing a `Client` after `Disconnect` is not safe (https://github.com/eclipse-paho/paho.mqtt.golang).
- v1.5.1 (2025-09-16) added connection notification handlers (https://pkg.go.dev/github.com/eclipse/paho.mqtt.golang?tab=versions).

### mochi-mqtt/server (embedded broker)
- Listeners: TCP, WebSocket (incl. TLS), Unix socket, HTTP `$SYS` stats, HTTP health check (https://github.com/mochi-mqtt/server).
- Hook events include `OnStarted`, `OnStopped`, `OnSysInfoTick`, `OnConnectAuthenticate`, `OnACLCheck`, `OnConnect`, `OnSessionEstablish`, `OnSessionEstablished`, `OnDisconnect`, `OnPacketRead`, `OnPacketEncode`, `OnPacketSent`, `OnPacketProcessed`, `OnPublish`, `OnPublished`, `OnSubscribe`, `OnSubscribed`, `OnUnsubscribe`, `OnQosPublish`, `OnQosComplete`, `OnQosDropped`, `OnWill`, `OnRetainMessage` and the `Stored*` loaders (https://pkg.go.dev/github.com/mochi-mqtt/server/v2) (https://github.com/mochi-mqtt/server).
- Built-in auth: `AllowHook` (allow-all, development only) and a rule-based auth/ACL ledger (https://github.com/mochi-mqtt/server).
- Storage hooks: Redis (go-redis v8), BadgerDB, PebbleDB; BoltDB is deprecated in favour of Badger (https://github.com/mochi-mqtt/server).
- The inline client publishes and subscribes in-process and bypasses ACL checks (https://github.com/mochi-mqtt/server).
- Capabilities include `MaximumMessageExpiryInterval` (default 86400 s), `MaximumClientWritesPending`, `MaximumPacketSize`, `ReceiveMaximum` and `TopicAliasMaximum` (https://pkg.go.dev/github.com/mochi-mqtt/server/v2).
- Benchmark (Mochi v2.2.10, Apple MacBook Air M2, mqtt-stresser): 2 clients x 10k messages gave a median score of 125,456 publish and 313,186 receive; 100 clients x 10k messages gave a median of 4,425 publish and 7,274 receive. The README warns these mqtt-stresser values "are not representative of true messages per second throughput" (https://github.com/mochi-mqtt/server).
- Passes the Paho interoperability tests (https://pkg.go.dev/github.com/mochi-mqtt/server/v2).

## 5. WebSocket

| Library | Latest | Date | License | Health (snapshot) | Notable |
|---|---|---|---|---|---|
| gorilla/websocket | v1.5.3 | 2024-06-14 | BSD-2-Clause | No tag since 2024-06; "new maintainers needed" issue #370 closed 2022-12 | GO-2026-6278 affects < v1.5.3 |
| coder/websocket | v1.8.15 | 2026-06-15 | ISC | Active (Coder-maintained since 2024) | Zero deps, context-first, Wasm |
| gobwas/ws | v1.4.0 | 2024-05-03 | MIT | No tag since 2024-05 | Zero-copy upgrade, low-level |

Sources per row: gorilla (https://github.com/gorilla/websocket) (https://pkg.go.dev/github.com/gorilla/websocket?tab=versions); coder (https://github.com/coder/websocket) (https://pkg.go.dev/github.com/coder/websocket?tab=versions); gobwas (https://github.com/gobwas/ws) (https://pkg.go.dev/github.com/gobwas/ws?tab=versions).

- gorilla/websocket passes the Autobahn server tests (https://github.com/gorilla/websocket). Its package docs describe permessage-deflate (RFC 7692) as experimental and limited (no context takeover), and allow one concurrent reader and one concurrent writer per connection (https://pkg.go.dev/github.com/gorilla/websocket).
- GO-2026-6278 / GHSA-w67g-5rqw-f597 (published 2026-08-25, status "unreviewed") reports a weak PRNG for mask keys; it affects versions before v1.5.3 and is fixed in v1.5.3 (https://pkg.go.dev/vuln/GO-2026-6278). The pkg.go.dev versions tab flags v1.5.2 and earlier (not v1.5.3), which matches the advisory's range (https://pkg.go.dev/github.com/gorilla/websocket?tab=versions).
- Issue #370, "New maintainers needed" (opened 2018-04-01), was closed on 2022-12-09; the README does not reference it (https://github.com/gorilla/websocket/issues/370).
- Cosmo Router and graphql-go-tools v2 both pin gorilla/websocket v1.5.1, which is inside the GO-2026-6278 affected range (https://raw.githubusercontent.com/wundergraph/cosmo/main/router/go.mod) (https://raw.githubusercontent.com/wundergraph/graphql-go-tools/master/v2/go.mod) (https://pkg.go.dev/vuln/GO-2026-6278).
- coder/websocket was formerly nhooyr.io/websocket and was "adopted by Coder in 2024". It has zero dependencies, first-class `context.Context`, concurrent writes, a `net.Conn` wrapper, `wsjson`, RFC 7692 permessage-deflate and Wasm compilation, and it passes Autobahn (https://github.com/coder/websocket).
- coder/websocket claims its masking is 1.75x faster than gorilla's in pure Go (https://github.com/coder/websocket).
- coder/websocket v1.8.14 (2025-09-05) added `ErrMessageTooBig`; v1.8.13 (2025-03-14) added ping/pong callbacks to `AcceptOptions`/`DialOptions` (https://pkg.go.dev/github.com/coder/websocket?tab=versions).
- gobwas/ws offers a zero-copy HTTP upgrade, no intermediate allocations, a low-level frame API, `wsutil` helpers and `wsflate` (RFC 7692); it passes Autobahn and has ~78% test coverage (https://github.com/gobwas/ws).

## 6. Server-Sent Events

| Option | Latest | Date | License | Health (snapshot) |
|---|---|---|---|---|
| stdlib `net/http` + `http.ResponseController` | Go 1.20+ (`EnableFullDuplex` Go 1.21) | n/a | BSD-3-Clause (Go) | Core |
| tmaxmax/go-sse | v0.11.0 | 2025-05-13 | MIT | Pre-1.0; README recommends tagged releases |
| r3labs/sse/v2 | v2.10.0 | 2023-01-18 | MPL-2.0 | Stale (no tag since 2023-01) |

Sources per row: stdlib (https://pkg.go.dev/net/http#ResponseController); go-sse (https://github.com/tmaxmax/go-sse) (https://pkg.go.dev/github.com/tmaxmax/go-sse?tab=versions); r3labs (https://github.com/r3labs/sse) (https://pkg.go.dev/github.com/r3labs/sse/v2?tab=versions).

- `http.ResponseController` (Go 1.20) provides `Flush()`, `SetWriteDeadline()` and `SetReadDeadline()`; `EnableFullDuplex()` (Go 1.21) permits reading the request body while writing the response (https://pkg.go.dev/net/http#ResponseController).
- go-sse server side has a pluggable `Provider` (built-in in-memory "Joe") and replay via `ValidReplayer`/`FiniteReplayer`; its client reconnects automatically with backoff and resumes with `Last-Event-ID` (https://github.com/tmaxmax/go-sse).
- The go-sse README states that master contains unreleased changes and recommends tagged versions; it supports the two most recent Go releases (https://github.com/tmaxmax/go-sse).
- r3labs/sse v2.10.0 added `TryPublish` and client `LastEventID` (https://pkg.go.dev/github.com/r3labs/sse/v2?tab=versions).
- graphql-go-tools v2 ships its own SSE subscription transport (https://github.com/wundergraph/graphql-go-tools/tree/master/v2).

## 7. Tokenizers

| Library | Latest | Date | License | Vocab loading | Encodings |
|---|---|---|---|---|---|
| tiktoken-go/tokenizer | v0.8.1 | 2026-07-17 | MIT | Embedded as Go maps at build (~4 MB) | gpt2, r50k_base, p50k_base, p50k_edit, cl100k_base, o200k_base |
| pkoukk/tiktoken-go | v0.1.8 | 2025-09-10 | MIT | Downloads at runtime; cache via `TIKTOKEN_CACHE_DIR`; offline via separate tiktoken-go-loader | o200k_base, cl100k_base, p50k_base, r50k_base/gpt2 |

Sources per row: tokenizer (https://github.com/tiktoken-go/tokenizer) (https://pkg.go.dev/github.com/tiktoken-go/tokenizer) (https://pkg.go.dev/github.com/tiktoken-go/tokenizer?tab=versions); pkoukk (https://github.com/pkoukk/tiktoken-go) (https://pkg.go.dev/github.com/pkoukk/tiktoken-go?tab=versions) (https://github.com/pkoukk/tiktoken-go-loader).

- tiktoken-go/tokenizer model constants cover OpenAI models only: O1, O1-preview, O1-mini, O3, O3-mini, O4-mini, GPT-5 variants, GPT-4o, GPT-4, GPT-4.1, GPT-3.5, embeddings and legacy models (https://pkg.go.dev/github.com/tiktoken-go/tokenizer).
- tiktoken-go/tokenizer v0.7.0 (2025-08-08) added GPT-4.1, GPT-5 and O-series constants (https://pkg.go.dev/github.com/tiktoken-go/tokenizer?tab=versions).
- Neither README documents Anthropic, Gemini, Llama or Mistral tokenizers (https://github.com/tiktoken-go/tokenizer) (https://github.com/pkoukk/tiktoken-go).
- pkoukk/tiktoken-go benchmark: cl100k_base ~8,795 ns/op against Python's ~8,838 ns/op (macOS 13.2, Apple M1); o200k_base ~108,522 ns/op against Python's ~70,198 ns/op, and cl100k_base ~94,502 against ~54,642 ns/op (Ubuntu 22.04, AMD Ryzen 9 5900HS) (https://github.com/pkoukk/tiktoken-go).
- pkoukk/tiktoken-go-loader is MIT-licensed, has 5 commits and plugs in via `tiktoken.SetBpeLoader` (https://github.com/pkoukk/tiktoken-go-loader).

## 8. Redis 8 Vector Sets and valkey-search

### Redis Vector Sets

| Command | Since | Purpose |
|---|---|---|
| VADD | 8.0.0 | Add/update element vector |
| VSIM | 8.0.0 | Similarity search (with FILTER) |
| VREM, VCARD, VDIM, VEMB, VINFO, VLINKS, VRANDMEMBER | 8.0.0 | Remove / inspect |
| VSETATTR, VGETATTR | 8.0.0 | JSON attributes |
| VISMEMBER | 8.2.0 | Membership |
| VRANGE | 8.4.0 | Lexicographic range |

Source: (https://redis.io/docs/latest/develop/data-types/vector-sets/).

- VADD syntax: `VADD key [REDUCE dim] (FP32 | VALUES num) vector element [CAS] [NOQUANT | Q8 | BIN] [EF build-exploration-factor] [SETATTR attributes] [M numlinks]`, O(log N) per element (https://redis.io/docs/latest/commands/vadd/).
- VADD defaults and options: int8 quantization (Q8) is the default; EF defaults to 200 and M to 16 (layer 0 uses M*2 links; M=64 uses at least 1024 bytes at layer 0 and ~1193 bytes per node in total); FP32 blobs must be little-endian; `REDUCE` applies random projection; `CAS` runs neighbour collection in a background thread (https://redis.io/docs/latest/commands/vadd/).
- VSIM syntax: `VSIM key (ELE | FP32 | VALUES num) (vector | element) [WITHSCORES] [WITHATTRIBS] [COUNT num] [EPSILON delta] [EF search-exploration-factor] [FILTER expression] [FILTER-EF max-filtering-effort] [TRUTH] [NOTHREAD]`. Scores run from 1 (identical) to 0 (opposite); `TRUTH` forces an O(N) linear scan; searches run on a background thread unless `NOTHREAD` is given (https://redis.io/docs/latest/commands/vsim/).
- VADD and VSIM are supported on Redis Software and Redis Cloud Standard databases, but not on Active-Active (https://redis.io/docs/latest/commands/vadd/) (https://redis.io/docs/latest/commands/vsim/).
- Redis 8.4 (listed under 8.4-RC1, November 2025; 8.4.0 GA followed the same month) optimized VADD/VSIM with AVX2/AVX512 dot products (https://redis.io/docs/latest/operate/oss_and_stack/stack-with-enterprise/release-notes/redisce/redisos-8.4-release-notes/).
- Redis 8.4.6 (August 2026) fixed three Vector Sets security issues, including a use-after-free when `VREM` mutates the HNSW graph during a background `VSIM`; 8.4.3 (May 2026) fixed a VADD crash on large `REDUCE` values (https://redis.io/docs/latest/operate/oss_and_stack/stack-with-enterprise/release-notes/redisce/redisos-8.4-release-notes/).
- License: Redis Open Source >= 8.0.0 is tri-licensed RSALv2 or SSPLv1 or AGPLv3; 7.4.x is RSALv2/SSPLv1; <= 7.2 is BSD-3-Clause (https://redis.io/legal/licenses/).

### valkey-search

| Field | Value | Source |
|---|---|---|
| License | BSD-3-Clause | (https://github.com/valkey-io/valkey-search) (https://valkey.io/blog/valkey-search-1_2/) |
| Latest | 1.2.1 (2026-07-07 per the release page timestamp) | (https://github.com/valkey-io/valkey-search/releases) |
| 1.2.0 | Announced 2026-03-17 with full-text search | (https://valkey.io/blog/valkey-search-1_2/) |
| Core requirement | 1.2.x requires Valkey 9.0.1+ | (https://github.com/valkey-io/valkey-search/releases) |
| Commands | FT.CREATE, FT.DROPINDEX, FT.INFO, FT._LIST, FT.SEARCH, FT.AGGREGATE | (https://github.com/valkey-io/valkey-search) |

- Vector search offers HNSW ANN and exact KNN over Valkey Hash or Valkey-JSON, with numeric, tag and full-text filters. A query planner chooses between pre-filtering and inline filtering. It runs in standalone and cluster mode (https://github.com/valkey-io/valkey-search).
- 1.2 adds full-text search (prefix, suffix, wildcard, fuzzy), tag search, numeric ranges, FT.AGGREGATE with COUNT/SUM/AVG reducers, and hybrid queries (https://valkey.io/blog/valkey-search-1_2/).
- 1.1.0 made the vector component optional in indexes and added FT.AGGREGATE; 1.2.0 added `SORTBY` on FT.SEARCH and `SKIPINITIALSCAN` on FT.CREATE (https://github.com/valkey-io/valkey-search/releases) (https://valkey.io/topics/search/).
- The Valkey 9.1 press release (dated 2026-05-19) lists Valkey Search 1.2 among recent launches "alongside 9.1"; 1.2.0 itself was tagged on 2026-03-17 (https://www.linuxfoundation.org/press/valkey-enhances-efficiency-security-and-modular-performance-with-9.1-release-and-new-ecosystem-integrations).
- Performance claim (no hardware given): "single-digit millisecond latency" and "billions of vectors with over 99% recall" (https://github.com/valkey-io/valkey-search).
- The two command surfaces differ: Redis Vector Sets use `V*` commands on a native type, while valkey-search uses `FT.*` index commands (https://redis.io/docs/latest/develop/data-types/vector-sets/) (https://github.com/valkey-io/valkey-search).

## 9. Rate limiting: GCRA and Lua atomicity

| Library | Latest | Date | License | Algorithm | Redis atomicity |
|---|---|---|---|---|---|
| go-redis/redis_rate/v10 | v10.0.1 | 2023-04-02 | BSD-2-Clause | GCRA | Single Lua script using server `TIME` |
| throttled/throttled/v2 | v2.15.0 | 2025-08-23 | BSD-3-Clause | GCRA | Lua CAS script; `SetIfNotExistsWithTTL` is SETNX + EXPIRE (not atomic) |

Sources per row: redis_rate (https://github.com/go-redis/redis_rate) (https://pkg.go.dev/github.com/go-redis/redis_rate/v10?tab=versions) (https://raw.githubusercontent.com/go-redis/redis_rate/v10/lua.go); throttled (https://github.com/throttled/throttled) (https://pkg.go.dev/github.com/throttled/throttled/v2?tab=versions) (https://raw.githubusercontent.com/throttled/throttled/master/store/goredisstore/goredisstore.go).

- redis_rate Lua script: KEYS[1] is the rate-limit key and ARGV holds burst, rate, period and cost. It reads the clock with `redis.call("TIME")` (epoch offset to 2017-01-01), sets `tat = max(tat, now)`, adds `emission_interval * cost`, stores the result with `SET key new_tat EX ceil(reset_after)`, and calls `redis.replicate_commands()` (https://raw.githubusercontent.com/go-redis/redis_rate/v10/lua.go).
- redis_rate requires Redis 3.2+ (for `replicate_commands`) and is built on go-redis (https://github.com/go-redis/redis_rate).
- throttled stores are `memstore`, `goredisstore` and `redigostore` (https://github.com/throttled/throttled); goredisstore imports the unversioned `github.com/go-redis/redis` (https://raw.githubusercontent.com/throttled/throttled/master/store/goredisstore/goredisstore.go).
- Redis guarantees atomic script execution: "all server activities are blocked during its entire runtime" (https://redis.io/docs/latest/develop/programmability/eval-intro/).
- Every key a script accesses must be passed in KEYS for correctness on both standalone and cluster (https://redis.io/docs/latest/develop/programmability/eval-intro/).
- The script cache is volatile and is lost on restart, failover or `SCRIPT FLUSH`. `EVALSHA` returns `NOSCRIPT` when the script is missing, and clients should fall back to `EVAL` inside pipelines (https://redis.io/docs/latest/develop/programmability/eval-intro/).
- Effects replication has been the default since Redis 5.0, and verbatim replication was removed in 7.0. `redis.replicate_commands()` is deprecated as of 7.0 and always succeeds. `TIME` is allowed in scripts under effects replication (https://redis.io/docs/latest/develop/programmability/eval-intro/).
- Scripts with a `#!lua` shebang cannot access keys in different hash slots by default (https://redis.io/docs/latest/develop/programmability/eval-intro/).
- Once memory is over `maxmemory`, the first write that needs more memory aborts the script unless `redis.pcall` is used (https://redis.io/docs/latest/develop/programmability/eval-intro/).
- Redis 8.4 added `DIGEST`, `DELEX`, `SET` compare-and-set/compare-and-delete extensions, and `MSETEX` (https://redis.io/docs/latest/operate/oss_and_stack/stack-with-enterprise/release-notes/redisce/redisos-8.4-release-notes/).

## 10. Testing: testcontainers-go and buf

| Tool | Latest | Date | License |
|---|---|---|---|
| testcontainers-go | v0.44.0 | 2026-08-07 | MIT |
| bufbuild/buf | v1.73.0 | 2026-09-11 | Apache-2.0 |

Sources: (https://pkg.go.dev/github.com/testcontainers/testcontainers-go?tab=versions) (https://github.com/testcontainers/testcontainers-go) (https://github.com/bufbuild/buf/releases) (https://github.com/bufbuild/buf).

| Module | Import path | Default/example image | Since | Helper |
|---|---|---|---|---|
| Redis, Valkey | `modules/redis`, `modules/valkey` | not checked | not checked | not checked |
| Kafka (KRaft) | `modules/kafka` | `confluentinc/confluent-local:7.5.0` (min 7.4.0) | v0.24.0 | `Brokers(ctx)`, `WithClusterID` |
| Redpanda | `modules/redpanda` | not checked | not checked | not checked |
| NATS | `modules/nats` | `nats:2.9` | v0.24.0 | `ConnectionString(ctx)`, `WithConfigFile` (v0.35.0) |
| Mosquitto (MQTT) | `modules/mosquitto` | `eclipse-mosquitto:2` | v0.44.0 | `BrokerURL(ctx)`, `WithConfigFile` |
| Consul, Registry, K3s | `modules/consul`, `modules/registry`, `modules/k3s` | not checked | not checked | not checked |

Sources: module catalog (https://golang.testcontainers.org/modules/); Kafka (https://golang.testcontainers.org/modules/kafka/); NATS (https://golang.testcontainers.org/modules/nats/); Mosquitto (https://golang.testcontainers.org/modules/mosquitto/).

- The catalog lists no HiveMQ or EMQX module (https://golang.testcontainers.org/modules/).
- The Mosquitto module injects a default config that enables anonymous connections on port 1883 (https://golang.testcontainers.org/modules/mosquitto/).
- testcontainers-go v0.42.0 (2026-04-09) migrated to Moby modules (breaking); v0.43.0 (2026-06-19) changed `wait.ForSQL` callbacks (breaking) (https://github.com/testcontainers/testcontainers-go/releases) (https://pkg.go.dev/github.com/testcontainers/testcontainers-go?tab=versions).
- buf provides `buf lint` (40+ rules), `buf breaking` (FILE, PACKAGE, WIRE_JSON and WIRE categories), `buf generate` (local and BSR remote plugins), `buf format`, `buf curl` and an LSP. Its internal compiler is "tested against protoc descriptor output" (https://github.com/bufbuild/buf).
- buf v1.73.0 updated the built-in Well-Known Types to Protobuf v35.1 and fixed managed mode for Edition 2024 files (https://github.com/bufbuild/buf/releases).

## 11. Service discovery clients

| Mechanism | Go package | Latest | License | Watch model |
|---|---|---|---|---|
| Consul | `github.com/hashicorp/consul/api` | v1.34.5 (2026-09-10) | MPL-2.0 (api module) | HTTP blocking queries |
| Kubernetes EndpointSlice | `k8s.io/client-go/informers/discovery/v1` | v0.37.0 (2026-08-26) | Apache-2.0 | Shared informer (list+watch) |
| DNS SRV | stdlib `net` | Go toolchain | BSD-3-Clause | Poll |

Sources per row: Consul (https://pkg.go.dev/github.com/hashicorp/consul/api?tab=versions) (https://raw.githubusercontent.com/hashicorp/consul/main/api/LICENSE); client-go (https://pkg.go.dev/k8s.io/client-go/informers/discovery/v1); net (https://pkg.go.dev/net).

- The Consul `api` module LICENSE is MPL-2.0 ("Copyright (c) 2020 HashiCorp, Inc.") (https://raw.githubusercontent.com/hashicorp/consul/main/api/LICENSE).
- Consul blocking queries take `index` from `X-Consul-Index`. `wait` defaults to 5 min with a 10 min maximum, plus jitter of up to `wait/16`. Hash-based blocking uses `X-Consul-ContentHash`. Clients must reset the index to 0 if it goes backwards and should check that it is > 0 (https://developer.hashicorp.com/consul/api-docs/features/blocking).
- EndpointSlice (https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/):
  - API group `discovery.k8s.io/v1`, stable since Kubernetes v1.21.
  - Default of 100 endpoints per slice, configurable up to 1000 with `--max-endpoints-per-slice`.
  - Slices carry the label `kubernetes.io/service-name`.
  - Endpoint conditions are `ready`, `serving` and `terminating`; `serving` and `terminating` have been stable since v1.26.
  - Address types are IPv4, IPv6 and FQDN; a dual-stack Service has at least two slices.
  - Each endpoint carries `nodeName` and `zone`.
- In client-go, `EndpointSliceInformer` exposes `Informer()` and `Lister()`, and `NewFilteredEndpointSliceInformer` accepts a list-options tweak function. The docs recommend a shared informer factory. `NewEndpointSliceInformerWithOptions` was added in v0.36.0 and the typed informer variants (`NewTypedEndpointSliceInformer` etc.) in v0.37.0 (https://pkg.go.dev/k8s.io/client-go/informers/discovery/v1).
- DNS: `net.LookupSRV(service, proto, name)` and `(*Resolver).LookupSRV(ctx, ...)` return SRV records (https://pkg.go.dev/net).
- The pure Go resolver queries the servers listed in `/etc/resolv.conf`, and a blocked query consumes only a goroutine. `GODEBUG=netdns=go|cgo` selects the resolver, and the `netgo` build tag disables the cgo resolver (https://pkg.go.dev/net).

## 12. OCI artifact distribution and signing for plugins

| Library | Latest | Date | License | Role |
|---|---|---|---|---|
| oras.land/oras-go/v2 | v2.6.2 | 2026-07-10 | Apache-2.0 | Push/pull/copy OCI artifacts |
| sigstore/sigstore-go | v1.3.0 | 2026-07-30 | Apache-2.0 | Sign/verify Sigstore bundles |
| sigstore/cosign | v3.1.3 (and v2.6.5) | 2026-08-06 | Apache-2.0 | CLI; library use de-emphasized |

Sources per row: oras-go (https://pkg.go.dev/oras.land/oras-go/v2?tab=versions) (https://github.com/oras-project/oras-go); sigstore-go (https://pkg.go.dev/github.com/sigstore/sigstore-go?tab=versions) (https://github.com/sigstore/sigstore-go); cosign (https://github.com/sigstore/cosign/releases) (https://github.com/sigstore/cosign).

- oras-go is compliant with the OCI Image Format and Distribution specifications (the README names no spec version) and works across registries, local file systems and in-memory stores; the main branch hosts v3 development that is not recommended for production (https://github.com/oras-project/oras-go). The v2 package docs show OCI layout, file and memory stores, registry auth, and a Referrers API client that cites Distribution Spec v1.1.1 (https://pkg.go.dev/oras.land/oras-go/v2) (https://pkg.go.dev/oras.land/oras-go/v2/registry/remote).
- GO-2026-5879 (CVE-2026-50162, GHSA-8xwf-rjm4-xvhv, published 2026-07-24) is a symlink traversal in the `content/file` store. It affects versions before v2.6.1 and is fixed in v2.6.1 (https://pkg.go.dev/vuln/GO-2026-5879). pkg.go.dev flags v2.6.0 and earlier with GO-2026-5879, GO-2026-5880, GO-2026-5882, GO-2026-5884 and GO-2026-5885, flags v2.6.1 with GO-2026-5880, and shows no flag on v2.6.2 (https://pkg.go.dev/oras.land/oras-go/v2?tab=versions).
- sigstore-go is "considered stable and ready for production use" and passes sigstore-conformance. It fetches the trusted root over TUF and also accepts custom trusted roots. It aims to keep the dependency tree small and needs Go 1.23+ (https://github.com/sigstore/sigstore-go).
- The Cosign README says future development targets a major release based on sigstore-go and points library contributions there (https://github.com/sigstore/cosign).
- Cosign supports keyless signing (Fulcio + OIDC), KMS (Vault, AWS, GCP, Azure), hardware tokens and encrypted ECDSA-P256 keys. Signatures can be stored in OCI registries, in Rekor and as protobuf bundles (https://github.com/sigstore/cosign).
- Cosign v3.1.3 and v2.6.5 (2026-08-06) fix verification bypass GHSA-fx35-mq7g-6g98; v3.1.2 (2026-07-17) fixed `artifactType` being omitted from OCI 1.1 signature referrer manifests, added `bundle inspect`, and deprecated `--payload` (https://github.com/sigstore/cosign/releases).

## Sources
https://github.com/wundergraph/graphql-go-tools
https://github.com/wundergraph/graphql-go-tools/releases
https://github.com/wundergraph/graphql-go-tools/tree/master/v2
https://raw.githubusercontent.com/wundergraph/graphql-go-tools/master/v2/go.mod
https://raw.githubusercontent.com/wundergraph/cosmo/main/router/go.mod
https://github.com/99designs/gqlgen
https://github.com/99designs/gqlgen/releases
https://github.com/movio/bramble
https://pkg.go.dev/github.com/movio/bramble?tab=versions
https://github.com/movio/bramble/releases/tag/v1.4.23
https://github.com/graphql-go/graphql
https://pkg.go.dev/github.com/graphql-go/graphql?tab=versions
https://github.com/twmb/franz-go
https://github.com/twmb/franz-go/releases
https://raw.githubusercontent.com/twmb/franz-go/master/CHANGELOG.md
https://github.com/twmb/franz-go/blob/master/docs/producing-and-consuming.md
https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo?tab=versions
https://github.com/IBM/sarama
https://github.com/IBM/sarama/releases
https://pkg.go.dev/github.com/IBM/sarama
https://github.com/segmentio/kafka-go
https://pkg.go.dev/github.com/segmentio/kafka-go?tab=versions
https://github.com/nats-io/nats.go
https://github.com/nats-io/nats.go/releases
https://github.com/nats-io/nats.go/blob/main/jetstream/README.md
https://pkg.go.dev/github.com/nats-io/nats.go?tab=versions
https://github.com/eclipse-paho/paho.golang
https://pkg.go.dev/github.com/eclipse/paho.golang?tab=versions
https://pkg.go.dev/github.com/eclipse/paho.golang/autopaho
https://github.com/eclipse-paho/paho.mqtt.golang
https://pkg.go.dev/github.com/eclipse/paho.mqtt.golang?tab=versions
https://github.com/mochi-mqtt/server
https://pkg.go.dev/github.com/mochi-mqtt/server/v2
https://pkg.go.dev/github.com/mochi-mqtt/server/v2?tab=versions
https://github.com/gorilla/websocket
https://github.com/gorilla/websocket/issues/370
https://pkg.go.dev/github.com/gorilla/websocket?tab=versions
https://pkg.go.dev/github.com/gorilla/websocket
https://pkg.go.dev/vuln/GO-2026-6278
https://github.com/coder/websocket
https://pkg.go.dev/github.com/coder/websocket?tab=versions
https://github.com/gobwas/ws
https://pkg.go.dev/github.com/gobwas/ws?tab=versions
https://pkg.go.dev/net/http#ResponseController
https://github.com/tmaxmax/go-sse
https://pkg.go.dev/github.com/tmaxmax/go-sse?tab=versions
https://github.com/r3labs/sse
https://pkg.go.dev/github.com/r3labs/sse/v2?tab=versions
https://github.com/tiktoken-go/tokenizer
https://pkg.go.dev/github.com/tiktoken-go/tokenizer
https://pkg.go.dev/github.com/tiktoken-go/tokenizer?tab=versions
https://github.com/pkoukk/tiktoken-go
https://pkg.go.dev/github.com/pkoukk/tiktoken-go?tab=versions
https://github.com/pkoukk/tiktoken-go-loader
https://redis.io/docs/latest/develop/data-types/vector-sets/
https://redis.io/docs/latest/commands/vadd/
https://redis.io/docs/latest/commands/vsim/
https://redis.io/docs/latest/operate/oss_and_stack/stack-with-enterprise/release-notes/redisce/redisos-8.4-release-notes/
https://redis.io/legal/licenses/
https://redis.io/docs/latest/develop/programmability/eval-intro/
https://github.com/valkey-io/valkey-search
https://github.com/valkey-io/valkey-search/releases
https://valkey.io/blog/valkey-search-1_2/
https://valkey.io/topics/search/
https://www.linuxfoundation.org/press/valkey-enhances-efficiency-security-and-modular-performance-with-9.1-release-and-new-ecosystem-integrations
https://github.com/go-redis/redis_rate
https://pkg.go.dev/github.com/go-redis/redis_rate/v10?tab=versions
https://raw.githubusercontent.com/go-redis/redis_rate/v10/lua.go
https://github.com/throttled/throttled
https://pkg.go.dev/github.com/throttled/throttled/v2?tab=versions
https://raw.githubusercontent.com/throttled/throttled/master/store/goredisstore/goredisstore.go
https://golang.testcontainers.org/modules/
https://golang.testcontainers.org/modules/kafka/
https://golang.testcontainers.org/modules/nats/
https://golang.testcontainers.org/modules/mosquitto/
https://github.com/testcontainers/testcontainers-go
https://github.com/testcontainers/testcontainers-go/releases
https://pkg.go.dev/github.com/testcontainers/testcontainers-go?tab=versions
https://github.com/bufbuild/buf
https://github.com/bufbuild/buf/releases
https://raw.githubusercontent.com/hashicorp/consul/main/api/LICENSE
https://pkg.go.dev/github.com/hashicorp/consul/api?tab=versions
https://developer.hashicorp.com/consul/api-docs/features/blocking
https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/
https://pkg.go.dev/k8s.io/client-go/informers/discovery/v1
https://pkg.go.dev/net
https://github.com/oras-project/oras-go
https://pkg.go.dev/oras.land/oras-go/v2?tab=versions
https://pkg.go.dev/oras.land/oras-go/v2
https://pkg.go.dev/oras.land/oras-go/v2/registry/remote
https://pkg.go.dev/vuln/GO-2026-5879
https://github.com/sigstore/sigstore-go
https://pkg.go.dev/github.com/sigstore/sigstore-go?tab=versions
https://github.com/sigstore/cosign
https://github.com/sigstore/cosign/releases

## Gaps
- **Date conflicts between GitHub release pages and pkg.go.dev.** For bramble, kafka-go, nats.go, testcontainers-go and graphql-go, summarised fetches of the GitHub release pages showed years one or two earlier than pkg.go.dev for the same tags. The raw `datetime` timestamps on those release pages match pkg.go.dev (for example, bramble v1.4.23 is 2026-09-14 and graphql-go v0.8.1 is 2023-04-10 on both), so the conflict came from the summaries, not the sources. This file uses the pkg.go.dev dates. The valkey-search release page timestamps 1.2.0 at 2026-03-17 and 1.2.1 at 2026-07-07.
- **Vector Sets status.** The redis.io vector-sets page, fetched on 2026-09-23, contains no "beta" wording. A search-result snippet describes Vector Sets as "currently available in beta, with APIs and behaviors that may change". GA versus beta status for Redis 8.x has not been confirmed from a dated primary statement.
- **Vector Sets default quantization.** A summarised fetch of the data-type page said the default was FP32. The VADD reference says Q8 (int8) is the default. This file follows the VADD reference.
- **gorilla/websocket.** GO-2026-6278 is marked "unreviewed". The advisory says versions before v1.5.3 are affected, and the pkg.go.dev versions tab flags v1.5.2 and earlier, which agrees. The current maintainer situation beyond issue #370 could not be confirmed, and no commit activity after v1.5.3 was checked.
- **kafka-go transactions.** Transaction protocol request types exist in the repo. Whether the high-level `Writer` supports transactional or idempotent production was not verified. No independent throughput benchmark with stated hardware was found for franz-go, sarama or kafka-go.
- **franz-go release dates.** CHANGELOG entries have no dates and GitHub Releases is empty. Dates come from pkg.go.dev tags only.
- **mochi-mqtt maintenance.** The last tag was v2.7.9 on 2025-03-01, with no release in the ~18 months before the snapshot. The README says new releases typically go out over the weekend, but no commit dates after that were checked. No independent MQTT v5 conformance report was found beyond the Paho interoperability claim.
- **paho.golang.** Still pre-1.0 (v0.23.0, 2025-09-06). The full list of URL schemes it supports (ws/wss/quic) is not documented on pkg.go.dev beyond `mqtt`, `tls` and `WebSocketCfg`.
- **Tokenizers.** No Go library found in this session provides official Anthropic, Gemini, Llama or Mistral tokenizers. CGO status of tiktoken-go/tokenizer is not stated, though it embeds vocabularies as Go maps. The pkoukk benchmarks state their hardware (Apple M1 and AMD Ryzen 9 5900HS) but were run by the author only.
- **Redis/Valkey module licenses and CGO.** valkey-search is a server-side module; its implementation language and build requirements were not checked. The Consul core (non-`api`) license was not fetched directly.
- **net.LookupSRV ordering.** The fetched doc summary did not include the text on priority sorting and weight randomisation, so RFC 2782 ordering behaviour is not confirmed here.
- **oras-go advisories.** Only GO-2026-5879 was read in detail. GO-2026-5880, GO-2026-5882, GO-2026-5884 and GO-2026-5885 were not checked individually.
- **testcontainers-go.** Default images and helper methods for the Redis, Valkey, Redpanda, Consul, Registry and K3s modules were not checked (marked "not checked" in the table).
- **CGO columns.** "Not stated" means the README does not mention CGO. It has not been confirmed with `go list -deps` or a `CGO_ENABLED=0` build.
