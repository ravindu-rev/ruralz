# M1 spec, area 8 of 11: State Store

Design reader output for milestone M1 "Core gateway". Normative sources are the reviewed documents below; where they are silent this spec marks a value or mechanism **(proposed)** and lists it again in section 9. Values tagged (target) or (hypothesis) keep the documents' tags.

Document abbreviations used in references:

| Abbrev. | File |
|---|---|
| PACK | `docs/_meta/foundation-pack.md` |
| SYS | `docs/architecture/01-system-overview.md` |
| D02 | `docs/architecture/02-configuration-model.md` |
| D03 | `docs/architecture/03-data-plane.md` |
| D05 | `docs/architecture/05-wasm-plugin-system.md` |
| D08 | `docs/architecture/08-security-and-identity.md` |
| D09 | `docs/architecture/09-traffic-management-and-resilience.md` |
| D10 | `docs/architecture/10-observability.md` |
| D11 | `docs/architecture/11-scalability-and-distributed-state.md` |
| D12 | `docs/architecture/12-performance-budgets-and-benchmarking.md` |
| ADR8 | `docs/adr/0008-rate-limiting-local-bucket-and-gcra.md` |
| TECH | `docs/engineering/01-tech-stack-and-libraries.md` |
| LAYOUT | `docs/engineering/02-repository-layout-and-conventions.md` |
| TEST | `docs/engineering/03-testing-and-quality-strategy.md` |
| REL | `docs/engineering/04-release-versioning-and-compatibility.md` |
| ZDU | `docs/operations/02-zero-downtime-upgrades-and-hot-reload.md` |
| CAP | `docs/operations/03-capacity-planning.md` |
| HA | `docs/operations/04-high-availability-and-disaster-recovery.md` |
| ROAD | `docs/roadmap/01-roadmap-and-milestones.md` |

## 1. Scope

Every M1 roadmap item that lands in this area, with its source. ROAD "M1 scope" is the authoritative list; the State Store area owns the storage side of each item, while Policy semantics stay with their feature areas (section 4).

| # | M1 item (State Store side) | Source |
|---|---|---|
| S1 | `memory` and `redis` drivers, tested on Valkey 9.0.1 or newer and Dragonfly (plus Redis 8 per TEST) | ROAD "M1 scope" (Traffic and state); PACK §7 "Other defaults"; TEST "Integration tests" |
| S2 | `redis` topologies `standalone` and `cluster` via `Gateway.spec.stateStore.topology`, rueidis URL forms, `ForceSingleClient`, `ShardsRefreshInterval` 10 s, RZ-CFG-026 on a non-fitting URL | D02 "Gateway"; D11 "Open questions" OQ-scalability-and-distributed-state-2 |
| S3 | State client on rueidis: auto-pipelining, 2 pipelined plus up to 8 dedicated connections per shard, 8,192 in-flight ceiling, per-shard breaker, reconnect pacing | D11 "State client and RZ-STS error codes"; D11 "Per-Node limits" |
| S4 | RZ-STS-001 to RZ-STS-005 and their status per pack 8.10 | D11 "State client and RZ-STS error codes"; PACK §8.6, §8.10 |
| S5 | Per-call `stateStoreTimeout`, per-request deadline, one blocking round trip per Policy, consumptive merge (`script_multi`), read pipelining, post-commit writes | PACK §8.7; SYS "State Store access"; D09 "Deadlines" |
| S6 | Distributed rate limiting with State Store-backed counters: GCRA as one `EVALSHA` with server `TIME`, `SCRIPT LOAD` at connect/reconnect/failover, `NOSCRIPT` applies `failureMode` and reloads off-path | ROAD "M1 scope" (Traffic and state); ADR8 "Decision outcome"; D09 "Decision path"; TECH "Library catalog" (State Store client, Rate limiting rows) |
| S7 | `quota` in `requests`: atomic reservation script, epoch-aligned windows, refund at `onLog` | ROAD "M1 scope"; D09 "Quotas" |
| S8 | `cache` (Response Cache) storage operations: pipelined lookup, leased store, generation bump, revalidation lease | ROAD "M1 scope"; D09 "Response caching" |
| S9 | Key layout `rz:rl:`, `rz:qt:`, `rz:rc:` with hash tags; `rzplg:` reserved | ADR8 "Decision outcome" (Keys); D09 "Decision path", "Quotas", "Response caching"; D11 "Distributed state catalog" |
| S10 | Post-commit write queue (four classes, eight writers, drop counters, Drain flush) | D11 "Post-commit write queue" (Planned (M1)); PACK §8.7 rule 4; D03 "Goroutines" |
| S11 | State Store availability checks: `noeviction` detection, `INFO memory` readings per shard, store byte budgets for the Response Cache | D11 "State Store availability", "Distributed caches"; D09 "Response caching" rules 2 and 3 |
| S12 | TLS and credentials to the State Store (TB-4), cleartext and unauthenticated reporting | ROAD "M1 scope" (Security: "TLS to ... State Store"); D08 "Transport security"; SYS TB-4 |
| S13 | Degraded reasons `state_store_breaker_open`, `state_store_eviction_policy`, `state_store_memory_fallback`, `state_store_unauthenticated`, `cleartext_hop` (State Store hop) and the `ruralz_state_*` metrics | ROAD "M1 scope" (Observability); D10 "Ruralz Gateway metrics", "Degraded states" |
| S14 | Fallback: `RURALZ_STATE_STORE_URL`, else `memory` with a startup warning | D02 "Gateway" |
| S15 | Hot Reload of `Gateway.spec.stateStore` as a State Store upgrade | ZDU "Hot Reload versus restart", "State Store upgrades" |
| S16 | Single-Region Cells (per-Node `secretRef` URL; no cross-Region dial) and per-Cell accuracy bounds | ROAD "M1 scope"; D11 "Multi-region", "Cells and blast radius", "Consistency and accuracy bounds"; PACK §8.13 |
| S17 | Quality: integration tests, round-trip test, chaos CE-3 to CE-6 (State Store side of CE-1, CE-12, CE-15, CE-16), GD-2 to GD-5 | ROAD "M1 scope" (Quality) and "M1 exit criteria" 5; ADR8 "Confirmation"; D11 "Chaos experiments"; HA GD table |
| S18 | Budgets: PB-9, PB-14, S5, S5x, G1, O2, and the `ratelimit` stage budget on the `memory` driver (0 allocations) | ROAD "M1 scope" (Budgets); D12 "Per-stage latency budget", "Throughput targets", scenario tables |
| S19 | Closing the M1-blocking open questions of section 7 | ROAD "M1 exit criteria" 7 |

Not in this area although nearby: local token buckets, over-limit cache, first-seen budget, per-Node ceilings, quota denial cache, RateLimit headers, Response Cache HTTP semantics and hot-entry layer (Traffic and resilience area); Filter Chain executor, failureMode application and error body (Data plane area); `auth.basic` throttling, which is local to each Node and never calls the State Store (D08 "Pre-authentication throttling", D08 "Client authentication": "never call the State Store").

## 2. Normative requirements

### 2.1 Drivers and configuration

1. There MUST be exactly two drivers, `memory` and `redis`; another needs an ADR. (PACK §7 "Other defaults")
2. Effective configuration MUST resolve in this order: (a) `Gateway.spec.stateStore` present: `driver` default `memory`, `topology` default `standalone` (`redis` only), `url` a `SecretValue`, `timeout` the Policies' default; (b) `spec.stateStore` absent and `RURALZ_STATE_STORE_URL` set: driver `redis`, topology `standalone`, URL from the variable; (c) neither: driver `memory`, a startup WARN log stating that `memory` multiplies every Rate Limit and Quota by the Node count, and degraded reason `state_store_memory_fallback` raised. (D02 "Gateway"; D10 "Degraded states")
3. `state_store_memory_fallback` MUST be raised only in case 2(c) while the Node count is unknown (always in M1 file mode); an explicit `driver: memory` logs the same WARN once per activation but is not degraded **(proposed)**. (D10 "Degraded states": "No State Store, Node count unknown")
4. `driver: redis` without `url` MUST use `RURALZ_STATE_STORE_URL`; if that is unset the Node rejects the Revision with RZ-CFG-026 **(proposed)**. `driver: memory` with `url` or `topology` set logs a WARN and ignores them **(proposed)**.
5. `stateStore.url` MUST be resolved on each Node by the `secretRef` providers (`env`, `file` in M1). An unresolvable reference is RZ-CFG-026: the Node rejects the Revision and keeps its active one. The resolved value MUST NOT appear in a Revision, diff, Last-Known-Good, `/config/dump`, log, trace, metric label or `/tap`. (D08 "Secrets" rules 1 to 4; D02 "Gateway")
6. `Gateway.spec.stateStore.timeout` is the default of every `Policy.spec.stateStoreTimeout`. The documents give no default for `timeout`; when unset the Node MUST use 50 ms **(proposed; every document example uses 50ms, D02 "Gateway", D05 brownout row)**. A resolved timeout of 0 means every call is not attempted (RZ-STS-004).
7. URL syntax is the rueidis form, parsed with `rueidis.ParseURL` and then checked by Ruralz (D02 "Gateway"):
   - `standalone`: one endpoint `rediss://[user:password@]host:port[/db]`; `db` from the path or `?db=`.
   - `cluster`: a seed list `rediss://host:port?addr=host:port&addr=host:port`; `db` MUST be absent or 0 **(proposed; Redis Cluster serves only database 0)**.
   - RZ-CFG-026 on the Node for any resolved URL that does not fit `topology`: `master_set` (no Sentinel, OQ-scalability-and-distributed-state-2 (a)), `addr` under `standalone` (D02 "Gateway").
   - Also RZ-CFG-026 **(proposed)**: scheme other than `redis`, `rediss` (and their rueidis aliases `valkey`, `valkeys`); `unix` scheme; `skip_verify` (no field disables certificate verification, D08 "Transport security"); `protocol=2`, `client_cache`, `max_retries`, `dial_timeout`, `write_timeout` (client bounds are fixed, OQ-scalability-and-distributed-state-4 (a)). Accepted query parameters: `addr` (cluster only), `db` (standalone only), `client_name`.
   - Diagnostics name the rule and the topology, never the URL, user name or password.
8. Credentials (TB-4): a non-loopback URL without credentials (no user name and no password) MUST be rejected; until OQ-security-and-identity-30 assigns a code the Node uses RZ-CFG-026 **(proposed interim, option (b))**. A loopback URL (literal `127.0.0.0/8`, `::1`, or `localhost`) without credentials is accepted and raises `state_store_unauthenticated`. Loopback is decided on the literal host, never on DNS results **(proposed)**. (D08 "Transport security"; D10 "Degraded states")
9. Transport: `rediss://` MUST use TLS 1.2 or newer (rueidis sets `MinVersion: tls.VersionTLS12`), verify the server certificate against the system roots, and never set `InsecureSkipVerify`. `redis://` is allowed as cleartext: gauge `ruralz_security_cleartext_hops{hop="state_store"}` = 1 while the active State Store is cleartext, which raises `cleartext_hop`. (D08 "Transport security"; D10 metrics table and "Degraded states")
10. TLS server name: the URL host for the seed; for cluster nodes discovered by `CLUSTER SLOTS`, the announced host when it is a name, else the seed host **(proposed; rueidis passes one `TLSConfig` to every node, section 9)**.
11. Readiness MUST NOT depend on the State Store: activation never waits on a dial, a lost State Store never fails `/readyz`. (PACK §8.5; D11 "L4 load balancing"; HA FC-10)
12. Cells: each Node resolves its own URL through `secretRef`, so Regions share one Revision; the Node never dials a State Store of another Region and Ruralz never replicates State Store data between Cells; detecting a Cluster resolving different stores is operator discipline (OQ-scalability-and-distributed-state-9 (a)); no code check. (PACK §8.13; D11 "Cells and blast radius", "Multi-region")

