# M1 area 7: `headers`, `transform.request`, `transform.response`, `validation.json-schema`

Design reader spec for milestone M1 "Core gateway". Doc abbreviations: **DP** `docs/architecture/03-data-plane.md`, **CM** `docs/architecture/02-configuration-model.md`, **SEC** `docs/architecture/08-security-and-identity.md`, **OBS** `docs/architecture/10-observability.md`, **TMR** `docs/architecture/09-traffic-management-and-resilience.md`, **MP** `docs/architecture/07-multi-protocol.md`, **WASM** `docs/architecture/05-wasm-plugin-system.md`, **PB** `docs/architecture/12-performance-budgets-and-benchmarking.md`, **FC** `docs/features/01-feature-catalog.md`, **RM** `docs/roadmap/01-roadmap-and-milestones.md`, **FP** `docs/_meta/foundation-pack.md`, **RL** `docs/engineering/02-repository-layout-and-conventions.md`, **TS** `docs/engineering/01-tech-stack-and-libraries.md`, **TQ** `docs/engineering/03-testing-and-quality-strategy.md`. Sibling specs in this directory: area 1 `01-config-load.md`, area 2 `02-config-revision.md`, area 3 `03-cel.md`, area 4 `04-dataplane-core.md`, area 5 `05-upstream-traffic.md`, area 6 `06-security.md`. "Proposed" marks a rule the docs do not state; it needs the owning document's amendment or is an implementation choice that changes no contract.

## 1. Scope

| # | M1 item | Source (file, section) |
|---|---|---|
| S1 | `headers`, `transform.request`, `transform.response`, `validation.json-schema` Policy types | RM "M1 Core gateway" › "M1 scope", row *Traffic and state*; FP §10 "Policy type registry"; CM "Policy" (type registry) |
| S2 | `headers` `valueExpression` compiled and cost-estimated as a CEL place | RM "M1 scope", row *CEL*; CM "CEL expressions and allowed places" › "Allowed places" |
| S3 | `transform.*` `config` schema: `body`, `contentType`, `set[]`, `remove[]`, `arrayOps[]`, `replace[]`; RE2 limits; 32-entry cap; Phase, variables and output cap per scope; failure behavior | DP "Filter Chain execution" › "Transform Policies"; CM "Policy" › "Registered from feature documents" |
| S4 | `headers` `config` fields `request.set[]`, `response.set[]` with `name` and exactly one of `value` / `valueExpression` | CM "Policy" (table "`config` fields fixed here") |
| S5 | `validation.json-schema` in `onRequestBody`, draft 2020-12, 400 `RZ-RT-009` | FC "Request and response transformation" (row *JSON Schema request validation*); DP "RZ-RT registry"; TS "Library catalog" (row *JSON Schema validator*, "also behind `validation.json-schema`") |
| S6 | Feature rows: Header manipulation; Request body fields to headers or query; CEL-built Upstream request bodies; CEL response shaping and queries; Regular expression replacement; Array operations on responses; Cache-Control headers for CDNs | FC "Request and response transformation" |
| S7 | Feature rows: Browser security headers (Gateway `headers`, `overridable: false`, also on generated responses); Upstream forwarding control (Upstream-scoped `headers` removes client headers; allowlist per OQ-feature-catalog-5) | FC "Security"; SEC "Request hardening" (rows *Browser protections*, *Request smuggling*) |
| S8 | Failure codes `RZ-RT-009`, `RZ-RT-011`, `RZ-RT-012` (and `RZ-RT-004` for spent budget) | DP "Failure semantics" (transform and validation rows), "RZ-RT registry"; FP §8.10 |
| S9 | Body buffering: the three body types are gates; rewritten body reserved from `limits.maxBufferedBytes`; `Content-Length` recomputed, `Content-Encoding` dropped | RM "M1 scope", row *Configuration* ("body buffering"); CM "Body buffering and limits"; DP "Transform Policies" |
| S10 | Header handling interplay: hop-by-hop removal, forwarding headers, `traceparent` injection with its sampling decision | RM "M1 scope", rows *Ruralz Gateway* ("Router match and header handling") and *Observability* ("traceparent injection"); SEC "Request hardening", "IP filtering and GeoIP" (*Client address*); OBS "Tracing" › "Propagation" |
| S11 | Telemetry per Filter call: `ruralz_filter_*` metrics, `ruralz.filter.<name>` spans only in Phases the config uses | OBS "Ruralz Gateway metrics", "Span model"; DP "Error handling" |
| S12 | Budgets: `validation.json-schema` on 1 KiB, `headers` two values in `onUpstreamRequest`, `transform.response` on 1 KiB | RM "M1 scope", row *Budgets*; PB "Per-stage latency budget" |
| S13 | Quality: unit, property, golden, fuzzing, protocol conformance (HTTP/1.1, HTTP/2), integration | RM "M1 scope", row *Quality*; TQ "Test layers", "Fuzzing", "Conformance suites" |

Not in this area (consumed): the Filter Chain executor, body gate reader and budget (area 4), CEL module (area 3), chain resolution and Phase selection (area 2), offline schema checks (area 1), forwarding-header computation and the Upstream layer (areas 4 and 5).

## 2. Normative requirements

### A. Registry, scopes, Phase placement, ordering

1. Registry rows, exact: `headers`: Filter class transform; Phases `onRequestHeaders`, `onResponse`, at U `onUpstreamRequest`, `onUpstreamResponseHeaders`; scopes G, R, U; slot `name`; `failureMode` default `closed`, either allowed. `transform.request`: transform; `onRequestBody`, at U `onUpstreamRequest`; G, R, U; `name`; `closed`, either. `transform.response`: transform; `onResponse`, at U `onUpstreamResponseBody`; G, R, U; `name`; `closed`, either. `validation.json-schema`: validation; `onRequestBody`; R only; slot `validation`; `closed`, either. [FP §10; CM "Policy"]
2. Phase subscription is derived from `config` ("`headers` and `transform.*` run only in Phases their `config` uses"): [CM "Policy"; OBS "Span model"; area 2 R-13]

   | Type | G or R scope | U scope |
   |---|---|---|
   | `headers` | `onRequestHeaders` iff request ops exist; `onResponse` iff response ops exist | `onUpstreamRequest` iff request ops; `onUpstreamResponseHeaders` iff response ops |
   | `transform.request` | `onRequestBody` iff `body` is set or any list entry exists | `onUpstreamRequest`, same condition |
   | `transform.response` | `onResponse`, same condition | `onUpstreamResponseBody`, same condition |
   | `validation.json-schema` | R: `onRequestBody` | RZ-CFG-020 |

   "Request ops" are entries of `config.request.set[]` (plus, once registered, the proposed `request.add[]`, `request.remove[]`, req 20); "response ops" likewise under `config.response`. An empty config subscribes to nothing, creates no span and costs one nil check. Each Factory's returned `PhaseSet` MUST equal area 2's `registry.Phases` for the same config and scope (test 6.T4).
