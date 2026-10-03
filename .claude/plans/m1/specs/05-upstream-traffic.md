# M1 spec, area 5: Upstream layer, traffic management, rate limiting, quota, Response Cache, composition

Design reader output for milestone M1 "Core gateway". Normative sources (cited by short name):
TMR = `docs/architecture/09-traffic-management-and-resilience.md`; ADR8 = `docs/adr/0008-rate-limiting-local-bucket-and-gcra.md`;
SDS = `docs/architecture/11-scalability-and-distributed-state.md`; FP = `docs/_meta/foundation-pack.md`; DP = `docs/architecture/03-data-plane.md`;
CM = `docs/architecture/02-configuration-model.md`; OBS = `docs/architecture/10-observability.md`; SEC = `docs/architecture/08-security-and-identity.md`;
MP = `docs/architecture/07-multi-protocol.md`; ADR9 = `docs/adr/0009-http-stack-net-http-quic-go.md`; PBB = `docs/architecture/12-performance-budgets-and-benchmarking.md`;
TQ = `docs/engineering/03-testing-and-quality-strategy.md`; RL = `docs/engineering/02-repository-layout-and-conventions.md`; TS = `docs/engineering/01-tech-stack-and-libraries.md`;
RVC = `docs/engineering/04-release-versioning-and-compatibility.md`; RM = `docs/roadmap/01-roadmap-and-milestones.md`; FC = `docs/features/01-feature-catalog.md`;
ZDT = `docs/operations/02-zero-downtime-upgrades-and-hot-reload.md`. Sibling specs: area 2 = `02-config-revision.md`, area 3 = `03-cel.md`, area 4 = `04-dataplane-core.md`.
"(target)"/"(hypothesis)" tags are copied from the docs; "(proposed)" marks a value this spec picks where the docs are silent (section 9 lists each).

## 1. Scope

| # | M1 item (RM "M1 scope" row) | Source sections |
|---|---|---|
| S1 | Upstream layer for `protocol: http` Upstreams, own attempt loop on `net/http` `Transport` (Ruralz Gateway row) | DP "Upstream layer"; ADR9 "Decision outcome" (Upstream layer row); MP "Streams and chunks" |
| S2 | Service discovery: static `endpoints` and `discovery.type: dns` for `http` Upstreams (Traffic row) | TMR "Service discovery"; FC "DNS service discovery" |
| S3 | Weighted splits, blue-green and header canaries (Traffic row) | TMR "Traffic shaping"; FC "Weighted splits, blue-green and header canaries" |
| S4 | Load balancing `round-robin`, `least-request`, `ring-hash` (CEL `hashKey`), `random`; balancer memory budget | TMR "Load balancing"; FC "Load balancing" |
| S5 | Health checks: passive ejection (default on), active probes, panic mode, stall/suspect detection | TMR "Health checking and outlier detection"; FC "Health checks and outlier detection" |
| S6 | Deadlines: Route, leg, attempt, dial, TLS; retries with CEL `retryOn`, backoff, retry budget | TMR "Deadlines", "Retries", "Retry budget"; FC "Granular timeouts", "Retries with a retry budget" |
| S7 | Circuit breaker with CEL `failureWhen`; bulkhead (`maxConnections`, `maxPendingRequests`) | TMR "Circuit breakers"; FC "Circuit breakers and bulkheads", "Upstream protection limits" |
| S8 | TLS and mTLS to Upstreams (Security row, TB-3); Node-connection restrictions | SEC "Transport security", "Secrets" rule 6; FC "Upstream mTLS" |
| S9 | `RZ-UP` registry and code selection; `RZ-RL` registry | TMR "Error codes this document owns"; FP 8.6 |
| S10 | Composition `aggregate`, `sequential`, `conditional`: steps, merge rules, partial responses, linear workflows, URL rewrite by step `path`/`pathExpression` (Traffic row "composition") | DP "Composition engine", "Merge rules", "Failures and partial responses", "Workflow composition"; CM "Route"; FC rows 134-137, 186-187, 189 |
| S11 | Catch-all fallback, header and query string routing, wildcard routes, virtual hosts (Traffic row); matching itself is area 4's Router | TMR "Traffic shaping"; DP "Router", "Precedence"; FC rows 183-188 |
| S12 | `ratelimit`: local token bucket and GCRA (ADR-0008), distributed limits with State Store counters, per-Node ceiling, over-limit cache, first-seen budget, `localOnly`, `failureMode` (Traffic row) | ADR8; TMR "Rate limiting", "Per-Node ceiling", "Service and tiered limits"; SDS "Distributed rate limits"; FP 8.8 |
| S13 | RateLimit response headers | TMR "RateLimit response headers"; FC "RateLimit response headers" |
| S14 | `quota` in `requests`; API governance with `overridable: false` Gateway Policies | TMR "Quotas"; SDS "Consistency and accuracy bounds"; CM "Policy" |
| S15 | `cache` (Response Cache) | TMR "Response caching"; SDS "Distributed caches", "Post-commit write queue"; FC "Response Cache" |
| S16 | `memory` and `redis` driver behavior of these Policies' State Store operations (scripts, keys, round-trip rules) | FP 8.7; SDS "State client and RZ-STS error codes", "State Store availability" |
| S17 | Admin `/debug/upstreams` (Ruralz Gateway row) | DP "Admin endpoints" |
| S18 | CEL places `Upstream.spec.loadBalancing.hashKey`, `retries.retryOn`, `circuitBreaker.failureWhen`, `composition.steps[].pathExpression`, `steps[].when`, `ratelimit`/`quota`/`cache` `config.key` (CEL row; compile is area 3) | CM "Allowed places"; RM CEL row |
| S19 | Metrics, spans and degraded reasons of these subsystems (Observability row) | OBS "Ruralz Gateway metrics", "Degraded states", "Span model", "Access logs" |
| S20 | Quality and budgets for this area: Rate Limit property, round-trip test, State Store integration tests, CE-3 to CE-6, CE-12, CE-15, CE-16, "Upstream Endpoints reset or slowed" chaos row; S1/S2 Upstream-layer and `ratelimit` stage budgets, S5, S5x, O1, O2, G1 | RM Quality/Budgets rows; TQ "Required properties", "Integration tests", "Chaos testing"; ADR8 "Confirmation"; SDS "Chaos experiments"; PBB "Per-stage latency budget", "Scenarios" |

## 2. Normative requirements

### A. Upstream model, compile and runtime state

1. An M1 Node serves only `protocol: http` Upstreams and `discovery.type: dns` or static `endpoints`. A Revision containing another protocol or `discovery.type: kubernetes` MUST be refused at activation (keep the active Revision; code: section 9 item 23). [CM "Upstream"; MP "One listener, dispatch after the match"]
2. Compiled Upstream settings live in the snapshot; mutable runtime state (Endpoint set, health, ejections, breaker, retry budget, bulkhead counters, balancer structures, connection pools, discovery loop) lives in a Node-lifetime runtime keyed by Upstream `metadata.name`, carried across Hot Reloads: "Health, ejection and breaker state survive Hot Reloads"; per-Endpoint state is kept for Endpoints whose identity remains. New thresholds apply from the new snapshot on; in-flight attempts keep their own snapshot's settings. A runtime no snapshot references is closed at zero pins (pools closed, discovery stopped). [TMR "Load balancing"; DP "Configuration snapshots and hot reload", "Activation" step 3]
3. Connection pools are keyed by (protocol, TLS settings digest including resolved CA/certificate material digests, Endpoint identity) and carried across Hot Reloads when the key is unchanged; a changed key gets a new pool and the old one closes idle connections at zero pins. [DP "Upstream layer"]
4. Runtime defaults. When a field (or its parent object) is absent the runtime uses these values; the static ones are also `+ruralz:default` schema markers per OQ-traffic-management-and-resilience-6 (section 7), materialized only inside present parents (area 2), so the runtime MUST apply identical values when the parent is absent:

   | Field | Default |
   |---|---|
   | `Route.spec.timeout` | 15 s for every M1 Route (1 h for stream Routes is M3) (target); runtime rule, not materialized |
   | `Upstream.spec.timeout` | the Route's `timeout` (target); runtime rule |
   | `retries.perTryTimeout` | leg time left ÷ (retries left + 1), computed at each attempt start (target); runtime rule |
   | `retries.attempts` | 1 (one retry) |
   | `retries.retryOn` | GET, HEAD, OPTIONS, PUT, DELETE: `connect` or `reset` errors, or status 503; other methods: `connect` errors. CEL form (area 3 `DefaultRetryOn`): `error != null ? (error.kind == "connect" \|\| (error.kind == "reset" && request.method in ["GET","HEAD","OPTIONS","PUT","DELETE"])) : (response.status == 503 && request.method in ["GET","HEAD","OPTIONS","PUT","DELETE"])` |
   | `circuitBreaker.failureWhen` | `error != null \|\| response.status in [502, 503, 504]` |
   | `circuitBreaker.maxConnections` / `maxPendingRequests` / `consecutiveFailures` / `openDuration` | 1,024 / 256 / 5 / 30 s (target) |
   | `circuitBreaker.minimumLegs` / `failureRatio` / `halfOpenSuccesses` (fields per OQ-traffic-management-and-resilience-5 (a)) | 20 legs / 0.5 / 3 (target) |
   | `healthCheck.passive.consecutiveErrors` / `ejectionTime` | 5 / 30 s (target); passive ejection is on even without `healthCheck` |
   | `healthCheck.active` fields when `active` is present | `path` `/`, `interval` 10 s, `timeout` 2 s, `healthyThreshold` 2, `unhealthyThreshold` 3 (proposed; section 9 item 9) |
   | `endpoints[].weight` | 1 (proposed) |
   | `loadBalancing.algorithm` | `least-request` ("proposed default") |
   | Fixed (not fields, OQ-6 (d) not adopted): dial 1 s, TLS handshake 2 s, stall 1 s, HTTP/2 ping after 1 s silence, ping timeout 5 s, backoff 25-250 ms, retry budget max(3, 20%), max `Retry-After` 10 s, window 10 s, open jitter ±20% | (target) |

   Unset `retryOn`/`failureWhen` MAY run as native Go predicates if a table test proves them equal to the CEL forms above over {GET, HEAD, OPTIONS, PUT, DELETE, POST, PATCH} × {connect, reset, timeout, tls, none} × {200, 429, 500, 502, 503, 504}. [TMR "Deadlines", "Circuit breakers"; area 3 req 46]

### B. Endpoint sources and discovery

5. Static `endpoints[].address` is `host:port`. An IP literal is one Endpoint. A host name is one Endpoint (identity = the configured address) whose IP list the Node resolves at compile time and every 30 s (target), keeping the last good answer; a dial tries the cached IPs in answer order within the single 1 s dial budget. [TMR "Service discovery"]
6. `discovery.type: dns` requires `service` and `port`. Numeric `port`: A and AAAA lookups of `service`, each IP an Endpoint `ip:port` of weight 1. Named `port`: SRV `_<port>._tcp.<service>`; each target is an Endpoint `target:port` (identity without trailing dot) with the SRV weight (SRV weight 0 becomes 1 when all targets in the group are 0, else it is excluded; only the lowest-priority group is used, proposed) resolved to IPs as in req 5. Refresh every 30 s with ±10% jitter (target), using the pure-Go `net.Resolver` (`CGO_ENABLED=0`). [TMR "Service discovery"; TS "Service discovery" row]
7. Lookup failure keeps the last set, raises `ruralz_upstream_degraded_info{upstream,reason="discovery_stale"}` and `ruralz_node_degraded_info{reason="discovery_stale"}`, and retries with full-jitter backoff from 1 s to 60 s (target). NXDOMAIN or an empty answer is a failure until 3 consecutive refreshes repeat it (target); then the set empties and selection returns 503 `RZ-UP-008`. A successful refresh clears `discovery_stale`. [TMR "Service discovery"; failure matrix row "Discovery source down"]
8. Endpoint sets are swapped copy-on-write (`atomic.Pointer`), never by a new Revision. Endpoints are sorted by identity before any balancer build so every Node with the same answer builds identical structures. [DP "Upstream layer"; TMR "Load balancing"]
9. Every Node connection to an Endpoint (requests, probes) refuses 0.0.0.0/8, ::/128, 127.0.0.0/8, ::1, 169.254.0.0/16, fe80::/10 and fd00:ec2::254, IPv4-mapped and NAT64 forms included, at connect time (checked on the actual socket address in `net.Dialer.Control`), unless `RURALZ_FETCH_ALLOW` lists them; such a refusal is a `connect` error. Proxy environment variables are ignored (`Transport.Proxy = nil`). Upstream 3xx responses pass through; the Upstream layer never follows redirects. [SEC "Secrets" rule 6, OQ-security-and-identity-22 (a); TMR "Traffic shaping"]

### C. Weighted splits and Endpoint selection

