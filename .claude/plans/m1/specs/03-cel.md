# M1 spec, area 3 of 11: CEL expressions

Design reader output for milestone M1 "Core gateway". Sources read in full for this area: `docs/architecture/02-configuration-model.md` (CM), `docs/adr/0011-expressions-and-authorization-engines.md` (ADR-0011), `docs/architecture/03-data-plane.md` (DP), `08-security-and-identity.md` (SEC), `09-traffic-management-and-resilience.md` (TM), `10-observability.md` (OBS), `12-performance-budgets-and-benchmarking.md` (PB), `docs/engineering/01-tech-stack-and-libraries.md` (TS), `02-repository-layout-and-conventions.md` (RL), `03-testing-and-quality-strategy.md` (TQ), `docs/roadmap/01-roadmap-and-milestones.md` (RM), `docs/_meta/foundation-pack.md` (FP), plus the M0 code (`pkg/config/v1alpha1`, `api/schema`, `internal/errcode`, `.golangci.yml`) and the sibling specs `01-config-load.md` and `02-config-revision.md`.

Conventions: "(target)" and "(hypothesis)" values are the documents'; "(proposed)" marks a decision this spec makes where the documents are silent or conflict (each is listed in section 9). "(measured)" values come from a throwaway probe of `cel.dev/cel-go` v0.32.0 on the 4-CPU build host (Go 1.24, outside the repository); they are evidence, not budgets.

## 1. Scope

| # | M1 item | Source (doc, section) |
|---|---|---|
| S1 | One CEL module on `cel.dev/cel-go` v0.32.x, linked into `ruralzd`, `ruralz` (and `ruralz-control` when it lands) so all three give identical diagnostics | RM "M1 scope" CEL row ("CEL module"); ADR-0011 "Decision outcome" rows *CEL module*, *Binaries*; TS "Library catalog" row *Expressions*, "Dependency graph" |
| S2 | Compile, type-check and cost estimator in every CEL place: `Route.spec.match.when`, `Policy.spec.when`, `Route.spec.composition.steps[].pathExpression`, `Route.spec.composition.steps[].when`, `Upstream.spec.retries.retryOn`, `Upstream.spec.circuitBreaker.failureWhen`, `Upstream.spec.loadBalancing.hashKey`, `Gateway.spec.telemetry.accessLog.when`, `authz.cel` `config.rule`, `ratelimit`, `quota` and `cache` `config.key`, `headers` `valueExpression` | RM "M1 scope" CEL row; CM "CEL expressions and allowed places" > "Allowed places" |
| S3 | `transform.request` and `transform.response` `config.body` and `config.set[].valueExpression` (Planned (M1) in the CEL table; registered from DP) | CM "Allowed places"; CM "Registered from feature documents"; DP "Transform Policies"; RM "M1 scope" Traffic row (`transform.request`, `transform.response`) |
| S4 | Variables and types per place (`request`, `source`, `route`, `consumer`, `auth`, `now`, `response`, `error`, `upstream`, `attempt`, `steps`, `duration`) | CM "Variables" |
| S5 | RZ-CFG-014 (syntax, type, unavailable variable) and RZ-CFG-015 (static cost over 10,000 units at nominal sizes: strings 256, lists and maps 32) | CM "Limits"; CM "Error codes"; ADR-0011 row *CEL cost* |
| S6 | Runtime bounds: cel-go `CostLimit` 1,000,000 units; `ContextEval` at the request deadline | CM "Limits"; ADR-0011 row *CEL cost* |
| S7 | Standard library plus strings and encoders extensions only; no side effects, I/O or randomness; no Lua | CM "Limits"; ADR-0011 row *CEL places*, *Lua* |
| S8 | Programs compiled once per Revision into the snapshot; typical match and key expressions under 2 µs at p99 (target) | CM "Limits"; DP "Configuration snapshots and hot reload" > "Activation" step 2; FP 8.x via System overview "Compile before swap" |
| S9 | Runtime-error rule per place (fail safe) with the RZ codes of the owning areas | CM "Allowed places" (runtime-error column is normative); DP "Error handling", "Failure semantics", "RZ-RT registry"; TM "Failure matrix", "Error codes this document owns"; SEC "RZ-AUTH decision codes" |
| S10 | `authz.cel` (Security Policies Engine, first part): false denies 403 RZ-AUTH-010 | RM "M1 scope" Security row; SEC "Authorization"; ADR-0011 row *`authz.cel`*; FP section 10 |
| S11 | CEL reading `request.body` or `response.body` is a body gate trigger; `steps` reads buffer step bodies | CM "Body buffering and limits"; RM "M1 scope" Configuration row ("body buffering") |
| S12 | Golden corpus: the example Bundle, including `authz-orders` (`split` and `exists`), passes RZ-CFG-015 | CM "Limits"; RM "M1 scope" Configuration row; ADR-0011 "Confirmation" |
| S13 | Conformance negative fixtures for RZ-CFG-014 and RZ-CFG-015, with file, line and column | TQ "Conformance suites" (Configuration row: "CEL cost ... fixtures"); ADR-0011 "Confirmation" |
| S14 | Fuzz target "CEL compile and cost estimator" | TQ "Fuzzing"; ADR-0011 "Confirmation"; RM "M1 scope" Quality row |
| S15 | CEL microbenchmarks with worst-case fixtures at the caps; alloc/op gate | RM "M1 scope" Budgets row ("component and CEL benchmarks"); PB "Per-stage latency budget" rule 3; TQ "Benchmarks and regression gates"; ADR-0011 "Confirmation" |
| S16 | `ruralz_upstream_cel_errors_total` (`upstream`, `field`: hashKey, retryOn, failureWhen) and the other metrics CEL failures feed | OBS "Ruralz Gateway metrics"; RM "M1 scope" Observability row |
| S17 | Features that are CEL in M1: catch-all fallback (`when: "true"`), header and query routing (`match.when` over `request.query`), conditional and sequential composition, tiered rate limits (`when`), user-agent filtering (`authz.cel`), CEL-built bodies | TM "Traffic shaping"; feature catalog rows cited in section 8 of `docs/features/01-feature-catalog.md` (Sequential composition, Conditional requests, CEL authorization rules, User-agent filtering, CEL-built Upstream request bodies, CEL response shaping) |
| S18 | Default `retries.retryOn` and `circuitBreaker.failureWhen` behavior, expressible as CEL | TM "Deadlines" (proposed defaults table) |
| S19 | Validation-only compile of later-milestone places that already exist in the schema and the golden corpus: `AIModel.spec.candidates[].when` (M3), `ai.semantic-cache` `config.key` (M3), `Upstream.spec.messaging.key` (M4) | CM "Allowed places"; CM "Complete annotated example Bundle" (`secondary` candidate `when`, `semantic-cache-support` key) |

## 2. Normative requirements

### A. Library, build and boundaries

1. The module MUST use `cel.dev/cel-go` pinned at v0.32.0 in `go.mod` (catalog floor v0.32.x). `github.com/google/cel-go` stays banned (the existing depguard `banned` rule). [ADR-0011 "Decision outcome" row *CEL module*; TS "Library catalog" *Expressions*; RL "Banned imports"]
2. Only `internal/cel` (and its test helper `internal/cel/celtest`) MAY import `cel.dev/cel-go/...`; a depguard rule `celgo` (`files: ["$all", "!**/internal/cel/**"]`, deny `cel.dev/cel-go`) MUST be added beside `wazero`/`rueidis`/`raft`, and `cel.dev/cel-go` added to the `admitted` allow list in the same pull request (proposed, S7). No cel-go type appears in an exported identifier of `internal/cel`. [TS "Selection criteria" S7, "Admission"; RL "Where Go code goes", "Import boundaries"]
3. One compiler serves all binaries: `ruralz bundle validate`, Ruralz Control ingest (M2) and Node activation MUST produce byte-identical RZ-CFG-014/015 diagnostics for the same input. [CM "Validation and diff semantics"; TQ "Conformance suites"]
4. Enabled language surface, exactly: the CEL standard library (`cel.NewEnv` default, standard macros `has`, `all`, `exists`, `exists_one`, `map` (2 and 3 argument), `filter`), `ext.Strings(ext.StringsVersion(5))` and `ext.Encoders(ext.EncodersVersion(1))`. Versions are pinned so a cel-go upgrade cannot silently add functions. NOT enabled: `cel.OptionalTypes` (and optional syntax `?.`, `[?`), `ext.Bindings`, `ext.TwoVarComprehensions`, `ext.Lists`, `ext.Sets`, `ext.Math`, `ext.Network` (`ip`, `cidr`), `ext.NativeTypes`, `ext.Protos`, `ext.Regex`, any HMAC or crypto library, cel-go async functions (`ConcurrentEval`, `AsyncCallObserver`), and any Ruralz or Plugin-defined function. Adding any of these amends ADR-0011. [CM "Limits"; ADR-0011 row *CEL places*, "Confirmation" (review checklist)]
5. Compile-time literal validators MUST be enabled: `cel.ValidateDurationLiterals()`, `cel.ValidateTimestampLiterals()`, `cel.ValidateRegexLiterals()`; their findings are RZ-CFG-014 (proposed; they add no function). `cel.CrossTypeNumericComparisons` stays at its default (off); runtime heterogeneous comparisons of `dyn` JSON numbers already work (measured). Parser limits stay at cel-go defaults: recursion depth 250, 100,000 code points, 100,000 expression nodes; exceeding one is RZ-CFG-014.
6. No side effects, I/O or randomness at evaluation:
   1. No function beyond requirement 4 is registered; activations are read-only views.
   2. The timestamp accessors with a time-zone argument (`getHours(tz)` and the other `*_with_tz` overloads) call `time.LoadLocation`, which reads zone data from disk on every evaluation (cel-go `common/types/timestamp.go`, `common/stdlib/standard.go`). A time-zone argument MUST therefore be a string literal equal to `"UTC"` or matching `^[+-]([01][0-9]|2[0-3]):[0-5][0-9]$`; a named zone or a non-literal argument is RZ-CFG-014 with hint "use a UTC offset such as \"+02:00\"" (proposed).
   3. Every map Ruralz supplies (headers, query, path parameters, labels, `steps`, decoded JSON objects, claims) MUST iterate keys in ascending byte order, so `map`, `filter`, `exists_one` errors and `join` results are deterministic (proposed).
   4. `now` is fixed per request (request start), never read from the clock during evaluation.
   [CM "Limits": "no side effects, I/O or randomness"]
7. Programs are built with `cel.EvalOptions(cel.OptOptimize)` (constant folding and literal regex precompilation) (proposed, performance). Env and Program construction happen only at compile time, never on the request path. [CM "Limits"; FP 8.x "Compile once, read lock-free" via System overview "Design principles"]
8. The linked set gained through cel-go MUST pass depgate G1 to G3 in stage 6 (section 5 lists it); no `crypto/...` package is linked by cel-go v0.32.0 (measured with `go list -deps`). [TS "Selection criteria" G1-G3, "Admission"]

### B. Places and environments