### 2.2 State client (redis driver)

13. The State client is `github.com/redis/rueidis` with auto-pipelining, imported only in `internal/statestore/redis` (depguard rule `rueidis` already in `.golangci.yml`). (D11 "State client and RZ-STS error codes"; LAYOUT "Where Go code goes")
14. rueidis `ClientOption` values MUST be:

    | Field | Value | Source |
    |---|---|---|
    | `ForceSingleClient` | `true` for `standalone` | D02 "Gateway" |
    | `ClusterOption.ShardsRefreshInterval` | 10 s (target) for `cluster` | D02 "Gateway" |
    | `DisableCache` | `true` (client-side caching off) | D11 "Distributed caches"; TECH State Store client row |
    | `DisableRetry` | `true` (a retry is a second round trip) | PACK §8.7 rule 1 |
    | `PipelineMultiplex` | 1 (2^1 = 2 pipelined connections per shard) | D11 "State client" |
    | `BlockingPoolSize` | 8 (dedicated connections, opened lazily) | D11 "State client" |
    | `BlockingPoolCleanup` | 60 s **(proposed)** | lazily opened, so idle ones close |
    | `AlwaysRESP2`, `DisableAutoPipelining` | `false` | RESP3, auto-pipelining |
    | `DialCtxFn` | the Ruralz dialer (requirements 22 to 24) | pacing, TLS, events |
    | `Dialer.Timeout`; TLS handshake | 1 s; 2 s **(proposed, mirrors D09 "Deadlines" dial row)** | rueidis default is 5 s |
    | `Dialer.KeepAlive` | 1 s (rueidis default) | |
    | `ConnWriteTimeout` | 2 s **(proposed)** | black-hole detection so CE-5 reconnects within 10 s |
    | `RingScaleEachConn` | 12 **(proposed)**: 2 × 4,096 queued commands per shard cover the 8,192 ceiling | |
    | `ReadBufferEachConn`, `WriteBufferEachConn` | 128 KiB **(proposed)**: one maximal cache entry (D09 "Response caching" Size) instead of 0.5 MiB | |
    | `ClusterOption.MaxMovedRedirections` | 3 **(proposed)**: bounds redirect loops | |
    | `ClientName` | `ruralzd-<node.id>` **(proposed)** | |
    | `SelectDB` | from the URL | |

15. Scripts MUST be sent only as raw `EVALSHA` through `Client.Do` or `DoMulti`; never `rueidis.NewLuaScript`/`Lua.Exec` and never `EVAL`, whose retry on `NOSCRIPT` is a second round trip. (ADR8 "Decision outcome" Code row; TECH Rate limiting row)
16. Nodes MUST `SCRIPT LOAD` the script library on every primary at connect, reconnect and failover (a new primary seen in the slot map or a new connection), off the request path, single-flight per shard. (D11 "State Store availability" MUST (Node); ADR8 "One round trip")
17. A `NOSCRIPT` reply MUST fail that call with RZ-STS-002; the Policy applies its `failureMode`; the driver schedules one reload per shard (single-flight); the request never makes a second call. Under `closed` each Node returns 503 RZ-STS-002 until its reload. (PACK §8.7 rule 1; D09 "Decision path" 5; ADR8 "Consequences"; TEST "Integration tests")
18. In-flight ceiling: at most 8,192 blocking calls in flight per Node (target), Node-wide across every live State Store; each call counts from send until its reply or a connection reset, never until its caller's timeout; an excess call is not attempted: RZ-STS-004, result `skipped`, Policy `failureMode`. Post-commit writes and maintenance commands do not count **(proposed)**. (D11 "State client"; D11 "Per-Node limits"; D12 O2)
19. Per-shard breaker, one per shard per Node (target values, D11 "State client"):
    - Closed: opens when at least 50% of at least 20 calls completed in a sliding 5 s window failed, or after 5 consecutive connect failures to that shard.
    - Open: every call to the shard is skipped with RZ-STS-003 (result `skipped`) without waiting; after a uniformly random delay in [1 s, 3 s] it becomes half-open.
    - Half-open: admits one probe call at a time (others RZ-STS-003); closes after 3 consecutive successful probes; any failed probe reopens it with a new random delay.
    - Breakers start closed after a restart; failing calls reopen them within 5 s (target) (D11 "Stateless data plane").
    - When no request probe arrives for 1 s in half-open, the driver sends `PING` as the probe **(proposed; without it an idle shard never closes and `state_store_breaker_open` pages)**.
20. Failures counted by the breaker: caller timeouts (recorded at the timeout), connection errors, error replies (including `NOSCRIPT`, `OOM`, `NOPERM`, `WRONGTYPE`, `CLUSTERDOWN`), unparsable replies. Not counted: denials, not-attempted calls, breaker skips. Post-commit write outcomes count **(proposed)**. Connect failures are dial or TLS handshake errors seen by the Ruralz dialer.
21. Degraded reason `state_store_breaker_open` MUST be 1 while any shard breaker of any live State Store is open or half-open **(proposed: half-open included)**, else 0. (D10 "Degraded states"; HA FC-10)
22. Reconnect pacing per shard per Node: full-jitter backoff, delay = uniform(0, min(5 s, 100 ms × 2^n)) after n consecutive connect failures, reset on success; and at most 4 connects in any sliding 1 s window (target). A dial refused by pacing fails at once without touching the network and does not count as a connect failure. (D11 "State client"; HA "State Store rebuild" step 5)
23. Shards: `standalone` has one shard, the URL endpoint. `cluster` shards are the primaries of the slot map read by the driver with `CLUSTER SLOTS` at start, every 10 s and after a connection close; breaker, pacing, `SCRIPT LOAD` and `INFO memory` are keyed by the primary's `host:port` **(proposed mechanism: rueidis does not export its slot map, section 9)**. Before the first slot map, every call maps to one pseudo-shard for the seed list.
24. rueidis cluster construction fails when no seed answers; the driver MUST then keep retrying construction in the background under the pacing of requirement 22, failing calls meanwhile with RZ-STS-002 on the pseudo-shard (so its breaker opens) **(proposed)**. A `standalone` client with `ForceSingleClient` is returned even when its first dial fails.
25. Cluster `MOVED` and `ASK` are followed inside the same call by rueidis, bounded by the caller's timeout and `MaxMovedRedirections`; promoted replicas and new shards need no Revision. (D02 "Gateway")
26. Keys never cross slots in one command: the driver MUST verify that every key of a command shares one slot before building it (the rueidis builder panics on cross-slot keys) and never produce `CROSSSLOT`. (TEST "Integration tests" cluster row)

### 2.3 Round trips and deadlines

27. Before commit each Policy makes at most one blocking State Store round trip per request. (PACK §8.7 rule 1; D11 SG-2)
28. A call's timeout is min(the Policy's effective `stateStoreTimeout`, the time left in the per-request State Store deadline, the time left in the request context). The per-request deadline is its start plus the Route's largest effective `stateStoreTimeout` among the Policies of its effective Filter Chain that can call the State Store (`ratelimit`, `quota`, `cache` in M1), computed at compile time. The deadline starts at the first blocking call of the request **(proposed)**. After it expires, remaining Policies apply `failureMode` without waiting (RZ-STS-004). (PACK §8.7 rule 2; D09 "Deadlines" State Store row; SG-4 "at most one deadline")
29. Consumptive calls (GCRA, Quota reservation; Token Budget reservation from M3) run in chain order and stop at the first deny. Adjacent consumptive calls whose keys share a hash slot MUST run as one script (`script_multi` when more than one), else as sequential round trips; grouping is by slot in both topologies and in the `memory` driver. The merged round trip's timeout is the minimum of its calls' timeouts, clamped as in requirement 28. (PACK §8.7 rule 3; CAP "State Store sizing")
30. Read-only calls that depend on no earlier Filter MAY share one pipelined batch (`op` `pipeline`); a Response Cache lookup MUST be one pipelined batch of two read-only commands. A batch is attempted only if every involved shard's breaker admits it, else every call gets RZ-STS-003. (PACK §8.7 rule 3; D09 "Response caching")
31. Post-commit writes (cache stores and invalidations, Quota refunds; Token Budget settlement M3; Plugin writes M2) pass through the bounded queue of section 2.8 and never delay a response. (PACK §8.7 rule 4)
32. `onChunk` never calls the State Store; the State Store API exposes no per-chunk path. (PACK §8.7 rule 5)
33. Pre-call checks run in this order, the first failing one deciding the code **(proposed)**: command set supported (else RZ-STS-005) → per-request deadline left (else RZ-STS-004) → shard breaker admits (else RZ-STS-003) → in-flight slot free (else RZ-STS-004) → send; then reply before the timeout (else RZ-STS-001) and a well-formed success reply (else RZ-STS-002).
34. A consumptive call that timed out may still be applied by the server; the Filter treats it as failed. This can over-charge (a reservation or TAT advance that the client never saw) but never under-charge. (D11 "Consistency and accuracy bounds": "never an under-charge")

### 2.4 Error codes and failureMode

35. Codes (registered in `internal/errcode`, area `STS`, owner scalability-and-distributed-state):

    | Code | `ruralz_state_calls_total` result | Meaning |
    |---|---|---|
    | RZ-STS-001 | `timeout` | The call exceeded `stateStoreTimeout` or the remaining per-request deadline |
    | RZ-STS-002 | `error` | Connection error, error reply (such as out of memory or `NOSCRIPT`) or unparsable reply |
    | RZ-STS-003 | `skipped` | Skipped: the shard's State client breaker was open |
    | RZ-STS-004 | `skipped` | Not attempted: the per-request deadline was spent or the in-flight ceiling full |
    | RZ-STS-005 | `skipped` | The deployment lacks a command set the Policy needs, such as vector search |

    (D11 "State client and RZ-STS error codes")
36. `STS` is used only when a State Store call failed or timed out and the Policy applied `failureMode: closed`; a decision (a deny) never uses `STS`. Status per PACK §8.10: in a request Phase 503 (401 for a `plugin` Policy of Filter class auth, 403 for class authz, M2); in a response Phase before commit 502; `onLog` failures reach telemetry only. The body is the RFC 9457 problem document with `code` and `requestId` (D03 "Error response format") and never contains the Redis error text. (PACK §8.6, §8.10; D11 "State client")
37. `failureMode` per M1 consumer (applied by the Filter; the State Store only reports): `ratelimit` `open` (default) admits from the local buckets, which switch to the clamped, epoch-aligned window refill, and counts `ruralz_ratelimit_decisions_total{result="fail_open"}`; `closed` 503 RZ-STS-00N. `quota` `open` (default, OQ-configuration-model-8 (a)) admits unmetered (`ruralz_quota_decisions_total{result="fail_open"}`), `closed` 503. `cache` lookup `open` (default) bypasses, `closed` 503; a failed store, invalidation or refund is dropped and counted. Each failure increments `ruralz_filter_failures_total{policy,phase,mode}` and appears in the access log `failure_modes`. (ADR8 "Decision outcome"; D09 "Failure matrix"; D03 "Failure semantics"; D02 "Policy")
38. In a merged round trip every call gets the same failure; the executor applies each Policy's `failureMode` in chain order and stops at the first `closed` rejection.

