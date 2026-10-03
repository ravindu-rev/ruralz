# M1 spec, area 1 of 11: Configuration loading pipeline

Reader: the implementer of the Ruralz configuration loader for milestone M1. This spec plus the cited doc sections are sufficient to build the area. Nothing here changes a design doc; where docs are silent or contradict each other, section 9 records the gap and the resolution this spec adopts.

Document abbreviations used in references:

| Abbrev. | File |
|---|---|
| CM | `docs/architecture/02-configuration-model.md` |
| ADR3 | `docs/adr/0003-configuration-format.md` |
| FP | `docs/_meta/foundation-pack.md` (binding for names) |
| SEC | `docs/architecture/08-security-and-identity.md` |
| CLI | `docs/reference/01-cli-and-api-surface.md` |
| RM | `docs/roadmap/01-roadmap-and-milestones.md` |
| RL | `docs/engineering/02-repository-layout-and-conventions.md` |
| TS | `docs/engineering/01-tech-stack-and-libraries.md` |
| TQ | `docs/engineering/03-testing-and-quality-strategy.md` |
| DP | `docs/architecture/03-data-plane.md` |
| SO | `docs/architecture/01-system-overview.md` |
| OBS | `docs/architecture/10-observability.md` |
| PB | `docs/architecture/12-performance-budgets-and-benchmarking.md` |
| TMR | `docs/architecture/09-traffic-management-and-resilience.md` |
| CPG | `docs/architecture/04-control-plane-and-gitops.md` |
| WASM | `docs/architecture/05-wasm-plugin-system.md` |
| FC | `docs/features/01-feature-catalog.md` |

## 1. Scope

Every M1 roadmap item this area implements (RM "M1 Core gateway", M1 scope, Configuration row unless noted), with its defining doc section.

| # | M1 item | Defined in |
|---|---|---|
| S1 | `Gateway`, `Route`, `Upstream`, `Policy`, `Consumer` loaded and validated from a Bundle (the loader also validates `Plugin`, `AIProvider`, `AIModel` structurally, because the golden-corpus example Bundle contains them) | CM "Scope of kinds", "Kind catalog", "Complete annotated example Bundle"; FP §3 |
| S2 | `Environment` for CLI renders, read from `--environments FILE` | CM "Scope of kinds", "Environment"; CLI "Global flags"; FC "Environment overlays and variables" |
| S3 | YAML 1.2 parser (`goccy/go-yaml`) under the restricted profile (RZ-CFG-001 to -004) | CM "Restricted YAML profile"; ADR3 "Decision outcome"; FP §5, §7 |
| S4 | JSON accepted as a strict subset through the same loader; JSON subset fixtures | CM "Format decision"; ADR3 "JSON" row and "Confirmation"; FC "YAML and JSON configuration" |
| S5 | Bundle directory discovery, lexical ordering, `.ruralzignore`, base union and identity (RZ-CFG-008, -016, -017) | CM "Bundle layout and merge", "Discovery and base merge"; FP §5, §8.1; FC "Multi-file Bundles" |
| S6 | Overlay merge `overlays/<env>/` with `x-ruralz-list` strategic merge and `$patch` (RZ-CFG-008, -030) | CM "Overlays"; FP §5 |
| S7 | `${VAR}` substitution (RZ-CFG-010, -011, -013), escapes, re-typing; CLI verbs without `--env` when no Environment exists | CM "Environment substitution"; FP §5; CLI "Global flags" |
| S8 | JSON Schema validation (`santhosh-tekuri/jsonschema/v6`) against the embedded rendered view with nearest-match hints (RZ-CFG-005, -006, -007, -012) | CM "Schema keywords that drive tooling", "Validation and diff semantics", "Error codes"; RL "Schema generation from Go types" |
| S9 | Decoding into `pkg/config/v1alpha1`; schema and registry defaults materialization (input to canonicalization and Revision digest, owned by the canonical area) | CM "Canonical form and Revision"; RL "Presence and defaults"; FP §8.1 |
| S10 | Semantic validation: references (009), gateway rules (016, 017), slots and guardrails (018, 019, 020), `failureMode` (029), Routes (023), composition and body-buffering limits (031, 032), Consumers (035, 036), `https` URLs (037), registered-field checks (005) | CM "Attachment and precedence", "Resolution rules", "Route", "Body buffering and limits", "Registered from feature documents"; FP §8.10, §8.12, §10; SEC "Basic and mTLS schemas" |
| S11 | Loader limits: 64 MiB source, 20,000 resources, 64 nesting levels (RZ-CFG-001) | CM "Restricted YAML profile"; CLI "Bundle verbs in detail"; OQ-configuration-model-18 |
| S12 | `secretRef` providers `env` and `file` resolved on Nodes (RZ-CFG-026), rotation, `RURALZ_SECRET_ROOT` | CM "secretRef"; SEC "Secrets"; FC "Secrets by reference"; RM Security row (redaction and secret leak tests) |
| S13 | Diagnostics with source map, shared by CLI and Node, text and `--output json` | CM "Diagnostics and source map"; CLI "Output formats and exit codes"; FC "Configuration validation and linting" |
| S14 | Loader conformance suite, hostile-input suite, upstream suites (YAML Test Suite, JSON-Schema-Test-Suite), Go test meta-validating both schema files and `examples/`, diagnostics goldens | ADR3 "Confirmation"; TQ "Conformance suites", "Fuzzing", "Golden tests"; RL "Generator rules" |
| S15 | Node-side pipeline gating every Hot Reload and Last-Known-Good boot (loader entry points; activation itself is the lifecycle area) | CM "Validation and diff semantics"; DP "Activation"; FP §8.2 |
| S16 | Loader worker model for PB-7 (W = max(1, `GOMAXPROCS`/2), yield every 100 µs) | PB "Hot Reload stages"; OQ-performance-budgets-and-benchmarking-6 |
| S17 | CEL places enumerated and handed to the CEL checker (RZ-CFG-014, -015 are produced by the CEL area) | CM "CEL expressions and allowed places"; RM CEL row |

Out of this area (consumers of its output): canonical form `ruralz.canonical.v1`, Revision digest and `ruralz.diff.v1` (canonical/diff area); CEL compile and cost estimator (CEL area); snapshot compile, Hot Reload, Last-Known-Good files, `/readyz` (lifecycle area); CLI flag parsing and exit codes (CLI area).

## 2. Normative requirements

HTTP status: every `RZ-CFG` code is a configuration-time diagnostic with no HTTP status (`internal/errcode` registers them with `Status: 0`). No spans are created by this area: pack span names are limited to `ruralz.filter.<name>`, `ruralz.upstream.<name>`, `ruralz.route.match` (FP §2) and loading is off the request path.

### A. Sources, discovery and limits

1. **Sources.** The loader MUST accept: (a) a Bundle directory rooted at `ruralz.yaml`; (b) a single file treated as a one-file Bundle in which that file plays the role of `ruralz.yaml` (a rendered Bundle; CM "Overlays": render output "is itself a valid one-file Bundle"); (c) an `fs.FS` rooted at the Bundle (tests; pushed Bundles in M2). File-mode `ruralzd` passes `RURALZ_CONFIG` when it is a path, which names a rendered Bundle directory (CPG "Node process configuration"; SO "File mode lifecycle"). [CM "Bundle layout and merge"]
2. **Root handling.** The directory path is resolved once with `filepath.EvalSymlinks` at load start and opened with `os.OpenRoot`; every read goes through that `*os.Root`, so an atomic symlink swap during a load cannot mix two trees and nothing outside the root is read. A file symlink resolving inside the root is followed (Kubernetes ConfigMap mounts use `..data` links); one resolving outside, a symlinked directory that is not hidden or ignored, and a non-regular file with a matching extension are each RZ-CFG-001 at that path. [CM "Discovery and base merge": "identical on every OS"; this spec, risk 25]
3. **Base discovery.** The loader reads every regular file whose name ends in `.yaml`, `.yml` or `.json` (case-sensitive; `.YAML` is ignored on every OS) under the root, except: the root-level `overlays/` directory; any path with a segment starting with `.` (hidden); paths matched by the root `.ruralzignore`. `ruralz.yaml` is always read and cannot be ignored. A missing `ruralz.yaml` is RZ-CFG-016 with `file: "ruralz.yaml"` and no line. [CM "Discovery and base merge"]
4. **`.ruralzignore`.** Only the root file is read; gitignore syntax: blank lines and `#` comments skipped (`\#` escapes), trailing unescaped spaces trimmed, `!` negates (`\!` literal), a `/` at the start or in the middle anchors to the root, a trailing `/` matches directories only, `*` and `?` never match `/`, `[...]` classes, `**/` leading, `/**` trailing and `/**/` middle; the last matching pattern wins; a path under an excluded directory cannot be re-included. Matching is case-sensitive on slash-separated root-relative paths. An invalid pattern is RZ-CFG-001 at `.ruralzignore:<line>:<column>`. The same patterns filter overlay discovery. [CM "Bundle layout and merge"]
5. **Ordering.** Files are processed in `strings.Compare` order of their slash-separated root-relative paths (byte order: `a.yaml` sorts before `a/b.yaml`), never in directory-walk order; documents within a file keep stream order. Diagnostic `file` values are these relative paths. [CM "Discovery and base merge"; FP §5]
6. **Limits (RZ-CFG-001).** `MaxSourceBytes` 64 MiB (target): the sum of bytes of every file read for one render (base files plus the selected overlay), checked by `Stat` before each read and enforced with a limited reader; the `--environments` file has its own 64 MiB cap. `MaxResources` 20,000 (target): resources in the rendered Bundle, also enforced as a running count of non-empty base documents and, separately, of overlay documents. `MaxDepth` 64 (target; OQ-configuration-model-18). `MaxDocumentTokens` 1,000,000 per YAML document (hypothesis; see requirement 9 and risk 1). The CLI exposes no flag to raise any limit in M1 (CLI "Bundle verbs in detail", until OQ-control-plane-and-gitops-25); a Node MUST NOT enforce a limit below the CLI default. [CM "Restricted YAML profile"; ADR3 "Restricted profile" row]

### B. Restricted YAML 1.2 profile (`.yaml`, `.yml`)

7. **Byte pre-checks** before tokenizing, each RZ-CFG-001 at the offending position: valid UTF-8 (`utf8.Valid`); no U+FEFF anywhere (a byte order mark at the start or later); only YAML printable characters (TAB, LF, CR, U+0020 to U+007E, U+0085, U+00A0 to U+D7FF, U+E000 to U+FFFD, U+10000 to U+10FFFF). CRLF and CR line breaks are accepted and parse identically to LF. [CM "Restricted YAML profile": non-UTF-8 and byte order marks; FP §5]
8. **Token pass.** Tokens are produced incrementally by `github.com/goccy/go-yaml/scanner.Scanner` (`Init`, then `Scan` until `io.EOF`), the same token stream `lexer.Tokenize` returns, so depth is "counted over `lexer.Tokenize` output before parsing" without materializing the whole file's tokens. The pass rejects, with the token's position:
   - `AnchorType`, `AliasType`, and `MergeKeyType` (a plain `<<` key): RZ-CFG-003;
   - `TagType` other than the YAML 1.2 core tags `!!str`, `!!int`, `!!float`, `!!bool`, `!!null`, `!!map`, `!!seq` (so `!include`, `!env`, `!<...>`, `!!binary`, `!!timestamp`, `!!set`, `!!omap` are rejected): RZ-CFG-004; a `%TAG` directive: RZ-CFG-004;
   - a `%YAML` directive other than `1.2`, an `InvalidType` token, depth above `MaxDepth`, or more than `MaxDocumentTokens` tokens in one document: RZ-CFG-001.

   RZ-CFG-003 and RZ-CFG-004 are collected for the whole file (scanning continues); an RZ-CFG-001 stops scanning that file. A file with any token-pass error is not parsed. [CM "Restricted YAML profile"; ADR3 "Restricted profile" row]