3. `validation.json-schema` at Gateway or Upstream scope is RZ-CFG-020; two of them in one Route's `spec.policies` share slot `validation` and are RZ-CFG-018. `headers` and `transform.*` stack (slot = own name) unless an author sets `slot`. [CM "Resolution rules" 2 and 4; FP §8.12]
4. Order within a Phase is Filter class (cors, auth, authz, admission, validation, cache, upstream-auth, transform, custom), then scope (G, R, U), then list position; response Phases run in reverse; `onLog` in request order. Consequences this area relies on: in `onRequestBody` a `validation.json-schema` runs after body-reading `authz.cel` and before every `transform.request`, so it validates the client body as received; in `onRequestHeaders` `headers` runs after auth, authz, admission and the cache lookup, so `consumer` and `auth` are bound; in `onUpstreamRequest` `headers` and `transform.request` run after `auth.upstream-oauth2`; in `onResponse` transform-class Policies run before `cache` (store), and within the class the Gateway `headers` Policy acts last on the response. [FP §8.12; CM "Resolution rules" 5, "Worked example"]
5. `transform.request`, `transform.response` and `validation.json-schema` are gates: the whole body is buffered before their Phase, nothing is forwarded and the response is uncommitted. `headers` is never a gate: its expressions MUST NOT select `request.body` or `response.body` (RZ-CFG-014, area 3 req 12). [CM "Body buffering and limits"]
6. Upstream-scoped instances run per leg attempt (`onUpstreamRequest`, `onUpstreamResponseHeaders`) or per leg (`onUpstreamResponseBody`). Every attempt starts from the request as the client-leg Phases left it (headers, raw query, buffered body); an attempt's U-scope edits never reach another attempt, leg or composition step. `transform.request` at U reads "the same buffered input each time". [DP "Transform Policies"; CM "Resolution rules" 4; area 5 req 28]
7. On generated responses (short-circuits and Node-generated errors, including this area's `RZ-RT-009`, `-011`, `-012`), `onResponse` runs `headers` response ops but never `transform.response`. [DP "Short-circuit"; area 4 req 36, 42]
8. `Policy.spec.when` is evaluated by the executor once before the Policy's first Phase at that attachment (area 3 req 11: `headers` first Phase is the request-op Phase when request ops exist, else the response-op Phase); `false` skips all its Phases for the request (per leg at U). [CM "Allowed places"; area 4 req 41]
9. None of these types calls the State Store or any remote dependency; `stateStoreTimeout` has no effect on them. [DP "Transform Policies"]

### B. Outcomes, failure semantics and codes

10. Outcomes follow the Filter SPI (area 4): *continue*, *respond*, *cannot decide*. Only `validation.json-schema` responds (400 `RZ-RT-009`, reqs 78 to 81). `headers` and `transform.*` never short-circuit by decision.
11. `headers` cannot decide on: a CEL runtime error in any `valueExpression` of the running side (area 3 kinds: cost limit at 1,000,000 units (target), deadline, selection on null, no such key or overload, conversion, arithmetic, result not a string); a computed value failing req 27; a recovered panic.
12. `transform.*` cannot decide on: a CEL runtime error in `body` or `set[].valueExpression`; a `body` result that cannot be written (NaN or infinity, non-string map keys; area 3 req 41); a JSON operation on a null body; a path through a non-object, a wildcard over a non-array or a numeric segment that would need a new array element (section H); output over its cap (reqs 49, 70); a computed header value failing req 27 or a query value over 8 KiB (proposed); a recovered panic. [DP "Transform Policies"]
13. `validation.json-schema` cannot decide only on a recovered panic or internal validator error. A body that is not acceptable JSON or fails the schema is a decision (400), never *cannot decide*.
14. Mapping (the Filter returns *cannot decide* with an empty code; the executor applies the class default): [DP "Failure semantics"; FP §8.10; area 4 req 43]

    | Phase kind | `closed` | `open` |
    |---|---|---|
    | Request Phase (`onRequestHeaders`, `onRequestBody`, `onUpstreamRequest`) | 503 `RZ-RT-011` | Skip this Policy for the request; message unchanged |
    | Response Phase before commit (`onUpstreamResponseHeaders`, `onUpstreamResponseBody`, `onResponse`) | 502 `RZ-RT-012`, replacing the response | Skip; the response passes unchanged |

    Statuses come from `errcode.Lookup` (`RZ-RT-009` 400, `RZ-RT-011` 503, `RZ-RT-012` 502), never literals elsewhere. "Unchanged" means byte-identical headers, raw query, body and framing (req 55).
15. A failed reservation from `limits.maxBufferedBytes` (decoded values or the rewritten body) is 503 `RZ-RT-004` under either `failureMode` and in any Phase before commit ("A spent buffer budget stays 503 `RZ-RT-004`"). A decoded value past 4 times the raw limit it arrived under (target) is oversized: request side 413 `RZ-RT-003`, plain `upstreams` response 502 `RZ-UP-010`, composition step failure (`RZ-RT-015`). The Filter reports these through SPI sentinels (section 3), not as *cannot decide*. [DP "Transform Policies", "Streaming"; CM "Body buffering and limits"; area 4 req 47]
16. At U scope, a `closed` failure in `onUpstreamRequest` ends only that leg with the generated 503 `RZ-RT-011` and no retry: for plain `upstreams` it becomes the client response; in composition it is the step's result and a non-2xx result fails a non-optional step (area 5 req 49; code: section 9 item 14). A `closed` failure in `onUpstreamResponseHeaders` or `onUpstreamResponseBody` replaces that leg's response with 502 `RZ-RT-012`; the replacement is final and triggers no retry (proposed). [DP "Short-circuit", "Failures and partial responses"]
17. After a response-Phase replacement in `onResponse`, the remaining `onResponse` Policies continue on the generated response (so a Gateway `headers-security` still applies), except `transform.response`, `cache` and `ai.semantic-cache`, which never run on generated responses (proposed refinement for area 4). A failure while processing an already generated response is counted and does not replace it again.
18. Problem documents: RFC 9457 `application/problem+json` with `title`, `status`, `code`, `requestId`, written by the executor through `internal/problem`; the body never echoes request content. `RZ-RT-009` MAY add the standard member `detail` naming only the failing schema keyword location (operator-authored text, req 81); never instance values, instance locations or property names taken from the request (proposed, needs area 4's `problem.Problem` to carry `Detail`). Proposed titles: `RZ-RT-009` "Request body failed validation", `RZ-RT-011` "Policy could not decide", `RZ-RT-012` "Response Policy failed". [DP "Error response format"]
19. Counting, recorded by the executor: each *cannot decide* increments `ruralz_filter_failures_total{policy, phase, mode}` (`mode` = `open` or `closed` applied); each 400 increments `ruralz_filter_short_circuits_total{policy, phase="onRequestBody", status_class="4xx"}`; every generated response counts in `ruralz_http_node_responses_total{code}`; the access log names the responding Policy and Phase in `short_circuit` and up to 8 undecided Policies in `failure_modes` (target). [OBS "Ruralz Gateway metrics", "Access logs"; area 4 req 44]

### C. `headers` configuration and validation

20. Registered fields (CM): `config.request.set[]` and `config.response.set[]`, `x-ruralz-list: map` keyed by `name`, each entry `name` (required) and exactly one of `value` (literal string) or `valueExpression` (CEL). Proposed for registration (OQ-configuration-model-19 (a), OQ-feature-catalog-5 (a); needed by FC rows "setting, adding and removing" and "Upstream-scoped `headers` Policies remove client headers"): `request.add[]`, `response.add[]` (same entry shape, `atomic` list, appends a field line) and `request.remove[]`, `response.remove[]` (`set` of names). Until registered, the Filter implements `set[]` only and ignores unregistered members, which the open `HeadersConfig` lets through unchecked. [CM "Policy"; FC "Header manipulation", "Upstream forwarding control"]
21. `name`: an RFC 9110 token (`1*tchar`; tchar is ALPHA, DIGIT or one of ``!#$%&'*+-.^_`|~``), 1 to 256 bytes (proposed), compared case-insensitively. Two entries of one list whose names are equal ignoring case are RZ-CFG-005 (the schema's map-key uniqueness is case-sensitive). [RFC 9110 §5.1]
22. Protected names, case-insensitive, RZ-CFG-005 in any `headers` op and any `transform.*` `set[]`/`remove[]` entry with `target: header` (proposed; mirrors WASM "Host Function rules" › *Message writes*): pseudo-header names (fail req 21 already); `host` (the Upstream layer sets `Host` from the Endpoint, area 5 req 29); hop-by-hop `connection`, `keep-alive`, `proxy-connection`, `te`, `trailer`, `transfer-encoding`, `upgrade`, `proxy-authenticate`, `proxy-authorization` (area 4 req 22); framing `content-length`; `expect` (dropped by area 5 req 28); trace context `traceparent`, `tracestate` (injected per attempt, OBS "Propagation"). The diagnostic names the protection class, e.g. "header traceparent is managed by the Node".
23. `value`: an RFC 9110 field value: no CR, LF, NUL or other control byte except HTAB, no leading or trailing SP/HTAB, at most 8 KiB (proposed); else RZ-CFG-005. `value` MAY be empty only once `value` becomes a pointer in the Go type (section 9 item 5). `${VAR}` is allowed in `value` (render time) and forbidden in `valueExpression` (RZ-CFG-011). A literal `value` on `authorization`, `proxy-authorization`, `cookie` or `x-api-key` warns RZ-CFG-013 (area 1 detector). [CM "Environment substitution", "secretRef"]
24. `valueExpression` places (area 3 req 10): `headers config.request.set[].valueExpression`: result string, variables Base (`request`, `source`, `route`, `consumer`, `auth`, `now`) at every scope; `headers config.response.set[].valueExpression`: result string, Base, `response` (status and headers, no body), `upstream`. Selecting `request.body` or `response.body` is RZ-CFG-014; `upstream` in a request op is RZ-CFG-014 as registered (section 9 item 6); an estimate over 10,000 cost units at nominal sizes (strings 256 bytes, lists and maps 32 entries) is RZ-CFG-015 (targets). The proposed `add[]` entries use the same places. [CM "Allowed places", "Limits"]
25. At most 32 entries across all lists of one `headers` Policy (proposed, mirrors the transform cap) else RZ-CFG-005, bounding per-request work to 32 CEL evaluations.

### D. `headers` runtime

26. Two passes per Phase: first evaluate every `valueExpression` of the running side in list order against the message as it reached the Policy (stop at the first error: *cannot decide*); then apply. No evaluation observes this Policy's own writes.
27. Computed values: leading and trailing SP/HTAB trimmed; then rejected (*cannot decide*) if any byte is a control byte other than HTAB (CR, LF and NUL included) or the value exceeds 8 KiB (proposed). An empty computed value sets an empty field. [RFC 9110 §5.5; WASM "Host Function rules"]
28. Apply order per side (proposed; only `set` exists until registration): `remove[]` (delete every field line of the name), `set[]` (replace all field lines of the name with exactly one), `add[]` (append one field line). `set` on `set-cookie` replaces every `Set-Cookie` line (documented behavior).
29. Target message per scope and side:

    | Scope, side | Phase | Edits |
    |---|---|---|
    | G/R request | `onRequestHeaders` | The client request headers: seen by later client-leg Policies and CEL `request.headers`, and the base of every leg's outgoing request |
    | U request | `onUpstreamRequest` | This attempt's outgoing request only |
    | G/R response | `onResponse` | The client response: an Upstream, merged or generated response |
    | U response | `onUpstreamResponseHeaders` | This attempt's Upstream response headers, before the retry decision |

30. Names are canonicalized once at compile time (`textproto.CanonicalMIMEHeaderKey`); runtime writes assign the canonical key directly; net/http lowercases names on HTTP/2.
31. Atomicity: under `open` (or when *cannot decide* is mapped to a replacement) no op of that Policy in that Phase applies. A literal-only Policy cannot fail.
32. Allocation target: setting two values allocates at most 2 Ruralz-owned objects (one `[]string` per set) (PB row `onUpstreamRequest`).

### E. Node header handling this area depends on

33. Hop-by-hop fields are removed in both directions: `Connection` and every field it names, `Keep-Alive`, `Proxy-Connection`, `TE` except `trailers`, `Trailer`, `Transfer-Encoding`, `Upgrade`, `Proxy-Authenticate`, `Proxy-Authorization`; `Expect` is dropped toward Upstreams. Owned by areas 4 (req 22) and 5 (reqs 28, 30); this area supplies the shared predicate `httpfield.IsHopByHop` so the lists cannot drift. [SEC "Request hardening" (*Request smuggling*); RFC 9110 §7.6.1]
34. Forwarding headers (area 4 req 18) are added when a leg's outgoing request is built, before `onUpstreamRequest`: from an untrusted peer `X-Forwarded-For` := peer address, `X-Forwarded-Proto` := listener scheme, `X-Forwarded-Host` := request host, `Forwarded` removed ("overwritten, never appended"); from a trusted peer (`Gateway.spec.trustedProxies`, PROXY v2) `X-Forwarded-For` gets `, <peer>` appended and the others are kept. They are not protected: a G/R `headers` op on them edits only the client copy, which leg build then overwrites or appends to; U-scope `headers` Policies are the supported way to remove or override them (for example `remove` of `x-forwarded-for` toward a third-party Upstream). [SEC "IP filtering and GeoIP" (*Client address*); CM "Gateway"]
35. `traceparent` (with the sampling decision) and `tracestate` are injected into every HTTP leg attempt by the telemetry area; they are protected (req 22), so no Policy can remove or forge them. The client's `traceparent` is never forwarded verbatim (its parent-id is replaced). [OBS "Propagation"]
36. Buffered bodies rewritten by this area are sent with `Content-Length` (never chunked); the Upstream layer and the response writer MUST take framing from the rewritten body, not from headers captured before the Phase. [DP "Transform Policies"]
37. Policy-added bytes are bounded by the entry cap and the per-value cap (32 × 8 KiB = 256 KiB per Policy, proposed); `limits.maxRequestHeaderBytes` applies to client input only.

### F. `transform.*` configuration and validation-time rules

38. Registered fields, exactly (closed configs; any other member is RZ-CFG-006): `config.body` (CEL, result `dyn`), `config.contentType` (string, default `application/json`, materialized into the canonical form), and `atomic` lists `config.set[]` (`target`: `header`, `query` (request only) or `body`; `name`; `valueExpression`, CEL string), `config.remove[]` (`target`, `name`), `config.arrayOps[]` (`op`: `move`, `append` or `delete`; `from`; `to`), `config.replace[]` (`path`, `pattern`, `replacement`, `literal`). `target: query` in `transform.response` fails the schema enum (RZ-CFG-005). [CM "Registered from feature documents"; DP "Transform Policies"]
39. Registered checks (implemented by area 1, `internal/config/validate`): a `pattern` over 1,024 bytes counted in UTF-8 bytes, not code points (target), or, unless `literal: true`, not valid RE2 under `regexp.Compile`, is RZ-CFG-005; more than 32 entries across `set`, `remove`, `arrayOps` and `replace` (`body` not counted) is RZ-CFG-005 (target). [CM "Registered from feature documents"; area 1 row 005]
40. Proposed additional RZ-CFG-005 checks (this area's `configcheck`, section 3): empty `pattern`; `name` of `target: body`, `replace[].path`, `arrayOps[].from` and `to` violating the dot-path grammar (req 59); `arrayOps[].to` containing `*`; `move` and `append` without both `from` and `to`; `delete` without `from` or with `to`; `target: header` names violating reqs 21 and 22; `target: query` names empty or over 256 bytes; `contentType` not parseable by `mime.ParseMediaType` or lacking `type/subtype`.
41. CEL places (area 3 req 10): `transform.request config.body` (dyn) and `config.set[].valueExpression` (string): Base with `request.body`, plus `upstream` only at U; `transform.response config.body` (dyn) and `config.set[].valueExpression` (string): Base and `response` with `body`, plus `upstream` only at U. `upstream` referenced by a Policy attached at G or R is RZ-CFG-014 (unavailable variable at that attachment); `${VAR}` in these fields is RZ-CFG-011; cost over 10,000 is RZ-CFG-015 (including the unbounded `json.encode`, whose hint is "a transform body may return the map or list itself", area 3 req 32). [CM "Allowed places", "Limits"]
42. `replacement` group references use `regexp.Regexp.Expand` syntax: `$1`, `${1}`, `$name`, `${name}`, `$$` for a literal `$`; `$1x` means the group named `1x` (use `${1}x`). Because `${` in any Bundle string is environment substitution, authors write `$${1}` (DP "Transform Policies"; CM "Environment substitution"). With `literal: true` neither `pattern` nor `replacement` is interpreted.
43. Snapshot compile (Node, per Policy and per distinct CEL environment, never per Route): `regexp.Compile(pattern)` once (literal patterns keep the string); dot paths parsed once; CEL programs from area 3's `cel.Builder`, reused across Revisions for identical (environment, source); a Filter whose Policy canonical config and attachment environment are unchanged MAY be carried over through `BuildEnv.Previous`. Compile never fails for a Bundle that passed validation; if it does, the Revision is rejected with the check's RZ-CFG code. [DP "Activation" step 3; area 3 req 48]

### G. Transform runtime

44. Input: the message body as it reached the Policy, after content decoding by the body layer, and the message's current `Content-Type`. A body is JSON iff its media type is `application/json` or ends in `+json` (case-insensitive, parameters ignored) and it decodes (req 58); otherwise, or when empty, `request.body`/`response.body` is null. [DP "Transform Policies"; area 3 req 25]
45. Pass 1, evaluation: `body` (if set), then every `set[].valueExpression` in list order, all against one activation describing the message as it reached the Policy; the first error is *cannot decide*. "Every CEL field reads the body as it reached the Policy, so no entry reads another's output." [DP "Transform Policies"]
46. Pass 2, operations, in the documented order: `body`, then `arrayOps[]`, `set[]`, `remove[]`, `replace[]`, each list in list order. The working state is either a JSON document (mutable tree) or raw bytes:
    - Initial state: if `body` is set, its result per area 3 req 41 (map or list: a document; top-level string: its UTF-8 bytes; bytes: raw; int, uint, double, bool, null: JSON text); else the input document if the input is JSON, else the input bytes with a null document.
    - A document-requiring step (`arrayOps`, `set`/`remove` with `target: body`, `replace` with `path`) on raw bytes decodes them (req 58); a null or undecodable document is *cannot decide* ("a JSON operation on a null body").
    - `replace` without `path` on a document first serializes it (req 60) and continues on the bytes.
    - `set`/`remove` with `target: header` or `query` are staged, in list order, and never touch the body state.
47. Commit, only after pass 2 succeeded: (a) if the body changed (`body` set, or any body-target op, `arrayOps` or `replace` entry executed), produce the final bytes, check the output cap (req 70), reserve them (reqs 69, 71), replace the body, set `Content-Type` to `contentType` when `body` is set, set `Content-Length` to the byte length, remove `Content-Encoding`, and (proposed) remove `Content-MD5`, `Digest`, `Content-Digest`, `Repr-Digest` and, on responses, turn a strong `ETag` into a weak one (`W/"…"`); (b) apply staged header ops in list order (`set` replaces all lines with one value, `remove` deletes all lines), so a `set` of `content-type` overrides `contentType`; (c) apply staged query ops. If the body did not change, its bytes and framing pass untouched (no re-serialization).
48. `set` with `target: query` replaces the first occurrence of `name` in place and drops later ones, or appends `name=value` at the end when absent; `remove` with `target: query` drops every occurrence. Keys compare after percent-decoding and `+` → space; untouched pairs keep their raw bytes and order; new keys and values are written with `url.QueryEscape`; `&` separates pairs and `;` is data (proposed; `url.Values.Encode` MUST NOT be used because it reorders). The rewritten query is seen by later Policies (`request.query`) and forwarded. [area 3 req 20]
49. Output caps and Phases: [DP "Transform Policies"]

    | Type and scope | Phase | CEL variables | Output cap |
    |---|---|---|---|
    | `transform.request` at G or R | `onRequestBody`, once | Base, `request.body` | `limits.maxRequestBodyBytes` |
    | `transform.request` at U | `onUpstreamRequest`, per leg attempt, from the same buffered input | Base, `request.body`, `upstream` | `limits.maxRequestBodyBytes` |
    | `transform.response` at G or R | `onResponse`, after any composition merge | Base, `response` with `body` | `limits.maxResponseBodyBytes` (default 10 MiB) |
    | `transform.response` at U | `onUpstreamResponseBody`, per leg | Base, `response` with `body`, `upstream` | Step `maxBodyBytes`, else `limits.maxResponseBodyBytes` |

50. A later Policy sees the rewritten body, headers and query: the executor MUST invalidate the shared decoded body and the CEL request/response view after commit (area 3 req 21, 25).
51. Messages that cannot carry content (a response to `HEAD`, status 1xx, 204, 304) and 206 responses: body-affecting steps are skipped without error; header and query steps apply (proposed).
52. Undecodable content codings (anything but `identity`, `gzip`, `deflate`) leave the input undecoded: the document is null, and a path-less `replace` on still-encoded bytes is *cannot decide* (proposed; section 9 item 12).
53. `transform.response` never runs on generated responses (req 7); it runs on Upstream error statuses like any other (authors guard with `when: 'response.status < 400'`).
54. At U scope, running the same Program twice on the same input yields the same output; the input message is never mutated in place (req 6).
55. Pass 1 happens before any mutation and pass 2 on a private working copy, so under `open` a skipped Policy leaves the message byte-identical (DP "under `open`, the Policy is skipped and the body passes unchanged").

### H. Dot paths and JSON operations

56. Working documents use a mutable tree of `map[string]any`, `[]any`, `string`, `json.Number` (input numbers keep their literal text), `bool` and `nil`. The shared decoded value used by CEL (area 3 `cel.Value`) is read-only and never mutated; the working copy is a separate decode of the current bytes, charged to the budget (req 69).
57. When the tree comes from a CEL `body` result, the tree is obtained by decoding the bytes area 3's `Value.AppendBody` writes (or an equivalent direct conversion, proposed `Value.Native`), so both paths give identical results.
58. JSON decoding for transforms: RFC 8259, one top-level value, trailing whitespace only, valid UTF-8, nesting depth at most 64 (proposed, the configuration loader's default of OQ-configuration-model-18), duplicate object names resolved "last wins" (area 3 req 25); cost reported for the budget, oversize at 4 times the raw limit (target).
59. Dot-path grammar (proposed; DP fixes only "a numeric segment indexes an array, and `*` matches every element"):

    ```abnf
    path     = segment *( "." segment )          ; 1 to 32 segments, at most 1,024 bytes
    segment  = wildcard / index / key
    wildcard = "*"
    index    = "0" / ( %x31-39 0*8DIGIT )         ; at most 9 digits
    key      = 1*keychar                          ; any UTF-8 except "."; not "*"; not an index
    ```

    A digit string with a leading zero or more than 9 digits is RZ-CFG-005. Keys containing `.` are unreachable in M1 (use `body`). The empty path is invalid; the root is addressed only by `body`.
60. Serialization (`jsonval.Append`): no insignificant whitespace; object members in ascending byte order of keys (as area 3 req 41); strings escaped per RFC 8259 minimal escaping (no HTML escaping); `json.Number` written verbatim, CEL ints in decimal, doubles in shortest round-trip form; no trailing newline. Output is deterministic for a given tree.
61. Resolution (read-like: selection for `move`/`append` sources, `remove`, `delete`, `replace` path): walk segments from the root. On an object, `key` and `index` segments look up the member named by the segment text; `*` on an object is `ErrWildcardNotArray`. On an array, `index` selects the element (out of range: missing), `*` fans out to every element in order, `key` is `ErrThroughScalar`. A missing member or element at any depth: no match. A scalar or `null` reached before the last segment: `ErrThroughScalar` ("a path through a non-object"). Both errors are *cannot decide*. (`null` handling: section 9 item 8.)
62. `set` with `target: body` writes the string from `valueExpression` (a JSON string; result type string per CM) at every match: a missing object member on the way is created as an empty object when the next segment is a `key` (or the value when last) ("`set[]` creates missing objects"); a missing path whose next segment is an `index` is `ErrCannotCreate`; an in-range `index` replaces the element; `*` over a missing member matches nothing; `*` elements must be objects when more segments follow.
63. `remove` with `target: body` deletes every match: an object member, or an array element (later elements shift); a missing path does nothing. With `*` over an array of objects, `items.*.internal` removes `internal` from each element.
64. `arrayOps` `move` (`from`, `to`): if `from` has no `*`, the single matched value is removed from `from` and written at `to` like `set` (a rename; missing `from` does nothing). If `from` has `*`, every matched value is collected in document order, each array value contributing its elements (one level of flattening), the matches are removed, and the collection is appended to the array at `to` (created when missing; a non-array at `to` is `ErrThroughScalar`). Example: `move` `items.*.sku` → `skus` yields `skus` = every item's `sku` in order, and each item loses `sku` (DP Transform Policies example).
65. `arrayOps` `append` (`from`, `to`): like wildcard `move` but copies instead of removing, for any `from`; an array matched at `from` contributes its elements; missing `from` does nothing; `to` is created as an array when missing.
66. `arrayOps` `delete` (`from`): deletes every match (array elements shift; `items.*` empties `items`); missing does nothing.
67. `replace` with `path`: every matched string value is rewritten; a matched non-string value is left unchanged (proposed); missing does nothing. Without `path`: applied to the raw body bytes. Regex mode: `re.ReplaceAll` semantics with `Expand` templates; literal mode: `bytes.ReplaceAll`/`strings.ReplaceAll`. The output length is checked incrementally against the cap before allocation (literal: `len + n × (len(replacement) − len(pattern))` with `n` from `bytes.Count`; regex: accumulate from `FindAllSubmatchIndex`), so a blow-up fails as *output over cap* without allocating it.
68. All operations are applied to the private working copy (req 55); errors leave the message untouched.

### I. Buffer accounting and caps

69. The working copy's decoded values are reserved from `limits.maxBufferedBytes` at the size the decoder reports (in 32 KiB increments, target), released at request end or when superseded. [CM "Body buffering and limits"; area 4 req 47]
70. The final bytes are checked against the output cap of req 49 before reservation; over the cap is *cannot decide* (not 413 or 502).
71. The rewritten body is reserved "like a decoded value"; failure is 503 `RZ-RT-004` (req 15). The superseded body's reservation is released after the swap.
72. Input bodies over their limits never reach these Filters: the gate reader answers 413 `RZ-RT-003` (request) or 502 `RZ-UP-010` (plain `upstreams` response) or fails the step (`RZ-RT-015`). [area 4 req 47; TMR "Error codes this document owns"]
73. Scratch buffers come from a `sync.Pool`; buffers larger than 64 KiB are not returned to the pool (proposed), so the pool never pins large bodies.

### J. `validation.json-schema` configuration (proposal for registration)

74. `config.schema` (required, proposed): an inline JSON Schema document, draft 2020-12, as a YAML or JSON object or a boolean. No other field in M1. The schema is part of the canonical form and Revision digest (keys canonicalized like any `config`); overlays merge it as one atomic value (proposed); environment substitution applies to its string scalars, so a `pattern` containing `${` needs `$${`. [FC row *JSON Schema request validation*; AI doc "`tools[].inputSchemaFrom`" (the body schema of an attached `validation.json-schema` Policy is materialized into the canonical form)]
75. Offline checks, RZ-CFG-005 (proposed; all three binaries, identical diagnostics): compile with `santhosh-tekuri/jsonschema/v6` using default draft 2020-12; every `$schema` in the document, when present, is exactly `https://json-schema.org/draft/2020-12/schema`; a URL loader that refuses every URL, so `$ref` resolves only to `#…` fragments and embedded `$id` resources (never `file:`, `http:`, `https:`; no filesystem or network access at validation or on the Node); `format` and content keywords are annotations only (library defaults, no `AssertFormat`, no `AssertContent`); regexes use Go `regexp` (RE2), so ECMA-262-only syntax (look-around, back-references) is RZ-CFG-005; canonical size at most 256 KiB (proposed); nested applicator fan-out (the product of `anyOf`/`oneOf`/`allOf` branch counts and `if`/`then`/`else` along any schema path, `$ref` cycles counted once) at most 1,024 (proposed). The diagnostic path points into `spec.config.schema` using the library's keyword location.
76. Unknown keywords inside `schema` are ignored per draft 2020-12 (including `x-ruralz-*`, which carry no meaning there).
77. The compiled schema is shared by every Route attaching the Policy; compile once per Policy per snapshot, reused across Revisions by canonical-bytes SHA-256 (proposed).

### K. `validation.json-schema` runtime

78. Media type: `application/json` or `*/*+json`, parameters allowed only `charset=utf-8` (case-insensitive); anything else, or no `Content-Type`, is 400 `RZ-RT-009` (detail "Content-Type is not JSON", proposed).
79. An empty body is 400 `RZ-RT-009` (detail "Request body is empty"); Routes accepting bodiless methods guard the Policy with `when: 'request.method in ["POST", "PUT", "PATCH"]'`.
80. Decode (strict, proposed): RFC 8259, valid UTF-8, one value, trailing whitespace only, nesting depth at most 64, and duplicate object names rejected (a validated body is forwarded unchanged, and a duplicate would let the Upstream parser see a value the schema never checked); any violation is 400 `RZ-RT-009` (detail "Request body is not valid JSON"). Numbers stay `json.Number` so `multipleOf`, `minimum` and large integers validate exactly. Decoded values are reserved like any decoded value (req 15).
81. Validation against the compiled schema; failure is 400 `RZ-RT-009` with `detail` "Request body does not match the schema at `<keyword location>`" where `<keyword location>` is the absolute keyword location of the first leaf error in the library's basic output (deterministic order), a JSON Pointer into `schema` (proposed).
82. Success is *continue*: the raw body bytes, headers and framing pass unchanged; the decoded tree is released.
83. Order: after `authz.cel` rules reading the body and before any `transform.request` (req 4); a Response Cache on the same Route is rejected at validation (proposed RZ-CFG-038, area 2 R-39; OQ-traffic-management-and-resilience-21).
84. Budget: `onRequestBody` with `validation.json-schema` on a 1 KiB body: 25 µs p50, 80 µs p99, 0 Ruralz-owned allocations (target) (PB "Per-stage latency budget"; section 9 item 20).

### L. Telemetry, redaction, logging

85. The executor records per call `ruralz_filter_duration_seconds{policy, phase}` (histogram `fast`), and the counters of req 19; this area adds no metric name. [OBS "Ruralz Gateway metrics"]
86. Sampled requests get one `ruralz.filter.<name>` span (name = Policy `metadata.name`) per subscribed Phase call only, attributes type, Phase, outcome and applied `failureMode`; on *cannot decide* the Filter supplies `error.type` from the fixed set `cel_error`, `invalid_value`, `null_body`, `path_error`, `output_cap`, `result_type`, `internal` (proposed). [OBS "Span model"]
87. No header value, query string, body content or CEL error detail text (`EvalError.Detail`) reaches a metric label, span attribute, log or `/tap` from this area. [OBS O4; SEC "Secrets" rule 2]
88. No request-path logging in these packages; compile problems surface only as RZ-CFG diagnostics. [RL "Code conventions"; area 3 req 54]

### M. Performance and compile budgets

89. `headers` setting two values in `onUpstreamRequest`: 2 µs p50, 8 µs p99, 2 allocations (target); `transform.response` on 1 KiB in `onUpstreamResponseBody`: 30 µs p50, 100 µs p99, 0 Ruralz-owned allocations (target); CEL evaluation under 2 µs p99 (target). [PB "Per-stage latency budget"; CM "Limits"]
90. Filters are built per Policy and scope environment, not per Route, inside the compile stage budget (Routers, Filter Chains and CEL at 100 µs or less per Route, 400 ms for 5,000 Routes, target); each Factory call is one unit of work between the loader's 100 µs yields (OQ-performance-budgets-and-benchmarking-6 (a)). [PB "Hot Reload stages"]
91. Compiled Filters are immutable and safe for concurrent `Handle` calls; per-request state lives in the Exchange and pooled scratch buffers. [RL "Code conventions"]

## 3. Proposed Go packages and API

Placement follows RL "Where Go code goes" (dots become slashes, hyphens dropped; `headers` → `internal/filter/header`) and area 4's rule that `internal/filter/...` never imports `internal/gateway/...`, so `internal/config` (linked into all three binaries) can import this area's pure check functions. Every file: license header; no `init()`; no mutable package-level variables (lookup tables are `switch` statements; sentinel errors allowed); context first; `%w` wrapping; `slog` only via `internal/telemetry`.

| Package | Responsibility | Imports beyond stdlib |
|---|---|---|
| `internal/httpfield` | RFC 9110 name and value rules, protected names, hop-by-hop predicate, order-preserving query editor | none |
| `internal/jsonval` | Mutable JSON tree: strict decode with cost accounting, deterministic encode, media-type test | none (stdlib `encoding/json/jsontext`) |
| `internal/filter/header` | `headers` Factory, Filter, `Check` | `internal/cel`, `internal/filter`, `internal/httpfield`, `pkg/config/v1alpha1` |
| `internal/filter/transform` | Shared engine: `Spec`, `Check`, `Compile`, `Program.Handle` | as above plus `internal/jsonval`, `.../transform/dotpath` |
| `internal/filter/transform/dotpath` | Path grammar and tree operations | `internal/jsonval` |
| `internal/filter/transform/request`, `.../response` | Factories adapting `v1alpha1` configs to `transform.Spec` | `internal/filter/transform` |
| `internal/filter/validation/jsonschema` | `validation.json-schema` Factory, schema compile, Filter | `github.com/santhosh-tekuri/jsonschema/v6` |
| `internal/filter/configcheck` | Pure offline RZ-CFG-005 checks for the four types, injected into area 1's validator | the four packages above (no CEL) |

```go
// Package httpfield holds the RFC 9110 field rules shared by headers,
// transform, the Upstream layer (areas 4, 5) and, from M2, the Plugin host.
package httpfield // internal/httpfield

const (
	MaxNameBytes          = 256     // proposed
	MaxComputedValueBytes = 8 << 10 // proposed
)

func ValidName(name string) bool // 1*tchar, at most MaxNameBytes
func ValidValue(v string) bool   // no CTL except HTAB; no leading or trailing SP/HTAB
func TrimOWS(v string) string

type Protection uint8

const (
	Unprotected Protection = iota
	Pseudo                 // ":"-prefixed
	HostField              // host
	HopByHop               // connection, keep-alive, proxy-connection, te, trailer, transfer-encoding, upgrade, proxy-authenticate, proxy-authorization
	Framing                // content-length
	ExpectField            // expect
	TraceContext           // traceparent, tracestate
)

func Protect(name string) Protection // case-insensitive switch, no allocation
func (p Protection) String() string
func IsHopByHop(name string) bool                // the fixed list only
func ConnectionOptions(h http.Header) []string   // names listed in Connection, lowercased

// Query edits a raw query string without reordering or re-encoding untouched pairs.
type Query struct{ /* pairs []pair{raw string; key string (decoded)} */ }

func ParseQuery(raw string) Query      // never fails: undecodable keys compare by raw text
func (q *Query) Set(name, value string) // first occurrence in place, others dropped; else appended
func (q *Query) Del(name string)
func (q *Query) AppendRaw(dst []byte) []byte
```

```go
// Package jsonval is the mutable JSON tree used by transforms and validation.
package jsonval // internal/jsonval

// Tree values: map[string]any, []any, string, json.Number, bool, nil.
type Options struct {
	MaxDepth         int   // 64 (proposed)
	MaxCost          int64 // 4 × the raw limit the body arrived under (target)
	RejectDuplicates bool  // validation.json-schema only
}

var (
	ErrSyntax        = errors.New("jsonval: invalid JSON")
	ErrDuplicateName = errors.New("jsonval: duplicate object name")
	ErrDepth         = errors.New("jsonval: nesting too deep")
	ErrTooLarge      = errors.New("jsonval: decoded value over its limit")
)

func Decode(data []byte, o Options) (v any, cost int64, err error)
func Append(dst []byte, v any) ([]byte, error) // req 60; error on NaN/Inf or unsupported types
func Cost(v any) int64                          // the same accounting Decode reports
func IsJSONMediaType(contentType string) bool   // application/json or +json, params ignored
```

```go
package dotpath // internal/filter/transform/dotpath

const (MaxSegments = 32; MaxBytes = 1024; MaxIndexDigits = 9)

type Kind uint8

const (Key Kind = iota + 1; Index; Wildcard)

type Segment struct {
	Kind  Kind
	Key   string // Key and Index: the segment text
	Index int    // Index only
}

type Path struct{ /* segs []Segment; wild int; src string */ }

func Parse(s string) (Path, error) // req 59; error text becomes the RZ-CFG-005 message
func (p Path) String() string
func (p Path) HasWildcard() bool

var (
	ErrThroughScalar    = errors.New("dotpath: path through a non-object")
	ErrWildcardNotArray = errors.New("dotpath: wildcard over a non-array")
	ErrCannotCreate     = errors.New("dotpath: cannot create an array element")
)

func Select(root any, p Path, visit func(v any) error) error          // document order
func Set(root *any, p Path, v any) error                              // req 62
func Remove(root *any, p Path) (n int, err error)                     // reqs 63, 66
func Take(root *any, p Path) (vals []any, err error)                  // select then remove (move)
func Update(root *any, p Path, fn func(s string) (string, error)) error // replace with path (req 67)
```

```go
package transform // internal/filter/transform

type Side uint8

const (Request Side = iota + 1; Response)

type Target uint8

const (Header Target = iota + 1; Query; Body)

type SetSpec struct{ Target Target; Name, ValueExpression string }
type RemoveSpec struct{ Target Target; Name string }

// Spec is the side-independent form of TransformRequestConfig and TransformResponseConfig.
type Spec struct {
	Body        string
	ContentType string // materialized default "application/json"
	Set         []SetSpec
	Remove      []RemoveSpec
	ArrayOps    []v1alpha1.ArrayOp
	Replace     []v1alpha1.Replace
}

// Check runs req 40 (no CEL; req 39 stays in area 1). Paths in Issues are relative to spec.config.
func Check(side Side, s Spec) []configcheck.Issue

// Program is one compiled Policy at one scope environment; immutable.
type Program struct{ /* side; body *cel.Program; ops; regexps; paths; flags writesBody, readsBody */ }

func Compile(side Side, s Spec, env filter.BuildEnv) (*Program, filter.PhaseSet, error) // error wraps errcode RZ-CFG-*
func (p *Program) Handle(ctx context.Context, ph filter.Phase, x *filter.Exchange) filter.Result
```

```go
package request // internal/filter/transform/request (response is symmetric)

const Type = v1alpha1.PolicyTypeTransformRequest

type Factory struct{}

func (Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, filter.PhaseSet, error)
func SpecOf(cfg *v1alpha1.TransformRequestConfig) transform.Spec
```

```go
package header // internal/filter/header

const Type = v1alpha1.PolicyTypeHeaders

type Factory struct{}

func (Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, filter.PhaseSet, error)
func Check(cfg *v1alpha1.HeadersConfig) []configcheck.Issue // reqs 21-23, 25

type op struct {
	kind  opKind // remove, set, add
	key   string // canonical MIME key
	value string // literal
	expr  *cel.Program
}

type Filter struct{ req, resp []op } // immutable

func (f *Filter) Handle(ctx context.Context, ph filter.Phase, x *filter.Exchange) filter.Result
```

```go
package jsonschema // internal/filter/validation/jsonschema

import sjs "github.com/santhosh-tekuri/jsonschema/v6"

const (
	Type           = v1alpha1.PolicyTypeValidationJSONSchema
	Draft          = "https://json-schema.org/draft/2020-12/schema"
	MaxSchemaBytes = 256 << 10 // proposed
	MaxFanOut      = 1024      // proposed
)

type Schema struct{ /* s *sjs.Schema; raw json.RawMessage; sum [32]byte */ }

// CompileSchema applies req 75 with a refusing sjs.URLLoader; errors carry the keyword location.
func CompileSchema(raw json.RawMessage) (*Schema, error)
func (s *Schema) Validate(v any) (keywordLocation string, ok bool)
func (s *Schema) Raw() json.RawMessage // M2: ruralz bundle export openapi, AI tools inputSchemaFrom

func Check(cfg *v1alpha1.ValidationJSONSchemaConfig) []configcheck.Issue

type Factory struct{} // Build: CompileSchema, reusing BuildEnv.Previous when the sum is equal
type Filter struct{ schema *Schema }

func (f *Filter) Handle(ctx context.Context, ph filter.Phase, x *filter.Exchange) filter.Result
```

```go
package configcheck // internal/filter/configcheck

type Issue struct {
	Code    string // "RZ-CFG-005"
	Pointer []any  // path under spec.config: string keys and int indexes
	Message string
}

// Checker implements area 1's proposed validate.PolicyConfigChecker; a constructed
// table dispatches headers, transform.request, transform.response, validation.json-schema.
type Checker struct{ /* unexported table */ }

func New() *Checker
func (c *Checker) CheckPolicyConfig(t v1alpha1.PolicyType, config json.RawMessage) []Issue
```

Proposed `pkg/config/v1alpha1` changes (Configuration model owner review; schema regenerated):

```go
// ValidationJSONSchemaConfig is the config of a validation.json-schema Policy.
// +ruralz:policyType=validation.json-schema
type ValidationJSONSchemaConfig struct {
	// Schema is an inline JSON Schema (draft 2020-12) the request body must match.
	// +ruralz:required
	Schema JSONSchemaDocument `json:"schema"`
}

// JSONSchemaDocument is any JSON object or boolean, kept as written; schemagen emits
// {"type": ["object", "boolean"]} and overlays replace it atomically.
type JSONSchemaDocument json.RawMessage

// HeaderRequestOps gains (after registration):
//	// +ruralz:list=atomic
//	Add []HeaderRequestSet `json:"add,omitempty"`
//	// +ruralz:list=set
//	Remove []string `json:"remove,omitempty"`
// and HeaderResponseOps likewise. HeaderRequestSet.Value and HeaderResponseSet.Value
// become *string; Replace.Path loses +ruralz:required (section 9 items 3, 5).
```

Concurrency model: Factories run on the loader's W = max(1, `GOMAXPROCS`/2) compile workers, one Policy per unit, and share only area 3's thread-safe `cel.Builder`; the resulting Filters, `regexp.Regexp`, `dotpath.Path` and `sjs.Schema` values are immutable and read concurrently by request goroutines. `Handle` runs on the request goroutine (or a composition step goroutine, with that step's Exchange), starts no goroutine, takes no lock, and uses only the Exchange plus pooled scratch buffers. No channels.

Exported for other areas: `httpfield` (areas 4 and 5 hop-by-hop and name checks; M2 Plugin host write rules), `jsonval` (area 5 composition merge MAY reuse it; area 3 `DecodeJSON` stays separate for CEL values), `dotpath` (area 5 `target` unwrap MAY reuse `Select`), `configcheck.Checker` (area 1), `jsonschema.CompileSchema`/`Schema.Raw` (M2 OpenAPI import and export), the four Factories (area 4 registry).

## 4. Dependencies on other areas

| Direction | Area | Contract |
|---|---|---|
| Needs | Area 4 (`internal/filter` SPI) | `Filter.Handle(ctx, Phase, *Exchange) Result`; `Factory.Build(ctx, BuildEnv) (Filter, PhaseSet, error)`; `Result{Outcome, Response, Code, Err}` with class-default codes (transform, validation → `RZ-RT-011`/`RZ-RT-012`). Additions requested: `BuildEnv.CEL *cel.Builder` and `BuildEnv.Limits` (`maxRequestBodyBytes`, `maxResponseBodyBytes`, step `maxBodyBytes`); Exchange accessors for the Phase's message: request headers (client or leg), `RawQuery`/`SetRawQuery`, `Method`, `RequestBody(ctx)` and `SetRequestBody(ctx, b []byte) error`, `Response().Body(ctx)` and `SetBody(ctx, b) error` (both reserve, recompute `Content-Length`, apply req 47(a)), `Response().Generated() bool`, `Budget().Reserve(n) error`/`Release(n)`, `Vars() *cel.Vars` refreshed after commit (req 50); SPI sentinels `filter.ErrBudget` (→ 503 `RZ-RT-004` under any `failureMode`, any Phase before commit) and `filter.ErrTooLarge` (→ req 15 oversize rule); `Response.Detail` for `RZ-RT-009` (req 18); req 17 continuation after a response-Phase replacement |
| Needs | Area 4 (body layer) | Gate buffering before `onRequestBody`, `onUpstreamResponseBody` and `onResponse` for these types; content decoding (`identity`, `gzip`, `deflate`) with the original bytes kept for untouched bodies; 413/502/`RZ-RT-015` for inputs over limits; framing from the rewritten body (req 36) |
| Needs | Area 4/5 (header handling) | Hop-by-hop stripping and `Expect` drop (req 33) using `httpfield`; forwarding headers at leg build before `onUpstreamRequest` (req 34); leg outgoing request cloned per attempt (req 6); `Host` from the Endpoint |
| Needs | Area 3 (CEL) | Places `headers config.request.set[].valueExpression`, `headers config.response.set[].valueExpression`, `transform.request config.body`/`config.set[].valueExpression`, `transform.response config.body`/`config.set[].valueExpression` with per-scope environments; `Builder.Compile(Site, expr)`; `Program.EvalString`, `EvalValue`; `Value.AppendBody` (req 46); body-selection RZ-CFG-014 (req 5); `EvalError.Kind` for `error.type`; proposed `Value.Native() (any, error)` |
| Needs | Area 2 (registry, precedence) | Registry rows of req 1; `Phases` per req 2, extended to the proposed `add`/`remove` ops; ordering of req 4; RZ-CFG-018/020; proposed RZ-CFG-038 for `cache` plus `validation.json-schema` |
| Needs | Area 1 (validation) | Existing RZ-CFG-005 rows for `pattern` (bytes) and the 32-entry cap; proposed `validate.Checkers.PolicyConfig` hook calling `configcheck.Checker`; RZ-CFG-006 on unknown transform members; RZ-CFG-011; RZ-CFG-013 detector for `headers` literals; schema regeneration for section 3's type changes |
| Needs | Telemetry area | Executor-side metrics and spans (req 85, 86), `error.type` attribute, `traceparent`/`tracestate` injection per attempt (req 35) |
| Needs | `internal/errcode` | `RZ-RT-009`, `-011`, `-012`, `-004`, `-003`, `-015`, `RZ-UP-010` (registered); RZ-CFG codes (registered) and proposed RZ-CFG-038 |
| Provides | Areas 1, 2, 4, 5, M2 Plugin host | Four `filter.Factory` implementations; `configcheck.Checker`; `httpfield`; `jsonval`; `dotpath`; `jsonschema.CompileSchema` |

## 5. Libraries

| Module | Version | Use here | Why | Catalog row |
|---|---|---|---|---|
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.3 (latest v6.0.x on proxy.golang.org; `go 1.21`) | `internal/filter/validation/jsonschema` only: `NewCompiler`, `DefaultDraft(Draft2020)`, `UseLoader` (refusing loader), `AddResource`, `Compile`, `Schema.Validate`, `ValidationError` basic output | Already linked into all three binaries for Bundle validation (area 1), draft 2020-12 compliance, Go `regexp` by default, instance and keyword locations | TS "JSON Schema validator" ("also behind `validation.json-schema`") |
| `cel.dev/cel-go` | v0.32.0 | Not imported here; reached through area 3's `internal/cel` | Single CEL environment owner | TS "Expressions" |
| `golang.org/x/text` | ≥ v0.22.0 by MVS (jsonschema asks v0.14.0, cel-go v0.22.0) | Transitive (jsonschema's `message` formatting) | Not imported directly | Transitive; license gate G2 (BSD-3-Clause) |
| `github.com/dlclark/regexp2` | v1.11.0 in jsonschema's `go.mod` | Not linked (jsonschema tests only); never passed to `UseRegexpEngine` | ECMA regex would break linear-time matching | None needed (not in `go list -deps ./cmd/...`) |
| Standard library | Go 1.26 floor, go1.27.1 toolchain | `net/http`, `net/textproto`, `net/url`, `mime`, `regexp`, `bytes`, `strings`, `unicode/utf8`, `sync`, `crypto/sha256`, `encoding/json` (`RawMessage`, `Number`), `encoding/json/jsontext` (strict tokenizer; needs `GOEXPERIMENT=jsonv2` on the 1.26 floor, already required by jwx and the floor guard) | No new module; `golang.org/x/net/http/httpguts` is not used (no catalog row), `httpfield` reimplements the token and value tables | TS "HTTP/1.1 and HTTP/2"; RL "Toolchain and modules" |

## 6. Test plan

All unit tests hermetic, shuffled, under `-race` and again with `CGO_ENABLED=0`; floor job on Go 1.26 with `GOEXPERIMENT=jsonv2`. Golden files under `testdata/` with `eol=lf`.

**Unit and table tests**

- T1 `httpfield`: `ValidName` (every tchar; empty; 256 and 257 bytes; space, `:`, `(`, non-ASCII); `ValidValue` (CR, LF, NUL, 0x01, 0x7F rejected; inner HTAB and obs-text 0x80-0xFF accepted; leading/trailing SP rejected); `Protect` for every protected name in three casings and for `x-forwarded-for`, `content-type`, `set-cookie` (unprotected); `Query`: `a=1&b=2&a=3` `Set(a,9)` → `a=9&b=2`; `Set(c,x y)` appends `c=x+y`; `Del(a)`; key `a+b` equals `a b`; `x=%7e` untouched stays `%7e`; `;` kept as data; empty query; bare `k`; `=v`.
- T2 `dotpath.Parse`: valid `a`, `a.b`, `items.*.sku`, `a.0`, `a.123456789`; invalid ``, `.`, `a.`, `.a`, `a..b`, `a.01`, `a.1234567890`, 33 segments, 1,025 bytes; each invalid case maps to an RZ-CFG-005 message.
- T3 Operation semantics table: every op (`set` body, `remove` body, `move` with and without `*`, `append`, `delete`, `replace` with path) × every shape at each segment (missing, `null`, scalar, empty array, array of objects, array of scalars, object, index in and out of range) with expected tree or error kind; the DP example `orders-shape` (`move items.*.sku → skus`, `set` header `x-order-total` from `string(response.body.total)`, `remove` body `internal`, `replace` `customer.email` `^[^@]+` → `***`) as a golden.
- T4 Phase selection: for each type and scope, configs with request ops only, response ops only, both, none; `Factory.Build` PhaseSet equals area 2 `registry.Phases`; empty `transform.*` config subscribes nothing.
- T5 Ordering and isolation within one Policy: `set` header from `response.body.x` after a `remove` of body `x` still sees the original `x`; `body` plus `arrayOps` operates on the `body` result; path-less `replace` after a JSON op sees the serialized document; a path `replace` after a path-less one re-decodes; `set` `content-type` overrides `contentType`.
- T6 `body` results: map, list, string, bytes, int, uint, double, bool, null, NaN (→ *cannot decide*, `result_type`), nested bytes (base64), map with an int key (→ *cannot decide*).
- T7 Failure mapping: for every *cannot decide* cause of reqs 11 to 13, in each subscribed Phase and scope: `closed` → `Result{CannotDecide, Code ""}` and, through the real executor, 503 `RZ-RT-011` (request Phase) or 502 `RZ-RT-012` (response Phase); `open` → message byte-identical (headers map deep-equal, raw query equal, body bytes and `Content-Length` equal).
- T8 Budget and oversize: a fake Budget refusing the rewritten body → `filter.ErrBudget` → 503 `RZ-RT-004` under `open` and `closed`, in `onRequestBody` and `onResponse`; decoded cost at 4× + 1 → `filter.ErrTooLarge` → 413 `RZ-RT-003` (request) and 502 `RZ-UP-010` (plain response).
- T9 Caps: output exactly at `maxRequestBodyBytes` passes, one byte over is *cannot decide* (`output_cap`); literal `replace` blow-up (1 KiB input, 1-byte pattern, 1 MiB replacement) fails before allocating (assert via `testing.AllocsPerRun` bound); computed header value 8 KiB passes, 8 KiB + 1 fails; query value over 8 KiB fails.
- T10 Framing: after a rewrite `Content-Length` = `len(body)`, `Content-Encoding`, `Content-MD5`, `Digest`, `Content-Digest`, `Repr-Digest` absent, strong `ETag` weakened; header-only transform leaves body, `Content-Length`, `Content-Encoding` and `ETag` untouched; HEAD, 204, 304 and 206 skip body steps but apply header steps.
- T11 `headers`: `set` replaces three existing lines with one; `set-cookie` replacement; response ops on a generated 401 and on 503 `RZ-RT-011` from another Policy; value `"a\r\nb"` and `"\x00"` → *cannot decide*; trimming `"  v  "` → `v`; literal-only Policy never fails; U-scope Policy run for attempts 1 and 2 of one leg produces one header line each time.
- T12 U-scope idempotence: `transform.request` at U handled twice on the same Exchange input yields identical outgoing bodies and never modifies the client-leg body.
- T13 `validation.json-schema` content types: `application/json`, `application/json; charset=UTF-8`, `application/vnd.api+json` pass to validation; `text/json`, `application/json; charset=utf-16`, `text/plain`, missing → 400 `RZ-RT-009`; empty body; `{} {}`; `{"a":1,"a":2}`; depth 65; invalid UTF-8; `1e400` and `12345678901234567890` against `maximum`/`multipleOf`; boolean schemas `true` and `false`; success forwards the body byte-identical.
- T14 Problem documents: goldens for 400 `RZ-RT-009` (each `detail`), 503 `RZ-RT-011`, 502 `RZ-RT-012`; property that `detail` is always one of the schema's keyword locations or a fixed string and never contains a body substring of 4+ bytes absent from the schema.
- T15 `CompileSchema`: `$ref` to `http://…`, `https://…`, `file:///etc/passwd` → RZ-CFG-005 with a spy loader proving no fetch or file open; `$schema` draft-07 at root and nested; `pattern: "(?=a)"` → RZ-CFG-005; 256 KiB + 1 → RZ-CFG-005; fan-out bomb (`anyOf` of 2 branches nested 11 deep) → RZ-CFG-005, 10 deep passes; `format: email` not asserted; internal `$defs` and `$id` refs resolve.
- T16 `configcheck` and configuration conformance fixtures (one negative fixture per code, file/line/column, supplied to TQ's configuration suite): RZ-CFG-005 (every req 21-23, 25, 39, 40, 75 rule), RZ-CFG-006 (unknown member in `transform.request` config), RZ-CFG-011 (`${X}` in `valueExpression` and `body`), RZ-CFG-013 warning (`headers` `authorization` literal), RZ-CFG-014 (`upstream` in a G-attached transform, `response` in a request op, `request.body` in `headers`, `response.body` in `transform.request`), RZ-CFG-015 (nested comprehension over `request.body`), RZ-CFG-018 (two `validation.json-schema` on one Route), RZ-CFG-020 (`validation.json-schema` at Gateway and at Upstream), proposed RZ-CFG-038 (`cache` plus `validation.json-schema`).

**Property tests** (`testing.F`, seeds in `pr-fast`, long runs in `nightly`; TQ "Unit and property tests")

- P1 Open-failure identity: for random messages and Programs forced to fail at a random step, the message is byte-identical after `Handle`.
- P2 Determinism: `Handle` twice on clones gives identical bytes; permuting input object member order gives identical output; `Decode(Append(t))` equals `t`.
- P3 Missing-path no-ops: `remove`, `move`, `delete`, `append` from a path absent in the document leave it equal.
- P4 Replace equivalence: literal mode equals `strings.ReplaceAll`; regex mode equals `regexp.ReplaceAllString` on the same input when under the cap.
- P5 Framing: after any rewrite `Content-Length == len(body)` and no `Content-Encoding`.
- P6 Protected fields: no compiled `headers` or `transform.*` Program changes a protected field (req 22).
- P7 Validation differential: for random JSON without duplicate names, the Filter accepts exactly when `sjs.UnmarshalJSON` plus `Schema.Validate` accepts, over the schema corpus.
- P8 Query editor: untouched pairs are byte-identical and in order; after `Set(k, v)` exactly one `k` remains, decoding to `v`.

**Fuzz targets** (Go native fuzzing, seeded from goldens; TQ "Fuzzing"; oracle: no panic, no hang, bounded memory)

- F1 `FuzzDotPathParse`; F2 `FuzzTransformApply` (random JSON × Programs from a small op grammar; result is success within the cap or one of the allowed error kinds); F3 `FuzzQueryEdit`; F4 `FuzzFieldValue` (validator agrees with a byte-table reference); F5 `FuzzJSONValDecode` (agrees with `encoding/json` on inputs without duplicates, rejects the rest with a typed error, cost monotone in size); F6 `FuzzCompileSchema` (never loads a URL, returns within 1 s); F7 `FuzzValidate` (corpus schemas × random bodies).

**Benchmarks** (TQ "Benchmarks and regression gates"; alloc/op gate 3%): `BenchmarkHeadersSetTwo` (U scope, literal and CEL values; target 2 µs p50 / 8 µs p99, 2 allocs), `BenchmarkValidate1KiB` (25/80 µs, 0 Ruralz-owned allocs), `BenchmarkTransformResponse1KiB` (the `orders-shape` Policy; 30/100 µs), worst cases at the caps (32 entries, 8 KiB values, `maxResponseBodyBytes` body), `BenchmarkCompileSchema256KiB`.

**Integration** (build tag `integration`, CI stage 8, no Docker): an in-process Node (area 4 handler and executor, area 5 Forwarder) on ephemeral ports with `httptest` Upstreams over HTTP/1.1, h2c prior knowledge and HTTP/2 over TLS (protocol conformance, TQ "Conformance suites"):

- I1 `headers-security` (`overridable: false`, `strict-transport-security`) present on 200, on a 401 from `auth.api-key`, on 503 `RZ-RT-011` and 502 `RZ-RT-012`; absent on pre-routing 404 `RZ-RT-001` (no chain; documented).
- I2 `headers-internal` at U on an `aggregate` Route with two legs and one retry: `x-shop-consumer` exactly once per attempt; `traceparent` present and not overridable; HTTP/2 names lowercased and connection-specific fields absent.
- I3 Forwarding: from an untrusted peer a client `X-Forwarded-For: 1.2.3.4` is replaced; a U-scope `remove` of `x-forwarded-for` (after registration; until then a `transform.request` `remove` `target: header`) removes it; a G-scope `set` of `x-forwarded-for` is overwritten at leg build.
- I4 Hop-by-hop: `Connection: x-foo` with `X-Foo` removed both ways; a Bundle setting `transfer-encoding` fails validation.
- I5 `transform.request` rewrites a gzip-encoded client body: the Upstream receives identity bytes with the right `Content-Length` and no `Content-Encoding`.
- I6 `transform.response` on a gzip Upstream response succeeds; on `br` a JSON op is 502 `RZ-RT-012` (`closed`) and passes unchanged (`open`).
- I7 `validation.json-schema` 400 happens before `transform.request` and the Upstream is never called; a valid body reaches the Upstream byte-identical.
- I8 Budget: `maxBufferedBytes` set to 64 KiB with concurrent 48 KiB transforms: some requests get 503 `RZ-RT-004`, none get `RZ-RT-011`, the budget returns to 0.
- I9 State Store independence: with the `redis` driver pointed at the local `redis-server` 7.0.15, stopped mid-test, these Policies behave identically.
- I10 Composition: a U-scope `transform.request` failing `closed` in one `aggregate` step fails the request per area 5 req 49 (code per section 9 item 14); with `optional: true` the response carries `ruralz-partial: true`.

**End-to-end** (CI stage 10, Docker Compose; not runnable locally): TQ scenario 2 `shop-bundle` asserts `headers-security` and `headers-internal` rows of `ruralz bundle render --effective --route orders-summary` and their effect on the wire.

**Every code path**: `RZ-RT-009` (T13, T14, I7), `RZ-RT-011` (T7, I1), `RZ-RT-012` (T7, I1, I6), `RZ-RT-004` (T8, I8), `RZ-RT-003` and `RZ-UP-010` (T8), `RZ-RT-015` (I10), RZ-CFG-005/006/011/013/014/015/018/020/038 (T15, T16).

## 7. Open questions blocking M1 in this area

| ID | Question | Adopt | What the code does |
|---|---|---|---|
| OQ-data-plane-9 (Blocking: "Yes, pack 8.6 amendment (M1)") | Which area covers Node-generated failures outside "before any Upstream" (RZ-RT-011 to RZ-RT-015)? | (a) Amend pack 8.6 `RT` to "request and response handling on a Node outside Upstream legs" (current) | Uses the registered `RZ-RT-011` (503) and `RZ-RT-012` (502) for request- and response-Phase failures of `headers`, `transform.*` and `validation.json-schema`, including U-scope Phases; no new code |
| OQ-traffic-management-and-resilience-21 (Blocking: "Yes, Response Cache (M1)") | How does `cache` coexist with `onRequestBody` authz, validation or Plugin auth? | (a) Reject with a new `RZ-CFG` code (proposed; area 2 names it RZ-CFG-038) | An effective chain holding `cache` and `validation.json-schema` fails validation; `transform.request` (transform class) is not covered by the rule, since a cache hit correctly skips it (section 9 item 19) |
| OQ-security-and-identity-21 (Blocking: "Yes, Planned (M1)") | Reject encoded NUL and backslashes? | (a) Yes (proposed) | Area 4 rejects them before routing; this area guarantees `transform.request` never rewrites the path (no `path` target exists), so SEC's "only a later `transform.request` rewrites it" has no M1 mechanism (section 9 item 16) |
| OQ-security-and-identity-6 (Blocking: "Yes, for `source.ip` users"; named in RM M1 exit criterion 7) | How are trusted proxies declared? | (c) Both, answered by CM "Gateway": `trustedProxies` and `listeners[].proxyProtocol` | Forwarding headers follow area 4 req 18 (req 34 here); `headers` Policies can edit them only at U scope |

Non-blocking questions this area's M1 rows still depend on (decide before coding):

| ID | Question | Adopt | What the code does |
|---|---|---|---|
| OQ-configuration-model-19 (No) | Which feature-authored `config` fields are registered next? | (a) Register, then close the config (proposed); plus register this spec's `validation.json-schema` `config.schema` and `headers` `add[]`/`remove[]` | Without `config.schema`, `validation.json-schema` cannot ship (FC row Planned (M1)); without `remove[]`, FC "Upstream forwarding control" and "Header manipulation" are partial (SM-3). Code is written against the proposed fields behind the registration change |
| OQ-feature-catalog-5 (No) | How does `headers` express an allowlist of client headers and query strings forwarded to an Upstream? | (a) Remove and allowlist fields in `headers` `config` (proposed) | M1 builds `remove[]`; an `allow` field is not built until registered (section 8) |
| OQ-feature-catalog-11 (No) | Is faster JSON decoding a catalog feature or a budget? | (a) Keep the row, met when budgets pass (proposed) | All decoding goes through `jsonval` (and area 3 `DecodeJSON`), the single swap point for M5 |
| OQ-data-plane-17 (Yes, for the SOAP row (M5)) | How does `transform.response` read XML? | (a) A `plugin` Policy (current) | Non-JSON bodies stay null; nothing XML-specific is built |
| OQ-observability-9 (No) | Trace ID response header? | (a) No (current) | No Node-added trace header; a `headers` response op may not set `traceparent` (protected) |

Checked and found not touching this area: the other M1-blocking rows (OQ-configuration-model-8, OQ-security-and-identity-1, -7, -9, -15, -22, -24, OQ-traffic-management-and-resilience-2, -5, -6, -11, -16, -19, -20, OQ-scalability-and-distributed-state-2, -3, -10, -11, OQ-observability-2, -16, OQ-data-plane-2, OQ-performance-budgets-and-benchmarking-1, -2, -6 (only its 100 µs yield, req 90), OQ-testing-and-quality-strategy-2, OQ-release-versioning-and-compatibility-3, OQ-repository-layout-and-conventions-6, OQ-zero-downtime-upgrades-and-hot-reload-7, OQ-cli-and-api-surface-10).

## 8. Deferred (M2+; do not build) and extension points

| Deferred item | Milestone | Extension point to leave |
|---|---|---|
| JSON Schema response validation (`validation.json-schema` in `onResponse`, OQ-feature-catalog-3) | M5 | `jsonschema.Filter.Handle` dispatches on Phase; the Factory's PhaseSet comes from config, so a future `response` schema field adds a Phase without new types |
| XML response reading (OQ-data-plane-17), SOAP envelopes, re-encoding (OQ-feature-catalog-2), response compression type | M5 | `jsonval.IsJSONMediaType` is the only JSON gate; body state is "document or raw bytes", so another decoder can plug in by media type |
| Prompt templates on `ai` Upstreams (`transform.request` `body`) | M3 | Nothing AI-specific; `transform.request` at G/R runs in `onRequestBody` before `onRoute` model selection |
| gRPC metadata, WebSocket and SSE, GraphQL, `onChunk` | M3 | Filters never subscribe `onChunk`; `httpfield.Protect` gains gRPC-reserved names (`grpc-*`) with M3; MP "Validation rules" forbid response-body gates on `websocket` Upstreams |
| Messaging (`validation.json-schema` and `transform.request` per message, MP "Topic ingress") | M4 | Filters read the message only through the Exchange, never `*http.Request` directly |
| `plugin` Policies writing headers | M2 | The Plugin host reuses `httpfield.ValidName`, `ValidValue`, `Protect` (WASM "Host Function rules") |
| `headers` allowlist (`allow`), secret-valued headers (`valueFrom: secretRef`) | Unregistered | Upstream credentials stay with `auth.upstream-*`; `op.kind` is an enum ready for `allow` |
| Path and host rewriting (`upstreams[].pathRewrite`, redirects, OQ-traffic-management-and-resilience-13) | M2 | No `path` target in `transform.request`; `host` protected |
| Signed transport-time hooks (`auth.upstream-sigv4`, OQ-security-and-identity-28) | M2 | Body rewrites finish before the transport; framing set once at commit |
| OpenAPI import and export of body schemas; CRD mirror of `config.schema` (`x-kubernetes-preserve-unknown-fields`) | M2 | `jsonschema.Schema.Raw`; `JSONSchemaDocument` named type for schemagen |
| Faster JSON decoder (OQ-feature-catalog-11) | M5 | `jsonval.Decode` behind one function |

## 9. Risks and ambiguities

1. **`headers` cannot remove or add in the registered schema.** CM fixes only `request.set[]`/`response.set[]`, while FC promises "setting, adding and removing" and "Upstream-scoped `headers` Policies remove client headers" (Planned (M1); SM-3 needs 100%). Resolution: register `add[]` and `remove[]` (req 20) through OQ-configuration-model-19 (a) before coding; interim workaround `transform.request` `remove` `target: header` (a gate, so costlier).
2. **`validation.json-schema` has no config fields** (`ValidationJSONSchemaConfig` is an empty open struct). Resolution: register `config.schema` (req 74); no alternative ships the FC row.
3. **`Replace.Path` is `+ruralz:required` in `pkg/config/v1alpha1`**, but DP says the raw body is used "when `path` is absent". Resolution: drop the marker (schema regenerated, CM owner review); otherwise path-less `replace` is impossible.
4. **`pattern` 1 KiB limit counts bytes; the schema `maxLength=1024` counts code points.** Resolution: keep both; the byte check (area 1) is authoritative.
5. **`HeaderRequestSet.Value string` with `omitempty`** cannot tell `value: ""` from absent, so an empty literal fails `exactlyOneOf` after canonicalization. Resolution: `*string` (req 23).
6. **`upstream` in `headers` request ops.** CM's CEL row gives `upstream` only "on response operations", yet `transform.request` at U gets it. Resolution: follow the registered row in M1 (RZ-CFG-014); propose adding `upstream` to U-scope request ops.
7. **Malformed client JSON under `transform.request` yields 503 `RZ-RT-011`** (a server-class status for a client fault) under `closed`. Resolution: keep DP semantics; recommend pairing with `validation.json-schema`, which runs first and answers 400; raise a DP question for a 400 mapping.
8. **`null` intermediates.** DP makes "a path through a non-object" *cannot decide*; JSON APIs often use `null` for absent objects, so `remove customer.email` on `"customer": null` fails `closed`. Resolution: M1 follows DP literally; propose treating `null` as missing for `remove`, `move`, `delete`, `append` sources and `replace`.
9. **`set[].valueExpression` result is `string`** even for `target: body`, so numbers and objects cannot be set. Resolution: M1 writes JSON strings; propose `dyn` for body targets (CEL row change).
10. **"`contentType` required for a string result"** is unenforceable because the default `application/json` materializes. Resolution: runtime uses the configured or default value; ask DP to reword as guidance.
11. **Both `transform.*` are always gates**, even for header- or query-only configs, adding buffering and latency. Resolution: M1 follows DP (area 2 already subscribes only when entries exist); propose "gate only when the body is read or written".
12. **Compressed bodies**: area 5 sets `DisableCompression` and forwards the client's `Accept-Encoding`, so Upstreams may answer `br` or `zstd`, which the stdlib cannot decode. Resolution: req 52 fails such JSON ops; propose that the Upstream layer restrict `Accept-Encoding` to `gzip, deflate` on legs whose chain holds a response gate.
13. **Stale validators after rewrite** (`ETag`, `Content-MD5`, digests) are not addressed by DP. Resolution: req 47(a) proposal; weakening keeps `If-None-Match` revalidation working (weak comparison).
14. **Composition code for Policy-generated step failures.** Area 5 req 49 lists only Upstream causes, `RZ-RT-015` and `RZ-RT-004`; a U-scope `transform.*`/`headers` `closed` failure produces a generated 503/502 that is none of these. Resolution: map to 502 `RZ-RT-015` (its meaning covers CEL runtime errors and oversize, the common causes); decide with area 5.
15. **Response-Phase replacement continuation** (req 17) and "no retry after a U response-Phase replacement" (req 16) are not in DP. Resolution: adopt as proposed; area 4 owns the executor.
16. **SEC "Path confusion" says "only a later `transform.request` rewrites it"**, but the registered schema has no path target. Resolution: ask SEC to reword ("nothing rewrites the path in M1; composition `path`/`pathExpression` set step paths").
17. **Protected names are unspecified for built-in Policies** (only the Plugin ABI lists them). Resolution: req 22 list, shared with the Plugin host; `host` protected until a host-rewrite field exists.
18. **`baggage`**: OBS says "Baggage is never propagated", ambiguous between "the Node never injects it" and "client `baggage` is stripped". Resolution: this area does not touch it; recommend the telemetry area strip client `baggage` toward Upstreams (trust boundary), else document pass-through.
19. **`cache` plus `transform.request`**: a hit skips `onRequestBody`, so the transform does not run; harmless (the stored response came from a transformed request) but responses are keyed without the body. Resolution: no rule; document.
20. **Allocation targets**: PB gives 0 Ruralz-owned allocations for `validation.json-schema` and `transform.response` on 1 KiB, which decoding into Go values cannot meet. Resolution: count decoder and library allocations outside "Ruralz-owned" (as PB does for `net/http` internals) and gate Ruralz code only; ask PB to state it.
21. **JSON Schema validation cannot be cancelled** (jsonschema v6 `Validate` takes no context). Resolution: the compile-time size and fan-out bounds (req 75) plus the body cap bound work; F6/F7 and `BenchmarkValidate` watch it.
22. **`$ref` to `file:` URLs**: jsonschema v6's default loader reads local files. Resolution: the refusing loader is mandatory (req 75); T15 proves it. Security-sensitive: add `internal/filter/validation/jsonschema` to the security-reviewed package list.
23. **Duplicate JSON names**: area 3 decodes "last wins" for CEL, while validation rejects them (req 80). A body with duplicates therefore fails validation but may reach `authz.cel` on Routes without validation. Resolution: acceptable split; recommend area 3 expose a duplicate flag so other body readers can refuse too.
24. **`limits.maxRequestBodyBytes` has no schema default** (CM proposes defaults only for response, steps and buffer budget), so the `transform.request` output cap and the 4× decoded cap are undefined when unset. Resolution: propose default 10 MiB (the CM example value); until then the Node treats unset as 10 MiB (proposed).
25. **`headers` response ops can set `Cache-Control` that the Ruralz Response Cache then honors** (transform runs before `cache` in reverse order). Resolution: intended for CDN headers; document the interaction.
26. **U-scope `headers` can overwrite the `Authorization` set by `auth.upstream-oauth2`** in the same leg (upstream-auth runs first). Resolution: document; optionally a validation warning later (no code exists for it).
27. **JSON handling exists in three areas** (area 3 CEL values, area 5 `OrderedObject` merge, this area's `jsonval`). Resolution: area 5 SHOULD reuse `jsonval` for merge; area 3 keeps its CEL-value decoder but MUST agree on media-type rules (`IsJSONMediaType`) and ordering (ascending keys).
28. **Environment substitution inside `config.schema`** rewrites `${…}` in schema strings. Resolution: document the `$${` escape (req 74); no special rule.