### 2.5 Script library and algorithms

39. One Lua script library, `ruralz_v1.lua`, is embedded with `go:embed`, identified by the lowercase hex SHA-1 of its bytes computed locally, loaded with `SCRIPT LOAD`, and called with `EVALSHA <sha> <numkeys> KEYS... ARGV...`. `ARGV[1]` is the format `1`; `ARGV[2]` the number m of sub-operations; then per sub-operation its code, its KEYS count, its ARGV count and its ARGV. KEYS are the concatenation in order. The reply is an array of per-sub-operation arrays; after a consumptive deny, later sub-operations are absent. Codes: `g` GCRA, `q` Quota reserve, `r` Quota refund, `s` cache store, `f` cache refresh, `x` cache end-stale, `i` cache invalidate **(proposed wire format)**.
40. Script portability: touch only keys passed in `KEYS` (Dragonfly and Redis Cluster require it); use only `TIME`, `GET`, `SET` (`NX`, `PX`), `INCR`, `DECR`, `PEXPIREAT`, `PTTL`, `PEXPIRE`, `DEL`, `HGET`, `HSET`, `HDEL`; run on Lua 5.1 (Redis, Valkey) and Lua 5.4 (Dragonfly); return only integers, bulk strings and arrays (Lua numbers are truncated to integers in replies); format stored floats with `string.format('%.17g', x)` (`tostring` keeps 14 digits, verified on redis-server 7.0.15); a non-numeric counter value is an error reply (`RZBADVAL`), hence RZ-STS-002.
41. GCRA (sub-op `g`), one key per `limits[]` entry of one Policy, all sharing one hash tag (ADR8; D09 "Decision path" and "Per-Node ceiling"):
    - Time base: `now = (TIME[1] − 1767225600) × 1e6 + TIME[2]`, microseconds since 2026-01-01T00:00:00Z as an exact double **(proposed epoch: keeps about 0.008 µs resolution)**. Server `TIME` only; Node clocks never enter a decision.
    - Per limit i: `T_i = window_i_us / requests_i`; `τ_i = burst_i × window_i_us / requests_i`, `burst` default `requests`, so τ is one window by default. The Node computes `T_i` and `τ_i` in float64 and sends them with `strconv.FormatFloat(x, 'g', -1, 64)`.
    - Decision (GCRA virtual scheduling): `tat_i = max(stored TAT or now, now)`; limit i conforms iff `tat_i − now ≤ τ_i`. The request is allowed only if every limit conforms. On allow: for every i, `new_i = tat_i + T_i`, `SET key_i format('%.17g', new_i) PX max(1, ceil((new_i − now + τ_i) / 1000))` (TAT − now + τ, D09 "Decision path"). On deny: no key is written (all or nothing, ADR8).
    - This admits `requests + burst` in one window from idle, `2 × requests` with default burst, `requests` per window sustained (D11 bounds table; D09 "Per-Node ceiling" `burst` row).
    - Reply: `{allowed(0|1), denied(1-based index of the non-conforming limit with the largest retry-after, lowest index on ties; 0 when allowed), retry_after_us = ceil(max_i(tat_i − τ_i − now)) over non-conforming i (0 when allowed), now_us = floor(now), then per limit: remaining_i, reset_after_us_i}` with `remaining_i = max(0, floor((τ_i − (x_i − now)) / T_i) + 1)` and `reset_after_us_i = ceil(x_i − now)`, where `x_i = new_i` on allow, `tat_i` on deny (0 remaining for non-conforming limits). Remaining and reset feed `RateLimit` fields (D09 "RateLimit response headers"; OQ-traffic-management-and-resilience-2).
    - The Node caches a deny until `retry_after` (Traffic area). A changed `requests` or `window` is a new key (fresh state); a changed `burst` keeps the key. (D09 "Decision path")
42. Quota reserve (sub-op `q`), one unit per request (D09 "Quotas"):
    - Windows are fixed and epoch-aligned in UTC: `ws = now_ms − (now_ms mod W)` with `now_ms` from server `TIME` and `W` the window in milliseconds (`24h` resets at midnight UTC, `720h` every 30 days from the Unix epoch).
    - The Node sends two candidate keys: its own window start `A = floor(node_ms / W) × W` and the nearer neighbor `B = A − W` if `node_ms − A < W / 2`, else `A + W`. The script uses the key whose start equals `ws`; if neither, it returns skew (`chosen = 0`) and the driver fails the call with RZ-STS-002 and logs a rate-limited clock-skew WARN (skew up to half a window is tolerated, target).
    - `c = GET key` (0 when absent); if `c ≥ limit` deny with `retry_after_ms = ws + W − now_ms`; else `c = INCR key` and, when `c == 1`, `PEXPIREAT key (ws + 2W)` (expiring a window after it closes). `limit` comes from the Consumer quota per call, so a raised limit applies at once; `limit: 0` always denies.
    - Reply: `{allowed, chosen(1|2|0), used(count after INCR, or found on deny), retry_after_ms, now_ms}`.
43. Quota refund (sub-op `r`, post-commit write kind `refund`): on the exact key the reservation charged; `c = GET key`; if `c > 0` then `DECR`; never creates a key, never goes below 0. A dropped refund only over-charges. (D09 "Quotas"; D11 "Post-commit write queue")
44. `script_multi` (`m > 1`, consumptive codes only): sub-operations run in order inside one `EVALSHA` and stop at the first deny, so the effect equals the same calls made sequentially. Its round trip is labeled `op="script_multi"`; each sub-operation adds one to `ruralz_state_ops_total{kind}`. (PACK §8.7 rule 3; D10 `op` values)
45. Response Cache storage (D09 "Response caching"; entry encoding, freshness and `Vary` handling belong to the cache area):
    - Lookup: one pipelined batch of `GET rz:rc:{U}:gen` and `HMGET rz:rc:{U:P} n g:<V> e:<V>` (plain read commands instead of a partition script, section 9), where V is the hex SHA-256 of the `Vary`-selected values under the names the Node holds. Result: current generation (0 when absent), stored names `n`, variant generation `g:<V>`, entry `e:<V>`. A hit requires equal names and `g:<V>` equal to the current generation. Round trip `op="pipeline"`, ops `cache_get` ×2.
    - Store (post-commit, kind `cache_set`), one script (`s`): `SET rz:rc:{U:P}:lease <token> NX PX 1000` (fill lease 1 s, target); if not acquired, return 0 and write nothing; if the stored names differ, `DEL` the partition hash first; `HSET n <names> g:<V> <lookup generation> t:<V> <server ms> e:<V> <entry>`; keep at most 8 variants per partition (target) in store order in field `o`, `HDEL`-ing the oldest; `PEXPIRE` the hash to the larger of its remaining TTL and the entry TTL, capped at 25 h (90,000,000 ms).
    - Invalidate (post-commit, kind `cache_invalidate`), one script (`i`): `g = TIME` in microseconds since the Unix epoch; if `g ≤` the stored value, `g = stored + 1` (never reused); `SET rz:rc:{U}:gen g PX 93600000` (26 h, target). Invalidations get one retry, then drop.
    - Revalidation lease: `SET rz:rc:{U:P}:lease <token> NX PX 5000` (5 s, target). Refresh after a 304 (script `f`) and end of the stale window (script `x`) act only while the lease holds the caller's token. These count as `cache_set` **(proposed: D10 has no label for them)**.
46. The `memory` driver MUST implement requirements 41 to 45 with the same arithmetic in Go (float64, the same epoch, the injected clock as "server TIME"), so a differential test gets identical decisions and TATs for identical inputs. (TEST "Required properties" Rate Limit)

### 2.6 Key layout

47. Prefixes: `rz:rl:` Rate Limit TATs, `rz:qt:` Quota (and from M3 Token Budget) counters, `rz:rc:` Response Cache; `rzplg:` reserved for Plugin state (M2, OQ-wasm-plugin-system-19 (a)); another prefix needs a D11 catalog row. (D11 "Distributed state catalog")
48. Exact formats (hex = lowercase hex SHA-256 of the UTF-8 value; D and duration rendering **(proposed)** as in section 9):
    - GCRA: `rz:rl:<policy>:<requests>/<window>:{<hex of config.key value>}`, `<policy>` the Policy `metadata.name` (RFC 1123 label, no escaping needed), `<requests>` decimal, `<window>` the canonical Go duration string of the Revision (`1s`, `1m0s`, `24h0m0s`). (ADR8 Keys row; D09 "Decision path")
    - Quota: `rz:qt:<consumerQuota>:<window>:<window start>:{<hex of config.key value>}`, `<window start>` in Unix milliseconds, `<consumerQuota>` escaped: every byte outside `[A-Za-z0-9._-]` becomes `%XX` (uppercase hex), so `{`, `}` and `:` never appear. (D09 "Quotas")
    - Response Cache: generation `rz:rc:{<hex U>}:gen`; partition hash `rz:rc:{<hex U>:<hex P>}` with fields `n`, `o`, `g:<V>`, `t:<V>`, `e:<V>`; lease `rz:rc:{<hex U>:<hex P>}:lease`. U is the SHA-256 of scheme, host, path and normalized query and P that of the partition key value, both computed by the cache area. (D09 "Response caching" Layout row)
49. Hash slot: CRC16-XMODEM (polynomial 0x1021, initial 0) of the hash tag (the bytes between the first `{` and the next `}` when non-empty, else the whole key), modulo 16384 (Redis Cluster specification). Consumptive keys of equal `config.key` value share one slot, so a `ratelimit` and a `quota` keyed alike merge.
50. Keys expire when idle: GCRA at TAT − now + τ; Quota a window after its window closes; generation keys 26 h after the last bump; partitions at most 25 h (targets). No key without a TTL is ever written. (D09; D11)
51. The deployment holding limit or Quota keys MUST run `maxmemory-policy noeviction`; each Node reads `maxmemory_policy` from `INFO memory` per shard (requirement 52) and raises `state_store_eviction_policy` while any shard reports another value and the active snapshot attaches a `ratelimit` or `quota` Policy on that State Store. A missing field (unknown) does not raise the reason and logs one WARN **(proposed)**. (D11 "State Store availability"; D09 "Failure matrix")

### 2.7 Memory readings and store budgets

52. `INFO memory` per shard, off the request path: every 10 s, or every 1 s while `used_memory` is above 50% of `maxmemory` (target). Fields used: `used_memory`, `maxmemory`, `maxmemory_policy`. Before the first successful reading of a shard its stores skip (no unmetered bytes). A failed read keeps the last reading for 30 s, then clears it **(proposed)**. `maxmemory: 0` reports "unbounded": no store skips for memory reasons, one WARN **(proposed)**. (D09 "Response caching" rule 2; D11 "Stateless data plane" readings row)
53. The State Store exposes store admission per shard for the Response Cache: skip when the last reading is above 70% of `maxmemory`; skip while a shard grew by more than 10% of `maxmemory` between reads, until growth falls under 2%; otherwise allow at most `c = min(2% of maxmemory per 10 s / N_c, 4 MiB per second)` bytes per shard per Node between reads, `N_c` the published Node count, or 1,000 without one (always 1,000 in M1). Skips count as `ruralz_cache_store_skipped_total{reason="memory"}` (by the cache Filter). Generation bumps bypass these rules. (D09 "Response caching" rules 2 and 3; D10 metrics table)
54. At startup and each activation, when the active snapshot attaches `cache` and a limit Policy type (`ratelimit`, `quota`) on one State Store, the Node logs one WARN. (D11 "Distributed caches"; D09 "Response caching")

