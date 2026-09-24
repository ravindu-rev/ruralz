# AI/LLM Gateways and Provider API Facts

| Field | Value |
|---|---|
| Topic | AI/LLM gateways (Portkey, LiteLLM, Kong AI Gateway, Cloudflare AI Gateway, Helicone, Envoy AI Gateway / Agent Router, KrakenD AI Gateway EE, Tyk AI Studio) and upstream provider API facts (OpenAI, Anthropic, Gemini, Bedrock, Mistral, Ollama) |
| Snapshot date | 2026-09-23 |
| Method | Vendor docs, GitHub READMEs, GitHub REST API (`gh api`: releases, licenses, branches), release notes, changelogs, pricing pages and GitHub issues, all read on 2026-09-23. Secondary sources (press coverage, reviews) appear only where noted and are flagged in Gaps. A claim is written as fact only when a primary source states it; anything else goes to Gaps. |

## 1. Gateway landscape: ownership, license, release status

| Gateway | Latest release / status (as of 2026-09-23) | License | Ownership / corporate events | Source |
|---|---|---|---|---|
| Portkey Gateway | `main` last stable tag v1.15.2 (2026-01-12); branch `2.0.0` has `package.json` version 2.2.3, last commit 2026-03-14; `main` last push 2026-05-25 | MIT (repo license API; `package.json` on `2.0.0` also says MIT) | Palo Alto Networks announced it would acquire Portkey on 2026-04-30 and closed the deal on 2026-05-29 | https://api.github.com/repos/Portkey-AI/gateway/releases ; https://api.github.com/repos/Portkey-AI/gateway/license ; https://www.paloaltonetworks.com/company/press/2026/palo-alto-networks-to-acquire-portkey-to-secure-the-rise-of-ai-agents ; https://www.paloaltonetworks.com/company/press/2026/palo-alto-networks-completes-acquisition-of-portkey-to-secure-ai-agents |
| LiteLLM proxy | v1.102.1 stable (2026-09-23); v1.104.0-dev.1 pre-release (2026-09-23) | MIT, except `enterprise/` directory, which uses `enterprise/LICENSE` | BerriAI | https://api.github.com/repos/BerriAI/litellm/releases?per_page=5 ; https://github.com/BerriAI/litellm/blob/main/LICENSE |
| Kong AI Gateway | AI Gateway 3.14 (blog dated 2026-04-14); the `Kong/kong` GitHub repo's latest release is 3.9.3 (2026-06-17) | AI plugins used here are "AI Gateway Enterprise" tier (see §5–§8) | Kong Inc. | https://konghq.com/blog/product-releases/kong-ai-gateway-3-14 ; https://github.com/Kong/kong/releases |
| Cloudflare AI Gateway | SaaS; latest changelog entry 2026-09-14 | Proprietary SaaS | Cloudflare | https://developers.cloudflare.com/ai-gateway/changelog/ |
| Helicone | Maintenance mode since the Mintlify acquisition (2026-03-03); main repo last push 2026-09-16; Rust `Helicone/ai-gateway` repo last push 2025-11-21, last release v0.2.0-beta.30 (2025-07-21) | `Helicone/helicone`: Apache-2.0; `Helicone/ai-gateway`: GPL-3.0 | Acquired by Mintlify | https://www.helicone.ai/blog/joining-mintlify ; https://github.com/Helicone/helicone ; https://github.com/Helicone/ai-gateway |
| Envoy AI Gateway (renamed Agent Router) | v1.1.0 (2026-08-21); v1.0.0 GA (2026-06-23) | Apache-2.0 | Repo is now `theagentrouter/agent-router`, "Formerly Envoy AI Gateway — now an Agentic AI Foundation project" | https://github.com/envoyproxy/ai-gateway/releases ; https://theagentrouter.ai/release-notes/ |
| KrakenD AI Gateway | Enterprise Edition only; AI Gateway introduced in EE 2.10 (2025-06-04), vendor integrations in EE 2.11, MCP in EE 2.12 | Commercial EE license | KrakenD | https://www.krakend.io/blog/krakend-ee-2.10-release-notes/ ; https://www.krakend.io/blog/krakend-ee-2.11-release-notes/ ; https://www.krakend.io/blog/krakend-ee-2.12-release-notes/ |
| Tyk AI Studio | Tags up to v2.2.0-rc9.5; latest GitHub "release" object is v1.0.5 (2025-11-19); repo pushed 2026-09-23 | AGPL-3.0 (Community Edition) | Tyk; open-sourced on 2026-03-12 | https://github.com/TykTechnologies/ai-studio ; https://tyk.io/blog/ai-studio-is-going-open-source-and-why-the-ai-control-plane-must-be-extensible/ |

### 1.1 Portkey details

