# Foundation Freeze Decisions

| Field | Value |
|---|---|
| Frozen artifact | `docs/_meta/foundation-pack.md`, version v1 (binding) |
| Date | 2026-09-23 |
| Inputs | `docs/_meta/reviews/foundation-a.amendments.json` (system-overview, configuration-model) and `docs/_meta/reviews/foundation-b.amendments.json` (vision-and-positioning, tech-stack-and-libraries) |
| Research used | `docs/_meta/research/tooling-and-licenses.md` (new addendum) and `docs/_meta/research/licensing-landscape.md` section 11 (corrected) |
| Proposals | 114 raw amendment strings (29 + 29 + 16 + 40), merged into 61 distinct amendments below |
| Outcome | 55 accepted, 6 modified, 0 rejected |

## Source key

Each distinct amendment lists the raw proposals it merges. `SO-n` and `CM-n` are the n-th entry (1-based) of the `amendments` array for `system-overview` and `configuration-model` in `foundation-a.amendments.json`; `VP-n` and `TS-n` are the n-th entry for `vision-and-positioning` and `tech-stack-and-libraries` in `foundation-b.amendments.json`. "Pack section" names the section of `docs/_meta/foundation-pack.md` that carries the decision; "manifest" means `docs/_meta/manifest.yaml`.

## Decisions

