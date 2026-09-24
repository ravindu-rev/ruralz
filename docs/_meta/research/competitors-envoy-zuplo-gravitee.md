# Competitor Research: Envoy Gateway, Agent Router, Zuplo, Gravitee, Traefik, NGINX Gateway Fabric, Gateway API

| Field | Value |
|---|---|
| Topic | Envoy Gateway and Agent Router (formerly Envoy AI Gateway), Zuplo, Gravitee, plus short profiles of Traefik Hub, NGINX Gateway Fabric, and the Kubernetes Gateway API v1.5/v1.6 conformance landscape |
| Snapshot date | 2026-09-23 |
| Method | Primary sources only: GitHub releases and tags (read through the GitHub REST API for exact ISO timestamps), vendor documentation (read as Markdown where the vendor publishes `.md` or `llms.txt` views), vendor pricing pages, and official changelogs. Each vendor is covered on the same dimensions: extensibility, AI, control plane, protocols, rate limiting, licensing/tiers, performance claims. Vendor-published benchmark numbers are labelled as vendor claims. Where sources disagree, the conflict is listed under Gaps. |
| Scope note | This file covers facts only. Ruralz positioning is defined in `docs/_meta/foundation-pack.md` and is not restated here. |

## 1. Cross-vendor summary

| Dimension | Envoy Gateway | Agent Router (ex-Envoy AI Gateway) | Zuplo | Gravitee APIM / Gamma | Traefik (Proxy / Hub) | NGINX Gateway Fabric |
|---|---|---|---|---|---|---|
| Latest release (as of 2026-09-23) | v1.9.1, 2026-08-28 (https://github.com/envoyproxy/gateway/releases) | v1.1.0, 2026-08-21 (https://github.com/theagentrouter/agent-router/releases/tag/v1.1.0) | SaaS, no public version; Zudoku portal v0.87.0, 2026-09-15 (https://github.com/zuplo/zudoku) | APIM 4.12.20 tag; Gamma first release 2026-06-26 (https://github.com/gravitee-io/gravitee-api-management; https://documentation.gravitee.io/gravitee-gamma/platform-management/gamma-release-notes.md) | Proxy v3.7.13, 2026-09-04; Hub v3.21.0-ea.3, 2026-09-14 (https://github.com/traefik/traefik/releases; https://github.com/traefik/hub/releases/tag/v3.21.0-ea.3) | v2.7.2, 2026-09-16 (https://github.com/nginx/nginx-gateway-fabric/releases) |
| Core license | Apache-2.0 (https://github.com/envoyproxy/gateway) | Apache-2.0 (https://github.com/theagentrouter/agent-router) | Proprietary SaaS; Zudoku portal is MIT (https://github.com/zuplo/zudoku) | Community Edition Apache-2.0; EE needs a license (https://github.com/gravitee-io/gravitee-api-management; https://documentation.gravitee.io/apim/introduction/enterprise-edition.md) | Proxy MIT; Hub commercial (https://github.com/traefik/traefik; https://traefik.io/pricing) | Apache-2.0; NGINX Plus features commercial (https://github.com/nginx/nginx-gateway-fabric; https://github.com/nginx/nginx-gateway-fabric/blob/v2.7.0/README.md) |
| Custom code | Wasm, Lua, ExtProc, Dynamic Modules (https://gateway.envoyproxy.io/docs/api/extension_types/) | CEL cost expressions; inherits Envoy Gateway (https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/) | TypeScript policies and handlers (https://zuplo.com/api-management.md) | Java plugin policies (Policy Studio) (https://documentation.gravitee.io/apim/plugins/customization.md; https://documentation.gravitee.io/apim/create-and-configure-apis/apply-policies/v4-api-policy-studio.md) | Yaegi-interpreted Go plugins (https://plugins.traefik.io/install) | Policy CRDs incl. SnippetsPolicy (https://github.com/nginx/nginx-gateway-fabric/blob/v2.7.0/CHANGELOG.md) |
| Token-based LLM limits | Via Agent Router | QuotaPolicy + usage-based rate limit (https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/) | Dollar/token/request budgets at gateway, team, app (https://zuplo.com/docs/ai-gateway/usage-limits.md) | Token Rate Limit policy (https://documentation.gravitee.io/gravitee-gamma/agent-management/build/llm-proxies/configure-an-llm-proxy/design/add-the-token-rate-limit-policy.md) | Rate Limit & Quota middleware (https://doc.traefik.io/traefik-hub/ai-gateway/overview) | Not documented; F5 AI Guardrails integration only (https://github.com/nginx/nginx-gateway-fabric/blob/v2.7.0/CHANGELOG.md) |
| Gateway API conformance badge | v1.6.1 (https://gateway-api.sigs.k8s.io/implementations/) | n/a (runs on Envoy Gateway) | n/a | Gravitee Kubernetes Operator v1.6.1 (https://gateway-api.sigs.k8s.io/implementations/) | Traefik Proxy v1.6.1 (https://gateway-api.sigs.k8s.io/implementations/) | v1.6.1 (https://gateway-api.sigs.k8s.io/implementations/) |

## 2. Envoy Gateway

### 2.1 Releases and compatibility

| Envoy Gateway | Envoy Proxy | Gateway API | Kubernetes | EOL | Source |
|---|---|---|---|---|---|
| v1.9 | distroless-v1.39.x | v1.6.1 | v1.33-v1.36 | 2027-02-14 | https://gateway.envoyproxy.io/news/releases/matrix/ |
| v1.8 | distroless-v1.38.x | v1.5.1 | v1.32-v1.35 | 2026-11-08 | https://gateway.envoyproxy.io/news/releases/matrix/ |
| v1.7 | distroless-v1.37.x | v1.4.1 | v1.32-v1.35 | 2026-08-05 | https://gateway.envoyproxy.io/news/releases/matrix/ |

- Release dates (GitHub `published_at`): v1.8.0 on 2026-05-13, v1.8.1 on 2026-06-05, v1.9.0-rc.1 on 2026-08-09, v1.9.0 on 2026-08-15, v1.9.1 and v1.8.4 on 2026-08-28 (https://github.com/envoyproxy/gateway/releases).
- v1.9.0 moved to Gateway API v1.6.0/v1.6.1 and Go 1.26.5 (https://github.com/envoyproxy/gateway/releases/tag/v1.9.0).
- v1.8.0 moved to Gateway API v1.5.1 (https://github.com/envoyproxy/gateway/releases/tag/v1.8.0).
- The Envoy Gateway implementation holds a Gateway API "Gateway Conformance v1.6.1" badge (https://gateway-api.sigs.k8s.io/implementations/).
- The repository is Apache-2.0 licensed and had 3,045 GitHub stars at snapshot (https://github.com/envoyproxy/gateway).

### 2.2 CRDs (API group `gateway.envoyproxy.io/v1alpha1`)

| Kind | Purpose | Source |
|---|---|---|
| `BackendTrafficPolicy` | Proxy-to-upstream behavior: rate limit, retries, circuit breaking, response override | https://gateway.envoyproxy.io/docs/api/extension_types/ |
| `ClientTrafficPolicy` | Client-facing listener settings (TLS, HTTP/3 settings, IP detection) | https://gateway.envoyproxy.io/docs/api/extension_types/ |
| `SecurityPolicy` | Authentication and authorization rules | https://gateway.envoyproxy.io/docs/api/extension_types/ |
| `EnvoyExtensionPolicy` | Wasm, Lua, ExtProc, DynamicModule filters | https://gateway.envoyproxy.io/docs/api/extension_types/ |
| `EnvoyPatchPolicy` | Raw patches to generated Envoy config | https://gateway.envoyproxy.io/docs/api/extension_types/ |
| `EnvoyProxy`, `EnvoyGateway` | Data-plane deployment and control-plane configuration | https://gateway.envoyproxy.io/docs/api/extension_types/ |
| `Backend`, `HTTPRouteFilter` | Non-Service upstream targets; extra route filters | https://gateway.envoyproxy.io/docs/api/extension_types/ <!-- alias-ok --> |

v1.9.0 API additions:
- `BackendTrafficPolicy`: response override can match on response headers; a `Week` unit was added to `RateLimitUnit` (https://github.com/envoyproxy/gateway/releases/tag/v1.9.0).
- `SecurityPolicy`: CSRF origin validation and CEL-based authorization (https://github.com/envoyproxy/gateway/releases/tag/v1.9.0).
- `ClientTrafficPolicy`, `SecurityPolicy`, `EnvoyExtensionPolicy`: ListenerSet policy attachment (https://github.com/envoyproxy/gateway/releases/tag/v1.9.0).
- `EnvoyExtensionPolicy`: `FilterContext` for Lua and `StatusOnError` for ExtProc (https://github.com/envoyproxy/gateway/releases/tag/v1.9.0).
- Also new: a remote infrastructure provider, HTTPFilter support for GRPCRoute, and BackendUtilization load balancing with out-of-band reporting (https://github.com/envoyproxy/gateway/releases/tag/v1.9.0).
- Lua's `disableLua` is deprecated in favor of `enableLua` (https://github.com/envoyproxy/gateway/releases/tag/v1.9.0).

v1.8.0 additions:
- Remote-source dynamic modules <!-- alias-ok -->, TLS for Wasm code sources, and chaining of multiple ExtensionManagers (https://github.com/envoyproxy/gateway/releases/tag/v1.8.0).
- Per-rule rate-limit options, shadow mode, and a bandwidth limit (https://github.com/envoyproxy/gateway/releases/tag/v1.8.0).
- GeoIP, and admission control in `BackendTrafficPolicy` (https://github.com/envoyproxy/gateway/releases/tag/v1.8.0).

### 2.3 Extensibility

- `EnvoyExtensionPolicy` supports four extension types: Wasm, Lua, ExtProc and DynamicModule (https://gateway.envoyproxy.io/docs/api/extension_types/). <!-- alias-ok -->
- The documented Dynamic Modules task <!-- alias-ok --> loads `.so` files from the local filesystem (`source.type: Local`, `local.path`), mounted via Kubernetes 1.35+ image volumes, custom images, or shared volumes populated by an init container (https://gateway.envoyproxy.io/docs/tasks/extensibility/dynamic-modules/).
- The API version for this feature is `v1alpha1` (https://gateway.envoyproxy.io/docs/tasks/extensibility/dynamic-modules/).

### 2.4 Rate limiting

- Global rate limiting uses the Envoy Ratelimit service and "requires a Redis instance as its caching layer" (https://gateway.envoyproxy.io/docs/tasks/traffic/global-rate-limit/).
- Global limits are set through `BackendTrafficPolicy.rateLimit.global.rules[]`, with `clientSelectors`, `limit.requests` and `limit.unit` (https://gateway.envoyproxy.io/docs/tasks/traffic/global-rate-limit/).
- `shared: true` makes one bucket for the whole gateway; the default is a bucket per route (https://gateway.envoyproxy.io/docs/tasks/traffic/global-rate-limit/).
- Client selectors match on headers (with `invert`), source CIDR (`Distinct` or `Exact`), path and method (https://gateway.envoyproxy.io/docs/tasks/traffic/global-rate-limit/).
- `RateLimitSpec` has two scopes: `Global` (applied across all Envoy proxy instances) and `Local` (per Envoy proxy instance). The older `type` field is deprecated in favor of setting `global` and/or `local` directly, and both can be combined (https://gateway.envoyproxy.io/docs/api/extension_types/).
- Costs can be a fixed number or read from per-request dynamic metadata (`RateLimitCost`, `RateLimitCostFrom` with values `Number` and `Metadata`, `RateLimitCostMetadata`) (https://gateway.envoyproxy.io/docs/api/extension_types/). This is the hook Agent Router uses for token-based limits, reading the response cost from the `io.envoy.ai_gateway` metadata namespace (https://theagentrouter.ai/docs/capabilities/traffic/usage-based-ratelimiting/).

### 2.5 Control plane and deployment

- Envoy Gateway is described as a Kubernetes-native control plane whose controller watches Gateway API and Envoy Gateway resources and translates them into Envoy Proxy configuration (https://gateway.envoyproxy.io/docs/concepts/).
- A standalone mode (File resource provider plus Host infrastructure provider) runs on bare metal or VMs (https://gateway.envoyproxy.io/docs/tasks/operations/standalone-deployment-mode/).
- The docs label standalone mode "an experimental feature, please DO NOT use it in production" (https://gateway.envoyproxy.io/docs/tasks/operations/standalone-deployment-mode/).
- There is no built-in web UI, GitOps rollout engine or developer portal in the project docs reviewed. See Gaps.

### 2.6 Performance claims

- Every release ships benchmark report assets alongside the binaries (https://github.com/envoyproxy/gateway/releases).
- No headline RPS figure is published in the release notes reviewed (https://github.com/envoyproxy/gateway/releases/tag/v1.9.0).

## 3. Agent Router (formerly Envoy AI Gateway)

### 3.1 Rename and governance change (2026-09)

- The announcement post, "Envoy AI Gateway is becoming Agent Router, an Agentic AI Foundation project", is dated 2026-09-09 (https://theagentrouter.ai/blog/envoy-ai-gateway-is-now-agent-router/).
- The project joined the Agentic AI Foundation (AAIF) under the Linux Foundation. It is no longer an Envoy/CNCF sub-project, but it is "still built on Envoy and Envoy Gateway" (https://theagentrouter.ai/blog/envoy-ai-gateway-is-now-agent-router/; https://www.linuxfoundation.org/press/linux-foundation-announces-the-formation-of-the-agentic-ai-foundation).
- The repository moved from `envoyproxy/ai-gateway` to `theagentrouter/agent-router`, and old links redirect (https://github.com/theagentrouter/agent-router).
- The README says: "Same code, same maintainers, same release cadence and Apache 2.0 license" (https://github.com/theagentrouter/agent-router).
- These did not change: the CRDs and API group (`aigateway.envoyproxy.io`: `AIGatewayRoute`, `AIServiceBackend`, `BackendSecurityPolicy`), the CLI `aigw`, the namespace `envoy-ai-gateway-system`, container images, Helm charts and Go module paths (https://theagentrouter.ai/blog/envoy-ai-gateway-is-now-agent-router/; https://github.com/theagentrouter/agent-router). <!-- alias-ok -->
- The repository had 2,131 GitHub stars at snapshot (https://github.com/theagentrouter/agent-router).

### 3.2 Releases

| Version | Date (ISO) | Highlights | Source |
|---|---|---|---|
| v1.1.0 | 2026-08-21 | `/tokenize` token counting across providers; stream idle timeout that can fail over before the first token; per-request `credentialOverride`; MCP hostname scoping and CEL-based upstream selection; built on Envoy Gateway v1.8.1 / Envoy v1.38.1 | https://github.com/theagentrouter/agent-router/releases/tag/v1.1.0 |
| v1.0.0 | 2026-06-23 | GA; `v1beta1` APIs promised free of breaking changes within 1.x; 16 providers behind one OpenAI-compatible API; MCP gateway; token/quota-aware rate limiting; provider fallback; OpenInference-compatible OTel tracing | https://github.com/theagentrouter/agent-router/releases/tag/v1.0.0 |
| v0.7.0 | 2026-06-06 | Hostname multi-tenant routing; Anthropic Messages to Bedrock Converse translation; rate limit filter injection for QuotaPolicy | https://github.com/theagentrouter/agent-router/releases |
| v0.6.0 | 2026-05-05 | First `v1beta1` core CRDs; body redaction | https://github.com/theagentrouter/agent-router/releases |
| v0.5.0 | 2026-01-23 | `GatewayConfig` CRD; OpenAI Responses API; CEL-based MCP authorization | https://github.com/theagentrouter/agent-router/releases |

Providers listed across releases: OpenAI, Azure OpenAI, Gemini, Vertex AI, AWS Bedrock, Anthropic, Mistral, Cohere, Groq, Together AI, DeepInfra, DeepSeek, Hunyuan, SambaNova, Grok and Tetrate Agent Router Service (https://github.com/theagentrouter/agent-router/releases).

### 3.3 Token-based limits: QuotaPolicy and usage-based rate limiting

- `QuotaPolicy` (`aigateway.envoyproxy.io/v1alpha1`) targets `AIServiceBackend` resources via `targetRefs` (https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/).
- Windows are `1s`, `1m`, `1h` or `1d`. It returns HTTP 429 when all related upstream quotas are exceeded (https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/).
- `perModelQuotas` sets a budget per model name. Bucket rules with header-based client selectors (`type: Distinct`) give per-tenant buckets (https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/).
- Cost is a CEL expression over `input_tokens`, `output_tokens`, `total_tokens`, `cached_input_tokens`, `cache_creation_input_tokens` and `reasoning_tokens`, for example `input_tokens + cached_input_tokens / 10u + output_tokens * 6u` (https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/).
- Only `Shared` mode is implemented, and service-wide quotas are not (https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/).
- Enforcement uses Redis plus the Envoy Gateway rate limit infrastructure (https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/).
- The two mechanisms differ in what they limit. QuotaPolicy caps cumulative spend (e.g., 100,000 tokens/hour). Usage-based rate limiting controls request velocity and uses `llmRequestCosts` on `AIGatewayRoute` (https://theagentrouter.ai/docs/capabilities/traffic/usage-based-ratelimiting/).

### 3.4 Provider fallback

- `AIGatewayRoute` lists several `backendRefs` with priorities: "The first backend is treated as primary, and subsequent backends are considered fallbacks" (https://theagentrouter.ai/docs/capabilities/traffic/provider-fallback/). <!-- alias-ok -->
- Retries are set with an Envoy Gateway `BackendTrafficPolicy` attached to the generated HTTPRoute (`numAttemptsPerPriority`, `numRetries`, triggers `connect-failure` and `retriable-status-codes`) (https://theagentrouter.ai/docs/capabilities/traffic/provider-fallback/).

### 3.5 Performance claims (vendor-affiliated source)

- Tetrate reports a Broadcom VMware Cloud Foundation validation from July 2026. It found about 2 ms of gateway overhead (about 0.01% of end-to-end latency) in a 3-hour run at 190 concurrent users on a four-H100 cluster, with an average TTFT of 0.103 s. Saturation came at 224 users because of GPU compute (https://tetrate.io/learn/ai/ai-gateway-benchmarks).
- The same page reports a February 2026 control-plane test to 2,000 `AIGatewayRoute` resources, which needed the gRPC max message size raised from 4 MB to 25 MB (https://tetrate.io/learn/ai/ai-gateway-benchmarks).

## 4. Zuplo

### 4.1 Plans and limits (pricing page generated 2026-09-23)

| Item | Free | Builder | Enterprise | Source |
|---|---|---|---|---|
| Price | $0 | $25/mo | From $1,000/mo, annual contract | https://zuplo.com/pricing |
| Monthly requests | 100K (rate-limited when exceeded) | 100K included, $100 per extra 100K, capped at 1M/mo | Custom; base package includes 1M | https://zuplo.com/pricing |
| Environments / projects | 5 / 2 | 10 / 2 | Custom | https://zuplo.com/pricing |
| Consumers and API keys | 100 | 1,000 | Custom | https://zuplo.com/pricing |
| Custom domains | 0 | 2 ($25/mo) | Custom | https://zuplo.com/pricing |
| Developer seats | 2 | 2 | Custom | https://zuplo.com/pricing |
| Analytics / log retention | 1 day / 15 min live | 7 days / 1 day | 30 days / 3 days | https://zuplo.com/pricing |
| Monetization | 100K events | 100K events | Add-on | https://zuplo.com/pricing |
| AI Gateway users / teams | Development / 1 | 5 / 1 | Custom | https://zuplo.com/pricing |
| MCP tool invocations | 1K/mo | 10K/mo | Up to unlimited | https://zuplo.com/pricing |
| GitOps | GitHub | GitHub | GitHub, Bitbucket, GitLab, Azure DevOps | https://zuplo.com/pricing |
| Self-hosted / dedicated | No | No | Available | https://zuplo.com/pricing |
| SSO/RBAC, audit logs, SOC 2 | No | No | Add-ons | https://zuplo.com/pricing |
| SLA | None | None | 99.5% baseline, up to 99.999% | https://zuplo.com/pricing |

### 4.2 Programmability and policies

- The vendor's summary: "Every policy is just a TypeScript function — customize it, ship it via git" (https://zuplo.com/api-management.md).
- The policy catalog links 111 distinct inbound/outbound policy pages. They include `custom-code-inbound`/`custom-code-outbound`, `rate-limit-inbound`, `complex-rate-limit-inbound`, `quota-inbound`, `monetization-inbound`, `semantic-cache-inbound`, `prompt-injection-outbound`, 12 MCP OAuth policies, and 10 `ai-gateway-*` policies such as `ai-gateway-fallback-model-v2-inbound` and `ai-gateway-metering-v2-inbound` (https://zuplo.com/docs/policies/overview). The count was taken from a URL crawl.
- The runtime "doesn't run Node.js". It is a custom JavaScript engine that is compatible with certain npm modules that do not use native code or Node.js-specific features such as the file system (https://zuplo.com/docs/programmable-api/node-modules.md).
- On SaaS deployments, "Zuplo routes all requests through Cloudflare" (https://zuplo.com/docs/programmable-api/compatibility-dates.md).
- Documented handlers include URL forward/rewrite, WebSocket, WebSocket pipeline, AWS Lambda, MCP server, redirect and custom handlers. There are GraphQL-specific policies. No gRPC handler is listed in the docs index (https://zuplo.com/docs/llms.txt).

### 4.3 Rate limiting

- Rate limiting uses a "sliding window algorithm enforced globally across all edge locations" (https://zuplo.com/docs/rate-limiting/how-it-works.md).
- `rateLimitBy` has four modes: `ip`, `user`, `function` (a TypeScript function returning `CustomRateLimitDetails`) and `all` (https://zuplo.com/docs/rate-limiting/how-it-works.md).

### 4.4 AI Gateway budgets

- Budgets apply at three levels: Gateway, Team (including nested sub-teams) and App (https://zuplo.com/docs/ai-gateway/usage-limits.md).
- Budgets cover spend, tokens and requests, and each limit is either Block or Warn (https://zuplo.com/docs/ai-gateway/usage-limits.md).
- An exhausted Block limit returns 429 unless a quota fallback is configured (https://zuplo.com/docs/ai-gateway/usage-limits.md).
- App budgets may add up to more than the team budget, but the team budget still caps combined usage (https://zuplo.com/docs/ai-gateway/usage-limits.md).
- Other AI features: routing to OpenAI, Anthropic, Gemini and Mistral through one OpenAI-compatible endpoint, semantic caching by vector similarity, prompt-injection blocking, secret/PII masking, and trace export to Galileo and Comet Opik (https://zuplo.com/ai-gateway.md). <!-- alias-ok -->
- All plans include the AI Gateway, the MCP Gateway and the developer portal (https://zuplo.com/pricing).

### 4.5 Developer portal and monetization

- The developer portal is Zudoku, which is MIT-licensed and was at v0.87.0 on 2026-09-15 (https://github.com/zuplo/zudoku).
- Monetization is built from four parts: Meters, Features, Plans and Rate cards (https://zuplo.com/docs/articles/monetization.md).
- Pricing models are flat, per-unit, tiered, volume and package, billed through Stripe (https://zuplo.com/docs/articles/monetization.md).
- A customer who exceeds quota gets `403 Forbidden` immediately (https://zuplo.com/docs/articles/monetization.md).

### 4.6 Deployment and control plane

- Three deployment models are documented. Managed Edge runs in 300+ data centers. Managed Dedicated runs on providers including AWS, Azure, GCP, Akamai Connected Cloud, Equinix and TerraSwitch. Self-Hosted runs in the customer's Kubernetes (https://zuplo.com/docs/dedicated/overview.md; https://zuplo.com/pricing).
- Self-Hosted is hybrid. Zuplo compiles the project and hosts the portal, and the customer's cluster builds, stores and runs the gateway image (https://zuplo.com/docs/self-hosted/overview.md).
- Self-Hosted requirements: Helm 3.8+, three or more nodes, one LoadBalancer for HAProxy ingress, and a privileged builder Job (https://zuplo.com/docs/self-hosted/requirements.md).

### 4.7 Performance claims

- The vendor claims "sub-50ms latency in 300+ locations worldwide" and lists "<50ms Edge latency". No methodology or hardware is given (https://zuplo.com/solutions/scale-and-reliable.md; https://zuplo.com/enterprise.md).

## 5. Gravitee

### 5.1 Versions and products

- The APIM repository is Apache-2.0 and had 455 GitHub stars at snapshot (https://github.com/gravitee-io/gravitee-api-management).
- The newest tag line is 4.12.x, up to 4.12.20. The 4.12.0 tag commit is dated 2026-06-26 and the 4.12.20 tag commit is dated 2026-09-21. No 4.13.0 or 5.0.0 tag exists (https://github.com/gravitee-io/gravitee-api-management).
- Gravitee Gamma is a new platform that combines Agent, API, Authorization, Edge and Event Stream Management. Its first release date is 2026-06-26 (https://documentation.gravitee.io/gravitee-gamma/platform-management/gamma-release-notes.md).

### 5.2 4.x feature timeline

| Release | Features | Source |
|---|---|---|
| 4.10 | LLM Proxy with an OpenAI-compatible interface (providers: Bedrock, Gemini, OpenAI, OpenAI-compatible); AI Token Rate Limit policy; new Developer Portal; Kafka Gateway SASL Delegate to Broker, including AWS MSK IAM | https://documentation.gravitee.io/apim/4.10/release-information/release-notes/apim-4.10 |
| 4.12 | Kafka port-based routing for native APIs; OTel tracing for Kafka native APIs; Token-Bucket Rate Limiting policy (strict or async); Hazelcast as a distributed rate-limit store alongside Redis; Redis Cluster support; Assign Metrics for LLM token cost during streaming; MCP server install widget in the portal; WSDL 1.1 import; requires JRE 21 and is incompatible with Java 25 | https://documentation.gravitee.io/apim/release-information/release-notes/apim-4.12 |
| Gamma (2026-06-26) | Unified AI Gateway runtime for LLM, MCP and A2A; LLM Proxy for Anthropic, OpenAI, Bedrock, Gemini Enterprise Agent Platform and Azure; MCP Studio composite servers; A2A Proxy; GAPL authorization language (Cedar-syntax subset) with an in-gateway PDP; EU AI Act compliance scoring; Kafka virtual clusters | https://documentation.gravitee.io/gravitee-gamma/platform-management/gamma-release-notes.md |

### 5.3 Kafka-native and event-native gateway

- The Kafka Gateway speaks the Kafka wire protocol over TCP and "is treated like a traditional Kafka broker by consumers and producers" (https://documentation.gravitee.io/apim/kafka-gateway.md).
- It embeds the Apache Kafka 3.9 client libraries and negotiates protocol API versions per connection (https://documentation.gravitee.io/apim/kafka-gateway.md).
- It supports virtual topics and partitions, and multi-tenant routing to different clusters (https://documentation.gravitee.io/apim/kafka-gateway.md).
- A separate Kafka connector does HTTP-to-Kafka protocol mediation for v4 Message APIs <!-- alias-ok -->, with PLAINTEXT, SASL_PLAINTEXT, SASL_SSL and SSL security (https://documentation.gravitee.io/apim/create-and-configure-apis/configure-v4-apis/endpoints/kafka.md).
- Kafka-specific policies for native Kafka APIs include Kafka ACL, Message Filtering, Offloading, Quota, Topic Mapping, Transform Key and Native IP filtering (https://documentation.gravitee.io/apim/kafka-gateway/create-and-configure-kafka-apis/configure-kafka-apis/policies.md), plus a Kafka Message Encryption & Decryption policy (https://documentation.gravitee.io/apim/create-and-configure-apis/apply-policies/policy-reference/kafka-message-encryption-decryption-policy-reference.md).

### 5.4 Community vs Enterprise split (APIM 4.12)

| Area | Enterprise-only items listed | Source |
|---|---|---|
| Global | Audit Trail, Custom Roles, Debug Mode, DCR, Enterprise OIDC SSO, Sharding Tags, Datadog/TCP/Cloud reporters, Bridge Gateway, Cache Redis, GeoIP | https://documentation.gravitee.io/apim/introduction/enterprise-edition.md |
| API Management | Proxy Reactor, HTTP GET/POST entrypoints, Assign Metrics, Data Logging Masking, GeoIP Filtering, Enterprise Policy Pack | https://documentation.gravitee.io/apim/introduction/enterprise-edition.md |
| Event Management | Message Reactor; SSE, Webhook and WebSocket entrypoints; Kafka, MQTT5, RabbitMQ, Solace, JMS and Azure Service Bus connectors; Confluent Schema Registry | https://documentation.gravitee.io/apim/introduction/enterprise-edition.md |
| AI Agent Management | A2A Proxy, LLM Proxy, MCP Proxy, MCP Tool Server | https://documentation.gravitee.io/apim/introduction/enterprise-edition.md |
| Other | Alert Engine; SaaS, hybrid and self-hosted EE hosting | https://documentation.gravitee.io/apim/introduction/enterprise-edition.md |

- Pricing for API Management: Planet at $2,500/mo (1 production gateway); Galaxy (custom, 2 gateways, "limited event broker support"); Universe (custom, 4+ gateways, "unlimited enterprise functionality") (https://www.gravitee.io/pricing).
- Pricing for Event Management (Kafka): Comet at $1,250/mo (1 gateway); Meteor and Asteroid (custom; Asteroid adds protocol mediation) (https://www.gravitee.io/pricing).
- The pricing page does not list a free tier (https://www.gravitee.io/pricing).

### 5.5 Token rate limiting (Gamma LLM Proxy)

- The Token Rate Limit policy counts input and output tokens together against one allowance and returns 429 (https://documentation.gravitee.io/gravitee-gamma/agent-management/build/llm-proxies/configure-an-llm-proxy/design/add-the-token-rate-limit-policy.md).
- Settings: static or Expression Language dynamic max tokens, time unit `SECONDS` or `MINUTES`, and a key that defaults to the plan+subscription pair (https://documentation.gravitee.io/gravitee-gamma/agent-management/build/llm-proxies/configure-an-llm-proxy/design/add-the-token-rate-limit-policy.md).
- Strategies: `ASYNC_MODE` (the default; may overshoot), `BLOCK_ON_INTERNAL_ERROR` and `FALLBACK_PASS_THROUGH`. Response headers are `X-Token-Rate-Limit-Limit`, `X-Token-Rate-Limit-Remaining` and `X-Token-Rate-Limit-Reset` (https://documentation.gravitee.io/gravitee-gamma/agent-management/build/llm-proxies/configure-an-llm-proxy/design/add-the-token-rate-limit-policy.md).

### 5.6 Performance claims (vendor, published 2025-03-31, AKS, k6 operator)

| Scenario | Resources | RPS | P95 | P99 | Source |
|---|---|---|---|---|---|
| API key | 500m vCPU / 512 MiB | 6.6k | 1.07 ms | 9.55 ms | https://www.gravitee.io/blog/api-event-stream-management-performance-testing |
| API key | 4 vCPU / 3 GiB | 23.8k | 13.50 ms | 19.80 ms | https://www.gravitee.io/blog/api-event-stream-management-performance-testing |
| API key | 16 vCPU / 3 GiB | 80.8k | 3.84 ms | 9.05 ms | https://www.gravitee.io/blog/api-event-stream-management-performance-testing |
| Rate limit | 16 vCPU / 3 GiB | 61.8k | 4.42 ms | 6.55 ms | https://www.gravitee.io/blog/api-event-stream-management-performance-testing |

## 6. Traefik (Proxy and Hub)

- Traefik Proxy is MIT-licensed, with 64,933 GitHub stars. v3.7.0 shipped on 2026-05-05 and v3.7.13 on 2026-09-04. The 2.11 line still gets patches (v2.11.57 on 2026-09-04) (https://github.com/traefik/traefik; https://github.com/traefik/traefik/releases).
- v3.7.0 moved to `sigs.k8s.io/gateway-api` v1.5.1, added Gateway API filters on HTTP upstreams, service failover in the `TraefikService` CRD, and many ingress-nginx annotation compatibility features (https://github.com/traefik/traefik/releases/tag/v3.7.0).
- Traefik Proxy holds a Gateway API "Gateway Conformance v1.6.1" badge (https://gateway-api.sigs.k8s.io/implementations/).
- Plugins are Go source run by the embedded Yaegi interpreter. They require Traefik v2.3+ and are declared in static config, so changes need a restart (https://plugins.traefik.io/install).
- Traefik Hub v3.21.0-ea.3 (2026-09-14) is built on Traefik Proxy v3.7.11, Coraza v3.7.0 with CRS v4.25.0, and Gateway API v1.6.1 (https://github.com/traefik/hub/releases/tag/v3.21.0-ea.3).
- Hub AI Gateway is "a thin, dedicated control layer on top of Traefik Hub's API Gateway". It is built from Middleware resources: Chat Completion, Messages API, Bedrock Mantle, Google Agent Platform, Semantic Cache, Content Guard, LLM Guard, Parallel LLM Guard, Rate Limit & Quota (token-based, per input/output/total), and Responses API. It adds a `Model(<pattern>)` router matcher (https://doc.traefik.io/traefik-hub/ai-gateway/overview).
- Tiers: Traefik Proxy is free. Hub API Gateway (custom pricing) adds WAF, OPA, Vault and FIPS 140-2/140-3, with AI Gateway and MCP Gateway as add-ons. Hub API Management (custom pricing) adds a central control plane, developer portal, API mocking and air-gapped mode (https://traefik.io/pricing).

## 7. NGINX Gateway Fabric (NGF)

- NGF is Apache-2.0 and had 1,167 GitHub stars at snapshot. v2.7.0 shipped on 2026-09-02, v2.7.1 on 2026-09-15 and v2.7.2 on 2026-09-16 (https://github.com/nginx/nginx-gateway-fabric; https://github.com/nginx/nginx-gateway-fabric/releases).
- v2.7.0 added:
  - support for Gateway API v1.6.1 and v1 TCPRoute/UDPRoute
  - HTTPRoute external authentication via `HTTPExternalAuthFilter`
  - TLSRoute Terminate mode
  - F5 AI Guardrails via a `PayloadProcessor` CRD
  - an `ExternalLoadBalancer` CRD, first used for F5 BIG-IP
  - upstream HTTP/2 (h2c)
  - `least_time` load balancing for NGINX OSS users

  (https://github.com/nginx/nginx-gateway-fabric/blob/v2.7.0/CHANGELOG.md; the `PayloadProcessor` CRD name is from https://github.com/nginx/nginx-gateway-fabric/pull/5697)
- The v2.7.0 fixes mention InferencePool `failureMode` handling, so NGF implements the Gateway API Inference Extension InferencePool (https://github.com/nginx/nginx-gateway-fabric/blob/v2.7.0/CHANGELOG.md). <!-- alias-ok -->
- NGF policy CRDs include ClientSettingsPolicy, UpstreamSettingsPolicy, ObservabilityPolicy, ProxySettingsPolicy, RateLimitPolicy and SnippetsPolicy (https://github.com/nginx/nginx-gateway-fabric/blob/v2.7.0/CHANGELOG.md).
- The compatibility table lists the Edge build at Gateway API 1.6.1 with NGINX OSS 1.31.4 / NGINX Plus R37.1, and 2.6.8 at Gateway API 1.5.1 (https://github.com/nginx/nginx-gateway-fabric/blob/v2.7.0/README.md).
- NGINX Plus features and dedicated support are commercial, with a 30-day trial (https://github.com/nginx/nginx-gateway-fabric/blob/v2.7.0/README.md).

## 8. Kubernetes Gateway API: v1.5, v1.6 and conformance

### 8.1 Release features

| Version | Date (ISO) | Key changes | Source |
|---|---|---|---|
| v1.5.0 | 2026-02-27 | To Standard/GA: client certificate validation (GEP-91, GEP-3567), certificate selection for TLS origination (GEP-3155), ListenerSet (GEP-1713), HTTPRoute CORS filter (GEP-1767), TLSRoute `v1` (GEP-2643); ReferenceGrant to `v1`; new `safe-upgrades.gateway.networking.k8s.io` ValidatingAdmissionPolicy that blocks installing Experimental CRDs over Standard and blocks downgrades below 1.5; Experimental: Gateway/HTTPRoute-level authentication (GEP-1494); TLSRoute CEL needs Kubernetes 1.31+ | https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.5.0 |
| v1.5.1 | 2026-03-14 | Conformance fixes (IPv6, TLSRoute FIN/RST, misdirected-request test limited to h2) | https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.5.1 |
| v1.6.0 | 2026-06-29 | TCPRoute and UDPRoute graduate to GA (`v1`); new GATEWAY-UDP conformance profile; `XBackend` GEP enters Experimental; CA references raised from 8 to 16; TLSRoute up to 1024 hostnames/rules; BackendTLSPolicy usable with other route types; `idleTimeout` removed from experimental SessionPersistence | https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.6.0 |
| v1.6.1 / v1.6.2 | 2026-07-16 / 2026-09-03 | Patch releases; monthly experimental tags (`monthly-2026.04` on 2026-04-08, `monthly-2026.05` on 2026-05-06) also appeared between v1.5.1 and v1.6.0 | https://github.com/kubernetes-sigs/gateway-api/releases |

### 8.2 Conformance landscape (implementations page, snapshot)

| Badge version | Implementations | Source |
|---|---|---|
| Gateway v1.6.2 | Kong Operator | https://gateway-api.sigs.k8s.io/implementations/ |
| Gateway v1.6.1 | Envoy Gateway, NGINX Gateway Fabric, Traefik Proxy, Gravitee Kubernetes Operator, kgateway, Cilium (plus Mesh v1.6.1), AWS Load Balancer Controller <!-- alias-ok -->, Cloudflare tunnel gateway (lexfrei) | https://gateway-api.sigs.k8s.io/implementations/ |
| Gateway v1.6.0 | Istio (plus Mesh v1.6.0), agentgateway, Airlock Microgateway, GKE, Higress | https://gateway-api.sigs.k8s.io/implementations/ |
| Gateway v1.5.x | N42 Gateway, WSO2 Gateway (v1.5.1), Varnish Gateway (v1.5.0) | https://gateway-api.sigs.k8s.io/implementations/ |
| Gateway v1.4.x | Amazon EKS (v1.4.0), Calico, Gloo Gateway (v1.4.1) | https://gateway-api.sigs.k8s.io/implementations/ |

- The upstream `conformance/reports/v1.6` directory holds reports from agentgateway, Airlock, AWS LBC, Buoyant Enterprise for Linkerd, Cilium, Envoy Gateway, GKE, Gravitee, Higress, Istio, kgateway, Kong Operator, the lexfrei Cloudflare tunnel, Linkerd, NGINX Gateway Fabric and Traefik (https://github.com/kubernetes-sigs/gateway-api/tree/main/conformance/reports).
- The `v1.5` directory also includes haproxy-ingress, N42, Varnish and WSO2 (https://github.com/kubernetes-sigs/gateway-api/tree/main/conformance/reports).
- Of the vendors in this file, Envoy Gateway, NGF, Traefik Proxy and the Gravitee Kubernetes Operator all report v1.6.1 conformance (https://gateway-api.sigs.k8s.io/implementations/).
- Zuplo has no Gateway API implementation in the list (https://gateway-api.sigs.k8s.io/implementations/).

## 9. Observations relevant to Ruralz (analysis, not vendor fact)

- Every Kubernetes-native competitor here reached Gateway API v1.6.1 conformance by 2026-09 (https://gateway-api.sigs.k8s.io/implementations/). Ruralz defers conformance (ADR-0016 in the foundation pack), which is a visible gap in comparison tables.
- Token budgets split into two models. The first is cumulative quota keyed by CEL-weighted token types: Agent Router `QuotaPolicy` (https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/). The second is dollar budgets arranged hierarchically: Zuplo gateway/team/app (https://zuplo.com/docs/ai-gateway/usage-limits.md). Gravitee exposes an explicit fail-open/fail-closed strategy on its token limiter (https://documentation.gravitee.io/gravitee-gamma/agent-management/build/llm-proxies/configure-an-llm-proxy/design/add-the-token-rate-limit-policy.md).
- Gravitee's native Kafka gateway, SSE/WebSocket/Webhook entrypoints and LLM/MCP/A2A proxies are all Enterprise-only (https://documentation.gravitee.io/apim/introduction/enterprise-edition.md; https://documentation.gravitee.io/apim/kafka-gateway/configure-the-kafka-client-and-gateway.md). Zuplo self-hosting is Enterprise-only (https://zuplo.com/pricing). Traefik AI Gateway is a paid Hub add-on (https://traefik.io/pricing).

## Sources

https://github.com/envoyproxy/gateway
https://github.com/envoyproxy/gateway/releases
https://github.com/envoyproxy/gateway/releases/tag/v1.9.0
https://github.com/envoyproxy/gateway/releases/tag/v1.8.0
https://gateway.envoyproxy.io/news/releases/matrix/
https://gateway.envoyproxy.io/docs/api/extension_types/
https://gateway.envoyproxy.io/docs/tasks/traffic/global-rate-limit/
https://gateway.envoyproxy.io/docs/tasks/operations/standalone-deployment-mode/
https://gateway.envoyproxy.io/docs/tasks/extensibility/dynamic-modules/
https://gateway.envoyproxy.io/docs/concepts/
https://github.com/theagentrouter/agent-router
https://github.com/theagentrouter/agent-router/releases
https://github.com/theagentrouter/agent-router/releases/tag/v1.0.0
https://github.com/theagentrouter/agent-router/releases/tag/v1.1.0
https://theagentrouter.ai/blog/envoy-ai-gateway-is-now-agent-router/
https://theagentrouter.ai/docs/capabilities/traffic/quota-policy/
https://theagentrouter.ai/docs/capabilities/traffic/usage-based-ratelimiting/
https://theagentrouter.ai/docs/capabilities/traffic/provider-fallback/
https://www.linuxfoundation.org/press/linux-foundation-announces-the-formation-of-the-agentic-ai-foundation
https://tetrate.io/learn/ai/ai-gateway-benchmarks
https://zuplo.com/pricing
https://zuplo.com/api-management.md
https://zuplo.com/ai-gateway.md
https://zuplo.com/enterprise.md
https://zuplo.com/solutions/scale-and-reliable.md
https://zuplo.com/docs/llms.txt
https://zuplo.com/docs/policies/overview
https://zuplo.com/docs/ai-gateway/usage-limits.md
https://zuplo.com/docs/rate-limiting/how-it-works.md
https://zuplo.com/docs/programmable-api/node-modules.md
https://zuplo.com/docs/programmable-api/compatibility-dates.md
https://zuplo.com/docs/self-hosted/overview.md
https://zuplo.com/docs/self-hosted/requirements.md
https://zuplo.com/docs/dedicated/overview.md
https://zuplo.com/docs/articles/monetization.md
https://github.com/zuplo/zudoku
https://github.com/gravitee-io/gravitee-api-management
https://documentation.gravitee.io/apim/4.10/release-information/release-notes/apim-4.10
https://documentation.gravitee.io/apim/release-information/release-notes/apim-4.12
https://documentation.gravitee.io/apim/introduction/enterprise-edition.md
https://documentation.gravitee.io/apim/kafka-gateway.md
https://documentation.gravitee.io/apim/create-and-configure-apis/configure-v4-apis/endpoints/kafka.md
https://documentation.gravitee.io/apim/kafka-gateway/create-and-configure-kafka-apis/configure-kafka-apis/policies.md
https://documentation.gravitee.io/apim/create-and-configure-apis/apply-policies/policy-reference/kafka-message-encryption-decryption-policy-reference.md
https://documentation.gravitee.io/apim/kafka-gateway/configure-the-kafka-client-and-gateway.md
https://documentation.gravitee.io/apim/plugins/customization.md
https://documentation.gravitee.io/apim/create-and-configure-apis/apply-policies/v4-api-policy-studio.md
https://documentation.gravitee.io/gravitee-gamma/platform-management/gamma-release-notes.md
https://documentation.gravitee.io/gravitee-gamma/agent-management/build/llm-proxies/configure-an-llm-proxy/design/add-the-token-rate-limit-policy.md
https://www.gravitee.io/pricing
https://www.gravitee.io/blog/api-event-stream-management-performance-testing
https://github.com/traefik/traefik
https://github.com/traefik/traefik/releases
https://github.com/traefik/traefik/releases/tag/v3.7.0
https://github.com/traefik/hub/releases/tag/v3.21.0-ea.3
https://doc.traefik.io/traefik-hub/ai-gateway/overview
https://traefik.io/pricing
https://plugins.traefik.io/install
https://github.com/nginx/nginx-gateway-fabric
https://github.com/nginx/nginx-gateway-fabric/releases
https://github.com/nginx/nginx-gateway-fabric/blob/v2.7.0/CHANGELOG.md
https://github.com/nginx/nginx-gateway-fabric/blob/v2.7.0/README.md
https://github.com/nginx/nginx-gateway-fabric/pull/5697
https://github.com/kubernetes-sigs/gateway-api/releases
https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.5.0
https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.5.1
https://github.com/kubernetes-sigs/gateway-api/releases/tag/v1.6.0
https://github.com/kubernetes-sigs/gateway-api/tree/main/conformance/reports
https://gateway-api.sigs.k8s.io/implementations/

## Gaps

- **Envoy Gateway v1.9.1 Gateway API version, conflicting sources.** A summary of the GitHub releases list gave Gateway API v1.5.1 for v1.9.1. The compatibility matrix and the v1.9.0 release notes both say v1.6.1. This file uses v1.6.1.
- **Envoy Gateway rate-limit header selector limit.** One reading of the releases page gave a MaxItems increase of 16 to 64, and the v1.9.0 tag notes gave 64 to 128. The exact numbers are left out of the findings.
- **Envoy Gateway HTTP/3.** An `HTTP3Settings` type exists in the API reference, but the HTTP/3 task page could not be fetched, so HTTP/3 maturity is not stated.
- **Envoy Gateway Dynamic Modules** <!-- alias-ok -->. The first version that supported them and the SDK languages were not confirmed. The docs only show local `.so` loading, and v1.8.0 notes mention remote sources.
- **Envoy Gateway performance.** No headline benchmark numbers were extracted. The per-release benchmark reports are binary assets that were not parsed.
- **Agent Router features.** Semantic caching and prompt guardrails were not found in the docs reviewed.
- **Agent Router performance.** The benchmark figures come from Tetrate, which is affiliated with the project, citing Broadcom. The underlying Broadcom report was not read.
- **Agent Router v0.4.0 date.** One source summary gave 2025-11-08. The GitHub API listing used for exact dates covered only v0.5.0 and later.
- **Zuplo policy count.** The figure of 111 comes from crawling policy URLs. It counts inbound and outbound variants separately and may include legacy/v2 duplicates. Zuplo does not publish an official number.
- **Zuplo protocols and engine.** No gRPC support is documented. The JavaScript engine is not named beyond "custom JavaScript engine". The SaaS path runs through Cloudflare.
- **Zuplo Builder pricing.** The row reads "100K Included ($100 per 100K)" and the footnote says Builder caps at 1M requests/mo. How overage is billed between those two figures is not explained.
- **Zuplo latency claim.** The "<50ms" figure has no published methodology.
- **Gravitee 4.12 release date.** The release notes page gives no release date. The date of 2026-06-26 comes from the 4.12.0 tag commit and matches the Gamma release date.
- **Gravitee rate-limit store.** "Hazelcast replaces Redis as an alternative" is ambiguous in the 4.12 notes. This file reads it as Hazelcast added as an alternative store.
- **Gravitee Enterprise Edition list.** The 4.12 EE page lists "Proxy Reactor" and the HTTP GET/POST entrypoints as enterprise-only. That is unexpected for the Apache-2.0 core, and it was not confirmed whether basic HTTP proxying needs a license.
- **Gravitee Gamma licensing.** Whether Gamma has a community edition was not confirmed. The self-hosted install page mentions adding a license key.
- **Gravitee benchmark.** It dates from 2025-03-31 and predates APIM 4.10 through 4.12. The APIM version tested is not stated.
- **Traefik Gateway API version, conflicting sources.** Traefik Proxy v3.7.0 notes bump Gateway API to v1.5.1, but the conformance page shows Traefik Proxy at v1.6.1. The Proxy release that adopted v1.6.1 was not found; Traefik Hub v3.21.0-ea.3 states v1.6.1.
- **Traefik plugins.** WebAssembly plugin support in Traefik Proxy was not confirmed from the plugin catalog page, which only mentions Yaegi.
- **Traefik Hub AI Gateway.** The version that introduced it was not found.
- **Traefik Hub and NGF pricing.** Neither publishes prices.
- **NGF AI features.** No token-based rate limiting is documented for NGF. Its only AI feature found is the F5 AI Guardrails `PayloadProcessor` integration.
- **Gateway API implementations page.** Badge versions were parsed from the page HTML. Some implementations, such as Linkerd, have reports in the repository but no parsed badge.