10. Plain `upstreams`: in `onRoute`, pick one entry per request by weighted random over entries with `weight` > 0 (`weight` default 1); the pick is final for the request (no cross-entry failover). Stickiness exists only through a higher-ranked Route matching a client header or cookie (`match.headers`, `match.when`). If every weight is 0 the request gets 503 `RZ-UP-008` (proposed; section 9 item 24). onRoute budget: 1 µs p50, 4 µs p99, 0 allocations (target). [TMR "Load balancing", "Traffic shaping"; PBB "Per-stage latency budget"]
11. Per attempt, in this order (TMR Figure 1): (a) Endpoint set empty → 503 `RZ-UP-008`; (b) drop passively ejected and actively unhealthy Endpoints; if none remains use every Endpoint (panic mode: `ruralz_upstream_degraded_info{reason="panic"}` and `ruralz_node_degraded_info{reason="upstream_panic"}` while it lasts; all-or-nothing per OQ-data-plane-7 (a)); (c) exclude Endpoints already tried in this leg, suspect Endpoints (req 21) and capped Endpoints (req 36) while another remains; (d) run `loadBalancing.algorithm`, weighted by `endpoints[].weight` or SRV weights; (e) gate: breaker closed, or this attempt is the half-open probe, and an in-flight slot or pending room (reqs 36-37), else 503 `RZ-UP-005` or `RZ-UP-006`; (f) attempt on a pooled connection, or dial within 1 s and TLS within 2 s (target). [TMR "Load balancing"]
12. `round-robin`: a precomputed smooth weighted schedule of L = min(65,536, 64 × E) 4-byte slots (uint32 Endpoint index), weights normalized to n_i = max(1, round(w_i × L / Σw)) and interleaved by earliest-deadline-first over n_i (heap, O(L log E)); pick = atomic cursor increment modulo the schedule length, cursor start randomized per build. A pick skips excluded slots for at most one pass, then ignores exclusions (panic for the pick). 3 Endpoints use 768 bytes; at most 256 KiB (target). [TMR "Load balancing"]
13. `least-request`: two distinct candidates by weighted random; the one with the lower (in-flight attempts + attempts matching `failureWhen` in the last 1 s) ÷ weight wins; ties pick either at random. In-flight counts the whole attempt including response body streaming. [TMR "Load balancing"]
14. `ring-hash`: `hashKey` (CEL, string, Base variables) is evaluated once per leg and reused by retries; the ring holds min(65,536, v × E) 16-byte virtual nodes (uint64 position, uint32 Endpoint index, padding), Endpoint i owning max(1, round(v × E × w_i/Σw)) of them at positions FNV-1a-64(identity + "#" + j) passed through the splitmix64 finalizer (standard library only, OQ-traffic-management-and-resilience-15 (a)); the key hashes with the same function; lookup is the first position ≥ key (wrapping), walking forward past excluded Endpoints for at most one full ring. A `hashKey` runtime error picks a weighted random Endpoint and counts `ruralz_upstream_cel_errors_total{upstream,field="hashKey"}`. `ring-hash` without `hashKey` is a validation error (req 97). [TMR "Load balancing", failure matrix CEL row; CM "Upstream"]
15. `random`: weighted pick by binary search over cumulative weights with a uniform 64-bit random (`math/rand/v2`); exclusions by rejection up to 8 draws, then a linear scan of eligible Endpoints. [TMR "Load balancing"]
16. Balancer structures rebuild off the request path at most every 10 s per Upstream, at most 8 builds at a time per Node, swapped copy-on-write (target). Endpoints removed from the set count as excluded until the rebuild; health changes never trigger a rebuild. [TMR "Load balancing"]
17. Balancer budget 256 MiB per Node (target), planned when a Revision compiles: S = bytes of every `round-robin` schedule; R = Endpoints summed over `ring-hash` Upstreams; v = clamp(floor((256 MiB − S − 16 MiB) ÷ (16 B × R)), 64, 1,024). If v = 64 does not fit, rings fall back to weighted `random`, largest ring first, until it fits, raising `ruralz_upstream_degraded_info{reason="balancer_budget"}` and `ruralz_node_degraded_info{reason="balancer_budget"}`. Endpoint-set changes lower v at once on overflow and raise it only when it can double (target). For `dns` Upstreams the plan uses current runtime set sizes. [TMR "Load balancing"; SDS "Per-Node limits"]

### D. Health checking and outlier detection

18. Passive ejection (always on): `consecutiveErrors` consecutive attempts to one Endpoint matching `failureWhen` eject it for `ejectionTime` × its ejection count, the count capped at 10 (target); the count decrements by one per `ejectionTime` spent not ejected (proposed; section 9 item 10). Emit `ruralz_upstream_ejections_total{upstream,reason="passive"}`. [TMR "Health checking and outlier detection"]
19. Ejection cap: at most floor(50% × E) Endpoints passively ejected at once (target); an ejection that would exceed it is skipped, except when the triggering error is `connect` (dial errors, dial timeouts, HTTP/2 ping closes) or an attempt timeout on a suspect Endpoint, which bypass the cap; active removals also bypass it. [TMR "Health checking and outlier detection"]
20. Active checks run only when `healthCheck.active` is set: `GET <path>` to each Endpoint every `interval` ±10% jitter (target) with the Upstream's TLS settings; status 200-399 within `timeout` succeeds; `healthyThreshold` consecutive successes mark healthy, `unhealthyThreshold` consecutive failures unhealthy (`ruralz_upstream_ejections_total{reason="active"}`); new Endpoints start healthy. Probes take no bulkhead slot. Probe slots per Node = min(256, ceil(Σ probes per second × `timeout`)) (target), shared across Upstreams; a probe that finds no slot is skipped and counted in `ruralz_upstream_probes_skipped_total{upstream}`; more than 10% skipped over 1 minute raises `ruralz_node_degraded_info{reason="probes_skipped"}` (target). [TMR "Health checking and outlier detection"; OBS]
21. Stall and suspect: an attempt without response headers 1 s after it started is stalled (target); an Endpoint holding a stalled attempt that has delivered no response headers for 1 s is suspect until any attempt to it gets headers. Selection skips suspect Endpoints while another remains; an attempt timeout on a suspect Endpoint ejects it at once (cap bypass). Once stalled attempts hold 50% of `maxConnections`, selection skips every Endpoint holding one (target). [TMR "Health checking and outlier detection", "Circuit breakers"]
22. HTTP/2 upstream connections set `HTTP2Config.SendPingTimeout` = 1 s and `PingTimeout` = 5 s (target), so a silent connection is pinged after 1 s and closed after 5 s unanswered, resetting its streams (kind `reset` for the attempts; counted as `connect` for ejection cap purposes, req 19). [TMR "Health checking and outlier detection"; ADR9 "Upstream layer"]
23. `ruralz_upstream_healthy_endpoints{upstream}` = Endpoints eligible after step (b) of req 11. [OBS]

### E. Deadlines

