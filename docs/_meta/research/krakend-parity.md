# Research: KrakenD Community vs Enterprise feature snapshot

- Source: https://www.krakend.io/features/ (fetched 2026-09-23)
- Enterprise overview: https://www.krakend.io/enterprise/
- Edition at snapshot: KrakenD EE 2.13 (released 2026-03-12), CE 2.x with CE 3.0 announced
- Related release notes:
  - EE 2.10 (2025-06-04), AI Gateway + token quotas: https://www.krakend.io/blog/krakend-ee-2.10-release-notes/
  - EE 2.11 (2025-09-23), `ai/llm` namespace, conditional routing, AWS SigV4: https://www.krakend.io/blog/krakend-ee-2.11-release-notes/
  - EE 2.13 (2026-03-12), Bedrock, AI Grafana dashboard, Kafka async agents, Redis injection in all plugin types: https://www.krakend.io/blog/krakend-ee-2.13-release-notes/
  - CE 3.0 drops Go plugin support (announced 2026-06-04): https://www.krakend.io/blog/dropping-plugins-support-on-community/
  - AI gateway docs: https://www.krakend.io/docs/ai-gateway/
  - Writing plugins: https://www.krakend.io/docs/extending/writing-plugins/

Legend: **Both** = available in CE and EE; **EE** = Enterprise only.

## CI/CD, GitOps, and development tools

| Feature | Availability |
|---|---|
| KrakenD Designer | Both |
| Audit configuration | Both |
| Syntax validation and linting | Both |
| Flexible configuration | Both |
| Extended flexible configuration | EE |
| Multi-format configuration | Both |
| Hot-reload in development | Both |
| IDE integration | Both |
| Plugin builder | Both |
| Plugin generator | EE |
| End-to-end testing tool | EE |
| OpenAPI importer | EE |
| OpenAPI exporter | EE |
| OpenAPI server | EE |
| Postman collection generation | EE |
| DOT image generator | EE |
| Dump to disk | EE |

## Request and response transformation

| Feature | Availability |
|---|---|
| Backend For Frontend | Both |
| Aggregation | Both |
| Data transformation | Both |
| HTTP Cache headers (for CDN) | Both |
| Automatic output encoding | Both |
| Faster JSON decoding (fastjson) | EE |
| Flatmap | Both |
| Gzip compression | EE |
| Request body extractor | EE |
| Request manipulation using Go templates | EE |
| Response manipulation using Go templates | EE |
| Response manipulation with query language | EE |
| Regular expression replacements | EE |
| Conditional request and responses (CEL) | Both |
| Lua scripting | Both |
| Lua advanced helpers | EE |
| Custom Go plugins | Both (CE support removed in CE 3.0) |
| JSON Schema response validation | EE |
| JSON Schema request validation | Both |
| Martian (DSL) | Both |
| Multistrategy error handling | Both |
| Cache | Both |
| Sequential proxy | Both |
| Mocked data | Both |
| Workflows | EE |

## Security

| Feature | Availability |
|---|---|
| FIPS-140-2 cryptography module | EE |
| Security Policies Engine | EE |
| TLS for HTTPS and HTTP/2 | Both |
| Zero-trust parameter forwarding | Both |
| Restrict connections by host | Both |
| Clickjacking protection | Both |
| MIME-Sniffing prevention | Both |
| Cross-site scripting (XSS) protection | Both |
| HTTP Strict Transport Security (HSTS) | Both |
| HTTP Public Key Pinning (HPKP) | Both |
| CORS | Both |

## Routing

| Feature | Availability |
|---|---|
| Noop proxy | Both |
| Traffic shadowing/mirroring | Both |
| JWT claim-based routing | Both |
| Catchall (fallback upstream) | EE |
| Header and query string based dynamic routing | EE |
| Conditional routing | EE |
| Wildcard routes | EE |
| URL rewrite | EE |
| Virtual hosts | EE |
| Configurable client redirects | EE |

## Authorization and authentication