9. CEL is accepted only in fields whose schema carries `x-ruralz-cel`; elsewhere CEL-like text is a plain string. `internal/cel` owns an explicit place registry (a constructed table returned by a function, no package variable) with exactly the rows of the table below. Every `x-ruralz-cel` annotation in `api/schema/ruralz/v1alpha1/rendered.schema.json` (21 today) MUST map to exactly one row whose `Result` equals the annotation's `result` and whose variable union equals the annotation's `variables`, and vice versa (CI test). [CM "Allowed places", "Schema keywords that drive tooling"; RL "Markers"]

   *Base* = `request`, `source`, `route`, `consumer`, `auth`, `now`. "Req body" / "Resp body" say whether `request.body` / `response.body` may be selected.

   | Place ID | Schema def | Result | Variables (by scope) | Req body | Resp body | Evaluated | Runtime error rule | Runtime |
   |---|---|---|---|---|---|---|---|---|
   | `Route.spec.match.when` | `RouteMatch.when` | bool | `request`, `source`, `now` | no | n/a | Route matching, per candidate Route whose other criteria matched | 500 RZ-RT-006, no fallthrough | M1 |
   | `Policy.spec.when` | `PolicySpec.when` | bool | Base; `response` iff the Policy's first Phase at that attachment is a response Phase | iff the Policy gates the request body at that scope (req. 12) | iff type `transform.response` | Once, before the Policy's first Phase | `closed`: Policy runs; `open`: skipped | M1 |
   | `Route.spec.composition.steps[].pathExpression` | `CompositionStep.pathExpression` | string | Base, `steps` | yes | n/a | Before the step | Step fails; `optional` decides | M1 |
   | `Route.spec.composition.steps[].when` | `CompositionStep.when` | bool | Base, `steps` | yes | n/a | Before the step | Step fails; `optional` decides | M1 |
   | `authz.cel config.rule` | `AuthzCELConfig.rule` | bool | Base | yes (selects onRequestBody, req. 13) | n/a | onRequestHeaders or onRequestBody | 403 RZ-AUTH-015 (false: 403 RZ-AUTH-010) | M1 |
   | `ratelimit config.key` | `RateLimitConfig.key` | string | Base | no | n/a | onRequestHeaders | `failureMode` (closed: 503 RZ-RL-005) | M1 |
   | `quota config.key` | `QuotaConfig.key` (default `consumer.name`) | string | Base | no | n/a | onRequestHeaders | `failureMode` (closed: 503 RZ-RL-005) | M1 |
   | `headers config.request.set[].valueExpression` | `HeaderRequestSet.valueExpression` | string | Base (all scopes) | no | n/a | onRequestHeaders (G, R); onUpstreamRequest (U) | `failureMode` (closed: 503 RZ-RT-011) | M1 |
   | `headers config.response.set[].valueExpression` | `HeaderResponseSet.valueExpression` | string | Base, `response`, `upstream` (all scopes) | no | no | onResponse (G, R); onUpstreamResponseHeaders (U) | `failureMode` (closed: 502 RZ-RT-012) | M1 |
   | `cache config.key` | `CacheConfig.key` | string | Base | no | n/a | onRequestHeaders | Cache bypassed | M1 |
   | `transform.request config.body` | `TransformRequestConfig.body` | dyn | Base; `upstream` only at U | yes | n/a | onRequestBody (G, R); onUpstreamRequest (U) | `failureMode` (closed: 503 RZ-RT-011) | M1 |
   | `transform.request config.set[].valueExpression` | `TransformRequestSet.valueExpression` | string | as above | yes | n/a | as above | as above | M1 |
   | `transform.response config.body` | `TransformResponseConfig.body` | dyn | Base, `response`; `upstream` only at U | no | yes | onResponse (G, R); onUpstreamResponseBody (U) | `failureMode` (closed: 502 RZ-RT-012) | M1 |
   | `transform.response config.set[].valueExpression` | `TransformResponseSet.valueExpression` | string | as above | no | yes | as above | as above | M1 |
   | `Upstream.spec.loadBalancing.hashKey` | `LoadBalancing.hashKey` | string | Base | no | n/a | Endpoint selection, once per leg (req. 45) | Random Endpoint | M1 |
   | `Upstream.spec.retries.retryOn` | `Retries.retryOn` | bool | `request`, `response`, `error`, `attempt`, `upstream` | no | no | After each attempt | No retry | M1 |
   | `Upstream.spec.circuitBreaker.failureWhen` | `CircuitBreaker.failureWhen` | bool | `request`, `response`, `error`, `upstream` | no | no | After each attempt | Counted as failure | M1 |
   | `Gateway.spec.telemetry.accessLog.when` | `AccessLog.when` | bool | Base, `response`, `upstream`, `duration` | no | no | onLog | Entry written | M1 |
   | `ai.semantic-cache config.key` | `AISemanticCacheConfig.key` | string | Base, `ai` | no | n/a | onRequestBody | Cache bypassed | M3 (validate in M1) |
   | `AIModel.spec.candidates[].when` | `AIModelCandidate.when` | bool | Base, `ai` | no | n/a | Model routing, after onRequestBody | Candidate skipped | M3 (validate in M1) |
   | `Upstream.spec.messaging.key` | `Messaging.key` | string | Base | no | n/a | onUpstreamRequest | 502 | M4 (validate in M1) |

   [CM "Allowed places" (every column); DP "Transform Policies" (per-scope Phase and variables table); TM "Load balancing", "Retries", "Circuit breakers", "Failure matrix"; OBS "Access logs"]
10. The environment of one occurrence (a *site*) declares only the variables available there; referencing any other Ruralz variable is RZ-CFG-014 "unavailable variable" (cel-go "undeclared reference"), with the hint naming the place and its available variables (for example `response` in `Route.spec.match.when`). [CM "Limits"]
11. **Policy first Phase** (input to `Policy.spec.when`, computed by the chain resolver, `internal/config/precedence`): auth, `authz.ip`, `ratelimit`, `quota`, `cors`, `cache`: onRequestHeaders; `authz.cel`: per requirement 13; `validation.json-schema`: onRequestBody; `headers`: G/R onRequestHeaders when `config.request.set` is non-empty, else onResponse; U onUpstreamRequest when request ops exist, else onUpstreamResponseHeaders; `transform.request`: G/R onRequestBody, U onUpstreamRequest; `transform.response`: G/R onResponse, U onUpstreamResponseBody; `auth.upstream-oauth2`: onUpstreamRequest. Response Phases are onUpstreamResponseHeaders, onUpstreamResponseBody and onResponse. [CM "Policy" registry and "Allowed places"; DP "Phases", "Transform Policies"; 02-config-revision R-13a]
12. **Body availability** (proposed resolution of CM "Variables": body "in body Phases and composition only"): `request.body` MAY be selected only in composition places, `transform.request` places, the `authz.cel` rule, and `Policy.spec.when` of a Policy that itself gates the request body at that attachment (`validation.json-schema`, `transform.request`, an `authz.cel` whose rule reads the body). `response.body` MAY be selected only in `transform.response` places and `Policy.spec.when` of a `transform.response` Policy. Any other selection of `body` on `request` or `response` (including inside `has()`) is RZ-CFG-014 "request.body is not available in <place>". The check uses checked types (a Select node with field `body` whose operand type is `ruralz.Request` or `ruralz.Response`), so shadowing cannot evade it. [CM "Variables", "Body buffering and limits"; DP "Transform Policies"]
13. **`authz.cel` Phase**: the rule is compiled in the `authz.cel config.rule` environment (body allowed); if the checked AST selects `request.body`, the Policy runs in onRequestBody (a gate), otherwise in onRequestHeaders, never both. The module exposes this as `(*Compiler).ReferencesRequestBody(expr)`, the `registry.BodyOracle` of 02-config-revision (R-13a). [CM "Allowed places" (*Evaluated* "onRequestHeaders or onRequestBody"), "Body buffering and limits" ("`authz.*` in `onRequestBody`"); SEC "OPA and Cedar schemas" (input `body` only in `onRequestBody`)]
14. **`steps`** (proposed validation tightening, see section 9): `steps` is declared in both composition places; a reference to `steps` in a Route whose `composition.mode` is `aggregate` or `conditional`, or a constant key (`steps.x`, `steps["x"]`) that is not the name of an earlier step of a `sequential` composition, is RZ-CFG-014. A non-constant key (`steps[request.headers["k"]]`) is allowed and marks every earlier step as read. [DP "Composition engine"; CM "Route"]
15. **Site derivation and deduplication.** CEL fields on a Route, Upstream or Gateway compile once per field. A Policy's `config` CEL fields compile once per distinct environment among the scopes it is attached at; its `when` once per distinct (scope, first Phase) environment. A diagnostic that only one attachment triggers names that attachment in its message ("when attached to Upstream/orders"), choosing the first attaching resource in canonical order (kind, then name). An unattached Policy compiles at the first scope its registry row allows in the order G, R, U (proposed). Fields of a Policy whose type is not allowed at a scope (RZ-CFG-020) are not compiled for that scope. [CM "Attachment and precedence", "Worked example"; 02-config-revision "precedence"]
16. An empty-string CEL field is treated as absent (the Go types use `omitempty` strings); a whitespace-only or comment-only field is RZ-CFG-014 (proposed). `${VAR}` in a CEL field is RZ-CFG-011, raised by substitution before this module sees the text; the module never substitutes. [CM "Environment substitution"; 01-config-load req. 28]

### C. Variables and types

17. Variables are declared with Ruralz object types from a custom `types.Provider` (no protobuf code generation, no `ext.NativeTypes`); field access goes through `types.FieldType.GetFrom` accessors over Ruralz view structs (no reflection). The type names are part of the contract (they appear in diagnostics and in `type(x)`):

   | Variable | CEL type | Fields and types |
   |---|---|---|
   | `request` | `ruralz.Request` | `method` string, `scheme` string, `host` string, `path` string, `pathParams` map(string, string), `query` map(string, string), `headers` map(string, string), `body` dyn |
   | `source` | `ruralz.Source` | `ip` string, `port` int, `tlsVersion` string, `clientCertSubject` string |
   | `route` | `ruralz.Route` | `name` string, `labels` map(string, string) |
   | `consumer` | `ruralz.Consumer`, nullable | `name` string, `tier` string, `tags` list(string), `labels` map(string, string), `quotas` list(string) |
   | `auth` | `ruralz.Auth`, nullable | `method` string, `claims` dyn |
   | `response` | `ruralz.Response`, nullable | `status` int, `headers` map(string, string), `body` dyn |
   | `error` | `ruralz.Error`, nullable | `kind` string |
   | `upstream` | `ruralz.Upstream`, nullable | `name` string, `endpoint` string |
   | `attempt` | int | attempt number from 1 |
   | `steps` | map(string, `ruralz.Step`) | `ruralz.Step`: `status` int, `headers` map(string, string), `body` dyn |
   | `duration` | `google.protobuf.Duration` | request duration |
   | `now` | `google.protobuf.Timestamp` | request start time, UTC |
   | `ai` (M3; declared in M1 for validation) | `ruralz.AI` | `model` string, `estimatedInputTokens` int, `maxOutputTokens` int, `stream` bool |

   [CM "Variables"] The variable named `duration` coexists with the standard function `duration("1s")` (measured: `response.status >= 400 || duration > duration("1s")` compiles and evaluates).