### 2.8 Post-commit write queue

55. Capacity 64,000 items and 85 MiB (target), fixed (OQ-scalability-and-distributed-state-4 (a)), in four classes, each capped in items and bytes; a full class drops only its own writes; `Enqueue` never blocks and never allocates on the drop path. (D11 "Post-commit write queue")

    | Class | Capacity | Turn | On full or failure |
    |---|---|---|---|
    | Quota refunds (M1), Token Budget settlement (M3) | 32,000 items, 16 MiB | First | Drop; over-charge only |
    | Cache invalidations | 6,400 items, 1 MiB (reserved 10%) | Second | One retry, then drop |
    | Cache stores | 16,000 items, 64 MiB of payload | Third, 4 of 5 turns | Drop |
    | Plugin writes (M2; empty in M1) | 9,600 items, 4 MiB | Third, 1 of 5 turns | Drop per Plugin Policy |

56. Eight writer goroutines per Node cycle settlement → invalidation → third class, skipping empty classes, taking up to 64 writes per batch sent as one pipelined `DoMulti` (target); so an invalidation waits at most one settlement batch plus one in progress, and Plugin writes get one batch in 15 under saturation. A write uses its Policy's effective `stateStoreTimeout`; the per-request deadline does not apply **(proposed)**. Writes to a shard whose breaker is open drop at once.
57. Every drop increments `ruralz_state_writes_dropped_total{kind}` with `kind` in `refund`, `cache_set`, `cache_invalidate` (M3: `settle`, `semantic_set`); `ruralz_state_write_queue_items` is the total depth. On Drain the queue flushes for up to 5 s (target), then drops and counts the rest. An item holds a reference on its State Store, so a Hot Reload never closes a store with queued writes. (D11; D10; RuralzStateWritesDropped alert)

### 2.9 memory driver

58. `memory` holds TATs, Quota counters, cache entries, generations and leases in the Node, with the semantics of section 2.5, bounded by **(proposed; documents give no bound, SG-7 requires one)**: 1,048,576 counter and lease entries in 256 mutex-guarded shards, and 64 MiB of cache entry bytes. Expired entries are reclaimed on access and by one sweeper goroutine within 10 s of expiry. When a shard is full of unexpired entries, a new key fails with RZ-STS-002, like `noeviction` refusing writes. Its memory reading reports `maxmemory` = 64 MiB and policy `noeviction`, so cache rules apply unchanged.
59. A single-limit GCRA call on `memory` MUST make 0 heap allocations; with the local bucket it fits the `ratelimit` stage budget of 2 µs p50, 8 µs p99 (target). It is not a round trip: its time stays gateway-added, it adds nothing to `state_store_duration` or `ruralz_state_call_duration_seconds`, but it counts in `ruralz_state_calls_total` and `ruralz_state_ops_total` **(proposed)**. (D12 "Per-stage latency budget" and S2 config comment)
60. `memory` never has breaker, in-flight or timeout failures; a spent per-request deadline still yields RZ-STS-004. Limits multiply by the Node count; `state_store_memory_multi_node` is M2. (PACK §8.8; D10)

### 2.10 Lifecycle and Hot Reload

61. A change of `Gateway.spec.stateStore` takes effect by Hot Reload as a State Store upgrade. The Manager MUST reuse the live driver when driver, topology and resolved URL are unchanged (a `timeout`-only change reuses it), so connections, breakers, scripts and `memory` state survive; otherwise it opens a new driver off the request path, requests pinned to the old snapshot keep the old driver, and the old driver closes when its last snapshot and queued write release it. (ZDU "Hot Reload versus restart", "State Store upgrades")
62. A rotated `file` secret that changes only credentials is used on the next connection (rueidis `AuthCredentialsFn` reading the current value); one that names another deployment keeps the old connection until restart and logs a WARN (OQ-zero-downtime-upgrades-and-hot-reload-9 (a)) **(proposed)**.
63. Shutdown order at Drain: in-flight requests finish, the queue flushes (up to 5 s), then drivers close; every goroutine this area starts has an owner that cancels and waits for it. (D03 "Goroutines"; LAYOUT "Code conventions")

### 2.11 Telemetry

64. Metrics (names and labels exact, D10 "Ruralz Gateway metrics"):
    - `ruralz_state_call_duration_seconds{op}` histogram, `fast` boundaries; one observation per attempted round trip when the caller stops waiting (reply or timeout); SLI of SLO-GW-5 (`op="gcra"`, `le="0.001"`, `script_multi` excluded).
    - `ruralz_state_calls_total{op,result}`, result `ok`, `error`, `timeout`, `skipped`; one per round trip or skipped call.
    - `ruralz_state_ops_total{kind}` operations carried in round trips; `ruralz_state_writes_dropped_total{kind}`; `ruralz_state_write_queue_items`.
    - `op` values: `gcra`, `quota`, `cache_get`, `script_multi`, `pipeline`, `cache_set`, `cache_invalidate`, `refund` (M3: `budget`, `semantic_get`, `semantic_set`, `settle`); `kind` is a single-operation `op`, or a write kind for drops.
    - `ruralz_security_cleartext_hops{hop="state_store"}`; `ruralz_node_degraded_info{reason}` for `state_store_breaker_open`, `state_store_eviction_policy`, `state_store_memory_fallback`, `state_store_unauthenticated`. Every reason and every enumerated label value exists at 0 from process start (D10 "Alert rules").
65. Spans: State Store calls are attributes of the calling Filter's `ruralz.filter.<name>` span (State Store `op` and duration); a shared round trip is recorded on the first Filter's span and named by the others in `ruralz.state.batch`. Other attribute names **(proposed, for the telemetry catalog)**: `ruralz.state.op`, `ruralz.state.result`, `ruralz.state.duration`, `ruralz.state.code`. (D10 "Span model")
66. Access log: `state_store_duration` sums the request's State Store round-trip time; that time is excluded from `ruralz_http_gateway_duration_seconds`. (D10 "Logs" field table, "Gateway-added time")
67. Logs through `internal/telemetry` `slog` only: breaker open (WARN) and close (INFO), script reload (INFO), eviction policy (WARN), memory fallback (WARN), cache sharing a limit store (WARN), clock skew (WARN, rate-limited), unauthenticated loopback (WARN). Shards appear as `ss-<first 8 hex of SHA-256 of host:port>`, never as the URL, user name or password **(proposed)**.

### 2.12 Security

68. State Store entry MAC (OQ-security-and-identity-22 (a)): when a MAC key is configured, every opaque entry the State Store stores for others (M1: Response Cache entries) carries HMAC-SHA-256 over `"rz1" ‖ full key ‖ 0x00 ‖ field name ‖ 0x00 ‖ entry`; a missing or wrong tag on read is a miss, never an error, logged rate-limited. Counters are not MACed (Lua has no HMAC; deletion cannot be prevented). Setting name comes from the pack amendment **(proposed: `RURALZ_STATE_STORE_MAC_KEY_FILE`, at least 32 bytes)**. (D08 "Threat model" T10)
69. Required server permissions (documented for ACL users): `EVALSHA`, `SCRIPT LOAD`, `GET`, `SET`, `HMGET`, `INFO`, `PING`, `CLUSTER SLOTS` (cluster), `SELECT` (db ≠ 0). A `NOPERM` reply is RZ-STS-002.

## 3. Proposed Go packages and API

Packages (LAYOUT "Monorepo tree": `internal/statestore/` memory, redis; lower-case singular names; context first; bounded goroutines):

| Package | Contents | Imports |
|---|---|---|
| `internal/statestore` | Driver-neutral API: `Store`, `Call`, operation types, `RequestBudget`, `Error` and codes, key builders, `Slot`, `Recorder`, `Manager`, `Handle`, constants | stdlib, `pkg/config/v1alpha1` |
| `internal/statestore/breaker` | Lock-free per-shard breaker and reconnect pacer, clock-injected | stdlib |
| `internal/statestore/memory` | `memory` driver | `internal/statestore` |
| `internal/statestore/redis` | `redis` driver: rueidis client, Ruralz dialer, slot map, script library (`ruralz_v1.lua`), URL validation, memory poller | rueidis, `internal/statestore`, `.../breaker` |
| `internal/statestore/postcommit` | Post-commit write queue and writers | `internal/statestore` |
| `internal/statestore/statestoretest` | Test only: failure-injecting fake `Store`, conformance suite, RESP counting proxy, TCP fault proxy | `testing`, `internal/statestore` |
| `internal/statestore/statestoretest/redisserver` | Test only: launches local `redis-server` processes (standalone, TLS, 3-primary cluster) or testcontainers in CI | stdlib `os/exec`, `crypto/x509` |

Only `cmd/ruralzd` wiring (through `internal/gateway`) imports `memory` and `redis`; Filters import `internal/statestore` only. Proposed depguard additions: `admitted` allow `github.com/redis/rueidis`; deny `internal/statestore/statestoretest` in non-test files (`files: ["$all", "!$test"]`).

### 3.1 `internal/statestore`

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package statestore is the driver-neutral State Store API (D11, pack 8.7).
package statestore

// DriverKind selects the implementation (pack 7: memory and redis only).
type DriverKind uint8

const (
	DriverMemory DriverKind = iota + 1
	DriverRedis
)

// Topology is the redis deployment shape.
type Topology uint8

const (
	TopologyStandalone Topology = iota + 1
	TopologyCluster
)

// Source records where the configuration came from.
type Source uint8

const (
	SourceGateway  Source = iota + 1 // Gateway spec.stateStore
	SourceEnv                        // RURALZ_STATE_STORE_URL
	SourceFallback                   // neither: memory, degraded state_store_memory_fallback
)

// DefaultTimeout applies when Gateway spec.stateStore.timeout is unset (proposed).
const DefaultTimeout = 50 * time.Millisecond

// Config is the resolved per-Node configuration. URL is a resolved secret:
// it is never logged, rendered or put in an error.
type Config struct {
	Driver         DriverKind
	Topology       Topology
	URL            string
	DefaultTimeout time.Duration
	Source         Source
}

// Resolve applies requirement 2. resolveURL resolves the SecretValue through
// the secretRef providers (configuration area); getenv reads RURALZ_* settings.
func Resolve(spec *v1alpha1.StateStore, resolveURL func(v1alpha1.SecretValue) (string, error),
	getenv func(string) (string, bool)) (Config, error)

// Digest is the SHA-256 of a key value.
type Digest [32]byte

// DigestOf hashes s without allocating for s shorter than 64 bytes.
func DigestOf(s string) Digest

// OpKind is one State Store operation; String returns the exact D10 label.
type OpKind uint8

const (
	OpGCRA            OpKind = iota + 1 // "gcra"
	OpQuota                             // "quota": reserve one unit
	OpCacheGet                          // "cache_get"
	OpCacheSet                          // "cache_set": store, lease, refresh, end-stale
	OpCacheInvalidate                   // "cache_invalidate"
	OpRefund                            // "refund"
	// Reserved, not built in M1: OpBudget "budget", OpSettle "settle",
	// OpSemanticGet "semantic_get", OpSemanticSet "semantic_set" (M3);
	// OpPluginGet, OpPluginIncr (M2).
)

// RoundTrip labels a round trip: a single OpKind, script_multi or pipeline.
type RoundTrip uint8