| Feature | Availability |
|---|---|
| JWT, OpenID Connect, OAuth2 | Both |
| JWT token signing | Both |
| Client credentials | Both |
| Basic authentication | EE |
| API keys | EE |
| Token revocation bloom filter | Both |
| Revoke Server | EE |
| Multiple identity providers per endpoint | EE |
| mTLS | Both |
| NTLM authentication | EE |
| Google GCP authentication | EE |
| AWS SigV4 authentication | EE |

## AI Gateway

| Feature | Availability |
|---|---|
| AI Gateway core | EE |
| AI Security | EE |
| AI Budget Control | EE |
| AI Governance | EE |
| Unified LLM interface and prompt templates | EE |
| LLM routing, multi-routing, and aggregation | EE |
| OpenAI integration | EE |
| Google Gemini integration | EE |
| Mistral integration | EE |
| Anthropic integration | EE |
| AWS Bedrock integration | EE |

## Services connectivity

| Feature | Availability |
|---|---|
| MCP Gateway | Both |
| MCP Server | EE |
| Protocol translation | Both |
| Streaming and Server-Sent Events (SSE) | EE |
| gRPC Server | EE |
| gRPC Client | EE |
| Static web server | EE |
| Service discovery | Both |
| GraphQL | Both |
| Load balancing | Both |
| Async agents | Both |
| Kafka async agents | EE |
| Lambda functions | Both |
| SOAP integration | EE |
| WebSockets multiplexer | EE |
| Direct WebSockets | EE |
| Intermediary web proxy | EE |
| AMQP/RabbitMQ consumer | Both |
| AMQP/RabbitMQ producer | Both |
| Azure Service Bus topic and subscription | Both |
| Google Cloud Pub/Sub | Both |
| NATS | Both |
| Apache Kafka | Both |
| Advanced Apache Kafka | EE |
| Amazon SNS | Both |
| Amazon SQS | Both |

## Traffic management

| Feature | Availability |
|---|---|
| Concurrent calls | Both |
| Circuit breaker | Both |
| Customizable HTTP circuit breaker | EE |
| Spike arrest and burst | Both |
| Bot detector | Both |
| Granular timeouts | Both |
| Service rate limit | EE |
| Tiered rate limit | EE |
| Endpoint rate limit | Both |
| Stateful rate limit (Redis backed) | EE |
| Proxy rate limit | Both |
| IP filtering | EE |
| MaxMind GeoIP | EE |

## Observability

| Feature | Availability |
|---|---|
| OpenTelemetry | Both |
| OpenTelemetry SaaS authentication | EE |
| Granular OpenTelemetry | Both |
| Exporter override for OpenTelemetry | EE |
| Logging | Both |
| Advanced logging | EE |
| Graylog/GELF logging | Both |
| Custom access log | EE |
| Extended metrics | Both |
| Jaeger tracing | Both |
| AWS X-Ray metrics and traces | Both |
| Zipkin tracing | Both |
| Elastic Logstash | Both |
| ELK Stack dashboard | Both |
| Prometheus | Both |
| InfluxDB metrics | Both |
| Grafana dashboard | Both |
| Google Cloud operations suite | Both |
| Datadog | Both |
| Auth0/Okta | Both |
| Keycloak | Both |
| Azure Active Directory | Both |
| New Relic (through OpenTelemetry) | Both |
| New Relic (native SDK) | EE |
| Azure OpenTelemetry Collector | Both |

## API governance and monetization

| Feature | Availability |
|---|---|
| API monetization (Moesif integration) | EE |
| API governance | EE |
| Token quota enforcement and quota management | EE |

## Enterprise support and services (from the enterprise page)

Direct assistance from core engineers, dedicated success engineer, SLA-backed commercial support, plugin development services, SRE and operations consulting, training and certification, code reviews and configuration audits, architecture reviews, performance optimization, onboarding assistance, security fixes and updates as standard, pricing not linked to API count or throughput.

## Facts relevant to positioning

- KrakenD has no WASM plugin runtime, no control plane, no console/UI beyond the stateless Designer, no GraphQL federation, and no HTTP/3 in either edition (as of the snapshot).
- Extensibility is Go plugins (EE-only from CE 3.0) and Lua.
- The whole AI Gateway category, gRPC server/client, SSE, WebSockets, API keys, stateful rate limiting, the security policies engine, and all OpenAPI tooling are Enterprise-only.
