# M1 spec, area 2: Canonical form, Revision digest, precedence, diff and render

Doc abbreviations used in references:
CM = `docs/architecture/02-configuration-model.md`; FP = `docs/_meta/foundation-pack.md`;
RVC = `docs/engineering/04-release-versioning-and-compatibility.md`; CLI = `docs/reference/01-cli-and-api-surface.md`;
RL = `docs/engineering/02-repository-layout-and-conventions.md`; TQ = `docs/engineering/03-testing-and-quality-strategy.md`;
TS = `docs/engineering/01-tech-stack-and-libraries.md`; RM = `docs/roadmap/01-roadmap-and-milestones.md`;
DP = `docs/architecture/03-data-plane.md`; SO = `docs/architecture/01-system-overview.md`;
SI = `docs/architecture/08-security-and-identity.md`; TMR = `docs/architecture/09-traffic-management-and-resilience.md`;
OBS = `docs/architecture/10-observability.md`; PBB = `docs/architecture/12-performance-budgets-and-benchmarking.md`;
ADR3 = `docs/adr/0003-configuration-format.md`.

## 1. Scope

Every M1 roadmap item in this area (RM § "M1 Core gateway", table "M1 scope" and "M1 exit criteria"):

| # | M1 item (RM wording) | Where it is specified |
|---|---|---|
| S1 | Configuration: "canonicalization, Revision digest" | CM § Canonical form and Revision; FP § 8.1 (Revision, Identifier rows); FP § 2 (Revision row); SO § Configuration lifecycle |
| S2 | Configuration: "golden corpus" | CM § Canonical form and Revision (third stability rule); RVC § One product version, § Versioned surfaces (`ruralz.canonical.v1` row); ADR3 § Confirmation ("Golden corpus"); TQ § Golden tests |
| S3 | CLI: `ruralz bundle render` "(`--api-version` conversion MUST yield the same Revision)", including `--effective --route` | CLI § Command table, § Bundle verbs in detail, § Output formats and exit codes; CM § Worked example (last paragraph), § Hub-and-spoke conversion; RVC § Migration tooling; FP § 9 |
| S4 | CLI: `ruralz bundle diff` "from directory, file or admin URL" | CM § Diff semantics; CLI § Bundle verbs in detail (source table), § Diff JSON compatibility, § Exit codes; FP § 9 |
| S5 | CLI: `ruralz bundle build` | CLI § Command table, § Output formats and exit codes; CM § Validation and diff semantics; FP § 9 |
| S6 | Traffic and state: "API governance (`quota`, `overridable: false` Gateway Policies)" (the precedence and guardrail half) | CM § Attachment and precedence (Resolution rules, Worked example); FP § 8.12, § 10 |
| S7 | Security: "digest checks on every Revision" (Revision half; Plugin artifacts are M2) | FP § 8.14 (first row), § 8.1 (Identifier row); DP § Activation step 1; SO § Compile before swap |
| S8 | Quality: "Property tests", "round-trip test", "fuzzing", end-to-end "`--effective --route`, `ruralz bundle diff` against Node `/config/dump`" | TQ § Required properties, § Fuzzing ("Canonicalization"), § End-to-end tests (scenarios 2 and 6); RM M1 scope Quality row |
| S9 | Exit criterion 5: "The golden corpus is byte-exact"; "every M1 command in pack section 9 has an end-to-end test" (render, diff, build) | RM § M1 exit criteria |
| S10 | Exit criterion 7: close the M1-blocking Open questions touching this area (section 7 below), explicitly OQ-configuration-model-8 | RM § M1 exit criteria |
| S11 | Hub types and conversion scaffolding; schema levels; RZ-CFG-024, RZ-CFG-025, RZ-CFG-027 | CM § apiVersion versioning and migration, § Hub-and-spoke conversion, § Version skew, § Error codes; RVC § apiVersion lifecycle and deprecation windows, § Control plane and data plane version skew (LKG and file-mode rows) |
| S12 | Schema-driven defaults and the overridable:false golden test | RL § Presence and defaults; CM § Policy (registry defaults paragraph) |
| S13 | Data served by admin `/config/dump` and consumed by `ruralz node dump` (format owned here, handler owned by Data plane) | DP § Admin endpoints; CLI § Ruralz Gateway admin API |

## 2. Normative requirements

Levels: MUST/SHOULD as stated. "(target)" and "(hypothesis)" are copied from the docs; "(proposed)" marks a value this spec proposes where the docs are silent (see section 9). RZ-CFG codes carry no HTTP status (internal/errcode `Status` is 0 for every CFG code); CLI surfaces map them to exit codes.

### 2.1 Pipeline placement

1. The area owns pipeline stages G "Materialize defaults and convert to hub form", I "Compute effective Filter Chains, slots, guardrails", L "Canonicalize as ruralz.canonical.v1" and M "Revision sha256 digest, displayed as rev-12hex" of CM § Validation and diff semantics, Figure 3. Stages A–F, H, J, K belong to other areas and call into this area's packages through the pipeline facade (section 4).
2. Stages up to the digest MUST run offline and report every error in one run (CM § Validation and diff semantics). Stage G MUST run on every resource that passed stage F; stage I on every Route whose references resolved; a digest MUST be produced only when the diagnostic list holds no `error` severity.
3. The same library code MUST run in `ruralz`, `ruralz-control` and `ruralzd` (RL § Where Go code goes), so a Bundle's digest is identical wherever computed.

### 2.2 Hub representation and conversion (stage G, part 1)

4. Every resource MUST be decoded under its own apiVersion's rendered schema, then converted to its hub representation; a Bundle MAY mix apiVersions (CM § Hub-and-spoke conversion). M1 serves only `ruralz/v1alpha1`; an unknown or unserved apiVersion is RZ-CFG-007 (CM § Error codes; RVC § apiVersion lifecycle).
5. The hub is internal (`internal/config/hub`, `internal/config/convert`, RL § Schema generation from Go types). In M1 the hub field set equals `ruralz/v1alpha1` exactly and `convert` registers one identity converter; conversion MUST still be invoked through the converter registry so `ruralz/v1beta1` (Planned (M3)) plugs in without touching callers.
6. Conversion MUST operate on generic value trees (not only typed structs) and accept partial documents (overlay patches with `$patch` entries and unsubstituted `${VAR}` strings), because `--api-version` rewrites source files "unmerged and unsubstituted" (CLI § Bundle verbs in detail).
7. Newer-only fields MUST survive down-conversion in annotation `ruralz.io/conversion-data` and be restored on up-conversion (CM § Hub-and-spoke conversion). That annotation MUST NOT appear in the hub or canonical form. In M1 the constant exists and the strip step runs; no converter populates it.
8. The converter writes a field explicitly whenever the source version's default differs from the target's (CM § Hub-and-spoke conversion). Converting a Bundle MUST yield the same Revision and an empty `ruralz bundle diff` (CM; RVC § Migration tooling). Round-trip property tests run from M1; golden-corpus conversion from M3 (RVC § Migration tooling).

### 2.3 Defaults (stage G, part 2)

9. Schema defaults MUST come only from the embedded rendered schema's `default` keywords (`api/schema.RenderedV1alpha1()`), never from Go zero values or `omitempty` (RL § Presence and defaults). They apply only to absent properties of objects that are present; an absent optional object MUST NOT be created (section 9, R-2). Defaults apply inside list entries (for example each `Route.spec.upstreams[].weight` defaults to 1).
10. Current schema defaults (generated, `api/schema/ruralz/v1alpha1/rendered.schema.json`): `Admin.port` 9901; `AuthUpstreamOAuth2Config.timeout` "2s"; `AuthUpstreamSigV4Config.payload` "signed"; `BasicCredential.iterations` 600000; `CompositionStep.collection` false; `CompositionStep.optional` false; `Limits.maxBufferedBytes` 536870912; `Limits.maxCompositionSteps` 16; `Limits.maxPluginMemoryBytes` 2147483648; `Limits.maxResponseBodyBytes` 10485760; `Listener.proxyProtocol` false; `ListenerTLS.minVersion` "1.3"; `PluginLimits.memoryBytes` 16777216; `PluginLimits.timeout` "5ms"; `PolicySpec.overridable` true; `QuotaConfig.key` "consumer.name"; `RateLimitConfig.localOnly` false; `RouteUpstream.weight` 1; `StateStore.driver` "memory"; `StateStore.topology` "standalone"; `TransformRequestConfig.contentType` and `TransformResponseConfig.contentType` "application/json". The mechanism MUST be table-free: adding a `+ruralz:default` marker (for example the OQ-traffic-management-and-resilience-6 values, section 7) needs no code change here.
11. Policy `config` defaults MUST be resolved through the `PolicySpec` `allOf`/`if`/`then` dispatch on `spec.type` (the `then.properties.config.$ref` of the matching `const`). `plugin` Policy `config` gets no defaults from `Plugin.spec.configSchema` in `ruralz.canonical.v1` (proposed, section 9, R-9).
12. Registry defaults MUST be materialized into every `Policy.spec` like schema defaults (CM § Policy; FP § 8.1): `slot` (registry slot, or the Policy's own `metadata.name` for types whose slot is `name`), `failureMode` (registry default), `filterClass` (for `type: plugin` the default `custom`; for every other type the registry Filter class, per FP § 8.1's binding list; see section 9, R-4). An explicitly authored value is kept.
13. Registry (FP § 10, CM § Policy), exact strings:

