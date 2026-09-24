# Research: KrakenD configuration model (for the import tool and parity matrix)

- **Topic:** the structure of the `krakend.json` configuration: top-level keys, endpoint and backend objects, every `extra_config` namespace (CE and EE), Flexible Configuration templating, Go plugin interfaces, Lua hooks, EE-specific configuration (workflows, security policies, API keys, tiered rate limits, quotas, `ai/llm`), and scaling/benchmark claims.
- **Snapshot date:** 2026-09-23
- **Versions at snapshot:** KrakenD CE v2.13.11 (released 2026-09-08) (https://github.com/krakend/krakend-ce/releases); EE 2.13 line (EE 2.13 released 2026-03-12) (https://www.krakend.io/blog/krakend-ee-2.13-release-notes/); config schema `v2.13` (https://www.krakend.io/schema/v2.13/krakend.json).
- **Method:** I downloaded the official JSON Schema `https://www.krakend.io/schema/v2.13/krakend.json` (589,381 bytes, draft 2019-09, 109 `$defs`) and walked it with a script to list every property and namespace. I read the CE source on GitHub (`krakend-ce` `cmd/krakend-ce/main.go` and `go.mod`, `krakend-flexibleconfig`, and Lura's plugin loaders) and cloned the CE docs repository `krakend/krakend-documentation` (commit `40cabc0`, 2026-09-01) to read page front-matter. EE-only doc pages, release notes and blog posts were fetched from krakend.io. Where a schema description and a doc page disagree, the disagreement is listed under Gaps.

## 1. File-level facts a migration tool must handle

| Fact | Detail | Source |
|---|---|---|
| Format version | `version` is a constant: it must be `3` for KrakenD v2.0 and later. It is the syntax version, not the product version | (https://www.krakend.io/schema/v2.13/krakend.json) |
| Required root keys | The schema requires only `version`. The structure doc also marks only `version` as mandatory; `endpoints` appears in its basic structure but is not labelled mandatory | (https://www.krakend.io/schema/v2.13/krakend.json), (https://www.krakend.io/docs/configuration/structure/) |
| Strictness | The root, `endpoint`, `backend` and every `*_extra_config` object set `additionalProperties: false`. Only keys matching `^[@$_#]` are accepted as extra keys, which makes them usable as comments | (https://www.krakend.io/schema/v2.13/krakend.json) |
| File formats (docs) | The docs list `.json` (recommended), `.toml`, `.yaml`, `.yml`, `.properties`, `.props`, `.prop` and `.hcl`. `--lint` works only on JSON | (https://www.krakend.io/docs/configuration/supported-formats/) |
| File formats (code) | CE v2.13 parses config through `krakend-koanf` v0.1.1. Its README says it reads "json / yaml / toml" files | (https://github.com/krakend/krakend-ce/blob/master/go.mod), (https://github.com/krakend/krakend-koanf) |
| Reserved paths | `/__health/`, `/__debug/`, `/__echo/`, `/__catchall` and `/__stats/` cannot be declared as endpoints | (https://www.krakend.io/schema/v2.13/krakend.json) |
| Env overrides | Setting `KRAKEND_<UPPERCASE_KEY>` overrides any root-level string, integer or boolean, but only when that key already exists in the file | (https://www.krakend.io/docs/configuration/environment-vars/) |
| Reload | Changing the configuration requires a restart, and the docs recommend blue/green deployment. The `:watch` image restarts the process on file change and is "not recommended for production workloads" | (https://www.krakend.io/docs/deploying/), (https://www.krakend.io/docs/developer/hot-reload/) |
| Legacy v1 migration | `krakend-config-migrator` converts 0.x/1.x configs to 2.0. It renames namespaces and fields, for example `whitelist`/`blacklist` to `allow`/`deny` | (https://www.krakend.io/docs/configuration/migrating/) |

## 2. Top-level (service) keys, schema v2.13

The schema has 36 root properties (https://www.krakend.io/schema/v2.13/krakend.json).

| Key | Type / default | Purpose | Source |
|---|---|---|---|
| `version` | const `3` | Syntax version | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `name` | string | Friendly name used in telemetry | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `port` | int, `8080` | Listening TCP port | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `listen_ip` | string, `0.0.0.0` | Bind address (IPv4 or IPv6) | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `timeout` | duration, `2s` | Default endpoint timeout, overridable per endpoint | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `cache_ttl` | duration, `0s` | Default `Cache-Control: public, max-age` header. The gateway does not cache anything because of this key | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `output_encoding` | `json` | Default encoding for all endpoints | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `host` | array | Default backend host list | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `endpoints` | array | The API contract | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `extra_config` | object | Service-scope namespaces | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `async_agent` | array | Async agents (consumers) | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `tls` | object | Server TLS: `keys`, `ca_certs`, `enable_mtls`, `min_version`/`max_version` (both default `TLS13`), `cipher_suites` (default `[4865,4866,4867]`), `curve_preferences` (default `[23,24,25]`), `disable_system_ca_pool`, `disabled` | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `client_tls` | object | Upstream TLS transport settings | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `plugin` | object | Required `pattern` (default `.so`) and `folder` (must end in a slash) | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `debug_endpoint`, `echo_endpoint` | bool, `false` | Enable `/__debug/` and `/__echo/` | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `disable_rest` | bool, `false` | Allow non-RESTful endpoint patterns | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `sequential_start` | bool, `false` | Register async agents in order | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `use_h2c` | bool, `false` | HTTP/2 without TLS | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `dns_cache_ttl` | `30s` | Service-discovery DNS cache | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `max_header_bytes` | int, `1000000` | Max request header size | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `max_shutdown_wait_time` | `0s` | Graceful drain limit | (https://www.krakend.io/schema/v2.13/krakend.json) |
| Server timeouts | `read_timeout`, `read_header_timeout`, `write_timeout`, `idle_timeout` (all `0s`) | HTTP server limits | (https://www.krakend.io/schema/v2.13/krakend.json) |
| Client transport | `dialer_timeout` `0s`, `dialer_keep_alive` `15s`, `dialer_fallback_delay` `300ms`, `idle_connection_timeout`, `response_header_timeout`, `expect_continue_timeout`, `max_idle_connections` `0`, `max_idle_connections_per_host` `250`, `disable_keep_alives`, `disable_compression` | Upstream HTTP transport | (https://www.krakend.io/schema/v2.13/krakend.json) |

`async_agent[]` items require `name`, `consumer` (`topic`, `workers` default 1, `max_rate`, `timeout`), `backend` and `extra_config`. They also accept `connection` (`max_retries` 0 = unlimited, `backoff_strategy` default `fallback`, `health_interval` default `1s`) and `encoding`. The driver is chosen in `extra_config` as `async/amqp` or `async/kafka` (https://www.krakend.io/schema/v2.13/krakend.json). Kafka in async agents arrived in EE 2.13 (https://www.krakend.io/blog/krakend-ee-2.13-release-notes/).

## 3. Endpoint object

The endpoint object requires `endpoint` and `backend` (https://www.krakend.io/schema/v2.13/krakend.json).

| Field | Type / default | Notes | Source |
|---|---|---|---|
| `endpoint` | string | Case-sensitive path starting with `/`. Supports `{placeholders}` | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `method` | enum `GET` (default), `POST`, `PUT`, `PATCH`, `DELETE` | One method per endpoint entry | (https://www.krakend.io/docs/endpoints/) |
| `output_encoding` | `json`, `json-collection`, `yaml`, `fast-json`, `xml`, `negotiate`, `string`, `no-op` | `no-op` is proxy-only, with no merging | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `concurrent_calls` | int, `1` | Sends the same request to the backend N times in parallel | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `timeout` | duration, `2s` | Covers the whole pipe, including all backends | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `cache_ttl` | duration | Overrides the root value. A zero value uses the root value | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `input_query_strings` | array, `[]` | Case-sensitive allowlist. Nothing is forwarded by default | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `input_headers` | array, `[]` | Case-insensitive allowlist. No client headers are forwarded by default | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `backend` | array | Backend objects | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `extra_config` | object | Endpoint-scope namespaces | (https://www.krakend.io/schema/v2.13/krakend.json) |

Endpoint-scope `proxy` accepts `sequential`, `sequential_propagated_params`, `combiner`, `static`, `flatmap_filter`, `max_payload` and `decompress_gzip`. Backend-scope `proxy` accepts `flatmap_filter` and `shadow` (https://www.krakend.io/schema/v2.13/krakend.json).

## 4. Backend object

The backend object requires `url_pattern` (https://www.krakend.io/schema/v2.13/krakend.json).

| Field | Type / default | Notes | Source |
|---|---|---|---|
| `url_pattern` | string | Path only, with no scheme or host | (https://www.krakend.io/docs/backends/) |
| `host` | array | Load-balanced host list | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `method` | enum, same as endpoint if omitted | Allows `GET` through `TRACE`, including `OPTIONS`, `HEAD`, `CONNECT` | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `encoding` | `json` (default), `safejson`, `fast-json`, `xml`, `rss`, `string`, `no-op`, `yaml` | How the response is parsed | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `group` | string (example `backend1`) | Wraps the response under a key | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `target` | string | Unwraps a nested object | (https://www.krakend.io/docs/backends/data-manipulation/) |
| `allow` / `deny` | array | Field allowlist or denylist. Dot notation reaches nested fields | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `mapping` | object | Renames fields | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `is_collection` | bool | Set when the response is a JSON array. The schema lists default `true` while the description implies opt-in (see Gaps) | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `sd` | `static` (default), `dns`, `dns-shared` | Service discovery | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `sd_scheme` | string, `http` | Scheme for resolved SD hosts | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `disable_host_sanitize` | bool, `false` | Needed for `sd=dns` and for non-HTTP schemes such as `amqp://`, `nats://`, `kafka://` | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `input_headers` / `input_query_strings` | array | A second filter per backend | (https://www.krakend.io/schema/v2.13/krakend.json) |
| `extra_config` | object | Backend-scope namespaces | (https://www.krakend.io/schema/v2.13/krakend.json) |

## 5. Full `extra_config` namespace catalogue (schema v2.13)

The catalogue was built from `service_extra_config`, `endpoint_extra_config`, `backend_extra_config`, `workflow_extra_config` and the async-agent `extra_config` in the v2.13 schema (https://www.krakend.io/schema/v2.13/krakend.json).

- **Scope** letters: S = service, E = endpoint, B = backend, W = workflow, A = async agent.
- **Edition** is "EE" when the schema description starts with "Enterprise only" or the feature is listed as Enterprise on the features page (https://www.krakend.io/features/).
- The doc URL in each row is usually the page the schema links to. Rows where the schema links no page, or a page that returns 404, use the matching doc page or the release notes, and four rows (`redis`, `backend/http/client`, `plugin/middleware`, `plugin/req-resp-modifier`) cite a different doc page from the one the schema links.

### 5.1 Authentication and authorization

| Namespace | Scope | Edition | Purpose | Doc URL |
|---|---|---|---|---|
| `auth/validator` | S, E | CE | JWT validation (E). At S it holds the global JWK HTTP client and cache settings | (https://www.krakend.io/docs/authorization/jwt-validation/) |
| `auth/signer` | E | CE | Signs tokens from legacy login backends | (https://www.krakend.io/docs/authorization/jwt-signing/) |
| `auth/revoker` | S | CE (bloom filter). The cluster-wide revoke server is EE | Revokes still-valid JWTs | (https://www.krakend.io/docs/authorization/revoking-tokens/) |
| `auth/client-credentials` | B | CE | OAuth2 2-legged token for the upstream | (https://www.krakend.io/docs/authorization/client-credentials/) |
| `auth/api-keys` | S, E | EE | API-key RBAC | (https://www.krakend.io/docs/enterprise/authentication/api-keys/) |
| `auth/basic` | S, E | EE | Basic auth | (https://www.krakend.io/docs/enterprise/authentication/basic-authentication/) |
| `auth/aws-sigv4` | B | EE | SigV4-signs upstream calls | (https://www.krakend.io/docs/enterprise/authentication/aws-sigv4/) |
| `auth/gcp` | B | EE | GCP service-account auth to the upstream | (https://www.krakend.io/docs/enterprise/authentication/gcloud/) |
| `auth/ntlm` | B | EE | NTLM to Microsoft servers | (https://www.krakend.io/docs/enterprise/authentication/ntlm/) |
| `security/policies` | E, B, W | EE | CEL policy engine (req/resp/jwt) | (https://www.krakend.io/docs/enterprise/security-policies/) |

### 5.2 Traffic management

| Namespace | Scope | Edition | Purpose | Doc URL |
|---|---|---|---|---|
| `qos/ratelimit/router` | E | CE | Per-node endpoint and per-client token bucket. Fields: `max_rate`, `client_max_rate`, `every`, `capacity`, `client_capacity`, `strategy`, `key`, `num_shards`, `cleanup_period`, `cleanup_threads` | (https://www.krakend.io/docs/endpoints/rate-limit/) |
| `qos/ratelimit/router/redis` | E | EE | Redis-backed endpoint limit | (https://www.krakend.io/docs/enterprise/throttling/endpoint-redis-rate-limit/) |
| `qos/ratelimit/service` | S | EE | Per-node service-wide limit | (https://www.krakend.io/docs/enterprise/service-settings/service-rate-limit/) |
| `qos/ratelimit/service/redis` | S | EE | Redis-backed cluster-wide service limit | (https://www.krakend.io/docs/enterprise/throttling/global-rate-limit/) |
| `qos/ratelimit/tiered` | S, E | EE | Limits selected by tier header or policy | (https://www.krakend.io/docs/enterprise/service-settings/tiered-rate-limit/) |
| `qos/ratelimit/proxy` | B | CE | Gateway-to-backend limit (`max_rate`, `capacity`, `every`) | (https://www.krakend.io/docs/backends/rate-limit/) |
| `qos/circuit-breaker` | B | CE | Consecutive-error breaker (`interval`, `timeout`, `max_errors`, `name`, `log_status_change`) | (https://www.krakend.io/docs/backends/circuit-breaker/) |
| `qos/circuit-breaker/http` | B | EE | Breaker driven by HTTP status codes | (https://www.krakend.io/docs/enterprise/backends/http-circuit-breaker/) |
| `qos/http-cache` | B | CE | In-memory cache that honours `Cache-Control` (GET/HEAD). Fields: `shared`, `max_items`, `max_size` | (https://www.krakend.io/docs/backends/caching/) |
| `governance/processors` | S | EE | Declares Redis-backed quota processors (`quotas[]`: `name`, `connection_name`, `rules[].limits[]` with `amount` and `unit` from second to year, `on_failure_allow`, `rejecter_cache`, `hash_keys`) | (https://www.krakend.io/docs/enterprise/governance/quota/) |
| `governance/quota` | S, E, B | EE | Attaches a quota (`quota_name`, `tier_key`, `tiers[]`, `weight_key`, `weight_strategy` body/header, `disable_quota_headers`, `on_unmatched_tier_allow`) | (https://www.krakend.io/docs/enterprise/governance/quota/) |
| `redis` | S | EE | Named Redis `connection_pools[]` (single address, `db`) and `clusters[]` (`addresses`). `pool_size` default 10, `max_retries` 3, `dial_timeout` 5s | (https://www.krakend.io/docs/enterprise/service-settings/redis-connection-pools/) |
| `security/bot-detector` | S, E | CE | Regex/User-Agent bot rejection | (https://www.krakend.io/docs/throttling/botdetector/) |

### 5.3 Security, routing and server

| Namespace | Scope | Edition | Purpose | Doc URL |
|---|---|---|---|---|
| `security/cors` | S, E | CE (S). The schema links E to an EE page | CORS (`allow_origins`, `allow_methods`, `allow_headers`, `expose_headers`, `max_age`, `allow_credentials`, `allow_private_network`, `options_passthrough`, `options_success_status`, `debug`) | (https://www.krakend.io/docs/service-settings/cors/) |
| `security/http` | S, E | CE | Security headers: HSTS, HPKP, clickjacking and similar | (https://www.krakend.io/docs/service-settings/security/) |
| `router` | S | CE | Gin router flags (`return_error_msg`, `disable_access_log`, `health_path`, `auto_options`, `max_payload`, `trusted_proxies`, `remote_ip_headers`, `logger_skip_paths` and others) | (https://www.krakend.io/docs/service-settings/router-options/) |
| `server/virtualhost` | S | EE | Different endpoint sets per Host header | (https://www.krakend.io/docs/enterprise/service-settings/virtual-hosts/) |
| `server/static-filesystem` | S | EE | Static web server on path prefixes | (https://www.krakend.io/docs/enterprise/endpoints/serve-static-content/) |
| `grpc` | S | EE | gRPC server (`catalog`, `server`) | (https://www.krakend.io/docs/enterprise/grpc/server/) |
| `websocket` | E | EE | WebSocket proxying and multiplexing | (https://www.krakend.io/docs/enterprise/websockets/) |
| `backend/conditional` | B | EE | Skips a backend unless a `header`, `policy` or `fallback` strategy matches | (https://www.krakend.io/docs/enterprise/backends/conditional/) |

### 5.4 Transformation and validation

| Namespace | Scope | Edition | Purpose | Doc URL |
|---|---|---|---|---|
| `proxy` | E, B, W | CE | Sequential proxy, flatmap, static responses, shadow backends | (https://www.krakend.io/docs/backends/flatmap/) |
| `modifier/lua-endpoint` | S, E | CE | Lua at the router layer | (https://www.krakend.io/docs/endpoints/lua/) |
| `modifier/lua-proxy` | E, W | CE | Lua at the endpoint proxy layer | (https://www.krakend.io/docs/endpoints/lua/) |
| `modifier/lua-backend` | B | CE | Lua per backend | (https://www.krakend.io/docs/endpoints/lua/) |
| `modifier/martian` | B | CE | Martian DSL request/response modifiers | (https://www.krakend.io/docs/backends/martian/) |
| `modifier/jmespath` | E, B, W | EE | JMESPath response queries | (https://www.krakend.io/docs/enterprise/endpoints/jmespath/) |
| `modifier/request-body-generator` | E, B, W | EE | Go-template request body | (https://www.krakend.io/docs/enterprise/backends/body-generator/) |
| `modifier/body-generator` | B | EE | Alias with the same schema as the request body generator | (https://www.krakend.io/docs/enterprise/backends/body-generator/) |
| `modifier/response-body-generator` | E, B, W | EE | Go-template response body | (https://www.krakend.io/docs/enterprise/backends/response-body-generator/) |
| `modifier/response-body` | E, B | EE | Content replacer: literal or regex replacement. E is described as an alias of the response body generator | (https://www.krakend.io/docs/enterprise/endpoints/content-replacer/) |
| `modifier/request-body-extractor` (+ `/early` at E) | S, E | EE | Promotes body fields to headers or query strings (new in 2.13) | (https://www.krakend.io/docs/enterprise/endpoints/request-body-extractor/) |
| `modifier/response-headers` | S | EE | Declarative response-header transforms | (https://www.krakend.io/docs/enterprise/service-settings/response-headers-modifier/) |
| `validation/cel` | E, B, W | CE | CEL expressions that abort the request when false | (https://www.krakend.io/docs/endpoints/common-expression-language-cel/) |
| `validation/json-schema` | E, W | CE | Request-body JSON Schema validation | (https://www.krakend.io/docs/endpoints/json-schema/) |
| `validation/response-json-schema` | E, B | EE | Response JSON Schema validation | (https://www.krakend.io/docs/enterprise/endpoints/response-schema-validator/) |
| `workflow` | B | EE | A nested, unpublished endpoint inside a backend | (https://www.krakend.io/docs/enterprise/endpoints/workflows/) |

### 5.5 Backend connectivity

| Namespace | Scope | Edition | Purpose | Doc URL |
|---|---|---|---|---|
| `backend/http` | B | CE | `return_error_code` or `return_error_details` | (https://www.krakend.io/docs/backends/detailed-errors/) |
| `backend/http/client` | B | CE subset since v2.11 (`send_body_on_redirect`). EE adds `client_tls`, `no_redirect`, `proxy_address` | Per-backend HTTP client | (https://www.krakend.io/docs/backends/http-client/) |
| `backend/graphql` | B | CE | REST-to-GraphQL adapter | (https://www.krakend.io/docs/backends/graphql/) |
| `backend/grpc` | B | EE | gRPC upstream from protobuf descriptors | (https://www.krakend.io/docs/enterprise/backends/grpc/) |
| `backend/lambda` | B | CE | AWS Lambda invocation | (https://www.krakend.io/docs/backends/lambda/) |
| `backend/amqp/consumer`, `backend/amqp/producer` | B | CE | RabbitMQ consume/produce | (https://www.krakend.io/docs/backends/amqp-consumer/) |
| `backend/pubsub/publisher`, `backend/pubsub/subscriber` | B | CE | Go CDK pub/sub drivers | (https://www.krakend.io/docs/backends/pubsub/) |
| `backend/pubsub/publisher/kafka`, `backend/pubsub/subscriber/kafka` | B | EE | Advanced Kafka connection settings (2.13) | (https://www.krakend.io/blog/krakend-ee-2.13-release-notes/) |
| `backend/soap` | B | EE | SOAP request building | (https://www.krakend.io/docs/enterprise/backends/soap/) |
| `backend/static-filesystem` | B | EE | Serves or mocks from disk | (https://www.krakend.io/docs/enterprise/endpoints/serve-static-content/) |
| `async/amqp` | A | CE | AMQP async-agent driver | (https://www.krakend.io/docs/async/amqp/) |
| `async/kafka` | A | EE | Kafka async-agent driver | (https://www.krakend.io/blog/krakend-ee-2.13-release-notes/) |

### 5.6 AI, documentation, telemetry, plugins

| Namespace | Scope | Edition | Purpose | Doc URL |
|---|---|---|---|---|
| `ai/llm` | B | EE (since v2.10) | Unified LLM interface: vendor keys `openai`, `anthropic`, `gemini`, `mistral`, `bedrock` | (https://www.krakend.io/docs/enterprise/ai-gateway/unified-llm-interface/) |
| `ai/mcp` | S, E | EE | MCP servers (S) and the MCP entrypoint endpoint (E, `server_name`) | (https://www.krakend.io/docs/enterprise/ai-gateway/mcp-server/) |
| `documentation/openapi` | S, E | EE | Feeds `krakend openapi export` | (https://www.krakend.io/docs/enterprise/developer/openapi/) |
| `documentation/postman` | S, E | EE | Feeds `krakend postman export` | (https://www.krakend.io/docs/enterprise/developer/postman/) |
| `telemetry/opentelemetry` | S, E, B | CE at S, E and B: the CE docs and features page list per-endpoint and per-backend overrides as Community, although the schema marks the E/B override objects "Enterprise only". Exporter overrides at E (`exporters_override`) are EE (https://www.krakend.io/docs/telemetry/opentelemetry-by-endpoint/) | OTel metrics and traces (`service_name`, `exporters`, `layers`, `trace_sample_rate`, `skip_paths` and others) | (https://www.krakend.io/docs/telemetry/opentelemetry/) |
| `telemetry/opentelemetry-security` | S | EE (features page: "OpenTelemetry SaaS authentication") | Auth for SaaS OTLP export | (https://www.krakend.io/docs/enterprise/telemetry/opentelemetry-security/) |
| `telemetry/logging` | S, B | CE at S. B (backend log) is EE | Extended logging | (https://www.krakend.io/docs/logging/) |
| `telemetry/gelf` | S | CE | Graylog GELF | (https://www.krakend.io/docs/logging/graylog-gelf/) |
| `telemetry/logstash` | S | CE | Logstash format (needs `telemetry/logging`) | (https://www.krakend.io/docs/logging/logstash/) |
| `telemetry/metrics` | S | CE | Extended metrics, `/__stats/` | (https://www.krakend.io/docs/telemetry/extended-metrics/) |
| `telemetry/influx` | S | CE | Native InfluxDB push | (https://www.krakend.io/docs/telemetry/influxdb-native/) |
| `telemetry/opencensus` | S | CE, deprecated | Legacy exporters. The docs say to move to OTel | (https://www.krakend.io/docs/telemetry/opencensus/) |
| `telemetry/newrelic` | S | EE | New Relic native SDK | (https://www.krakend.io/docs/enterprise/telemetry/newrelic/) |
| `telemetry/moesif` | S | EE | Moesif analytics and billing | (https://www.krakend.io/docs/enterprise/governance/moesif/) |
| `plugin/http-server` | S | CE (Go plugins removed in CE 3.0) | Router-layer handler plugins. Built-in EE plugin sub-keys: `geoip`, `ip-filter`, `jwk-aggregator`, `redis-ratelimit`, `static-filesystem`, `url-rewrite`, `virtualhost`, `wildcard` | (https://www.krakend.io/docs/extending/http-server-plugins/) |
| `plugin/http-client` | B | CE (removed in CE 3.0) | Replaces the backend HTTP client | (https://www.krakend.io/docs/extending/http-client-plugins/) |
| `plugin/req-resp-modifier` | E, B, W | CE (removed in CE 3.0) | Request/response modifiers. EE built-ins: `content-replacer`, `ip-filter`, `response-schema-validator` | (https://www.krakend.io/docs/extending/plugin-modifiers/) |
| `plugin/middleware` | E, B | EE (v2.10+) | Proxy-layer middleware plugins | (https://www.krakend.io/docs/enterprise/extending/middleware-plugins/) |

### 5.7 Namespaces named in the brief that do not exist in schema v2.13

| Brief item | Finding | Source |
|---|---|---|
| `sse` | There is no namespace. EE streaming/SSE (since v2.12) is configured with `output_encoding: "no-op"` on the endpoint, `encoding: "no-op"` on the backend, and a long `timeout`. It is proxy-only and no response manipulation is possible | (https://www.krakend.io/docs/enterprise/endpoints/streaming/) |
| `github_com/devopsfaith/krakend-*` | These are legacy 1.x names. CE v2.13 still registers 34 aliases that map them to the v2 names (table in §6) | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `ai/llm` "providers, routing, prompt templates, quotas" | `ai/llm` holds only per-vendor blocks. Routing, quotas and templates come from other namespaces (§8) | (https://www.krakend.io/schema/v2.13/krakend.json) |

## 6. Legacy namespace aliases (CE `main.go`)

On startup CE sets `config.ExtraConfigAlias[alias] = key` for each pair below. The v2 names are therefore aliases, and Lura still reads the legacy keys internally (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go). Lura's modifier plugin package still defines `Namespace = "github.com/devopsfaith/krakend/proxy/plugin"` (https://github.com/luraproject/lura/blob/master/proxy/plugin/modifier.go).

| Legacy key | v2 name | Source |
|---|---|---|
| `github_com/devopsfaith/krakend/transport/http/server/handler` | `plugin/http-server` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend/transport/http/client/executor` | `plugin/http-client` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend/proxy/plugin` | `plugin/req-resp-modifier` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend/proxy` | `proxy` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github_com/luraproject/lura/router/gin` | `router` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend-httpcache` | `qos/http-cache` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend-circuitbreaker/gobreaker` | `qos/circuit-breaker` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend-oauth2-clientcredentials` | `auth/client-credentials` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend-jose/validator`, `.../signer` | `auth/validator`, `auth/signer` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github_com/devopsfaith/bloomfilter` | `auth/revoker` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github_com/devopsfaith/krakend-botdetector`, `-httpsecure`, `-cors` | `security/bot-detector`, `security/http`, `security/cors` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend-cel`, `-jsonschema` | `validation/cel`, `validation/json-schema` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend-amqp/agent`, `/consume`, `/produce` | `async/amqp`, `backend/amqp/consumer`, `backend/amqp/producer` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend-lambda` | `backend/lambda` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend-pubsub/publisher`, `/subscriber` | `backend/pubsub/publisher`, `backend/pubsub/subscriber` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend/transport/http/client/graphql` | `backend/graphql` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend/http` | `backend/http` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github_com/devopsfaith/krakend-gelf`, `-gologging`, `-logstash`, `-metrics`, `-opencensus` | `telemetry/gelf`, `telemetry/logging`, `telemetry/logstash`, `telemetry/metrics`, `telemetry/opencensus` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github_com/letgoapp/krakend-influx` | `telemetry/influx` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend-lua/router`, `/proxy`, `/proxy/backend` | `modifier/lua-endpoint`, `modifier/lua-proxy`, `modifier/lua-backend` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| `github.com/devopsfaith/krakend-martian` | `modifier/martian` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |

Import implication: the importer should accept both the `github_com/...` and `github.com/...` spellings and normalise them to the v2 names, because both forms appear in the alias map (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go).

## 7. Flexible Configuration (templating)

### 7.1 CE mechanism

| Item | Behaviour | Source |
|---|---|---|
| Env vars | `FC_ENABLE`, `FC_SETTINGS`, `FC_PARTIALS`, `FC_TEMPLATES`, `FC_OUT` (constants in CE `main.go`) | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| Enable | Any non-empty `FC_ENABLE` wraps the koanf parser in `flexibleconfig.NewTemplateParser` | (https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go) |
| Library | `krakend-flexibleconfig/v2` v2.3.1 | (https://github.com/krakend/krakend-ce/blob/master/go.mod) |
| Settings | Only `*.json` files directly in `FC_SETTINGS` are read (no recursion). Each file becomes a top-level variable named after its basename, for example `settings/service.json` becomes `.service` | (https://github.com/krakend/krakend-flexibleconfig/blob/master/template.go) |
| Templates | Only `*.tmpl` files in `FC_TEMPLATES` are loaded as named sub-templates | (https://github.com/krakend/krakend-flexibleconfig/blob/master/template.go) |
| Partials | Inserted verbatim with `{{ include "file" }}` and not evaluated | (https://www.krakend.io/docs/configuration/flexible-config/), (https://www.krakend.io/docs/configuration/templates/) |
| Engine and functions | Go `text/template` with `sprig.GenericFuncMap()` plus custom `marshal` (JSON-encodes a value) and `include`. The docs list Sprig OS functions `env` and `expandenv` | (https://github.com/krakend/krakend-flexibleconfig/blob/master/template.go), (https://www.krakend.io/docs/configuration/templates/) |
| Sub-templates | `{{ template "name.tmpl" <context> }}` | (https://www.krakend.io/docs/configuration/templates/) |
| Output | Rendered to a temp file and then parsed. `FC_OUT` keeps the rendered file | (https://github.com/krakend/krakend-flexibleconfig/blob/master/template.go) |
| Failure mode | If template parsing or execution fails, the parser logs the error and falls back to parsing the raw file with the base parser | (https://github.com/krakend/krakend-flexibleconfig/blob/master/template.go) |
| Documented use | Scaling a per-node limit by cluster size: `{{ env "NUM_PODS" \| div 100 }}` | (https://www.krakend.io/docs/throttling/cluster/) |

### 7.2 EE "Extended Flexible Config"

| Item | Behaviour | Source |
|---|---|---|
| Control file | `flexible_config.json` (name overridable via `FC_CONFIG`). `FC_DEBUG=true` turns on debug output | (https://www.krakend.io/docs/enterprise/configuration/flexible-config/) |
| Keys | `settings.paths`, `settings.allow_overwrite`, `settings.allowed_suffixes`, `settings.dir_field_prefix`, `partials.paths`, `templates.paths`, `templates.undefined_vars` (`error`/`zero`/`invalid`), `meta_key` (default `meta`), `out`, `debug`, `ref_key`, `keys_naming_rules` (`strict`/`freeform`) | (https://www.krakend.io/docs/enterprise/configuration/flexible-config/) |
| `$ref` | JSON-pointer references such as `"$ref": "./backends/websockets.json#/host"` | (https://www.krakend.io/docs/enterprise/configuration/flexible-config/) |
| Settings formats | Recursive scan of the default `allowed_suffixes`: `.yaml`, `.yml`, `.json`, `.toml`, `.tml`, `.ini`, `.dotenv`, `.env`, `.properties`, `.prop`, `.props` | (https://www.krakend.io/docs/enterprise/configuration/flexible-config/) |
| Compatibility | "All your open source edition templates are 100% compatible with the Enterprise counterpart" | (https://www.krakend.io/docs/configuration/flexible-config/) |

## 8. Go plugin types and interfaces

The root `plugin` object (`folder`, `pattern`) selects which `.so` files are loaded (https://www.krakend.io/docs/extending/injecting-plugins/). Plugins are built with `-buildmode=plugin` using the same Go version as KrakenD and checked with `krakend check-plugin` (https://www.krakend.io/docs/extending/http-server-plugins/), (https://www.krakend.io/docs/extending/check-plugin/). CE v2.13 is built with `go 1.26.0` (https://github.com/krakend/krakend-ce/blob/master/go.mod).

| Type | Symbol looked up | Registerer method (exact) | Namespace / edition | Source |
|---|---|---|---|---|
| HTTP server (router layer) | `HandlerRegisterer` | `RegisterHandlers(func(name string, handler func(context.Context, map[string]interface{}, http.Handler) (http.Handler, error)))` | `plugin/http-server`, CE+EE | (https://www.krakend.io/docs/extending/http-server-plugins/), (https://github.com/luraproject/lura/blob/master/transport/http/server/plugin/plugin.go) |
| HTTP client (replaces backend client, cannot be chained) | `ClientRegisterer` | `RegisterClients(func(name string, handler func(context.Context, map[string]interface{}) (http.Handler, error)))` | `plugin/http-client`, CE+EE | (https://www.krakend.io/docs/extending/http-client-plugins/), (https://github.com/luraproject/lura/blob/master/transport/http/client/plugin/plugin.go) |
| Request/response modifier | `ModifierRegisterer` | `RegisterModifiers(func(name string, modifierFactory func(map[string]interface{}) func(interface{}) (interface{}, error), appliesToRequest bool, appliesToResponse bool))` | `plugin/req-resp-modifier`, CE+EE since v2.0 | (https://github.com/luraproject/lura/blob/master/proxy/plugin/modifier.go), (https://www.krakend.io/docs/extending/plugin-modifiers/) |
| Middleware | `MiddlewareRegisterer` | `RegisterMiddlewares(func(string, func(map[string]interface{}, func(context.Context, interface{}) (interface{}, error)) func(context.Context, interface{}) (interface{}, error)))` | `plugin/middleware`, EE v2.10+ | (https://www.krakend.io/docs/enterprise/extending/middleware-plugins/) |

- **Optional interfaces (Lura):** `LoggerRegisterer.RegisterLogger(interface{})` is available on all three CE loaders. `ContextRegisterer.RegisterContext(context.Context)` is available on modifiers (https://github.com/luraproject/lura/blob/master/proxy/plugin/modifier.go).
- **Modifier wrappers:** `RequestWrapper` exposes `Params`, `Headers`, `Body`, `Method`, `URL`, `Query` and `Path`. `ResponseWrapper` exposes `Data`, `Io`, `StatusCode`, `Headers` and `IsComplete` (https://www.krakend.io/docs/extending/plugin-modifiers/).
- **EE 2.13 injection:** EE 2.13 injects Redis and quota processors into all plugin types (https://www.krakend.io/blog/krakend-ee-2.13-release-notes/).
- **Removal from CE:** "Starting with version 3.0, the Community Edition of KrakenD and the Lura Project will no longer support Go plugins", announced 2026-06-04. The suggested alternatives are compiling custom code in or using Lua (https://www.krakend.io/blog/dropping-plugins-support-on-community/). PR #1106 "Drop plugin support" was merged into `dev-3.0` on 2026-09-21 and touched `plugin.go`, `executor.go`, `backend_factory.go` and `main.go` (https://github.com/krakend/krakend-ce/pull/1106).

## 9. Lua hooks

| Namespace | Layer | Hooks | Source |
|---|---|---|---|
| `modifier/lua-endpoint` | Router, before conversion to the internal request | `pre` only. `post` cannot modify the response here | (https://www.krakend.io/docs/endpoints/lua/) |
| `modifier/lua-proxy` | Endpoint proxy, before the split to backends and after the merge | `pre`, `post` | (https://www.krakend.io/docs/endpoints/lua/) |
| `modifier/lua-backend` | Per backend | `pre`, `post`, `skip_next` | (https://www.krakend.io/docs/endpoints/lua/) |

- **Keys:** `sources`, `pre`, `post`, `live` (reload on change), `allow_open_libs` (default false), `skip_next` and `md5` checksums (https://www.krakend.io/schema/v2.13/krakend.json).
- **Helpers:** `ctx` (router), `request`, `response`, `luaTable`, `luaList`, `luaNil`, `http_response.new(url, ...)` and `custom_error(msg, status, content_type)`. The VM is sandboxed with no `require` (https://www.krakend.io/docs/endpoints/lua/).
- **EE extras:** EE adds native helpers for JSON/YAML/XML/CSV, base64, hashing and time (https://www.krakend.io/docs/endpoints/lua/).
- **VM:** `krakend-lua` depends on `yuin/gopher-lua` v1.1.1 (https://github.com/krakend/krakend-lua), which is "a Lua5.1(+ goto statement in Lua5.2) VM" (https://github.com/yuin/gopher-lua).
- **Cost:** Lua scripts "need to be compiled in every execution" and are the #2 memory/CPU consumer in the sizing guide (https://www.krakend.io/docs/deploying/server-dimensioning/).

## 10. EE-specific configuration in detail

### 10.1 Workflows (EE, since v2.7)

- **Placement:** a `workflow` object lives under a backend's `extra_config` (https://www.krakend.io/docs/enterprise/endpoints/workflows/).
- **Fields:** it requires `endpoint` (a log name that is appended to `/__workflow/`) and `backend`, and accepts `concurrent_calls`, `timeout`, `ignore_errors` (default false), `output_encoding` and `extra_config` (https://www.krakend.io/schema/v2.13/krakend.json).
- **Nesting:** "There is no logical limit to nested `workflow` components" (https://www.krakend.io/docs/enterprise/endpoints/workflows/).
- **Allowed namespaces inside `extra_config`:** `modifier/jmespath`, `modifier/lua-proxy`, `modifier/request-body-generator`, `modifier/response-body-generator`, `plugin/req-resp-modifier`, `proxy`, `security/policies`, `validation/cel` and `validation/json-schema` (https://www.krakend.io/schema/v2.13/krakend.json).

### 10.2 Security policies (EE, since v2.2)

- **Structure:** `security/policies` has contexts `req`, `resp` and `jwt`. Each has `policies[]` (CEL strings) and, for req/resp, `error` (`body`, `status` default 500, `content_type` default `text/plain`). Global flags are `auto_join_policies`, `disable_macros` and `debug` (https://www.krakend.io/docs/enterprise/security-policies/).
- **JWT context:** `jwt` policies require `auth/validator` (https://www.krakend.io/schema/v2.13/krakend.json).
- **Advanced macros:** the macros include `hasHeader`, `getHeader`, `hasCookie`, `getCookie`, `hasQuerystring`, `geoIP()`, `between`, `isEmpty`, the string, list, URL and bitwise helpers, `toJSON`/`fromJSON`, `base64`, `sha256`, `hmac` and `uuid()`. They are "not available on the CEL component, only on Security Policies" (https://www.krakend.io/docs/enterprise/security-policies/advanced-policy-macros/).
- **2.13 addition:** EE 2.13 added a `quotaProcessor` macro for falling back when a quota is exhausted (https://www.krakend.io/blog/krakend-ee-2.13-release-notes/).

### 10.3 API keys (EE)

- **Service level:** `keys[]` (`key`, `roles`), `strategy` (`header` default, or `query_string`), `identifier` (default `Authorization`, or `key` for query strings), `hash` (`plain` default, `fnv128`, `sha256`, `sha1`), `salt` and `propagate_role` (https://www.krakend.io/schema/v2.13/krakend.json).
- **Endpoint level:** `roles`, plus optional `client_max_rate` per key (HTTP 429 when exceeded), and `identifier`/`strategy` overrides (https://www.krakend.io/docs/enterprise/authentication/api-keys/).
- **Key storage:** keys live inline in the config, either plain or hashed (https://www.krakend.io/docs/enterprise/authentication/api-keys/).

### 10.4 Tiered rate limits (EE, since v2.1)

- **Structure:** `qos/ratelimit/tiered` requires `tier_key` (a header name, case-insensitive) and `tiers[]`. Each tier has `tier_value`, `tier_value_as` (`literal` default, `policy`, or `*` catch-all, which must be last), `ratelimit` (stateless) and/or `ratelimit_redis` (stateful, `connection_name` pointing at the `redis` namespace) (https://www.krakend.io/docs/enterprise/service-settings/tiered-rate-limit/).
- **Matching:** the first matching tier wins (https://www.krakend.io/docs/enterprise/service-settings/tiered-rate-limit/).

### 10.5 AI gateway (EE, since v2.10)

| Concern | How it is configured | Source |
|---|---|---|
| Providers | `ai/llm` at backend scope with one vendor key: `openai`, `anthropic`, `gemini`, `mistral`, `bedrock` (Bedrock added in 2.13) | (https://www.krakend.io/schema/v2.13/krakend.json), (https://www.krakend.io/blog/krakend-ee-2.13-release-notes/) |
| Per-vendor fields | Versioned block (for example `v1`) with `credentials`, `debug`, `input_template`, `output_template` and `variables` | (https://www.krakend.io/docs/enterprise/ai-gateway/openai/) |
| OpenAI `variables` | `model`, `max_output_tokens`, `temperature` (0 to 2), `top_p`, `truncation` (`auto`/`disabled`, default `disabled`), `extra_payload` | (https://www.krakend.io/schema/v2.13/krakend.json) |
| Prompt templates | `input_template` and `output_template` are paths to Go `text/template` files that shape the vendor payload and response | (https://www.krakend.io/schema/v2.13/krakend.json) |
| Routing | No routing object in `ai/llm`. Routing is built from `backend/conditional` (`header`, `policy`, `fallback`), `auth/validator` `propagate_claims`, path parameters, or separate endpoints | (https://www.krakend.io/docs/enterprise/ai-gateway/llm-routing/) |
| Token quotas | `governance/processors` (Redis) plus `governance/quota` with `weight_key` and `weight_strategy` (`body`/`header`) to charge variable units | (https://www.krakend.io/docs/enterprise/ai-gateway/budget-control/) |
| MCP | `ai/mcp.servers[]` (`name`, `tools[]` with `input_schema`/`output_schema`/`workflow`, `stateless`, `json_response`, `ping_period`) | (https://www.krakend.io/docs/enterprise/ai-gateway/mcp-server/) |

## 11. Scaling model and published numbers

### 11.1 Statelessness and state

- **Nodes:** "KrakenD nodes are stateless and they don't store data or application state to a persistent storage." A cluster needs only a load balancer and "two or more KrakenD services with the same configuration file" (https://www.krakend.io/docs/deploying/clustering/).
- **Per-node limits:** stateless rate limits apply "individually to each running instance". The docs give the example that 3 nodes with `max_rate=100` allow 300 req/s in total (https://www.krakend.io/docs/throttling/cluster/).
- **Redis-backed limits:** EE stateful limits use Redis, with `qos/ratelimit/service/redis`, `qos/ratelimit/router/redis`, `ratelimit_redis` in tiers, and quotas. With Redis, limits stay "invariable if you add or remove nodes on the fly". `on_failure_allow` defaults to `false`, which blocks requests when Redis fails (https://www.krakend.io/docs/enterprise/throttling/global-rate-limit/).
- **Algorithm:** the Redis rate limit uses a token bucket (`capacity`, `max_rate`, `every` default `1s`) (https://www.krakend.io/schema/v2.13/krakend.json).
- **Deprecated plugin:** before EE v2.8 this feature was a Redis plugin, which is now deprecated (https://www.krakend.io/docs/enterprise/throttling/global-rate-limit/).
- **Token revocation:** the CE bloom filter does not synchronise. "Every node must receive the RPC notification" (https://www.krakend.io/docs/authorization/revoking-tokens/).

### 11.2 Recommended deployment

- **High availability:** use at least two instances (https://www.krakend.io/docs/deploying/).
- **Immutable images:** build an immutable Docker image with the configuration baked in during CI/CD (https://www.krakend.io/docs/deploying/).
- **Sizing:** prefer "more small machines rather than one or two big ones". Network bandwidth is "the first limit that KrakenD hits on public-cloud instances" (https://www.krakend.io/docs/deploying/server-dimensioning/).

| Sizing tier (per node, high throughput) | vCPU / RAM | Source |
|---|---|---|
| Baseline (routing, encoding, filters, circuit breaker, no logging or JWT) | 0.5 CPU / 512 MB | (https://www.krakend.io/docs/deploying/server-dimensioning/) |
| Baseline + light JWT crypto + moderate logging | 1 CPU / 512 MB | (https://www.krakend.io/docs/deploying/server-dimensioning/) |
| All-purpose | 2 vCPU / 2 GB | (https://www.krakend.io/docs/deploying/server-dimensioning/) |
| Massive traffic + heavy crypto/transforms | 4 vCPU / 4 GB | (https://www.krakend.io/docs/deploying/server-dimensioning/) |

- **Baseline footprint:** the baseline uses about 100-200 MB of RAM (https://www.krakend.io/docs/deploying/server-dimensioning/).
- **Logging cost:** DEBUG logging versus no logging makes an "x2 and x5" throughput difference (https://www.krakend.io/docs/deploying/server-dimensioning/).

### 11.3 Published benchmarks

| Hardware | Req/s | Avg latency | Source |
|---|---|---|---|
| AWS EC2 c4.2xlarge (8 vCPU, 15 GB) | 10,126.16 | 9.8 ms | (https://www.krakend.io/docs/benchmarks/) |
| AWS EC2 c4.xlarge (4 vCPU, 7.5 GB) | 8,465.40 | 11.7 ms | (https://www.krakend.io/docs/benchmarks/) |
| AWS EC2 m4.large (2 vCPU, 8 GB) | 3,634.12 | 27.3 ms | (https://www.krakend.io/docs/benchmarks/) |
| AWS EC2 t2.medium (2 vCPU, 4 GB) | 2,781.86 | 351.3 ms | (https://www.krakend.io/docs/benchmarks/) |
| AWS EC2 t2.micro (1 vCPU, 1 GB) | 2,757.64 | 35.8 ms | (https://www.krakend.io/docs/benchmarks/) |
| MacBook Pro (Aug 2015), 2.2 GHz Intel Core i7 | 18,157.43 | 5.5 ms | (https://www.krakend.io/docs/benchmarks/) |

- **AWS setup:** the backend was an LWAN web server on c4.xlarge and the load generator was `hey` on t2.medium (https://www.krakend.io/docs/benchmarks/aws/).
- **Laptop run:** a separate page shows 17,549.88 req/s (https://www.krakend.io/docs/benchmarks/local/).
- **varnish benchmark:** on the varnish api-gateway-benchmarks suite, the requests/s figures range from 7,534.55 (test00) down to 451.30 (https://www.krakend.io/docs/benchmarks/api-gateway-benchmark/).
- **Config vintage:** the benchmark configs use `"version": 1`, which is the pre-2.0 format (https://www.krakend.io/docs/benchmarks/local/). They are therefore not measurements of v2.x.

## Sources

https://www.krakend.io/schema/v2.13/krakend.json
https://www.krakend.io/docs/configuration/structure/
https://www.krakend.io/docs/configuration/supported-formats/
https://www.krakend.io/docs/configuration/environment-vars/
https://www.krakend.io/docs/configuration/flexible-config/
https://www.krakend.io/docs/configuration/templates/
https://www.krakend.io/docs/configuration/migrating/
https://www.krakend.io/docs/enterprise/configuration/flexible-config/
https://www.krakend.io/docs/endpoints/
https://www.krakend.io/docs/backends/
https://www.krakend.io/docs/backends/data-manipulation/
https://www.krakend.io/docs/backends/http-client/
https://www.krakend.io/docs/backends/detailed-errors/
https://www.krakend.io/docs/telemetry/opencensus/
https://www.krakend.io/docs/telemetry/opentelemetry-by-endpoint/
https://www.krakend.io/docs/endpoints/lua/
https://www.krakend.io/docs/extending/injecting-plugins/
https://www.krakend.io/docs/extending/http-server-plugins/
https://www.krakend.io/docs/extending/check-plugin/
https://www.krakend.io/docs/extending/http-client-plugins/
https://www.krakend.io/docs/extending/plugin-modifiers/
https://www.krakend.io/docs/enterprise/extending/middleware-plugins/
https://www.krakend.io/docs/enterprise/endpoints/workflows/
https://www.krakend.io/docs/enterprise/endpoints/streaming/
https://www.krakend.io/docs/enterprise/security-policies/
https://www.krakend.io/docs/enterprise/security-policies/advanced-policy-macros/
https://www.krakend.io/docs/enterprise/authentication/api-keys/
https://www.krakend.io/docs/enterprise/service-settings/tiered-rate-limit/
https://www.krakend.io/docs/enterprise/throttling/global-rate-limit/
https://www.krakend.io/docs/enterprise/ai-gateway/unified-llm-interface/
https://www.krakend.io/docs/enterprise/ai-gateway/openai/
https://www.krakend.io/docs/enterprise/ai-gateway/llm-routing/
https://www.krakend.io/docs/enterprise/ai-gateway/budget-control/
https://www.krakend.io/docs/enterprise/ai-gateway/mcp-server/
https://www.krakend.io/docs/authorization/revoking-tokens/
https://www.krakend.io/docs/deploying/
https://www.krakend.io/docs/deploying/clustering/
https://www.krakend.io/docs/deploying/server-dimensioning/
https://www.krakend.io/docs/developer/hot-reload/
https://www.krakend.io/docs/throttling/cluster/
https://www.krakend.io/docs/benchmarks/
https://www.krakend.io/docs/benchmarks/local/
https://www.krakend.io/docs/benchmarks/aws/
https://www.krakend.io/docs/benchmarks/api-gateway-benchmark/
https://www.krakend.io/features/
https://www.krakend.io/blog/krakend-ee-2.13-release-notes/
https://www.krakend.io/blog/dropping-plugins-support-on-community/
https://github.com/krakend/krakend-ce/releases
https://github.com/krakend/krakend-ce/pull/1106
https://github.com/krakend/krakend-ce/blob/master/cmd/krakend-ce/main.go
https://github.com/krakend/krakend-ce/blob/master/go.mod
https://github.com/krakend/krakend-koanf
https://github.com/krakend/krakend-flexibleconfig/blob/master/template.go
https://github.com/krakend/krakend-lua
https://github.com/krakend/krakend-documentation
https://github.com/luraproject/lura/blob/master/proxy/plugin/modifier.go
https://github.com/luraproject/lura/blob/master/transport/http/server/plugin/plugin.go
https://github.com/luraproject/lura/blob/master/transport/http/client/plugin/plugin.go
https://github.com/yuin/gopher-lua

## Gaps

- **Edition markings are inconsistent.** Several namespaces lack the "Enterprise only" prefix in the schema although their docs live under `/docs/enterprise/`: `ai/llm`, `backend/conditional`, `governance/processors`, `redis`, `telemetry/moesif`, `telemetry/newrelic`. In the other direction, `backend/http/client` is marked "Enterprise only" in the schema but has a CE doc page ("since v2.11"), and the `telemetry/opentelemetry` endpoint/backend override objects are marked "Enterprise only" in the schema while the CE docs and features page list them as Community. My edition column combines the schema text with the features page. It is not a verified per-key CE/EE matrix.
- **The EE docs are not in the public docs repo.** `krakend/krakend-documentation` contains only CE pages. EE details come from single web fetches summarised by a tool, and field lists were cross-checked against the schema where possible. The EE middleware interface signature was taken from the rendered doc page, not from source code.
- **Supported file formats conflict.** The supported-formats doc lists `.properties` and `.hcl`. CE v2.13 uses `krakend-koanf`, whose README mentions only json/yaml/toml, although `hashicorp/hcl` is an indirect dependency. I did not test whether HCL or properties files still parse.
- **`is_collection` default.** The schema shows `"default": true`, while the description ("Set to true when your API ... returns a collection") implies opt-in. I did not confirm the runtime default.
- **`security/policies` scope.** The schema allows it at endpoint, backend and workflow scope. The fetched doc page summary mentioned only endpoint and backend.
- **AI gateway.** I could not find documented token-count extraction for quotas (which response field feeds `weight_key` per provider) or provider failover semantics beyond `backend/conditional` `fallback`. The schema does define `variables` for Gemini (under `v1beta`), Mistral and Bedrock, but only the OpenAI fields are summarised here.
- **Benchmarks are old.** All published benchmark pages use config `"version": 1` (KrakenD 0.x/1.x era), and the AWS instance types are c4/m4/t2. There are no published v2.x or EE benchmark numbers and no p99 figures. Throughput for Redis-backed rate limiting is not published.
- **CE 3.0 is unreleased.** CE 3.0 had no release tag at snapshot. What else changes in the config model on the `dev-3.0` branch beyond plugin removal was not investigated.
- **Duplicate alias entries.** CE `main.go` maps `github_com/...` and `github.com/...` spellings inconsistently across namespaces. I did not verify whether both spellings are accepted for every namespace at runtime.
- **No pricing numbers.** The pricing page returned HTTP 404 at `https://www.krakend.io/pricing/`, so I captured no EE licensing or node-count terms.