9. **Depth algorithm.** Depth is the number of open collections. A per-document stack holds entries `(kind, column)`: `SequenceStartType`/`MappingStartType` push a flow entry and their end tokens pop it; in block context (no open flow entry) a `SequenceEntryType` at column c, or a `MappingValueType` whose key token starts at column c, first pops block entries with column greater than c and then pushes `(kind, c)` unless the top entry already has that kind and column. Depth is the stack size after each push; `DocumentHeaderType` resets the stack. The document root mapping is depth 1. Tokens are grouped per document (split at `DocumentHeaderType` and `DocumentEndType`) and each group is parsed and released before the next, so peak token memory is one document. [CM "Restricted YAML profile"; OQ-configuration-model-18]
10. **Parse.** Each document group is parsed with `parser.Parse(tokens, 0, parser.AllowDuplicateMapKey())`; duplicate detection is Ruralz code (requirement 11) so both positions are reported. A `*yaml.SyntaxError` (from `errors.As`) is RZ-CFG-001 at `Token.Position` with `GetMessage()` text; a non-scalar mapping key (`? [a, b]`) is RZ-CFG-001. [ADR3 "Libraries" row]
11. **Duplicate keys.** Two keys of one mapping with equal decoded string content are RZ-CFG-002 at the second key, `related` the first; every duplicate is reported. Keys are strings: the plain text as written or the decoded quoted content, never typed (`1:` equals `"1":`). [CM profile table; ADR3 Pros ("the Ruralz AST pass enforces RZ-CFG-002")]
12. **Scalar typing is Ruralz-owned**, from the token `Value`, never from goccy's token type (goccy types `0777` as octal, `1_000` and `0b1` as integers, and `1e3` as a string). Plain scalars follow the YAML 1.2 core schema: null `null|Null|NULL|~` or empty; bool `true|True|TRUE|false|False|FALSE`; int `[-+]?[0-9]+` (decimal, leading zeros allowed: `0777` is 777), `0o[0-7]+`, `0x[0-9a-fA-F]+`; float `[-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?`, `[-+]?\.(inf|Inf|INF)`, `\.(nan|NaN|NAN)`; everything else is a string (`yes`, `on`, `0b1`, `1_000`, `2026-09-23` are strings). Integers are stored as normalized decimal text (`0o17` is `15`, `-0` is `0`); an integer outside int64, `.inf` and `.nan` are RZ-CFG-005 (not representable in the JSON data model). Quoted, literal (`|`) and folded (`>`) scalars are strings, decoded by goccy. A core tag forces its type (`!!str 0777` is `"0777"`); a tagged value that does not match its tag is RZ-CFG-001. [ADR3 "Libraries" row, "Confirmation" loader conformance suite; FP §5]
13. **Documents.** Empty, comment-only and null documents are skipped. A document whose root is not a mapping is RZ-CFG-005 ("a resource must be a mapping"). Multi-document files, flow style and comments are accepted. [CM profile table]
14. **Positions.** Every key and value records file, 1-based line and 1-based column counted in Unicode code points (goccy's `Position.Line` and `Column` are rune-based). [CM "Diagnostics and source map"]

### C. JSON (`.json`)

15. `.json` files use a strict RFC 8259 front end on the standard library `encoding/json/jsontext` (`jsontext.NewDecoder` with `jsontext.AllowDuplicateNames(true)`, so Ruralz reports RZ-CFG-002 with both positions), feeding the same tree and pipeline. Invalid UTF-8, unpaired surrogate escapes, comments, trailing commas, leading zeros, and more than one top-level value are RZ-CFG-001 at the decoder's offset converted to line and column. The single top-level value MUST be an object (else RZ-CFG-005). Numbers keep their exact text; depth is checked while streaming (RZ-CFG-001 above `MaxDepth`). Tabs, `\/` and surrogate-pair escapes (`😀`) MUST produce the same tree as their YAML twins. [CM "Format decision"; ADR3 "JSON" row, "JSON subset fixtures"; risk 3]

### D. Envelope and identity

16. Each document MUST have string `apiVersion`, `kind` and `metadata.name`; a missing one is RZ-CFG-005 at the document start ("missing required field `kind`"). `${` in any of them, or in any mapping key anywhere, is RZ-CFG-011. `apiVersion` MUST be served (M1: `ruralz/v1alpha1`), else RZ-CFG-007 with a hint listing served versions; `ruralz.io/v1alpha1` gets the hint "Bundles use `ruralz/v1alpha1`; `ruralz.io` is the CRD group". `kind` MUST be one of the ten kinds, else RZ-CFG-007 with a nearest-match hint (`Rout` and `route` both hint `Route`). [CM "Resource model", "Kubernetes mapping", "Error codes"]
17. `Environment` or `Cluster` in a base or overlay document is RZ-CFG-017 at `kind`. [CM "Discovery and base merge"; FP §3]
18. Identity is `(kind, metadata.name)`; a duplicate among base documents is RZ-CFG-008 at the second document's `metadata.name`, `related` the first, message naming both files. [CM "Identity and references", "Discovery and base merge"]
19. After overlays, exactly one `Gateway` MUST exist and it MUST come from the root file (`ruralz.yaml`, or the single source file): none is RZ-CFG-016 at `ruralz.yaml:1:1`; each extra Gateway, including one an overlay adds, is RZ-CFG-016 at its `kind`; a Gateway declared in another base file is RZ-CFG-016 at its `kind`. An overlay may patch the Gateway. [CM "Scope of kinds"; FP §3]
20. A document failing requirements 16 to 18 is excluded from later stages, so no cascade diagnostics follow.

### E. Environments for CLI renders

21. `LoadEnvironments(path)` applies requirements 7 to 16 and the same limits to the `--environments` file. Each document MUST be `Environment` or `Cluster` (other kinds: RZ-CFG-005 "not allowed in an --environments file"). Documents are validated against the rendered view; no substitution runs on them (values are inserted literally; substitution is not recursive). Duplicate identities are RZ-CFG-008. `spec.overlay` defaults to `metadata.name` and MUST be one path segment matching `^[A-Za-z0-9][A-Za-z0-9._-]*$` (RZ-CFG-005). The loader SHOULD also report `promotion.from` naming an Environment absent from the file (RZ-CFG-009) and promotion cycles (RZ-CFG-022); `Cluster` documents are schema-checked and otherwise ignored. [CM "Environment", "Error codes"; CLI "Global flags"]
22. **Variable source and overlay selection** (the CLI area wires flags; this area MUST expose the behavior):
    - `--env NAME --environments FILE`: overlay `overlays/<spec.overlay>/` and variables only from that Environment's `spec.variables`, never the process environment; a `NAME` absent from `FILE` is a usage error (CLI exit 2) with a nearest-match hint.
    - `--env` without `--environments`: exit 2 in M1 ("fetching Environments from Ruralz Control is Planned (M2)"; CM table: "with neither source, an error").
    - Neither flag: the CLI process environment snapshot, no overlay (RM "CLI verbs without `--env` when no Environment exists").
    - `--environments` without `--env`: `validate` (and `audit`, M2) run once per Environment in name order, each diagnostic carrying `environment`; other readers exit 2.
    - File-mode `ruralzd`: its process environment, never an overlay (OQ-configuration-model-10, decided by CLI).
    [CM "Environment substitution" renderer table; CLI "Global flags"]

### F. Overlays

23. **Selection.** One overlay at most: `overlays/<overlay>/`. When the directory is absent and `spec.overlay` was defaulted, the overlay is empty; when `spec.overlay` was set explicitly and the directory is absent, the loader SHOULD report RZ-CFG-009 at `spec.overlay` in the `--environments` file (risk 18). Files are discovered and ordered as in requirements 3 to 5. [CM "Overlays"]
24. Overlay documents follow requirements 16 and 17. One identity in two documents of one overlay, in one file or two, is RZ-CFG-008 ("results never depend on file order"). A document patching an existing identity MUST use the base resource's `apiVersion`, else RZ-CFG-030 at the overlay `apiVersion`, `related` the base; an unserved `apiVersion` is RZ-CFG-007 only. [CM "Overlays"]
25. **Merge algorithm** (Kubernetes strategic merge driven by `x-ruralz-list`, read from the base resource's apiVersion view; inside `Policy.spec.config` the type dispatch uses the merged `spec.type`):
    - New identity: added, merged against an empty base (directives consumed); it must validate alone.
    - Annotation `ruralz.io/patch: delete` removes the resource (other overlay content ignored; a missing base is a no-op); `ruralz.io/patch: replace` replaces the base `spec` whole while `metadata` still merges; any other value is RZ-CFG-005. The annotation never survives into the result.
    - Objects merge recursively; an overlay scalar or non-keyed list wins; an overlay `null` removes the member (defaults then apply).
    - `map` list: entries matched by the key field merge recursively; new entries are appended in overlay order. `orderedMap` list: matched entries merge in place (position kept); new entries are appended at the end; when the overlay list's first element is exactly `{$patch: replace}`, the result is the remaining overlay elements in order (this is how an overlay reorders). `set` and `atomic` lists: replaced. A list with no `x-ruralz-list` (open `config` members, Plugin `configSchema`, `plugin` configs): atomic.
    - An entry `{<key>: v, $patch: delete}` in a `map` or `orderedMap` removes that entry (absent key: no-op); such an entry holds only the key and `$patch`.
    - `$patch` in any other place, with any other value, `{$patch: replace}` not first or on a non-`orderedMap` list, or a keyed-list entry without its key field: RZ-CFG-005. `$patch` in a base document: RZ-CFG-006 ("valid only in overlays").
    - Each result node keeps the position of the document that supplied it (base or overlay).
    [CM "Overlays" merge table; FP §5]

### G. Substitution

26. **Grammar.** In a string scalar, `$${` is a literal `${`; `${NAME}` and `${NAME:-default}` with `NAME` matching `[A-Za-z_][A-Za-z0-9_]*` and `default` any run of characters without `}`; a `$` not followed by `{` is literal. Any other `${` (unterminated, `${1X}`, `${X-d}`, `${X:=d}`) and a default containing `${` are RZ-CFG-005 ("malformed substitution; write `$${` for a literal"). [CM "Environment substitution"; authoring-view pattern `^\$\{[A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?\}$`]
27. **When.** Exactly once, after overlay merge and before schema validation, on string scalars of any style, never on keys; not recursive (substituted text is never re-scanned). `:-` treats empty as unset; plain `${NAME}` with `NAME` set to empty yields empty. An undefined `NAME` without default is RZ-CFG-010, one diagnostic per occurrence; when the source is an Environment, the hint names the nearest `spec.variables` key (never process-environment names). [CM "Environment substitution"; FP §5]
28. **Forbidden positions (RZ-CFG-011):** mapping keys; `apiVersion`; `kind`; `metadata.name`; any node whose schema carries `x-ruralz-ref`; the whole subtree of an `x-ruralz-secret` node; any node carrying `x-ruralz-cel`. Only RZ-CFG-011 is reported for such a scalar (not -010 or -012). `$${` is still unescaped there. [CM "Environment substitution", "Allowed places"; RL "Presence and defaults"]
29. **Re-typing.** A scalar that is exactly one expression, in any quoting style, takes the rendered view's type at its path: integer (core int grammar), number (core int or float grammar), boolean (core bool literals), `ByteSize` or `IntOrString` (integer when the text is a core int, else string); string or no schema: string. Text that does not fit stays a string and fails schema validation (RZ-CFG-005, message naming the variable). Partial substitution always yields a string. `port: ${HTTPS_PORT:-8443}` becomes the integer 8443; a value `0777` stays `"0777"` in a string field and becomes 777 in an integer field. [CM "Environment substitution"; ADR3 "Libraries" row, "Confirmation"]
30. RZ-CFG-013 (warning) at each use of a `NAME` matching `(?i)(secret|password|token|apikey)`, telling the author to use `secretRef` with `provider: env`. [CM "Environment substitution"]
31. **Render escaping.** `profile.Encode` writes every literal `${` inside a string as `$${`, whether it came from a `$${` escape, a variable value or a default, so rendering rendered output again yields the same tree and Revision. [CM "Overlays": render is idempotent; TQ "Required properties": Render]

### H. Schema validation

32. Each resource tree, after substitution, is validated against its kind's definition in the rendered view (`api/schema.RenderedV1alpha1()`), compiled once per process with `jsonschema/v6`: draft 2020-12, formats not asserted, `UseLoader` refusing every URL (no file or network access), and a Ruralz vocabulary registered with `AssertVocabs()` that parses the seven `x-ruralz-*` keywords into `Schema.Extensions` (the navigable schema index; see section 3). The authoring view is not used in the pipeline (editors and the meta-validation test use it). [CM "Schema keywords that drive tooling"; RL "Normative artifact"; FP §7 JSON Schema row]
33. **Error mapping.** `additionalProperties` violations: RZ-CFG-006 per unknown member at its key, hint = nearest allowed property (`timout` hints `timeout`), with dedicated messages for `metadata.namespace` ("Bundles have no namespace") and top-level `status` ("only Ruralz Control writes status"). `enum`: RZ-CFG-005 with the nearest enum value (`rateLimit` hints `ratelimit`). `required`: RZ-CFG-005 at the parent object naming the field. `type`, `pattern`, `minimum`/`maximum`, `minItems`, `minProperties`, `maxLength` and the combination keywords generated by `exactlyOneOf` (`oneOf`), `atMostOneOf` (`not`) and `atLeastOneOf` (`anyOf`): RZ-CFG-005 naming the fields ("exactly one of `path`, `pathExpression`"). Any non-conforming value at an `x-ruralz-secret` position (a scalar, or an object with members other than `secretRef`): RZ-CFG-012 instead, and the message MUST NOT contain the value. For `anyOf` branches, the reported cause is the branch matching the instance's JSON type. Duration and ByteSize failures use the CM syntax text ("Go duration such as `50ms`"; "integer or quantity such as `10Mi`"). [CM "Schema keywords that drive tooling", "Error codes", "secretRef"]
34. **Keyed-list and set uniqueness** (not expressible in JSON Schema): two entries with the same key value in a `map` or `orderedMap` list (for example two listeners `name: https`, one Policy twice in `spec.policies`) and two equal elements in a `set` are RZ-CFG-005 at the second, `related` the first. [CM "Schema keywords that drive tooling"; risk 4]
35. **Reserved prefix.** A label key, or an annotation key other than `ruralz.io/conversion-data`, starting with `ruralz.io/` is RZ-CFG-005 (`ruralz.io/patch` is consumed in overlays before validation). [CM "Identity and references": prefix reserved]

### I. Decoding and defaults

36. **Defaults materialization**, after schema validation: for every object present in the instance, each absent property with a schema `default` receives the rendered view's normalized default. Absent parent objects are never created (a Gateway without `stateStore` must stay without it, because the Node then reads `RURALZ_STATE_STORE_URL`, else uses `memory`; CM "Gateway"). Defaults apply inside list items and inside `Policy.spec.config` through the type dispatch. The M1 schema defaults are: `Admin.port` 9901; `Listener.proxyProtocol` false; `ListenerTLS.minVersion` "1.3"; `Limits.maxResponseBodyBytes` 10485760, `maxCompositionSteps` 16, `maxBufferedBytes` 536870912, `maxPluginMemoryBytes` 2147483648; `StateStore.driver` "memory", `topology` "standalone"; `RouteUpstream.weight` 1; `CompositionStep.collection` false, `optional` false; `PolicySpec.overridable` true; `BasicCredential.iterations` 600000; `QuotaConfig.key` "consumer.name"; `RateLimitConfig.localOnly` false; `Transform{Request,Response}Config.contentType` "application/json"; `AuthUpstreamOAuth2Config.timeout` "2s"; `AuthUpstreamSigV4Config.payload` "signed"; `PluginLimits.memoryBytes` 16777216, `timeout` "5ms". Registry defaults are materialized on every Policy: `failureMode` (registry default), `slot` (registry slot, or `metadata.name` where the registry says `name`) and, for `type: plugin` only, `filterClass: custom`. Materialized nodes are marked defaulted (diagnostics on them point at the parent). Materialization is idempotent. [CM "Canonical form and Revision", "Policy"; RL "Presence and defaults"; FP §8.1, §10]
37. Numbers at integer-typed positions are normalized to integer text (`1.0` becomes `1`, `1e3` becomes `1000`) before decoding.
38. **Decode.** The materialized tree decodes into the `pkg/config/v1alpha1` kind structs (`encoding/json`); a decode failure (for example an out-of-range `int32`) is RZ-CFG-005 at the field path. `Policy.spec.config` is also decoded into the registry's config type. In M1 the hub representation is the v1alpha1 types (type aliases) and the conversion registry holds only the identity conversion. [CM "Hub-and-spoke conversion"; RL "Schema generation from Go types"]

### J. Semantic validation

39. Per-resource and cross-resource checks, each reported at the listed position (`related` in parentheses):

| Code | Rule | Position | Source |
|---|---|---|---|
| 009 | Every `x-ruralz-ref` value names an existing resource of the keyword's kind; `Route.spec.listeners` entries name Gateway listeners; `quota` and `ai.token-budget` `config.consumerQuota` is defined by some Consumer quota with `unit: requests`, resp. `tokens`. Missing target: hint = nearest name of that kind; a resource of another kind with that name: "names a Policy, not an Upstream" | the reference value | CM "Identity and references", "Policy" |
| 016, 017, 008 | Requirements 17 to 19 | as stated | CM |
| 018 | Within one attaching list (`Gateway.spec.policies`, one `Route.spec.policies`, one `Upstream.spec.policies`), two distinct Policies with the same effective slot | second ref (first ref) | CM "Resolution rules" 2; FP §8.12 |
| 019 | A Route `excludePolicies` entry naming a Gateway-attached Policy with `overridable: false`; a Route Policy whose slot equals that of a remaining Gateway Policy with `overridable: false` (the same Policy on both lists is not a replacement) | Route ref (Policy declaration, e.g. `policies/common.yaml:46:3`) | CM "Resolution rules" 3; FP §8.12 |
| 020 | The type's registry Scopes (G, R, U) exclude the attaching scope; a `plugin` Policy whose Plugin `phases` are not all allowed at the scope: client Phases `onRequestHeaders`, `onRequestBody`, `onRoute`, `onResponse`, `onLog`, `onChunk` at G and R; upstream-leg Phases `onUpstreamRequest`, `onUpstreamResponseHeaders`, `onUpstreamResponseBody`, `onChunk` at U | the PolicyRef | CM "Policy" registry, "Resolution rules" 4; WASM "phases fixes scope" |
| 029 | `failureMode: open` on `auth.*` (including `auth.upstream-*`), `authz.*`, or `plugin` with `filterClass` `auth` or `authz` | `failureMode` value | CM "Policy"; FP §8.10 |
| 023 | Two Routes with identical match criteria: equal effective listener set (default: all Gateway listeners) and equal normalized `match` (hosts lower-cased and sorted, methods sorted, header names lower-cased and sorted, `path`, `grpc`, `graphql`, `topic` as written, `when` trimmed) | later Route by name (earlier Route) | CM "Route"; DP "Precedence" |
| 031 | `len(composition.steps)` above Gateway `limits.maxCompositionSteps` (default 16) | `spec.composition.steps` | CM "Route" |
| 032 | Sum of explicit step `maxBodyBytes` above `maxResponseBodyBytes` (default 10 MiB), or a step without `maxBodyBytes` whose share `floor((max - explicit sum) / unset count)` is 0 | the step or `steps` | CM "Body buffering and limits" |
| 035 | One `credentials.basic` `username`, or one `credentials.certificates` `subject` or `uriSan`, in two Consumers | second Consumer's value (first) | CM "Registered from feature documents"; SEC "Basic and mTLS schemas" |
| 036 | `credentials.basic[].iterations` outside 600,000 to 1,000,000 | the value | same |
| 037 | `auth.jwt` `config.issuers[].jwksUrl` or `auth.upstream-oauth2` `config.tokenUrl` not an absolute URL with scheme `https` (case-insensitive) and a non-empty host | the value | same; SEC "JWT and OIDC" |
| 021 (SHOULD in M1) | `Plugin.spec.image` without `@sha256:<64 lower-case hex>` suffix; a `plugin` Policy `config` failing its Plugin's `configSchema` (compiled with jsonschema/v6, no remote refs; `x-ruralz-validations` rules are M2) | image value; config path | CM "Plugin", "Error codes" |
| 034 (SHOULD in M1) | A Route whose effective chain holds `ai.token-budget` reaches an `ai` Upstream whose `ai.models` include an `AIModel` without `limits.maxOutputTokens` | the AIModel | CM "Policy"; FP §8.9 |
| 022 (SHOULD) | Requirement 21 | `promotion.from` | CM "Environment" |
| 005 | Conditional rules the schema cannot express: an `https` listener needs non-empty `tls.certificates`; `http3` only on `https`; `trustedProxies` entries parse with `netip.ParsePrefix` (malformed CIDR); Route `match` has at least one non-empty criterion; a `match.hosts` entry may start with one `*.` label and holds no other `*` (OQ-data-plane-2); `match.path.regex` and `match.headers[].regex` compile with `regexp`; Upstream `endpoints` or `discovery` required unless `protocol: ai`, `ai` only and required for `protocol: ai`, `messaging` only for `kafka`, `nats`, `mqtt`, `loadBalancing.algorithm: ring-hash` requires `hashKey`; `plugin` required exactly when `type: plugin`, `filterClass` only for `plugin`; `authz.ip` and `authz.geoip` with `allow` and `deny` both empty, `authz.ip` entries parse as CIDR or address; `ratelimit` `limits[].perNodeCeiling` in 1 to `requests`, `burst` in 0 to `requests`; `transform.*` `replace[].pattern` over 1,024 bytes or (unless `literal: true`) not valid RE2, or more than 32 entries across `set`, `remove`, `arrayOps`, `replace`; Consumer `credentials` with every list empty; `secretRef` `file` names an absolute, clean slash path (`path.IsAbs`, `path.Clean(name) == name`) and `env` names match `[A-Za-z_][A-Za-z0-9_]*`; AIProvider `credentials` required except `ollama`, `baseUrl` required for `ollama` (SHOULD) | the value or object | CM "Gateway", "Route", "Upstream", "Policy", "Consumer", "AIProvider", "Registered from feature documents", "secretRef" |
| 013 (warning) | Credential-shaped literal in any string outside `x-ruralz-secret`, CEL strings included, by the versioned detector list: PEM `-----BEGIN [A-Z ]*PRIVATE KEY-----`; URL userinfo with a password `^[a-zA-Z][a-zA-Z0-9+.-]*://[^/@:\s]+:[^/@\s]+@`; JWT `eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`; `(AKIA\|ASIA)[0-9A-Z]{16}`; `gh[pousr]_[A-Za-z0-9]{36,}`; `github_pat_[A-Za-z0-9_]{22,}`; `xox[abposr]-[A-Za-z0-9-]{10,}`; `sk-(ant-)?[A-Za-z0-9_-]{20,}`; `(Bearer\|Basic) [A-Za-z0-9._~+/-]{16,}=*`; a `headers` `set[].value` literal for header names `authorization`, `proxy-authorization`, `cookie`, `x-api-key`. The message names the detector, never the value | the value | CM "secretRef"; risk 35 |

    [Gating rule: see requirement 51.]
40. The effective slot is `spec.slot` if set, else the registry slot (`auth` for the four client auth types, `validation`, `cors`, `cache`, `upstream-auth`, `semantic-cache`), else `metadata.name`. The effective chain per Route (Gateway policies minus `excludePolicies`, plus Route policies replacing same-slot Gateway policies, ordered by Filter class `cors, auth, authz, admission, validation, cache, upstream-auth, transform, custom`, then scope G, R, U, then list position) and one leg set per Upstream are computed here and exported (section 3); execution order of response Phases is the Filter Chain area's. [CM "Resolution rules" 1 to 5; FP §8.12]

### K. CEL

41. Every node carrying `x-ruralz-cel`, including materialized defaults (`quota` `config.key` "consumer.name"), is passed with its `{variables, result}` spec to the CEL area's checker; returned issues become RZ-CFG-014 or -015 at the scalar (column offset added for single-line plain and double-quoted scalars, else the scalar start). CEL runs for every resource that passed schema validation. `x-ruralz-validations` rules are not evaluated in M1. [CM "CEL expressions and allowed places", "Limits"]

### L. Secrets

42. **Offline.** The CLI and Ruralz Control never resolve secrets; offline validation checks only the static rules of requirement 39 (RZ-CFG-005) and RZ-CFG-011/-012. `provider: kubernetes` and `vault` validate offline. [SEC "Secrets" rule 1; CM "secretRef"]
43. **Collection.** The loader returns every `secretRef` use (reference, resource, key-aware path) found at `x-ruralz-secret` positions, for the Node's resolver. [CM "secretRef"]
44. **Node resolution** (`internal/secret`), before a Revision is activated; every failing use is reported (not only the first), each RZ-CFG-026 naming provider, reference and path but never a value:
    - `env`: `name` MUST be `RURALZ_STATE_STORE_URL` or start with `RURALZ_SECRET_` (OQ-security-and-identity-22, adopted), else 026; unset or empty is 026; `key` is ignored.
    - `file`: `name` MUST lie under `RURALZ_SECRET_ROOT` (default `/etc/ruralz`), read through an `os.Root` opened on the root so `..` and symlinks cannot escape (else 026); at most 4 MiB (proposed target, risk 24); with `key`, the file MUST be a JSON object whose member `key` is a JSON string, whose UTF-8 bytes are the value; without `key`, the exact file bytes (no trimming).
    - `kubernetes`, `vault`: 026 "provider Planned (M2), not supported by this Node".
    - A use may carry a consumer-supplied check (PEM parse, State Store URL fits `topology`, API key at least 22 base64url characters); a failed check is 026.
    [CM "secretRef"; SEC "Secrets" rules 3 and 5; OQ-scalability-and-distributed-state-2; SEC "Consumers and tiers"]
45. **Startup refusal.** `NewResolver` MUST fail (process exits 2 with a message naming the setting; no RZ code) when `RURALZ_SECRET_ROOT` is relative, or when `${RURALZ_DATA_DIR}`, `RURALZ_ADMIN_TLS_DIR`, the admin token file (`RURALZ_ADMIN_TOKEN_FILE`, OQ-security-and-identity-7), the Enrollment token file (M2) or the Vault credential (M2) is the root or inside it (compared after `filepath.Clean` and `EvalSymlinks` where the path exists). [SEC "Secrets" rule 5]
46. **Table and rotation.** Resolved values live only in memory in an immutable table keyed by `(provider, name, key)`, published by `atomic.Pointer`; unchanged references keep their values across Hot Reloads (no re-read). `file` references are polled (stat, then re-read on change of size, modification time or inode) every 2 s (proposed target); a changed value that fails its check keeps the last value, increments `ruralz_config_secret_rotation_failures_total{provider="file"}` and logs a warning; `env` never rotates (restart). Resolved values are never written to disk, logs, traces, metric labels, diagnostics, `/config/dump` or `/tap`. [CM "secretRef"; SEC "Secrets" rules 2 and 4, Figure 3; OBS "Ruralz Gateway metrics"; OQ-configuration-model-15 option (a)]

### M. Diagnostics

47. **Model.** A diagnostic has `code` (registered `RZ-CFG-NNN`), `severity` (`error`, or `warning` for RZ-CFG-013 and -025), optional `file`, `line`, `column` (1-based, code points), `resource` `{kind, name}`, key-aware `path`, `message`, optional `hint`, optional `related` locations, optional `environment`. Warnings never change a verb's exit code. [CM "Diagnostics and source map"; CLI "Exit codes"]
48. **Paths.** Human form: fields joined by `.` (a field not matching `[A-Za-z_$][A-Za-z0-9_$-]*` is written `["<json string>"]`); `map` and `orderedMap` entries `[<keyField>=<value>]`; `set` elements `[item=<value>]`; `atomic` entries `[<index>]`; values not matching `[A-Za-z0-9_./:@*-]+` are JSON-quoted. JSON form: an array of strings (fields), integers (atomic index) and single-member objects (`{"name": "stock"}`, `{"item": "request.body.read"}`). [CM "Diagnostics and source map"]
49. **Text form** (one line per diagnostic): `<file>:<line>:<column> <severity> <code> <Kind>/<name> <path>: <message>`, then ` (<hint>)` when a hint exists, then ` (<related message> <file>:<line>:<column>)` per related location, then ` [environment=<name>]` when set; absent parts are omitted (`<file>` alone when line is unknown; `-` when no file). Messages are single-line (newlines escaped as `\n`). Example: `routes/cart-grpc.yaml:11:7 error RZ-CFG-019 Route/cart-grpc spec.excludePolicies[name=ratelimit-global]: cannot exclude a Policy with overridable: false (declared in policies/common.yaml:46:3)`. [CM "Diagnostics and source map"; CLI "Output formats"]
50. **JSON form:** a bare array (`[]` when clean; OQ-cli-and-api-surface-1 option (a)) of objects with members in the order `code, severity, file, line, column, resource, path, message, hint, related, environment`, omitting empty ones; `related` items are `{file, line, column, message}`. Diagnostics are sorted by `(environment, file, line, column, code, path, message)` so `ruralz`, `ruralz-control` and `ruralzd` emit identical output for the same stages, whatever `GOMAXPROCS`, OS or worker count. At most 10,000 diagnostics per run (proposed target); on reaching it the pipeline stops and appends one RZ-CFG-001 "too many diagnostics" error. [CM "Validation and diff semantics": identical diagnostics; TQ "Conformance suites"]

### N. Pipeline, gating, concurrency, performance

51. **Stages and gating** (Figure 3 of CM): discovery → token pass and parse per file → envelope, identity, Gateway placement → overlay parse and merge → substitution → schema validation → defaults and decode → per-resource semantics → cross-resource semantics → CEL. Every per-file and per-resource stage runs for every file or resource that passed the previous stage, so all such errors surface in one run. Cross-resource checks (009, 018, 019, 020, 023, 031, 032, 034, 035, config against Plugin `configSchema`) run only when every file parsed, every identity is valid and every resource decoded; otherwise they are skipped (no cascades). `Load` returns a typed Bundle only when no error diagnostic exists. [CM "Validation and diff semantics": "report every error in one run"]
52. **Entry points.** `Load` (source Bundle or rendered directory or file; CLI and file-mode Node); `LoadEnvironments`; `FromResources` for already-rendered resources (Last-Known-Good boot from the canonical source in M1, Control Stream in M2), which skips parse, overlay and substitution and runs schema validation onward ("re-runs the checks from reference resolution onward" plus cheap re-validation). [CM "Validation and diff semantics"; DP "Activation"; FP §8.2]
53. **Concurrency.** Stages run in order; inside a stage a bounded pool of W goroutines processes files or resources, created and joined within `Load` (no goroutine outlives the call). Default W: CLI `GOMAXPROCS`; Node `max(1, GOMAXPROCS/2)` (OQ-performance-budgets-and-benchmarking-6 option (a)). Each worker calls `runtime.Gosched()` once at least 100 µs of work have passed since its last yield (checked between units and every 256 nodes inside a unit). File parsing also bounds in-flight source bytes to `max(16 MiB, largest file)`. `ctx` is checked between units; cancellation returns `ctx.Err()` and no partial result. [PB "Hot Reload stages"; RL "Code conventions": bounded goroutines, context first]
54. **Performance targets.** Validating 10,000 resources in under 2 s on a four-core laptop and 20,000 in under 4 s (target; CM "Validation and diff semantics"); decode and re-validate about 40 µs per resource on a Node (hypothesis; PB "Hot Reload stages", stage `compile`). Peak loader memory for the 64 MiB limit is measured by the hostile-input fixtures (OQ-configuration-model-18).
55. **Purity.** `internal/config/...` packages never log, never read the process environment, filesystem outside the given source, network or clock (except the yield timer), keep no mutable package-level state and use no `init()`; the caller injects variables (`os.Environ()` snapshot), limits, compiled schemas and checkers. [RL "Code conventions"]

### O. Telemetry

56. The Node's lifecycle code (not this area) records `ruralz_config_activations_total{result, code}` with `code` = the first error diagnostic's code after sorting and `ruralz_config_activation_duration_seconds{stage="compile"}` using `Result.Stats`; it logs each error diagnostic once via `internal/telemetry` `slog` at `error` level (warnings at `warn`) with keys `code`, `file`, `line`, `column`, `resourceKind`, `resourceName`, `path`, `message`. `internal/secret` records `ruralz_config_secret_rotation_failures_total{provider}` (series for `env` and `file` pre-created at 0) and logs rotation failures at `warn` with `provider` and the reference, never the value. [OBS "Ruralz Gateway metrics", "Process logs", "Alert rules"]

## 3. Proposed Go packages and API

All new code under `internal/`; package names singular; every file carries the Revington Apache-2.0 header. `internal/config/*` packages link into all three binaries (RL "Where Go code goes": shared validation library).

| Package | Responsibility | Third-party import |
|---|---|---|
| `internal/config/diag` | Diagnostic model, key-aware paths, text and JSON writers, sorting, cap, nearest-match hints | none |
| `internal/config/tree` | Value tree with positions, file table, raw resource and identity types, JSON conversion | none |
| `internal/config/profile` | Byte pre-checks, token pass, YAML parse and typing, JSON front end, restricted-profile YAML encoder | `github.com/goccy/go-yaml` (only here) |
| `internal/config/schemaview` | Compile the rendered view with the `x-ruralz-*` vocabulary, navigate schema by instance path, validate, map errors | `github.com/santhosh-tekuri/jsonschema/v6` (only here and, later, `internal/filter/validation/jsonschema`) |
| `internal/config/overlay` | Strategic merge and `$patch` | none |
| `internal/config/subst` | `${VAR}` scan, substitution, re-typing, escaping | none |
| `internal/config/registry` | Policy type registry table (FP §10) and slot and `failureMode` rules | none |
| `internal/config/defaults` | Schema and registry defaults materialization | none |
| `internal/config/validate` | Per-resource and cross-resource semantic checks, effective chains, CEL hook | none |
| `internal/config/hub` | Typed validated Bundle (M1: v1alpha1 aliases), conversion registry | none |
| `internal/config/loader` | Sources, discovery, `.ruralzignore`, limits, Environments, stage orchestration | none |
| `internal/secret` | `secretRef` resolver, providers `env` and `file`, table, rotation poller | none |

depguard additions to `.golangci.yml`: admit `github.com/goccy/go-yaml` and `github.com/santhosh-tekuri/jsonschema/v6` in the strict `admitted` list; confine goccy to `**/internal/config/profile/**`; confine jsonschema to `**/internal/config/schemaview/**` and `**/internal/filter/validation/jsonschema/**`; deny `github.com/ravindu-rev/ruralz/internal/secret` in `**/internal/cli/**` and `**/internal/control/**` (SEC rule 1: only Nodes resolve). `internal/secret/` SHOULD join the security-sensitive package list (RL "Pull request rules").

```go
// Package diag: shared by CLI, Ruralz Control (M2) and Node.
package diag

type Severity uint8

const (
	SeverityError Severity = iota + 1
	SeverityWarning
)

type ResourceID struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

type Location struct {
	File   string // slash path relative to the Bundle root, or as given for --environments
	Line   int    // 0 = unknown
	Column int    // code points; 0 = unknown
}

// PathElem is a field (Field != ""), a keyed entry (KeyField, Value), a set element
// (KeyField == "item") or an atomic index (Index >= 0).
type PathElem struct {
	Field    string
	KeyField string
	Value    string
	Index    int
}
type Path []PathElem

func (p Path) String() string               // spec.composition.steps[name=stock].upstream
func (p Path) MarshalJSON() ([]byte, error) // ["spec","composition","steps",{"name":"stock"},"upstream"]

type Related struct {
	Location
	Message string // "declared in"
}

type Diagnostic struct {
	Code        string // registered RZ-CFG-NNN
	Severity    Severity
	Location
	Resource    *ResourceID
	Path        Path
	Message     string
	Hint        string
	Related     []Related
	Environment string
}

type List []Diagnostic

func (l List) HasErrors() bool
func (l List) Sort()                   // (environment, file, line, column, code, path, message)
func (l List) FirstErrorCode() string  // after Sort
func WriteText(w io.Writer, l List) error
func WriteJSON(w io.Writer, l List) error

// Collector is safe for concurrent Add; it enforces the diagnostic cap.
type Collector struct{ /* mutex, list, max */ }

func NewCollector(max int) *Collector
func (c *Collector) Add(d Diagnostic) (accepted bool)
func (c *Collector) Full() bool
func (c *Collector) List() List // sorted copy

// Nearest returns the best candidate by Damerau-Levenshtein (optimal string alignment):
// exact case-insensitive match first, else distance <= max(1, min(3, len(input)/3)),
// ties broken by byte order; at most 64 candidate comparisons per call after a
// length prefilter of |len(c)-len(input)| <= 3.
func Nearest(input string, candidates []string) (string, bool)
```

```go
// Package tree: the loader's only in-memory document model.
package tree

type FileID uint32
type Role uint8 // RoleBase, RoleOverlay, RoleEnvironments, RoleCanonical

type File struct {
	Path string
	Role Role
}
type FileTable struct{ /* append-only */ }

func (t *FileTable) Add(f File) FileID
func (t *FileTable) File(id FileID) File

type Pos struct {
	File   FileID
	Line   int32
	Column int32
}

type Kind uint8 // Null, Bool, Int, Float, String, Map, List
type Style uint8 // Plain, SingleQuoted, DoubleQuoted, Literal, Folded, JSON, Defaulted
type Node struct {
	Kind    Kind
	Style   Style
	Pos     Pos
	Text    string // string value; normalized decimal for Int; original text for Float
	Bool    bool
	Members []Member // Map, authored order
	Items   []*Node  // List
	Vars    []string // variables substituted into this scalar
}
type Member struct {
	Key    string
	KeyPos Pos
	Value  *Node
}

func (n *Node) Get(key string) (*Node, bool)
func (n *Node) Clone() *Node
func (n *Node) JSONValue() any                 // map[string]any, []any, json.Number, string, bool, nil
func (n *Node) AtInstanceLocation(loc []string) (*Node, bool) // jsonschema InstanceLocation

type ID struct {
	Kind v1alpha1.Kind
	Name string
}
type Resource struct {
	ID         ID
	APIVersion string
	Root       *Node // envelope mapping
	Start      Pos   // document start in the declaring file
}
```

```go
// Package profile: restricted YAML 1.2 and JSON.
package profile

type Format uint8 // FormatYAML, FormatJSON (by extension)

type Limits struct {
	MaxDepth          int // 64
	MaxDocumentTokens int // 1_000_000
}

type Document struct {
	Root  *tree.Node
	Start tree.Pos
}

// Parse applies requirements 7 to 15. Diagnostics carry tree positions resolved by the caller.
func Parse(ctx context.Context, src []byte, file tree.FileID, path string, f Format, lim Limits) ([]Document, diag.List)

// Encode writes resources as a restricted-profile YAML stream (block style, two-space
// indent, "---" separators, every string double-quoted with JSON escapes, "${" written
// as "$${", authored key order); Parse(Encode(x)) == x.
func Encode(w io.Writer, docs []*tree.Node) error
```

```go
// Package schemaview: the compiled rendered view and its x-ruralz-* keywords.
package schemaview

type ListType uint8 // ListNone, ListMap, ListOrderedMap, ListSet, ListAtomic

type CELSpec struct {
	Variables []string
	Result    string // "bool" | "string" | "dyn"
}

type Keywords struct {
	List        ListType
	ListKey     string
	Ref         v1alpha1.Kind // x-ruralz-ref; "" when absent
	Secret      bool
	CEL         *CELSpec
	Impact      []string
	Since       int
	Validations []Validation // parsed, not evaluated in M1
}

type Info struct {
	Keywords
	Known      bool     // a schema exists at this path (false inside open config members)
	Types      TypeSet  // object, array, string, integer, number, boolean
	Properties []string // allowed member names (for hints), sorted
	Enum       []string
	Default    *tree.Node // for this property when absent
	Closed     bool       // additionalProperties: false
}

type View struct{ /* compiled *jsonschema.Schema per kind; immutable */ }
type Set struct{ /* views by apiVersion; immutable */ }

func Compile(rendered []byte, apiVersion string) (*View, error)
func NewSet(views ...*View) *Set
func (s *Set) View(apiVersion string) (*View, bool)
func (s *Set) Served() []string // for `ruralz version` apiVersions

// Lookup resolves $ref, allOf, if/then (evaluated on the instance), anyOf (merged).
func (v *View) Lookup(kind v1alpha1.Kind, res *tree.Node, path diag.Path) (Info, bool)
func (v *View) Walk(kind v1alpha1.Kind, res *tree.Node,
	fn func(path diag.Path, n *tree.Node, info Info) error) error
func (v *View) Validate(kind v1alpha1.Kind, res *tree.Node) diag.List // requirement 33, positions from res
```

```go
package overlay

// Apply merges patches (one overlay) into base; diagnostics per requirements 23 to 25.
func Apply(base map[tree.ID]*tree.Resource, patches []*tree.Resource, views *schemaview.Set) diag.List

package subst

type Source interface{ Lookup(name string) (string, bool) }
type Map map[string]string // Environment.spec.variables

func Environ(environ []string) Map // from an os.Environ() snapshot taken by cmd/
func Apply(res *tree.Resource, view *schemaview.View, src Source, hintNames []string) diag.List
func Escape(s string) string // "${" -> "$${"

package defaults

func Materialize(res *tree.Resource, view *schemaview.View, reg *registry.Registry)
```

```go
package registry

type Scope uint8 // bit set: ScopeGateway | ScopeRoute | ScopeUpstream

type TypeInfo struct {
	Type           v1alpha1.PolicyType
	Class          v1alpha1.FilterClass // "" for plugin (from filterClass)
	Phases         []v1alpha1.Phase     // at G and R
	UpstreamPhases []v1alpha1.Phase     // at U
	Scopes         Scope
	Slot           string // "" means the Policy's own name
	DefaultFailure v1alpha1.FailureMode
	ClosedOnly     bool
	Milestone      string // "M1", "M2", "M3"
	NewConfig      func() any
}

type Registry struct{ /* explicit table built by New; immutable */ }

func New() *Registry
func (r *Registry) Lookup(t v1alpha1.PolicyType) (TypeInfo, bool)
func (r *Registry) ClassOrder() []v1alpha1.FilterClass
func EffectiveSlot(p *v1alpha1.Policy, ti TypeInfo) string
func ClosedOnly(p *v1alpha1.Policy, ti TypeInfo) bool // includes plugin with auth or authz class
```

```go
package hub

type Scope uint8 // Gateway, Route, Upstream

type Attached struct {
	Policy   *v1alpha1.Policy
	Scope    Scope
	Class    v1alpha1.FilterClass
	Slot     string
	Position int
	Ref      diag.Location // the PolicyRef position
}

type EffectiveChain struct {
	Route  string
	Client []Attached            // class, scope, position order
	Legs   map[string][]Attached // per Upstream name
}

type SecretUse struct {
	Ref      v1alpha1.SecretRef
	Resource diag.ResourceID
	Path     diag.Path
}

type CELUse struct {
	Expr     string
	Spec     schemaview.CELSpec
	Resource diag.ResourceID
	Path     diag.Path
	Loc      diag.Location
}

type Resource struct {
	ID         tree.ID
	APIVersion string
	Object     any        // *v1alpha1.Gateway, *v1alpha1.Route, ...
	Tree       *tree.Node // materialized, for the canonical encoder
	Source     diag.Location
}

// Bundle is the validated, typed input to canonicalization and snapshot compile.
type Bundle struct {
	Gateway    *v1alpha1.Gateway
	Resources  []*Resource // canonical kind order, then name
	Effective  map[string]*EffectiveChain
	SecretUses []SecretUse
	CELUses    []CELUse
}

func (b *Bundle) Lookup(kind v1alpha1.Kind, name string) (*Resource, bool)
func (b *Bundle) Routes() []*v1alpha1.Route // likewise Upstreams, Policies, Consumers, ...
```

```go
package validate

type CELIssue struct {
	Code    string // RZ-CFG-014 or RZ-CFG-015
	Message string
	Offset  int // byte offset in Expr; -1 unknown
}

// CELChecker is implemented by the CEL area (cel.dev/cel-go); safe for concurrent use.
type CELChecker interface {
	Check(expr string, spec schemaview.CELSpec) []CELIssue
}

// TemplateChecker is implemented by the Router area (path template grammar).
type TemplateChecker interface{ CheckTemplate(template string) error }

type Checkers struct {
	CEL      CELChecker      // required outside unit tests
	Template TemplateChecker // optional; nil skips
}

func Resources(ctx context.Context, res []*tree.Resource, views *schemaview.Set,
	reg *registry.Registry, chk Checkers, opts Options) (*hub.Bundle, diag.List)
```

```go
package loader

type Source struct {
	Dir  string // Bundle root containing ruralz.yaml
	File string // one-file (rendered) Bundle
	FS   fs.FS  // rooted at the Bundle; tests and M2 pushed Bundles
}

type Limits struct {
	MaxSourceBytes    int64 // 64 << 20
	MaxResources      int   // 20_000
	MaxDepth          int   // 64
	MaxDocumentTokens int   // 1_000_000
	MaxDiagnostics    int   // 10_000
}

func DefaultLimits() Limits

type Options struct {
	Environment *v1alpha1.Environment // nil: no overlay
	Variables   subst.Source          // Environment.spec.variables or process environment
	HintNames   []string              // variable names eligible for RZ-CFG-010 hints
	Limits      Limits
	Workers     int           // 0: caller must set (CLI GOMAXPROCS; Node max(1, GOMAXPROCS/2))
	Yield       time.Duration // 100 * time.Microsecond
	Schemas     *schemaview.Set
	Registry    *registry.Registry
	Checkers    validate.Checkers
}

type Stats struct {
	Files, Resources, Routes int
	Parse, Merge, Substitute, Schema, Decode, Semantic, CEL time.Duration
}

type Result struct {
	Files       *tree.FileTable
	Rendered    []*tree.Resource // after overlay and substitution, for `ruralz bundle render`
	Bundle      *hub.Bundle      // nil when Diagnostics has an error
	Diagnostics diag.List        // sorted, positions resolved
	Stats       Stats
}

// Load returns an error only for unusable input (unreadable root, ctx cancelled);
// configuration problems are diagnostics.
func Load(ctx context.Context, src Source, opts Options) (*Result, error)
func FromResources(ctx context.Context, res []*tree.Resource, opts Options) (*Result, error)

type Environments struct{ /* by name; immutable */ }

func LoadEnvironments(ctx context.Context, path string, schemas *schemaview.Set, lim Limits) (*Environments, diag.List, error)
func (e *Environments) Get(name string) (*v1alpha1.Environment, bool)
func (e *Environments) Names() []string // sorted
func OverlayName(env *v1alpha1.Environment) string
```

```go
package secret

type Ref struct {
	Provider v1alpha1.SecretProvider
	Name     string
	Key      string
}

func (r Ref) String() string // "file:/etc/ruralz/tls/tls.key" or "env:RURALZ_SECRET_X"; never a value

// Value prints as "[redacted]" through String, GoString, Format, MarshalJSON and slog.LogValuer.
type Value struct{ b []byte }

func (v Value) Bytes() []byte // copy

type Use struct {
	Ref      Ref
	Resource diag.ResourceID
	Path     diag.Path
	Check    func([]byte) error // consumer validation; failure is RZ-CFG-026
}

type Counter interface{ Inc(provider string) } // ruralz_config_secret_rotation_failures_total

type Config struct {
	Root         string // RURALZ_SECRET_ROOT; default "/etc/ruralz"
	LookupEnv    func(string) (string, bool)
	Protected    map[string]string // setting name -> path (RURALZ_DATA_DIR, RURALZ_ADMIN_TLS_DIR, ...)
	PollInterval time.Duration     // 2 * time.Second
	MaxFileBytes int64             // 4 << 20
	Logger       *slog.Logger      // from internal/telemetry
	Failures     Counter
}

type Table struct{ /* immutable */ }

func (t *Table) Get(r Ref) (Value, bool)

type Resolver struct{ /* atomic.Pointer[Table], providers table, poll set */ }

func NewResolver(cfg Config) (*Resolver, error) // requirement 45
func (r *Resolver) Resolve(ctx context.Context, uses []Use) (*Table, diag.List)
func (r *Resolver) Activate(t *Table) // becomes Current and defines the poll set
func (r *Resolver) Current() *Table
func (r *Resolver) Subscribe(fn func(Ref, Value)) (cancel func())
func (r *Resolver) Run(ctx context.Context) error // poller; owner cancels and waits

// Provider is the M2 extension point (kubernetes, vault).
type Provider interface {
	Resolve(ctx context.Context, r Ref) ([]byte, error)
}
```

Concurrency model: `schemaview.Set`, `registry.Registry` and compiled checkers are immutable after construction and shared by all goroutines; `Load` owns its worker goroutines per stage (requirement 53); results land in pre-indexed slices and diagnostics in a `diag.Collector`, then sort, so output is independent of scheduling. `secret.Resolver.Resolve` is serialized by a mutex; `Run` is one goroutine owned by the Node's lifecycle supervisor, polls only the active table's `file` references, publishes a new table copy-on-write and calls subscribers sequentially on its goroutine (subscribers MUST NOT block; they swap their own pointers).

Exported for other areas: `hub.Bundle` (typed resources, materialized trees, effective chains, secret uses, CEL uses), `registry`, `diag`, `schemaview.Set.Served`, `profile.Encode`, `loader.Load/FromResources/LoadEnvironments`, `secret.Resolver`.

## 4. Dependencies on other areas

| Direction | Area | Contract |
|---|---|---|
| Needs | CEL | `validate.CELChecker` over `cel.dev/cel-go`: parse, type-check against the place's variables and result, cost-estimate (RZ-CFG-014, -015); the CEL area owns the environments and the size estimator |
| Needs | Router | `validate.TemplateChecker` for `match.path.template` grammar (RZ-CFG-005); wildcard-host rule semantics (OQ-data-plane-2) |
| Needs | Telemetry | `*slog.Logger` from `internal/telemetry`; a counter handle for `ruralz_config_secret_rotation_failures_total` on Ruralz aggregates (OQ-observability-16) |
| Needs | Security filters, State Store, TLS | `secret.Use.Check` validators: PEM certificate and key parse, CA and CRL parse, API key length (OQ-security-and-identity-23), State Store URL fits `topology` (RZ-CFG-026) |
| Needs | Existing M0 code | `pkg/config/v1alpha1` types, `api/schema.RenderedV1alpha1()`, `internal/errcode` registry (all codes already registered) |
| Needs | Canonical and diff | Decoding `ruralz.canonical.v1` into `[]*tree.Resource` for `FromResources` (Last-Known-Good boot, `/config/dump` and saved dumps as diff sources) |
| Provides | Canonical and diff | `hub.Bundle.Resources[i].Tree` (materialized, schema-driven encoding with `x-ruralz-list`, `x-ruralz-since` from `schemaview`), `schemaview` keyword lookup for diff impact classes |
| Provides | Snapshot compile, Filter Chain, Router, Upstream | `hub.Bundle`, `hub.EffectiveChain`, `registry.TypeInfo` (class, Phases, scopes) |
| Provides | Lifecycle (Hot Reload, Last-Known-Good, readiness) | `loader.Load`/`FromResources` as the validation gate, `Result.Stats` for `stage="compile"`, `Diagnostics.FirstErrorCode()` for `ruralz_config_activations_total{code}`, `secret.Resolver` for readiness ("every `secretRef` resolved") |
| Provides | CLI | `loader` entry points, `LoadEnvironments`, `diag.WriteText/WriteJSON`, `profile.Encode` for `ruralz bundle render`, per-Environment runs with `Diagnostic.Environment` |

## 5. Libraries

| Module | Version | License | Used for | Catalog row |
|---|---|---|---|---|
| `github.com/goccy/go-yaml` | v1.19.2 (2026-01-08; latest on proxy.golang.org) | MIT, no dependencies | `scanner.Scanner` token stream (depth, anchors, tags before parse), `parser.Parse` with `AllowDuplicateMapKey`, `ast` nodes, `yaml.SyntaxError` | TS "YAML 1.2 parser" v1.19.x; FP §7 |
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.3 | Apache-2.0 | Draft 2020-12 compile and validate of the rendered view, `Vocabulary` for `x-ruralz-*` keywords, `InstanceLocation` for the source map; Plugin `configSchema` checks | TS "JSON Schema validator" v6.0.x; FP §7 |
| `golang.org/x/text` | v0.14.0 or MVS-higher (indirect only) | BSD-3-Clause | Transitive requirement of jsonschema/v6 (message printing); never imported by Ruralz code, so custom `ErrorKind`s are avoided (risk 29) | none needed (transitive, G2 license) |
| Standard library | Go 1.26 floor, 1.27.1 toolchain | BSD-3-Clause | `encoding/json/jsontext` (JSON front end; needs `GOEXPERIMENT=jsonv2` on the 1.26 floor, already required for jwx; default in 1.27), `encoding/json`, `os.Root`, `net/netip`, `net/url`, `regexp`, `unicode/utf8`, `path`, `io/fs`, `sync`, `runtime`, `log/slog` via `internal/telemetry` | n/a |

`cel.dev/cel-go` is reached only through the CEL area's checker; `internal/config/*` does not import it. `github.com/dlclark/regexp2` appears in jsonschema's `go.mod` for its tests only and is not linked.

## 6. Test plan

Coverage target 90% statements for `internal/config/loader`, `profile`, `subst`, `overlay`, `schemaview`, `defaults`, `validate` and `internal/secret` (TQ "Test layers": loader, validation, precedence 90%). Unit tests are hermetic, shuffled, and run under `-race` and without it under `CGO_ENABLED=0`; CLI-platform jobs (darwin, windows) run the loader tests too.

**Negative fixture per code.** Layout `test/conformance/config/<code>-<slug>/` with `bundle/` (and optional `environments.yaml`), `fixture.yaml` (`code`, `stage`, `binaries: [ruralz, ruralz-control, ruralzd]`, `env`, `vars`) and goldens `expected.txt`, `expected.json` with file, line and column (TQ "Conformance suites"). Cases:

| Code | Cases (each asserts code, severity, position, path, hint) |
|---|---|
| 001 | `\xff` byte (1:4); BOM at start and inside a quoted string; BEL control char; 65 nested flow sequences and 65 nested block mappings (at the 65th opener); generated 64 MiB + 1 byte Bundle; 20,001 generated resources; `%YAML 1.1`; `a: [1,`; `? [a]: b`; `!!int abc`; `.ruralzignore` with `[`; symlink escaping the root; FIFO named `x.yaml`; JSON trailing comma, comment, `0777`, two top-level values, lone surrogate `\ud83d`; diagnostic cap reached |
| 002 | block map, flow map, JSON object (both positions, `related`); `1:` versus `"1":` |
| 003 | `&a`, `*a`, `<<:`; billion-laughs alias bomb rejected in the token pass with bounded heap |
| 004 | `!include x`, `!env X`, `!<tag:yaml.org,2002:str>`, `%TAG`, `!!binary`, `!!timestamp`, `!!set` |
| 005 | missing `spec`; missing `metadata.name`; `port: "abc"`; upper-case name; `type: rateLimit` (hint `ratelimit`); `path` plus `pathExpression`; list document root; `.inf`; int above int64; `https` listener without certificates; `hosts: ["api.*.example"]`; `trustedProxies: [10.0.0.0/33]`; `authz.ip` both lists empty; `burst` above `requests`; 1,025-byte pattern; 33 transform entries; invalid RE2; Upstream without endpoints; `ring-hash` without `hashKey`; `type: plugin` without `plugin`; `filterClass` on `cors`; Consumer with empty lists; two listeners `name: https`; `methods: [GET, GET]`; `ruralz.io/team` label; `$patch: merge`; `{$patch: replace}` second; `${1X}`; `${A:-${B}}`; Gateway in `--environments`; `secretRef.name: tls.key` (relative) for `file`; overlay name `../x`; `ruralz.io/patch: patch` |
| 006 | `spec.timout` (hint `timeout`); `metadata.namespace`; top-level `status`; `$patch` in a base file; unknown member in a closed config (`auth.basic` `config.realm`); unknown member accepted in an open config (`cors` `config.maxAge`, no diagnostic) |
| 007 | `ruralz/v1beta1`; `ruralz.io/v1alpha1` (CRD hint); `kind: Rout`; `kind: route` |
| 008 | Route in two base files; one identity in two overlay files; one identity twice in one overlay file; Environment twice in `--environments` |
| 009 | `upstream: inventroy` (hint `inventory`); a Policy name used as `upstreams[].name`; `listeners: [htps]`; unknown `excludePolicies` Policy; `consumerQuota: daily` with no `requests` quota; AIModel candidate provider missing; explicit `spec.overlay` without directory; `promotion.from` missing |
| 010 | `${UNDEFINED}` with an Environment (hint nearest variable) and with the process environment (no hint) |
| 011 | `${X}` in `kind`, `apiVersion`, `metadata.name`, a key, `upstreams[].name`, `certificate.secretRef.name`, `match.when`; only 011 reported |
| 012 | literal PEM in `privateKey`; literal `rediss://` in `stateStore.url`; `apiKeys[].secretRef: "abc"`; `{value: x}`; redaction: sentinel literal absent from text, JSON and logs |
| 013 | `${DB_PASSWORD}`; `authorization` header literal; PEM in a label; OTLP endpoint with `user:pass@`; JWT in a CEL literal; exit code stays 0 |
| 014, 015 | via a fake `CELChecker`: positions and offsets for single-line plain, double-quoted and block scalars; `quota` default key checked; with the real checker (CEL area): `response` in `match.when` and a nested comprehension over 10,000 cost units |
| 016 | no Gateway; two Gateways; Gateway in `routes/gw.yaml`; missing `ruralz.yaml`; overlay adding a second Gateway; overlay deleting the Gateway; single-file source (no 016) |
| 017 | Environment in the Bundle; Cluster in an overlay |
| 018 | two Gateway Policies with explicit `slot: edge`; `auth.jwt` and `auth.api-key` on one Route; two `auth.upstream-oauth2` on one Upstream |
| 019 | excluding `ratelimit-global`; Route `cors` replacing an `overridable: false` Gateway `cors`; the same non-overridable Policy on both lists (no error) |
| 020 | `cache` on the Gateway; `auth.jwt` on an Upstream; `auth.upstream-oauth2` on a Route; Plugin with `onUpstreamRequest` attached to a Route |
| 021 | image without digest; config missing `denyCountries` against `configSchema` |
| 022 | `a` from `b`, `b` from `a` |
| 023 | identical match with reordered hosts and header names differing in case; same match on disjoint listeners (no error) |
| 029 | `auth.jwt` open; `plugin` with `filterClass: authz` open; `ratelimit` open (no error) |
| 030 | overlay `ruralz/v1alpha2` against base `ruralz/v1alpha1`, using a test-only second view registered in `schemaview.Set` (one served version cannot produce it; risk 17) |
| 031, 032 | 17 steps with default max; `maxCompositionSteps: 2` with 3 steps; explicit sum 11Mi; explicit sum 10Mi with one unset step |
| 034 | token-budget Route to an AIModel without `maxOutputTokens` |
| 035, 036, 037 | same `username` in two Consumers; same `uriSan`; iterations 599999 and 1000001; `jwksUrl: http://...`; `tokenUrl: ftp://...`; `https:///x` |
| 026 (Node) | `env` unset, empty, disallowed name; `file` missing, outside root, `..` escape, symlink escape, permission denied, over 4 MiB, `key` absent, not JSON; `kubernetes` provider; failing `Check`; all failures reported together |

**Table-driven unit tests.** Scalar typing table (every core-schema form, `yes`, `on`, `0777`, `0b1`, `1_000`, `2026-09-23`, `+5`, `-0`, `0o17`, `0x1F`, `1e3`, tags); depth algorithm (compact `- - a`, flow inside block, multi-document reset); `.ruralzignore` pattern table (anchoring, `**`, negation, directory-only, parent exclusion); ordering (`a.yaml` versus `a/b.yaml`, Unicode names); substitution grammar and re-typing per schema type (`ByteSize` `10Mi` stays string, `1024` becomes integer); overlay merge per list type including reorder via `{$patch: replace}`, `null` removal, `ruralz.io/patch` replace and delete, config dispatch after a `spec.type` change; defaults table (one case per default in requirement 36; absent `stateStore` stays absent; explicit `overridable: false` survives); registry table equals FP §10 row by row; effective chain for the CM "Worked example" (12 rows); nearest-match hints; path rendering (text and JSON) including escaping; diagnostics sort and cap; secret `Value` redaction through `fmt` verbs, `slog` and `json`.

**Property tests** (`testing.F` targets, seeds in `pr-fast`, long runs `nightly`; TQ "Required properties"): (P1) permuting file names and the order of `map` and `set` entries and mapping keys yields equal materialized trees after sorting unordered lists, and equal diagnostic sets; (P2) random trees emitted as block YAML, flow YAML and JSON parse to equal trees (YAML/JSON twins); (P3) substitution with values containing `\n`, `: `, `- `, `{`, `#`, `${`, `$${` never changes tree shape and never leaves an unescaped `${` in a forbidden position; (P4) `Load(Encode(Load(x).Rendered))` equals `Load(x)` (render idempotency, `$${` included); (P5) splitting an overlay across files in any order yields the same result; (P6) `Materialize` is idempotent; (P7) diagnostics are identical for `Workers` 1, 2 and 8.

**Fuzz targets** (TQ "Fuzzing"; each crasher becomes a committed seed): `FuzzProfileYAML` (no panic or hang; only RZ-CFG-001 to -005; heap growth under 256 MiB for inputs up to 1 MiB; 2 s per input); `FuzzProfileJSON`; `FuzzSubstitute` (output validates or yields a registered code, checked with `errcode.Lookup`); `FuzzOverlayMerge`; `FuzzIgnore`; `FuzzLoadBundle` (txtar archive as a Bundle; every code registered; no goroutine left running); `FuzzEncodeRoundTrip`.

**Golden tests.** Every negative fixture's `expected.txt` and `expected.json` (byte-exact, `eol=lf` in `.gitattributes`, `-update` flag to regenerate); `examples/shop-bundle` (the CM example Bundle, plus its JSON twin) rendered for `prod` and `staging` from `control/environments.yaml` (`profile.Encode` output) and its materialized trees as JSON; a multi-error, multi-file ordering golden.

**Upstream suites** (ADR3 "Confirmation"; TQ "Conformance suites"; `pr-full`): the YAML Test Suite at a pinned data release vendored under `test/fixtures/yaml-test-suite/`, each case through `profile.Parse` compared with `in.json` or expected to fail when `error` exists, failing only on `expected-failures.txt` entries (case ID plus reason: `profile:anchor`, `profile:tag`, `profile:complex-key`, `profile:directive`, `reviewed:<goccy issue>`); a listed case that starts passing also fails, forcing the list down. The JSON-Schema-Test-Suite at a pinned commit, `tests/draft2020-12/` without `optional/`, run through jsonschema/v6 with `remotes/` served by an in-memory loader: zero failures.

**Meta-validation test** (`internal/config/schemaview/meta_test.go`, RL "Generator rules"): both views compile against draft 2020-12 with the vocabulary asserting keyword shapes; every `$ref` resolves; all ten kinds and every Policy type dispatch exist; every raw document under `examples/` validates against the authoring view; every Bundle under `examples/` loads with zero errors for each Environment in its `control/environments.yaml` and without `--env`.

**Integration tests** (`//go:build integration`, stage 8): `file` provider on real temporary directories: Kubernetes-style `..data` symlink swap observed within 2 poll intervals; rotation to an invalid PEM keeps the old value and increments the counter; `NewResolver` refusals (relative root, `RURALZ_DATA_DIR` inside root); a Bundle directory replaced by atomic rename during `Load` yields one consistent tree; CRLF checkout of the example Bundle yields the LF trees.

**Benchmarks** (component benchmarks, `nightly`; PB "Component benchmarks"): `BenchmarkLoadLadder` on the PB synthetic ladder (1,151; 5,751; 11,501; 19,551 resources) with `Workers=4`: 10,000 resources under 2 s and 20,000 under 4 s (target); `BenchmarkFromResources` per resource (about 40 µs hypothesis); `BenchmarkParseYAML` MB/s and allocs per resource; peak heap for the hostile fixtures recorded for OQ-configuration-model-18.

**Secret leak tests** (RM Security row): sentinel values in `env` and `file` secrets and in RZ-CFG-012 literals never appear in diagnostics (text and JSON), logs captured from the test logger, `Encode` output, or panics.

## 7. Open questions blocking M1 in this area

| ID | Question | Adopt | What the code does |
|---|---|---|---|
| OQ-configuration-model-8 (RM exit criterion 7) | Default `failureMode` for `quota` (open) and `ai.token-budget` (closed) | (a) As registered | `registry` table: `quota` default `open`, `ai.token-budget` default `closed`; `defaults.Materialize` writes them into every Policy lacking `failureMode` |
| OQ-security-and-identity-22 | How are secrets and Node connections restricted? | (a) `RURALZ_SECRET_ROOT`, `RURALZ_SECRET_` prefix, `RURALZ_FETCH_ALLOW`, `security` impact class, secret-to-destination binding, State Store MAC key (proposed) | `secret.Resolver` enforces the root (default `/etc/ruralz`) through `os.Root`, allows `env` only for `RURALZ_STATE_STORE_URL` and `RURALZ_SECRET_*`, refuses to start when protected paths are inside the root; `RURALZ_FETCH_ALLOW`, impact class, binding and MAC key belong to the connection, diff and State Store areas |
| OQ-performance-budgets-and-benchmarking-6 | Loader workers and yield | (a) W = max(1, `GOMAXPROCS`/2), yield every 100 µs (proposed) | `Options.Workers` and `Options.Yield`; Node passes those defaults, CLI uses `GOMAXPROCS` (requirement 53) |
| OQ-traffic-management-and-resilience-6 | Deadlines defaults and timeout fields ("canonical form (M1)") | (a) proposed values with (c) per-protocol Route timeouts (recommended) | Static values become schema defaults via `+ruralz:default` markers in `pkg/config/v1alpha1` (`retries.attempts` 1; `circuitBreaker` 1,024 / 256 / 5 / 30s; `healthCheck.passive` 5 / 30s), materialized only inside present parents; per-protocol Route `timeout`, `Upstream.timeout` and `perTryTimeout` stay runtime rules, not materialized (risk 11). Must be settled before the first golden digest |
| OQ-data-plane-2 | Wildcard host registration | (a) one leading `*.` label (answered in CM "Route") | `validate`: any other `*` in `match.hosts` is RZ-CFG-005 |
| OQ-scalability-and-distributed-state-2 | `redis` topologies and URL form | (a) standalone and cluster, no Sentinel (answered in CM "Gateway") | Schema already has `topology`; the resolver runs the State Store area's URL check as a `secret.Use.Check` (mismatch RZ-CFG-026) |
| OQ-security-and-identity-2, -3, -18 (RM exit criterion 7) | `auth.basic`, `auth.mtls`, `authz.ip` schemas | (a) register as authored (answered in CM) | RZ-CFG-035, -036, `authz.ip` both-empty and CIDR checks, `auth.mtls` exactly-one (schema) |
| OQ-security-and-identity-6 (RM exit criterion 7) | Trusted proxies | (c) both, answered in CM "Gateway" | `trustedProxies` CIDR check (RZ-CFG-005); `proxyProtocol` default `false` materialized |
| OQ-security-and-identity-24 | How does `oauthClients` name its issuer? | (a) the Consumer's `jwt` issuer (current) | No new field; `validate` SHOULD report RZ-CFG-005 for a Consumer with `oauthClients` and no `credentials.jwt` entry (risk 34) |
| OQ-security-and-identity-7 | Admin credential settings | (a) three `RURALZ_ADMIN_*` settings (proposed) | `secret.Config.Protected` includes `RURALZ_ADMIN_TLS_DIR` and `RURALZ_ADMIN_TOKEN_FILE` for the startup refusal |
| OQ-traffic-management-and-resilience-5 (RM exit criterion 7, M1 part) | Breaker guard fields | (a) proposed, `minimumLegs` in M1 | Schema-only change in `pkg/config/v1alpha1`; the loader picks it up from the generated view with no code change |
| OQ-scalability-and-distributed-state-3 (RM exit criterion 7, M1 part) | Second State Store connection for caches | (a) a cache connection under `Gateway.spec.stateStore` | Schema-only; its URL is an `x-ruralz-secret` field, collected and resolved generically |
| OQ-scalability-and-distributed-state-11 | `config.localOnly` and pack amendments | (a) all (recommended) | No loader rule beyond the registered schema (`localOnly` default `false`) |
| OQ-security-and-identity-1 | Extra `auth.jwt` and `auth.api-key` fields | (a) as proposed | Schema-only for this area |
| OQ-configuration-model-18 (non-blocking; 64 applies) | Depth default and peak loader memory | (a) 64 levels, memory measured (proposed) | `MaxDepth` 64; hostile fixtures record peak heap; `MaxDocumentTokens` reported with it |
| OQ-configuration-model-19, -20 (non-blocking) | Open configs; `${VAR}` in constrained strings in the authoring view | (a) each (proposed) | Loader follows the generated view; neither changes pipeline code (the pipeline validates the rendered view after substitution) |
| OQ-cli-and-api-surface-1 (non-blocking) | Diagnostics envelope | (a) bare array gaining `environment` (proposed) | `diag.WriteJSON` writes a bare array; `environment` set per run |

## 8. Deferred (M2 and later): do not build now

| Item | Milestone | Extension point left in M1 |
|---|---|---|
| `--env` without `--environments` fetching Environments from Ruralz Control; Ruralz Control ingest, re-render and RZ-CFG-027 on push | M2 | `loader.Load` takes `*v1alpha1.Environment` and `subst.Source`, not a file; `Source.FS` accepts a pushed tree |
| Control-mode canonical JSON path on Nodes | M2 | `loader.FromResources` (already used in M1 for Last-Known-Good boot) |
| `secretRef` providers `kubernetes` and `vault`; cloud managers (OQ-configuration-model-5); encrypted secret persistence (OQ-configuration-model-15, never through M3) | M2+ | `secret.Provider` interface and providers table; M1 returns RZ-CFG-026 "Planned (M2)" |
| `x-ruralz-validations` rules (CEL over `self`) in schemas and Plugin `configSchema` | M2 | Parsed into `schemaview.Keywords.Validations`; not evaluated |
| Online Plugin check (RZ-CFG-028), signatures (RZ-CFG-033), OCI sources `oci://` | M2 | `validate.Checkers` gains a `Plugin` checker; `loader.Source` gains an OCI variant |
| CRD path (`ruralz.io/v1alpha1` translation), status conditions | M2 | RZ-CFG-007 hint only; `tree.Resource.APIVersion` kept per resource |
| Second apiVersion, hub types, conversion, `--api-version` beyond identity, RZ-CFG-024 skew and RZ-CFG-025 deprecations | M2+ | `schemaview.Set` keyed by apiVersion; `hub` aliases replaceable by own types; conversion registry; `Keywords.Since` carried |
| Runtime Consumer source (OQ-configuration-model-17) | not before M4 | none |
| Semantic rules of M2 to M4 features (Plugin runtime, AI kinds, `grpc`, `graphql`, `topic` matching, `kubernetes` discovery, messaging) | M2 to M4 | Structural validation only; the Node's compile stage rejects unsupported features (risk 28) |
| Limit flags and the encoded Snapshot limit (OQ-control-plane-and-gitops-25) | M2 | `loader.Limits` is a struct, not constants |

## 9. Risks and ambiguities

1. **Token materialization is a memory bomb.** CM and ADR3 say depth is counted over `lexer.Tokenize` output, but `Tokenize` builds every token first (about 180 B each measured on goccy v1.19.2), and goccy's recursive parser on 200,000 nested `[` exhausted over 4.7 GB and died with a fatal out-of-memory error (probe during this review). Resolution: incremental `scanner.Scanner` (same tokens), abort at depth 65, per-document parse, and a `MaxDocumentTokens` cap of 1,000,000 (hypothesis) reported under OQ-configuration-model-18. Remaining cost: `scanner.Init` converts a file to `[]rune` (4x its size).
2. **goccy scalar typing is not YAML 1.2 core.** Probe: `0777` is typed octal, `1_000` and `0b1` integers, `1e3` a string. Ruralz typing from token text (requirement 12) is mandatory, as ADR3 states.
3. **JSON front end.** CM says `.json` uses "the same loader"; goccy parses JSON but reported a tab-indented key at column 1 instead of 2 (probe). Resolution: a `jsontext` front end feeding the same tree and pipeline; property P2 pins YAML/JSON equivalence. Needs `GOEXPERIMENT=jsonv2` on the 1.26 floor, already required by jwx (FP §7).
4. **Schema gaps.** Many documented rules are not in the generated schema (keyed-list uniqueness, wildcard hosts, `https` needs certificates, iterations range, CIDRs, `authz.ip` present-but-empty lists, pattern byte length since `maxLength` counts code points). They are RZ-CFG-005 semantic checks here; moving expressible ones into schema markers is a schema-level change for the configuration-model owner.
5. **Plain scalar typing ignores the field.** A raw plain `0777` or `true` in a string field is an integer or boolean and fails with RZ-CFG-005 (quote it); ADR3 only fixes the integer-field and substituted cases. The message hints "quote the value".
6. **`APIKey.secretRef` is a `SecretRef`, not a `SecretValue`.** RZ-CFG-012 is keyed on `x-ruralz-secret`, not on the `SecretValue` shape.
7. **`Route.spec.listeners` is unmarked.** CM says references resolve statically, but the schema marks no keyword there, so `${VAR}` is allowed in M1; recommend a marker in a later schema level.
8. **Materialized CEL default.** `QuotaConfig.key` defaults to the CEL text `consumer.name`, so the CEL checker must see materialized defaults (requirement 41).
9. **`filterClass` materialization.** "every registry default (`slot`, `failureMode`, `filterClass`)" is read as `filterClass: custom` for `plugin` only; built-in types imply their class, and an authored `filterClass` on them is RZ-CFG-005.
10. **Defaults never create parents.** Needed for the `stateStore` fallback semantics; consequence: `circuitBreaker: {}` and an absent `circuitBreaker` behave alike but digest differently (acceptable: equal digests imply equal behavior, not the converse).
11. **OQ-traffic-management-and-resilience-6 is blocking for the canonical form.** Route and Upstream timeout defaults are not in the schema and depend on other resources or the request; they must be decided (runtime rule versus materialized) before the first golden digest.
12. **Limit counting is unspecified.** This spec counts source bytes across base and selected overlay, resources after overlay, and documents as a running guard; the `--environments` file has its own cap.
13. **Token cap and diagnostic cap are not in the docs** (1,000,000 tokens per document, 10,000 diagnostics); both are proposals to record under OQ-configuration-model-18 and OQ-cli-and-api-surface-1.
14. **Text format details** (`related` suffix, ` [environment=...]`, `-` for no file) and the JSON `related` member are additions; OQ-cli-and-api-surface-1 should record them (additive members only).
15. **Column units** are Unicode code points (goccy's unit); LSP clients use UTF-16, so editor integrations must convert.
16. **RZ-CFG-024 in file mode** ("a file-mode Node rejects an unserved field at load") cannot be told apart from an unknown field (RZ-CFG-006) with a single schema level; M1 emits 006. Needs a schema-level marker in the Revision (M2).
17. **RZ-CFG-030 is unreachable with one served apiVersion** (an unserved overlay apiVersion is RZ-CFG-007); tested with a test-only second view.
18. **Overlay edge cases not in CM:** deleting an absent list entry or resource is a no-op (Kubernetes behavior); an explicit `spec.overlay` naming a missing directory SHOULD be RZ-CFG-009 to catch typos, while a defaulted one means "no overlay".
19. **`excludePolicies` naming a Policy not attached to the Gateway** resolves (the Policy exists) and is a no-op; a warning code does not exist.
20. **RZ-CFG-018 at Upstream scope.** CM says Upstream Policies "take no part in slot comparison" (with Gateway and Route); this spec still applies one Policy per slot within one Upstream's list, per "a slot holds one Policy per scope".
21. **RZ-CFG-023 normalization** is not defined in CM; this spec includes effective listeners and case-folds hosts and header names (DP "Compiled structure": hosts case-insensitive).
22. **Step default shares** (`maxBodyBytes`) are derived at runtime, not materialized; they are a deterministic function of the Revision.
23. **Secret value edge cases:** `env` `key` "Unused" is ignored; an empty `env` value is RZ-CFG-026; `file` bytes are exact (no newline trimming), so single-line consumers (State Store URL, client secret, API key) must decide trimming in their `Check`.
24. **File watch, size cap:** CM says "File watch"; this spec polls every 2 s with a 4 MiB cap (both proposed targets), portable to macOS development and correct for `..data` symlink swaps.
25. **Discovery details absent from CM:** `.ruralzignore` scope (root only, also filters overlays, cannot exclude `ruralz.yaml`), symlink policy, case-sensitive extensions, non-regular files.
26. **`--environments` file contents:** only `Environment` and `Cluster` allowed; errors in it are diagnostics (verb exit 1), an unknown `--env` name is a usage error (exit 2).
27. **Tags and directives:** CM rejects "custom tags"; this spec accepts the seven YAML 1.2 core tags and rejects `%TAG`, `%YAML 1.1` and YAML 1.1 types (`!!binary`, `!!timestamp`, `!!set`); ADR3's review checklist item applies if the owner disagrees.
28. **M2 and M3 kinds in the M1 golden corpus.** The CM example Bundle (golden corpus, RZ-CFG-015 must pass) contains `Plugin`, `AIProvider`, `AIModel`, a `grpc` Upstream and `kubernetes` discovery, so M1 `ruralz bundle validate` and `build --offline` must accept them; the M1 Node must reject them at compile, but no RZ code names "not supported by this release" (owner: lifecycle area and configuration-model).
29. **jsonschema/v6 custom errors import `golang.org/x/text/message`.** A custom `ErrorKind` would make x/text a direct dependency needing a catalog row; this spec keeps the vocabulary to keyword parsing and does uniqueness checks in `validate`.
30. **jsonschema default loader** can read files and URLs; `UseLoader` with a refusing loader is mandatory (also for Plugin `configSchema` `$ref`s).
31. **Transform pattern limit** "1 KiB" is bytes while JSON Schema `maxLength: 1024` counts code points; the byte check in `validate` is authoritative.
32. **`limits.maxPluginMemoryBytes`** defaults to 2 GiB in `pkg/config/v1alpha1` while the CM Gateway sketch shows `1Gi` as an example value; the WASM doc owns the default, and materialization uses the schema's 2147483648.
33. **Duplicate-within-one-Consumer credentials** (two `certificates` entries with one `subject`) are allowed; RZ-CFG-035 covers two Consumers only.
34. **OQ-security-and-identity-24 consequence** (a Consumer with `oauthClients` and no `jwt` can never bind) is a recommended RZ-CFG-005, not stated in SEC.
35. **"Credential-shaped" (RZ-CFG-013) is undefined** in CM; the detector list in requirement 39 is versioned with the loader and only warns.
36. **Malformed `${`** has no dedicated code; RZ-CFG-005 is used, and nested defaults are rejected rather than read literally.
37. **Short API keys** from `secretRef` (under 22 base64url characters) have no registered code (OQ-security-and-identity-23 option (a) wants one); M1 reports RZ-CFG-026 through the key's `Check`.