24. Nested bounds, each clamping those below: Route `timeout` (request, streamed body included; context set by area 4 at `t0 + timeout`) ⊇ leg deadline = min(Route deadline, leg start + `Upstream.spec.timeout`) (attempts and backoff up to the final response headers) ⊇ attempt deadline = min(leg deadline, attempt start + `perTryTimeout`) (to the last response header byte) ⊇ dial 1 s and TLS handshake 2 s (target). Only attempt, leg or Route deadlines yield error kind `timeout`; dial expiry is `connect`, handshake expiry `tls`. [TMR "Deadlines"]
25. The attempt and leg deadlines stop applying once response headers arrive (the timer that cancels the attempt is stopped); after commit only the Route `timeout` (and area 4's write deadlines) bound the body. [TMR "Deadlines"]
26. Expiry mapping: Route `timeout` before any attempt is area 4's 504 `RZ-RT-007`; during a leg 504 `RZ-UP-003`; after commit the stream ends with `RZ-UP-009` (HTTP/2 `RST_STREAM` / HTTP/1.1 connection close via `http.ErrAbortHandler`), recorded in `onLog`. [TMR "Deadlines"; DP "Failures and partial responses"]

### F. Attempt execution and forwarding

27. The Upstream layer uses its own loop over `http.Transport` (never `httputil.ReverseProxy`, never the `x/net/http2` `Transport`). Transport per pool key: `Protocols` = HTTP/1 only for cleartext; HTTP/1 and HTTP/2 (ALPN `h2`, `http/1.1`) over TLS; no h2c or HTTP/3 (OQ-multi-protocol-12 (b)); `DialContext` = a `net.Dialer{Timeout: 1s, Control: req 9}`; `TLSHandshakeTimeout` 2 s; `Proxy` nil; `MaxConnsPerHost` 0 (bulkhead bounds it); `MaxIdleConnsPerHost` = the Endpoint's in-flight cap (req 36); `IdleConnTimeout` 90 s (proposed); `DisableCompression` true (bodies pass unchanged); `HTTP2` = `&http.HTTP2Config{StrictMaxConcurrentRequests: false, SendPingTimeout: 1s, PingTimeout: 5s}`, so a server stream limit opens another connection within `maxConnections`. [DP "Upstream layer"; ADR9; MP "Streams and chunks"]
28. Outgoing request per attempt: method (plain `upstreams`: the client's; step: req 54); path = the one normalized path in escaped form (SEC "Path confusion"); raw query unchanged; headers = the client request headers as the request Phases left them, minus hop-by-hop fields (RFC 9110 §7.6.1: `Connection` and fields it names, `Keep-Alive`, `Proxy-Connection`, `TE` except `trailers`, `Trailer`, `Transfer-Encoding`, `Upgrade`, `Proxy-Authorization`, `Proxy-Authenticate`) and `Expect`, plus area 4's forwarding headers (area 4 req 18) and `traceparent`/`tracestate` from the telemetry area; `Host` = the Endpoint authority (req 29). Each attempt then runs `onUpstreamRequest` (upstream-leg Policies), whose short-circuit ends the leg with the generated response and no retry. [DP "Upstream layer", "Short-circuit"; SEC "Request hardening"; OBS "Propagation"]
29. `Host` and SNI (proposed; section 9 item 5): `Host` = `tls.sni` when set, else the configured Endpoint host (static host name, SRV target, or `discovery.service` for A/AAAA), else the IP, with `:port` when not the scheme default; TLS `ServerName` = `tls.sni`, else the same host name; an IP-only Endpoint without `sni` verifies the IP SAN.
30. Response handling: hop-by-hop fields removed; status, headers, body and trailers pass through unchanged when no Filter changes them; `onUpstreamResponseHeaders` runs per attempt (reverse order), then the retry decision (reqs 32-33); the body streams through area 4's 32 KiB buffer unless a gate buffers it. A buffered plain `upstreams` response over `limits.maxResponseBodyBytes` (default 10 MiB) is 502 `RZ-UP-010` before commit. The bulkhead slot and Endpoint in-flight count are held until the response body is closed (EOF, error or client gone). 1xx interim responses are not forwarded (proposed). Upgrade requests to an `http` Upstream are rejected by area 4 (MP, OQ-multi-protocol-16). [DP "Streaming"; TMR "Circuit breakers"; MP "One listener"]
31. Request bodies: a leg is replayable when its body is empty, or buffered within `limits.maxRequestBodyBytes` (gate), or streamed and not a single byte has been read from it yet (proposed; section 9 item 26). The layer never buffers a body only to enable retries. `GetBody` is set only for buffered bodies. [SO "Replay rule"; TMR "Retries"]

### G. Retries and retry budget

32. `retries.attempts` counts retries (`attempts: 2` allows three attempts). After each attempt: evaluate `failureWhen` (passive ejection, least-request failures, suspect bookkeeping), run `onUpstreamResponseHeaders` if a response arrived, then evaluate `retryOn` over `request` (no body), `response` (status and headers; null without a response), `error` (`kind` in `connect`, `timeout`, `reset`, `tls`; null when a response arrived), `attempt` (1-based), `upstream` (`name`, `endpoint`). A `retryOn` runtime error means no retry (`ruralz_upstream_cel_errors_total{field="retryOn"}`); a `failureWhen` runtime error counts as failure (`field="failureWhen"`). An `onUpstreamResponseHeaders` Filter MAY also request a retry, subject to req 33. [TMR "Retries", failure matrix; CM "Allowed places", "Variables"; DP "Phases"]
33. A retry needs `retryOn` true (or a Filter request) and all of: (1) replayable body and nothing committed; (2) attempts left and leg deadline left after the backoff; (3) breaker not open and retry-budget room; (4) any Upstream `Retry-After` (delta-seconds or HTTP-date) at most 10 s (target) and ending before the leg deadline. Backoff: full jitter, sleep = uniform(0, min(250 ms, 25 ms × 2^(n−1))) for retry n (target), or the `Retry-After` delay when longer. The retry prefers an untried Endpoint (req 11 c) and reruns `onUpstreamRequest`; a retried response's body is drained up to 64 KiB (proposed) and closed. `ruralz_upstream_retries_total{upstream}` counts retries made. [TMR "Retries"]
34. Retry budget per Upstream per Node: in-flight retries (from backoff start to leg end) MUST NOT exceed max(3, floor(20% × in-flight originals)) (target). Over budget the leg returns the attempt result it has (the "original failure"), counted in `ruralz_upstream_retry_budget_exhausted_total{upstream}`. From 15 originals in flight amplification is at most 1.2× (target). [TMR "Retry budget"]
35. A leg whose last attempt got a response returns it unchanged. A retry attempt that cannot start (breaker open, bulkhead full, no Endpoint) ends the leg with the previous attempt's outcome (proposed; section 9 item 25). Hedging is not built (M4). [TMR "Retries", "Hedging"]

### H. Circuit breaker and bulkhead

36. Bulkhead per Upstream per Node: `maxConnections` bounds in-flight attempts (a semaphore taken before `RoundTrip`, so HTTP/2 streams count), released when the attempt's response body closes; waiters are at most `maxPendingRequests`, each bounded by its attempt context (FIFO, proposed); a full waiter queue gives 503 `RZ-UP-006` at once (with `maxPendingRequests: 0`, at once when slots are full). Per-Endpoint cap: max(8, floor(min(2 × w_i/Σw, 0.5) × `maxConnections`)) in-flight attempts (target); a capped Endpoint is excluded while another remains. [TMR "Circuit breakers"; ADR9]
37. Breaker per Upstream per Node, counting legs by their final result against `failureWhen`; legs ended by `RZ-UP-005`, `RZ-UP-006` or `RZ-UP-008` gates are not counted (proposed); a leg whose final error kind is `connect` counts only while more than 50% of Endpoints are ejected (target). Closed → open when `consecutiveFailures` consecutive counted legs failed AND at least `failureRatio` (0.5) of at least `minimumLegs` (20) legs in a rolling 10 s window (ten 1 s buckets) failed (target). Open: every new leg gets 503 `RZ-UP-005` at once, no retry. After `openDuration` jittered ±20% (target) → half-open: exactly one probe leg at a time (a real request), others 503 `RZ-UP-005`; `halfOpenSuccesses` (3) consecutive probe successes close it, a probe failure reopens it (target). Below `minimumLegs` legs per window the breaker never opens. Gauge `ruralz_upstream_breaker_state_info{upstream,state="closed"|"open"|"half_open"}` (exactly one series at 1). [TMR "Circuit breakers", Figure 2]
38. Streaming legs hold their slot for life up to the Route `timeout` (no M1 stream protocols; the rule holds for long HTTP bodies). [TMR "Circuit breakers"]

### I. Error classification and code selection

39. Error kinds from Go errors: `connect` = dial errors (refused, unreachable, dial timeout, req 9 refusal, `*net.DNSError` of a static host); `tls` = handshake, certificate verification, alert or handshake timeout errors; `reset` = connection reset, EOF or unexpected EOF before response headers, HTTP/2 stream reset or connection loss (ping timeout), protocol errors; `timeout` = expiry of the attempt, leg or Route context. Classification MUST be table-tested against real sockets (section 6). [TMR "Deadlines"; CM "Variables"]
40. Leg ending without a response takes the first matching code: (1) leg or Route deadline expired, or the final attempt hit `perTryTimeout`: 504 `RZ-UP-003`; (2) a retry ran and the last attempt ended in `connect`, `tls` or `reset`: 502 `RZ-UP-007`, naming that kind; (3) otherwise the last kind: 502 `RZ-UP-001` connect, 502 `RZ-UP-002` tls, 502 `RZ-UP-004` reset. Gates: 503 `RZ-UP-005` breaker, 503 `RZ-UP-006` bulkhead, 503 `RZ-UP-008` empty set. After commit: `RZ-UP-009` (stream ended, no status). Buffered response over cap: 502 `RZ-UP-010`. Problem bodies follow area 4 (no `detail` member; the kind for `RZ-UP-007` goes to the span, log and access log, proposed). [TMR "Error codes this document owns"; DP "Error response format"]

    | Code | Status | Meaning |
    |---|---|---|
    | RZ-UP-001 | 502 | Connect failed or dial timed out, no retry |
    | RZ-UP-002 | 502 | TLS handshake or verification failed or timed out, no retry |
    | RZ-UP-003 | 504 | Deadline expired, or the final attempt timed out |
    | RZ-UP-004 | 502 | Reset or protocol error before a response, no retry |
    | RZ-UP-005 | 503 | Circuit breaker open, or half-open with its probe in flight |
    | RZ-UP-006 | 503 | In-flight ceiling and pending queue full |
    | RZ-UP-007 | 502 | Retries ran and the last attempt got no response |
    | RZ-UP-008 | 503 | Endpoint set empty |
    | RZ-UP-009 | n/a | Upstream failed after commit; the stream ended with an error |
    | RZ-UP-010 | 502 | Buffered plain `upstreams` response over its cap |

### J. Upstream TLS

41. `Upstream.spec.tls` present → TLS 1.2 or newer (`MinVersion` TLS 1.2); absent → cleartext, reported as `ruralz_security_cleartext_hops{hop="upstream"}` (count of cleartext Upstreams in the active snapshot) and `cleartext_hop` (not alerted for `upstream`). Server certificates are always verified: `caCertificate` (PEM bundle via `secretRef`) replaces system roots when set; no field disables verification. `clientCertificate` and `clientKey` (both or neither) enable mTLS through `GetClientCertificate` reading the current resolved secret, so rotation reaches new handshakes; CA rotation changes the pool key (req 3). [SEC "Transport security"; OBS "Degraded states"; FC "Upstream mTLS"]

### K. Composition

42. A Route with `composition` has 1 to `limits.maxCompositionSteps` steps (default 16; more is RZ-CFG-031). Each step is an upstream leg of `steps[].upstream` with its own upstream-leg Policies, retries, breaker, bulkhead and `ruralz.upstream.<name>` spans (attribute: step name), all under the Route `timeout`. [DP "Composition engine"; CM "Route"]
43. `aggregate`: the Node takes one in-flight unit per parallel step before starting any (area 4 `admission.Units.TryAcquire(k)`; failure 503 `RZ-RT-005`), runs every step in parallel, one goroutine each sharing a cancel context, and merges all bodies into one JSON object (req 47). `when` on an `aggregate` step is evaluated too and false skips the step (proposed; section 9 item 31). [DP "Composition engine"]
44. `sequential`: steps in list order; a step whose `when` is false is skipped; later steps read completed steps through CEL `steps` (`status`, `headers`, `body`); the response is the last executed step's response, streamed unless that step sets `target`, `select`, `rename` or `group` (then merged per req 47 alone). [DP "Composition engine"; CM "Allowed places"]
45. `conditional`: the first step whose `when` is true (absent `when` = true) runs; none true: 404 `RZ-RT-010`; the response is that step's, streamed unless transformed. [DP "Composition engine"]
46. `pathExpression` (string) and `when` (bool) are evaluated before their step with Base variables and `steps`; a runtime error fails the step (`optional` decides). [CM "Allowed places"]
47. Merge (every `aggregate` step, and any step with `target`, `select`, `rename` or `group`): the body MUST be JSON (`application/json` or `+json`); order: `target` (unwrap the nested object at that dot path), `collection: true` accepts a top-level JSON array (then `select`/`rename` apply per element and `group` is required, proposed), `select` (allowlist of top-level fields), `rename` (old → new), then merge under `group` or at the top level; on a key written by two steps the later in list order wins. Merged responses are `application/json`, status 200, with `Content-Length` recomputed. Decoded values are charged to `limits.maxBufferedBytes` at the size built, at most 4× the raw step limit (target). A Route-scoped `transform.response` runs on the merged body. [DP "Merge rules"; CM "Body buffering and limits"]
48. Step bodies are gates when merged or read later through `steps` (a compile-time scan of `steps.<name>.body` references in later `pathExpression`/`when`, via area 3 `Refs`); each step body is capped at `maxBodyBytes`, default an equal share of what `limits.maxResponseBodyBytes` leaves after explicit values (RZ-CFG-032 when explicit values break it). [CM "Body buffering and limits"]
49. Failures: a step fails on an Upstream error, a non-2xx result (req 50 exception), a timeout, a CEL runtime error or a body over `maxBodyBytes`. A failed non-optional step cancels its siblings through the shared context and fails the request: the leg's `RZ-UP-<NNN>` code for an Upstream cause (status per that code's registry row, proposed; section 9 item 3), 502 `RZ-RT-015` for a CEL error or oversized body, 503 `RZ-RT-004` when the buffer budget is spent. A failed `optional: true` step is left out and the response carries `ruralz-partial: true`. An `onUpstreamRequest` short-circuit records the generated response as the step's result (non-2xx fails a non-optional step). [DP "Failures and partial responses", "Short-circuit"]
50. Non-2xx pass-through (proposed; section 9 item 2): in `sequential` (the last executed step) and `conditional`, a step that produces the response unmerged passes a non-2xx Upstream response through unchanged instead of failing; non-2xx fails aggregate steps, merged steps and non-final sequential steps with 502 and the proposed code `RZ-UP-011` "Composition step got a non-2xx response".
51. Linear workflows are `sequential` with per-step `when`; declared dependency graphs are not built (OQ-data-plane-3 (a)). [DP "Workflow composition"]

### L. Routing features (behavior this area verifies; matching is area 4)

52. Catch-all fallback: a Route whose only criterion is `match.when: "true"` ranks last by precedence (host tier none, path tier none, one constraint, name order). Header canaries: a higher-ranked Route with `match.headers` (`exact`, `regex`, `present`) or `match.when` over `request.headers`/cookies. Query routing: `match.when` over `request.query` (map(string,string), repeated parameter joined by `","`, area 3 req 20) or a `conditional` composition. JWT claim routing: `conditional` step `when` over `auth.claims`. Wildcard routes: `match.path` `prefix`, `template` or `regex`; wildcard hosts: one leading `*.` label (OQ-data-plane-2 (a)); virtual hosts: `match.hosts` with listener `hostnames`. URL rewrite: a single-step `conditional` (or `sequential`) with `path` or `pathExpression`; rewrites on plain `upstreams` wait for OQ-traffic-management-and-resilience-13. [TMR "Traffic shaping"; DP "Router", "Precedence"; FC rows 183-190]
53. A spike arrest is a short-`window` `ratelimit`; upstream protection is a constant-key `ratelimit` on each Route reaching the Upstream plus `circuitBreaker.maxPendingRequests`. [TMR "Traffic shaping"; FC "Upstream protection limits"]
54. Step request (proposed; section 9 item 6): method = `steps[].method` or the client method; path = `path` or the `pathExpression` result; if that string contains `?` its query is used as-is, else the client's raw query is appended; headers and body as for plain `upstreams`; when more than one step would send a non-empty request body, the body is gated (buffered once, `limits.maxRequestBodyBytes`) and replayed to each.
55. `steps` CEL values for completed steps: `status`, `headers` (lowercased, joined by `", "`), `body` (JSON `dyn` when gated, else null). [CM "Variables"; area 3 req 17]

### M. Rate limiting (`ratelimit`, ADR-0008)

56. Registry: Filter class admission, Phase `onRequestHeaders`, scopes G and R, slot `name`, `failureMode` default `open` (either allowed). Route limits stack on Gateway limits (FP 8.12); authentication runs first (class order). [FP 10; TMR "Rate limiting"]
57. `config.key` (CEL string, Base variables; unset = the constant `""`, a service limit, proposed) partitions traffic; `config.limits[]` (atomic list, ≥1): `requests` (≥1), `window` (Duration), `perNodeCeiling` (1..`requests`, unset = derived), `burst` (0..`requests`, default `requests`); `config.localOnly` (default false). Out-of-range values are RZ-CFG-005. [CM "Registered from feature documents"; TMR "Per-Node ceiling"]
58. Decision path, per request, in order (TMR "Decision path"; ADR8 Figure 1):
    1. `Policy.spec.when` false → skipped (area 4 evaluates). `config.key` runtime error → `failureMode`: `open` admits unmetered (`ruralz_ratelimit_decisions_total{result="fail_open"}`, `ruralz_filter_failures_total{mode="open"}`), `closed` 503 `RZ-RL-005`; no bucket is touched.
    2. Over-limit cache: a key whose last GCRA answer denied stays denied until its retry-after: 429 `RZ-RL-002` with `Retry-After`, zero State Store commands.
    3. First-seen: a key with no local entry spends one unit of the Node-wide first-seen budget shared by all Policies: rate min(500, B ÷ N_published) per second with a count, else 200 per second (target), B = 20,000 per Cell (hypothesis), burst one second's rate. Within budget it gets full buckets. Past budget (OQ-traffic-management-and-resilience-20 (a)): a 10 s local-only entry (target) in the local-only segment, capacity and refill both the per-Node ceiling, no GCRA. File mode has no count: 200 per second. `config.localOnly` Policies skip this step, the over-limit cache and GCRA.
    4. Local buckets: one token bucket per key and `limits[]` entry, capacity = per-Node ceiling c, continuous refill c per `window`, starting full; every bucket must hold a token (all-or-nothing take), else 429 `RZ-RL-001` with `Retry-After` = seconds until the emptiest denying bucket holds one token (≥1, rounded up), zero State Store commands. Local-only entries and `localOnly` Policies then admit (bounded by N × c per window).
    5. GCRA: one `EVALSHA` of the GCRA script for every limit of the Policy with server `TIME`, updating TATs only if all allow (req 62). Allow → admit. Deny → store the over-limit entry until retry-after and return 429 `RZ-RL-002`. Nodes `SCRIPT LOAD` every script at connect, reconnect and failover; `NOSCRIPT` applies `failureMode` and schedules a single-flight reload per shard, never an `EVAL` retry (State Store area).
    6. A failed, timed-out, skipped (breaker open) or unattempted call applies `failureMode`: `open` admits from the fail-open buckets (req 61); `closed` 503 `RZ-STS-001` (timeout), `RZ-STS-002` (error, `NOSCRIPT`, unparsable reply), `RZ-STS-003` (breaker open), `RZ-STS-004` (deadline spent or in-flight ceiling full).
59. Per-Node ceiling c per limit: `perNodeCeiling` when declared; else, with a published count N_published (Control mode, M2), min(`requests`, max(10, ceil(2 × `requests` ÷ N_published))) (OQ-traffic-management-and-resilience-16 (b), target); a Control-mode Node without a count uses `requests` with `ruralz_node_degraded_info{reason="node_count_unknown"}` (M2); file mode (all of M1) without a declared ceiling uses `requests`, not degraded. Ceilings come from a `NodeCount` source interface that returns "unknown, file mode" in M1. [TMR "Per-Node ceiling"; FP 8.8]
60. GCRA parameters per limit: emission interval T = `window` ÷ `requests` in integer microseconds (at least 1 µs, proposed clamp); τ = `burst` × T (default `burst` = `requests`, so τ = `window` and one window admits up to 2 × `requests`). A key is allowed when max(TAT, now) − now ≤ τ; the new TAT is max(TAT, now) + T; retry-after = max(TAT, now) − now − τ. [TMR "Per-Node ceiling", "Algorithm choice"; ADR8]
61. Fail-open accounting (applies while a call fails for this request, to every limit of the Policy, declared and derived ceilings alike per ADR8 "State Store failure"): a per-key, per-limit counter over epoch-aligned windows (start = floor(now ÷ `window`) × `window`, Unix epoch, UTC) without carry-over, capacity c_fo = declared `perNodeCeiling`; else derived max(1, 2 × `requests` ÷ N_published) (unfloored); else Control mode without count max(1, `requests` ÷ 100) (M2); else file mode `requests`. Windows longer than 1 minute refill in 60 equal steps (c_fo ÷ 60 each `window` ÷ 60) (target). An exhausted fail-open counter denies with 429 `RZ-RL-001`. Result `fail_open` when admitted. Bounds: declared N_serving × c per window; file mode N × `requests` (target). [ADR8 "Decision outcome"; TMR "Per-Node ceiling", failure matrix; SDS "Consistency and accuracy bounds"]
62. GCRA script contract (redis driver; the memory driver implements the same arithmetic in Go under the shard lock, Node clock instead of `TIME`). Keys `rz:rl:<policy>:<requests>/<window>:{<sha256-hex of the key value>}` with `<policy>` = `metadata.name`, `<window>` = the canonical Duration string (`time.Duration.String()`, e.g. `1s`, `1m0s`, proposed), so every limit of one Policy and key shares one hash slot. A changed limit starts fresh state. [TMR "Decision path"; ADR8 "Keys"]

    ```lua
    -- rz_gcra v1. KEYS[i]: limit keys (one hash tag). ARGV[1] = n; ARGV[2i] = T_i µs; ARGV[2i+1] = tau_i µs.
    local t = redis.call('TIME')
    local now = tonumber(t[1]) * 1000000 + tonumber(t[2])
    local n = tonumber(ARGV[1])
    local tats, denied, retry = {}, 0, 0
    for i = 1, n do
      local T, tau = tonumber(ARGV[2*i]), tonumber(ARGV[2*i+1])
      local tat = tonumber(redis.call('GET', KEYS[i]) or now)
      if tat < now then tat = now end
      if tat - now > tau then
        if denied == 0 then denied = i end
        if tat - now - tau > retry then retry = tat - now - tau end
      end
      tats[i] = tat + T
    end
    if denied > 0 then return {0, denied, string.format('%.0f', retry), string.format('%.0f', now)} end
    for i = 1, n do
      local tau = tonumber(ARGV[2*i+1])
      redis.call('SET', KEYS[i], string.format('%.0f', tats[i]), 'PX', math.ceil((tats[i] - now + tau) / 1000))
    end
    return {1, 0, '0', string.format('%.0f', now)}
    ```

    Numbers cross the Lua boundary only through `string.format('%.0f', v)` (Lua `tostring` loses precision above 10^14). The reply gives the Node what it needs for RateLimit fields (req 67) by recomputing remaining = max(0, floor((τ − (TAT' − now)) ÷ T) + 1) and reset = TAT' − now from the returned `now` and its own arguments.
63. Local state table: buckets, over-limit and local-only entries share one Node-wide table of 1,048,576 entries, about 64 MiB (target), in 256 shards (target) each a short mutex with open addressing and CLOCK eviction; local-only entries use their own 131,072-entry segment (target) so a key-rotation flood never evicts a key with a GCRA answer. Entry key = (Policy identity, limit index, first 128 bits of the key's SHA-256); an evicted bucket restarts full (at most one extra ceiling per eviction) and counts `ruralz_ratelimit_bucket_evictions_total{policy}`. The table survives Hot Reloads; entries carry over by Policy name and limit (`requests`, `window`); a changed limit gets new entries. ratelimit stage budget (memory driver, S2): 2 µs p50, 8 µs p99, 0 allocations (target). [TMR "Decision path"; SDS "Stateless data plane", "Per-Node limits"; DP "Goroutines"; PBB]
64. Consumptive calls (FP 8.7 rule 3): GCRA and Quota reservations of consecutive admission-class Policies in `onRequestHeaders` run in chain order and stop at the first deny; consecutive calls whose keys share a hash slot (in `cluster` topology; every call in `standalone` and `memory`) run as one script (`rz_consume`, op `script_multi`), otherwise as sequential round trips. Local tokens taken by Policies after the first deny are returned. Each participating Policy still makes at most one blocking round trip (FP 8.7 rule 1); a shared call's timeout is the smallest participating `stateStoreTimeout` within the per-request deadline, and on failure each Policy applies its own `failureMode`. [FP 8.7; ADR8 "Confirmation" round-trip test]
65. A key reaches the State Store at most min(offered, 2 × N × c) times per window (target); a hot key above about 25% of a shard (25,000 per second, hypothesis) needs `perNodeCeiling` or `localOnly`. `localOnly` Policies never call the State Store, admitting at most N_serving × c per window even when healthy (OQ-scalability-and-distributed-state-11 (a)). With the `memory` driver every limit multiplies by N (M2 warns via `state_store_memory_multi_node`). [TMR "Decision path"; SDS "Distributed rate limits"; FP 8.8]
66. Metrics: `ruralz_ratelimit_decisions_total{policy,result}` with `allow` (GCRA allow, local-only or `localOnly` admit), `deny_local` (`RZ-RL-001`), `deny_global` (`RZ-RL-002`, cache or GCRA), `fail_open`; State Store calls are recorded by the State client as `ruralz_state_calls_total{op,result}` and `ruralz_state_call_duration_seconds{op}` with `op` = `gcra`, `quota`, `script_multi` (SLO-GW-5 uses `op="gcra"`). State Store call attributes go on the Policy's `ruralz.filter.<name>` span; a shared script is recorded on the first Policy's span and named by the others in `ruralz.state.batch`. [OBS]

### N. RateLimit response headers

67. On every 429 from `RZ-RL-001`, `RZ-RL-002` or `RZ-RL-003` (Content-Type `application/problem+json`), as RFC 9651 Structured Field lists (draft-ietf-httpapi-ratelimit-headers-11): `RateLimit-Policy` lists every limit applied to this request so far (ratelimit and quota Policies whose `when` held, in chain order): member = sf-string Policy name, suffixed `.1`, `.2` … when the Policy has several limits, with parameters `q` = `requests` (or the quota `limit`) and `w` = `window` in seconds rounded up; `RateLimit` names the denied limit with `r` (remaining; 0) and `t` (seconds until it admits, rounded up, ≥ 1); `Retry-After` = `t`. `RZ-RL-001`: `t` until the local bucket (or fail-open window) refills; `RZ-RL-002`: the GCRA or cached retry-after; `RZ-RL-003`: `t` to the jittered window end (req 73). `pk`, `qu` and `ai.token-budget` fields are omitted; `X-RateLimit-*` is not emitted (OQ-traffic-management-and-resilience-3). Example: `RateLimit-Policy: "ratelimit-global";q=1000;w=1, "ratelimit-gold.1";q=100;w=1, "ratelimit-gold.2";q=5000;w=60`, `RateLimit: "ratelimit-gold.1";r=0;t=1`, `Retry-After: 1`. [TMR "RateLimit response headers"]
68. Admitted responses (OQ-traffic-management-and-resilience-2 (c), aligned with area 2): the Filters record per applied limit (name, `q`, `w`, `r`, `t`) in the Exchange; area 4 appends `RateLimit-Policy` and `RateLimit` (one member per applied limit with a known `r`: GCRA allow → req 62 remaining/reset; local-only or `localOnly` → floor(tokens) and seconds to full; quota → `limit` − used and seconds to window end) after `onResponse`; a limit admitted `fail_open` or unmetered contributes only its `RateLimit-Policy` member. Registry Phases stay as FP 10.

### O. Quota (`quota`, unit `requests`)

69. Registry: admission, Phases `onRequestHeaders` and `onLog`, scopes G and R, `failureMode` default `open` (OQ-configuration-model-8 (a)). `config.consumerQuota` (required) names a Consumer quota with `unit: requests` (no Consumer defining it with that unit is RZ-CFG-009); `config.key` (CEL string) default `consumer.name`. API governance uses `overridable: false` Gateway `quota` Policies (precedence is area 2). [FP 10; CM "Policy"; TMR "Quotas"]
70. Order: `when` → a null Consumer, or one whose `quotas` lack the name with `unit: requests`, gets 403 `RZ-RL-004` (a decision; `ruralz_quota_decisions_total{result="no_quota"}`) → `config.key` (runtime error: `open` admits unmetered `fail_open`, `closed` 503 `RZ-RL-005`) → denial cache (req 74) → reservation script. Policies naming one Consumer quota share its counter and are charged once per request (a request-scoped set of reserved (consumerQuota, key hash) pairs; a second Policy admits without a call). [TMR "Quotas"; CM "Policy"]
71. Windows are fixed and epoch-aligned in UTC: start = floor(now ÷ `window`) × `window` from the Unix epoch (`24h` resets at midnight, `720h` every 30 days). Key: `rz:qt:<consumerQuota>:<window>:<window start>:{<sha256-hex of the key value>}` with `<window>` the canonical Duration string and `<window start>` Unix milliseconds (proposed encodings). The Node passes KEYS for its clock's window and the nearer neighbor (previous if in the first half, else next) with their starts; the script picks by server `TIME` and returns an error reply when skew exceeds half the window (→ `failureMode`). [TMR "Quotas"]

    ```lua
    -- rz_quota_reserve v1. KEYS[1..2]: counters; ARGV[1..2]: their starts (ms); ARGV[3]: window ms; ARGV[4]: limit.
    local t = redis.call('TIME'); local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
    local W = tonumber(ARGV[3]); local ws = now - (now % W); local k = 0
    if tonumber(ARGV[1]) == ws then k = 1 elseif tonumber(ARGV[2]) == ws then k = 2 end
    if k == 0 then return redis.error_reply('RZSKEW') end
    local used = tonumber(redis.call('GET', KEYS[k]) or '0')
    if used >= tonumber(ARGV[4]) then return {0, k, used, string.format('%.0f', ws + W - now)} end
    used = redis.call('INCR', KEYS[k])
    redis.call('PEXPIRE', KEYS[k], string.format('%.0f', ws + 2 * W - now))   -- expires one window after it closes
    return {1, k, used, string.format('%.0f', ws + W - now)}
    ```

72. Settlement in `onLog`: the unit is refunded (post-commit queue, class "Quota refunds, Token Budget settlement", `DECR` only if the chosen key holds > 0) when a later Filter rejected the request (short-circuit or `closed` failure after this Policy, including a later admission deny) or the final code is `RZ-UP-005`, `RZ-UP-006` or `RZ-UP-008`; cache hits are charged. Dropped refunds only over-charge (`ruralz_state_writes_dropped_total{kind}`). [TMR "Quotas"; SDS "Post-commit write queue"]
73. Exhausted quota: 429 `RZ-RL-003`, `Retry-After` = seconds to the window end plus uniform jitter up to min(1% of `window`, 60 s) (target), RateLimit fields per req 67 (`q` = `limit`, `w` = window seconds, `r=0`). [TMR "Quotas"]
74. Denial cache: 65,536 entries, CLOCK eviction (target), keyed by (Policy, consumerQuota, key hash, `limit`, window start), valid until min(60 s, window end) (target); a raised `limit` misses the cache at once. State Store failure: `open` admits unmetered (`fail_open`), `closed` 503 `RZ-STS-001..004`. Metrics `ruralz_quota_decisions_total{policy,result="allow"|"deny"|"no_quota"|"fail_open"}`. Healthy-store bound: at most `limit` per window per Cell, 0% over-admission (target). [TMR "Quotas"; SDS "Consistency and accuracy bounds", "Per-Node limits"; OBS]

### P. Response Cache (`cache`)

75. Registry: Filter class cache, Phases `onRequestHeaders` (lookup, after auth, authz and admission) and `onResponse` (store), scope R only (RZ-CFG-020), slot `cache`, `failureMode` default `open`. A `config.key` runtime error always bypasses (never 503). A hit skips `onRequestBody`; an effective chain combining `cache` with an `onRequestBody` authz or validation Policy, or a `plugin` auth/authz Policy in `onRequestBody`, is proposed RZ-CFG-038 (area 2 R-39). Semantics: RFC 9111 shared cache plus RFC 5861 `stale-while-revalidate` and `stale-if-error`. [TMR "Response caching"; FP 10]
76. Methods: GET is looked up and stored; HEAD is served from GET entries (headers only) and never stored; requests with `Range` bypass (proposed). Request directives: `no-store` bypasses lookup and store; `no-cache` or `max-age=0` forces revalidation; `only-if-cached` on a miss returns 504 (code: section 9 item 4). [TMR "Response caching"]
77. Storable: explicit freshness (`s-maxage`, `max-age` or `Expires`); status in {200, 203, 204, 300, 301, 308, 404, 405, 410, 414, 501} (proposed); none of `no-store`, `no-cache` (never stored at M1), `private`, `Vary: *`, `Set-Cookie`; a request carrying `Authorization` only under `public`, `s-maxage` or `must-revalidate`; body read through a tee of at most 128 KiB (target) within the buffer budget, else skipped (`size_limit` or `buffer_budget`). The stored response is the one the cache Filter sees in `onResponse` (after Route-level transforms). [TMR "Response caching"; CM "Body buffering and limits"]
78. Freshness lifetime: `s-maxage`, else `max-age`, else `Expires` − `Date`, capped at 24 h; `stale-while-revalidate` and `stale-if-error` capped at 1 h (target). `must-revalidate`, `proxy-revalidate` and `s-maxage` (implying `proxy-revalidate`, RFC 9111 §5.2.2.10) forbid serving stale (§4.2.4), overriding RFC 5861. Age per RFC 9111 §4.2.3 using the Node clock. Hits carry `Age` and `Cache-Status` (RFC 9211) with cache name `ruralz` (proposed): fresh hit `ruralz; hit; ttl=<s>`; stale under SWR `ruralz; hit; ttl=<negative s>`; stale-if-error `ruralz; fwd=stale; fwd-status=<status>; ttl=<negative s>`; miss `ruralz; fwd=miss` (`; stored` when stored). [TMR "Response caching"]
79. Partition value: `config.key` when set; otherwise, on a Route whose effective chain holds an auth-class Policy, the principal (`consumer.name`, else `auth.method` with JWT `iss` and `sub`, or the client certificate subject); otherwise `""` (proposed reading; section 9 item 1). [TMR "Response caching" (Principal row); SEC "Consumers and tiers"]
80. Layout: U = SHA-256(scheme, 0x00, lowercased host without default port, 0x00, escaped normalized path, 0x00, normalized query: parameters sorted by name then value, RFC 3986 percent-encoded); P = SHA-256(partition value); V = SHA-256 over the Vary names sorted and lowercased, each with its request value (trimmed, internal whitespace collapsed, absent marked distinctly). Keys: generation `rz:rc:{<U>}:gen` (expires 26 h after its last bump); partition hash `rz:rc:{<U>:<P>}` with fields `names` (sorted, comma-joined Vary names), `idx` (variant ids with absolute expiry) and `v:<V>` (at most 8 variants, target); fill lease `rz:rc:{<U>:<P>}:lease`. Hex encoding of the digests, proposed. [TMR "Response caching" (Layout row)]
81. Lookup: one pipelined batch of two read-only calls (FP 8.7 rule 3): `GET rz:rc:{U}:gen` and `HMGET rz:rc:{U:P} names v:<V>`, V computed from the per-URI Vary names the Node has cached in a bounded LRU (65,536 URIs, target; empty list when unknown). A hit needs equal names, a present variant, variant generation = current generation (missing key reads as 0), an intact entry (MAC per OQ-security-and-identity-22 (a) when a key is configured) and freshness or an allowed stale window; else miss, and differing names are learned. A hit first reserves its size from `limits.maxBufferedBytes`, else bypasses. (HMGET replaces the doc's read-only "partition-slot script" with identical semantics, section 9 item 18.) [TMR "Response caching"]
82. Hot-entry layer: 64 MiB per Node by LRU (target) keeps entries and generation values read twice within 1 s, each for at most 1 s (target); lookups consult it first, so a hot URI costs at most N reads per second per Cell and an invalidation may be missed for 1 s longer. [TMR "Response caching" rule 1]
83. Store (post-commit, class "Cache stores"): TTL = freshness + the larger stale window (≤ 25 h); one script in the partition slot: `SET lease NX PX 1000` (target) or skip; if stored `names` differ, delete the partition hash first; write `names`, `v:<V>` (tagged with the generation the lookup read), update `idx` (drop expired, evict the earliest-expiring beyond 8), `PEXPIRE` the hash to the max TTL. A store racing an invalidation never hits because its generation is stale. [TMR "Response caching"]
84. Memory protection (until OQ-traffic-management-and-resilience-11 option (a) isolates the cache; always on): rule 2: read `INFO memory` per shard off the request path every 10 s, or 1 s above 50% of `maxmemory` (target); skip stores above 70% and to a shard that grew over 10% of `maxmemory` between reads until growth falls under 2% (target); skip all stores to a shard with `maxmemory` 0 or before its first reading (proposed). Rule 3: per-Node store byte cap per shard c = min(2% of `maxmemory` per 10 s ÷ N_c, 4 MiB per second), N_c = N_published or 1,000 without a count (target). Skips count `ruralz_cache_store_skipped_total{policy,reason="memory"}`. Nodes warn at startup when `cache` and a limit Policy type share a State Store. Generation keys bypass rules 2 and 3. [TMR "Response caching"; SDS "Distributed caches"]
85. Invalidation: after commit, a non-error response (status < 400, proposed reading of "non-error") to any unsafe method (not GET, HEAD, OPTIONS, TRACE) on a `cache` Route queues a generation bump for the request's U (class "Cache invalidations", reserved 10%, one retry, then `ruralz_state_writes_dropped_total{kind="cache_invalidate"}`): `rz_cache_gen` sets the key to server `TIME` in µs (or old + 1 if not greater), `PEXPIRE` 26 h (target). `Location`/`Content-Location` invalidation is not done. [TMR "Response caching"; SDS "Post-commit write queue"]
86. `stale-while-revalidate`: serve stale at once and enqueue a revalidation on a 256-entry queue served by 4 workers, dropping when full (target). A worker takes the lease `SET NX PX 5000` (target) or gives up; it replays the saved request (the upstream request as the request Phases left it, minus upstream credentials and minus `Authorization`, `Proxy-Authorization`, `Cookie` and `auth.api-key` headers, proposed) through upstream-leg Policies, breaker and bulkhead with `If-None-Match`/`If-Modified-Since`; 304 refreshes metadata under the lease; any other result deletes the variant ("ends the stale window, unstored"). [TMR "Response caching", Figure 4]
87. `stale-if-error`: when the leg ends with an `RZ-UP-*` code (the breaker open included) or an Upstream 500, 502, 503 or 504, and a stale variant found at lookup is within `stale-if-error` and not forbidden by req 78, `onResponse` replaces the response with it (`ruralz_cache_requests_total{result="stale_error"}`). [TMR failure matrix]
88. Miss coalescing: concurrent misses for one (U, P, V) on one Node, up to 4,096 keys (target), wait for the first fetch and reuse it like a hit through their own response chain when it is storable with matching Vary-selected values and within the tee; otherwise, or when the leader fails, each fetches itself, bounded by its own deadline. [TMR "Response caching"]
89. State Store failure: `open` bypasses (lookup) or skips (store); `closed` 503 `RZ-STS-001..004` on lookup. `ruralz_cache_requests_total{route,result="hit"|"miss"|"bypass"|"stale"|"stale_error"}`; `ruralz_cache_store_skipped_total{policy,reason="size_limit"|"buffer_budget"|"memory"}`. [TMR failure matrix; OBS]
90. Cache operations use the cache State Store handle (OQ-scalability-and-distributed-state-3 (a): a cache connection under `Gateway.spec.stateStore`, field name owned by area 1/2), falling back to the main State Store with reqs 84 and the startup warning when unset. [SDS "Distributed caches"]

### Q. State Store interaction rules for this area

91. Before commit each Policy makes at most one blocking State Store round trip; its timeout is min(Policy `stateStoreTimeout` (default Gateway `stateStore.timeout`), time left in the per-request deadline = the Route's largest `stateStoreTimeout` from the start of `onRequestHeaders`, area 4 req 45); after it expires remaining Policies apply `failureMode` without waiting; an open shard breaker skips calls. Local denials, over-limit hits and denial-cache hits send zero commands. Post-commit writes never delay a response. `onChunk` never calls the State Store. [FP 8.7; SDS "State client"]
92. Every script key is in `KEYS`, and all keys of one script share one hash slot (no `CROSSSLOT` in `cluster` topology). Scripts run with `EVALSHA` through the State client (rueidis `Do`, never `Lua.Exec`). The `memory` driver implements every script's semantics in Go with identical test vectors. [ADR8 "Code"; TS "Rate limiting" row; TQ "Integration tests"]
93. The State Store holding limit keys MUST run `maxmemory-policy noeviction`; a Node detecting otherwise raises `state_store_eviction_policy` (State Store area). [TMR "Response caching"; SDS "State Store availability"]

### R. Telemetry

94. Metrics of this area (OBS "Ruralz Gateway metrics", all Planned (M1)): `ruralz_upstream_attempts_total{upstream,status_class,error}` (`error` `none`, `connect`, `timeout`, `reset`, `tls`), `ruralz_upstream_attempt_duration_seconds{upstream}` (histogram `request`, connect to last byte), `ruralz_upstream_retries_total{upstream}`, `ruralz_upstream_retry_budget_exhausted_total{upstream}`, `ruralz_upstream_breaker_state_info{upstream,state}`, `ruralz_upstream_ejections_total{upstream,reason}`, `ruralz_upstream_healthy_endpoints{upstream}`, `ruralz_upstream_probes_skipped_total{upstream}`, `ruralz_upstream_degraded_info{upstream,reason="panic"|"discovery_stale"|"balancer_budget"}`, `ruralz_upstream_cel_errors_total{upstream,field="hashKey"|"retryOn"|"failureWhen"}`, `ruralz_upstream_pool_connections{upstream,state="idle"|"active"}`, the ratelimit, quota and cache families above, `ruralz_state_writes_dropped_total{kind}`, `ruralz_security_cleartext_hops{hop="upstream"}`, and `ruralz_node_degraded_info` reasons `upstream_panic`, `discovery_stale`, `balancer_budget`, `probes_skipped`, `cleartext_hop`. `ruralz_upstream_attempts_total` and `..._attempt_duration_seconds` label sets of the first 1,000 Upstreams are striped hot sets. Enumerated label values exist at 0 from start. No Consumer, client address, path or key value ever becomes a label. [OBS]
95. Spans: one CLIENT span `ruralz.upstream.<Upstream name>` per leg attempt (attributes: attempt number, Endpoint, status or error kind, composition step, HTTP client semantic conventions); upstream-leg Policies' spans are its children. Access log: `upstream`, `endpoint`, `attempts` of the last leg, `upstream_duration`, `state_store_duration`. [OBS "Span model", "Access logs"]

### S. `/debug/upstreams`

96. The Upstream area provides `WriteJSON(w io.Writer) error` for area 4's admin handler: `{"upstreams":[{"name","protocol","tls":bool,"source":{"type":"static"|"dns","lastRefresh","stale":bool,"failures":int,"error"},"balancer":{"algorithm","virtualNodes","fallback":bool},"breaker":{"state","openedAt","windowLegs","windowFailures","consecutiveFailures"},"bulkhead":{"inFlight","pending","maxConnections","maxPendingRequests"},"retryBudget":{"retriesInFlight","originalsInFlight"},"panic":bool,"degraded":[reason],"endpoints":[{"identity","addresses":[ip],"weight","healthy":bool,"ejected":bool,"ejectedUntil","ejections","consecutiveErrors","suspect":bool,"inFlight","failures1s"}]}]}`, sorted by name and identity, times RFC 3339 UTC, no secrets, no request data (proposed shape). [DP "Admin endpoints"]

### T. Validation contributions (run by area 1/2 validators)

97. `ring-hash` without `hashKey`: RZ-CFG-005. `dns` discovery without `service` or `port`: RZ-CFG-005. `tls.clientCertificate` without `clientKey` or vice versa: RZ-CFG-005 (proposed). `perNodeCeiling` outside 1..`requests`, `burst` outside 0..`requests`: RZ-CFG-005. Missing Consumer quota: RZ-CFG-009. Steps over `maxCompositionSteps`: RZ-CFG-031; step `maxBodyBytes` sums: RZ-CFG-032. CEL errors in this area's places: RZ-CFG-014/015. `cache` at G or U scope: RZ-CFG-020. `cache` with onRequestBody authz/validation: proposed RZ-CFG-038. Balancer budget warning (OQ-traffic-management-and-resilience-23 (b), a warning only). [CM "Error codes"]

## 3. Proposed Go packages and API

Placement follows RL "Monorepo tree" (`internal/gateway/upstream`), "Where Go code goes" (Filters at `internal/filter/<type>`: `internal/filter/ratelimit`, `internal/filter/quota`, `internal/filter/cache`) and "Import boundaries"; rueidis stays in `internal/statestore/redis` (depguard), cel-go in `internal/cel` (area 3). Every file: license header, no `init()`, no mutable package globals, `context.Context` first on blocking calls, `%w` wrapping, `slog` only via `internal/telemetry`, bounded goroutines and channels.

| Package | Responsibility | Imports (non-stdlib) |
|---|---|---|
| `internal/gateway/upstream` | Manager, runtimes, attempt loop, deadlines, retries, retry budget, breaker, bulkhead, error kinds and codes, transports, dialer policy, weighted leg pick, `/debug/upstreams` | `internal/filter`, `internal/cel`, `internal/errcode`, `internal/telemetry`, `internal/gateway/body`, `internal/gateway/snapshot` |
| `internal/gateway/upstream/balance` | Four algorithms, schedule/ring builders, budget planner | none |
| `internal/gateway/upstream/health` | Passive ejection, active prober, probe slots, stall/suspect | none |
| `internal/gateway/upstream/discovery` | Static host re-resolution, DNS A/AAAA/SRV, backoff, `Resolver` interface | none |
| `internal/gateway/composition` | Step executor for the three modes, merge, partial responses | `internal/gateway/upstream`, `internal/cel`, `internal/gateway/body`, `internal/gateway/admission` |
| `internal/filter/consume` | Consumptive-call group runner (FP 8.7 rule 3), request-scoped RateLimit field accumulator | `internal/filter`, `internal/statestore` |
| `internal/filter/ratelimit` | `ratelimit` Filter, Node-wide key table, first-seen budget, fail-open windows | `internal/filter`, `internal/filter/consume`, `internal/cel`, `internal/statestore` |
| `internal/filter/quota` | `quota` Filter, denial cache, refunds | same |
| `internal/filter/cache` | `cache` Filter, directive parsing, entry codec, hot layer, coalescing, revalidation workers, memory guard | `internal/filter`, `internal/cel`, `internal/statestore` |
| `internal/sfv` | RFC 9651 Structured Field list serializer (tiny, stdlib) | none |
| `internal/statestore` (State Store area) | Typed operations listed in section 4; scripts of section 2 embedded in `internal/statestore/redis/script/*.lua` | rueidis only in `redis` |

`internal/filter/*` never imports `internal/gateway/...` (it sees buffers and the snapshot only through area 4's `filter.Exchange`); the cache Filter's revalidation replays through its own `cache.Replayer` interface, which `upstream.Manager` implements and area 4 injects at build time.

```go
package upstream // internal/gateway/upstream

type ErrorKind uint8

const (
	KindNone ErrorKind = iota
	KindConnect
	KindTLS
	KindTimeout
	KindReset
)

func (k ErrorKind) String() string // "none" | "connect" | "tls" | "timeout" | "reset"
func Classify(err error, attemptCtx, legCtx, routeCtx context.Context) ErrorKind

// Compiled is the immutable per-snapshot form of one Upstream.
type Compiled struct {
	Name          string
	Timeout       time.Duration // 0: the Route's
	Attempts      int           // retries
	PerTry        time.Duration // 0: derived
	RetryOn       *cel.Program  // nil: DefaultRetryOn
	FailureWhen   *cel.Program  // nil: DefaultFailureWhen
	HashKey       *cel.Program  // ring-hash only
	Algorithm     balance.Algorithm
	Breaker       BreakerPolicy
	Passive       health.PassivePolicy
	Active        *health.ActivePolicy // nil: off
	TLS           *TLSPolicy           // nil: cleartext
	Source        discovery.Spec
	LegChain      any // area 4 per-leg Policy arrays, opaque here
	rt            *runtime
}

type BreakerPolicy struct {
	MaxConnections, MaxPending, ConsecutiveFailures, MinimumLegs, HalfOpenSuccesses int
	FailureRatio float64
	OpenDuration time.Duration
}

type TLSPolicy struct {
	SNI        string
	CA         secret.Ref // resolved through area 1's secret.Resolver
	ClientCert secret.Ref
	ClientKey  secret.Ref
}

type ManagerConfig struct {
	Clock       Clock
	Rand        func() uint64
	Resolver    discovery.Resolver // net.Resolver adapter in production
	FetchAllow  []netip.Prefix     // RURALZ_FETCH_ALLOW
	Secrets     SecretSource
	Tel         Telemetry
	BudgetBytes int64 // 256 MiB balancer budget
}

// Manager owns every Upstream runtime for the Node's lifetime.
type Manager struct{ /* runtimes map[string]*runtime under mu; scheduler; worker pools */ }

func NewManager(cfg ManagerConfig) *Manager
func (m *Manager) Compile(ctx context.Context, ups []*v1alpha1.Upstream, progs CELSource) (*Set, error) // carries state by name
func (m *Manager) Run(ctx context.Context) error // scheduler: discovery, rebuilds (8), probes, breaker timers
func (m *Manager) WriteJSON(w io.Writer) error   // /debug/upstreams

// Set is the snapshot-held collection; Close releases runtime references at zero pins.
type Set struct{ /* by name */ }

func (s *Set) Get(name string) (*Compiled, bool)
func (s *Set) Close() error

// Hooks runs the per-attempt Phases; implemented by area 4's chain executor.
type Hooks interface {
	OnUpstreamRequest(ctx context.Context, a *Attempt) *filter.Response // non-nil ends the leg
	OnUpstreamResponseHeaders(ctx context.Context, a *Attempt) (retry bool)
	OnUpstreamResponseBody(ctx context.Context, a *Attempt) *filter.Response
}

type Attempt struct {
	Upstream *Compiled
	Number   int
	Endpoint string
	Req      *http.Request  // outgoing; mutable in OnUpstreamRequest
	Resp     *http.Response // after headers
	Err      ErrorKind
}

type LegRequest struct {
	Upstream      *Compiled
	Method        string
	Path, RawQuery string
	Header        http.Header
	Body          ReplayableBody
	RouteDeadline time.Time
	Vars          *cel.Vars // Base variables for hashKey; leg copies for retryOn/failureWhen
	Step          string    // "" for plain upstreams
}

type ReplayableBody interface {
	io.ReadCloser
	Replayable() bool // empty, buffered, or zero bytes read so far
	Rewind() error
}

type Result struct {
	Resp      *http.Response   // last attempt's response; Body.Close releases the slot
	Generated *filter.Response // onUpstreamRequest short-circuit
	Code      string           // RZ-UP-00x when Resp and Generated are nil
	Kind      ErrorKind
	Attempts  int
	Endpoint  string
}

func (m *Manager) Do(ctx context.Context, req *LegRequest, h Hooks) Result

// RoutePlan is stored in snapshot.Route (as snapshot.ForwardPlan) for plain upstreams.
type RoutePlan struct{ Legs []WeightedLeg; total uint64 }
type WeightedLeg struct{ Upstream *Compiled; Weight uint32 }

func (p *RoutePlan) Pick(r uint64) (*Compiled, bool) // false: every weight 0 → RZ-UP-008

// Forwarder implements area 4's handler.Forwarder for plain upstreams and composition.
type Forwarder struct{ M *Manager; C *composition.Executor }

func (f *Forwarder) Forward(ctx context.Context, x *filter.Exchange, route *snapshot.Route, h Hooks) (*Response, error)

// Response is what the handler commits: a streamed or buffered body plus the code for onLog.
type Response struct {
	Status  int
	Header  http.Header
	Body    io.ReadCloser
	Code    string // RZ code when generated
	Partial bool   // ruralz-partial: true
}

// Replay implements cache.Replayer: revalidation through upstream-leg Policies, breaker and bulkhead.
func (m *Manager) Replay(ctx context.Context, upstream string, req *http.Request) (*http.Response, error)

type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}
type Timer interface{ Stop() bool; Reset(d time.Duration) bool }
```

```go
package balance // internal/gateway/upstream/balance

type Algorithm uint8 // RoundRobin, LeastRequest, RingHash, Random

type Endpoint struct {
	Identity string
	Weight   uint32
	InFlight *atomic.Int64
	Fails1s  func(now int64) int64
}

// Picker is immutable once built; swapped copy-on-write.
type Picker interface {
	Pick(key uint64, rnd func() uint64, exclude func(i int) bool) (i int, ok bool)
	Bytes() int64
}

func BuildRoundRobin(eps []Endpoint, seed uint64) Picker // L = min(65536, 64E)
func BuildRing(eps []Endpoint, v int) Picker             // 16-byte vnodes, FNV-1a-64 + splitmix64
func BuildLeastRequest(eps []Endpoint) Picker
func BuildRandom(eps []Endpoint) Picker
func HashKey(s string) uint64

type Plan struct{ V int; Fallback map[string]bool } // per ring Upstream
func PlanBudget(budget int64, schedules int64, rings map[string]int) Plan
```

```go
package health // internal/gateway/upstream/health

type PassivePolicy struct{ ConsecutiveErrors int; EjectionTime time.Duration }
type ActivePolicy struct {
	Path                                string
	Interval, Timeout                   time.Duration
	HealthyThreshold, UnhealthyThreshold int
}

type EndpointState struct{ /* atomics: consecutive, ejectedUntil, ejections, healthy, stalled, lastHeaders */ }

type Tracker struct{ /* per Upstream */ }

func (t *Tracker) Observe(i int, failed bool, kind upstreamKind, suspect bool, now time.Time) (ejected bool)
func (t *Tracker) Eligible(i int, now time.Time) bool
type Prober struct{ /* shared probe slots ≤256 */ }
func (p *Prober) Run(ctx context.Context) error
```

```go
package discovery // internal/gateway/upstream/discovery

type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

type Spec struct {
	Static []StaticEndpoint // address, weight
	DNS    *DNSSpec         // service, port (number or name)
}

type Set struct{ Endpoints []Endpoint; Stale bool; Err error; Refreshed time.Time }
type Endpoint struct{ Identity, TLSName string; Addrs []netip.AddrPort; Weight uint32 }

type Source struct{ /* spec, resolver, last good, NXDOMAIN streak, backoff */ }
func (s *Source) Refresh(ctx context.Context) (Set, time.Duration) // next delay: 30s ±10%, or backoff 1-60s
```

```go
package composition // internal/gateway/composition

type Step struct {
	Name, Upstream, Method, Path string
	PathExpr, When               *cel.Program
	Target                       string
	Select                       []string
	Rename                       map[string]string
	Collection, Optional, Merge  bool
	BodyRead                     bool  // read later through steps: gate
	MaxBodyBytes                 int64
}

type Compiled struct{ Mode v1alpha1.CompositionMode; Steps []Step }

type Executor struct{ Upstreams *upstream.Manager; Units UnitAcquirer; Budget BudgetReserver }

func (e *Executor) Run(ctx context.Context, x *filter.Exchange, c *Compiled, h upstream.Hooks) (*upstream.Response, error)
func Merge(dst *OrderedObject, body []byte, s *Step) error // RZ-RT-015 on non-JSON or oversize
```

```go
package consume // internal/filter/consume

// Member is implemented by the ratelimit and quota Filters.
type Member interface {
	// Prepare runs every Node-local step and returns a final local result or one State Store op.
	Prepare(ctx context.Context, x *filter.Exchange) (op *statestore.ConsumeOp, local filter.Result, done bool)
	// Complete applies the reply (or the call error) and returns the Filter result.
	Complete(ctx context.Context, x *filter.Exchange, r statestore.ConsumeResult, err error) filter.Result
	// Undo returns local tokens when an earlier member denied.
	Undo(x *filter.Exchange)
}

// Run executes a maximal run of consecutive admission-class Members (when already true) in chain order.
func Run(ctx context.Context, x *filter.Exchange, ms []Member, st statestore.Consumer) []filter.Result

// Fields is the request-scoped RateLimit accumulator stored in the Exchange.
type Fields struct{ /* ≤ 16 entries inline: name, q, w, r, t, known */ }
func (f *Fields) Add(name string, q int64, wSeconds int64, r, t int64, known bool)
func (f *Fields) AppendPolicy(dst []byte) []byte         // RateLimit-Policy
func (f *Fields) AppendRateLimit(dst []byte, only int) []byte // RateLimit; only = denied index or -1
```

```go
package ratelimit // internal/filter/ratelimit

type TableConfig struct{ Entries, LocalOnly, Shards int } // 1_048_576, 131_072, 256

type Table struct{ /* shards; CLOCK hands; eviction counters by Policy */ }
func NewTable(cfg TableConfig, clk Clock) *Table

type NodeCount interface{ Published() (n int, known, controlMode bool) } // M1: (0, false, false)

type FirstSeen struct{ /* token bucket: rate from NodeCount, 200/s without */ }

type Factory struct{ Table *Table; First *FirstSeen; Count NodeCount; Store statestore.Consumer; Tel Telemetry }

func (f *Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, filter.PhaseSet, error)
// The built Filter implements filter.Filter (Handle for a lone member) and consume.Member.
```

```go
package quota // internal/filter/quota

type Factory struct {
	Consumers ConsumerQuotas // (consumer, quota name) → limit, window, unit from the snapshot
	Denials   *DenialCache   // 65_536, CLOCK, 60 s
	Store     statestore.Consumer
	Queue     statestore.Enqueuer
	Rand      func() uint64
}
func (f *Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, filter.PhaseSet, error)
```

```go
package cache // internal/filter/cache

type Directives struct {
	NoStore, NoCache, Private, Public, MustRevalidate, ProxyRevalidate, OnlyIfCached bool
	MaxAge, SMaxAge, StaleWhileRevalidate, StaleIfError, MinFresh, MaxStale time.Duration
	Has                                                               uint32 // bitset of present directives
}
func ParseCacheControl(values []string) Directives // tolerant, never allocates per directive

type Entry struct {
	Generation            int64
	ResponseTime, Date    time.Time
	AgeValue, Lifetime    time.Duration
	SWR, SIE              time.Duration
	StaleForbidden        bool
	Status                int
	Header                http.Header
	Body                  []byte
	ETag, LastModified    string
	Saved                 SavedRequest // for revalidation
}
func (e *Entry) MarshalBinary(mac []byte) ([]byte, error) // version byte 1; HMAC-SHA-256 when mac key set
func UnmarshalEntry(b, macKey []byte) (*Entry, error)

type Replayer interface {
	Replay(ctx context.Context, upstream string, req *http.Request) (*http.Response, error)
}

type Keys struct{ Gen, Part, Lease, Field string }
func KeysFor(scheme, host, escapedPath, rawQuery, partition string, varyNames []string, h http.Header) Keys

type Factory struct {
	Store    statestore.Cache // cache handle (OQ-SDS-3 a) or main store
	Queue    statestore.Enqueuer
	Memory   statestore.MemoryWatch
	Replayer Replayer // implemented by upstream.Manager
	Hot      *HotLayer // 64 MiB LRU
	Vary     *VaryLRU  // 65_536 URIs
	MACKey   []byte    // nil: no MAC
}
func (f *Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, filter.PhaseSet, error)
func (f *Factory) Run(ctx context.Context) error // 4 revalidation workers, 256-entry queue; INFO memory poller
```

Concurrency model:

- Request path: the request goroutine runs the leg synchronously (attempt loop, backoff via a pooled timer and `select` on the context). `aggregate` runs one goroutine per step under a shared cancel context, joined before merge; each holds one in-flight unit. No global lock across I/O: Endpoint sets and pickers are `atomic.Pointer`; per-Endpoint and per-Upstream counters are atomics; breaker transitions take a short per-Upstream mutex; the bulkhead is an atomic counter plus a mutex-guarded FIFO of at most `maxPendingRequests` waiters; the ratelimit table uses 256 sharded mutexes; the hot layer and Vary LRU are sharded.
- Background goroutines, all owned by `Manager.Run` or `cache.Factory.Run` and cancelled/awaited: one scheduler (min-heap of due work) per Manager; a discovery pool of 8 workers; a rebuild pool of 8 (TMR "8 at a time"); a probe pool bounded by probe slots (≤256); 4 revalidation workers with a 256-entry queue; one `INFO memory` poller per cache store handle. Stall timers are per attempt (pooled `time.Timer` reset per attempt, no allocation). Post-commit writes (refunds, stores, invalidations) go to the State Store area's bounded queue.
- Snapshot interplay: `Manager.Compile` runs inside area 4's activation; runtimes are reference-counted by `Set`s; `Set.Close` is called at zero pins.

Exported for other areas: `upstream.Forwarder` (area 4 `handler.Forwarder`), `upstream.Hooks` and `upstream.Response` (area 4 moves `LegHooks`/`UpstreamResponse` to these types to avoid an import cycle), `upstream.Manager.WriteJSON` (admin), `upstream.RoutePlan` (snapshot `ForwardPlan`), `Manager.Replay` (implements `cache.Replayer`), `consume.Run`/`Member` (area 4 executor), `ratelimit.NodeCount` (M2 Control Stream), the three `filter.Factory` implementations, `sfv`.

## 4. Dependencies on other areas

| Direction | Area | Contract |
|---|---|---|
| Needs | Area 1 (config load) | `hub.Bundle` typed resources; `secret.Resolver` for `tls.caCertificate`, `clientCertificate`, `clientKey` (current value, rotation subscription); `RURALZ_FETCH_ALLOW` parsing (with OQ-security-and-identity-22 settings); cache-connection field of OQ-scalability-and-distributed-state-3 (a) resolved like `stateStore.url` |
| Needs | Area 2 (canonical, precedence, validation) | `+ruralz:default` markers of req 4 (static defaults) and new fields `circuitBreaker.minimumLegs`, `failureRatio`, `halfOpenSuccesses` in `pkg/config/v1alpha1` (schema regenerated); per-Route chain arrays including per-leg arrays; registration of RZ-CFG-038 and the validation rules of req 97; leg order rule (area 2 R-41) |
| Needs | Area 3 (CEL) | `cel.Program` `EvalBool`/`EvalString` with `cel.Vars` (leg copies: `Request`, `Response`, `Error`, `Upstream`, `Attempt`, `Steps`), `Compiler.Default(PlaceRetryOn / PlaceFailureWhen)`, `Refs` (to detect `steps.<name>.body` readers), `EvalError` without request data; `request.query` shape |
| Needs | Area 4 (data plane core) | `filter` SPI (`Filter.Handle`, `Exchange`, `Result`, `Response`, `BuildEnv`, `Factory`), `Exchange.StateDeadline`, `Budget`, `CELActivation`; executor extension to call `consume.Run` for consecutive consumptive admission Policies and to append RateLimit fields after `onResponse` (req 68); `LegHooks.OnUpstreamResponseHeaders` returning a retry request; `admission.Units.TryAcquire(k)` for aggregate steps; `body.Budget`, `ReadGate`, `Tee`, `ErrTooLarge`; forwarding headers (area 4 req 18); problem writer and `RZ-RT-004/005/007/010/015` codes; `snapshot.Route` fields (timeout, `ForwardPlan`); commit/abort API for `RZ-UP-009`; admin `UpstreamDebugger` |
| Needs | State Store area (`internal/statestore`) | Typed ops: `Consumer.Consume(ctx, []ConsumeOp, []ConsumeResult) error` (GCRA, QuotaReserve; slot-grouped, stop at first deny, one round trip per group), `Cache.Lookup(ctx, genKey, partKey, fields) (gen int64, names, blob []byte, err)` (one pipelined batch), `Cache.Store/Revalidate/Delete/Lease` scripts, `Enqueuer.Enqueue(class, write) bool` with classes refunds/invalidations/stores (FP 8.7 rule 4, SDS queue), `MemoryWatch` (`INFO memory` per shard), `SlotOf(key)`, errors mapped to `RZ-STS-001..004`, per-shard breaker, `SCRIPT LOAD`/`NOSCRIPT` handling, `noeviction` check; `memory` driver implementing identical semantics; embedding this area's Lua scripts |
| Needs | Telemetry area | Metric handles on Ruralz aggregates (striped for hot Upstream label sets), tracer (`ruralz.upstream.<name>` CLIENT spans, `traceparent` injection into legs), degraded-state setter, cleartext-hop gauge, access-log fields, `internal/telemetry` logger |
| Needs | Security area | `auth.upstream-oauth2` as an upstream-leg Filter; credential header names to strip from saved cache requests; entry MAC key source (OQ-security-and-identity-22) |
| Needs | M2 Control Stream (future) | `ratelimit.NodeCount` implementation from `HeartbeatReply.clusterNodeCount` |
| Provides | Area 4 | `handler.Forwarder` implementation, `Hooks`/`Response` types, `/debug/upstreams` JSON, `RZ-UP-*` responses, weighted leg pick in `onRoute` |
| Provides | Filters registry | Factories for `ratelimit`, `quota`, `cache` |
| Provides | Validation (area 1/2) | Balancer budget planner (warning), composition compile checks |

## 5. Libraries

| Module | Version | Use | Catalog row |
|---|---|---|---|
| Standard library (go 1.26.0 floor, toolchain go1.27.1) | — | `net/http` (`Transport`, `Protocols`, `HTTP2Config`, `httptrace`), `net` (`Dialer`, `Resolver`, `LookupSRV`), `net/netip`, `crypto/tls`, `crypto/x509`, `crypto/sha256`, `crypto/hmac`, `hash/fnv`, `math/rand/v2`, `container/heap`, `sync`, `sync/atomic`, `encoding/json`, `encoding/binary`, `time`, `context`, `strconv` | TS "HTTP/1.1 and HTTP/2", "Service discovery" (`net` for DNS SRV); OQ-traffic-management-and-resilience-15 (a) standard-library hash |
| `cel.dev/cel-go` | v0.32.0 (only through area 3's `internal/cel`) | `hashKey`, `retryOn`, `failureWhen`, `pathExpression`, step `when`, `config.key` | TS "Expressions" |
| `github.com/redis/rueidis` | v1.0.78 (only in `internal/statestore/redis`) | `EVALSHA` of `rz_gcra`, `rz_quota_reserve`, `rz_quota_refund`, `rz_consume`, `rz_cache_store`, `rz_cache_meta`, `rz_cache_gen`; pipelined cache lookups; `INFO memory` | TS "State Store client", "Rate limiting" (Ruralz code, no redis_rate or throttled) |
| `go.opentelemetry.io/otel` | v1.46.0 (only through `internal/telemetry`) | spans and metric aggregates | TS "Telemetry" |

No other module: no `x/net` (the HTTP/2 client is `net/http`'s), no hashing, LRU, singleflight, DNS or Structured Field library (own code). rueidis v1.0.78 declares `go 1.25.0` and requires `golang.org/x/sys v0.47.0` (below the catalog's v0.48.x row) and `github.com/onsi/gomega` (test requirement) in its module graph; the depgate license check must accept them (section 9 item 21).

## 6. Test plan

Hermetic unit tests use an injected `Clock`, `Rand` and fake State Store; all run shuffled under `-race` and again with `CGO_ENABLED=0`. Integration tests use build tag `integration` and the local `redis-server` 7.0.15 (no Docker): a fixture starts `redis-server --port 0`-style ephemeral instances (standalone, and three `cluster-enabled yes` instances assembled with `CLUSTER ADDSLOTSRANGE`/`CLUSTER MEET` for cluster cases); Redis 8, Valkey 9.0.1+ and Dragonfly run in CI stage 8 via testcontainers.

Unit and table tests:

1. `Classify`: real sockets for each kind: dial to a closed port (`connect`), dial to a black-holed address with 1 s dial timeout via a listener that never accepts (backlog full) (`connect`), refused private address without `RURALZ_FETCH_ALLOW` (`connect`), TLS to a plaintext server and to an untrusted certificate and a handshake that never completes (`tls`, 2 s), server closing after reading headers (`reset`), HTTP/2 server sending `RST_STREAM`, and a ping-timeout close with a silent HTTP/2 server (`reset`), per-try, leg and Route context expiry (`timeout`).
2. Code selection table (req 40): {no retry, retry} × {connect, tls, reset, timeout(per-try), timeout(leg), timeout(Route)} → `RZ-UP-001/002/004/003/007`; response after retries returned unchanged (status 503 passes through); breaker open → `RZ-UP-005`; half-open with probe in flight → `RZ-UP-005`; bulkhead and waiters full → `RZ-UP-006`; `maxPendingRequests: 0` → immediate `RZ-UP-006`; empty set (static empty after DNS NXDOMAIN ×3) → `RZ-UP-008`; all-zero weights → `RZ-UP-008`; body error after commit → `RZ-UP-009` (stream reset observed by client); buffered response over 10 MiB → `RZ-UP-010`.
3. Retry conditions: non-replayable streamed body after bytes read (no retry), zero bytes read (retry), `Retry-After: 11` (no retry), `Retry-After: 2` beyond leg deadline (no retry), HTTP-date form, budget exhausted (counter increments, original failure returned), `retryOn` runtime error (no retry, metric), Filter-requested retry, attempts count (`attempts: 2` → 3 attempts), backoff bounds [0, min(250 ms, 25·2^(n−1))] with a fixed Rand, untried Endpoint preference, `onUpstreamRequest` rerun per attempt.
4. Deadlines: perTry default = time left ÷ (retries left + 1) at each attempt; leg timeout covers backoff; headers stop the attempt timer (slow body not cut by `perTryTimeout`); Route timeout before any attempt returns `RZ-RT-007` (area 4) and during a leg `RZ-UP-003`.
5. Default predicates equal their CEL forms over the full matrix of req 4.
6. Breaker state machine: consecutive and ratio conditions both required; below `minimumLegs` never opens; connect errors counted only above 50% ejected; open jitter within ±20%; half-open single probe; 3 successes close; one failure reopens; gated legs not counted; state carried across a Hot Reload.
7. Passive ejection: `consecutiveErrors` threshold, `ejectionTime × count` capped at 10, 50% cap with bypass for connect and suspect-timeout, decay; panic mode when all ejected (metrics and degraded reasons set and cleared).
8. Active probes: thresholds, jitter window, 200-399 success, timeout failure, new Endpoints healthy, probe slots computation and skipped counter, `probes_skipped` degraded after 1 minute > 10%.
9. Stall/suspect: attempt without headers at 1 s marks stalled; Endpoint suspect after 1 s without headers; skip while another remains; timeout on suspect ejects; 50% stalled rule.
10. Discovery: fake Resolver: A/AAAA to Endpoints, SRV weights and priorities, target resolution, refresh interval 27-33 s, failure keeps last set with `discovery_stale`, backoff 1-60 s full jitter, NXDOMAIN counted until 3 repeats then empty, recovery clears state; static host re-resolution keeps last good answer.
11. Balancers (table and property, `testing.F`): round-robin schedule proportions within one slot of n_i, max run length bounded (smoothness), size min(65,536, 64E), 3 Endpoints = 768 bytes; ring-hash identical across two builds from shuffled inputs (cross-Node consistency), key movement ≤ 1/E + ε when one Endpoint is added, vnode proportion within 5% of weight share at v = 1,024; P2C prefers the lower score; random distribution chi-square at p > 0.001 over 10^6 picks; exclusion fallbacks; budget planner formula incl. v = 244 for 1,000 rings of 64 Endpoints and fallback order.
12. Weighted leg pick: distribution 95/5 within tolerance; weight 0 never picked.
13. Upstream TLS: SNI/Host rules (req 29), CA replaces system roots, mTLS client certificate presented, certificate rotation reaches a new handshake, TLS 1.1 server refused, cleartext gauge.
14. Forwarding: hop-by-hop and `Connection`-named fields removed both ways, `Expect` dropped, trailers passed, normalized path escaped form, raw query unchanged, `traceparent` present, forwarding headers from area 4, no redirect following.
15. Composition: aggregate merge golden (target, select, rename, group, collection, later-wins), non-JSON body fails the step (`RZ-RT-015`), oversize step body (`RZ-RT-015`), buffer budget spent (`RZ-RT-004`), in-flight units unavailable (`RZ-RT-005`), optional failure → `ruralz-partial: true`, sibling cancellation on non-optional failure (upstream observes cancel), Upstream cause code and status, proposed `RZ-UP-011` non-2xx, sequential `steps` variable and skip on `when` false, pathExpression runtime error, conditional first-true and 404 `RZ-RT-010`, step query rule, body fan-out gate.
16. RateLimit decision path (fake store): `when` false; key error open/closed (`RZ-RL-005`); over-limit cache zero calls and `Retry-After`; first-seen within budget and past budget (local-only entry, 10 s expiry, own segment); local bucket deny `RZ-RL-001` with `Retry-After`; all-or-nothing token take across two limits; GCRA allow/deny `RZ-RL-002`; store failure open (fail-open window bounds) and closed `RZ-STS-001` (timeout), `-002` (error and NOSCRIPT), `-003` (breaker open), `-004` (deadline spent); `localOnly` never calls; declared and file-mode ceilings; derived ceiling formula and floor (unit test through a fake `NodeCount`).
17. Consumptive grouping: two ratelimits and a quota with equal key → one `script_multi` call; different keys in cluster topology → sequential calls stopping at first deny; local tokens undone after an earlier deny; per-Policy `failureMode` on shared failure.
18. RateLimit headers golden (exact bytes): single-limit and multi-limit Policies, Gateway plus Route stacking, `RZ-RL-001/002/003`, admitted-response fields per req 68, SF string escaping, rounding up of `w` and `t`.
19. Quota: 403 `RZ-RL-004` for null Consumer and missing quota name and wrong unit; key error open/closed; window start and neighbor selection at both halves; skew error → `failureMode`; `RZ-RL-003` with jitter bounds; denial cache TTL min(60 s, window end), raised limit bypasses cache; charged once for two Policies; refund on later Filter rejection, `RZ-UP-005/006/008`, not on cache hit; refund drop counter.
20. Response Cache: Cache-Control parser table (RFC 9111 examples, quoted values, duplicates, invalid numbers); storability table (each exclusion, `Authorization` rules, status list); freshness and Age computation; stale limits (`must-revalidate`, `proxy-revalidate`, `s-maxage`); request directives (`no-cache`, `max-age=0`, `no-store`, `only-if-cached` 504); partition rules; key derivation golden (U, P, V hex); Vary learning miss then hit; generation mismatch miss; MAC failure miss; hot layer promotion on second read within 1 s and 1 s expiry; coalescing reuse and fallback; SWR queue drop at 256 and lease loss; stale-if-error on `RZ-UP-003`, `RZ-UP-005` and 503; memory rules 2 and 3 skip reasons; HEAD from GET entry; `Cache-Status` strings golden.
21. `/debug/upstreams` golden JSON (sorted, no secrets).
22. Allocation gates (`testing.AllocsPerRun`): Upstream layer ≤ 6 Ruralz-owned allocations per pooled HTTP/1.1 attempt, `onRoute` pick 0, ratelimit local path and memory GCRA 0, `onUpstreamResponseHeaders` accounting 0 (PBB).

Property and fuzz targets (`testing.F`, seeds in `pr-fast`, long runs nightly):

23. `FuzzGCRAModel`: random arrival sequences, limits and bursts, deterministic clock: memory driver equals a reference model; admitted per window ≤ `requests` + `burst` (+1 edge) and sustained ≤ rate; all-or-nothing TAT updates (TQ "Required properties" Rate Limit).
24. `FuzzRateLimitFailOpen`: with the store failing, admissions per key per aligned window ≤ N × c_fo for N simulated Nodes (declared, file mode, derived).
25. `FuzzQuotaNoOverAdmission`: concurrent reservations on the memory driver never exceed `limit` per window.
26. `FuzzRetryBudget`: random concurrent legs never exceed max(3, 20%) retries in flight.
27. `FuzzBreakerTransitions`: every transition is one of Figure 2's edges.
28. Fuzz parsers: `ParseCacheControl`, `Retry-After`, query normalization for U, Vary value normalization, SRV answer handling, `sfv` serializer (output reparses per RFC 9651 grammar), composition `Merge` on arbitrary JSON (never panics; oversize bounded at 4×), entry codec `UnmarshalEntry` (corrupt bytes rejected).

Integration tests (`integration` tag, local redis-server 7.0.15; CI adds Redis 8, Valkey, Dragonfly):

29. GCRA script: allow/deny sequences equal the model for 1, 2 and 3 limits; τ = window admits 2 × `requests` in one window; burst 0 admits exactly one per T; deny leaves every TAT unchanged; `PEXPIRE` drops idle keys; server `TIME` used (Node clock skewed ±30 s has no effect).
30. `NOSCRIPT`: `SCRIPT FLUSH` → the next request applies `failureMode` with exactly one command sent (no `EVAL`), the reload happens off-path, the following request's `EVALSHA` succeeds (TQ "Integration tests").
31. Cluster (three local instances): same-slot consumptive calls run as one script; different slots run sequentially and stop at the first deny; never `CROSSSLOT`; cache generation and partition keys in different slots pipelined.
32. Quota script across a window boundary and with Node-supplied neighbor keys; skew error reply; refund script.
33. Cache scripts: lease contention (second store skipped), 8-variant cap and expiry pruning, names change drops variants, generation bump monotonic under a lowered `TIME` simulation (value + 1), 304 metadata refresh under the lease, variant delete on non-304.
34. Round-trip test (ADR8 "Confirmation"): a counting TCP proxy between Node and redis-server asserts zero commands for local denials and over-limit and denial-cache hits, at most one blocking command per admitted request per Policy, one shared script when consumptive keys share a slot, and exactly two pipelined commands per cache lookup.
35. End-to-end Upstream tests with `httptest` servers (HTTP/1.1 cleartext, HTTP/2 over TLS with ALPN): retries across Endpoints, ejection and recovery, breaker opening under 20+ failing legs, bulkhead saturation, panic mode, weighted split, composition modes, pooled connection reuse ≥ 99.9% after warm-up (PBB), carry-over of breaker and ejection state across a Hot Reload, pool carry-over when the key is unchanged.
36. Real DNS: a minimal stdlib UDP DNS responder in test code answering A, AAAA, SRV and NXDOMAIN (wired through `net.Resolver{PreferGo: true, Dial}`) to exercise req 6-7 end to end.

Chaos and benchmarks (nightly; OQ-scalability-and-distributed-state-10 (a) TCP fault proxy):

37. CE-3 (200 ms added per State Store call), CE-4 (60 s black-hole: per-key admissions within fail-open bounds; `closed` gets `RZ-STS-001` then `RZ-STS-003`), CE-5 (primary killed: reconnect ≤ 10 s, `failureMode` ≤ 15 s), CE-6 (fill to `maxmemory`: stores skip above 70%, no limit key evicted), CE-12 (hot key: GCRA calls ≤ 2 × N × c per window; none with `localOnly`), CE-15 (100,000 new keys per second: per key ≤ N × c, first-seen calls within budget, no GCRA-answered key evicted), CE-16 (cache stores saturating the queue: zero `cache_invalidate` drops), and TQ "Upstream Endpoints reset or slowed" (ejection, budgeted retries, breaker, `RZ-UP` codes). Locally: 1 Node plus redis-server; CI: 10 Nodes.
38. Benchmarks with gates: `BenchmarkLegPooled` (Upstream layer 12/50 µs, 6 allocations), `BenchmarkPick{RoundRobin,LeastRequest,RingHash,Random}`, `BenchmarkRateLimitLocal` and `...MemoryGCRA` (2/8 µs, 0 allocations), `BenchmarkBreakerAccounting` (2/8 µs), S5/S5x (GCRA p99 1 ms same-zone, 2 ms cross-zone, hypothesis), O2 (8,192 in-flight State Store calls plateau), G1 (per-shard GCRA calls at 70% script CPU).

Every error code path is covered: RZ-UP-001..010 (tests 1-2, 15, 35), proposed RZ-UP-011 (15), RZ-RL-001..005 (16, 18, 19), RZ-STS-001..004 through this area's Filters (16, 19, 20, 30, 37), RZ-RT-004/005/010/015 in composition (15), RZ-RT-007 boundary (4), the cache `only-if-cached` 504 (20), and validation codes RZ-CFG-005/009/014/015/020/031/032/038 (fixtures supplied to the configuration conformance suite).

## 7. Open questions blocking M1 in this area

| ID | Question | Adopt | What the code does |
|---|---|---|---|
| OQ-traffic-management-and-resilience-2 | How do admitted responses get RateLimit fields? | (c) Data plane appends them (no option marked; aligned with area 2) | Filters record fields in `consume.Fields`; area 4 appends after `onResponse`; 429s carry them from the Filter; registry Phases unchanged (req 68) |
| OQ-traffic-management-and-resilience-5 (M1 part) | Backoff, budget, hedging, breaker guard fields | (a) proposed: `circuitBreaker.minimumLegs` (with `failureRatio`, `halfOpenSuccesses`) registered; backoff and budget stay fixed; `hedgeDelay` M4 | Breaker reads the three fields with defaults 20 / 0.5 / 3; backoff 25-250 ms and budget max(3, 20%) are constants in one `resilience` defaults struct |
| OQ-traffic-management-and-resilience-6 | Deadline defaults; which timeouts are fields | (c) per-protocol Route timeouts with (a)'s values, no (d)/(e) fields | Static defaults as schema markers (area 2); Route `timeout` 15 s, Upstream `timeout`, `perTryTimeout`, `retryOn` as runtime rules (req 4); dial, TLS, ping, stall fixed |
| OQ-traffic-management-and-resilience-11 | Response Cache isolation; unsafe-method invalidation; `s-maxage` stale | (a) separate State Store (recommended), plus the interim rules always on; all unsafe methods invalidate; `s-maxage` as `proxy-revalidate` (current) | Cache uses the cache handle when configured (req 90), else the main store with rules 1-3 and a startup warning; invalidation per req 85; stale forbidden per req 78 |
| OQ-traffic-management-and-resilience-16 | Ceiling derivation and fail-open refill | (b) 2× headroom, floor 10, aligned-window step refill (recommended) | Derived ceiling formula and fail-open windows (reqs 59, 61); dormant in file mode except fail-open windows |
| OQ-traffic-management-and-resilience-19 | Node count source; Node without a count | (c) handover and persistence under `${RURALZ_DATA_DIR}` (recommended) | M1 file mode: `NodeCount` returns unknown and file mode; full-limit ceiling, 200/s first-seen, not degraded; persistence and handover arrive with Control mode (M2) |
| OQ-traffic-management-and-resilience-20 | First-seen keys past the budget | (a) local-only entries at the per-Node ceiling in their own segment (proposed) | Req 58 step 3; option (c) kept as a one-constant switch until OQ-scalability-and-distributed-state-11 amends pack 8.8 |
| OQ-traffic-management-and-resilience-21 | `cache` with `onRequestBody` authz/validation/Plugin auth | (a) reject with RZ-CFG-038 (proposed; area 2 R-39) | Validation error; no runtime path |
| OQ-scalability-and-distributed-state-2 | `redis` topologies | (a) standalone and cluster, no Sentinel (answered in CM "Gateway") | Hash-tagged keys; slot grouping for consumptive calls; no `CROSSSLOT` |
| OQ-scalability-and-distributed-state-3 (M1 part) | Second State Store deployment for caches | (a) a cache connection under `Gateway.spec.stateStore` | `cache.Factory.Store` = cache handle when present (field name from area 1/2) |
| OQ-scalability-and-distributed-state-10 | Chaos tooling | (a) TCP fault proxy and signals in the harness | CE-3..6, 12, 15, 16 harness (test 37) |
| OQ-scalability-and-distributed-state-11 | Pack 8.7/8.8/8.11 amendments (-16 (b), -20 (a), -19 (c), `localOnly`, `rzplg:`) | (a) all (recommended) | `localOnly` implemented (req 65); local-only entries; derived formula |
| OQ-configuration-model-8 (RM criterion 7) | Default `failureMode` of `quota` (open) and `ai.token-budget` (closed) | (a) as registered (TMR recommends) | `quota` fails open; token budgets are M3 |
| OQ-security-and-identity-22 | Secret and Node-connection restrictions, State Store entry MAC | (a) proposed | Dialer refuses req 9 ranges unless `RURALZ_FETCH_ALLOW`; proxies ignored; cache entries carry an HMAC when a key is configured |
| OQ-data-plane-2 | Wildcard host registration | (a) one leading `*.` label (answered in CM) | Router area; this area's routing tests use it |
| OQ-data-plane-9 | Area of Node-generated failures (RZ-RT-011..015, RZ-UP-010) | (a) amend pack 8.6 `RT` (current) | Composition uses `RZ-RT-010`/`RZ-RT-015`; `RZ-UP-010` stays in `UP` |

Non-blocking questions this area resolves by their current option: OQ-traffic-management-and-resilience-3 (IETF fields only), -4 (one Policy per Tier), -7 and OQ-data-plane-7 (fixed 50% cap, all-or-nothing panic), -8 (no locality or slow start), -9 (fixed 30 s refresh), -10 (fixed windows), -12 (no mirroring; M2), -13 (composition `path`), -14 (no Tier shedding), -15 (standard-library hash), -17 (header/cookie canaries only), -18 (no IPv6 prefix keys), -22 (no handover of key table, ZDT), -23 (warning only); OQ-multi-protocol-12 (b) (HTTP/1.1 cleartext, HTTP/2 by ALPN); OQ-scalability-and-distributed-state-1 (per-request GCRA), -8 (no session affinity beyond `ring-hash`); OQ-security-and-identity-25 (no sharing field; see section 9 item 1). Benchmark gate questions (OQ-performance-budgets-and-benchmarking-1, -2; OQ-testing-and-quality-strategy-2) gate S5/G1 publication but not code.

## 8. Deferred

M2+ items near this area that MUST NOT be built in M1, with the extension point M1 code leaves for each.

| Deferred | Milestone | Extension point to leave |
|---|---|---|
| `kubernetes` discovery (EndpointSlices, client-go, OQ-tech-stack-and-libraries-23) | M2 | `discovery.Spec` variant and a `Source` interface producing `Set`; zero-serving-Endpoint slice empties at once (`RZ-UP-008`) |
| Node count from `HeartbeatReply.clusterNodeCount`, `node_count_unknown`, count persistence (OQ-19 (c)), `state_store_memory_multi_node` warning | M2 | `ratelimit.NodeCount`; derived-ceiling and countless clamp code paths already unit-tested |
| `grpc`, `graphql`, `websocket`, `ai` Upstreams; `grpc-timeout`; gRPC health (`grpc.health.v1`); stream Route timeout 1 h; `onChunk`; SSE flushing; WebSocket slots for life | M3 | `Compiled.Protocol` dispatch in the Forwarder; `Hooks` gains chunk hooks; bulkhead already holds slots until body close; `Route.timeout` default computed by a per-protocol function |
| Hedging (`hedgeDelay`) | M4 | Attempt loop structured as "start attempt, wait result" so a second concurrent attempt can be raced; hedges spend retry budget |
| Route mirroring (OQ-12), built-in redirects and plain-`upstreams` rewrites (OQ-13) | M2 | Leg executor accepts a fire-and-forget leg later; `RoutePlan` can carry a rewrite |
| `plugin` Policies on upstream legs (`auth.upstream-sigv4`, Plugin phases) | M2 | `Hooks` is Filter-agnostic |
| `ai.token-budget` reservations sharing the consumptive script and `rz:qt:` counters; `ai.semantic-cache` | M3 | `statestore.ConsumeOp` kind enum and `consume.Member`; cache key partition helpers |
| Multi-region Quota patterns, leases (OQ-scalability-and-distributed-state-1, -5) | M4 / not planned | Keys already per Cell |
| Locality-aware balancing, slow start (OQ-8), percentage-sticky splits (OQ-17), Tier-priority shedding (OQ-14), IPv6 prefix keys (OQ-18), `X-RateLimit-*` (OQ-3), weighted quota costs and calendar months (OQ-10), `Upstream`-scoped `ratelimit` (OQ-feature-catalog-4) | Not scheduled | `balance.Picker` interface; `consume.Fields` output switch; `QuotaReserveOp.Cost` field reserved as 1 |
| h2c or HTTP/3 to Upstreams (OQ-multi-protocol-12) | Undecided | Transport factory keyed by protocol |
| Client-side caching in rueidis | Not in M1 (off) | none |

## 9. Risks and ambiguities

| # | Issue | Recommended resolution |
|---|---|---|
| 1 | Cache principal: TMR says `auth.*` Routes key per principal "unless `config.key` replaces it"; SEC and the AI doc say `config.key` only adds dimensions and sharing needs OQ-security-and-identity-25's field. TMR's `cache-catalog` example shares per Tier. | Follow the owning document (TMR): `config.key` replaces the principal (req 79); ask SEC to align or close OQ-security-and-identity-25 before 0.1.0 |
| 2 | DP says a non-2xx result fails a step, which turns a `conditional` single-step URL rewrite's 404 into a 502, contradicting FC "Error pass-through"; no code exists for "step got non-2xx" | Req 50: pass through for the response-producing unmerged step; register `RZ-UP-011` (502) in TMR and `internal/errcode` |
| 3 | DP: a failed step returns "502 with the `RZ-UP-<NNN>` code", but `RZ-UP-003/005/006/008` are 504/503 | Use the code's registered status (req 49) |
| 4 | `only-if-cached` miss must be 504 (RFC 9111) but no RZ code fits | Register a new RT code (data-plane owns; coordinate numbering with area 4's proposed RZ-RT-016/017, e.g. RZ-RT-018 "only-if-cached request missed the Response Cache", 504) |
| 5 | Upstream `Host` header unspecified | Req 29 (Endpoint authority or `sni`; client host in `X-Forwarded-Host`); a `Upstream.spec.host`-style field would need a CM Open question |
| 6 | Query handling of composition steps unspecified | Req 54 |
| 7 | `request.query` type unspecified in CM | Area 3's map(string,string) joined by `","` |
| 8 | `ratelimit` `config.key` unset: semantics unspecified | Constant `""` (service limit) |
| 9 | `healthCheck.active` field defaults unspecified | Req 4 proposed values, registered as schema markers with area 2 |
| 10 | Ejection-count decay unspecified | Decrement per `ejectionTime` not ejected (req 18) |
| 11 | SRV priority and weight-0 handling unspecified | Lowest-priority group only; weight 0 per req 6 |
| 12 | Per-request State Store deadline origin and the timeout of a shared consumptive script | Origin = start of `onRequestHeaders` (area 4 req 45); shared call uses the smallest participant timeout (req 64) |
| 13 | Gateway `stateStore.timeout` default unspecified (CM example 50 ms; SDS 5-10 ms for latency-sensitive Policies) | State Store area decides; blocks CE-3 expectations |
| 14 | Rule 3 with N_c = 1,000 in file mode caps each Node at ~34 KB/s per 16 GiB shard, so a single-Node file-mode cache barely stores; `maxmemory 0` makes rules 2-3 undefined | Accept for M1 (safety first), document, skip stores when `maxmemory` is 0; push OQ-11 (a) (separate store, where rules could relax) or (c)'s declared count |
| 15 | TMR applies aligned-window fail-open refill to derived ceilings; ADR8 to all local buckets (declared too) and lists it as a pending pack 8.8 amendment | Apply to all (req 61), per ADR8 |
| 16 | GCRA edge: classic GCRA admits `burst` + 1 at once; TMR says a window admits `requests` + `burst` | Keep classic GCRA (burst 0 must admit 1 per T); document the +1 edge in the property test |
| 17 | Key encodings (`<window>`, `<window start>`, cache digests) are unspecified yet become layout (RVC "State Store layout changes") | Decide before 0.1.0: canonical Duration string, Unix ms, lowercase hex |
| 18 | TMR's cache lookup "partition-slot script" vs HMGET | HMGET is read-only and equivalent; update TMR wording |
| 19 | `RURALZ_FETCH_ALLOW` (OQ-security-and-identity-22) refuses loopback Upstreams, breaking `ruralz dev run` against a local service and T1 quickstarts; its format is unspecified | Define it as a comma-separated CIDR list; `ruralz dev run` sets it for loopback; document in the quickstart |
| 20 | HTTP/2 ping-close and stream-reset errors are unexported inside `net/http` (moved to `net/http/internal/http2` in Go 1.27), so classification needs string matching | Isolate matching in `Classify` with tests pinned per toolchain (floor 1.26 and 1.27) |
| 21 | rueidis v1.0.78 requires `golang.org/x/sys v0.47.0` and pulls `gomega` into the module graph | Pin `x/sys` at the catalog's v0.48.x via MVS; confirm depgate/license gate treat test-only requirements |
| 22 | Local tests only have Redis 7.0.15; Dragonfly's `TIME`-in-script and strict key declaration, Redis 8 and Valkey behavior are CI-only | Keep scripts to Redis 7.0 commands; gate multi-server matrix in `pr-full` |
| 23 | M1 Revisions with `kubernetes` discovery or non-`http` protocols: no activation code is defined | Refuse with RZ-CFG-024 semantics ("field newer than the oldest target Node serves") or a new CFG code; area 1/2 decide |
| 24 | All `upstreams` weights 0 is schema-valid | 503 `RZ-UP-008` plus a validation warning |
| 25 | A retry that cannot start (breaker, bulkhead) has no stated outcome | Return the previous attempt's outcome (req 35) |
| 26 | "Replayable" excludes streamed bodies even when a connect error consumed nothing | Treat zero-bytes-read streams as replayable (req 31); confirm with DP owner |
| 27 | `ruralz_upstream_pool_connections{state}` is not observable from `http.Transport` | Count via a dial wrapper and `httptrace.GotConn`/`PutIdleConn`; document HTTP/2 approximation |
| 28 | `CircuitBreaker.MaxConnections` Go doc says "caps connections per Endpoint set" while TMR defines in-flight attempts | Fix the doc comment in `pkg/config/v1alpha1` (area 2) |
| 29 | Entry MAC key source for OQ-security-and-identity-22 is unspecified | Optional key via a `RURALZ_SECRET_*` file; no MAC when unset |
| 30 | Revalidation replay with client credentials stored in the State Store (T10) | Strip credential headers from the saved request (req 86); some upstreams then answer 401 and end the stale window |
| 31 | `when` on `aggregate` steps is outside CM's stated modes | Evaluate it (req 43) or have area 2 warn |
| 32 | Admitted RateLimit fields (OQ-2 (c)) rely on an area 4 hook not yet in its spec | Add the hook in area 4; fall back to (b) if it slips |
| 33 | OQ-20 (a) and `localOnly` depend on a pack 8.8 amendment (OQ-scalability-and-distributed-state-11) that may stall (RM R-5) | Keep option (c) as a single-constant switch; `localOnly` rejected at validation if the amendment is refused |
| 34 | `HTTP2Config.PingTimeout` reading of "closes it after 5 s unanswered" | 5 s after the ping (close within 6 s of silence); pin in test 1 |
| 35 | Upstream idle connection timeout unspecified | 90 s (req 27); Go retries unwritten requests on reused-but-closed connections, which is not an extra attempt |
| 36 | T = `window` ÷ `requests` below 1 µs for extreme limits | Clamp at 1 µs and warn at validation |
| 37 | CEL area recommends materializing default `retryOn` as a schema default; area 2 keeps it a runtime rule | Area 2's rule (runtime, `Compiler.Default`); equality test 5 keeps both forms identical |
