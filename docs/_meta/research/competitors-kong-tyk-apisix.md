# Research: Kong Gateway, Tyk and Apache APISIX (+ API7) competitor snapshot

- Topic: Feature, tier and licensing comparison of Kong Gateway (OSS / Enterprise / Konnect), Tyk (OSS Gateway, Dashboard, Streams, AI Studio, MCP Gateway) and Apache APISIX (+ API7 Gateway / AISIX), against the four Ruralz differentiators (WASM plugins, AI/LLM gateway, built-in control plane + GitOps, multi-protocol native).
- Snapshot date: 2026-09-23
- Method: Vendor documentation, release notes, changelogs, GitHub READMEs/releases and pricing pages fetched on 2026-09-23 (WebSearch + WebFetch). Every claim carries its source URL on the same line. Where two first-party pages disagree, both values are given and the conflict is listed under Gaps. Nothing in this file was measured by Ruralz; all performance numbers are vendor-published.

## 1. Current versions and release dates

| Product | Latest version | Release date | Notes | Source |
|---|---|---|---|---|
| Kong Gateway Enterprise | 3.16.0.0 | 2026-09-15 | Supported until 2027-09-15; 3.14 (2026-04-07) and 3.10 (2025-03-27) are LTS, 3 years each | https://developer.konghq.com/gateway/version-support-policy/ |
| Kong Gateway 3.16 announcement | 3.16 | blog dated 2026-09-22 | Runtime log levels, CEL dynamic plugin config, Entitlement Enforcement, FIPS 140-3 | https://konghq.com/blog/product-releases/kong-api-gateway-3-16 |
| Kong Gateway OSS (Kong/kong) | 3.9.3 | not stated on page | No OSS release after the 3.9.x line; CHANGELOG tops out at 3.9.3 | https://github.com/Kong/kong/releases · https://raw.githubusercontent.com/Kong/kong/master/CHANGELOG.md |
| Kong AI Gateway 2.0 | 2.0 | GA 2026-09-01 (private beta July 2026) | Separate product with its own runtime/control plane and release train | https://konghq.com/blog/product-releases/kong-ai-gateway-2-0-ga |
| decK | v1.66.1 | 2026-09-08 (year inferred, see Gaps) | v1.66.0 added "support new AI Gateway 2.0 features" | https://github.com/Kong/deck/releases |
| Tyk Gateway | 5.15.0 | 2026-09-01 (release-notes page) / 2026-07-07 (overview page) | 5.14.0 2026-07-07; 5.13.0 2026-05-19; 5.13.2 2026-08-26 | https://tyk.io/docs/developer-support/release-notes/gateway · https://tyk.io/docs/developer-support/release-notes/overview |
| Tyk Gateway / Dashboard LTS | 5.13.2 LTS | 2026-08-26 | Only the latest release and the current LTS branch are supported | https://tyk.io/docs/developer-support/release-notes/overview · https://tyk.io/docs/developer-support/release-notes/gateway |
| Tyk Operator / Tyk Sync / MDCB | 1.4.2 / 2.2.2 / 2.13.0 | 2026-05-21 / 2026-05-21 / 2026-05-19 | Operator and Sync closed source since Oct 2024 | https://tyk.io/docs/developer-support/release-notes/overview |
| Tyk AI Studio | 2.1.0 | 2026-05-14 | AGPL-3.0 Community Edition + Enterprise Edition | https://tyk.io/docs/developer-support/release-notes/overview · https://github.com/TykTechnologies/ai-studio |
| Apache APISIX | 3.18.0 | 2026-08-20 | 3.17.0 2026-06-15; 3.16.0 2026-04-07; 3.15.0 2026-02-05 | https://apisix.apache.org/blog/archive/ · https://apisix.apache.org/blog/2026/08/20/release-apache-apisix-3.18.0/ |
| API7 Gateway (commercial APISIX distribution) | 3.10.4 | 2026-07-27 | LTS lines: 3.10 (2026-06-01 to 2029-05-31), 3.9 (2026-01-06 to 2029-01-05), 3.8 (2025-04-22 to 2028-04-21) | https://api7.ai/blog/api7-gateway-3-10-4-runtime-reliability · https://docs.api7.ai/api7-gateway/version-support-policy |
| ADC (API7 declarative CLI) | v0.30.5 | 2026-09-16 | Targets APISIX, APISIX standalone and API7 Enterprise | https://github.com/api7/adc/releases |
| AISIX (API7) | no tagged version on README | n/a | Rust AI gateway, Apache-2.0 | https://github.com/api7/aisix |

## 2. Licensing and free-tier changes 2024-2026

| Date | Vendor | Change | Source |
|---|---|---|---|
| 2024-10 (repo archived 2024-10-11) | Tyk | Tyk Operator becomes closed source, "only available to paying customers under EULA license"; open code remains on the `legacy` branch | https://github.com/TykTechnologies/tyk-operator |
| 2024-10 | Tyk | Operator v1.0+ requires `TYK_OPERATOR_LICENSEKEY` and exits with an error if it is missing or invalid | https://tyk.io/docs/5.6/product-stack/tyk-operator/release-notes/operator-1.0/ · https://tyk.io/docs/tyk-stack/tyk-operator/installing-tyk-operator |
| 2024-10 | Tyk | Tyk Sync latest release closed source under EULA; repository archived (previously MPL-2.0) | https://github.com/TykTechnologies/tyk-sync |
| ongoing | Tyk | Tyk Gateway repo is dual-licensed: MPL-2.0 for root directories, commercial license for the `ee` folder | https://github.com/TykTechnologies/tyk |
| 3.10 (2025-03-27) | Kong | "Free mode is no longer available. Running Kong Gateway without a license will now behave the same as running it with an expired license." (release-note text quoted in discussion) | https://github.com/Kong/kong/discussions/14628 |
| 3.10 onward | Kong | Community reply (not Kong staff) dated 2025-12-15: stay on OSS images such as `kong:3.9.1` to remain free; `kong/kong-gateway` 3.10+ needs a valid license | https://github.com/Kong/kong/discussions/14628 |
| 3.10 onward | Kong | Kong/kong GitHub releases stop at 3.9.x (3.9.3 latest) | https://github.com/Kong/kong/releases |
| 3.15 (2026-07-02) | Kong | Expired license: Admin API and all interfaces become read-only; traffic continues; `/licenses` stays reachable | https://developer.konghq.com/gateway/breaking-changes/ |
| 3.18 (announced, not released) | Kong | "Kong Gateway 3.18 remains the release where AI plugins become opt-in rather than bundled"; AI plugins stay supported in 3.14 LTS | https://konghq.com/blog/product-releases/kong-ai-gateway-2-0-ga |
| 2026-03-12 | Tyk | AI Studio open-sourced (announcement); repository license AGPL-3.0 | https://tyk.io/blog/ai-studio-is-going-open-source-and-why-the-ai-control-plane-must-be-extensible/ · https://github.com/TykTechnologies/ai-studio |
| unchanged | Apache APISIX | Apache-2.0 for the gateway and the dashboard | https://github.com/apache/apisix · https://github.com/apache/apisix-dashboard |
| unchanged | API7 | ADC and AISIX are Apache-2.0; AISIX spend caps only in commercial AISIX Cloud | https://github.com/api7/adc · https://github.com/api7/aisix |

