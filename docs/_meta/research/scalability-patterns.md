# Research: Scalability and operations patterns for API gateways

- Topic: distributed rate limiting, multi-region quotas, cells, zero-downtime upgrades, benchmark methodology, Go server tuning, capacity coefficients, control-plane HA
- Snapshot date: 2026-09-23
- Method: primary sources only (vendor docs, API references, READMEs, release notes, RFC/IETF drafts, vendor engineering blogs), fetched 2026-09-23. Every factual line carries its source URL. Lines marked **Derived** are arithmetic on sourced numbers, not vendor claims. Lines marked **Analysis** are this author's interpretation for Ruralz and are not facts.

## 1. Rate limiting algorithms: accuracy, state and latency

### 1.1 Algorithm comparison

| Algorithm | State per key | Accuracy at window edges | Used by (sourced) |
|---|---|---|---|
| Fixed window counter | 1 counter + TTL | **Derived:** a client can send up to twice the limit across a window boundary (the full limit at the end of one window and again at the start of the next) | No non-vendor primary source captured (see Gaps) |
| Sliding window log | 1 entry per request | Exact; state grows with the request rate (**Analysis**) | No non-vendor primary source captured (see Gaps) |
| Sliding window counter (weighted previous + current window) | 2 counters | Approximate; Cloudflare measured "0.003% of requests have been wrongly allowed or rate limited" over 400 million requests from 270,000 sources, 6% average variance between estimated and real rate (https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) | Cloudflare (2017-06-07) (https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) |
| Token bucket | tokens + last refill time | Smooth, with configurable burst | Stripe request rate limiter in Redis (https://stripe.com/blog/rate-limiters) |
| GCRA | 1 timestamp (TAT) | Leaky-bucket smoothing without a drip process (https://brandur.org/rate-limiting) | go-redis/redis_rate v10 ("implements GCRA (aka leaky bucket)", Lua script, Redis >= 3.2, BSD-2-Clause) (https://github.com/go-redis/redis_rate); Go library throttled, "taking production traffic at Stripe" per the 2015-09-18 post (https://brandur.org/rate-limiting) |

- GCRA tracks a Theoretical Arrival Time (TAT); the emission interval T is added per allowed request and the delay variation tolerance τ sets burst capacity; it stores one value per key and needs no background drip worker (https://brandur.org/rate-limiting).
- redis_rate relies on Redis `replicate_commands` for script replication (https://github.com/go-redis/redis_rate).
- Redis advises against client-side caching of keys that change continuously, citing "a global counter that is continuously INCRemented" (https://redis.io/docs/latest/develop/reference/client-side-caching/). **Analysis:** rate-limit counters and TAT keys should not rely on RESP3 client tracking; only over-limit decisions are worth caching locally.

### 1.2 Local, global and hybrid designs

| System | Local | Global | Hybrid / async sync | Failure default |
|---|---|---|---|---|
| Cloudflare WAF rate limiting | Per data center: "Cloudflare does not support global rate limiting counters across the entire network" (https://developers.cloudflare.com/waf/rate-limiting-rules/request-rate/) | None, by design | 2017 design: Twemproxy + memcache per PoP, asynchronous increments, mitigation flag cached locally once a source exceeds its threshold (https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) | n/a |
| Cloudflare Workers rate limit binding | Local to a location; "permissive, eventually consistent, and intentionally designed to not be used as an accurate accounting system"; `period` must be 10 or 60 s (https://developers.cloudflare.com/workers/runtime-apis/bindings/rate-limit/) | None | Counters cached on the machine and updated asynchronously (https://developers.cloudflare.com/workers/runtime-apis/bindings/rate-limit/) | n/a |
| Stripe (2017-03-30) | n/a | Token bucket in Redis, plus concurrent-request limiter, fleet usage load shedder (non-critical traffic over its 80% allocation gets 503) and worker utilization load shedder (https://stripe.com/blog/rate-limiters) | Dark-launch each limiter before enforcing (https://stripe.com/blog/rate-limiters) | Fail open if Redis is down (https://stripe.com/blog/rate-limiters) |

- **Analysis:** any asynchronous synchronization of counters permits a cluster-wide overage that grows with node count and sync interval; the Cloudflare and Workers rows above trade that overage for no per-request round trip.

### 1.3 Hot-key mitigation (sourced techniques)

| Technique | Source |
|---|---|
| Local cache of the mitigation decision after a threshold breach, so repeated denials skip the shared store | https://blog.cloudflare.com/counting-things-a-lot-of-different-things/ |
| Auto-pipelining of concurrent commands in the Go client (rueidis, Apache-2.0; claims ~14x go-redis throughput in a local MacBook Pro 16" M1 Pro 2021 benchmark) | https://github.com/redis/rueidis |

**Analysis:** a local token-bucket pre-filter ahead of the global limiter, and splitting one hot GCRA key into N sub-keys with budget/N each, are known patterns, but no non-vendor primary source was captured in this pass (see Gaps).

### 1.4 Fail-open vs fail-closed conventions

| System | Default | Source |
|---|---|---|
| Stripe | Fail open | https://stripe.com/blog/rate-limiters |
| Doorman (archived) | On server loss, leases expire and clients revert to a configured safe capacity (unlimited, zero, or fixed rate) | https://github.com/youtube/doorman |

Ruralz ADR-0008 already fixes fail-open by default, configurable per Policy (foundation pack); the sources above are consistent with that default.

### 1.5 IETF RateLimit header fields

- `draft-ietf-httpapi-ratelimit-headers-11`, published 2026-05-23, expires 2026-11-24, intended status Standards Track, HTTPAPI WG; not an RFC as of the snapshot (https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers).
- Defines `RateLimit-Policy` (items with `q` quota, `w` window in seconds, `qu` quota unit: requests / content-bytes / concurrent-requests, `pk` partition key) and `RateLimit` (`r` remaining, `t` seconds until reset, `pk`), as RFC 9651 Structured Fields lists (https://datatracker.ietf.org/doc/draft-ietf-httpapi-ratelimit-headers/, https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers).
- "If a response contains both the RateLimit and Retry-After fields, the Retry-After field MUST take precedence" (https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers).
- "Clients MUST NOT consider the available quota parameter as a service level agreement" (https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers).

## 2. Multi-region quotas and data residency

| Strategy | Sourced example | Consistency |
|---|---|---|
| Independent per-region budgets | Cloudflare counters are per data center (https://developers.cloudflare.com/waf/rate-limiting-rules/request-rate/) | Independent; effective global total is the sum of regions |
| Central server leases capacity to clients | Doorman: leases typically 5 minutes, refresh typically every 5 s, hierarchical servers with master election; Apache-2.0, alpha, archived 2024-11 (https://github.com/youtube/doorman) | Rebalanced on each refresh interval |
| CRDT counters with multi-primary replication | Redis Software Active-Active: CRDT-based, strong eventual consistency, vector clocks; string counters (`INCRBY`/`DECRBY`) merge additively across regions; 59-bit counters; at least two participating clusters; Lua scripts are not replicated (https://redis.io/docs/latest/operate/rs/databases/active-active/, https://redis.io/docs/latest/operate/rs/databases/active-active/develop/data-types/strings/) | Converges after sync; overshoot possible between syncs |

- Active-Active is documented as a Redis Software feature (https://redis.io/docs/latest/operate/rs/databases/active-active/). Non-counter strings replicate with last-write-wins on OS wall-clock time (https://redis.io/docs/latest/operate/rs/databases/active-active/develop/data-types/strings/). **Analysis:** a GCRA TAT written with `SET` would fall under last-write-wins, not counter merge, so GCRA does not map onto CRDT counters; per-region budgets are the fit for Ruralz's OSS `redis` State Store.
- Data residency precedent: Cloudflare Data Localization Suite (Enterprise add-on) comprises Regional Services (choose which data centers may decrypt and process HTTPS traffic), Customer Metadata Boundary (logs and analytics stay in region) and Geo Key Manager (private key storage location) (https://developers.cloudflare.com/data-localization/).

## 3. Cell-based architecture

- AWS whitepaper "Reducing the Scope of Impact with Cell-Based Architecture", published 2023-09-20, extends the Well-Architected bulkhead best practice to workload architecture (https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/reducing-scope-of-impact-with-cell-based-architecture.html).
- Cell sizing: cap maximum cell size and keep sizes consistent across AZs/Regions; smaller cells reduce scope of impact ("If with 10 cells, each one has 10% with their customers, with 100 each one has 1%") and are easier to test; larger cells are easier to operate and use capacity better (https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/cell-sizing.html).
- Each cell should have a known maximum in transactions per second, tenants, and GB/s or stored capacity (https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/cell-sizing.html).
- Cell router: shared across cells, so keep it "as simple and horizontally scalable as possible", map partition keys with a cryptographic hash plus modular arithmetic, minimize business logic, and keep other cells working when one is unreachable (https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/cell-routing.html).
- **Analysis:** this matches the foundation-pack Cell (Cluster + its State Store + optional regional Control): rate-limit and quota keys never cross Cells, so State Store hot keys and failures stay bounded per Cell.

## 4. Zero-downtime upgrade and reload patterns

### 4.1 Listener handover and drain

| Mechanism | How listeners survive | Existing connections | Key parameters / caveats |
|---|---|---|---|
| `SO_REUSEPORT` (Linux 3.9+) | New process binds the same address; kernel distributes TCP connections across listener sockets; all binders need the same effective UID (https://man7.org/linux/man-pages/man7/socket.7.html) | Old process drains | HAProxy measured "155 connection failures for one million connections after 180 reloads" (10 reloads/s at 55,000 conn/s) using SO_REUSEPORT alone, caused by closing sockets with queued connections (https://www.haproxy.com/blog/truly-seamless-reloads-with-haproxy-no-more-hacks) |
| Go `net/http` | n/a (application level) | `Server.Shutdown` waits for active requests but does not handle hijacked connections such as WebSockets; `RegisterOnShutdown` callbacks let the application notify them (https://pkg.go.dev/net/http#Server.Shutdown) | `SetKeepAlivesEnabled(false)` stops keep-alive reuse (https://pkg.go.dev/net/http#Server.Shutdown) |

- Ruralz ADR-0015 chose `SO_REUSEPORT` + drain + readiness gating and no socket passing in v1 (foundation pack). **Analysis:** the HAProxy measurement is the documented residual risk of that choice at high reload rates.

### 4.2 HTTP/2 GOAWAY and draining

- RFC 9113 two-phase shutdown: an initial GOAWAY with last stream ID 2^31-1 and NO_ERROR, then a final GOAWAY after at least one round-trip time; grpc-go times that round trip with a PING/ACK pair, falling back to a 5 s timer (https://www.rfc-editor.org/rfc/rfc9113.html, https://github.com/grpc/grpc-go/blob/master/internal/transport/http2_server.go).

### 4.3 Kubernetes termination

- `terminationGracePeriodSeconds` defaults to 30 s (https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/).
- A preStop hook "must complete its execution before the TERM signal can be sent", and the grace period covers hook plus shutdown (example: 60 s grace, 55 s hook, 10 s stop gets the container killed) (https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/).
- Hook handler types: exec, HTTP, sleep (https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/).
- EndpointSlice `serving` and `terminating` conditions are stable since v1.26; `terminating` is set when the Pod gets a deletion timestamp; service proxies may still route to entries that are both `serving` and `terminating` when all entries are terminating (https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/).
- **Analysis:** because EndpointSlice updates run concurrently with termination, the conventional sequence is preStop sleep (wait for EndpointSlice propagation) -> fail `/readyz` -> GOAWAY / `Connection: close` -> `Shutdown` with a deadline shorter than the grace period.

### 4.4 Long-lived WebSocket and SSE drain

| Signal | Meaning | Source |
|---|---|---|
| WebSocket close 1001 | Going Away (server going down) | https://www.iana.org/assignments/websocket/websocket.xhtml |
| WebSocket close 1012 | Service Restart | https://www.iana.org/assignments/websocket/websocket.xhtml |
| WebSocket close 1013 | Try Again Later | https://www.iana.org/assignments/websocket/websocket.xhtml |
| SSE `retry:` field | Sets the client's reconnection time | https://html.spec.whatwg.org/multipage/server-sent-events.html |
| SSE `Last-Event-ID` | Sent by the client on reconnect so the server can resume | https://html.spec.whatwg.org/multipage/server-sent-events.html |
| HTTP 204 on SSE reconnect | Tells the client to stop reconnecting | https://html.spec.whatwg.org/multipage/server-sent-events.html |

**Analysis:** hijacked WebSocket connections sit outside `Server.Shutdown` (https://pkg.go.dev/net/http#Server.Shutdown), so a gateway must track them itself and send close 1012 with jitter spread across the drain window, to avoid a reconnect storm onto the remaining Nodes.

## 5. Benchmark methodology

### 5.1 Open vs closed loop and coordinated omission

- In the closed model new iterations start only after the previous one finishes, so load drops when the system slows ("coordinated omission"); k6 open-model executors are `constant-arrival-rate` and `ramping-arrival-rate` (https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/).
- wrk2 (Gil Tene) measures latency from the intended send time at a constant rate `-R`, recorded in HdrHistogram; its README shows p99 10.52 ms corrected vs 5.43 ms uncorrected on a quiet run, and 1.27 s vs 6.04 ms with a 1.4 s stall (https://github.com/giltene/wrk2).
- HdrHistogram `recordValueWithExpectedInterval` back-fills synthetic samples when a recorded value exceeds the expected interval (https://github.com/HdrHistogram/HdrHistogram).

### 5.2 Tools

| Tool | Load model | Coordinated omission handling | Protocols | License / version | Source |
|---|---|---|---|---|---|
| wrk2 | Open (fixed `-R`) | Corrected; HdrHistogram | HTTP/1.1 | README calls it experimental | https://github.com/giltene/wrk2 |
| oha (Rust) | Closed by default; `-q` sets QPS | `--latency-correction` with `-q` | HTTP/1, HTTP/2, experimental HTTP/3 | MIT; v1.16.0 | https://github.com/hatoo/oha, https://github.com/hatoo/oha/releases |
| k6 | Both; arrival-rate executors are open | Open model avoids it; `preAllocatedVUs` | HTTP and others | not captured | https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/ |
| vegeta | Open (`-rate`, default 50/s; `-max-workers`) | README claims to "avoid nasty Coordinated Omission"; `hdrplot` report | HTTP | MIT | https://github.com/tsenart/vegeta |
| fortio | Fixed `-qps` with `-c`, `-uniform`, `-jitter` | Percentiles p50-p99.9, histogram | HTTP, gRPC | Apache-2.0; 1.75.2 | https://github.com/fortio/fortio |

## 6. Go server tuning

| Knob | Fact | Source |
|---|---|---|
| `GOGC` | Default 100; target heap = live heap + (live heap + GC roots) x GOGC/100; doubling GOGC roughly halves GC CPU cost | https://pkg.go.dev/runtime, https://go.dev/doc/gc-guide |
| `GOMEMLIMIT` | Soft limit since Go 1.19; GC CPU capped at roughly 50% over a 2 x GOMAXPROCS CPU-second window (worst case 2x slowdown); leave 5-10% headroom in containers; `GOGC=off` + limit only when the process owns the memory | https://go.dev/doc/gc-guide |
| GOMAXPROCS | Go 1.25 (2025-08) respects cgroup CPU bandwidth limits and updates periodically; opt out via `GODEBUG=containermaxprocs=0` / `updatemaxprocs=0` | https://go.dev/doc/go1.25 |
| Green Tea GC | Experimental in 1.25 (`GOEXPERIMENT=greenteagc`); default in Go 1.26 (2026-02) with expected 10-40% GC overhead reduction and ~10% more on Ice Lake / Zen 4+; opt-out `nogreenteagc` expected to be removed in 1.27 | https://go.dev/doc/go1.25, https://go.dev/doc/go1.26 |
| PGO | `default.pgo` in the main package; `-pgo=auto` default since Go 1.21; 2-14% improvement across representative programs as of Go 1.22; drives inlining from CPU pprof profiles | https://go.dev/doc/pgo |
| Upstream pooling | `DefaultMaxIdleConnsPerHost` = 2; `DefaultTransport` MaxIdleConns 100, IdleConnTimeout 90 s; create Transports once and reuse | https://pkg.go.dev/net/http#Transport |
| HTTP/2 config | Go 1.24 added `Server.HTTP2` / `Transport.HTTP2` (`HTTP2Config`) and `Protocols` including `UnencryptedHTTP2` (prior knowledge; `Upgrade: h2c` not supported) | https://go.dev/doc/go1.24 |
| HTTP/2 flow control | `MaxConcurrentStreams` defaults to at least 100; `MaxReceiveBufferPerConnection` valid from 64 KiB to under 4 MiB; `MaxReceiveBufferPerStream` under 4 MiB; `MaxReadFrameSize` 16 KiB-16 MiB | https://raw.githubusercontent.com/golang/go/master/src/net/http/http.go |
| HTTP/2 stream limits | Go 1.26 adds `HTTP2Config.StrictMaxConcurrentRequests` (whether to open a new connection when an existing one hits its stream limit) | https://go.dev/doc/go1.26 |
| x/net/http2 | `PingTimeout` defaults to 15 s; `ReadIdleTimeout` 0 = no health check; `MaxHandlers` "has never had any effect" | https://pkg.go.dev/golang.org/x/net/http2#Server |

## 7. Capacity coefficients

| Coefficient | Value | Context | Source |
|---|---|---|---|
| Valkey 8.0 SET throughput | >1.19M SET/s; 780K before the prefetch optimization; RC1 post (r7g): "up to 1.2 million" vs "previous limit of 380K" | c7g.4xlarge (16 vCPU, arm64), 8 I/O threads + main thread, 3M keys, 512 B values, 650 clients, `valkey-benchmark`; posts 2024-08-02 and 2024-09-13 | https://valkey.io/blog/unlock-one-million-rps-part2/, https://valkey.io/blog/valkey-8-0-0-rc1/ |
| Valkey ops per vCPU | **Derived** ~74K SET/s per vCPU (1.19M / 16) or ~132K per active thread (1.19M / 9) | same | https://valkey.io/blog/unlock-one-million-rps-part2/ |
| Idle WebSocket memory, idiomatic Go | **Derived** ~24 KB per connection from 3M connections using ~72 GB: 24 GB goroutine stacks (2 goroutines at 2-8 KB each), 12 GB 4 KB bufio.Reader, 12 GB 4 KB bufio.Writer, 24 GB net/http upgrade buffers | Mail.Ru (Sergey Kamardin), 2017-08-02; reduced via netpoll (epoll), on-demand writer goroutines and zero-copy upgrade (gobwas/ws) | https://www.freecodecamp.org/news/million-websockets-and-go-cc58418460bb/ |
| gorilla/websocket buffers | Default 4096 B; "Buffers are held for the lifetime of the connection by default"; with `WriteBufferPool` a connection holds the write buffer only while writing | gorilla/websocket | https://pkg.go.dev/github.com/gorilla/websocket |

## 8. Control-plane HA

### 8.1 Raft sizing and timing

| Servers | Quorum | Failures tolerated | Source |
|---|---|---|---|
| 1 | 1 | 0 | https://developer.hashicorp.com/consul/docs/architecture/consensus |
| 3 | 2 | 1 | https://etcd.io/docs/v3.6/faq/ |
| 5 | 3 | 2 | https://etcd.io/docs/v3.6/faq/ |
| 7 | 4 | 3 | https://etcd.io/docs/v3.6/faq/ |

- etcd says a cluster "probably should have no more than seven nodes", cites Google Chubby's suggestion of five (a 5-member cluster tolerates two failures, "enough in most cases"), and prefers odd sizes (https://etcd.io/docs/v3.6/faq/); Consul recommends 3 or 5 servers (https://developer.hashicorp.com/consul/docs/architecture/consensus).
- etcd defaults: heartbeat 100 ms, election timeout 1000 ms; heartbeat ~0.5-1.5x RTT, election timeout at least 10x RTT, maximum 50,000 ms (https://etcd.io/docs/v3.6/tuning/).
- `hashicorp/raft` `DefaultConfig()`: HeartbeatTimeout 1000 ms, ElectionTimeout 1000 ms, LeaderLeaseTimeout 500 ms, CommitTimeout 50 ms, MaxAppendEntries 64, SnapshotInterval 120 s, SnapshotThreshold 8192, TrailingLogs 10240 (https://raw.githubusercontent.com/hashicorp/raft/main/config.go). pkg.go.dev lists v1.8.0 published 2026-09-16, MPL-2.0 (https://pkg.go.dev/github.com/hashicorp/raft#DefaultConfig).
- Consul `raft_multiplier` default is equivalent to 5 (lower-performance timing); 1 is the highest-performance mode and shortens leader-failure detection and elections (https://developer.hashicorp.com/consul/docs/reference/agent/configuration-file/general).
- **Derived:** with hashicorp/raft defaults a follower waits at least HeartbeatTimeout (1 s) before becoming a candidate, so unplanned leader failover is on the order of 1-2 s plus election round trips; Consul's multiplier of 5 scales the timeouts about 5x. No source publishes a measured failover time (see Gaps).

### 8.2 Config push fan-out

- **Analysis:** the Ruralz Control Stream (ADR-0007: snapshot+delta, ACK/NACK) follows the usual lessons for config distribution protocols: deltas cap fan-out bandwidth compared with resending full state, nonces echoed in ACK/NACK prevent stale acknowledgements, and a single ordered stream per Node allows make-before-break ordering (add new resources before removing stale ones, to avoid blackholing traffic). No non-vendor primary source for these lessons was captured in this pass (see Gaps).

## Sources

https://blog.cloudflare.com/counting-things-a-lot-of-different-things/
https://developers.cloudflare.com/waf/rate-limiting-rules/request-rate/
https://developers.cloudflare.com/workers/runtime-apis/bindings/rate-limit/
https://developers.cloudflare.com/data-localization/
https://stripe.com/blog/rate-limiters
https://brandur.org/rate-limiting
https://github.com/go-redis/redis_rate
https://github.com/redis/rueidis
https://redis.io/docs/latest/develop/reference/client-side-caching/
https://redis.io/docs/latest/operate/rs/databases/active-active/
https://redis.io/docs/latest/operate/rs/databases/active-active/develop/data-types/strings/
https://datatracker.ietf.org/doc/draft-ietf-httpapi-ratelimit-headers/
https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers
https://github.com/youtube/doorman
https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/reducing-scope-of-impact-with-cell-based-architecture.html
https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/cell-sizing.html
https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/cell-routing.html
https://man7.org/linux/man-pages/man7/socket.7.html
https://www.haproxy.com/blog/truly-seamless-reloads-with-haproxy-no-more-hacks
https://www.rfc-editor.org/rfc/rfc9113.html
https://github.com/grpc/grpc-go/blob/master/internal/transport/http2_server.go
https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/
https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/
https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/
https://pkg.go.dev/net/http#Server.Shutdown
https://www.iana.org/assignments/websocket/websocket.xhtml
https://html.spec.whatwg.org/multipage/server-sent-events.html
https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/
https://github.com/giltene/wrk2
https://github.com/HdrHistogram/HdrHistogram
https://github.com/hatoo/oha
https://github.com/hatoo/oha/releases
https://github.com/tsenart/vegeta
https://github.com/fortio/fortio
https://pkg.go.dev/runtime
https://go.dev/doc/gc-guide
https://go.dev/doc/go1.24
https://go.dev/doc/go1.25
https://go.dev/doc/go1.26
https://go.dev/doc/pgo
https://pkg.go.dev/net/http#Transport
https://raw.githubusercontent.com/golang/go/master/src/net/http/http.go
https://pkg.go.dev/golang.org/x/net/http2#Server
https://valkey.io/blog/valkey-8-0-0-rc1/
https://valkey.io/blog/unlock-one-million-rps-part2/
https://www.freecodecamp.org/news/million-websockets-and-go-cc58418460bb/
https://pkg.go.dev/github.com/gorilla/websocket
https://etcd.io/docs/v3.6/faq/
https://etcd.io/docs/v3.6/tuning/
https://developer.hashicorp.com/consul/docs/architecture/consensus
https://developer.hashicorp.com/consul/docs/reference/agent/configuration-file/general
https://raw.githubusercontent.com/hashicorp/raft/main/config.go
https://pkg.go.dev/github.com/hashicorp/raft#DefaultConfig

## Gaps

- No non-vendor primary source was captured for fixed-window or sliding-log rate limiting; the Algorithm table describes them from first principles.
- No primary source was found for a CPU-per-10k-rps coefficient of a current Go reverse proxy; Section 7 therefore has no proxy CPU coefficient.
- The Valkey 8 1.19M rps result is SET-only via `valkey-benchmark`; no Lua/EVAL (GCRA-style script) throughput per core was found for Redis 8 or Valkey 8/9.
- The Mail.Ru post-optimization per-connection memory figure was not captured precisely; only the pre-optimization ~24 KB/connection breakdown is sourced.
- No primary source gives a measured Raft leader failover time for hashicorp/raft, etcd or Consul; the 1-2 s figure is derived from default timeouts.
- No non-vendor primary source was found for hot-key splitting (sharding one rate-limit key into sub-keys), a local pre-filter ahead of a global limiter, or config-push fan-out limits (connections per control-plane instance).
- RFC 9113 section 6.8 text could not be retrieved verbatim (page truncation); the two-phase GOAWAY summary comes from search-result excerpts of the RFC plus the grpc-go server source.
- The Kubernetes pod-termination flow section was truncated in fetch; preStop and grace-period facts come from the lifecycle-hooks page. The Kubernetes version in which the preStop `sleep` handler became GA was not verified.
- The oha releases page dates v1.16.0 to 2026-08-23 (https://github.com/hatoo/oha/releases). The k6 latest version was not captured.
- The AWS Builders' Library shuffle-sharding article redirected to builder.aws.com and returned no body, so shuffle sharding is not covered.