// Result is the ruralz_state_calls_total result label: ok, error, timeout, skipped.
type Result uint8

// GCRALimit is one limits[] entry; tau = Burst * Window / Requests.
type GCRALimit struct {
	Requests int64
	Window   time.Duration
	Burst    int64
}

// GCRAOutcome is one limit's state after the decision.
type GCRAOutcome struct {
	Remaining  int64
	ResetAfter time.Duration
}

// GCRA is one ratelimit Policy's call: every limit in one script, all or nothing.
type GCRA struct {
	Policy string        // Policy metadata.name
	Digest Digest        // SHA-256 of the config.key value
	Limits []GCRALimit   // the compiled Policy's limits, shared and immutable
	Out    []GCRAOutcome // caller-provided, len(Limits)

	Allowed    bool
	Denied     int // index of the limit setting RetryAfter; -1 when allowed
	RetryAfter time.Duration
	ServerNow  time.Time
}

// Quota reserves one unit of a Consumer quota window.
type Quota struct {
	Name   string        // config.consumerQuota
	Window time.Duration // the Consumer quota window
	Limit  int64         // the Consumer quota limit
	Digest Digest        // SHA-256 of the config.key value (default consumer.name)

	Allowed     bool
	WindowStart time.Time     // server-selected window, charged or exhausted
	Used        int64         // count after the reservation, or found on deny
	RetryAfter  time.Duration // to the window end on deny; the Filter adds jitter
	ServerNow   time.Time
}

// CacheKey locates a Response Cache partition.
type CacheKey struct{ URI, Partition Digest }

// CacheLookup is the pipelined lookup (GET generation, HMGET partition).
type CacheLookup struct {
	Key     CacheKey
	Variant Digest // SHA-256 of the Vary-selected values under the Node's names

	Generation  int64  // current generation, 0 when absent
	Names       string // names stored for the partition, "" when none
	Found       bool
	EntryGen    int64
	Entry       []byte // opaque, MAC-verified; valid until the Call is released
}

// Call is one Policy's blocking operation and its outcome; callers own it
// (per-request arena or pool). Exactly one of the operation fields is used.
type Call struct {
	Kind    OpKind
	Timeout time.Duration // the Policy's effective stateStoreTimeout
	GCRA    GCRA
	Quota   Quota
	Lookup  CacheLookup

	// Outcome, written by the Store.
	Done    bool          // decided (false for calls after a deny)
	Err     *Error        // nil when the store answered; one of the sentinels
	Elapsed time.Duration // round-trip time, 0 when not attempted or memory
	Batch   int           // index of the call whose span records the shared round trip, or -1
	Trip    RoundTrip
}

// Write is one post-commit write.
type Write struct {
	Kind       OpKind // OpRefund, OpCacheSet, OpCacheInvalidate in M1
	Timeout    time.Duration
	Refund     Refund
	Cache      CacheStore   // Kind OpCacheSet: store one variant
	Lease      CacheLease   // Kind OpCacheSet: take, refresh or end-stale (Cache unused)
	Invalidate Digest       // URI digest for OpCacheInvalidate
	Err        *Error
	Applied    bool         // e.g. lease acquired, entry stored
}

// Refund returns one unit to the window a reservation charged.
type Refund struct {
	Name        string
	Window      time.Duration
	WindowStart time.Time
	Digest      Digest
}

// CacheStore stores one variant under the partition's fill lease.
type CacheStore struct {
	Key        CacheKey
	Names      string
	Variant    Digest
	Generation int64 // the lookup's generation
	TTL        time.Duration
	Entry      []byte
	Token      uint64
}

// CacheLease takes, uses or ends the partition lease (revalidation).
type CacheLease struct {
	Key     CacheKey
	Variant Digest
	Mode    LeaseMode // take (5 s), refresh (304), end-stale
	Token   uint64
	Entry   []byte
	TTL     time.Duration
}

// Store is what Filters and the post-commit queue use. Implementations are
// safe for concurrent use and never block beyond the computed timeouts.
type Store interface {
	// Consume runs, as one round trip, the longest prefix of calls whose keys
	// share one hash slot (one script, script_multi when longer than one),
	// in order, stopping at the first deny. It returns how many calls it took
	// from the front (at least 1); calls after a deny stay !Done.
	Consume(ctx context.Context, rb *RequestBudget, calls []*Call) int
	// Read runs independent read-only calls as one pipelined batch.
	Read(ctx context.Context, rb *RequestBudget, calls []*Call)
	// Write sends one post-commit batch (postcommit writers only).
	Write(ctx context.Context, batch []*Write)
	// AdmitStoreBytes applies Response caching rules 2 and 3 to the shard of key.
	AdmitStoreBytes(key CacheKey, n int) bool
	// Supports reports whether the deployment offers a command set (RZ-STS-005).
	Supports(c Capability) bool
	// RoundTrips is false for memory: its time stays gateway-added.
	RoundTrips() bool
}

// Driver is a live Store with a lifecycle, owned by the Manager.
type Driver interface {
	Store
	Status() Status
	Close(ctx context.Context) error
}

// Status feeds degraded reasons and the cleartext gauge.
type Status struct {
	BreakerNotClosed bool
	EvictionPolicy   Tristate // noeviction, other, unknown
	Cleartext        bool
	Unauthenticated  bool
}

// Capability is a command set a Policy may need.
type Capability uint8

const (
	CapScripts Capability = 1 << iota
	CapVectorSets     // M3, never reported in M1
	CapValkeySearch   // M3
)

// Failure classifies an unanswered call.
type Failure uint8

const (
	FailTimeout      Failure = iota + 1 // RZ-STS-001, result timeout
	FailError                           // RZ-STS-002, result error
	FailBreakerOpen                     // RZ-STS-003, result skipped
	FailNotAttempted                    // RZ-STS-004, result skipped
	FailUnsupported                     // RZ-STS-005, result skipped
)

// Error is a State Store failure; the five values below are the only ones,
// so failures never allocate. Causes are logged, never shown to clients.
type Error struct{ failure Failure }

func (e *Error) Error() string
func (e *Error) Code() string   // "RZ-STS-001" ... "RZ-STS-005"
func (e *Error) Failure() Failure
func (e *Error) Result() Result

var (
	ErrTimeout      = &Error{FailTimeout}
	ErrFailed       = &Error{FailError}
	ErrBreakerOpen  = &Error{FailBreakerOpen}
	ErrNotAttempted = &Error{FailNotAttempted}
	ErrUnsupported  = &Error{FailUnsupported}
)

// RequestBudget is pack 8.7 rule 2's per-request deadline. Zero value: no
// State Store Policy on the Route. Policies of one request run sequentially.
type RequestBudget struct {
	max      time.Duration
	deadline time.Time // zero until the first blocking call
}

func NewRequestBudget(routeMax time.Duration) RequestBudget

// CallTimeout starts the budget on first use and returns the call timeout:
// min(policy, deadline-now, ctx deadline-now); ok is false when spent.
func (b *RequestBudget) CallTimeout(ctx context.Context, now time.Time, policy time.Duration) (time.Duration, bool)

// RouteMax returns the largest effective stateStoreTimeout (compile time).
func RouteMax(timeouts []time.Duration) time.Duration

// Key builders append to dst and never allocate when dst has capacity.
func AppendRateLimitKey(dst []byte, policy string, l GCRALimit, d Digest) []byte
func AppendQuotaKey(dst []byte, name string, window time.Duration, start time.Time, d Digest) []byte
func AppendCacheGenKey(dst []byte, uri Digest) []byte
func AppendCachePartitionKey(dst []byte, k CacheKey, suffix string) []byte

// Slot is the Redis Cluster hash slot of key (CRC16-XMODEM of its hash tag, mod 16384).
func Slot(key []byte) uint16

// SlotOf is the slot of a call's hash tag (GCRA and Quota: hex digest; lookup: -1 for multi-slot).
func SlotOf(c *Call) int

// Clock is injected everywhere; production uses time.Now.
type Clock interface{ Now() time.Time }

// Recorder is implemented by internal/telemetry with pre-bound, allocation-free
// instruments (OQ-observability-16); a no-op Recorder serves tests.
type Recorder interface {
	RoundTrip(trip RoundTrip, res Result, d time.Duration, attempted bool)
	Ops(kind OpKind, n int)
	WriteDropped(kind OpKind)
	QueueItems(n int64)
	Degraded(reason Reason, on bool)
	CleartextHop(on bool)
}

// Reason is a ruralz_node_degraded_info reason this area raises.
type Reason uint8

const (
	ReasonBreakerOpen    Reason = iota + 1 // state_store_breaker_open
	ReasonEvictionPolicy                   // state_store_eviction_policy
	ReasonMemoryFallback                   // state_store_memory_fallback
	ReasonUnauthenticated                  // state_store_unauthenticated
	// Reserved: state_store_memory_multi_node (M2), semantic_cache_unsupported (M3).
)

// Limits are the fixed bounds (OQ-scalability-and-distributed-state-4 (a));
// only tests override them.
type Limits struct {
	InFlight                        int           // 8192
	PipelinedPerShard               int           // 2
	DedicatedPerShard               int           // 8
	BreakerWindow                   time.Duration // 5s
	BreakerMinCalls                 int           // 20
	BreakerFailureRatio             float64       // 0.5
	BreakerConnectFailures          int           // 5
	BreakerOpenMin, BreakerOpenMax  time.Duration // 1s, 3s
	BreakerCloseAfter               int           // 3
	ReconnectBase, ReconnectCap     time.Duration // 100ms, 5s
	ConnectsPerSecond               int           // 4
	ShardsRefresh                   time.Duration // 10s
	MemoryPollSlow, MemoryPollFast  time.Duration // 10s, 1s
}

func DefaultLimits() Limits

// Deps are shared by drivers.
type Deps struct {
	Logger   *slog.Logger // from internal/telemetry
	Recorder Recorder
	Clock    Clock
	NodeID   string
	InFlight *InFlight // Node-wide ceiling shared by every driver
	Limits   Limits
	MACKey   []byte // OQ-security-and-identity-22, optional
}

// Opener constructs a driver without dialing synchronously; it fails only
// for an invalid configuration (RZ-CFG-026 reasons).
type Opener func(ctx context.Context, cfg Config, deps Deps) (Driver, error)

// Openers is the explicit driver table wired by cmd/ruralzd (no init()).
type Openers struct{ Memory, Redis Opener }

// Manager owns drivers across Hot Reloads and aggregates degraded reasons.
type Manager struct{ /* unexported */ }

func NewManager(openers Openers, deps Deps) *Manager

// Open returns a Handle, reusing the live driver for an equal
// (driver, topology, URL); it never waits on a dial.
func (m *Manager) Open(ctx context.Context, cfg Config) (*Handle, error)

// SetNodeCount feeds N_c for store budgets (M2 Control Stream; unknown in M1).
func (m *Manager) SetNodeCount(n int, known bool)

// Shutdown closes every driver after the post-commit queue flushed.
func (m *Manager) Shutdown(ctx context.Context) error

// Handle is a snapshot's (or queued write's) reference to a driver.
type Handle struct{ /* unexported */ }

func (h *Handle) Store() Store
func (h *Handle) Retain()
func (h *Handle) Release()
```

### 3.2 `internal/statestore/breaker`

```go
// Package breaker is the per-shard State client breaker and reconnect pacer (D11).
package breaker

type State uint8 // Closed, Open, HalfOpen

