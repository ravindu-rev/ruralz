# Research: Scalability and operations patterns for API gateways

- Topic: distributed rate limiting, multi-region quotas, cells, zero-downtime upgrades, benchmark methodology, Go server tuning, capacity coefficients, control-plane HA
- Snapshot date: 2026-09-23
- Method: primary sources only (vendor docs, API references, READMEs, release notes, RFC/IETF drafts, vendor engineering blogs), fetched 2026-09-23. Every factual line carries its source URL. Lines marked **Derived** are arithmetic on sourced numbers, not vendor claims. Lines marked **Analysis** are this author's interpretation for Ruralz and are not facts.

## 1. Rate limiting algorithms: accuracy, state and latency

### 1.1 Algorithm comparison

| Algorithm | State per key | Accuracy at window edges | Used by (sourced) |
|---|---|---|---|
| Fixed window counter | 1 counter + TTL | Allows a burst across a window boundary; once hit, "requests will be blocked for the remainder of the configured window duration" (https://tyk.io/docs/api-management/rate-limit/) | Kong `rate-limiting` (fixed windows, second..year) (https://developer.konghq.com/gateway/rate-limiting/, https://developer.konghq.com/plugins/rate-limiting/); Tyk fixed window limiter (https://tyk.io/docs/api-management/rate-limit/); envoyproxy/ratelimit limits per unit of second, minute, hour, day (https://github.com/envoyproxy/ratelimit) |
| Sliding window log | 1 entry per request | Exact; every request "including blocked requests that return HTTP 429, is written to the sliding log", producing "spike arrest" behavior (https://tyk.io/docs/api-management/rate-limit/) | Tyk Redis Rate Limiter (`enable_redis_rolling_limiter`) (https://tyk.io/docs/api-management/rate-limit/) |
| Sliding window counter (weighted previous + current window) | 2 counters | Approximate; Cloudflare measured "0.003% of requests have been wrongly allowed or rate limited" over 400 million requests from 270,000 sources, 6% average variance between estimated and real rate (https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) | Cloudflare (2017-06-07) (https://blog.cloudflare.com/counting-things-a-lot-of-different-things/); Kong Rate Limiting Advanced `window_type: sliding` (default) (https://developer.konghq.com/plugins/rate-limiting-advanced/reference/) |
| Token bucket | tokens + last refill time | Smooth, with configurable burst | Stripe request rate limiter in Redis (https://stripe.com/blog/rate-limiters); Envoy local rate limit (`max_tokens`, `tokens_per_fill`, `fill_interval`) (https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/local_rate_limit_filter); AWS API Gateway throttling (https://docs.aws.amazon.com/apigateway/latest/developerguide/api-gateway-request-throttling.html) |
| GCRA | 1 timestamp (TAT) | Leaky-bucket smoothing without a drip process (https://brandur.org/rate-limiting) | go-redis/redis_rate v10 ("implements GCRA (aka leaky bucket)", Lua script, Redis >= 3.2, BSD-2-Clause) (https://github.com/go-redis/redis_rate); Go library throttled, "taking production traffic at Stripe" per the 2015-09-18 post (https://brandur.org/rate-limiting) |

- GCRA tracks a Theoretical Arrival Time (TAT); the emission interval T is added per allowed request and the delay variation tolerance τ sets burst capacity; it stores one value per key and needs no background drip worker (https://brandur.org/rate-limiting).
- redis_rate relies on Redis `replicate_commands` for script replication (https://github.com/go-redis/redis_rate).
- Redis advises against client-side caching of keys that change continuously, citing "a global counter that is continuously INCRemented" (https://redis.io/docs/latest/develop/reference/client-side-caching/). **Analysis:** rate-limit counters and TAT keys should not rely on RESP3 client tracking; only over-limit decisions are worth caching locally.

### 1.2 Local, global and hybrid designs

| System | Local | Global | Hybrid / async sync | Failure default |
|---|---|---|---|---|
| Envoy | Local rate limit filter: token bucket shared across workers per process by default; `local_rate_limit_per_downstream_connection: true` makes it per connection (https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/local_rate_limit_filter) | HTTP ratelimit filter calls an external rate limit service per request; RPC `timeout` defaults to 20 ms (https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/http/ratelimit/v3/rate_limit.proto) | "Local rate limiting can be used in conjunction with global rate limiting to reduce load on the global rate limit service" (https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting); RLQS quota assignments with periodic usage reports (https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_quota_filter) | `failure_mode_deny: true` blocks traffic on communication failure; stat `failure_mode_allowed` counts requests let through when it is false (https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/http/ratelimit/v3/rate_limit.proto, https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_filter) |
| envoyproxy/ratelimit | freecache local cache of over-limit keys (`LocalCacheSizeInBytes`, default 0 = off) (https://github.com/envoyproxy/ratelimit) | Redis (`REDIS_TYPE` single/sentinel/cluster; `REDIS_POOL_SIZE` default 10; `REDIS_TIMEOUT` default 10s) or memcache (https://github.com/envoyproxy/ratelimit) | `REDIS_PIPELINE_WINDOW` 150-500 µs batches writes (default 0 = off); memcache increments are asynchronous and can briefly overshoot (https://github.com/envoyproxy/ratelimit) | Not documented in README (see Gaps) |
| Kong `rate-limiting` (OSS) | `policy: local` is the default (https://developer.konghq.com/plugins/rate-limiting/reference/) | `cluster` (Kong datastore; not supported in hybrid mode or Konnect) or `redis` (https://developer.konghq.com/plugins/rate-limiting/) | `sync_rate` default -1 (https://developer.konghq.com/plugins/rate-limiting/reference/) | `fault_tolerant` default `true`: proxy even when the data store is unreachable (https://developer.konghq.com/plugins/rate-limiting/reference/) |
| Kong Rate Limiting Advanced (Enterprise tier) | `strategy: local` default (https://developer.konghq.com/plugins/rate-limiting-advanced/reference/) | `redis`, `cluster` (https://developer.konghq.com/plugins/rate-limiting-advanced/reference/) | `sync_rate: 0` synchronous; `>0` interval in seconds (0.5 = every 500 ms); `-1` memory only (https://developer.konghq.com/plugins/rate-limiting-advanced/, https://developer.konghq.com/plugins/rate-limiting-advanced/reference/) | Not stated on reference page |
| Tyk | Distributed Rate Limiter (DRL, default) divides the limit equally across gateways; "unreliable at low rate limits where requests are not fairly balanced" (https://tyk.io/docs/api-management/rate-limit/) | Redis Rate Limiter (sliding log), Sentinel variant, fixed window (https://tyk.io/docs/api-management/rate-limit/) | `drl_threshold` switches to the Redis limiter when traffic is too low for DRL (https://tyk.io/docs/api-management/rate-limit/) | Not stated |
| Cloudflare WAF rate limiting | Per data center: "Cloudflare does not support global rate limiting counters across the entire network" (https://developers.cloudflare.com/waf/rate-limiting-rules/request-rate/) | None, by design | 2017 design: Twemproxy + memcache per PoP, asynchronous increments, mitigation flag cached locally once a source exceeds its threshold (https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) | n/a |
| Cloudflare Workers rate limit binding | Local to a location; "permissive, eventually consistent, and intentionally designed to not be used as an accurate accounting system"; `period` must be 10 or 60 s (https://developers.cloudflare.com/workers/runtime-apis/bindings/rate-limit/) | None | Counters cached on the machine and updated asynchronously (https://developers.cloudflare.com/workers/runtime-apis/bindings/rate-limit/) | n/a |
| Stripe (2017-03-30) | n/a | Token bucket in Redis, plus concurrent-request limiter, fleet usage load shedder (non-critical traffic over its 80% allocation gets 503) and worker utilization load shedder (https://stripe.com/blog/rate-limiters) | Dark-launch each limiter before enforcing (https://stripe.com/blog/rate-limiters) | Fail open if Redis is down (https://stripe.com/blog/rate-limiters) |
| AWS API Gateway | n/a | Token bucket per account per Region; throttles and quotas are "applied on a best-effort basis and should be thought of as targets rather than guaranteed request ceilings" (https://docs.aws.amazon.com/apigateway/latest/developerguide/api-gateway-request-throttling.html) | n/a | n/a |

- Kong documents that asynchronous `sync_rate` permits a worst-case cluster-wide overage that grows with node count and sync interval (https://developer.konghq.com/plugins/rate-limiting-advanced/).
- Envoy RLQS: `no_assignment_behavior` applies before the first assignment arrives; `expired_assignment_behavior` applies when an assignment TTL lapses; connection failure falls back to one of these (https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_quota_filter). Envoy states RLQS currently integrates with Google Cloud Rate Limit Service (https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting).
- envoyproxy/ratelimit supports per-rule or global shadow mode (`SHADOW_MODE`), which always returns OK while recording breach stats; `NEAR_LIMIT_RATIO` defaults to 0.8 (https://github.com/envoyproxy/ratelimit).

### 1.3 Hot-key mitigation (sourced techniques)

| Technique | Source |
|---|---|
| Local pre-filter token bucket ahead of the global limiter | https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting |
| Local cache of over-limit keys, so repeated denials skip Redis | https://github.com/envoyproxy/ratelimit |
| Cache the mitigation decision on the edge server after a threshold breach | https://blog.cloudflare.com/counting-things-a-lot-of-different-things/ |
| Batch/pipeline writes (150-500 µs window), trading latency for fewer syscalls | https://github.com/envoyproxy/ratelimit |
| Auto-pipelining of concurrent commands in the Go client (rueidis, Apache-2.0; claims ~14x go-redis throughput in a local MacBook Pro 16" M1 Pro 2021 benchmark) | https://github.com/redis/rueidis |
| Divide the budget across gateway instances locally (Tyk DRL), falling back to a shared counter at low rates | https://tyk.io/docs/api-management/rate-limit/ |

**Analysis:** splitting one hot GCRA key into N sub-keys with budget/N each is a known pattern, but no primary source was found in this pass (see Gaps).

### 1.4 Fail-open vs fail-closed conventions

| Product | Default | Source |
|---|---|---|
| Stripe | Fail open | https://stripe.com/blog/rate-limiters |
| Kong `rate-limiting` | `fault_tolerant: true` (fail open) | https://developer.konghq.com/plugins/rate-limiting/reference/ |
| Envoy HTTP ratelimit filter | Allows traffic unless `failure_mode_deny: true` (inferred from the `failure_mode_allowed` stat; default value not printed on the page) | https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_filter |
| Doorman (archived) | On server loss, leases expire and clients revert to a configured safe capacity (unlimited, zero, or fixed rate) | https://github.com/youtube/doorman |

Ruralz ADR-0008 already fixes fail-open by default, configurable per Policy (foundation pack); the sources above are consistent with that default.

### 1.5 IETF RateLimit header fields

- `draft-ietf-httpapi-ratelimit-headers-11`, published 2026-05-23, expires 2026-11-24, intended status Standards Track, HTTPAPI WG; not an RFC as of the snapshot (https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers).
- Defines `RateLimit-Policy` (items with `q` quota, `w` window in seconds, `qu` quota unit: requests / content-bytes / concurrent-requests, `pk` partition key) and `RateLimit` (`r` remaining, `t` seconds until reset, `pk`), as RFC 9651 Structured Fields lists (https://datatracker.ietf.org/doc/draft-ietf-httpapi-ratelimit-headers/, https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers).
- "If a response contains both the RateLimit and Retry-After fields, the Retry-After field MUST take precedence" (https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers).
- "Clients MUST NOT consider the available quota parameter as a service level agreement" (https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers).
- Envoy's `enable_x_ratelimit_headers: DRAFT_VERSION_03` still emits the older `X-RateLimit-Limit` / `-Remaining` / `-Reset` triple from draft 03 (https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/http/ratelimit/v3/rate_limit.proto).

## 2. Multi-region quotas and data residency

| Strategy | Sourced example | Consistency |
|---|---|---|
| Independent per-region budgets | AWS API Gateway account limits apply "per Region" (https://docs.aws.amazon.com/apigateway/latest/developerguide/api-gateway-request-throttling.html); Cloudflare counters are per data center (https://developers.cloudflare.com/waf/rate-limiting-rules/request-rate/) | Independent; effective global total is the sum of regions |
| Central server leases capacity to clients | Doorman: leases typically 5 minutes, refresh typically every 5 s, hierarchical servers with master election; Apache-2.0, alpha, archived 2024-11 (https://github.com/youtube/doorman); Envoy RLQS assignments + periodic reports (https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_quota_filter) | Rebalanced on each report/refresh interval |
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

### 4.1 Comparison

| Mechanism | How listeners survive | Existing connections | Key parameters / caveats |
|---|---|---|---|
| `SO_REUSEPORT` (Linux 3.9+) | New process binds the same address; kernel distributes TCP connections across listener sockets; all binders need the same effective UID (https://man7.org/linux/man-pages/man7/socket.7.html) | Old process drains | HAProxy measured "155 connection failures for one million connections after 180 reloads" (10 reloads/s at 55,000 conn/s) using SO_REUSEPORT alone, caused by closing sockets with queued connections (https://www.haproxy.com/blog/truly-seamless-reloads-with-haproxy-no-more-hacks) |
| Envoy hot restart | New process fetches listen sockets from the old one over a Unix domain socket; counters and most gauges are transferred to the new process; not supported on Windows (https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/hot_restart) | Not transferred; drained | `--drain-time-s` default 600 s, `--parent-shutdown-time-s` default 900 s, `--drain-strategy` gradual (default) or immediate (https://www.envoyproxy.io/docs/envoy/latest/operations/cli); reuse_port sockets are handed over by worker index, so lowering concurrency can drop queued connections (https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/hot_restart) |
| NGINX binary upgrade | `USR2` to master renames PID file to `.oldbin` and starts a new master sharing listeners; `WINCH` makes old workers exit gracefully; `HUP` to old master rolls back; `QUIT` to old master finalizes (https://nginx.org/en/docs/control.html) | Old workers finish serving | `HUP` config reload validates syntax first and keeps the old config on failure (https://nginx.org/en/docs/control.html) |
| HAProxy seamless reload (1.8+) | Listening FDs passed via `SCM_RIGHTS`; `expose-fd listeners` on the stats socket plus `-x` (https://www.haproxy.com/blog/truly-seamless-reloads-with-haproxy-no-more-hacks); master-worker mode does this automatically over `sockpair@` and re-execs on `SIGUSR2` with `-sf` (https://docs.haproxy.org/3.2/management.html) | Old workers stop gracefully on `SIGUSR1` (https://docs.haproxy.org/3.2/management.html) | — |
| Go `net/http` | n/a (application level) | `Server.Shutdown` waits for active requests but does not handle hijacked connections such as WebSockets; `RegisterOnShutdown` callbacks let the application notify them (https://pkg.go.dev/net/http#Server.Shutdown) | `SetKeepAlivesEnabled(false)` stops keep-alive reuse (https://pkg.go.dev/net/http#Server.Shutdown) |

- Ruralz ADR-0015 chose `SO_REUSEPORT` + drain + readiness gating and no socket passing in v1 (foundation pack). **Analysis:** the HAProxy measurement is the documented residual risk of that choice at high reload rates; Envoy's worker-index note shows the same class of issue.

### 4.2 HTTP/2 GOAWAY and draining

- Envoy HTTP connection manager `drain_timeout` (default 5000 ms) is the wait between the HTTP/2 "shutdown notification" (GOAWAY with max stream ID) and the final GOAWAY; `delayed_close_timeout` default 1000 ms; `stream_idle_timeout` default 5 minutes (https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/network/http_connection_manager/v3/http_connection_manager.proto).
- On drain Envoy adds `Connection: close` for HTTP/1 and sends GOAWAY for HTTP/2; drains are triggered by hot restart, `/drain_listeners?graceful`, health-check fail, or LDS changes (https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/draining).
- RFC 9113 two-phase shutdown: an initial GOAWAY with last stream ID 2^31-1 and NO_ERROR, then a final GOAWAY after at least one round-trip time; grpc-go times that round trip with a PING/ACK pair, falling back to a 5 s timer (https://www.rfc-editor.org/rfc/rfc9113.html, https://github.com/grpc/grpc-go/blob/master/internal/transport/http2_server.go).

### 4.3 Kubernetes termination

- `terminationGracePeriodSeconds` defaults to 30 s (https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/).
- A preStop hook "must complete its execution before the TERM signal can be sent", and the grace period covers hook plus shutdown (example: 60 s grace, 55 s hook, 10 s stop gets the container killed) (https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/).
- Hook handler types: exec, HTTP, sleep (https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/).
- EndpointSlice `serving` and `terminating` conditions are stable since v1.26; `terminating` is set when the Pod gets a deletion timestamp; service proxies may still route to entries that are both `serving` and `terminating` when all entries are terminating (https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/). <!-- alias-ok -->
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
- Envoy's benchmarking FAQ: use open-loop generators, match `--concurrency` to the cores of the compared system, disable circuit breaking, `generate_request_id` and `dynamic_stats`, align TLS and HTTP/2 settings, and never measure latency at maximum load; Nighthawk is suggested (https://www.envoyproxy.io/docs/envoy/latest/faq/performance/how_to_benchmark_envoy).

### 5.2 Tools

| Tool | Load model | Coordinated omission handling | Protocols | License / version | Source |
|---|---|---|---|---|---|
| wrk2 | Open (fixed `-R`) | Corrected; HdrHistogram | HTTP/1.1 | README calls it experimental | https://github.com/giltene/wrk2 |
| oha (Rust) | Closed by default; `-q` sets QPS | `--latency-correction` with `-q` | HTTP/1, HTTP/2, experimental HTTP/3 | MIT; v1.16.0 | https://github.com/hatoo/oha, https://github.com/hatoo/oha/releases |
| k6 | Both; arrival-rate executors are open | Open model avoids it; `preAllocatedVUs` | HTTP and others | not captured | https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/ |
| vegeta | Open (`-rate`, default 50/s; `-max-workers`) | README claims to "avoid nasty Coordinated Omission"; `hdrplot` report | HTTP | MIT | https://github.com/tsenart/vegeta |
| fortio (ex-Istio) | Fixed `-qps` with `-c`, `-uniform`, `-jitter` | Percentiles p50-p99.9, histogram | HTTP, gRPC | Apache-2.0; 1.75.2 | https://github.com/fortio/fortio |
| Nighthawk | Closed by default; `--open-loop` removes backpressure | Percentile histograms | HTTP/1.1, HTTP/2, HTTP/3 | not captured | https://github.com/envoyproxy/nighthawk |

### 5.3 Published gateway benchmarks and comparability

| Product | Published figure | Hardware | Tool / model | Date | Source |
|---|---|---|---|---|---|
| KrakenD | 10,126 req/s (c4.2xlarge, 8 vCPU, 15 GB); 8,465 (c4.xlarge); 2,758 (t2.micro); 18,157 req/s on MacBook Pro i7 2.2 GHz | AWS c4/m4/t2; LWAN fake API on c4.xlarge | `hey` (closed loop) | 2016-10-28 | https://www.krakend.io/docs/benchmarks/, https://www.krakend.io/docs/benchmarks/aws/ |
| KrakenD (homepage) | "80K+ requests/second on commodity hardware" | Not specified | Not specified | current | https://www.krakend.io/ |
| Kong Gateway 3.16 | 144,273.5 rps, p95 2.89 ms, p99 4.5 ms (no plugins, 1 route); 120,012 rps (rate limit, 1 route); 97,290.2 rps, p99 8.65 ms (rate limit + key-auth, 100 routes / 100 consumers) | c5.4xlarge (16 vCPU), 16 workers; k6 and observability on c5.metal | k6, 5 runs x 15 min; load model not stated | versions 3.6-3.16 listed | https://developer.konghq.com/gateway/performance/benchmarks/, https://github.com/Kong/kong-gateway-performance-benchmark |
| Envoy | "we do not currently publish any official benchmarks" | — | — | — | https://www.envoyproxy.io/docs/envoy/latest/faq/performance/how_fast_is_envoy |
| Envoy (Istio 1.24 sidecar) | ~0.20 vCPU and 60 MB per 1,000 rps, 1 KB payload, 2 worker threads, mTLS on | 5x M3 Large bare metal (CNCF lab) | Istio harness, 500-1,500 rps, 4 client connections | Istio 1.24 | https://istio.io/latest/docs/ops/deployment/performance-and-scalability/ |
| APISIX | Numbers published only as images; setup: GCP n1-highcpu-8, 4 cores for APISIX, 1 KB response, with and without limit-count + prometheus | GCP n1-highcpu-8 (8 vCPU, 7.2 GB) | wrk (closed loop) | undated | https://apisix.apache.org/docs/apisix/benchmark/ |
| Tyk | Claims to beat Kong on RPS and P99 with auth/rate limiting on AWS, GCP, Azure (4 machine classes each); claims Kong "does not appear to scale as effectively past 8 cores" | c5.2xlarge example in harness | Ansible harness, results written locally | undated | https://tyk.io/performance-benchmarks/, https://github.com/TykTechnologies/tyk-ansible-performance-testing |
| NGINX (web server, not gateway) | ~145K rps (0 KB) / ~74K rps (1 KB) on 1 CPU; ~2M / ~972K on 16 CPUs; HTTPS roughly one-quarter lower at 16 CPUs | 2x Xeon E5-2699 v3 (36 cores), 2x 40 GbE XL710, 16 GB | wrk 4.0.0 | 2017-08-24 | https://blog.nginx.org/blog/testing-the-performance-of-nginx-and-nginx-plus-web-servers |

Comparability: no two rows share hardware, payload, plugin set, TLS setting or load model. KrakenD figures are 2016 closed-loop `hey` runs on retired EC2 generations (https://www.krakend.io/docs/benchmarks/aws/); Kong does not state whether k6 uses constant VUs or arrival rate (https://github.com/Kong/kong-gateway-performance-benchmark); APISIX uses wrk (https://apisix.apache.org/docs/apisix/benchmark/); Envoy publishes none (https://www.envoyproxy.io/docs/envoy/latest/faq/performance/how_fast_is_envoy). Of these, only Kong publishes p95/p99 together with instance type, worker count and run duration (https://developer.konghq.com/gateway/performance/benchmarks/).

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
| Envoy CPU per 10k rps | **Derived** 2.0 vCPU per 10k rps (0.20 vCPU per 1k) | Istio 1.24 sidecar, mTLS, telemetry, 1 KB, 2 workers; measured only at 500-1,500 rps | https://istio.io/latest/docs/ops/deployment/performance-and-scalability/ |
| Kong (NGINX/Lua) CPU per 10k rps | **Derived** ~1.1 vCPU per 10k rps (144,273.5 rps / 16 vCPU), assuming full saturation | Kong 3.16, no plugins, c5.4xlarge | https://developer.konghq.com/gateway/performance/benchmarks/ |
| Kong with rate limit + key-auth | **Derived** ~1.6 vCPU per 10k rps (97,290.2 rps / 16 vCPU) | same | https://developer.konghq.com/gateway/performance/benchmarks/ |
| NGINX static web server | **Derived** ~0.14 core per 10k rps (~74K rps per core, 1 KB) | Xeon E5-2699 v3, 2017 | https://blog.nginx.org/blog/testing-the-performance-of-nginx-and-nginx-plus-web-servers |
| KrakenD (Go) | **Derived** ~7.9 vCPU per 10k rps (10,126 rps on 8 vCPU) | 2016, c4.2xlarge, closed loop; not representative of current Go runtimes | https://www.krakend.io/docs/benchmarks/aws/ |
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

### 8.2 Config push fan-out (xDS lessons)

- State-of-the-World xDS requires the server to return all LDS/CDS resources on each response; Incremental (Delta) xDS sends only changed resources (https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol).
- ACK/NACK uses the echoed `version_info` and `response_nonce`; a NACK populates `error_detail` (https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol).
- Make-before-break ordering over ADS: CDS -> EDS -> LDS -> RDS, then remove stale resources, to avoid blackholing traffic (https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol).
- Istio states istiod load scales with configuration change rate and number of connected proxies, but the current page publishes no istiod CPU/memory figures (https://istio.io/latest/docs/ops/deployment/performance-and-scalability/).
- **Analysis:** the Ruralz Control Stream (ADR-0007: snapshot+delta, xDS-style ACK/NACK) inherits these lessons: deltas cap fan-out bandwidth, nonces prevent stale ACKs, and a single ordered stream per Node allows make-before-break.

## Sources

https://github.com/envoyproxy/ratelimit
https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_filter
https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/http/ratelimit/v3/rate_limit.proto
https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting
https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/local_rate_limit_filter
https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rate_limit_quota_filter
https://developer.konghq.com/plugins/rate-limiting/
https://developer.konghq.com/plugins/rate-limiting/reference/
https://developer.konghq.com/plugins/rate-limiting-advanced/
https://developer.konghq.com/plugins/rate-limiting-advanced/reference/
https://developer.konghq.com/gateway/rate-limiting/
https://tyk.io/docs/api-management/rate-limit/
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
https://docs.aws.amazon.com/apigateway/latest/developerguide/api-gateway-request-throttling.html
https://github.com/youtube/doorman
https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/reducing-scope-of-impact-with-cell-based-architecture.html
https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/cell-sizing.html
https://docs.aws.amazon.com/wellarchitected/latest/reducing-scope-of-impact-with-cell-based-architecture/cell-routing.html
https://man7.org/linux/man-pages/man7/socket.7.html
https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/hot_restart
https://www.envoyproxy.io/docs/envoy/latest/operations/cli
https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/operations/draining
https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/network/http_connection_manager/v3/http_connection_manager.proto
https://nginx.org/en/docs/control.html
https://www.haproxy.com/blog/truly-seamless-reloads-with-haproxy-no-more-hacks
https://docs.haproxy.org/3.2/management.html
https://www.rfc-editor.org/rfc/rfc9113.html
https://github.com/grpc/grpc-go/blob/master/internal/transport/http2_server.go
https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/
https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/
https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/ <!-- alias-ok -->
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
https://github.com/envoyproxy/nighthawk
https://www.envoyproxy.io/docs/envoy/latest/faq/performance/how_to_benchmark_envoy
https://www.envoyproxy.io/docs/envoy/latest/faq/performance/how_fast_is_envoy
https://www.krakend.io/
https://www.krakend.io/docs/benchmarks/
https://www.krakend.io/docs/benchmarks/aws/
https://developer.konghq.com/gateway/performance/benchmarks/
https://github.com/Kong/kong-gateway-performance-benchmark
https://apisix.apache.org/docs/apisix/benchmark/
https://tyk.io/performance-benchmarks/
https://github.com/TykTechnologies/tyk-ansible-performance-testing
https://blog.nginx.org/blog/testing-the-performance-of-nginx-and-nginx-plus-web-servers
https://istio.io/latest/docs/ops/deployment/performance-and-scalability/
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
https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol

## Gaps

- Envoy `failure_mode_deny` default is not printed on the fetched pages; "allows traffic by default" is inferred from the `failure_mode_allowed` stat description.
- The envoyproxy/ratelimit README does not name its counting algorithm or state what it returns to Envoy when Redis is unreachable.
- Kong `sync_rate` is contradictory across pages: the OSS plugin overview says only `0` gives synchronous (accurate) behavior, while the OSS reference says the default `-1` "results in synchronous behavior"; the Advanced reference says `-1` means memory-only. The worst-case overage formula on the Advanced page was not captured verbatim.
- The Kong benchmark page does not state the k6 load model (VUs vs arrival rate), payload size or upstream; the ~1.1 vCPU per 10k rps figure assumes 100% CPU saturation, which Kong does not state.
- APISIX benchmark numbers exist only as images on the docs page, with no version or date. Tyk's benchmark page gives no numeric RPS/P99 values in fetched text.
- KrakenD publishes no benchmark with hardware newer than 2016; the homepage "80K+ requests/second on commodity hardware" claim has no stated hardware. No primary source was found for a current Go gateway CPU-per-10k-rps coefficient.
- Envoy publishes no official benchmarks; the only Envoy coefficient is Istio's sidecar figure at 500-1,500 rps with mTLS and telemetry, which likely overstates the per-request cost of a bare proxy.
- The Valkey 8 1.19M rps result is SET-only via `valkey-benchmark`; no Lua/EVAL (GCRA-style script) throughput per core was found for Redis 8 or Valkey 8/9.
- The Mail.Ru post-optimization per-connection memory figure was not captured precisely; only the pre-optimization ~24 KB/connection breakdown is sourced.
- No primary source gives a measured Raft leader failover time for hashicorp/raft, etcd or Consul; the 1-2 s figure is derived from default timeouts.
- No primary source was found for hot-key splitting (sharding one rate-limit key into sub-keys) or for istiod/xDS push fan-out limits (connections per control-plane instance).
- RFC 9113 section 6.8 text could not be retrieved verbatim (page truncation); the two-phase GOAWAY summary comes from search-result excerpts of the RFC plus Envoy's `drain_timeout` description.
- The Kubernetes pod-termination flow section was truncated in fetch; preStop and grace-period facts come from the lifecycle-hooks page. The Kubernetes version in which the preStop `sleep` handler became GA was not verified.
- The oha releases page dates v1.16.0 to 2026-08-23 (https://github.com/hatoo/oha/releases). Nighthawk and k6 latest versions were not captured.
- The AWS Builders' Library shuffle-sharding article redirected to builder.aws.com and returned no body, so shuffle sharding is not covered.