18. **Nullability.** A nullable variable resolves to CEL `null` when absent: `consumer` before auth or when anonymous or unbound; `auth` before auth or with no auth-class Policy; `response` when the attempt got no response (and in accessLog when the client connection ended before any response); `error` when a response arrived; `upstream` when no leg ran (short-circuit, no Route). `x != null` and `x == null` type-check for these object types (measured). Selecting a field of `null` is a runtime error. [CM "Variables"; SEC "Authentication" Figure 2]
19. **Header maps** (`request.headers`, `response.headers`, `steps[].headers`): keys are lowercased field names; repeated fields are joined by `", "` in received order (RFC 9110); lookup is case-insensitive over the canonical `http.Header` and SHOULD NOT allocate for a single-valued field (case-insensitive scan, no `CanonicalMIMEHeaderKey` allocation, proposed); `size()` counts distinct names; `in` tests presence; iteration is by ascending lowercased name. `request.headers` never contains `Host` (it is `request.host`). [CM "Variables"]
20. **`request` fields.** `method` as received; `scheme` `"http"` or `"https"` (listener protocol); `host` lowercased with the port stripped (the Router's normalized host); `path` the one normalized path the Router, `authz.*` and the Upstream use (unreserved percent-escapes decoded, dot segments removed, encoded slash kept) [DP "Compiled structure"; SEC "Request hardening" (Path confusion)]; `pathParams` the `template` captures by name (empty map otherwise); `query` parsed from the raw query with `url.ParseQuery` semantics, values percent-decoded, a repeated parameter joined by `","` (proposed; see section 9), parsed lazily on first access and cached in the view; `body` the decoded JSON body (req. 25).
21. **Which request `request` is** (proposed): before routing (`match.when`), the normalized client request; in client-leg places, the client request as modified by earlier client-leg Policies in chain order; in upstream-leg places (`retryOn`, `failureWhen`, `hashKey`, Upstream-scoped Policy fields and `when`, composition `pathExpression` and `when`), that leg's outgoing request as modified by earlier upstream-leg Policies, except that `scheme`, `host` and `pathParams` always describe the client request. A Transform Policy's CEL fields all read the body "as it reached the Policy". [DP "Transform Policies"; DP "Short-circuit"]
22. **`source`**: `ip` is the client address after trusted-proxy handling (`Gateway.spec.trustedProxies`, `listeners[].proxyProtocol`), IPv4-mapped IPv6 unmapped to IPv4, IPv6 in RFC 5952 form; `port` the peer port (0 when taken from a forwarding header without a port); `tlsVersion` `"1.2"` or `"1.3"`, `""` on cleartext (proposed, matches `tls.minVersion` spellings); `clientCertSubject` the verified client leaf subject as RFC 4514 text, `""` when none. [CM "Gateway" (`trustedProxies`); SEC "IP filtering and GeoIP" (Client address)]
23. **`route`**: the matched Route's `metadata.name` and `metadata.labels`. **`consumer`**: the bound Consumer's `metadata.name`, `spec.tier` (`""` when unset), `spec.tags` (sorted), `metadata.labels`, and the names of `spec.quotas` (sorted). **`auth`**: `method` is the authenticating type without the `auth.` prefix: `"jwt"`, `"api-key"`, `"basic"`, `"mtls"` (proposed); `claims` is the verified JWT claim set as JSON `dyn` for `jwt`, and an empty map for the other methods (proposed). Route and Consumer views, with their labels, tags and quota lists as CEL values, are built once per snapshot and shared read-only. [CM "Variables", "Consumer"; SEC "Consumers and tiers"]
24. **`response`, `error`, `upstream`, `attempt`, `steps`, `duration`, `now`**: `response` is the leg attempt's response headers and status in `retryOn`/`failureWhen`/U-scope Phases, and the final client response (after composition merge; Node-generated responses included) in client-leg response Phases and `accessLog.when`; `error.kind` is one of `connect`, `timeout`, `reset`, `tls` (only attempt, leg or Route deadlines give `timeout`); `upstream` is `{name, endpoint}` with `endpoint` the selected Endpoint address `host:port`, the current leg in upstream-leg places and the last leg in client-leg response Phases and `accessLog.when`; `attempt` counts from 1 and names the attempt just finished; `steps` holds completed steps only (skipped, failed-optional and not-yet-run steps are absent: `has(steps.x)` or `"x" in steps` tests presence); `duration` is total request time (session lifetime for sessions), the access log `duration`; `now` is request start. [CM "Variables"; TM "Deadlines", "Retries"; DP "Composition engine"; OBS "Access logs"]
25. **JSON values** (`request.body`, `response.body`, `steps[].body`, `auth.claims`): decoded once per body and shared by every reader. A `Content-Type` other than `application/json` or `+json` gives `null`. Objects decode to Ruralz ordered maps (keys iterate in ascending byte order); integer literals within int64 decode to CEL `int`, all other numbers to `double` (proposed, keeps 64-bit IDs exact); strings, booleans, arrays and null map directly. The decoder MUST report the bytes of values it builds so the caller charges them against `limits.maxBufferedBytes`, and MUST stop with an oversize error past 4 times the raw limit the body arrived under (target). Malformed JSON under a JSON content type makes the variable an error value: an expression that reads it gets a runtime error (fail safe, proposed). Duplicate object keys: the last wins (proposed). [CM "Body buffering and limits"; DP "Transform Policies", "Streaming"]

### D. Compile and validation (RZ-CFG-014)

26. The Configuration pipeline stage "Compile CEL and check cost" (CM Figure 3, stage J) runs after effective Filter Chains are computed (stage I), because site environments depend on attachment scope and first Phase. Per site, in order: parse, type-check, literal validators (req. 5), Ruralz AST checks (unavailable body, time-zone literals, `steps` keys), result-type check, cost estimate (section E), and, on a Node only, program planning. [CM "Validation and diff semantics"; 01-config-load req. 41 and 51]
27. RZ-CFG-014 ("CEL syntax, type or unavailable-variable error", no HTTP status; a Node rejects the Revision) covers: parse errors, including disabled syntax (`?.`, `[?`) and parser limits; undeclared references (unavailable variables, and disabled extension functions such as `cel.bind`, `math.greatest`, `ip`, which surface as "undeclared reference" or "found no matching overload"); undefined fields (`request.bogus`); no matching overload (`request.method + 1`); requirement 5, 6.2, 12 and 14 violations; and a result-type mismatch (req. 29). [CM "Limits", "Error codes"]
28. Diagnostics: one `Issue` per cel-go issue, in cel-go order, then Ruralz issues by position. Each carries the code, the expression-relative byte offset (for the loader's source mapping; -1 when not positional), 1-based line and column, a deterministic message (cel-go's first line, prefixed `CEL <place>: `) and an optional hint. Messages MUST NOT depend on map iteration, pointer values or timing. [CM "Diagnostics and source map"; 01-config-load `CELIssue`]
29. Result types: a `bool` place accepts output type `bool` or `dyn`; a `string` place accepts `string` or `dyn`; a `dyn` place accepts any type. Any other output type is RZ-CFG-014 "expression has type int; <place> needs string (use string(...))". A `dyn` output is checked at runtime (req. 41). [CM "Schema keywords" (`x-ruralz-cel` result)]
30. Validation of a whole Bundle reports every CEL error in one run and never stops at the first; compile work is split into units of one expression so the loader's W workers (max(1, GOMAXPROCS/2) on a Node, GOMAXPROCS in the CLI) can yield every 100 µs (target) between units. [CM "Validation and diff semantics"; PB "Hot Reload stages"; OQ-performance-budgets-and-benchmarking-6]

### E. Cost

31. **Nominal-size estimator** (`checker.CostEstimator`), applied only where cel-go cannot size a node itself (cel-go sizes literals, list and map literals and concatenations first): `EstimateSize(node)` returns `{Min: 0, Max: 256}` for type string or bytes, `{0, 32}` for list or map, and for `dyn` (and `any`, type parameters) `{0, 32}` when the node's expression ID is a comprehension iteration range or the container operand of `in`, else `{0, 256}` (proposed usage inference; see section 9); `nil` otherwise. `EstimateCallCost` returns nil for every call (no custom functions; the library estimators registered by `ext.Strings` v5 and `ext.Encoders` v1 apply). No `checker.CostOption` overrides. [CM "Limits": 256 bytes and 32 entries (target), "typed or `dyn`"; ADR-0011 "Consequences" (nominal-size estimator)]
32. RZ-CFG-015 ("CEL cost ceiling exceeded") when the estimate's `Max` exceeds 10,000 (target); `Max` equal to 10,000 passes. The message states the estimate and the nominal sizes: "CEL <place>: estimated cost 169123 exceeds 10000 at nominal sizes (strings 256, lists and maps 32)". An unbounded estimate (`Max` = 2^64-1, which `json.encode` always yields in cel-go v0.32.0, measured) is RZ-CFG-015 with "estimated cost is unbounded" and, for `json.encode`, the hint "a transform body may return the map or list itself" (proposed). [CM "Limits"; ADR-0011 row *CEL cost*]
33. Estimates are pure functions of (site environment, source text, cel-go version) and are identical in every binary; the golden corpus pins them (section 6).
34. **Runtime cost limit**: evaluation stops with a runtime error when actual cost exceeds 1,000,000 units (target), through `cel.CostLimit(1_000_000)`. Optimization that MUST NOT change behavior: at compile time a second estimate runs with an estimator that sizes nothing (cel-go treats sizes as unknown); when its `Max` is at most 1,000,000 the program's actual cost can never reach the limit, so it is built without cost tracking ("untracked"); otherwise it is built with `CostLimit` ("tracked"). A property test (section 6) checks untracked actual cost ≤ that bound. Measured: untracked evaluation of `consumer != null && consumer.tier == "gold"` 199 ns, 2 allocs; tracked 1.1 µs, 12 allocs. [CM "Limits"; ADR-0011 row *CEL cost*]
35. **Deadline**: a tracked program containing a comprehension is evaluated with `ContextEval` and `cel.InterruptCheckFrequency(100)` (proposed value), under a context whose deadline is the smaller of the request deadline and 50 ms (proposed cap; see section 9, risk on cel-go's quadratic cost tracker). Programs without comprehensions use `Eval` (cel-go checks interrupts only inside folds, so `ContextEval` would add allocations without effect). The parent context MUST be a standard-library context (or implement `AfterFunc`), so `context.WithCancel` starts no goroutine. [ADR-0011 row *CEL cost* ("`ContextEval` at the request deadline")]
36. A cost-limit stop or deadline stop is a runtime error handled by the place's rule (section G), never a panic; cel-go's `EvalCancelledError` and `InterruptError` map to `KindCostLimit` and `KindDeadline`. [TQ "Fuzzing" (oracle); CM "Allowed places"]
37. Every CEL expression of the example Bundle MUST pass RZ-CFG-014 and RZ-CFG-015 in its site: `response.status >= 400 || duration > duration("1s")`, `error != null ? error.kind in ["connect", "reset"] : response.status == 503`, `'"/orders/" + request.pathParams.orderId'`, `'"/stock?order=" + request.pathParams.orderId'`, `auth.claims.scope.split(" ").exists(s, s == "orders:read")` (measured estimate 2,087 with this estimator), `source.ip`, `consumer == null ? source.ip : consumer.name`, `'consumer == null ? "anonymous" : consumer.name'`, `consumer.name` (quota default and `semantic-cache-support`), `ai.estimatedInputTokens < 100000` and `< 50000` (overlay). Also the document examples: `request.headers["x-tenant"] == "acme"`, `request.headers["x-user"]`, `'error != null ? error.kind in ["connect", "reset"] : response.status in [502, 503]'`, `error != null || response.status >= 500`, `request.method != "OPTIONS"`, `consumer != null && consumer.tier == "gold"`, `consumer != null && "daily-tokens" in consumer.quotas`, `'"x-api-key" in request.headers'`, `string(response.body.total)`, `"true"`. [CM "Limits", "Complete annotated example Bundle"; SEC; TM; DP]

### F. Evaluation and activation

38. `Program` values are immutable and safe for concurrent evaluation (cel-go: "stateless, thread-safe, and cachable"). The module starts no goroutine. [RL "Code conventions" (Goroutines)]
39. Activation: one `Vars` per request, owned by the request goroutine (pooled with the request context), filled as the request progresses: `request`, `source`, `route`, `now` at routing (for `match.when`, a pre-route `Vars` without `route`); `consumer`, `auth` after the auth class; `response`, `upstream` in response Phases; `duration` at onLog. An upstream leg (retry loop, composition step goroutine) evaluates against its own `Vars` value copied from the client `Vars` with the leg fields (`request`, `upstream`, `attempt`, `response`, `error`) replaced; a `Vars` is never mutated while another goroutine evaluates against it. `aggregate` composition evaluates every step's `pathExpression` on the request goroutine before fan-out (proposed). Views reference request memory, so neither `Vars` nor a `Value` may outlive the request; callers copy results they keep (the access log copies fields). [DP "Goroutines", "Composition engine"; OBS "Access logs"]
40. Evaluation happens only where the place is evaluated and only when needed: `Policy.spec.when` once before the Policy's first Phase, the result reused for its later Phases (once per request for client-leg Policies, once per leg for upstream-leg Policies, reused across retries, proposed); `hashKey` once per leg; `retryOn` and `failureWhen` after each attempt; `match.when` only for a candidate Route whose `methods`, `headers` and `grpc` criteria matched, in precedence order. [DP "Ordering", "Precedence"; TM "Load balancing", "Retries", "Circuit breakers"]
41. Result conversion: a `bool` place whose value is not a CEL bool, or a `string` place whose value is not a CEL string, is a runtime error `KindResultType`. For `dyn` places (`transform.*` `config.body`), the writer emits: map or list as JSON (object keys in ascending byte order); a top-level string as its UTF-8 bytes; top-level bytes raw; int, uint, double, bool, null as JSON text (proposed); nested bytes base64, timestamps RFC 3339, durations as proto3 JSON (`"1.5s"`); NaN or infinity is `KindResultType`. [DP "Transform Policies" ("a map or list is written as JSON, a string as its bytes")]
42. Runtime error values: `EvalError{Place, Kind}`; `Error()` MUST NOT contain request data (cel-go messages can embed input, such as `strconv` errors quoting a header value). The cel-go text is available only through `Detail()`, for debug logs and tests; it never reaches a problem document, `/tap`, a span attribute or an info-level log. [DP "Error response format" ("never echoes request content"); OBS "Access logs" (O4); SEC "Secrets" rule 2]

### G. Runtime error semantics per place

43. Every runtime error (cel-go error value, cost limit, deadline, result type, body decode) takes the place's rule. No authentication or authorization step is skipped because an expression errored. [CM "Allowed places" (runtime-error column is normative and fails safe)]

   | Place | Outcome on runtime error | Code, HTTP status | Telemetry |
   |---|---|---|---|
   | `Route.spec.match.when` | Request fails; no fallthrough to a lower-ranked Route | RZ-RT-006, 500 (pre-routing response, 5 s write deadline (target)) | span `ruralz.route.match` error; `ruralz_http_node_responses_total{code="RZ-RT-006"}`; `route="_unmatched"` |
   | `Policy.spec.when` | `closed`: the Policy runs (for auth types this feeds SEC rule 1: an erroring `when` never counts as "skipped"); `open`: the Policy is skipped for this request (or leg) | none | `ruralz_filter_failures_total{policy, phase=<first Phase>, mode}`; event on `ruralz.filter.<name>`; access log `failure_modes` (proposed to count; see section 9) |
   | composition `pathExpression`, `when` | Step fails. `optional: true`: left out, response header `ruralz-partial: true`; otherwise siblings cancelled through the shared context and the request fails | RZ-RT-015, 502 | error on the server span; `ruralz_http_node_responses_total{code="RZ-RT-015"}` |
   | `authz.cel config.rule` | Could not decide: deny (closed only) | RZ-AUTH-015, 403; a `false` result is RZ-AUTH-010, 403 | `ruralz_auth_decisions_total{policy, result="deny", code}`; `ruralz_filter_failures_total{mode="closed"}` on error |
   | `ratelimit config.key` | `open` (default): Policy skipped, admitted unmetered, no bucket touched; `closed`: reject | RZ-RL-005, 503 | `ruralz_ratelimit_decisions_total{result="fail_open"}` (open); `ruralz_filter_failures_total` |
   | `quota config.key` | As `ratelimit` (default `open`, OQ-configuration-model-8 (a)); the null-Consumer or missing-quota check (403 RZ-RL-004) runs before the key | RZ-RL-005, 503 | `ruralz_quota_decisions_total{result="fail_open"}` (open); `ruralz_filter_failures_total` |
   | `headers` request `valueExpression` | `closed`: reject (at U scope only that leg ends: a composition step fails, a plain leg returns the code); `open`: Policy skipped in that Phase | RZ-RT-011, 503 | `ruralz_filter_failures_total`; `ruralz.filter.<name>` |
   | `headers` response `valueExpression` | `closed`: response replaced before commit; `open`: skipped | RZ-RT-012, 502 | as above |
   | `cache config.key` | Cache bypassed, whatever `failureMode` | none | `ruralz_cache_requests_total{route, result="bypass"}` |
   | `transform.request` `body`, `set[]` | `closed`: reject; `open`: skipped, body unchanged | RZ-RT-011, 503 | `ruralz_filter_failures_total` |
   | `transform.response` `body`, `set[]` | `closed`: response replaced before commit; `open`: skipped, body unchanged | RZ-RT-012, 502 | `ruralz_filter_failures_total` |
   | `loadBalancing.hashKey` | Weighted random Endpoint for this leg | none | `ruralz_upstream_cel_errors_total{upstream, field="hashKey"}` |
   | `retries.retryOn` | No retry; the leg result follows TM code selection | none | `ruralz_upstream_cel_errors_total{field="retryOn"}` |
   | `circuitBreaker.failureWhen` | The attempt counts as a failure (breaker and passive ejection) | none | `ruralz_upstream_cel_errors_total{field="failureWhen"}` |
   | `accessLog.when` | Entry written | none | none (proposed; no catalog name exists) |
   | M3/M4 rows | Candidate skipped; cache bypassed; 502 | owned by M3/M4 | none in M1 |

   [DP "Precedence", "Failures and partial responses", "Failure semantics", "RZ-RT registry"; SEC "RZ-AUTH decision codes", "Authorization"; TM "Decision path" step 1, "Quotas", "Failure matrix", "Error codes this document owns"; OBS "Ruralz Gateway metrics", "Span model"; FP 8.10]
44. Codes are used exactly as registered in `internal/errcode` (RZ-RT-006, -011, -012, -015; RZ-AUTH-010, -015; RZ-RL-005; RZ-CFG-014, -015). This module returns classified errors; the calling area writes the problem document (`application/problem+json` with `code` and `requestId`) and records telemetry. [DP "Error response format"; FP 8.6]
45. `hashKey` is evaluated once per leg at the first Endpoint selection and reused for retries on the same leg (proposed; its variables cannot change between attempts). [TM "Load balancing"]
46. Default `retryOn` and `failureWhen` (when the fields are absent) are these CEL sources, compiled once per process in their places and shared by every snapshot (proposed; how they enter the canonical form is OQ-traffic-management-and-resilience-6, section 7):
   - `DefaultRetryOn`: `error != null ? (error.kind == "connect" || (error.kind == "reset" && request.method in ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"])) : (response.status == 503 && request.method in ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"])`
   - `DefaultFailureWhen`: `error != null || response.status in [502, 503, 504]`
   [TM "Deadlines" (proposed defaults table)]

### H. Program cache and snapshot

47. Programs compile once per Revision and live in the snapshot ("CEL programs" are part of the compiled snapshot). Within one Revision, identical (environment, source) pairs share one `*Program`. [CM "Limits"; DP "Configuration snapshots and hot reload"]
48. Across Revisions, the Node MUST reuse a `*Program` of the active snapshot for an identical (environment key, source) pair instead of recompiling ("reusing artifacts by digest"); the new `Set` holds only programs its snapshot references, so retired entries are collected with their snapshots (K + 2 snapshots at most). The environment key includes a module-level `EnvVersion` constant bumped whenever declarations or options change. A cache hit returns the same diagnostics a fresh compile would (only valid programs are cached; errors are recomputed). [System overview "Compile before swap" step 2; DP "Activation" step 3]
49. A compile failure on a Node before the swap rejects the Revision with RZ-CFG-014/015 (file mode logs it and keeps serving; Control mode NACKs, M2) and increments `ruralz_config_activations_total{result="rejected", code}`. [DP "Activation"; OBS metrics catalog]

### I. Performance targets

50. Evaluation: typical match and key expressions under 2 µs at p99 (target) [CM "Limits"]; `authz.cel` under 2 µs at p99 (target) [SEC "Authorization"]; the `authz.cel` Filter stage 2 µs p50, 5 µs p99, 0 Ruralz-owned allocations (target) [PB "Per-stage latency budget"]; access log 2 µs or less at p99 per logged request with `when` included, 8 or fewer allocations (target) [OBS "Overhead per signal"]; `headers` setting two values in onUpstreamRequest 2 µs p50, 8 µs p99, 2 allocations (target); `transform.response` on 1 KiB 30 µs p50, 100 µs p99 (target) [PB "Per-stage latency budget"].
51. Compile: Routers, Filter Chains and CEL at 100 µs or less per Route (target), compile stage 400 ms or less for 5,000 Routes (target) [PB "Hot Reload stages"]; 10,000 Routes in 2 s or less on one core [DP "Activation"]; Bundle validation of 10,000 resources under 2 s on a four-core laptop, 20,000 under 4 s (target) [CM "Validation and diff semantics"]. Measured per expression: parse and check 70 to 100 µs, estimate 8 to 12 µs, plan 9 to 12 µs; so reuse (req. 47, 48) is what keeps Hot Reloads in budget.
52. Microbenchmarks MUST include worst-case fixtures at the caps (largest header value, largest body) and report ns/op and allocs/op; the alloc/op gate fails on more than 3% regression (target). [ADR-0011 "Confirmation" (Benchmarks); TQ "Benchmarks and regression gates"]

### J. Telemetry and logging

53. The module records no metric, span or log itself (no import of `internal/telemetry` needed); callers record the names in section G. No new `ruralz_*` name is introduced by this area: the OBS catalog CI check rejects names missing from it. [OBS "Ruralz Gateway metrics" (catalog gate); RL "Code conventions" (Telemetry names)]
54. Process logs about CEL (Node activation failures) use `slog` through `internal/telemetry` with the RZ code, resource identity and place; evaluation errors are logged, if at all, at `debug` with `Kind` only. [RL "Code conventions" (Logging); OBS "Process logs"]

## 3. Proposed Go packages and API

`internal/cel` (Ruralz package name `cel`; it imports cel-go as `celgo "cel.dev/cel-go/cel"`). It imports only the standard library, `cel.dev/cel-go/...`, `internal/errcode` and `pkg/config/v1alpha1` (for `Phase`, `PolicyType`, `CompositionMode`); it never imports `internal/config`, `internal/gateway`, `internal/control` or `internal/cli`, so config (all binaries) and gateway (Node) can both import it. `internal/cel/celtest` holds test helpers (nominal and capped activations, example expressions) usable by other areas' tests. Files: `place.go`, `types.go` (provider, views as CEL values), `env.go`, `compile.go`, `cost.go`, `refs.go`, `program.go`, `vars.go`, `json.go`, `errors.go`, `set.go`.

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package cel compiles, cost-checks and evaluates the CEL expressions of a
// Revision (ADR-0011). It is the only importer of cel.dev/cel-go.
package cel

// ---- place.go -------------------------------------------------------------

type PlaceID string

const (
	PlaceRouteMatchWhen            PlaceID = "Route.spec.match.when"
	PlacePolicyWhen                PlaceID = "Policy.spec.when"
	PlaceStepPathExpression        PlaceID = "Route.spec.composition.steps[].pathExpression"
	PlaceStepWhen                  PlaceID = "Route.spec.composition.steps[].when"
	PlaceAuthzCELRule              PlaceID = "authz.cel config.rule"
	PlaceRateLimitKey              PlaceID = "ratelimit config.key"
	PlaceQuotaKey                  PlaceID = "quota config.key"
	PlaceHeadersRequestValue       PlaceID = "headers config.request.set[].valueExpression"
	PlaceHeadersResponseValue      PlaceID = "headers config.response.set[].valueExpression"
	PlaceCacheKey                  PlaceID = "cache config.key"
	PlaceTransformRequestBody      PlaceID = "transform.request config.body"
	PlaceTransformRequestValue     PlaceID = "transform.request config.set[].valueExpression"
	PlaceTransformResponseBody     PlaceID = "transform.response config.body"
	PlaceTransformResponseValue    PlaceID = "transform.response config.set[].valueExpression"
	PlaceHashKey                   PlaceID = "Upstream.spec.loadBalancing.hashKey"
	PlaceRetryOn                   PlaceID = "Upstream.spec.retries.retryOn"
	PlaceFailureWhen               PlaceID = "Upstream.spec.circuitBreaker.failureWhen"
	PlaceAccessLogWhen             PlaceID = "Gateway.spec.telemetry.accessLog.when"
	PlaceSemanticCacheKey          PlaceID = "ai.semantic-cache config.key"   // validation only until M3
	PlaceCandidateWhen             PlaceID = "AIModel.spec.candidates[].when" // validation only until M3
	PlaceMessagingKey              PlaceID = "Upstream.spec.messaging.key"    // validation only until M4
)

type Result uint8

const (ResultBool Result = iota + 1; ResultString; ResultDyn)

// Var is a bit set of CEL variables.
type Var uint16

const (
	VarRequest Var = 1 << iota
	VarSource
	VarRoute
	VarConsumer
	VarAuth
	VarNow
	VarResponse
	VarError
	VarUpstream
	VarAttempt
	VarSteps
	VarDuration
	VarAI   // M3 runtime
	VarSelf // reserved for x-ruralz-validations (M2)
)

const VarsBase = VarRequest | VarSource | VarRoute | VarConsumer | VarAuth | VarNow

// Names returns the x-ruralz-cel spellings in schema order.
func (v Var) Names() []string

// ErrorRule is a place's normative runtime-error behavior (section G).
type ErrorRule uint8

const (
	RuleInternalError   ErrorRule = iota + 1 // 500 RZ-RT-006, no fallthrough
	RulePolicyWhen                           // closed: run; open: skip
	RuleStepFails                            // optional decides; else 502 RZ-RT-015
	RuleAuthzUndecided                       // 403 RZ-AUTH-015
	RuleFailureMode                          // the Policy's failureMode
	RuleCacheBypass                          // bypass, whatever failureMode
	RuleRandomEndpoint
	RuleNoRetry
	RuleCountFailure
	RuleWriteEntry
	RuleSkipCandidate // M3
	RuleBadGateway    // M4 messaging: 502
)

type Place struct {
	ID            PlaceID
	SchemaPointer string // "$defs/RouteMatch/properties/when"
	Result        Result
	Vars          Var    // union over scopes; equals the x-ruralz-cel variables
	Rule          ErrorRule
	Runtime       string // milestone whose code evaluates it: "M1", "M3", "M4"
}

func Places() []Place                         // constructed table, sorted by ID
func LookupPlace(id PlaceID) (Place, bool)

type Scope uint8

const (ScopeNone Scope = iota; ScopeGateway; ScopeRoute; ScopeUpstream)

// Site is one occurrence of a place; it refines the environment (section B).
type Site struct {
	Place        PlaceID
	Scope        Scope                    // Policy fields and when; else ScopeNone
	PolicyType   v1alpha1.PolicyType      // Policy.spec.when only
	FirstPhase   v1alpha1.Phase           // Policy.spec.when only (req. 11)
	GatesBody    bool                     // Policy.spec.when: the Policy gates the request body here (req. 12)
	Mode         v1alpha1.CompositionMode // composition places
	EarlierSteps []string                 // composition places: earlier step names, list order
}

// Env is the resolved environment of a Site.
type Env struct {
	Vars         Var
	RequestBody  bool
	ResponseBody bool
	Result       Result
}

func (s Site) Env() (Env, error) // error: unknown place or inconsistent site (programming error)

// ---- compile.go -----------------------------------------------------------

type Options struct {
	StaticCostCeiling       uint64        // 10_000 (target)
	RuntimeCostLimit        uint64        // 1_000_000 (target)
	NominalStringSize       uint64        // 256 (target)
	NominalContainerSize    uint64        // 32 (target)
	InterruptCheckFrequency uint          // 100 (proposed)
	EvalDeadlineCap         time.Duration // 50 ms (proposed); 0 = request deadline only
}

func DefaultOptions() Options

// EnvVersion changes whenever declarations, libraries or options change.
const EnvVersion = 1

// Compiler owns the cel-go environments (built lazily per Env, at most one
// per distinct Env) and the default programs. Safe for concurrent use.
type Compiler struct{ /* opts; mu sync.Mutex; envs map[Env]*celgo.Env; defaults */ }

func NewCompiler(opts Options) (*Compiler, error)

type Issue struct {
	Code    string // errcode ID: "RZ-CFG-014" or "RZ-CFG-015"
	Offset  int    // byte offset in the expression; -1 when not positional
	Line    int    // 1-based, 0 when not positional
	Column  int    // 1-based, in bytes
	Message string // deterministic
	Hint    string
}

// Check compiles and cost-checks without keeping a program (CLI path).
func (c *Compiler) Check(site Site, expr string) []Issue

// CheckSpec checks against the schema-level union environment only; it
// implements the 01-config-load validate.CELChecker shape for sites whose
// environment needs no refinement (requirement 15 still needs Check).
func (c *Compiler) CheckSpec(expr string, vars []string, result string) []Issue

// ReferencesRequestBody implements registry.BodyOracle (02-config-revision):
// whether an authz.cel rule selects request.body (req. 13).
func (c *Compiler) ReferencesRequestBody(expr string) (bool, error)

// Default returns the compiled default of PlaceRetryOn or PlaceFailureWhen.
func (c *Compiler) Default(p PlaceID) *Program

const (
	DefaultRetryOn     = `error != null ? (error.kind == "connect" || (error.kind == "reset" && request.method in ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"])) : (response.status == 503 && request.method in ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"])`
	DefaultFailureWhen = `error != null || response.status in [502, 503, 504]`
)

// ---- set.go ---------------------------------------------------------------

// Set is the immutable program set of one snapshot.
type Set struct{ /* progs map[progKey]*Program; progKey{env Env; src string} */ }

func (s *Set) Len() int

// Builder collects the programs of one snapshot; prev (may be nil) is the
// active snapshot's Set, whose programs are reused (req. 48). Safe for
// concurrent Compile calls from the loader's W workers.
type Builder struct{ /* c *Compiler; prev *Set; mu sync.Mutex; progs map[progKey]*Program */ }

func (c *Compiler) NewBuilder(prev *Set) *Builder
func (b *Builder) Compile(site Site, expr string) (*Program, []Issue) // nil Program iff an error Issue
func (b *Builder) Build() *Set                                         // once; the Builder is spent

// ---- program.go -----------------------------------------------------------

type CostRange struct{ Min, Max uint64 }

// Refs are static facts about what a program reads (gate triggers, Phase).
type Refs struct {
	Vars         Var
	RequestBody  bool     // selects request.body
	ResponseBody bool     // selects response.body
	Steps        []string // constant steps keys, sorted, unique
	StepBodies   []string // constant keys whose .body is selected
	AnyStep      bool     // a non-constant steps key: every earlier step counts as read
}

type Program struct{ /* place; env; src; prg celgo.Program; tracked, folds bool; cost CostRange; refs Refs */ }

func (p *Program) Place() PlaceID
func (p *Program) Source() string
func (p *Program) Env() Env
func (p *Program) Cost() CostRange // at nominal sizes
func (p *Program) Tracked() bool   // runtime cost tracking on (req. 34)
func (p *Program) Refs() Refs

// Eval*: ctx supplies the request deadline; it is used only by tracked
// programs with comprehensions (req. 35). v is read, never written.
func (p *Program) EvalBool(ctx context.Context, v *Vars) (bool, error)
func (p *Program) EvalString(ctx context.Context, v *Vars) (string, error)
func (p *Program) EvalValue(ctx context.Context, v *Vars) (Value, error)

// ---- errors.go ------------------------------------------------------------

type ErrorKind uint8

const (
	KindCostLimit ErrorKind = iota + 1
	KindDeadline
	KindNull           // field selection on null
	KindNoSuchKey
	KindNoSuchOverload
	KindConversion     // int("x"), duration("bad") at runtime
	KindArithmetic     // division by zero, overflow
	KindResultType
	KindBody           // body not decodable or not available
	KindOther
)

type EvalError struct {
	Place PlaceID
	Kind  ErrorKind
	// detail: unexported cel-go text (may contain request data)
}

func (e *EvalError) Error() string  // "cel: <place>: <kind>"; never request data
func (e *EvalError) Detail() string // cel-go text; debug and tests only

// ---- vars.go --------------------------------------------------------------

// Vars is the activation of one evaluation point. nil pointer = CEL null.
// The zero Vars is valid (every variable null or empty).
type Vars struct {
	Request  *Request
	Source   *Source
	Route    *Route
	Consumer *Consumer
	Auth     *Auth
	Response *Response
	Error    *AttemptError
	Upstream *Upstream
	Attempt  int
	Steps    *Steps
	AI       *AI // M3
	Now      time.Time
	Duration time.Duration
}

func (v *Vars) Reset() // for pooling

type Param struct{ Name, Value string }

type Request struct {
	Method, Scheme, Host, Path string
	PathParams                 []Param     // template order
	RawQuery                   string      // parsed lazily into query (req. 20)
	Header                     http.Header // canonical keys; read-only while referenced
	Body                       Value       // zero Value: not available (KindBody)
	// query cache: unexported
}

type Source struct {
	IP                netip.Addr
	Port              int
	TLSVersion        uint16 // tls.VersionTLS12 / VersionTLS13; 0 on cleartext
	ClientCertSubject string // RFC 4514
}

// Route and Consumer are built once per snapshot; their CEL values are cached.
type Route struct{ /* name; labels value */ }
func NewRoute(name string, labels map[string]string) *Route

type Consumer struct{ /* name, tier; tags, quotas, labels values */ }
func NewConsumer(name, tier string, tags, quotas []string, labels map[string]string) *Consumer

type Auth struct {
	Method string // "jwt" | "api-key" | "basic" | "mtls"
	Claims Value  // JWT claims; empty map otherwise
}

type Response struct {
	Status int
	Header http.Header
	Body   Value
}

type AttemptError struct{ Kind string } // "connect" | "timeout" | "reset" | "tls"
type Upstream struct{ Name, Endpoint string }

type Step struct {
	Status int
	Header http.Header
	Body   Value
}
type Steps struct{ /* names []string; steps []Step, completed only, list order */ }
func (s *Steps) Add(name string, st Step)
func (s *Steps) Reset()

type AI struct { // M3
	Model                string
	EstimatedInputTokens int64
	MaxOutputTokens      int64
	Stream               bool
}

// ---- json.go --------------------------------------------------------------

// Value is an opaque CEL value (JSON bodies, claims, dyn results).
type Value struct{ /* v ref.Val; zero = absent */ }

func Null() Value
func (v Value) IsNull() bool
func (v Value) IsZero() bool

// DecodeJSON decodes one JSON document into CEL values (req. 25). built is
// the size of the values built, for the maxBufferedBytes reservation; past
// budget it returns ErrTooLarge.
func DecodeJSON(data []byte, budget int64) (v Value, built int64, err error)

var ErrTooLarge = errors.New("cel: decoded value over budget") // sentinel error (allowed global)

// AppendBody writes a transform body result (req. 41): map or list as JSON,
// string or bytes raw, other scalars as JSON text.
func (v Value) AppendBody(dst []byte) ([]byte, error)
```

Internal (unexported) pieces: `provider` (implements cel-go `types.Provider` and `types.Adapter` for `ruralz.*` object types with `FieldType.GetFrom` accessors over the views; nullable objects resolve to `types.NullValue`), `activation` (`type activation Vars` implementing cel-go `interpreter.Activation` by a `switch` on the name, converted from `*Vars` without allocation), `headerMap`, `paramMap`, `queryMap`, `stepsMap`, `orderedObject` (implement `traits.Mapper` with sorted iteration), `nominalEstimator` and `unsizedEstimator` (`checker.CostEstimator`), `refsVisitor` (walks the checked AST for body selections, `steps` keys, time-zone literals), and `bodyValidator`/`tzValidator`/`stepsValidator` as cel-go `ASTValidator`s so their findings arrive as ordinary issues with positions.

**Concurrency model.** `Compiler`: immutable options; environments created lazily under one mutex (fewer than 30 distinct `Env` values exist); `Check`, `Compile` and evaluation are safe from any goroutine. `Builder`: many concurrent `Compile` calls (the loader's W workers), one `Build` call after they finish; a duplicate (Env, source) compiles at most once (per-key `sync.Once` or double-checked map under the mutex). `Set` and `Program`: immutable after `Build`. `Vars`: single-writer; concurrent `Eval*` over one `Vars` is safe only while nobody writes it. The module starts no goroutine; `ContextEval` derives a cancel context from a standard parent.

**Exported for other areas.** `Compiler` (config validation, snapshot build, CLI), `Site`/`Env`/`Place`/`Places` (chain resolver and loader), `Builder`/`Set` (snapshot), `Program` and its `Eval*` and `Refs` (Router, Filter executor, Filters, Upstream layer, composition, access log, body planner), `Vars` and views (request pipeline, auth Filters, Upstream layer), `DecodeJSON`/`Value` (body buffering, JWT claims, Transform Policies), `EvalError`/`ErrorKind` (callers map to codes), `ReferencesRequestBody` (02-config-revision `registry.BodyOracle`), `DefaultRetryOn`/`DefaultFailureWhen`.

## 4. Dependencies on other areas

| Direction | Area (package) | Contract |
|---|---|---|
| Needs | Configuration loading and validation (area 1, `internal/config/validate`, `hub`) | Calls the checker for every `x-ruralz-cel` node, including materialized defaults (`quota` `config.key` = `consumer.name`), after stage I, and maps `Issue.Offset` to file, line and column (01-config-load req. 41). **Gap:** 01's `CELChecker.Check(expr, schemaview.CELSpec)` carries only the schema-level variables and result; requirements 11 to 15 need the `Site` (scope, first Phase, gate flag, composition mode, earlier steps). Proposed: area 1 builds a `cel.Site` per `CELUse` from the effective chains and calls `Compiler.Check(site, expr)`; `CheckSpec` remains for the union environment |
| Needs | Revision and precedence (area 2, `internal/config/registry`, `precedence`) | First Phase per Policy attachment (req. 11), scope per attachment, composition mode and step order; consumes `ReferencesRequestBody` for `authz.cel` Phase selection (R-13a) and for the proposed RZ-CFG-038 (`cache` plus onRequestBody authz); canonical form keeps CEL text byte-exact (no HTML escaping, R-25) |
| Needs | Snapshot build, Hot Reload, Last-Known-Good (lifecycle area) | Holds one `*cel.Set` per snapshot; passes the active `Set` as `prev` to `NewBuilder`; runs compile units on W workers with 100 µs yields; rejects the Revision on any error Issue (RZ-CFG-014/015) and records `ruralz_config_activations_total` |
| Needs | Router (data plane area) | Normalized request view (host, path, captures), `Source`, `now`; evaluates `match.when` per candidate; RZ-RT-006 on error |
| Needs | Filter Chain executor (data plane area) | Evaluates `Policy.spec.when` once before the first Phase; applies failureMode per section G; records `ruralz_filter_failures_total`, spans `ruralz.filter.<name>` |
| Needs | Built-in Filters: `authz.cel` (security area, `internal/filter/authz/cel`), `ratelimit`, `quota`, `cache` (traffic area), `headers`, `transform.*` (data plane area, `internal/filter/header`, `internal/filter/transform/...`) | Each evaluates its programs with `Vars` and maps `EvalError` to its code |
| Needs | Upstream layer and composition (traffic and data plane areas) | Leg `Vars` copies (`request`, `upstream`, `attempt`, `response`, `error`), `hashKey` once per leg, `retryOn`/`failureWhen` per attempt, `steps` population, RZ-RT-015 |
| Needs | Body buffering (data plane area) | Gate decisions from `Refs.RequestBody`, `Refs.ResponseBody`, `Refs.Steps`/`StepBodies`/`AnyStep`; calls `DecodeJSON` with the 4× budget and reserves `built` bytes from `limits.maxBufferedBytes` |
| Needs | Authentication Filters (security area) | Set `Vars.Consumer` (snapshot-built `*cel.Consumer`) and `Vars.Auth` (method, claims via `DecodeJSON` of the verified payload); trusted-proxy `source.ip` |
| Needs | Telemetry (`internal/telemetry`) and access log (observability area) | Callers own metric aggregates and spans; the access log evaluates `accessLog.when` on the request goroutine and copies fields |
| Needs | `internal/errcode` | Code IDs and statuses listed in requirement 44 |
| Needs | State Store | None: no CEL function touches it |
| Provides | Everything listed under "Exported for other areas" in section 3 | |

## 5. Libraries

| Module | Version | License | Linked? | Why |
|---|---|---|---|---|
| `cel.dev/cel-go` | v0.32.0 (pin; catalog floor v0.32.x) | Apache-2.0 | Direct, `internal/cel` only | TS "Library catalog" *Expressions*; typed, non-Turing-complete, `CostLimit`, `ContextEval`, static `EstimateCost`, `ext.Strings`, `ext.Encoders` |
| `cel.dev/expr` | v0.25.1 (from cel-go `go.mod`) | Apache-2.0 | Transitive | Canonical expression protos |
| `github.com/antlr4-go/antlr/v4` | v4.13.1 | BSD-3-Clause | Transitive | CEL parser runtime |
| `google.golang.org/protobuf` | v1.36.10 | BSD-3-Clause | Transitive | CEL value model (`structpb`, `durationpb`, `timestamppb`) |
| `google.golang.org/genproto/googleapis/api`, `.../rpc` | v0.0.0-20240826202546-f6391c0de4c7 | Apache-2.0 | Transitive | `expr/v1alpha1`, `rpc/status` |
| `golang.org/x/text` | v0.22.0 | BSD-3-Clause | Transitive | `ext.Strings` formatting |
| `golang.org/x/exp` | v0.0.0-20240823005443-9b4947da3948 | BSD-3-Clause | Transitive | `constraints`, `slices` |
| `go.yaml.in/yaml/v3` | v3.0.4 | MIT and Apache-2.0 (one file, both texts) | Transitive (cel-go `common/env/io.go`) | Not used by Ruralz; see risks (S6, depgate classification) |
| Standard library | Go 1.26 floor, toolchain go1.27.1 | BSD-3-Clause | | `context`, `net/http` (`Header`), `net/netip`, `net/url` (`ParseQuery`), `encoding/json` (tokenizer for `DecodeJSON` with `UseNumber`), `strconv`, `sync`, `time`, `errors`, `slices` |

Minimal version selection may raise the transitive versions when other M1 modules (OpenTelemetry, jwx, rueidis) require newer ones; nothing here is imported directly except cel-go. No other catalog row is needed. `cel.dev/cel-go` joins the depguard `admitted` list; the `celgo` confinement rule is new (requirement 2).

## 6. Test plan

All tests are hermetic, shuffled and race-enabled (TQ "Unit and property tests"); golden files under `internal/cel/testdata/` with `eol=lf`.

**Unit and table tests (`internal/cel`)**

1. Registry vs schema: walk both embedded views (`api/schema.RenderedV1alpha1()`, `AuthoringV1alpha1()`); every `x-ruralz-cel` maps to exactly one `Place` with equal result and variable set, and every `Place` has one annotation (21 today). Adding a marker without a row fails.
2. Site environments (table): every place × scope × first Phase in requirement 11 → expected `Env` (vars, body flags). Cases: `Policy.spec.when` for `headers` with only response ops at G (response declared), at U with request ops (no response); `transform.response` at R (no `upstream`), at U (`upstream`); `validation.json-schema` `when` (request body); `authz.cel` `when` with body-reading rule (request body).
3. RZ-CFG-014 table, each with expected offset, line, column and golden message: `request.method ==` (syntax); `response.status == 200` in `match.when` (unavailable variable, hint lists `request, source, now`); `request.body.x` in `ratelimit` key; `has(request.body)` in `hashKey`; `response.body` in `headers` response value; `upstream.name` in `transform.request` body at R; `request.bogus` (undefined field); `request.method + 1` (no overload); `response.status` in a string place (result type, hint `string(...)`); `"x"` in a bool place; `now.getHours("Europe/Oslo")`, `now.getHours(request.headers["tz"])` (time zone); `now.getHours("+02:00")` and `"UTC"` accepted; `duration("1x") > duration("1s")`, `timestamp("bad")`, `request.path.matches("(")` (literal validators); `request.?x`, `cel.bind(x, 1, x)`, `math.greatest(1, 2)`, `ip("1.2.3.4")`, `sets.contains([1], [1])`, `json.decode("{}")` (disabled surface); `steps.order.status == 200` in `aggregate` mode; `steps.later` in `sequential` naming a later step; whitespace-only; nesting 251 levels; 100,001 code points; an empty string is absent (no Issue).
4. RZ-CFG-015 table: `request.body.items.all(i, i.tags.all(t, t.tags.all(u, u == "x")))` (measured 169,123); `json.encode(request.body)` (unbounded, hint); a boundary pair built from known unit costs so one expression estimates exactly 10,000 (passes) and one 10,001 (fails); `request.headers.exists(k, request.headers.exists(j, k + j == "x"))`.
5. Golden estimates: every expression of requirement 37 and of `examples/` in its site: no Issue, and its `Cost()` pinned in `testdata/estimates.golden` (a cel-go bump that changes a number shows in review).
6. Evaluation per variable: each field of each view, including nulls (`consumer == null` true when anonymous; `consumer.name` on null gives `KindNull`); headers join (`a: 1`, `a: 2` → `"1, 2"`), lowercase keys, case-insensitive lookup, `size()`, `in`, sorted iteration; `pathParams`; `query` repeated key joined by `","`, percent-decoding; `source.ip` IPv4-mapped IPv6 → IPv4; `tlsVersion` `"1.3"` and `""`; `now` and `duration` from injected values; `steps` presence and absence; `attempt`.
7. JSON: ints stay `int` up to int64 (9007199254740993 round-trips exactly), fractions and exponents are `double`; ordered iteration (`request.body.map(k, k)` over `{"b":1,"a":2}` yields `["a","b"]`); non-JSON content type → `null`; malformed JSON → `KindBody`; duplicate keys last-wins; budget: a document whose built size exceeds the budget returns `ErrTooLarge` and `built` ≤ budget + one value; `AppendBody` for map, list, string, bytes, int, double, bool, null, timestamp, duration, NaN (`KindResultType`).
8. Result types at runtime: `request.body.flag` returning a string in a bool place → `KindResultType`; `EvalString` of `dyn` int → `KindResultType`.
9. Runtime limits: a tracked program over a capped list crosses 1,000,000 → `KindCostLimit`; a comprehension under a 1 ms context → `KindDeadline` within the deadline plus 5 ms; `Tracked()` is false for `consumer == null ? source.ip : consumer.name` and `request.headers["x-tenant"] == "acme"`, true for the `authz-orders` rule.
10. Redaction: `int(request.headers["x-n"])` with value `secret-4242`: `EvalError.Error()` never contains `secret-4242`; `Detail()` may.
11. Defaults: `DefaultRetryOn` over the matrix {GET, HEAD, OPTIONS, PUT, DELETE, POST, PATCH} × {connect, reset, timeout, tls, none} × {200, 502, 503, 504} matches TM "Deadlines"; `DefaultFailureWhen` over {error set} ∪ statuses {200, 429, 500, 502, 503, 504}; both pass RZ-CFG-014/015 in their places.
12. Refs: `request.body` inside comprehensions, ternaries and `has()`; `steps.order.body.id` → `Steps=[order]`, `StepBodies=[order]`; `steps["order"].status` → constant key; `steps[request.headers["s"]]` → `AnyStep`; `ReferencesRequestBody` true and false cases and an uncompilable rule (error).
13. Cache: identical (Env, source) compiled once per `Builder` (count via a test hook); reused from `prev`; a different `Env` with the same text compiles separately; `Build` drops unreferenced programs; 64 goroutines compiling 1,000 overlapping expressions under `-race`.
14. Diagnostics determinism: compiling the negative corpus 100 times in random order yields byte-identical Issue lists.

**Property and fuzz targets (`testing.F`, seeds in `pr-fast`, long runs `nightly`)**

15. `FuzzCompile(expr string, place uint8)`: never panics; returns a `Program` or Issues whose codes are only RZ-CFG-014/015; a `Program` has a finite `Cost().Max` ≤ 10,000. Seeds: requirement 37 expressions and the negative corpus. [TQ "Fuzzing" CEL row]
16. `FuzzCostWithinEstimate(expr, seed)`: for compiled programs, build an activation at nominal sizes from the seed (strings ≤ 256 code points, lists and maps ≤ 32 entries, JSON of depth ≤ 4); evaluate with cost tracking forced on; actual cost ≤ `Cost().Max`. For untracked programs, actual cost ≤ the unsized bound ≤ 1,000,000 on any seeded input.
17. `FuzzCappedInputs(expr, seed)`: inputs at the Gateway caps (`maxRequestHeaderBytes` 64 KiB total headers, a 64 KiB value, JSON bodies to the 4× decoded limit of 10 MiB); evaluation finishes or returns `KindCostLimit`/`KindDeadline` within the deadline cap plus 10 ms; the place's rule applies (asserted through `Place.Rule`).
18. `FuzzDecodeJSON(data)`: never panics; `built` ≤ budget + one value; encode(decode(x)) re-decodes to an equal value.
19. Property: evaluating any compiled program twice on the same `Vars` gives equal results (determinism; map iteration order).

**Benchmarks (`pr-full`, alloc/op gate; latency on RH-1 `nightly`)**

20. `BenchmarkMatchWhen` (`request.headers["x-tenant"] == "acme"`), `BenchmarkRateLimitKey` (`consumer == null ? source.ip : consumer.name`), `BenchmarkAuthzGold`, `BenchmarkAuthzOrders` (`split`/`exists`, tracked), `BenchmarkAccessLogWhen` (example `when`), `BenchmarkHeadersTwoValues`, `BenchmarkTransformResponse1KiB`, `BenchmarkDefaultRetryOn`, `BenchmarkDecodeJSON1KiB`; each reports ns/op and `testing.AllocsPerRun`, with `GOGC=off` and fixed `GOMAXPROCS`; thresholds from requirement 50.
21. Worst-case fixtures at the caps: `split(",")` of a 64 KiB header with `exists` and `map`; a 32 × 32 nested comprehension over a body; assert wall time under the deadline cap (catches the cel-go tracker regression, section 9).
22. `BenchmarkCompile` per expression (parse, check, estimate, plan) and a 5,000-Route Hot Reload with 1% changed expressions (reuse keeps the CEL share under 100 µs per Route).

**Integration and conformance (with other areas; `integration` tag, stage 8)**

23. Configuration conformance negative fixtures (TQ "Conformance suites"): one RZ-CFG-014 and one RZ-CFG-015 fixture with file, line and column, metadata naming stage J and the binaries; single-line plain, double-quoted and block scalars for offset mapping; `ruralz bundle validate` and file-mode `ruralzd` emit identical diagnostics.
24. Golden corpus: the example Bundle validates with no CEL Issue; `ruralz bundle render --effective --route orders-summary` shows `authz-orders` in onRequestHeaders; a variant whose rule reads `request.body` shows onRequestBody and, with a `cache` Policy on the Route, RZ-CFG-038 (proposed code of area 2).
25. Node end-to-end (file mode, `memory` State Store; the local `redis-server` 7.0.15 for the `redis` driver variant of the `ratelimit` cases): `match.when` error (`int(request.headers["x-n"]) > 1` with `x-n: a`) → 500 RZ-RT-006 problem document, no fallthrough to a catch-all `when: "true"` Route; `authz.cel` false → 403 RZ-AUTH-010; `authz.cel` error → 403 RZ-AUTH-015; `ratelimit` key error open → admitted with `ruralz_ratelimit_decisions_total{result="fail_open"}`, closed → 503 RZ-RL-005; `quota` null Consumer → 403 RZ-RL-004 before the key; `headers` closed error → 503 RZ-RT-011, response op → 502 RZ-RT-012, open → header absent; `cache` key error → `result="bypass"`; `transform.response` error closed → 502 RZ-RT-012; composition non-optional step error → 502 RZ-RT-015, optional → `ruralz-partial: true`; `hashKey`, `retryOn`, `failureWhen` errors → `ruralz_upstream_cel_errors_total{field}` and the documented behavior (random Endpoint, no retry, counted failure); `accessLog.when` error → entry written; `Policy.spec.when` error on a closed `auth.jwt` → the Policy runs (401 without a token), on an open `ratelimit` → skipped.
26. Hot Reload: a Revision with an RZ-CFG-014 error is rejected in file mode, the active snapshot keeps serving, `ruralz_config_activations_total{result="rejected",code="RZ-CFG-014"}` increments; a reload changing one Route reuses every other program (test hook count).

## 7. Open questions blocking M1 in this area

| ID | Question (short) | Adopt | What the code does |
|---|---|---|---|
| OQ-traffic-management-and-resilience-6 | Are the Deadlines defaults right, which timeouts are fields ("Yes, canonical form (M1)") | (a) as proposed (with (c) per-protocol Route timeouts, as TM recommends) | The default `retryOn`/`failureWhen` are the CEL sources of requirement 46. Recommended with (a): materialize them as schema defaults (`+ruralz:default=` on `Retries.RetryOn` and `CircuitBreaker.FailureWhen`) so the canonical form carries effective values like `quota` `config.key`; they then flow through the normal site compile. Until area 2 decides, the Node uses `Compiler.Default` for absent fields |
| OQ-traffic-management-and-resilience-21 | How does `cache` coexist with onRequestBody authz, validation or Plugin auth | (a) reject with a new RZ-CFG code (proposed; area 2 names it RZ-CFG-038) | `ReferencesRequestBody` decides whether `authz.cel` runs in onRequestBody (req. 13), which the resolver uses for the rejection |
| OQ-data-plane-9 | Which area covers Node-generated failures outside "before any Upstream" (RZ-RT-011 to -015) | (a) amend pack 8.6 `RT` (current) | CEL runtime errors in `headers`, `transform.*` and composition use RZ-RT-011, -012, -015 as registered |
| OQ-configuration-model-8 (exit criterion 7) | Default `failureMode` of `quota` (open) and `ai.token-budget` (closed) | (a) as registered | A `quota` `config.key` error admits unmetered by default; `closed` gives 503 RZ-RL-005 |
| OQ-security-and-identity-6 (exit criterion 7) | How trusted proxies are declared (`source.ip`) | (c) both, CIDR list and PROXY protocol v2 (answered in CM "Gateway") | `source.ip` is the trusted-proxy-resolved address (req. 22) |
| OQ-security-and-identity-21 | Reject encoded NUL and backslashes | (a) yes (proposed) | `request.path` never contains NUL or backslash; CEL sees the one normalized path |
| OQ-performance-budgets-and-benchmarking-6 | Loader W = max(1, GOMAXPROCS/2) workers yielding every 100 µs | (a) yes (proposed) | Compile units are one expression each (70 to 125 µs measured), so a worker can yield between units; one unit may exceed 100 µs (section 9) |
| OQ-testing-and-quality-strategy-2 | Statistics tool and runner hardware for benchmark gates | (c) alloc/op on shared runners, latency on bare metal (current) | CEL microbenchmarks gate alloc/op in `pr-full`; p99 on RH-1 `nightly` |
| OQ-observability-16 | SDK interface exporting Ruralz aggregates | (a) external producer, consistent with "Ruralz-owned aggregates read by both exporters" (owned by the telemetry area) | No CEL code change: callers increment Ruralz aggregates for the names in section G |

Checked and not touching CEL: the other rows whose Blocking column names M1 (OQ-cli-and-api-surface-10, OQ-data-plane-2, OQ-observability-2, OQ-performance-budgets-and-benchmarking-1 and -2, OQ-release-versioning-and-compatibility-3, OQ-repository-layout-and-conventions-6, OQ-scalability-and-distributed-state-2, -3, -10, -11, OQ-security-and-identity-1, -7, -22, -24, OQ-traffic-management-and-resilience-2, -5, -11, -16, -19, -20, OQ-vision-and-positioning-10, OQ-zero-downtime-upgrades-and-hot-reload-7) and exit-criterion extras OQ-security-and-identity-2, -3, -15, -18 (answered by registration; they fix the Transform Policies CEL rows used here).

## 8. Deferred (do not build in M1) and extension points

| Item | Milestone | Extension point left now |
|---|---|---|
| `x-ruralz-validations[].rule` over `self` (Plugin `configSchema`, CRD `x-kubernetes-validations`) | M2 | `VarSelf` bit reserved; a place row is added with the `self` type derived from the schema; 01-config-load parses but does not evaluate |
| `authz.opa`/`authz.cedar` input = base CEL variables as JSON, `null` where CEL is null | M2 | Views are plain structs so a JSON encoder over `Vars` can be added (`AppendBaseJSON`) without touching programs |
| `plugin` Policies' `when` and first Phase from `Plugin.spec.phases` (including onChunk) | M2 | `Site.FirstPhase` accepts any `v1alpha1.Phase`; requirement 11 gains rows |
| Ruralz Control ingest compile and NACK path | M2 | Same `Compiler`; no code change |
| `ai` variable at runtime, `AIModel.spec.candidates[].when`, `ai.semantic-cache` `config.key`, prompt templates via `transform.request` on `ai` Upstreams | M3 | Places registered and validated now; `Vars.AI` and `RuleSkipCandidate` exist; evaluation wiring waits |
| gRPC, WebSocket, SSE, GraphQL request views and onChunk evaluation | M3 | New view fields need a CM "Variables" change first; do not add fields |
| `Upstream.spec.messaging.key` evaluation (502 on error) | M4 | Place validated now; `RuleBadGateway` |
| XML response bodies to CEL `dyn` (OQ-data-plane-17) | M5 | `DecodeJSON` is one decoder behind `Value`; an XML decoder can produce the same `Value` |
| `source.country` (OQ-security-and-identity-14), CEL /64 function (OQ-traffic-management-and-resilience-18), CEL hash for sticky splits (-17), `limits[].when` (-4) | Not planned or later | Each needs a CM change and an ADR-0011 amendment; the `Var` bit set and place table grow additively |
| Any further cel-go extension, optional types, bindings, HMAC, async functions, Plugin-provided functions | Never without ADR-0011 amendment | Requirement 4 list; review checklist |
| Lua or any other expression language | Never | depguard bans stay |

## 9. Risks and ambiguities

1. **cel-go cost tracking is quadratic in comprehension length (measured, v0.32.0).** The runtime cost observer keeps a value stack that grows per iteration and scans it linearly on each drop (`interpreter/runtimecost.go` `refValStack.drop`/`dropArgs`). `l.exists(p, p == "zz")` over 30,000 elements took 7.6 ms untracked but 2.75 s with `CostLimit`; `ua.split(",").exists(...)` over 100,000 elements took 36.7 s while staying under 1,000,000 units. A 64 KiB header can yield about 32,000 list elements, so one request could burn seconds of CPU. Resolution: requirement 34 skips tracking where it is provably redundant; requirement 35 bounds tracked comprehensions with `ContextEval` under min(request deadline, 50 ms) (proposed cap, which needs a CM "Limits"/ADR-0011 amendment, since ADR-0011 names only "the request deadline"; 50 ms matches cel-go's own 50 ns-per-unit calibration of 1,000,000 units); report the defect upstream and remove the cap once fixed; the worst-case benchmark (test 21) guards regressions.
2. **Allocation and latency targets vs cel-go internals.** Untracked evaluation measured 180 to 200 ns with 2 allocations; tracked 0.76 to 1.1 µs with 9 to 12 allocations; `ContextEval` adds about 0.4 µs and 3 allocations. PB's 0 Ruralz-owned allocations for `authz.cel` is met only if cel-go's internal allocations count as library-owned (as `net/http` internals do in PB) and only for untracked programs. Resolution: requirement 34; ask the PB owner to state that exclusion.
3. **`json.encode` is unusable**: `ext.Encoders` v1 estimates it as unbounded and tracks it at 2^64-1, so it always fails RZ-CFG-015 and would always hit the runtime limit. Resolution: keep Encoders v1 (base64 cost estimators need v1) and give a specific hint (req. 32); transform bodies return maps or lists directly.
4. **Named time zones read the disk** (`time.LoadLocation` on every call), contradicting "no I/O". Resolution: requirement 6.2 (literal UTC offsets or `"UTC"` only).
5. **Map iteration randomness** from Go maps would make `map`/`filter`/`join` and error selection nondeterministic, contradicting "no randomness" and making rate-limit keys unstable. Resolution: sorted iteration everywhere (req. 6.3, 19, 25).
6. **Roadmap CEL row omits the `transform.*` places**, though CM lists them Planned (M1) and RM's Traffic row ships the types. Resolution: in scope (S3).
7. **`authz.cel` runtime error: "Deny" (CM table) vs RZ-AUTH-015 (SEC, ADR-0011 *Failure* row).** Both are 403; resolution: error → RZ-AUTH-015, `false` → RZ-AUTH-010.
8. **`authz.cel` Phase criterion unspecified.** Resolution: body reference decides (req. 13), matching 02-config-revision R-13a.
9. **Body availability in *Base* places is implicit** ("body in body Phases"). Resolution: requirement 12's explicit list; a `request.body` read anywhere else is RZ-CFG-014 rather than a silent buffering trigger.
10. **Schema markers vs scope refinement.** `+ruralz:cel` on `transform.*` lists `upstream` although CM allows it only at Upstream scope, and `Policy.spec.when` lists `response` for every Policy. The annotation is the union; refinement is code (req. 10 to 15). The 01-config-load `CELChecker` interface passes only the annotation, so area 1 must pass a `Site` (section 4).
11. **Which `request` a leg sees, `query` repeated values, `auth.method` spellings, `auth.claims` for non-JWT methods, `tlsVersion` spelling, invalid JSON handling, JSON integer typing, duplicate JSON keys** are unspecified; the proposed answers are in requirements 20 to 25. JSON integers as `int` (not protobuf-Struct doubles) keep 64-bit IDs exact through `transform.*`.
12. **`steps` in aggregate/conditional modes and forward references** are allowed by the schema; requirement 14 tightens them to RZ-CFG-014 (proposed). If rejected, downgrade to a runtime "no such key" error.
13. **Later-milestone places in the golden corpus.** The example Bundle has `AIModel` candidate `when` over `ai` (M3) and an unattached `ai.semantic-cache` Policy; M1 validation must type-check them, so `ai` is declared now (S19). The Plugin `configSchema` `x-ruralz-validations` rule (M2) is not compiled in M1 (01-config-load agrees).
14. **Unattached Policies** have no scope or first Phase; requirement 15 compiles them at their first allowed scope. A Policy attached at two scopes compiles per distinct environment, and an error at one attachment names it.
15. **Compile cost vs 100 µs per Route.** One unique expression costs 90 to 125 µs (measured) before Router and chain work, so a cold compile (Last-Known-Good boot, first load) of a Bundle with one unique expression per Route exceeds the per-Route target; Hot Reloads stay within it through reuse (req. 48). A single compile unit can exceed the 100 µs yield interval. Report to the PB owner.
16. **Diagnostic text stability.** RZ-CFG-014 messages embed cel-go text and are pinned in golden diagnostics; any cel-go upgrade may change bytes. Pin v0.32.0 exactly and treat golden changes as release-noted.
17. **Transitive modules.** cel-go links `go.yaml.in/yaml/v3` (a second YAML implementation beside goccy/go-yaml, soft criterion S6) whose single LICENSE file holds both MIT and Apache-2.0 texts, which depgate may need a reviewed override to classify; also antlr4, protobuf, genproto, `x/text`, `x/exp`. Binary size counts toward the 160 MiB `ruralzd` budget (OQ-tech-stack-and-libraries-22 for Control and CLI).
18. **`Policy.spec.when` errors in `ruralz_filter_failures_total`.** The docs count "each failure"; a `when` error applies `failureMode` but the Policy may still run. Proposed: count it (mode = applied), so failures stay visible (P10). `accessLog.when` errors have no catalog metric; none is added.
19. **Evaluation frequency** of `Policy.spec.when` for upstream-leg Policies (per leg vs per attempt) and of `hashKey` is unspecified; proposed once per leg (req. 40, 45).
20. **`upstream` in upstream-scope `Policy.spec.when` and in `headers` request operations** is not listed by CM although a leg exists there; kept literal (unavailable) to match the schema; a CM change can add it later additively.
21. **Dynamic regex patterns** (`request.path.matches(request.headers["x-re"])`) compile per evaluation; RE2 is linear and the cost model charges pattern length, but a `RegexProgramSizeLimit` is not documented. Left at the cel-go default; revisit with the worst-case benchmarks.
22. **Header lookup allocations.** `textproto.CanonicalMIMEHeaderKey` allocates for uncommon lowercase names; requirement 19 asks for a non-allocating case-insensitive lookup to meet the allocation targets.