// Breaker is lock-free on the admission path: a 10-bucket ring of 500 ms
// (successes, failures) counters with atomic adds, and a CAS-guarded state word.
type Breaker struct{ /* unexported */ }

func New(l statestore.Limits, rnd func(lo, hi time.Duration) time.Duration) *Breaker

// Allow reports whether a call may go; probe is true in half-open, where only
// one probe is in flight at a time.
func (b *Breaker) Allow(now time.Time) (probe bool, ok bool)

// Done records a completed (or timed-out) call exactly once.
func (b *Breaker) Done(now time.Time, probe, success bool)

func (b *Breaker) ConnectFailed(now time.Time)
func (b *Breaker) ConnectSucceeded()
func (b *Breaker) State(now time.Time) State

// Pacer gates dials of one shard: full jitter, base 100 ms, cap 5 s, and at
// most 4 dial starts in any 1 s (ring of the last 4 start times).
type Pacer struct{ /* unexported */ }

func (p *Pacer) TryDial(now time.Time) bool
func (p *Pacer) Result(now time.Time, ok bool)
```

### 3.3 Drivers

```go
package memory

// New returns the memory driver: 256 shards, 1,048,576 entries, 64 MiB of
// cache bytes (proposed), one sweeper goroutine stopped by Close.
func New(ctx context.Context, cfg statestore.Config, deps statestore.Deps) (statestore.Driver, error)
```

```go
package redis

// New validates cfg (RZ-CFG-026 reasons), builds rueidis ClientOption per
// requirement 14 and starts the maintenance goroutine; it never waits on a dial.
func New(ctx context.Context, cfg statestore.Config, deps statestore.Deps) (statestore.Driver, error)

// Validate checks a resolved URL against its topology (used at activation).
func Validate(cfg statestore.Config) (URLInfo, error)

// URLInfo drives degraded reasons without exposing the URL.
type URLInfo struct {
	TLS, Loopback, Credentials bool
	Seeds                      int
}

// URLError is an RZ-CFG-026 reason; Error never contains the URL.
type URLError struct{ Rule string }

//go:embed ruralz_v1.lua
var script []byte // go:embed variables are exempt from the no-globals rule
```

### 3.4 `internal/statestore/postcommit`

```go
package postcommit

type Class uint8 // ClassSettle, ClassInvalidate, ClassStore, ClassPlugin

// Item is one queued write with its State Store reference and byte size.
type Item struct {
	Class  Class
	Handle *statestore.Handle // retained by Enqueue, released after the write
	Write  statestore.Write
	Bytes  int
}

// Queue: four bounded rings with item and byte caps (section 2.8).
type Queue struct{ /* unexported */ }

func New(rec statestore.Recorder) *Queue

// Enqueue never blocks; false means dropped and counted.
func (q *Queue) Enqueue(it Item) bool

// Run starts the eight writers and returns when ctx ends and they exited.
func (q *Queue) Run(ctx context.Context) error

// Flush drains for up to the Drain bound (5 s), then drops and counts the rest.
func (q *Queue) Flush(ctx context.Context) error
```

### 3.5 Concurrency model

- Filters call `Consume` and `Read` on the request goroutine; Policies of one request run sequentially, so `RequestBudget` needs no locking.
- `memory`: synchronous; 256 shards, each a short `sync.Mutex` with a map keyed by a comparable struct (namespace, Policy or quota name, requests, window, window start, digest), so no key string is built; all limits of one call share a digest and therefore one shard lock, which makes multi-limit and `script_multi` updates atomic. One sweeper goroutine.
- `redis`, per blocking call: pre-checks (requirement 33) with atomics only; take a pooled `pending` (1-buffered done channel, reusable timer, reply slot, refcount 2); start one goroutine that runs `client.Do` or `DoMulti` with `context.WithoutCancel(ctx)` plus a 5 s safety deadline **(proposed)**, decodes into the pending, releases the in-flight slot and signals; the caller waits on done or its timer. The first of reply and timeout records breaker and metrics (CAS on the pending state); the pending returns to the pool when both parties released it. Goroutines are bounded by the 8,192 ceiling. This is needed because rueidis returns at the caller's deadline and waits for the reply in an internal goroutine that Ruralz cannot observe.
- `redis` maintenance: one goroutine per driver with a bounded, coalescing event channel (per-shard flags): `SCRIPT LOAD` after a dial event or `NOSCRIPT`, `INFO memory` on the poll schedule, `CLUSTER SLOTS` every 10 s and after a close, half-open `PING` probes, background cluster construction. The Ruralz dialer never calls rueidis synchronously (deadlock), it only posts events.
- Shared read state (slot map `[16384]uint16` shard index, shard table, memory readings) is published through `atomic.Pointer` to immutable values.
- Post-commit: eight writer goroutines owned by `Queue.Run`; rings guarded by one short mutex per class.
- rueidis owns its per-connection goroutines, bounded by the connection limits.

### 3.6 Exported for other areas

`statestore.Store`, `Call` and the operation types, `RequestBudget`, `RouteMax`, `Error` sentinels and `Code()`, `DigestOf`, key builders, `Slot`, `Recorder` and `Reason` (for telemetry), `Manager`/`Handle`/`Openers` (for gateway wiring), `postcommit.Queue`, and the test kit `statestoretest` (`Fake` with scripted outcomes and a fake clock, `RunConformance(t, open func(t) statestore.Driver)`, `CountingProxy`, `FaultProxy`) plus `redisserver`.

## 4. Dependencies on other areas

| Needs | From area | Used for |
|---|---|---|
| `v1alpha1.StateStore`, `Policy.StateStoreTimeout`, `RateLimitConfig`, `QuotaConfig`, `CacheConfig`, Consumer `Quota` (existing in `pkg/config/v1alpha1`) | Configuration | Config resolution, compiled calls |
| `secretRef` resolution (`env`, `file`), RZ-CFG-026 diagnostics, redaction in `/config/dump` and diffs | Configuration / Security | Requirement 5 |
| Snapshot compile and retirement hooks (acquire and release a `Handle`), per-Route effective Filter Chain to compute `RouteMax` | Configuration loader / Data plane | Hot Reload (requirement 61), per-request deadline |
| Filter Chain executor: calls `Consume`/`Read`, applies `failureMode` and status per pack 8.10, stops at first deny, groups adjacent consumptive Policies (a Prepare-then-Finish Filter contract is proposed so keys and local checks run before a merged call, with local tokens returned for Policies after a deny), excluded-time accounting, access-log `state_store_duration` and `failure_modes`, error body, Drain sequencing | Data plane | Requirements 27 to 38, 63, 66 |
| `ratelimit`, `quota`, `cache` Filters: build `Call`s, compute digests, local buckets, over-limit and quota denial caches, RateLimit headers, cache entry encoding, freshness, `Vary` LRU, hot-entry layer, `ruralz_ratelimit_*`, `ruralz_quota_*`, `ruralz_cache_*` metrics | Traffic and resilience | Consumers of this API |
| `slog` logger, OpenTelemetry instruments implementing `Recorder`, degraded-info gauge, cleartext-hop gauge, span attribute constants | Telemetry | Section 2.11 |
| `internal/errcode` (RZ-STS-001 to -005 and RZ-CFG-026 already registered) | M0 | Codes |
| `node.id` (ULID) | Data plane / identity | `ClientName` |
| MAC key setting | Security (OQ-security-and-identity-22) | Requirement 68 |
| CEL | None directly: key values arrive as strings evaluated by the Filters | |

Provides: the whole State Store API of section 3, the post-commit queue, `state_store_*` degraded reasons, the `ruralz_state_*` metrics feed, and the test kit used by every stateful Filter's unit tests (TEST "Unit and property tests": "the State Store arrive[s] as failure-injecting fakes").

## 5. Libraries

| Module | Version | Where | Why |
|---|---|---|---|
| `github.com/redis/rueidis` | v1.0.78 (catalog floor and latest on proxy.golang.org; `go 1.25.0`, requires `golang.org/x/sys` v0.47.0, BSD) | `internal/statestore/redis` only | TECH "Library catalog" State Store client row (Apache-2.0, RESP3, auto-pipelining, Valkey-aware); ADR8 |
| `github.com/testcontainers/testcontainers-go` | v0.44.0 | `_test.go` files with tag `integration`, CI only (no Docker in this environment) | TECH "Test tooling" row; TEST "Integration tests". Use the generic container API with images pinned by digest: `modules/redis` v0.44.0 pulls `github.com/redis/go-redis/v9` and `modules/valkey` pulls `github.com/valkey-io/valkey-go` into the graph (TECH S6) |
| Standard library | Go 1.26 floor | everywhere | `crypto/sha256` (key digests), `crypto/sha1` (Redis script identifier only; gosec G401/G505 suppression with reason), `crypto/hmac` (entry MAC), `crypto/tls`, `math/rand/v2` (jitter, lease tokens), `os/exec` (local `redis-server` harness), CRC16-XMODEM written in-house (no stdlib CRC16) |

Not usable: `miniredis` (links `github.com/yuin/gopher-lua`, banned by depguard "no Lua", ADR-0011); `redis_rate` and `throttled` (TECH alternatives not chosen); `go-redis`. OpenTelemetry is reached only through `internal/telemetry`.

## 6. Test plan

Unit tests are hermetic, shuffled, race-enabled (TEST "Unit and property tests"); integration tests use tag `integration` and `make integration` (LAYOUT stage 8).

### 6.1 Unit and table tests

| Target | Cases |
|---|---|
| `Resolve` | Gateway redis standalone; cluster; no `spec.stateStore` with env → redis standalone, `SourceEnv`; neither → memory, `SourceFallback`, WARN, `ReasonMemoryFallback` on; explicit memory → WARN, reason off; `driver: redis` without url → env, else RZ-CFG-026; timeout unset → 50 ms; secret resolution error → RZ-CFG-026 |
| `redis.Validate` (RZ-CFG-026 rules) | `addr` under standalone; `master_set`; cluster with `/1`; schemes `http`, `unix`; `skip_verify`, `skip_verify=false`; `protocol=2`; `client_cache=0`; `max_retries=0`; `dial_timeout`; `write_timeout`; non-loopback without credentials; accepted: `rediss://u:p@h:6380/2`, cluster seeds, `redis://127.0.0.1:6379` (Loopback, !Credentials → unauthenticated), `redis://[::1]:6379`, `redis://localhost`, `valkeys://` alias. Every error string checked: no URL, user or password |
| `RequestBudget` | unstarted: returns policy; policy < remaining; remaining < policy; spent → !ok (RZ-STS-004); ctx deadline earlier than both; policy 0 → !ok; `RouteMax` of empty and mixed lists |
| `Error` | each sentinel's `Code()`, `Result()`, `errors.Is`; zero allocations for failure paths |
| Keys (golden files under `testdata/`) | `rz:rl:ratelimit-gold:100/1s:{…}`, `…:5000/1m0s:{…}`; quota `rz:qt:monthly-requests:720h0m0s:<ms>:{…}`; escaping of `a{b}`, `a:b`, `%`, UTF-8 `é`; cache gen, partition, lease; digest of empty string |
| `Slot` | CRC16("123456789") = 0x31C3 → 12739; Redis Cluster spec examples: `{user1000}.following` = `{user1000}.followers`; `foo{}{bar}` hashes the whole key; `foo{{bar}}zap` uses `{bar`; `foo{bar}{zap}` uses `bar` |
| `breaker` (fake clock, fixed rand) | 19 failures of 19 → closed; 10 of 20 → open; 9 of 20 → closed; bucket roll after 5 s; 5 consecutive connect failures with zero calls → open; open → half-open at the injected delay within [1 s, 3 s]; half-open admits exactly one probe (concurrent `Allow` from 64 goroutines); 3 successes close; failing probe reopens with a new delay; timeout recorded once when reply also arrives |
| `Pacer` | delays within [0, min(5 s, 100 ms × 2^n)]; 5th dial within 1 s refused; reset after success; refused dial is not a connect failure |
| In-flight | 8,192 held, next → RZ-STS-004 `skipped`; slot released at reply, not caller timeout (fake rueidis-like conn with delayed replies); release on connection reset; goroutine count ≤ ceiling + constant |
| `Consume` grouping (fake driver and memory) | [GCRA k1, Quota k1] → one trip `script_multi`, n=2; [GCRA k1, Quota k2 other slot] → n=1 then n=1; deny in first → second !Done and quota counter unchanged; merged timeout = min; failure applies to both |
| `Read` | lookup with one shard breaker open → RZ-STS-003 for both, no command sent |
| Pre-call order | unsupported beats spent budget; spent budget beats open breaker; open breaker beats full in-flight |
| `postcommit` | per-class item and byte caps drop only that class with `kind` counters; enqueue under full queue does not block and allocates 0; scheduling: saturated stores never delay an invalidation beyond one settle batch plus one in progress; plugin class gets 1 in 15 batches (M1 test with fake plugin items); empty classes skipped; open breaker → immediate drop; invalidation retried once then dropped; `Flush` stops at 5 s and counts the remainder; `Handle` retained and released per item |
| `Manager` | equal config → same driver instance; timeout-only change reuses; URL change → new driver, old closed after last `Release` and queued write; memory→redis switch; degraded reasons aggregated over two live drivers; all reasons emitted at 0 at start; `Shutdown` waits for every goroutine |
| Telemetry | `op`/`kind`/`result` label strings equal the D10 enumerations; duration observed once per attempted trip; memory driver records calls and ops but no duration |
| MAC | tampered entry, missing tag, wrong key → miss, never error; no key → entries without tag accepted |

