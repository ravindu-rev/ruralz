---
title: Migration from KrakenD
status: reviewed
owner: ruralz-core
last_updated: 2026-09-25
depends_on:
  - docs/_meta/foundation-pack.md
  - docs/_meta/style-guide.md
  - docs/architecture/02-configuration-model.md
  - docs/comparison/01-krakend-ee-parity-matrix.md
adrs: [ADR-0002, ADR-0003, ADR-0005, ADR-0008, ADR-0010, ADR-0011, ADR-0014, ADR-0017]
milestone_tags_used: [M1, M2, M3, M4, M5]
---

# Migration from KrakenD

## Summary

This guide takes KrakenD Community Edition (CE) and Enterprise Edition (EE) configurations to Ruralz. It maps every top-level key and `extra_config` namespace of the v2.13 schema to Ruralz kinds, fields and Policy types, defines the four fidelity levels that `ruralz bundle import krakend`, Planned (M2), reports, sets two plugin migration paths, gives a cutover runbook with shadow traffic, side-by-side cutover and rollback, and lists the gaps. Nothing is implemented yet.

## Scope and non-goals

In scope: KrakenD CE v2.13.11 ([source](https://github.com/krakend/krakend-ce/releases)) and EE 2.13 ([source](https://www.krakend.io/blog/krakend-ee-2.13-release-notes/)) configurations at the 2026-09-23 snapshot, per the v2.13 JSON Schema ([source](https://www.krakend.io/schema/v2.13/krakend.json)). The [Configuration model](../architecture/02-configuration-model.md#coming-from-krakend) fixes the core mapping and delegates level definitions and the full mapping here. What the importer "emits" or a Node "does" is planned behavior.

Non-goals:

- **Parity decisions.** The [KrakenD EE parity matrix](01-krakend-ee-parity-matrix.md) owns them; this document follows its status column.
- **Field design.** Kinds, fields and Policy types belong to the [Configuration model](../architecture/02-configuration-model.md#kind-catalog); a missing field becomes an Open question, never an invented field.
- **Pre-2.0 KrakenD configurations.** They are first converted with KrakenD's `krakend-config-migrator` ([source](https://www.krakend.io/docs/configuration/migrating/)).
- **KrakenD CE 3.0 beyond Go plugin removal.** No CE 3.0 release tag existed at the snapshot, and other changes on the `dev-3.0` branch, where plugin removal was merged ([source](https://github.com/krakend/krakend-ce/pull/1106)), are uninvestigated.
- **Performance comparisons.** KrakenD's published benchmarks use the pre-2.0 `"version": 1` format ([source](https://www.krakend.io/docs/benchmarks/local/)); comparative numbers wait for the [Performance budgets and benchmarking](../architecture/12-performance-budgets-and-benchmarking.md) bench suite, Planned (M4).

KrakenD vocabulary appears verbatim where it names KrakenD objects: a KrakenD "endpoint" is a Ruralz `Route`, and a KrakenD "backend" is a Ruralz `Upstream` plus, when a request fans out, a composition step. <!-- alias-ok -->

*Figure 1: the migration path from a KrakenD configuration to decommission, with rollback edges.*

```mermaid
flowchart LR
    kd["KrakenD configuration rendered to JSON"] --> imp["ruralz bundle import krakend"]
    imp --> b["Bundle: Gateway, Route, Upstream, Policy, Consumer"]
    imp --> rep["Fidelity report"]
    rep --> fix["Resolve manual items, review approximate items"]
    fix --> b
    b --> val["ruralz bundle validate and ruralz bundle audit"]
    val --> tst["ruralz test run against KrakenD and Ruralz Gateway"]
    tst --> dark["Ruralz Gateway Nodes deployed dark"]
    dark --> sh["Shadow traffic"]
    sh --> cut["Side-by-side cutover by weight"]
    cut --> done["KrakenD decommissioned"]
    sh -.->|"rollback"| kdl["KrakenD serves all traffic"]
    cut -.->|"rollback"| kdl
```

## Concept mapping

A KrakenD configuration is one file: a service object, an `endpoints` array with backends, and `extra_config` namespaces at five scopes ([source](https://www.krakend.io/schema/v2.13/krakend.json)). A Ruralz Bundle is a directory of resources: one `Gateway`, one `Route` per published path and method, and named `Upstream` and `Policy` resources attached by reference ([Configuration model](../architecture/02-configuration-model.md#resource-model)). <!-- alias-ok -->

### Core concepts

| KrakenD concept | Ruralz concept | Planned | Notes |
|---|---|---|---|
| Service (root object of the file) | `Gateway` in `ruralz.yaml` | Planned (M1) | Exactly one per Bundle (RZ-CFG-016) |
| One configuration file | Bundle directory rendered into a Revision per Environment | Planned (M1) | Content-addressed, verified by digest ([ADR-0003](../adr/0003-configuration-format.md)) |
| endpoint (a published path and method) | `Route` with `match.path` and `match.methods` | Planned (M1) | One Route per endpoint entry <!-- alias-ok --> |
| backend | `Upstream` for the target service, plus a composition step when the Route fans out or rewrites | Planned (M1) | Upstreams are named and shared across Routes <!-- alias-ok --> |
| `host` list of a backend | `Upstream.spec.endpoints[].address` | Planned (M1) | An Endpoint is an address, never a path <!-- alias-ok --> |
| `extra_config` namespace | A `Policy` of one registered `type`, or a field of `Gateway`, `Route` or `Upstream` | Planned (M1) to Planned (M5) | Mapping per namespace below |
| Aggregation of several backends | `Route` `composition.mode: aggregate` | Planned (M1) | Steps merge under `group` <!-- alias-ok --> |
| Sequential proxy | `composition.mode: sequential` | Planned (M1) | Earlier results reach later steps through CEL `steps` |
| `group`, `target`, `allow`, `mapping`, `is_collection` | Step `group`, `target`, `select`, `rename`, `collection` | Planned (M1) | Same shape ([source](https://www.krakend.io/docs/backends/data-manipulation/)) |
| Workflow | `composition.mode: sequential` with step `when` | Planned (M1) | Linear workflows only ([parity matrix](01-krakend-ee-parity-matrix.md#request-and-response-transformation)) |
| Flexible Configuration templates | `overlays/<env>/` and `${VAR}` substitution per `Environment` | Planned (M1) | No template language; the importer reads the rendered file |
| `KRAKEND_<KEY>` environment overrides | `${VAR}` at render time, or `secretRef` with `provider: env` at runtime | Planned (M1) | Overrides only keys already in the file ([source](https://www.krakend.io/docs/configuration/environment-vars/)) |
| Restart or blue/green deploy per change ([source](https://www.krakend.io/docs/deploying/)) | Hot Reload of a new Revision; Rollouts in Control mode | Planned (M1); Rollouts Planned (M2) | No restart |
| KrakenD nodes sharing only a file ([source](https://www.krakend.io/docs/deploying/clustering/)) | Nodes reading one rendered source (file mode), or a `Cluster` of enrolled Nodes (Control mode), sharing one Revision | Planned (M1) file mode; Planned (M2) Control mode | Digest-verified |
| Go plugins (`.so`) | WASM `Plugin` attached by a `plugin` Policy, or a built-in Filter | Planned (M2) | [Plugin migration](#plugin-migration) |
| Lua scripts | CEL fields or a WASM `Plugin` | Planned (M1) CEL; Planned (M2) Plugins | No Lua runtime ([ADR-0011](../adr/0011-expressions-and-authorization-engines.md)) |
| API keys listed in the file | One `Consumer` per key with a hashed `credentials.apiKeys` entry and an `auth.api-key` Policy | Planned (M1) | Never stored plain |
| Tier header in rate limits and quotas | `Consumer.spec.tier` read by `Policy.spec.when`, or a CEL `when` over `request.headers` | Planned (M1) | [Namespace table](#extra_config-namespaces) |
| `/__health/` on the service port | `/healthz` and `/readyz` on the `ruralzd` admin port | Planned (M1) | Load balancer checks move to the admin port |
| `/__stats/` and extended metrics | `/metrics` on the admin port and OTLP export | Planned (M1) | Metric names `ruralz_<component>_<name>_<unit>` |
| `/__debug/` and `/__echo/` | `ruralz dev tap` over `/tap` on the admin port | Planned (M1) | Redacted traffic metadata |
| `/__catchall` | A lowest-ranked `Route` matching only `match.when: "true"` | Planned (M1) | [Traffic management](../architecture/09-traffic-management-and-resilience.md#krakend-ee-routing-and-traffic-features) |
| EE license file | None: no license key, file or entitlement check | Planned (M1) | KrakenD EE needs one ([parity matrix](01-krakend-ee-parity-matrix.md#non-goals-of-this-matrix)); P1, [ADR-0002](../adr/0002-apache-2-license-no-feature-gating.md) |

### Top-level keys

The v2.13 schema has 36 root properties, only `version` required ([source](https://www.krakend.io/schema/v2.13/krakend.json)); each maps as follows, with levels from [Import tool fidelity levels](#import-tool-fidelity-levels).

| KrakenD key | Ruralz target | Fidelity | Notes |
|---|---|---|---|
| `version` | Checked, not emitted | `exact` | Not `3`: exit 2, naming `krakend-config-migrator` ([source](https://www.krakend.io/docs/configuration/migrating/)) |
| `name` | `Gateway` `metadata.name`, converted to an RFC 1123 label | `approximate` | Telemetry service name per OQ-migration-from-krakend-4 |
| `port` | `Gateway` `listeners[]` entry with `protocol: http` (or `https` when `tls` is enabled) and `port` | `exact` | Default `8080` in both products |
| `listen_ip` | None | `manual` | No listener bind-address field (OQ-migration-from-krakend-2) |
| `timeout` | `Route` `timeout` on every Route that does not override it | `equivalent` | Covers the whole pipe including all backends, like the Ruralz whole-request deadline; when absent, KrakenD's `2s` default is written <!-- alias-ok --> |
| `cache_ttl` | Route-scoped `headers` Policy with `response.set[]` writing `cache-control: public, max-age=<seconds>` | `approximate` | KrakenD only sets the header ([source](https://www.krakend.io/schema/v2.13/krakend.json)); the Policy writes it on every response of the Route |
| `output_encoding` | Default for every Route without its own value | Per value, see [KrakenD path entries and their targets](#krakend-path-entries-and-their-targets) | `json` and `no-op` import; other encodings per OQ-krakend-ee-parity-matrix-2 <!-- alias-ok --> |
| `host` | `Upstream.spec.endpoints[]` for every backend without its own `host` | `equivalent` | A scheme of `https` needs an Upstream TLS block (OQ-migration-from-krakend-5) <!-- alias-ok --> |
| `endpoints` | One `Route` per entry | Per entry | [KrakenD path entries and their targets](#krakend-path-entries-and-their-targets) <!-- alias-ok --> |
| `extra_config` | Gateway-scoped Policies and `Gateway` fields | Per namespace | [extra_config namespaces](#extra_config-namespaces) |
| `async_agent` | `Route` `match.topic` with a `kafka` Upstream, Planned (M4) | `manual` | Ingress declaration per OQ-configuration-model-11; the AMQP driver is Not planned |
| `tls` | `Gateway` `listeners[]` entry with `protocol: https`, `tls.minVersion` and `tls.certificates` (`secretRef` with `provider: file` for each key pair path) | `equivalent`; `approximate` when `max_version`, `cipher_suites`, `curve_preferences` or `disable_system_ca_pool` differ from Ruralz defaults, or a `min_version` below `TLS12` is raised to `"1.2"` | `enable_mtls` with `ca_certs` needs a Gateway `auth.mtls` Policy, whose fields are pending (OQ-security-and-identity-3): `manual` with a hold |
| `client_tls` | `Upstream.spec.tls` (`caCertificate`, `clientCertificate`, `clientKey`) on every generated Upstream | `approximate` | Cipher and version settings have no Upstream field |
| `plugin` | None; each loaded plugin becomes a report item | `manual` | [Plugin migration](#plugin-migration) |
| `debug_endpoint` | None; use `ruralz dev tap` | `manual` | `/__debug/` is not recreated <!-- alias-ok --> |
| `echo_endpoint` | None; use `ruralz dev tap` | `manual` | `/__echo/` is not recreated <!-- alias-ok --> |
| `disable_rest` | `match.path` with `exact`, `prefix`, `template` or `regex` | `equivalent` | No RESTful-only restriction |
| `sequential_start` | None | `approximate` | Orders async agent registration only |
| `use_h2c` | None | `manual` | No listener field enables h2c (OQ-migration-from-krakend-2) |
| `dns_cache_ttl` | `Upstream` `discovery.type: dns` | `approximate` | No DNS refresh field |
| `max_header_bytes` | `Gateway` `limits.maxRequestHeaderBytes` | `exact` | The importer writes KrakenD's value, default `1000000`, explicitly |
| `max_shutdown_wait_time` | None in the Bundle | `manual` | Process-level Drain bound ([Zero-downtime upgrades and hot reload](../operations/02-zero-downtime-upgrades-and-hot-reload.md)) |
| `read_timeout`, `read_header_timeout`, `write_timeout`, `idle_timeout` | None | `manual` | No listener timeout fields (OQ-migration-from-krakend-2) |
| `dialer_timeout`, `dialer_keep_alive`, `dialer_fallback_delay` | None | `approximate` | Connection tuning has no Upstream fields (OQ-migration-from-krakend-3) |
| `idle_connection_timeout`, `response_header_timeout`, `expect_continue_timeout` | None; `Upstream` `timeout` and `retries.perTryTimeout` bound attempts | `approximate` | OQ-migration-from-krakend-3 |
| `max_idle_connections`, `max_idle_connections_per_host` | None; `circuitBreaker.maxConnections` caps connections | `approximate` | OQ-migration-from-krakend-3 |
| `disable_keep_alives`, `disable_compression` | None | `approximate` | OQ-migration-from-krakend-3 |

### KrakenD path entries and their targets

An endpoint entry requires `endpoint` and `backend`; a backend requires only `url_pattern` ([source](https://www.krakend.io/schema/v2.13/krakend.json)). The importer emits one `Route` per endpoint entry and one `Upstream` per distinct host list, service discovery, TLS and backend-scope namespace combination. <!-- alias-ok -->

| KrakenD field | Ruralz target | Fidelity | Notes |
|---|---|---|---|
| endpoint `endpoint` | `match.path.template` when the path holds `{placeholders}`, else `match.path.exact` | `exact` | Case-sensitive in KrakenD ([source](https://www.krakend.io/schema/v2.13/krakend.json)); matching per [Data plane](../architecture/03-data-plane.md) <!-- alias-ok --> |
| endpoint `method` | `match.methods: [<method>]` | `exact` | One method per entry, default `GET` ([source](https://www.krakend.io/docs/endpoints/)) <!-- alias-ok --> |
| endpoint `output_encoding: no-op` | `Route` with plain `upstreams`; bytes stream unchanged | `exact`; `approximate` for streaming | Proxy-only in KrakenD. A no-op endpoint whose backends are all `encoding: no-op` is KrakenD's streaming configuration ([source](https://www.krakend.io/docs/enterprise/endpoints/streaming/)); before M3 the importer adds one `approximate` item per such Route, because `text/event-stream` flushes per read, not per event, until SSE, Planned (M3) ([Data plane](../architecture/03-data-plane.md)) <!-- alias-ok --> |
| endpoint `output_encoding: json` or `fast-json` | Plain `upstreams` for one backend without manipulation; `composition.mode: aggregate` otherwise | `equivalent` | JSON key order and whitespace may differ <!-- alias-ok --> |
| endpoint `output_encoding: json-collection` | `composition.mode: aggregate` with step `collection: true` | `approximate` | Array framing follows the Ruralz merge <!-- alias-ok --> |
| endpoint `output_encoding` `xml`, `yaml`, `negotiate`, `string` | None | `manual` | No re-encoding (OQ-krakend-ee-parity-matrix-2) <!-- alias-ok --> |
| endpoint `concurrent_calls` above 1 | None; hedging on the Upstream leg, Planned (M4) | `approximate` | Ruralz sends one request per leg (partial parity) <!-- alias-ok --> |
| endpoint `timeout` | `Route` `timeout` | `equivalent` | Whole-request deadline <!-- alias-ok --> |
| endpoint `cache_ttl` | Route-scoped `headers` Policy, as the root key | `approximate` | A zero value means the root value, which the importer resolves <!-- alias-ok --> |
| endpoint `input_headers`, `input_query_strings`, present or absent | None yet; allowlist per OQ-krakend-ee-parity-matrix-5 | `manual`, security-flagged, one item per Route | KrakenD forwards only listed parameters, none by default; Ruralz forwards every client header and query string, `Authorization` and cookies included, unless a Policy removes them (OQ-migration-from-krakend-7) <!-- alias-ok --> |
| endpoint `backend` | `upstreams` or `composition.steps` | Per backend | See the backend rows <!-- alias-ok --> |
| endpoint `extra_config` | Route-scoped Policies | Per namespace | Same slots as Gateway Policies, so a Route `cors` Policy replaces the Gateway one <!-- alias-ok --> |
| backend `url_pattern` equal to the endpoint path | Plain `upstreams` | `exact` | No rewrite needed <!-- alias-ok --> |
| backend `url_pattern` that rewrites the path | A composition step with `path`, or `pathExpression` using `request.pathParams` | `equivalent` for `GET` and `HEAD`; `approximate` for methods with a body | Plain `upstreams` rewrites per OQ-traffic-management-and-resilience-13; body forwarding per OQ-migration-from-krakend-16 <!-- alias-ok --> |
| backend `host` | `Upstream.spec.endpoints[].address` (host and port; scheme dropped) | `equivalent` for one host; `approximate` for several | KrakenD's balancing algorithm is unresearched; Ruralz uses the `loadBalancing` default <!-- alias-ok --> |
| backend `method` | Step `method`, or pass-through | `exact` | Default: the incoming method <!-- alias-ok --> |
| backend `encoding` `json`, `safejson`, `fast-json` | Merge input for composition | `equivalent` for `json`; `approximate` for `safejson` and `fast-json` | The research records only that `encoding` selects the parser, so their differences are unresearched <!-- alias-ok --> |
| backend `encoding: no-op` | Plain `upstreams` | `exact` | Bytes pass unchanged <!-- alias-ok --> |
| backend `encoding` `xml`, `rss`, `string`, `yaml` | None | `manual` | CEL sees bodies only as JSON (OQ-krakend-ee-parity-matrix-1) <!-- alias-ok --> |
| backend `group` | Step `group` | `equivalent` | Same wrapping key <!-- alias-ok --> |
| backend `target` | Step `target` | `equivalent` | Unwraps before merge <!-- alias-ok --> |
| backend `allow` | Step `select` | `equivalent` for top-level fields; `approximate` for dot-notation paths | Nested paths per OQ-migration-from-krakend-6 <!-- alias-ok --> |
| backend `deny` | None | `manual` | No denylist field (OQ-migration-from-krakend-6) <!-- alias-ok --> |
| backend `mapping` | Step `rename` | `equivalent` | Top-level fields <!-- alias-ok --> |
| backend `is_collection` | Step `collection` | `equivalent` | Unclear schema default, so the value read is written ([source](https://www.krakend.io/schema/v2.13/krakend.json)) <!-- alias-ok --> |
| backend `sd: static` | `Upstream.spec.endpoints` | `exact` | The KrakenD default <!-- alias-ok --> |
| backend `sd` `dns` or `dns-shared` | `Upstream` `discovery.type: dns` | `approximate` | `dns-shared` has no separate mode <!-- alias-ok --> |
| backend `sd_scheme` | Upstream TLS block when `https` | `equivalent` | OQ-migration-from-krakend-5 <!-- alias-ok --> |
| backend `disable_host_sanitize` | None needed | `exact` | Addresses are written without a scheme <!-- alias-ok --> |
| backend `input_headers`, `input_query_strings` | As the endpoint rows | `manual`, security-flagged | Folded into the Route's forwarding item <!-- alias-ok --> |
| backend `extra_config` | Upstream-scoped Policies or `Upstream` fields | Per namespace | Upstream-scoped Policies run only in that upstream leg <!-- alias-ok --> |
| endpoint `proxy.sequential` | `composition.mode: sequential` | `equivalent` | Steps run in list order <!-- alias-ok --> |
| endpoint `proxy.sequential_propagated_params` and the placeholders it propagates | Step `pathExpression` over `steps.<name>.body` | `approximate` | Placeholder syntax and propagation semantics are unresearched (the research records only the key); the importer reports each placeholder it rewrites to a CEL expression <!-- alias-ok --> |
| endpoint `proxy.combiner` | None | `manual` | Custom merge logic becomes a `plugin` Policy <!-- alias-ok --> |
| endpoint `proxy.static` | A `plugin` Policy that short-circuits with a static response | `manual` | Built-in static responses per OQ-krakend-ee-parity-matrix-3 <!-- alias-ok --> |
| `proxy.flatmap_filter` | `transform.response` | `manual` | `config` schema per OQ-krakend-ee-parity-matrix-1 ([source](https://www.krakend.io/docs/backends/flatmap/)) |
| endpoint `proxy.max_payload` | `Gateway` `limits.maxRequestBodyBytes` | `approximate` | One Gateway-wide limit: the largest value, reporting each endpoint that set less <!-- alias-ok --> |
| endpoint `proxy.decompress_gzip` | None | `manual` | Compression type per OQ-krakend-ee-parity-matrix-3 <!-- alias-ok --> |
| backend `proxy.shadow` | `Route` mirroring, Planned (M2) | `manual` | Declaration per OQ-traffic-management-and-resilience-12 <!-- alias-ok --> |

### extra_config namespaces

The 87 rows cover every v2.13 namespace the research records, split by scope where mappings differ: S service, E endpoint, B backend, W workflow, A async agent ([source](https://www.krakend.io/schema/v2.13/krakend.json)). Editions, marked inconsistently ([source](https://www.krakend.io/features/)), do not change the mapping, since every Ruralz target is free (P1). Both legacy spellings, `github_com/...` and `github.com/...`, normalize to the v2 names, as KrakenD CE registers both ([source](https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go)). <!-- alias-ok -->

| Namespace | Scope | Ruralz target | Fidelity | Notes |
|---|---|---|---|---|
| `auth/validator` | E | `auth.jwt` Policy on the `Route` with `issuers[]` (`issuer`, `jwksUrl`, `audiences`); role and scope checks become an `authz.cel` rule over `auth.claims`, Planned (M1) | `equivalent` for signature, issuer and audience checks from a key-set URL whose algorithms Ruralz accepts (RS256, PS256, ES256, EdDSA per the [parity matrix](01-krakend-ee-parity-matrix.md#authentication-and-authorization)); `approximate` for the role and scope rule, since KrakenD role and scope fields are unresearched; `manual` with a hold for local keys, other algorithms, or an endpoint that also sets `auth/api-keys` or `auth/basic` | One `auth` slot per Route (OQ-configuration-model-13); [Security and identity](../architecture/08-security-and-identity.md#jwt-and-oidc) ([source](https://www.krakend.io/docs/authorization/jwt-validation/)) <!-- alias-ok --> |
| `auth/validator` | S | None: Nodes fetch and cache issuer keys under Security and identity rules | `approximate` | KrakenD holds the global key-set client and cache settings here |
| `auth/validator` `propagate_claims` | E | Route-scoped `headers` Policy with `request.set[].valueExpression` over `auth.claims` and `when: 'auth != null'`, run in `onRequestHeaders` after the auth Filter class | `equivalent` | Route scope keeps it off Routes that share the Upstream without JWT validation; named in KrakenD's LLM routing guide ([source](https://www.krakend.io/docs/enterprise/ai-gateway/llm-routing/)) |
| `auth/signer` | E | A built-in JWT signing type, not yet in the registry, Planned (M2) | `manual` | OQ-security-and-identity-11 ([source](https://www.krakend.io/docs/authorization/jwt-signing/)) |
| `auth/revoker` | S | Signed revocation list checked by `auth.*` Policies, Planned (M2) | `manual` | Revoked entries are not migrated ([source](https://www.krakend.io/docs/authorization/revoking-tokens/)) |
| `auth/client-credentials` | B | `auth.upstream-oauth2` on the `Upstream` (`tokenUrl`, `clientId`, `clientSecret` as `secretRef`, `scopes`), Planned (M1) | `equivalent` | The operator provisions the secret ([source](https://www.krakend.io/docs/authorization/client-credentials/)) <!-- alias-ok --> |
| `auth/api-keys` | S | One `Consumer` per `keys[]` entry with `credentials.apiKeys[].hash`; `identifier` sets `config.header` of each Route `auth.api-key` Policy (next row), Planned (M1) | `equivalent` for `hash: plain`; `manual` with a hold for `fnv128`, `sha1` or a `salt` | Plain keys are hashed at import and never written; `roles` become Consumer `tags`; `strategy: query_string` has no field (OQ-migration-from-krakend-1); hashed keys need reissue (OQ-migration-from-krakend-13) ([source](https://www.krakend.io/docs/enterprise/authentication/api-keys/)) |
| `auth/api-keys` | E | A Route `auth.api-key` Policy; `roles` become an `authz.cel` rule over `consumer.tags`; `client_max_rate` a `ratelimit` keyed on `consumer.name` | `equivalent` for roles; `approximate` for the per-key limit; `manual` with a hold when the endpoint also sets `auth/validator` or `auth/basic` | Only these Routes get a key check; a Route holds one `auth` slot Policy (RZ-CFG-018, OQ-configuration-model-13); algorithm differs ([ADR-0008](../adr/0008-rate-limiting-local-bucket-and-gcra.md)) <!-- alias-ok --> |
| `auth/basic` | S, E | `auth.basic`, Planned (M1) | `manual` with a hold | Consumer binding field pending (OQ-security-and-identity-2) ([source](https://www.krakend.io/docs/enterprise/authentication/basic-authentication/)) |
| `auth/aws-sigv4` | B | `auth.upstream-sigv4` on the `Upstream`, Planned (M2) | `manual` | No registered `config` fields yet (OQ-migration-from-krakend-1) ([source](https://www.krakend.io/docs/enterprise/authentication/aws-sigv4/)) <!-- alias-ok --> |
| `auth/gcp` | B | JWT-bearer grant in `auth.upstream-oauth2`, Planned (M2) | `manual` | Fields per OQ-security-and-identity-12 ([source](https://www.krakend.io/docs/enterprise/authentication/gcloud/)) <!-- alias-ok --> |
| `auth/ntlm` | B | None: Not planned | `manual` | [Migration gaps](#migration-gaps) ([source](https://www.krakend.io/docs/enterprise/authentication/ntlm/)) <!-- alias-ok --> |
| `security/policies` | E | `authz.cel` `config.rule` on the `Route` for `req` and `jwt` contexts, Planned (M1); `authz.opa` or `authz.cedar` when rules outgrow CEL, Planned (M2) | `approximate` for plain CEL; `manual` with a hold for `resp` rules, `geoIP()` and hash or `uuid()` macros | Variables are rewritten to `request` and `auth.claims`; the `error` body and status have no field (OQ-migration-from-krakend-15) ([source](https://www.krakend.io/docs/enterprise/security-policies/)) |
| `security/policies` | B, W | An Upstream-scoped `plugin` Policy with `filterClass: authz` in `onUpstreamRequest`, Planned (M2), or a step `when` where skipping the leg is acceptable | `manual` with a hold | `authz.cel` is Gateway and Route scope only (RZ-CFG-020), and on the Route it would reject the whole aggregated request, not one backend call <!-- alias-ok --> |
| `qos/ratelimit/router` | E | Route `ratelimit` with `limits[]` (`requests` from `max_rate`, `window` from `every`) and a constant `config.key` such as `route.name`; a header-keyed `client_max_rate` becomes a second `ratelimit` keyed on that `request.headers` entry | `approximate`; `manual` for an IP-keyed `client_max_rate`, which behind a load balancer puts every client in one bucket (OQ-security-and-identity-6) | KrakenD limits apply per instance ([source](https://www.krakend.io/docs/throttling/cluster/)); on the `redis` State Store of rule 8 Ruralz enforces one Cluster-wide limit ([OQ-migration-from-krakend-12](#open-questions)). A constant key is one State Store hot key, safe to about 25,000 requests per second (hypothesis) ([parity matrix](01-krakend-ee-parity-matrix.md#traffic-management)); `capacity` and higher limits wait for OQ-traffic-management-and-resilience-1 |
| `qos/ratelimit/router/redis` | E | Route `ratelimit` on the `redis` State Store driver | `approximate` | The importer sets `failureMode: closed` unless `on_failure_allow` is true, keeping KrakenD's default of blocking when Redis fails ([source](https://www.krakend.io/docs/enterprise/throttling/global-rate-limit/)) |
| `qos/ratelimit/service` | S | Gateway `ratelimit` with a constant `config.key` | `approximate` | Per-instance in KrakenD ([source](https://www.krakend.io/docs/enterprise/service-settings/service-rate-limit/)); a constant key is one State Store hot key ([parity matrix](01-krakend-ee-parity-matrix.md#traffic-management)) |
| `qos/ratelimit/service/redis` | S | Gateway `ratelimit` with a constant key on the `redis` driver | `approximate` | `failureMode` as for the router variant |
| `qos/ratelimit/tiered` | S, E | One `ratelimit` per tier, guarded by mutually exclusive `Policy.spec.when` expressions over `request.headers["<tier_key>"]` or `consumer.tier` | `equivalent` for `literal` tiers; `approximate` for the `*` catch-all; `manual` for `tier_value_as: policy` | KrakenD picks the first matching tier ([source](https://www.krakend.io/docs/enterprise/service-settings/tiered-rate-limit/)) |
| `qos/ratelimit/proxy` | B | Route `ratelimit` with a constant key on every Route reaching the `Upstream`; `circuitBreaker.maxPendingRequests` | `approximate` | Upstream scope per OQ-krakend-ee-parity-matrix-4 ([source](https://www.krakend.io/docs/backends/rate-limit/)) <!-- alias-ok --> |
| `qos/circuit-breaker` | B | `Upstream` `circuitBreaker`: `max_errors` to `consecutiveFailures`, `timeout` to `openDuration` | `approximate` | `interval` has no field; the volume guard differs ([Traffic management](../architecture/09-traffic-management-and-resilience.md#krakend-ee-routing-and-traffic-features)) ([source](https://www.krakend.io/docs/backends/circuit-breaker/)) |
| `qos/circuit-breaker/http` | B | `circuitBreaker.failureWhen` CEL over `response.status` | `approximate` | ([source](https://www.krakend.io/docs/enterprise/backends/http-circuit-breaker/)) |
| `qos/http-cache` | B | Route `cache` Policy (Response Cache) in the State Store | `approximate` | KrakenD caches in memory per instance; `shared`, `max_items` and `max_size` are reported unmapped, since the research records only their names ([source](https://www.krakend.io/docs/backends/caching/)) |
| `governance/processors` | S | Gateway `stateStore` plus `Consumer` `quotas` (`unit: requests`, `limit`, `window`) | `approximate` | Calendar units per OQ-traffic-management-and-resilience-10; `on_failure_allow` sets `failureMode` ([source](https://www.krakend.io/docs/enterprise/governance/quota/)) |
| `governance/quota` | S, E, B | `quota` Policy with `consumerQuota` on the `Gateway` (S) or `Route` (E); at B, on every Route reaching the `Upstream`, counting client requests, not backend calls; `weight_key` charging becomes `ai.token-budget` for LLM tokens, Planned (M3) | `approximate` when tiers map to imported Consumers; `manual` otherwise | `quota` is Gateway and Route scope only; weighted costs are partial parity (OQ-traffic-management-and-resilience-10); counters are not migrated <!-- alias-ok --> |
| `redis` | S | Gateway `stateStore` with `driver: redis` and `url` as `secretRef` | `approximate` | Named pools collapse into one State Store; pool tuning has no fields ([source](https://www.krakend.io/docs/enterprise/service-settings/redis-connection-pools/)) |
| `security/bot-detector` | S, E | `authz.cel` `config.rule` matching `request.headers["user-agent"]` | `approximate` | Long pattern lists exceeding the CEL cost bound need a `plugin` Policy ([source](https://www.krakend.io/docs/throttling/botdetector/)) |
| `security/cors` | S, E | `cors` Policy with `allowOrigins` and `allowMethods`, Gateway or Route scope | `equivalent` when only those are set; `approximate` otherwise | Other fields wait for OQ-migration-from-krakend-1 ([source](https://www.krakend.io/docs/service-settings/cors/)) |
| `security/http` | S, E | `headers` Policy with `response.set[]` for each security header, on the `Gateway` with `overridable: false` (S) or on the `Route` (E) | `equivalent` | HSTS, HPKP, clickjacking and similar headers ([source](https://www.krakend.io/docs/service-settings/security/)) |
| `router` | S | `max_payload` to `limits.maxRequestBodyBytes`; `logger_skip_paths` and `disable_access_log` to a `telemetry.accessLog.when` expression the report carries, unwritten, for use after cutover Stage 6; `health_path` to 9901 `/readyz` | `approximate`; `manual` for `trusted_proxies` and `remote_ip_headers` | Those two wait for OQ-security-and-identity-6; `return_error_msg` follows the Ruralz error format ([source](https://www.krakend.io/docs/service-settings/router-options/)) |
| `server/virtualhost` | S | `Route` `match.hosts` with listener `hostnames` | `equivalent` | ([source](https://www.krakend.io/docs/enterprise/service-settings/virtual-hosts/)) |
| `server/static-filesystem` | S | Built-in static-content type, Planned (M5) | `manual` | OQ-krakend-ee-parity-matrix-3 ([source](https://www.krakend.io/docs/enterprise/endpoints/serve-static-content/)) |
| `grpc` | S | `Route` `match.grpc` and `grpc` Upstreams, Planned (M3) | `manual` | Descriptor source per OQ-multi-protocol-1 ([source](https://www.krakend.io/docs/enterprise/grpc/server/)) |
| `websocket` | E | `websocket` Upstream, Planned (M3) | `manual` before M3; then `equivalent` for direct mode and `manual` for the multiplexer | Envelope per OQ-multi-protocol-6 ([source](https://www.krakend.io/docs/enterprise/websockets/)) |
| `backend/conditional` | B | `composition.mode: conditional`: `header` strategy to a step `when` over `request.headers`, `policy` to a CEL `when`, `fallback` to a last step with `when: "true"` | `equivalent` for `header` and `fallback`; `approximate` for `policy` | ([source](https://www.krakend.io/docs/enterprise/backends/conditional/)) <!-- alias-ok --> |
| `proxy` | E, B, W | Composition modes and steps | Per sub-key | Sub-keys in [KrakenD path entries and their targets](#krakend-path-entries-and-their-targets) <!-- alias-ok --> |
| `modifier/lua-endpoint` | S, E | CEL or a WASM Plugin | `manual` with a hold when the script can reject | [Lua to CEL or WASM Plugins](#lua-to-cel-or-wasm-plugins) ([source](https://www.krakend.io/docs/endpoints/lua/)) |
| `modifier/lua-proxy` | E, W | CEL or a WASM Plugin | `manual` with a hold when the script can reject | Same |
| `modifier/lua-backend` | B | CEL, a composition step `when`, or an Upstream-scoped WASM Plugin | `manual` with a hold when the script can reject | Same <!-- alias-ok --> |
| `modifier/martian` | B | Header modifiers to Upstream-scoped `headers` Policies; body and query modifiers to `transform.request` | `equivalent` for header set; `manual` otherwise | No DSL ([source](https://www.krakend.io/docs/backends/martian/)) |
| `modifier/jmespath` | E, B, W | `transform.response` CEL over `response.body` | `manual` | OQ-krakend-ee-parity-matrix-1 ([source](https://www.krakend.io/docs/enterprise/endpoints/jmespath/)) |
| `modifier/request-body-generator` | E, B, W | `transform.request` building the body with CEL | `manual` | Go templates are rewritten by hand ([source](https://www.krakend.io/docs/enterprise/backends/body-generator/)) |
| `modifier/body-generator` | B | As `modifier/request-body-generator` | `manual` | Alias with the same schema |
| `modifier/response-body-generator` | E, B, W | `transform.response` | `manual` | ([source](https://www.krakend.io/docs/enterprise/backends/response-body-generator/)) |
| `modifier/response-body` | E, B | `transform.response` literal or regular expression replacement | `manual` | ([source](https://www.krakend.io/docs/enterprise/endpoints/content-replacer/)) |
| `modifier/request-body-extractor` and its `/early` variant | S, E | `transform.request` copying body fields to headers or the query string | `manual` | New in 2.13 ([source](https://www.krakend.io/docs/enterprise/endpoints/request-body-extractor/)) |
| `modifier/response-headers` | S | Gateway `headers` Policy `response.set[]` | `equivalent` for set operations; `approximate` otherwise | ([source](https://www.krakend.io/docs/enterprise/service-settings/response-headers-modifier/)) |
| `validation/cel` | E | `authz.cel` `config.rule` on the `Route` | `approximate` | Variables are rewritten; the rejection status follows Ruralz authz codes ([source](https://www.krakend.io/docs/endpoints/common-expression-language-cel/)) |
| `validation/cel` | W | A step `when` over `steps`, Planned (M1), where skipping the leg is acceptable, or an Upstream-scoped `plugin` Policy with `filterClass: validation`, Planned (M2) | `manual` with a hold | KrakenD aborts one backend call; a Route `authz.cel` would reject the whole aggregated request and cannot read `steps` <!-- alias-ok --> |
| `validation/cel` | B | A validation-class `plugin` Policy at Upstream scope | `manual` with a hold | Response checks have no built-in type <!-- alias-ok --> |
| `validation/json-schema` | E, W | `validation.json-schema` Policy on the `Route`, Planned (M1) | `manual` with a hold until the schema field is registered, then `equivalent` | OQ-migration-from-krakend-1 ([source](https://www.krakend.io/docs/endpoints/json-schema/)) |
| `validation/response-json-schema` | E, B | `validation.json-schema` in response Phases, Planned (M5) | `manual` | OQ-krakend-ee-parity-matrix-3 ([source](https://www.krakend.io/docs/enterprise/endpoints/response-schema-validator/)) |
| `workflow` | B | `composition.mode: sequential` with step `when` | `approximate` for linear workflows; `manual` for nested workflows | Nesting is unlimited in KrakenD ([source](https://www.krakend.io/docs/enterprise/endpoints/workflows/)) |
| `backend/http` | B | Upstream statuses pass through; step `optional` | `approximate` | `return_error_code` and `return_error_details` differ from the `RZ-<AREA>-<NNN>` format ([source](https://www.krakend.io/docs/backends/detailed-errors/)) <!-- alias-ok --> |
| `backend/http/client` | B | `client_tls` to `Upstream.spec.tls`; `proxy_address` to an egress field, Planned (M5) | `equivalent` for `no_redirect: true`; `approximate` for `client_tls`, as the root row, for `no_redirect` false or absent, and for `send_body_on_redirect`, reported unmapped; `manual` for `proxy_address` (rule 4) | KrakenD redirect handling is unresearched (the research records sub-key names only); Ruralz passes 3xx responses to the client (OQ-traffic-management-and-resilience-13); egress field per OQ-krakend-ee-parity-matrix-6 ([source](https://www.krakend.io/docs/backends/http-client/)) <!-- alias-ok --> |
| `backend/graphql` | B | `graphql` Upstream, Planned (M3); REST-to-GraphQL request building through `transform.request` | `manual` | ([source](https://www.krakend.io/docs/backends/graphql/)) <!-- alias-ok --> |
| `backend/grpc` | B | `grpc` Upstream, Planned (M3) | `manual` | Transcoding descriptors per OQ-multi-protocol-1 ([source](https://www.krakend.io/docs/enterprise/backends/grpc/)) <!-- alias-ok --> |
| `backend/lambda` | B | None: Not planned | `manual` | OQ-krakend-ee-parity-matrix-10 ([source](https://www.krakend.io/docs/backends/lambda/)) <!-- alias-ok --> |
| `backend/amqp/consumer` | B | None: Not planned | `manual` | OQ-multi-protocol-14 ([source](https://www.krakend.io/docs/backends/amqp-consumer/)) <!-- alias-ok --> |
| `backend/amqp/producer` | B | None: Not planned | `manual` | OQ-multi-protocol-14 <!-- alias-ok --> |
| `backend/pubsub/publisher` | B | `kafka` or `nats` Upstream by the host's URL scheme, Planned (M4); other brokers Not planned | `manual` | ([source](https://www.krakend.io/docs/backends/pubsub/)) <!-- alias-ok --> |
| `backend/pubsub/subscriber` | B | `Route` `match.topic`, Planned (M4) | `manual` | OQ-configuration-model-11 <!-- alias-ok --> |
| `backend/pubsub/publisher/kafka` | B | `kafka` Upstream `messaging` options, Planned (M4) | `manual` | OQ-multi-protocol-8 ([source](https://www.krakend.io/blog/krakend-ee-2.13-release-notes/)) <!-- alias-ok --> |
| `backend/pubsub/subscriber/kafka` | B | `Route` `match.topic` through a `kafka` Upstream, Planned (M4) | `manual` | OQ-multi-protocol-8 <!-- alias-ok --> |
| `backend/soap` | B | `transform.request` XML envelopes, Planned (M5) | `manual` | XML responses per OQ-krakend-ee-parity-matrix-1 ([source](https://www.krakend.io/docs/enterprise/backends/soap/)) <!-- alias-ok --> |
| `backend/static-filesystem` | B | Built-in static-content type, Planned (M5) | `manual` | OQ-krakend-ee-parity-matrix-3 <!-- alias-ok --> |
| `async/amqp` | A | None: Not planned | `manual` | ([source](https://www.krakend.io/docs/async/amqp/)) |
| `async/kafka` | A | `Route` `match.topic` through a `kafka` Upstream, Planned (M4) | `manual` | Kafka async agents arrived in EE 2.13 ([source](https://www.krakend.io/blog/krakend-ee-2.13-release-notes/)) |
| `ai/llm` | B | `AIProvider` with `dialect` from the vendor key, an `AIModel` and an `ai` `Upstream`, Planned (M3) | `manual` before M3; then `approximate` | `credentials` become `secretRef`, `model` a candidate `model`, `max_output_tokens` `limits.maxOutputTokens`; templates and sampling variables need `transform.request` ([source](https://www.krakend.io/docs/enterprise/ai-gateway/unified-llm-interface/)) ([ADR-0014](../adr/0014-ai-api-surface.md)) |
| `ai/mcp` | S, E | MCP Server surface, Planned (M3) | `manual` | OQ-vision-and-positioning-12, OQ-ai-llm-gateway-10 ([source](https://www.krakend.io/docs/enterprise/ai-gateway/mcp-server/)) |
| `documentation/openapi` | S, E | None in the Bundle; `ruralz bundle export openapi` derives a document from Routes | `manual` | Descriptions are not carried (OQ-migration-from-krakend-14) ([source](https://www.krakend.io/docs/enterprise/developer/openapi/)) |
| `documentation/postman` | S, E | None in the Bundle; `ruralz bundle export postman` | `manual` | ([source](https://www.krakend.io/docs/enterprise/developer/postman/)) |
| `telemetry/opentelemetry` | S | `Gateway` `telemetry.otlp.endpoint` and `traceSampling` from `trace_sample_rate` | `approximate` | Exporters become one OTLP endpoint; `service_name` per OQ-migration-from-krakend-4 ([source](https://www.krakend.io/docs/telemetry/opentelemetry/)) ([ADR-0010](../adr/0010-telemetry-opentelemetry-first.md)) |
| `telemetry/opentelemetry` | E, B | Gateway-wide sampling | `approximate` | Per-Route sampling per OQ-observability-5 ([source](https://www.krakend.io/docs/telemetry/opentelemetry-by-endpoint/)) |
| `telemetry/opentelemetry-security` | S | Authenticated OTLP export, Planned (M5) | `manual` | OQ-observability-2 ([source](https://www.krakend.io/docs/enterprise/telemetry/opentelemetry-security/)) |
| `telemetry/logging` | S, B | `slog` JSON on stdout, level from `RURALZ_LOG_LEVEL`; per-Upstream records Planned (M5) | `approximate` at S; `manual` at B | ([source](https://www.krakend.io/docs/logging/)) |
| `telemetry/gelf` | S | OTLP logs through the operator's Collector | `approximate` | No native GELF writer ([source](https://www.krakend.io/docs/logging/graylog-gelf/)) |
| `telemetry/logstash` | S | Access log records or OTLP logs through the Collector | `approximate` | ([source](https://www.krakend.io/docs/logging/logstash/)) |
| `telemetry/metrics` | S | `/metrics` on the admin port | `approximate` | `/__stats/` is not recreated ([source](https://www.krakend.io/docs/telemetry/extended-metrics/)) |
| `telemetry/influx` | S | OTLP metrics through the Collector | `approximate` | ([source](https://www.krakend.io/docs/telemetry/influxdb-native/)) |
| `telemetry/opencensus` | S | OTLP export | `approximate` | Deprecated by KrakenD in favor of OpenTelemetry ([source](https://www.krakend.io/docs/telemetry/opencensus/)) |
| `telemetry/newrelic` | S | None: Not planned; OTLP through the Collector instead | `manual` | ([source](https://www.krakend.io/docs/enterprise/telemetry/newrelic/)) |
| `telemetry/moesif` | S | Monetization hooks, Planned (M5) | `manual` | OQ-krakend-ee-parity-matrix-8 ([source](https://www.krakend.io/docs/enterprise/governance/moesif/)) |
| `plugin/http-server` | S | Built-in types for KrakenD's own sub-keys ([next table](#built-in-krakend-ee-plugins)); a WASM Plugin for custom handlers | `manual` for custom handlers | ([source](https://www.krakend.io/docs/extending/http-server-plugins/)) |
| `plugin/http-client` | B | Built-in `Upstream` fields, or a separate service reached as an `http` Upstream | `manual` | Plugins make no outbound calls (OQ-wasm-plugin-system-5) ([source](https://www.krakend.io/docs/extending/http-client-plugins/)) <!-- alias-ok --> |
| `plugin/req-resp-modifier` | E, B, W | A WASM `Plugin` and `plugin` Policy, or `headers` and `transform.*` | `manual` for custom modifiers | ([source](https://www.krakend.io/docs/extending/plugin-modifiers/)) |
| `plugin/middleware` | E, B | A WASM `Plugin` in the matching Phases | `manual` | EE since 2.10 ([source](https://www.krakend.io/docs/enterprise/extending/middleware-plugins/)) |

### Built-in KrakenD EE plugins

KrakenD EE ships built-in plugins: `geoip`, `ip-filter`, `jwk-aggregator`, `redis-ratelimit`, `static-filesystem`, `url-rewrite`, `virtualhost` and `wildcard` under `plugin/http-server`, and `content-replacer`, `ip-filter` and `response-schema-validator` under `plugin/req-resp-modifier` ([source](https://www.krakend.io/docs/extending/http-server-plugins/)) ([source](https://www.krakend.io/docs/extending/plugin-modifiers/)). Being configuration, not code, they map to built-in types; the research records only their names, so mappings that depend on sub-key semantics are `approximate`.

| KrakenD built-in plugin | Ruralz target | Fidelity | Notes |
|---|---|---|---|
| `geoip` | `authz.geoip`, Planned (M2) | `manual`, security-flagged, with a hold | The country comes from `source.ip`, as for `ip-filter`; header enrichment for Upstreams is an Open question of Security and identity |
| `ip-filter` | `authz.ip` by CIDR on `source.ip`, Planned (M1) | `manual`, security-flagged, with a hold | Behind a load balancer `source.ip` is the balancer's address until OQ-security-and-identity-6 closes, so a denylist admits and an allowlist denies every client ([parity matrix](01-krakend-ee-parity-matrix.md#traffic-management)); the hold goes only once Nodes see client addresses directly or that question closes |
| `jwk-aggregator` | Several `issuers[]` entries in one `auth.jwt` Policy | `approximate` | Sub-key semantics unresearched |
| `redis-ratelimit` | `ratelimit` on the `redis` driver | `approximate` | Deprecated since EE 2.8 ([source](https://www.krakend.io/docs/enterprise/throttling/global-rate-limit/)) |
| `static-filesystem` | Built-in static-content type, Planned (M5) | `manual` | OQ-krakend-ee-parity-matrix-3 |
| `url-rewrite` | Composition step `path` or `pathExpression` | `approximate` | Sub-key semantics unresearched; OQ-traffic-management-and-resilience-13 |
| `virtualhost` | `match.hosts` | `equivalent` | Host header matching |
| `wildcard` | `match.path.prefix` | `approximate` | Sub-key semantics unresearched; wildcard hosts per OQ-data-plane-2 |
| `content-replacer` | `transform.response` | `manual` | OQ-krakend-ee-parity-matrix-1 |
| `response-schema-validator` | `validation.json-schema` in response Phases, Planned (M5) | `manual` | OQ-krakend-ee-parity-matrix-3 |

### Worked import example

A small rendered KrakenD file: one aggregating endpoint with field selection, a per-endpoint rate limit, a root cache TTL, service-level CORS and no parameter allowlists. JSON code blocks quote KrakenD verbatim. <!-- alias-ok -->

```json
{
  "version": 3,
  "port": 8080,
  "timeout": "3s",
  "cache_ttl": "300s",
  "extra_config": {
    "security/cors": {
      "allow_origins": ["https://shop.example"],
      "allow_methods": ["GET", "POST"]
    }
  },
  "endpoints": [
    {
      "endpoint": "/v1/orders/{id}/summary",
      "method": "GET",
      "extra_config": {
        "qos/ratelimit/router": {"max_rate": 50, "every": "1s"}
      },
      "backend": [
        {
          "host": ["http://orders.shop.svc:8080"],
          "url_pattern": "/orders/{id}",
          "allow": ["id", "status", "total"],
          "mapping": {"total": "amount"},
          "group": "order"
        },
        {
          "host": ["http://inventory.shop.svc:8080"],
          "url_pattern": "/stock/{id}",
          "target": "items",
          "is_collection": true,
          "group": "stock"
        }
      ]
    }
  ]
}
```

`ruralz bundle import krakend krakend.json --output-dir ./bundle` would emit these resources (file layout shortened):

```yaml
apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: gateway
  annotations:
    ruralz.io/import-fidelity: exact
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
  limits:
    maxRequestHeaderBytes: 1000000     # KrakenD default, written explicitly
  stateStore:                          # rule 8: the Bundle holds a ratelimit Policy
    driver: redis
    url: {secretRef: {provider: env, name: RURALZ_STATE_STORE_URL}}
  policies:
    - name: cors-gateway
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: orders-shop-svc-8080
  annotations:
    ruralz.io/import-fidelity: equivalent
spec:
  protocol: http
  endpoints:
    - address: "orders.shop.svc:8080"
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: inventory-shop-svc-8080
  annotations:
    ruralz.io/import-fidelity: equivalent
spec:
  protocol: http
  endpoints:
    - address: "inventory.shop.svc:8080"
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: get-v1-orders-id-summary
  annotations:
    ruralz.io/import-fidelity: manual   # lowest level among its items: client parameter forwarding
spec:
  match:
    path: {template: "/v1/orders/{id}/summary"}
    methods: [GET]
  policies:
    - name: ratelimit-get-v1-orders-id-summary
    - name: cache-ttl-get-v1-orders-id-summary
  composition:
    mode: aggregate
    steps:
      - name: order
        upstream: orders-shop-svc-8080
        pathExpression: '"/orders/" + request.pathParams.id'
        select: [id, status, total]
        rename: {total: amount}
        group: order
      - name: stock
        upstream: inventory-shop-svc-8080
        pathExpression: '"/stock/" + request.pathParams.id'
        target: items
        collection: true
        group: stock
  timeout: 3s
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: cors-gateway
  annotations:
    ruralz.io/import-fidelity: equivalent
spec:
  type: cors
  config: {allowOrigins: ["https://shop.example"], allowMethods: [GET, POST]}
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: ratelimit-get-v1-orders-id-summary
  annotations:
    ruralz.io/import-fidelity: approximate
spec:
  type: ratelimit
  config:
    key: "route.name"                  # one limit for the whole Route, as KrakenD's router limit
    limits: [{requests: 50, window: 1s}]
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: cache-ttl-get-v1-orders-id-summary
  annotations:
    ruralz.io/import-fidelity: approximate
spec:
  type: headers
  config:
    response:
      set: [{name: cache-control, value: "public, max-age=300"}]
```

The human summary of the fidelity report for the same run:

```text
ruralz bundle import krakend: krakend.json -> ./bundle (7 resources)
items: exact 4, equivalent 13, approximate 3, manual 1
manual       /endpoints/0  [security]
             Route/get-v1-orders-id-summary: KrakenD forwarded no client headers or query strings;
             Ruralz forwards all of them, Authorization and cookies included, until a Policy removes them
approximate  /cache_ttl
             Policy/cache-ttl-get-v1-orders-id-summary: header written on every response of the Route
approximate  /endpoints/0/extra_config/qos~1ratelimit~1router
             Policy/ratelimit-get-v1-orders-id-summary: KrakenD limits each instance to 50 per 1s;
             Ruralz enforces 50 per 1s across the Cluster on the redis State Store (rule 8);
             multiply by the KrakenD instance count to keep capacity
approximate  /endpoints/0/backend
             Route/get-v1-orders-id-summary: a failed step fails the request; set optional: true to allow partial responses
provision    secretRef env RURALZ_STATE_STORE_URL for Gateway stateStore.url
exit status 1 (1 manual item)
```

## Import tool fidelity levels

`ruralz bundle import krakend FILE --output-dir DIR`, Planned (M2), reads one rendered KrakenD configuration and writes a Bundle plus a fidelity report ([CLI and API surface](../reference/01-cli-and-api-surface.md#command-table)). Every mapped setting is one report item with exactly one of four levels, and every generated resource carries the annotation `ruralz.io/import-fidelity` with the lowest level among its items ([Configuration model](../architecture/02-configuration-model.md#coming-from-krakend)). Levels rank `exact`, `equivalent`, `approximate`, `manual`.

### Levels and guarantees

| Level | Guarantee | What the importer does | Operator action before cutover | Examples |
|---|---|---|---|---|
| `exact` | Every request gets the same routing, decision and bytes on the wire as under the KrakenD setting, apart from headers each product adds and client parameter forwarding, which each Route's forwarding item covers | Translates field for field | None | `port`, endpoint `method`, `max_header_bytes`, `output_encoding: no-op` <!-- alias-ok --> |
| `equivalent` | Same routing, allow or deny decisions, status and payload semantics; only JSON key order and whitespace, header casing and order, and gateway error bodies (`RZ-<AREA>-<NNN>`) differ; forwarding as for `exact` | Translates to a different mechanism with the same observable contract | Check clients that parse KrakenD error bodies | Aggregation with `group`, `select`, `rename`; `auth/client-credentials`; `server/virtualhost`; `security/http` |
| `approximate` | The Bundle validates and serves traffic; the report states each behavioral difference, such as Cluster-wide instead of per-instance limits, another algorithm, dropped tuning fields or one Gateway-wide value | Emits the nearest configuration and records the difference and whether it is more or less permissive | Accept each difference or adjust values | `qos/ratelimit/router`, `qos/circuit-breaker`, `cache_ttl`, `telemetry/opentelemetry` <!-- alias-ok --> |
| `manual` | No automatic translation at this release; the behavior is absent unless the operator adds it | Emits no resource for the setting, or a fail-closed hold for security controls, and records the source, reason and recommended target | Implement the recommended target, then remove any hold | Lua scripts, Go plugins, `auth/basic` until its binding field exists, `async/amqp` |

For SM-8, a configuration imports at full fidelity when every item is `exact` or `equivalent`, and at partial fidelity when no item is `manual` and at least one is `approximate` (proposed). Until OQ-krakend-ee-parity-matrix-5 closes, every import holds forwarding items and exits 1; whether they count is OQ-migration-from-krakend-10. The M2 exit criterion requires 90% or more of the SM-8 corpus at full or partial fidelity (target) ([Vision and positioning](../vision/01-vision-and-positioning.md#success-metrics)).

### Rules that hold at every level

1. **The output validates.** The importer runs the same offline validation as `ruralz bundle validate`; an `RZ-CFG` error in generated output is an importer defect, never a report item.
2. **Security controls are never dropped silently.** When a setting that can reject requests imports as `manual` (any `auth/*` client namespace, `security/policies`, `validation/*`, `security/bot-detector`, an `ip-filter` or `geoip` plugin, or rejecting Lua or Go plugin code), the importer attaches a hold: an `authz.cel` Policy with `rule: "false"` on each affected Route, or on the `Gateway` with `overridable: false` for service-scope settings, denying every request with an `RZ-AUTH` code until the operator replaces the control and deletes the hold. That includes an endpoint with two client authentication namespaces, whose Route holds one `auth` slot Policy (OQ-configuration-model-13). Imports thus fail closed, like every `auth.*` and `authz.*` type (P9). The per-Route forwarding item rejects nothing, so it is security-flagged without a hold. <!-- alias-ok -->
3. **Secrets never appear in the Bundle.** Plain API keys are hashed to `sha256:<hex>`, and keys under 32 characters (target) are flagged for reissue; client secrets, AI credentials and Redis URLs become `secretRef` entries (`provider: env`, or `provider: file` for TLS key paths the KrakenD file already names); the report lists each value to provision. A literal in a `SecretValue` field would be RZ-CFG-012.
4. **Items whose target is not in the running release are `manual`.** Before M3, for example, `ai/llm` and `grpc` import `manual`.
5. **Output is deterministic.** The same input produces byte-identical files and report, so a re-import after a KrakenD change diffs cleanly with `ruralz bundle diff`.
6. **Names are stable.** Route names derive from the method and path, Upstream names from the first host, Policy names from the namespace and the owning resource, all converted to RFC 1123 labels of at most 63 characters, with a suffix from a hash of the source entry on collision, so inserting an entry renames nothing.
7. **Nothing is guessed.** Settings the research documents as ambiguous, such as the `is_collection` default ([source](https://www.krakend.io/schema/v2.13/krakend.json)), are written with the value read from the file, and a missing value is reported rather than assumed.
8. **Shared limits get a shared State Store.** When the Bundle holds a `ratelimit`, `quota` or `cache` Policy, the importer writes Gateway `stateStore` with `driver: redis` and `url` as `secretRef` (`provider: env`, `name: RURALZ_STATE_STORE_URL`) and lists the URL to provision. Limits therefore never fall back to the `memory` driver, which multiplies them by the Node count; an unresolved URL keeps Nodes not ready (RZ-CFG-026).

A hold, as the importer writes it:

```yaml
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: import-hold-post-v1-payments
  annotations:
    ruralz.io/import-fidelity: manual
spec:
  type: authz.cel
  config:
    rule: "false"                      # deny every request until the unmapped control is replaced
```

### Report, annotation and exit status

| Output | Content | Where |
|---|---|---|
| Bundle | `ruralz.yaml` with the `Gateway`, and one file per kind under `routes/`, `upstreams/`, `policies/`, `consumers/` and `ai/` | `DIR` |
| Report, human form | Counts per level; each `approximate` and `manual` item with its RFC 6901 pointer into the source, target resource, difference or reason, and a documentation link; values to provision | Standard output |
| Report, JSON form | The same items plus the `exact` and `equivalent` ones, a `security` flag per item and the list of `secretRef` values to provision | A hidden file under `DIR`, skipped by the Bundle loader; format per OQ-migration-from-krakend-8 |
| Exit status | 0 when no item is `manual`; 1 when any item is `manual`; 2 when the input is unreadable, is not JSON or has a `version` other than `3` | [Exit codes](../reference/01-cli-and-api-surface.md#exit-codes) |

### Input the importer reads

- **Rendered JSON only.** KrakenD recommends `.json`, and its linter reads only JSON ([source](https://www.krakend.io/docs/configuration/supported-formats/)); other formats are converted first (OQ-migration-from-krakend-9).
- **Flexible Configuration rendered first.** CE templating writes the rendered file when `FC_OUT` is set ([source](https://github.com/krakend/krakend-flexibleconfig/blob/master/template.go)); EE Extended Flexible Configuration has an `out` setting in `flexible_config.json` ([source](https://www.krakend.io/docs/enterprise/configuration/flexible-config/)). A failed template makes KrakenD CE parse the raw file instead ([source](https://github.com/krakend/krakend-flexibleconfig/blob/master/template.go)), so the operator MUST confirm the rendered file holds no template syntax.
- **Environment overrides applied.** `KRAKEND_<UPPERCASE_KEY>` overrides root values at runtime ([source](https://www.krakend.io/docs/configuration/environment-vars/)), invisibly to the importer, so the operator writes production values into the file.
- **Comment keys ignored.** Keys starting with `@`, `$`, `_` or `#` are schema comments ([source](https://www.krakend.io/schema/v2.13/krakend.json)), skipped silently.

*Figure 2: the import pipeline from a rendered KrakenD file to a Bundle and a fidelity report.*

```mermaid
flowchart TD
    A["Read rendered KrakenD JSON"] --> B{"version equals 3?"}
    B -- "no" --> X["Exit 2: convert with krakend-config-migrator"]
    B -- "yes" --> C["Normalize legacy namespace spellings"]
    C --> D["Map service keys to Gateway"]
    D --> E["Map each path entry to a Route, hosts to Upstreams"]
    E --> F["Map extra_config namespaces to Policies and fields"]
    F --> G["Hash API keys, turn credentials into secretRef"]
    G --> H{"Security control imported as manual?"}
    H -- "yes" --> I["Attach fail-closed authz.cel hold"]
    H -- "no" --> J["Assign one fidelity level per item"]
    I --> J
    J --> K["Validate offline as ruralz bundle validate"]
    K --> L["Write Bundle with import-fidelity annotations"]
    K --> M["Write fidelity report"]
    M --> N{"Any manual item?"}
    N -- "yes" --> O["Exit 1"]
    N -- "no" --> P["Exit 0"]
```

## Plugin migration

KrakenD offers two custom-code paths: Go plugins, `.so` files built with `-buildmode=plugin` against KrakenD's Go version ([source](https://www.krakend.io/docs/extending/http-server-plugins/)), and Lua scripts run by `gopher-lua`, "a Lua5.1(+ goto statement in Lua5.2) VM" ([source](https://github.com/yuin/gopher-lua)). KrakenD CE 3.0 drops Go plugins, announced on 2026-06-04 ([source](https://www.krakend.io/blog/dropping-plugins-support-on-community/)). Ruralz loads neither (vision non-goal 3, [ADR-0011](../adr/0011-expressions-and-authorization-engines.md)), so:

1. **Go plugins** become a built-in Filter configured by a `Policy` where one exists, else a sandboxed WASM `Plugin` on the Go PDK compiled by TinyGo, or another PDK language ([ADR-0005](../adr/0005-plugin-abi-v1.md)).
2. **Lua scripts** become CEL in a registered field where the script only decides or computes a value, else a WASM `Plugin`.

The importer never translates code: every Go plugin and Lua script is a `manual` item, with a hold when it can reject requests.

*Figure 3: choosing the Ruralz target for a piece of KrakenD custom code.*

```mermaid
flowchart TD
    S["KrakenD Go plugin or Lua script"] --> A{"Does a built-in Policy type do this?"}
    A -- "yes" --> F["Built-in Filter: configure a Policy"]
    A -- "no" --> B{"Only decides or computes a value from request data?"}
    B -- "yes" --> C["CEL: authz.cel rule, Policy when, headers valueExpression, step when"]
    B -- "no" --> D{"Needs outbound calls, sockets or files?"}
    D -- "yes" --> E["Separate service reached as an http Upstream, or a composition step"]
    D -- "no" --> W["WASM Plugin on Plugin ABI v1: Go PDK with TinyGo, or Rust"]
    W --> T["ruralz plugin test, then ruralz plugin push with a signature"]
```

### Go plugins to built-in Filters or WASM Plugins

| KrakenD plugin type | Registerer and namespace | Ruralz target | Planned |
|---|---|---|---|
| HTTP server (router layer) | `HandlerRegisterer`, `plugin/http-server` ([source](https://github.com/luraproject/lura/blob/master/transport/http/server/plugin/plugin.go)) | Built-in `auth.*`, `authz.ip`, `authz.geoip`, `headers` or `cors`; else a Route-scoped `plugin` Policy that short-circuits with `response_send` | Planned (M1) built-ins, `authz.geoip` Planned (M2); Planned (M2) Plugins |
| HTTP client (replaces the backend client) | `ClientRegisterer`, `plugin/http-client` ([source](https://github.com/luraproject/lura/blob/master/transport/http/client/plugin/plugin.go)) | Built-in `Upstream` protocols and upstream-auth types; else a separate service behind an `http` Upstream, since Plugins make no outbound calls | Planned (M1) to Planned (M4) <!-- alias-ok --> |
| Request and response modifier | `ModifierRegisterer`, `plugin/req-resp-modifier` ([source](https://www.krakend.io/docs/extending/plugin-modifiers/)) | `headers` or `transform.*`; else a `plugin` Policy in body or header Phases, Upstream-scoped for backend-level modifiers | Planned (M1) built-ins; Planned (M2) Plugins <!-- alias-ok --> |
| Middleware (EE) | `MiddlewareRegisterer`, `plugin/middleware` ([source](https://www.krakend.io/docs/enterprise/extending/middleware-plugins/)) | A `plugin` Policy at the matching scope and Phases | Planned (M2) <!-- alias-ok --> |

Porting steps for a Go plugin that becomes a WASM Plugin:

1. **Scaffold.** `ruralz plugin init tenant-guard --language go` creates a Go PDK project, Planned (M2) ([SDK matrix](../architecture/05-wasm-plugin-system.md#sdk-matrix)).
2. **Move configuration.** The KrakenD factory receives `map[string]interface{}` ([source](https://github.com/luraproject/lura/blob/master/proxy/plugin/modifier.go)); a Ruralz Plugin declares `Plugin.spec.configSchema` and receives the checked `Policy` `config` once in `rz_configure`.
3. **Replace wrapper accessors with Host Functions**, each needing a declared Capability (table below).
4. **Remove ambient authority.** The sandbox has no filesystem, sockets, environment or threads; a call defaults to a 5 ms deadline (target) and 16 MiB of memory (target), 32 MiB suggested for the Go PDK (hypothesis) ([Default limits](../architecture/05-wasm-plugin-system.md#default-limits)). Code that opens connections, reads files or keeps goroutines running needs a redesign.
5. **Replace injected Redis.** KrakenD EE 2.13 injects Redis and quota processors into every plugin type ([source](https://www.krakend.io/blog/krakend-ee-2.13-release-notes/)); a Ruralz Plugin gets `state_get` and `state_incr` (`state.read`, `state.write`), one blocking call per request, and quotas use the `quota` type.
6. **Test.** `ruralz plugin build` compiles to WASM; `ruralz plugin test` runs it on the wazero host of `ruralzd`.
7. **Publish and pin.** `ruralz plugin push` publishes and signs with Sigstore ([ADR-0017](../adr/0017-artifact-signing.md)); `ruralz plugin inspect` shows the digest for `Plugin.spec.image`.
8. **Attach.** A `plugin` Policy names the Plugin, sets `filterClass` (`auth` and `authz` are closed only) and carries `config`.

| KrakenD accessor | Ruralz Host Function | Capability | Valid Phases or note |
|---|---|---|---|
| `RequestWrapper` `Method`, `Path`, `Query` | `request_info` fields 0, 3 and 4 | `request.metadata.read` | All Phases |
| `RequestWrapper` `URL` | `request_info` fields 1 to 4 | `request.metadata.read` | Separate scheme, host, path and query |
| `RequestWrapper` `Headers` (read) | `request_header_get`, `request_headers_list` | `request.headers.read` | `authorization` also needs `credentials.read` |
| `RequestWrapper` `Headers` (write) | `request_header_set`, `request_header_remove` | `request.headers.write` | Request header Phases, the body Phase and `onUpstreamRequest` |
| `RequestWrapper` `Body` | `request_body_read`, `request_body_replace` | `request.body.read`, `request.body.write` | `onRequestBody`, buffered within Gateway limits |
| `RequestWrapper` `Params` | None | None | Not exposed (OQ-migration-from-krakend-11) |
| `ResponseWrapper` `Data`, `Io` | `response_body_read`, `response_body_replace` | `response.body.read`, `response.body.write` | `onUpstreamResponseBody` |
| `ResponseWrapper` `Headers` | `response_header_get`, `response_header_set` | `response.headers.read`, `response.headers.write` | `onUpstreamResponseHeaders` onward |
| `ResponseWrapper` `StatusCode` | None | None | No status read (OQ-migration-from-krakend-11) |
| `ResponseWrapper` `IsComplete` | None | None | Partial responses come from step `optional` |
| Returning an error from a modifier | `response_send` then RESPOND in a request Phase; a negative result elsewhere | `response.send` | A negative result applies `failureMode` |
| `LoggerRegisterer` | `log` | `log.write` | Rate-limited guest logs |

Accessors: ([source](https://www.krakend.io/docs/extending/plugin-modifiers/)); Host Functions: [Host Function table](../architecture/05-wasm-plugin-system.md#host-function-table).

A modifier that admits listed tenants and copies the tenant into a header needs only two built-in Filters with CEL:

```yaml
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: tenant-allowlist
spec:
  type: authz.cel
  config:
    rule: '"x-tenant" in request.headers && request.headers["x-tenant"] in ["acme", "globex"]'
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: tenant-header
spec:
  type: headers
  when: '"x-tenant" in request.headers'
  config:
    request:
      set:
        - name: x-tenant-id
          valueExpression: 'request.headers["x-tenant"]'
```

When the logic outgrows CEL, for example a tenant list beyond the CEL cost bound, it becomes a WASM Plugin; `clock.read` and `random.read` cover the Go PDK scaffold's WASI imports (RZ-CFG-028):

```yaml
apiVersion: ruralz/v1alpha1
kind: Plugin
metadata:
  name: tenant-guard
spec:
  image: registry.example.com/platform/tenant-guard:1.0.0@sha256:dc2d15f5ae1773101c928bb8673b3ea75ee886305f757a5f9451873e2e3a5611
  abi: ruralz.plugin.v1
  phases: [onRequestHeaders]
  capabilities: [request.headers.read, request.headers.write, response.send, log.write, clock.read, random.read]
  limits: {memoryBytes: 32Mi, timeout: 5ms}
  configSchema:
    type: object
    required: [allowedTenants]
    properties:
      allowedTenants: {type: array, items: {type: string}}
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: tenant-guard-default
spec:
  type: plugin
  plugin: tenant-guard
  filterClass: authz                   # closed only: a trap or timeout rejects the request
  config:
    allowedTenants: [acme, globex]
```

### Lua to CEL or WASM Plugins

KrakenD runs Lua at three layers, `modifier/lua-endpoint` (router, `pre` only), `modifier/lua-proxy` (`pre` and `post`) and `modifier/lua-backend` (`pre`, `post` and `skip_next`), with helpers such as `custom_error` and `http_response.new`, plus EE-only encoding, hashing and time helpers ([source](https://www.krakend.io/docs/endpoints/lua/)). <!-- alias-ok -->

| KrakenD Lua layer | Ruralz Phase and scope | Notes |
|---|---|---|
| `modifier/lua-endpoint` `pre` | Route scope, request header Phase or `onRequestBody` | Runs before the Route's Upstream legs |
| `modifier/lua-proxy` `pre` | Route scope, `onRequestBody` | After the header match, before the fan-out |
| `modifier/lua-proxy` `post` | Route scope, `onResponse` | After the aggregate merge |
| `modifier/lua-backend` `pre` | Upstream scope, `onUpstreamRequest` | Per upstream leg <!-- alias-ok --> |
| `modifier/lua-backend` `post` | Upstream scope, `onUpstreamResponseHeaders` or `onUpstreamResponseBody` | Per upstream leg <!-- alias-ok --> |

| What the Lua does | CEL target | WASM Plugin target | Notes |
|---|---|---|---|
| Reject a request on a header, path or query condition, often with `custom_error` | `authz.cel` `config.rule`; or `Policy.spec.when` to skip a Policy | `response_send` with the custom status and body | A custom status or body needs a Plugin (OQ-migration-from-krakend-15) |
| Reject on a body field | `authz.cel` in `onRequestBody` over `request.body` | `request_body_read` | Buffered within `limits.maxRequestBodyBytes` |
| Set or copy a header | `headers` `request.set[]` or `response.set[]` with `valueExpression` | `request_header_set`, `response_header_set` | None |
| Rewrite a JSON body | `transform.request` or `transform.response`, `config` per OQ-krakend-ee-parity-matrix-1 | `request_body_replace`, `response_body_replace` | A Plugin until the transform schema exists |
| Choose whether to call a backend (`skip_next`) | Composition step `when` | Not needed | `conditional` or `sequential` mode <!-- alias-ok --> |
| Call another URL with `http_response.new` | A composition step to an `Upstream` for that service | Not available: no outbound calls | OQ-wasm-plugin-system-5 |
| Hash, HMAC or encode | CEL strings and encoders extensions for base64 | `crypto_digest`, `crypto_hmac` (`crypto.use`) | Host cryptography, FIPS-ready |
| Read the time | CEL `now` | `clock_now` (`clock.read`) | `now` is the request start time |
| Keep state across requests | `ratelimit`, `quota`, `cache` | `state_get`, `state_incr` | One blocking State Store call per request |
| Reload scripts with `live` | Hot Reload of a new Revision | A new `Plugin.spec.image` digest | None |

## Cutover runbook

The runbook moves client traffic from KrakenD to Ruralz Gateway in stages, keeping KrakenD warm and unchanged until the retention period ends, so rollback is a traffic shift and never a redeploy. It works in file mode, Planned (M1); Control mode, Planned (M2), adds canary Rollouts for later changes ([System overview](../architecture/01-system-overview.md#deployment-modes)). Every figure below is a proposed default.

*Figure 4: cutover stages; every stage after Prepared can return to KrakenD serving all traffic.*

```mermaid
stateDiagram-v2
    state "Prepared, import reviewed and tests green" as Prepared
    state "Dark, Nodes deployed without client traffic" as Dark
    state "Shadow, safe requests mirrored to shadow Nodes" as Shadow
    state "Split, weighted client traffic" as Split
    state "Full, all client traffic on Ruralz Gateway" as Full
    state "Rolled back, KrakenD serves all traffic" as RolledBack
    state "Decommissioned" as Decom
    [*] --> Prepared
    Prepared --> Dark
    Dark --> Shadow: readiness and smoke tests pass
    Shadow --> Split: shadow exit met, serving Nodes on the production State Store
    Split --> Split: weight raised after each bake
    Split --> Full: full weight baked
    Full --> Decom: retention period ends
    Dark --> RolledBack: smoke test fails
    Shadow --> RolledBack: divergence found
    Split --> RolledBack: rollback trigger
    Full --> RolledBack: rollback trigger
    RolledBack --> Prepared: fix and re-import
    Decom --> [*]
```

### Stage 0: prepare

1. Render the production KrakenD configuration to one JSON file with Flexible Configuration output enabled, apply every `KRAKEND_<KEY>` override in use, and confirm the file has no template syntax ([Input the importer reads](#input-the-importer-reads)).
2. Inventory custom code: each `.so` named by the root `plugin` object and each Lua source file ([source](https://www.krakend.io/docs/extending/injecting-plugins/)). Assign an owner per item and a target from [Plugin migration](#plugin-migration).
3. Freeze KrakenD configuration from Stage 3 until decommission; an emergency change is applied to KrakenD, re-imported and reviewed with `ruralz bundle diff` against the previous import.
4. Provision the production `redis` State Store that rule 8 writes whenever the Bundle uses `ratelimit`, `quota` or `cache`, and a separate shadow State Store for Stage 4.
5. Size Nodes and the State Store per [Capacity planning](../operations/03-capacity-planning.md), and keep KrakenD sized for 100% of traffic (target) until Stage 6.

### Stage 1: import and review

1. Run `ruralz bundle import krakend krakend.json --output-dir ./bundle` and keep the report with the change record.
2. Resolve every `manual` item. Security-flagged items, including each Route's client parameter forwarding item, MUST be resolved or explicitly accepted by the service owner before Stage 3, and every hold Policy MUST be replaced, not deleted alone. Until OQ-security-and-identity-6 closes, no Policy may filter on or key by `source.ip` while Nodes sit behind a load balancer.
3. Confirm the Gateway `stateStore` of rule 8, then review every `approximate` item and record the decision: accept, adjust values (for example multiply a per-instance rate limit by the KrakenD instance count), or implement a closer mechanism.
4. Keep `telemetry.accessLog.when` unset, or logging every request on migrated Routes, so shadow and serving Nodes share one digest; defer the imported `disable_access_log` or `logger_skip_paths` expression, and any errors-only filter, to Stage 6. Pairing, baselines and most rollback triggers need one record per request.
5. Provision each `secretRef` value the report lists.
6. Run `ruralz bundle validate`, `ruralz bundle audit` and `ruralz bundle render --effective --route NAME` for the busiest Routes, and commit the Bundle to Git (P5).

### Stage 2: test before traffic

1. Write request and expected-response cases for every Route from KrakenD access logs and API contracts, in `./tests` outside the Bundle, in the `ruralz.test.v1` format of [CLI and API surface](../reference/01-cli-and-api-surface.md#test-case-format).
2. Run `ruralz test run ./tests --target URL` against the production KrakenD to prove the cases describe today's behavior; fix cases, not KrakenD.
3. Run `ruralz test run ./tests --bundle ./bundle` against a local `ruralzd`, with `RURALZ_STATE_STORE_URL` naming a test State Store. Every failure is either an importer item already in the report or a new difference to add to the review.
4. Test ported Plugins with `ruralz plugin test`, then replay their Routes through the cases.

### Stage 3: deploy dark

1. Deploy the serving Ruralz Gateway Nodes next to KrakenD behind the same load balancer or ingress, with a weight of zero and `RURALZ_STATE_STORE_URL` naming the production State Store.
2. Point load balancer health checks at `/readyz` on admin port 9901 instead of KrakenD's `/__health/`; the admin bind and authentication default is OQ-system-overview-6.
3. Confirm every Node reports ready, the active Revision digest matches the one `ruralz bundle build` printed, and `ruralz dev tap` shows smoke-test requests on the expected Routes.
4. Size log shipping for full access logging through Stage 5, possibly tens of MB per second per Node (hypothesis) ([Observability](../architecture/10-observability.md#access-logs)), and confirm each Node writes one record per smoke-test request.

### Stage 4: shadow traffic

The load balancer or ingress in front of both gateways mirrors production requests to a separate shadow Node set and discards its responses; clients keep KrakenD's responses, and KrakenD stays unchanged. A KrakenD shadow backend (`proxy` `shadow` ([source](https://www.krakend.io/schema/v2.13/krakend.json))) is not a mirroring point: it sends the request KrakenD builds for a backend, with that backend's `url_pattern` path ([source](https://www.krakend.io/docs/backends/)) and only allowlisted headers, none by default ([source](https://www.krakend.io/schema/v2.13/krakend.json)), so its copies miss imported Routes and lack credentials. <!-- alias-ok -->

Shadowing rules:

1. Mirror only safe methods (`GET`, `HEAD`, `OPTIONS`). A mirrored `POST`, `PUT`, `PATCH` or `DELETE` would repeat writes on production Upstreams; mirror unsafe methods only to Upstreams that point at non-production copies. A Route with no mirrored traffic, because it serves only unsafe methods or the load balancer cannot mirror, is gated by its Stage 2 cases and enters Stage 5 at the 1% step (target) under closer watch.
2. Shadow Nodes use the shadow State Store through their own `RURALZ_STATE_STORE_URL` and never join the serving pool, so mirrored requests never consume the production Quotas, Rate Limits or Token Budgets that serving Nodes use.
3. Confirm Upstream capacity for the mirrored share and extra `auth.upstream-oauth2` token fetches.
4. Pair requests: the load balancer stamps one W3C `traceparent` on each request before mirroring and logs it with KrakenD's path and status. Shadow Nodes, logging every request (Stage 1 step 4), log the same trace ID as `trace_id` beside `route`, `status` and `code`. An offline join on the trace ID gives per-request status-class agreement; where the load balancer cannot stamp and log one, compare per-Route status-class shares instead.
5. Exclude `ratelimit`, `quota` and `ai.token-budget` decisions from agreement; the shadow State Store, safe-method subset and per-instance KrakenD limits make them differ by design.
6. Record each Route's Gateway-added p99, from the access log `gateway_duration` field ([Observability](../architecture/10-observability.md#access-logs)), as its Stage 5 baseline; it counts Filters and Plugins, not Upstream or State Store time.
7. Discard incomplete windows: a window in which `ruralz_telemetry_logs_dropped_total{stream="access",reason="queue_full"}` rises on any Node raises an alert and counts toward neither the exit gate nor a baseline.
8. Exit when, over at least 24 hours (target) of mirrored traffic, each Route meets one test and every disagreement is explained by an accepted report item: paired status classes agree for 99.9% or more of requests (target), or, unpaired, each status class's share differs from KrakenD's by 0.1 percentage points or less (target).

### Stage 5: side-by-side cutover

1. Before any weight above 0, confirm that every serving Node is ready on the expected digest and resolves the production State Store, which no shadow Node has used; with `provider: env`, a changed `RURALZ_STATE_STORE_URL` takes effect only after a Node restart.
2. Shift client traffic by load balancer weight in steps of 1%, 5%, 25%, 50% and 100% (target), baking each step for at least 1 hour and one daily peak before the next (target). Avoid DNS weighting; resolver caching delays rollback.
3. Keep session affinity for long-lived connections such as WebSocket, Planned (M3), so shifts move only new sessions.
4. Account for split enforcement. While both gateways serve traffic, each enforces its own limits, KrakenD per instance and Ruralz Cluster-wide on the rule 8 State Store, so admitted traffic can exceed one limit. KrakenD keeps its full limits throughout, so a rollback needs no KrakenD limit change and the excess is bounded by the Ruralz share. Either accept it or lower only the Ruralz `ratelimit` and `quota` values through a new Revision per weight step, reviewed with `ruralz bundle diff` and covered by the bad configuration change trigger.
5. Expect Quotas to restart: KrakenD counters are not migrated, so a Consumer can get up to one extra window allowance; where that matters, lower the first window's `quota` `limit`.
6. Watch the rollback triggers continuously; any trigger returns the weight to KrakenD before investigation.
7. At 100% (target), keep KrakenD running and unchanged for the retention period, 14 days (target).

### Rollback

Rollback is a traffic shift first; configuration rollback inside Ruralz comes later. Each trigger compares one Route over one window against KrakenD's side in the load balancer's logs, and fires only with at least 1,000 requests in the window (target). The 5xx trigger reads `ruralz_http_requests_total{route,status_class}`; the others read access logs, so a window with `queue_full` drops (Stage 4 rule 7) raises an alert, holds the weight step and cannot clear a trigger.

| Trigger | Threshold (proposed) | Action |
|---|---|---|
| 5xx rate on Ruralz Gateway, from `ruralz_http_requests_total`, above KrakenD's for the same Routes | More than 0.5 percentage points for 5 minutes (target) | Set the Ruralz weight to 0 |
| Gateway-added p99 of a Route, from `gateway_duration` | More than 50% above its Stage 4 baseline for 10 minutes (target); Routes without shadow data use their first 1% bake as baseline | Set the Ruralz weight to 0 |
| Authentication or authorization disagreement | The Route's `RZ-AUTH` 401 and 403 rate differs from KrakenD's 401 and 403 rate in the same window by more than 0.1 percentage points for 5 minutes (target) | Set the Ruralz weight to 0; treat a lower Ruralz rate as a security incident |
| State Store degraded | `RZ-STS` rejections, or `ruralz_filter_failures_total` with mode `open` above its Stage 4 rate | Set the Ruralz weight to 0, then restore the State Store |
| A bad configuration change during cutover | A new Revision causes any trigger above | Control mode: `ruralz rollout rollback ROLLOUT_ID`; file mode: re-publish the previous Revision digest or rendered Bundle |

Steps:

1. Shift the weight back to KrakenD at the load balancer; unchanged and fully sized, it serves all traffic at once.
2. Record the Routes, report items and metrics that triggered rollback.
3. Fix the Bundle or ported code, re-run Stage 2, and restart from Stage 3; shadowing may be shortened only for fixes confined to already shadowed Routes.
4. During the retention period after full weight, rollback follows the same steps; after decommission, rollback is no longer a traffic shift and needs a KrakenD redeploy from its retained image.

### Stage 6: decommission

1. After the retention period without a trigger, remove KrakenD from the load balancer, then scale it to zero, keeping its last image and rendered file for 90 days (target).
2. Remove the shadow Node set and the shadow State Store.
3. Apply any access log filter deferred in Stage 1.
4. Lift the configuration freeze; later changes go through Git, `ruralz bundle diff` review and, in Control mode, canary Rollouts.

### Gate summary

| Stage | Entry gate | Exit gate |
|---|---|---|
| 0 Prepare | Decision to migrate | Rendered file, custom code inventory, freeze date, State Stores |
| 1 Import and review | Rendered file | No unresolved `manual` item; every `approximate` item decided; access logging unfiltered; Bundle validates |
| 2 Test before traffic | Validated Bundle | Case suite passes against KrakenD and against local `ruralzd` |
| 3 Deploy dark | Passing case suite | Serving Nodes ready on the expected digest and the production State Store; one access log record per request, with log shipping sized |
| 4 Shadow traffic | Ready serving Nodes; shadow Node set on the shadow State Store | Stage 4 exit criteria met over windows without access log drops; per-Route baselines recorded |
| 5 Side-by-side cutover | Shadow exit; serving Nodes confirmed on the production State Store | 100% weight baked, no trigger (target) |
| 6 Decommission | Retention period ended | KrakenD removed |

## Migration gaps

The [KrakenD EE parity matrix](01-krakend-ee-parity-matrix.md#gaps-and-non-goals) owns every status; this section adds each gap's migration effect.

### Not planned features

The parity matrix marks 13 rows Not planned, three EE-only ([Not planned rows](01-krakend-ee-parity-matrix.md#not-planned-rows)); their items import as `manual`.

| KrakenD feature | EE-only | Parity reason | Migration path |
|---|---|---|---|
| Lua scripting | No | No Lua runtime (ADR-0011) | [Lua to CEL or WASM Plugins](#lua-to-cel-or-wasm-plugins) |
| Lua advanced helpers | Yes | Follows the Lua decision | CEL extensions, or `crypto.use` and other Host Functions in a Plugin |
| Custom Go plugins | No | No Go `plugin` or shared-object loading (vision non-goal 3) | [Go plugins to built-in Filters or WASM Plugins](#go-plugins-to-built-in-filters-or-wasm-plugins) |
| NTLM authentication | Yes | Connection-bound authentication breaks pooling; MD4 and HMAC-MD5 fail the FIPS build | `auth.upstream-oauth2` or mTLS toward the `Upstream`, if the server accepts it |
| Lambda functions | No | No researched invocation library or protocol value | An `http` `Upstream` to an HTTP-reachable function (OQ-krakend-ee-parity-matrix-10) |
| AMQP/RabbitMQ consumer | No | No researched Go library (OQ-multi-protocol-14) | A bridge that republishes to Kafka, NATS or MQTT |
| AMQP/RabbitMQ producer | No | Same | `kafka`, `nats` or `mqtt` Upstreams, Planned (M4), or a bridge |
| Azure Service Bus topic and subscription | No | Same | A bridge |
| Google Cloud Pub/Sub | No | Same | A bridge |
| Amazon SNS | No | Same | A bridge |
| Amazon SQS | No | Same | A bridge |
| ELK Stack dashboard | No | Grafana dashboards only, Planned (M1) | JSON access logs into any log store |
| New Relic (native SDK) | Yes | No vendor SDKs ([ADR-0010](../adr/0010-telemetry-opentelemetry-first.md)) | OTLP through the operator's Collector |

### Partial parity that changes migrated behavior

The matrix lists 17 partial-parity rows ([Partial parity](01-krakend-ee-parity-matrix.md#partial-parity)); each surfaces in an importer run as `approximate` or `manual` items, or as a pre-import conversion:

| KrakenD feature | Effect on a migrated configuration | Open question |
|---|---|---|
| Multi-format configuration | TOML, HCL and properties sources are converted to JSON before import | OQ-migration-from-krakend-9 |
| Automatic output encoding | XML, YAML and negotiated responses are `manual` | OQ-krakend-ee-parity-matrix-2 |
| Martian (DSL) | Header sets import; body and query modifiers are `manual` | OQ-krakend-ee-parity-matrix-1 |
| Zero-trust parameter forwarding | Client headers and query strings reach Upstreams unless a Policy removes them; security-flagged | OQ-krakend-ee-parity-matrix-5, OQ-migration-from-krakend-7 |
| Workflows | Nested workflows are `manual` | OQ-data-plane-3 |
| Mocked data | Static responses need a user-written Plugin | OQ-krakend-ee-parity-matrix-3 |
| Security Policies Engine | Response-context rules and `geoIP()` inside expressions are `manual` with a hold | OQ-security-and-identity-14 |
| Configurable client redirects | Upstream 3xx responses pass through; Route-issued redirects need a user-written Plugin | OQ-traffic-management-and-resilience-13 |
| LLM routing, multi-routing, and aggregation | `ai/llm` is `manual` before M3, then `approximate`; aggregation is buffered only | OQ-krakend-ee-parity-matrix-7 |
| Async agents | `async/amqp` agents are `manual` with no Ruralz source | OQ-multi-protocol-14 |
| SOAP integration | `backend/soap` is `manual`; CEL cannot read XML responses <!-- alias-ok --> | OQ-krakend-ee-parity-matrix-1 |
| Concurrent calls | Duplicate parallel requests are dropped; hedging arrives Planned (M4) | OQ-traffic-management-and-resilience-5 |
| Service rate limit and Proxy rate limit | A constant key is one State Store hot key; the proxy limit moves to Route scope | OQ-traffic-management-and-resilience-1; OQ-krakend-ee-parity-matrix-4 |
| Bot detector | Long pattern lists need a Plugin | None |
| API governance | Weighted quota costs are not imported | OQ-traffic-management-and-resilience-10 |
| Granular OpenTelemetry | Per-endpoint sampling becomes Gateway-wide | OQ-observability-5 <!-- alias-ok --> |

### Features that arrive after the importer

Items whose target postdates the importer, Planned (M2), import as `manual` (rule 4); their Routes stay on KrakenD longer. KrakenD streaming Routes are the exception: they import `approximate` through the no-op rows, since the reverse proxy exists from Planned (M1).

| KrakenD feature group | Ruralz target | Planned |
|---|---|---|
| AI Gateway (`ai/llm`), token quotas, MCP Server | `AIProvider`, `AIModel`, `ai.token-budget` | Planned (M3) |
| gRPC server and client, direct WebSockets, multiplexer, SSE per-event flushing and `onChunk` Policies | `match.grpc`, `grpc` and `websocket` Upstreams, `onChunk` | Planned (M3) |
| GraphQL adapter | `graphql` Upstreams | Planned (M3) |
| Async agents, Kafka and NATS publishers and subscribers | `match.topic`, `kafka` and `nats` Upstreams | Planned (M4) |
| Concurrent calls | Hedging | Planned (M4) |
| Static web server, SOAP, response JSON Schema validation, intermediary web proxy, OpenTelemetry SaaS authentication, advanced logging, Moesif | Per the parity matrix | Planned (M5) |

### Behavior changes to plan for

These hold even at `equivalent` fidelity:

| Area | KrakenD | Ruralz | Plan |
|---|---|---|---|
| Rate limit scope | Stateless limits apply per instance ([source](https://www.krakend.io/docs/throttling/cluster/)) | One Cluster-wide limit through the `redis` State Store of rule 8 ([ADR-0008](../adr/0008-rate-limiting-local-bucket-and-gcra.md)) | Recompute limits as totals |
| Header forwarding | Nothing forwarded unless allowlisted ([source](https://www.krakend.io/schema/v2.13/krakend.json)) | Forwarded unless a Policy removes it | Resolve before Stage 3 |
| Two client credentials on one path | Both namespaces are valid on one endpoint ([source](https://www.krakend.io/schema/v2.13/krakend.json)) | One `auth` slot Policy per Route (OQ-configuration-model-13) | Decide per Route which check stays, or keep the Route on KrakenD <!-- alias-ok --> |
| Client address | Router settings `trusted_proxies` and `remote_ip_headers` ([source](https://www.krakend.io/docs/service-settings/router-options/)) | `source.ip` is the load balancer until OQ-security-and-identity-6 closes | Enforce IP rules at the load balancer until then |
| API key storage | Inline, plain or hashed ([source](https://www.krakend.io/docs/enterprise/authentication/api-keys/)) | SHA-256 digests in `Consumer` resources; every key change is a new Revision | Reissue keys stored with other hashes |

## Open questions

| ID | Question | Options | Owner | Blocking? |
|---|---|---|---|---|
| OQ-migration-from-krakend-1 | Which Policy `config` fields must feature documents register so these imports reach `equivalent`: `cors` headers, max age, credentials and private network; the `validation.json-schema` schema; `auth.api-key` query-string credentials; `auth.upstream-sigv4` region and service; a `ratelimit` burst (OQ-traffic-management-and-resilience-1)? | (a) Register them as each feature document authors its `config` (proposed); (b) Keep these items `approximate` or `manual` | configuration-model | No |
| OQ-migration-from-krakend-2 | Where do a listener bind address (`listen_ip`), server read, header, write and idle timeouts and h2c (`use_h2c`) live? | (a) New `Gateway` `listeners[]` fields; (b) `RURALZ_*` process settings, a pack section 2 amendment; (c) Fixed Node defaults, imported `approximate` | data-plane | No |
| OQ-migration-from-krakend-3 | Should `Upstream` expose connection tuning (dial timeout, keep-alive, idle pool sizes, response header timeout, compression)? | (a) Data plane defaults, imported `approximate` (current); (b) An `Upstream.spec` connection block | traffic-management-and-resilience | No |
| OQ-migration-from-krakend-4 | Which field names the OpenTelemetry service name that KrakenD takes from `name` or `service_name`? | (a) `Gateway` `metadata.name`; (b) A `telemetry` field; (c) The Collector's resource processor | observability | No |
| OQ-migration-from-krakend-5 | Does an `Upstream.spec.tls` block with only `sni` enable TLS with system trust roots, as KrakenD's `https` hosts need? | (a) Yes, presence of `tls` selects TLS (proposed); (b) An explicit enable field | configuration-model | Yes, for the M2 importer |
| OQ-migration-from-krakend-6 | Do step `select` and `rename` accept dot-notation paths, and how is a KrakenD `deny` list expressed? | (a) Dot paths, `deny` through `transform.response`; (b) Top-level fields only | data-plane | No |
| OQ-migration-from-krakend-7 | Until OQ-krakend-ee-parity-matrix-5 closes, how does the importer preserve KrakenD's zero-trust forwarding, which applies whether or not `input_headers` is set? | (a) One security-flagged `manual` item per Route that blocks Stage 3 (current); (b) A Ruralz-published header allowlist Plugin emitted as an Upstream-scoped `plugin` Policy; (c) An emitted Upstream-scoped `headers` removal once that question adds the fields | migration-from-krakend | No |
| OQ-migration-from-krakend-8 | Should pack section 12 register a versioned format for the JSON fidelity report, and where is it written? | (a) A versioned identifier and a hidden file under the output directory (proposed); (b) Standard output only | cli-and-api-surface | No |
| OQ-migration-from-krakend-9 | Should the importer read YAML or TOML KrakenD files directly? | (a) Rendered JSON only (current); (b) Also YAML; (c) Also TOML | migration-from-krakend | No |
| OQ-migration-from-krakend-10 | How is SM-8 measured, answering OQ-vision-and-positioning-13: which corpus, and do code-only or forwarding-only `manual` items still allow partial fidelity? | (a) A curated corpus in the monorepo plus community submissions; partial means no `manual` item (proposed); (b) Count code-only `manual` items as partial; (c) Exclude forwarding items once the owner accepts them | migration-from-krakend | Yes, for the M2 exit criterion 3 |
| OQ-migration-from-krakend-11 | Should Plugin ABI v1 expose Route template captures and the response status, which ported modifiers read through `Params` and `StatusCode`? | (a) New Host Functions before the ABI freezes (proposed); (b) Guests parse the path; no status | wasm-plugin-system | No |
| OQ-migration-from-krakend-12 | How should the importer convert KrakenD's per-instance rate limits to Cluster-wide limits on the rule 8 State Store, and protect hot constant keys? | (a) Copy the per-instance value, `approximate` (current); (b) A CLI flag giving the KrakenD instance count to multiply by; (c) Declared per-Node ceilings or `localOnly` mode for hot constant keys once OQ-traffic-management-and-resilience-1 adds them | migration-from-krakend | No |
| OQ-migration-from-krakend-13 | Can API keys that KrakenD stored as `fnv128`, `sha1` or salted `sha256` hashes migrate without reissue? | (a) Reissue keys (current); (b) A time-limited legacy verifier in `auth.api-key` | security-and-identity | No |
| OQ-migration-from-krakend-14 | Should `documentation/openapi` summaries and descriptions survive import for `ruralz bundle export openapi`? | (a) Route annotations the exporter reads; (b) Not carried (current) | cli-and-api-surface | No |
| OQ-migration-from-krakend-15 | Can an authorization denial carry a custom status and body, as KrakenD Security Policies `error` and Lua `custom_error` do? | (a) Fields in `authz.cel` `config`; (b) A `plugin` Policy only (current); (c) The Data plane error format only | security-and-identity | No |
| OQ-migration-from-krakend-16 | When a single composition step rewrites a path, is the client request body forwarded for methods with a body? | (a) Steps forward the body when `method` equals the incoming method; (b) URL rewrite on plain `upstreams` per OQ-traffic-management-and-resilience-13 | traffic-management-and-resilience | No |