| `type` | Filter class | Base Phases (G/R; U) | Scopes | Slot | failureMode default; allowed |
|---|---|---|---|---|---|
| `auth.jwt`, `auth.api-key`, `auth.basic`, `auth.mtls` | auth | onRequestHeaders | G, R | `auth` | closed; closed only |
| `authz.cel` | authz | onRequestHeaders or onRequestBody (R-13a) | G, R | name | closed; closed only |
| `authz.opa`, `authz.cedar` | authz | onRequestHeaders (M1 display; body analysis M2) | G, R | name | closed; closed only |
| `authz.ip`, `authz.geoip` | authz | onRequestHeaders | G, R | name | closed; closed only |
| `ratelimit` | admission | onRequestHeaders | G, R | name | open; either |
| `quota` | admission | onRequestHeaders, onLog | G, R | name | open; either (OQ-configuration-model-8 (a)) |
| `validation.json-schema` | validation | onRequestBody | R | `validation` | closed; either |
| `cors` | cors | onRequestHeaders, onResponse | G, R | `cors` | closed; either |
| `cache` | cache | onRequestHeaders, onResponse | R | `cache` | open; either |
| `headers` | transform | onRequestHeaders if `config.request.set` non-empty, onResponse if `config.response.set` non-empty; U: onUpstreamRequest / onUpstreamResponseHeaders by the same rule | G, R, U | name | closed; either |
| `transform.request` | transform | onRequestBody; U: onUpstreamRequest (only if `config` sets `body` or any list entry) | G, R, U | name | closed; either |
| `transform.response` | transform | onResponse; U: onUpstreamResponseBody (same rule) | G, R, U | name | closed; either |
| `auth.upstream-oauth2`, `auth.upstream-sigv4` | upstream-auth | U: onUpstreamRequest | U | `upstream-auth` | closed; closed only |
| `ai.token-budget` | admission | onRequestBody, onChunk, onLog | G, R | name | closed; either (OQ-configuration-model-8 (a)) |
| `ai.semantic-cache` | cache | onRequestBody, onResponse | R | `semantic-cache` | open; either |
| `ai.guardrail` | validation | onRequestBody, onChunk, onResponse | G, R | name | closed; either |
| `plugin` | `spec.filterClass` (default custom) | `Plugin.spec.phases` | Scopes whose Phases include every Plugin Phase | name | closed; closed only when filterClass is auth or authz |

   13a. `authz.cel` runs in onRequestBody when its `config.rule` references `request.body` (decided by the CEL area's reference oracle), else in onRequestHeaders, never both (proposed; section 9, R-13). `headers` and `transform.*` run "only in Phases their config uses" (OBS § Span model; CM § Worked example).
14. Scope-to-Phase rule: client-leg Phases are onRequestHeaders, onRequestBody, onRoute, onResponse, onLog, onChunk; upstream-leg Phases are onUpstreamRequest, onUpstreamResponseHeaders, onUpstreamResponseBody, onChunk (CM § Resolution rules rule 4). A `plugin` Policy whose Plugin lists any Phase outside its attachment scope's set is RZ-CFG-020 (proposed strict reading, section 9, R-14).

### 2.4 Normalization (stage G, part 3)

15. Scalars MUST be normalized schema-driven by definition name in the rendered schema: `$defs/Duration` values become `time.ParseDuration(v).String()` (`90s` → `1m30s`, `0.5s` → `500ms`, `0` → `0s`); `$defs/ByteSize` values become an integer number of bytes via `v1alpha1.ParseByteSize` (`10Mi` → `10485760`, `1k` → `1000`); `$defs/Decimal` strings become the shortest equal decimal (strip leading zeros of the integer part keeping one `0`, strip trailing fraction zeros and a bare `.`: `"3.00"` → `"3"`, `"0.30"` → `"0.3"`) (CM § Schema keywords that drive tooling: "equivalent spellings never diff"); `$defs/IntOrString` keeps its JSON type. A duration that overflows `time.Duration` is RZ-CFG-005.
16. Numbers are carried as literals (`json.Number`), never through `float64` before serialization. An integer literal (no `.`, `e` or `E`) outside ±(2^53−1) = ±9007199254740991 MUST be RZ-CFG-005 ("integer outside the I-JSON range"), because RFC 8785 serializes numbers as IEEE 754 doubles; a non-finite number is RZ-CFG-005 (proposed; section 9, R-8).
17. Lists MUST be normalized by their `x-ruralz-list` keyword (CM § Schema keywords that drive tooling; FP § 8.1):
    - `set`: sorted ascending by the bytes of each element's RFC 8785 encoding (bytewise); a duplicate element is RZ-CFG-005 with path `[item=<value>]` (proposed).
    - `map`: sorted ascending by the key field's string value (Go string comparison, UTF-8 bytewise); a missing or duplicate key is RZ-CFG-005 with path `[<key>=<value>]`.
    - `orderedMap` (`spec.policies` on Gateway, Route and Upstream; `AIModel.spec.candidates`; `Route.spec.composition.steps`): authored order kept; duplicate or missing key is RZ-CFG-005.
    - `atomic`: authored order kept, no checks.
    - Lists inside open objects, `Plugin.spec.configSchema` and `plugin` Policy `config` have no keyword and are treated as `atomic`.
18. Unknown members of `+ruralz:open` config objects (OQ-configuration-model-19) MUST be preserved and normalized generically (keys sorted by RFC 8785, numbers per R-16, lists atomic).
19. Presence MUST be preserved: an absent field stays absent, a present empty object or list stays present, an empty string stays `""`. JSON `null` never reaches the hub (an overlay `null` removes the field before defaults, CM § Overlays); a `null` in the hub is an internal error.
20. Normalization MUST be idempotent: normalizing a normalized tree is a no-op, and applying stage G to a decoded canonical document changes nothing (property test, section 6).

### 2.5 `ruralz.canonical.v1` serialization (stage L)

21. The Revision digest covers `ruralz.canonical.v1`, "never an internal Go type" (CM § Canonical form and Revision). The document is exactly one JSON object, serialized per RFC 8785 (JCS), with members `format` and `resources` only in M1:
    ```json
    {"format":"ruralz.canonical.v1","resources":[{"kind":"Gateway","metadata":{"labels":{"team":"platform"},"name":"edge"},"spec":{...}}, ...]}
    ```
    Each resource object has members `kind`, `metadata` (`name`, `labels` if present, `annotations` if present, minus `ruralz.io/conversion-data`) and `spec` (hub tree). It carries no `apiVersion`, no source file name, no comments, no `Environment` name and no signature, so a converted Bundle keeps its digest (CM § Hub-and-spoke conversion) and Environment/Cluster metadata stays outside the digest (CM § Canonical form and Revision last paragraph). (Envelope shape proposed; section 9, R-1.)
22. Resources MUST be sorted by kind in the order `Gateway`, `Upstream`, `Plugin`, `Policy`, `Consumer`, `AIProvider`, `AIModel`, `Route`, then by `metadata.name` bytewise (CM § Canonical form and Revision). `Environment` and `Cluster` never appear (RZ-CFG-017 upstream).
23. A field whose schema carries `x-ruralz-since` > 0 MUST be written only when its value differs from its `default` (compared by canonical bytes); without a `default` it is written only when present (CM § Canonical form and Revision rule 1; RL § Presence and defaults). Fields at level 0 (no keyword) are always written when present. In M1 no field has `x-ruralz-since`; the mechanism MUST exist and be tested with a synthetic schema.
24. RFC 8785 rules, implemented in own code (section 5):
    - Object members sorted by the UTF-16 code units of their names (RFC 8785 § 3.2.3), no whitespace anywhere, no duplicate names.
    - Strings: UTF-8 output; escape only `"` (`\"`), `\` (`\\`) and U+0000–U+001F as `\b`, `\t`, `\n`, `\f`, `\r` or `\u00xx` with lowercase hex; everything else literal, including `/`, U+007F, U+2028, U+2029 and non-BMP characters. Invalid UTF-8 is an internal error (the loader already rejected it as RZ-CFG-001).
    - Numbers: ECMAScript `Number.prototype.toString` of the double: `0` for ±0; `'f'`-style shortest round-trip digits when 1e-6 ≤ |x| < 1e21; else shortest `'e'` form with the exponent written without leading zeros and with an explicit sign (`1e+21`, `1e-7`); `1.0` → `1`.
    - Literals `true`, `false`; `null` never emitted.
25. The serialized bytes MUST NOT pass through `encoding/json` with HTML escaping enabled (it rewrites `<`, `>`, `&`, which appear in CEL such as `response.status >= 400`) or through any re-indenting writer.
26. Canonicalization parse, canonicalize, parse again MUST yield identical bytes (TQ § Fuzzing, "Canonicalization"; RM round-trip test).
27. The document has no trailing newline; `ruralz bundle build --output-file` writes exactly these bytes, so `sha256sum FILE` prints the digest's hex (proposed).
28. Changing the serialization requires a new format identifier such as `ruralz.canonical.v2` (CM rule 3; RVC § One product version). Every digest-changing change in `ruralz/v1alpha1` ships as a new schema level (RVC § One product version). The encoder MUST accept an option naming the schema level to render at; M1 accepts only the binary's own level (0) and rejects others with an internal error (extension point for OQ-release-versioning-and-compatibility-12, M2).

### 2.6 Revision digest and verification (stage M)

29. The digest is SHA-256 over the exact canonical bytes. Wire and storage form `sha256:<64 lowercase hex>`; display form `rev-<first 12 hex>` (for example `rev-162af81f5de4`) (FP § 2 Revision row, § 8.1 Identifier row). The display form appears only in CLI human output, logs and prose; JSON output, `/config/dump`, Last-Known-Good and every verification carry and check the full digest (CM § Canonical form and Revision; CLI § Output formats and exit codes).
30. Digest parsing accepts only `sha256:` followed by exactly 64 lowercase hex characters; anything else (uppercase hex, other algorithms, 12-hex prefixes) is a usage error in CLI contexts and an internal error elsewhere. `rev-<12 lowercase hex>` parses only as a display prefix (for M2 lookups, CLI § Bundle verbs in detail).
31. Verification MUST check, in order: (a) `sha256(content)` equals the expected digest, else RZ-CFG-027 "content hashes to sha256:X, want sha256:Y"; (b) content re-encodes to identical bytes (it is in `ruralz.canonical.v1` form), else RZ-CFG-027 "content is not in ruralz.canonical.v1 form" (FP § 8.1; FP § 8.14 row 1 "Always on; not configurable"; CM § Error codes RZ-CFG-027).
32. M1 call sites of verification: Last-Known-Good boot and Zero-Downtime Upgrade handover candidates (DP § Activation step 1, SO § Compile before swap step 1, FP § 8.2 Boot order (1)); the CLI reading a `/config/dump` document or a saved `ruralz node dump` file (R-58). A Node that rejects a Revision increments `ruralz_config_activations_total{result="rejected",code="RZ-CFG-027"}` and keeps its active snapshot (OBS metric table; DP § Activation). Signature checks (RZ-CFG-033), OCI pulls and Control Stream deltas are M2.
33. Decoding canonical content (Last-Known-Good, handover candidate, dump, a build output file used as a diff source) MUST reject content from a newer producer with RZ-CFG-024: a top-level member other than `format` and `resources`; a `format` other than `ruralz.canonical.v1`; an unknown `kind`; a field unknown to the hub schema at this binary's level or whose `x-ruralz-since` exceeds it (RVC § Control plane and data plane version skew, rows "Last-Known-Good and Node" and "CLI and Nodes": "File-mode Node: RZ-CFG-024"; CM § Version skew). Authored YAML with an unknown field stays RZ-CFG-006 (section 9, R-11). Decoding MUST re-apply stage G so omitted since>0 fields regain their defaults.
34. Equal digests mean equal effective behavior on every Node that accepts them; only `secretRef` values and `RURALZ_*` process settings differ (FP § 8.1). `secretRef` references (provider, name, key) are part of the canonical form; resolved values never are (CM § Secrets and environment variables; SI § Secrets rule 2).

### 2.7 Precedence and the effective Filter Chain (stage I)

35. Each Route's effective Filter Chain MUST be computed at validation time, never per request (FP § 8.12), from hub resources after stage G, as:
    1. Take the Gateway's `spec.policies` minus the Route's `spec.excludePolicies`. Excluding a Gateway Policy whose `overridable` is `false` is RZ-CFG-019 at path `spec.excludePolicies[name=<p>]` of the Route, message `cannot exclude a Policy with overridable: false (declared in <file>:<line>:<column>)` (CM § Diagnostics and source map example).
    2. Add the Route's `spec.policies`. A Route Policy in the same `slot` as a remaining Gateway Policy replaces it; other slots stack. If that Gateway Policy has `overridable: false`, RZ-CFG-019 at `spec.policies[name=<r>]`, message `cannot replace Gateway Policy "<g>" in slot "<slot>": it has overridable: false (declared in <file>:<line>:<column>)` (proposed wording). A Route attaching the same Policy the Gateway already attaches is a replacement in its slot (section 9, R-16).
    3. One Policy per slot per scope: two entries of one `spec.policies` list with equal slots are RZ-CFG-018 at the second entry, message `Policies "<a>" and "<b>" share slot "<slot>" at <Gateway|Route|Upstream> scope`. Applied to the Gateway list once, each Route list, and each Upstream list (section 9, R-15).
    4. Upstream-scoped Policies form one set per Upstream, run only in upstream-leg Phases and take no part in slot comparison. A Policy type at a disallowed scope is RZ-CFG-020 at the attaching list entry, message `Policy type "<type>" is not allowed at <scope> scope (allowed: <scopes>)`, or for plugins `Plugin "<p>" Phases [<phases>] are not allowed at <scope> scope`.
    5. Order within a Phase: Filter class (`cors`, `auth`, `authz`, `admission`, `validation`, `cache`, `upstream-auth`, `transform`, `custom`), then scope (Gateway, Route, Upstream), then zero-based position in that scope's `spec.policies`. Response Phases (onUpstreamResponseHeaders, onUpstreamResponseBody, onResponse) run in exact reverse of that order; onLog keeps request order (FP § 8.12 rule 5; CM § Resolution rules rule 5; DP § Ordering). onChunk is ordered as a response Phase in M1 (proposed; section 9, R-12).
36. `overridable` only has effect on Gateway-attached Policies (CM § Policy sketch comment) but is always materialized (R-12, golden test R-79).
37. `failureMode: open` on `auth.*` (including `auth.upstream-oauth2`, `auth.upstream-sigv4`), `authz.*`, or a `plugin` Policy with `filterClass` `auth` or `authz` is RZ-CFG-029 at `spec.failureMode`, message `failureMode open is not allowed for Policy type "<type>" (closed only)` (FP § 8.10; CM § Policy). Checked once per Policy, whether attached or not.
38. `filterClass` authored on a non-`plugin` type with a value other than the registry class is RZ-CFG-005 (proposed; section 9, R-4). `filterClass` on `plugin` must be one of the nine classes (schema enum).
39. Guardrail for OQ-traffic-management-and-resilience-21 (a): an effective client chain holding a `cache` Policy and any Policy that runs in onRequestBody with Filter class `authz` or `validation`, or a `plugin` Policy with `filterClass` `auth` or `authz` in onRequestBody, is a validation error with a new code, proposed RZ-CFG-038 "Response Cache combined with an onRequestBody authorization or validation Policy", at the Route's path of the `cache` attachment (TMR § Response caching: "validation rejects an effective Filter Chain combining `cache` with an `onRequestBody` authz, validation, or `plugin` auth or authz Policy"). The code MUST be registered in CM § Error codes and `internal/errcode` before any literal lands (repocheck fails on unregistered literals, RL § Ruralz-specific checks).
40. References that failed resolution (RZ-CFG-009, owned by the reference stage) are skipped here without further diagnostics; precedence continues for all other Routes, so every error is reported in one run.
41. Leg order of a Route: order of first appearance of each Upstream name in `spec.upstreams` (after map sorting) or `spec.composition.steps` (authored order); each Upstream appears once.
42. The Data plane Filter Chain executor MUST build its per-Phase arrays from this area's chain value, so `render --effective` and runtime order cannot diverge (DP § Filter Chain execution; TQ § End-to-end scenario 2; OBS § Span model "Children follow pack 8.12 order").

### 2.8 `ruralz bundle render`

43. `ruralz bundle render [DIR] [--env NAME] [--environments FILE] [--output yaml|json] [--output-file PATH]` renders one Environment into "one YAML stream that is itself a valid one-file Bundle" (CM § Overlays; FP § 8.1 Rendered Bundle row). `DIR` defaults to `.`. `--output` defaults to `yaml`.
44. Rendered content = every Bundle resource after overlay merge and substitution, in its source apiVersion, authored values only: no schema or registry default is materialized (section 9, R-5), comments dropped. Resources in canonical kind order then name; within a resource, keys `apiVersion`, `kind`, `metadata` (`name`, `labels`, `annotations`), `spec`; spec keys in the source tree's key order (overlay-added keys appended); lists in authored order. YAML documents separated by `---\n`, no leading separator, output ends with a newline (proposed).
45. Strings MUST be emitted so the YAML 1.2 core resolution of ADR3 § Decision outcome (Libraries row) reads them back as strings: double-quoted with JSON escapes unless the value matches `^[A-Za-z_/][A-Za-z0-9_./@-]*$` and is not `null`, `Null`, `NULL`, `true`, `True`, `TRUE`, `false`, `False`, `FALSE` (proposed). So `"1.3"`, `"0777"`, `"yes"` stay strings on re-parse.
46. Render MUST be idempotent: a literal `${` that came from a `$${` escape is written back as `$${`, only in positions where substitution runs (not in keys, `apiVersion`, `kind`, `metadata.name`, `x-ruralz-ref`, `x-ruralz-secret` or `x-ruralz-cel` fields) (CM § Overlays, § Environment substitution). Rendering the output again MUST yield the same Revision (TQ § Required properties, Render) and byte-identical output (proposed).
47. `--output json` writes a JSON array of the same resource objects, two-space indented, HTML escaping off, trailing newline (proposed; section 9, R-19).
48. With `--environments` and no `--env`, render exits 2 (CLI § Global flags: "Other Bundle readers exit 2 there"), except with `--api-version` (R-53).
49. Exit codes (CLI § Exit codes): 0 success (warnings included); 1 invalid Bundle (any `error` diagnostic, printed to stderr as `file:line:column severity code Kind/name path: message`); 2 usage error or unreadable input. Data to stdout, diagnostics and warnings to stderr.

### 2.9 `render --effective --route NAME`

50. `--effective` requires `--route NAME` (exit 2 without it); `--route` without `--effective` exits 2; an unknown Route name exits 2 with `ruralz bundle render: Route "<name>" not found`. The Bundle is fully validated first (exit 1 on errors).
51. Text output (default with `--effective`) is a table with header `PHASE  LEG  POLICY  FROM  REASON` (columns: Phase, Leg, Policy, From, Reason, as CM § Worked example), aligned with `text/tabwriter` (minwidth 0, tabwidth 8, padding 2, padchar space). Rows: Phases in the fixed order onRequestHeaders, onRequestBody, onRoute, onUpstreamRequest, onUpstreamResponseHeaders, onUpstreamResponseBody, onResponse, onLog, onChunk; within a Phase the client leg first, then legs in R-41 order; within a leg the execution order of R-35.5; then removed Policies with Phase `none` and Leg `none`, replaced before excluded, each in Gateway list order.
    - Leg: `client` or the Upstream name. From: `Gateway`, `Route` or `Upstream <name>`.
    - Reason templates (proposed; golden-pinned; section 9, R-17): Upstream scope `leg <upstream> only`; Gateway, slot equals own name: `own slot` plus `, not overridable` when `overridable: false`; Gateway, other slot: `slot <slot>, inherited` plus the same suffix; Route replacing Gateway Policy g: `slot <slot>, replaces <g>`; Route, own slot: `own slot`; Route, other slot, nothing replaced: `slot <slot>`; removed by replacement: `replaced in slot <slot> by <r>`; removed by exclusion: `excluded by Route`.
52. `--output json` prints `{"route","revision","digest","environment","rows":[{"phase","leg","policy","type","filterClass","slot","from","reason"}]}` (member order as listed; `environment` omitted without `--env`; proposed). For the example Bundle and `orders-summary` the rows MUST equal CM § Worked example in Phase, Leg, Policy and From (golden, R-79).

### 2.10 `render --api-version` (conversion)

53. `ruralz bundle render --api-version VERSION --output-dir DIR --environments FILE [DIR_SRC]` (CLI § Bundle verbs in detail; CM § Hub-and-spoke conversion; OQ-configuration-model-7 (a), decided by CLI):
    - VERSION must be served; otherwise RZ-CFG-007, exit 2. `--output-dir` and `--environments` are required (exit 2 without); `--effective` with `--api-version` exits 2; `--output-dir` without `--api-version` exits 2.
    - DIR must not exist or be empty (exit 2 otherwise, proposed, so nothing is clobbered).
    - Every discovered base and overlay file is rewritten under VERSION into DIR at the same relative path, keeping `overlays/<env>/`, `$patch` entries and `${VAR}` / `$${` text, unmerged and unsubstituted; `.ruralzignore` is copied; nothing else is copied; comments are lost; `.json` files stay JSON (two-space indent), `.yaml`/`.yml` stay YAML (R-44/R-45 emitter); multi-document files keep document order; key order preserved for fields that survive conversion (proposed).
    - Then, for every Environment in FILE, both trees are rendered and their Revisions compared; stdout gets one line per Environment, `environment <name>: <rev-src> -> <rev-dst> equal` or `... differ`; for a difference the human diff goes to stderr. Exit 0 when every pair is equal, 1 otherwise ("`--api-version` conversion that changed a Revision", CLI § Exit codes), 2 on usage or I/O errors.
54. In M1 the only served target is `ruralz/v1alpha1` (identity); the command MUST still run the full rewrite-and-compare path so it is exercised end to end.

### 2.11 `ruralz bundle build`

55. `ruralz bundle build [DIR] [--env NAME] [--environments FILE] [--offline] [--output text|json] [--output-file PATH]` produces the Revision (CLI § Command table). Text output: one line `rev-<12 hex> sha256:<64 hex>` (proposed layout of "rev-<12 hex> and the full digest"). JSON output: `{"digest":"sha256:…","revision":"rev-…","environment":"<name>","diagnostics":[…]}` (CLI § Output formats; `environment` omitted without `--env`; `diagnostics` is `[]` when clean, warnings included). `--output-file` writes the canonical bytes (R-27).
56. Without `--offline`, build runs the online Plugin stage (CM § Validation and diff semantics). M1 has no OCI client: a Bundle with no `Plugin` resources passes the stage trivially; each `Plugin` otherwise yields RZ-CFG-028 "Plugin artifact unavailable: online Plugin check is Planned (M2); use --offline" (proposed; section 9, R-20). The online stage never changes the digest.
57. Exit codes: 0 Revision produced; 1 invalid Bundle (including RZ-CFG-028); 2 usage or unreadable input; `--environments` without `--env` exits 2.

### 2.12 `ruralz bundle diff`

58. `ruralz bundle diff FROM TO [--env NAME] [--environments FILE] [--from-env NAME] [--to-env NAME] [--output text|json] [--admin-token-file PATH] [--ca-file PATH] [--client-cert PATH --client-key PATH]` compares FROM and TO in that order (CM § Diff semantics; CLI § Command table, § Global flags). Each side resolves to a canonical document (CLI § Bundle verbs in detail):
    - Directory: rendered as `render` would, with `--from-env`/`--to-env` overriding `--env` per side.
    - File: sniffed by content. A JSON object whose `format` is `ruralz.canonical.v1` is canonical content (verified by re-encoding, R-31b, and its digest computed); a JSON object with members `content`, `digest`, `revision` is a `/config/dump` document (R-73; verified, R-31); anything else is a rendered one-file Bundle loaded by the loader (YAML or JSON) and rendered with the side's Environment.
    - Admin URL (`http://` or `https://` prefix): `GET <URL>/config/dump` with `Authorization: Bearer <token>` from `--admin-token-file`, or the client certificate; `--ca-file` adds trust anchors. A token is never sent over `http://` to a non-loopback host (exit 2) (CLI § Admin API). Request timeout 30 s, response capped at 256 MiB (both proposed). Non-200 prints the RFC 9457 problem document to stderr and exits 2.
    - `rev-<12 hex>`, `sha256:<64 hex>`, `oci://…`: Planned (M2); M1 exits 2 with `ruralz bundle diff: Revision sources are Planned (M2)`.
59. Rules (CM § Diff semantics): resources match by identity `(kind, metadata.name)`, renames are never inferred; comparison uses the canonical form (decoded, with stage G re-applied, R-33); `map` lists compare by key and `set` lists by element, so reordering is never a change; `orderedMap` lists compare by key and report `move` (R-60); `atomic` lists report one `replace` of the whole list; `SecretValue` fields show only the reference; a change of `Policy.spec.type` reports `replace` of `spec.type` and of the whole `spec.config`.
60. Move rule (proposed; section 9, R-7): among keys present on both sides, an entry is reported as moved when its index in the common-key subsequence differs between sides; the op carries its absolute zero-based positions in the FROM and TO lists (JSON) and one-based positions in the human form. Pure insertions or removals move nothing; swapping two entries reports two moves (matches CM example).
61. Op order within a resource (proposed): depth-first over the TO tree with object members in RFC 8785 order; within a keyed list, entries in TO order (recurse, then `move`), then FROM-only entries as `remove` in FROM order; `add` ops at their TO position; `set` adds in TO order, removes in FROM order.
62. Resource order (both forms): modified, then added, then removed; within a group by kind in catalog order `Gateway`, `Route`, `Upstream`, `Policy`, `Plugin`, `Consumer`, `AIProvider`, `AIModel` (the `v1alpha1.Kind` constant order), then name (proposed; reproduces the CM example's order).
63. `source` = the resource's base file (slash-separated, relative to the Bundle root; overlay file if the overlay added the resource) from the TO side when it is a directory or rendered file, else from FROM; omitted when neither side has a source map (canonical, dump or admin sides) (proposed).
64. Impact classes (`routing`, `security`, `traffic`, `plugin`, `ai`, `metadata`; CM § Schema keywords) of an op = union of: every `x-ruralz-impact` on the schema nodes from the resource root to the changed node; the Policy type impact for any op on a `Policy` (R-65); the referenced Policy's type impact for `add`/`remove`/`move` of a `spec.policies` or `spec.excludePolicies` entry (TO side, else FROM); `security` for any op on or under an `x-ruralz-secret` node and for secret destinations (R-66) (SI § Secrets rule 7, OQ-security-and-identity-22 (a)); `metadata` for `metadata.labels`/`metadata.annotations`; and, if still empty, the kind default: Gateway, Route, Upstream `routing`; Plugin `plugin`; Consumer `security`; AIProvider, AIModel `ai`. Added/removed resources carry the kind default (Policy: type impact; Plugin: `plugin`, `security`). A resource's `impact` is the sorted union over its ops; `summary.impact` the sorted union over resources.
65. Policy type impact (proposed registry column): `auth.*`, `authz.*`, `cors`, `validation.json-schema`, `auth.upstream-oauth2`, `auth.upstream-sigv4` → `security`; `ratelimit`, `quota`, `cache` → `traffic`; `headers`, `transform.request`, `transform.response` → `routing`; `ai.*` → `ai`; `plugin` → `plugin` plus `security` when `filterClass` is auth or authz. Changing `spec.overridable` adds `security`.
66. Secret destinations (SI § Secrets rule 7): `AIProvider.spec.baseUrl` (for `credentials.apiKey`), `auth.upstream-oauth2` `config.tokenUrl` (for `clientSecret`), and `Upstream.spec.endpoints`, `spec.discovery`, `spec.tls.sni` when that Upstream has any `spec.tls` secret; the StateStore URL is itself secret.
67. Flags: `capabilityGrant` when `Plugin.spec.capabilities` gains an element (CM example); proposed additions `capabilityRevoke` (element removed) and `imageChange` (`Plugin.spec.image` replaced), also set on an added Plugin with non-empty capabilities (`capabilityGrant`). Human annotations: `(Capability grant)`, `(Capability revoke)`, `(image change)` after the op, two spaces before.
68. Effective layer: chains of both sides are computed (R-35); for every Route present on both sides, per Phase and leg, ops `add` (Policy in TO chain only), `remove` (FROM only), `move` (R-60 rule on the chain order). JSON entry: `{"route","phase","op","policy","filterClass","after"}` where `after` is the Policy immediately before it in the TO chain (omitted when first or for `remove`), plus proposed additive `leg` (omitted for the client leg). Routes added or removed are not listed here (section 9, R-18). `summary.routeChainsChanged` counts Routes with at least one entry.
69. JSON form (`--output json`) is `ruralz.diff.v1`, versioned independently of the apiVersion, additive changes only, consumers MUST ignore unknown members (CM; CLI § Diff JSON compatibility). Members in this order: `format` (`"ruralz.diff.v1"`), `from`, `to` (each `{"revision":"rev-…","bundle"?,"file"?,"admin"?,"environment"?,"digest":"sha256:…"}`; `bundle` as in the CM example; `file`, `admin` and per-side `environment` proposed), `environment` (when both sides share one), `resources` (each `{"kind","name","change":"modified|added|removed","source"?,"impact":[…],"flags"?,"ops"?}`; added and removed resources carry identity only, no `ops`), `effective`, `summary` (`{"changed","added","removed","modified","routeChainsChanged","impact"}`). Ops: `{"op":"add|remove|replace|move","path":[…],"from"?,"to"?}`; `add` has `to`, `remove` has `from`, `replace` both, `move` integer positions. Path elements: string (object member), integer (index, diagnostics only), `{"<keyField>":"<key>"}` for `map`/`orderedMap` entries, `{"item":<value>}` for `set` elements. Empty arrays are `[]`, never `null`. Two-space indent, HTML escaping off, trailing newline.
70. Human form (CM example; exact layout golden-pinned, proposed details marked):
    - Header `ruralz bundle diff: <rev-from> -> <rev-to>` plus ` (environment: <env>)` or ` (environment: <from-env> -> <to-env>)` when known; blank line.
    - Resource line `<sym> <Kind>/<name>`, then source, then `[<impact>, …]`, columns aligned by `text/tabwriter` (padding 3, proposed); `~` modified, `+` added, `-` removed.
    - Op lines indented 4 spaces: `~ <path>: <from> -> <to>`; `+ <path>` for keyed entries and set items, `+ <path>: <value>` otherwise; `- <path>` / `- <path>: <value>` likewise; `> <path>: position <i+1> -> <j+1>`.
    - Path text: object members joined by `.` (a member not matching `^[A-Za-z_$][A-Za-z0-9_$-]*$` written `["<json-escaped>"]`), keyed entries `[<keyField>=<key>]`, set items `[item=<value>]`, indices `[<n>]`; a key or item needing quoting (contains `]`, `[`, `=`, `"`, whitespace, or is empty) is JSON-quoted; object items print as canonical JSON.
    - Values: canonical scalars printed bare (`5s`, `4`, `true`) unless empty, containing whitespace or `:,[]{}"`, or reading as a number, bool or `null` when they are strings, then JSON-quoted; objects and lists as canonical JSON.
    - If effective ops exist: blank line, `Effective Filter Chain changes:`, then `  Route/<name> <phase>[ [<leg>]]: <+|-|>> <policy> (<filterClass>[, after <p>])`.
    - Blank line and summary `<n> resource(s) changed: <a> added, <r> removed, <m> modified; <k> Route chain(s) changed; impact: <sorted list>` (singular when 1; `; impact: …` omitted when empty); no changes prints `no changes`.
71. Exit status 0 for no changes, 1 for changes, 2 for errors, including an invalid Bundle on either side, an unreadable source, RZ-CFG-027 or RZ-CFG-024 on a dump or canonical file, authentication failure (CM § Diff semantics; CLI § Exit codes: "2 for `diff`, whose result is a difference").
72. `diff(A, A)` is empty; `diff(A, B)` is empty exactly when the Revisions are equal (TQ § Required properties, Diff).

### 2.13 `/config/dump` document (format owned here, handler owned by Data plane)

73. Body: the RFC 8785 serialization of `{"content":<canonical document>,"digest":"sha256:…","lastKnownGood":"sha256:…","revision":"rev-…"}` (`lastKnownGood` omitted when none); `content` bytes are exactly the active Revision's canonical bytes; `Content-Type: application/json` (proposed; section 9, R-10). DP § Admin endpoints: "The active Revision in `ruralz.canonical.v1` form with its full digest and the Last-Known-Good digest; `secretRef` shown, secrets omitted".
74. `ruralz node dump --output-file` saves the body unchanged, so the file is a valid diff source (CLI § Command table; R-58).

### 2.14 Schema levels, deprecation, codes RZ-CFG-024 and RZ-CFG-025

75. The served level per apiVersion is the maximum `x-ruralz-since` in its embedded schema (0 in M1). RZ-CFG-024 in M1 arises only from R-33. Rollout-time skew checks and heartbeat levels are Planned (M2) (CM § Version skew).
76. RZ-CFG-025 is a warning (never changes the exit code, CLI § Exit codes) raised for a deprecated apiVersion or field, naming the removing release, e.g. `apiVersion ruralz/v1alpha1 is deprecated and is removed in <x.y.0>` (RVC § Migration tooling). The source is an explicit lifecycle table in `internal/config/convert` (apiVersion → removal release; (apiVersion, schema path) → removal release and replacement); empty in M1. Announced default changes are OQ-release-versioning-and-compatibility-7 (not M1-blocking; not reported in M1).

### 2.15 Telemetry, concurrency and performance

77. Library packages in this area do not log and emit no spans or metrics; they return diagnostics or `%w`-wrapped errors carrying RZ codes (RL § Code conventions). Consumers: the Node loader labels `ruralz_config_revision_info{revision="rev-<12 hex>",role="active|lkg"}`, `ruralz_config_activations_total{result,code}` and `ruralz_config_activation_duration_seconds{stage}` (`verify`, `compile`) and logs the `revision` attribute as `rev-<12 hex>` (OBS § metric table, § logs). Canonicalization of a watched directory counts in stage `compile`; verification in `verify` (25 ms or less for 5,000 Routes, PBB § Hot Reload stages).
78. Budgets: validation of 10,000 resources under 2 s and 20,000 under 4 s on a four-core laptop (target, CM § Validation and diff semantics), of which stages G+I+L+M SHOULD take at most 25% (proposed); Node compile of 5,000 Routes within 400 ms (target, PBB); Control-mode peak validation memory under four times the canonical size (target, CM). The encoder and resolver take a worker count; the Node passes W = max(1, `GOMAXPROCS`/2) (OQ-performance-budgets-and-benchmarking-6 (a)); the CLI passes `GOMAXPROCS`.

### 2.16 Golden corpus

79. A golden corpus of Bundles and their expected digests runs in CI (`pr-full`, Configuration conformance suite, TQ § Conformance suites) and every release MUST reproduce it byte for byte, except entries recording a change shipped as a new schema level (CM rule 3; RVC § Versioned surfaces). It pins digests and canonical bytes per Environment, diagnostics, `render --effective` tables and `ruralz.diff.v1` documents (TQ § Golden tests). It includes the CM example Bundle (`shop-bundle`) and its JSON twin, which MUST yield one digest (ADR3 § Confirmation), and the overridable golden test (RL § Presence and defaults: "an explicit `overridable: false` survives canonicalization and changes the Revision"). Golden files are LF (`.gitattributes` `eol=lf`, already global). The floor job (Go 1.26, `GOEXPERIMENT=jsonv2`) MUST produce identical golden output (TQ § Unit and property tests).

## 3. Proposed Go packages and API

All under `internal/` (RL § Monorepo tree: `internal/config/` holds "loader, profile, overlay, subst, validate, hub, convert, canonical, diff"). No package in this area imports `internal/gateway`, `internal/control`, `internal/cli` or `internal/tool`; all link into the three binaries. Every file carries the license header; no `init()`, no mutable package-level variables (tables are constructed values or `switch` functions).

```
internal/config/fieldpath   key-aware paths (shared with diagnostics)
internal/config/schemaidx   index over the embedded rendered schema: defaults, list types, impact, since, secret, ref, cel, def names
internal/config/registry    Policy type registry (FP 10) + impact column + Phase selection
internal/config/hub         hub resource model, stage G normalizer, typed views (M1 aliases of v1alpha1)
internal/config/convert     converter registry, v1alpha1 identity converter, lifecycle (deprecation) table
internal/config/canonical   RFC 8785 encoder, ruralz.canonical.v1 document, Decode, Verify, dump document
internal/config/revision    Digest type
internal/config/precedence  effective Filter Chains, RZ-CFG-018/019/020/029/038
internal/config/diff        ruralz.diff.v1 engine and writers
internal/config/render      rendered-Bundle emitter (YAML/JSON), effective table writer, conversion writer
internal/cli/bundle         verbs render, diff, build (validate belongs to area 1), diff source resolution
```

Dependency order, each package importing only those before it (no cycles): `fieldpath` → `schemaidx` → `revision` → `registry` → `convert` → `hub` → `canonical` → `precedence` → `diff` → `render` → `internal/cli/bundle`. Area 1's `diag` package (which uses `fieldpath`) sits between `fieldpath` and `schemaidx`.

```go
// Package revision: internal/config/revision
type Digest struct{ sum [sha256.Size]byte } // comparable; zero value invalid

func Sum(canonical []byte) Digest
func Parse(s string) (Digest, error)       // "sha256:" + 64 lowercase hex only
func ParseDisplay(s string) (string, error) // "rev-" + 12 lowercase hex → the 12-hex prefix (M2 lookups)
func (d Digest) String() string             // "sha256:<64 hex>"
func (d Digest) Short() string              // "rev-<12 hex>"
func (d Digest) Hex() string
func (d Digest) IsZero() bool
func (d Digest) MarshalText() ([]byte, error)
func (d *Digest) UnmarshalText(b []byte) error
```

```go
// Package fieldpath: internal/config/fieldpath
type ElemKind uint8
const (Member ElemKind = iota; Index; Keyed; Item)
type Elem struct {
    Kind     ElemKind
    Name     string // Member: object member name
    Index    int    // Index: atomic list position
    KeyField string // Keyed: e.g. "name", "address", "issuer"
    Key      string // Keyed: key value
    Item     any    // Item: canonical value of a set element
}
type Path []Elem
func (p Path) String() string                 // human form, R-70
func (p Path) MarshalJSON() ([]byte, error)   // JSON form, R-69 / CM § Diagnostics and source map
func (p Path) Append(e Elem) Path             // copies
```

```go
// Package schemaidx: internal/config/schemaidx
type ListType uint8
const (ListNone ListType = iota; ListMap; ListOrderedMap; ListSet; ListAtomic)
type Scalar uint8
const (ScalarNone Scalar = iota; ScalarDuration; ScalarByteSize; ScalarDecimal; ScalarIntOrString)
type Node struct { // one resolved schema node; immutable
    Default    any       // json.Number / string / bool / map / slice, or nil
    HasDefault bool
    List       ListType
    ListKey    string
    Impact     []string
    Since      int
    Secret, Ref, CEL bool
    Scalar     Scalar
    Open       bool      // object without additionalProperties:false
    // unexported: properties, items, additionalProperties, policy config dispatch
}
type Index struct{ /* per apiVersion, per kind spec root */ }
func Load(apiVersion string, rendered []byte) (*Index, error) // built once at startup, passed explicitly
func (x *Index) Level() int                                    // max x-ruralz-since
func (x *Index) Spec(kind string) (*Node, bool)
func (n *Node) Property(name string) (*Node, bool)
func (n *Node) Items() *Node
func (n *Node) Values() *Node                                  // additionalProperties schema of typed maps
func (n *Node) PolicyConfig(policyType string) (*Node, bool)   // PolicySpec if/then dispatch
func (n *Node) PropertyNames() []string                        // for round-trip enumeration tests
```

```go
// Package registry: internal/config/registry
type Scope uint8
const (ScopeGateway Scope = iota; ScopeRoute; ScopeUpstream)
type Entry struct {
    Type               v1alpha1.PolicyType
    Class              v1alpha1.FilterClass // "" for plugin (from spec.filterClass)
    Scopes             []Scope
    Slot               string               // fixed slot; "" means the Policy's own name
    DefaultFailureMode v1alpha1.FailureMode
    ClosedOnly         bool                 // plugin: computed from filterClass
    Impact             []string             // R-65 (proposed column)
    Planned            string               // "M1".."M3", informational
}
type BodyOracle interface { // implemented by the CEL area
    ReferencesRequestBody(expr string) (bool, error)
}
type Registry struct{ /* unexported table */ }
func New(body BodyOracle) *Registry
func (r *Registry) Lookup(t v1alpha1.PolicyType) (Entry, bool)
func (r *Registry) ClassRank(c v1alpha1.FilterClass) int           // cors=0 … custom=8
func (r *Registry) Defaults(name string, spec map[string]any)       // slot, failureMode, filterClass (R-12)
// PolicyView is the part of a Policy spec Phase selection reads; hub resources implement it.
type PolicyView interface {
    Type() v1alpha1.PolicyType
    FilterClass() v1alpha1.FilterClass
    Config() map[string]any // normalized config tree
}
// Phases returns the Phases the Policy runs in at scope s (R-13, R-14); pluginPhases is
// Plugin.spec.phases for type plugin, else nil. A disallowed scope or Phase is RZ-CFG-020.
func (r *Registry) Phases(p PolicyView, s Scope, pluginPhases []v1alpha1.Phase) ([]v1alpha1.Phase, error)
func PhaseRank(p v1alpha1.Phase) int // onRequestHeaders 0 … onLog 7, onChunk 8
func IsResponsePhase(p v1alpha1.Phase) bool
func ClientPhase(p v1alpha1.Phase) bool
func UpstreamPhase(p v1alpha1.Phase) bool
```
(`registry` never imports `hub`; `hub` imports `registry`.)

```go
// Package hub: internal/config/hub
type Kind = v1alpha1.Kind
func KindOrder(k Kind) int    // canonical: Gateway 0, Upstream 1, Plugin 2, Policy 3, Consumer 4, AIProvider 5, AIModel 6, Route 7
func CatalogOrder(k Kind) int // display: Gateway, Route, Upstream, Policy, Plugin, Consumer, AIProvider, AIModel
type ID struct{ Kind Kind; Name string }
type Source struct{ File string; APIVersion string } // outside the digest
type Resource struct {
    ID
    Labels, Annotations map[string]string
    Spec   map[string]any // normalized hub tree: map[string]any, []any, string, json.Number, bool
    Source Source
}
type Bundle struct{ /* sorted resources + index */ }
func NewBundle(rs []Resource) *Bundle
func (b *Bundle) Resources() []Resource
func (b *Bundle) Get(id ID) (*Resource, bool)
func (b *Bundle) Gateway() (*Resource, bool)
func (b *Bundle) Routes() []*Resource

// Typed views for semantic stages. M1: aliases; v1beta1 (M3) replaces them with hub-owned types.
type (
    GatewaySpec = v1alpha1.GatewaySpec; RouteSpec = v1alpha1.RouteSpec; UpstreamSpec = v1alpha1.UpstreamSpec
    PolicySpec = v1alpha1.PolicySpec; PluginSpec = v1alpha1.PluginSpec; ConsumerSpec = v1alpha1.ConsumerSpec
    AIProviderSpec = v1alpha1.AIProviderSpec; AIModelSpec = v1alpha1.AIModelSpec
)
func Decode[T any](r *Resource) (*T, error) // tree → typed view

type Meta struct {
    Name                string
    Labels, Annotations map[string]string
}
type Normalizer struct{ idx *schemaidx.Index; reg *registry.Registry; conv *convert.Registry }
func NewNormalizer(idx *schemaidx.Index, reg *registry.Registry, conv *convert.Registry) *Normalizer
// Normalize runs stage G on one decoded source resource: convert to hub, defaults, registry
// defaults, scalar and list normalization, strip conversion-data. Pure; safe for concurrent use.
// Diagnostics carry resource identity and fieldpath.Path; the pipeline adds file positions.
func (n *Normalizer) Normalize(apiVersion string, kind Kind, meta Meta, spec map[string]any, src Source) (Resource, diag.List)
```

```go
// Package convert: internal/config/convert
const ConversionDataAnnotation = "ruralz.io/conversion-data"
type Converter interface {
    APIVersion() string
    ToHub(kind string, spec map[string]any, partial bool) (map[string]any, diag.List)
    FromHub(kind string, spec map[string]any, partial bool) (map[string]any, diag.List)
}
type Notice struct{ Removal, Replacement string }
type Lifecycle struct {
    APIVersions map[string]Notice            // deprecated apiVersions
    Fields      map[string]map[string]Notice // apiVersion → schema path → notice
}
type Registry struct{ /* explicit table */ }
func NewRegistry(lc Lifecycle, cs ...Converter) *Registry
func Default() *Registry                  // constructs {v1alpha1 identity}, empty Lifecycle; not a global
func V1alpha1() Converter
func (r *Registry) Served() []string
func (r *Registry) Deprecations(apiVersion string, spec map[string]any) diag.List // RZ-CFG-025
func (r *Registry) Convert(from, to, kind string, doc map[string]any, partial bool) (map[string]any, diag.List) // RZ-CFG-007
```

```go
// Package canonical: internal/config/canonical
const Format = "ruralz.canonical.v1"
type Options struct {
    Level   int // schema level to render at; M1: must equal idx.Level()
    Workers int // parallel resource encoders; <1 means 1
}
type Revision struct {
    Digest  revision.Digest
    Content []byte // exact canonical bytes
}
type Encoder struct{ /* idx, immutable */ }
func NewEncoder(idx *schemaidx.Index) *Encoder
// Encode sorts resources (R-22), applies since-omission (R-23) and writes JCS. It checks ctx
// between resources so a superseded Node reload can stop early.
func (e *Encoder) Encode(ctx context.Context, b *hub.Bundle, opt Options) (Revision, error)

type Decoder struct{ /* idx, normalizer */ }
func NewDecoder(idx *schemaidx.Index, n *hub.Normalizer) *Decoder
func (d *Decoder) Decode(content []byte) (*hub.Bundle, error)                         // RZ-CFG-024 / RZ-CFG-027(b)
func (d *Decoder) Verify(content []byte, want revision.Digest) (*hub.Bundle, error)   // R-31

// RFC 8785 primitives (exported for the dump envelope and tests).
func AppendJCS(dst []byte, v any) ([]byte, error) // map[string]any, []any, string, json.Number, bool
func AppendString(dst []byte, s string) []byte
func AppendNumber(dst []byte, lit json.Number) ([]byte, error) // R-16, R-24
func CompareNames(a, b string) int                             // UTF-16 code unit order

// /config/dump (R-73)
type Dump struct {
    Content       []byte
    Digest        revision.Digest
    LastKnownGood revision.Digest // zero: none
}
func AppendDump(dst []byte, active Revision, lkg revision.Digest) []byte
func (d *Decoder) ReadDump(r io.Reader, limit int64) (*hub.Bundle, Dump, error)
```

```go
// Package precedence: internal/config/precedence
type Entry struct {
    Policy      string
    Type        v1alpha1.PolicyType
    Class       v1alpha1.FilterClass
    Slot        string
    Scope       registry.Scope
    Position    int
    FailureMode v1alpha1.FailureMode
    Overridable bool
    Replaces    string // Gateway Policy this Route Policy replaced, or ""
}
type RemovalReason uint8
const (Replaced RemovalReason = iota; Excluded)
type Removed struct{ Policy, Slot, By string; Reason RemovalReason }
const NumPhases = 9
type Leg struct {
    Upstream string
    Phases   [NumPhases][]Entry // execution order
}
type Chain struct {
    Route   string
    Client  [NumPhases][]Entry
    Legs    []Leg
    Removed []Removed
}
type Resolver struct{ /* registry */ }
func NewResolver(reg *registry.Registry) *Resolver
func (r *Resolver) CheckPolicies(b *hub.Bundle) diag.List                         // RZ-CFG-029, R-38
func (r *Resolver) Resolve(ctx context.Context, b *hub.Bundle, workers int) (map[string]*Chain, diag.List) // RZ-CFG-018/019/020/038
type Row struct{ Phase, Leg, Policy, Type, Class, Slot, From, Reason string }
func (c *Chain) Rows() []Row // R-51 order and reason templates
```

```go
// Package diff: internal/config/diff
const Format = "ruralz.diff.v1"
type Side struct {
    Revision    revision.Digest
    Bundle      *hub.Bundle
    Chains      map[string]*precedence.Chain
    Sources     map[hub.ID]string // nil for canonical, dump and admin sides
    Environment string
    Ref         SideRef           // bundle / file / admin label for JSON
}
type SideRef struct {
    Revision    string `json:"revision"`
    Bundle      string `json:"bundle,omitempty"`
    File        string `json:"file,omitempty"`
    Admin       string `json:"admin,omitempty"`
    Environment string `json:"environment,omitempty"`
    Digest      string `json:"digest"`
}
type Op struct {
    Op   string         `json:"op"`
    Path fieldpath.Path `json:"path"`
    From any            `json:"from,omitempty"`
    To   any            `json:"to,omitempty"`
    flag string         // human annotation
}
type ResourceChange struct {
    Kind   string   `json:"kind"`
    Name   string   `json:"name"`
    Change string   `json:"change"`
    Source string   `json:"source,omitempty"`
    Impact []string `json:"impact"`
    Flags  []string `json:"flags,omitempty"`
    Ops    []Op     `json:"ops,omitempty"`
}
type ChainOp struct {
    Route       string `json:"route"`
    Leg         string `json:"leg,omitempty"`
    Phase       string `json:"phase"`
    Op          string `json:"op"`
    Policy      string `json:"policy"`
    FilterClass string `json:"filterClass"`
    After       string `json:"after,omitempty"`
    From        *int   `json:"from,omitempty"` // move only: zero-based FROM position
    To          *int   `json:"to,omitempty"`   // move only: zero-based TO position
}
type Summary struct {
    Changed, Added, Removed, Modified, RouteChainsChanged int
    Impact []string
}
type Result struct {
    Format      string           `json:"format"`
    From, To    SideRef
    Environment string           `json:"environment,omitempty"`
    Resources   []ResourceChange `json:"resources"`
    Effective   []ChainOp        `json:"effective"`
    Summary     Summary          `json:"summary"`
}
type Engine struct{ /* idx, reg */ }
func NewEngine(idx *schemaidx.Index, reg *registry.Registry) *Engine
func (e *Engine) Compare(from, to *Side) (*Result, error)
func (r *Result) Empty() bool
func WriteJSON(w io.Writer, r *Result) error
func WriteText(w io.Writer, r *Result) error
```

```go
// Package render: internal/config/render
type Format uint8
const (YAML Format = iota; JSON)
type SourceDoc interface { // provided by the loader (area 1): ordered, merged, substituted tree
    APIVersion() string; Kind() string; Name() string
    Tree() tree.Value // ordered object, positions, substitution-position info
}
// SourceFile is one discovered base or overlay file, parsed but unmerged and unsubstituted.
type SourceFile struct {
    Path string       // slash-separated, relative to the Bundle root
    JSON bool         // .json source: emit JSON
    Docs []tree.Value // documents in file order
}
// LoadFunc discovers and parses a source Bundle (area 1's discovery + profile parser).
type LoadFunc func(ctx context.Context, root fs.FS) ([]SourceFile, diag.List, error)
func WriteBundle(w io.Writer, docs []SourceDoc, idx *schemaidx.Index, f Format) error     // R-44..R-47
func WriteEffective(w io.Writer, c *precedence.Chain, rev revision.Digest, env string, f Format) error // R-51, R-52
// ConvertTree rewrites every source file under target into dst (R-53); the caller then renders both trees.
func ConvertTree(ctx context.Context, src fs.FS, dst string, target string, conv *convert.Registry, load LoadFunc) (diag.List, error)
```

```go
// Package bundle: internal/cli/bundle (verbs render, diff, build)
func Render(ctx context.Context, args []string, stdout, stderr io.Writer) int
func Diff(ctx context.Context, args []string, stdout, stderr io.Writer) int
func Build(ctx context.Context, args []string, stdout, stderr io.Writer) int
type sourceKind uint8 // dir, file, admin, revision (M2), oci (M2)
func classify(arg string) sourceKind
func loadSide(ctx context.Context, arg, env string, o sideOptions) (*diff.Side, error)
```

Concurrency model:
- `schemaidx.Index`, `registry.Registry`, `hub.Normalizer`, `canonical.Encoder/Decoder`, `precedence.Resolver`, `diff.Engine` are immutable after construction and safe for concurrent use; they are built once per process (CLI invocation, Node boot) and passed explicitly.
- Parallelism is opt-in by `workers`: a call owns a bounded pool (`workers` goroutines fed from a bounded channel of resource or Route indices), waits for all of them before returning (`sync.WaitGroup`), stops dispatching on `ctx.Done()`, and merges results by index so output order never depends on scheduling. No goroutine outlives its call.
- No function blocks on I/O except `internal/cli/bundle` source loading and `ReadDump`'s reader; the admin fetch takes `ctx` with a 30 s timeout; FROM and TO load concurrently in two goroutines joined before comparison.

Exported for other areas: `revision.Digest` (Node loader, Last-Known-Good, admin, telemetry labels, Control M2); `canonical.Encoder/Decoder/Verify/AppendDump/ReadDump` (Node file-mode loader, LKG, handover, `/config/dump` handler, `ruralz node dump`, `ruralz dev run` testDigest, Control M2); `hub.Normalizer`/`hub.Bundle` (pipeline facade); `registry.Registry` (Filter implementations, Data plane chain builder, audit M2); `precedence.Chain` (Data plane executor, telemetry label admission order, export dot M2); `diff.Engine` (CLI, Control `/api/v1/revisions/{digest}/diff` M2); `fieldpath.Path` (diagnostics).

## 4. Dependencies on other areas

Needs:

| From | What | Minimal contract |
|---|---|---|
| Area 1 (loader, profile, overlay, subst, validate) | Ordered generic tree per resource after merge and substitution, with source positions and a flag per string scalar saying whether substitution ran there | `tree.Value` (ordered object, `json.Number` numbers, no `null`), `SourceMap.Lookup(hub.ID, fieldpath.Path) (file string, line, col int)`, `SourceMap.File(hub.ID) string` |
| Area 1 | Diagnostics type and pipeline facade | `diag.Diagnostic{Code, Severity, File, Line, Column, Resource{Kind,Name}, Path fieldpath.Path, Message, Hint}`, `diag.List`; facade `pipeline.Run(ctx, Input) (*Output, diag.List)` calling `hub.Normalizer` (G), `precedence.Resolver` (I) and `canonical.Encoder` (L, M) |
| Area 1 | Environment handling for `--env`, `--environments`, `--from-env`, `--to-env`; single-file Bundle loading for diff file sides; reference resolution (RZ-CFG-009) before stage I | `pipeline.Input{Dir or File, Env, Environments}` |
| CEL area | Body-reference oracle for `authz.cel` Phase selection | `registry.BodyOracle` |
| Errors (internal/errcode) | Registration of proposed RZ-CFG-038; a wrapper error type with code | `errcode.Error{Code string; Err error}`, `errcode.CodeOf(err) (string, bool)` |
| CLI framework | Noun dispatch for `bundle`; interspersed flags (the CM and CLI examples put flags after FROM TO); shared admin HTTP client for `--admin`, `--admin-token-file`, `--ca-file`, `--client-cert/--client-key` | `adminclient.Get(ctx, base, path) (io.ReadCloser, error)` |
| Data plane | `/config/dump` handler (writes `canonical.AppendDump`), LKG file layout, handover candidate; builds per-Phase arrays from `precedence.Chain` | Uses provided APIs |
| Telemetry | None in these packages; the Node loader owns metrics and logs (R-77) | — |
| Schema owner (area 1 / traffic / security) | New `+ruralz:default` and `+ruralz:impact` markers in `pkg/config/v1alpha1` (sections 7, 9) | Regenerated `api/schema` |

Provides: see "Exported for other areas" in section 3.

## 5. Libraries

| Module | Version | Use | Why |
|---|---|---|---|
| Standard library | go1.27.1 toolchain, go 1.26.0 floor with `GOEXPERIMENT=jsonv2` | `crypto/sha256`, `encoding/hex`, `encoding/json` (decode with `UseNumber`; diff/effective JSON with `SetEscapeHTML(false)`), `unicode/utf8`, `unicode/utf16`, `strconv`, `math`, `slices`, `time`, `text/tabwriter`, `io/fs`, `net/http`, `crypto/tls`, `crypto/x509`, `context`, `sync` | Everything here is deterministic serialization, hashing and tree walking; no catalog row is needed (TS § Library catalog) |
| `encoding/json/jsontext` (standard library) | Go 1.27 | Test-only differential oracle (`jsontext.Value.Canonicalize`, which implements RFC 8785) in `_test.go` files constrained `//go:build go1.27` | Production code cannot use it: with `go 1.26.0` in `go.mod`, `go vet`'s stdversion check fails ("jsontext.Value requires go1.27 or later (module is go1.26)"), and a `go1.27 \|\| goexperiment.jsonv2` constraint still fails vet (verified locally); the floor job simply skips the oracle file |
| `github.com/goccy/go-yaml` | v1.19.2 (catalog v1.19.x) | Not imported by this area; area 1's loader parses YAML and JSON. Render and conversion use this area's own emitter so golden output never moves with a library release | TS catalog row; ADR3 |
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.3 (catalog v6.0.x) | Not imported here; `schemaidx` reads the generated schema JSON directly (a known subset: `$ref`, `properties`, `items`, `additionalProperties`, `allOf`/`if`/`then`, annotations) | Avoids coupling to validator internals |

No new module enters `go.mod` from this area; depguard needs no change.

## 6. Test plan

Unit and table tests (race detector, shuffled, `pr-fast`; coverage 90% for precedence and canonical form, TQ § Test pyramid):

1. `revision`: `Parse` accepts `sha256:` + 64 lowercase hex; rejects uppercase hex, 63/65 chars, `sha512:`, missing prefix, `rev-…`; `Short()` of the CM example digest is `rev-162af81f5de4`; text round trip; `ParseDisplay` accepts only 12 lowercase hex.
2. JCS strings: table over U+0000–U+001F (expect `\b \t \n \f \r`, else `\u00xx` lowercase), `"`, `\`, `/` (literal), U+007F, U+2028, U+2029, U+1F600 (literal); invalid UTF-8 → internal error.
3. JCS numbers (RFC 8785 Appendix B and edge cases): `0`, `-0` → `0`, `1.0` → `1`, `100`, `1E2` → `100`, `0.05`, `0.000001`, `1e-7`, `1e21` → `1e+21`, `1e20` → `100000000000000000000`, `333333333.3333333`, `5e-324`, `1.7976931348623157e308`, float literal `1.2345678901234568e20` → `123456789012345680000`; integer literal `123456789012345678901` → RZ-CFG-005; integer literal `9007199254740991` OK, `9007199254740992` → RZ-CFG-005; NaN/Inf literals → RZ-CFG-005.
4. JCS member order: RFC 8785 § 3.2.3 set `"€"`, `"\r"`, `"דּ"`, `"1"`, `"😀"`, `"\u0080"`, `"ö"` → `\r`, `1`, U+0080, U+00F6, U+20AC, U+1F600, U+FB33; duplicate names → error.
5. Differential (`//go:build go1.27`): `AppendJCS` equals `jsontext.Value.Canonicalize` on every golden canonical document and on a generated corpus of 10,000 random JSON values without integers above 2^53.
6. Scalar normalization tables: durations `90s`→`1m30s`, `1.5h`→`1h30m0s`, `1000ms`→`1s`, `0`→`0s`, `1h0m0s`→`1h0m0s`, overflow → RZ-CFG-005; byte sizes `10Mi`, `64Ki`, `1.5Gi`, `1k`, integer passthrough; decimals `3.00`→`3`, `0.30`→`0.3`, `007`→`7`, `0.0`→`0`, `15.50`→`15.5`; IntOrString `8080` vs `"http"` unchanged.
7. Defaults: generated from `schemaidx` — for every property with `default` in the rendered schema, a minimal resource with the parent present and the field absent canonicalizes with the default; with an explicit different value keeps it; an absent parent object is not created (e.g. Gateway without `limits` has no `limits` in canonical form); list-entry defaults (`upstreams[].weight`); Policy config defaults via type dispatch (`quota` `config.key`, `transform.*` `contentType`, `auth.upstream-oauth2` `timeout`); every default validates against the rendered schema.
8. Registry defaults: for each of the 23 types, absent `slot`/`failureMode`/`filterClass` materialize the table values of R-13; explicit values kept; `plugin` without `filterClass` → `custom`; authored `filterClass: admission` on `ratelimit` accepted, `filterClass: auth` on `ratelimit` → RZ-CFG-005.
9. Overridable golden (RL § Presence and defaults): three Bundles differing only in `ratelimit-global` `overridable` absent / `true` / `false`; digests absent == true != false; canonical bytes of the false case contain `"overridable":false`; an explicit `false` on a Route-attached Policy also changes the digest.
10. Lists: `set` of strings reordered → same digest; `AuthMTLSConfig.subjects` (set of objects) sorted by canonical bytes; duplicate set element → RZ-CFG-005 at `[item=GET]`; `map` lists sorted by key (`endpoints` by `address`, `credentials.jwt` by `issuer`); duplicate key → RZ-CFG-005 at `[name=x]`; `orderedMap` and `atomic` order preserved (reordering changes the digest).
11. Since-omission with a synthetic schema (level 1 field with default): equal default omitted, different value written, absent-without-default stays absent; `Decode` restores the default.
12. Resource order and envelope: shuffled input → identical bytes; output starts `{"format":"ruralz.canonical.v1","resources":[`; no `apiVersion`; `ruralz.io/conversion-data` stripped.
13. Decode/Verify: round trip equals; added whitespace or reordered members → RZ-CFG-027 "not in ruralz.canonical.v1 form"; one flipped byte → RZ-CFG-027 hash mismatch; top-level `schemaLevel` member, `format: ruralz.canonical.v2`, unknown kind, unknown spec field → RZ-CFG-024 each; `Materialize(Decode(x))` == `Decode(x)`.
14. Dump: `AppendDump`/`ReadDump` round trip; `content` with CEL `>=` and `&&` survives byte-exact; tampered content → RZ-CFG-027; oversize (limit+1 bytes) → error; `lastKnownGood` omitted when zero.
15. Precedence, example Bundle: `orders-summary` rows equal CM § Worked example (Phase, Leg, Policy, From); `support-chat`: `apikey-partner` replaces `jwt-default` in slot `auth`, `token-budget-partner` in onRequestBody, onChunk, onLog; `cart-grpc`: `cors-default` excluded, `headers-security` in onResponse only.
16. Precedence errors (one fixture each, with file/line/column, TQ § Conformance suites): RZ-CFG-018 two Route `auth.*` Policies; two Gateway `ratelimit` with explicit equal `slot`; two `auth.upstream-oauth2` on one Upstream. RZ-CFG-019 exclude `ratelimit-global` (message and "declared in" position as CM § Diagnostics); Route `cors` replacing an `overridable: false` Gateway `cors`; Route re-listing `ratelimit-global`. RZ-CFG-020 `validation.json-schema` at Gateway; `cache` at Gateway; `auth.upstream-oauth2` at Route; `auth.jwt` at Upstream; `plugin` with `onResponse` at Upstream; `plugin` with `onUpstreamRequest` at Route. RZ-CFG-029 `auth.jwt` open; `authz.cel` open; `auth.upstream-oauth2` open; `plugin` `filterClass: authz` open; `ratelimit` open and `plugin` `custom` open accepted. RZ-CFG-038 (proposed) `cache` + `validation.json-schema`; `cache` + `authz.cel` whose rule reads `request.body`; `cache` + `authz.cel` without body accepted; `cache` + `plugin` authz with onRequestBody.
17. Phase selection: `headers` with only `response.set` at G → onResponse only; only `request.set` at U → onUpstreamRequest only; both at R → onRequestHeaders and onResponse; empty config → no rows; `transform.request` at U → onUpstreamRequest; `authz.cel` body oracle both ways.
18. Ordering: onResponse reverses class, scope and position (`headers-security` before `cors-partner`); onLog keeps request order (`quota` fixtures); two Route `ratelimit` at positions 0 and 1 keep list order.
19. Render: idempotence (bytes and digest) on the golden corpus; `$${HOME}` round trip in a string field, and verbatim `${` in a CEL field; quoting table (`1.3`, `0777`, `yes`, `on`, `null`, `true`, `""`, `a: b`, `-x`) re-parses to strings; `--output json` parses; `--environments` without `--env` → exit 2.
20. Effective CLI: missing `--route` → 2; `--route` alone → 2; unknown Route → 2; invalid Bundle → 1 with diagnostics on stderr; text golden; JSON golden.
21. Conversion: `--api-version ruralz/v1alpha1` on `shop-bundle` → exit 0, lines `environment prod: rev-X -> rev-X equal` (and staging); overlays, `$patch`, `${OTEL_EXPORTER_OTLP_ENDPOINT:-…}` preserved textually; comments gone; `ruralz/v9` → RZ-CFG-007 exit 2; missing `--environments` or `--output-dir` → 2; non-empty DIR → 2; a fault-injected converter that changes a value → exit 1 with the diff on stderr. Round-trip property: for every property path enumerated from `schemaidx`, a generated value survives ToHub→FromHub.
22. Build: text line format; JSON members; `--output-file` bytes equal `Content` and `sha256` of the file equals the digest; Bundle with a Plugin and no `--offline` → RZ-CFG-028 exit 1; with `--offline` → exit 0.
23. Diff engine: reproduce the CM example (FROM: `secondary` first, no `response.send`, extra `Upstream/legacy-orders`, no `ratelimit-orders`, `orders-summary` `timeout: 5s`; TO: example for `prod`) → golden JSON and human (ordering per R-62, R-61); impacts `[traffic]`, `[plugin, security]`, `[ai]`, `[traffic]`, `[routing]`; `flags: ["capabilityGrant"]`; effective `add ratelimit-orders after ratelimit-global`; summary `5 resources changed: 1 added, 1 removed, 3 modified; 1 Route chain changed; impact: ai, plugin, routing, security, traffic`.
24. Diff rules: orderedMap `[a,b,c]`→`[x,a,b,c]` one `add`, no moves; swap → two moves with 0-based JSON and 1-based human positions; atomic `limits` change → one `replace` at `spec.config.limits`; Policy type change → `replace spec.type` and `replace spec.config`; secretRef provider change → op shows references only, impact includes `security`; label change → `metadata`; `AIProvider.baseUrl` change → `ai, security`; Gateway Policy added → every Route's chain changes, `routeChainsChanged` = Route count.
25. Diff sources: directory with `--from-env staging --to-env prod`; rendered YAML file; JSON twin file; canonical file from `build --output-file`; dump file; admin URL via `httptest.NewTLSServer` (bearer header asserted, client-cert variant, 401 → exit 2 with problem document, 256 MiB+1 body → exit 2, tampered dump → RZ-CFG-027 exit 2, dump from a "newer" Node with an unknown member → RZ-CFG-024 exit 2); `http://` non-loopback with a token → exit 2; `rev-162af81f5de4`, `sha256:…`, `oci://…` → exit 2 "Planned (M2)"; invalid Bundle side → exit 2; equal sides → `no changes`, exit 0.
26. Secret leak: a canary value in `RURALZ_STATE_STORE_URL` and a `file` secret; assert it never appears in render, build, canonical bytes, dump or diff output (TQ § End-to-end tests; RM Security "secret leak tests").

Property tests (`testing.F`, seeds in `pr-fast`, longer in `nightly`; TQ § Required properties):

27. `FuzzRevisionStableUnderReorder`: reordering files, `map` and `set` lists and reformatting keeps the digest; YAML and JSON twins give one digest.
28. `FuzzRenderIdempotent`: render(render(B)) same digest and bytes.
29. `FuzzConversionRoundTrip`: every field through the hub; converting keeps the Revision.
30. `FuzzPrecedenceInvariants`: random Gateway/Route/Upstream attachments of registry types: one Policy per slot per scope; `overridable: false` never removed (or an RZ-CFG-019 is raised); order follows class, scope, position; response Phases reversed; onLog request order.
31. `FuzzDiffProperties`: `diff(A,A)` empty; empty iff digests equal; added/removed are set differences mirrored in `diff(B,A)`; applying ops to A's canonical tree yields B's; reordering `map`/`set` yields no op.

Fuzz targets (TQ § Fuzzing; crashers become committed seeds):

32. `FuzzCanonicalRoundTrip`: parse canonical → re-encode identical bytes (oracle "parse, canonicalize and parse again: identical bytes").
33. `FuzzDecodeCanonical` and `FuzzReadDump`: arbitrary bytes never panic; errors carry RZ-CFG-024, RZ-CFG-027 or a parse error.
34. `FuzzJCSDifferential` (`go1.27` tag).
35. `FuzzHumanPath`: path printing never panics and JSON paths round-trip.

Golden corpus (location `test/conformance/config/golden/<entry>/` with `bundle/`, `environments.yaml`, `expected.json` holding `{"level":0,"digests":{"<env>|-":"sha256:…"}}`, `canonical/<env>.json`, `effective/<env>/<route>.txt|.json`, `diff/<name>.json|.txt`, and `CHANGES` recording any schema-level change; `examples/shop-bundle/` is the CM example verbatim, validated in CI):

36. Entries: `shop-bundle` for `prod`, `staging` and no Environment (process-environment variables fixed by the test); its JSON twin (`ruralz.yaml` holding JSON text, one resource per `.json` file); `overridable-{absent,true,false}`; `minimal` (one Gateway, one Route, one Upstream); one entry per Policy type with every registry default visible; `lists` (every set/map/orderedMap/atomic list); `numbers-and-units`. A golden digest that changes without a `CHANGES` record fails; regeneration only with `go test -run Golden -update`.
37. The floor job reproduces all golden outputs.

Integration and end-to-end (`pr-full` stage 8, stage 10; no Docker daemon locally, so the Compose scenario runs in CI only; a local variant runs two `ruralzd` processes in one test):

38. File-mode Node on a watched rendered directory: `/config/dump` digest equals `ruralz bundle build` of the same source; LKG written; restart with a tampered LKG content file → not activated, `ruralz_config_activations_total{result="rejected",code="RZ-CFG-027"}` increments, active Revision (or not-ready) per FP § 8.2; LKG with an unknown field → RZ-CFG-024.
39. E2E scenario 6: `ruralz bundle diff https://<node>:9901 ./bundle` after an injected change shows it, exit 1; without a change exit 0.
40. E2E scenario 2 (REST part): `shop-bundle` `orders-summary` spans follow the `render --effective --route orders-summary` rows.
41. E2E per M1 command (exit criterion 5): `render`, `render --effective --route`, `render --api-version`, `diff` (three source kinds), `build`.

Benchmarks (`pr-full` microbenchmarks, alloc/op gate, TQ § Benchmarks and regression gates): canonicalize and hash the PBB ladder Bundles (116, 1,151, 5,751, 11,501 resources); `Decoder.Verify` on the 5,751-resource Revision (within the 25 ms `verify` budget, target); `Resolver.Resolve` for 5,000 Routes; `diff` of the two R1 Revisions.

## 7. Open questions blocking M1 in this area

| ID | Question | Adopt | What the code does |
|---|---|---|---|
| OQ-configuration-model-8 (listed in exit criterion 7) | Are the registered default `failureMode` values right for `quota` (open) and `ai.token-budget` (closed)? | (a) As registered | Registry defaults `quota` → `open`, `ai.token-budget` → `closed`, materialized in every canonical Policy; golden entry per type pins them; a later change ships as a new schema level with a `CHANGES` record |
| OQ-traffic-management-and-resilience-6 ("Yes, canonical form (M1)") | Are the Deadlines defaults right; which timeouts are fields; gRPC keepalive? | (c) per-protocol Route timeouts (marked recommended), without (d)/(e) in M1 | Static defaults become `+ruralz:default` markers in `pkg/config/v1alpha1` and are materialized by R-9 inside present objects: `retries.attempts` 1, `circuitBreaker.maxConnections` 1024, `maxPendingRequests` 256, `consecutiveFailures` 5, `openDuration` 30s, `failureWhen` `error != null \|\| response.status in [502, 503, 504]`, `healthCheck.passive.consecutiveErrors` 5, `ejectionTime` 30s. Context-derived defaults stay absent in canonical form and are computed at compile time: `Route.spec.timeout` (15 s; 1 h for stream Routes, Planned (M3)), `Upstream.spec.timeout` (the Route's), `retries.perTryTimeout` (leg time left / (retries left + 1)), `retries.retryOn` (method-dependent); changing any derivation rule is a default change needing a schema level (section 9, R-3) |
| OQ-traffic-management-and-resilience-21 | How does `cache` coexist with `onRequestBody` authz, validation or Plugin auth? | (a) Reject with a new `RZ-CFG` code (proposed) | Precedence raises proposed RZ-CFG-038 (R-39) after registration in CM and `internal/errcode` |
| OQ-traffic-management-and-resilience-2 | How do admitted responses get RateLimit fields? | (c) Data plane appends them (no option is marked; recommended here) | Registry Phases for `ratelimit` and `quota` stay as FP § 10, so the worked-example table and golden stay valid. If (a) is chosen instead, add onResponse to both rows of the registry table (one-line change) and regenerate effective goldens before 0.1.0 |
| OQ-traffic-management-and-resilience-5 ("breaker `minimumLegs` (M1)") | Which fields set backoff, retry budget, hedging and breaker guards? | (a) Proposed fields; `minimumLegs` in M1 | New fields added before 0.1.0 are schema level 0 (no `x-ruralz-since`); their defaults (`minimumLegs` 20, 50% ratio, 3 half-open successes per TMR § Circuit breakers, target) are markers materialized by R-9; no code change here |
| OQ-security-and-identity-22 | How are secrets and Node connections restricted? | (a) proposed, incl. the `security` impact class and secret-to-destination binding | Diff impact rules R-64 and R-66: any change on or under `x-ruralz-secret`, and changes of secret destinations, carry `security` |
| OQ-security-and-identity-1 | Which extra `auth.jwt` and `auth.api-key` fields? | (a) incl. per-issuer maximum token lifetime default 24 h (target) | Registered as schema fields with `+ruralz:default` (level 0); materialized in canonical form; affects golden digests of every `auth.jwt` entry, so it MUST land before goldens are frozen for 0.1.0 |
| OQ-performance-budgets-and-benchmarking-6 | Does the config loader adopt W = max(1, `GOMAXPROCS`/2) workers yielding every 100 µs? | (a) Yes, fixed (proposed) | `canonical.Options.Workers` and `Resolver.Resolve(…, workers)` take W from the Node loader; work units are one resource or one Route, well under 100 µs each (target) |

Related, not M1-blocking: OQ-release-versioning-and-compatibility-13 (blocking "for the first digest-changing fix"; proposed (a)); OQ-release-versioning-and-compatibility-12 (M2, renderer takes the level as input: `Options.Level` exists); OQ-configuration-model-13 (auth slot); OQ-configuration-model-19 (open configs, R-18); OQ-release-versioning-and-compatibility-7 (default-change warnings); OQ-control-plane-and-gitops-26 (M2 `Delta` encoding over `ruralz.canonical.v1`). Already decided and implemented as such: OQ-configuration-model-7 (a) and OQ-configuration-model-10 (a) by CLI; OQ-configuration-model-6 by RVC.

## 8. Deferred (M2+, do not build) and extension points

| Deferred item | Milestone | Extension point left in M1 |
|---|---|---|
| Diff sources `rev-<12 hex>`, `sha256:<64 hex>` (REST `GET /api/v1/revisions?digestPrefix=…&environment=…&limit=2`), `oci://REPOSITORY@sha256:…` | M2 | `sourceKind` values exist and exit 2 "Planned (M2)"; `revision.ParseDisplay`; `diff.Side` accepts any canonical document |
| Ruralz Control rendering at pinned schema levels (OQ-release-versioning-and-compatibility-12) | M2 | `canonical.Options.Level` (M1: current level only) |
| Rollout-time skew checks, Node schema levels on the Control Stream | M2 | `schemaidx.Index.Level()`; RZ-CFG-024 decode path; unknown top-level canonical members reserved for a level declaration |
| Control Stream `Snapshot`/`Delta` of canonical content, re-hash on delivery (RZ-CFG-027) | M2 | `Decoder.Verify` |
| `ruralz bundle push` re-render mismatch (RZ-CFG-027), OCI publish, Revision and Plugin signatures (RZ-CFG-033), online Plugin artifact check (RZ-CFG-028 real) | M2 | Online stage hook in build (R-56); signing never touches canonical bytes |
| `ruralz/v1beta1`, conversion webhook, golden-corpus conversion, `ruralz.io/conversion-data` population | M3 | `convert.Converter`, `convert.Registry`, `ConversionDataAnnotation`, hub typed aliases to be replaced by hub-owned types; hub schema generated by schemagen from `internal/config/hub` |
| Mirrored CRDs, CRD status conditions | M2 | None here (translation changes only the group) |
| `ruralz bundle audit`, `export dot`, REST `/api/v1/revisions/{digest}/diff` and `/resources/{kind}/{name}` | M2 | `precedence.Chain`, `diff.Engine`, `canonical.Decoder` reused |
| Plugin execution, `configSchema` handling, `x-ruralz-validations` | M2 | Registry `plugin` row, Phase-scope rule; no configSchema defaults (R-11) |
| onChunk ordering for client-to-upstream chunks (WebSocket), gRPC/GraphQL/AI Route timeout defaults | M3 | `registry.IsResponsePhase` is the single switch |
| Deprecation of real fields or apiVersions (RZ-CFG-025 content) | When announced | `convert.Lifecycle` table |
| `ruralz.canonical.v2`, `ruralz.diff.v2` | Only if ever needed | `canonical.Format`, `diff.Format` constants; decoders reject other formats (RZ-CFG-024) |

## 9. Risks and ambiguities

- R-1 Canonical envelope unspecified. CM says "serialized as RFC 8785 canonical JSON behind the format identifier" without a shape. Resolution: `{"format":"ruralz.canonical.v1","resources":[…]}`; resources without `apiVersion` (otherwise conversion would change the digest, contradicting CM § Hub-and-spoke conversion). Needs a sentence in CM before the first golden freeze.
- R-2 "Every schema default materialized" vs absent objects. Creating absent objects is unsafe (`stateStore` absent means the `RURALZ_STATE_STORE_URL` fallback; `healthCheck.active` presence enables probes; `tls` on an `http` listener). Resolution: defaults only inside present objects (Kubernetes convention); code behavior for an absent object is part of the schema-level contract. Consequence: `admin: {}` and no `admin` differ in digest though they behave alike (harmless; diff shows it).
- R-3 Context-derived defaults (Route/Upstream `timeout`, `perTryTimeout`, `stateStoreTimeout` inherited from the Gateway, method-dependent `retryOn`) cannot be materialized per resource. Resolution: absent in canonical form; the derivation rule is versioned like a default; digest still determines behavior because the inputs are in the Revision.
- R-4 `filterClass` materialization. FP § 8.1 lists `filterClass` among materialized registry defaults; the CM Policy sketch says "plugin only". Resolution: follow the binding pack; materialize for all types; accept an authored value equal to the registry class, reject others (RZ-CFG-005). Needs a CM wording fix.
- R-5 Render does not materialize defaults. Otherwise an older file-mode Node would meet newer default-bearing fields (RZ-CFG-006) and render output would freeze defaults. Not stated in CM; recorded here.
- R-6 Impact classes are annotated on only four fields (`Route.spec.timeout`, `Plugin.spec.image`, `Plugin.spec.capabilities`, `AIModel.spec.candidates`); the CM example relies on unstated rules (e.g. `+ Policy/ratelimit-orders [traffic]`, `- Upstream/legacy-orders [routing]`). Resolution: R-64 to R-66 plus proposed markers: Gateway `listeners` routing, `listeners[].tls`, `trustedProxies`, `admin` and `Listener.proxyProtocol` security, `telemetry` metadata, `limits` and `stateStore` traffic; Upstream `loadBalancing`, `healthCheck`, `retries`, `circuitBreaker`, `timeout` traffic, `tls` security, `ai` ai; Consumer `credentials` security, `quotas` and `tier` traffic, `tags` metadata; AIProvider `baseUrl`, `credentials` ai+security; a registry impact column. Needs CM amendment; RL's marker example (`Route.spec.policies` `security`) conflicts with the CM example (`[traffic]` for a ratelimit attachment) and should drop in favor of the derived rule.
- R-7 `move` semantics and op/resource ordering are unspecified; the CM example's op order (`timeout` before `policies`) matches no natural traversal. Resolution: R-60 to R-62; the resource order reproduces the example; op order differs and golden files pin ours. Ask CM to state the ordering.
- R-8 RFC 8785 loses integers above 2^53, yet `ByteSize` accepts up to about 8 EiB. Resolution: RZ-CFG-005 on integer literals outside ±(2^53−1); recommend schemagen emit `maximum: 9007199254740991` on integer and `ByteSize` definitions so the rendered view reports it earlier.
- R-9 Plugin `configSchema` defaults. Materializing them would be a digest-changing M2 addition. Resolution: never materialized in `ruralz.canonical.v1`; the Plugin applies its own defaults.
- R-10 `/config/dump` body shape unspecified by DP; `encoding/json` would HTML-escape canonical bytes. Resolution: R-73 JCS envelope, written by hand; DP should adopt it.
- R-11 RZ-CFG-024 in file mode for authored YAML cannot be told from a typo (the Node's schema lacks future fields), so it stays RZ-CFG-006 there; RZ-CFG-024 applies to canonical content (LKG, handover, dumps, M2 OCI/Control Stream). A future canonical member declaring a level (only when above 0) is the M2 path; any default change on a field with `x-ruralz-since` above 0 would be misread by re-materialization unless the content records its level, so such defaults should never change.
- R-12 onChunk direction: FP says response Phases run in reverse but does not classify onChunk; M1 orders it as a response Phase (all M1–M3 subscribers act on response chunks). Needs a decision before WebSocket (M3).
- R-13 `authz.cel` lists two Phases; the worked example shows it only in onRequestHeaders. Resolution: one Phase chosen by body reference (R-13a); OPA/Cedar body analysis is M2.
- R-14 Plugin scope rule "Scopes matching those Phases" is ambiguous for mixed Phase sets. Resolution: every Phase must be allowed at the scope (RZ-CFG-020 otherwise).
- R-15 RZ-CFG-018 at Upstream scope: "take no part in slot comparison" vs "one Policy per slot per scope". Resolution: apply within each Upstream list only.
- R-16 Same Policy attached at Gateway and Route: literal rule treats it as a replacement (RZ-CFG-019 when not overridable). Excluding a Policy the Gateway does not attach has no registered code: no diagnostic in M1 (a warning code could be added later).
- R-17 The worked-example Reason column is prose; generated reasons (R-51) differ. Golden files pin the generated text; CM table is illustrative.
- R-18 Effective-layer entries for added or removed Routes are unspecified; listing only Routes on both sides avoids noise but hides a new Route's chain (visible via `render --effective`).
- R-19 `render --output json` "Same resources as JSON" does not say whether the output must be a loadable Bundle file; a JSON array is not one resource per file. Resolution: array for tooling; ask area 1 whether the loader accepts arrays.
- R-20 `ruralz bundle build` runs the online Plugin check unless `--offline`, but M1 has no OCI client. Resolution: RZ-CFG-028 per Plugin; CI for Bundles with Plugins uses `--offline` in M1; the golden corpus builds offline.
- R-21 `jsontext` (stdlib RFC 8785) cannot be used in production code under `go 1.26.0` (vet stdversion); own encoder is required and cross-checked in go1.27-only tests. Divergence risk in shortest-digit generation is covered by the differential fuzz target.
- R-22 Duplicate `set` elements and `map` keys are not rejected by the generated schema (no `uniqueItems`); rejecting in stage G (RZ-CFG-005) is proposed. Recommend schemagen emit `uniqueItems: true` for `set` lists.
- R-23 Flags after positional arguments (`ruralz bundle diff FROM TO --env prod …` in CM and CLI examples) are not supported by `flag.FlagSet.Parse`; the CLI framework must parse interspersed flags.
- R-24 Diff `source` for removed resources in the CM example comes from a Revision side that has no files; M1 takes sources only from Bundle sides. Ruralz Control's recorded Bundle path (M2) could supply it later.
- R-25 Golden freeze timing: OQ-security-and-identity-1 and OQ-traffic-management-and-resilience-5/-6 add defaults that change golden digests; all must land before the corpus is frozen for `0.1.0`, or each needs a `CHANGES` record and schema level after release.
- R-26 The pipeline order puts defaults (G) before reference resolution (H) but `slot` defaults need only the Policy itself, and effective chains (I) need resolved references; no conflict. Duplicate/range checks now surface in stage G, beside schema errors, keeping "every error in one run".