### 6.2 Conformance suite (`statestoretest.RunConformance`, run against `memory` and, with tag `integration`, `redis` standalone and cluster)

- GCRA with requests 10, window 1 s, default burst 10 (T = 100 ms, τ = 1 s): from idle, 11 calls at one instant are admitted and the 12th is denied with RetryAfter = T = 100 ms; one more is admitted 100 ms later; over the first window exactly `requests + burst` = 20 are admitted; burst 0: one admitted instantly, then one per 100 ms; multi-limit (2/1s, 3/1m): a deny by one limit leaves every TAT unchanged; remaining and reset values per limit; key PTTL ≈ TAT − now + τ.
- Quota: reserve up to limit then deny with RetryAfter to window end; limit 0 denies; raised limit admits at once; refund decrements the charged window only and never below 0; refund after window expiry creates no key; 24h window starts at UTC midnight; TTL = window start + 2 windows.
- Skew: Node clock 0.6 × window ahead → RZ-STS-002 and WARN; 0.4 × window → correct window.
- Cache: store requires lease (second store within 1 s → not applied); names change resets partition; ninth variant evicts the oldest; lookup hit only with equal generation; invalidation strictly increases even when two bumps land in one microsecond (memory with frozen clock); revalidation lease 5 s; refresh and end-stale only with the token.

### 6.3 Property and fuzz tests (`testing.F`, seeds in `pr-fast`, long runs nightly)

- GCRA property: for random limit sets and arrival sequences on a fake clock, admissions in any window of length W never exceed `requests + burst` per limit, and sustained admission converges to `requests` per W (TEST "Required properties" Rate Limit).
- Differential property (integration): random op sequences against `redis` use the `now` returned by each reply to drive the `memory` model; decisions, retry-after and stored TATs are identical.
- Fuzz: `Slot` against a reference implementation; key escaping is injective and never emits `{`, `}` or `:`; `redis.Validate` never panics and never echoes the password; script-reply decoders (GCRA, quota, multi, HMGET) on arbitrary RESP messages never panic and map malformed input to RZ-STS-002; `ARGV` encoding round-trips `T` and `τ` exactly.

### 6.4 Integration tests (tag `integration`)

Backends: `RURALZ_TEST_REDIS_SERVER=/usr/bin/redis-server` launches local processes (this environment: redis-server 7.0.15, TLS-capable); otherwise testcontainers in CI with Redis 8, Valkey 9.0.1 or newer, Dragonfly, and three-primary Redis and Valkey clusters (TEST "Integration tests").

1. Standalone plain, `db` select, ACL user with password, wrong password → RZ-STS-002 and breaker opening on errors.
2. TLS: certificates generated in the test with `crypto/x509`; `rediss://` succeeds; unknown CA → connect failures open the breaker after 5; wrong server name fails; gauge `cleartext_hops{hop="state_store"}` 0 for TLS, 1 for `redis://`; `state_store_unauthenticated` for loopback without credentials.
3. Cluster: three local `--cluster-enabled yes` primaries joined by `CLUSTER MEET` and `CLUSTER ADDSLOTSRANGE` from Go; slot map equals `CLUSTER SLOTS`; `Slot` equals `CLUSTER KEYSLOT` for 10,000 random keys; merged GCRA+Quota sharing a slot is one `EVALSHA`; different slots are sequential; zero `CROSSSLOT` errors (`INFO errorstats`); `CLUSTER SETSLOT` migration → `MOVED` followed and map refreshed; stopping one primary opens only its shard's breaker while co-tenant shards keep answering.
4. `NOSCRIPT`: `SCRIPT FLUSH`, then 100 concurrent calls → each RZ-STS-002 with no second command per call, exactly one `SCRIPT LOAD` (`INFO commandstats`), then success; `cmdstat_eval` stays 0 across the whole suite.
5. Restart `redis-server` on the same port: reconnect within pacing, `SCRIPT LOAD` observed, breaker closes after 3 successes; stopping it: RZ-STS-002 then RZ-STS-003.
6. Fault proxy (Go TCP proxy): black-hole → RZ-STS-001 at the timeout, breaker open after at least 20 calls, then RZ-STS-003 (GD-3/CE-4 in miniature); 500 ms delay with a lowered in-flight test ceiling (64) → plateau at the ceiling, excess `skipped`, bounded goroutines (O2 in miniature); connection reset frees slots; refused connections → at most 4 dials per second observed at the proxy.
7. Memory: `maxmemory 10mb` readings, `used_memory` above 70% → store admission false; `maxmemory-policy allkeys-lru` → `state_store_eviction_policy` on, back to `noeviction` → off; `maxmemory` tiny with `noeviction` → GCRA write gets `OOM` → RZ-STS-002; `maxmemory 0` → unbounded.
8. TTLs: GCRA `PTTL` ≈ TAT − now + τ; quota `PEXPIRETIME` = start + 2 windows; generation key 26 h; partition ≤ 25 h.
9. Round-trip test (ADR8 "Confirmation"), with the Traffic area's Filters: the counting RESP proxy sees zero commands for local denials and over-limit cache hits, at most one blocking call per admitted request per Policy, and one shared script when consumptive keys share a slot.
10. CI-only matrix: the same suites on Redis 8, Valkey 9.0.1+, Dragonfly (Lua 5.4, `TIME` in scripts, `INFO memory` fields, declared keys), Redis and Valkey clusters.

### 6.5 Chaos, disaster and benchmark gates (nightly and release)

- CE-3 (200 ms added per call: p99 rises at most one timeout, then within 10% once breakers open), CE-4 (60 s black-hole: fail-open bounds; `closed` Policies get RZ-STS-001 then RZ-STS-003), CE-5 (primary killed under 100 Nodes: reconnected within 10 s, `failureMode` at most 15 s), CE-6 (filled to `maxmemory`: stores skip above 70%, no limit key evicted), State Store side of CE-1, CE-12, CE-15 and CE-16 (zero `cache_invalidate` drops); GD-2 to GD-5 (HA "Game days"). Tooling per OQ-scalability-and-distributed-state-10 (a).
- Benchmarks: `memory` GCRA component benchmark 0 allocs/op, 2 µs p50 and 8 µs p99 with the local bucket; S5 at 40,000 rps on 4 vCPU; PB-9 (same-zone GCRA p99 ≤ 1 ms, hypothesis) and PB-14 (cross-zone ≤ 2 ms) via `ruralz_state_call_duration_seconds{op="gcra"}`; S5x with `stateStoreTimeout: 10ms`; G1 per-shard GCRA calls at 70% script CPU (and the 10-connections-per-emulated-Node variant); O2 in-flight plateau at 8,192 with skipped requests adding at most 1 ms p99.

### 6.6 Every error code path

| Code | Unit | Integration |
|---|---|---|
| RZ-STS-001 | fake conn delaying past timeout; per-request deadline shorter than policy timeout | fault proxy black-hole and 200 ms delay |
| RZ-STS-002 | reply decoder fed an error and a malformed reply; memory shard full; quota skew | `NOSCRIPT`, `OOM`, `WRONGTYPE` (key preset to a list), `NOPERM` ACL, wrong password, TLS failure, `RZBADVAL` (preset non-number TAT) |
| RZ-STS-003 | breaker forced open; half-open second caller | stopped server after 20 failures; one cluster shard down |
| RZ-STS-004 | spent budget; in-flight ceiling full | lowered ceiling with delayed proxy |
| RZ-STS-005 | fake `Store` lacking a capability (no M1 driver reports vector sets) | none in M1 |
| RZ-CFG-026 | every `Validate` rule; unresolvable secret; redis without URL | Node activation with a cluster URL under `standalone` |

## 7. Open questions blocking M1 in this area