## 3. Extensibility model and proxy-wasm status

| Vendor | Native plugin language | Other languages | proxy-wasm / WASM status | Source |
|---|---|---|---|---|
| Kong | Lua (PDK) | Go (Go PDK), Python (`kong-python-pdk`), JavaScript (JS PDK) | Beta WASM (proxy-wasm filters) introduced in 3.4; "Support for the beta WASM module has been removed" in 3.11.0.0; Datakit re-bundled as a Lua plugin | https://developer.konghq.com/custom-plugins/ · https://konghq.com/blog/product-releases/webassembly-in-kong-gateway-3-4 · https://developer.konghq.com/gateway/breaking-changes/ |
| Kong | 3.16 adds a Metrics PDK (`kong.metrics`) for custom plugins | — | — | https://developer.konghq.com/gateway/changelog/ |
| Tyk | Go native plugins ("the recommended plugin type") | JavaScript JSVM (Goja engine recommended since 5.15.0, `otto` legacy), Python and Lua rich plugins, gRPC (any language) | No WASM in plugin docs | https://tyk.io/docs/api-management/plugins/overview |
| Tyk | Hooks: Go/JS/gRPC/Python cover auth, pre, post-auth, post, response; Lua lacks the response hook | — | — | https://tyk.io/docs/api-management/plugins/overview |
| Tyk | 5.15.0 adds a next-generation plugin compiler with native arm64 | — | — | https://tyk.io/docs/developer-support/release-notes/gateway |
| APISIX | Lua | Java, Go, Python, Node.js via RPC plugin runners | Proxy-wasm via `wasm-nginx-module`, experimental; "only a few APIs are implemented"; callbacks `proxy_on_configure` and HTTP request/response headers and body; Go examples | https://github.com/apache/apisix · https://apisix.apache.org/docs/apisix/wasm/ |
| API7 Gateway | Custom Lua plugins managed per gateway group | — | not stated | https://docs.api7.ai/api7-gateway/enterprise-features/overview |
| Tyk AI Studio | Plugins as isolated processes over gRPC; "Unified Plugin SDK" | — | not stated | https://github.com/TykTechnologies/ai-studio · https://tyk.io/docs/ai-management/ai-studio/core-concepts |

