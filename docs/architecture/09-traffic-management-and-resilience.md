---
title: Traffic Management and Resilience
status: draft
owner: ruralz-core
last_updated: 2026-09-23
depends_on:
  - docs/_meta/foundation-pack.md
  - docs/_meta/style-guide.md
  - docs/architecture/01-system-overview.md
  - docs/architecture/02-configuration-model.md
  - docs/engineering/01-tech-stack-and-libraries.md
adrs: [ADR-0008, ADR-0011, ADR-0014]
milestone_tags_used: [M1, M2, M3, M4]
---

# Traffic Management and Resilience

## Summary

This document fixes how a Ruralz Gateway Node selects and ejects Endpoints, bounds time and retries, breaks circuits, enforces Rate Limits and Quotas, serves the Response Cache and sheds overload. It defines runtime behavior for `Upstream` resilience fields and the `ratelimit`, `quota` and `cache` Policy types, and owns the `RZ-UP` and `RZ-RL` registries. Rate limiting follows [ADR-0008](../adr/0008-rate-limiting-local-bucket-and-gcra.md): local token bucket, then State Store GCRA, failing open by default. Architects, operators and contributors should read it.

## Scope and non-goals

In scope: the Summary's subjects, including `config` of `ratelimit`, `quota` and `cache` ([Configuration model](02-configuration-model.md#policy)), plus `Route.spec.timeout`, weighted `upstreams`, IETF RateLimit headers and the KrakenD EE traffic mapping. "Pack 8.8" names a [foundation pack](../_meta/foundation-pack.md) section. Nothing here is implemented.

Non-goals, with owners: kinds and fields ([Configuration model](02-configuration-model.md)); precedence, error format, buffer budgets ([Data plane](03-data-plane.md)); accuracy bounds, State Store sizing, Cells ([Scalability and distributed state](11-scalability-and-distributed-state.md)); Token Budgets ([ADR-0014](../adr/0014-ai-api-surface.md)), Provider Fallback ([AI/LLM gateway](06-ai-llm-gateway.md)); `authz.ip`, `authz.geoip`, pre-authentication limits ([Security and identity](08-security-and-identity.md)); streams ([Multi-protocol](07-multi-protocol.md)); authoritative numbers ([Performance budgets and benchmarking](12-performance-budgets-and-benchmarking.md), which wins).

## Load balancing

Endpoint selection runs per attempt, between `onRoute` and `onUpstreamRequest`, on Node-local state, Planned (M1) for `http` Upstreams. A Route picks one of its `upstreams` per request by `weight`; the Upstream picks an Endpoint by `loadBalancing.algorithm`, weighted by `endpoints[].weight` or SRV weights.

| Algorithm | Selection | Best for |
|---|---|---|
| `round-robin` | Smooth weighted rotation over a precomputed schedule | Uniform, short requests |
| `least-request` | Two random candidates; fewer in-flight per weight wins, counting failures of the last 1 s (target) | Mixed latency; proposed default |
| `ring-hash` | Required CEL `hashKey` onto a ring, skipping ejected or tried Endpoints | Cache affinity |
| `random` | Uniform weighted pick | Very large Endpoint sets |

Schedules (max(65,536, Endpoints) 4-byte slots, about 256 KiB) and rings (min(65,536, 1,024 × Endpoints) 16-byte virtual nodes, at most 1 MiB, fixed hash per OQ-traffic-management-and-resilience-15) rebuild off the request path at most every 10 s and swap copy-on-write (target). A pick skips ejected slots for one pass, then applies panic mode.

Health, ejection and breaker state survive Hot Reloads.

*Figure 1: load-balancing Endpoint selection for one attempt on an upstream leg.*

```mermaid
flowchart TD
    leg["onRoute: pick the upstream leg by upstreams weight"] --> ups["Upstream settings from the active Revision snapshot"]
    ups --> src{"Endpoint source"}
    src -- "endpoints" --> set["Endpoint set, swapped copy-on-write"]
    src -- "dns or kubernetes discovery" --> set
    set --> empty{"Set empty?"}
    empty -- "yes" --> e008["503 RZ-UP-008"]
    empty -- "no" --> filt["Drop passively ejected and actively unhealthy Endpoints"]
    filt --> any{"Healthy Endpoint left?"}
    any -- "no" --> panic["Panic mode: use every Endpoint, ruralz_upstream_degraded_info"]
    any -- "yes" --> excl["Exclude tried Endpoints while an untried healthy one remains"]
    panic --> excl
    excl --> alg{"loadBalancing.algorithm"}
    alg -- "round-robin" --> rr["Bounded smooth weighted schedule, one pass over ejected slots"]
    alg -- "least-request" --> p2c["Two random choices, in-flight plus recent failures per weight"]
    alg -- "ring-hash" --> rh["Hash hashKey onto the ring"]
    alg -- "random" --> rnd["Uniform weighted pick"]
    rr --> gate{"Breaker closed and in-flight slot or pending room?"}
    p2c --> gate
    rh --> gate
    rnd --> gate
    gate -- "no" --> e005["503 RZ-UP-005 or RZ-UP-006"]
    gate -- "yes" --> att["Attempt: pooled connection, or dial within 1 s and TLS within 2 s (target)"]
```

## Service discovery

| Source | Fields | Behavior | Planned |
|---|---|---|---|
| Static | `endpoints[].address`, `weight` | Host names resolve every 30 s (target), keeping the last good answer | Planned (M1) |
| DNS | `discovery.type: dns`, `service`, `port` | Numeric `port`: A and AAAA; named: SRV `_<port>._tcp.<service>`. Every 30 s, 10% jitter (target) | Planned (M1) |
| Kubernetes | `discovery.type: kubernetes`, `service`, `namespace`, `port` | EndpointSlice watch (client: OQ-tech-stack-and-libraries-23) | Planned (M2) |

Kubernetes discovery uses serving, non-terminating Endpoints, or terminating ones when all terminate ([source](https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/)).