| ID | Question | Adopt | What the code does |
|---|---|---|---|
| OQ-scalability-and-distributed-state-2 | Which `redis` topologies are Planned (M1), and how does `stateStore.url` express them? | (a) Standalone with replicas, and sharded; no Sentinel (already answered in D02 "Gateway") | `Topology` enum `standalone`/`cluster`; `ForceSingleClient` for standalone; cluster with `ShardsRefreshInterval` 10 s; `master_set` and standalone `addr` are RZ-CFG-026 |
| OQ-scalability-and-distributed-state-3 (M1 part: Response Cache) | How does a Gateway name a second State Store deployment for caches? | No option is marked. Recommended for M1: no second connection (single `Gateway.spec.stateStore`), with the interim rules of D09 and D11, matching OQ-traffic-management-and-resilience-11's current option; option (a), a cache connection under `Gateway.spec.stateStore`, for M3 | One `Store` per Gateway; startup WARN when `cache` and a limit type share it; rules 1 to 3 via `AdmitStoreBytes`; `Manager` keyed so a second role can be added without API change |
| OQ-scalability-and-distributed-state-10 | Which fault-injection tooling runs the chaos experiments? | (a) A TCP fault proxy and signals in the harness (with OQ-testing-and-quality-strategy-3 (a), a Ruralz Go proxy) | `statestoretest.FaultProxy` (delay, black-hole, reset, refuse, per-connection rules) and process signals in the chaos harness |
| OQ-scalability-and-distributed-state-11 | Do packs 8.8, 8.11 and 8.7 adopt the listed amendments (`config.localOnly`, -16 (b), -20 (a), -19 (c), `rzplg:`)? | (a) All (recommended) | No State Store call for `config.localOnly` Policies and past-budget first-seen keys (Traffic area); `rzplg:` prefix reserved; nothing else changes here |
| OQ-traffic-management-and-resilience-11 | Response Cache isolation, invalidating unsafe methods, `s-maxage` | (c) interim rules (current) for M1; (a) recommended once a second store exists | Requirements 52 to 54: `INFO memory` polling, 70% skip, growth rule, per-shard byte cap `c`, generation keys bypass |
| OQ-traffic-management-and-resilience-16 | Ceiling derivation and fail-open refill | (b) recommended | None here (local buckets); fail-open follows any State Store failure code |
| OQ-traffic-management-and-resilience-19 | Node count for derived ceilings and first-seen budget | (c) recommended (persisted count; pack 8.11) | None in M1 file mode; `Manager.SetNodeCount` feeds N_c from M2 |
| OQ-traffic-management-and-resilience-20 | May first-seen keys past budget skip GCRA? | (a) proposed | Fewer `gcra` calls; no API change |
| OQ-traffic-management-and-resilience-2 | RateLimit fields on admitted responses | (a) `onResponse` on both types | GCRA and quota replies carry remaining and reset for every limit, so either option works |
| OQ-configuration-model-8 (exit criterion 7) | Default `failureMode` of `quota` (open) and `ai.token-budget` (closed) | (a) as registered (recommended by D09 "Quotas") | Quota failures admit unmetered by default; RZ-STS only under `closed` |
| OQ-security-and-identity-22 (exit criterion 7) | Secrets and Node connections, including the State Store entry MAC key | (a) proposed | Optional HMAC-SHA-256 on cache entries (requirement 68); `RURALZ_STATE_STORE_URL` stays an allowed `env` reference |
| OQ-observability-16 | Which OpenTelemetry SDK interface exports Ruralz aggregates? | Owned by tech stack; not decided | `Recorder` interface keeps this area independent; the telemetry area implements it allocation-free |
| OQ-system-overview-15 | GCRA per admitted request or leased allowance? (blocking for this document) | Per request (current, pack 8.8) | One `gcra` call per locally admitted request |

Related, not blocking M1 but decided here: OQ-scalability-and-distributed-state-4 (a) fixed bounds (`Limits` constants); -9 (a) operator discipline; -12 and OQ-observability-18 (a) failover seen only as call errors; -13 (a) persistence SHOULD, no startup check; OQ-zero-downtime-upgrades-and-hot-reload-9 (a) keep the old connection and warn (requirement 62); OQ-security-and-identity-29 (a) cleartext allowed and reported; OQ-security-and-identity-30 (Planned (M2)) interim RZ-CFG-026; OQ-performance-budgets-and-benchmarking-12 (a) would add an in-flight gauge, not built until its name is catalogued.

## 8. Deferred (M2 and later): do not build, leave extension points

| Deferred item | Milestone | Extension point left in M1 |
|---|---|---|
| Plugin state Host Functions `state_get`, `state_incr`, keys `rzplg:<Environment name>:` plus length-prefixed Policy name, Plugin write class (D05 "Host Functions", D11 "Post-commit write queue") | M2 | Reserved `OpKind` values and prefix; `postcommit.ClassPlugin` exists and stays empty; status 401/403 mapping for `plugin` auth/authz in `Error` consumers |
| Published Node count, derived ceilings, `node_count_unknown`, `state_store_memory_multi_node` | M2 | `Manager.SetNodeCount`; `Reason` reserved |
| Token Budget reservation and settlement in `rz:qt:` (`budget`, `settle`) | M3 | Reserved `OpKind`, script codes `b`/`t` unused, settlement class shared with refunds |
| Semantic Cache: separate rueidis client with dedicated connections and own timeouts, Vector Sets or valkey-search probing, RZ-STS-005, `semantic_cache_unsupported`, CE-14 | M3 | `Capability` bits; `Supports`; `Error` 005 path tested with fakes |
| Second State Store connection (OQ-scalability-and-distributed-state-3 (a)) | M3 | `Manager` keyed by role |
| Client-side caching (caps 16 MiB per connection, 64 MiB per Node) | Later | `DisableCache: true` in one place |
| Leased allowances (OQ-scalability-and-distributed-state-1 (b)), multi-region, cross-Region rules, CE-11 | M4 | None needed beyond per-Node URL |
| Layout migration machinery: minimum Node version signal, dual-key scripts, lazy migration (REL "State Store layout changes", OQ-release-versioning-and-compatibility-9) | First layout change | Script library versioned `ruralz_v1.lua`, `ARGV[1]` format `1`; keys unversioned v1 with stable hash tags |
| Sentinel topology | Not planned (OQ-scalability-and-distributed-state-2 (a)) | `master_set` rejected |
| State Store identity in heartbeats (OQ-scalability-and-distributed-state-9 (b)), failover reporting (-12 (b)/(c)) | Later | None |
| `kubernetes` and `vault` secret providers for the URL | M2 | Resolution stays in the configuration area |
| Control mode, Ruralz Control, Control Stream, Console, OCI, WASM, gRPC, GraphQL, WebSocket, SSE, HTTP/3, AI | M2+ | Nothing in this area |

## 9. Risks and ambiguities

1. **No default for `Gateway.spec.stateStore.timeout`.** `pkg/config/v1alpha1.StateStore.Timeout` has no `+ruralz:default`, yet pack 8.7 makes it every Policy's default. Recommend `+ruralz:default=50ms` (configuration area; it changes the canonical form, so decide before the golden corpus). Code uses 50 ms meanwhile. A `stateStoreTimeout: 0` should be rejected by validation (minimum 1ms).
2. **Key layout details are unspecified but become a compatibility surface** (REL "State Store layout changes"): `<window>` rendering (canonical Go duration chosen), `<window start>` unit (Unix ms chosen), digest encoding (lowercase hex chosen), cache sub-keys and fields (chosen here). Fix them in D11 or D09 before 0.1.0.
3. **`consumerQuota` and Consumer `quotas[].name` have no pattern**; a `{`, `}` or `:` would break hash tags or keys. Escaping is specified; recommend a pattern in the schema.
4. **Per-request deadline start** is not defined (pack 8.7: "the per-request deadline (the Route's largest `stateStoreTimeout`)"). Starting at request start would make `onRequestBody` calls (M3) always late; starting at the first blocking call is proposed. Also which Policies count toward "the Route's largest": only State Store-capable types is proposed.
5. **rueidis internals**: it does not export its slot map (breaker attribution uses a Ruralz `CLUSTER SLOTS` map that may lag 10 s, and announced addresses may differ from rueidis's, which prefers `CLUSTER SHARDS` on Redis 8+); it returns at the caller's deadline while an unobservable goroutine waits for the reply (hence the per-call goroutine design, a small CPU cost at S5 rates); cluster construction fails when no seed answers (background construction required); it uses one `TLSConfig.ServerName` for every cluster node (per-destination server name proposed; managed clusters need certificates valid for the seed name).
6. **`rueidis.ParseURL` accepts `skip_verify`, `protocol`, `client_cache`, `max_retries`, timeouts and `unix`**; without explicit rejection a URL could disable certificate verification or retries. The rejection list is a proposal the configuration area should register in D02 (RZ-CFG-026 reasons).
7. **MOVED/ASK redirects are extra network round trips inside one call**, arguably against the letter of pack 8.7 rule 1; bounded by the timeout and `MaxMovedRedirections: 3`. Recommend D11 state it explicitly.
8. **Response Cache lookup uses `HMGET`, not a "partition-slot script"** (D09): equivalent because the Node computes the variant from the names it holds; the store evicts through hash fields, since Dragonfly forbids undeclared keys. Recommend D09 adopt the field layout.
9. **Missing metric labels**: no `op` for revalidation lease, 304 refresh and end-stale (mapped to `cache_set`); no drop reason fits a queue-full cache store (counted only as `ruralz_state_writes_dropped_total{kind="cache_set"}`, although D11 says "counted as a skipped store"); Plugin write drop `kind` (M2); span attribute names beyond `ruralz.state.batch`. The D10 catalog check will fail on any new name, so propose additions to Observability.
10. **`state_store_breaker_open` in half-open** and **idle half-open shards**: the documents do not say; including half-open and probing with `PING` is proposed so the page alert (for 5 m) neither flaps nor sticks.
11. **`state_store_memory_fallback` semantics** ("No State Store, Node count unknown") do not say whether an explicit `driver: memory` is degraded; fallback only is proposed.
12. **Loopback cleartext** raises `cleartext_hop`, a ticket alert for `state_store`, even for a development T1 Node on `redis://127.0.0.1`. D08 has no loopback exemption for TB-4 (OQ-observability-20 covers only OTLP); follow the documents, flag for Security.
13. **Uncredentialed non-loopback URL code** is OQ-security-and-identity-30 (blocking M2); M1 uses RZ-CFG-026 as the interim. Loopback judged on the literal host only.
14. **No TLS CA or client-certificate fields** for the State Store, although D08's transport table mentions "optional client certificate": M1 uses system roots (`SSL_CERT_FILE`, `SSL_CERT_DIR`) and no client certificate.
15. **`memory` driver bounds are absent from the documents** (SG-7 requires one); 1,048,576 entries and 64 MiB with `noeviction`-like refusal are proposed and need a D11 table row.
16. **GCRA convention and precision**: the documents give τ and `requests + burst` per window but not the exact inequality; virtual scheduling (`tat − now ≤ τ`) is the one that yields `requests + burst` for every burst from 0 to `requests`. Storing TATs as doubles relative to a 2026 epoch keeps about 8 ns resolution; very high rates (T under 1 µs) lose accuracy. A validation cap on `requests / window` may be wanted.
17. **Quota window alignment**: `720h` aligns to the Unix epoch, not calendar months (OQ-traffic-management-and-resilience-10 owns months); sub-second windows are legal in the schema but meaningless; skew beyond half a window becomes RZ-STS-002.
18. **Timed-out calls may still apply** (requirement 34): a `closed` quota can reject a request whose unit was charged. This is within "over-charge, never under-charge" but should be documented in D09.
19. **Post-commit queue ownership**: D11 defines it and D03 points to D11; this spec takes it. If the orchestration assigns it to the Data plane area, only the package owner changes.
20. **"auth throttling" as a State Store consumer** (task focus) contradicts D08: `auth.basic` throttling is per-Node and client auth types never call the State Store. No State Store API is provided for it.
21. **Pack 8.7 says "hash slot", CAP says "hash tag"** for merging; slot grouping is specified (a superset). In standalone every key could merge, but slot grouping keeps behavior identical across topologies.
22. **OQ-scalability-and-distributed-state-3** has no marked option; the M1 recommendation (no second connection) must be recorded by its owner (configuration-model) to meet exit criterion 7.
23. **OQ-security-and-identity-22 (a)** does not name the MAC key setting, its scope (cache entries only here) or rotation; a rotation turns every entry into a miss.
24. **Environment limits**: no Docker here, so the testcontainers matrix (Redis 8, Valkey 9.0.1+, Dragonfly, clusters) runs only in CI; the local `redis-server` 7.0.15 is below the documented test floors and cannot check Dragonfly's Lua 5.4 or `INFO` differences. The local harness still covers standalone, TLS, ACL and a three-primary cluster.
25. **SHA-1 for script identifiers** needs a gosec suppression with a reason; it is not a security use and remains available under `GOFIPS140`.
26. **Hot Reload of a `memory` store switched to `redis`** loses all counters (one extra limit per open window), like a new Cell; during a Zero-Downtime Upgrade two processes each hold `memory` state, doubling limits for the overlap.