Observation: at the snapshot none of the three ships a GA WASM plugin system. Kong removed its beta (https://developer.konghq.com/gateway/breaking-changes/), APISIX's is experimental with partial ABI coverage (https://apisix.apache.org/docs/apisix/wasm/), and Tyk documents none (https://tyk.io/docs/api-management/plugins/overview).

## 4. AI gateway features and tiers

### 4.1 Kong

| Capability | Kong implementation | Tier | Min version | Source |
|---|---|---|---|---|
| Multi-provider proxy (OpenAI, Azure OpenAI, Bedrock, Anthropic, Gemini, Vertex AI, Cohere, Mistral, Ollama, vLLM and others) | AI Proxy | Code in OSS repo (`kong/plugins/ai-proxy`) | 3.6 | https://developer.konghq.com/plugins/ai-proxy/ · https://github.com/Kong/kong/tree/master/kong/plugins |
| OSS AI plugin set | ai-proxy, ai-prompt-decorator, ai-prompt-guard, ai-prompt-template, ai-request-transformer, ai-response-transformer | OSS | — | https://github.com/Kong/kong/tree/master/kong/plugins |
| Load balancing + fallback | AI Proxy Advanced: round-robin, consistent-hashing, least-connections, lowest-latency (EWMA), lowest-usage (tokens/cost), semantic, priority; cross-format fallback since 3.10 | AI Gateway Enterprise | 3.8 | https://developer.konghq.com/plugins/ai-proxy-advanced/ |
| Token-based rate limiting | AI Rate Limiting Advanced: `total_tokens`, `prompt_tokens`, `completion_tokens`, `cost` (3.8+); local/cluster/redis; fixed or sliding window; policy-based limits by consumer, model, provider (3.14+) | AI Gateway Enterprise | 3.7 | https://developer.konghq.com/plugins/ai-rate-limiting-advanced/ |
| Cost enforcement lag | "The cost for the AI Proxy or AI Proxy Advanced is only reflected during the next request" | — | — | https://developer.konghq.com/plugins/ai-rate-limiting-advanced/ |
| Semantic cache | AI Semantic Cache: Redis vector search, Valkey (3.14+), PostgreSQL pgvector (3.10+); streams cached responses | AI Gateway Enterprise | 3.8 | https://developer.konghq.com/plugins/ai-semantic-cache/ |
| Guardrails | AI AWS Guardrails, Azure Content Safety, GCP Model Armor, Lakera Guard, Custom Guardrail, PII Sanitizer, Semantic Prompt Guard, Semantic Response Guard, LLM as Judge | Tier not shown on index | varies | https://developer.konghq.com/plugins/?category=ai |
| MCP | AI MCP Proxy modes passthrough-listener, conversion-listener, conversion-only, listener (aggregation); HTTP/HTTPS upstreams only; plus AI MCP OAuth2 | AI Gateway Enterprise | 3.12 | https://developer.konghq.com/plugins/ai-mcp-proxy/ · https://developer.konghq.com/plugins/?category=ai |
| A2A | AI A2A Proxy | Tier not shown on index | — | https://developer.konghq.com/plugins/?category=ai |
| Budgets | "Scope model access and token budgets by team or department"; "Enforce per-tier spend caps driven by identity claims" | AI Gateway | — | https://developer.konghq.com/ai-gateway/ |
| AI Gateway 2.0 | Entity model (AI Model Provider, AI Model) replacing plugin config; MCP server bundling with authorization-aware tool discovery; modality-aware pricing; available in Konnect | Konnect | GA 2026-09-01 | https://konghq.com/blog/product-releases/kong-ai-gateway-2-0-ga · https://developer.konghq.com/ai-gateway/ |
| Konnect AI pricing | Plus: 5 unique LLM models included, $100/month per extra model; Enterprise: unlimited, custom | Konnect | — | https://konghq.com/pricing |

### 4.2 Tyk

| Capability | Tyk implementation | Tier | Source |
|---|---|---|---|
| MCP proxy for remote MCP servers | MCP Gateway (Gateway 5.13+), Streamable HTTP (`POST /mcp`, `GET /mcp` SSE); no stdio | All Tyk licenses | https://tyk.io/docs/ai-management/mcp-gateway/overview |
| MCP access control | Filtered `tools/list` per consumer; per tool/resource/prompt permissions; rate limits per API, per key, per JSON-RPC method, per primitive; OAuth 2.1 Protected Resource Metadata | Proxy features all licenses | https://tyk.io/docs/ai-management/mcp-gateway/overview |
| REST-to-MCP conversion | Turn a Tyk OAS API into an MCP proxy (5.15) | Enterprise Edition | https://tyk.io/docs/ai-management/mcp-gateway/overview · https://tyk.io/docs/developer-support/release-notes/gateway |
| MCP registry, upstream OAuth | Registry lives in the Tyk Dashboard; upstream OAuth and token exchange require Enterprise Edition | Dashboard (registry); Enterprise Edition (upstream OAuth, token exchange) | https://tyk.io/docs/ai-management/mcp-gateway/overview |
| LLM gateway | AI Studio LLM proxy (OpenAI, Anthropic, Mistral, Vertex/Gemini, Bedrock, Hugging Face, Ollama) | Community (AGPL-3.0) and Enterprise | https://github.com/TykTechnologies/ai-studio |
| Budgets | Budget management, enforcement, alerts | Enterprise per README; listed without an edition marker on core-concepts page, which marks Model Router as Enterprise (unclear) | https://github.com/TykTechnologies/ai-studio · https://tyk.io/docs/ai-management/ai-studio/core-concepts |
| Model routing / fallback | Model Router with load balancing and failover; request-level failover "waterfall" in the core proxy (PR merged 2026-09-11), reached by the unified router, the Enterprise Model Router and the microgateway | Model Router Enterprise; waterfall edition not stated | https://tyk.io/docs/ai-management/ai-studio/core-concepts · https://github.com/TykTechnologies/ai-studio/pull/564 |
| Guardrails | Filters as scripts or provider-backed guardrails across proxy, chat and tool scopes | Community and Enterprise | https://github.com/TykTechnologies/ai-studio/pull/580 · https://tyk.io/docs/ai-management/ai-studio/core-concepts |
| Rate limiting / cost tracking | Cost Tracking & Analytics ticked for both editions; rate limiting listed as a core AI Gateway capability with no edition marker | Cost tracking Community and Enterprise; rate limiting not edition-marked | https://github.com/TykTechnologies/ai-studio · https://tyk.io/docs/ai-management/ai-studio/overview |
| Semantic cache | Not documented as a distinct AI Studio feature | — | https://github.com/TykTechnologies/ai-studio |

### 4.3 Apache APISIX / API7

| Capability | Implementation | Tier | Source |
|---|---|---|---|
| Multi-provider proxy | ai-proxy / ai-proxy-multi: OpenAI, DeepSeek, Azure OpenAI, AIMLAPI, Anthropic, OpenRouter, Gemini, Vertex AI, Bedrock, openai-compatible; Bedrock/Anthropic Messages added in 3.17 | Free (Apache-2.0) | https://apisix.apache.org/docs/apisix/plugins/ai-proxy-multi/ · https://raw.githubusercontent.com/apache/apisix/master/CHANGELOG.md |
| Load balancing + fallback | roundrobin (weighted), chash, semantic (3.18); `fallback_strategy` values `rate_limiting`, `http_429`, `http_5xx`; priority + weight; active health checks | Free | https://apisix.apache.org/docs/apisix/plugins/ai-proxy-multi/ · https://apisix.apache.org/blog/2026/08/20/release-apache-apisix-3.18.0/ |
| Token rate limiting | ai-rate-limiting: `total_tokens`, `prompt_tokens`, `completion_tokens`, `expression`; per-instance limits; policies local, redis, redis-cluster, redis-sentinel; default reject 503 | Free | https://apisix.apache.org/docs/apisix/plugins/ai-rate-limiting/ |
| Distributed token counters | Redis-backed distributed counters for ai-rate-limiting (3.18) | Free | https://apisix.apache.org/blog/2026/08/20/release-apache-apisix-3.18.0/ |
| Semantic cache | ai-cache (3.18): L1 exact (SHA-256 key), L2 semantic via Redis Stack/RediSearch, OpenAI or Azure OpenAI embeddings, default threshold 0.95, SSE replay | Free | https://apisix.apache.org/docs/apisix/next/plugins/ai-cache/ · https://apisix.apache.org/blog/2026/08/20/release-apache-apisix-3.18.0/ |
| Guardrails | ai-prompt-guard; ai-lakera-guard (3.18); ai-aws-content-moderation and ai-aliyun-content-moderation extended to responses, default `fail_mode: skip` | Free | https://apisix.apache.org/blog/2026/08/20/release-apache-apisix-3.18.0/ · https://raw.githubusercontent.com/apache/apisix/master/CHANGELOG.md |
| AI metrics | Prometheus AI cache metrics, embedding latency histograms, token distributions, TTFT; `apisix_llm_latency` gains a `type` label (breaking) | Free | https://apisix.apache.org/blog/2026/08/20/release-apache-apisix-3.18.0/ |
| MCP | mcp-bridge (stdio MCP server to HTTP SSE) "is deprecated and will be removed in a future release" | Free | https://apisix.apache.org/docs/apisix/plugins/mcp-bridge/ |
| Budgets | No budget plugin found in the changelog | — | https://raw.githubusercontent.com/apache/apisix/master/CHANGELOG.md |
| AISIX (API7, Rust) | Six routing strategies, RPS/RPM/RPH/RPD + TPM/TPD rate and token limits plus concurrency caps (no budgets in OSS), semantic routing, guardrails (Lakera, Presidio, Azure Content Safety), exact-match memory/Redis cache, MCP and A2A gateways; budgets and spend caps only in AISIX Cloud | OSS Apache-2.0; budgets and spend caps commercial | https://github.com/api7/aisix |
| API7 Cloud AI | Load balancing, retry/fallback, token rate limiting listed under "AI Gateway" | Commercial | https://api7.ai/pricing |

### 4.4 Cross-vendor AI tier summary

| Capability | Kong | Tyk | APISIX |
|---|---|---|---|
| Token-based rate limiting | Enterprise (https://developer.konghq.com/plugins/ai-rate-limiting-advanced/) | AI Studio core capability, no edition marker (https://github.com/TykTechnologies/ai-studio) | Free (https://apisix.apache.org/docs/apisix/plugins/ai-rate-limiting/) |
| Semantic caching | Enterprise (https://developer.konghq.com/plugins/ai-semantic-cache/) | Not documented (https://github.com/TykTechnologies/ai-studio) | Free since 3.18 (https://apisix.apache.org/docs/apisix/next/plugins/ai-cache/) |
| Provider fallback / LB | Enterprise (https://developer.konghq.com/plugins/ai-proxy-advanced/) | Model Router Enterprise (https://tyk.io/docs/ai-management/ai-studio/core-concepts); core-proxy failover waterfall (https://github.com/TykTechnologies/ai-studio/pull/564) | Free (https://apisix.apache.org/docs/apisix/plugins/ai-proxy-multi/) |
| Budgets | AI Gateway / Konnect (https://developer.konghq.com/ai-gateway/) | Enterprise per README (https://github.com/TykTechnologies/ai-studio) | Not in APISIX; AISIX OSS has TPM/TPD token limits only, budgets and spend caps paid (https://github.com/api7/aisix) |
| Guardrails | Many plugins, tier per plugin (https://developer.konghq.com/plugins/?category=ai) | Both editions (https://tyk.io/docs/ai-management/ai-studio/core-concepts) | Free (https://apisix.apache.org/blog/2026/08/20/release-apache-apisix-3.18.0/) |
| MCP | Enterprise, 3.12+ (https://developer.konghq.com/plugins/ai-mcp-proxy/) | Proxy all licenses; conversion Enterprise (https://tyk.io/docs/ai-management/mcp-gateway/overview) | Deprecated stdio bridge only (https://apisix.apache.org/docs/apisix/plugins/mcp-bridge/) |

## 5. Control plane and GitOps

| Vendor | Component | Status / license | Source |
|---|---|---|---|
| Kong | decK declarative config CLI; v1.66.0 supports AI Gateway 2.0 entities | Open source on GitHub | https://github.com/Kong/deck/releases |
| Kong | Konnect SaaS control plane. Plus: up to 5 Serverless, 2 Hybrid, 2 Dedicated Cloud gateways; 1M requests/month included, $200 per extra 1M, 10M/month cap. Enterprise: custom | Commercial | https://konghq.com/pricing |
| Kong | Hybrid data planes accept runtime log-level changes with TTL (3.16) | Enterprise | https://konghq.com/blog/product-releases/kong-api-gateway-3-16 |
| Kong | Rate-limit `cluster` strategy unavailable in hybrid/Konnect | — | https://developer.konghq.com/plugins/rate-limiting-advanced/ |
| Tyk | Dashboard (Management Control Plane GUI) and Developer Portal are the commercial Self Managed/Cloud platform; paid Core/Professional/Enterprise plans list the portal, SSO, RBAC, and multi-region deployments (Enterprise) | Commercial | https://tyk.io/pricing/ · https://github.com/TykTechnologies/tyk |
| Tyk | Tyk Operator (Kubernetes) closed source, license key required | Commercial | https://github.com/TykTechnologies/tyk-operator · https://tyk.io/docs/5.6/product-stack/tyk-operator/release-notes/operator-1.0/ |
| Tyk | Tyk Sync (config promotion between environments) closed source since Oct 2024 | Commercial | https://github.com/TykTechnologies/tyk-sync |
| APISIX | Embedded dashboard at `:9180/ui/`, `deployment.admin.enable_admin_ui: true` by default; last legacy standalone release 3.0.1; embedded since 3.13 | Apache-2.0 | https://apisix.apache.org/docs/apisix/dashboard/ · https://raw.githubusercontent.com/apache/apisix/master/CHANGELOG.md |
| APISIX | apisix-dashboard repo "will not be released independently but will use a fixed git tag for each APISIX release" | Apache-2.0 | https://github.com/apache/apisix-dashboard |
| APISIX | Standalone mode status/validate APIs (3.15) and rejection of configs with unknown plugins (3.16) | Apache-2.0 | https://raw.githubusercontent.com/apache/apisix/master/CHANGELOG.md |
| API7 | ADC commands `sync`, `diff`, `dump`, `lint`, `convert openapi` against APISIX and API7 Enterprise | Apache-2.0 | https://github.com/api7/adc |
| API7 | API7 Gateway: multi-cluster control plane (Gateway Groups), SSO (OIDC, SAML, LDAP, CAS), immutable audit log, developer portal, config fallback to object storage during control-plane outage, FIPS 140-2 Level 1 | Commercial, licensed per CPU core | https://docs.api7.ai/api7-gateway/enterprise-features/overview · https://api7.ai/pricing |
| API7 | API7 Cloud Standard: $2 per million calls, $250 per gateway group/month, $10 per service/month, 99.95% control-plane SLA | Commercial | https://api7.ai/pricing |
| APISIX OSS | Static admin API keys with two fixed roles; no audit log (per API7's comparison page) | Apache-2.0 | https://docs.api7.ai/api7-gateway/enterprise-features/overview |

## 6. Protocol coverage

| Protocol | Kong | Tyk | APISIX |
|---|---|---|---|
| HTTP/3 (downstream) | No `http3`/`quic` flag in `proxy_listen` (https://developer.konghq.com/gateway/configuration/); long-standing request (https://github.com/Kong/kong/discussions/8983) | No HTTP/3 option in gateway config reference (https://tyk.io/docs/tyk-oss-gateway/configuration) | Experimental client-side `enable_http3`, TLS 1.3 required, no HTTP/3 to upstreams (https://apisix.apache.org/docs/apisix/http3/) |
| gRPC | Supported; grpc/grpcs listed as route protocols (https://developer.konghq.com/plugins/ai-mcp-proxy/) | Upstream `proxy_enable_http2` "Required for gRPC" (https://tyk.io/docs/tyk-oss-gateway/configuration) | gRPC and gRPC transcoding (https://github.com/apache/apisix) |
| gRPC-Web | grpc-web plugin in OSS repo (https://github.com/Kong/kong/tree/master/kong/plugins), since 2.1 (https://developer.konghq.com/plugins/grpc-web/) | Not found | Supported (https://github.com/apache/apisix) |
| GraphQL | GraphQL Rate Limiting Advanced (Enterprise, cost strategies); no federation mention (https://developer.konghq.com/plugins/graphql-rate-limiting-advanced/) | Proxy, Universal Data Graph, Federation v1, subscriptions over WebSocket/SSE; Federation setup documented via Dashboard (https://tyk.io/docs/api-management/graphql) | `graphql-limit-count`, `graphql-proxy-cache` added in 3.17 (https://raw.githubusercontent.com/apache/apisix/master/CHANGELOG.md) |
| WebSocket | ws/wss protocols; WebSocket Validator Enterprise since 3.0 (https://developer.konghq.com/plugins/websocket-validator/) | Supported when WebSockets enabled (https://tyk.io/docs/api-management/graphql) | Supported (https://github.com/apache/apisix) |
| SSE | kafka-consume SSE mode (https://developer.konghq.com/plugins/kafka-consume/); semantic cache streams (https://developer.konghq.com/plugins/ai-semantic-cache/) | MCP Streamable HTTP SSE (https://tyk.io/docs/ai-management/mcp-gateway/overview); Streams SSE (https://tyk.io/docs/api-management/event-driven-apis) | ai-cache SSE replay (https://apisix.apache.org/docs/apisix/next/plugins/ai-cache/) |
| Kafka | kafka-consume (Enterprise, 3.10+; HTTP GET, SSE, WebSocket 3.11+) (https://developer.konghq.com/plugins/kafka-consume/); separate Kong Event Gateway, a Kafka-protocol proxy managed in Konnect (https://developer.konghq.com/event-gateway/architecture/) | Tyk Streams, Enterprise only (https://tyk.io/docs/api-management/event-driven-apis) | Pub-sub over WebSocket + protobuf, fetch/list-offset only, no consumer groups (https://apisix.apache.org/docs/apisix/pubsub/kafka/) |
| MQTT | Not found | Current Streams docs list Kafka, WebSocket, SSE, webhooks only (https://tyk.io/docs/api-management/event-driven-apis) | MQTT proxy with client_id load balancing (https://github.com/apache/apisix) |
| NATS | Not found | Streams NATS output in 5.6-era docs (https://tyk.io/docs/5.6/product-stack/tyk-streaming/configuration/outputs/nats/) | Not found |
| TCP/UDP, other | — | TCP, SOAP (https://github.com/TykTechnologies/tyk) | TCP/UDP proxying, Dubbo (https://github.com/apache/apisix) |

## 7. Rate limiting

| Vendor | Plugin / mode | Algorithms | Distributed backend | Accuracy notes | Source |
|---|---|---|---|---|---|
| Kong OSS | rate-limiting | One fixed window only | local / cluster / redis | — | https://developer.konghq.com/plugins/rate-limiting-advanced/ |
| Kong Enterprise | Rate Limiting Advanced | Sliding (default) and fixed window; multiple windows; throttling (3.12+); CEL limits and keys (3.16+) | local, cluster (not in hybrid/Konnect), Redis incl. Sentinel/Cluster, cloud IAM auth | `sync_rate = 0` is synchronous; larger values allow overage by formula | https://developer.konghq.com/plugins/rate-limiting-advanced/ |
| Tyk | Gateway limiter | Redis rolling (sliding log, "100% rate limiting accuracy", two extra Redis round trips); Sentinel (off-thread); fixed window; DRL (inaccurate on small limits, falls back per user); smoothing; non-transactional mode | Redis | DRL threshold fallback | https://tyk.io/docs/tyk-oss-gateway/configuration |
| Tyk MCP | Per API, per key, per JSON-RPC method, per primitive | — | — | — | https://tyk.io/docs/ai-management/mcp-gateway/overview |
| APISIX | limit-count | Fixed window (default; up to 2x at boundaries); sliding window (3.18) | local, redis, redis-cluster, redis-sentinel; `sync_interval` delayed sync | Global count can lag up to one interval | https://apisix.apache.org/docs/apisix/plugins/limit-count/ |
| APISIX | limit-count, limit-conn, limit-req | — | Redis keepalive params `redis_keepalive_timeout`, `redis_keepalive_pool` (3.15) | — | https://apisix.apache.org/blog/2026/02/05/release-apache-apisix-3.15.0/ |

## 8. Observability (OpenTelemetry)

| Vendor | OTel status | Source |
|---|---|---|
| Kong | opentelemetry plugin present in OSS repo | https://github.com/Kong/kong/tree/master/kong/plugins |
| Kong | 3.13: one OTel plugin emits metrics, logs and traces, which "previously required three separate plugins"; edition split not stated in the post | https://konghq.com/blog/product-releases/kong-gateway-3-13 |
| Kong | 3.16: Metrics PDK (`kong.metrics`) | https://developer.konghq.com/gateway/changelog/ |
| Tyk | OTel tracing since Gateway 5.2; OTLP exporter `grpc` (default, `localhost:4317`) or `http` | https://tyk.io/docs/5.2/product-stack/tyk-gateway/advanced-configurations/distributed-tracing/open-telemetry/open-telemetry-overview/ · https://tyk.io/docs/tyk-oss-gateway/configuration |
| Tyk | 5.13: OpenTelemetry metrics export | https://tyk.io/docs/developer-support/release-notes/gateway |
| Tyk | MCP metrics `tyk.mcp.requests.total` and `tyk.mcp.primitive.duration` | https://tyk.io/tyk-mcp-gateway/ |
| APISIX | opentelemetry plugin: traces only; "only supports binary-encoded OTLP over HTTP"; samplers always_on, always_off, trace_id_ratio, parent_base | https://apisix.apache.org/docs/apisix/plugins/opentelemetry/ |
| APISIX | Metrics via the Prometheus plugin (LLM metrics restructured in 3.18) | https://apisix.apache.org/blog/2026/08/20/release-apache-apisix-3.18.0/ |

## 9. Published performance numbers

| Vendor | Test | Hardware | Result | Source |
|---|---|---|---|---|
| Kong | Gateway 3.16, K6, no plugins, 1 route | c5.4xlarge (16 vCPU) for Kong; c5.metal for load/observability | 144,273.5 RPS, p99 4.5 ms | https://developer.konghq.com/gateway/performance/benchmarks/ |
| Kong | 3.16, no plugins, 100 routes/100 consumers | same | 139,640.9 RPS, p99 4.59 ms | https://developer.konghq.com/gateway/performance/benchmarks/ |
| Kong | 3.16, rate limiting + key-auth, 1 route | same | 102,088.8 RPS, p99 8.45 ms | https://developer.konghq.com/gateway/performance/benchmarks/ |
| Kong | 3.16, rate limiting + key-auth, 100 routes | same | 97,290.2 RPS, p99 8.65 ms | https://developer.konghq.com/gateway/performance/benchmarks/ |
| Kong AI | Gateway 3.10 vs Portkey OSS 1.9.19 vs LiteLLM 1.63.7 (post 2025-07-07); WireMock LLM, 400 VUs, 1,000 prompt tokens, proxy-only | EKS 1.32, c5.4xlarge nodes, 12 CPUs per gateway | Kong +228% vs Portkey, +859% vs LiteLLM throughput; WireMock baseline 29,005.51 RPS, p99 30.35 ms | https://konghq.com/blog/engineering/ai-gateway-benchmark-kong-ai-gateway-portkey-litellm |
| Tyk | Vendor blog, performance tuning | Commodity DigitalOcean droplet, 2 vCPUs, 4GB RAM, with gateway, Redis, upstream and load generator on one box | 6,400 RPS; blog dated 2019-04-09, no version stated | https://tyk.io/blog/performance-tuning-your-tyk-api-gateway/ |
| Tyk | Tyk vs Kong charts | AWS, GCP, Azure, 4 machine classes each | Numbers only in charts; harness `tyk-ansible-performance-testing` | https://tyk.io/performance-benchmarks/ |
| APISIX | README claim | 8-core AWS server | 140,000 QPS at 0.2 ms; ~18k QPS per core | https://github.com/apache/apisix |
| APISIX | Benchmark doc | GCP n1-highcpu-8 (8 vCPU, 7.2 GB), 4 cores for APISIX, 1 KB body, with/without limit-count + prometheus | Figures in charts only | https://apisix.apache.org/docs/apisix/benchmark/ |
| AISIX | Published baseline | 4 vCPUs (instance type not stated) | ~28,300 req/s at saturation; ~0.65 ms added TTFT for streaming | https://api7.ai/ai-gateway/pricing |

## 10. Vendor profiles

### Kong Gateway (OSS / Enterprise / Konnect)

- Current: Enterprise 3.16.0.0 (2026-09-15); LTS 3.10 and 3.14; quarterly cadence since March 2025, first release of each year is LTS (https://developer.konghq.com/gateway/version-support-policy/).
- OSS: no public release after 3.9.x; free mode removed in 3.10; unlicensed Enterprise behaves as expired (https://github.com/Kong/kong/releases) (https://github.com/Kong/kong/discussions/14628).
- Since 3.15 an expired license makes the Admin API read-only (https://developer.konghq.com/gateway/breaking-changes/).
- Extensibility: Lua PDK plus Go/Python/JS PDKs; beta WASM removed in 3.11.0.0 (https://developer.konghq.com/custom-plugins/) (https://developer.konghq.com/gateway/breaking-changes/).
- AI: 23 AI plugins listed on the hub (https://developer.konghq.com/plugins/?category=ai).
- AI load balancing, token rate limiting, semantic cache and MCP are AI Gateway Enterprise (https://developer.konghq.com/plugins/ai-proxy-advanced/) (https://developer.konghq.com/plugins/ai-rate-limiting-advanced/) (https://developer.konghq.com/plugins/ai-semantic-cache/) (https://developer.konghq.com/plugins/ai-mcp-proxy/).
- AI Gateway 2.0 is a separate, Konnect-available product; AI plugins become opt-in in 3.18 (https://konghq.com/blog/product-releases/kong-ai-gateway-2-0-ga).
- 3.16: CEL dynamic config for ACL/Rate Limiting/RLA, Entitlement Enforcement plugin (OpenMeter), FIPS 140-3 package (https://konghq.com/blog/product-releases/kong-api-gateway-3-16).
- 3.16 breaking: CEL backend switched from cel-rust 0.11.6 to cel-cpp 0.15.0 (https://developer.konghq.com/gateway/changelog/).
- GitOps: decK plus Konnect; Konnect Plus priced per gateway with request caps (https://github.com/Kong/deck/releases) (https://konghq.com/pricing).
- Against Ruralz differentiators: no WASM, no HTTP/3 listener flag, no MQTT/NATS found, Kafka only via Enterprise plugins or the separate Event Gateway (https://developer.konghq.com/gateway/configuration/) (https://developer.konghq.com/plugins/kafka-consume/).

### Tyk

- Current: Gateway 5.15.0; LTS 5.13.2 (https://tyk.io/docs/developer-support/release-notes/gateway) (https://tyk.io/docs/developer-support/release-notes/overview).
- License: Gateway MPL-2.0 with commercial `ee` folder (https://github.com/TykTechnologies/tyk).
- Dashboard and Portal are the commercial platform; SSO, RBAC and multi-region deployments are listed on paid plans (https://tyk.io/pricing/) (https://github.com/TykTechnologies/tyk).
- Operator and Sync closed source since 2024-10 (https://github.com/TykTechnologies/tyk-operator) (https://github.com/TykTechnologies/tyk-sync).
- Extensibility: Go native, JS (Goja), Python, Lua, gRPC; no WASM (https://tyk.io/docs/api-management/plugins/overview).
- AI: MCP Gateway in the gateway since 5.13; REST-to-MCP conversion is Enterprise (5.15) (https://tyk.io/docs/ai-management/mcp-gateway/overview).
- LLM features live in AI Studio (AGPL-3.0 CE, 2.1.0); Model Router is Enterprise; budgets Enterprise per README (https://github.com/TykTechnologies/ai-studio) (https://tyk.io/docs/ai-management/ai-studio/core-concepts).
- Protocols: REST, SOAP, GraphQL, gRPC, TCP, MCP (https://github.com/TykTechnologies/tyk).
- GraphQL: UDG and Federation v1 (https://tyk.io/docs/api-management/graphql).
- Event protocols only through Enterprise Tyk Streams (https://tyk.io/docs/api-management/event-driven-apis).
- Rate limiting: widest algorithm menu of the three (rolling, sentinel, fixed, DRL, smoothing) (https://tyk.io/docs/tyk-oss-gateway/configuration).

### Apache APISIX (+ API7)

- Current: 3.18.0 (2026-08-20); four minor releases in 2026 so far (https://apisix.apache.org/blog/archive/).
- License: Apache-2.0 for the project, including all AI plugins (https://github.com/apache/apisix).
- Dashboard embedded since 3.13 and on by default (https://apisix.apache.org/docs/apisix/dashboard/) (https://raw.githubusercontent.com/apache/apisix/master/CHANGELOG.md).
- Extensibility: Lua native, external runners for Java/Go/Python/Node.js, experimental proxy-wasm (https://github.com/apache/apisix) (https://apisix.apache.org/docs/apisix/wasm/).
- AI: free token rate limiting, multi-provider LB with semantic routing, exact + semantic cache (3.18), Lakera/AWS/Aliyun guardrails (https://apisix.apache.org/blog/2026/08/20/release-apache-apisix-3.18.0/).
- MCP support limited to a deprecated stdio bridge (https://apisix.apache.org/docs/apisix/plugins/mcp-bridge/).
- 3.18 breaking: `ai-proxy` uses FFI HTTP client by default, 64 MiB body buffering cap, 8192 logger backlog cap (https://apisix.apache.org/blog/2026/08/20/release-apache-apisix-3.18.0/).
- Protocols: HTTP/3 experimental downstream only (https://apisix.apache.org/docs/apisix/http3/).
- gRPC/gRPC-Web, WebSocket, MQTT, Dubbo, TCP/UDP (https://github.com/apache/apisix).
- Kafka pub-sub over WebSocket (https://apisix.apache.org/docs/apisix/pubsub/kafka/).
- Commercial: API7 Gateway (3.10 LTS, per-core licensing) adds multi-cluster control plane, SSO, audit, portal (https://docs.api7.ai/api7-gateway/enterprise-features/overview) (https://api7.ai/pricing).
- AISIX is a separate Apache-2.0 Rust AI gateway with commercial AISIX Cloud (https://github.com/api7/aisix).

## Sources

https://developer.konghq.com/gateway/version-support-policy/
https://konghq.com/blog/product-releases/kong-api-gateway-3-16
https://developer.konghq.com/gateway/changelog/
https://konghq.com/blog/product-releases/kong-ai-gateway-2-0-ga
https://konghq.com/blog/product-releases/kong-gateway-3-13
https://konghq.com/blog/product-releases/webassembly-in-kong-gateway-3-4
https://developer.konghq.com/gateway/breaking-changes/
https://developer.konghq.com/gateway/configuration/
https://developer.konghq.com/gateway/performance/benchmarks/
https://developer.konghq.com/custom-plugins/
https://developer.konghq.com/ai-gateway/
https://developer.konghq.com/plugins/?category=ai
https://developer.konghq.com/plugins/ai-proxy/
https://developer.konghq.com/plugins/ai-proxy-advanced/
https://developer.konghq.com/plugins/ai-rate-limiting-advanced/
https://developer.konghq.com/plugins/ai-semantic-cache/
https://developer.konghq.com/plugins/ai-mcp-proxy/
https://developer.konghq.com/plugins/rate-limiting-advanced/
https://developer.konghq.com/plugins/graphql-rate-limiting-advanced/
https://developer.konghq.com/plugins/websocket-validator/
https://developer.konghq.com/plugins/grpc-web/
https://developer.konghq.com/plugins/kafka-consume/
https://developer.konghq.com/event-gateway/architecture/
https://konghq.com/pricing
https://konghq.com/blog/engineering/ai-gateway-benchmark-kong-ai-gateway-portkey-litellm
https://github.com/Kong/kong/tree/master/kong/plugins
https://github.com/Kong/kong/releases
https://raw.githubusercontent.com/Kong/kong/master/CHANGELOG.md
https://github.com/Kong/kong/discussions/14628
https://github.com/Kong/kong/discussions/8983
https://github.com/Kong/deck/releases
https://tyk.io/docs/developer-support/release-notes/gateway
https://tyk.io/docs/developer-support/release-notes/overview
https://github.com/TykTechnologies/tyk
https://tyk.io/docs/api-management/plugins/overview
https://tyk.io/pricing/
https://github.com/TykTechnologies/ai-studio
https://github.com/TykTechnologies/ai-studio/pull/564
https://github.com/TykTechnologies/ai-studio/pull/580
https://tyk.io/blog/ai-studio-is-going-open-source-and-why-the-ai-control-plane-must-be-extensible/
https://tyk.io/docs/ai-management/ai-studio/overview
https://tyk.io/docs/ai-management/ai-studio/core-concepts
https://tyk.io/docs/ai-management/mcp-gateway/overview
https://tyk.io/tyk-mcp-gateway/
https://tyk.io/docs/api-management/event-driven-apis
https://tyk.io/docs/5.6/product-stack/tyk-streaming/configuration/outputs/nats/
https://tyk.io/docs/api-management/graphql
https://tyk.io/docs/tyk-oss-gateway/configuration
https://tyk.io/docs/5.2/product-stack/tyk-gateway/advanced-configurations/distributed-tracing/open-telemetry/open-telemetry-overview/
https://github.com/TykTechnologies/tyk-operator
https://tyk.io/docs/5.6/product-stack/tyk-operator/release-notes/operator-1.0/
https://tyk.io/docs/tyk-stack/tyk-operator/installing-tyk-operator
https://github.com/TykTechnologies/tyk-sync
https://tyk.io/performance-benchmarks/
https://tyk.io/blog/performance-tuning-your-tyk-api-gateway/
https://apisix.apache.org/blog/archive/
https://apisix.apache.org/blog/2026/08/20/release-apache-apisix-3.18.0/
https://apisix.apache.org/blog/2026/02/05/release-apache-apisix-3.15.0/
https://raw.githubusercontent.com/apache/apisix/master/CHANGELOG.md
https://github.com/apache/apisix
https://apisix.apache.org/docs/apisix/wasm/
https://apisix.apache.org/docs/apisix/http3/
https://apisix.apache.org/docs/apisix/dashboard/
https://github.com/apache/apisix-dashboard
https://apisix.apache.org/docs/apisix/plugins/ai-rate-limiting/
https://apisix.apache.org/docs/apisix/plugins/ai-proxy-multi/
https://apisix.apache.org/docs/apisix/next/plugins/ai-cache/
https://apisix.apache.org/docs/apisix/plugins/mcp-bridge/
https://apisix.apache.org/docs/apisix/pubsub/kafka/
https://apisix.apache.org/docs/apisix/plugins/opentelemetry/
https://apisix.apache.org/docs/apisix/plugins/limit-count/
https://apisix.apache.org/docs/apisix/benchmark/
https://github.com/api7/adc
https://github.com/api7/adc/releases
https://api7.ai/pricing
https://docs.api7.ai/api7-gateway/enterprise-features/overview
https://docs.api7.ai/api7-gateway/version-support-policy
https://api7.ai/blog/api7-gateway-3-10-4-runtime-reliability
https://github.com/api7/aisix
https://api7.ai/ai-gateway/pricing

## Gaps

- Kong 3.16 date: the version-support policy and the changelog give 2026-09-15; the release blog is dated 2026-09-22. This file uses 2026-09-15.
- Kong free-mode wording conflict: the current breaking-changes page still says free mode "is deprecated and will be removed in a future 3.x version", while the 3.10 release-note text quoted in GitHub discussion #14628 says it "is no longer available". The primary 3.10 changelog entry was not located in the fetched changelog page.
- Kong OSS: whether prebuilt OSS Docker images exist for 3.10+ was not checked on Docker Hub; only the absence of GitHub releases after 3.9.x was confirmed. Release dates for 3.9.1 to 3.9.3 could not be reliably extracted.
- Kong AI tier per plugin: the AI plugin index does not show tiers. Enterprise status was confirmed only for AI Proxy Advanced, AI Rate Limiting Advanced, AI Semantic Cache and AI MCP Proxy. The AI Proxy page does not say whether it needs an AI license on the 3.10+ Enterprise image. Guardrail plugin and AI A2A Proxy tiers were not checked.
- Kong AI Gateway 2.0: the GA post does not state self-hosted availability or licensing; the AI Gateway docs mention both Konnect and self-hosted deployments.
- Kong WebSocket in OSS: the WebSocket Validator page does not say whether ws/wss service protocols require Enterprise. Kong Event Gateway GA status is unclear across secondary sources (early access vs GA in Q4 2025); only its architecture page is referenced.
- Tyk 5.15.0 date conflict: 2026-09-01 on the Gateway release-notes page vs 2026-07-07 on the release overview page. The Gateway release-notes page dates 5.14.0 as 2026-07-07; the overview page does not list 5.14.0.
- Tyk AI Studio budgets: the README lists budgets as Enterprise-only, while the core-concepts page lists them with no edition marker. Semantic caching is not documented. Rate limiting is not in the README's edition table; it appears only as a core AI Gateway capability.
- Tyk Streams protocols: current docs list Kafka, WebSocket, SSE, webhooks and HTTP. MQTT appears only in marketing text and NATS only in 5.6-era docs. The underlying engine is not stated.
- Tyk GraphQL: the OSS vs Dashboard split for UDG and Federation may describe where they are configured (UI) rather than a runtime license gate. Federation v2 is not documented. No gRPC-Web or HTTP/3 support was found in Tyk docs.
- Tyk MCP OTel metric names come from tyk.io product content found via search and are not confirmed in the reference docs.
- The Tyk benchmark page shows charts only, with no extractable numbers, versions or dates; the 6,400 RPS / 2-vCPU figure comes from a 2019-04-09 performance-tuning blog post with no Tyk version stated.
- Date rendering: the fetch tool showed wrong years for GitHub release pages (for example APISIX 3.15.0 as 2024). APISIX dates here come from the APISIX blog archive. decK dates were affected too; their 2026 year is inferred from the content (Go 1.26.6, AI Gateway 2.0 support).
- The APISIX benchmark doc shows charts only; the README's 140,000 QPS / 8-core claim has no version or date. GraphQL federation, NATS and Kafka produce support were not found in APISIX docs, which does not prove they are absent.
- APISIX MCP: no replacement for the deprecated mcp-bridge was found in the 3.18 notes.
- API7: docs URLs suggest "API7 Gateway" is the new name for "API7 Enterprise", but no page says so. AISIX has no tagged release on its README. Its 28,300 req/s / 4 vCPU baseline comes from API7's AISIX pricing page (the docs overview page does not publish it), and the CPU model and instance type are unknown.
