---
title: Security and Identity
status: reviewed
owner: ruralz-core
last_updated: 2026-09-26
depends_on:
  - docs/_meta/foundation-pack.md
  - docs/_meta/style-guide.md
  - docs/architecture/01-system-overview.md
  - docs/architecture/02-configuration-model.md
  - docs/engineering/01-tech-stack-and-libraries.md
adrs: [ADR-0001, ADR-0002, ADR-0004, ADR-0005, ADR-0007, ADR-0008, ADR-0011, ADR-0014, ADR-0017]
milestone_tags_used: [M1, M2, M3, M4, M5]
---

# Security and Identity

## Summary

This document defines how Ruralz protects traffic, identities, secrets and configuration, from the threat model through authorization with CEL, OPA and Cedar to audit events. It owns the `RZ-AUTH` registry, ADR-0011 and ADR-0017. Nothing is implemented yet: every capability is Planned (Mx). Read it before exposing a Node or granting a Plugin a Capability.

## Scope and non-goals

In scope: every section below, including per-hop transport and identity, `auth.*` and `authz.*` runtime semantics and artifact trust policies ([ADR-0017](../adr/0017-artifact-signing.md)). "Pack" means the [foundation pack](../_meta/foundation-pack.md); `ruralz bundle audit` is Planned (M2).

Non-goals:

