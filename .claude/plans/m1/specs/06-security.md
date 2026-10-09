# M1 area 6: Security and identity Filters and TLS

Design reader spec for milestone M1 "Core gateway". Everything here is derived from the reviewed docs; items marked **[proposed]** are decisions this spec makes where the docs are silent (each is repeated in section 9 so an owner can overrule it).

Reference abbreviations (file + section heading):

| Tag | File |
|---|---|
| SEC | `docs/architecture/08-security-and-identity.md` |
| CFG | `docs/architecture/02-configuration-model.md` |
| DP | `docs/architecture/03-data-plane.md` |
| SYS | `docs/architecture/01-system-overview.md` |
| OBS | `docs/architecture/10-observability.md` |
| PERF | `docs/architecture/12-performance-budgets-and-benchmarking.md` |
| WASM | `docs/architecture/05-wasm-plugin-system.md` |
| PACK | `docs/_meta/foundation-pack.md` |
| ADR11 | `docs/adr/0011-expressions-and-authorization-engines.md` |
| ADR17 | `docs/adr/0017-artifact-signing.md` |
| TECH | `docs/engineering/01-tech-stack-and-libraries.md` |
| LAYOUT | `docs/engineering/02-repository-layout-and-conventions.md` |
| TEST | `docs/engineering/03-testing-and-quality-strategy.md` |
| CLI | `docs/reference/01-cli-and-api-surface.md` |
| HA | `docs/operations/04-high-availability-and-disaster-recovery.md` |
| ZDU | `docs/operations/02-zero-downtime-upgrades-and-hot-reload.md` |
| RM | `docs/roadmap/01-roadmap-and-milestones.md` |

## 1. Scope

Every M1 roadmap item of this area (RM "M1 scope", Security row, plus the security-owned parts of other rows):

| # | Item | Source |
|---|---|---|
| S1 | `auth.jwt` (JWT, OpenID Connect, OAuth2 access tokens; JOSE library jwx v4): issuers, audiences, `exp`/`nbf`, allowed `alg`, `kid`, JWKS fetch, caching, prefetch, refresh | RM "M1 scope" (Security); SEC "JWT and OIDC"; TECH "Library catalog" (JOSE row) |
| S2 | `auth.api-key` (SHA-256 digests, `secretRef`-held keys hashed at load, removal in file mode = Revision without the key) | RM "M1 scope"; SEC "Authentication", "Consumers and tiers", "Revocation" (file-mode row); CFG "Consumer" |
| S3 | `auth.basic` (PBKDF2-HMAC-SHA-256) and the pre-authentication throttle | SEC "Basic and mTLS schemas", "Pre-authentication throttling" |
| S4 | `auth.mtls` (client chain, `subjects`/`uriSan`, `crl`), listener client-certificate request mode, 421 rule | SEC "Mutual TLS", "Basic and mTLS schemas" |
| S5 | Consumer matching and binding rules (API key digest, JWT issuer+`subject`/`claims`, `oauthClients`, basic username, certificates), the `consumer` and `auth` CEL values, rule 1 and rule 2 | SEC "Authentication", "Consumers and tiers"; CFG "Variables" |
| S6 | `authz.cel` (Security Policies Engine, first part) | SEC "Authorization"; ADR11 "Decision outcome" |
| S7 | `authz.ip` with the client-address rule (`Gateway.spec.trustedProxies`, `listeners[].proxyProtocol`, `X-Forwarded-For`/`Forwarded`, PROXY v2) | SEC "IP filtering and GeoIP"; CFG "Gateway" |
| S8 | `auth.upstream-oauth2` (client credentials, single-flight token cache, 2/3 refresh, `https` `tokenUrl`) | SEC "Upstream authentication"; PACK §10 |
| S9 | `cors` | RM "M1 scope"; PACK §10; CFG "Policy"; DP "Short-circuit", "Failure semantics" |
| S10 | TLS: listeners (8443, `minVersion`, SNI, TLS 1.2 suites, certificate rotation by `file` watch), Upstreams (TB-3, verified, optional client cert), State Store (`rediss://`, TB-4), OTLP (TB-12), IdP (TB-10); cleartext reporting | RM "M1 scope" ("TLS to IdP JWKS endpoint (TB-10), Upstreams, State Store, OTLP"); SEC "Transport security", "Certificate rotation" |
| S11 | Admin authentication on 9901 (`RURALZ_ADMIN_METRICS_TOKEN_FILE`, `RURALZ_ADMIN_TOKEN_FILE`, `RURALZ_ADMIN_TLS_DIR`) | SEC "Admin ports"; CLI "Admin API"; DP "Admin endpoints"; OQ-security-and-identity-7 |
| S12 | Secrets rules 1 to 7 on Nodes: `RURALZ_SECRET_ROOT`, `RURALZ_SECRET_*` env prefix, `RURALZ_FETCH_ALLOW` and SSRF refusal for `jwksUrl`, `tokenUrl`, Endpoints, discovery results; `security` diff impact | SEC "Secrets"; OQ-security-and-identity-22 |
| S13 | Redaction of secrets and credential headers in logs, traces, metrics, `/config/dump`, diffs, `/tap`; redaction unit tests; secret leak tests | RM "M1 scope"; SEC "Secrets" rule 2; OBS O4; TEST "Security scanning", "End-to-end tests" |
| S14 | Digest checks on every Revision (and Plugin artifact) by full `sha256:<64 hex>` | RM "M1 scope"; ADR17 "Decision outcome" (row 1); PACK §8.1, §8.14; ZDU "Validation gate" step 2 |
| S15 | Security telemetry: `ruralz_auth_*`, `ruralz_security_cleartext_hops`, `ruralz_listener_tls_handshake_duration_seconds`, degraded reasons `jwks_stale`, `secret_rotation_failed`, `cleartext_hop`, `state_store_unauthenticated` | OBS "Ruralz Gateway metrics", "Degraded states" |

Out of scope here but adjacent (owned by other areas, consumed or supplied below): request hardening parsing limits (DP), path normalization and OQ-security-and-identity-21 (DP), the Filter Chain executor (DP), CEL environment (CFG/CEL area), Revision canonicalization and digest computation (config area), release signing (release area).

## 2. Normative requirements

### 2.1 Auth class: common rules

1. `auth.jwt`, `auth.api-key`, `auth.basic`, `auth.mtls` MUST be Filter class `auth`, run only in `onRequestHeaders`, be attachable at Gateway and Route scope only, default slot `auth`, `failureMode` default `closed` and `closed` only (`open` is RZ-CFG-029 at validation). They MUST NOT call the State Store. (PACK §10, §8.10; SEC "Authentication")
2. Rule 1: when a Route's effective chain holds at least one auth-class Policy (M2: including `plugin` with `filterClass: auth`) and `Policy.spec.when` skipped every one of them, the request MUST fail with 401 RZ-AUTH-001. A `when` that errors at runtime makes the (closed) auth Policy run. The executor decides this after the last auth-class Policy of `onRequestHeaders`. (SEC "Authentication" rule 1; CFG "Allowed places")
3. Rule 2: a second binding fails with 401 RZ-AUTH-002. This spec reads it as: once any auth-class Policy has succeeded on a request, any later auth-class Policy that also succeeds MUST fail the request with 401 RZ-AUTH-002, whether or not either bound a Consumer **[proposed interpretation; see 9.3]**. With the complementary-`when` pattern, a `when` error therefore makes both run and the request fail closed. (SEC "Authentication" rule 2, "Accepting more than one credential type")
4. No auth-class Policy in the effective chain: `auth` and `consumer` MUST be null (anonymous); authz-class Policies still run. (SEC Figure 2)
5. Decision order per request (SEC Figure 2), first failure wins: credential present (else 001) → Filter can decide (005 unknown `kid`, 006 no usable keys, 007 throttled) → signature, hash or chain valid (002; 003 for token time/issuer/audience claims) → revocation (004; M2 hook, always "not revoked" in M1) → Consumer match (API key or basic without a match: 002; JWT or mTLS without a match: `auth` set, `consumer` null; more than one distinct Consumer: 002).
6. On success the request identity MUST be set: `auth.method` is the Policy type without the `auth.` prefix (`jwt`, `api-key`, `basic`, `mtls`) **[proposed]**; `auth.claims` is the verified JWT payload for `jwt` and an empty map for the other methods **[proposed]**; `consumer` is the bound Consumer or null. (CFG "Variables": `auth` = `method`, `claims`; `consumer` = `name`, `tier`, `tags`, `labels`, `quotas`)
7. Rejections MUST be RFC 9457 problem documents (`application/problem+json`, members `title`, `status`, `code`, `requestId`) written through the Data plane's error writer; titles are generic (`Unauthorized`, `Forbidden`, `Too Many Requests`) and the body never echoes a credential, username, claim or reason detail. (DP "Error response format"; SEC "RZ-AUTH decision codes": "Client messages are generic")
8. Every 401 MUST carry `WWW-Authenticate` (RFC 9110 §11.6.1) **[proposed values]**: `auth.jwt` → `Bearer realm="ruralz"`; `auth.basic` → `Basic realm="ruralz", charset="UTF-8"`; `auth.api-key` → `APIKey header="<config.header>"`; `auth.mtls` → none (documented deviation); a 401 RZ-AUTH-020 from a failed `upstream-auth` Policy → none, because it is not a client credential failure (architecture 3.4 scopes challenges to auth-class Policies; OQ-security-and-identity-9 (a)). An RZ-AUTH-001 from rule 1 carries the challenges of every auth-class Policy in the chain. Every 429 RZ-AUTH-007 MUST carry `Retry-After` in integer seconds, at least 1.
9. Credential forwarding: after success `auth.api-key` MUST delete every value of its `config.header` and `auth.basic` MUST delete `Authorization` from the request forwarded upstream; `auth.jwt` MUST forward `Authorization` unchanged; `auth.upstream-oauth2` overwrites it on its leg. (SEC "Request hardening", "Credential forwarding" row)
10. Every auth- and authz-class decision MUST increment `ruralz_auth_decisions_total{policy, result, code}` with `result` = `allow` or `deny` and `code` the RZ code on deny, empty on allow. (OBS "Ruralz Gateway metrics")
11. Filters MUST NOT log, trace or label a credential, token, password, key, `Authorization` value, username, subject or claim; the Filter Chain executor records only the RZ code on the `ruralz.filter.<name>` span. (SEC "Secrets" rule 2; OBS O4)

### 2.2 Consumer index and binding