- Portkey announced on 2026-03-24 that its Gateway was "fully open source". The release named the unified Gateway, usage policies, model catalog, control-plane connection, real-time metrics, MCP registry and OAuth 2.1/2.0 authentication (https://www.globenewswire.com/news-release/2026/03/24/3261574/0/en/portkey-s-gateway-is-now-fully-open-source-processing-over-1-trillion-tokens-every-day.html).
- Portkey's own claimed volume in that release: 1T+ tokens/day, 120M+ requests/day, $180M+ annualized AI spend managed, 24,000+ organizations (https://www.globenewswire.com/news-release/2026/03/24/3261574/0/en/portkey-s-gateway-is-now-fully-open-source-processing-over-1-trillion-tokens-every-day.html).
- The README describes "Gateway 2.0 (Pre-Release)", which merges the "core enterprise gateway into open-source", routes to 1,600+ models across 45+ providers, ships 40+ pre-built guardrails and an MCP Gateway, and claims "<1ms latency" and a "122kb" footprint (no hardware context given) (https://github.com/Portkey-AI/gateway).
- Under Palo Alto Networks, Portkey becomes "the core AI Gateway for Prisma AIRS". Financial terms were not disclosed (https://www.paloaltonetworks.com/company/press/2026/palo-alto-networks-completes-acquisition-of-portkey-to-secure-ai-agents).
- At announcement, the deal was expected to close in PANW's fiscal Q4 2026. Neither PANW press release mentions open source (https://www.paloaltonetworks.com/company/press/2026/palo-alto-networks-to-acquire-portkey-to-secure-the-rise-of-ai-agents).
- The `2.0.0` branch has no LICENSE file at its root, but its `package.json` declares `"license": "MIT"` (https://github.com/Portkey-AI/gateway/tree/2.0.0).

### 1.2 LiteLLM details

- Stable releases ship weekly (typically Sunday), and each one bumps the minor version. The support policy, flagged as changing on 2026-05-18, covers only the latest stable, plus up to 90 days on the prior stable after a major-version change. The page names no supported version range (https://docs.litellm.ai/docs/proxy/release_cycle).
- Virtual keys need PostgreSQL via `DATABASE_URL` and a master key (prefix `sk-`) set in `general_settings.master_key` or `LITELLM_MASTER_KEY`. Keys are created with `POST /key/generate` (https://docs.litellm.ai/docs/proxy/virtual_keys).
- Budget and limit fields are `max_budget` (USD), `budget_duration`, `tpm_limit` and `rpm_limit`. They can be set on a key, user or team, and keys inherit their owner's `tpm_limit` and `rpm_limit` (and `max_budget` when the key has no team) unless the same fields are set on the key (https://docs.litellm.ai/docs/proxy/virtual_keys).
- Spend is computed with `completion_cost()` and written to the `LiteLLM_SpendLogs` table (hashed key, user, team, tags, model, tokens, spend). It can be read from `/key/info`, `/user/info` and `/team/info` (https://docs.litellm.ai/docs/proxy/virtual_keys ; https://docs.litellm.ai/docs/proxy/cost_tracking).
- The pricing source is `model_prices_and_context_window.json` on GitHub. Per-deployment overrides use `input_cost_per_token` and `output_cost_per_token`, and a custom model cost map is supported (https://docs.litellm.ai/docs/proxy/cost_tracking).
- Fallback classes are `fallbacks` (general errors such as RateLimitError), `context_window_fallbacks` and `content_policy_fallbacks`. Related settings are `num_retries`, `allowed_fails` and `cooldown_time` (https://docs.litellm.ai/docs/proxy/reliability).
- Enterprise features listed: SSO, audit logs, RBAC and SLA support (https://docs.litellm.ai/docs/enterprise).

## 2. Unified API surface: OpenAI-compatible façade vs native passthrough

| Gateway | OpenAI-compatible façade | Native passthrough / native formats | Source |
|---|---|---|---|
| LiteLLM | Yes (`/chat/completions` across providers) | `/v1/messages` Anthropic surface, which bridges to the Responses API for `mode: responses` models (see the §6 bug) | https://github.com/BerriAI/litellm/issues/41424 |
| Portkey | Yes (`/v1/chat/completions`) | `/v1/messages` (Anthropic format) | https://github.com/Portkey-AI/gateway/issues/1579 |
| Kong AI Proxy | Default `llm_format` is OpenAI. Route types include `llm/v1/chat`, `llm/v1/completions`, `llm/v1/embeddings`, `llm/v1/responses`, `llm/v1/assistants`, audio, image and video | `config.llm_format` accepts native `anthropic`, `bedrock`, `cohere`, `gemini` (also used for Vertex AI) and `huggingface` formats (native formats need AI Gateway 3.10+); plugin baseline Kong Gateway 3.6 | https://developer.konghq.com/plugins/ai-proxy/ |
| Envoy AI Gateway / Agent Router | "single OpenAI-compatible API across 16 providers with cross-provider translation" (v1.0) | v1.1 adds Anthropic-format `/anthropic/v1/models` and `/anthropic/v1/messages/count_tokens`; v0.6 added the Anthropic endpoint on OpenAI backends and Bedrock InvokeModel for Claude | https://theagentrouter.ai/release-notes/ ; https://github.com/envoyproxy/ai-gateway/releases |
| Cloudflare AI Gateway | Unified REST endpoints launched 2026-05-21: "OpenAI-compatible, Anthropic, and universal formats" | Provider-specific endpoints | https://developers.cloudflare.com/ai-gateway/changelog/ |
| KrakenD EE | "Unified LLM interface" with transformations | "No-op" backends for direct vendor passthrough. Namespace `ai/llm` with per-vendor keys (`openai`, `anthropic`, `gemini` in the examples; Mistral also has an integration page) | https://www.krakend.io/docs/enterprise/ai-gateway/llm-routing/ |
| Tyk AI Studio | LLM proxy gateway with multi-vendor support (OpenAI, Anthropic, Mistral, Vertex AI, Bedrock, Gemini, Hugging Face, Ollama) | Not documented on README | https://github.com/TykTechnologies/ai-studio |

Provider-side OpenAI-compatible endpoints (useful for a façade):

- Gemini: `https://generativelanguage.googleapis.com/v1beta/openai/`, labelled beta. It supports `stream_options.include_usage`, and for image generation, parameters not listed on the page are "silently ignored" (https://ai.google.dev/gemini-api/docs/openai).
- Ollama: `http://localhost:11434/v1/` serves `/v1/chat/completions`, `/v1/completions`, `/v1/models`, `/v1/embeddings` and a non-stateful `/v1/responses`. It supports `include_usage`, and the local server ignores the API key (https://docs.ollama.com/api/openai-compatibility).
- Ollama also serves a subset of the Anthropic Messages API at `/v1/messages`. `cache_control` is listed as not supported, and token counts are "approximations" (https://docs.ollama.com/api/anthropic-compatibility).
- Bedrock serves the OpenAI Responses API on both the `bedrock-runtime` and `bedrock-mantle` endpoints for OpenAI models (https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html).

## 3. Provider matrix

| Dimension | OpenAI | Anthropic | Gemini (Developer API) | Bedrock | Mistral | Ollama |
|---|---|---|---|---|---|---|
| Base URL shape | `https://api.openai.com/v1/...` (Chat Completions, Responses) | `https://api.anthropic.com/v1/messages` | `https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent` and `:streamGenerateContent?alt=sse` | `https://bedrock-runtime.{region}.amazonaws.com/model/{modelId}/converse` and `/converse-stream` | `https://api.mistral.ai/v1/chat/completions` | `http://localhost:11434/api/chat` (native); `/v1/` (OpenAI-compatible) |
| Auth header | `Authorization: Bearer` | `x-api-key` + `anthropic-version: 2023-06-01` | `x-goog-api-key` header or `?key=` query | AWS SigV4, or `Authorization: Bearer <Bedrock API key>` (short-term ≤12 h or long-term) | `Authorization: Bearer` | None on local server (`security: []`) |
| Streaming format | SSE `data:` chunks, terminated by `data: [DONE]` (Chat Completions); typed SSE events (Responses) | SSE with named events: `message_start`, `content_block_*`, `message_delta`, `message_stop`, `ping`, `error` | SSE with `alt=sse` | AWS event stream: `messageStart`, `contentBlockStart/Delta/Stop`, `messageStop`, `metadata` | SSE, `data: [DONE]` terminator | NDJSON, `stream` defaults to `true` |
| Usage in stream | Chat Completions: only with `stream_options.include_usage`, as an extra chunk before `[DONE]` with `choices: []`; other chunks carry `usage: null`. Responses: in `response.completed`, under `response.usage` | `message_start.message.usage` (input and cache fields); `message_delta.usage`, whose counts are cumulative | `usageMetadata` (`promptTokenCount`, `candidatesTokenCount`, `totalTokenCount`, `cachedContentTokenCount`, `thoughtsTokenCount`, `toolUsePromptTokenCount`) | `metadata` event: `usage.inputTokens`, `outputTokens`, `totalTokens`, `cacheReadInputTokens`, `cacheWriteInputTokens`, `cacheDetails[]`; `metrics.latencyMs` | `usage` in final message | Final object with `done: true`: `prompt_eval_count`, `eval_count`, plus `*_duration` fields in ns |
| Prompt cache | Automatic. `usage.input_tokens_details.cached_tokens` and `cache_write_tokens`; `prompt_cache_key`; GPT-5.6+ uses `prompt_cache_options.ttl` (`30m`) | Explicit `cache_control` (up to 4 breakpoints) or top-level automatic `cache_control`; TTL `5m` or `1h` | Implicit caching (default on 2.5+) plus explicit `cachedContents` resource | Converse `cachePoint` / InvokeModel `cache_control` for Claude; `prompt_cache_breakpoint` for GPT-5.6 | `prompt_cache_key`; cached tokens billed at 10% | `cache_control` not supported on `/v1/messages` |
| Rate-limit headers | `x-ratelimit-{limit,remaining,reset}-{requests,tokens}`, project-token variants, `Retry-After` | `anthropic-ratelimit-{requests,tokens,input-tokens,output-tokens}-{limit,remaining,reset}` (reset in RFC 3339), priority-tier variants, `retry-after` | None documented; 429 `RESOURCE_EXHAUSTED`; limits are per project | 429 `ThrottlingException` (no headers documented) | Unverified (see Gaps) | N/A |
| Tokenizer | `tiktoken` (MIT, v0.14.0, 2026-08-17) | No public tokenizer; `POST /v1/messages/count_tokens` (free, estimate) | `models/{model}:countTokens`; ~4 chars/token | Via Converse `/tokenize` in Envoy AI Gateway (gateway-side); native count API not verified | `mistral-common` (Apache-2.0, v1.12.0, 2026-09-22; Tekken tokenizers) | Model-local; counts returned in response |

Sources per column:
- OpenAI: https://github.com/openai/openai-python/blob/main/src/openai/types/chat/chat_completion_stream_options_param.py ; https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_completed_event.py ; https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_usage.py ; https://developers.openai.com/api/docs/guides/rate-limits ; https://developers.openai.com/api/docs/guides/prompt-caching ; https://github.com/openai/tiktoken
- Anthropic: https://platform.claude.com/docs/en/build-with-claude/streaming ; https://platform.claude.com/docs/en/api/rate-limits ; https://platform.claude.com/docs/en/build-with-claude/prompt-caching ; https://platform.claude.com/docs/en/build-with-claude/token-counting
- Gemini: https://ai.google.dev/api/generate-content ; https://ai.google.dev/gemini-api/docs/caching ; https://ai.google.dev/api/caching ; https://ai.google.dev/gemini-api/docs/rate-limits ; https://ai.google.dev/gemini-api/docs/tokens
- Bedrock: https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ConverseStream.html ; https://docs.aws.amazon.com/bedrock/latest/userguide/api-keys.html ; https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html
- Mistral: https://docs.mistral.ai/api/ ; https://github.com/mistralai/mistral-common
- Ollama: https://docs.ollama.com/api/chat ; https://docs.ollama.com/api/openai-compatibility ; https://docs.ollama.com/api/anthropic-compatibility

### 3.1 Provider details that affect gateway accounting

- OpenAI: with `include_usage`, "If the stream is interrupted, you may not receive the final usage chunk" (https://github.com/openai/openai-python/blob/main/src/openai/types/chat/chat_completion_stream_options_param.py).
- OpenAI: stream obfuscation (`include_obfuscation`) is on by default and adds an `obfuscation` field to streaming deltas (https://github.com/openai/openai-python/blob/main/src/openai/types/chat/chat_completion_stream_options_param.py).
- Anthropic: "The token counts shown in the `usage` field of the `message_delta` event are *cumulative*." A gateway must therefore take the last value, not sum the deltas (https://platform.claude.com/docs/en/build-with-claude/streaming).
- Anthropic: in a server-tool example, the final `message_delta` carries `input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens` and `server_tool_use.web_search_requests` (https://platform.claude.com/docs/en/build-with-claude/streaming).
- Anthropic: total input = `cache_read_input_tokens + cache_creation_input_tokens + input_tokens` (https://platform.claude.com/docs/en/build-with-claude/prompt-caching).
- Anthropic: `cache_read_input_tokens` do not count toward ITPM on most models; Claude Haiku 3.5 is the exception (https://platform.claude.com/docs/en/api/rate-limits).
- Anthropic: rate limits use a token bucket. ITPM is estimated at request start and then adjusted, while OTPM is evaluated in real time and `max_tokens` does not factor in (https://platform.claude.com/docs/en/api/rate-limits).
- Anthropic: a spend-cap 429 has `error.details.error_code = enforced_spend_limit_reached` and no `retry-after` header (https://platform.claude.com/docs/en/api/rate-limits).
- Anthropic: Claude Opus 4.7 and later use a newer tokenizer that produces about 30% more tokens for the same text (https://platform.claude.com/docs/en/build-with-claude/token-counting).
- Anthropic: the token-counting endpoint has its own RPM limits (Start 5,000 / Build 10,000 / Scale 20,000) (https://platform.claude.com/docs/en/build-with-claude/token-counting).
- Bedrock: when caching is active, `inputTokens` covers only non-cached tokens. Total = `inputTokens + cacheReadInputTokens + cacheWriteInputTokens` (https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html).
- Bedrock: ConverseStream errors include `modelStreamErrorException` (424) and `throttlingException` (429) (https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ConverseStream.html).
- Bedrock: the IAM action `bedrock:CallWithBearerToken` (and `bedrock-mantle:CallWithBearerToken`) controls API-key use (https://docs.aws.amazon.com/bedrock/latest/userguide/api-keys.html).

## 4. Streaming usage accounting in gateways

| Gateway | Mechanism | Source |
|---|---|---|
| Envoy AI Gateway / Agent Router | Extracts token usage "from LLM responses that follow the OpenAI schema format" into metadata that `llmRequestCosts` names. Cost types: `InputToken`, `CachedInputToken`, `OutputToken`, `TotalToken`, `CEL` | https://theagentrouter.ai/docs/capabilities/traffic/usage-based-ratelimiting/ |
| Envoy AI Gateway v1.1 | `streamIdleTimeout`: if it fires before the first token, a retry policy can fail over; mid-stream it returns 504 | https://github.com/envoyproxy/ai-gateway/releases |
| Kong AI Rate Limiting Advanced | Uses provider-returned token data. "The cost ... is only reflected during the next request." | https://developer.konghq.com/plugins/ai-rate-limiting-advanced/ |
| LiteLLM | Spend written to DB after each call; cost docs do not describe streaming separately | https://docs.litellm.ai/docs/proxy/cost_tracking |

## 5. Token-aware rate limiting and budgets

| Gateway | Algorithm / window | Unit | Enforcement point | State backend | Source |
|---|---|---|---|---|---|
| Envoy AI Gateway / Agent Router | Envoy Gateway Global Rate Limit via `BackendTrafficPolicy` | Tokens (input, cached input, output, total, CEL-weighted) | Checks usage already charged before admitting; charges the actual usage after the response completes; 429 when over the limit | Redis | https://theagentrouter.ai/docs/capabilities/traffic/usage-based-ratelimiting/ |
| Kong `ai-rate-limiting-advanced` (Enterprise) | `window_type` `sliding` (default) or `fixed` | `total_tokens`, `prompt_tokens`, `completion_tokens`, `cost` (3.8+) | Post-response; applied on the next request | `local`, `cluster` (Kong DB), `redis` | https://developer.konghq.com/plugins/ai-rate-limiting-advanced/ ; https://developer.konghq.com/plugins/ai-rate-limiting-advanced/reference/ |
| Kong 3.14 | Global token budgets with per-model sub-caps (example: 1M tokens/day global, 200K for one model) | Tokens | — | — | https://konghq.com/blog/product-releases/kong-ai-gateway-3-14 |
| LiteLLM | `tpm_limit` / `rpm_limit` per key/user/team; `max_budget` + `budget_duration` | Tokens, requests, USD | — | PostgreSQL (required) | https://docs.litellm.ai/docs/proxy/virtual_keys |
| Cloudflare AI Gateway | Fixed or sliding window (`rate_limiting_technique`) | Requests only | 429 | Cloudflare-managed | https://developers.cloudflare.com/ai-gateway/features/rate-limiting/ |
| Cloudflare AI Gateway | Spend limits (2026-06-05): "cost-based budgets that track cumulative dollar spend and block requests" | USD | — | — | https://developers.cloudflare.com/ai-gateway/changelog/ |
| Helicone | `Helicone-RateLimit-Policy: [quota];w=[seconds≥60];u=[request\|cents];s=[user\|property]` | Requests or cents | 429; returns `Helicone-RateLimit-Limit`, `-Remaining` and `-Policy` | Helicone-managed | https://docs.helicone.ai/features/advanced-usage/custom-rate-limits |
| KrakenD EE | `governance/quota` with tiers (`tier_key`, `tier_value`), `weight_key`, `weight_strategy: body` | Weighted (tokens) over hourly/daily/monthly/yearly windows | — | Redis | https://www.krakend.io/docs/enterprise/ai-gateway/budget-control/ ; https://www.krakend.io/blog/krakend-ee-2.10-release-notes/ |
| Tyk AI Studio | Rate limiting and budget controls; budget management/enforcement/alerts are Enterprise Edition | — | — | — | https://github.com/TykTechnologies/ai-studio |

## 6. Prompt-caching passthrough and known bugs

### 6.1 Provider caching mechanics

| Provider | Trigger | TTL | Min tokens | Pricing | Source |
|---|---|---|---|---|---|
| Anthropic | `cache_control: {type: "ephemeral", ttl}` on blocks (max 4) or once at top level (automatic) | `5m` default, `1h` | 512 (Opus 5.5, Opus 5, Fable 5.x, Mythos 5.x); 1,024 (Opus 4.8, Sonnet 5, Sonnet 4.5/4.6); 2,048 (Opus 4.7); 4,096 (Opus 4.5/4.6, Haiku 4.5) | Write 1.25× (5m) or 2× (1h); read 0.1× (0.05× Opus 5.5; 0.025× Fable/Mythos 5.1) | https://platform.claude.com/docs/en/build-with-claude/prompt-caching |
| OpenAI | Automatic; `prompt_cache_key` optional | `in_memory` (~5–10 min) or `24h` on earlier models; GPT-5.6+ "at least 30 minutes" via `prompt_cache_options.ttl: "30m"` | 1,024 visible input tokens (GPT-5.6+) | Cached reads at 0.1× input; GPT-5.6+ cache writes at 1.25× input | https://developers.openai.com/api/docs/guides/prompt-caching |
| Gemini | Implicit (default for 2.5+); explicit `POST /v1beta/cachedContents`, referenced via `cachedContent` | Explicit: `ttl` or `expireTime` | Implicit: 4,096 (3.x Flash, 3.1 Pro); 2,048 (2.5 Flash/Pro) | Savings passed through automatically (implicit) | https://ai.google.dev/gemini-api/docs/caching ; https://ai.google.dev/api/caching |
| Bedrock (Claude) | Converse `cachePoint: {type: "default", ttl}`; InvokeModel `cache_control`; max 4 checkpoints in `system`, `messages`, `tools` | 5m, 1h | Per model, same as Anthropic (for example Opus 5: 512; Haiku 4.5: 4,096) | Per Bedrock pricing | https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html |
| Bedrock (GPT-5.6) | `prompt_cache_breakpoint: {mode: "explicit"}`; `prompt_cache_options.mode` `implicit` or `explicit` | 30m minimum | 1,024 | Write 1.25×; read 90% discount | https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html |
| Mistral | `prompt_cache_key` | — | — | Cached tokens at 10% | https://docs.mistral.ai/api/ |

### 6.2 Gateway cache passthrough and documented bugs

| Gateway | Issue | Status | Source |
|---|---|---|---|
| Portkey | #1579 "cache_control blocks stripped when routing to Vertex AI Anthropic models" affects `/v1/messages` and `/v1/chat/completions` to Claude Sonnet 4.6 and Opus 4.6 on Vertex; opened 2026-03-25 | Open (triage) | https://github.com/Portkey-AI/gateway/issues/1579 |
| LiteLLM | #41424: the Anthropic `/v1/messages` to Responses bridge recognizes only `prompt_cache_breakpoint`, drops `cache_control`, and flattens the system prompt. Result: zero cache reads while still paying 1.25× writes. Reported on v1.100.0, opened 2026-09-16 | Open | https://github.com/BerriAI/litellm/issues/41424 |
| LiteLLM | #23149: Vertex AI rejects a `scope` field in `cache_control`. The strip was added for Bedrock and Azure in PR #22867, and Vertex was fixed in PR #23183; opened 2026-03-09 | Closed | https://github.com/BerriAI/litellm/issues/23149 |
| LiteLLM | #19984: Vertex Anthropic passthrough fails with the `prompt-caching-scope-2026-01-05` beta header; #14293: an invalid cache header on the Vertex route | Closed (both) | https://github.com/BerriAI/litellm/issues/19984 ; https://github.com/BerriAI/litellm/issues/14293 |
| Coder AI gateway | PR #29563: the chat-completions interceptor rebuilt bodies from typed openai-go params that have no `cache_control` field, so markers were dropped for OpenAI-compatible upstreams | Open PR (not merged) | https://github.com/coder/coder/pull/29563 |
| Cloudflare AI Gateway | Custom costs support `per_cache_read_token` and `per_cache_write_token` (2026-09-09) | Shipped | https://developers.cloudflare.com/ai-gateway/changelog/ |
| Envoy AI Gateway | v0.5.0 (2026-01-23) added prompt-caching cost savings; v0.6.0 added Gemini context caching; `CachedInputToken` is a rate-limit cost type | Shipped | https://theagentrouter.ai/release-notes/ ; https://theagentrouter.ai/docs/capabilities/traffic/usage-based-ratelimiting/ |

Common root cause in the verified cases: a translating layer rebuilds the upstream request from a typed schema that has no field for `cache_control` or breakpoints, and drops the unknown field (https://github.com/coder/coder/pull/29563 ; https://github.com/BerriAI/litellm/issues/41424).

## 7. Response and semantic caching backends

| Gateway | Exact cache | Semantic cache | Backends | Source |
|---|---|---|---|---|
| Cloudflare AI Gateway | Yes: SHA-256 over provider, endpoint, model, auth header and body; headers `cf-aig-cache-ttl` (60 s to 1 month), `cf-aig-skip-cache`, `cf-aig-cache-key` | Not available ("plan on adding") | Cloudflare | https://developers.cloudflare.com/ai-gateway/features/caching/ |
| Kong `ai-semantic-cache` (Enterprise) | Yes | Yes | Redis (vector search), Redis Cloud, Valkey (3.14+), PostgreSQL pgvector (3.10+), ElastiCache, Azure and Memorystore Redis; returns `X-Cache-Status: Hit\|Miss\|Bypass\|Refresh` | https://developer.konghq.com/plugins/ai-semantic-cache/ |
| LiteLLM | `redis` (Redis/Valkey), `s3`, `gcs`, `local`, `disk` | `redis-semantic`, `valkey-semantic`, `qdrant-semantic` | as listed | https://docs.litellm.ai/docs/proxy/caching |
| Portkey | Yes: README "Smart caching" "Supports simple and semantic* caching" | Asterisked "Available in hosted and enterprise versions" (not the OSS build) | — | https://github.com/Portkey-AI/gateway |

Vector store facts relevant to a Redis/Valkey State Store:

- Redis Vector Sets have been available since Redis 8.0.0 (each command is tagged v8.0.0; the page shows no Beta label). They use HNSW. Commands are `VADD`, `VSIM` (`WITHSCORES`, `COUNT`, `FILTER`), `VREM`, `VCARD`, `VDIM`, `VEMB`, `VSETATTR`, `VGETATTR`, `VINFO`, `VLINKS`, `VRANDMEMBER`, `VRANGE` and `VISMEMBER`. Quantization options are `Q8`, `BIN` and `NOQUANT`. Binary FP32 input must be little-endian (https://redis.io/docs/latest/develop/data-types/vector-sets/).
- The latest Redis release is 8.10.2 (2026-09-17). Redis 8+ is tri-licensed RSALv2 / SSPLv1 / AGPLv3, and 7.2 and earlier remain BSD-3 (https://github.com/redis/redis/releases ; https://github.com/redis/redis/blob/unstable/LICENSE.txt).
- valkey-search is BSD-3-Clause. Its latest releases are 1.2.1 (2026-07-07), 1.1.1 and 1.0.3 (2026-07-15). It supports HNSW and exact KNN, numeric/tag/full-text hybrid queries, `FT.CREATE`, `FT.SEARCH` and `FT.AGGREGATE`, in standalone and cluster modes (https://github.com/valkey-io/valkey-search ; https://github.com/valkey-io/valkey-search/releases).
- valkey-search claims "billions of vectors with over 99% recall" and single-digit ms latency, with no hardware context given (https://github.com/valkey-io/valkey-search).

## 8. Guardrails (PII, prompt injection, content safety)

| Gateway | Guardrail features | Tier | Source |
|---|---|---|---|
| LiteLLM | Modes `pre_call`, `post_call`, `during_call`, `logging_only`; providers Presidio (PII), Lakera, Aporia, Bedrock Guardrails, Guardrails AI, Azure Content Safety, OpenAI Moderation, Cato Networks, generic API; `default_on` | Per-key guardrail control and tag-based modes are Enterprise | https://docs.litellm.ai/docs/proxy/guardrails/quick_start |
| Kong `ai-sanitizer` | PII detection with placeholder or synthetic replacement; needs the Kong AI PII Anonymizer Service container; 9 languages; restores originals in responses (3.12+); min 3.10 | AI Gateway Enterprise | https://developer.konghq.com/plugins/ai-sanitizer/ |
| Kong 3.14 | `ai-custom-guardrail` for third-party systems (for example NVIDIA NeMo Guardrails); standardized guardrail analytics | — | https://konghq.com/blog/product-releases/kong-ai-gateway-3-14 |
| Cloudflare AI Gateway | Evaluates prompts and responses (violence, hate, sexual content); flag or block | Billed at Workers AI inference rates; DLP free (full profiles need Zero Trust) | https://developers.cloudflare.com/ai-gateway/features/guardrails/ ; https://developers.cloudflare.com/ai-gateway/reference/pricing/ |
| Portkey | 40+ pre-built guardrails | OSS (README) | https://github.com/Portkey-AI/gateway |
| Envoy AI Gateway | Request/response body redaction (v0.6.0) | OSS | https://theagentrouter.ai/release-notes/ |
| Bedrock (provider-side) | `guardrailConfig` on Converse with PII entities, regexes, topics, word and content policies, reported in `metadata.trace.guardrail` | AWS | https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ConverseStream.html |
| Tyk AI Studio | Content filtering ("scriptable policy enforcement"); custom content-policy plugins run as isolated gRPC processes; the blog says proprietary guardrails can be implemented | CE | https://github.com/TykTechnologies/ai-studio ; https://tyk.io/blog/ai-studio-is-going-open-source-and-why-the-ai-control-plane-must-be-extensible/ |

## 9. MCP and A2A gateway features

| Gateway | MCP | A2A | Source |
|---|---|---|---|
| Envoy AI Gateway / Agent Router | `MCPRoute` and `MCPRouteSecurityPolicy`; v1beta1 CRDs in v0.6; v1.1 adds `MCPRoute.spec.hostnames` (max 16) and a CEL `backendSelector` evaluated at initialize (default Deny); MCP Go SDK v1.7.0 | Not found in release notes | https://github.com/envoyproxy/ai-gateway/releases ; https://theagentrouter.ai/release-notes/ |
| Kong | `ai-mcp-proxy` (Enterprise, 3.12+) with modes passthrough-listener, conversion-listener, conversion-only and listener, plus default and per-tool ACLs; `ai-mcp-oauth2` (JWK validation) and RFC 8693 token exchange in 3.14 | `ai-a2a-proxy` in 3.14 | https://developer.konghq.com/plugins/ai-mcp-proxy/ ; https://konghq.com/blog/product-releases/kong-ai-gateway-3-14 |
| LiteLLM | Streamable HTTP, SSE and stdio servers; permissions by key/team/org; OAuth 2.0 (PKCE, client credentials), SigV4; REST `/mcp-rest/tools/list` and `/mcp-rest/tools/call` | A2A Agent Gateway (introduced v1.80.8); a2a-sdk 1.x, serves A2A 0.3 or 1.0 wire format per agent; `message/send` and `message/stream` go through logging, guardrails and spend | https://docs.litellm.ai/docs/mcp ; https://docs.litellm.ai/docs/a2a ; https://docs.litellm.ai/release_notes/v1.80.8-stable/v1-80-8 |
| Portkey | MCP Gateway and MCP registry with OAuth 2.1 | Not verified | https://www.globenewswire.com/news-release/2026/03/24/3261574/0/en/portkey-s-gateway-is-now-fully-open-source-processing-over-1-trillion-tokens-every-day.html |
| KrakenD EE | EE 2.12: MCP Gateway (proxy to existing MCP servers) and MCP Server (auto-generates tools from KrakenD Routes) | Not found | https://www.krakend.io/blog/krakend-ee-2.12-release-notes/ |
| Tyk AI Studio | MCP integration with remote and local servers | Not found | https://github.com/TykTechnologies/ai-studio |
| Cloudflare AI Gateway | Not found in docs read | Not found | https://developers.cloudflare.com/ai-gateway/ |

## 10. OpenTelemetry `gen_ai` semantic conventions status

- In semantic-conventions v1.42.0 (released 2026-06-12), all `gen_ai.*` attributes, metrics, events and spans (plus `model/openai/` and `model/mcp/`) were deprecated there and moved to `open-telemetry/semantic-conventions-genai` (https://github.com/open-telemetry/semantic-conventions/releases/tag/v1.42.0).
- The new repo was created on 2026-05-05, is Apache-2.0, has no tagged releases as of 2026-09-23, and its README lists the Schema URL as "TODO" (https://github.com/open-telemetry/semantic-conventions-genai).
- Stability: `gen-ai-spans.md`, `gen-ai-metrics.md` and `gen-ai-token-metrics.md` are all marked **Development** (https://github.com/open-telemetry/semantic-conventions-genai/tree/main/docs/gen-ai).
- Provider pages cover OpenAI, Anthropic, AWS Bedrock and Azure AI Inference, alongside `mcp.md` and `gen-ai-agent-spans.md` (https://github.com/open-telemetry/semantic-conventions-genai/tree/main/docs/gen-ai).
- Span attributes include `gen_ai.provider.name`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.usage.cache_read.input_tokens`, `gen_ai.usage.cache_write.input_tokens` and `gen_ai.usage.reasoning.output_tokens`, plus modality splits (`gen_ai.usage.text.*`, `.image.*`, `.audio.*`) (https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-spans.md).
- Opt-in content attributes: `gen_ai.input.messages`, `gen_ai.output.messages`, `gen_ai.system_instructions` and `gen_ai.tool.definitions` (https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-spans.md).
- Client metrics: `gen_ai.client.operation.duration`, `gen_ai.client.operation.time_to_first_chunk` and `gen_ai.client.operation.time_per_output_chunk` (https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-metrics.md).
- Server metrics: `gen_ai.server.request.duration`, `gen_ai.server.time_to_first_token` and `gen_ai.server.time_per_output_token` (https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-metrics.md).
- Agent, workflow and tool metrics: `gen_ai.invoke_agent.duration`, `gen_ai.invoke_workflow.duration` and `gen_ai.execute_tool.duration` (https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-metrics.md).
- Token metrics now live in `gen-ai-token-metrics.md`: `gen_ai.client.inference.usage.input_tokens`, `.output_tokens`, `.cache_read.input_tokens`, `.cache_write.input_tokens` and `.reasoning.output_tokens`, plus `gen_ai.client.inference.operation.input_tokens` and `.output_tokens`. The older `gen_ai.client.token.usage` metric does not appear in the current metrics docs (https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-token-metrics.md).
- Gateway adoption: Envoy AI Gateway v1.1.0 emits `gen_ai.*` span attributes when `AI_GATEWAY_TRACING_SEMCONV=gen_ai` is set and ships a Grafana dashboard for `gen_ai_*` Prometheus metrics (https://github.com/envoyproxy/ai-gateway/releases).

## 11. Cost tracking and pricing-table handling

| Gateway | Pricing source | Cache-token pricing | Source |
|---|---|---|---|
| LiteLLM | `model_prices_and_context_window.json` (GitHub; sync recommended); per-deployment `input_cost_per_token` and `output_cost_per_token`; provider-tier-aware costs (Vertex PayGo, Bedrock service tiers) | Not stated on the cost page | https://docs.litellm.ai/docs/proxy/cost_tracking |
| Cloudflare AI Gateway | Provider pricing passed through "with no markup"; Unified Billing has a 5% fee on credit purchases; custom costs via headers | `per_cache_read_token` and `per_cache_write_token` (2026-09-09) | https://developers.cloudflare.com/ai-gateway/reference/pricing/ ; https://developers.cloudflare.com/ai-gateway/changelog/ |
| Kong | `cost` strategy = (prompt_tokens × input_cost + completion_tokens × output_cost) / 1,000,000 | Not stated | https://developer.konghq.com/plugins/ai-rate-limiting-advanced/ |
| Envoy AI Gateway | CEL cost expressions over token metadata | `CachedInputToken` cost type | https://theagentrouter.ai/docs/capabilities/traffic/usage-based-ratelimiting/ |
| Helicone | Cost-based limits in cents | Not stated | https://docs.helicone.ai/features/advanced-usage/custom-rate-limits |
| Tyk AI Studio | Cost tracking (CE) | Not stated | https://github.com/TykTechnologies/ai-studio |

Cloudflare log retention caps: Workers Free allows 100,000 logs across all gateways, and Workers Paid allows 10,000,000 logs per gateway. Logpush is available only on Workers Paid: 10 million requests/month included, then $0.05 per million (https://developers.cloudflare.com/ai-gateway/reference/pricing/).

## Sources

https://www.paloaltonetworks.com/company/press/2026/palo-alto-networks-completes-acquisition-of-portkey-to-secure-ai-agents
https://www.paloaltonetworks.com/company/press/2026/palo-alto-networks-to-acquire-portkey-to-secure-the-rise-of-ai-agents
https://www.globenewswire.com/news-release/2026/03/24/3261574/0/en/portkey-s-gateway-is-now-fully-open-source-processing-over-1-trillion-tokens-every-day.html
https://github.com/Portkey-AI/gateway
https://github.com/Portkey-AI/gateway/tree/2.0.0
https://api.github.com/repos/Portkey-AI/gateway/releases
https://api.github.com/repos/Portkey-AI/gateway/license
https://github.com/Portkey-AI/gateway/issues/1579
https://api.github.com/repos/BerriAI/litellm/releases?per_page=5
https://github.com/BerriAI/litellm/blob/main/LICENSE
https://docs.litellm.ai/docs/proxy/release_cycle
https://docs.litellm.ai/docs/proxy/virtual_keys
https://docs.litellm.ai/docs/proxy/cost_tracking
https://docs.litellm.ai/docs/proxy/reliability
https://docs.litellm.ai/docs/enterprise
https://docs.litellm.ai/docs/proxy/caching
https://docs.litellm.ai/docs/proxy/guardrails/quick_start
https://docs.litellm.ai/docs/mcp
https://docs.litellm.ai/docs/a2a
https://docs.litellm.ai/release_notes/v1.80.8-stable/v1-80-8
https://github.com/BerriAI/litellm/issues/41424
https://github.com/BerriAI/litellm/issues/23149
https://github.com/BerriAI/litellm/issues/19984
https://github.com/BerriAI/litellm/issues/14293
https://github.com/coder/coder/pull/29563
https://konghq.com/blog/product-releases/kong-ai-gateway-3-14
https://github.com/Kong/kong/releases
https://developer.konghq.com/plugins/ai-proxy/
https://developer.konghq.com/plugins/ai-rate-limiting-advanced/
https://developer.konghq.com/plugins/ai-rate-limiting-advanced/reference/
https://developer.konghq.com/plugins/ai-semantic-cache/
https://developer.konghq.com/plugins/ai-mcp-proxy/
https://developer.konghq.com/plugins/ai-sanitizer/
https://developers.cloudflare.com/ai-gateway/
https://developers.cloudflare.com/ai-gateway/changelog/
https://developers.cloudflare.com/ai-gateway/features/rate-limiting/
https://developers.cloudflare.com/ai-gateway/features/caching/
https://developers.cloudflare.com/ai-gateway/features/guardrails/
https://developers.cloudflare.com/ai-gateway/reference/pricing/
https://www.helicone.ai/blog/joining-mintlify
https://github.com/Helicone/helicone
https://github.com/Helicone/ai-gateway
https://docs.helicone.ai/features/advanced-usage/custom-rate-limits
https://github.com/envoyproxy/ai-gateway/releases
https://theagentrouter.ai/release-notes/
https://theagentrouter.ai/docs/capabilities/traffic/usage-based-ratelimiting/
https://www.krakend.io/blog/krakend-ee-2.10-release-notes/
https://www.krakend.io/blog/krakend-ee-2.11-release-notes/
https://www.krakend.io/blog/krakend-ee-2.12-release-notes/
https://www.krakend.io/docs/enterprise/ai-gateway/llm-routing/
https://www.krakend.io/docs/enterprise/ai-gateway/budget-control/
https://github.com/TykTechnologies/ai-studio
https://tyk.io/blog/ai-studio-is-going-open-source-and-why-the-ai-control-plane-must-be-extensible/
https://github.com/openai/openai-python/blob/main/src/openai/types/chat/chat_completion_stream_options_param.py
https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_completed_event.py
https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_usage.py
https://developers.openai.com/api/docs/guides/rate-limits
https://developers.openai.com/api/docs/guides/prompt-caching
https://github.com/openai/tiktoken
https://platform.claude.com/docs/en/build-with-claude/streaming
https://platform.claude.com/docs/en/api/rate-limits
https://platform.claude.com/docs/en/build-with-claude/prompt-caching
https://platform.claude.com/docs/en/build-with-claude/token-counting
https://ai.google.dev/api/generate-content
https://ai.google.dev/gemini-api/docs/caching
https://ai.google.dev/api/caching
https://ai.google.dev/gemini-api/docs/openai
https://ai.google.dev/gemini-api/docs/rate-limits
https://ai.google.dev/gemini-api/docs/tokens
https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ConverseStream.html
https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html
https://docs.aws.amazon.com/bedrock/latest/userguide/api-keys.html
https://docs.mistral.ai/api/
https://github.com/mistralai/mistral-common
https://docs.ollama.com/api/chat
https://docs.ollama.com/api/openai-compatibility
https://docs.ollama.com/api/anthropic-compatibility
https://redis.io/docs/latest/develop/data-types/vector-sets/
https://github.com/redis/redis/releases
https://github.com/redis/redis/blob/unstable/LICENSE.txt
https://github.com/valkey-io/valkey-search
https://github.com/valkey-io/valkey-search/releases
https://github.com/open-telemetry/semantic-conventions/releases/tag/v1.42.0
https://github.com/open-telemetry/semantic-conventions-genai
https://github.com/open-telemetry/semantic-conventions-genai/tree/main/docs/gen-ai
https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-spans.md
https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-metrics.md
https://github.com/open-telemetry/semantic-conventions-genai/blob/main/docs/gen-ai/gen-ai-token-metrics.md

## Gaps

- **Portkey Gateway 2.0 license:** one third-party review (chatforest.com) says Gateway 2.0 is Apache 2.0. GitHub shows MIT on `main` and `"license": "MIT"` in `package.json` on the `2.0.0` branch, and the 2026-03-24 press release names no license. Treated as MIT here; not confirmed for any future 2.x tag.
- **Portkey after the acquisition:** neither PANW press release says whether the OSS gateway will keep being maintained. `main` has had no push since 2026-05-25 and no stable tag since v1.15.2 (2026-01-12), so it is unclear whether 2.x has been tagged.
- **Portkey semantic cache:** the README (on both `main` and `2.0.0`) asterisks semantic caching with the footnote "Available in hosted and enterprise versions", so it is not in the OSS build.
- **Kong editions:** the `Kong/kong` repo's latest release is 3.9.3, while AI Gateway features are at 3.14. I could not find a primary statement on whether any 3.10+ OSS build exists or which AI plugins (for example `ai-proxy`) remain free/OSS. The `ai-proxy` page does not state a tier.
- **Kong rate-limit window:** `ai-rate-limiting-advanced` mentions `window_type=sliding`; I did not verify fixed-window semantics or how streaming responses are counted.
- **KrakenD version facts:** the LLM-routing page, read directly, does show a "Since v2.3" label, which conflicts with the EE 2.10 AI Gateway launch (2025-06-04). The budget-control page states no version requirement; "EE v2.13.10" appears only as the current docs version, and the site news lists "KrakenD EE 2.13.10 update released". The KrakenD AI Gateway overview and MCP Gateway pages could not be fetched.
- **Tyk AI Studio versions:** the README says 2.1.0, tags run to v2.2.0-rc9.5, and the GitHub `releases/latest` object is v1.0.5 (2025-11-19). Stable version unclear. The database requirement is not specified.
- **Helicone:** maintenance-mode status comes from the Helicone and Mintlify posts. Whether the Rust `ai-gateway` (GPL-3.0, last push 2025-11-21) is still supported is not stated.
- **Envoy AI Gateway / Agent Router:** v1.0 blog claims (MCPRoute graduated to stable, CEL per-tool authorization) come from a third-party blog (bex.co). The GitHub v1.0.0 release notes do declare the v1beta1 control-plane API, including `MCPRoute`, stable, and v0.5.0 added CEL-based MCP authorization. No A2A support was found.
- **Mistral:** the rate-limit headers (`x-ratelimit-limit-requests`, `x-ratelimit-remaining-tokens`, etc.) came only from third-party skill listings, not Mistral docs. The response field for cached tokens and the exact placement of `usage` in stream chunks were not confirmed.
- **Gemini streaming usage:** it is not documented whether `usageMetadata` appears on every SSE chunk or only the last one. No rate-limit response headers are documented. The default explicit-cache TTL and the explicit-cache minimum token count were not stated on the pages read.
- **Gemini implicit caching field:** the caching doc cites `usage.total_cached_tokens` (Interactions API SDK), while `generateContent` uses `usageMetadata.cachedContentTokenCount`. Two surfaces, two names.
- **Bedrock:** no rate-limit response headers are documented for Converse or ConverseStream. A native Bedrock token-count API was not verified; the `/tokenize` mapping is Envoy AI Gateway's own.
- **OpenAI prompt caching for GPT-5.6:** the fetched doc describes `cache_write_tokens` and `prompt_cache_options.ttl` "30m". Because this changes earlier "automatic, no write fee" behavior, re-verify before relying on it. Bedrock's page agrees: 1.25× writes for GPT-5.6.
- **Redis Vector Sets:** the redis.io page no longer shows a Beta label or "Part of Redis Stack" wording; I did not verify GA status in the Redis release notes. valkey-search's supported Valkey versions and distance metrics were not verified.
- **Cloudflare:** semantic caching is "planned"; MCP/A2A support was not found; the Guardrails feature page does not name its model, but the pricing page says it uses `@cf/meta/llama-guard-3-8b` on Workers AI.
- **OTel GenAI:** no tagged release or schema URL exists yet in `semantic-conventions-genai`, so there is no version number to pin. I found no `OTEL_SEMCONV_STABILITY_OPT_IN` value in the new repo.
- **Performance numbers:** Portkey ("<1ms", "122kb") and valkey-search ("single-digit ms", "billions of vectors") claims have no hardware or benchmark context.