On lookup failure or an unreachable Kubernetes API the Node keeps its last set, raises `ruralz_upstream_degraded_info` (reason `discovery_stale`) and retries with full-jitter backoff from 1 s to 60 s (target). NXDOMAIN or an empty answer counts as a resolver failure until 3 consecutive refreshes repeat it (target); only an EndpointSlice with zero serving Endpoints empties the set at once (`RZ-UP-008`). Registry-specific clients are Not planned: each adds a dependency to every `ruralzd` build, and registries answering DNS SRV work through `discovery.type: dns`.

## Health checking and outlier detection

Each Node judges health alone (P3 in [Vision and positioning](../vision/01-vision-and-positioning.md#principles)), Planned (M1):

| Mechanism | Fields | Rule |
|---|---|---|
| Passive ejection, on by default | `healthCheck.passive.consecutiveErrors`, `ejectionTime` | That many attempts matching `circuitBreaker.failureWhen` eject the Endpoint for `ejectionTime` times its ejection count, at most 10 (target) |
| Active checks, when set | `healthCheck.active.path`, `interval`, `timeout`, `healthyThreshold`, `unhealthyThreshold` | `GET path` every `interval`, 10% jitter (target); 200 to 399 within `timeout` succeeds; new Endpoints start healthy |

At most 50% of Endpoints are passively ejected at once (target), so a bad deploy cannot empty the set; active removals and connect errors, dial timeouts included, bypass the cap, so a lost zone leaves rotation. With no healthy Endpoint the Node uses all (panic mode, reason `panic`); for OQ-data-plane-7 this document recommends all-or-nothing panic at M1, a threshold field being OQ-traffic-management-and-resilience-7. `ruralz_upstream_ejections_total` (reason `passive` or `active`) is defined in [Observability](10-observability.md).

Probe slots per Node are min(256, ceil(probes per second × `timeout`)) (target), shared fairly across Upstreams; skipped probes count in `ruralz_upstream_probes_skipped_total` and, above 10% for 1 minute, raise degraded reason `probes_skipped` (target), both proposed to Observability. A Node actively checks about 1,280 Endpoints at `interval: 10s`, `timeout: 2s` (target); beyond that, and in large Clusters (N / `interval` probes per Endpoint), zone loss rests on passive ejection and the dial timeout. gRPC checks use `grpc.health.v1`, Planned (M3).

## Timeouts, deadlines, retries and hedging

### Deadlines

Each bound is clamped by the one above:

| Bound | Field | On expiry |
|---|---|---|
| Request, streamed body included | `Route.spec.timeout` | `RZ-RT-007` before any attempt, `RZ-UP-003` during one, `RZ-UP-009` after commit |
| Leg, attempts and backoff included | `Upstream.spec.timeout` | `RZ-UP-003` |
| Attempt, to the last response header byte | `retries.perTryTimeout` | Error kind `timeout`; `retryOn` decides |
| Dial; TLS handshake | Fixed 1 s; 2 s (target); fields per OQ-traffic-management-and-resilience-6 | Error kind `connect`; `tls` |
| State Store round trip | `Policy.spec.stateStoreTimeout`, clamped by the Route's largest (pack 8.7) | `failureMode` |

Only attempt, leg or Route deadlines yield kind `timeout`, so a black-holed Endpoint fails fast as `connect` and retries elsewhere. gRPC carries the remaining deadline in `grpc-timeout`. Proposed defaults (OQ-traffic-management-and-resilience-6):

| Field | Proposed default |
|---|---|
| `Route.spec.timeout` | 15 s; 1 h for WebSocket, GraphQL subscription, gRPC streaming and `ai` Routes (target) |
| `retries.perTryTimeout` | Leg time left divided by (retries left + 1) (target) |
| `retries.attempts` | 1 retry |
| `retries.retryOn` | GET, HEAD, OPTIONS, PUT and DELETE: `connect` or `reset` errors, or 503; other methods: `connect` errors only |
| `circuitBreaker.failureWhen` | `error != null \|\| response.status in [502, 503, 504]` |
| `circuitBreaker` limits | `maxConnections` 1,024, `maxPendingRequests` 256, `consecutiveFailures` 5, `openDuration` 30 s (target) |
| `healthCheck.passive` | `consecutiveErrors` 5, `ejectionTime` 30 s (target) |

For OQ-data-plane-10 this document recommends a fixed 60 s idle limit between chunks (target).

### Retries

`retries.attempts` counts retries, so `attempts: 2` allows three attempts. A retry needs `retryOn` true (a runtime error means no retry) and all of:

1. A replayable body, empty or buffered within the `onRequestBody` cap, and nothing committed.
2. Attempts and deadline left.
3. A breaker not open, and retry budget room.
4. Any Upstream `Retry-After`, capped at 10 s (target), ending before the leg deadline.

Backoff is full-jitter exponential from 25 ms to 250 ms (target), or a longer `Retry-After`; each retry reruns `onUpstreamRequest`. A leg whose last attempt got a response returns it unchanged; otherwise the [code selection rule](#error-codes-this-document-owns) applies.

### Retry budget

Each Node keeps a retry budget per Upstream: in-flight retries, backoff included, MUST NOT exceed max(3, 20% of in-flight originals) (target). Over budget, the retry is skipped, the original failure returns, and `ruralz_upstream_retry_budget_exhausted_total` increments. From 15 originals in flight, amplification is at most 1.2 times (target). Budget fields are OQ-traffic-management-and-resilience-5.

### Hedging

A hedge sends a second attempt to another Endpoint after a delay, for idempotent methods with replayable bodies only; the first response wins, the loser is cancelled, and hedges count against the retry budget. Planned (M4), once OQ-traffic-management-and-resilience-5 adds a delay field.

*Figure 2: retry loop per upstream leg and circuit breaker per Upstream per Node.*

```mermaid
stateDiagram-v2
    state "Retry loop, per upstream leg" as RetryLoop {
        [*] --> Attempt
        Attempt --> Returned: retryOn false or runtime error
        Attempt --> Evaluate: retryOn true
        Evaluate --> Backoff: attempts, budget, deadline, replayable body, breaker not open, Retry-After fits
        Evaluate --> Exhausted: any condition fails
        Backoff --> Attempt: jittered delay, untried Endpoint
        Returned --> [*]
        Exhausted --> [*]
    }
    state "Circuit breaker, per Upstream per Node" as Breaker {
        state "Half-open" as HalfOpen
        [*] --> Closed
        Closed --> Open: consecutiveFailures failed legs and at least 50% of at least 20 legs in 10 s (target); connect errors only past 50% ejected (target)
        Open --> HalfOpen: jittered openDuration elapsed
        HalfOpen --> Closed: 3 consecutive probe successes (target)
        HalfOpen --> Open: a probe fails
    }
```

## Circuit breakers

Each Node keeps one breaker and one in-flight ceiling per Upstream, Planned (M1); passive ejection isolates Endpoints. The breaker counts legs whose final result after retries matches `failureWhen`; a connect error counts only while over 50% of Endpoints are ejected (target), so a bad Endpoint or lost zone never opens it.

| Field | Role | When exceeded |
|---|---|---|
| `consecutiveFailures` | Opens the breaker once that many consecutive legs failed and at least 50% of at least 20 legs in a rolling 10 s window (target) | 503 `RZ-UP-005` at once, no retry |
| `openDuration` | Open time, jittered ±20% (target) | Half-open: one probe at a time; 3 consecutive successes close it, a failure reopens (target) |
| `failureWhen` | CEL failure class, such as 429; a runtime error counts as failure | Not applicable |
| `maxConnections` | In-flight attempts per Upstream per Node, a semaphore taken before `RoundTrip`, so HTTP/2 streams count | Attempts wait |
| `maxPendingRequests` | Waiters for that semaphore, each bounded by its attempt context | 503 `RZ-UP-006` at once |

The guards are fixed until OQ-traffic-management-and-resilience-5 adds fields. At 10% independent failures, a 20-leg window reaches 50% with probability about 7 × 10^-6 (hypothesis).

A dial holds its slot at most 1 s (target): at 1,000 attempts per second per Node, a black-holed third of Endpoints holds about 333 of 1,024 slots until ejected (hypothesis), fewer under `least-request`.

`ruralz_upstream_breaker_state_info` is defined in [Observability](10-observability.md). Post-close ramp, locality and slow start are OQ-traffic-management-and-resilience-8.

## Rate limiting

A Rate Limit is a `ratelimit` Policy: admission class, `onRequestHeaders`, Gateway or Route scope, default `failureMode: open`, Planned (M1). `config.key` (CEL) partitions traffic; every `config.limits[]` entry must allow `requests` per `window`, plus a burst of `requests`. Route limits stack on Gateway limits (pack 8.12). Limits apply per Cell, so R Regions allow R × each limit. Auth runs first.

### Algorithm choice

[ADR-0008](../adr/0008-rate-limiting-local-bucket-and-gcra.md) chooses a hybrid over the algorithms other gateways use:

| Algorithm | Accuracy | State Store cost per request | Multi-Node behavior | Burst handling | Used by |
|---|---|---|---|---|---|
| Fixed window counter | Up to 2 × limit across a boundary | One `INCR` | Per Node unless counters are shared | Full limit per window | Kong ([source](https://developer.konghq.com/plugins/rate-limiting/reference/)), Tyk ([source](https://tyk.io/docs/api-management/rate-limit/)) |
| Sliding window counter | Approximate; Cloudflare measured 0.003% wrong decisions | Two counters | Counters per location | Smoothed | Cloudflare ([source](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/)), Kong Advanced ([source](https://developer.konghq.com/plugins/rate-limiting-advanced/reference/)) |
| Token bucket | Exact within one store | Tokens and refill time | Local, or one shared bucket | Bucket size | Stripe ([source](https://stripe.com/blog/rate-limiters)), Envoy local ([source](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/local_rate_limit_filter)) |
| GCRA | Exact within one store | One timestamp (TAT) per key | Global when shared | τ; τ = window allows 2 × limit | redis_rate ([source](https://github.com/go-redis/redis_rate)), Brandur ([source](https://brandur.org/rate-limiting)) |
| Local bucket plus GCRA (chosen) | GCRA for spread keys; per-Node ceiling for a pinned client | None for local denials, else one script | Bucket shields hot keys; GCRA enforces the Cell limit | Ceiling per Node; τ globally | Envoy local plus global ([source](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/other_features/global_rate_limiting)) |

### Decision path

1. `when` false skips the Policy. A `config.key` runtime error applies `failureMode` with no bucket: `open` admits unmetered; `closed` returns 503 `RZ-RL-005`.
2. A key whose last GCRA answer was a deny is denied from a per-Node over-limit cache until its retry-after ([source](https://github.com/envoyproxy/ratelimit)).
3. A key with no local entry is first-seen and spends one unit of a per-Node budget, max(1, B / N_published) per second across all Policies, with B = 20,000 per Cell, 20% of a shard (hypothesis). Within budget it gets full buckets. Past it, it gets a local-only entry for 10 s or until evicted (target): capacity 1, refill 2 × `requests` / N_published per window, no GCRA. First-seen keys thus cost at most B calls per second per Cell, whatever N and the Policy count; a local-only key admits about 2 × limit per window Cell-wide, plus one token per Node (target).
4. Each `limits[]` entry has a local token bucket at its per-Node ceiling; all must hold a token, else 429 `RZ-RL-001`. A local-only key is then admitted.
5. Otherwise one `EVALSHA` runs GCRA for every limit with server `TIME`, updating TATs only if all allow; a deny is 429 `RZ-RL-002`. On `NOSCRIPT` it is resent as `EVAL` within the same deadline.
6. A failed call or an open State client breaker applies `failureMode`: `open` admits within the local buckets; `closed` returns 503 `RZ-STS-<NNN>` ([Scalability and distributed state](11-scalability-and-distributed-state.md)).

This local admission amends pack 8.8 (OQ-traffic-management-and-resilience-20). A new Node, after scale-out or a Zero-Downtime Upgrade, starts empty: 50,000 active keys on 100 Nodes leave local-only in about 250 s (hypothesis).

Keys are `rz:rl:<policy>:<requests>/<window>:{<SHA-256 of the key value>}`; the tag keeps a partition in one slot, so consumptive calls with equal keys share one script (pack 8.7). `PEXPIRE` at TAT − now + τ drops idle keys; τ is one `window` (burst field: OQ-traffic-management-and-resilience-1).

Buckets, local-only and over-limit entries share a Node-wide budget of 1,048,576 entries, about 64 MiB (target), in the CLOCK-evicted shards of [Data plane](03-data-plane.md), surviving Hot Reloads.

A key reaches the State Store at most min(offered, N × ceiling) times per window. A shard serves about 100,000 calls per second (hypothesis; Valkey 8 measured 1.19 million `SET`, not `EVAL`, per second, [source](https://valkey.io/blog/unlock-one-million-rps-part2/)); hotter keys, such as a busy service limit's constant key, need local-only enforcement (OQ-traffic-management-and-resilience-1). The State client breaker is per shard per Node; a GCRA round trip takes 1 ms at p99 within a zone (hypothesis).

*Figure 3: rate-limit decision, local token bucket first, then GCRA in the State Store.*

```mermaid
flowchart TD
    req["onRequestHeaders reaches a ratelimit Policy"] --> when{"Policy when true?"}
    when -- "false" --> skip["Policy skipped"]
    when -- "true" --> key["Evaluate config.key"]
    key -- "runtime error" --> kfm{"failureMode on key error"}
    kfm -- "open" --> kskip["Policy skipped, unmetered"]
    kfm -- "closed" --> r503k["503 RZ-RL-005"]
    key -- "key evaluated" --> olc{"Key in over-limit cache?"}
    olc -- "yes" --> deny2["429 RZ-RL-002 with Retry-After"]
    olc -- "no" --> nk{"Local entry?"}
    nk -- "yes, or first-seen within budget" --> lb{"Every local bucket has a token?"}
    nk -- "first-seen past budget" --> local["Local-only entry: capacity 1, 10 s (target)"]
    local --> lb
    lb -- "no" --> deny1["429 RZ-RL-001, no State Store call"]
    lb -- "yes" --> lo{"Local-only entry?"}
    lo -- "yes" --> ok["Admit"]
    lo -- "no" --> brk{"State client breaker open for the shard?"}
    brk -- "yes" --> fm{"failureMode on State Store failure"}
    brk -- "no" --> gcra["GCRA for all limits in one script, server TIME"]
    gcra -- "allow" --> ok
    gcra -- "deny" --> cache["Cache the deny until retry-after"]
    cache --> deny2
    gcra -- "error or timeout" --> fm
    fm -- "open, default" --> admit["Admit from unfloored local buckets (target)"]
    fm -- "closed" --> r503["503 RZ-STS-NNN"]
```

### Per-Node ceiling

The ceiling is declared on the Policy (OQ-traffic-management-and-resilience-1, blocking for M1), else derived from N_published, the `HeartbeatReply` `clusterNodeCount`, else the full limit with a degraded metric. Pack 8.8 derives limit / N; this document proposes min(`requests`, max(10, ceil(2 × `requests` / N_published))) (target), pending OQ-traffic-management-and-resilience-16: the factor 2 absorbs uneven spread, the floor serves pinned clients.

The floor alone breaks the fail-open bound (10 per `1s` on 100 Nodes admits 1,000 per second), so while GCRA fails each bucket is clamped to max(1, 2 × `requests` / N_published) tokens, refilled in one step per epoch-aligned window without carry-over (target). An aligned window then admits N_serving / N_published × 2 × limit, at least one token per Node; declared and full-limit ceilings admit N_serving × ceiling (target).

[Control plane and GitOps](04-control-plane-and-gitops.md#replica-roles) counts "ready, streaming Nodes that ACKed a Revision and stayed connected 10 minutes (target)", publishes large decreases at once and raises increases at most 10% per minute. Nodes apply the value as received and keep it while Ruralz Control is down. Since a larger N tightens ceilings, this lag is unsafe: scaling 10 to 100 Nodes keeps N_serving / N_published up to 10 for about 34 minutes (hypothesis), sending hot keys to GCRA up to 10 times as often and letting fail-open admit 20 × limit; declare ceilings where that matters until OQ-traffic-management-and-resilience-19 closes.

### Service and tiered limits

A service limit uses a constant `key`; a tiered limit is one Policy per Tier, guarded by `when`:

```yaml
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: ratelimit-gold}
spec:
  type: ratelimit
  when: 'consumer != null && consumer.tier == "gold"'
  config:
    key: "consumer.name"
    limits:
      - {requests: 100, window: 1s}
      - {requests: 5000, window: 1m}
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: ratelimit-free}
spec:
  type: ratelimit
  when: 'consumer == null || consumer.tier != "gold"'
  config:
    key: 'consumer == null ? source.ip : consumer.name'
    limits:
      - {requests: 10, window: 1s}
```

A tier table in one Policy is OQ-traffic-management-and-resilience-4; IPv6 prefix keys are OQ-traffic-management-and-resilience-18.

### RateLimit response headers

Ruralz Gateway will emit Internet-Draft `draft-ietf-httpapi-ratelimit-headers-11` fields as RFC 9651 Structured Field lists, Planned (M1) ([source](https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-ratelimit-headers)):

| Field | Content | When |
|---|---|---|
| `RateLimit-Policy` | Per applied limit: the Policy name as a String (`.1`, `.2` for several limits), `q` = `requests` or the quota `limit`, `w` = `window` in seconds, rounded up | 429 responses (`RZ-RL-001` to `RZ-RL-003`) |
| `RateLimit` | The denied limit: `r` remaining, `t` seconds until it admits | Same |
| `Retry-After` | Equal to `t`, taking precedence over `RateLimit` as the draft requires | Every 429 |

`RZ-RL-001` gives `r=0` and `t` until the local bucket refills; `RZ-RL-002` the GCRA or cached retry-after; `RZ-RL-003` `r=0` and `t` to the jittered window end. `q` is the sustained rate; τ of one window allows 2 × `q` in one window. `pk`, which would disclose a client address or Consumer name, and `qu` are omitted, as is every field for `ai.token-budget`. Admitted responses wait on OQ-traffic-management-and-resilience-2, and `X-RateLimit-*` on OQ-traffic-management-and-resilience-3.

```text
HTTP/1.1 429 Too Many Requests
Content-Type: application/problem+json
RateLimit-Policy: "ratelimit-global";q=1000;w=1, "ratelimit-gold.1";q=100;w=1, "ratelimit-gold.2";q=5000;w=60
RateLimit: "ratelimit-gold.1";r=0;t=1
Retry-After: 1
```

## Quotas

A Quota is a long-window Consumer allowance (the Consumer quota's `limit` per `window`), checked before commit and settled asynchronously:

| Unit | Policy | Admission | Settlement | Planned |
|---|---|---|---|---|
| `requests` | `quota` (`config.consumerQuota`, `config.key`) | `onRequestHeaders`: one atomic script reserves one unit if the window has room | `onLog`: refunds after a later Filter rejection or `RZ-UP-005`, `RZ-UP-006` or `RZ-UP-008`; cache hits are charged | Planned (M1) |
| `tokens` (LLM input plus output) | `ai.token-budget` (`config.consumerQuota`) | `onRequestBody`: reserves estimated input plus output cap C (pack 8.9) | `onLog`: charges provider-reported usage, releases the rest | Planned (M3) |

Windows are fixed and epoch-aligned in UTC: `24h` resets at UTC midnight, `720h` every 30 days. Each key and window costs one counter, `rz:qt:<consumerQuota>:<window>:<window start>:{<SHA-256 of the key value>}`, expiring one window after it closes. Policies naming one Consumer quota share its counter, charged once per request; renaming a Consumer resets usage under the default `key`, `consumer.name`. Since scripts touch only `KEYS` ([source](https://redis.io/docs/latest/develop/programmability/eval-intro/)), the Node passes the window keys for its clock and the nearer neighbor; the script picks by server `TIME`, tolerating skew up to half the window (target). Quotas apply per Cell; calendar months and weighted costs are OQ-traffic-management-and-resilience-10.

```yaml
apiVersion: ruralz/v1alpha1
kind: Consumer
metadata: {name: partner-acme}
spec:
  tier: gold
  credentials:
    apiKeys:
      - name: primary
        hash: "sha256:d1bec0f9342e5607570b2636ed0256023b13be7d7d4bb45e1bed3cfee80dc1b6"
  quotas:
    - {name: monthly-requests, unit: requests, limit: 3000000, window: 720h}
    - {name: daily-tokens, unit: tokens, limit: 2000000, window: 24h}
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: quota-monthly}
spec:
  type: quota
  config:
    consumerQuota: monthly-requests
```

An exhausted quota returns 429 `RZ-RL-003` with `Retry-After` at the window end plus up to 1% of the window, at most 60 s (target). The Node caches the denial, keyed with the limit, until the window end or at most 60 s (target), so a raised limit applies at once and refunds within 60 s, for one script per minute per exhausted key per Node, about 17 per second at 1,000 Nodes (hypothesis). Before exhaustion each request costs one script: 20,000 per second on one key take 20% of a shard (hypothesis), unless a `ratelimit` on that key, sharing the script (pack 8.7), bounds it. A null Consumer, or one lacking the quota, gets 403 `RZ-RL-004`.

Dropped refunds only over-charge, and a failover can lose recent reservations. For OQ-configuration-model-8 this document recommends option (a): `quota` fails open, `ai.token-budget` closed, since unmetered tokens are provider spend.

## Response caching

The Response Cache is the `cache` Policy: Route scope, lookup in `onRequestHeaders` after auth, authz and admission, store in `onResponse`, default `failureMode: open`, Planned (M1). It is an RFC 9111 shared cache plus RFC 5861 `stale-while-revalidate` and `stale-if-error`.

| Rule | Behavior |
|---|---|
| Methods | GET, and HEAD from GET entries |
| Layout | One hash tag per URI, `{<SHA-256 of scheme, host, path, normalized query>}`, holds a metadata key (generation, `Vary` names), a fill lease and at most 8 variants per partition (target) |
| Storable | Explicit freshness; no `no-store`, `no-cache` (never stored at M1), `private`, `Vary: *` or `Set-Cookie`; with `Authorization`, only under `public`, `s-maxage` or `must-revalidate` |
| Principal | `auth.*` Routes key per principal unless `config.key` replaces it (Tier example below), proposed as the sharing field of OQ-security-and-identity-25 |
| Freshness | `s-maxage`, `max-age`, then `Expires`, at most 24 h, stale windows at most 1 h (target); hits carry `Age` and `Cache-Status` (RFC 9211) |
| Stale limits | `must-revalidate`, `proxy-revalidate` and `s-maxage` (implying `proxy-revalidate`, RFC 9111 section 5.2.2.10) forbid stale (section 4.2.4), overriding the older RFC 5861 |
| Request directives | `no-cache` or `max-age=0` revalidates; `no-store` bypasses; `only-if-cached` misses with 504 |
| Revalidation | `If-None-Match` or `If-Modified-Since`; 304 refreshes metadata |
| Invalidation | A non-error response to an unsafe method on a `cache` Route bumps the URI's generation after commit, in that Cell; hot-entry layers may serve the old entry 1 s longer (target) |
| Size | From a tee, up to 128 KiB (target), within the Node buffer budget |

A hit skips `onRequestBody`, so validation rejects an effective Filter Chain combining `cache` with an `onRequestBody` authz, validation, or `plugin` auth or authz Policy (`RZ-CFG` code: OQ-traffic-management-and-resilience-21).

Nodes cache `Vary` names per URI in a bounded LRU and send the metadata, lease and selected variant keys; one round trip returns the variant if names and generation match, else a miss with current names. On a miss or stale answer the script also sets the 1 s lease with `SET NX PX` (target): only the holder stores or revalidates; other Nodes serve stale or fetch without storing, so a cold miss costs up to N origin fetches (hypothesis) but one store. Stores use the post-commit queue (pack 8.7), with TTL of freshness plus the larger stale window.

Generation bumps get a reserved 10% of that queue and one retry (target); a drop counts in `ruralz_state_writes_dropped_total` (`kind` value `cache_invalidate` proposed to [Observability](10-observability.md)) and leaves the old variant up to 25 h (target). RFC 9111 section 4.4 invalidates after any unsafe method (OQ-traffic-management-and-resilience-11).

The State Store holding limit keys MUST run `maxmemory-policy noeviction`. Until OQ-traffic-management-and-resilience-11 closes (blocking for M1), the entry size cap and two rules protect limits:

1. A 64 MiB per-Node hot-entry layer keeps, by LRU, entries read twice within 1 s, for at most 1 s (target): a hot entry costs at most N reads per second, about 131 MB/s for 128 KiB at 1,000 Nodes, 10% of a 10 Gb/s shard link (hypothesis).
2. Nodes read `INFO memory` every 10 s off the request path and skip stores above 70% of `maxmemory` (target).

`ruralz_cache_store_skipped_total` reasons are `size_limit`, `buffer_budget` and `state_store_memory`. A hit first reserves its size from `limits.maxBufferedBytes`, else bypasses.

A store saves the upstream request as the request Phases left it, minus upstream credentials. Under `stale-while-revalidate` the Node serves stale at once and the lease holder queues a revalidation; four workers drain a 256-entry queue that drops when full, replaying that request through upstream-leg Policies under the breaker, the bulkhead and a 5 s deadline (target). A 304 refreshes metadata; any other result ends the stale window, unstored. `stale-if-error` serves stale when the Upstream fails or its breaker is open.

Concurrent misses for one key on one Node, up to 4,096 keys (target), wait for the first fetch and reuse it, like a hit through their own response chain, only if storable with matching `Vary`-selected values; otherwise each fetches within its own deadline.

```yaml
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: cache-catalog}
spec:
  type: cache
  config:
    key: 'consumer == null ? "anonymous" : consumer.tier'   # one partition per Tier
```

*Figure 4: a Response Cache lookup that serves a stale entry and revalidates in the background.*

```mermaid
sequenceDiagram
    participant C as Client
    participant N as Node
    participant SS as State Store
    participant UP as Upstream
    C->>N: GET /v1/catalog
    N->>SS: lookup script, metadata, lease and variant keys in KEYS, one round trip
    SS-->>N: variant stale, inside stale-while-revalidate, lease granted
    N-->>C: stale response with Age and Cache-Status
    N-)UP: replay saved request with If-None-Match, breaker, bulkhead, 5 s deadline
    alt 304 Not Modified
        UP--)N: 304
        N-)SS: refresh entry metadata, post-commit queue
    else any other status or error
        UP--)N: other result
        N-)SS: end the stale window, store nothing
    end
```

## Traffic shaping

Weighted splits and blue-green use `upstreams[].weight`, picked per request; header, cookie or query canaries use a higher-ranked Route with `match.headers` or `match.when`; spike arrest is a short-`window` `ratelimit`; the bulkhead is `circuitBreaker.maxConnections`. All are Planned (M1). Splits stick only through a client-sent header or cookie (percentage-sticky: OQ-traffic-management-and-resilience-17). Mirroring, which KrakenD has in both editions ([source](https://www.krakend.io/features/)), is OQ-traffic-management-and-resilience-12.

```yaml
apiVersion: ruralz/v1alpha1
kind: Route
metadata: {name: orders-split}
spec:
  match:
    hosts: ["api.shop.example"]
    path: {prefix: "/v1/orders"}
  upstreams:
    - {name: orders-v1, weight: 95}
    - {name: orders-v2, weight: 5}
  timeout: 3s
```

### KrakenD EE routing and traffic features

Every KrakenD EE-only routing and traffic feature in the 2026-09-23 snapshot ([source](https://www.krakend.io/features/)) maps to a free mechanism:

| KrakenD EE feature | Ruralz mechanism | Planned |
|---|---|---|
| Catch-all fallback | Lowest-ranked Route matching `when: "true"` ([Data plane](03-data-plane.md#precedence)) | Planned (M1) |
| Header and query string based dynamic routing | `match.headers`, `match.when` over `request.query`, or `conditional` composition | Planned (M1) |
| Conditional routing | `composition.mode: conditional` with CEL ([ADR-0011](../adr/0011-expressions-and-authorization-engines.md)) | Planned (M1) |
| Wildcard routes | `match.path` `prefix`, `template` or `regex`; wildcard hosts per OQ-data-plane-2 | Planned (M1) |
| URL rewrite | Composition step `path` or `pathExpression`; plain `upstreams` per OQ-traffic-management-and-resilience-13 | Planned (M1) |
| Virtual hosts | `match.hosts` with listener `hostnames` | Planned (M1) |
| Configurable client redirects | Upstream 3xx pass through; Route-issued redirects use a `plugin` Policy; built-in fields per OQ-traffic-management-and-resilience-13 | Planned (M2) |
| Customizable HTTP circuit breaker | `circuitBreaker` with CEL `failureWhen` | Planned (M1) |
| Service rate limit | `ratelimit` with a constant `config.key` | Planned (M1) |
| Tiered rate limit | One `ratelimit` per Tier guarded by `when` | Planned (M1) |
| Stateful rate limit (Redis backed) | GCRA in the `redis` driver | Planned (M1) |
| IP filtering | `authz.ip` ([Security and identity](08-security-and-identity.md)) | Planned (M1) |
| MaxMind GeoIP | `authz.geoip` | Planned (M2) |

## Overload protection

Every protection is a bounded resource that rejects fast: [Data plane](03-data-plane.md) owns the Node ceilings (503 `RZ-RT-004`, `RZ-RT-005`); the failure matrix lists the rest. Tier-priority shedding (Stripe sheds non-critical traffic first, [source](https://stripe.com/blog/rate-limiters)) and adaptive concurrency are OQ-traffic-management-and-resilience-14.

## Failure matrix

"Open" admits or bypasses; "closed" rejects. [Observability](10-observability.md) maps these rows to metrics; failover and Cell loss show only as call errors (OQ-observability-18).

| Dependency failure | Affected mechanism | Default behavior | Fail-open or fail-closed | Configurable |
|---|---|---|---|---|
| State Store slow or unreachable | `ratelimit` | Admit from unfloored buckets: N_serving / N_published × 2 × limit per aligned window, at least one token per Node (target) | Open | `failureMode` |
| State Store slow or unreachable | `quota`, `cache`, `ai.token-budget` | `quota` admits unmetered; `cache` bypasses; `ai.token-budget` gives 503 `RZ-STS-<NNN>` | Open; Closed for `ai.token-budget` | `failureMode` |
| One shard failing, breaker open | Policies with keys there | `failureMode` at once, no deadline paid | Open; Closed for `ai.token-budget` | `failureMode` |
| Key-rotation flood or new Node | `ratelimit` | First-seen keys past the Cell budget of 20,000 calls per second (hypothesis) go local-only | Open | OQ-traffic-management-and-resilience-20 |
| Hot, expiring or invalidated cache entry | `cache`; its shard; Upstream | At most N reads per second per entry (target); one store and revalidation per lease; cold misses up to N fetches (hypothesis) | Open | OQ-traffic-management-and-resilience-11 |
| State Store out of memory | Limit scripts; cache stores | Stores skipped above 70% (target); at OOM limits fail open | Open; Closed for `ai.token-budget` | `failureMode` |
| State Store evicts keys (not `noeviction`) | `ratelimit`, `quota`, `ai.token-budget` | Silent resets, unbounded over-admission | Open | No; `noeviction` is REQUIRED |
| Post-commit queue full | Post-commit writes | Drop and count; invalidations get a reserved slice, else stale up to 25 h (target) | Open | No |
| State Store failover | `ratelimit`, `quota` | Lag rolls state back: over-admission up to lag × limit / window (hypothesis) | Open | No |
| Region or Cell lost | `ratelimit`, `quota`, `cache` | Survivors count alone: up to R × each limit; other Cells miss invalidations | Open | No |
| Scale-out within the count qualification window | Derived ceiling | New Nodes count after 10 minutes, then +10% per minute (target); meanwhile the bound is N_serving / N_published × 2 × limit | Open | Declared ceiling; OQ-traffic-management-and-resilience-19 |
| No Node count yet | Derived ceiling | Full limit, degraded metric | Open | Declared ceiling |
| Nodes lost | Derived ceiling | Large decreases publish at once, small ones after 60 s (target); keys under-admit meanwhile | Closed | No |
| Endpoint or zone refusing connections, or dial timeout | Load balancing | Retry elsewhere; eject after `consecutiveErrors`, past the cap; breaker counts it only past 50% ejected (target) | Open | `retries`, `healthCheck` |
| Zone lost, black-holed | Load balancing | Dials fail at 1 s (target) as connect errors, retry and eject after `consecutiveErrors`; pooled requests end at `perTryTimeout`; the ratio guard holds the breaker | Open | `healthCheck`, `perTryTimeout` |
| Endpoint reset | Load balancing | Idempotent methods retry; others get 502 `RZ-UP-004` | Closed for non-idempotent methods | `retryOn` |
| Upstream TLS failure or handshake timeout | Upstream layer | 502 `RZ-UP-002`, no retry | Closed | `retryOn`, `tls` |
| Every Endpoint unhealthy, or none left | Load balancing | Panic mode; an empty set gives 503 `RZ-UP-008` | Open; Closed when empty | OQ-traffic-management-and-resilience-7 |
| Discovery source down, NXDOMAIN or empty answer | Discovery | Keep the last set, `discovery_stale`; NXDOMAIN until 3 refreshes repeat it (target) | Open | No |
| Breaker open, or bulkhead full | Circuit breaker | 503 `RZ-UP-005` or `RZ-UP-006` | Closed | `circuitBreaker` |
| Timeout, or retry budget exhausted | Retries | Retry if allowed, else code selection | Closed | `timeout` fields; OQ-traffic-management-and-resilience-5 |
| Upstream fails, stale entry exists | `cache` | Serve stale within `stale-if-error`, unless a stale limit forbids it | Open | Upstream `Cache-Control` |
| Upstream fails after commit | Streaming | End the stream with `RZ-UP-009` | Closed | No |
| CEL error in `hashKey`, `retryOn` or `failureWhen` | Upstream | Random Endpoint; no retry; counted as failure | Open for `hashKey`, else Closed | No |
| CEL error in a `ratelimit` or `quota` `config.key` | `ratelimit`, `quota` | Admit unmetered | Open | `failureMode` (closed: 503 `RZ-RL-005`) |
| CEL error in a `cache` `config.key` | `cache` | Bypass the cache | Open | No (Configuration model) |

### Error codes this document owns

Pack 8.6 assigns `UP` and `RL` here ([body format](03-data-plane.md#error-response-format)). A leg ending without a response takes the first matching code:

1. The leg or Route deadline expired, or the final attempt hit `perTryTimeout`: 504 `RZ-UP-003`.
2. A retry ran and the last attempt ended in a connect, tls or reset error: 502 `RZ-UP-007`, naming that kind.
3. Otherwise the last error kind: `RZ-UP-001` connect, `RZ-UP-002` tls, `RZ-UP-004` reset.

| Code | Status | Meaning |
|---|---|---|
| RZ-UP-001 | 502 | Connect failed or dial timed out, no retry |
| RZ-UP-002 | 502 | TLS handshake or verification failed or timed out, no retry |
| RZ-UP-003 | 504 | Deadline expired, or the final attempt timed out |
| RZ-UP-004 | 502 | Reset or protocol error before a response, no retry |
| RZ-UP-005 | 503 | Circuit breaker open |
| RZ-UP-006 | 503 | In-flight ceiling and pending queue full |
| RZ-UP-007 | 502 | Retries ran and the last attempt got no response |
| RZ-UP-008 | 503 | Endpoint set empty |
| RZ-UP-009 | Not applicable | Upstream failed after commit; the stream ended with an error |
| RZ-UP-010 | 502 | Buffered plain `upstreams` response over its cap (area per OQ-data-plane-9) |
| RZ-RL-001 | 429 | Denied by a local token bucket |
| RZ-RL-002 | 429 | Denied by GCRA or the over-limit cache |
| RZ-RL-003 | 429 | Consumer quota exhausted |
| RZ-RL-004 | 403 | Consumer missing or lacks the named quota |
| RZ-RL-005 | 503 | `config.key` CEL error under `failureMode: closed` |

## Open questions

| ID | Question | Options | Owner | Blocking? |
|---|---|---|---|---|
| OQ-traffic-management-and-resilience-1 | Which `ratelimit` fields set the per-Node ceiling and burst, and enforce keys hotter than a shard? | (a) `config.perNodeCeiling`, `config.burst`; (b) a headroom multiplier; (c) a `localOnly` mode | traffic-management-and-resilience | Yes, per-Node ceiling (M1) |
| OQ-traffic-management-and-resilience-2 | How are RateLimit fields written on admitted responses? | (a) `onResponse` on both types; (b) 429 only; (c) Data plane appends them | configuration-model | Yes, header contract (M1) |
| OQ-traffic-management-and-resilience-3 | Should header output be selectable? | (a) `config.responseHeaders`; (b) `ietf` only; (c) also `X-RateLimit-*` | traffic-management-and-resilience | No |
| OQ-traffic-management-and-resilience-4 | Should one `ratelimit` hold a limit table per Tier? | (a) `config.limits[].when`; (b) a `tiers` map; (c) one Policy per Tier (current) | traffic-management-and-resilience | No |
| OQ-traffic-management-and-resilience-5 | Which fields configure backoff, the retry budget, hedging and breaker guards? | (a) `retries.backoff`, `budget`, `hedgeDelay`; `circuitBreaker.failureRatio`, `minimumLegs`, `halfOpenSuccesses`; (b) fixed, no hedging (current) | configuration-model | Yes, hedging (M4) |
| OQ-traffic-management-and-resilience-6 | Are the Deadlines defaults right, and are dial and handshake timeouts fields? | (a) As proposed; (b) no default retries; (c) per-protocol Route timeouts (recommended); any with (d) `Upstream.spec.connectTimeout`, `tls.handshakeTimeout` | configuration-model | Yes, canonical form (M1) |
| OQ-traffic-management-and-resilience-7 | Should the ejection cap and panic threshold be fields? | (a) `healthCheck.passive.maxEjectionPercent`, a panic percentage; (b) fixed (current) | traffic-management-and-resilience | No |
| OQ-traffic-management-and-resilience-8 | Should balancing prefer same-zone Endpoints and ramp new ones? | (a) `loadBalancing.locality`, `slowStart`; (b) slow start only; (c) neither | traffic-management-and-resilience | No |
| OQ-traffic-management-and-resilience-9 | DNS refresh from 1,000 Nodes over 200 Upstreams is about 13,300 queries per second (hypothesis): tunable or relayed? | (a) `discovery.refreshInterval`; (b) fixed (current); (c) Control Stream relay | configuration-model | No |
| OQ-traffic-management-and-resilience-10 | Do Quotas need calendar windows and weighted costs? | (a) `calendar: month`, `config.cost`; (b) cost only; (c) fixed windows (current) | configuration-model | No |
| OQ-traffic-management-and-resilience-11 | How is the Response Cache isolated from limit keys, must all unsafe methods invalidate, and does `s-maxage` forbid stale? | (a) A separate State Store; (b) a per-Policy entry cap; (c) interim rules as fields, TTL defaults, purge; any with `s-maxage` as `proxy-revalidate` (current) | configuration-model | Yes, Response Cache (M1) |
| OQ-traffic-management-and-resilience-12 | How is mirroring declared? | (a) `Route.spec.mirror`, Planned (M2); (b) a fire-and-forget step; (c) a Plugin | configuration-model | No |
| OQ-traffic-management-and-resilience-13 | How are redirects and plain `upstreams` rewrites declared? | (a) `Route.spec.redirect`, `upstreams[].pathRewrite`; (b) `plugin` and composition `path` (current) | configuration-model | Yes, built-in redirects (M2) |
| OQ-traffic-management-and-resilience-14 | Should shedding honor Tier priority? | (a) Tier-ordered; (b) adaptive concurrency; (c) OQ-data-plane-6 only | data-plane | No |
| OQ-traffic-management-and-resilience-15 | Which hash does `ring-hash` use? | (a) Standard library; (b) third party, after a Tech stack row | tech-stack-and-libraries | No |
| OQ-traffic-management-and-resilience-16 | Which ceiling derivation and fail-open refill replace pack 8.8's limit / N? | (a) limit / N; (b) 2× headroom, floor 10, step refill per aligned window (recommended); (c) declared multiplier only | scalability-and-distributed-state | Yes, pack 8.8 amendment (M1) |
| OQ-traffic-management-and-resilience-17 | How is a percentage-sticky split declared? | (a) A hash key on `upstreams`; (b) a CEL hash; (c) a cookie-setting Plugin | configuration-model | No |
| OQ-traffic-management-and-resilience-18 | Should keys aggregate IPv6 sources to a prefix? | (a) A CEL /64 function; (b) a `config` prefix length; (c) no (current) | traffic-management-and-resilience | No |
| OQ-traffic-management-and-resilience-19 | Which Node count should derived ceilings use? | (a) `clusterNodeCount` plus declared ceilings (current); (b) a ceiling count from each Node's first ACK, raised at once, lowered slowly | control-plane-and-gitops, with scalability-and-distributed-state | Yes, cross-document amendment (M1) |
| OQ-traffic-management-and-resilience-20 | May first-seen keys past the budget skip GCRA while the State Store is healthy? | (a) Local-only entries (proposed); (b) 429 `RZ-RL-001`; (c) wait for GCRA | scalability-and-distributed-state | Yes, pack 8.8 amendment (M1) |
| OQ-traffic-management-and-resilience-21 | How does `cache` coexist with `onRequestBody` authz, validation or Plugin auth? | (a) Reject with a new `RZ-CFG` code (proposed); (b) bypass lookups as a degraded state | configuration-model | Yes, Response Cache (M1) |