12. Consumers come only from the Bundle through M3 (OQ-configuration-model-17). The config loader MUST compile one immutable credential index per snapshot, read without locks. Literal `credentials.apiKeys[].hash` digests live in the snapshot; digests of `secretRef`-held keys live in a side index beside the snapshot, replaced copy-on-write on secret rotation; the raw key is discarded after hashing and never kept. (DP "Configuration snapshots and hot reload"; CFG "Consumer"; SEC "Consumers and tiers")
13. A `secretRef`-held API key MUST be normalized by trimming leading and trailing SP, HTAB, CR and LF **[proposed; a header value can never carry them]** and then rejected when shorter than 22 bytes (SEC "Consumers and tiers": "rejected under 22 base64url characters"): at activation the Revision is rejected with RZ-CFG-026 **[proposed code, pending OQ-security-and-identity-23's "short-key code"]**; on rotation the last value is kept and `ruralz_config_secret_rotation_failures_total{provider}` increments (degraded reason `secret_rotation_failed`).
14. API key binding: the digest is SHA-256 over the exact bytes of the single `config.header` value as `net/http` delivers it; stored form `sha256:` + 64 lowercase hex (schema pattern `^sha256:[0-9a-f]{64}$`). Header absent or empty → 401 RZ-AUTH-001; more than one field line of that header → 401 RZ-AUTH-002 **[proposed]**; digest unknown → 401 RZ-AUTH-002; digest held by two distinct Consumers → 401 RZ-AUTH-002. The lookup is a map on the 32-byte digest (no plaintext comparison exists). (SEC "Authentication" table)
15. `auth.api-key` `config.header`: when absent the Filter MUST read `x-api-key` **[proposed default; the schema registers no default, see 9.6]**; the name is matched case-insensitively.
16. JWT binding (SEC "Consumers and tiers", Binding row): a Consumer matches a verified token when one of its `credentials.jwt` entries has `issuer` byte-equal to the token `iss` and either `subject` byte-equal to the string claim `sub`, or every `claims` entry equal to a top-level claim whose JSON value is a string byte-equal to the expected value (non-string claims never match **[proposed]**).
17. OAuth client binding (OQ-security-and-identity-24, option (a)): a Consumer matches when some `credentials.oauthClients[].clientId` equals the string claim `client_id`, or, when `client_id` is absent, the string claim `azp`, and that Consumer has a `credentials.jwt` entry whose `issuer` equals the token `iss`.
18. The set of Consumers matched by rules 16 and 17 is de-duplicated; one Consumer binds; zero leaves `consumer` null with `auth` set; two or more MUST fail with 401 RZ-AUTH-002 ("a double match needs an error", SEC "Consumers and tiers"; OQ-security-and-identity-1).
19. Basic binding: `credentials.basic` is keyed by `username`, matched byte-exact against the presented username. mTLS binding: `credentials.certificates` entries match a leaf that `auth.mtls` verified, `subject` against the leaf's RFC 4514 subject string (rule 49) and `uriSan` against each URI SAN, exactly; a leaf matching two Consumers (for example subject → A, `uriSan` → B) MUST fail with 401 RZ-AUTH-002.
20. Static checks the config validator MUST run (this area supplies the check functions to `internal/config`): a `credentials.basic` `username`, or a `credentials.certificates` `subject` or `uriSan`, declared by two Consumers is RZ-CFG-035; `credentials.basic[].iterations` outside 600,000 to 1,000,000 is RZ-CFG-036; an `auth.jwt` `jwksUrl` or `auth.upstream-oauth2` `tokenUrl` whose scheme is not `https` or whose host is empty is RZ-CFG-037; an `authz.ip` or `trustedProxies` entry that is not a CIDR or bare address is RZ-CFG-005; an `auth.mtls` `subjects` rule with both or neither of `subject`/`uriSan` is RZ-CFG-005 (schema). (CFG "Error codes", "Registered from feature documents")
21. The compiled Consumer exposed to CEL and other Filters MUST carry `name`, `tier`, `tags`, `labels` (`metadata.labels`), and `quotas` (the list of quota names) plus a by-name quota map for `quota` and `ai.token-budget`. (CFG "Variables")
22. Every identity MUST expose a non-secret principal key for cache partitioning: `consumer.name` when bound, else `jwt:` + `iss` + `#` + `sub`, else `mtls:` + the certificate subject **[proposed encoding]**; the Response Cache and later the Semantic Cache use it on authenticated Routes. (SEC "Consumers and tiers", Cache partitioning row; AI doc "Partitions")

### 2.3 `auth.jwt`

23. Config (registered, `+ruralz:open`): `config.issuers[]` (map keyed by `issuer`, at least one), each with required `issuer` (string), `jwksUrl` (`https`, RZ-CFG-037) and `audiences` (set, at least one). (CFG "Policy", fields fixed table; `pkg/config/v1alpha1.AuthJWTConfig`)
24. Token source: the `Authorization` field only, scheme `Bearer` matched case-insensitively, followed by one or more SP and the token (RFC 6750 §2.1) **[proposed: no query or cookie source in M1]**. Absent field, or a non-`Bearer` scheme → 401 RZ-AUTH-001; more than one `Authorization` field line, empty token, or a token over 16 KiB **[proposed cap]** → 401 RZ-AUTH-002.
25. Parsing: only JWS Compact Serialization with exactly three base64url (unpadded) segments; a five-segment JWE or JSON serialization → 401 RZ-AUTH-002. The protected header MUST be a JSON object without duplicate member names; the payload MUST be a JSON object without duplicate member names (decode with `encoding/json/v2`, which rejects duplicates) **[proposed]**; failure → 002.
26. Algorithms: `alg` MUST be one of `RS256`, `PS256`, `ES256`, `EdDSA`; anything else (including `HS256`, `none`, `RS384`, `ES384`, `Ed25519`) → 401 RZ-AUTH-002. A `crit` header → 002 (no extensions are supported). The header parameters `jku`, `x5u`, `jwk` and `x5c` MUST be ignored: keys come only from the issuer's cached JWKS. `typ` and `cty` are not checked. (SEC "JWT and OIDC")
27. Issuer selection: the (unverified) payload claim `iss` MUST be a string byte-equal to exactly one `config.issuers[].issuer`; otherwise 401 RZ-AUTH-003. Unverified claims are used only for this selection and never exposed. (SEC "JWT and OIDC", bullet 1)
28. Key selection: if the issuer has no usable key set (never fetched successfully, or expired) → 401 RZ-AUTH-006 and an asynchronous fetch is requested under the 30 s limiter of rule 36. If the token has a `kid` not present among usable keys compatible with `alg` → 401 RZ-AUTH-005 and one asynchronous refresh is requested (rule 36). A token without `kid` uses the single usable key compatible with `alg` when exactly one exists, else 401 RZ-AUTH-002 without a refresh **[proposed]**.
29. Signature: verified with the selected public key and the header `alg` only (jwx `jws.VerifyCompactFast`, falling back to `jws.Verify(buf, jws.WithKey(alg, key))` on `jws.ErrNonMinimalHeader`); failure → 401 RZ-AUTH-002.
30. Claims, after the signature, with a fixed clock skew of 60 s **[proposed target; OQ-security-and-identity-1 option (b)]** and maximum token lifetime 24 h (target): `exp` MUST be present and a JSON number (missing → 003; non-number → 002); `exp` ≤ now − skew → 003; `exp` > now + 24 h → 003 ("over-long `exp`"); `nbf`, when present, > now + skew → 003; `aud` (string or array of strings) MUST contain at least one value byte-equal to a configured `audiences` entry, missing or no match → 003; `iat` is not checked. Numeric dates are seconds, fractions truncated. (SEC "JWT and OIDC" bullets 2 to 3; "RZ-AUTH decision codes")
31. Revocation hook (M2): after rule 30 and before binding, the Filter MUST call `identity.Revoker.RevokedJWT(iss, kid, jti, sub, iat)`; M1 wires a no-op that never revokes. RZ-AUTH-004 is reserved for it. (SEC "Revocation")
32. Budget: `auth.jwt` with RS256 and a 2,048-bit key from the cached JWKS: 45 µs p50, 100 µs p99 (target); allocations reported, not gated. There is no verified-token cache in M1 (the PB fixture rotates 10,000 tokens without one). (PERF "Per-stage latency budget", jwt-bench fixture)
33. Link only `github.com/lestrrat-go/jwx/v4/jwk`, `/jws` and `/jwa`; MUST NOT import `/jwt` or `/jwe`: `jwt` imports `jwe`, whose `jwebb` links `golang.org/x/crypto/pbkdf2`, which the depgate G3 check (empty x/crypto delegation allowlist) would reject. Claims are decoded by Ruralz code. MUST NOT call jwx global `Settings` functions (no mutable package state). (TECH "FIPS build", `internal/tool/depgate/policy.go` `cryptoDelegation`; LAYOUT "Code conventions")

### 2.4 JWKS cache and fetch

34. One Node-wide JWKS manager keyed by (`issuer`, `jwksUrl`), shared by all `auth.jwt` Policies and snapshots, carried across Hot Reloads; after each swap, entries referenced by no live (active, retired, closing or ending) snapshot are dropped. Reads on the request path are lock-free (atomic pointer per entry). (SEC "JWT and OIDC" bullet 4; DP "Goroutines": JWKS is a fixed background goroutine)
35. Prefetch: when compiling a Revision the loader MUST fetch every issuer not already cached, at most 16 concurrently, within 5 s total (target), reusing unchanged issuers' keys, and MUST activate even if a fetch fails (those issuers answer 006 until a later fetch succeeds). Prefetch runs concurrently with the rest of compilation. (SEC "JWT and OIDC" bullet 4)
36. Lifetime: a successful fetch's key set lives for `Cache-Control: max-age` clamped to [5 min, 6 h], or 1 h when `max-age` is absent or the response says `no-cache` (target); `no-store` is treated as `no-cache` **[proposed]**. Refresh is scheduled at half-life, with ±10% jitter **[proposed]**. After a failed refresh the last set keeps serving until its expiry while degraded reason `jwks_stale` is raised; retries use full-jitter exponential backoff from 1 s to 60 s **[proposed]**, never later than expiry; past expiry tokens get 401 RZ-AUTH-006. An unknown-`kid` or missing-set refresh runs at most once per issuer per 30 s (target), and never before a `Retry-After` (seconds or HTTP date) from the last 429 or 503 **[proposed cap: 6 h]**. Refreshes never block a request. (SEC "JWT and OIDC" bullets 5 to 6; HA FC-16)
37. Fetch: `GET jwksUrl` through the egress client (section 2.14) with TLS 1.2 or newer, server verified against system roots, `Accept: application/jwk-set+json, application/json`, no cookies, no credentials; redirects followed only `https`→`https`, at most 3 hops **[proposed]**, each hop re-checked by the egress guard; per-fetch timeout 5 s **[proposed]**; status other than 200 fails; body over 1 MiB **[proposed]** fails. (SEC "Transport security" IdP row; "Secrets" rule 6)
38. Parse with `jwk.Parse`; for each key use only public material (`jwk.PublicKeyOf`). A key is usable only when `use` is absent or `sig`, `key_ops` is absent or contains `verify`, `alg` is absent or one of the four allowed values, and its type fits: RSA with a modulus of at least 2,048 bits **[proposed]** for RS256/PS256, EC P-256 for ES256, OKP Ed25519 for EdDSA. Symmetric, X25519, ML-DSA and other keys are ignored. A document with zero usable keys, or more than 256 keys **[proposed]**, is a failed fetch (the previous set keeps serving). Duplicate `kid` values are kept as a list, tried in document order for the token's `alg`.
39. `ruralz_auth_jwks_age_seconds{policy}` reports now minus the oldest successful fetch time among the Policy's issuers; `jwks_stale` is raised while any issuer is serving past its refresh point after a failure, or has no usable set. (OBS "Ruralz Gateway metrics", "Degraded states")
40. M2 hook: the manager MUST consult `identity.Revoker.RevokedKey(issuer, kid)` on every load and refuse revoked keys (SEC "Revocation", Keys bullet); M1 no-op.

### 2.5 `auth.basic` and the pre-authentication throttle

41. Config: none; reads `Authorization: Basic` (scheme case-insensitive, RFC 7617). Absent field or another scheme → 401 RZ-AUTH-001; malformed value (bad base64, no `:`, more than one field line, username over 256 bytes or password over 1,024 bytes **[proposed caps]**) → 401 RZ-AUTH-002. Username is the bytes before the first `:`, password the rest. (SEC "Basic and mTLS schemas")
42. Stored hash `pbkdf2-sha256:<salt>:<key>`: base64url without padding of a 16-byte salt (22 chars) and a 32-byte key (43 chars); verification computes `crypto/pbkdf2.Key(sha256.New, password, salt, iterations, 32)` with the credential's `iterations` (600,000 to 1,000,000; default 600,000, target) and compares with `crypto/subtle.ConstantTimeCompare`. Mismatch or undeclared username → 401 RZ-AUTH-002. (SEC "Basic and mTLS schemas"; CFG "Consumer")
43. Throttle state is Node-wide (shared by every `auth.basic` Policy), in memory, lost on restart; a per-process 32-byte HMAC key from `crypto/rand` keys every structure (SEC "Pre-authentication throttling"):
    - Success cache: key HMAC-SHA-256(k, len(username) ‖ username ‖ password); entry = Consumer credential digest (SHA-256 over the credential's `hash` and `iterations` **[proposed definition]**), bound Consumer, `verifiedAt`, `lastHit`, jitter j ∈ [0, 0.2]; valid while now < min(`lastHit` + 60 s, `verifiedAt` + 15 min × (1 − j)) (target); 64 shards, 10,000 entries per Node, LRU (target). A hit skips throttle and hash, but MUST still pass the revocation hook; a hit whose recorded credential digest differs from the current snapshot's credential for that username is a miss and is evicted (this implements "a swap keeps unchanged entries and evicts only changed or removed credentials").
    - Username bucket: one per presented username, declared or not, keyed by HMAC-SHA-256(k, username), in one 64-shard LRU of 100,000 (target); burst 10, refill 1 per second (target). A bucket is created only after the hash queue admits the attempt, at most 100 creations per second per Node (target; token bucket, burst 100 **[proposed]**).
    - Hash queue: `slots` = max(1, `GOMAXPROCS`/4) read at start; at most 4 × `slots` waiters; wait deadline 1 s (target), also bounded by the request context.
    - Delay: an empty username bucket holds at most 2 waiters (1,024 per Node) for up to 2 s (target).
    - Source key (on in M1, since OQ-security-and-identity-6 is answered): per `source.ip`, IPv6 aggregated to /64, 10 failed verifications per minute (target); modelled as a bucket of 10 refilled 10 per 60 s in a 64-shard LRU of 100,000 **[proposed size]**; an empty source bucket refuses new non-cached attempts.
    - Undeclared usernames go through identical steps plus a dummy PBKDF2 at the default 600,000 iterations with a per-process random salt, then 002, so neither timing nor status reveals existence.
44. Order for a success-cache miss **[proposed, consistent with the table]**: (a) source bucket empty → 429; (b) existing username bucket: take a token, else join the delay (overflow → 429), then acquire a hash slot (queue full or 1 s passed → 429); (c) no bucket: take a hash-queue position first (full → 429), then the creation cap (exceeded → release, 429), create the bucket full and take a token, then wait for a slot. Every refusal is 429 RZ-AUTH-007 with `Retry-After` (seconds until the next username token for delay overflow, else 1) and happens before any hash; each is counted by cause (`hash_queue`, `bucket_creation`, `delay`, `source`). A failed verification debits the source bucket. (SEC "Pre-authentication throttling", Refusal row; Figure 2)

### 2.6 `auth.mtls` and client certificates

45. Config (registered, closed): `caCertificate` required `SecretValue` (PEM bundle of trusted client CAs); `subjects` optional set of rules, each exactly one of `subject` (RFC 4514, exact) or `uriSan` (exact); `crl` optional `SecretValue` (PEM CRLs). (SEC "Basic and mTLS schemas")
46. Listener mode: a listener MUST request an unverified client certificate (`tls.RequestClientCert`) exactly when some Route bound to it attaches `auth.mtls` in its effective chain; the mode is read from the current snapshot in `GetConfigForClient`, so it applies to new handshakes. A Hot Reload that changes the mode MUST send GOAWAY on HTTP/2 connections, close idle connections and set `Connection: close` on busy HTTP/1.1 ones. (SEC "Mutual TLS")
47. A Route with `auth.mtls` reached over a connection whose handshake did not request a certificate (including a coalesced HTTP/2 connection) MUST get 421; the code is missing from the registry: register `RZ-AUTH-008` 421 "`auth.mtls` Route reached over a connection that requested no client certificate" **[proposed]**. A connection that requested one but got none → 401 RZ-AUTH-001.
48. Verification by the Filter against its own anchors: `x509.Certificate.Verify` with `Roots` = the Policy's current CA pool, `Intermediates` = the other presented certificates, `KeyUsages` = `[ExtKeyUsageClientAuth]`, `CurrentTime` = now; additionally the leaf MUST list `ExtKeyUsageClientAuth` explicitly (Go treats a leaf without EKU as any usage). Failure → 401 RZ-AUTH-002. Results are cached per connection, keyed by (Policy, snapshot), at most 8 entries per connection **[proposed]**, so a removed CA stops matching at the next swap. (SEC "Mutual TLS"; "Basic and mTLS schemas")
49. Subject form: the leaf subject string is `pkix.RDNSequence.String()` of the parsed `RawSubject` (RFC 4514 order and escaping as Go renders it) **[proposed canonical form]**; URI SANs are the raw IA5String values of `uniformResourceIdentifier` GeneralNames read from the SAN extension with `encoding/asn1`. `subjects` empty admits any verified leaf; a leaf matching no rule → 401 RZ-AUTH-002.
50. CRL: every PEM `X509 CRL` block is parsed with `x509.ParseRevocationList` and MUST verify (`CheckSignatureFrom`) against a CA in the Policy's pool; a certificate in the verified chain (leaf and intermediates) whose serial a CRL from its issuer lists → 401 RZ-AUTH-004. Past `nextUpdate` the last CRL stays enforced and the Node reports a degraded state; the reason is missing from OBS: propose `crl_stale` **[proposed]**. (SEC "Basic and mTLS schemas")
51. `caCertificate` and `crl` rotate by watch like listener certificates; an invalid new value (no certificate parses; a CRL that does not verify) keeps the last value and counts a rotation failure. At activation an invalid value rejects the Revision with RZ-CFG-026 **[proposed code; RZ-CFG-026 is the only secret-resolution code]**.
52. `source.clientCertSubject` MUST stay null unless this Route's own `auth.mtls` verified the leaf; `source.tlsVersion` is `"1.2"` or `"1.3"` on TLS connections, null otherwise **[proposed format]**. (SEC "Mutual TLS"; CFG "Variables")

### 2.7 Authorization class and `authz.cel`

53. `authz.*` are Filter class `authz`, `failureMode` closed only, default slot = the Policy's own name (they stack); within the class Policies run by scope then list position and the first deny stops. Deny → 403; a Policy that cannot decide → 403 RZ-AUTH-015. Codes: `authz.cel` 010, `authz.ip` 013 (M2: `authz.opa` 011, `authz.cedar` 012, `authz.geoip` 014, `plugin` 016). A `when` that is false skips an authz Policy (rule 2 applies only to the auth class). (SEC "Authorization"; ADR11 Figure 1; PACK §10)
54. `authz.cel` `config.rule` is CEL over the base variables (`request`, `source`, `route`, `consumer`, `auth`, `now`) with result type `bool`, compiled once per Revision in all three binaries with the shared environment (standard library plus strings and encoders extensions only; module `cel.dev/cel-go` v0.32.x; the `github.com/google/cel-go` alias is banned). Static cost above 10,000 units at nominal sizes is RZ-CFG-015 (target); syntax/type/variable errors RZ-CFG-014; runtime cost limit 1,000,000 units (target) and `ContextEval` at the request deadline. (ADR11 "Decision outcome"; CFG "Limits")
55. `true` allows; `false` → 403 RZ-AUTH-010; any runtime error (including a null dereference such as `auth.claims.scope` when `auth` is null, the cost limit, or the deadline) or a non-bool result → 403 RZ-AUTH-015. (ADR11 `authz.cel` and Failure rows)
56. Phase: a rule whose checked AST references `request.body` subscribes to `onRequestBody` (making the Policy a gate that buffers the body under CFG "Body buffering and limits"); any other rule runs in `onRequestHeaders` **[proposed rule for the registry's two Phases]**.
57. Budget: 2 µs p50, 5 µs p99, 0 Ruralz-owned allocations (target) for `authz.cel`, `authz.ip` and `cors` each. (PERF "Per-stage latency budget")

### 2.8 `authz.ip` and the client address

58. Config (registered, closed): `allow`, `deny`, sets of CIDRs or bare addresses (bare = /32 or /128); both empty is RZ-CFG-005. IPv4-mapped IPv6 matches as IPv4: the request address is `Unmap()`ed, and a configured IPv4-mapped prefix with at least 96 bits is converted to the IPv4 prefix of (bits − 96); IPv6 prefixes never match IPv4 addresses otherwise. Zones are invalid (RZ-CFG-005). (SEC "IP filtering and GeoIP")
59. Precedence: a `deny` match → 403 RZ-AUTH-013; else a non-empty `allow` refuses every address outside it (403 RZ-AUTH-013); else admit. An unavailable or unparsable `source.ip` → 403 RZ-AUTH-015. Matching uses a compiled per-family binary trie, O(address bits), 0 allocations.
60. Client address (SEC "IP filtering and GeoIP", Client address): `source.ip` is the TCP peer unless the peer is inside `Gateway.spec.trustedProxies`; then it is the rightmost `X-Forwarded-For` or `Forwarded` (`for=`) address outside that list, parsing at most the 16 rightmost entries (target). Details **[proposed]**: `Forwarded` wins when both headers are present; multiple field lines are concatenated in order; entries are walked right to left skipping trusted addresses; the first untrusted parsable address is the client; if every parsed entry is trusted, the leftmost parsed one is used; an unparsable entry (`unknown`, obfuscated `_id`) ends the walk and the address to its right (or the peer) is used; ports in entries are ignored; IPv6 brackets and zones are stripped.
61. PROXY protocol v2: on a listener with `proxyProtocol: true` every connection MUST begin with a valid v2 header (signature `\r\n\r\n\x00\r\nQUIT\n`, version 2, command `PROXY` or `LOCAL`), read within `ReadHeaderTimeout` (10 s, target), else the connection is closed; the header's source address and port replace the TCP peer for rule 60 (`LOCAL` keeps the TCP peer). TLVs are skipped; the variable part (address block plus TLVs) is capped at 4 KiB **[proposed cap]**. (CFG "Gateway": `proxyProtocol` requires PROXY v2)
62. `source.port` is the peer (or PROXY) source port; 0 when `source.ip` came from a forwarding header **[proposed]**.
63. Forwarding headers from untrusted peers MUST be overwritten, never appended: when the effective peer is untrusted, the upstream request's `X-Forwarded-For` is set to the peer address and any `Forwarded` field is removed **[proposed for `Forwarded`]**; when trusted, the peer address is appended to `X-Forwarded-For`. The access-log `client_address` is `source.ip` after this rule. (SEC "IP filtering and GeoIP"; OBS "Access logs")

### 2.9 `cors`

64. Registry: Filter class `cors` (first class), Phases `onRequestHeaders` and `onResponse`, scopes G and R, slot `cors`, `failureMode` default `closed` (either allowed); registered fields `allowOrigins` (set) and `allowMethods` (set), config open. Could-not-decide under `closed` → 503 RZ-RT-011 (request Phase) or 502 RZ-RT-012 (response Phase); under `open` skip, no CORS headers. (PACK §10; CFG "Policy"; DP "Failure semantics")
65. Preflight = `OPTIONS` with `Origin` and `Access-Control-Request-Method`. An allowed preflight MUST short-circuit in `onRequestHeaders` (so no auth-class Policy runs) with 204, `Access-Control-Allow-Origin: <Origin>` (or `*` when `allowOrigins` contains `*` **[proposed]**), `Access-Control-Allow-Methods` = `allowMethods` joined by `, `, `Access-Control-Allow-Headers` = the requested `Access-Control-Request-Headers` token list when every name is an RFC 9110 token (at most 64 names **[proposed]**, reflected because no `allowHeaders` field is registered), and `Vary: Origin, Access-Control-Request-Method, Access-Control-Request-Headers`. A preflight whose origin or method is not allowed → 403 RZ-RT-008. Preflights are never forwarded upstream. (Feature catalog "CORS": "first in the chain; preflights skip authentication"; DP "RZ-RT registry")
66. Origin match: exact byte comparison of the `Origin` value with an `allowOrigins` entry (serialized origin `scheme://host[:port]`); `null` matches only a literal `null` entry. Method match: exact, case-sensitive. Defaults **[proposed]**: absent `allowOrigins` allows no origin; absent `allowMethods` means `GET`, `HEAD`, `POST`.
67. Non-preflight requests are never rejected by `cors` **[proposed]**: in `onResponse`, when `Origin` is allowed, set (overwrite) `Access-Control-Allow-Origin` and append `Vary: Origin`; when not allowed, add nothing (the browser blocks the read). `Access-Control-Allow-Credentials`, `Expose-Headers` and `Max-Age` are never emitted (fields unregistered). The response side MUST also run on generated responses (401, 403, 429, 503) because `onResponse` runs the full chain on them. (DP "Short-circuit")

### 2.10 `auth.upstream-oauth2`

68. Registry: Filter class `upstream-auth`, Phase `onUpstreamRequest` per leg attempt, scope U only, slot `upstream-auth`, closed only. Config: `tokenUrl` (required, `https`, RZ-CFG-037), `clientId` (required), `clientSecret` (required `SecretValue`), `scopes` (set), `timeout` (default 2 s, target). (PACK §10; CFG "Registered from feature documents"; SEC "Upstream authentication")
69. Grant: client credentials (RFC 6749 §4.4): `POST tokenUrl`, `Content-Type: application/x-www-form-urlencoded`, body `grant_type=client_credentials` plus `scope=<scopes joined by SP>` when non-empty, client authentication by HTTP Basic with form-urlencoded `clientId` and secret (`client_secret_basic`, RFC 6749 §2.3.1) **[proposed; no field chooses the method]**, `Accept: application/json`. Redirects are never followed (any 3xx fails); egress guard applies; TLS 1.2+ verified against system roots.
70. Response: 200 with JSON `access_token` (non-empty string, at most 8 KiB **[proposed]**), `token_type` equal to `bearer` case-insensitively, `expires_in` positive integer seconds (absent → 300 s **[proposed]**; capped at 24 h **[proposed]**); body at most 64 KiB **[proposed]**. Anything else is a failed fetch; only the status and the RFC 6749 `error` code are logged, never the body.
71. Cache per Policy per Node, keyed by (Policy name, config digest, current `clientSecret` value digest), carried across Hot Reloads when unchanged. Background refresh at 2/3 of the lifetime (target); the current token keeps being used until expiry minus 5 s **[proposed margin]**. A miss (no usable token) runs exactly one single-flight fetch per Policy and Node; the fetch runs detached from the triggering request with deadline `config.timeout` (2 s); each waiting request also stops at its own deadline. Failure or timeout → 401 RZ-AUTH-020 (status per OQ-security-and-identity-9; see section 7). After a failed miss-fetch, misses within 1 s fail fast without a new fetch **[proposed negative cache]**. (SEC "Upstream authentication")
72. On success set `Authorization: Bearer <access_token>` on the upstream request, overwriting any value. The token is never logged, traced, exposed to CEL, `/tap` or Plugins. The fetch is a declared remote call excluded from gateway-added time (pack 8.7 rule 6; OBS "Gateway-added time").
73. Metrics: `ruralz_auth_upstream_token_age_seconds{policy}`, `ruralz_auth_upstream_refresh_failures_total{policy}` (every failed fetch, background or miss). (OBS "Ruralz Gateway metrics")

### 2.11 Listener TLS

74. `https` listeners: TLS 1.3 by default; `tls.minVersion: "1.2"` allowed. With 1.2, cipher suites MUST be exactly `TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256`, `TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256`, `TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384`, `TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384`, `TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256`, `TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256`. Curve preferences stay Go defaults (hybrid post-quantum X25519MLKEM768 on by default in Go 1.26). 0-RTT off; no renegotiation. ALPN `h2`, `http/1.1`. No field may disable verification anywhere. (SEC "Transport security"; DP "Listeners and protocols")
75. Certificate choice by SNI from `tls.certificates` (map keyed by `name`): `GetCertificate` reads the current resolved `certificate`/`privateKey` values from the secret store and the certificate set from the current snapshot; match the leaf's DNS SANs exactly (lowercased), then one-label wildcard `*.suffix`; no SNI or no match → the first certificate in `name` order **[proposed]**. Accepted keys: ECDSA P-256/P-384, RSA ≥ 2,048 bits, Ed25519. (DP "Listeners and protocols")
76. Rotation: `file` certificates rotate by watch without a Rollout; a new value that does not parse or whose key does not match keeps the last value and increments `ruralz_config_secret_rotation_failures_total{provider}` (reason `secret_rotation_failed`). Only new handshakes see the new certificate. (SEC "Certificate rotation")
77. Handshake timeout is `ReadHeaderTimeout` (10 s, target; `net/http` applies it to the TLS handshake while `ReadTimeout`/`WriteTimeout` are unset). Failures count `ruralz_listener_connections_total{result="tls_failure"}`; successes observe `ruralz_listener_tls_handshake_duration_seconds{listener}` from accept to the end of the handshake. (DP "Bounded resources"; OBS)
78. `http` listeners are cleartext (SHOULD serve only redirects or traffic behind a TLS-terminating load balancer); each counts in `ruralz_security_cleartext_hops{hop="client"}`. (SEC "Transport security")

### 2.12 Client-side TLS (Upstreams, State Store, OTLP, IdP)

79. Upstreams use TLS when `Upstream.spec.tls` is present: TLS 1.2 minimum with the rule 74 suites, server always verified, `RootCAs` = `tls.caCertificate` when set else system roots, `ServerName` = `tls.sni` when set else the Endpoint host (an IP literal verifies IP SANs and sends no SNI), client certificate from `clientCertificate` + `clientKey` (both or neither, else RZ-CFG-005 **[proposed semantic check]**). Rotated values reach new connections: the transport's `DialTLSContext` loads the current `*tls.Config` from an atomic pointer rebuilt on rotation; `InsecureSkipVerify` MUST never be set (gosec G402 finding blocks). Handshake/verification failure is the Upstream layer's 502 RZ-UP-002 (TLS within 2 s, target). Upstreams without `tls` count in `ruralz_security_cleartext_hops{hop="upstream"}`. (SEC "Transport security"; CFG "Upstream"; traffic doc "Failure matrix")
80. State Store: `rediss://` uses TLS 1.2+ verified against system roots (the rueidis wrapper takes its `*tls.Config` from this area); `redis://` counts `hop="state_store"`. A non-loopback URL without credentials MUST be rejected with RZ-CFG-026 **[proposed code; OQ-security-and-identity-30 option (b) until its new codes exist]**; a loopback one without credentials raises degraded reason `state_store_unauthenticated`. (SEC "Transport security"; OBS "Degraded states")
81. OTLP: an `https` `telemetry.otlp.endpoint` uses TLS 1.2+ verified against system roots; `http` counts `hop="telemetry"`. Client certificates wait for OQ-observability-2. (SEC "Transport security" row TB-12)
82. IdP (`jwksUrl`, `tokenUrl`): TLS 1.2+, system roots; private CAs are supplied through the standard `SSL_CERT_FILE`/`SSL_CERT_DIR` process environment (no Bundle field exists). (SEC "Transport security" IdP row)
83. Degraded reason `cleartext_hop` is raised while `ruralz_security_cleartext_hops` is above 0 for any `hop` (`client`, `upstream`, `state_store`, `telemetry`, `admin`), recomputed per snapshot and at admin start. (OBS)

### 2.13 Egress guard and `RURALZ_FETCH_ALLOW` (SSRF)

84. Every Node connection made for the Bundle — `jwksUrl`, `tokenUrl`, Upstream static Endpoints, DNS discovery results, active health probes (M3: `AIProvider` `baseUrl`) — MUST refuse, at connect time (after DNS resolution, in `net.Dialer.Control`) and on each redirect hop, destinations in `0.0.0.0/8`, `::/128`, `127.0.0.0/8`, `::1/128`, `169.254.0.0/16`, `fe80::/10` and `fd00:ec2::254/128`, including their IPv4-mapped forms (`::ffff:0:0/96`) and NAT64 forms (`64:ff9b::/96` and `64:ff9b:1::/48`, low 32 bits checked as IPv4), plus IPv4-compatible `::/96` **[proposed hardening]**, unless the address is inside `RURALZ_FETCH_ALLOW`. State Store and OTLP connections are not covered (not Bundle destinations). (SEC "Secrets" rule 6; OQ-security-and-identity-22)
85. `RURALZ_FETCH_ALLOW` format **[proposed]**: comma-separated entries, each an IP address or CIDR; the reserved entry `env-proxy` opts into `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY` for jwks and token fetches (then the guard checks the proxy's address). Without it, proxy variables are ignored (`Transport.Proxy = nil`). An unparsable entry makes `ruralzd` refuse to start (exit 2, error log naming the setting).
86. The egress HTTP client never follows `https` → `http`, never follows a `tokenUrl` redirect, sends no cookies (no jar), and caps bodies (rules 37, 70). A refused connection is a fetch failure (JWKS: rule 36; token: RZ-AUTH-020; Upstream: RZ-UP-001 connect error).

### 2.14 Secrets on Nodes and redaction

87. Rules 1 to 4 (MUST): only Nodes resolve Bundle secrets; resolved values never appear in a Revision, diff, Last-Known-Good, `/config/dump`, log, trace, metric label or `/tap`; an unresolvable reference is RZ-CFG-026 (Revision rejected, active one kept); after a cold start `/readyz` waits for every reference. (SEC "Secrets"; PACK §8.5)
88. Rule 5 (adopted, OQ-security-and-identity-22 (a)): a `file` reference resolves only when its cleaned absolute path, and its path after `filepath.EvalSymlinks`, lie under `RURALZ_SECRET_ROOT` (default `/etc/ruralz`, itself symlink-resolved); an `env` reference resolves only for `RURALZ_STATE_STORE_URL` and names starting with `RURALZ_SECRET_`. A violation is an unresolvable reference, RZ-CFG-026 (SEC Figure 3). `ruralzd` MUST refuse to start when the secret root contains `${RURALZ_DATA_DIR}`, `RURALZ_ADMIN_TLS_DIR`, `RURALZ_ADMIN_TOKEN_FILE`, `RURALZ_ADMIN_METRICS_TOKEN_FILE` or (M2) the Enrollment token file or Vault credential.
89. Secret values are capped at 1 MiB, CRL values at 16 MiB **[proposed]**; `file` with `key` parses the file as a JSON object and takes the string member; parse errors MUST be replaced by fixed messages that never contain file content. `file` values are watched (polling stat and content hash every 5 s **[proposed]**); `env` needs a restart.
90. Rule 7: adding a `secretRef` or changing a secret's destination has diff impact `security`. Implementation: every `+ruralz:secret` field and `auth.upstream-oauth2` `tokenUrl` get `+ruralz:impact=security`; the diff engine additionally flags changes to `endpoints`, `discovery` or `tls.sni` of an Upstream that sets `tls.clientCertificate` or attaches an `auth.upstream-oauth2` Policy **[proposed]**. (SEC "Secrets" rule 7; LAYOUT "Markers")
91. Every resolved secret MUST be held in a redacting type whose `String`, `GoString`, `Format` (all verbs), `MarshalJSON`, `MarshalText` and `slog.LogValuer` output `[REDACTED]`; raw bytes only through an explicit `Reveal()`. Errors MUST NOT wrap values. `/config/dump` serves the canonical Revision (references only) and never the digests computed from `secretRef`-held keys (T19).
92. Credential headers (shared with WASM `credentials.read`): `authorization`, `proxy-authorization`, `cookie`, response `set-cookie`, and every `auth.api-key` Policy's `config.header` in the active Revision; plus headers set by upstream-auth Filters. `/tap` MUST replace their values with `[REDACTED]`, drop query strings and bodies; access logs and spans never carry header values, query strings or bodies. (WASM "Credentials"; OBS O4, "Access logs")

### 2.15 Admin authentication (9901)

93. Adopt OQ-security-and-identity-7 (a): process settings `RURALZ_ADMIN_METRICS_TOKEN_FILE` (`/metrics` only), `RURALZ_ADMIN_TOKEN_FILE` (operator token, all paths), `RURALZ_ADMIN_TLS_DIR` (admin server TLS; its client CA also admits certificates). Never Bundle fields. Token files are read at start and re-read on change (as built in WP-17): a rotated token applies without a restart; empty, invalid, unreadable or non-regular content keeps the last valid token; a removed file revokes its token (requests get RZ-AUTH-002, logged once at warn) until a valid file reappears; the TLS dir is read at start only; content trimmed of trailing whitespace; an empty or shorter-than-22-byte token refuses start **[proposed]**. TLS dir files `tls.crt`, `tls.key`, optional `ca.crt` **[proposed names]**; missing `tls.crt`/`tls.key` refuses start. (SEC "Admin ports"; CLI "Admin API")
94. The admin port binds all interfaces (OQ-system-overview-6). Authorization matrix: `/healthz`, `/readyz` open; `/metrics` needs the metrics token, the operator token or an admitted client certificate on every interface, loopback included; `/debug/*` (`/debug/pprof/`, `/debug/snapshots`, `/debug/upstreams`), `/config/dump`, `/tap` need the operator token or an admitted client certificate and are off (401) when no operator token is configured. With no admin credential configured only `/healthz` and `/readyz` answer; everything else is 401. (SEC "Admin ports"; CLI "Authentication surfaces" table)
95. Tokens arrive as `Authorization: Bearer <token>`, are compared in constant time (`subtle.ConstantTimeCompare` over SHA-256 digests of presented and configured tokens), and are accepted only over TLS or from a loopback peer (`127.0.0.0/8`, `::1`, mapped forms); a token over cleartext from a non-loopback peer is rejected. Client certificates: `tls.VerifyClientCertIfGiven` against `ca.crt`, leaf MUST carry `clientAuth` EKU. Failures: 401 problem document with RZ-AUTH-001 (no credential) or RZ-AUTH-002 (invalid) **[proposed codes]** and `WWW-Authenticate: Bearer realm="ruralz-admin"`. Admin without TLS on a non-loopback bind counts `hop="admin"`.
96. Each `/tap` and `/config/dump` use MUST be logged at `info` with path, peer address and credential kind (`metrics`, `operator`, `certificate` + subject), never the token. (SEC "Admin ports")

### 2.16 Digest checks

97. Every Revision verification uses the full `sha256:<64 lowercase hex>`; the display form `rev-<12 hex>` is never accepted as an expected digest. Content that does not hash to its digest is RZ-CFG-027 (Revisions), RZ-CFG-028 (Plugin artifacts, wired in M2). Always on; not configurable. (PACK §8.1, §8.14; ADR17 row 1)
98. M1 call sites: Last-Known-Good boot (re-hash the stored canonical content; mismatch → do not boot it, stay not ready, `ruralz_config_activations_total{result="rejected",code="RZ-CFG-027"}`); Zero-Downtime Upgrade handover boot of the predecessor's verified candidate; `ruralz bundle diff` against `/config/dump` re-hashes the dumped canonical content against its digest **[proposed]**. A watched Bundle directory is unsigned and carries no expected digest (skip, pack 8.14). (SYS "Compile before swap"; ZDU "Validation gate" step 2)

## 3. Proposed Go packages and API

Layout follows LAYOUT "Where Go code goes" (Policy `type` → path: dots to slashes, hyphens dropped) and the import boundaries (no `internal/gateway` → `internal/control`; `pkg/` stdlib only). All packages below are Node-side except `identity` check functions and `signing`, which `internal/config` and the CLI also link. Security-sensitive list (LAYOUT "Pull request rules") already covers `internal/filter/auth/`, `internal/filter/authz/`, `internal/signing/`; add `internal/identity`, `internal/egress`, `internal/secret`, `internal/tlsconf`, `internal/adminauth`, `internal/clientaddr` **[proposed]**.

| Package | Contents |
|---|---|
| `internal/identity` | `Consumer`, `Identity`, `Method`, credential `Index`, `KeyIndex` (secretRef key digests), `Revoker` (M2 hook), static check functions for RZ-CFG-035/036 |
| `internal/filter/auth` | Shared helpers: challenge headers, decision recording, rule 2 helper |
| `internal/filter/auth/jwt` | `auth.jwt` Filter, compact parser, claim checks |
| `internal/filter/auth/jwt/jwks` | Node-wide JWKS manager (fetch, clamp, refresh, prefetch, limiter) |
| `internal/filter/auth/apikey` | `auth.api-key` |
| `internal/filter/auth/basic` | `auth.basic`, `Throttle` (success cache, buckets, hash queue) |
| `internal/filter/auth/mtls` | `auth.mtls`, CA/CRL compile, RFC 4514 and SAN extraction |
| `internal/filter/auth/upstreamoauth2` | `auth.upstream-oauth2`, Node-wide `TokenSource` registry |
| `internal/filter/authz/cel` | `authz.cel` |
| `internal/filter/authz/ip` | `authz.ip`, prefix trie |
| `internal/filter/cors` | `cors` |
| `internal/clientaddr` | Trusted-proxy resolution, forwarding-header rewrite, PROXY v2 reader and listener wrapper |
| `internal/egress` | SSRF guard (`RURALZ_FETCH_ALLOW`), guarded dialer and HTTP client |
| `internal/tlsconf` | Listener, upstream, admin and generic client `*tls.Config` builders, cipher list, cert index |
| `internal/secret` | Redacting `Value`, env/file providers with rule 5, watch, rotation fan-out |
| `internal/adminauth` | 9901 (M2: 9902) authentication middleware and settings loader |
| `internal/signing` | Digest parse/verify (M1); Sigstore/OCI verifier interface (M2) |
| `internal/redact` | Credential-header set per snapshot, `/tap` and slog helpers |

```go
// Package identity: request identities and the per-snapshot credential index.
package identity

type Method string

const (
	MethodJWT    Method = "jwt"
	MethodAPIKey Method = "api-key"
	MethodBasic  Method = "basic"
	MethodMTLS   Method = "mtls"
)

// Consumer is the compiled, immutable Consumer shared by CEL, quota and logs.
type Consumer struct {
	Name   string
	Tier   string
	Tags   []string
	Labels map[string]string
	Quotas map[string]Quota // by name; QuotaNames() lists them for CEL
}

// Identity is set once per request by the first successful auth-class Policy.
type Identity struct {
	Method      Method
	Policy      string         // Policy metadata.name
	Claims      map[string]any // verified JWT payload; empty map otherwise; never nil
	Consumer    *Consumer      // nil when unbound
	CertSubject string         // RFC 4514, auth.mtls only
	Principal   string         // non-secret partition key (rule 22)
}

// Index is built by the config loader per snapshot; safe for concurrent reads.
type Index struct{ /* maps below */ }

func BuildIndex(consumers []ConsumerInput) (*Index, error)
func (ix *Index) APIKey(digest [32]byte) (c *Consumer, ambiguous bool)
func (ix *Index) JWT(iss string, claims map[string]any) (c *Consumer, ambiguous bool)
func (ix *Index) Basic(username string) (cred *BasicCredential, ok bool)
func (ix *Index) Certificate(subject string, uriSANs []string) (c *Consumer, ambiguous bool)

// BasicCredential is a decoded credentials.basic entry.
type BasicCredential struct {
	Consumer   *Consumer
	Salt       [16]byte
	Key        [32]byte
	Iterations int
	Digest     [32]byte // credential digest recorded by the success cache
}

// KeyIndex holds digests of secretRef-held API keys; replaced copy-on-write.
type KeyIndex struct{ /* map[[32]byte][]*Consumer */ }

func (ix *Index) WithSecretKeys(k *KeyIndex) *Index // lookup view: literal first, then k

// Revoker is the M2 revocation hook; M1 uses NopRevoker.
type Revoker interface {
	RevokedJWT(iss, kid, jti, sub string, iat time.Time) bool
	RevokedKey(iss, kid string) bool
	RevokedAPIKey(digest [32]byte) bool
	RevokedBasic(username string) bool
	RevokedCert(issuer []byte, serial *big.Int) bool
}

type NopRevoker struct{}

// Static validation used by internal/config (RZ-CFG-035, RZ-CFG-036).
func CheckConsumers(consumers []ConsumerInput) []Diagnostic
```

```go
// Consumed contract of the Filter Chain executor (owned by the data-plane area;
// names here are this spec's assumption and adapt to that area's API).
package filter

type Action uint8 // Continue, Respond, CannotDecide

type Result struct {
	Action Action
	Code   string      // RZ code for Respond/CannotDecide
	Header http.Header // extra response headers (WWW-Authenticate, Retry-After, CORS)
	Status int         // only for non-problem responses such as the 204 preflight
}

type Exchange interface {
	Context() context.Context        // request deadline
	RequestHeader() http.Header      // lowercased access; mutable before forwarding
	UpstreamHeader() http.Header     // onUpstreamRequest only
	Source() clientaddr.Result
	TLS() *ConnTLS                   // nil on cleartext; handshake mode, verified-chain cache
	Identity() *identity.Identity    // nil until set
	SetIdentity(*identity.Identity) error // returns errSecondBinding (rule 3)
	Activation() cel.Activation      // base variables for CEL
	Now() time.Time
}
```

```go
// Package jwks: Node-wide JWKS cache. One owner goroutine plus a pool of 16 fetchers.
package jwks

type Source struct{ Issuer, URL string }

type Key struct {
	KID string
	Alg jwa.SignatureAlgorithm // RS256, PS256, ES256 or EdDSA compatibility
	Pub any                    // *rsa.PublicKey, *ecdsa.PublicKey or ed25519.PublicKey
}

type Set struct {
	ByKID     map[string][]Key
	FetchedAt time.Time
	RefreshAt time.Time
	Expires   time.Time
}

type Options struct {
	Concurrency   int           // 16
	PrefetchLimit time.Duration // 5 s
	FetchTimeout  time.Duration // 5 s [proposed]
	MinTTL, MaxTTL, DefaultTTL time.Duration // 5m, 6h, 1h
	KidRefreshGap time.Duration // 30 s
	MaxBody       int64         // 1 MiB [proposed]
	Revoker       identity.Revoker
}

func NewManager(client *http.Client, clock Clock, opts Options) *Manager
func (m *Manager) Run(ctx context.Context) error                 // owner loop; returns on ctx cancel
func (m *Manager) Prefetch(ctx context.Context, srcs []Source)    // blocks ≤ PrefetchLimit
func (m *Manager) Keys(src Source) (*Set, Health)                 // lock-free
func (m *Manager) RequestRefresh(src Source)                      // non-blocking, rate-limited
func (m *Manager) Retain(live []Source)                           // after each swap
```

```go
package jwt // internal/filter/auth/jwt

type Filter struct{ /* compiled issuers, audiences, *jwks.Manager, *identity.Index */ }

func Compile(name string, cfg v1alpha1.AuthJWTConfig, m *jwks.Manager, ix *identity.Index) (*Filter, error)
func (f *Filter) OnRequestHeaders(x filter.Exchange) filter.Result
func (f *Filter) Sources() []jwks.Source // for prefetch and Retain

// verify is exported to tests only through a test file; order per rules 24 to 31.
```

```go
package basic // internal/filter/auth/basic

type Throttle struct{ /* sharded LRUs, semaphores, per-process key */ }

type ThrottleOptions struct {
	Slots          int           // max(1, GOMAXPROCS/4)
	WaitersPerSlot int           // 4
	QueueDeadline  time.Duration // 1 s
	CacheEntries   int           // 10,000 in 64 shards
	CacheIdle      time.Duration // 60 s
	CacheMaxAge    time.Duration // 15 min, jitter up to 20%
	Buckets        int           // 100,000 in 64 shards
	Burst          int           // 10
	Refill         time.Duration // 1 s
	CreatePerSec   int           // 100
	DelayPerBucket int           // 2
	DelayPerNode   int           // 1,024
	DelayMax       time.Duration // 2 s
	SourceFailures int           // 10 per minute
}

func NewThrottle(clock Clock, rand io.Reader, o ThrottleOptions) (*Throttle, error)
func Compile(name string, ix *identity.Index, t *Throttle) (*Filter, error)
func (f *Filter) OnRequestHeaders(x filter.Exchange) filter.Result
```

```go
package upstreamoauth2 // internal/filter/auth/upstreamoauth2

type Registry struct{ /* map[key]*tokenSource carried across snapshots */ }
func NewRegistry(client *http.Client, clock Clock) *Registry
func (r *Registry) Run(ctx context.Context) error // background refresher, bounded workers (8 [proposed])
func Compile(name string, cfg v1alpha1.AuthUpstreamOAuth2Config, secrets secret.Store, r *Registry) (*Filter, error)
func (f *Filter) OnUpstreamRequest(x filter.Exchange) filter.Result
```

```go
package mtls // internal/filter/auth/mtls
func Compile(name string, cfg v1alpha1.AuthMTLSConfig, secrets secret.Store, ix *identity.Index) (*Filter, error)
func (f *Filter) OnRequestHeaders(x filter.Exchange) filter.Result
func SubjectString(leaf *x509.Certificate) (string, error) // RFC 4514 via RawSubject
func URISANs(leaf *x509.Certificate) ([]string, error)     // raw SAN IA5Strings

package ip // internal/filter/authz/ip
type Set struct{ /* v4, v6 tries */ }
func CompileSet(entries []string) (*Set, error) // RZ-CFG-005 on bad entry
func (s *Set) Contains(a netip.Addr) bool
func Compile(name string, cfg v1alpha1.AuthzIPConfig) (*Filter, error)

package cel // internal/filter/authz/cel (import cel-go as celgo)
func Compile(name string, rule string, env *celenv.Env) (*Filter, error) // RZ-CFG-014/015
func (f *Filter) Phase() filter.Phase // onRequestHeaders or onRequestBody (rule 56)

package cors // internal/filter/cors
func Compile(name string, cfg v1alpha1.CORSConfig) (*Filter, error)
func (f *Filter) OnRequestHeaders(x filter.Exchange) filter.Result
func (f *Filter) OnResponse(x filter.Exchange, h http.Header)
```

```go
package clientaddr

type Source uint8 // Peer, ProxyV2, XForwardedFor, Forwarded

type Result struct {
	IP   netip.Addr
	Port uint16
	Via  Source
}

type Trusted struct{ /* *ip.Set-like trie; empty means trust none */ }

func Compile(cidrs []string) (*Trusted, error)
func (t *Trusted) Resolve(peer netip.AddrPort, h http.Header) Result // ≤16 entries parsed
func (t *Trusted) RewriteForwarding(h http.Header, peer netip.Addr)  // overwrite vs append

// PROXY protocol v2
type ProxyHeader struct{ Command byte; Source, Dest netip.AddrPort }
func ReadProxyV2(r *bufio.Reader, max int) (ProxyHeader, error)
func NewProxyListener(l net.Listener, timeout time.Duration) net.Listener // conns expose ProxyHeader
```

```go
package egress

var ErrDenied = errors.New("egress: destination address denied") // sentinel

type Guard struct{ /* allow prefixes, envProxy */ }

func ParseAllow(v string) (*Guard, error)                       // RURALZ_FETCH_ALLOW
func (g *Guard) Allowed(a netip.Addr) bool
func (g *Guard) Control(network, address string, _ syscall.RawConn) error // net.Dialer.Control
func (g *Guard) Dialer(timeout time.Duration) *net.Dialer

type ClientOptions struct {
	TLS           *tls.Config
	MaxRedirects  int  // 3 for JWKS; 0 means never follow (tokenUrl)
	Timeout       time.Duration
}

func (g *Guard) Client(o ClientOptions) *http.Client // no jar, https-only redirects
```

```go
package tlsconf

func TLS12Suites() []uint16 // the six suites of rule 74

type ListenerState struct { // per listener, read from the current snapshot
	MinVersion        uint16
	Certs             *CertIndex
	RequestClientCert bool
}

// Server returns a static *tls.Config whose GetConfigForClient reads state()
// and records "certificate requested" on the connection wrapper.
func Server(state func() *ListenerState, secrets secret.Store, onHandshake func(time.Duration, error)) *tls.Config

type CertIndex struct{ /* exact and wildcard maps over leaf DNS SANs, default */ }
func BuildCertIndex(certs []v1alpha1.Certificate, secrets secret.Store) (*CertIndex, error)

// Upstream returns an atomically swapped client config (rule 79).
func Upstream(t *v1alpha1.UpstreamTLS, host string, secrets secret.Store) (*atomic.Pointer[tls.Config], error)
func Client(roots *x509.CertPool) *tls.Config // IdP, OTLP, State Store; min 1.2
func Admin(dir string) (*tls.Config, error)   // tls.crt, tls.key, optional ca.crt
```

```go
package secret

type Value struct{ b []byte }

func (v Value) Reveal() []byte
func (Value) String() string                 // "[REDACTED]"
func (Value) GoString() string               // "[REDACTED]"
func (Value) Format(f fmt.State, _ rune)     // writes "[REDACTED]" for every verb
func (Value) MarshalJSON() ([]byte, error)   // "\"[REDACTED]\""
func (Value) MarshalText() ([]byte, error)   // "[REDACTED]"
func (Value) LogValue() slog.Value           // "[REDACTED]"

type Store interface {
	Get(ref v1alpha1.SecretRef) (Value, bool)                         // current value, lock-free
	Watch(ref v1alpha1.SecretRef, fn func(Value) error) (stop func()) // fn error = rotation failure
}

type Settings struct {
	Root     string // RURALZ_SECRET_ROOT, default /etc/ruralz
	DataDir  string
	Guarded  []string // admin token files, admin TLS dir (and M2 enrollment/vault paths)
}

func CheckSettings(s Settings) error // refuse start when the root contains a guarded path
func NewResolver(s Settings, env func(string) (string, bool), clock Clock) (*Resolver, error)
func (r *Resolver) Resolve(ctx context.Context, refs []v1alpha1.SecretRef) (Store, error) // RZ-CFG-026
func (r *Resolver) Run(ctx context.Context) error // file polling and rotation fan-out
```

```go
package adminauth

type Class uint8 // Open, Metrics, Operator

type Settings struct{ MetricsTokenFile, TokenFile, TLSDir string }

func LoadSettings(env func(string) string) (Settings, error)
func New(s Settings) (*Authenticator, *tls.Config, error) // tls.Config nil without TLS dir
func ClassOf(path string) Class                          // /healthz,/readyz Open; /metrics Metrics; else Operator
func (a *Authenticator) Middleware(next http.Handler) http.Handler
```

```go
package signing

type Digest [32]byte

func ParseDigest(s string) (Digest, error)       // exactly "sha256:" + 64 lowercase hex
func Sum(b []byte) Digest
func (d Digest) String() string                  // sha256:<hex>
func (d Digest) Display() string                 // rev-<12 hex>
func VerifyRevision(content []byte, want Digest) error                 // RZ-CFG-027
func VerifyArtifact(r io.Reader, want Digest, limit int64) error       // RZ-CFG-028 (M2 wiring)

// Verifier is the M2 extension point (Sigstore, Control-mode signatures).
type Verifier interface {
	Verify(ctx context.Context, content []byte, want Digest) error
}
type DigestOnly struct{} // M1
```

Concurrency model:

- Compiled Filters are immutable per snapshot and safe for concurrent use; the request goroutine runs them synchronously, with no lock held across I/O.
- Node-scoped mutable state is owned by long-lived objects created at start and carried across snapshots by identity: `jwks.Manager` (one owner goroutine with a timer heap, 16 fetch workers, a bounded refresh-request channel of 1,024 with non-blocking sends and per-issuer coalescing), `basic.Throttle` (64-shard mutex-protected LRUs, a semaphore of `slots` plus a bounded waiter count, per-bucket delay counters), `upstreamoauth2.Registry` (one refresher goroutine with a timer heap and at most 8 fetch workers **[proposed]**; per-entry single-flight with a done channel), `secret.Resolver` (one polling goroutine). Each has an owner that cancels it with the process context and waits for it (`Run(ctx)` returns after workers exit).
- Values read on the request path (JWKS sets, tokens, secret values, key indexes, listener TLS state) are published through `atomic.Pointer` and replaced copy-on-write.
- No `init()`, no mutable package globals; clocks and randomness are injected; context is the first parameter of every blocking function.

Exports for other areas: `identity.Identity`/`Consumer`/`Principal` (CEL activation, quota, rate-limit keys, Response Cache partition, access log `consumer`/`tier`/`auth_method`), `clientaddr.Result` (Router `source`, rate-limit keys, access log), `egress.Guard` (Upstream layer dialer, health checks, DNS discovery), `tlsconf` builders (listener server, Upstream transport, rueidis, OTLP exporter, admin), `secret.Store`/`Value` (every `SecretValue` consumer), `redact` (tap, logging), `adminauth` (admin server), `signing` (loader, LKG, CLI diff).

## 4. Dependencies on other areas

Needs:

| From | What |
|---|---|
| Configuration (loader, validation, canonical) | Decoded `pkg/config/v1alpha1` types; the compiled snapshot with each Route's effective chain per Phase and per leg; a per-type compile registry to plug these Filters in; the Consumer list; the listener ↔ Route binding (to compute `RequestClientCert` per listener); config digest per Policy (carry-over keys); diagnostics type with RZ-CFG codes and source positions; calling this area's static checks (rule 20); schema changes: `+ruralz:impact=security` markers (rule 90), `auth.api-key` `header` default (rule 15); canonical digest function (signing verifies it) |
| Filter Chain executor (data plane) | `Exchange`/`Result` contract; class and scope ordering; `when` evaluation with the closed-runs-on-error rule; rule 1 accounting of auth-class Policies; rule 2 via `SetIdentity`; short-circuit then full `onResponse` on generated responses (CORS on 401); per-attempt `onUpstreamRequest`; problem-document writer with `requestId`; `ruralz_filter_*` metrics and `ruralz.filter.<name>` spans |
| Listener / server (data plane) | Counting listener conn wrapper reachable from `ClientHelloInfo.Conn` and `ConnContext`; PROXY v2 wrapper placement before TLS; `GetConfigForClient` hook; GOAWAY/`Connection: close` on mTLS mode change; header limits before auth; `source` computed before routing |
| Upstream layer (traffic area) | Using `egress.Guard.Control` on every Endpoint dial and probe; `tlsconf.Upstream` atomic config in `DialTLSContext`; pool keys from TLS settings references; RZ-UP-001/002 mapping |
| CEL area | Shared `cel.Env` for base variables with nominal-size estimator; compile with RZ-CFG-014/015; program evaluation with runtime cost limit 1,000,000 and `ContextEval`; activation built from `identity.Identity` (`auth.claims` as `dyn`) and `clientaddr.Result`; AST query "references `request.body`" |
| Telemetry | Meter provider and instruments (`ruralz_auth_*`, `ruralz_security_cleartext_hops`, `ruralz_listener_*`, rotation failures), degraded-state reporter (`ruralz_node_degraded_info{reason}`), `slog` via `internal/telemetry` with a `ReplaceAttr` hook for `redact` |
| Lifecycle / loader | Call sites: `jwks.Prefetch` during compile, `Retain` after swap, `signing.VerifyRevision` on LKG and handover boots, secret resolution in the validation gate (step 8), readiness blocked on secrets |
| Admin server (data plane) | Mounting `adminauth.Middleware`; `/tap` using `redact`; `/config/dump` from the canonical Revision |
| State Store area | Consuming `tlsconf.Client` for `rediss://`; the uncredentialed-URL rule (rule 80) |
| errcode | Registry entries; a shared coded-error type (for example `errcode.Wrap(code, err)`); new entries RZ-AUTH-008 (proposed) and the static duplicate-binding RZ-CFG code (proposed) |
| CLI | `ruralz dev run` sets `RURALZ_ADMIN_TOKEN_FILE` and (proposed) `RURALZ_FETCH_ALLOW=127.0.0.0/8,::1` for loopback mocks; `--admin-token-file`, `--client-cert`, `--client-key` on `ruralz node dump`, `ruralz dev tap`, `ruralz bundle diff` |

Provides: see "Exports for other areas" in section 3. The State Store is never used by this area (auth Filters never call it; pack 8.7 does not apply).

## 5. Libraries

| Module | Version | Use | Notes |
|---|---|---|---|
| `github.com/lestrrat-go/jwx/v4` | v4.5.0 (floor, GHSA-4cf7-xm37-g63h; depgate advisory floor already set) | `jwk.Parse`, `jwk.PublicKeyOf`, `jws.VerifyCompactFast`, `jws.Verify` + `jws.WithKey`, `jwa.RS256()/PS256()/ES256()/EdDSA()` | MIT. Import only `jwk`, `jws`, `jwa` (rule 33). Pulls `github.com/lestrrat-go/dsig` v1.4.0 (MIT), `github.com/lestrrat-go/option/v3` v3.0.0-alpha1 (MIT, pre-release), `github.com/valyala/fastjson` v1.6.10 (MIT); needs `encoding/json/v2` (Go 1.27, or `GOEXPERIMENT=jsonv2` on the 1.26 floor, already guarded). `dsig`'s ML-DSA file is `//go:build go1.27` |
| `cel.dev/cel-go` | v0.32.0 (v0.32.x) | `authz.cel` through the CEL area's environment | Apache-2.0; alias banned |
| `go.opentelemetry.io/otel` (+ `metric`) | v1.46.0 | Instruments via `internal/telemetry` | Exporter is `go.opentelemetry.io/otel/exporters/prometheus` v0.68.0 (telemetry area) |
| Standard library | Go 1.26 floor, 1.27.1 toolchain | `crypto/tls`, `crypto/x509` (`ParseRevocationList`, `Verify`), `crypto/pbkdf2` (Go 1.24+), `crypto/sha256`, `crypto/hmac`, `crypto/subtle`, `crypto/rand`, `encoding/asn1`, `encoding/base64` (`RawURLEncoding`), `encoding/json/v2`, `net/netip`, `net/http`, `log/slog`, `sync/atomic` | All G3-compliant (Go Cryptographic Module) |

Not used: `golang.org/x/crypto` (keep it unlinked; `crypto/pbkdf2` replaces `x/crypto/pbkdf2`), jwx `jwt`/`jwe`, go-oidc/go-jose (Ruralz Control only, M5), `golang.org/x/sys` (not needed: `net.Dialer.Control` suffices), rueidis (the State Store wrapper consumes `tlsconf`). Recommended depguard additions **[proposed]**: deny `github.com/lestrrat-go/jwx/v4/jwt` and `/jwe` everywhere; deny `crypto/tls.Config.InsecureSkipVerify` via gosec (already G402); confine `jwx` to `internal/filter/auth/jwt/...`.

## 6. Test plan

Conventions: unit and table tests beside code, `testing.F` for fuzz and properties (seeds in `pr-fast`, long runs nightly), `integration` build tag for stage 8, `-race` in stage 5, injected fake clocks, keys and certificates generated at test time (no private keys committed). No Docker in this environment: integration tests use `httptest.NewTLSServer`, in-process `ruralzd` wiring and the local `redis-server` 7.0.15 binary (built with TLS support: `--tls-port`, `--tls-cert-file`, `--tls-key-file`, `--tls-ca-cert-file`, `--requirepass`).

### 6.1 Unit and table tests (every code path)

- `auth.jwt` table (fake clock, generated RSA-2048, RSA-1024, P-256, Ed25519 keys): no `Authorization` → 001; `Basic xyz` → 001; `Bearer` + empty → 002; two `Authorization` lines → 002; 16 KiB + 1 token → 002; 2 and 5 segments → 002; padded base64 → 002; header not an object / duplicate `alg` → 002; `alg` `HS256`, `none`, `RS384`, `ES384`, `Ed25519` → 002; `crit` present → 002; unknown `iss` → 003; non-string `iss` → 003; issuer never fetched → 006 and one fetch requested; set expired → 006; unknown `kid` → 005 and exactly one refresh within 30 s across 1,000 requests; after refresh the same token passes; no `kid` with one compatible key → pass, with two → 002; wrong signature → 002; token signed by an attacker key embedded in `jwk`/`x5c`/`jku` header → 005 or 002, never pass; `exp` missing → 003; `exp` string → 002; `exp` = now − 61 s → 003; now − 59 s → pass; `exp` = now + 24 h + 1 s → 003; `nbf` = now + 61 s → 003; `aud` missing → 003; `aud` array with one match → pass; `aud` number → 002; duplicate `sub` in payload → 002; PS256, ES256, EdDSA happy paths; consumer by `subject`, by `claims` (string match; numeric claim never matches), by `client_id`, by `azp` when `client_id` absent, `oauthClients` without a matching `jwt` issuer → no binding; two Consumers → 002; no Consumer → identity with nil consumer; `Authorization` still forwarded.
- JWKS manager: clamp table (`max-age` absent → 1 h; `no-cache` → 1 h; `no-store` → 1 h; `max-age=60` → 5 min; `=1800` → 30 min; `=86400` → 6 h; malformed → 1 h); refresh at half-life ± 10%; failed refresh → stale serving, `jwks_stale` raised, age metric grows, backoff 1 s…60 s never past expiry; expiry → 006; recovery clears `jwks_stale`; 429 with `Retry-After: 120` delays the next attempt 120 s; non-200, oversized body (1 MiB + 1), invalid JSON, zero usable keys, 257 keys → failed fetch keeping the old set; key filters (`use: enc`, `key_ops: [sign]`, `alg: RS512`, RSA 1024, P-384 for ES256, X25519, `oct`) → unusable; private JWK → public part used; duplicate `kid` → both tried; prefetch with 40 issuers never exceeds 16 in flight and returns by 5 s with slow servers; unchanged issuers are not refetched on the next Revision; `Retain` drops unreferenced entries; revocation hook refuses a key (M2 hook test with a fake `Revoker`).
- `auth.api-key`: absent/empty → 001; unknown → 002; two field lines → 002; literal hash match; `secretRef` key match after trim; key of 21 bytes → activation RZ-CFG-026; rotation to a short key keeps the old digest and counts a rotation failure; hash declared by two Consumers → 002; header stripped from the upstream request (all values); default header `x-api-key` when unset.
- `auth.basic` + throttle (fake clock, `slots` = 1): no header → 001; `Bearer` → 001; bad base64, no colon, 257-byte username → 002; right password → pass and cache insert; wrong password → 002 after a hash; undeclared user → 002 after a dummy hash with the same iteration count (assert the hash function was called with 600,000); cache hit skips the hash (counted); cache entry expiry at 60 s idle and at 15 min × (1 − j); credential change in a new snapshot evicts on next hit, unchanged credential survives a swap; bucket burst 10 then refill 1/s; delay: third waiter → 429 `delay` with `Retry-After` ≥ 1; 1,025th Node-wide delayed waiter → 429; hash queue full (5th waiter with 1 slot) → 429 `hash_queue`; 1 s queue timeout → 429; 101st bucket creation in 1 s → 429 `bucket_creation`; source bucket: 10 failures in a minute → 11th non-cached attempt 429 `source`, IPv6 /64 aggregation; `Authorization` stripped upstream; PBKDF2 vector test against RFC 6070-style known answers for SHA-256 and a fixture hash from the CFG example.
- `auth.mtls` (generated root, intermediate, leaves): valid leaf → pass; leaf without EKU → 002; EKU `serverAuth` only → 002; expired → 002; unknown CA → 002; intermediate chain from the client → pass; `subjects` exact subject match / mismatch (002); `uriSan` SPIFFE match; CRL listing leaf serial → 004, listing intermediate → 004; CRL signed by another CA → activation RZ-CFG-026; CRL past `nextUpdate` still enforced and `crl_stale` raised; connection without a certificate request → 421 RZ-AUTH-008; requested but none sent → 001; per-connection cache hit (verify called once for 10 requests) and miss after a swap; consumer binding by subject, by `uriSan`, conflict → 002; `source.clientCertSubject` null on a Route without `auth.mtls`.
- `authz.cel`: `true` → allow; `false` → 403 010; `auth.claims.scope` with anonymous → 403 015; runtime cost limit (list comprehension over a 100k-element header split) → 015; deadline exceeded → 015; rule referencing `request.body` reports `onRequestBody`; the CFG example `authz-orders` rule compiles under 10,000 units.
- `authz.ip`: table over allow/deny precedence, bare addresses, `::ffff:10.0.0.1` vs `10.0.0.0/8`, `::ffff:10.0.0.0/104` config, IPv6 `/0` not matching IPv4, invalid entry → RZ-CFG-005, zone → RZ-CFG-005.
- `clientaddr`: untrusted peer ignores XFF; trusted peer with `XFF: 1.1.1.1, 10.0.0.2` (10/8 trusted) → 1.1.1.1; all trusted → leftmost; 20 entries → only 16 parsed; `unknown` entry → right neighbour; `Forwarded: for="[2001:db8::1]:4711"` → 2001:db8::1; both headers → `Forwarded`; rewrite: untrusted peer overwrites XFF and removes `Forwarded`, trusted appends; PROXY v2: TCP4, TCP6, LOCAL, bad signature, v1 text, truncated, oversized TLVs, timeout.
- `cors`: allowed preflight → 204 with exact headers and `Vary`; disallowed origin → 403 RZ-RT-008; disallowed method → 403; invalid `Access-Control-Request-Headers` token → no reflection; `*` entry → `*`; actual request allowed → ACAO + `Vary: Origin`; not allowed → no CORS headers, not rejected; generated 401 from a later auth Policy still gets ACAO (executor integration test).
- `auth.upstream-oauth2`: body and Basic auth encoding (clientId with `:` and `%`); scopes join; token injected; 100 concurrent misses → exactly 1 fetch; refresh at 2/3 with fake clock; token used until expiry − 5 s; token endpoint 500 → 020; 2 s timeout → 020 while a waiter with a 50 ms deadline returns 020 at 50 ms; 302 → 020 (not followed); `token_type: mac` → 020; missing `expires_in` → 300 s; negative cache 1 s; `http://` tokenUrl → RZ-CFG-037 at validation; refresh-failure metric increments; secret rotation → new cache key.
- `egress`: deny table for every listed range and its mapped, NAT64 (`64:ff9b::7f00:1`, `64:ff9b:1::a9fe:a9fe`) and IPv4-compatible forms; RFC 1918 allowed; `RURALZ_FETCH_ALLOW=127.0.0.1/32` admits loopback; `env-proxy` toggles proxy use; bad entry → error; DNS-rebinding test with a custom resolver returning 127.0.0.1 → refused at connect; redirect `https`→`http` refused; redirect to a denied address refused; `tokenUrl` redirect not followed.
- `tlsconf`: default min 1.3; `"1.2"` → exactly six suites; SNI exact, wildcard one label only, no SNI → first by name; rotation to a valid pair used by the next handshake; invalid pair keeps the old one and counts; `RequestClientCert` toggled by listener state; Upstream verification against `caCertificate`, SNI default from host, IP literal SAN, missing client key → RZ-CFG-005; no `InsecureSkipVerify` in any returned config (reflection check).
- `secret`: `Value` with `%v %+v %#v %s %q %x %d`, `fmt.Sprint`, `json.Marshal` of a struct containing it, `slog` JSON and text handlers, `errors` wrapping → only `[REDACTED]`; rule 5: path outside root, `..` traversal, symlink escaping the root, Kubernetes-style `..data` symlinks inside the root (allowed), env `HOME` refused, `RURALZ_SECRET_DB` allowed; root containing the data dir or admin token file → start refused; JSON `key` parse error message contains no file bytes; file size cap.
- `adminauth`: full matrix (paths × {no credential configured, metrics token only, operator token only, both, TLS client CA only} × {no header, metrics token, operator token, wrong token, client cert} × {loopback cleartext, non-loopback cleartext, TLS}); constant-time comparison used (code review + test of equal-length and different-length tokens); `/tap` and `/config/dump` access logged without the token; start refused for empty or 21-byte token, missing `tls.crt`.
- `signing`: `ParseDigest` rejects `rev-…`, uppercase hex, 63/65 hex, missing prefix, `sha512:`; `VerifyRevision` match and single-bit flip → RZ-CFG-027; `VerifyArtifact` limit exceeded → RZ-CFG-028; LKG boot with a tampered file stays not ready.

### 6.2 Golden tests

- Problem documents for every RZ-AUTH code (001 to 007, 010, 013, 015, 020, proposed 008) and RZ-RT-008 with their `WWW-Authenticate`/`Retry-After` headers (`testdata/problems/*.json`).
- `/config/dump` of a Bundle using every `SecretValue` field and a `secretRef` API key: references only, no derived digest (`testdata/dump/*.json`).
- `ruralz bundle diff` human and JSON output for a `secretRef` change and a `tokenUrl` change carrying `security` impact.
- JWKS fixtures from real IdP shapes (Okta, Auth0, Keycloak, Entra ID style documents with `x5c`, `x5t`, mixed `use`) parsed to the same usable-key set in the 1.26 floor job (`GOEXPERIMENT=jsonv2`) and the 1.27 job (TECH "Version floor": JOSE JSON golden tests identical in both jobs).

### 6.3 Property tests

- `authz.ip`: for random prefixes and addresses, `Set.Contains` equals a naive linear `netip.Prefix.Contains` over unmapped forms.
- `clientaddr.Resolve`: the result is never a trusted address when an untrusted parsable entry exists within the 16 rightmost; appending trusted hops never changes the result.
- Throttle: success-cache behaviour equals a reference model (map + fake clock) under random operation sequences; the number of concurrent PBKDF2 calls never exceeds `slots`.
- Consumer index: for random Consumer sets, `JWT()`'s match set equals a brute-force evaluation of rules 16 to 18.

### 6.4 Fuzz targets (12, toward TEST "25 or more by M2")

`FuzzJWTCompact` (header/payload parsing and ordering of checks; never panics, only 001 to 006), `FuzzJWKSParse` (wrapper over `jwk.Parse` + key filters), `FuzzBasicHeader`, `FuzzForwardedFor`, `FuzzForwarded`, `FuzzProxyV2`, `FuzzCIDRSet`, `FuzzCORSRequestHeaders`, `FuzzTokenResponse`, `FuzzSubjectAndSAN` (DER certificates → RFC 4514 string and URI SANs), `FuzzPEMBundleAndCRL`, `FuzzFetchAllow` + `FuzzParseDigest`. Oracle for all: no panic, bounded memory, a registered code or a typed error.

### 6.5 Integration tests (`integration` tag, stage 8)

- In-process `ruralzd` with an `https` listener, an `httptest` TLS JWKS server and an Upstream echo server (egress allow for loopback set in the test Guard, not the process env): end-to-end 200 with a valid token; key rotation at the IdP (new `kid`) → first request 005, next request after refresh 200; IdP down → stale serving then 006 after expiry (fake clock); prefetch on activation of a new issuer; Hot Reload keeps keys of unchanged issuers (no fetch).
- mTLS: Go client with and without certificates against a listener that has one `auth.mtls` Route; Hot Reload adding the first `auth.mtls` Route switches request mode, existing HTTP/2 connection gets GOAWAY and a coalesced request gets 421.
- Upstream TLS and mTLS against an `httptest` server requiring client certificates; wrong CA → RZ-UP-002.
- `auth.upstream-oauth2` against an `httptest` token server with 200 concurrent client requests → one token fetch; forced expiry → one refetch.
- State Store TLS: `redis-server --tls-port` with a generated CA (`SSL_CERT_FILE` pointing to it) and `--requirepass`; `rediss://:pass@127.0.0.1:port` works; `redis://127.0.0.1` without password raises `state_store_unauthenticated`; a non-loopback URL without credentials (bind redis to the container's non-loopback IP) is rejected RZ-CFG-026.
- Admin 9901 matrix over real sockets (loopback and the non-loopback interface).
- Throttle under load: 64 goroutines × 100 wrong passwords on one username → 429s, hash calls ≤ slots at any time, legitimate cached user keeps passing.

### 6.6 Redaction and secret leak tests

- Redaction unit tests (`pr-fast`, TEST "Security scanning"): a canary `secretRef` value in `/config/dump`, diff output and every log encoder path (slog JSON/text, OTLP log bridge) never appears.
- Secret leak test (`main` and `release`; TEST "End-to-end tests"): run the in-process e2e harness (Docker Compose is unavailable here) with a canary injected through `env` (`RURALZ_SECRET_CANARY`) and `file` providers into `clientSecret`, a `secretRef` API key, `stateStore.url` password and a listener key; drive traffic including failures; assert the canary (raw, base64, hex, URL-encoded forms) is absent from stdout/stderr logs, access logs, `/config/dump`, `/tap` output, `/metrics`, exported spans (in-memory exporter), diffs and error bodies; `/debug/pprof` heap profiles excluded.

### 6.7 Benchmarks (PERF component benchmarks)

`BenchmarkJWT/{RS256-2048,PS256,ES256,EdDSA}` over the full Filter path with a warm JWKS (gate RS256 at 45 µs p50 / 100 µs p99 on RH-1; allocations reported); `BenchmarkAuthzCEL`, `BenchmarkAuthzIP` (1,000 prefixes), `BenchmarkCORS` with `testing.AllocsPerRun` = 0; `BenchmarkBasicCacheHit` (no hash); `BenchmarkAPIKey`; `BenchmarkClientAddr` with 16 XFF entries.

## 7. Open questions blocking M1 in this area

| ID | Question | Adopt | What the code does |
|---|---|---|---|
| OQ-security-and-identity-1 (Yes, M1) | Which extra `auth.jwt` and `auth.api-key` fields? | (b) Fixed defaults for M1 (no option is marked proposed; (a)'s fields are unauthored, and adding them later is additive because the configs are `+ruralz:open`) | Algorithms fixed to RS256/PS256/ES256/EdDSA; clock skew 60 s **[proposed target]**; `exp` required, lifetime ≤ 24 h; no configurable required claims; double Consumer match → 401 RZ-AUTH-002 at runtime; `issuerConfig` struct keeps per-issuer fields so (a) can populate them in M2 |
| OQ-security-and-identity-7 (Yes, M1) | Which settings hold admin credentials? | (a) three `RURALZ_ADMIN_*` settings (proposed), amending pack section 2 | `internal/adminauth` reads the three settings; section 2.15 rules |
| OQ-security-and-identity-9 + OQ-data-plane-8 (Yes; pack 8.10 amendment, RM R-5 "before M1") | Status for failed upstream auth? | (a) 401 for M1: pack 8.10 is binding and the registry encodes RZ-AUTH-020 as 401 (Data plane marks (a) current; Security's proposed (b) 503 needs a pack amendment under pack section 14) | RZ-AUTH-020 status comes only from `errcode.Lookup`, never a literal, so an adopted amendment is a one-line registry change plus golden updates |
| OQ-security-and-identity-15 (Yes, `auth.basic`; RM exit 7) | Are throttle defaults and residuals acceptable? | (a) Local throttle (current) | `basic.Throttle` with the section 2.5 constants |
| OQ-security-and-identity-21 (Yes, M1; owner data-plane) | Reject encoded NUL and backslashes? | (a) Yes (proposed) | Data plane normalizer rejects `%00`, raw `\` and `%5C` in the path before routing (needs an RZ-RT code from data-plane); this area relies on the single normalized path for `authz.*` and CEL (T4) |
| OQ-security-and-identity-22 (Yes, M1) | How are secrets and Node connections restricted? | (a) (proposed), amending pack sections 2 and 5 | Rules 84 to 90: `RURALZ_SECRET_ROOT` (default `/etc/ruralz`), `RURALZ_SECRET_` env prefix, `RURALZ_FETCH_ALLOW` guard, `security` impact class. The "secret-to-destination binding" and "State Store entry MAC key" parts have no specification yet: not built in M1 (section 9) |
| OQ-security-and-identity-24 (Yes, M1) | How does `oauthClients` name its issuer? | (a) The Consumer's `jwt` issuer (current) | Rule 17 |
| OQ-security-and-identity-6 (Yes, `source.ip`; RM exit 7) | How are trusted proxies declared? | (c) Both, already answered by CFG "Gateway" (`trustedProxies`, `listeners[].proxyProtocol` exist in `pkg/config/v1alpha1`) | `internal/clientaddr` rules 60 to 63; the basic throttle's source key is on |
| OQ-security-and-identity-2, -3, -18 (RM exit 7) | `auth.basic`, `auth.mtls`, `authz.ip` schemas | (a) Register as authored (answered in CFG "Registered from feature documents") | Schemas exist; this area implements their semantics |
| OQ-tech-stack-and-libraries-25 (Yes, container images) | Where do static binaries get CA roots? | (a) The image ships a CA bundle; archives read the host store | `tlsconf.Client` uses `x509.SystemCertPool()`; a Node with an empty pool logs a startup warning **[proposed]**; JWKS/token/Upstream verification depends on it |
| OQ-observability-2 (Yes, TLS export M1) | OTLP TLS client settings and authentication | (a) `telemetry.otlp` fields (config-model owner) | Until fields exist: `https` endpoint with system roots via `tlsconf.Client`; builder accepts an optional client certificate so the fields plug in |

Not blocking M1 but touching code paths: OQ-security-and-identity-23 (short-key code; use RZ-CFG-026 meanwhile), -30 (M2; RZ-CFG-026 for the uncredentialed State Store URL meanwhile), -29 (cleartext as degraded), -20 (Go 1.27 `MaxHeaderValueCount`, data plane), OQ-configuration-model-19 (registering more `cors`/`auth.*` fields).

## 8. Deferred

| Deferred item | Milestone | Extension point in M1 code |
|---|---|---|
| Revocation list (`jti`, subject cut-off, `kid`, API key hash, basic username, cert serial), signed entries, marks, `revocation_sequence_gap` (OQ-security-and-identity-4, -5) | M2 | `identity.Revoker` called at SEC Figure 2 step F in every auth Filter, on success-cache hits and on every JWKS key load; M1 `NopRevoker`; RZ-AUTH-004 already emitted by CRLs |
| `authz.opa`, `authz.cedar` (OQ-13), engine memory ceiling | M2 | authz class accepts any `filter.Filter`; CEL activation builder exposes base variables as JSON for the OPA `input`; `identity.Identity` carries `Claims` and `Principal` for the Cedar principal mapping |
| `authz.geoip` (OQ-tech-stack-and-libraries-24), `source.country` (OQ-14) | M2 | `clientaddr.Result` is the single address input; `authz/ip.Set` reusable for per-country tables |
| `plugin` auth/authz classes, RZ-AUTH-016, `credentials.read` Capability | M2 | `redact` credential-header set is the one list the Plugin host reuses; `SetIdentity` accepts Plugin identities |
| `auth.upstream-sigv4` and transport-time signing hook (OQ-28) | M2 | Upstream-auth Filters set credentials only through `UpstreamHeader()`; leave a post-chain transport hook slot in the Upstream layer |
| `auth.upstream-oauth2` `grantType: jwt-bearer`, `serviceAccountKey`, `audience` (OQ-12) | M2 | `tokenSource` holds a `grant` interface (`Fetch(ctx) (token, ttl, error)`) with the M1 client-credentials implementation |
| Built-in JWT signing type (OQ-11) | M2 | None needed beyond jwx `jws` already linked |
| Signed Revisions (Control mode), Sigstore OCI Revisions and Plugins (RZ-CFG-033), trust policies, `RURALZ_TRUST_POLICY_FILE`, `revision_signature_off`/`plugin_signature_off`, signed OCI annotations (OQ-26) | M2 | `signing.Verifier` interface with M1 `DigestOnly`; loader calls one `Verify` step; `VerifyArtifact` for Plugins |
| Enrollment, 8091/8092 mTLS, TB-5/TB-11/TB-13, Node certificate renewal | M2/M4 | `tlsconf` builders are the single TLS policy place |
| Ruralz Control admin 9902 | M2 | `internal/adminauth` has no gateway imports |
| `kubernetes` and `vault` secret providers (OQ-10) | M2 | `secret.Resolver` provider switch over `SecretProvider`; M1 returns RZ-CFG-026 "provider not available" for them |
| OIDC discovery (`.well-known/openid-configuration`), Console SSO | M5 (Console) | `auth.jwt` takes explicit `jwksUrl` only |
| HTTP/3 (QUIC TLS), FIPS build (algorithm allowlist, `http3` refusal) | M3, M5 | `tlsconf` and the `jwt` algorithm table are single constants to narrow in FIPS builds |
| `AIProvider` `baseUrl` TLS and egress | M3 | `egress.Guard` and `tlsconf.Client` reused |
| gRPC `WithRequestGate` auth before decompression | M3 | Auth Filters depend only on headers |
| Per-issuer JWT fields, `auth.api-key` `config.query`, more `cors` fields (OQ-1 (a), OQ-configuration-model-19) | Later | `issuerConfig` and `cors` compiled structs carry defaults in one place |
| Secret-to-destination binding and State Store entry MAC key (OQ-22 residual) | Later | Diff impact rule 90 flags destination changes meanwhile |
| `ruralz bundle audit` | M2 | Static checks in `identity`/`egress` are pure functions it can call |

## 9. Risks and ambiguities

1. **jwx pulls `golang.org/x/crypto` through `jwt`→`jwe`.** `jwt` imports `jwe`, whose `jwebb` imports `golang.org/x/crypto/pbkdf2`; depgate's G3 x/crypto delegation allowlist is empty, so linking `jwt` fails stage 6. Resolution: import only `jwk`/`jws`/`jwa` and decode claims ourselves (rule 33); add depguard denies. Also note jwx brings `option/v3` v3.0.0-alpha1 (a pre-release) and `dsig`, `fastjson`: all MIT, but G2 must classify them and the tech-stack JOSE row should list them.
2. **OQ-9 contradiction.** Security proposes 503 (amend pack 8.10), Data plane marks 401 current, pack 8.10 and the registry say 401. Resolution: ship 401; status only from the registry.
3. **Rule 2 scope.** "A second Consumer binding" vs "if a `when` errors, both run and rule 2 applies": with JWT (no Consumer) plus API key both succeeding, a Consumer-only reading lets the request through with mixed identity. Resolution: any second successful authentication is RZ-AUTH-002 (rule 3). The doc should say so.
4. **Missing codes.** 421 for `auth.mtls` over a connection without a certificate request has no RZ code (propose RZ-AUTH-008 421); OQ-21's path rejection needs an RZ-RT code; static duplicate API key hash, `(issuer, subject)` or `(issuer, clientId)` across Consumers has no RZ-CFG code (propose RZ-CFG-038, until then runtime 002 only); short `secretRef` key and invalid PEM content reuse RZ-CFG-026 though its meaning is "unresolvable". Admin 401s reuse RZ-AUTH-001/002.
5. **Missing degraded reason and metric.** CRL past `nextUpdate` needs a reason (propose `crl_stale`); per-cause throttle refusal counters are "proposed to Observability" without a name (propose `ruralz_auth_throttle_refusals_total{cause}`). The OBS catalog CI check fails on names absent from the catalog, so OBS must add both.
6. **`auth.api-key` `header` has no default** in the schema; an absent header is legal. Resolution: register `+ruralz:default=x-api-key` (materialized in the canonical form) rather than a runtime default.
7. **CORS is under-specified.** Only `allowOrigins` and `allowMethods` exist; no `allowHeaders`, `exposeHeaders`, `allowCredentials`, `maxAge`, no wildcard rule, no rule for non-preflight disallowed origins. Resolution in rules 65 to 67 (reflect requested headers, `*` entry, no rejection of actual requests, GET/HEAD/POST default); SEC should author the full schema (OQ-configuration-model-19). Alternative for 67: reject disallowed actual requests with RZ-RT-008 (stricter, breaks server-to-server callers that send `Origin`).
8. **`auth.method` values and `auth.claims` for non-JWT methods** are not enumerated. Resolution: `jwt`, `api-key`, `basic`, `mtls`; empty map claims.
9. **SSRF guard covers Upstream Endpoints.** Loopback Upstreams (T1 development, quickstart mocks, CI) are refused unless `RURALZ_FETCH_ALLOW` lists them; `RURALZ_FETCH_ALLOW`'s format and "proxy variables unless allowlisted" are unspecified. Resolution: format of rule 85; `ruralz dev run` sets loopback allow; quickstart docs set it. Risk to SM-7 if forgotten.
10. **Pack amendments pending.** `RURALZ_ADMIN_*`, `RURALZ_SECRET_ROOT`, `RURALZ_FETCH_ALLOW` are not in pack section 2's process settings; OQ-7 and OQ-22 require pack section 2 and 5 amendments under pack section 14 before (or with) the code, since the pack is binding for names.
11. **Source-key throttle collapses behind a load balancer** when `trustedProxies`/`proxyProtocol` are unset: 10 failures per minute from anyone then refuse every uncached `auth.basic` attempt Node-wide. The doc turns it on once OQ-6 closes (it has). Mitigation: document the dependency; consider enabling the source key only when the effective peer is not the only address seen **[not adopted]**; flag for the security owner.
12. **Prefetch vs PB-7.** A Revision adding a new issuer can hold activation up to 5 s, beyond the 500 ms PB-7 budget; PB-7's fixture reuses its issuer, so the gate passes, but operators may see slow reloads. Resolution: prefetch concurrent with compilation; never wait for issuers already cached.
13. **Token without `kid`, JWKS limits, clock skew, fetch timeouts, redirect count, response caps** are unspecified; this spec proposes values (rules 28, 30, 36 to 38, 69 to 71). SEC should record them as targets.
14. **Subject matching is byte-exact against Go's RFC 4514 rendering**; authors writing `CN=a, O=b` (space) or another attribute order will never match. Resolution: document the canonical form; `ruralz bundle validate` could warn when a configured subject does not round-trip through a parser (no RFC 4514 parser in the stdlib; future work).
15. **`requestClientCert` on a listener** makes browsers prompt for certificates on every connection to that listener; operators SHOULD put `auth.mtls` Routes on a dedicated listener. Documentation risk.
16. **Upstream 401 with a cached upstream token** keeps failing until the 2/3 refresh; no invalidation rule exists. Suggest (not adopted): on an Upstream 401, mark the token stale and refetch at most once per 30 s.
17. **`client_secret_basic` only.** IdPs that accept only `client_secret_post` cannot be used in M1; needs a field (`config.clientAuthMethod`) later.
18. **CA roots in images** (OQ-tech-stack-and-libraries-25) are open; without them every JWKS/token/Upstream TLS verification fails in `FROM scratch` images.
19. **`EdDSA` vs RFC 9864 `Ed25519` `alg` name.** IdPs moving to `Ed25519` will be rejected (002) under the doc's allowlist; add under OQ-1 (a).
20. **`errcode.Lookup` allocates** (builds the full slice per call); Filters MUST resolve codes and statuses at compile time, not per request, to keep the 0-allocation budgets.
21. **Secret-provider ownership.** `internal/secret` may belong to the configuration area; if so, that area must implement rules 87 to 91 as written here, and this area consumes `secret.Store`.
22. **OQ-22 residual parts** ("secret-to-destination binding", "State Store entry MAC key") appear only as option names; M1 cannot build them without a design. Flag to the security owner so closing OQ-22 does not imply they exist.
23. **No verified-token cache** keeps JWT at one signature verification per request (budgeted 45/100 µs); high-rate RS256 deployments pay CPU. A cache keyed by token digest until `min(exp, key expiry)` is a possible M2 optimization; it must still consult the revocation hook.
