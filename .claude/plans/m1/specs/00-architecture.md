# M1 Core gateway: integration architecture

Integration architecture for milestone M1 "Core gateway" of `github.com/ravindu-rev/ruralz` (branch `develop`, M0 done). Scope and exit criteria come from `docs/roadmap/01-roadmap-and-milestones.md` "M1 Core gateway", which is authoritative. The eleven area specs in this directory (`01-config-load.md` to `11-ops-quality.md`) give the normative requirements; this document fixes how their packages fit together, the shared contracts every work package codes against, the conflicts between specs and how they are resolved, the work packages with their waves, the third-party modules, and the adopted option of every M1-blocking open question.

Where this document and an area spec disagree, this document wins; where it is silent, the area spec applies. Spec references use the form "04 req 34" (spec `04-dataplane-core.md`, requirement 34 of its section 2).

Revision 2 (2026-09-26) answers the architecture review: the request-path and Filter contracts gain per-request Policy state, a post-response hook, leg views, a body and trace handoff, replay, Filter retries, Node-wide components and the missing metric indexes (2.13, 2.14, R-39 to R-45, R-56 to R-59); the layer rule is "acyclic, never upward" and the package table is checked mechanically (1.1, 1.2); OQ-security-and-identity-22 is built in full (R-49); the oversized work packages are split and the unowned items (State Store container matrix, stage 12, T5 verification, Node integration suites, chaos at scale, RH-1, release cut, Compose, scenario coverage, roadmap amendments) get owners (WP-83 to WP-99); section 7 maps every exit criterion to its evidence.

Revision 3 (2026-10-03) records the wave 2 boundary decisions on the engineers' change requests: R-63 to R-75 and the R-49 and R-62 amendments (2.16); RZ-CFG-040 as a `ruralzd`-only serve check with the complete M1 list (section 0 item 8, 3.1); the import and responsibility changes of 1.2; the doc-only and additive contract changes mirrored in section 2 (`tree`, `expr`, `secret`, `hub`, `catalog`, `emit` with `GatewayTimer` and `OriginOf`, `statestore`, `filter`, `snapshot`); the 3.1, 3.3, 3.4, 3.8 and 3.9 wording; and the matching work package scopes in 4.4 and `arch/wps.json`.

## 0. Conventions binding every work package

1. **Toolchain.** `go.mod` keeps `go 1.26.0` and `toolchain go1.27.1`. The floor job builds with Go 1.26 and `GOEXPERIMENT=jsonv2` (jwx needs `encoding/json/v2`). `go vet` (stdversion) rejects `encoding/json/jsontext` and `encoding/json/v2` in module code under `go 1.26.0` (verified: "jsontext.NewDecoder requires go1.27 or later (file is go1.26)"). Production code therefore never imports them: `internal/jsonval` is the strict JSON scanner; `jsontext` appears only in `//go:build go1.27` test oracles.
2. **Code conventions** (docs/engineering/02-repository-layout-and-conventions.md "Code conventions"): the two-line license header `// Copyright 2026 Revington` / `// SPDX-License-Identifier: Apache-2.0`; `context.Context` first; every goroutine has an owner, a bound and a stop path; errors wrap with `%w`; every RZ code literal is registered in `internal/errcode` (repocheck rejects unregistered literals, and `TestMatchesDocs` requires every registered code in its owning document's registry and the reverse); `log/slog` only through loggers handed out by `internal/telemetry`; no `init()`; no mutable package-level variables (sentinel errors and constant tables are fine); no `fmt.Print*` outside `internal/cli`; depguard confinement per section 1.3.
3. **Tests.** Every package ships unit tests (`go test -race`), the fuzz targets its spec names (`testing.F`, seeds from `internal/testkit/corpus` where the spec says golden or negative corpora), and the benchmarks its spec budgets. Integration tests carry `//go:build integration` and run against a local `redis-server` (7.0.15 here, the `process` flavor of `internal/statestore/statestoretest/redisserver`); the `container` flavors (Redis 8, Valkey 9.0.1 or newer, Dragonfly, three-primary Redis and Valkey clusters) run in the CI stage 8 leg `RURALZ_TEST_STATESTORE=container`. Suites that need an assembled `ruralzd` live under `test/` (`integration`, `e2e`, `t5`, `compose` tags). End-to-end, chaos and macro-bench suites carry `//go:build e2e`. No Docker is available in this environment: the process harness (`internal/testkit/topology`) is the end-to-end implementation everywhere, and the Docker Compose end-to-end run with a real OpenTelemetry Collector plus the air-gapped image start run in CI only (R-51).
4. **Clock.** Code that measures or schedules time takes an `internal/clock.Clock`; tests use `clocktest.Fake`. `time.Now` appears only in `clock.Real`. Packages under `internal/testkit/` and `test/` that stand in for external systems or the network may use real time (`time.Now`, timers, tickers); where a test must control time they take a Now or Clock in their Config, as mockidp's `Config.Now` does. Production packages keep the clock rule.
5. **Errors on the wire.** A failure that reaches a client carries an RZ code through `errcode.Wrap`/`errcode.Errorf`, is mapped to a status with `errcode.Status`, and is written by `problem.Write` (RFC 9457, `application/problem+json`, member order `title`, `status`, `code`, `requestId`, `detail`).
6. **Diagnostics.** Every configuration finding is a `diag.Diagnostic`; the text and JSON forms are byte-identical in `ruralz`, `ruralzd` (and `ruralz-control` in M2).
7. **Wave integration.** At each wave boundary the lead merges the wave, runs `make hygiene lint generate build test integration` on the merged tree, and lands any additive contract change requested by the next wave (WP-01 directories, `go.mod`) before it starts.
8. **No M2+ features.** No WASM, Ruralz Control, Control Stream, Console, Kubernetes, OCI, signatures beyond digest checks, gRPC/GraphQL/WebSocket/SSE/HTTP/3 or AI. The extension points left open are named in each work package (typically an interface, a dispatch table keyed by protocol or type, or a reserved enum value). `ruralzd` activation refuses a Revision that uses a feature this release does not serve with RZ-CFG-040 (the serve check, 3.1, run only by a serving Node), while `ruralz bundle validate|render|diff|build` validate such a Revision structurally and never raise RZ-CFG-040. Each finding is reported at the field with "is not served by this release (Planned (Mx))", as `registry.CheckServed` does. The complete M1 list: (1) Policy types whose registry entry has Served false: `plugin`, `authz.opa`, `authz.cedar`, `authz.geoip`, `auth.upstream-sigv4` and `ai.*`; (2) every `Plugin` (M2), `AIProvider` and `AIModel` (M3) resource, referenced or not, reported at `kind`; (3) an Upstream `spec.protocol` other than `http`, and Upstream `spec.discovery.type: kubernetes` (M2); (4) Gateway `listeners[].http3: true` (M3); (5) Route `match.grpc` and `match.graphql` (M3) and `match.topic` (M4). Unreferenced resources fail too: the rule is a static per-resource check, and their `secretRef`s would otherwise still be resolved on the Node. Not RZ-CFG-040: the `kubernetes` and `vault` secret providers (RZ-CFG-026 "Planned (M2)", 01 section 8), `x-ruralz-validations` rules (not evaluated in M1) and unknown fields (RZ-CFG-006, 01 risk 16).

## 1. Package map

### 1.1 Layers

Layers group packages by what they may build on. The rule is **acyclic, never upward**: a package imports only packages of its own layer or a lower one, and only those its row in 1.2 lists; the import graph is acyclic. Same-layer imports are allowed (for example `statestore` uses `telemetry/emit`, `filter` uses `statestore`, `gateway/listener` uses `gateway/retire`). Test support and CI tools sit outside the production layers: production code never imports them (depguard `testsupport` and stage 6 `depgate`), and they may import any production package. The 1.2 table was checked mechanically (every listed import exists, points to the same or a lower layer, and the whole graph, test support included, is acyclic), and the WP-01 sources import nothing outside their rows.

```text
L8  cmd/ruralzd -> gateway            cmd/ruralz -> cli            cmd/ruralz-control -> control (M0 stub)
L7  gateway (Run)   cli (root)   control (M0 stub)
L6  gateway/{reload,lkg,source}   cli/{bundle,launch,dev}
L5  config/pipeline   gateway/{handler,handover,compile}   filter/builtin{,/checks}   cli/node
L4  config/{loader,validate,diff,render}   gateway/{composition,upstream/forward,drain,replay}   cli/adminclient
L3  cel   telemetry (Runtime)   config/{profile,schemaview,overlay,subst,defaults,convert,precedence,canonical}
    statestore/{memory,redis,manager,postcommit}   gateway/{executor,exchange,retire,router,listener,reuseport,
    tap,readiness,admin,upstream}   filter/<type packages>   filter/cache/{revalidate,coalesce}
L2  config/{schemaidx,registry}   identity   filter/auth   secret/resolver   cel/celtypes
    telemetry/{aggregate,tracing,logsink,accesslog}   statestore/{breaker,keys}   nodedir   gateway/setting
    clientaddr   egress   tlsconf   adminauth   redact   signing   gateway/{admission,body,sdnotify}
    gateway/upstream/{balance,discovery,health,resilience}   cli/{command,completion}
L1  core contracts (WP-01): expr   secret   config/{tree,hub}   telemetry/emit   statestore   filter   gateway/snapshot
L0  errcode   buildinfo   clock   phase   problem   adminapi   config/{diag,revision}   telemetry/catalog   ulid
    jsonval   httpfield   sfv   routematch   pkg/config/v1alpha1   api/schema
test clock/clocktest   filter/filtertest   cel/celtest   statestore/statestoretest{,/redisserver,/respproxy}
    gateway/handler/handlertest   testkit/...   test/...
tool tool/{telemetrygen,repocheck,schemagen,benchgate,sizegate,fuzzplan,releasekit,depgate,releasegate,modpin}
```

(Paths are under `internal/` unless they start with `pkg/`, `api/`, `cmd/` or `test/`.)

Mandatory boundaries (lint-enforced, 1.3): `ruralzd` (`internal/gateway/...`) never imports `internal/cli`, `internal/control` or `internal/controlstore`; `internal/cli/...` never imports `internal/gateway/...` or a State Store driver; `internal/config/...`, `internal/expr` and `internal/cel` never import `internal/gateway`, `internal/filter`, `internal/statestore` or `internal/cli`; `internal/filter/...`, `internal/identity` and `internal/statestore/...` never import `internal/gateway` or `internal/cli`; only `ruralzd` resolves secrets (`internal/secret/resolver`).

### 1.2 Packages

"May import" lists Ruralz packages (prefix `internal/` omitted, `v1alpha1` = `pkg/config/v1alpha1`, `api/schema` the embedded schema); the standard library is always allowed. Third-party imports are only those listed, and depguard confines them (1.3). Every package that raises or clears a `ruralz_node_degraded_info` reason (`emit.NodeStatus.SetDegraded(catalog.Reason, ...)`) lists `telemetry/catalog`.

| Layer | Package | Responsibility | May import | Third party | WP |
|---|---|---|---|---|---|
| L0 | `buildinfo` | Version, commit, flavor (M0) | none | none | M0 |
| L0 | `errcode` | RZ registry (M0) plus `Error`, `Wrap`, `Errorf`, `CodeOf`, `Status`; M1 codes | none | none | WP-01 |
| L0 | `clock` | Clock and timer abstraction | none | none | WP-01 |
| L0 | `phase` | Phase, Phase set, Filter class, scope enums | v1alpha1 | none | WP-01 |
| L0 | `problem` | RFC 9457 writer, RZ-RT titles, decoder | errcode | none | WP-01 |
| L0 | `adminapi` | Admin wire types shared by `ruralzd` and `ruralz` (`/readyz`, `/tap`, `/debug/snapshots`) | none | none | WP-01 |
| L0 | `config/diag` | Diagnostic model, key-aware paths, text and JSON writers, collector, nearest-name hints | none | none | WP-01 |
| L0 | `config/revision` | `sha256` digest, `rev-<12 hex>` display | none | none | WP-01 |
| L0 | `telemetry/catalog` | Metric families, bounds, degraded reasons, hops, span names, attribute and log keys | none | none | WP-01 |
| L0 | `ulid` | ULID on `crypto/rand` | none | none | WP-14 |
| L0 | `jsonval` | Strict RFC 8259 scanner (offsets, duplicate names, surrogates, depth, cost), mutable tree, deterministic encoder, media-type test | none | none | WP-02 |
| L0 | `httpfield` | RFC 9110 name and value rules, protected and hop-by-hop sets, query editor | none | none | WP-18 |
| L0 | `sfv` | RFC 9651 list serializer (RateLimit fields) | none | none | WP-18 |
| L0 | `routematch` | Host, path, template, prefix primitives; `CheckTemplate`; match identity `Key` over `Criteria` and the shared converter `CriteriaOf` (R-68); `RequestPath`, `NormalizePath`, `NormalizeHost` | errcode, v1alpha1 | none | WP-05 |
| L0 | `pkg/config/v1alpha1` | Configuration Go types (M0; additive M1 fields) | none | none | WP-28 |
| L0 | `api/schema` | Embedded JSON Schema views (M0; regenerated) | none | none | WP-28 |
| L1 | `expr` | CEL contracts: places, sites, `Compiler`, `Builder`, `Program`, `Value`, activation `Vars` | phase, v1alpha1 | none | WP-01 |
| L1 | `secret` | `Ref`, redacting `Value`, `Kind`, `Use`, `Store`, `Resolver` contracts; Node-wide watches | config/diag, v1alpha1 | none | WP-01 |
| L1 | `config/tree` | Positioned value tree, file table, raw resources | config/diag, v1alpha1 | none | WP-01 |
| L1 | `config/hub` | Typed validated Bundle, effective chains, `Validated`, `SecretUse` destinations, `PolicyCheck`/`Checks` | config/diag, config/tree, config/revision, expr, phase, secret, v1alpha1 | none | WP-01 |
| L1 | `telemetry/emit` | Hot-path metric, trace and access-log handles; State Store label indexes; `GatewayTimer` (moved from `telemetry/aggregate`) and `OriginOf` (09 req 44) for the request path | phase, telemetry/catalog | none | WP-01 |
| L1 | `statestore` | Driver-neutral State Store API, calls, budgets (with stripe), errors, limits, op label mapping | clock, telemetry/emit | none | WP-01 |
| L1 | `filter` | Filter SPI: `Filter`, `Factory`, `Exchange`, `Result`, `Consumptive`, `Finisher`, `Component`, `Replayer`, `Registry` | clock, config/hub, expr, phase, secret, statestore, telemetry/emit, v1alpha1 | none | WP-01 |
| L1 | `gateway/snapshot` | Compiled immutable Revision, Routers, chains, pins; request-path contracts `Forwarder`, `Outbound`, `RequestBody`, `LegHooks`, `LegRun`, `RequestState`, `LegRunner` | config/hub, config/revision, expr, filter, phase, secret, statestore, telemetry/emit, v1alpha1 | none | WP-01 |
| L2 | `config/schemaidx` | Stdlib navigation of the rendered schema: keywords, `x-ruralz-*` annotations, defaults, dispatch, `Lookup`/`Walk`/`Info`; item JSON through `jsonval`'s RFC 8785 number form and key order | api/schema, config/diag, config/tree, jsonval | none | WP-03 |
| L2 | `config/registry` | Policy type table (FP 10): class, Phases, scopes, slots, `failureMode` rules, served flag, typed config constructor | phase, config/diag, errcode, v1alpha1 | none | WP-04 |
| L2 | `identity` | Consumer compile, credential `Index`, `KeyIndex`, static checks RZ-CFG-035/036, `Revoker` hook (M2) | config/hub, config/diag, expr, secret, errcode, v1alpha1 | none | WP-06 |
| L2 | `filter/auth` | Shared auth helpers: challenges, decision recording, Security rule 2 (package root) | filter, identity, expr, errcode, telemetry/emit | none | WP-06 |
| L2 | `secret/resolver` | `env` and `file` providers, `RURALZ_SECRET_ROOT`, per-kind caps, 2 s poll, Node-wide watch table | secret, config/diag, clock, errcode, telemetry/emit, telemetry/catalog | none | WP-07 |
| L2 | `cel/celtypes` | CEL type provider and value adapters for `expr` views and `expr.Value` | expr, phase | cel-go | WP-08 |
| L2 | `telemetry/aggregate` | Ruralz-owned aggregates, admission `Plan`, `Binding`, stripes, exemplars, fold and retire, `sdkmetric.Producer`; implements `emit.Meter` | telemetry/emit, telemetry/catalog, clock, phase, v1alpha1 | otel attribute, otel sdk/instrumentation, otel sdk/metric (with metricdata); tests also use otel exporters/prometheus, prometheus client_golang and otlptranslator for the Prometheus name-set check | WP-09 |
| L2 | `telemetry/tracing` | W3C parse and format, sampling `Decision`, ID generator, span helpers and processor, `Inject`; implements `emit.Tracer` | telemetry/emit, telemetry/catalog, clock | otel, otel/trace, otel/sdk/trace, semconv | WP-10 |
| L2 | `telemetry/logsink` | slog handler (redaction, levels, key checks), bounded stdout worker | telemetry/catalog, secret, clock | none | WP-11 |
| L2 | `telemetry/accesslog` | Pooled records, truncation, JSON encoder; implements `emit.AccessLog` (the handler evaluates `accessLog.when`) | telemetry/emit, telemetry/catalog, clock | none | WP-11 |
| L2 | `statestore/breaker` | Lock-free per-shard breaker and reconnect pacer | clock | none | WP-13 |
| L2 | `statestore/keys` | Key layout `rz:rl:`, `rz:qt:`, `rz:rc:`, `rzplg:` reserved, hash tags, CRC16 slots, entry MAC | statestore | none | WP-13 |
| L2 | `nodedir` | Data dir layout, `flock` (`syscall`, unix; stub elsewhere), `holder.json`, `node.id` | ulid, errcode | none | WP-14 |
| L2 | `gateway/setting` | `RURALZ_*` settings, including `RURALZ_STATE_STORE_MAC_KEY_FILE` | errcode | none | WP-14 |
| L2 | `clientaddr` | PROXY v2 reader and listener wrapper, trusted-proxy resolution, forwarding-header rewrite | none | none | WP-15 |
| L2 | `egress` | SSRF guard (`RURALZ_FETCH_ALLOW`) for Bundle destinations (`jwksUrl`, `tokenUrl`, Upstream Endpoints, DNS results, health probes); guarded dialer and https-only client: JWKS follows at most 3 https-to-https redirects across origins, each hop re-checked; no redirect when `ClientOptions.Destination` is set or `MaxRedirects` is 0 | secret, errcode | none | WP-16 |
| L2 | `tlsconf` | `*tls.Config` builders, cipher list, certificate index | secret, errcode | none | WP-16 |
| L2 | `adminauth` | Admin authentication middleware and settings | secret, problem, errcode | none | WP-17 |
| L2 | `redact` | Credential-header set and slog/`/tap` helpers | secret | none | WP-17 |
| L2 | `signing` | Digest `Verifier` interface, `DigestOnly`, `VerifyArtifact` (RZ-CFG-028) | config/revision, errcode | none | WP-17 |
| L2 | `gateway/admission` | In-flight units, pre-routing limiter, connection ceiling | telemetry/emit, clock, errcode | none | WP-19 |
| L2 | `gateway/body` | Buffer budget, gate reader, tee, limited reader; implements `snapshot.RequestBody` | filter, gateway/snapshot, telemetry/emit, clock, errcode | none | WP-19 |
| L2 | `gateway/upstream/balance` | Four algorithms, schedule and ring builders, budget planner | none | none | WP-22 |
| L2 | `gateway/upstream/discovery` | Static re-resolution, DNS A/AAAA/SRV, `Resolver` | clock, telemetry/emit, telemetry/catalog | none | WP-23 |
| L2 | `gateway/upstream/health` | Passive ejection, active prober (probe function injected) | clock, telemetry/emit, telemetry/catalog | none | WP-23 |
| L2 | `gateway/upstream/resilience` | Retry policy, backoff, retry budget, circuit breaker, bulkhead, error kinds and code selection | clock, expr, errcode | none | WP-24 |
| L2 | `gateway/sdnotify` | systemd notify (`READY=1`, `STOPPING=1`, `MAINPID=`, `STATUS=`) | none | none | WP-67 |
| L2 | `cli/command` | Command specs, interspersed `flag` parsing, IO, exit codes, JSON/NDJSON writers | errcode, problem | none | WP-25 |
| L2 | `cli/completion` | Shell completion generators | cli/command | none | WP-25 |
| L3 | `cel` | Environments per place, check, cost, program cache, evaluation, runtime error rules; implements `expr.Compiler` | expr, phase, cel/celtypes, jsonval, errcode, v1alpha1 | cel-go | WP-34 |
| L3 | `telemetry` | `Runtime`: env scrub, resource, providers, OTLP pipelines, `/metrics` handler, degraded set and cleartext hops (`emit.NodeStatus`), `Apply`, `Shutdown`, logger factory (package root) | telemetry/aggregate, telemetry/tracing, telemetry/logsink, telemetry/accesslog, telemetry/emit, telemetry/catalog, tlsconf, secret, buildinfo, clock | otel (all modules in section 5), otelslog, prometheus, grpc | WP-40 |
| L3 | `config/profile` | Byte pre-checks, token pass, restricted YAML parse and typing, JSON front end, restricted YAML encoder | config/diag, config/tree, jsonval | goccy/go-yaml | WP-33 |
| L3 | `config/schemaview` | Compile the rendered view with the `x-ruralz-*` vocabulary, validate, map errors | config/schemaidx, config/diag, config/tree, api/schema | jsonschema/v6 | WP-35 |
| L3 | `config/overlay`, `config/subst` | Strategic merge and `$patch`; `${VAR}` scan, substitution, re-typing | config/tree, config/diag, config/schemaidx | none | WP-36 |
| L3 | `config/defaults`, `config/convert` | Stage G: defaults, normalization, decode to typed hub; RZ-CFG-005 number range on non-canonical trees (R-64) | config/tree, config/diag, config/schemaidx, config/registry, config/hub, jsonval, v1alpha1 | none | WP-37 |
| L3 | `config/precedence` | Stage I: effective chains (`hub.Chain`), RZ-CFG-018/019/020/029/038, removal reasons, `Rows()` | config/hub, config/registry, config/diag, phase, expr | none | WP-38 |
| L3 | `config/canonical` | Stage L: `ruralz.canonical.v1` encode; strict decode to `tree.Resource`; `/config/dump` document | config/tree, config/hub, config/revision, config/diag, jsonval | none | WP-39 |
| L3 | `statestore/memory` | `memory` driver (GCRA, quota, cache, same semantics as Lua) | statestore, statestore/keys, clock, telemetry/emit, telemetry/catalog | none | WP-13 |
| L3 | `statestore/redis` | `redis` driver: client, dialer (`net.Dialer` with `tlsconf.StateStore`, not the egress guard, 06 req 84), slot map, `ruralz_v1.lua`, URL validation, memory poller | statestore, statestore/keys, statestore/breaker, tlsconf, clock, telemetry/emit, telemetry/catalog | rueidis | WP-41 |
| L3 | `statestore/manager` | Driver lifecycle keyed by role (main, cache) with injected `statestore.Opener`s, MAC key, handles for snapshots | statestore, statestore/postcommit, secret, clock, telemetry/emit, telemetry/catalog | none | WP-65 |
| L3 | `statestore/postcommit` | Post-commit queue and writers | statestore, clock, telemetry/emit | none | WP-65 |
| L3 | `gateway/executor` | Filter Chain executor over `snapshot.RequestState`: Phases, `when`, `failureMode`, short-circuit, consumptive batching, onLog, `Finish`, `LegHooks` | filter, gateway/snapshot, expr, phase, statestore, telemetry/emit, telemetry/catalog, clock, errcode, v1alpha1 | none | WP-20 |
| L3 | `gateway/exchange` | Pooled per-request state implementing `filter.Exchange` (client and leg views), `filter.Message` and `snapshot.RequestState` | filter, gateway/body, gateway/snapshot, httpfield, clientaddr, expr, statestore, telemetry/emit, phase, v1alpha1 | none | WP-43 |
| L3 | `gateway/retire` | Snapshot holder, pin protocol, K = 2 retirement, grace, ending protocol, `Binding` retire and release | gateway/snapshot, adminapi, clock, telemetry/emit, telemetry/catalog, errcode | none | WP-21 |
| L3 | `gateway/router` | Per-listener compiled Router (`snapshot.Router`) | routematch, gateway/snapshot, expr, telemetry/emit | none | WP-42 |
| L3 | `gateway/listener` | Listener set, `http.Server`s, TLS (`GetConfigForClient` over the published snapshot), conn wrapper, `source.ip`, `Binder` | clientaddr, tlsconf, gateway/snapshot, gateway/admission, gateway/retire, gateway/reuseport, secret, telemetry/emit, errcode | none | WP-44 |
| L3 | `gateway/reuseport` | `SO_REUSEPORT` bind without listen, CBPF attach (Linux; stubs elsewhere) | none | x/sys/unix | WP-44 |
| L3 | `gateway/tap` | `/tap` hub, per-subscriber sampling, dropped lines | adminapi, redact, telemetry/emit, problem, errcode | none | WP-45 |
| L3 | `gateway/readiness` | Readiness reasons | adminapi | none | WP-45 |
| L3 | `gateway/admin` | Admin server 9901 and handlers (`/config/dump`, `/metrics`, `/debug/upstreams` injected) | gateway/tap, gateway/readiness, adminauth, adminapi, redact, problem, gateway/snapshot, telemetry/emit, errcode | none | WP-45 |
| L3 | `gateway/upstream` | Upstream manager, runtimes, transports, attempt loop, deadlines, retries (`retryOn`, Filter `Retry`), `hashKey`, outgoing request build (package root) | gateway/upstream/balance, gateway/upstream/discovery, gateway/upstream/health, gateway/upstream/resilience, gateway/snapshot, gateway/body, gateway/admission, filter, expr, egress, tlsconf, httpfield, clientaddr, telemetry/emit, telemetry/catalog, errcode, clock | none | WP-46 |
| L3 | `filter/auth/jwt`, `filter/auth/jwt/jwks` | `auth.jwt`, compact parser, claim checks; Node-wide JWKS manager (`filter.Component`) | filter, filter/auth, identity, jsonval, egress, tlsconf, expr, clock, telemetry/emit, telemetry/catalog, v1alpha1 | jwx/v4 (`jwk`, `jws`, `jwa` only) | WP-47 |
| L3 | `filter/auth/apikey`, `filter/auth/basic` | `auth.api-key` (header only); `auth.basic` and its throttle (`filter.Component`) | filter, filter/auth, identity, secret, clock, telemetry/emit, v1alpha1 | none | WP-48 |
| L3 | `filter/auth/mtls`, `filter/authz/ip`, `filter/cors` | `auth.mtls`; `authz.ip` prefix trie; `cors` | filter, filter/auth, identity, tlsconf, httpfield, secret, telemetry/emit, telemetry/catalog, v1alpha1 | none | WP-49 |
| L3 | `filter/auth/upstreamoauth2` | `auth.upstream-oauth2`, Node-wide token registry (`filter.Component`), destination-bound client secret | filter, egress, tlsconf, jsonval, secret, clock, telemetry/emit, v1alpha1 | none | WP-50 |
| L3 | `filter/validation/jsonschema` | `validation.json-schema`, exported `Check` | filter, jsonval, problem, config/hub, config/diag, v1alpha1 | jsonschema/v6 | WP-51 |
| L3 | `filter/authz/cel`, `filter/header` | `authz.cel`; `headers` Factory, Filter, exported `Check` | filter, filter/auth, expr, httpfield, config/hub, config/diag, v1alpha1 | none | WP-59 |
| L3 | `filter/transform`, `filter/transform/dotpath`, `filter/transform/request`, `filter/transform/response` | Transform engine, dot paths, request and response Factories, exported `Check` | filter, expr, jsonval, httpfield, config/hub, config/diag, v1alpha1 | none | WP-60 |
| L3 | `filter/ratelimit` | `ratelimit`: Node-wide key table (`filter.Component`), GCRA calls, RateLimit fields | filter, statestore, statestore/keys, sfv, httpfield, expr, clock, telemetry/emit, v1alpha1 | none | WP-61 |
| L3 | `filter/quota` | `quota`: reservation, denial cache, refunds from `PolicyState` | filter, statestore, statestore/keys, sfv, httpfield, expr, clock, telemetry/emit, v1alpha1 | none | WP-62 |
| L3 | `filter/cache/revalidate`, `filter/cache/coalesce` | Revalidation workers and lease (`filter.Component`, replays through `filter.Replayer`); miss coalescing | filter, statestore, statestore/keys, clock, telemetry/emit | none | WP-88 |
| L3 | `filter/cache` | `cache` Filter: RFC 9111 lookup and store, hot layer, stale-if-error, invalidation (`Finisher`) (package root) | filter, filter/cache/revalidate, filter/cache/coalesce, statestore, statestore/keys, httpfield, expr, clock, telemetry/emit, v1alpha1 | none | WP-63 |
| L4 | `config/loader` | Stages A to E: sources, discovery, `.ruralzignore`, limits, Environments (RZ-CFG-022), base union, overlays, substitution | config/profile, config/overlay, config/subst, config/schemaidx, config/tree, config/diag | none | WP-55 |
| L4 | `config/validate` | Stage H plus J/K hooks: references, cross-resource rules, RZ-CFG-041 binding, CEL site checks, RZ-CFG-034 over effective chains, injected `hub.Checks`; the `ruralzd`-only serve check (RZ-CFG-040, R-75) as a separate pass | config/hub, config/registry, config/precedence, config/diag, config/tree, routematch, identity, expr, secret, errcode, v1alpha1 | none | WP-56 |
| L4 | `config/diff` | `ruralz bundle diff` model | config/canonical, config/schemaidx, config/hub, config/tree, config/diag | none | WP-57 |
| L4 | `config/render` | `ruralz bundle render` (rendered, effective, conversion) | config/profile, config/canonical, config/convert, config/precedence, config/hub, config/tree, config/schemaidx, config/diag | none | WP-58 |
| L4 | `gateway/composition` | Step executor (three modes), merge, partial responses; composition `Forwarder` | gateway/upstream, gateway/snapshot, gateway/body, gateway/admission, filter, expr, jsonval, errcode, telemetry/emit | none | WP-64 |
| L4 | `gateway/upstream/forward` | Plain-upstreams `Forwarder`, weighted leg pick, response streaming, `LegRunner`, `/debug/upstreams` handler | gateway/upstream, gateway/snapshot, filter, telemetry/emit, errcode | none | WP-86 |
| L4 | `gateway/drain` | SIGTERM timeline, Drain deadline, flush hooks | nodedir, gateway/listener, gateway/reuseport, gateway/retire, gateway/admission, gateway/readiness, gateway/sdnotify, clock, telemetry/emit, errcode | none | WP-67 |
| L4 | `gateway/replay` | `filter.Replayer`: pins the published snapshot, runs one Upstream leg on a synthetic exchange | gateway/retire, gateway/executor, gateway/exchange, gateway/snapshot, filter, errcode | none | WP-66 |
| L4 | `cli/adminclient` | Admin URL rules, TLS, token, problem documents, `/config/dump`, `/tap`, `/readyz` | cli/command, problem, adminapi, config/canonical, tlsconf | none | WP-68 |
| L5 | `config/pipeline` | One entry for stages A to M in every binary; `FromResources` (re-entry at stage F) for Last-Known-Good and handover | config/loader, config/schemaview, config/defaults, config/convert, config/validate, config/precedence, config/canonical, config/revision, config/hub, config/tree, config/diag, expr, secret | none | WP-69 |
| L5 | `gateway/handler` | `http.Handler`: admission, pin, normalization, match, dispatch by `Route.Protocol`, commit, onLog, `accessLog.when` | gateway/router, gateway/exchange, gateway/executor, gateway/retire, gateway/admission, gateway/body, gateway/snapshot, gateway/tap, routematch, clientaddr, sfv, problem, expr, telemetry/emit, errcode | none | WP-66 |
| L5 | `gateway/handover` | Handover protocol `ruralz.handover.v1`, usage reports, steering | nodedir, gateway/drain, gateway/listener, gateway/reuseport, gateway/retire, gateway/admission, gateway/readiness, gateway/sdnotify, clock, telemetry/emit, errcode | x/sys/unix | WP-89 |
| L5 | `gateway/compile` | `hub.Validated` to `*snapshot.Snapshot`, carry-over, bounded workers | gateway/snapshot, gateway/router, gateway/upstream, gateway/upstream/forward, gateway/composition, config/hub, filter, identity, expr, secret, statestore, telemetry/emit, telemetry/catalog, tlsconf, clock, phase, v1alpha1 | none | WP-70 |
| L5 | `filter/builtin` | `builtin.New(Deps)`: `*filter.Registry` of every served type with its Node-wide Components (`ruralzd` only) | filter, filter/auth/jwt, filter/auth/jwt/jwks, filter/auth/apikey, filter/auth/basic, filter/auth/mtls, filter/authz/ip, filter/cors, filter/auth/upstreamoauth2, filter/validation/jsonschema, filter/authz/cel, filter/header, filter/transform/request, filter/transform/response, filter/ratelimit, filter/quota, filter/cache, filter/cache/revalidate, egress, tlsconf, statestore, clock, telemetry/emit | none | WP-71 |
| L5 | `filter/builtin/checks` | `checks.Table()`: offline `hub.Checks` for every binary (no JOSE, no State Store code) | filter/header, filter/transform/request, filter/transform/response, filter/validation/jsonschema, config/hub, config/diag, v1alpha1 | none | WP-71 |
| L5 | `cli/node` | `node drain` (Linux), `node dump` | cli/command, cli/adminclient, nodedir | none | WP-72 |
| L6 | `gateway/reload`, `gateway/lkg`, `gateway/source` | Activation pipeline, pending slot, LKG store and boot order, file-mode watcher | config/pipeline, config/canonical, config/revision, config/hub, gateway/compile, gateway/retire, gateway/snapshot, gateway/listener, gateway/readiness, nodedir, secret, secret/resolver, signing, telemetry/emit, telemetry/catalog, clock, errcode | none | WP-73 |
| L6 | `cli/bundle` | `validate`, `render`, `diff`, `build` | cli/command, cli/adminclient, config/pipeline, config/render, config/diff, config/canonical, config/hub, config/diag, filter/builtin/checks, cel | none | WP-74 |
| L6 | `cli/launch`, `cli/dev` | Local `ruralzd` launcher; `dev run`, `dev tap` | cli/command, cli/adminclient, config/pipeline, config/render, filter/builtin/checks, cel, nodedir, buildinfo | none | WP-77 |
| L7 | `gateway` | `Run`: supervisor wiring everything for `ruralzd` (package root) | gateway/setting, gateway/admin, gateway/tap, gateway/readiness, gateway/listener, gateway/reuseport, gateway/handler, gateway/replay, gateway/retire, gateway/admission, gateway/body, gateway/drain, gateway/handover, gateway/sdnotify, gateway/upstream, gateway/upstream/forward, gateway/reload, gateway/lkg, gateway/source, gateway/snapshot, telemetry, telemetry/emit, telemetry/catalog, statestore, statestore/manager, statestore/memory, statestore/redis, statestore/postcommit, secret/resolver, filter, filter/builtin, cel, config/pipeline, config/canonical, adminauth, egress, tlsconf, nodedir, ulid, clock, buildinfo, errcode | none | WP-76 |
| L7 | `cli` | Root, help, `version`, `completion` wiring (package root); supplies `clock.Real().Now` to `command.NewIO` (`cli/command` stays clock-free) | cli/command, cli/completion, cli/bundle, cli/dev, cli/launch, cli/node, buildinfo, clock | none | WP-77 |
| L7 | `control` | `ruralz-control` stub (M0; built in M2, untouched in M1) | buildinfo | none | M0 |
| L8 | `cmd/ruralzd` | Binary entry: wiring only | gateway | none | WP-76 |
| L8 | `cmd/ruralz-control` | Binary entry (M0 stub) | control | none | M0 |
| L8 | `cmd/ruralz` | Binary entry: wiring only | cli | none | WP-77 |
| test | `clock/clocktest` | Manual fake clock | clock | none | WP-01 |
| test | `filter/filtertest` | In-memory `Exchange`, `Message`, `Store` and `Replayer` fakes | expr, filter, statestore, telemetry/emit | none | WP-01 |
| test | `cel/celtest` | Real-CEL helpers for Filter tests | cel, expr | none | WP-34 |
| test | `statestore/statestoretest` | Failure-injecting fake `Store`, driver conformance suite | statestore, clock | none | WP-13 |
| test | `statestore/statestoretest/redisserver`, `statestore/statestoretest/respproxy` | Local `redis-server` launcher (standalone, replica, TLS, ACL, cluster, failover) and container flavors; RESP counting and fault proxy | testkit/faultproxy, testkit/pki, testkit/proc | testcontainers-go (container flavor) | WP-85 |
| test | `gateway/handler/handlertest` | AllocsPerRun fixtures for the bench suite | gateway/handler, gateway/snapshot, filter/filtertest | none | WP-66 |
| test | `testkit/binaries`, `testkit/proc`, `testkit/pki`, `testkit/faultproxy`, `testkit/canary`, `testkit/promtext`, `testkit/benchrecord`, `testkit/corpus` | Test kit base | none | x/sys/unix (`proc`) | WP-26 |
| test | `testkit/mockup`, `testkit/mockidp`, `testkit/otlpsink` | Mock Upstream, mock IdP, in-process OTLP/gRPC collector (self-contained) | none | otlp proto, grpc (`otlpsink`); in `otlpsink` tests only, the OTel SDK (`otel`, `otel/sdk`, `sdk/metric`, `otel/log`, `sdk/log`) and the otlptracegrpc, otlpmetricgrpc and otlploggrpc exporters (client interop test) | WP-84 |
| test | `testkit/l4lb`, `testkit/loadgen` | L4 balancer with `/readyz` probes; open-loop load generator | none | none | WP-52 |
| test | `testkit/topology` | T1, T2, T5 and chaos topologies | testkit/binaries, testkit/proc, testkit/pki, testkit/faultproxy, testkit/canary, testkit/promtext, testkit/mockup, testkit/mockidp, testkit/otlpsink, testkit/l4lb, testkit/loadgen, statestore/statestoretest/redisserver, statestore/statestoretest/respproxy | none | WP-87 |
| tool | `tool/telemetrygen` | Alert rules and dashboards generator (stage 3 drift test) | telemetry/catalog | none | WP-12 |
| tool | `tool/repocheck` | Stage 1 checks (extended with catalog names) | telemetry/catalog, errcode | none | WP-12 |
| tool | `tool/schemagen` | Schema generator (M0) | none | none | WP-28 |
| tool | `tool/benchgate`, `tool/sizegate`, `tool/fuzzplan` | Stage 9 and 11 gates | none | none | WP-27 |
| tool | `tool/releasekit`, `tool/depgate` | Stage 12 packaging and notices; stage 6 gate (extended) | none | none | WP-54 |
| tool | `tool/releasegate` | Stage 12 release gate checks | none | none | WP-96 |
| tool | `tool/modpin` | `//go:build tools` module pins (lead-owned, removed at the end of M1) | none | all section 5 direct modules | lead |

Injected rather than imported (to keep the graph acyclic and each work package independent): the admin server receives its `/metrics` handler (from `telemetry`), `/config/dump` writer (from `config/canonical` over the published snapshot) and `/debug/upstreams` handler (from `gateway/upstream/forward`) from the wiring; `statestore/manager` receives one `statestore.Opener` per `DriverKind` (`memory.Open`, `redis.Open`); `config/pipeline` receives `hub.Checks` and the `expr.Compiler`; `config/precedence` receives the `authz.cel` body oracle (`Compiler.ReferencesRequestBody`); `filter/builtin` receives its `Deps` (clock, logger, `emit.NodeStatus`, guarded HTTP client, `filter.Replayer`).

Extension points left for M2+: `snapshot.Route.Protocol` keys the handler's dispatch table (M1 serves `http`; M3 and M4 add handlers for `grpc`, `graphql`, `websocket`, `kafka`, `nats`, `mqtt`, `ai`), and `snapshot.Forwarder` is per Route; `phase.OnChunk` exists and the executor accepts `onChunk` subscribers; `filter.Registry` and `builtin` tables admit new types (the `plugin` type, `authz.opa`, `authz.cedar`, `ai.*` return RZ-CFG-040 on a Node until served, R-75) and new `filter.Component`s; `balance.Algorithm` and the Picker build and pick dispatch take locality-aware balancing and slow start (OQ-traffic-management-and-resilience-8): a new algorithm adds an enum value, a builder and a dispatch case; `balance.Picker` is a concrete immutable struct, not an interface (this supersedes the "`balance.Picker` interface" entry in 05 section 8's deferred table); `expr.Places()` lists M3/M4 places marked with their milestone; `signing.Verifier` takes the Sigstore verifier in M2 and `signing.VerifyArtifact` gets its Plugin artifact fetch in M2; `identity.Revoker` is the M2 revocation hook; `statestore.Role` has room for further roles; `hub.SecretUse.Destination` takes further transmitting fields (the `AIProvider` `apiKey` destination is assigned from M1 and transmitted from M3, R-49); `telemetry.Runtime` resource attributes add Cluster and Environment in Control mode; `retire` keeps the stream-ending hook for M3 streams; `hub` keeps the conversion registry shape for a second apiVersion.

### 1.3 Import rules added to `.golangci.yml` (WP-01)

WP-01 replaces the depguard block with the following (verified with golangci-lint v2.13.2: `golangci-lint config verify` passes, `golangci-lint run ./...` reports 0 issues over the wave 1 packages, and a probe import of `internal/clock` into `telemetry/emit` is rejected by the `contracts` rule, so the emit package's "no OpenTelemetry" contract is lint-enforced even though the `otel` rule excludes `internal/telemetry/**`). `cel.dev/expr` is admitted because cel-go's public API returns its types; the `celgo` rule still confines it to `internal/cel/**`. The `go/` allow entry works around depguard dropping `go/...` from `$gostd` once an allow entry starts with `go.` (verified on `internal/tool/schemagen`).

```yaml
    depguard:
      rules:
        admitted:
          list-mode: strict
          files: ["$all"]
          allow:
            - $gostd
            # depguard drops go/... from $gostd once a go.opentelemetry.io
            # entry shares the "go" prefix; list the standard go/ tree.
            - go/
            - github.com/ravindu-rev/ruralz
            # M1 modules; each has a Tech stack catalog row
            # (docs/engineering/01-tech-stack-and-libraries.md).
            - cel.dev/cel-go
            - cel.dev/expr
            - github.com/goccy/go-yaml
            - github.com/lestrrat-go/jwx/v4
            - github.com/prometheus/client_golang
            - github.com/prometheus/common
            - github.com/prometheus/otlptranslator
            - github.com/redis/rueidis
            - github.com/santhosh-tekuri/jsonschema/v6
            - github.com/testcontainers/testcontainers-go
            - go.opentelemetry.io/contrib/bridges/otelslog
            - go.opentelemetry.io/otel
            - go.opentelemetry.io/proto/otlp
            - golang.org/x/net/http2
            - golang.org/x/sys/unix
            - google.golang.org/grpc
        # banned, wazero, rueidis: unchanged from M0.
        goyaml:
          files: ["$all", "!**/internal/config/profile/**"]
          deny:
            - pkg: github.com/goccy/go-yaml
              desc: the restricted YAML profile is the only YAML parser and emitter
        jsonschema:
          files: ["$all", "!**/internal/config/schemaview/**", "!**/internal/filter/validation/jsonschema/**"]
          deny:
            - pkg: github.com/santhosh-tekuri/jsonschema
              desc: wrapped by internal/config/schemaview and validation.json-schema
        celgo:
          files: ["$all", "!**/internal/cel/**"]
          deny:
            - pkg: cel.dev/cel-go
              desc: CEL is reached through internal/expr contracts implemented by internal/cel
            - pkg: cel.dev/expr
              desc: CEL is reached through internal/expr contracts implemented by internal/cel
        jwx:
          files: ["$all", "!**/internal/filter/auth/jwt/**"]
          deny:
            - pkg: github.com/lestrrat-go/jwx
              desc: JOSE is confined to internal/filter/auth/jwt
        jwxpackages:
          files: ["$all"]
          deny:
            - pkg: github.com/lestrrat-go/jwx/v4/jwt
              desc: links jwe and golang.org/x/crypto (G3); decode claims with internal/jsonval
            - pkg: github.com/lestrrat-go/jwx/v4/jwe
              desc: no JWE in ruralzd; links golang.org/x/crypto (G3)
            - pkg: github.com/lestrrat-go/jwx/v4$
              desc: import only jwk, jws and jwa
        otel:
          files: ["$all", "!**/internal/telemetry/**", "!**/internal/testkit/otlpsink/**"]
          deny:
            - pkg: go.opentelemetry.io
              desc: OpenTelemetry is confined to internal/telemetry (ADR-0010)
            - pkg: github.com/prometheus
              desc: /metrics exposition is internal/telemetry's
            - pkg: google.golang.org/grpc
              desc: gRPC links only for OTLP export in internal/telemetry until M3
        xsys:
          files: ["$all", "!**/internal/gateway/reuseport/**", "!**/internal/gateway/handover/**", "!**/internal/testkit/proc/**"]
          deny:
            - pkg: golang.org/x/sys
              desc: socket steering, peer credentials and test namespaces only
        xnet:
          files: ["$all", "!**/test/conformance/protocol/**"]
          deny:
            - pkg: golang.org/x/net
              desc: x/net/http2 Framer is for the protocol conformance suite only
        testcontainers:
          files: ["$all", "!$test", "!**/internal/statestore/statestoretest/redisserver/**"]
          deny:
            - pkg: github.com/testcontainers
              desc: test tooling, never linked into a binary
        testsupport:
          files: ["$all", "!$test", "!**/internal/testkit/**", "!**/test/**", "!**/internal/statestore/statestoretest/**", "!**/internal/filter/filtertest/**", "!**/internal/clock/clocktest/**", "!**/internal/cel/celtest/**", "!**/internal/gateway/handler/handlertest/**"]
          deny:
            - pkg: github.com/ravindu-rev/ruralz/internal/testkit
              desc: test support is imported by tests only
            - pkg: github.com/ravindu-rev/ruralz/internal/statestore/statestoretest
              desc: test support is imported by tests only
            - pkg: github.com/ravindu-rev/ruralz/internal/filter/filtertest
              desc: test support is imported by tests only
            - pkg: github.com/ravindu-rev/ruralz/internal/clock/clocktest
              desc: test support is imported by tests only
            - pkg: github.com/ravindu-rev/ruralz/internal/cel/celtest
              desc: test support is imported by tests only
            - pkg: github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest
              desc: test support is imported by tests only
        secretresolver:
          files: ["**/internal/cli/**", "**/internal/config/**", "**/internal/control/**", "**/internal/filter/**"]
          deny:
            - pkg: github.com/ravindu-rev/ruralz/internal/secret/resolver
              desc: only ruralzd resolves secrets; others read secret.Store
        config:
          files: ["**/internal/config/**", "**/internal/expr/**", "**/internal/cel/**"]
          deny:
            - pkg: github.com/ravindu-rev/ruralz/internal/gateway
              desc: the configuration pipeline runs in every binary
            - pkg: github.com/ravindu-rev/ruralz/internal/filter
              desc: filters depend on configuration, not the reverse; checks are injected
            - pkg: github.com/ravindu-rev/ruralz/internal/statestore
              desc: the configuration pipeline never talks to a State Store
            - pkg: github.com/ravindu-rev/ruralz/internal/cli
              desc: the configuration pipeline is shared by all binaries
        filter:
          files: ["**/internal/filter/**", "**/internal/identity/**", "**/internal/statestore/**"]
          deny:
            - pkg: github.com/ravindu-rev/ruralz/internal/gateway
              desc: filters and stores are data-plane neutral; the gateway implements their contracts
            - pkg: github.com/ravindu-rev/ruralz/internal/cli
              desc: the CLI is never linked into filters
        contracts:
          list-mode: strict
          files: ["**/internal/phase/**", "**/internal/expr/**", "**/internal/clock/*.go", "**/internal/problem/**", "**/internal/adminapi/**", "**/internal/telemetry/catalog/**", "**/internal/telemetry/emit/**", "**/internal/config/diag/**", "**/internal/config/revision/**", "!$test"]
          allow:
            - $gostd
            - github.com/ravindu-rev/ruralz/pkg/config/v1alpha1
            - github.com/ravindu-rev/ruralz/internal/phase
            - github.com/ravindu-rev/ruralz/internal/errcode
            - github.com/ravindu-rev/ruralz/internal/telemetry/catalog
        # raft, gateway, control: unchanged from M0.
        cli:
          files: ["**/internal/cli/**"]
          deny:
            - pkg: github.com/ravindu-rev/ruralz/internal/gateway
              desc: ruralz dev run launches a local ruralzd instead
            - pkg: github.com/ravindu-rev/ruralz/internal/statestore/
              desc: the CLI never opens a State Store driver
        # public, tools: unchanged from M0.
```

Also added under `linters.settings`:

```yaml
    sloglint:
      no-global: all
      context: scope
      static-msg: true
      no-raw-keys: false
      key-naming-case: snake
      forbidden-keys: [time, level, msg, source]
```

Stage 6 (`depgate`, WP-54) adds `internal/testkit`, `internal/tool` and every `*test` support package to the per-binary denylist checked over `go list -deps ./cmd/...`.

## 2. Shared core types and interfaces (WP-01)

The blocks below are the complete wave 1 sources. They were compiled in a scratch copy of the repository with the `.golangci.yml` of section 1.3: `go build ./...`, `go vet ./...`, `golangci-lint run ./...` (0 issues, golangci-lint v2.13.2), `golangci-lint fmt --diff`, repocheck and the `internal/errcode` tests (with the registry rows of 2.3 added to the four owning documents) all pass, and scratch tests of the revised behavior (zero-duration fake timers, empty secret values, set-item paths, State Store label indexes against `catalog.Ops()`) pass under `-race`. WP-01 commits them verbatim (extracted mechanically from this document), adds unit tests (listed per package) and nothing else; its session effort is the tests, the lint block and the document rows (about 2,500 lines), not the 5,600 lines of pasted sources.

Rules for every later work package:

1. The contracts are frozen after wave 1. A work package that needs a change reports it to the lead; the lead lands additive changes between waves. No work package edits a WP-01 directory.
2. Implementations live in the packages named in section 1.2 (for example `internal/cel` implements `expr.Compiler`, `internal/telemetry/aggregate` implements `emit.Meter`, `internal/gateway/exchange` implements `filter.Exchange`, `internal/secret/resolver` implements `secret.Resolver`, `internal/statestore/memory` and `.../redis` implement `statestore.Store`).
3. Types that cross a package boundary on the request path are plain structs or small interfaces; nothing here allocates per request by itself (pins, stripes and records are pooled by their owners).

### 2.1 Clock

`internal/clock/clock.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package clock abstracts time for every timer-driven component (snapshot
// retirement, Drain timelines, JWKS refresh, breakers, discovery, token
// buckets). Components take a Clock in their constructor and never call
// time.Now or time.AfterFunc directly, so tests drive them with
// clocktest.Fake.
package clock

import (
	"context"
	"time"
)

// Clock is the time source. Implementations are safe for concurrent use.
type Clock interface {
	// Now returns the current time, with a monotonic reading when real.
	Now() time.Time
	// Since returns the time elapsed since t.
	Since(t time.Time) time.Duration
	// NewTimer returns a Timer that fires once after d.
	NewTimer(d time.Duration) Timer
	// AfterFunc calls f in its own goroutine after d, like time.AfterFunc.
	// f must not block for long; the owner of f cancels it with Stop.
	AfterFunc(d time.Duration, f func()) Timer
	// Sleep blocks for d or until ctx is done, returning ctx.Err() then.
	Sleep(ctx context.Context, d time.Duration) error
}

// Timer is a stoppable one-shot timer.
type Timer interface {
	// C returns the channel that receives the fire time; nil for AfterFunc timers.
	C() <-chan time.Time
	// Stop prevents the timer from firing; it reports whether it was active.
	Stop() bool
	// Reset changes the timer to fire after d; it reports whether it was active.
	Reset(d time.Duration) bool
}

// Real returns the wall clock backed by package time.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time                  { return time.Now() }
func (realClock) Since(t time.Time) time.Duration { return time.Since(t) }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{t: time.NewTimer(d)} }

func (realClock) AfterFunc(d time.Duration, f func()) Timer {
	return realTimer{t: time.AfterFunc(d, f)}
}

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }
```

`internal/clock/clocktest/fake.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package clocktest provides a manually advanced clock.Clock for tests.
package clocktest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Fake is a clock.Clock whose time moves only through Advance and Set.
// Timers with a positive duration fire synchronously inside Advance, in
// deadline order, and their AfterFunc callbacks run on the goroutine that
// calls Advance. Like clock.Real, a zero or negative duration fires at
// once: NewTimer's channel is already filled, AfterFunc runs its callback
// in a new goroutine, and Sleep returns without waiting.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

// New returns a Fake set to start.
func New(start time.Time) *Fake { return &Fake{now: start} }

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Since returns the fake time elapsed since t.
func (f *Fake) Since(t time.Time) time.Duration { return f.Now().Sub(t) }

// NewTimer returns a timer whose channel receives on Advance, or at once
// when d <= 0.
func (f *Fake) NewTimer(d time.Duration) clock.Timer {
	return f.add(&fakeTimer{f: f, ch: make(chan time.Time, 1)}, d)
}

// AfterFunc returns a timer that calls fn on Advance, or at once in a new
// goroutine when d <= 0.
func (f *Fake) AfterFunc(d time.Duration, fn func()) clock.Timer {
	return f.add(&fakeTimer{f: f, fn: fn}, d)
}

// Sleep blocks until Advance passes d or ctx is done; d <= 0 returns at
// once (ctx.Err() when ctx is already done).
func (f *Fake) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := f.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C():
		return nil
	}
}

// Advance moves time forward by d and fires every timer due by then.
func (f *Fake) Advance(d time.Duration) { f.Set(f.Now().Add(d)) }

// Set moves time to t (never backwards) and fires every timer due by then.
func (f *Fake) Set(t time.Time) {
	for {
		f.mu.Lock()
		if t.Before(f.now) {
			t = f.now
		}
		slices.SortStableFunc(f.timers, func(a, b *fakeTimer) int { return a.at.Compare(b.at) })
		if len(f.timers) == 0 || f.timers[0].at.After(t) {
			f.now = t
			f.mu.Unlock()
			return
		}
		ft := f.timers[0]
		f.timers = f.timers[1:]
		f.now = ft.at
		f.mu.Unlock()
		ft.fire(false)
	}
}

// Pending returns the number of armed timers.
func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// add arms ft to fire after d; d <= 0 fires it at once without arming.
func (f *Fake) add(ft *fakeTimer, d time.Duration) *fakeTimer {
	f.mu.Lock()
	ft.at = f.now.Add(max(d, 0))
	if d > 0 {
		f.timers = append(f.timers, ft)
	}
	f.mu.Unlock()
	if d <= 0 {
		ft.fire(true)
	}
	return ft
}

func (f *Fake) remove(ft *fakeTimer) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.Index(f.timers, ft)
	if i < 0 {
		return false
	}
	f.timers = slices.Delete(f.timers, i, i+1)
	return true
}

type fakeTimer struct {
	f  *Fake
	at time.Time
	ch chan time.Time
	fn func()
}

// fire delivers the timer: a channel send that never blocks, or the
// callback (in a new goroutine when async, as time.AfterFunc does).
func (t *fakeTimer) fire(async bool) {
	switch {
	case t.fn != nil && async:
		go t.fn()
	case t.fn != nil:
		t.fn()
	default:
		select {
		case t.ch <- t.at:
		default:
		}
	}
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop() bool          { return t.f.remove(t) }

func (t *fakeTimer) Reset(d time.Duration) bool {
	active := t.f.remove(t)
	t.f.add(t, d)
	return active
}
```

Tests (WP-01): `Real` basics; `Fake` ordering of timers with equal deadlines, `Stop`/`Reset` return values, `AfterFunc` firing in `Advance`, `Sleep` cancellation; zero and negative durations fire at once like `Real` (`NewTimer(0)` channel ready, `AfterFunc(0, f)` runs `f` on a new goroutine, `Sleep(ctx, 0)` returns, `Reset(0)` fires), so full-jitter backoff that draws 0 never hangs a test.

### 2.2 Phases, Filter classes and scopes

A leaf package so that `expr`, `config/hub`, `filter` and `gateway/snapshot` share the enums without an import cycle. `phase.Phase` is the compact runtime form of `v1alpha1.Phase`; `Class` orders a Phase (FP 8.12).

`internal/phase/phase.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package phase is the Filter Chain vocabulary shared by the configuration
// pipeline, the CEL module, the Filter SPI and the data plane: the fixed
// Phases (foundation pack section 4), the Filter classes and their order,
// and the attachment scopes (section 8.12). Values are small integers so
// chains index arrays by Phase; String returns the v1alpha1 spelling.
package phase

import "github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"

// Phase is one Filter Chain Phase, in execution order.
type Phase uint8

// Phases. Count is the number of Phases, for arrays indexed by Phase.
const (
	OnRequestHeaders Phase = iota
	OnRequestBody
	OnRoute
	OnUpstreamRequest
	OnUpstreamResponseHeaders
	OnUpstreamResponseBody
	OnResponse
	OnLog
	OnChunk
	Count
)

// V1alpha1 returns the configuration spelling, such as "onRequestHeaders".
func (p Phase) V1alpha1() v1alpha1.Phase {
	switch p {
	case OnRequestHeaders:
		return v1alpha1.PhaseOnRequestHeaders
	case OnRequestBody:
		return v1alpha1.PhaseOnRequestBody
	case OnRoute:
		return v1alpha1.PhaseOnRoute
	case OnUpstreamRequest:
		return v1alpha1.PhaseOnUpstreamRequest
	case OnUpstreamResponseHeaders:
		return v1alpha1.PhaseOnUpstreamResponseHeaders
	case OnUpstreamResponseBody:
		return v1alpha1.PhaseOnUpstreamResponseBody
	case OnResponse:
		return v1alpha1.PhaseOnResponse
	case OnLog:
		return v1alpha1.PhaseOnLog
	case OnChunk:
		return v1alpha1.PhaseOnChunk
	default:
		return ""
	}
}

// String returns the configuration spelling; it is also the metric label.
func (p Phase) String() string { return string(p.V1alpha1()) }

// Parse maps a configuration spelling to a Phase.
func Parse(v v1alpha1.Phase) (Phase, bool) {
	for p := OnRequestHeaders; p < Count; p++ {
		if p.V1alpha1() == v {
			return p, true
		}
	}
	return 0, false
}

// IsResponse reports whether the Phase runs in reverse chain order:
// onUpstreamResponseHeaders, onUpstreamResponseBody, onResponse, and
// onChunk (ordered as a response Phase in M1).
func (p Phase) IsResponse() bool {
	return p == OnUpstreamResponseHeaders || p == OnUpstreamResponseBody || p == OnResponse || p == OnChunk
}

// CanShortCircuit reports whether a Filter may respond in the Phase:
// onRequestHeaders, onRequestBody, onRoute and onUpstreamRequest.
func (p Phase) CanShortCircuit() bool { return p <= OnUpstreamRequest }

// ClientLeg reports whether the Phase runs on the client leg (Gateway and
// Route scope): onRequestHeaders, onRequestBody, onRoute, onResponse,
// onLog, onChunk.
func (p Phase) ClientLeg() bool {
	return p <= OnRoute || p == OnResponse || p == OnLog || p == OnChunk
}

// UpstreamLeg reports whether the Phase runs per upstream leg (Upstream
// scope): onUpstreamRequest, onUpstreamResponseHeaders,
// onUpstreamResponseBody, onChunk.
func (p Phase) UpstreamLeg() bool {
	return (p >= OnUpstreamRequest && p <= OnUpstreamResponseBody) || p == OnChunk
}

// Set is a set of Phases.
type Set uint16

// Of returns the set of ps.
func Of(ps ...Phase) Set {
	var s Set
	for _, p := range ps {
		s |= 1 << p
	}
	return s
}

// Has reports whether p is in s.
func (s Set) Has(p Phase) bool { return s&(1<<p) != 0 }

// Add returns s with p.
func (s Set) Add(p Phase) Set { return s | 1<<p }

// Empty reports whether s has no Phase.
func (s Set) Empty() bool { return s == 0 }

// First returns the earliest Phase of s in execution order.
func (s Set) First() (Phase, bool) {
	for p := OnRequestHeaders; p < Count; p++ {
		if s.Has(p) {
			return p, true
		}
	}
	return 0, false
}

// Phases lists the members of s in execution order.
func (s Set) Phases() []Phase {
	var out []Phase
	for p := OnRequestHeaders; p < Count; p++ {
		if s.Has(p) {
			out = append(out, p)
		}
	}
	return out
}

// Class is a Filter class; its value is its rank in a Phase.
type Class uint8

// Filter classes in chain order (foundation pack section 8.12).
const (
	ClassCORS Class = iota
	ClassAuth
	ClassAuthz
	ClassAdmission
	ClassValidation
	ClassCache
	ClassUpstreamAuth
	ClassTransform
	ClassCustom
	NumClasses
)

// V1alpha1 returns the configuration spelling.
func (c Class) V1alpha1() v1alpha1.FilterClass {
	switch c {
	case ClassCORS:
		return v1alpha1.FilterClassCORS
	case ClassAuth:
		return v1alpha1.FilterClassAuth
	case ClassAuthz:
		return v1alpha1.FilterClassAuthz
	case ClassAdmission:
		return v1alpha1.FilterClassAdmission
	case ClassValidation:
		return v1alpha1.FilterClassValidation
	case ClassCache:
		return v1alpha1.FilterClassCache
	case ClassUpstreamAuth:
		return v1alpha1.FilterClassUpstreamAuth
	case ClassTransform:
		return v1alpha1.FilterClassTransform
	case ClassCustom:
		return v1alpha1.FilterClassCustom
	default:
		return ""
	}
}

// String returns the configuration spelling.
func (c Class) String() string { return string(c.V1alpha1()) }

// ParseClass maps a configuration spelling to a Class.
func ParseClass(v v1alpha1.FilterClass) (Class, bool) {
	for c := ClassCORS; c < NumClasses; c++ {
		if c.V1alpha1() == v {
			return c, true
		}
	}
	return 0, false
}

// Scope is where a Policy is attached. The zero value means none (an
// unattached Policy or a non-Policy CEL field).
type Scope uint8

// Scopes in chain order.
const (
	ScopeNone Scope = iota
	ScopeGateway
	ScopeRoute
	ScopeUpstream
)

// String returns "Gateway", "Route", "Upstream" or "".
func (s Scope) String() string {
	switch s {
	case ScopeGateway:
		return "Gateway"
	case ScopeRoute:
		return "Route"
	case ScopeUpstream:
		return "Upstream"
	default:
		return ""
	}
}

// ScopeSet is a set of scopes, such as the registry's allowed scopes.
type ScopeSet uint8

// Scopes returns the set of ss.
func Scopes(ss ...Scope) ScopeSet {
	var out ScopeSet
	for _, s := range ss {
		out |= 1 << s
	}
	return out
}

// Has reports whether s is in set.
func (set ScopeSet) Has(s Scope) bool { return set&(1<<s) != 0 }
```

Tests (WP-01): round trip with every `v1alpha1.Phase` and `v1alpha1.FilterClass` value; `IsResponse`, `CanShortCircuit`, `ClientLeg`, `UpstreamLeg` truth tables against 04 reqs 39 and 42.

### 2.3 Problem errors and RZ codes

`errcode.Error` carries an RZ code through `%w` chains; `errcode.Status` maps a code to its registered status (codes with a `StatusNote` return 0 and the caller decides). New M1 codes are registered here and in the owning documents in the same commit (the `TestMatchesDocs` gate).

`internal/errcode/error.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package errcode

import (
	"errors"
	"fmt"
)

// Error is an error that carries a registered RZ code. Every layer wraps
// client-facing failures in it (directly or through %w), so the code
// survives to the problem writer, the diagnostics and the metrics.
type Error struct {
	// Code is a registered RZ-<AREA>-<NNN> code.
	Code string
	// Err is the cause; it is logged and never sent to a client.
	Err error
}

// Error returns "<code>: <cause>".
func (e *Error) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Code + ": " + e.Err.Error()
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// Wrap annotates err with code. A nil err still yields an error, so callers
// can report a code without a cause.
func Wrap(code string, err error) error { return &Error{Code: code, Err: err} }

// Errorf returns an Error whose cause is fmt.Errorf(format, args...).
func Errorf(code, format string, args ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// CodeOf returns the code of the outermost Error in err's chain.
func CodeOf(err error) (string, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return "", false
}

// Status returns the registered HTTP status of code, or 0 when the code
// has none or is unknown. Callers on the request path resolve statuses at
// snapshot compile time, because Lookup builds the registry per call.
func Status(code string) int {
	c, ok := Lookup(code)
	if !ok {
		return 0
	}
	return c.Status
}
```

Rows added to the `Registry()` table of `internal/errcode/errcode.go` (in ID order):

```go
		{ID: "RZ-CFG-038", Area: AreaCFG, Meaning: "Effective Filter Chain combines a `cache` Policy with an `onRequestBody` authorization, validation or Plugin auth or authz Policy"},
		{ID: "RZ-CFG-039", Area: AreaCFG, Meaning: "A listener or admin port could not be bound during activation; the active Revision keeps serving"},
		{ID: "RZ-CFG-040", Area: AreaCFG, Meaning: "A resource uses a feature this Node release does not serve"},
		{ID: "RZ-CFG-041", Area: AreaCFG, Meaning: "A `secretRef` is used for two destinations (secret-to-destination binding)"},
		{ID: "RZ-RT-016", Area: AreaRT, StatusNote: "503 before commit; stream ended after", Meaning: "The Drain deadline was reached"},
		{ID: "RZ-RT-017", Area: AreaRT, Status: 400, Meaning: "Request target or framing rejected by request hardening"},
		{ID: "RZ-RT-018", Area: AreaRT, Status: 504, Meaning: "An `only-if-cached` request missed the Response Cache"},
		{ID: "RZ-RT-019", Area: AreaRT, Status: 503, Meaning: "The admin `/tap` subscriber limit is reached"},
		{ID: "RZ-UP-011", Area: AreaUP, Status: 502, Meaning: "A merged or non-final composition step got a non-2xx response"},
		{ID: "RZ-AUTH-008", Area: AreaAUTH, Status: 421, Meaning: "An `auth.mtls` Route was reached over a connection that requested no client certificate"},
```

Registry rows added by WP-01 to the owning documents (same wording; parentheses name the adopted open question):

| Document | Rows |
|---|---|
| `docs/architecture/02-configuration-model.md` (after RZ-CFG-037) | RZ-CFG-038 (OQ-traffic-management-and-resilience-21 (a)); RZ-CFG-039 (OQ-zero-downtime-upgrades-and-hot-reload-4 (a)); RZ-CFG-040; RZ-CFG-041 (OQ-security-and-identity-22 (a), R-49) |
| `docs/architecture/03-data-plane.md` (after RZ-RT-015) | RZ-RT-016 (OQ-zero-downtime-upgrades-and-hot-reload-2 (b)); RZ-RT-017; RZ-RT-018; RZ-RT-019 |
| `docs/architecture/08-security-and-identity.md` (after RZ-AUTH-007) | RZ-AUTH-008 |
| `docs/architecture/09-traffic-management-and-resilience.md` (after RZ-UP-010) | RZ-UP-011 |

Codes deliberately not added: a static duplicate-credential RZ-CFG code (runtime RZ-AUTH-002 covers the double match, 06 req 18); a header-count code (reuses RZ-RT-002); an admin 401 code (reuses RZ-AUTH-001 and RZ-AUTH-002); a short API key or uncredentialed State Store URL code (interim RZ-CFG-026, OQ-security-and-identity-23 and -30).

`internal/problem/problem.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package problem writes and reads RFC 9457 problem documents, the error
// body of every Node-generated response and admin error. A document never
// echoes request content: title, status and code come from the RZ registry,
// requestId is the 32-hex trace ID, and detail is fixed operator-authored
// text (RZ-RT-009 names a schema keyword location only).
package problem

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// ContentType is the media type of a problem document.
const ContentType = "application/problem+json"

// Problem is one Node-generated error body. Members are written in the
// order title, status, code, requestId, detail; there is no type member
// (about:blank).
type Problem struct {
	// Title is fixed per code; see Title.
	Title string
	// Status is the HTTP status.
	Status int
	// Code is a registered RZ code.
	Code string
	// RequestID is the request's 32 lowercase hex trace ID.
	RequestID string
	// Detail is optional operator-authored text, never request content.
	Detail string
}

// New returns the problem for code with its registered status and title.
// A code without a single registered status gets status 500 and must be
// given an explicit status by the caller instead.
func New(code, requestID string) Problem {
	status := errcode.Status(code)
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return Problem{Title: Title(code, status), Status: status, Code: code, RequestID: requestID}
}

// Title returns the fixed title of code. RZ-RT codes have their own titles;
// RZ-AUTH titles are generic by status (Security and identity forbids
// revealing the reason); every other code uses the HTTP status text.
func Title(code string, status int) string {
	switch code {
	case "RZ-RT-001":
		return "No matching Route"
	case "RZ-RT-002":
		return "Request header block too large"
	case "RZ-RT-003":
		return "Request body too large"
	case "RZ-RT-004":
		return "Buffer budget exhausted"
	case "RZ-RT-005":
		return "Node at capacity"
	case "RZ-RT-006":
		return "Route match failed"
	case "RZ-RT-007":
		return "Route timeout before any Upstream attempt"
	case "RZ-RT-008":
		return "CORS preflight rejected"
	case "RZ-RT-009":
		return "Request body failed validation"
	case "RZ-RT-010":
		return "No composition step matched"
	case "RZ-RT-011":
		return "Policy could not decide"
	case "RZ-RT-012":
		return "Response Policy failed"
	case "RZ-RT-014":
		return "Configuration snapshot retired"
	case "RZ-RT-015":
		return "Composition step failed"
	case "RZ-RT-016":
		return "Node draining"
	case "RZ-RT-017":
		return "Request rejected"
	case "RZ-RT-018":
		return "Not in cache"
	case "RZ-RT-019":
		return "Tap subscriber limit reached"
	}
	if t := http.StatusText(status); t != "" {
		return t
	}
	return "Error"
}

// Append appends the JSON document to dst.
func Append(dst []byte, p Problem) []byte {
	dst = append(dst, `{"title":`...)
	dst = appendString(dst, p.Title)
	dst = append(dst, `,"status":`...)
	dst = strconv.AppendInt(dst, int64(p.Status), 10)
	dst = append(dst, `,"code":`...)
	dst = appendString(dst, p.Code)
	dst = append(dst, `,"requestId":`...)
	dst = appendString(dst, p.RequestID)
	if p.Detail != "" {
		dst = append(dst, `,"detail":`...)
		dst = appendString(dst, p.Detail)
	}
	return append(dst, '}')
}

// Write sets Content-Type, Cache-Control: no-store and an exact
// Content-Length, copies extra (such as WWW-Authenticate or Retry-After),
// writes the status and the body. It never reads the request.
func Write(w http.ResponseWriter, p Problem, extra http.Header) {
	var buf [256]byte
	body := Append(buf[:0], p)
	h := w.Header()
	for k, vs := range extra {
		h[k] = vs
	}
	h.Set("Content-Type", ContentType)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}

// Document is a decoded problem document from any Ruralz server, for the
// CLI and tests. Unknown members are ignored.
type Document struct {
	Type      string `json:"type,omitempty"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Code      string `json:"code,omitempty"`
	RequestID string `json:"requestId,omitempty"`
}

// Decode parses a problem document.
func Decode(b []byte) (Document, error) {
	var d Document
	if err := json.Unmarshal(b, &d); err != nil {
		return Document{}, fmt.Errorf("problem document: %w", err)
	}
	return d, nil
}

// appendString appends s as a JSON string: minimal RFC 8259 escaping, no
// HTML escaping, invalid UTF-8 replaced by U+FFFD.
func appendString(dst []byte, s string) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				dst = append(dst, '\\', c)
			case c < 0x20:
				dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			default:
				dst = append(dst, c)
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			dst = append(dst, "�"...)
		} else {
			dst = append(dst, s[i:i+n]...)
		}
		i += n
	}
	return append(dst, '"')
}
```

Tests (WP-01): exact bytes of a document with and without `detail`; escaping of control bytes and invalid UTF-8; `Content-Length` and `Cache-Control: no-store`; `Decode` round trip of `Append` output.

### 2.4 Diagnostics

One model for every binary. The text form is one line, `<file>:<line>:<column> <severity> <code> <Kind>/<name> <path>: <message>`, followed by ` (<hint>)`, related locations and ` [environment=<name>]` when present; the JSON form is a bare array with two-space indentation, no HTML escaping, and `[]` plus newline when empty. `Path` renders keyed list items as `routes[name=checkout]` (the area 1 quoting rule) and is the only field-path type (the area 2 `fieldpath` package is dropped).

`internal/config/diag/diag.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package diag is the diagnostics model shared by ruralz, ruralz-control
// (M2) and ruralzd: one Diagnostic type, key-aware paths, the text and JSON
// forms of docs/architecture/02-configuration-model.md "Diagnostics and
// source map", deterministic sorting and nearest-match hints. It imports
// only the standard library, so every configuration stage and both
// binaries emit byte-identical output for the same input.
package diag

import (
	"cmp"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Severity is error or warning. Warnings (RZ-CFG-013, RZ-CFG-025) never
// change a command's exit code.
type Severity uint8

// Severities.
const (
	// SeverityError blocks a Revision.
	SeverityError Severity = iota + 1
	// SeverityWarning is reported only.
	SeverityWarning
)

// String returns "error" or "warning".
func (s Severity) String() string {
	if s == SeverityWarning {
		return "warning"
	}
	return "error"
}

// MarshalText encodes the severity name.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// ResourceID names the resource a diagnostic is about.
type ResourceID struct {
	// Kind is the resource kind, such as Route.
	Kind string `json:"kind"`
	// Name is metadata.name.
	Name string `json:"name"`
}

// Location is a source position. Line and Column are 1-based; Column
// counts Unicode code points; 0 means unknown.
type Location struct {
	// File is the slash path relative to the Bundle root, or as given.
	File string
	// Line is the 1-based line; 0 when unknown.
	Line int
	// Column is the 1-based column in code points; 0 when unknown.
	Column int
}

// ElemKind selects the form of a path element.
type ElemKind uint8

// Path element kinds.
const (
	// ElemField is an object member.
	ElemField ElemKind = iota
	// ElemIndex is a position in an atomic list.
	ElemIndex
	// ElemKeyed is an entry of a map or orderedMap list.
	ElemKeyed
	// ElemItem is an element of a set list.
	ElemItem
)

// PathElem is one step of a key-aware path.
type PathElem struct {
	// Kind selects which other fields are set.
	Kind ElemKind
	// Name is the member name (ElemField).
	Name string
	// Index is the list position (ElemIndex).
	Index int
	// KeyField is the key member, such as "name" (ElemKeyed).
	KeyField string
	// Key is the key value (ElemKeyed).
	Key string
	// Item is the element value (ElemItem): a string, json.Number, bool or
	// a canonical JSON value (json.RawMessage) for object elements.
	Item any
}

// Field returns an object member element.
func Field(name string) PathElem { return PathElem{Kind: ElemField, Name: name} }

// Index returns an atomic list element.
func Index(i int) PathElem { return PathElem{Kind: ElemIndex, Index: i} }

// Keyed returns a map or orderedMap entry element.
func Keyed(keyField, key string) PathElem {
	return PathElem{Kind: ElemKeyed, KeyField: keyField, Key: key}
}

// Item returns a set element.
func Item(v any) PathElem { return PathElem{Kind: ElemItem, Item: v} }

// Path is a key-aware path from a resource root, such as
// spec.composition.steps[name=stock].upstream.
type Path []PathElem

// Append returns a new path with elems appended; p is not modified.
func (p Path) Append(elems ...PathElem) Path {
	out := make(Path, 0, len(p)+len(elems))
	return append(append(out, p...), elems...)
}

// isFieldName reports whether s matches [A-Za-z_$][A-Za-z0-9_$-]*.
func isFieldName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c == '$' || (c|0x20 >= 'a' && c|0x20 <= 'z') || (i > 0 && (c == '-' || (c >= '0' && c <= '9')))
		if !ok {
			return false
		}
	}
	return true
}

// isBare reports whether s matches [A-Za-z0-9_./:@*-]+.
func isBare(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		letter := c|0x20 >= 'a' && c|0x20 <= 'z'
		digit := c >= '0' && c <= '9'
		if !letter && !digit && !strings.ContainsRune("_./:@*-", rune(c)) {
			return false
		}
	}
	return true
}

// String returns the human form: fields joined by ".", a field not
// matching [A-Za-z_$][A-Za-z0-9_$-]* written ["<json>"], keyed entries
// [<keyField>=<key>], set elements [item=<value>], atomic entries [<n>];
// a key or item not matching [A-Za-z0-9_./:@*-]+ is JSON-quoted and object
// items print as canonical JSON.
func (p Path) String() string {
	var b strings.Builder
	for i, e := range p {
		switch e.Kind {
		case ElemField:
			if isFieldName(e.Name) {
				if i > 0 {
					b.WriteByte('.')
				}
				b.WriteString(e.Name)
			} else {
				b.WriteString(`[`)
				b.WriteString(strconv.Quote(e.Name))
				b.WriteString(`]`)
			}
		case ElemIndex:
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(e.Index))
			b.WriteByte(']')
		case ElemKeyed:
			b.WriteByte('[')
			b.WriteString(e.KeyField)
			b.WriteByte('=')
			b.WriteString(quoteValue(e.Key))
			b.WriteByte(']')
		case ElemItem:
			b.WriteString("[item=")
			b.WriteString(itemText(e.Item))
			b.WriteByte(']')
		}
	}
	return b.String()
}

func quoteValue(s string) string {
	if isBare(s) {
		return s
	}
	q, _ := json.Marshal(s)
	return string(q)
}

func itemText(v any) string {
	switch t := v.(type) {
	case string:
		return quoteValue(t)
	case json.RawMessage:
		return string(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "?"
		}
		return string(b)
	}
}

// MarshalJSON returns the JSON form: an array of strings (fields), integers
// (atomic indexes) and single-member objects ({"name":"stock"},
// {"item":"GET"}).
func (p Path) MarshalJSON() ([]byte, error) {
	out := make([]any, 0, len(p))
	for _, e := range p {
		switch e.Kind {
		case ElemField:
			out = append(out, e.Name)
		case ElemIndex:
			out = append(out, e.Index)
		case ElemKeyed:
			out = append(out, map[string]string{e.KeyField: e.Key})
		case ElemItem:
			out = append(out, map[string]any{"item": e.Item})
		}
	}
	return json.Marshal(out)
}

// Related is a secondary location, such as the first of two duplicates.
type Related struct {
	Location
	// Message describes the location, such as "declared in".
	Message string
}

// Diagnostic is one configuration finding.
type Diagnostic struct {
	// Code is a registered RZ-CFG-NNN code.
	Code string
	// Severity is error or warning.
	Severity Severity
	Location
	// Resource is the resource the finding is about, when known.
	Resource *ResourceID
	// Path is the key-aware path inside the resource.
	Path Path
	// Message is one line; it never contains a secret value.
	Message string
	// Hint is optional, such as a nearest match.
	Hint string
	// Related lists secondary locations.
	Related []Related
	// Environment names the Environment of a multi-Environment run.
	Environment string
}

// List is an ordered set of diagnostics.
type List []Diagnostic

// HasErrors reports whether any diagnostic has error severity.
func (l List) HasErrors() bool {
	return slices.ContainsFunc(l, func(d Diagnostic) bool { return d.Severity == SeverityError })
}

// Sort orders by (environment, file, line, column, code, path, message).
func (l List) Sort() {
	slices.SortStableFunc(l, func(a, b Diagnostic) int {
		return cmp.Or(
			cmp.Compare(a.Environment, b.Environment),
			cmp.Compare(a.File, b.File),
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Column, b.Column),
			cmp.Compare(a.Code, b.Code),
			cmp.Compare(a.Path.String(), b.Path.String()),
			cmp.Compare(a.Message, b.Message),
		)
	})
}

// FirstErrorCode returns the code of the first error after Sort, or "".
func (l List) FirstErrorCode() string {
	for _, d := range l {
		if d.Severity == SeverityError {
			return d.Code
		}
	}
	return ""
}

// AppendText appends the one-line text form:
// <file>:<line>:<column> <severity> <code> <Kind>/<name> <path>: <message>
// then " (<hint>)", " (<related message> <file>:<line>:<column>)" per
// related location and " [environment=<name>]". "-" stands for no file.
func (d Diagnostic) AppendText(dst []byte) []byte {
	dst = appendLocation(dst, d.Location)
	dst = append(dst, ' ')
	dst = append(dst, d.Severity.String()...)
	dst = append(dst, ' ')
	dst = append(dst, d.Code...)
	if d.Resource != nil {
		dst = append(dst, ' ')
		dst = append(dst, d.Resource.Kind...)
		dst = append(dst, '/')
		dst = append(dst, d.Resource.Name...)
	}
	if len(d.Path) > 0 {
		dst = append(dst, ' ')
		dst = append(dst, d.Path.String()...)
	}
	dst = append(dst, ": "...)
	dst = append(dst, oneLine(d.Message)...)
	if d.Hint != "" {
		dst = append(dst, " ("...)
		dst = append(dst, oneLine(d.Hint)...)
		dst = append(dst, ')')
	}
	for _, r := range d.Related {
		dst = append(dst, " ("...)
		dst = append(dst, oneLine(r.Message)...)
		dst = append(dst, ' ')
		dst = appendLocation(dst, r.Location)
		dst = append(dst, ')')
	}
	if d.Environment != "" {
		dst = append(dst, " [environment="...)
		dst = append(dst, d.Environment...)
		dst = append(dst, ']')
	}
	return dst
}

func appendLocation(dst []byte, l Location) []byte {
	if l.File == "" {
		return append(dst, '-')
	}
	dst = append(dst, l.File...)
	if l.Line > 0 {
		dst = append(dst, ':')
		dst = strconv.AppendInt(dst, int64(l.Line), 10)
		if l.Column > 0 {
			dst = append(dst, ':')
			dst = strconv.AppendInt(dst, int64(l.Column), 10)
		}
	}
	return dst
}

func oneLine(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(s)
}

// WriteText writes one line per diagnostic, in list order.
func WriteText(w io.Writer, l List) error {
	buf := make([]byte, 0, 256)
	for _, d := range l {
		buf = append(d.AppendText(buf[:0]), '\n')
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

type jsonRelated struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message,omitempty"`
}

type jsonDiagnostic struct {
	Code        string        `json:"code"`
	Severity    Severity      `json:"severity"`
	File        string        `json:"file,omitempty"`
	Line        int           `json:"line,omitempty"`
	Column      int           `json:"column,omitempty"`
	Resource    *ResourceID   `json:"resource,omitempty"`
	Path        Path          `json:"path,omitempty"`
	Message     string        `json:"message"`
	Hint        string        `json:"hint,omitempty"`
	Related     []jsonRelated `json:"related,omitempty"`
	Environment string        `json:"environment,omitempty"`
}

// WriteJSON writes a bare JSON array ("[]" when empty) with members in the
// order code, severity, file, line, column, resource, path, message, hint,
// related, environment; two-space indent, no HTML escaping, final newline.
func WriteJSON(w io.Writer, l List) error {
	out := make([]jsonDiagnostic, 0, len(l))
	for _, d := range l {
		jd := jsonDiagnostic{
			Code: d.Code, Severity: d.Severity, File: d.File, Line: d.Line, Column: d.Column,
			Resource: d.Resource, Path: d.Path, Message: d.Message, Hint: d.Hint, Environment: d.Environment,
		}
		for _, r := range d.Related {
			jd.Related = append(jd.Related, jsonRelated{File: r.File, Line: r.Line, Column: r.Column, Message: r.Message})
		}
		out = append(out, jd)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

// Collector gathers diagnostics from concurrent workers and enforces the
// per-run cap (10,000, proposed); reaching it appends one RZ-CFG-001
// "too many diagnostics" error and rejects further adds.
type Collector struct {
	mu   sync.Mutex
	list List
	max  int
	full bool
}

// NewCollector returns a Collector holding at most maxDiags diagnostics.
func NewCollector(maxDiags int) *Collector { return &Collector{max: maxDiags} }

// Add records d; it returns false once the cap is reached.
func (c *Collector) Add(d Diagnostic) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.full {
		return false
	}
	if c.max > 0 && len(c.list) >= c.max {
		c.full = true
		c.list = append(c.list, Diagnostic{Code: "RZ-CFG-001", Severity: SeverityError, Message: "too many diagnostics; stopped after " + strconv.Itoa(c.max)})
		return false
	}
	c.list = append(c.list, d)
	return true
}

// AddAll records every diagnostic of l.
func (c *Collector) AddAll(l List) bool {
	for _, d := range l {
		if !c.Add(d) {
			return false
		}
	}
	return true
}

// Full reports whether the cap was reached.
func (c *Collector) Full() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.full
}

// List returns a sorted copy.
func (c *Collector) List() List {
	c.mu.Lock()
	out := slices.Clone(c.list)
	c.mu.Unlock()
	out.Sort()
	return out
}

// Nearest returns the best candidate by optimal string alignment distance:
// an exact case-insensitive match first, else the smallest distance at most
// max(1, min(3, len(input)/3)), ties broken by byte order. Candidates whose
// length differs by more than 3 are skipped; at most 64 are compared.
func Nearest(input string, candidates []string) (string, bool) {
	for _, c := range candidates {
		if strings.EqualFold(c, input) {
			return c, true
		}
	}
	limit := max(1, min(3, len(input)/3))
	best, bestD, compared := "", limit+1, 0
	for _, c := range candidates {
		if abs(len(c)-len(input)) > 3 {
			continue
		}
		if compared++; compared > 64 {
			break
		}
		d := osa(strings.ToLower(input), strings.ToLower(c))
		if d < bestD || (d == bestD && c < best) {
			best, bestD = c, d
		}
	}
	return best, bestD <= limit
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// osa is the optimal string alignment distance over bytes.
func osa(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}
```

Tests (WP-01): golden text and JSON for the CM example line; `Sort` order (environment, file, line, column, code, path, message); collector cap appends one RZ-CFG-001 "too many diagnostics"; `Nearest` limits; `Path.String` quoting of names that are not bare.

### 2.5 Value tree

`internal/config/tree/tree.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package tree is the configuration pipeline's only in-memory document
// model: an ordered value tree whose every key and scalar keeps its source
// position. The YAML and JSON front ends produce it; overlay merge,
// substitution, schema validation, defaults, canonicalization and
// rendering all read and rewrite it. Numbers keep their exact text, so no
// value passes through float64 before the canonical encoder.
package tree

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// FileID indexes a FileTable.
type FileID uint32

// Role says why a file was read.
type Role uint8

// File roles.
const (
	// RoleBase is a base Bundle file.
	RoleBase Role = iota + 1
	// RoleOverlay is a file of the selected overlay.
	RoleOverlay
	// RoleEnvironments is an --environments file.
	RoleEnvironments
	// RoleCanonical is ruralz.canonical.v1 content (Last-Known-Good, dumps).
	RoleCanonical
)

// File is one source file.
type File struct {
	// Path is slash-separated and relative to the Bundle root, or as given.
	Path string
	// Role says why the file was read.
	Role Role
}

// FileTable is the append-only table of files read by one load. It is
// safe for concurrent use.
type FileTable struct {
	mu    sync.Mutex
	files []File
}

// Add appends f and returns its ID.
func (t *FileTable) Add(f File) FileID {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := FileID(len(t.files)) //nolint:gosec // G115: a load reads at most 20,000 files.
	t.files = append(t.files, f)
	return id
}

// File returns the file with id; the zero File when id is unknown.
func (t *FileTable) File(id FileID) File {
	t.mu.Lock()
	defer t.mu.Unlock()
	if int(id) >= len(t.files) {
		return File{}
	}
	return t.files[id]
}

// Location converts a position to a diagnostic location.
func (t *FileTable) Location(p Pos) diag.Location {
	if !p.Known() {
		return diag.Location{}
	}
	return diag.Location{File: t.File(p.File).Path, Line: int(p.Line), Column: int(p.Column)}
}

// Pos is a source position; Line and Column are 1-based, Column counts
// code points. The zero Pos is unknown.
type Pos struct {
	// File is the source file.
	File FileID
	// Line is 1-based; 0 means unknown.
	Line int32
	// Column is 1-based in code points; 0 means unknown.
	Column int32
}

// Known reports whether p carries a line.
func (p Pos) Known() bool { return p.Line > 0 }

// Kind is a node's JSON data model type.
type Kind uint8

// Node kinds.
const (
	// KindNull is null (removed by overlay merge before the hub).
	KindNull Kind = iota
	// KindBool is true or false.
	KindBool
	// KindInt is an integer; Text holds its normalized decimal form.
	KindInt
	// KindFloat is a non-integer number; Text holds its source text in
	// RFC 8259 number syntax (see Node.Text).
	KindFloat
	// KindString is a string; Text holds the decoded value.
	KindString
	// KindMap is an object with ordered members.
	KindMap
	// KindList is an array.
	KindList
)

// Style records how a scalar was written; renderers and diagnostics use it.
type Style uint8

// Scalar styles.
const (
	// StylePlain is an unquoted YAML scalar.
	StylePlain Style = iota
	// StyleSingleQuoted is a single-quoted YAML scalar.
	StyleSingleQuoted
	// StyleDoubleQuoted is a double-quoted YAML scalar.
	StyleDoubleQuoted
	// StyleLiteral is a YAML literal block (|).
	StyleLiteral
	// StyleFolded is a YAML folded block (>).
	StyleFolded
	// StyleJSON is a value from a .json file.
	StyleJSON
	// StyleDefaulted is a value materialized from a schema or registry
	// default; diagnostics on it point at the parent.
	StyleDefaulted
)

// Node is one value.
type Node struct {
	// Kind is the data model type.
	Kind Kind
	// Style is how the value was written.
	Style Style
	// Pos is where the value starts.
	Pos Pos
	// Text is the string value, the normalized decimal of an integer or
	// the source text of a float. A float's text is in RFC 8259 number
	// syntax, so JSONValue, jsonval.CheckNumber and
	// jsonval.AppendCanonicalNumber accept it: the loader rewrites a YAML
	// 1.2 core float that is not an RFC 8259 number (.5, 1., +1.5) by
	// dropping a leading '+', putting a '0' before a leading '.' and
	// dropping a '.' with no fraction digits (0.5, 1, 1.5), and never
	// round-trips it through float64. Leading zeros of the integer part,
	// which RFC 8259 also excludes, are stripped keeping one digit (01.5
	// is 1.5).
	Text string
	// Bool is the value of a KindBool node.
	Bool bool
	// Members are the object members in authored order (KindMap).
	Members []Member
	// Items are the list elements (KindList).
	Items []*Node
	// Vars lists the variables substituted into this scalar.
	Vars []string
}

// Member is one object member.
type Member struct {
	// Key is the decoded key; keys are always strings.
	Key string
	// KeyPos is where the key starts.
	KeyPos Pos
	// Value is the member value.
	Value *Node
}

// Get returns the value of member key of a map node.
func (n *Node) Get(key string) (*Node, bool) {
	if n == nil || n.Kind != KindMap {
		return nil, false
	}
	for i := range n.Members {
		if n.Members[i].Key == key {
			return n.Members[i].Value, true
		}
	}
	return nil, false
}

// Set replaces or appends member key of a map node.
func (n *Node) Set(key string, keyPos Pos, v *Node) {
	for i := range n.Members {
		if n.Members[i].Key == key {
			n.Members[i].Value = v
			return
		}
	}
	n.Members = append(n.Members, Member{Key: key, KeyPos: keyPos, Value: v})
}

// Delete removes member key of a map node and reports whether it existed.
func (n *Node) Delete(key string) bool {
	i := slices.IndexFunc(n.Members, func(m Member) bool { return m.Key == key })
	if i < 0 {
		return false
	}
	n.Members = slices.Delete(n.Members, i, i+1)
	return true
}

// Clone returns a deep copy.
func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Vars = slices.Clone(n.Vars)
	if n.Members != nil {
		c.Members = make([]Member, len(n.Members))
		for i, m := range n.Members {
			c.Members[i] = Member{Key: m.Key, KeyPos: m.KeyPos, Value: m.Value.Clone()}
		}
	}
	if n.Items != nil {
		c.Items = make([]*Node, len(n.Items))
		for i, it := range n.Items {
			c.Items[i] = it.Clone()
		}
	}
	return &c
}

// At follows a key-aware path. Keyed elements match the list entry whose
// KeyField member equals Key; Item elements match the first set element
// equal to Item (itemEqual); Index elements select by position.
func (n *Node) At(p diag.Path) (*Node, bool) {
	cur := n
	for _, e := range p {
		switch e.Kind {
		case diag.ElemField:
			var ok bool
			if cur, ok = cur.Get(e.Name); !ok {
				return nil, false
			}
		case diag.ElemIndex:
			if cur == nil || cur.Kind != KindList || e.Index < 0 || e.Index >= len(cur.Items) {
				return nil, false
			}
			cur = cur.Items[e.Index]
		case diag.ElemKeyed:
			if cur == nil || cur.Kind != KindList {
				return nil, false
			}
			i := slices.IndexFunc(cur.Items, func(it *Node) bool {
				k, ok := it.Get(e.KeyField)
				return ok && k.Text == e.Key
			})
			if i < 0 {
				return nil, false
			}
			cur = cur.Items[i]
		case diag.ElemItem:
			if cur == nil || cur.Kind != KindList {
				return nil, false
			}
			i := slices.IndexFunc(cur.Items, func(it *Node) bool { return itemEqual(it, e.Item) })
			if i < 0 {
				return nil, false
			}
			cur = cur.Items[i]
		}
	}
	return cur, cur != nil
}

// itemEqual reports whether n equals a diag.Item value: a string matches a
// string node, a json.Number an integer or float node of equal value, a
// bool a bool node, and a json.RawMessage (object and array elements) the
// node of equal JSON value; numbers compare by float64 value, as the
// canonical form does.
func itemEqual(n *Node, v any) bool {
	switch t := v.(type) {
	case string:
		return n.Kind == KindString && n.Text == t
	case json.Number:
		return numberEqual(n, string(t))
	case bool:
		return n.Kind == KindBool && n.Bool == t
	case json.RawMessage:
		d := json.NewDecoder(bytes.NewReader(t))
		d.UseNumber()
		var x any
		if d.Decode(&x) != nil {
			return false
		}
		return valueEqual(n, x)
	default:
		return false
	}
}

func numberEqual(n *Node, text string) bool {
	if n.Kind != KindInt && n.Kind != KindFloat {
		return false
	}
	if n.Text == text {
		return true
	}
	a, errA := strconv.ParseFloat(n.Text, 64)
	b, errB := strconv.ParseFloat(text, 64)
	return errA == nil && errB == nil && a == b
}

// valueEqual compares n with a value decoded by encoding/json with
// UseNumber.
func valueEqual(n *Node, v any) bool {
	switch t := v.(type) {
	case nil:
		return n.Kind == KindNull
	case string, bool, json.Number:
		return itemEqual(n, t)
	case []any:
		if n.Kind != KindList || len(n.Items) != len(t) {
			return false
		}
		for i := range t {
			if !valueEqual(n.Items[i], t[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		if n.Kind != KindMap || len(n.Members) != len(t) {
			return false
		}
		for _, m := range n.Members {
			x, ok := t[m.Key]
			if !ok || !valueEqual(m.Value, x) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// JSONValue converts to the encoding/json data model: map[string]any,
// []any, string, json.Number, bool or nil.
func (n *Node) JSONValue() any {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case KindBool:
		return n.Bool
	case KindInt, KindFloat:
		return json.Number(n.Text)
	case KindString:
		return n.Text
	case KindMap:
		m := make(map[string]any, len(n.Members))
		for _, mem := range n.Members {
			m[mem.Key] = mem.Value.JSONValue()
		}
		return m
	case KindList:
		l := make([]any, len(n.Items))
		for i, it := range n.Items {
			l[i] = it.JSONValue()
		}
		return l
	default:
		return nil
	}
}

// ID identifies a resource by kind and metadata.name.
type ID struct {
	// Kind is the resource kind.
	Kind v1alpha1.Kind
	// Name is metadata.name.
	Name string
}

// String returns "<Kind>/<name>".
func (id ID) String() string { return string(id.Kind) + "/" + id.Name }

// ResourceID converts to the diagnostic form.
func (id ID) ResourceID() *diag.ResourceID {
	return &diag.ResourceID{Kind: string(id.Kind), Name: id.Name}
}

// Resource is one document after parsing: its identity, apiVersion and
// envelope tree (apiVersion, kind, metadata, spec).
type Resource struct {
	// ID is the identity (kind, metadata.name).
	ID ID
	// APIVersion is the document's apiVersion.
	APIVersion string
	// Root is the envelope mapping.
	Root *Node
	// Start is the document start in its file.
	Start Pos
}
```

Tests (WP-01): `At` over keyed and indexed paths and over set items of every `diag.Item` form (string, `json.Number` compared by value, bool, `json.RawMessage` object elements); `Clone` independence; `JSONValue` of every kind; `FileTable.Location`.

### 2.6 Revision digest

`internal/config/revision/revision.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package revision is the Revision identity: the SHA-256 digest of the
// exact ruralz.canonical.v1 bytes. The wire and storage form is
// sha256:<64 lowercase hex>; the display form rev-<12 hex> appears only in
// human output, logs and metric labels, never as an expected digest.
package revision

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Digest is a Revision digest. The zero value means "none".
type Digest struct{ sum [sha256.Size]byte }

// Revision is canonical content and its digest.
type Revision struct {
	// Digest is SHA-256 over Content.
	Digest Digest
	// Content is the exact ruralz.canonical.v1 bytes (no trailing newline).
	Content []byte
}

// ErrSyntax reports a malformed digest string.
var ErrSyntax = errors.New("revision: want sha256:<64 lowercase hex>")

// Sum returns the digest of canonical bytes.
func Sum(canonical []byte) Digest { return Digest{sum: sha256.Sum256(canonical)} }

// Parse accepts exactly "sha256:" followed by 64 lowercase hex characters.
func Parse(s string) (Digest, error) {
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(h) != 2*sha256.Size || !lowerHex(h) {
		return Digest{}, ErrSyntax
	}
	var d Digest
	_, _ = hex.Decode(d.sum[:], []byte(h))
	return d, nil
}

// ParseDisplay accepts "rev-" followed by 12 lowercase hex characters and
// returns the 12-hex prefix (for M2 lookups by display form).
func ParseDisplay(s string) (string, error) {
	h, ok := strings.CutPrefix(s, "rev-")
	if !ok || len(h) != 12 || !lowerHex(h) {
		return "", errors.New("revision: want rev-<12 lowercase hex>")
	}
	return h, nil
}

// String returns "sha256:<64 hex>".
func (d Digest) String() string { return "sha256:" + d.Hex() }

// Short returns "rev-<first 12 hex>".
func (d Digest) Short() string { return "rev-" + d.Hex()[:12] }

// Hex returns the 64 lowercase hex characters.
func (d Digest) Hex() string { return hex.EncodeToString(d.sum[:]) }

// IsZero reports whether d is the zero value.
func (d Digest) IsZero() bool { return d == Digest{} }

// Bytes returns a copy of the raw sum.
func (d Digest) Bytes() [sha256.Size]byte { return d.sum }

// MarshalText encodes the wire form.
func (d Digest) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText parses the wire form.
func (d *Digest) UnmarshalText(b []byte) error {
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
```

Tests (WP-01): `Parse` rejects upper-case hex, wrong length and missing prefix; `Short` is `rev-` plus 12 hex; text round trip.

### 2.7 CEL contracts

`internal/expr` is the stdlib-only contract every consumer of CEL codes against (config validation, snapshot compile, Router, executor, filters, access log, Upstream layer). `internal/cel` implements `Compiler`, `Builder`, `Program` and `Value` with cel-go. The place table is the single source for result types, variables and runtime error rules (03 section B and G). Wave 2 boundary additions (additive): `Compiler.PrepareRoute` and `PrepareConsumer`, the only writers of `Route.Prepared` and `Consumer.Prepared`, live on the contract because `gateway/compile` may import neither `internal/cel` nor `cel/celtypes`; WP-34 implements them by delegating to `celtypes.PrepareRoute` and `PrepareConsumer` (also in `celtest`), and WP-70 calls them after `identity` builds the Consumers and after it fills each Route's `expr.Route`. `Consumer.QuotaByName` (06 req 21) is filled by `identity.CompileConsumer` (first entry wins on a duplicate quota name) and read by the `quota` Filter (WP-62: a missing Consumer or quota is 403 RZ-RL-004); CEL reads `Quotas`, not this field. The `headers` places cover both `set[]` and `add[]` entries, which share one schema definition per side, so no PlaceID is added; WP-56 emits one `expr.Site` per `add[]` entry as for `set[]` (07 req 24). `JoinedHeader` folds ASCII only and picks deterministically, as `celtypes` does.

`internal/expr/expr.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package expr is the contract of Ruralz CEL expressions (ADR-0011): the
// places where CEL is allowed, the site of one occurrence, compiled
// programs, the activation (Vars) and its request views, and runtime
// errors. It imports no CEL library: internal/cel implements Compiler,
// Builder, Program and Value on cel.dev/cel-go, and every other package
// (configuration validation, Router, Filters, Upstream layer, access log)
// depends only on this package.
package expr

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// PlaceID names a field whose schema carries x-ruralz-cel.
type PlaceID string

// The 21 places of docs/architecture/02-configuration-model.md "Allowed
// places". A test in internal/cel maps each one to exactly one schema
// annotation.
//
// PlaceHeadersRequestValue covers the valueExpression of every
// config.request.set[] and add[] entry (schema definition HeaderRequestSet),
// and PlaceHeadersResponseValue does the same for config.response
// (HeaderResponseSet): set[] and add[] share one definition per side, so the
// set[] spelling of the ID names both.
const (
	PlaceRouteMatchWhen         PlaceID = "Route.spec.match.when"
	PlacePolicyWhen             PlaceID = "Policy.spec.when"
	PlaceStepPathExpression     PlaceID = "Route.spec.composition.steps[].pathExpression"
	PlaceStepWhen               PlaceID = "Route.spec.composition.steps[].when" //nolint:gosec // G101: a CEL place name, not a credential.
	PlaceAuthzCELRule           PlaceID = "authz.cel config.rule"
	PlaceRateLimitKey           PlaceID = "ratelimit config.key"
	PlaceQuotaKey               PlaceID = "quota config.key"
	PlaceHeadersRequestValue    PlaceID = "headers config.request.set[].valueExpression"
	PlaceHeadersResponseValue   PlaceID = "headers config.response.set[].valueExpression"
	PlaceCacheKey               PlaceID = "cache config.key"
	PlaceTransformRequestBody   PlaceID = "transform.request config.body"
	PlaceTransformRequestValue  PlaceID = "transform.request config.set[].valueExpression"
	PlaceTransformResponseBody  PlaceID = "transform.response config.body"
	PlaceTransformResponseValue PlaceID = "transform.response config.set[].valueExpression"
	PlaceHashKey                PlaceID = "Upstream.spec.loadBalancing.hashKey"
	PlaceRetryOn                PlaceID = "Upstream.spec.retries.retryOn"
	PlaceFailureWhen            PlaceID = "Upstream.spec.circuitBreaker.failureWhen"
	PlaceAccessLogWhen          PlaceID = "Gateway.spec.telemetry.accessLog.when"
	PlaceSemanticCacheKey       PlaceID = "ai.semantic-cache config.key"   // validation only until M3
	PlaceCandidateWhen          PlaceID = "AIModel.spec.candidates[].when" // validation only until M3
	PlaceMessagingKey           PlaceID = "Upstream.spec.messaging.key"    // validation only until M4
)

// Result is the result type a place requires.
type Result uint8

// Result types.
const (
	ResultBool Result = iota + 1
	ResultString
	ResultDyn
)

// Var is a bit set of CEL variables.
type Var uint16

// Variables.
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
	VarAI   // declared for validation; evaluated from M3
	VarSelf // reserved for x-ruralz-validations (M2)
)

// VarsBase is request, source, route, consumer, auth and now.
const VarsBase = VarRequest | VarSource | VarRoute | VarConsumer | VarAuth | VarNow

// Names returns the x-ruralz-cel spellings of the variables in v, in
// declaration order.
func (v Var) Names() []string {
	names := [...]string{
		"request", "source", "route", "consumer", "auth", "now", "response",
		"error", "upstream", "attempt", "steps", "duration", "ai", "self",
	}
	var out []string
	for i, n := range names {
		if v&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return out
}

// ErrorRule is a place's normative runtime-error behavior.
type ErrorRule uint8

// Runtime error rules (docs/architecture/02-configuration-model.md
// "Allowed places", runtime-error column).
const (
	RuleInternalError  ErrorRule = iota + 1 // 500 RZ-RT-006, no fallthrough
	RulePolicyWhen                          // closed: the Policy runs; open: skipped
	RuleStepFails                           // optional decides; else 502 RZ-RT-015
	RuleAuthzUndecided                      // 403 RZ-AUTH-015
	RuleFailureMode                         // the Policy's failureMode
	RuleCacheBypass                         // cache bypassed, whatever failureMode
	RuleRandomEndpoint                      // weighted random Endpoint
	RuleNoRetry                             // no retry
	RuleCountFailure                        // the attempt counts as a failure
	RuleWriteEntry                          // the access log entry is written
	RuleSkipCandidate                       // M3
	RuleBadGateway                          // M4 messaging: 502
)

// Place describes one CEL place.
type Place struct {
	// ID names the place.
	ID PlaceID
	// Result is the required result type.
	Result Result
	// Vars is the union over scopes; it equals the x-ruralz-cel variables.
	Vars Var
	// Rule is the runtime-error behavior.
	Rule ErrorRule
	// Runtime is the milestone whose code evaluates the place.
	Runtime string
}

// Places returns the place table, sorted by ID.
func Places() []Place {
	base := VarsBase
	out := []Place{
		{PlaceRouteMatchWhen, ResultBool, VarRequest | VarSource | VarNow, RuleInternalError, "M1"},
		{PlacePolicyWhen, ResultBool, base | VarResponse, RulePolicyWhen, "M1"},
		{PlaceStepPathExpression, ResultString, base | VarSteps, RuleStepFails, "M1"},
		{PlaceStepWhen, ResultBool, base | VarSteps, RuleStepFails, "M1"},
		{PlaceAuthzCELRule, ResultBool, base, RuleAuthzUndecided, "M1"},
		{PlaceRateLimitKey, ResultString, base, RuleFailureMode, "M1"},
		{PlaceQuotaKey, ResultString, base, RuleFailureMode, "M1"},
		{PlaceHeadersRequestValue, ResultString, base, RuleFailureMode, "M1"},
		{PlaceHeadersResponseValue, ResultString, base | VarResponse | VarUpstream, RuleFailureMode, "M1"},
		{PlaceCacheKey, ResultString, base, RuleCacheBypass, "M1"},
		{PlaceTransformRequestBody, ResultDyn, base | VarUpstream, RuleFailureMode, "M1"},
		{PlaceTransformRequestValue, ResultString, base | VarUpstream, RuleFailureMode, "M1"},
		{PlaceTransformResponseBody, ResultDyn, base | VarResponse | VarUpstream, RuleFailureMode, "M1"},
		{PlaceTransformResponseValue, ResultString, base | VarResponse | VarUpstream, RuleFailureMode, "M1"},
		{PlaceHashKey, ResultString, base, RuleRandomEndpoint, "M1"},
		{PlaceRetryOn, ResultBool, VarRequest | VarResponse | VarError | VarAttempt | VarUpstream, RuleNoRetry, "M1"},
		{PlaceFailureWhen, ResultBool, VarRequest | VarResponse | VarError | VarUpstream, RuleCountFailure, "M1"},
		{PlaceAccessLogWhen, ResultBool, base | VarResponse | VarUpstream | VarDuration, RuleWriteEntry, "M1"},
		{PlaceSemanticCacheKey, ResultString, base | VarAI, RuleCacheBypass, "M3"},
		{PlaceCandidateWhen, ResultBool, base | VarAI, RuleSkipCandidate, "M3"},
		{PlaceMessagingKey, ResultString, base, RuleBadGateway, "M4"},
	}
	slices.SortFunc(out, func(a, b Place) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out
}

// LookupPlace returns the place with id.
func LookupPlace(id PlaceID) (Place, bool) {
	for _, p := range Places() {
		if p.ID == id {
			return p, true
		}
	}
	return Place{}, false
}

// Site is one occurrence of a place; it refines the environment by
// attachment (docs: 03-cel requirements 10 to 15). The configuration
// pipeline computes sites after effective Filter Chains (stage J); the
// snapshot compiler passes the same sites to Builder.Compile.
type Site struct {
	// Place is the field's place.
	Place PlaceID
	// Scope is the attachment scope of a Policy field; ScopeNone otherwise.
	Scope phase.Scope
	// PolicyType is set for Policy fields.
	PolicyType v1alpha1.PolicyType
	// FirstPhase is the Policy's first Phase at this attachment
	// (Policy.spec.when only; response Phases declare response).
	FirstPhase phase.Phase
	// GatesBody is true when the Policy gates the request body at this
	// attachment, so Policy.spec.when may select request.body.
	GatesBody bool
	// Mode is the composition mode (composition places).
	Mode v1alpha1.CompositionMode
	// EarlierSteps are the names of earlier steps in list order.
	EarlierSteps []string
}

// Issue is one compile finding: RZ-CFG-014 or RZ-CFG-015.
type Issue struct {
	// Code is "RZ-CFG-014" or "RZ-CFG-015".
	Code string
	// Offset is the byte offset in the expression; -1 when not positional.
	Offset int
	// Line and Column are 1-based within the expression; 0 when unknown.
	Line, Column int
	// Message is deterministic and prefixed "CEL <place>: ".
	Message string
	// Hint is optional.
	Hint string
}

// CostRange is a static cost estimate at nominal sizes.
type CostRange struct{ Min, Max uint64 }

// Refs are static facts about what a program reads.
type Refs struct {
	// Vars are the variables referenced.
	Vars Var
	// RequestBody is true when request.body is selected (a body gate).
	RequestBody bool
	// ResponseBody is true when response.body is selected.
	ResponseBody bool
	// Steps are constant steps keys, sorted and unique.
	Steps []string
	// StepBodies are constant keys whose .body is selected (step gates).
	StepBodies []string
	// AnyStep is true for a non-constant steps key.
	AnyStep bool
}

// Program is an immutable compiled expression, safe for concurrent
// evaluation. Evaluation reads v and never writes it; ctx supplies the
// request deadline used by cost-tracked programs with comprehensions.
type Program interface {
	// Place returns the place the program was compiled for.
	Place() PlaceID
	// Source returns the expression text.
	Source() string
	// Refs returns what the program reads.
	Refs() Refs
	// Cost returns the static estimate at nominal sizes.
	Cost() CostRange
	// EvalBool evaluates a bool place; a non-bool result is KindResultType.
	EvalBool(ctx context.Context, v *Vars) (bool, error)
	// EvalString evaluates a string place.
	EvalString(ctx context.Context, v *Vars) (string, error)
	// EvalValue evaluates a dyn place (transform bodies).
	EvalValue(ctx context.Context, v *Vars) (Value, error)
}

// ProgramSet is the immutable program set of one snapshot, passed as the
// previous set to the next snapshot's Builder so identical (environment,
// source) pairs reuse their Program across Revisions.
type ProgramSet interface {
	// Len returns the number of programs.
	Len() int
}

// Builder collects the programs of one snapshot. Compile is safe for
// concurrent use by the loader's workers; Build is called once after.
type Builder interface {
	// Compile returns a program, or nil and at least one error Issue.
	Compile(site Site, src string) (Program, []Issue)
	// Build returns the set; the Builder is spent.
	Build() ProgramSet
}

// Compiler owns the CEL environments. It is safe for concurrent use and
// built once per process.
type Compiler interface {
	// Check compiles and cost-checks src at site without keeping a program
	// (validation in every binary). An empty src is absent: no Issue.
	Check(site Site, src string) []Issue
	// ReferencesRequestBody reports whether an authz.cel rule selects
	// request.body, which moves it to onRequestBody.
	ReferencesRequestBody(src string) (bool, error)
	// NewBuilder starts a snapshot's program set; prev may be nil.
	NewBuilder(prev ProgramSet) Builder
	// Default returns the compiled default of PlaceRetryOn or
	// PlaceFailureWhen, used when the field is absent.
	Default(p PlaceID) Program
	// DecodeJSON decodes one JSON document into a Value for CEL (bodies,
	// claims). built is the size charged to limits.maxBufferedBytes; past
	// budget it returns ErrTooLarge.
	DecodeJSON(data []byte, budget int64) (v Value, built int64, err error)
	// PrepareRoute caches r's converted CEL values in r.Prepared; the
	// snapshot compiler calls it once per snapshot for every Route before
	// any evaluation reads r, and r must not change afterwards.
	PrepareRoute(r *Route)
	// PrepareConsumer does the same for a compiled Consumer: it caches c's
	// converted CEL values in c.Prepared, once per snapshot, before any
	// evaluation reads c, and c must not change afterwards.
	PrepareConsumer(c *Consumer)
}

// Default CEL sources of Upstream.spec.retries.retryOn and
// circuitBreaker.failureWhen when the field is absent (runtime rule,
// OQ-traffic-management-and-resilience-6).
const (
	DefaultRetryOn     = `error != null ? (error.kind == "connect" || (error.kind == "reset" && request.method in ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"])) : (response.status == 503 && request.method in ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"])`
	DefaultFailureWhen = `error != null || response.status in [502, 503, 504]`
)

// Value is an opaque decoded value owned by the CEL implementation: a JSON
// body, JWT claims or a dyn result. Maps iterate keys in ascending byte
// order.
type Value interface {
	// IsNull reports JSON null.
	IsNull() bool
	// AppendBody writes a transform body result: a map or list as JSON with
	// sorted keys, a top-level string as its UTF-8 bytes, bytes raw, other
	// scalars as JSON text.
	AppendBody(dst []byte) ([]byte, error)
	// Native converts to map[string]any, []any, string, json.Number, bool
	// or nil, for code that edits documents (transforms).
	Native() (any, error)
}

// ErrTooLarge is returned by DecodeJSON past its budget.
var ErrTooLarge = errors.New("expr: decoded value over budget")

// ErrorKind classifies a runtime error.
type ErrorKind uint8

// Runtime error kinds.
const (
	KindCostLimit ErrorKind = iota + 1
	KindDeadline
	KindNull
	KindNoSuchKey
	KindNoSuchOverload
	KindConversion
	KindArithmetic
	KindResultType
	KindBody
	KindOther
)

// String returns the kind name used as span error.type.
func (k ErrorKind) String() string {
	switch k {
	case KindCostLimit:
		return "cost_limit"
	case KindDeadline:
		return "deadline"
	case KindNull:
		return "null"
	case KindNoSuchKey:
		return "no_such_key"
	case KindNoSuchOverload:
		return "no_such_overload"
	case KindConversion:
		return "conversion"
	case KindArithmetic:
		return "arithmetic"
	case KindResultType:
		return "result_type"
	case KindBody:
		return "body"
	default:
		return "other"
	}
}

// EvalError is a runtime error. Error never contains request data; the
// library text is available only through Detail, for debug logs and tests.
type EvalError struct {
	// Place is where the program ran.
	Place PlaceID
	// Kind classifies the error.
	Kind   ErrorKind
	detail string
}

// NewEvalError returns an EvalError; detail may contain request data.
func NewEvalError(place PlaceID, kind ErrorKind, detail string) *EvalError {
	return &EvalError{Place: place, Kind: kind, detail: detail}
}

// Error returns "cel: <place>: <kind>".
func (e *EvalError) Error() string { return "cel: " + string(e.Place) + ": " + e.Kind.String() }

// Detail returns the library text; never send it to a client or a span.
func (e *EvalError) Detail() string { return e.detail }

// Param is one path template capture.
type Param struct{ Name, Value string }

// Request is the request view. Fields are set by the data plane; Header is
// the live canonical header map (read-only while referenced by an
// evaluation).
type Request struct {
	// Method is the method as received.
	Method string
	// Scheme is "http" or "https".
	Scheme string
	// Host is lowercased without port (the Router's normalized host).
	Host string
	// Path is the one normalized path.
	Path string
	// PathParams are template captures in template order.
	PathParams []Param
	// Header holds request headers; CEL reads them lowercased and joined.
	Header http.Header
	// Body is the decoded JSON body; nil when not available.
	Body Value

	rawQuery string
	query    atomic.Pointer[map[string]string]
}

// SetRawQuery sets the raw query and drops the parsed cache. Call it only
// while no evaluation reads the view.
func (r *Request) SetRawQuery(q string) {
	r.rawQuery = q
	r.query.Store(nil)
}

// RawQuery returns the raw query.
func (r *Request) RawQuery() string { return r.rawQuery }

// Query returns request.query: parameters percent-decoded, a repeated
// parameter joined by ",". It is parsed once, lazily, and is safe for
// concurrent readers.
func (r *Request) Query() map[string]string {
	if m := r.query.Load(); m != nil {
		return *m
	}
	vals, _ := url.ParseQuery(r.rawQuery)
	m := make(map[string]string, len(vals))
	for k, vs := range vals {
		m[k] = strings.Join(vs, ",")
	}
	r.query.CompareAndSwap(nil, &m)
	return *r.query.Load()
}

// Reset clears r for reuse from a pool.
func (r *Request) Reset() {
	h := r.Header
	clear(h)
	*r = Request{Header: h, PathParams: r.PathParams[:0]}
}

// Source is the client address view.
type Source struct {
	// IP is source.ip after trusted-proxy handling, IPv4-mapped unmapped.
	IP netip.Addr
	// Port is the peer port; 0 when taken from a forwarding header.
	Port int
	// TLSVersion is tls.VersionTLS12 or VersionTLS13; 0 on cleartext.
	TLSVersion uint16
	// ClientCertSubject is the RFC 4514 subject verified by this Route's
	// auth.mtls, else "".
	ClientCertSubject string
}

// Route is the route variable, built once per snapshot.
type Route struct {
	// Name is metadata.name.
	Name string
	// Labels are metadata.labels.
	Labels map[string]string
	// Prepared is set only by Compiler.PrepareRoute; nil is valid, and the
	// CEL implementation then converts on selection.
	Prepared any
}

// Consumer is the compiled, read-only Consumer shared by CEL, quota keys,
// cache partitions and logs; built once per snapshot.
type Consumer struct {
	// Name is metadata.name.
	Name string
	// Tier is spec.tier, "" when unset.
	Tier string
	// Tags is spec.tags, sorted.
	Tags []string
	// Labels are metadata.labels.
	Labels map[string]string
	// Quotas are the names of spec.quotas, sorted.
	Quotas []string
	// QuotaByName holds spec.quotas by name for the quota and
	// ai.token-budget Policies (spec 06 requirement 21); nil when there are
	// none; shared by every snapshot reader, read only; CEL reads Quotas,
	// not this field.
	QuotaByName map[string]v1alpha1.Quota
	// Prepared is set only by Compiler.PrepareConsumer; nil is valid, and
	// the CEL implementation then converts on selection.
	Prepared any
}

// Auth is the auth variable.
type Auth struct {
	// Method is "jwt", "api-key", "basic" or "mtls".
	Method string
	// Claims is the verified JWT payload; an empty map for other methods.
	Claims Value
}

// Response is the response view.
type Response struct {
	// Status is the HTTP status.
	Status int
	// Header holds response headers.
	Header http.Header
	// Body is the decoded JSON body; nil when not available.
	Body Value
}

// AttemptError is the error variable: kind is connect, timeout, reset or tls.
type AttemptError struct{ Kind string }

// Upstream is the upstream variable: the Upstream name and the selected
// Endpoint address host:port.
type Upstream struct{ Name, Endpoint string }

// Step is one completed composition step.
type Step struct {
	// Status is the step's response status.
	Status int
	// Header holds the step's response headers.
	Header http.Header
	// Body is the decoded body when the step is a gate, else nil.
	Body Value
}

// Steps holds completed steps in list order; skipped, failed-optional and
// not-yet-run steps are absent.
type Steps struct {
	names []string
	steps []Step
}

// Add records a completed step.
func (s *Steps) Add(name string, st Step) {
	s.names = append(s.names, name)
	s.steps = append(s.steps, st)
}

// Get returns the named step.
func (s *Steps) Get(name string) (Step, bool) {
	i := slices.Index(s.names, name)
	if i < 0 {
		return Step{}, false
	}
	return s.steps[i], true
}

// Names returns the completed step names in list order.
func (s *Steps) Names() []string { return s.names }

// Reset clears s for reuse.
func (s *Steps) Reset() { s.names, s.steps = s.names[:0], s.steps[:0] }

// AI is the ai variable (declared for validation; evaluated from M3).
type AI struct {
	Model                string
	EstimatedInputTokens int64
	MaxOutputTokens      int64
	Stream               bool
}

// Vars is the activation of one evaluation point. A nil pointer is CEL
// null. One Vars belongs to one goroutine; an upstream leg evaluates
// against its own copy with the leg fields replaced.
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
	AI       *AI
	// Now is the request start time, UTC.
	Now time.Time
	// Duration is the total request time (onLog and accessLog.when).
	Duration time.Duration
}

// Reset clears v for reuse from a pool.
func (v *Vars) Reset() { *v = Vars{} }

// JoinedHeader returns a request or response header as CEL sees it: the
// lookup is case-insensitive and repeated field lines are joined by ", ".
// The exact http.CanonicalHeaderKey key wins, so a canonical key is always
// preferred; otherwise, among the keys equal to name under ASCII case
// folding (field names are ASCII, so Unicode folding never applies), the
// smallest in byte order wins. The choice is deterministic (03 requirement
// 6.3) and equals the CEL implementation's.
func JoinedHeader(h http.Header, name string) (string, bool) {
	vs, ok := h[http.CanonicalHeaderKey(name)]
	if !ok {
		best := ""
		for k, v := range h {
			if asciiEqualFold(k, name) && (!ok || k < best) {
				vs, best, ok = v, k, true
			}
		}
	}
	if !ok {
		return "", false
	}
	if len(vs) == 1 {
		return vs[0], true
	}
	return strings.Join(vs, ", "), true
}

// asciiEqualFold reports whether a and b are equal under ASCII case folding.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if asciiLower(a[i]) != asciiLower(b[i]) {
			return false
		}
	}
	return true
}

// asciiLower lowers an ASCII upper-case letter.
func asciiLower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}
```

Tests (WP-01): `Places()` sorted and unique; every M1 place present with the variables of 03 section B; `Request.Query` lazily joins repeated values with ","; `Vars.Reset` clears every field; `JoinedHeader` joins with ", ", returns the same result on every call when two non-canonical keys fold to one name, and never matches `k` against a key holding the Kelvin sign U+212A (ASCII folding, 03 req 6.3).

### 2.8 Secrets

The redacting `Value` never prints its bytes (`String`, `GoString`, `Format`, `MarshalJSON`, `MarshalText`, `LogValue` all give `[REDACTED]`); only `Reveal` returns a copy. `Resolver` is implemented by `internal/secret/resolver`, which only `ruralzd` links. Rotation after carry-over (R-55): a `Store`'s set of references is fixed but its values follow rotations, and `Watch` registrations are Node-wide by `Ref`, owned by the resolver and kept across `Activate`, so a Filter carried over through `BuildEnv.Previous` (API-key `KeyIndex`, `auth.mtls` CA and CRL, listener certificates) keeps receiving rotations; the Filter's `Close` calls `stop`. Wave 2 boundary (doc only, plus the additive `Use.Loc`): within one poll the resolver publishes every changed reference of the active `Store` before it calls any watcher, so a watcher can `Get` the other half of a certificate and key pair; a watcher's failure never withdraws a published value; and `fn` may call `Watch` and any stop function, which lets `tlsconf` re-register a failed sibling half once a later callback forms a valid pair, clearing a stuck `secret_rotation_failed`.

`internal/secret/secret.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package secret is the contract for resolved secretRef values. Only Nodes
// resolve secrets (Security and identity, "Secrets" rule 1); the resolver
// lives in internal/secret/resolver, which ruralz and ruralz-control never
// import (depguard). A Value never prints its bytes: every formatting,
// logging and encoding path yields "[REDACTED]".
package secret

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Redacted replaces every secret value in output.
const Redacted = "[REDACTED]"

// Ref identifies a secret: provider, name and optional key. It is part of
// the canonical form; the resolved value never is.
type Ref struct {
	// Provider is env or file in M1 (kubernetes and vault from M2).
	Provider v1alpha1.SecretProvider
	// Name is the variable name or absolute file path.
	Name string
	// Key selects a JSON member of a file; unused for env.
	Key string
}

// RefOf converts a configuration reference.
func RefOf(r v1alpha1.SecretRef) Ref { return Ref{Provider: r.Provider, Name: r.Name, Key: r.Key} }

// String returns "<provider>:<name>" plus "#<key>" when set; never a value.
func (r Ref) String() string {
	s := string(r.Provider) + ":" + r.Name
	if r.Key != "" {
		s += "#" + r.Key
	}
	return s
}

// Value is a resolved secret. Its bytes are reachable only through Reveal.
type Value struct{ b []byte }

// NewValue copies b into a set Value; an empty b gives a set, empty value
// (IsZero false), a nil b the unset one.
func NewValue(b []byte) Value {
	if b == nil {
		return Value{}
	}
	c := make([]byte, len(b))
	copy(c, b)
	return Value{b: c}
}

// Reveal returns a copy of the bytes (nil when unset). Never log or wrap
// the result.
func (v Value) Reveal() []byte { return bytes.Clone(v.b) }

// Len returns the value length.
func (v Value) Len() int { return len(v.b) }

// IsZero reports an unset value; a resolved empty secret is not zero.
func (v Value) IsZero() bool { return v.b == nil }

// String returns "[REDACTED]".
func (Value) String() string { return Redacted }

// GoString returns "[REDACTED]".
func (Value) GoString() string { return Redacted }

// Format writes "[REDACTED]" for every verb.
func (Value) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// MarshalJSON returns the JSON string "[REDACTED]".
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// MarshalText returns "[REDACTED]".
func (Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// LogValue returns "[REDACTED]" for log/slog.
func (Value) LogValue() slog.Value { return slog.StringValue(Redacted) }

// Kind says what a value holds; it selects the check run at resolution
// and rotation, whose failure is RZ-CFG-026 (activation) or a rotation
// failure (running Node).
type Kind uint8

// Secret kinds.
const (
	// KindOpaque is checked only for size.
	KindOpaque Kind = iota
	// KindPEMCertificate is a PEM certificate chain.
	KindPEMCertificate
	// KindPEMPrivateKey is a PEM private key.
	KindPEMPrivateKey
	// KindPEMCertPool is a PEM CA bundle.
	KindPEMCertPool
	// KindPEMCRL is PEM certificate revocation lists (16 MiB cap).
	KindPEMCRL
	// KindAPIKey is an API key: trimmed, at least 22 bytes.
	KindAPIKey
	// KindStateStoreURL is a State Store URL that must fit its topology.
	KindStateStoreURL
)

// Use is one reference to resolve, with its location for diagnostics.
type Use struct {
	// Ref is the reference.
	Ref Ref
	// Resource and Path locate the x-ruralz-secret field.
	Resource diag.ResourceID
	// Path is the key-aware path of the field.
	Path diag.Path
	// Loc is the secretRef's source position; zero when unknown, such as
	// canonical re-entry.
	Loc diag.Location
	// Kind selects the default check and size cap.
	Kind Kind
	// Check is an extra consumer check (such as a State Store URL fitting
	// its topology); nil for none. Its error text must not contain the value.
	Check func([]byte) error
}

// Store is the resolved secret table of one Revision, read lock-free on
// the request path. The set of references a Store holds never changes;
// their values follow rotations (copy-on-write in the resolver's
// Node-wide table), so a Store kept by a carried-over Filter or a retired
// snapshot keeps seeing current values.
type Store interface {
	// Get returns the latest resolved value of r, rotations included;
	// false when r is not one of the Store's uses. Get returns a new value
	// as soon as the resolver publishes it. Within one poll the resolver
	// publishes every changed reference of the active Store before it
	// calls any watcher, so a watcher can Get the other half of a pair
	// (a certificate and its key) and see this poll's value. A watcher's
	// error or panic never withdraws a published value: Get keeps
	// returning it, and only that watcher keeps its last value and counts
	// a failure. A file value that fails a use check of the active Store
	// is never published: Get keeps the last good value and
	// secret_rotation_failed is raised.
	Get(r Ref) (Value, bool)
	// Watch registers fn for rotations of r. Registrations are Node-wide,
	// keyed by Ref and owned by the resolver: they survive Activate of a
	// later Store (a Filter carried over through BuildEnv.Previous keeps
	// its watch), fire whenever the active Store polls r, and stay silent
	// while no active Store references r. fn runs on the resolver's
	// goroutine with no resolver lock held and must not block; an fn error
	// keeps the watcher's last value and counts a rotation failure. fn may
	// call Watch and any stop function, its own included. A Watch
	// registered inside fn starts at the current version of its reference
	// and does not receive the value being delivered; after a stop of the
	// running watcher, fn's result is ignored. stop unregisters and is
	// called from the Filter's Close.
	Watch(r Ref, fn func(Value) error) (stop func())
}

// Resolver resolves every Use of a Revision before activation (env and
// file in M1) and polls file references afterwards.
type Resolver interface {
	// Resolve returns a Store holding every use, or RZ-CFG-026 diagnostics
	// for each failing use (all reported, values never included).
	Resolve(ctx context.Context, uses []Use) (Store, diag.List)
	// Activate makes s current: the polled set becomes s's file
	// references. Watch registrations are kept.
	Activate(s Store)
	// Current returns the active Store.
	Current() Store
	// Run polls file references until ctx is done; its owner waits for it.
	Run(ctx context.Context) error
}
```

Tests (WP-01): every formatting verb and encoder prints `[REDACTED]`; `Reveal` returns a copy; `NewValue([]byte{})` is set (`IsZero` false) and `NewValue(nil)` unset; `RefOf` from `v1alpha1.SecretRef`.

### 2.9 Validated Bundle and effective chains (hub)

The unified hub: each `Resource` carries the normalized positioned tree (`Tree`), the typed `v1alpha1` object (`Object`) and, for Policies, the typed config (`Config`). `Chain` is the effective Filter Chain computed by `internal/config/precedence` (stage I) and consumed by validation, `render --effective` and the snapshot compiler; `Validated` is the pipeline's output in every binary. `PolicyCheck` and `Checks` fix the offline per-type check contract (R-34): the type packages with offline checks (`filter/header`, `filter/transform/request`, `filter/transform/response`, `filter/validation/jsonschema`) export `func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic))`, `filter/builtin/checks.Table()` returns them as `hub.Checks`, and `config/validate` runs them at stage H without importing `internal/filter`. `SecretUse.Destination` carries the secret-to-destination binding (R-49). `SecretUse.Loc` (wave 2 boundary, additive, mirroring `CELUse.Loc`) is the `secretRef`'s source position, filled by WP-56 at stage H from the tree node and copied by WP-73 into `secret.Use.Loc`, so RZ-CFG-026 diagnostics carry file:line:column; it is zero on canonical re-entry.

`internal/config/hub/hub.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package hub is the validated configuration model: the typed, normalized
// resources of one rendered Bundle, the effective Filter Chain of every
// Route, and the pipeline output a Node compiles and the CLI renders. In
// M1 the hub types are the ruralz/v1alpha1 types; a later apiVersion
// converts into them through internal/config/convert. This package holds
// data only; internal/config/defaults (stage G) and
// internal/config/precedence (stage I) produce it.
package hub

import (
	"cmp"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ID identifies a resource by kind and metadata.name.
type ID = tree.ID

// KindOrder is the canonical order: Gateway, Upstream, Plugin, Policy,
// Consumer, AIProvider, AIModel, Route; unknown kinds sort last.
func KindOrder(k v1alpha1.Kind) int {
	i := slices.Index([]v1alpha1.Kind{
		v1alpha1.KindGateway, v1alpha1.KindUpstream, v1alpha1.KindPlugin,
		v1alpha1.KindPolicy, v1alpha1.KindConsumer, v1alpha1.KindAIProvider, v1alpha1.KindAIModel,
		v1alpha1.KindRoute,
	}, k)
	if i < 0 {
		return 99
	}
	return i
}

// CatalogOrder is the display order of the kind catalog: Gateway, Route,
// Upstream, Policy, Plugin, Consumer, AIProvider, AIModel.
func CatalogOrder(k v1alpha1.Kind) int {
	i := slices.Index([]v1alpha1.Kind{
		v1alpha1.KindGateway, v1alpha1.KindRoute, v1alpha1.KindUpstream,
		v1alpha1.KindPolicy, v1alpha1.KindPlugin, v1alpha1.KindConsumer, v1alpha1.KindAIProvider,
		v1alpha1.KindAIModel,
	}, k)
	if i < 0 {
		return 99
	}
	return i
}

// Source records where a resource came from; it is outside the digest.
type Source struct {
	// File is the base (or overlay-added) file, slash-separated; "" for
	// canonical content.
	File string
	// APIVersion is the apiVersion the resource was authored in.
	APIVersion string
	// Start is the document start.
	Start diag.Location
}

// Resource is one validated resource.
type Resource struct {
	ID
	// Labels are metadata.labels.
	Labels map[string]string
	// Annotations are metadata.annotations minus ruralz.io/conversion-data.
	Annotations map[string]string
	// Tree is the normalized envelope after stage G: defaults materialized
	// (StyleDefaulted), scalars and lists normalized, positions kept. The
	// canonical encoder and the diff engine read it.
	Tree *tree.Node
	// Object is the typed hub view: *v1alpha1.Gateway, *v1alpha1.Route,
	// *v1alpha1.Upstream, *v1alpha1.Policy, *v1alpha1.Consumer, and so on.
	Object any
	// Config is the typed Policy config (such as *v1alpha1.RateLimitConfig)
	// for a Policy; nil otherwise.
	Config any
	// Source is where it came from.
	Source Source
}

// Object returns r's typed view when it has type *T.
func Object[T any](r *Resource) (*T, bool) {
	if r == nil {
		return nil, false
	}
	t, ok := r.Object.(*T)
	return t, ok
}

// Bundle is the set of validated resources, sorted in canonical order
// (KindOrder, then name bytewise). It is immutable after NewBundle.
type Bundle struct {
	resources []*Resource
	byID      map[ID]*Resource
}

// NewBundle sorts rs and indexes them; a duplicate identity keeps the first.
func NewBundle(rs []*Resource) *Bundle {
	out := slices.Clone(rs)
	slices.SortStableFunc(out, func(a, b *Resource) int {
		return cmp.Or(cmp.Compare(KindOrder(a.Kind), KindOrder(b.Kind)), cmp.Compare(a.Name, b.Name))
	})
	b := &Bundle{byID: make(map[ID]*Resource, len(out))}
	for _, r := range out {
		if _, dup := b.byID[r.ID]; !dup {
			b.byID[r.ID] = r
			b.resources = append(b.resources, r)
		}
	}
	return b
}

// Resources returns every resource in canonical order.
func (b *Bundle) Resources() []*Resource { return b.resources }

// Get returns the resource with id.
func (b *Bundle) Get(id ID) (*Resource, bool) {
	r, ok := b.byID[id]
	return r, ok
}

// Gateway returns the Gateway, or nil.
func (b *Bundle) Gateway() *v1alpha1.Gateway {
	for _, r := range b.resources {
		if g, ok := Object[v1alpha1.Gateway](r); ok {
			return g
		}
	}
	return nil
}

// Route returns the named Route.
func (b *Bundle) Route(name string) (*v1alpha1.Route, bool) {
	return typed[v1alpha1.Route](b, v1alpha1.KindRoute, name)
}

// Upstream returns the named Upstream.
func (b *Bundle) Upstream(name string) (*v1alpha1.Upstream, bool) {
	return typed[v1alpha1.Upstream](b, v1alpha1.KindUpstream, name)
}

// Policy returns the named Policy.
func (b *Bundle) Policy(name string) (*v1alpha1.Policy, bool) {
	return typed[v1alpha1.Policy](b, v1alpha1.KindPolicy, name)
}

// Consumer returns the named Consumer.
func (b *Bundle) Consumer(name string) (*v1alpha1.Consumer, bool) {
	return typed[v1alpha1.Consumer](b, v1alpha1.KindConsumer, name)
}

// Plugin returns the named Plugin.
func (b *Bundle) Plugin(name string) (*v1alpha1.Plugin, bool) {
	return typed[v1alpha1.Plugin](b, v1alpha1.KindPlugin, name)
}

// Routes returns every Route in name order.
func (b *Bundle) Routes() []*v1alpha1.Route { return all[v1alpha1.Route](b, v1alpha1.KindRoute) }

// Upstreams returns every Upstream in name order.
func (b *Bundle) Upstreams() []*v1alpha1.Upstream {
	return all[v1alpha1.Upstream](b, v1alpha1.KindUpstream)
}

// Policies returns every Policy in name order.
func (b *Bundle) Policies() []*v1alpha1.Policy { return all[v1alpha1.Policy](b, v1alpha1.KindPolicy) }

// Consumers returns every Consumer in name order.
func (b *Bundle) Consumers() []*v1alpha1.Consumer {
	return all[v1alpha1.Consumer](b, v1alpha1.KindConsumer)
}

func typed[T any](b *Bundle, k v1alpha1.Kind, name string) (*T, bool) {
	r, ok := b.byID[ID{Kind: k, Name: name}]
	if !ok {
		return nil, false
	}
	return Object[T](r)
}

func all[T any](b *Bundle, k v1alpha1.Kind) []*T {
	var out []*T
	for _, r := range b.resources {
		if r.Kind == k {
			if t, ok := Object[T](r); ok {
				out = append(out, t)
			}
		}
	}
	return out
}

// Entry is one Policy in an effective chain.
type Entry struct {
	// Policy is the Policy metadata.name.
	Policy string
	// Type is spec.type.
	Type v1alpha1.PolicyType
	// Class is the Filter class (registry, or filterClass for plugin).
	Class phase.Class
	// Slot is the effective slot.
	Slot string
	// Scope is the attachment scope.
	Scope phase.Scope
	// Position is the zero-based index in that scope's spec.policies.
	Position int
	// FailureMode is the materialized failureMode.
	FailureMode v1alpha1.FailureMode
	// Overridable is the materialized spec.overridable.
	Overridable bool
	// Replaces names the Gateway Policy a Route Policy replaced, or "".
	Replaces string
	// Phases are the Phases the Policy runs in at this attachment.
	Phases phase.Set
	// Ref is the position of the attaching PolicyRef.
	Ref diag.Location
}

// FirstPhase returns the earliest Phase the entry runs in.
func (e Entry) FirstPhase() phase.Phase {
	p, _ := e.Phases.First()
	return p
}

// RemovalReason says why a Gateway Policy left a Route's chain.
type RemovalReason uint8

// Removal reasons.
const (
	// Replaced by a Route Policy in the same slot.
	Replaced RemovalReason = iota + 1
	// Excluded by spec.excludePolicies.
	Excluded
)

// Removed is a Gateway Policy absent from a Route's chain.
type Removed struct {
	// Policy is the removed Policy.
	Policy string
	// Slot is its slot.
	Slot string
	// By is the replacing Route Policy, or "" when excluded.
	By string
	// Reason says why.
	Reason RemovalReason
}

// Leg is the upstream-leg chain of one Upstream a Route reaches.
type Leg struct {
	// Upstream is the Upstream name.
	Upstream string
	// Phases holds, per Phase, the Policies in execution order.
	Phases [phase.Count][]Entry
}

// Chain is the effective Filter Chain of one Route. Each Phase holds its
// Policies in execution order: class, scope, position; reversed for
// response Phases; request order for onLog. The data plane builds its
// per-Phase arrays from this value, so render --effective and runtime
// order cannot diverge.
type Chain struct {
	// Route is the Route name.
	Route string
	// Client holds the client-leg Phases.
	Client [phase.Count][]Entry
	// Legs are the upstream legs in first-appearance order.
	Legs []Leg
	// Removed lists Gateway Policies removed, replaced before excluded.
	Removed []Removed
}

// Leg returns the leg of upstream.
func (c *Chain) Leg(upstream string) (*Leg, bool) {
	for i := range c.Legs {
		if c.Legs[i].Upstream == upstream {
			return &c.Legs[i], true
		}
	}
	return nil, false
}

// SecretUse is one secretRef found at an x-ruralz-secret position.
type SecretUse struct {
	// Ref is the reference.
	Ref v1alpha1.SecretRef
	// Kind selects the Node's check.
	Kind secret.Kind
	// Resource and Path locate the field.
	Resource diag.ResourceID
	// Path is the key-aware path.
	Path diag.Path
	// Loc is the secretRef's source position; zero when unknown, such as
	// canonical re-entry.
	Loc diag.Location
	// Destination is where the Node transmits the resolved value
	// (secret-to-destination binding, OQ-security-and-identity-22 (a)):
	// DestinationLocal for verification material that never leaves the
	// Node (TLS keys, CA bundles, CRLs, API keys), else the remote origin
	// "https://host:port" (auth.upstream-oauth2 clientSecret: its tokenUrl)
	// or "state-store" (stateStore.url, self-describing). One Ref used with
	// two destinations is RZ-CFG-041 at stage H.
	Destination string
}

// DestinationLocal marks a secret that is never transmitted.
const DestinationLocal = "local"

// PolicyCheck is the offline check of one Policy type's config (R-34). It
// runs at stage H in every binary on a Policy resource whose Config is
// decoded, and reports findings with RZ-CFG codes at key-aware paths under
// spec.config; loc maps such a path to its source location (the resource
// start when unknown). It never resolves secrets, compiles CEL (stage J
// does) or opens a connection.
type PolicyCheck func(r *Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic))

// Checks maps a Policy type to its offline check; a type without one is
// absent. Each binary passes internal/filter/builtin/checks.Table() to the
// pipeline, so internal/config never imports internal/filter.
type Checks map[v1alpha1.PolicyType]PolicyCheck

// CELUse is one CEL expression at its site.
type CELUse struct {
	// Site is the place refined by the attachment.
	Site expr.Site
	// Expr is the source text.
	Expr string
	// Resource and Path locate the field.
	Resource diag.ResourceID
	// Path is the key-aware path.
	Path diag.Path
	// Loc is the scalar's source position.
	Loc diag.Location
}

// Validated is the output of the configuration pipeline for one
// Environment: what a Node compiles into a snapshot and what the CLI
// renders, builds and diffs.
type Validated struct {
	// Bundle holds the resources.
	Bundle *Bundle
	// Chains holds the effective chain of every Route, by Route name.
	Chains map[string]*Chain
	// Revision is the canonical content and its digest.
	Revision revision.Revision
	// Secrets lists every secretRef use.
	Secrets []SecretUse
	// CEL lists every expression with its site.
	CEL []CELUse
}
```

Tests (WP-01): `NewBundle` sorts by `KindOrder` then name and indexes by kind; `Chain.Leg` lookup; `Entry.FirstPhase`; a `Checks` table entry is callable with a fake `loc`.

### 2.10 Telemetry catalog

Names, bounds and enumerations only (stdlib), so repocheck, `telemetrygen` and every emitter share one table. The families and constants cover 09 sections 2.5 to 2.9 and 2.16; `ReasonCRLStale` ("crl_stale") is added for `auth.mtls` CRL freshness (06 section 2.6) and is recorded in the observability document by WP-12 (R-37).

`internal/telemetry/catalog/catalog.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package catalog is the Ruralz telemetry name catalog of
// docs/architecture/10-observability.md for M1: metric families with type,
// histogram bounds, unit and labels; degraded reasons; cleartext hops; span
// names and attribute keys; process-log keys. It imports only the standard
// library so repocheck can import it: a metric or span name literal outside
// this package is a finding.
package catalog

import "slices"

// Kind is an instrument type.
type Kind uint8

// Instrument types.
const (
	Counter Kind = iota + 1
	Gauge
	Histogram
)

// Bounds names one of the five histogram bound sets.
type Bounds uint8

// Histogram bound sets (13 bounds each).
const (
	BoundsNone Bounds = iota
	BoundsFast
	BoundsRequest
	BoundsControl
	BoundsBytes
	BoundsRatio
)

// Seconds returns the bounds in base units.
func (b Bounds) Seconds() []float64 {
	switch b {
	case BoundsFast:
		return []float64{0.00001, 0.000025, 0.00005, 0.0001, 0.00015, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.1}
	case BoundsRequest:
		return []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 60}
	case BoundsControl:
		return []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 3600}
	case BoundsBytes:
		return []float64{64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 67108864, 268435456, 1073741824}
	case BoundsRatio:
		return []float64{0.5, 0.75, 0.9, 0.95, 0.98, 0.99, 1, 1.01, 1.02, 1.05, 1.1, 1.25, 1.5}
	default:
		return nil
	}
}

// Class is how a family's label sets are admitted.
type Class uint8

// Admission classes.
const (
	// ClassListener families (listener or enumeration-only) never fold.
	ClassListener Class = iota + 1
	// ClassRoute families fold per Route.
	ClassRoute
	// ClassUpstream families fold per Upstream.
	ClassUpstream
	// ClassPolicy families fold per Policy.
	ClassPolicy
)

// Label is one label of a family.
type Label struct {
	// Name is the label name.
	Name string
	// Values are the enumeration values; nil for resource names and codes.
	Values []string
	// Code is true when values are registered RZ codes.
	Code bool
}

// Family is one metric family.
type Family struct {
	// Name is the exact name, identical in OTLP and /metrics.
	Name string
	// Kind is the instrument type.
	Kind Kind
	// Bounds is the histogram set.
	Bounds Bounds
	// Unit is UCUM.
	Unit string
	// Labels in order.
	Labels []Label
	// Class is the admission class.
	Class Class
	// Striped marks hot label sets.
	Striped bool
}

// Metric family names (M1). Code uses these constants, never literals.
const (
	HTTPRequestsTotal                   = "ruralz_http_requests_total"
	HTTPListenerRequestsTotal           = "ruralz_http_listener_requests_total"
	HTTPNodeResponsesTotal              = "ruralz_http_node_responses_total"
	HTTPRequestDurationSeconds          = "ruralz_http_request_duration_seconds"
	HTTPGatewayDurationSeconds          = "ruralz_http_gateway_duration_seconds"
	HTTPGatewayDurationSkippedTotal     = "ruralz_http_gateway_duration_skipped_total"
	HTTPRequestBodyBytes                = "ruralz_http_request_body_bytes"
	HTTPResponseBodyBytes               = "ruralz_http_response_body_bytes"
	HTTPActiveRequests                  = "ruralz_http_active_requests"
	ListenerOpenConnections             = "ruralz_listener_open_connections"
	ListenerConnectionsTotal            = "ruralz_listener_connections_total"
	ListenerTLSHandshakeDurationSeconds = "ruralz_listener_tls_handshake_duration_seconds"
	FilterDurationSeconds               = "ruralz_filter_duration_seconds"
	FilterShortCircuitsTotal            = "ruralz_filter_short_circuits_total"
	FilterFailuresTotal                 = "ruralz_filter_failures_total"
	AuthDecisionsTotal                  = "ruralz_auth_decisions_total"
	AuthJWKSAgeSeconds                  = "ruralz_auth_jwks_age_seconds"
	AuthUpstreamTokenAgeSeconds         = "ruralz_auth_upstream_token_age_seconds" //nolint:gosec // G101: a metric name, not a credential.
	AuthUpstreamRefreshFailuresTotal    = "ruralz_auth_upstream_refresh_failures_total"
	SecurityCleartextHops               = "ruralz_security_cleartext_hops"
	RateLimitDecisionsTotal             = "ruralz_ratelimit_decisions_total"
	RateLimitBucketEvictionsTotal       = "ruralz_ratelimit_bucket_evictions_total"
	QuotaDecisionsTotal                 = "ruralz_quota_decisions_total"
	CacheRequestsTotal                  = "ruralz_cache_requests_total"
	CacheStoreSkippedTotal              = "ruralz_cache_store_skipped_total"
	UpstreamAttemptsTotal               = "ruralz_upstream_attempts_total"
	UpstreamAttemptDurationSeconds      = "ruralz_upstream_attempt_duration_seconds"
	UpstreamRetriesTotal                = "ruralz_upstream_retries_total"
	UpstreamRetryBudgetExhaustedTotal   = "ruralz_upstream_retry_budget_exhausted_total"
	UpstreamBreakerStateInfo            = "ruralz_upstream_breaker_state_info"
	UpstreamEjectionsTotal              = "ruralz_upstream_ejections_total"
	UpstreamHealthyEndpoints            = "ruralz_upstream_healthy_endpoints"
	UpstreamProbesSkippedTotal          = "ruralz_upstream_probes_skipped_total"
	UpstreamDegradedInfo                = "ruralz_upstream_degraded_info"
	UpstreamCELErrorsTotal              = "ruralz_upstream_cel_errors_total"
	UpstreamPoolConnections             = "ruralz_upstream_pool_connections"
	StateCallDurationSeconds            = "ruralz_state_call_duration_seconds"
	StateCallsTotal                     = "ruralz_state_calls_total"
	StateOpsTotal                       = "ruralz_state_ops_total"
	StateWritesDroppedTotal             = "ruralz_state_writes_dropped_total"
	StateWriteQueueItems                = "ruralz_state_write_queue_items"
	ConfigRevisionInfo                  = "ruralz_config_revision_info"
	ConfigActivationsTotal              = "ruralz_config_activations_total"
	ConfigActivationDurationSeconds     = "ruralz_config_activation_duration_seconds"
	ConfigRetiredSnapshots              = "ruralz_config_retired_snapshots"
	SnapshotRetirementEndedTotal        = "ruralz_snapshot_retirement_ended_total"
	ConfigSecretRotationFailuresTotal   = "ruralz_config_secret_rotation_failures_total" //nolint:gosec // G101: a metric name, not a credential.
	NodeDegradedInfo                    = "ruralz_node_degraded_info"
	NodeBufferedBytes                   = "ruralz_node_buffered_bytes"
	RuntimeGoroutines                   = "ruralz_runtime_goroutines"
	RuntimeHeapBytes                    = "ruralz_runtime_heap_bytes"
	RuntimeGCCyclesTotal                = "ruralz_runtime_gc_cycles_total"
	TelemetrySpansTotal                 = "ruralz_telemetry_spans_total"
	TelemetrySpansDroppedTotal          = "ruralz_telemetry_spans_dropped_total"
	TelemetryTracesUnsampledTotal       = "ruralz_telemetry_traces_unsampled_total"
	TelemetryLogsTotal                  = "ruralz_telemetry_logs_total"
	TelemetryLogsDroppedTotal           = "ruralz_telemetry_logs_dropped_total"
	TelemetryFoldedLabelSets            = "ruralz_telemetry_folded_label_sets"
	TelemetrySeries                     = "ruralz_telemetry_series"
	TapEventsDroppedTotal               = "ruralz_tap_events_dropped_total"
)

// Enumerations used as label values.
func statusClasses() []string { return []string{"1xx", "2xx", "3xx", "4xx", "5xx"} }

// Ops returns the State Store op values (M1 subset first).
func Ops() []string {
	return []string{"gcra", "quota", "cache_get", "script_multi", "pipeline", "cache_set", "cache_invalidate", "refund"}
}

// WriteKinds returns the dropped-write kinds of M1.
func WriteKinds() []string { return []string{"refund", "cache_set", "cache_invalidate"} }

// Families returns the M1 metric families (docs/architecture/10-observability.md
// "Ruralz Gateway metrics"; spec 09 requirement 43).
func Families() []Family {
	route := Label{Name: "route"}
	up := Label{Name: "upstream"}
	pol := Label{Name: "policy"}
	lis := Label{Name: "listener"}
	proto := Label{Name: "protocol", Values: []string{"http1", "http2"}}
	sc := Label{Name: "status_class", Values: statusClasses()}
	ph := Label{Name: "phase", Values: []string{
		"onRequestHeaders", "onRequestBody", "onRoute", "onUpstreamRequest",
		"onUpstreamResponseHeaders", "onUpstreamResponseBody", "onResponse", "onLog", "onChunk",
	}}
	code := Label{Name: "code", Code: true}
	return []Family{
		{HTTPRequestsTotal, Counter, BoundsNone, "{requests}", []Label{route, sc}, ClassRoute, true},
		{HTTPListenerRequestsTotal, Counter, BoundsNone, "{requests}", []Label{lis, proto, sc, {Name: "origin", Values: []string{"upstream", "node", "dependency"}}}, ClassListener, true},
		{HTTPNodeResponsesTotal, Counter, BoundsNone, "{responses}", []Label{code}, ClassListener, false},
		{HTTPRequestDurationSeconds, Histogram, BoundsRequest, "s", []Label{route}, ClassRoute, true},
		{HTTPGatewayDurationSeconds, Histogram, BoundsFast, "s", []Label{lis}, ClassListener, true},
		{HTTPGatewayDurationSkippedTotal, Counter, BoundsNone, "{requests}", []Label{{Name: "reason", Values: []string{"clock_anomaly"}}}, ClassListener, false},
		{HTTPRequestBodyBytes, Histogram, BoundsBytes, "By", []Label{lis}, ClassListener, true},
		{HTTPResponseBodyBytes, Histogram, BoundsBytes, "By", []Label{lis}, ClassListener, true},
		{HTTPActiveRequests, Gauge, BoundsNone, "{requests}", []Label{lis}, ClassListener, true},
		{ListenerOpenConnections, Gauge, BoundsNone, "{connections}", []Label{lis, proto}, ClassListener, false},
		{ListenerConnectionsTotal, Counter, BoundsNone, "{connections}", []Label{lis, proto, {Name: "result", Values: []string{"accepted", "tls_failure", "refused"}}}, ClassListener, false},
		{ListenerTLSHandshakeDurationSeconds, Histogram, BoundsRequest, "s", []Label{lis}, ClassListener, false},
		{FilterDurationSeconds, Histogram, BoundsFast, "s", []Label{pol, ph}, ClassPolicy, true},
		{FilterShortCircuitsTotal, Counter, BoundsNone, "{responses}", []Label{pol, ph, sc}, ClassPolicy, true},
		{FilterFailuresTotal, Counter, BoundsNone, "{failures}", []Label{pol, ph, {Name: "mode", Values: []string{"open", "closed"}}}, ClassPolicy, false},
		{AuthDecisionsTotal, Counter, BoundsNone, "{decisions}", []Label{pol, {Name: "result", Values: []string{"allow", "deny"}}, code}, ClassPolicy, true},
		{AuthJWKSAgeSeconds, Gauge, BoundsNone, "s", []Label{pol}, ClassPolicy, false},
		{AuthUpstreamTokenAgeSeconds, Gauge, BoundsNone, "s", []Label{pol}, ClassPolicy, false},
		{AuthUpstreamRefreshFailuresTotal, Counter, BoundsNone, "{failures}", []Label{pol}, ClassPolicy, false},
		{SecurityCleartextHops, Gauge, BoundsNone, "{hops}", []Label{{Name: "hop", Values: HopNames()}}, ClassListener, false},
		{RateLimitDecisionsTotal, Counter, BoundsNone, "{decisions}", []Label{pol, {Name: "result", Values: []string{"allow", "deny_local", "deny_global", "fail_open"}}}, ClassPolicy, true},
		{RateLimitBucketEvictionsTotal, Counter, BoundsNone, "{entries}", []Label{pol}, ClassPolicy, false},
		{QuotaDecisionsTotal, Counter, BoundsNone, "{decisions}", []Label{pol, {Name: "result", Values: []string{"allow", "deny", "no_quota", "fail_open"}}}, ClassPolicy, true},
		{CacheRequestsTotal, Counter, BoundsNone, "{requests}", []Label{route, {Name: "result", Values: []string{"hit", "miss", "bypass", "stale", "stale_error"}}}, ClassRoute, false},
		{CacheStoreSkippedTotal, Counter, BoundsNone, "{stores}", []Label{pol, {Name: "reason", Values: []string{"size_limit", "buffer_budget", "memory"}}}, ClassPolicy, false},
		{UpstreamAttemptsTotal, Counter, BoundsNone, "{attempts}", []Label{up, sc, {Name: "error", Values: []string{"none", "connect", "timeout", "reset", "tls"}}}, ClassUpstream, true},
		{UpstreamAttemptDurationSeconds, Histogram, BoundsRequest, "s", []Label{up}, ClassUpstream, true},
		{UpstreamRetriesTotal, Counter, BoundsNone, "{retries}", []Label{up}, ClassUpstream, false},
		{UpstreamRetryBudgetExhaustedTotal, Counter, BoundsNone, "{retries}", []Label{up}, ClassUpstream, false},
		{UpstreamBreakerStateInfo, Gauge, BoundsNone, "1", []Label{up, {Name: "state", Values: []string{"closed", "open", "half_open"}}}, ClassUpstream, false},
		{UpstreamEjectionsTotal, Counter, BoundsNone, "{ejections}", []Label{up, {Name: "reason", Values: []string{"passive", "active"}}}, ClassUpstream, false},
		{UpstreamHealthyEndpoints, Gauge, BoundsNone, "{endpoints}", []Label{up}, ClassUpstream, false},
		{UpstreamProbesSkippedTotal, Counter, BoundsNone, "{probes}", []Label{up}, ClassUpstream, false},
		{UpstreamDegradedInfo, Gauge, BoundsNone, "1", []Label{up, {Name: "reason", Values: []string{"panic", "discovery_stale", "balancer_budget"}}}, ClassUpstream, false},
		{UpstreamCELErrorsTotal, Counter, BoundsNone, "{errors}", []Label{up, {Name: "field", Values: []string{"hashKey", "retryOn", "failureWhen"}}}, ClassUpstream, false},
		{UpstreamPoolConnections, Gauge, BoundsNone, "{connections}", []Label{up, {Name: "state", Values: []string{"idle", "active"}}}, ClassUpstream, false},
		{StateCallDurationSeconds, Histogram, BoundsFast, "s", []Label{{Name: "op", Values: Ops()}}, ClassListener, true},
		{StateCallsTotal, Counter, BoundsNone, "{calls}", []Label{{Name: "op", Values: Ops()}, {Name: "result", Values: []string{"ok", "error", "timeout", "skipped"}}}, ClassListener, true},
		{StateOpsTotal, Counter, BoundsNone, "{operations}", []Label{{Name: "kind", Values: Ops()}}, ClassListener, true},
		{StateWritesDroppedTotal, Counter, BoundsNone, "{writes}", []Label{{Name: "kind", Values: WriteKinds()}}, ClassListener, false},
		{StateWriteQueueItems, Gauge, BoundsNone, "{items}", nil, ClassListener, false},
		{ConfigRevisionInfo, Gauge, BoundsNone, "1", []Label{{Name: "revision"}, {Name: "role", Values: []string{"active", "lkg"}}}, ClassListener, false},
		{ConfigActivationsTotal, Counter, BoundsNone, "{activations}", []Label{{Name: "result", Values: []string{"activated", "rejected"}}, code}, ClassListener, false},
		{ConfigActivationDurationSeconds, Histogram, BoundsControl, "s", []Label{{Name: "stage", Values: []string{"verify", "plugin_compile", "compile", "swap", "total"}}, {Name: "size_class", Values: []string{"le1000", "le10000", "gt10000"}}}, ClassListener, false},
		{ConfigRetiredSnapshots, Gauge, BoundsNone, "{snapshots}", nil, ClassListener, false},
		{SnapshotRetirementEndedTotal, Counter, BoundsNone, "{requests}", nil, ClassListener, false},
		{ConfigSecretRotationFailuresTotal, Counter, BoundsNone, "{failures}", []Label{{Name: "provider", Values: []string{"env", "file", "kubernetes", "vault"}}}, ClassListener, false},
		{NodeDegradedInfo, Gauge, BoundsNone, "1", []Label{{Name: "reason", Values: ReasonNames()}}, ClassListener, false},
		{NodeBufferedBytes, Gauge, BoundsNone, "By", nil, ClassListener, false},
		{RuntimeGoroutines, Gauge, BoundsNone, "{goroutines}", nil, ClassListener, false},
		{RuntimeHeapBytes, Gauge, BoundsNone, "By", nil, ClassListener, false},
		{RuntimeGCCyclesTotal, Counter, BoundsNone, "{cycles}", nil, ClassListener, false},
		{TelemetrySpansTotal, Counter, BoundsNone, "{spans}", nil, ClassListener, false},
		{TelemetrySpansDroppedTotal, Counter, BoundsNone, "{spans}", []Label{{Name: "reason", Values: []string{"queue_full", "export_error"}}}, ClassListener, false},
		{TelemetryTracesUnsampledTotal, Counter, BoundsNone, "{traces}", []Label{{Name: "reason", Values: []string{"rate_cap_root", "rate_cap_parent"}}}, ClassListener, false},
		{TelemetryLogsTotal, Counter, BoundsNone, "{records}", []Label{{Name: "stream", Values: []string{"access", "process"}}}, ClassListener, false},
		{TelemetryLogsDroppedTotal, Counter, BoundsNone, "{records}", []Label{{Name: "stream", Values: []string{"access", "process"}}, {Name: "reason", Values: []string{"queue_full", "export_error"}}}, ClassListener, false},
		{TelemetryFoldedLabelSets, Gauge, BoundsNone, "{label_sets}", []Label{{Name: "instrument"}}, ClassListener, false},
		{TelemetrySeries, Gauge, BoundsNone, "{series}", []Label{{Name: "state", Values: []string{"live", "retiring"}}}, ClassListener, false},
		{TapEventsDroppedTotal, Counter, BoundsNone, "{events}", nil, ClassListener, false},
	}
}

// Lookup returns the family named name.
func Lookup(name string) (Family, bool) {
	fs := Families()
	i := slices.IndexFunc(fs, func(f Family) bool { return f.Name == name })
	if i < 0 {
		return Family{}, false
	}
	return fs[i], true
}

// Reason is a ruralz_node_degraded_info reason.
type Reason uint8

// M1 degraded reasons, all exported at 0 from process start.
const (
	ReasonLKGBoot Reason = iota + 1
	ReasonLKGWriteFailed
	ReasonStateStoreMemoryFallback
	ReasonStateStoreUnauthenticated
	ReasonStateStoreBreakerOpen
	ReasonStateStoreEvictionPolicy
	ReasonUpstreamPanic
	ReasonDiscoveryStale
	ReasonBalancerBudget
	ReasonProbesSkipped
	ReasonHeaderLimitCapped
	ReasonSnapshotEndingOverdue
	ReasonSecretRotationFailed
	ReasonJWKSStale
	ReasonCleartextHop
	ReasonTelemetryExportFailing
	ReasonCRLStale
	NumReasons
)

// String returns the label value.
func (r Reason) String() string {
	if r == 0 || r >= NumReasons {
		return ""
	}
	return ReasonNames()[r-1]
}

// ReasonNames returns the label values in Reason order.
func ReasonNames() []string {
	return []string{
		"lkg_boot", "lkg_write_failed", "state_store_memory_fallback", "state_store_unauthenticated",
		"state_store_breaker_open", "state_store_eviction_policy", "upstream_panic", "discovery_stale",
		"balancer_budget", "probes_skipped", "header_limit_capped", "snapshot_ending_overdue",
		"secret_rotation_failed", "jwks_stale", "cleartext_hop", "telemetry_export_failing", "crl_stale",
	}
}

// Hop is a cleartext hop kind.
type Hop uint8

// Hops.
const (
	HopClient Hop = iota
	HopUpstream
	HopStateStore
	HopTelemetry
	HopAdmin
	NumHops
)

// HopNames returns the label values in Hop order.
func HopNames() []string { return []string{"client", "upstream", "state_store", "telemetry", "admin"} }

// String returns the label value.
func (h Hop) String() string {
	if h >= NumHops {
		return ""
	}
	return HopNames()[h]
}

// Span names and prefixes (foundation pack section 2).
const (
	SpanRouteMatch     = "ruralz.route.match"
	SpanFilterPrefix   = "ruralz.filter."
	SpanUpstreamPrefix = "ruralz.upstream."
)

// FilterSpanName returns "ruralz.filter.<policy>"; call it at snapshot
// compile time only.
func FilterSpanName(policy string) string { return SpanFilterPrefix + policy }

// UpstreamSpanName returns "ruralz.upstream.<upstream>"; compile time only.
func UpstreamSpanName(upstream string) string { return SpanUpstreamPrefix + upstream }

// Span attribute keys.
const (
	AttrRoute         = "ruralz.route"
	AttrListener      = "ruralz.listener"
	AttrRevision      = "ruralz.revision"
	AttrConsumer      = "ruralz.consumer"
	AttrTier          = "ruralz.tier"
	AttrCode          = "ruralz.code"
	AttrPolicyType    = "ruralz.policy.type"
	AttrPhase         = "ruralz.phase"
	AttrOutcome       = "ruralz.outcome"
	AttrFailureMode   = "ruralz.failure_mode"
	AttrStateOp       = "ruralz.state.op"
	AttrStateDuration = "ruralz.state.duration"
	AttrStateBatch    = "ruralz.state.batch"
	AttrAttempt       = "ruralz.upstream.attempt"
	AttrCompStep      = "ruralz.composition.step"
)

// Process log keys (snake_case; sloglint no-raw-keys).
const (
	KeyComponent    = "component"
	KeyNodeID       = "node_id"
	KeyRevision     = "revision"
	KeyTraceID      = "trace_id"
	KeySpanID       = "span_id"
	KeyCode         = "code"
	KeyError        = "error"
	KeyFile         = "file"
	KeyLine         = "line"
	KeyColumn       = "column"
	KeyResourceKind = "resource_kind"
	KeyResourceName = "resource_name"
	KeyPath         = "path"
	KeyProvider     = "provider"
	KeyReference    = "reference"
	KeyShard        = "shard"
	KeyPolicy       = "policy"
	KeyUpstream     = "upstream"
	KeyListener     = "listener"
	KeyReason       = "reason"
	KeyPhase        = "phase"
	KeyPanicType    = "panic_type"
	KeyStack        = "stack"
	KeyErrorType    = "error_type"
	KeyMetric       = "metric" // a metric family name
)
```

Tests (WP-01): names match `^ruralz_[a-z0-9_]+$` with unit suffixes; labels per family unique; every `Reason` and `Hop` has a name; `Lookup` of every family; process log keys (including `KeyPhase`, `KeyPanicType`, `KeyStack`, `KeyErrorType` and `KeyMetric`, added at the wave 2 boundary for the executor's panic and undecided logs, `upstream_panic` logs and the dropped-code DEBUG record) are snake_case, unique and never a slog built-in key.

### 2.11 Telemetry handles

What request-path code calls. Handles are bound once per snapshot (`Meter.Bind`) so the hot path indexes arrays and never looks up label sets. `internal/telemetry/aggregate` implements `Meter` and the metric interfaces, `internal/telemetry/tracing` implements `Tracer` and `Span`, `internal/telemetry/accesslog` implements `AccessLog`, and the telemetry `Runtime` implements `NodeStatus`.

`internal/telemetry/emit/emit.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package emit is the telemetry contract every area records through:
// metric handles resolved at snapshot compile time, the tracer, the access
// log, degraded states and cleartext hops. internal/telemetry implements
// it on Ruralz-owned aggregates and the OpenTelemetry SDK; no other
// package imports OpenTelemetry. Recording methods take no locks, allocate
// nothing and never block; loggers are *slog.Logger values from
// internal/telemetry (tests pass slog.New(slog.DiscardHandler)). It also
// provides GatewayTimer, the excluded-section clock of gateway-added time,
// and OriginOf, the origin classification of listener requests, so the
// recording sites need no other telemetry package.
package emit

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Stripe selects a counter stripe (min(GOMAXPROCS, 8) stripes); it is
// assigned per connection at accept and offset per request.
type Stripe uint8

// Counter is a monotonic counter handle.
type Counter interface{ Add(s Stripe, n uint64) }

// Gauge is a gauge handle.
type Gauge interface {
	Add(s Stripe, d int64)
	Set(v int64)
}

// Histogram records integers: nanoseconds for duration sets, bytes for the
// bytes set, millionths for the ratio set.
type Histogram interface {
	Record(s Stripe, v uint64)
	// RecordExemplar is called only for sampled requests.
	RecordExemplar(s Stripe, v uint64, ex Exemplar)
}

// StatusCounter counts by status_class derived from an HTTP status.
type StatusCounter interface{ Inc(s Stripe, status int) }

// CodeCounter counts by a registered RZ code label, created on first use.
type CodeCounter interface{ Inc(s Stripe, code string) }

// Exemplar links a histogram observation to a sampled trace.
type Exemplar struct {
	TraceID [16]byte
	SpanID  [8]byte
	Time    time.Time
}

// Label value indexes. Each enumerated label of catalog.Families is an
// index into the handle arrays below; Num* is the array length.

// Protocols: http1, http2.
const (
	ProtoHTTP1 = iota
	ProtoHTTP2
	NumProtocols
)

// Origins: upstream, node, dependency.
const (
	OriginUpstream = iota
	OriginNode
	OriginDependency
	NumOrigins
)

// OriginOf returns the origin label index of a response for
// ruralz_http_listener_requests_total (spec 09 req 44): OriginUpstream
// when the status was passed through from an Upstream response;
// OriginDependency for a Node-generated response with an RZ-UP-* code or
// RZ-AI-004, RZ-AI-005 or RZ-AI-013 (the Upstream or provider could not
// answer); OriginNode for every other Node-generated response, RZ-STS
// codes and responses without a code included.
func OriginOf(code string, fromUpstream bool) int {
	switch {
	case fromUpstream:
		return OriginUpstream
	case strings.HasPrefix(code, "RZ-UP-"):
		return OriginDependency
	}
	switch code {
	case "RZ-AI-004", "RZ-AI-005", "RZ-AI-013":
		return OriginDependency
	default:
		return OriginNode
	}
}

// Connection results: accepted, tls_failure, refused.
const (
	ConnAccepted = iota
	ConnTLSFailure
	ConnRefused
	NumConnResults
)

// Cache results: hit, miss, bypass, stale, stale_error.
const (
	CacheHit = iota
	CacheMiss
	CacheBypass
	CacheStale
	CacheStaleError
	NumCacheResults
)

// Rate limit results: allow, deny_local, deny_global, fail_open.
const (
	RateLimitAllow = iota
	RateLimitDenyLocal
	RateLimitDenyGlobal
	RateLimitFailOpen
	NumRateLimitResults
)

// Quota results: allow, deny, no_quota, fail_open.
const (
	QuotaAllow = iota
	QuotaDeny
	QuotaNoQuota
	QuotaFailOpen
	NumQuotaResults
)

// Store skip reasons: size_limit, buffer_budget, memory.
const (
	SkipSizeLimit = iota
	SkipBufferBudget
	SkipMemory
	NumSkipReasons
)

// Failure modes: open, closed.
const (
	ModeOpen = iota
	ModeClosed
	NumModes
)

// Attempt errors: none, connect, timeout, reset, tls.
const (
	ErrNone = iota
	ErrConnect
	ErrTimeout
	ErrReset
	ErrTLS
	NumAttemptErrors
)

// Breaker states: closed, open, half_open.
const (
	BreakerClosed = iota
	BreakerOpen
	BreakerHalfOpen
	NumBreakerStates
)

// Ejection reasons: passive, active.
const (
	EjectPassive = iota
	EjectActive
	NumEjectReasons
)

// Upstream degraded reasons: panic, discovery_stale, balancer_budget.
const (
	UpDegradedPanic = iota
	UpDegradedDiscoveryStale
	UpDegradedBalancerBudget
	NumUpDegraded
)

// Upstream CEL fields: hashKey, retryOn, failureWhen.
const (
	CELHashKey = iota
	CELRetryOn
	CELFailureWhen
	NumCELFields
)

// Pool states: idle, active.
const (
	PoolIdle = iota
	PoolActive
	NumPoolStates
)

// State Store op labels, in catalog.Ops() order: gcra, quota, cache_get,
// script_multi, pipeline, cache_set, cache_invalidate, refund. They index
// StateMetrics; statestore.OpKind.Label and RoundTrip.Label map onto them.
const (
	StateOpGCRA = iota
	StateOpQuota
	StateOpCacheGet
	StateOpScriptMulti
	StateOpPipeline
	StateOpCacheSet
	StateOpCacheInvalidate
	StateOpRefund
	NumStateOps
)

// State Store call results: ok, error, timeout, skipped (statestore.Result
// values are these indexes).
const (
	StateResultOK = iota
	StateResultError
	StateResultTimeout
	StateResultSkipped
	NumStateResults
)

// Post-commit write kinds, in catalog.WriteKinds() order: refund,
// cache_set, cache_invalidate.
const (
	WriteRefund = iota
	WriteCacheSet
	WriteCacheInvalidate
	NumWriteKinds
)

// ListenerRequests counts ruralz_http_listener_requests_total.
type ListenerRequests interface {
	Inc(s Stripe, protocol int, status int, origin int)
}

// ListenerMetrics are one listener's handles.
type ListenerMetrics struct {
	Requests        ListenerRequests
	GatewayDuration Histogram
	RequestBody     Histogram
	ResponseBody    Histogram
	Active          Gauge
	OpenConns       [NumProtocols]Gauge
	Conns           [NumProtocols][NumConnResults]Counter
	TLSHandshake    Histogram
}

// RouteMetrics are one Route's handles (folded Routes share _overflow).
type RouteMetrics struct {
	Requests StatusCounter
	Duration Histogram
	Cache    [NumCacheResults]Counter
}

// AuthDecisions counts ruralz_auth_decisions_total: allow with no code,
// deny with the RZ code.
type AuthDecisions interface {
	Allow(s Stripe)
	Deny(s Stripe, code string)
}

// PolicyMetrics are one Policy's handles. Families a Policy type never
// records hold no-op handles.
type PolicyMetrics struct {
	Duration          [phase.Count]Histogram
	ShortCircuits     [phase.Count]StatusCounter
	Failures          [phase.Count][NumModes]Counter
	Auth              AuthDecisions
	JWKSAge           Gauge
	UpstreamTokenAge  Gauge
	UpstreamRefreshes Counter
	RateLimit         [NumRateLimitResults]Counter
	BucketEvictions   Counter
	Quota             [NumQuotaResults]Counter
	StoreSkipped      [NumSkipReasons]Counter
}

// UpstreamAttempts counts ruralz_upstream_attempts_total.
type UpstreamAttempts interface {
	Inc(s Stripe, status int, attemptErr int)
}

// UpstreamMetrics are one Upstream's handles.
type UpstreamMetrics struct {
	Attempts             UpstreamAttempts
	AttemptDuration      Histogram
	Retries              Counter
	RetryBudgetExhausted Counter
	BreakerState         [NumBreakerStates]Gauge
	Ejections            [NumEjectReasons]Counter
	HealthyEndpoints     Gauge
	ProbesSkipped        Counter
	Degraded             [NumUpDegraded]Gauge
	CELErrors            [NumCELFields]Counter
	PoolConnections      [NumPoolStates]Gauge
}

// StateMetrics are the State Store handles, indexed by the StateOp*,
// StateResult* and Write* constants (never by statestore.OpKind values).
type StateMetrics struct {
	CallDuration  [NumStateOps]Histogram
	Calls         [NumStateOps][NumStateResults]Counter
	Ops           [NumStateOps]Counter // by kind (single-operation ops)
	WritesDropped [NumWriteKinds]Counter
	QueueItems    Gauge
}

// ConfigMetrics are the configuration and snapshot handles.
type ConfigMetrics struct {
	// Activations counts {result, code}: Activated(code "") or Rejected(code).
	Activated          Counter
	Rejected           CodeCounter
	ActivationDuration func(stage string, sizeClass string) Histogram
	RetiredSnapshots   Gauge
	RetirementEnded    Counter
	// RevisionInfo sets the at most two ruralz_config_revision_info series.
	RevisionInfo       func(role string, display string)
	SecretRotationFail func(provider string) Counter
}

// NodeMetrics are the Node-wide handles, available before any Revision.
type NodeMetrics struct {
	NodeResponses          CodeCounter
	GatewayDurationSkipped Counter
	BufferedBytes          Gauge
	TapEventsDropped       Counter
	State                  StateMetrics
	Config                 ConfigMetrics
}

// Shape is what admission needs from a compiled Revision.
type Shape struct {
	Listeners []string
	Routes    []string
	Upstreams []string
	Policies  []PolicyShape
	// CachedRoutes are the names (a subset of Routes) of the Routes whose
	// effective chain holds a cache Policy at any scope. Only these get
	// ruralz_cache_requests_total label sets; every other Route's
	// RouteMetrics.Cache holds no-op handles.
	CachedRoutes []string
}

// PolicyShape describes one Policy for admission.
type PolicyShape struct {
	Name    string
	Type    string
	Scopes  phase.ScopeSet
	Phases  phase.Set
	Codes   []string
	Gateway bool // attached at Gateway scope: its hot label sets are striped
}

// Plan is the admission result of one Revision: handles for every
// resource, folded ones pointing at _overflow. Never nil handles.
type Plan interface {
	Listener(name string) *ListenerMetrics
	Route(name string) *RouteMetrics
	Upstream(name string) *UpstreamMetrics
	Policy(name string) *PolicyMetrics
}

// Binding tracks which label sets a snapshot references.
type Binding interface {
	// Retire marks the snapshot retired or closing.
	Retire()
	// Release ends label sets no live snapshot references.
	Release()
}

// Meter admits Revisions and exposes Node-wide handles.
type Meter interface {
	// Admit plans handles off the request path, deterministically.
	Admit(s Shape) (Plan, error)
	// Bind is called at the swap.
	Bind(p Plan) Binding
	// Node returns the Node-wide handles.
	Node() *NodeMetrics
	// NewStripe returns the stripe of a new connection (round robin).
	NewStripe() Stripe
}

// NodeStatus sets degraded reasons and cleartext hops.
type NodeStatus interface {
	// SetDegraded raises or clears reason for source; the gauge is 1 while
	// any source holds it.
	SetDegraded(r catalog.Reason, source string, on bool)
	// SetCleartextHops sets the count of cleartext hops of one kind.
	SetCleartextHops(h catalog.Hop, n int)
}

// Decision is the one sampling decision of a request.
type Decision struct {
	TraceID      [16]byte
	ServerSpanID [8]byte
	ParentSpanID [8]byte
	Remote       bool
	Sampled      bool
	// TraceState is the validated raw tracestate, re-injected unchanged.
	TraceState string
}

// Outcome values of ruralz.outcome.
const (
	OutcomeContinue     = "continue"
	OutcomeRespond      = "respond"
	OutcomeCannotDecide = "cannot_decide"
	OutcomeSkipped      = "skipped"
)

// Span is a started span; the no-op span of an unsampled request costs
// nothing. End must be called exactly once.
type Span interface {
	SetAttr(key string, v slog.Value)
	SpanID() [8]byte
	End(status int, code, errorType string)
}

// ServerAttrs are the SERVER span attributes known at start.
type ServerAttrs struct {
	Listener, Scheme, Host, Path, ClientAddress, UserAgent, Protocol string
	Port                                                             int
}

// FilterAttrs are the attributes of ruralz.filter.<name>.
type FilterAttrs struct {
	PolicyType string
	Phase      phase.Phase
}

// UpstreamAttrs are the attributes of ruralz.upstream.<name>.
type UpstreamAttrs struct {
	Attempt  int
	Endpoint string
	Step     string
}

// Tracer makes the per-request trace decision and starts spans. Span names
// are precomputed at compile time (catalog.FilterSpanName).
type Tracer interface {
	// Decide extracts traceparent and tracestate, applies the ratio and the
	// root and parent caps, and fills d. It allocates nothing except the
	// combined value when a request carries several tracestate field lines
	// that are valid together: one allocation, since RFC 9110 requires the
	// lines to be combined and Decision.TraceState is a single string.
	// Invalid client input allocates nothing.
	Decide(h http.Header, ratio float64, d *Decision)
	StartServer(ctx context.Context, d *Decision, method string, a ServerAttrs) (context.Context, Span)
	StartRouteMatch(ctx context.Context) (context.Context, Span)
	StartFilter(ctx context.Context, spanName string, a FilterAttrs) (context.Context, Span)
	StartUpstream(ctx context.Context, spanName string, a UpstreamAttrs) (context.Context, Span)
	// Inject replaces client traceparent and tracestate on an outgoing leg.
	Inject(ctx context.Context, d *Decision, h http.Header)
}

// Excluder marks sections excluded from gateway-added time (client I/O,
// upstream I/O, State Store round trips, declared remote calls). Calls
// nest; it is safe from parallel legs.
type Excluder interface {
	Enter(now time.Time)
	Leave(now time.Time)
}

// AccessRecord is one access log record, filled on the request goroutine
// in onLog. Strings must be immutable values (net/http request strings are),
// never views of pooled buffers; the writer truncates and encodes.
type AccessRecord struct {
	Start                                time.Time
	TraceID                              [16]byte
	SpanID                               [8]byte
	Sampled                              bool
	Revision                             string
	Listener, Protocol, Route            string
	Method, Host, Path                   string
	Status                               int
	Code                                 string
	Duration                             time.Duration
	RequestBytes, ResponseBytes          int64
	GatewayDuration                      time.Duration
	GatewayDurationSkipped               bool
	UpstreamDuration, StateStoreDuration time.Duration
	ClientAddress, UserAgent, TLSVersion string
	Consumer, Tier, AuthMethod           string
	Upstream, Endpoint                   string
	Attempts                             int
	ShortCircuitPolicy                   string
	ShortCircuitPhase                    phase.Phase
	FailureModes                         []FailureModeEntry
	Cache                                string
}

// FailureModeEntry is one undecided Policy (at most 8 per record).
type FailureModeEntry struct {
	Policy string
	Phase  phase.Phase
	Mode   string
}

// AccessLog is the bounded, non-blocking access log writer.
type AccessLog interface {
	// Acquire returns a pooled, reset record.
	Acquire() *AccessRecord
	// Submit enqueues r (dropping with a counter when full) and returns it
	// to the pool after encoding; the caller must not touch r afterwards.
	Submit(r *AccessRecord)
}
```

Tests (WP-01): label index constants match the catalog label value order, including `StateOp*` against `catalog.Ops()`, `StateResult*` against the result enumeration and `Write*` against `catalog.WriteKinds()`.

Wave 2 boundary additions to `emit` (all additive). `OriginOf` (09 req 44, listed above) and `GatewayTimer` moved verbatim from `internal/telemetry/aggregate`, so `gateway/exchange` and `gateway/handler` reach them without importing the OpenTelemetry-backed aggregate; this supersedes 09's placement of the timer in `internal/telemetry`. `internal/telemetry/emit/timer.go` imports only `sync/atomic` and `time` (the `contracts` depguard rule holds); its exported surface:

```go
// GatewayTimer measures gateway-added time (spec 09 req 53 and 54): wall
// clock time minus the union of excluded sections (client reads and
// writes, upstream I/O, State Store round trips, declared remote calls).
// One word packs the excluded depth and the start of the current excluded
// run and is updated by compare and swap, so sections nest and parallel
// legs overlap safely; the zero value is ready after Reset. It implements
// Excluder and is meant to live in a pooled per-request structure.
type GatewayTimer struct{ /* atomic state, unexported */ }

var _ Excluder = (*GatewayTimer)(nil)

// Reset starts a new measurement at start; call it before any Enter.
func (g *GatewayTimer) Reset(start time.Time)

// Enter opens an excluded section at now; the outermost one starts a run.
func (g *GatewayTimer) Enter(now time.Time)

// Leave closes an excluded section at now; the outermost one adds its run
// to the excluded total. A leave at depth 0, or a run that would end
// before it started, marks the measurement as a clock anomaly.
func (g *GatewayTimer) Leave(now time.Time)

// Result returns the gateway-added time of a request that ended at end,
// read once after its sections closed; a section still open counts as
// excluded up to end. It returns false for a clock anomaly (a result
// below 0 or above wall-clock time, or a leave at depth 0): the caller
// counts ruralz_http_gateway_duration_skipped_total{reason="clock_anomaly"}
// instead of recording.
func (g *GatewayTimer) Result(end time.Time) (time.Duration, bool)

// Observe records the result into h on stripe s, or counts skipped when
// it is a clock anomaly.
func (g *GatewayTimer) Observe(end time.Time, s Stripe, h Histogram, skipped Counter) (time.Duration, bool)
```

WP-43 embeds one `GatewayTimer` in the pooled exchange; WP-66 calls `Reset` at request start and `Observe(end, stripe, ListenerMetrics.GatewayDuration, Node().GatewayDurationSkipped)` at the end, and records `ListenerMetrics.Requests.Inc(stripe, protocol, status, emit.OriginOf(code, fromUpstream))` with no copy of the origin rule. `Shape.CachedRoutes`: 09 req 45 pre-creates `ruralz_cache_requests_total` label sets only for Routes whose effective chain holds a `cache` Policy, so 5,000 uncached Routes no longer use up the family's 6,000-set budget; `telemetry/aggregate` admits the cache family only for `CachedRoutes` and rejects a name that is not in `Routes` with `ErrShape`, and WP-70 fills it from each Route's effective chains (`hub.Chain`) when it builds the metric `Plan`.

### 2.12 State Store

Driver-neutral API (08 sections 2.1 to 2.4, 2.8). One consumptive `Call` per admission Policy; `Store.Consume` executes a batch in the fewest round trips the topology allows (pack 8.7 rule 3) and never retries. `RequestBudget` starts at the first blocking call of the request (resolution R-12) and carries the request's metric stripe, so drivers record `emit.StateMetrics` on it. `Role` distinguishes the main store and the Response Cache connection (OQ-scalability-and-distributed-state-3 (a)). `OpKind` values are not label indexes: `OpKind.Label`, `OpKind.WriteLabel` and `RoundTrip.Label` map them onto the `emit.StateOp*` and `emit.Write*` indexes (R-56). `Deps.MACKey` is filled from `RURALZ_STATE_STORE_MAC_KEY_FILE` (R-49).

`internal/statestore/statestore.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package statestore is the driver-neutral State Store contract
// (docs/architecture/11-scalability-and-distributed-state.md, foundation
// pack section 8.7): typed operations, the Store every stateful Filter
// calls, the per-request deadline, the five RZ-STS failures and the
// post-commit queue interface. Drivers live in internal/statestore/memory
// and internal/statestore/redis (the only rueidis importer); the Manager
// that owns them across Hot Reloads lives in internal/statestore/manager.
package statestore

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// DriverKind selects the implementation (pack section 7: memory and redis).
type DriverKind uint8

// Drivers.
const (
	DriverMemory DriverKind = iota + 1
	DriverRedis
)

// Topology is the redis deployment shape.
type Topology uint8

// Topologies.
const (
	TopologyStandalone Topology = iota + 1
	TopologyCluster
)

// Role says which connection of a Gateway a driver serves.
type Role uint8

// Roles: the main State Store and the optional Response Cache connection
// (OQ-scalability-and-distributed-state-3 (a)).
const (
	RoleMain Role = iota + 1
	RoleCache
)

// Source records where the configuration came from.
type Source uint8

// Sources.
const (
	SourceGateway  Source = iota + 1 // Gateway spec.stateStore
	SourceEnv                        // RURALZ_STATE_STORE_URL
	SourceFallback                   // neither: memory, state_store_memory_fallback
)

// DefaultTimeout applies when Gateway spec.stateStore.timeout is unset.
const DefaultTimeout = 50 * time.Millisecond

// Config is the resolved configuration of one driver. URL is a resolved
// secret: never logged, rendered or put in an error.
type Config struct {
	Role           Role
	Driver         DriverKind
	Topology       Topology
	URL            string
	DefaultTimeout time.Duration
	Source         Source
}

// Digest is the SHA-256 of a key value (config.key result).
type Digest [sha256.Size]byte

// DigestOf hashes s.
func DigestOf(s string) Digest { return sha256.Sum256([]byte(s)) }

// OpKind is one operation; String returns the ruralz_state_* op label.
type OpKind uint8

// Operations of M1. M2/M3 add budget, settle, semantic_get,
// semantic_set and the Plugin operations.
const (
	OpGCRA OpKind = iota + 1
	OpQuota
	OpCacheGet
	OpCacheSet
	OpCacheInvalidate
	OpRefund
)

// String returns the op label.
func (o OpKind) String() string {
	switch o {
	case OpGCRA:
		return "gcra"
	case OpQuota:
		return "quota"
	case OpCacheGet:
		return "cache_get"
	case OpCacheSet:
		return "cache_set"
	case OpCacheInvalidate:
		return "cache_invalidate"
	case OpRefund:
		return "refund"
	default:
		return ""
	}
}

// Label returns the op's index in emit.StateMetrics (the emit.StateOp*
// constants, catalog.Ops order), or -1 for an unknown op. OpKind values
// are not label indexes: catalog.Ops places script_multi and pipeline
// before cache_set.
func (o OpKind) Label() int {
	switch o {
	case OpGCRA:
		return emit.StateOpGCRA
	case OpQuota:
		return emit.StateOpQuota
	case OpCacheGet:
		return emit.StateOpCacheGet
	case OpCacheSet:
		return emit.StateOpCacheSet
	case OpCacheInvalidate:
		return emit.StateOpCacheInvalidate
	case OpRefund:
		return emit.StateOpRefund
	default:
		return -1
	}
}

// WriteLabel returns the index of a post-commit write in
// emit.StateMetrics.WritesDropped (catalog.WriteKinds order), or -1 for an
// op that is not a post-commit write.
func (o OpKind) WriteLabel() int {
	switch o {
	case OpRefund:
		return emit.WriteRefund
	case OpCacheSet:
		return emit.WriteCacheSet
	case OpCacheInvalidate:
		return emit.WriteCacheInvalidate
	default:
		return -1
	}
}

// RoundTrip labels one round trip: a single op, script_multi or pipeline.
type RoundTrip uint8

// Round trip labels beyond single ops.
const (
	TripSingle RoundTrip = iota
	TripScriptMulti
	TripPipeline
)

// Label returns the op label index of a round trip: script_multi, pipeline,
// or single's own label for TripSingle.
func (t RoundTrip) Label(single OpKind) int {
	switch t {
	case TripScriptMulti:
		return emit.StateOpScriptMulti
	case TripPipeline:
		return emit.StateOpPipeline
	default:
		return single.Label()
	}
}

// Result is the ruralz_state_calls_total result label; its values are the
// emit.StateResult* indexes.
type Result uint8

// Results.
const (
	ResultOK      Result = emit.StateResultOK
	ResultError   Result = emit.StateResultError
	ResultTimeout Result = emit.StateResultTimeout
	ResultSkipped Result = emit.StateResultSkipped
)

// GCRALimit is one ratelimit limits[] entry; tau = Burst*Window/Requests.
type GCRALimit struct {
	Requests int64
	Window   time.Duration
	// Burst is used as given: tau = Burst × Window / Requests, so 0 means
	// no burst tolerance (after the first admission, one per emission
	// interval T). Drivers never default it. The ratelimit Filter (WP-61)
	// sets Burst = Requests when config.limits[].burst is unset (05 reqs 57
	// and 60); v1alpha1 Burst is *int64 with no schema default.
	Burst int64
}

// GCRAOutcome is one limit's state after the decision.
type GCRAOutcome struct {
	Remaining  int64
	ResetAfter time.Duration
}

// GCRA is one ratelimit Policy's call: every limit in one script, all or
// nothing, with the server's clock.
type GCRA struct {
	Policy string
	Digest Digest
	Limits []GCRALimit
	// Out is caller-provided with len(Limits).
	Out []GCRAOutcome

	Allowed    bool
	Denied     int // index of the limit setting RetryAfter; -1 when allowed
	RetryAfter time.Duration
	ServerNow  time.Time
}

// Quota reserves one unit of a Consumer quota window.
type Quota struct {
	Name   string
	Window time.Duration
	Limit  int64
	Digest Digest

	Allowed     bool
	WindowStart time.Time
	Used        int64
	RetryAfter  time.Duration
	ServerNow   time.Time
}

// CacheKey locates a Response Cache partition: SHA-256 of the URI and of
// the partition value.
type CacheKey struct{ URI, Partition Digest }

// CacheLookup is the pipelined lookup: GET generation, HMGET partition.
type CacheLookup struct {
	Key     CacheKey
	Variant Digest

	Generation int64
	Names      string
	Found      bool
	EntryGen   int64
	// Entry is opaque and MAC-verified; valid until the Call is reused.
	Entry []byte
}

// Call is one Policy's blocking operation and outcome; callers own it.
// Exactly one operation field is used, selected by Kind.
type Call struct {
	Kind    OpKind
	Timeout time.Duration
	GCRA    GCRA
	Quota   Quota
	Lookup  CacheLookup

	// Done is true when the store decided (false after an earlier deny).
	Done bool
	// Err is nil when the store answered, else one of the sentinels.
	Err *Error
	// Elapsed is the round-trip time; 0 when not attempted or memory.
	Elapsed time.Duration
	// Batch is the index of the call whose span records a shared round
	// trip, or -1.
	Batch int
	// Trip labels the round trip.
	Trip RoundTrip
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
	Generation int64
	TTL        time.Duration
	Entry      []byte
	Token      uint64
}

// LeaseMode selects a revalidation lease operation.
type LeaseMode uint8

// Lease modes.
const (
	LeaseTake LeaseMode = iota + 1
	LeaseRefresh
	LeaseEndStale
)

// CacheLease takes, uses or ends the partition's revalidation lease.
// Refresh and end-stale act only while the lease holds Token, and neither
// releases the lease, which expires after its 5 s (R-71).
type CacheLease struct {
	// Key is the partition the lease guards.
	Key CacheKey
	// Variant is the variant LeaseRefresh refreshes and LeaseEndStale
	// deletes; LeaseTake ignores it.
	Variant Digest
	// Mode selects the operation.
	Mode LeaseMode
	// Token identifies the holder: LeaseTake stores it, and LeaseRefresh
	// and LeaseEndStale act only while the lease still holds it.
	Token uint64
	// Entry is the refreshed entry bytes, LeaseRefresh only.
	Entry []byte
	// TTL applies only to LeaseRefresh, where it extends the partition as
	// CacheStore.TTL does (the larger of the remaining TTL and the entry
	// TTL, capped at 25 h). LeaseTake ignores it, because the revalidation
	// lease is fixed at 5 s (08 req 45, SET NX PX 5000); LeaseEndStale
	// ignores it too.
	TTL time.Duration
}

// Write is one post-commit write.
type Write struct {
	Kind OpKind // OpRefund, OpCacheSet, OpCacheInvalidate
	// Timeout is the effective stateStoreTimeout of the Policy that
	// enqueued the write (08 req 56; the per-request RequestBudget does not
	// apply). The enqueuing Filter sets it from BuildEnv.StateStoreTimeout:
	// quota refunds (WP-62), cache stores and invalidations (WP-63),
	// revalidation lease writes (WP-88). Zero means the write is not
	// attempted, and drivers answer it with ErrNotAttempted, RZ-STS-004 (08
	// req 6). The post-commit queue (WP-65) passes it unchanged.
	Timeout    time.Duration
	Refund     Refund
	Cache      CacheStore
	Lease      CacheLease
	Invalidate Digest // URI digest for OpCacheInvalidate

	Err     *Error
	Applied bool
}

// WriteClass is a post-commit queue class.
type WriteClass uint8

// Queue classes in writer turn order.
const (
	ClassSettle WriteClass = iota + 1 // Quota refunds (Token Budget settlement M3)
	ClassInvalidate
	ClassStore
	ClassPlugin // M2; empty in M1
)

// Enqueuer is the bounded post-commit queue. Enqueue never blocks and
// never allocates on the drop path; false means dropped and counted.
type Enqueuer interface {
	Enqueue(class WriteClass, w *Write) bool
}

// Capability is a command set a Policy may need (RZ-STS-005).
type Capability uint8

// Capabilities.
const (
	CapScripts Capability = 1 << iota
	CapVectorSets
	CapValkeySearch
)

// Store is what Filters use. Implementations are safe for concurrent use
// and never block beyond the computed timeouts.
type Store interface {
	// Consume runs, as one round trip, the longest prefix of calls whose
	// keys share a hash slot (one script; script_multi when longer than
	// one), in order, stopping at the first deny. It returns how many calls
	// it took (at least 1); calls after a deny stay !Done.
	Consume(ctx context.Context, rb *RequestBudget, calls []*Call) int
	// Read runs independent read-only calls as one pipelined batch.
	Read(ctx context.Context, rb *RequestBudget, calls []*Call)
	// Write sends one post-commit batch (queue writers only).
	Write(ctx context.Context, batch []*Write)
	// AdmitStoreBytes applies the Response Cache memory rules to key's shard.
	AdmitStoreBytes(key CacheKey, n int) bool
	// Supports reports whether the deployment offers c.
	Supports(c Capability) bool
	// RoundTrips is false for memory: its time stays gateway-added.
	RoundTrips() bool
}

// Status feeds degraded reasons and the cleartext gauge.
type Status struct {
	BreakerNotClosed bool
	// EvictionPolicy reports maxmemory-policy noeviction (08 req 51). Yes:
	// every shard's latest INFO memory reading reports noeviction. No: any
	// shard reports another policy; state_store_eviction_policy is raised
	// while a ratelimit or quota Policy uses this store. Unknown: no shard
	// reports another policy, but some reading lacks the field or has not
	// arrived; no reason is raised and the driver logs one WARN. The
	// precedence is No, then Unknown, then Yes. The memory driver reports
	// Yes.
	EvictionPolicy  Tristate
	Cleartext       bool
	Unauthenticated bool
}

// Tristate is yes, no or unknown.
type Tristate uint8

// Tristate values.
const (
	Unknown Tristate = iota
	Yes
	No
)

// Driver is a live Store with a lifecycle, owned by the Manager.
type Driver interface {
	Store
	Status() Status
	Close(ctx context.Context) error
}

// Failure classifies an unanswered call.
type Failure uint8

// Failures.
const (
	FailTimeout      Failure = iota + 1 // RZ-STS-001, result timeout
	FailError                           // RZ-STS-002, result error
	FailBreakerOpen                     // RZ-STS-003, result skipped
	FailNotAttempted                    // RZ-STS-004, result skipped
	FailUnsupported                     // RZ-STS-005, result skipped
)

// Error is a State Store failure; the five sentinels are the only values,
// so failures never allocate. Causes are logged, never shown to clients.
type Error struct{ failure Failure }

// Sentinel failures.
var (
	ErrTimeout      = &Error{FailTimeout}
	ErrFailed       = &Error{FailError}
	ErrBreakerOpen  = &Error{FailBreakerOpen}
	ErrNotAttempted = &Error{FailNotAttempted}
	ErrUnsupported  = &Error{FailUnsupported}
)

// Error returns the code and meaning.
func (e *Error) Error() string { return e.Code() + ": state store call failed" }

// Failure returns the classification.
func (e *Error) Failure() Failure { return e.failure }

// Code returns RZ-STS-001 to RZ-STS-005.
func (e *Error) Code() string {
	switch e.failure {
	case FailTimeout:
		return "RZ-STS-001"
	case FailError:
		return "RZ-STS-002"
	case FailBreakerOpen:
		return "RZ-STS-003"
	case FailNotAttempted:
		return "RZ-STS-004"
	default:
		return "RZ-STS-005"
	}
}

// Result returns the metric result label.
func (e *Error) Result() Result {
	switch e.failure {
	case FailTimeout:
		return ResultTimeout
	case FailError:
		return ResultError
	default:
		return ResultSkipped
	}
}

// RequestBudget is pack 8.7 rule 2's per-request deadline: it starts at
// the request's first blocking State Store call and lasts the Route's
// largest stateStoreTimeout. Policies of one request run sequentially, so
// it needs no lock. It also carries the request's metric stripe, which
// drivers use for emit.StateMetrics. The zero value means no State Store
// Policy.
type RequestBudget struct {
	max      time.Duration
	deadline time.Time
	stripe   emit.Stripe
}

// NewRequestBudget returns a budget of routeMax (compile-time RouteMax)
// for a request recorded on stripe s.
func NewRequestBudget(routeMax time.Duration, s emit.Stripe) RequestBudget {
	return RequestBudget{max: routeMax, stripe: s}
}

// Stripe returns the request's metric stripe.
func (b *RequestBudget) Stripe() emit.Stripe { return b.stripe }

// CallTimeout starts the budget on first use and returns
// min(policy, budget left, ctx deadline left); ok is false when spent
// (RZ-STS-004).
func (b *RequestBudget) CallTimeout(ctx context.Context, now time.Time, policy time.Duration) (time.Duration, bool) {
	if b.max <= 0 || policy <= 0 {
		return 0, false
	}
	if b.deadline.IsZero() {
		b.deadline = now.Add(b.max)
	}
	t := min(policy, b.deadline.Sub(now))
	if d, ok := ctx.Deadline(); ok {
		t = min(t, d.Sub(now))
	}
	return t, t > 0
}

// RouteMax returns the largest effective stateStoreTimeout of the Route's
// State Store Policies (ratelimit, quota, cache in M1).
func RouteMax(timeouts []time.Duration) time.Duration {
	var m time.Duration
	for _, t := range timeouts {
		m = max(m, t)
	}
	return m
}

// Limits are the fixed bounds (OQ-scalability-and-distributed-state-4 (a));
// only tests override them.
type Limits struct {
	InFlight                       int
	PipelinedPerShard              int
	DedicatedPerShard              int
	BreakerWindow                  time.Duration
	BreakerMinCalls                int
	BreakerFailureRatio            float64
	BreakerConnectFailures         int
	BreakerOpenMin, BreakerOpenMax time.Duration
	BreakerCloseAfter              int
	ReconnectBase, ReconnectCap    time.Duration
	ConnectsPerSecond              int
	ShardsRefresh                  time.Duration
	MemoryPollSlow, MemoryPollFast time.Duration
}

// DefaultLimits returns the target values of the scalability document.
func DefaultLimits() Limits {
	return Limits{
		InFlight: 8192, PipelinedPerShard: 2, DedicatedPerShard: 8,
		BreakerWindow: 5 * time.Second, BreakerMinCalls: 20, BreakerFailureRatio: 0.5, BreakerConnectFailures: 5,
		BreakerOpenMin: time.Second, BreakerOpenMax: 3 * time.Second, BreakerCloseAfter: 3,
		ReconnectBase: 100 * time.Millisecond, ReconnectCap: 5 * time.Second, ConnectsPerSecond: 4,
		ShardsRefresh: 10 * time.Second, MemoryPollSlow: 10 * time.Second, MemoryPollFast: time.Second,
	}
}

// Deps are shared by drivers.
type Deps struct {
	Clock   clock.Clock
	Metrics *emit.StateMetrics
	Status  emit.NodeStatus
	NodeID  string
	Limits  Limits
	// MACKey is the optional entry MAC key read from
	// RURALZ_STATE_STORE_MAC_KEY_FILE (at least 32 bytes); nil stores and
	// accepts entries without a tag. A wrong or missing tag is a miss.
	MACKey []byte
	// Logger is from internal/telemetry; shards appear as ss-<8 hex>, never
	// as the URL.
	Logger *slog.Logger
}

// Opener constructs a driver without dialing synchronously; it fails only
// for an invalid configuration (RZ-CFG-026 reasons).
type Opener func(ctx context.Context, cfg Config, deps Deps) (Driver, error)
```

Tests (WP-01): `RequestBudget.CallTimeout` is the smaller of the Policy timeout and the remaining budget and starts at the first call; `Stripe` round trip; `RouteMax`; `Error` codes and `Result` mapping of every sentinel; `catalog.Ops()[op.Label()] == op.String()` for every `OpKind`, trip labels `script_multi` and `pipeline`, `catalog.WriteKinds()[op.WriteLabel()]` for the three write kinds; `DigestOf` stable.

### 2.13 Filter SPI

What a built-in Filter implements and sees. `Exchange` is an interface (implemented by `internal/gateway/exchange` over pooled state and by `filtertest.Exchange` in tests); `Filter.Handle(ctx, phase, x)` is called for each subscribed Phase; `Consumptive` admission Filters are batched by the executor. Factories do not declare Phases: the effective chains are the source of truth (`BuildEnv.Phases`). The revision adds the parts the area specs needed but the first contract lacked:

- `Exchange.PolicyState()` (R-39): a per-request slot per Policy (per leg for Upstream-scope Policies) for state carried between Phases, Consumptive steps and `Finish`: the quota reservation's key digest and window start for the `onLog` refund, the tokens `ratelimit`'s `Prepare` took for `Undo`, the Response Cache lookup (keys, stale variant for stale-if-error, coalescing role, saved request).
- `Finisher` (R-40): a post-response hook, independent of subscribed Phases, so `cache` keeps the FP 10 Phases (`onRequestHeaders`, `onResponse`) yet stores the teed body, queues unsafe-method invalidations (05 req 85) and releases a hit's reservation after the body finished streaming.
- `Retry` outcome (R-44): the only way a Filter asks for a retry in `onUpstreamResponseHeaders` (05 req 33 "or a Filter request").
- `Component` and `Registry.Components` (R-45): the lifecycle of Node-wide parts (JWKS manager, token registry, basic throttle, rate-limit key table, cache revalidation workers): constructed once by `filter/builtin`, run and stopped by the `ruralzd` supervisor.
- `Replayer` and `BuildEnv.Replayer` (R-43): stale-while-revalidate replays through the Upstream's leg on the currently published snapshot.
- `Exchange.Stripe` and `Exchange.RouteMetrics` (R-56): Route-labeled families (`ruralz_cache_requests_total`) and every striped handle are reachable from a Filter.
- `filtertest.Store` and `filtertest.ReplayFunc`: the executor's batching tests (WP-20) and cache tests run in wave 2 without a driver.
- Wave 2 boundary (additive or doc only): `DecodedLimitFactor` (R-63), cited by `Message.Decoded`; the `ErrorType() string` convention on `Result.Err`, which the executor already applies, so WP-51 and WP-59 to WP-62 name their `CannotDecide` causes; `Consumptive.Undo` also covers a pending member whose round trip was never sent after a later member's local deny, as `gateway/executor`'s batch runner does (before WP-61 and WP-62); the exact `Identity.Principal` encoding, as `internal/identity` builds it.

`internal/filter/filter.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package filter is the Filter SPI: what a built-in Filter implements
// (Filter, Factory), what it sees of one request (Exchange) and what it
// returns (Result). Filters under internal/filter/... never import
// internal/gateway/...; the data plane implements Exchange and runs the
// executor (internal/gateway/executor), so the offline validators of all
// three binaries can reuse Filter check code.
package filter

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Outcome is what a Filter decided.
type Outcome uint8

// Outcomes.
const (
	// Continue passes the request or response on.
	Continue Outcome = iota
	// Respond short-circuits with Result.Response (request Phases only).
	Respond
	// CannotDecide applies the Policy's failureMode.
	CannotDecide
	// Retry asks the Upstream layer to retry the current attempt
	// (onUpstreamResponseHeaders only; 05 req 33 "or a Filter request"):
	// every other retry condition still applies and retryOn is not
	// consulted; the remaining Filters of the Phase are skipped for this
	// attempt. In any other Phase the executor treats it as CannotDecide.
	Retry
)

// Response is a Filter- or Node-generated response. With Body nil and Code
// set, the request handler writes the RFC 9457 problem document for Code
// (internal/problem); the executor only passes the Response on.
type Response struct {
	// Status is the HTTP status.
	Status int
	// Header holds extra headers (WWW-Authenticate, Retry-After, CORS).
	Header http.Header
	// Body is a literal body (a cache hit, a 204 preflight); nil for a
	// problem document.
	Body []byte
	// Code is the RZ code of an error response; "" for a plain response.
	Code string
	// Detail is the optional problem detail (RZ-RT-009 keyword location).
	Detail string
}

// Result is a Filter's answer for one Phase.
type Result struct {
	Outcome Outcome
	// Response is set with Respond.
	Response *Response
	// Code is the RZ code to use under closed with CannotDecide; "" means
	// the class default (docs/architecture/03-data-plane.md "Failure
	// semantics").
	Code string
	// Err is the cause of CannotDecide: logged at debug level and
	// classified as the span's error.type, never sent to the client. A
	// Filter names the error.type by returning or wrapping an error with an
	// `ErrorType() string` method (found with errors.As). Its value is one
	// of cel_error, invalid_value, null_body, path_error, output_cap,
	// result_type or internal (spec 07 req 86), or the executor's own
	// state_store, buffer_budget, too_large or invalid_result. Any other
	// value is recorded as internal, so the attribute never carries request
	// data. Without such an error the executor maps *expr.EvalError to
	// cel_error, *statestore.Error to state_store, ErrBudget to
	// buffer_budget, ErrTooLarge to too_large, a recovered panic to
	// internal, and an outcome the Phase does not allow to invalid_result;
	// anything else is internal.
	Err error
}

// Next returns Continue.
func Next() Result { return Result{} }

// Reply returns Respond with r.
func Reply(r *Response) Result { return Result{Outcome: Respond, Response: r} }

// Deny returns Respond with a problem document for code at status.
func Deny(status int, code string, h http.Header) Result {
	return Result{Outcome: Respond, Response: &Response{Status: status, Code: code, Header: h}}
}

// Undecided returns CannotDecide with an optional code and cause.
func Undecided(code string, err error) Result {
	return Result{Outcome: CannotDecide, Code: code, Err: err}
}

// RetryAttempt returns Retry (onUpstreamResponseHeaders only).
func RetryAttempt() Result { return Result{Outcome: Retry} }

// Sentinel errors of the SPI.
var (
	// ErrBudget: a limits.maxBufferedBytes reservation failed. 503
	// RZ-RT-004 under either failureMode, in any Phase before commit.
	ErrBudget = errors.New("filter: buffer budget spent")
	// ErrTooLarge: a body or decoded value is over its limit: 413
	// RZ-RT-003 (request), 502 RZ-UP-010 (plain upstreams response) or a
	// step failure RZ-RT-015.
	ErrTooLarge = errors.New("filter: body over limit")
	// ErrSecondBinding: SetIdentity after a successful authentication
	// (401 RZ-AUTH-002, Security and identity rule 2).
	ErrSecondBinding = errors.New("filter: second authentication")
	// ErrNotAvailable: the message or body is not available in this Phase.
	ErrNotAvailable = errors.New("filter: not available in this Phase")
)

// DecodedLimitFactor bounds a decoded JSON body (architecture R-63): the
// values built from a body may cost at most DecodedLimitFactor times the
// raw limit it arrived under (maxRequestBodyBytes, maxResponseBodyBytes or
// a composition step's maxBodyBytes), measured as the jsonval cost of the
// values actually built; past it the body is oversized (ErrTooLarge).
const DecodedLimitFactor = 4

// Identity methods (auth.method).
const (
	MethodJWT    = "jwt"
	MethodAPIKey = "api-key"
	MethodBasic  = "basic"
	MethodMTLS   = "mtls"
)

// Identity is set once per request by the first successful auth-class
// Policy; it feeds the CEL consumer and auth variables, quota keys, cache
// partitions and the access log.
type Identity struct {
	// Method is one of the Method constants.
	Method string
	// Policy is the authenticating Policy's name.
	Policy string
	// Claims is the verified JWT payload; an empty map otherwise.
	Claims expr.Value
	// Consumer is the bound Consumer; nil when unbound.
	Consumer *expr.Consumer
	// CertSubject is the RFC 4514 leaf subject (auth.mtls only).
	CertSubject string
	// Principal is the non-secret cache partition key (spec 06 requirement
	// 22): the bound Consumer's name; else, for jwt with a non-empty string
	// sub, "jwt:" + iss + "#" + sub, with "%" and "#" in iss
	// percent-encoded as %25 and %23 so the encoding is one-to-one; else,
	// for mtls with a non-empty subject, "mtls:" + the leaf's RFC 4514
	// subject; else "": no principal, never a partition key
	// (OQ-security-and-identity-32). Treat it as opaque.
	Principal string
}

// Source is the client address after trusted-proxy and PROXY v2 handling.
type Source struct {
	// IP is source.ip, IPv4-mapped unmapped.
	IP netip.Addr
	// Port is the peer port; 0 when taken from a forwarding header.
	Port uint16
	// Peer is the TCP (or PROXY v2) peer.
	Peer netip.AddrPort
	// FromHeader is true when IP came from Forwarded or X-Forwarded-For.
	FromHeader bool
}

// ConnTLS is the TLS state of the client connection.
type ConnTLS struct {
	// State is the handshake result.
	State *tls.ConnectionState
	// ClientCertRequested is true when the handshake requested a client
	// certificate (a listener serving an auth.mtls Route).
	ClientCertRequested bool
	// Cache memoizes per-connection verification results (auth.mtls).
	Cache *ConnCache
}

// PeerCertificates returns the presented client chain.
func (c *ConnTLS) PeerCertificates() []*x509.Certificate {
	if c == nil || c.State == nil {
		return nil
	}
	return c.State.PeerCertificates
}

// ConnCache is a small per-connection memo (at most 8 entries), safe for
// concurrent HTTP/2 streams.
type ConnCache struct {
	mu   sync.Mutex
	keys [8]any
	vals [8]any
	next int
}

// Get returns the value stored under key.
func (c *ConnCache) Get(key any) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, k := range c.keys {
		if k != nil && k == key {
			return c.vals[i], true
		}
	}
	return nil, false
}

// Put stores v under key, evicting the oldest entry when full.
func (c *ConnCache) Put(key, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keys[c.next], c.vals[c.next] = key, v
	c.next = (c.next + 1) % len(c.keys)
}

// Message is the HTTP message a Phase acts on: the client request
// (onRequestHeaders, onRequestBody, onRoute), the leg's outgoing request
// (onUpstreamRequest), the leg's response (onUpstreamResponseHeaders,
// onUpstreamResponseBody) or the client response (onResponse).
type Message interface {
	// Header is the live header map; edits are seen by later Policies.
	Header() http.Header
	// Body returns the whole body when the Phase is a gate for it;
	// ErrNotAvailable otherwise; nil for an empty body.
	Body(ctx context.Context) ([]byte, error)
	// SetBody replaces the body: it reserves b from the buffer budget
	// (ErrBudget), sets Content-Length, removes Content-Encoding and body
	// digests, and invalidates the decoded body and CEL views.
	SetBody(ctx context.Context, b []byte) error
	// Decoded returns the shared read-only decoded JSON body (nil when not
	// JSON); ErrTooLarge past DecodedLimitFactor times the raw limit.
	Decoded(ctx context.Context) (expr.Value, error)
	// RawQuery returns the raw query of a request message; "" otherwise.
	RawQuery() string
	// SetRawQuery replaces the raw query of a request message.
	SetRawQuery(q string)
	// Status returns the status of a response message; 0 for requests.
	Status() int
	// Generated reports a Node- or Filter-generated response.
	Generated() bool
}

// Leg describes the current upstream attempt in upstream-leg Phases.
type Leg struct {
	// Upstream is the Upstream name.
	Upstream string
	// Step is the composition step name, "" for plain upstreams.
	Step string
	// Attempt counts from 1.
	Attempt int
	// Endpoint is the selected Endpoint address host:port.
	Endpoint string
	// Request is the outgoing request, mutable in onUpstreamRequest.
	Request *http.Request
	// Response is set after response headers arrived.
	Response *http.Response
	// ErrorKind is connect, timeout, reset, tls or "".
	ErrorKind string
}

// RateLimitField is one applied limit for the RateLimit-Policy and
// RateLimit response fields (OQ-traffic-management-and-resilience-2 (c):
// the data plane appends them after onResponse).
type RateLimitField struct {
	// Name is the Policy name, suffixed .1, .2 for multi-limit Policies.
	Name string
	// Q is the quota (requests or quota limit); W the window in seconds.
	Q, W int64
	// R is the remaining count and T the seconds to reset, when Known.
	R, T  int64
	Known bool
}

// Final is the outcome of a request, available in onLog.
type Final struct {
	// Status is the final HTTP status; 0 when the client went away first.
	Status int
	// Code is the final RZ code, if any.
	Code string
	// Committed is true once response headers were sent.
	Committed bool
	// RejectedAfter is true when a Policy later in request order than the
	// calling Policy responded or failed closed (quota refunds).
	RejectedAfter bool
}

// Tee is the store side of the Response Cache: a copy kept while the
// response streams, which stops (skipping the store) past its limit or
// the buffer budget.
type Tee interface {
	// Result returns the copied body and whether it is complete; valid in
	// Finish, after the response finished streaming.
	Result() (body []byte, complete bool)
}

// Exchange is one request as a Filter sees it, valid only during the call
// it is passed to (Handle, Prepare, Complete, Undo, Finish). The data plane
// implements it over pooled per-request state. Upstream-leg Phases get a
// leg view: the same client request read-only, with its own Leg, Message,
// Vars copy (upstream fields replaced), PolicyState slots and span; legs of
// an aggregate composition run concurrently, each on its own view, so a
// Filter never shares a view between goroutines. On a leg view
// SetIdentity returns ErrNotAvailable, TeeResponse returns nil, Final
// returns the zero value (a leg ends before the request does), and
// ReplaceResponse and AddRateLimitField do nothing.
type Exchange interface {
	// RequestID returns the 32-hex trace ID.
	RequestID() string
	// Now returns the request start time.
	Now() time.Time
	// Listener returns the listener name.
	Listener() string
	// Route returns the matched Route view.
	Route() *expr.Route
	// Method, Scheme, Host and Path describe the client request (Path is
	// the one normalized path).
	Method() string
	Scheme() string
	Host() string
	Path() string
	// Header returns the client request headers.
	Header() http.Header
	// Source returns the client address.
	Source() Source
	// TLS returns the connection TLS state; nil on cleartext.
	TLS() *ConnTLS
	// Message returns the message of the current Phase.
	Message() Message
	// Leg returns the current attempt in upstream-leg Phases; nil otherwise.
	Leg() *Leg
	// Vars returns the CEL activation of the current Phase.
	Vars() *expr.Vars
	// Identity returns the request identity; nil before authentication.
	Identity() *Identity
	// SetIdentity records a successful authentication; a second one
	// returns ErrSecondBinding.
	SetIdentity(id *Identity) error
	// StateBudget returns the per-request State Store deadline (pack 8.7
	// rule 2); stores and the post-commit queue come from BuildEnv.
	StateBudget() *statestore.RequestBudget
	// Reserve and Release charge decoded values to limits.maxBufferedBytes.
	Reserve(n int64) error
	Release(n int64)
	// TeeResponse starts the cache store copy (onResponse); nil when the
	// budget or limit refuses.
	TeeResponse(limit int64) Tee
	// ReplaceResponse replaces the uncommitted response (stale-if-error).
	ReplaceResponse(r *Response)
	// AddRateLimitField records an applied limit.
	AddRateLimitField(f RateLimitField)
	// Annotate adds an attribute to the current Filter span.
	Annotate(key string, v slog.Value)
	// Final returns the request outcome; valid in onLog and Finish.
	Final() Final
	// PolicyState returns the per-request slot of the Policy being run
	// (the executor selects it before every call), for state a Filter
	// carries between its Phases, Consumptive steps and Finish: a quota
	// reservation's key digest and window start, the rate-limit tokens
	// Prepare took, a cache lookup's keys, stale variant and coalescing
	// role. It holds nil at the Policy's first call. A Filter stores a
	// pointer to state it pools itself (no allocation) and releases it in
	// Finish. On a leg view the slot is private to that leg.
	PolicyState() *any
	// Stripe returns the request's metric stripe for emit handles.
	Stripe() emit.Stripe
	// RouteMetrics returns the matched Route's handles (the Response Cache
	// records ruralz_cache_requests_total there); never nil.
	RouteMetrics() *emit.RouteMetrics
}

// Filter is a compiled Policy at one attachment environment. Handle is
// called for each Phase the Policy subscribes to, on the request
// goroutine (or a composition step goroutine); it must not retain x.
type Filter interface {
	Handle(ctx context.Context, p phase.Phase, x Exchange) Result
}

// Finisher is implemented by Filters that act once the request is over:
// the Response Cache stores the teed body, queues an unsafe-method
// invalidation and releases a hit's buffer reservation; Filters release
// their PolicyState. The executor calls Finish for every Filter that
// implements it and ran in at least one Phase of the request (not skipped
// by spec.when), after onLog, in request order, on the request goroutine,
// whether or not the Policy subscribes to onLog, including when the client
// went away (x.Final() tells). Upstream-scope Filters get Finish on their
// leg view when the leg ends. Finish must not block: State Store writes go
// to the post-commit queue.
type Finisher interface {
	Finish(ctx context.Context, x Exchange)
}

// Consumptive is implemented by admission Filters whose decision needs one
// consumptive State Store call (ratelimit, quota). The executor batches a
// run of consecutive Consumptive Filters of onRequestHeaders into shared
// round trips (pack 8.7 rule 3) instead of calling Handle, selecting each
// member's PolicyState before Prepare, Complete and Undo; a member keeps in
// it what Undo and onLog need (the call is the executor's and is reused).
type Consumptive interface {
	Filter
	// Prepare runs every Node-local step. It either decides (done true,
	// with r) or fills call and returns done false.
	Prepare(ctx context.Context, x Exchange, call *statestore.Call) (r Result, done bool)
	// Complete applies the reply (call.Err set on failure) and decides.
	Complete(ctx context.Context, x Exchange, call *statestore.Call) Result
	// Undo returns the local tokens Prepare took when the member will not
	// be completed. That happens when an earlier member denied or failed
	// closed (spec 05 req 64), or when the member was waiting for its round
	// trip and a later member's Prepare denied or failed closed locally, so
	// the round trip was never sent. Complete never runs for a member that
	// gets Undo. A member that decided in Prepare gets Undo only when an
	// earlier member denied or failed closed.
	Undo(x Exchange)
}

// Closer is implemented by Filters holding pools, goroutines or secret
// watches; it is called when no live snapshot shares the Filter.
type Closer interface{ Close() error }

// Component is a Node-wide part of a Policy type that outlives snapshots:
// the JWKS manager, the upstream token registry, the auth.basic throttle,
// the rate-limit key table, the Response Cache revalidation workers.
// internal/filter/builtin constructs each once per process and hands it to
// the Factories that use it; the ruralzd supervisor runs every Component
// on one goroutine it owns before the first activation and cancels it at
// shutdown after the last snapshot retired (Registry.Components).
type Component interface {
	// Name identifies the component in logs and shutdown errors.
	Name() string
	// Run serves until ctx is done and returns after every goroutine it
	// started has exited.
	Run(ctx context.Context) error
}

// Replayer sends a saved request through one Upstream's leg (upstream-leg
// Policies, breaker, bulkhead, retries) outside any client request, for
// Response Cache revalidation (05 req 86). The implementation
// (internal/gateway/replay) pins the currently published snapshot for the
// whole call and resolves route and upstream by name in it; either being
// gone is ErrNotAvailable. The caller closes the response body.
type Replayer interface {
	Replay(ctx context.Context, route, upstream string, req *http.Request) (*http.Response, error)
}

// Limits are the effective Node limits a Filter may need.
type Limits struct {
	MaxRequestBodyBytes  int64
	MaxResponseBodyBytes int64
	MaxBufferedBytes     int64
}

// Shared memoizes per-snapshot artifacts (such as the Consumer credential
// index) across the Factories of one snapshot compile.
type Shared interface {
	Get(key any, build func() (any, error)) (any, error)
}

// BuildEnv is everything a Factory needs to compile one Policy at one
// attachment environment. Filters are built per (Policy, scope, Phase
// set), never per Route.
type BuildEnv struct {
	// Policy is the typed Policy and Config its typed config.
	Policy *v1alpha1.Policy
	Config any
	// Scope and Phases come from the effective chains (precedence).
	Scope  phase.Scope
	Phases phase.Set
	// Bundle is the validated Revision; Consumers the compiled Consumers.
	Bundle    *hub.Bundle
	Consumers map[string]*expr.Consumer
	// CEL compiles this Policy's expressions; Site refines a place for
	// this attachment.
	CEL  expr.Builder
	Site func(place expr.PlaceID) expr.Site
	// Secrets is the resolved secret table of the Revision.
	Secrets secret.Store
	// StateStore is the main store; CacheStore the Response Cache
	// connection (the main store when none is configured).
	StateStore statestore.Store
	CacheStore statestore.Store
	Enqueuer   statestore.Enqueuer
	// StateStoreTimeout is the effective spec.stateStoreTimeout.
	StateStoreTimeout time.Duration
	Limits            Limits
	Metrics           *emit.PolicyMetrics
	Status            emit.NodeStatus
	Clock             clock.Clock
	Logger            *slog.Logger
	Shared            Shared
	// Replayer revalidates Response Cache entries (cache type only).
	Replayer Replayer
	// Previous is the Filter built for the same Policy identity and
	// canonical config in the previous snapshot, for carry-over; nil if none.
	Previous Filter
}

// Factory builds Filters for one Policy type. An error carries an RZ-CFG
// code (errcode.Wrap) and rejects the Revision.
type Factory interface {
	Build(ctx context.Context, env BuildEnv) (Filter, error)
}

// Registry maps Policy types to Factories and lists the Node-wide
// Components they share; internal/filter/builtin constructs it as an
// explicit table.
type Registry struct {
	factories  map[v1alpha1.PolicyType]Factory
	components []Component
}

// NewRegistry returns a Registry over a copy of f with the components c.
func NewRegistry(f map[v1alpha1.PolicyType]Factory, c ...Component) *Registry {
	m := make(map[v1alpha1.PolicyType]Factory, len(f))
	for k, v := range f {
		m[k] = v
	}
	return &Registry{factories: m, components: append([]Component(nil), c...)}
}

// Factory returns the Factory of t.
func (r *Registry) Factory(t v1alpha1.PolicyType) (Factory, bool) {
	f, ok := r.factories[t]
	return f, ok
}

// Components returns the Node-wide components to run, in construction order.
func (r *Registry) Components() []Component { return append([]Component(nil), r.components...) }
```

`internal/filter/filtertest/filtertest.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package filtertest provides an in-memory filter.Exchange,
// filter.Message, statestore.Store and filter.Replayer for Filter and
// executor unit tests, so Policy packages and the executor are tested
// without the data plane or a State Store driver.
package filtertest

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Message is an in-memory filter.Message. Set Unavailable to model a
// Phase that is not a body gate.
type Message struct {
	Hdr http.Header
	// Raw is the body; Unavailable makes Body return ErrNotAvailable.
	Raw         []byte
	Unavailable bool
	// DecodedValue is returned by Decoded.
	DecodedValue expr.Value
	Query        string
	Code         int
	Gen          bool
	// Budget, when non-nil, is charged by SetBody.
	Budget *Budget
}

var _ filter.Message = (*Message)(nil)

// Header implements filter.Message.
func (m *Message) Header() http.Header {
	if m.Hdr == nil {
		m.Hdr = http.Header{}
	}
	return m.Hdr
}

// Body implements filter.Message.
func (m *Message) Body(context.Context) ([]byte, error) {
	if m.Unavailable {
		return nil, filter.ErrNotAvailable
	}
	return m.Raw, nil
}

// SetBody implements filter.Message.
func (m *Message) SetBody(_ context.Context, b []byte) error {
	if m.Budget != nil {
		if err := m.Budget.Reserve(int64(len(b))); err != nil {
			return err
		}
	}
	m.Raw = b
	m.DecodedValue = nil
	h := m.Header()
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Del("Content-Encoding")
	h.Del("Content-Digest")
	h.Del("Repr-Digest")
	return nil
}

// Decoded implements filter.Message.
func (m *Message) Decoded(context.Context) (expr.Value, error) { return m.DecodedValue, nil }

// RawQuery implements filter.Message.
func (m *Message) RawQuery() string { return m.Query }

// SetRawQuery implements filter.Message.
func (m *Message) SetRawQuery(q string) { m.Query = q }

// Status implements filter.Message.
func (m *Message) Status() int { return m.Code }

// Generated implements filter.Message.
func (m *Message) Generated() bool { return m.Gen }

// Budget models limits.maxBufferedBytes.
type Budget struct {
	mu        sync.Mutex
	Limit     int64
	Reserved  int64
	Exhausted bool
}

// Reserve charges n bytes; ErrBudget past Limit (Limit 0 is unlimited).
func (b *Budget) Reserve(n int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Exhausted || (b.Limit > 0 && b.Reserved+n > b.Limit) {
		return filter.ErrBudget
	}
	b.Reserved += n
	return nil
}

// Release returns n bytes.
func (b *Budget) Release(n int64) {
	b.mu.Lock()
	b.Reserved -= n
	b.mu.Unlock()
}

// Tee is a completed filter.Tee.
type Tee struct {
	Body     []byte
	Complete bool
}

// Result implements filter.Tee.
func (t *Tee) Result() ([]byte, bool) { return t.Body, t.Complete }

// Exchange is an in-memory filter.Exchange. Zero values are usable;
// recorded effects (identity, rate-limit fields, annotations, replaced
// response) are readable after Handle returns.
type Exchange struct {
	ID            string
	Start         time.Time
	ListenerV     string
	RouteV        *expr.Route
	MethodV       string
	SchemeV       string
	HostV         string
	PathV         string
	Hdr           http.Header
	SourceV       filter.Source
	TLSV          *filter.ConnTLS
	Msg           filter.Message
	LegV          *filter.Leg
	VarsV         *expr.Vars
	Ident         *filter.Identity
	Budget        Budget
	StateDeadline *statestore.RequestBudget
	TeeV          *Tee
	FinalV        filter.Final
	// State is the PolicyState slot; StripeV and RouteMetricsV are returned
	// as is (RouteMetrics falls back to an empty value).
	State         any
	StripeV       emit.Stripe
	RouteMetricsV *emit.RouteMetrics

	// Effects recorded by the Filter under test.
	Replaced    *filter.Response
	RateLimits  []filter.RateLimitField
	Annotations map[string]slog.Value
}

var _ filter.Exchange = (*Exchange)(nil)

// RequestID implements filter.Exchange.
func (x *Exchange) RequestID() string { return x.ID }

// Now implements filter.Exchange.
func (x *Exchange) Now() time.Time { return x.Start }

// Listener implements filter.Exchange.
func (x *Exchange) Listener() string { return x.ListenerV }

// Route implements filter.Exchange.
func (x *Exchange) Route() *expr.Route { return x.RouteV }

// Method implements filter.Exchange.
func (x *Exchange) Method() string { return x.MethodV }

// Scheme implements filter.Exchange.
func (x *Exchange) Scheme() string { return x.SchemeV }

// Host implements filter.Exchange.
func (x *Exchange) Host() string { return x.HostV }

// Path implements filter.Exchange.
func (x *Exchange) Path() string { return x.PathV }

// Header implements filter.Exchange.
func (x *Exchange) Header() http.Header {
	if x.Hdr == nil {
		x.Hdr = http.Header{}
	}
	return x.Hdr
}

// Source implements filter.Exchange.
func (x *Exchange) Source() filter.Source { return x.SourceV }

// TLS implements filter.Exchange.
func (x *Exchange) TLS() *filter.ConnTLS { return x.TLSV }

// Message implements filter.Exchange.
func (x *Exchange) Message() filter.Message { return x.Msg }

// Leg implements filter.Exchange.
func (x *Exchange) Leg() *filter.Leg { return x.LegV }

// Vars implements filter.Exchange.
func (x *Exchange) Vars() *expr.Vars {
	if x.VarsV == nil {
		x.VarsV = &expr.Vars{}
	}
	return x.VarsV
}

// Identity implements filter.Exchange.
func (x *Exchange) Identity() *filter.Identity { return x.Ident }

// SetIdentity implements filter.Exchange.
func (x *Exchange) SetIdentity(id *filter.Identity) error {
	if x.Ident != nil {
		return filter.ErrSecondBinding
	}
	x.Ident = id
	return nil
}

// StateBudget implements filter.Exchange.
func (x *Exchange) StateBudget() *statestore.RequestBudget { return x.StateDeadline }

// Reserve implements filter.Exchange.
func (x *Exchange) Reserve(n int64) error { return x.Budget.Reserve(n) }

// Release implements filter.Exchange.
func (x *Exchange) Release(n int64) { x.Budget.Release(n) }

// TeeResponse implements filter.Exchange; it returns TeeV as is.
func (x *Exchange) TeeResponse(int64) filter.Tee {
	if x.TeeV == nil {
		return nil
	}
	return x.TeeV
}

// ReplaceResponse implements filter.Exchange.
func (x *Exchange) ReplaceResponse(r *filter.Response) { x.Replaced = r }

// AddRateLimitField implements filter.Exchange.
func (x *Exchange) AddRateLimitField(f filter.RateLimitField) {
	x.RateLimits = append(x.RateLimits, f)
}

// Annotate implements filter.Exchange.
func (x *Exchange) Annotate(key string, v slog.Value) {
	if x.Annotations == nil {
		x.Annotations = map[string]slog.Value{}
	}
	x.Annotations[key] = v
}

// Final implements filter.Exchange.
func (x *Exchange) Final() filter.Final { return x.FinalV }

// PolicyState implements filter.Exchange; one slot for the Filter under test.
func (x *Exchange) PolicyState() *any { return &x.State }

// Stripe implements filter.Exchange.
func (x *Exchange) Stripe() emit.Stripe { return x.StripeV }

// RouteMetrics implements filter.Exchange.
func (x *Exchange) RouteMetrics() *emit.RouteMetrics {
	if x.RouteMetricsV == nil {
		x.RouteMetricsV = &emit.RouteMetrics{}
	}
	return x.RouteMetricsV
}

// Store is a scriptable statestore.Store that counts round trips. With a
// nil ConsumeFn every call is answered: Done, no error, Allowed. It is
// safe for concurrent use.
type Store struct {
	mu sync.Mutex
	// ConsumeFn answers one Consume round trip and returns how many calls
	// it took (at least 1).
	ConsumeFn func(calls []*statestore.Call) int
	// ReadFn answers one Read batch.
	ReadFn func(calls []*statestore.Call)
	// Trips counts Consume and Read round trips; Writes the Write batches.
	Trips, Writes int
	// Written records every post-commit write.
	Written []*statestore.Write
	// Caps are the supported capabilities (default CapScripts).
	Caps statestore.Capability
}

var _ statestore.Store = (*Store)(nil)

// Consume implements statestore.Store.
func (s *Store) Consume(_ context.Context, _ *statestore.RequestBudget, calls []*statestore.Call) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Trips++
	if s.ConsumeFn != nil {
		return max(s.ConsumeFn(calls), 1)
	}
	for _, c := range calls {
		c.Done = true
		c.GCRA.Allowed, c.Quota.Allowed = true, true
		c.GCRA.Denied = -1
	}
	return len(calls)
}

// Read implements statestore.Store.
func (s *Store) Read(_ context.Context, _ *statestore.RequestBudget, calls []*statestore.Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Trips++
	if s.ReadFn != nil {
		s.ReadFn(calls)
		return
	}
	for _, c := range calls {
		c.Done = true
	}
}

// Write implements statestore.Store.
func (s *Store) Write(_ context.Context, batch []*statestore.Write) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Writes++
	for _, w := range batch {
		w.Applied = true
		s.Written = append(s.Written, w)
	}
}

// AdmitStoreBytes implements statestore.Store; it always admits.
func (*Store) AdmitStoreBytes(statestore.CacheKey, int) bool { return true }

// Supports implements statestore.Store.
func (s *Store) Supports(c statestore.Capability) bool {
	caps := s.Caps
	if caps == 0 {
		caps = statestore.CapScripts
	}
	return caps&c == c
}

// RoundTrips implements statestore.Store; the fake counts as remote.
func (*Store) RoundTrips() bool { return true }

// ReplayFunc adapts a function to filter.Replayer.
type ReplayFunc func(ctx context.Context, route, upstream string, req *http.Request) (*http.Response, error)

// Replay implements filter.Replayer.
func (f ReplayFunc) Replay(ctx context.Context, route, upstream string, req *http.Request) (*http.Response, error) {
	return f(ctx, route, upstream, req)
}
```

Tests (WP-01): `ConnCache` eviction order; `Registry` copy semantics and `Components` order; `filtertest.Exchange.SetIdentity` second call returns `ErrSecondBinding`; `filtertest.Exchange.PolicyState` keeps its value across calls; `filtertest.Message.SetBody` drops `Content-Encoding` and digests; `filtertest.Store` counts one trip per `Consume` and answers every call.

### 2.14 Snapshot

The compiled, immutable Revision the request path reads lock-free, and the request-path contracts between the packages that serve it. `Forwarder` is implemented by `gateway/upstream/forward` (plain `upstreams`) and `gateway/composition`; `LegRunner` by `gateway/upstream/forward` per snapshot; `LegHooks` and `LegRun` by the executor; `RequestState` by `gateway/exchange`; `RequestBody` by `gateway/body`; `Router` by `internal/gateway/router`; `StoreHandle` by `internal/statestore/manager`. `Pins` implements the striped pin counters and per-stripe intrusive lists of 04 reqs 50 and 53; the holder, retirer and ending protocol live in `internal/gateway/retire`. The revision adds:

- `Outbound` and `RequestBody` (R-41): the handler hands the Upstream layer the client body (replayable when empty, gated or unread; `Buffered` for `GetBody`; one gated body replayed to several composition steps, 05 req 54), the trace `Decision` for `Tracer.Inject`, the stripe and the Route deadline, so `gateway/upstream` never imports `gateway/exchange`.
- `LegHooks.BeginLeg` returning a `LegRun`, and `RequestState.Leg` (R-42): every leg (a plain Upstream leg or a composition step) runs its upstream-leg Policies on its own leg view (Vars copy with upstream fields replaced, private `PolicyState` slots, `when` decisions and span), so the concurrent legs of an `aggregate` composition share nothing mutable; `LegRun.End` runs the leg Filters' `Finish`.
- `RequestState` (R-42): the executor's view of per-request state, fixed here so `gateway/executor` (WP-20) and `gateway/exchange` (WP-43) code against one contract and WP-43 does not depend on WP-20.
- `Snapshot.Binding` (R-58): the `emit.Binding` set by activation just before publication; the retirer calls `Retire` at retirement and `Release` at zero pins.
- `Snapshot.LegChains` and `Snapshot.Legs` (R-43): replay finds the Upstream's leg chain and runs one leg without a Route plan.
- `Route.Protocol` (R-59): the key of the handler's dispatch table.
- `StoreHandle` reference rule (wave 2 boundary, doc only): `retire` releases each instance once, when the last live snapshot holding it is freed, comparing instances by identity; so the manager's handle is a pointer type (WP-65, which keeps `Retain` for post-commit queue items), and the snapshot compiler (WP-70) opens a new handle per snapshot and role or carries the previous instance over without retaining it again.

`internal/gateway/snapshot/snapshot.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package snapshot is the compiled, immutable form of one Revision that
// the request path reads lock-free: Routers per listener, compiled Routes
// with per-Phase Filter arrays and forwarders, listener and Gateway
// settings, compiled Consumers, CEL programs and metric handles. It is
// built by internal/gateway/compile, published and retired by
// internal/gateway/retire, and read by the handler, executor, Router and
// Upstream layer. It also fixes the request-path contracts between those
// packages: Forwarder and Outbound (handler to Upstream layer), LegHooks
// and LegRun (Upstream layer to executor), RequestBody (body buffering to
// Upstream layer) and RequestState (exchange to executor). It holds data
// and interfaces only, plus the pin stripes.
package snapshot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Snapshot is one compiled Revision. Every field is immutable after
// publication; resources are closed only at zero pins when no newer
// snapshot shares them. Resolved secrets and Endpoint sets live beside it
// and change copy-on-write.
type Snapshot struct {
	// Revision is the digest and exact canonical bytes (/config/dump, LKG).
	Revision revision.Revision
	// Validated is the pipeline output the snapshot was compiled from.
	Validated *hub.Validated
	// Gateway holds the Node-wide settings of the Revision.
	Gateway Gateway
	// Listeners are the client listeners in name order.
	Listeners []*Listener
	// Routers holds one Router per listener name.
	Routers map[string]Router
	// Routes are indexed by Route.Index.
	Routes []*Route
	// Consumers are the compiled Consumers by name.
	Consumers map[string]*expr.Consumer
	// Programs is the CEL program set, passed as prev to the next Builder.
	Programs expr.ProgramSet
	// Metrics is the admission plan of the Revision.
	Metrics emit.Plan
	// Binding is the snapshot's metric label-set binding (emit.Meter.Bind).
	// Activation sets it just before publication; the retirer calls Retire
	// when the snapshot is retired and Release at zero pins.
	Binding emit.Binding
	// LegChains are the upstream-leg chains by Upstream name; Route chains
	// share these values (a leg chain depends only on its Upstream).
	LegChains map[string]*LegChain
	// Legs runs one leg to a named Upstream outside a Route's forwarding
	// plan (Response Cache revalidation through internal/gateway/replay).
	Legs LegRunner
	// StateStore and CacheStore are the retained State Store handles.
	StateStore, CacheStore StoreHandle
	// Resources are closed at zero pins unless a newer snapshot shares them.
	Resources []io.Closer
	// Pins counts requests running on the snapshot.
	Pins *Pins
}

// StoreHandle is a snapshot's reference to a State Store driver.
//
// internal/gateway/retire releases each StoreHandle instance exactly once,
// when the last live snapshot holding it is freed. Instances are compared
// by identity, so implementations must be comparable pointer types: a
// value that cannot be compared is treated as unshared and released at
// every free. One instance set as both StateStore and CacheStore counts
// once. The snapshot compiler therefore either opens a new handle per
// snapshot and role (one reference each) or carries the previous
// snapshot's instance over without retaining it again. Retaining a
// carried-over instance once per snapshot leaks references, and the old
// driver never closes (08 req 61).
type StoreHandle interface {
	Store() statestore.Store
	Enqueuer() statestore.Enqueuer
	Release()
}

// Gateway holds the compiled Gateway settings.
type Gateway struct {
	// AdminPort is spec.admin.port (default 9901).
	AdminPort int
	// TrustedProxies is spec.trustedProxies.
	TrustedProxies []netip.Prefix
	// Limits are the effective limits.
	Limits Limits
	// TraceSampling is the root ratio (default 0.01).
	TraceSampling float64
	// OTLPEndpoint is spec.telemetry.otlp.endpoint, "" for none.
	OTLPEndpoint string
	// AccessLogWhen is the compiled accessLog.when; nil selects every request.
	AccessLogWhen expr.Program
	// StateStoreTimeout is the default Policy stateStoreTimeout.
	StateStoreTimeout time.Duration
}

// Limits are effective Node limits of the Revision (bytes after decoding).
type Limits struct {
	MaxRequestBodyBytes  int64
	MaxResponseBodyBytes int64
	MaxBufferedBytes     int64
	MaxCompositionSteps  int
	// MaxRequestHeaderBytes is min(configured, 256 KiB).
	MaxRequestHeaderBytes int64
	// HeaderLimitCapped raises header_limit_capped while active.
	HeaderLimitCapped bool
}

// Listener is one compiled client listener.
type Listener struct {
	Name          string
	Protocol      v1alpha1.ListenerProtocol
	Port          int
	ProxyProtocol bool
	// Hostnames limits the listener; empty accepts any host.
	Hostnames []string
	// TLS is set for https.
	TLS     *ListenerTLS
	Metrics *emit.ListenerMetrics
}

// ListenerKey is the identity that decides whether a Hot Reload needs a
// new socket: TLS and hostnames changes never do.
type ListenerKey struct {
	Name          string
	Port          int
	Protocol      v1alpha1.ListenerProtocol
	ProxyProtocol bool
}

// Key returns the listener identity.
func (l *Listener) Key() ListenerKey {
	return ListenerKey{Name: l.Name, Port: l.Port, Protocol: l.Protocol, ProxyProtocol: l.ProxyProtocol}
}

// ListenerTLS is read by GetConfigForClient for new handshakes.
type ListenerTLS struct {
	// MinVersion is tls.VersionTLS13 (default) or VersionTLS12.
	MinVersion uint16
	// Certificates in name order; values come from the secret Store.
	Certificates []Certificate
	// RequestClientCert is true when a Route bound to the listener has
	// auth.mtls in its effective chain.
	RequestClientCert bool
}

// Certificate references one served certificate and key.
type Certificate struct {
	Name        string
	Certificate secret.Ref
	PrivateKey  secret.Ref
}

// Router is one listener's compiled Router.
type Router interface {
	// Match selects a Route; it allocates nothing. params is caller storage.
	Match(ctx context.Context, q *MatchRequest, params []expr.Param) MatchResult
}

// MatchRequest is the pre-body request data routing uses.
type MatchRequest struct {
	Host   string
	Path   string
	Method string
	Header http.Header
	// Vars is the pre-route activation (request, source, now) for match.when.
	Vars *expr.Vars
}

// MatchResult is the Router's answer.
type MatchResult struct {
	// Route is the index into Snapshot.Routes; -1 means RZ-RT-001.
	Route int32
	// Params are template captures.
	Params []expr.Param
	// Err is a match.when runtime error: RZ-RT-006, no fallthrough.
	Err error
}

// Route is one compiled Route.
type Route struct {
	Index int32
	Name  string
	// View is the CEL route variable.
	View *expr.Route
	// Listeners are the bound listener names.
	Listeners []string
	// Protocol is the protocol of the Route's Upstreams; the handler's
	// dispatch table is keyed by it. M1 serves only http (validation
	// rejects every other protocol with RZ-CFG-040); M3 and M4 add handlers.
	Protocol v1alpha1.UpstreamProtocol
	// Timeout is the effective Route timeout (15 s default in M1).
	Timeout time.Duration
	// StateStoreMax is the largest stateStoreTimeout of its State Store
	// Policies (statestore.RouteMax); 0 when none.
	StateStoreMax time.Duration
	// Chain holds the compiled Policies.
	Chain Chain
	// Forward runs plain upstreams or composition.
	Forward Forwarder
	// Body says which bodies are gated or teed.
	Body    BodyNeeds
	Metrics *emit.RouteMetrics
}

// BodyNeeds are the compile-time body decisions of a Route.
type BodyNeeds struct {
	// RequestGate buffers the request body before onRequestBody.
	RequestGate bool
	// ResponseGate buffers the client response before onResponse.
	ResponseGate bool
	// ResponseTee copies the response for the Response Cache store.
	ResponseTee bool
}

// Chain is a Route's compiled Filter Chain, built from hub.Chain.
type Chain struct {
	// Client holds client-leg Phases in execution order.
	Client [phase.Count][]*Policy
	// Legs holds upstream-leg chains by Upstream name.
	Legs map[string]*LegChain
	// Policies lists every Policy once, in request order; Policy.Index
	// points here (per-request skip bitsets, onLog order).
	Policies []*Policy
	// AuthPolicies counts auth-class Policies (Security rule 1).
	AuthPolicies int
}

// LegChain is one Upstream's upstream-leg chain.
type LegChain struct {
	Upstream string
	Phases   [phase.Count][]*Policy
}

// Policy is one compiled Policy attachment.
type Policy struct {
	// Index is the position in Chain.Policies.
	Index       int
	Name        string
	Type        v1alpha1.PolicyType
	Class       phase.Class
	Scope       phase.Scope
	Position    int
	FailureMode v1alpha1.FailureMode
	// When is the compiled spec.when; nil when absent.
	When       expr.Program
	FirstPhase phase.Phase
	Phases     phase.Set
	// StateStoreTimeout is the effective timeout for State Store Policies.
	StateStoreTimeout time.Duration
	Filter            filter.Filter
	// Consumptive is Filter as filter.Consumptive, or nil.
	Consumptive filter.Consumptive
	// SpanName is "ruralz.filter.<name>".
	SpanName string
	Metrics  *emit.PolicyMetrics
}

// Forwarder sends a request to its Upstream legs: the Upstream layer
// (plain upstreams) or the composition engine. Exactly one of the results
// is non-nil; a failure error carries its RZ code (errcode.CodeOf).
type Forwarder interface {
	Forward(ctx context.Context, o *Outbound) (*UpstreamResponse, error)
}

// LegRunner runs one leg to a named Upstream of the snapshot, outside any
// Route's forwarding plan; the Upstream layer implements it per snapshot.
type LegRunner interface {
	RunLeg(ctx context.Context, upstream string, o *Outbound) (*UpstreamResponse, error)
}

// Outbound is what a Forwarder needs from the request besides the context.
// The handler fills it after the request Phases; it is read-only for the
// Forwarder and its legs.
type Outbound struct {
	// X is the client exchange: method, normalized path, query, headers
	// after the request Phases, Source for forwarding headers, Vars (the
	// Base variables hashKey and step expressions read).
	X filter.Exchange
	// Body is the client request body.
	Body RequestBody
	// Trace is the request's sampling decision; every leg injects it
	// (emit.Tracer.Inject).
	Trace *emit.Decision
	// Stripe is the request's metric stripe.
	Stripe emit.Stripe
	// Deadline is the Route deadline (request start plus Route timeout).
	Deadline time.Time
	// Hooks runs the upstream-leg Phases of every leg.
	Hooks LegHooks
}

// ErrNotReplayable is returned by RequestBody.Open when the body cannot be
// sent again.
var ErrNotReplayable = errors.New("snapshot: request body is not replayable")

// RequestBody is the client request body as the Upstream layer sees it
// (05 req 31, 54); internal/gateway/body implements it over the body gate
// or the client stream. Opens happen on one goroutine at a time except for
// a gated body, whose readers are independent.
type RequestBody interface {
	// ContentLength is the length when known (declared or gated), -1 when
	// unknown, 0 for no body.
	ContentLength() int64
	// Replayable reports whether another Open can succeed: the body is
	// empty, gated (buffered within maxRequestBodyBytes), or streamed with
	// no byte read yet.
	Replayable() bool
	// Open returns a reader from the first byte for one attempt or step;
	// ErrNotReplayable once a streamed body was read. Closing the reader
	// never closes the client stream.
	Open() (io.ReadCloser, error)
	// Buffered returns the gated bytes, or nil for a streamed body; the
	// Upstream layer sets http.Request.GetBody only when it is non-nil.
	Buffered() []byte
}

// LegHooks run upstream-leg Phases; the executor implements them over the
// request's RequestState.
type LegHooks interface {
	// BeginLeg starts one leg: an Upstream leg of plain upstreams or one
	// composition step. Legs of an aggregate composition run concurrently,
	// each with its own LegRun.
	BeginLeg(ctx context.Context, upstream, step string) LegRun
}

// LegRun is one leg's hooks; its methods are called from the leg's
// goroutine only. End must be called exactly once, after the leg's last
// attempt; it runs Finish for the leg's Filters and releases the leg view.
type LegRun interface {
	// OnUpstreamRequest runs per attempt; a non-nil response ends the leg
	// without retry.
	OnUpstreamRequest(ctx context.Context, leg *filter.Leg) *filter.Response
	// OnUpstreamResponseHeaders runs per attempt in reverse order; retry is
	// true when a Filter returned filter.Retry; replace is a leg response
	// set by a failure (502 RZ-RT-012).
	OnUpstreamResponseHeaders(ctx context.Context, leg *filter.Leg) (retry bool, replace *filter.Response)
	// OnUpstreamResponseBody runs once per leg when subscribed.
	OnUpstreamResponseBody(ctx context.Context, leg *filter.Leg) *filter.Response
	// End finishes the leg.
	End(ctx context.Context)
}

// RequestState is the per-request state the executor drives, implemented
// by internal/gateway/exchange over pooled memory. A RequestState belongs
// to one goroutine at a time: the request goroutine for the client state,
// the leg's goroutine for a leg state.
type RequestState interface {
	// Exchange returns the Filter view; its Message, Leg, Vars,
	// PolicyState and Annotate follow the last Enter and SetLeg.
	Exchange() filter.Exchange
	// Enter selects the Policy and Phase of the next Filter call and the
	// span Annotate writes to (nil when the request is not sampled).
	Enter(p *Policy, ph phase.Phase, span emit.Span)
	// SetLeg sets the attempt of a leg state (Leg and Message follow it).
	SetLeg(l *filter.Leg)
	// When returns p's recorded spec.when decision: decided is false until
	// SetWhen, skip is true when the Policy is skipped. A client state
	// records one decision per request, a leg state one per leg.
	When(p *Policy) (decided, skip bool)
	// SetWhen records p's decision; a Policy without when is recorded
	// with skip false before its first Phase, so decided and !skip means
	// the Policy ran.
	SetWhen(p *Policy, skip bool)
	// RecordShortCircuit and RecordFailure feed the access record, the
	// Filter metrics labels and /tap.
	RecordShortCircuit(p *Policy, ph phase.Phase, status int)
	RecordFailure(p *Policy, ph phase.Phase, mode v1alpha1.FailureMode)
	// Budget is the per-request State Store deadline and stripe.
	Budget() *statestore.RequestBudget
	// Leg returns a leg state: the client request read-only, and its own
	// Leg, Message, Vars copy (upstream fields replaced), PolicyState slots
	// and when decisions for leg Policies.
	Leg(upstream, step string) RequestState
	// Release returns a leg state to its pool; a no-op on the client state,
	// which the handler releases.
	Release()
}

// UpstreamResponse is what the handler commits.
type UpstreamResponse struct {
	Status  int
	Header  http.Header
	Trailer http.Header
	// Body streams the response; Close releases the bulkhead slot.
	Body io.ReadCloser
	// Generated is set when a leg Filter responded.
	Generated *filter.Response
	// Partial sets ruralz-partial: true (optional steps failed).
	Partial bool
	// Upstream, Endpoint and Attempts describe the last leg for logs.
	Upstream  string
	Endpoint  string
	Attempts  int
	ErrorKind string
}

// EndReason says why the data plane ended a pinned request.
type EndReason uint8

// End reasons.
const (
	EndNone  EndReason = iota
	EndGrace           // RZ-RT-014
	EndDrain           // RZ-RT-016
)

// PinnedRequest is the pooled per-request record the ending protocol acts
// on. Mu guards Committed and Ended; the handler commits under Mu.
type PinnedRequest struct {
	Mu        sync.Mutex
	Committed bool
	Ended     EndReason
	// Cancel cancels the request's root context.
	Cancel context.CancelFunc
	// RC sets deadlines on the client connection.
	RC *http.ResponseController

	prev, next *PinnedRequest
}

// Pins counts and lists the requests pinned to one snapshot, in
// cache-line-padded stripes (S = min(GOMAXPROCS at start, 8)). Zero pins
// proves no request runs on the snapshot.
type Pins struct{ stripes []pinStripe }

type pinStripe struct {
	n    atomic.Int64
	mu   sync.Mutex
	head *PinnedRequest
	_    [40]byte
}

// NewPins returns n stripes.
func NewPins(n int) *Pins { return &Pins{stripes: make([]pinStripe, max(n, 1))} }

// Add pins r on stripe s.
func (p *Pins) Add(s int, r *PinnedRequest) {
	st := &p.stripes[s%len(p.stripes)]
	st.n.Add(1)
	st.mu.Lock()
	r.prev, r.next = nil, st.head
	if st.head != nil {
		st.head.prev = r
	}
	st.head = r
	st.mu.Unlock()
}

// Remove unpins r from stripe s.
func (p *Pins) Remove(s int, r *PinnedRequest) {
	st := &p.stripes[s%len(p.stripes)]
	st.mu.Lock()
	if r.prev != nil {
		r.prev.next = r.next
	} else if st.head == r {
		st.head = r.next
	}
	if r.next != nil {
		r.next.prev = r.prev
	}
	r.prev, r.next = nil, nil
	st.mu.Unlock()
	st.n.Add(-1)
}

// Count returns the number of pinned requests.
func (p *Pins) Count() int64 {
	var n int64
	for i := range p.stripes {
		n += p.stripes[i].n.Load()
	}
	return n
}

// Each calls fn for every pinned request, one stripe at a time; fn must
// not call Add or Remove.
func (p *Pins) Each(fn func(*PinnedRequest)) {
	for i := range p.stripes {
		st := &p.stripes[i]
		st.mu.Lock()
		for r := st.head; r != nil; r = r.next {
			fn(r)
		}
		st.mu.Unlock()
	}
}
```

Tests (WP-01): `Pins` add/remove/count/each under `-race` with 8 stripes; `Remove` of head, middle and tail; `Listener.Key` ignores TLS and hostnames; `ErrNotReplayable` is a distinct sentinel.

### 2.15 Admin wire types

Shared by the admin server (`ruralzd`) and `ruralz` (`dev tap`, `node dump`, `node drain`), so both sides encode and decode the same JSON.

`internal/adminapi/adminapi.go`:

```go
// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package adminapi holds the JSON wire shapes of the Ruralz Gateway admin
// API (port 9901) that ruralzd serves and the ruralz CLI reads: /readyz,
// /tap events and drop notices, /debug/snapshots. /config/dump is the
// RFC 8785 document of internal/config/canonical. Shapes are versioned
// surfaces: members are only added, and readers ignore unknown members.
package adminapi

// Readiness reasons of /readyz.
const (
	ReasonNoRevision        = "no_revision"
	ReasonSecretsUnresolved = "secrets_unresolved" //nolint:gosec // G101: a readiness reason, not a credential.
	ReasonListenersUnbound  = "listeners_unbound"
	ReasonDraining          = "draining"
)

// Readyz is the /readyz body: {"status":"ready","revision":"rev-…"} or
// {"status":"not_ready","reasons":[…]}.
type Readyz struct {
	Status   string        `json:"status"`
	Revision string        `json:"revision,omitempty"`
	Reasons  []ReadyReason `json:"reasons,omitempty"`
}

// ReadyReason is one reason a Node is not ready; Detail is non-secret.
type ReadyReason struct {
	Reason string `json:"reason"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// TapEvent is one NDJSON line of /tap per sampled exchange. Header values
// of credential headers are "[REDACTED]"; bodies and query strings never
// appear.
type TapEvent struct {
	Time            string              `json:"time"`
	TraceID         string              `json:"traceId"`
	Listener        string              `json:"listener"`
	Protocol        string              `json:"protocol"`
	Route           string              `json:"route"`
	Method          string              `json:"method"`
	Host            string              `json:"host"`
	Path            string              `json:"path"`
	Status          int                 `json:"status"`
	Code            string              `json:"code,omitempty"`
	DurationMs      float64             `json:"durationMs"`
	RequestBytes    int64               `json:"requestBytes"`
	ResponseBytes   int64               `json:"responseBytes"`
	Upstream        string              `json:"upstream,omitempty"`
	Endpoint        string              `json:"endpoint,omitempty"`
	Consumer        string              `json:"consumer,omitempty"`
	RequestHeaders  map[string][]string `json:"requestHeaders,omitempty"`
	ResponseHeaders map[string][]string `json:"responseHeaders,omitempty"`
}

// TapDropped is the NDJSON line sent before the next delivered event after
// a slow subscriber lost events: {"dropped":N}.
type TapDropped struct {
	Dropped int64 `json:"dropped"`
}

// Snapshot states of /debug/snapshots.
const (
	StateActive  = "active"
	StateRetired = "retired"
	StateClosing = "closing"
	StateEnding  = "ending"
)

// Snapshots is the /debug/snapshots body.
type Snapshots struct {
	Snapshots     []SnapshotInfo `json:"snapshots"`
	Pending       *string        `json:"pending"`
	LastKnownGood *string        `json:"lastKnownGood"`
}

// SnapshotInfo describes one live snapshot; times are RFC 3339 UTC.
type SnapshotInfo struct {
	State       string `json:"state"`
	Revision    string `json:"revision"`
	Digest      string `json:"digest"`
	Pins        int64  `json:"pins"`
	ActivatedAt string `json:"activatedAt"`
	RetiredAt   string `json:"retiredAt,omitempty"`
	GraceEndsAt string `json:"graceEndsAt,omitempty"`
}
```

Tests (WP-01): JSON golden of `Readyz` ready and not ready; `TapEvent` member order; `Snapshots` with and without `pending`.

### 2.16 Resolved conflicts between the area specs

Each resolution is binding for every work package; the reason follows the dash.

| # | Conflict | Resolution |
|---|---|---|
| R-1 | Hub model: 01 treats the hub as typed v1alpha1 aliases; 02 builds a map-based hub | One `hub.Resource` with the normalized positioned `Tree`, the typed `Object` and the Policy `Config` (2.9). The map-based hub is dropped: positions stay available for diagnostics and the typed form for compile. |
| R-2 | Effective chain types: 01 `hub.EffectiveChain`, 02 `precedence.Chain` | `hub.Chain` (core) is the one type; `internal/config/precedence` computes it and owns `Rows()` for `render --effective`. |
| R-3 | Schema navigation: 01 puts `Lookup`/`Walk`/`Info` in `schemaview`; 02 proposes `schemaidx` | `internal/config/schemaidx` (stdlib) owns navigation, keywords, `+ruralz` markers, defaults and dispatch for overlay, substitution, defaults, canonical, diff and render; `internal/config/schemaview` only compiles and validates with jsonschema/v6 and maps errors. |
| R-4 | Field paths: 02 `fieldpath` package versus 01 `diag.Path` | `diag.Path` (with `Item any`) is the only path type; quoting follows 01. |
| R-5 | Import cycle `expr` to `hub` to `filter` over Phase, Class, Scope | New leaf `internal/phase` (2.2). |
| R-6 | CEL placement: 03 `internal/cel` imported everywhere; 01 `CELChecker` interface | Contracts in `internal/expr` (stdlib only); implementation in `internal/cel` plus `internal/cel/celtypes`; 01's `CELChecker` becomes `expr.Compiler.Check(site, src)`; cel-go is confined to `internal/cel/**`. |
| R-7 | Policy registry: 01 `NewConfig` table versus 02 registry API | One `internal/config/registry` with 02's API plus 01's typed config constructor; `filterClass` is materialized for every type per FP 8.1, and an authored different value is RZ-CFG-005. Each type carries a served flag; unserved types fail the serve check of `ruralzd` with RZ-CFG-040 (R-75). |
| R-8 | Stage G split across 01 (defaults) and 02 (conversion, normalization) | One work package (WP-37) owns `defaults` and `convert`. |
| R-9 | Secret types: 01 resolver in `internal/secret`; 06 redacting value in `internal/secret` | Contracts in `internal/secret` (WP-01); providers, poller and fan-out in `internal/secret/resolver`, linked only by `ruralzd`. Redaction marker is `[REDACTED]` everywhere; poll interval 2 s; per-kind size caps 4 MiB default, 16 MiB for CRLs. |
| R-10 | Digest: 02 `revision` versus 06 `signing` | `revision.Digest` is the only digest type; `internal/signing` holds the `Verifier` interface and `DigestOnly` (Sigstore in M2). |
| R-11 | Client address: 04 `listener/proxyproto` and `SourceAddr` versus 06 `clientaddr` | One `internal/clientaddr` (PROXY v2, trusted proxies, forwarding rewrite); 04's packages are not created. |
| R-12 | Per-request State Store deadline origin: 04 start of `onRequestHeaders`; 08 first blocking call | 08 wins: `RequestBudget` starts at the first blocking call (`statestore.RequestBudget.CallTimeout`). |
| R-13 | Filter SPI shape: 04 struct `Exchange` with `Handle(ctx, phase, x)`; 06 interface `Exchange`; 07 message abstraction | `Exchange` is an interface (06) with `Handle(ctx, phase, x)` (04) and `Message` for the current Phase's message (07); body sentinels live in `filter`; `Factory.Build` does not return Phases (chains are the source of truth). |
| R-14 | Consumptive batching: 05 `internal/filter/consume` package | Replaced by `filter.Consumptive` (core) and the batching runner in `internal/gateway/executor`; `internal/filter/consume` is not created. RateLimit fields go through `Exchange.AddRateLimitField`. |
| R-15 | Upstream hook types: 05 own attempt and response types | `snapshot.Forwarder`, `snapshot.LegHooks`, `snapshot.UpstreamResponse`; an attempt is a `filter.Leg`. |
| R-16 | Executor package name: 04 `internal/gateway/chain` | `internal/gateway/executor`; per-request state is the new `internal/gateway/exchange`. |
| R-17 | Snapshot holder and retirer inside `snapshot` (04) | `snapshot` holds data and `Pins` only (core); holder, retirer and ending protocol are `internal/gateway/retire`. |
| R-18 | OQ-traffic-management-and-resilience-2: 05 and 02 (c), 08 (a) | (c): the data plane appends `RateLimit-Policy` and `RateLimit` fields after `onResponse` from the fields Filters recorded (serialized by `internal/sfv`); registry Phases stay as FP 10. |
| R-19 | OQ-scalability-and-distributed-state-3: 05 (a) cache connection; 08 no second connection | (a): `Gateway.spec.stateStore.cache` is a schema field (WP-28); `statestore/manager` keys drivers by `Role`; without it the cache uses the main store with the interim rules and a startup WARN. |
| R-20 | Lua scripts: 05 per-operation scripts; 08 one library | 08's single `ruralz_v1.lua` (sub-operations, 2026 epoch, `%.17g`, `HMGET n g:<V> e:<V>`); `statestore.Call` replaces 05's `ConsumeOp`. |
| R-21 | Default markers | `stateStore.timeout` 50ms, `limits.maxRequestBodyBytes` 10Mi, `limits.maxRequestHeaderBytes` 64Ki, `auth.api-key` `header` `x-api-key`, `telemetry.traceSampling` 0.01, and the OQ-traffic-management-and-resilience-6 static defaults become `+ruralz:default` markers (WP-28). `retryOn` and `failureWhen` stay runtime rules (`expr.DefaultRetryOn`, `expr.DefaultFailureWhen`, `Compiler.Default`). |
| R-22 | New code numbers proposed by several areas | RZ-CFG-038 cache guardrail (02), RZ-CFG-039 listener bind (04), RZ-CFG-040 unserved feature; RZ-RT-016 Drain, RZ-RT-017 hardening (04), RZ-RT-018 only-if-cached (05), RZ-RT-019 `/tap` limit (04); RZ-UP-011; RZ-AUTH-008. 06's duplicate-credential CFG code is deferred (runtime RZ-AUTH-002). |
| R-23 | Access logging of RZ-RT-005 rejections | Counted in `ruralz_http_node_responses_total` with code RZ-RT-005, never access-logged (no Route, no pin, bounded cost); WP-31 amends OBS. |
| R-24 | `/tap` sampling: 04 per-subscriber sample; 09 `{"dropped":N}` line | Both: all requests are offered, each subscriber samples, and a subscriber that falls behind gets one `{"dropped":N}` line (`adminapi.TapDropped`). |
| R-25 | `holder.json` members | 04's members plus `pidNamespace` (10), format `ruralz.holder.v1`. |
| R-26 | `flock` implementation: 04 and 10 via x/sys | `internal/nodedir` uses stdlib `syscall.Flock` (`//go:build linux || darwin`); x/sys is limited to `reuseport`, `handover` (`SO_PEERCRED`) and `testkit/proc`. |
| R-27 | Problem `detail` position | Last member (`title`, `status`, `code`, `requestId`, `detail`). |
| R-28 | Upstream `Host`: 05 Endpoint authority or `sni`; 11 conformance case expected the client authority | 05 wins; the client authority travels in `X-Forwarded-Host`; WP-82 adjusts the case. |
| R-29 | YAML emitters: 01 restricted-profile encoder; 02 render emitter | One emitter, `profile.Encode`, with 02's quoting rule R-45; golden output never depends on a library release. |
| R-30 | Strict JSON: 07 uses `encoding/json/jsontext` | Not allowed under `go 1.26.0` (vet stdversion, verified). `internal/jsonval` is the strict scanner for the profile JSON front end, canonical decode, transforms, `validation.json-schema` instance decode, JWT claims and composition merge; `jsontext` only as a `//go:build go1.27` test oracle. |
| R-31 | Upstream test suites | YAML Test Suite runner in `internal/config/profile` integration tests (data under `test/fixtures/yaml-test-suite`), JSON-Schema-Test-Suite runner in `internal/config/schemaview` tests (data under `test/fixtures/json-schema-test-suite`), keeping library confinement. |
| R-32 | OQ-security-and-identity-1: 01/02 (a) new fields; 06 (b) fixed defaults | (b) for M1: algorithms RS256, PS256, ES256, EdDSA; skew 60 s; `exp` required with at most 24 h lifetime; double Consumer match is 401 RZ-AUTH-002. No schema change, so golden digests do not move; (a)'s fields remain additive later (`+ruralz:open`). |
| R-33 | OQ-traffic-management-and-resilience-11: 05 (a); 08 (c) | (a) when `stateStore.cache` is configured, with 08's interim rules (memory polling, 70% skip, growth rule, per-shard byte cap) always on; all unsafe methods invalidate; `s-maxage` as `proxy-revalidate`. |
| R-34 | Offline Policy checks: 07 `internal/filter/configcheck` injected into 01's validator | `internal/filter/builtin/checks.Table()` is passed to `config/pipeline` by each binary (the CLI links no JOSE or State Store code through it); `internal/config` never imports `internal/filter`. |
| R-35 | Path and host normalization placement (04 handler) | Pure functions in `internal/routematch` (`NormalizePath`, `NormalizeHost`) so the Router, handler, CEL views, access log and the reference-matcher property share them. |
| R-36 | File watcher | Stdlib polling (1 s scan, 2 s settle, 30 s cap, immediate load on root identity or one-file inode change, SIGHUP rescan); no watcher library, no inotify. |
| R-37 | `crl_stale` degraded reason missing from OBS | Added to the catalog (`catalog.ReasonCRLStale`); WP-12 adds its OBS row in wave 2, so the repocheck doc check it introduces passes from the start (every other catalog name already appears in OBS, checked). 06's proposed throttle-refusal metric is not added. |
| R-38 | Telemetry of the CLI | `ruralz` exports no telemetry; it writes diagnostics and command output only. |
| R-39 | Per-request Filter state: 05 keeps quota refunds, `Undo` tokens and cache lookups in unspecified places; Filters are shared per (Policy, scope, Phase set) | `filter.Exchange.PolicyState()`: one slot per Policy per request (per leg for Upstream-scope Policies), holding a pointer to state the Filter pools; the executor selects the slot before every call and `Finish` releases it. The executor's `statestore.Call` is reused and never read after `Complete`. |
| R-40 | Post-response hook: 3.8 and `Tee` put cache stores in `onLog`, but FP 10 registers `cache` for `onRequestHeaders` and `onResponse` only | `filter.Finisher`: called after `onLog` for every Filter that ran, subscribed to `onLog` or not; `cache` stores, invalidates (05 req 85) and releases there. Registry Phases stay FP 10, so `render --effective` output does not change. |
| R-41 | Body and trace handoff to the Upstream layer: 05 `ReplayableBody` and `GetBody`; `filter.Message.Body` is `ErrNotAvailable` unless gated; `gateway/upstream` may not import `gateway/exchange` | `snapshot.Outbound` (exchange, `RequestBody`, trace `Decision`, stripe, deadline, hooks) is the one argument of `Forwarder.Forward`; `gateway/body` implements `snapshot.RequestBody` (05 req 31 replay rule, 05 req 54 step replay of a gated body). |
| R-42 | Leg-scoped exchange: `LegHooks` took only `(ctx, *filter.Leg)`; aggregate legs run concurrently on one pooled exchange | `LegHooks.BeginLeg` returns a `LegRun` per leg; `snapshot.RequestState.Leg` gives each leg a view with its own Leg, Message, Vars copy, `PolicyState` slots, `when` decisions and span; the client request is read-only during forwarding. `RequestState` is a WP-01 contract, so WP-20 and WP-43 code against it independently (WP-43 no longer depends on WP-20). |
| R-43 | Stale-while-revalidate: 05 `cache.Replayer` implemented by `upstream.Manager`, injected at build time, replaying without a snapshot pin | `filter.Replayer` in `BuildEnv`, implemented by `internal/gateway/replay` (WP-66): pins the currently published snapshot for the whole replay, resolves the Route and the Upstream's `LegChain` by name (`Snapshot.LegChains`), runs one leg through `Snapshot.Legs` on a synthetic exchange; a Route or Upstream that disappeared ends the stale window. |
| R-44 | Filter retry request (05 req 33, test plan item 3) with no Result to express it | `filter.Retry` outcome, valid in `onUpstreamResponseHeaders` only; all other retry conditions still apply. |
| R-45 | Node-wide Filter state (06 JWKS manager, token registry, basic throttle; 05 rate-limit key table, `cache.Factory.Run`) without an owner | `filter.Component` (`Name`, `Run`); `filter/builtin.New(Deps)` constructs them once and returns a `*filter.Registry` whose `Components()` the `ruralzd` supervisor (WP-76) runs before the first activation and cancels after the last snapshot retired. |
| R-46 | RZ-CFG-018 to 020 and 038 listed under both stage H and stage I; RZ-CFG-022 under stage H | Stage I (`config/precedence`, WP-38) alone raises RZ-CFG-018, 019, 020, 029 and 038 (02 req 36-38); stage H (`config/validate`) never reports them. RZ-CFG-022 comes from the `--environments` file in stage C (`config/loader`, 01 req 21). |
| R-47 | `pipeline.FromResources` "re-enters at stage H" from canonical bytes, but `config/canonical` cannot build typed resources | `canonical.Decode` returns `[]tree.Resource` (`tree.RoleCanonical`); `FromResources` runs stages F to M on them (schema validation of a Last-Known-Good written by an older release, RZ-CFG-024 for newer content, idempotent defaults, typed decode, H to K), and the recomputed canonical bytes must equal the input (RZ-CFG-027 otherwise). |
| R-48 | `accessLog.when`: WP-11 scoped to evaluate it in `telemetry/accesslog`, 3.8 has the handler decide, `emit.AccessLog` receives no Vars | The handler (WP-66) evaluates `snapshot.Gateway.AccessLogWhen` with the onLog Vars (`duration` set) before `Acquire`; a runtime error writes the entry (`expr.RuleWriteEntry`). `telemetry/accesslog` only pools, truncates and encodes. |
| R-49 | OQ-security-and-identity-22 (a) names "secret-to-destination binding" and "State Store entry MAC key"; 06 defers both (risk 22), 08 req 68 specifies the MAC with a proposed setting | Both are built in M1. Binding: every transmitting `x-ruralz-secret` field has a static destination (`auth.upstream-oauth2` `clientSecret`: the origin of its `tokenUrl`; `stateStore.url` and `stateStore.cache.url`: `state-store`; `AIProvider` `credentials.apiKey`: the origin of its `baseUrl`, transmitted from M3 but assigned by WP-56's stage H already in M1, because the CLI validates `AIProvider` resources (R-75) and RZ-CFG-041 results must not change when M3 serves them), every other secret field is `local` (TLS keys, CA bundles, CRLs, Consumer API keys: "API keys" here always means Consumer API keys); one `secretRef` used with two destinations is RZ-CFG-041 at stage H in every binary (so a Bundle author cannot send a Consumer API key or a TLS key to a `tokenUrl`); the Node sends a value only to its destination (token requests follow no redirect, the State Store dialer dials only the URL's host); a destination change has diff impact `security` (02 R-66). MAC key: `RURALZ_STATE_STORE_MAC_KEY_FILE` (absolute path, owner-only mode, at least 32 bytes, outside `RURALZ_SECRET_ROOT`, read at start; rotation needs a restart and turns entries into misses), parsed by `gateway/setting` (WP-14), passed through `statestore/manager` (WP-65) into `statestore.Deps.MACKey`, HMAC per 08 req 68 in `statestore/keys` (WP-13). An operator-declared destination allow-list is a later extension (`hub.SecretUse.Destination` is the hook). |
| R-50 | Roadmap M1 "digest checks on every Revision and Plugin artifact"; 02 S7 and 06 req 97 wire RZ-CFG-028 for Plugins only in M2 | `signing.VerifyArtifact` (streaming SHA-256 with a size limit, RZ-CFG-028) ships in M1 with tests (WP-17). No Plugin artifact reaches a Node or the CLI in M1: Plugins are RZ-CFG-040 on Nodes and RZ-CFG-028 at `bundle build` without `--offline`, so nothing is ever used unverified; the fetch wiring lands with Plugins in M2. WP-83 records this in the roadmap. |
| R-51 | Roadmap "Docker Compose end-to-end test in file mode" and 09 S17/test 48 (Compose with an OpenTelemetry Collector) versus no Docker daemon (11 risk 1) | The process harness is the end-to-end implementation in every environment (stage 10). A Compose suite (`test/compose`, WP-95) runs the roadmap's file-mode scenarios (quickstart, `--effective --route`, `bundle diff` against `/config/dump`, Zero-Downtime Upgrade) and 09 test 48 against a real OpenTelemetry Collector, plus the air-gapped image start on a Docker `--internal` network, in CI only (nightly and stage 12). WP-83 records the split in the roadmap, WP-32 in the testing strategy. |
| R-52 | Size gate measures a stripped `ruralzd`, release builds are not stripped (11 risk 15) | Stage 4 and stage 12 build with `-s -w` added to the release flags (WP-27 Makefile); `testkit/binaries` uses the same flags; the gated binary is the shipped binary. WP-32 adds the sentence to the release document. |
| R-53 | `testkit/otlpsink` "reuses A9's test collector types", and WP-40 needs the sink to test | `otlpsink` is self-contained on `go.opentelemetry.io/proto/otlp` and grpc (WP-84, wave 2) and imports nothing from `internal/telemetry`; WP-40 tests against it. |
| R-54 | `statestore/manager` importing both drivers | The manager receives one `statestore.Opener` per `DriverKind`; `gateway.Run` passes `memory.Open` and `redis.Open`. The manager (WP-65) no longer waits for the redis driver. |
| R-55 | Secret rotation after carry-over (`Store.Watch` per Store, `Activate` redefining the polled set) | Watches are Node-wide by `Ref` and survive `Activate`; a `Store`'s values follow rotations (2.8). |
| R-56 | `emit.StateMetrics` indexed "by catalog position" while `statestore.OpKind` order differs (`script_multi`, `pipeline` sit before `cache_set`); no stripe reaches Filters, drivers or forwarders | Fixed-size arrays indexed by `emit.StateOp*`, `emit.StateResult*`, `emit.Write*`; explicit `OpKind.Label`, `OpKind.WriteLabel`, `RoundTrip.Label` with a catalog test. The stripe travels in `RequestBudget`, `Exchange.Stripe` and `Outbound.Stripe`. |
| R-57 | Admin server importing `config/canonical` and the Upstream layer | `/config/dump`, `/metrics` and `/debug/upstreams` are injected handlers; WP-45 stays independent of WP-39 and WP-86. |
| R-58 | `emit.Binding` returned by `Meter.Bind` had no home | `snapshot.Snapshot.Binding`. |
| R-59 | Dispatch "keyed by Upstream protocol" with no protocol on the Route | `snapshot.Route.Protocol`, the protocol of the Route's Upstreams. The `ruralzd` serve check rejects every Upstream protocol but `http` with RZ-CFG-040 (R-75), so every Route a Node serves in M1 is `http`; the one-protocol-per-Route rule lands with the M3 handlers. |
| R-60 | `nodedir` uses `syscall.Flock` under `linux || darwin` but the CLI (windows/amd64) links it | `nodedir` keeps paths, `holder.json` and `node.id` platform-neutral; `Lock` has a `unix` implementation and a `!unix` stub returning `errors.ErrUnsupported`; `ruralz node drain` is Linux-only (10 req 93). `make build` cross-compiles `ruralz` for windows. |
| R-61 | `go mod tidy` at wave boundaries drops modules not yet imported | The lead adds every section 5 module before wave 2 together with `internal/tool/modpin/modpin.go` (`//go:build tools`, one blank import per direct module); `go mod tidy` keeps pinned requirements and nothing links them (verified in a scratch copy: `go build`, `go vet`, `go test`, golangci-lint and repocheck all pass with the pin file present). The lead deletes the file before wave 9, once every module has a real importer. |
| R-62 | WP-03 and WP-04 run in parallel with WP-28, which changes schema markers and config shapes | WP-28 is additive only (new fields and markers; no Go type, field or JSON name renamed or removed). WP-03 reads markers from the generated schema at test time and WP-04 keys its table by existing config type names, so both are correct against either schema; the wave-end integration run (convention 7) re-runs them on the merged tree. Exception recorded at the wave 2 boundary: four optional string fields became `*string` with unchanged JSON names, under doc 02 Presence and defaults and 07 req 23: `AuthAPIKeyConfig.Header` (default `x-api-key`, 06 req 15), `ActiveHealthCheck.Path` (default `/`, 05 req 4), `HeaderRequestSet.Value` and `HeaderResponseSet.Value` (no default). WP-37 materializes the two defaults only inside a present parent, so consumers read through the pointer and apply the same default when the pointer, the config or the parent object is nil. WP-48 (`auth.api-key`) uses `x-api-key`; WP-46 maps `healthCheck.active.path` to `health.Policy.Path` as `"/"` when nil. WP-59 (`headers`, including its offline `Check`) treats a nil `Value` as absent, in which case `valueExpression` is set, and a non-nil `""` as an explicit empty field value. |
| R-63 | Decoded-value stop for JSON bodies: 07 req 15 and 58, 03 req 25, 04 req 47, 05 req 47 and `Message.Decoded` stop at 4 times the raw limit in built-value size, which `jsonval` measures at about 3 times the raw bytes for typical documents and up to about 21 times for degenerate arrays (`jsonval.MaxCostPerByte` = 33 bounds it) | The stop stays at 4 times the raw limit the body arrived under, measured as the `jsonval` cost of the values actually built, as docs/architecture/02-configuration-model.md "Body buffering and limits" and its per-request bound of 150 MiB counting decoded values already state; `MaxCostPerByte` times the limit would let one 10 MiB body reserve up to 330 MiB of the 512 MiB `maxBufferedBytes` and void that bound. One constant: `filter.DecodedLimitFactor = 4` (WP-01 contract, additive, cited by `Message.Decoded`). WP-43 (`Message.Decoded` through `expr.Compiler.DecodeJSON`), WP-51, WP-60, WP-64 and the gateway body reservations pass budget = `DecodedLimitFactor` times the raw limit (`maxRequestBodyBytes`, `maxResponseBodyBytes` or the step's `maxBodyBytes`); WP-34's `DecodeJSON` only enforces the budget it is given (`cel` may not import `filter`). Accepted consequence: dense bodies become oversized inside the raw limit (arrays of one-digit numbers above about a quarter of it, arrays of empty objects above about a fifth, short-member objects above about 0.38 of it), while typical documents fit at the full limit. The errors stay those of 07 req 15: 413 RZ-RT-003 on the request, 502 RZ-UP-010 on a plain `upstreams` response, RZ-RT-015 for a composition step; 07 T8 stays as written. |
| R-64 | Number range and canonical numbers: 02 req 16 (RZ-CFG-005 for an integer outside the I-JSON range) versus canonical content, whose RFC 8785 doubles can print as integers above 2^53; YAML 1.2 core float text versus RFC 8259 numbers | `jsonval.AppendCanonicalNumber` is total over finite literals and idempotent: integer literals beyond ±(2^53−1) are rounded through the double path as `encoding/json/jsontext` does (`11e17` becomes `1100000000000000000`), and WP-39's verification step 31(b) relies on that idempotence. 02 req 16 (RZ-CFG-005 "integer outside the I-JSON range", also for literals that overflow a double) is raised only in stage G: after scalar normalization, so a ByteSize normalized above 2^53 is caught, WP-37 calls `jsonval.CheckNumber` on every `KindInt` and `KindFloat` node of resources whose role is not `tree.RoleCanonical`. Canonical content (`canonical.Decode` output re-entering through `pipeline.FromResources`, R-47) is exempt: an integer-looking literal above 2^53 there, such as `123456789012345680000`, is the RFC 8785 form of a double, so neither WP-39's strict decoder nor stage G applies the range rule to it. Float text: `tree.Node.Text` of a `KindFloat` node is in RFC 8259 syntax; WP-33 converts YAML 1.2 core floats such as `.5`, `1.` or `+1.5` textually (drop a leading `+`, put a `0` before a leading `.`, drop a `.` with no fraction digits, strip leading zeros of the integer part keeping one digit), never through `float64`, so `Node.JSONValue`, `CheckNumber` and `AppendCanonicalNumber` accept every float the profile admits. |
| R-65 | PROXY v2 LOCAL with a UDP or UNIX family byte: 04 req 16 closes the connection; the PROXY v2 specification requires receivers to accept LOCAL and discard its protocol block, family included | A LOCAL header is accepted whatever its family/transport byte: its address block is skipped within the 4,096-byte cap and the TCP peer is used. UDP and UNIX families still close the connection with PROXY. This overrides the literal 04 req 16: LOCAL discards the addresses, so accepting it adds no spoofing risk, while refusing it fails proxy health checks that send LOCAL with a non-UNSPEC family. `internal/clientaddr` skips the family check for LOCAL (applied at the wave 2 boundary); WP-82's PROXY cases expect acceptance. |
| R-66 | Class default when a Filter supplies no code: 04 req 43 names only Filter codes for the auth, admission and cache classes | A cannot decide with no Filter code (in M1, a recovered panic, or `filter.Undecided("", err)`) takes the class default, which the executor (`CodeAuthUndecided`, `CodeUndecided`) and `filter/auth` already use: auth 401 RZ-AUTH-002, admission and cache 503 RZ-RT-011, custom (M2) RZ-PLG-001 at 503 or 502. The registered meanings widen instead of adding a code (2.3 prefers reusing codes): RZ-RT-011 "A Policy could not decide under `closed` without a code of its own: `cors`, `validation.json-schema`, `headers`, `transform.*`, or a recovered fault in an admission or cache Policy"; RZ-AUTH-002 "Credential invalid, no matching Consumer, a second binding, or an auth Policy that could not decide without a code", in `internal/errcode` and the 03-data-plane and 08-security-and-identity registry rows, plus one sentence under the 03-data-plane Failure semantics table. |
| R-67 | `Policy.spec.when` runtime error: 03 req 43 asks for an "event on `ruralz.filter.<name>`"; `emit.Span` has no event method | The span is marked instead, as docs/architecture/03-data-plane.md says ("marks the span `ruralz.filter.<name>`"): under `closed` the Policy's call span carries `ruralz.failure_mode=closed` and `error.type` (`cel_error`, or `internal` after a panic); under `open` a span of its own ends with outcome `skipped`, `ruralz.failure_mode=open` and `error.type`. `ruralz_filter_failures_total{phase=<first Phase reached>,mode}` and the access log `failure_modes` are also recorded. `emit.Span` gets no `Event` method in M1: adding a method to the interface breaks `tracing`'s span, `noopSpan` and `noopServer` (WP-10) and the executor fakes; it is revisited in M3 with the `onChunk` span event (docs/architecture/10-observability.md). WP-91 asserts these signals, with no span event. |
| R-68 | Match identity key input: 04 section 3 `MatchKey(*v1alpha1.RouteMatch)`; the 1.2 `routematch` row keys over its own `Criteria` | `routematch.MatchKey(*routematch.Criteria)` stays, so the key is independent of the API types. One shared converter, `routematch.CriteriaOf(*v1alpha1.RouteMatch) Criteria`, uses struct conversions for `PathMatch`, `HeaderMatch`, `GRPCMatch` and `GraphQLMatch`, whose field sets are identical, so a field that diverges breaks compilation; a test compares the field names of `RouteMatch` and `Criteria` by reflection. WP-42 and WP-56 build `Criteria` with it instead of copying fields; `routematch` may import `v1alpha1` (both L0). |
| R-69 | Sampled trace cost: 09 req 38, req 70 and test 44 set 60 allocations for a sampled 15-span trace, which equals the OpenTelemetry SDK floor of 4 per recording span before any Ruralz code; 89 were measured | The target becomes at most 90 allocations: 6 per span, the SDK's 4 (`newRecordingSpan`, `context.WithValue`, the `SetAttributes` slice, the snapshot) plus at most 2 for Ruralz's span wrapper and the server span's share, export excluded. The 30 µs CPU target stays a target on the RH-1 reference hardware, checked in WP-97's runs, not a per-PR gate (42 to 63 µs were measured on a shared 4-vCPU machine). Rejected: keeping 60 by replacing SDK recording spans with a Ruralz recorder, which conflicts with 09 req 1. WP-31 records OQ-observability-22; WP-81 lists `BenchmarkSampledTrace15Spans` (`internal/telemetry/tracing`, owner WP-10) in `allocgate.json` at 90 allocs/op. |
| R-70 | RuralzSeriesFolded: 09 req 72 requires OBS's exact expressions, and also adds the rare-event clause and (with test 43) the first-event-before-first-scrape case to every rule | RuralzSeriesFolded keeps OBS's exact expression `delta(ruralz_telemetry_folded_label_sets[15m]) > 0` (new folding only, since large Bundles fold by design). It is not a rare-event rule, so the `or (x > 0 unless x offset <window>)` clause of 09 req 72 and the first-event-before-first-scrape case of req 72 and test 43 do not apply to it. This matches the committed `deploy/grafana/rules/ruralz.rules.yaml` and docs/architecture/10-observability.md, so no OQ-observability row is needed; WP-31 adds one sentence in OBS "Alert rules". |
| R-71 | Revalidation lease release: 08 req 45 is silent on whether refresh or end-stale release the lease | Neither refresh after a 304 (script f, `LeaseRefresh`) nor end of the stale window (script x, `LeaseEndStale`) deletes the lease. The 5 s revalidation lease, like the 1 s fill lease, only expires (`PX`): it bounds revalidation of a partition to one per 5 s across Nodes (05 req 86, "takes the lease or gives up"), so a Node that read the stale entry before the refresh gives up instead of revalidating again. The cost is bounded: stores of that partition are skipped for the rest of the 5 s (target). The memory driver already behaves this way; WP-41's f and x scripts never `DEL rz:rc:{U:P}:lease`, and WP-88 issues no release. |
| R-72 | Store admission on the memory driver: 08 reqs 53 and 58 and 05 req 84 use N_c = N_published, or 1,000 without one, which admits about 134 bytes per second on the default 64 MiB memory store | The memory driver uses N_c = 1 in store admission, because its store has one writer, its own Node: at the default 64 MiB that admits min(2% × 64 MiB / 10 s, 4 MiB/s) = 134,217 bytes per second, or 1,342,170 bytes per 10 s reading. `redis` keeps N_c = N_published, or 1,000 without one (always 1,000 in M1). This overrides 08 reqs 53 and 58 and 05 req 84 for memory only (`storeNodes = 1` in `internal/statestore/memory`, applied at the wave 2 boundary). WP-31 records the memory driver bounds (08 risk 15) with N_c = 1 in docs/architecture/11-scalability-and-distributed-state.md and states in docs/architecture/09-traffic-management-and-resilience.md "Response caching" rule 3 that N_c applies to shared stores. |
| R-73 | Ending callback order: 04 req 53 cancels the request's root context (step 1) before it moves the deadlines and marks the request ended under `Mu` (step 2) | Steps (1) and (2) swap: under the request's `Mu` the retirer moves the connection deadlines and marks the request ended, then cancels its root context (3.3 step 9). Marking first guarantees that a handler woken by the cancel sees `Ended` when it commits (04 req 38) and writes the end code, not its own cancellation error; the client sees no other difference. `retire`'s `endOne` already does this; docs/architecture/03-data-plane.md "Why no in-flight request is dropped" rule 5 swaps its sub-steps to match. |
| R-74 | Balancer rebuild interval: 05 req 16 rebuilds structures at most every 10 s per Upstream; 05 req 17 lowers v at once and raises it only when it can double | Structures rebuild at most every 10 s per Upstream, with two exceptions: (a) the first structure over a non-empty set, for an Upstream whose set was empty since it was built, is not a rebuild (waiting would serve 503 RZ-UP-008 for up to 10 s); (b) a budget plan that lowers v or starts a fallback builds as soon as a `BuildGate` slot is free, which puts the Node memory budget ahead of the rate limit (req 17 and TMR say "lower v at once"). The limit of 8 builds at a time per Node still applies. A ring leaving fallback counts as a raise under req 17's "raise only when it can double": it returns to `ring-hash` only when the fresh plan gives it V ≥ 2 × 64 = 128 virtual nodes per Endpoint, and between 64 and 127 it stays on weighted random, which avoids fallback flapping. WP-31 states both rules in TMR "Load balancing"; no OQ row is needed. |
| R-75 | RZ-CFG-040 placement: section 0 item 8 and 3.1 put it at stages H and K, which run in every binary, so the verbatim example Bundle (`plugin` Policies, gRPC and `ai` Upstreams, `ai.*` types) would fail `ruralz bundle validate` and `ruralz bundle build`; 01 S1 and risk 28, 03 S19 and req 37, 11 req 13 and 106 and risk 3, 02 req 52 and 79 need its digest, golden entry, `--effective` table and CEL checks through the CLI | Only the pipeline run of a serving Node raises RZ-CFG-040: `pipeline.Options.Serving`, set only by `ruralzd` (activation, and Last-Known-Good and handover through `FromResources`), adds the serve check after stage M, so the offline diagnostics, the canonical bytes and the digest are identical in every binary (3.1). The `ruralz bundle` commands (`validate`, `render`, `diff`, `build`) never raise it, so the verbatim `examples/shop-bundle` runs stages A to M in the CLI and keeps its digest, its `--effective` table and its CEL checks. This also follows 05 req 1 (refused at activation), R-50 and the configuration model's meaning "this Node release". The complete M1 list is section 0 item 8. WP-56 builds the check as a separate pass, WP-69 adds `Options.Serving`, WP-73 sets it, WP-74 and WP-75 never set it, and WP-79's cross-binary diagnostics equality (11 req 16) uses M1-servable fixtures or excludes RZ-CFG-040. |

## 3. Data flow

### 3.1 Configuration pipeline (every binary)

`config/pipeline.Run(ctx, Options) (*hub.Validated, diag.List)` runs stages A to M in order, then the serve check when `Options.Serving` is set; each stage stops the run when it produced an error diagnostic, except that stages F and H collect every finding of their stage first (01 section N). `ruralz bundle validate|render|diff|build`, `ruralzd` activation and the conformance harness call the same entry with the same `Options` (only `Variables` differ by binary, and `Serving`, which only `ruralzd` sets (R-75); every binary passes `checks.Table()` as `Options.Checks` and its `expr.Compiler`).

| Stage | Package | Input to output | Codes |
|---|---|---|---|
| A discover | `config/loader` | `RURALZ_CONFIG` or CLI path, `.ruralzignore`, limits (files, bytes, depth) to file list | RZ-CFG-001 |
| B parse | `config/profile` | bytes to `tree.Node` per document (restricted YAML 1.2 or strict JSON via `jsonval`) | RZ-CFG-001 to 004 |
| C base union | `config/loader` | documents to `[]tree.Resource`, identity checks, Environments for CLI renders (`--environments`: RZ-CFG-022 promotion cycles) | RZ-CFG-007, 008, 016, 017, 022 |
| D overlay | `config/overlay` | strategic merge and `$patch` | RZ-CFG-008, 030 |
| E substitution | `config/subst` | `${VAR}` with re-typing and escaping (process environment in `ruralzd`, Environment files in the CLI) | RZ-CFG-010, 011, 013 |
| F schema | `config/schemaview` | rendered view validation with `x-ruralz-*` vocabulary, error mapping to `diag.Path` | RZ-CFG-005, 006, 012 |
| G defaults and hub | `config/defaults`, `config/convert` | markers materialized inside present parents, normalization, decode into `v1alpha1` and registry config types, `hub.Bundle` | RZ-CFG-005 (decode; number range on non-canonical trees, R-64) |
| H references | `config/validate` | references, cross-resource rules, identity static checks, secret uses with destinations, injected `hub.Checks` | RZ-CFG-005, 009, 021, 023, 031, 032, 035, 036, 037, 041 |
| I chains | `config/precedence` | `hub.Chain` per Route (client Phases, legs, removals) | RZ-CFG-018, 019, 020, 029, 038 |
| J CEL | `config/validate` via `expr.Compiler.Check` | every CEL field at its `expr.Site` (sites need chains) | RZ-CFG-014, 015 |
| K effective-chain rules and Plugin config | `config/validate` | RZ-CFG-034 over each Route's `hub.Chain` (01 J row 034, SHOULD in M1, gated by 01 req 51); `plugin` Policy config hook kept for M2 | RZ-CFG-034 |
| L canonical | `config/canonical` | `ruralz.canonical.v1` bytes (RFC 8785 over the materialized tree) | none |
| M digest | `config/revision` | `sha256` over the canonical bytes, `rev-<12 hex>` | none |
| serve (`ruralzd` only) | `config/validate` | unserved features (the M1 list of section 0 item 8), run after the digest and only when `Options.Serving` is set, so the offline diagnostics stay identical in every binary (R-75) | RZ-CFG-040 |

RZ-CFG-001 (A, B), 005 (F, G, H) and 008 (C, D) are raised by several stages for different rules; R-46 fixes the codes only stage I raises. Stage E raises RZ-CFG-010, 011 and 013, never 012: inside an `x-ruralz-secret` subtree only RZ-CFG-011 is reported (01 req 28), and 012 stays at stage F only (schema mapping keyed on `x-ruralz-secret`, 01 S8 and risk 6). RZ-CFG-013 is raised at stage E only: WP-36's `config/subst` owns both detectors (secret-like variable names, and the credential-shaped literal list of the 01 J row for 013). This overrides 01 J's placement, because `subst` already walks every string with its `schemaidx` position (`InSecret`, `x-ruralz-cel`); content entering through `FromResources` skips A to E and needs no warning.

Secrets are not resolved in the pipeline: stage H records every `secretRef` as a `hub.SecretUse` with its `secret.Kind` and destination (R-49); `ruralzd` resolves them in activation (3.3), the CLI never does. `pipeline.FromResources` (Last-Known-Good boot and handover candidates) decodes `ruralz.canonical.v1` bytes into `[]tree.Resource` with `config/canonical` (RZ-CFG-024 for content from a newer producer, 02 R-33), runs stages F to M on them, then requires the recomputed canonical bytes to equal the input and the recorded digest (RZ-CFG-027 otherwise) (R-47); `ruralzd` sets `Serving` on these runs too, so the serve check applies to Last-Known-Good and handover candidates (R-75).

### 3.2 File-mode boot (`ruralzd`)

1. `cmd/ruralzd` builds the signal context; `gateway.Run` parses `RURALZ_*` settings (`gateway/setting`, including `RURALZ_STATE_STORE_MAC_KEY_FILE`), creates the telemetry `Runtime` (resource, stdout log worker, `/metrics` producer; OTLP pipelines start once a Revision names an endpoint), and opens the data dir (`nodedir`): creates `node.id` (ULID) on first start, takes `lock` with `flock`.
2. Lock busy: the Node is a handover successor (3.7). Lock free: write `holder.json` (`ruralz.holder.v1`, PID, start time, version, `pidNamespace`).
3. Construct the Node-wide parts: State Store manager (openers for `memory` and `redis`, MAC key), secret resolver, upstream manager, `filter/builtin.New(Deps)` (factories and `filter.Component`s: JWKS manager, token registry, basic throttle, rate-limit key table, cache revalidation workers), the replay service, the CEL compiler. The supervisor starts every `Component.Run`, the resolver poller, the upstream scheduler and the post-commit writers, each on one owned goroutine.
4. Start the admin server on `admin.port` from the LKG pointer or default 9901 (reuseport-bound, CBPF self-steering on Linux); `/healthz` 200, `/readyz` 503 `no_revision`.
5. Boot order (04 req 58): a valid Bundle at `RURALZ_CONFIG` activates through 3.3; otherwise the Last-Known-Good (`gateway/lkg`: pointer `lkg.json`, content `sha256-<hex>.json`, digest verified, RZ-CFG-027 on mismatch) activates through `pipeline.FromResources` with `ruralz_node_degraded_info{reason="lkg_boot"}` until a source Revision activates; otherwise not ready while the watcher keeps polling. `sd_notify READY=1` (with `STATUS=` carrying the readiness reason) follows the first boot attempt, ready or not (11 risk 9).
6. Activation binds every client listener with `SO_REUSEPORT` (bind, CBPF attach, listen), publishes the snapshot, writes the candidate and LKG pointers, and `/readyz` turns 200 when a Revision is active, every `secretRef` it uses resolved and every listener bound.
7. The watcher (`gateway/source`) polls every 1 s (R-36); SIGHUP forces a rescan.

### 3.3 Activation and Hot Reload

One loader goroutine (`gateway/reload`) owns activation; a pending slot keeps only the latest candidate.

1. **Trigger**: watcher change, SIGHUP, boot, handover `reload` message.
2. **Gate**: activate only when fewer than K = 2 snapshots are retired, or none is closing or ending (04 req 54); otherwise wait, never cutting grace short. With "or", the activation that makes the (K + 1)th retirement can run, and at most one snapshot is closing or ending at a time (`retire.Holder.canActivateLocked`; the loader calls `Holder.WaitActivate` before compiling the latest pending Revision).
3. **Validate**: `pipeline.Run` (or `FromResources` for LKG/handover) with `Checks = checks.Table()`, `Serving` set (RZ-CFG-040 for unserved features, R-75) and the process environment as variables. A failure keeps the active Revision, logs the diagnostics, counts `ruralz_config_activations_total{result="rejected",code}`.
4. **Resolve secrets**: `resolver.Resolve(ctx, uses)` gives a `secret.Store` for the Revision; an unresolvable reference is RZ-CFG-026 and rejects it (at cold start, retried with backoff while `/readyz` reports `secrets_unresolved`).
5. **Compile** (`gateway/compile`, W = max(1, `GOMAXPROCS`/2) workers yielding every 100 µs): Consumers (`identity`), CEL programs (`expr.Builder` seeded with the previous `ProgramSet`), one Filter per (Policy, scope, Phase set) through `filter.Registry` with `BuildEnv.Previous` for carry-over and `BuildEnv.Replayer` for `cache`, Routers per listener, forwarders (`gateway/upstream/forward` per-snapshot set and `LegRunner`, `gateway/composition`), `LegChains` by Upstream, `Route.Protocol`, listener TLS, metric `Plan` (`emit.Meter.Admit`), State Store handles (`statestore/manager`, main and cache roles). Pools, Filter instances and token buckets with the same identity and canonical config are carried over; only new ones warm.
6. **Bind**: added or changed listener keys (`snapshot.ListenerKey`) are bound through `listener.Binder`; a bind failure is RZ-CFG-039 and the active Revision keeps serving. TLS and hostname changes never rebind (`GetConfigForClient` reads the published snapshot through the holder).
7. **Publish**: `snapshot.Binding = meter.Bind(plan)`, then `retire.Holder.Publish` swaps the `atomic.Pointer`; the old snapshot becomes retired and its `Binding.Retire()` runs; the resolver `Activate`s the new secret table (watches are kept, R-55).
8. **Record**: candidate then LKG pointer written (temp file, `fsync`, `rename`, directory `fsync`, content before pointer); `ruralz_config_revision_info{role="active"|"lkg"}`; `ruralz_config_activation_duration_seconds`.
9. **Retire**: at zero pins the retirer calls `Binding.Release()` and closes resources no newer snapshot shares (`filter.Closer`, which also stops secret watches; pools; `StoreHandle.Release`). A third retirement makes the oldest *closing*; after 30 s grace it is *ending*: the retirer walks its pinned requests (`Pins.Each`). For each it moves the connection deadlines and marks the request ended under the request's `Mu` (uncommitted: read deadline in the past and write deadline 5 s ahead, so the handler writes 503 RZ-RT-014; committed: both in the past, so the stream resets), then cancels its root context (R-73).

Secret rotation is not a reload: the resolver polls every 2 s and fans out through its Node-wide watch table; TLS certificates and keys are read per handshake, credential indexes swap copy-on-write, and carried-over Filters keep their watches.

### 3.4 Request path

`gateway/handler.ServeHTTP` (04 req 34), with the executor and exchange:

1. Take an in-flight unit (`admission`); full: 503 RZ-RT-005 at once (counted, not logged, R-23).
2. Pin the active snapshot (stripe from the connection, load, increment, re-load) and register the `PinnedRequest`.
3. Trace context and `requestId` (`emit.Tracer.Decide`, W3C `traceparent`), server span when sampled.
4. Header size and count (431 RZ-RT-002) and framing. Before normalization, a CONNECT request (HTTP/1.1, or HTTP/2 extended CONNECT) gets 404 RZ-RT-001 (04 req 22); otherwise its empty path would normalize to `/` and match a prefix `/` Route. Path normalization is `routematch.NormalizePath(routematch.RequestPath(r.URL))`, which supersedes 04 req 23's input `r.URL.EscapedPath()`: on Go 1.27.1 `EscapedPath` re-escapes the decoded `Path` when the raw path holds a byte `net/url` will not leave unescaped (`{`, `|`, `"`, non-ASCII), so `/a%2Fb/{x}` would become `/a/b/%7Bx%7D` and the encoded slash would split a segment; `RequestPath` returns `u.RawPath` when set and `EscapedPath` otherwise, without allocating. A `NormalizePath` error is answered with the code and status it carries (`errcode.CodeOf`, `errcode.Status`): errors matching `routematch.ErrRejected` are 400 RZ-RT-017, and `routematch.ErrAsteriskForm` (`OPTIONS *`) is 404 RZ-RT-001. Then host (`routematch.NormalizeHost`) and listener `hostnames` (404 RZ-RT-001), `source.ip` (`clientaddr`: PROXY v2 peer, LOCAL accepted whatever its family (R-65), trusted proxies, forwarding headers). Every rejection in this step is a pre-routing response (04 req 20: 5 s write deadline, no Filter Chain).
5. Route match (`snapshot.Router.Match`, span `ruralz.route.match`): no match 404 RZ-RT-001, `match.when` runtime error 500 RZ-RT-006. Pre-routing responses run no Filter Chain (5 s write deadline, at most 2,000 concurrent).
6. Dispatch by `Route.Protocol` (only `http` in M1). Route deadline `t0 + timeout`; declared `Content-Length` over `maxRequestBodyBytes`: 413 RZ-RT-003 without reading.
7. The exchange is initialized from the pool (`gateway/exchange`, implementing `snapshot.RequestState` and `filter.Exchange`), and the executor runs the client Phases from `snapshot.Route.Chain`, calling `RequestState.Enter` before every Filter call so `Message`, `Vars`, `PolicyState` and `Annotate` refer to that Policy:
   - Each Policy's `when` is evaluated once before its first Phase and recorded with `SetWhen` (skip on false; runtime error: closed runs, open skips); all auth-class Policies skipped gives 401 RZ-AUTH-001.
   - `onRequestHeaders`: in class order (cors, auth, authz, admission, validation, cache, upstream-auth, transform, custom), then scope (Gateway, Route, Upstream), then position. A run of consecutive `Consumptive` admission Policies is batched: `Prepare` each (the Filter keeps its taken tokens or reservation in `PolicyState`), one `Store.Consume` round trip for all prepared calls (per-request `RequestBudget`, which carries the stripe), `Complete` each, `Undo` on an earlier deny.
   - `onRequestBody` when gated (`BodyNeeds.RequestGate`: body buffered from the budget; 413 RZ-RT-003, 503 RZ-RT-004).
   - `onRoute`.
   - Result handling per Filter: `Continue`; `Respond` short-circuits to `onResponse`, `onLog` and `Finish`; `CannotDecide` applies `failureMode` (04 req 43 table) with the Filter's code or the class default (R-66); a `closed` 401 of an auth-class Policy carries that Policy's own `WWW-Authenticate` challenge when its Filter has a non-empty one (06 req 8; sentinel failures and authz 403s carry none); `ErrBudget` is 503 RZ-RT-004 under either mode; `Retry` outside `onUpstreamResponseHeaders` is `CannotDecide`.
8. Forward: the handler fills a pooled `snapshot.Outbound` (exchange, `RequestBody` from `gateway/body`, trace `Decision`, stripe, Route deadline, the executor's `LegHooks`) and calls `Route.Forward.Forward(ctx, o)`: plain `upstreams` (3.5) or composition (3.6). Before any attempt, an expired Route deadline is 504 RZ-RT-007.
9. `onResponse` runs in reverse order on the response (generated responses included, except `transform.response` and `cache`); a failure before commit is 502 RZ-RT-012 (closed) or skip (open); `cache` may start its tee (`TeeResponse`) or replace an error with a stale variant (`ReplaceResponse`, stale-if-error). The handler appends the recorded RateLimit fields (`sfv`), writes a problem document for any `filter.Response` with a `Code` (`problem.Write`), then commits under the `PinnedRequest.Mu`: an ended request writes 503 RZ-RT-014 (grace end) or RZ-RT-016 (Drain deadline) instead.
10. The body streams through one pooled 32 KiB buffer per direction unless gated or teed (`BodyNeeds`); after commit an Upstream failure resets the stream (RZ-UP-009).
11. `onLog` and `Finish` (3.8), then unpin, release the unit.

### 3.5 Upstream legs (plain `upstreams`)

`gateway/upstream/forward` implements `snapshot.Forwarder` for a Route's weighted `upstreams` on top of the attempt loop in `gateway/upstream`:

1. Pick the Upstream leg by weight (every weight 0: 503 RZ-UP-008); `hooks.BeginLeg(ctx, upstream, "")` gives the leg its `LegRun` and leg view; take a bulkhead slot and check the circuit breaker (`resilience`); open breaker or full bulkhead: 503 RZ-UP-005/006; empty Endpoint set: 503 RZ-UP-008.
2. Evaluate `hashKey` once per leg (`ring-hash` only) with the Base variables of `Outbound.X.Vars()`; a runtime error picks a weighted random Endpoint and counts `ruralz_upstream_cel_errors_total{field="hashKey"}` (05 req 14); retries reuse the key.
3. Per attempt: select an Endpoint (`balance`, skipping ejected ones from `health`), build the outgoing request (hop-by-hop removed, forwarding headers per `clientaddr`, `Host` = Endpoint authority or `sni` per R-28, `Tracer.Inject` with `Outbound.Trace`, body from `Outbound.Body.Open`, `GetBody` only when `Buffered` is non-nil), and fill a `filter.Leg`.
4. `LegRun.OnUpstreamRequest(ctx, leg)` runs the leg's `onUpstreamRequest` chain on the leg view (upstream-auth, Upstream-scoped transforms and headers); a non-nil response ends the leg without retry and becomes the response.
5. Dial through the guarded dialer (`egress`) and pool (`http.Transport` per Upstream, TLS from `tlsconf`), per-try timeout, deadline propagation.
6. `LegRun.OnUpstreamResponseHeaders(ctx, leg)` (reverse order) may ask for a retry (a Filter returned `Retry`) or replace the response (502 RZ-RT-012).
7. Error classification (`connect`, `timeout`, `reset`, `tls`), `retryOn` (`expr.Program`, default `expr.DefaultRetryOn`) or a Filter retry request, replayable body (`RequestBody.Replayable`), retry budget, backoff full jitter 0 to 250 ms; `failureWhen` feeds the breaker and passive ejection.
8. `LegRun.OnUpstreamResponseBody(ctx, leg)` once per leg when subscribed (response gate; over `maxResponseBodyBytes` is 502 RZ-UP-010); `LegRun.End` runs the leg Filters' `Finish` and releases the leg view.
9. Return `snapshot.UpstreamResponse` (streaming `Body` whose `Close` releases the bulkhead slot) or an error carrying RZ-UP-001 to RZ-UP-008.

`Snapshot.Legs` (`LegRunner`, same package) runs steps 1 to 9 for one named Upstream without a weighted plan; replay (3.9) uses it.

### 3.6 Composition

`gateway/composition` implements `snapshot.Forwarder` for Routes with `composition`:

1. Steps run in the Route's mode within `maxCompositionSteps`: `aggregate` runs steps in parallel and merges bodies under `group`; `sequential` runs them in list order, exposing earlier results as `steps`; `conditional` runs the first step whose `when` is true (none: 404 RZ-RT-010). Each parallel step takes its own in-flight unit and buffer reservations.
2. Each step is an Upstream leg through the same attempt loop as 3.5 with its own `LegRun` from `hooks.BeginLeg(ctx, upstream, step)` (`filter.Leg.Step` set); concurrent `aggregate` steps never share a leg view (R-42), and the client request is read-only while they run. A gated client body is replayed to every step that sends one (05 req 54); each step is bounded by its `maxBodyBytes`.
3. Step results populate `expr.Steps` for later steps and the merge; a non-2xx merged or non-final step is 502 RZ-UP-011; a CEL runtime error or body over its cap is 502 RZ-RT-015.
4. Optional steps that fail leave the response `Partial` (`ruralz-partial: true`); the merge builds the JSON body with `jsonval` and returns it as a generated `UpstreamResponse`.

### 3.7 Drain and Zero-Downtime Upgrade

SIGTERM or SIGINT (`gateway/drain`): at 0 s `/readyz` 503 `draining`, `STOPPING=1`, lock and `holder.json` released, watcher and loader stop; 5 s `Server.Shutdown` on client servers (one GOAWAY, `Connection: close`); 25 s Drain deadline ends remaining requests through the ending protocol with RZ-RT-016; 25 to 30 s the post-commit queue and telemetry flush; the supervisor cancels the `filter.Component`s after the last snapshot retired; exit 0 by 30 s. A second signal jumps to the deadline.

Handover (`gateway/handover`, Linux): the successor finds the lock busy, loads the holder's candidate through 3.3's gate, binds every client port and the admin port with `SO_REUSEPORT` without listening, and speaks `ruralz.handover.v1` over `handover.sock` (peer UID checked with `SO_PEERCRED`): `ready`, `refused`/`reload`/`accepted` with 1 s usage reports, `listening`, steering swap, 3 s linger, `released`; the holder then drains with the handover timeline (`gateway/drain`). Shared ceilings subtract the holder's reported usage. As successor, after taking the lock, the Node sends `MAINPID=<own pid>` then `READY=1`.

### 3.8 onLog, Finish and telemetry

`onLog` runs every subscribed Policy in request order, read-only, after the response finished or the client went away (`filter.Final` gives status, code, commit and whether a later Policy rejected); quota refunds use the reservation kept in `PolicyState` and go to the post-commit queue (`statestore.Enqueuer`), never blocking the request. Then the executor calls `Finish` on every Filter that ran and implements `filter.Finisher`, in request order: the Response Cache stores the teed body (`Tee.Result`, `statestore.CacheStore`), queues the generation bump of a successful unsafe method (05 req 85) and releases a hit's reservation; Filters release their `PolicyState`. Then the handler:

1. Records metrics through the snapshot-bound handles (`emit.RouteMetrics`, `emit.ListenerMetrics`, per-Policy `emit.PolicyMetrics`) on the request's stripe (`ruralz_http_listener_requests_total` with `emit.OriginOf(code, fromUpstream)`; `gateway_duration` through the exchange's `emit.GatewayTimer`, reset at request start); aggregates collect at scrape and OTLP export through the `sdkmetric.Producer`.
2. Ends the server span (filter spans ended per call; `onLog` creates none).
3. Evaluates `accessLog.when` (`snapshot.Gateway.AccessLogWhen`, onLog Vars with `duration`; a runtime error writes the entry, R-48) and, when selected, fills a record (`emit.AccessLog.Acquire`/`Submit`); a bounded worker encodes JSON to stdout or OTLP logs.
4. Publishes a `/tap` event to the hub (each subscriber samples; laggards get `{"dropped":N}`).

### 3.9 Response Cache revalidation (stale-while-revalidate)

A stale hit within `stale-while-revalidate` is served at once; the `cache` Filter enqueues the saved request (credentials stripped, 05 req 86) on the Node-wide revalidation pool (`filter/cache/revalidate`, a `filter.Component`: 256-entry queue, 4 workers, drop when full). A worker takes the partition lease (`SET NX PX 5000` through `statestore.CacheLease`), then calls `BuildEnv.Replayer.Replay(ctx, route, upstream, req)`: `gateway/replay` pins the currently published snapshot, finds the Route and `Snapshot.LegChains[upstream]`, builds a synthetic exchange (no client, no identity) and runs one leg through `Snapshot.Legs` with the executor's leg hooks, so upstream-leg Policies, breaker and bulkhead apply and the snapshot cannot retire under the replay. 304 refreshes the variant's metadata under the lease; any other result deletes the variant; a missing Route or Upstream ends the stale window. Neither refresh nor end-stale releases the lease, which expires after its 5 s (R-71).

## 4. Work packages

### 4.1 Rules

1. **Exclusive ownership.** A work package writes only inside the paths it owns (`dir/` means the directory and everything below it; `dir/*.go (package root only)` means the files directly in that directory, not its subdirectories; a file path means that file). No two work packages of one wave share a path, and none owns a path inside another same-wave package's directory. A later wave may own a path an earlier wave owned only for documents (a later document owner edits only the sections its row names).
2. **Dependencies.** A work package depends only on work packages of earlier waves and starts when they are merged; every package sits in the earliest wave its dependencies allow (computed). Wave 1 is WP-01 alone. A package's "Done when" is reachable with its dependencies merged, never waiting for a same-wave package.
3. **Nobody edits** `go.mod`, `go.sum` or `internal/tool/modpin` (the lead adds section 5 up front, R-61), a WP-01 directory after wave 1 (contract changes go through the lead between waves), the `Makefile`, `.gitattributes`, `pr-fast.yml`, `pr-full.yml`, `main.yml` or `nightly.yml` after WP-27 (WP-27 defines every stage job and `make` target of M1, each tolerant of suites that do not exist yet), `internal/errcode` after WP-01, or a document outside WP-01, WP-12 (one OBS row), WP-29 to WP-32, WP-83 and WP-98. Later CI jobs that need their own trigger or runner (T5 live systemd, chaos at scale, image checks, release) live in workflow files owned by their work package and expose `workflow_call`, so `release.yml` (WP-96) calls them.
4. **Definition of done** for every work package: the section 0 conventions; `make lint test` green with `-race`; the package's spec requirements implemented with tests that cite requirement numbers; fuzz targets and benchmarks named by its spec present; no goroutine without an owner and a stop path; no new third-party import beyond section 5 and the confinement rules of 1.3; the row's "Done when".
5. **Sizes.** S up to about 1,000 lines with tests, M up to about 3,000, L up to about 6,000; nothing is XL (split instead). WP-01's size counts what it writes (tests, lint block, document rows), not the pasted section 2 sources.
6. **Integration environment.** No Docker here: integration tests use local `redis-server` 7.0.15 through `redisserver` (`process` flavor); container flavors, live systemd and Docker Compose run in GitHub Actions; RH-1 runs need the hardware of WP-97. End-to-end suites use the process harness. Network access is limited to proxy.golang.org.
7. **Maintainer packages.** WP-97 (RH-1 provisioning) and WP-98 (repository settings and release cut) need repository administration and funding; the lead or a maintainer executes them, and each lists the evidence that closes its exit criteria.

### 4.2 Waves

| Wave | Theme | Work packages |
|---|---|---|
| 1 | Core contracts (alone) | WP-01 (L) |
| 2 | Leaves, primitives, infrastructure, executor, schema, test kit base and mocks, documents | WP-02 (M), WP-03 (M), WP-04 (M), WP-05 (S), WP-06 (M), WP-07 (M), WP-08 (M), WP-09 (L), WP-10 (M), WP-11 (M), WP-12 (S), WP-13 (L), WP-14 (S), WP-15 (M), WP-16 (M), WP-17 (S), WP-18 (S), WP-19 (M), WP-20 (L), WP-21 (M), WP-22 (M), WP-23 (M), WP-24 (M), WP-25 (M), WP-26 (L), WP-27 (L), WP-28 (M), WP-29 (M), WP-30 (M), WP-32 (M), WP-83 (S), WP-84 (M) |
| 3 | Configuration stages, CEL, data-plane components, upstream runtimes, non-CEL Filters, State Store test servers | WP-31 (M), WP-33 (L), WP-34 (L), WP-35 (M), WP-36 (M), WP-37 (M), WP-38 (M), WP-39 (M), WP-40 (L), WP-42 (M), WP-43 (L), WP-44 (M), WP-45 (M), WP-46 (L), WP-47 (L), WP-48 (M), WP-49 (M), WP-50 (M), WP-51 (M), WP-52 (M), WP-53 (S), WP-54 (M), WP-65 (M), WP-85 (L), WP-88 (M) |
| 4 | Configuration assembly, CEL Filters, cache, composition, forwarder, handler, drain, redis driver, topology | WP-41 (L), WP-55 (M), WP-56 (L), WP-57 (M), WP-58 (M), WP-59 (M), WP-60 (L), WP-61 (L), WP-62 (M), WP-63 (L), WP-64 (L), WP-66 (L), WP-67 (M), WP-68 (M), WP-86 (M), WP-87 (M) |
| 5 | Pipeline facade, snapshot compiler, built-in tables, handover | WP-69 (M), WP-70 (L), WP-71 (S), WP-72 (S), WP-89 (L) |
| 6 | Activation and Hot Reload, CLI bundle, configuration conformance | WP-73 (L), WP-74 (L), WP-75 (L) |
| 7 | Binaries wiring and cross-package properties | WP-76 (L), WP-77 (L), WP-78 (M) |
| 8 | End-to-end, Node integration, chaos, benchmarks, protocol conformance, T5, image checks | WP-79 (L), WP-80 (L), WP-81 (L), WP-82 (M), WP-90 (L), WP-91 (M), WP-92 (M), WP-93 (M), WP-94 (M), WP-95 (M), WP-99 (M) |
| 9 | Release pipeline and RH-1 budget runs | WP-96 (L), WP-97 (M) |
| 10 | Release candidate and 0.1.0 cut | WP-98 (S) |

99 work packages. Every work package sits in the earliest wave its dependencies allow; the longest dependency chain is WP-01, WP-02, WP-33, WP-55, WP-69, WP-73, WP-76, WP-79, WP-96, WP-98 (10 waves).

### 4.3 Summary

| ID | Title | Wave | Size | Depends on |
|---|---|---|---|---|
| WP-01 | Core contracts, lint rules and code registrations | 1 | L | none |
| WP-02 | Strict JSON value library | 2 | M | WP-01 |
| WP-03 | Schema index | 2 | M | WP-01 |
| WP-04 | Policy type registry | 2 | M | WP-01 |
| WP-05 | Route match primitives and request normalization | 2 | S | WP-01 |
| WP-06 | Identity, Consumer index and auth helpers | 2 | M | WP-01 |
| WP-07 | Secret resolver | 2 | M | WP-01 |
| WP-08 | CEL type adapters | 2 | M | WP-01 |
| WP-09 | Telemetry aggregates | 2 | L | WP-01 |
| WP-10 | Tracing | 2 | M | WP-01 |
| WP-11 | Process logs and access logs | 2 | M | WP-01 |
| WP-12 | Telemetry gates, alert rules and dashboards | 2 | S | WP-01 |
| WP-13 | State Store base: breaker, keys, memory driver, test fake | 2 | L | WP-01 |
| WP-14 | Process identity, data dir and settings | 2 | S | WP-01 |
| WP-15 | Client address | 2 | M | WP-01 |
| WP-16 | Egress guard and TLS configuration | 2 | M | WP-01 |
| WP-17 | Admin authentication, redaction and digest checks | 2 | S | WP-01 |
| WP-18 | HTTP field rules and structured fields | 2 | S | WP-01 |
| WP-19 | Admission and body buffering | 2 | M | WP-01 |
| WP-20 | Filter Chain executor | 2 | L | WP-01 |
| WP-21 | Snapshot holder and retirement | 2 | M | WP-01 |
| WP-22 | Endpoint balancing | 2 | M | WP-01 |
| WP-23 | Discovery and health | 2 | M | WP-01 |
| WP-24 | Resilience primitives | 2 | M | WP-01 |
| WP-25 | CLI framework and completion | 2 | M | WP-01 |
| WP-26 | Test kit base | 2 | L | WP-01 |
| WP-27 | CI pipelines, make targets and gates | 2 | L | WP-01 |
| WP-28 | Schema and configuration types | 2 | M | WP-01 |
| WP-29 | Docs: configuration model, foundation pack, system overview | 2 | M | WP-01 |
| WP-30 | Docs: data plane, security, zero-downtime, CLI reference, topologies | 2 | M | WP-01 |
| WP-32 | Docs: engineering, performance, vision, features, ADRs | 2 | M | WP-01 |
| WP-83 | Docs: roadmap amendments for M1 | 2 | S | WP-01 |
| WP-84 | Test kit mocks and OTLP sink | 2 | M | WP-01 |
| WP-31 | Docs: traffic, observability, scalability | 3 | M | WP-01, WP-12 |
| WP-33 | Restricted YAML and JSON profile | 3 | L | WP-01, WP-02 |
| WP-34 | CEL module | 3 | L | WP-01, WP-02, WP-08 |
| WP-35 | Schema validation view | 3 | M | WP-01, WP-03, WP-28 |
| WP-36 | Overlays and substitution | 3 | M | WP-01, WP-03 |
| WP-37 | Defaults, normalization and conversion | 3 | M | WP-01, WP-02, WP-03, WP-04, WP-28 |
| WP-38 | Precedence and effective chains | 3 | M | WP-01, WP-04 |
| WP-39 | Canonical form | 3 | M | WP-01, WP-02 |
| WP-40 | Telemetry runtime | 3 | L | WP-09, WP-10, WP-11, WP-16, WP-28, WP-84 |
| WP-42 | Router | 3 | M | WP-05 |
| WP-43 | Per-request exchange | 3 | L | WP-15, WP-18, WP-19 |
| WP-44 | Listeners, servers and SO_REUSEPORT | 3 | M | WP-15, WP-16, WP-19, WP-21 |
| WP-45 | Admin server, tap and readiness | 3 | M | WP-17 |
| WP-46 | Upstream runtimes and attempt loop | 3 | L | WP-15, WP-16, WP-18, WP-19, WP-22, WP-23, WP-24, WP-28 |
| WP-47 | auth.jwt and JWKS | 3 | L | WP-02, WP-06, WP-16 |
| WP-48 | auth.api-key and auth.basic | 3 | M | WP-06 |
| WP-49 | auth.mtls, authz.ip and cors | 3 | M | WP-06, WP-16, WP-18 |
| WP-50 | auth.upstream-oauth2 | 3 | M | WP-02, WP-16 |
| WP-51 | validation.json-schema | 3 | M | WP-02, WP-28 |
| WP-52 | Test kit load generation and balancing | 3 | M | WP-26 |
| WP-53 | Examples | 3 | S | WP-28 |
| WP-54 | Release tooling and packaging | 3 | M | WP-27 |
| WP-65 | State Store manager and post-commit queue | 3 | M | WP-13, WP-28 |
| WP-85 | State Store test servers and container flavors | 3 | L | WP-26 |
| WP-88 | Response Cache revalidation and coalescing | 3 | M | WP-13 |
| WP-41 | Redis State Store driver | 4 | L | WP-13, WP-16, WP-85 |
| WP-55 | Loader | 4 | M | WP-03, WP-33, WP-36 |
| WP-56 | Semantic validation | 4 | L | WP-04, WP-05, WP-06, WP-38 |
| WP-57 | Diff | 4 | M | WP-03, WP-39 |
| WP-58 | Render | 4 | M | WP-33, WP-37, WP-38, WP-39 |
| WP-59 | authz.cel and headers | 4 | M | WP-06, WP-18, WP-28, WP-34 |
| WP-60 | Transforms | 4 | L | WP-02, WP-18, WP-28, WP-34 |
| WP-61 | ratelimit | 4 | L | WP-13, WP-18, WP-28, WP-34 |
| WP-62 | quota | 4 | M | WP-13, WP-18, WP-34 |
| WP-63 | Response Cache Filter | 4 | L | WP-13, WP-18, WP-34, WP-88 |
| WP-64 | Composition | 4 | L | WP-02, WP-19, WP-34, WP-46 |
| WP-66 | Request handler and replay | 4 | L | WP-05, WP-15, WP-18, WP-19, WP-20, WP-21, WP-42, WP-43, WP-45 |
| WP-67 | Drain and sd_notify | 4 | M | WP-14, WP-19, WP-21, WP-44, WP-45 |
| WP-68 | CLI admin client | 4 | M | WP-16, WP-25, WP-39 |
| WP-86 | Plain-upstreams forwarder and upstream debug | 4 | M | WP-46 |
| WP-87 | Test kit topology | 4 | M | WP-26, WP-52, WP-84, WP-85 |
| WP-69 | Configuration pipeline facade | 5 | M | WP-34, WP-35, WP-37, WP-39, WP-55, WP-56 |
| WP-70 | Snapshot compiler | 5 | L | WP-06, WP-34, WP-42, WP-46, WP-47, WP-48, WP-49, WP-50, WP-51, WP-59, WP-60, WP-61, WP-62, WP-63, WP-64, WP-65, WP-86 |
| WP-71 | Built-in Filter tables | 5 | S | WP-47, WP-48, WP-49, WP-50, WP-51, WP-59, WP-60, WP-61, WP-62, WP-63, WP-88 |
| WP-72 | CLI node commands | 5 | S | WP-14, WP-68 |
| WP-89 | Zero-Downtime handover | 5 | L | WP-14, WP-19, WP-21, WP-26, WP-44, WP-45, WP-52, WP-67 |
| WP-73 | Activation, Hot Reload and Last-Known-Good | 6 | L | WP-07, WP-14, WP-17, WP-21, WP-44, WP-45, WP-69, WP-70 |
| WP-74 | CLI bundle commands | 6 | L | WP-25, WP-34, WP-57, WP-58, WP-68, WP-69, WP-71 |
| WP-75 | Configuration conformance and golden corpus | 6 | L | WP-26, WP-53, WP-57, WP-58, WP-69, WP-71 |
| WP-76 | ruralzd wiring | 7 | L | WP-07, WP-14, WP-34, WP-40, WP-41, WP-44, WP-45, WP-46, WP-53, WP-65, WP-66, WP-67, WP-70, WP-71, WP-73, WP-86, WP-89 |
| WP-77 | CLI launcher, dev commands and root | 7 | L | WP-25, WP-58, WP-68, WP-72, WP-74 |
| WP-78 | Cross-package property suites | 7 | M | WP-26, WP-37, WP-42, WP-57, WP-58, WP-69, WP-73 |
| WP-79 | End-to-end suite | 8 | L | WP-52, WP-53, WP-76, WP-77, WP-87 |
| WP-80 | Chaos experiments and chaos at scale | 8 | L | WP-41, WP-52, WP-76, WP-87 |
| WP-81 | Benchmark harness, gates and records | 8 | L | WP-26, WP-27, WP-52, WP-66, WP-76 |
| WP-82 | Protocol conformance | 8 | M | WP-26, WP-76 |
| WP-90 | Benchmark scenarios, R1 generator and functional variants | 8 | L | WP-69, WP-76, WP-87 |
| WP-91 | Node integration: CEL and telemetry | 8 | M | WP-26, WP-76, WP-84, WP-85 |
| WP-92 | Node integration: security | 8 | M | WP-26, WP-76, WP-84, WP-85 |
| WP-93 | Node integration: State Store and round trip | 8 | M | WP-26, WP-41, WP-76, WP-85 |
| WP-94 | T5 systemd verification | 8 | M | WP-52, WP-54, WP-76, WP-87, WP-89 |
| WP-95 | Compose end-to-end and air-gapped image start | 8 | M | WP-53, WP-54, WP-76, WP-77, WP-84 |
| WP-99 | Game-day drills and failure rows | 8 | M | WP-41, WP-52, WP-76, WP-87 |
| WP-96 | Stage 12 release workflows and gates | 9 | L | WP-27, WP-54, WP-79, WP-80, WP-81, WP-82, WP-90, WP-91, WP-92, WP-93, WP-94, WP-95, WP-99 |
| WP-97 | RH-1 provisioning, budget runs and soak | 9 | M | WP-81, WP-90 |
| WP-98 | 0.1.0 release candidate, cut and repository settings | 10 | S | WP-96 |

### 4.4 Details

#### Wave 1: Core contracts (alone)

##### WP-01 Core contracts, lint rules and code registrations (L)

- **Owns**: `internal/clock/`; `internal/phase/`; `internal/problem/`; `internal/config/diag/`; `internal/config/tree/`; `internal/config/revision/`; `internal/config/hub/`; `internal/secret/*.go` (package root only); `internal/expr/`; `internal/telemetry/catalog/`; `internal/telemetry/emit/`; `internal/statestore/*.go` (package root only); `internal/filter/*.go` (package root only); `internal/filter/filtertest/`; `internal/gateway/snapshot/`; `internal/adminapi/`; `internal/errcode/`; `.golangci.yml`; `docs/architecture/02-configuration-model.md` (RZ-CFG registry rows only); `docs/architecture/03-data-plane.md` (RZ-RT registry rows only); `docs/architecture/08-security-and-identity.md` (RZ-AUTH registry row only); `docs/architecture/09-traffic-management-and-resilience.md` (RZ-UP registry row only); `docs/engineering/01-tech-stack-and-libraries.md` (library catalog rows and OQ-tech-stack-and-libraries-15, -16, -18 rows only)
- **Depends on**: none
- **Specs**: `00-architecture.md`, `01-config-load.md`, `02-config-revision.md`, `03-cel.md`, `04-dataplane-core.md`, `05-upstream-traffic.md`, `06-security.md`, `07-transform.md`, `08-statestore.md`, `09-observability.md`, `10-cli.md`
- **Scope**: Commit the section 2 sources verbatim (clock, clocktest, phase, problem, diag, tree, revision, hub, secret, expr, catalog, emit, statestore, filter, filtertest, snapshot, adminapi, errcode error.go), including the revision's contract additions: filter PolicyState, Finisher, Retry, Component, Replayer, Stripe and RouteMetrics, Registry components (R-39, R-40, R-44, R-45, R-43, R-56); snapshot Outbound, RequestBody, BeginLeg/LegRun, RequestState, LegRunner, Binding, LegChains, Route.Protocol (R-41, R-42, R-58, R-59); hub PolicyCheck/Checks and SecretUse.Destination (R-34, R-49); statestore label mapping and stripe; emit State Store label indexes; clocktest zero-duration timers; secret empty values and Node-wide watches (R-55); tree set-item paths; filtertest Store and ReplayFunc. Add the listed unit tests. Register RZ-CFG-038/039/040/041, RZ-RT-016..019, RZ-UP-011, RZ-AUTH-008 in errcode and the four registry documents (TestMatchesDocs green); replace the depguard block and add sloglint settings (section 1.3); in the tech stack catalog add the Metrics exposition row (OTel Prometheus exporter v0.68.x with client_golang v1.24.x, prometheus/common v0.71.x, otlptranslator v1.0.x), mark ULID own code and CLI stdlib flag, move the grpc-go row's first use to M1 (OTLP export), widen the x/sys row to SO_PEERCRED and test namespaces, list jwx transitive modules, otlp proto and cel.dev/expr, and close OQ-tech-stack-and-libraries-15/-16/-18. Contracts only: 01 M 47-50; 02 2.2 4-8, 2.6 29-34; 03 B 9-16 (place table); 04 H 49-53 (types); 05 F 31, G 33 (contract parts); 06 2.14 87-92 (redacting value); 08 2.1-2.4 (API), 2.12 68 (MAC key field); 09 2.6, 2.16 74-78 (catalog); 10 2.2 (problem Document).
- **Done when**: make lint test generate green; errcode TestMatchesDocs green; repocheck green; label-index, zero-duration, empty-secret and set-item tests pass under -race; no other path touched.

#### Wave 2: Leaves, primitives, infrastructure, executor, schema, test kit base and mocks, documents

##### WP-02 Strict JSON value library (M)

- **Owns**: `internal/jsonval/`
- **Depends on**: WP-01
- **Specs**: `07-transform.md`, `02-config-revision.md`, `06-security.md`, `05-upstream-traffic.md`
- **Scope**: Strict RFC 8259 scanner with byte offsets (line/column), duplicate member names, surrogate and UTF-8 checks, depth limit and cost accounting against a budget; mutable tree (object member order kept), deterministic encoder (no HTML escaping), RFC 8785 number formatting helper for canonical form, JSON media-type test. 07 H 56-68, I 69-73; used by 02 2.5 decode, 06 2.3 claims, 05 K merge. Fuzz target FuzzDecode; //go:build go1.27 oracle test against encoding/json/jsontext.
- **Done when**: Fuzz seeds pass; oracle test agrees on accept/reject for the JSON conformance cases; 0 allocations per scanned token in benchmarks.

##### WP-03 Schema index (M)

- **Owns**: `internal/config/schemaidx/`
- **Depends on**: WP-01
- **Specs**: `02-config-revision.md`, `01-config-load.md`
- **Scope**: Stdlib navigation of api/schema rendered.schema.json: $ref resolution, properties/items/additionalProperties/allOf/if-then dispatch, x-ruralz-* annotations (+ruralz:default, impact, open, cel, secret, keyed lists, since), Lookup(path)/Walk/Info used by overlay (keyed merge), subst (forbidden positions), defaults, canonical, diff (impact classes) and render (R-3). Tests read markers from the generated schema at run time, never from hard-coded lists (R-62). 02 2.2-2.4 support, 2.14 75-76; 01 F 23-25, G 26-31, I 36-37 support.
- **Done when**: Every schema path of the committed schema resolvable; keyed-list keys and defaults found for every marker the generator emitted, whichever WP-28 schema is committed.

##### WP-04 Policy type registry (M)

- **Owns**: `internal/config/registry/`
- **Depends on**: WP-01
- **Specs**: `01-config-load.md`, `02-config-revision.md`, `06-security.md`, `07-transform.md`, `05-upstream-traffic.md`
- **Scope**: FP 10 table for every v1alpha1 PolicyType: filter class, Phases (cache stays onRequestHeaders and onResponse, R-40), allowed scopes, slot, default and allowed failureMode (RZ-CFG-029 data; quota open, ai.token-budget closed per OQ-configuration-model-8 (a)), served-in-this-release flag (RZ-CFG-040 for plugin, authz.opa, authz.cedar, authz.geoip, auth.upstream-sigv4, ai.*), typed config constructor NewConfig keyed by existing config type names (R-7, R-62), materialized filterClass (authored different value RZ-CFG-005). 01 J 39-40; 02 2.7 35-38; 06 2.1 1-11 (class rules); 07 A 1-9; 05 T 97 (registration parts).
- **Done when**: Table test equals FP 10 row by row; every type has a config type or explicit none; served set table-tested (the worked-example chain is WP-38's).

##### WP-05 Route match primitives and request normalization (S)

- **Owns**: `internal/routematch/`
- **Depends on**: WP-01
- **Specs**: `04-dataplane-core.md`, `06-security.md`
- **Scope**: Host tables (exact, *. wildcard per OQ-data-plane-2 (a), any), segment trie helpers, template grammar CheckTemplate (RZ-CFG-005), prefix on segment boundaries, match identity Key (RZ-CFG-023), precedence comparator; NormalizePath (reject %00, backslash, %5C, control bytes, bad escapes: RZ-RT-017; unreserved decoding, dot segments, %2F kept) and NormalizeHost (R-35). 04 C 23-24, D 26-31; 06 OQ-security-and-identity-21 (a). Fuzz targets FuzzNormalizePath, FuzzTemplate.
- **Done when**: 04 D worked example holds on the primitives; fuzz seeds pass; 0 allocations for already-normal paths.

##### WP-06 Identity, Consumer index and auth helpers (M)

- **Owns**: `internal/identity/`; `internal/filter/auth/*.go` (package root only)
- **Depends on**: WP-01
- **Specs**: `06-security.md`
- **Scope**: Consumer compile to expr.Consumer (tier, tags, labels, quotas), credential Index (API key digests, JWT issuer+subject/claims, OAuth client_id else azp per OQ-security-and-identity-24 (a), basic usernames, certificate subjects/uriSan), KeyIndex for secretRef-held keys (copy-on-write on rotation through Node-wide watches, R-55), double match 401 RZ-AUTH-002, static checks RZ-CFG-035/036, Revoker hook (M2). filter/auth helpers: WWW-Authenticate challenges, decision recording into emit.AuthDecisions, second-binding rule (06 rule 2). 06 2.1 1-11, 2.2 12-22.
- **Done when**: Index lookups constant time; rotation swaps without locks on the read path and survives a carried-over KeyIndex; static checks table-tested.

##### WP-07 Secret resolver (M)

- **Owns**: `internal/secret/resolver/`
- **Depends on**: WP-01
- **Specs**: `01-config-load.md`, `06-security.md`, `04-dataplane-core.md`
- **Scope**: Implements secret.Resolver: env provider (RURALZ_SECRET_ prefix rule) and file provider under RURALZ_SECRET_ROOT (no symlink escape, owner and mode checks, rule 5; refuse to start when the root contains the data dir, admin TLS dir, token files or the MAC key file), per-kind size caps (4 MiB, CRL 16 MiB) and PEM/key validation by secret.Kind (RZ-CFG-026 interim for short API keys and uncredentialed State Store URLs), Resolve per Revision, Activate/Current, Node-wide watch table keyed by Ref that survives Activate (R-55), 2 s poll with fan-out, rotation failure metric and secret_rotation_failed degraded reason, cold-start retry with backoff. 01 L 42-46; 06 2.14 87-92; 04 A 3.
- **Done when**: Rotation observed by watchers within one poll, including a watch registered on a Store that is no longer active; nothing secret appears in errors or logs (canary test).

##### WP-08 CEL type adapters (M)

- **Owns**: `internal/cel/celtypes/`
- **Depends on**: WP-01
- **Specs**: `03-cel.md`
- **Scope**: cel-go type provider and ref.Val adapters for the expr views (request with lazy headers, query and pathParams; source; route; consumer; auth; response; error; upstream; attempt; steps; ai placeholder), expr.Value implementation over natives (map, list, string, json.Number, bool, null) with ascending-key iteration, AppendBody and Native. 03 C 17-25.
- **Done when**: Every expr.Vars field reachable from CEL with the documented types; adapters allocate nothing for unused fields.

##### WP-09 Telemetry aggregates (L)

- **Owns**: `internal/telemetry/aggregate/`
- **Depends on**: WP-01
- **Specs**: `09-observability.md`
- **Scope**: Implements emit.Meter and the metric handle interfaces: registry from catalog.Families, admission Plan per Revision with cardinality budget, folding to _overflow and retiring, Binding per snapshot (Retire, Release; R-58), cache-line striped counters and gauges, histograms with catalog bounds and exemplar slots, State Store handles as fixed arrays by emit.StateOp*/StateResult*/Write* (R-56), pre-created series at 0, collection into metricdata via an sdkmetric.Producer (OQ-observability-16 (a)). 09 2.6 39-46, 2.7 47-54, 2.8 55-56. The Node-level cardinality test is WP-91's.
- **Done when**: Hot-path Inc/Record 0 allocations; fold and retire property tests over simulated Revision sequences; producer output equals a reference encoding.

##### WP-10 Tracing (M)

- **Owns**: `internal/telemetry/tracing/`
- **Depends on**: WP-01
- **Specs**: `09-observability.md`, `04-dataplane-core.md`
- **Scope**: Implements emit.Tracer and emit.Span: W3C traceparent/tracestate parse and format, root sampling at traceSampling (default 0.01) with parent respect and lock-free GCRA caps, request ID = trace ID, ID generator on crypto/rand, span caps and bounded processor queue, span names from catalog (server, ruralz.route.match, ruralz.filter.<name>, upstream), attribute sets, Inject for upstream legs, unsampled counter. 09 2.4 27-32, 2.5 33-38.
- **Done when**: Unsampled path allocates nothing; propagation conformance table passes.

##### WP-11 Process logs and access logs (M)

- **Owns**: `internal/telemetry/logsink/`; `internal/telemetry/accesslog/`
- **Depends on**: WP-01
- **Specs**: `09-observability.md`, `04-dataplane-core.md`
- **Scope**: logsink: slog.Handler with catalog keys, level from RURALZ_LOG_LEVEL, redaction of secret values and credential headers, bounded stdout worker with drop counter. accesslog: implements emit.AccessLog with pooled records, field truncation, JSON encoder (stable member order), query never logged; accessLog.when is evaluated by the handler (R-48). 09 2.11 61-65, 2.12 66-68; 04 L 79.
- **Done when**: Encoder golden bytes; drop behavior under a blocked stdout (package level); no allocation per record at steady state.

##### WP-12 Telemetry gates, alert rules and dashboards (S)

- **Owns**: `internal/tool/telemetrygen/`; `internal/tool/repocheck/`; `deploy/grafana/`; `docs/architecture/10-observability.md` (crl_stale degraded reason row only)
- **Depends on**: WP-01
- **Specs**: `09-observability.md`
- **Scope**: telemetrygen generates deploy/grafana/rules/ruralz.rules.yaml from catalog alert rules (go:generate directive in its own package) and checks dashboards; an in-package drift test (TestGeneratedUpToDate) regenerates in memory and compares with the committed files, so stage 5 catches drift without a Makefile change (WP-27 also adds deploy/grafana to the make generate diff); repocheck gains metric-name, label and span-name checks against the catalog, and a doc check of metric names and degraded reasons in docs/architecture/10-observability.md (every catalog name already appears there except crl_stale, whose row this package adds, R-37), plus testdata/bad cases; promtool check and test rules (09 test 43, integration tag, skipped without promtool); dashboards for the M1 SLOs. 09 2.15 71-73, 2.16 74, 75, 77, 78.
- **Done when**: Drift test green; repocheck green on the repository (doc check included) and flags each testdata/bad case; promtool rule tests pass where promtool is installed.

##### WP-13 State Store base: breaker, keys, memory driver, test fake (L)

- **Owns**: `internal/statestore/breaker/`; `internal/statestore/keys/`; `internal/statestore/memory/`; `internal/statestore/statestoretest/*.go` (package root only)
- **Depends on**: WP-01
- **Specs**: `08-statestore.md`, `05-upstream-traffic.md`
- **Scope**: breaker: lock-free per-shard breaker and full-jitter reconnect pacer on clock (08 2.2 19-22). keys: key layout rz:rl:, rz:qt:, rz:rc:, rzplg: reserved, escaping, hash tags and CRC16 slots, generation keys, entry MAC over "rz1" || key || field || entry with Deps.MACKey (08 2.6 47-51, 2.12 68; R-49). memory: memory driver implementing statestore.Store with GCRA, quota reserve/refund, cache get/set/meta/generation and leases with the Lua semantics, bounded entries, metrics by OpKind.Label on the RequestBudget stripe, 0-allocation single GCRA (08 2.9 58-60, 2.5 semantics 39-46). statestoretest: failure-injecting fake Store and the driver conformance suite shared with redis (08 6.1-6.3).
- **Done when**: Conformance suite passes on memory; GCRA property test with clocktest.Fake; breaker transitions table-tested; MAC tamper cases are misses.

##### WP-14 Process identity, data dir and settings (S)

- **Owns**: `internal/ulid/`; `internal/nodedir/`; `internal/gateway/setting/`
- **Depends on**: WP-01
- **Specs**: `04-dataplane-core.md`, `10-cli.md`, `08-statestore.md`, `06-security.md`
- **Scope**: ulid on crypto/rand (48-bit ms + 80 random). nodedir: layout (dirs 0700, files 0600), node.id, lock via stdlib syscall.Flock in a unix file with a !unix stub returning errors.ErrUnsupported (R-60), holder.json ruralz.holder.v1 with pidNamespace (OQ-cli-and-api-surface-10 (a)), ReadHolderAt for the CLI, lkg/ and handover.sock paths, DefaultRoot. setting: RURALZ_* parsing and validation (exit 2 rules, secret root overlap refusal), RURALZ_STATE_STORE_MAC_KEY_FILE (absolute path, owner-only mode, at least 32 bytes, read at start, outside RURALZ_SECRET_ROOT; R-49, 08 req 68). 04 A 1-7; 10 2.11 93-97 (holder reading).
- **Done when**: Lock contention test with two processes; holder round trip; settings table test of every exit-2 case including the MAC key file; GOOS=windows go build ./internal/nodedir/... and GOOS=darwin build succeed.

##### WP-15 Client address (M)

- **Owns**: `internal/clientaddr/`
- **Depends on**: WP-01
- **Specs**: `06-security.md`, `04-dataplane-core.md`
- **Scope**: PROXY v2 reader and net.Listener wrapper (first-bytes deadline, LOCAL command, TLV skip), trusted-proxy resolution from Forwarded and X-Forwarded-For (OQ-security-and-identity-6 (c)), source.ip with IPv4-mapped unmapping, forwarding-header rewrite toward Upstreams (untrusted peer overwrite, X-Forwarded-Host/Proto). 06 2.8 58-63; 04 B 16-18. Fuzz target FuzzProxyV2.
- **Done when**: Malformed PROXY headers close the connection; header rewrite table test; fuzz seeds pass.

##### WP-16 Egress guard and TLS configuration (M)

- **Owns**: `internal/egress/`; `internal/tlsconf/`
- **Depends on**: WP-01
- **Specs**: `06-security.md`
- **Scope**: egress: SSRF guard with RURALZ_FETCH_ALLOW prefixes, guarded dialer and HTTP client for jwksUrl and tokenUrl fetches and Upstream connections (0.0.0.0/8, ::/128, loopback, link-local and fd00:ec2::254, with their IPv4-mapped, NAT64 and IPv4-compatible forms, blocked unless allowed, per redirect hop; RFC 1918 allowed; State Store and OTLP connections not guarded; a client option that follows no redirect at all for destination-bound secrets, R-49). tlsconf: listener (TLS 1.3 default, 1.2 opt), upstream, State Store, OTLP and admin *tls.Config builders, cipher list, SNI certificate index over secret.Store (read per handshake), client-certificate request mode. 06 2.11 74-78, 2.12 79-83, 2.13 84-86.
- **Done when**: Dial to blocked ranges refused with the allow list honored; no-redirect client never contacts a second origin; handshake tests with in-package PKI.

##### WP-17 Admin authentication, redaction and digest checks (S)

- **Owns**: `internal/adminauth/`; `internal/redact/`; `internal/signing/`
- **Depends on**: WP-01
- **Specs**: `06-security.md`, `04-dataplane-core.md`
- **Scope**: adminauth: RURALZ_ADMIN_TOKEN_FILE, RURALZ_ADMIN_METRICS_TOKEN_FILE, RURALZ_ADMIN_TLS_DIR (OQ-security-and-identity-7 (a)), constant-time bearer check, loopback cleartext rule, 401 with RZ-AUTH-001/002. redact: credential-header set per snapshot, helpers for /tap and slog. signing: Verifier interface and DigestOnly (RZ-CFG-027), ParseDigest (rejects rev-..., uppercase, wrong lengths), VerifyArtifact (streaming SHA-256 with a size limit, RZ-CFG-028; the Plugin half of the roadmap's digest checks, wired to fetched artifacts in M2, R-50), Sigstore slot for M2. 06 2.15 93-96, 2.16 97-98; 04 K 70.
- **Done when**: Token rotation picked up; redaction covers every credential header of the Revision; VerifyArtifact match, single-bit flip and limit cases.

##### WP-18 HTTP field rules and structured fields (S)

- **Owns**: `internal/httpfield/`; `internal/sfv/`
- **Depends on**: WP-01
- **Specs**: `07-transform.md`, `05-upstream-traffic.md`, `04-dataplane-core.md`
- **Scope**: httpfield: RFC 9110 token and value tables, protected names, hop-by-hop predicate (Connection-named fields), order-preserving query editor. sfv: RFC 9651 list serializer for RateLimit-Policy and RateLimit fields. 07 E 33-37; 05 N 67-68; 04 C 22.
- **Done when**: Table tests against RFC examples; serializer golden bytes.

##### WP-19 Admission and body buffering (M)

- **Owns**: `internal/gateway/admission/`; `internal/gateway/body/`
- **Depends on**: WP-01
- **Specs**: `04-dataplane-core.md`, `05-upstream-traffic.md`, `07-transform.md`
- **Scope**: admission: in-flight units (20,000, one atomic add, 503 RZ-RT-005), pre-routing write limiter (2,000), connection ceiling counting listener wrapper, externally reduced ceilings during handover. body: Node buffer budget (maxBufferedBytes, 25% stream share, 32 KiB increments, carried across reloads), gate reader with maxRequestBodyBytes (413 RZ-RT-003), tee with limit, limited reader, decoded-value reservations (4x cap), snapshot.RequestBody implementation over the gate or the client stream (replayable when empty, gated or unread; Open per attempt or step; Buffered for GetBody; R-41). 04 B 11, C 19-20, G 46-48; 05 F 31, K 54 (step replay); 07 I 69-73.
- **Done when**: Budget never negative under -race stress; reservation failures map to the documented codes; RequestBody replay matrix (empty, gated, streamed unread, streamed read) table-tested.

##### WP-20 Filter Chain executor (L)

- **Owns**: `internal/gateway/executor/`
- **Depends on**: WP-01
- **Specs**: `04-dataplane-core.md`, `05-upstream-traffic.md`, `07-transform.md`, `06-security.md`, `08-statestore.md`
- **Scope**: Runs snapshot.Chain per Phase over snapshot.RequestState (R-42): Enter before every call, once-per-request when evaluation recorded with SetWhen, auth-all-skipped 401 RZ-AUTH-001, Result handling (Retry only in onUpstreamResponseHeaders, R-44), failureMode table by class and Phase with codes, short-circuit rules, response Phases reversed, onLog in request order, Finish for every Finisher that ran (R-40), Consumptive batching (Prepare, one Store.Consume, Complete, Undo) with per-member PolicyState and RequestBudget, recovered panics as CannotDecide, LegHooks.BeginLeg/LegRun over RequestState.Leg (concurrent legs share nothing mutable; End runs leg Finish), filter metrics on the request stripe and spans. 04 F 39-45; 05 G 33 (Filter retry), Q 91-93; 07 B 10-19; 06 2.1 rule 1; 08 2.3 27-38.
- **Done when**: Table tests over every class x Phase x failureMode cell with a fake RequestState; batching test with filtertest.Store (trip counts); concurrent leg test under -race; AllocsPerRun on a 3-Policy chain within the PB-8 share.

##### WP-21 Snapshot holder and retirement (M)

- **Owns**: `internal/gateway/retire/`
- **Depends on**: WP-01
- **Specs**: `04-dataplane-core.md`, `09-observability.md`
- **Scope**: Holder with atomic.Pointer and the pin protocol (load, increment stripe, re-load, retry), stripe assignment, K = 2 retired bound, closing and 30 s grace, ending protocol walking Pins with bounded goroutines (deadlines and mark ended under Mu, then cancel; R-73), Binding.Retire at retirement and Binding.Release at zero pins (R-58), resource close at zero pins unless shared by a newer snapshot, snapshot_ending_overdue degraded reason, retired-snapshots gauge, /debug/snapshots view (adminapi). 04 H 49-54; 09 2.8 55-56 (retire hooks).
- **Done when**: Race test: no request observes a closed resource; ending protocol with clocktest.Fake; pin path 0 allocations; Binding calls counted in order.

##### WP-22 Endpoint balancing (M)

- **Owns**: `internal/gateway/upstream/balance/`
- **Depends on**: WP-01
- **Specs**: `05-upstream-traffic.md`
- **Scope**: Four algorithms (round robin with weights, least request, random, consistent hash ring with bounded load) with schedule and ring builders over a uint64 key (the hashKey value is evaluated by WP-46), ejection-aware selection, balancer budget planner (warning per OQ-traffic-management-and-resilience-23 (b)). 05 C 10-13, 15-17 and the ring structure of 14.
- **Done when**: Distribution tests within tolerance; ring stability under endpoint churn.

##### WP-23 Discovery and health (M)

- **Owns**: `internal/gateway/upstream/discovery/`; `internal/gateway/upstream/health/`
- **Depends on**: WP-01
- **Specs**: `05-upstream-traffic.md`
- **Scope**: discovery: static host re-resolution, DNS A/AAAA and SRV with TTL clamps and backoff, Resolver interface, discovery_stale degraded reason. health: passive ejection (consecutive failures, max ejection percent), active prober with probe slots, stall and suspect handling, probes-skipped counter and reason. 05 B 5-9, D 18-23.
- **Done when**: Deterministic tests with a fake resolver and clocktest.Fake; ejection bound never exceeded.

##### WP-24 Resilience primitives (M)

- **Owns**: `internal/gateway/upstream/resilience/`
- **Depends on**: WP-01
- **Specs**: `05-upstream-traffic.md`
- **Scope**: Own Config structs (filled by upstream core from v1alpha1): retry policy with retryOn program (expr.DefaultRetryOn default) or a Filter retry request, backoff full jitter uniform(0, min(250 ms, 25 ms x 2^(n-1))), retry budget max(3, 20%), circuit breaker with consecutiveFailures, failureRatio, minimumLegs 20, halfOpenSuccesses 3, openDuration (OQ-traffic-management-and-resilience-5 (a)), failureWhen, bulkhead (maxConnections 1024, maxPendingRequests 256), error kinds and RZ-UP code selection, deadline arithmetic. 05 E 24-26, G 32-35, H 36-38, I 39-40.
- **Done when**: Breaker state machine and budget property tests (zero-jitter draws included, clocktest.Fake); code selection table equals 05 I.

##### WP-25 CLI framework and completion (M)

- **Owns**: `internal/cli/command/`; `internal/cli/completion/`
- **Depends on**: WP-01
- **Specs**: `10-cli.md`
- **Scope**: Spec/Command model, interspersed stdlib flag parsing with flag specs for help and completion, IO streams, exit codes (0/1/2/3, ExitError, Interrupted on SIGINT), --output text|json and NDJSON writers, atomic file write, bash/zsh/fish/PowerShell completion generators. 10 2.1 1-12, 2.2 13-24, 2.14 106-108.
- **Done when**: Golden help and completion scripts; exit-code table test.

##### WP-26 Test kit base (L)

- **Owns**: `internal/testkit/binaries/`; `internal/testkit/proc/`; `internal/testkit/pki/`; `internal/testkit/faultproxy/`; `internal/testkit/canary/`; `internal/testkit/promtext/`; `internal/testkit/benchrecord/`; `internal/testkit/corpus/`
- **Depends on**: WP-01
- **Specs**: `11-ops-quality.md`
- **Scope**: binaries (build ruralzd/ruralz with the release flags including -s -w (R-52) or RURALZ_TEST_BIN_DIR, -overlay variants), proc (start/signal/wait, bounded log rings, /proc sampling, InNetNS re-exec with x/sys), pki (test CA, server and client certs), faultproxy (pass, delay, black-hole, reset, refuse, switch upstream; OQ-scalability-and-distributed-state-10 (a)), canary (multi-encoding leak scan), promtext (scrape and parse), benchrecord (ruralz.bench.v1 record and schema check), corpus (fuzz seeds from test/conformance/config golden and negative corpora, tolerant of absent dirs). 11 section 3, D 27, I 77 (record format).
- **Done when**: Each package unit-tested (11 test plan items 7, 10-12); faultproxy modes verified against a TCP echo.

##### WP-27 CI pipelines, make targets and gates (L)

- **Owns**: `Makefile`; `.gitattributes`; `.github/workflows/pr-fast.yml`; `.github/workflows/pr-full.yml`; `.github/workflows/main.yml`; `.github/workflows/nightly.yml`; `scripts/install-tools.sh`; `scripts/update-test-suites.sh`; `scripts/ci-changes.sh`; `scripts/ci-approvals.sh`; `internal/tool/benchgate/`; `internal/tool/sizegate/`; `internal/tool/fuzzplan/`
- **Depends on**: WP-01
- **Specs**: `11-ops-quality.md`, `09-observability.md`
- **Scope**: Make targets (each tolerant of suites not present yet): integration, gates, e2e, nightly (NIGHTLY_JOB=fuzz|chaos|latency), fuzz, chaos, chaos-scale, t5, image-checks, bench-latency, soak, release, release-dry-run, release-gates, quickstart; build and release ldflags add -s -w (R-52, 11 risk 15); generate diffs api/schema/ruralz and deploy/grafana (09 req 72). main.yml runs stage 10 (make e2e with the leak assertions) and make quickstart on a clean runner (11 req 5, 33); pr-full stages 7-10 with the aggregator job pr-full (11 req 4) and the stage 8 matrix: linux/amd64 and ubuntu-24.04-arm process legs (OQ-repository-layout-and-conventions-6 (a)) plus a linux/amd64 container leg with RURALZ_TEST_STATESTORE=container for the State Store flavors (11 req 29, 08 6.4 item 10), where make integration also runs systemd-analyze verify (11 req 86, first part); stage 5 adds ./test/conformance/config/... to linux, floor and CLI jobs; nightly Fuzz, Chaos (at least 10 Nodes), Latency (rh-1 label, failing with an explicit 'RH-1 not provisioned' annotation until WP-97) jobs; pr-fast, pr-full, main and nightly accept workflow_call with all: true (11 req 11). .gitattributes adds test/fixtures/**/raw/** -text (11 req 20). scripts/update-test-suites.sh refreshes the pinned YAML Test Suite and JSON-Schema-Test-Suite tarballs with SOURCE and LICENSE files (11 req 18). install-tools.sh pins promtool, oha, vegeta, cosign v3.1.3, Syft v1.52.0, actionlint by SHA-256. benchgate (alloc/op A/B, four-of-five), sizegate (160 MiB stripped, 89 MiB idle RSS), fuzzplan (list, shard, run). 11 A 1-11, B 18, 20, D 28, I 62-68 (gates).
- **Done when**: actionlint clean; make help lists every target; each tolerant target exits 0 with a skip line when its suite is absent; tools unit-tested (11 test plan items 1, 2, 5).

##### WP-28 Schema and configuration types (M)

- **Owns**: `pkg/config/v1alpha1/`; `api/schema/`; `internal/tool/schemagen/`
- **Depends on**: WP-01
- **Specs**: `02-config-revision.md`, `01-config-load.md`, `05-upstream-traffic.md`, `06-security.md`, `07-transform.md`, `08-statestore.md`, `09-observability.md`
- **Scope**: Additive-only v1alpha1 changes (no Go type, field or JSON name renamed or removed, R-62) and regenerated schema: +ruralz:default markers (stateStore.timeout 50ms, limits.maxRequestBodyBytes 10Mi, maxRequestHeaderBytes 64Ki, auth.api-key header x-api-key, telemetry.traceSampling 0.01, OQ-traffic-management-and-resilience-6 static defaults incl. healthCheck.active values); circuitBreaker.minimumLegs, failureRatio, halfOpenSuccesses; Gateway.spec.stateStore.cache (OQ-scalability-and-distributed-state-3 (a)); telemetry.otlp.tls with the UpstreamTLS shape (OQ-observability-2 (a)); ratelimit config.localOnly (OQ-scalability-and-distributed-state-11 (a)); validation.json-schema config.schema (inline draft 2020-12, 07 J 74); headers add/remove shape (OQ-configuration-model-19 (a)); drop +ruralz:required on transform Replace.Path; +ruralz:impact markers; CircuitBreaker doc fix. No auth.jwt field changes (R-32); OQ-configuration-model-19 (a) is applied to the fields M1 builds (validation.json-schema config.schema, headers add/remove), while other feature-authored fields (auth.api-key config.query, ratelimit responseHeaders and cost, and the rest of that row) are registered with their features later, their configs staying open. 02 2.3 9-14, 2.14 75-76; 05 A 4, T 97; 07 J 74-77; 08 2.1 6; 09 OQ rows.
- **Done when**: make generate produces the committed schema; schemagen tests green; golden digests are not frozen before this lands.

##### WP-29 Docs: configuration model, foundation pack, system overview (M)

- **Owns**: `docs/architecture/02-configuration-model.md`; `docs/_meta/foundation-pack.md`; `docs/architecture/01-system-overview.md`; `docs/glossary.md`
- **Depends on**: WP-01
- **Specs**: `00-architecture.md`, `01-config-load.md`, `02-config-revision.md`, `03-cel.md`, `07-transform.md`
- **Scope**: Record adopted options and field text: OQ-configuration-model-8 (a), -18 (a), -19 (a) applied to the fields M1 builds, the other feature-authored fields registered with their features; fields of WP-28 (stateStore.cache, otlp.tls, minimumLegs/failureRatio/halfOpenSuccesses, localOnly, validation.json-schema config.schema registration, headers add/remove, default markers); RZ-CFG-041 and the secret-to-destination binding rule in the validation section (R-49); stage ownership of codes (R-46); pack amendments under pack section 14: 8.6 RT meaning (OQ-data-plane-9 (a)), 8.7/8.8/8.11 (OQ-scalability-and-distributed-state-11 (a), OQ-traffic-management-and-resilience-16 (b), -19 (c), -20 (a)), section 2 and 5 settings (OQ-security-and-identity-7 (a), -22 (a) including RURALZ_STATE_STORE_MAC_KEY_FILE and the binding), OQ-vision-and-positioning-10 row; CEL 50 ms comprehension cap (03 risk 1); RZ-CFG-040 meaning.
- **Done when**: Every listed OQ row reads 'No (answered)' with the option; every WP-28 field has text; docs lint (style guide) clean.

##### WP-30 Docs: data plane, security, zero-downtime, CLI reference, topologies (M)

- **Owns**: `docs/architecture/03-data-plane.md`; `docs/architecture/08-security-and-identity.md`; `docs/operations/01-deployment-topologies.md`; `docs/operations/02-zero-downtime-upgrades-and-hot-reload.md`; `docs/reference/01-cli-and-api-surface.md`
- **Depends on**: WP-01
- **Specs**: `00-architecture.md`, `04-dataplane-core.md`, `06-security.md`, `10-cli.md`
- **Scope**: Close OQ-data-plane-2 (a), -8 (a), -9 (a); OQ-security-and-identity-1 (b), -6 (c), -7 (a), -9 (a for M1), -15 (a), -21 (a), -22 (a) with the M1 definitions of R-49 (Secrets rules 5 to 7 become binding; new rule for the destination binding and RZ-CFG-041; T10 and T18 rows point at the built MAC and binding), -24 (a), confirm -2/-3/-18; OQ-zero-downtime-upgrades-and-hot-reload-7 (a) and record -1 (a), -2 (b), -3 (a), -4 (a), -12 (a); OQ-cli-and-api-surface-10 (a) with holder.json members; document RZ-RT-017 hardening, /readyz, /tap (R-24), /debug/snapshots, lkg layout, handover protocol, drain timeline, sd_notify READY timing (11 risk 9), admin settings, JWT fixed defaults, the T5 unit and its verification job.
- **Done when**: Every listed OQ row closed with the adopted option and no part of a closed option left unbuilt by a named WP; no contradiction with section 3 of this document.

##### WP-32 Docs: engineering, performance, vision, features, ADRs (M)

- **Owns**: `docs/engineering/02-repository-layout-and-conventions.md`; `docs/engineering/03-testing-and-quality-strategy.md`; `docs/engineering/04-release-versioning-and-compatibility.md`; `docs/engineering/01-tech-stack-and-libraries.md` (CI tooling rows and OQ-tech-stack-and-libraries-25 only); `docs/architecture/12-performance-budgets-and-benchmarking.md`; `docs/vision/01-vision-and-positioning.md`; `docs/features/01-feature-catalog.md`; `docs/adr/`
- **Depends on**: WP-01
- **Specs**: `00-architecture.md`, `11-ops-quality.md`
- **Scope**: Close OQ-performance-budgets-and-benchmarking-1 (a), -2 (b, owner decision recorded; provisioning is WP-97), -6 (a), record -3 (a), -4 (a); OQ-release-versioning-and-compatibility-3 (a); OQ-repository-layout-and-conventions-6 (a), record -4 (b); OQ-testing-and-quality-strategy-2 (c), record -1, -3, -4, -5 (a); OQ-tech-stack-and-libraries-25 (a). CI tooling rows before first use (11 req 10, 76, risk 11): cosign v3.1.3, Syft v1.52.0, actionlint, promtool, oha v1.16.0, vegeta, actions/attest-build-provenance, actions/attest-sbom, actions/upload-artifact, actions/download-artifact, docker/setup-buildx-action and the runner's docker buildx CLI, the CA source image golang:1.27.1-bookworm by digest, the testcontainers images (redis:8, valkey/valkey:9.0.x, dragonfly) and the OpenTelemetry Collector image for WP-95, the self-hosted RH-1 runner for chaos-scale (OQ-testing-and-quality-strategy-11 (b)). Layout doc: new packages, layers and depguard rules of section 1; testing doc: process harness as the end-to-end implementation, Compose and the image air-gapped start in CI (R-51), container State Store matrix, node integration suites; release doc: -s -w release builds (R-52), release.yml stages and gates; ADR-0018 superseding ADR-0010 (exporter, external producer, otel confinement) and ADR-0019 superseding ADR-0011 (50 ms comprehension stop, closing OQ-configuration-model-21 (a)), both at the user's request of 2026-10-03, written at the wave-2 boundary; feature catalog milestones (SM-2).
- **Done when**: Every listed OQ row closed; every CI tool, action and image used by WP-27, WP-54, WP-81, WP-85, WP-95, WP-96 has a row; layout doc lists every M1 package of section 1.2.

##### WP-83 Docs: roadmap amendments for M1 (S)

- **Owns**: `docs/roadmap/01-roadmap-and-milestones.md`
- **Depends on**: WP-01
- **Specs**: `00-architecture.md`, `11-ops-quality.md`
- **Scope**: Record in the M1 section, as dated notes with reason and owner (no scope change): end-to-end runs on the process harness everywhere, with the Docker Compose file-mode test (quickstart, --effective --route, bundle diff against /config/dump, Zero-Downtime Upgrade) and a real OpenTelemetry Collector plus the air-gapped image start in CI (R-51, WP-95); Plugin artifact digest checks: verifier in M1, no Plugin artifact is fetched or used in M1, wiring with Plugin fetch in M2 (R-50); State Store matrix: local process flavor, container flavors Redis 8, Valkey 9.0.1+, Dragonfly and clusters in CI stage 8 (WP-85, WP-27); exit criteria 2 and 3, the soak and CE-12 at 1,000,000 calls per second need RH-1 (OQ-performance-budgets-and-benchmarking-2 (b), WP-97), and CE-5 at 100 Nodes and CE-15 at target rate also run on RH-1 (OQ-testing-and-quality-strategy-11 (b), WP-80).
- **Done when**: Each note names its reason, the resolution in this document and the owning work package; docs lint clean.

##### WP-84 Test kit mocks and OTLP sink (M)

- **Owns**: `internal/testkit/mockup/`; `internal/testkit/mockidp/`; `internal/testkit/otlpsink/`
- **Depends on**: WP-01
- **Specs**: `11-ops-quality.md`, `09-observability.md`, `06-security.md`
- **Scope**: mockup (HTTP/1.1, h2c, TLS mock upstream with programmable body size, delay distribution, status, reset, slow body, echo, request log; TLS config passed in), mockidp (JWKS with rotating RS256/ES256 keys and Cache-Control, OAuth2 client-credentials token endpoint, JWT minting on stdlib crypto), otlpsink (in-process OTLP/gRPC collector over TLS or h2c recording traces, logs and metrics, span-order queries, raw bytes for canary scans), self-contained on go.opentelemetry.io/proto/otlp and grpc with no internal/telemetry import (R-53). 11 section 3.
- **Done when**: otlpsink receives each signal from an OTLP gRPC client built on the proto module; mockidp tokens verify with stdlib crypto; mockup behaviors table-tested.

#### Wave 3: Configuration stages, CEL, data-plane components, upstream runtimes, non-CEL Filters, State Store test servers

##### WP-31 Docs: traffic, observability, scalability (M)

- **Owns**: `docs/architecture/09-traffic-management-and-resilience.md`; `docs/architecture/10-observability.md`; `docs/architecture/11-scalability-and-distributed-state.md`
- **Depends on**: WP-01, WP-12
- **Specs**: `00-architecture.md`, `05-upstream-traffic.md`, `08-statestore.md`, `09-observability.md`
- **Scope**: Close OQ-traffic-management-and-resilience-2 (c), -5 (a, M1 part), -6 (c with (a) values), -11 (a), -16 (b), -19 (c), -20 (a), -21 (a), record -23 (b); OQ-scalability-and-distributed-state-2 (a), -3 (a, M1 part), -10 (a), -11 (a); OQ-observability-2 (a), -16 (a), record -1 (a); RZ-RT-005 counting rule (R-23), /metrics exporter wording, accessLog.when evaluation point (R-48), Lua library ruralz_v1 (R-20), entry MAC scope and key setting (R-49), Filter retry requests (R-44), Response Cache post-response work (R-40) and revalidation replay (R-43); observability S17: Compose with a Collector runs in CI (R-51). Wave 2 boundary decisions: record OQ-observability-22 in docs/architecture/10-observability.md as `| OQ-observability-22 | What does a sampled 15-span trace cost on the OpenTelemetry SDK? | (a) 90 allocations, 30 µs on the reference hardware (proposed); (b) 60 allocations with a Ruralz span recorder | observability | No |`, change the Overhead table cell to "90 or fewer (target)" and the Estimates sentence "4 allocations per span" to "4 SDK allocations per span plus up to 2 by Ruralz" (R-69); add one sentence in OBS "Alert rules" stating that RuralzSeriesFolded alerts on new folding only and is exempt from the first-event test (R-70); record the memory driver bounds (08 risk 15) with N_c = 1 in docs/architecture/11-scalability-and-distributed-state.md, and say in TMR "Response caching" rule 3 that N_c applies to shared stores (R-72); state in TMR "Load balancing" both exceptions to the 10 s rebuild interval (the first structure over a non-empty set; a budget plan that lowers v or starts a fallback) and that a ring in fallback returns to ring-hash only when the plan gives it at least 128 virtual nodes per Endpoint (target) (R-74).
- **Done when**: Every listed OQ row closed; metric names in the document equal the catalog (WP-12's repocheck doc check green).

##### WP-33 Restricted YAML and JSON profile (L)

- **Owns**: `internal/config/profile/`; `test/fixtures/yaml-test-suite/`
- **Depends on**: WP-01, WP-02
- **Specs**: `01-config-load.md`, `02-config-revision.md`, `11-ops-quality.md`
- **Scope**: Byte pre-checks (UTF-8, BOM, size), incremental scanner token pass (depth 64, MaxDocumentTokens 1,000,000, anchors/aliases/merge keys RZ-CFG-003, custom tags RZ-CFG-004), per-document parse with duplicate keys RZ-CFG-002, YAML 1.2 core scalar typing into tree.Node with positions (KindFloat text stored in RFC 8259 syntax: drop a leading '+', put a '0' before a leading '.', drop a '.' with no fraction digits, strip leading zeros of the integer part keeping one digit, never through float64; R-64), JSON front end on jsonval, restricted-profile Encode with the R-45 quoting rule (R-29); YAML Test Suite vendored with scripts/update-test-suites.sh and run (integration tag) against expected-failures.txt with the ratchet. 01 B 7-14, C 15; 02 2.8 43-49 (emitter); 11 B 18. Fuzz FuzzLoadYAML (memory bounded under alias bombs).
- **Done when**: YAML Test Suite passes except expected-failures.txt, and a listed case that passes fails the run; fuzz seeds pass; Encode(Parse(x)) idempotent.

##### WP-34 CEL module (L)

- **Owns**: `internal/cel/*.go` (package root only); `internal/cel/celtest/`; `internal/cel/testdata/`
- **Depends on**: WP-01, WP-02, WP-08
- **Specs**: `03-cel.md`
- **Scope**: Implements expr.Compiler/Builder/Program/Value on cel.dev/cel-go v0.32.0: one environment per place and site refinement, standard library plus strings and encoders only, Check with RZ-CFG-014/015 messages, static cost at nominal sizes (10,000 ceiling, unbounded handling), runtime cost limit 1,000,000 with ContextEval under min(deadline, 50 ms) for comprehensions, runtime error kinds and per-place rules, program cache across snapshots (ProgramSet), ReferencesRequestBody, Default for retryOn/failureWhen, DecodeJSON on jsonval enforcing only the budget it is given (callers pass filter.DecodedLimitFactor times the raw limit, R-63), PrepareRoute and PrepareConsumer delegating to celtypes.PrepareRoute and PrepareConsumer (also in celtest); the place-to-annotation test maps each headers place to its shared schema definition (HeaderRequestSet, HeaderResponseSet), which covers set[] and add[] entries. 03 A 1-8, B 9-16, D 26-30, E 31-37, F 38-42, G 43-46, H 47-49, I 50-52, J 53-54. Fuzz FuzzCompile and cost estimator target. Node-level runtime error matrix is WP-91's.
- **Done when**: Diagnostics golden across places; worst-case benchmark within the proposed cap; no goroutines started.

##### WP-35 Schema validation view (M)

- **Owns**: `internal/config/schemaview/`; `test/fixtures/json-schema-test-suite/`
- **Depends on**: WP-01, WP-03, WP-28
- **Specs**: `01-config-load.md`, `11-ops-quality.md`
- **Scope**: Compile the rendered view once with jsonschema/v6 (Draft 2020-12, refusing loader, x-ruralz-* vocabulary), validate resource trees, map errors to diag.Path with RZ-CFG-005/006/012 and nearest-name hints; JSON-Schema-Test-Suite vendored with scripts/update-test-suites.sh and run over tests/draft2020-12 without optional/ except the formats Ruralz uses (R-31); schema meta test (both schema files and examples/, 11 req 19). 01 H 32-35; 11 B 18-19.
- **Done when**: Zero JSON-Schema-Test-Suite failures (11 req 18); every error kind maps to a registered code; meta test green.

##### WP-36 Overlays and substitution (M)

- **Owns**: `internal/config/overlay/`; `internal/config/subst/`
- **Depends on**: WP-01, WP-03
- **Specs**: `01-config-load.md`
- **Scope**: overlay: strategic merge with keyed lists and $patch (delete, replace), identity rules RZ-CFG-008/030. subst: ${VAR} and ${VAR:-default} scan, forbidden positions RZ-CFG-011, undefined RZ-CFG-010, re-typing by schema, $${ escaping, secret-like names and credential literals RZ-CFG-013, never injects structure. 01 F 23-25, G 26-31. Fuzz FuzzSubst, FuzzOverlay; property: substitution never injects structure.
- **Done when**: Fuzz and property seeds pass; results independent of file order.

##### WP-37 Defaults, normalization and conversion (M)

- **Owns**: `internal/config/defaults/`; `internal/config/convert/`
- **Depends on**: WP-01, WP-02, WP-03, WP-04, WP-28
- **Specs**: `02-config-revision.md`, `01-config-load.md`
- **Scope**: Stage G (R-8): materialize +ruralz:default and registry defaults only inside present parents, normalization (sets sorted, durations and sizes canonical, host case), decode into v1alpha1 kinds and registry config types (RZ-CFG-005 on decode failure), RZ-CFG-005 number range via jsonval.CheckNumber on non-canonical trees after scalar normalization (02 req 16, 02 test plan item 3; R-64), hub.Bundle construction, conversion registry (identity for v1alpha1, round-trip property); idempotent on canonical input (R-47). 02 2.2 4-8, 2.3 9-14, 2.4 15-20, 2.10 53-54; 01 I 36-38.
- **Done when**: Round-trip property; defaults table equals schema markers; materialization idempotent on its own output.

##### WP-38 Precedence and effective chains (M)

- **Owns**: `internal/config/precedence/`
- **Depends on**: WP-01, WP-04
- **Specs**: `02-config-revision.md`, `04-dataplane-core.md`
- **Scope**: Stage I, sole owner of RZ-CFG-018/019/020/029/038 (R-46): build hub.Chain per Route (client Phases and per-Upstream legs) from Gateway, Route and Upstream attachments with slots, replaces/excludes, overridable, CheckPolicies for failureMode, Phase placement including authz.cel body detection through an injected oracle, cache guardrail (OQ-traffic-management-and-resilience-21 (a)), leg order rule, Removed reasons, Rows() for render --effective. 02 2.7 35-42; 04 F 40. Property: precedence laws.
- **Done when**: Worked-example chain golden (moved from WP-04); one fixture per code of 02 test 16; property seeds pass.

##### WP-39 Canonical form (M)

- **Owns**: `internal/config/canonical/`
- **Depends on**: WP-01, WP-02
- **Specs**: `02-config-revision.md`, `04-dataplane-core.md`, `10-cli.md`
- **Scope**: Stage L: ruralz.canonical.v1 encoder over the materialized tree (RFC 8785 ordering and numbers, no HTML escaping, CEL text byte-exact), strict decoder on jsonval to []tree.Resource with RoleCanonical for pipeline.FromResources (R-47; RZ-CFG-024 for a newer producer; no integer range rule on canonical content, R-64), /config/dump document (AppendDump, injected into the admin server, R-57), digest wiring with revision. 02 2.5 21-28, 2.6 29-34, 2.13 73-74. Fuzz FuzzCanonical (parse, canonicalize, parse identical); property: reorder invariance.
- **Done when**: Canonical bytes golden for the example Bundles; decode then encode is byte-identical; fuzz seeds pass.

##### WP-40 Telemetry runtime (L)

- **Owns**: `internal/telemetry/*.go` (package root only)
- **Depends on**: WP-09, WP-10, WP-11, WP-16, WP-28, WP-84
- **Specs**: `09-observability.md`, `04-dataplane-core.md`
- **Scope**: Runtime: environment scrub of OTEL_*, resource (service.name ruralzd, version, instance id = node.id, semconv v1.43.0 schema URL), providers, OTLP/gRPC trace, metric and log pipelines with bounded queues and otlp.tls via tlsconf (OQ-observability-2 (a)), otelslog bridge, /metrics handler over a private prometheus registry with the exporter and producer (bounded, OpenMetrics exemplars; injected into the admin server), degraded set and cleartext hops implementing emit.NodeStatus, Apply per Revision, Shutdown with 5 s flush, logger factory, runtime metrics. Metric name identity gate (09 req 76; 09 tests 32 and 33): a golden over one fixed registry, exported through /metrics as text 0.0.4 and OpenMetrics (test 32) and through OTLP into internal/testkit/otlpsink (test 33, ResourceMetrics normalized with protojson), asserting that the two metric name sets are equal. 09 2.1 1-8, 2.2 9-14, 2.3 15-26, 2.9 57-58, 2.10 59-60, 2.14 70; 09 tests 36-38.
- **Done when**: Export over TLS to testkit/otlpsink with the resource and no header leakage; collector down and stalled cases (09 tests 36-38); /metrics parsed by promtext; shutdown within budget; the metric name sets of /metrics and OTLP are equal (09 tests 32 and 33).

##### WP-42 Router (M)

- **Owns**: `internal/gateway/router/`
- **Depends on**: WP-05
- **Specs**: `04-dataplane-core.md`
- **Scope**: Implements snapshot.Router per listener: host tiers, path trie, candidate filters (methods, headers, when) over routematch.Criteria built with routematch.CriteriaOf (R-68), precedence total order, match.when errors RZ-RT-006, span ruralz.route.match, compile of 10,000 Routes within budget, zero-allocation Match. 04 D 26-33. Fuzz FuzzRouterMatch against a reference matcher.
- **Done when**: 04 worked example exact; 3 us p50 / 12 us p99 benchmark reported; fuzz seeds pass.

##### WP-43 Per-request exchange (L)

- **Owns**: `internal/gateway/exchange/`
- **Depends on**: WP-15, WP-18, WP-19
- **Specs**: `04-dataplane-core.md`, `07-transform.md`, `06-security.md`, `05-upstream-traffic.md`
- **Scope**: Pooled per-request state implementing filter.Exchange, filter.Message (client request, leg request, leg response, client response) and snapshot.RequestState (R-42): Enter/SetLeg, when decisions, per-Policy PolicyState slots (R-39), Stripe and RouteMetrics (R-56), leg views from Leg(upstream, step) with a Vars copy (upstream fields replaced), private slots and span, pooled and released; lazy expr.Vars population, header views joined per RFC 9110, body access through gates and budget, SetBody (Content-Length, Content-Encoding and digest removal), Decoded via expr.Compiler.DecodeJSON with budget filter.DecodedLimitFactor times the raw limit (R-63), identity once, rate-limit field accumulation, tee, ReplaceResponse, Final, access-record feed, an embedded emit.GatewayTimer (gateway_duration exclusion), reset for reuse. 04 E 34-38, G 46-48; 07 D 26-32, G 44-55 (message side); 06 2.1 rule 2.
- **Done when**: Reset leaves no state (reuse test under -race); concurrent leg views race-free; AllocsPerRun on a pass-through request.

##### WP-44 Listeners, servers and SO_REUSEPORT (M)

- **Owns**: `internal/gateway/listener/`; `internal/gateway/reuseport/`
- **Depends on**: WP-15, WP-16, WP-19, WP-21
- **Specs**: `04-dataplane-core.md`, `06-security.md`
- **Scope**: Listener set keyed by snapshot.ListenerKey: http.Server per listener (Protocols HTTP/1.1 + h2c or TLS h2, fixed timeouts, header limits), connection ceiling wrapper, PROXY v2 via clientaddr, TLS GetConfigForClient from the snapshot published by the retire holder (SNI, client-cert request mode, GOAWAY on mode change), Binder for activation (RZ-CFG-039 on failure), reuseport bind-without-listen and CBPF self-steering (Linux; stubs elsewhere). 04 B 9-18; 06 2.11 74-78.
- **Done when**: Rebind only on key change; cert rotation served on next handshake; CBPF steering test on Linux.

##### WP-45 Admin server, tap and readiness (M)

- **Owns**: `internal/gateway/admin/`; `internal/gateway/tap/`; `internal/gateway/readiness/`
- **Depends on**: WP-17
- **Specs**: `04-dataplane-core.md`, `09-observability.md`, `10-cli.md`
- **Scope**: Admin mux on admin.port (own server, never DefaultServeMux): /healthz, /readyz (adminapi reasons), /metrics, /config/dump and /debug/upstreams as injected handlers (R-57), /debug/snapshots, /debug/pprof, /tap NDJSON hub with per-subscriber sampling, dropped lines and subscriber limit RZ-RT-019 (R-24), readiness reason set, adminauth middleware, problem errors. 04 K 69-75, J 60-61; 09 2.13 69.
- **Done when**: Endpoint contract tests with adminapi types and fake injected handlers; tap backpressure test.

##### WP-46 Upstream runtimes and attempt loop (L)

- **Owns**: `internal/gateway/upstream/*.go` (package root only)
- **Depends on**: WP-15, WP-16, WP-18, WP-19, WP-22, WP-23, WP-24, WP-28
- **Specs**: `05-upstream-traffic.md`, `04-dataplane-core.md`, `06-security.md`
- **Scope**: Upstream manager and runtimes carried over by identity (discovery, health trackers, pickers, breaker, bulkhead, retry budget), transports per Upstream (pools, TLS via tlsconf, guarded dialer), Run scheduler, attempt loop Do(ctx, leg request, snapshot.LegRun) with deadlines and per-try timeouts, retries (retryOn, Filter Retry, replayable RequestBody, backoff), hashKey evaluation once per leg with random-Endpoint fallback and ruralz_upstream_cel_errors_total{field="hashKey"} (05 req 14), failureWhen, error classification and RZ-UP codes, outgoing request build (hop-by-hop removal, forwarding headers per clientaddr, Host = Endpoint authority or sni per R-28, Tracer.Inject, GetBody only for buffered bodies), pool metrics, upstream_panic and balancer_budget reasons. 05 A 1-4, C 14, E 24-26, F 27-31, G 32-35, J 41, R 94-95; 06 2.12 (upstream TLS).
- **Done when**: Retry, breaker, hashKey-error and replay-refusal scenarios table-tested against httptest upstreams with a fake LegRun; 05 test 35 subset (retries across Endpoints, carry-over across a reload).

##### WP-47 auth.jwt and JWKS (L)

- **Owns**: `internal/filter/auth/jwt/`
- **Depends on**: WP-02, WP-06, WP-16
- **Specs**: `06-security.md`
- **Scope**: auth.jwt Factory and Filter: compact parser, alg allow list RS256/PS256/ES256/EdDSA, jws verification with jwk/jws/jwa only (no jwt/jwe packages), claims decoded with jsonval, fixed skew 60 s and 24 h lifetime (R-32), audiences, issuer config, Consumer binding via identity; jwks subpackage: Node-wide JWKS manager as a filter.Component (fetch through egress, cache-control clamp, refresh, prefetch, limiter, jwks_stale degraded, age metric). 06 2.3 23-33, 2.4 34-40.
- **Done when**: Token matrix tests (every RZ-AUTH-00x path) with a local JWKS server; Component Run stops all goroutines; no jwt/jwe in go list -deps.

##### WP-48 auth.api-key and auth.basic (M)

- **Owns**: `internal/filter/auth/apikey/`; `internal/filter/auth/basic/`
- **Depends on**: WP-06
- **Specs**: `06-security.md`
- **Scope**: auth.api-key: header source only (default x-api-key; config.query is not registered in M1, 06 section 8 and OQ-configuration-model-19), digest lookup through the identity index, constant-time compare. auth.basic: PBKDF2 verify (iterations 600,000..1,000,000), Node-wide throttle as a filter.Component with success cache, per-source and per-username buckets, bounded hash queue, 429 RZ-AUTH-007 (OQ-security-and-identity-15 (a)). 06 2.5 41-44 and api-key rows of 2.2.
- **Done when**: Throttle bounds under a spraying simulation; timing-safe comparisons.

##### WP-49 auth.mtls, authz.ip and cors (M)

- **Owns**: `internal/filter/auth/mtls/`; `internal/filter/authz/ip/`; `internal/filter/cors/`
- **Depends on**: WP-06, WP-16, WP-18
- **Specs**: `06-security.md`
- **Scope**: auth.mtls: CA and CRL compile from secrets with Node-wide watches kept across carry-over (R-55; crl_stale reason), chain verification with per-connection ConnCache, RFC 4514 subject and SAN extraction, 421 RZ-AUTH-008 without a requested certificate. authz.ip: allow/deny prefix trie over source.ip, both-empty and CIDR checks. cors: preflight 204, origin matching, credentials rules, RZ-RT-008. 06 2.6 45-52, 2.8 58-63 (Filter side), 2.9 64-67.
- **Done when**: Verification cache hit/miss test; CRL rotation reaches a carried-over Filter; CORS conformance table; trie property test.

##### WP-50 auth.upstream-oauth2 (M)

- **Owns**: `internal/filter/auth/upstreamoauth2/`
- **Depends on**: WP-02, WP-16
- **Specs**: `06-security.md`
- **Scope**: Client-credentials token sources shared Node-wide by (tokenUrl, client, scopes) in a token registry filter.Component, fetch through egress with https only (RZ-CFG-037 checked in validate) and no redirect (the client secret goes only to its tokenUrl origin, R-49), refresh ahead of expiry with jitter, single flight, failure 401 RZ-AUTH-020 (OQ-data-plane-8 (a)), Authorization injection in onUpstreamRequest, age and refresh-failure metrics. 06 2.10 68-73.
- **Done when**: Token rotation and outage tests against a local token endpoint; a redirecting token endpoint never receives the secret at the second origin.

##### WP-51 validation.json-schema (M)

- **Owns**: `internal/filter/validation/jsonschema/`
- **Depends on**: WP-02, WP-28
- **Specs**: `07-transform.md`
- **Scope**: Factory compiling config.schema with jsonschema/v6 (refusing loader, Go regexp, draft 2020-12, size and fan-out bounds), instance decode with jsonval under the buffer budget (decoded values at most filter.DecodedLimitFactor times the raw limit, R-63), onRequestBody validation with 400 RZ-RT-009 and keyword-location detail, exported Check (hub.PolicyCheck) for offline RZ-CFG-005. 07 J 74-77, K 78-84.
- **Done when**: Validation matrix incl. oversized and non-JSON bodies; compile budget met; Check findings carry spec.config paths.

##### WP-52 Test kit load generation and balancing (M)

- **Owns**: `internal/testkit/l4lb/`; `internal/testkit/loadgen/`
- **Depends on**: WP-26
- **Specs**: `11-ops-quality.md`
- **Scope**: l4lb (readyz-probing TCP balancer with redispatch, idle timeout and connection-to-backend attribution), loadgen (open-loop HTTP/1.1, h2c and h2 load, coordinated-omission-free log-linear histogram, key sources, Missed accounting, validity check). 11 section 3, F 32 (harness parts).
- **Done when**: 11 test plan items 6 and 8 pass.

##### WP-53 Examples (S)

- **Owns**: `examples/`
- **Depends on**: WP-28
- **Specs**: `11-ops-quality.md`, `01-config-load.md`
- **Scope**: examples/quickstart (SM-7 path, quickstart.sh with RURALZ_FETCH_ALLOW), examples/file-mode (T2/T5 Bundle with overlays and systemd install README), examples/shop-bundle (CM example verbatim) and examples/control (environments.yaml, clusters.yaml) using only v1alpha1 fields; README per example. 11 M 106-109.
- **Done when**: Each example parses and schema-validates with the committed schema (full pipeline check by WP-75 once WP-69 lands).

##### WP-54 Release tooling and packaging (M)

- **Owns**: `internal/tool/releasekit/`; `internal/tool/depgate/`; `deploy/container/`; `deploy/systemd/`
- **Depends on**: WP-27
- **Specs**: `11-ops-quality.md`, `06-security.md`
- **Scope**: releasekit (package, checksums, notes with budgets and skipped gates, verify manifest, reproducibility compare); depgate notices subcommand and testkit/tool denylist per binary, jwx and testcontainers transitive license classification (go.yaml.in/yaml/v3, option/v3); Dockerfile (scratch, released binaries, CA bundle per OQ-tech-stack-and-libraries-25 (a)); systemd unit, env file, sysctl drop-in and ruralzd-handover helper (OQ-zero-downtime-upgrades-and-hot-reload-7 (a)). Workflows are WP-96's. 11 J 83-85, K 88-89, L 91-94, 99-100, 103.
- **Done when**: make release-dry-run produces a verified manifest with deterministic archives (11 test plan items 3, 4); depgate denylist catches an injected testkit import.

##### WP-65 State Store manager and post-commit queue (M)

- **Owns**: `internal/statestore/manager/`; `internal/statestore/postcommit/`
- **Depends on**: WP-13, WP-28
- **Specs**: `08-statestore.md`, `05-upstream-traffic.md`
- **Scope**: manager: effective configuration resolution (driver, topology, url secretRef, RURALZ_STATE_STORE_URL, memory fallback degraded), drivers keyed by Role with the cache connection (R-19), created through injected statestore.Opener per DriverKind (R-54) with Deps.MACKey from the setting (R-49), reuse on unchanged config, snapshot.StoreHandle with retain/release (Handle is a pointer type; a snapshot gets a new handle per role or the previous instance carried over without a second Retain, which stays for post-commit queue items; 2.14), SetNodeCount hook (M2), shutdown order. postcommit: 64,000-item four-class queue with byte caps, eight writers, batch 64, drop counters by Write label, flush at Drain within 5 s. 08 2.1 1-6, 2.8 55-57, 2.10 61-63.
- **Done when**: Hot Reload reuse and upgrade tests with fake openers and the memory driver; queue caps enforced; flush bound met.

##### WP-85 State Store test servers and container flavors (L)

- **Owns**: `internal/statestore/statestoretest/redisserver/`; `internal/statestore/statestoretest/respproxy/`
- **Depends on**: WP-26
- **Specs**: `08-statestore.md`, `11-ops-quality.md`
- **Scope**: redisserver: process flavor launching local redis-server (RURALZ_TEST_REDIS_SERVER or PATH) as standalone, replica with scripted failover (REPLICAOF NO ONE behind a faultproxy address), TLS (testkit/pki), ACL user, three-primary cluster (CLUSTER MEET, ADDSLOTSRANGE); container flavor on testcontainers-go v0.44.0 with images pinned by digest: Redis 8, Valkey 9.0.1 or newer, Dragonfly, three-primary Redis and Valkey clusters; selection RURALZ_TEST_STATESTORE=process|container (process when a binary is found, container when Docker answers, else skip with a reason). respproxy: RESP command counting and fault proxy on testkit/faultproxy (tap). 11 E 29 (flavors, CI wiring through WP-27's stage 8 container leg); 08 6.4 (launchers), risk 10; 11 E 31 (counting proxy).
- **Done when**: Process flavors start, answer PING and stop cleanly locally (standalone, replica failover, TLS, ACL, cluster); container flavor compiles, is unit-tested with a fake Docker probe, skips with a reason here and runs in the CI container leg; respproxy counts EVALSHA, GET and HMGET exactly.

##### WP-88 Response Cache revalidation and coalescing (M)

- **Owns**: `internal/filter/cache/revalidate/`; `internal/filter/cache/coalesce/`
- **Depends on**: WP-13
- **Specs**: `05-upstream-traffic.md`, `08-statestore.md`
- **Scope**: revalidate: Node-wide pool as a filter.Component (256-entry queue, 4 workers, drop when full), partition lease through statestore.CacheLease (SET NX PX 5000; never released by refresh or end-stale, it only expires, R-71), replay through filter.Replayer with If-None-Match/If-Modified-Since, 304 refresh under the lease, variant delete otherwise (05 req 86, R-43). coalesce: miss coalescing for up to 4,096 (U, P, V) keys, leader and followers, per-follower deadline, fallback to own fetch (05 req 88). 05 P 86, 88.
- **Done when**: Lease contention, queue-full drop and replay outcomes with filtertest.ReplayFunc and the memory driver; coalescing under -race.

#### Wave 4: Configuration assembly, CEL Filters, cache, composition, forwarder, handler, drain, redis driver, topology

##### WP-41 Redis State Store driver (L)

- **Owns**: `internal/statestore/redis/`
- **Depends on**: WP-13, WP-16, WP-85
- **Specs**: `08-statestore.md`, `05-upstream-traffic.md`, `11-ops-quality.md`
- **Scope**: rueidis client with the required options, Ruralz dialer (net.Dialer with tlsconf.StateStore, not the egress guard, spec 06 req 84; dials only the URL's host, R-49), URL validation (credentials rule, topology fit RZ-CFG-026), ruralz_v1.lua library (R-20; scripts f and x never DEL the revalidation lease, which only expires, R-71) with SCRIPT LOAD per primary, EVALSHA only, NOSCRIPT to RZ-STS-002 plus single-flight reload, 8,192 in-flight ceiling, per-shard breaker and pacing, cluster slot map and same-slot verification, INFO memory poller and store-byte admission rules (N_c = 1,000 in M1; only the memory driver uses 1, R-72), entry MAC (keys), metrics by OpKind.Label and RoundTrip.Label on the request stripe (R-56), degraded reasons. 08 2.1 1-12, 2.2 13-26, 2.5 39-46, 2.7 52-54, 2.11 64-67, 2.12 68-69.
- **Done when**: Conformance suite (statestoretest) and 08 6.4 items 1-8 pass on the process flavors (standalone, TLS, ACL, cluster, failover) with local redis-server 7.0.15 (integration tag); round-trip counts verified by respproxy; the same suites pass in the CI container leg (Redis 8, Valkey 9.0.1+, Dragonfly, Redis and Valkey clusters; 08 6.4 item 10).

##### WP-55 Loader (M)

- **Owns**: `internal/config/loader/`
- **Depends on**: WP-03, WP-33, WP-36
- **Specs**: `01-config-load.md`
- **Scope**: Stages A to E: sources (directory, one-file Bundle, stdin for CLI), discovery and .ruralzignore, limits (files, bytes, depth), envelope and identity (kind, apiVersion, name rules, RZ-CFG-007/008/016/017), Environments for CLI renders (RZ-CFG-022 promotion cycles, R-46), overlay application, substitution with Options.Variables, bounded worker parallelism. 01 A 1-6, D 16-20, E 21-22, N 51-55, O 56.
- **Done when**: Loader conformance cases (negative corpus subset) pass; deterministic output independent of file order.

##### WP-56 Semantic validation (L)

- **Owns**: `internal/config/validate/`
- **Depends on**: WP-04, WP-05, WP-06, WP-38
- **Specs**: `01-config-load.md`, `02-config-revision.md`, `03-cel.md`, `05-upstream-traffic.md`, `06-security.md`, `07-transform.md`
- **Scope**: Stages H, J, K (R-46: never 018/019/020/029/038) and the ruralzd-only serve check: reference resolution (RZ-CFG-009), cross-resource rules (listeners, hosts via routematch, identical match RZ-CFG-023 over routematch.Criteria built with routematch.CriteriaOf (R-68), composition limits RZ-CFG-031/032, https certificate presence, jwksUrl/tokenUrl https RZ-CFG-037, identity static checks), RZ-CFG-034 over each Route's hub.Chain at stage K (01 J row 034, gated by 01 req 51), secret use collection with kinds, destinations (AIProvider credentials.apiKey bound to its baseUrl origin already in M1, R-49) and source positions (hub.SecretUse.Loc from the tree node, as for CELUse.Loc) and the binding check RZ-CFG-041 (R-49), CEL sites (expr.Site from chains; one site per headers set[] and add[] entry, 07 req 24) checked with expr.Compiler, injected hub.Checks (R-34). The serve check RZ-CFG-040 is a separate pass that the pipeline runs after stage M only with Options.Serving (R-75); it covers the complete M1 list of section 0 item 8 (Policy types with Served false: plugin, authz.opa, authz.cedar, authz.geoip, auth.upstream-sigv4, ai.*; every Plugin, AIProvider and AIModel resource, referenced or not, at kind; an Upstream spec.protocol other than http and spec.discovery.type kubernetes; Gateway listeners[].http3 true; Route match.grpc, match.graphql and match.topic), each reported at the field with 'is not served by this release (Planned (Mx))' as registry.CheckServed does. 01 J 39-40, K 41; 02 2.7 (validation side); 03 D 26-30 (sites); 05 T 97; 06 2.2 static rules, 2.14 (binding); 07 C 20-25, F 38-43 (via Checks).
- **Done when**: Negative corpus codes and paths exact, including an API key reused as a clientSecret (RZ-CFG-041); all findings of a stage collected; the verbatim example Bundle raises no error in stages H to K, and the serve check reports RZ-CFG-040 once per unserved field.

##### WP-57 Diff (M)

- **Owns**: `internal/config/diff/`
- **Depends on**: WP-03, WP-39
- **Specs**: `02-config-revision.md`, `10-cli.md`
- **Scope**: Structural diff of two canonical Revisions keyed by identity and keyed lists, impact classes from +ruralz:impact (security class per OQ-security-and-identity-22 (a), including secret destination changes, 02 R-64/R-66), stable ordering, text and JSON models consumed by the CLI. 02 2.12 58-72. Property: diff laws (identity, symmetry, composition).
- **Done when**: Diff golden cases; property seeds pass.

##### WP-58 Render (M)

- **Owns**: `internal/config/render/`
- **Depends on**: WP-33, WP-37, WP-38, WP-39
- **Specs**: `02-config-revision.md`, `10-cli.md`
- **Scope**: bundle render outputs: rendered Bundle (profile.Encode, $${ preserved), --effective --route NAME table from precedence Rows, --api-version conversion (identity in M1), build artifact (canonical plus digest). 02 2.8 43-49, 2.9 50-52, 2.10 53-54, 2.11 55-57. Property: render idempotence with $${.
- **Done when**: Render golden outputs; idempotence property passes.

##### WP-59 authz.cel and headers (M)

- **Owns**: `internal/filter/authz/cel/`; `internal/filter/header/`
- **Depends on**: WP-06, WP-18, WP-28, WP-34
- **Specs**: `06-security.md`, `07-transform.md`, `03-cel.md`
- **Scope**: authz.cel: rule at PlaceAuthzCELRule, 403 RZ-AUTH-010, undecided RZ-AUTH-015, onRequestBody placement when the rule reads request.body. headers: Factory, Filter and exported Check (hub.PolicyCheck) for request and response add/set/remove with CEL values (add[] valueExpressions compiled at the same places as set[], PlaceHeadersRequestValue and PlaceHeadersResponseValue, with RuleFailureMode; a nil Value is absent and a non-nil "" an explicit empty value, R-62), protected and hop-by-hop names refused, Upstream-scope forwarding-header edits only. 06 2.7 53-57; 07 A 1-9 (headers), C 20-25, D 26-32.
- **Done when**: Decision and header matrix tests with real CEL programs (celtest).

##### WP-60 Transforms (L)

- **Owns**: `internal/filter/transform/`
- **Depends on**: WP-02, WP-18, WP-28, WP-34
- **Specs**: `07-transform.md`, `03-cel.md`
- **Scope**: Shared engine (Spec, Check, Compile, Program.Handle), dotpath grammar and tree operations on jsonval, request and response Factories (subpackages request and response) with exported Check (hub.PolicyCheck), body rewrite with budget reservation (decoded values at most filter.DecodedLimitFactor times the raw limit, R-63) and header recomputation, query and path edits, per-attempt isolation on leg views at Upstream scope, failure semantics RZ-RT-011/012. 07 F 38-43, G 44-55, H 56-68, I 69-73, L 85-88, M 89-91. Fuzz FuzzDotPath.
- **Done when**: Transform golden cases; compile budget benchmark; fuzz seeds pass.

##### WP-61 ratelimit (L)

- **Owns**: `internal/filter/ratelimit/`
- **Depends on**: WP-13, WP-18, WP-28, WP-34
- **Specs**: `05-upstream-traffic.md`, `08-statestore.md`
- **Scope**: Consumptive Filter: key program, Node-wide key table as a filter.Component with local token buckets at the per-Node ceiling (OQ-traffic-management-and-resilience-16 (b)), first-seen budget and local-only entries (-20 (a)), config.localOnly (OQ-scalability-and-distributed-state-11 (a)), GCRA calls through statestore.Call, tokens taken kept in PolicyState for Undo (R-39), fail-open windows, RZ-RL-001/002/005, RateLimit fields via AddRateLimitField (R-18), metrics. 05 M 56-66, N 67-68. Property: admission with a deterministic clock.
- **Done when**: Property seeds pass; Undo returns exactly the tokens Prepare took; stage budget 2 us p50 on the memory store.

##### WP-62 quota (M)

- **Owns**: `internal/filter/quota/`
- **Depends on**: WP-13, WP-18, WP-34
- **Specs**: `05-upstream-traffic.md`, `08-statestore.md`
- **Scope**: Consumptive Filter for unit requests: key program, Consumer quotas read from expr.Consumer.QuotaByName (a missing Consumer or quota is 403 RZ-RL-004), reservation with key digest and window start kept in PolicyState (R-39), denial cache, refunds in onLog through the post-commit Enqueuer when a later Policy rejected or the final code is RZ-UP-005/006/008, Finish releases the state, failureMode open default (OQ-configuration-model-8 (a)), RZ-RL-003/004, RateLimit fields. 05 O 69-74.
- **Done when**: Refund and denial-cache tests with filtertest.Store and the memory driver.

##### WP-63 Response Cache Filter (L)

- **Owns**: `internal/filter/cache/*.go` (package root only)
- **Depends on**: WP-13, WP-18, WP-34, WP-88
- **Specs**: `05-upstream-traffic.md`, `08-statestore.md`
- **Scope**: cache Filter (Phases onRequestHeaders and onResponse, plus Finisher, R-40): RFC 9111 shared-cache directives, key program (error bypasses), partitions by principal, pipelined lookup with lookup state in PolicyState (R-39), hot layer, coalescing (WP-88), stale-while-revalidate enqueue (WP-88), stale-if-error (ReplaceResponse), only-if-cached 504 RZ-RT-018, tee in onResponse and store in Finish (CacheStore through the post-commit queue), unsafe-method invalidation in Finish (05 req 85), hit reservation release, store-byte admission (OQ-traffic-management-and-resilience-11 (a) with interim rules), ruralz_cache_requests_total through Exchange.RouteMetrics (R-56), uses BuildEnv.CacheStore (cache role). 05 P 75-85, 87, 89-90; 08 2.7 52-54 (admission use).
- **Done when**: RFC 9111 table tests; invalidation after a 2xx POST observed in Finish; store skipped counter on budget exhaustion.

##### WP-64 Composition (L)

- **Owns**: `internal/gateway/composition/`
- **Depends on**: WP-02, WP-19, WP-34, WP-46
- **Specs**: `05-upstream-traffic.md`, `03-cel.md`
- **Scope**: snapshot.Forwarder for composition Routes: aggregate, sequential and conditional modes, step when and steps variables, per-step units and buffer reservations (decoded step bodies at most filter.DecodedLimitFactor times the step's maxBodyBytes, R-63), step legs through the upstream attempt loop with one LegRun each (BeginLeg; concurrent aggregate legs share nothing mutable, R-42), gated client body replayed to every step that sends one (05 req 54), merge under group with jsonval, partial responses (ruralz-partial), RZ-UP-011, RZ-RT-010, RZ-RT-015. 05 K 42-55.
- **Done when**: Mode matrix tests with httptest upstreams under -race; budget release on every path.

##### WP-66 Request handler and replay (L)

- **Owns**: `internal/gateway/handler/`; `internal/gateway/replay/`
- **Depends on**: WP-05, WP-15, WP-18, WP-19, WP-20, WP-21, WP-42, WP-43, WP-45
- **Specs**: `04-dataplane-core.md`, `09-observability.md`, `05-upstream-traffic.md`
- **Scope**: handler: http.Handler implementing the 04 req 34 order: unit, deadlines, pin, trace, header limit and count, framing and path/host normalization per 3.4 step 4 (CONNECT 404 RZ-RT-001 first; routematch.NormalizePath(routematch.RequestPath(r.URL)); errors through errcode.CodeOf and errcode.Status, RZ-RT-017 or RZ-RT-001), source.ip, match, dispatch table keyed by Route.Protocol (R-59), Route deadline, early 413, executor client Phases, Outbound fill and Forward (R-41), onResponse, problem documents for generated responses, RateLimit field append (sfv), commit under PinnedRequest.Mu with RZ-RT-014/016, streaming copy with pooled buffers, onLog and Finish, metrics on the stripe (Reset of the exchange's emit.GatewayTimer at request start and Observe(end, stripe, ListenerMetrics.GatewayDuration, Node().GatewayDurationSkipped) at the end; ListenerMetrics.Requests.Inc(stripe, protocol, status, emit.OriginOf(code, fromUpstream))), accessLog.when evaluation and access record (R-48), tap publish; handlertest package with AllocsPerRun fixtures for the bench suite. replay: filter.Replayer pinning the published snapshot, Route and LegChains lookup, synthetic exchange, one leg through Snapshot.Legs (R-43). 04 C 19-25, E 34-38, L 76-80; 05 P 86 (replay side); 09 2.12 66-68 (when).
- **Done when**: In-process tests with httptest upstreams and a fake Forwarder; replay test proves the snapshot stays pinned until the leg ends; PB-8 AllocsPerRun at or under 30.

##### WP-67 Drain and sd_notify (M)

- **Owns**: `internal/gateway/drain/`; `internal/gateway/sdnotify/`
- **Depends on**: WP-14, WP-19, WP-21, WP-44, WP-45
- **Specs**: `04-dataplane-core.md`, `11-ops-quality.md`
- **Scope**: drain: SIGTERM/SIGINT timeline (readiness off, STOPPING=1, lock release, 5 s stop accepting with one GOAWAY, 25 s deadline via the ending protocol with RZ-RT-016, flush hooks, exit by 30 s, second signal jumps), timelines parameterized for the handover holder. sdnotify: READY=1 after the first boot attempt with STATUS= reason (11 risk 9), STOPPING=1, MAINPID=. 04 A 8, J 60-62; 11 G 46 (hooks).
- **Done when**: Drain timeline with clocktest.Fake; sd_notify datagrams verified against a unixgram socket.

##### WP-68 CLI admin client (M)

- **Owns**: `internal/cli/adminclient/`
- **Depends on**: WP-16, WP-25, WP-39
- **Specs**: `10-cli.md`
- **Scope**: Admin URL validation (loopback cleartext guard), TLS from RURALZ_ADMIN_TLS_DIR or flags, bearer token file, no redirects, 256 MiB dump cap, problem document errors, /config/dump decode through canonical, /tap NDJSON stream with dropped lines, /readyz. 10 2.8 62-69.
- **Done when**: Contract tests against an httptest admin server using adminapi types.

##### WP-86 Plain-upstreams forwarder and upstream debug (M)

- **Owns**: `internal/gateway/upstream/forward/`
- **Depends on**: WP-46
- **Specs**: `05-upstream-traffic.md`, `04-dataplane-core.md`
- **Scope**: snapshot.Forwarder for plain upstreams: weighted leg pick (RZ-UP-008 when every weight is 0), BeginLeg per leg, attempt loop call, response streaming with bulkhead slot release, UpstreamResponse fields for logs; snapshot.LegRunner per snapshot for replay (R-43); /debug/upstreams JSON handler injected into the admin server (R-57). 05 C 10 (weighted splits), F 27-30, S 96.
- **Done when**: Weighted split distribution and zero-weight tests; slot released on every Close path; debug JSON golden.

##### WP-87 Test kit topology (M)

- **Owns**: `internal/testkit/topology/`
- **Depends on**: WP-26, WP-52, WP-84, WP-85
- **Specs**: `11-ops-quality.md`
- **Scope**: Compose the test kit into T1, T2, T5-host and chaos topologies of Nodes, State Store (redisserver behind faultproxy), mocks, IdP and OTLP sink, with per-Node port overlays rendered through ${E2E_*} variables, symlink swap, restart, handover, scrape and canary scan; cleanup on t.Cleanup (Drain, then kill after 35 s, logs kept on failure). 11 section 3, F 32, 42-43.
- **Done when**: Topology smoke test starts two stub processes, a redis-server and the sink, and tears down cleanly with goroutines back to baseline.

#### Wave 5: Pipeline facade, snapshot compiler, built-in tables, handover

##### WP-69 Configuration pipeline facade (M)

- **Owns**: `internal/config/pipeline/`
- **Depends on**: WP-34, WP-35, WP-37, WP-39, WP-55, WP-56
- **Specs**: `01-config-load.md`, `02-config-revision.md`, `03-cel.md`
- **Scope**: Run(ctx, Options) for stages A to M with gating and full-stage collection, Options (sources, Environment, Variables, Compiler, Checks as hub.Checks, limits, worker count, and Serving, set only by ruralzd, which runs config/validate's serve check RZ-CFG-040 after stage M, R-75), FromResources re-entering at stage F from canonical content with the byte-identity and digest check (R-47), hub.Validated output with Revision, SecretUses (with destinations) and CEL sites; identical diagnostics for every binary. 01 N 51-55; 02 2.1 1-3, 2.15 77-78.
- **Done when**: Example Bundles validate; YAML and JSON forms of one Bundle yield the same digest; FromResources of a Run's canonical output yields the same Validated digest.

##### WP-70 Snapshot compiler (L)

- **Owns**: `internal/gateway/compile/`
- **Depends on**: WP-06, WP-34, WP-42, WP-46, WP-47, WP-48, WP-49, WP-50, WP-51, WP-59, WP-60, WP-61, WP-62, WP-63, WP-64, WP-65, WP-86
- **Specs**: `04-dataplane-core.md`, `05-upstream-traffic.md`, `06-security.md`, `07-transform.md`, `03-cel.md`
- **Scope**: hub.Validated plus secret.Store to snapshot.Snapshot: Consumers, CEL Builder seeded with the previous ProgramSet, Compiler.PrepareConsumer for every compiled Consumer and Compiler.PrepareRoute for every Route once its expr.Route is filled, one Filter per (Policy, scope, Phase set) via filter.Registry with carry-over by identity and canonical config, BuildEnv.Replayer, compiled Policies with When and SpanName, BodyNeeds, Route.Protocol (R-59), LegChains by Upstream and Legs (R-43), Routers, forwarders (upstream/forward, composition), listener TLS and RequestClientCert, Gateway limits (header cap, header_limit_capped), metric Plan (Shape.CachedRoutes filled from each Route's effective chains, hub.Chain), State Store handles (a new handle per snapshot and role, or the previous instance carried over without retaining it again, 2.14); W workers yielding every 100 us (OQ-performance-budgets-and-benchmarking-6 (a)). 04 I 55-56 (compile part); 05 A 1-4 (compile); 06 2.11 (listener TLS).
- **Done when**: 10,000 Routes compile within 2 s on one core; carry-over keeps Filter instances and their secret watches; real-CEL tests for every Filter type.

##### WP-71 Built-in Filter tables (S)

- **Owns**: `internal/filter/builtin/`
- **Depends on**: WP-47, WP-48, WP-49, WP-50, WP-51, WP-59, WP-60, WP-61, WP-62, WP-63, WP-88
- **Specs**: `07-transform.md`, `00-architecture.md`
- **Scope**: builtin.New(Deps) returns a *filter.Registry with every served PolicyType's Factory and the Node-wide Components (JWKS manager, token registry, basic throttle, rate-limit key table, cache revalidation pool; R-45) for ruralzd; builtin/checks.Table() returns hub.Checks for headers, transform.request, transform.response and validation.json-schema, importing only those packages so the CLI links no JOSE or State Store code (R-34); tests that the served set equals the registry served flags and that checks do not import jwt or statestore drivers (go list -deps).
- **Done when**: Table consistency test with the registry; Components listed once each.

##### WP-72 CLI node commands (S)

- **Owns**: `internal/cli/node/`
- **Depends on**: WP-14, WP-68
- **Specs**: `10-cli.md`
- **Scope**: node drain (Linux only: read holder.json, verify PID namespace, start time and the FLOCK WRITE line in /proc/locks, signal through a pidfd, await the PID; darwin and windows exit 2 per 10 req 97) and node dump (/config/dump to file or stdout). 10 2.11 93-100, 2.12 101-103.
- **Done when**: Command tests with a fake holder and httptest admin; GOOS=windows and darwin builds exit 2 as specified.

##### WP-89 Zero-Downtime handover (L)

- **Owns**: `internal/gateway/handover/`
- **Depends on**: WP-14, WP-19, WP-21, WP-26, WP-44, WP-45, WP-52, WP-67
- **Specs**: `04-dataplane-core.md`, `11-ops-quality.md`
- **Scope**: ruralz.handover.v1 over handover.sock with SO_PEERCRED: ready, refused (older candidate, unknown_marker, draining, 60 s timeout), reload, accepted, 1 s usage reports, listening, CBPF steering swap, 3 s linger, released; shared ceilings from usage reports, cgroup soft memory limit, successor MAINPID then READY, holder Drain through gateway/drain; format marker constant in its own file for -overlay variants. 04 A 8, J 62-68; 11 G 44-45 (hooks).
- **Done when**: Two-process handover test on Linux (a test binary re-exec with listener and handover, testkit/proc and loadgen) with zero failed requests on loopback load; each refusal reason table-tested.

#### Wave 6: Activation and Hot Reload, CLI bundle, configuration conformance

##### WP-73 Activation, Hot Reload and Last-Known-Good (L)

- **Owns**: `internal/gateway/reload/`; `internal/gateway/lkg/`; `internal/gateway/source/`
- **Depends on**: WP-07, WP-14, WP-17, WP-21, WP-44, WP-45, WP-69, WP-70
- **Specs**: `04-dataplane-core.md`, `02-config-revision.md`, `01-config-load.md`
- **Scope**: Loader goroutine with pending slot and K gate (Holder.WaitActivate before compiling the latest pending Revision, 3.3 step 2), verify, pipeline with Options.Serving set on every run, including LKG and handover through FromResources (R-75), secret resolution (hub.SecretUse.Loc copied into secret.Use.Loc for RZ-CFG-026 positions), compile, bind via listener Binder, Binding set then publish (R-58), resolver Activate, LKG store (content and pointers, fsync protocol, format markers, cleanup, retry with lkg_write_failed), boot order incl. lkg_boot through FromResources, file-mode polling watcher (R-36) and SIGHUP, readiness reasons, activation metrics and revision info. 04 I 55-59; 01 N (runtime gating).
- **Done when**: PB-7 Hot Reload of 5,000 Routes measured (informational locally); failed reload keeps serving; LKG boot property (file mode) passes.

##### WP-74 CLI bundle commands (L)

- **Owns**: `internal/cli/bundle/`
- **Depends on**: WP-25, WP-34, WP-57, WP-58, WP-68, WP-69, WP-71
- **Specs**: `10-cli.md`, `02-config-revision.md`
- **Scope**: bundle validate, render (rendered, --effective --route, --api-version, --output-dir), diff (paths, URL sides via adminclient, impact summary), build (online Plugin stage behind OnlineChecker: RZ-CFG-028 per Plugin in M1); Environment selection, sources and limits flags, text and JSON outputs with exit codes. No command sets pipeline.Options.Serving, so the verbatim example Bundle validates, renders, diffs and builds without RZ-CFG-040 (R-75). 10 2.3 25-34, 2.4 35-39, 2.5 40-48, 2.6 49-57, 2.7 58-61.
- **Done when**: Command golden outputs; exit codes per 10 2.2.

##### WP-75 Configuration conformance and golden corpus (L)

- **Owns**: `test/conformance/config/`
- **Depends on**: WP-26, WP-53, WP-57, WP-58, WP-69, WP-71
- **Specs**: `11-ops-quality.md`, `01-config-load.md`, `02-config-revision.md`
- **Scope**: Golden corpus (canonical bytes, digests, render and effective outputs, diagnostics text and JSON) recorded after WP-28 defaults land (R-25 freeze), JSON subset, loader, hostile-input and negative corpora with exact codes and paths (every offline RZ-CFG code including 034 and 041; RZ-CFG-040 is Node-only, so the corpus never sets Options.Serving, R-75), examples validation through the pipeline; untagged tests in stage 5 on linux, floor and CLI platforms with identical digests. Cross-binary diagnostics equality is WP-79's. 11 B 12-17; 02 2.16 79.
- **Done when**: Corpus byte-exact on linux and floor jobs; every registered offline RZ-CFG code has at least one negative case.

#### Wave 7: Binaries wiring and cross-package properties

##### WP-76 ruralzd wiring (L)

- **Owns**: `internal/gateway/*.go` (package root only); `cmd/ruralzd/`
- **Depends on**: WP-07, WP-14, WP-34, WP-40, WP-41, WP-44, WP-45, WP-46, WP-53, WP-65, WP-66, WP-67, WP-70, WP-71, WP-73, WP-86, WP-89
- **Specs**: `04-dataplane-core.md`, `09-observability.md`, `08-statestore.md`, `06-security.md`
- **Scope**: gateway.Run supervisor: settings (MAC key file), telemetry Runtime, data dir and lock, handover successor path, admin server with injected /metrics, /config/dump (canonical AppendDump over the published snapshot) and /debug/upstreams handlers (R-57), State Store manager with memory.Open and redis.Open (R-54), secret resolver, upstream manager, builtin.New with Deps and the replay service, running every filter.Component, resolver poller, upstream scheduler and post-commit writers on owned goroutines (R-45), CEL compiler, reload loop, listener set, signal handling (SIGHUP rescan, SIGTERM/SIGINT drain), shutdown order (components cancelled after the last snapshot retired), exit codes 0/1/2; cmd/ruralzd stays wiring only. The real gateway.Run never prints sizegate's M0 stub message ('Ruralz Gateway is not implemented yet; it is Planned (M1)'). Lead step at the wave 7 merge (convention 7; WP-76 may not edit internal/tool/sizegate, 4.1 rule 1): delete notImplemented, stubMessage and the skip branches in internal/tool/sizegate/rss.go and main.go, the 'M0 stub' table cases and TestNotImplemented in sizegate_test.go, and the -annotate skip-warning path (keep -annotate accepted as a no-op, or delete it together with ANNOTATE_FLAG in the Makefile); from then on the 89 MiB idle RSS gate is enforced. 04 A 1-4, I 58, J 60-63 (wiring).
- **Done when**: Binary boots on examples/file-mode and examples/quickstart, serves a proxied request and drains cleanly in a process test; goroutine count returns to baseline after shutdown; make gates GATES=size measures idle RSS on Linux.

##### WP-77 CLI launcher, dev commands and root (L)

- **Owns**: `internal/cli/launch/`; `internal/cli/dev/`; `internal/cli/*.go` (package root only); `cmd/ruralz/`
- **Depends on**: WP-25, WP-58, WP-68, WP-72, WP-74
- **Specs**: `10-cli.md`
- **Scope**: launch: private dir, render to one-file Bundle with atomic rename, watch and re-render, ephemeral ports mapping line, secret overrides, child ruralzd process and readiness, SIGTERM then SIGKILL, exit 130; dev run and dev tap; root Run with SignalContext, registry, root and noun help, planned-command table, version, completion wiring. 10 2.9 70-85, 2.10 86-92, 2.13 104-105, 2.15 109-112.
- **Done when**: Launcher tests against a stub ruralzd built from testdata (serves /readyz, /config/dump and a proxied route) pass, including ports, secret overrides, re-render and Ctrl-C paths; the real-binary T1 test is WP-79's.

##### WP-78 Cross-package property suites (M)

- **Owns**: `test/property/`
- **Depends on**: WP-26, WP-37, WP-42, WP-57, WP-58, WP-69, WP-73
- **Specs**: `11-ops-quality.md`, `02-config-revision.md`
- **Scope**: testing.F properties spanning packages: YAML and JSON digest equality and reorder invariance through pipeline.Run, conversion round trip through convert and canonical, render idempotence through the pipeline, diff laws over pipeline outputs, LKG file-mode property through reload with a fake source; seeds from testkit/corpus; each target registered for the nightly fuzz plan. Package-level targets stay with their owners (WP-33, 34, 36, 38, 39, 42, 57, 58, 61). 11 D 25-27.
- **Done when**: All seeds pass in stage 5; targets listed by fuzzplan.

#### Wave 8: End-to-end, Node integration, chaos, benchmarks, protocol conformance, T5, image checks

##### WP-79 End-to-end suite (L)

- **Owns**: `test/e2e/`
- **Depends on**: WP-52, WP-53, WP-76, WP-77, WP-87
- **Specs**: `11-ops-quality.md`, `10-cli.md`
- **Scope**: Scenarios 1, 2, 6, 7; one end-to-end test per M1 command (10 section 6.6); stage 8 process-level ruralz bundle validate over examples/ per Environment (integration tag, 11 req 1); cross-binary diagnostics equality (ruralz vs ruralzd, 11 req 16) on M1-servable fixtures or excluding RZ-CFG-040 (R-75); secret leak canaries in every run (11 req 39); air-gapped process variant (11 req 40); T1 real-binary dev run and T2 topologies; Hot Reload and LKG (11 req 38); handover and Drain verification with zero failed requests (11 G 44, 46-48, reduced counts); quickstart under 10 minutes (SM-7). 11 F 32-43, G 44-48.
- **Done when**: make e2e green locally with local redis-server; every M1 command covered; leak scan finds nothing.

##### WP-80 Chaos experiments and chaos at scale (L)

- **Owns**: `test/chaos/*.go` (package root only); `test/chaos/testdata/`; `.github/workflows/chaos-scale.yml`
- **Depends on**: WP-41, WP-52, WP-76, WP-87
- **Specs**: `11-ops-quality.md`, `08-statestore.md`, `05-upstream-traffic.md`
- **Scope**: CE-1 to CE-6, CE-12, CE-15, CE-16 with faultproxy, signals and redis commands, at a scale chosen by RURALZ_CHAOS_SCALE=reduced|nightly|target (localOnly and first-seen assertions per OQ-scalability-and-distributed-state-11 (a)); every asserted degraded state visible as a metric. chaos-scale.yml (weekly, workflow_dispatch, workflow_call) on the self-hosted RH-1 runner, runs-on [self-hosted, rh-1] (OQ-testing-and-quality-strategy-11 (b), user decision 2026-10-03), serialized with the latency rotation; until the repository variable RH1_PROVISIONED is true it fails with the 'RH-1 not provisioned' annotation nightly.yml uses: CE-5 with at least 100 Nodes (11 req 54), CE-15 at 100,000 new keys per second (req 57) and CE-12 at 1,000,000 calls per second (WP-97 provisions RH-1). 11 H 49-58.
- **Done when**: Every experiment passes at reduced scale locally and at 10 Nodes in the nightly Chaos job; chaos-scale.yml passes actionlint and runs CE-5 at 100 Nodes on RH-1 once RH1_PROVISIONED is true.

##### WP-81 Benchmark harness, gates and records (L)

- **Owns**: `test/bench/harness/`; `test/bench/allocgate.json`; `test/bench/record.schema.json`
- **Depends on**: WP-26, WP-27, WP-52, WP-66, WP-76
- **Specs**: `11-ops-quality.md`, `04-dataplane-core.md`, `09-observability.md`
- **Scope**: Macro harness with a LoadTool adapter (oha primary with --latency-correction, vegeta cross-check for S1; OQ-performance-budgets-and-benchmarking-1 (a)), measurement rules (rate ladder, warm-up stability, five interleaved A/B runs, validity checks; 11 req 73), macro p99 gate and absolute gate over every budget, throughput target, scenario criterion and constants row (req 74), latency job rotation (req 75), result records and publication (req 77), budget table for PB-1 to PB-4, PB-6 to PB-11, PB-14 to PB-16 with SLO links (req 71), soak driver (S2 at half saturation for 2 hours, RSS drift and goroutines; req 79), memory runs (req 78), component and CEL benchmark registration (req 80), PB-7 and PB-8 measurement hooks, cold-start and Hot Reload ladder measurements (req 81), budget export for releasekit notes (req 99); allocgate.json lists every package benchmark with owners, including BenchmarkSampledTrace15Spans (internal/telemetry/tracing, owner WP-10) at 90 allocs/op (R-69); record.schema.json. The harness is host-agnostic and dry-runs locally with the Go generator (numbers informational). 11 I 62-80.
- **Done when**: Stage 9 alloc gate runs on shared runners; harness dry run on a fixture scenario validates records against the schema; latency job defined for rh-1.

##### WP-82 Protocol conformance (M)

- **Owns**: `test/conformance/protocol/`
- **Depends on**: WP-26, WP-76
- **Specs**: `11-ops-quality.md`, `04-dataplane-core.md`, `06-security.md`
- **Scope**: HTTP/1.1 and HTTP/2 wire suite with x/net/http2 Framer and hpack: framing rejections RZ-RT-017, header limits, hop-by-hop removal, h2c prior knowledge, GOAWAY on drain, Host and X-Forwarded-Host per R-28, encoded NUL and backslash and percent-encoded control bytes (OQ-security-and-identity-21 (a)), PROXY v2 LOCAL accepted whatever its family byte (R-65), TLS cases. 11 C 21-24.
- **Done when**: 100% of cases pass against a built ruralzd (integration tag).

##### WP-90 Benchmark scenarios, R1 generator and functional variants (L)

- **Owns**: `test/bench/scenarios/`; `test/bench/gen/`; `test/bench/functional/`
- **Depends on**: WP-69, WP-76, WP-87
- **Specs**: `11-ops-quality.md`, `04-dataplane-core.md`, `05-upstream-traffic.md`
- **Scope**: Scenario Bundles S1, S2 (PBB verbatim with the lab-host overlay), S3, S5, S5x, O1 (O1a, O1b), O2, C1, C2, G1 and the soak scenario, using only CM fields, validated in stage 5 through pipeline.Run with recorded digests (11 req 69-70); R1 ladder generator for R = 100 to 17,000 with the alternating Revisions (req 81); functional reduced-scale variants run locally and in the nightly job with pass criteria scaled from req 72 (O1 admitted plus RZ-RT-005 within offered, O2 in-flight plateau at a lowered ceiling with skipped counted, C1 connection memory rows, C2 existing-connection p99 under a handshake burst) and the constants runs of req 82 at reduced scale (stream 251 refused, plain 431, pre-routing writes, K = 2 under activations, 512 MiB buffered bytes flood, rate-limit key table, post-commit queue, State Store in-flight). 11 I 69-72, 81-82.
- **Done when**: Every scenario Bundle validates with a recorded digest; R1 Bundles generate deterministically; functional variants and constants runs pass at reduced scale locally (numbers never published as budget results).

##### WP-91 Node integration: CEL and telemetry (M)

- **Owns**: `test/integration/cel/`; `test/integration/telemetry/`
- **Depends on**: WP-26, WP-76, WP-84, WP-85
- **Specs**: `03-cel.md`, `09-observability.md`, `11-ops-quality.md`
- **Scope**: Against a built ruralzd (integration tag, stage 8): the per-place CEL runtime-error matrix (03 test 25: RZ-RT-006 without fallthrough, RZ-AUTH-010/015, RZ-RL-004/005, RZ-RT-011/012/015, ruralz_upstream_cel_errors_total per field, accessLog.when error writes the entry, Policy.spec.when errors under closed and open, asserted through ruralz_filter_failures_total{phase,mode}, the access log failure_modes and the ruralz.filter.<name> span attributes ruralz.failure_mode and error.type, with no span event, R-67) with memory and redis drivers; Hot Reload rejecting RZ-CFG-014 and reusing programs (03 test 26); ADR10 blocked-stdout test (09 test 39); telemetry Hot Reload A to B to none (09 test 40); the roadmap cardinality test: 1,000 Hot Reloads of a 1,000-Route Bundle with pinned streams (09 test 41, req 56); gateway_duration excludes the GCRA round trip (09 test 42).
- **Done when**: Every listed test passes locally with redis-server 7.0.15 and the OTLP sink; the cardinality test keeps retiring series at or under 25,000 and at most 2 revision_info series.

##### WP-92 Node integration: security (M)

- **Owns**: `test/integration/security/`
- **Depends on**: WP-26, WP-76, WP-84, WP-85
- **Specs**: `06-security.md`, `11-ops-quality.md`
- **Scope**: 06 6.5 against ruralzd with TLS listeners: JWKS rotation, stale keys and prefetch; mTLS request-mode switch on Hot Reload with GOAWAY and 421; upstream TLS and mTLS, wrong CA RZ-UP-002; auth.upstream-oauth2 single fetch under 200 concurrent requests and forced expiry; State Store TLS with requirepass, state_store_unauthenticated, uncredentialed non-loopback URL RZ-CFG-026; admin 9901 matrix over real sockets; basic-auth throttle under load; secret-to-destination binding (a Bundle reusing a key as clientSecret rejected, RZ-CFG-041) and entry MAC with RURALZ_STATE_STORE_MAC_KEY_FILE (tampered entries are misses) (R-49).
- **Done when**: Every 06 6.5 case and the R-49 cases pass locally (integration tag).

##### WP-93 Node integration: State Store and round trip (M)

- **Owns**: `test/integration/statestore/`
- **Depends on**: WP-26, WP-41, WP-76, WP-85
- **Specs**: `05-upstream-traffic.md`, `08-statestore.md`, `11-ops-quality.md`
- **Scope**: Node-level State Store scenarios per available flavor (process locally; container flavors in the CI stage 8 leg): NOSCRIPT after SCRIPT FLUSH applies failureMode with exactly one command and reloads off path (05 test 30, 11 req 30); cluster same-slot script, different-slot sequential stop at first deny, never CROSSSLOT (05 test 31); a full post-commit queue drops with counters and never delays the response (11 req 30); the ADR8 round-trip test through respproxy between a Node and redis-server: zero commands for local denials and over-limit or denial-cache hits, at most one blocking command per admitted request per Policy, one shared script for same-slot keys, two pipelined commands per cache lookup (05 test 34, 08 6.4 item 9, 11 req 31).
- **Done when**: Every scenario passes on the process flavors locally and on every container flavor in CI.

##### WP-94 T5 systemd verification (M)

- **Owns**: `test/t5/`; `.github/workflows/t5-systemd.yml`
- **Depends on**: WP-52, WP-54, WP-76, WP-87, WP-89
- **Specs**: `11-ops-quality.md`, `04-dataplane-core.md`
- **Scope**: systemd-analyze verify of deploy/systemd with ExecStart rewritten to a built binary (integration tag, stage 8 through make integration, skipped with a reason without systemd-analyze); live test (build tag t5) run by t5-systemd.yml on a GitHub-hosted Ubuntu runner with sudo (weekly, workflow_dispatch, workflow_call): install the unit and helper, start with a valid Bundle and with none, 20 systemctl reload handovers under load with the 9901 keep-alive checker (zero failed requests, MainPID equals the successor, old PID exits by 28 s), a refused handover (non-zero reload, service stays on the old PID), systemctl stop within 40 s with status 0, systemctl kill -s SIGKILL then automatic restart booting Last-Known-Good (11 req 86; OQ-zero-downtime-upgrades-and-hot-reload-7 (a) evidence before 0.1.0).
- **Done when**: systemd-analyze test green locally; t5-systemd.yml passes actionlint and its run is green on GitHub before the 0.1.0 release candidate.

##### WP-95 Compose end-to-end and air-gapped image start (M)

- **Owns**: `test/compose/`; `.github/workflows/image-checks.yml`
- **Depends on**: WP-53, WP-54, WP-76, WP-77, WP-84
- **Specs**: `11-ops-quality.md`, `09-observability.md`
- **Scope**: Docker Compose file-mode suite (build tag compose): ruralzd image built from dist binaries with deploy/container/Dockerfile (or a given image digest), redis, a mock upstream image built from testkit/mockup, and an OpenTelemetry Collector with a file exporter; covers the roadmap scenarios (quickstart, --effective --route, bundle diff against /config/dump, Zero-Downtime Upgrade) and 09 test 48 (spans, metrics and logs reach the Collector; the requestId of a 404 finds its access-log line and trace); air-gapped image variant: the image by digest on a Docker --internal network with only the fixtures, every M1 feature path serving (11 req 40). image-checks.yml runs nightly and via workflow_call with an image-digest input (stage 12). R-51.
- **Done when**: image-checks.yml passes actionlint and runs green on GitHub (Docker); the suite skips with a reason here.

##### WP-99 Game-day drills and failure rows (M)

- **Owns**: `test/chaos/gameday/`
- **Depends on**: WP-41, WP-52, WP-76, WP-87
- **Specs**: `11-ops-quality.md`, `08-statestore.md`, `05-upstream-traffic.md`, `06-security.md`
- **Scope**: GD-1 to GD-5 automated drills (restart RTO, zone loss with scripted failover, black-hole, partition, State Store rebuild), failure rows FC-16 (JWKS unreachable), FC-19 (DNS failure with the stdlib DNS responder), FC-20 (file mode), FC-21 (file secret missing at cold start), and the TQ row 'Upstream Endpoints reset or slowed' with RZ-UP codes; run by the nightly Chaos job. 11 H 59-61.
- **Done when**: Every drill and row passes at reduced scale locally and at 10 Nodes nightly, each degraded state asserted as a metric.

#### Wave 9: Release pipeline and RH-1 budget runs

##### WP-96 Stage 12 release workflows and gates (L)

- **Owns**: `.github/workflows/release.yml`; `.github/workflows/release-build.yml`; `.github/workflows/release-verify.yml`; `internal/tool/releasegate/`
- **Depends on**: WP-27, WP-54, WP-79, WP-80, WP-81, WP-82, WP-90, WP-91, WP-92, WP-93, WP-94, WP-95, WP-99
- **Specs**: `11-ops-quality.md`, `06-security.md`
- **Scope**: release.yml on v* tags (release candidates and releases): re-run pr-fast, pr-full, main and nightly on the tagged commit through workflow_call with all: true (11 req 7, 11), including the secret leak assertions of stage 10 (req 39); t5-systemd.yml and chaos-scale.yml on the candidate; release gates on the candidate commit through releasegate (req 105: every stage green, golden byte-exact, conformance 100%, zero open fuzz-crasher issues and a seed per fixed crasher, every chaos experiment passed on this commit, security scanning green, benchmarks within 5% p99 and 3% alloc/op of the previous release or, for 0.1.0, the absolute budgets from WP-97 records, else a 'Skipped gates' entry with an Environment reviewer's approval); the ADR2 release audit per tag (req 100: repocheck no-license scan, depgate G2/G3 per binary and platform, LICENSE, NOTICE and THIRD_PARTY_LICENSES per archive and image); release-build.yml reusable workflow (stage 4 flags with -s -w, archives, SHA256SUMS, schema assets, Syft CycloneDX 1.7 SBOMs, cosign keyless signing, GitHub attestations, images pushed by digest; req 88-97, 104 permissions, Environment release); image-checks.yml with the release image digest (Compose and air-gapped image start; req 40); release-verify.yml (req 98); GitHub Release as a draft until every gate and verification passes, then published (req 101). releasegate is a stdlib tool with a fake GitHub API in tests. The lead deletes internal/tool/modpin before this wave (R-61).
- **Done when**: actionlint clean; releasegate unit tests green; a workflow_dispatch dry run on a test tag with signing and publishing disabled is green on GitHub.

##### WP-97 RH-1 provisioning, budget runs and soak (M)

- **Owns**: `deploy/benchhost/`
- **Depends on**: WP-81, WP-90
- **Specs**: `11-ops-quality.md`, `00-architecture.md`
- **Scope**: Maintainer package (4.1 rule 7): provision RH-1 per the PBB hardware spec under OQ-performance-budgets-and-benchmarking-2 (b) (rented bare metal, owner decision), with host preparation scripts (cpusets, governor, sysctls, NIC settings, fingerprint check) under deploy/benchhost/, and register an ephemeral self-hosted runner labeled rh-1 that runs only scheduled or dispatched jobs from protected branches and tags, never pull-request code; run the nightly Latency rotation (11 req 75), the gating runs of PB-1 to PB-4, PB-6 to PB-11, PB-14 to PB-16 (req 71-74), the soak (req 79), CE-12 at 1,000,000 calls per second with the RH-1 State Store host (req 56), publish the records (req 77) and export the budget results for the 0.1.0 notes (req 99).
- **Done when**: Latency job green on rh-1 with published records showing PB-1 to PB-4 and PB-7 met (exit criteria 2 and 3), soak and CE-12 full-rate results recorded; until funded, the job fails with the 'RH-1 not provisioned' annotation and exit criteria 2 and 3 stay open.

#### Wave 10: Release candidate and 0.1.0 cut

##### WP-98 0.1.0 release candidate, cut and repository settings (S)

- **Owns**: `scripts/repo-settings.sh`; `docs/engineering/04-release-versioning-and-compatibility.md` (release runbook section only)
- **Depends on**: WP-96
- **Specs**: `11-ops-quality.md`
- **Scope**: Maintainer package (4.1 rule 7): scripts/repo-settings.sh applies and reads back, through gh api, the branch protection of main and develop with pr-fast and pr-full as required checks and the merge queue (11 req 4), the GitHub Environment release with required reviewers from outside the author's team and tag deployment rules (req 104, 105), GHCR package write for the release image job only; the release runbook section records the steps. Then tag v0.1.0-rc.1 (signed release candidate) at least 2 weeks before v0.1.0 (req 90), run release.yml on it and fix until green, and tag v0.1.0; confirm SM-12 (100% of artifacts signed) with release-verify (exit criterion 1). Release notes carry WP-97's budget results, or list the latency gate under 'Skipped gates' with its approver when RH-1 is not provisioned (M1 then ships 0.1.0 but does not exit, criteria 2 and 3).
- **Done when**: Settings read back as specified; v0.1.0-rc.1 and v0.1.0 releases published with every artifact signed and verified.

## 5. Third-party modules

The lead adds every module below to `go.mod` before wave 2 with `go get module@version` (no work package edits `go.mod` or `go.sum`), together with `internal/tool/modpin/modpin.go`: a `//go:build tools` file that blank-imports one package of each direct module, so `go mod tidy` at a wave boundary keeps requirements whose first importer lands in a later wave (goccy, jsonschema, jwx, rueidis, prometheus, grpc, OTLP and testcontainers until wave 3 or 4, `golang.org/x/net` until wave 8) (R-61). Verified on 2026-09-26 in a scratch module: `go mod tidy` keeps a requirement imported only from a `//go:build tools` file, keeps `go 1.26.0`, and `go list -deps` of the binary does not contain it. The lead deletes `modpin` before wave 9, when every module has a real importer. Verified on 2026-09-25 in a scratch module: every version exists on proxy.golang.org, `go mod tidy` keeps `go 1.26.0`, `go build` with go1.27.1 succeeds with every package below imported, and `golang.org/x/crypto` is not among the linked packages (jwx imported as `jwk`, `jws`, `jwa` only).

Direct requirements:

| Module | Version | Catalog row (docs/engineering/01-tech-stack-and-libraries.md) | Imported by | Linked into |
|---|---|---|---|---|
| `github.com/goccy/go-yaml` | v1.19.2 | YAML 1.2 parser | `internal/config/profile` | all binaries |
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.3 | JSON Schema validator | `internal/config/schemaview`, `internal/filter/validation/jsonschema` | all binaries |
| `cel.dev/cel-go` | v0.32.0 | Expressions | `internal/cel/...` | all binaries |
| `github.com/lestrrat-go/jwx/v4` | v4.5.0 | JOSE (WP-01 lists its transitive modules) | `internal/filter/auth/jwt/...` (`jwk`, `jws`, `jwa` only) | `ruralzd` |
| `github.com/redis/rueidis` | v1.0.78 | State Store client | `internal/statestore/redis` | `ruralzd` |
| `go.opentelemetry.io/otel` (with `/trace`, `/metric`, `/sdk`, `/sdk/metric`, `/semconv/v1.43.0`) | v1.46.0 | Telemetry | `internal/telemetry/...`, `internal/testkit/otlpsink` (tests only) | `ruralzd` |
| `go.opentelemetry.io/otel/log`, `go.opentelemetry.io/otel/sdk/log` | v0.22.0 | Telemetry (pre-stable Logs SDK behind the bridge) | `internal/telemetry`, `internal/testkit/otlpsink` (tests only) | `ruralzd` |
| `go.opentelemetry.io/contrib/bridges/otelslog` | v0.20.1 | Telemetry | `internal/telemetry` | `ruralzd` |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc`, `.../otlpmetric/otlpmetricgrpc` | v1.46.0 | Telemetry (OTLP export) | `internal/telemetry`, `internal/testkit/otlpsink` (tests only) | `ruralzd` |
| `go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc` | v0.22.0 | Telemetry (OTLP export) | `internal/telemetry`, `internal/testkit/otlpsink` (tests only) | `ruralzd` |
| `go.opentelemetry.io/otel/exporters/prometheus` | v0.68.0 | Metrics exposition (new row, WP-01; OQ-tech-stack-and-libraries-16 decided) | `internal/telemetry`, `internal/telemetry/aggregate` (tests only) | `ruralzd` |
| `github.com/prometheus/client_golang` | v1.24.1 | Metrics exposition (same row) | `internal/telemetry` (private registry), `internal/telemetry/aggregate` (tests only) | `ruralzd` |
| `github.com/prometheus/common` | v0.71.0 | Metrics exposition (same row; `expfmt`) | `internal/telemetry` | `ruralzd` |
| `github.com/prometheus/otlptranslator` | v1.0.0 | Metrics exposition (same row; translation strategy constant) | `internal/telemetry`, `internal/telemetry/aggregate` (tests only) | `ruralzd` |
| `google.golang.org/grpc` | v1.84.0 | gRPC upstream client (row's first use moves from M3 to M1 for OTLP/gRPC export, WP-01) | `internal/telemetry`, `internal/testkit/otlpsink` | `ruralzd` |
| `golang.org/x/sys` | v0.48.0 | Zero-Downtime Upgrade socket steering (row widened to `SO_PEERCRED` and test namespaces, WP-01) | `internal/gateway/reuseport`, `internal/gateway/handover`, `internal/testkit/proc` | `ruralzd` |
| `go.opentelemetry.io/proto/otlp` | v1.11.0 | Telemetry (OTLP protocol module, named in the row by WP-01) | `internal/testkit/otlpsink` (tests only) | none |
| `golang.org/x/net` | v0.59.0 | HTTP/1.1 and HTTP/2 (`x/net/http2` low-level APIs such as `Framer`) | `test/conformance/protocol` (tests only) | none |
| `github.com/testcontainers/testcontainers-go`, `github.com/testcontainers/testcontainers-go/modules/redis`, `github.com/testcontainers/testcontainers-go/modules/valkey` (three modules) | v0.44.0 each | Test tooling (not shipped) | `internal/statestore/statestoretest/redisserver` container flavor (WP-85; Redis 8 and Valkey 9.0.1 or newer through the modules, Dragonfly and the three-primary clusters through `GenericContainer`; CI stage 8 container leg). Verified 2026-09-26: the three resolve on proxy.golang.org and `go vet` passes with `go 1.26.0` kept | none |

Resolved indirect requirements (from `go mod tidy`, recorded for the license gate; `cel.dev/expr` is admitted by depguard because cel-go's API exposes it, and confined to `internal/cel`): `cel.dev/expr` v0.25.2, `github.com/antlr4-go/antlr/v4` v4.13.1, `github.com/beorn7/perks` v1.0.1, `github.com/cenkalti/backoff/v5` v5.0.3, `github.com/cespare/xxhash/v2` v2.3.0, `github.com/go-logr/logr` v1.4.4, `github.com/go-logr/stdr` v1.2.2, `github.com/google/uuid` v1.6.0, `github.com/grpc-ecosystem/grpc-gateway/v2` v2.30.0, `github.com/lestrrat-go/dsig` v1.4.0, `github.com/lestrrat-go/option/v3` v3.0.0-alpha1, `github.com/munnerz/goautoneg` v0.0.0-20191010083416-a7dc8b61c822, `github.com/prometheus/client_model` v0.6.2, `github.com/prometheus/procfs` v0.21.1, `github.com/valyala/fastjson` v1.6.10, `go.opentelemetry.io/auto/sdk` v1.2.1, `go.opentelemetry.io/otel/exporters/otlp/otlptrace` v1.46.0, `go.yaml.in/yaml/v3` v3.0.5, `golang.org/x/exp` v0.0.0-20240823005443-9b4947da3948, `golang.org/x/text` v0.42.0, `google.golang.org/genproto/googleapis/api` and `/rpc` v0.0.0-20260819154853-08b0e4226688, `google.golang.org/protobuf` v1.36.12. Test-only requirements that appear in the module graph but are not linked: `github.com/dlclark/regexp2` v1.11.0 (jsonschema tests), `github.com/onsi/gomega` (rueidis tests), `golang.org/x/crypto` v0.57.0 (graph only). The license gate (G2) must classify `go.yaml.in/yaml/v3` (MIT and Apache-2.0 in one file) and the pre-release `option/v3` (WP-54).

Not modules: CLI parsing is stdlib `flag` (OQ-tech-stack-and-libraries-18); ULID is own code on `crypto/rand` (OQ-tech-stack-and-libraries-15); the file watcher is stdlib polling; `flock` is stdlib `syscall`. CI binaries pinned by `scripts/install-tools.sh` (WP-27), not in `go.mod`: promtool, oha v1.16.0, vegeta, cosign v3.1.3, Syft v1.52.0, actionlint. Container images pinned by digest: `golang:1.27.1-bookworm` (CA source, WP-54), `redis:8`, `valkey/valkey:9.0.x`, Dragonfly (WP-85), an OpenTelemetry Collector image (WP-95). GitHub actions pinned by SHA: `actions/checkout` v7.0.1, `actions/setup-go` v7.0.0 (existing), `actions/upload-artifact`, `actions/download-artifact`, `actions/attest-build-provenance`, `actions/attest-sbom`, `docker/setup-buildx-action` (WP-27, WP-96). Every tool, image and action gets its catalog or CI tooling row from WP-32 before first use.

## 6. M1-blocking open questions

M1 exit criterion 7 requires every open question blocking an M1 feature to be closed: the 32 rows whose Blocking column names M1 (the 30 whose Blocking column reads M1 plus OQ-data-plane-8 and OQ-security-and-identity-9, which block the pack 8.10 amendment before M1), plus OQ-configuration-model-8 and OQ-security-and-identity-2, -3, -6, -15 and -18. The table gives the adopted option, what the code does, and the work package that records the closure in the owning document (the row's document; pack amendments are recorded by WP-29 in `docs/_meta/foundation-pack.md` under pack section 14). Code work packages implement the option regardless of when the document lands.

| Open question | Adopted option | Code consequence | Recorded by |
|---|---|---|---|
| OQ-cli-and-api-surface-10 | (a) a file under `${RURALZ_DATA_DIR}`: `holder.json` (`ruralz.holder.v1`: PID, start time, version, `pidNamespace`) | `nodedir` (WP-14); `node drain` reads it (WP-72); systemd helper (WP-54), verified live (WP-94) | WP-30 (`docs/reference/01-cli-and-api-surface.md`) |
| OQ-configuration-model-8 | (a) as registered: `quota` open, `ai.token-budget` closed | Registry defaults (WP-04), materialized in canonical form | WP-29 (`docs/architecture/02-configuration-model.md`) |
| OQ-data-plane-2 | (a) at most one leading `*.` label, no other `*` | `routematch.CheckTemplate`/host rules, RZ-CFG-005 (WP-05, WP-56) | WP-30 (`docs/architecture/03-data-plane.md`) |
| OQ-data-plane-8 with OQ-security-and-identity-9 | (a) 401 RZ-AUTH-020 for a failed `upstream-auth` Policy in M1 (pack 8.10 binding; (b) 503 needs a pack amendment and is a one-row registry change later) | Status only through `errcode.Status` (WP-50, WP-20) | WP-30 (`03-data-plane.md`, `08-security-and-identity.md`) |
| OQ-data-plane-9 | (a) pack 8.6 `RT` means request and response handling on a Node outside Upstream legs | RZ-RT-011 to 019 stay in `RT` | WP-30 (row), WP-29 (pack 8.6) |
| OQ-observability-2 | (a) `telemetry.otlp` fields, M1 limited to `otlp.tls` with the `UpstreamTLS` shape (`sni`, `caCertificate`, `clientCertificate`, `clientKey`); authentication headers later | Schema (WP-28), OTLP client TLS via `tlsconf` (WP-40), cleartext hop reason when absent | WP-31 (`docs/architecture/10-observability.md`), field text WP-29 |
| OQ-observability-16 | (a) external producer (`sdkmetric.Producer`) over Ruralz-owned aggregates | `telemetry/aggregate` (WP-09), exporter wiring (WP-40), cardinality test (WP-91) | WP-31, ADR-0010 amendment WP-32 |
| OQ-performance-budgets-and-benchmarking-1 | (a) oha primary, vegeta cross-check, fortio (M3); a statistics tool after research | `LoadTool` adapter in the macro harness (WP-81); tools pinned (WP-27); CI tooling rows (WP-32) | WP-32 (`docs/architecture/12-performance-budgets-and-benchmarking.md`) |
| OQ-performance-budgets-and-benchmarking-2 | (b) rented bare metal meeting the RH-1 spec (owner decision recorded; (a) stays the long-term home) | WP-97 provisions the host, registers the `rh-1` runner and runs the budgets and soak; until then the Latency job (WP-27) fails with an explicit "RH-1 not provisioned" annotation and exit criteria 2 and 3 stay open | WP-32 |
| OQ-performance-budgets-and-benchmarking-6 | (a) W = max(1, `GOMAXPROCS`/2) loader workers yielding every 100 µs | `gateway/compile` (WP-70), PB-7 measured (WP-73, WP-81) | WP-32 |
| OQ-release-versioning-and-compatibility-3 | (a) cosign v3.1.3 and Syft v1.52.0; CycloneDX 1.7 only, no SPDX in `0.1.0`; provenance through GitHub artifact attestations | Packaging (WP-54); `release.yml`, `release-build.yml`, `release-verify.yml` (WP-96); tool pins (WP-27); first signed release candidate (WP-98) | WP-32 (`docs/engineering/04-release-versioning-and-compatibility.md`, tooling rows in the tech stack) |
| OQ-repository-layout-and-conventions-6 | (a) native arm64 runners (`ubuntu-24.04-arm`), fallback (c) native nightly | `pr-full` stage 8 matrix and arm64 size gate (WP-27); `pr-full` a required check (WP-98) | WP-32 (`docs/engineering/02-repository-layout-and-conventions.md`) |
| OQ-scalability-and-distributed-state-2 | (a) standalone with replicas, and sharded (cluster); no Sentinel | `redis` driver topologies (WP-41), launchers and container flavors (WP-85), Node scenarios (WP-93), chaos (WP-80, WP-99) | WP-31 (`docs/architecture/11-scalability-and-distributed-state.md`) |
| OQ-scalability-and-distributed-state-3 (M1 part) | (a) a cache connection under `Gateway.spec.stateStore` (`stateStore.cache`) | Schema (WP-28), manager roles (WP-65), cache uses `BuildEnv.CacheStore` (WP-63); absent: main store plus interim rules and a startup WARN | WP-31, field text WP-29 |
| OQ-scalability-and-distributed-state-10 | (a) TCP fault proxy and signals in the harness (Ruralz Go proxy, with OQ-testing-and-quality-strategy-3 (a)) | `testkit/faultproxy` (WP-26), `respproxy` (WP-85), chaos (WP-80, WP-99) | WP-31 |
| OQ-scalability-and-distributed-state-11 | (a) all: OQ-traffic-management-and-resilience-16 (b), -20 (a), -19 (c), `config.localOnly`, the `rzplg:` namespace | `ratelimit` (WP-61), `localOnly` schema (WP-28), `rzplg:` reserved in `statestore/keys` (WP-13) | WP-31, pack 8.7/8.8/8.11 WP-29 |
| OQ-security-and-identity-1 | (b) fixed defaults for M1 (R-32) | `auth.jwt` constants (WP-47); no schema change | WP-30 (`docs/architecture/08-security-and-identity.md`) |
| OQ-security-and-identity-2, -3, -18 | (a) register as authored (already answered in the configuration model) | Schemas exist; semantics in WP-48, WP-49 | WP-30 confirms the rows read answered |
| OQ-security-and-identity-6 | (c) both: `trustedProxies` CIDR list and `listeners[].proxyProtocol` | `clientaddr` (WP-15) | WP-30 |
| OQ-security-and-identity-7 | (a) `RURALZ_ADMIN_TOKEN_FILE`, `RURALZ_ADMIN_METRICS_TOKEN_FILE`, `RURALZ_ADMIN_TLS_DIR` (pack section 2 amendment) | `adminauth` (WP-17), `setting` (WP-14), CLI admin client (WP-68) | WP-30, pack section 2 WP-29 |
| OQ-security-and-identity-15 | (a) local throttle with the documented defaults and residuals | `auth.basic` throttle (WP-48) | WP-30 |
| OQ-security-and-identity-21 | (a) reject encoded NUL and backslashes (and raw or percent-encoded control bytes, invalid escapes) with 400 RZ-RT-017 | `routematch.NormalizePath` (WP-05), protocol suite (WP-82) | WP-30 |
| OQ-security-and-identity-22 | (a) `RURALZ_SECRET_ROOT`, `RURALZ_SECRET_` prefix, `RURALZ_FETCH_ALLOW`, `security` impact class, secret-to-destination binding, State Store entry MAC key (pack sections 2 and 5 amendment), with the M1 definitions of R-49 for the two parts 06 left unspecified | Root and prefix: resolver (WP-07); fetch guard and no-redirect client: egress (WP-16); impact class: diff (WP-57); binding: `hub.SecretUse.Destination` (WP-01), RZ-CFG-041 at stage H (WP-56), token client (WP-50), State Store dialer (WP-41); MAC key: `RURALZ_STATE_STORE_MAC_KEY_FILE` (WP-14), manager (WP-65), HMAC (WP-13, WP-41); Node tests (WP-92). Every part is built, so the closure implies nothing unbuilt (06 risk 22) | WP-30, pack WP-29 |
| OQ-security-and-identity-24 | (a) `oauthClients` use the Consumer's `jwt` issuer; a Consumer with `oauthClients` and no `credentials.jwt` is RZ-CFG-005 | `identity` index (WP-06), validation (WP-56) | WP-30 |
| OQ-testing-and-quality-strategy-2 | (c) alloc/op on shared runners, latency on RH-1 | `benchgate` in stage 9 (WP-27), latency job (WP-81) | WP-32 (`docs/engineering/03-testing-and-quality-strategy.md`) |
| OQ-traffic-management-and-resilience-2 | (c) the data plane appends `RateLimit-Policy` and `RateLimit` fields after `onResponse` (R-18) | `Exchange.AddRateLimitField` (WP-01, WP-43), append in the handler (WP-66), `sfv` (WP-18) | WP-31 (`docs/architecture/09-traffic-management-and-resilience.md`) |
| OQ-traffic-management-and-resilience-5 (M1 part) | (a) `circuitBreaker.minimumLegs` 20, `failureRatio` 0.5, `halfOpenSuccesses` 3 registered; backoff and budget fixed; `hedgeDelay` M4 | Schema (WP-28), breaker (WP-24) | WP-31, field text WP-29 |
| OQ-traffic-management-and-resilience-6 | (c) per-protocol Route timeouts with (a)'s values; no (d) or (e) fields; static defaults as markers, `retryOn`/`failureWhen` runtime | Markers (WP-28), resilience (WP-24), upstream core (WP-46) | WP-31 |
| OQ-traffic-management-and-resilience-11 | (a) with the interim rules always on; every unsafe method invalidates; `s-maxage` as `proxy-revalidate` (R-33) | `cache` (WP-63), revalidation and coalescing (WP-88), store admission (WP-41, WP-13) | WP-31 |
| OQ-traffic-management-and-resilience-16 | (b) 2x headroom, floor 10, step refill per aligned window | `ratelimit` (WP-61) | WP-31, pack 8.8 WP-29 |
| OQ-traffic-management-and-resilience-19 | (c) handover and persistence under `${RURALZ_DATA_DIR}`; M1 file mode: count unknown, full-limit ceiling, 200 first-seen calls per second, not degraded | `ratelimit` (WP-61), `manager.SetNodeCount` hook (WP-65) | WP-31, pack 8.11 WP-29 |
| OQ-traffic-management-and-resilience-20 | (a) local-only entries at the per-Node ceiling in their own segment | `ratelimit` (WP-61) | WP-31, pack 8.8 WP-29 |
| OQ-traffic-management-and-resilience-21 | (a) reject with RZ-CFG-038 | `precedence` (WP-38) | WP-31; code row WP-01 |
| OQ-vision-and-positioning-10 | Decided: built-in `authz.ip` in M1, `authz.geoip` in M2 | `authz.ip` (WP-49); `authz.geoip` unserved, RZ-CFG-040 (WP-04) | WP-29 (row in `docs/_meta/foundation-pack.md`), WP-32 (vision text) |
| OQ-zero-downtime-upgrades-and-hot-reload-7 | (a) research through the live CI test, then a shipped unit and helper | `deploy/systemd` unit, drop-in and `ruralzd-handover` helper (WP-54); `sdnotify` (WP-67); handover (WP-89); `systemd-analyze verify` and the live T5 job `t5-systemd.yml` (WP-94), which must pass before the `0.1.0` release candidate and is re-run by `release.yml` (WP-96) | WP-30 (`docs/operations/02-zero-downtime-upgrades-and-hot-reload.md`) |

Non-blocking questions this architecture also settles (recorded by the same work packages): OQ-tech-stack-and-libraries-15 (own ULID), -16 (OTel Prometheus exporter), -18 (stdlib `flag`) by WP-01; OQ-tech-stack-and-libraries-25 (a) CA bundle in the image by WP-32; OQ-zero-downtime-upgrades-and-hot-reload-1 (a), -2 (b) RZ-RT-016, -3 (a), -4 (a) RZ-CFG-039, -12 (a) by WP-30; OQ-data-plane-6 (a), -12 (a), -13 (a) by WP-30; OQ-security-and-identity-23 and -30 (interim RZ-CFG-026) by WP-30; OQ-observability-1 (a) 0.01 by WP-31; OQ-traffic-management-and-resilience-23 (b) by WP-31; OQ-configuration-model-18 (a) and -19 (a) by WP-29; OQ-testing-and-quality-strategy-1, -3, -4, -5 (a), OQ-performance-budgets-and-benchmarking-3 (a), -4 (a) and OQ-repository-layout-and-conventions-4 (b) by WP-32.

## 7. Exit criteria and roadmap coverage

Every M1 exit criterion and every roadmap M1 item that needs more than one package maps to the work packages that produce its evidence. "ENV" marks what this environment cannot run (no Docker, no systemd as PID 1, no RH-1); each ENV item has an owning work package and a CI or hardware job, never only a local stand-in.

| Exit criterion or roadmap item | Evidence and owners | ENV dependency |
|---|---|---|
| 1. `0.1.0` for every production platform, 100% of artifacts signed (SM-12) | Packaging WP-54; `release.yml`, `release-build.yml`, `release-verify.yml`, release gates and audit WP-96; repository settings, `v0.1.0-rc.1` at least 2 weeks ahead, `v0.1.0` WP-98 | GitHub Actions (OIDC, GHCR, Environment `release`) |
| 2. S2 at half saturation: p99 1 ms, p50 150 µs on RH-1 | Harness and gates WP-81; scenarios WP-90; runs WP-97 | RH-1 (OQ-performance-budgets-and-benchmarking-2 (b), owner funding) |
| 3. S1 50,000 rps on 4 vCPU (PB-4); Hot Reload of 5,000 Routes 500 ms (PB-7) | WP-81, WP-90, WP-97; PB-7 measured informationally by WP-73 | RH-1 |
| 4. Quickstart first proxied request in 10 minutes (SM-7) | Examples WP-53; e2e scenario 1 WP-79; clean-runner job in `main` WP-27 | none (clean runner in CI) |
| 5. Golden corpus byte-exact | WP-75 | none |
| 5. Conformance 100% for shipped features | Configuration WP-75, WP-33 and WP-35 suites; protocol WP-82 | none |
| 5. No open fuzz crasher | Fuzz targets per package; property suites WP-78; nightly plan WP-27; release gate WP-96 | none |
| 5. End-to-end test for every M1 pack section 9 command | WP-79 (with WP-77's stub-based launcher tests) | none |
| 5. CE-1 to CE-6, CE-12, CE-15, CE-16 | WP-80 at reduced and 10-Node scale; CE-5 at 100 Nodes, CE-15 at target rate and CE-12 at 1,000,000 calls per second on RH-1 (`chaos-scale.yml`, WP-97) | RH-1 |
| 5. GD-1 to GD-5 | WP-99 (automated drills; human game days need a staging environment, 11 risk 12) | none for the drills |
| 6. Feature Catalog 100% milestones, Planned (M1) rows implemented | Catalog text WP-32; implementation by the code packages | none |
| 7. Every M1-blocking open question closed | Section 6; recorded by WP-29 to WP-32 and WP-83 | none |
| Configuration row: upstream test suites, meta-validation, hostile input | WP-33 (YAML Test Suite), WP-35 (JSON-Schema-Test-Suite, zero failures, meta test), WP-75 (hostile, loader, JSON subset) | none |
| CEL row: every M1 place, runtime behavior on a Node | WP-34 (compile, cost), WP-46 (`hashKey`, `retryOn`, `failureWhen`), WP-66 (`accessLog.when`), WP-91 (Node matrix) | none |
| Lifecycle row: T1, T2, T5 | T1 and T2 WP-79; T5 unit WP-54, live verification WP-94 | systemd as PID 1 (GitHub runner) |
| Security row: digest checks on every Revision and Plugin artifact | Revisions WP-17, WP-39, WP-73; Plugin artifact verifier WP-17 (wired to fetch in M2, R-50, recorded by WP-83) | none |
| Security row: redaction unit tests, secret leak tests | WP-11, WP-17 (unit); WP-79 (every e2e run), WP-96 (stage 12 re-run) | none |
| Traffic row: `memory` and `redis` drivers tested on Valkey 9.0.1 or newer and Dragonfly | Driver WP-41; flavors WP-85; stage 8 container leg WP-27; Node scenarios WP-93 | Docker (CI container leg) |
| Observability row: cardinality test | WP-91 (1,000 Hot Reloads of a 1,000-Route Bundle) | none |
| Quality row: round-trip test | WP-93 (Node to `redis-server` through `respproxy`) | none |
| Quality row: Docker Compose end-to-end test in file mode; air-gapped start | Process harness everywhere (WP-79, air-gapped process variant); Compose with a real OpenTelemetry Collector and the air-gapped image start WP-95 (R-51) | Docker (nightly and stage 12) |
| Quality row: CI stages 8 to 12 | Stages 8 to 11 WP-27; stage 12 WP-96 | GitHub Actions |
| Budgets row: scenarios S1, S2, S3, S5, S5x, O1, O2, C1, C2, R1, G1; microbenchmarks; soak; gates; records | Scenario Bundles, R1 generator and functional variants WP-90; harness, gates, soak driver and records WP-81; alloc/op and size gates WP-27; gating runs and soak WP-97 | RH-1 for gating numbers |