| # | Amendment (short) | Decision | Reason | Pack section |
|---|---|---|---|---|
| 1 | Remove the forbidden milestone name "v1" from the ADR-0015 row and Other defaults; record `SO_REUSEPORT` limits and the mitigation (SO-1) | accept | Row now names CBPF steering through `golang.org/x/sys/unix`, `tcp_migrate_req` as the alternative and QUIC loss on UDP 8443; no milestone name remains | 7 (ADR-0015 row, Other defaults), 8.4 |
| 2 | Define the local token bucket as a per-Node ceiling, optionally derived from a published Node count; state the N x ceiling fail-open bound (SO-2) | accept | Removes the hidden Node-count coordination; bound owned by Scalability and distributed state | 7 (ADR-0008 row), 8.8 |
| 3 | Fix the architecture summary: Bundles render into Revisions; use Node and Last-Known-Good (SO-3, CM-4 second half, CM-26) | accept | Aligns section 6 with the section 2 canonical names | 6 |
| 4 | At most one blocking State Store round trip per Policy before commit; post-commit writes asynchronous, bounded and droppable, also in section 6 and P3 (SO-4, SO-12) | accept | Keeps Response Cache stores and settlement from breaking the rule; P3 in the vision document already matches | 6, 8.7 |
| 5 | Decide whether local token estimates may enforce a Token Budget in `onChunk` (SO-5) | accept | Local counts guard the stream within the reservation and are never billed; closes OQ-system-overview-20 | 7 (ADR-0014 row), 8.9, 14 |
| 6 | Add the canonical term active Revision (SO-6) | accept | Separates the served Revision from the persisted boot Revision | 2, 8.2, 11 |
| 7 | Policy precedence per slot, additive types stack, `overridable: false` guardrail (SO-7, SO-18, CM-10, CM-19) | accept | Matches the Configuration model resolution rules, including the shared `auth` slot | 3 (Policy row), 8.12 |
| 8 | `RURALZ_STATE_STORE_URL` is the fallback source for `spec.stateStore.url`, not a process setting (SO-8) | accept | Matches the Configuration model Gateway section | 2 (Env vars row) |
| 9 | One-line meaning per error-code area, especially `STS` and `RT` (SO-9) | accept | Documents now choose areas consistently; selection rule added | 8.6 |
| 10 | The artifact a file-mode Node pulls is a Revision built by `ruralz bundle build` and published by `ruralz bundle push`; the digest covers the rendered per-Environment Bundle (SO-10) | accept | Producers row states both | 8.1 |
| 11 | Define the optional regional Ruralz Control or point it to an Open question (SO-11, SO-21) | accept | Defined as a Ruralz Control deployment serving one Region, Planned (M4); its role stays OQ-system-overview-16 | 2 (Cell row), 8.13 |
| 12 | A Rollout exists only when Ruralz Control delivers a Revision; file-mode delivery is not a Rollout (SO-13) | accept | Stated in the canonical names and the Rollout states section | 2 (Rollout row), 8.3 |
| 13 | `rev-<12 hex>` is the display form only; wire, storage and verification use the full digest (SO-14) | accept | Mismatch is RZ-CFG-027 | 2 (Revision row), 8.1 |
| 14 | Distinguish Last-Known-Good from the active Revision; promote to Last-Known-Good when the Rollout reaches `complete` (Control mode) or on activation (file mode) (SO-15, SO-23) | modify | Promotion is level-triggered on the Cluster's promoted digest carried by every snapshot, delta and heartbeat, so late reconnects, Enrollment and scale-out also promote; file mode promotes on activation as proposed; closes OQ-system-overview-2 | 8.2, 11 |
| 15 | Add a `paused` Rollout state (SO-16, SO-19) | accept | Chosen over pausing inside `canary` or `progressing`; `ruralz rollout pause` and `resume` added; closes OQ-system-overview-17 | 2 (Rollout row), 8.3, 9 |
| 16 | Say whether Revision content and audit segments stored by digest are part of the Control Store (SO-17) | accept | They are, and `ruralz control backup` covers them | 2 (Control-plane persistence row), 11 |
| 17 | Revision is the content-addressed digest of a Bundle rendered for one Environment (SO-20, SO-27, CM-4 first half, CM-14, CM-16) | accept | Resolves OQ-system-overview-14 as the Configuration model proposes | 2 (Revision row), 8.1 |
| 18 | Nodes always dial `ruralz-control` on 8091 and Revisions flow down that stream (SO-22) | accept | Connection direction fixes NAT and egress-only firewall compatibility | 2 (Control Stream row), 8.4 |
| 19 | One-line meaning per Rollout state; any NACK or failed gate ends in `rolled-back`; `failed` only when the revert fails (SO-24) | modify | Only a deterministic NACK, failed gate or lagging threshold rolls back, and only with `autoRollback: true` (otherwise `paused`); a transient NACK is retried, then the Node is quarantined; `failed` means the revert itself failed, as proposed | 8.3 |
| 20 | Add a signing decision backed by a new ADR (SO-25) | accept | ADR-0017 decides digest verification always, Ruralz Control and Sigstore signatures, Node verification by default; manifest `adrs` entry added at freeze completion; closes OQ-system-overview-3 | 7 (Artifact signing row), 8.14, 14 |
| 21 | Add a Raft peer port for `ruralz-control` with mutual TLS, or multiplex on 8091 (SO-26, TS-5, TS-21, TS-33 first half, TS-37) | accept | Dedicated port 8092 keeps Nodes off peer traffic in firewall rules; closes OQ-system-overview-7 | 2 (Default ports row), 8.4 |
| 22 | Give downstream HTTP/3 on UDP 8443 a milestone (SO-28, VP-8) | accept | Planned (M3) with the other multi-protocol work; off in FIPS builds; closes OQ-system-overview-1 | 2 (Milestones, Default ports rows), 7, 8.4 |
| 23 | `/readyz` reports an active validated Revision, bound listeners and no drain, and never fails on Control Stream loss (SO-29) | accept | Prevents a control-plane incident from pulling every Node | 8.5 |
| 24 | Fix library choices for the YAML 1.2 parser, JSON Schema 2020-12 validator, protobuf runtime, `/metrics` exporter, ULID and SigV4, or defer them explicitly (CM-1, TS-6, TS-13) | modify | YAML parser (`goccy/go-yaml`) and JSON Schema validator (`santhosh-tekuri/jsonschema/v6`) are selected, because the research addendum shows each as the only candidate meeting the configuration model and gates; the other concerns stay explicit pending selections until the tech stack document evaluates the researched candidates | 7 (YAML 1.2 parser, JSON Schema validator, Pending selections rows), 14 |
| 25 | Revision digest over a versioned canonical serialization that pins schema and registry defaults (CM-2) | accept | `ruralz.canonical.v1` with defaults materialized, so equal digests mean equal behavior across Node versions | 8.1, 12 |
| 26 | A Filter failure in `onChunk` after commit ends the stream; in `onLog` it only records telemetry (CM-3) | accept | Consistent with read-only `onLog` | 4, 8.10 |
| 27 | Each Policy type's `spec.config` is authored by its feature document and registered and published by the Configuration model; relax style guide section 5 accordingly (CM-5, CM-11, CM-18, CM-21) | accept | The pack states that a registered `config` field counts as defined in the Configuration model for style guide section 5, so the style guide text needs no change; closes OQ-configuration-model-9 | 3 (preamble) |
| 28 | Mirrored CRDs use `ruralz.io/v1alpha1` and translate to Bundle `ruralz/v1alpha1` by changing only the group (CM-6, CM-12, CM-27) | accept | Kubernetes rejects a CRD group without a dot; option (a) of OQ-configuration-model-1 | 2 (Config unit row), 7 (ADR-0016 row), 12, 14 |
| 29 | Base files form a lexical-order union, duplicate identity is an error, only `overlays/<env>/` patches (CM-7) | accept | Replaces the ambiguous "multi-file merge" wording | 5 |
| 30 | Consumer API keys stored hashed; a `secretRef`-held key is hashed at load and only the hash kept in memory (CM-8, CM-15, CM-17) | accept | Option (a) of OQ-configuration-model-14 | 3 (Consumer row), 14 |
| 31 | Add Filter class, slot and upstream leg to the glossary seed (CM-9) | accept | Defined in section 11; added with active Revision, Enrollment and promoted digest to manifest `glossary_terms` at freeze completion | 11 |
| 32 | The RZ-CFG registry is owned by the Configuration model (CM-13) | accept | Registry owner column; range updated to RZ-CFG-032 | 8.6 |
| 33 | List the dotted Policy type identifiers from the Configuration model registry (CM-20) | accept | Full registry table with forbidden spellings | 10 |
| 34 | Exactly one `Gateway` per Bundle, declared in `ruralz.yaml` (CM-22) | accept | Removes Route-to-Gateway binding rules (RZ-CFG-016) | 3 (Gateway row) |
| 35 | Define Bundle scope; `Environment` and `Cluster` are never in a Bundle or its digest (CM-23) | accept | Allows one Revision to roll out to many Clusters (RZ-CFG-017) | 3 (Scope) |
| 36 | Revision digest covers the Bundle rendered for one Environment with schema defaults not materialized, serialized as RFC 8785 (CM-24) | modify | Rendered-per-Environment scope and RFC 8785 accepted; "defaults not materialized" rejected in favor of #25, which the Configuration model itself adopted | 8.1 |
| 37 | An Environment is a promotion path for source commits, each Environment rendering its own Revision (CM-25) | accept | Overlays and variables are inside the digest, so a Revision is never promoted unchanged | 3 (Environment row), 8.1 |
| 38 | The loader rejects anchors, aliases, merge keys, custom tags, duplicate keys and non-UTF-8 or BOM input; overlays use schema-driven strategic merge (CM-28) | accept | Prevents alias-expansion denial of service and whole-list overlay diffs | 5 |
| 39 | Keep `topic` matching only with an event-ingress declaration on `Gateway`, such as `eventSources`, Planned (M4) (CM-29) | modify | `topic` matching stays Planned (M4), but no `Gateway` field is invented at the freeze; the ingress declaration stays OQ-configuration-model-11 | 3 (Route row) |
| 40 | Enrollment identity is the second kind of durable Node state (VP-1) | accept | Persisted under `${RURALZ_DATA_DIR}/identity/` | 8.11 |
| 41 | Token Budget reservation and settlement: reserve estimate plus cap, settle with provider usage, charge the reservation when usage is missing (VP-2) | accept | Reservation and settlement table | 8.9 |
| 42 | State the Ruralz Cloud tenancy model and public-interface billing (VP-3) | accept | Makes "no cloud-only hooks" enforceable | 1 (Ruralz Cloud tenancy row) |
| 43 | Reword the vision acceptance criterion to "Kong 3.10 ending free mode per its release note" (VP-4, VP-9, VP-15) | accept | Research records conflicting first-party wording; missing from pack v1 and applied to the manifest `vision-and-positioning` acceptance at freeze completion | none (manifest) |
| 44 | The FIPS variant is a build flavor with the same features and license (VP-5) | accept | "No enterprise build" means no feature difference, not a single binary | 1 (FIPS build row) |
| 45 | CLI commands for the OpenAPI importer and exporter and end-to-end tests (VP-6) | accept | `ruralz bundle import openapi`, `ruralz bundle export openapi`, `postman`, `dot` and `ruralz test run`; serving stays OQ-vision-and-positioning-11 | 9, 13, 14 |
| 46 | Decide IP filtering and GeoIP as Policy types or Plugins (VP-7) | accept | Built-in `authz.ip` Planned (M1) and `authz.geoip` Planned (M2); closes OQ-vision-and-positioning-10 | 10, 14 |
| 47 | Correct `licensing-landscape.md` section 11 on DCO and relicensing (VP-10, VP-16) | accept | Research correction, not a pack change; done in the research file and recorded as complete | 14 (Research actions) |
| 48 | Admit a Token Budget request only if the remaining budget covers the full reservation; write the default output cap into the upstream request (VP-11) | accept | Makes the reservation an upper bound on output tokens | 8.9 |
| 49 | A Control-mode Node without Last-Known-Good needs Ruralz Control or an OCI-published seed before it is ready (VP-12) | modify | Fixed rule: it stays not ready until it enrolls and receives its first Revision; OCI seeding during an outage stays OQ-system-overview-12 | 8.11 |
| 50 | Name the Node-wide aggregate Plugin memory cap field (VP-13) | accept | Gateway `spec.limits.maxPluginMemoryBytes`; closes OQ-vision-and-positioning-14 | 3 (Gateway row), 8.11, 14 |
| 51 | `failureMode` is overridable only for non-security types; authentication and authorization are closed only (VP-14) | accept | RZ-CFG-029 | 8.10 |
| 52 | wazero repository `github.com/wazero/wazero`, Go module path `github.com/tetratelabs/wazero` (TS-1, TS-8, TS-14, TS-23, TS-28) | accept | Corrected at freeze; manifest ADR-0004 decision synced at freeze completion; closes OQ-tech-stack-and-libraries-1 | 7 (WASM runtime row) |
| 53 | `net/http` (HTTP/1.1, HTTP/2, h2c) plus quic-go; `x/net/http2` only for non-deprecated low-level APIs (TS-2, TS-9, TS-15, TS-24, TS-29, TS-34) | accept | x/net v0.59.0 deprecates `Server` and `Transport`; manifest ADR-0009 title and decision synced at freeze completion; closes OQ-tech-stack-and-libraries-2 | 7 (HTTP stack row) |
| 54 | cel-go import path `cel.dev/cel-go`; update the manifest acceptance text naming `google/cel-go` (TS-3, TS-10, TS-16, TS-25, TS-30, TS-35) | accept | Pack row corrected in v1; the manifest acceptance text was still missing and is updated at freeze completion; closes OQ-tech-stack-and-libraries-3 | 7 (Expressions / authz row) |
| 55 | Go 1.26 floor with `GOEXPERIMENT=jsonv2` for floor builds; release and FIPS builds pin the newest Go through `toolchain` (TS-4, TS-11, TS-17, TS-26) | accept | Keeps the 1.26 floor rather than raising it to 1.27 | 7 (Language / build row) |
| 56 | Name `hashicorp/raft-boltdb/v2` (v2.4.2 or newer) on `go.etcd.io/bbolt`; list admitted MPL-2.0 modules as named exceptions once research confirms them (TS-12, TS-18, TS-31, TS-36) | accept | Research now confirms the licenses and the row names them; manifest ADR-0006 title and decision synced at freeze completion | 7 (Control Store row) |
| 57 | Control Stream on `connectrpc.com/connect` in gRPC mode on both ends; protos gated by `buf breaking` (TS-19, TS-39) | accept | One RPC library for Ruralz APIs | 7 (Control Stream row) |
| 58 | Research: docs-tooling URLs and licenses and Control Store module licenses, or relax acceptance criterion 1 for CI-only tooling (TS-7, TS-20 first half, TS-27, TS-40) | accept | Research option chosen, relaxation not taken; complete in `tooling-and-licenses.md` sections 1 and 3 | 14 (Research actions) |
| 59 | Research: candidates for the pending selections (YAML, JSON Schema, protobuf, CLI, ULID, `/metrics`, SigV4, SAML, `postgres`) (TS-20 second half) | accept | Complete in `tooling-and-licenses.md` sections 6 to 8; a MaxMind reader remains an open research action | 14 (Research actions) |
| 60 | Semantic Cache on the same State Store deployment through dedicated connections; unavailable on servers without Vector Sets or valkey-search (TS-22, TS-32, TS-38) | accept | Unsupported servers behave as a State Store failure under `failureMode`; closes OQ-tech-stack-and-libraries-7 | 7 (Semantic cache row) |
| 61 | State where `ruralz-control` exposes `/metrics` (TS-33 second half) | accept | Admin port 9902 with `/healthz`, `/readyz`, `/metrics`, `/debug/*`; the exporter library stays pending (OQ-tech-stack-and-libraries-16) | 2 (Default ports row), 8.4, 14 |

