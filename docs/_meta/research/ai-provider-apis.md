# AI Provider APIs and Standards

| Field | Value |
|---|---|
| Topic | Upstream AI provider API facts that Ruralz calls (OpenAI, Anthropic, Gemini, Bedrock, Mistral, Ollama): request surfaces, streaming and usage fields, prompt-caching mechanics and published cache pricing; OpenTelemetry `gen_ai` semantic conventions; Redis/Valkey vector search for semantic caching |
| Snapshot date | 2026-09-23 |
| Method | Provider API docs, SDK source on GitHub, GitHub REST API (`gh api`: releases, licenses), release notes and pricing statements in provider docs, all read on 2026-09-23. A claim is written as fact only when a primary source states it; anything else goes to Gaps. |

## 1. Provider-side OpenAI-compatible endpoints

Several providers serve an OpenAI-compatible surface next to their native API, which a unified façade can target directly:

- Gemini: `https://generativelanguage.googleapis.com/v1beta/openai/`, labelled beta. It supports `stream_options.include_usage`, and for image generation, parameters not listed on the page are "silently ignored" (https://ai.google.dev/gemini-api/docs/openai).
- Ollama: `http://localhost:11434/v1/` serves `/v1/chat/completions`, `/v1/completions`, `/v1/models`, `/v1/embeddings` and a non-stateful `/v1/responses`. It supports `include_usage`, and the local server ignores the API key (https://docs.ollama.com/api/openai-compatibility).
- Ollama also serves a subset of the Anthropic Messages API at `/v1/messages`. `cache_control` is listed as not supported, and token counts are "approximations" (https://docs.ollama.com/api/anthropic-compatibility).
- Bedrock serves the OpenAI Responses API on both the `bedrock-runtime` and `bedrock-mantle` endpoints for OpenAI models (https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html).

## 2. Provider matrix

| Dimension | OpenAI | Anthropic | Gemini (Developer API) | Bedrock | Mistral | Ollama |
|---|---|---|---|---|---|---|
| Base URL shape | `https://api.openai.com/v1/...` (Chat Completions, Responses) | `https://api.anthropic.com/v1/messages` | `https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent` and `:streamGenerateContent?alt=sse` | `https://bedrock-runtime.{region}.amazonaws.com/model/{modelId}/converse` and `/converse-stream` | `https://api.mistral.ai/v1/chat/completions` | `http://localhost:11434/api/chat` (native); `/v1/` (OpenAI-compatible) |
| Auth header | `Authorization: Bearer` | `x-api-key` + `anthropic-version: 2023-06-01` | `x-goog-api-key` header or `?key=` query | AWS SigV4, or `Authorization: Bearer <Bedrock API key>` (short-term ≤12 h or long-term) | `Authorization: Bearer` | None on local server (`security: []`) |
| Streaming format | SSE `data:` chunks, terminated by `data: [DONE]` (Chat Completions); typed SSE events (Responses) | SSE with named events: `message_start`, `content_block_*`, `message_delta`, `message_stop`, `ping`, `error` | SSE with `alt=sse` | AWS event stream: `messageStart`, `contentBlockStart/Delta/Stop`, `messageStop`, `metadata` | SSE, `data: [DONE]` terminator | NDJSON, `stream` defaults to `true` |
| Usage in stream | Chat Completions: only with `stream_options.include_usage`, as an extra chunk before `[DONE]` with `choices: []`; other chunks carry `usage: null`. Responses: in `response.completed`, under `response.usage` | `message_start.message.usage` (input and cache fields); `message_delta.usage`, whose counts are cumulative | `usageMetadata` (`promptTokenCount`, `candidatesTokenCount`, `totalTokenCount`, `cachedContentTokenCount`, `thoughtsTokenCount`, `toolUsePromptTokenCount`) | `metadata` event: `usage.inputTokens`, `outputTokens`, `totalTokens`, `cacheReadInputTokens`, `cacheWriteInputTokens`, `cacheDetails[]`; `metrics.latencyMs` | `usage` in final message | Final object with `done: true`: `prompt_eval_count`, `eval_count`, plus `*_duration` fields in ns |
| Prompt cache | Automatic. `usage.input_tokens_details.cached_tokens` and `cache_write_tokens`; `prompt_cache_key`; GPT-5.6+ uses `prompt_cache_options.ttl` (`30m`) | Explicit `cache_control` (up to 4 breakpoints) or top-level automatic `cache_control`; TTL `5m` or `1h` | Implicit caching (default on 2.5+) plus explicit `cachedContents` resource | Converse `cachePoint` / InvokeModel `cache_control` for Claude; `prompt_cache_breakpoint` for GPT-5.6 | `prompt_cache_key`; cached tokens billed at 10% | `cache_control` not supported on `/v1/messages` |
| Rate-limit headers | `x-ratelimit-{limit,remaining,reset}-{requests,tokens}`, project-token variants, `Retry-After` | `anthropic-ratelimit-{requests,tokens,input-tokens,output-tokens}-{limit,remaining,reset}` (reset in RFC 3339), priority-tier variants, `retry-after` | None documented; 429 `RESOURCE_EXHAUSTED`; limits are per project | 429 `ThrottlingException` (no headers documented) | Unverified (see Gaps) | N/A |
| Tokenizer | `tiktoken` (MIT, v0.14.0, 2026-08-17) | No public tokenizer; `POST /v1/messages/count_tokens` (free, estimate) | `models/{model}:countTokens`; ~4 chars/token | Native count API not verified | `mistral-common` (Apache-2.0, v1.12.0, 2026-09-22; Tekken tokenizers) | Model-local; counts returned in response |

Sources per column:
- OpenAI: https://github.com/openai/openai-python/blob/main/src/openai/types/chat/chat_completion_stream_options_param.py ; https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_completed_event.py ; https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_usage.py ; https://developers.openai.com/api/docs/guides/rate-limits ; https://developers.openai.com/api/docs/guides/prompt-caching ; https://github.com/openai/tiktoken
- Anthropic: https://platform.claude.com/docs/en/build-with-claude/streaming ; https://platform.claude.com/docs/en/api/rate-limits ; https://platform.claude.com/docs/en/build-with-claude/prompt-caching ; https://platform.claude.com/docs/en/build-with-claude/token-counting
- Gemini: https://ai.google.dev/api/generate-content ; https://ai.google.dev/gemini-api/docs/caching ; https://ai.google.dev/api/caching ; https://ai.google.dev/gemini-api/docs/rate-limits ; https://ai.google.dev/gemini-api/docs/tokens
- Bedrock: https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ConverseStream.html ; https://docs.aws.amazon.com/bedrock/latest/userguide/api-keys.html ; https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html
- Mistral: https://docs.mistral.ai/api/ ; https://github.com/mistralai/mistral-common
- Ollama: https://docs.ollama.com/api/chat ; https://docs.ollama.com/api/openai-compatibility ; https://docs.ollama.com/api/anthropic-compatibility

### 2.1 Provider details that affect usage accounting

- OpenAI: with `include_usage`, "If the stream is interrupted, you may not receive the final usage chunk" (https://github.com/openai/openai-python/blob/main/src/openai/types/chat/chat_completion_stream_options_param.py).
- OpenAI: stream obfuscation (`include_obfuscation`) is on by default and adds an `obfuscation` field to streaming deltas (https://github.com/openai/openai-python/blob/main/src/openai/types/chat/chat_completion_stream_options_param.py).
- Anthropic: "The token counts shown in the `usage` field of the `message_delta` event are *cumulative*." A consumer must therefore take the last value, not sum the deltas (https://platform.claude.com/docs/en/build-with-claude/streaming).
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
- Bedrock (provider-side guardrails): `guardrailConfig` on Converse supports PII entities, regexes, topics, word and content policies, and reports results in `metadata.trace.guardrail` (https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ConverseStream.html).

## 3. Prompt-caching mechanics and published cache pricing

| Provider | Trigger | TTL | Min tokens | Pricing | Source |
|---|---|---|---|---|---|
| Anthropic | `cache_control: {type: "ephemeral", ttl}` on blocks (max 4) or once at top level (automatic) | `5m` default, `1h` | 512 (Opus 5.5, Opus 5, Fable 5.x, Mythos 5.x); 1,024 (Opus 4.8, Sonnet 5, Sonnet 4.5/4.6); 2,048 (Opus 4.7); 4,096 (Opus 4.5/4.6, Haiku 4.5) | Write 1.25× (5m) or 2× (1h); read 0.1× (0.05× Opus 5.5; 0.025× Fable/Mythos 5.1) | https://platform.claude.com/docs/en/build-with-claude/prompt-caching |
| OpenAI | Automatic; `prompt_cache_key` optional | `in_memory` (~5–10 min) or `24h` on earlier models; GPT-5.6+ "at least 30 minutes" via `prompt_cache_options.ttl: "30m"` | 1,024 visible input tokens (GPT-5.6+) | Cached reads at 0.1× input; GPT-5.6+ cache writes at 1.25× input | https://developers.openai.com/api/docs/guides/prompt-caching |
| Gemini | Implicit (default for 2.5+); explicit `POST /v1beta/cachedContents`, referenced via `cachedContent` | Explicit: `ttl` or `expireTime` | Implicit: 4,096 (3.x Flash, 3.1 Pro); 2,048 (2.5 Flash/Pro) | Savings passed through automatically (implicit) | https://ai.google.dev/gemini-api/docs/caching ; https://ai.google.dev/api/caching |
| Bedrock (Claude) | Converse `cachePoint: {type: "default", ttl}`; InvokeModel `cache_control`; max 4 checkpoints in `system`, `messages`, `tools` | 5m, 1h | Per model, same as Anthropic (for example Opus 5: 512; Haiku 4.5: 4,096) | Per Bedrock pricing | https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html |
| Bedrock (GPT-5.6) | `prompt_cache_breakpoint: {mode: "explicit"}`; `prompt_cache_options.mode` `implicit` or `explicit` | 30m minimum | 1,024 | Write 1.25×; read 90% discount | https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html |
| Mistral | `prompt_cache_key` | — | — | Cached tokens at 10% | https://docs.mistral.ai/api/ |

## 4. Semantic-cache backends: Redis and Valkey vector search

- Redis Vector Sets have been available since Redis 8.0.0 (each command is tagged v8.0.0; the page shows no Beta label). They use HNSW. Commands are `VADD`, `VSIM` (`WITHSCORES`, `COUNT`, `FILTER`), `VREM`, `VCARD`, `VDIM`, `VEMB`, `VSETATTR`, `VGETATTR`, `VINFO`, `VLINKS`, `VRANDMEMBER`, `VRANGE` and `VISMEMBER`. Quantization options are `Q8`, `BIN` and `NOQUANT`. Binary FP32 input must be little-endian (https://redis.io/docs/latest/develop/data-types/vector-sets/).
- The latest Redis release is 8.10.2 (2026-09-17). Redis 8+ is tri-licensed RSALv2 / SSPLv1 / AGPLv3, and 7.2 and earlier remain BSD-3 (https://github.com/redis/redis/releases ; https://github.com/redis/redis/blob/unstable/LICENSE.txt).
- valkey-search is BSD-3-Clause. Its latest releases are 1.2.1 (2026-07-07), 1.1.1 and 1.0.3 (2026-07-15). It supports HNSW and exact KNN, numeric/tag/full-text hybrid queries, `FT.CREATE`, `FT.SEARCH` and `FT.AGGREGATE`, in standalone and cluster modes (https://github.com/valkey-io/valkey-search ; https://github.com/valkey-io/valkey-search/releases).
- valkey-search claims "billions of vectors with over 99% recall" and single-digit ms latency, with no hardware context given (https://github.com/valkey-io/valkey-search).

## 5. OpenTelemetry `gen_ai` semantic conventions status

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

## Sources

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

- **Mistral:** the rate-limit headers (`x-ratelimit-limit-requests`, `x-ratelimit-remaining-tokens`, etc.) came only from third-party skill listings, not Mistral docs. The response field for cached tokens and the exact placement of `usage` in stream chunks were not confirmed.
- **Gemini streaming usage:** it is not documented whether `usageMetadata` appears on every SSE chunk or only the last one. No rate-limit response headers are documented. The default explicit-cache TTL and the explicit-cache minimum token count were not stated on the pages read.
- **Gemini implicit caching field:** the caching doc cites `usage.total_cached_tokens` (Interactions API SDK), while `generateContent` uses `usageMetadata.cachedContentTokenCount`. Two surfaces, two names.
- **Bedrock:** no rate-limit response headers are documented for Converse or ConverseStream. A native Bedrock token-count API was not verified.
- **OpenAI prompt caching for GPT-5.6:** the fetched doc describes `cache_write_tokens` and `prompt_cache_options.ttl` "30m". Because this changes earlier "automatic, no write fee" behavior, re-verify before relying on it. Bedrock's page agrees: 1.25× writes for GPT-5.6.
- **Redis Vector Sets:** the redis.io page no longer shows a Beta label or "Part of Redis Stack" wording; I did not verify GA status in the Redis release notes. valkey-search's supported Valkey versions and distance metrics were not verified.
- **OTel GenAI:** no tagged release or schema URL exists yet in `semantic-conventions-genai`, so there is no version number to pin. I found no `OTEL_SEMCONV_STABILITY_OPT_IN` value in the new repo.
- **MCP and A2A:** this snapshot holds no primary facts from the MCP or A2A specifications themselves; spec versions, transports and authorization requirements must be read from the specs before any doc relies on them.
- **Performance numbers:** valkey-search claims ("single-digit ms", "billions of vectors") have no hardware or benchmark context.