- Kinds and fields ([Configuration model](02-configuration-model.md)).
- Capability identifiers ([WASM plugin system](05-wasm-plugin-system.md)); Control Stream, Enrollment and RBAC APIs ([Control plane and GitOps](04-control-plane-and-gitops.md), [ADR-0007](../adr/0007-control-stream-protocol.md)).
- Guardrail detectors and residency routing ([AI/LLM gateway](06-ai-llm-gateway.md), [ADR-0014](../adr/0014-ai-api-surface.md)).
- Producing release signatures and SBOMs ([Release, versioning and compatibility](../engineering/04-release-versioning-and-compatibility.md)).
- Curated WAF rule sets, a [product non-goal](../vision/01-vision-and-positioning.md#product-non-goals).

## Threat model and trust boundaries

Per P6 (sandboxed custom code) and P9 (fail static) of [Vision and positioning](../vision/01-vision-and-positioning.md#principles), authentication and authorization fail closed. Assets: credentials, Revisions (holding key hashes), Plugin artifacts, secrets, keys, Enrollment identities, backups, State Store contents, audit log. Adversaries: clients, Plugin publishers, Bundle authors, insiders, network attackers, and compromised Upstreams, providers, relays or Nodes.

*Figure 1: trust boundaries between clients, Ruralz Gateway, Upstreams, Ruralz Control and State Store, labeled with System overview IDs.*

```mermaid
flowchart LR
    subgraph untrusted["Untrusted network"]
        cl["API clients and AI agents"]
    end
    subgraph nodez["Node zone"]
        gw["Ruralz Gateway (ruralzd)"]
        pl["Plugin sandbox (wazero)"]
        adm["Admin port 9901"]
    end
    subgraph infra["Operator infrastructure"]
        ss["State Store"]
        ctl["Ruralz Control"]
        cadm["Admin port 9902"]
        cst["Control Store"]
        rctl["Regional Ruralz Control, Planned (M4)"]
        ops["Operators and CI"]
        otel["OpenTelemetry collector"]
        kapi["Vault and Kubernetes API"]
    end
    subgraph ext["External services"]
        up["Upstreams"]
        ai["AIProviders"]
        idp["IdP and JWKS"]
        oci["OCI registry"]
        git["Git repository"]
    end
    cl -->|"TB-1 TLS or QUIC on 8443, cleartext 8080, Consumer credentials"| gw
    gw -->|"TB-2 Capabilities only"| pl
    gw -->|"TB-3 TLS or mTLS when configured, secretRef"| up
    gw -->|"TB-3 TLS for an https baseUrl, secretRef"| ai
    gw -->|"TB-3 TLS, secrets and discovery"| kapi
    gw -->|"TB-4 TLS when configured, credentials"| ss
    gw -->|"TB-5 8091, certificate or Enroll token, signed Revisions"| ctl
    gw -->|"TB-8 digest and Sigstore"| oci
    gw -->|"TB-10 https only, pinned issuer"| idp
    gw -->|"TB-12 TLS when configured, no secrets"| otel
    ctl -->|"TB-12"| otel
    ctl -->|"TB-8 digest pulls, host credentials"| oci
    ctl -->|"TB-7 service account, CRD watch"| kapi
    ops -->|"TB-6 RBAC and audit, 8090"| ctl
    ops -->|"TB-9 token or certificate over TLS, or loopback"| adm
    ops -->|"TB-9"| cadm
    ctl -->|"TB-7 repository credentials, HMAC webhooks"| git
    ctl -->|"TB-11 Raft mTLS 8092, token join"| cst
    rctl -->|"TB-13 relay certificate, mTLS"| ctl
```

| ID | Threat | Boundary | Mitigation |
|---|---|---|---|
| T1 | Credential guessing, username enumeration, hashing CPU exhaustion, targeted throttling | TB-1 | 128-bit keys; bounded [throttle](#pre-authentication-throttling) with a bucket per presented username; residuals accepted (OQ-security-and-identity-15 (a)) |
| T2 | JWT forgery, leaked IdP key | TB-1, TB-10 | [JWT and OIDC](#jwt-and-oidc); `kid` [revocation](#revocation) entries that last until the key leaves the issuer's JWKS, pending OQ-security-and-identity-4 |
| T3 | Spoofed or unavailable JWKS | TB-10 | `https`, pinned issuer, bounded key lifetime |
| T4 | Path confusion | TB-1 | One normalized path; encoded NUL and backslashes rejected ([Request hardening](#request-hardening)) |
| T5 | Address or certificate spoofing | TB-1 | [Client address](#ip-filtering-and-geoip) rule; per-Route `auth.mtls` verification |
| T6 | Revision or Plugin tampering, replay, retagging | TB-5, TB-8 | [Signed Revisions](#signed-revisions-and-plugin-artifacts) |
| T7 | Rogue enrollment | TB-5 | [Enrollment](#enrollment) |
| T8 | Sandbox escape, over-grant, credential harvesting | TB-2 | [Plugin sandbox](#plugin-sandbox) |
| T9 | Secret leakage; spoofed or compromised secret provider | All | [Secrets](#secrets) rule 2; verified TLS; least-privilege provider access |
| T10 | State Store eavesdropping or tampering (poisoned caches, reset limits), cross-user hits | TB-4 | Required credentials, TLS when configured, principal in cache keys; entry MAC ([Secrets](#secrets) rule 9), not on counters |
| T11 | Stolen session, insider change | TB-6 | [RBAC](#operator-identity-sso-and-rbac), approvals, audit |
| T12 | Resource exhaustion | TB-1 | [Request hardening](#request-hardening) |
| T13 | Cross-tenant leakage via Semantic Cache, Prompt Cache or provider resources | TB-1, TB-3 | Planned (M3): principal in cache keys; OQ-ai-llm-gateway-19 answer |
| T14 | Admin exposure, stolen scrape credential, Pod sidecars reading admin paths over shared loopback | TB-9 | [Admin ports](#admin-ports): a credential on every interface, loopback included; separate metrics token |
| T15 | Compromised Git account, forged webhook | TB-7 | Branch protection, HMAC, commit signatures |
| T16 | Rogue Ruralz Control replica | TB-11 | Separate peer CA |
| T17 | Relay or stale replica withholding revocations or forging `Ack`s | TB-13 | Signed mark exposes withholding within 30 s (target); forged `Ack`s residual (OQ-security-and-identity-4) |
| T18 | Secret exfiltration, SSRF by a Bundle author | TB-3, TB-10 | [Secrets](#secrets) rules 5 to 8, including the secret-to-destination binding (RZ-CFG-041) |
| T19 | Offline guessing of leaked key hashes | TB-8 | Private registries, key length; keyed hashing under OQ-security-and-identity-23 (b) |
| T20 | Prompt injection, exfiltration through a model | TB-3 | Partial detection (`ai.guardrail`, validation Plugins); residual bounded by least-privilege tool authorization (`authz.cel`), response URL checks and no privileged action on model output, Planned (M3) |
| T21 | Residency violation in generation, caches or telemetry | TB-3, TB-4, TB-12 | [Compliance](#compliance) fail-closed rule; client steering residual |
| T22 | Authentication skipped by `when` | TB-1 | [Authentication](#authentication) rule 1 |

IDs are stable; component documents cite them and MUST NOT add an unauthenticated crossing. For OQ-control-plane-and-gitops-14, option (a): token `Enroll` maps to TB-5, token replica `Join` to TB-11, webhooks and Ruralz Control's Kubernetes client to TB-7, its registry client to TB-8, the relay to TB-13, and Node calls to Vault and the Kubernetes API to TB-3, widening those boundaries and pack 8.4's 8091 and 8092 rows (OQ-security-and-identity-31).

## Transport security

Every TLS hop verifies the server certificate; no field disables it. Cleartext, where the table allows it (amending TB-1, TB-3, TB-4 and TB-12: OQ-security-and-identity-29), is a degraded state and audit finding. Per TB-4, a Node rejects a non-loopback State Store URL without credentials, with RZ-CFG-026 until OQ-security-and-identity-30 names a code.

| Hop | Protocol | Peer authentication | Planned |
|---|---|---|---|
| Client, TCP 8443 | TLS 1.3 by default; `tls.minVersion: "1.2"` allowed | Server certificate; client certificate for `auth.mtls` | Planned (M1) |
| Client, UDP 8443 | HTTP/3, TLS 1.3 | As above | Planned (M3) |
| Client, 8080 | Cleartext, SHOULD serve only redirects or traffic behind a TLS-terminating load balancer | Consumer credentials | Planned (M1) |
| Upstream, `AIProvider`, State Store, OTLP (TB-3, TB-4, TB-12) | TLS 1.2 or newer with `Upstream.spec.tls`, an `https` URL or `rediss://`; else cleartext | Server verified; optional client certificate; State Store credentials | Planned (M1) |
| Vault, Kubernetes API (TB-3, TB-7) | TLS 1.2 or newer, never cleartext | Server verified; Vault authentication (OQ-security-and-identity-10); namespace-scoped service account | Planned (M2) |
| IdP (TB-10) | TLS 1.2 or newer; `jwksUrl` and `tokenUrl` MUST be `https` (RZ-CFG-037) | Server verified | Planned (M1) |
| Admin 9901, 9902 (TB-9) | TLS when configured | [Admin ports](#admin-ports) | 9901 Planned (M1); 9902 Planned (M2) |
| 8091 (TB-5) | TLS 1.3; client certificate required for every RPC except `Enroll`, which uses server-authenticated TLS with a pinned server CA and a one-time token (proposed: OQ-security-and-identity-31) | Node certificate or token | Planned (M2) |
| 8092 (TB-11) | mTLS, TLS 1.3 only, except token `Join` from a replica without a peer certificate (proposed: OQ-control-plane-and-gitops-15, OQ-security-and-identity-31) | Disjoint CAs, so a Node key cannot join Raft; `Join` redeems a one-time `admin` token over a pinned server CA | Planned (M2) |
| Relay (TB-13) | mTLS, TLS 1.3 only | Relay certificate | Planned (M4) |
| 8090 (TB-6) | TLS 1.3 by default | Sessions or API tokens | Planned (M2) |

TLS 1.2 suites are fixed to ECDHE with AES-GCM or ChaCha20-Poly1305. Go 1.26 enables hybrid post-quantum key exchange by default ([source](https://go.dev/doc/go1.26)). 0-RTT is off, since early data can be replayed.

### Mutual TLS

A listener requests an unverified client certificate when any of its Routes attaches `auth.mtls`. A Hot Reload changing this mode sends GOAWAY, closes idle connections and sets `Connection: close` on busy HTTP/1.1 ones; an `auth.mtls` Route reached over a connection that requested none, coalesced HTTP/2 included, returns 421. The Filter verifies the chain against its own anchors, caching per connection, Policy and snapshot, so a removed CA stops matching at the next swap. Values such as `source.clientCertSubject` stay null unless the Route's own `auth.mtls` verified them ([fields](#basic-and-mtls-schemas)).

### Certificate rotation

Listener and upstream certificates from `file`, Planned (M1), or `kubernetes`, Planned (M2), rotate by watch without a Rollout, keeping the last value on failure (`ruralz_config_secret_rotation_failures_total`). Enrollment certificates (30 days, target) and Ruralz Control certificates renew at two thirds; signing keys arrive in a root-signed `TrustUpdate` before use ([Control plane and GitOps](04-control-plane-and-gitops.md#revision-signing-and-verification)); all Planned (M2).

### HTTP/3

HTTP/3, Planned (M3), uses `http3: true` on an `https` listener with the same certificates and Policies. FIPS builds turn it off, because quic-go skips FIPS enforcement for Initial packet protection and the Retry integrity tag ([source](https://github.com/quic-go/quic-go/blob/master/FIPS140.md)).

### FIPS build

The FIPS build, Planned (M5), is the same source built with `GOFIPS140` as separate artifacts ([Tech stack and libraries](../engineering/01-tech-stack-and-libraries.md#fips-build), [ADR-0001](../adr/0001-implementation-language-go.md)): `GOFIPS140=v1.0.0` (CMVP #5247) until v1.26.0 leaves "Pending Review", with `GODEBUG=fips140=on`, never `only` ([source](https://go.dev/doc/security/fips140)). HTTP/3 and hybrid key exchange are off, only approved `alg` values register, and Plugins get cryptography only through a crypto Host Function Capability, since FIPS mode does not cover Wasm. P1 and [ADR-0002](../adr/0002-apache-2-license-no-feature-gating.md) apply: same license and features.

## Authentication

Client authentication runs in `onRequestHeaders`, Filter class `auth`. The four client types share slot `auth`, are `failureMode: closed` only and never call the State Store. Two runtime rules close skip paths:

1. If `when` skips every auth-class Policy in a Route's chain, the request fails with 401 RZ-AUTH-001, so a public path uses `excludePolicies` (proposed to Configuration model and Data plane).
2. A second successful authentication on a request, whether or not either bound a Consumer, fails with 401 RZ-AUTH-002.

A `when` that errors makes a closed Policy run ([Configuration model](02-configuration-model.md#cel-expressions-and-allowed-places)); auth `when` conditions SHOULD NOT read values an Upstream may reinterpret, such as `X-HTTP-Method-Override`.

| Type | Verification | Consumer binding | Planned |
|---|---|---|---|
| `auth.jwt` | Signature with keys from each issuer's `jwksUrl`; `iss`, `aud`, required `exp`, `nbf` | `credentials.jwt` by issuer plus `subject` or `claims` | Planned (M1) |
| `auth.api-key` | SHA-256 of the key in `config.header`, looked up by digest | `credentials.apiKeys` | Planned (M1) |
| `auth.basic` | PBKDF2 behind the [throttle](#pre-authentication-throttling) | `credentials.basic` by username ([proposed](#basic-and-mtls-schemas)) | Planned (M1) |
| `auth.mtls` | Chain to `config.caCertificate`, `config.subjects`, `config.crl` | Optional `credentials.certificates` by subject or URI SAN ([proposed](#basic-and-mtls-schemas)) | Planned (M1) |

### JWT and OIDC

- The token's `iss` selects exactly one `config.issuers[]` entry, which MUST set `audiences` and an `https` `jwksUrl`.
- When compiling a Revision, Nodes prefetch issuers, 16 at once, within 5 s (target), reuse unchanged issuers' keys and activate even if a fetch fails.
- Keys live for `max-age` clamped to 5 minutes to 6 hours, or 1 hour when absent or `no-cache` (target), refresh at half-life and, after failed refreshes, serve degraded until expiry (TB-10), then RZ-AUTH-006.
- An unknown `kid` fails with RZ-AUTH-005 and triggers at most one refresh per issuer per 30 s (target), honoring `Retry-After`; a `kid` [revocation](#revocation) entry evicts a leaked key and blocks every later load of it until the entry is removed.
- jwx v4.5.0 or newer fixes GHSA-4cf7-xm37-g63h ([source](https://github.com/lestrrat-go/jwx/releases/tag/v4.5.0)). For OQ-tech-stack-and-libraries-6, option (b): go-oidc, with go-jose/v4 ([source](https://github.com/coreos/go-oidc/blob/v3/go.mod)), stays in Ruralz Control.

For OQ-security-and-identity-1, option (b), M1 fixes these `auth.jwt` defaults instead of adding fields:

| Default | M1 value |
|---|---|
| Algorithms | RS256, PS256, ES256 or EdDSA, so HS256 is rejected; any other `alg`, or a `crit` header, is RZ-AUTH-002; `jku`, `x5u`, `jwk` and `x5c` are ignored |
| Token | JWS compact form from `Authorization: Bearer` only, at most 16 KiB (target) |
| Clock skew | 60 s (target) on `exp` and `nbf` |
| `exp` | Required, at most 24 hours ahead (target), else RZ-AUTH-003; `iat` is not checked |
| Keys | JWKS public keys only: RSA of 2,048 bits or more, P-256 or Ed25519; at most 256 keys and 1 MiB per document (target) |

*Figure 2: the authentication decision for a request.*

```mermaid
flowchart TD
    A["Request, Route fixed by header match"] --> B{"Auth-class Policy in the effective chain?"}
    B -- "no" --> AN["Anonymous: consumer and auth null"]
    B -- "yes" --> W{"Does any run after its when condition?"}
    W -- "none runs" --> R1["401 RZ-AUTH-001"]
    W -- "yes, or when errors" --> C{"Credential present?"}
    C -- "no" --> R1
    C -- "yes" --> D{"Can the Filter decide?"}
    D -- "unknown kid" --> R5["401 RZ-AUTH-005, one refresh scheduled"]
    D -- "no usable keys" --> R6["401 RZ-AUTH-006, failureMode closed"]
    D -- "auth.basic, not in success cache" --> L{"Username bucket and hash queue admit it?"}
    L -- "no, at once or after bounded wait" --> R7["429 RZ-AUTH-007, no hash computed"]
    L -- "yes" --> E{"Signature, hash or chain valid?"}
    D -- "yes" --> E
    E -- "no" --> R2["401 RZ-AUTH-002 or 003"]
    E -- "yes" --> F{"On the revocation list?"}
    F -- "yes" --> R4["401 RZ-AUTH-004"]
    F -- "no" --> G{"Matching Consumer?"}
    G -- "yes, first binding" --> H["consumer and auth set"]
    G -- "no, API key or basic; or a second binding" --> R2
    G -- "no, JWT or mTLS" --> J["auth set, consumer null"]
    AN --> Z["authz Policies in class order"]
    H --> Z
    J --> Z
    Z -- "any deny" --> R3["403 RZ-AUTH-010 to 016"]
    Z -- "all allow" --> OK["Admission and later classes"]
```

### Basic and mTLS schemas

Per pack section 3, this document authors these schemas, all Planned (M1) and registered in the [Configuration model](02-configuration-model.md#registered-from-feature-documents).

| Type or kind | Field | Shape and rule |
|---|---|---|
| `auth.basic` | `config` | No fields; reads `Authorization: Basic`, and a malformed value is RZ-AUTH-002 |
| `auth.mtls` | `config.caCertificate` | Required `SecretValue`: PEM bundle of the client CAs this Policy trusts, rotated by watch like listener certificates |
| `auth.mtls` | `config.subjects` | Optional `set`; each rule holds exactly one of `subject` (RFC 4514 name, exact) or `uriSan` (exact, for example a SPIFFE ID). Empty admits any verified leaf; a leaf matching no rule is RZ-AUTH-002 |
| `auth.mtls` | `config.crl` | Optional `SecretValue`: PEM CRLs for those CAs, watched; a listed serial is RZ-AUTH-004. Past `nextUpdate`, the last CRL stays enforced and the Node reports a degraded state |
| `Consumer` | `credentials.basic` | `map` keyed by `username`; each entry holds `hash` and `iterations` |
| `Consumer` | `credentials.basic[].hash` | `pbkdf2-sha256:<salt>:<key>`, base64url of a 16-byte random salt and a 32-byte PBKDF2-HMAC-SHA-256 key; T19 applies as to key hashes |
| `Consumer` | `credentials.basic[].iterations` | 600,000 to 1,000,000, default 600,000 (target); outside that range is RZ-CFG-036 |
| `Consumer` | `credentials.certificates` | `map` keyed by `name`; each entry holds exactly one of `subject` or `uriSan`, matched exactly against a leaf `auth.mtls` verified |

A leaf MUST carry the `clientAuth` extended key usage. A leaf matching no Consumer sets `auth` and leaves `consumer` null, as for JWT. A `username`, `subject` or `uriSan` declared by two Consumers is RZ-CFG-035, so each credential binds at most one Consumer.

### Accepting more than one credential type

For OQ-configuration-model-13, option (a): distinct slots with complementary `when` conditions. The Route MUST exclude any Gateway slot-`auth` Policy, so the pattern fails where that Policy is `overridable: false`; if a `when` errors, both run and rule 2 applies.

```yaml
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: auth-either-apikey}
spec:
  type: auth.api-key
  slot: auth-apikey                 # explicit slot, so it does not replace the JWT Policy
  when: '"x-api-key" in request.headers'
  config: {header: x-api-key}
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: auth-either-jwt}
spec:
  type: auth.jwt
  slot: auth-jwt
  when: '!("x-api-key" in request.headers)'   # exactly one of the two runs
  config:
    issuers:
      - issuer: https://auth.shop.example
        jwksUrl: https://auth.shop.example/.well-known/jwks.json
        audiences: [shop-api]
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata: {name: partner-orders}
spec:
  match: {hosts: ["api.shop.example"], path: {prefix: "/v1/partner/"}}
  policies: [{name: auth-either-apikey}, {name: auth-either-jwt}]
  excludePolicies: [{name: jwt-default}]    # the Gateway slot-auth Policy would otherwise stack
  upstreams: [{name: orders}]
```

### Upstream authentication

`auth.upstream-oauth2`, Planned (M1), runs the client credentials grant, refreshing at two thirds of the token lifetime (target). A miss runs one single-flight fetch per Policy and Node within the request deadline and a 2 s `timeout` (target); failure is 401 RZ-AUTH-020. Token requests follow no redirect ([Secrets](#secrets) rule 8).

`auth.upstream-sigv4`, Planned (M2), records SigV4 signing parameters in `onUpstreamRequest`; the Node signs at the transport on every attempt, after every Filter, Plugin and dialect translation (hook: OQ-security-and-identity-28). The body is buffered within `limits.maxRequestBodyBytes` or, with `payload: unsigned`, sent as UNSIGNED-PAYLOAD where accepted. For OQ-ai-llm-gateway-12, option (b): this type on the `ai` Upstream, signing only `bedrock` dialect legs.

Authored here and registered in the [Configuration model](02-configuration-model.md#registered-from-feature-documents): `auth.upstream-sigv4` requires `config.region` and `config.service`, and `config.payload` is `signed` (default) or `unsigned`; `auth.upstream-oauth2` adds `config.timeout`, default 2 s (target).

Also authored here, Planned (M2) and proposed for registration (OQ-security-and-identity-12, option (b)): `auth.upstream-oauth2` `config.grantType` is `client-credentials` (default) or `jwt-bearer` (RFC 7523). `jwt-bearer` replaces `clientId` and `clientSecret` with `config.serviceAccountKey`, a required `SecretValue` holding a service-account JSON key; per fetch, the Node signs an RS256 assertion with it for the `https` `tokenUrl`. `config.audience`, when set, requests an ID token for that audience instead of an access token for `scopes`. This serves Google Cloud service-account authentication.

Every mechanism is free. JWT token signing is a built-in type that never takes keys in Plugin `config`, Planned (M2), pending OQ-security-and-identity-11. Token revocation uses the signed [revocation](#revocation) list, Planned (M2). NTLM authentication is Not planned: it authenticates a TCP connection, breaking pooling, and uses non-FIPS MD4 and HMAC-MD5 (P1).

### RZ-AUTH decision codes

Client messages are generic; metric names are proposed to [Observability](10-observability.md).

| Code | Status | Meaning |
|---|---|---|
| RZ-AUTH-001 | 401 | Credential missing, or every auth-class Policy skipped |
| RZ-AUTH-002 | 401 | Credential invalid, no matching Consumer, or a second binding |
| RZ-AUTH-003 | 401 | Token expired, not yet valid, without or over-long `exp`, or wrong issuer or audience |
| RZ-AUTH-004 | 401 | Credential revoked |
| RZ-AUTH-005 | 401 | Unknown `kid` |
| RZ-AUTH-006 | 401 | No usable keys for the issuer |
| RZ-AUTH-007 | 429 | Authentication throttled |
| RZ-AUTH-008 | 421 | An `auth.mtls` Route was reached over a connection that requested no client certificate |
| RZ-AUTH-010 to RZ-AUTH-014 | 403 | Denied by `authz.cel`, `authz.opa`, `authz.cedar`, `authz.ip` or `authz.geoip` |
| RZ-AUTH-015 | 403 | Authorization could not decide |
| RZ-AUTH-016 | 401 or 403 | Rejected by a `plugin` Policy of class `auth` (401) or `authz` (403) |
| RZ-AUTH-020 | 401 | Upstream credential not obtained or signed (pack 8.10, OQ-security-and-identity-9 (a)) |

## Authorization

Authorization runs in Filter class `authz`. Each type's slot defaults to its name, so Policies stack and all must allow; a deny returns 403 and an evaluation error RZ-AUTH-015. [ADR-0011](../adr/0011-expressions-and-authorization-engines.md) fixes CEL inline, OPA and Cedar as pluggable engines, and no Lua.

| Engine | When to use | Cost per request | Guardrails | Planned |
|---|---|---|---|---|
| CEL, `authz.cel` | Short rules over claims, Consumer, Tier, method and path; the default | Compiled per Revision; under 2 µs at p99 (target) | RZ-CFG-015; runtime `CostLimit` ([source](https://github.com/cel-expr/cel-go/blob/master/cel/options.go)) | Planned (M1) |
| OPA (Rego), `authz.opa` | Large or shared data-driven rule sets | `PrepareForEval` per Revision ([source](https://github.com/open-policy-agent/opa/blob/main/v1/rego/rego.go)); under 100 µs at p99 (hypothesis) | Deadline; only allowlisted pure built-ins, so no `http.send`; 16 MiB data per Policy (target) | Planned (M2) |
| Cedar, `authz.cedar` | Principal, action and resource models with hierarchies | In process; under 100 µs at p99 (hypothesis) | 1,000 policies and 10,000 entities per Policy, ancestor depth at most 8 (target), transitive closure precomputed per Revision; no schema validator in cedar-go ([source](https://github.com/cedar-policy/cedar-go/blob/main/README.md)) | Planned (M2) |

Identical engines compile once, by digest. A Revision-wide ceiling of 64 MiB of compiled engine memory and 50,000 entities (target), reported per snapshot, counts toward snapshot size, so peak engine memory is (K + 2) = 4 times it, 256 MiB (hypothesis) ([System overview](01-system-overview.md#compile-before-swap)); compilation counts against the 2 s compile time budget. `ruralz bundle validate` checks the ceiling (code: OQ-security-and-identity-30), or, with OCI delivery (OQ-security-and-identity-13), Ruralz Control ingest and Node activation, NACKing a violation. CEL SHOULD come first; cedar-go has had no commits since 2026-06-01 ([source](https://github.com/cedar-policy/cedar-go)), so OPA is the fallback.

```yaml
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: authz-gold-only}
spec:
  type: authz.cel
  config:
    rule: 'consumer != null && consumer.tier == "gold"'   # Tier gate with existing fields
```

### OPA and Cedar schemas

This document authors both engine schemas ([ADR-0011](../adr/0011-expressions-and-authorization-engines.md)), Planned (M2) and proposed for Configuration model registration with OQ-security-and-identity-13, option (c); until then, no example uses them.

| Type | Field | Shape and rule |
|---|---|---|
| `authz.opa` | `config.module`, `config.bundle` | Exactly one: an inline Rego module, or an OCI reference pinned by `sha256:<64 hex>` holding modules and data, verified under the Plugin trust policy (proposed) |
| `authz.opa` | `config.query` | Required Rego reference, for example `data.ruralz.allow`; only `true` allows, and `false`, undefined or a non-boolean is 403 RZ-AUTH-011 |
| `authz.opa` | `config.data` | Optional inline JSON document served as `data`; with `module` only |
| `authz.cedar` | `config.policies`, `config.bundle` | Exactly one: inline Cedar policy text, or a pinned OCI reference holding policies and entities, verified as above |
| `authz.cedar` | `config.entities` | Optional inline Cedar entity JSON (`uid`, `attrs`, `parents`); with `policies` only |

Modules, data, policies and entities are fixed per Revision and count toward the caps above; no engine fetches anything at request time, so changing them is a Revision.

The OPA `input` is fixed: the base CEL variables as JSON, namely `request` (`body` only in `onRequestBody`), `source` (including `ip`), `route`, `consumer`, and `auth` with its `claims`, null where CEL is null.

Cedar requests use a fixed mapping:

- **Principal:** `Ruralz::Consumer::"<consumer.name>"`, with `tier` and `tags` as attributes, when bound; else `Ruralz::Subject::"<iss>#<sub>"` from JWT claims; else `Ruralz::Anonymous::""`.
- **Action:** `Ruralz::Action::"<method>"`.
- **Resource:** `Ruralz::Path::"<normalized path>"`, whose parent is `Ruralz::Route::"<route.name>"`, so `resource in Ruralz::Route::"orders"` scopes a rule.
- **Context:** the remaining `input` fields, claims included.

Ruralz adds the principal and resource entities per request, merging parents with a declared entity of the same `uid`, such as a Consumer in a group. Any policy error in the diagnostics is RZ-AUTH-015, so an erroring `forbid` never admits a request.

### IP filtering and GeoIP

`authz.ip`, Planned (M1), filters by CIDR on `source.ip`; `authz.geoip`, Planned (M2), by country from a local MaxMind-format database (reader: OQ-tech-stack-and-libraries-24). This document authors both schemas, registered in the [Configuration model](02-configuration-model.md#registered-from-feature-documents):

| Type | Field | Shape |
|---|---|---|
| `authz.ip` | `config.allow`, `config.deny` | `set` of CIDRs; a bare address means /32 or /128, and IPv4-mapped IPv6 matches as IPv4 |
| `authz.geoip` | `config.allow`, `config.deny` | `set` of ISO 3166-1 alpha-2 codes; an address the database cannot place matches neither list |

Precedence is the same for both: a `deny` match refuses; otherwise a non-empty `allow` refuses every address outside it, and an empty one admits. Both lists empty fails validation. Refusals are 403 RZ-AUTH-013 (`authz.ip`) or RZ-AUTH-014 (`authz.geoip`).

**Client address**, Planned (M1), takes both fields of OQ-security-and-identity-6 (c): `listeners[].proxyProtocol: true` requires a PROXY protocol v2 header on every connection, whose source replaces the TCP peer, and `Gateway.spec.trustedProxies` lists trusted-proxy CIDRs. `source.ip` is that peer unless the list holds it; then it is the rightmost `Forwarded` (preferred) or `X-Forwarded-For` address outside the list, from at most 16 parsed (target). Forwarding headers from untrusted peers are overwritten, never appended. Behind a load balancer without either field, `source.ip` controls collapse to one key.

## Consumers and tiers

A `Consumer` is a Bundle caller identity with credentials, a Tier, Quotas and tags ([Configuration model](02-configuration-model.md#consumer)); authentication binds at most one, readable as `consumer`.

| Rule | Statement |
|---|---|
| Binding | API keys by digest; JWTs by issuer plus `subject` or `claims`; OAuth clients by `client_id`, else `azp`, when `iss` is in that Consumer's `credentials.jwt`, so `oauthClients` without `credentials.jwt` is RZ-CFG-005 (OQ-security-and-identity-24 (a)); two matching Consumers are 401 RZ-AUTH-002 |
| Keys | SHA-256 digests only; `secretRef`-held keys are hashed at load and rejected under 22 base64url characters, with RZ-CFG-026 until OQ-security-and-identity-23 names a code. Keys SHOULD carry 128 random bits; OCI registries holding Revisions MUST be private |
| Tier | A label Policies read; it grants nothing |
| Upstream identity | An Upstream `headers` Policy `set` overwrites client identity headers |
| Cache partitioning | Proposed to `cache` and `ai.semantic-cache` owners: authenticated Routes key by principal, so entries grow with principals (hypothesis); sharing needs a field (OQ-security-and-identity-25) |

```yaml
apiVersion: ruralz/v1alpha1
kind: Consumer
metadata: {name: partner-beta}
spec:
  tier: gold
  credentials:
    apiKeys:                         # rotation: two named keys during an overlap window
      - name: primary-2026-09
        hash: "sha256:7f1ea8dd39cc46903ce1e949d43efd9ba5b94b40e98a0583688a2f934dd88058"
      - name: primary-2026-06        # removed after the overlap window
        secretRef: {provider: kubernetes, name: beta-keys, key: previous}
```

Every key change is a Revision; urgent removal uses [Revocation](#revocation). For OQ-configuration-model-17, the Bundle stays the only Consumer source through Planned (M3).

## Secrets

`secretRef` is the only way a secret enters Ruralz: a literal in a `SecretValue` field is RZ-CFG-012. Providers `env` (rotation needs a restart) and `file` (preferred on Kubernetes) are Planned (M1); `kubernetes` (`get` and `watch` in the Node's namespace, limited by `resourceNames`) and `vault` (Node authentication: OQ-security-and-identity-10) are Planned (M2).

Components MUST follow rules 1 to 9, Planned (M1) on Nodes and the CLI, the Ruralz Control, Enrollment and Vault parts Planned (M2); rules 5 to 9 take OQ-security-and-identity-22 (a), amending pack sections 2 and 5.

1. Only Nodes resolve Bundle secrets; Ruralz Control and the CLI see references.
2. Resolved values never appear in a Revision, diff, Last-Known-Good, `/config/dump`, log, trace, metric label or `/tap`, which also redacts credential headers.
3. An unresolvable reference is RZ-CFG-026: the Node rejects the Revision (a NACK in Control mode) and keeps its active one.
4. After a cold start, `/readyz` waits for every reference, so TLS keys and the State Store URL SHOULD use `file` or `env`. For OQ-configuration-model-15 and -5: no persistence through Planned (M3); `vault` for cloud managers.
5. A `file` reference resolves only when its path, before and after resolving symlinks, lies under `RURALZ_SECRET_ROOT`, default `/etc/ruralz`, and `env` only for `RURALZ_STATE_STORE_URL` and `RURALZ_SECRET_*`; a violation is RZ-CFG-026. A Node refuses to start if the root contains `${RURALZ_DATA_DIR}`, `RURALZ_ADMIN_TLS_DIR`, an admin or Enrollment token file, the MAC key file or the Vault credential.
6. Every Node connection for the Bundle (`jwksUrl`, `tokenUrl`, `baseUrl`, Endpoints, discovery results) refuses 0.0.0.0/8, ::/128, 127.0.0.0/8, ::1, 169.254.0.0/16, fe80::/10 and fd00:ec2::254, IPv4-mapped and NAT64 forms included, at connect time and on each redirect hop, unless `RURALZ_FETCH_ALLOW`, comma-separated addresses and CIDRs (an unparsable entry refuses start), holds them, for example sidecars or a loopback `ollama`. It never follows `https` to `http` or a `tokenUrl` redirect, and ignores proxy variables unless the list holds `env-proxy`.
7. Adding a `secretRef` or changing a secret's destination has diff impact `security`.
8. Each `secretRef` use has one static destination, and a reference with two is RZ-CFG-041 in every binary ([Secret-to-destination binding](02-configuration-model.md#secret-to-destination-binding)); token requests follow no redirect, and the State Store dialer dials only its URL's host.
9. With `RURALZ_STATE_STORE_MAC_KEY_FILE` set (an absolute path, owner-only, 32 bytes or more, outside the secret root, read at start), every opaque entry Nodes share through the State Store, Response Cache entries in M1, carries an HMAC-SHA-256 tag over key, field and value; a missing or wrong tag reads as a miss. Counters carry no tag, as State Store scripts cannot compute one; a new key needs a restart and turns entries into misses.

*Figure 3: secret resolution through `secretRef` on activation and rotation.*

```mermaid
flowchart TD
    A["Revision verified"] --> D{"One destination per secretRef?"}
    D -- "no" --> X["RZ-CFG-041: rejected in every binary"]
    D -- "yes" --> B["Collect every secretRef"]
    B --> P{"Inside RURALZ_SECRET_ROOT or the RURALZ_SECRET_ prefix?"}
    P -- "no" --> N
    P -- "yes" --> C{"Provider"}
    C -- "env" --> E1["Read ruralzd process variable"]
    C -- "file" --> E2["Read file, start watch"]
    C -- "kubernetes" --> E3["Get Secret over verified TLS, start watch"]
    C -- "vault" --> E4["Read path over verified TLS, renew lease"]
    E1 --> R{"All resolved?"}
    E2 --> R
    E3 --> R
    E4 --> R
    R -- "no" --> N["RZ-CFG-026: reject the Revision, a NACK in Control mode"]
    R -- "yes" --> M["In-memory secret table keyed by reference"]
    M --> S["Atomic snapshot swap, readiness can pass"]
    W["Watch or lease event"] --> V{"New value valid?"}
    V -- "yes" --> M
    V -- "no" --> K["Keep last value, rotation failure metric"]
```

## Request hardening

These controls are always on and Planned (M1) unless noted ([System overview](01-system-overview.md#design-principles)); [Data plane](03-data-plane.md) owns parsing.

| Concern | Control |
|---|---|
| Oversized requests | RZ-RT-002 over `limits.maxRequestHeaderBytes`; RZ-RT-003 over `maxRequestBodyBytes`, counted decoded |
| Header floods | 256 fields (target), RZ-RT-002, counted after the parse `maxRequestHeaderBytes` bounds; Go 1.27 adds `Server.MaxHeaderValueCount` ([source](https://go.dev/doc/go1.27)) (OQ-security-and-identity-20) |
| Slow clients | Header-read and idle timeouts (OQ-security-and-identity-17) |
| Request smuggling | Conflicting framing never forwarded, 400 RZ-RT-017 when the handler detects it ([Data plane](03-data-plane.md#listeners-and-protocols)); hop-by-hop headers removed |
| Path confusion | One normalized path for Router, CEL, `authz.*` and upstream; only a later `transform.request` rewrites it. `%00`, raw or encoded backslashes, other control bytes and invalid escapes are 400 RZ-RT-017 (OQ-security-and-identity-21 (a)) |
| Credential forwarding | `auth.api-key` and `auth.basic` strip their credential; `auth.jwt` forwards it |
| gRPC | Planned (M3): connect-go `WithRequestGate` authenticates before decompression ([source](https://github.com/connectrpc/connect-go/releases/tag/v1.21.0)) |
| Browser protections | `cors`; security headers in a non-overridable Gateway `headers` Policy |

### Pre-authentication throttling

Rate Limits run after `auth` and never see failed attempts, so a throttle, Planned (M1) and local to each Node, covers `auth.basic` verifications that miss the success cache; API keys rely on entropy.

| Element | Rule |
|---|---|
| Success cache | Keyed by HMAC-SHA-256 of username and password under a per-process key; each entry records the Consumer credential digest it matched. An entry lasts 60 s after its last hit and at most 15 minutes from verification, less up to 20% jitter (target); 64 shards, 10,000 entries per Node (target). A hit skips throttle and hash |
| Cache on change | A hit still passes the [revocation](#revocation) check (Figure 2, E to F), so a revocation advance evicts nothing. A snapshot swap keeps each entry whose username and credential digest are unchanged, and evicts only changed or removed credentials |
| Username bucket | One per presented username, declared or not, keyed by HMAC-SHA-256 of the username under the per-process key, in one 64-shard LRU of 100,000 (target); burst 10, refill 1 per second (target). A bucket is created only after the hash queue admits an attempt, at most 100 per second per Node (target) |
| Undeclared usernames | The same bucket rules, plus a dummy hash at the default iteration count through the same queue, so neither timing nor status reveals whether a username exists |
| Hash queue | max(1, `GOMAXPROCS`/4) slots, which cap the Node's hash rate; 4 × slots waiters, 1 s deadline (target) |
| Delay | An empty bucket holds at most 2 waiters, 1,024 per Node, for up to 2 s (target) |
| Refusal | Hash-queue overflow, the bucket-creation cap and delay overflow each return 429 RZ-AUTH-007 with `Retry-After` before any hash, for declared and undeclared usernames alike, counted by cause |
| Source key | `source.ip` ([Client address](#ip-filtering-and-geoip)), IPv6 as /64, 10 failures per minute (target); an empty bucket refuses uncached attempts |

**Cost.** At 600,000 iterations a slot verifies 10 to 20 credentials per second (hypothesis), so hashing uses at most a quarter of the cores when `GOMAXPROCS` is 4 or more, otherwise one core.

**Capacity.** A client calling at least once per 60 s is re-verified about once per maximum age, so a Node sustains about slots × 10 to 20 × 800 distinct active `auth.basic` credentials (hypothesis): 8,000 to 16,000 on one slot, capped at the 10,000-entry cache (target), which is sized to one slot's rate. A client calling less often pays one hash per call. Beyond that bound, legitimate clients get RZ-AUTH-007 with no attack, so `auth.basic` suits low-cardinality clients, and high-cardinality callers SHOULD use API keys or JWT.

**Ramp.** A restart or Zero-Downtime Upgrade starts with an empty cache and LRU, since the key is per process, so each active client needs one hash; the ramp runs at the lower of the hash rate and the 100 per second creation cap (target). With 1,000 active clients on an 8-core Node (2 slots), it takes about 25 to 50 s (hypothesis), and callers beyond the queue get RZ-AUTH-007 with `Retry-After` meanwhile.

**Eviction.** At 100 new usernames per second, evicting a live bucket takes 1,000 s (hypothesis), 100 times its refill; an evicted bucket returns full, adding at most 10 guesses per username per 1,000 s (hypothesis).

Residuals (T1), accepted for M1 (OQ-security-and-identity-15 (a)): no fixed lockout, but a flood shares the username's rate with its owner, exempt only through the success cache; spraying across usernames is bounded only by the hash queue, shown by per-cause refusal counters proposed to Observability; credentials stored at a non-default iteration count differ in timing from the dummy hash; the capacity bound and the restart ramp above.

## Revocation

Ruralz Control delivers one signed revocation list to every Node of a Cluster, checked in memory, outside the Revision digest: a proposed exception to pack 8.1 and 8.4 (OQ-security-and-identity-4). Until then, entries are unavailable.

| What is revoked | Mechanism | Propagation | Planned |
|---|---|---|---|
| A JWT by `jti`; a subject's older tokens; an IdP key by (issuer, `kid`), forcing a JWKS refetch; an API key hash or `auth.basic` username; a client certificate by issuer and serial (per Policy: `config.crl`) | Signed entry, REST API | 5 s or less at p99 with a fresh mark (target) | Planned (M2), pending OQ-security-and-identity-4 |
| An API key, file mode | Removed; CI and Hot Reload | No Ruralz target | Planned (M1) |
| An API key, Control mode | Removed; Rollout | 30 s or less at p95 for 100 Nodes, `all-at-once` (target) (SM-9) | Planned (M2) |
| An Enrollment identity | `ruralz node revoke` | 5 s or less at p99 (target) | Planned (M2) |
| An operator session or token | Raft-replicated set | 1 s or less on connected replicas (target) | Planned (M2) |
| A Plugin publisher | Trust policy change; RZ-CFG-033 | Next activation | Planned (M2) |

- **Integrity.** The leader signs each entry with a monotonic sequence, and a high-water mark (sequence, Cluster, issue time) every 10 s (target) and on each entry; followers and relays forward both unchanged. A gap, or a mark older than 30 s with clocks within 5 s (target), is the degraded state `revocation_sequence_gap`.
- **API keys.** A Revision holding a revoked hash reaches only Nodes reporting a covering sequence. Once the removing Revision is `complete`, Ruralz Control tombstones older Revisions holding the hash as rollback targets (the last 10 `complete` per Cluster, target; OQ-security-and-identity-27) and collects the entry when no Node reports such a digest.
- **Expiry.** A `jti` entry MUST carry the token's `exp`, capped at the maximum lifetime, and expires then; a cut-off expires after that lifetime and meanwhile refuses the subject's tokens without `iat`.
- **Keys.** A `kid` entry never expires on its own, because a leaked key keeps minting fresh tokens. Nodes refuse to load a revoked (issuer, `kid`) from any JWKS fetch and report whether the issuer still serves it; while any Node sees it served, the Cluster is in a degraded state and alerts. An operator MAY remove the entry; Ruralz Control collects it only after no Node has seen the key in that issuer's JWKS for the 6-hour key clamp plus the maximum lifetime, 30 hours (target). Both emit `credential.revoked`.
- **Cap.** 100,000 entries per Cluster (target), 1,000 of them reserved for `kid` entries (target) so other entries never crowd them out, alerting at 80% (target); at the cap new entries are refused and audited. Tombstoning relieves API-key entries; replacing a subject's `jti` entries with one cut-off relieves the rest.
- **Quorum loss.** A revoked session keeps reading until it expires, at most 12 hours (target).
- **Persistence.** A detached restart forgets entries (OQ-security-and-identity-5); file mode reads a watched entry file.

## Plugin sandbox

Plugins are a Node's only third-party code, so the sandbox is the whole TB-2 defense ([WASM plugin system](05-wasm-plugin-system.md), [ADR-0004](../adr/0004-wasm-runtime-wazero.md), [ADR-0005](../adr/0005-plugin-abi-v1.md)). Every control is Planned (M2).

| Control | Design |
|---|---|
| Isolation | wazero; the host is reachable only through Plugin ABI v1 Host Functions |
| Capabilities | Deny by default; RZ-CFG-028 for requests beyond `Plugin.spec.capabilities` |
| Credential headers | Hidden unless a separate, flagged credential Capability is granted (OQ-wasm-plugin-system-3, option (b)) |
| Memory | `limits.memoryBytes` via `WithMemoryLimitPages`, since wazero defaults to 4 GiB ([source](https://github.com/wazero/wazero/blob/main/config.go)); `limits.maxPluginMemoryBytes` caps the Node |
| CPU time | Context deadlines, since wazero has no fuel metering ([source](https://github.com/wazero/wazero/issues/422)) |
| Supply chain | Digest-pinned `image`, Sigstore `enforce`, flagged Capability diffs |
| Failure | Traps fail the Policy (`RZ-PLG`); auth and authz classes closed only |
| Secrets and network | None in Plugin ABI v1 (OQ-wasm-plugin-system-4, -5); no literal keys in `config` |

An instance serves one call at a time, shares no memory across pools and is recycled after 100,000 calls (target); Plugins MUST NOT keep request data in globals (OQ-wasm-plugin-system-18). Unpatched sandbox escapes older than 30 days stay at zero (target) (SM-13).

## Control plane security

Ruralz Control holds the keys to every Cluster, so it denies by default ([Control plane and GitOps](04-control-plane-and-gitops.md)).

### Operator identity, SSO and RBAC

Local accounts, TOTP, sessions and the seven roles follow [RBAC and SSO](04-control-plane-and-gitops.md#rbac-and-sso), Planned (M2); OIDC and SAML SSO for Ruralz Console is Planned (M5), free. Added rules:

- Nobody edits their own bindings or approves their own change.
- `auditor` reads the audit log without write roles; for OQ-control-plane-and-gitops-20, option (a), `viewer` gets none.
- `ruralz bundle push` skips review, so it is off by default under `requireApproval`.

### Enrollment

Tokens, the `Enroll` lockout and certificates follow [Enrollment and mTLS](04-control-plane-and-gitops.md#enrollment-and-mtls); private keys never leave the Node, and certificates name `node.id` and the Cluster, scoping delivery. For OQ-control-plane-and-gitops-19 and -10, option (a): Cluster-scoped minting; expired Nodes re-enroll, since a grace window extends stolen identities.

### Signed Revisions and Plugin artifacts

[ADR-0017](../adr/0017-artifact-signing.md) fixes what is signed; these trust policies are Planned (M2).

| Trust policy | Contents | Lives in | Default |
|---|---|---|---|
| Anchor sets | Online signing keys signed by the offline root, plus the Plugin trust policy | Enrollment and root-signed `TrustUpdate` | Always verified |
| OCI Revision trust policy | Exact keyless issuer and subject pairs, or keys | Node process configuration (`RURALZ_TRUST_POLICY_FILE`) | `enforce` |
| Plugin trust policy | Publisher identities or keys | Ruralz Control process configuration; Node's in file mode | `enforce`; `warn` in `ruralz dev run` |

Under `enforce`, an empty trust policy rejects everything (RZ-CFG-033); identity patterns are opt-in. Verification is offline (pack 8.14); trust policies never live in a Bundle. For OQ-control-plane-and-gitops-9, option (b): verify commit signatures where approvals depend on authorship. For OQ-control-plane-and-gitops-24, option (a): after an emergency key removal, a Node refuses to boot a candidate or Last-Known-Good accepted from that key after `compromisedSince`, staying not ready until the Control Stream delivers; older ones boot.

Proposed (OQ-security-and-identity-26): `ruralz bundle push` adds signed Environment and sequence annotations; file-mode Nodes reject other Environments and sequences below a high-water mark kept in `${RURALZ_DATA_DIR}/lkg/`, and a rollback re-pushes with a new sequence. Until then, Nodes check only digest and signature.

### Admin ports

For OQ-system-overview-6: 9901 and 9902 bind all interfaces for kubelet probes, and every path but `/healthz` and `/readyz` needs a credential on every interface, loopback included, because every container in a Pod, sidecars included, shares loopback. For OQ-security-and-identity-7 (a), amending pack section 2, three process settings, never Bundle fields and each an absolute path, hold the admin credentials (TB-9), Planned (M1):

| Setting | Holds | Admits |
|---|---|---|
| `RURALZ_ADMIN_TOKEN_FILE` | The operator token | Every path |
| `RURALZ_ADMIN_METRICS_TOKEN_FILE` | The metrics token | `/metrics` |
| `RURALZ_ADMIN_TLS_DIR` | `tls.crt` and `tls.key` for admin TLS, optional `ca.crt` | Certificates chaining to `ca.crt` with `clientAuth` usage: `/metrics`, plus operator paths while an operator token is set |

A token file is a regular file holding 22 to 4,096 bytes of RFC 6750 `b64token` characters once trailing whitespace is trimmed; any other token file, or a TLS directory without `tls.crt` or `tls.key`, refuses start (exit 2). Token files are re-read on change, so a rotated token applies without a restart and an invalid one keeps the last valid token; the TLS directory is read at start.

Bearer tokens are compared in constant time over SHA-256 digests and accepted only over TLS or from a loopback peer; without the operator token, `/tap`, `/config/dump` and `/debug/*` are off. Failures are 401 problem documents with RZ-AUTH-001 (no credential) or RZ-AUTH-002 (invalid) and `WWW-Authenticate: Bearer realm="ruralz-admin"`; admin without TLS is a reported cleartext hop. `/tap` and `/config/dump` use is logged with path, peer and credential kind, never the token.

### Audit log

Every state change is audited, Raft-committed before the write returns and hash-chained ([Audit and security](04-control-plane-and-gitops.md#audit-and-security)). Chain heads MUST be exported off-box, so OQ-control-plane-and-gitops-12 should block Planned (M2); a restore appends a fork record.

### Release supply chain

[Release, versioning and compatibility](../engineering/04-release-versioning-and-compatibility.md) implements, all Planned (M1): signed images, a CycloneDX SBOM per artifact as an attestation, and SLSA Build Level 3 provenance.

## Compliance

Every audit event carries time, actor, role, source address, target, outcome and any Revision digest, never a secret. All are Planned (M2) except `sso.config.changed`, Planned (M5).

| Event | Emitted when |
|---|---|
| `session.created`, `token.created` | An operator signs in; an API token is issued |
| `session.failed` | Sign-in failures, aggregated per minute |
| `session.revoked` | A session or token is revoked |
| `rbac.binding.changed` | A role binding changes |
| `source.changed`, `crd.changed` | A Git source, credential reference or CRD changes |
| `revision.recorded`, `revision.pushed` | A Revision is built and signed, or pushed by `ruralz bundle push` |
| `revision.rejected` | Validation or a digest check fails |
| `revision.tombstoned` | A Revision stops being a rollback target |
| `writeback.submitted` | A Ruralz Console write-back is submitted |
| `rollout.changed` | A Rollout changes state or is approved |
| `node.token.issued` | An Enrollment token is issued |
| `node.enrolled` | A Node enrolls |
| `node.revoked` | `ruralz node revoke` runs |
| `credential.revoked` | A revocation entry is added, collected, removed by an operator or refused |
| `trust-policy.changed` | A trust policy, anchor set or signing key changes |
| `signature.failed` | A signature fails at ingest or on a Node |
| `control.backup.created`, `control.restored` | A backup completes or is downloaded; a restore completes |
| `sso.config.changed` | An SSO provider setting changes |

Data-plane decisions are telemetry, not audit events. Other evidence:

- FIPS 140-3: the [FIPS build](#fips-build).
- Data residency, Planned (M3): for OQ-ai-llm-gateway-9, option (a), allowed Regions on `AIModel` and embedding providers plus a named Node Region input. A request whose `AIModel` excludes the serving Node's Region fails closed with an `RZ-AI` code before any provider call, so no content is generated, embedded, cached or logged; `ai.semantic-cache` embedding providers MUST share that Region. Residual (T21): access-log metadata stays in the serving Region, and operators steer clients.
- Vulnerabilities: a published security policy, SM-13 and OQ-vision-and-positioning-6 (Cyber Resilience Act).
- Degradation metrics: fail-open Rate Limits ([ADR-0008](../adr/0008-rate-limiting-local-bucket-and-gcra.md)), cleartext hops, stale JWKS or marks.

## Open questions

| ID | Question | Options | Owner | Blocking? |
|---|---|---|---|---|
| OQ-security-and-identity-4 | How do revocations bypass the Revision? | (a) Signed entries and high-water mark, Node-signed `Ack`s, watched file, amending pack 8.1 and 8.4 (proposed); (b) State Store; (c) Revisions only | security-and-identity, control-plane-and-gitops | Yes, Planned (M2) |
| OQ-security-and-identity-5 | Do revocations survive a detached restart? | (a) No; (b) Persisted, amending pack 8.11 | security-and-identity | Yes, Planned (M2) |
| OQ-security-and-identity-10 | How does a Node authenticate to Vault? | (a) Kubernetes; (b) AppRole; (c) Token file | security-and-identity | Yes, for `vault` |
| OQ-security-and-identity-11 | How is JWT signing served? | (a) Plugin with signing Host Function; (b) Built-in type, amending pack section 10 (proposed) | security-and-identity | Yes, Planned (M2) |
| OQ-security-and-identity-12 | How is GCP authentication served? | (a) Plugin; (b) JWT-bearer fields as authored in [Upstream authentication](#upstream-authentication), for registration (proposed); (c) New type | security-and-identity, configuration-model | Yes, Planned (M2) |
| OQ-security-and-identity-13 | How do OPA and Cedar get policies? | (a) Inline; (b) OCI; (c) Both, as authored in [OPA and Cedar schemas](#opa-and-cedar-schemas), for registration (proposed) | security-and-identity, configuration-model | Yes, Planned (M2) |
| OQ-security-and-identity-14 | Should `authz.geoip` enrich upstream requests? | (a) No; (b) Header; (c) `source.country` | security-and-identity | No |
| OQ-security-and-identity-17 | Which fields set client timeouts? | (a) Gateway `limits`; (b) Fixed | data-plane | No |
| OQ-security-and-identity-19 | One switch requiring TLS everywhere? | (a) No (current); (b) Gateway field | configuration-model | No |
| OQ-security-and-identity-20 | Record a `go1.27` build-tag file? | (a) Yes (proposed); (b) Wait | tech-stack-and-libraries | No |
| OQ-security-and-identity-23 | Keyed digests; short-key code? | (a) SHA-256, RZ-CFG code (current); (b) HMAC-SHA-256 | configuration-model | No |
| OQ-security-and-identity-25 | Which field shares cache entries across principals? | (a) None; (b) Flag | configuration-model | No |
| OQ-security-and-identity-26 | Which signed annotations bind an OCI Revision? | (a) Environment and sequence, amending pack 8.11 and 8.14 and System overview file-mode rollback (proposed); (b) Digest | security-and-identity | Yes, Planned (M2) |
| OQ-security-and-identity-27 | Codes for cap overflow and tombstoned rollbacks; target count? | (a) Two `RZ-CP` codes, 10 targets (proposed); (b) Existing codes | control-plane-and-gitops | Yes, Planned (M2) |
| OQ-security-and-identity-28 | Where does transport-time signing live? | (a) Hook after the last `onUpstreamRequest` Filter, amending pack sections 8.12 and 10 (proposed); (b) Forbid later mutation | security-and-identity | Yes, for `auth.upstream-sigv4` |
| OQ-security-and-identity-29 | Should TB-1, TB-3, TB-4 and TB-12 read "TLS when configured; cleartext reported"? | (a) Yes (proposed); (b) Require TLS | system-overview | No |
| OQ-security-and-identity-30 | Which codes reject the engine ceiling and an uncredentialed State Store URL? | (a) New RZ-CFG codes (proposed); (b) RZ-CFG-015 and RZ-CFG-026 | configuration-model | Yes, Planned (M2) |
| OQ-security-and-identity-31 | Should pack 8.4 (8091 and 8092 rows, section 2 Control Stream row) and System overview's TB-3, TB-5, TB-7, TB-8 and TB-11 adopt the `Enroll` and `Join` [transports](#transport-security) and the [boundary mapping](#threat-model-and-trust-boundaries) above? | (a) Amend (proposed); (b) New TB IDs | system-overview | Yes, Planned (M2) |

Closed:

- For M1: OQ-security-and-identity-1 (b), [JWT and OIDC](#jwt-and-oidc); -6 (c), [Client address](#ip-filtering-and-geoip); -7 (a), [Admin ports](#admin-ports); -9 (a), merging OQ-data-plane-8, [codes](#rz-auth-decision-codes); -15 (a), [throttle](#pre-authentication-throttling); -21 (a), [Request hardening](#request-hardening); -22 (a), [Secrets](#secrets); -24 (a), [Consumers](#consumers-and-tiers).
- OQ-security-and-identity-16; -2, -3 and -18 with option (a), registered as authored by the Configuration model; -8 by Control plane, `RURALZ_TRUST_POLICY_FILE`.
- Answered above: OQ-system-overview-6; OQ-configuration-model-5, -13, -15, -17; OQ-tech-stack-and-libraries-6; OQ-wasm-plugin-system-3, -18; OQ-ai-llm-gateway-9, -12.

Recommended, pending host documents: OQ-ai-llm-gateway-14 (b); OQ-ai-llm-gateway-19 (b), plus (c) for untrusted tenants; OQ-multi-protocol-7, -9 (a); OQ-observability-4, -7 (b); OQ-release-versioning-and-compatibility-5 (a). For OQ-tech-stack-and-libraries-24, option (a); -17 and -19 need the same research addendum, with OIDC only as the -19 fallback.