## Changes made when completing the freeze

Pack v1 already reflected every decision above except the items marked "at freeze completion". This step made these changes:

| File | Change |
|---|---|
| `docs/_meta/foundation-pack.md` section 7 | Added the YAML 1.2 parser and JSON Schema validator rows (**Selected at freeze**); narrowed Pending selections to OQ-tech-stack-and-libraries-14 to -20 and the MaxMind reader; stated the research-confirmed licenses in the Control Store row |
| `docs/_meta/foundation-pack.md` section 14 | History now counts 61 distinct amendments and cites the research addendum; decided OQ-tech-stack-and-libraries-12, -13 and OQ-configuration-model-16; the Research actions table gained Status and Evidence columns (four complete, one open); "the research phase" reworded |
| `docs/_meta/manifest.yaml` `adrs` | Added ADR-0017 (accepted, owned by `docs/architecture/08-security-and-identity.md`, the owner of OQ-system-overview-3); synced ADR-0004, ADR-0006 and ADR-0009 decisions (and the ADR-0006 and ADR-0009 titles) with the corrected section 7 rows; added research pointers for ADR-0002, -0003, -0004, -0006 and -0009 |
| `docs/_meta/manifest.yaml` `glossary_terms` | Added active Revision, Enrollment, Filter class, promoted digest, slot and upstream leg (48 terms); glossary `min_table_rows` raised to 48 |
| `docs/_meta/manifest.yaml` docs | docs-index: 17 ADRs; security-and-identity and control-plane-and-gitops cite ADR-0017; control-plane-and-gitops lists the `paused` state, Revision signing and active-Revision serving during an outage; data-plane durable state matches section 8.11; vision Kong criterion (#43); tech stack criterion names `cel.dev/cel-go`, raft-boltdb/v2 on bbolt and the `x/net/http2` limit |
| `docs/_meta/manifest.yaml` `research_files` | Added `docs/_meta/research/tooling-and-licenses.md` (present) |

## Verification

- Forbidden milestone names: the only "v1", "v2", "phase" or "GA" left in the pack are in front matter, the section 2 rule that forbids them, Plugin ABI v1, versioned identifiers (`ruralz.plugin.v1`, `ruralz.control.v1`, `ruralz.canonical.v1`, `ruralz.diff.v1`, `ruralz/v1alpha1`), Go module paths (`raft-boltdb/v2`, `oras-go/v2`) and the Filter Chain term Phase.
- Sections 1 and 2 are byte-identical before and after this step. Their identifiers (binaries, images, ports, Control Stream service, Plugin ABI, module path, apiVersion, milestones) match the three system-overview and configuration-model drafts written against the previous pack; the section 1 rows added in v1 (FIPS build, Ruralz Cloud tenancy) and the section 2 additions (active Revision, ports 8092 and 9902, `paused`, HTTP/3 in M3) come from amendments #6, #15, #21, #22, #42, #44 and #61.
- `python -c "import yaml; ..."` loads the manifest: 29 docs, 17 ADRs.

## Follow-ups for conformance (not freeze blockers)

- System overview acceptance criterion 2 in the manifest still says Nodes serve Last-Known-Good while Ruralz Control is unavailable; the conformance step should use "keep serving their active Revision and boot Last-Known-Good after a restart" (section 8.2), as the control-plane-and-gitops criterion now does.
- The tech stack document names `go-immutable-radix` and `golang-lru` as MPL-2.0 exceptions, decides how its license gate reads the module graph (`go-cleanhttp`, `go-retryablehttp`), adds catalog rows for the two selections and the docs tooling, and closes OQ-tech-stack-and-libraries-5, -10, -12 and -13.
